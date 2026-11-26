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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Pagination bounds and tab names for GET /api/v1/projects/{id}/decisions.
const (
	defaultProjectDecisionsLimit = 50
	maxProjectDecisionsLimit     = 200

	// Search (q) bounds, in runes after trimming (design D1).
	minProjectDecisionsQueryRunes = 2
	maxProjectDecisionsQueryRunes = 256
	// maxProjectDecisionsConversationBytes bounds the conversation param,
	// the decisions.chat_key column width.
	maxProjectDecisionsConversationBytes = 256
	// maxProjectDecisionFacetOptions caps the agents and conversations
	// facets after sorting by label (design D5).
	maxProjectDecisionFacetOptions = 500

	ProjectDecisionsTabPending = "pending"
	ProjectDecisionsTabMade    = "made"

	// Per-row waiting reasons when a decision is in "Waiting on others".
	WaitingReasonReadOnly         = "read_only"
	WaitingReasonMessageDenied    = "message_denied"
	WaitingReasonOtherUserConfirm = "other_user_confirm"
)

// ProjectDecisionCounts holds the live pending counts across the project's
// conversations the caller can read.
type ProjectDecisionCounts struct {
	WaitingOnYou    int `json:"waitingOnYou"`
	WaitingOnOthers int `json:"waitingOnOthers"`
}

