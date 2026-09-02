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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// AC-1: Idempotence — second boot performs no migration writes
// ---------------------------------------------------------------------------

// TestBootDMKeyMigration_AlreadyComplete verifies that when the DM key
// migration marker is already present, runDMKeyMigration does not
// instantiate the migration service or perform any writes.
func TestBootDMKeyMigration_AlreadyComplete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed a conversation that would be migrated if the migration ran.
	seedOldFormatDMConversation(t, ctx, s)

	// Mark migration already complete.
	err := MarkMigrationComplete(ctx, s, MigrationDMKey, 0)
	require.NoError(t, err)

	// Run the boot hook — should skip.
	runDMKeyMigration(ctx, s)

	// Verify the old-format conversation was NOT modified.
	convs, err := s.ListConversations(ctx, store.ConversationFilter{Kind: "direct"}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, convs.Items, 1)

	conv := convs.Items[0]
	_, _, _, _, parseErr := messages.ParseDMKey(conv.ExternalRef)
	assert.Error(t, parseErr,
		"already-complete marker must cause skip; conversation should remain old-format")
}

// TestBootDMKeyMigration_IdempotentSecondBoot verifies AC-1: boot twice
// against a migrated database and the second boot performs no writes.
func TestBootDMKeyMigration_IdempotentSecondBoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seedOldFormatDMConversation(t, ctx, s)

	// First boot: runs the migration.
	runDMKeyMigration(ctx, s)

	// Verify migration ran and marker was written.
	done, err := IsMigrationComplete(ctx, s, MigrationDMKey)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written after first boot")

	// Record conversation state after first boot.
	convs, err := s.ListConversations(ctx, store.ConversationFilter{Kind: "direct"}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, convs.Items, 1)
	keyAfterFirstBoot := convs.Items[0].ExternalRef

	// Second boot: should skip.
	runDMKeyMigration(ctx, s)

	// Verify conversation is unchanged.
	convs, err = s.ListConversations(ctx, store.ConversationFilter{Kind: "direct"}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, convs.Items, 1)
	assert.Equal(t, keyAfterFirstBoot, convs.Items[0].ExternalRef,
		"second boot must not modify the conversation")
}

// ---------------------------------------------------------------------------
// AC-2: M-1' — both halves
// ---------------------------------------------------------------------------

// TestBootDMKeyMigration_RowRefusal_MarkerWritten verifies AC-2a: when the
// migration pass completes with row-level refusals (deterministic, non-
// retryable per-row outcomes), the completion marker IS written with the
// residual count. The next boot does not re-run.
//
// A test asserting the marker is ABSENT here would encode superseded M-1
// and create a livelock: on production data that is 11,593 deterministic
// refusals re-running on every boot forever, making no progress.
func TestBootDMKeyMigration_RowRefusal_MarkerWritten(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed a direct conversation with an old-format key using IDs that
	// cannot be resolved to a kind (no user/agent rows). This produces
	// a row-level refusal: the migration completes but this row is
	// "ambiguous" — found in neither table.
	id1 := uuid.NewString()
	id2 := uuid.NewString()
	if id1 > id2 {
		id1, id2 = id2, id1
	}
	oldKey := "dm:" + id1 + ":" + id2

	convID := uuid.NewString()
	err := s.CreateConversation(ctx, &store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})
	require.NoError(t, err)

	// Run the boot hook.
	runDMKeyMigration(ctx, s)

	// The marker MUST be written — row-level refusals do not block it.
	done, err := IsMigrationComplete(ctx, s, MigrationDMKey)
	require.NoError(t, err)
	assert.True(t, done,
		"M-1': row-level refusal must NOT block the marker (would livelock on production data)")

	// Verify the residual count is persisted.
	_, raw, err := loadMigrationsDoc(ctx, s)
	require.NoError(t, err)
	require.NotNil(t, raw)

	var marker migrationMarker
	err = unmarshalMigrationMarker(raw, MigrationDMKey, &marker)
	require.NoError(t, err)
	assert.Greater(t, marker.Residuals, 0,
		"residual count must be non-zero when rows were refused")

	// The conversation should be unmodified (kind resolution failed).
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	assert.Equal(t, oldKey, conv.ExternalRef,
		"unresolvable key must be left unmodified (fail-closed)")
}

// TestBootDMKeyMigration_RunLevelFailure_NoMarker verifies AC-2b: when
// the migration pass itself fails (could not list, could not write,
// context cancelled), no marker is written and the next boot retries.
func TestBootDMKeyMigration_RunLevelFailure_NoMarker(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// We need a run-level failure. The DMMigrationService returns a
	// non-nil error only when collectDirectConversations fails. We
	// achieve this by cancelling the context before running.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel() // immediately cancelled

	// Seed data that would be migrated normally.
	// Use the non-cancelled ctx for seeding.
	seedOldFormatDMConversation(t, ctx, s)

	// Run with the cancelled context — the listing query should fail.
	runDMKeyMigration(cancelledCtx, s)

	// The marker MUST NOT be written — the pass did not complete.
	done, err := IsMigrationComplete(ctx, s, MigrationDMKey)
	require.NoError(t, err)
	assert.False(t, done,
		"M-1': run-level failure must NOT write the marker; next boot must retry")
}

