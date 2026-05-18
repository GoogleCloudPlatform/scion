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

package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AskUserResponse carries the user's response to an InputNeeded callback
// back to the broker for delivery to the hub.
type AskUserResponse struct {
	RequestID string
	AgentSlug string
	ProjectID string
	ChatID    int64
	Response  string
}

// CallbackHandler processes inline keyboard button presses (callback queries).
type CallbackHandler struct {
	store     Store
	api       *TelegramAPIClient
	hubClient HubClient
	log       *slog.Logger

	mu             sync.Mutex
	pendingSetups  map[int64]*pendingSetup // chatID → setup state
	cachedProjects []ProjectOption         // hub-injected project list
}

// SetProjects updates the cached project list used by the "change project" flow.
func (h *CallbackHandler) SetProjects(projects []ProjectOption) {
	h.mu.Lock()
	h.cachedProjects = projects
	h.mu.Unlock()
}

// pendingSetup tracks the multi-step /setup flow between project and agent selection.
type pendingSetup struct {
	projectID   string
	projectSlug string
	createdAt   time.Time
}

// NewCallbackHandler creates a new CallbackHandler.
func NewCallbackHandler(store Store, api *TelegramAPIClient, hubClient HubClient, log *slog.Logger) *CallbackHandler {
	if log == nil {
		log = slog.Default()
	}
	return &CallbackHandler{
		store:         store,
		api:           api,
		hubClient:     hubClient,
		log:           log,
		pendingSetups: make(map[int64]*pendingSetup),
	}
}

// HandleCallback processes a callback query from an inline keyboard button press.
// Callback data format: <action>:<entity>[:<extra>]
// Returns an AskUserResponse if the callback was an ask-user response that
// needs to be delivered to the hub.
func (h *CallbackHandler) HandleCallback(ctx context.Context, cb *CallbackQuery) (*AskUserResponse, error) {
	if cb == nil || cb.Data == "" {
		return nil, nil
	}

	parts := strings.SplitN(cb.Data, ":", 4)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid callback data: %s", cb.Data)
	}

	switch parts[0] {
	case "setup":
		return nil, h.handleSetupCallback(ctx, cb, parts[1:])
	case "dflt":
		return nil, h.handleDefaultCallback(ctx, cb, parts[1:])
	case "ask":
		return h.handleAskCallback(ctx, cb, parts[1:])
	case "settings":
		return nil, h.handleSettingsCallback(ctx, cb, parts[1:])
	case "notify":
		return nil, h.handleNotifyCallback(ctx, cb, parts[1:])
	default:
		return nil, fmt.Errorf("unknown callback action: %s", parts[0])
	}
}

func (h *CallbackHandler) handleSetupCallback(ctx context.Context, cb *CallbackQuery, parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("missing setup sub-action")
	}

	chatID := int64(0)
	if cb.Message != nil {
		chatID = cb.Message.Chat.ID
	}
	messageID := int64(0)
	if cb.Message != nil {
		messageID = cb.Message.MessageID
	}

	switch parts[0] {
	case "proj":
		if len(parts) < 2 {
			return fmt.Errorf("missing project ID in callback")
		}
		return h.handleSetupProject(ctx, cb, chatID, messageID, parts[1])

	case "dflt":
		if len(parts) < 2 {
			return fmt.Errorf("missing agent slug in callback")
		}
		return h.handleSetupDefaultAgent(ctx, cb, chatID, messageID, parts[1])

	case "change":
		return h.handleSetupChange(ctx, cb, chatID, messageID)

	case "keep":
		return h.handleSetupKeep(ctx, cb, chatID, messageID)

	case "cancel":
		return h.handleSetupCancel(ctx, cb, chatID, messageID)

	case "unlink":
		return h.handleSetupUnlink(ctx, cb, chatID, messageID)

	default:
		return fmt.Errorf("unknown setup sub-action: %s", parts[0])
	}
}

