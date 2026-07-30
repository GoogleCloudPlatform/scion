package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
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

// Template holds a template's identifiers for display in selection UI.
type Template struct {
	Slug string
	Name string
}

// CreateAgentRequest holds the parameters for creating a new agent via the hub.
type CreateAgentRequest struct {
	Name     string `json:"name"`
	Template string `json:"template,omitempty"`
}

// CreateAgentResponse holds the hub's response after creating an agent.
type CreateAgentResponse struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// HubClient provides access to the Scion hub API for project and agent listing.
type HubClient interface {
	ListProjects(ctx context.Context) ([]ProjectOption, error)
	ListProjectsFresh(ctx context.Context) ([]ProjectOption, error)
	ListProjectsForUser(ctx context.Context, ownerID string) ([]ProjectOption, error)
	ListAgents(ctx context.Context, projectID string) ([]AgentInfo, error)
	ListTemplates(ctx context.Context, projectID string) ([]Template, error)

	// CreateAgent POSTs /api/v1/projects/{projectId}/agents.
	// onBehalfOf is a namespaced principal (e.g. "user:alice@example.com"); it is
	// sent as the X-Scion-On-Behalf-Of header, NOT in the body.
	CreateAgent(ctx context.Context, projectID string, req CreateAgentRequest, onBehalfOf string) (*CreateAgentResponse, error)
}

// CommandHandler manages Discord slash command registration and dispatch.
type CommandHandler struct {
	store          Store
	session        *discordgo.Session
	hubClient      HubClient
	log            *slog.Logger
	appID          string
	guildIDs       []string // empty = global commands
	agentCacheTTL  time.Duration
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError
}

// NewCommandHandler creates a new CommandHandler. agentCacheTTL controls how
// long agent lists are cached before refreshing from the Hub API.
func NewCommandHandler(store Store, session *discordgo.Session, hubClient HubClient, deliverInbound func(string, *messages.StructuredMessage) *hubError, appID string, guildIDs []string, agentCacheTTL time.Duration, log *slog.Logger) *CommandHandler {
	if log == nil {
		log = slog.Default()
	}
	return &CommandHandler{
		store:          store,
		session:        session,
		hubClient:      hubClient,
		deliverInbound: deliverInbound,
		log:            log,
		appID:          appID,
		guildIDs:       guildIDs,
		agentCacheTTL:  agentCacheTTL,
	}
}

// RegisterCommands registers the /scion command and its subcommands with Discord.
// When guild IDs are configured, commands are registered per-guild for instant
// availability. When no guild IDs are configured, commands are registered globally.
func (h *CommandHandler) RegisterCommands() error {
	if len(h.guildIDs) == 0 {
		return h.RegisterCommandsForGuild("")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, guildID := range h.guildIDs {
		wg.Add(1)
		go func(gID string) {
			defer wg.Done()
			if err := h.RegisterCommandsForGuild(gID); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(guildID)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// RegisterCommandsForGuild registers the /scion command for a single guild.
// Pass an empty guildID for global (non-guild-scoped) registration.
func (h *CommandHandler) RegisterCommandsForGuild(guildID string) error {
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
				Options: []*discordgo.ApplicationCommandOption{{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "agent",
					Description:  "Agent name (leave empty to show/clear current default)",
					Required:     false,
					Autocomplete: true,
				}},
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
				Name:        "thread",
				Description: "Create a thread with a new agent in it",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionString,
						Name:        "title",
						Description: "Thread name; also the basis for the agent slug",
						Required:    true,
						MaxLength:   100,
					},
					{
						Type:         discordgo.ApplicationCommandOptionString,
						Name:         "template",
						Description:  "Agent template (defaults to the project's default)",
						Required:     false,
						Autocomplete: true,
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "help",
				Description: "Show available commands",
			},
		},
	}

	_, err := h.session.ApplicationCommandCreate(h.appID, guildID, cmd)
	if err != nil {
		return fmt.Errorf("registering /scion command for guild %q: %w", guildID, err)
	}

	logGuildID := guildID
	if logGuildID == "" {
		logGuildID = "global"
	}
	h.log.Info("Registered /scion slash command", "app_id", h.appID, "guild_id", logGuildID)
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
	"thread":   true,
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
		case "thread":
			h.HandleThread(s, i)
		// register and unregister are handled by RegistrationHandler
		// and should be wired up in the broker's dispatch
		default:
			h.followup(s, i, fmt.Sprintf("Unknown subcommand: %s", subcommand))
		}
	}()
}

