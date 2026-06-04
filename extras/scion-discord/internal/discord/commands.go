package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// AgentInfo holds an agent's slug and current activity state.
type AgentInfo struct {
	Slug     string `json:"slug"`
	Activity string `json:"activity,omitempty"`
}

// ProjectOption holds a project's identifiers for display in selection UI.
type ProjectOption struct {
	ID   string
	Name string
	Slug string
}

// DisplayName returns a human-readable label for the project.
func (p ProjectOption) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	if p.Slug != "" {
		return p.Slug
	}
	return p.ID
}

// HubClient provides access to the Scion hub API for project and agent listing.
type HubClient interface {
	ListProjects(ctx context.Context) ([]ProjectOption, error)
	ListProjectsFresh(ctx context.Context) ([]ProjectOption, error)
	ListProjectsForUser(ctx context.Context, ownerID string) ([]ProjectOption, error)
	ListAgents(ctx context.Context, projectID string) ([]AgentInfo, error)
}

// CommandHandler manages Discord slash command registration and dispatch.
type CommandHandler struct {
	store         Store
	session       *discordgo.Session
	hubClient     HubClient
	log           *slog.Logger
	appID         string
	guildID       string // empty = global commands
	agentCacheTTL time.Duration
}

// NewCommandHandler creates a new CommandHandler. agentCacheTTL controls how
// long agent lists are cached before refreshing from the Hub API.
func NewCommandHandler(store Store, session *discordgo.Session, hubClient HubClient, appID, guildID string, agentCacheTTL time.Duration, log *slog.Logger) *CommandHandler {
	if log == nil {
		log = slog.Default()
	}
	return &CommandHandler{
		store:         store,
		session:       session,
		hubClient:     hubClient,
		log:           log,
		appID:         appID,
		guildID:       guildID,
		agentCacheTTL: agentCacheTTL,
	}
}

// RegisterCommands registers the /scion command and its subcommands with Discord.
func (h *CommandHandler) RegisterCommands() error {
	cmd := &discordgo.ApplicationCommand{
		Name:        "scion",
		Description: "Scion agent management",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "setup",
				Description: "Link this channel to a Scion project",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "unlink",
				Description: "Unlink this channel from its project",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "agents",
				Description: "List agents in the linked project",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "status",
				Description: "Show agent status",
				Options: []*discordgo.ApplicationCommandOption{{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "agent",
					Description:  "Agent name",
					Required:     true,
					Autocomplete: true,
				}},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "start",
				Description: "Start an agent",
				Options: []*discordgo.ApplicationCommandOption{{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "agent",
					Description:  "Agent name",
					Required:     true,
					Autocomplete: true,
				}},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "stop",
				Description: "Stop an agent",
				Options: []*discordgo.ApplicationCommandOption{{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "agent",
					Description:  "Agent name",
					Required:     true,
					Autocomplete: true,
				}},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "msg",
				Description: "Send a message to an agent",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:         discordgo.ApplicationCommandOptionString,
						Name:         "agent",
						Description:  "Agent name",
						Required:     true,
						Autocomplete: true,
					},
					{
						Type:        discordgo.ApplicationCommandOptionString,
						Name:        "text",
						Description: "Message text",
						Required:    true,
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "logs",
				Description: "View agent logs",
				Options: []*discordgo.ApplicationCommandOption{{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "agent",
					Description:  "Agent name",
					Required:     true,
					Autocomplete: true,
				}},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "default",
				Description: "Set or show the default agent for this channel",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "register",
				Description: "Link your Discord account to Scion Hub",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "unregister",
				Description: "Unlink your Discord account from Scion Hub",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "settings",
				Description: "Configure channel notification settings",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "info",
				Description: "Show your registration info and linked project",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "help",
				Description: "Show available commands",
			},
		},
	}

	_, err := h.session.ApplicationCommandCreate(h.appID, h.guildID, cmd)
	if err != nil {
		return fmt.Errorf("registering /scion command: %w", err)
	}

	h.log.Info("Registered /scion slash command", "app_id", h.appID, "guild_id", h.guildID)
	return nil
}

