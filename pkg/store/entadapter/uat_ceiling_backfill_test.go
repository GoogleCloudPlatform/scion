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
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createLegacyUAT inserts a user_access_tokens row directly through the Ent
// client, the way a row exists in an un-migrated database: every field
// CreateUserAccessToken would have set is present, but ceiling_permission_ids
// is left unset (SQL NULL) and ceiling_version keeps its column default (0).
// It deliberately goes around ExternalStore.CreateUserAccessToken — that
// method always populates the ceiling columns for a newly minted token, so
// it cannot produce the "never backfilled" shape this test needs.
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

// TestBackfillUATCeilings_PreservesFieldsAndNormalizes is the migration
// round-trip test: token IDs, hashes, expiry, revocation, and scopes survive
// the backfill unchanged, and the persisted ceiling equals what the frozen
// legacy snapshot computes directly from those scopes.
//
// These entadapter tests run against SQLite by default (enttest.NewClient).
// Building with -tags integration and SCION_TEST_POSTGRES_URL set runs the
// same suite against Postgres instead.
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

// TestBackfillUATCeilings_ImmuneToRegistryChange pins that the backfill
// itself writes the frozen legacy snapshot's value, never a live
// selector-resolved one. The alias mutation is installed and proven
// effective on ResolveSelector BEFORE Migrate runs, so a backfill that used
// live resolution (instead of permissions.NormalizeLegacyUATScopes) would
// persist the widened value — the exact hazard a skipped-upgrade or
// restored-old-database rolling deploy could hit for real, since the
// mutation reflects registry/alias state that can differ between when a row
// was minted and when its first Migrate finally runs.
func TestBackfillUATCeilings_ImmuneToRegistryChange(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	legacy := createLegacyUAT(t, cs, userID, projectID, []string{"agent:attach"})

	// Retarget agent:attach as a manage alias for "agent" BEFORE Migrate:
	// buildSelectorRegistry processes aliases after plain UATScope entries,
	// so this alias candidate wins the same map key, and a live resolution
	// of "agent:attach" would jump from {agent.attach} to the full
	// agent:manage expansion, which includes agent.lifecycle.
	mutatedAliases := make(map[string]string, len(permissions.UATManageAliases)+1)
	for k, v := range permissions.UATManageAliases {
		mutatedAliases[k] = v
	}
	mutatedAliases["agent:attach"] = permissions.ResourceAgent
	t.Cleanup(permissions.OverrideSelectorInputsForTest(permissions.Registry, mutatedAliases))

	live, ok := permissions.ResolveSelector("agent:attach")
	require.True(t, ok)
	require.Contains(t, live.PermissionIDs, "agent.lifecycle", "test setup: expected the alias mutation to be effective before Migrate runs")

	require.NoError(t, cs.Migrate(ctx))

	persisted, err := cs.GetUserAccessToken(ctx, legacy.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.attach"}, persisted.CeilingPermissionIDs,
		"the backfill must persist the frozen legacy snapshot's value, not a live-resolved one")
	assert.False(t, persisted.NormalizedCeiling().Allows("agent.lifecycle"),
		"a live-resolving backfill would have persisted lifecycle; the frozen snapshot must not")
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

// TestBackfillUATCeilings_Pagination exercises more than one page of the
// keyset-paginated backfill query, with the page size shrunk so the test
// does not need hundreds of rows to cross a page boundary. This alone would
// still pass if the per-store page-size field were ignored (7 rows fit in
// one page at the 500 default too); TestBackfillUATCeilings_PageSizeControlsQueryCount
// below is the test that actually distinguishes the two.
func TestBackfillUATCeilings_Pagination(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	cs.uatCeilingBackfillPageSize = 3

	const rowCount = 7 // more than two pages at page size 3
	ids := make([]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		row := createLegacyUAT(t, cs, userID, projectID, []string{"agent:read"})
		ids = append(ids, row.ID)
	}

	require.NoError(t, cs.Migrate(ctx))

	for _, id := range ids {
		loaded, err := cs.GetUserAccessToken(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, []string{"agent.read"}, loaded.CeilingPermissionIDs, "row %s must be backfilled exactly once across pagination", id)
	}
}

// TestBackfillUATCeilings_PageSizeControlsQueryCount observes the number of
// underlying UserAccessToken queries directly, via an Ent query interceptor,
// rather than only checking the backfill's end result: 7 rows at a page size
// of 3 must round-trip in exactly 3 pages (3, 3, 1). Unlike
// TestBackfillUATCeilings_Pagination above, this fails if the per-store
// uatCeilingBackfillPageSizeOrDefault field is ever ignored in favor of the
// 500 default — 7 rows would then come back in a single page (1 query), not
// 3. This test calls BackfillUATCeilings directly rather than Migrate, so
// only its own queries are counted; HubSetting reads/writes go through a
// different Ent client and are not observed by this interceptor.
func TestBackfillUATCeilings_PageSizeControlsQueryCount(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	cs.uatCeilingBackfillPageSize = 3

	const rowCount = 7 // 3 pages at page size 3: 3, 3, 1
	for i := 0; i < rowCount; i++ {
		createLegacyUAT(t, cs, userID, projectID, []string{"agent:read"})
	}

	var queries int
	client.UserAccessToken.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(ctx context.Context, q ent.Query) (ent.Value, error) {
			queries++
			return next.Query(ctx, q)
		})
	}))

	require.NoError(t, cs.BackfillUATCeilings(ctx))
	assert.Equal(t, 3, queries, "a page size of 3 over 7 rows must query in 3 pages, not fall back to the 500 default's single page")
}