// HandleAutocomplete handles autocomplete interactions by dispatching on the
// focused option. Supports "agent" (existing) and "template" (new) options.
func (h *CommandHandler) HandleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		return
	}

	focused := focusedOption(data.Options[0])
	if focused == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
	if err != nil || link == nil {
		// No link — return empty choices.
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{},
		})
		return
	}

	var choices []*discordgo.ApplicationCommandOptionChoice
	switch focused.Name {
	case "agent":
		choices = h.completeAgents(ctx, link.ProjectID, focused.StringValue())
	case "template":
		choices = h.completeTemplates(ctx, link.ProjectID, focused.StringValue())
	default:
		// Unknown option — return empty choices.
	}

	// Discord hard-caps at 25 choices.
	if len(choices) > 25 {
		choices = choices[:25]
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	})
}

// focusedOption returns the option with Focused==true in the subcommand, or nil.
func focusedOption(sub *discordgo.ApplicationCommandInteractionDataOption) *discordgo.ApplicationCommandInteractionDataOption {
	for _, opt := range sub.Options {
		if opt.Focused {
			return opt
		}
	}
	return nil
}

// completeAgents returns autocomplete choices for the "agent" option, filtered
// by the typed prefix.
func (h *CommandHandler) completeAgents(ctx context.Context, projectID, typed string) []*discordgo.ApplicationCommandOptionChoice {
	agents, err := h.getAgents(ctx, projectID)
	if err != nil {
		h.log.Debug("Failed to get agents for autocomplete", "error", err)
		return nil
	}

	prefix := strings.ToLower(typed)
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
	return choices
}

