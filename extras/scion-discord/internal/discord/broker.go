// Package discord implements a Discord bot message broker plugin for Scion.
// It provides bidirectional messaging between Discord channels and Scion agents:
//   - Outbound: Hub publishes StructuredMessages which are formatted and sent
//     to Discord channels via the Discord API / gateway session.
//   - Inbound: Discord messages received via the Gateway WebSocket are converted
//     to StructuredMessages and forwarded to the hub's inbound endpoint.
package discord

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

const (
	defaultAgentCacheTTL = 5 * time.Minute
	defaultDBPath        = "discord.db"

	// dedupTTL is how long a message ID is remembered for deduplication.
	dedupTTL = 5 * time.Minute

	// OriginMarkerKey is the config key injected into outbound messages
	// to identify messages originating from the scion hub.
	OriginMarkerKey = "scion_origin"

	// OriginMarkerValue is the marker value for hub-originated messages.
	OriginMarkerValue = "hub"
)

// Config holds Discord-specific configuration parsed from the plugin config map.
type Config struct {
	BotToken       string
	ApplicationID  string
	PublicKey      string
	GuildID        string // empty = global commands
	DBPath         string
	MentionRouting bool
}

// inboundPayload is the JSON body sent to the hub API inbound endpoint.
type inboundPayload struct {
	Topic   string                      `json:"topic"`
	Message *messages.StructuredMessage `json:"message"`
}

// hubError represents a structured error returned by the hub API.
type hubError struct {
	StatusCode int
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *hubError) Error() string {
	return fmt.Sprintf("hub error %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

// userFacingMessage returns a short message suitable for displaying to chat users.
func (e *hubError) userFacingMessage() string {
	switch e.Code {
	case "agent_not_found":
		return "Target agent not found. Use `/scion agents` to see available agents."
	case "forbidden":
		return "You don't have permission to message this agent."
	case "broker_auth_failed", "unauthorized":
		return "Authentication error — please contact an administrator."
	default:
		return "Failed to deliver message. Please try again or contact an administrator."
	}
}

// parseHubError reads and parses a hub API error response.
func parseHubError(resp *http.Response) *hubError {
	he := &hubError{StatusCode: resp.StatusCode}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(body) == 0 {
		he.Code = "unknown"
		he.Message = http.StatusText(resp.StatusCode)
		return he
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error.Code == "" {
		he.Code = "unknown"
		he.Message = http.StatusText(resp.StatusCode)
		return he
	}
	he.Code = envelope.Error.Code
	he.Message = envelope.Error.Message
	return he
}

// DiscordBroker implements plugin.MessageBrokerPluginInterface with
// Discord Gateway WebSocket, slash commands, message components, and
// persistent SQLite state.
type DiscordBroker struct {
	mu     sync.RWMutex
	closed bool
	log    *slog.Logger

	session *discordgo.Session // Discord gateway session
	botUser *discordgo.User    // Bot's own user info

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

	sentIDs   map[string]time.Time
	sentIDsMu sync.Mutex

	sendQueue *SendQueue
	webhooks  *WebhookManager

	threadParents map[string]string // channelID -> parentID (cached thread lookups)

	agentCacheTTL  time.Duration
	projectSlugMap map[string]string // injected by hub: projectID -> slug

	config *Config

	hostCallbacks plugin.HostCallbacks

	InboundHandler func(topic string, msg *messages.StructuredMessage)
}

// NewBroker creates a new DiscordBroker with the given logger.
func NewBroker(log *slog.Logger) *DiscordBroker {
	if log == nil {
		log = slog.Default()
	}
	return &DiscordBroker{
		subs:          make(map[string]bool),
		sentIDs:       make(map[string]time.Time),
		log:           log,
		pluginName:    "discord",
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		agentCacheTTL: defaultAgentCacheTTL,
	}
}

// SetHostCallbacks implements plugin.HostCallbacksAware, allowing the
// host to inject a reverse-channel for dynamic subscription management.
func (b *DiscordBroker) SetHostCallbacks(hc plugin.HostCallbacks) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hostCallbacks = hc
}

// Configure sets up the Discord broker from the provided config map.
// This is called in two phases:
//   - Phase 1 (bot_token present): Creates discordgo.Session, inits SQLite store,
//     parses Discord-specific config. Does NOT call session.Open() yet.
//   - Phase 2 (hub_url present): Sets hub credentials, creates HubClient and
//     component handlers, resolves stale project slugs.
func (b *DiscordBroker) Configure(config map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Extract hub credentials (may arrive in either phase).
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

	// Phase 1: Bot token configuration.
	botToken, hasBotToken := config["bot_token"]
	if hasBotToken && botToken != "" {
		// Create a discordgo session but do NOT open the gateway yet.
		// Gateway connection happens on first Subscribe().
		session, err := discordgo.New("Bot " + botToken)
		if err != nil {
			return fmt.Errorf("create discord session: %w", err)
		}

		// Configure gateway intents.
		session.Identify.Intents = discordgo.IntentsGuilds |
			discordgo.IntentsGuildMessages |
			discordgo.IntentsDirectMessages |
			discordgo.IntentsMessageContent

		b.session = session

		// Parse Discord-specific config.
		cfg := &Config{
			BotToken:       botToken,
			ApplicationID:  config["application_id"],
			PublicKey:      config["public_key"],
			GuildID:        config["guild_id"],
			MentionRouting: true, // default
		}

		if v, ok := config["mention_routing"]; ok && v != "" {
			cfg.MentionRouting = v != "false" && v != "0"
		}

		cfg.DBPath = config["db_path"]
		if cfg.DBPath == "" {
			cfg.DBPath = defaultDBPath
		}
		b.config = cfg

		// Initialize store: use PostgreSQL when hub injects database config,
		// otherwise fall back to SQLite.
		dbDriver, hasDriver := config["database_driver"]
		dbURL, hasURL := config["database_url"]
		if hasDriver && dbDriver == "postgres" && hasURL && dbURL != "" {
			store, err := NewPostgresStore(dbURL)
			if err != nil {
				return fmt.Errorf("init postgres store: %w", err)
			}
			b.store = store
			b.log.Info("Using PostgreSQL store for Discord broker")
		} else {
			store, err := NewSQLiteStore(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("init sqlite store: %w", err)
			}
			b.store = store
		}

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
		b.sendQueue = NewSendQueue(session, b.log, sqSize, sqDelay)

		// Initialize webhook manager for per-agent identity.
		b.webhooks = NewWebhookManager(session, b.log)

		// Parse optional agent cache TTL.
		if v, ok := config["agent_cache_ttl"]; ok && v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("invalid agent_cache_ttl: %w", err)
			}
			b.agentCacheTTL = d
		}

		b.log.Info("Discord broker phase 1 configured",
			"application_id", cfg.ApplicationID,
			"guild_id", cfg.GuildID,
			"db_path", cfg.DBPath,
			"mention_routing", cfg.MentionRouting,
		)
	}

	// Phase 2: Hub credentials and component handlers.
	if b.hubURL != "" && b.session != nil {
		// Create hub client.
		b.hubClient = NewHTTPHubClient(b.hubURL, b.hmacKey, b.brokerID)

		// Create component handlers.
		appID := ""
		guildID := ""
		if b.config != nil {
			appID = b.config.ApplicationID
			guildID = b.config.GuildID
		}
		b.commands = NewCommandHandler(b.store, b.session, b.hubClient, b.deliverInbound, appID, guildID, b.agentCacheTTL, b.log)
		b.callbacks = NewCallbackHandler(b.store, b.session, b.hubClient, b.deliverInbound, b.log)
		b.registration = NewRegistrationHandler(b.store, b.session, b.hubURL, b.hmacKey, b.brokerID, b.log)

		// Parse hub-injected project slug map (projectID -> slug).
		if slugMapJSON, ok := config["project_slug_map"]; ok && slugMapJSON != "" {
			var m map[string]string
			if err := json.Unmarshal([]byte(slugMapJSON), &m); err == nil {
				b.projectSlugMap = m
			}
		}

		// Resolve stale channel link slugs that were stored as UUIDs.
		if len(b.projectSlugMap) > 0 {
			slugCtx, slugCancel := context.WithTimeout(context.Background(), 15*time.Second)
			b.resolveStaleChannelSlugs(slugCtx)
			slugCancel()
		}

		b.log.Info("Discord broker phase 2 configured",
			"hub_url", b.hubURL,
			"broker_id", b.brokerID,
		)

		// Bootstrap Gateway: request a wildcard subscription so the Hub calls
		// Subscribe(), which triggers startGateway() on the first call.
		// Host callbacks are wired after Configure() returns, so we defer
		// the request in a goroutine that retries until they're available.
		go func() {
			for i := 0; i < 20; i++ {
				time.Sleep(500 * time.Millisecond)
				b.mu.RLock()
				hc := b.hostCallbacks
				b.mu.RUnlock()
				if hc == nil {
					continue
				}
				if err := hc.RequestSubscription(projectcompat.AllProjectsPattern()); err != nil {
					b.log.Warn("Failed to request bootstrap subscription", "error", err)
					continue
				}
				b.log.Info("Requested bootstrap subscription for Discord Gateway")
				return
			}
			b.log.Error("Bootstrap subscription timed out — host callbacks never became available")
		}()
	}

	return nil
}

