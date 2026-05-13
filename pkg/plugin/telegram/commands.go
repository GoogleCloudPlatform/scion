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

// HubClient provides access to the Scion hub API for project and agent listing.
type HubClient interface {
	ListProjects(ctx context.Context) ([]ProjectOption, error)
	ListAgents(ctx context.Context, projectID string) ([]string, error)
}

// CommandHandler processes bot commands from incoming Telegram messages.
type CommandHandler struct {
	store       Store
	api         *TelegramAPIClient
	hubClient   HubClient
	botUsername string
	log         *slog.Logger
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
		botUsername:  botUsername,
		log:         log,
	}
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

	projects, err := h.hubClient.ListProjects(ctx)
	if err != nil {
		h.log.Error("Failed to list projects from hub", "error", err)
		h.reply(chatID, "Failed to fetch projects. Please try again later.")
		return
	}

	h.log.Debug("Listed projects from hub", "count", len(projects))

	if len(projects) == 0 {
		senderID := ""
		if msg.From != nil {
			senderID = strconv.FormatInt(msg.From.ID, 10)
		}
		if senderID != "" {
			mapping, mErr := h.store.GetUserMapping(ctx, senderID)
			if mErr == nil && mapping == nil {
				h.reply(chatID, "No projects found. Please /register first (DM me), then try /setup again.")
				return
			}
		}
		h.reply(chatID, "No projects found. Create a project in the hub first.")
		return
	}

	kb := buildProjectSelectionKeyboard(projects)
	h.replyWithKeyboard(chatID, "Select a project to link this group to:", kb)
}

func (h *CommandHandler) handleDefault(msg *TGMessage) {
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

	agents, err := h.getAgents(ctx, link.ProjectID)
	if err != nil {
		h.log.Error("Failed to list agents", "project_id", link.ProjectID, "error", err)
		h.reply(chatID, "Failed to fetch agents. Please try again later.")
		return
	}

	if len(agents) == 0 {
		h.reply(chatID, "No agents found for this project.")
		return
	}

	kb := buildDefaultAgentKeyboard(agents, link.DefaultAgent)
	h.replyWithKeyboard(chatID, "Select the default agent for @-mentions:", kb)
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

	agents, err := h.getAgents(ctx, link.ProjectID)
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
		label := agent
		if agent == link.DefaultAgent {
			label += " (default)"
		}
		lines = append(lines, fmt.Sprintf("• @%s", label))
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
			"/help — Show this help message")
	} else {
		h.reply(chatID, "Available commands:\n"+
			"/status — Show linked groups\n"+
			"/help — Show this help message\n\n"+
			"Use /setup in a group chat to get started.")
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

	if msg.From != nil {
		senderID := strconv.FormatInt(msg.From.ID, 10)
		mapping, err := h.store.GetUserMapping(ctx, senderID)
		if err != nil {
			h.log.Error("Failed to check user mapping", "error", err, "telegram_user_id", senderID)
			h.reply(chatID, "Something went wrong. Please try again.")
			return
		}
		if mapping == nil {
			h.reply(chatID, "Please /register first to use this bot.")
			return
		}
	}

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
			title = fmt.Sprintf("chat %d", link.ChatID)
		}
		lines = append(lines, fmt.Sprintf("• %s → %s (default: %s)", title, link.ProjectSlug, link.DefaultAgent))
	}

	h.reply(chatID, "Linked groups:\n"+strings.Join(lines, "\n"))
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

	kb := buildSettingsKeyboard(link.ShowAgentToAgent)
	h.replyWithKeyboard(chatID, "Group settings:", kb)
}

// getAgents returns agents for a project, using the store cache with a
// fallback to the hub API.
func (h *CommandHandler) getAgents(ctx context.Context, projectID string) ([]string, error) {
	cached, err := h.store.GetProjectAgents(ctx, projectID)
	if err != nil {
		h.log.Warn("Failed to read agent cache", "project_id", projectID, "error", err)
	}
	if cached != nil && time.Since(cached.RefreshedAt) < 5*time.Minute {
		return cached.AgentSlugs, nil
	}

	agents, err := h.hubClient.ListAgents(ctx, projectID)
	if err != nil {
		if cached != nil {
			return cached.AgentSlugs, nil
		}
		return nil, err
	}

	saveErr := h.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   projectID,
		AgentSlugs:  agents,
		RefreshedAt: time.Now(),
	})
	if saveErr != nil {
		h.log.Warn("Failed to cache agents", "project_id", projectID, "error", saveErr)
	}

	return agents, nil
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
	Slug string `json:"slug"`
}

func (c *httpHubClient) ListProjects(ctx context.Context) ([]ProjectOption, error) {
	url := c.hubURL + "/api/v1/groves"

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

func (c *httpHubClient) ListAgents(ctx context.Context, projectID string) ([]string, error) {
	url := fmt.Sprintf("%s/api/v1/groves/%s/agents", c.hubURL, projectID)
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

	agents := make([]string, len(result.Agents))
	for i, a := range result.Agents {
		agents[i] = a.Slug
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
