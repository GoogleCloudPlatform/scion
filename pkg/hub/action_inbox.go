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

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// ActionInboxTab* are the valid values for the ?tab= query parameter on
	// /api/v1/users/me/<action-kind> endpoints.
	ActionInboxTabOpen     = "open"
	ActionInboxTabResolved = "resolved"

	// ActionInboxSort* are the valid values for the ?sort= query parameter.
	ActionInboxSortRecent  = "recent"
	ActionInboxSortProject = "project"

	maxActionInboxProjects   = 200
	actionInboxScanCap       = 500
	defaultActionInboxLimit  = 50
	maxActionInboxLimit      = 100
	maxActionInboxCursorSize = 4096
)

// actionInboxRow is the kind-neutral projection every message-action kind's
// rows map onto.
type actionInboxRow struct {
	ID          string
	ProjectID   string
	AgentID     string
	AgentName   string
	ChatKey     string
	MessageID   string
	Key         string
	Description string
	Class       msgaction.Class
	CreatedAt   time.Time
	ExpiresAt   time.Time // zero when the kind does not expire
	ResolvedAt  time.Time
	Raw         any // the kind's own row, handed back to CardsFor
}

type actionOpenQuery struct {
	ProjectIDs []string // explicit member projects, capped at 200
	ViewerID   string   // also matches dm:...:user:<ViewerID> chat keys outside ProjectIDs
	Now        time.Time
	ScanCap    int // max rows to fetch from the store
}

type actionCursor struct {
	Time time.Time `json:"t"`
	ID   string    `json:"id"`
}

type actionHistoryQuery struct {
	ProjectIDs      []string
	ViewerID        string
	FilterProjectID string
	FilterAgentID   string
	Cursor          *actionCursor
	Limit           int
}

type authorizedActionRow struct {
	Row        actionInboxRow
	Answerable bool
}

// actionInboxScopeMode selects how an actionInboxKind discovers candidate rows.
type actionInboxScopeMode int

const (
	// scopeMemberProjects discovers rows across the viewer's member projects
	// (capped at maxActionInboxProjects) and DMs.
	scopeMemberProjects actionInboxScopeMode = iota
	// scopeViewer discovers rows directly by viewer identity (e.g. named
	// approver) without loading project memberships.
	scopeViewer
)

// actionInboxKind is implemented once per message-action kind that participates
// in the reusable action-inbox list endpoint and inbox summary category.
type actionInboxKind interface {
	Kind() string       // msgaction kind name
	Experiment() string // feature gate experiment key
	Category() string   // inbox summary category key
	Href() string       // web detail page href
	Scope() actionInboxScopeMode
	ListOpen(ctx context.Context, q actionOpenQuery) ([]actionInboxRow, error)
	ListResolved(ctx context.Context, q actionHistoryQuery) ([]actionInboxRow, error)
	// Recipient is the kind's recipient test for read (answering=false) or
	// respond (answering=true).
	Recipient(ctx context.Context, sc *actionInboxScope, row actionInboxRow, answering bool) bool
	// CanRead is the kind's extra read permission beyond Recipient(false).
	CanRead(ctx context.Context, sc *actionInboxScope, row actionInboxRow) bool
	// CanRespond is the kind's extra permission beyond Recipient(true).
	CanRespond(ctx context.Context, sc *actionInboxScope, row actionInboxRow) bool
	// CardsFor groups authorized rows into the kind's existing card wire shape.
	CardsFor(ctx context.Context, sc *actionInboxScope, rows []authorizedActionRow) (any, error)
}

// ActionInboxCounts reports open and answerable row counts on the Open tab.
type ActionInboxCounts struct {
	Answerable int `json:"answerable"`
	Open       int `json:"open"`
}

// ActionInboxFacetItem is one agent or project facet entry.
type ActionInboxFacetItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ActionInboxFacets holds the agent and project filter facets.
type ActionInboxFacets struct {
	Agents   []ActionInboxFacetItem `json:"agents"`
	Projects []ActionInboxFacetItem `json:"projects"`
}

// ActionInboxResponse is the shared wire response for
// GET /api/v1/users/me/<action-kind>.
//
// Facet and sort semantics differ slightly by tab:
//   - On tab=open, Facets cover the full unfiltered readable open set (before
//     project/agent/q filters), and sort=project orders globally across open cards.
//   - On tab=resolved, pagination uses a (resolved_at DESC, id DESC) row keyset
//     cursor (so a multi-row card whose rows resolved at different times may
//     appear as fragments across adjacent pages), Facets cover the scanned,
//     filtered rows emitted on the current page, and sort=project orders cards
//     within the current page only.
type ActionInboxResponse struct {
	Counts            ActionInboxCounts  `json:"counts"`
	FilteredCounts    *ActionInboxCounts `json:"filteredCounts,omitempty"`
	Cards             any                `json:"cards"`
	Facets            ActionInboxFacets  `json:"facets"`
	TruncatedProjects bool               `json:"truncatedProjects"`
	Capped            bool               `json:"capped"`
	NextCursor        string             `json:"nextCursor,omitempty"`
}

