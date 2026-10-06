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

//go:build !no_sqlite

package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boundedCeiling(ids ...string) store.EffectCeiling {
	if ids == nil {
		ids = []string{}
	}
	return store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: ids}
}

func allRegistryIDs() []string {
	ids := make([]string, 0, len(permissions.Registry))
	for _, p := range permissions.Registry {
		ids = append(ids, p.ID)
	}
	return ids
}

// knownCeilingVersion agrees with FrozenPermissionCeiling.Allows for every
// version in [-1, 100].
func TestKnownCeilingVersionMatchesAllows(t *testing.T) {
	for v := -1; v <= 100; v++ {
		version := permissions.CeilingVersion(v)
		f := permissions.FrozenPermissionCeiling{Version: version, PermissionIDs: []string{"agent.create"}}
		assert.Equal(t, f.Allows("agent.create"), knownCeilingVersion(version), "version %d", v)
	}
}

// A scope with no registry coverage and no reviewed mapping is withheld
// under every bounded ceiling, including an empty one and one holding the
// whole registry.
func TestZeroCoverageScopeDenied(t *testing.T) {
	synthetic := []AgentTokenScope{"project:agent:not_a_scope", AgentTokenScope(ScopeGCPTokenPrefix)}
	for _, scope := range synthetic {
		require.Empty(t, agentScopeCoverage([]AgentTokenScope{scope}), "scope %q must have zero coverage", scope)
		assert.False(t, ceilingAllowsScope(boundedCeiling(), scope), "empty bounded, scope %q", scope)
		assert.False(t, ceilingAllowsScope(boundedCeiling(allRegistryIDs()...), scope), "full bounded, scope %q", scope)
	}
	assert.False(t, ceilingAllowsScope(store.EffectCeiling{Kind: "other"}, ScopeProjectRead), "unknown kind")
}

// project:gcp:token:<sa> is issued under bounded only when the ceiling allows
// gcp_service_account.assign; always under principal; unchanged under
// unrecorded.
func TestGCPTokenScopeRequiresSAAssignInCeiling(t *testing.T) {
	scope := GCPTokenScopeForSA("sa-1")
	assert.True(t, ceilingAllowsScope(boundedCeiling("gcp_service_account.assign"), scope))
	assert.False(t, ceilingAllowsScope(boundedCeiling("gcp_service_account.use", "project.read"), scope))
	assert.False(t, ceilingAllowsScope(boundedCeiling(), scope))
	assert.False(t, ceilingAllowsScope(store.EffectCeiling{
		Kind: store.EffectCeilingBounded, Version: 99, PermissionIDs: []string{"gcp_service_account.assign"},
	}, scope), "unknown version")
	assert.True(t, ceilingAllowsScope(store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, scope))
	assert.True(t, ceilingAllowsScope(store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}, scope))
}

// Every agent-token scope constant and prefix has registry coverage or a
// reviewed zero-coverage mapping.
func TestEveryAgentScopeHasCoverageOrReviewedMapping(t *testing.T) {
	scopes := []AgentTokenScope{
		ScopeProjectRead, ScopeAgentStatusUpdate, ScopeAgentTokenRefresh, ScopeAgentNotify,
		ScopeAgentPortForward, ScopeAgentCreate, ScopeAgentSAAssign, ScopeAgentLifecycle,
		ScopeProjectSecretRead, ScopeProjectTemplateWrite, ScopeAgentSetMessageMode, ScopeIdentityToken,
		GCPTokenScopeForSA("sa-1"),
	}
	for _, role := range []AgentRole{AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull} {
		scopes = append(scopes, ScopesForRole(role)...)
	}
	for _, scope := range scopes {
		if len(agentScopeCoverage([]AgentTokenScope{scope})) > 0 {
			continue
		}
		_, mapped := zeroCoverageMappedPermission(scope)
		assert.True(t, mapped, "scope %q has no registry coverage and no reviewed mapping", scope)
	}
	for prefix, perm := range zeroCoverageScopeMapping {
		_, ok := registryPermission(perm)
		assert.True(t, ok, "mapping for %q names unknown permission %q", prefix, perm)
	}
}

// Every registry permission with an empty UATScope and non-empty AgentScopes
// is in exactly one of the self and non-self lists, and every list entry is
// such a permission. Self operations target the agent resource.
func TestSelfOperationTableMatchesRegistry(t *testing.T) {
	listed := map[string]int{}
	for _, id := range selfOperationPermissionIDs {
		listed[id]++
	}
	for _, id := range nonSelfOperationPermissionIDs {
		listed[id]++
	}
	rows := 0
	for _, p := range permissions.Registry {
		inScope := p.UATScope == "" && len(p.AgentScopes) > 0
		if inScope {
			rows++
			assert.Equal(t, 1, listed[p.ID], "permission %q must be in exactly one list", p.ID)
		} else {
			assert.Zero(t, listed[p.ID], "permission %q must not be listed", p.ID)
		}
	}
	assert.Equal(t, len(listed), rows, "every list entry is a registry permission in scope")
	for _, id := range selfOperationPermissionIDs {
		p, ok := registryPermission(id)
		require.True(t, ok, id)
		assert.Equal(t, "agent", p.Resource, "self operation %q must target the agent resource", id)
	}
}

