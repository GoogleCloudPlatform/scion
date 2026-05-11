//go:build !no_sqlite

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

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGCPServiceAccount_CRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:            "sa-1",
		Scope:         store.ScopeProject,
		ScopeID:       "project-1",
		Email:         "agent@project.iam.gserviceaccount.com",
		ProjectID:     "my-project",
		DisplayName:   "Agent Worker",
		DefaultScopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
		CreatedBy:     "user-1",
	}

	// Create
	err := s.CreateGCPServiceAccount(ctx, sa)
	require.NoError(t, err)
	assert.False(t, sa.CreatedAt.IsZero())

	// Get
	got, err := s.GetGCPServiceAccount(ctx, "sa-1")
	require.NoError(t, err)
	assert.Equal(t, "agent@project.iam.gserviceaccount.com", got.Email)
	assert.Equal(t, "my-project", got.ProjectID)
	assert.Equal(t, "Agent Worker", got.DisplayName)
	assert.Equal(t, []string{"https://www.googleapis.com/auth/cloud-platform"}, got.DefaultScopes)
	assert.False(t, got.Verified)
	assert.Equal(t, "user-1", got.CreatedBy)

	// Update (verify)
	got.Verified = true
	got.VerifiedAt = time.Now()
	err = s.UpdateGCPServiceAccount(ctx, got)
	require.NoError(t, err)

	got2, err := s.GetGCPServiceAccount(ctx, "sa-1")
	require.NoError(t, err)
	assert.True(t, got2.Verified)
	assert.False(t, got2.VerifiedAt.IsZero())

	// List
	list, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: "project-1",
	})
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "sa-1", list[0].ID)

	// List with email filter
	list, err = s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Email: "agent@project.iam.gserviceaccount.com",
	})
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// List with wrong filter
	list, err = s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		ScopeID: "project-999",
		Scope:   store.ScopeProject,
	})
	require.NoError(t, err)
	assert.Len(t, list, 0)

	// Delete
	err = s.DeleteGCPServiceAccount(ctx, "sa-1")
	require.NoError(t, err)

	_, err = s.GetGCPServiceAccount(ctx, "sa-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestGCPServiceAccount_DuplicateEmail(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	sa1 := &store.GCPServiceAccount{
		ID:        "sa-1",
		Scope:     store.ScopeProject,
		ScopeID:   "project-1",
		Email:     "agent@project.iam.gserviceaccount.com",
		ProjectID: "my-project",
		CreatedBy: "user-1",
	}
	err := s.CreateGCPServiceAccount(ctx, sa1)
	require.NoError(t, err)

	// Same email, same scope = should fail
	sa2 := &store.GCPServiceAccount{
		ID:        "sa-2",
		Scope:     store.ScopeProject,
		ScopeID:   "project-1",
		Email:     "agent@project.iam.gserviceaccount.com",
		ProjectID: "my-project",
		CreatedBy: "user-1",
	}
	err = s.CreateGCPServiceAccount(ctx, sa2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)

	// Same email, different scope = should succeed
	sa3 := &store.GCPServiceAccount{
		ID:        "sa-3",
		Scope:     store.ScopeProject,
		ScopeID:   "project-2",
		Email:     "agent@project.iam.gserviceaccount.com",
		ProjectID: "my-project",
		CreatedBy: "user-1",
	}
	err = s.CreateGCPServiceAccount(ctx, sa3)
	assert.NoError(t, err)
}

func TestGCPServiceAccount_ManagedFields(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:        "sa-managed-1",
		Scope:     store.ScopeProject,
		ScopeID:   "project-1",
		Email:     "scion-abc123@hub-project.iam.gserviceaccount.com",
		ProjectID: "hub-project",
		Managed:   true,
		ManagedBy: "hub-instance-1",
		CreatedBy: "user-1",
	}

	err := s.CreateGCPServiceAccount(ctx, sa)
	require.NoError(t, err)

	got, err := s.GetGCPServiceAccount(ctx, "sa-managed-1")
	require.NoError(t, err)
	assert.True(t, got.Managed)
	assert.Equal(t, "hub-instance-1", got.ManagedBy)

	// List with managed filter
	managed := true
	list, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: "project-1",
		Managed: &managed,
	})
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.True(t, list[0].Managed)

	// Create a non-managed SA
	sa2 := &store.GCPServiceAccount{
		ID:        "sa-byosa-1",
		Scope:     store.ScopeProject,
		ScopeID:   "project-1",
		Email:     "user-sa@other-project.iam.gserviceaccount.com",
		ProjectID: "other-project",
		Managed:   false,
		CreatedBy: "user-1",
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa2))

	// Filter managed=true should return only the managed one
	list, err = s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: "project-1",
		Managed: &managed,
	})
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "sa-managed-1", list[0].ID)

	// Filter managed=false should return only the BYOSA one
	notManaged := false
	list, err = s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: "project-1",
		Managed: &notManaged,
	})
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "sa-byosa-1", list[0].ID)

	// No managed filter should return both
	list, err = s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: "project-1",
	})
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestGCPServiceAccount_NotFound(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, err := s.GetGCPServiceAccount(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)

	err = s.DeleteGCPServiceAccount(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)

	err = s.UpdateGCPServiceAccount(ctx, &store.GCPServiceAccount{ID: "nonexistent"})
	assert.ErrorIs(t, err, store.ErrNotFound)
}
