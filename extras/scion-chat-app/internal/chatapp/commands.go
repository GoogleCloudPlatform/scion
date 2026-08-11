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
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
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
	downloader  AttachmentDownloader
	broker      *BrokerServer
	log         *slog.Logger

	mu             sync.Mutex
	pendingAuth    map[string]*pendingDeviceAuth // keyed by platformUserID+platform
	pendingDeletes map[string]string             // keyed by actionID -> agentID

	// testClient, when non-nil, is returned by clientForUser instead of
	// going through the identity mapper. Used only in tests.
	testClient hubclient.Client
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
		adminClient: adminClient,
		hubURL:      hubURL,
		store:       store,
		idMapper:    idMapper,
		messenger:   messenger,
		broker:      broker,
		log:         log,
		pendingAuth: make(map[string]*pendingDeviceAuth),
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
	// If the messenger also implements AttachmentDownloader, wire it up.
	if d, ok := m.(AttachmentDownloader); ok {
		r.downloader = d
	}
}

// SetDownloader sets the attachment downloader (used for downloading files
// from chat platform events).
func (r *CommandRouter) SetDownloader(d AttachmentDownloader) {
	r.downloader = d
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
		return r.handleAction(ctx, event)
	case EventDialogSubmit:
		return r.handleDialogSubmit(ctx, event)
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
		if len(parts) == 1 {
			r.log.Info("scion command (help)", "space", event.SpaceID, "user", event.UserID)
			return r.cmdScionHelp(ctx, event)
		}
		return r.cmdMessage(ctx, event, parts)
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
	case "terminal":
		resp, err = r.cmdTerminal(ctx, event, args)
	case "thread":
		resp, err = r.cmdThread(ctx, event, args)
	case "send":
		resp, err = r.cmdSend(ctx, event, args)
	case "secret":
		resp, err = r.cmdSecret(ctx, event, args)
	case "settings":
		resp, err = r.cmdSettings(ctx, event, args)
	case "help":
		if len(args) == 0 {
			resp, err = r.cmdAdminHelp(ctx, event)
		} else {
			r.log.Warn("unknown admin command", "subcommand", strings.Join(parts, " "))
			resp = textResponse(event, fmt.Sprintf("Unknown command: `%s`. Use `/scionAdmin help` for available commands.", strings.Join(parts, " ")))
		}
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
		return r.reply(ctx, event, "This space is not linked to a project. Use `/scionAdmin link <project-slug>` to link it.")
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
func (r *CommandRouter) handleAction(ctx context.Context, event *ChatEvent) (*EventResponse, error) {
	parts := strings.Split(event.ActionID, ".")
	if len(parts) < 2 {
		return nil, nil
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
				return nil, r.reply(ctx, event, "Authentication required. Use `/scionAdmin register` first.")
			}
			return nil, client.Notifications().Acknowledge(ctx, targetID)
		}
	case "subscribe":
		// Card-based subscribe filter: arrives as EventAction when no
		// checkboxes are selected (no formInputs).
		if actionVerb == "filter" && targetID != "" {
			return r.handleSubscribeFilter(ctx, event)
		}
	case "secret":
		return nil, r.handleSecretAction(ctx, event, actionVerb, targetID)
	case "send":
		return nil, r.handleSendAction(ctx, event, actionVerb, targetID)
	case "settings":
		return nil, r.handleSettingsAction(ctx, event, actionVerb)
	}
	return nil, nil
}

// handleSecretAction processes secret-related button actions.
func (r *CommandRouter) handleSecretAction(ctx context.Context, event *ChatEvent, verb, targetID string) error {
	if verb != "set" || targetID == "" {
		return nil
	}

	// targetID is the secret key. The value comes from DialogData using the
	// action ID as the widget key, matching how the input card is constructed.
	key := targetID
	widgetKey := fmt.Sprintf("secret.set.%s", key)
	value := event.DialogData[widgetKey]
	if value == "" {
		return r.reply(ctx, event, "No secret value provided.")
	}

	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return r.reply(ctx, event, "This space is not linked to a project.")
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return r.reply(ctx, event, "Authentication required. Use `/scionAdmin register` first.")
	}

	if _, err := client.Secrets().Set(ctx, key, &hubclient.SetSecretRequest{
		Value:   value,
		Scope:   "project",
		ScopeID: link.ProjectID,
	}); err != nil {
		return r.reply(ctx, event, fmt.Sprintf("Failed to set secret `%s`: %v", key, err))
	}

	return r.reply(ctx, event, fmt.Sprintf("Secret `%s` has been set.", key))
}

// handleSendAction processes send-related file selection actions.
func (r *CommandRouter) handleSendAction(ctx context.Context, event *ChatEvent, verb, targetID string) error {
	if verb != "select" || targetID == "" {
		return nil
	}
	// targetID format: <agent-slug>.<path-hash>
	// Phase 5 will implement actual file selection and upload.
	return r.reply(ctx, event, "File selection and upload will be available in a future update.")
}

// handleDialogSubmit processes form submissions from interactive cards.
func (r *CommandRouter) handleDialogSubmit(ctx context.Context, event *ChatEvent) (*EventResponse, error) {
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
			return nil, r.reply(ctx, event, "No response text provided.")
		}

		link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
		if err != nil {
			return nil, fmt.Errorf("getting space link: %w", err)
		}
		if link == nil {
			return nil, r.reply(ctx, event, "This space is not linked to a project.")
		}

		mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
		if err != nil {
			r.log.Error("Failed to resolve user mapping", "error", err, "userID", event.UserID)
			return nil, r.reply(ctx, event, "Something went wrong, please try again later.")
		}
		if mapping == nil {
			return nil, r.reply(ctx, event, "Authentication required. Use `/scionAdmin register` first.")
		}
		client, err := r.idMapper.ClientFor(ctx, mapping)
		if err != nil {
			return nil, r.reply(ctx, event, fmt.Sprintf("Failed to create client: %v", err))
		}

		senderEmail := mapping.HubUserEmail
		if senderEmail == "" {
			return nil, r.reply(ctx, event, "Your user mapping is missing a valid email address.")
		}
		msg := messages.NewInstruction("user:"+senderEmail, agentID, responseText)
		msg.Channel = r.broker.ChannelName()
		if event.ThreadID != "" {
			msg.ThreadID = event.ThreadID
		}
		if _, err := client.ProjectAgents(link.ProjectID).SendStructuredMessage(ctx, agentID, msg, false, false, false); err != nil {
			return nil, r.reply(ctx, event, fmt.Sprintf("Failed to send response to agent: %v", err))
		}
		return nil, r.reply(ctx, event, fmt.Sprintf("Response sent to agent `%s`.", agentID))
	}

	// Handle delete confirmation (also handled via card action; kept for
	// backward compatibility with any remaining dialog-based flows).
	if strings.HasPrefix(event.ActionID, "agent.delete.confirm.") {
		agentID := strings.TrimPrefix(event.ActionID, "agent.delete.confirm.")
		return r.executeDelete(ctx, event, agentID, "")
	}

	// Handle subscription activity filter (arrives here when checkboxes are
	// selected, producing formInputs that the adapter classifies as DialogSubmit).
	if strings.HasPrefix(event.ActionID, "subscribe.filter.") {
		return r.handleSubscribeFilter(ctx, event)
	}

	// Google Chat normalizes button clicks that contain form inputs (e.g.
	// secret set, send select) as DialogSubmit events because formInputs is
	// non-empty. Fall back to the regular action handler so those buttons
	// are processed correctly.
	if event.ActionID != "" {
		return r.handleAction(ctx, event)
	}

	return nil, nil
}

