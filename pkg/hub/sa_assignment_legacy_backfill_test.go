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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// slbAgent stores an assign-mode agent for the fixture account with the
// given ancestry and no delegation edge and no assignment row.
func slbAgent(t *testing.T, f *spcFixture, name string, ancestry ...string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(name), Slug: name, Name: name, ProjectID: f.projectID,
		Phase: "running", CreatedBy: ancestry[len(ancestry)-1], OwnerID: ancestry[0], Ancestry: ancestry,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(AgentRoleFull),
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: f.sa.ID,
			},
		},
	}
	require.NoError(t, f.s.CreateAgent(context.Background(), a))
	return a
}

// slbEdge stores an active project edge to delegate with the given
// provenance and ceiling.
func slbEdge(t *testing.T, f *spcFixture, delegatorType, delegatorID, delegateID string, prov store.AuthorityProvenance, ceiling store.EffectCeiling) {
	t.Helper()
	require.NoError(t, f.s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: delegateID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
		Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: prov, EffectCeiling: ceiling,
	}))
}

func adoptedProvenance(kind, id string) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: kind, SourcePrincipalID: id,
		SourceCredentialKind: store.SourceCredentialSystemMigration,
	}
}

func runLegacyBackfill(t *testing.T, f *spcFixture) SAAssignmentLegacyBackfillResult {
	t.Helper()
	res, err := BackfillLegacySAAssignments(context.Background(), f.s)
	require.NoError(t, err)
	return res
}

func skipReasons(res SAAssignmentLegacyBackfillResult) map[string]string {
	out := map[string]string{}
	for _, s := range res.Skipped {
		out[s.AgentID] = s.Reason
	}
	return out
}

// A legacy agent with a recorded session edge and no row is refused until
// the backfill copies the edge into a row; then the mint decision allows.
func TestSALegacyBackfill_SessionEdgeCopiedAndAllows(t *testing.T) {
	f := newSPCFixture(t)
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceMissing)

	res := runLegacyBackfill(t, f)
	assert.Equal(t, []string{f.agent.ID}, res.Written)
	assert.Empty(t, res.Skipped)

	d := f.eval(t)
	assertParentCeilingAllowed(t, d)
	assert.Equal(t, ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: f.assigner}, d.Proof.SourcePrincipal)
	assert.Equal(t, store.SourceCredentialSession, d.Proof.SourceCredentialKind)

	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SAAssignmentOriginLegacyBackfill, rows[0].Origin)
	assert.Equal(t, f.projectID, rows[0].ProjectID)
	assert.Equal(t, f.sa.ID, rows[0].ServiceAccountID)
	assert.Equal(t, store.EffectCeilingPrincipal, rows[0].Kind, "the edge's ceiling is copied")

	// A second pass finds nothing to do.
	again := runLegacyBackfill(t, f)
	assert.Zero(t, again.Examined)
	assert.Empty(t, again.Written)
}

// An edge recorded by delegation adoption (system_migration) from an agent
// delegator yields an agent-credential row with the edge's bounded ceiling.
func TestSALegacyBackfill_AdoptedAgentEdgeAllows(t *testing.T) {
	f := newSPCFixture(t)
	parent := f.assignModeAgent(t, "slb-parent", f.sa.ID, f.assigner)
	child := slbAgent(t, f, "slb-child", f.assigner, parent.ID)
	ceiling := boundedCeiling("agent.create", "gcp_service_account.assign", "gcp_service_account.use")
	slbEdge(t, f, store.DelegationPrincipalAgent, parent.ID, child.ID,
		adoptedProvenance(store.DelegationPrincipalAgent, parent.ID), ceiling)

	res := runLegacyBackfill(t, f)
	assert.ElementsMatch(t, []string{f.agent.ID, parent.ID, child.ID}, res.Written)

	rows := activeAssignments(t, f.s, child.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.DelegationPrincipalAgent, rows[0].SourcePrincipalKind)
	assert.Equal(t, parent.ID, rows[0].SourcePrincipalID)
	assert.Equal(t, store.SourceCredentialAgent, rows[0].SourceCredentialKind)
	assert.Equal(t, store.EffectCeilingBounded, rows[0].Kind)
	assert.ElementsMatch(t, ceiling.PermissionIDs, rows[0].PermissionIDs)

	d := f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), child.ID, f.sa.ID)
	assertParentCeilingAllowed(t, d)
	assert.Equal(t, ProvenancePrincipal{Kind: store.DelegationPrincipalAgent, ID: parent.ID}, d.Proof.SourcePrincipal)
}