// Subscribe records a subscription pattern and starts the Discord gateway
// connection on the first subscribe call.
func (b *DiscordBroker) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("discord broker is closed")
	}

	if b.subs[pattern] {
		return nil
	}

	wasEmpty := len(b.subs) == 0
	b.subs[pattern] = true

	// Open gateway connection on first subscription.
	if wasEmpty && b.session != nil {
		if err := b.startGateway(); err != nil {
			delete(b.subs, pattern)
			return fmt.Errorf("start discord gateway: %w", err)
		}
	}

	b.log.Debug("Subscription registered", "pattern", pattern)
	return nil
}

// Unsubscribe removes a subscription pattern. When all subscriptions are
// removed, the gateway connection is closed.
func (b *DiscordBroker) Unsubscribe(pattern string) error {
	b.mu.Lock()

	if !b.subs[pattern] {
		b.mu.Unlock()
		return nil
	}

	delete(b.subs, pattern)
	shouldStop := len(b.subs) == 0
	session := b.session

	b.mu.Unlock()

	if shouldStop && session != nil {
		if err := session.Close(); err != nil {
			b.log.Warn("Failed to close discord gateway", "error", err)
		}
		b.log.Info("Discord gateway closed (no subscriptions)")
	}

	b.log.Debug("Subscription removed", "pattern", pattern)
	return nil
}

