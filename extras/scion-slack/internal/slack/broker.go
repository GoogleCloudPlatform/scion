package slack

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
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

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

const (
	defaultAgentCacheTTL = 5 * time.Minute
	defaultDBPath        = "slack.db"
	dedupTTL             = 5 * time.Minute
)

// SlackConfig holds Slack-specific configuration parsed from the plugin config map.
type SlackConfig struct {
	BotToken      string
	AppToken      string
	SigningSecret string
	SocketMode    bool
	ListenAddress string
	DBPath        string
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

// SlackBroker implements plugin.MessageBrokerPluginInterface with
// Slack Bot API, Socket Mode / HTTP webhooks, slash commands, interactive
// components, and persistent SQLite state.
type SlackBroker struct {
	mu     sync.RWMutex
	closed bool
	log    *slog.Logger

	client       *slackapi.Client
	socketClient *socketmode.Client
	botUserID    string

	hubURL     string
	hmacKey    string
	brokerID   string
	pluginName string
	httpClient *http.Client

	store        Store
	hubClient    HubClient
	registration *RegistrationHandler

	subs map[string]bool

	sentIDs   map[string]time.Time
	sentIDsMu sync.Mutex

	events *eventServer

	agentCacheTTL  time.Duration
	projectSlugMap map[string]string

	config *SlackConfig

	hostCallbacks plugin.HostCallbacks
}

// NewBroker creates a new SlackBroker with the given logger.
func NewBroker(log *slog.Logger) *SlackBroker {
	if log == nil {
		log = slog.Default()
	}
	return &SlackBroker{
		subs:          make(map[string]bool),
		sentIDs:       make(map[string]time.Time),
		log:           log,
		pluginName:    "slack",
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		agentCacheTTL: defaultAgentCacheTTL,
	}
}

// SetHostCallbacks implements plugin.HostCallbacksAware.
func (b *SlackBroker) SetHostCallbacks(hc plugin.HostCallbacks) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hostCallbacks = hc
}

// Configure sets up the Slack broker from the provided config map.
// Two-phase configuration:
//   - Phase 1 (bot_token present): Creates Slack client, inits SQLite store.
//   - Phase 2 (hub_url present): Sets hub credentials, creates HubClient.
func (b *SlackBroker) Configure(config map[string]string) error {
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

	botToken, hasBotToken := config["bot_token"]
	if hasBotToken && botToken != "" {
		cfg := &SlackConfig{
			BotToken:      botToken,
			AppToken:      config["app_token"],
			SigningSecret: config["signing_secret"],
			ListenAddress: config["listen_address"],
		}

		if cfg.ListenAddress == "" {
			cfg.ListenAddress = ":3000"
		}

		if v, ok := config["socket_mode"]; ok && (v == "true" || v == "1") {
			cfg.SocketMode = true
		}

		if !cfg.SocketMode && cfg.SigningSecret == "" {
			return fmt.Errorf("signing_secret is required when socket_mode is false")
		}

		cfg.DBPath = config["db_path"]
		if cfg.DBPath == "" {
			cfg.DBPath = defaultDBPath
		}
		b.config = cfg

		if b.client == nil {
			clientOpts := []slackapi.Option{}
			if cfg.AppToken != "" {
				clientOpts = append(clientOpts, slackapi.OptionAppLevelToken(cfg.AppToken))
			}
			b.client = slackapi.New(botToken, clientOpts...)
		}

		if b.socketClient == nil && cfg.SocketMode && cfg.AppToken != "" {
			b.socketClient = socketmode.New(b.client)
		}

		if b.store == nil {
			store, err := NewSQLiteStore(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("init sqlite store: %w", err)
			}
			b.store = store
		}

		if v, ok := config["agent_cache_ttl"]; ok && v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("invalid agent_cache_ttl: %w", err)
			}
			b.agentCacheTTL = d
		}

		b.log.Info("Slack broker phase 1 configured",
			"socket_mode", cfg.SocketMode,
			"db_path", cfg.DBPath,
		)
	}

	if b.hubURL != "" && b.client != nil {
		b.hubClient = NewHTTPHubClient(b.hubURL, b.hmacKey, b.brokerID)
		b.registration = NewRegistrationHandler(b.store, b.hubURL, b.hmacKey, b.brokerID, b.log)

		if slugMapJSON, ok := config["project_slug_map"]; ok && slugMapJSON != "" {
			var m map[string]string
			if err := json.Unmarshal([]byte(slugMapJSON), &m); err == nil {
				b.projectSlugMap = m
			}
		}

		b.events = &eventServer{
			client:         b.client,
			socketClient:   b.socketClient,
			signingSecret:  b.config.SigningSecret,
			log:            b.log,
			store:          b.store,
			hubClient:      b.hubClient,
			registration:   b.registration,
			deliverInbound: b.deliverInbound,
			onBotUserID: func(id string) {
				b.mu.Lock()
				b.botUserID = id
				b.mu.Unlock()
			},
		}

		b.log.Info("Slack broker phase 2 configured",
			"hub_url", b.hubURL,
			"broker_id", b.brokerID,
		)

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
				b.log.Info("Requested bootstrap subscription for Slack broker")
				return
			}
			b.log.Error("Bootstrap subscription timed out — host callbacks never became available")
		}()
	}

	return nil
}