type recipientMemoKey struct {
	chatKey   string
	messageID string
	answering bool
}

type agentPermMemoKey struct {
	agentID   string
	projectID string
	action    Action
}

// actionInboxScope holds per-request membership and memoized authorization
// lookups shared across all rows evaluated during one list or summary request.
type actionInboxScope struct {
	srv               *Server
	user              UserIdentity
	viewerID          string
	scopeMode         actionInboxScopeMode
	memberProjectIDs  []string
	projectIDsLoaded  bool
	truncatedProjects bool

	recipientMemo map[recipientMemoKey]bool
	agentMemo     map[string]*store.Agent
	agentLoaded   map[string]bool
	agentPermMemo map[agentPermMemoKey]bool
	promptMsgMemo map[string]*store.Message
	promptLoaded  map[string]bool
	projectNames  map[string]string
}

func (s *Server) actionInboxMaxProjects() int {
	if s.actionInboxMaxProjectsForTest > 0 {
		return s.actionInboxMaxProjectsForTest
	}
	return maxActionInboxProjects
}

func (s *Server) actionInboxScanCapLimit() int {
	if s.actionInboxScanCapForTest > 0 {
		return s.actionInboxScanCapForTest
	}
	return actionInboxScanCap
}

func (s *Server) newActionInboxScope(ctx context.Context, user UserIdentity, mode actionInboxScopeMode) (*actionInboxScope, error) {
	sc := &actionInboxScope{
		srv:           s,
		user:          user,
		viewerID:      user.ID(),
		scopeMode:     mode,
		recipientMemo: make(map[recipientMemoKey]bool),
		agentMemo:     make(map[string]*store.Agent),
		agentLoaded:   make(map[string]bool),
		agentPermMemo: make(map[agentPermMemoKey]bool),
		promptMsgMemo: make(map[string]*store.Message),
		promptLoaded:  make(map[string]bool),
		projectNames:  make(map[string]string),
	}
	if mode == scopeMemberProjects {
		if _, err := sc.projectIDs(ctx); err != nil {
			return nil, err
		}
	}
	return sc, nil
}

func (sc *actionInboxScope) projectIDs(ctx context.Context) ([]string, error) {
	if sc.scopeMode == scopeViewer {
		return nil, nil
	}
	if sc.projectIDsLoaded {
		return sc.memberProjectIDs, nil
	}
	memberMap, err := sc.srv.chatMemberProjectIDs(ctx, sc.viewerID)
	if err != nil {
		return nil, err
	}
	projIDs := make([]string, 0, len(memberMap))
	for pid := range memberMap {
		projIDs = append(projIDs, pid)
	}
	sort.Strings(projIDs)
	maxProj := sc.srv.actionInboxMaxProjects()
	truncated := false
	if len(projIDs) > maxProj {
		projIDs = projIDs[:maxProj]
		truncated = true
	}
	sc.memberProjectIDs = projIDs
	sc.truncatedProjects = truncated
	sc.projectIDsLoaded = true
	return sc.memberProjectIDs, nil
}

// isMessageActionRecipient reports whether user is a recipient of a message
// action card (the DM target, or a participant of the thread/project).
// answering is false for read access and true for response actions.
func (s *Server) isMessageActionRecipient(ctx context.Context, user UserIdentity, chatKey string, promptMsg *store.Message, answering bool) bool {
	if user == nil {
		return false
	}
	if chatKey == "" {
		if promptMsg != nil {
			if promptMsg.RecipientID != "" && promptMsg.RecipientID == user.ID() {
				return true
			}
			if promptMsg.ConversationID != "" && s.store != nil {
				if parts, err := s.store.ListParticipants(ctx, promptMsg.ConversationID); err == nil {
					for _, p := range parts {
						if p.PrincipalKind == "user" && p.PrincipalID == user.ID() {
							return true
						}
					}
				}
			}
		}
		return false
	}
	if strings.HasPrefix(chatKey, "dm:") {
		return authorizeDMKeyParticipant(ctx, user, chatKey, chatSendPath(chatKey)) == nil
	}
	return s.authorizeDecisionChatKey(ctx, user, chatKey, answering) == nil
}