// ephemeralCommands lists subcommands whose responses should be ephemeral.
var ephemeralCommands = map[string]bool{
	"help":     true,
	"info":     true,
	"register": true,
	"setup":    true,
	"unlink":   true,
	"settings": true,
	"default":  true,
}

// ephemeralFlag returns MessageFlagsEphemeral if the subcommand should be
// ephemeral, or 0 otherwise.
func ephemeralFlag(i *discordgo.InteractionCreate) discordgo.MessageFlags {
	data := i.ApplicationCommandData()
	if len(data.Options) > 0 {
		if ephemeralCommands[data.Options[0].Name] {
			return discordgo.MessageFlagsEphemeral
		}
	}
	return 0
}

// HandleSlashCommand dispatches a slash command interaction to the
// appropriate handler. Simple commands that don't need async Hub API
// calls respond immediately; others defer and process asynchronously.
func (h *CommandHandler) HandleSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if data.Name != "scion" || len(data.Options) == 0 {
		return
	}

	subcommand := data.Options[0].Name

	// Commands that don't need async Hub API calls respond immediately.
	if subcommand == "help" {
		h.respondImmediate(s, i, helpText())
		return
	}

	// All other commands defer — Discord requires a response within 3 seconds.
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: ephemeralFlag(i),
		},
	})
	if err != nil {
		h.log.Error("Failed to acknowledge slash command", "error", err)
		return
	}

	go func() {
		switch subcommand {
		case "setup":
			h.HandleSetup(s, i)
		case "unlink":
			h.HandleUnlink(s, i)
		case "agents":
			h.HandleAgents(s, i)
		case "info":
			h.HandleInfo(s, i)
		case "status":
			h.HandleStatus(s, i)
		case "start":
			h.HandleStart(s, i)
		case "stop":
			h.HandleStop(s, i)
		case "msg":
			h.HandleMessage(s, i)
		case "logs":
			h.HandleLogs(s, i)
		case "settings":
			h.HandleSettings(s, i)
		case "default":
			h.HandleDefault(s, i)
		// register and unregister are handled by RegistrationHandler
		// and should be wired up in the broker's dispatch
		default:
			h.followup(s, i, fmt.Sprintf("Unknown subcommand: %s", subcommand))
		}
	}()
}

// HandleAutocomplete handles autocomplete interactions for the "agent"
// option. It looks up the channel link, fetches agents, and returns
// matching choices.
func (h *CommandHandler) HandleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		return
	}

	sub := data.Options[0]

	for _, opt := range sub.Options {
		if !opt.Focused {
			continue
		}
		if opt.Name != "agent" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		link, err := h.store.GetChannelLink(ctx, i.ChannelID)
		if err != nil || link == nil {
			// No link — return empty choices.
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionApplicationCommandAutocompleteResult,
				Data: &discordgo.InteractionResponseData{},
			})
			return
		}

		agents, err := h.getAgents(ctx, link.ProjectID)
		if err != nil {
			h.log.Debug("Failed to get agents for autocomplete", "error", err)
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionApplicationCommandAutocompleteResult,
				Data: &discordgo.InteractionResponseData{},
			})
			return
		}

		prefix := strings.ToLower(opt.StringValue())
		var choices []*discordgo.ApplicationCommandOptionChoice

		for _, slug := range agents {
			if strings.HasPrefix(strings.ToLower(slug), prefix) {
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
					Name:  slug,
					Value: slug,
				})
			}
			if len(choices) >= 25 {
				break
			}
		}

		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: choices},
		})
		return
	}
}

