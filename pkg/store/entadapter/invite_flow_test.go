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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// IsUserInvitedOrActive tests
// ============================================================================

func TestIsUserInvitedOrActive_InvitedUser(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create an invited user.
	err := cs.CreateUser(ctx, &store.User{
		ID:      uuid.New().String(),
		Email:   "invited@example.com",
		Status:  store.UserStatusInvited,
		Role:    store.UserRoleMember,
		Created: time.Now(),
	})
	require.NoError(t, err)

	found, err := cs.IsUserInvitedOrActive(ctx, "invited@example.com")
	require.NoError(t, err)
	assert.True(t, found, "invited user should be found")
}

func TestIsUserInvitedOrActive_ActiveUser(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create an active user.
	err := cs.CreateUser(ctx, &store.User{
		ID:          uuid.New().String(),
		Email:       "active@example.com",
		DisplayName: "Active User",
		Status:      store.UserStatusActive,
		Role:        store.UserRoleMember,
		Created:     time.Now(),
	})
	require.NoError(t, err)

	found, err := cs.IsUserInvitedOrActive(ctx, "active@example.com")
	require.NoError(t, err)
	assert.True(t, found, "active user should be found")
}

func TestIsUserInvitedOrActive_SuspendedUser(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create a suspended user.
	err := cs.CreateUser(ctx, &store.User{
		ID:          uuid.New().String(),
		Email:       "suspended@example.com",
		DisplayName: "Suspended User",
		Status:      store.UserStatusSuspended,
		Role:        store.UserRoleMember,
		Created:     time.Now(),
	})
	require.NoError(t, err)

	found, err := cs.IsUserInvitedOrActive(ctx, "suspended@example.com")
	require.NoError(t, err)
	assert.False(t, found, "suspended user should not be found")
}

func TestIsUserInvitedOrActive_NonexistentUser(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	found, err := cs.IsUserInvitedOrActive(ctx, "nobody@example.com")
	require.NoError(t, err)
	assert.False(t, found, "nonexistent user should not be found")
}

func TestIsUserInvitedOrActive_CaseInsensitive(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	err := cs.CreateUser(ctx, &store.User{
		ID:      uuid.New().String(),
		Email:   "alice@example.com",
		Status:  store.UserStatusInvited,
		Role:    store.UserRoleMember,
		Created: time.Now(),
	})
	require.NoError(t, err)

	found, err := cs.IsUserInvitedOrActive(ctx, "Alice@Example.COM")
	require.NoError(t, err)
	assert.True(t, found, "email lookup should be case-insensitive")
}

// ============================================================================
// Data Migration tests
// ============================================================================

func TestMigrateAllowListToInvitedUsers_CreatesNewUsers(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Add AllowListEntries for users that don't exist yet.
	for _, entry := range []*store.AllowListEntry{
		{ID: uuid.New().String(), Email: "alice@co.com", AddedBy: "admin@co.com", Note: "Workshop attendee", Created: time.Now()},
		{ID: uuid.New().String(), Email: "bob@co.com", AddedBy: "admin@co.com", Created: time.Now()},
	} {
		require.NoError(t, cs.AddAllowListEntry(ctx, entry))
	}

	// Run migration.
	err := cs.MigrateAllowListToInvitedUsers(ctx)
	require.NoError(t, err)

	// Verify users were created with status=invited.
	alice, err := cs.GetUserByEmail(ctx, "alice@co.com")
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusInvited, alice.Status)
	assert.NotNil(t, alice.InvitedBy)
	assert.Equal(t, "admin@co.com", *alice.InvitedBy)
	assert.NotNil(t, alice.InviteNote)
	assert.Equal(t, "Workshop attendee", *alice.InviteNote)

	bob, err := cs.GetUserByEmail(ctx, "bob@co.com")
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusInvited, bob.Status)
	assert.NotNil(t, bob.InvitedBy)
	assert.Equal(t, "admin@co.com", *bob.InvitedBy)
}