// Publish sends a message to Discord channels using dynamic routing.
// Routing priority:
//  1. Direct channel ID from metadata (discord_channel_id)
//  2. ConversationContext for recipient
//  3. Broadcast to all ChannelLinks for project
func (b *DiscordBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return fmt.Errorf("discord broker is closed")
	}
	session := b.session
	store := b.store
	sendQueue := b.sendQueue
	webhooks := b.webhooks
	b.mu.RUnlock()

	if session == nil {
		return fmt.Errorf("discord broker not configured")
	}

	if msg == nil {
		return fmt.Errorf("message is nil")
	}

	// Channel filtering: if the message targets a specific channel that
	// isn't ours, skip it. FanOutEventBus already does this, but
	// belt-and-suspenders.
	if msg != nil && msg.Channel != "" && msg.Channel != "discord" {
		return nil
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

	// Collect target channel IDs via dynamic routing.
	var channelIDs []string

	// Priority 0: Thread routing — ThreadID maps directly to a Discord
	// channel or thread snowflake. This takes precedence over all other
	// routing so replies land in the same channel/thread as the original.
	if msg != nil && msg.ThreadID != "" {
		channelIDs = append(channelIDs, msg.ThreadID)
	}

	// Priority 1: Direct channel ID from metadata.
	if len(channelIDs) == 0 && msg != nil && msg.Metadata != nil {
		if chID, ok := msg.Metadata["discord_channel_id"]; ok && chID != "" {
			channelIDs = append(channelIDs, chID)
		}
	}

	// Priority 2: Look up via ConversationContext for the recipient.
	if len(channelIDs) == 0 && msg != nil && msg.Recipient != "" && store != nil {
		ccSlug := agentSlug
		if ccSlug == "" && msg.Sender != "" && strings.HasPrefix(msg.Sender, "agent:") {
			ccSlug = strings.TrimPrefix(msg.Sender, "agent:")
		}
		channelIDs = b.resolveRecipientChannels(ctx, msg.Recipient, projectID, ccSlug)
	}

	// Priority 3: Broadcast to all ChannelLinks for the project.
	if len(channelIDs) == 0 && projectID != "" && store != nil {
		links, err := store.GetChannelLinksForProject(ctx, projectID)
		if err != nil {
			b.log.Warn("Failed to get channel links for broadcast", "project_id", projectID, "error", err)
		}
		for _, link := range links {
			if link.Active {
				channelIDs = append(channelIDs, link.ChannelID)
			}
		}
	}

	if len(channelIDs) == 0 {
		b.log.Debug("No Discord channel for topic, dropping message", "topic", topic)
		return nil
	}

	// Always suppress commentary messages — Discord has no user toggle for this.
	if msg != nil && msg.Type == messages.TypeAssistantReply {
		b.log.Debug("Filtering assistant-reply message (commentary always suppressed in Discord)")
		return nil
	}

	// Determine whether this message should be sent via webhook (agent identity)
	// or via the bot API. Webhook routing applies when:
	//   - Sender is an agent (starts with "agent:")
	//   - Message type is TypeInstruction
	// State changes and input-needed messages keep the bot identity (embed style).
	useWebhook := webhooks != nil &&
		strings.HasPrefix(msg.Sender, "agent:") &&
		msg.Type == messages.TypeInstruction

	// Extract agent slug from sender for webhook username.
	senderSlug := agentSlug
	if senderSlug == "" && strings.HasPrefix(msg.Sender, "agent:") {
		senderSlug = strings.TrimPrefix(msg.Sender, "agent:")
	}

	// Format the message text. When sending via webhook, the webhook username
	// already shows the agent name, so we skip the agent name header and just
	// send the body with prefix tags.
	var text string
	if useWebhook {
		text = formatWebhookMessage(msg)
	} else {
		text = formatMessage(msg, agentSlug)
	}
	if text == "" {
		return nil
	}

	// Open attachment files if present. Read files into memory once so
	// we can create a fresh bytes.Reader for each target channel.
	type attachmentData struct {
		name string
		data []byte
	}
	var attachments []attachmentData
	if msg != nil && len(msg.Attachments) > 0 {
		for _, raw := range msg.Attachments {
			if raw == "" {
				continue
			}
			resolved := b.resolveAttachmentPath(ctx, raw, projectID)
			if resolved == "" {
				continue
			}
			fi, statErr := os.Stat(resolved)
			if statErr != nil {
				b.log.Error("Failed to stat attachment file", "path", resolved, "error", statErr)
				continue
			}
			if fi.Size() > 25*1024*1024 {
				b.log.Error("Attachment file too large for Discord", "path", resolved, "size", fi.Size())
				continue
			}
			data, readErr := os.ReadFile(resolved)
			if readErr != nil {
				b.log.Error("Failed to read attachment file",
					"path", resolved, "error", readErr)
				continue
			}
			attachments = append(attachments, attachmentData{
				name: filepath.Base(resolved),
				data: data,
			})
		}
	}

	// Per-channel filtering based on channel link settings.
	isAgentToAgent := msg != nil &&
		strings.HasPrefix(msg.Sender, "agent:") &&
		strings.HasPrefix(msg.Recipient, "agent:")
	isStateChange := msg != nil && msg.Type == messages.TypeStateChange
	needsFilter := isAgentToAgent || isStateChange

	// Send to each target channel.
	var errs []error
	for _, channelID := range channelIDs {
		// Forum and media channels (types 15, 16) are thread-only containers.
		// Sending without a thread ID would broadcast to an invalid target;
		// return an error so callers know a thread ID is required.
		if msg.ThreadID == "" && b.isForumChannel(channelID) {
			errs = append(errs, fmt.Errorf(
				"discord channel %s is a forum/media channel — a thread ID is required to send messages; omit the channel or specify a thread ID",
				channelID,
			))
			continue
		}

		if needsFilter && store != nil {
			link, linkErr := store.GetChannelLink(ctx, channelID)
			if linkErr == nil && link != nil {
				if isAgentToAgent && !link.ShowAgentToAgent {
					b.log.Debug("Filtering agent-to-agent message", "channel_id", channelID)
					continue
				}
				if isStateChange && !link.ShowStateChanges {
					b.log.Debug("Filtering state change notification", "channel_id", channelID)
					continue
				}
			}
		}

		// Build per-channel file slice; each channel gets a fresh reader.
		var files []*discordgo.File
		for _, a := range attachments {
			files = append(files, &discordgo.File{
				Name:   a.name,
				Reader: bytes.NewReader(a.data),
			})
		}

		var err error

		if useWebhook {
			// Send via webhook with per-agent identity.
			// For threads (forum channels, text channel threads), create the
			// webhook on the parent channel and execute with thread_id.
			parentID, isThread := b.resolveThreadParent(channelID)
			if isThread && parentID != "" {
				_, err = webhooks.SendAsAgentInThread(parentID, channelID, senderSlug, text, nil, nil, files)
			} else {
				_, err = webhooks.SendAsAgent(channelID, senderSlug, text, nil, nil, files)
			}
			if err != nil {
				// Fallback to bot API if webhook send fails.
				b.log.Warn("Webhook send failed, falling back to bot API",
					"channel_id", channelID,
					"agent", senderSlug,
					"error", err)
				botText := formatMessage(msg, agentSlug)
				// Reset file readers for fallback send.
				files = nil
				for _, a := range attachments {
					files = append(files, &discordgo.File{
						Name:   a.name,
						Reader: bytes.NewReader(a.data),
					})
				}
				if sendQueue != nil {
					_, err = sendQueue.Send(ctx, channelID, botText, nil, nil, files)
				} else {
					_, err = session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
						Content: botText,
						Files:   files,
					})
				}
			}
		} else {
			// Send via bot API (state changes, input-needed, non-agent messages).
			if sendQueue != nil {
				_, err = sendQueue.Send(ctx, channelID, text, nil, nil, files)
			} else {
				if len(files) > 0 {
					_, err = session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
						Content: text,
						Files:   files,
					})
				} else {
					_, err = session.ChannelMessageSend(channelID, text)
				}
			}
		}

		if err != nil {
			b.log.Error("Failed to send Discord message",
				"channel_id", channelID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close shuts down the Discord broker, closing the gateway session,
// draining the send queue, and closing the store.
func (b *DiscordBroker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.subs = make(map[string]bool)
	session := b.session
	store := b.store
	sendQueue := b.sendQueue
	b.mu.Unlock()

	if session != nil {
		if err := session.Close(); err != nil {
			b.log.Warn("Failed to close discord session", "error", err)
		}
	}

	if sendQueue != nil {
		sendQueue.Close()
	}

	if store != nil {
		store.Close()
	}

	b.log.Info("Discord broker closed")
	return nil
}

// GetInfo returns plugin metadata.
func (b *DiscordBroker) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:      "discord",
		Version:   "1.0.0",
		ChannelID: "discord",
		Capabilities: []string{
			"echo-filter",
			"gateway-websocket",
			"discord-bot-api",
			"user-registration",
			"slash-commands",
			"message-components",
			"channel-links",
			"mention-routing",
		},
	}, nil
}

