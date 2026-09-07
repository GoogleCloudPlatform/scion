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
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAttributedMessage creates a message WITH a conversation_id already set,
// simulating a post-dual-write message.
func seedAttributedMessage(t *testing.T, ctx context.Context, s store.Store, projectID string) string {
	t.Helper()
	senderID := uuid.NewString()
	recipientID := uuid.NewString()
	msgID := uuid.NewString()
	convID := uuid.NewString()
	err := s.CreateMessage(ctx, &store.Message{
		ID:             msgID,
		ProjectID:      projectID,
		Sender:         "user:" + senderID,
		SenderID:       senderID,
		Recipient:      "agent:" + recipientID,
		RecipientID:    recipientID,
		Msg:            "attributed message",
		Type:           "instruction",
		ConversationID: convID,
		CreatedAt:      time.Now(),
	})
	require.NoError(t, err)
	return msgID
}

// seedFederatedMessage creates a message whose sender_id is a federated
// identity string (not a UUID), simulating a federated OIDC principal.
func seedFederatedMessage(t *testing.T, ctx context.Context, s store.Store, projectID string) string {
	t.Helper()
	recipientID := uuid.NewString()
	msgID := uuid.NewString()
	federatedID := "https://accounts.google.com:subject123"
	err := s.CreateMessage(ctx, &store.Message{
		ID:          msgID,
		ProjectID:   projectID,
		Sender:      "user:" + federatedID,
		SenderID:    federatedID,
		Recipient:   "agent:" + recipientID,
		RecipientID: recipientID,
		Msg:         "federated message",
		Type:        "instruction",
		CreatedAt:   time.Now(),
	})
	require.NoError(t, err)
	return msgID
}

// seedSlugMessage creates a message whose recipient_id is a non-UUID slug.
func seedSlugMessage(t *testing.T, ctx context.Context, s store.Store, projectID string) string {
	t.Helper()
	senderID := uuid.NewString()
	msgID := uuid.NewString()
	err := s.CreateMessage(ctx, &store.Message{
		ID:          msgID,
		ProjectID:   projectID,
		Sender:      "user:" + senderID,
		SenderID:    senderID,
		Recipient:   "agent:my-agent-slug",
		RecipientID: "my-agent-slug",
		Msg:         "slug message",
		Type:        "instruction",
		CreatedAt:   time.Now(),
	})
	require.NoError(t, err)
	return msgID
}

// seedBroadcastMessage creates a broadcasted message without a conversation_id.
func seedBroadcastMessage(t *testing.T, ctx context.Context, s store.Store, projectID string) string {
	t.Helper()
	senderID := uuid.NewString()
	recipientID := uuid.NewString()
	msgID := uuid.NewString()
	err := s.CreateMessage(ctx, &store.Message{
		ID:          msgID,
		ProjectID:   projectID,
		Sender:      "user:" + senderID,
		SenderID:    senderID,
		Recipient:   "agent:" + recipientID,
		RecipientID: recipientID,
		Msg:         "broadcast message",
		Type:        "instruction",
		Broadcasted: true,
		CreatedAt:   time.Now(),
	})
	require.NoError(t, err)
	return msgID
}

// --------------------------------------------------------------------------
// AC-G-1: mutation guard — the report must be read-only
// --------------------------------------------------------------------------

