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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestExternalStore(t *testing.T) *ExternalStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewExternalStore(client)
}

func TestExternalStore_GCPServiceAccountCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestExternalStore(t)

	projectID := uuid.NewString()
	sa := &store.GCPServiceAccount{
		ID:            uuid.NewString(),
		Scope:         "project",
		ScopeID:       projectID,
		Email:         "agent@project.iam.gserviceaccount.com",
		ProjectID:     projectID,
		DisplayName:   "Worker SA",
		DefaultScopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
		Verified:      true,
		VerifiedAt:    time.Now().UTC().Truncate(time.Second),
		CreatedBy:     "tester",
		Managed:       true,
		ManagedBy:     "hub-1",
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	got, err := s.GetGCPServiceAccount(ctx, sa.ID)
	require.NoError(t, err)
	assert.Equal(t, sa.Email, got.Email)
	assert.Equal(t, []string{"https://www.googleapis.com/auth/cloud-platform"}, got.DefaultScopes)
	assert.True(t, got.Verified)
	assert.False(t, got.VerifiedAt.IsZero())
	assert.True(t, got.Managed)

	// Duplicate (email, scope, scope_id) -> ErrAlreadyExists.
	dup := *sa
	dup.ID = uuid.NewString()
	assert.ErrorIs(t, s.CreateGCPServiceAccount(ctx, &dup), store.ErrAlreadyExists)

	// Update.
	got.DisplayName = "Renamed SA"
	got.Verified = false
	got.VerifiedAt = time.Time{}
	require.NoError(t, s.UpdateGCPServiceAccount(ctx, got))
	got, err = s.GetGCPServiceAccount(ctx, sa.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed SA", got.DisplayName)
	assert.False(t, got.Verified)
	assert.True(t, got.VerifiedAt.IsZero())

	// Filter + count.
	managed := true
	list, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{Scope: "project", Managed: &managed})
	require.NoError(t, err)
	assert.Len(t, list, 1)

	count, err := s.CountGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{Scope: "project"})
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Delete.
	require.NoError(t, s.DeleteGCPServiceAccount(ctx, sa.ID))
	_, err = s.GetGCPServiceAccount(ctx, sa.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, s.DeleteGCPServiceAccount(ctx, sa.ID), store.ErrNotFound)
}

