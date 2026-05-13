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

// Package telegram implements a Telegram bot message broker plugin for Scion.
// It provides bidirectional messaging between Telegram chats and Scion agents:
//   - Outbound: Hub publishes StructuredMessages which are formatted and sent
//     to Telegram chats via the Bot API.
//   - Inbound: Telegram messages received via long-polling are converted to
//     StructuredMessages and forwarded to the hub's inbound endpoint.
package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

const (
	// OriginMarkerKey is the config key injected into outbound messages
	// to identify messages originating from the scion hub.
	OriginMarkerKey = "scion_origin"

	// OriginMarkerValue is the marker value for hub-originated messages.
	OriginMarkerValue = "hub"

	// defaultPollBackoff is the backoff duration after a polling error.
	defaultPollBackoff = 5 * time.Second

	// dedupTTL is how long a message ID is remembered for deduplication.
	dedupTTL = 5 * time.Minute
)

// inboundPayload is the JSON body sent to the hub API inbound endpoint.
type inboundPayload struct {
	Topic   string                      `json:"topic"`
	Message *messages.StructuredMessage `json:"message"`
}

// TelegramBroker implements plugin.MessageBrokerPluginInterface as a Telegram
// Bot API message broker. It supports:
//   - Outbound message delivery to Telegram chats via the Bot API
//   - Inbound message reception via long-polling with echo filtering
//   - Chat-to-topic routing via configurable mappings
//   - Hub API callback for inbound message delivery with HMAC auth
type TelegramBroker struct {
	mu     sync.RWMutex
	closed bool
	log    *slog.Logger

	// Telegram API client
	api     *TelegramAPIClient
	botInfo *BotUser

	// Hub API callback config (set via Configure)
	hubURL     string
	hmacKey    string
	brokerID   string
	pluginName string

	httpClient *http.Client

	// Subscription management
	subs map[string]bool // pattern -> active

	// Chat-topic routing
	chatRoutes map[int64]string   // chatID -> topic
	topicChats map[string][]int64 // topic -> chatIDs (inverse of chatRoutes)

	// User identity mapping: Telegram user ID -> scion user (e.g. "user:ptone@google.com")
	userMappings map[string]string

	// Registration server and config
	regServer    *registrationServer
	registerURL  string // external URL for registration links
	registerAddr string // HTTP listen address
	mappingsFile string // path to persisted mappings JSON

	// Long polling state
	pollCancel context.CancelFunc
	pollDone   chan struct{}
	lastOffset int64

	// Outbound deduplication: prevents sending the same message twice
	// when the broker delivers duplicates within a short window.
	sentIDs   map[string]time.Time
	sentIDsMu sync.Mutex

	// InboundHandler is an optional callback for inbound messages.
	// When set, messages are delivered here instead of via the hub API.
	// This is used for in-process testing without a running hub.
	InboundHandler func(topic string, msg *messages.StructuredMessage)
}

