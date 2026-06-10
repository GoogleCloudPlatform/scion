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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// AgentInfo holds an agent's slug and current activity state.
type AgentInfo struct {
	Slug     string `json:"slug"`
	Activity string `json:"activity,omitempty"`
}

// HubClient provides access to the Scion hub API for project and agent listing.
type HubClient interface {
	ListProjects(ctx context.Context) ([]ProjectOption, error)
	ListProjectsFresh(ctx context.Context) ([]ProjectOption, error)
	ListProjectsForUser(ctx context.Context, ownerID string) ([]ProjectOption, error)
	ListAgents(ctx context.Context, projectID string) ([]AgentInfo, error)
}

// CommandHandler processes bot commands from incoming Telegram messages.
type CommandHandler struct {
	store          Store
	api            *TelegramAPIClient
	hubClient      HubClient
	botUsername    string
	log            *slog.Logger
	cachedProjects []ProjectOption
}

// NewCommandHandler creates a new CommandHandler.
func NewCommandHandler(store Store, api *TelegramAPIClient, hubClient HubClient, botUsername string, log *slog.Logger) *CommandHandler {
	if log == nil {
		log = slog.Default()
	}
	return &CommandHandler{
		store:       store,
		api:         api,
		hubClient:   hubClient,
		botUsername: botUsername,
		log:         log,
	}
}

// SetProjects updates the cached project list used by /setup.
func (h *CommandHandler) SetProjects(projects []ProjectOption) {
	h.cachedProjects = projects
}

// HandleCommand dispatches an incoming message to the appropriate command
// handler based on the command text. Returns true if the message was a
// recognized command (even if it failed).
func (h *CommandHandler) HandleCommand(msg *TGMessage) bool {
	if msg == nil || !strings.HasPrefix(msg.Text, "/") {
		return false
	}

	text := strings.TrimSpace(msg.Text)
	cmd := text
	if idx := strings.Index(cmd, " "); idx != -1 {
		cmd = cmd[:idx]
	}
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/setup":
		h.handleSetup(msg)
		return true
	case "/default":
		h.handleDefault(msg)
		return true
	case "/agents":
		h.handleAgents(msg)
		return true
	case "/unlink":
		h.handleUnlink(msg)
		return true
	case "/help":
		h.handleHelp(msg)
		return true
	case "/status":
		h.handleStatus(msg)
		return true
	case "/settings":
		h.handleSettings(msg)
		return true
	case "/notifications":
		h.handleNotifications(msg)
		return true
	default:
		return false
	}
}

