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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedUser creates a test user and returns its UUID string.
func seedUser(t *testing.T, ctx context.Context, s *ExternalIdentityStore, email string) string {
	t.Helper()
	// Use the ent client directly to create a user.
	u, err := s.client.User.Create().
		SetEmail(email).
		SetDisplayName("Test User").
		SetRole("member").
		SetStatus("active").
		Save(ctx)
	require.NoError(t, err)
	return u.ID.String()
}

func newTestExtIDStore(t *testing.T) *ExternalIdentityStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewExternalIdentityStore(client)
}

// ---------------------------------------------------------------------------
// Basic CRUD
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_CreateAndGet(t *testing.T) {
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID := seedUser(t, ctx, s, "alice@gmail.com")

	binding := &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "google-sub-123",
		UserID:   userID,
		Email:    "alice@gmail.com",
	}
	require.NoError(t, s.CreateExternalIdentity(ctx, binding))

	got, err := s.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "google-sub-123")
	require.NoError(t, err)
	assert.Equal(t, userID, got.UserID)
	assert.Equal(t, "alice@gmail.com", got.Email)
	assert.Equal(t, "google", got.Provider)
	assert.Equal(t, "google-sub-123", got.Subject)
	assert.False(t, got.CreatedAt.IsZero())
}

func TestExternalIdentityStore_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestExtIDStore(t)

	_, err := s.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestExternalIdentityStore_GetByUserID(t *testing.T) {
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID := seedUser(t, ctx, s, "multi@gmail.com")

	// Create two bindings for the same user.
	for i, sub := range []string{"sub-a", "sub-b"} {
		require.NoError(t, s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
			ID:       uuid.New().String(),
			Provider: "google",
			Issuer:   "https://accounts.google.com",
			Subject:  sub,
			UserID:   userID,
			Email:    "multi@gmail.com",
		}), "binding %d", i)
	}

	bindings, err := s.GetExternalIdentitiesByUserID(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, bindings, 2)
}

func TestExternalIdentityStore_UpdateEmail(t *testing.T) {
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID := seedUser(t, ctx, s, "update@gmail.com")

	bindingID := uuid.New().String()
	require.NoError(t, s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID:       bindingID,
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "sub-update",
		UserID:   userID,
		Email:    "old@gmail.com",
	}))

	require.NoError(t, s.UpdateExternalIdentityEmail(ctx, bindingID, "new@gmail.com"))

	got, err := s.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "sub-update")
	require.NoError(t, err)
	assert.Equal(t, "new@gmail.com", got.Email)
}

func TestExternalIdentityStore_UpdateEmail_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestExtIDStore(t)

	err := s.UpdateExternalIdentityEmail(ctx, uuid.New().String(), "new@gmail.com")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Unique constraint (transactional conflict safety)
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_UniqueConstraint(t *testing.T) {
	// Creating a second binding with the same (provider, issuer, subject)
	// must fail — this is the core conflict-safety requirement.
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID1 := seedUser(t, ctx, s, "user1@gmail.com")
	userID2 := seedUser(t, ctx, s, "user2@gmail.com")

	require.NoError(t, s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "unique-sub",
		UserID:   userID1,
		Email:    "user1@gmail.com",
	}))

	// Same (provider, issuer, subject) → different user: must fail.
	err := s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "unique-sub",
		UserID:   userID2,
		Email:    "user2@gmail.com",
	})
	assert.Error(t, err, "duplicate binding must fail")
}

// ---------------------------------------------------------------------------
// Canonical issuer aliases
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_IssuerAliases(t *testing.T) {
	// Verify that two different issuer strings (the canonical form and
	// a non-canonical form) produce separate bindings. The caller is
	// responsible for canonicalization before calling the store; the store
	// itself does exact matching.
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID := seedUser(t, ctx, s, "issuer@gmail.com")

	// Binding with canonical issuer.
	require.NoError(t, s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "issuer-sub",
		UserID:   userID,
		Email:    "issuer@gmail.com",
	}))

	// Lookup with canonical issuer: found.
	got, err := s.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "issuer-sub")
	require.NoError(t, err)
	assert.Equal(t, userID, got.UserID)

	// Lookup with non-canonical issuer: not found (store does exact match).
	_, err = s.GetExternalIdentity(ctx, "google", "accounts.google.com", "issuer-sub")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"store must do exact issuer match; canonicalization is caller's responsibility")
}

// ---------------------------------------------------------------------------
// Changed email / no-relink
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_ChangedEmail_NoRelink(t *testing.T) {
	// Updating the email field must NOT change the binding's user_id.
	// The binding is keyed by (provider, issuer, subject), not email.
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID := seedUser(t, ctx, s, "original@gmail.com")

	bindingID := uuid.New().String()
	require.NoError(t, s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID:       bindingID,
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "relink-sub",
		UserID:   userID,
		Email:    "original@gmail.com",
	}))

	// Update email.
	require.NoError(t, s.UpdateExternalIdentityEmail(ctx, bindingID, "changed@gmail.com"))

	// Verify the binding still points to the same user.
	got, err := s.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "relink-sub")
	require.NoError(t, err)
	assert.Equal(t, userID, got.UserID, "email change must not relink to a different user")
	assert.Equal(t, "changed@gmail.com", got.Email)
}