// TestBackfillUATCeilings_LeavesExistingV1RowsUntouched pins the backfill's
// ceiling_permission_ids IS NULL filter: a V1 row that already exists before
// the first Migrate call (e.g. minted by a replica whose own Migrate already
// wrote the completion marker while another replica is still rolling out)
// must not be rewritten. Its Scopes deliberately normalize to a DIFFERENT
// permission than its persisted V1 ceiling, so the assertion cannot pass by
// the backfill coincidentally recomputing the same value.
func TestBackfillUATCeilings_LeavesExistingV1RowsUntouched(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	v1Token := &store.UserAccessToken{
		ID: uuid.NewString(), UserID: userID, Name: "pre-migrate-v1", Prefix: "scion_pat_premig",
		KeyHash: uuid.NewString(), ProjectID: projectID, Scopes: []string{"agent:attach"},
		CeilingVersion: permissions.CeilingVersionV1, CeilingPermissionIDs: []string{"agent.read"},
		Created: time.Now(),
	}
	require.NoError(t, cs.CreateUserAccessToken(ctx, v1Token))

	require.NoError(t, cs.Migrate(ctx)) // first Migrate on this store

	after, err := cs.GetUserAccessToken(ctx, v1Token.ID)
	require.NoError(t, err)
	assert.Equal(t, permissions.CeilingVersionV1, after.CeilingVersion, "a pre-existing V1 row must not be rewritten to version 0")
	assert.Equal(t, []string{"agent.read"}, after.CeilingPermissionIDs, "a pre-existing V1 row's ceiling must not be recomputed from Scopes")
}

// TestBackfillUATCeilings_PreservesRevokedAndNilExpiry covers two field
// shapes the other tests do not: a row that starts revoked (the backfill
// must not clear it) and a row with no expiry at all (the backfill must not
// invent one).
func TestBackfillUATCeilings_PreservesRevokedAndNilExpiry(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	revokedID := uuid.New()
	_, err := client.UserAccessToken.Create().
		SetID(revokedID).
		SetUserID(uuid.MustParse(userID)).
		SetName("revoked-legacy").
		SetPrefix("scion_pat_revoked").
		SetKeyHash(uuid.NewString()).
		SetProjectID(uuid.MustParse(projectID)).
		SetScopes(marshalScopes([]string{"agent:read"})).
		SetRevoked(true).
		SetCreated(time.Now()).
		Save(ctx)
	require.NoError(t, err)

	noExpiryID := uuid.New()
	_, err = client.UserAccessToken.Create().
		SetID(noExpiryID).
		SetUserID(uuid.MustParse(userID)).
		SetName("no-expiry-legacy").
		SetPrefix("scion_pat_noexp").
		SetKeyHash(uuid.NewString()).
		SetProjectID(uuid.MustParse(projectID)).
		SetScopes(marshalScopes([]string{"agent:read"})).
		SetRevoked(false).
		SetCreated(time.Now()).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, cs.Migrate(ctx))

	revoked, err := cs.GetUserAccessToken(ctx, revokedID.String())
	require.NoError(t, err)
	assert.True(t, revoked.Revoked, "backfill must not clear revocation")
	assert.Equal(t, []string{"agent.read"}, revoked.CeilingPermissionIDs)

	noExpiry, err := cs.GetUserAccessToken(ctx, noExpiryID.String())
	require.NoError(t, err)
	assert.Nil(t, noExpiry.ExpiresAt, "backfill must not invent an expiry")
	assert.Equal(t, []string{"agent.read"}, noExpiry.CeilingPermissionIDs)
}

