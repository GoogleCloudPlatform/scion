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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Legacy adopted-agent fallback for an owner's user secrets
// (legacyAdoptedSecretUseFallback).

// lafV1 is role's CompatibilityPolicyV1 row.
func lafV1(t *testing.T, role AgentRole, withSA bool) []string {
	t.Helper()
	ids, ok := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, string(role), withSA)
	require.True(t, ok)
	return ids
}

// TestLegacyAdoptedSecretUseFallback_Predicate exercises the cohort
// predicate in isolation: every condition, one at a time.
func TestLegacyAdoptedSecretUseFallback_Predicate(t *testing.T) {
	mf := newMaterialFixture(t, "laf-pred")
	ctx := context.Background()
	authz := mf.Server.authzService
	owner := mf.UserID
	agent := &store.Agent{ID: uuid.NewString(), OwnerID: owner, Ancestry: []string{owner}}

	cohortEdge := func(role AgentRole) *store.DelegationEdge {
		return &store.DelegationEdge{
			ID: uuid.NewString(), DelegatorType: store.DelegationPrincipalUser, DelegatorID: owner,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: agent.ID, Role: string(role), Active: true,
			AuthorityProvenance: store.AuthorityProvenance{
				ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: store.DelegationPrincipalUser,
				SourcePrincipalID: owner, SourceCredentialKind: store.SourceCredentialSystemMigration,
			},
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingBounded, PermissionIDs: lafV1(t, role, false)},
		}
	}
	record := func(edge *store.DelegationEdge, mutate func(*store.DelegationAdoption)) {
		rec := &store.DelegationAdoption{
			CohortID: "laf-cohort", Origin: store.DelegationAdoptionOriginBoot, PolicyVersion: int(permissions.CompatibilityPolicyV1),
			OriginalEdgeID: uuid.NewString(), AdoptedEdgeID: edge.ID, DelegateID: agent.ID, Status: store.DelegationAdoptionAdopted,
		}
		if mutate != nil {
			mutate(rec)
		}
		require.NoError(t, mf.Store.CreateDelegationAdoption(ctx, rec))
	}

	cases := []struct {
		name  string
		build func() (*store.DelegationEdge, int)
		want  bool
	}{
		{"baseline adopted", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			record(e, nil)
			return e, 0
		}, true},
		{"readonly adopted", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleReadOnly)
			record(e, nil)
			return e, 0
		}, true},
		{"baseline adopted with assigned SA row", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.EffectCeiling.PermissionIDs = lafV1(t, AgentRoleBaseline, true)
			record(e, nil)
			return e, 0
		}, true},
		{"recognized record", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			record(e, func(r *store.DelegationAdoption) { r.Status = store.DelegationAdoptionRecognized })
			return e, 0
		}, true},
		{"not the agent's own edge (depth 1)", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			record(e, nil)
			return e, 1
		}, false},
		{"agent delegator", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.DelegatorType, e.DelegatorID = store.DelegationPrincipalAgent, uuid.NewString()
			record(e, nil)
			return e, 0
		}, false},
		{"delegator is not the owner", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.DelegatorID = uuid.NewString()
			record(e, nil)
			return e, 0
		}, false},
		{"recorded session source, not adoption", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.SourceCredentialKind = store.SourceCredentialSession
			record(e, nil)
			return e, 0
		}, false},
		{"principal ceiling", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.Kind = store.EffectCeilingPrincipal
			record(e, nil)
			return e, 0
		}, false},
		{"full edge role carrying the baseline row (adopted at the applied role)", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.Role = string(AgentRoleFull)
			record(e, nil)
			return e, 0
		}, true},
		{"baseline edge role carrying the readonly row", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleReadOnly)
			e.Role = string(AgentRoleBaseline)
			record(e, nil)
			return e, 0
		}, true},
		{"readonly edge role carrying the baseline row", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.Role = string(AgentRoleReadOnly)
			record(e, nil)
			return e, 0
		}, false},
		{"empty edge role", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleReadOnly)
			e.Role = ""
			record(e, nil)
			return e, 0
		}, false},
		{"full row", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.Role = string(AgentRoleFull)
			e.EffectCeiling.PermissionIDs = lafV1(t, AgentRoleFull, false)
			record(e, nil)
			return e, 0
		}, false},
		{"ceiling narrower than the V1 row", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.EffectCeiling.PermissionIDs = e.EffectCeiling.PermissionIDs[1:]
			record(e, nil)
			return e, 0
		}, false},
		{"ceiling with an extra permission", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			e.EffectCeiling.PermissionIDs = append(append([]string{}, e.EffectCeiling.PermissionIDs...), "zz.extra")
			record(e, nil)
			return e, 0
		}, false},
		{"no adoption record", func() (*store.DelegationEdge, int) {
			return cohortEdge(AgentRoleBaseline), 0
		}, false},
		{"record under another policy version", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			record(e, func(r *store.DelegationAdoption) { r.PolicyVersion = 2 })
			return e, 0
		}, false},
		{"reverted record", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			record(e, func(r *store.DelegationAdoption) {
				now := time.Now()
				r.Status = store.DelegationAdoptionReverted
				r.RevertedByKind, r.RevertedByID, r.RevertedAt = "user", uuid.NewString(), &now
			})
			return e, 0
		}, false},
		{"only an excluded record for the agent (names no adopted edge)", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			record(e, func(r *store.DelegationAdoption) {
				r.Status = store.DelegationAdoptionExcluded
				r.Reason = "role_none"
				r.AdoptedEdgeID = ""
			})
			return e, 0
		}, false},
		{"record for another agent", func() (*store.DelegationEdge, int) {
			e := cohortEdge(AgentRoleBaseline)
			record(e, func(r *store.DelegationAdoption) { r.DelegateID = uuid.NewString() })
			return e, 0
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edge, depth := tc.build()
			fb, err := authz.legacyAdoptedSecretUseFallback(ctx, agent, edge, depth)
			require.NoError(t, err)
			if tc.want {
				require.NotNil(t, fb)
				rowRole, ok := legacyCompatibilityV1RowRole(edge.EffectCeiling.PermissionIDs)
				require.True(t, ok)
				assert.Equal(t, string(rowRole), fb.Role, "the cohort role is the carried row's role")
				assert.Equal(t, edge.ID, fb.EdgeID)
			} else {
				assert.Nil(t, fb)
			}
		})
	}

	t.Run("lookup error", func(t *testing.T) {
		e := cohortEdge(AgentRoleBaseline)
		failing := &AuthzService{store: &lafAdoptionErrStore{Store: mf.Store}}
		fb, err := failing.legacyAdoptedSecretUseFallback(ctx, agent, e, 0)
		require.Error(t, err)
		assert.Nil(t, fb)
	})
}