func (h *CommandHandler) handleSetup(msg *TGMessage) {
	chatID := msg.Chat.ID

	if !isGroupChat(chatID) {
		h.reply(chatID, "Use /setup in a group chat.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetGroupLink(ctx, chatID)
	if err != nil {
		h.log.Error("Failed to get group link", "chat_id", chatID, "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}

	if link != nil {
		kb := buildSetupConfirmKeyboard(link.ProjectSlug)
		h.replyWithKeyboard(chatID, fmt.Sprintf("This group is already linked to project *%s*.\nWould you like to keep or change it?", link.ProjectSlug), kb)
		return
	}

	var projects []ProjectOption
	promptText := "Select a project to link this group to:"

	senderID := ""
	if msg.From != nil {
		senderID = strconv.FormatInt(msg.From.ID, 10)
	}

	if senderID != "" {
		mapping, mapErr := h.store.GetUserMapping(ctx, senderID)
		if mapErr != nil {
			h.log.Warn("Failed to check user mapping for /setup filtering", "error", mapErr)
		}
		if mapping != nil && mapping.ScionUserID != "" {
			userProjects, userErr := h.hubClient.ListProjectsForUser(ctx, mapping.ScionUserID)
			if userErr != nil {
				h.log.Warn("Failed to list user projects, falling back to all", "error", userErr)
			} else if len(userProjects) > 0 {
				projects = userProjects
				h.log.Debug("Using user-filtered project list for /setup", "user_id", mapping.ScionUserID, "count", len(projects))
			}
		}
	}

	if len(projects) == 0 {
		fresh, freshErr := h.hubClient.ListProjectsFresh(ctx)
		if freshErr == nil && len(fresh) > 0 {
			projects = fresh
			h.cachedProjects = fresh
			h.log.Debug("Using fresh project list from hub for /setup", "count", len(projects))
		} else {
			if freshErr != nil {
				h.log.Warn("Failed to fetch fresh projects, falling back", "error", freshErr)
			}
			if len(h.cachedProjects) > 0 {
				projects = h.cachedProjects
				h.log.Debug("Using cached project list for /setup", "count", len(projects))
			}
		}
	}

	if len(projects) == 0 {
		h.reply(chatID, "No projects found. Create a project in the hub first.")
		return
	}

	kb := buildProjectSelectionKeyboard(projects)
	h.replyWithKeyboard(chatID, promptText, kb)
}

func (h *CommandHandler) handleDefault(msg *TGMessage) {
	chatID := msg.Chat.ID
	threadID := msg.MessageThreadID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetGroupLink(ctx, chatID)
	if err != nil {
		h.log.Error("Failed to get group link", "chat_id", chatID, "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}

	if link == nil {
		h.reply(chatID, "This group is not linked to a project. Use /setup first.")
		return
	}

	// Always fetch fresh agent list so the keyboard reflects current state.
	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents", "project_id", link.ProjectID, "error", err)
		h.reply(chatID, "Failed to fetch agents. Please try again later.")
		return
	}

	if len(agents) == 0 {
		h.reply(chatID, "No agents found for this project.")
		return
	}

	promptText := "Select the default agent for @-mentions:"
	currentDefault := link.DefaultAgent

	if threadID != 0 {
		topicDefault, err := h.store.GetTopicDefault(ctx, chatID, threadID)
		if err != nil {
			h.log.Error("Failed to get topic default", "error", err)
		} else if topicDefault != "" {
			currentDefault = topicDefault
		}
		promptText = "Select the default agent for this topic:"
		if link.DefaultAgent != "" {
			promptText += fmt.Sprintf("\nChat-wide default: @%s", link.DefaultAgent)
		}
	}

	kb := buildDefaultAgentKeyboard(ctx, h.store, agentSlugs(agents), currentDefault, threadID)
	h.replyWithKeyboardInThread(chatID, threadID, promptText, kb)
}

func (h *CommandHandler) handleAgents(msg *TGMessage) {
	chatID := msg.Chat.ID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetGroupLink(ctx, chatID)
	if err != nil {
		h.log.Error("Failed to get group link", "chat_id", chatID, "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}

	if link == nil {
		h.reply(chatID, "This group is not linked to a project. Use /setup first.")
		return
	}

	// Always fetch fresh state for /agents display — bypass the cache.
	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents", "project_id", link.ProjectID, "error", err)
		h.reply(chatID, "Failed to fetch agents. Please try again later.")
		return
	}

	if len(agents) == 0 {
		h.reply(chatID, "No agents found for this project.")
		return
	}

	var lines []string
	for _, agent := range agents {
		emoji := activityEmoji(agent.Activity)
		label := agent.Slug
		if agent.Activity != "" {
			label += " — " + agent.Activity
		}
		if agent.Slug == link.DefaultAgent {
			label += " (default)"
		}
		lines = append(lines, fmt.Sprintf("%s 🤖 %s", emoji, label))
	}

	h.reply(chatID, fmt.Sprintf("Agents in *%s*:\n%s", link.ProjectSlug, strings.Join(lines, "\n")))
}

func (h *CommandHandler) handleUnlink(msg *TGMessage) {
	chatID := msg.Chat.ID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetGroupLink(ctx, chatID)
	if err != nil {
		h.log.Error("Failed to get group link", "chat_id", chatID, "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}

	if link == nil {
		h.reply(chatID, "This group is not linked to a project.")
		return
	}

	senderID := ""
	if msg.From != nil {
		senderID = strconv.FormatInt(msg.From.ID, 10)
	}
	if link.LinkedBy != "" && senderID != link.LinkedBy {
		h.reply(chatID, "Only the user who linked this group can unlink it.")
		return
	}

	if err := h.store.DeleteGroupLink(ctx, chatID); err != nil {
		h.log.Error("Failed to delete group link", "chat_id", chatID, "error", err)
		h.reply(chatID, "Failed to unlink. Please try again.")
		return
	}

	h.reply(chatID, fmt.Sprintf("Group unlinked from project *%s*.", link.ProjectSlug))
}

func (h *CommandHandler) handleHelp(msg *TGMessage) {
	chatID := msg.Chat.ID

	if isGroupChat(chatID) {
		h.reply(chatID, "Available commands:\n"+
			"/setup — Link this group to a project\n"+
			"/default — Set the default agent\n"+
			"/agents — List agents in the linked project\n"+
			"/settings — Configure group settings\n"+
			"/unlink — Unlink this group from its project\n"+
			"/help — Show this help message\n\n"+
			"Send /help in a DM to the bot for account management commands.")
	} else {
		h.reply(chatID, "Available commands (DM):\n"+
			"/register — Link your Telegram account to your scion hub identity\n"+
			"/unregister — Remove your Telegram account link\n"+
			"/status — Show linked groups and registration status\n"+
			"/notifications — Manage per-agent notification subscriptions\n"+
			"/help — Show this help message\n\n"+
			"Add me to a group and use /setup there to link it to a scion project.")
	}
}

func (h *CommandHandler) handleStatus(msg *TGMessage) {
	chatID := msg.Chat.ID

	if isGroupChat(chatID) {
		h.reply(chatID, "Use /status in a direct message.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	links, err := h.store.GetAllGroupLinks(ctx)
	if err != nil {
		h.log.Error("Failed to get group links", "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}

	if len(links) == 0 {
		h.reply(chatID, "No groups are currently linked.")
		return
	}

	var lines []string
	for _, link := range links {
		title := link.ChatTitle
		if title == "" {
			chat, err := h.api.GetChat(ctx, link.ChatID)
			if err == nil && chat.Title != "" {
				title = chat.Title
			}
		}
		// Resolve slug from cached projects if stored as UUID.
		slug := link.ProjectSlug
		if slug == link.ProjectID && len(h.cachedProjects) > 0 {
			for _, p := range h.cachedProjects {
				if p.ID == link.ProjectID {
					slug = p.DisplayName()
					break
				}
			}
		}
		var line string
		if title != "" {
			line = fmt.Sprintf("• %s (%d) → %s", title, link.ChatID, slug)
		} else {
			line = fmt.Sprintf("• chat %d → %s", link.ChatID, slug)
		}
		if link.DefaultAgent != "" {
			line += " (default: " + link.DefaultAgent + ")"
		}
		lines = append(lines, line)
	}

	// Build status with registration info first.
	regStatus := "Not registered"
	if msg.From != nil {
		senderID := strconv.FormatInt(msg.From.ID, 10)
		if m, _ := h.store.GetUserMapping(ctx, senderID); m != nil {
			if m.ScionEmail != "" {
				regStatus = "Registered as " + m.ScionEmail
			} else if m.ScionUserID != "" {
				regStatus = "Registered (user ID: " + m.ScionUserID + ")"
			}
		}
	}

	output := "Registration: " + regStatus + "\n\nLinked groups:\n" + strings.Join(lines, "\n")
	h.reply(chatID, output)
}

func (h *CommandHandler) handleSettings(msg *TGMessage) {
	chatID := msg.Chat.ID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := h.store.GetGroupLink(ctx, chatID)
	if err != nil {
		h.log.Error("Failed to get group link", "chat_id", chatID, "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}

	if link == nil {
		h.reply(chatID, "This group is not linked to a project. Use /setup first.")
		return
	}

	kb := buildSettingsKeyboard(link.ShowAgentToAgent, link.NotifyInGroup, link.ShowAssistantReply)
	h.replyWithKeyboard(chatID, "Group settings:", kb)
}

func (h *CommandHandler) handleNotifications(msg *TGMessage) {
	chatID := msg.Chat.ID

	if isGroupChat(chatID) {
		h.reply(chatID, "Use /notifications in a direct message.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	senderID := ""
	if msg.From != nil {
		senderID = strconv.FormatInt(msg.From.ID, 10)
	}
	if senderID == "" {
		h.reply(chatID, "Could not identify your user.")
		return
	}

	mapping, err := h.store.GetUserMapping(ctx, senderID)
	if err != nil {
		h.log.Error("Failed to check user mapping", "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}
	if mapping == nil {
		h.reply(chatID, "Please /register first to manage notifications.")
		return
	}

	links, err := h.store.GetAllGroupLinks(ctx)
	if err != nil {
		h.log.Error("Failed to get group links", "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}

	if len(links) == 0 {
		h.reply(chatID, "No linked projects found. Link a group to a project with /setup first.")
		return
	}

	existingPrefs, err := h.store.GetNotificationPrefs(ctx, senderID)
	if err != nil {
		h.log.Error("Failed to get notification prefs", "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}
	prefMap := make(map[string]bool)
	for _, p := range existingPrefs {
		prefMap[p.ProjectID+":"+p.AgentSlug] = p.Enabled
	}

	seen := make(map[string]bool)
	var entries []notificationAgentEntry
	for _, link := range links {
		if !link.Active {
			continue
		}
		if seen[link.ProjectID] {
			continue
		}
		seen[link.ProjectID] = true

		agents, agentErr := h.getAgents(ctx, link.ProjectID)
		if agentErr != nil {
			h.log.Warn("Failed to list agents for notification prefs", "project_id", link.ProjectID, "error", agentErr)
			continue
		}

		for _, agent := range agents {
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

	if len(entries) == 0 {
		h.reply(chatID, "No agents found across linked projects.")
		return
	}

	kb := buildNotificationsKeyboard(entries)
	h.replyWithKeyboard(chatID, "Tap an agent to toggle notifications:", kb)
}

// getAgents returns agents for a project, using the store cache with a
// fallback to the hub API.
func (h *CommandHandler) getAgents(ctx context.Context, projectID string) ([]AgentInfo, error) {
	cached, err := h.store.GetProjectAgents(ctx, projectID)
	if err != nil {
		h.log.Warn("Failed to read agent cache", "project_id", projectID, "error", err)
	}
	if cached != nil && time.Since(cached.RefreshedAt) < 5*time.Minute {
		return cached.Agents, nil
	}

	agents, err := h.hubClient.ListAgents(ctx, projectID)
	if err != nil {
		if cached != nil {
			return cached.Agents, nil
		}
		return nil, err
	}

	saveErr := h.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   projectID,
		Agents:      agents,
		RefreshedAt: time.Now(),
	})
	if saveErr != nil {
		h.log.Warn("Failed to cache agents", "project_id", projectID, "error", saveErr)
	}

	return agents, nil
}

// agentSlugs extracts just the slug strings from a slice of AgentInfo.
func agentSlugs(agents []AgentInfo) []string {
	slugs := make([]string, len(agents))
	for i, a := range agents {
		slugs[i] = a.Slug
	}
	return slugs
}

func (h *CommandHandler) reply(chatID int64, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.api.SendMessage(ctx, chatID, text, ""); err != nil {
		h.log.Error("Failed to send reply", "chat_id", chatID, "error", err)
	}
}

func (h *CommandHandler) replyWithKeyboard(chatID int64, text string, kb *InlineKeyboardMarkup) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.api.SendMessageWithKeyboard(ctx, chatID, text, "", kb, 0); err != nil {
		h.log.Error("Failed to send reply with keyboard", "chat_id", chatID, "error", err)
	}
}

func (h *CommandHandler) replyWithKeyboardInThread(chatID int64, threadID int64, text string, kb *InlineKeyboardMarkup) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var opts []SendOption
	if threadID != 0 {
		opts = append(opts, SendOption{MessageThreadID: threadID})
	}
	if _, err := h.api.SendMessageWithKeyboard(ctx, chatID, text, "", kb, 0, opts...); err != nil {
		h.log.Error("Failed to send reply with keyboard", "chat_id", chatID, "error", err)
	}
}

func isGroupChat(chatID int64) bool { return chatID < 0 }

// --- httpHubClient ---

// httpHubClient implements HubClient using HTTP calls to the Scion hub API.
type httpHubClient struct {
	hubURL     string
	hmacKey    string
	brokerID   string
	httpClient *http.Client
}

// NewHTTPHubClient creates a new HubClient that calls the Scion hub API.
func NewHTTPHubClient(hubURL, hmacKey, brokerID string) HubClient {
	return &httpHubClient{
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

type hubProjectsResponse struct {
	Projects []hubProject `json:"projects"`
}

type hubProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type hubAgentsResponse struct {
	Agents []hubAgent `json:"agents"`
}

type hubAgent struct {
	Slug     string `json:"slug"`
	Activity string `json:"activity"`
}

func (c *httpHubClient) ListProjects(ctx context.Context) ([]ProjectOption, error) {
	url := c.hubURL + "/api/v1/projects"

	slog.Debug("Listing projects from hub", "url", url, "broker_id", c.brokerID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list projects request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Debug("Hub returned non-OK for list projects", "status", resp.StatusCode, "url", url)
		return nil, fmt.Errorf("list projects returned status %d", resp.StatusCode)
	}

	var result hubProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list projects response: %w", err)
	}

	slog.Debug("Hub returned projects", "count", len(result.Projects))

	projects := make([]ProjectOption, len(result.Projects))
	for i, p := range result.Projects {
		projects[i] = ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug}
	}
	return projects, nil
}

func (c *httpHubClient) ListProjectsFresh(ctx context.Context) ([]ProjectOption, error) {
	url := c.hubURL + "/api/v1/broker/projects"

	slog.Debug("Listing fresh projects from hub broker endpoint", "url", url, "broker_id", c.brokerID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list fresh projects request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list fresh projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Debug("Hub returned non-OK for list fresh projects", "status", resp.StatusCode, "url", url)
		return nil, fmt.Errorf("list fresh projects returned status %d", resp.StatusCode)
	}

	var result hubProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list fresh projects response: %w", err)
	}

	slog.Debug("Hub returned fresh projects", "count", len(result.Projects))

	projects := make([]ProjectOption, len(result.Projects))
	for i, p := range result.Projects {
		projects[i] = ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug}
	}
	return projects, nil
}

func (c *httpHubClient) ListProjectsForUser(ctx context.Context, ownerID string) ([]ProjectOption, error) {
	url := c.hubURL + "/api/v1/projects?ownerId=" + ownerID

	slog.Debug("Listing projects for user from hub", "url", url, "owner_id", ownerID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list user projects request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list user projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list user projects returned status %d", resp.StatusCode)
	}

	var result hubProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list user projects response: %w", err)
	}

	projects := make([]ProjectOption, len(result.Projects))
	for i, p := range result.Projects {
		projects[i] = ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug}
	}
	return projects, nil
}

func (c *httpHubClient) ListAgents(ctx context.Context, projectID string) ([]AgentInfo, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%s/agents", c.hubURL, projectID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list agents request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list agents request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list agents returned status %d", resp.StatusCode)
	}

	var result hubAgentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list agents response: %w", err)
	}

	agents := make([]AgentInfo, len(result.Agents))
	for i, a := range result.Agents {
		agents[i] = AgentInfo{Slug: a.Slug, Activity: a.Activity}
	}
	return agents, nil
}

func (c *httpHubClient) signRequest(req *http.Request) error {
	if c.brokerID == "" || c.hmacKey == "" {
		return nil
	}

	secretKey, err := decodeBase64(c.hmacKey)
	if err != nil {
		return fmt.Errorf("decode HMAC key: %w", err)
	}

	auth := &apiclient.HMACAuth{
		BrokerID:  c.brokerID,
		SecretKey: secretKey,
	}
	return auth.ApplyAuth(req)
}

// activityEmoji returns an emoji for an agent activity state, matching the web UI.
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
