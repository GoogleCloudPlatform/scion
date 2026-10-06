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
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// AgentInfo holds an agent's slug and current activity state.
type AgentInfo struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Activity string `json:"activity,omitempty"`
	Phase    string `json:"phase,omitempty"`
}

// HubClient provides access to the Scion hub API for project and agent listing.
//
// Methods that take onBehalfOf act as the linked Scion user identified by
// that principal ("user:<email>", see linkedUserPrincipal). User-initiated
// reads must pass the requesting user's principal.
type HubClient interface {
	// ListProjectsFresh lists every project served by this broker. It is
	// not scoped to a user and must not feed user-facing project pickers.
	ListProjectsFresh(ctx context.Context) ([]ProjectOption, error)
	// ListProjectsForUser returns the projects visible to the linked user,
	// following pagination. At the page limit the list is truncated.
	ListProjectsForUser(ctx context.Context, onBehalfOf string) ([]ProjectOption, error)
	// ListAgents returns the agents of a project as seen by the linked user,
	// following pagination. At the page limit the list is truncated.
	ListAgents(ctx context.Context, projectID, onBehalfOf string) ([]AgentInfo, error)

	// HubBaseURL returns the base URL of the hub (e.g. "https://hub.example.com").
	HubBaseURL() string
}

// CommandHandler processes bot commands from incoming Telegram messages.
type CommandHandler struct {
	store          Store
	api            *TelegramAPIClient
	hubClient      HubClient
	botUsername    string
	log            *slog.Logger
	cachedProjects []ProjectOption
	projectsMu     sync.Mutex // guards cachedProjects

	// memberCache remembers successful group-membership checks for /status,
	// keyed by "userID:chatID".
	memberCache   map[string]memberCacheEntry
	memberCacheMu sync.Mutex
}

type memberCacheEntry struct {
	member    bool
	failed    bool
	checkedAt time.Time
}

// ttl returns how long the entry is reused.
func (e memberCacheEntry) ttl() time.Duration {
	if e.failed {
		return memberFailureCacheTTL
	}
	return memberCacheTTL
}

// errMembershipCheckFailed reports a recently failed membership check that
// is not retried yet.
var errMembershipCheckFailed = errors.New("group membership check failed recently")

const (
	// memberCacheTTL bounds how long a group-membership check is reused.
	memberCacheTTL = 2 * time.Minute
	// memberFailureCacheTTL bounds how long a failed check is remembered.
	memberFailureCacheTTL = 30 * time.Second
	// memberCheckWorkers bounds concurrent membership checks for /status.
	memberCheckWorkers = 6
	// memberCheckLimit caps membership checks per /status; further groups
	// are reported as not checked.
	memberCheckLimit = 50
	// statusMemberCheckBudget is the share of the /status time budget given
	// to membership checks.
	statusMemberCheckBudget = 6 * time.Second
	// statusUncheckedNote is appended when some groups could not be checked.
	statusUncheckedNote = "(some groups could not be checked)"
)

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

// SetProjects updates the cached project list used to display project names
// (e.g. in /status). It is not offered in setup pickers.
func (h *CommandHandler) SetProjects(projects []ProjectOption) {
	h.projectsMu.Lock()
	h.cachedProjects = projects
	h.projectsMu.Unlock()
}

// registerHint is the reply sent when a command needs a linked Scion account
// and the sender has none.
const registerHint = "Please /register first to use this bot. Send /register to me in a direct message."

