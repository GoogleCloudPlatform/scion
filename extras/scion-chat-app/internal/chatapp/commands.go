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
	"net/url"
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

// hubHostname returns the hostname portion of the hub URL.
func (r *CommandRouter) hubHostname() string {
	if u, err := url.Parse(r.hubURL); err == nil && u.Host != "" {
		return u.Host
	}
	return r.hubURL
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
		if event.Command == "scionAdmin" {
			return r.handleAdminCommand(ctx, event)
		}
		return r.handleCommand(ctx, event)
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

// handleCommand parses "/scion <args>" and routes to messaging.
// The /scion command is focused entirely on sending messages to agents.
// If a default agent is set, the entire text is sent directly to it.
// Otherwise, the first word is tried as an agent slug.
func (r *CommandRouter) handleCommand(ctx context.Context, event *ChatEvent) (*EventResponse, error) {
	parts := strings.Fields(event.Args)
	if len(parts) == 0 {
		r.log.Info("scion command (no args, showing help)", "space", event.SpaceID, "user", event.UserID)
		return r.cmdScionHelp(ctx, event)
	}

	sub := strings.ToLower(parts[0])

	switch sub {
	case "help":
		r.log.Info("scion command (help)", "space", event.SpaceID, "user", event.UserID)
		return r.cmdScionHelp(ctx, event)
	case "message", "msg":
		r.log.Info("scion command (message)", "args", strings.Join(parts[1:], " "), "space", event.SpaceID, "user", event.UserID)
		return r.cmdMessage(ctx, event, parts[1:])
	default:
		r.log.Info("scion command (message mode)", "args", event.Args, "space", event.SpaceID, "user", event.UserID)
		return r.cmdMessage(ctx, event, parts)
	}
}

// handleAdminCommand parses "/scionAdmin <subcommand> <args>" and routes to
// administrative handlers (agent management, space linking, identity, etc.).
func (r *CommandRouter) handleAdminCommand(ctx context.Context, event *ChatEvent) (*EventResponse, error) {
	parts := strings.Fields(event.Args)
	if len(parts) == 0 {
		r.log.Info("admin command (no subcommand, showing help)", "space", event.SpaceID, "user", event.UserID)
		return r.cmdAdminHelp(ctx, event)
	}

	subcommand := strings.ToLower(parts[0])
	args := parts[1:]

	r.log.Info("admin command received", "subcommand", subcommand, "args", strings.Join(args, " "), "space", event.SpaceID, "user", event.UserID)

	var resp *EventResponse
	var err error

	switch subcommand {
	case "info":
		resp, err = r.cmdInfo(ctx, event, args)
	case "list":
		resp, err = r.cmdList(ctx, event, args)
	case "status":
		resp, err = r.cmdStatus(ctx, event, args)
	case "start":
		resp, err = r.cmdStart(ctx, event, args)
	case "stop":
		resp, err = r.cmdStop(ctx, event, args)
	case "create":
		resp, err = r.cmdCreate(ctx, event, args)
	case "delete":
		resp, err = r.cmdDelete(ctx, event, args)
	case "logs":
		resp, err = r.cmdLogs(ctx, event, args)
	case "link":
		resp, err = r.cmdLink(ctx, event, args)
	case "unlink":
		resp, err = r.cmdUnlink(ctx, event, args)
	case "register":
		resp, err = r.cmdRegister(ctx, event, args)
	case "unregister":
		resp, err = r.cmdUnregister(ctx, event, args)
	case "subscribe":
		resp, err = r.cmdSubscribe(ctx, event, args)
	case "unsubscribe":
		resp, err = r.cmdUnsubscribe(ctx, event, args)
	case "set-default":
		resp, err = r.cmdSetDefault(ctx, event, args)
	case "help":
		resp, err = r.cmdAdminHelp(ctx, event)
	default:
		r.log.Warn("unknown admin command", "subcommand", subcommand)
		resp = textResponse(event, fmt.Sprintf("Unknown command: `%s`. Use `/scionAdmin help` for available commands.", subcommand))
	}

	if err != nil {
		r.log.Error("admin command failed", "subcommand", subcommand, "error", err)
	} else {
		r.log.Info("admin command completed", "subcommand", subcommand)
	}
	return resp, err
}

// handleMessage routes @mention messages to an agent.
func (r *CommandRouter) handleMessage(ctx context.Context, event *ChatEvent) error {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return r.reply(ctx, event, "This space is not linked to a grove. Use `/scionAdmin link <grove-slug>` to link it.")
	}

	// Try to resolve the user
	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil {
		return fmt.Errorf("resolving user: %w", err)
	}
	if mapping == nil {
		return r.reply(ctx, event, "You are not registered. Use `/scionAdmin register` to link your chat account to your Hub account.")
	}

	// For MVP: send to the first running agent mentioned in the text,
	// or prompt for target if ambiguous
	return r.reply(ctx, event, "Message received. Use `/scion <agent> <text>` to send to a specific agent.")
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
				return r.reply(ctx, event, "Authentication required. Use `/scionAdmin register` first.")
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

		link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
		if err != nil {
			return fmt.Errorf("getting space link: %w", err)
		}
		if link == nil {
			return r.reply(ctx, event, "This space is not linked to a grove.")
		}

		client, err := r.clientForUser(ctx, event)
		if err != nil {
			return r.reply(ctx, event, "Authentication required. Use `/scionAdmin register` first.")
		}

		if err := client.GroveAgents(link.GroveID).SendMessage(ctx, agentID, responseText, false); err != nil {
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
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return r.reply(ctx, event, "This space is not linked to a grove.")
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scionAdmin register` first.")
	}

	agents := client.GroveAgents(link.GroveID)

	switch verb {
	case "start":
		if err := agents.Start(ctx, agentID); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to start agent: %v", err))
		}
		return r.reply(ctx, event, fmt.Sprintf("Agent `%s` started.", agentID))
	case "stop":
		if err := agents.Stop(ctx, agentID); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to stop agent: %v", err))
		}
		return r.reply(ctx, event, fmt.Sprintf("Agent `%s` stopped.", agentID))
	case "logs":
		logs, err := agents.GetLogs(ctx, agentID, &hubclient.GetLogsOptions{Tail: 50})
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
		resp, err := r.showDeleteConfirmation(ctx, event, agentID)
		if err != nil {
			return err
		}
		if resp != nil && resp.Message != nil {
			if resp.Message.Card != nil {
				_, err = r.messenger.SendCard(ctx, event.SpaceID, *resp.Message.Card)
			} else {
				_, err = r.messenger.SendMessage(ctx, *resp.Message)
			}
		}
		return err
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
	return r.reply(ctx, event, "Hello! I'm Scion Bot. Use `/scionAdmin link <grove-slug>` to connect this space to a grove, then `/scionAdmin help` for available commands.")
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

func (r *CommandRouter) cmdList(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	link, resp := r.requireSpaceLink(ctx, event)
	if resp != nil {
		return resp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	// Fetch current grove info from hub to ensure we display the latest slug.
	grove, err := client.Groves().Get(ctx, link.GroveID)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to get grove info: %v", err)), nil
	}
	if grove.Slug != link.GroveSlug {
		link.GroveSlug = grove.Slug
		if storeErr := r.store.SetSpaceLink(link); storeErr != nil {
			r.log.Warn("failed to update cached grove slug", "error", storeErr)
		}
	}

	agents, err := client.GroveAgents(link.GroveID).List(ctx, nil)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to list agents: %v", err)), nil
	}

	if len(agents.Agents) == 0 {
		return textResponse(event, fmt.Sprintf("No agents in grove `%s`.", grove.Slug)), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Agents in %s:*\n", grove.Slug))
	for _, a := range agents.Agents {
		status := a.Activity
		if status == "" {
			status = a.Phase
		}
		marker := ""
		if link.DefaultAgent != "" && a.Slug == link.DefaultAgent {
			marker = " *"
		}
		sb.WriteString(fmt.Sprintf("• `%s` — %s%s\n", a.Slug, status, marker))
	}
	if link.DefaultAgent != "" {
		sb.WriteString("\n_* default agent_")
	}
	return textResponse(event, sb.String()), nil
}

func (r *CommandRouter) cmdStatus(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin status <agent-slug>`"), nil
	}

	link, resp := r.requireSpaceLink(ctx, event)
	if resp != nil {
		return resp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	agent, err := client.GroveAgents(link.GroveID).Get(ctx, args[0])
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to get agent: %v", err)), nil
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

	return cardResponse(event, &card), nil
}

func (r *CommandRouter) cmdStart(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin start <agent-slug>`"), nil
	}

	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	if err := client.GroveAgents(link.GroveID).Start(ctx, args[0]); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to start agent: %v", err)), nil
	}
	return textResponse(event, fmt.Sprintf("Agent `%s` started.", args[0])), nil
}

func (r *CommandRouter) cmdStop(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin stop <agent-slug>`"), nil
	}

	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	if err := client.GroveAgents(link.GroveID).Stop(ctx, args[0]); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to stop agent: %v", err)), nil
	}
	return textResponse(event, fmt.Sprintf("Agent `%s` stopped.", args[0])), nil
}

