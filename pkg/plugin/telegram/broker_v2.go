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

package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
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
	defaultAgentCacheTTL = 5 * time.Minute
	defaultDBPath        = "telegram_v2.db"
	askUserExpiry        = 30 * time.Minute
)

// TelegramBrokerV2 implements plugin.MessageBrokerPluginInterface with
// dynamic group-link routing, inline keyboard support, and persistent
// SQLite state. It wires together the v2 component handlers (commands,
// callbacks, registration, mentions) into a complete broker.
type TelegramBrokerV2 struct {
	mu     sync.RWMutex
	closed bool
	log    *slog.Logger

	api     *TelegramAPIClient
	botInfo *BotUser

	hubURL     string
	hmacKey    string
	brokerID   string
	pluginName string
	httpClient *http.Client

	store Store

	commands     *CommandHandler
	callbacks    *CallbackHandler
	registration *RegistrationHandler
	hubClient    HubClient

	subs map[string]bool

	pollCancel context.CancelFunc
	pollDone   chan struct{}
	lastOffset int64

	sentIDs   map[string]time.Time
	sentIDsMu sync.Mutex

	agentCacheTTL  time.Duration
	projectSlugMap map[string]string // injected by hub: projectID → slug

	InboundHandler func(topic string, msg *messages.StructuredMessage)

	hostCallbacks plugin.HostCallbacks
}

// NewV2 creates a new TelegramBrokerV2 with the given logger.
func NewV2(log *slog.Logger) *TelegramBrokerV2 {
	if log == nil {
		log = slog.Default()
	}
	return &TelegramBrokerV2{
		subs:          make(map[string]bool),
		sentIDs:       make(map[string]time.Time),
		log:           log,
		pluginName:    "telegram",
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		agentCacheTTL: defaultAgentCacheTTL,
	}
}

// SetHostCallbacks implements plugin.HostCallbacksAware, allowing the
// host to inject a reverse-channel for dynamic subscription management.
func (b *TelegramBrokerV2) SetHostCallbacks(hc plugin.HostCallbacks) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hostCallbacks = hc
}

// Configure sets up the v2 Telegram broker from the provided config map.
func (b *TelegramBrokerV2) Configure(config map[string]string) error {
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

	botToken, ok := config["bot_token"]
	if !ok || botToken == "" {
		return fmt.Errorf("bot_token is required")
	}

	baseURL := config["api_base_url"]
	b.api = NewAPIClient(botToken, baseURL)

	// Parse optional agent cache TTL.
	if v, ok := config["agent_cache_ttl"]; ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid agent_cache_ttl: %w", err)
		}
		b.agentCacheTTL = d
	}

	// Initialize SQLite store.
	dbPath := config["db_path"]
	if dbPath == "" {
		dbPath = defaultDBPath
	}
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}
	b.store = store

	// Validate bot token.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bot, err := b.api.GetMe(ctx)
	if err != nil {
		b.store.Close()
		return fmt.Errorf("failed to validate bot token: %w", err)
	}
	b.botInfo = bot

	// Create hub client.
	b.hubClient = NewHTTPHubClient(b.hubURL, b.hmacKey, b.brokerID)

	// Create component handlers.
	b.commands = NewCommandHandler(b.store, b.api, b.hubClient, bot.Username, b.log)
	b.callbacks = NewCallbackHandler(b.store, b.api, b.hubClient, b.log)
	b.registration = NewRegistrationHandler(b.store, b.api, b.hubURL, b.log)

	// Handle v1 migration: import chat routes as group links.
	if routesJSON, ok := config["v1_chat_routes"]; ok && routesJSON != "" {
		b.importV1ChatRoutes(ctx, routesJSON)
	} else if routesJSON, ok := config["chat_routes"]; ok && routesJSON != "" {
		b.importV1ChatRoutes(ctx, routesJSON)
	}

	// Handle v1 migration: import user mappings.
	if mappingsJSON, ok := config["user_mappings"]; ok && mappingsJSON != "" {
		b.importV1UserMappings(ctx, mappingsJSON)
	}

	// Parse hub-injected project slug map (projectID → slug).
	if slugMapJSON, ok := config["project_slug_map"]; ok && slugMapJSON != "" {
		var m map[string]string
		if err := json.Unmarshal([]byte(slugMapJSON), &m); err == nil {
			b.projectSlugMap = m

			projects := make([]ProjectOption, 0, len(m))
			for id, slug := range m {
				projects = append(projects, ProjectOption{ID: id, Slug: slug})
			}
			b.commands.SetProjects(projects)
		}
	}

	// After hub credentials are available, resolve any group link slugs that
	// were stored as UUIDs during the first Configure() call (before hub_url
	// was injected). Run synchronously so errors are visible in startup logs.
	if b.hubURL != "" && len(b.projectSlugMap) > 0 {
		slugCtx, slugCancel := context.WithTimeout(context.Background(), 15*time.Second)
		b.resolveStaleGroupSlugs(slugCtx)
		slugCancel()
	}

	b.log.Info("Telegram v2 broker configured",
		"bot_username", bot.Username,
		"bot_id", bot.ID,
		"hub_url", b.hubURL,
		"broker_id", b.brokerID,
		"db_path", dbPath,
	)
	return nil
}

