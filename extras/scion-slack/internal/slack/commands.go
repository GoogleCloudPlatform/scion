package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

// HandleCommand dispatches a Slack slash command to the appropriate handler.
func HandleCommand(
	ctx context.Context,
	client *slackapi.Client,
	store Store,
	hubClient HubClient,
	registration *RegistrationHandler,
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError,
	cmd slackapi.SlashCommand,
	log *slog.Logger,
) {
	if log == nil {
		log = slog.Default()
	}

	parts := strings.Fields(cmd.Text)
	subcommand := ""
	if len(parts) > 0 {
		subcommand = strings.ToLower(parts[0])
	}

	switch subcommand {
	case "setup":
		handleSetup(ctx, client, store, hubClient, cmd, log)
	case "unlink":
		handleUnlink(ctx, client, store, cmd, log)
	case "agents":
		handleAgents(ctx, client, store, hubClient, cmd, log)
	case "status":
		handleStatus(ctx, client, store, hubClient, cmd, parts[1:], log)
	case "msg":
		handleMsg(ctx, client, store, hubClient, deliverInbound, cmd, parts[1:], log)
	case "default":
		handleDefault(ctx, client, store, hubClient, cmd, parts[1:], log)
	case "register":
		handleRegister(ctx, client, store, registration, cmd, log)
	case "unregister":
		handleUnregister(ctx, client, store, cmd, log)
	case "info":
		handleInfo(ctx, client, store, cmd, log)
	case "settings":
		handleSettings(ctx, client, store, cmd, log)
	case "help", "":
		postEphemeral(client, cmd.ChannelID, cmd.UserID, helpText())
	default:
		postEphemeral(client, cmd.ChannelID, cmd.UserID,
			fmt.Sprintf("Unknown subcommand: `%s`. Use `/scion help` for available commands.", subcommand))
	}
}

func helpText() string {
	return "*Scion Bot Commands*\n\n" +
		"`/scion setup` — Link this channel to a Scion project\n" +
		"`/scion unlink` — Unlink this channel from its project\n" +
		"`/scion agents` — List agents in the linked project\n" +
		"`/scion status <agent>` — Show agent status\n" +
		"`/scion msg <agent> <text>` — Send a message to an agent\n" +
		"`/scion default [agent]` — Set or show the default agent\n" +
		"`/scion register` — Link your Slack account to Scion Hub\n" +
		"`/scion unregister` — Unlink your Slack account\n" +
		"`/scion settings` — Configure channel notification settings\n" +
		"`/scion info` — Show your registration info\n" +
		"`/scion help` — Show this help message\n\n" +
		"Mention the bot in a linked channel to send messages to agents."
}

func handleSetup(ctx context.Context, client *slackapi.Client, store Store, hubClient HubClient, cmd slackapi.SlashCommand, log *slog.Logger) {
	mapping, err := store.GetUserMapping(ctx, cmd.UserID)
	if err != nil {
		log.Error("Failed to check user mapping", "error", err, "user_id", cmd.UserID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Something went wrong. Please try again.")
		return
	}
	if mapping == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Please link your Slack account first with `/scion register`.")
		return
	}

	link, err := store.GetChannelLink(ctx, cmd.ChannelID)
	if err != nil {
		log.Error("Failed to check channel link", "error", err, "channel_id", cmd.ChannelID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Something went wrong. Please try again.")
		return
	}
	if link != nil && link.Active {
		postEphemeral(client, cmd.ChannelID, cmd.UserID,
			fmt.Sprintf("This channel is already linked to project *%s*.\nUse `/scion unlink` first to change it.", link.ProjectSlug))
		return
	}

	var projects []ProjectOption
	if mapping.ScionUserID != "" {
		projects, err = hubClient.ListProjectsForUser(ctx, mapping.ScionUserID)
		if err != nil {
			log.Warn("Failed to list user projects", "error", err)
		}
	}
	if len(projects) == 0 {
		projects, err = hubClient.ListProjectsFresh(ctx)
		if err != nil {
			log.Warn("Failed to list projects from hub", "error", err)
		}
	}
	if len(projects) == 0 {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "No projects found. Create a project in the hub first.")
		return
	}

	var buttons []slackapi.BlockElement
	for _, proj := range projects {
		buttons = append(buttons, slackapi.NewButtonBlockElement(
			fmt.Sprintf("setup:proj:%s", proj.ID),
			proj.ID,
			slackapi.NewTextBlockObject("plain_text", proj.DisplayName(), false, false),
		))
		if len(buttons) >= 10 {
			break
		}
	}

	var blocks []slackapi.Block
	blocks = append(blocks, slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject("mrkdwn", "Select a project to link this channel to:", false, false),
		nil, nil,
	))

	for i := 0; i < len(buttons); i += 5 {
		end := i + 5
		if end > len(buttons) {
			end = len(buttons)
		}
		blocks = append(blocks, slackapi.NewActionBlock("", buttons[i:end]...))
	}

	client.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slackapi.MsgOptionBlocks(blocks...))
}