// New creates a new TelegramBroker with the given logger.
func New(log *slog.Logger) *TelegramBroker {
	if log == nil {
		log = slog.Default()
	}
	return &TelegramBroker{
		subs:       make(map[string]bool),
		chatRoutes: make(map[int64]string),
		topicChats: make(map[string][]int64),
		sentIDs:    make(map[string]time.Time),
		log:        log,
		pluginName: "telegram",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Configure sets up the Telegram broker from the provided config map.
// Recognized keys: bot_token (required), hub_url, hmac_key, broker_id,
// plugin_name, chat_routes (JSON), user_mappings (JSON), api_base_url (for testing).
func (b *TelegramBroker) Configure(config map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Store hub config (same pattern as refbroker)
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

	// Bot token is required
	botToken, ok := config["bot_token"]
	if !ok || botToken == "" {
		return fmt.Errorf("bot_token is required")
	}

	// Allow overriding the API base URL for testing
	baseURL := config["api_base_url"]

	b.api = NewAPIClient(botToken, baseURL)

	// Parse chat routes if provided
	if routesJSON, ok := config["chat_routes"]; ok && routesJSON != "" {
		if err := b.parseChatRoutes(routesJSON); err != nil {
			return fmt.Errorf("invalid chat_routes: %w", err)
		}
	}

	// Parse outbound routes if provided (topic pattern → chat ID, reverse of chat_routes)
	if outboundJSON, ok := config["outbound_routes"]; ok && outboundJSON != "" {
		if err := b.parseOutboundRoutes(outboundJSON); err != nil {
			return fmt.Errorf("invalid outbound_routes: %w", err)
		}
	}

	// Parse user mappings if provided
	if mappingsJSON, ok := config["user_mappings"]; ok && mappingsJSON != "" {
		var raw map[string]string
		if err := json.Unmarshal([]byte(mappingsJSON), &raw); err != nil {
			return fmt.Errorf("invalid user_mappings: %w", err)
		}
		b.userMappings = raw
	}

	// Registration config
	if v, ok := config["register_addr"]; ok && v != "" {
		b.registerAddr = v
	}
	if v, ok := config["register_url"]; ok && v != "" {
		b.registerURL = v
	}
	if v, ok := config["mappings_file"]; ok && v != "" {
		b.mappingsFile = v
	}

	// Load persisted mappings (merges with static config, static takes precedence)
	if b.mappingsFile != "" {
		if err := b.loadUserMappings(b.mappingsFile); err != nil {
			b.log.Warn("Failed to load user mappings file", "path", b.mappingsFile, "error", err)
		}
	}

	// Validate the bot token by calling getMe
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bot, err := b.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("failed to validate bot token: %w", err)
	}
	b.botInfo = bot

	// Start registration server if an address is configured
	if b.registerAddr != "" {
		b.regServer = newRegistrationServer(b)
		actualAddr, err := b.regServer.start(b.registerAddr)
		if err != nil {
			return fmt.Errorf("start registration server: %w", err)
		}
		if b.registerURL == "" {
			b.registerURL = "http://" + actualAddr
		}
		b.log.Info("Registration server started", "addr", actualAddr, "url", b.registerURL)
	}

	b.log.Info("Telegram broker configured",
		"bot_username", bot.Username,
		"bot_id", bot.ID,
		"hub_url", b.hubURL,
		"broker_id", b.brokerID,
		"chat_routes", len(b.chatRoutes),
	)
	return nil
}

// parseChatRoutes parses a JSON string mapping chat IDs to topic patterns.
// Expected format: {"123456789": "scion.project.myproj.agent.coder.messages"}
func (b *TelegramBroker) parseChatRoutes(routesJSON string) error {
	var raw map[string]string
	if err := json.Unmarshal([]byte(routesJSON), &raw); err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}

	b.chatRoutes = make(map[int64]string, len(raw))
	b.topicChats = make(map[string][]int64)

	for chatIDStr, topic := range raw {
		chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid chat ID %q: %w", chatIDStr, err)
		}
		b.chatRoutes[chatID] = topic
		b.topicChats[topic] = append(b.topicChats[topic], chatID)
	}

	return nil
}

// parseOutboundRoutes parses a JSON string mapping topic patterns to chat IDs.
// This is the reverse of chat_routes: it allows routing outbound messages
// (e.g. user-topic replies) to specific Telegram chats.
// Expected format: {"scion.project.*.user.*.messages": "-5242408331"}
func (b *TelegramBroker) parseOutboundRoutes(routesJSON string) error {
	var raw map[string]string
	if err := json.Unmarshal([]byte(routesJSON), &raw); err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}

	for topicPattern, chatIDStr := range raw {
		chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid chat ID %q for pattern %q: %w", chatIDStr, topicPattern, err)
		}
		b.topicChats[topicPattern] = append(b.topicChats[topicPattern], chatID)
	}

	return nil
}