// TestAttributionReport_MutationGuard verifies that running the attribution
// report does not modify any database rows. It seeds a known set of messages,
// captures their state, runs the report, and asserts the state is identical.
func TestAttributionReport_MutationGuard(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	senderID := uuid.NewString()
	recipientID := uuid.NewString()
	now := time.Now()

	// Seed a mix of message types.
	var allMsgIDs []string

	// 1. Unattributed with UUID principals (backfillable).
	for i := 0; i < 3; i++ {
		id := seedDMMessage(t, ctx, s, projectID, senderID, recipientID, now.Add(time.Duration(i)*time.Second))
		allMsgIDs = append(allMsgIDs, id)
	}

	// 2. Attributed message.
	id := seedAttributedMessage(t, ctx, s, projectID)
	allMsgIDs = append(allMsgIDs, id)

	// 3. Federated (non-UUID) message.
	id = seedFederatedMessage(t, ctx, s, projectID)
	allMsgIDs = append(allMsgIDs, id)

	// Snapshot: capture the state of every message before the report.
	preMessages, err := s.GetMessagesByIDs(ctx, allMsgIDs)
	require.NoError(t, err)
	require.Len(t, preMessages, len(allMsgIDs))

	// Run the report.
	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)
	require.NotNil(t, report)

	// Post-check: every message must be identical to its pre-run state.
	postMessages, err := s.GetMessagesByIDs(ctx, allMsgIDs)
	require.NoError(t, err)
	require.Len(t, postMessages, len(allMsgIDs))

	for _, msgID := range allMsgIDs {
		pre := preMessages[msgID]
		post := postMessages[msgID]
		require.NotNil(t, pre, "pre-message %s not found", msgID)
		require.NotNil(t, post, "post-message %s not found", msgID)

		// Compare the fields that matter for mutation detection.
		assert.Equal(t, pre.ConversationID, post.ConversationID, "conversation_id mutated on message %s", msgID)
		assert.Equal(t, pre.SenderID, post.SenderID, "sender_id mutated on message %s", msgID)
		assert.Equal(t, pre.RecipientID, post.RecipientID, "recipient_id mutated on message %s", msgID)
		assert.Equal(t, pre.ThreadID, post.ThreadID, "thread_id mutated on message %s", msgID)
		assert.Equal(t, pre.Read, post.Read, "read flag mutated on message %s", msgID)
		assert.Equal(t, pre.Msg, post.Msg, "message content mutated on message %s", msgID)
	}

	// Also verify the unbackfilled count didn't change.
	preCount, err := s.CountUnbackfilledMessages(ctx, projectID)
	require.NoError(t, err)
	// We seeded 3 backfillable + 1 federated = 4 unattributed.
	assert.Equal(t, 4, preCount, "unbackfilled count should be unchanged")
}

// --------------------------------------------------------------------------
// AC-G-2: non-UUID principal is distinct from unresolvable, and flip-blocking
// --------------------------------------------------------------------------

// TestAttributionReport_BucketClassification verifies that the four
// unattributed buckets are correctly populated and distinct.
func TestAttributionReport_BucketClassification(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	senderID := uuid.NewString()
	recipientID := uuid.NewString()
	now := time.Now()

	// 1. Attributed message.
	seedAttributedMessage(t, ctx, s, projectID)

	// 2. Backfillable: unattributed, both principals are UUIDs.
	seedDMMessage(t, ctx, s, projectID, senderID, recipientID, now)
	seedDMMessage(t, ctx, s, projectID, senderID, recipientID, now.Add(time.Second))

	// 3. Non-UUID principal: federated identity.
	seedFederatedMessage(t, ctx, s, projectID)

	// 4. Non-UUID principal: slug.
	seedSlugMessage(t, ctx, s, projectID)

	// 5. Broadcast: unattributed, Broadcasted=true.
	seedBroadcastMessage(t, ctx, s, projectID)

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)

	assert.Equal(t, 6, report.Total, "total messages")
	assert.Equal(t, 1, report.Attributed, "attributed messages")
	assert.Equal(t, 2, report.Backfillable, "backfillable messages")
	assert.Equal(t, 1, report.BroadcastNotBackfillable, "broadcast messages")
	assert.Equal(t, 2, report.NonUUIDPrincipal, "non-UUID principal messages")
	assert.Equal(t, 0, report.Unresolvable, "unresolvable messages")

	// Verify the buckets are distinct and sum correctly.
	unattributed := report.Backfillable + report.BroadcastNotBackfillable +
		report.NonUUIDPrincipal + report.Unresolvable
	assert.Equal(t, report.Total-report.Attributed, unattributed,
		"unattributed buckets must sum to total minus attributed")
}

