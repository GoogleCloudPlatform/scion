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

package chatapp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// eventUserLookup returns user info from the ChatEvent itself, using the
// Google-asserted email from the signed event payload. This avoids the need
// for a separate API call to look up the user's email.
type eventUserLookup struct {
	event *ChatEvent
}

func (el *eventUserLookup) GetUser(ctx context.Context, userID string) (*identity.ChatUserInfo, error) {
	return &identity.ChatUserInfo{
		PlatformID: el.event.UserID,
		Email:      el.event.UserEmail,
	}, nil
}

// pendingDeviceAuth tracks an in-progress device authorization flow.
type pendingDeviceAuth struct {
	deviceCode string
	userCode   string
	verifyURL  string
	expiresAt  time.Time
	interval   time.Duration
}

// CommandRouter parses and executes chat commands.
type CommandRouter struct {
	adminClient hubclient.Client
	hubURL      string
	store       *state.Store
	idMapper    *identity.Mapper
	messenger   Messenger
	broker      *BrokerServer
	log         *slog.Logger

	mu             sync.Mutex
	pendingAuth    map[string]*pendingDeviceAuth // keyed by platformUserID+platform
	pendingDeletes map[string]string             // keyed by actionID -> agentID
}

// NewCommandRouter creates a new command router.
func NewCommandRouter(
	adminClient hubclient.Client,
	hubURL string,
	store *state.Store,
	idMapper *identity.Mapper,
	messenger Messenger,
	broker *BrokerServer,
	log *slog.Logger,
) *CommandRouter {
	return &CommandRouter{
		adminClient:    adminClient,
		hubURL:         hubURL,
		store:          store,
		idMapper:       idMapper,
		messenger:      messenger,
		broker:         broker,
		log:            log,
		pendingAuth:    make(map[string]*pendingDeviceAuth),
		pendingDeletes: make(map[string]string),
	}
}

// SetMessenger sets the messenger after construction, breaking the
// circular dependency between the command router and chat adapter.
func (r *CommandRouter) SetMessenger(m Messenger) {
	r.messenger = m
}

// HandleEvent processes a ChatEvent and routes it to the appropriate handler.
// Returns an optional EventResponse for synchronous HTTP responses.
func (r *CommandRouter) HandleEvent(ctx context.Context, event *ChatEvent) (*EventResponse, error) {
	switch event.Type {
	case EventCommand:
		return nil, r.handleCommand(ctx, event)
	case EventMessage:
		return nil, r.handleMessage(ctx, event)
	case EventAction:
		return nil, r.handleAction(ctx, event)
	case EventDialogSubmit:
		return nil, r.handleDialogSubmit(ctx, event)
	case EventSpaceJoin:
		return nil, r.handleSpaceJoin(ctx, event)
	case EventSpaceRemove:
		return nil, r.handleSpaceRemove(ctx, event)
	default:
		r.log.Debug("unhandled event type", "type", event.Type)
		return nil, nil
	}
}

// handleCommand parses "/scion <subcommand> <args>" and routes.
func (r *CommandRouter) handleCommand(ctx context.Context, event *ChatEvent) error {
	parts := strings.Fields(event.Args)
	if len(parts) == 0 {
		return r.cmdHelp(ctx, event)
	}

	subcommand := strings.ToLower(parts[0])
	args := parts[1:]

	switch subcommand {
	case "list":
		return r.cmdList(ctx, event, args)
	case "status":
		return r.cmdStatus(ctx, event, args)
	case "start":
		return r.cmdStart(ctx, event, args)
	case "stop":
		return r.cmdStop(ctx, event, args)
	case "create":
		return r.cmdCreate(ctx, event, args)
	case "delete":
		return r.cmdDelete(ctx, event, args)
	case "logs":
		return r.cmdLogs(ctx, event, args)
	case "link":
		return r.cmdLink(ctx, event, args)
	case "unlink":
		return r.cmdUnlink(ctx, event, args)
	case "register":
		return r.cmdRegister(ctx, event, args)
	case "unregister":
		return r.cmdUnregister(ctx, event, args)
	case "subscribe":
		return r.cmdSubscribe(ctx, event, args)
	case "unsubscribe":
		return r.cmdUnsubscribe(ctx, event, args)
	case "message", "msg":
		return r.cmdMessage(ctx, event, args)
	case "info":
		return r.cmdInfo(ctx, event, args)
	case "help":
		return r.cmdHelp(ctx, event)
	default:
		return r.reply(ctx, event, fmt.Sprintf("Unknown command: `%s`. Use `/scion help` for available commands.", subcommand))
	}
}