func (h *CallbackHandler) handleSetupProject(ctx context.Context, cb *CallbackQuery, chatID, messageID int64, projectID string) error {
	agentInfos, err := h.hubClient.ListAgents(ctx, projectID)
	if err != nil {
		h.log.Error("Failed to list agents for project", "project_id", projectID, "error", err)
		h.answerCallback(ctx, cb.ID, "Failed to fetch agents. Try again.", false)
		return err
	}
	agents := agentSlugs(agentInfos)

	// Look up the project slug from cached list or fresh hub fetch.
	projectSlug := projectID
	h.mu.Lock()
	cached := h.cachedProjects
	h.mu.Unlock()
	for _, p := range cached {
		if p.ID == projectID {
			projectSlug = p.DisplayName()
			break
		}
	}
	if projectSlug == projectID {
		// Not in cache — try fresh fetch
		if fresh, err := h.hubClient.ListProjectsFresh(ctx); err == nil {
			for _, p := range fresh {
				if p.ID == projectID {
					projectSlug = p.DisplayName()
					break
				}
			}
		}
	}

	h.mu.Lock()
	h.pendingSetups[chatID] = &pendingSetup{
		projectID:   projectID,
		projectSlug: projectSlug,
		createdAt:   time.Now(),
	}
	h.mu.Unlock()

	if len(agents) == 0 {
		// No agents: create the link with no default agent.
		return h.finishSetup(ctx, cb, chatID, messageID, projectID, projectSlug, "", "")
	}

	kb := buildAgentSelectionKeyboard(agents, "")
	h.editMessage(ctx, chatID, messageID,
		fmt.Sprintf("Project *%s* selected.\nChoose a default agent:\nAny plain message (without a / command or @mention) will be sent to the default agent. Mention a specific agent by name to route there instead.", projectSlug), kb)
	h.answerCallback(ctx, cb.ID, "", false)
	return nil
}

func (h *CallbackHandler) handleSetupDefaultAgent(ctx context.Context, cb *CallbackQuery, chatID, messageID int64, agentSlug string) error {
	h.mu.Lock()
	setup := h.pendingSetups[chatID]
	delete(h.pendingSetups, chatID)
	h.mu.Unlock()

	if setup == nil {
		h.answerCallback(ctx, cb.ID, "Setup session expired. Use /setup again.", false)
		return nil
	}

	return h.finishSetup(ctx, cb, chatID, messageID, setup.projectID, setup.projectSlug, agentSlug, "")
}

func (h *CallbackHandler) finishSetup(ctx context.Context, cb *CallbackQuery, chatID, messageID int64, projectID, projectSlug, agentSlug, chatTitle string) error {
	linkedBy := ""
	if cb.From != nil {
		linkedBy = strconv.FormatInt(cb.From.ID, 10)
	}

	if chatTitle == "" {
		chat, err := h.api.GetChat(ctx, chatID)
		if err == nil && chat.Title != "" {
			chatTitle = chat.Title
		}
	}

	link := &GroupLink{
		ChatID:       chatID,
		ChatTitle:    chatTitle,
		ProjectID:    projectID,
		ProjectSlug:  projectSlug,
		DefaultAgent: agentSlug,
		LinkedBy:     linkedBy,
		LinkedAt:     time.Now(),
		Active:       true,
	}

	if err := h.store.SaveGroupLink(ctx, link); err != nil {
		h.log.Error("Failed to save group link", "chat_id", chatID, "error", err)
		h.answerCallback(ctx, cb.ID, "Failed to save configuration.", false)
		return err
	}

	text := fmt.Sprintf("Group linked to project *%s*.", projectSlug)
	if agentSlug != "" {
		text += fmt.Sprintf("\nDefault agent: @%s", agentSlug)
	}

	h.editMessage(ctx, chatID, messageID, text, nil)
	h.answerCallback(ctx, cb.ID, "Setup complete!", false)
	return nil
}