// Publish sends a message to Telegram chats matching the topic. Messages are
// routed to chats via:
//  1. Direct chat ID from message metadata (telegram_chat_id)
//  2. Topic-to-chat mapping configured via chat_routes
func (b *TelegramBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return fmt.Errorf("telegram broker is closed")
	}
	api := b.api
	chatRoutes := b.chatRoutes
	topicChats := b.topicChats
	b.mu.RUnlock()

	if api == nil {
		return fmt.Errorf("telegram broker not configured")
	}

	// Dedup: skip if we've already sent this exact message recently.
	dedupKey := msgDedupKey(msg)
	if dedupKey != "" {
		b.sentIDsMu.Lock()
		if t, ok := b.sentIDs[dedupKey]; ok && time.Since(t) < dedupTTL {
			b.sentIDsMu.Unlock()
			b.log.Debug("Skipping duplicate message", "topic", topic, "dedup_key", dedupKey)
			return nil
		}
		b.sentIDs[dedupKey] = time.Now()
		b.pruneSentIDsLocked()
		b.sentIDsMu.Unlock()
	}

	// Format the message for Telegram
	text := FormatMessage(msg)
	if text == "" {
		return nil
	}

	// Collect target chat IDs
	var chatIDs []int64

	// Priority 1: Direct routing via metadata
	if msg != nil && msg.Metadata != nil {
		if chatIDStr, ok := msg.Metadata["telegram_chat_id"]; ok {
			if chatID, err := strconv.ParseInt(chatIDStr, 10, 64); err == nil {
				chatIDs = append(chatIDs, chatID)
			}
		}
	}

	// Priority 2: Topic-to-chat mapping
	if len(chatIDs) == 0 {
		// Try exact match first
		if chats, ok := topicChats[topic]; ok {
			chatIDs = append(chatIDs, chats...)
		}

		// Try pattern matching if no exact match
		if len(chatIDs) == 0 {
			for routeTopic, chats := range topicChats {
				if subjectMatchesPattern(routeTopic, topic) {
					chatIDs = append(chatIDs, chats...)
				}
			}
		}

		// Try reverse lookup: check if any chatRoute pattern matches
		if len(chatIDs) == 0 {
			for chatID, routePattern := range chatRoutes {
				if subjectMatchesPattern(routePattern, topic) {
					chatIDs = append(chatIDs, chatID)
				}
			}
		}
	}

	if len(chatIDs) == 0 {
		b.log.Debug("No Telegram chat mapped for topic, dropping message", "topic", topic)
		return nil
	}

	// Send to each matching chat.
	// Transient Telegram API errors (429 rate limits, 5xx) are logged and
	// swallowed — propagating them would cause the caller to retry and
	// amplify the problem into a message storm.
	var errs []error
	for _, chatID := range chatIDs {
		if _, err := api.SendMessage(ctx, chatID, text, ""); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.IsTransient() {
				b.log.Warn("Transient Telegram API error, dropping message",
					"chat_id", chatID, "topic", topic,
					"code", apiErr.Code, "retry_after_sec", apiErr.RetryAfterSec,
					"error", err)
				continue
			}
			b.log.Error("Failed to send Telegram message",
				"chat_id", chatID, "topic", topic, "error", err)
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Subscribe registers a subscription pattern. When the first subscription is
// added, the long-polling goroutine is started to receive inbound Telegram
// messages. The pattern is stored as a hint but the Telegram transport does
// not support topic filtering — all messages from all chats are received.
func (b *TelegramBroker) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("telegram broker is closed")
	}

	if b.subs[pattern] {
		return nil // already subscribed
	}

	wasEmpty := len(b.subs) == 0
	b.subs[pattern] = true

	// Start polling on first subscription
	if wasEmpty && b.api != nil {
		b.startPolling()
	}

	b.log.Debug("Subscription registered", "pattern", pattern)
	return nil
}

// Unsubscribe removes a subscription pattern. When all subscriptions are
// removed, the long-polling goroutine is stopped.
func (b *TelegramBroker) Unsubscribe(pattern string) error {
	b.mu.Lock()

	if !b.subs[pattern] {
		b.mu.Unlock()
		return nil
	}

	delete(b.subs, pattern)
	shouldStop := len(b.subs) == 0

	b.mu.Unlock()

	if shouldStop {
		b.stopPolling()
	}

	b.log.Debug("Subscription removed", "pattern", pattern)
	return nil
}