// TestBackfillUATCeilings_MalformedOrUnknownCeilingDenies is the malformed-
// ceiling round-trip: garbage written directly into ceiling_permission_ids,
// through neither the mint path nor the backfill, must deny on load rather
// than being mistaken for "never backfilled" and re-derived from Scopes.
func TestBackfillUATCeilings_MalformedOrUnknownCeilingDenies(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	userID, projectID := seedProjectAndUser(t, cs)

	t.Run("malformed JSON, version 0", func(t *testing.T) {
		row := createLegacyUAT(t, cs, userID, projectID, []string{"agent:read"})
		_, err := client.UserAccessToken.UpdateOneID(uuid.MustParse(row.ID)).
			SetCeilingPermissionIds("not-json").
			Save(ctx)
		require.NoError(t, err)

		loaded, err := cs.GetUserAccessToken(ctx, row.ID)
		require.NoError(t, err)
		assert.False(t, loaded.NormalizedCeiling().Allows("agent.read"),
			"malformed ceiling JSON must deny rather than re-derive from Scopes, even though agent:read would otherwise resolve")
	})

	t.Run("malformed JSON, version 1", func(t *testing.T) {
		id := uuid.New()
		_, err := client.UserAccessToken.Create().
			SetID(id).
			SetUserID(uuid.MustParse(userID)).
			SetName("v1-malformed").
			SetPrefix("scion_pat_v1mal").
			SetKeyHash(uuid.NewString()).
			SetProjectID(uuid.MustParse(projectID)).
			SetScopes(marshalScopes([]string{"agent:read"})).
			SetCeilingVersion(int32(permissions.CeilingVersionV1)).
			SetCeilingPermissionIds("not-json").
			SetRevoked(false).
			SetCreated(time.Now()).
			Save(ctx)
		require.NoError(t, err)

		loaded, err := cs.GetUserAccessToken(ctx, id.String())
		require.NoError(t, err)
		assert.False(t, loaded.NormalizedCeiling().Allows("agent.read"))
	})

	t.Run("persisted empty list wins over Scopes re-derivation", func(t *testing.T) {
		row := createLegacyUAT(t, cs, userID, projectID, []string{"agent:read"})
		_, err := client.UserAccessToken.UpdateOneID(uuid.MustParse(row.ID)).
			SetCeilingPermissionIds("[]").
			Save(ctx)
		require.NoError(t, err)

		loaded, err := cs.GetUserAccessToken(ctx, row.ID)
		require.NoError(t, err)
		require.NotNil(t, loaded.CeilingPermissionIDs, "an explicit [] must not be read back as nil/never-backfilled")
		assert.Empty(t, loaded.CeilingPermissionIDs)
		assert.False(t, loaded.NormalizedCeiling().Allows("agent.read"),
			"a persisted empty list must deny even though Scopes would otherwise resolve to agent.read")
	})

	t.Run("null literal denies", func(t *testing.T) {
		row := createLegacyUAT(t, cs, userID, projectID, []string{"agent:read"})
		_, err := client.UserAccessToken.UpdateOneID(uuid.MustParse(row.ID)).
			SetCeilingPermissionIds("null").
			Save(ctx)
		require.NoError(t, err)

		loaded, err := cs.GetUserAccessToken(ctx, row.ID)
		require.NoError(t, err)
		assert.False(t, loaded.NormalizedCeiling().Allows("agent.read"))
	})

	t.Run("unknown version denies even a well-formed list", func(t *testing.T) {
		row := createLegacyUAT(t, cs, userID, projectID, []string{"agent:read"})
		validList := marshalCeilingPermissionIDs([]string{"agent.read"})
		require.NotNil(t, validList)
		_, err := client.UserAccessToken.UpdateOneID(uuid.MustParse(row.ID)).
			SetCeilingVersion(9).
			SetCeilingPermissionIds(*validList).
			Save(ctx)
		require.NoError(t, err)

		loaded, err := cs.GetUserAccessToken(ctx, row.ID)
		require.NoError(t, err)
		assert.False(t, loaded.NormalizedCeiling().Allows("agent.read"), "an unknown ceiling version must deny even a well-formed permission list")
	})

	// A versioned row with NULL permission IDs is malformed: every V1+ mint
	// sets both columns together, so this shape means something else wrote
	// the version without the list. It must deny both before AND after
	// Migrate runs, and the backfill must not touch it — rewriting it to
	// version 0 would turn a denying row into an allowing one; this
	// sub-test pins that the backfill leaves it unchanged.
	//
	// Each case uses its own client/store, not the outer cs: Migrate's
	// completion marker is per-client, so sharing cs across cases would make
	// every case after the first a no-op (the marker already set) rather
	// than a real exercise of the backfill query.
	for _, version := range []int32{int32(permissions.CeilingVersionV1), 9} {
		t.Run(fmt.Sprintf("versioned row with NULL ids is left as is by Migrate, version %d", version), func(t *testing.T) {
			caseClient := enttest.NewClient(t)
			caseCS := NewCompositeStore(caseClient)
			caseUserID, caseProjectID := seedProjectAndUser(t, caseCS)

			id := uuid.New()
			_, err := caseClient.UserAccessToken.Create().
				SetID(id).
				SetUserID(uuid.MustParse(caseUserID)).
				SetName(fmt.Sprintf("v%d-null-ids", version)).
				SetPrefix(fmt.Sprintf("scion_pat_v%dnull", version)).
				SetKeyHash(uuid.NewString()).
				SetProjectID(uuid.MustParse(caseProjectID)).
				SetScopes(marshalScopes([]string{"agent:read"})).
				SetCeilingVersion(version).
				SetRevoked(false).
				SetCreated(time.Now()).
				Save(ctx)
			require.NoError(t, err)

			before, err := caseCS.GetUserAccessToken(ctx, id.String())
			require.NoError(t, err)
			require.Equal(t, permissions.CeilingVersion(version), before.CeilingVersion)
			require.Nil(t, before.CeilingPermissionIDs)
			assert.False(t, before.NormalizedCeiling().Allows("agent.read"), "a versioned row with NULL ids must deny before Migrate, even though Scopes would otherwise resolve to agent.read")

			require.NoError(t, caseCS.Migrate(ctx))

			after, err := caseCS.GetUserAccessToken(ctx, id.String())
			require.NoError(t, err)
			assert.Equal(t, permissions.CeilingVersion(version), after.CeilingVersion, "the backfill must not rewrite a versioned row's version")
			assert.Nil(t, after.CeilingPermissionIDs, "the backfill must not populate a versioned row's permission ids")
			assert.False(t, after.NormalizedCeiling().Allows("agent.read"), "a versioned row with NULL ids must still deny after Migrate")
		})
	}
}