type lafAdoptionErrStore struct{ store.Store }

func (lafAdoptionErrStore) ListDelegationAdoptions(context.Context, store.DelegationAdoptionFilter) ([]*store.DelegationAdoption, int, error) {
	return nil, 0, errors.New("fake adoption lookup failure")
}

// lafFixture is the start delivery fixture whose owner shares the three
// sd* secrets, with helpers to seed legacy agents and adopt them.
type lafFixture struct {
	*sdFixture
}

func newLAFFixture(t *testing.T, name string) *lafFixture {
	t.Helper()
	base := &userScopeCeilingFixture{materialFixture: newMaterialFixture(t, name)}
	sdShareOwnerSecrets(t, base.Server.secretBackend, base.UserID)
	// Model production: the test server's dev-auth mint override raises
	// every agent token to the full role's scopes.
	base.Server.authzService.mintDevAuthOverride = false
	return &lafFixture{sdFixture: &sdFixture{userScopeCeilingFixture: base, disp: sdDispatcher(base.Server, base.Store)}}
}

// legacy stores a legacy agent of role directly under the owner, with an
// edge that carries no provenance (adoption turns it into a V1 edge).
func (f *lafFixture) legacy(t *testing.T, name string, role AgentRole) string {
	t.Helper()
	return f.legacyMixed(t, name, role, role)
}

// legacyMixed is legacy with an edge role different from the agent's
// applied role (adoption ceilings at the lower of the two).
func (f *lafFixture) legacyMixed(t *testing.T, name string, edgeRole, appliedRole AgentRole) string {
	t.Helper()
	id := tid("laf-" + name)
	createDCAgent(t, f.Store, id, f.ProjectID, f.UserID, appliedRole)
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, id,
		store.RoleScopeProject, f.ProjectID, string(edgeRole))
	return id
}

