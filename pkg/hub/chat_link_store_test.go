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

package hub

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/chatlinkcode"
)

// newTestChatLinkStore creates a ChatLinkStore backed by an in-memory SQLite DB
// with the schema auto-migrated. Each test gets an isolated database.
func newTestChatLinkStore(t *testing.T) *ChatLinkStore {
	t.Helper()

	client, err := ent.Open("sqlite3", fmt.Sprintf("file:%s?mode=memory&cache=shared&_fk=1", t.Name()))
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() }) //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, client.Schema.Create(ctx))

	return NewChatLinkStore(client)
}

// ---------------------------------------------------------------------------
// hashCode tests
// ---------------------------------------------------------------------------

func TestHashCode_Deterministic(t *testing.T) {
	h1 := hashCode("ABC123")
	h2 := hashCode("ABC123")
	assert.Equal(t, h1, h2, "same input should produce same hash")
}

func TestHashCode_CaseInsensitive(t *testing.T) {
	h1 := hashCode("abc123")
	h2 := hashCode("ABC123")
	assert.Equal(t, h1, h2, "hash should be case-insensitive (uppercased before hashing)")
}

func TestHashCode_DifferentCodes(t *testing.T) {
	h1 := hashCode("ABC123")
	h2 := hashCode("XYZ789")
	assert.NotEqual(t, h1, h2, "different codes should produce different hashes")
}

func TestHashCode_NotEmpty(t *testing.T) {
	h := hashCode("TESTCODE")
	assert.NotEmpty(t, h)
	assert.Len(t, h, 64, "SHA-256 hex digest should be 64 characters")
}

// ---------------------------------------------------------------------------
// ChatLinkStore DB-backed tests
// ---------------------------------------------------------------------------