// handleMessage routes @mention messages to an agent.
func (r *CommandRouter) handleMessage(ctx context.Context, event *ChatEvent) error {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return r.reply(ctx, event, "This space is not linked to a grove. Use `/scion link <grove-slug>` to link it.")
	}

	// Try to resolve the user
	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil {
		return fmt.Errorf("resolving user: %w", err)
	}
	if mapping == nil {
		return r.reply(ctx, event, "You are not registered. Use `/scion register` to link your chat account to your Hub account.")
	}

	// For MVP: send to the first running agent mentioned in the text,
	// or prompt for target if ambiguous
	return r.reply(ctx, event, "Message received. Use `/scion message <agent> <text>` to send to a specific agent.")
}

// handleAction processes button clicks and interactive elements.
func (r *CommandRouter) handleAction(ctx context.Context, event *ChatEvent) error {
	parts := strings.Split(event.ActionID, ".")
	if len(parts) < 2 {
		return nil
	}

	actionType := parts[0]
	actionVerb := parts[1]
	var targetID string
	if len(parts) > 2 {
		targetID = strings.Join(parts[2:], ".")
	}

	switch actionType {
	case "agent":
		return r.handleAgentAction(ctx, event, actionVerb, targetID)
	case "notification":
		if actionVerb == "ack" && targetID != "" {
			client, err := r.clientForUser(ctx, event)
			if err != nil {
				return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
			}
			return client.Notifications().Acknowledge(ctx, targetID)
		}
	}
	return nil
}

// handleDialogSubmit processes form submissions from interactive cards.
func (r *CommandRouter) handleDialogSubmit(ctx context.Context, event *ChatEvent) error {
	// Handle agent.respond submissions (ask_user inline response)
	if strings.HasPrefix(event.ActionID, "agent.respond.") {
		agentID := strings.TrimPrefix(event.ActionID, "agent.respond.")
		responseText := ""
		// The response field name matches the actionID used in the input widget
		if v, ok := event.DialogData[event.ActionID]; ok {
			responseText = v
		}
		// Also try just the agentID as field name
		if responseText == "" {
			if v, ok := event.DialogData["response"]; ok {
				responseText = v
			}
		}
		if responseText == "" {
			return r.reply(ctx, event, "No response text provided.")
		}

		client, err := r.clientForUser(ctx, event)
		if err != nil {
			return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
		}

		if err := client.Agents().SendMessage(ctx, agentID, responseText, false); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to send response to agent: %v", err))
		}
		return r.reply(ctx, event, fmt.Sprintf("Response sent to agent `%s`.", agentID))
	}

	// Handle delete confirmation
	if strings.HasPrefix(event.ActionID, "agent.delete.confirm.") {
		agentID := strings.TrimPrefix(event.ActionID, "agent.delete.confirm.")
		return r.executeDelete(ctx, event, agentID)
	}

	// Handle subscription activity filter dialog
	if strings.HasPrefix(event.ActionID, "subscribe.filter.") {
		return r.handleSubscribeFilter(ctx, event)
	}

	return nil
}