// HealthCheck returns the runtime health of the Discord broker.
func (b *DiscordBroker) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return &plugin.HealthStatus{
			Status:  "unhealthy",
			Message: "broker is closed",
		}, nil
	}

	if b.session == nil {
		return &plugin.HealthStatus{
			Status:  "degraded",
			Message: "broker not configured",
		}, nil
	}

	details := map[string]string{
		"subscriptions": fmt.Sprintf("%d", len(b.subs)),
	}

	if b.botUser != nil {
		details["bot_username"] = b.botUser.Username + "#" + b.botUser.Discriminator
		details["bot_id"] = b.botUser.ID
	}
	if b.hubURL != "" {
		details["hub_url"] = b.hubURL
	}

	return &plugin.HealthStatus{
		Status:  "healthy",
		Message: "discord bot operational",
		Details: details,
	}, nil
}

// --- Gateway setup ---

// startGateway opens the Discord gateway WebSocket connection and
// registers event handlers. Must be called with b.mu held.
func (b *DiscordBroker) startGateway() error {
	session := b.session
	if session == nil {
		return fmt.Errorf("no discord session configured")
	}

	// Register gateway event handlers.
	session.AddHandler(b.handleReady)
	session.AddHandler(b.handleGuildCreate)
	session.AddHandler(b.handleGuildDelete)
	session.AddHandler(b.handleMessageCreate)
	session.AddHandler(b.handleInteractionCreate)

	// Open the gateway connection.
	if err := session.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}

	b.log.Info("Discord gateway connected")
	return nil
}

// --- Gateway event handlers ---

// handleReady is called when the bot connects to the Discord gateway.
func (b *DiscordBroker) handleReady(_ *discordgo.Session, r *discordgo.Ready) {
	b.mu.Lock()
	b.botUser = r.User
	commands := b.commands
	b.mu.Unlock()

	b.log.Info("Discord bot ready",
		"username", r.User.Username,
		"discriminator", r.User.Discriminator,
		"id", r.User.ID,
		"guilds", len(r.Guilds),
	)

	// Register slash commands once the gateway is connected.
	if commands != nil {
		if err := commands.RegisterCommands(); err != nil {
			b.log.Error("Failed to register slash commands", "error", err)
		}
	}
}

// handleGuildCreate is called when the bot joins a guild or when guild
// data is received during the initial gateway connection.
func (b *DiscordBroker) handleGuildCreate(_ *discordgo.Session, g *discordgo.GuildCreate) {
	b.log.Info("Discord guild available",
		"guild_id", g.ID,
		"guild_name", g.Name,
		"member_count", g.MemberCount,
	)
}