// TestAttributionReport_FlipBlockingOutput verifies that non-zero non-UUID
// principal or unresolvable counts produce flip-blocking output and enumerate
// the offending principal IDs.
func TestAttributionReport_FlipBlockingOutput(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	// Seed a federated message to trigger non-UUID principal count.
	seedFederatedMessage(t, ctx, s, projectID)

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)
	require.Equal(t, 1, report.NonUUIDPrincipal)

	// Verify examples capture the offending principal IDs.
	require.Len(t, report.NonUUIDExamples, 1)
	assert.Equal(t, "https://accounts.google.com:subject123", report.NonUUIDExamples[0].SenderID,
		"non-UUID example must capture the federated principal ID")

	// Render the output and check for flip-blocking language.
	var buf bytes.Buffer
	printAttributionReport(&buf, report, projectID)
	output := buf.String()

	assert.Contains(t, output, "BLOCKS FLIP", "output must declare flip-blocking status")
	assert.Contains(t, output, "FLIP BLOCKED", "output must contain flip blocked warning")
	assert.Contains(t, output, "non-UUID principal", "output must name the blocking bucket")
	assert.Contains(t, output, "MUST NOT be enabled", "output must state the switch must not be enabled")

	// The offending principal IDs must appear in the output.
	assert.Contains(t, output, "https://accounts.google.com:subject123",
		"output must print the offending non-UUID principal ID")
	assert.Contains(t, output, "Non-UUID principal IDs",
		"output must have a section header for non-UUID principal IDs")
}

// TestAttributionReport_NoFlipBlockWhenClean verifies that when all messages
// are either attributed or backfillable, no flip-blocking output is produced.
func TestAttributionReport_NoFlipBlockWhenClean(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	senderID := uuid.NewString()
	recipientID := uuid.NewString()

	seedAttributedMessage(t, ctx, s, projectID)
	seedDMMessage(t, ctx, s, projectID, senderID, recipientID, time.Now())

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)

	var buf bytes.Buffer
	printAttributionReport(&buf, report, projectID)
	output := buf.String()

	assert.NotContains(t, output, "BLOCKS FLIP", "clean report must not mention flip blocking")
	assert.NotContains(t, output, "FLIP BLOCKED", "clean report must not mention flip blocked")
}

// TestAttributionReport_EmptyDatabase verifies the report handles an empty
// database gracefully.
func TestAttributionReport_EmptyDatabase(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)

	assert.Equal(t, 0, report.Total)
	assert.Equal(t, 0, report.Attributed)
	assert.Equal(t, 0, report.Backfillable)
	assert.Equal(t, 0, report.NonUUIDPrincipal)
	assert.Equal(t, 0, report.Unresolvable)
}

// --------------------------------------------------------------------------
// AC-G-3: no DivergenceMetrics dependency — enforced by test
// --------------------------------------------------------------------------

// TestAttributionReport_NoDivergenceMetricsDependency statically verifies
// that the attribution report source file does not import the
// messaging.DivergenceMetrics symbol or the divergence.go file's types.
// This is enforced by parsing the Go source, not by review.
func TestAttributionReport_NoDivergenceMetricsDependency(t *testing.T) {
	// Parse the attribution report source file.
	fset := token.NewFileSet()
	srcPath := filepath.Join(".", "server_attribution_report.go")

	// Read the source to also check for string references.
	src, err := os.ReadFile(srcPath)
	require.NoError(t, err, "failed to read attribution report source")

	f, err := parser.ParseFile(fset, srcPath, src, parser.ImportsOnly)
	require.NoError(t, err, "failed to parse attribution report source")

	// Check that no import path contains "divergence".
	for _, imp := range f.Imports {
		importPath := imp.Path.Value
		if strings.Contains(importPath, "divergence") {
			t.Fatalf("attribution report must not import a divergence package, found import: %s", importPath)
		}
	}

	// Check that the source text does not reference DivergenceMetrics.
	srcStr := string(src)
	if strings.Contains(srcStr, "DivergenceMetrics") {
		t.Fatal("attribution report source must not reference DivergenceMetrics")
	}
	if strings.Contains(srcStr, "DivergenceCounter") {
		t.Fatal("attribution report source must not reference DivergenceCounter")
	}
}