func handleUnlink(ctx context.Context, client *slackapi.Client, store Store, cmd slackapi.SlashCommand, log *slog.Logger) {
	link, err := store.GetChannelLink(ctx, cmd.ChannelID)
	if err != nil {
		log.Error("Failed to check channel link", "error", err)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "This channel is not linked to a project.")
		return
	}

	if err := store.DeleteChannelLink(ctx, cmd.ChannelID); err != nil {
		log.Error("Failed to delete channel link", "error", err, "channel_id", cmd.ChannelID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Failed to unlink. Please try again.")
		return
	}

	postEphemeral(client, cmd.ChannelID, cmd.UserID,
		fmt.Sprintf("Channel unlinked from project *%s*.", link.ProjectSlug))
	log.Info("Channel unlinked", "channel_id", cmd.ChannelID, "project", link.ProjectSlug)
}

func handleAgents(ctx context.Context, client *slackapi.Client, store Store, hubClient HubClient, cmd slackapi.SlashCommand, log *slog.Logger) {
	link, err := store.GetChannelLink(ctx, cmd.ChannelID)
	if err != nil || link == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	agents, err := hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		log.Error("Failed to list agents", "error", err, "project_id", link.ProjectID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Failed to fetch agents. Please try again later.")
		return
	}

	if len(agents) == 0 {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "No agents found for this project.")
		return
	}

	var lines []string
	for _, agent := range agents {
		label := agent.Slug
		if agent.Activity != "" {
			label += " — " + agent.Activity
		}
		if agent.Slug == link.DefaultAgent {
			label += " (default)"
		}
		lines = append(lines, "• "+label)
	}

	postEphemeral(client, cmd.ChannelID, cmd.UserID,
		fmt.Sprintf("*Agents in %s:*\n%s", link.ProjectSlug, strings.Join(lines, "\n")))
}

func handleStatus(ctx context.Context, client *slackapi.Client, store Store, hubClient HubClient, cmd slackapi.SlashCommand, args []string, log *slog.Logger) {
	if len(args) == 0 {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Usage: `/scion status <agent>`")
		return
	}
	agentSlug := args[0]

	link, err := store.GetChannelLink(ctx, cmd.ChannelID)
	if err != nil || link == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	agents, err := hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Failed to fetch agent status. Please try again.")
		return
	}

	for _, agent := range agents {
		if agent.Slug == agentSlug {
			activity := agent.Activity
			if activity == "" {
				activity = "unknown"
			}
			postEphemeral(client, cmd.ChannelID, cmd.UserID,
				fmt.Sprintf("*%s* — %s", agent.Slug, activity))
			return
		}
	}

	postEphemeral(client, cmd.ChannelID, cmd.UserID,
		fmt.Sprintf("Agent *%s* not found in this project.", agentSlug))
}

