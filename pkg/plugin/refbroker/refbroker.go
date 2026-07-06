// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package refbroker implements a reference message broker plugin for testing
// and development. It provides in-memory topic routing with no external
// dependencies, echo filtering via origin markers, and optional hub API
// callback delivery for inbound messages.
package refbroker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

const (
	// OriginMarkerKey is the config key injected into outbound messages
	// to identify messages originating from the scion hub. The plugin
	// filters these on inbound to prevent echo loops.
	OriginMarkerKey = "scion_origin"

	// OriginMarkerValue is the marker value for hub-originated messages.
	OriginMarkerValue = "hub"

	// defaultBufferSize is the channel buffer for each subscription.
	defaultBufferSize = 64
)

// inboundPayload is the JSON body sent to the hub API inbound endpoint.
type inboundPayload struct {
	Topic   string                      `json:"topic"`
	Message *messages.StructuredMessage `json:"message"`
}

// RefBroker implements plugin.MessageBrokerPluginInterface as a pure in-memory
// message broker. It supports:
//   - In-memory topic routing with NATS-style wildcard patterns
//   - Echo filtering via origin marker to prevent circular delivery
//   - Optional hub API callback for inbound message delivery
type RefBroker struct {
	mu     sync.RWMutex
	subs   map[string]*subscription // pattern -> subscription
	closed bool
	log    *slog.Logger

	// Hub API callback config (set via Configure)
	hubURL     string // e.g. "http://localhost:8080"
	hmacKey    string // HMAC key for hub API auth (base64-encoded)
	brokerID   string // Broker ID for HMAC signing
	pluginName string // X-Scion-Plugin-Name header value

	httpClient *http.Client

	// InboundHandler is an optional callback for inbound messages.
	// When set, messages are delivered here instead of via the hub API.
	// This is used for in-process testing without a running hub.
	InboundHandler func(topic string, msg *messages.StructuredMessage)
}

type subscription struct {
	pattern string
	ch      chan publishedMessage
	done    chan struct{}
}

type publishedMessage struct {
	topic string
	msg   *messages.StructuredMessage
}

// New creates a new RefBroker with the given logger.
func New(log *slog.Logger) *RefBroker {
	if log == nil {
		log = slog.Default()
	}
	return &RefBroker{
		subs:       make(map[string]*subscription),
		log:        log,
		pluginName: "refbroker",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Configure sets up the reference broker from the provided config map.
// Recognized keys: hub_url, hmac_key, broker_id, plugin_name.
func (b *RefBroker) Configure(config map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if v, ok := config["hub_url"]; ok {
		b.hubURL = v
	}
	if v, ok := config["hmac_key"]; ok {
		b.hmacKey = v
	}
	if v, ok := config["broker_id"]; ok {
		b.brokerID = v
	}
	if v, ok := config["plugin_name"]; ok {
		b.pluginName = v
	}

	b.log.Info("Reference broker configured",
		"hub_url", b.hubURL,
		"broker_id", b.brokerID,
		"plugin_name", b.pluginName,
	)
	return nil
}

// Publish sends a message to all matching subscribers. Messages are tagged with
// an origin marker so the plugin can filter echoes on inbound delivery.
func (b *RefBroker) Publish(_ context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return fmt.Errorf("reference broker is closed")
	}

	pm := publishedMessage{topic: topic, msg: msg}

	for _, sub := range b.subs {
		if subjectMatchesPattern(sub.pattern, topic) {
			select {
			case sub.ch <- pm:
			default:
				b.log.Warn("Message dropped: subscriber buffer full",
					"pattern", sub.pattern, "topic", topic)
			}
		}
	}

	return nil
}

// Subscribe registers a subscription for the given pattern. When messages
// arrive matching the pattern, they are delivered via the hub API callback
// (or InboundHandler if set). Messages with the scion origin marker are
// filtered out to prevent echo loops.
func (b *RefBroker) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("reference broker is closed")
	}

	if _, exists := b.subs[pattern]; exists {
		return nil // already subscribed
	}

	sub := &subscription{
		pattern: pattern,
		ch:      make(chan publishedMessage, defaultBufferSize),
		done:    make(chan struct{}),
	}

	go b.dispatchLoop(sub)

	b.subs[pattern] = sub
	b.log.Debug("Subscription registered", "pattern", pattern)
	return nil
}

// dispatchLoop reads messages from the subscription channel and delivers them
// via the hub API or InboundHandler. It filters messages with the origin marker.
func (b *RefBroker) dispatchLoop(sub *subscription) {
	defer close(sub.done)
	for pm := range sub.ch {
		// Echo filtering: skip messages that originated from the hub
		if isEcho(pm.msg) {
			b.log.Debug("Filtered echo message", "topic", pm.topic)
			continue
		}
		b.deliverInbound(pm.topic, pm.msg)
	}
}