func (h *CallbackHandler) handleSetupChange(ctx context.Context, cb *CallbackQuery, chatID, messageID int64) error {
	fresh, freshErr := h.hubClient.ListProjectsFresh(ctx)
	var projects []ProjectOption
	if freshErr == nil && len(fresh) > 0 {
		projects = fresh
		h.mu.Lock()
		h.cachedProjects = fresh
		h.mu.Unlock()
		h.log.Debug("Using fresh project list for setup change", "count", len(projects))
	} else {
		if freshErr != nil {
			h.log.Warn("Failed to fetch fresh projects, falling back", "error", freshErr)
		}
		h.mu.Lock()
		projects = h.cachedProjects
		h.mu.Unlock()
	}

	if len(projects) == 0 {
		h.editMessage(ctx, chatID, messageID, "No projects found. Please /register first.", nil)
		h.answerCallback(ctx, cb.ID, "", false)
		return nil
	}

	kb := buildProjectSelectionKeyboard(projects)
	h.editMessage(ctx, chatID, messageID, "Select a project to link this group to:", kb)
	h.answerCallback(ctx, cb.ID, "", false)
	return nil
}

func (h *CallbackHandler) handleSetupKeep(ctx context.Context, cb *CallbackQuery, chatID, messageID int64) error {
	h.editMessage(ctx, chatID, messageID, "Keeping current configuration.", nil)
	h.answerCallback(ctx, cb.ID, "Configuration kept.", false)
	return nil
}

func (h *CallbackHandler) handleSetupCancel(ctx context.Context, cb *CallbackQuery, chatID, messageID int64) error {
	h.mu.Lock()
	delete(h.pendingSetups, chatID)
	h.mu.Unlock()

	h.editMessage(ctx, chatID, messageID, "Setup cancelled.", nil)
	h.answerCallback(ctx, cb.ID, "", false)
	return nil
}

func (h *CallbackHandler) handleSetupUnlink(ctx context.Context, cb *CallbackQuery, chatID, messageID int64) error {
	if err := h.store.DeleteGroupLink(ctx, chatID); err != nil {
		h.log.Error("Failed to unlink group", "chat_id", chatID, "error", err)
		h.answerCallback(ctx, cb.ID, "Failed to unlink.", false)
		return err
	}
	h.editMessage(ctx, chatID, messageID, "This group has been unlinked from the scion hub. Use /setup to link it again.", nil)
	h.answerCallback(ctx, cb.ID, "Group unlinked.", false)
	return nil
}

func (h *CallbackHandler) handleDefaultCallback(ctx context.Context, cb *CallbackQuery, parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("missing agent slug")
	}

	agentSlug := parts[0]
	chatID := int64(0)
	messageID := int64(0)
	if cb.Message != nil {
		chatID = cb.Message.Chat.ID
		messageID = cb.Message.MessageID
	}

	link, err := h.store.GetGroupLink(ctx, chatID)
	if err != nil || link == nil {
		h.answerCallback(ctx, cb.ID, "Group is not linked to a project.", false)
		return err
	}

	if agentSlug == "__none__" {
		link.DefaultAgent = ""
	} else {
		link.DefaultAgent = agentSlug
	}
	if err := h.store.SaveGroupLink(ctx, link); err != nil {
		h.log.Error("Failed to update default agent", "chat_id", chatID, "error", err)
		h.answerCallback(ctx, cb.ID, "Failed to update default agent.", false)
		return err
	}

	if agentSlug == "__none__" {
		h.editMessage(ctx, chatID, messageID, "Default agent removed.", nil)
		h.answerCallback(ctx, cb.ID, "Default: none", false)
	} else {
		h.editMessage(ctx, chatID, messageID,
			fmt.Sprintf("Default agent set to @%s.", agentSlug), nil)
		h.answerCallback(ctx, cb.ID, fmt.Sprintf("Default: @%s", agentSlug), false)
	}
	return nil
}

