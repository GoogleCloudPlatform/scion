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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
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

	sendQueue *SendQueue

	agentCacheTTL  time.Duration
	projectSlugMap map[string]string // injected by hub: projectID → slug

	// Webhook mode fields.
	inboundMode   string // "poll" (default) or "webhook"
	webhookServer *WebhookServer

	InboundHandler func(topic string, msg *messages.StructuredMessage)

	hostCallbacks plugin.HostCallbacks

	errorCooldown           map[string]time.Time // key: "chatID:threadID:errorType" → last sent time
	errorCooldownMu         sync.Mutex
	errorCooldownCheckCount int
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
		errorCooldown: make(map[string]time.Time),
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

	// Initialize send queue with rate limiting.
	sqSize := 0
	if v, ok := config["send_queue_size"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			sqSize = n
		}
	}
	var sqDelay time.Duration
	if v, ok := config["send_min_delay"]; ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			sqDelay = d
		}
	}
	b.sendQueue = NewSendQueue(b.api, b.log, sqSize, sqDelay)

	// Parse optional agent cache TTL.
	if v, ok := config["agent_cache_ttl"]; ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid agent_cache_ttl: %w", err)
		}
		b.agentCacheTTL = d
	}

	// Initialize store: Postgres if database_url is set, otherwise SQLite.
	if databaseURL, ok := config["database_url"]; ok && databaseURL != "" {
		store, err := NewPostgresStore(databaseURL)
		if err != nil {
			return fmt.Errorf("init postgres store: %w", err)
		}
		b.store = store
	} else {
		dbPath := config["db_path"]
		if dbPath == "" {
			dbPath = defaultDBPath
		}
		store, err := NewSQLiteStore(dbPath)
		if err != nil {
			return fmt.Errorf("init store: %w", err)
		}
		b.store = store
	}

	// Validate bot token.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bot, err := b.api.GetMe(ctx)
	if err != nil {
		b.store.Close()
		return fmt.Errorf("failed to validate bot token: %w", err)
	}
	b.botInfo = bot

	if !bot.CanReadAllGroupMessages {
		b.log.Warn("Bot has privacy mode enabled (can_read_all_group_messages=false). " +
			"Group members must use /commands or reply to bot messages. " +
			"Regular @mentions will NOT be delivered. " +
			"Disable via BotFather: /mybots → Bot Settings → Group Privacy → Turn OFF.")
	}

	b.registerBotCommands(ctx)

	// Parse inbound mode: "poll" (default) or "webhook".
	b.inboundMode = "poll"
	if v, ok := config["inbound_mode"]; ok && v != "" {
		switch v {
		case "poll", "webhook":
			b.inboundMode = v
		default:
			b.store.Close()
			return fmt.Errorf("invalid inbound_mode %q: must be \"poll\" or \"webhook\"", v)
		}
	}

	// Set up webhook if configured.
	if b.inboundMode == "webhook" {
		webhookURL, ok := config["webhook_url"]
		if !ok || webhookURL == "" {
			b.store.Close()
			return fmt.Errorf("webhook_url is required when inbound_mode is \"webhook\"")
		}

		webhookListen := config["webhook_listen"]
		if webhookListen == "" {
			webhookListen = ":9094"
		}

		webhookSecret := config["webhook_secret"]

		// In standalone HA mode, skip_set_webhook=true means this instance
		// should start the HTTP listener but NOT call setWebhook. Only the
		// lock holder calls RegisterWebhook() separately.
		skipSetWebhook := config["skip_set_webhook"] == "true"
		if !skipSetWebhook {
			if err := b.api.SetWebhook(ctx, webhookURL, webhookSecret); err != nil {
				b.store.Close()
				return fmt.Errorf("failed to set webhook: %w", err)
			}
		}

		// Stop any existing webhook server before starting a new one.
		// Configure() is called twice (first with plugin config, then with hub
		// credentials). The second call must not fail on port-already-bound.
		if b.webhookServer != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = b.webhookServer.Stop(stopCtx)
			stopCancel()
			b.webhookServer = nil
		}

		// Create and start the webhook server.
		b.webhookServer = NewWebhookServer(webhookListen, webhookSecret, func(update Update) {
			if update.CallbackQuery != nil {
				cbCtx, cbCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cbCancel()
				b.handleCallbackQuery(cbCtx, update.CallbackQuery)
			}
			if update.Message != nil {
				b.handleIncomingMessageV2(update.Message)
			}
		}, b.log)

		if _, err := b.webhookServer.Start(); err != nil {
			b.store.Close()
			return fmt.Errorf("failed to start webhook server: %w", err)
		}
	}

	// Create hub client.
	b.hubClient = NewHTTPHubClient(b.hubURL, b.hmacKey, b.brokerID)

	// Create component handlers.
	b.commands = NewCommandHandler(b.store, b.api, b.hubClient, bot.Username, b.log)
	b.callbacks = NewCallbackHandler(b.store, b.api, b.hubClient, b.log)
	b.registration = NewRegistrationHandler(b.store, b.api, b.hubURL, b.hmacKey, b.brokerID, b.log)

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
			b.callbacks.SetProjects(projects)
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

	storeBackend := "sqlite"
	if _, ok := config["database_url"]; ok && config["database_url"] != "" {
		storeBackend = "postgres"
	}
	b.log.Info("Telegram v2 broker configured",
		"bot_username", bot.Username,
		"bot_id", bot.ID,
		"hub_url", b.hubURL,
		"broker_id", b.brokerID,
		"store_backend", storeBackend,
		"inbound_mode", b.inboundMode,
	)
	return nil
}

