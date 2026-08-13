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
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// pendingTeamsLink tracks an in-progress identity linking registration that is
// polling the hub for confirmation.
type pendingTeamsLink struct {
	Code        string
	TeamsUserID string
	Activity    *Activity // original activity, used for sending replies
	Cancel      context.CancelFunc
	ExpiresAt   time.Time
}

// CommandHandler processes bot commands from incoming message activities.
type CommandHandler struct {
	broker       *TeamsBroker
	log          *slog.Logger
	pendingMu    sync.Mutex
	pendingLinks map[string]*pendingTeamsLink // keyed by teamsUserID
}

// NewCommandHandler creates a new CommandHandler.
func NewCommandHandler(broker *TeamsBroker, log *slog.Logger) *CommandHandler {
	return &CommandHandler{
		broker:       broker,
		log:          log,
		pendingLinks: make(map[string]*pendingTeamsLink),
	}
}

// Close cancels all pending polling goroutines.
func (h *CommandHandler) Close() {
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	for _, link := range h.pendingLinks {
		if link.Cancel != nil {
			link.Cancel()
		}
	}
	h.pendingLinks = make(map[string]*pendingTeamsLink)
}

// Handle checks if the activity text starts with a known command and dispatches it.
// Returns (handled bool, err error). If handled=false, the caller should
// treat the message as a regular chat message.
func (h *CommandHandler) Handle(ctx context.Context, activity *Activity) (bool, error) {
	text := activity.Text

	// Strip bot @-mention from the text.
	botID := ""
	if h.broker.config != nil {
		botID = h.broker.config.AppID
	}
	if botID != "" {
		if len(activity.Entities) > 0 {
			text = stripBotMentionByEntity(text, botID, activity.Entities)
		} else {
			text = stripBotMention(text, botID)
		}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return false, nil
	}

	parts := strings.Fields(text)
	command := strings.ToLower(parts[0])
	args := parts[1:]

	switch command {
	case "setup":
		return true, h.handleSetup(ctx, activity, args)
	case "unlink":
		return true, h.handleUnlink(ctx, activity)
	case "agents":
		return true, h.handleAgents(ctx, activity)
	case "status":
		return true, h.handleStatus(ctx, activity, args)
	case "register":
		return true, h.handleRegister(ctx, activity)
	case "unregister":
		return true, h.handleUnregister(ctx, activity)
	case "default":
		return true, h.handleDefault(ctx, activity, args)
	case "help":
		return true, h.handleHelp(ctx, activity)
	default:
		return false, nil
	}
}