// handleAgentAction processes agent-specific button actions.
func (r *CommandRouter) handleAgentAction(ctx context.Context, event *ChatEvent, verb, agentID string) error {
	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	switch verb {
	case "start":
		if err := client.Agents().Start(ctx, agentID); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to start agent: %v", err))
		}
		return r.reply(ctx, event, fmt.Sprintf("Agent `%s` started.", agentID))
	case "stop":
		if err := client.Agents().Stop(ctx, agentID); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to stop agent: %v", err))
		}
		return r.reply(ctx, event, fmt.Sprintf("Agent `%s` stopped.", agentID))
	case "logs":
		logs, err := client.Agents().GetLogs(ctx, agentID, &hubclient.GetLogsOptions{Tail: 50})
		if err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to get logs: %v", err))
		}
		if len(logs) > 2000 {
			logs = logs[len(logs)-2000:]
		}
		return r.reply(ctx, event, fmt.Sprintf("*Logs for `%s`:*\n```\n%s\n```", agentID, logs))
	case "respond":
		// This is handled via dialog submit when user fills the inline input field.
		// If triggered as a plain action (no dialog data), prompt for input.
		return r.reply(ctx, event, fmt.Sprintf("Use the inline response field in the notification card to respond to agent `%s`.", agentID))
	case "delete":
		return r.showDeleteConfirmation(ctx, event, agentID)
	}
	return nil
}

// handleSpaceJoin is called when the bot is added to a space.
// When added via @mention (InteractionAdd=true), a subsequent messagePayload
// or appCommandPayload will follow, so we suppress the welcome message to
// avoid duplicate responses.
func (r *CommandRouter) handleSpaceJoin(ctx context.Context, event *ChatEvent) error {
	if event.InteractionAdd {
		r.log.Debug("space join via @mention, deferring to subsequent event")
		return nil
	}
	return r.reply(ctx, event, "Hello! I'm Scion Bot. Use `/scion link <grove-slug>` to connect this space to a grove, then `/scion help` for available commands.")
}

// handleSpaceRemove is called when the bot is removed from a space.
func (r *CommandRouter) handleSpaceRemove(ctx context.Context, event *ChatEvent) error {
	// Clean up space link
	if err := r.store.DeleteSpaceLink(event.SpaceID, event.Platform); err != nil {
		r.log.Error("cleaning up space link on removal", "error", err)
	}
	return nil
}

// --- Command implementations ---

func (r *CommandRouter) cmdList(ctx context.Context, event *ChatEvent, args []string) error {
	link, err := r.requireSpaceLink(ctx, event)
	if err != nil || link == nil {
		return err
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	agents, err := client.GroveAgents(link.GroveID).List(ctx, nil)
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to list agents: %v", err))
	}

	if len(agents.Agents) == 0 {
		return r.reply(ctx, event, fmt.Sprintf("No agents in grove `%s`.", link.GroveSlug))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Agents in %s:*\n", link.GroveSlug))
	for _, a := range agents.Agents {
		status := a.Activity
		if status == "" {
			status = a.Phase
		}
		sb.WriteString(fmt.Sprintf("• `%s` — %s\n", a.Slug, status))
	}
	return r.reply(ctx, event, sb.String())
}

func (r *CommandRouter) cmdStatus(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion status <agent-slug>`")
	}

	link, err := r.requireSpaceLink(ctx, event)
	if err != nil || link == nil {
		return err
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	agent, err := client.GroveAgents(link.GroveID).Get(ctx, args[0])
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to get agent: %v", err))
	}

	card := Card{
		Header: CardHeader{
			Title:    agent.Name,
			Subtitle: fmt.Sprintf("Grove: %s | %s", link.GroveSlug, agent.Activity),
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetKeyValue, Label: "Slug", Content: agent.Slug},
					{Type: WidgetKeyValue, Label: "Phase", Content: agent.Phase},
					{Type: WidgetKeyValue, Label: "Activity", Content: agent.Activity},
					{Type: WidgetKeyValue, Label: "Template", Content: agent.Template},
				},
			},
		},
		Actions: []CardAction{
			{Label: "Start", ActionID: fmt.Sprintf("agent.start.%s", agent.ID), Style: "primary"},
			{Label: "Stop", ActionID: fmt.Sprintf("agent.stop.%s", agent.ID), Style: "danger"},
			{Label: "View Logs", ActionID: fmt.Sprintf("agent.logs.%s", agent.ID)},
		},
	}

	_, err = r.messenger.SendCard(ctx, event.SpaceID, card)
	return err
}

func (r *CommandRouter) cmdStart(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion start <agent-slug>`")
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	if err := client.Agents().Start(ctx, args[0]); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to start agent: %v", err))
	}
	return r.reply(ctx, event, fmt.Sprintf("Agent `%s` started.", args[0]))
}