// Subscribe records a subscription pattern and starts the event listener
// on the first subscribe call.
func (b *SlackBroker) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("slack broker is closed")
	}

	if b.subs[pattern] {
		return nil
	}

	wasEmpty := len(b.subs) == 0
	b.subs[pattern] = true

	if wasEmpty && b.events != nil {
		go b.startEventListener()
	}

	b.log.Debug("Subscription registered", "pattern", pattern)
	return nil
}

// Unsubscribe removes a subscription pattern.
func (b *SlackBroker) Unsubscribe(pattern string) error {
	b.mu.Lock()

	if !b.subs[pattern] {
		b.mu.Unlock()
		return nil
	}

	delete(b.subs, pattern)
	shouldStop := len(b.subs) == 0
	events := b.events

	b.mu.Unlock()

	if shouldStop && events != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := events.stop(ctx); err != nil {
			b.log.Warn("Failed to stop event listener", "error", err)
		}
	}

	b.log.Debug("Subscription removed", "pattern", pattern)
	return nil
}

// Publish sends a message to Slack channels using dynamic routing.
func (b *SlackBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return fmt.Errorf("slack broker is closed")
	}
	client := b.client
	store := b.store
	b.mu.RUnlock()

	if client == nil {
		return fmt.Errorf("slack broker not configured")
	}

	if msg == nil {
		return fmt.Errorf("message is nil")
	}

	if msg.Channel != "" && msg.Channel != "slack" {
		return nil
	}

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

	projectID, agentSlug := parseTopicComponents(topic)

	if msg.Type == messages.TypeAssistantReply {
		b.log.Debug("Filtering assistant-reply message")
		return nil
	}

	var channelIDs []string
	var threadTSs []string

	if msg.ThreadID != "" {
		parts := strings.SplitN(msg.ThreadID, ":", 2)
		if len(parts) == 2 {
			channelIDs = append(channelIDs, parts[0])
			threadTSs = append(threadTSs, parts[1])
		} else {
			channelIDs = append(channelIDs, msg.ThreadID)
			threadTSs = append(threadTSs, "")
		}
	}

	if len(channelIDs) == 0 && msg.Metadata != nil {
		if chID, ok := msg.Metadata["slack_channel_id"]; ok && chID != "" {
			channelIDs = append(channelIDs, chID)
			ts := ""
			if threadTS, ok := msg.Metadata["slack_thread_ts"]; ok {
				ts = threadTS
			}
			threadTSs = append(threadTSs, ts)
		}
	}

	if len(channelIDs) == 0 && msg.Recipient != "" && store != nil {
		ccSlug := agentSlug
		if ccSlug == "" && strings.HasPrefix(msg.Sender, "agent:") {
			ccSlug = strings.TrimPrefix(msg.Sender, "agent:")
		}
		channelIDs, threadTSs = b.resolveRecipientChannels(ctx, msg.Recipient, projectID, ccSlug)
	}

	if len(channelIDs) == 0 && projectID != "" && store != nil {
		links, err := store.GetChannelLinksForProject(ctx, projectID)
		if err != nil {
			b.log.Warn("Failed to get channel links for broadcast", "project_id", projectID, "error", err)
		}
		for _, link := range links {
			if link.Active {
				channelIDs = append(channelIDs, link.ChannelID)
				threadTSs = append(threadTSs, "")
			}
		}
	}

	if len(channelIDs) == 0 {
		b.log.Debug("No Slack channel for topic, dropping message", "topic", topic)
		return nil
	}

	senderSlug := agentSlug
	if senderSlug == "" && strings.HasPrefix(msg.Sender, "agent:") {
		senderSlug = strings.TrimPrefix(msg.Sender, "agent:")
	}

	isAgentToAgent := strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "agent:")
	isStateChange := msg.Type == messages.TypeStateChange

	var errs []error
	for i, channelID := range channelIDs {
		if store != nil && (isAgentToAgent || isStateChange) {
			link, linkErr := store.GetChannelLink(ctx, channelID)
			if linkErr == nil && link != nil {
				if isAgentToAgent && !link.ShowAgentToAgent {
					continue
				}
				if isStateChange && !link.ShowStateChanges {
					continue
				}
			}
		}

		threadTS := ""
		if i < len(threadTSs) {
			threadTS = threadTSs[i]
		}

		if msg.Type == messages.TypeInputNeeded {
			requestID := generateRequestID()
			blocks := RenderInputNeededBlocks(msg, senderSlug, requestID)

			opts := []slackapi.MsgOption{slackapi.MsgOptionBlocks(blocks...)}
			if threadTS != "" {
				opts = append(opts, slackapi.MsgOptionTS(threadTS))
			}

			_, msgTS, err := client.PostMessage(channelID, opts...)
			if err != nil {
				b.log.Error("Failed to send input-needed message", "channel_id", channelID, "error", err)
				errs = append(errs, err)
				continue
			}

			if store != nil {
				pending := &PendingAskUser{
					RequestID: requestID,
					MessageTS: msgTS,
					ChannelID: channelID,
					AgentSlug: senderSlug,
					ProjectID: projectID,
					ExpiresAt: time.Now().Add(15 * time.Minute),
				}
				if err := store.CreatePendingAskUser(ctx, pending); err != nil {
					b.log.Warn("Failed to save pending ask-user", "error", err)
				}
			}
			continue
		}

		var text string
		if isStateChange {
			text = FormatStateChange(msg, agentSlug)
		} else {
			text = FormatWebhookMessage(msg)
		}
		if text == "" {
			continue
		}

		opts := []slackapi.MsgOption{
			slackapi.MsgOptionText(text, false),
		}

		if senderSlug != "" && !isStateChange {
			opts = append(opts, slackapi.MsgOptionUsername(senderSlug))
		}

		if threadTS != "" {
			opts = append(opts, slackapi.MsgOptionTS(threadTS))
		}

		_, _, err := client.PostMessage(channelID, opts...)
		if err != nil {
			b.log.Error("Failed to send Slack message", "channel_id", channelID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close shuts down the Slack broker.
func (b *SlackBroker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.subs = make(map[string]bool)
	events := b.events
	store := b.store
	b.mu.Unlock()

	if b.registration != nil {
		b.registration.Stop()
	}

	if events != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := events.stop(ctx); err != nil {
			b.log.Warn("Failed to stop event listener", "error", err)
		}
	}

	if store != nil {
		store.Close()
	}

	b.log.Info("Slack broker closed")
	return nil
}

// GetInfo returns plugin metadata.
func (b *SlackBroker) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:      "slack",
		Version:   "1.0.0",
		ChannelID: "slack",
		Capabilities: []string{
			"echo-filter",
			"socket-mode",
			"http-webhooks",
			"slash-commands",
			"interactive-components",
			"channel-links",
			"user-registration",
		},
	}, nil
}

