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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reissueOverlay carries the would-be result of a bulk scope re-issue dry
// run to the agents processed after it: for each agent the dry run would
// change, the edge that would replace its active edge (E') and the role it
// would store. The bulk run processes agents top-down, so a child computed
// in a dry run sees exactly what it would see after its parent's commit in
// an applied run, and the dry run reports exactly what --apply would
// change, without writing anything and without holding a transaction
// across the batch.
//
// It is carried in the context of a bulk dry run only; the dry-run branch
// of runScopeReissue writes to it where the applied run commits. The two lookups the
// re-issue computation reads an agent's delegation through consult it:
// edges by delegate (activeProjectEdges and getCachedDelegationEdges) and a
// delegator agent's stored role (planAgentDelegatorReissue and
// checkAgentHoldsPermission). Everywhere else, and in every other context,
// reissueOverlayFrom returns nil and the methods below return their input.
type reissueOverlay struct {
	mu      sync.RWMutex
	byAgent map[string]*store.DelegationEdge
	roles   map[string]AgentRole
}

type reissueOverlayKey struct{}

func newReissueOverlay() *reissueOverlay {
	return &reissueOverlay{byAgent: map[string]*store.DelegationEdge{}, roles: map[string]AgentRole{}}
}

// withReissueOverlay returns ctx carrying o.
func withReissueOverlay(ctx context.Context, o *reissueOverlay) context.Context {
	return context.WithValue(ctx, reissueOverlayKey{}, o)
}

// reissueOverlayFrom returns the overlay carried by ctx, or nil.
func reissueOverlayFrom(ctx context.Context) *reissueOverlay {
	o, _ := ctx.Value(reissueOverlayKey{}).(*reissueOverlay)
	return o
}

// record stores the would-be edge and role of agentID.
func (o *reissueOverlay) record(agentID string, edge *store.DelegationEdge, role AgentRole) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.byAgent[agentID] = edge
	o.roles[agentID] = role
}

// edges returns in with the would-be edge of an overlaid agent in place of
// its active edges in the same scope. The returned slice and edges are
// copies; in is not modified.
func (o *reissueOverlay) edges(delegateType, delegateID string, in []*store.DelegationEdge) []*store.DelegationEdge {
	if o == nil || delegateType != store.DelegationPrincipalAgent {
		return in
	}
	o.mu.RLock()
	next, ok := o.byAgent[delegateID]
	o.mu.RUnlock()
	if !ok {
		return in
	}
	out := make([]*store.DelegationEdge, 0, len(in)+1)
	for _, e := range in {
		if e.Active && e.ScopeType == next.ScopeType && e.ScopeID == next.ScopeID {
			replaced := *e
			replaced.Active = false
			out = append(out, &replaced)
			continue
		}
		out = append(out, e)
	}
	added := *next
	return append(out, &added)
}

// agent returns a with its would-be stored role when it is overlaid, as a
// copy; otherwise a.
func (o *reissueOverlay) agent(a *store.Agent) *store.Agent {
	if o == nil || a == nil {
		return a
	}
	o.mu.RLock()
	role, ok := o.roles[a.ID]
	o.mu.RUnlock()
	if !ok {
		return a
	}
	row := *a
	cfg := store.AgentAppliedConfig{}
	if a.AppliedConfig != nil {
		cfg = *a.AppliedConfig
	}
	cfg.AgentRole = string(role)
	row.AppliedConfig = &cfg
	return &row
}