func (r *CommandRouter) cmdCreate(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin create <agent-name>`"), nil
	}

	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	createResp, err := client.GroveAgents(link.GroveID).Create(ctx, &hubclient.CreateAgentRequest{
		Name: args[0],
	})
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to create agent: %v", err)), nil
	}
	return textResponse(event, fmt.Sprintf("Agent `%s` created (ID: `%s`).", createResp.Agent.Slug, createResp.Agent.ID)), nil
}

func (r *CommandRouter) cmdLink(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin link <grove-slug>`"), nil
	}

	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil || mapping == nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	client, err := r.idMapper.ClientFor(ctx, mapping)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to create client: %v", err)), nil
	}

	// Look up the grove by slug
	groveList, err := client.Groves().List(ctx, &hubclient.ListGrovesOptions{Slug: args[0]})
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to look up grove `%s`: %v", args[0], err)), nil
	}
	if len(groveList.Groves) == 0 {
		return textResponse(event, fmt.Sprintf("Grove `%s` not found. Use the grove slug, not the ID.", args[0])), nil
	}
	grove := &groveList.Groves[0]

	// Save the link
	link := &state.SpaceLink{
		SpaceID:   event.SpaceID,
		Platform:  event.Platform,
		GroveID:   grove.ID,
		GroveSlug: grove.Slug,
		LinkedBy:  mapping.HubUserID,
	}
	if err := r.store.SetSpaceLink(link); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to save link: %v", err)), nil
	}

	// Subscribe only to user-targeted messages so that agent-to-agent
	// traffic and broadcasts do not leak into chat.
	if r.broker != nil {
		pattern := fmt.Sprintf("scion.grove.%s.user.>", grove.ID)
		if err := r.broker.RequestSubscription(pattern); err != nil {
			r.log.Warn("failed to request grove subscription", "grove_id", grove.ID, "error", err)
		}
	}

	return textResponse(event, fmt.Sprintf("This space is now linked to grove `%s`.", grove.Slug)), nil
}