// helpText returns the help message listing available commands.
func helpText() string {
	return "**Scion Bot Commands**\n\n" +
		"`/scion setup` — Link this channel to a Scion project\n" +
		"`/scion unlink` — Unlink this channel from its project\n" +
		"`/scion agents` — List agents in the linked project\n" +
		"`/scion status <agent>` — Show agent status\n" +
		"`/scion start <agent>` — Start an agent\n" +
		"`/scion stop <agent>` — Stop an agent\n" +
		"`/scion message <agent> <text>` — Send a message to an agent\n" +
		"`/scion logs <agent>` — View agent logs\n" +
		"`/scion default` — Set or clear the default agent\n" +
		"`/scion register` — Link your Discord account to Scion Hub\n" +
		"`/scion unregister` — Unlink your Discord account\n" +
		"`/scion settings` — Configure channel notification settings\n" +
		"`/scion info` — Show your registration info\n" +
		"`/scion help` — Show this help message\n\n" +
		"Mention the bot or an agent by name in a linked channel to send messages."
}

// HandleHelp responds with a listing of available commands.
// Used as a fallback when the command is dispatched via the deferred path.
func (h *CommandHandler) HandleHelp(s *discordgo.Session, i *discordgo.InteractionCreate) {
	h.followup(s, i, helpText())
}

// respondImmediate sends an immediate (non-deferred) response to an
// interaction, suitable for commands that don't need async processing.
func (h *CommandHandler) respondImmediate(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   ephemeralFlag(i),
		},
	})
	if err != nil {
		h.log.Error("Failed to send immediate response", "error", err)
	}
}

// HandleSetup starts the channel setup flow: check permissions, check
// registration, list projects, and present selection buttons.
func (h *CommandHandler) HandleSetup(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Check Discord permissions.
	if !hasChannelAdminPermission(i) {
		h.followup(s, i, "You need **Manage Channels** or **Administrator** permission to set up this channel.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Check if user is registered.
	discordUserID := ""
	discordUsername := ""
	if i.Member != nil && i.Member.User != nil {
		discordUserID = i.Member.User.ID
		discordUsername = i.Member.User.Username
	} else if i.User != nil {
		discordUserID = i.User.ID
		discordUsername = i.User.Username
	}

	if discordUserID == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}

	mapping, err := h.store.GetUserMapping(ctx, discordUserID)
	if err != nil {
		h.log.Error("Failed to check user mapping", "error", err, "discord_user_id", discordUserID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if mapping == nil {
		h.followup(s, i, "Please link your Discord account first with `/scion register`.")
		return
	}

	// Check existing link.
	link, err := h.store.GetChannelLink(ctx, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to check channel link", "error", err, "channel_id", i.ChannelID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link != nil {
		h.followup(s, i, fmt.Sprintf(
			"This channel is already linked to project **%s**.\nUse `/scion unlink` first to change it.",
			link.ProjectSlug,
		))
		return
	}

	// Get user's projects.
	var projects []ProjectOption
	if mapping.ScionUserID != "" {
		projects, err = h.hubClient.ListProjectsForUser(ctx, mapping.ScionUserID)
		if err != nil {
			h.log.Warn("Failed to list user projects", "error", err, "user_id", mapping.ScionUserID)
		}
	}

	if len(projects) == 0 {
		projects, err = h.hubClient.ListProjectsFresh(ctx)
		if err != nil {
			h.log.Warn("Failed to list projects from hub", "error", err)
		}
	}

	if len(projects) == 0 {
		h.followup(s, i, "No projects found. Create a project in the hub first.")
		return
	}

	// Build button rows for project selection (max 5 buttons per row, max 5 rows).
	var rows []discordgo.MessageComponent
	var buttons []discordgo.MessageComponent
	for idx, proj := range projects {
		buttons = append(buttons, discordgo.Button{
			Label:    proj.DisplayName(),
			Style:    discordgo.PrimaryButton,
			CustomID: fmt.Sprintf("setup:proj:%s", proj.ID),
		})
		if len(buttons) == 5 || idx == len(projects)-1 {
			rows = append(rows, discordgo.ActionsRow{Components: buttons})
			buttons = nil
		}
		// Discord max 5 action rows per message.
		if len(rows) >= 5 {
			break
		}
	}

	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    "Select a project to link this channel to:",
		Components: rows,
	})

	h.log.Info("Setup initiated",
		"channel_id", i.ChannelID,
		"discord_user", discordUsername,
		"project_count", len(projects),
	)
}

// HandleUnlink removes the channel-to-project link.
func (h *CommandHandler) HandleUnlink(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !hasChannelAdminPermission(i) {
		h.followup(s, i, "You need **Manage Channels** or **Administrator** permission to unlink this channel.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetChannelLink(ctx, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to check channel link", "error", err)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.followup(s, i, "This channel is not linked to a project.")
		return
	}

	if err := h.store.DeleteChannelLink(ctx, i.ChannelID); err != nil {
		h.log.Error("Failed to delete channel link", "error", err, "channel_id", i.ChannelID)
		h.followup(s, i, "Failed to unlink. Please try again.")
		return
	}

	h.followup(s, i, fmt.Sprintf("Channel unlinked from project **%s**.", link.ProjectSlug))
	h.log.Info("Channel unlinked", "channel_id", i.ChannelID, "project", link.ProjectSlug)
}

// HandleAgents lists agents in the linked project.
func (h *CommandHandler) HandleAgents(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetChannelLink(ctx, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to get channel link", "error", err, "channel_id", i.ChannelID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents", "error", err, "project_id", link.ProjectID)
		h.followup(s, i, "Failed to fetch agents. Please try again later.")
		return
	}

	if len(agents) == 0 {
		h.followup(s, i, "No agents found for this project.")
		return
	}

	var lines []string
	for _, agent := range agents {
		emoji := activityEmoji(agent.Activity)
		label := agent.Slug
		if agent.Activity != "" {
			label += " -- " + agent.Activity
		}
		if agent.Slug == link.DefaultAgent {
			label += " (default)"
		}
		lines = append(lines, fmt.Sprintf("%s %s", emoji, label))
	}

	h.followup(s, i, fmt.Sprintf("**Agents in %s:**\n%s", link.ProjectSlug, strings.Join(lines, "\n")))
}

