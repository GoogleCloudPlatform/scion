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
	"encoding/base64"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testEncodeCursor replicates the store's cursor format for test use.
// Format: base64(RFC3339Nano + "," + uuid)
func testEncodeCursor(created time.Time, id string) string {
	raw := created.Format(time.RFC3339Nano) + "," + id
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// seedBackfillProject creates a project and returns its ID.
func seedBackfillProject(t *testing.T, ctx context.Context, s store.Store) string {
	t.Helper()
	projectID := uuid.NewString()
	err := s.CreateProject(ctx, &store.Project{
		ID:         projectID,
		Name:       "backfill-test",
		Slug:       "backfill-test-" + projectID[:8],
		Visibility: "private",
	})
	require.NoError(t, err)
	return projectID
}

// seedDMMessage creates a message with a DM-style ThreadID between two UUIDs.
// The message is created without a ConversationID, simulating pre-conversation data.
func seedDMMessage(t *testing.T, ctx context.Context, s store.Store, projectID string, senderID, recipientID string, createdAt time.Time) string {
	t.Helper()
	msgID := uuid.NewString()
	err := s.CreateMessage(ctx, &store.Message{
		ID:          msgID,
		ProjectID:   projectID,
		Sender:      "user:" + senderID,
		SenderID:    senderID,
		Recipient:   "agent:" + recipientID,
		RecipientID: recipientID,
		Msg:         "test message",
		Type:        "instruction",
		CreatedAt:   createdAt,
	})
	require.NoError(t, err)
	return msgID
}

// TestBackfillDryRunMutatesNothing verifies AC-12-2: dry-run mode reports
// what would change without modifying any database rows.
func TestBackfillDryRunMutatesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	projectID := seedBackfillProject(t, ctx, s)
	senderID := uuid.NewString()
	recipientID := uuid.NewString()

	// Seed messages without conversation_id.
	now := time.Now()
	msgIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		msgIDs[i] = seedDMMessage(t, ctx, s, projectID, senderID, recipientID, now.Add(time.Duration(i)*time.Second))
	}

	// Run backfill in dry-run mode.
	result, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:    true,
		ProjectID: projectID,
	})
	require.NoError(t, err)

	// Report should show what would change.
	assert.Equal(t, 3, result.TotalProcessed, "should process all messages")
	assert.True(t, result.Attributed+result.Inferred > 0, "should report attributions")
	assert.Equal(t, 0, result.Skipped, "no messages should be skipped (none have conversation_id)")

	// Verify messages still have NO conversation_id.
	for _, msgID := range msgIDs {
		msg, err := s.GetMessage(ctx, msgID)
		require.NoError(t, err)
		assert.Empty(t, msg.ConversationID, "dry-run should not set conversation_id on message %s", msgID)
	}
}

// TestBackfillExecuteAndIdempotent verifies AC-12-3: a real run attributes
// messages to conversations, and a second run is idempotent (all skipped).
func TestBackfillExecuteAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	projectID := seedBackfillProject(t, ctx, s)
	senderID := uuid.NewString()
	recipientID := uuid.NewString()

	// Seed messages without conversation_id.
	now := time.Now()
	msgIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		msgIDs[i] = seedDMMessage(t, ctx, s, projectID, senderID, recipientID, now.Add(time.Duration(i)*time.Second))
	}

	// First run: execute mode.
	result1, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:    false,
		ProjectID: projectID,
	})
	require.NoError(t, err)

	assert.Equal(t, 3, result1.TotalProcessed)
	assert.True(t, result1.Attributed+result1.Inferred > 0, "should attribute messages")
	assert.Equal(t, 1, result1.ConversationsCreated, "should create one conversation for the DM pair")

	// Verify messages now have a conversation_id.
	for _, msgID := range msgIDs {
		msg, err := s.GetMessage(ctx, msgID)
		require.NoError(t, err)
		assert.NotEmpty(t, msg.ConversationID, "execute should set conversation_id on message %s", msgID)
	}

	// Second run: should be idempotent (all skipped).
	result2, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:    false,
		ProjectID: projectID,
	})
	require.NoError(t, err)

	assert.Equal(t, 3, result2.TotalProcessed)
	assert.Equal(t, 3, result2.Skipped, "idempotent re-run should skip all messages")
	assert.Equal(t, 0, result2.Attributed, "idempotent re-run should attribute nothing new")
	assert.Equal(t, 0, result2.ConversationsCreated, "idempotent re-run should create no conversations")
}