func (r *CommandRouter) cmdStop(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion stop <agent-slug>`")
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	if err := client.Agents().Stop(ctx, args[0]); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to stop agent: %v", err))
	}
	return r.reply(ctx, event, fmt.Sprintf("Agent `%s` stopped.", args[0]))
}

func (r *CommandRouter) cmdCreate(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion create <agent-name>`")
	}

	link, err := r.requireSpaceLink(ctx, event)
	if err != nil || link == nil {
		return err
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	resp, err := client.GroveAgents(link.GroveID).Create(ctx, &hubclient.CreateAgentRequest{
		Name: args[0],
	})
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to create agent: %v", err))
	}
	return r.reply(ctx, event, fmt.Sprintf("Agent `%s` created (ID: `%s`).", resp.Agent.Slug, resp.Agent.ID))
}

func (r *CommandRouter) cmdLink(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion link <grove-slug>`")
	}

	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil || mapping == nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	client, err := r.idMapper.ClientFor(ctx, mapping)
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to create client: %v", err))
	}

	// Look up the grove
	grove, err := client.Groves().Get(ctx, args[0])
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Grove `%s` not found: %v", args[0], err))
	}

	// Save the link
	link := &state.SpaceLink{
		SpaceID:   event.SpaceID,
		Platform:  event.Platform,
		GroveID:   grove.ID,
		GroveSlug: grove.Slug,
		LinkedBy:  mapping.HubUserID,
	}
	if err := r.store.SetSpaceLink(link); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to save link: %v", err))
	}

	// Request subscription for the grove's messages via broker plugin
	if r.broker != nil {
		pattern := fmt.Sprintf("grove.%s.>", grove.ID)
		if err := r.broker.RequestSubscription(pattern); err != nil {
			r.log.Warn("failed to request grove subscription", "grove_id", grove.ID, "error", err)
		}
	}

	return r.reply(ctx, event, fmt.Sprintf("This space is now linked to grove `%s`.", grove.Slug))
}

func (r *CommandRouter) cmdUnlink(ctx context.Context, event *ChatEvent, args []string) error {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return r.reply(ctx, event, "This space is not linked to any grove.")
	}

	// Cancel broker subscription
	if r.broker != nil {
		pattern := fmt.Sprintf("grove.%s.>", link.GroveID)
		if err := r.broker.CancelSubscription(pattern); err != nil {
			r.log.Warn("failed to cancel grove subscription", "grove_id", link.GroveID, "error", err)
		}
	}

	if err := r.store.DeleteSpaceLink(event.SpaceID, event.Platform); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to unlink: %v", err))
	}
	return r.reply(ctx, event, fmt.Sprintf("Unlinked from grove `%s`.", link.GroveSlug))
}

func (r *CommandRouter) cmdRegister(ctx context.Context, event *ChatEvent, args []string) error {
	// Check if already registered
	existing, err := r.idMapper.Resolve(event.UserID, event.Platform)
	if err != nil {
		return fmt.Errorf("checking registration: %w", err)
	}
	if existing != nil {
		return r.reply(ctx, event, fmt.Sprintf("You are already registered as `%s`.", existing.HubUserEmail))
	}

	// Try auto-registration by email (short-circuit)
	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil {
		return fmt.Errorf("auto-registration: %w", err)
	}
	if mapping != nil {
		return r.reply(ctx, event, fmt.Sprintf("Registered! Your chat account is linked to Hub user `%s`.", mapping.HubUserEmail))
	}

	// No email match — the user's chat email doesn't match any Hub user.
	// Fall back to device authorization flow so they can authenticate
	// with the Hub account they want to link.
	// Check if there's a pending auth and the user is confirming
	authKey := event.UserID + ":" + event.Platform
	r.mu.Lock()
	pending := r.pendingAuth[authKey]
	r.mu.Unlock()

	if pending != nil && len(args) > 0 && args[0] == "confirm" {
		return r.pollDeviceAuth(ctx, event, pending)
	}

	// Initiate device auth flow
	resp, err := r.adminClient.Auth().RequestDeviceCode(ctx, "")
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to start device authorization: %v", err))
	}

	pa := &pendingDeviceAuth{
		deviceCode: resp.DeviceCode,
		userCode:   resp.UserCode,
		verifyURL:  resp.VerificationURL,
		expiresAt:  time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second),
		interval:   time.Duration(resp.Interval) * time.Second,
	}
	r.mu.Lock()
	r.pendingAuth[authKey] = pa
	r.mu.Unlock()

	verifyURL := resp.VerificationURL
	if resp.VerificationURLComplete != "" {
		verifyURL = resp.VerificationURLComplete
	}

	card := Card{
		Header: CardHeader{
			Title:    "Device Authorization",
			Subtitle: "No matching Hub account found for your chat email",
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetText, Content: fmt.Sprintf("Your chat email doesn't match any Hub user. Sign in with your Hub account to link it:\n\n*URL:* %s\n*Code:* `%s`", verifyURL, resp.UserCode)},
				},
			},
			{
				Header: "After completing authorization:",
				Widgets: []Widget{
					{Type: WidgetText, Content: "Run `/scion register confirm` to finish registration."},
				},
			},
		},
	}

	_, err = r.messenger.SendCard(ctx, event.SpaceID, card)
	return err
}

// pollDeviceAuth polls for device authorization completion and registers the user.
func (r *CommandRouter) pollDeviceAuth(ctx context.Context, event *ChatEvent, pending *pendingDeviceAuth) error {
	authKey := event.UserID + ":" + event.Platform

	if time.Now().After(pending.expiresAt) {
		r.mu.Lock()
		delete(r.pendingAuth, authKey)
		r.mu.Unlock()
		return r.reply(ctx, event, "Device authorization expired. Run `/scion register` to start again.")
	}

	resp, err := r.adminClient.Auth().PollDeviceToken(ctx, pending.deviceCode, "")
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to check authorization status: %v", err))
	}

	switch resp.Status {
	case "authorization_pending":
		return r.reply(ctx, event, "Authorization still pending. Complete the flow in your browser, then run `/scion register confirm` again.")
	case "expired_token":
		r.mu.Lock()
		delete(r.pendingAuth, authKey)
		r.mu.Unlock()
		return r.reply(ctx, event, "Device authorization expired. Run `/scion register` to start again.")
	case "slow_down":
		return r.reply(ctx, event, "Please wait a moment before trying again.")
	case "":
		// Success — token received
		if resp.User == nil {
			return r.reply(ctx, event, "Authorization succeeded but no user info returned. Please try again.")
		}

		// Register the mapping
		if err := r.idMapper.Register(event.UserID, event.Platform, resp.User.ID, resp.User.Email); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Authorization succeeded but failed to save registration: %v", err))
		}

		r.mu.Lock()
		delete(r.pendingAuth, authKey)
		r.mu.Unlock()

		return r.reply(ctx, event, fmt.Sprintf("Registered! Your chat account is linked to Hub user `%s`.", resp.User.Email))
	default:
		return r.reply(ctx, event, fmt.Sprintf("Unexpected authorization status: %s", resp.Status))
	}
}

func (r *CommandRouter) cmdUnregister(ctx context.Context, event *ChatEvent, args []string) error {
	if err := r.idMapper.Unregister(event.UserID, event.Platform); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to unregister: %v", err))
	}
	return r.reply(ctx, event, "Your chat account has been unlinked from your Hub account.")
}

func (r *CommandRouter) cmdDelete(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion delete <agent-slug>`")
	}
	return r.showDeleteConfirmation(ctx, event, args[0])
}

