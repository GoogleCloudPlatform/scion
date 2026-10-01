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
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
)

// P1b (ptone/scion#2383) implements sorted mode on the project agents
// endpoint only: sort=updated (both directions), fit/complete, stats=1, the
// candidate-count ceiling, and the v2 cursor. sort=created and the global
// endpoint's sorted mode are later phases (design lists-graph.md 11 P1b/P2).
// An agent-JWT caller supplying "sort" gets a 400 here, before any SQL; the
// agent-JWT sorted path itself ships in P2 (design 5.3 "P1b build").

// errCodeSortedViewUnavailable is the 422 error code for the sorted-mode
// candidate ceiling refusal (design lists-graph.md 4.6).
const errCodeSortedViewUnavailable = "sorted_view_unavailable"

// agentJWTSortedModeMessage is the exact 400 message a sorted-mode request
// from an agent JWT gets in P1b (design lists-graph.md 5.3 "P1b build"; S9
// matches on this string).
const agentJWTSortedModeMessage = "sorted mode is not yet available for agent tokens"

// writeSortedViewUnavailable writes the 422 refusal for the sorted-mode
// candidate ceiling (design lists-graph.md 4.6): the APIError envelope with
// code "sorted_view_unavailable" and details.reason "too_many_candidates".
func writeSortedViewUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusUnprocessableEntity, errCodeSortedViewUnavailable,
		"too many agents for a sorted view",
		map[string]interface{}{"reason": "too_many_candidates"})
}

// sortSuffix extends a cursor-binding endpoint string with the sort mode, so
// a cursor minted for one sort/dir cannot bind-match a request for another
// even if DecodeAgentCursor's own sort/dir fields were somehow bypassed
// (design lists-graph.md 4.4: "Sort and dir enter only through the endpoint
// string ... sortSuffix(e) = e in legacy mode, and e + "|sort=" + sort +
// "|dir=" + dir in sorted mode").
func sortSuffix(endpoint, sort, dir string) string {
	if sort == "" {
		return endpoint
	}
	return endpoint + "|sort=" + sort + "|dir=" + dir
}

// sortedProjectListParams is the parsed, validated query for a sorted-mode
// project agent list request.
type sortedProjectListParams struct {
	sort   string
	dir    string
	limit  int
	fit    int // 0 means "fit" was not supplied
	hasFit bool
	stats  bool
	cursor string
}

// parseSortedProjectListParams validates the sort-mode-specific parameters
// (design lists-graph.md 4.1). filter/limit parsing shared with legacy mode
// happens in the caller; this only validates sort, dir, fit, stats and the
// fit/cursor exclusion. It writes the 400 response itself on failure so
// callers can just check the returned ok.
func parseSortedProjectListParams(w http.ResponseWriter, query url.Values, limit int) (sortedProjectListParams, bool) {
	p := sortedProjectListParams{sort: query.Get("sort"), limit: limit}

	p.dir = query.Get("dir")
	if p.dir == "" {
		p.dir = agentsort.Desc
	}
	if p.dir != agentsort.Asc && p.dir != agentsort.Desc {
		BadRequest(w, "invalid dir")
		return p, false
	}

	// P1b implements sort=updated only on the project endpoint; sort=created
	// is P2 (design 11 P1b/P2). Both remain valid *values* of the sort
	// parameter per the final contract (4.1), so a request for "created"
	// gets the same "not yet available" shape as an unrecognized value would
	// under the final contract's "any other value returns 400" rule, rather
	// than a confusing partial 200.
	if p.sort != agentsort.Updated {
		BadRequest(w, "invalid sort")
		return p, false
	}

	p.cursor = query.Get("cursor")
	if fitStr := query.Get("fit"); fitStr != "" {
		if p.cursor != "" {
			BadRequest(w, "fit is not valid together with cursor")
			return p, false
		}
		parsed, err := strconv.Atoi(fitStr)
		if err != nil || parsed < 1 || parsed > 500 {
			BadRequest(w, "invalid fit")
			return p, false
		}
		if parsed < p.limit {
			BadRequest(w, "fit must be at least limit")
			return p, false
		}
		p.fit = parsed
		p.hasFit = true
	}

	p.stats = query.Get("stats") == "1"
	return p, true
}