// resolveStaleGroupSlugs updates GroupLinks where ProjectSlug equals ProjectID
// (i.e., slug was not resolved during initial import). Called after hub credentials
// become available on the second Configure() call.
func (b *TelegramBrokerV2) resolveStaleGroupSlugs(ctx context.Context) {
	// Use the project slug map injected by the hub at configure time.
	// This avoids needing user-level API access from broker credentials.
	if len(b.projectSlugMap) == 0 {
		b.log.Debug("Slug resolution skipped: no project_slug_map injected by hub")
		return
	}
	slugByID := b.projectSlugMap
	b.log.Debug("Slug resolution: using hub-injected project slug map", "count", len(slugByID))

	links, err := b.store.GetAllGroupLinks(ctx)
	if err != nil {
		b.log.Warn("Could not list group links for slug resolution", "error", err)
		return
	}
	for _, link := range links {
		if link.ProjectSlug == link.ProjectID {
			if slug, ok := slugByID[link.ProjectID]; ok {
				link.ProjectSlug = slug
				if err := b.store.SaveGroupLink(ctx, link); err != nil {
					b.log.Warn("Failed to update group link slug", "chat_id", link.ChatID, "error", err)
				} else {
					b.log.Info("Resolved group link project slug", "chat_id", link.ChatID, "project_id", link.ProjectID, "slug", slug)
				}
			}
		}
	}
}

// importV1ChatRoutes parses v1-format chat_routes JSON and creates GroupLinks.
func (b *TelegramBrokerV2) importV1ChatRoutes(ctx context.Context, routesJSON string) {
	var raw map[string]string
	if err := json.Unmarshal([]byte(routesJSON), &raw); err != nil {
		b.log.Warn("Failed to parse v1 chat_routes for migration", "error", err)
		return
	}

	imported := 0
	for chatIDStr, topic := range raw {
		chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err != nil {
			b.log.Warn("Invalid chat ID in v1 migration", "chat_id", chatIDStr, "error", err)
			continue
		}

		existing, err := b.store.GetGroupLink(ctx, chatID)
		if err != nil {
			b.log.Warn("Failed to check existing group link", "chat_id", chatID, "error", err)
			continue
		}
		if existing != nil {
			continue
		}

		projectID, agentSlug := parseTopicComponents(topic)
		// Attempt to resolve the project slug from the hub. Falls back to
		// the project ID if the hub is unavailable during migration.
		projectSlug := projectID
		if b.hubClient != nil {
			if projects, err := b.hubClient.ListProjects(ctx); err == nil {
				for _, p := range projects {
					if p.ID == projectID {
						if p.Slug != "" {
							projectSlug = p.Slug
						} else if p.Name != "" {
							projectSlug = p.Name
						}
						break
					}
				}
			}
		}
		link := &GroupLink{
			ChatID:       chatID,
			ProjectID:    projectID,
			ProjectSlug:  projectSlug,
			DefaultAgent: agentSlug,
			LinkedBy:     "v1-migration",
			LinkedAt:     time.Now(),
			Active:       true,
		}
		if err := b.store.SaveGroupLink(ctx, link); err != nil {
			b.log.Warn("Failed to import v1 chat route", "chat_id", chatID, "error", err)
			continue
		}
		imported++
	}
	if imported > 0 {
		b.log.Info("Imported v1 chat routes as group links", "imported", imported)
	}
}

