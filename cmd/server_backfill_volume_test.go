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

//go:build volume_test && !no_sqlite

package cmd

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedVolumeMessages creates count messages with a realistic distribution:
//
//	~60% DM-style ThreadIDs (canonical dm:<kind>:<uuid>:<kind>:<uuid>)
//	~15% non-DM ThreadIDs (topic/thread style)
//	~10% malformed DM ThreadIDs (parse failures)
//	~10% empty ThreadID (legacy — derive key from sender/recipient)
//	~5%  pre-backfilled (ConversationID already set)
//
// It uses deterministic seeding for reproducibility and at least 20 distinct
// sender/recipient pairs to create multiple conversation groups.
func seedVolumeMessages(t *testing.T, ctx context.Context, s store.Store, projectID string, count int) (malformedCount, preBackfilledCount int) {
	t.Helper()

	// 20 distinct user→agent pairs.
	type principalPair struct {
		senderID    string // user UUID
		recipientID string // agent UUID
	}

	rng := rand.New(rand.NewSource(42)) // deterministic

	const numPairs = 20
	pairs := make([]principalPair, numPairs)
	for i := range pairs {
		pairs[i] = principalPair{
			senderID:    uuid.NewString(),
			recipientID: uuid.NewString(),
		}
	}

	// Pre-compute canonical DM keys for each pair.
	dmKeys := make([]string, numPairs)
	for i, p := range pairs {
		key, err := messages.DMConversationKey("user", p.senderID, "agent", p.recipientID)
		require.NoError(t, err)
		dmKeys[i] = key
	}

	malformedThreadIDs := []string{
		"dm:invalid:bad:format",
		"dm:not-a-uuid:also-not",
		"dm:user:notauuid:agent:alsonotauuid",
		"dm:foo:bar:baz:qux",
		"dm:user:123:agent:456",
	}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Distribute categories uniformly via modular arithmetic so any
	// page of results contains a representative mix of all types.
	for i := 0; i < count; i++ {
		pairIdx := rng.Intn(numPairs)
		p := pairs[pairIdx]

		msg := &store.Message{
			ID:          uuid.NewString(),
			ProjectID:   projectID,
			Sender:      "user:" + p.senderID,
			SenderID:    p.senderID,
			Recipient:   "agent:" + p.recipientID,
			RecipientID: p.recipientID,
			Msg:         "volume test message",
			Type:        "instruction",
			CreatedAt:   baseTime.Add(time.Duration(i) * time.Millisecond),
		}

		category := i % 100
		switch {
		case category < 60:
			// 60% — DM-style ThreadID in canonical form.
			msg.ThreadID = dmKeys[pairIdx]

		case category < 75:
			// 15% — non-DM ThreadID (topic/thread style).
			msg.ThreadID = fmt.Sprintf("topic-%d", rng.Intn(50))

		case category < 85:
			// 10% — malformed DM ThreadID → key-derivation errors.
			msg.ThreadID = malformedThreadIDs[rng.Intn(len(malformedThreadIDs))]
			malformedCount++

		case category < 95:
			// 10% — empty ThreadID (legacy), key derived from sender/recipient.
			msg.ThreadID = ""

		default:
			// 5% — pre-backfilled, ConversationID already set.
			msg.ConversationID = uuid.NewString()
			preBackfilledCount++
		}

		err := s.CreateMessage(ctx, msg)
		require.NoError(t, err, "creating message %d", i)
	}

	return malformedCount, preBackfilledCount
}