// --------------------------------------------------------------------------
// Unresolvable bucket test
// --------------------------------------------------------------------------

// TestAttributionReport_UnresolvableMessages verifies that messages with valid
// UUID principals but failing key derivation land in the unresolvable bucket.
func TestAttributionReport_UnresolvableMessages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	senderID := uuid.NewString()
	recipientID := uuid.NewString()
	msgID := uuid.NewString()

	// Create a message with a malformed dm: thread_id that has valid UUIDs
	// as sender/recipient but will fail DeriveConversationKey because the
	// dm: prefix triggers parse validation which will fail on the malformed key.
	err := s.CreateMessage(ctx, &store.Message{
		ID:          msgID,
		ProjectID:   projectID,
		Sender:      "user:" + senderID,
		SenderID:    senderID,
		Recipient:   "agent:" + recipientID,
		RecipientID: recipientID,
		Msg:         "unresolvable message",
		Type:        "instruction",
		ThreadID:    "dm:broken:key", // malformed dm: key
		CreatedAt:   time.Now(),
	})
	require.NoError(t, err)

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Total, "total")
	assert.Equal(t, 0, report.Attributed, "attributed")
	assert.Equal(t, 0, report.Backfillable, "backfillable")
	assert.Equal(t, 0, report.NonUUIDPrincipal, "non-UUID principal")
	assert.Equal(t, 1, report.Unresolvable, "unresolvable")

	// Verify examples contain IDs and error but no message content.
	require.Len(t, report.UnresolvableExamples, 1)
	assert.Equal(t, msgID, report.UnresolvableExamples[0].MessageID)
	assert.Equal(t, senderID, report.UnresolvableExamples[0].SenderID)
	assert.Equal(t, recipientID, report.UnresolvableExamples[0].RecipientID)
	assert.NotEmpty(t, report.UnresolvableExamples[0].DeriveError)
	// Content must not leak.
	assert.NotContains(t, report.UnresolvableExamples[0].DeriveError, "unresolvable message")
}

// --------------------------------------------------------------------------
// Production key derivation reuse test
// --------------------------------------------------------------------------

// TestAttributionReport_UsesProductionDerivation verifies that the report
// uses the same key derivation as production (messaging.DeriveConversationKey),
// not a reimplementation.
func TestAttributionReport_UsesProductionDerivation(t *testing.T) {
	// This is a structural test: parse the source and verify it calls
	// messaging.DeriveConversationKey.
	src, err := os.ReadFile(filepath.Join(".", "server_attribution_report.go"))
	require.NoError(t, err)

	srcStr := string(src)
	assert.Contains(t, srcStr, "messaging.DeriveConversationKey",
		"attribution report must use the production DeriveConversationKey function")
}

// --------------------------------------------------------------------------
// Broadcast bucket tests
// --------------------------------------------------------------------------

// TestAttributionReport_BroadcastFlipBlocking verifies that broadcasts with
// no conversation_id are reported as flip-blocking.
func TestAttributionReport_BroadcastFlipBlocking(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	seedBroadcastMessage(t, ctx, s, projectID)

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Total)
	assert.Equal(t, 0, report.Attributed)
	assert.Equal(t, 0, report.Backfillable)
	assert.Equal(t, 1, report.BroadcastNotBackfillable)
	assert.Equal(t, 0, report.NonUUIDPrincipal)
	assert.Equal(t, 0, report.Unresolvable)

	var buf bytes.Buffer
	printAttributionReport(&buf, report, projectID)
	output := buf.String()

	assert.Contains(t, output, "BLOCKS FLIP", "broadcast must be flip-blocking")
	assert.Contains(t, output, "FLIP BLOCKED", "broadcast must trigger flip blocked warning")
	assert.Contains(t, output, "broadcast", "output must name the broadcast bucket")
	assert.Contains(t, output, "backfill skips broadcasts", "output must explain why broadcasts block")
}

