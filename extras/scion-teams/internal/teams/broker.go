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

package teams

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

const (
	// OriginMarkerKey is the metadata key injected into outbound messages
	// to identify messages originating from the hub.
	OriginMarkerKey = "scion_origin"

	// OriginMarkerValue is the marker value for hub-originated messages.
	OriginMarkerValue = "hub"
)

// Config holds the parsed configuration for the Teams broker.
type Config struct {
	// Phase 1: Azure credentials and local settings.
	AppID          string
	AppSecret      string
	TenantID       string
	ListenAddress  string
	DBPath         string
	MentionRouting bool

	// Phase 2: Hub connection.
	HubURL        string
	HMACKey       string
	BrokerID      string
	SendQueueSize int
	SendMinDelay  time.Duration
}

// TeamsBroker implements plugin.MessageBrokerPluginInterface for Microsoft Teams.
type TeamsBroker struct {
	log    *slog.Logger
	config *Config

	tokenProvider *TokenProvider
	jwtValidator  *JWTValidator
	webhookServer *WebhookServer
	hubClient     *HubClient

	// Outbound messaging (Phase 2).
	sender    *Sender
	sendQueue *SendQueue

	configured bool
	phase      int // 1 or 2

	mu sync.Mutex

	// In-memory conversation references (proper Store in Phase 3).
	conversationRefs map[string]*ConversationReference

	// In-memory channel links (proper Store in Phase 3).
	channelLinks map[string]*ChannelLink

	// In-memory conversation contexts (proper Store in Phase 3).
	// Key: teamsUserID + ":" + projectID + ":" + agentSlug.
	conversationContexts map[string]*ConversationContext

	// Subscription tracking.
	subscriptions map[string]bool
	serverRunning bool
}

// NewBroker creates a new TeamsBroker instance.
func NewBroker(log *slog.Logger) *TeamsBroker {
	return &TeamsBroker{
		log:                  log,
		conversationRefs:     make(map[string]*ConversationReference),
		channelLinks:         make(map[string]*ChannelLink),
		conversationContexts: make(map[string]*ConversationContext),
		subscriptions:        make(map[string]bool),
	}
}

// Configure implements MessageBrokerPluginInterface.Configure.
// It supports two-phase configuration:
//   - Phase 1: Azure credentials (app_id, app_secret, tenant_id) + local settings
//   - Phase 2: Hub connection (hub_url, hmac_key, broker_id)
func (b *TeamsBroker) Configure(config map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.config == nil {
		b.config = &Config{
			ListenAddress:  ":3978",
			DBPath:         "teams.db",
			MentionRouting: true,
		}
	}

	// Parse Phase 1 keys.
	if v, ok := config["app_id"]; ok {
		b.config.AppID = v
	}
	if v, ok := config["app_secret"]; ok {
		b.config.AppSecret = v
	}
	if v, ok := config["tenant_id"]; ok {
		b.config.TenantID = v
	}
	if v, ok := config["listen_address"]; ok {
		b.config.ListenAddress = v
	}
	if v, ok := config["db_path"]; ok {
		b.config.DBPath = v
	}
	if v, ok := config["mention_routing"]; ok {
		b.config.MentionRouting = v != "false"
	}

	// Phase 1 requires Azure credentials.
	// NOTE: Re-calling Configure with different Phase 1 credentials does not
	// recreate the TokenProvider or JWTValidator — a process restart is required.
	// This is acceptable for Phase 1 but should be improved for production.
	if b.config.AppID != "" && b.config.AppSecret != "" && b.config.TenantID != "" {
		if b.phase < 1 {
			b.tokenProvider = NewTokenProvider(b.config.AppID, b.config.AppSecret, b.config.TenantID)
			b.jwtValidator = NewJWTValidator(b.config.AppID)
			b.phase = 1
			b.log.Info("Phase 1 configuration complete",
				"app_id", b.config.AppID,
				"tenant_id", b.config.TenantID,
				"listen_address", b.config.ListenAddress,
			)
		}
	}

	// Parse Phase 2 keys.
	if v, ok := config["hub_url"]; ok {
		b.config.HubURL = v
	}
	if v, ok := config["hmac_key"]; ok {
		b.config.HMACKey = v
	}
	if v, ok := config["broker_id"]; ok {
		b.config.BrokerID = v
	}
	if v, ok := config["send_queue_size"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			b.config.SendQueueSize = n
		}
	}
	if v, ok := config["send_min_delay"]; ok {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			b.config.SendMinDelay = d
		}
	}

	// Phase 2 requires hub connection details AND phase 1 to be complete.
	if b.config.HubURL != "" && b.config.HMACKey != "" && b.config.BrokerID != "" {
		if b.phase == 1 {
			b.hubClient = NewHubClient(b.config.HubURL, b.config.HMACKey, b.config.BrokerID, b.log)

			// Initialize outbound messaging components.
			b.sender = NewSender(b.tokenProvider, b.log)
			b.sendQueue = NewSendQueue(b.sender, b.config.SendQueueSize, b.config.SendMinDelay, b.log)

			b.phase = 2
			b.configured = true
			b.log.Info("Phase 2 configuration complete",
				"hub_url", b.config.HubURL,
				"broker_id", b.config.BrokerID,
			)
		} else if b.phase < 1 {
			b.log.Warn("Hub connection keys provided but Azure credentials not yet configured; deferring phase 2")
		}
	}

	return nil
}