func handleMsg(
	ctx context.Context,
	client *slackapi.Client,
	store Store,
	hubClient HubClient,
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError,
	cmd slackapi.SlashCommand,
	args []string,
	log *slog.Logger,
) {
	if len(args) < 2 {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Usage: `/scion msg <agent> <message>`")
		return
	}
	agentSlug := args[0]
	text := strings.Join(args[1:], " ")

	link, err := store.GetChannelLink(ctx, cmd.ChannelID)
	if err != nil || link == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	mapping, err := store.GetUserMapping(ctx, cmd.UserID)
	if err != nil {
		log.Error("Failed to get user mapping", "error", err, "user_id", cmd.UserID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "An internal error occurred. Please try again later.")
		return
	}
	if mapping == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Please link your Slack account first with `/scion register`.")
		return
	}

	sender := "user:" + mapping.ScionEmail
	if mapping.ScionEmail == "" {
		sender = "slack:" + mapping.SlackUsername
	}

	cc := &ConversationContext{
		SlackUserID:   cmd.UserID,
		ProjectID:     link.ProjectID,
		AgentSlug:     agentSlug,
		LastChannelID: cmd.ChannelID,
		LastMessageAt: time.Now(),
	}
	if err := store.SetConversationContext(ctx, cc); err != nil {
		log.Warn("Failed to save conversation context", "error", err)
	}

	topic := projectcompat.AgentTopic(link.ProjectID, agentSlug)
	hubMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Channel:   "slack",
		ThreadID:  cmd.ChannelID,
		Sender:    sender,
		SenderID:  cmd.UserID,
		Recipient: "agent:" + agentSlug,
		Msg:       text,
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"slack_channel_id": cmd.ChannelID,
			"project_id":       link.ProjectID,
		},
	}

	if deliverInbound == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Message delivery is not configured.")
		return
	}

	if he := deliverInbound(topic, hubMsg); he != nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, he.userFacingMessage())
		return
	}

	postEphemeral(client, cmd.ChannelID, cmd.UserID,
		fmt.Sprintf("Message sent to *%s*: %s", agentSlug, text))
	log.Info("Slash command message delivered", "agent", agentSlug, "sender", sender, "channel_id", cmd.ChannelID)
}

func handleDefault(ctx context.Context, client *slackapi.Client, store Store, hubClient HubClient, cmd slackapi.SlashCommand, args []string, log *slog.Logger) {
	link, err := store.GetChannelLink(ctx, cmd.ChannelID)
	if err != nil {
		log.Error("Failed to get channel link", "error", err, "channel_id", cmd.ChannelID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "An internal error occurred. Please try again later.")
		return
	}
	if link == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	if len(args) == 0 {
		if link.DefaultAgent == "" {
			postEphemeral(client, cmd.ChannelID, cmd.UserID, "No default agent set. Use `/scion default <agent>` to set one.")
		} else {
			postEphemeral(client, cmd.ChannelID, cmd.UserID,
				fmt.Sprintf("Default agent: *%s*", link.DefaultAgent))
		}
		return
	}

	agentSlug := args[0]
	if agentSlug == "none" || agentSlug == "clear" {
		link.DefaultAgent = ""
	} else {
		link.DefaultAgent = agentSlug
	}

	if err := store.UpdateChannelLink(ctx, link); err != nil {
		log.Error("Failed to update default agent", "error", err)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Failed to update default agent. Please try again.")
		return
	}

	if link.DefaultAgent == "" {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Default agent cleared for this channel.")
	} else {
		postEphemeral(client, cmd.ChannelID, cmd.UserID,
			fmt.Sprintf("Default agent set to *%s*.", link.DefaultAgent))
	}
	log.Info("Default agent updated", "channel_id", cmd.ChannelID, "default_agent", link.DefaultAgent)
}

func handleRegister(ctx context.Context, client *slackapi.Client, store Store, registration *RegistrationHandler, cmd slackapi.SlashCommand, log *slog.Logger) {
	if registration == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID,
			"Registration is not configured. Please contact an administrator.")
		return
	}

	hubLink, err := registration.StartRegistration(ctx, cmd.UserID, cmd.UserName, cmd.ChannelID)
	if err != nil {
		if strings.Contains(err.Error(), "already registered") {
			postEphemeral(client, cmd.ChannelID, cmd.UserID,
				fmt.Sprintf("You are %s. Use `/scion unregister` first to re-link.", err.Error()))
			return
		}
		log.Error("Registration failed", "error", err, "user_id", cmd.UserID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Registration failed. Please try again later.")
		return
	}

	postEphemeral(client, cmd.ChannelID, cmd.UserID,
		fmt.Sprintf("To link your Slack account, open this link and sign in:\n<%s>\n\nThe link expires in 15 minutes.", hubLink))
}

