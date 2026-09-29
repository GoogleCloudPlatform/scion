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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createLegacyUAT inserts a user_access_tokens row directly through the Ent
// client, the way a pre-A.2 row exists in an un-migrated database: every
// field CreateUserAccessToken would have set is present, but
// ceiling_permission_ids is left unset (SQL NULL) and ceiling_version keeps
// its column default (0). It deliberately goes around
// ExternalStore.CreateUserAccessToken — that method now always populates the
// ceiling columns for a newly minted token, so it cannot produce the "never
// backfilled" shape this test needs.
func createLegacyUAT(t *testing.T, cs *CompositeStore, userID, projectID string, scopes []string) *store.UserAccessToken {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	future := time.Now().Add(90 * 24 * time.Hour)
	_, err := cs.client.UserAccessToken.Create().
		SetID(id).
		SetUserID(uuid.MustParse(userID)).
		SetName("legacy-token").
		SetPrefix("scion_pat_legacy").
		SetKeyHash(uuid.NewString()).
		SetProjectID(uuid.MustParse(projectID)).
		SetScopes(marshalScopes(scopes)).
		SetRevoked(false).
		SetExpiresAt(future).
		SetCreated(time.Now()).
		Save(ctx)
	require.NoError(t, err)

	stored, err := cs.GetUserAccessToken(ctx, id.String())
	require.NoError(t, err)
	require.Equal(t, permissions.CeilingVersionUnspecified, stored.CeilingVersion)
	require.Nil(t, stored.CeilingPermissionIDs, "precondition: legacy row must start as never-backfilled (NULL), not an explicit empty list")
	return stored
}

func seedProjectAndUser(t *testing.T, cs *CompositeStore) (userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{
		ID: uuid.NewString(), Name: "uat-ceiling-project", Slug: "uat-ceiling-" + uuid.NewString(),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))
	user := &store.User{
		ID: uuid.NewString(), Email: uuid.NewString() + "@example.com", DisplayName: "UAT Ceiling Test User",
		Role: "member", Status: store.UserStatusActive,
	}
	require.NoError(t, cs.CreateUser(ctx, user))
	return user.ID, project.ID
}

// TestBackfillUATCeilings_PreservesFieldsAndNormalizes is the AC migration
// round-trip test: token IDs, hashes, expiry, revocation, and scopes survive
// the backfill unchanged, and the persisted ceiling equals what the frozen
// legacy snapshot computes directly from those scopes (pat-a-lead
// requirement (b)).
func TestBackfillUATCeilings_PreservesFieldsAndNormalizes(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	before := createLegacyUAT(t, cs, userID, projectID, []string{"agent:attach", "agent:read"})

	require.NoError(t, cs.Migrate(ctx))

	after, err := cs.GetUserAccessToken(ctx, before.ID)
	require.NoError(t, err)

	// Untouched fields.
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.UserID, after.UserID)
	assert.Equal(t, before.Prefix, after.Prefix)
	assert.Equal(t, before.KeyHash, after.KeyHash)
	assert.Equal(t, before.ProjectID, after.ProjectID)
	assert.ElementsMatch(t, before.Scopes, after.Scopes)
	assert.Equal(t, before.Revoked, after.Revoked)
	assert.WithinDuration(t, *before.ExpiresAt, *after.ExpiresAt, time.Second)
	assert.WithinDuration(t, before.Created, after.Created, time.Second)

	// Normalized ceiling.
	assert.Equal(t, permissions.CeilingVersionUnspecified, after.CeilingVersion)
	want := permissions.NormalizeLegacyUATScopes(before.Scopes)
	assert.ElementsMatch(t, want, after.CeilingPermissionIDs)
	assert.NotNil(t, after.CeilingPermissionIDs, "backfilled row must no longer be 'never backfilled'")

	ceiling := after.NormalizedCeiling()
	assert.True(t, ceiling.Allows("agent.attach"))
	assert.True(t, ceiling.Allows("agent.read"))
	assert.False(t, ceiling.Allows("agent.lifecycle"), "attach-only legacy token must not gain lifecycle")
}