// memberResource builds the authorization Resource for a member row via the
// one construction path design lists-graph.md 5.1/5.3 requires:
// memberResource(m) = agentResource(m.ToAgent()). Comparing this against
// agentResource(full) with resourceEqual is the whole basis of the step 5a
// race check, and the non-waivable S6 gate exists to prove this equality
// holds for every row when nothing raced.
func memberResource(m store.AgentMember) Resource {
	return agentResource(m.ToAgent())
}

// resourceEqual is a deep equality over the fields of Resource that
// authorization actually reads for an agent, normalizing a nil and an empty
// Labels map or Ancestry slice as equal (design 5.3 step 5a, r8 NB-2): a
// difference in how the narrow member decoder and the full-row decoder
// represent "no labels" or "no ancestry" must never by itself trigger a
// re-decision.
func resourceEqual(a, b Resource) bool {
	if a.Type != b.Type || a.ID != b.ID || a.OwnerID != b.OwnerID ||
		a.ParentType != b.ParentType || a.ParentID != b.ParentID ||
		a.ScopeKind != b.ScopeKind {
		return false
	}
	if !stringMapEqualNormalized(a.Labels, b.Labels) {
		return false
	}
	if !stringSliceEqualNormalized(a.Ancestry, b.Ancestry) {
		return false
	}
	return true
}

