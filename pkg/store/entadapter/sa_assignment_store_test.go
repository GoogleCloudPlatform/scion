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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func saAssignmentFixture(agentID, saID string) *store.AgentServiceAccountAssignment {
	return &store.AgentServiceAccountAssignment{
		AgentID:          agentID,
		ProjectID:        "proj-a",
		ServiceAccountID: saID,
		Origin:           store.SAAssignmentOriginCreateExplicit,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    "user-1",
			SourceCredentialKind: store.SourceCredentialSession,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
}

// allSAAssignmentsFor reads every row (active or not) of an agent, keyed by ID.
func allSAAssignmentsFor(t *testing.T, s *SAAssignmentStore, agentID string) map[string]store.AgentServiceAccountAssignment {
	t.Helper()
	rows, err := s.client.AgentServiceAccountAssignment.Query().All(context.Background())
	require.NoError(t, err)
	out := map[string]store.AgentServiceAccountAssignment{}
	for _, r := range rows {
		if r.AgentID == agentID {
			a := entSAAssignmentToStore(r)
			out[a.ID] = a
		}
	}
	return out
}

func TestAgentSAAssignmentStore_RoundTripsProvenanceAndCeiling(t *testing.T) {
	ctx := context.Background()
	s := NewSAAssignmentStore(enttest.NewClient(t))
	exp := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	a := saAssignmentFixture("agent-a", "sa-1")
	a.Origin = store.SAAssignmentOriginScheduledHubDefault
	a.AuthorityProvenance = store.AuthorityProvenance{
		ProvenanceVersion:           store.ProvenanceVersionV1,
		SourcePrincipalKind:         store.DelegationPrincipalUser,
		SourcePrincipalID:           "user-1",
		SourceCredentialKind:        store.SourceCredentialScheduler,
		SourceCredentialID:          "cred-1",
		SourceEventID:               "evt-1",
		SourceScheduleID:            "sched-1",
		SourceAuthorizationRevision: 3,
		InitiatorPrincipalKind:      "user",
		InitiatorPrincipalID:        "user-1",
		InitiatorCredentialKind:     store.InitiatorCredentialKindUAT,
		InitiatorCredentialID:       "tok-1",
	}
	a.EffectCeiling = store.EffectCeiling{
		Kind:              store.EffectCeilingBounded,
		Version:           permissions.CeilingVersionV1,
		PermissionIDs:     []string{"agent.create", "gcp_service_account.assign"},
		BoundaryKind:      "project",
		BoundaryProjectID: "proj-a",
		SourceExpiresAt:   &exp,
	}
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, a))
	require.NotEmpty(t, a.ID)

	got, err := s.GetActiveAgentServiceAccountAssignments(ctx, "agent-a")
	require.NoError(t, err)
	require.Len(t, got, 1)
	g := got[0]
	assert.Equal(t, a.ID, g.ID)
	assert.Equal(t, "sa-1", g.ServiceAccountID)
	assert.Equal(t, "proj-a", g.ProjectID)
	assert.Equal(t, store.SAAssignmentOriginScheduledHubDefault, g.Origin)
	assert.True(t, g.Active)
	assert.Equal(t, a.AuthorityProvenance, g.AuthorityProvenance)
	assert.Equal(t, a.Kind, g.Kind)
	assert.Equal(t, a.Version, g.Version)
	assert.Equal(t, a.PermissionIDs, g.PermissionIDs)
	assert.Equal(t, a.BoundaryKind, g.BoundaryKind)
	assert.Equal(t, a.BoundaryProjectID, g.BoundaryProjectID)
	require.NotNil(t, g.SourceExpiresAt)
	assert.True(t, exp.Equal(*g.SourceExpiresAt))
	assert.Equal(t, store.Deactivation{}, g.Deactivation)
}

func TestAgentSAAssignmentStore_ReplaceDeactivatesPrevious(t *testing.T) {
	ctx := context.Background()
	s := NewSAAssignmentStore(enttest.NewClient(t))
	first := saAssignmentFixture("agent-a", "sa-1")
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, first))
	other := saAssignmentFixture("agent-b", "sa-1")
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, other))
	second := saAssignmentFixture("agent-a", "sa-2")
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, second))

	active, err := s.GetActiveAgentServiceAccountAssignments(ctx, "agent-a")
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, second.ID, active[0].ID)
	assert.Equal(t, "sa-2", active[0].ServiceAccountID)

	all := allSAAssignmentsFor(t, s, "agent-a")
	require.Len(t, all, 2)
	old := all[first.ID]
	assert.False(t, old.Active)
	assert.Equal(t, store.EdgeDeactivationSAReplaced, old.Cause)
	assert.NotEmpty(t, old.OpID)
	assert.NotNil(t, old.At)

	// Another agent's row is untouched.
	b, err := s.GetActiveAgentServiceAccountAssignments(ctx, "agent-b")
	require.NoError(t, err)
	require.Len(t, b, 1)
	assert.Equal(t, other.ID, b[0].ID)
}