// handleSetup links a Teams conversation to a Scion project.
// Usage: setup [project-slug]
func (h *CommandHandler) handleSetup(ctx context.Context, activity *Activity, args []string) error {
	conversationID := stripThreadSuffix(activity.Conversation.ID)

	// Check if already linked.
	store := h.getStore()
	if store != nil {
		existing, err := store.GetChannelLink(ctx, conversationID)
		if err != nil {
			h.log.Warn("Error checking existing channel link", "error", err)
		}
		if existing != nil {
			return h.sendReply(ctx, activity,
				fmt.Sprintf("This conversation is already linked to project **%s**. Use `unlink` first to change projects.", existing.ProjectSlug))
		}
	}

	// Check if user is registered.
	teamsUserID := activity.From.AadObjectID
	if teamsUserID == "" {
		teamsUserID = activity.From.ID
	}
	var mapping *TeamsUserMapping
	if store != nil {
		var err error
		mapping, err = store.GetUserMapping(ctx, teamsUserID)
		if err != nil {
			h.log.Warn("Error checking user mapping", "error", err)
		}
	}
	if mapping == nil {
		return h.sendReply(ctx, activity, "Please link your Teams account first with the `register` command.")
	}

	if len(args) > 0 {
		// Direct setup with project slug.
		projectSlug := args[0]
		return h.completeSetup(ctx, activity, projectSlug)
	}

	// Get projects - try user-scoped first, then fall back to broker endpoint.
	hubClient := h.broker.hubClient
	var projects []ProjectOption
	if hubClient != nil {
		if mapping.ScionUserID != "" {
			var err error
			projects, err = hubClient.ListProjectsForUser(ctx, mapping.ScionUserID)
			if err != nil {
				h.log.Warn("Failed to list user projects", "error", err, "user_id", mapping.ScionUserID)
			}
		}
		if len(projects) == 0 {
			var err error
			projects, err = hubClient.ListProjects(ctx)
			if err != nil {
				h.log.Warn("Failed to list projects from hub", "error", err)
			}
		}
	}

	if len(projects) == 0 {
		return h.sendReply(ctx, activity, "No projects found. Create a project in the hub first.")
	}

	// Build Adaptive Card with project buttons.
	card := NewAdaptiveCard()
	card.Body = append(card.Body,
		TextBlock{
			Type:   "TextBlock",
			Text:   "Link this conversation to a Scion project",
			Weight: "Bolder",
			Size:   "Medium",
		},
		TextBlock{
			Type:     "TextBlock",
			Text:     "Select a project to link this conversation.",
			Wrap:     true,
			IsSubtle: true,
		},
		TextBlock{
			Type:     "TextBlock",
			Text:     "Available projects:",
			Weight:   "Bolder",
			IsSubtle: true,
		},
	)

	// Add submit buttons for each project (up to 6 to keep card manageable).
	maxProjects := 6
	if len(projects) < maxProjects {
		maxProjects = len(projects)
	}
	for _, p := range projects[:maxProjects] {
		displayName := p.Slug
		if p.Name != "" && p.Name != p.Slug {
			displayName = fmt.Sprintf("%s (%s)", p.Name, p.Slug)
		}
		card.Actions = append(card.Actions, ActionExecute{
			Type:  "Action.Execute",
			Title: displayName,
			Data: map[string]string{
				"action":       "setup_confirm",
				"project_slug": p.Slug,
				"project_id":   p.ID,
			},
		})
	}
	return h.sendCardReply(ctx, activity, card)
}

// completeSetup finishes the setup process by creating the channel link.
func (h *CommandHandler) completeSetup(ctx context.Context, activity *Activity, projectSlug string) error {
	store := h.getStore()
	if store == nil {
		return h.sendReply(ctx, activity, "Setup failed: store not initialized.")
	}

	// Resolve project ID from slug via hub if possible.
	// TODO: Consider caching the project list or adding a GetProjectBySlug
	// hub endpoint to avoid the O(N) scan on every setup invocation.
	projectID := projectSlug
	hubClient := h.broker.hubClient
	if hubClient != nil {
		projects, err := hubClient.ListProjects(ctx)
		if err == nil {
			for _, p := range projects {
				if strings.EqualFold(p.Slug, projectSlug) || strings.EqualFold(p.Name, projectSlug) {
					projectID = p.ID
					projectSlug = p.Slug
					break
				}
			}
		}
	}

	// Extract team/channel info.
	teamID := ""
	if activity.ChannelData != nil {
		if activity.ChannelData.TeamsTeamID != "" {
			teamID = activity.ChannelData.TeamsTeamID
		} else if activity.ChannelData.Team != nil {
			teamID = activity.ChannelData.Team.ID
		}
	}

	linkedBy := activity.From.AadObjectID
	if linkedBy == "" {
		linkedBy = activity.From.ID
	}

	link := &ChannelLink{
		ConversationID:     stripThreadSuffix(activity.Conversation.ID),
		TeamID:             teamID,
		ProjectID:          projectID,
		ProjectSlug:        projectSlug,
		LinkedBy:           linkedBy,
		LinkedAt:           time.Now(),
		Active:             true,
		ShowAssistantReply: true,
	}

	if err := store.CreateChannelLink(ctx, link); err != nil {
		h.log.Error("Failed to create channel link", "error", err)
		return h.sendReply(ctx, activity, "Failed to link conversation. Please try again.")
	}

	// Send confirmation card.
	card := NewAdaptiveCard()
	card.Body = append(card.Body,
		TextBlock{
			Type:   "TextBlock",
			Text:   fmt.Sprintf("✅ Conversation linked to project **%s**", projectSlug),
			Weight: "Bolder",
			Wrap:   true,
		},
		TextBlock{
			Type:     "TextBlock",
			Text:     "Messages in this conversation will now be routed to agents in this project. Use `agents` to see available agents or `help` for all commands.",
			Wrap:     true,
			IsSubtle: true,
		},
	)

	return h.sendCardReply(ctx, activity, card)
}