// TestBackfillResumeViaCheckpoint verifies AC-12-4: the cursor-based checkpoint
// mechanism allows resuming a backfill from where pagination left off.
func TestBackfillResumeViaCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	projectID := seedBackfillProject(t, ctx, s)
	senderID := uuid.NewString()
	recipientID := uuid.NewString()

	// Seed 6 messages with distinct timestamps.
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		seedDMMessage(t, ctx, s, projectID, senderID, recipientID, baseTime.Add(time.Duration(i)*time.Minute))
	}

	// Run with batch size 2 → 3 pages (DESC order: newest first).
	// All messages get processed; LastCheckpoint records the cursor of the
	// second-to-last completed page.
	result1, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:    false,
		ProjectID: projectID,
		BatchSize: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, 6, result1.TotalProcessed)
	assert.NotEmpty(t, result1.LastCheckpoint, "multi-page run should record a checkpoint")

	// All messages should be attributed after the full run.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	for _, msg := range msgs.Items {
		assert.NotEmpty(t, msg.ConversationID,
			"all messages should have conversation_id after full backfill")
	}

	// Resume from checkpoint: the remaining messages (those after the cursor
	// in DESC pagination order) were already attributed, so they are skipped.
	result2, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:     false,
		Checkpoint: result1.LastCheckpoint,
		ProjectID:  projectID,
		BatchSize:  2,
	})
	require.NoError(t, err)
	assert.Equal(t, result2.Skipped, result2.TotalProcessed,
		"resume after completed run should skip all remaining messages (idempotent)")
}

// TestBackfillResumeViaCheckpoint_SameTimestamp is the KEY regression test
// for the cursor-based checkpoint fix. It verifies that messages sharing an
// identical created_at timestamp are NOT silently skipped when resuming from
// a checkpoint cursor.
//
// Unlike the prior version of this test, NO prior backfill run is performed.
// Instead, a cursor is manually constructed pointing at a middle message,
// and the checkpoint's behaviour is the ONLY thing determining whether the
// remaining same-timestamp messages get processed.
//
// On FIXED code (cursor-based): the store paginates past the cursor position
// using (created, id) keyset, so same-timestamp siblings after the cursor
// are returned → TotalProcessed = 2.
//
// On BUGGY code (fda9977f, filter.After = T): the checkpoint is resolved to
// a timestamp and "created > T" filters out ALL same-timestamp messages
// → the test fails (either via error or TotalProcessed = 0).
//
// NOTE: This test guards the STORE-LAYER mutation site (the (created, id)
// tuple comparison in message_store.go:406-411). The companion test
// TestBackfill_SameTimestampMessages in pkg/messaging guards the SERVICE-LAYER
// mutation site (the filter.After form of the bug) against a mock store.
// Neither is redundant — they guard different layers. Do not remove one
// believing the other covers it.
func TestBackfillResumeViaCheckpoint_SameTimestamp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)
	senderID := uuid.NewString()

	// 5 messages at IDENTICAL timestamp, different recipients.
	same := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, seedDMMessage(t, ctx, s, projectID, senderID, uuid.NewString(), same))
	}

	// Sort IDs to get deterministic ordering. ListMessages uses DESC by (created, id).
	// So sorted ascending: ids[0] < ids[1] < ids[2] < ids[3] < ids[4]
	// DESC order: ids[4], ids[3], ids[2], ids[1], ids[0]
	sort.Strings(ids)

	// Construct a cursor pointing at ids[2] — the middle message.
	// This simulates "we already processed ids[4] and ids[3] (first page in DESC),
	// and the cursor is positioned at ids[2]."
	// Messages after the cursor in DESC: ids[1], ids[0] — these should be processed.
	//
	// NO prior backfill run — all messages still have empty conversation_id.
	cursor := testEncodeCursor(same, ids[2])

	result, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:     false,
		ProjectID:  projectID,
		Checkpoint: cursor,
	})
	require.NoError(t, err)

	// EXACT counts — this is the regression guard.
	// Fixed code (cursor-based): processes ids[1] and ids[0] — 2 messages at same timestamp.
	// Buggy code (filter.After = T): "created > T" matches 0 messages. TotalProcessed = 0.
	assert.Equal(t, 2, result.TotalProcessed,
		"cursor-based resume must process same-timestamp messages after cursor position")
	assert.Equal(t, 2, result.Attributed+result.Inferred,
		"should attribute the 2 same-timestamp messages after cursor")
	assert.Equal(t, 0, result.Skipped,
		"no messages should be skipped (none have conversation_id)")
}