// publishDMActionInboxHint emits a content-free inbox invalidation hint for
// each user participant of a DM chat key.
func (s *Server) publishDMActionInboxHint(ctx context.Context, chatKey, category string) {
	if !strings.HasPrefix(chatKey, "dm:") || category == "" {
		return
	}
	kindA, idA, kindB, idB, err := messages.ParseDMKey(chatKey)
	if err != nil {
		return
	}
	if kindA == "user" && idA != "" {
		s.publishInboxHint(ctx, idA, InboxHint{Category: category})
	}
	if kindB == "user" && idB != "" && (kindA != "user" || idB != idA) {
		s.publishInboxHint(ctx, idB, InboxHint{Category: category})
	}
}

func (sc *actionInboxScope) preloadPromptMessages(ctx context.Context, messageIDs []string) {
	missing := make([]string, 0, len(messageIDs))
	seen := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		if id == "" || sc.promptLoaded[id] {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return
	}
	msgs, err := sc.srv.store.GetMessagesByIDs(ctx, missing)
	if err != nil {
		return
	}
	for _, id := range missing {
		sc.promptLoaded[id] = true
		if m := msgs[id]; m != nil {
			sc.promptMsgMemo[id] = m
		}
	}
}

func (sc *actionInboxScope) getPromptMessage(ctx context.Context, messageID string) *store.Message {
	if messageID == "" {
		return nil
	}
	if sc.promptLoaded[messageID] {
		return sc.promptMsgMemo[messageID]
	}
	sc.promptLoaded[messageID] = true
	if m, err := sc.srv.store.GetMessage(ctx, messageID); err == nil && m != nil {
		sc.promptMsgMemo[messageID] = m
		return m
	}
	return nil
}

func (sc *actionInboxScope) isRecipient(ctx context.Context, chatKey, messageID string, answering bool) bool {
	key := recipientMemoKey{chatKey: chatKey, answering: answering}
	if chatKey == "" {
		key.messageID = messageID
	}
	if cached, ok := sc.recipientMemo[key]; ok {
		return cached
	}
	var promptMsg *store.Message
	if chatKey == "" {
		promptMsg = sc.getPromptMessage(ctx, messageID)
		if promptMsg != nil && promptMsg.ThreadID != "" {
			chatKey = promptMsg.ThreadID
		}
	}
	allowed := sc.srv.isMessageActionRecipient(ctx, sc.user, chatKey, promptMsg, answering)
	sc.recipientMemo[key] = allowed
	return allowed
}

func (sc *actionInboxScope) getAgent(ctx context.Context, agentID string) *store.Agent {
	if agentID == "" {
		return nil
	}
	if sc.agentLoaded[agentID] {
		return sc.agentMemo[agentID]
	}
	sc.agentLoaded[agentID] = true
	if a, err := sc.srv.store.GetAgent(ctx, agentID); err == nil && a != nil {
		sc.agentMemo[agentID] = a
		return a
	}
	return nil
}

// checkMessageActionAgentPerm checks whether user holds action on agentID.
// When the agent has been hard-deleted (loadedAgent == nil after lookup),
// ActionAttach is always denied and other actions (such as ActionRead) fall
// back to a synthetic project-scoped agent resource so retained cards remain
// readable by project readers.
func (s *Server) checkMessageActionAgentPerm(ctx context.Context, user UserIdentity, agentID, projectID string, action Action, loadedAgent *store.Agent, agentWasLoaded bool) (*store.Agent, bool) {
	a := loadedAgent
	if !agentWasLoaded {
		if fetched, err := s.store.GetAgent(ctx, agentID); err == nil {
			a = fetched
		}
	}
	if a == nil {
		if action == ActionAttach {
			return nil, false
		}
		d := s.authzService.CheckAccess(ctx, user, Resource{
			Type:       "agent",
			ID:         agentID,
			ParentType: "project",
			ParentID:   projectID,
		}, action)
		return nil, d.Allowed
	}
	d := s.authzService.CheckAccess(ctx, user, agentResource(a), action)
	return a, d.Allowed
}

func (sc *actionInboxScope) checkAgentPerm(ctx context.Context, agentID, projectID string, action Action) bool {
	key := agentPermMemoKey{agentID: agentID, projectID: projectID, action: action}
	if cached, ok := sc.agentPermMemo[key]; ok {
		return cached
	}
	_, allowed := sc.srv.checkMessageActionAgentPerm(ctx, sc.user, agentID, projectID, action, sc.getAgent(ctx, agentID), true)
	sc.agentPermMemo[key] = allowed
	return allowed
}

func (sc *actionInboxScope) isReadable(ctx context.Context, kind actionInboxKind, row actionInboxRow) bool {
	if !kind.Recipient(ctx, sc, row, false) {
		return false
	}
	return kind.CanRead(ctx, sc, row)
}