// importV1UserMappings parses v1-format user_mappings JSON and imports them.
func (b *TelegramBrokerV2) importV1UserMappings(ctx context.Context, mappingsJSON string) {
	var raw map[string]string
	if err := json.Unmarshal([]byte(mappingsJSON), &raw); err != nil {
		b.log.Warn("Failed to parse v1 user_mappings for migration", "error", err)
		return
	}
	if err := b.registration.ImportV1Mappings(ctx, raw); err != nil {
		b.log.Warn("V1 user mapping import had errors", "error", err)
	}
}

// parseTopicComponents extracts projectID and agentSlug from a topic string.
// Example: "scion.grove.myproj.agent.coder.messages" → ("myproj", "coder")
func parseTopicComponents(topic string) (projectID, agentSlug string) {
	parts := strings.Split(topic, ".")
	for i, part := range parts {
		if part == "grove" && i+1 < len(parts) {
			projectID = parts[i+1]
		}
		if part == "project" && i+1 < len(parts) {
			projectID = parts[i+1]
		}
		if part == "agent" && i+1 < len(parts) {
			agentSlug = parts[i+1]
		}
	}
	if projectID == "" {
		projectID = topic
	}
	return projectID, agentSlug
}

// --- Publish (outbound: Hub → Telegram) ---