// completeTemplates returns autocomplete choices for the "template" option,
// filtered by the typed prefix.
func (h *CommandHandler) completeTemplates(ctx context.Context, projectID, typed string) []*discordgo.ApplicationCommandOptionChoice {
	templates, err := h.hubClient.ListTemplates(ctx, projectID)
	if err != nil {
		h.log.Debug("Failed to get templates for autocomplete", "error", err)
		return nil
	}

	prefix := strings.ToLower(typed)
	var choices []*discordgo.ApplicationCommandOptionChoice
	for _, t := range templates {
		label := t.Name
		if label == "" {
			label = t.Slug
		}
		if strings.HasPrefix(strings.ToLower(t.Slug), prefix) ||
			strings.HasPrefix(strings.ToLower(label), prefix) {
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
				Name:  label,
				Value: t.Slug,
			})
		}
		if len(choices) >= 25 {
			break
		}
	}
	return choices
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
		"`/scion msg <agent> <text>` — Send a message to an agent\n" +
		"`/scion logs <agent>` — View agent logs\n" +
		"`/scion default [agent]` — Set or clear the default agent\n" +
		"`/scion thread <title> [template]` — Create a thread with a new agent\n" +
		"`/scion register` — Link your Discord account to Scion Hub\n" +
		"`/scion unregister` — Unlink your Discord account\n" +
		"`/scion settings` — Configure channel notification settings\n" +
		"`/scion info` — Show your registration info\n" +
		"`/scion help` — Show this help message\n\n" +
		"Mention the bot or an agent by name in a linked channel to send messages.\n\n" +
		fmt.Sprintf("_Scion Discord Integration — %s_", version.Get())
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

	// If running in a thread/forum topic, resolve the parent channel.
	var link *ChannelLink
	parentID := threadParentID(s, i.ChannelID)
	if parentID != "" {
		link, err = h.store.GetChannelLink(ctx, parentID)
		if err != nil {
			h.log.Error("Failed to check parent channel link", "error", err, "parent_id", parentID)
			h.followup(s, i, "Something went wrong. Please try again.")
			return
		}
		if link != nil && link.Active {
			h.followup(s, i, fmt.Sprintf(
				"This channel is already set up (project **%s**). Use `/scion default` to set a per-thread default agent.",
				link.ProjectSlug,
			))
			return
		}
	} else {
		// Check existing link.
		link, err = h.store.GetChannelLink(ctx, i.ChannelID)
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

	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
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
		link, linkErr := resolveChannelLink(ctx, s, h.store, i.ChannelID)
		if linkErr == nil && link != nil {
			if link.GuildName != "" {
				sb.WriteString(fmt.Sprintf("\n**Server:** %s", link.GuildName))
			}
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

	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
	if err != nil || link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.followup(s, i, "Failed to fetch agent status. Please try again.")
		return
	}

	// Detect thread context for default info display.
	statusParentID := threadParentID(s, i.ChannelID)

	for _, agent := range agents {
		if agent.Slug == agentSlug {
			emoji := activityEmoji(agent.Activity)
			activity := agent.Activity
			if activity == "" {
				activity = "unknown"
			}
			statusMsg := fmt.Sprintf("%s **%s** -- %s", emoji, agent.Slug, activity)
			if statusParentID != "" {
				threadDefault, err := h.store.GetThreadDefault(ctx, link.ChannelID, i.ChannelID)
				if err != nil {
					h.log.Error("Failed to get thread default", "error", err)
				} else if threadDefault != "" {
					statusMsg += fmt.Sprintf("\nThread default: **%s**", threadDefault)
				}
				channelDefault := link.DefaultAgent
				if channelDefault != "" {
					statusMsg += fmt.Sprintf("\nChannel default: **%s**", channelDefault)
				} else {
					statusMsg += "\nChannel default: none"
				}
			}
			h.followup(s, i, statusMsg)
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

// HandleMessage sends a message to an agent via the hub.
func (h *CommandHandler) HandleMessage(s *discordgo.Session, i *discordgo.InteractionCreate) {
	agentSlug := getSubcommandOption(i, "agent")
	text := getSubcommandOption(i, "text")
	if agentSlug == "" || text == "" {
		h.followup(s, i, "Please specify both an agent name and message text.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
	if err != nil || link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	discordUserID := interactionUserID(i)
	if discordUserID == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}

	mapping, err := h.store.GetUserMapping(ctx, discordUserID)
	if err != nil || mapping == nil {
		h.followup(s, i, "Please link your Discord account first with `/scion register`.")
		return
	}

	sender := "user:" + mapping.ScionEmail
	if mapping.ScionEmail == "" {
		sender = "discord:" + mapping.DiscordUsername
	}

	// Verify the agent exists.
	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.followup(s, i, "Failed to verify agent. Please try again.")
		return
	}
	found := false
	for _, a := range agents {
		if a.Slug == agentSlug {
			found = true
			break
		}
	}
	if !found {
		h.followup(s, i, fmt.Sprintf("Agent **%s** not found in this project. Use `/scion agents` to see available agents.", agentSlug))
		return
	}

	// Save conversation context so the agent's reply routes back here.
	cc := &ConversationContext{
		DiscordUserID: discordUserID,
		ProjectID:     link.ProjectID,
		AgentSlug:     agentSlug,
		LastChannelID: i.ChannelID,
		LastMessageAt: time.Now(),
	}
	if err := h.store.SetConversationContext(ctx, cc); err != nil {
		h.log.Warn("Failed to save conversation context", "error", err)
	}

	if h.deliverInbound == nil {
		h.followup(s, i, "Message delivery is not configured.")
		return
	}

	topic := projectcompat.AgentTopic(link.ProjectID, agentSlug)
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Channel:   "discord",
		ThreadID:  i.ChannelID,
		Sender:    sender,
		SenderID:  discordUserID,
		Recipient: "agent:" + agentSlug,
		Msg:       text,
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"discord_channel_id": i.ChannelID,
			"discord_guild_id":   i.GuildID,
			"project_id":         link.ProjectID,
		},
	}

	if he := h.deliverInbound(topic, msg); he != nil {
		h.followup(s, i, he.userFacingMessage())
		return
	}

	h.log.Info("Slash command message delivered",
		"agent", agentSlug, "sender", sender, "channel_id", i.ChannelID)

	h.followup(s, i, fmt.Sprintf("Message sent to **%s**: %s", agentSlug, text))
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
// When invoked from a thread, it manages per-thread overrides instead of
// the channel-level default.
//
// Hybrid routing:
//   - If the "agent" parameter is provided: validate and set the default directly.
//   - If no parameter + ≤20 agents: show the existing button grid.
//   - If no parameter + >20 agents: show info + prompt to use autocomplete.
func (h *CommandHandler) HandleDefault(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
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

	// Detect thread context.
	parentID := threadParentID(s, i.ChannelID)
	isThread := parentID != ""
	threadID := ""
	if isThread {
		threadID = i.ChannelID
	}

	// If the "agent" parameter was provided, set the default directly.
	agentParam := getSubcommandOption(i, "agent")
	if agentParam != "" {
		// Validate that the agent exists in the project (case-insensitive).
		matchedSlug := ""
		for _, slug := range agents {
			if strings.EqualFold(slug, agentParam) {
				matchedSlug = slug
				break
			}
		}
		if matchedSlug == "" {
			h.followup(s, i, fmt.Sprintf(
				"Agent **%s** not found in this project. Use `/scion agents` to see available agents.",
				agentParam,
			))
			return
		}

		// Set the default at the appropriate level using the correctly-cased slug.
		if isThread {
			if err := h.store.SetThreadDefault(ctx, link.ChannelID, threadID, matchedSlug); err != nil {
				h.log.Error("Failed to set thread default", "error", err)
				h.followup(s, i, "Failed to set thread default. Please try again.")
				return
			}
			h.followup(s, i, fmt.Sprintf("Default agent for this thread set to **%s**.", matchedSlug))
			h.log.Info("Thread default set via autocomplete",
				"channel_id", link.ChannelID, "thread_id", threadID, "agent", matchedSlug)
		} else {
			link.DefaultAgent = matchedSlug
			if err := h.store.UpdateChannelLink(ctx, link); err != nil {
				h.log.Error("Failed to set default agent", "error", err)
				h.followup(s, i, "Failed to set default agent. Please try again.")
				return
			}
			h.followup(s, i, fmt.Sprintf("Default agent set to **%s** for this channel.", matchedSlug))
			h.log.Info("Default agent set via autocomplete",
				"channel_id", i.ChannelID, "agent", matchedSlug)
		}
		return
	}

	// No agent parameter — show UI based on agent count.

	// Determine the current effective default for highlighting.
	currentDefault := link.DefaultAgent
	if isThread {
		td, err := h.store.GetThreadDefault(ctx, link.ChannelID, threadID)
		if err != nil {
			h.log.Error("Failed to get thread default", "error", err)
		} else if td != "" {
			currentDefault = td
		}
	}

	var currentText string
	if currentDefault != "" {
		currentText = fmt.Sprintf("Current default: **%s**\n", currentDefault)
	}

	// Large project path (>20 agents): info message + clear button only.
	if len(agents) > 20 {
		promptText := fmt.Sprintf(
			"This project has %d agents. Use `/scion default agent:<name>` with autocomplete to select.",
			len(agents),
		)
		if isThread {
			promptText = fmt.Sprintf(
				"This project has %d agents. Use `/scion default agent:<name>` with autocomplete to select a thread default.",
				len(agents),
			)
			if link.DefaultAgent != "" {
				promptText += fmt.Sprintf("\nChannel-wide default: **%s**", link.DefaultAgent)
			}
		}

		noneCustomID := "default:none"
		if threadID != "" {
			noneCustomID = fmt.Sprintf("default:none:%s", threadID)
		}
		rows := []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    "Clear Default",
						Style:    discordgo.DangerButton,
						CustomID: noneCustomID,
					},
				},
			},
		}

		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content:    currentText + promptText,
			Components: rows,
		})
		return
	}

	// Small project path (≤20 agents): existing button grid.
	promptText := "Select the default agent for this channel:"
	if isThread {
		promptText = "Select the default agent for this thread:"
		if link.DefaultAgent != "" {
			promptText += fmt.Sprintf("\nChannel-wide default: **%s**", link.DefaultAgent)
		}
	}

	var rows []discordgo.MessageComponent
	var buttons []discordgo.MessageComponent
	for idx, slug := range agents {
		style := discordgo.SecondaryButton
		if slug == currentDefault {
			style = discordgo.PrimaryButton
		}
		customID := fmt.Sprintf("default:set:%s", slug)
		if threadID != "" {
			customID = fmt.Sprintf("default:set:%s:%s", slug, threadID)
		}
		buttons = append(buttons, discordgo.Button{
			Label:    slug,
			Style:    style,
			CustomID: customID,
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
		noneCustomID := "default:none"
		if threadID != "" {
			noneCustomID = fmt.Sprintf("default:none:%s", threadID)
		}
		rows = append(rows, discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "None",
					Style:    discordgo.DangerButton,
					CustomID: noneCustomID,
				},
			},
		})
	}

	_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    currentText + promptText,
		Components: rows,
	})
}