// handleGuildDelete is called when the bot is removed from a guild or
// when a guild becomes unavailable.
func (b *DiscordBroker) handleGuildDelete(_ *discordgo.Session, g *discordgo.GuildDelete) {
	b.log.Info("Discord guild unavailable",
		"guild_id", g.ID,
	)
}

// handleMessageCreate is called for every new message in channels the bot
// can see. It routes to handleIncomingMessage for processing.
func (b *DiscordBroker) handleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	b.handleIncomingMessage(s, m)
}

// handleInteractionCreate dispatches Discord interactions (slash commands,
// message components, modals, autocomplete) to the appropriate handler.
func (b *DiscordBroker) handleInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	b.mu.RLock()
	commands := b.commands
	callbacks := b.callbacks
	registration := b.registration
	b.mu.RUnlock()

	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		// Slash command.
		if commands != nil {
			data := i.ApplicationCommandData()
			b.log.Debug("Slash command received",
				"command", data.Name,
				"user", interactionUserID(i),
			)
			// Check if this is a register/unregister command handled by registration.
			if data.Name == "scion" && len(data.Options) > 0 {
				sub := data.Options[0].Name
				if (sub == "register" || sub == "unregister") && registration != nil {
					// Acknowledge immediately (ephemeral).
					_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Flags: discordgo.MessageFlagsEphemeral,
						},
					})
					go func() {
						if sub == "register" {
							registration.HandleRegister(s, i)
						} else {
							registration.HandleUnregister(s, i)
						}
					}()
					return
				}
			}
			commands.HandleSlashCommand(s, i)
		}

	case discordgo.InteractionMessageComponent:
		// Button press or select menu.
		if callbacks != nil {
			data := i.MessageComponentData()
			b.log.Debug("Message component interaction",
				"custom_id", data.CustomID,
				"user", interactionUserID(i),
			)

			// Special case: "ask:reply:" buttons open a modal, which must
			// be the FIRST interaction response. Do NOT pre-acknowledge
			// with DeferredMessageUpdate — the callback itself responds
			// with InteractionResponseModal.
			if strings.HasPrefix(data.CustomID, "ask:reply:") {
				go func() {
					callbacks.Dispatch(s, i, data.CustomID, data.Values)
				}()
			} else {
				// Acknowledge with deferred update for all other components.
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseDeferredMessageUpdate,
				})
				go func() {
					callbacks.Dispatch(s, i, data.CustomID, data.Values)
				}()
			}
		}

	case discordgo.InteractionModalSubmit:
		// Modal form submission.
		data := i.ModalSubmitData()
		b.log.Debug("Modal submit interaction",
			"custom_id", data.CustomID,
			"user", interactionUserID(i),
		)

		if strings.HasPrefix(data.CustomID, "ask:") {
			// Acknowledge with deferred ephemeral message so we can
			// send a follow-up after processing.
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags: discordgo.MessageFlagsEphemeral,
				},
			})

			store := b.store
			go func() {
				HandleModalSubmit(s, i, store, b.deliverInbound, b.log)
			}()
		}

	case discordgo.InteractionApplicationCommandAutocomplete:
		// Autocomplete for slash command options.
		if commands != nil {
			b.log.Debug("Autocomplete interaction",
				"command", i.ApplicationCommandData().Name,
				"user", interactionUserID(i),
			)
			commands.HandleAutocomplete(s, i)
		}
	}
}

// --- Inbound message handling ---