func (sc *actionInboxScope) isAnswerable(ctx context.Context, kind actionInboxKind, row actionInboxRow) bool {
	if !row.Class.Answerable() {
		return false
	}
	if !kind.Recipient(ctx, sc, row, true) {
		return false
	}
	return kind.CanRespond(ctx, sc, row)
}

func (sc *actionInboxScope) preloadProjectNames(ctx context.Context, projectIDs []string) {
	missing := make([]string, 0, len(projectIDs))
	seen := make(map[string]struct{}, len(projectIDs))
	for _, pid := range projectIDs {
		if pid == "" {
			continue
		}
		if _, already := sc.projectNames[pid]; already {
			continue
		}
		if _, dup := seen[pid]; dup {
			continue
		}
		seen[pid] = struct{}{}
		missing = append(missing, pid)
	}
	if len(missing) == 0 {
		return
	}
	if res, err := sc.srv.store.ListProjectSummaries(ctx, store.ProjectFilter{
		MemberProjectIDs: missing,
	}, store.ListOptions{
		Limit:          len(missing),
		SkipTotalCount: true,
	}); err == nil && res != nil {
		for _, p := range res.Items {
			name := p.Name
			if name == "" {
				name = p.Slug
			}
			sc.projectNames[p.ID] = name
		}
	}
	for _, pid := range missing {
		if _, ok := sc.projectNames[pid]; !ok {
			if proj, err := sc.srv.store.GetProject(ctx, pid); err == nil && proj != nil {
				name := proj.Name
				if name == "" {
					name = proj.Slug
				}
				sc.projectNames[pid] = name
			} else {
				sc.projectNames[pid] = ""
			}
		}
	}
}

func (sc *actionInboxScope) projectName(ctx context.Context, projectID string) string {
	if projectID == "" {
		return ""
	}
	if name, ok := sc.projectNames[projectID]; ok {
		return name
	}
	sc.preloadProjectNames(ctx, []string{projectID})
	return sc.projectNames[projectID]
}

// collectOpenActionRows is the single shared code path used by both
// handleUserMeActionInbox and actionInboxCategory so the Inbox summary count
// and the page's counts.answerable match by construction.
func (s *Server) collectOpenActionRows(ctx context.Context, sc *actionInboxScope, kind actionInboxKind, now time.Time) ([]authorizedActionRow, bool, error) {
	projIDs, err := sc.projectIDs(ctx)
	if err != nil {
		return nil, false, err
	}
	scanCap := s.actionInboxScanCapLimit()
	raw, err := kind.ListOpen(ctx, actionOpenQuery{
		ProjectIDs: projIDs,
		ViewerID:   sc.viewerID,
		Now:        now,
		ScanCap:    scanCap + 1,
	})
	if err != nil {
		return nil, false, err
	}
	capped := len(raw) > scanCap
	if capped {
		raw = raw[:scanCap]
	}
	out := make([]authorizedActionRow, 0, len(raw))
	for _, row := range raw {
		if !sc.isReadable(ctx, kind, row) {
			continue
		}
		ans := sc.isAnswerable(ctx, kind, row)
		out = append(out, authorizedActionRow{
			Row:        row,
			Answerable: ans,
		})
	}
	return out, capped, nil
}

// actionInboxCategory computes the InboxCategory summary for a message-action
// kind. It is session-only (UATs get state:"unavailable") and gated by
// kind.Experiment() (state:"disabled" when off).
func (s *Server) actionInboxCategory(ctx context.Context, v inboxViewer, kind actionInboxKind) InboxCategory {
	href := kind.Href()
	if !s.experimentEnabled(kind.Experiment()) {
		return InboxCategory{
			State:             InboxStateDisabled,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			Previews:          []any{},
		}
	}
	if v.token != nil {
		return InboxCategory{
			State:             InboxStateUnavailable,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			Previews:          []any{},
		}
	}

	sc, err := s.newActionInboxScope(ctx, v.user, kind.Scope())
	if err != nil {
		code := inboxCategoryErrorCode(ctx, err)
		slog.Warn("inbox summary: category source failed",
			"category", kind.Category(),
			"error_code", code,
			"error", err)
		return InboxCategory{
			State:             InboxStateError,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			ErrorCode:         code,
			Previews:          []any{},
		}
	}

	rows, capped, err := s.collectOpenActionRows(ctx, sc, kind, time.Now().UTC())
	if err != nil {
		code := inboxCategoryErrorCode(ctx, err)
		slog.Warn("inbox summary: category source failed",
			"category", kind.Category(),
			"error_code", code,
			"error", err)
		return InboxCategory{
			State:             InboxStateError,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              href,
			ErrorCode:         code,
			Previews:          []any{},
		}
	}

	count := 0
	seenAgents := make(map[string]struct{})
	seenProjects := make(map[string]struct{})
	var nextExpiresAt *time.Time
	for _, r := range rows {
		if !r.Answerable {
			continue
		}
		count++
		if r.Row.AgentID != "" {
			seenAgents[r.Row.AgentID] = struct{}{}
		}
		if r.Row.ProjectID != "" {
			seenProjects[r.Row.ProjectID] = struct{}{}
		}
		if !r.Row.ExpiresAt.IsZero() {
			exp := r.Row.ExpiresAt.UTC()
			if nextExpiresAt == nil || exp.Before(*nextExpiresAt) {
				t := exp
				nextExpiresAt = &t
			}
		}
	}

	return InboxCategory{
		State:             InboxStateOK,
		Count:             count,
		Capped:            sc.truncatedProjects || capped,
		CountsTowardTotal: true,
		Href:              href,
		Previews:          []any{},
		Breakdown: map[string]int{
			InboxBreakdownAgents:   len(seenAgents),
			InboxBreakdownProjects: len(seenProjects),
		},
		NextExpiresAt: nextExpiresAt,
		projectIDSet:  seenProjects,
	}
}