// HandleInfo shows the user's registration status and linked project info.
func (h *CommandHandler) HandleInfo(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	discordUserID := interactionUserID(i)
	if discordUserID == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}

	mapping, err := h.store.GetUserMapping(ctx, discordUserID)
	if err != nil {
		h.log.Error("Failed to check user mapping", "error", err)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}

	var sb strings.Builder
	if mapping == nil {
		sb.WriteString("**Registration:** Not registered\n")
		sb.WriteString("Use `/scion register` to link your Discord account to Scion Hub.")
	} else {
		sb.WriteString("**Registration:** Linked\n")
		if mapping.ScionEmail != "" {
			sb.WriteString(fmt.Sprintf("**Email:** %s\n", mapping.ScionEmail))
		}
		if mapping.ScionUserID != "" {
			sb.WriteString(fmt.Sprintf("**User ID:** %s\n", mapping.ScionUserID))
		}
		sb.WriteString(fmt.Sprintf("**Linked at:** %s\n", mapping.LinkedAt.UTC().Format(time.RFC3339)))
	}

	// Show channel link if in a guild channel.
	if i.ChannelID != "" {
		link, linkErr := h.store.GetChannelLink(ctx, i.ChannelID)
		if linkErr == nil && link != nil {
			sb.WriteString(fmt.Sprintf("\n**Channel project:** %s", link.ProjectSlug))
			if link.DefaultAgent != "" {
				sb.WriteString(fmt.Sprintf("\n**Default agent:** %s", link.DefaultAgent))
			}
		}
	}

	h.followup(s, i, sb.String())
}