func TestExternalStore_GitHubInstallation(t *testing.T) {
	ctx := context.Background()
	s := newTestExternalStore(t)

	inst := &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          999,
		Repositories:   []string{"acme/repo1", "acme/repo2"},
	}
	require.NoError(t, s.CreateGitHubInstallation(ctx, inst))

	got, err := s.GetGitHubInstallation(ctx, 12345)
	require.NoError(t, err)
	assert.Equal(t, "acme", got.AccountLogin)
	assert.Equal(t, store.GitHubInstallationStatusActive, got.Status)
	assert.Equal(t, []string{"acme/repo1", "acme/repo2"}, got.Repositories)

	// Create with the same installation_id is an idempotent no-op (INSERT OR IGNORE).
	dup := &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "changed",
		AppID:          1,
	}
	require.NoError(t, s.CreateGitHubInstallation(ctx, dup))
	got, err = s.GetGitHubInstallation(ctx, 12345)
	require.NoError(t, err)
	assert.Equal(t, "acme", got.AccountLogin, "duplicate create must not overwrite")

	// Repository lookup.
	found, err := s.GetInstallationForRepository(ctx, "acme/repo2")
	require.NoError(t, err)
	assert.Equal(t, int64(12345), found.InstallationID)

	_, err = s.GetInstallationForRepository(ctx, "other/repo")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Update.
	got.Repositories = []string{"acme/repo3"}
	got.Status = store.GitHubInstallationStatusSuspended
	require.NoError(t, s.UpdateGitHubInstallation(ctx, got))
	got, err = s.GetGitHubInstallation(ctx, 12345)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/repo3"}, got.Repositories)
	assert.Equal(t, store.GitHubInstallationStatusSuspended, got.Status)

	// List filter by status.
	active, err := s.ListGitHubInstallations(ctx, store.GitHubInstallationFilter{Status: store.GitHubInstallationStatusActive})
	require.NoError(t, err)
	assert.Empty(t, active)

	// Delete.
	require.NoError(t, s.DeleteGitHubInstallation(ctx, 12345))
	_, err = s.GetGitHubInstallation(ctx, 12345)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestExternalStore_UserAccessToken(t *testing.T) {
	ctx := context.Background()
	s := newTestExternalStore(t)

	userID := uuid.NewString()
	projectID := uuid.NewString()
	expires := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	token := &store.UserAccessToken{
		ID:        uuid.NewString(),
		UserID:    userID,
		Name:      "ci-token",
		Prefix:    "scion_pat_abc",
		KeyHash:   "hash-1",
		ProjectID: projectID,
		Scopes:    []string{"project:read", "agent:list"},
		ExpiresAt: &expires,
	}
	require.NoError(t, s.CreateUserAccessToken(ctx, token))

	got, err := s.GetUserAccessToken(ctx, token.ID)
	require.NoError(t, err)
	assert.Equal(t, "ci-token", got.Name)
	assert.Equal(t, []string{"project:read", "agent:list"}, got.Scopes)
	require.NotNil(t, got.ExpiresAt)

	// Lookup by hash.
	byHash, err := s.GetUserAccessTokenByHash(ctx, "hash-1")
	require.NoError(t, err)
	assert.Equal(t, token.ID, byHash.ID)

	_, err = s.GetUserAccessTokenByHash(ctx, "missing")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Duplicate hash -> ErrAlreadyExists.
	dup := *token
	dup.ID = uuid.NewString()
	assert.ErrorIs(t, s.CreateUserAccessToken(ctx, &dup), store.ErrAlreadyExists)

	// LastUsed update.
	require.NoError(t, s.UpdateUserAccessTokenLastUsed(ctx, token.ID))
	got, err = s.GetUserAccessToken(ctx, token.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastUsed)

	// Count active tokens.
	count, err := s.CountUserAccessTokens(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Revoke removes from active count but the row still exists.
	require.NoError(t, s.RevokeUserAccessToken(ctx, token.ID))
	got, err = s.GetUserAccessToken(ctx, token.ID)
	require.NoError(t, err)
	assert.True(t, got.Revoked)
	count, err = s.CountUserAccessTokens(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	list, err := s.ListUserAccessTokens(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// Delete.
	require.NoError(t, s.DeleteUserAccessToken(ctx, token.ID))
	_, err = s.GetUserAccessToken(ctx, token.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, s.RevokeUserAccessToken(ctx, token.ID), store.ErrNotFound)
}

// =============================================================================
// verification_status / verification_error persistence (P0.1)
// =============================================================================

// newTestExternalStoreWithClient is newTestExternalStore plus the raw Ent client,
// which the backfill tests need to force rows into the pre-migration shape the
// store's own write path will no longer produce.
func newTestExternalStoreWithClient(t *testing.T) (*ExternalStore, *ent.Client) {
	t.Helper()
	client := enttest.NewClient(t)
	return NewExternalStore(client), client
}

func newTestSA(projectID string) *store.GCPServiceAccount {
	return &store.GCPServiceAccount{
		ID:        uuid.NewString(),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     "sa-" + uuid.NewString()[:8] + "@proj.iam.gserviceaccount.com",
		ProjectID: projectID,
		CreatedBy: "tester",
	}
}

// The defect P0.1 fixes: a failed verification was reported once in the HTTP
// response and then lost, because status was recomputed from the bool on read.
func TestExternalStore_GCPServiceAccount_FailedVerificationSurvivesRead(t *testing.T) {
	ctx := context.Background()
	s := newTestExternalStore(t)

	sa := newTestSA(uuid.NewString())
	sa.Verified = false
	sa.VerificationStatus = store.GCPVerificationFailed
	sa.VerificationError = "caller does not have permission to impersonate"
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	got, err := s.GetGCPServiceAccount(ctx, sa.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPVerificationFailed, got.VerificationStatus)
	assert.Equal(t, "caller does not have permission to impersonate", got.VerificationError)

	// ...and through a list, which reads via the same converter.
	list, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{ScopeID: sa.ScopeID})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.GCPVerificationFailed, list[0].VerificationStatus)
	assert.Equal(t, "caller does not have permission to impersonate", list[0].VerificationError)
}

// The verify handler's sequence: register, fail, then succeed on retry.
func TestExternalStore_GCPServiceAccount_UpdatePersistsVerificationTransitions(t *testing.T) {
	ctx := context.Background()
	s := newTestExternalStore(t)

	sa := newTestSA(uuid.NewString())
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	got, err := s.GetGCPServiceAccount(ctx, sa.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPVerificationUnverified, got.VerificationStatus,
		"a freshly registered account has not been checked yet")
	assert.Empty(t, got.VerificationError)

	got.Verified = false
	got.VerificationStatus = store.GCPVerificationFailed
	got.VerificationError = "IAM policy missing tokenCreator"
	require.NoError(t, s.UpdateGCPServiceAccount(ctx, got))

	got, err = s.GetGCPServiceAccount(ctx, sa.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPVerificationFailed, got.VerificationStatus)
	assert.Equal(t, "IAM policy missing tokenCreator", got.VerificationError)

	got.Verified = true
	got.VerifiedAt = time.Now()
	got.VerificationStatus = store.GCPVerificationVerified
	got.VerificationError = ""
	require.NoError(t, s.UpdateGCPServiceAccount(ctx, got))

	got, err = s.GetGCPServiceAccount(ctx, sa.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPVerificationVerified, got.VerificationStatus)
	assert.Empty(t, got.VerificationError, "a successful re-verify must clear the stale error")
}

// Callers that set only the bool must still produce a coherent row, and the
// struct they hold must match what was written.
func TestExternalStore_GCPServiceAccount_NormalizesUnsetStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestExternalStore(t)

	verified := newTestSA(uuid.NewString())
	verified.Verified = true
	require.NoError(t, s.CreateGCPServiceAccount(ctx, verified))
	assert.Equal(t, store.GCPVerificationVerified, verified.VerificationStatus,
		"create should write the resolved status back into the caller's struct")

	got, err := s.GetGCPServiceAccount(ctx, verified.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPVerificationVerified, got.VerificationStatus)

	unverified := newTestSA(uuid.NewString())
	require.NoError(t, s.CreateGCPServiceAccount(ctx, unverified))
	got, err = s.GetGCPServiceAccount(ctx, unverified.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPVerificationUnverified, got.VerificationStatus)
}