func (h *CallbackHandler) handleAskCallback(ctx context.Context, cb *CallbackQuery, parts []string) (*AskUserResponse, error) {
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid ask callback data")
	}

	action := parts[0]
	requestID := parts[1]

	pending, err := h.store.GetPendingAskUser(ctx, requestID)
	if err != nil {
		h.log.Error("Failed to get pending ask user", "request_id", requestID, "error", err)
		h.answerCallback(ctx, cb.ID, "Something went wrong.", false)
		return nil, err
	}

	if pending == nil || pending.Responded || time.Now().After(pending.ExpiresAt) {
		h.answerCallback(ctx, cb.ID, "This request has expired.", false)
		return nil, nil
	}

	var responseText string
	switch action {
	case "yes":
		responseText = "Yes"
	case "no":
		responseText = "No"
	case "opt":
		if len(parts) < 3 {
			return nil, fmt.Errorf("missing choice index")
		}
		idx, parseErr := strconv.Atoi(parts[2])
		if parseErr != nil || idx < 0 || idx >= len(pending.Choices) {
			h.answerCallback(ctx, cb.ID, "Invalid choice.", false)
			return nil, fmt.Errorf("invalid choice index: %s", parts[2])
		}
		responseText = pending.Choices[idx]
	default:
		return nil, fmt.Errorf("unknown ask action: %s", action)
	}

	if err := h.store.MarkPendingAskUserResponded(ctx, requestID); err != nil {
		h.log.Error("Failed to mark ask user as responded", "request_id", requestID, "error", err)
	}

	// Remove the inline keyboard from the message.
	if cb.Message != nil {
		h.editMarkup(ctx, pending.ChatID, pending.MessageID, nil)
	}

	h.answerCallback(ctx, cb.ID, fmt.Sprintf("Answered: %s", responseText), false)

	return &AskUserResponse{
		RequestID: requestID,
		AgentSlug: pending.AgentSlug,
		ProjectID: pending.ProjectID,
		ChatID:    pending.ChatID,
		Response:  responseText,
	}, nil
}

func (h *CallbackHandler) handleSettingsCallback(ctx context.Context, cb *CallbackQuery, parts []string) error {
	if len(parts) < 2 {
		return fmt.Errorf("invalid settings callback data")
	}

	setting := parts[0]
	value := parts[1]

	chatID := int64(0)
	messageID := int64(0)
	if cb.Message != nil {
		chatID = cb.Message.Chat.ID
		messageID = cb.Message.MessageID
	}

	link, err := h.store.GetGroupLink(ctx, chatID)
	if err != nil || link == nil {
		h.answerCallback(ctx, cb.ID, "Group is not linked to a project.", false)
		return err
	}

	switch setting {
	case "a2a":
		switch value {
		case "on":
			link.ShowAgentToAgent = true
		case "off":
			link.ShowAgentToAgent = false
		default:
			return fmt.Errorf("invalid a2a value: %s", value)
		}
	case "commentary":
		switch value {
		case "on":
			link.ShowAssistantReply = true
		case "off":
			link.ShowAssistantReply = false
		default:
			return fmt.Errorf("invalid commentary value: %s", value)
		}
	case "grp":
		switch value {
		case "on":
			link.NotifyInGroup = true
		case "off":
			link.NotifyInGroup = false
		default:
			return fmt.Errorf("invalid grp value: %s", value)
		}
	default:
		return fmt.Errorf("unknown setting: %s", setting)
	}

	if err := h.store.SaveGroupLink(ctx, link); err != nil {
		h.log.Error("Failed to update settings", "chat_id", chatID, "error", err)
		h.answerCallback(ctx, cb.ID, "Failed to update setting.", false)
		return err
	}

	kb := buildSettingsKeyboard(link.ShowAgentToAgent, link.NotifyInGroup, link.ShowAssistantReply)
	h.editMarkup(ctx, chatID, messageID, kb)

	var toastMsg string
	switch setting {
	case "a2a":
		label := "off"
		if link.ShowAgentToAgent {
			label = "on"
		}
		toastMsg = fmt.Sprintf("Observer mode: %s", label)
	case "commentary":
		label := "off"
		if link.ShowAssistantReply {
			label = "on"
		}
		toastMsg = fmt.Sprintf("Commentary: %s", label)
	case "grp":
		label := "off"
		if link.NotifyInGroup {
			label = "on"
		}
		toastMsg = fmt.Sprintf("Group notifications: %s", label)
	}
	h.answerCallback(ctx, cb.ID, toastMsg, false)
	return nil
}

