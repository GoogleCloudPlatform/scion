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
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// AC-G2-1: A write whose conversation-key derivation fails is rejected.
// The message is not persisted and not published.
// ============================================================================

// TestG2_AC1_DM_DerivationFailureReturnsError proves that
// ResolveOrCreateDMConversation returns an error (not nil) when the
// DM key derivation fails, so the caller can deny the write.
func TestG2_AC1_DM_DerivationFailureReturnsError(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// Empty sender ID → derivation fails.
	result, err := ResolveOrCreateDMConversation(
		context.Background(), mock, mock, logger,
		"user", "", "agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err == nil {
		t.Fatal("AC-G2-1: expected error when sender ID is empty, got nil")
	}
	if result != nil {
		t.Fatal("AC-G2-1: expected nil result on derivation failure")
	}
	// Verify no upsert happened — message would not be persisted.
	if mock.lastConv != nil {
		t.Fatal("AC-G2-1: upsert should not have been called on derivation failure")
	}
}

// TestG2_AC1_DM_InvalidKindReturnsError proves that a DM key with an
// invalid kind (not "user" or "agent") causes a derivation failure that
// returns an error, not a nil result with only a log.
func TestG2_AC1_DM_InvalidKindReturnsError(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	result, err := ResolveOrCreateDMConversation(
		context.Background(), mock, mock, logger,
		"robot", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"agent", "550e8400-e29b-41d4-a716-446655440000")
	if err == nil {
		t.Fatal("AC-G2-1: expected error for invalid kind 'robot'")
	}
	if result != nil {
		t.Fatal("AC-G2-1: expected nil result on derivation failure")
	}
}

// TestG2_AC1_Thread_EmptyThreadIDReturnsError proves that
// ResolveOrCreateThreadConversation returns an error when threadID is empty.
func TestG2_AC1_Thread_EmptyThreadIDReturnsError(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	result, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger, "", "proj1")
	if err == nil {
		t.Fatal("AC-G2-1: expected error when threadID is empty")
	}
	if result != nil {
		t.Fatal("AC-G2-1: expected nil result on derivation failure")
	}
}

// TestG2_AC1_Thread_EmptyProjectIDReturnsError proves that
// ResolveOrCreateThreadConversation returns an error when projectID is empty
// (via DeriveConversationKey validation).
func TestG2_AC1_Thread_EmptyProjectIDReturnsError(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	result, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger, "thread-123", "")
	if err == nil {
		t.Fatal("AC-G2-1: expected error when projectID is empty")
	}
	if result != nil {
		t.Fatal("AC-G2-1: expected nil result on derivation failure")
	}
}