func (r *CommandRouter) cmdUnlink(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return textResponse(event, "This space is not linked to any grove."), nil
	}

	// Cancel broker subscription (must match the pattern used during link).
	if r.broker != nil {
		pattern := fmt.Sprintf("scion.grove.%s.user.>", link.GroveID)
		if err := r.broker.CancelSubscription(pattern); err != nil {
			r.log.Warn("failed to cancel grove subscription", "grove_id", link.GroveID, "error", err)
		}
	}

	if err := r.store.DeleteSpaceLink(event.SpaceID, event.Platform); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to unlink: %v", err)), nil
	}
	return textResponse(event, fmt.Sprintf("Unlinked from grove `%s`.", link.GroveSlug)), nil
}

func (r *CommandRouter) cmdRegister(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	// Check if already registered
	existing, err := r.idMapper.Resolve(event.UserID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("checking registration: %w", err)
	}
	if existing != nil {
		return textResponse(event, fmt.Sprintf("You are already registered as `%s`.", existing.HubUserEmail)), nil
	}

	// Try auto-registration by email (short-circuit)
	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("auto-registration: %w", err)
	}
	if mapping != nil {
		return textResponse(event, fmt.Sprintf("Registered! Your chat account is linked to Hub user `%s`.", mapping.HubUserEmail)), nil
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
	authResp, err := r.adminClient.Auth().RequestDeviceCode(ctx, "")
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to start device authorization: %v", err)), nil
	}

	pa := &pendingDeviceAuth{
		deviceCode: authResp.DeviceCode,
		userCode:   authResp.UserCode,
		verifyURL:  authResp.VerificationURL,
		expiresAt:  time.Now().Add(time.Duration(authResp.ExpiresIn) * time.Second),
		interval:   time.Duration(authResp.Interval) * time.Second,
	}
	r.mu.Lock()
	r.pendingAuth[authKey] = pa
	r.mu.Unlock()

	verifyURL := authResp.VerificationURL
	if authResp.VerificationURLComplete != "" {
		verifyURL = authResp.VerificationURLComplete
	}

	card := Card{
		Header: CardHeader{
			Title:    "Device Authorization",
			Subtitle: "No matching Hub account found for your chat email",
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetText, Content: fmt.Sprintf("Your chat email doesn't match any Hub user. Sign in with your Hub account to link it:\n\n*URL:* %s\n*Code:* `%s`", verifyURL, authResp.UserCode)},
				},
			},
			{
				Header: "After completing authorization:",
				Widgets: []Widget{
					{Type: WidgetText, Content: "Run `/scionAdmin register confirm` to finish registration."},
				},
			},
		},
	}

	return cardResponse(event, &card), nil
}