// TestAttributionReport_BroadcastNotInBackfillable verifies that a broadcast
// message does not land in the backfillable bucket, even when its principals
// are valid UUIDs.
func TestAttributionReport_BroadcastNotInBackfillable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	senderID := uuid.NewString()
	recipientID := uuid.NewString()

	// Seed one backfillable and one broadcast (same UUIDs, only
	// Broadcasted flag differs).
	seedDMMessage(t, ctx, s, projectID, senderID, recipientID, time.Now())
	seedBroadcastMessage(t, ctx, s, projectID)

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Backfillable, "non-broadcast should be backfillable")
	assert.Equal(t, 1, report.BroadcastNotBackfillable, "broadcast should be in its own bucket")
	assert.Equal(t, 0, report.Unresolvable, "neither should be unresolvable")
}

// --------------------------------------------------------------------------
// Reconciliation test
// --------------------------------------------------------------------------

// TestAttributionReport_ReconciliationMatch verifies that when all projects
// are scanned, the report's unattributed total matches the global
// CountUnbackfilledMessages.
func TestAttributionReport_ReconciliationMatch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	senderID := uuid.NewString()
	recipientID := uuid.NewString()

	seedAttributedMessage(t, ctx, s, projectID)
	seedDMMessage(t, ctx, s, projectID, senderID, recipientID, time.Now())
	seedFederatedMessage(t, ctx, s, projectID)

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)

	reportUnattributed := report.Backfillable + report.BroadcastNotBackfillable +
		report.NonUUIDPrincipal + report.Unresolvable

	globalCount, err := s.CountUnbackfilledMessages(ctx, "")
	require.NoError(t, err)

	assert.Equal(t, globalCount, reportUnattributed,
		"report unattributed total must match global CountUnbackfilledMessages")
}

// --------------------------------------------------------------------------
// Multi-project aggregation test
// --------------------------------------------------------------------------

// TestAttributionReport_MultiProject verifies that results are correctly
// aggregated across multiple projects.
func TestAttributionReport_MultiProject(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	projectA := seedBackfillProject(t, ctx, s)
	projectB := seedBackfillProject(t, ctx, s)

	senderID := uuid.NewString()
	recipientID := uuid.NewString()
	now := time.Now()

	// Project A: 1 attributed, 1 backfillable.
	seedAttributedMessage(t, ctx, s, projectA)
	seedDMMessage(t, ctx, s, projectA, senderID, recipientID, now)

	// Project B: 1 federated, 1 broadcast.
	seedFederatedMessage(t, ctx, s, projectB)
	seedBroadcastMessage(t, ctx, s, projectB)

	// Run for each project separately.
	reportA, err := runAttributionReportForProject(ctx, s, projectA)
	require.NoError(t, err)
	reportB, err := runAttributionReportForProject(ctx, s, projectB)
	require.NoError(t, err)

	// Merge.
	total := &AttributionReport{}
	mergeAttributionReport(total, reportA)
	mergeAttributionReport(total, reportB)

	assert.Equal(t, 4, total.Total)
	assert.Equal(t, 1, total.Attributed)
	assert.Equal(t, 1, total.Backfillable)
	assert.Equal(t, 1, total.BroadcastNotBackfillable)
	assert.Equal(t, 1, total.NonUUIDPrincipal)
	assert.Equal(t, 0, total.Unresolvable)
}

// --------------------------------------------------------------------------
// G1-c: Behavioral production derivation test
// --------------------------------------------------------------------------