func handleUnregister(ctx context.Context, client *slackapi.Client, store Store, cmd slackapi.SlashCommand, log *slog.Logger) {
	existing, err := store.GetUserMapping(ctx, cmd.UserID)
	if err != nil {
		log.Error("Failed to check user mapping", "error", err, "user_id", cmd.UserID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Something went wrong. Please try again.")
		return
	}
	if existing == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "You don't have a linked Scion account.")
		return
	}

	if err := store.DeleteUserMapping(ctx, cmd.UserID); err != nil {
		log.Error("Failed to delete user mapping", "error", err, "user_id", cmd.UserID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Failed to unlink your account. Please try again.")
		return
	}

	postEphemeral(client, cmd.ChannelID, cmd.UserID, "Your Slack account has been unlinked from Scion.")
	log.Info("User unregistered", "user_id", cmd.UserID, "scion_email", existing.ScionEmail)
}

func handleInfo(ctx context.Context, client *slackapi.Client, store Store, cmd slackapi.SlashCommand, log *slog.Logger) {
	mapping, err := store.GetUserMapping(ctx, cmd.UserID)
	if err != nil {
		log.Error("Failed to check user mapping", "error", err)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "Something went wrong. Please try again.")
		return
	}

	var sb strings.Builder
	if mapping == nil {
		sb.WriteString("*Registration:* Not registered\n")
		sb.WriteString("Use `/scion register` to link your Slack account.")
	} else {
		sb.WriteString("*Registration:* Linked\n")
		if mapping.ScionEmail != "" {
			fmt.Fprintf(&sb, "*Email:* %s\n", mapping.ScionEmail)
		}
		if mapping.ScionUserID != "" {
			fmt.Fprintf(&sb, "*User ID:* %s\n", mapping.ScionUserID)
		}
		fmt.Fprintf(&sb, "*Linked at:* %s\n", mapping.LinkedAt.UTC().Format(time.RFC3339))
	}

	link, err := store.GetChannelLink(ctx, cmd.ChannelID)
	if err == nil && link != nil {
		fmt.Fprintf(&sb, "\n*Channel project:* %s", link.ProjectSlug)
		if link.DefaultAgent != "" {
			fmt.Fprintf(&sb, "\n*Default agent:* %s", link.DefaultAgent)
		}
	}

	postEphemeral(client, cmd.ChannelID, cmd.UserID, sb.String())
}

func handleSettings(ctx context.Context, client *slackapi.Client, store Store, cmd slackapi.SlashCommand, log *slog.Logger) {
	link, err := store.GetChannelLink(ctx, cmd.ChannelID)
	if err != nil {
		log.Error("Failed to get channel link", "error", err, "channel_id", cmd.ChannelID)
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "An internal error occurred. Please try again later.")
		return
	}
	if link == nil {
		postEphemeral(client, cmd.ChannelID, cmd.UserID, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	observeLabel := "Observe Mode: OFF"
	if link.ShowAgentToAgent {
		observeLabel = "Observe Mode: ON"
	}

	stateLabel := "State Notifications: OFF"
	if link.ShowStateChanges {
		stateLabel = "State Notifications: ON"
	}

	var blocks []slackapi.Block
	blocks = append(blocks, slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject("mrkdwn",
			fmt.Sprintf("*Channel Settings* — %s\n\n"+
				"*Observe Mode* — Show agent-to-agent messages\n"+
				"*State Notifications* — Show agent state changes",
				link.ProjectSlug), false, false),
		nil, nil,
	))

	blocks = append(blocks, slackapi.NewActionBlock("",
		slackapi.NewButtonBlockElement(
			fmt.Sprintf("settings:observe:%s", cmd.ChannelID),
			"observe",
			slackapi.NewTextBlockObject("plain_text", observeLabel, false, false),
		),
		slackapi.NewButtonBlockElement(
			fmt.Sprintf("settings:statechange:%s", cmd.ChannelID),
			"statechange",
			slackapi.NewTextBlockObject("plain_text", stateLabel, false, false),
		),
	))

	client.PostEphemeral(cmd.ChannelID, cmd.UserID, slackapi.MsgOptionBlocks(blocks...))
}

