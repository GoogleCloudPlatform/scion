package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// CallbackHandler processes Discord message component interactions (buttons, selects).
type CallbackHandler struct {
	store     Store
	session   *discordgo.Session
	hubClient HubClient
	log       *slog.Logger
}

// NewCallbackHandler creates a new CallbackHandler.
func NewCallbackHandler(store Store, session *discordgo.Session, hubClient HubClient, log *slog.Logger) *CallbackHandler {
	if log == nil {
		log = slog.Default()
	}
	return &CallbackHandler{
		store:     store,
		session:   session,
		hubClient: hubClient,
		log:       log,
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