// RegisterWebhook calls the Telegram setWebhook API to register this instance's
// webhook URL. In standalone HA mode, only the lock holder should call this.
func (b *TelegramBrokerV2) RegisterWebhook(webhookURL, webhookSecret string) error {
	b.mu.RLock()
	api := b.api
	b.mu.RUnlock()

	if api == nil {
		return fmt.Errorf("broker not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return api.SetWebhook(ctx, webhookURL, webhookSecret)
}

// DeregisterWebhook calls the Telegram deleteWebhook API to remove the
// webhook registration. Called when the lock holder loses the lock.
func (b *TelegramBrokerV2) DeregisterWebhook() error {
	b.mu.RLock()
	api := b.api
	b.mu.RUnlock()

	if api == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return api.DeleteWebhook(ctx)
}

// registerBotCommands sets the bot's command menu in Telegram for autocomplete.
// It registers separate command lists for private chats and group chats.
func (b *TelegramBrokerV2) registerBotCommands(ctx context.Context) {
	privateCommands := []BotCommand{
		{Command: "register", Description: "Link your Telegram account to your scion hub identity"},
		{Command: "unregister", Description: "Remove your Telegram account link"},
		{Command: "status", Description: "Show linked groups and registration status"},
		{Command: "notifications", Description: "Manage agent notification subscriptions"},
		{Command: "help", Description: "Show available commands"},
	}
	groupCommands := []BotCommand{
		{Command: "setup", Description: "Link this group to a scion project"},
		{Command: "agents", Description: "List agents in the linked project"},
		{Command: "default", Description: "Set the default agent"},
		{Command: "settings", Description: "Configure group settings (observer mode, notifications)"},
		{Command: "unlink", Description: "Unlink this group from its project"},
		{Command: "help", Description: "Show available commands"},
	}

	if err := b.api.SetMyCommands(ctx, privateCommands, &BotCommandScope{Type: "all_private_chats"}); err != nil {
		b.log.Warn("failed to register private chat commands", "error", err)
	}
	if err := b.api.SetMyCommands(ctx, groupCommands, &BotCommandScope{Type: "all_group_chats"}); err != nil {
		b.log.Warn("failed to register group chat commands", "error", err)
	}
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

// parseTopicComponents extracts projectID and agentSlug from a broker topic.
// Legacy scion.grove topics are accepted by projectcompat at this adapter boundary.
func parseTopicComponents(topic string) (projectID, agentSlug string) {
	parsed, err := projectcompat.ParseTopic(topic)
	if err == nil {
		projectID = parsed.ProjectID
		if parsed.Kind == projectcompat.TopicKindAgent {
			agentSlug = parsed.Actor
		}
	} else {
		parts := strings.Split(topic, ".")
		for i, part := range parts {
			if (part == "grove" || part == "project") && i+1 < len(parts) {
				projectID = parts[i+1]
			}
			if part == "agent" && i+1 < len(parts) {
				agentSlug = parts[i+1]
			}
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
	sq := b.sendQueue
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

	// Route state-change notifications to the user's personal DM
	// instead of the group chat.
	if msg != nil && msg.Type == messages.TypeStateChange {
		// Route state changes to the recipient's personal DM AND to any
		// linked groups that have notify_in_group enabled.
		dmErr := b.publishStateChangeDM(ctx, api, sq, store, msg, projectID, agentSlug)
		if store != nil && projectID != "" {
			links, _ := store.GetGroupLinksForProject(ctx, projectID)
			for _, link := range links {
				if link.Active && link.NotifyInGroup {
					text := FormatStateChangeCard(msg, agentSlug)
					if text != "" {
						if sq != nil {
							if _, err := sq.Send(ctx, link.ChatID, text, "HTML", nil, 0); err != nil {
								b.log.Warn("Failed to send state-change group notification",
									"chat_id", link.ChatID, "error", err)
							}
						} else if _, err := api.SendMessage(ctx, link.ChatID, text, "HTML"); err != nil {
							b.log.Warn("Failed to send state-change group notification",
								"chat_id", link.ChatID, "error", err)
						}
					}
				}
			}
		}
		return dmErr
	}

	// Route input-needed prompts to the recipient user's personal DM when
	// the recipient is a known user. Falls through to normal chat routing
	// when there is no user recipient (e.g. metadata-based routing).
	if msg != nil && msg.Type == messages.TypeInputNeeded &&
		strings.HasPrefix(msg.Recipient, "user:") {
		dmErr := b.publishInputNeededDM(ctx, api, sq, store, msg, projectID, agentSlug)
		if store != nil && projectID != "" {
			links, _ := store.GetGroupLinksForProject(ctx, projectID)
			for _, link := range links {
				if link.Active && link.NotifyInGroup {
					b.publishInputNeeded(ctx, api, sq, []int64{link.ChatID}, msg, agentSlug, projectID)
				}
			}
		}
		return dmErr
	}

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

	// Observer mode: filter agent-to-agent messages per group link setting.
	if msg != nil && !msg.Broadcasted &&
		strings.HasPrefix(msg.Sender, "agent:") &&
		strings.HasPrefix(msg.Recipient, "agent:") {
		filtered := make([]int64, 0, len(chatIDs))
		for _, chatID := range chatIDs {
			link, err := store.GetGroupLink(ctx, chatID)
			if err != nil {
				b.log.Warn("Failed to load group link for observer check", "chat_id", chatID, "error", err)
				filtered = append(filtered, chatID)
				continue
			}
			if link == nil || link.ShowAgentToAgent {
				filtered = append(filtered, chatID)
				continue
			}
			b.log.Debug("Suppressed agent-to-agent message (observer mode off)",
				"chat_id", chatID, "sender", msg.Sender, "recipient", msg.Recipient)
		}
		chatIDs = filtered
		if len(chatIDs) == 0 {
			return nil
		}
	}

	// Commentary filter: suppress assistant-reply messages per group link setting.
	if msg != nil && msg.Type == messages.TypeAssistantReply {
		filtered := make([]int64, 0, len(chatIDs))
		for _, chatID := range chatIDs {
			link, err := store.GetGroupLink(ctx, chatID)
			if err != nil {
				b.log.Warn("Failed to load group link for commentary check", "chat_id", chatID, "error", err)
				filtered = append(filtered, chatID)
				continue
			}
			if link == nil || link.ShowAssistantReply {
				filtered = append(filtered, chatID)
				continue
			}
			b.log.Debug("Suppressed assistant-reply message (commentary off)",
				"chat_id", chatID, "sender", msg.Sender)
		}
		chatIDs = filtered
		if len(chatIDs) == 0 {
			return nil
		}
	}

	// Handle InputNeeded messages with inline keyboards.
	if msg != nil && msg.Type == messages.TypeInputNeeded {
		return b.publishInputNeeded(ctx, api, sq, chatIDs, msg, agentSlug, projectID)
	}

	// File attachment: check msg.Attachments (scion CLI --attach flag convention)
	// first, then fall back to telegram_attachment_path metadata key.
	if msg != nil {
		attachPath := ""
		if len(msg.Attachments) > 0 {
			attachPath = msg.Attachments[0]
		} else if msg.Metadata != nil {
			attachPath = msg.Metadata["telegram_attachment_path"]
		}
		if attachPath != "" {
			// Translate /workspace/<file> → /home/scion/.scion/projects/<projectSlug>/<file>
			// Agent containers mount the hub's project directory as /workspace.
			attachPath = b.resolveAttachmentPath(ctx, store, attachPath, projectID)
			return b.publishAttachment(ctx, api, chatIDs, msg, agentSlug, attachPath)
		}
	}

	// Resolve recipient's Telegram @username for the message header.
	// Try msg.Recipient first, then fall back to extracting user ID from the topic.
	recipientUsername := ""
	if msg != nil && strings.HasPrefix(msg.Sender, "agent:") && store != nil {
		if strings.HasPrefix(msg.Recipient, "user:") {
			recipientUsername = b.resolveRecipientUsername(ctx, store, msg.Recipient)
		}
		// Fallback: extract user ID from topic (scion.grove.<id>.user.<userid>.messages)
		if recipientUsername == "" {
			if userID := extractUserIDFromTopic(topic); userID != "" {
				recipientUsername = b.resolveRecipientUsername(ctx, store, "user:"+userID)
			}
		}
		b.log.Debug("Resolved recipient username for outbound header",
			"recipient", msg.Recipient, "topic", topic, "username", recipientUsername)
	}

	// Replace scion user emails with Telegram @mentions in the message body.
	if msg != nil && store != nil {
		msg.Msg = resolveOutboundMentions(ctx, store, msg.Msg)
	}

	// Format the message for Telegram.
	text := FormatMessageV2(msg, agentSlug, recipientUsername)
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

	// Determine thread ID for Telegram forum topics.
	var threadOpts []SendOption
	if msg != nil && msg.ThreadID != "" {
		if tid, err := strconv.ParseInt(msg.ThreadID, 10, 64); err == nil && tid != 0 {
			threadOpts = append(threadOpts, SendOption{MessageThreadID: tid})
		}
	}

	var errs []error
	for _, chatID := range chatIDs {
		var err error
		if sq != nil {
			var keyboard *InlineKeyboardMarkup
			if replyToMsgID > 0 {
				_, err = sq.Send(ctx, chatID, text, "", keyboard, replyToMsgID, threadOpts...)
			} else {
				_, err = sq.Send(ctx, chatID, text, "", nil, 0, threadOpts...)
			}
		} else if replyToMsgID > 0 {
			_, err = api.SendMessageWithKeyboard(ctx, chatID, text, "", nil, replyToMsgID, threadOpts...)
		} else {
			_, err = api.SendMessage(ctx, chatID, text, "", threadOpts...)
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
			if errors.As(err, &apiErr) && apiErr.IsMigrated() {
				newChatID := apiErr.MigrateToChatID
				b.log.Info("Group upgraded to supergroup, migrating",
					"old_chat_id", chatID, "new_chat_id", newChatID)
				if store != nil {
					if merr := store.MigrateGroupLink(ctx, chatID, newChatID); merr != nil {
						b.log.Error("Failed to migrate group_link", "error", merr)
					}
				}
				// Retry send with the new chat_id.
				if sq != nil {
					var keyboard *InlineKeyboardMarkup
					if replyToMsgID > 0 {
						_, err = sq.Send(ctx, newChatID, text, "", keyboard, replyToMsgID, threadOpts...)
					} else {
						_, err = sq.Send(ctx, newChatID, text, "", nil, 0, threadOpts...)
					}
				} else if replyToMsgID > 0 {
					_, err = api.SendMessageWithKeyboard(ctx, newChatID, text, "", nil, replyToMsgID, threadOpts...)
				} else {
					_, err = api.SendMessage(ctx, newChatID, text, "", threadOpts...)
				}
				if err != nil {
					b.log.Error("Retry after migration failed", "chat_id", newChatID, "error", err)
					errs = append(errs, err)
				}
				continue
			}
			b.log.Error("Failed to send Telegram message",
				"chat_id", chatID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// resolveRecipientUsername looks up the Telegram username for a recipient
// string of the form "user:email" or "user:uuid". Returns "" if the
// recipient cannot be resolved.
func (b *TelegramBrokerV2) resolveRecipientUsername(ctx context.Context, store Store, recipient string) string {
	if store == nil || !strings.HasPrefix(recipient, "user:") {
		return ""
	}
	val := strings.TrimPrefix(recipient, "user:")
	if val == "" {
		return ""
	}
	var mapping *TelegramUserMapping
	if strings.Contains(val, "@") {
		mapping, _ = store.GetUserMappingByEmail(ctx, val)
	} else {
		mapping, _ = store.GetUserMappingByScionUserID(ctx, val)
	}
	if mapping != nil && mapping.TelegramUsername != "" {
		return mapping.TelegramUsername
	}
	return ""
}

var outboundEmailRe = regexp.MustCompile(`(?:user:)?[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// resolveOutboundMentions scans text for scion user emails (with optional
// "user:" prefix) and replaces them with Telegram @mentions when the user
// is registered and has a username.
func resolveOutboundMentions(ctx context.Context, store Store, text string) string {
	if store == nil || text == "" {
		return text
	}

	matches := outboundEmailRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	for i := len(matches) - 1; i >= 0; i-- {
		start, end := matches[i][0], matches[i][1]

		if start > 0 {
			prev := text[start-1]
			if prev == '/' || prev == ':' {
				continue
			}
		}
		if end < len(text) && text[end] == '/' {
			continue
		}

		match := text[start:end]
		email := match
		if strings.HasPrefix(email, "user:") {
			email = strings.TrimPrefix(email, "user:")
		}

		mapping, err := store.GetUserMappingByEmail(ctx, email)
		if err != nil || mapping == nil || mapping.TelegramUsername == "" {
			continue
		}

		text = text[:start] + "@" + mapping.TelegramUsername + text[end:]
	}

	return text
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
func (b *TelegramBrokerV2) publishInputNeeded(ctx context.Context, api *TelegramAPIClient, sq *SendQueue, chatIDs []int64, msg *messages.StructuredMessage, agentSlug, projectID string) error {
	recipientUsername := ""
	if msg != nil && strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "user:") {
		recipientUsername = b.resolveRecipientUsername(ctx, b.store, msg.Recipient)
	}
	text := FormatMessageV2(msg, agentSlug, recipientUsername)
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
		var sent *TGMessage
		var err error
		if sq != nil {
			sent, err = sq.Send(ctx, chatID, text, "", keyboard, 0)
		} else if keyboard == nil {
			sent, err = api.SendMessage(ctx, chatID, text, "")
		} else {
			sent, err = api.SendMessageWithKeyboard(ctx, chatID, text, "", keyboard, 0)
		}
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.IsTransient() {
				b.log.Warn("Transient error sending input-needed",
					"chat_id", chatID, "error", err)
				continue
			}
			if errors.As(err, &apiErr) && apiErr.IsMigrated() {
				newChatID := apiErr.MigrateToChatID
				b.log.Info("Group upgraded to supergroup, migrating",
					"old_chat_id", chatID, "new_chat_id", newChatID)
				if b.store != nil {
					if merr := b.store.MigrateGroupLink(ctx, chatID, newChatID); merr != nil {
						b.log.Error("Failed to migrate group_link", "error", merr)
					}
				}
				// Retry send with the new chat_id.
				keyboard = buildAskUserKeyboard(requestID, choices)
				if sq != nil {
					sent, err = sq.Send(ctx, newChatID, text, "", keyboard, 0)
				} else if keyboard == nil {
					sent, err = api.SendMessage(ctx, newChatID, text, "")
				} else {
					sent, err = api.SendMessageWithKeyboard(ctx, newChatID, text, "", keyboard, 0)
				}
				if err != nil {
					b.log.Error("Retry after migration failed", "chat_id", newChatID, "error", err)
					errs = append(errs, err)
					continue
				}
				// Save PendingAskUser with the new chat_id.
				pending := &PendingAskUser{
					RequestID: requestID,
					MessageID: sent.MessageID,
					ChatID:    newChatID,
					AgentSlug: agentSlug,
					ProjectID: projectID,
					Choices:   choices,
					ExpiresAt: time.Now().Add(askUserExpiry),
				}
				if perr := b.store.SavePendingAskUser(ctx, pending); perr != nil {
					b.log.Error("Failed to save pending ask user after migration", "error", perr)
				}
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

// publishStateChangeDM sends a state-change notification to the recipient
// user's personal Telegram DM (private chat). The DM chat ID equals the
// user's Telegram user ID. If the recipient cannot be resolved to a
// Telegram user, the message is silently dropped.
func (b *TelegramBrokerV2) publishStateChangeDM(ctx context.Context, api *TelegramAPIClient, sq *SendQueue, store Store, msg *messages.StructuredMessage, projectID, agentSlug string) error {
	if store == nil {
		return nil
	}

	recipientVal := strings.TrimPrefix(msg.Recipient, "user:")
	if recipientVal == msg.Recipient || recipientVal == "" {
		b.log.Debug("State-change recipient is not a user, dropping", "recipient", msg.Recipient)
		return nil
	}

	// Hub may pass recipient as email OR as UUID; try both lookups.
	var mapping *TelegramUserMapping
	var err error
	if strings.Contains(recipientVal, "@") {
		mapping, err = store.GetUserMappingByEmail(ctx, recipientVal)
	} else {
		mapping, err = store.GetUserMappingByScionUserID(ctx, recipientVal)
		if err == nil && mapping == nil {
			mapping, err = store.GetUserMappingByEmail(ctx, recipientVal)
		}
	}
	if err != nil {
		b.log.Warn("Failed to look up user mapping for state-change DM", "recipient", recipientVal, "error", err)
		return nil
	}
	if mapping == nil {
		b.log.Debug("No user mapping for state-change recipient, dropping", "recipient", recipientVal)
		return nil
	}

	// Respect per-user notification preferences: if the user explicitly
	// disabled notifications for this agent, skip the DM.
	if projectID != "" && agentSlug != "" {
		pref, prefErr := store.GetNotificationPref(ctx, mapping.TelegramUserID, projectID, agentSlug)
		if prefErr != nil {
			b.log.Warn("Failed to check notification pref", "error", prefErr)
		}
		if pref != nil && !pref.Enabled {
			b.log.Debug("User disabled notifications for agent, skipping DM",
				"recipient", recipientVal, "project", projectID, "agent", agentSlug)
			return nil
		}
	}

	tgUserID, err := strconv.ParseInt(mapping.TelegramUserID, 10, 64)
	if err != nil {
		b.log.Warn("Invalid Telegram user ID in mapping", "telegram_user_id", mapping.TelegramUserID, "error", err)
		return nil
	}

	text := FormatStateChangeCard(msg, agentSlug)
	if text == "" {
		return nil
	}

	if sq != nil {
		_, err = sq.Send(ctx, tgUserID, text, "HTML", nil, 0)
	} else {
		_, err = api.SendMessage(ctx, tgUserID, text, "HTML")
	}
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsTransient() {
			b.log.Warn("Transient error sending state-change DM",
				"telegram_user_id", tgUserID, "error", err)
			return nil
		}
		b.log.Error("Failed to send state-change DM",
			"telegram_user_id", tgUserID, "error", err)
		return err
	}

	return nil
}

// publishInputNeededDM sends an InputNeeded prompt to the recipient user's
// personal Telegram DM. When the message has structured choices, inline
// keyboard buttons are shown; otherwise Telegram's ForceReply markup is used
// to pre-focus the reply input. A PendingAskUser record is saved so the
// callback handler can route the user's response back.
func (b *TelegramBrokerV2) publishInputNeededDM(ctx context.Context, api *TelegramAPIClient, sq *SendQueue, store Store, msg *messages.StructuredMessage, projectID, agentSlug string) error {
	if store == nil {
		return nil
	}

	recipientVal := strings.TrimPrefix(msg.Recipient, "user:")
	if recipientVal == msg.Recipient || recipientVal == "" {
		b.log.Debug("InputNeeded recipient is not a user, dropping", "recipient", msg.Recipient)
		return nil
	}

	var mapping *TelegramUserMapping
	var err error
	if strings.Contains(recipientVal, "@") {
		mapping, err = store.GetUserMappingByEmail(ctx, recipientVal)
	} else {
		mapping, err = store.GetUserMappingByScionUserID(ctx, recipientVal)
		if err == nil && mapping == nil {
			mapping, err = store.GetUserMappingByEmail(ctx, recipientVal)
		}
	}
	if err != nil {
		b.log.Warn("Failed to look up user mapping for input-needed DM", "recipient", recipientVal, "error", err)
		return nil
	}
	if mapping == nil {
		b.log.Debug("No user mapping for input-needed recipient, dropping", "recipient", recipientVal)
		return nil
	}

	tgUserID, err := strconv.ParseInt(mapping.TelegramUserID, 10, 64)
	if err != nil {
		b.log.Warn("Invalid Telegram user ID in mapping", "telegram_user_id", mapping.TelegramUserID, "error", err)
		return nil
	}

	text := FormatInputNeededCard(msg, agentSlug)
	if text == "" {
		return nil
	}

	var choices []string
	if msg.Metadata != nil {
		if choicesJSON, ok := msg.Metadata["choices"]; ok && choicesJSON != "" {
			json.Unmarshal([]byte(choicesJSON), &choices)
		}
	}

	requestID := generateRequestID()
	keyboard := buildAskUserKeyboard(requestID, choices)

	var sent *TGMessage
	if keyboard != nil {
		if sq != nil {
			sent, err = sq.Send(ctx, tgUserID, text, "HTML", keyboard, 0)
		} else {
			sent, err = api.SendMessageWithKeyboard(ctx, tgUserID, text, "HTML", keyboard, 0)
		}
	} else {
		sent, err = api.SendMessageWithForceReply(ctx, tgUserID, text, "HTML", nil)
	}
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsTransient() {
			b.log.Warn("Transient error sending input-needed DM",
				"telegram_user_id", tgUserID, "error", err)
			return nil
		}
		b.log.Error("Failed to send input-needed DM",
			"telegram_user_id", tgUserID, "error", err)
		return err
	}

	pending := &PendingAskUser{
		RequestID: requestID,
		MessageID: sent.MessageID,
		ChatID:    tgUserID,
		AgentSlug: agentSlug,
		ProjectID: projectID,
		Choices:   choices,
		ExpiresAt: time.Now().Add(askUserExpiry),
	}
	if err := b.store.SavePendingAskUser(ctx, pending); err != nil {
		b.log.Error("Failed to save pending ask user for DM", "error", err)
	}

	return nil
}

// resolveAttachmentPath translates an agent-relative /workspace path to the
// corresponding host-side path under /home/scion/.scion/projects/<slug>/.
// Agent containers mount /home/scion/.scion/projects/<slug> as /workspace.
// Accepts "/workspace/file", "/workspace", "workspace/file", "workspace",
// and bare relative paths like "file.png". Falls back to the original path
// if translation is not possible.
func (b *TelegramBrokerV2) resolveAttachmentPath(ctx context.Context, store Store, attachPath, projectID string) string {
	originalPath := attachPath

	var relPath string
	switch {
	case strings.HasPrefix(attachPath, "/workspace/"):
		relPath = strings.TrimPrefix(attachPath, "/workspace/")
	case attachPath == "/workspace":
		relPath = "."
	case strings.HasPrefix(attachPath, "workspace/"):
		relPath = strings.TrimPrefix(attachPath, "workspace/")
	case attachPath == "workspace":
		relPath = "."
	case !strings.HasPrefix(attachPath, "/"):
		relPath = attachPath
	default:
		return attachPath
	}

	relPath = filepath.Clean(relPath)
	if strings.HasPrefix(relPath, "..") || (filepath.IsAbs(relPath) && relPath != ".") {
		b.log.Warn("Attachment path escapes workspace, ignoring translation",
			"attach_path", attachPath, "rel_path", relPath)
		return attachPath
	}

	// Look up project slug from group links, falling back to the injected slug map.
	slug := ""
	if store != nil && projectID != "" {
		links, err := store.GetGroupLinksForProject(ctx, projectID)
		if err == nil && len(links) > 0 && links[0].ProjectSlug != "" {
			slug = links[0].ProjectSlug
		}
	}
	if slug == "" && projectID != "" {
		slug = b.projectSlugMap[projectID]
	}
	if slug == "" {
		b.log.Debug("Attachment path unchanged, no project slug found",
			"original", originalPath, "project_id", projectID)
		return attachPath
	}

	projectDir := filepath.Join("/home/scion/.scion/projects", slug)
	var hostPath string
	if relPath == "." {
		hostPath = projectDir
	} else {
		hostPath = filepath.Join(projectDir, relPath)
		if !strings.HasPrefix(hostPath, projectDir+"/") {
			b.log.Warn("Resolved attachment path escapes project directory",
				"host_path", hostPath, "expected_prefix", projectDir+"/")
			return attachPath
		}
	}

	b.log.Debug("Resolved attachment path", "original", originalPath, "resolved", hostPath)
	return hostPath
}

// publishAttachment reads a file from the local filesystem and sends it to
// each target chat via Telegram's sendDocument API. The message body is used
// as the document caption.
//
// This reads from telegram_attachment_path on the local filesystem, which
// requires the plugin process to share a volume mount with the agent that
// wrote the file. This is valid in single-VM / shared-dir setups but will
// NOT work when agents and the plugin run in separate GKE pods with isolated
// volumes. A future telegram_attachment_url metadata key should support
// fetching the file from a URL (e.g. GCS signed URL) instead.
func (b *TelegramBrokerV2) publishAttachment(ctx context.Context, api *TelegramAPIClient, chatIDs []int64, msg *messages.StructuredMessage, agentSlug, attachPath string) error {
	f, err := os.Open(attachPath)
	if err != nil {
		b.log.Error("Failed to open attachment file",
			"path", attachPath, "error", err)
		return fmt.Errorf("open attachment %q: %w", attachPath, err)
	}
	defer f.Close()

	filename := filepath.Base(attachPath)
	if name, ok := msg.Metadata["telegram_attachment_name"]; ok && name != "" {
		filename = name
	}

	caption := truncateMessage(FormatMessageV2(msg, agentSlug))
	// Telegram caption limit is 1024 characters.
	if len(caption) > 1024 {
		caption = caption[:1021] + "..."
	}

	var errs []error
	for i, chatID := range chatIDs {
		// Rewind the file reader for each send after the first.
		if i > 0 {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				errs = append(errs, fmt.Errorf("seek attachment for chat %d: %w", chatID, err))
				continue
			}
		}

		_, err := api.SendDocument(ctx, chatID, filename, f, caption, "")
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.IsTransient() {
				b.log.Warn("Transient error sending attachment",
					"chat_id", chatID, "error", err)
				continue
			}
			b.log.Error("Failed to send attachment",
				"chat_id", chatID, "path", attachPath, "error", err)
			errs = append(errs, err)
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
	api := b.api
	webhookSrv := b.webhookServer
	inboundMode := b.inboundMode
	sendQueue := b.sendQueue
	b.mu.Unlock()

	// Shut down inbound transport.
	if inboundMode == "webhook" {
		// Remove the webhook registration from Telegram.
		if api != nil {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := api.DeleteWebhook(shutCtx); err != nil {
				b.log.Warn("Failed to delete webhook on close", "error", err)
			}
			shutCancel()
		}
		// Stop the local HTTP server.
		if webhookSrv != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := webhookSrv.Stop(stopCtx); err != nil {
				b.log.Warn("Failed to stop webhook server", "error", err)
			}
			stopCancel()
		}
	} else {
		b.stopPolling()
	}

	if sendQueue != nil {
		sendQueue.Close()
	}

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
	if b.inboundMode == "webhook" {
		return // webhook mode handles inbound via HTTP
	}
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
	if tgMsg.Text == "" && tgMsg.Caption == "" && tgMsg.Photo == nil && tgMsg.Document == nil {
		return
	}

	// Use caption as text fallback for photo/document messages.
	if tgMsg.Text == "" && tgMsg.Caption != "" {
		tgMsg.Text = tgMsg.Caption
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

	// Resolve effective default agent: topic-level override first, then chat-level.
	effectiveDefault := link.DefaultAgent
	if tgMsg.MessageThreadID != 0 {
		if topicDefault, err := b.store.GetTopicDefault(ctx, chatID, tgMsg.MessageThreadID); err != nil {
			b.log.Error("Failed to get topic default", "error", err)
		} else if topicDefault != "" {
			effectiveDefault = topicDefault
		}
	}

	// Resolve target agents from @-mentions.
	targets, isAll := resolveTargetAgents(tgMsg, botUsername, effectiveDefault, agents)

	// Fallback 1: reply-to-bot-message — extract the agent from the replied-to message.
	if len(targets) == 0 && tgMsg.ReplyToMessage != nil {
		replyFromID := int64(0)
		if tgMsg.ReplyToMessage.From != nil {
			replyFromID = tgMsg.ReplyToMessage.From.ID
		}
		botID := int64(0)
		if b.botInfo != nil {
			botID = b.botInfo.ID
		}
		b.log.Debug("Fallback1: checking reply-to message", "reply_from_id", replyFromID, "bot_id", botID)
		replyText := tgMsg.ReplyToMessage.Text
		if len([]rune(replyText)) > 100 {
			replyText = string([]rune(replyText)[:100])
		}
		b.log.Debug("Fallback1: reply-to message text sample", "text_prefix", replyText)
		if b.botInfo != nil && tgMsg.ReplyToMessage.From != nil && tgMsg.ReplyToMessage.From.ID == b.botInfo.ID {
			slug := extractAgentFromBotMessage(tgMsg.ReplyToMessage.Text)
			b.log.Debug("Fallback1: extractAgentFromBotMessage result", "slug", slug)
			if slug != "" {
				// Trust the extracted slug — it came from a message the bot sent.
				// Skip knownAgents validation: the agent may not be in the cached
				// list (e.g., created after the last cache refresh, or not running).
				targets = []string{slug}
			}
		}
	}

	// Fallback 2: most recent conversation context for this user+project.
	if len(targets) == 0 && tgMsg.ReplyToMessage != nil {
		b.log.Debug("Fallback1: failed - trying conversation context")
		if b.botInfo != nil && tgMsg.ReplyToMessage.From != nil && tgMsg.ReplyToMessage.From.ID == b.botInfo.ID {
			if tgMsg.From != nil {
				senderIDStr := strconv.FormatInt(tgMsg.From.ID, 10)
				cc, err := b.store.GetLatestConversationContext(ctx, senderIDStr, link.ProjectID)
				ccSlug := ""
				if err == nil && cc != nil {
					ccSlug = cc.AgentSlug
				}
				b.log.Debug("Fallback2: conversation context result", "agent_slug", ccSlug, "err", err)
				if ccSlug != "" && slices.Contains(agents, ccSlug) {
					targets = []string{ccSlug}
				}
			}
		}
	}

	// Fallback 3: unaddressed plain text or file attachment → default agent.
	// Skip when the message leads with (offset=0) an @mention of another
	// Telegram user — that's a user-to-user message. Mentions embedded
	// later (offset>0) do not block default routing; resolveUserMentions
	// injects the resolved scion identity for those.
	if len(targets) == 0 && effectiveDefault != "" {
		hasAttachment := tgMsg.Photo != nil || tgMsg.Document != nil
		text := strings.TrimSpace(tgMsg.Text)
		textRoutes := text != "" && !strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "@") && !hasNonBotUserMention(tgMsg, botUsername, agents)
		if textRoutes || hasAttachment {
			// Validate default agent against the cached agent list before routing.
			if !slices.Contains(agents, effectiveDefault) {
				threadID := 0
				if tgMsg.MessageThreadID != 0 {
					threadID = int(tgMsg.MessageThreadID)
				}
				if !b.shouldSuppressError(chatID, threadID, "default_agent_not_found") {
					replyTo := ""
					if tgMsg.MessageID != 0 {
						replyTo = strconv.FormatInt(int64(tgMsg.MessageID), 10)
					}
					errMsg := fmt.Sprintf("Default agent %q is no longer available. Use /agents to see available agents, or /default to change the default.", effectiveDefault)
					b.api.SendMessage(ctx, chatID, errMsg, replyTo) //nolint:errcheck
				}
				return
			}
			b.log.Debug("Using default agent", "agent", effectiveDefault)
			targets = []string{effectiveDefault}
		}
	}

	if len(targets) == 0 {
		// If the bot was explicitly mentioned but no agent matched, tell the user.
		if isBotMentioned(tgMsg, botUsername) {
			unresolved := extractUnresolvedMentions(tgMsg.Text, botUsername, agents)
			if len(unresolved) > 0 {
				errMsg := fmt.Sprintf("No agent named %q found in this project. Use /agents to see available agents.", unresolved[0])
				b.api.SendMessage(ctx, chatID, errMsg, "") //nolint:errcheck
			}
		} else {
			// Check for @tokens not matching any known agent. Filter out
			// real Telegram user mentions (those have a "mention" entity
			// from the Bot API) so we only flag likely agent-name typos.
			unresolved := extractUnresolvedMentions(tgMsg.Text, botUsername, agents)
			if len(unresolved) > 0 {
				entityMentions := entityMentionSet(tgMsg)
				var typos []string
				for _, name := range unresolved {
					if !entityMentions[strings.ToLower(name)] {
						typos = append(typos, "@"+name)
					}
				}
				if len(typos) > 0 {
					errMsg := fmt.Sprintf("Unknown agent(s): %s. Use /agents to see available agents.", strings.Join(typos, ", "))
					b.api.SendMessage(ctx, chatID, errMsg, "") //nolint:errcheck
				}
			}
		}
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

	// Resolve @username mentions to scion user identities and replace
	// text_mention display names with "user:email" in the message text.
	resolvedText, resolvedMentionsJSON := b.resolveUserMentions(ctx, tgMsg)

	// Strip bot/agent mentions to get clean message text.
	cleanText := stripMentions(resolvedText, botUsername, targets)
	cleanText = strings.TrimSpace(cleanText)

	// Download file attachments (photos/documents).
	var attachmentPath, placeholder string
	if tgMsg.Photo != nil || tgMsg.Document != nil {
		var err error
		attachmentPath, placeholder, err = b.downloadTelegramFile(ctx, tgMsg, link.ProjectSlug)
		if err != nil {
			b.log.Error("Failed to download telegram file", "error", err)
			b.api.SendMessage(ctx, chatID, "Failed to process attachment: "+err.Error(), "")
		}
	}

	// Build final message text: clean text + attachment placeholder.
	msgText := cleanText
	if placeholder != "" {
		if msgText != "" {
			msgText = msgText + "\n" + placeholder
		} else {
			msgText = placeholder
		}
	}
	if msgText == "" {
		return
	}

	// Determine message type: multi-agent @mentions (not @all) use group-set.
	msgType := messages.TypeInstruction
	if !isAll && len(targets) > 1 {
		msgType = messages.TypeGroupSet
	}

	// Build the recipients group string for group messages.
	var recipientsSet string
	if msgType == messages.TypeGroupSet {
		prefixed := make([]string, len(targets))
		for i, slug := range targets {
			prefixed[i] = "agent:" + slug
		}
		recipientsSet = messages.FormatGroupRecipients(sender, prefixed)
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

		topic := projectcompat.AgentTopic(link.ProjectID, agentSlug)
		recipient := "agent:" + agentSlug

		msg := &messages.StructuredMessage{
			Version:    messages.Version,
			Timestamp:  time.Unix(tgMsg.Date, 0).UTC().Format(time.RFC3339),
			Sender:     sender,
			SenderID:   senderID,
			Recipient:  recipient,
			Recipients: recipientsSet,
			Msg:        msgText,
			Type:       msgType,
			Channel:    "telegram",
			Metadata: map[string]string{
				"telegram_chat_id":    strconv.FormatInt(chatID, 10),
				"telegram_message_id": strconv.FormatInt(tgMsg.MessageID, 10),
				"project_id":          link.ProjectID,
			},
		}

		if tgMsg.MessageThreadID != 0 {
			msg.ThreadID = strconv.FormatInt(tgMsg.MessageThreadID, 10)
		}

		if attachmentPath != "" {
			msg.Attachments = []string{attachmentPath}
		}

		if resolvedMentionsJSON != "" {
			msg.Metadata["resolved_mentions"] = resolvedMentionsJSON
		}

		if isEcho(msg) {
			b.log.Debug("Filtered echo message via origin marker", "topic", topic)
			continue
		}

		b.log.Debug("Delivering inbound message",
			"topic", topic, "sender", sender, "agent", agentSlug)

		statusCode, deliveryErr := b.deliverInboundWithFeedback(ctx, topic, msg)
		if statusCode >= 400 && deliveryErr != "" {
			threadID := 0
			if tgMsg.MessageThreadID != 0 {
				threadID = int(tgMsg.MessageThreadID)
			}
			errorType := "delivery_error"
			if statusCode == http.StatusNotFound {
				errorType = "agent_not_found"
			} else if statusCode == http.StatusForbidden {
				errorType = "permission_denied"
			}
			if !b.shouldSuppressError(chatID, threadID, errorType) {
				replyTo := ""
				if tgMsg.MessageID != 0 {
					replyTo = strconv.FormatInt(int64(tgMsg.MessageID), 10)
				}
				b.api.SendMessage(ctx, chatID, "Message delivery failed: "+deliveryErr, replyTo) //nolint:errcheck
			}
		}
	}
}

const maxTelegramFileSize = 20 * 1024 * 1024 // 20 MB

// downloadTelegramFile downloads a photo or document from a Telegram message
// and saves it to the agent's workspace downloads directory. Returns the
// agent-relative path and a placeholder string for the message body.
func (b *TelegramBrokerV2) downloadTelegramFile(ctx context.Context, tgMsg *TGMessage, projectSlug string) (agentPath, placeholder string, err error) {
	var fileID, fileName, fileType string
	var fileSize int64

	switch {
	case tgMsg.Document != nil:
		fileID = tgMsg.Document.FileID
		fileSize = tgMsg.Document.FileSize
		fileName = tgMsg.Document.FileName
		if fileName == "" {
			fileName = tgMsg.Document.FileUniqueID
		}
		fileType = "Document"
	case len(tgMsg.Photo) > 0:
		largest := tgMsg.Photo[len(tgMsg.Photo)-1]
		fileID = largest.FileID
		fileSize = largest.FileSize
		fileName = fmt.Sprintf("photo_%s.jpg", largest.FileUniqueID)
		fileType = "Photo"
	default:
		return "", "", fmt.Errorf("message has no photo or document")
	}

	if fileSize > maxTelegramFileSize {
		return "", "", fmt.Errorf("file too large (%d bytes, max %d)", fileSize, maxTelegramFileSize)
	}

	tgFile, err := b.api.GetFile(ctx, fileID)
	if err != nil {
		return "", "", fmt.Errorf("getFile: %w", err)
	}

	body, err := b.api.DownloadFile(ctx, tgFile.FilePath)
	if err != nil {
		return "", "", fmt.Errorf("download file: %w", err)
	}
	defer body.Close()

	timestamp := time.Now().Unix()
	destName := fmt.Sprintf("tg_%d_%s", timestamp, fileName)

	hostDir := filepath.Join("/home/scion/.scion/projects", projectSlug, "downloads")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create downloads dir: %w", err)
	}

	destPath := filepath.Join(hostDir, destName)
	f, err := os.Create(destPath)
	if err != nil {
		return "", "", fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, body); err != nil {
		os.Remove(destPath)
		return "", "", fmt.Errorf("write file: %w", err)
	}

	agentPath = filepath.Join("/workspace/downloads", destName)
	placeholder = fmt.Sprintf("📎 [%s attached: %s]", fileType, fileName)

	b.log.Info("Downloaded telegram file",
		"type", fileType, "file", fileName, "path", destPath, "agent_path", agentPath)

	return agentPath, placeholder, nil
}

// resolveUserMentions extracts @username and text_mention entities from a
// Telegram message, looks each up in the user_mappings store, and returns:
//   - modifiedText: the message text with text_mention display names (offset>0)
//     replaced by "user:email" for registered users, and "code"/"pre" entity
//     spans re-wrapped in backticks (Telegram strips backticks from the text
//     field and stores the formatting as entities)
//   - resolvedJSON: a JSON string mapping display names → "user:email" for all
//     resolved mentions (empty string if none resolve)
func (b *TelegramBrokerV2) resolveUserMentions(ctx context.Context, tgMsg *TGMessage) (string, string) {
	if len(tgMsg.Entities) == 0 {
		return tgMsg.Text, ""
	}

	resolved := make(map[string]string)

	type textReplacement struct {
		offset      int
		length      int
		replacement string
	}
	var replacements []textReplacement

	for _, ent := range tgMsg.Entities {
		switch ent.Type {
		case "mention":
			mention, ok := utf16Extract(tgMsg.Text, ent.Offset, ent.Length)
			if !ok {
				continue
			}
			// Skip if the entity is a partial match — Telegram may truncate
			// at a hyphen in agent names like "@agent-dev", creating an entity
			// covering only "@agent". Replacing partial text would corrupt the
			// message (e.g., "user:email-dev" instead of "@agent-dev").
			if isPartialMentionEntity(tgMsg.Text, ent.Offset, ent.Length) {
				continue
			}
			username := strings.TrimPrefix(mention, "@")
			if username == "" {
				continue
			}
			mapping, err := b.store.GetUserMappingByUsername(ctx, username)
			if err != nil {
				b.log.Warn("Failed to look up mention username", "username", username, "error", err)
				continue
			}
			if mapping == nil || mapping.ScionEmail == "" {
				continue
			}
			scionIdentity := "user:" + mapping.ScionEmail
			resolved[mention] = scionIdentity
			if ent.Offset > 0 {
				replacements = append(replacements, textReplacement{
					offset:      ent.Offset,
					length:      ent.Length,
					replacement: scionIdentity,
				})
			}
		case "text_mention":
			if ent.User == nil {
				continue
			}
			tgUserID := strconv.FormatInt(ent.User.ID, 10)
			mapping, err := b.store.GetUserMapping(ctx, tgUserID)
			if err != nil {
				b.log.Warn("Failed to look up text_mention user", "telegram_user_id", tgUserID, "error", err)
				continue
			}
			if mapping == nil || mapping.ScionEmail == "" {
				continue
			}
			displayName := ent.User.FirstName
			if ent.User.Username != "" {
				displayName = ent.User.Username
			}
			scionIdentity := "user:" + mapping.ScionEmail
			resolved["@"+displayName] = scionIdentity
			if ent.Offset > 0 {
				replacements = append(replacements, textReplacement{
					offset:      ent.Offset,
					length:      ent.Length,
					replacement: scionIdentity,
				})
			}
		case "code":
			span, ok := utf16Extract(tgMsg.Text, ent.Offset, ent.Length)
			if !ok {
				continue
			}
			replacements = append(replacements, textReplacement{
				offset:      ent.Offset,
				length:      ent.Length,
				replacement: "`" + span + "`",
			})
		case "pre":
			span, ok := utf16Extract(tgMsg.Text, ent.Offset, ent.Length)
			if !ok {
				continue
			}
			prefix := "```"
			if ent.Language != "" {
				prefix += ent.Language
			}
			replacements = append(replacements, textReplacement{
				offset:      ent.Offset,
				length:      ent.Length,
				replacement: prefix + "\n" + span + "\n```",
			})
		}
	}

	modifiedText := tgMsg.Text
	if len(replacements) > 0 {
		slices.SortFunc(replacements, func(a, b textReplacement) int {
			return b.offset - a.offset
		})
		for _, r := range replacements {
			start, end, ok := utf16ByteRange(modifiedText, r.offset, r.length)
			if !ok {
				continue
			}
			modifiedText = modifiedText[:start] + r.replacement + modifiedText[end:]
		}
	}

	if len(resolved) == 0 {
		return modifiedText, ""
	}

	data, err := json.Marshal(resolved)
	if err != nil {
		b.log.Warn("Failed to marshal resolved mentions", "error", err)
		return modifiedText, ""
	}
	return modifiedText, string(data)
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
	topic := projectcompat.AgentTopic(resp.ProjectID, resp.AgentSlug)

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
		Channel:   "telegram",
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
		return agentSlugs(cached.Agents)
	}

	agents, err := b.hubClient.ListAgents(ctx, projectID)
	if err != nil {
		b.log.Warn("Failed to refresh agent list from hub", "project_id", projectID, "error", err)
		if cached != nil {
			return agentSlugs(cached.Agents)
		}
		return nil
	}

	saveErr := b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   projectID,
		Agents:      agents,
		RefreshedAt: time.Now(),
	})
	if saveErr != nil {
		b.log.Warn("Failed to cache agents", "project_id", projectID, "error", saveErr)
	}

	return agentSlugs(agents)
}

// --- Dynamic subscription management ---

func (b *TelegramBrokerV2) subscribeForProject(projectID string) {
	pattern := projectcompat.ProjectPattern(projectID)

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
	pattern := projectcompat.ProjectPattern(projectID)

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

const errorCooldownDuration = 5 * time.Minute

// shouldSuppressError checks whether an error of the given type was already
// reported to the given chat+thread within the cooldown window. If not, it
// records the current time and returns false (do not suppress).
func (b *TelegramBrokerV2) shouldSuppressError(chatID int64, threadID int, errorType string) bool {
	key := fmt.Sprintf("%d:%d:%s", chatID, threadID, errorType)

	b.errorCooldownMu.Lock()
	defer b.errorCooldownMu.Unlock()

	now := time.Now()

	b.errorCooldownCheckCount++
	if len(b.errorCooldown) > 1000 && b.errorCooldownCheckCount%100 == 0 {
		for k, v := range b.errorCooldown {
			if now.Sub(v) >= errorCooldownDuration {
				delete(b.errorCooldown, k)
			}
		}
	}

	if last, ok := b.errorCooldown[key]; ok && now.Sub(last) < errorCooldownDuration {
		return true
	}
	b.errorCooldown[key] = now
	return false
}

// deliverInboundWithFeedback delivers an inbound message to the Hub and
// returns the HTTP status code and parsed error message (if any) so the
// caller can report delivery failures back to the originating chat.
func (b *TelegramBrokerV2) deliverInboundWithFeedback(ctx context.Context, topic string, msg *messages.StructuredMessage) (statusCode int, errMsg string) {
	b.mu.RLock()
	handler := b.InboundHandler
	hubURL := b.hubURL
	hmacKey := b.hmacKey
	brokerID := b.brokerID
	pluginName := b.pluginName
	b.mu.RUnlock()

	if handler != nil {
		handler(topic, msg)
		return http.StatusOK, ""
	}

	if hubURL == "" {
		b.log.Debug("No hub URL configured, dropping inbound message", "topic", topic)
		return http.StatusOK, ""
	}

	payload := inboundPayload{
		Topic:   topic,
		Message: msg,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		b.log.Error("Failed to marshal inbound message", "error", err)
		return http.StatusInternalServerError, "internal error"
	}

	inboundURL := hubURL + "/api/v1/broker/inbound"
	req, err := http.NewRequestWithContext(ctx, "POST", inboundURL, bytes.NewReader(body))
	if err != nil {
		b.log.Error("Failed to create inbound request", "error", err)
		return http.StatusInternalServerError, "internal error"
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Plugin-Name", pluginName)

	if brokerID != "" && hmacKey != "" {
		if err := signInboundRequest(req, brokerID, hmacKey); err != nil {
			b.log.Error("Failed to sign inbound request", "error", err)
			return http.StatusInternalServerError, "internal error"
		}
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.log.Error("Failed to deliver inbound message", "error", err, "topic", topic)
		return http.StatusBadGateway, "delivery failed"
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b.log.Error("Hub rejected inbound message",
			"status", resp.StatusCode, "topic", topic)
		// Parse the Hub error response for a human-readable message.
		var hubErr struct {
			Error struct {
				Message string                 `json:"message"`
				Code    string                 `json:"code"`
				Details map[string]interface{} `json:"details,omitempty"`
			} `json:"error"`
		}
		if decErr := json.NewDecoder(resp.Body).Decode(&hubErr); decErr == nil && hubErr.Error.Message != "" {
			errDetail := hubErr.Error.Message
			if rem, ok := hubErr.Error.Details["remediation"].(string); ok && rem != "" {
				errDetail += " " + rem
			}
			return resp.StatusCode, errDetail
		}
		return resp.StatusCode, fmt.Sprintf("delivery failed (HTTP %d)", resp.StatusCode)
	}

	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, ""
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
// An optional recipientUsername (e.g. "bob585") appends " → @bob585" to the
// header when the message is agent-to-user, making the target clear in groups.
func FormatMessageV2(msg *messages.StructuredMessage, agentSlug string, recipientUsername ...string) string {
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

	// For agent-to-agent messages, show: "👀 🤖sender → 🤖recipient 👀"
	if strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "agent:") {
		senderSlug := strings.TrimPrefix(msg.Sender, "agent:")
		recipientSlug := strings.TrimPrefix(msg.Recipient, "agent:")
		fmt.Fprintf(&b, "👀 🤖 %s → 🤖 %s 👀", senderSlug, recipientSlug)
	} else if agentSlug != "" {
		fmt.Fprintf(&b, "🤖 %s", agentSlug)
		if len(recipientUsername) > 0 && recipientUsername[0] != "" &&
			strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "user:") {
			fmt.Fprintf(&b, " → @%s", recipientUsername[0])
		}
	} else if strings.HasPrefix(msg.Sender, "agent:") {
		slug := strings.TrimPrefix(msg.Sender, "agent:")
		fmt.Fprintf(&b, "🤖 %s", slug)
		if len(recipientUsername) > 0 && recipientUsername[0] != "" &&
			strings.HasPrefix(msg.Recipient, "user:") {
			fmt.Fprintf(&b, " → @%s", recipientUsername[0])
		}
	} else {
		b.WriteString(msg.Sender)
	}

	// No type qualifier — keep the header clean.

	if msg.Status != "" {
		fmt.Fprintf(&b, " [%s]", msg.Status)
	}

	b.WriteString("\n\n")
	b.WriteString(unescapeNewlines(msg.Msg))

	text := b.String()
	return truncateMessage(text)
}

// extractUserIDFromTopic extracts the user ID from a topic of the form
// scion.grove.<id>.user.<userid>.messages or scion.project.<id>.user.<userid>.messages.
func extractUserIDFromTopic(topic string) string {
	parts := strings.Split(topic, ".")
	for i, p := range parts {
		if p == "user" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
