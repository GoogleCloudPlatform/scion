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
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
)

// sortedProjectDecisionCeiling is the security gate for a sorted-mode
// project agents request: no single request may cost more than this many
// authorization decisions (the race exception adds up to 500 more).
// effectivePagedPageSize enforces this for the paged branch.
const sortedProjectDecisionCeiling = 4005

// effectivePagedPageSize keeps the paged branch inside
// sortedProjectDecisionCeiling at every legal (limit, n) pair even with no
// race involved: the sorted project endpoint's paged branch costs
// 5 + n + 7*pageSize decisions, which breaches the ceiling on its own at
// legal (limit, n) pairs -- e.g. limit=500 at n=2,000 costs 5,505 unraced.
// P_eff = min(limit, floor((4000-n)/7)) keeps every paged request within
// 4,005 unraced (4,504 raced) for every n up to the 2,000 candidate
// ceiling, where n is the member-read count (len(members), already
// capped at authorizedListMaxCandidates by the time this is called), not
// the ceiling pre-check COUNT. At n<=500, P_eff==limit (up to 500, the
// size before this page-size clamp was added); at n=2,000, P_eff<=285.
// P_eff is NOT part of the cursor binding, so a later page computing a
// different P_eff (n having changed) does not invalidate the cursor --
// only the position within the walk is bound, never the page size.
func effectivePagedPageSize(limit, n int) int {
	maxP := (sortedProjectDecisionCeiling - 5 - n) / 7
	if maxP < limit {
		return maxP
	}
	return limit
}

// This file implements sorted mode on the project agents endpoint: both
// sort keys and both directions, fit/complete,
// stats=1, the candidate-count ceiling, the v2 cursor, and both the user
// path (per-item read filter, this file) and the agent-JWT path (no read
// filter, agent_sorted_project_list_jwt.go).

// errCodeSortedViewUnavailable is the 422 error code for the sorted-mode
// candidate ceiling refusal.
const errCodeSortedViewUnavailable = "sorted_view_unavailable"

// writeSortedViewUnavailable writes the 422 refusal for the sorted-mode
// candidate ceiling: the APIError envelope with
// code "sorted_view_unavailable" and details.reason "too_many_candidates".
func writeSortedViewUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusUnprocessableEntity, errCodeSortedViewUnavailable,
		"too many agents for a sorted view",
		map[string]interface{}{"reason": "too_many_candidates"})
}

// sortSuffix extends a cursor-binding endpoint string with the sort mode, so
// a cursor minted for one sort/dir cannot bind-match a request for another
// even if DecodeAgentCursor's own sort/dir fields were somehow bypassed.
// Sort and dir enter the binding only through the endpoint string: it is
// the endpoint unchanged in legacy mode, and endpoint + "|sort=" + sort +
// "|dir=" + dir in sorted mode.
func sortSuffix(endpoint, sort, dir string) string {
	if sort == "" {
		return endpoint
	}
	return endpoint + "|sort=" + sort + "|dir=" + dir
}

// sortedProjectCandidates resolves the sorted-mode candidate set for the
// project endpoint (the candidate ceiling check, the member read, and
// candidate-count completeness), shared by the user
// path (listProjectAgentsSorted) and the agent-JWT path
// (listProjectAgentsSortedAgentJWT): the COUNT-first ceiling pre-check, the
// bounded member read, and candidate-count completeness. On refusal (the 422)
// or a store error, it writes the response itself and returns ok=false, so
// callers only need to check ok.
func (s *Server) sortedProjectCandidates(w http.ResponseWriter, ctx context.Context, memberFilter store.AgentFilter, p agentListParams) (members []store.AgentMember, complete bool, ok bool) {
	// Candidate ceiling pre-check.
	n, err := s.store.CountAgents(ctx, memberFilter)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return nil, false, false
	}
	if n > authorizedListMaxCandidates {
		writeSortedViewUnavailable(w)
		return nil, false, false
	}

	// Member read, max = ceiling+1 so a candidate pool that grew
	// between the COUNT and this read is still caught.
	members, err = s.store.ListAgentMembers(ctx, memberFilter, p.sort, p.dir, authorizedListMaxCandidates+1)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return nil, false, false
	}
	if len(members) > authorizedListMaxCandidates {
		writeSortedViewUnavailable(w)
		return nil, false, false
	}

	// Completeness is decided on the candidate count, before the
	// read pass.
	complete = p.hasFit && len(members) <= p.fit
	return members, complete, true
}