// HandleStatus shows the status of a specific agent.
func (h *CommandHandler) HandleStatus(s *discordgo.Session, i *discordgo.InteractionCreate) {
	agentSlug := getSubcommandOption(i, "agent")
	if agentSlug == "" {
		h.followup(s, i, "Please specify an agent name.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetChannelLink(ctx, i.ChannelID)
	if err != nil || link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.followup(s, i, "Failed to fetch agent status. Please try again.")
		return
	}

	for _, agent := range agents {
		if agent.Slug == agentSlug {
			emoji := activityEmoji(agent.Activity)
			activity := agent.Activity
			if activity == "" {
				activity = "unknown"
			}
			h.followup(s, i, fmt.Sprintf("%s **%s** -- %s", emoji, agent.Slug, activity))
			return
		}
	}

	h.followup(s, i, fmt.Sprintf("Agent **%s** not found in this project.", agentSlug))
}

// HandleStart is a placeholder for starting an agent (Phase 4).
func (h *CommandHandler) HandleStart(s *discordgo.Session, i *discordgo.InteractionCreate) {
	agentSlug := getSubcommandOption(i, "agent")
	if agentSlug == "" {
		h.followup(s, i, "Please specify an agent name.")
		return
	}
	h.followup(s, i, fmt.Sprintf("Starting agent **%s** is not yet implemented.", agentSlug))
}

// HandleStop is a placeholder for stopping an agent (Phase 4).
func (h *CommandHandler) HandleStop(s *discordgo.Session, i *discordgo.InteractionCreate) {
	agentSlug := getSubcommandOption(i, "agent")
	if agentSlug == "" {
		h.followup(s, i, "Please specify an agent name.")
		return
	}
	h.followup(s, i, fmt.Sprintf("Stopping agent **%s** is not yet implemented.", agentSlug))
}

// HandleMessage is a placeholder for sending a message to an agent (Phase 4).
func (h *CommandHandler) HandleMessage(s *discordgo.Session, i *discordgo.InteractionCreate) {
	agentSlug := getSubcommandOption(i, "agent")
	text := getSubcommandOption(i, "text")
	if agentSlug == "" || text == "" {
		h.followup(s, i, "Please specify both an agent name and message text.")
		return
	}
	h.followup(s, i, fmt.Sprintf("Sending messages to agents via slash command is not yet implemented.\nAgent: **%s**\nMessage: %s", agentSlug, text))
}

// HandleLogs is a placeholder for viewing agent logs (Phase 4).
func (h *CommandHandler) HandleLogs(s *discordgo.Session, i *discordgo.InteractionCreate) {
	agentSlug := getSubcommandOption(i, "agent")
	if agentSlug == "" {
		h.followup(s, i, "Please specify an agent name.")
		return
	}
	h.followup(s, i, fmt.Sprintf("Viewing logs for agent **%s** is not yet implemented.", agentSlug))
}

// HandleDefault shows agent selection buttons for setting the default agent.
func (h *CommandHandler) HandleDefault(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetChannelLink(ctx, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to get channel link", "error", err, "channel_id", i.ChannelID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	agents, err := h.getAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents", "error", err, "project_id", link.ProjectID)
		h.followup(s, i, "Failed to fetch agents. Please try again later.")
		return
	}

	if len(agents) == 0 {
		h.followup(s, i, "No agents found in this project.")
		return
	}

	var currentText string
	if link.DefaultAgent != "" {
		currentText = fmt.Sprintf("Current default: **%s**\n", link.DefaultAgent)
	}

	var rows []discordgo.MessageComponent
	var buttons []discordgo.MessageComponent
	for idx, slug := range agents {
		style := discordgo.SecondaryButton
		if slug == link.DefaultAgent {
			style = discordgo.PrimaryButton
		}
		buttons = append(buttons, discordgo.Button{
			Label:    slug,
			Style:    style,
			CustomID: fmt.Sprintf("default:set:%s", slug),
		})
		if len(buttons) == 5 || idx == len(agents)-1 {
			rows = append(rows, discordgo.ActionsRow{Components: buttons})
			buttons = nil
		}
		if len(rows) >= 4 {
			break
		}
	}
	if len(rows) < 5 {
		rows = append(rows, discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "None",
					Style:    discordgo.DangerButton,
					CustomID: "default:none",
				},
			},
		})
	}

	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    currentText + "Select the default agent for this channel:",
		Components: rows,
	})
}

// HandleSettings shows channel settings with toggle buttons.
func (h *CommandHandler) HandleSettings(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetChannelLink(ctx, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to get channel link", "error", err, "channel_id", i.ChannelID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	content, components := settingsPanel(link)
	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    content,
		Components: components,
	})
}