// uatCeilingFromSelectors returns the bounded ceiling a V1 UAT with the given
// selectors carries.
func uatCeilingFromSelectors(t *testing.T, selectors ...string) store.EffectCeiling {
	t.Helper()
	var ids []string
	for _, sel := range selectors {
		found := false
		for _, p := range permissions.Registry {
			if p.UATScope == sel {
				ids = append(ids, p.ID)
				found = true
			}
		}
		require.True(t, found, "unknown UAT selector %q", sel)
	}
	return boundedCeiling(sortedUniqueIDs(ids)...)
}

// readonlyRoleUATSelectors returns the UAT selectors that cover the
// readonly role's required scopes: the seven read selectors of the worked
// example. Ceiling-optional role scopes (ceilingOptionalRoleScopes) are left
// out: they never decide whether a ceiling fits the role.
func readonlyRoleUATSelectors(t *testing.T) []string {
	t.Helper()
	readSelectors := []string{}
	for _, scope := range ScopesForRole(AgentRoleReadOnly) {
		if ceilingOptionalRoleScopes[scope] {
			continue
		}
		for _, permID := range agentScopeCoverage([]AgentTokenScope{scope}) {
			p, _ := registryPermission(permID)
			if p.UATScope != "" {
				readSelectors = append(readSelectors, p.UATScope)
			}
		}
	}
	readSelectors = sortedUniqueIDs(readSelectors)
	require.Len(t, readSelectors, 7, "the worked example uses seven read selectors")
	return readSelectors
}

// The pure role cap: the minimal selector set fits baseline (self
// operations are free); a readonly default stays readonly; an explicit role
// over the ceiling denies; explicit none is allowed; a defaulted role that
// fits nothing above none denies.
func TestUATChildRoleCappedWithinCeiling(t *testing.T) {
	readSelectors := readonlyRoleUATSelectors(t)
	createP, _ := registryPermission("agent.create")
	minimal := uatCeilingFromSelectors(t, append([]string{createP.UATScope}, readSelectors...)...)
	createAndProjectRead := uatCeilingFromSelectors(t, createP.UATScope, readSelectors[0])
	principal := store.EffectCeiling{Kind: store.EffectCeilingPrincipal}

	cases := []struct {
		name     string
		ceiling  store.EffectCeiling
		role     AgentRole
		explicit bool
		want     AgentRole
		cause    DenyCause
	}{
		{"minimal set caps full default to baseline", minimal, AgentRoleFull, false, AgentRoleBaseline, ""},
		{"readonly default stays readonly", minimal, AgentRoleReadOnly, false, AgentRoleReadOnly, ""},
		{"defaulted none stays none", minimal, AgentRoleNone, false, AgentRoleNone, ""},
		{"explicit baseline fits", minimal, AgentRoleBaseline, true, AgentRoleBaseline, ""},
		{"explicit full over ceiling", minimal, AgentRoleFull, true, "", DenyCauseCeilingEffectExceeded},
		{"explicit none allowed", boundedCeiling(), AgentRoleNone, true, AgentRoleNone, ""},
		{"defaulted role fits nothing", createAndProjectRead, AgentRoleFull, false, "", DenyCauseCeilingEffectExceeded},
		{"principal keeps role", principal, AgentRoleFull, false, AgentRoleFull, ""},
		{"principal explicit full", principal, AgentRoleFull, true, AgentRoleFull, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cause, ok := childRoleWithinCeiling(tc.ceiling, tc.role, tc.explicit)
			assert.Equal(t, tc.cause == "", ok)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.cause, cause)
		})
	}

	// The baseline cap carries project:read, the four self-op scopes and the
	// ceiling-optional project:artifact:read and project:artifact:write. The
	// minimal set holds no artifact permission, so the ceiling filter issues
	// all but the two artifact scopes: optional scopes never decide the fit
	// and are never issued beyond the ceiling.
	capped, _, _ := childRoleWithinCeiling(minimal, AgentRoleFull, false)
	want := []AgentTokenScope{ScopeProjectRead, ScopeAgentStatusUpdate, ScopeAgentTokenRefresh, ScopeAgentNotify, ScopeAgentPortForward}
	assert.ElementsMatch(t, append(append([]AgentTokenScope{}, want...), ScopeProjectArtifactRead, ScopeProjectArtifactWrite), ScopesForRole(capped))
	assert.ElementsMatch(t, want, filterScopes(ScopesForRole(capped), minimal, ScopeCeilings{}))
}

