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

// scion-broker-log is a minimal Scion message broker plugin that logs all
// messages flowing through the broker. It serves as both a reference
// implementation of the broker plugin interface and a debugging tool for
// inspecting message traffic.
//
// Usage:
//
//	scion-broker-log [flags]
//
// Hub configuration (server.yaml):
//
//	server:
//	  message_broker:
//	    enabled: true
//	    type: "broker-log"
//	  plugins:
//	    broker:
//	      broker-log:
//	        self_managed: true
//	        address: "localhost:9091"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	goplugin "github.com/hashicorp/go-plugin"
)

// CLI flags
var (
	flagAddr    = flag.String("addr", "localhost:9091", "RPC listen address")
	flagTopic   = flag.String("topic", "scion.>", "Subscription pattern (NATS-style wildcards: * = one token, > = remainder)")
	flagJSON    = flag.Bool("json", false, "Output JSON Lines instead of human-readable format")
	flagFullMsg = flag.Bool("full-msg", false, "Show full message body (default truncates to 120 chars)")
	flagFields  = flag.String("fields", "", "Comma-separated fields to include (e.g. topic,sender,type,msg). Empty = all")
)

func main() {
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fieldSet := parseFieldSet(*flagFields)

	bl := &brokerLog{
		log:           log,
		topicPattern:  *flagTopic,
		jsonOutput:    *flagJSON,
		fullMsg:       *flagFullMsg,
		fields:        fieldSet,
		subscriptions: make(map[string]bool),
	}

	listener, err := net.Listen("tcp", *flagAddr)
	if err != nil {
		log.Error("failed to listen", "addr", *flagAddr, "error", err)
		os.Exit(1)
	}

	pluginMap := map[string]goplugin.Plugin{
		plugin.BrokerPluginName: &plugin.BrokerPlugin{Impl: bl},
	}

	stdoutR, _ := io.Pipe()
	stderrR, _ := io.Pipe()

	rpcServer := &goplugin.RPCServer{
		Plugins: pluginMap,
		Stdout:  stdoutR,
		Stderr:  stderrR,
		DoneCh:  make(chan struct{}),
	}

	go rpcServer.Serve(listener)

	log.Info("broker-log started", "addr", listener.Addr().String(), "topic", *flagTopic)
	if *flagJSON {
		log.Info("output format: JSON Lines")
	} else {
		log.Info("output format: human-readable")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Info("shutting down", "signal", sig, "messages_logged", bl.msgCount.Load())
	listener.Close()
}

// brokerLog implements plugin.MessageBrokerPluginInterface and plugin.HostCallbacksAware.
// On each Publish() call it formats the message and writes it to stdout.
type brokerLog struct {
	log           *slog.Logger
	topicPattern  string
	jsonOutput    bool
	fullMsg       bool
	fields        map[string]bool // nil = all fields
	hostCallbacks plugin.HostCallbacks
	mu            sync.RWMutex
	subscriptions map[string]bool
	configured    bool
	msgCount      atomic.Int64
}

var _ plugin.MessageBrokerPluginInterface = (*brokerLog)(nil)
var _ plugin.HostCallbacksAware = (*brokerLog)(nil)

func (b *brokerLog) Configure(config map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.configured = true

	keys := make([]string, 0, len(config))
	for k := range config {
		if !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	b.log.Info("configured", "config_keys", keys)
	return nil
}

func (b *brokerLog) Publish(_ context.Context, topic string, msg *messages.StructuredMessage) error {
	b.msgCount.Add(1)
	if b.jsonOutput {
		writeJSONLine(topic, msg, b.fullMsg, b.fields)
	} else {
		writeHumanLine(topic, msg, b.fullMsg, b.fields)
	}
	return nil
}

func (b *brokerLog) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscriptions[pattern] = true
	b.log.Info("hub subscribed us to pattern", "pattern", pattern)
	return nil
}

func (b *brokerLog) Unsubscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscriptions, pattern)
	b.log.Info("hub unsubscribed us from pattern", "pattern", pattern)
	return nil
}

func (b *brokerLog) Close() error {
	b.log.Info("close requested", "messages_logged", b.msgCount.Load())
	return nil
}

func (b *brokerLog) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:         "scion-broker-log",
		Version:      "0.1.0",
		Capabilities: []string{"observer"},
	}, nil
}

func (b *brokerLog) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	status := "healthy"
	msg := "broker-log operational"
	if !b.configured {
		status = "degraded"
		msg = "not yet configured by hub"
	}

	return &plugin.HealthStatus{
		Status:  status,
		Message: msg,
		Details: map[string]string{
			"messages_logged": fmt.Sprintf("%d", b.msgCount.Load()),
		},
	}, nil
}

// SetHostCallbacks receives the reverse RPC channel from the hub and requests
// subscriptions for the configured topic pattern.
func (b *brokerLog) SetHostCallbacks(hc plugin.HostCallbacks) {
	b.mu.Lock()
	b.hostCallbacks = hc
	b.mu.Unlock()

	b.log.Info("host callbacks connected, requesting subscription", "pattern", b.topicPattern)

	go func() {
		for i := 0; i < 10; i++ {
			err := hc.RequestSubscription(b.topicPattern)
			if err == nil {
				b.log.Info("subscribed", "pattern", b.topicPattern)
				return
			}
			if err.Error() == "host callbacks not yet available" {
				b.log.Debug("host callbacks not ready, retrying", "attempt", i+1)
				time.Sleep(time.Second)
				continue
			}
			b.log.Error("failed to request subscription", "pattern", b.topicPattern, "error", err)
			return
		}
		b.log.Error("gave up requesting subscription after 10 attempts", "pattern", b.topicPattern)
	}()
}