// HealthCheck returns the runtime health of the Slack broker.
func (b *SlackBroker) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return &plugin.HealthStatus{
			Status:  "unhealthy",
			Message: "broker is closed",
		}, nil
	}

	if b.client == nil {
		return &plugin.HealthStatus{
			Status:  "degraded",
			Message: "broker not configured",
		}, nil
	}

	details := map[string]string{
		"subscriptions": fmt.Sprintf("%d", len(b.subs)),
	}

	if b.botUserID != "" {
		details["bot_user_id"] = b.botUserID
	}
	if b.hubURL != "" {
		details["hub_url"] = b.hubURL
	}
	if b.config != nil {
		details["socket_mode"] = strconv.FormatBool(b.config.SocketMode)
	}

	return &plugin.HealthStatus{
		Status:  "healthy",
		Message: "slack bot operational",
		Details: details,
	}, nil
}

// --- Event listener ---

func (b *SlackBroker) startEventListener() {
	b.mu.RLock()
	events := b.events
	cfg := b.config
	b.mu.RUnlock()

	if events == nil || cfg == nil {
		return
	}

	if cfg.SocketMode {
		b.log.Info("Starting Slack Socket Mode listener")
		if err := events.startSocketMode(); err != nil {
			b.log.Error("Socket Mode listener failed", "error", err)
		}
	} else {
		b.log.Info("Starting Slack HTTP listener", "address", cfg.ListenAddress)
		if err := events.startHTTP(cfg.ListenAddress); err != nil && err != http.ErrServerClosed {
			b.log.Error("HTTP listener failed", "error", err)
		}
	}
}