// handleUnlink removes the channel link for the current conversation.
// Only the user who created the link is allowed to unlink it.
func (h *CommandHandler) handleUnlink(ctx context.Context, activity *Activity) error {
	store := h.getStore()
	if store == nil {
		return h.sendReply(ctx, activity, "Unlink failed: store not initialized.")
	}

	conversationID := stripThreadSuffix(activity.Conversation.ID)
	link, err := store.GetChannelLink(ctx, conversationID)
	if err != nil {
		h.log.Warn("Error looking up channel link", "error", err)
		return h.sendReply(ctx, activity, "An error occurred. Please try again.")
	}
	if link == nil {
		return h.sendReply(ctx, activity, "This conversation is not linked to any project.")
	}

	// Authorization: only the user who linked this channel can unlink it.
	if !isChannelAdmin(activity, link) {
		return h.sendReply(ctx, activity, "Only the user who linked this channel can unlink it.")
	}

	if err := store.DeleteChannelLink(ctx, conversationID); err != nil {
		h.log.Error("Failed to delete channel link", "error", err)
		return h.sendReply(ctx, activity, "Failed to unlink conversation. Please try again.")
	}

	return h.sendReply(ctx, activity, fmt.Sprintf("✅ Conversation unlinked from project **%s**.", link.ProjectSlug))
}

// isChannelAdmin checks whether the activity sender is the user who
// created the channel link. This can be extended later with Teams
// Graph API role checks.
func isChannelAdmin(activity *Activity, link *ChannelLink) bool {
	if link.LinkedBy == "" {
		// No recorded linker — allow anyone (backward compat).
		return true
	}
	userID := activity.From.AadObjectID
	if userID == "" {
		userID = activity.From.ID
	}
	return userID == link.LinkedBy
}

// handleAgents lists agents in the linked project.
func (h *CommandHandler) handleAgents(ctx context.Context, activity *Activity) error {
	link, err := h.resolveChannelLink(ctx, activity)
	if err != nil {
		return err
	}

	hubClient := h.broker.hubClient
	if hubClient == nil {
		return h.sendReply(ctx, activity, "Hub client not configured.")
	}

	agents, err := hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents from hub", "error", err, "project_id", link.ProjectID)
		return h.sendReply(ctx, activity, "Failed to retrieve agents. Please try again.")
	}

	// Cache agent slugs in store.
	store := h.getStore()
	if store != nil {
		slugs := make([]string, len(agents))
		for i, a := range agents {
			slugs[i] = a.Slug
		}
		_ = store.SetProjectAgents(ctx, &ProjectAgents{
			ProjectID:   link.ProjectID,
			AgentSlugs:  slugs,
			RefreshedAt: time.Now(),
		})
	}

	if len(agents) == 0 {
		return h.sendReply(ctx, activity, fmt.Sprintf("No agents found in project **%s**.", link.ProjectSlug))
	}

	// Build an Adaptive Card with agent info.
	card := NewAdaptiveCard()
	card.Body = append(card.Body, TextBlock{
		Type:   "TextBlock",
		Text:   fmt.Sprintf("Agents in **%s**", link.ProjectSlug),
		Weight: "Bolder",
		Size:   "Medium",
	})

	for _, agent := range agents {
		emoji := agentPhaseEmoji(agent.Phase)
		activityText := agent.Activity
		if activityText == "" {
			activityText = agent.Phase
		}
		if activityText == "" {
			activityText = "idle"
		}

		line := fmt.Sprintf("%s **%s** — %s", emoji, agent.Slug, activityText)
		if link.DefaultAgent == agent.Slug {
			line += " *(default)*"
		}

		card.Body = append(card.Body, TextBlock{
			Type: "TextBlock",
			Text: line,
			Wrap: true,
		})
	}

	return h.sendCardReply(ctx, activity, card)
}

