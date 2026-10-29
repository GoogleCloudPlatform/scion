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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// GET /api/v1/chat/starred-conversations: the Starred rail
// (web.chat_starred_messages). Each item is one conversation the caller
// can read now in which the caller has at least one live star, with its
// current name, activity and project. No excerpt, count or other user's
// state is returned.
//
// Query: projectId (optional, a project the caller can read), sort
// (activity, the default, or alpha), limit (1-100, default 30) and cursor.
// The store groups the caller's stars by conversation before a page is
// cut (webchat_star_conversations.go); this handler authorizes every
// candidate with the same rules as the other star routes
// (resolveStarScope), batched: one lookup per batch for projects, agents
// and users, one read decision per project, and request-local caches only.
// Candidates are scanned in batches of 100, at most 1,000 per response;
// the sealed cursor resumes after the last scanned candidate, so a page
// can be short, or empty with a cursor, when many candidates are hidden.

const (
	// chatStarredConversationsBinding is the cursor binding endpoint; its
	// version suffix is the schema version of the sealed position.
	chatStarredConversationsBinding = "chat.starred_conversations.v1"
	// starredConversationsScanBatch is the candidate batch size.
	starredConversationsScanBatch = 100
	// starredConversationsScanBudget bounds the candidates one response
	// scans.
	starredConversationsScanBudget = 1000
)

// starredConversationItem is one row of the Starred rail.
type starredConversationItem struct {
	ConversationKey  string     `json:"conversationKey"`
	ConversationKind string     `json:"conversationKind"`
	ConversationName string     `json:"conversationName"`
	LastActivityAt   *time.Time `json:"lastActivityAt,omitempty"`
	ProjectID        string     `json:"projectId,omitempty"`
	ProjectName      string     `json:"projectName,omitempty"`
}

// starredConversationsResponse is one page of the Starred rail.
type starredConversationsResponse struct {
	Items      []starredConversationItem `json:"items"`
	NextCursor string                    `json:"nextCursor,omitempty"`
}

// starredConversationsCursor is the sealed position: the sort value and
// the conversation key of the last scanned candidate.
type starredConversationsCursor struct {
	Value string `json:"value"`
	Key   string `json:"key"`
}

// handleChatStarredConversations serves /api/v1/chat/starred-conversations.
func (s *Server) handleChatStarredConversations(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(experiments.ChatStarredMessages) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	call, ok := s.beginStarCall(w, r, permInboxRead)
	if !ok {
		return
	}
	ctx := r.Context()
	q, ok := starQuery(w, r, []string{"projectId", "sort", "limit", "cursor"}, "")
	if !ok {
		return
	}
	projectID := ""
	if raw, set := q["projectId"]; set {
		id, ok := parseStarMessageID(raw[0])
		if !ok {
			BadRequest(w, "invalid projectId")
			return
		}
		projectID = id
	}
	sortMode := StarredConversationsByActivity
	if raw, set := q["sort"]; set {
		if raw[0] != StarredConversationsByActivity && raw[0] != StarredConversationsByName {
			BadRequest(w, "sort must be activity or alpha")
			return
		}
		sortMode = raw[0]
	}
	limit := chatStarsDefaultLimit
	if raw, set := q["limit"]; set {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > chatStarsMaxLimit {
			BadRequest(w, "limit must be between 1 and 100")
			return
		}
		limit = n
	}

	auth := s.newStarredConversationAuthorizer(r, call)
	if projectID != "" {
		// The project scope is authorized before anything is queried; a
		// project the caller may not read is answered as a missing one.
		allowed, err := auth.projectScopeAllowed(ctx, projectID)
		if err != nil {
			slog.Error("chat stars: starred conversations project scope failed", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to look up project", nil)
			return
		}
		if !allowed {
			logReferenceRefused(ctx, logging.RequestPath(r), "starred conversations project scope is not readable", GetIdentityFromContext(ctx))
			NotFound(w, "Project")
			return
		}
	}

	binding := scopedCursorBinding(chatStarredConversationsBinding, struct {
		ProjectID string `json:"projectId"`
		Sort      string `json:"sort"`
	}{projectID, sortMode}, GetIdentityFromContext(ctx))
	var after *StarredConversationPosition
	if cursor := q.Get("cursor"); cursor != "" {
		pos, err := s.openStarredConversationsCursor(cursor, binding, sortMode)
		if err != nil {
			writeErrorFromErr(w, errInvalidCursor, "")
			return
		}
		after = &pos
	}

	// The read switch chooses the history selector, as resolveChatReadTarget
	// reads it for the other star paths.
	ops := s.GetOperationalSettings()
	filter := StarredConversationFilter{
		UserID:           call.user.ID(),
		Channel:          chatHistoryChannel(GetIdentityFromContext(ctx)),
		ByConversationID: ops != nil && ops.ConversationEnvelopeSwitch(),
		ProjectID:        projectID,
		Sort:             sortMode,
	}
	items, next, err := s.scanStarredConversations(ctx, call, auth, filter, after, limit,
		starredConversationsScanBatch, starredConversationsScanBudget)
	if err != nil {
		slog.Error("chat stars: starred conversations failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to list starred conversations", nil)
		return
	}
	resp := starredConversationsResponse{Items: items}
	if next != nil {
		sealed, err := s.sealStarredConversationsCursor(*next, binding)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to list starred conversations", nil)
			return
		}
		resp.NextCursor = sealed
	}
	writeJSON(w, http.StatusOK, resp)
}