// requireLinkedSender looks up the sender's link mapping and returns the
// principal to act as on hub reads. When the sender is not linked it replies
// with a register hint and returns ok=false.
func (h *CommandHandler) requireLinkedSender(ctx context.Context, msg *TGMessage) (mapping *TelegramUserMapping, principal string, ok bool) {
	chatID := msg.Chat.ID
	if msg.From == nil {
		h.reply(chatID, registerHint)
		return nil, "", false
	}
	senderID := strconv.FormatInt(msg.From.ID, 10)
	mapping, err := h.store.GetUserMapping(ctx, senderID)
	if err != nil {
		h.log.Error("Failed to look up user mapping", "sender_id", senderID, "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return nil, "", false
	}
	if mapping == nil {
		h.reply(chatID, registerHint)
		return nil, "", false
	}
	principal = linkedUserPrincipal(mapping)
	if principal == "" {
		// Linked without a Scion email: the link cannot be used.
		h.reply(chatID, staleLinkText)
		return nil, "", false
	}
	return mapping, principal, true
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
	case "/terminal":
		h.handleTerminal(msg)
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

	// Setup offers the sender's own projects, so it needs a linked account.
	mapping, principal, ok := h.requireLinkedSender(ctx, msg)
	if !ok {
		return
	}

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

	projects, err := h.hubClient.ListProjectsForUser(ctx, principal)
	if err != nil {
		h.log.Warn("Failed to list projects for linked user", "error", err)
		h.reply(chatID, hubErrorText(err, mapping.ScionEmail, "", setupProjectsFailedText))
		return
	}

	if len(projects) == 0 {
		h.reply(chatID, noUserProjectsText)
		return
	}

	kb := buildProjectSelectionKeyboard(projects)
	h.replyWithKeyboard(chatID, "Select a project to link this group to:", kb)
}

// Replies for the setup project pickers, which list only the linked user's
// projects.
const (
	setupProjectsFailedText = "Failed to fetch your projects. Please try again later."
	noUserProjectsText      = "Your Scion account isn't a member of any project yet. Ask a project owner to add you, then run /setup again."
)

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

	mapping, principal, ok := h.requireLinkedSender(ctx, msg)
	if !ok {
		return
	}

	// Always fetch fresh agent list so the keyboard reflects current state.
	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID, principal)
	if err != nil {
		h.log.Error("Failed to list agents", "project_id", link.ProjectID, "error", err)
		h.reply(chatID, hubErrorText(err, mapping.ScionEmail, link.ProjectSlug, "Failed to fetch agents. Please try again later."))
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

func (h *CommandHandler) handleTerminal(msg *TGMessage) {
	chatID := msg.Chat.ID

	// Parse agent name from the message text.
	parts := strings.Fields(msg.Text)
	if len(parts) < 2 {
		h.reply(chatID, "Usage: /terminal <agent-name>")
		return
	}
	agentName := parts[1]

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

	mapping, principal, ok := h.requireLinkedSender(ctx, msg)
	if !ok {
		return
	}

	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID, principal)
	if err != nil {
		h.log.Error("Failed to list agents", "project_id", link.ProjectID, "error", err)
		h.reply(chatID, hubErrorText(err, mapping.ScionEmail, link.ProjectSlug, "Failed to fetch agents. Please try again later."))
		return
	}

	for _, agent := range agents {
		if strings.EqualFold(agent.Slug, agentName) {
			if strings.ToLower(agent.Phase) != "running" {
				phase := agent.Phase
				if phase == "" {
					phase = "unknown"
				}
				h.reply(chatID, fmt.Sprintf("Agent '%s' is not running (phase: %s).", agent.Slug, phase))
				return
			}
			terminalURL := fmt.Sprintf("%s/agents/%s/terminal", h.hubClient.HubBaseURL(), agent.ID)
			h.reply(chatID, fmt.Sprintf("Terminal for *%s*: %s", agent.Slug, terminalURL))
			return
		}
	}

	h.reply(chatID, fmt.Sprintf("Agent '%s' not found in this project.", agentName))
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

	mapping, principal, ok := h.requireLinkedSender(ctx, msg)
	if !ok {
		return
	}

	// Always fetch fresh state for /agents display — bypass the cache.
	agents, err := h.hubClient.ListAgents(ctx, link.ProjectID, principal)
	if err != nil {
		h.log.Error("Failed to list agents", "project_id", link.ProjectID, "error", err)
		h.reply(chatID, hubErrorText(err, mapping.ScionEmail, link.ProjectSlug, "Failed to fetch agents. Please try again later."))
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

	versionLine := fmt.Sprintf("\n\n<i>Scion Telegram Integration — %s</i>", html.EscapeString(version.Get()))

	var text string
	if isGroupChat(chatID) {
		text = "Available commands:\n" +
			"/setup — Link this group to a project\n" +
			"/default — Set the default agent\n" +
			"/agents — List agents in the linked project\n" +
			"/terminal <agent> — Get the web terminal URL for an agent\n" +
			"/settings — Configure group settings\n" +
			"/unlink — Unlink this group from its project\n" +
			"/help — Show this help message\n\n" +
			"Send /help in a DM to the bot for account management commands." +
			versionLine
	} else {
		text = "Available commands (DM):\n" +
			"/register — Link your Telegram account to your scion hub identity\n" +
			"/unregister — Remove your Telegram account link\n" +
			"/status — Show linked groups and registration status\n" +
			"/notifications — Manage per-agent notification subscriptions\n" +
			"/help — Show this help message\n\n" +
			"Add me to a group and use /setup there to link it to a scion project." +
			versionLine
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.api.SendMessage(ctx, chatID, text, "HTML"); err != nil {
		h.log.Error("Failed to send help reply", "chat_id", chatID, "error", err)
	}
}

func (h *CommandHandler) handleStatus(msg *TGMessage) {
	chatID := msg.Chat.ID

	if isGroupChat(chatID) {
		h.reply(chatID, "Use /status in a direct message.")
		return
	}

	if msg.From == nil {
		h.reply(chatID, "Could not identify your user.")
		return
	}

	// Registration status first, on its own short budget.
	regStatus := h.registrationStatus(msg.From.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	allLinks, err := h.store.GetAllGroupLinks(ctx)
	if err != nil {
		h.log.Error("Failed to get group links", "error", err)
		h.reply(chatID, "Something went wrong. Please try again.")
		return
	}
	var activeLinks []*GroupLink
	for _, link := range allLinks {
		if link.Active {
			activeLinks = append(activeLinks, link)
		}
	}

	// List only groups the sender linked or is currently a member of.
	// Membership checks get their own share of the budget so title
	// lookups below still have time.
	memberCtx, memberCancel := context.WithTimeout(ctx, statusMemberCheckBudget)
	links, unchecked := h.groupsVisibleTo(memberCtx, msg.From.ID, activeLinks)
	memberCancel()

	h.projectsMu.Lock()
	cachedProjects := h.cachedProjects
	h.projectsMu.Unlock()

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
		if slug == link.ProjectID && len(cachedProjects) > 0 {
			for _, p := range cachedProjects {
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

	groups := "No groups you linked or belong to are linked to a project."
	if len(lines) > 0 {
		groups = "Linked groups:\n" + strings.Join(lines, "\n")
	} else if unchecked {
		groups = "No linked groups could be confirmed for you."
	}
	if unchecked {
		groups += "\n" + statusUncheckedNote
	}
	h.reply(chatID, "Registration: "+regStatus+"\n\n"+groups)
}

// registrationStatus describes the Telegram user's link to Scion for
// /status. A store error is reported as unknown, not as unregistered.
func (h *CommandHandler) registrationStatus(userID int64) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := h.store.GetUserMapping(ctx, strconv.FormatInt(userID, 10))
	switch {
	case err != nil:
		h.log.Warn("Failed to look up user mapping for /status", "error", err)
		return "Unknown (could not be checked)"
	case m == nil:
		return "Not registered"
	case m.ScionEmail == "":
		// Linked without a Scion email: the link cannot be used.
		return staleLinkText
	default:
		return "Registered as " + m.ScionEmail
	}
}

// groupsVisibleTo returns, in their original order, the group links that
// the Telegram user linked or is currently a member of. Membership is
// checked with bounded concurrency, for at most memberCheckLimit groups per
// call, and cached briefly per user and chat. unchecked reports that some
// groups could not be checked (check failed, limit reached, or ctx ended);
// those groups are left out.
func (h *CommandHandler) groupsVisibleTo(ctx context.Context, userID int64, links []*GroupLink) (visible []*GroupLink, unchecked bool) {
	senderID := strconv.FormatInt(userID, 10)
	const (
		notMember = iota
		member
		failed
	)
	results := make([]int, len(links))

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < memberCheckWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				ok, err := h.isGroupMember(ctx, links[i].ChatID, userID)
				switch {
				case err != nil:
					results[i] = failed
				case ok:
					results[i] = member
				}
			}
		}()
	}
	queued := 0
	for i, link := range links {
		if link.LinkedBy == senderID {
			results[i] = member
			continue
		}
		// Cached answers are free and do not count toward the limit.
		if e, ok := h.cachedMembership(link.ChatID, userID); ok {
			switch {
			case e.failed:
				results[i] = failed
			case e.member:
				results[i] = member
			}
			continue
		}
		if queued >= memberCheckLimit {
			results[i] = failed
			continue
		}
		queued++
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	for i, link := range links {
		switch results[i] {
		case member:
			visible = append(visible, link)
		case failed:
			unchecked = true
		}
	}
	return visible, unchecked
}

// isNotVisibleChatError reports whether a getChatMember error means the
// group is not visible to the user, as opposed to the check itself failing:
// the user is not in the chat (400 user/member not found), the chat is gone
// (400 chat not found), or the bot can no longer see the chat (403, e.g.
// the bot was kicked).
func isNotVisibleChatError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		desc := strings.ToLower(apiErr.Description)
		return strings.Contains(desc, "user not found") ||
			strings.Contains(desc, "member not found") ||
			strings.Contains(desc, "participant_id_invalid") ||
			strings.Contains(desc, "chat not found")
	default:
		return false
	}
}