// HandleBlockAction dispatches block action callbacks from interactive messages.
func HandleBlockAction(
	ctx context.Context,
	client *slackapi.Client,
	store Store,
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError,
	callback slackapi.InteractionCallback,
	action *slackapi.BlockAction,
	log *slog.Logger,
) {
	if log == nil {
		log = slog.Default()
	}

	actionID := action.ActionID
	parts := strings.SplitN(actionID, ":", 3)
	if len(parts) < 2 {
		log.Debug("Ignoring unknown action", "action_id", actionID)
		return
	}

	switch parts[0] {
	case "setup":
		handleSetupCallback(ctx, client, store, callback, parts[1:], log)
	case "ask":
		handleAskCallback(ctx, client, store, deliverInbound, callback, actionID, log)
	case "settings":
		handleSettingsCallback(ctx, client, store, callback, actionID, log)
	case "default":
		handleDefaultCallback(ctx, client, store, callback, actionID, log)
	default:
		log.Debug("Unhandled block action prefix", "prefix", parts[0])
	}
}

// HandleViewSubmission processes Slack view (modal) submissions.
func HandleViewSubmission(
	ctx context.Context,
	client *slackapi.Client,
	store Store,
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError,
	callback slackapi.InteractionCallback,
	log *slog.Logger,
) {
	if log == nil {
		log = slog.Default()
	}

	callbackID := callback.View.CallbackID
	if strings.HasPrefix(callbackID, "ask:modal:") {
		HandleAskModalSubmit(ctx, client, store, deliverInbound, callback, log)
	}
}

func handleSetupCallback(ctx context.Context, client *slackapi.Client, store Store, callback slackapi.InteractionCallback, parts []string, log *slog.Logger) {
	if len(parts) == 0 {
		return
	}

	channelID := callback.Channel.ID
	userID := callback.User.ID

	switch parts[0] {
	case "proj":
		if len(parts) < 2 {
			return
		}
		projectID := parts[1]

		link := &ChannelLink{
			ChannelID:        channelID,
			TeamID:           callback.Team.ID,
			ProjectID:        projectID,
			ProjectSlug:      projectID,
			LinkedBy:         userID,
			LinkedAt:         time.Now(),
			Active:           true,
			ShowStateChanges: true,
		}

		if err := store.CreateChannelLink(ctx, link); err != nil {
			log.Error("Failed to save channel link", "error", err, "channel_id", channelID)
			postEphemeral(client, channelID, userID, "Failed to link channel. Please try again.")
			return
		}

		postEphemeral(client, channelID, userID,
			fmt.Sprintf("Channel linked to project *%s*. Use `/scion default <agent>` to set a default agent.", projectID))
		log.Info("Channel linked", "channel_id", channelID, "project_id", projectID)
	}
}

func handleAskCallback(
	ctx context.Context,
	client *slackapi.Client,
	store Store,
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError,
	callback slackapi.InteractionCallback,
	actionID string,
	log *slog.Logger,
) {
	parts := strings.SplitN(actionID, ":", 3)
	if len(parts) < 3 {
		log.Warn("Malformed ask callback action_id", "action_id", actionID)
		return
	}
	action := parts[1]
	requestID := parts[2]

	switch action {
	case "reply":
		pending, err := store.GetPendingAskUser(ctx, requestID)
		if err != nil || pending == nil {
			log.Warn("Ask-user request not found", "request_id", requestID)
			return
		}
		if pending.Responded {
			return
		}
		OpenAskUserModal(client, callback.TriggerID, requestID, "")

	case "dismiss":
		if err := store.MarkAskUserResponded(ctx, requestID); err != nil {
			log.Error("Failed to mark ask-user as dismissed", "request_id", requestID, "error", err)
		}
		log.Info("Ask-user dismissed", "request_id", requestID, "user", callback.User.ID)

	default:
		if strings.HasPrefix(action, "opt:") {
			// Choice option: ask:opt:<requestID> with action value
			handleAskOption(ctx, client, store, deliverInbound, callback, requestID, log)
		}
	}
}

