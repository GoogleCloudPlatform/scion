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

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateAllowListEntryInviteID(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Add an allow-list entry
	entry := &store.AllowListEntry{
		ID:      uuid.New().String(),
		Email:   "user@example.com",
		Note:    "test user",
		AddedBy: "admin-1",
	}
	require.NoError(t, s.AddAllowListEntry(ctx, entry))

	// Update its invite ID
	inviteID := uuid.New().String()
	err := s.UpdateAllowListEntryInviteID(ctx, "user@example.com", inviteID)
	require.NoError(t, err)

	// Verify the update
	got, err := s.GetAllowListEntry(ctx, "user@example.com")
	require.NoError(t, err)
	assert.Equal(t, inviteID, got.InviteID)
}

func TestUpdateAllowListEntryInviteID_CaseInsensitive(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	entry := &store.AllowListEntry{
		ID:      uuid.New().String(),
		Email:   "user@example.com",
		Note:    "test",
		AddedBy: "admin-1",
	}
	require.NoError(t, s.AddAllowListEntry(ctx, entry))

	inviteID := uuid.New().String()
	err := s.UpdateAllowListEntryInviteID(ctx, "User@Example.COM", inviteID)
	require.NoError(t, err)

	got, err := s.GetAllowListEntry(ctx, "user@example.com")
	require.NoError(t, err)
	assert.Equal(t, inviteID, got.InviteID)
}

func TestUpdateAllowListEntryInviteID_NotFound(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	err := s.UpdateAllowListEntryInviteID(ctx, "nonexistent@example.com", "some-id")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestListAllowListEntriesWithInvites_NoInvite(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	entry := &store.AllowListEntry{
		ID:      uuid.New().String(),
		Email:   "user@example.com",
		Note:    "test",
		AddedBy: "admin-1",
	}
	require.NoError(t, s.AddAllowListEntry(ctx, entry))

	result, err := s.ListAllowListEntriesWithInvites(ctx, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)

	item := result.Items[0]
	assert.Equal(t, "user@example.com", item.Email)
	assert.Empty(t, item.InviteCodePrefix)
	assert.Zero(t, item.InviteMaxUses)
	assert.False(t, item.InviteRevoked)
}

func TestListAllowListEntriesWithInvites_WithLinkedInvite(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create an invite code
	invite := &store.InviteCode{
		ID:         uuid.New().String(),
		CodeHash:   "testhash123",
		CodePrefix: "scion_inv_abcdefgh",
		MaxUses:    1,
		UseCount:   0,
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		CreatedBy:  "admin-1",
		Note:       "test invite",
		Created:    time.Now(),
	}
	require.NoError(t, s.CreateInviteCode(ctx, invite))

	// Create an allow-list entry linked to the invite
	entry := &store.AllowListEntry{
		ID:       uuid.New().String(),
		Email:    "user@example.com",
		Note:     "test",
		AddedBy:  "admin-1",
		InviteID: invite.ID,
	}
	require.NoError(t, s.AddAllowListEntry(ctx, entry))

	result, err := s.ListAllowListEntriesWithInvites(ctx, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)

	item := result.Items[0]
	assert.Equal(t, "user@example.com", item.Email)
	assert.Equal(t, invite.ID, item.InviteID)
	assert.Equal(t, "scion_inv_abcdefgh", item.InviteCodePrefix)
	assert.Equal(t, 1, item.InviteMaxUses)
	assert.Equal(t, 0, item.InviteUseCount)
	assert.False(t, item.InviteRevoked)
	assert.False(t, item.InviteExpiresAt.IsZero())
}

func TestListAllowListEntriesWithInvites_RevokedInvite(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	invite := &store.InviteCode{
		ID:         uuid.New().String(),
		CodeHash:   "testhash456",
		CodePrefix: "scion_inv_xyz12345",
		MaxUses:    1,
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		Revoked:    true,
		CreatedBy:  "admin-1",
		Created:    time.Now(),
	}
	require.NoError(t, s.CreateInviteCode(ctx, invite))

	entry := &store.AllowListEntry{
		ID:       uuid.New().String(),
		Email:    "revoked@example.com",
		Note:     "test",
		AddedBy:  "admin-1",
		InviteID: invite.ID,
	}
	require.NoError(t, s.AddAllowListEntry(ctx, entry))

	result, err := s.ListAllowListEntriesWithInvites(ctx, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.True(t, result.Items[0].InviteRevoked)
}

func TestListAllowListEntriesWithInvites_MixedEntries(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create an invite
	invite := &store.InviteCode{
		ID:         uuid.New().String(),
		CodeHash:   "testhash789",
		CodePrefix: "scion_inv_mixed123",
		MaxUses:    5,
		UseCount:   2,
		ExpiresAt:  time.Now().Add(48 * time.Hour),
		CreatedBy:  "admin-1",
		Created:    time.Now(),
	}
	require.NoError(t, s.CreateInviteCode(ctx, invite))

	// Entry with invite
	entry1 := &store.AllowListEntry{
		ID:       uuid.New().String(),
		Email:    "with-invite@example.com",
		Note:     "has invite",
		AddedBy:  "admin-1",
		InviteID: invite.ID,
	}
	require.NoError(t, s.AddAllowListEntry(ctx, entry1))

	// Entry without invite
	entry2 := &store.AllowListEntry{
		ID:      uuid.New().String(),
		Email:   "no-invite@example.com",
		Note:    "no invite",
		AddedBy: "admin-1",
	}
	require.NoError(t, s.AddAllowListEntry(ctx, entry2))

	result, err := s.ListAllowListEntriesWithInvites(ctx, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)
	require.Len(t, result.Items, 2)

	// Find the entry with invite
	var withInvite, withoutInvite store.AllowListEntryWithInvite
	for _, item := range result.Items {
		if item.Email == "with-invite@example.com" {
			withInvite = item
		} else {
			withoutInvite = item
		}
	}

	assert.Equal(t, "scion_inv_mixed123", withInvite.InviteCodePrefix)
	assert.Equal(t, 5, withInvite.InviteMaxUses)
	assert.Equal(t, 2, withInvite.InviteUseCount)

	assert.Empty(t, withoutInvite.InviteCodePrefix)
	assert.Zero(t, withoutInvite.InviteMaxUses)
}