func memberCacheKey(chatID, userID int64) string {
	return strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(chatID, 10)
}

// cachedMembership returns the unexpired cached membership check.
func (h *CommandHandler) cachedMembership(chatID, userID int64) (memberCacheEntry, bool) {
	h.memberCacheMu.Lock()
	defer h.memberCacheMu.Unlock()
	e, ok := h.memberCache[memberCacheKey(chatID, userID)]
	if !ok || time.Since(e.checkedAt) >= e.ttl() {
		return memberCacheEntry{}, false
	}
	return e, true
}

// isGroupMember reports whether the user is currently in the chat, using a
// short-lived cache of results and of failed checks.
func (h *CommandHandler) isGroupMember(ctx context.Context, chatID, userID int64) (bool, error) {
	key := memberCacheKey(chatID, userID)
	if entry, ok := h.cachedMembership(chatID, userID); ok {
		if entry.failed {
			return false, errMembershipCheckFailed
		}
		return entry.member, nil
	}

	if err := ctx.Err(); err != nil {
		return false, err
	}
	result := memberCacheEntry{checkedAt: time.Now()}
	m, err := h.api.GetChatMember(ctx, chatID, userID)
	switch {
	case err == nil:
		result.member = m.IsCurrentMember()
	case isNotVisibleChatError(err):
		// Not visible to the user: not a member.
	case ctx.Err() != nil:
		// The request ended with the caller's context; not remembered.
		return false, err
	default:
		h.log.Debug("Could not check group membership for /status", "chat_id", chatID, "error", err)
		result.failed = true
	}

	h.memberCacheMu.Lock()
	if h.memberCache == nil {
		h.memberCache = make(map[string]memberCacheEntry)
	}
	for k, e := range h.memberCache {
		if time.Since(e.checkedAt) >= e.ttl() {
			delete(h.memberCache, k)
		}
	}
	h.memberCache[key] = result
	h.memberCacheMu.Unlock()

	if result.failed {
		return false, err
	}
	return result.member, nil
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

	mapping, _, ok := h.requireLinkedSender(ctx, msg)
	if !ok {
		return
	}

	// Only group-linked projects the linked user can read are offered.
	result, err := buildNotificationEntries(ctx, h.store, h.hubClient, h.log, mapping)
	if err != nil {
		h.log.Warn("Failed to build notification toggles", "error", err)
		h.reply(chatID, hubErrorText(err, mapping.ScionEmail, "", setupProjectsFailedText))
		return
	}
	if result.LinkedProjects == 0 {
		h.reply(chatID, "No linked projects found. Link a group to a project with /setup first.")
		return
	}
	entries := result.Entries
	if len(entries) == 0 {
		h.reply(chatID, "No agents found across linked projects.")
		return
	}

	kb := buildNotificationsKeyboard(entries)
	h.replyWithKeyboard(chatID, "Tap an agent to toggle notifications:", kb)
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
// If httpClient is nil, a default client with a 10s timeout is used.
func NewHTTPHubClient(hubURL, hmacKey, brokerID string, httpClient *http.Client) HubClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &httpHubClient{
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: httpClient,
	}
}