func TestChatLinkStore_RegisterAndVerify(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	err := store.RegisterCode(ctx, "ABC123", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	uid, reason := store.VerifyCode(ctx, "ABC123", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)
}

func TestChatLinkStore_VerifyCaseInsensitive(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	err := store.RegisterCode(ctx, "abc123", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	// Verify with different case should work (codes are uppercased before hashing).
	uid, reason := store.VerifyCode(ctx, "ABC123", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)
}

func TestChatLinkStore_VerifyNotFound(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	uid, reason := store.VerifyCode(ctx, "NOTEXIST", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Equal(t, "code_not_found", reason)
	assert.Empty(t, uid)
}

func TestChatLinkStore_VerifyExpired(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	// Register with a TTL that has already expired.
	err := store.RegisterCode(ctx, "EXP001", "tg-user-1", chatlinkcode.ProviderTelegram, -1*time.Minute)
	require.NoError(t, err)

	uid, reason := store.VerifyCode(ctx, "EXP001", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Equal(t, "code_expired", reason)
	assert.Empty(t, uid)
}

func TestChatLinkStore_VerifyAlreadyConfirmed(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	err := store.RegisterCode(ctx, "DUP001", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	// First verify succeeds.
	uid, reason := store.VerifyCode(ctx, "DUP001", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)

	// Second verify returns the user identifier (already confirmed).
	uid, reason = store.VerifyCode(ctx, "DUP001", chatlinkcode.ProviderTelegram, "scion-user-2", "user2@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)
}

func TestChatLinkStore_RegisterReplacesOldCode(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	err := store.RegisterCode(ctx, "OLD001", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	err = store.RegisterCode(ctx, "NEW001", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	// Old code should no longer work.
	_, reason := store.VerifyCode(ctx, "OLD001", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Equal(t, "code_not_found", reason)

	// New code should work.
	uid, reason := store.VerifyCode(ctx, "NEW001", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)
}

func TestChatLinkStore_GetStatusByUser(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	// Not found.
	status, _, _ := store.GetStatusByUser(ctx, chatlinkcode.ProviderTelegram, "nonexistent")
	assert.Equal(t, "not_found", status)

	// Pending.
	err := store.RegisterCode(ctx, "ST001", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	status, userID, userEmail := store.GetStatusByUser(ctx, chatlinkcode.ProviderTelegram, "tg-user-1")
	assert.Equal(t, "pending", status)
	assert.Empty(t, userID)
	assert.Empty(t, userEmail)

	// Confirmed.
	_, _ = store.VerifyCode(ctx, "ST001", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")

	status, userID, userEmail = store.GetStatusByUser(ctx, chatlinkcode.ProviderTelegram, "tg-user-1")
	assert.Equal(t, "confirmed", status)
	assert.Equal(t, "scion-user-1", userID)
	assert.Equal(t, "user@example.com", userEmail)
}

func TestChatLinkStore_GetStatusByUser_Expired(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	err := store.RegisterCode(ctx, "EXPST", "tg-user-1", chatlinkcode.ProviderTelegram, -1*time.Minute)
	require.NoError(t, err)

	status, _, _ := store.GetStatusByUser(ctx, chatlinkcode.ProviderTelegram, "tg-user-1")
	assert.Equal(t, "expired", status)
}

func TestChatLinkStore_RegisterDoesNotDeleteConfirmed(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	// Register and confirm a code.
	err := store.RegisterCode(ctx, "CONF01", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	uid, reason := store.VerifyCode(ctx, "CONF01", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)

	// Register a new code for the same user. The confirmed code should NOT
	// be deleted — confirmed codes represent completed account links.
	err = store.RegisterCode(ctx, "CONF02", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	// GetStatusByUser returns the most recently created entry.
	// The new pending code was created after the confirmed code.
	status, _, _ := store.GetStatusByUser(ctx, chatlinkcode.ProviderTelegram, "tg-user-1")
	assert.Equal(t, "pending", status, "most recent code (pending) should be returned")

	// The confirmed code should still be verifiable — proving it was not deleted.
	uid, reason = store.VerifyCode(ctx, "CONF01", chatlinkcode.ProviderTelegram, "scion-user-2", "user2@example.com")
	assert.Empty(t, reason, "confirmed code should still exist")
	assert.Equal(t, "tg-user-1", uid)
}

func TestChatLinkStore_ConsumePending(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	err := store.RegisterCode(ctx, "CON001", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	_, _ = store.VerifyCode(ctx, "CON001", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")

	store.ConsumePending(ctx, chatlinkcode.ProviderTelegram, "tg-user-1")

	status, _, _ := store.GetStatusByUser(ctx, chatlinkcode.ProviderTelegram, "tg-user-1")
	assert.Equal(t, "not_found", status)
}

func TestChatLinkStore_PurgeExpired(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	// Register one expired and one valid code.
	err := store.RegisterCode(ctx, "PURGE1", "tg-user-1", chatlinkcode.ProviderTelegram, -1*time.Minute)
	require.NoError(t, err)

	err = store.RegisterCode(ctx, "PURGE2", "tg-user-2", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	// Purge expired.
	err = store.PurgeExpired(ctx)
	require.NoError(t, err)

	// Expired should be gone.
	_, reason := store.VerifyCode(ctx, "PURGE1", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Equal(t, "code_not_found", reason)

	// Valid should remain.
	uid, reason := store.VerifyCode(ctx, "PURGE2", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-2", uid)
}

func TestChatLinkStore_ProviderIsolation(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	// Different codes for different providers should not interfere.
	err := store.RegisterCode(ctx, "TGCODE", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	err = store.RegisterCode(ctx, "DCCODE", "dc-user-1", chatlinkcode.ProviderDiscord, 15*time.Minute)
	require.NoError(t, err)

	// Verify the telegram code against discord provider should fail.
	_, reason := store.VerifyCode(ctx, "TGCODE", chatlinkcode.ProviderDiscord, "scion-user-1", "user@example.com")
	assert.Equal(t, "code_not_found", reason)

	// Verify the discord code against telegram provider should fail.
	_, reason = store.VerifyCode(ctx, "DCCODE", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Equal(t, "code_not_found", reason)

	// Correct provider matches work.
	uid, reason := store.VerifyCode(ctx, "TGCODE", chatlinkcode.ProviderTelegram, "scion-user-1", "user@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)

	uid, reason = store.VerifyCode(ctx, "DCCODE", chatlinkcode.ProviderDiscord, "scion-user-1", "user@example.com")
	assert.Empty(t, reason)
	assert.Equal(t, "dc-user-1", uid)
}

func TestChatLinkStore_ConcurrentVerify_AtomicUpdate(t *testing.T) {
	store := newTestChatLinkStore(t)
	ctx := context.Background()

	err := store.RegisterCode(ctx, "RACE01", "tg-user-1", chatlinkcode.ProviderTelegram, 15*time.Minute)
	require.NoError(t, err)

	// Launch multiple concurrent verifications.
	const n = 10
	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			uid, reason := store.VerifyCode(ctx, "RACE01", chatlinkcode.ProviderTelegram,
				fmt.Sprintf("user-%d", idx), fmt.Sprintf("user%d@test.com", idx))
			if reason == "" && uid == "tg-user-1" {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	// All verifications should succeed because after the first one transitions
	// the status to "confirmed", subsequent ones see "confirmed" and return
	// the user identifier (the code is one-time-use for the pending→confirmed
	// transition, but already-confirmed codes are still readable). The important
	// invariant is that exactly one goroutine performs the pending→confirmed
	// state transition (via the conditional UPDATE), and all others either see
	// the confirmed state or get code_not_found.
	assert.Greater(t, successCount, 0, "at least one concurrent verify should succeed")
}

// ---------------------------------------------------------------------------
// In-memory fallback tests (no DB store set)
// ---------------------------------------------------------------------------

func TestTelegramLinkService_FallsBackToInMemory(t *testing.T) {
	svc := NewTelegramLinkService()
	defer svc.Close()

	svc.RegisterCode("TELE01", "tg-user-1")

	uid, reason := svc.VerifyCode("TELE01", "user-1", "user@test.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)

	status, userID, email := svc.GetStatusByTelegramUser("tg-user-1")
	assert.Equal(t, "confirmed", status)
	assert.Equal(t, "user-1", userID)
	assert.Equal(t, "user@test.com", email)
}

func TestDiscordLinkService_FallsBackToInMemory(t *testing.T) {
	svc := NewDiscordLinkService()
	defer svc.Close()

	svc.RegisterCode("DISC01", "dc-user-1")

	uid, reason := svc.VerifyCode("DISC01", "user-1", "user@test.com")
	assert.Empty(t, reason)
	assert.Equal(t, "dc-user-1", uid)

	status, userID, email := svc.GetStatusByDiscordUser("dc-user-1")
	assert.Equal(t, "confirmed", status)
	assert.Equal(t, "user-1", userID)
	assert.Equal(t, "user@test.com", email)
}

func TestTeamsLinkService_FallsBackToInMemory_WithStore(t *testing.T) {
	svc := NewTeamsLinkService()
	defer svc.Close()

	svc.RegisterCode("TEAM01", "teams-user-1")

	uid, reason := svc.VerifyCode("TEAM01", "user-1", "user@test.com")
	assert.Empty(t, reason)
	assert.Equal(t, "teams-user-1", uid)

	svc.ConsumePending("teams-user-1")

	status, _, _ := svc.GetStatusByTeamsUser("teams-user-1")
	assert.Equal(t, "not_found", status)
}