func (h *CallbackHandler) handleNotifyCallback(ctx context.Context, cb *CallbackQuery, parts []string) error {
	if len(parts) < 2 {
		return fmt.Errorf("invalid notify callback data")
	}

	projectID := parts[0]
	agentSlug := parts[1]

	chatID := int64(0)
	messageID := int64(0)
	if cb.Message != nil {
		chatID = cb.Message.Chat.ID
		messageID = cb.Message.MessageID
	}

	senderID := ""
	if cb.From != nil {
		senderID = strconv.FormatInt(cb.From.ID, 10)
	}
	if senderID == "" {
		h.answerCallback(ctx, cb.ID, "Could not identify user.", false)
		return nil
	}

	existing, err := h.store.GetNotificationPref(ctx, senderID, projectID, agentSlug)
	if err != nil {
		h.log.Error("Failed to get notification pref", "error", err)
		h.answerCallback(ctx, cb.ID, "Something went wrong.", false)
		return err
	}

	newEnabled := false
	if existing == nil {
		newEnabled = false
	} else {
		newEnabled = !existing.Enabled
	}

	if err := h.store.SaveNotificationPref(ctx, &NotificationPref{
		TelegramUserID: senderID,
		ProjectID:      projectID,
		AgentSlug:      agentSlug,
		Enabled:        newEnabled,
	}); err != nil {
		h.log.Error("Failed to save notification pref", "error", err)
		h.answerCallback(ctx, cb.ID, "Failed to update.", false)
		return err
	}

	allPrefs, err := h.store.GetNotificationPrefs(ctx, senderID)
	if err != nil {
		h.log.Error("Failed to reload notification prefs", "error", err)
		h.answerCallback(ctx, cb.ID, "Updated but failed to refresh.", false)
		return err
	}
	prefMap := make(map[string]bool)
	for _, p := range allPrefs {
		prefMap[p.ProjectID+":"+p.AgentSlug] = p.Enabled
	}

	links, err := h.store.GetAllGroupLinks(ctx)
	if err != nil {
		h.log.Error("Failed to get group links", "error", err)
		h.answerCallback(ctx, cb.ID, "Updated but failed to refresh.", false)
		return err
	}

	seen := make(map[string]bool)
	var entries []notificationAgentEntry
	for _, link := range links {
		if !link.Active || seen[link.ProjectID] {
			continue
		}
		seen[link.ProjectID] = true

		cached, _ := h.store.GetProjectAgents(ctx, link.ProjectID)
		if cached == nil {
			continue
		}
		for _, agent := range cached.Agents {
			enabled := true
			if val, ok := prefMap[link.ProjectID+":"+agent.Slug]; ok {
				enabled = val
			}
			entries = append(entries, notificationAgentEntry{
				ProjectSlug: link.ProjectSlug,
				ProjectID:   link.ProjectID,
				AgentSlug:   agent.Slug,
				Enabled:     enabled,
			})
		}
	}

	kb := buildNotificationsKeyboard(entries)
	h.editMarkup(ctx, chatID, messageID, kb)

	label := "off"
	if newEnabled {
		label = "on"
	}
	h.answerCallback(ctx, cb.ID, fmt.Sprintf("%s notifications: %s", agentSlug, label), false)
	return nil
}

func (h *CallbackHandler) answerCallback(ctx context.Context, callbackID, text string, showAlert bool) {
	if err := h.api.AnswerCallbackQuery(ctx, callbackID, text, showAlert); err != nil {
		h.log.Error("Failed to answer callback query", "error", err)
	}
}

func (h *CallbackHandler) editMessage(ctx context.Context, chatID, messageID int64, text string, kb *InlineKeyboardMarkup) {
	if _, err := h.api.EditMessageText(ctx, chatID, messageID, text, "", kb); err != nil {
		h.log.Error("Failed to edit message", "chat_id", chatID, "message_id", messageID, "error", err)
	}
}

func (h *CallbackHandler) editMarkup(ctx context.Context, chatID, messageID int64, kb *InlineKeyboardMarkup) {
	if _, err := h.api.EditMessageReplyMarkup(ctx, chatID, messageID, kb); err != nil {
		h.log.Error("Failed to edit message markup", "chat_id", chatID, "message_id", messageID, "error", err)
	}
}