type actionInboxQueryParams struct {
	viewerID string
	kind     string
	tab      string
	sort     string
	project  string
	agent    string
	q        string
	qLower   string
	limit    int
	cursor   string
}

type actionInboxCursorEnvelope struct {
	Tab      string        `json:"tab"`
	Sort     string        `json:"sort"`
	Bind     string        `json:"bind"`
	Offset   int           `json:"offset,omitempty"`
	Resolved *actionCursor `json:"resolved,omitempty"`
}

func (p actionInboxQueryParams) cursorBind() string {
	h := sha256.New()
	for _, part := range []string{
		p.viewerID,
		p.kind,
		p.tab,
		p.sort,
		p.project,
		p.agent,
		p.qLower,
	} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func encodeActionInboxCursor(p actionInboxQueryParams, offset int, resolved *actionCursor) (string, error) {
	env := actionInboxCursorEnvelope{
		Tab:      p.tab,
		Sort:     p.sort,
		Bind:     p.cursorBind(),
		Offset:   offset,
		Resolved: resolved,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeActionInboxCursor(token string, p actionInboxQueryParams) (actionInboxCursorEnvelope, error) {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) > maxActionInboxCursorSize {
		return actionInboxCursorEnvelope{}, errors.New("malformed cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		return actionInboxCursorEnvelope{}, errors.New("malformed cursor")
	}
	var env actionInboxCursorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return actionInboxCursorEnvelope{}, errors.New("malformed cursor")
	}
	if env.Tab != p.tab || env.Sort != p.sort || env.Bind != p.cursorBind() {
		return actionInboxCursorEnvelope{}, errors.New("cursor does not match query")
	}
	if p.tab == ActionInboxTabOpen {
		if env.Offset <= 0 || env.Resolved != nil {
			return actionInboxCursorEnvelope{}, errors.New("malformed open cursor")
		}
		return env, nil
	}
	if env.Resolved == nil || env.Resolved.ID == "" || env.Resolved.Time.IsZero() || env.Offset != 0 {
		return actionInboxCursorEnvelope{}, errors.New("malformed resolved cursor")
	}
	env.Resolved.Time = env.Resolved.Time.UTC()
	return env, nil
}

func parseActionInboxQuery(w http.ResponseWriter, r *http.Request, viewerID, kindName string) (actionInboxQueryParams, bool) {
	q := r.URL.Query()
	p := actionInboxQueryParams{
		viewerID: viewerID,
		kind:     kindName,
		tab:      strings.TrimSpace(q.Get("tab")),
		sort:     strings.TrimSpace(q.Get("sort")),
		project:  strings.TrimSpace(q.Get("project")),
		agent:    strings.TrimSpace(q.Get("agent")),
		q:        strings.TrimSpace(q.Get("q")),
		limit:    defaultActionInboxLimit,
		cursor:   strings.TrimSpace(q.Get("cursor")),
	}
	p.qLower = strings.ToLower(p.q)

	switch p.tab {
	case "", ActionInboxTabOpen:
		p.tab = ActionInboxTabOpen
	case ActionInboxTabResolved:
	default:
		ValidationError(w, fmt.Sprintf("tab %q is not valid; expected %q or %q",
			p.tab, ActionInboxTabOpen, ActionInboxTabResolved),
			map[string]interface{}{"field": "tab"})
		return actionInboxQueryParams{}, false
	}

	switch p.sort {
	case "", ActionInboxSortRecent:
		p.sort = ActionInboxSortRecent
	case ActionInboxSortProject:
	default:
		ValidationError(w, fmt.Sprintf("sort %q is not valid; expected %q or %q",
			p.sort, ActionInboxSortRecent, ActionInboxSortProject),
			map[string]interface{}{"field": "sort"})
		return actionInboxQueryParams{}, false
	}

	if q.Has("limit") {
		raw := strings.TrimSpace(q.Get("limit"))
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxActionInboxLimit {
			ValidationError(w, fmt.Sprintf("limit must be an integer between 0 and %d", maxActionInboxLimit),
				map[string]interface{}{"field": "limit"})
			return actionInboxQueryParams{}, false
		}
		p.limit = n
	}

	return p, true
}

func actionRowMatchesFilters(row actionInboxRow, p actionInboxQueryParams) bool {
	if p.project != "" && row.ProjectID != p.project {
		return false
	}
	if p.agent != "" && row.AgentID != p.agent {
		return false
	}
	if p.qLower != "" {
		if !strings.Contains(strings.ToLower(row.Key), p.qLower) &&
			!strings.Contains(strings.ToLower(row.Description), p.qLower) &&
			!strings.Contains(strings.ToLower(row.AgentName), p.qLower) {
			return false
		}
	}
	return true
}

func buildActionInboxFacets(ctx context.Context, sc *actionInboxScope, rows []authorizedActionRow) ActionInboxFacets {
	agentCounts := make(map[string]int)
	agentNames := make(map[string]string)
	projectCounts := make(map[string]int)
	projectIDs := make([]string, 0)

	for _, r := range rows {
		if r.Row.AgentID != "" {
			agentCounts[r.Row.AgentID]++
			if r.Row.AgentName != "" && agentNames[r.Row.AgentID] == "" {
				agentNames[r.Row.AgentID] = r.Row.AgentName
			}
		}
		if r.Row.ProjectID != "" {
			if _, ok := projectCounts[r.Row.ProjectID]; !ok {
				projectIDs = append(projectIDs, r.Row.ProjectID)
			}
			projectCounts[r.Row.ProjectID]++
		}
	}
	sc.preloadProjectNames(ctx, projectIDs)

	agents := make([]ActionInboxFacetItem, 0, len(agentCounts))
	for id, count := range agentCounts {
		agents = append(agents, ActionInboxFacetItem{
			ID:    id,
			Name:  agentNames[id],
			Count: count,
		})
	}
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Count != agents[j].Count {
			return agents[i].Count > agents[j].Count
		}
		li, lj := strings.ToLower(agents[i].Name), strings.ToLower(agents[j].Name)
		if li != lj {
			return li < lj
		}
		return agents[i].ID < agents[j].ID
	})

	projects := make([]ActionInboxFacetItem, 0, len(projectCounts))
	for id, count := range projectCounts {
		projects = append(projects, ActionInboxFacetItem{
			ID:    id,
			Name:  sc.projectName(ctx, id),
			Count: count,
		})
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Count != projects[j].Count {
			return projects[i].Count > projects[j].Count
		}
		li, lj := strings.ToLower(projects[i].Name), strings.ToLower(projects[j].Name)
		if li != lj {
			return li < lj
		}
		return projects[i].ID < projects[j].ID
	})

	return ActionInboxFacets{
		Agents:   agents,
		Projects: projects,
	}
}