// TestBackfillDefaultIsDryRun_FlagWiring exercises the flag-to-config wiring
// in runServerBackfill. It verifies that when backfillExecute is false (the
// default, meaning --execute was NOT passed), the config sent to the backfill
// engine has DryRun=true and messages are NOT stamped.
//
// Mutation guard: if someone changes `!backfillExecute` to `backfillExecute`
// on line ~109 of server_backfill.go, DryRun becomes false and the assertion
// on ConversationID will fail.
func TestBackfillDefaultIsDryRun_FlagWiring(t *testing.T) {
	// Save and restore all global flags that runServerBackfill reads.
	origExecute := backfillExecute
	origDB := backfillDB
	origProject := backfillProject
	origBatch := backfillBatchSize
	origCheckpoint := backfillCheckpoint
	origConfigPath := serverConfigPath
	defer func() {
		backfillExecute = origExecute
		backfillDB = origDB
		backfillProject = origProject
		backfillBatchSize = origBatch
		backfillCheckpoint = origCheckpoint
		serverConfigPath = origConfigPath
	}()

	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Seed data through a direct store connection.
	backfillDB = dbPath
	serverConfigPath = filepath.Join(tmpDir, "nonexistent.yaml")
	seedStore, err := openBackfillStore(ctx)
	require.NoError(t, err)

	projectID := seedBackfillProject(t, ctx, seedStore)
	senderID := uuid.NewString()
	recipientID := uuid.NewString()
	seedDMMessage(t, ctx, seedStore, projectID, senderID, recipientID, time.Now())
	require.NoError(t, seedStore.Close())

	// Set global flags to their defaults (no --execute).
	backfillExecute = false // default: should produce DryRun=true
	backfillDB = dbPath
	backfillProject = projectID
	backfillBatchSize = 0
	backfillCheckpoint = ""

	// Call runServerBackfill — the actual production code path where the
	// flag-to-config wiring lives.
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	err = runServerBackfill(cmd, nil)
	require.NoError(t, err)

	// Re-open and verify messages were NOT stamped.
	verifyStore, err := openBackfillStore(ctx)
	require.NoError(t, err)
	defer func() { _ = verifyStore.Close() }()

	msgs, err := verifyStore.ListMessages(ctx, store.MessageFilter{ProjectID: projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.True(t, len(msgs.Items) > 0, "should have seeded at least one message")
	for _, msg := range msgs.Items {
		assert.Empty(t, msg.ConversationID,
			"default (no --execute) must be dry-run: message %s should not be stamped", msg.ID)
	}
}

// TestBackfillMalformedThreadID verifies AC-12-5: messages with malformed
// ThreadIDs produce errors in the result without crashing.
func TestBackfillMalformedThreadID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	projectID := seedBackfillProject(t, ctx, s)

	// Seed a message with a malformed DM ThreadID.
	msgID := uuid.NewString()
	err := s.CreateMessage(ctx, &store.Message{
		ID:          msgID,
		ProjectID:   projectID,
		Sender:      "user:" + uuid.NewString(),
		SenderID:    uuid.NewString(),
		Recipient:   "agent:" + uuid.NewString(),
		RecipientID: uuid.NewString(),
		Msg:         "test message with bad thread",
		Type:        "instruction",
		ThreadID:    "dm:invalid:bad:format",
		CreatedAt:   time.Now(),
	})
	require.NoError(t, err)

	// Run backfill.
	result, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:    true,
		ProjectID: projectID,
	})
	require.NoError(t, err)

	// The malformed ThreadID should produce an error entry.
	assert.NotEmpty(t, result.Errors, "malformed ThreadID should produce errors")
	assert.Equal(t, 1, result.TotalProcessed, "should still process the message")
}

// TestBackfillMergeResult verifies the result aggregation helper.
func TestBackfillMergeResult(t *testing.T) {
	dst := &messaging.BackfillResult{
		TotalProcessed:       10,
		Attributed:           5,
		Inferred:             2,
		Skipped:              3,
		ConversationsCreated: 1,
		HazardAEmailCount:    1,
		HazardBSlugCount:     0,
		LastCheckpoint:       "old-cp",
		Errors:               []string{"err1"},
	}
	src := &messaging.BackfillResult{
		TotalProcessed:       20,
		Attributed:           15,
		Inferred:             3,
		Skipped:              2,
		ConversationsCreated: 4,
		HazardAEmailCount:    2,
		HazardBSlugCount:     1,
		LastCheckpoint:       "new-cp",
		Errors:               []string{"err2", "err3"},
	}

	mergeBackfillResult(dst, src)

	assert.Equal(t, 30, dst.TotalProcessed)
	assert.Equal(t, 20, dst.Attributed)
	assert.Equal(t, 5, dst.Inferred)
	assert.Equal(t, 5, dst.Skipped)
	assert.Equal(t, 5, dst.ConversationsCreated)
	assert.Equal(t, 3, dst.HazardAEmailCount)
	assert.Equal(t, 1, dst.HazardBSlugCount)
	assert.Equal(t, "new-cp", dst.LastCheckpoint)
	assert.Equal(t, []string{"err1", "err2", "err3"}, dst.Errors)
}