// --- Output formatting ---

func writeHumanLine(topic string, msg *messages.StructuredMessage, fullMsg bool, fields map[string]bool) {
	ts := time.Now().Format("15:04:05.000")
	var b strings.Builder

	b.WriteString(ts)
	b.WriteString(" PUB ")

	if includeField(fields, "topic") {
		b.WriteString(topic)
	}
	b.WriteByte('\n')

	if includeField(fields, "sender") || includeField(fields, "recipient") {
		b.WriteString("  ")
		if includeField(fields, "sender") {
			b.WriteString("sender=")
			b.WriteString(msg.Sender)
			if msg.SenderID != "" {
				fmt.Fprintf(&b, " (%s)", msg.SenderID)
			}
		}
		if includeField(fields, "sender") && includeField(fields, "recipient") {
			b.WriteString(" → ")
		}
		if includeField(fields, "recipient") {
			b.WriteString("recipient=")
			b.WriteString(msg.Recipient)
			if msg.RecipientID != "" {
				fmt.Fprintf(&b, " (%s)", msg.RecipientID)
			}
		}
		b.WriteByte('\n')
	}

	if includeField(fields, "type") || includeField(fields, "status") {
		b.WriteString("  ")
		if includeField(fields, "type") {
			fmt.Fprintf(&b, "type=%s", msg.Type)
		}
		flags := flagsSummary(msg)
		if flags != "" {
			b.WriteString("  ")
			b.WriteString(flags)
		}
		if includeField(fields, "status") && msg.Status != "" {
			fmt.Fprintf(&b, "  status=%s", msg.Status)
		}
		b.WriteByte('\n')
	}

	if includeField(fields, "msg") && msg.Msg != "" {
		body := msg.Msg
		bodyLen := len(body)
		if !fullMsg && len(body) > 120 {
			body = body[:117] + "..."
		}
		body = strings.ReplaceAll(body, "\n", "\\n")
		fmt.Fprintf(&b, "  msg=%q [%d bytes]\n", body, bodyLen)
	}

	if includeField(fields, "attachments") && len(msg.Attachments) > 0 {
		fmt.Fprintf(&b, "  attachments=%v\n", msg.Attachments)
	}

	os.Stdout.WriteString(b.String())
}

type jsonEntry struct {
	Timestamp   string   `json:"ts"`
	Topic       string   `json:"topic,omitempty"`
	Sender      string   `json:"sender,omitempty"`
	SenderID    string   `json:"sender_id,omitempty"`
	Recipient   string   `json:"recipient,omitempty"`
	RecipientID string   `json:"recipient_id,omitempty"`
	Type        string   `json:"type,omitempty"`
	Plain       bool     `json:"plain,omitempty"`
	Raw         bool     `json:"raw,omitempty"`
	Urgent      bool     `json:"urgent,omitempty"`
	Broadcasted bool     `json:"broadcasted,omitempty"`
	Status      string   `json:"status,omitempty"`
	MsgLen      int      `json:"msg_len,omitempty"`
	Msg         string   `json:"msg,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
}

func writeJSONLine(topic string, msg *messages.StructuredMessage, fullMsg bool, fields map[string]bool) {
	e := jsonEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}

	if includeField(fields, "topic") {
		e.Topic = topic
	}
	if includeField(fields, "sender") {
		e.Sender = msg.Sender
		e.SenderID = msg.SenderID
	}
	if includeField(fields, "recipient") {
		e.Recipient = msg.Recipient
		e.RecipientID = msg.RecipientID
	}
	if includeField(fields, "type") {
		e.Type = msg.Type
		e.Plain = msg.Plain
		e.Raw = msg.Raw
		e.Urgent = msg.Urgent
		e.Broadcasted = msg.Broadcasted
	}
	if includeField(fields, "status") {
		e.Status = msg.Status
	}
	if includeField(fields, "msg") {
		e.MsgLen = len(msg.Msg)
		if fullMsg {
			e.Msg = msg.Msg
		} else if len(msg.Msg) > 120 {
			e.Msg = msg.Msg[:117] + "..."
		} else {
			e.Msg = msg.Msg
		}
	}
	if includeField(fields, "attachments") {
		e.Attachments = msg.Attachments
	}

	data, _ := json.Marshal(e)
	os.Stdout.Write(data)
	os.Stdout.WriteString("\n")
}

// --- Helpers ---

func flagsSummary(msg *messages.StructuredMessage) string {
	var parts []string
	if msg.Plain {
		parts = append(parts, "plain")
	}
	if msg.Raw {
		parts = append(parts, "raw")
	}
	if msg.Urgent {
		parts = append(parts, "urgent")
	}
	if msg.Broadcasted {
		parts = append(parts, "broadcasted")
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func parseFieldSet(s string) map[string]bool {
	if s == "" {
		return nil
	}
	fields := make(map[string]bool)
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			fields[f] = true
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func includeField(fields map[string]bool, name string) bool {
	if fields == nil {
		return true
	}
	return fields[name]
}