type actionCardGroup struct {
	messageID   string
	projectID   string
	projectName string
	sortTime    time.Time
	rows        []authorizedActionRow
}

func groupActionRowsByMessage(ctx context.Context, sc *actionInboxScope, rows []authorizedActionRow, useResolvedTime bool) []*actionCardGroup {
	byMsg := make(map[string]*actionCardGroup, len(rows))
	groups := make([]*actionCardGroup, 0, len(rows))
	projIDs := make([]string, 0)
	for _, r := range rows {
		g, ok := byMsg[r.Row.MessageID]
		t := r.Row.CreatedAt
		if useResolvedTime {
			t = r.Row.ResolvedAt
		}
		if !ok {
			g = &actionCardGroup{
				messageID: r.Row.MessageID,
				projectID: r.Row.ProjectID,
				sortTime:  t,
			}
			byMsg[r.Row.MessageID] = g
			groups = append(groups, g)
			if r.Row.ProjectID != "" {
				projIDs = append(projIDs, r.Row.ProjectID)
			}
		} else if t.After(g.sortTime) {
			g.sortTime = t
		}
		g.rows = append(g.rows, r)
	}
	sc.preloadProjectNames(ctx, projIDs)
	for _, g := range groups {
		g.projectName = sc.projectName(ctx, g.projectID)
	}
	return groups
}