// Close shuts down the Telegram broker, stopping the polling goroutine,
// registration server, and releasing resources.
func (b *TelegramBroker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.subs = make(map[string]bool)
	regServer := b.regServer
	b.mu.Unlock()

	b.stopPolling()

	if regServer != nil {
		regServer.stop()
	}

	b.log.Info("Telegram broker closed")
	return nil
}

// GetInfo returns plugin metadata.
func (b *TelegramBroker) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:         "telegram",
		Version:      "0.1.0",
		Capabilities: []string{"echo-filter", "long-polling", "telegram-bot-api", "user-registration"},
	}, nil
}

// HealthCheck returns the runtime health of the Telegram broker.
func (b *TelegramBroker) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return &plugin.HealthStatus{
			Status:  "unhealthy",
			Message: "broker is closed",
		}, nil
	}

	if b.api == nil || b.botInfo == nil {
		return &plugin.HealthStatus{
			Status:  "degraded",
			Message: "broker not configured",
		}, nil
	}

	details := map[string]string{
		"subscriptions": fmt.Sprintf("%d", len(b.subs)),
		"bot_username":  "@" + b.botInfo.Username,
		"bot_id":        strconv.FormatInt(b.botInfo.ID, 10),
	}
	if b.hubURL != "" {
		details["hub_url"] = b.hubURL
	}

	return &plugin.HealthStatus{
		Status:  "healthy",
		Message: "telegram bot operational",
		Details: details,
	}, nil
}

// --- Long polling ---

// startPolling starts the long-polling goroutine. Must be called with mu held.
func (b *TelegramBroker) startPolling() {
	if b.pollCancel != nil {
		return // already polling
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.pollCancel = cancel
	b.pollDone = make(chan struct{})

	go b.pollLoop(ctx)
	b.log.Info("Telegram polling started")
}

// stopPolling stops the long-polling goroutine and waits for it to finish.
func (b *TelegramBroker) stopPolling() {
	b.mu.RLock()
	cancel := b.pollCancel
	done := b.pollDone
	b.mu.RUnlock()

	if cancel == nil {
		return
	}

	cancel()
	if done != nil {
		<-done
	}

	b.mu.Lock()
	b.pollCancel = nil
	b.pollDone = nil
	b.mu.Unlock()
}

// pollLoop continuously polls the Telegram Bot API for updates and processes
// incoming messages. It runs until the context is canceled.
func (b *TelegramBroker) pollLoop(ctx context.Context) {
	defer close(b.pollDone)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, err := b.api.GetUpdates(ctx, b.lastOffset+1, longPollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return // context canceled, clean shutdown
			}
			b.log.Error("getUpdates failed", "error", err)
			select {
			case <-time.After(defaultPollBackoff):
			case <-ctx.Done():
				return
			}
			continue
		}

		for _, update := range updates {
			b.lastOffset = update.UpdateID
			if update.Message != nil {
				b.handleIncomingMessage(update.Message)
			}
		}
	}
}

