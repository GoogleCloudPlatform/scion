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

// seedEdge writes an active principal edge from delegator to delegate in
// scope project:scopeID.
func seedEdge(t *testing.T, s *DelegationEdgeStore, delegatorType, delegatorID, delegateID, scopeID string) *store.DelegationEdge {
	t.Helper()
	e := &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    delegateID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       scopeID,
		Role:          "worker",
		Active:        true,
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), e))
	return e
}

// allEdgesFor reads every edge (active or not) of a delegate, keyed by ID.
func allEdgesFor(t *testing.T, s *DelegationEdgeStore, delegateID string) map[string]*store.DelegationEdge {
	t.Helper()
	rows, err := s.client.DelegationEdge.Query().All(context.Background())
	require.NoError(t, err)
	out := map[string]*store.DelegationEdge{}
	for _, r := range rows {
		if r.DelegateID == delegateID {
			e := entDelegationEdgeToStore(r)
			out[e.ID] = e
		}
	}
	return out
}

func TestDeactivateEdgesForDelegateStore(t *testing.T) {
	ctx := context.Background()
	s := NewDelegationEdgeStore(enttest.NewClient(t))
	a := seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-a", "proj-a")
	b := seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-a", "proj-b")
	other := seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-b", "proj-a")

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	n, err := s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-a",
		store.Deactivation{Cause: store.EdgeDeactivationCreateCompensation, At: &at, OpID: "op-1"})
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	got := allEdgesFor(t, s, "agent-a")
	for _, id := range []string{a.ID, b.ID} {
		e := got[id]
		require.NotNil(t, e)
		assert.False(t, e.Active)
		assert.Equal(t, store.EdgeDeactivationCreateCompensation, e.Cause)
		assert.Equal(t, "op-1", e.OpID)
		require.NotNil(t, e.At)
		assert.True(t, at.Equal(*e.At))
	}
	active, err := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-a")
	require.NoError(t, err)
	assert.Empty(t, active)

	o := allEdgesFor(t, s, "agent-b")[other.ID]
	assert.True(t, o.Active, "another delegate's edge is untouched")
	assert.Equal(t, store.Deactivation{}, o.Deactivation)

	// Already inactive edges keep their first deactivation record.
	n, err = s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-a",
		store.Deactivation{Cause: store.EdgeDeactivationAgentHardDelete, OpID: "op-2"})
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, "op-1", allEdgesFor(t, s, "agent-a")[a.ID].OpID)
}

func TestDeactivateEdgesForDelegatorStore(t *testing.T) {
	ctx := context.Background()
	s := NewDelegationEdgeStore(enttest.NewClient(t))
	e1 := seedEdge(t, s, store.DelegationPrincipalUser, "user-del", "agent-1", "proj-a")
	e2 := seedEdge(t, s, store.DelegationPrincipalUser, "user-del", "agent-2", "proj-a")
	keep := seedEdge(t, s, store.DelegationPrincipalUser, "user-keep", "agent-3", "proj-a")
	// Same ID, different delegator type: not matched.
	agentTyped := seedEdge(t, s, store.DelegationPrincipalAgent, "user-del", "agent-4", "proj-a")

	n, err := s.DeactivateDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, "user-del",
		store.Deactivation{Cause: store.EdgeDeactivationDelegatorDeleted, OpID: "op-user"})
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	for _, tc := range []struct {
		edge     *store.DelegationEdge
		delegate string
	}{{e1, "agent-1"}, {e2, "agent-2"}} {
		e := allEdgesFor(t, s, tc.delegate)[tc.edge.ID]
		assert.False(t, e.Active)
		assert.Equal(t, store.EdgeDeactivationDelegatorDeleted, e.Cause)
		assert.Equal(t, "op-user", e.OpID)
		assert.NotNil(t, e.At, "a zero At is set to the write time")
	}
	assert.True(t, allEdgesFor(t, s, "agent-3")[keep.ID].Active)
	assert.True(t, allEdgesFor(t, s, "agent-4")[agentTyped.ID].Active)
	remaining, err := s.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, "user-del")
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

func TestReactivateEdgesForDelegateStoreIsExact(t *testing.T) {
	ctx := context.Background()
	s := NewDelegationEdgeStore(enttest.NewClient(t))
	target := seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-r", "proj-a")
	n, err := s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-r",
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, OpID: "op-this"})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Edges of other operations and causes on other scopes.
	earlier := seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-r", "proj-b")
	_, err = s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-r",
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, OpID: "op-earlier"})
	require.NoError(t, err)
	comp := seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-r", "proj-c")
	_, err = s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-r",
		store.Deactivation{Cause: store.EdgeDeactivationCreateCompensation, OpID: "op-this"})
	require.NoError(t, err)

	n, err = s.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-r",
		store.EdgeDeactivationAgentSoftDelete, "op-this")
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	got := allEdgesFor(t, s, "agent-r")
	assert.True(t, got[target.ID].Active)
	assert.Equal(t, store.Deactivation{}, got[target.ID].Deactivation, "the deactivation record is cleared")
	assert.False(t, got[earlier.ID].Active, "an earlier operation's edge stays inactive")
	assert.False(t, got[comp.ID].Active, "a different cause with the same op ID stays inactive")

	// Reactivating again is a no-op.
	n, err = s.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-r",
		store.EdgeDeactivationAgentSoftDelete, "op-this")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// A conflicting reactivate inside a transaction is refused and changes
