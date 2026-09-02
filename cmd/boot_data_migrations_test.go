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
	"errors"
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

// listConversationsFailStore wraps a real store.Store and overrides
// ListConversations to return an error, simulating a run-level failure
// in DMMigrationService.collectDirectConversations. All other methods
// — critically including GetHubSetting and UpsertHubSetting — pass
// through to the real store on a live context.
//
// This is the test fixture for AC-2b. A cancelled-context approach is
// insufficient because it also disables the marker write, making the
// test tautological: the marker would be absent because the write was
// impossible, not because the guard refused it.
type listConversationsFailStore struct {
	store.Store
}

func (s *listConversationsFailStore) ListConversations(_ context.Context, _ store.ConversationFilter, _ store.ListOptions) (*store.ListResult[store.Conversation], error) {
	return nil, errors.New("injected: listing direct conversations failed")
}

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
// the migration pass itself fails (could not list conversations), no
// marker is written and the next boot retries.
//
// This test uses a store wrapper that fails ListConversations while
// leaving the marker-writing path (GetHubSetting / UpsertHubSetting)
// fully functional on a live context. This is critical: a cancelled-
// context approach would be tautological because the cancelled context
// also prevents the marker write, making the marker absent because
// the write was impossible rather than because the guard refused it.
//
// Mutation-tested: removing the `return` after the run-level error
// check in runDMKeyMigration causes this test to fail — the marker
// IS then written for a pass that did not complete, which is the
// exact bug AC-2b exists to prevent.
func TestBootDMKeyMigration_RunLevelFailure_NoMarker(t *testing.T) {
	ctx := context.Background()
	realStore := newTestStore(t)

	// Seed data that would be migrated if listing worked.
	seedOldFormatDMConversation(t, ctx, realStore)

	// Wrap the store: ListConversations fails, everything else works.
	failStore := &listConversationsFailStore{Store: realStore}

	// Run with a live context — the listing fails but the marker
	// write path is fully operational.
	runDMKeyMigration(ctx, failStore)

	// The marker MUST NOT be written — the pass did not complete.
	// Read from the real store (same underlying DB) on a live context.
	done, err := IsMigrationComplete(ctx, realStore, MigrationDMKey)
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

// TestBootDataMigrations_ReachableWarnFires verifies that the residual
// report emits a WARN for messages that remain unattributed in listed
// projects (M6 §4.6). The message is unattributable: it has no ThreadID
// and non-UUID principals, so key derivation fails (DeriveErrPrincipalPair).
// The backfill processes it, refuses it as a row-level refusal, and the
// WARN fires because conversation_id is still NULL and the project exists.
//
// Replaces the old TestBootDataMigrations_WarningStillFires whose
// precondition expired: it asserted "Messages without conversation
// attribution detected" which was the old maybeWarnUnbackfilledMessages
// message. M6 replaced that with the split reachable/unreachable report.
func TestBootDataMigrations_ReachableWarnFires(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed an unattributed message that cannot be attributed.
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
		// No ThreadID — forces principal-pair derivation path,
		// which fails on non-UUID principals.
		Msg:       "test message for warning check",
		Sender:    "user:alice@example.com",
		Recipient: "agent:some-bot",
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
	assert.Contains(t, logOutput, "Messages remain unattributed in listed projects",
		"WARN must fire for reachable unattributed messages")
	assert.NotContains(t, logOutput, "scion server backfill",
		"remediation string must not appear (M6 removed it)")
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

	// Also seed an unattributed message that cannot be attributed
	// (non-UUID principals, no ThreadID → DeriveErrPrincipalPair).
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
		// No ThreadID — forces principal-pair path, fails on non-UUID.
		Msg:       "test message for full flow",
		Sender:    "user:alice@example.com",
		Recipient: "agent:some-bot",
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

	// Markers should be written.
	done, err := IsMigrationComplete(ctx, s, MigrationDMKey)
	require.NoError(t, err)
	assert.True(t, done, "DM key marker should be written after migration pass")

	backfillDone, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	assert.NotNil(t, backfillDone.CompletedAt,
		"backfill marker should be written after backfill pass")

	// Reachable WARN should fire for the unattributable message
	// (it's in a valid project, so it's reachable but unattributed).
	assert.Contains(t, logOutput, "Messages remain unattributed in listed projects")

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
// AC-9: Residual report — reachable/unreachable split
// ---------------------------------------------------------------------------

// TestResidualReport_AC9 verifies acceptance criterion 9 (design §9):
//
//   - Seed one unattributed message in a listed project (reachable) and one
//     referencing a project ID with no row (unreachable/orphan).
//   - After the boot hook runs: the reachable one is attributed; the orphan
//     is reported as unreachable at INFO, not as a WARN; the WARN for
//     reachable messages does not fire; and no log line advertises
//     "scion server backfill --execute".
//   - The specific failure guarded against is the orphan being counted in
//     the actionable bucket — that is the bug that makes the warning
//     permanent.
//
// Steady-state case: run the boot hook a second time so every project is
// already in projects_done and no backfill work is performed. Both counts
// must still be correct and the WARN must still not fire.
func TestResidualReport_AC9(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// ---- Seed a reachable, attributable message ----
	userID := uuid.NewString()
	agentID := uuid.NewString()

	// Create user and agent so the backfill's principal resolution works.
	err := s.CreateUser(ctx, &store.User{
		ID:    userID,
		Email: "ac9-user@example.com",
		Role:  "member",
	})
	require.NoError(t, err)

	projectID := uuid.NewString()
	err = s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "ac9-reachable-project",
		Slug: "ac9-reach-" + projectID[:8],
	})
	require.NoError(t, err)

	err = s.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Name:      "ac9-agent",
		Slug:      "ac9-agent-" + agentID[:8],
		ProjectID: projectID,
	})
	require.NoError(t, err)

	reachableMsgID := uuid.NewString()
	err = s.CreateMessage(ctx, &store.Message{
		ID:          reachableMsgID,
		ProjectID:   projectID,
		Msg:         "reachable message for AC-9",
		Sender:      "user:" + userID,
		SenderID:    userID,
		Recipient:   "agent:" + agentID,
		RecipientID: agentID,
		// No ThreadID — principal-pair derivation with valid UUIDs.
		// ConversationID empty — this is unattributed.
	})
	require.NoError(t, err)

	// ---- Seed an unreachable orphan message ----
	// This message references a project_id that has no row in the projects
	// table, simulating a hard-deleted project (DEF-111).
	orphanProjectID := uuid.NewString() // no CreateProject for this
	orphanMsgID := uuid.NewString()
	orphanUserID := uuid.NewString()
	orphanAgentID := uuid.NewString()

	// Must insert via raw SQL because CreateMessage may validate project_id
	// existence in some store implementations.
	cs, ok := s.(*entadapter.CompositeStore)
	require.True(t, ok, "test store must be a CompositeStore for DB access")
	db := cs.DB()
	require.NotNil(t, db)

	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, project_id, msg, sender, sender_id, recipient, recipient_id, created)
		 VALUES (?, ?, 'orphan message for AC-9', ?, ?, ?, ?, datetime('now'))`,
		orphanMsgID, orphanProjectID,
		"user:"+orphanUserID, orphanUserID,
		"agent:"+orphanAgentID, orphanAgentID,
	)
	require.NoError(t, err)

	// ---- Capture log output ----
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	origLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(origLogger)

	// ---- First boot ----
	runBootDataMigrations(ctx, s)

	logOutput := buf.String()

	// 1. The reachable message must be attributed (conversation_id set).
	msg, err := s.GetMessage(ctx, reachableMsgID)
	require.NoError(t, err)
	assert.NotEmpty(t, msg.ConversationID,
		"reachable message must be attributed after boot hook")

	// 2. The orphan must be reported as unreachable at INFO, not WARN.
	assert.Contains(t, logOutput, "Message attribution complete",
		"INFO line must appear reporting unreachable count")
	assert.Contains(t, logOutput, "unreachable=1",
		"unreachable count must be 1 (the orphan)")
	assert.Contains(t, logOutput, "hard-deleted projects",
		"INFO detail must mention hard-deleted projects (DEF-111)")

	// 3. The WARN for reachable messages must NOT fire (the reachable one
	//    was attributed, so reachable count is 0).
	assert.NotContains(t, logOutput, "Messages remain unattributed in listed projects",
		"WARN must not fire when all reachable messages are attributed")

	// 4. No log line advertises the backfill command.
	assert.NotContains(t, logOutput, "scion server backfill",
		"no log line may advertise 'scion server backfill --execute' (M6)")

	// 5. The specific failure to test for: the orphan must NOT be counted
	//    in the actionable (reachable) bucket.
	// (Covered by assertions 2 and 3 above — if the orphan were counted
	// as reachable, the WARN would fire with count=1.)

	// ---- Steady-state case: second boot ----
	// Every project is now in projects_done and no backfill work is performed.
	// Both counts must still be correct and WARN must not fire.
	buf.Reset()
	runBootDataMigrations(ctx, s)

	logOutput = buf.String()

	// The unreachable count must still be reported correctly.
	assert.Contains(t, logOutput, "Message attribution complete",
		"steady-state: INFO line must appear on second boot")
	assert.Contains(t, logOutput, "unreachable=1",
		"steady-state: unreachable count must still be 1")

	// The WARN must still not fire (reachable is still 0).
	assert.NotContains(t, logOutput, "Messages remain unattributed in listed projects",
		"steady-state: WARN must not fire on second boot")

	// No backfill command advertised.
	assert.NotContains(t, logOutput, "scion server backfill",
		"steady-state: no backfill command must appear")
}

// ---------------------------------------------------------------------------
// Steady-state reachable WARN gate
// ---------------------------------------------------------------------------

// TestResidualReport_SteadyStateReachableWarn gates the specific defect that
// caused M6 to be re-specified: a reachable count that reads 0 on a
// steady-state boot because the count was derived from work the backfill
// performed and a steady-state boot performs none.
//
// This test is distinct from AC-9's steady-state case. AC-9 seeds an
// attributable message: it gets attributed on boot 1, so boot 2's reachable
// count is legitimately 0 and the WARN correctly does not fire. That test
// cannot distinguish "reachable is correctly 0" from "reachable reads 0
// because the counter is broken." This test seeds an UNATTRIBUTABLE reachable
// message (non-UUID principals in a listed project) so the reachable count
// is non-zero on both boots.
//
// Mutation-tested: replacing the anti-join count with a sum-of-per-project-
// counts approach (reachable derived from backfill work performed) makes
// the second-boot assertion fail — the sum yields 0 because no work was
// performed, and the WARN is silently suppressed.
func TestResidualReport_SteadyStateReachableWarn(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed a message that CANNOT be attributed: non-UUID principals in a
	// listed project. The backfill will process it, refuse it as a row-level
	// refusal (DeriveErrPrincipalPair), and leave conversation_id NULL.
	// It is reachable (project exists) but permanently unattributable.
	projectID := uuid.NewString()
	err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "steady-state-warn-project",
		Slug: "ss-warn-" + projectID[:8],
	})
	require.NoError(t, err)

	err = s.CreateMessage(ctx, &store.Message{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		Msg:       "permanently unattributable reachable message",
		Sender:    "user:alice@example.com",   // non-UUID principal
		Recipient: "agent:some-bot",            // non-UUID principal
		// No ThreadID — forces principal-pair derivation, which fails
		// on non-UUID principals.
	})
	require.NoError(t, err)

	// ---- First boot ----
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	origLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(origLogger)

	runBootDataMigrations(ctx, s)

	logOutput := buf.String()

	// Sanity: the WARN must fire on the first boot. The message is
	// unattributable but reachable — reachable count is 1.
	assert.Contains(t, logOutput, "Messages remain unattributed in listed projects",
		"first boot: WARN must fire for reachable unattributable message")

	// Verify the backfill completed and the project is in projects_done.
	marker, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	require.NotNil(t, marker.CompletedAt,
		"first boot: backfill marker must be complete after processing all projects")

	// ---- Second boot (steady state) ----
	// Every project is in projects_done. The backfill's fast path fires:
	// "already complete, skipping." No runBackfillForProject call, no
	// per-project counts. If reachable were derived from work performed,
	// the sum would be 0 and the WARN would be silently suppressed.
	buf.Reset()
	runBootDataMigrations(ctx, s)

	logOutput = buf.String()

	// The backfill must be skipped (steady state).
	assert.Contains(t, logOutput, "already complete, skipping",
		"steady-state: backfill must be skipped on second boot")

	// THE GATE: the WARN must STILL fire on the second boot.
	// This is the assertion that catches the sum-of-per-project-counts
	// approach. On a steady-state boot no work is performed, the naive
	// sum yields 0, and this assertion fails.
	assert.Contains(t, logOutput, "Messages remain unattributed in listed projects",
		"steady-state: WARN must still fire for reachable unattributable messages "+
			"even when no backfill work was performed on this boot")
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
