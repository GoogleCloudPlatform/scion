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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	DecisionsSortRecent  = "recent"
	DecisionsSortProject = "project"

	maxProfileDecisionProjects          = 200
	maxProfileCursorDoneProjects        = maxProfileDecisionProjects * 2
	maxProfileCursorBytes               = 32768
	profileMadeOverviewPerProject       = 5
	maxProfileDecisionProjectParamBytes = 128
	profileProjectSummariesBatchSize    = 500
)

// ProfileDecisionMadeGroupView is one project's overview group on
// GET /api/v1/users/me/decisions?tab=made&sort=project (design R5, 5.1).
type ProfileDecisionMadeGroupView struct {
	Project ProjectDecisionProjectRef     `json:"project"`
	Entries []ProjectDecisionLogEntryView `json:"entries"`
	More    bool                          `json:"more"`
}

// ProfilePendingDecisionsResponse is the response of
// GET /api/v1/users/me/decisions?tab=pending.
type ProfilePendingDecisionsResponse struct {
	Counts            ProjectDecisionCounts             `json:"counts"`
	FilteredCounts    *ProjectDecisionCounts            `json:"filteredCounts,omitempty"`
	Prompts           []ProjectDecisionPromptView       `json:"prompts"`
	Agents            []ProjectDecisionActorView        `json:"agents"`
	Answerers         []ProjectDecisionActorView        `json:"answerers"`
	Conversations     []ProjectDecisionConversationView `json:"conversations"`
	Projects          []ProjectDecisionProjectRef       `json:"projects"`
	TruncatedProjects bool                              `json:"truncatedProjects"`
	NextCursor        string                            `json:"nextCursor,omitempty"`
}

// ProfileMadeDecisionsResponse is the response of
// GET /api/v1/users/me/decisions?tab=made&sort=recent (the default Made sort).
type ProfileMadeDecisionsResponse struct {
	Counts            ProjectDecisionCounts             `json:"counts"`
	Entries           []ProjectDecisionLogEntryView     `json:"entries"`
	Agents            []ProjectDecisionActorView        `json:"agents"`
	Answerers         []ProjectDecisionActorView        `json:"answerers"`
	Conversations     []ProjectDecisionConversationView `json:"conversations"`
	Projects          []ProjectDecisionProjectRef       `json:"projects"`
	TruncatedProjects bool                              `json:"truncatedProjects"`
	NextCursor        string                            `json:"nextCursor,omitempty"`
}

// ProfileMadeByProjectDecisionsResponse is the response of
// GET /api/v1/users/me/decisions?tab=made&sort=project (overview mode, no cursor).
type ProfileMadeByProjectDecisionsResponse struct {
	Counts            ProjectDecisionCounts             `json:"counts"`
	Groups            []ProfileDecisionMadeGroupView    `json:"groups"`
	Agents            []ProjectDecisionActorView        `json:"agents"`
	Answerers         []ProjectDecisionActorView        `json:"answerers"`
	Conversations     []ProjectDecisionConversationView `json:"conversations"`
	Projects          []ProjectDecisionProjectRef       `json:"projects"`
	TruncatedProjects bool                              `json:"truncatedProjects"`
}

// profileDecisionCursor binds a profile decisions cursor to the caller, tab,
// sort and active filter set so cursors cannot cross tabs, sorts, filters or
// endpoints (design R4, R5, 5.1).
type profileDecisionCursor struct {
	Tab     string               `json:"tab"`
	Sort    string               `json:"sort"`
	Bind    string               `json:"bind"`
	Pending *pendingPromptCursor `json:"pending,omitempty"`
	Made    string               `json:"made,omitempty"`
}