type hubProjectsResponse struct {
	Projects   []hubProject `json:"projects"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

type hubProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type hubAgentsResponse struct {
	Agents     []hubAgent `json:"agents"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

type hubAgent struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Activity string `json:"activity"`
	Phase    string `json:"phase"`
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
		return nil, newHubError("list fresh projects", resp)
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

// maxUserProjectPages bounds how many pages ListProjectsForUser follows.
const maxUserProjectPages = 20

// ListProjectsForUser follows nextCursor for up to maxUserProjectPages
// pages, sending the linked user on each. If more pages remain at the limit
// it returns the projects fetched so far (truncated) and logs a warning. An
// error on any page returns the error, not a partial list.
func (c *httpHubClient) ListProjectsForUser(ctx context.Context, onBehalfOf string) ([]ProjectOption, error) {
	var projects []ProjectOption
	cursor := ""
	for page := 0; page < maxUserProjectPages; page++ {
		result, err := c.listUserProjectsPage(ctx, onBehalfOf, cursor)
		if err != nil {
			return nil, err
		}
		for _, p := range result.Projects {
			projects = append(projects, ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug})
		}
		if result.NextCursor == "" {
			return projects, nil
		}
		cursor = result.NextCursor
	}
	slog.Warn("User project list truncated at page limit", "pages", maxUserProjectPages, "count", len(projects))
	return projects, nil
}