// EffectCeilingAllows over every kind.
func TestEffectCeilingAllows(t *testing.T) {
	principal := store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	unrecorded := store.EffectCeiling{}
	b := boundedCeiling("agent.create")
	for _, p := range allRegistryIDs() {
		assert.True(t, EffectCeilingAllows(principal, p, false), p)
		assert.Equal(t, !recordedProvenanceRequired[p] && !legacyChainExcludedPermissions[p], EffectCeilingAllows(unrecorded, p, false), p)
		assert.False(t, EffectCeilingAllows(store.EffectCeiling{Kind: "other"}, p, true), p)
		want := p == "agent.create"
		assert.Equal(t, want, EffectCeilingAllows(b, p, false), p)
		assert.Equal(t, want || selfOperationSet[p], EffectCeilingAllows(b, p, true), p)
	}
	assert.False(t, EffectCeilingAllows(principal, "", false), "empty permission")
}

// The pure intersection step of parentDeliverEligibility: for every subset
// of the three delivery IDs held by a bounded chain, exactly that subset is
// returned; principal returns all; any unrecorded hop or kind returns none.
func TestAgentCreateDeliverIDs_BoundedParentSubset(t *testing.T) {
	t.Run("pure deliverIDsAllowedByChain table", func(t *testing.T) {
		for mask := 0; mask < 1<<len(hubDeliveryPermissionList); mask++ {
			var subset []string
			for i, id := range hubDeliveryPermissionList {
				if mask&(1<<i) != 0 {
					subset = append(subset, id)
				}
			}
			ids := append([]string{"agent.create", "agent.status_update"}, subset...)
			got := deliverIDsAllowedByChain(ChainCeiling{Ceiling: boundedCeiling(ids...)})
			assert.ElementsMatch(t, subset, got, "mask %b", mask)
			assert.Empty(t, deliverIDsAllowedByChain(ChainCeiling{Ceiling: boundedCeiling(ids...), UnrecordedHops: 1}),
				"mask %b with an unrecorded hop", mask)
		}
		assert.ElementsMatch(t, hubDeliveryPermissionList,
			deliverIDsAllowedByChain(ChainCeiling{Ceiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal}}))
		assert.Empty(t, deliverIDsAllowedByChain(ChainCeiling{Ceiling: store.EffectCeiling{}}))
		assert.Empty(t, deliverIDsAllowedByChain(ChainCeiling{
			Ceiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, UnrecordedHops: 1,
		}))
	})
}

// TestHubDeliveryPermissionListLiteral pins the delivery permissions an
// agent-created edge may carry to a literal, sorted list. The list is derived
// from hubDeliveryPermissionIDs, so adding a deliver permission to that map
// widens agent-created edges; this test makes that a deliberate test edit.
func TestHubDeliveryPermissionListLiteral(t *testing.T) {
	want := []string{"env_var.deliver", "secret.deliver", "skill_injection.deliver"}
	assert.Equal(t, want, hubDeliveryPermissionList)
	assert.Len(t, hubDeliveryPermissionSet, len(want))
	for _, id := range want {
		assert.True(t, hubDeliveryPermissionSet[id], id)
	}
}

// childEffectCeiling: bounded V1, sorted and de-duplicated; a bounded source
// narrows the coverage; deliver IDs outside the fixed set are dropped.
func TestChildEffectCeiling(t *testing.T) {
	cov := []string{"project.read", "agent.create", "agent.create"}
	got := childEffectCeiling(store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, cov, []string{"secret.deliver", "agent.delete"})
	assert.Equal(t, store.EffectCeilingBounded, got.Kind)
	assert.Equal(t, permissions.CeilingVersionV1, got.Version)
	assert.Equal(t, []string{"agent.create", "project.read", "secret.deliver"}, got.PermissionIDs)

	got = childEffectCeiling(boundedCeiling("project.read"), cov, nil)
	assert.Equal(t, []string{"project.read"}, got.PermissionIDs)

	got = childEffectCeiling(store.EffectCeiling{}, nil, nil)
	assert.NotNil(t, got.PermissionIDs)
	assert.Empty(t, got.PermissionIDs)
}

