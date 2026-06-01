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

func newTestEntClient(t *testing.T) *ent.Client {
	t.Helper()
	client := enttest.NewClient(t)
	return client
}

// TestUserStore_EmailCaseInsensitive verifies that email uniqueness and lookup
// are case-insensitive, preserving the legacy COLLATE NOCASE semantics.
func TestUserStore_EmailCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	us := NewUserStore(newTestEntClient(t))

	require.NoError(t, us.CreateUser(ctx, &store.User{
		ID:          uuid.NewString(),
		Email:       "Mixed.Case@Example.com",
		DisplayName: "Mixed",
		Role:        store.UserRoleMember,
		Status:      "active",
	}))

	// Lookup with a different case must find the same user.
	got, err := us.GetUserByEmail(ctx, "mixed.case@EXAMPLE.COM")
	require.NoError(t, err)
	assert.Equal(t, "mixed.case@example.com", got.Email, "email is normalized to lower case")

	// A case-variant insert must collide on the unique index.
	err = us.CreateUser(ctx, &store.User{
		ID:          uuid.NewString(),
		Email:       "MIXED.CASE@example.com",
		DisplayName: "Dup",
		Role:        store.UserRoleMember,
		Status:      "active",
	})
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

// TestUserStore_UpdateLastSeen verifies the dedicated last_seen mutator.
func TestUserStore_UpdateLastSeen(t *testing.T) {
	ctx := context.Background()
	us := NewUserStore(newTestEntClient(t))

	id := uuid.NewString()
	require.NoError(t, us.CreateUser(ctx, &store.User{
		ID: id, Email: "seen@example.com", DisplayName: "Seen",
		Role: store.UserRoleMember, Status: "active",
	}))

	ts := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, us.UpdateUserLastSeen(ctx, id, ts))

	got, err := us.GetUser(ctx, id)
	require.NoError(t, err)
	assert.WithinDuration(t, ts, got.LastSeen, time.Second)

	// Missing user → ErrNotFound.
	assert.ErrorIs(t, us.UpdateUserLastSeen(ctx, uuid.NewString(), ts), store.ErrNotFound)
}

