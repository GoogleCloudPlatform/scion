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

package messaging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/google/uuid"
)

func TestLegacyDirectMessageExternalRef_Deterministic(t *testing.T) {
	// Order should not matter — the ref is sorted.
	refAB := directMessageExternalRef("aaa", "bbb")
	refBA := directMessageExternalRef("bbb", "aaa")
	if refAB != refBA {
		t.Errorf("refs should be identical regardless of order: %q vs %q", refAB, refBA)
	}
	if refAB != "dm:aaa:bbb" {
		t.Errorf("unexpected ref format: %q", refAB)
	}
}

func TestLegacyDirectMessageExternalRef_EmptyID(t *testing.T) {
	ref := directMessageExternalRef("", "xyz")
	if ref != "dm::xyz" {
		t.Errorf("expected dm::xyz, got %q", ref)
	}
}

func TestOldRoutingFromMessage_WithThreadID(t *testing.T) {
	routing := OldRoutingFromMessage("sender1", "recip1", "dm:abc123")
	if routing != "thread:dm:abc123" {
		t.Errorf("expected thread:dm:abc123, got %q", routing)
	}
}

func TestOldRoutingFromMessage_WithoutThreadID(t *testing.T) {
	routing := OldRoutingFromMessage("sender1", "recip1", "")
	// sender-recipient with sorted IDs
	expected := "sender-recipient:recip1:sender1"
	if routing != expected {
		t.Errorf("expected %q, got %q", expected, routing)
	}

	// Order-independent
	routing2 := OldRoutingFromMessage("recip1", "sender1", "")
	if routing != routing2 {
		t.Errorf("routing should be order-independent: %q vs %q", routing, routing2)
	}
}

func TestLogDivergence_Match(t *testing.T) {
	// Reset global counter for this test.
	DivergenceMetrics = &DivergenceCounter{}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	entry := DivergenceEntry{
		MessageID:  "msg-1",
		OldRouting: "thread:dm:abc",
		NewRouting: "conv:uuid-123",
		Match:      true,
		Reason:     "both resolve to same pair",
	}
	LogDivergence(logger, entry)

	if DivergenceMetrics.Matches() != 1 {
		t.Errorf("expected 1 match, got %d", DivergenceMetrics.Matches())
	}
	if DivergenceMetrics.Mismatches() != 0 {
		t.Errorf("expected 0 mismatches, got %d", DivergenceMetrics.Mismatches())
	}
	if DivergenceMetrics.Total() != 1 {
		t.Errorf("expected 1 total, got %d", DivergenceMetrics.Total())
	}

	output := buf.String()
	if !contains(output, "match") {
		t.Errorf("expected 'match' in log output, got: %s", output)
	}
}

func TestLogDivergence_Mismatch(t *testing.T) {
	DivergenceMetrics = &DivergenceCounter{}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	entry := DivergenceEntry{
		MessageID:  "msg-2",
		OldRouting: "thread:dm:abc",
		NewRouting: "conv:uuid-456",
		Match:      false,
		Reason:     "thread_id maps to different conversation",
	}
	LogDivergence(logger, entry)

	if DivergenceMetrics.Mismatches() != 1 {
		t.Errorf("expected 1 mismatch, got %d", DivergenceMetrics.Mismatches())
	}

	output := buf.String()
	if !contains(output, "DIVERGENCE") {
		t.Errorf("expected 'DIVERGENCE' in log output, got: %s", output)
	}
}

func TestDivergenceCounter_Concurrent(t *testing.T) {
	DivergenceMetrics = &DivergenceCounter{}

	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func(match bool) {
			DivergenceMetrics.Inc(match)
			done <- struct{}{}
		}(i%2 == 0)
	}
	for i := 0; i < 100; i++ {
		<-done
	}

	if DivergenceMetrics.Total() != 100 {
		t.Errorf("expected 100 total, got %d", DivergenceMetrics.Total())
	}
	if DivergenceMetrics.Matches() != 50 {
		t.Errorf("expected 50 matches, got %d", DivergenceMetrics.Matches())
	}
	if DivergenceMetrics.Mismatches() != 50 {
		t.Errorf("expected 50 mismatches, got %d", DivergenceMetrics.Mismatches())
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && bytes.Contains([]byte(s), []byte(substr))
}

// ---------------------------------------------------------------------------
// ComputeDivergenceMatch tests
// ---------------------------------------------------------------------------

func TestComputeDivergenceMatch_EmptyConvID(t *testing.T) {
	match, reason := ComputeDivergenceMatch("sender-recipient:a:b", "dm:a:b", "")
	if match {
		t.Error("expected match=false when convID is empty")
	}
	if reason != "no-new-routing" {
		t.Errorf("expected reason 'no-new-routing', got %q", reason)
	}
}

func TestComputeDivergenceMatch_NoOldRouting(t *testing.T) {
	match, reason := ComputeDivergenceMatch("", "dm:a:b", "conv-abc")
	if match {
		t.Error("expected match=false when oldRouting is empty")
	}
	if reason != "unknown/no-old-routing" {
		t.Errorf("expected reason 'unknown/no-old-routing', got %q", reason)
	}
}