// memberResource builds the authorization Resource for a member row via the
// one construction path: memberResource(m) = agentResource(m.ToAgent()).
// Comparing this against agentResource(full) with resourceEqual is the whole
// basis of the race re-check after the full-row read, and the decision-count
// test suite's non-waivable deep-equality gate exists to prove this equality
// holds for every row when nothing raced.
func memberResource(m store.AgentMember) Resource {
	return agentResource(m.ToAgent())
}

// resourceEqual is a whole-Resource deep equality: comparing whole Resources
// rather than a field list tracks any future input automatically. It
// normalizes a nil and an empty Labels map or Ancestry slice as equal first:
// a difference in how the narrow member decoder and the full-row decoder
// represent "no labels" or "no ancestry" must never by itself trigger a
// re-decision. Deliberately NOT a hand-written field list: a field this
// function doesn't know about (such as a new Resource input added later that
// nothing compares) would let the race re-check silently skip a re-decision
// on a raced row — exactly the TOCTOU this gate exists to close.
// TestResourceEqual_MutationCoversEveryField asserts every exported Resource
// field is covered by reflection-filling and mutating it.
func resourceEqual(a, b Resource) bool {
	return reflect.DeepEqual(normalizeResourceForCompare(a), normalizeResourceForCompare(b))
}

// normalizeResourceForCompare returns a copy of r with a nil Labels or
// Ancestry replaced by an empty (non-nil) value of the same type, so
// resourceEqual's reflect.DeepEqual treats "no labels"/"no ancestry" the
// same way regardless of which decoder produced it.
func normalizeResourceForCompare(r Resource) Resource {
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	if r.Ancestry == nil {
		r.Ancestry = []string{}
	}
	return r
}

// loadFullRowsForPage fetches the full store.Agent rows for the page items,
// honoring filter.IncludeDeleted. GetAgentsByIDs hard-codes
// agent.DeletedAtIsNil(), so a soft-deleted row would be indistinguishable
// from "deleted between the two reads" even when the caller asked for it
// with includeDeleted=true -- while CountAgents, ListAgentMembers and stats
// already honor it. Using ListAgents with an IDs-only filter instead goes
// through the same agentFilterPredicates the rest of this request already
// uses, so IncludeDeleted (and everything else) is applied uniformly.
func (s *Server) loadFullRowsForPage(ctx context.Context, ids []string, includeDeleted bool) (map[string]*store.Agent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// The store clamps a ListAgents read to its own page cap, so a request
	// for more ids than maxSortedLimit could come back short without any
	// error, and the missing rows would look like rows deleted between the
	// two reads. No caller can exceed the bound today; fail closed before
	// any read if one ever does. A short read within the bound is still the
	// legitimate race-drop signal and is not an error.
	if len(ids) > maxSortedLimit {
		return nil, fmt.Errorf("sorted full-row read of %d ids exceeds the %d-row page bound", len(ids), maxSortedLimit)
	}
	result, err := s.store.ListAgents(ctx, store.AgentFilter{IDs: ids, IncludeDeleted: includeDeleted},
		store.ListOptions{Limit: len(ids), SkipTotalCount: true})
	if err != nil {
		return nil, err
	}
	out := make(map[string]*store.Agent, len(result.Items))
	for i := range result.Items {
		out[result.Items[i].ID] = &result.Items[i]
	}
	return out, nil
}