// Every new ceiling error maps to a DenyCause; lookup faults map to none.
func TestCeilingErrorsMapToDenyCause(t *testing.T) {
	cases := map[error]DenyCause{
		errSourceNotAllowed:        DenyCauseCeilingSourceNotAllowed,
		errSourceCeilingUnrecorded: DenyCauseCeilingUnrecorded,
		ErrProvenanceMissing:       DenyCauseCeilingOrphaned,
		ErrProvenanceAmbiguous:     DenyCauseCeilingOrphaned,
		ErrProvenanceChain:         DenyCauseCeilingOrphaned,
	}
	for err, want := range cases {
		got, ok := ceilingDenyCauseForError(fmt.Errorf("wrapped: %w", err))
		assert.True(t, ok, err.Error())
		assert.Equal(t, want, got, err.Error())
		assert.True(t, isStructuralProvenanceError(err), err.Error())
	}
	_, ok := ceilingDenyCauseForError(errors.New("store fault"))
	assert.False(t, ok)
	assert.False(t, isStructuralProvenanceError(errors.New("store fault")))
}

// --- Chain fold ---

// ambiguousEdgeStore returns every edge of dupID twice.
type ambiguousEdgeStore struct {
	store.Store
	dupID string
}

func (s *ambiguousEdgeStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	edges, err := s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil || delegateID != s.dupID {
		return edges, err
	}
	out := make([]*store.DelegationEdge, 0, 2*len(edges))
	for _, e := range edges {
		cp := *e
		cp.ID = e.ID + "-dup"
		out = append(out, e, &cp)
	}
	return out, nil
}

type ceilingFixture struct {
	store     store.Store
	projectID string
	userID    string
}

func newCeilingFixture(t *testing.T, name string) ceilingFixture {
	t.Helper()
	_, s := authzTestSetup(t)
	f := ceilingFixture{store: s, projectID: tid("ec-proj-" + name), userID: tid("ec-user-" + name)}
	createDCProject(t, s, f.projectID, "ec-"+name)
	createDCUser(t, s, f.userID, "ec-"+name+"@test.com", f.projectID, store.ProjectRoleOwner)
	return f
}

// authz returns an AuthzService on store s with explicit flags.
func (f ceilingFixture) authz(s store.Store, devLocal, mintOverride bool) *AuthzService {
	a := NewAuthzService(s, slog.Default())
	a.setDevLocalAuthorityEnabled(devLocal)
	a.mintDevAuthOverride = mintOverride
	return a
}

func (f ceilingFixture) agent(t *testing.T, name string, role AgentRole) *store.Agent {
	t.Helper()
	id := tid("ec-agent-" + name)
	createDCAgent(t, f.store, id, f.projectID, f.userID, role)
	a, err := f.store.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

func (f ceilingFixture) edge(t *testing.T, delegatorType, delegatorID, delegateID string, c store.EffectCeiling, p store.AuthorityProvenance) {
	t.Helper()
	require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType:       delegatorType,
		DelegatorID:         delegatorID,
		DelegateType:        store.DelegationPrincipalAgent,
		DelegateID:          delegateID,
		ScopeType:           store.RoleScopeProject,
		ScopeID:             f.projectID,
		Role:                string(AgentRoleFull),
		Active:              true,
		AuthorityProvenance: p,
		EffectCeiling:       c,
	}))
}

var (
	provSession  = store.AuthorityProvenance{ProvenanceVersion: 1, SourceCredentialKind: store.SourceCredentialSession}
	provAgent    = store.AuthorityProvenance{ProvenanceVersion: 1, SourceCredentialKind: store.SourceCredentialAgent}
	provDevLocal = store.AuthorityProvenance{ProvenanceVersion: 1, SourceCredentialKind: store.SourceCredentialDevLocal}
	ceilPrincip  = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
)