// handleIncomingMessage processes an incoming Discord message through the
// three-tier @-mention routing system and delivers to the hub.
func (b *DiscordBroker) handleIncomingMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}

	if m.Content == "" {
		return
	}

	b.mu.RLock()
	store := b.store
	botUser := b.botUser
	b.mu.RUnlock()

	channelID := m.ChannelID

	if store == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := store.GetChannelLink(ctx, channelID)
	if err != nil {
		b.log.Error("Failed to get channel link", "channel_id", channelID, "error", err)
		return
	}
	// If no direct link, check if this is a thread whose parent channel is linked.
	if link == nil || !link.Active {
		if parentID, isThread := b.resolveThreadParent(channelID); isThread && parentID != "" {
			link, err = store.GetChannelLink(ctx, parentID)
			if err != nil {
				b.log.Error("Failed to get parent channel link", "parent_id", parentID, "error", err)
				return
			}
		}
	}
	if link == nil || !link.Active {
		return
	}

	botUserID := ""
	if botUser != nil {
		botUserID = botUser.ID
	}

	// Get project agents (with cache refresh).
	agents := b.getProjectAgents(ctx, link.ProjectID)

	// Three-tier @-mention routing.
	targets, _ := resolveTargetAgents(m, botUserID, link.DefaultAgent, agents)

	// Fallback: reply-to-bot message — extract agent from webhook username.
	if len(targets) == 0 && m.ReferencedMessage != nil {
		slug := agentFromReply(m.ReferencedMessage, botUserID)
		if slug != "" {
			targets = []string{slug}
		}
	}

	// Fallback: unaddressed text → default agent (if configured).
	if len(targets) == 0 && link.DefaultAgent != "" {
		text := strings.TrimSpace(m.Content)
		if text != "" && !strings.HasPrefix(text, "/") {
			targets = []string{link.DefaultAgent}
		}
	}

	if len(targets) == 0 {
		// If bot was mentioned but no agent resolved, send error feedback.
		if isBotMentioned(m, botUserID) {
			unresolved := extractUnresolvedMentions(m.Content, botUserID, agents)
			if len(unresolved) > 0 {
				errMsg := fmt.Sprintf("Unknown agent: %q. Use `/scion agents` to see available agents.", unresolved[0])
				s.ChannelMessageSend(channelID, errMsg)
			}
		}
		return
	}

	// Determine sender identity.
	sender := "discord:" + m.Author.Username
	senderID := m.Author.ID

	mapping, err := store.GetUserMapping(ctx, senderID)
	if err == nil && mapping != nil && mapping.ScionEmail != "" {
		sender = "user:" + mapping.ScionEmail
	} else if mapping == nil {
		b.log.Debug("Unregistered user tried to mention agent", "sender_id", senderID)
		s.ChannelMessageSend(channelID, "Please use `/scion register` first to interact with agents.")
		return
	}

	// Strip bot and agent mentions from message text.
	cleanText := stripMentions(m.Content, botUserID, targets)
	cleanText = strings.TrimSpace(cleanText)
	if cleanText == "" {
		return
	}

	// Deliver to each target agent.
	for _, agentSlug := range targets {
		cc := &ConversationContext{
			DiscordUserID: senderID,
			ProjectID:     link.ProjectID,
			AgentSlug:     agentSlug,
			LastChannelID: channelID,
			LastMessageAt: time.Now(),
		}
		if err := store.SetConversationContext(ctx, cc); err != nil {
			b.log.Warn("Failed to save conversation context", "error", err)
		}

		topic := projectcompat.AgentTopic(link.ProjectID, agentSlug)
		recipient := "agent:" + agentSlug

		msg := &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: m.Timestamp.UTC().Format(time.RFC3339),
			Channel:   "discord",
			ThreadID:  channelID,
			Sender:    sender,
			SenderID:  senderID,
			Recipient: recipient,
			Msg:       cleanText,
			Type:      messages.TypeInstruction,
			Metadata: map[string]string{
				"discord_channel_id": channelID,
				"discord_message_id": m.ID,
				"discord_guild_id":   m.GuildID,
				"project_id":         link.ProjectID,
			},
		}

		if isEcho(msg) {
			b.log.Debug("Filtered echo message via origin marker", "topic", topic)
			continue
		}

		b.log.Debug("Delivering inbound message",
			"topic", topic, "sender", sender, "agent", agentSlug)

		if he := b.deliverInbound(topic, msg); he != nil {
			s.ChannelMessageSend(channelID, he.userFacingMessage())
		}
	}
}

// --- Hub delivery ---