// nothing: the deactivated edge stays inactive with its record.
func TestReactivateEdgesForDelegateStoreConflict(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	s := NewDelegationEdgeStore(client)
	old := seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-c", "proj-a")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err := s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-c",
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &at, OpID: "op-1"})
	require.NoError(t, err)
	current := seedEdge(t, s, store.DelegationPrincipalUser, "user-2", "agent-c", "proj-a")

	err = cs.WithTx(ctx, func(tx store.Store) error {
		_, err := tx.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-c",
			store.EdgeDeactivationAgentSoftDelete, "op-1")
		return err
	})
	assert.ErrorIs(t, err, store.ErrAlreadyExists, "a second active edge in the same scope is refused")

	got := allEdgesFor(t, s, "agent-c")
	o := got[old.ID]
	require.NotNil(t, o)
	assert.False(t, o.Active, "the deactivated edge stays inactive")
	assert.Equal(t, store.EdgeDeactivationAgentSoftDelete, o.Cause)
	assert.Equal(t, "op-1", o.OpID)
	require.NotNil(t, o.At)
	assert.True(t, at.Equal(*o.At))
	assert.True(t, got[current.ID].Active, "the other active edge is untouched")
}

// Deactivate and reactivate write only the active flag and the deactivation
// record: a bounded ceiling and v1 provenance come back unchanged.
func TestReactivatePreservesCeilingAndProvenance(t *testing.T) {
	ctx := context.Background()
	s := NewDelegationEdgeStore(enttest.NewClient(t))
	expires := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	e := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   "user-1",
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    "agent-p",
		ScopeType:     store.RoleScopeProject,
		ScopeID:       "proj-a",
		Role:          "worker",
		Active:        true,
		EffectCeiling: store.EffectCeiling{
			Kind:              store.EffectCeilingBounded,
			Version:           permissions.CeilingVersionV1,
			PermissionIDs:     []string{"agent.read", "agent.stop"},
			BoundaryKind:      "project",
			BoundaryProjectID: "proj-a",
			SourceExpiresAt:   &expires,
		},
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:       1,
			SourcePrincipalKind:     store.DelegationPrincipalUser,
			SourcePrincipalID:       "user-1",
			SourceCredentialKind:    store.SourceCredentialUAT,
			SourceCredentialID:      "uat-1",
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    "user-1",
			InitiatorCredentialKind: "uat",
			InitiatorCredentialID:   "uat-1",
		},
	}
	require.NoError(t, s.CreateDelegationEdge(ctx, e))
	before := allEdgesFor(t, s, "agent-p")[e.ID]
	require.NotNil(t, before)
	require.Equal(t, store.EffectCeilingBounded, before.Kind)
	require.Equal(t, 1, before.ProvenanceVersion)

	n, err := s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-p",
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, OpID: "op-p"})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = s.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-p",
		store.EdgeDeactivationAgentSoftDelete, "op-p")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	after := allEdgesFor(t, s, "agent-p")[e.ID]
	require.NotNil(t, after)
	assert.True(t, after.Active)
	assert.Equal(t, before.EffectCeiling, after.EffectCeiling, "ceiling unchanged")
	assert.Equal(t, before.AuthorityProvenance, after.AuthorityProvenance, "provenance unchanged")
}

func TestEdgeDeactivationRejectsInvalidRecord(t *testing.T) {
	ctx := context.Background()
	s := NewDelegationEdgeStore(enttest.NewClient(t))
	seedEdge(t, s, store.DelegationPrincipalUser, "user-1", "agent-v", "proj-a")

	for name, d := range map[string]store.Deactivation{
		"empty cause":      {OpID: "op"},
		"unknown cause":    {Cause: "not_a_cause", OpID: "op"},
		"missing op id":    {Cause: store.EdgeDeactivationAgentSoftDelete},
		"assignment cause": {Cause: "sa_cleared", OpID: "op"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-v", d)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
			_, err = s.DeactivateDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, "user-1", d)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
			_, err = s.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-v", d.Cause, d.OpID)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}
	active, err := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, "agent-v")
	require.NoError(t, err)
	assert.Len(t, active, 1, "a rejected record writes nothing")
}