// Subscribe implements MessageBrokerPluginInterface.Subscribe.
// Starts the webhook HTTP server if not already running.
func (b *TeamsBroker) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.phase < 1 {
		return fmt.Errorf("broker not configured (phase 1 required)")
	}

	b.subscriptions[pattern] = true

	if b.serverRunning {
		b.log.Debug("Webhook server already running, added subscription", "pattern", pattern)
		return nil
	}

	// Create and start the webhook server.
	b.webhookServer = NewWebhookServer(b.config.ListenAddress, b.jwtValidator, b, b.log)

	go func() {
		if err := b.webhookServer.Start(); err != nil {
			b.log.Error("Webhook server error", "error", err)
		}
	}()

	b.serverRunning = true
	b.log.Info("Webhook server started", "pattern", pattern, "addr", b.config.ListenAddress)
	return nil
}

// Unsubscribe implements MessageBrokerPluginInterface.Unsubscribe.
// Stops the webhook server if no active subscriptions remain.
func (b *TeamsBroker) Unsubscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.subscriptions, pattern)

	if len(b.subscriptions) == 0 && b.serverRunning && b.webhookServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := b.webhookServer.Stop(ctx); err != nil {
			b.log.Warn("Error stopping webhook server", "error", err)
		}
		b.serverRunning = false
		b.log.Info("Webhook server stopped, no active subscriptions")
	}

	return nil
}

