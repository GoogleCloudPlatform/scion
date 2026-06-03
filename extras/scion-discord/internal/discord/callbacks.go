package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// CallbackHandler processes Discord message component interactions (buttons, selects).
type CallbackHandler struct {
	store     Store
	session   *discordgo.Session
	hubClient HubClient
	log       *slog.Logger

	// deliverInbound delivers a StructuredMessage to the hub on the given topic.
	// Injected by the broker so callbacks can route responses back to agents.
	deliverInbound func(topic string, msg *messages.StructuredMessage)
}

// NewCallbackHandler creates a new CallbackHandler.
// deliverInbound is a function that posts a StructuredMessage to the hub.
func NewCallbackHandler(store Store, session *discordgo.Session, hubClient HubClient, deliverInbound func(string, *messages.StructuredMessage), log *slog.Logger) *CallbackHandler {
	if log == nil {
		log = slog.Default()
	}
	return &CallbackHandler{
		store:          store,
		session:        session,
		hubClient:      hubClient,
		deliverInbound: deliverInbound,
		log:            log,
	}
}

// Dispatch routes a component interaction based on custom_id prefix.
func (h *CallbackHandler) Dispatch(s *discordgo.Session, i *discordgo.InteractionCreate, customID string, values []string) {
	parts := strings.SplitN(customID, ":", 3)
	if len(parts) < 2 {
		h.log.Warn("Invalid callback custom_id", "custom_id", customID)
		return
	}

	switch parts[0] {
	case "setup":
		h.handleSetupCallback(s, i, parts[1:])
	case "ask":
		h.handleAskCallback(s, i, customID)
	case "notif":
		h.handleNotifCallback(s, i, customID)
	case "settings":
		h.handleSettingsCallback(s, i, customID)
	case "default":
		h.handleDefaultCallback(s, i, customID)
	default:
		h.log.Debug("Unhandled callback prefix", "prefix", parts[0], "custom_id", customID)
	}
}

// handleSetupCallback handles setup-related button callbacks.
func (h *CallbackHandler) handleSetupCallback(s *discordgo.Session, i *discordgo.InteractionCreate, parts []string) {
	if len(parts) == 0 {
		return
	}

	switch parts[0] {
	case "proj":
		if len(parts) < 2 {
			return
		}
		h.handleSetupProject(s, i, parts[1])
	case "dflt":
		if len(parts) < 2 {
			return
		}
		h.handleSetupDefaultAgent(s, i, parts[1])
	default:
		h.log.Debug("Unknown setup sub-action", "action", parts[0])
	}
}

// handleSetupProject handles project selection during /scion setup.
func (h *CallbackHandler) handleSetupProject(s *discordgo.Session, i *discordgo.InteractionCreate, projectID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Fetch agents for the selected project.
	agents, err := h.hubClient.ListAgents(ctx, projectID)
	if err != nil {
		h.log.Error("Failed to list agents for project", "project_id", projectID, "error", err)
		h.respondUpdate(s, i, "Failed to fetch agents. Please try `/scion setup` again.", nil)
		return
	}

	// Resolve project slug.
	projectSlug := projectID
	projects, projErr := h.hubClient.ListProjectsFresh(ctx)
	if projErr == nil {
		for _, p := range projects {
			if p.ID == projectID {
				projectSlug = p.DisplayName()
				break
			}
		}
	}

	// Save the link immediately with no default agent.
	h.saveChannelLink(ctx, i, projectID, projectSlug, "")

	if len(agents) == 0 {
		h.respondUpdate(s, i,
			fmt.Sprintf("Channel linked to project **%s**.", projectSlug), nil)
		return
	}

	// Build agent selection buttons for choosing a default agent.
	var rows []discordgo.MessageComponent
	var buttons []discordgo.MessageComponent
	for idx, agent := range agents {
		buttons = append(buttons, discordgo.Button{
			Label:    agent.Slug,
			Style:    discordgo.SecondaryButton,
			CustomID: fmt.Sprintf("setup:dflt:%s", agent.Slug),
		})
		if len(buttons) == 5 || idx == len(agents)-1 {
			rows = append(rows, discordgo.ActionsRow{Components: buttons})
			buttons = nil
		}
		if len(rows) >= 5 {
			break
		}
	}

	h.respondUpdate(s, i,
		fmt.Sprintf("Channel linked to project **%s**.\nChoose a default agent (receives bot @-mentions):", projectSlug),
		rows,
	)
}