// recheckStillMatchesFilter re-applies memberFilter (the request filter with
// Phase cleared) to the page's freshly re-read rows by asking the store the
// same question agentFilterPredicates already answers for the rest of this
// request, rather than hand-duplicating its logic here, so the re-check
// uses the same matcher as the store predicate. A hand-written
// duplicate would drift as store.AgentFilter grows; asking the store
// directly cannot. memberFilter.ProjectID is already the request project,
// so a row that moved to another project between the two reads is excluded
// here too, with no separate check needed. Phase is deliberately not
// rechecked: a complete response is unphased, and the paged phase filter is
// applied to the member snapshot, which carries the same staleness a single
// read has today.
func (s *Server) recheckStillMatchesFilter(ctx context.Context, memberFilter store.AgentFilter, candidateIDs []string, sortKey, dir string) (map[string]bool, error) {
	if len(candidateIDs) == 0 {
		return nil, nil
	}
	recheckFilter := memberFilter
	recheckFilter.IDs = candidateIDs
	matched, err := s.store.ListAgentMembers(ctx, recheckFilter, sortKey, dir, len(candidateIDs))
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(matched))
	for _, m := range matched {
		out[m.ID] = true
	}
	return out, nil
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
		if agentsort.Less(dir, curRow, row) {
			return i
		}
	}
	return len(members)
}