// An adopted edge from a user delegator yields a session-credential row.
func TestSALegacyBackfill_AdoptedUserEdgeAllows(t *testing.T) {
	f := newSPCFixture(t)
	agent := slbAgent(t, f, "slb-adopted-user", f.assigner)
	slbEdge(t, f, store.DelegationPrincipalUser, f.assigner, agent.ID,
		adoptedProvenance(store.DelegationPrincipalUser, f.assigner),
		boundedCeiling("gcp_service_account.assign", "gcp_service_account.use"))

	runLegacyBackfill(t, f)
	rows := activeAssignments(t, f.s, agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SourceCredentialSession, rows[0].SourceCredentialKind)
	assertParentCeilingAllowed(t,
		f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), agent.ID, f.sa.ID))
}

// Agents whose edge cannot back a row are skipped with a reason and get no
// row; agents with a row, without assign mode, or soft-deleted are not
// examined.
func TestSALegacyBackfill_SkipsAndLeavesOthersAlone(t *testing.T) {
	f := newSPCFixture(t)
	ctx := context.Background()
	session := func(id string) store.AuthorityProvenance {
		return store.AuthorityProvenance{
			ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: store.DelegationPrincipalUser,
			SourcePrincipalID: id, SourceCredentialKind: store.SourceCredentialSession,
		}
	}

	noEdge := slbAgent(t, f, "slb-no-edge", f.assigner)

	sentinel := slbAgent(t, f, "slb-sentinel", f.assigner)
	slbEdge(t, f, store.DelegationPrincipalUser, migrationDelegatorID, sentinel.ID,
		adoptedProvenance(store.DelegationPrincipalUser, migrationDelegatorID),
		boundedCeiling("gcp_service_account.assign"))

	unrecorded := slbAgent(t, f, "slb-unrecorded", f.assigner)
	slbEdge(t, f, store.DelegationPrincipalUser, f.assigner, unrecorded.ID, store.AuthorityProvenance{}, store.EffectCeiling{})

	noAssign := slbAgent(t, f, "slb-no-assign", f.assigner)
	slbEdge(t, f, store.DelegationPrincipalUser, f.assigner, noAssign.ID, session(f.assigner), boundedCeiling("agent.create"))

	goneParent := f.assignModeAgent(t, "slb-gone-parent", f.sa.ID, f.assigner)
	orphan := slbAgent(t, f, "slb-orphan", f.assigner, goneParent.ID)
	slbEdge(t, f, store.DelegationPrincipalAgent, goneParent.ID, orphan.ID,
		adoptedProvenance(store.DelegationPrincipalAgent, goneParent.ID), boundedCeiling("gcp_service_account.assign"))
	markAgentDeleted(t, f.s, goneParent.ID)

	suspended := tid("slb-suspended")
	createDCUser(t, f.s, suspended, "slb-suspended@test.com", f.projectID, store.ProjectRoleMember)
	u, err := f.s.GetUser(ctx, suspended)
	require.NoError(t, err)
	u.Status = "suspended"
	require.NoError(t, f.s.UpdateUser(ctx, u))
	inactive := slbAgent(t, f, "slb-inactive-src", suspended)
	slbEdge(t, f, store.DelegationPrincipalUser, suspended, inactive.ID, session(suspended), store.EffectCeiling{Kind: store.EffectCeilingPrincipal})

	// Not examined: an agent that already has a row keeps it untouched.
	withRow := f.assignModeAgent(t, "slb-with-row", f.sa.ID, f.assigner)
	existing := &store.AgentServiceAccountAssignment{
		AgentID: withRow.ID, ProjectID: f.projectID, ServiceAccountID: f.sa.ID,
		Origin:              store.SAAssignmentOriginCreateExplicit,
		AuthorityProvenance: session(f.assigner),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, f.s.ReplaceAgentServiceAccountAssignment(ctx, existing))

	res := runLegacyBackfill(t, f)
	assert.Equal(t, map[string]string{
		noEdge.ID:     saBackfillSkipNoEdge,
		sentinel.ID:   saBackfillSkipSentinelEdge,
		unrecorded.ID: saBackfillSkipUnrecordedEdge,
		noAssign.ID:   saBackfillSkipCeilingLacksAssign,
		orphan.ID:     saBackfillSkipDelegatorMissing,
		inactive.ID:   saBackfillSkipDelegatorInactive,
	}, skipReasons(res))
	assert.Equal(t, []string{f.agent.ID}, res.Written, "only the fixture agent qualifies")
	assert.Equal(t, 7, res.Examined)

	for id := range skipReasons(res) {
		assert.Empty(t, activeAssignments(t, f.s, id), "skipped agent %s gets no row", id)
	}
	rows := activeAssignments(t, f.s, withRow.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, existing.ID, rows[0].ID, "an existing row is never replaced")
}