func sortActionCardGroups(groups []*actionCardGroup, sortMode string) {
	sort.Slice(groups, func(i, j int) bool {
		if sortMode == ActionInboxSortProject {
			pi := strings.ToLower(groups[i].projectName)
			pj := strings.ToLower(groups[j].projectName)
			if pi != pj {
				return pi < pj
			}
			if groups[i].projectID != groups[j].projectID {
				return groups[i].projectID < groups[j].projectID
			}
		}
		if !groups[i].sortTime.Equal(groups[j].sortTime) {
			return groups[i].sortTime.After(groups[j].sortTime)
		}
		return groups[i].messageID > groups[j].messageID
	})
}

func countActionRows(rows []authorizedActionRow) ActionInboxCounts {
	var c ActionInboxCounts
	c.Open = len(rows)
	for _, r := range rows {
		if r.Answerable {
			c.Answerable++
		}
	}
	return c
}

// handleUserMeActionInbox returns the HTTP handler for
// GET /api/v1/users/me/<action-kind>. Non-GET methods return 405 before the
// experiment gate, which returns 404 while kind.Experiment() is off.
func (s *Server) handleUserMeActionInbox(kind actionInboxKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		s.requireExperiment(kind.Experiment(), func(w http.ResponseWriter, r *http.Request) {
			s.serveUserMeActionInbox(w, r, kind)
		})(w, r)
	}
}