// scanStarredConversations fills one page: it reads candidate batches
// after the position, authorizes each batch, and keeps the readable ones
// until limit items are found, the candidates end, or the scan budget is
// spent (batch candidates per store query, at most budget in all). next is
// the position of the last scanned candidate when more may remain, nil at
// the end.
func (s *Server) scanStarredConversations(ctx context.Context, call starCall, auth *starredConversationAuthorizer, filter StarredConversationFilter, after *StarredConversationPosition, limit, batch, budget int) ([]starredConversationItem, *StarredConversationPosition, error) {
	items := []starredConversationItem{}
	pos := after
	scanned := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		filter.After = pos
		filter.Limit = min(batch, budget-scanned)
		page, err := call.stars.ListStarredConversationCandidates(ctx, filter)
		if err != nil {
			return nil, nil, err
		}
		rows, err := auth.decide(ctx, page.Items, filter.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		for i, c := range page.Items {
			scanned++
			p := c.Position
			pos = &p
			if rows[i] != nil {
				items = append(items, *rows[i])
			}
			if len(items) == limit {
				if i < len(page.Items)-1 || page.Next != nil {
					return items, pos, nil
				}
				return items, nil, nil
			}
		}
		if page.Next == nil {
			return items, nil, nil
		}
		if scanned >= budget {
			return items, pos, nil
		}
	}
}

// sealStarredConversationsCursor seals a position under binding. The
// position (a sort value and a conversation key) does not fit
// authorizedList's "created,id" cursor, so it is sealed with
// SealPosition, whose cursors never open as list cursors (or the reverse).
func (s *Server) sealStarredConversationsCursor(pos StarredConversationPosition, binding string) (string, error) {
	raw, err := json.Marshal(starredConversationsCursor(pos))
	if err != nil {
		return "", err
	}
	return s.listCursorSealer.SealPosition(string(raw), binding)
}