// pollDeviceAuth polls for device authorization completion and registers the user.
func (r *CommandRouter) pollDeviceAuth(ctx context.Context, event *ChatEvent, pending *pendingDeviceAuth) (*EventResponse, error) {
	authKey := event.UserID + ":" + event.Platform

	if time.Now().After(pending.expiresAt) {
		r.mu.Lock()
		delete(r.pendingAuth, authKey)
		r.mu.Unlock()
		return textResponse(event, "Device authorization expired. Run `/scionAdmin register` to start again."), nil
	}

	resp, err := r.adminClient.Auth().PollDeviceToken(ctx, pending.deviceCode, "")
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to check authorization status: %v", err)), nil
	}

	switch resp.Status {
	case "authorization_pending":
		return textResponse(event, "Authorization still pending. Complete the flow in your browser, then run `/scionAdmin register confirm` again."), nil
	case "expired_token":
		r.mu.Lock()
		delete(r.pendingAuth, authKey)
		r.mu.Unlock()
		return textResponse(event, "Device authorization expired. Run `/scionAdmin register` to start again."), nil
	case "slow_down":
		return textResponse(event, "Please wait a moment before trying again."), nil
	case "":
		// Success — token received
		if resp.User == nil {
			return textResponse(event, "Authorization succeeded but no user info returned. Please try again."), nil
		}

		// Register the mapping
		if err := r.idMapper.Register(event.UserID, event.Platform, resp.User.ID, resp.User.Email); err != nil {
			return textResponse(event, fmt.Sprintf("Authorization succeeded but failed to save registration: %v", err)), nil
		}

		r.mu.Lock()
		delete(r.pendingAuth, authKey)
		r.mu.Unlock()

		return textResponse(event, fmt.Sprintf("Registered! Your chat account is linked to Hub user `%s`.", resp.User.Email)), nil
	default:
		return textResponse(event, fmt.Sprintf("Unexpected authorization status: %s", resp.Status)), nil
	}
}