func TestMigrateAllowListToInvitedUsers_BackfillsExisting(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create an existing active user (simulating someone who logged in before).
	err := cs.CreateUser(ctx, &store.User{
		ID:          uuid.New().String(),
		Email:       "existing@co.com",
		DisplayName: "Existing User",
		Status:      store.UserStatusActive,
		Role:        store.UserRoleMember,
		Created:     time.Now(),
	})
	require.NoError(t, err)

	// Add an AllowListEntry for the same email.
	require.NoError(t, cs.AddAllowListEntry(ctx, &store.AllowListEntry{
		ID:      uuid.New().String(),
		Email:   "existing@co.com",
		AddedBy: "admin@co.com",
		Created: time.Now(),
	}))

	// Run migration.
	err = cs.MigrateAllowListToInvitedUsers(ctx)
	require.NoError(t, err)

	// Verify the existing user was NOT changed to invited, but invited_by was backfilled.
	user, err := cs.GetUserByEmail(ctx, "existing@co.com")
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, user.Status, "existing active user should stay active")
	assert.NotNil(t, user.InvitedBy)
	assert.Equal(t, "admin@co.com", *user.InvitedBy, "invited_by should be backfilled")
}

func TestMigrateAllowListToInvitedUsers_Idempotent(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Add AllowListEntry.
	require.NoError(t, cs.AddAllowListEntry(ctx, &store.AllowListEntry{
		ID:      uuid.New().String(),
		Email:   "idempotent@co.com",
		AddedBy: "admin@co.com",
		Created: time.Now(),
	}))

	// Run migration twice — should be safe.
	err := cs.MigrateAllowListToInvitedUsers(ctx)
	require.NoError(t, err)

	err = cs.MigrateAllowListToInvitedUsers(ctx)
	require.NoError(t, err)

	// Verify only one user exists.
	result, err := cs.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)

	count := 0
	for _, u := range result.Items {
		if u.Email == "idempotent@co.com" {
			count++
		}
	}
	assert.Equal(t, 1, count, "should have exactly one user record")
}

func TestMigrateAllowListToInvitedUsers_NoEntries(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Run migration with no entries — should be a no-op.
	err := cs.MigrateAllowListToInvitedUsers(ctx)
	require.NoError(t, err)
}

// ============================================================================
// User invited_by / invite_note field persistence tests
// ============================================================================

func TestUserInvitedByFieldPersistence(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	invitedBy := "admin@example.com"
	note := "Workshop attendee"
	err := cs.CreateUser(ctx, &store.User{
		ID:         uuid.New().String(),
		Email:      "invited-fields@example.com",
		Status:     store.UserStatusInvited,
		Role:       store.UserRoleMember,
		InvitedBy:  &invitedBy,
		InviteNote: &note,
		Created:    time.Now(),
	})
	require.NoError(t, err)

	user, err := cs.GetUserByEmail(ctx, "invited-fields@example.com")
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusInvited, user.Status)
	require.NotNil(t, user.InvitedBy)
	assert.Equal(t, "admin@example.com", *user.InvitedBy)
	require.NotNil(t, user.InviteNote)
	assert.Equal(t, "Workshop attendee", *user.InviteNote)
}

func TestUserInvitedToActiveTransition(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	invitedBy := "admin@example.com"
	userID := uuid.New().String()
	err := cs.CreateUser(ctx, &store.User{
		ID:        userID,
		Email:     "transition@example.com",
		Status:    store.UserStatusInvited,
		Role:      store.UserRoleMember,
		InvitedBy: &invitedBy,
		Created:   time.Now(),
	})
	require.NoError(t, err)

	// Simulate the transition that provisionUser does.
	user, err := cs.GetUserByEmail(ctx, "transition@example.com")
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusInvited, user.Status)

	user.Status = store.UserStatusActive
	user.DisplayName = "Transition User"
	user.AvatarURL = "https://example.com/avatar.png"
	user.LastLogin = time.Now()
	err = cs.UpdateUser(ctx, user)
	require.NoError(t, err)

	// Verify the transition.
	updated, err := cs.GetUserByEmail(ctx, "transition@example.com")
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, updated.Status)
	assert.Equal(t, "Transition User", updated.DisplayName)
	assert.Equal(t, "https://example.com/avatar.png", updated.AvatarURL)
	// InvitedBy should still be set (provenance is preserved).
	require.NotNil(t, updated.InvitedBy)
	assert.Equal(t, "admin@example.com", *updated.InvitedBy)
}