// isEcho returns true if the message was tagged with the scion origin marker,
// indicating it originated from the hub and should not be re-delivered.
func isEcho(msg *messages.StructuredMessage) bool {
	if msg == nil {
		return false
	}
	// Check sender prefix for the origin marker convention.
	// The hub tags outbound messages with a sender prefixed by the origin marker.
	return strings.HasPrefix(msg.Sender, OriginMarkerKey+":"+OriginMarkerValue+":")
}

// deliverInbound sends a message to the hub API or InboundHandler.
func (b *RefBroker) deliverInbound(topic string, msg *messages.StructuredMessage) {
	// Prefer the in-process handler if set (testing mode)
	if b.InboundHandler != nil {
		b.InboundHandler(topic, msg)
		return
	}

	b.mu.RLock()
	hubURL := b.hubURL
	hmacKey := b.hmacKey
	brokerID := b.brokerID
	pluginName := b.pluginName
	b.mu.RUnlock()

	if hubURL == "" {
		b.log.Debug("No hub URL configured, dropping inbound message", "topic", topic)
		return
	}

	payload := inboundPayload{
		Topic:   topic,
		Message: msg,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		b.log.Error("Failed to marshal inbound message", "error", err)
		return
	}

	url := hubURL + "/api/v1/broker/inbound"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		b.log.Error("Failed to create inbound request", "error", err)
		return
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Plugin-Name", pluginName)

	// Sign the request with HMAC if broker credentials are configured
	if brokerID != "" && hmacKey != "" {
		secretKey, decErr := decodeBase64(hmacKey)
		if decErr != nil {
			b.log.Error("Failed to decode HMAC key", "error", decErr)
			return
		}
		auth := &apiclient.HMACAuth{
			BrokerID:  brokerID,
			SecretKey: secretKey,
		}
		if signErr := auth.ApplyAuth(req); signErr != nil {
			b.log.Error("Failed to sign inbound request", "error", signErr)
			return
		}
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.log.Error("Failed to deliver inbound message", "error", err, "topic", topic)
		return
	}
	_ = resp.Body.Close()

	if resp.StatusCode >= 400 {
		b.log.Error("Hub rejected inbound message",
			"status", resp.StatusCode, "topic", topic)
	}
}

// Unsubscribe removes the subscription for the given pattern.
func (b *RefBroker) Unsubscribe(pattern string) error {
	b.mu.Lock()
	sub, exists := b.subs[pattern]
	if !exists {
		b.mu.Unlock()
		return nil
	}
	delete(b.subs, pattern)
	b.mu.Unlock()

	close(sub.ch)
	<-sub.done
	b.log.Debug("Subscription removed", "pattern", pattern)
	return nil
}

// Close shuts down the reference broker and all subscriptions.
func (b *RefBroker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true

	// Collect subs to close
	subs := make([]*subscription, 0, len(b.subs))
	for _, sub := range b.subs {
		subs = append(subs, sub)
	}
	b.subs = make(map[string]*subscription)
	b.mu.Unlock()

	// Close all channels and wait for goroutines
	for _, sub := range subs {
		close(sub.ch)
	}
	for _, sub := range subs {
		<-sub.done
	}

	b.log.Info("Reference broker closed")
	return nil
}

// GetInfo returns plugin metadata.
func (b *RefBroker) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:         "refbroker",
		Version:      "0.1.0",
		Capabilities: []string{"echo-filter", "in-memory"},
	}, nil
}

// HealthCheck returns the runtime health of the reference broker.
func (b *RefBroker) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return &plugin.HealthStatus{
			Status:  "unhealthy",
			Message: "broker is closed",
		}, nil
	}

	details := map[string]string{
		"subscriptions": fmt.Sprintf("%d", len(b.subs)),
	}
	if b.hubURL != "" {
		details["hub_url"] = b.hubURL
	}

	return &plugin.HealthStatus{
		Status:  "healthy",
		Message: "in-memory broker operational",
		Details: details,
	}, nil
}

// PublishExternal publishes a message as if it came from an external source
// (i.e., without the origin marker). This is used by the REPL and tests to
// simulate external messages arriving into the broker.
func (b *RefBroker) PublishExternal(topic string, msg *messages.StructuredMessage) error {
	return b.Publish(context.Background(), topic, msg)
}

// decodeBase64 decodes a base64-encoded string, trying standard then URL-safe encoding.
func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("invalid base64 encoding")
}

// subjectMatchesPattern checks if a subject matches a NATS-style pattern.
// '*' matches exactly one token, '>' matches one or more remaining tokens.
func subjectMatchesPattern(pattern, subject string) bool {
	patternParts := strings.Split(pattern, ".")
	subjectParts := strings.Split(subject, ".")

	for i, pp := range patternParts {
		if pp == ">" {
			return i < len(subjectParts)
		}
		if i >= len(subjectParts) {
			return false
		}
		if pp == "*" {
			continue
		}
		if pp != subjectParts[i] {
			return false
		}
	}

	return len(patternParts) == len(subjectParts)
}