// handleSetupDefaultAgent handles default agent selection during /scion setup.
// The channel link was already saved by handleSetupProject; this updates
// the default agent.
func (h *CallbackHandler) handleSetupDefaultAgent(s *discordgo.Session, i *discordgo.InteractionCreate, agentSlug string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, _ := h.store.GetChannelLink(ctx, i.ChannelID)
	if link == nil {
		h.respondUpdate(s, i, "Setup session expired. Please use `/scion setup` again.", nil)
		return
	}

	link.DefaultAgent = agentSlug
	if err := h.store.UpdateChannelLink(ctx, link); err != nil {
		h.log.Error("Failed to update default agent", "error", err, "channel_id", i.ChannelID)
		h.respondUpdate(s, i, "Failed to save default agent. Please try again.", nil)
		return
	}

	h.respondUpdate(s, i,
		fmt.Sprintf("Channel linked to project **%s**.\nDefault agent: **%s**", link.ProjectSlug, agentSlug),
		nil,
	)
	h.log.Info("Default agent set during setup",
		"channel_id", i.ChannelID,
		"project_id", link.ProjectID,
		"default_agent", agentSlug,
	)
}

// saveChannelLink persists a channel-to-project link.
func (h *CallbackHandler) saveChannelLink(ctx context.Context, i *discordgo.InteractionCreate, projectID, projectSlug, agentSlug string) {
	linkedBy := interactionUserID(i)
	guildID := i.GuildID

	link := &ChannelLink{
		ChannelID:          i.ChannelID,
		GuildID:            guildID,
		ProjectID:          projectID,
		ProjectSlug:        projectSlug,
		DefaultAgent:       agentSlug,
		LinkedBy:           linkedBy,
		LinkedAt:           time.Now(),
		Active:             true,
		ShowAssistantReply: true,
		ShowStateChanges:   true,
		NotifyInGroup:      true,
	}

	if err := h.store.CreateChannelLink(ctx, link); err != nil {
		h.log.Error("Failed to save channel link", "error", err, "channel_id", i.ChannelID)
	} else {
		h.log.Info("Channel link saved",
			"channel_id", i.ChannelID,
			"guild_id", guildID,
			"project_id", projectID,
		)
	}
}

// respondUpdate edits the deferred interaction response to update the message.
// This is used after the broker has already acknowledged with
// InteractionResponseDeferredMessageUpdate.
func (h *CallbackHandler) respondUpdate(s *discordgo.Session, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent) {
	edit := &discordgo.WebhookEdit{
		Content: &content,
	}
	if components != nil {
		edit.Components = &components
	} else {
		empty := []discordgo.MessageComponent{}
		edit.Components = &empty
	}
	_, err := s.InteractionResponseEdit(i.Interaction, edit)
	if err != nil {
		h.log.Error("Failed to edit interaction response", "error", err)
	}
}

// --- Ask-user callback handlers ---

// handleAskCallback routes ask-user component interactions.
// custom_id formats:
//   - ask:opt:<requestID>:<index>  — user picked a choice button
//   - ask:reply:<requestID>        — user clicked "Reply" (opens modal; NOT pre-acknowledged)
//   - ask:dismiss:<requestID>      — user clicked "Dismiss"
func (h *CallbackHandler) handleAskCallback(s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	// Parse: "ask:<action>:<requestID>[:<extra>]"
	parts := strings.SplitN(customID, ":", 4)
	if len(parts) < 3 {
		h.log.Warn("Malformed ask callback custom_id", "custom_id", customID)
		return
	}
	action := parts[1]
	requestID := parts[2]

	switch action {
	case "opt":
		// ask:opt:<requestID>:<index>
		if len(parts) < 4 {
			h.log.Warn("Missing index in ask:opt callback", "custom_id", customID)
			return
		}
		idx, err := strconv.Atoi(parts[3])
		if err != nil {
			h.log.Warn("Invalid index in ask:opt callback", "custom_id", customID, "error", err)
			return
		}
		h.handleAskOption(s, i, requestID, idx)

	case "reply":
		// ask:reply:<requestID> — open a modal for free-text response.
		// NOTE: The broker must NOT pre-acknowledge this interaction with
		// InteractionResponseDeferredMessageUpdate, because we need to
		// respond with InteractionResponseModal instead.
		h.handleAskReply(s, i, requestID)

	case "dismiss":
		// ask:dismiss:<requestID>
		h.handleAskDismiss(s, i, requestID)

	default:
		h.log.Debug("Unknown ask sub-action", "action", action, "custom_id", customID)
	}
}