func handleAskOption(
	ctx context.Context,
	client *slackapi.Client,
	store Store,
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError,
	callback slackapi.InteractionCallback,
	requestID string,
	log *slog.Logger,
) {
	pending, err := store.GetPendingAskUser(ctx, requestID)
	if err != nil || pending == nil {
		log.Warn("Ask-user request not found for option", "request_id", requestID)
		return
	}
	if pending.Responded {
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		return
	}

	action := callback.ActionCallback.BlockActions[0]
	choice := action.Value

	if deliverInbound != nil {
		sender := "slack:" + callback.User.ID
		mapping, _ := store.GetUserMapping(ctx, callback.User.ID)
		if mapping != nil && mapping.ScionEmail != "" {
			sender = "user:" + mapping.ScionEmail
		}

		topic := projectcompat.AgentTopic(pending.ProjectID, pending.AgentSlug)
		msg := &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			ThreadID:  pending.ChannelID,
			Sender:    sender,
			SenderID:  callback.User.ID,
			Recipient: "agent:" + pending.AgentSlug,
			Msg:       choice,
			Type:      messages.TypeInstruction,
			Metadata: map[string]string{
				"slack_channel_id": pending.ChannelID,
				"project_id":       pending.ProjectID,
				"ask_request_id":   pending.RequestID,
			},
		}

		if he := deliverInbound(topic, msg); he != nil {
			log.Error("Failed to deliver ask-user option response", "request_id", requestID, "error", he)
			return
		}
	}

	if err := store.MarkAskUserResponded(ctx, requestID); err != nil {
		log.Error("Failed to mark ask-user as responded", "request_id", requestID, "error", err)
	}

	log.Info("Ask-user option selected", "request_id", requestID, "choice", choice, "user", callback.User.ID)
}

func handleSettingsCallback(ctx context.Context, client *slackapi.Client, store Store, callback slackapi.InteractionCallback, actionID string, log *slog.Logger) {
	parts := strings.SplitN(actionID, ":", 3)
	if len(parts) < 3 {
		return
	}
	action := parts[1]
	channelID := parts[2]

	link, err := store.GetChannelLink(ctx, channelID)
	if err != nil || link == nil {
		return
	}

	switch action {
	case "observe":
		link.ShowAgentToAgent = !link.ShowAgentToAgent
	case "statechange":
		link.ShowStateChanges = !link.ShowStateChanges
	default:
		return
	}

	if err := store.UpdateChannelLink(ctx, link); err != nil {
		log.Error("Failed to update channel settings", "error", err)
		return
	}

	log.Info("Channel settings updated",
		"channel_id", channelID,
		"action", action,
		"observe_mode", link.ShowAgentToAgent,
		"state_changes", link.ShowStateChanges,
	)
}

func handleDefaultCallback(ctx context.Context, client *slackapi.Client, store Store, callback slackapi.InteractionCallback, actionID string, log *slog.Logger) {
	parts := strings.SplitN(actionID, ":", 3)
	if len(parts) < 2 {
		return
	}

	channelID := callback.Channel.ID

	link, err := store.GetChannelLink(ctx, channelID)
	if err != nil || link == nil {
		return
	}

	switch parts[1] {
	case "none":
		link.DefaultAgent = ""
	case "set":
		if len(parts) < 3 {
			return
		}
		link.DefaultAgent = parts[2]
	default:
		return
	}

	if err := store.UpdateChannelLink(ctx, link); err != nil {
		log.Error("Failed to update default agent via callback", "error", err)
	}
}

func postEphemeral(client *slackapi.Client, channelID, userID, text string) {
	client.PostEphemeral(channelID, userID, slackapi.MsgOptionText(text, false))
}
