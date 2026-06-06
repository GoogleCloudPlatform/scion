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