func TestAgentSAAssignmentStore_PartialUniqueIndex(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := NewSAAssignmentStore(client)
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, saAssignmentFixture("agent-a", "sa-1")))

	// A second active row for the same agent, written without Replace's
	// deactivation, is refused by the index.
	_, err := client.AgentServiceAccountAssignment.Create().
		SetAgentID("agent-a").SetServiceAccountID("sa-2").SetActive(true).Save(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, mapError(err), store.ErrAlreadyExists)

	// Inactive rows are not limited.
	for i := 0; i < 2; i++ {
		_, err := client.AgentServiceAccountAssignment.Create().
			SetAgentID("agent-a").SetServiceAccountID("sa-old").SetActive(false).Save(ctx)
		require.NoError(t, err)
	}
}

func TestAgentSAAssignmentStore_DeactivateRejectsInvalidCause(t *testing.T) {
	ctx := context.Background()
	s := NewSAAssignmentStore(enttest.NewClient(t))
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, saAssignmentFixture("agent-a", "sa-1")))
	for _, cause := range []store.EdgeDeactivationCause{"", store.EdgeDeactivationDelegatorDeleted, store.EdgeDeactivationReincarnateReplaced, "bogus"} {
		_, err := s.DeactivateAgentServiceAccountAssignments(ctx, "agent-a", store.Deactivation{Cause: cause, OpID: "op-1"})
		assert.ErrorIs(t, err, store.ErrInvalidInput, "cause %q", cause)
	}
	_, err := s.DeactivateAgentServiceAccountAssignments(ctx, "agent-a", store.Deactivation{Cause: store.EdgeDeactivationSACleared})
	assert.ErrorIs(t, err, store.ErrInvalidInput, "an operation ID is required")

	active, err := s.GetActiveAgentServiceAccountAssignments(ctx, "agent-a")
	require.NoError(t, err)
	assert.Len(t, active, 1, "a refused deactivation changes nothing")
}

func TestAgentSAAssignmentStore_ReactivateByOpID(t *testing.T) {
	ctx := context.Background()
	s := NewSAAssignmentStore(enttest.NewClient(t))
	a := saAssignmentFixture("agent-a", "sa-1")
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, a))
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	n, err := s.DeactivateAgentServiceAccountAssignments(ctx, "agent-a",
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &at, OpID: "op-1"})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := allSAAssignmentsFor(t, s, "agent-a")[a.ID]
	assert.False(t, got.Active)
	assert.Equal(t, store.EdgeDeactivationAgentSoftDelete, got.Cause)
	assert.Equal(t, "op-1", got.OpID)
	require.NotNil(t, got.At)
	assert.True(t, at.Equal(*got.At))

	// The wrong op ID or cause reactivates nothing.
	n, err = s.ReactivateAgentServiceAccountAssignments(ctx, "agent-a", store.EdgeDeactivationAgentSoftDelete, "op-2")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	n, err = s.ReactivateAgentServiceAccountAssignments(ctx, "agent-a", store.EdgeDeactivationAgentHardDelete, "op-1")
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	n, err = s.ReactivateAgentServiceAccountAssignments(ctx, "agent-a", store.EdgeDeactivationAgentSoftDelete, "op-1")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	active, err := s.GetActiveAgentServiceAccountAssignments(ctx, "agent-a")
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, a.ID, active[0].ID)
	assert.Equal(t, store.Deactivation{}, active[0].Deactivation)
}

func TestAgentSAAssignmentStore_ReactivateConflict(t *testing.T) {
	ctx := context.Background()
	s := NewSAAssignmentStore(enttest.NewClient(t))
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, saAssignmentFixture("agent-a", "sa-1")))
	_, err := s.DeactivateAgentServiceAccountAssignments(ctx, "agent-a",
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, OpID: "op-1"})
	require.NoError(t, err)
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, saAssignmentFixture("agent-a", "sa-2")))

	_, err = s.ReactivateAgentServiceAccountAssignments(ctx, "agent-a", store.EdgeDeactivationAgentSoftDelete, "op-1")
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestAgentSAAssignmentStore_ReplaceRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	s := NewSAAssignmentStore(enttest.NewClient(t))
	assert.ErrorIs(t, s.ReplaceAgentServiceAccountAssignment(ctx, nil), store.ErrInvalidInput)
	noSA := saAssignmentFixture("agent-a", "")
	assert.ErrorIs(t, s.ReplaceAgentServiceAccountAssignment(ctx, noSA), store.ErrInvalidInput)
	badCeiling := saAssignmentFixture("agent-a", "sa-1")
	badCeiling.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal, PermissionIDs: []string{"x"}}
	assert.ErrorIs(t, s.ReplaceAgentServiceAccountAssignment(ctx, badCeiling), store.ErrInvalidInput)
}

func TestDelegationEdgeStore_RejectsAssignmentCauses(t *testing.T) {
	ctx := context.Background()
	s := NewDelegationEdgeStore(enttest.NewClient(t))
	seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-a", "proj-a")
	for _, cause := range []store.EdgeDeactivationCause{store.EdgeDeactivationSAReplaced, store.EdgeDeactivationSACleared} {
		_, err := s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-a",
			store.Deactivation{Cause: cause, OpID: "op-1"})
		assert.ErrorIs(t, err, store.ErrInvalidInput, "cause %q", cause)
	}
}