// Publish implements MessageBrokerPluginInterface.Publish.
// Routes outbound messages to Teams conversations using the priority chain:
//  1. ThreadID match — look up conversation reference for that thread.
//  2. Metadata teams_conversation_id — explicit target.
//  3. ConversationContext — last context for sender+project+agent.
//  4. ChannelLinks broadcast — all channels linked to the project.
func (b *TeamsBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	if msg == nil {
		return fmt.Errorf("message is nil")
	}

	// Echo prevention: skip messages with scion_origin marker.
	if msg.Metadata != nil {
		if origin, ok := msg.Metadata[OriginMarkerKey]; ok && origin == OriginMarkerValue {
			b.log.Debug("Skipping echo (scion_origin marker)",
				"topic", topic)
			return nil
		}
	}

	// Channel filtering: if the message targets a specific channel that
	// isn't ours, skip it.
	if msg.Channel != "" && msg.Channel != "teams" {
		return nil
	}

	b.mu.Lock()
	sendQueue := b.sendQueue
	b.mu.Unlock()

	if sendQueue == nil {
		b.log.Debug("Publish called but send queue not initialized", "topic", topic)
		return nil
	}

	// Mark outbound messages with origin to prevent echo loops.
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]string)
	}
	msg.Metadata[OriginMarkerKey] = OriginMarkerValue

	// Extract project and agent from topic.
	projectID, agentSlug := parsePublishTopic(topic)
	if msg.Metadata["project_id"] == "" && projectID != "" {
		msg.Metadata["project_id"] = projectID
	}

	// Format the message into a Teams Activity.
	activity, err := formatStructuredMessage(msg)
	if err != nil {
		return fmt.Errorf("format message: %w", err)
	}

	// Resolve target conversations via priority chain.
	type sendTarget struct {
		conversationID string
		serviceURL     string
		replyToID      string
	}
	var targets []sendTarget

	// Priority 1: ThreadID match.
	if msg.ThreadID != "" {
		b.mu.Lock()
		ref, ok := b.conversationRefs[msg.ThreadID]
		b.mu.Unlock()
		if ok {
			targets = append(targets, sendTarget{
				conversationID: ref.ConversationID,
				serviceURL:     ref.ServiceURL,
			})
		}
	}

	// Priority 2: Metadata teams_conversation_id.
	if len(targets) == 0 && msg.Metadata != nil {
		if convID, ok := msg.Metadata["teams_conversation_id"]; ok && convID != "" {
			serviceURL := msg.Metadata["teams_service_url"]
			if serviceURL == "" {
				b.mu.Lock()
				ref, ok := b.conversationRefs[convID]
				b.mu.Unlock()
				if ok {
					serviceURL = ref.ServiceURL
				}
			}
			if serviceURL != "" {
				targets = append(targets, sendTarget{
					conversationID: convID,
					serviceURL:     serviceURL,
					replyToID:      msg.Metadata["teams_reply_to_id"],
				})
			}
		}
	}

	// Priority 3: ConversationContext — last context for recipient+project+agent.
	if len(targets) == 0 && msg.Recipient != "" && projectID != "" {
		ccSlug := agentSlug
		if ccSlug == "" && strings.HasPrefix(msg.Sender, "agent:") {
			ccSlug = strings.TrimPrefix(msg.Sender, "agent:")
		}

		recipientID := msg.RecipientID
		if recipientID == "" {
			recipientID = msg.Recipient
		}

		ccKey := conversationContextKey(recipientID, projectID, ccSlug)
		b.mu.Lock()
		cc, ok := b.conversationContexts[ccKey]
		b.mu.Unlock()
		if ok && cc.LastConversationID != "" {
			serviceURL := ""
			b.mu.Lock()
			ref, refOK := b.conversationRefs[cc.LastConversationID]
			b.mu.Unlock()
			if refOK {
				serviceURL = ref.ServiceURL
			}
			if serviceURL != "" {
				targets = append(targets, sendTarget{
					conversationID: cc.LastConversationID,
					serviceURL:     serviceURL,
					replyToID:      cc.LastActivityID,
				})
			}
		}
	}

	// Priority 4: ChannelLinks broadcast.
	if len(targets) == 0 && projectID != "" {
		b.mu.Lock()
		for _, link := range b.channelLinks {
			if link.Active && (link.ProjectID == projectID || link.ProjectSlug == projectID) {
				serviceURL := ""
				ref, ok := b.conversationRefs[link.ConversationID]
				if ok {
					serviceURL = ref.ServiceURL
				}
				if serviceURL != "" {
					targets = append(targets, sendTarget{
						conversationID: link.ConversationID,
						serviceURL:     serviceURL,
					})
				}
			}
		}
		b.mu.Unlock()
	}

	if len(targets) == 0 {
		b.log.Debug("No Teams conversation for topic, dropping message", "topic", topic)
		return nil
	}

	// Send to each target via the SendQueue.
	var sendErrors []error
	for _, target := range targets {
		if target.replyToID != "" {
			activity.ReplyToID = target.replyToID
		}
		_, sendErr := sendQueue.Enqueue(ctx, target.conversationID, target.serviceURL, activity)
		if sendErr != nil {
			b.log.Error("Failed to send to conversation",
				"conversation_id", target.conversationID,
				"error", sendErr,
			)
			sendErrors = append(sendErrors, sendErr)
		}
	}

	if len(sendErrors) > 0 {
		return fmt.Errorf("failed to send to %d/%d targets", len(sendErrors), len(targets))
	}
	return nil
}