// handleAgentAction processes agent-specific button actions.
func (r *CommandRouter) handleAgentAction(ctx context.Context, event *ChatEvent, verb, agentID string) (*EventResponse, error) {
	switch verb {
	case "delete":
		// Sub-actions from the delete confirmation card:
		//   agent.delete.confirm.<id> → execute deletion, update card in place
		//   agent.delete.cancel.<id>  → cancel deletion, update card in place
		if strings.HasPrefix(agentID, "confirm.") {
			trimmed := strings.TrimPrefix(agentID, "confirm.")
			parts := strings.SplitN(trimmed, ".", 2)
			realID := parts[0]
			var slug string
			if len(parts) > 1 {
				slug = parts[1]
			}
			return r.executeDelete(ctx, event, realID, slug)
		}
		if strings.HasPrefix(agentID, "cancel.") {
			return updateMessageResponse(event, "Deletion cancelled."), nil
		}
		// Plain agent.delete.<id> → show the confirmation card
		return r.showDeleteConfirmation(ctx, event, agentID)
	}

	// All other verbs require a space link and authenticated client.
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return nil, r.reply(ctx, event, "This space is not linked to a project.")
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return nil, r.reply(ctx, event, "Authentication required. Use `/scionAdmin register` first.")
	}

	agents := client.ProjectAgents(link.ProjectID)

	switch verb {
	case "status":
		agent, err := agents.Get(ctx, agentID)
		if err != nil {
			return nil, r.reply(ctx, event, fmt.Sprintf("Failed to get agent: %v", err))
		}
		card := Card{
			Header: CardHeader{
				Title:    agent.Name,
				Subtitle: fmt.Sprintf("Project: %s | %s", link.ProjectSlug, agent.Activity),
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
		return nil, err
	case "start":
		if err := agents.Start(ctx, agentID); err != nil {
			return nil, r.reply(ctx, event, fmt.Sprintf("Failed to start agent: %v", err))
		}
		return nil, r.reply(ctx, event, fmt.Sprintf("Agent `%s` started.", agentID))
	case "stop":
		if err := agents.Stop(ctx, agentID); err != nil {
			return nil, r.reply(ctx, event, fmt.Sprintf("Failed to stop agent: %v", err))
		}
		return nil, r.reply(ctx, event, fmt.Sprintf("Agent `%s` stopped.", agentID))
	case "logs":
		logs, err := agents.GetLogs(ctx, agentID, &hubclient.GetLogsOptions{Tail: 50})
		if err != nil {
			return nil, r.reply(ctx, event, fmt.Sprintf("Failed to get logs: %v", err))
		}
		if len(logs) > 2000 {
			logs = logs[len(logs)-2000:]
		}
		return nil, r.reply(ctx, event, fmt.Sprintf("*Logs for `%s`:*\n```\n%s\n```", agentID, logs))
	case "respond":
		// This is handled via dialog submit when user fills the inline input field.
		// If triggered as a plain action (no dialog data), prompt for input.
		return nil, r.reply(ctx, event, fmt.Sprintf("Use the inline response field in the notification card to respond to agent `%s`.", agentID))
	}
	return nil, nil
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
	return r.reply(ctx, event, "Hello! I'm Scion Bot. Use `/scionAdmin link <project-slug>` to connect this space to a project, then `/scionAdmin help` for available commands.")
}

// handleSpaceRemove is called when the bot is removed from a space.
func (r *CommandRouter) handleSpaceRemove(ctx context.Context, event *ChatEvent) error {
	// Clean up thread defaults for this space
	if err := r.store.DeleteThreadDefaultsForSpace(event.SpaceID); err != nil {
		r.log.Error("cleaning up thread defaults on removal", "error", err)
	}
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

	// Fetch current project info from hub to ensure we display the latest slug.
	proj, err := client.Projects().Get(ctx, link.ProjectID)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to get project info: %v", err)), nil
	}
	if proj.Slug != link.ProjectSlug {
		link.ProjectSlug = proj.Slug
		if storeErr := r.store.SetSpaceLink(link); storeErr != nil {
			r.log.Warn("failed to update cached project slug", "error", storeErr)
		}
	}

	agents, err := client.ProjectAgents(link.ProjectID).List(ctx, nil)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to list agents: %v", err)), nil
	}

	if len(agents.Agents) == 0 {
		return textResponse(event, fmt.Sprintf("No agents in project `%s`.", proj.Slug)), nil
	}

	// Check for a thread-level default if we're in a thread.
	var threadDefault string
	if event.ThreadID != "" {
		var err error
		threadDefault, err = r.store.GetThreadDefault(event.SpaceID, event.ThreadID, event.Platform)
		if err != nil {
			r.log.Error("failed to get thread default", "error", err)
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Agents in %s:*\n", proj.Slug))
	for _, a := range agents.Agents {
		status := a.Activity
		if status == "" {
			status = a.Phase
		}
		marker := ""
		if threadDefault != "" && a.Slug == threadDefault {
			marker = " †"
		} else if link.DefaultAgent != "" && a.Slug == link.DefaultAgent {
			marker = " *"
		}
		sb.WriteString(fmt.Sprintf("• `%s` — %s%s\n", a.Slug, status, marker))
	}
	hasLegend := false
	if link.DefaultAgent != "" {
		sb.WriteString("\n_* default agent_")
		hasLegend = true
	}
	if threadDefault != "" {
		if hasLegend {
			sb.WriteString("  ")
		} else {
			sb.WriteString("\n")
		}
		sb.WriteString("_† thread default_")
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

	agent, err := client.ProjectAgents(link.ProjectID).Get(ctx, args[0])
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to get agent: %v", err)), nil
	}

	card := Card{
		Header: CardHeader{
			Title:    agent.Name,
			Subtitle: fmt.Sprintf("Project: %s | %s", link.ProjectSlug, agent.Activity),
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

	if err := client.ProjectAgents(link.ProjectID).Start(ctx, args[0]); err != nil {
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

	if err := client.ProjectAgents(link.ProjectID).Stop(ctx, args[0]); err != nil {
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

	createResp, err := client.ProjectAgents(link.ProjectID).Create(ctx, &hubclient.CreateAgentRequest{
		Name: args[0],
	})
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to create agent: %v", err)), nil
	}
	return textResponse(event, fmt.Sprintf("Agent `%s` created (ID: `%s`).", createResp.Agent.Slug, createResp.Agent.ID)), nil
}

func (r *CommandRouter) cmdLink(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin link <project-slug>`"), nil
	}

	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil || mapping == nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	client, err := r.idMapper.ClientFor(ctx, mapping)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to create client: %v", err)), nil
	}

	// Look up the project by slug
	projectList, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{Slug: args[0]})
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to look up project `%s`: %v", args[0], err)), nil
	}
	if len(projectList.Projects) == 0 {
		return textResponse(event, fmt.Sprintf("Project `%s` not found. Use the project slug, not the ID.", args[0])), nil
	}
	proj := &projectList.Projects[0]

	// Save the link
	link := &state.SpaceLink{
		SpaceID:          event.SpaceID,
		Platform:         event.Platform,
		ProjectID:        proj.ID,
		ProjectSlug:      proj.Slug,
		LinkedBy:         mapping.HubUserID,
		ShowStateChanges: true,
	}
	if err := r.store.SetSpaceLink(link); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to save link: %v", err)), nil
	}

	// Subscribe to all project messages; observe mode filtering is
	// applied at delivery time in HandleBrokerMessage.
	if r.broker != nil {
		pattern := fmt.Sprintf("scion.grove.%s.>", proj.ID)
		if err := r.broker.RequestSubscription(pattern); err != nil {
			r.log.Warn("failed to request project subscription", "project_id", proj.ID, "error", err)
		}
	}

	return textResponse(event, fmt.Sprintf("This space is now linked to project `%s`.", proj.Slug)), nil
}

func (r *CommandRouter) cmdUnlink(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return textResponse(event, "This space is not linked to any project."), nil
	}

	// Cancel broker subscription (must match the pattern used during link).
	if r.broker != nil {
		pattern := fmt.Sprintf("scion.grove.%s.>", link.ProjectID)
		if err := r.broker.CancelSubscription(pattern); err != nil {
			r.log.Warn("failed to cancel project subscription", "project_id", link.ProjectID, "error", err)
		}
	}

	// Clean up thread defaults for this space.
	if err := r.store.DeleteThreadDefaultsForSpace(event.SpaceID); err != nil {
		r.log.Warn("failed to clean up thread defaults on unlink", "error", err)
	}

	if err := r.store.DeleteSpaceLink(event.SpaceID, event.Platform); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to unlink: %v", err)), nil
	}
	return textResponse(event, fmt.Sprintf("Unlinked from project `%s`.", link.ProjectSlug)), nil
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

	agent, err := client.ProjectAgents(link.ProjectID).Get(ctx, agentSlug)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Agent `%s` not found: %v", agentSlug, err)), nil
	}

	confirmID := fmt.Sprintf("agent.delete.confirm.%s.%s", agent.ID, agent.Slug)
	cancelID := fmt.Sprintf("agent.delete.cancel.%s.%s", agent.ID, agent.Slug)

	card := Card{
		Header: CardHeader{
			Title:    "Confirm Delete",
			Subtitle: fmt.Sprintf("Agent: %s", agent.Slug),
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetText, Content: fmt.Sprintf("Are you sure you want to delete agent *%s*?\n\nThis action cannot be undone.", agent.Slug)},
					{Type: WidgetKeyValue, Label: "Name", Content: agent.Name},
					{Type: WidgetKeyValue, Label: "Phase", Content: agent.Phase},
					{Type: WidgetKeyValue, Label: "Activity", Content: agent.Activity},
				},
			},
		},
		Actions: []CardAction{
			{Label: "Confirm Delete", ActionID: confirmID, Style: "danger"},
			{Label: "Cancel", ActionID: cancelID},
		},
	}

	return cardResponse(event, &card), nil
}

// executeDelete performs the actual agent deletion after confirmation.
// Returns an UpdateMessage response to replace the confirmation card in place.
func (r *CommandRouter) executeDelete(ctx context.Context, event *ChatEvent, agentID, agentSlug string) (*EventResponse, error) {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return updateMessageResponse(event, "This space is not linked to a project."), nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return updateMessageResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	if err := client.ProjectAgents(link.ProjectID).Delete(ctx, agentID, nil); err != nil {
		return updateMessageResponse(event, fmt.Sprintf("Failed to delete agent: %v", err)), nil
	}
	deletedName := agentSlug
	if deletedName == "" {
		deletedName = agentID
	}
	return updateMessageResponse(event, fmt.Sprintf("Agent `%s` deleted.", deletedName)), nil
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
	logs, err := client.ProjectAgents(link.ProjectID).GetLogs(ctx, args[0], opts)
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
			ProjectID:      link.ProjectID,
			Activities:     activities,
		}
		if err := r.store.SetAgentSubscription(sub); err != nil {
			return textResponse(event, fmt.Sprintf("Failed to subscribe: %v", err)), nil
		}
		return textResponse(event, fmt.Sprintf("Subscribed to notifications for agent `%s`. Filtered to: %s", agentSlug, activities)), nil
	}

	// Show activity filter dialog with checkboxes
	filterID := fmt.Sprintf("subscribe.filter.%s.%s", link.ProjectID, agentSlug)
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

// handleSubscribeFilter processes the subscription activity filter submission.
// This handler is reached via both EventAction (no checkboxes selected) and
// EventDialogSubmit (checkboxes selected, producing formInputs).
// Returns an UpdateMessage response to replace the filter card in place.
func (r *CommandRouter) handleSubscribeFilter(ctx context.Context, event *ChatEvent) (*EventResponse, error) {
	// ActionID format: subscribe.filter.<projectID>.<agentSlug>
	parts := strings.SplitN(event.ActionID, ".", 4)
	if len(parts) < 4 {
		return updateMessageResponse(event, "Invalid subscription filter action."), nil
	}
	projectID := parts[2]
	agentSlug := parts[3]

	// Collect selected activities from form data (populated from card
	// checkbox widgets via commonEventObject.formInputs).
	var activities string
	if selected, ok := event.DialogData[event.ActionID]; ok && selected != "" {
		activities = selected
	}

	sub := &state.AgentSubscription{
		PlatformUserID: event.UserID,
		Platform:       event.Platform,
		AgentID:        agentSlug,
		ProjectID:      projectID,
		Activities:     activities,
	}
	if err := r.store.SetAgentSubscription(sub); err != nil {
		return updateMessageResponse(event, fmt.Sprintf("Failed to subscribe: %v", err)), nil
	}

	msg := fmt.Sprintf("Subscribed to notifications for agent `%s`.", agentSlug)
	if activities != "" {
		msg += fmt.Sprintf(" Filtered to: %s", activities)
	} else {
		msg += " Receiving all activity types."
	}
	return updateMessageResponse(event, msg), nil
}

func (r *CommandRouter) cmdUnsubscribe(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin unsubscribe <agent-slug>`"), nil
	}

	link, linkResp := r.requireSpaceLink(ctx, event)
	if linkResp != nil {
		return linkResp, nil
	}

	if err := r.store.DeleteAgentSubscription(event.UserID, event.Platform, args[0], link.ProjectID); err != nil {
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
	agent, err := client.ProjectAgents(link.ProjectID).Get(ctx, agentSlug)
	if err != nil {
		targetThread := threadID
		if targetThread == "" {
			targetThread = event.ThreadID
		}
		defaultAgent, resolveErr := r.resolveDefaultAgent(event.SpaceID, targetThread, event.Platform, link.DefaultAgent)
		if resolveErr != nil {
			return textResponse(event, fmt.Sprintf("Failed to resolve default agent: %v", resolveErr)), nil
		}
		if defaultAgent == "" {
			return textResponse(event, fmt.Sprintf("Agent `%s` not found: %v", agentSlug, err)), nil
		}
		r.log.Warn("agent slug lookup failed, falling back to default agent",
			"original_slug", agentSlug,
			"default_agent", defaultAgent,
			"project_id", link.ProjectID,
			"error", err,
		)
		agentSlug = defaultAgent
		messageText = strings.Join(remaining, " ")
		agent, err = client.ProjectAgents(link.ProjectID).Get(ctx, agentSlug)
		if err != nil {
			return textResponse(event, fmt.Sprintf("Default agent `%s` not found: %v", agentSlug, err)), nil
		}
	}
	if agent.Phase == "stopped" {
		return textResponse(event, fmt.Sprintf("Agent `%s` is stopped. Start it with `/scionAdmin start %s` before sending messages.", agentSlug, agentSlug)), nil
	}

	// Use the hub user email with "user:" prefix so agents can address replies
	msg := messages.NewInstruction("user:"+mapping.HubUserEmail, agentSlug, messageText)
	msg.Channel = r.broker.ChannelName()
	if threadID != "" {
		msg.ThreadID = threadID
	} else if event.ThreadID != "" {
		msg.ThreadID = event.ThreadID
	}

	// Download and attach any files uploaded with the message.
	if len(event.Attachments) > 0 && r.downloader != nil {
		for _, att := range event.Attachments {
			agentPath, dlErr := r.downloadInboundAttachment(
				ctx, att, event.SpaceID, link.ProjectSlug, link.ProjectID,
			)
			if dlErr != nil {
				r.log.Error("failed to download attachment",
					"filename", att.Name, "error", dlErr)
				if strings.Contains(dlErr.Error(), "too large") {
					errCard := SizeLimitErrorCard(att.Name, 0)
					if _, sendErr := r.messenger.SendCard(ctx, event.SpaceID, errCard); sendErr != nil {
						r.log.Error("failed to send size limit card", "error", sendErr)
					}
				}
				continue
			}
			msg.Attachments = append(msg.Attachments, agentPath)
			if messageText != "" {
				messageText += "\n"
			}
			messageText += fmt.Sprintf("[Attachment: %s (%s)]", att.Name, att.ContentType)
		}
		// Update the message body with attachment placeholders.
		msg.Msg = messageText
	}

	if _, err := client.ProjectAgents(link.ProjectID).SendStructuredMessage(ctx, agentSlug, msg, false, false, false); err != nil {
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

	// Parse --thread flag from args.
	threadMode := false
	var filtered []string
	for _, a := range args {
		if a == "--thread" {
			threadMode = true
		} else {
			filtered = append(filtered, a)
		}
	}

	if threadMode && event.ThreadID == "" {
		return textResponse(event, "The `--thread` flag can only be used inside a thread."), nil
	}

	if len(filtered) == 0 {
		if threadMode {
			agent, err := r.store.GetThreadDefault(event.SpaceID, event.ThreadID, event.Platform)
			if err != nil {
				return textResponse(event, fmt.Sprintf("Failed to get thread default: %v", err)), nil
			}
			if agent == "" {
				return textResponse(event, "No thread-level default agent is set. Usage: `/scionAdmin set-default <agent-slug> --thread`"), nil
			}
			return textResponse(event, fmt.Sprintf("Thread default agent is `%s`. Use `/scionAdmin set-default clear --thread` to remove.", agent)), nil
		}
		if link.DefaultAgent == "" {
			return textResponse(event, "No default agent is set. Usage: `/scionAdmin set-default <agent-slug>`"), nil
		}
		return textResponse(event, fmt.Sprintf("Default agent is `%s`. Use `/scionAdmin set-default clear` to remove.", link.DefaultAgent)), nil
	}

	arg := strings.ToLower(filtered[0])
	if arg == "clear" || arg == "none" {
		if threadMode {
			if err := r.store.DeleteThreadDefault(event.SpaceID, event.ThreadID, event.Platform); err != nil {
				return textResponse(event, fmt.Sprintf("Failed to clear thread default agent: %v", err)), nil
			}
			return textResponse(event, "Thread-level default agent cleared."), nil
		}
		if err := r.store.ClearDefaultAgent(event.SpaceID, event.Platform); err != nil {
			return textResponse(event, fmt.Sprintf("Failed to clear default agent: %v", err)), nil
		}
		return textResponse(event, "Default agent cleared."), nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	agent, err := client.ProjectAgents(link.ProjectID).Get(ctx, filtered[0])
	if err != nil {
		return textResponse(event, fmt.Sprintf("Agent `%s` not found: %v", filtered[0], err)), nil
	}

	if threadMode {
		if err := r.store.SetThreadDefault(event.SpaceID, event.ThreadID, event.Platform, agent.Slug, event.UserEmail); err != nil {
			return textResponse(event, fmt.Sprintf("Failed to set thread default agent: %v", err)), nil
		}
		return textResponse(event, fmt.Sprintf("Thread default agent set to `%s`. Messages in this thread that don't match an agent name will be sent here.", agent.Slug)), nil
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

	// Project linkage state
	linkStatus := "Not linked"
	projectSlug := ""
	var link *state.SpaceLink
	link, err = r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return nil, fmt.Errorf("checking space link: %w", err)
	}
	if link != nil {
		linkStatus = "Linked"
		projectSlug = link.ProjectSlug
	}

	// Build info card
	widgets := []Widget{
		{Type: WidgetKeyValue, Label: "Registration", Content: registrationStatus},
	}
	if registeredEmail != "" {
		widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Hub Email", Content: registeredEmail})
	}
	widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Project Link", Content: linkStatus})
	if projectSlug != "" {
		widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Project", Content: projectSlug})
	}

	// If linked and registered, fetch agent count from the project
	if link != nil && mapping != nil {
		client, clientErr := r.idMapper.ClientFor(ctx, mapping)
		if clientErr == nil {
			projectList, projectErr := client.Projects().List(ctx, &hubclient.ListProjectsOptions{Slug: link.ProjectSlug})
			if projectErr == nil && len(projectList.Projects) > 0 {
				widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Agents", Content: fmt.Sprintf("%d", projectList.Projects[0].AgentCount)})
			}
		}
	}
	if link != nil && link.DefaultAgent != "" {
		widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Default Agent", Content: link.DefaultAgent})
	}
	if link != nil && event.ThreadID != "" {
		threadAgent, threadErr := r.store.GetThreadDefault(event.SpaceID, event.ThreadID, event.Platform)
		if threadErr != nil {
			r.log.Error("failed to get thread default", "error", threadErr)
		} else if threadAgent != "" {
			widgets = append(widgets, Widget{Type: WidgetKeyValue, Label: "Thread Default", Content: threadAgent})
		}
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
• ` + "`/scionAdmin list`" + ` — List agents in linked project
• ` + "`/scionAdmin status <agent>`" + ` — Show agent status
• ` + "`/scionAdmin start <agent>`" + ` — Start an agent
• ` + "`/scionAdmin stop <agent>`" + ` — Stop an agent
• ` + "`/scionAdmin create <name>`" + ` — Create a new agent
• ` + "`/scionAdmin delete <agent>`" + ` — Delete an agent (with confirmation)
• ` + "`/scionAdmin logs <agent>`" + ` — View recent agent logs
• ` + "`/scionAdmin set-default <agent>`" + ` — Set default agent for ` + "`/scion`" + ` messages (clear with ` + "`clear`" + `)
• ` + "`/scionAdmin set-default <agent> --thread`" + ` — Set default agent for the current thread (clear with ` + "`clear --thread`" + `)
• ` + "`/scionAdmin terminal <agent>`" + ` — Get the web terminal URL for an agent
• ` + "`/scionAdmin thread <name> [instruction]`" + ` — Create a new agent and thread
• ` + "`/scionAdmin send <agent> <path>`" + ` — Show file info from an agent's workspace

*Secrets:*
• ` + "`/scionAdmin secret list`" + ` — List project secrets (metadata only)
• ` + "`/scionAdmin secret set <key>`" + ` — Set a secret value (entered via secure card input)
• ` + "`/scionAdmin secret get <key>`" + ` — Show secret metadata
• ` + "`/scionAdmin secret delete <key>`" + ` — Delete a secret

*Space & Identity:*
• ` + "`/scionAdmin info`" + ` — Show registration, project link, and agent info
• ` + "`/scionAdmin link <project-slug>`" + ` — Link this space to a project
• ` + "`/scionAdmin unlink`" + ` — Unlink this space
• ` + "`/scionAdmin register`" + ` — Register your chat account
• ` + "`/scionAdmin unregister`" + ` — Unregister your account
• ` + "`/scionAdmin settings`" + ` — Toggle observe mode and notification filters

*Notifications:*
• ` + "`/scionAdmin subscribe <agent>`" + ` — Subscribe to agent notifications
• ` + "`/scionAdmin unsubscribe <agent>`" + ` — Unsubscribe from notifications

Use ` + "`/scion <text>`" + ` to message agents directly.`

	return textResponse(event, help), nil
}

// resolveDefaultAgent returns the default agent for a message, checking
// thread-level defaults first (if threadID is non-empty), then falling back
// to the provided space-level default.
func (r *CommandRouter) resolveDefaultAgent(spaceID, threadID, platform, spaceDefault string) (string, error) {
	if threadID != "" {
		agent, err := r.store.GetThreadDefault(spaceID, threadID, platform)
		if err != nil {
			return "", err
		}
		if agent != "" {
			return agent, nil
		}
	}
	return spaceDefault, nil
}

// --- Terminal, Thread, Send, Secret commands ---

func (r *CommandRouter) cmdTerminal(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin terminal <agent-slug>`"), nil
	}

	link, resp := r.requireSpaceLink(ctx, event)
	if resp != nil {
		return resp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	agentSlug := args[0]
	// Use Get (which supports slug lookup) instead of List+scan.
	agent, err := client.ProjectAgents(link.ProjectID).Get(ctx, agentSlug)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Agent `%s` not found: %v", agentSlug, err)), nil
	}
	if agent == nil {
		return textResponse(event, fmt.Sprintf("Agent `%s` not found.", agentSlug)), nil
	}

	if strings.ToLower(agent.Phase) != "running" {
		phase := agent.Phase
		if phase == "" {
			phase = "unknown"
		}
		return textResponse(event, fmt.Sprintf("Agent `%s` is not running (phase: %s).", agent.Slug, phase)), nil
	}
	terminalURL := fmt.Sprintf("%s/agents/%s/terminal", r.hubURL, agent.ID)
	card := Card{
		Header: CardHeader{
			Title:    "Web Terminal",
			Subtitle: fmt.Sprintf("Agent: %s", agent.Slug),
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetText, Content: fmt.Sprintf("Open the web terminal for agent `%s`:", agent.Slug)},
				},
			},
		},
		Actions: []CardAction{
			{Label: "Open Terminal", ActionID: fmt.Sprintf("link.%s", terminalURL), Style: "primary"},
		},
	}
	return cardResponse(event, &card), nil
}

func (r *CommandRouter) cmdThread(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin thread <agent-name> [instruction]`"), nil
	}

	link, resp := r.requireSpaceLink(ctx, event)
	if resp != nil {
		return resp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	agentName := args[0]
	instruction := ""
	if len(args) > 1 {
		instruction = strings.Join(args[1:], " ")
	}

	// Create the agent.
	createResp, err := client.ProjectAgents(link.ProjectID).Create(ctx, &hubclient.CreateAgentRequest{
		Name: agentName,
	})
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to create agent: %v", err)), nil
	}
	if createResp == nil || createResp.Agent == nil {
		return textResponse(event, "Failed to create agent: received empty response from server"), nil
	}

	// Start the agent.
	if err := client.ProjectAgents(link.ProjectID).Start(ctx, createResp.Agent.Slug); err != nil {
		return textResponse(event, fmt.Sprintf("Agent `%s` created but failed to start: %v", createResp.Agent.Slug, err)), nil
	}

	// Send a kickoff message in the space to start a new thread.
	// Use a threadKey derived from the agent slug so Google Chat creates a
	// new thread and subsequent messages can target it.
	threadMsg := fmt.Sprintf("Agent `%s` created and started.", createResp.Agent.Slug)
	if instruction != "" {
		threadMsg += fmt.Sprintf("\n\n*Instruction:* %s", instruction)
	}
	threadKey := fmt.Sprintf("scion-agent-%s", createResp.Agent.Slug)
	kickoffMsgName, msgErr := r.messenger.SendMessage(ctx, SendMessageRequest{
		SpaceID:   event.SpaceID,
		ThreadKey: threadKey,
		Text:      threadMsg,
	})
	if msgErr != nil {
		r.log.Warn("failed to post thread kickoff message", "error", msgErr)
	}

	// If instruction provided, send it to the agent.
	if instruction != "" {
		senderEmail := r.senderEmailForUser(ctx, event)
		msg := messages.NewInstruction("user:"+senderEmail, createResp.Agent.Slug, instruction)
		if r.broker != nil {
			msg.Channel = r.broker.ChannelName()
		}
		// Use the newly created thread (derived from the kickoff message name)
		// so the agent replies in the new thread, not the admin command's thread.
		if msgErr == nil && kickoffMsgName != "" {
			msg.ThreadID = strings.Replace(kickoffMsgName, "/messages/", "/threads/", 1)
		} else if event.ThreadID != "" {
			msg.ThreadID = event.ThreadID
		}
		if _, err := client.ProjectAgents(link.ProjectID).SendStructuredMessage(ctx, createResp.Agent.Slug, msg, false, false, false); err != nil {
			return textResponse(event, fmt.Sprintf("Agent `%s` created and started, but failed to send instruction: %v", createResp.Agent.Slug, err)), nil
		}
	}

	card := Card{
		Header: CardHeader{
			Title:    "Thread Created",
			Subtitle: fmt.Sprintf("Agent: %s", createResp.Agent.Slug),
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetKeyValue, Label: "Agent", Content: createResp.Agent.Slug},
					{Type: WidgetKeyValue, Label: "Status", Content: "Running"},
				},
			},
		},
		Actions: []CardAction{
			{Label: "View Status", ActionID: fmt.Sprintf("agent.status.%s", createResp.Agent.ID)},
			{Label: "View Logs", ActionID: fmt.Sprintf("agent.logs.%s", createResp.Agent.ID)},
		},
	}
	if instruction != "" {
		card.Sections[0].Widgets = append(card.Sections[0].Widgets,
			Widget{Type: WidgetKeyValue, Label: "Instruction", Content: instruction})
	}

	return cardResponse(event, &card), nil
}

func (r *CommandRouter) cmdSend(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) < 2 {
		return textResponse(event, "Usage: `/scionAdmin send <agent-slug> <path-or-query>`"), nil
	}

	link, resp := r.requireSpaceLink(ctx, event)
	if resp != nil {
		return resp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	agentSlug := args[0]
	filePath := strings.Join(args[1:], " ")

	// Verify the agent exists.
	agent, err := client.ProjectAgents(link.ProjectID).Get(ctx, agentSlug)
	if err != nil {
		return textResponse(event, fmt.Sprintf("Agent `%s` not found: %v", agentSlug, err)), nil
	}

	// Phase 3: return a card with the file info.
	// Phase 5 will add actual file upload via media.upload API.
	card := Card{
		Header: CardHeader{
			Title:    "Send File",
			Subtitle: fmt.Sprintf("Agent: %s", agent.Slug),
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetKeyValue, Label: "Agent", Content: agent.Slug},
					{Type: WidgetKeyValue, Label: "File Path", Content: filePath},
					{Type: WidgetText, Content: "_File upload via Google Chat is not yet available. Use the web terminal to access files directly._"},
				},
			},
		},
		Actions: []CardAction{
			{Label: "Open Terminal", ActionID: fmt.Sprintf("link.%s/agents/%s/terminal", r.hubURL, agent.ID), Style: "primary"},
		},
	}

	return cardResponse(event, &card), nil
}

func (r *CommandRouter) cmdSecret(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin secret <list|set|get|delete> [key] [value]`"), nil
	}

	link, resp := r.requireSpaceLink(ctx, event)
	if resp != nil {
		return resp, nil
	}

	client, err := r.clientForUser(ctx, event)
	if err != nil {
		return textResponse(event, "Authentication required. Use `/scionAdmin register` first."), nil
	}

	subcmd := strings.ToLower(args[0])
	secretArgs := args[1:]

	switch subcmd {
	case "list":
		return r.cmdSecretList(ctx, event, link, client)
	case "set":
		return r.cmdSecretSet(ctx, event, link, client, secretArgs)
	case "get":
		return r.cmdSecretGet(ctx, event, link, client, secretArgs)
	case "delete":
		return r.cmdSecretDelete(ctx, event, link, client, secretArgs)
	default:
		return textResponse(event, fmt.Sprintf("Unknown secret subcommand: `%s`. Use `list`, `set`, `get`, or `delete`.", subcmd)), nil
	}
}

func (r *CommandRouter) cmdSecretList(ctx context.Context, event *ChatEvent, link *state.SpaceLink, client hubclient.Client) (*EventResponse, error) {
	secrets, err := client.Secrets().List(ctx, &hubclient.ListSecretOptions{
		Scope:   "project",
		ScopeID: link.ProjectID,
	})
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to list secrets: %v", err)), nil
	}

	if secrets == nil || len(secrets.Secrets) == 0 {
		return textResponse(event, fmt.Sprintf("No secrets found in project `%s`.", link.ProjectSlug)), nil
	}

	// Cap output to avoid oversized chat messages.
	const maxSecrets = 50
	display := secrets.Secrets
	truncated := false
	if len(display) > maxSecrets {
		display = display[:maxSecrets]
		truncated = true
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Secrets in %s* (%d):\n", link.ProjectSlug, len(secrets.Secrets)))
	for _, sec := range display {
		line := fmt.Sprintf("• `%s`", sec.Key)
		if sec.SecretType != "" {
			line += fmt.Sprintf(" (type: %s)", sec.SecretType)
		}
		if sec.Description != "" {
			line += fmt.Sprintf(" — %s", sec.Description)
		}
		sb.WriteString(line + "\n")
	}
	if truncated {
		sb.WriteString(fmt.Sprintf("\n_Showing %d of %d secrets._", maxSecrets, len(secrets.Secrets)))
	}
	return textResponse(event, sb.String()), nil
}

func (r *CommandRouter) cmdSecretSet(ctx context.Context, event *ChatEvent, link *state.SpaceLink, client hubclient.Client, args []string) (*EventResponse, error) {
	if len(args) < 1 {
		return textResponse(event, "Usage: `/scionAdmin secret set <key>`"), nil
	}

	key := args[0]
	if err := validateSecretKey(key); err != nil {
		return textResponse(event, fmt.Sprintf("Invalid key: %v", err)), nil
	}

	// Always use a card-based text input for the secret value to avoid
	// exposing values in chat history. Any extra args are ignored.
	card := Card{
		Header: CardHeader{
			Title:    "Set Secret",
			Subtitle: fmt.Sprintf("Key: %s", key),
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetInput, Label: "Secret Value", ActionID: fmt.Sprintf("secret.set.%s", key)},
				},
			},
		},
		Actions: []CardAction{
			{Label: "Save", ActionID: fmt.Sprintf("secret.set.%s", key), Style: "primary"},
		},
	}
	return cardResponse(event, &card), nil
}