// handleAskOption handles a choice button click for an ask-user request.
func (h *CallbackHandler) handleAskOption(s *discordgo.Session, i *discordgo.InteractionCreate, requestID string, index int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pending, err := h.store.GetPendingAskUser(ctx, requestID)
	if err != nil {
		h.log.Error("Failed to get pending ask-user", "request_id", requestID, "error", err)
		h.respondUpdate(s, i, "Error looking up request. Please try again.", nil)
		return
	}
	if pending == nil {
		h.respondUpdate(s, i, "This request has expired or was not found.", nil)
		return
	}
	if pending.Responded {
		h.respondUpdate(s, i, "This request has already been answered.", nil)
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		h.respondUpdate(s, i, "This request has expired.", nil)
		return
	}
	if index < 0 || index >= len(pending.Choices) {
		h.log.Warn("Choice index out of range", "request_id", requestID, "index", index, "choices", len(pending.Choices))
		h.respondUpdate(s, i, "Invalid choice.", nil)
		return
	}

	choice := pending.Choices[index]

	// Deliver the response to the hub.
	h.deliverAskUserResponse(ctx, i, pending, choice)

	// Mark as responded.
	if err := h.store.MarkAskUserResponded(ctx, requestID); err != nil {
		h.log.Error("Failed to mark ask-user as responded", "request_id", requestID, "error", err)
	}

	// Update the original message to show the selection and disable buttons.
	h.respondUpdate(s, i, fmt.Sprintf("✅ Responded: **%s**", choice), nil)

	h.log.Info("Ask-user option selected",
		"request_id", requestID,
		"choice", choice,
		"user", interactionUserID(i),
	)
}

// handleAskReply opens a modal for free-text response to an ask-user request.
func (h *CallbackHandler) handleAskReply(s *discordgo.Session, i *discordgo.InteractionCreate, requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pending, err := h.store.GetPendingAskUser(ctx, requestID)
	if err != nil || pending == nil {
		// Can't open a modal after a deferred update. Since this interaction
		// was NOT pre-acknowledged, respond with a simple message.
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "This request has expired or was not found.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	if pending.Responded {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "This request has already been answered.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "This request has expired.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Open the modal. The prompt is included in the modal for context.
	OpenAskUserModal(s, i, requestID, "")
}

// handleAskDismiss handles the "Dismiss" button for an ask-user request.
func (h *CallbackHandler) handleAskDismiss(s *discordgo.Session, i *discordgo.InteractionCreate, requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pending, err := h.store.GetPendingAskUser(ctx, requestID)
	if err != nil {
		h.log.Error("Failed to get pending ask-user for dismiss", "request_id", requestID, "error", err)
		h.respondUpdate(s, i, "Error looking up request.", nil)
		return
	}
	if pending == nil {
		h.respondUpdate(s, i, "This request has expired or was not found.", nil)
		return
	}
	if pending.Responded {
		h.respondUpdate(s, i, "This request has already been answered.", nil)
		return
	}

	// Mark as responded (dismissed).
	if err := h.store.MarkAskUserResponded(ctx, requestID); err != nil {
		h.log.Error("Failed to mark ask-user as dismissed", "request_id", requestID, "error", err)
	}

	// Update the original message to show dismissal and remove buttons.
	h.respondUpdate(s, i, "Dismissed.", nil)

	h.log.Info("Ask-user dismissed",
		"request_id", requestID,
		"user", interactionUserID(i),
	)
}

// deliverAskUserResponse builds a StructuredMessage from the user's response
// and delivers it to the hub, targeting the agent that asked.
func (h *CallbackHandler) deliverAskUserResponse(ctx context.Context, i *discordgo.InteractionCreate, pending *PendingAskUser, responseText string) {
	if h.deliverInbound == nil {
		h.log.Error("deliverInbound not configured, cannot deliver ask-user response")
		return
	}

	// Resolve the sender identity from Discord user → Scion identity.
	discordUserID := interactionUserID(i)
	sender := "discord:" + discordUserID
	if mapping, err := h.store.GetUserMapping(ctx, discordUserID); err == nil && mapping != nil && mapping.ScionEmail != "" {
		sender = "user:" + mapping.ScionEmail
	}

	topic := fmt.Sprintf("scion.project.%s.agent.%s.messages", pending.ProjectID, pending.AgentSlug)
	recipient := "agent:" + pending.AgentSlug

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Channel:   "discord",
		ThreadID:  pending.ChannelID,
		Sender:    sender,
		SenderID:  discordUserID,
		Recipient: recipient,
		Msg:       responseText,
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"discord_channel_id": pending.ChannelID,
			"project_id":        pending.ProjectID,
			"ask_request_id":    pending.RequestID,
		},
	}

	h.deliverInbound(topic, msg)
}

// --- Settings callback handlers ---

