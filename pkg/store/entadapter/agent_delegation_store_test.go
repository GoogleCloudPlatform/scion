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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testDelegationGrant() *store.AgentDelegationGrant {
	return &store.AgentDelegationGrant{
		AgentID:                 uuid.NewString(),
		AgentProjectID:          uuid.NewString(),
		AgentGeneration:         2,
		AgentStateVersion:       7,
		IssuerUserID:            uuid.NewString(),
		BoundaryKind:            "hub",
		CeilingVersion:          1,
		CeilingPermissionIDs:    []string{"agent.read"},
		Name:                    "grant",
		Purpose:                 "purpose",
		Labels:                  map[string]string{"team": "reports"},
		MaxCredentialTTLSeconds: 900,
		ExpiresAt:               time.Now().Add(time.Hour).UTC().Truncate(time.Second),
		IssuanceAuditID:         uuid.NewString(),
	}
}

func TestAgentDelegationStore_GrantRoundTripAndValidation(t *testing.T) {
	s := NewCompositeStore(enttest.NewClient(t))
	ctx := context.Background()

	g := testDelegationGrant()
	require.NoError(t, s.CreateAgentDelegationGrant(ctx, g))
	require.NotEmpty(t, g.ID)

	got, err := s.GetAgentDelegationGrant(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, g.AgentID, got.AgentID)
	assert.Equal(t, g.AgentProjectID, got.AgentProjectID)
	assert.Equal(t, 2, got.AgentGeneration)
	assert.Equal(t, int64(7), got.AgentStateVersion)
	assert.Equal(t, "hub", got.BoundaryKind)
	assert.Empty(t, got.BoundaryProjectID)
	assert.Equal(t, 1, got.CeilingVersion)
	assert.Equal(t, []string{"agent.read"}, got.CeilingPermissionIDs)
	assert.Equal(t, "purpose", got.Purpose)
	assert.Equal(t, map[string]string{"team": "reports"}, got.Labels)
	assert.False(t, got.AllowSubdelegation)
	assert.Empty(t, got.ParentGrantID)
	assert.Zero(t, got.Depth)
	assert.Equal(t, 900, got.MaxCredentialTTLSeconds)
	assert.True(t, g.ExpiresAt.Equal(got.ExpiresAt))
	assert.Nil(t, got.LastExchangedAt)
	assert.Nil(t, got.RevokedAt)

	_, err = s.GetAgentDelegationGrant(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAgentDelegationGrant(ctx, "not-a-uuid")
	assert.ErrorIs(t, err, store.ErrNotFound)

	at := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.MarkAgentDelegationGrantExchanged(ctx, g.ID, at))
	got, err = s.GetAgentDelegationGrant(ctx, g.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastExchangedAt)
	assert.True(t, at.Equal(*got.LastExchangedAt))

	for name, mutate := range map[string]func(*store.AgentDelegationGrant){
		"hub boundary with a project": func(g *store.AgentDelegationGrant) { g.BoundaryProjectID = uuid.NewString() },
		"project boundary without one": func(g *store.AgentDelegationGrant) { g.BoundaryKind = "project" },
		"unknown boundary kind":        func(g *store.AgentDelegationGrant) { g.BoundaryKind = "galaxy" },
		"ceiling version zero":         func(g *store.AgentDelegationGrant) { g.CeilingVersion = 0 },
		"empty ceiling":                func(g *store.AgentDelegationGrant) { g.CeilingPermissionIDs = nil },
		"subdelegation":                func(g *store.AgentDelegationGrant) { g.AllowSubdelegation = true },
		"parent grant":                 func(g *store.AgentDelegationGrant) { g.ParentGrantID = uuid.NewString() },
	} {
		t.Run(name, func(t *testing.T) {
			bad := testDelegationGrant()
			mutate(bad)
			assert.ErrorIs(t, s.CreateAgentDelegationGrant(ctx, bad), store.ErrInvalidInput)
		})
	}

	p := testDelegationGrant()
	p.BoundaryKind = "project"
	p.BoundaryProjectID = uuid.NewString()
	require.NoError(t, s.CreateAgentDelegationGrant(ctx, p))
	got, err = s.GetAgentDelegationGrant(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, p.BoundaryProjectID, got.BoundaryProjectID)
}