func TestExternalStore_BackfillGCPVerificationStatus(t *testing.T) {
	ctx := context.Background()
	s, client := newTestExternalStoreWithClient(t)

	// Build the four row shapes, then force verification_status back to the
	// column default to simulate rows that predate the column. Going through
	// the store first keeps the rest of each row realistic.
	type seed struct {
		name       string
		verified   bool
		verifiedAt time.Time
		want       string
	}
	seeds := []seed{
		{"verified", true, time.Now(), store.GCPVerificationVerified},
		{"regressed", false, time.Now(), store.GCPVerificationFailed},
		{"never checked", false, time.Time{}, store.GCPVerificationUnverified},
	}

	ids := make([]uuid.UUID, len(seeds))
	for i, sd := range seeds {
		sa := newTestSA(uuid.NewString())
		sa.Verified = sd.verified
		sa.VerifiedAt = sd.verifiedAt
		require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

		id := uuid.MustParse(sa.ID)
		ids[i] = id
		require.NoError(t, client.GCPServiceAccount.UpdateOneID(id).
			SetVerificationStatus(store.GCPVerificationUnverified).Exec(ctx))
	}

	require.NoError(t, s.BackfillGCPVerificationStatus(ctx))

	for i, sd := range seeds {
		got, err := s.GetGCPServiceAccount(ctx, ids[i].String())
		require.NoError(t, err)
		assert.Equal(t, sd.want, got.VerificationStatus, "seed %q", sd.name)
		assert.Empty(t, got.VerificationError,
			"seed %q: the error text is not recoverable and must not be invented", sd.name)
	}

	// Idempotent.
	require.NoError(t, s.BackfillGCPVerificationStatus(ctx))
	for i, sd := range seeds {
		got, err := s.GetGCPServiceAccount(ctx, ids[i].String())
		require.NoError(t, err)
		assert.Equal(t, sd.want, got.VerificationStatus, "seed %q after second run", sd.name)
	}
}

// A first-time verification failure leaves verified=false with verified_at NULL
// — the same shape as an account nobody ever checked. The backfill must not
// treat that as a row to relabel, or every boot would erase a real failure.
func TestExternalStore_BackfillGCPVerificationStatus_DoesNotClobberLiveFailures(t *testing.T) {
	ctx := context.Background()
	s := newTestExternalStore(t)

	sa := newTestSA(uuid.NewString())
	sa.Verified = false
	sa.VerificationStatus = store.GCPVerificationFailed
	sa.VerificationError = "no such service account"
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	require.NoError(t, s.BackfillGCPVerificationStatus(ctx))

	got, err := s.GetGCPServiceAccount(ctx, sa.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPVerificationFailed, got.VerificationStatus)
	assert.Equal(t, "no such service account", got.VerificationError)
}

func TestExternalStore_BackfillGCPVerificationStatus_EmptyTable(t *testing.T) {
	s := newTestExternalStore(t)
	assert.NoError(t, s.BackfillGCPVerificationStatus(context.Background()))
}