// deliverInbound sends a message to the hub API or InboundHandler.
// Returns a non-nil *hubError when the hub rejects the message with an HTTP
// error status (4xx/5xx), allowing callers to surface feedback to the sender.
func (b *DiscordBroker) deliverInbound(topic string, msg *messages.StructuredMessage) *hubError {
	b.mu.RLock()
	handler := b.InboundHandler
	hubURL := b.hubURL
	hmacKey := b.hmacKey
	brokerID := b.brokerID
	pluginName := b.pluginName
	b.mu.RUnlock()

	if handler != nil {
		handler(topic, msg)
		return nil
	}

	if hubURL == "" {
		b.log.Debug("No hub URL configured, dropping inbound message", "topic", topic)
		return nil
	}

	payload := inboundPayload{
		Topic:   topic,
		Message: msg,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		b.log.Error("Failed to marshal inbound message", "error", err)
		return nil
	}

	inboundURL := hubURL + "/api/v1/broker/inbound"
	req, err := http.NewRequest("POST", inboundURL, bytes.NewReader(body))
	if err != nil {
		b.log.Error("Failed to create inbound request", "error", err)
		return nil
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Plugin-Name", pluginName)

	if brokerID != "" && hmacKey != "" {
		if err := signInboundRequest(req, brokerID, hmacKey); err != nil {
			b.log.Error("Failed to sign inbound request", "error", err)
			return nil
		}
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.log.Error("Failed to deliver inbound message", "error", err, "topic", topic)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		he := parseHubError(resp)
		b.log.Error("Hub rejected inbound message",
			"status", resp.StatusCode, "code", he.Code, "message", he.Message, "topic", topic)
		return he
	}

	io.Copy(io.Discard, resp.Body)
	return nil
}

// --- Agent cache ---

// getProjectAgents returns the cached agent slugs for a project, refreshing
// from the Hub API if the cache is stale.
func (b *DiscordBroker) getProjectAgents(ctx context.Context, projectID string) []string {
	b.mu.RLock()
	store := b.store
	hubClient := b.hubClient
	ttl := b.agentCacheTTL
	b.mu.RUnlock()

	if store == nil {
		return nil
	}

	cached, err := store.GetProjectAgents(ctx, projectID)
	if err != nil {
		b.log.Warn("Failed to read agent cache", "project_id", projectID, "error", err)
	}
	if cached != nil && time.Since(cached.RefreshedAt) < ttl {
		return cached.AgentSlugs
	}

	if hubClient == nil {
		if cached != nil {
			return cached.AgentSlugs
		}
		return nil
	}

	agents, err := hubClient.ListAgents(ctx, projectID)
	if err != nil {
		b.log.Warn("Failed to refresh agent list from hub", "project_id", projectID, "error", err)
		if cached != nil {
			return cached.AgentSlugs
		}
		return nil
	}

	slugs := agentSlugs(agents)
	saveErr := store.SetProjectAgents(ctx, &ProjectAgents{
		ProjectID:   projectID,
		AgentSlugs:  slugs,
		RefreshedAt: time.Now(),
	})
	if saveErr != nil {
		b.log.Warn("Failed to cache agents", "project_id", projectID, "error", saveErr)
	}

	return slugs
}

// --- Dynamic subscription management ---

func (b *DiscordBroker) subscribeForProject(projectID string) {
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

func (b *DiscordBroker) unsubscribeForProject(projectID string) {
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

// --- Routing helpers ---

// resolveThreadParent checks if a Discord channel ID refers to a thread.
// If so, it returns the parent channel ID. Otherwise it returns empty strings.
// Results are cached to avoid repeated API calls.
func (b *DiscordBroker) resolveThreadParent(channelID string) (parentID string, isThread bool) {
	// Check cache first.
	b.mu.RLock()
	if parent, ok := b.threadParents[channelID]; ok {
		b.mu.RUnlock()
		return parent, parent != ""
	}
	b.mu.RUnlock()

	session := b.session
	if session == nil {
		return "", false
	}

	// Try the local state cache first to avoid REST API rate limits.
	var ch *discordgo.Channel
	var err error
	if session.State != nil {
		ch, err = session.State.Channel(channelID)
	}
	if ch == nil || err != nil {
		ch, err = session.Channel(channelID)
		if err != nil {
			return "", false
		}
	}

	// Thread types: GuildPublicThread (11), GuildPrivateThread (12), GuildNewsThread (10)
	if ch.Type == discordgo.ChannelTypeGuildPublicThread ||
		ch.Type == discordgo.ChannelTypeGuildPrivateThread ||
		ch.Type == discordgo.ChannelTypeGuildNewsThread {
		b.mu.Lock()
		if b.threadParents == nil {
			b.threadParents = make(map[string]string)
		}
		b.threadParents[channelID] = ch.ParentID
		b.mu.Unlock()
		return ch.ParentID, true
	}

	// Not a thread — cache negative result as empty string.
	b.mu.Lock()
	if b.threadParents == nil {
		b.threadParents = make(map[string]string)
	}
	b.threadParents[channelID] = ""
	b.mu.Unlock()
	return "", false
}

// isForumChannel checks whether a Discord channel ID refers to a forum or media
// channel (types 15 and 16). These channel types are thread-only containers and
// cannot receive messages directly — a thread ID is required.
func (b *DiscordBroker) isForumChannel(channelID string) bool {
	session := b.session
	if session == nil {
		return false
	}

	var ch *discordgo.Channel
	var err error
	if session.State != nil {
		ch, err = session.State.Channel(channelID)
	}
	if ch == nil || err != nil {
		// Fall back to REST API only if the session has a Ratelimiter
		// (i.e. is fully initialized). Avoids panics in tests and during
		// early startup.
		if session.Ratelimiter == nil {
			return false
		}
		ch, err = session.Channel(channelID)
		if err != nil || ch == nil {
			return false
		}
	}

	return ch.Type == discordgo.ChannelTypeGuildForum ||
		ch.Type == discordgo.ChannelTypeGuildMedia
}

// resolveRecipientChannels looks up target channels for a specific recipient.
func (b *DiscordBroker) resolveRecipientChannels(ctx context.Context, recipient, projectID, agentSlug string) []string {
	email := strings.TrimPrefix(recipient, "user:")
	if email == recipient {
		return nil
	}

	b.mu.RLock()
	store := b.store
	b.mu.RUnlock()

	if store == nil {
		return nil
	}

	mapping, err := store.GetUserMappingByEmail(ctx, email)
	if err != nil || mapping == nil {
		return nil
	}

	// Try exact agent match first.
	if agentSlug != "" {
		cc, err := store.GetConversationContext(ctx, mapping.DiscordUserID, projectID, agentSlug)
		if err == nil && cc != nil {
			return []string{cc.LastChannelID}
		}
	}

	// Fallback: latest conversation context for this user+project (any agent).
	cc, err := store.GetLatestConversationContext(ctx, mapping.DiscordUserID, projectID)
	if err == nil && cc != nil {
		return []string{cc.LastChannelID}
	}

	return nil
}

// resolveStaleChannelSlugs updates ChannelLinks where ProjectSlug equals
// ProjectID (i.e., slug was not resolved during initial import).
func (b *DiscordBroker) resolveStaleChannelSlugs(ctx context.Context) {
	if len(b.projectSlugMap) == 0 {
		b.log.Debug("Slug resolution skipped: no project_slug_map injected by hub")
		return
	}

	if b.store == nil {
		return
	}

	links, err := b.store.GetAllChannelLinks(ctx)
	if err != nil {
		b.log.Warn("Could not list channel links for slug resolution", "error", err)
		return
	}
	for _, link := range links {
		if link.ProjectSlug == link.ProjectID {
			if slug, ok := b.projectSlugMap[link.ProjectID]; ok {
				link.ProjectSlug = slug
				if err := b.store.UpdateChannelLink(ctx, link); err != nil {
					b.log.Warn("Failed to update channel link slug",
						"channel_id", link.ChannelID, "error", err)
				} else {
					b.log.Info("Resolved channel link project slug",
						"channel_id", link.ChannelID,
						"project_id", link.ProjectID,
						"slug", slug)
				}
			}
		}
	}
}

// --- Attachment helpers ---

// resolveAttachmentPath translates an agent-relative /workspace path to the
// host-side path under /home/scion/.scion/projects/<slug>/. Agent containers
// mount /home/scion/.scion/projects/<slug> as /workspace.
// Returns empty string if the path is unsafe or cannot be resolved.
func (b *DiscordBroker) resolveAttachmentPath(ctx context.Context, attachPath, projectID string) string {
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
		b.log.Warn("Rejecting absolute attachment path outside /workspace",
			"attach_path", attachPath)
		return ""
	}

	relPath = filepath.Clean(relPath)
	if strings.HasPrefix(relPath, "..") || (filepath.IsAbs(relPath) && relPath != ".") {
		b.log.Warn("Attachment path escapes workspace, rejecting",
			"attach_path", attachPath, "rel_path", relPath)
		return ""
	}

	// Look up project slug from channel links, falling back to the injected slug map.
	slug := ""
	if b.store != nil && projectID != "" {
		links, err := b.store.GetChannelLinksForProject(ctx, projectID)
		if err == nil && len(links) > 0 && links[0].ProjectSlug != "" {
			slug = links[0].ProjectSlug
		}
	}
	if slug == "" && projectID != "" {
		slug = b.projectSlugMap[projectID]
	}
	if slug == "" {
		b.log.Warn("Cannot resolve attachment path: no project slug found",
			"attach_path", attachPath, "project_id", projectID)
		return ""
	}

	projectDir := filepath.Join("/home/scion/.scion/projects", slug)
	var hostPath string
	if relPath == "." {
		hostPath = projectDir
	} else {
		hostPath = filepath.Join(projectDir, relPath)
		if !strings.HasPrefix(hostPath, projectDir+"/") {
			b.log.Warn("Resolved attachment path escapes project directory, rejecting",
				"host_path", hostPath, "expected_prefix", projectDir+"/")
			return ""
		}
	}

	b.log.Debug("Resolved attachment path", "original", attachPath, "resolved", hostPath)
	return hostPath
}

// --- Topic parsing ---

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

// --- Message formatting ---

// formatWebhookMessage formats a StructuredMessage for sending via webhook.
// The webhook username already displays the agent name, so this function
// omits the agent name header and just sends the body with prefix tags.
func formatWebhookMessage(msg *messages.StructuredMessage) string {
	if msg == nil {
		return ""
	}

	var b strings.Builder

	// Prefix tags are kept — they carry important context.
	if msg.Urgent {
		b.WriteString("**[URGENT]** ")
	}
	if msg.Broadcasted {
		b.WriteString("**[Broadcast]** ")
	}

	// For agent-to-agent messages, show the recipient (the sender is in
	// the webhook username already).
	if strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "agent:") {
		recipientSlug := strings.TrimPrefix(msg.Recipient, "agent:")
		fmt.Fprintf(&b, "→ **%s**\n", recipientSlug)
	}

	// Status tag (e.g. [RUNNING], [COMPLETED]).
	if msg.Status != "" {
		fmt.Fprintf(&b, "[%s] ", msg.Status)
	}

	// Body text.
	b.WriteString(msg.Msg)

	return truncateMessage(b.String())
}

// formatMessage formats a StructuredMessage for Discord plain text output.
// Used for bot API sends where agent identity needs to be in the message text.
func formatMessage(msg *messages.StructuredMessage, agentSlug string) string {
	if msg == nil {
		return ""
	}

	var b strings.Builder

	if msg.Urgent {
		b.WriteString("**[URGENT]** ")
	}
	if msg.Broadcasted {
		b.WriteString("**[Broadcast]** ")
	}

	// Header with agent identity.
	if strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "agent:") {
		senderSlug := strings.TrimPrefix(msg.Sender, "agent:")
		recipientSlug := strings.TrimPrefix(msg.Recipient, "agent:")
		fmt.Fprintf(&b, "**%s** -> **%s**", senderSlug, recipientSlug)
	} else if agentSlug != "" {
		fmt.Fprintf(&b, "**%s**", agentSlug)
	} else if strings.HasPrefix(msg.Sender, "agent:") {
		slug := strings.TrimPrefix(msg.Sender, "agent:")
		fmt.Fprintf(&b, "**%s**", slug)
	} else {
		b.WriteString(msg.Sender)
	}

	if msg.Status != "" {
		fmt.Fprintf(&b, " [%s]", msg.Status)
	}

	b.WriteString("\n")
	b.WriteString(msg.Msg)

	text := b.String()
	return truncateMessage(text)
}