// handleStatus shows project or agent status.
// Usage: status [agent-slug]
func (h *CommandHandler) handleStatus(ctx context.Context, activity *Activity, args []string) error {
	link, err := h.resolveChannelLink(ctx, activity)
	if err != nil {
		return err
	}

	hubClient := h.broker.hubClient
	if hubClient == nil {
		return h.sendReply(ctx, activity, "Hub client not configured.")
	}

	if len(args) > 0 {
		// Show specific agent status.
		agentSlug := args[0]
		return h.showAgentStatus(ctx, activity, link, agentSlug)
	}

	// Show project overview.
	agents, err := hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents for status", "error", err)
		return h.sendReply(ctx, activity, "Failed to retrieve project status. Please try again.")
	}

	card := NewAdaptiveCard()
	card.Body = append(card.Body, TextBlock{
		Type:   "TextBlock",
		Text:   fmt.Sprintf("Project **%s** Status", link.ProjectSlug),
		Weight: "Bolder",
		Size:   "Medium",
	})

	card.Body = append(card.Body, TextBlock{
		Type: "TextBlock",
		Text: fmt.Sprintf("**Agents:** %d", len(agents)),
	})

	if link.DefaultAgent != "" {
		card.Body = append(card.Body, TextBlock{
			Type: "TextBlock",
			Text: fmt.Sprintf("**Default agent:** %s", link.DefaultAgent),
		})
	}

	// Summarize agent phases (sorted for consistent output).
	phases := make(map[string]int)
	for _, a := range agents {
		phase := a.Phase
		if phase == "" {
			phase = "unknown"
		}
		phases[phase]++
	}
	phaseKeys := make([]string, 0, len(phases))
	for phase := range phases {
		phaseKeys = append(phaseKeys, phase)
	}
	sort.Strings(phaseKeys)
	for _, phase := range phaseKeys {
		card.Body = append(card.Body, TextBlock{
			Type:     "TextBlock",
			Text:     fmt.Sprintf("  %s %s: %d", agentPhaseEmoji(phase), phase, phases[phase]),
			IsSubtle: true,
		})
	}

	return h.sendCardReply(ctx, activity, card)
}

// showAgentStatus shows detailed status for a specific agent.
// TODO: Consider adding a GetAgent(ctx, projectID, slug) hub endpoint
// instead of re-fetching the full agent list for a single agent lookup.
func (h *CommandHandler) showAgentStatus(ctx context.Context, activity *Activity, link *ChannelLink, agentSlug string) error {
	hubClient := h.broker.hubClient
	agents, err := hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents for agent status", "error", err)
		return h.sendReply(ctx, activity, "Failed to retrieve agent status. Please try again.")
	}

	for _, agent := range agents {
		if strings.EqualFold(agent.Slug, agentSlug) {
			activityText := agent.Activity
			if activityText == "" {
				activityText = "idle"
			}

			card := NewAdaptiveCard()
			card.Body = append(card.Body,
				TextBlock{
					Type:   "TextBlock",
					Text:   fmt.Sprintf("%s Agent **%s**", agentPhaseEmoji(agent.Phase), agent.Slug),
					Weight: "Bolder",
					Size:   "Medium",
				},
				TextBlock{
					Type: "TextBlock",
					Text: fmt.Sprintf("**Phase:** %s", agent.Phase),
				},
				TextBlock{
					Type: "TextBlock",
					Text: fmt.Sprintf("**Activity:** %s", activityText),
					Wrap: true,
				},
			)

			if link.DefaultAgent == agent.Slug {
				card.Body = append(card.Body, TextBlock{
					Type:     "TextBlock",
					Text:     "*(default agent for this channel)*",
					IsSubtle: true,
				})
			}

			return h.sendCardReply(ctx, activity, card)
		}
	}

	return h.sendReply(ctx, activity, fmt.Sprintf("Agent **%s** not found in project **%s**.", agentSlug, link.ProjectSlug))
}