// token issues the agent's token from its stored row (its role's scopes).
func (f *lafFixture) token(t *testing.T, agentID string) string {
	t.Helper()
	a, err := f.Store.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	tok, err := f.Server.issueAgentTokenForTest(context.Background(), a)
	require.NoError(t, err)
	return tok
}

// delivery is what each path gives the agent of the owner's shared,
// always-injected secret: start (resolveSecrets), restart (a real
// DispatchAgentRestart), the runtime read and the agent secret list.
// readCode and listCode are the HTTP statuses of the read and the list.
type lafDelivery struct {
	start, restart, read, list bool
	readCode, listCode         int
}

func (f *lafFixture) delivery(t *testing.T, agentID string) lafDelivery {
	t.Helper()
	tok := f.token(t, agentID)
	got := usaDispatch(t, f.sdFixture, agentID)
	listCode, keys := f.listKeys(t, agentID, tok, "user")
	readCode, _ := f.readAs(t, agentID, tok, sdAlwaysKey)
	return lafDelivery{
		start:    sdStartDelivery(t, f.disp, f.Store, agentID).always,
		restart:  got.restart,
		read:     readCode == http.StatusOK,
		list:     containsString(keys, sdAlwaysKey),
		readCode: readCode,
		listCode: listCode,
	}
}

// assertCohortDelivery checks the cohort's delivery: start and restart
// deliver; the runtime read and the list are refused at the precheck.
func assertCohortDelivery(t *testing.T, got lafDelivery, msg string) {
	t.Helper()
	assert.True(t, got.start, "%s: start delivers", msg)
	assert.True(t, got.restart, "%s: restart delivers", msg)
	assert.False(t, got.read, "%s: the runtime read stays refused", msg)
	assert.Equal(t, http.StatusForbidden, got.readCode, "%s: capability_required", msg)
	assert.False(t, got.list, "%s: the list stays refused", msg)
	assert.Equal(t, http.StatusForbidden, got.listCode, "%s: capability_required", msg)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestLegacyAdoptedFallback_CohortDeliversOwnerSecrets: adopted baseline and
// readonly agents receive their owner's shared secret at start and restart;
// each use is counted and logged at Warn with the cohort and the secret
// name, never the value. The runtime read and the list stay refused with
// capability_required: their precheck has required project:secret:read
// since #2072, and these tokens never carry it.
func TestLegacyAdoptedFallback_CohortDeliversOwnerSecrets(t *testing.T) {
	f := newLAFFixture(t, "laf-cohort")
	b := f.legacy(t, "b", AgentRoleBaseline)
	r := f.legacy(t, "r", AgentRoleReadOnly)
	runBootAdoption(t, f.Store)
	for _, id := range []string{b, r} {
		edges := activeEdgesFor(t, f.Store, id)
		require.Len(t, edges, 1)
		require.Equal(t, store.SourceCredentialSystemMigration, edges[0].SourceCredentialKind, "the edge is adopted")
	}
	log := usaCaptureDefaultLog(t)
	before := f.Server.authzService.LegacyAdoptedFallbackUses()

	for _, tc := range []struct {
		id, cohort string
	}{{b, "adopted_baseline"}, {r, "adopted_readonly"}} {
		d := f.ceiling(t, tc.id, sdAlwaysKey)
		require.True(t, d.Allowed, "the ceiling allows the owner's secret under the fallback; reason %q", d.Reason)
		require.NotNil(t, d.legacyFallback)
		assertCohortDelivery(t, f.delivery(t, tc.id), tc.cohort)
		assert.Contains(t, log.String(), "cohort="+tc.cohort)
	}
	assert.Contains(t, log.String(), "legacy adopted-agent fallback")
	assert.Contains(t, log.String(), "secret="+sdAlwaysKey)
	assert.NotContains(t, log.String(), sdAlwaysValue, "the Warn never carries the value")
	assert.Greater(t, f.Server.authzService.LegacyAdoptedFallbackUses(), before, "every use is counted")
}

// TestLegacyAdoptedFallback_FullAdoptedNotCounted: an adopted full agent
// passes S3c on its own; the fallback is not used or counted.
func TestLegacyAdoptedFallback_FullAdoptedNotCounted(t *testing.T) {
	f := newLAFFixture(t, "laf-full")
	full := f.legacy(t, "full", AgentRoleFull)
	runBootAdoption(t, f.Store)
	before := f.Server.authzService.LegacyAdoptedFallbackUses()
	d := f.ceiling(t, full, sdAlwaysKey)
	require.True(t, d.Allowed, d.Reason)
	assert.Nil(t, d.legacyFallback)
	got := f.delivery(t, full)
	assert.True(t, got.start && got.restart && got.read && got.list, "a full adopted agent keeps every path: %+v", got)
	assert.Equal(t, before, f.Server.authzService.LegacyAdoptedFallbackUses())
}

// TestLegacyAdoptedFallback_HeldAgentStillRefused: a held cohort agent
// keeps the S2 denial on every path.
func TestLegacyAdoptedFallback_HeldAgentStillRefused(t *testing.T) {
	f := newLAFFixture(t, "laf-held")
	b := f.legacy(t, "held", AgentRoleBaseline)
	runBootAdoption(t, f.Store)
	tok := f.token(t, b)
	_, err := f.Store.CreateAgentHolds(context.Background(), []*store.AgentHold{{
		AgentID: b, ProjectID: f.ProjectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: f.UserID,
		Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "test",
	}})
	require.NoError(t, err)
	d := f.ceiling(t, b, sdAlwaysKey)
	require.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingOrphaned, d.DenyCause)
	assert.False(t, sdStartDelivery(t, f.disp, f.Store, b).always)
	assert.False(t, f.read(t, b, tok, sdAlwaysKey))
	_, keys := f.listKeys(t, b, tok, "user")
	assert.NotContains(t, keys, sdAlwaysKey)
}