func TestComputeDivergenceMatch_DMAgreement(t *testing.T) {
	// Use realistic UUIDs and kind-safe DMConversationKey, matching what
	// production actually stores. The old model routes by raw sorted IDs;
	// the new model stores kind-prefixed canonical keys.
	agentID := uuid.New().String()
	userID := uuid.New().String()

	oldRouting := OldRoutingFromMessage(agentID, userID, "")
	actualExternalRef, err := messages.DMConversationKey("agent", agentID, "user", userID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	match, reason := ComputeDivergenceMatch(oldRouting, actualExternalRef, "conv-abc")
	if !match {
		t.Errorf("expected match=true for DM agreement, got reason=%q (old=%s new=%s)",
			reason, oldRouting, actualExternalRef)
	}
	if match && reason != "dm-routing-agreement" {
		t.Errorf("expected reason 'dm-routing-agreement', got %q", reason)
	}
}

func TestComputeDivergenceMatch_ThreadAgreement(t *testing.T) {
	oldRouting := OldRoutingFromMessage("", "", "thread-ABC")
	actualExternalRef := "thread:proj1:thread-ABC"
	match, reason := ComputeDivergenceMatch(oldRouting, actualExternalRef, "conv-xyz")
	if !match {
		t.Error("expected match=true for thread agreement")
	}
	if reason != "thread-routing-agreement" {
		t.Errorf("expected reason 'thread-routing-agreement', got %q", reason)
	}
}

func TestComputeDivergenceMatch_RoutingTypeMismatch(t *testing.T) {
	// Old says DM, new says thread
	oldRouting := OldRoutingFromMessage("sender", "recip", "")
	actualExternalRef := "thread:proj1:thread-ABC"
	match, reason := ComputeDivergenceMatch(oldRouting, actualExternalRef, "conv-xyz")
	if match {
		t.Error("expected match=false when routing types differ")
	}
	if !strings.Contains(reason, "routing-type-mismatch") {
		t.Errorf("expected reason to contain 'routing-type-mismatch', got %q", reason)
	}

	// Old says thread, new says DM
	senderID := uuid.New().String()
	recipID := uuid.New().String()
	oldRouting = OldRoutingFromMessage("", "", "thread-ABC")
	dmRef, err := messages.DMConversationKey("agent", senderID, "user", recipID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	match, reason = ComputeDivergenceMatch(oldRouting, dmRef, "conv-xyz")
	if match {
		t.Error("expected match=false when routing types differ (thread vs DM)")
	}
	if !strings.Contains(reason, "routing-type-mismatch") {
		t.Errorf("expected reason to contain 'routing-type-mismatch', got %q", reason)
	}
}

func TestComputeDivergenceMatch_ThreadFormatUnexpected(t *testing.T) {
	// Thread without projectID separator
	oldRouting := "thread:thread-ABC"
	actualExternalRef := "thread:noProjectSep"
	match, reason := ComputeDivergenceMatch(oldRouting, actualExternalRef, "conv-xyz")
	if match {
		t.Error("expected match=false for unexpected thread format")
	}
	if !strings.Contains(reason, "thread-format-unexpected") {
		t.Errorf("expected reason to contain 'thread-format-unexpected', got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// MANDATORY: Genuine disagreement tests (architect-required)
// ---------------------------------------------------------------------------

func TestComputeDivergenceMatch_GenuineDisagreement(t *testing.T) {
	// Construct a case where old-model and new-model genuinely disagree:
	// message has sender=A, recipient=B (old routing = sender-recipient:A:B)
	// but the conversation it was stamped with belongs to a different pair (dm:X:Y)
	agentA := uuid.New().String()
	userB := uuid.New().String()
	agentX := uuid.New().String()
	userY := uuid.New().String()

	oldRouting := OldRoutingFromMessage(agentA, userB, "")
	actualExternalRef, err := messages.DMConversationKey("agent", agentX, "user", userY) // DIFFERENT pair
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}

	match, reason := ComputeDivergenceMatch(oldRouting, actualExternalRef, "some-conv-id")
	if match {
		t.Fatal("expected match=false when conversation belongs to a different pair than the message's sender/recipient")
	}
	if !strings.Contains(reason, "mismatch") {
		t.Errorf("expected reason to contain 'mismatch', got %q", reason)
	}

	// Also verify Mismatches counter increments
	DivergenceMetrics = &DivergenceCounter{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	LogDivergence(logger, DivergenceEntry{
		MessageID:  "test-msg",
		OldRouting: oldRouting,
		NewRouting: "conv:some-conv-id",
		Match:      match,
		Reason:     reason,
	})
	if DivergenceMetrics.Mismatches() != 1 {
		t.Fatalf("expected Mismatches()=1, got %d", DivergenceMetrics.Mismatches())
	}
}

func TestComputeDivergenceMatch_ThreadDisagreement(t *testing.T) {
	oldRouting := OldRoutingFromMessage("", "", "thread-ABC")
	actualExternalRef := "thread:proj1:thread-XYZ" // DIFFERENT thread

	match, reason := ComputeDivergenceMatch(oldRouting, actualExternalRef, "some-conv-id")
	if match {
		t.Fatal("expected match=false when thread IDs differ")
	}
	if !strings.Contains(reason, "mismatch") {
		t.Errorf("expected reason to contain 'mismatch', got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// NewRoutingStr tests
// ---------------------------------------------------------------------------

func TestNewRoutingStr(t *testing.T) {
	if got := NewRoutingStr(""); got != "none" {
		t.Errorf("expected 'none' for empty convID, got %q", got)
	}
	if got := NewRoutingStr("conv-abc"); got != "conv:conv-abc" {
		t.Errorf("expected 'conv:conv-abc', got %q", got)
	}
}