// isForumChannelType checks whether a Discord channel ID refers to a forum or
// media channel (types 15 and 16). Standalone helper that works with any
// *discordgo.Session — unlike DiscordBroker.isForumChannel, it does not
// require broker state.
func isForumChannelType(s *discordgo.Session, channelID string) bool {
	if s == nil {
		return false
	}

	var ch *discordgo.Channel
	var err error
	if s.State != nil {
		ch, err = s.State.Channel(channelID)
	}
	if ch == nil || err != nil {
		if s.Ratelimiter == nil {
			return false
		}
		ch, err = s.Channel(channelID)
		if err != nil || ch == nil {
			return false
		}
	}

	return ch.Type == discordgo.ChannelTypeGuildForum ||
		ch.Type == discordgo.ChannelTypeGuildMedia
}

// HandleThread creates a Discord thread and a Scion agent in one command.
// It implements the full lifecycle: validation (Phase 0), concurrent fan-out
// (Phase 1/6), binding + kickoff (Phase 2–3/7), and hub capability probe
// (Phase 8). See arch-scion-thread-cmd.md for the complete design.
func (h *CommandHandler) HandleThread(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	title := getSubcommandOption(i, "title")
	if title == "" {
		h.followup(s, i, "Please provide a thread title.")
		return
	}

	// Step 0.1: Resolve parent channel.
	// If invoked from inside a thread, create a sibling in the parent channel
	// (decision 1a). If in a regular channel, use the current channel.
	channelID := i.ChannelID
	parentID := threadParentID(s, channelID)
	if parentID != "" {
		// We are inside a thread — create sibling in parent channel.
		channelID = parentID
	}

	// Step 0.2: Resolve channel link.
	link, err := resolveChannelLink(ctx, s, h.store, channelID)
	if err != nil {
		h.log.Error("Failed to resolve channel link for thread command", "error", err, "channel_id", channelID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.followup(s, i, "This channel is not linked to a project. Run `/scion setup` first.")
		return
	}

	// Step 0.3: Check user registration.
	discordUserID := interactionUserID(i)
	if discordUserID == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}

	mapping, err := h.store.GetUserMapping(ctx, discordUserID)
	if err != nil {
		h.log.Error("Failed to check user mapping for thread command", "error", err, "discord_user_id", discordUserID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if mapping == nil {
		h.followup(s, i, "You need to link your Discord account first. Run `/scion register`.")
		return
	}
	if mapping.ScionEmail == "" {
		h.followup(s, i, "Your registered account does not have an associated email address. Please re-register.")
		return
	}

	// Step 0.4: Slugify the title.
	// Use api.Slugify (not the local slugify in brokerauth.go) for hub compatibility.
	slug := api.Slugify(title)
	if slug == "" {
		h.followup(s, i, fmt.Sprintf("The title %q produces an invalid agent name. Please use a title with at least one letter or number.", title))
		return
	}

	// Step 0.5: Check for slug conflicts.
	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents for slug conflict check", "error", err, "project_id", link.ProjectID)
		h.followup(s, i, "Failed to verify agent name availability. Please try again.")
		return
	}
	for _, agent := range agents {
		if agent.Slug == slug {
			h.followup(s, i, fmt.Sprintf("An agent named **%s** already exists in this project. Please choose a different title.", slug))
			return
		}
	}

	// Step 0.6: Validate template (if provided).
	templateName := getSubcommandOption(i, "template")
	if templateName != "" {
		templates, err := h.hubClient.ListTemplates(ctx, link.ProjectID)
		if err != nil {
			h.log.Error("Failed to list templates for validation", "error", err, "project_id", link.ProjectID)
			h.followup(s, i, "Failed to verify template. Please try again.")
			return
		}
		found := false
		for _, t := range templates {
			if t.Slug == templateName || t.Name == templateName {
				found = true
				break
			}
		}
		if !found {
			h.followup(s, i, fmt.Sprintf("Template **%s** not found. Use autocomplete to pick from available templates, or omit the template to use the project default.", templateName))
			return
		}
	}

	// --- Phase 8: Hub capability probe ---
	// TODO: Implement a proper hub capability probe at startup or via a
	// lightweight endpoint to detect whether X-Scion-On-Behalf-Of is supported.
	// For now, after CreateAgent returns we could verify agent.OwnerID matches
	// the expected user. If the hub silently ignores the header, the agent
	// would be ownerless — a condition that should be detected and reported.
	// Full probe deferred to a follow-up change.

	// --- Phase 6: Concurrent fan-out ---
	h.log.Info("Thread command validation passed, starting orchestration",
		"title", title,
		"slug", slug,
		"template", templateName,
		"channel_id", channelID,
		"project_id", link.ProjectID,
		"discord_user_id", discordUserID,
	)

	statusContent := fmt.Sprintf("⏳ Creating agent `%s`…", slug)
	isForum := isForumChannelType(s, channelID)

	var wg sync.WaitGroup
	var agentResp *CreateAgentResponse
	var agentErr error
	var thread *discordgo.Channel
	var statusMsgID string
	var threadErr error

	wg.Add(2)

	// Goroutine A: Create the agent via the hub (long-running, up to 5 min).
	go func() {
		defer wg.Done()
		createCtx, createCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer createCancel()
		agentResp, agentErr = h.hubClient.CreateAgent(createCtx, link.ProjectID, CreateAgentRequest{
			Name:     slug,
			Template: templateName,
		}, "user:"+mapping.ScionEmail)
	}()

	// Goroutine B: Create the Discord thread + post the status message.
	go func() {
		defer wg.Done()
		if isForum {
			// Forum channels: the post IS the thread; body is mandatory.
			var ch *discordgo.Channel
			ch, threadErr = s.ForumThreadStartComplex(channelID, &discordgo.ThreadStart{
				Name:                title,
				AutoArchiveDuration: 10080, // 7 days
			}, &discordgo.MessageSend{
				Content: statusContent,
			})
			if threadErr == nil && ch != nil {
				thread = ch
				// In a forum post the starter message ID matches the thread ID.
				statusMsgID = ch.ID
			}
		} else {
			// Text channels: create thread then post a message inside it.
			var ch *discordgo.Channel
			ch, threadErr = s.ThreadStart(channelID, title,
				discordgo.ChannelTypeGuildPublicThread, 10080)
			if threadErr != nil {
				return
			}
			thread = ch
			var msg *discordgo.Message
			msg, msgErr := s.ChannelMessageSend(ch.ID, statusContent)
			if msgErr == nil && msg != nil {
				statusMsgID = msg.ID
			} else {
				h.log.Warn("Failed to send status message in new thread", "error", msgErr)
			}
		}
	}()

	wg.Wait()

	// --- Error compensation matrix ---
	// Handle all four outcomes from the concurrent fan-out.

	switch {
	case agentErr != nil && threadErr != nil:
		// Both failed — single ephemeral error.
		h.log.Error("Thread command: both agent and thread creation failed",
			"agent_error", agentErr, "thread_error", threadErr,
			"slug", slug, "title", title)
		h.followup(s, i, fmt.Sprintf(
			"Failed to create both the agent and the thread.\n"+
				"Agent error: %s\n"+
				"Thread error: %s",
			agentErr.Error(), threadErr.Error()))
		return

	case agentErr != nil && threadErr == nil:
		// Agent failed, thread OK — edit status message to show error; ephemeral reply.
		h.log.Error("Thread command: agent creation failed but thread was created",
			"agent_error", agentErr, "slug", slug, "thread_id", thread.ID)
		if statusMsgID != "" {
			_, editErr := s.ChannelMessageEdit(thread.ID, statusMsgID,
				fmt.Sprintf("❌ Agent creation failed: %s", agentErr.Error()))
			if editErr != nil {
				h.log.Error("Failed to edit status message after agent error", "error", editErr)
			}
		}
		h.followup(s, i, fmt.Sprintf(
			"Thread **%s** was created but the agent could not be started.\n"+
				"Error: %s\n"+
				"You can retry with `/scion thread` or manually create an agent and bind it with `/scion default <agent>` in the thread.",
			title, agentErr.Error()))
		return

	case agentErr == nil && threadErr != nil:
		// Agent OK, thread failed — ephemeral reply MUST name the slug.
		h.log.Error("Thread command: thread creation failed but agent was created",
			"thread_error", threadErr, "slug", agentResp.Slug)
		h.followup(s, i, fmt.Sprintf(
			"Agent **%s** was created but the Discord thread could not be created.\n"+
				"Error: %s\n"+
				"The agent is running. Create a thread manually and run `/scion default %s` in it to bind.",
			agentResp.Slug, threadErr.Error(), agentResp.Slug))
		return
	}

	// --- Both succeeded: Phase 7 — Binding + Kickoff ---

	// Step 1: SetThreadDefault — bind the thread to the agent.
	bindCtx, bindCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer bindCancel()

	bindErr := h.store.SetThreadDefault(bindCtx, channelID, thread.ID, agentResp.Slug)
	if bindErr != nil {
		h.log.Error("Failed to set thread default after creation",
			"error", bindErr, "channel_id", channelID, "thread_id", thread.ID, "slug", agentResp.Slug)
	}

	// Step 2: SetConversationContext — pre-seed the outbound route.
	ccErr := h.store.SetConversationContext(bindCtx, &ConversationContext{
		DiscordUserID: discordUserID,
		ProjectID:     link.ProjectID,
		AgentSlug:     agentResp.Slug,
		LastChannelID: thread.ID,
		LastMessageAt: time.Now(),
	})
	if ccErr != nil {
		h.log.Error("Failed to set conversation context after creation",
			"error", ccErr, "discord_user_id", discordUserID, "slug", agentResp.Slug)
	}

	// Check if binding failed — report success-with-caveat.
	if bindErr != nil || ccErr != nil {
		// Both resources exist and are healthy, only the link is missing.
		if statusMsgID != "" {
			templateLabel := "default"
			if templateName != "" {
				templateLabel = templateName
			}
			_, editErr := s.ChannelMessageEdit(thread.ID, statusMsgID,
				fmt.Sprintf("✅ Agent `%s` created (template: %s) — but automatic binding failed. Run `/scion default %s` in this thread.",
					agentResp.Slug, templateLabel, agentResp.Slug))
			if editErr != nil {
				h.log.Error("Failed to edit status message after bind error", "error", editErr)
			}
		}
		h.followup(s, i, fmt.Sprintf(
			"Agent **%s** and thread **%s** were created, but binding failed.\n"+
				"Run `/scion default %s` in the thread to bind them manually.\n"+
				"Jump to thread: https://discord.com/channels/%s/%s",
			agentResp.Slug, title, agentResp.Slug, i.GuildID, thread.ID))
		return
	}

	// Step 3: deliverInbound kickoff message.
	if h.deliverInbound != nil {
		topic := projectcompat.AgentTopic(link.ProjectID, agentResp.Slug)
		kickoffMsg := &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			ThreadID:  thread.ID,
			Sender:    "user:" + mapping.ScionEmail,
			SenderID:  discordUserID,
			Recipient: "agent:" + agentResp.Slug,
			Msg: fmt.Sprintf(
				"You have been created and asked to participate in the Discord thread %q. "+
					"Read the 'scion-messaging' skill if available and then introduce yourself in the thread to ask how you can help.",
				title),
			Type: messages.TypeInstruction,
			Metadata: map[string]string{
				"discord_channel_id": thread.ID,
				"discord_guild_id":   i.GuildID,
				"project_id":         link.ProjectID,
			},
		}
		if he := h.deliverInbound(topic, kickoffMsg); he != nil {
			h.log.Warn("Failed to deliver kickoff message",
				"error", he.Error(), "slug", agentResp.Slug, "topic", topic)
			// Non-fatal — the agent exists and is bound, user can message it directly.
		}
	}

	// Step 4: Edit the status message to show ready state.
	if statusMsgID != "" {
		templateLabel := "default"
		if templateName != "" {
			templateLabel = templateName
		}
		_, editErr := s.ChannelMessageEdit(thread.ID, statusMsgID,
			fmt.Sprintf("✅ Ready — agent `%s` (template: %s)", agentResp.Slug, templateLabel))
		if editErr != nil {
			h.log.Error("Failed to edit status message to ready state", "error", editErr)
		}
	}

	// Step 5: Ephemeral followup with jump link.
	templateInfo := ""
	if templateName != "" {
		templateInfo = fmt.Sprintf(" (template: **%s**)", templateName)
	}
	h.followup(s, i, fmt.Sprintf(
		"Thread created with agent **%s**%s.\nhttps://discord.com/channels/%s/%s",
		agentResp.Slug, templateInfo, i.GuildID, thread.ID))

	h.log.Info("Thread command completed successfully",
		"title", title,
		"slug", agentResp.Slug,
		"template", templateName,
		"channel_id", channelID,
		"thread_id", thread.ID,
		"project_id", link.ProjectID,
		"discord_user_id", discordUserID,
	)
}