// TestLegacyAdoptedFallback_AgentDelegatedChildStillRefused: an adopted
// baseline child of an adopted agent is not the owner's own edge; S3c still
// denies it (and the parent rule S3d would apply above it).
func TestLegacyAdoptedFallback_AgentDelegatedChildStillRefused(t *testing.T) {
	f := newLAFFixture(t, "laf-child")
	parent := f.legacy(t, "parent", AgentRoleFull)
	child := tid("laf-child-c")
	require.NoError(t, f.Store.CreateAgent(context.Background(), &store.Agent{
		ID: child, Slug: "slug-" + child[:8], Name: "name-" + child[:8], ProjectID: f.ProjectID, Phase: "running",
		CreatedBy: parent, OwnerID: parent, Ancestry: []string{f.UserID, parent},
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleBaseline)},
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalAgent, parent, store.DelegationPrincipalAgent, child,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleBaseline))
	runBootAdoption(t, f.Store)
	d := f.ceiling(t, child, sdAlwaysKey)
	require.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause)
	assert.False(t, sdStartDelivery(t, f.disp, f.Store, child).always)
	assert.False(t, f.read(t, child, f.token(t, child), sdAlwaysKey))
}

// TestLegacyAdoptedFallback_NonCohortBaselineStillRefusedAtRead: a baseline
// agent outside the cohort keeps every refusal. A recorded baseline agent
// (principal edge, as a user's create writes today) passes the ceiling, so
// start delivers as before #4234, but the runtime read is still refused at
// its precheck: its role token lacks project:secret:read. A baseline
// agent with a bounded V1-row ceiling from a session source (not adoption)
// is refused by S3c everywhere.
func TestLegacyAdoptedFallback_NonCohortBaselineStillRefusedAtRead(t *testing.T) {
	f := newLAFFixture(t, "laf-noncohort")
	setBackfillCompleted(t, f.Store)
	recorded := tid("laf-noncohort-recorded")
	createDCAgent(t, f.Store, recorded, f.ProjectID, f.UserID, AgentRoleBaseline)
	seedRecordedDelegationEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, recorded,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleBaseline))

	bounded := tid("laf-noncohort-bounded")
	createDCAgent(t, f.Store, bounded, f.ProjectID, f.UserID, AgentRoleBaseline)
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, bounded,
		store.EffectCeiling{Kind: store.EffectCeilingBounded, PermissionIDs: lafV1(t, AgentRoleBaseline, false)},
		func(e *store.DelegationEdge) { e.Role = string(AgentRoleBaseline) })

	before := f.Server.authzService.LegacyAdoptedFallbackUses()

	tok := f.token(t, recorded)
	code, _ := f.readAs(t, recorded, tok, sdAlwaysKey)
	assert.Equal(t, http.StatusForbidden, code, "a baseline token is refused at the runtime read precheck (project:secret:read)")
	assert.True(t, sdStartDelivery(t, f.disp, f.Store, recorded).always, "start is unchanged for a recorded principal edge")

	d := f.ceiling(t, bounded, sdAlwaysKey)
	require.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause)
	assert.False(t, sdStartDelivery(t, f.disp, f.Store, bounded).always)
	assert.False(t, f.read(t, bounded, f.token(t, bounded), sdAlwaysKey))

	assert.Equal(t, before, f.Server.authzService.LegacyAdoptedFallbackUses(), "no use outside the cohort")
}