// listUserProjectsPage fetches one page of the linked user's projects.
func (c *httpHubClient) listUserProjectsPage(ctx context.Context, onBehalfOf, cursor string) (*hubProjectsResponse, error) {
	endpoint := c.hubURL + "/api/v1/projects"
	if cursor != "" {
		endpoint += "?cursor=" + neturl.QueryEscape(cursor)
	}

	slog.Debug("Listing projects for linked user from hub", "url", endpoint, "on_behalf_of", onBehalfOf)

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create list user projects request: %w", err)
	}
	setOnBehalfOf(req, onBehalfOf)

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list user projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, newHubError("list user projects", resp)
	}

	var result hubProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list user projects response: %w", err)
	}
	return &result, nil
}

// maxAgentPages bounds how many pages ListAgents follows.
const maxAgentPages = 20

// ListAgents follows nextCursor for up to maxAgentPages pages, sending the
// linked user on each. If more pages remain at the limit it returns the
// agents fetched so far (truncated) and logs a warning. An error on any page
// returns the error, not a partial list.
func (c *httpHubClient) ListAgents(ctx context.Context, projectID, onBehalfOf string) ([]AgentInfo, error) {
	var agents []AgentInfo
	cursor := ""
	for page := 0; page < maxAgentPages; page++ {
		result, err := c.listAgentsPage(ctx, projectID, onBehalfOf, cursor)
		if err != nil {
			return nil, err
		}
		for _, a := range result.Agents {
			agents = append(agents, AgentInfo{ID: a.ID, Slug: a.Slug, Activity: a.Activity, Phase: a.Phase})
		}
		if result.NextCursor == "" {
			return agents, nil
		}
		cursor = result.NextCursor
	}
	slog.Warn("Agent list truncated at page limit", "project_id", projectID, "pages", maxAgentPages, "count", len(agents))
	return agents, nil
}

// listAgentsPage fetches one page of a project's agents as the linked user.
func (c *httpHubClient) listAgentsPage(ctx context.Context, projectID, onBehalfOf, cursor string) (*hubAgentsResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/projects/%s/agents", c.hubURL, projectID)
	if cursor != "" {
		endpoint += "?cursor=" + neturl.QueryEscape(cursor)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create list agents request: %w", err)
	}
	setOnBehalfOf(req, onBehalfOf)

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list agents request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, newHubError("list agents", resp)
	}

	var result hubAgentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list agents response: %w", err)
	}
	return &result, nil
}

func (c *httpHubClient) HubBaseURL() string {
	return c.hubURL
}

// onBehalfOfHeader names the linked user the plugin acts for on a request.
const onBehalfOfHeader = "X-Scion-On-Behalf-Of"

// setOnBehalfOf makes req act as the linked user identified by onBehalfOf
// ("user:<email>"). An empty principal leaves the request unchanged.
// Call it before signRequest.
func setOnBehalfOf(req *http.Request, onBehalfOf string) {
	if onBehalfOf == "" {
		return
	}
	req.Header.Set(onBehalfOfHeader, onBehalfOf)

	// Add the header name to the semicolon-separated signed-headers list,
	// keeping any names already listed on the request.
	name := strings.ToLower(onBehalfOfHeader)
	listed := req.Header.Get(apiclient.HeaderSignedHeaders)
	for _, n := range strings.Split(listed, ";") {
		if strings.EqualFold(strings.TrimSpace(n), name) {
			return
		}
	}
	if strings.TrimSpace(listed) != "" {
		name = listed + ";" + name
	}
	req.Header.Set(apiclient.HeaderSignedHeaders, name)
}

// linkedUserPrincipal returns the "user:<email>" principal for a linked
// Telegram user, or "" when there is no mapping or it has no Scion email.
func linkedUserPrincipal(m *TelegramUserMapping) string {
	if m == nil || m.ScionEmail == "" {
		return ""
	}
	return "user:" + m.ScionEmail
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