// TestBackfillVolume exercises the backfill pipeline with 50 000 messages
// across the full spectrum of ThreadID shapes. It validates dry-run fidelity,
// execute correctness, and idempotency in three sequential phases.
//
// Run explicitly:
//
//	go test ./cmd/... -run TestBackfillVolume -tags volume_test -v -count=1 -timeout 300s
func TestBackfillVolume(t *testing.T) {
	const totalMessages = 50_000

	ctx := context.Background()
	s := newTestStore(t)

	projectID := seedBackfillProject(t, ctx, s)

	// ── Seed ───────────────────────────────────────────────────────────────
	t.Log("Seeding messages...")
	seedStart := time.Now()
	malformedCount, preBackfilledCount := seedVolumeMessages(t, ctx, s, projectID, totalMessages)
	t.Logf("Seed:                 %v, %d messages (%d malformed, %d pre-backfilled)",
		time.Since(seedStart), totalMessages, malformedCount, preBackfilledCount)

	// Sanity-check seed counts.
	require.Greater(t, malformedCount, 0, "seed should produce malformed messages")
	require.Greater(t, preBackfilledCount, 0, "seed should produce pre-backfilled messages")

	// Use a larger batch size to reduce round-trip overhead with 50k rows.
	const batchSize = 1000

	// ── Phase 1: Dry-run ───────────────────────────────────────────────────
	t.Log("Phase 1: dry-run...")
	phase1Start := time.Now()
	result1, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:    true,
		ProjectID: projectID,
		BatchSize: batchSize,
	})
	phase1Elapsed := time.Since(phase1Start)
	require.NoError(t, err)

	t.Logf("Phase 1 (dry-run):    %v, processed %d messages", phase1Elapsed, result1.TotalProcessed)

	assert.Equal(t, totalMessages, result1.TotalProcessed, "dry-run should process all messages")
	// Invariant: every message is either attributed, inferred, skipped, or errored.
	assert.Equal(t, result1.TotalProcessed,
		result1.Attributed+result1.Inferred+result1.Skipped+len(result1.Errors),
		"attributed+inferred+skipped+errors should equal total processed")
	assert.Equal(t, malformedCount, len(result1.Errors),
		"error count should match malformed message count")
	assert.Greater(t, result1.ConversationsCreated, 0,
		"dry-run should report conversations that would be created")

	// Dry-run must not stamp any messages. Sample 100 random un-backfilled
	// messages and verify they still have no ConversationID.
	verifyNoStamping(t, ctx, s, projectID, 100)

	// ── Phase 2: Execute ───────────────────────────────────────────────────
	t.Log("Phase 2: execute...")
	phase2Start := time.Now()
	result2, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:    false,
		ProjectID: projectID,
		BatchSize: batchSize,
	})
	phase2Elapsed := time.Since(phase2Start)
	require.NoError(t, err)

	t.Logf("Phase 2 (execute):    %v, processed %d messages, %d conversations",
		phase2Elapsed, result2.TotalProcessed, result2.ConversationsCreated)

	assert.Equal(t, totalMessages, result2.TotalProcessed, "execute should process all messages")
	assert.Greater(t, result2.Attributed+result2.Inferred, 0, "execute should attribute messages")
	assert.Equal(t, preBackfilledCount, result2.Skipped,
		"execute should skip exactly the pre-backfilled messages")
	assert.Equal(t, malformedCount, len(result2.Errors),
		"error count should match malformed messages")
	assert.Greater(t, result2.ConversationsCreated, 0,
		"execute should create conversations")
	// Invariant holds for execute too.
	assert.Equal(t, result2.TotalProcessed,
		result2.Attributed+result2.Inferred+result2.Skipped+len(result2.Errors),
		"attributed+inferred+skipped+errors should equal total processed")

	// Verify a sample of messages now have ConversationID.
	verifyStamped(t, ctx, s, projectID, 100)

	// ── Phase 3: Re-execute (idempotency) ──────────────────────────────────
	t.Log("Phase 3: re-execute (idempotency)...")
	phase3Start := time.Now()
	result3, err := runBackfillWithStore(ctx, s, messaging.BackfillConfig{
		DryRun:    false,
		ProjectID: projectID,
		BatchSize: batchSize,
	})
	phase3Elapsed := time.Since(phase3Start)
	require.NoError(t, err)

	t.Logf("Phase 3 (re-execute): %v, processed %d messages, %d skipped",
		phase3Elapsed, result3.TotalProcessed, result3.Skipped)

	assert.Equal(t, totalMessages, result3.TotalProcessed, "re-execute should process all messages")
	// Everything except malformed should be skipped (they already have ConversationID).
	expectedSkipped := totalMessages - malformedCount
	assert.Equal(t, expectedSkipped, result3.Skipped,
		"re-execute should skip all non-malformed messages")
	assert.Equal(t, malformedCount, len(result3.Errors),
		"re-execute errors should match malformed messages")
	assert.Equal(t, 0, result3.Attributed,
		"re-execute should attribute nothing new")
	assert.Equal(t, 0, result3.Inferred,
		"re-execute should infer nothing new")
	assert.Equal(t, 0, result3.ConversationsCreated,
		"re-execute should create no new conversations")

	// ── Summary ────────────────────────────────────────────────────────────
	t.Log("─── Volume test summary ───")
	t.Logf("  Seed:       %d messages (%d malformed, %d pre-backfilled)", totalMessages, malformedCount, preBackfilledCount)
	t.Logf("  Phase 1:    %v  (dry-run, %d conversations would be created)", phase1Elapsed, result1.ConversationsCreated)
	t.Logf("  Phase 2:    %v  (execute, %d conversations created, %d attributed)", phase2Elapsed, result2.ConversationsCreated, result2.Attributed+result2.Inferred)
	t.Logf("  Phase 3:    %v  (re-execute, %d skipped, %d errors)", phase3Elapsed, result3.Skipped, len(result3.Errors))
	t.Logf("  Total wall: %v", phase1Elapsed+phase2Elapsed+phase3Elapsed)
}

// verifyNoStamping samples messages that originally had no ConversationID and
// confirms they still lack one (proving dry-run did not mutate the database).
func verifyNoStamping(t *testing.T, ctx context.Context, s store.Store, projectID string, sampleSize int) {
	t.Helper()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: projectID}, store.ListOptions{Limit: sampleSize})
	require.NoError(t, err)

	unstamped := 0
	for _, msg := range msgs.Items {
		if msg.ConversationID == "" {
			unstamped++
		}
	}
	// After dry-run, non-pre-backfilled messages should still have empty ConversationID.
	assert.Greater(t, unstamped, 0, "dry-run: at least some sampled messages should have no ConversationID")
}

// verifyStamped samples messages and confirms that a meaningful fraction now
// have ConversationID set (proving execute actually persisted changes).
func verifyStamped(t *testing.T, ctx context.Context, s store.Store, projectID string, sampleSize int) {
	t.Helper()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: projectID}, store.ListOptions{Limit: sampleSize})
	require.NoError(t, err)

	stamped := 0
	for _, msg := range msgs.Items {
		if msg.ConversationID != "" {
			stamped++
		}
	}
	assert.Greater(t, stamped, 0, "execute: at least some sampled messages should have ConversationID")
}