// TestLegacyAdoptedFallback_OnlyOwnersOwnSecrets: the fallback does not
// lift the source check. A secret the owner shares but did not author
// (S7) stays withheld from the cohort agent.
func TestLegacyAdoptedFallback_OnlyOwnersOwnSecrets(t *testing.T) {
	f := newLAFFixture(t, "laf-own")
	b := f.legacy(t, "own", AgentRoleBaseline)
	runBootAdoption(t, f.Store)
	f.shareUserSecret(t, f.UserID, "LAF_FOREIGN_AUTHOR", "agent:"+uuid.NewString())
	meta, err := f.Server.secretBackend.GetMeta(context.Background(), "LAF_FOREIGN_AUTHOR", store.ScopeUser, f.UserID)
	require.NoError(t, err)
	agent, err := f.Store.GetAgent(context.Background(), b)
	require.NoError(t, err)
	ident := newFullAgentIdentity(b, f.ProjectID, agent.Ancestry, nil)
	d := f.Server.authzService.EvaluateUserSecretParentCeiling(context.Background(), ident, userSecretResource(meta.ID, meta.ScopeID))
	require.False(t, d.Allowed)
	assert.Nil(t, d.legacyFallback)
	assert.False(t, f.read(t, b, f.token(t, b), "LAF_FOREIGN_AUTHOR"))
	_, keys := f.listKeys(t, b, f.token(t, b), "user")
	assert.NotContains(t, keys, "LAF_FOREIGN_AUTHOR")
	assert.True(t, strings.HasPrefix(meta.CreatedBy, "agent:"))
}

// TestLegacyAdoptedFallback_MixedRoleCohorts: adoption keeps the edge's role
// but ceilings it at the lower of that role and the agent's applied role.
// An edge "full" adopted at the baseline row (applied baseline) and an edge
// "baseline" adopted at the readonly row (applied readonly) are in the
// cohort, reported under the row's role.
func TestLegacyAdoptedFallback_MixedRoleCohorts(t *testing.T) {
	f := newLAFFixture(t, "laf-mixed")
	fullAtBaseline := f.legacyMixed(t, "full-b", AgentRoleFull, AgentRoleBaseline)
	baselineAtReadonly := f.legacyMixed(t, "base-r", AgentRoleBaseline, AgentRoleReadOnly)
	runBootAdoption(t, f.Store)
	log := usaCaptureDefaultLog(t)
	for _, tc := range []struct {
		id, edgeRole, rowRole string
	}{
		{fullAtBaseline, string(AgentRoleFull), string(AgentRoleBaseline)},
		{baselineAtReadonly, string(AgentRoleBaseline), string(AgentRoleReadOnly)},
	} {
		edges := activeEdgesFor(t, f.Store, tc.id)
		require.Len(t, edges, 1)
		require.Equal(t, store.SourceCredentialSystemMigration, edges[0].SourceCredentialKind)
		require.Equal(t, tc.edgeRole, edges[0].Role, "adoption keeps the edge's role")
		require.Equal(t, lafV1(t, AgentRole(tc.rowRole), false), edges[0].EffectCeiling.PermissionIDs, "adoption ceilings at the lower role's row")
		d := f.ceiling(t, tc.id, sdAlwaysKey)
		require.True(t, d.Allowed, d.Reason)
		require.NotNil(t, d.legacyFallback)
		assert.Equal(t, tc.rowRole, d.legacyFallback.Role)
		assertCohortDelivery(t, f.delivery(t, tc.id), tc.edgeRole+" at "+tc.rowRole)
		assert.Contains(t, log.String(), "cohort=adopted_"+tc.rowRole)
	}
}