// --- Hub delivery ---

func (b *SlackBroker) deliverInbound(topic string, msg *messages.StructuredMessage) *hubError {
	b.mu.RLock()
	hubURL := b.hubURL
	hmacKey := b.hmacKey
	brokerID := b.brokerID
	pluginName := b.pluginName
	b.mu.RUnlock()

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

func (b *SlackBroker) getProjectAgents(ctx context.Context, projectID string) []string {
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

// --- Routing helpers ---

func (b *SlackBroker) resolveRecipientChannels(ctx context.Context, recipient, projectID, agentSlug string) ([]string, []string) {
	email := strings.TrimPrefix(recipient, "user:")
	if email == recipient {
		return nil, nil
	}

	b.mu.RLock()
	store := b.store
	b.mu.RUnlock()

	if store == nil {
		return nil, nil
	}

	mapping, err := store.GetUserMappingByEmail(ctx, email)
	if err != nil || mapping == nil {
		return nil, nil
	}

	if agentSlug != "" {
		cc, err := store.GetConversationContext(ctx, mapping.SlackUserID, projectID, agentSlug)
		if err == nil && cc != nil {
			return []string{cc.LastChannelID}, []string{cc.LastThreadTS}
		}
	}

	cc, err := store.GetLatestConversationContext(ctx, mapping.SlackUserID, projectID)
	if err == nil && cc != nil {
		return []string{cc.LastChannelID}, []string{cc.LastThreadTS}
	}

	return nil, nil
}

// --- Topic parsing ---

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

// --- Dedup helpers ---

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

func (b *SlackBroker) pruneSentIDsLocked() {
	if len(b.sentIDs) < 1000 {
		return
	}
	now := time.Now()
	for k, t := range b.sentIDs {
		if now.Sub(t) > dedupTTL {
			delete(b.sentIDs, k)
		}
	}
}

// --- HMAC auth helpers ---

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

func generateRequestID() string {
	b := make([]byte, 12)
	crand.Read(b)
	return hex.EncodeToString(b)
}

func agentSlugs(agents []AgentInfo) []string {
	slugs := make([]string, len(agents))
	for i, a := range agents {
		slugs[i] = a.Slug
	}
	return slugs
}
