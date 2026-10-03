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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// listProjectAgentsSortedAgentJWT implements the project endpoint's
// sorted-mode agent-JWT path.
// Unlike the user path (listProjectAgentsSorted) there is no per-item
// read filter — an agent JWT reads every member of its own project today,
// unfiltered — so every member is "readable", there is no read pass, and
// the capability pass is one ComputeCapabilitiesBatch call over the page's
// full rows, exactly like the legacy agent-JWT branch. The candidate
// ceiling, the member read, candidate-count completeness and the
// project/filter re-check after the full-row read are shared with the user
// path via sortedProjectCandidates/loadFullRowsForPage/
// recheckStillMatchesFilter. The agent.list gate does not apply to agent
// JWTs (the caller has already checked agentIdent.ProjectID() ==
// projectID); the only race drop this path applies is a missing row or a
// row that now fails the project or filter check, on the user path and on
// the agent-JWT path alike. There is no resourceEqual race re-decision
// here, because there is no read decision to race against: a changed row
// that still passes gets its capabilities from the full row, with no extra
// decisions.
func (s *Server) listProjectAgentsSortedAgentJWT(w http.ResponseWriter, r *http.Request, projectID string, filter store.AgentFilter, p agentListParams) {
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

	memberFilter := filter
	memberFilter.Phase = ""
	members, complete, ok := s.sortedProjectCandidates(w, ctx, memberFilter, p)
	if !ok {
		return
	}

	// Stats: there is no read filter, so every member is readable,
	// and stats is computed over the unphased candidate set.
	var statsResp *ListAgentsStats
	if p.stats {
		statsResp = buildAgentStats(members)
	}

	var page []store.AgentMember
	var totalCount int
	var nextCursor string

	if complete {
		page = members
		totalCount = len(members)
	} else {
		r := filterMembersByPhase(members, filter.Phase)
		totalCount = len(r)
		start := 0
		if cur != nil {
			start = positionAfterCursor(p.sort, p.dir, r, *cur)
		}
		// No page-size clamp on the agent-JWT path: its paged cost has no
		// term that grows with the candidate count n, unlike the user path,
		// so p.limit (already bounded to maxSortedLimit, 500) is used as is.
		end := start + p.limit
		if end > len(r) {
			end = len(r)
		}
		if start < len(r) {
			page = r[start:end]
		}
		if end < len(r) && len(page) > 0 {
			last := page[len(page)-1]
			lastRow := memberKeyOf(p.sort, last)
			nextCursor = store.EncodeAgentCursor(p.sort, p.dir, lastRow.K, lastRow.Created, last.ID, binding)
		}
	}

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

	// Race drops (missing row, project or filter mismatch) apply exactly
	// as the user path; everything that survives keeps its order from page.
	plainAgents := make([]store.Agent, 0, len(page))
	for _, m := range page {
		full, ok := fullRows[m.ID]
		if !ok {
			continue // deleted between the two reads
		}
		if full.ProjectID != projectID {
			continue
		}
		if !stillMatches[m.ID] {
			continue
		}
		plainAgents = append(plainAgents, *full)
	}

	// Capabilities: one ComputeCapabilitiesBatch pass over the full rows, exactly
	// like the legacy agent-JWT branch — no read/rest split, since there is
	// no read filter to split against.
	resources := make([]Resource, len(plainAgents))
	for i := range plainAgents {
		resources[i] = agentResource(&plainAgents[i])
	}
	caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "agent")

	s.enrichAgents(ctx, plainAgents)
	agents := make([]AgentWithCapabilities, len(plainAgents))
	for i := range plainAgents {
		item := plainAgents[i]
		item.AppliedConfig = redactAppliedConfigEnvForResponse(item.AppliedConfig, s.envViewAllowed(ctx, identity, &item, caps[i]))
		agents[i] = AgentWithCapabilities{Agent: item, Cap: caps[i]}
	}

	// A complete response IS the whole set: a race drop must be
	// reflected in totalCount, the same rule as the user path.
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