func TestAgentDelegationStore_CredentialAndRevocation(t *testing.T) {
	s := NewCompositeStore(enttest.NewClient(t))
	ctx := context.Background()

	g := testDelegationGrant()
	require.NoError(t, s.CreateAgentDelegationGrant(ctx, g))
	newCred := func(hash string) *store.AgentDelegatedCredential {
		return &store.AgentDelegatedCredential{
			GrantID: g.ID, AgentID: g.AgentID, KeyHash: hash, Prefix: "scion_adt_", Audience: "scion-hub",
			CeilingPermissionIDs: []string{"agent.read"}, ExchangeAgentCredentialID: uuid.NewString(),
			ExpiresAt: time.Now().Add(15 * time.Minute),
		}
	}
	c1 := newCred("hash-one")
	require.NoError(t, s.CreateAgentDelegatedCredential(ctx, c1))
	c2 := newCred("hash-two")
	require.NoError(t, s.CreateAgentDelegatedCredential(ctx, c2))
	assert.ErrorIs(t, s.CreateAgentDelegatedCredential(ctx, newCred("hash-one")), store.ErrAlreadyExists, "key hashes are unique")
	empty := newCred("hash-empty")
	empty.CeilingPermissionIDs = nil
	assert.ErrorIs(t, s.CreateAgentDelegatedCredential(ctx, empty), store.ErrInvalidInput)

	got, err := s.GetAgentDelegatedCredentialByKeyHash(ctx, "hash-one")
	require.NoError(t, err)
	assert.Equal(t, c1.ID, got.ID)
	assert.Equal(t, g.ID, got.GrantID)
	assert.Equal(t, []string{"agent.read"}, got.CeilingPermissionIDs)
	assert.Equal(t, c1.ExchangeAgentCredentialID, got.ExchangeAgentCredentialID)
	_, err = s.GetAgentDelegatedCredentialByKeyHash(ctx, "missing")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAgentDelegatedCredentialByKeyHash(ctx, "")
	assert.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, s.UpdateAgentDelegatedCredentialLastSeen(ctx, c1.ID, time.Now()))

	auditID := uuid.NewString()
	revoked, err := s.RevokeAgentDelegationGrant(ctx, g.ID, "admin", "test", auditID, time.Now())
	require.NoError(t, err)
	assert.True(t, revoked)
	gotGrant, err := s.GetAgentDelegationGrant(ctx, g.ID)
	require.NoError(t, err)
	require.NotNil(t, gotGrant.RevokedAt)
	assert.Equal(t, "admin", gotGrant.RevokedBy)
	assert.Equal(t, "test", gotGrant.RevokeReason)
	assert.Equal(t, auditID, gotGrant.RevocationAuditID)
	for _, hash := range []string{"hash-one", "hash-two"} {
		c, err := s.GetAgentDelegatedCredentialByKeyHash(ctx, hash)
		require.NoError(t, err)
		assert.NotNil(t, c.RevokedAt, "revoking the grant revokes its credential %s", hash)
		assert.Equal(t, "test", c.RevokeReason)
	}

	// Revoking again changes nothing.
	revoked, err = s.RevokeAgentDelegationGrant(ctx, g.ID, "someone-else", "other", uuid.NewString(), time.Now())
	require.NoError(t, err)
	assert.False(t, revoked)
	gotGrant, err = s.GetAgentDelegationGrant(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, "admin", gotGrant.RevokedBy)

	_, err = s.RevokeAgentDelegationGrant(ctx, uuid.NewString(), "a", "b", "", time.Now())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAgentDelegationStore_WritesRollBackWithTheTransaction(t *testing.T) {
	s := NewCompositeStore(enttest.NewClient(t))
	ctx := context.Background()
	g := testDelegationGrant()
	err := s.WithTx(ctx, func(tx store.Store) error {
		require.NoError(t, tx.CreateAgentDelegationGrant(ctx, g))
		return assert.AnError
	})
	require.ErrorIs(t, err, assert.AnError)
	_, err = s.GetAgentDelegationGrant(ctx, g.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAgentDelegationStore_GetAgentCredentialByID(t *testing.T) {
	s := NewCompositeStore(enttest.NewClient(t))
	ctx := context.Background()
	cred := &store.AgentCredential{AgentID: uuid.NewString(), ProjectID: uuid.NewString(), TokenJTIHash: "jti-hash", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, s.CreateAgentCredential(ctx, cred))
	got, err := s.GetAgentCredentialByID(ctx, cred.ID)
	require.NoError(t, err)
	assert.Equal(t, cred.AgentID, got.AgentID)
	_, err = s.GetAgentCredentialByID(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestMutationAuditStore_AgentDelegationColumns: the agent delegation block
// round-trips, defaults empty for every other writer, and adding it leaves
// existing filtered reads (the shape the GCP IAM settings attribution reads
// through) returning exactly the same records.
func TestMutationAuditStore_AgentDelegationColumns(t *testing.T) {
	s := NewMutationAuditStore(enttest.NewClient(t))
	ctx := context.Background()

	plain := &store.MutationAuditRecord{
		MutationType: "hub_setting_update", ActorPrincipalKind: "user", ActorPrincipalID: "u1",
		TargetType: "hub_setting", TargetID: "gcp_iam.check_mode", AfterSummary: "enforce",
	}
	require.NoError(t, s.CreateMutationAudit(ctx, plain))
	before, total, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "hub_setting_update", TargetType: "hub_setting", TargetID: "gcp_iam.check_mode"})
	require.NoError(t, err)
	require.Equal(t, 1, total)

	delegated := &store.MutationAuditRecord{
		MutationType: "agent_delegation_credential_issue", ActorPrincipalKind: "agent", ActorPrincipalID: "a1",
		TargetType: "agent_delegated_credential", TargetID: "c1",
		ActorAgentID: "a1", AuthorizingUserID: "u1", SourceGrantID: "g1", ParentGrantID: "pg",
		DelegationEdgeID: "de", ExchangeAgentCredentialID: "ac1", ActorKind: "agent_delegated", AgentDelegationCode: "code",
	}
	require.NoError(t, s.CreateMutationAudit(ctx, delegated))

	after, total, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "hub_setting_update", TargetType: "hub_setting", TargetID: "gcp_iam.check_mode"})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	assert.Equal(t, before, after, "filtered reads are unchanged by delegation records")
	p := after[0]
	for name, v := range map[string]string{
		"ActorAgentID": p.ActorAgentID, "AuthorizingUserID": p.AuthorizingUserID, "SourceGrantID": p.SourceGrantID,
		"ParentGrantID": p.ParentGrantID, "DelegationEdgeID": p.DelegationEdgeID,
		"ExchangeAgentCredentialID": p.ExchangeAgentCredentialID, "ActorKind": p.ActorKind, "AgentDelegationCode": p.AgentDelegationCode,
	} {
		assert.Empty(t, v, "%s defaults empty", name)
	}

	got, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "agent_delegation_credential_issue"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	d := got[0]
	assert.Equal(t, "a1", d.ActorAgentID)
	assert.Equal(t, "u1", d.AuthorizingUserID)
	assert.Equal(t, "g1", d.SourceGrantID)
	assert.Equal(t, "pg", d.ParentGrantID)
	assert.Equal(t, "de", d.DelegationEdgeID)
	assert.Equal(t, "ac1", d.ExchangeAgentCredentialID)
	assert.Equal(t, "agent_delegated", d.ActorKind)
	assert.Equal(t, "code", d.AgentDelegationCode)
}
