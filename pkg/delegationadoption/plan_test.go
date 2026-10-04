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

package delegationadoption

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// world is an in-memory Reader for planner tests. It can hold rows no real
// store accepts (unknown delegator types, unknown ceiling kinds), which is
// what the exclusion tests need.
type world struct {
	agents map[string]*store.Agent
	users  map[string]*store.User
	edges  []*store.DelegationEdge
	seq    int
	clock  time.Time
}

func newWorld() *world {
	return &world{
		agents: map[string]*store.Agent{},
		users:  map[string]*store.User{},
		clock:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
}

const proj = "p1"

func (w *world) user(id string) *store.User {
	u := &store.User{ID: id, Status: store.UserStatusActive}
	w.users[id] = u
	return u
}

// agent adds a live agent created by creator (a user or agent ID) with the
// matching ancestry and an unrecorded edge from creator.
func (w *world) agent(id, creator, role string) *store.Agent {
	a := &store.Agent{ID: id, ProjectID: proj, CreatedBy: creator,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: role}}
	delegatorType := store.DelegationPrincipalUser
	if parent, ok := w.agents[creator]; ok {
		delegatorType = store.DelegationPrincipalAgent
		a.Ancestry = append(append([]string{}, parent.Ancestry...), creator)
	} else {
		a.Ancestry = []string{creator}
	}
	w.agents[id] = a
	w.edge(delegatorType, creator, id, role)
	return a
}

func (w *world) edge(delegatorType, delegatorID, delegateID, role string) *store.DelegationEdge {
	w.seq++
	w.clock = w.clock.Add(time.Minute)
	e := &store.DelegationEdge{
		ID:            fmt.Sprintf("e%d", w.seq),
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    delegateID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       proj,
		Role:          role,
		Active:        true,
		CreatedAt:     w.clock,
		UpdatedAt:     w.clock,
	}
	w.edges = append(w.edges, e)
	return e
}

// edgeOf returns the single active edge of delegateID.
func (w *world) edgeOf(t *testing.T, delegateID string) *store.DelegationEdge {
	t.Helper()
	var found *store.DelegationEdge
	for _, e := range w.edges {
		if e.DelegateID == delegateID && e.Active {
			require.Nil(t, found, "more than one active edge")
			found = e
		}
	}
	require.NotNil(t, found)
	return found
}