func TestChainEffectCeilingFold(t *testing.T) {
	ctx := context.Background()

	t.Run("principal user edge", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-p")
		a := f.agent(t, "fold-p", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provSession)
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingPrincipal, got.Ceiling.Kind)
		assert.Zero(t, got.UnrecordedHops)
	})

	t.Run("unrecorded user edge", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-u")
		a := f.agent(t, "fold-u", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingUnrecorded, got.Ceiling.Kind)
		assert.Equal(t, 1, got.UnrecordedHops)
	})

	t.Run("bounded hops intersect", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-b")
		p := f.agent(t, "fold-b-p", AgentRoleFull)
		c := f.agent(t, "fold-b-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, p.ID, boundedCeiling("agent.create", "project.read", "agent.delete"), provSession)
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID, boundedCeiling("project.read", "agent.delete", "agent.lifecycle", "not.a.permission"), provAgent)
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, c)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingBounded, got.Ceiling.Kind)
		assert.Equal(t, permissions.CeilingVersionV1, got.Ceiling.Version)
		assert.Equal(t, []string{"agent.delete", "project.read"}, got.Ceiling.PermissionIDs)
		assert.Zero(t, got.UnrecordedHops)
	})

	t.Run("unknown provenance version counts unrecorded and narrows", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-v2")
		a := f.agent(t, "fold-v2", AgentRoleFull)
		prov := provSession
		prov.ProvenanceVersion = 2
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, boundedCeiling("project.read"), prov)
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingBounded, got.Ceiling.Kind)
		assert.Equal(t, []string{"project.read"}, got.Ceiling.PermissionIDs)
		assert.Equal(t, 1, got.UnrecordedHops)
	})

	t.Run("unknown provenance version on a principal hop counts unrecorded", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-pv2")
		a := f.agent(t, "fold-pv2", AgentRoleFull)
		prov := provSession
		prov.ProvenanceVersion = 2
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, prov)
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingUnrecorded, got.Ceiling.Kind, "never read as principal")
		assert.Equal(t, 1, got.UnrecordedHops)
		assert.Nil(t, deliverIDsAllowedByChain(got))
	})

	// A chain that mixes an unrecorded hop with a bounded hop is bounded by
	// the bounded hop and reports the unrecorded hop. The step-10 walk
	// denies recordedProvenanceRequired permissions on the unrecorded hop at
	// use; delivery is withheld on the fold.
	t.Run("unrecorded hop above a bounded hop folds to bounded", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-ub")
		p := f.agent(t, "fold-ub-p", AgentRoleFull)
		c := f.agent(t, "fold-ub-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, p.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID,
			boundedCeiling("project.read", "gcp_service_account.assign", "agent.create", "secret.deliver"), provAgent)
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, c)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingBounded, got.Ceiling.Kind)
		assert.Equal(t, []string{"agent.create", "gcp_service_account.assign", "project.read", "secret.deliver"}, got.Ceiling.PermissionIDs)
		assert.Equal(t, 1, got.UnrecordedHops)
		assert.Nil(t, deliverIDsAllowedByChain(got), "a bounded delivery ID is withheld while a hop is unrecorded")
	})

	t.Run("unrecorded hop above a principal hop folds to unrecorded", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-up")
		p := f.agent(t, "fold-up-p", AgentRoleFull)
		c := f.agent(t, "fold-up-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, p.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID, ceilPrincip, provAgent)
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, c)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingUnrecorded, got.Ceiling.Kind, "not principal")
		assert.Equal(t, 1, got.UnrecordedHops)
		assert.Nil(t, deliverIDsAllowedByChain(got))
	})

	t.Run("missing own edge before and after backfill", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-m")
		a := f.agent(t, "fold-m", AgentRoleFull)
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingUnrecorded, got.Ceiling.Kind)
		assert.Equal(t, 1, got.UnrecordedHops)

		markEdgeBackfillComplete(t, f.store)
		_, err = f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		assert.ErrorIs(t, err, ErrProvenanceMissing)
	})

	t.Run("missing intermediate edge", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-mi")
		p := f.agent(t, "fold-mi-p", AgentRoleFull)
		c := f.agent(t, "fold-mi-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID, ceilPrincip, provAgent)
		_, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, c)
		assert.ErrorIs(t, err, ErrProvenanceMissing)
	})

	t.Run("migration sentinel is an unrecorded terminal hop", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-s")
		a := f.agent(t, "fold-s", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, migrationDelegatorID, a.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingUnrecorded, got.Ceiling.Kind)
		assert.Equal(t, 1, got.UnrecordedHops)
	})

	t.Run("ambiguous hop", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-amb")
		p := f.agent(t, "fold-amb-p", AgentRoleFull)
		c := f.agent(t, "fold-amb-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, p.ID, ceilPrincip, provSession)
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID, ceilPrincip, provAgent)
		_, err := f.authz(&ambiguousEdgeStore{Store: f.store, dupID: p.ID}, false, false).chainEffectCeiling(ctx, c)
		assert.ErrorIs(t, err, ErrProvenanceAmbiguous)
		cause, ok := ceilingDenyCauseForError(err)
		assert.True(t, ok)
		assert.Equal(t, DenyCauseCeilingOrphaned, cause)
	})

	t.Run("cycle", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-cyc")
		a := f.agent(t, "fold-cyc-a", AgentRoleFull)
		b := f.agent(t, "fold-cyc-b", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalAgent, b.ID, a.ID, ceilPrincip, provAgent)
		f.edge(t, store.DelegationPrincipalAgent, a.ID, b.ID, ceilPrincip, provAgent)
		_, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		assert.ErrorIs(t, err, ErrProvenanceChain)
	})

	t.Run("depth exceeded", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-depth")
		agents := make([]*store.Agent, maxDelegationDepth+2)
		for i := range agents {
			agents[i] = f.agent(t, fmt.Sprintf("fold-depth-%02d", i), AgentRoleFull)
		}
		f.edge(t, store.DelegationPrincipalUser, f.userID, agents[0].ID, ceilPrincip, provSession)
		for i := 1; i < len(agents); i++ {
			f.edge(t, store.DelegationPrincipalAgent, agents[i-1].ID, agents[i].ID, ceilPrincip, provAgent)
		}
		_, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, agents[len(agents)-1])
		assert.ErrorIs(t, err, ErrProvenanceChain)
		// The longest permitted chain resolves.
		got, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, agents[maxDelegationDepth])
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingPrincipal, got.Ceiling.Kind)
	})

	t.Run("lookup error is not structural", func(t *testing.T) {
		f := newCeilingFixture(t, "fold-err")
		a := f.agent(t, "fold-err", AgentRoleFull)
		_, err := f.authz(&edgeLookupErrStore{Store: f.store, failID: a.ID}, false, false).chainEffectCeiling(ctx, a)
		require.Error(t, err)
		assert.False(t, isStructuralProvenanceError(err))
	})
}