// ProjectDecisionActorView is one filter-facet option (an asking agent or a
// recorded answerer) scoped to conversations the caller can read. Hint tells
// apart agents whose names collide ("deleted · 3f9c"); clients show it muted.
// ProjectID is set on asking agents (an agent belongs to one project, design
// D5) and omitted on answerers (users span projects).
type ProjectDecisionActorView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Hint      string `json:"hint,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
}

// ProjectDecisionConversationView is one conversations-facet option: a
// readable conversation with at least one decision.
type ProjectDecisionConversationView struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	ProjectID string `json:"projectId"`
}

// ProjectDecisionProjectRef names the project a prompt or log entry belongs
// to (design S2).
type ProjectDecisionProjectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// ProjectDecisionPromptView is one prompt card on tab=pending.
type ProjectDecisionPromptView struct {
	Project               ProjectDecisionProjectRef `json:"project"`
	PromptMessageID       string                    `json:"promptMessageId"`
	PromptAvailable       bool                      `json:"promptAvailable"`
	ChatKey               string                    `json:"chatKey"`
	ConversationLabel     string                    `json:"conversationLabel"`
	ConversationAvailable bool                      `json:"conversationAvailable"`
	AskerAgentID          string                    `json:"askerAgentId"`
	AskerAgentName        string                    `json:"askerAgentName"`
	CreatedAt             time.Time                 `json:"createdAt"`
	AnswerMode            string                    `json:"answerMode"`
	WaitingOnYou          bool                      `json:"waitingOnYou"`
	CanAnswer             bool                      `json:"canAnswer"`
	DisabledReason        string                    `json:"disabledReason,omitempty"`
	WaitingReasons        map[string]string         `json:"waitingReasons,omitempty"`
	Decisions             []DecisionView            `json:"decisions"`
}

// ProjectDecisionSummaryView is the decision metadata embedded in one entry of
// the tab=made decision log.
type ProjectDecisionSummaryView struct {
	RowID           string                `json:"rowId"`
	PromptMessageID string                `json:"promptMessageId"`
	PromptAvailable bool                  `json:"promptAvailable"`
	Key             string                `json:"key"`
	Ref             string                `json:"ref"`
	Version         int                   `json:"version"`
	Status          string                `json:"status"`
	Question        string                `json:"question"`
	Choices         []messages.Choice     `json:"choices"`
	DefaultChoiceID string                `json:"defaultChoiceId,omitempty"`
	Other           *messages.OtherOption `json:"other,omitempty"`
	ReplacedBy      *DecisionRef          `json:"replacedBy,omitempty"`
	AskerAgentID    string                `json:"askerAgentId"`
	AskerAgentName  string                `json:"askerAgentName"`
}

// ProjectDecisionLogEntryView is one Made-tab entry: a decision version's final answer, with any superseded interpreted answer folded in.
type ProjectDecisionLogEntryView struct {
	Project                ProjectDecisionProjectRef  `json:"project"`
	AnswerID               string                     `json:"answerId"`
	Decision               ProjectDecisionSummaryView `json:"decision"`
	ChoiceID               string                     `json:"choiceId"`
	Label                  string                     `json:"label"`
	OtherText              string                     `json:"otherText,omitempty"`
	MatchedDefault         bool                       `json:"matchedDefault"`
	PreviousChoiceLabel    string                     `json:"previousChoiceLabel,omitempty"`
	PreviousOtherText      string                     `json:"previousOtherText,omitempty"`
	InterpretedAnswer      *AnswerView                `json:"interpretedAnswer,omitempty"`
	AwaitingConfirmation   bool                       `json:"awaitingConfirmation,omitempty"`
	AnsweredByUserID       string                     `json:"answeredByUserId,omitempty"`
	AnsweredByName         string                     `json:"answeredByName"`
	Provenance             string                     `json:"provenance"`
	Outcome                string                     `json:"outcome"`
	DismissReason          string                     `json:"dismissReason,omitempty"`
	Surface                string                     `json:"surface"`
	InterpretedByAgentID   string                     `json:"interpretedByAgentId,omitempty"`
	InterpretedByAgentName string                     `json:"interpretedByAgentName,omitempty"`
	SourceMessageID        string                     `json:"sourceMessageId,omitempty"`
	SourceMessageAvailable bool                       `json:"sourceMessageAvailable"`
	ResponseMessageID      string                     `json:"responseMessageId,omitempty"`
	BatchID                string                     `json:"batchId,omitempty"`
	At                     time.Time                  `json:"at"`
	ChatKey                string                     `json:"chatKey"`
	ConversationLabel      string                     `json:"conversationLabel"`
	ConversationAvailable  bool                       `json:"conversationAvailable"`
}

// DecisionRetentionInfo describes the effective settled-decision retention
// policy for a project (design-8984 §3.5). Present only when the
// hub.decision_retention experiment is on, retention is enabled, and the
// project has an effective window.
type DecisionRetentionInfo struct {
	Active bool `json:"active"`
	Days   int  `json:"days"`
}

// ProjectPendingDecisionsResponse is the response of GET /api/v1/projects/{id}/decisions?tab=pending.
// Counts are never filtered; FilteredCounts is present when q, conversation
// or agent narrows the list (design D6).
type ProjectPendingDecisionsResponse struct {
	Counts          ProjectDecisionCounts             `json:"counts"`
	FilteredCounts  *ProjectDecisionCounts            `json:"filteredCounts,omitempty"`
	Prompts         []ProjectDecisionPromptView       `json:"prompts"`
	Agents          []ProjectDecisionActorView        `json:"agents"`
	Answerers       []ProjectDecisionActorView        `json:"answerers"`
	Conversations   []ProjectDecisionConversationView `json:"conversations"`
	FacetsTruncated *ProjectDecisionFacetsTruncated   `json:"facetsTruncated,omitempty"`
	Retention       *DecisionRetentionInfo            `json:"retention,omitempty"`
	NextCursor      string                            `json:"nextCursor,omitempty"`
}

// ProjectMadeDecisionsResponse is the response of GET /api/v1/projects/{id}/decisions?tab=made.
type ProjectMadeDecisionsResponse struct {
	Counts          ProjectDecisionCounts             `json:"counts"`
	Entries         []ProjectDecisionLogEntryView     `json:"entries"`
	Agents          []ProjectDecisionActorView        `json:"agents"`
	Answerers       []ProjectDecisionActorView        `json:"answerers"`
	Conversations   []ProjectDecisionConversationView `json:"conversations"`
	FacetsTruncated *ProjectDecisionFacetsTruncated   `json:"facetsTruncated,omitempty"`
	Retention       *DecisionRetentionInfo            `json:"retention,omitempty"`
	NextCursor      string                            `json:"nextCursor,omitempty"`
}

// projectDecisionRetentionInfo returns the effective retention info for
// projectID from cached operational settings (0 DB queries), or nil when the
// experiment is off, retention is disabled, or the project has no window.
func (s *Server) projectDecisionRetentionInfo(projectID string) *DecisionRetentionInfo {
	if projectID == "" || !s.experimentEnabled(experiments.DecisionRetention) {
		return nil
	}
	cfg := s.decisionRetentionConfig()
	if !cfg.Enabled {
		return nil
	}
	days, ok := cfg.EffectiveDays(projectID)
	if !ok {
		return nil
	}
	return &DecisionRetentionInfo{Active: true, Days: days}
}

// profileDecisionRetentionInfo returns per-project retention info for all
// projects in projectIDs that have an active retention window, or nil when
// none do (or when the experiment is off / retention is disabled).
func (s *Server) profileDecisionRetentionInfo(projectIDs []string) map[string]DecisionRetentionInfo {
	if len(projectIDs) == 0 || !s.experimentEnabled(experiments.DecisionRetention) {
		return nil
	}
	cfg := s.decisionRetentionConfig()
	if !cfg.Enabled {
		return nil
	}
	var out map[string]DecisionRetentionInfo
	for _, pid := range projectIDs {
		days, ok := cfg.EffectiveDays(pid)
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]DecisionRetentionInfo)
		}
		out[pid] = DecisionRetentionInfo{Active: true, Days: days}
	}
	return out
}

// pendingPromptCursor is the keyset cursor for tab=pending: section 0 is
// "Waiting on you" prompts and section 1 is "Waiting on others"-only prompts.
// DoneProjects and ProjectID are populated on profile sort=project cursors.
type pendingPromptCursor struct {
	Section      int       `json:"s"`
	DoneProjects []string  `json:"d,omitempty"`
	ProjectID    string    `json:"p,omitempty"`
	CreatedAt    time.Time `json:"t"`
	MessageID    string    `json:"m"`
}

func encodePendingPromptCursor(section int, createdAt time.Time, messageID string) (string, error) {
	raw, err := json.Marshal(pendingPromptCursor{
		Section:   section,
		CreatedAt: createdAt.UTC(),
		MessageID: messageID,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodePendingPromptCursor(token string) (pendingPromptCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return pendingPromptCursor{}, err
	}
	var c pendingPromptCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return pendingPromptCursor{}, err
	}
	if (c.Section != 0 && c.Section != 1) || c.MessageID == "" || c.CreatedAt.IsZero() || len(c.DoneProjects) > 0 || c.ProjectID != "" {
		return pendingPromptCursor{}, errors.New("malformed pending cursor")
	}
	c.CreatedAt = c.CreatedAt.UTC()
	return c, nil
}

// decisionViewsFor builds DecisionView slices for ds in slice order, reading
// any replacing decision rows in one batch.
func (s *Server) decisionViewsFor(ctx context.Context, ds []*store.Decision) []DecisionView {
	if len(ds) == 0 {
		return []DecisionView{}
	}
	var replacedByIDs []string
	for _, d := range ds {
		if d != nil && d.ReplacedByID != "" {
			replacedByIDs = append(replacedByIDs, d.ReplacedByID)
		}
	}
	replacedBy := s.decisionRows(ctx, replacedByIDs)
	out := make([]DecisionView, 0, len(ds))
	for _, d := range ds {
		if d == nil {
			continue
		}
		out = append(out, decisionView(d, replacedBy[d.ReplacedByID], s.decisionDismissMinAge()))
	}
	return out
}

// handleProjectDecisions serves GET /api/v1/projects/{id}/decisions.
// Unsupported methods return 405 before the experiment gate, which returns 404
// while web.decisions is off.
func (s *Server) handleProjectDecisions(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.requireExperiment(experiments.Decisions, func(w http.ResponseWriter, r *http.Request) {
		s.listProjectDecisions(w, r, projectID)
	})(w, r)
}

// projectDecisionsCaller returns the authenticated user identity (web session,
// dev credential, or scoped UAT) and optional UAT token, or writes the 401/403
// refusal for agents, visitors, federated users, or unadmitted credentials.
func projectDecisionsCaller(w http.ResponseWriter, r *http.Request, projectID string) (UserIdentity, *ScopedUserIdentity, bool) {
	ctx := r.Context()
	if GetAgentIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agents cannot read project decisions", nil)
		return nil, nil, false
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		Unauthorized(w)
		return nil, nil, false
	}
	if isVisitorIdentity(identity) {
		logAuthzDenial(r, identity, Resource{Type: "project", ID: projectID}, ActionRead, "project decisions are not open to visitors")
		writeVisitorRefusal(w)
		return nil, nil, false
	}
	cls, token := inboxCredentialOf(ctx)
	if cls != inboxCredentialSession && cls != inboxCredentialToken {
		logAuthzDenial(r, identity, Resource{Type: "project", ID: projectID}, ActionRead, "credential cannot read project decisions")
		Forbidden(w)
		return nil, nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, nil, false
	}
	return user, token, true
}

// projectDecisionsQuery is one validated decisions query. q, conversation
// and agentID apply to both tabs; madeFilter holds the Made-only filters
// (its AgentID mirrors agentID). On the profile endpoint (profile == true),
// sort and projectID also apply.
type projectDecisionsQuery struct {
	profile                bool
	viewerID               string
	tab                    string
	sort                   string
	projectID              string
	truncatedProjects      bool
	limit                  int
	cursor                 string
	pendingCursor          *pendingPromptCursor
	q                      string
	parsedQuery            *filterquery.Query
	conversation           string
	agentID                string
	madeFilter             store.ProjectDecisionAnswerFilter
	onUnfilteredBellCounts func(total, projects int)
}

// hasPendingStructuredFilter reports whether parsedQuery contains any term or
// text clause that applies to tab=pending (ignoring Made-only keys by/provenance).
func (q projectDecisionsQuery) hasPendingStructuredFilter() bool {
	if q.parsedQuery == nil || q.parsedQuery.Mode != filterquery.ModeStructured {
		return false
	}
	if q.parsedQuery.Text != "" || len(q.parsedQuery.ExcludeText) > 0 {
		return true
	}
	for _, t := range q.parsedQuery.Terms {
		switch strings.ToLower(t.Key) {
		// "project" is used by the profile-decisions schema in Phase 3 (ptone/scion#9348).
		case "agent", "conversation", "project":
			return true
		}
	}
	return false
}

// filtered reports whether a filter that applies to tab=pending is set.
func (q projectDecisionsQuery) filtered() bool {
	return q.q != "" || q.conversation != "" || q.agentID != "" || q.projectID != "" || q.hasPendingStructuredFilter()
}

func parseProjectDecisionsQuery(w http.ResponseWriter, r *http.Request) (projectDecisionsQuery, bool) {
	q := r.URL.Query()
	out := projectDecisionsQuery{
		tab:   strings.TrimSpace(q.Get("tab")),
		limit: defaultProjectDecisionsLimit,
	}
	switch out.tab {
	case "", ProjectDecisionsTabPending:
		out.tab = ProjectDecisionsTabPending
	case ProjectDecisionsTabMade:
	default:
		ValidationError(w, fmt.Sprintf("tab %q is not valid; expected %q or %q", out.tab, ProjectDecisionsTabPending, ProjectDecisionsTabMade),
			map[string]interface{}{"field": "tab"})
		return projectDecisionsQuery{}, false
	}

	if q.Has("limit") {
		rawLimit := strings.TrimSpace(q.Get("limit"))
		if rawLimit != "" {
			n, err := strconv.Atoi(rawLimit)
			if err != nil || n < 0 || n > maxProjectDecisionsLimit {
				ValidationError(w, fmt.Sprintf("limit must be an integer between 0 and %d", maxProjectDecisionsLimit),
					map[string]interface{}{"field": "limit"})
				return projectDecisionsQuery{}, false
			}
			out.limit = n
		}
	}

	out.cursor = strings.TrimSpace(q.Get("cursor"))
	if out.tab == ProjectDecisionsTabPending && out.cursor != "" {
		cur, err := decodePendingPromptCursor(out.cursor)
		if err != nil {
			ValidationError(w, "invalid cursor", map[string]interface{}{"field": "cursor"})
			return projectDecisionsQuery{}, false
		}
		out.pendingCursor = &cur
	}

	textQ, parsedQ, ok := parseDecisionsFilterQ(w, q.Get("q"), schemas.Decisions)
	if !ok {
		return projectDecisionsQuery{}, false
	}
	out.q = textQ
	out.parsedQuery = parsedQ
	out.conversation = strings.TrimSpace(q.Get("conversation"))
	if len(out.conversation) > maxProjectDecisionsConversationBytes {
		ValidationError(w, fmt.Sprintf("conversation must be at most %d bytes", maxProjectDecisionsConversationBytes),
			map[string]interface{}{"field": "conversation"})
		return projectDecisionsQuery{}, false
	}
	if agentParam := strings.TrimSpace(q.Get("agent")); agentParam != "" {
		if _, err := uuid.Parse(agentParam); err != nil {
			ValidationError(w, "agent must be a valid UUID", map[string]interface{}{"field": "agent"})
			return projectDecisionsQuery{}, false
		}
		out.agentID = agentParam
	}

	if out.tab == ProjectDecisionsTabMade {
		out.madeFilter.AgentID = out.agentID
		out.madeFilter.UserID = strings.TrimSpace(q.Get("user"))

		prov := strings.TrimSpace(q.Get("provenance"))
		switch prov {
		case "":
		case store.ProjectDecisionProvenanceDirect,
			store.ProjectDecisionProvenanceInterpreted,
			store.ProjectDecisionProvenanceConfirmed,
			store.ProjectDecisionProvenanceOverridden,
			store.ProjectDecisionProvenanceDismissed:
			out.madeFilter.Provenance = prov
		case store.DecisionProvenanceAgentInterpreted:
			out.madeFilter.Provenance = store.ProjectDecisionProvenanceInterpreted
		default:
			ValidationError(w, "provenance must be one of direct, interpreted, confirmed, overridden, dismissed",
				map[string]interface{}{"field": "provenance"})
			return projectDecisionsQuery{}, false
		}

		if rawFrom := strings.TrimSpace(q.Get("from")); rawFrom != "" {
			t, err := parseDecisionDateBound(rawFrom, false)
			if err != nil {
				ValidationError(w, "from must be YYYY-MM-DD or RFC3339", map[string]interface{}{"field": "from"})
				return projectDecisionsQuery{}, false
			}
			out.madeFilter.From = &t
		}
		if rawTo := strings.TrimSpace(q.Get("to")); rawTo != "" {
			t, err := parseDecisionDateBound(rawTo, true)
			if err != nil {
				ValidationError(w, "to must be YYYY-MM-DD or RFC3339", map[string]interface{}{"field": "to"})
				return projectDecisionsQuery{}, false
			}
			out.madeFilter.To = &t
		}
		if out.madeFilter.From != nil && out.madeFilter.To != nil && out.madeFilter.From.After(*out.madeFilter.To) {
			ValidationError(w, "from must not be after to", map[string]interface{}{"field": "from"})
			return projectDecisionsQuery{}, false
		}
	}

	return out, true
}

// parseDecisionDateBound parses YYYY-MM-DD (start or end of UTC day) or RFC3339.
func parseDecisionDateBound(raw string, endOfDay bool) (time.Time, error) {
	if d, err := time.Parse("2006-01-02", raw); err == nil {
		if endOfDay {
			return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 999999999, time.UTC), nil
		}
		return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

type projectReadableScope struct {
	project               *store.Project
	chatKeys              []string
	conversationLabel     map[string]string
	conversationAvailable map[string]bool
	userCanPost           map[string]bool
	agentsByID            map[string]*store.Agent
	queriedAgentIDs       map[string]bool
	liveAgents            map[string]*store.Agent
	agentNames            map[string]string
}

// resolveProjectReadableScope computes the conversations in project that user
// can read (readable topics + viewer's own canonical DMs with project agents),
// along with labels, availability, chat-post capability, and agent display
// names. DMs are strictly participant-only (dm:agent:<agentUUID>:user:<viewerID>),
// even for project owners/admins and hub super-admins.
func (s *Server) resolveProjectReadableScope(
	ctx context.Context,
	project *store.Project,
	user UserIdentity,
	token *ScopedUserIdentity,
	wcs WebChatStore,
) (*projectReadableScope, error) {
	scopes, err := s.resolveProfileDecisionScopes(ctx, []*store.Project{project}, user, token, wcs)
	if err != nil {
		return nil, err
	}
	return scopes[0], nil
}

// buildProjectReadableScope assembles one project's readable conversation scope
// from pre-fetched access, topics, asking-agent summaries, and agent rows
// (design-scale §3.4). It performs no store or web-chat queries.
func (s *Server) buildProjectReadableScope(
	ctx context.Context,
	project *store.Project,
	viewerID string,
	token *ScopedUserIdentity,
	dmTokenCheck *selfScopeCheck,
	projectChatAccess ChatAccess,
	topics []WebChatTopic,
	decAgents []store.ProjectDecisionActor,
	agentsByID map[string]*store.Agent,
) *projectReadableScope {
	scope := &projectReadableScope{
		project:               project,
		conversationLabel:     make(map[string]string),
		conversationAvailable: make(map[string]bool),
		userCanPost:           make(map[string]bool),
		agentsByID:            make(map[string]*store.Agent),
		queriedAgentIDs:       make(map[string]bool),
		liveAgents:            make(map[string]*store.Agent),
		agentNames:            make(map[string]string),
	}
	seenKeys := make(map[string]bool)

	// 1. Readable topics in the project (including soft-deleted topics so
	// audit history is never erased; soft-deleted topics report
	// conversationAvailable=false and userCanPost=false).
	if projectChatAccess.Can(ChatActionRead) {
		canPostInActiveTopic := projectChatAccess.Can(ChatActionPost)
		for _, t := range topics {
			if !seenKeys[t.ID] {
				seenKeys[t.ID] = true
				scope.chatKeys = append(scope.chatKeys, t.ID)
			}
			scope.conversationLabel[t.ID] = "#" + strings.TrimPrefix(t.Name, "#")
			if t.DeletedAt == nil {
				scope.conversationAvailable[t.ID] = true
				scope.userCanPost[t.ID] = canPostInActiveTopic
			} else {
				scope.conversationAvailable[t.ID] = false
				scope.userCanPost[t.ID] = false
			}
		}
	}

	// 2. Collect distinct asking agent UUIDs that actually appear on decisions
	// in this project and populate their cached agent rows and display names.
	candidateAgentIDs := make([]string, 0, len(decAgents))
	for _, da := range decAgents {
		if da.Name != "" {
			scope.agentNames[da.ID] = da.Name
		}
		scope.queriedAgentIDs[da.ID] = true
		candidateAgentIDs = append(candidateAgentIDs, da.ID)
		if ag := agentsByID[da.ID]; ag != nil {
			scope.agentsByID[da.ID] = ag
			if name := agentDisplayName(ag); name != "" {
				scope.agentNames[da.ID] = name
			}
			if ag.DeletedAt.IsZero() {
				scope.liveAgents[da.ID] = ag
			}
		}
	}

	// 3. Build viewer's canonical DM keys dm:agent:<agentUUID>:user:<viewerID>.
	// Strictly participant-only even for project owners/admins and hub admins.
	for _, agentID := range candidateAgentIDs {
		dmKey, err := messages.DMConversationKey("agent", agentID, "user", viewerID)
		if err != nil {
			continue
		}
		if token != nil && !s.chatTokenDMVisible(ctx, token, dmTokenCheck, dmKey) {
			continue
		}
		if !seenKeys[dmKey] {
			seenKeys[dmKey] = true
			scope.chatKeys = append(scope.chatKeys, dmKey)
		}
		name := scope.agentNames[agentID]
		if name == "" {
			name = agentID
		}
		scope.conversationLabel[dmKey] = "DM with " + name
		scope.conversationAvailable[dmKey] = scope.liveAgents[agentID] != nil
		scope.userCanPost[dmKey] = true
	}

	return scope
}

func (s *Server) listProjectDecisions(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	user, token, ok := projectDecisionsCaller(w, r, projectID)
	if !ok {
		return
	}

	pq, ok := parseProjectDecisionsQuery(w, r)
	if !ok {
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	// A search runs under the chat search time limit (design D9).
	searchCtx := ctx
	if pq.q != "" || pq.parsedQuery != nil {
		var cancel context.CancelFunc
		searchCtx, cancel = context.WithTimeout(ctx, s.chatSearchTimeout())
		defer cancel()
	}

	scope, err := s.resolveProjectReadableScope(searchCtx, project, user, token, wcs)
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	resp, err := s.decisionsCore(searchCtx, user, token, wcs, []*projectReadableScope{scope}, pq)
	if err == nil && errors.Is(searchCtx.Err(), context.DeadlineExceeded) {
		// A step that tolerates a failed lookup may have run past the
		// limit: no partial results, as in chat search.
		err = searchCtx.Err()
	}
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeDecisionsError maps a decisions query failure the way chat search
// does (handlers_chat_v2.go finishCtxErr): a client that went away gets no
// body; a passed search deadline is 503 SEARCH_TIMEOUT whatever error the
// interrupted query surfaced (a SQLite interrupt during rows.Next is not a
// context error); an invalid filter or cursor is 400.
func writeDecisionsError(w http.ResponseWriter, r *http.Request, searchCtx context.Context, err error) {
	switch {
	case r.Context().Err() != nil:
		return
	case errors.Is(searchCtx.Err(), context.DeadlineExceeded), isChatSearchTimeout(err):
		writeChatSearchTimeout(w)
	case errors.Is(err, store.ErrInvalidInput):
		ValidationError(w, "invalid query filter or cursor", map[string]interface{}{"field": "cursor"})
	default:
		writeErrorFromErr(w, err, "")
	}
}

// decisionScopeSet is the union of one or more projects' readable scopes.
// Chat keys are unique across the hub and an agent belongs to one project,
// so the per-key and per-agent maps merge without qualification.
type decisionScopeSet struct {
	*projectReadableScope
	projectIDs []string
	projects   map[string]*store.Project
	keyProject map[string]string // chat key -> project ID
	readable   map[string]bool
}

func newDecisionScopeSet(scopes []*projectReadableScope) *decisionScopeSet {
	set := &decisionScopeSet{
		projects:   make(map[string]*store.Project, len(scopes)),
		keyProject: make(map[string]string),
		readable:   make(map[string]bool),
	}
	if len(scopes) == 1 {
		set.projectReadableScope = scopes[0]
	} else {
		set.projectReadableScope = &projectReadableScope{
			conversationLabel:     make(map[string]string),
			conversationAvailable: make(map[string]bool),
			userCanPost:           make(map[string]bool),
			agentsByID:            make(map[string]*store.Agent),
			queriedAgentIDs:       make(map[string]bool),
			liveAgents:            make(map[string]*store.Agent),
			agentNames:            make(map[string]string),
		}
	}
	merged := set.projectReadableScope
	for _, sc := range scopes {
		if sc == nil || sc.project == nil {
			continue
		}
		pid := sc.project.ID
		if set.projects[pid] == nil {
			set.projects[pid] = sc.project
			set.projectIDs = append(set.projectIDs, pid)
		}
		for _, k := range sc.chatKeys {
			if set.readable[k] {
				continue
			}
			set.readable[k] = true
			set.keyProject[k] = pid
			if len(scopes) > 1 {
				merged.chatKeys = append(merged.chatKeys, k)
			}
		}
		if len(scopes) == 1 {
			continue
		}
		for k, v := range sc.conversationLabel {
			merged.conversationLabel[k] = v
		}
		for k, v := range sc.conversationAvailable {
			merged.conversationAvailable[k] = v
		}
		for k, v := range sc.userCanPost {
			merged.userCanPost[k] = v
		}
		for k, v := range sc.agentsByID {
			merged.agentsByID[k] = v
		}
		for k, v := range sc.queriedAgentIDs {
			merged.queriedAgentIDs[k] = v
		}
		for k, v := range sc.liveAgents {
			merged.liveAgents[k] = v
		}
		for k, v := range sc.agentNames {
			merged.agentNames[k] = v
		}
	}
	return set
}

// projectRef returns the S2 project reference for projectID.
func (set *decisionScopeSet) projectRef(projectID string) ProjectDecisionProjectRef {
	if p := set.projects[projectID]; p != nil {
		return ProjectDecisionProjectRef{ID: p.ID, Name: p.Name, Slug: p.Slug}
	}
	return ProjectDecisionProjectRef{ID: projectID}
}

// labelMatchKeys returns the keys among keys whose conversation label
// contains the case-folded search qFold (design D2).
func (set *decisionScopeSet) labelMatchKeys(keys []string, qFold string) []string {
	var out []string
	for _, k := range keys {
		if strings.Contains(strings.ToLower(set.conversationLabel[k]), qFold) {
			out = append(out, k)
		}
	}
	return out
}

// decisionsCore runs the decisions query for viewer over scopes: facets,
// unfiltered counts, then the pending page (filtered in memory) or the made
// page (filtered in the store). The project endpoint passes one scope.
func (s *Server) decisionsCore(
	ctx context.Context,
	user UserIdentity,
	token *ScopedUserIdentity,
	wcs WebChatStore,
	scopes []*projectReadableScope,
	pq projectDecisionsQuery,
) (any, error) {
	set := newDecisionScopeSet(scopes)

	facets := decisionFacetsBundle{
		agents:        []ProjectDecisionActorView{},
		answerers:     []ProjectDecisionActorView{},
		conversations: []ProjectDecisionConversationView{},
		projects:      []ProjectDecisionProjectRef{},
	}
	if len(set.chatKeys) > 0 && (pq.limit > 0 || pq.parsedQuery != nil) {
		kind := decisionFacetAll
		if pq.limit == 0 {
			kind = decisionFacetRawOnly
		}
		var err error
		facets, err = s.buildDecisionFacets(ctx, set, kind, true, pq.profile)
		if err != nil {
			return nil, err
		}
	}
	facetAgents := facets.rawAgents
	facetAnswerers := facets.rawAnswerers
	agentsFacet := facets.agents
	answerersFacet := facets.answerers
	conversationsFacet := facets.conversations
	projectsFacet := facets.projects
	facetsTruncated := facets.truncated

	// The keys and projects the list is narrowed to. An unreadable conversation
	// or non-member project narrows to nothing, so neither is an existence
	// oracle (design D4, R1).
	listKeys := set.chatKeys
	if pq.conversation != "" {
		listKeys = nil
		if set.readable[pq.conversation] {
			listKeys = []string{pq.conversation}
		}
	}
	targetProjectIDs := set.projectIDs
	if pq.projectID != "" {
		targetProjectIDs = nil
		if set.projects[pq.projectID] != nil {
			targetProjectIDs = []string{pq.projectID}
		}
		var projKeys []string
		for _, k := range listKeys {
			if set.keyProject[k] == pq.projectID {
				projKeys = append(projKeys, k)
			}
		}
		listKeys = projKeys
	}

	resolved := resolveProjectDecisionsFilterQuery(set, user.ID(), pq, facetAgents, facetAnswerers, listKeys)
	if pq.parsedQuery != nil {
		listKeys = resolved.listKeys
	}

	// Live counts and classified prompt cards across all open prompts.
	var openPrompts []*store.ProjectDecisionPrompt
	if len(set.chatKeys) > 0 {
		var err error
		openPrompts, err = s.store.ListOpenPrompts(ctx, set.projectIDs, set.chatKeys)
		if err != nil {
			return nil, err
		}
	}
	counts, classified, err := s.classifyProjectOpenPrompts(ctx, user, token, wcs, set.projectReadableScope, openPrompts)
	if err != nil {
		return nil, err
	}
	if pq.onUnfilteredBellCounts != nil {
		pq.onUnfilteredBellCounts(counts.WaitingOnYou, countProjectsWithWaiting(classified))
	}

	if pq.tab == ProjectDecisionsTabPending {
		var filteredCounts *ProjectDecisionCounts
		if pq.filtered() {
			if resolved.impossible {
				classified = nil
			} else {
				classified = filterClassifiedPrompts(set, classified, listKeys, pq, resolved)
			}
			fc := sumClassifiedCounts(classified)
			filteredCounts = &fc
		}
		if pq.profile {
			return s.finishProfilePendingDecisions(ctx, set, pq, targetProjectIDs, counts, filteredCounts, classified, agentsFacet, answerersFacet, conversationsFacet, projectsFacet, facetsTruncated)
		}
		pagePrompts, nextCursor, err := paginateClassifiedPrompts(classified, pq.pendingCursor, pq.limit)
		if err != nil {
			return nil, err
		}
		var projectID string
		if len(set.projectIDs) > 0 {
			projectID = set.projectIDs[0]
		}
		return ProjectPendingDecisionsResponse{
			Counts:          counts,
			FilteredCounts:  filteredCounts,
			Prompts:         s.buildProjectDecisionPromptViews(ctx, set, pagePrompts),
			Agents:          agentsFacet,
			Answerers:       answerersFacet,
			Conversations:   conversationsFacet,
			FacetsTruncated: facetsTruncated,
			Retention:       s.projectDecisionRetentionInfo(projectID),
			NextCursor:      nextCursor,
		}, nil
	}

	// tab=made
	if resolved.impossible || (pq.parsedQuery != nil && len(listKeys) == 0) {
		if pq.profile {
			return s.finishProfileMadeDecisions(ctx, wcs, set, pq, nil, nil, pq.madeFilter, counts, agentsFacet, answerersFacet, conversationsFacet, projectsFacet, facetsTruncated)
		}
		return ProjectMadeDecisionsResponse{
			Counts:          counts,
			Entries:         []ProjectDecisionLogEntryView{},
			Agents:          agentsFacet,
			Answerers:       answerersFacet,
			Conversations:   conversationsFacet,
			FacetsTruncated: facetsTruncated,
		}, nil
	}
	filter := pq.madeFilter
	if pq.parsedQuery != nil {
		if len(resolved.agentIDs) > 0 {
			filter.AgentID = ""
			filter.AgentIDs = resolved.agentIDs
		}
		filter.ExcludeAgentIDs = resolved.excludeAgentIDs
		if len(resolved.userIDs) > 0 {
			filter.UserID = ""
			filter.UserIDs = resolved.userIDs
		}
		filter.ExcludeUserIDs = resolved.excludeUserIDs
		if len(resolved.provenances) > 0 {
			filter.Provenance = ""
			filter.Provenances = resolved.provenances
		}
		filter.ExcludeProvenances = resolved.excludeProvenances
		if len(resolved.excludeTexts) > 0 {
			filter.ExcludeQueries = make([]store.ProjectDecisionExcludeQuery, 0, len(resolved.excludeTexts))
			for _, ex := range resolved.excludeTexts {
				filter.ExcludeQueries = append(filter.ExcludeQueries, store.ProjectDecisionExcludeQuery{
					Text:     ex,
					ChatKeys: set.labelMatchKeys(listKeys, strings.ToLower(ex)),
				})
			}
		}
	}
	if pq.q != "" {
		filter.Query = pq.q
		filter.QueryChatKeys = set.labelMatchKeys(listKeys, strings.ToLower(pq.q))
	}
	if pq.profile {
		return s.finishProfileMadeDecisions(ctx, wcs, set, pq, targetProjectIDs, listKeys, filter, counts, agentsFacet, answerersFacet, conversationsFacet, projectsFacet, facetsTruncated)
	}
	rawEntries, nextCursor, err := s.store.ListDecisionAnswers(ctx, targetProjectIDs, listKeys, filter, pq.cursor, pq.limit)
	if err != nil {
		return nil, err
	}
	var projectID string
	if len(set.projectIDs) > 0 {
		projectID = set.projectIDs[0]
	}
	return ProjectMadeDecisionsResponse{
		Counts:          counts,
		Entries:         s.buildProjectDecisionLogViews(ctx, wcs, set, rawEntries),
		Agents:          agentsFacet,
		Answerers:       answerersFacet,
		Conversations:   conversationsFacet,
		FacetsTruncated: facetsTruncated,
		Retention:       s.projectDecisionRetentionInfo(projectID),
		NextCursor:      nextCursor,
	}, nil
}

// decisionAgentFacetViews labels the asking agents (design D5): the live
// agent's name, else the newest decision's name snapshot. Agents whose labels
// collide get a hint: "deleted · <id>" for a deleted agent, the short id for
// one of several live agents. Sorted by label, optionally capped.
func decisionAgentFacetViews(set *decisionScopeSet, agents []store.ProjectDecisionActor, capped bool) ([]ProjectDecisionActorView, bool) {
	type option struct {
		view    ProjectDecisionActorView
		deleted bool
	}
	opts := make([]option, 0, len(agents))
	for _, a := range agents {
		live := set.liveAgents[a.ID]
		label := a.Name
		if live != nil {
			label = agentDisplayName(live)
		}
		if label == "" {
			label = set.agentNames[a.ID]
		}
		if label == "" {
			label = a.ID
		}
		projectID := a.ProjectID
		if projectID == "" && live != nil {
			projectID = live.ProjectID
		}
		if projectID == "" {
			if ag := set.agentsByID[a.ID]; ag != nil {
				projectID = ag.ProjectID
			}
		}
		opts = append(opts, option{
			view:    ProjectDecisionActorView{ID: a.ID, Name: label, ProjectID: projectID},
			deleted: live == nil,
		})
	}
	groups := make(map[string][]int)
	for i, o := range opts {
		k := strings.ToLower(o.view.Name)
		groups[k] = append(groups[k], i)
	}
	for _, idx := range groups {
		if len(idx) < 2 {
			continue
		}
		liveCount := 0
		for _, i := range idx {
			if !opts[i].deleted {
				liveCount++
			}
		}
		for _, i := range idx {
			short := shortDecisionAgentID(opts[i].view.ID)
			switch {
			case opts[i].deleted:
				opts[i].view.Hint = "deleted · " + short
			case liveCount > 1:
				opts[i].view.Hint = short
			}
		}
	}
	// By label; a live agent before a deleted namesake; then by ID.
	sort.Slice(opts, func(i, j int) bool {
		ni, nj := strings.ToLower(opts[i].view.Name), strings.ToLower(opts[j].view.Name)
		if ni != nj {
			return ni < nj
		}
		if opts[i].deleted != opts[j].deleted {
			return !opts[i].deleted
		}
		return opts[i].view.ID < opts[j].view.ID
	})
	out := make([]ProjectDecisionActorView, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.view)
	}
	truncated := len(out) > maxProjectDecisionFacetOptions
	if capped && truncated {
		out = out[:maxProjectDecisionFacetOptions]
	}
	return out, truncated
}

// shortDecisionAgentID is the first four characters of an agent ID, the
// disambiguating suffix of the agents facet.
func shortDecisionAgentID(id string) string {
	if len(id) > 4 {
		return id[:4]
	}
	return id
}

// decisionConversationFacetViews builds the conversations facet from the
// readable keys that have decisions, sorted by label, optionally capped.
func decisionConversationFacetViews(set *decisionScopeSet, keys []string, capped bool) ([]ProjectDecisionConversationView, bool) {
	out := make([]ProjectDecisionConversationView, 0, len(keys))
	for _, k := range keys {
		if !set.readable[k] {
			continue
		}
		label := set.conversationLabel[k]
		if label == "" {
			label = k
		}
		out = append(out, ProjectDecisionConversationView{Key: k, Label: label, ProjectID: set.keyProject[k]})
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Label), strings.ToLower(out[j].Label)
		if li != lj {
			return li < lj
		}
		return out[i].Key < out[j].Key
	})
	truncated := len(out) > maxProjectDecisionFacetOptions
	if capped && truncated {
		out = out[:maxProjectDecisionFacetOptions]
	}
	return out, truncated
}

// filterClassifiedPrompts applies the pending filters in memory (design D6):
// the prompt's conversation is in keys, its asker is pq.agentID, and q
// matches the conversation label, the asker's name, or any row's question,
// key or answer label.
func filterClassifiedPrompts(
	set *decisionScopeSet,
	all []classifiedOpenPrompt,
	keys []string,
	pq projectDecisionsQuery,
	resolved resolvedDecisionsQuery,
) []classifiedOpenPrompt {
	inKeys := make(map[string]bool, len(keys))
	for _, k := range keys {
		inKeys[k] = true
	}
	var inAgents map[string]bool
	if len(resolved.agentIDs) > 0 {
		inAgents = make(map[string]bool, len(resolved.agentIDs))
		for _, id := range resolved.agentIDs {
			inAgents[id] = true
		}
	}
	var excAgents map[string]bool
	if len(resolved.excludeAgentIDs) > 0 {
		excAgents = make(map[string]bool, len(resolved.excludeAgentIDs))
		for _, id := range resolved.excludeAgentIDs {
			excAgents[id] = true
		}
	}
	qFold := strings.ToLower(pq.q)
	var excFolds []string
	if len(resolved.excludeTexts) > 0 {
		excFolds = make([]string, 0, len(resolved.excludeTexts))
		for _, ex := range resolved.excludeTexts {
			if ex != "" {
				excFolds = append(excFolds, strings.ToLower(ex))
			}
		}
	}
	out := make([]classifiedOpenPrompt, 0, len(all))
	for _, c := range all {
		p := c.raw
		if pq.projectID != "" && p.ProjectID != pq.projectID {
			continue
		}
		if !inKeys[p.ChatKey] {
			continue
		}
		if pq.agentID != "" && p.AskerAgentID != pq.agentID {
			continue
		}
		if inAgents != nil && !inAgents[p.AskerAgentID] {
			continue
		}
		if excAgents != nil && excAgents[p.AskerAgentID] {
			continue
		}
		if qFold != "" && !pendingPromptMatches(set, p, qFold) {
			continue
		}
		excluded := false
		for _, exFold := range excFolds {
			if pendingPromptMatches(set, p, exFold) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		out = append(out, c)
	}
	return out
}

func pendingPromptMatches(set *decisionScopeSet, p *store.ProjectDecisionPrompt, qFold string) bool {
	contains := func(s string) bool { return s != "" && strings.Contains(strings.ToLower(s), qFold) }
	if contains(set.conversationLabel[p.ChatKey]) || contains(p.AskerAgentName) || contains(set.agentNames[p.AskerAgentID]) {
		return true
	}
	for _, d := range p.Decisions {
		if d == nil {
			continue
		}
		if contains(d.Question) || contains(d.Key) || contains(d.AskerAgentName) {
			return true
		}
		if a := d.EffectiveAnswer(); a != nil && (contains(a.ChoiceLabel) || contains(a.OtherText)) {
			return true
		}
	}
	return false
}

func sumClassifiedCounts(all []classifiedOpenPrompt) ProjectDecisionCounts {
	var c ProjectDecisionCounts
	for _, p := range all {
		c.WaitingOnYou += p.onYou
		c.WaitingOnOthers += p.onOthers
	}
	return c
}

func countProjectsWithWaiting(classified []classifiedOpenPrompt) int {
	projectsWithWaiting := make(map[string]bool)
	for _, p := range classified {
		if p.onYou > 0 {
			projectsWithWaiting[p.raw.ProjectID] = true
		}
	}
	return len(projectsWithWaiting)
}

type classifiedOpenPrompt struct {
	raw            *store.ProjectDecisionPrompt
	onYou          int // rows waiting on the viewer
	onOthers       int // rows waiting on others
	section        int // 0 = waitingOnYou, 1 = waitingOnOthers only
	waitingOnYou   bool
	canAnswer      bool
	disabledReason string
	waitingReasons map[string]string
	promptAvail    bool
}

func isMessageSoftDeleted(ext *WebChatMessageExt) bool {
	return ext != nil && ext.DeletedAt != nil
}

// classifyProjectOpenPrompts evaluates each non-settled decision row across
// openPrompts for viewer (user), computing counts.waitingOnYou and
// counts.waitingOnOthers and classifying each prompt card.
func (s *Server) classifyProjectOpenPrompts(
	ctx context.Context,
	user UserIdentity,
	token *ScopedUserIdentity,
	wcs WebChatStore,
	scope *projectReadableScope,
	openPrompts []*store.ProjectDecisionPrompt,
) (ProjectDecisionCounts, []classifiedOpenPrompt, error) {
	if len(openPrompts) == 0 {
		return ProjectDecisionCounts{}, nil, nil
	}

	// Batch check soft-deleted prompt messages in WebChatStore.
	var extByMsgID map[string]*WebChatMessageExt
	if wcs != nil {
		msgIDs := make([]string, 0, len(openPrompts))
		for _, p := range openPrompts {
			if p.PromptExistsInMessages {
				msgIDs = append(msgIDs, p.MessageID)
			}
		}
		if len(msgIDs) > 0 {
			exts, err := wcs.GetMessageExts(ctx, msgIDs)
			if err == nil {
				extByMsgID = exts
			}
		}
	}

	viewerID := user.ID()
	postUser := chatPostIdentity(user)

	// Batch-prefetch any asking agent IDs that were inserted concurrently after
	// resolveProjectReadableScope ran so checkAsker never issues per-row queries.
	var missingAskerIDs []string
	seenMissingAsker := make(map[string]bool)
	for _, p := range openPrompts {
		if p.AskerAgentID != "" && !scope.queriedAgentIDs[p.AskerAgentID] && !seenMissingAsker[p.AskerAgentID] {
			seenMissingAsker[p.AskerAgentID] = true
			missingAskerIDs = append(missingAskerIDs, p.AskerAgentID)
		}
	}
	if len(missingAskerIDs) > 0 {
		extraAgents, err := s.store.GetAgentsByIDsIncludingDeleted(ctx, missingAskerIDs)
		if err != nil {
			return ProjectDecisionCounts{}, nil, err
		}
		for _, id := range missingAskerIDs {
			scope.queriedAgentIDs[id] = true
			if ag := extraAgents[id]; ag != nil {
				scope.agentsByID[id] = ag
				if name := agentDisplayName(ag); name != "" {
					scope.agentNames[id] = name
				}
				if ag.DeletedAt.IsZero() {
					scope.liveAgents[id] = ag
				}
			}
		}
	}

	// Cache per-asker authorizeAgentMessage(postUser, asker) using the
	// batch-loaded scope.liveAgents map (absent or soft-deleted = gone).
	type askerCheck struct {
		gone    bool
		allowed bool
	}
	askerCache := make(map[string]askerCheck)
	checkAsker := func(agentID string) askerCheck {
		if c, ok := askerCache[agentID]; ok {
			return c
		}
		asker := scope.liveAgents[agentID]
		if asker == nil {
			c := askerCheck{gone: true, allowed: true}
			askerCache[agentID] = c
			return c
		}
		allowed, _, _ := s.authorizeAgentMessage(ctx, postUser, asker, false)
		c := askerCheck{gone: false, allowed: allowed}
		askerCache[agentID] = c
		return c
	}

	var counts ProjectDecisionCounts
	out := make([]classifiedOpenPrompt, 0, len(openPrompts))

	for _, p := range openPrompts {
		canPostConv := scope.userCanPost[p.ChatKey]
		ac := checkAsker(p.AskerAgentID)

		promptWaitingOnYou := false
		onYou, onOthers := 0, 0
		var rowWaitingReasons map[string]string
		var firstBlockReason string

		for _, d := range p.Decisions {
			reason := store.DecisionActionableReason(d, viewerID)
			switch reason {
			case store.AnswerSetReasonReplaced, store.AnswerSetReasonWithdrawn, store.AnswerSetReasonDismissed, store.AnswerSetReasonDecided:
				// Already settled sibling row in a multi-question set prompt.
				continue
			case store.AnswerSetReasonNotRecordedAnswerer:
				onOthers++
				if rowWaitingReasons == nil {
					rowWaitingReasons = make(map[string]string)
				}
				rowWaitingReasons[d.ID] = WaitingReasonOtherUserConfirm
				if firstBlockReason == "" {
					firstBlockReason = "Waiting for the recorded answerer to confirm"
				}
			case "":
				// Actionable by state: check whether the user's account can
				// submit an answer in this conversation and to this asker.
				// Note: confirming an interpreted answer does not deliver to
				// the asker (decisions_answer.go:298), whereas answering a
				// pending row delivers when the asker is still live.
				needsAskerDelivery := !ac.gone && d.EffectiveAnswer() == nil
				switch {
				case !canPostConv:
					onOthers++
					if rowWaitingReasons == nil {
						rowWaitingReasons = make(map[string]string)
					}
					rowWaitingReasons[d.ID] = WaitingReasonReadOnly
					if firstBlockReason == "" {
						firstBlockReason = "Read-only in this conversation"
					}
				case needsAskerDelivery && !ac.allowed:
					onOthers++
					if rowWaitingReasons == nil {
						rowWaitingReasons = make(map[string]string)
					}
					rowWaitingReasons[d.ID] = WaitingReasonMessageDenied
					if firstBlockReason == "" {
						firstBlockReason = "Asking agent is not accepting messages from you"
					}
				default:
					onYou++
					promptWaitingOnYou = true
				}
			}
		}

		section := 1
		if promptWaitingOnYou {
			section = 0
		}
		canAnswer := promptWaitingOnYou && token == nil
		disabledReason := ""
		if !canAnswer {
			if token != nil && promptWaitingOnYou {
				disabledReason = "Decisions are answered from a web session"
			} else {
				disabledReason = firstBlockReason
			}
		}

		promptAvail := p.PromptExistsInMessages && !isMessageSoftDeleted(extByMsgID[p.MessageID])

		counts.WaitingOnYou += onYou
		counts.WaitingOnOthers += onOthers
		out = append(out, classifiedOpenPrompt{
			raw:            p,
			onYou:          onYou,
			onOthers:       onOthers,
			section:        section,
			waitingOnYou:   promptWaitingOnYou,
			canAnswer:      canAnswer,
			disabledReason: disabledReason,
			waitingReasons: rowWaitingReasons,
			promptAvail:    promptAvail,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].section != out[j].section {
			return out[i].section < out[j].section
		}
		if !out[i].raw.CreatedAt.Equal(out[j].raw.CreatedAt) {
			return out[i].raw.CreatedAt.After(out[j].raw.CreatedAt)
		}
		return out[i].raw.MessageID > out[j].raw.MessageID
	})

	return counts, out, nil
}

func paginateClassifiedPrompts(
	all []classifiedOpenPrompt,
	cur *pendingPromptCursor,
	limit int,
) ([]classifiedOpenPrompt, string, error) {
	if limit == 0 || len(all) == 0 {
		return nil, "", nil
	}
	start := 0
	if cur != nil {
		start = len(all)
		for i, item := range all {
			after := false
			switch {
			case item.section > cur.Section:
				after = true
			case item.section == cur.Section && item.raw.CreatedAt.Before(cur.CreatedAt):
				after = true
			case item.section == cur.Section && item.raw.CreatedAt.Equal(cur.CreatedAt) && item.raw.MessageID < cur.MessageID:
				after = true
			}
			if after {
				start = i
				break
			}
		}
	}
	if start >= len(all) {
		return nil, "", nil
	}
	rem := all[start:]
	if len(rem) <= limit {
		return rem, "", nil
	}
	page := rem[:limit]
	last := page[len(page)-1]
	next, err := encodePendingPromptCursor(last.section, last.raw.CreatedAt, last.raw.MessageID)
	if err != nil {
		return nil, "", err
	}
	return page, next, nil
}

func (s *Server) buildProjectDecisionPromptViews(
	ctx context.Context,
	scope *decisionScopeSet,
	page []classifiedOpenPrompt,
) []ProjectDecisionPromptView {
	if len(page) == 0 {
		return []ProjectDecisionPromptView{}
	}
	var replacedByIDs []string
	for _, item := range page {
		for _, d := range item.raw.Decisions {
			if d.ReplacedByID != "" {
				replacedByIDs = append(replacedByIDs, d.ReplacedByID)
			}
		}
	}
	replacedBy := s.decisionRows(ctx, replacedByIDs)

	out := make([]ProjectDecisionPromptView, 0, len(page))
	for _, item := range page {
		p := item.raw
		decViews := make([]DecisionView, 0, len(p.Decisions))
		for _, d := range p.Decisions {
			decViews = append(decViews, decisionView(d, replacedBy[d.ReplacedByID], s.decisionDismissMinAge()))
		}
		askerName := p.AskerAgentName
		if askerName == "" {
			askerName = scope.agentNames[p.AskerAgentID]
		}
		out = append(out, ProjectDecisionPromptView{
			Project:               scope.projectRef(p.ProjectID),
			PromptMessageID:       p.MessageID,
			PromptAvailable:       item.promptAvail,
			ChatKey:               p.ChatKey,
			ConversationLabel:     scope.conversationLabel[p.ChatKey],
			ConversationAvailable: scope.conversationAvailable[p.ChatKey],
			AskerAgentID:          p.AskerAgentID,
			AskerAgentName:        askerName,
			CreatedAt:             p.CreatedAt,
			AnswerMode:            decisionAnswerMode(len(p.Decisions)),
			WaitingOnYou:          item.waitingOnYou,
			CanAnswer:             item.canAnswer,
			DisabledReason:        item.disabledReason,
			WaitingReasons:        item.waitingReasons,
			Decisions:             decViews,
		})
	}
	return out
}

func (s *Server) buildProjectDecisionLogViews(
	ctx context.Context,
	wcs WebChatStore,
	scope *decisionScopeSet,
	entries []*store.ProjectDecisionAnswerEntry,
) []ProjectDecisionLogEntryView {
	if len(entries) == 0 {
		return []ProjectDecisionLogEntryView{}
	}

	// Batch check soft-deleted prompt and source reply messages in WebChatStore.
	var extByMsgID map[string]*WebChatMessageExt
	if wcs != nil {
		var msgIDs []string
		seen := make(map[string]bool)
		for _, e := range entries {
			if e.PromptExistsInMessages && e.Decision != nil && e.Decision.MessageID != "" && !seen[e.Decision.MessageID] {
				seen[e.Decision.MessageID] = true
				msgIDs = append(msgIDs, e.Decision.MessageID)
			}
			if e.SourceExistsInMessages && e.Answer != nil && e.Answer.SourceMessageID != "" && !seen[e.Answer.SourceMessageID] {
				seen[e.Answer.SourceMessageID] = true
				msgIDs = append(msgIDs, e.Answer.SourceMessageID)
			}
		}
		if len(msgIDs) > 0 {
			exts, err := wcs.GetMessageExts(ctx, msgIDs)
			if err == nil {
				extByMsgID = exts
			}
		}
	}

	// Batch-load any interpreter agents that never asked a decision themselves
	// (so they were not in ListProjectDecisionAgents) in one query across the page.
	var missingInterpreterIDs []string
	seenMissingInterp := make(map[string]bool)
	for _, e := range entries {
		if e != nil && e.Answer != nil && e.Answer.InterpretedByAgentID != "" {
			id := e.Answer.InterpretedByAgentID
			if !scope.queriedAgentIDs[id] && !seenMissingInterp[id] {
				seenMissingInterp[id] = true
				missingInterpreterIDs = append(missingInterpreterIDs, id)
			}
		}
	}
	if len(missingInterpreterIDs) > 0 {
		if extraAgents, err := s.store.GetAgentsByIDsIncludingDeleted(ctx, missingInterpreterIDs); err == nil {
			for _, id := range missingInterpreterIDs {
				scope.queriedAgentIDs[id] = true
				if ag := extraAgents[id]; ag != nil {
					scope.agentsByID[id] = ag
					if name := agentDisplayName(ag); name != "" {
						scope.agentNames[id] = name
					}
				}
			}
		}
	}

	resolveAgentName := func(agentID, fallback string) string {
		if agentID == "" {
			return ""
		}
		if name := scope.agentNames[agentID]; name != "" {
			return name
		}
		if ag := scope.agentsByID[agentID]; ag != nil {
			if name := agentDisplayName(ag); name != "" {
				return name
			}
		}
		return fallback
	}

	out := make([]ProjectDecisionLogEntryView, 0, len(entries))
	for _, e := range entries {
		if e == nil || e.Answer == nil || e.Decision == nil {
			continue
		}
		a := e.Answer
		d := e.Decision

		promptAvail := e.PromptExistsInMessages && !isMessageSoftDeleted(extByMsgID[d.MessageID])
		sourceAvail := a.SourceMessageID != "" && e.SourceExistsInMessages && !isMessageSoftDeleted(extByMsgID[a.SourceMessageID])

		askerName := d.AskerAgentName
		if askerName == "" {
			askerName = scope.agentNames[d.AskerAgentID]
		}

		interpretedByName := ""
		if a.InterpretedByAgentID != "" {
			fallback := ""
			if a.InterpretedByAgentID == d.AskerAgentID {
				fallback = askerName
			}
			interpretedByName = resolveAgentName(a.InterpretedByAgentID, fallback)
		}

		choices := d.Choices
		if choices == nil {
			choices = []messages.Choice{}
		}
		var replacedBy *DecisionRef
		if d.ReplacedByID != "" {
			// The replacing version is always active.Version+1 (see loadKeyStates in decision_store.go).
			replacedBy = &DecisionRef{
				RowID:   d.ReplacedByID,
				Version: d.Version + 1,
			}
		}

		out = append(out, ProjectDecisionLogEntryView{
			Project:  scope.projectRef(d.ProjectID),
			AnswerID: a.ID,
			Decision: ProjectDecisionSummaryView{
				RowID:           d.ID,
				PromptMessageID: d.MessageID,
				PromptAvailable: promptAvail,
				Key:             d.Key,
				Ref:             d.Ref(),
				Version:         d.Version,
				Status:          d.Status,
				Question:        d.Question,
				Choices:         choices,
				DefaultChoiceID: d.DefaultChoiceID,
				Other:           cloneOtherOption(d.Other),
				ReplacedBy:      replacedBy,
				AskerAgentID:    d.AskerAgentID,
				AskerAgentName:  askerName,
			},
			ChoiceID:               a.ChoiceID,
			Label:                  a.ChoiceLabel,
			OtherText:              a.OtherText,
			MatchedDefault:         a.MatchedDefault,
			PreviousChoiceLabel:    e.PreviousChoiceLabel,
			PreviousOtherText:      e.PreviousOtherText,
			InterpretedAnswer:      answerView(e.InterpretedAnswer),
			AwaitingConfirmation:   e.AwaitingConfirmation,
			AnsweredByUserID:       a.AnsweredByUserID,
			AnsweredByName:         answererDisplayName(a),
			Provenance:             a.Provenance,
			Outcome:                a.Outcome,
			DismissReason:          a.DismissReason,
			Surface:                a.Surface,
			InterpretedByAgentID:   a.InterpretedByAgentID,
			InterpretedByAgentName: interpretedByName,
			SourceMessageID:        a.SourceMessageID,
			SourceMessageAvailable: sourceAvail,
			ResponseMessageID:      a.ResponseMessageID,
			BatchID:                a.BatchID,
			At:                     a.CreatedAt,
			ChatKey:                d.ChatKey,
			ConversationLabel:      scope.conversationLabel[d.ChatKey],
			ConversationAvailable:  scope.conversationAvailable[d.ChatKey],
		})
	}
	return out
}