// Publish sends a message to Telegram chats using dynamic routing.
func (b *TelegramBrokerV2) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return fmt.Errorf("telegram v2 broker is closed")
	}
	api := b.api
	store := b.store
	b.mu.RUnlock()

	if api == nil {
		return fmt.Errorf("telegram v2 broker not configured")
	}

	// Dedup check.
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

	// Determine the project and agent from the topic.
	projectID, agentSlug := parseTopicComponents(topic)

	// Collect target chat IDs via dynamic routing.
	var chatIDs []int64

	// Priority 1: Direct chat ID from metadata.
	if msg != nil && msg.Metadata != nil {
		if chatIDStr, ok := msg.Metadata["telegram_chat_id"]; ok {
			if chatID, err := strconv.ParseInt(chatIDStr, 10, 64); err == nil {
				chatIDs = append(chatIDs, chatID)
			}
		}
	}

	// Priority 2: Look up via ConversationContext for the recipient.
	if len(chatIDs) == 0 && msg != nil && msg.Recipient != "" && store != nil {
		chatIDs = b.resolveRecipientChats(ctx, msg.Recipient, projectID, agentSlug)
	}

	// Priority 3: Broadcast to all GroupLinks for the project.
	if len(chatIDs) == 0 && projectID != "" && store != nil {
		links, err := store.GetGroupLinksForProject(ctx, projectID)
		if err != nil {
			b.log.Warn("Failed to get group links for broadcast", "project_id", projectID, "error", err)
		}
		for _, link := range links {
			if link.Active {
				chatIDs = append(chatIDs, link.ChatID)
			}
		}
	}

	if len(chatIDs) == 0 {
		b.log.Debug("No Telegram chat for topic, dropping message", "topic", topic)
		return nil
	}

	// Handle InputNeeded messages with inline keyboards.
	if msg != nil && msg.Type == messages.TypeInputNeeded {
		return b.publishInputNeeded(ctx, api, chatIDs, msg, agentSlug, projectID)
	}

	// Format the message for Telegram.
	text := FormatMessageV2(msg, agentSlug)
	if text == "" {
		return nil
	}

	// Determine reply-to if available.
	var replyToMsgID int64
	if msg != nil && msg.Metadata != nil {
		if v, ok := msg.Metadata["telegram_message_id"]; ok {
			replyToMsgID, _ = strconv.ParseInt(v, 10, 64)
		}
	}

	var errs []error
	for _, chatID := range chatIDs {
		var err error
		if replyToMsgID > 0 {
			_, err = api.SendMessageWithKeyboard(ctx, chatID, text, "", nil, replyToMsgID)
		} else {
			_, err = api.SendMessage(ctx, chatID, text, "")
		}
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.IsTransient() {
				b.log.Warn("Transient Telegram API error, dropping message",
					"chat_id", chatID, "topic", topic,
					"code", apiErr.Code, "retry_after_sec", apiErr.RetryAfterSec,
					"error", err)
				continue
			}
			b.log.Error("Failed to send Telegram message",
				"chat_id", chatID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// resolveRecipientChats looks up target chats for a specific recipient.
func (b *TelegramBrokerV2) resolveRecipientChats(ctx context.Context, recipient, projectID, agentSlug string) []int64 {
	// Extract email from "user:email@example.com" format.
	email := strings.TrimPrefix(recipient, "user:")
	if email == recipient {
		return nil
	}

	mapping, err := b.store.GetUserMappingByEmail(ctx, email)
	if err != nil || mapping == nil {
		return nil
	}

	cc, err := b.store.GetConversationContext(ctx, mapping.TelegramUserID, projectID, agentSlug)
	if err != nil || cc == nil {
		return nil
	}

	return []int64{cc.LastChatID}
}

// publishInputNeeded sends an InputNeeded message with an inline keyboard.
func (b *TelegramBrokerV2) publishInputNeeded(ctx context.Context, api *TelegramAPIClient, chatIDs []int64, msg *messages.StructuredMessage, agentSlug, projectID string) error {
	text := FormatMessageV2(msg, agentSlug)
	if text == "" {
		return nil
	}

	// Parse choices from metadata.
	var choices []string
	if msg.Metadata != nil {
		if choicesJSON, ok := msg.Metadata["choices"]; ok && choicesJSON != "" {
			json.Unmarshal([]byte(choicesJSON), &choices)
		}
	}

	requestID := generateRequestID()

	var errs []error
	for _, chatID := range chatIDs {
		keyboard := buildAskUserKeyboard(requestID, choices)
		sent, err := api.SendMessageWithKeyboard(ctx, chatID, text, "", keyboard, 0)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.IsTransient() {
				b.log.Warn("Transient error sending input-needed",
					"chat_id", chatID, "error", err)
				continue
			}
			b.log.Error("Failed to send input-needed message",
				"chat_id", chatID, "error", err)
			errs = append(errs, err)
			continue
		}

		// Save PendingAskUser so the callback handler can match the response.
		pending := &PendingAskUser{
			RequestID: requestID,
			MessageID: sent.MessageID,
			ChatID:    chatID,
			AgentSlug: agentSlug,
			ProjectID: projectID,
			Choices:   choices,
			ExpiresAt: time.Now().Add(askUserExpiry),
		}
		if err := b.store.SavePendingAskUser(ctx, pending); err != nil {
			b.log.Error("Failed to save pending ask user", "error", err)
		}
	}
	return errors.Join(errs...)
}

// --- Subscribe / Unsubscribe / Close ---

func (b *TelegramBrokerV2) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("telegram v2 broker is closed")
	}

	if b.subs[pattern] {
		return nil
	}

	wasEmpty := len(b.subs) == 0
	b.subs[pattern] = true

	if wasEmpty && b.api != nil {
		b.startPolling()
	}

	b.log.Debug("Subscription registered", "pattern", pattern)
	return nil
}

func (b *TelegramBrokerV2) Unsubscribe(pattern string) error {
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

func (b *TelegramBrokerV2) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.subs = make(map[string]bool)
	store := b.store
	b.mu.Unlock()

	b.stopPolling()

	if store != nil {
		store.Close()
	}

	b.log.Info("Telegram v2 broker closed")
	return nil
}

func (b *TelegramBrokerV2) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:         "telegram",
		Version:      "2.0.0",
		Capabilities: []string{"echo-filter", "long-polling", "telegram-bot-api", "user-registration", "inline-keyboards", "group-links", "mention-routing"},
	}, nil
}

func (b *TelegramBrokerV2) HealthCheck() (*plugin.HealthStatus, error) {
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
		"version":       "v2",
		"subscriptions": fmt.Sprintf("%d", len(b.subs)),
		"bot_username":  "@" + b.botInfo.Username,
		"bot_id":        strconv.FormatInt(b.botInfo.ID, 10),
	}
	if b.hubURL != "" {
		details["hub_url"] = b.hubURL
	}

	return &plugin.HealthStatus{
		Status:  "healthy",
		Message: "telegram v2 bot operational",
		Details: details,
	}, nil
}