// ---------------------------------------------------------------------------
// AC-4: The repair works — old-format DM is re-keyed, access is restored
// ---------------------------------------------------------------------------

// TestBootDMKeyMigration_OldFormatRekeyed verifies AC-4: an old-format
// dm:<uuidA>:<uuidB> row is re-keyed to dm:<kind>:<uuid>:<kind>:<uuid>
// after one boot. Before: isDMParticipant denies. After: access granted.
func TestBootDMKeyMigration_OldFormatRekeyed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	convID, userID, _ := seedOldFormatDMConversation(t, ctx, s)

	// Before migration: old-format key denies access.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)

	assert.False(t, isDMParticipantCheck(conv.ExternalRef, userID),
		"old-format key should deny access before migration")

	// Run the boot hook.
	runBootDataMigrations(ctx, s)

	// After migration: kind-encoded key grants access.
	conv, err = s.GetConversation(ctx, convID)
	require.NoError(t, err)

	kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(conv.ExternalRef)
	require.NoError(t, parseErr, "re-keyed conversation should parse as kind-encoded")

	// Verify both principals are named in the key.
	principals := map[string]string{idA: kindA, idB: kindB}
	_, hasUser := principals[userID]
	assert.True(t, hasUser, "user should be named in the re-keyed key")

	// The isDMParticipant check should now pass.
	assert.True(t, isDMParticipantCheck(conv.ExternalRef, userID),
		"re-keyed conversation should grant access to its own participants")
}

// isDMParticipantCheck replicates the isDMParticipant logic from
// handlers_chat_v2.go. We don't import it to avoid a circular dependency
// on pkg/hub; instead we replicate the exact check the design requires
// we assert against (AC-4: "Assert against isDMParticipant, not against
// the stored string").
func isDMParticipantCheck(key, userID string) bool {
	parts := strings.Split(key, ":")
	if len(parts) < 5 {
		return false
	}
	return (parts[1] == "user" && parts[2] == userID) ||
		(parts[3] == "user" && parts[4] == userID)
}

// ---------------------------------------------------------------------------
// AC-5: Fail-closed — unresolvable key is left unmodified
// ---------------------------------------------------------------------------

// TestBootDMKeyMigration_FailClosed verifies AC-5: a key that cannot be
// resolved to kinds is left unmodified and still denies. Re-keying is
// never best-effort.
func TestBootDMKeyMigration_FailClosed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create an old-format DM with IDs that exist in neither the user
	// nor agent table. Kind resolution will fail.
	id1 := uuid.NewString()
	id2 := uuid.NewString()
	if id1 > id2 {
		id1, id2 = id2, id1
	}
	oldKey := "dm:" + id1 + ":" + id2

	convID := uuid.NewString()
	err := s.CreateConversation(ctx, &store.Conversation{
		ID:          convID,
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: oldKey,
	})
	require.NoError(t, err)

	// Run the boot hook.
	runBootDataMigrations(ctx, s)

	// The key must be unmodified.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	assert.Equal(t, oldKey, conv.ExternalRef,
		"unresolvable key must be left unmodified (fail-closed)")

	// isDMParticipant must still deny.
	assert.False(t, isDMParticipantCheck(conv.ExternalRef, id1),
		"unresolvable key must still deny access")
}

// ---------------------------------------------------------------------------
// AC-6: Boot is never blocked
// ---------------------------------------------------------------------------

// TestBootDataMigrations_NeverBlocksBoot verifies AC-6: with the
// migration forced to fail, runBootDataMigrations returns normally
// (it never returns an error and must not panic).
func TestBootDataMigrations_NeverBlocksBoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Force a run-level failure by cancelling the context.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()

	// Seed data so the migration would attempt work.
	seedOldFormatDMConversation(t, ctx, s)

	// This must not panic or block. runBootDataMigrations has no return
	// value — it never returns an error. A panic is the only failure mode.
	assert.NotPanics(t, func() {
		runBootDataMigrations(cancelledCtx, s)
	}, "runBootDataMigrations must never block boot, even on failure")
}

// ---------------------------------------------------------------------------
// AC-10 / B14: Empty-ref row stays keyless
// ---------------------------------------------------------------------------