// TestBackfillUATCeilings_Idempotent mirrors
// TestBackfillDelegationEdges_Idempotent: a second Migrate call must not
// reprocess already-backfilled rows, and a token created after the first
// backfill (via the real mint path, which always sets the ceiling itself)
// must be left exactly as minted.
func TestBackfillUATCeilings_Idempotent(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	legacy := createLegacyUAT(t, cs, userID, projectID, []string{"agent:read"})
	require.NoError(t, cs.Migrate(ctx))

	afterFirst, err := cs.GetUserAccessToken(ctx, legacy.ID)
	require.NoError(t, err)

	// A token minted normally after the backfill already carries its own
	// explicit ceiling; a second Migrate must not touch it.
	minted := &store.UserAccessToken{
		ID: uuid.NewString(), UserID: userID, Name: "post-backfill", Prefix: "scion_pat_post",
		KeyHash: uuid.NewString(), ProjectID: projectID, Scopes: []string{"agent:read"},
		CeilingVersion: permissions.CeilingVersionV1, CeilingPermissionIDs: []string{"agent.read"},
		Created: time.Now(),
	}
	require.NoError(t, cs.CreateUserAccessToken(ctx, minted))

	require.NoError(t, cs.Migrate(ctx))

	afterSecond, err := cs.GetUserAccessToken(ctx, legacy.ID)
	require.NoError(t, err)
	assert.Equal(t, afterFirst.CeilingPermissionIDs, afterSecond.CeilingPermissionIDs, "second migrate must not reprocess an already-backfilled row")

	mintedAfter, err := cs.GetUserAccessToken(ctx, minted.ID)
	require.NoError(t, err)
	assert.Equal(t, permissions.CeilingVersionV1, mintedAfter.CeilingVersion)
	assert.Equal(t, []string{"agent.read"}, mintedAfter.CeilingPermissionIDs)
}

// TestBackfillUATCeilings_ImmuneToRegistryChange pins that once a legacy
// row's ceiling has been persisted, reloading it is unaffected by a
// subsequent change to the live permissions.Registry — the persisted value,
// not a re-derivation, is authoritative.
func TestBackfillUATCeilings_ImmuneToRegistryChange(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	legacy := createLegacyUAT(t, cs, userID, projectID, []string{"agent:attach"})
	require.NoError(t, cs.Migrate(ctx))

	before, err := cs.GetUserAccessToken(ctx, legacy.ID)
	require.NoError(t, err)

	originalRegistry := permissions.Registry
	t.Cleanup(func() { permissions.Registry = originalRegistry })
	mutated := append([]permissions.Permission(nil), originalRegistry...)
	for i := range mutated {
		if mutated[i].UATScope == "agent:attach" {
			mutated[i].ID = "agent.attach.renamed"
		}
	}
	permissions.Registry = mutated

	after, err := cs.GetUserAccessToken(ctx, legacy.ID)
	require.NoError(t, err)
	assert.Equal(t, before.CeilingPermissionIDs, after.CeilingPermissionIDs, "reloading a backfilled row must not change after a Registry mutation")
	assert.Contains(t, after.CeilingPermissionIDs, "agent.attach")
	assert.NotContains(t, after.CeilingPermissionIDs, "agent.attach.renamed")
}

// TestBackfillUATCeilings_PreservesTransactionalAudit is the AC's
// "preserves ... transactional audit" coverage: a migrated legacy token can
// still be revoked with an atomic mutation-audit record in the same
// transaction, exactly as an un-migrated token could.
func TestBackfillUATCeilings_PreservesTransactionalAudit(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	legacy := createLegacyUAT(t, cs, userID, projectID, []string{"agent:read"})
	require.NoError(t, cs.Migrate(ctx))

	err := cs.WithTx(ctx, func(tx store.Store) error {
		if err := tx.RevokeUserAccessToken(ctx, legacy.ID); err != nil {
			return err
		}
		return tx.CreateMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType:       "credential_revoke",
			ActorPrincipalKind: "user",
			ActorPrincipalID:   userID,
			TargetType:         "user_access_token",
			TargetID:           legacy.ID,
			BeforeSummary:      `{"action":"revoke"}`,
		})
	})
	require.NoError(t, err)

	revoked, err := cs.GetUserAccessToken(ctx, legacy.ID)
	require.NoError(t, err)
	assert.True(t, revoked.Revoked)
	// Ceiling survives the revoke unchanged.
	assert.ElementsMatch(t, permissions.NormalizeLegacyUATScopes(legacy.Scopes), revoked.CeilingPermissionIDs)

	records, total, err := cs.ListMutationAudits(ctx, store.MutationAuditFilter{TargetID: legacy.ID})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, records, 1)
	assert.Equal(t, "credential_revoke", records[0].MutationType)
	assert.Equal(t, legacy.ID, records[0].TargetID)
}