// --- Long polling ---

func (b *TelegramBrokerV2) startPolling() {
	if b.pollCancel != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.pollCancel = cancel
	b.pollDone = make(chan struct{})

	go b.pollLoop(ctx)
	b.log.Info("Telegram v2 polling started")
}

func (b *TelegramBrokerV2) stopPolling() {
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

func (b *TelegramBrokerV2) pollLoop(ctx context.Context) {
	defer close(b.pollDone)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, err := b.getUpdatesV2(ctx, b.lastOffset+1, longPollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
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
			if update.CallbackQuery != nil {
				b.handleCallbackQuery(ctx, update.CallbackQuery)
			}
			if update.Message != nil {
				b.handleIncomingMessageV2(update.Message)
			}
		}
	}
}

// getUpdatesV2 calls GetUpdates with both "message" and "callback_query"
// in allowed_updates, extending the v1 behavior.
func (b *TelegramBrokerV2) getUpdatesV2(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	body := getUpdatesRequest{
		Offset:         offset,
		Timeout:        timeout,
		AllowedUpdates: []string{"message", "callback_query"},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal getUpdates request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", b.api.methodURL("getUpdates"), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create getUpdates request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.api.pollClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getUpdates request failed: %w", b.api.redactToken(err))
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode getUpdates response: %w", err)
	}

	if !apiResp.OK {
		return nil, fmt.Errorf("getUpdates failed: %s (code %d)", apiResp.Description, apiResp.ErrorCode)
	}

	var updates []Update
	if err := json.Unmarshal(apiResp.Result, &updates); err != nil {
		return nil, fmt.Errorf("unmarshal getUpdates result: %w", err)
	}

	return updates, nil
}

// --- Inbound message handling ---

func (b *TelegramBrokerV2) handleIncomingMessageV2(tgMsg *TGMessage) {
	if tgMsg.Text == "" {
		return
	}

	// Echo filtering.
	b.mu.RLock()
	botInfo := b.botInfo
	b.mu.RUnlock()

	if botInfo != nil && tgMsg.From != nil && tgMsg.From.ID == botInfo.ID {
		b.log.Debug("Filtered echo message from bot", "message_id", tgMsg.MessageID)
		return
	}

	// Command handling.
	if strings.HasPrefix(tgMsg.Text, "/") {
		if b.handleCommandV2(tgMsg) {
			return
		}
	}

	chatID := tgMsg.Chat.ID

	// DM handling — send help if not a command.
	if chatID > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.api.SendMessage(ctx, chatID,
			"I respond to commands and @-mentions in group chats.\n\n"+
				"Commands: /register, /unregister, /status, /help",
			"")
		return
	}

	// Group message — @-mention routing.
	b.handleGroupMessage(tgMsg)
}

// handleCommandV2 dispatches commands to the appropriate handler.
func (b *TelegramBrokerV2) handleCommandV2(tgMsg *TGMessage) bool {
	text := strings.TrimSpace(tgMsg.Text)
	cmd := text
	if idx := strings.Index(cmd, " "); idx != -1 {
		cmd = cmd[:idx]
	}
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/register":
		if strings.Contains(text, "confirm") {
			b.registration.HandleRegisterConfirm(tgMsg)
		} else {
			b.registration.HandleRegister(tgMsg)
		}
		return true
	case "/unregister":
		b.registration.HandleUnregister(tgMsg)
		return true
	}

	return b.commands.HandleCommand(tgMsg)
}