// showDeleteConfirmation presents a confirmation card before deleting an agent.
func (r *CommandRouter) showDeleteConfirmation(ctx context.Context, event *ChatEvent, agentSlug string) error {
	link, err := r.requireSpaceLink(ctx, event)
	if err != nil || link == nil {
		return err
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	agent, err := client.GroveAgents(link.GroveID).Get(ctx, agentSlug)
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Agent `%s` not found: %v", agentSlug, err))
	}

	confirmID := fmt.Sprintf("agent.delete.confirm.%s", agent.ID)

	card := Card{
		Header: CardHeader{
			Title:    "Confirm Delete",
			Subtitle: fmt.Sprintf("Agent: %s", agent.Slug),
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetText, Content: fmt.Sprintf("Are you sure you want to delete agent `%s`?\n\nThis action cannot be undone.", agent.Slug)},
					{Type: WidgetKeyValue, Label: "Name", Content: agent.Name},
					{Type: WidgetKeyValue, Label: "Phase", Content: agent.Phase},
					{Type: WidgetKeyValue, Label: "Activity", Content: agent.Activity},
				},
			},
		},
		Actions: []CardAction{
			{Label: "Delete", ActionID: confirmID, Style: "danger"},
			{Label: "Cancel", ActionID: "noop"},
		},
	}

	_, err = r.messenger.SendCard(ctx, event.SpaceID, card)
	return err
}