// settingsPanel builds the settings message content and toggle buttons.
func settingsPanel(link *ChannelLink) (string, []discordgo.MessageComponent) {
	observeLabel := "Observe Mode: OFF"
	observeStyle := discordgo.SecondaryButton
	if link.ShowAgentToAgent {
		observeLabel = "Observe Mode: ON"
		observeStyle = discordgo.SuccessButton
	}

	stateLabel := "State Notifications: OFF"
	stateStyle := discordgo.SecondaryButton
	if link.ShowStateChanges {
		stateLabel = "State Notifications: ON"
		stateStyle = discordgo.SuccessButton
	}

	content := fmt.Sprintf("**Channel Settings** — %s\n\n"+
		"**Observe Mode** — Show agent-to-agent messages in this channel\n"+
		"**State Notifications** — Show agent state change cards (working/idle/stalled)",
		link.ProjectSlug)

	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    observeLabel,
					Style:    observeStyle,
					CustomID: fmt.Sprintf("settings:observe:%s", link.ChannelID),
				},
				discordgo.Button{
					Label:    stateLabel,
					Style:    stateStyle,
					CustomID: fmt.Sprintf("settings:statechange:%s", link.ChannelID),
				},
			},
		},
	}

	return content, components
}

// getAgents returns agent slugs for a project, using the store cache with
// a fallback to the hub API.
func (h *CommandHandler) getAgents(ctx context.Context, projectID string) ([]string, error) {
	cached, err := h.store.GetProjectAgents(ctx, projectID)
	if err != nil {
		h.log.Warn("Failed to read agent cache", "project_id", projectID, "error", err)
	}
	if cached != nil && time.Since(cached.RefreshedAt) < h.agentCacheTTL {
		return cached.AgentSlugs, nil
	}

	agents, err := h.hubClient.ListAgents(ctx, projectID)
	if err != nil {
		if cached != nil {
			return cached.AgentSlugs, nil
		}
		return nil, err
	}

	slugs := make([]string, len(agents))
	for i, a := range agents {
		slugs[i] = a.Slug
	}

	saveErr := h.store.SetProjectAgents(ctx, &ProjectAgents{
		ProjectID:   projectID,
		AgentSlugs:  slugs,
		RefreshedAt: time.Now(),
	})
	if saveErr != nil {
		h.log.Warn("Failed to cache agents", "project_id", projectID, "error", saveErr)
	}

	return slugs, nil
}

// followup sends a follow-up message to the interaction.
func (h *CommandHandler) followup(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: content,
	})
	if err != nil {
		h.log.Error("Failed to send follow-up message", "error", err)
	}
}

// hasChannelAdminPermission checks if the invoking member has MANAGE_CHANNELS
// or ADMINISTRATOR permission.
func hasChannelAdminPermission(i *discordgo.InteractionCreate) bool {
	if i.Member == nil {
		return false
	}
	perms := i.Member.Permissions
	return perms&discordgo.PermissionManageChannels != 0 ||
		perms&discordgo.PermissionAdministrator != 0
}

// getSubcommandOption extracts a named option value from a subcommand interaction.
func getSubcommandOption(i *discordgo.InteractionCreate, name string) string {
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		return ""
	}
	sub := data.Options[0]
	for _, opt := range sub.Options {
		if opt.Name == name {
			return opt.StringValue()
		}
	}
	return ""
}

// interactionUserID extracts the Discord user ID from an interaction,
// handling both guild (Member) and DM (User) contexts.
func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// activityEmoji returns an emoji for an agent activity state.
func activityEmoji(activity string) string {
	switch strings.ToLower(activity) {
	case "idle":
		return "💤"
	case "executing":
		return "⚙️"
	case "thinking":
		return "💭"
	case "blocked":
		return "🚧"
	case "completed":
		return "✅"
	case "error":
		return "❌"
	case "stalled":
		return "⏳"
	default:
		return "▶️"
	}
}