// handleGroupMessage processes a group message through @-mention routing.
func (b *TelegramBrokerV2) handleGroupMessage(tgMsg *TGMessage) {
	chatID := tgMsg.Chat.ID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Look up group link.
	link, err := b.store.GetGroupLink(ctx, chatID)
	if err != nil {
		b.log.Error("Failed to get group link", "chat_id", chatID, "error", err)
		return
	}
	if link == nil || !link.Active {
		return
	}

	// Get project agents (with cache refresh).
	agents := b.getProjectAgents(ctx, link.ProjectID)

	b.mu.RLock()
	botUsername := ""
	if b.botInfo != nil {
		botUsername = b.botInfo.Username
	}
	b.mu.RUnlock()

	// Resolve target agents from @-mentions.
	targets := resolveTargetAgents(tgMsg, botUsername, link.DefaultAgent, agents)
	if len(targets) == 0 {
		return
	}

	// Determine sender identity.
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

	// Check for scion identity mapping — unregistered users cannot route messages.
	if senderID != "" {
		mapping, err := b.store.GetUserMapping(ctx, senderID)
		if err == nil && mapping != nil {
			if mapping.ScionEmail != "" {
				sender = "user:" + mapping.ScionEmail
			}
		} else if mapping == nil {
			b.log.Debug("Unregistered user tried to mention agent", "sender_id", senderID)
			b.api.SendMessage(ctx, chatID, "Please /register first to use this bot.", "")
			return
		}
	}

	// Strip mentions to get clean message text.
	cleanText := stripMentions(tgMsg.Text, botUsername, targets)
	cleanText = strings.TrimSpace(cleanText)
	if cleanText == "" {
		return
	}

	// Deliver to each target agent.
	for _, agentSlug := range targets {
		// Update conversation context.
		if senderID != "" {
			cc := &ConversationContext{
				TelegramUserID: senderID,
				ProjectID:      link.ProjectID,
				AgentSlug:      agentSlug,
				LastChatID:     chatID,
				LastMessageAt:  time.Now(),
			}
			if err := b.store.SaveConversationContext(ctx, cc); err != nil {
				b.log.Warn("Failed to save conversation context", "error", err)
			}
		}

		topic := fmt.Sprintf("scion.project.%s.agent.%s.messages", link.ProjectID, agentSlug)
		recipient := "agent:" + agentSlug

		msg := &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Unix(tgMsg.Date, 0).UTC().Format(time.RFC3339),
			Sender:    sender,
			SenderID:  senderID,
			Recipient: recipient,
			Msg:       cleanText,
			Type:      messages.TypeInstruction,
			Metadata: map[string]string{
				"telegram_chat_id":    strconv.FormatInt(chatID, 10),
				"telegram_message_id": strconv.FormatInt(tgMsg.MessageID, 10),
				"project_id":         link.ProjectID,
			},
		}

		if isEcho(msg) {
			b.log.Debug("Filtered echo message via origin marker", "topic", topic)
			continue
		}

		b.log.Debug("Delivering inbound message",
			"topic", topic, "sender", sender, "agent", agentSlug)

		b.deliverInbound(topic, msg)
	}
}

// --- Callback query handling ---

func (b *TelegramBrokerV2) handleCallbackQuery(ctx context.Context, cb *CallbackQuery) {
	resp, err := b.callbacks.HandleCallback(ctx, cb)
	if err != nil {
		b.log.Error("Callback handling failed", "error", err, "data", cb.Data)
		return
	}

	if resp == nil {
		return
	}

	// Deliver the ask-user response to the hub.
	topic := fmt.Sprintf("scion.project.%s.agent.%s.messages", resp.ProjectID, resp.AgentSlug)

	// Determine sender identity from the callback user.
	sender := "telegram:unknown"
	senderID := ""
	if cb.From != nil {
		senderID = strconv.FormatInt(cb.From.ID, 10)
		if cb.From.Username != "" {
			sender = "telegram:" + cb.From.Username
		} else {
			sender = "telegram:" + senderID
		}

		mapping, mErr := b.store.GetUserMapping(ctx, senderID)
		if mErr == nil && mapping != nil && mapping.ScionEmail != "" {
			sender = "user:" + mapping.ScionEmail
		}
	}

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    sender,
		SenderID:  senderID,
		Recipient: "agent:" + resp.AgentSlug,
		Msg:       resp.Response,
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"telegram_chat_id": strconv.FormatInt(resp.ChatID, 10),
			"ask_request_id":   resp.RequestID,
			"project_id":       resp.ProjectID,
		},
	}

	b.deliverInbound(topic, msg)
}

// --- Agent cache ---