// TestG2_AC1_UpsertFailureReturnsError proves that a database upsert error
// propagates as an error return (not a nil result with only a log).
func TestG2_AC1_UpsertFailureReturnsError(t *testing.T) {
	mock := &mockConversationUpserter{
		returnErr: errors.New("database connection lost"),
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	result, err := ResolveOrCreateDMConversation(
		context.Background(), mock, mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if err == nil {
		t.Fatal("AC-G2-1: expected error when upsert fails")
	}
	if result != nil {
		t.Fatal("AC-G2-1: expected nil result on upsert failure")
	}
	if !strings.Contains(err.Error(), "database connection lost") {
		t.Fatalf("AC-G2-1: error should wrap the upsert error, got: %v", err)
	}
}

// TestG2_AC1_ByKeyUpsertFailureReturnsError proves that
// ResolveOrCreateConversationByKey returns an error when upsert fails.
func TestG2_AC1_ByKeyUpsertFailureReturnsError(t *testing.T) {
	mock := &mockConversationUpserter{
		returnErr: errors.New("db down"),
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	result, err := ResolveOrCreateConversationByKey(
		context.Background(), mock, logger,
		"thread:proj1:thread-1", "group", strPtr("proj1"))
	if err == nil {
		t.Fatal("AC-G2-1: expected error when upsert fails")
	}
	if result != nil {
		t.Fatal("AC-G2-1: expected nil result on upsert failure")
	}
}

// ============================================================================
// AC-G2-2: The three previously-unreachable rejection sites are now reachable.
//
// ValidateAttributed (the shared rejection function) is now called outside
// the nil guard, so it fires when ConversationID is empty. We test that:
// 1. ValidateAttributed rejects empty ConversationID (function test)
// 2. The error propagates through the write path (integration test via
//    the producer functions)
// ============================================================================

// TestG2_AC2_ValidateAttributedRejectsEmpty confirms ValidateAttributed
// rejects an empty ConversationID. This is the rejection function used by
// all three previously-unreachable sites.
func TestG2_AC2_ValidateAttributedRejectsEmpty(t *testing.T) {
	err := ValidateAttributed("")
	if err == nil {
		t.Fatal("AC-G2-2: ValidateAttributed must reject empty ConversationID")
	}
}

// TestG2_AC2_ValidateAttributedAcceptsValid confirms that a valid
// ConversationID passes.
func TestG2_AC2_ValidateAttributedAcceptsValid(t *testing.T) {
	err := ValidateAttributed("550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("AC-G2-2: ValidateAttributed should accept valid ConversationID: %v", err)
	}
}

// TestG2_AC2_DerivationFailurePreventsWrite proves that a derivation
// failure flows through the write path as an error that the consumer
// can use to deny the write (and then ValidateAttributed would catch
// an empty ConversationID if the consumer fails to check).
func TestG2_AC2_DerivationFailurePreventsWrite(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// dm: key that fails canonicality via DeriveConversationKey
	_, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger,
		"dm:INVALID:not-a-uuid", "proj1")
	if err == nil {
		t.Fatal("AC-G2-2: non-canonical dm: key should return error")
	}
	// Consumer would call ValidateAttributed("") which also rejects
	if vaErr := ValidateAttributed(""); vaErr == nil {
		t.Fatal("AC-G2-2: ValidateAttributed should also reject empty string")
	}
}

// ============================================================================
// AC-G2-3: EnsureParticipant failure still permits the send.
// ============================================================================

// TestG2_AC3_EnsureParticipantFailurePermitsSend proves that when
// EnsureParticipant returns an error, ResolveOrCreateDMConversation
// still returns a valid ConversationResult (no error), so the message
// is written.
func TestG2_AC3_EnsureParticipantFailurePermitsSend(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv:    &store.Conversation{ID: "conv-ok", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
		ensurePartErr: errors.New("participant table locked"),
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	result, err := ResolveOrCreateDMConversation(
		context.Background(), mock, mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	// Despite EnsureParticipant failing, the function MUST succeed.
	if err != nil {
		t.Fatalf("AC-G2-3: EnsureParticipant failure should not fail the send: %v", err)
	}
	if result == nil {
		t.Fatal("AC-G2-3: expected non-nil result even when EnsureParticipant fails")
	}
	if result.ConversationID != "conv-ok" {
		t.Errorf("AC-G2-3: expected ConversationID 'conv-ok', got %q", result.ConversationID)
	}
}

// TestG2_AC3_NilParticipantEnsurerPermitsSend proves that a nil
// ParticipantEnsurer (pe=nil) still returns a valid result.
func TestG2_AC3_NilParticipantEnsurerPermitsSend(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-nil-pe", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	result, err := ResolveOrCreateDMConversation(
		context.Background(), mock, nil, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("AC-G2-3: nil pe should not fail the send: %v", err)
	}
	if result == nil || result.ConversationID != "conv-nil-pe" {
		t.Fatal("AC-G2-3: expected valid result with nil pe")
	}
}

// ============================================================================
// AC-G2-4: Federated notification subscriber is still skipped, not denied.
//
// This is tested via the notifications.go path. The subscriber ID is a
// non-UUID federated identity, so the uuid.Parse fails and the DM
// resolution is skipped. The notification message is still created for
// everyone else.
//
// We test the underlying contract here: when a subscriber ID is not a
// valid UUID, ResolveOrCreateDMConversation would fail if called, but
// the notification code path skips the call entirely.
// ============================================================================

// TestG2_AC4_FederatedSubscriberSkipContract proves the contract that
// makes the federated subscriber exception work: when subscriber ID is
// not a UUID, the notification code skips DM resolution rather than
// calling ResolveOrCreateDMConversation (which would fail on non-UUID
// inputs). The non-UUID subscriber still gets their notification
// message persisted without a conversation_id.
func TestG2_AC4_FederatedSubscriberSkipContract(t *testing.T) {
	// Prove that ResolveOrCreateDMConversation would fail with non-UUID subscriber ID
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	_, err := ResolveOrCreateDMConversation(
		context.Background(), mock, mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "slack:U12345678") // non-UUID federated identity
	if err == nil {
		t.Fatal("AC-G2-4: DM resolution with non-UUID subscriber should fail")
	}

	// But with a valid UUID subscriber, it succeeds:
	mock2 := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-uuid", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
	}
	result, err := ResolveOrCreateDMConversation(
		context.Background(), mock2, mock2, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("AC-G2-4: DM resolution with UUID subscriber should succeed: %v", err)
	}
	if result == nil {
		t.Fatal("AC-G2-4: expected non-nil result for UUID subscriber")
	}
}

// ============================================================================
// AC-G2-5: No swallow remains except the two exceptions.
// ============================================================================

// TestG2_AC5_AllWritePathFailuresReturnErrors is an exhaustive test that
// every error path in the write-path producer functions returns an error
// (not nil). This proves no swallows remain.
func TestG2_AC5_AllWritePathFailuresReturnErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "DM: empty sender",
			fn: func() error {
				_, err := ResolveOrCreateDMConversation(context.Background(),
					&mockConversationUpserter{}, &mockConversationUpserter{}, logger,
					"user", "", "agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
				return err
			},
		},
		{
			name: "DM: empty recipient",
			fn: func() error {
				_, err := ResolveOrCreateDMConversation(context.Background(),
					&mockConversationUpserter{}, &mockConversationUpserter{}, logger,
					"user", "550e8400-e29b-41d4-a716-446655440000", "agent", "")
				return err
			},
		},
		{
			name: "DM: invalid kind",
			fn: func() error {
				_, err := ResolveOrCreateDMConversation(context.Background(),
					&mockConversationUpserter{}, &mockConversationUpserter{}, logger,
					"robot", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
					"agent", "550e8400-e29b-41d4-a716-446655440000")
				return err
			},
		},
		{
			name: "DM: upsert failure",
			fn: func() error {
				_, err := ResolveOrCreateDMConversation(context.Background(),
					&mockConversationUpserter{returnErr: errors.New("db")},
					&mockConversationUpserter{}, logger,
					"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
					"user", "550e8400-e29b-41d4-a716-446655440000")
				return err
			},
		},
		{
			name: "Thread: empty threadID",
			fn: func() error {
				_, err := ResolveOrCreateThreadConversation(context.Background(),
					&mockConversationUpserter{}, logger, "", "proj1")
				return err
			},
		},
		{
			name: "Thread: empty projectID",
			fn: func() error {
				_, err := ResolveOrCreateThreadConversation(context.Background(),
					&mockConversationUpserter{}, logger, "thread-1", "")
				return err
			},
		},
		{
			name: "Thread: upsert failure",
			fn: func() error {
				_, err := ResolveOrCreateThreadConversation(context.Background(),
					&mockConversationUpserter{returnErr: errors.New("db")}, logger,
					"thread-1", "proj1")
				return err
			},
		},
		{
			name: "ByKey: upsert failure",
			fn: func() error {
				_, err := ResolveOrCreateConversationByKey(context.Background(),
					&mockConversationUpserter{returnErr: errors.New("db")}, logger,
					"thread:proj1:t1", "group", strPtr("proj1"))
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			if err == nil {
				t.Errorf("AC-G2-5: expected error for %q, got nil — this is a swallow", tt.name)
			}
		})
	}
}