// buildAgentStats computes the sorted-mode "stats" block over
// members: total and running counts, plus the full [id,phase] population
// (never omitted on the project endpoint, which the 2,000 candidate ceiling
// already bounds).
func buildAgentStats(members []store.AgentMember) *ListAgentsStats {
	agents := make([][2]string, 0, len(members))
	stats := &ListAgentsStats{}
	for _, m := range members {
		stats.Total++
		if m.Phase == "running" {
			stats.Running++
		}
		agents = append(agents, [2]string{m.ID, m.Phase})
	}
	stats.Agents = &agents
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

// listProjectAgentsSorted implements the project endpoint's sorted mode user
// path (per-item read filter, kept exactly as the legacy user path has it).
//
// Agent-list rule (ptone/scion#3346): for a user caller, an agent appears
// in an agent list, its pages and its totalCount only if the caller can
// read that agent. listAgents and listProjectAgents both apply it, so the
// two endpoints return the same set for the same project. Here the read
// pass below covers every candidate, so totalCount and stats are the
// readable counts.
// The agent-JWT path is listProjectAgentsSortedAgentJWT. The agent.list gate has
// already run in the caller.
func (s *Server) listProjectAgentsSorted(w http.ResponseWriter, r *http.Request, projectID string, filter store.AgentFilter, p agentListParams) {
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

	// Candidate ceiling, member read and candidate-count completeness,
	// shared with the agent-JWT path.
	// The member filter is the request filter with Phase cleared: the
	// ceiling, completeness and stats are all decided on the unphased
	// candidate set.
	memberFilter := filter
	memberFilter.Phase = ""
	members, complete, ok := s.sortedProjectCandidates(w, ctx, memberFilter, p)
	if !ok {
		return
	}
	n := len(members)

	// The thin ActionRead-only read pass over every candidate, decided as
	// agent-list row reads (AuthorizeListReadBatch). Its result is also
	// the row's read capability, except for a scoped token without
	// agent:read, whose read capability is recomputed for the page rows
	// below.
	resources := make([]Resource, len(members))
	for i, m := range members {
		resources[i] = memberResource(m)
	}
	listed, err := s.authzService.AuthorizeListReadBatch(ctx, identity, resources)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	// Only a scoped token without agent.read in its ceiling can be listed
	// a row it cannot read (listRowReadCeiling); for every other caller
	// the list read is the plain read decision.
	scopedToken := false
	if scoped, ok := identity.(*ScopedUserIdentity); ok && scoped != nil {
		scopedToken = !scoped.Ceiling().Allows("agent.read")
	}

	readable := make([]store.AgentMember, 0, len(members))
	readableReadCaps := make([]*Capabilities, 0, len(members))
	for i, m := range members {
		if listed[i] {
			readable = append(readable, m)
			readableReadCaps = append(readableReadCaps, &Capabilities{Actions: []string{string(ActionRead)}})
		}
	}

	// Stats, computed from the readable set.
	var statsResp *ListAgentsStats
	if p.stats {
		statsResp = buildAgentStats(readable)
	}

	var page []store.AgentMember
	var pageReadCaps []*Capabilities
	var totalCount int
	var nextCursor string

	if complete {
		// Complete branch: the whole unphased readable set, in sorted-mode
		// order (ListAgentMembers already returned it sorted).
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
		// The paged branch's page size is bounded by n (this request's
		// member-read count, i.e. len(members) above -- not the ceiling
		// pre-check COUNT, which can be lower if the pool grew in between),
		// not just the request's limit, so the per-request decision cost
		// 5+n+7*pageSize never exceeds the decision ceiling. pEff deliberately
		// does not enter the cursor binding (binding, above, is built before
		// pEff exists): n can differ from one page to the next without
		// invalidating a cursor.
		pEff := effectivePagedPageSize(p.limit, n)
		end := start + pEff
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

	// Resolve full rows for the page, apply the race rule, and compute the
	// remaining 7 actions.
	ids := make([]string, len(page))
	for i, m := range page {
		ids[i] = m.ID
	}
	fullRows, err := s.loadFullRowsForPage(ctx, ids, filter.IncludeDeleted)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	stillMatches, err := s.recheckStillMatchesFilter(ctx, memberFilter, ids, p.sort, p.dir)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if scopedToken && len(page) > 0 {
		// The row is listed, but its read capability is the plain read
		// decision, which a token without agent:read does not pass.
		pageResources := make([]Resource, len(page))
		for i, m := range page {
			pageResources[i] = memberResource(m)
		}
		pageReadCaps = s.authzService.ComputeCapabilitiesForActions(ctx, identity, pageResources, []Action{ActionRead})
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
			continue // deleted between the two reads: dropped
		}
		// An explicit, single-field check kept alongside the store-driven
		// recheck below as deliberate belt-and-suspenders -- it also still
		// fires if a full row were ever fetched by a path that does not
		// itself filter by project.
		if full.ProjectID != projectID {
			continue
		}
		if !stillMatches[m.ID] { // no longer matches the request's own (non-phase) filter
			continue
		}

		fullRes := agentResource(full)
		memberRes := memberResource(m)

		var finalCap *Capabilities
		if !resourceEqual(fullRes, memberRes) {
			// Race: authorization inputs changed. Re-run the read decision
			// and the remaining actions on the full row's Resource,
			// fail-closed.
			redecided := s.authzService.ComputeCapabilitiesForActions(ctx, identity, []Resource{fullRes}, allAgentActions)[0]
			if !capabilityAllows(redecided, ActionRead) {
				if !scopedToken {
					continue // no longer readable
				}
				relisted, err := s.authzService.AuthorizeListReadBatch(ctx, identity, []Resource{fullRes})
				if err != nil || !relisted[0] {
					continue // no longer listed
				}
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
		agents[i].AppliedConfig = redactAppliedConfigEnvForResponse(plainAgents[i].AppliedConfig, capabilityAllows(agents[i].Cap, ActionAttach))
	}

	// A complete response IS the whole set: a race drop (missing row,
	// project/filter mismatch, or no-longer-readable race) must be reflected
	// in totalCount, not just in the item count, so a complete response
	// simply has fewer items (totalCount = len(page)). A paged response's
	// totalCount is the phase-filtered readable count across the whole walk,
	// not just this page, so a short page from race drops does not change
	// it: a short page is valid.
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
	writeAgentList(w, p.view, resp)
}

// isSortedModeRequest reports whether query requests sorted mode: any
// non-empty sort parameter. Without sort, the request is legacy mode.
func isSortedModeRequest(query url.Values) bool {
	return query.Get("sort") != ""
}