func TestChainEffectCeilingDevLocalHop(t *testing.T) {
	ctx := context.Background()

	t.Run("dev auth enabled and dev user active", func(t *testing.T) {
		f := newCeilingFixture(t, "dl-ok")
		a := f.agent(t, "dl-ok", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, ceilPrincip, provDevLocal)
		got, err := f.authz(f.store, true, false).chainEffectCeiling(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingPrincipal, got.Ceiling.Kind)
	})

	t.Run("dev auth disabled", func(t *testing.T) {
		f := newCeilingFixture(t, "dl-off")
		a := f.agent(t, "dl-off", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, ceilPrincip, provDevLocal)
		_, err := f.authz(f.store, false, false).chainEffectCeiling(ctx, a)
		assert.ErrorIs(t, err, errSourceNotAllowed)
	})

	t.Run("delegator is not the dev user", func(t *testing.T) {
		f := newCeilingFixture(t, "dl-other")
		a := f.agent(t, "dl-other", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provDevLocal)
		_, err := f.authz(f.store, true, false).chainEffectCeiling(ctx, a)
		assert.ErrorIs(t, err, errSourceNotAllowed)
	})

	t.Run("dev user suspended", func(t *testing.T) {
		f := newCeilingFixture(t, "dl-susp")
		a := f.agent(t, "dl-susp", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, ceilPrincip, provDevLocal)
		u, err := f.store.GetUser(ctx, DevUserID)
		require.NoError(t, err)
		u.Status = "suspended"
		require.NoError(t, f.store.UpdateUser(ctx, u))
		_, err = f.authz(f.store, true, false).chainEffectCeiling(ctx, a)
		assert.ErrorIs(t, err, errSourceNotAllowed)
	})

	t.Run("dev user lookup fault", func(t *testing.T) {
		f := newCeilingFixture(t, "dl-err")
		a := f.agent(t, "dl-err", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, ceilPrincip, provDevLocal)
		_, err := f.authz(&getUserErrStore{Store: f.store, failID: DevUserID}, true, false).chainEffectCeiling(ctx, a)
		require.Error(t, err)
		assert.False(t, isStructuralProvenanceError(err))
	})
}

// A structural chain outcome gives no delivery IDs and no error; a lookup
// fault is returned.
func TestParentDeliverEligibilityStructuralGivesNone(t *testing.T) {
	ctx := context.Background()
	f := newCeilingFixture(t, "pde")
	p := f.agent(t, "pde-p", AgentRoleFull)
	markEdgeBackfillComplete(t, f.store)

	ids, err := f.authz(f.store, false, false).parentDeliverEligibility(ctx, p)
	require.NoError(t, err, "missing edge after backfill")
	assert.Nil(t, ids)

	f.edge(t, store.DelegationPrincipalUser, DevUserID, p.ID, ceilPrincip, provDevLocal)
	ids, err = f.authz(f.store, false, false).parentDeliverEligibility(ctx, p)
	require.NoError(t, err, "dev_local hop with dev auth disabled")
	assert.Nil(t, ids)

	ids, err = f.authz(&ambiguousEdgeStore{Store: f.store, dupID: p.ID}, true, false).parentDeliverEligibility(ctx, p)
	require.NoError(t, err, "ambiguous hop")
	assert.Nil(t, ids)

	ids, err = f.authz(f.store, true, false).parentDeliverEligibility(ctx, p)
	require.NoError(t, err)
	assert.ElementsMatch(t, hubDeliveryPermissionList, ids, "principal chain carries every delivery ID")

	_, err = f.authz(&edgeLookupErrStore{Store: f.store, failID: p.ID}, true, false).parentDeliverEligibility(ctx, p)
	assert.Error(t, err, "lookup fault is returned")
}