// HandleSettings shows channel settings with toggle buttons.
func (h *CommandHandler) HandleSettings(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
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

var (
	threadParentsMu sync.Mutex
	threadParents   = make(map[string]string)
)

// threadParentID returns the parent channel ID if channelID is a thread,
// or empty string if it is not a thread or the lookup fails.
func threadParentID(s *discordgo.Session, channelID string) string {
	var ch *discordgo.Channel
	var err error
	if s.State != nil {
		ch, err = s.State.Channel(channelID)
	}
	if ch == nil || err != nil {
		ch, err = s.Channel(channelID)
		if err != nil {
			return ""
		}
	}
	if ch.Type == discordgo.ChannelTypeGuildPublicThread ||
		ch.Type == discordgo.ChannelTypeGuildPrivateThread ||
		ch.Type == discordgo.ChannelTypeGuildNewsThread {
		return ch.ParentID
	}
	return ""
}

// resolveChannelLink looks up a ChannelLink for channelID. If no active link
// is found and the channel is a thread, it falls back to the parent channel.
func resolveChannelLink(ctx context.Context, s *discordgo.Session, store Store, channelID string) (*ChannelLink, error) {
	link, err := store.GetChannelLink(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if link == nil || !link.Active {
		threadParentsMu.Lock()
		parentID, cached := threadParents[channelID]
		threadParentsMu.Unlock()

		if !cached {
			parentID = threadParentID(s, channelID)
			threadParentsMu.Lock()
			threadParents[channelID] = parentID
			threadParentsMu.Unlock()
		}

		if parentID != "" {
			return store.GetChannelLink(ctx, parentID)
		}
	}
	return link, nil
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