// handleSettingsCallback toggles channel settings.
// custom_id formats:
//   - settings:observe:<channelID>      — toggle observe mode
//   - settings:statechange:<channelID>  — toggle state change notifications
func (h *CallbackHandler) handleSettingsCallback(s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	parts := strings.SplitN(customID, ":", 3)
	if len(parts) < 3 {
		h.log.Warn("Malformed settings callback custom_id", "custom_id", customID)
		return
	}
	action := parts[1]
	channelID := parts[2]

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetChannelLink(ctx, channelID)
	if err != nil || link == nil {
		h.respondUpdate(s, i, "This channel is no longer linked to a project.", nil)
		return
	}

	switch action {
	case "observe":
		link.ShowAgentToAgent = !link.ShowAgentToAgent
	case "statechange":
		link.ShowStateChanges = !link.ShowStateChanges
	default:
		h.log.Debug("Unknown settings action", "action", action)
		return
	}

	if err := h.store.UpdateChannelLink(ctx, link); err != nil {
		h.log.Error("Failed to update channel settings", "error", err, "channel_id", channelID)
		h.respondUpdate(s, i, "Failed to update settings. Please try again.", nil)
		return
	}

	content, components := settingsPanel(link)
	h.respondUpdate(s, i, content, components)

	h.log.Info("Channel settings updated",
		"channel_id", channelID,
		"action", action,
		"observe_mode", link.ShowAgentToAgent,
		"state_changes", link.ShowStateChanges,
	)
}

// --- Default agent callback handlers ---

// handleDefaultCallback handles default agent selection buttons.
// custom_id formats:
//   - default:set:<agentSlug>  — set agent as default
//   - default:none             — clear default agent
func (h *CallbackHandler) handleDefaultCallback(s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	parts := strings.SplitN(customID, ":", 3)
	if len(parts) < 2 {
		h.log.Warn("Malformed default callback custom_id", "custom_id", customID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetChannelLink(ctx, i.ChannelID)
	if err != nil || link == nil {
		h.respondUpdate(s, i, "This channel is not linked to a project.", nil)
		return
	}

	action := parts[1]
	switch action {
	case "none":
		link.DefaultAgent = ""
		if err := h.store.UpdateChannelLink(ctx, link); err != nil {
			h.log.Error("Failed to clear default agent", "error", err)
			h.respondUpdate(s, i, "Failed to clear default agent. Please try again.", nil)
			return
		}
		h.respondUpdate(s, i, "Default agent cleared for this channel.", nil)
		h.log.Info("Default agent cleared via button", "channel_id", i.ChannelID)

	case "set":
		if len(parts) < 3 {
			h.log.Warn("Missing agent slug in default:set callback", "custom_id", customID)
			return
		}
		agentSlug := parts[2]
		link.DefaultAgent = agentSlug
		if err := h.store.UpdateChannelLink(ctx, link); err != nil {
			h.log.Error("Failed to set default agent", "error", err)
			h.respondUpdate(s, i, "Failed to set default agent. Please try again.", nil)
			return
		}
		h.respondUpdate(s, i, fmt.Sprintf("Default agent set to **%s** for this channel.", agentSlug), nil)
		h.log.Info("Default agent set via button", "channel_id", i.ChannelID, "agent", agentSlug)

	default:
		h.log.Debug("Unknown default action", "action", action, "custom_id", customID)
	}
}

// --- Notification callback handlers ---

// handleNotifCallback toggles notification preferences.
// custom_id formats:
//   - notif:on:<agentSlug>   — enable notifications for agent
//   - notif:off:<agentSlug>  — disable notifications for agent
func (h *CallbackHandler) handleNotifCallback(s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	parts := strings.SplitN(customID, ":", 3)
	if len(parts) < 3 {
		h.log.Warn("Malformed notif callback custom_id", "custom_id", customID)
		return
	}
	action := parts[1]
	agentSlug := parts[2]

	enabled := action == "on"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	discordUserID := interactionUserID(i)

	// Look up the channel link to determine the project.
	link, err := h.store.GetChannelLink(ctx, i.ChannelID)
	if err != nil || link == nil {
		h.respondUpdate(s, i, "This channel is not linked to a project.", nil)
		return
	}

	pref := &NotificationPref{
		DiscordUserID: discordUserID,
		ProjectID:     link.ProjectID,
		AgentSlug:     agentSlug,
		Enabled:       enabled,
		UpdatedAt:     time.Now(),
	}

	if err := h.store.SetNotificationPref(ctx, pref); err != nil {
		h.log.Error("Failed to save notification pref", "error", err)
		h.respondUpdate(s, i, "Failed to update notification preference.", nil)
		return
	}

	stateText := "enabled"
	if !enabled {
		stateText = "disabled"
	}
	h.respondUpdate(s, i,
		fmt.Sprintf("Notifications for **%s**: %s", agentSlug, stateText),
		nil,
	)

	h.log.Info("Notification preference updated",
		"user", discordUserID,
		"agent", agentSlug,
		"enabled", enabled,
	)
}