func (r *CommandRouter) cmdUnregister(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if err := r.idMapper.Unregister(event.UserID, event.Platform); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to unregister: %v", err)), nil
	}
	return textResponse(event, "Your chat account has been unlinked from your Hub account."), nil
}

func (r *CommandRouter) cmdDelete(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin delete <agent-slug>`"), nil
	}
	return r.showDeleteConfirmation(ctx, event, args[0])
}

// showDeleteConfirmation presents a confirmation card before deleting an agent.
func (r *CommandRouter) showDeleteConfirmation(ctx context.Context, event *ChatEvent, agentSlug string) (*EventResponse, error) {
	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	agent, err := client.GroveAgents(link.GroveID).Get(ctx, agentSlug)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Agent `%s` not found: %v", agentSlug, err)), nil
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

	return cardResponse(event, &card), nil
}

// executeDelete performs the actual agent deletion after confirmation.
func (r *CommandRouter) executeDelete(ctx context.Context, event *ChatEvent, agentID string) error {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return r.reply(ctx, event, "This space is not linked to a grove.")
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scionAdmin register` first.")
	}

	if err := client.GroveAgents(link.GroveID).Delete(ctx, agentID, nil); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to delete agent: %v", err))
	}
	return r.reply(ctx, event, fmt.Sprintf("Agent `%s` deleted.", agentID))
}

func (r *CommandRouter) cmdLogs(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin logs <agent-slug>`"), nil
	}

	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	opts := &hubclient.GetLogsOptions{Tail: 50}
	logs, err := client.GroveAgents(link.GroveID).GetLogs(ctx, args[0], opts)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to get logs for `%s`: %v", args[0], err)), nil
	}

	if logs == "" {
		return textResponse(event, fmt.Sprintf("No logs available for agent `%s`.", args[0])), nil
	}

	// Truncate for chat display
	if len(logs) > 2000 {
		logs = "...\n" + logs[len(logs)-2000:]
	}
	return textResponse(event, fmt.Sprintf("*Logs for `%s`:*\n```\n%s\n```", args[0], logs)), nil
}

func (r *CommandRouter) cmdSubscribe(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin subscribe <agent-slug>`"), nil
	}

	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
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
			return textResponse(event, fmt.Sprintf("Failed to subscribe: %v", err)), nil
		}
		return textResponse(event, fmt.Sprintf("Subscribed to notifications for agent `%s`. Filtered to: %s", agentSlug, activities)), nil
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

	return cardResponse(event, &card), nil
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