// executeDelete performs the actual agent deletion after confirmation.
func (r *CommandRouter) executeDelete(ctx context.Context, event *ChatEvent, agentID string) error {
	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	if err := client.Agents().Delete(ctx, agentID, nil); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to delete agent: %v", err))
	}
	return r.reply(ctx, event, fmt.Sprintf("Agent `%s` deleted.", agentID))
}

func (r *CommandRouter) cmdLogs(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion logs <agent-slug>`")
	}

	link, err := r.requireSpaceLink(ctx, event)
	if err != nil || link == nil {
		return err
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	opts := &hubclient.GetLogsOptions{Tail: 50}
	logs, err := client.GroveAgents(link.GroveID).GetLogs(ctx, args[0], opts)
	if err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to get logs for `%s`: %v", args[0], err))
	}

	if logs == "" {
		return r.reply(ctx, event, fmt.Sprintf("No logs available for agent `%s`.", args[0]))
	}

	// Truncate for chat display
	if len(logs) > 2000 {
		logs = "...\n" + logs[len(logs)-2000:]
	}
	return r.reply(ctx, event, fmt.Sprintf("*Logs for `%s`:*\n```\n%s\n```", args[0], logs))
}

func (r *CommandRouter) cmdSubscribe(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion subscribe <agent-slug>`")
	}

	link, err := r.requireSpaceLink(ctx, event)
	if err != nil || link == nil {
		return err
	}

	agentSlug := args[0]

	// If additional args are provided, use them as activity filters directly
	if len(args) > 1 {
		activities := strings.Join(args[1:], ",")
		sub := &state.AgentSubscription{
			PlatformUserID: event.UserID,
			Platform:       event.Platform,
			AgentID:        agentSlug,
			GroveID:        link.GroveID,
			Activities:     activities,
		}
		if err := r.store.SetAgentSubscription(sub); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to subscribe: %v", err))
		}
		return r.reply(ctx, event, fmt.Sprintf("Subscribed to notifications for agent `%s`. Filtered to: %s", agentSlug, activities))
	}

	// Show activity filter dialog with checkboxes
	filterID := fmt.Sprintf("subscribe.filter.%s.%s", link.GroveID, agentSlug)
	card := Card{
		Header: CardHeader{
			Title:    "Subscribe to Agent Notifications",
			Subtitle: fmt.Sprintf("Agent: %s", agentSlug),
		},
		Sections: []CardSection{
			{
				Header: "Select activity types to be @mentioned for:",
				Widgets: []Widget{
					{
						Type:     WidgetCheckbox,
						Label:    "Activities",
						ActionID: filterID,
						Options: []SelectOption{
							{Label: "Completed", Value: "COMPLETED"},
							{Label: "Waiting for Input", Value: "WAITING_FOR_INPUT"},
							{Label: "Error", Value: "ERROR"},
							{Label: "Stalled", Value: "STALLED"},
							{Label: "Limits Exceeded", Value: "LIMITS_EXCEEDED"},
						},
					},
				},
			},
			{
				Widgets: []Widget{
					{Type: WidgetText, Content: "_Leave all unchecked to subscribe to all activity types._"},
				},
			},
		},
		Actions: []CardAction{
			{Label: "Subscribe", ActionID: filterID, Style: "primary"},
		},
	}

	_, err = r.messenger.SendCard(ctx, event.SpaceID, card)
	return err
}