func (w *world) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	if a, ok := w.agents[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (w *world) ListAgents(_ context.Context, f store.AgentFilter, _ store.ListOptions) (*store.ListResult[store.Agent], error) {
	ids := make([]string, 0, len(w.agents))
	for id := range w.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	res := &store.ListResult[store.Agent]{}
	for _, id := range ids {
		a := w.agents[id]
		if (f.ProjectID != "" && a.ProjectID != f.ProjectID) || (!f.IncludeDeleted && !a.DeletedAt.IsZero()) {
			continue
		}
		res.Items = append(res.Items, *a)
	}
	return res, nil
}

func (w *world) GetUser(_ context.Context, id string) (*store.User, error) {
	if u, ok := w.users[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (w *world) GetDelegationEdgesForDelegate(_ context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	var out []*store.DelegationEdge
	for _, e := range w.edges {
		if e.DelegateType == delegateType && e.DelegateID == delegateID && e.Active {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (w *world) ListAllDelegationEdgesForDelegate(_ context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	var out []*store.DelegationEdge
	for _, e := range w.edges {
		if e.DelegateType == delegateType && e.DelegateID == delegateID {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (w *world) plan(t *testing.T) *Plan {
	t.Helper()
	p, err := Build(context.Background(), w, Scope{})
	require.NoError(t, err)
	return p
}

func requireHop(t *testing.T, p *Plan, id string) *Hop {
	t.Helper()
	h := p.Hop(id)
	require.NotNil(t, h, "no hop for %s", id)
	return h
}

func assertExcluded(t *testing.T, p *Plan, id string, reason Reason) {
	t.Helper()
	h := requireHop(t, p, id)
	assert.Equal(t, OutcomeExcluded, h.Outcome, "%s", id)
	assert.Equal(t, reason, h.Reason, "%s", id)
	assert.Empty(t, h.CeilingIDs)
}

func policy(t *testing.T, role string, sa bool) []string {
	t.Helper()
	ids, ok := permissions.CompatibilityCeiling(PolicyVersion, role, sa)
	require.True(t, ok)
	return ids
}

// recorded marks e as a recorded hop with ceiling c.
func recorded(e *store.DelegationEdge, c store.EffectCeiling, cred store.SourceCredentialKind) {
	e.AuthorityProvenance = store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  e.DelegatorType,
		SourcePrincipalID:    e.DelegatorID,
		SourceCredentialKind: cred,
	}
	e.EffectCeiling = c
}

func migrationCeiling(ids []string) store.EffectCeiling {
	return store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1,
		PermissionIDs: ids, BoundaryKind: string(permissions.BoundaryKindProject), BoundaryProjectID: proj}
}

func TestPlanAdoptsValidChainTopDown(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.agent("b", "a", "full")
	p := w.plan(t)
	require.Len(t, p.Hops, 2)
	assert.Equal(t, "a", p.Hops[0].DelegateID)
	assert.Equal(t, 1, p.Hops[0].Depth)
	assert.Equal(t, "b", p.Hops[1].DelegateID)
	assert.Equal(t, 2, p.Hops[1].Depth)
	for _, h := range p.Hops {
		assert.Equal(t, OutcomeAdopt, h.Outcome)
		assert.Equal(t, policy(t, "full", false), h.CeilingIDs)
		assert.Equal(t, h.Edge.ID, h.OriginalEdgeID)
	}
}

func TestPlanExcludesDeletedDelegate(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full").DeletedAt = time.Now()
	p := w.plan(t)
	assert.Nil(t, p.Hop("a"), "a deleted delegate is not examined")
	assert.Zero(t, p.Count(OutcomeAdopt))
}

func TestPlanExcludesInactiveEdges(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.edgeOf(t, "a").Active = false
	p := w.plan(t)
	assertExcluded(t, p, "a", ReasonMissingEdge)
}

func TestPlanExcludesDeletedParent(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full").DeletedAt = time.Now()
	w.agent("b", "a", "full")
	assertExcluded(t, w.plan(t), "b", ReasonParentDeleted)
}

func TestPlanExcludesMissingParent(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.agent("b", "a", "full")
	delete(w.agents, "a")
	assertExcluded(t, w.plan(t), "b", ReasonParentMissing)
}

func TestPlanExcludesMissingRoot(t *testing.T) {
	w := newWorld()
	w.agent("a", "ghost", "full")
	assertExcluded(t, w.plan(t), "a", ReasonRootMissing)
}

func TestPlanExcludesInactiveRoot(t *testing.T) {
	w := newWorld()
	w.user("u").Status = "suspended"
	w.agent("a", "u", "full")
	assertExcluded(t, w.plan(t), "a", ReasonRootInactive)
}

func TestPlanExcludesMigrationSentinelRoot(t *testing.T) {
	w := newWorld()
	w.user(migrationDelegatorID)
	w.agent("a", migrationDelegatorID, "full")
	assertExcluded(t, w.plan(t), "a", ReasonNoPrincipalRoot)
}

func TestPlanExcludesCycle(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.agent("b", "a", "full")
	// Rewire a's edge to b: a -> b -> a.
	ea := w.edgeOf(t, "a")
	ea.DelegatorType = store.DelegationPrincipalAgent
	ea.DelegatorID = "b"
	w.agents["a"].Ancestry = []string{"u", "b"}
	p := w.plan(t)
	assertExcluded(t, p, "a", ReasonCycle)
	assertExcluded(t, p, "b", ReasonCycle)
}

func TestPlanExcludesDepthOverLimit(t *testing.T) {
	w := newWorld()
	w.user("u")
	prev := "u"
	for i := 0; i <= MaxDelegationDepth+1; i++ {
		id := fmt.Sprintf("a%02d", i)
		w.agent(id, prev, "full")
		prev = id
	}
	p := w.plan(t)
	last := fmt.Sprintf("a%02d", MaxDelegationDepth)
	assert.Equal(t, OutcomeAdopt, requireHop(t, p, last).Outcome, "the deepest hop the walk examines")
	assertExcluded(t, p, fmt.Sprintf("a%02d", MaxDelegationDepth+1), ReasonTooDeep)
}

func TestPlanExcludesDuplicateActiveEdges(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.edge(store.DelegationPrincipalUser, "u", "a", "full")
	assertExcluded(t, w.plan(t), "a", ReasonDuplicateActiveEdges)
}

func TestPlanExcludesMissingAncestorEdge(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.agent("b", "a", "full")
	w.edgeOf(t, "a").Active = false
	p := w.plan(t)
	assertExcluded(t, p, "a", ReasonMissingEdge)
	assertExcluded(t, p, "b", ReasonAncestorExcluded)
}

func TestPlanExcludesUnknownProvenanceVersion(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.edgeOf(t, "a").ProvenanceVersion = 2
	assertExcluded(t, w.plan(t), "a", ReasonUnknownProvenanceVersion)
}

func TestPlanExcludesMalformedUnrecordedRow(t *testing.T) {
	cases := map[string]func(e *store.DelegationEdge){
		"source set":      func(e *store.DelegationEdge) { e.SourceCredentialKind = store.SourceCredentialSession },
		"initiator set":   func(e *store.DelegationEdge) { e.InitiatorPrincipalID = "x" },
		"ceiling ids":     func(e *store.DelegationEdge) { e.PermissionIDs = []string{} },
		"bounded kind":    func(e *store.DelegationEdge) { e.Kind = store.EffectCeilingBounded },
		"boundary":        func(e *store.DelegationEdge) { e.BoundaryProjectID = proj },
		"deactivation":    func(e *store.DelegationEdge) { e.OpID = "op" },
		"ceiling version": func(e *store.DelegationEdge) { e.Version = permissions.CeilingVersionV1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.user("u")
			w.agent("a", "u", "full")
			mutate(w.edgeOf(t, "a"))
			assertExcluded(t, w.plan(t), "a", ReasonMalformedProvenance)
		})
	}
}

func TestPlanExcludesUnknownCeilingKind(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.edgeOf(t, "a").Kind = "unbounded"
	assertExcluded(t, w.plan(t), "a", ReasonMalformedCeiling)

	w2 := newWorld()
	w2.user("u")
	w2.agent("a", "u", "full")
	recorded(w2.edgeOf(t, "a"), store.EffectCeiling{}, store.SourceCredentialSession)
	assertExcluded(t, w2.plan(t), "a", ReasonMalformedCeiling)
}

func TestPlanExcludesUnknownAndNoneRoles(t *testing.T) {
	cases := []struct {
		edgeRole, appliedRole string
		reason                Reason
	}{
		{"none", "full", ReasonRoleNone},
		{"admin", "full", ReasonUnknownRole},
		{"full", "none", ReasonRoleNone},
		{"full", "owner", ReasonUnknownRole},
		{"full", "", ReasonUnreadableAgentConfig},
	}
	for _, c := range cases {
		t.Run(c.edgeRole+"/"+c.appliedRole, func(t *testing.T) {
			w := newWorld()
			w.user("u")
			a := w.agent("a", "u", c.appliedRole)
			w.edgeOf(t, "a").Role = c.edgeRole
			_ = a
			assertExcluded(t, w.plan(t), "a", c.reason)
		})
	}
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full").AppliedConfig = nil
	assertExcluded(t, w.plan(t), "a", ReasonUnreadableAgentConfig)
}

// Scheduled dispatch writes its children with applied role none and an edge
// of role none. Such rows never enter the cohort.
func TestPlanExcludesRoleNoneScheduledRows(t *testing.T) {
	w := newWorld()
	w.user("u")
	a := w.agent("sched", "u", "none")
	a.AppliedConfig.NoAuth = true
	assertExcluded(t, w.plan(t), "sched", ReasonRoleNone)
}

func TestPlanExcludesDelegatorAncestryMismatch(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.user("v")
	w.agent("a", "u", "full")
	w.agents["a"].Ancestry = []string{"v"}
	assertExcluded(t, w.plan(t), "a", ReasonDelegatorAncestryMismatch)

	// Empty ancestry: a user delegator must equal CreatedBy.
	w2 := newWorld()
	w2.user("u")
	w2.agent("a", "u", "full")
	w2.agents["a"].Ancestry = nil
	assert.Equal(t, OutcomeAdopt, requireHop(t, w2.plan(t), "a").Outcome)
	w2.agents["a"].CreatedBy = "v"
	assertExcluded(t, w2.plan(t), "a", ReasonDelegatorAncestryMismatch)

	// Agent-created: the parent must be the last ancestry entry.
	w3 := newWorld()
	w3.user("u")
	w3.agent("a", "u", "full")
	w3.agent("c", "u", "full")
	w3.agent("b", "a", "full")
	w3.agents["b"].Ancestry = []string{"u", "c"}
	assertExcluded(t, w3.plan(t), "b", ReasonDelegatorAncestryMismatch)
}

func TestPlanExcludesUnsupportedDelegatorType(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.edgeOf(t, "a").DelegatorType = "group"
	assertExcluded(t, w.plan(t), "a", ReasonUnsupportedDelegatorType)
}

func TestPlanExcludesScopeMismatch(t *testing.T) {
	// The agent's only active edge is in another project.
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.edgeOf(t, "a").ScopeID = "p2"
	assertExcluded(t, w.plan(t), "a", ReasonScopeMismatch)

	// An agent delegator in another project.
	w2 := newWorld()
	w2.user("u")
	w2.agent("a", "u", "full")
	w2.agent("b", "a", "full")
	w2.agents["a"].ProjectID = "p2"
	w2.edgeOf(t, "a").ScopeID = "p2"
	assertExcluded(t, w2.plan(t), "b", ReasonScopeMismatch)
}

func TestPlanExcludesDescendantsOfExcludedHop(t *testing.T) {
	w := newWorld()
	w.user("u").Status = "suspended"
	w.agent("a", "u", "full")
	w.agent("b", "a", "full")
	w.agent("c", "b", "full")
	p := w.plan(t)
	assertExcluded(t, p, "a", ReasonRootInactive)
	assertExcluded(t, p, "b", ReasonAncestorExcluded)
	assertExcluded(t, p, "c", ReasonAncestorExcluded)
}

func TestPlanUsesLowerOfEdgeAndAgentRole(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "baseline")
	w.edgeOf(t, "a").Role = "full"
	w.agent("b", "u", "full")
	w.edgeOf(t, "b").Role = "readonly"
	p := w.plan(t)
	ha, hb := requireHop(t, p, "a"), requireHop(t, p, "b")
	assert.Equal(t, "baseline", ha.Role)
	assert.Equal(t, policy(t, "baseline", false), ha.CeilingIDs)
	assert.Equal(t, "readonly", hb.Role)
	assert.Equal(t, policy(t, "readonly", false), hb.CeilingIDs)
	assert.Equal(t, "full", ha.Edge.Role, "the recorded edge role is unchanged")
}

func TestPlanAddsAssignedSAPermissionsFromConfig(t *testing.T) {
	w := newWorld()
	w.user("u")
	a := w.agent("a", "u", "readonly")
	a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: "sa1"}
	b := w.agent("b", "u", "readonly")
	b.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough}
	p := w.plan(t)
	ha := requireHop(t, p, "a")
	assert.True(t, ha.HasAssignedSA)
	assert.Contains(t, ha.CeilingIDs, "gcp_service_account.assign")
	assert.Contains(t, ha.CeilingIDs, "gcp_service_account.use")
	hb := requireHop(t, p, "b")
	assert.False(t, hb.HasAssignedSA)
	assert.Equal(t, policy(t, "readonly", false), hb.CeilingIDs)
}

func TestPlanIntersectsAgentDelegatorHopWithParentCeiling(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "baseline")
	w.agent("b", "a", "full")
	p := w.plan(t)
	hb := requireHop(t, p, "b")
	assert.Equal(t, policy(t, "baseline", false), hb.CeilingIDs, "full ∩ baseline parent")
	assert.Subset(t, requireHop(t, p, "a").CeilingIDs, hb.CeilingIDs)
}

// A recorded bounded ancestor (for example created from a token) bounds an
// unrecorded descendant's adopted ceiling.
func TestPlanIntersectsExistingBoundedCeiling(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	recorded(w.edgeOf(t, "a"), store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1,
		PermissionIDs: []string{"agent.create", "project.read", "template.read"}}, store.SourceCredentialUAT)
	w.agent("b", "a", "full")
	p := w.plan(t)
	assert.Equal(t, OutcomeRecorded, requireHop(t, p, "a").Outcome)
	hb := requireHop(t, p, "b")
	assert.Equal(t, OutcomeAdopt, hb.Outcome)
	assert.Equal(t, []string{"agent.create", "project.read", "template.read"}, hb.CeilingIDs)

	// A principal ancestor does not narrow.
	w2 := newWorld()
	w2.user("u")
	w2.agent("a", "u", "full")
	recorded(w2.edgeOf(t, "a"), store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, store.SourceCredentialSession)
	w2.agent("b", "a", "full")
	assert.Equal(t, policy(t, "full", false), requireHop(t, w2.plan(t), "b").CeilingIDs)
}

func TestPlanSkipsRecordedHops(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	recorded(w.edgeOf(t, "a"), store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, store.SourceCredentialSession)
	p := w.plan(t)
	h := requireHop(t, p, "a")
	assert.Equal(t, OutcomeRecorded, h.Outcome)
	assert.Empty(t, h.CeilingIDs)
	assert.Zero(t, p.Count(OutcomeAdopt))
}

func TestPlanRecognizesSystemMigrationEdges(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	orig := w.edgeOf(t, "a")
	orig.Active = false
	repaired := w.edge(store.DelegationPrincipalUser, "u", "a", "full")
	recorded(repaired, migrationCeiling(policy(t, "full", false)), store.SourceCredentialSystemMigration)
	w.agent("b", "a", "full")
	p := w.plan(t)
	ha := requireHop(t, p, "a")
	assert.Equal(t, OutcomeRecognized, ha.Outcome)
	assert.Equal(t, orig.ID, ha.OriginalEdgeID)
	assert.Equal(t, repaired.PermissionIDs, ha.CeilingIDs)
	assert.Equal(t, OutcomeAdopt, requireHop(t, p, "b").Outcome, "a recognized ancestor is a valid hop")

	// Two matching inactive rows: the original is not resolved.
	w.edge(store.DelegationPrincipalUser, "u", "a", "full").Active = false
	assert.Empty(t, requireHop(t, w.plan(t), "a").OriginalEdgeID)
}

func TestPlanFlagsAdoptedEdgeAbovePolicy(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "baseline")
	recorded(w.edgeOf(t, "a"), migrationCeiling(policy(t, "full", false)), store.SourceCredentialSystemMigration)
	w.edgeOf(t, "a").Role = "baseline"
	h := requireHop(t, w.plan(t), "a")
	assert.Equal(t, OutcomeRecognizedAbovePolicy, h.Outcome)
	assert.Equal(t, policy(t, "full", false), h.CeilingIDs, "reported, never narrowed")
}

