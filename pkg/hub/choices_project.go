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
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Pagination bounds and tab names for GET /api/v1/projects/{id}/decisions.
const (
	defaultProjectDecisionsLimit = 50
	maxProjectDecisionsLimit     = 200

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
// recorded answerer) scoped to conversations the caller can read.
type ProjectDecisionActorView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectDecisionPromptView is one prompt card on tab=pending.
type ProjectDecisionPromptView struct {
	PromptMessageID       string            `json:"promptMessageId"`
	PromptAvailable       bool              `json:"promptAvailable"`
	ChatKey               string            `json:"chatKey"`
	ConversationLabel     string            `json:"conversationLabel"`
	ConversationAvailable bool              `json:"conversationAvailable"`
	AskerAgentID          string            `json:"askerAgentId"`
	AskerAgentName        string            `json:"askerAgentName"`
	CreatedAt             time.Time         `json:"createdAt"`
	AnswerMode            string            `json:"answerMode"`
	WaitingOnYou          bool              `json:"waitingOnYou"`
	CanAnswer             bool              `json:"canAnswer"`
	DisabledReason        string            `json:"disabledReason,omitempty"`
	WaitingReasons        map[string]string `json:"waitingReasons,omitempty"`
	Decisions             []DecisionView    `json:"decisions"`
}

// ProjectDecisionSummaryView is the decision metadata embedded in one entry of
// the tab=made decision log.
type ProjectDecisionSummaryView struct {
	RowID           string `json:"rowId"`
	PromptMessageID string `json:"promptMessageId"`
	PromptAvailable bool   `json:"promptAvailable"`
	Key             string `json:"key"`
	Ref             string `json:"ref"`
	Version         int    `json:"version"`
	Question        string `json:"question"`
	AskerAgentID    string `json:"askerAgentId"`
	AskerAgentName  string `json:"askerAgentName"`
}

// ProjectDecisionLogEntryView is one append-only answer log line on tab=made.
type ProjectDecisionLogEntryView struct {
	AnswerID               string                     `json:"answerId"`
	Decision               ProjectDecisionSummaryView `json:"decision"`
	ChoiceID               string                     `json:"choiceId"`
	Label                  string                     `json:"label"`
	MatchedDefault         bool                       `json:"matchedDefault"`
	PreviousChoiceLabel    string                     `json:"previousChoiceLabel,omitempty"`
	AnsweredByUserID       string                     `json:"answeredByUserId,omitempty"`
	AnsweredByName         string                     `json:"answeredByName"`
	Provenance             string                     `json:"provenance"`
	Outcome                string                     `json:"outcome"`
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

// ProjectPendingDecisionsResponse is the response of GET /api/v1/projects/{id}/decisions?tab=pending.
type ProjectPendingDecisionsResponse struct {
	Counts     ProjectDecisionCounts       `json:"counts"`
	Prompts    []ProjectDecisionPromptView `json:"prompts"`
	Agents     []ProjectDecisionActorView  `json:"agents"`
	Answerers  []ProjectDecisionActorView  `json:"answerers"`
	NextCursor string                      `json:"nextCursor,omitempty"`
}

// ProjectMadeDecisionsResponse is the response of GET /api/v1/projects/{id}/decisions?tab=made.
type ProjectMadeDecisionsResponse struct {
	Counts     ProjectDecisionCounts         `json:"counts"`
	Entries    []ProjectDecisionLogEntryView `json:"entries"`
	Agents     []ProjectDecisionActorView    `json:"agents"`
	Answerers  []ProjectDecisionActorView    `json:"answerers"`
	NextCursor string                        `json:"nextCursor,omitempty"`
}

// pendingPromptCursor is the keyset cursor for tab=pending: section 0 is
// "Waiting on you" prompts and section 1 is "Waiting on others"-only prompts.
type pendingPromptCursor struct {
	Section   int       `json:"s"`
	CreatedAt time.Time `json:"t"`
	MessageID string    `json:"m"`
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
	if (c.Section != 0 && c.Section != 1) || c.MessageID == "" || c.CreatedAt.IsZero() {
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
		out = append(out, decisionView(d, replacedBy[d.ReplacedByID]))
	}
	return out
}

// handleProjectDecisions serves GET /api/v1/projects/{id}/decisions.
// Unsupported methods return 405 before the dual experiment gate, which
// returns 404 unless both web.project_decisions and web.chat_choices are on.
func (s *Server) handleProjectDecisions(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.requireExperiment(experiments.ProjectDecisions,
		s.requireExperiment(experiments.ChatChoices, func(w http.ResponseWriter, r *http.Request) {
			s.listProjectDecisions(w, r, projectID)
		}),
	)(w, r)
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

type projectDecisionsQuery struct {
	tab           string
	limit         int
	cursor        string
	pendingCursor *pendingPromptCursor
	madeFilter    store.ProjectDecisionAnswerFilter
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

	if out.tab == ProjectDecisionsTabMade {
		agentParam := strings.TrimSpace(q.Get("agent"))
		if agentParam != "" {
			if _, err := uuid.Parse(agentParam); err != nil {
				ValidationError(w, "agent must be a valid UUID", map[string]interface{}{"field": "agent"})
				return projectDecisionsQuery{}, false
			}
			out.madeFilter.AgentID = agentParam
		}
		out.madeFilter.UserID = strings.TrimSpace(q.Get("user"))

		prov := strings.TrimSpace(q.Get("provenance"))
		switch prov {
		case "":
		case store.ProjectDecisionProvenanceDirect,
			store.ProjectDecisionProvenanceInterpreted,
			store.ProjectDecisionProvenanceConfirmed,
			store.ProjectDecisionProvenanceOverridden:
			out.madeFilter.Provenance = prov
		case store.DecisionProvenanceAgentInterpreted:
			out.madeFilter.Provenance = store.ProjectDecisionProvenanceInterpreted
		default:
			ValidationError(w, "provenance must be one of direct, interpreted, confirmed, overridden",
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
	chatKeys              []string
	conversationLabel     map[string]string
	conversationAvailable map[string]bool
	userCanPost           map[string]bool
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
	scope := &projectReadableScope{
		conversationLabel:     make(map[string]string),
		conversationAvailable: make(map[string]bool),
		userCanPost:           make(map[string]bool),
		liveAgents:            make(map[string]*store.Agent),
		agentNames:            make(map[string]string),
	}
	seenKeys := make(map[string]bool)
	projectChatAccess := s.chatUserAccess(ctx, user, project,
		[]string{ChatActionRead}, chatThreadWriteActions, nil, false, false)

	// 1. Readable topics in the project (including soft-deleted topics so
	// audit history is never erased; soft-deleted topics report
	// conversationAvailable=false and userCanPost=false).
	if wcs != nil && projectChatAccess.Can(ChatActionRead) {
		topics, err := wcs.ListTopicsIncludingDeleted(ctx, project.ID)
		if err != nil {
			return nil, fmt.Errorf("list topics: %w", err)
		}
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
	// in this project (SQL GROUP BY asker_agent_id). An agent with zero
	// decisions in this project can have no DM decision rows, so candidate DM
	// keys are derived solely from this distinct set.
	decAgents, err := s.store.ListProjectDecisionAgents(ctx, project.ID)
	if err != nil {
		return nil, fmt.Errorf("list project decision agents: %w", err)
	}
	candidateAgentIDs := make([]string, 0, len(decAgents))
	for _, da := range decAgents {
		if da.Name != "" {
			scope.agentNames[da.ID] = da.Name
		}
		ag, gerr := s.store.GetAgent(ctx, da.ID)
		if gerr != nil && !errors.Is(gerr, store.ErrNotFound) {
			return nil, fmt.Errorf("get decision agent %s: %w", da.ID, gerr)
		}
		if ag != nil {
			if name := agentDisplayName(ag); name != "" {
				scope.agentNames[da.ID] = name
			}
			if ag.DeletedAt.IsZero() {
				scope.liveAgents[da.ID] = ag
			}
		}
		candidateAgentIDs = append(candidateAgentIDs, da.ID)
	}

	// Build viewer's canonical DM keys dm:agent:<agentUUID>:user:<viewerID>.
	viewerID := user.ID()
	var dmTokenCheck *selfScopeCheck
	if token != nil {
		dmTokenCheck = s.newSelfScopeCheck(ctx, token, permInboxRead)
	}
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

	return scope, nil
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

	scope, err := s.resolveProjectReadableScope(ctx, project, user, token, wcs)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if len(scope.chatKeys) == 0 {
		if pq.tab == ProjectDecisionsTabPending {
			writeJSON(w, http.StatusOK, ProjectPendingDecisionsResponse{
				Prompts:   []ProjectDecisionPromptView{},
				Agents:    []ProjectDecisionActorView{},
				Answerers: []ProjectDecisionActorView{},
			})
		} else {
			writeJSON(w, http.StatusOK, ProjectMadeDecisionsResponse{
				Entries:   []ProjectDecisionLogEntryView{},
				Agents:    []ProjectDecisionActorView{},
				Answerers: []ProjectDecisionActorView{},
			})
		}
		return
	}

	agentsFacet := []ProjectDecisionActorView{}
	answerersFacet := []ProjectDecisionActorView{}
	if pq.limit > 0 {
		// Load facets (agents + answerers) scoped strictly to readable chat keys.
		facetAgents, facetAnswerers, err := s.store.ListProjectDecisionFacets(ctx, project.ID, scope.chatKeys)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		agentsFacet = make([]ProjectDecisionActorView, 0, len(facetAgents))
		for _, a := range facetAgents {
			name := scope.agentNames[a.ID]
			if name == "" {
				name = a.Name
			}
			agentsFacet = append(agentsFacet, ProjectDecisionActorView{ID: a.ID, Name: name})
		}
		sort.Slice(agentsFacet, func(i, j int) bool {
			ni, nj := strings.ToLower(agentsFacet[i].Name), strings.ToLower(agentsFacet[j].Name)
			if ni != nj {
				return ni < nj
			}
			return agentsFacet[i].ID < agentsFacet[j].ID
		})
		answerersFacet = make([]ProjectDecisionActorView, 0, len(facetAnswerers))
		for _, a := range facetAnswerers {
			answerersFacet = append(answerersFacet, ProjectDecisionActorView{ID: a.ID, Name: a.Name})
		}
	}

	// Compute live counts and classified prompt cards across open prompts.
	openPrompts, err := s.store.ListProjectOpenPrompts(ctx, project.ID, scope.chatKeys)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	counts, classifiedPrompts, err := s.classifyProjectOpenPrompts(ctx, user, token, wcs, scope, openPrompts)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if pq.tab == ProjectDecisionsTabPending {
		pagePrompts, nextCursor, err := paginateClassifiedPrompts(classifiedPrompts, pq.pendingCursor, pq.limit)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		promptsView := s.buildProjectDecisionPromptViews(ctx, scope, pagePrompts)
		writeJSON(w, http.StatusOK, ProjectPendingDecisionsResponse{
			Counts:     counts,
			Prompts:    promptsView,
			Agents:     agentsFacet,
			Answerers:  answerersFacet,
			NextCursor: nextCursor,
		})
		return
	}

	// tab=made
	rawEntries, nextCursor, err := s.store.ListProjectDecisionAnswers(ctx, project.ID, scope.chatKeys, pq.madeFilter, pq.cursor, pq.limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, "invalid query filter or cursor", map[string]interface{}{"field": "cursor"})
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	entriesView := s.buildProjectDecisionLogViews(ctx, wcs, scope, rawEntries)
	writeJSON(w, http.StatusOK, ProjectMadeDecisionsResponse{
		Counts:     counts,
		Entries:    entriesView,
		Agents:     agentsFacet,
		Answerers:  answerersFacet,
		NextCursor: nextCursor,
	})
}

type classifiedOpenPrompt struct {
	raw            *store.ProjectDecisionPrompt
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

	// Cache per-asker live lookup and authorizeAgentMessage(postUser, asker).
	type askerCheck struct {
		gone    bool
		allowed bool
	}
	askerCache := make(map[string]askerCheck)
	checkAsker := func(agentID string) (askerCheck, error) {
		if c, ok := askerCache[agentID]; ok {
			return c, nil
		}
		asker, ok := scope.liveAgents[agentID]
		if !ok {
			ag, err := s.store.GetAgent(ctx, agentID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				c := askerCheck{gone: true, allowed: true}
				askerCache[agentID] = c
				return c, nil
			case err != nil:
				return askerCheck{}, err
			case ag == nil || !ag.DeletedAt.IsZero():
				c := askerCheck{gone: true, allowed: true}
				askerCache[agentID] = c
				return c, nil
			default:
				asker = ag
			}
		}
		allowed, _, _ := s.authorizeAgentMessage(ctx, postUser, asker, false)
		c := askerCheck{gone: false, allowed: allowed}
		askerCache[agentID] = c
		return c, nil
	}

	var counts ProjectDecisionCounts
	out := make([]classifiedOpenPrompt, 0, len(openPrompts))

	for _, p := range openPrompts {
		canPostConv := scope.userCanPost[p.ChatKey]
		ac, err := checkAsker(p.AskerAgentID)
		if err != nil {
			return ProjectDecisionCounts{}, nil, err
		}

		promptWaitingOnYou := false
		var rowWaitingReasons map[string]string
		var firstBlockReason string

		for _, d := range p.Decisions {
			reason := store.DecisionActionableReason(d, viewerID)
			switch reason {
			case store.AnswerSetReasonReplaced, store.AnswerSetReasonDecided:
				// Already settled sibling row in a multi-question set prompt.
				continue
			case store.AnswerSetReasonNotRecordedAnswerer:
				counts.WaitingOnOthers++
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
				// the asker (choices_answer.go:298), whereas answering a
				// pending row delivers when the asker is still live.
				needsAskerDelivery := !ac.gone && d.EffectiveAnswer() == nil
				switch {
				case !canPostConv:
					counts.WaitingOnOthers++
					if rowWaitingReasons == nil {
						rowWaitingReasons = make(map[string]string)
					}
					rowWaitingReasons[d.ID] = WaitingReasonReadOnly
					if firstBlockReason == "" {
						firstBlockReason = "Read-only in this conversation"
					}
				case needsAskerDelivery && !ac.allowed:
					counts.WaitingOnOthers++
					if rowWaitingReasons == nil {
						rowWaitingReasons = make(map[string]string)
					}
					rowWaitingReasons[d.ID] = WaitingReasonMessageDenied
					if firstBlockReason == "" {
						firstBlockReason = "Asking agent is not accepting messages from you"
					}
				default:
					counts.WaitingOnYou++
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

		out = append(out, classifiedOpenPrompt{
			raw:            p,
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
	scope *projectReadableScope,
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
			decViews = append(decViews, decisionView(d, replacedBy[d.ReplacedByID]))
		}
		askerName := p.AskerAgentName
		if askerName == "" {
			askerName = scope.agentNames[p.AskerAgentID]
		}
		out = append(out, ProjectDecisionPromptView{
			PromptMessageID:       p.MessageID,
			PromptAvailable:       item.promptAvail,
			ChatKey:               p.ChatKey,
			ConversationLabel:     scope.conversationLabel[p.ChatKey],
			ConversationAvailable: scope.conversationAvailable[p.ChatKey],
			AskerAgentID:          p.AskerAgentID,
			AskerAgentName:        askerName,
			CreatedAt:             p.CreatedAt,
			AnswerMode:            choiceAnswerMode(len(p.Decisions)),
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
	scope *projectReadableScope,
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

	interpreterNameCache := make(map[string]string)
	resolveAgentName := func(agentID, fallback string) string {
		if agentID == "" {
			return ""
		}
		if name := scope.agentNames[agentID]; name != "" {
			return name
		}
		if name, ok := interpreterNameCache[agentID]; ok {
			return name
		}
		if ag, err := s.store.GetAgent(ctx, agentID); err == nil && ag != nil {
			name := agentDisplayName(ag)
			interpreterNameCache[agentID] = name
			if name != "" {
				return name
			}
		} else {
			interpreterNameCache[agentID] = fallback
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

		out = append(out, ProjectDecisionLogEntryView{
			AnswerID: a.ID,
			Decision: ProjectDecisionSummaryView{
				RowID:           d.ID,
				PromptMessageID: d.MessageID,
				PromptAvailable: promptAvail,
				Key:             d.Key,
				Ref:             d.Ref(),
				Version:         d.Version,
				Question:        d.Question,
				AskerAgentID:    d.AskerAgentID,
				AskerAgentName:  askerName,
			},
			ChoiceID:               a.ChoiceID,
			Label:                  a.ChoiceLabel,
			MatchedDefault:         a.MatchedDefault,
			PreviousChoiceLabel:    e.PreviousChoiceLabel,
			AnsweredByUserID:       a.AnsweredByUserID,
			AnsweredByName:         answererDisplayName(a),
			Provenance:             a.Provenance,
			Outcome:                a.Outcome,
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