// handleIncomingMessage processes a Telegram message, converts it to a
// StructuredMessage, and delivers it to the hub.
func (b *TelegramBroker) handleIncomingMessage(tgMsg *TGMessage) {
	if tgMsg.Text == "" {
		return // skip non-text messages
	}

	// Echo filtering: skip messages from the bot itself
	b.mu.RLock()
	botInfo := b.botInfo
	b.mu.RUnlock()

	if botInfo != nil && tgMsg.From != nil && tgMsg.From.ID == botInfo.ID {
		b.log.Debug("Filtered echo message from bot", "message_id", tgMsg.MessageID)
		return
	}

	// Handle bot commands (/register, /unregister)
	if strings.HasPrefix(tgMsg.Text, "/") && b.handleBotCommand(tgMsg) {
		return
	}

	// Determine sender identity
	sender := "telegram:unknown"
	senderID := ""
	if tgMsg.From != nil {
		senderID = strconv.FormatInt(tgMsg.From.ID, 10)
		if tgMsg.From.Username != "" {
			sender = "telegram:" + tgMsg.From.Username
		} else {
			sender = "telegram:" + senderID
		}
	}

	// Check user mappings for a scion identity override
	b.mu.RLock()
	if senderID != "" && b.userMappings != nil {
		if mapped, ok := b.userMappings[senderID]; ok {
			sender = "user:" + mapped
		}
	}

	// Map chat to topic
	topic, ok := b.chatRoutes[tgMsg.Chat.ID]
	b.mu.RUnlock()

	if !ok {
		// Default topic based on chat ID
		topic = fmt.Sprintf("scion.telegram.chat.%d.messages", tgMsg.Chat.ID)
	}

	// Determine recipient from topic
	recipient := recipientFromTopic(topic)

	// Build StructuredMessage
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Unix(tgMsg.Date, 0).UTC().Format(time.RFC3339),
		Sender:    sender,
		SenderID:  senderID,
		Recipient: recipient,
		Msg:       tgMsg.Text,
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"telegram_chat_id":    strconv.FormatInt(tgMsg.Chat.ID, 10),
			"telegram_message_id": strconv.FormatInt(tgMsg.MessageID, 10),
		},
	}

	// Check for echo via origin marker (same as refbroker)
	if isEcho(msg) {
		b.log.Debug("Filtered echo message via origin marker", "topic", topic)
		return
	}

	b.log.Debug("Delivering inbound Telegram message",
		"topic", topic, "sender", sender, "telegram_user_id", senderID)

	b.deliverInbound(topic, msg)
}

// recipientFromTopic extracts a recipient identity from a topic string.
// For example, "scion.project.myproj.agent.coder.messages" yields "agent:coder".
func recipientFromTopic(topic string) string {
	parts := strings.Split(topic, ".")
	for i, part := range parts {
		if part == "agent" && i+1 < len(parts) {
			return "agent:" + parts[i+1]
		}
		if part == "user" && i+1 < len(parts) {
			return "user:" + parts[i+1]
		}
	}
	return "broker:topic"
}

// isEcho returns true if the message was tagged with the scion origin marker,
// indicating it originated from the hub and should not be re-delivered.
func isEcho(msg *messages.StructuredMessage) bool {
	if msg == nil {
		return false
	}
	return strings.HasPrefix(msg.Sender, OriginMarkerKey+":"+OriginMarkerValue+":")
}

// --- Hub delivery (same pattern as refbroker) ---

// deliverInbound sends a message to the hub API or InboundHandler.
func (b *TelegramBroker) deliverInbound(topic string, msg *messages.StructuredMessage) {
	b.mu.RLock()
	handler := b.InboundHandler
	hubURL := b.hubURL
	hmacKey := b.hmacKey
	brokerID := b.brokerID
	pluginName := b.pluginName
	b.mu.RUnlock()

	// Prefer the in-process handler if set (testing mode)
	if handler != nil {
		handler(topic, msg)
		return
	}

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
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		b.log.Error("Hub rejected inbound message",
			"status", resp.StatusCode, "topic", topic)
	}
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

// msgDedupKey returns a stable fingerprint for a message, used to detect
// duplicate deliveries of the same logical message. Returns "" if the
// message is nil or has no content to fingerprint.
func msgDedupKey(msg *messages.StructuredMessage) string {
	if msg == nil || msg.Msg == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(msg.Sender))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Recipient)) // include recipient so set[] broadcasts to N agents aren't deduped
	h.Write([]byte("|"))
	h.Write([]byte(msg.Timestamp))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Type))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Msg))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// pruneSentIDsLocked removes dedup entries older than dedupTTL.
// Must be called with sentIDsMu held.
func (b *TelegramBroker) pruneSentIDsLocked() {
	now := time.Now()
	for k, t := range b.sentIDs {
		if now.Sub(t) > dedupTTL {
			delete(b.sentIDs, k)
		}
	}
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