// handleRegister links the user's Teams identity to their Scion account via a
// code-based verification flow. The code is registered with the hub and
// displayed to the user via an Adaptive Card.
func (h *CommandHandler) handleRegister(ctx context.Context, activity *Activity) error {
	teamsUserID := activity.From.AadObjectID
	if teamsUserID == "" {
		teamsUserID = activity.From.ID
	}
	if teamsUserID == "" {
		return h.sendReply(ctx, activity, "Could not determine your Teams user ID. Please try again.")
	}

	// Check if already linked.
	store := h.getStore()
	if store != nil {
		existing, err := store.GetUserMapping(ctx, teamsUserID)
		if err != nil {
			h.log.Warn("Error checking existing user mapping", "error", err)
		}
		if existing != nil {
			return h.sendReply(ctx, activity,
				fmt.Sprintf("Your Teams account is already linked to Scion user **%s** (%s). Use `unregister` first to change.",
					existing.ScionUserID, existing.ScionEmail))
		}
	}

	hubClient := h.broker.hubClient
	if hubClient == nil {
		return h.sendReply(ctx, activity, "Hub client not configured. Cannot register identity.")
	}

	code, err := hubClient.RegisterTeamsLink(ctx, teamsUserID)
	if err != nil {
		h.log.Error("Failed to register identity link code with hub", "error", err)
		return h.sendReply(ctx, activity, "Failed to start identity linking. Please try again later.")
	}

	// Build a direct registration link so the user can click through.
	hubURL := strings.TrimRight(h.broker.config.HubURL, "/")
	linkURL := fmt.Sprintf("%s/profile/teams?code=%s", hubURL, code)

	// Send an Adaptive Card showing the code, instructions, and a clickable link.
	card := NewAdaptiveCard()
	card.Body = append(card.Body,
		TextBlock{
			Type:   "TextBlock",
			Text:   "Link Your Teams Account to Scion",
			Weight: "Bolder",
			Size:   "Medium",
		},
		TextBlock{
			Type:     "TextBlock",
			Text:     "Click the button below to link your account, or enter the code manually in the Scion web UI.",
			Wrap:     true,
			IsSubtle: true,
		},
		TextBlock{
			Type:                "TextBlock",
			Text:                code,
			Weight:              "Bolder",
			Size:                "ExtraLarge",
			HorizontalAlignment: "Center",
		},
		TextBlock{
			Type:     "TextBlock",
			Text:     "This code expires in 15 minutes.",
			Wrap:     true,
			IsSubtle: true,
		},
	)
	card.Actions = append(card.Actions, ActionOpenURL{
		Type:  "Action.OpenUrl",
		Title: "Link Account",
		URL:   linkURL,
	})

	if err := h.sendCardReply(ctx, activity, card); err != nil {
		return err
	}

	// Cancel any existing pending link for this user before starting a new one.
	h.pendingMu.Lock()
	if old, ok := h.pendingLinks[teamsUserID]; ok && old.Cancel != nil {
		old.Cancel()
	}

	pollCtx, pollCancel := context.WithCancel(context.Background())
	h.pendingLinks[teamsUserID] = &pendingTeamsLink{
		Code:        code,
		TeamsUserID: teamsUserID,
		Activity:    activity,
		Cancel:      pollCancel,
		ExpiresAt:   time.Now().Add(15 * time.Minute),
	}
	h.pendingMu.Unlock()

	go h.pollForConfirmation(pollCtx, activity, teamsUserID, code, pollCancel)

	return nil
}

// pollForConfirmation polls the hub in the background until the identity link
// is confirmed or the code expires (15 minutes). On confirmation it saves the
// user mapping and sends a reply to the conversation.
func (h *CommandHandler) pollForConfirmation(ctx context.Context, activity *Activity, teamsUserID, code string, cancel context.CancelFunc) {
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	deadline := time.Now().Add(15 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if t.After(deadline) {
				// Clean up expired pending link.
				h.pendingMu.Lock()
				if cur, ok := h.pendingLinks[teamsUserID]; ok && cur.Code == code {
					delete(h.pendingLinks, teamsUserID)
				}
				h.pendingMu.Unlock()
				return
			}

			checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
			status, userID, email, err := h.broker.hubClient.CheckTeamsLinkStatus(checkCtx, teamsUserID)
			checkCancel()

			if err != nil {
				h.log.Debug("Poll check failed", "error", err, "teams_user_id", teamsUserID)
				continue
			}

			if status == "confirmed" && userID != "" {
				// Save user mapping.
				store := h.getStore()

				if store != nil {
					mapping := &TeamsUserMapping{
						TeamsUserID:      teamsUserID,
						TeamsDisplayName: activity.From.Name,
						ScionUserID:      userID,
						ScionEmail:       email,
						LinkedAt:         time.Now(),
					}
					if err := store.CreateUserMapping(ctx, mapping); err != nil {
						h.log.Error("Failed to save user mapping", "error", err)
					}
				}

				// Send confirmation reply.
				replyCtx, replyCancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = h.sendReply(replyCtx, activity, fmt.Sprintf("Linked! Your Teams account is now connected to Scion user **%s**.", email))
				replyCancel()

				// Clean up pending link.
				h.pendingMu.Lock()
				if cur, ok := h.pendingLinks[teamsUserID]; ok && cur.Code == code {
					delete(h.pendingLinks, teamsUserID)
				}
				h.pendingMu.Unlock()
				return
			}
		}
	}
}