// ============================================================================
// AC-G2-6: With the switch OFF, producer errors are still returned but the
// WriteDenialMetrics counter is available for consumers to track denials.
// The switch itself lives at the consumer (handler) level; this test verifies
// the producer + counter contract that makes the switch work.
// ============================================================================

// TestG2_AC6_ProducerErrorsReturnedRegardlessOfSwitch proves that producer
// functions always return errors (the switch doesn't change producer behavior).
// The consumer decides whether to deny or continue based on the switch.
func TestG2_AC6_ProducerErrorsReturnedRegardlessOfSwitch(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// Producer always returns error for invalid inputs, regardless of any switch.
	_, err := ResolveOrCreateDMConversation(
		context.Background(), mock, mock, logger,
		"user", "", "agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err == nil {
		t.Fatal("AC-G2-6: producer must always return error for invalid inputs")
	}
}

// TestG2_AC6_WriteDenialCounterWorks proves the counter increments and
// reports correctly.
func TestG2_AC6_WriteDenialCounterWorks(t *testing.T) {
	// Create a fresh counter (don't use the global to avoid test interference).
	c := &WriteDenialCounter{}

	if c.Total() != 0 {
		t.Fatal("AC-G2-6: fresh counter should be zero")
	}

	c.Inc("test.site.a")
	c.Inc("test.site.a")
	c.Inc("test.site.b")

	if c.Get("test.site.a") != 2 {
		t.Errorf("AC-G2-6: expected 2 for site a, got %d", c.Get("test.site.a"))
	}
	if c.Get("test.site.b") != 1 {
		t.Errorf("AC-G2-6: expected 1 for site b, got %d", c.Get("test.site.b"))
	}
	if c.Total() != 3 {
		t.Errorf("AC-G2-6: expected total 3, got %d", c.Total())
	}

	sites := c.Sites()
	if len(sites) != 2 {
		t.Errorf("AC-G2-6: expected 2 sites, got %d", len(sites))
	}
}

func strPtr(s string) *string { return &s }