// truncateMessage ensures the message fits within Discord's 2000-character limit.
func truncateMessage(text string) string {
	const maxLen = 2000
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen-4] + "\n..."
}

// --- Dedup helpers ---

// isEcho returns true if the message was tagged with the scion origin marker.
func isEcho(msg *messages.StructuredMessage) bool {
	if msg == nil {
		return false
	}
	return strings.HasPrefix(msg.Sender, OriginMarkerKey+":"+OriginMarkerValue+":")
}

// msgDedupKey returns a stable fingerprint for a message, used to detect
// duplicate deliveries of the same logical message.
func msgDedupKey(msg *messages.StructuredMessage) string {
	if msg == nil || msg.Msg == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(msg.Sender))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Recipient))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Timestamp))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Type))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Msg))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// pruneSentIDsLocked removes dedup entries older than dedupTTL.
func (b *DiscordBroker) pruneSentIDsLocked() {
	now := time.Now()
	for k, t := range b.sentIDs {
		if now.Sub(t) > dedupTTL {
			delete(b.sentIDs, k)
		}
	}
}

// --- HMAC auth helpers ---

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

// generateRequestID generates a random hex request ID.
func generateRequestID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// agentSlugs extracts slug strings from a slice of AgentInfo.
func agentSlugs(agents []AgentInfo) []string {
	slugs := make([]string, len(agents))
	for i, a := range agents {
		slugs[i] = a.Slug
	}
	return slugs
}