func TestPlanFingerprintChangesOnRelevantState(t *testing.T) {
	mutations := map[string]func(w *world, t *testing.T){
		"edge updated": func(w *world, t *testing.T) { w.edgeOf(t, "b").UpdatedAt = w.edgeOf(t, "b").UpdatedAt.Add(time.Second) },
		"edge role":    func(w *world, t *testing.T) { w.edgeOf(t, "b").Role = "baseline" },
		"applied role": func(w *world, t *testing.T) { w.agents["b"].AppliedConfig.AgentRole = "baseline" },
		"assigned SA": func(w *world, t *testing.T) {
			w.agents["b"].AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: "sa"}
		},
		"root status":    func(w *world, t *testing.T) { w.users["u"].Status = "suspended" },
		"parent deleted": func(w *world, t *testing.T) { w.agents["a"].DeletedAt = time.Now() },
		"parent ceiling": func(w *world, t *testing.T) {
			recorded(w.edgeOf(t, "a"), migrationCeiling([]string{"project.read"}), store.SourceCredentialSystemMigration)
		},
		"delegate edge gone": func(w *world, t *testing.T) { w.edgeOf(t, "b").Active = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.user("u")
			w.agent("a", "u", "full")
			w.agent("b", "a", "full")
			before := w.plan(t).Fingerprint("adopt")
			mutate(w, t)
			assert.NotEqual(t, before, w.plan(t).Fingerprint("adopt"))
		})
	}
}

func TestPlanFingerprintStableOtherwise(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.agent("b", "a", "full")
	first := w.plan(t).Fingerprint("adopt")
	// Unrelated state: another project's agent, an unrelated user, the
	// agent's phase and task.
	w.user("other")
	w.agents["b"].Phase = "stopped"
	w.agents["b"].AppliedConfig.Task = "something else"
	assert.Equal(t, first, w.plan(t).Fingerprint("adopt"))
	assert.NotEqual(t, first, w.plan(t).Fingerprint("revert"), "the operation is bound")
}

func TestPlanScopeSelectsAncestorClosure(t *testing.T) {
	w := newWorld()
	w.user("u")
	w.agent("a", "u", "full")
	w.agent("b", "a", "full")
	w.agent("c", "u", "full")
	p, err := Build(context.Background(), w, Scope{AgentIDs: []string{"b"}})
	require.NoError(t, err)
	assert.NotNil(t, p.Hop("a"))
	assert.NotNil(t, p.Hop("b"))
	assert.Nil(t, p.Hop("c"))
}