// TestPersistedCeilingColumnValue_NilNormalizeResultFailsClosed pins the
// backfill's guard against a defensive scenario: permissions.
// NormalizeLegacyUATScopes is documented to always return a non-nil slice,
// but if that invariant were ever violated, marshalCeilingPermissionIDs(nil)
// returns a nil *string, and dereferencing it directly would panic. The
// guard must report ok == false instead, so the caller can skip the row
// rather than default it to the literal "[]" — a persisted, intentionally
// issued empty ceiling is a different, stronger claim than "not yet
// resolved," and would stop the row from ever being reconsidered once the
// invariant is restored.
func TestPersistedCeilingColumnValue_NilNormalizeResultFailsClosed(t *testing.T) {
	var value string
	var ok bool
	require.NotPanics(t, func() {
		value, ok = persistedCeilingColumnValue(nil)
	})
	assert.False(t, ok, "a nil permission-ID list must not be treated as a persistable ceiling value")
	assert.Empty(t, value)
}

// TestPersistedCeilingColumnValue_EmptyNonNilListPersists confirms the
// ordinary, always-true-today case is unaffected by the guard above: a
// non-nil empty list (a real, intentionally empty ceiling) still persists as
// the literal "[]", distinct from the nil case.
func TestPersistedCeilingColumnValue_EmptyNonNilListPersists(t *testing.T) {
	value, ok := persistedCeilingColumnValue([]string{})
	require.True(t, ok)
	assert.Equal(t, "[]", value)
}

// TestCompositeStore_UATCeilingBackfillPageSizeOrDefault pins the page-size
// resolution BackfillUATCeilings reads, directly and independent of any row
// count: zero and negative field values fall back to the 500 default, and a
// positive value is used as is. This does not require a real store or
// client — the method only reads the one field.
func TestCompositeStore_UATCeilingBackfillPageSizeOrDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field int
		want  int
	}{
		{"zero uses the default", 0, defaultUATCeilingBackfillPageSize},
		{"negative uses the default", -1, defaultUATCeilingBackfillPageSize},
		{"positive value is used as is", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := &CompositeStore{uatCeilingBackfillPageSize: tc.field}
			assert.Equal(t, tc.want, cs.uatCeilingBackfillPageSizeOrDefault())
		})
	}
}