// handleSubscribeFilter processes the subscription activity filter dialog submission.
func (r *CommandRouter) handleSubscribeFilter(ctx context.Context, event *ChatEvent) error {
	// ActionID format: subscribe.filter.<groveID>.<agentSlug>
	parts := strings.SplitN(event.ActionID, ".", 4)
	if len(parts) < 4 {
		return r.reply(ctx, event, "Invalid subscription filter action.")
	}
	groveID := parts[2]
	agentSlug := parts[3]

	// Collect selected activities from dialog data
	var activities string
	if selected, ok := event.DialogData[event.ActionID]; ok && selected != "" {
		activities = selected
	}

	sub := &state.AgentSubscription{
		PlatformUserID: event.UserID,
		Platform:       event.Platform,
		AgentID:        agentSlug,
		GroveID:        groveID,
		Activities:     activities,
	}
	if err := r.store.SetAgentSubscription(sub); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to subscribe: %v", err))
	}

	msg := fmt.Sprintf("Subscribed to notifications for agent `%s`.", agentSlug)
	if activities != "" {
		msg += fmt.Sprintf(" Filtered to: %s", activities)
	} else {
		msg += " Receiving all activity types."
	}
	return r.reply(ctx, event, msg)
}

func (r *CommandRouter) cmdUnsubscribe(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) == 0 {
		return r.reply(ctx, event, "Usage: `/scion unsubscribe <agent-slug>`")
	}

	if err := r.store.DeleteAgentSubscription(event.UserID, event.Platform, args[0]); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to unsubscribe: %v", err))
	}
	return r.reply(ctx, event, fmt.Sprintf("Unsubscribed from notifications for agent `%s`.", args[0]))
}