// handleUnregister removes the user's Teams-to-Scion identity link.
func (h *CommandHandler) handleUnregister(ctx context.Context, activity *Activity) error {
	teamsUserID := activity.From.AadObjectID
	if teamsUserID == "" {
		teamsUserID = activity.From.ID
	}
	if teamsUserID == "" {
		return h.sendReply(ctx, activity, "Could not determine your Teams user ID. Please try again.")
	}

	store := h.getStore()
	if store == nil {
		return h.sendReply(ctx, activity, "Store not initialized. Cannot unregister.")
	}

	existing, err := store.GetUserMapping(ctx, teamsUserID)
	if err != nil {
		h.log.Warn("Error looking up user mapping", "error", err)
		return h.sendReply(ctx, activity, "An error occurred. Please try again.")
	}
	if existing == nil {
		return h.sendReply(ctx, activity, "Your Teams account is not linked to any Scion account.")
	}

	if err := store.DeleteUserMapping(ctx, teamsUserID); err != nil {
		h.log.Error("Failed to delete user mapping", "error", err)
		return h.sendReply(ctx, activity, "Failed to unlink your account. Please try again.")
	}

	return h.sendReply(ctx, activity,
		fmt.Sprintf("✅ Your Teams account has been unlinked from Scion user **%s** (%s).",
			existing.ScionUserID, existing.ScionEmail))
}

// handleDefault sets or shows the default agent for the current channel.
func (h *CommandHandler) handleDefault(ctx context.Context, activity *Activity, args []string) error {
	store := h.getStore()
	if store == nil {
		return h.sendReply(ctx, activity, "Store not initialized.")
	}

	// Resolve channel link.
	link, err := h.resolveChannelLink(ctx, activity)
	if err != nil {
		return nil // resolveChannelLink already sent reply
	}

	if len(args) == 0 {
		// Show current default.
		if link.DefaultAgent == "" {
			return h.sendReply(ctx, activity,
				fmt.Sprintf("No default agent set for this channel (project: **%s**). Use `default <agent-slug>` to set one.", link.ProjectSlug))
		}
		return h.sendReply(ctx, activity,
			fmt.Sprintf("Default agent for this channel: **%s** (project: **%s**). Use `default clear` to remove.", link.DefaultAgent, link.ProjectSlug))
	}

	agentSlug := args[0]

	if agentSlug == "clear" || agentSlug == "none" || agentSlug == "remove" {
		// Clear default.
		link.DefaultAgent = ""
		if err := store.UpdateChannelLink(ctx, link); err != nil {
			h.log.Error("Failed to clear default agent", "error", err)
			return h.sendReply(ctx, activity, "Failed to clear default agent.")
		}
		return h.sendReply(ctx, activity, "Default agent cleared for this channel.")
	}

	// Set default agent — validate it exists.
	hubClient := h.broker.hubClient
	if hubClient == nil {
		return h.sendReply(ctx, activity, "Hub client not configured.")
	}

	agents, err := hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents for validation", "error", err)
		return h.sendReply(ctx, activity, "Failed to validate agent. Please try again.")
	}

	var found bool
	for _, a := range agents {
		if strings.EqualFold(a.Slug, agentSlug) {
			agentSlug = a.Slug // normalize to actual slug
			found = true
			break
		}
	}
	if !found {
		return h.sendReply(ctx, activity, fmt.Sprintf("Agent **%s** not found in project **%s**.", agentSlug, link.ProjectSlug))
	}

	link.DefaultAgent = agentSlug
	if err := store.UpdateChannelLink(ctx, link); err != nil {
		h.log.Error("Failed to set default agent", "error", err)
		return h.sendReply(ctx, activity, "Failed to set default agent.")
	}
	return h.sendReply(ctx, activity,
		fmt.Sprintf("Default agent set to **%s** for this channel.", agentSlug))
}