func stringMapEqualNormalized(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func stringSliceEqualNormalized(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// agentMatchesMemberFilter re-applies filter's non-phase predicates to a
// freshly re-read full agent row (design lists-graph.md 5.3 step 5a, r8 F-1
// and NB-3): if the full row no longer matches the request's own filter —
// most concretely, its labels changed so a label=value filter no longer
// holds, or (belt-and-suspenders alongside the explicit ProjectID check the
// caller performs separately) it no longer belongs to the request's project
// — the row is dropped at no decision cost. Phase is deliberately not
// rechecked here: a complete response is unphased, and the paged phase
// filter is applied to the member snapshot, which carries the same
// staleness a single read has today.
func agentMatchesMemberFilter(a *store.Agent, filter store.AgentFilter) bool {
	if filter.ProjectID != "" && a.ProjectID != filter.ProjectID {
		return false
	}
	if filter.RuntimeBrokerID != "" && a.RuntimeBrokerID != filter.RuntimeBrokerID {
		return false
	}
	if filter.RequestedOwnerID != "" && a.OwnerID != filter.RequestedOwnerID {
		return false
	}
	if filter.AncestorID != "" && !containsString(a.Ancestry, filter.AncestorID) {
		return false
	}
	for k, v := range filter.Labels {
		if a.Labels[k] != v {
			return false
		}
	}
	if filter.IDs != nil {
		if len(filter.IDs) == 0 || !containsString(filter.IDs, a.ID) {
			return false
		}
	}
	if filter.LineageRootID != "" {
		if a.ID != filter.LineageRootID && !containsString(a.Ancestry, filter.LineageRootID) {
			return false
		}
	}
	if !filter.IncludeDeleted && !a.DeletedAt.IsZero() {
		return false
	}
	return true
}

// memberKeyOf returns the agentsort.Row for m under sort.
func memberKeyOf(sort string, m store.AgentMember) agentsort.Row {
	return agentsort.KeyFor(sort, m.ID, m.Created, m.Updated, m.LastActivityEvent)
}

// positionAfterCursor returns the index of the first member in members
// (already sorted per agentsort for (sort,dir)) that sorts strictly after
// cur, i.e. len(members) if every member is at or before cur.
func positionAfterCursor(sortKey, dir string, members []store.AgentMember, cur store.AgentCursor) int {
	curRow := agentsort.Row{K: cur.K, Created: cur.Created, ID: cur.ID}
	for i, m := range members {
		row := memberKeyOf(sortKey, m)
		// The first row that is NOT before-or-equal to cur in the walk
		// order, i.e. the first row that sorts strictly after cur.
		if agentsort.Less(sortKey, dir, curRow, row) {
			return i
		}
	}
	return len(members)
}

// buildAgentStats computes the design lists-graph.md 4.6 "stats" block over
// members: total and running counts, plus the full [id,phase] population
// (never omitted on the project endpoint, which the 2,000 candidate ceiling
// already bounds).
func buildAgentStats(members []store.AgentMember) *ListAgentsStats {
	stats := &ListAgentsStats{Agents: make([][2]string, 0, len(members))}
	for _, m := range members {
		stats.Total++
		if m.Phase == "running" {
			stats.Running++
		}
		stats.Agents = append(stats.Agents, [2]string{m.ID, m.Phase})
	}
	return stats
}

// filterMembersByPhase returns the subset of members matching phase, or
// members unchanged when phase is empty.
func filterMembersByPhase(members []store.AgentMember, phase string) []store.AgentMember {
	if phase == "" {
		return members
	}
	out := make([]store.AgentMember, 0, len(members))
	for _, m := range members {
		if m.Phase == phase {
			out = append(out, m)
		}
	}
	return out
}

// listProjectAgentsSorted implements the project endpoint's sorted mode
// (design lists-graph.md 5.3, user path only — the agent-JWT sorted path is
// P2). The agent.list gate has already run in the caller.
func (s *Server) listProjectAgentsSorted(w http.ResponseWriter, r *http.Request, projectID string, query url.Values, filter store.AgentFilter, p sortedProjectListParams) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)

	binding := scopedCursorBinding(sortSuffix("project-agents:"+projectID, p.sort, p.dir), filter, identity)

	var cur *store.AgentCursor
	if p.cursor != "" {
		decoded, err := store.DecodeAgentCursor(p.cursor, p.sort, p.dir, binding)
		if err != nil {
			BadRequest(w, "invalid cursor")
			return
		}
		cur = &decoded
	}

	// Step 0: candidate ceiling pre-check (design 5.3 step 0; Q-C). The
	// member filter is the request filter with Phase cleared (R2-B4): the
	// ceiling, completeness and stats are all decided on the unphased
	// candidate set.
	memberFilter := filter
	memberFilter.Phase = ""

	n, err := s.store.CountAgents(ctx, memberFilter)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if n > authorizedListMaxCandidates {
		writeSortedViewUnavailable(w)
		return
	}

	// Step 1: member read, max = ceiling+1 so a candidate pool that grew
	// between the COUNT and this read is still caught (design 5.3 step 1).
	members, err := s.store.ListAgentMembers(ctx, memberFilter, p.sort, p.dir, authorizedListMaxCandidates+1)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if len(members) > authorizedListMaxCandidates {
		writeSortedViewUnavailable(w)
		return
	}
	n = len(members)

	// Step 2: completeness is decided on the candidate count n, before the
	// read pass (design 5.3 step 2; Q-G).
	complete := p.hasFit && n <= p.fit

	// Step 3: the thin ActionRead-only read pass over every candidate
	// (design 5.3 step 3; security Q-B).
	resources := make([]Resource, len(members))
	for i, m := range members {
		resources[i] = memberResource(m)
	}
	readCaps := s.authzService.ComputeCapabilitiesForActions(ctx, identity, resources, []Action{ActionRead})

	readable := make([]store.AgentMember, 0, len(members))
	readableReadCaps := make([]*Capabilities, 0, len(members))
	for i, m := range members {
		if capabilityAllows(readCaps[i], ActionRead) {
			readable = append(readable, m)
			readableReadCaps = append(readableReadCaps, readCaps[i])
		}
	}

	// Step 4: stats, computed from the readable set (design 5.3 step 4).
	var statsResp *ListAgentsStats
	if p.stats {
		statsResp = buildAgentStats(readable)
	}

	var page []store.AgentMember
	var pageReadCaps []*Capabilities
	var totalCount int
	var nextCursor string

	if complete {
		// Step 5, complete branch: the whole unphased readable set, in
		// section-4.2 order (ListAgentMembers already returned it sorted).
		page = readable
		pageReadCaps = readableReadCaps
		totalCount = len(readable)
	} else {
		r := filterMembersByPhase(readable, filter.Phase)
		totalCount = len(r)
		start := 0
		if cur != nil {
			start = positionAfterCursor(p.sort, p.dir, r, *cur)
		}
		end := start + p.limit
		if end > len(r) {
			end = len(r)
		}
		if start < len(r) {
			page = r[start:end]
		}
		// Recover the read caps for the sliced page items by ID: readable
		// and readableReadCaps share an index, but r (the phase-filtered
		// subset) does not.
		byID := make(map[string]*Capabilities, len(readable))
		for i, m := range readable {
			byID[m.ID] = readableReadCaps[i]
		}
		pageReadCaps = make([]*Capabilities, len(page))
		for i, m := range page {
			pageReadCaps[i] = byID[m.ID]
		}
		if end < len(r) && len(page) > 0 {
			last := page[len(page)-1]
			lastRow := memberKeyOf(p.sort, last)
			nextCursor = store.EncodeAgentCursor(p.sort, p.dir, lastRow.K, lastRow.Created, last.ID, binding)
		}
	}

	// Step 5a + step 6: resolve full rows for the page, apply the race rule,
	// and compute the remaining 7 actions (design 5.3 step 5a/6).
	ids := make([]string, len(page))
	for i, m := range page {
		ids[i] = m.ID
	}
	fullRows, err := s.store.GetAgentsByIDs(ctx, ids)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	allAgentActions := ResourceActions["agent"]
	remainingActions := make([]Action, 0, len(allAgentActions))
	for _, action := range allAgentActions {
		if action != ActionRead {
			remainingActions = append(remainingActions, action)
		}
	}

	agents := make([]AgentWithCapabilities, 0, len(page))
	plainAgents := make([]store.Agent, 0, len(page))
	for i, m := range page {
		full, ok := fullRows[m.ID]
		if !ok {
			continue // deleted between the two reads: dropped (design 5.3 step 5a)
		}
		if full.ProjectID != projectID { // r8 NB-3, both paths
			continue
		}
		if !agentMatchesMemberFilter(full, filter) { // r8 F-1
			continue
		}

		fullRes := agentResource(full)
		memberRes := memberResource(m)

		var finalCap *Capabilities
		if !resourceEqual(fullRes, memberRes) {
			// Race: authorization inputs changed. Re-run the read decision
			// and the remaining actions on the full row's Resource,
			// fail-closed (design 5.3 step 5a).
			redecided := s.authzService.ComputeCapabilitiesForActions(ctx, identity, []Resource{fullRes}, allAgentActions)[0]
			if !capabilityAllows(redecided, ActionRead) {
				continue // no longer readable
			}
			finalCap = redecided
		} else {
			restCaps := s.authzService.ComputeCapabilitiesForActions(ctx, identity, []Resource{fullRes}, remainingActions)[0]
			finalCap = mergeCapabilities(allAgentActions, pageReadCaps[i], restCaps)
		}

		item := *full
		agents = append(agents, AgentWithCapabilities{Agent: item, Cap: finalCap})
		plainAgents = append(plainAgents, item)
	}

	s.enrichAgents(ctx, plainAgents)
	for i := range agents {
		agents[i].Agent = plainAgents[i]
		agents[i].Agent.AppliedConfig = redactAppliedConfigEnvForResponse(plainAgents[i].AppliedConfig, capabilityAllows(agents[i].Cap, ActionAttach))
	}

	// A complete response IS the whole set: a step 5a drop (missing row,
	// project/filter mismatch, or no-longer-readable race) must be reflected
	// in totalCount, not just in the item count (design 5.3: "a complete
	// response simply has fewer items (totalCount = len(page))"). A paged
	// response's totalCount is the phase-filtered readable count across the
	// whole walk, not just this page, so a short page from step 5a drops
	// does not change it (design: "A short page is valid").
	if complete {
		totalCount = len(agents)
	}

	scopeCap := s.authzService.ComputeScopeCapabilities(ctx, identity, "project", projectID, "agent")

	resp := ListAgentsResponse{
		Agents:       agents,
		NextCursor:   nextCursor,
		TotalCount:   totalCount,
		Sort:         p.sort,
		Dir:          p.dir,
		Stats:        statsResp,
		ServerTime:   time.Now().UTC(),
		Capabilities: scopeCap,
	}
	if p.hasFit {
		c := complete
		resp.Complete = &c
	}
	writeJSON(w, http.StatusOK, resp)
}

// isSortedModeRequest reports whether query requests sorted mode (design
// lists-graph.md 4.1: "sort ... absent (legacy)").
func isSortedModeRequest(query url.Values) bool {
	return query.Get("sort") != ""
}

// rejectAgentJWTSortedMode writes the P1b 400 for a sorted-mode request from
// an agent JWT, before any SQL runs (design 5.3 "P1b build"; S9 matches this
// exact message).
func rejectAgentJWTSortedMode(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, agentJWTSortedModeMessage, nil)
}