// TestBootDMKeyMigration_EmptyRefUntouched verifies that an empty-ref
// direct conversation row is left keyless after the boot hook runs.
// B14 ruling: deriving a key from the participant index would fabricate
// an ACL from the listing index, inverting direction of authority.
//
// The store API now validates that direct conversations must have a
// non-empty external_ref (the DEF-29 guard). The empty-ref row in
// production predates that guard, so we insert it via raw SQL to
// replicate the legacy state.
func TestBootDMKeyMigration_EmptyRefUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Insert via raw SQL to bypass the store validation that now
	// prevents creating direct conversations with empty external_ref.
	cs, ok := s.(*entadapter.CompositeStore)
	require.True(t, ok, "test store must be a CompositeStore for DB access")
	db := cs.DB()
	require.NotNil(t, db, "DB() must return a non-nil *sql.DB")

	convID := uuid.NewString()
	_, err := db.ExecContext(ctx,
		`INSERT INTO conversations (id, kind, surface, external_ref, drift_state, last_activity_at, created_at)
		 VALUES (?, 'direct', 'native', '', 'active', datetime('now'), datetime('now'))`,
		convID)
	require.NoError(t, err)

	// Run the boot hook.
	runBootDataMigrations(ctx, s)

	// The row must remain keyless.
	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	assert.Equal(t, "", conv.ExternalRef,
		"empty-ref row must stay keyless (B14); deriving a key would fabricate an ACL")
}

// ---------------------------------------------------------------------------
// Warning still fires
// ---------------------------------------------------------------------------

// TestBootDataMigrations_WarningStillFires verifies that the backfill
// warning still fires after the boot hook. Re-pointing the warning is
// M6, not this commit.
func TestBootDataMigrations_WarningStillFires(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed an unattributed message so the warning triggers.
	projectID := uuid.NewString()
	err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "warn-test-project",
		Slug: "warn-test-" + projectID[:8],
	})
	require.NoError(t, err)

	msgID := uuid.NewString()
	err = s.CreateMessage(ctx, &store.Message{
		ID:        msgID,
		ProjectID: projectID,
		ThreadID:  "thread:" + uuid.NewString(),
		Msg:       "test message for warning check",
		Sender:    "user:test",
		Recipient: "agent:test",
		// ConversationID is empty — this message is unattributed.
	})
	require.NoError(t, err)

	// Capture log output.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	origLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(origLogger)

	// Run the boot hook.
	runBootDataMigrations(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "Messages without conversation attribution detected",
		"the existing backfill warning must still fire after M4")
}

// Error log bounding tests are in boot_data_migrations_safety_test.go
// (no build tag, visible under the no_sqlite gate).

// ---------------------------------------------------------------------------
// Integration: full runBootDataMigrations flow
// ---------------------------------------------------------------------------

// TestBootDataMigrations_FullFlow exercises the complete boot hook: DM
// migration runs, marker is written, and warning still fires.
func TestBootDataMigrations_FullFlow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, userID, agentID := seedOldFormatDMConversation(t, ctx, s)

	// Also seed an unattributed message so the warning fires.
	projectID := uuid.NewString()
	err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "flow-test-project",
		Slug: "flow-test-" + projectID[:8],
	})
	require.NoError(t, err)

	err = s.CreateMessage(ctx, &store.Message{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		ThreadID:  "thread:" + uuid.NewString(),
		Msg:       "test message for full flow",
		Sender:    "user:test",
		Recipient: "agent:test",
	})
	require.NoError(t, err)

	// Capture log output.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	origLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(origLogger)

	// First boot.
	runBootDataMigrations(ctx, s)

	logOutput := buf.String()

	// DM migration should have run.
	assert.Contains(t, logOutput, "DM key migration: starting")
	assert.Contains(t, logOutput, "DM key migration: pass completed")

	// Marker should be written.
	done, err := IsMigrationComplete(ctx, s, MigrationDMKey)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written after migration pass")

	// Warning should still fire.
	assert.Contains(t, logOutput, "Messages without conversation attribution detected")

	// Verify the conversation was re-keyed.
	convs, err := s.ListConversations(ctx, store.ConversationFilter{Kind: "direct"}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, convs.Items, 1)

	conv := convs.Items[0]
	kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(conv.ExternalRef)
	require.NoError(t, parseErr, "conversation should be re-keyed to kind-encoded format")

	principals := map[string]string{idA: kindA, idB: kindB}
	assert.Equal(t, "user", principals[userID])
	assert.Equal(t, "agent", principals[agentID])

	// Second boot: should skip.
	buf.Reset()
	runBootDataMigrations(ctx, s)
	logOutput = buf.String()
	assert.Contains(t, logOutput, "already complete, skipping",
		"second boot should skip the migration")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// unmarshalMigrationMarker is a test helper to read a specific marker
// from the raw migrations document.
func unmarshalMigrationMarker(raw map[string]json.RawMessage, name MigrationName, out *migrationMarker) error {
	entry, ok := raw[string(name)]
	if !ok {
		return store.ErrNotFound
	}
	return json.Unmarshal(entry, out)
}