// parsePublishTopic extracts projectID and agentSlug from a topic string.
// Topics follow the pattern "project.agent.event" or similar dot-delimited formats.
func parsePublishTopic(topic string) (projectID, agentSlug string) {
	parts := strings.SplitN(topic, ".", 3)
	if len(parts) >= 1 {
		projectID = parts[0]
	}
	if len(parts) >= 2 {
		agentSlug = parts[1]
	}
	return
}

// conversationContextKey builds the map key for conversation context lookups.
func conversationContextKey(teamsUserID, projectID, agentSlug string) string {
	return teamsUserID + ":" + projectID + ":" + agentSlug
}

// SetConversationContext updates the in-memory conversation context.
func (b *TeamsBroker) SetConversationContext(cc *ConversationContext) {
	key := conversationContextKey(cc.TeamsUserID, cc.ProjectID, cc.AgentSlug)
	b.mu.Lock()
	b.conversationContexts[key] = cc
	b.mu.Unlock()
}

// AddChannelLink adds or updates a channel link in the in-memory map.
func (b *TeamsBroker) AddChannelLink(link *ChannelLink) {
	b.mu.Lock()
	b.channelLinks[link.ConversationID] = link
	b.mu.Unlock()
}

// Close implements MessageBrokerPluginInterface.Close.
func (b *TeamsBroker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.sendQueue != nil {
		b.sendQueue.Close()
	}

	if b.webhookServer != nil && b.serverRunning {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := b.webhookServer.Stop(ctx); err != nil {
			b.log.Warn("Error stopping webhook server on close", "error", err)
		}
		b.serverRunning = false
	}

	b.subscriptions = make(map[string]bool)
	b.log.Info("Teams broker closed")
	return nil
}

// GetInfo implements MessageBrokerPluginInterface.GetInfo.
func (b *TeamsBroker) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:            "teams",
		Version:         "0.1.0",
		MinScionVersion: "0.1.0",
		ChannelID:       "teams",
		Capabilities:    []string{"inbound", "outbound"},
	}, nil
}

// HealthCheck implements MessageBrokerPluginInterface.HealthCheck.
func (b *TeamsBroker) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	details := map[string]string{
		"configured": fmt.Sprintf("%v", b.configured),
		"phase":      fmt.Sprintf("%d", b.phase),
	}

	if b.serverRunning {
		details["webhook_server"] = "running"
		return &plugin.HealthStatus{
			Status:  "healthy",
			Message: "webhook server running",
			Details: details,
		}, nil
	}

	if b.configured {
		details["webhook_server"] = "stopped"
		return &plugin.HealthStatus{
			Status:  "degraded",
			Message: "configured but webhook server not running",
			Details: details,
		}, nil
	}

	return &plugin.HealthStatus{
		Status:  "unhealthy",
		Message: "not fully configured",
		Details: details,
	}, nil
}

// HandleActivity implements the ActivityHandler interface.
// Dispatches activities by type.
func (b *TeamsBroker) HandleActivity(ctx context.Context, activity *Activity) (*InvokeResponse, error) {
	// Upsert conversation reference for every inbound activity.
	b.upsertConversationRef(activity)

	switch activity.Type {
	case "message":
		return nil, b.handleMessage(ctx, activity)
	case "conversationUpdate":
		handleConversationUpdate(activity, b.log)
		return nil, nil
	case "invoke":
		b.log.Debug("Invoke activity received (stub in Phase 1)",
			"name", activity.Name,
			"conversation_id", activity.Conversation.ID,
		)
		return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}, nil
	case "messageReaction":
		b.log.Debug("Message reaction received (ignored in Phase 1)",
			"activity_id", activity.ID,
		)
		return nil, nil
	default:
		b.log.Debug("Unknown activity type, acknowledging",
			"type", activity.Type,
			"activity_id", activity.ID,
		)
		return nil, nil
	}
}

