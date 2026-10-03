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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// listAgentsSorted implements the global agents endpoint's sorted mode:
// pure SQL, because the SQL scope predicate already baked into filter by
// the caller IS the authorization (AuthorizedProjectIDs and the
// classification fields). There is no per-item read filter, unlike the
// project endpoint's user path, so returned rows go through the same
// buildGlobalAgentPage as the legacy branch, with no narrow AgentMember
// projection and no race/re-decision machinery.
func (s *Server) listAgentsSorted(w http.ResponseWriter, r *http.Request, filter store.AgentFilter, p agentListParams, identity Identity) {
	ctx := r.Context()

	binding := scopedCursorBinding(sortSuffix("agents", p.sort, p.dir), filter, identity)

	var cur *store.AgentCursor
	if p.cursor != "" {
		decoded, err := store.DecodeAgentCursor(p.cursor, p.sort, p.dir, binding)
		if err != nil {
			BadRequest(w, "invalid cursor")
			return
		}
		cur = &decoded
	}

	// statsFilter is the request filter with Phase cleared: the fit probe
	// and the stats population are both decided on the unphased candidate
	// set.
	statsFilter := filter
	statsFilter.Phase = ""

	var (
		items      []store.Agent
		totalCount int
		nextCursor string
		complete   bool
	)

	if p.hasFit {
		// fit (race-free): the store's own limit+1 probe says whether
		// more rows exist. No decision is made on any row yet.
		result, err := s.store.ListAgents(ctx, statsFilter, store.ListOptions{
			Limit: p.fit, SortBy: p.sort, SortDir: p.dir, SkipTotalCount: true,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if result.NextCursor == "" {
			// Complete: the candidate set fit. Caps and messageability for
			// every row, exactly as today's load.
			items = result.Items
			complete = true
			totalCount = len(items)
		}
		// Otherwise: the candidate set did not fit. Discard these rows (no
		// decision has been made on any of them) and fall through to the
		// paged branch below.
	}

	if !complete {
		// Paged: filter (phase applied) in sorted order, keyset
		// after cur. totalCount is the COUNT of filter, phase applied,
		// which is exactly what ListAgents' own COUNT (driven
		// by filter) already computes when SkipTotalCount is false.
		result, err := s.store.ListAgents(ctx, filter, store.ListOptions{
			Limit: p.limit, SortBy: p.sort, SortDir: p.dir,
			SortCursor: cur, CursorBinding: binding,
		})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		items = result.Items
		totalCount = result.TotalCount
		nextCursor = result.NextCursor
	}

	// stats is read before any decision is made, so a stats read error
	// costs no decisions.
	var statsResp *ListAgentsStats
	if p.stats {
		var err error
		statsResp, err = buildGlobalAgentStats(ctx, s, statsFilter)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	agents, scopeCap := s.buildGlobalAgentPage(ctx, identity, items)

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

// buildGlobalAgentPage turns the authorized store rows of a global agents
// list page into the response items and scope capabilities. Both the legacy
// and the sorted branch of the global endpoint call it, so enrichment,
// capability computation, env redaction and messageability stay identical
// between them. Enrichment runs only after the authorized store result is
// obtained.
func (s *Server) buildGlobalAgentPage(ctx context.Context, identity Identity, items []store.Agent) ([]AgentWithCapabilities, *Capabilities) {
	s.enrichAgents(ctx, items)

	agents := make([]AgentWithCapabilities, 0, len(items))
	resources := make([]Resource, len(items))
	for i := range items {
		resources[i] = agentResource(&items[i])
	}
	for i, cap := range s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "agent") {
		item := items[i]
		item.AppliedConfig = redactAppliedConfigEnvForResponse(item.AppliedConfig, s.envViewAllowed(ctx, identity, &item, cap))
		agents = append(agents, AgentWithCapabilities{Agent: item, Cap: cap})
	}

	// Messageability for each agent relative to the viewer.
	for i := range agents {
		agents[i].Messageability = s.ComputeMessageability(ctx, identity, &agents[i].Agent)
	}

	scopeCap := s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "agent")
	s.addAgentCreateIfAnyProjectAllows(ctx, identity, scopeCap)
	return agents, scopeCap
}

// globalAgentStatsCap is the global endpoint's "stats.agents" omission
// threshold: above this many agents the per-agent [id,phase] population
// would grow unbounded with the agent count (2.75MB at 50k), so only the
// counts are sent.
const globalAgentStatsCap = 2000

// buildGlobalAgentStats computes the "stats" block for the global
// endpoint via CountAgentsByPhaseIDs (a SQL read with no decision made).
// Unlike the project endpoint's buildAgentStats (which reads the already
// in-memory, read-filtered member set), this is a dedicated store read: the
// global endpoint's stats population has no read filter to piggyback on.
// The [id, phase] list is only built when it will be sent.
func buildGlobalAgentStats(ctx context.Context, s *Server, statsFilter store.AgentFilter) (*ListAgentsStats, error) {
	idPhases, err := s.store.CountAgentsByPhaseIDs(ctx, statsFilter)
	if err != nil {
		return nil, err
	}
	stats := &ListAgentsStats{Total: len(idPhases)}
	for _, ip := range idPhases {
		if ip.Phase == "running" {
			stats.Running++
		}
	}
	if stats.Total <= globalAgentStatsCap {
		agentsOut := make([][2]string, len(idPhases))
		for i, ip := range idPhases {
			agentsOut[i] = [2]string{ip.ID, ip.Phase}
		}
		stats.Agents = &agentsOut
	}
	return stats, nil
}

// sortedShortCircuitResponse builds the empty-list short-circuit response
// for an unauthenticated caller or a None-scope result. p holds the
// already-validated sorted parameters, or is the zero value for a request
// without sort, in which case the response is the legacy empty list. With
// sort, the response echoes sort/dir, reports complete: true when fit was
// supplied, and reports stats: {total: 0, running: 0, agents: []} when
// stats=1.
func sortedShortCircuitResponse(p agentListParams) ListAgentsResponse {
	resp := ListAgentsResponse{
		Agents:     []AgentWithCapabilities{},
		TotalCount: 0,
		ServerTime: time.Now().UTC(),
	}
	if p.sort != "" {
		resp.Sort = p.sort
		resp.Dir = p.dir
		if p.hasFit {
			c := true
			resp.Complete = &c
		}
		if p.stats {
			empty := [][2]string{}
			resp.Stats = &ListAgentsStats{Agents: &empty}
		}
	}
	return resp
}