func (b *TelegramBrokerV2) getProjectAgents(ctx context.Context, projectID string) []string {
	cached, err := b.store.GetProjectAgents(ctx, projectID)
	if err != nil {
		b.log.Warn("Failed to read agent cache", "project_id", projectID, "error", err)
	}
	if cached != nil && time.Since(cached.RefreshedAt) < b.agentCacheTTL {
		return cached.AgentSlugs
	}

	agents, err := b.hubClient.ListAgents(ctx, projectID)
	if err != nil {
		b.log.Warn("Failed to refresh agent list from hub", "project_id", projectID, "error", err)
		if cached != nil {
			return cached.AgentSlugs
		}
		return nil
	}

	saveErr := b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   projectID,
		AgentSlugs:  agents,
		RefreshedAt: time.Now(),
	})
	if saveErr != nil {
		b.log.Warn("Failed to cache agents", "project_id", projectID, "error", saveErr)
	}

	return agents
}

// --- Dynamic subscription management ---

func (b *TelegramBrokerV2) subscribeForProject(projectID string) {
	pattern := fmt.Sprintf("scion.project.%s.>", projectID)

	b.mu.RLock()
	hc := b.hostCallbacks
	b.mu.RUnlock()

	if hc != nil {
		if err := hc.RequestSubscription(pattern); err != nil {
			b.log.Warn("Failed to request subscription via host callbacks",
				"pattern", pattern, "error", err)
		}
	}
}

func (b *TelegramBrokerV2) unsubscribeForProject(projectID string) {
	pattern := fmt.Sprintf("scion.project.%s.>", projectID)

	b.mu.RLock()
	hc := b.hostCallbacks
	b.mu.RUnlock()

	if hc != nil {
		if err := hc.CancelSubscription(pattern); err != nil {
			b.log.Warn("Failed to cancel subscription via host callbacks",
				"pattern", pattern, "error", err)
		}
	}
}

// --- Hub delivery (reuses the same pattern as v1) ---

func (b *TelegramBrokerV2) deliverInbound(topic string, msg *messages.StructuredMessage) {
	b.mu.RLock()
	handler := b.InboundHandler
	hubURL := b.hubURL
	hmacKey := b.hmacKey
	brokerID := b.brokerID
	pluginName := b.pluginName
	b.mu.RUnlock()

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

	inboundURL := hubURL + "/api/v1/broker/inbound"
	req, err := http.NewRequest("POST", inboundURL, bytes.NewReader(body))
	if err != nil {
		b.log.Error("Failed to create inbound request", "error", err)
		return
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Plugin-Name", pluginName)

	if brokerID != "" && hmacKey != "" {
		if err := signInboundRequest(req, brokerID, hmacKey); err != nil {
			b.log.Error("Failed to sign inbound request", "error", err)
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

// signInboundRequest signs an HTTP request with HMAC auth.
func signInboundRequest(req *http.Request, brokerID, hmacKey string) error {
	secretKey, err := decodeBase64(hmacKey)
	if err != nil {
		return fmt.Errorf("decode HMAC key: %w", err)
	}
	auth := &apiclient.HMACAuth{
		BrokerID:  brokerID,
		SecretKey: secretKey,
	}
	return auth.ApplyAuth(req)
}

// pruneSentIDsLocked removes dedup entries older than dedupTTL.
func (b *TelegramBrokerV2) pruneSentIDsLocked() {
	now := time.Now()
	for k, t := range b.sentIDs {
		if now.Sub(t) > dedupTTL {
			delete(b.sentIDs, k)
		}
	}
}

// --- Helpers ---

func generateRequestID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// FormatMessageV2 extends FormatMessage with v2-specific formatting.
func FormatMessageV2(msg *messages.StructuredMessage, agentSlug string) string {
	if msg == nil {
		return ""
	}

	var b strings.Builder

	if msg.Urgent {
		b.WriteString("[URGENT] ")
	}
	if msg.Broadcasted {
		b.WriteString("[Broadcast] ")
	}

	// Add agent slug header when available.
	if agentSlug != "" {
		fmt.Fprintf(&b, "[%s] ", agentSlug)
	}

	switch msg.Type {
	case messages.TypeInputNeeded:
		b.WriteString("Input Needed")
	case messages.TypeStateChange:
		b.WriteString("Status Update")
	case messages.TypeAssistantReply:
		b.WriteString("Reply")
	default:
		b.WriteString("Message")
	}

	if msg.Status != "" {
		fmt.Fprintf(&b, " [%s]", msg.Status)
	}

	b.WriteString("\n\n")
	b.WriteString(msg.Msg)

	text := b.String()
	return truncateMessage(text)
}