// openStarredConversationsCursor opens a sealed position and validates it
// for the order: a conversation key, and for activity a fixed-width UTC
// activity value or "" (no activity). A name value is not length-bounded:
// the hub sealed it from a stored name, and the sealer's authentication
// already rejects any other value.
func (s *Server) openStarredConversationsCursor(sealed, binding, sortMode string) (StarredConversationPosition, error) {
	raw, err := s.listCursorSealer.OpenPosition(sealed, binding)
	if err != nil {
		return StarredConversationPosition{}, errInvalidCursor
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var c starredConversationsCursor
	if err := dec.Decode(&c); err != nil || dec.More() {
		return StarredConversationPosition{}, errInvalidCursor
	}
	if !validStarConversationKey(c.Key) {
		return StarredConversationPosition{}, errInvalidCursor
	}
	if sortMode == StarredConversationsByActivity {
		if _, err := parseStarActivityKey(c.Value); err != nil {
			return StarredConversationPosition{}, errInvalidCursor
		}
	}
	return StarredConversationPosition(c), nil
}

// starredConversationAuthorizer decides read access for Starred rail
// candidates with the rules of resolveStarScope, batched. Its caches hold
// only this request's lookups and decisions.
type starredConversationAuthorizer struct {
	s        *Server
	call     starCall
	identity Identity
	// check is the token's inbox:read self-scope check (nil without a
	// token); it caches project membership per project.
	check *selfScopeCheck

	// projects holds loaded projects; a nil value is a project that does
	// not exist.
	projects map[string]*store.Project
	// projectRead caches the project read decision per project.
	projectRead map[string]bool
	// agents holds loaded agents (soft-deleted included); nil when absent.
	agents map[string]*store.Agent
	// users holds loaded users; nil when absent.
	users map[string]*store.User
	// agentRead caches a token's agent:read decision per agent.
	agentRead map[string]bool
}

func (s *Server) newStarredConversationAuthorizer(r *http.Request, call starCall) *starredConversationAuthorizer {
	a := &starredConversationAuthorizer{
		s: s, call: call,
		identity:    GetIdentityFromContext(r.Context()),
		projects:    map[string]*store.Project{},
		projectRead: map[string]bool{},
		agents:      map[string]*store.Agent{},
		users:       map[string]*store.User{},
		agentRead:   map[string]bool{},
	}
	if call.token != nil {
		a.check = s.newSelfScopeCheck(r.Context(), call.token, permInboxRead)
	}
	return a
}

// projectScopeAllowed reports whether the caller may scope the rail to a
// project: it exists, the caller may read it, and a token's boundary
// admits it under inbox:read.
func (a *starredConversationAuthorizer) projectScopeAllowed(ctx context.Context, projectID string) (bool, error) {
	if err := a.loadProjects(ctx, []string{projectID}); err != nil {
		return false, err
	}
	if !a.readsProject(ctx, projectID) {
		return false, nil
	}
	return a.check == nil || a.check.allows(projectID), nil
}

// starDMPeerRef is the other participant of a DM key the caller is in.
type starDMPeerRef struct {
	kind, id string
}

// starDMPeer returns the peer of a well-formed, canonical DM key in which
// userID is a user participant (as resolveStarDM derives it), or false. A
// key that is not canonical (a legacy star stored under swapped slots, or
// one that does not canonicalise) is refused here as resolveStarDM refuses
// it, so it never yields a row whose conversation then answers 400.
func starDMPeer(key, userID string) (starDMPeerRef, bool) {
	if !validDMKey(key) || !isDMParticipant(key, userID) {
		return starDMPeerRef{}, false
	}
	if canonical, err := canonicalStarDMKey(key); err != nil || canonical != key {
		return starDMPeerRef{}, false
	}
	parts := strings.Split(key, ":")
	kind, id := parts[1], parts[2]
	if kind == "user" && id == userID {
		kind, id = parts[3], parts[4]
	}
	return starDMPeerRef{kind: kind, id: id}, true
}

// decide returns, aligned with cands, the rail row of each candidate the
// caller may read now, or nil. projectScope, when set, also drops an agent
// DM whose agent is no longer in that project. A failed lookup is an
// error, never a refusal.
func (a *starredConversationAuthorizer) decide(ctx context.Context, cands []StarredConversationCandidate, projectScope string) ([]*starredConversationItem, error) {
	userID := a.call.user.ID()
	peers := make([]starDMPeerRef, len(cands))
	participant := make([]bool, len(cands))
	var agentIDs, userIDs, threadProjectIDs []string
	for i, c := range cands {
		if !c.IsDM {
			threadProjectIDs = append(threadProjectIDs, c.ProjectID)
			continue
		}
		peer, ok := starDMPeer(c.ConversationKey, userID)
		if !ok {
			continue
		}
		peers[i], participant[i] = peer, true
		switch {
		case !canonicalStarID(peer.id):
			// No usable stable ID: nothing to look up (the batch lookups skip
			// it), as hydrateStarItems labels it.
		case peer.kind == "agent":
			agentIDs = append(agentIDs, peer.id)
		case peer.kind == "user":
			userIDs = append(userIDs, peer.id)
		}
	}
	if err := a.loadAgents(ctx, agentIDs); err != nil {
		return nil, err
	}
	if err := a.loadUsers(ctx, userIDs); err != nil {
		return nil, err
	}
	// One project lookup for the threads' projects and the agents' projects
	// (an agent DM row names its agent's project); read decisions are needed
	// for the threads' projects only.
	projectIDs := append([]string{}, threadProjectIDs...)
	for i := range cands {
		if participant[i] && peers[i].kind == "agent" {
			if agent := a.agents[peers[i].id]; agent != nil && agent.ProjectID != "" {
				projectIDs = append(projectIDs, agent.ProjectID)
			}
		}
	}
	if err := a.loadProjects(ctx, projectIDs); err != nil {
		return nil, err
	}
	a.decideProjectReads(ctx, threadProjectIDs)

	out := make([]*starredConversationItem, len(cands))
	for i, c := range cands {
		if c.IsDM {
			if participant[i] {
				out[i] = a.dmRow(ctx, c, peers[i], projectScope)
			}
			continue
		}
		out[i] = a.threadRow(c)
	}
	return out, nil
}

// threadRow is a thread candidate's row when its project exists, the
// caller may read it, and a token may read it under inbox:read
// (authorizeChatThreadRead and chatTokenMayUse).
func (a *starredConversationAuthorizer) threadRow(c StarredConversationCandidate) *starredConversationItem {
	project := a.projects[c.ProjectID]
	if project == nil || !a.projectRead[c.ProjectID] {
		return nil
	}
	if a.check != nil && !a.check.allows(c.ProjectID) {
		return nil
	}
	return &starredConversationItem{
		ConversationKey:  c.ConversationKey,
		ConversationKind: "thread",
		ConversationName: c.TopicName,
		LastActivityAt:   starActivityPtr(c.LastActivityAt),
		ProjectID:        project.ID,
		ProjectName:      project.Name,
	}
}

// dmRow is a DM candidate's row; the caller is a participant (checked by
// decide). A token needs, as chatTokenMayUse decides: for an agent DM the
// agent's project inside its boundary and agent:read on the agent; for a
// DM between users (or with an agent that no longer exists) a hub
// boundary. The row carries the agent's project when it still exists, as
// resolveStarDM stores and labels it.
func (a *starredConversationAuthorizer) dmRow(ctx context.Context, c StarredConversationCandidate, peer starDMPeerRef, projectScope string) *starredConversationItem {
	var agent *store.Agent
	if peer.kind == "agent" {
		agent = a.agents[peer.id]
	}
	if a.check != nil {
		tokenProject := ""
		if agent != nil {
			tokenProject = agent.ProjectID
		}
		if !a.check.allows(tokenProject) {
			return nil
		}
		if agent != nil && !a.tokenReadsAgent(ctx, agent) {
			return nil
		}
	}
	row := &starredConversationItem{
		ConversationKey:  c.ConversationKey,
		ConversationKind: "dm",
		LastActivityAt:   starActivityPtr(c.LastActivityAt),
	}
	var project *store.Project
	if agent != nil && agent.ProjectID != "" {
		project = a.projects[agent.ProjectID]
	}
	if project != nil {
		row.ProjectID = project.ID
		row.ProjectName = project.Name
	}
	if projectScope != "" && row.ProjectID != projectScope {
		return nil
	}
	switch {
	case !canonicalStarID(peer.id):
		row.ConversationName = starKindLabel(peer.kind)
	case peer.kind == "user":
		row.ConversationName = starUserLabel(a.users[peer.id])
	case peer.kind == "agent":
		row.ConversationName = starAgentLabel(agent)
	default:
		row.ConversationName = starLabelDM
	}
	return row
}

// starActivityPtr is t as an optional wire time (nil when zero).
func starActivityPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// readsProject reports the cached project read decision, deciding it
// when not yet known.
func (a *starredConversationAuthorizer) readsProject(ctx context.Context, projectID string) bool {
	a.decideProjectReads(ctx, []string{projectID})
	return a.projectRead[projectID]
}

// decideProjectReads decides ActionRead for every loaded project not yet
// decided, in one batch (the decision authorizeChatThreadRead makes per
// thread). A project that does not exist is never readable.
func (a *starredConversationAuthorizer) decideProjectReads(ctx context.Context, projectIDs []string) {
	var resources []Resource
	var ids []string
	for _, id := range dedupStrings(projectIDs) {
		if _, done := a.projectRead[id]; done {
			continue
		}
		project := a.projects[id]
		if project == nil || isNilIdentity(a.identity) || a.s.authzService == nil {
			a.projectRead[id] = false
			continue
		}
		resources = append(resources, projectResource(project))
		ids = append(ids, id)
	}
	if len(resources) == 0 {
		return
	}
	caps := a.s.authzService.ComputeCapabilitiesForActions(ctx, a.identity, resources, []Action{ActionRead})
	for i, id := range ids {
		a.projectRead[id] = i < len(caps) && capabilityAllows(caps[i], ActionRead)
	}
}

// tokenReadsAgent caches a token's agent:read decision on an agent.
func (a *starredConversationAuthorizer) tokenReadsAgent(ctx context.Context, agent *store.Agent) bool {
	ok, seen := a.agentRead[agent.ID]
	if !seen {
		ok = a.s.authzService != nil && a.s.authzService.CheckAccess(ctx, a.call.token, agentResource(agent), ActionRead).Allowed
		a.agentRead[agent.ID] = ok
	}
	return ok
}

// loadProjects loads the projects not yet loaded, in one query. IDs that
// are not canonical UUIDs cannot name a project and are recorded absent.
func (a *starredConversationAuthorizer) loadProjects(ctx context.Context, ids []string) error {
	var missing []string
	for _, id := range dedupStrings(ids) {
		if _, done := a.projects[id]; done {
			continue
		}
		if !canonicalStarID(id) {
			a.projects[id] = nil
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return nil
	}
	res, err := a.s.store.ListProjectSummaries(ctx, store.ProjectFilter{MemberProjectIDs: missing},
		store.ListOptions{Limit: len(missing), SkipTotalCount: true})
	if err != nil {
		return err
	}
	for _, id := range missing {
		a.projects[id] = nil
	}
	if res == nil {
		return errors.New("chat stars: project lookup returned no result")
	}
	for i := range res.Items {
		p := res.Items[i]
		a.projects[p.ID] = &p
	}
	return nil
}

// loadAgents loads the agents not yet loaded (soft-deleted included, as
// GetAgent and the DM rail read them), in one query.
func (a *starredConversationAuthorizer) loadAgents(ctx context.Context, ids []string) error {
	var missing []string
	for _, id := range dedupStrings(ids) {
		if _, done := a.agents[id]; !done {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	found, err := a.s.store.GetAgentsByIDsIncludingDeleted(ctx, missing)
	if err != nil {
		return err
	}
	for _, id := range missing {
		a.agents[id] = found[id]
	}
	return nil
}

// loadUsers loads the users not yet loaded, in one query.
func (a *starredConversationAuthorizer) loadUsers(ctx context.Context, ids []string) error {
	var missing []string
	for _, id := range dedupStrings(ids) {
		if _, done := a.users[id]; !done {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	found, err := a.s.store.GetUsersByIDs(ctx, missing)
	if err != nil {
		return err
	}
	for _, id := range missing {
		a.users[id] = found[id]
	}
	return nil
}