// lafRecordingMetrics records (path, role) of each fallback use.
type lafRecordingMetrics struct{ uses []string }

func (m *lafRecordingMetrics) RecordLegacyAdoptedFallback(_ context.Context, path, role string) {
	m.uses = append(m.uses, path+"/"+role)
}

// TestLegacyAdoptedFallback_MetricAttributes: only start and restart (the
// owner path, path "start") record a use, with the cohort role; the runtime
// read and the list record none.
func TestLegacyAdoptedFallback_MetricAttributes(t *testing.T) {
	f := newLAFFixture(t, "laf-metrics")
	b := f.legacy(t, "metrics", AgentRoleBaseline)
	runBootAdoption(t, f.Store)
	m := &lafRecordingMetrics{}
	f.Server.SetLegacyAdoptedFallbackMetrics(m)
	f.delivery(t, b)
	require.NotEmpty(t, m.uses)
	for _, u := range m.uses {
		assert.Equal(t, "start/baseline", u)
	}
}

// TestLegacyAdoptedFallback_CohortStillRefusedOnS2S4S4a: the fallback lifts
// S3c only. A deleted cohort agent (S2), an inactive owner (S4) and an owner
// no longer admitted to the project (S4a) are refused at start, runtime read
// and list.
func TestLegacyAdoptedFallback_CohortStillRefusedOnS2S4S4a(t *testing.T) {
	cases := []struct {
		name    string
		cause   DenyCause
		breakIt func(t *testing.T, f *lafFixture, agentID string)
	}{
		{"deleted agent (S2)", DenyCauseCeilingOrphaned, func(t *testing.T, f *lafFixture, agentID string) {
			a, err := f.Store.GetAgent(context.Background(), agentID)
			require.NoError(t, err)
			a.DeletedAt = time.Now()
			require.NoError(t, f.Store.UpdateAgent(context.Background(), a))
		}},
		{"inactive owner (S4)", DenyCauseCeilingOrphaned, func(t *testing.T, f *lafFixture, _ string) {
			u, err := f.Store.GetUser(context.Background(), f.UserID)
			require.NoError(t, err)
			u.Status = "suspended"
			require.NoError(t, f.Store.UpdateUser(context.Background(), u))
		}},
		{"owner not admitted to the project (S4a)", DenyCauseCeilingDelegatorLacksPermission, func(t *testing.T, f *lafFixture, _ string) {
			rbs, err := f.Store.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, f.UserID)
			require.NoError(t, err)
			for _, rb := range rbs {
				if rb.ScopeType == store.RoleScopeProject && rb.ScopeID == f.ProjectID {
					require.NoError(t, f.Store.DeleteRoleBinding(context.Background(), rb.ID))
				}
			}
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLAFFixture(t, "laf-s2s4-"+string(rune('a'+i)))
			b := f.legacy(t, "cohort", AgentRoleBaseline)
			runBootAdoption(t, f.Store)
			tok := f.token(t, b)
			require.True(t, f.ceiling(t, b, sdAlwaysKey).Allowed, "in the cohort before the change")

			tc.breakIt(t, f, b)
			before := f.Server.authzService.LegacyAdoptedFallbackUses()
			d := f.ceiling(t, b, sdAlwaysKey)
			require.False(t, d.Allowed)
			assert.Equal(t, tc.cause, d.DenyCause, d.Reason)
			if tc.name != "deleted agent (S2)" {
				assert.False(t, sdStartDelivery(t, f.disp, f.Store, b).always)
			}
			assert.False(t, f.read(t, b, tok, sdAlwaysKey))
			_, keys := f.listKeys(t, b, tok, "user")
			assert.NotContains(t, keys, sdAlwaysKey)
			assert.Equal(t, before, f.Server.authzService.LegacyAdoptedFallbackUses())
		})
	}
}
