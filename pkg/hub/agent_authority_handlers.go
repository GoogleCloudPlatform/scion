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
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Where an agent's authority state (agent_authority_state.go) is shown:
//   - the single-agent GET always includes it;
//   - the agent list endpoints include it in the full view only when the
//     request carries include=authority, so a list without it makes no
//     extra reads;
//   - GET /api/v1/admin/orphaned-agents pages over the hub's non-deleted
//     agents and lists those whose computed state is orphaned or revoked,
//     for operators.
//
// Who sees what: anyone who may read the agent sees the state and cause.
// The principal's ID is shown only to a reader who may read that
// principal: for a user, a reader holding hub.health.read (hub admins);
// for an agent, a reader who may read that agent. Everyone else gets the
// principal's kind and whether it is deleted.

// agentListIncludeAuthority is the include= value that adds each item's
// authority state to an agent list.
const agentListIncludeAuthority = "authority"

// agentListIncludesAuthority reports whether query asks for authority
// states (include=authority, alone or in a comma-separated list). Unknown
// include values are ignored, as legacy list callers never got a 400 for
// an unknown parameter.
func agentListIncludesAuthority(query url.Values) bool {
	for _, v := range query["include"] {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) == agentListIncludeAuthority {
				return true
			}
		}
	}
	return false
}

// agentAuthorityReader answers authority states for one reader within one
// request. Its resolver memoizes the chain reads, and the visibility
// decisions are memoized per principal.
type agentAuthorityReader struct {
	s        *Server
	identity Identity
	res      *agentAuthorityResolver

	userIDsVisible *bool
	agentVisible   map[string]bool
}

func (s *Server) newAgentAuthorityReader(identity Identity) *agentAuthorityReader {
	return &agentAuthorityReader{
		s:            s,
		identity:     identity,
		res:          newAgentAuthorityResolver(s.authzService),
		agentVisible: map[string]bool{},
	}
}

// authority returns agent's authority state as this reader may see it,
// or nil when it cannot be computed (logged; the response omits it).
func (r *agentAuthorityReader) authority(ctx context.Context, agent *store.Agent) *api.AgentAuthority {
	out, err := r.res.resolve(ctx, agent)
	if err != nil {
		slog.WarnContext(ctx, "agent authority state could not be computed", "agent_id", agent.ID, "error", err)
		return nil
	}
	if out.Principal != nil && out.Principal.ID != "" && !r.principalVisible(ctx, agent, *out.Principal) {
		p := *out.Principal
		p.ID = ""
		out.Principal = &p
	}
	return &out
}

// principalVisible reports whether the reader may see principal p's ID.
// agent is the agent whose state names p; an agent principal is in the
// same project, because every hop of a chain is scoped to that project.
func (r *agentAuthorityReader) principalVisible(ctx context.Context, agent *store.Agent, p api.AgentAuthorityPrincipal) bool {
	if r.identity == nil {
		return false
	}
	switch p.Kind {
	case store.DelegationPrincipalUser:
		if r.userIDsVisible == nil {
			allowed := r.s.authzService.Decide(ctx, AuthzRequest{
				Principal:  principalContextForIdentity(r.identity),
				Credential: credentialContextForIdentity(r.identity),
				Resource:   Resource{Type: "hub", ID: "hub"},
				Action:     ActionRead,
				Permission: "hub.health.read",
			}).Allowed
			r.userIDsVisible = &allowed
		}
		return *r.userIDsVisible
	case store.DelegationPrincipalAgent:
		if v, ok := r.agentVisible[p.ID]; ok {
			return v
		}
		resource := Resource{Type: "agent", ID: p.ID, ParentType: "project", ParentID: agent.ProjectID}
		if principal, err := r.res.getAgent(ctx, p.ID); err == nil {
			resource = agentStatusReadResource(principal)
		}
		v := r.s.authzService.CheckAccess(ctx, r.identity, resource, ActionRead).Allowed
		r.agentVisible[p.ID] = v
		return v
	}
	return false
}

// setAgentListAuthority sets every item's authority state for the caller.
// Called for the full view of a list that carries include=authority; the
// cost is bounded by the page, and reads shared by several items are made
// once.
func (s *Server) setAgentListAuthority(ctx context.Context, agents []AgentWithCapabilities) {
	if len(agents) == 0 {
		return
	}
	reader := s.newAgentAuthorityReader(GetIdentityFromContext(ctx))
	for i := range agents {
		agents[i].Authority = reader.authority(ctx, &agents[i].Agent)
	}
}

// Paging of GET /api/v1/admin/orphaned-agents.
const (
	orphanedAgentsDefaultLimit = 100
	orphanedAgentsMaxLimit     = 500
)

// OrphanedAgentItem is one agent of GET /api/v1/admin/orphaned-agents.
type OrphanedAgentItem struct {
	ID        string             `json:"id"`
	Slug      string             `json:"slug"`
	Name      string             `json:"name,omitempty"`
	ProjectID string             `json:"projectId"`
	Phase     string             `json:"phase,omitempty"`
	Authority api.AgentAuthority `json:"authority"`
}

// OrphanedAgentsResponse is the body of GET /api/v1/admin/orphaned-agents.
// Each page scans up to limit of the hub's non-deleted agents (whether or
// not they have a delegation) and lists those whose computed state is
// orphaned or revoked, so a page can be empty while nextCursor is set:
// follow nextCursor until it is empty. scanned is the number of agents the
// page examined.
type OrphanedAgentsResponse struct {
	Agents     []OrphanedAgentItem `json:"agents"`
	Scanned    int                 `json:"scanned"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

// handleAdminOrphanedAgents serves GET /api/v1/admin/orphaned-agents: it
// pages over the hub's non-deleted agents and lists those whose computed
// authority state is orphaned or revoked, for operators. The route guard
// requires hub.health.read (the permission of the admin health summary) on
// a session credential (GOV_PENDING). Query parameters:
// state (orphaned or revoked; both when absent), limit, cursor. Soft-deleted
// agents are not listed. The cost is linear in the agents scanned.
func (s *Server) handleAdminOrphanedAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	ctx := r.Context()
	query := r.URL.Query()

	wantState := query.Get("state")
	switch wantState {
	case "", api.AgentAuthorityOrphaned, api.AgentAuthorityRevoked:
	default:
		BadRequest(w, "state must be orphaned or revoked")
		return
	}
	limit := orphanedAgentsDefaultLimit
	if v := query.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			BadRequest(w, "limit must be a positive integer")
			return
		}
		limit = min(n, orphanedAgentsMaxLimit)
	}

	page, err := s.store.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{
		Limit:          limit,
		Cursor:         query.Get("cursor"),
		SkipTotalCount: true,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	reader := s.newAgentAuthorityReader(GetIdentityFromContext(ctx))
	resp := OrphanedAgentsResponse{Agents: []OrphanedAgentItem{}, Scanned: len(page.Items), NextCursor: page.NextCursor}
	for i := range page.Items {
		agent := &page.Items[i]
		auth := reader.authority(ctx, agent)
		if !auth.Flagged() || (wantState != "" && auth.State != wantState) {
			continue
		}
		resp.Agents = append(resp.Agents, OrphanedAgentItem{
			ID:        agent.ID,
			Slug:      agent.Slug,
			Name:      agent.Name,
			ProjectID: agent.ProjectID,
			Phase:     agent.Phase,
			Authority: *auth,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