// The mint candidate scopes follow the role, the config-derived GCP scope,
// and the dev-auth override; the ceiling filter narrows them.
func TestMintCandidateScopesAndFilter(t *testing.T) {
	ctx := context.Background()
	f := newCeilingFixture(t, "mint")
	a := f.agent(t, "mint", AgentRoleNone)

	assert.Empty(t, f.authz(f.store, false, false).mintCandidateScopes(a))
	assert.Equal(t, ScopesForRole(AgentRoleFull), f.authz(f.store, false, true).mintCandidateScopes(a))

	a.AppliedConfig.AgentRole = string(AgentRoleReadOnly)
	a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: "sa-1"}
	assert.Equal(t, []AgentTokenScope{ScopeProjectRead, ScopeProjectArtifactRead, GCPTokenScopeForSA("sa-1")}, f.authz(f.store, false, false).mintCandidateScopes(a))

	readCoverage := agentScopeCoverage([]AgentTokenScope{ScopeProjectRead})
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, boundedCeiling(append(readCoverage, "agent.create")...), provSession)
	got, err := f.authz(f.store, false, true).ceilingFilteredAgentScopes(ctx, a, f.authz(f.store, false, true).mintCandidateScopes(a))
	require.NoError(t, err)
	assert.Equal(t, []AgentTokenScope{ScopeProjectRead, ScopeAgentStatusUpdate, ScopeAgentTokenRefresh, ScopeAgentNotify, ScopeAgentPortForward, ScopeAgentCreate}, got,
		"the dev-auth full override is narrowed by the bounded ceiling; the GCP scope needs gcp_service_account.assign; project:artifact:read needs artifact.read")

	// project:read needs every permission it covers.
	got = filterScopes([]AgentTokenScope{ScopeProjectRead}, boundedCeiling("project.read"), ScopeCeilings{})
	assert.Empty(t, got, "a ceiling holding only part of a scope's coverage withholds the scope")
}

// The dev-auth mint override defaults to off: neither NewAuthzService nor a
// bare AuthzService raises a role-none agent.
func TestMintDevAuthOverrideDefaultsOff(t *testing.T) {
	f := newCeilingFixture(t, "mint-default")
	a := f.agent(t, "mint-default", AgentRoleNone)

	svc := NewAuthzService(f.store, slog.Default())
	assert.False(t, svc.mintDevAuthOverride)
	assert.Empty(t, svc.mintCandidateScopes(a), "NewAuthzService")
	assert.Empty(t, (&AuthzService{}).mintCandidateScopes(a), "bare AuthzService")
}

// New wires the dev-auth mint override from ServerConfig.DevAuthToken: off
// when the token is empty, on when it is set.
func TestNewWiresMintDevAuthOverrideFromDevAuthToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		want  bool
	}{
		{name: "dev auth off", token: "", want: false},
		{name: "dev auth on", token: "dev-token-value", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newTestStore(t, ":memory:")
			require.NoError(t, err)
			require.NoError(t, s.Migrate(context.Background()))

			srv, err := New(ServerConfig{DevAuthToken: tc.token}, s)
			require.NoError(t, err)
			assert.Equal(t, tc.want, srv.authzService.mintDevAuthOverride)

			a := &store.Agent{ID: tid("wiring-agent"), AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleNone)}}
			if tc.want {
				assert.Equal(t, ScopesForRole(AgentRoleFull), srv.authzService.mintCandidateScopes(a))
			} else {
				assert.Empty(t, srv.authzService.mintCandidateScopes(a))
			}
		})
	}
}