func (s *Server) serveUserMeActionInbox(w http.ResponseWriter, r *http.Request, kind actionInboxKind) {
	user := s.messageActionHumanSessionCaller(w, r, kind.Kind(), "", "")
	if user == nil {
		return
	}
	params, ok := parseActionInboxQuery(w, r, user.ID(), kind.Kind())
	if !ok {
		return
	}
	var cursorEnv actionInboxCursorEnvelope
	if params.cursor != "" {
		var cerr error
		cursorEnv, cerr = decodeActionInboxCursor(params.cursor, params)
		if cerr != nil {
			ValidationError(w, cerr.Error(), map[string]interface{}{"field": "cursor"})
			return
		}
	}

	ctx := withAuthzInputMemo(r.Context())
	ctx = withChatMemberProjectIDsMemo(ctx)

	sc, err := s.newActionInboxScope(ctx, user, kind.Scope())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	now := time.Now().UTC()
	openRows, openCapped, err := s.collectOpenActionRows(ctx, sc, kind, now)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	openCounts := countActionRows(openRows)

	if params.tab == ActionInboxTabOpen {
		facets := buildActionInboxFacets(ctx, sc, openRows)
		filtered := make([]authorizedActionRow, 0, len(openRows))
		for _, r := range openRows {
			if actionRowMatchesFilters(r.Row, params) {
				filtered = append(filtered, r)
			}
		}
		filteredCounts := countActionRows(filtered)

		var cards any = []any{}
		var nextCursor string
		if params.limit > 0 {
			groups := groupActionRowsByMessage(ctx, sc, filtered, false)
			sortActionCardGroups(groups, params.sort)
			start := cursorEnv.Offset
			if start > len(groups) {
				start = len(groups)
			}
			end := start + params.limit
			hasMore := end < len(groups)
			if end > len(groups) {
				end = len(groups)
			}
			pageGroups := groups[start:end]
			pageRows := make([]authorizedActionRow, 0, len(pageGroups))
			for _, g := range pageGroups {
				pageRows = append(pageRows, g.rows...)
			}
			if len(pageRows) > 0 {
				var cerr error
				cards, cerr = kind.CardsFor(ctx, sc, pageRows)
				if cerr != nil {
					writeErrorFromErr(w, cerr, "")
					return
				}
			}
			if hasMore {
				if tok, eerr := encodeActionInboxCursor(params, end, nil); eerr == nil {
					nextCursor = tok
				}
			}
		}

		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, ActionInboxResponse{
			Counts:            openCounts,
			FilteredCounts:    &filteredCounts,
			Cards:             cards,
			Facets:            facets,
			TruncatedProjects: sc.truncatedProjects,
			Capped:            openCapped,
			NextCursor:        nextCursor,
		})
		return
	}

	// Resolved tab: row-keyset pagination over (resolved_at DESC, id DESC).
	// Rows are emitted in keyset order until a readable, filter-matching row
	// belonging to the (limit+1)-th distinct message is encountered (or the DB
	// is exhausted / scanCap is reached). The next cursor points to the last
	// emitted row so no row or card is ever skipped across pages; a multi-row
	// message whose rows resolved at different times may continue as a fragment
	// on a later page. sort=project orders cards within the current page only,
	// and facets cover the emitted rows of the current page.
	if params.limit == 0 {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, ActionInboxResponse{
			Counts: openCounts,
			Cards:  []any{},
			Facets: ActionInboxFacets{
				Agents:   []ActionInboxFacetItem{},
				Projects: []ActionInboxFacetItem{},
			},
			TruncatedProjects: sc.truncatedProjects,
			Capped:            false,
		})
		return
	}

	scanCap := s.actionInboxScanCapLimit()
	batchSize := params.limit * 2
	if batchSize < defaultActionInboxLimit {
		batchSize = defaultActionInboxLimit
	}
	if batchSize > maxActionInboxLimit*2 {
		batchSize = maxActionInboxLimit * 2
	}
	if s.actionInboxResolvedBatchForTest > 0 {
		batchSize = s.actionInboxResolvedBatchForTest
	}

	var cur *actionCursor
	if cursorEnv.Resolved != nil {
		c := *cursorEnv.Resolved
		cur = &c
	}

	scanned := 0
	resolvedCapped := false
	exhausted := false
	hasMore := false
	emitted := make([]authorizedActionRow, 0)
	emittedMsgs := make(map[string]struct{})
	var lastEmitted *actionInboxRow
	var lastDBRow *actionInboxRow

	for !exhausted && !hasMore && !resolvedCapped {
		remaining := scanCap - scanned
		if remaining <= 0 {
			resolvedCapped = true
			break
		}
		reqLimit := batchSize
		if reqLimit > remaining+1 {
			reqLimit = remaining + 1
		}
		projIDs, perr := sc.projectIDs(ctx)
		if perr != nil {
			writeErrorFromErr(w, perr, "")
			return
		}
		batch, lerr := kind.ListResolved(ctx, actionHistoryQuery{
			ProjectIDs:      projIDs,
			ViewerID:        sc.viewerID,
			FilterProjectID: params.project,
			FilterAgentID:   params.agent,
			Cursor:          cur,
			Limit:           reqLimit,
		})
		if lerr != nil {
			writeErrorFromErr(w, lerr, "")
			return
		}
		if len(batch) == 0 {
			exhausted = true
			break
		}
		if len(batch) < reqLimit {
			exhausted = true
		}
		for i := range batch {
			if scanned >= scanCap {
				resolvedCapped = true
				break
			}
			scanned++
			row := batch[i]
			rCopy := row
			lastDBRow = &rCopy
			if !sc.isReadable(ctx, kind, row) {
				continue
			}
			if !actionRowMatchesFilters(row, params) {
				continue
			}
			if _, seen := emittedMsgs[row.MessageID]; !seen {
				if len(emittedMsgs) >= params.limit {
					// This row starts the (limit+1)-th distinct message; stop
					// here and resume strictly after lastEmitted on the next page.
					hasMore = true
					break
				}
				emittedMsgs[row.MessageID] = struct{}{}
			}
			emitted = append(emitted, authorizedActionRow{
				Row:        row,
				Answerable: false,
			})
			lastEmitted = &rCopy
		}
		if hasMore || resolvedCapped {
			break
		}
		if lastDBRow != nil {
			cur = &actionCursor{
				Time: lastDBRow.ResolvedAt.UTC(),
				ID:   lastDBRow.ID,
			}
		}
	}

	facets := buildActionInboxFacets(ctx, sc, emitted)
	groups := groupActionRowsByMessage(ctx, sc, emitted, true)
	// Within-page ordering: sort=recent preserves recency; sort=project groups
	// the page's cards by project name without altering the row-keyset cursor.
	sortActionCardGroups(groups, params.sort)

	pageRows := make([]authorizedActionRow, 0, len(emitted))
	for _, g := range groups {
		pageRows = append(pageRows, g.rows...)
	}

	var cards any = []any{}
	if len(pageRows) > 0 {
		var cerr error
		cards, cerr = kind.CardsFor(ctx, sc, pageRows)
		if cerr != nil {
			writeErrorFromErr(w, cerr, "")
			return
		}
	}

	var cursorRow *actionInboxRow
	if hasMore {
		cursorRow = lastEmitted
	} else if resolvedCapped && !exhausted {
		cursorRow = lastDBRow
	}

	var nextCursor string
	if cursorRow != nil && !cursorRow.ResolvedAt.IsZero() && cursorRow.ID != "" {
		if tok, eerr := encodeActionInboxCursor(params, 0, &actionCursor{
			Time: cursorRow.ResolvedAt.UTC(),
			ID:   cursorRow.ID,
		}); eerr == nil {
			nextCursor = tok
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, ActionInboxResponse{
		Counts:            openCounts,
		Cards:             cards,
		Facets:            facets,
		TruncatedProjects: sc.truncatedProjects,
		Capped:            resolvedCapped,
		NextCursor:        nextCursor,
	})
}