func (q projectDecisionsQuery) profileCursorBind() string {
	var fromStr, toStr string
	if q.madeFilter.From != nil {
		fromStr = q.madeFilter.From.UTC().Format(time.RFC3339Nano)
	}
	if q.madeFilter.To != nil {
		toStr = q.madeFilter.To.UTC().Format(time.RFC3339Nano)
	}
	h := sha256.New()
	for _, part := range []string{
		q.viewerID,
		q.tab,
		q.sort,
		q.q,
		q.conversation,
		q.agentID,
		q.projectID,
		q.madeFilter.UserID,
		q.madeFilter.Provenance,
		fromStr,
		toStr,
	} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func encodeProfileDecisionCursor(pq projectDecisionsQuery, pending *pendingPromptCursor, made string) (string, error) {
	env := profileDecisionCursor{
		Tab:     pq.tab,
		Sort:    pq.sort,
		Bind:    pq.profileCursorBind(),
		Pending: pending,
		Made:    made,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeProfileDecisionCursor(token string, pq projectDecisionsQuery) (profileDecisionCursor, error) {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) > maxProfileCursorBytes {
		return profileDecisionCursor{}, errors.New("malformed cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		return profileDecisionCursor{}, errors.New("malformed cursor")
	}
	var env profileDecisionCursor
	if err := json.Unmarshal(raw, &env); err != nil {
		return profileDecisionCursor{}, errors.New("malformed cursor")
	}
	if env.Tab != pq.tab || env.Sort != pq.sort || env.Bind != pq.profileCursorBind() {
		return profileDecisionCursor{}, errors.New("cursor does not match query")
	}
	if pq.tab == ProjectDecisionsTabPending {
		if env.Pending == nil || env.Made != "" {
			return profileDecisionCursor{}, errors.New("malformed pending cursor")
		}
		c := env.Pending
		if (c.Section != 0 && c.Section != 1) || c.MessageID == "" || c.CreatedAt.IsZero() {
			return profileDecisionCursor{}, errors.New("malformed pending cursor")
		}
		c.CreatedAt = c.CreatedAt.UTC()
		if pq.sort == DecisionsSortProject {
			if c.ProjectID == "" || len(c.ProjectID) > maxProfileDecisionProjectParamBytes || len(c.DoneProjects) > maxProfileCursorDoneProjects {
				return profileDecisionCursor{}, errors.New("malformed project pending cursor")
			}
			seenDone := make(map[string]bool, len(c.DoneProjects))
			for _, pid := range c.DoneProjects {
				if pid == "" || len(pid) > maxProfileDecisionProjectParamBytes || pid == c.ProjectID || seenDone[pid] {
					return profileDecisionCursor{}, errors.New("malformed project pending cursor")
				}
				seenDone[pid] = true
			}
		} else if len(c.DoneProjects) > 0 || c.ProjectID != "" {
			return profileDecisionCursor{}, errors.New("malformed recent pending cursor")
		}
		return env, nil
	}
	if env.Made == "" || env.Pending != nil {
		return profileDecisionCursor{}, errors.New("malformed made cursor")
	}
	return env, nil
}

// handleUserMeDecisions serves GET /api/v1/users/me/decisions.
// Unsupported methods return 405 before the experiment gate, which returns 404
// while web.decisions is off.
func (s *Server) handleUserMeDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.requireExperiment(experiments.Decisions, func(w http.ResponseWriter, r *http.Request) {
		s.listUserMeDecisions(w, r)
	})(w, r)
}

func profileDecisionsCaller(w http.ResponseWriter, r *http.Request) (UserIdentity, *ScopedUserIdentity, bool) {
	ctx := r.Context()
	if GetAgentIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agents cannot read decisions", nil)
		return nil, nil, false
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		Unauthorized(w)
		return nil, nil, false
	}
	if isVisitorIdentity(identity) {
		logAuthzDenial(r, identity, Resource{Type: "user", ID: identity.ID()}, ActionRead, "profile decisions are not open to visitors")
		writeVisitorRefusal(w)
		return nil, nil, false
	}
	cls, token := inboxCredentialOf(ctx)
	if cls != inboxCredentialSession && cls != inboxCredentialToken {
		logAuthzDenial(r, identity, Resource{Type: "user", ID: identity.ID()}, ActionRead, "credential cannot read profile decisions")
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

func parseProfileDecisionsQuery(w http.ResponseWriter, r *http.Request, viewerID string) (projectDecisionsQuery, bool) {
	q := r.URL.Query()
	out := projectDecisionsQuery{
		profile:  true,
		viewerID: viewerID,
		tab:      strings.TrimSpace(q.Get("tab")),
		sort:     strings.TrimSpace(q.Get("sort")),
		limit:    defaultProjectDecisionsLimit,
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

	switch out.sort {
	case "":
		// R6 defaults: Waiting on you -> project, Made -> recent.
		if out.tab == ProjectDecisionsTabPending {
			out.sort = DecisionsSortProject
		} else {
			out.sort = DecisionsSortRecent
		}
	case DecisionsSortRecent, DecisionsSortProject:
	default:
		ValidationError(w, fmt.Sprintf("sort %q is not valid; expected %q or %q", out.sort, DecisionsSortRecent, DecisionsSortProject),
			map[string]interface{}{"field": "sort"})
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

	if search := strings.TrimSpace(q.Get("q")); search != "" {
		if n := utf8.RuneCountInString(search); n < minProjectDecisionsQueryRunes || n > maxProjectDecisionsQueryRunes {
			ValidationError(w, fmt.Sprintf("q must be %d to %d characters", minProjectDecisionsQueryRunes, maxProjectDecisionsQueryRunes),
				map[string]interface{}{"field": "q"})
			return projectDecisionsQuery{}, false
		}
		out.q = search
	}
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
	out.projectID = strings.TrimSpace(q.Get("project"))
	if len(out.projectID) > maxProfileDecisionProjectParamBytes {
		ValidationError(w, fmt.Sprintf("project must be at most %d bytes", maxProfileDecisionProjectParamBytes),
			map[string]interface{}{"field": "project"})
		return projectDecisionsQuery{}, false
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

	rawCursor := strings.TrimSpace(q.Get("cursor"))
	if rawCursor != "" {
		if out.tab == ProjectDecisionsTabMade && out.sort == DecisionsSortProject {
			ValidationError(w, "cursor is not supported when sort=project on tab=made",
				map[string]interface{}{"field": "cursor"})
			return projectDecisionsQuery{}, false
		}
		env, err := decodeProfileDecisionCursor(rawCursor, out)
		if err != nil {
			ValidationError(w, "invalid cursor", map[string]interface{}{"field": "cursor"})
			return projectDecisionsQuery{}, false
		}
		if out.tab == ProjectDecisionsTabPending {
			out.pendingCursor = env.Pending
		} else {
			out.cursor = env.Made
		}
	}

	return out, true
}

func (s *Server) listUserMeDecisions(w http.ResponseWriter, r *http.Request) {
	user, token, ok := profileDecisionsCaller(w, r)
	if !ok {
		return
	}
	pq, ok := parseProfileDecisionsQuery(w, r, user.ID())
	if !ok {
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	// Every profile request runs under the chat search time limit (design D9).
	// Install a request-local authorization input memo across this read-only
	// evaluation phase so up to 200 per-project scopes and asker checks load
	// the caller's principal closure, bindings and constraints once.
	rawSearchCtx, cancel := context.WithTimeout(r.Context(), s.chatSearchTimeout())
	defer cancel()
	searchCtx := withAuthzInputMemo(rawSearchCtx)

	selected, truncated, err := s.selectProfileDecisionProjects(searchCtx, user, token, pq.projectID)
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	if pq.limit == 0 && len(selected) > 0 {
		// A limit=0 probe returns only open-prompt counts and truncation state
		// (no facets or answer lists), so projects without unsettled decisions
		// contribute nothing and can skip per-project scope resolution.
		selected, err = s.filterProjectsWithUnsettledDecisions(searchCtx, selected)
		if err != nil {
			writeDecisionsError(w, r, searchCtx, err)
			return
		}
	}
	scopes, err := s.resolveProfileDecisionScopes(searchCtx, selected, user, token, wcs)
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	pq.truncatedProjects = truncated

	resp, err := s.decisionsCore(searchCtx, user, token, wcs, scopes, pq)
	if err == nil && errors.Is(searchCtx.Err(), context.DeadlineExceeded) {
		err = searchCtx.Err()
	}
	if err != nil {
		writeDecisionsError(w, r, searchCtx, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// profileDecisionScopes resolves readable conversation scopes across the
// projects viewer is a member of and can read (design R2, R3, 5.2).
// Non-member projects (even for hub admins) and visitor-only bindings are
// excluded by chatMemberProjectIDs; member projects with no decisions are
// skipped via ProjectsWithDecisions; and the scope count is capped at
// maxProfileDecisionProjects (200), returning truncated == true beyond the cap.
func (s *Server) profileDecisionScopes(
	ctx context.Context,
	user UserIdentity,
	token *ScopedUserIdentity,
	wcs WebChatStore,
	ensureProjectID string,
) ([]*projectReadableScope, bool, error) {
	selected, truncated, err := s.selectProfileDecisionProjects(ctx, user, token, ensureProjectID)
	if err != nil || len(selected) == 0 {
		return nil, truncated, err
	}
	scopes, err := s.resolveProfileDecisionScopes(ctx, selected, user, token, wcs)
	if err != nil {
		return nil, false, err
	}
	return scopes, truncated, nil
}

// selectProfileDecisionProjects selects up to maxProfileDecisionProjects
// readable member projects that have decisions, ordered by newest decision
// (design R2, R3, 5.2). Shared by profileDecisionScopes and waitingOnYouTotal
// so both use identical project selection.
func (s *Server) selectProfileDecisionProjects(
	ctx context.Context,
	user UserIdentity,
	token *ScopedUserIdentity,
	ensureProjectID string,
) ([]*store.Project, bool, error) {
	memberMap, err := s.chatMemberProjectIDs(ctx, user.ID())
	if err != nil {
		return nil, false, err
	}
	if len(memberMap) == 0 {
		return nil, false, nil
	}

	memberIDs := make([]string, 0, len(memberMap))
	for pid := range memberMap {
		memberIDs = append(memberIDs, pid)
	}
	sort.Strings(memberIDs)

	candidateIDs, err := s.store.ProjectsWithDecisions(ctx, memberIDs)
	if err != nil {
		return nil, false, err
	}
	if len(candidateIDs) == 0 {
		return nil, false, nil
	}

	byID := make(map[string]*store.Project, len(candidateIDs))
	for start := 0; start < len(candidateIDs); start += profileProjectSummariesBatchSize {
		end := start + profileProjectSummariesBatchSize
		if end > len(candidateIDs) {
			end = len(candidateIDs)
		}
		batch := candidateIDs[start:end]
		res, err := s.store.ListProjectSummaries(ctx, store.ProjectFilter{MemberProjectIDs: batch}, store.ListOptions{Limit: len(batch), SkipTotalCount: true})
		if err != nil {
			return nil, false, err
		}
		for i := range res.Items {
			p := res.Items[i]
			byID[p.ID] = &p
		}
	}

	ordered := make([]*store.Project, 0, len(candidateIDs))
	for _, pid := range candidateIDs {
		if p := byID[pid]; p != nil {
			ordered = append(ordered, p)
		}
	}
	if len(ordered) == 0 {
		return nil, false, nil
	}

	readable := ordered
	if s.authzService != nil {
		var authIdentity Identity = user
		if token != nil {
			authIdentity = token
		}
		resources := make([]Resource, len(ordered))
		for i, p := range ordered {
			resources[i] = projectResource(p)
		}
		// Evaluate ActionRead only (same decision path as ComputeCapabilitiesBatch
		// without deciding the remaining project actions across 200 projects).
		caps := s.authzService.ComputeCapabilitiesForActions(ctx, authIdentity, resources, []Action{ActionRead})
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		readable = make([]*store.Project, 0, len(ordered))
		for i, p := range ordered {
			if capabilityAllows(caps[i], ActionRead) {
				readable = append(readable, p)
			}
		}
	}
	if len(readable) == 0 {
		return nil, false, nil
	}

	truncated := len(readable) > maxProfileDecisionProjects
	selected := readable
	if truncated {
		selected = make([]*store.Project, maxProfileDecisionProjects)
		copy(selected, readable[:maxProfileDecisionProjects])
		// When the caller explicitly picks a readable member project beyond
		// the 200 cap ("pick a project to see others", design R3), swap it
		// into the last slot so total scope resolutions stay <= 200.
		if ensureProjectID != "" {
			inTop := false
			for _, p := range selected {
				if p.ID == ensureProjectID {
					inTop = true
					break
				}
			}
			if !inTop {
				for _, p := range readable[maxProfileDecisionProjects:] {
					if p.ID == ensureProjectID {
						selected[len(selected)-1] = p
						break
					}
				}
			}
		}
	}

	return selected, truncated, nil
}

// filterProjectsWithUnsettledDecisions narrows projects to the subset holding
// at least one decision with settled=false (design-scale §3.5).
func (s *Server) filterProjectsWithUnsettledDecisions(ctx context.Context, projects []*store.Project) ([]*store.Project, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ID
	}
	unsettled, err := s.store.ProjectsWithUnsettledDecisions(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(unsettled) == 0 {
		return nil, nil
	}
	out := make([]*store.Project, 0, len(unsettled))
	for _, p := range projects {
		if unsettled[p.ID] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Server) resolveProfileDecisionScopes(
	ctx context.Context,
	selected []*store.Project,
	user UserIdentity,
	token *ScopedUserIdentity,
	wcs WebChatStore,
) ([]*projectReadableScope, error) {
	scopes := make([]*projectReadableScope, 0, len(selected))
	for _, p := range selected {
		sc, err := s.resolveProjectReadableScope(ctx, p, user, token, wcs)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, sc)
	}
	return scopes, nil
}

// decisionProjectFacetViews builds the profile projects facet from the
// readable conversation keys that have decisions (design AC 6a, D5): sorted
// case-insensitively by project name, capped at maxProjectDecisionFacetOptions.
func decisionProjectFacetViews(set *decisionScopeSet, convKeys []string) []ProjectDecisionProjectRef {
	seen := make(map[string]bool)
	out := make([]ProjectDecisionProjectRef, 0, len(set.projectIDs))
	for _, k := range convKeys {
		if !set.readable[k] {
			continue
		}
		pid := set.keyProject[k]
		if pid == "" || seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, set.projectRef(pid))
	}
	sort.Slice(out, func(i, j int) bool {
		ni, nj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if ni != nj {
			return ni < nj
		}
		si, sj := strings.ToLower(out[i].Slug), strings.ToLower(out[j].Slug)
		if si != sj {
			return si < sj
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > maxProjectDecisionFacetOptions {
		out = out[:maxProjectDecisionFacetOptions]
	}
	return out
}

func (s *Server) finishProfilePendingDecisions(
	ctx context.Context,
	set *decisionScopeSet,
	pq projectDecisionsQuery,
	counts ProjectDecisionCounts,
	filteredCounts *ProjectDecisionCounts,
	classified []classifiedOpenPrompt,
	agentsFacet []ProjectDecisionActorView,
	answerersFacet []ProjectDecisionActorView,
	conversationsFacet []ProjectDecisionConversationView,
	projectsFacet []ProjectDecisionProjectRef,
) (ProfilePendingDecisionsResponse, error) {
	var (
		pagePrompts []classifiedOpenPrompt
		nextPending *pendingPromptCursor
	)
	if pq.sort == DecisionsSortProject {
		pagePrompts, nextPending = paginateClassifiedPromptsByProject(classified, pq.pendingCursor, pq.limit)
	} else {
		var rawNext string
		var err error
		pagePrompts, rawNext, err = paginateClassifiedPrompts(classified, pq.pendingCursor, pq.limit)
		if err != nil {
			return ProfilePendingDecisionsResponse{}, err
		}
		if rawNext != "" {
			pc, derr := decodePendingPromptCursor(rawNext)
			if derr != nil {
				return ProfilePendingDecisionsResponse{}, derr
			}
			nextPending = &pc
		}
	}

	var nextCursor string
	if nextPending != nil {
		var err error
		nextCursor, err = encodeProfileDecisionCursor(pq, nextPending, "")
		if err != nil {
			return ProfilePendingDecisionsResponse{}, err
		}
	}

	return ProfilePendingDecisionsResponse{
		Counts:            counts,
		FilteredCounts:    filteredCounts,
		Prompts:           s.buildProjectDecisionPromptViews(ctx, set, pagePrompts),
		Agents:            agentsFacet,
		Answerers:         answerersFacet,
		Conversations:     conversationsFacet,
		Projects:          projectsFacet,
		TruncatedProjects: pq.truncatedProjects,
		NextCursor:        nextCursor,
	}, nil
}

// paginateClassifiedPromptsByProject orders open prompts within each section
// (0 = waitingOnYou, 1 = waitingOnOthers-only) by each project group's newest
// prompt CreatedAt DESC (tie-broken by ProjectID ASC), then newest first
// within each project group (CreatedAt DESC, MessageID DESC), and slices the
// page after cur (design R4, AC 7).
//
// Because a group's live newest prompt timestamp shifts when a prompt is
// added or answered inline, the cursor records the finished project IDs in
// cur.Section plus the keyset position (CreatedAt, MessageID) inside the
// active group cur.ProjectID: subsequent pages finish cur.ProjectID first,
// skip cur.DoneProjects, and order remaining unfinished groups by live
// groupAt DESC, ProjectID ASC.
func paginateClassifiedPromptsByProject(
	all []classifiedOpenPrompt,
	cur *pendingPromptCursor,
	limit int,
) ([]classifiedOpenPrompt, *pendingPromptCursor) {
	if limit == 0 || len(all) == 0 {
		return nil, nil
	}

	type sectionProject struct {
		section   int
		projectID string
	}
	groupNewest := make(map[sectionProject]time.Time)
	for _, c := range all {
		sp := sectionProject{section: c.section, projectID: c.raw.ProjectID}
		if curAt, ok := groupNewest[sp]; !ok || c.raw.CreatedAt.After(curAt) {
			groupNewest[sp] = c.raw.CreatedAt.UTC()
		}
	}
	groupAt := func(c classifiedOpenPrompt) time.Time {
		return groupNewest[sectionProject{section: c.section, projectID: c.raw.ProjectID}]
	}
	promptBefore := func(a, b classifiedOpenPrompt) bool {
		if !a.raw.CreatedAt.Equal(b.raw.CreatedAt) {
			return a.raw.CreatedAt.After(b.raw.CreatedAt)
		}
		return a.raw.MessageID > b.raw.MessageID
	}
	groupAndPromptOrder := func(items []classifiedOpenPrompt) {
		sort.Slice(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if a.section != b.section {
				return a.section < b.section
			}
			ga, gb := groupAt(a), groupAt(b)
			if !ga.Equal(gb) {
				return ga.After(gb)
			}
			if a.raw.ProjectID != b.raw.ProjectID {
				return a.raw.ProjectID < b.raw.ProjectID
			}
			return promptBefore(a, b)
		})
	}

	var rem []classifiedOpenPrompt
	if cur == nil {
		rem = make([]classifiedOpenPrompt, len(all))
		copy(rem, all)
		groupAndPromptOrder(rem)
	} else {
		doneSet := make(map[string]bool, len(cur.DoneProjects))
		for _, pid := range cur.DoneProjects {
			doneSet[pid] = true
		}
		var activeGroup []classifiedOpenPrompt
		var rest []classifiedOpenPrompt
		for _, item := range all {
			if item.section < cur.Section {
				continue
			}
			if item.section == cur.Section {
				if doneSet[item.raw.ProjectID] {
					continue
				}
				if item.raw.ProjectID == cur.ProjectID {
					afterInGroup := item.raw.CreatedAt.Before(cur.CreatedAt) ||
						(item.raw.CreatedAt.Equal(cur.CreatedAt) && item.raw.MessageID < cur.MessageID)
					if afterInGroup {
						activeGroup = append(activeGroup, item)
					}
					continue
				}
			}
			rest = append(rest, item)
		}
		sort.Slice(activeGroup, func(i, j int) bool {
			return promptBefore(activeGroup[i], activeGroup[j])
		})
		groupAndPromptOrder(rest)
		rem = make([]classifiedOpenPrompt, 0, len(activeGroup)+len(rest))
		rem = append(rem, activeGroup...)
		rem = append(rem, rest...)
	}

	if len(rem) == 0 {
		return nil, nil
	}
	if len(rem) <= limit {
		return rem, nil
	}
	page := rem[:limit]
	last := page[len(page)-1]

	var done []string
	seenDone := make(map[string]bool)
	addDone := func(pid string) {
		if pid == "" || pid == last.raw.ProjectID || seenDone[pid] {
			return
		}
		seenDone[pid] = true
		done = append(done, pid)
	}
	if cur != nil && cur.Section == last.section {
		for _, pid := range cur.DoneProjects {
			addDone(pid)
		}
		addDone(cur.ProjectID)
	}
	for _, item := range page {
		if item.section == last.section {
			addDone(item.raw.ProjectID)
		}
	}

	return page, &pendingPromptCursor{
		Section:      last.section,
		DoneProjects: done,
		ProjectID:    last.raw.ProjectID,
		CreatedAt:    last.raw.CreatedAt.UTC(),
		MessageID:    last.raw.MessageID,
	}
}

func (s *Server) finishProfileMadeDecisions(
	ctx context.Context,
	wcs WebChatStore,
	set *decisionScopeSet,
	pq projectDecisionsQuery,
	targetProjectIDs []string,
	listKeys []string,
	filter store.ProjectDecisionAnswerFilter,
	counts ProjectDecisionCounts,
	agentsFacet []ProjectDecisionActorView,
	answerersFacet []ProjectDecisionActorView,
	conversationsFacet []ProjectDecisionConversationView,
	projectsFacet []ProjectDecisionProjectRef,
) (any, error) {
	if pq.sort == DecisionsSortProject {
		groups, err := s.profileMadeByProjectGroups(ctx, wcs, set, targetProjectIDs, listKeys, filter, pq.limit)
		if err != nil {
			return nil, err
		}
		return ProfileMadeByProjectDecisionsResponse{
			Counts:            counts,
			Groups:            groups,
			Agents:            agentsFacet,
			Answerers:         answerersFacet,
			Conversations:     conversationsFacet,
			Projects:          projectsFacet,
			TruncatedProjects: pq.truncatedProjects,
		}, nil
	}

	rawEntries, rawNext, err := s.store.ListDecisionAnswers(ctx, targetProjectIDs, listKeys, filter, pq.cursor, pq.limit)
	if err != nil {
		return nil, err
	}
	var nextCursor string
	if rawNext != "" {
		nextCursor, err = encodeProfileDecisionCursor(pq, nil, rawNext)
		if err != nil {
			return nil, err
		}
	}
	return ProfileMadeDecisionsResponse{
		Counts:            counts,
		Entries:           s.buildProjectDecisionLogViews(ctx, wcs, set, rawEntries),
		Agents:            agentsFacet,
		Answerers:         answerersFacet,
		Conversations:     conversationsFacet,
		Projects:          projectsFacet,
		TruncatedProjects: pq.truncatedProjects,
		NextCursor:        nextCursor,
	}, nil
}

func (s *Server) profileMadeByProjectGroups(
	ctx context.Context,
	wcs WebChatStore,
	set *decisionScopeSet,
	targetProjectIDs []string,
	listKeys []string,
	filter store.ProjectDecisionAnswerFilter,
	limit int,
) ([]ProfileDecisionMadeGroupView, error) {
	if limit == 0 {
		return []ProfileDecisionMadeGroupView{}, nil
	}
	keysByProject := make(map[string][]string, len(targetProjectIDs))
	for _, k := range listKeys {
		if pid := set.keyProject[k]; pid != "" {
			keysByProject[pid] = append(keysByProject[pid], k)
		}
	}
	byProject, err := s.store.ListDecisionAnswersPerProject(
		ctx,
		targetProjectIDs,
		keysByProject,
		filter,
		profileMadeOverviewPerProject+1,
	)
	if err != nil {
		return nil, err
	}
	if len(byProject) == 0 {
		return []ProfileDecisionMadeGroupView{}, nil
	}

	type rawGroup struct {
		projectID string
		entries   []*store.ProjectDecisionAnswerEntry
		more      bool
		latestAt  time.Time
		latestID  string
	}
	rawGroups := make([]rawGroup, 0, len(byProject))
	totalEntries := 0
	for pid, entries := range byProject {
		if len(entries) == 0 {
			continue
		}
		more := len(entries) > profileMadeOverviewPerProject
		if more {
			entries = entries[:profileMadeOverviewPerProject]
		}
		rawGroups = append(rawGroups, rawGroup{
			projectID: pid,
			entries:   entries,
			more:      more,
			latestAt:  entries[0].Answer.CreatedAt,
			latestID:  entries[0].Answer.ID,
		})
		totalEntries += len(entries)
	}
	sort.Slice(rawGroups, func(i, j int) bool {
		if !rawGroups[i].latestAt.Equal(rawGroups[j].latestAt) {
			return rawGroups[i].latestAt.After(rawGroups[j].latestAt)
		}
		if rawGroups[i].latestID != rawGroups[j].latestID {
			return rawGroups[i].latestID > rawGroups[j].latestID
		}
		return rawGroups[i].projectID < rawGroups[j].projectID
	})

	flatEntries := make([]*store.ProjectDecisionAnswerEntry, 0, totalEntries)
	for _, g := range rawGroups {
		flatEntries = append(flatEntries, g.entries...)
	}
	allViews := s.buildProjectDecisionLogViews(ctx, wcs, set, flatEntries)

	groups := make([]ProfileDecisionMadeGroupView, len(rawGroups))
	offset := 0
	for i, g := range rawGroups {
		n := len(g.entries)
		sliceViews := make([]ProjectDecisionLogEntryView, n)
		copy(sliceViews, allViews[offset:offset+n])
		offset += n
		groups[i] = ProfileDecisionMadeGroupView{
			Project: set.projectRef(g.projectID),
			Entries: sliceViews,
			More:    g.more,
		}
	}
	return groups, nil
}

// waitingOnYouTotal returns the total count of open decisions currently
// waiting on userID across userID's readable member projects (capped at
// maxProfileDecisionProjects), and the count of distinct projects that hold at
// least one of those decisions (design 5.2, for P6's bell reconciler).
//
// Contract: roll-up evaluation across up to 200 member projects honors the D9
// chat search time limit by construction — if ctx carries no deadline,
// waitingOnYouTotal wraps it in context.WithTimeout(ctx, s.chatSearchTimeout()).
// Missing or non-active accounts (suspended, invited, pending, deleting)
// return (0, 0, nil); store lookup faults return (0, 0, err).
//
func (s *Server) waitingOnYouTotal(ctx context.Context, userID string) (total, projects int, err error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0, 0, nil
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.chatSearchTimeout())
		defer cancel()
	}
	u, err := checkUserAccount(ctx, s.store, PrincipalContext{Kind: PrincipalKindUser, ID: userID})
	switch classifyUserAccount(u, err) {
	case accountAllowed:
	case accountLookupFault:
		return 0, 0, err
	default:
		return 0, 0, nil
	}
	if u == nil {
		return 0, 0, nil
	}

	viewer := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
	ctx = withAuthzInputMemo(ctx)
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	selected, _, err := s.selectProfileDecisionProjects(ctx, viewer, nil, "")
	if err != nil || len(selected) == 0 {
		return 0, 0, err
	}
	openSelected, err := s.filterProjectsWithUnsettledDecisions(ctx, selected)
	if err != nil || len(openSelected) == 0 {
		return 0, 0, err
	}
	scopes, err := s.resolveProfileDecisionScopes(ctx, openSelected, viewer, nil, wcs)
	if err != nil || len(scopes) == 0 {
		return 0, 0, err
	}
	set := newDecisionScopeSet(scopes)
	if len(set.chatKeys) == 0 {
		return 0, 0, nil
	}

	openPrompts, err := s.store.ListOpenPrompts(ctx, set.projectIDs, set.chatKeys)
	if err != nil {
		return 0, 0, err
	}
	counts, classified, err := s.classifyProjectOpenPrompts(ctx, viewer, nil, wcs, set.projectReadableScope, openPrompts)
	if err != nil {
		return 0, 0, err
	}

	projectsWithWaiting := make(map[string]bool)
	for _, p := range classified {
		if p.onYou > 0 {
			projectsWithWaiting[p.raw.ProjectID] = true
		}
	}
	return counts.WaitingOnYou, len(projectsWithWaiting), nil
}