// handleMessage processes an incoming message Activity:
// converts it to a StructuredMessage and delivers it to the hub.
func (b *TeamsBroker) handleMessage(ctx context.Context, activity *Activity) error {
	// Skip messages from the bot itself (secondary echo guard).
	if b.config != nil && activity.From.ID == b.config.AppID {
		b.log.Debug("Skipping message from self", "activity_id", activity.ID)
		return nil
	}

	botID := ""
	if b.config != nil {
		botID = b.config.AppID
	}

	msg := activityToStructuredMessage(activity, botID)

	// Echo prevention: check for scion_origin marker in the activity's
	// channel data or value. Messages sent by this bot via Publish() carry
	// this marker; if the platform echoes them back, we skip them.
	if msg.Metadata != nil {
		if origin, ok := msg.Metadata[OriginMarkerKey]; ok && origin == OriginMarkerValue {
			b.log.Debug("Skipping echo (scion_origin marker on inbound)", "activity_id", activity.ID)
			return nil
		}
	}

	// Apply entity-based mention stripping for more precision.
	if len(activity.Entities) > 0 && botID != "" {
		msg.Msg = stripBotMentionByEntity(activity.Text, botID, activity.Entities)
		if msg.Msg == "" {
			msg.Msg = strings.TrimSpace(activity.Text)
		}
	}

	// Tier 1 mention routing: check if message starts with an @-mention
	// of the bot followed by an agent name.
	if b.config != nil && b.config.MentionRouting {
		if recipient := extractMentionTarget(msg.Msg); recipient != "" {
			msg.Recipient = recipient
			// Strip the agent name from the message text.
			msg.Msg = strings.TrimSpace(strings.TrimPrefix(msg.Msg, recipient))
		}
	}

	b.log.Info("Processing inbound message",
		"from", msg.Sender,
		"sender_id", msg.SenderID,
		"text_length", len(msg.Msg),
		"conversation_id", activity.Conversation.ID,
	)

	// Update conversation context for routing replies back.
	if msg.SenderID != "" && msg.Recipient != "" {
		agentSlug := msg.Recipient
		if strings.HasPrefix(agentSlug, "agent:") {
			agentSlug = strings.TrimPrefix(agentSlug, "agent:")
		}
		projectID := msg.Channel
		if projectID == "" && msg.Metadata != nil {
			projectID = msg.Metadata["project_id"]
		}
		if projectID != "" {
			b.SetConversationContext(&ConversationContext{
				TeamsUserID:        msg.SenderID,
				ProjectID:          projectID,
				AgentSlug:          agentSlug,
				LastConversationID: activity.Conversation.ID,
				LastActivityID:     activity.ID,
				LastMessageAt:      time.Now(),
			})
		}
	}

	if b.hubClient == nil {
		b.log.Warn("Hub client not configured, message not delivered")
		return nil
	}

	topic := "teams.message"
	if err := b.hubClient.DeliverInbound(ctx, topic, msg); err != nil {
		b.log.Error("Failed to deliver message to hub",
			"error", err,
			"conversation_id", activity.Conversation.ID,
		)
		return fmt.Errorf("deliver to hub: %w", err)
	}

	b.log.Debug("Message delivered to hub",
		"topic", topic,
		"sender", msg.Sender,
	)
	return nil
}

// extractMentionTarget extracts an agent name from the beginning of
// a message, if present. Returns empty string if no target found.
// Expected format: "agent-name rest of message" or "@agent-name rest of message"
func extractMentionTarget(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	// Look for a leading word that looks like an agent slug.
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return ""
	}

	candidate := parts[0]
	// Strip leading @ if present.
	candidate = strings.TrimPrefix(candidate, "@")

	// Agent slugs are typically lowercase with hyphens.
	if isValidAgentSlug(candidate) {
		return candidate
	}

	return ""
}

// isValidAgentSlug checks if a string looks like a valid agent slug.
func isValidAgentSlug(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// upsertConversationRef updates the in-memory conversation reference map.
func (b *TeamsBroker) upsertConversationRef(activity *Activity) {
	ref := &ConversationReference{
		ServiceURL:     activity.ServiceURL,
		ConversationID: activity.Conversation.ID,
		ChannelID:      activity.ChannelID,
		BotID:          activity.Recipient.ID,
	}

	if activity.Conversation.TenantID != "" {
		ref.TenantID = activity.Conversation.TenantID
	} else if activity.ChannelData != nil && activity.ChannelData.Tenant != nil {
		ref.TenantID = activity.ChannelData.Tenant.ID
	}

	b.mu.Lock()
	b.conversationRefs[activity.Conversation.ID] = ref
	b.mu.Unlock()
}