func (r *CommandRouter) cmdSecretGet(ctx context.Context, event *ChatEvent, link *state.SpaceLink, client hubclient.Client, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin secret get <key>`"), nil
	}

	key := args[0]
	if err := validateSecretKey(key); err != nil {
		return textResponse(event, fmt.Sprintf("Invalid key: %v", err)), nil
	}

	secret, err := client.Secrets().Get(ctx, key, &hubclient.SecretScopeOptions{
		Scope:   "project",
		ScopeID: link.ProjectID,
	})
	if err != nil {
		return textResponse(event, fmt.Sprintf("Failed to get secret `%s`: %v", key, err)), nil
	}

	card := Card{
		Header: CardHeader{
			Title:    "Secret Details",
			Subtitle: secret.Key,
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetKeyValue, Label: "Key", Content: secret.Key},
					{Type: WidgetKeyValue, Label: "Scope", Content: secret.Scope},
					{Type: WidgetKeyValue, Label: "Version", Content: fmt.Sprintf("%d", secret.Version)},
				},
			},
		},
	}
	if secret.SecretType != "" {
		card.Sections[0].Widgets = append(card.Sections[0].Widgets,
			Widget{Type: WidgetKeyValue, Label: "Type", Content: secret.SecretType})
	}
	if secret.Description != "" {
		card.Sections[0].Widgets = append(card.Sections[0].Widgets,
			Widget{Type: WidgetKeyValue, Label: "Description", Content: secret.Description})
	}
	if !secret.Updated.IsZero() {
		card.Sections[0].Widgets = append(card.Sections[0].Widgets,
			Widget{Type: WidgetKeyValue, Label: "Last Updated", Content: secret.Updated.UTC().Format("2006-01-02 15:04:05 UTC")})
	}
	card.Sections[0].Widgets = append(card.Sections[0].Widgets,
		Widget{Type: WidgetText, Content: "_(Secret value is never shown)_"})

	return cardResponse(event, &card), nil
}

func (r *CommandRouter) cmdSecretDelete(ctx context.Context, event *ChatEvent, link *state.SpaceLink, client hubclient.Client, args []string) (*EventResponse, error) {
	if len(args) == 0 {
		return textResponse(event, "Usage: `/scionAdmin secret delete <key>`"), nil
	}

	key := args[0]
	if err := validateSecretKey(key); err != nil {
		return textResponse(event, fmt.Sprintf("Invalid key: %v", err)), nil
	}

	if err := client.Secrets().Delete(ctx, key, &hubclient.SecretScopeOptions{
		Scope:   "project",
		ScopeID: link.ProjectID,
	}); err != nil {
		return textResponse(event, fmt.Sprintf("Failed to delete secret `%s`: %v", key, err)), nil
	}

	return textResponse(event, fmt.Sprintf("Secret `%s` has been deleted.", key)), nil
}

// validateSecretKey checks that a key is non-empty and contains no spaces,
// newlines, or equals signs.
func validateSecretKey(key string) error {
	if key == "" {
		return fmt.Errorf("secret key cannot be empty")
	}
	if strings.ContainsAny(key, " \t\n\r=:") {
		return fmt.Errorf("secret key must not contain spaces, tabs, newlines, '=' or ':'")
	}
	return nil
}

// --- Settings command ---

func (r *CommandRouter) cmdSettings(ctx context.Context, event *ChatEvent, args []string) (*EventResponse, error) {
	link, resp := r.requireSpaceLink(ctx, event)
	if resp != nil {
		return resp, nil
	}

	return r.buildSettingsCard(event, link), nil
}

// buildSettingsCard creates a card showing the current observe mode and state notification settings.
func (r *CommandRouter) buildSettingsCard(event *ChatEvent, link *state.SpaceLink) *EventResponse {
	observeLabel := "Observe Mode: OFF"
	if link.ShowAgentToAgent {
		observeLabel = "Observe Mode: ON"
	}
	stateLabel := "State Notifications: OFF"
	if link.ShowStateChanges {
		stateLabel = "State Notifications: ON"
	}

	card := Card{
		Header: CardHeader{
			Title:    "Space Settings",
			Subtitle: fmt.Sprintf("Project: %s", link.ProjectSlug),
		},
		Sections: []CardSection{
			{
				Header: "Message Filtering",
				Widgets: []Widget{
					{Type: WidgetText, Content: "Control which messages are relayed to this space."},
				},
			},
		},
		Actions: []CardAction{
			{Label: observeLabel, ActionID: "settings.observe"},
			{Label: stateLabel, ActionID: "settings.statechange"},
		},
	}

	return cardResponse(event, &card)
}

// handleSettingsAction toggles observe mode or state change notification settings.
func (r *CommandRouter) handleSettingsAction(ctx context.Context, event *ChatEvent, verb string) error {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return fmt.Errorf("getting space link: %w", err)
	}
	if link == nil {
		return r.reply(ctx, event, "This space is not linked to a project.")
	}

	switch verb {
	case "observe":
		link.ShowAgentToAgent = !link.ShowAgentToAgent
		if err := r.store.SetShowAgentToAgent(event.SpaceID, event.Platform, link.ShowAgentToAgent); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to update setting: %v", err))
		}
	case "statechange":
		link.ShowStateChanges = !link.ShowStateChanges
		if err := r.store.SetShowStateChanges(event.SpaceID, event.Platform, link.ShowStateChanges); err != nil {
			return r.reply(ctx, event, fmt.Sprintf("Failed to update setting: %v", err))
		}
	default:
		return r.reply(ctx, event, fmt.Sprintf("Unknown setting: `%s`", verb))
	}

	// Re-render the settings card with updated state via UpdateMessage
	resp := r.buildSettingsCard(event, link)
	if resp.Message != nil && resp.Message.Card != nil {
		if event.ActionData != "" {
			// Use UpdateMessage to refresh the card in-place
			if err := r.messenger.UpdateMessage(ctx, event.ActionData, *resp.Message); err != nil {
				r.log.Warn("failed to update settings card, sending new message", "error", err)
				_, err = r.messenger.SendCard(ctx, event.SpaceID, *resp.Message.Card)
				return err
			}
			return nil
		}
		_, err = r.messenger.SendCard(ctx, event.SpaceID, *resp.Message.Card)
		return err
	}
	return nil
}

// --- Attachment handling ---

// downloadInboundAttachment downloads a user-uploaded file from Google Chat and
// saves it to the shared volume. Returns the agent-visible path.
func (r *CommandRouter) downloadInboundAttachment(ctx context.Context, att EventAttachment, spaceID, projectSlug, projectID string) (string, error) {
	if r.downloader == nil {
		return "", fmt.Errorf("no attachment downloader configured")
	}

	// Sanitize the conversation ID (space name) for use as a directory.
	safeConvID := sanitizePathComponent(spaceID)
	if safeConvID == "" {
		safeConvID = "unknown"
	}

	// Resolve the host-side directory.
	hostDir, err := resolveGChatAttachmentDir(projectSlug, projectID, safeConvID)
	if err != nil {
		return "", fmt.Errorf("resolving attachment directory: %w", err)
	}
	if mkErr := os.MkdirAll(hostDir, 0o755); mkErr != nil {
		return "", fmt.Errorf("creating attachment directory: %w", mkErr)
	}

	// Download the file via the authenticated downloader.
	body, err := r.downloader.DownloadAttachment(ctx, att.DownloadURI)
	if err != nil {
		return "", fmt.Errorf("downloading %q: %w", att.Name, err)
	}
	defer body.Close()

	// Build a timestamped filename.
	safeName := sanitizePathComponent(att.Name)
	if safeName == "" {
		safeName = "attachment"
	}
	destName := fmt.Sprintf("%d_%s", time.Now().UnixNano(), safeName)
	destPath := filepath.Join(hostDir, destName)

	f, fErr := os.Create(destPath)
	if fErr != nil {
		return "", fmt.Errorf("creating file: %w", fErr)
	}
	defer f.Close()

	written, copyErr := io.Copy(f, io.LimitReader(body, MaxAttachmentSize+1))
	if copyErr != nil {
		f.Close()
		os.Remove(destPath)
		return "", fmt.Errorf("writing attachment: %w", copyErr)
	}
	if written > MaxAttachmentSize {
		f.Close()
		os.Remove(destPath)
		return "", fmt.Errorf("attachment %q too large (%d bytes, max %d)", att.Name, written, MaxAttachmentSize)
	}

	agentPath := filepath.ToSlash(filepath.Join(
		"/scion-volumes/scratchpad/.attachments", gchatAttachmentDir, safeConvID, destName,
	))

	r.log.Info("downloaded Google Chat attachment",
		"filename", att.Name,
		"content_type", att.ContentType,
		"agent_path", agentPath,
	)

	return agentPath, nil
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

// updateMessageResponse creates a synchronous EventResponse that updates the
// triggering message in place (e.g. replacing a confirmation card with a result).
func updateMessageResponse(event *ChatEvent, text string) *EventResponse {
	return &EventResponse{
		UpdateMessage: &SendMessageRequest{
			SpaceID:  event.SpaceID,
			ThreadID: event.ThreadID,
			Text:     text,
		},
	}
}

// requireSpaceLink checks that the space is linked to a project, returning an error response if not.
func (r *CommandRouter) requireSpaceLink(ctx context.Context, event *ChatEvent) (*state.SpaceLink, *EventResponse) {
	link, err := r.store.GetSpaceLink(event.SpaceID, event.Platform)
	if err != nil {
		return nil, textResponse(event, fmt.Sprintf("Failed to check project link: %v", err))
	}
	if link == nil {
		return nil, textResponse(event, "This space is not linked to a project. Use `/scionAdmin link <project-slug>` first.")
	}
	return link, nil
}

// senderEmailForUser returns the Hub-registered email for the event's user.
// Falls back to the chat platform email if no identity mapper is configured
// (e.g. in tests). This keeps sender identity consistent across all commands.
func (r *CommandRouter) senderEmailForUser(ctx context.Context, event *ChatEvent) string {
	if r.idMapper != nil {
		mapping, err := r.idMapper.Resolve(event.UserID, event.Platform)
		if err == nil && mapping != nil && mapping.HubUserEmail != "" {
			return mapping.HubUserEmail
		}
	}
	if event.UserEmail != "" {
		return event.UserEmail
	}
	return "unknown"
}

// clientForUser creates a Hub client authenticated as the event's user.
func (r *CommandRouter) clientForUser(ctx context.Context, event *ChatEvent) (hubclient.Client, error) {
	if r.testClient != nil {
		return r.testClient, nil
	}
	mapping, err := r.idMapper.ResolveOrAutoRegister(ctx, &eventUserLookup{event}, event.UserID, event.Platform)
	if err != nil {
		return nil, err
	}
	if mapping == nil {
		return nil, fmt.Errorf("user not registered")
	}
	return r.idMapper.ClientFor(ctx, mapping)
}