func (r *CommandRouter) cmdMessage(ctx context.Context, event *ChatEvent, args []string) error {
	if len(args) < 2 {
		return r.reply(ctx, event, "Usage: `/scion message [--thread <thread-id>] <agent-slug> <text>`")
	}

	link, err := r.requireSpaceLink(ctx, event)
	if err != nil || link == nil {
		return err
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scion register` first.")
	}

	// Parse --thread flag
	var threadID string
	remaining := args
	for i := 0; i < len(remaining)-1; i++ {
		if remaining[i] == "--thread" {
			threadID = remaining[i+1]
			remaining = append(remaining[:i], remaining[i+2:]...)
			break
		}
	}

	if len(remaining) < 2 {
		return r.reply(ctx, event, "Usage: `/scion message [--thread <thread-id>] <agent-slug> <text>`")
	}

	agentSlug := remaining[0]
	messageText := strings.Join(remaining[1:], " ")

	// Use structured message to include thread context
	msg := messages.NewInstruction(event.UserID, agentSlug, messageText)
	if threadID != "" {
		// Include thread ID as part of the message metadata
		msg.Msg = fmt.Sprintf("[thread:%s] %s", threadID, msg.Msg)
	}

	if err := client.GroveAgents(link.GroveID).SendStructuredMessage(ctx, agentSlug, msg, false, false); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to send message to `%s`: %v", agentSlug, err))
	}

	reply := fmt.Sprintf("Message sent to agent `%s`.", agentSlug)
	if threadID != "" {
		reply += fmt.Sprintf(" (thread: `%s`)", threadID)
	}
	return r.reply(ctx, event, reply)
}

func (r *CommandRouter) cmdInfo(ctx context.Context, event *ChatEvent, args []string) error {
	// User registration state
	registrationStatus := "Not registered"
	registeredEmail := ""
	mapping, err := r.idMapper.Resolve(event.UserID, event.Platform)
	if err != nil {
		return fmt.Errorf("checking registration: %w", err)
	}
	if mapping != nil {
		registrationStatus = "Registered"
		registeredEmail = mapping.HubUserEmail
	}

	// Grove linkage state
	linkStatus := "Not linked"
	groveSlug := ""
	var link *state.SpaceLink
	link, err = r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return fmt.Errorf("checking space link: %w", err)
	}
	if link != nil {
		linkStatus = "Linked"
		groveSlug = link.GroveSlug
	}

	// Build info card
	widgets := []Widget{
		{Type: WidgetKeyValue, Label: "Registration", Content: registrationStatus},
	}
	if registeredEmail != "" {
		widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Hub Email", Content: registeredEmail})
	}
	widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Grove Link", Content: linkStatus})
	if groveSlug != "" {
		widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Grove", Content: groveSlug})
	}

	// If linked and registered, fetch agent count from the grove
	if link != nil && mapping != nil {
		client, clientErr := r.idMapper.ClientFor(ctx, mapping)
		if clientErr == nil {
			grove, groveErr := client.Groves().Get(ctx, link.GroveSlug)
			if groveErr == nil {
				widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Agents", Content: fmt.Sprintf("%d", grove.AgentCount)})
			}
		}
	}

	card := Card{
		Header: CardHeader{
			Title:    "Scion Info",
			Subtitle: fmt.Sprintf("Space: %s", event.SpaceID),
		},
		Sections: []CardSection{
			{
				Header:  "Space & Identity",
				Widgets: widgets,
			},
		},
	}

	_, err = r.messenger.SendCard(ctx, event.SpaceID, card)
	return err
}

func (r *CommandRouter) cmdHelp(ctx context.Context, event *ChatEvent) error {
	help := `*Scion Chat Bot Commands:*

*Agent Management:*
• ` + "`/scion list`" + ` — List agents in linked grove
• ` + "`/scion status <agent>`" + ` — Show agent status
• ` + "`/scion start <agent>`" + ` — Start an agent
• ` + "`/scion stop <agent>`" + ` — Stop an agent
• ` + "`/scion create <name>`" + ` — Create a new agent
• ` + "`/scion delete <agent>`" + ` — Delete an agent (with confirmation)
• ` + "`/scion logs <agent>`" + ` — View recent agent logs
• ` + "`/scion message [--thread <id>] <agent> <text>`" + ` — Send a message to an agent

*Space & Identity:*
• ` + "`/scion info`" + ` — Show registration, grove link, and agent info
• ` + "`/scion link <grove-slug>`" + ` — Link this space to a grove
• ` + "`/scion unlink`" + ` — Unlink this space
• ` + "`/scion register`" + ` — Register your chat account
• ` + "`/scion unregister`" + ` — Unregister your account

*Notifications:*
• ` + "`/scion subscribe <agent>`" + ` — Subscribe to agent notifications
• ` + "`/scion unsubscribe <agent>`" + ` — Unsubscribe from notifications

• ` + "`/scion help`" + ` — Show this help message`

	return r.reply(ctx, event, help)
}

// --- Helper methods ---

// reply sends a text message back to the space where the event originated.
func (r *CommandRouter) reply(ctx context.Context, event *ChatEvent, text string) error {
	_, err := r.messenger.SendMessage(ctx, SendMessageRequest{
		SpaceID:  event.SpaceID,
		ThreadID: event.ThreadID,
		Text:     text,
	})
	return err
}

// requireSpaceLink checks that the space is linked to a grove, replying with an error if not.
func (r *CommandRouter) requireSpaceLink(ctx context.Context, event *ChatEvent) (*state.SpaceLink, error) {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return nil, r.reply(ctx, event, "This space is not linked to a grove. Use `/scion link <grove-slug>` first.")
	}
	return link, nil
}

// clientForUser creates a Hub client authenticated as the event's user.
func (r *CommandRouter) clientForUser(ctx context.Context, event *ChatEvent) (hubclient.Client, error) {
	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil {
		return nil, err
	}
	if mapping == nil {
		return nil, fmt.Errorf("user not registered")
	}
	return r.idMapper.ClientFor(ctx, mapping)
}