// handleHelp sends a card listing all available commands.
func (h *CommandHandler) handleHelp(ctx context.Context, activity *Activity) error {
	card := NewAdaptiveCard()
	card.Body = append(card.Body, TextBlock{
		Type:   "TextBlock",
		Text:   "Scion Bot Commands",
		Weight: "Bolder",
		Size:   "Medium",
	})

	commands := []struct {
		name string
		desc string
	}{
		{"setup [project]", "Link this conversation to a Scion project"},
		{"unlink", "Unlink this conversation from its project"},
		{"agents", "List agents in the linked project"},
		{"status [agent]", "Show project or agent status"},
		{"default [agent]", "Set or show the default agent for this channel"},
		{"register", "Link your Teams account to Scion"},
		{"unregister", "Unlink your Teams account from Scion"},
		{"help", "Show this help message"},
	}

	for _, cmd := range commands {
		card.Body = append(card.Body, TextBlock{
			Type: "TextBlock",
			Text: fmt.Sprintf("**%s** — %s", cmd.name, cmd.desc),
			Wrap: true,
		})
	}

	return h.sendCardReply(ctx, activity, card)
}

// getStore returns the broker's store under the broker mutex.
func (h *CommandHandler) getStore() Store {
	h.broker.mu.Lock()
	store := h.broker.store
	h.broker.mu.Unlock()
	return store
}

// --- Helpers ---

// resolveChannelLink looks up the channel link for the current conversation.
// Returns an error (with a reply sent to the user) if not linked.
func (h *CommandHandler) resolveChannelLink(ctx context.Context, activity *Activity) (*ChannelLink, error) {
	store := h.getStore()
	if store == nil {
		_ = h.sendReply(ctx, activity, "Store not initialized.")
		return nil, fmt.Errorf("store not initialized")
	}

	link, err := store.GetChannelLink(ctx, stripThreadSuffix(activity.Conversation.ID))
	if err != nil {
		h.log.Warn("Error looking up channel link", "error", err)
		_ = h.sendReply(ctx, activity, "An error occurred. Please try again.")
		return nil, fmt.Errorf("lookup channel link: %w", err)
	}
	if link == nil {
		_ = h.sendReply(ctx, activity, "This conversation is not linked to a project. Run `setup <project-slug>` first.")
		return nil, fmt.Errorf("no channel link for conversation %s", activity.Conversation.ID)
	}

	return link, nil
}

// sendReply sends a plain-text reply to the activity's conversation.
func (h *CommandHandler) sendReply(ctx context.Context, activity *Activity, text string) error {
	reply := &Activity{
		Type: "message",
		Text: text,
	}

	sender := h.broker.sender
	if sender == nil {
		h.log.Warn("Sender not initialized, cannot send reply")
		return fmt.Errorf("sender not initialized")
	}

	_, err := sender.sendActivity(ctx, activity.ServiceURL, activity.Conversation.ID, reply)
	if err != nil {
		h.log.Error("Failed to send command reply", "error", err)
	}
	return err
}

// sendCardReply sends an Adaptive Card reply to the activity's conversation.
func (h *CommandHandler) sendCardReply(ctx context.Context, activity *Activity, card *AdaptiveCard) error {
	cardJSON, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}

	reply := &Activity{
		Type: "message",
		Attachments: []Attachment{
			{
				ContentType: adaptiveCardContentType,
				Content:     json.RawMessage(cardJSON),
			},
		},
	}

	sender := h.broker.sender
	if sender == nil {
		h.log.Warn("Sender not initialized, cannot send card reply")
		return fmt.Errorf("sender not initialized")
	}

	_, err = sender.sendActivity(ctx, activity.ServiceURL, activity.Conversation.ID, reply)
	if err != nil {
		h.log.Error("Failed to send card reply", "error", err)
	}
	return err
}

// agentPhaseEmoji returns a status emoji for an agent's phase.
func agentPhaseEmoji(phase string) string {
	switch strings.ToLower(phase) {
	case "running":
		return "🟢"
	case "starting":
		return "🟡"
	case "stopping", "stopped":
		return "🔴"
	case "waiting", "waiting_for_input":
		return "🟠"
	case "error":
		return "❌"
	case "completed":
		return "✅"
	default:
		return "⚪"
	}
}