// TestAttributionReport_DerivationBehavioral verifies that the report's
// classification tracks the success and failure of the production
// messaging.DeriveConversationKey function. This is a behavioural guard:
// inputs known to fail derivation must land in unresolvable, and inputs
// known to succeed must land in backfillable.
func TestAttributionReport_DerivationBehavioral(t *testing.T) {
	// First, confirm our test inputs against production DeriveConversationKey
	// to establish the ground truth.
	validSender := uuid.NewString()
	validRecipient := uuid.NewString()

	// Table of inputs and expected derivation outcomes.
	cases := []struct {
		name        string
		threadID    string
		senderKind  string
		senderID    string
		recipKind   string
		recipID     string
		wantSuccess bool
	}{
		{
			name:        "valid DM — no thread",
			senderKind:  "user",
			senderID:    validSender,
			recipKind:   "agent",
			recipID:     validRecipient,
			wantSuccess: true,
		},
		{
			name:        "malformed dm: prefix",
			threadID:    "dm:broken:key",
			senderKind:  "user",
			senderID:    validSender,
			recipKind:   "agent",
			recipID:     validRecipient,
			wantSuccess: false,
		},
		{
			name:        "dm: key with non-canonical UUID",
			threadID:    "dm:user:" + strings.ToUpper(validSender) + ":agent:" + validRecipient,
			senderKind:  "user",
			senderID:    validSender,
			recipKind:   "agent",
			recipID:     validRecipient,
			wantSuccess: false,
		},
		{
			name:        "unknown kind in dm: key",
			threadID:    "dm:bot:" + validSender + ":user:" + validRecipient,
			senderKind:  "user",
			senderID:    validSender,
			recipKind:   "agent",
			recipID:     validRecipient,
			wantSuccess: false,
		},
	}

	// Verify our ground truth: confirm each case behaves as expected
	// against the production DeriveConversationKey.
	for _, tc := range cases {
		_, _, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
			ThreadID:      tc.threadID,
			ProjectID:     uuid.NewString(),
			SenderKind:    tc.senderKind,
			SenderID:      tc.senderID,
			RecipientKind: tc.recipKind,
			RecipientID:   tc.recipID,
		})
		if tc.wantSuccess {
			require.NoError(t, err, "ground truth: %s should succeed", tc.name)
		} else {
			require.Error(t, err, "ground truth: %s should fail", tc.name)
		}
	}

	// Now run each case through the report's classifier and verify the
	// bucket assignment matches.
	ctx := context.Background()
	s := newTestStore(t)
	projectID := seedBackfillProject(t, ctx, s)

	for _, tc := range cases {
		msgID := uuid.NewString()
		err := s.CreateMessage(ctx, &store.Message{
			ID:          msgID,
			ProjectID:   projectID,
			Sender:      tc.senderKind + ":" + tc.senderID,
			SenderID:    tc.senderID,
			Recipient:   tc.recipKind + ":" + tc.recipID,
			RecipientID: tc.recipID,
			Msg:         "test",
			Type:        "instruction",
			ThreadID:    tc.threadID,
			CreatedAt:   time.Now().Add(time.Duration(len(tc.name)) * time.Millisecond),
		})
		require.NoError(t, err, "seeding message for %s", tc.name)
	}

	report, err := runAttributionReportForProject(ctx, s, projectID)
	require.NoError(t, err)

	// 1 success case → backfillable, 3 failure cases → unresolvable.
	assert.Equal(t, 4, report.Total, "total")
	assert.Equal(t, 1, report.Backfillable,
		"exactly the case where DeriveConversationKey succeeds must be backfillable")
	assert.Equal(t, 3, report.Unresolvable,
		"all cases where DeriveConversationKey fails must be unresolvable")
	assert.Equal(t, 0, report.NonUUIDPrincipal,
		"all principals are valid UUIDs")
	assert.Equal(t, 0, report.BroadcastNotBackfillable,
		"no broadcasts in this test")
}