// sourceEffectCeiling rows at the unit level.
func TestSourceEffectCeilingRows(t *testing.T) {
	ctx := context.Background()
	f := newCeilingFixture(t, "src")
	authz := f.authz(f.store, true, false)

	t.Run("session", func(t *testing.T) {
		user := NewAuthenticatedUser(f.userID, "u@test.com", "U", "member", "web")
		c, p, err := authz.sourceEffectCeiling(ctx, user)
		require.NoError(t, err)
		assert.Equal(t, ceilPrincip, c)
		assert.Equal(t, store.SourceCredentialSession, p.SourceCredentialKind)
		assert.Equal(t, f.userID, p.SourcePrincipalID)
		assert.Equal(t, store.ProvenanceVersionV1, p.ProvenanceVersion)
	})

	t.Run("dev user", func(t *testing.T) {
		c, p, err := authz.sourceEffectCeiling(ctx, NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev"}))
		require.NoError(t, err)
		assert.Equal(t, ceilPrincip, c)
		assert.Equal(t, store.SourceCredentialDevLocal, p.SourceCredentialKind)
		assert.Equal(t, DevUserID, p.SourcePrincipalID)
	})

	t.Run("uat copies version and ids", func(t *testing.T) {
		user := NewAuthenticatedUser(f.userID, "u@test.com", "U", "member", "api")
		for _, v := range []permissions.CeilingVersion{permissions.CeilingVersionUnspecified, permissions.CeilingVersionV1} {
			uat := NewScopedUserIdentityWithCeiling(user, f.projectID, nil, "uat-1",
				permissions.FrozenPermissionCeiling{Version: v, PermissionIDs: []string{"project.read", "agent.create"}})
			c, p, err := authz.sourceEffectCeiling(ctx, uat)
			require.NoError(t, err)
			assert.Equal(t, store.EffectCeilingBounded, c.Kind)
			assert.Equal(t, v, c.Version)
			assert.Equal(t, []string{"project.read", "agent.create"}, c.PermissionIDs)
			assert.Equal(t, f.projectID, c.BoundaryProjectID)
			assert.Equal(t, store.SourceCredentialUAT, p.SourceCredentialKind)
			assert.Equal(t, "uat-1", p.SourceCredentialID)
		}
		uat := NewScopedUserIdentityWithCeiling(user, f.projectID, nil, "uat-2", permissions.FrozenPermissionCeiling{Version: 99})
		_, _, err := authz.sourceEffectCeiling(ctx, uat)
		assert.ErrorIs(t, err, errSourceCeilingUnrecorded)
		cause, _ := ceilingDenyCauseForError(err)
		assert.Equal(t, DenyCauseCeilingUnrecorded, cause)
	})

	t.Run("agent parent", func(t *testing.T) {
		p := f.agent(t, "src-p", AgentRoleBaseline)
		f.edge(t, store.DelegationPrincipalUser, f.userID, p.ID, ceilPrincip, provSession)
		w := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: p.ID, ID: "jti-1"}, ProjectID: f.projectID}}
		c, prov, err := authz.sourceEffectCeiling(ctx, w)
		require.NoError(t, err)
		want := append(agentScopeCoverage(ScopesForRole(AgentRoleBaseline)), hubDeliveryPermissionList...)
		sort.Strings(want)
		assert.Equal(t, store.EffectCeilingBounded, c.Kind)
		assert.Equal(t, permissions.CeilingVersionV1, c.Version)
		assert.Equal(t, want, c.PermissionIDs)
		assert.Equal(t, store.SourceCredentialAgent, prov.SourceCredentialKind)
		assert.Equal(t, "jti-1", prov.SourceCredentialID)
		assert.Equal(t, p.ID, prov.SourcePrincipalID)
	})

	t.Run("agent parent without edge", func(t *testing.T) {
		g := newCeilingFixture(t, "src-ne")
		p := g.agent(t, "src-ne-p", AgentRoleReadOnly)
		w := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: p.ID}, ProjectID: g.projectID}}
		c, _, err := g.authz(g.store, true, false).sourceEffectCeiling(ctx, w)
		require.NoError(t, err)
		// An agent without an edge is an unrecorded chain: its role coverage,
		// less the permissions withheld from unrecorded chains, and no
		// delivery IDs.
		var want []string
		for _, id := range agentScopeCoverage(ScopesForRole(AgentRoleReadOnly)) {
			if !legacyChainExcludedPermissions[id] {
				want = append(want, id)
			}
		}
		assert.Equal(t, want, c.PermissionIDs, "coverage only, no delivery IDs, no withheld permissions")

		markEdgeBackfillComplete(t, g.store)
		_, _, err = g.authz(g.store, true, false).sourceEffectCeiling(ctx, w)
		assert.ErrorIs(t, err, ErrProvenanceMissing)
	})

	t.Run("agent parent not found", func(t *testing.T) {
		w := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: tid("src-missing")}, ProjectID: f.projectID}}
		_, _, err := authz.sourceEffectCeiling(ctx, w)
		assert.ErrorIs(t, err, ErrProvenanceChain)
	})

	t.Run("identities that are not authority sources", func(t *testing.T) {
		var nilUser *AuthenticatedUser
		var nilWrapper *agentIdentityWrapper
		var nilDev *DevUser
		cases := map[string]Identity{
			"nil":                  nil,
			"typed-nil user":       nilUser,
			"typed-nil wrapper":    nilWrapper,
			"typed-nil dev user":   nilDev,
			"wrapper nil claims":   &agentIdentityWrapper{},
			"stored agent":         &storedAgentIdentity{},
			"peer agent":           &peerAgentIdentity{},
			"explain agent":        &explainAgentIdentity{},
			"dev user with bad id": &DevUser{id: "not-the-dev-user"},
		}
		for name, id := range cases {
			_, _, err := authz.sourceEffectCeiling(ctx, id)
			assert.ErrorIs(t, err, errSourceNotAllowed, name)
		}
	})
}