// TestAllowList_BulkAddIdempotent verifies INSERT-OR-IGNORE semantics: emails
// already present or repeated within the batch are skipped, not errored.
func TestAllowList_BulkAddIdempotent(t *testing.T) {
	ctx := context.Background()
	as := NewAllowListStore(newTestEntClient(t))

	require.NoError(t, as.AddAllowListEntry(ctx, &store.AllowListEntry{
		ID: uuid.NewString(), Email: "existing@example.com", AddedBy: "admin",
	}))

	entries := []*store.AllowListEntry{
		{ID: uuid.NewString(), Email: "EXISTING@example.com", AddedBy: "admin"}, // dup of existing (case-insensitive)
		{ID: uuid.NewString(), Email: "new1@example.com", AddedBy: "admin"},
		{ID: uuid.NewString(), Email: "new2@example.com", AddedBy: "admin"},
		{ID: uuid.NewString(), Email: "New1@example.com", AddedBy: "admin"}, // dup within batch
	}
	added, skipped, err := as.BulkAddAllowListEntries(ctx, entries)
	require.NoError(t, err)
	assert.Equal(t, 2, added, "new1 and new2 are added")
	assert.Equal(t, 2, skipped, "existing and the repeated new1 are skipped")

	ok, err := as.IsEmailAllowListed(ctx, "NEW2@EXAMPLE.COM")
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestInvite_IncrementUseCount verifies the conditional increment honors
// revoked, expired, and max-uses guards.
func TestInvite_IncrementUseCount(t *testing.T) {
	ctx := context.Background()
	as := NewAllowListStore(newTestEntClient(t))

	mk := func(maxUses int, revoked bool, expires time.Time) string {
		id := uuid.NewString()
		require.NoError(t, as.CreateInviteCode(ctx, &store.InviteCode{
			ID: id, CodeHash: "h-" + id, CodePrefix: "scion_in", MaxUses: maxUses,
			Revoked: revoked, ExpiresAt: expires, CreatedBy: "admin",
		}))
		return id
	}

	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	// Redeemable: increments up to max_uses, then refuses.
	limited := mk(2, false, future)
	require.NoError(t, as.IncrementInviteUseCount(ctx, limited))
	require.NoError(t, as.IncrementInviteUseCount(ctx, limited))
	assert.ErrorIs(t, as.IncrementInviteUseCount(ctx, limited), store.ErrNotFound, "exhausted")

	got, err := as.GetInviteCode(ctx, limited)
	require.NoError(t, err)
	assert.Equal(t, 2, got.UseCount)

	// Unlimited (max_uses == 0) keeps incrementing.
	unlimited := mk(0, false, future)
	require.NoError(t, as.IncrementInviteUseCount(ctx, unlimited))
	require.NoError(t, as.IncrementInviteUseCount(ctx, unlimited))

	// Revoked and expired codes are not redeemable.
	assert.ErrorIs(t, as.IncrementInviteUseCount(ctx, mk(5, true, future)), store.ErrNotFound)
	assert.ErrorIs(t, as.IncrementInviteUseCount(ctx, mk(5, false, past)), store.ErrNotFound)
}

// TestInvite_GetStats verifies aggregate stats over invite codes and the allow
// list.
func TestInvite_GetStats(t *testing.T) {
	ctx := context.Background()
	as := NewAllowListStore(newTestEntClient(t))

	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	// Pending (redeemed once), exhausted, and expired codes.
	mk := func(maxUses, useCount int, revoked bool, expires time.Time) {
		id := uuid.NewString()
		require.NoError(t, as.CreateInviteCode(ctx, &store.InviteCode{
			ID: id, CodeHash: "h-" + id, CodePrefix: "scion_in", MaxUses: maxUses,
			UseCount: useCount, Revoked: revoked, ExpiresAt: expires, CreatedBy: "admin",
		}))
	}
	mk(5, 2, false, future) // pending, 2 redemptions
	mk(1, 1, false, future) // exhausted, 1 redemption
	mk(5, 3, false, past)   // expired, 3 redemptions

	require.NoError(t, as.AddAllowListEntry(ctx, &store.AllowListEntry{
		ID: uuid.NewString(), Email: "a@example.com", AddedBy: "admin",
	}))

	stats, err := as.GetInviteStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PendingInvites, "only the non-expired, non-exhausted code is pending")
	assert.Equal(t, 6, stats.TotalRedemptions, "2+1+3")
	assert.Equal(t, 1, stats.AllowListCount)
	assert.Len(t, stats.RecentRedemptions, 3, "all three have use_count > 0")
}

// TestAllowList_WithInvites verifies the manual join enriching entries with
// invite details.
func TestAllowList_WithInvites(t *testing.T) {
	ctx := context.Background()
	as := NewAllowListStore(newTestEntClient(t))

	inviteID := uuid.NewString()
	require.NoError(t, as.CreateInviteCode(ctx, &store.InviteCode{
		ID: inviteID, CodeHash: "h-" + inviteID, CodePrefix: "scion_inv_abc",
		MaxUses: 10, UseCount: 4, ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "admin",
	}))
	require.NoError(t, as.AddAllowListEntry(ctx, &store.AllowListEntry{
		ID: uuid.NewString(), Email: "linked@example.com", AddedBy: "admin", InviteID: inviteID,
	}))
	require.NoError(t, as.AddAllowListEntry(ctx, &store.AllowListEntry{
		ID: uuid.NewString(), Email: "unlinked@example.com", AddedBy: "admin",
	}))

	res, err := as.ListAllowListEntriesWithInvites(ctx, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, res.Items, 2)

	byEmail := map[string]store.AllowListEntryWithInvite{}
	for _, e := range res.Items {
		byEmail[e.Email] = e
	}
	linked := byEmail["linked@example.com"]
	assert.Equal(t, "scion_inv_abc", linked.InviteCodePrefix)
	assert.Equal(t, 10, linked.InviteMaxUses)
	assert.Equal(t, 4, linked.InviteUseCount)
	assert.Empty(t, byEmail["unlinked@example.com"].InviteCodePrefix)
}