// ---------------------------------------------------------------------------
// Process restart persistence
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_PersistenceAcrossReconnect(t *testing.T) {
	// Simulates a Hub process restart: create a binding with one store
	// instance, then create a new store instance from the same ent client
	// (same underlying database) and verify the binding survives.
	ctx := context.Background()
	client := enttest.NewClient(t)
	s1 := NewExternalIdentityStore(client)
	userID := seedUser(t, ctx, s1, "persist@gmail.com")

	require.NoError(t, s1.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "persist-sub",
		UserID:   userID,
		Email:    "persist@gmail.com",
	}))

	// Create a new store instance (simulating reconnect after restart).
	s2 := NewExternalIdentityStore(client)

	got, err := s2.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "persist-sub")
	require.NoError(t, err, "binding must survive store reconnect (process restart)")
	assert.Equal(t, userID, got.UserID)
	assert.Equal(t, "persist@gmail.com", got.Email)
}

// ---------------------------------------------------------------------------
// Concurrent first linkage from simultaneous Hub instances
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_ConcurrentFirstLinkage(t *testing.T) {
	// Simulates two Hub instances racing to create the first binding for the
	// same external identity. Exactly one must succeed; the other must fail
	// due to the unique constraint.
	ctx := context.Background()
	client := enttest.NewClient(t)

	// Create two store instances (simulating two Hub replicas).
	s1 := NewExternalIdentityStore(client)
	s2 := NewExternalIdentityStore(client)

	userID1 := seedUser(t, ctx, s1, "race1@gmail.com")
	userID2 := seedUser(t, ctx, s2, "race2@gmail.com")

	binding1 := &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "race-sub",
		UserID:   userID1,
		Email:    "race1@gmail.com",
	}
	binding2 := &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "race-sub",
		UserID:   userID2,
		Email:    "race2@gmail.com",
	}

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		err1 = s1.CreateExternalIdentity(ctx, binding1)
	}()
	go func() {
		defer wg.Done()
		err2 = s2.CreateExternalIdentity(ctx, binding2)
	}()
	wg.Wait()

	// Exactly one must succeed and one must fail.
	successes := 0
	if err1 == nil {
		successes++
	}
	if err2 == nil {
		successes++
	}
	assert.Equal(t, 1, successes,
		"exactly one concurrent first linkage must succeed (unique constraint); err1=%v, err2=%v", err1, err2)

	// The binding that persisted must be consistent.
	got, err := s1.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "race-sub")
	require.NoError(t, err)
	// The winner's userID should be the one in the store.
	if err1 == nil {
		assert.Equal(t, userID1, got.UserID)
	} else {
		assert.Equal(t, userID2, got.UserID)
	}
}

// ---------------------------------------------------------------------------
// Stable linkage — same sub resolves to same user after restart
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_StableLinkage_AfterRestart(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := NewExternalIdentityStore(client)
	userID := seedUser(t, ctx, s, "stable@gmail.com")

	require.NoError(t, s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "stable-sub",
		UserID:   userID,
		Email:    "stable@gmail.com",
	}))

	// "Restart" — new store instance.
	s2 := NewExternalIdentityStore(client)

	// First lookup: binding exists.
	got1, err := s2.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "stable-sub")
	require.NoError(t, err)
	assert.Equal(t, userID, got1.UserID)

	// Second lookup: same result (stable).
	got2, err := s2.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "stable-sub")
	require.NoError(t, err)
	assert.Equal(t, got1.UserID, got2.UserID, "stable linkage: same sub must resolve to same user")
}

// ---------------------------------------------------------------------------
// Timestamps
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_Timestamps(t *testing.T) {
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID := seedUser(t, ctx, s, "time@gmail.com")

	before := time.Now().Add(-time.Second)
	require.NoError(t, s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		ID:       uuid.New().String(),
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "time-sub",
		UserID:   userID,
		Email:    "time@gmail.com",
	}))
	after := time.Now().Add(time.Second)

	got, err := s.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "time-sub")
	require.NoError(t, err)
	assert.True(t, got.CreatedAt.After(before) && got.CreatedAt.Before(after),
		"created_at should be around now, got %v", got.CreatedAt)
}

// ---------------------------------------------------------------------------
// Empty result set
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_GetByUserID_Empty(t *testing.T) {
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID := seedUser(t, ctx, s, "empty@gmail.com")

	bindings, err := s.GetExternalIdentitiesByUserID(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, bindings, 0)
}

// ---------------------------------------------------------------------------
// Cascade: deleting a user cascades to delete bindings
// ---------------------------------------------------------------------------

func TestExternalIdentityStore_UserDeleteCascade(t *testing.T) {
	ctx := context.Background()
	s := newTestExtIDStore(t)
	userID := seedUser(t, ctx, s, "cascade@gmail.com")

	// Create a binding for this user.
	err := s.CreateExternalIdentity(ctx, &store.ExternalIdentityBinding{
		Provider: "google",
		Issuer:   "https://accounts.google.com",
		Subject:  "cascade-sub-001",
		UserID:   userID,
		Email:    "cascade@gmail.com",
	})
	require.NoError(t, err)

	// Verify binding exists.
	got, err := s.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "cascade-sub-001")
	require.NoError(t, err)
	assert.Equal(t, userID, got.UserID)

	// Delete the user — cascade should delete the binding.
	uid, err := uuid.Parse(userID)
	require.NoError(t, err)
	err = s.client.User.DeleteOneID(uid).Exec(ctx)
	require.NoError(t, err)

	// Binding should now be gone.
	_, err = s.GetExternalIdentity(ctx, "google", "https://accounts.google.com", "cascade-sub-001")
	assert.Error(t, err, "binding should be deleted by cascade")

	// GetByUserID should return empty.
	bindings, err := s.GetExternalIdentitiesByUserID(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, bindings, 0, "bindings should be empty after cascade delete")
}