func (r *CommandRouter) cmdUnsubscribe(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin unsubscribe <agent-slug>`"), nil
	}

	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	if err := r.store.DeleteAgentSubscription(event.UserID, event.Platform, args[0], link.GroveID); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to unsubscribe: %v", err)), nil
	}
	return textResponse(event, fmt.Sprintf("Unsubscribed from notifications for agent `%s`.", args[0])), nil
}

func (r *CommandRouter) cmdMessage(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) < 1 {
		return textResponse(event, "Usage: `/scion [--thread <thread-id>] <agent-slug> <text>`"), nil
	}

	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil || mapping == nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}
	client, err := r.idMapper.ClientFor(ctx, mapping)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to create client: %v", err)), nil
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

	if len(remaining) < 1 {
		return textResponse(event, "Usage: `/scion [--thread <thread-id>] <agent-slug> <text>`"), nil
	}

	agentSlug := remaining[0]
	messageText := strings.Join(remaining[1:], " ")

	// Try to resolve the first arg as an agent. If it doesn't match and a
	// default agent is configured, treat the entire input as the message text.
	agent, err := client.GroveAgents(link.GroveID).Get(ctx, agentSlug)
	if err != nil {
		if link.DefaultAgent == "" {
			return textResponse(event, fmt.Sprintf("Agent `%s` not found: %v", agentSlug, err)), nil
		}
		r.log.Warn("agent slug lookup failed, falling back to default agent",
			"original_slug", agentSlug,
			"default_agent", link.DefaultAgent,
			"grove_id", link.GroveID,
			"error", err,
		)
		agentSlug = link.DefaultAgent
		messageText = strings.Join(remaining, " ")
		agent, err = client.GroveAgents(link.GroveID).Get(ctx, agentSlug)
		if err != nil {
			return textResponse(event, fmt.Sprintf("Default agent `%s` not found: %v", agentSlug, err)), nil
		}
	}
	if agent.Phase == "stopped" {
		return textResponse(event, fmt.Sprintf("Agent `%s` is stopped. Start it with `/scionAdmin start %s` before sending messages.", agentSlug, agentSlug)), nil
	}

	// Use the hub user email with "user:" prefix so agents can address replies
	msg := messages.NewInstruction("user:"+mapping.HubUserEmail, agentSlug, messageText)
	if threadID != "" {
		// Include thread ID as part of the message metadata
		msg.Msg = fmt.Sprintf("[thread:%s] %s", threadID, msg.Msg)
	}

	if err := client.GroveAgents(link.GroveID).SendStructuredMessage(ctx, agentSlug, msg, false, false); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to send message to `%s`: %v", agentSlug, err)), nil
	}

	displayName := event.UserDisplayName
	if displayName == "" {
		displayName = event.UserEmail
	}
	replyText := fmt.Sprintf("Message from *%s* sent to *%s*:\n%s", displayName, agentSlug, messageText)
	return textResponse(event, replyText), nil
}

func (r *CommandRouter) cmdSetDefault(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	if len(args) == 0 {
		if link.DefaultAgent == "" {
			return textResponse(event, "No default agent is set. Usage: `/scionAdmin set-default <agent-slug>`"), nil
		}
		return textResponse(event, fmt.Sprintf("Default agent is `%s`. Use `/scionAdmin set-default clear` to remove.", link.DefaultAgent)), nil
	}

	arg := strings.ToLower(args[0])
	if arg == "clear" || arg == "none" {
		if err := r.store.ClearDefaultAgent(event.SpaceID, event.Platform); err != nil {
			return textResponse(event, fmt.Sprintf("Failed to clear default agent: %v", err)), nil
		}
		return textResponse(event, "Default agent cleared."), nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	agent, err := client.GroveAgents(link.GroveID).Get(ctx, args[0])
	if err != nil {
		return textResponse(event, fmt.Sprintf("Agent `%s` not found: %v", args[0], err)), nil
	}

	if err := r.store.SetDefaultAgent(event.SpaceID, event.Platform, agent.Slug); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to set default agent: %v", err)), nil
	}
	return textResponse(event, fmt.Sprintf("Default agent set to `%s`. Messages that don't match an agent name will be sent here.", agent.Slug)), nil
}

func (r *CommandRouter) cmdInfo(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	// User registration state
	registrationStatus := "Not registered"
	registeredEmail := ""
	mapping, err := r.idMapper.Resolve(event.UserID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("checking registration: %w", err)
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
		return nil, fmt.Errorf("checking space link: %w", err)
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
			groveList, groveErr := client.Groves().List(ctx, &hubclient.ListGrovesOptions{Slug: link.GroveSlug})
			if groveErr == nil && len(groveList.Groves) > 0 {
				widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Agents", Content: fmt.Sprintf("%d", groveList.Groves[0].AgentCount)})
			}
		}
	}
	if link != nil && link.DefaultAgent != "" {
		widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Default Agent", Content: link.DefaultAgent})
	}

	card := Card{
		Header: CardHeader{
			Title:    "Scion Info",
			Subtitle: fmt.Sprintf("Hub: %s", r.hubHostname()),
		},
		Sections: []CardSection{
			{
				Header:  "Space & Identity",
				Widgets: widgets,
			},
		},
	}

	return &EventResponse{
		Message: &SendMessageRequest{
			Card: &card,
		},
	}, nil
}

func (r *CommandRouter) cmdScionHelp(ctx context.Context, event *ChatEvent) (*EventResponse, error) {
	help := `*Scion — Message Agents:*

• ` + "`/scion <text>`" + ` — Send a message to the default agent
• ` + "`/scion <agent> <text>`" + ` — Send a message to a specific agent
• ` + "`/scion --thread <id> <agent> <text>`" + ` — Send in a specific thread

_If a default agent is set, all text is sent directly to it. Otherwise, the first word is used as the agent slug._

Use ` + "`/scionAdmin help`" + ` for agent management and space administration commands.`

	return textResponse(event, help), nil
}

func (r *CommandRouter) cmdAdminHelp(ctx context.Context, event *ChatEvent) (*EventResponse, error) {
	help := `*Scion Admin Commands:*

*Agent Management:*
• ` + "`/scionAdmin list`" + ` — List agents in linked grove
• ` + "`/scionAdmin status <agent>`" + ` — Show agent status
• ` + "`/scionAdmin start <agent>`" + ` — Start an agent
• ` + "`/scionAdmin stop <agent>`" + ` — Stop an agent
• ` + "`/scionAdmin create <name>`" + ` — Create a new agent
• ` + "`/scionAdmin delete <agent>`" + ` — Delete an agent (with confirmation)
• ` + "`/scionAdmin logs <agent>`" + ` — View recent agent logs
• ` + "`/scionAdmin set-default <agent>`" + ` — Set default agent for ` + "`/scion`" + ` messages (clear with ` + "`clear`" + `)

*Space & Identity:*
• ` + "`/scionAdmin info`" + ` — Show registration, grove link, and agent info
• ` + "`/scionAdmin link <grove-slug>`" + ` — Link this space to a grove
• ` + "`/scionAdmin unlink`" + ` — Unlink this space
• ` + "`/scionAdmin register`" + ` — Register your chat account
• ` + "`/scionAdmin unregister`" + ` — Unregister your account

*Notifications:*
• ` + "`/scionAdmin subscribe <agent>`" + ` — Subscribe to agent notifications
• ` + "`/scionAdmin unsubscribe <agent>`" + ` — Unsubscribe from notifications

Use ` + "`/scion <text>`" + ` to message agents directly.`

	return textResponse(event, help), nil
}

// --- Helper methods ---

// reply sends a text message back to the space where the event originated.
// Used by non-command handlers (actions, messages, etc.) that respond asynchronously.
func (r *CommandRouter) reply(ctx context.Context, event *ChatEvent, text string) error {
	_, err := r.messenger.SendMessage(ctx, SendMessageRequest{
		SpaceID:  event.SpaceID,
		ThreadID: event.ThreadID,
		Text:     text,
	})
	return err
}

// textResponse creates a synchronous EventResponse containing a text message.
func textResponse(event *ChatEvent, text string) *EventResponse {
	return &EventResponse{
		Message: &SendMessageRequest{
			SpaceID:  event.SpaceID,
			ThreadID: event.ThreadID,
			Text:     text,
		},
	}
}

// cardResponse creates a synchronous EventResponse containing a card.
func cardResponse(event *ChatEvent, card *Card) *EventResponse {
	return &EventResponse{
		Message: &SendMessageRequest{
			SpaceID:  event.SpaceID,
			ThreadID: event.ThreadID,
			Card:     card,
		},
	}
}

// requireSpaceLink checks that the space is linked to a grove, returning an error response if not.
func (r *CommandRouter) requireSpaceLink(ctx context.Context, event *ChatEvent) (*state.SpaceLink, *EventResponse) {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return nil, textResponse(event, fmt.Sprintf("Failed to check grove link: %v", err))
	}
	if link == nil {
		return nil, textResponse(event, "This space is not linked to a grove. Use `/scionAdmin link <grove-slug>` first.")
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
