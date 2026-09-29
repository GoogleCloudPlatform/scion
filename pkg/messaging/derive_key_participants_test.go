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

// Tests for the p2a-u1 review round-2 fixes (design agent-reincarnate
// §3.7, Amendment A25.7, R1 widened): table tests for
// ResolveOrCreateConversationByKey's WithParticipants option and
// ResolveOrCreateThreadConversation's WithThreadParticipants forwarding,
// covering every branch the review's mutations m1, m5, m6 and m12 target.
// Reuses mockConversationUpserter from conversation_test.go (same package).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// failFirstEnsureParticipant wraps mockConversationUpserter's
// ParticipantEnsurer and fails only the FIRST EnsureParticipant call,
// delegating every subsequent call to the wrapped mock (which succeeds and
// records it). This proves the G2 loop in ensureConversationParticipants
// attempts BOTH principals even when the first fails — mutation m12 (an
// early `return` after the first error) is only caught by a test that can
// tell "attempted once" from "attempted twice".
type failFirstEnsureParticipant struct {
	*mockConversationUpserter
	calls int
}

func (m *failFirstEnsureParticipant) EnsureParticipant(ctx context.Context, p *store.ConversationParticipant) error {
	m.calls++
	if m.calls == 1 {
		return errors.New("injected failure on first ensure")
	}
	return m.mockConversationUpserter.EnsureParticipant(ctx, p)
}

const (
	participantsTestDMRef  = "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"
	participantsTestThread = "thread:proj-1:topic-1"
)

// TestResolveOrCreateConversationByKey_Participants is the A25.7 R1 table
// test for the sink's WithParticipants option.
func TestResolveOrCreateConversationByKey_Participants(t *testing.T) {
	t.Run("direct_with_participants_both_ensured", func(t *testing.T) {
		mock := &mockConversationUpserter{
			returnConv: &store.Conversation{ID: "c-direct", ExternalRef: participantsTestDMRef, Kind: "direct"},
		}
		logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

		got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
			participantsTestDMRef, "direct", nil, WithParticipants(mock))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if len(mock.ensuredParticipants) != 2 {
			t.Fatalf("expected 2 ensured participants, got %d: %+v", len(mock.ensuredParticipants), mock.ensuredParticipants)
		}
		// Parsed from the canonical ExternalRef: agent first (sorts first), user second.
		if mock.ensuredParticipants[0].PrincipalKind != "agent" || mock.ensuredParticipants[0].PrincipalID != "6ba7b810-9dad-11d1-80b4-00c04fd430c8" {
			t.Errorf("unexpected first participant: %+v", mock.ensuredParticipants[0])
		}
		if mock.ensuredParticipants[1].PrincipalKind != "user" || mock.ensuredParticipants[1].PrincipalID != "550e8400-e29b-41d4-a716-446655440000" {
			t.Errorf("unexpected second participant: %+v", mock.ensuredParticipants[1])
		}
		for _, p := range mock.ensuredParticipants {
			if p.ConversationID != "c-direct" {
				t.Errorf("expected ConversationID c-direct, got %q", p.ConversationID)
			}
		}
	})

	t.Run("group_kind_zero_ensure_calls", func(t *testing.T) {
		// A25.6 scope pin: kind=="group" must never register participants,
		// even when a ParticipantEnsurer is supplied.
		mock := &mockConversationUpserter{
			returnConv: &store.Conversation{ID: "c-group", ExternalRef: participantsTestThread, Kind: "group"},
		}
		logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
		projID := "proj-1"

		got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
			participantsTestThread, "group", &projID, WithParticipants(mock))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if len(mock.ensuredParticipants) != 0 {
			t.Fatalf("expected 0 ensured participants for kind=group, got %d: %+v", len(mock.ensuredParticipants), mock.ensuredParticipants)
		}
	})

	t.Run("direct_no_option_zero_calls", func(t *testing.T) {
		// No WithParticipants option at all: must be a pure no-op, exactly
		// like every call site before A25.6.
		mock := &mockConversationUpserter{
			returnConv: &store.Conversation{ID: "c-noopt", ExternalRef: participantsTestDMRef, Kind: "direct"},
		}
		logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

		got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
			participantsTestDMRef, "direct", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if len(mock.ensuredParticipants) != 0 {
			t.Fatalf("expected 0 ensured participants with no option, got %d: %+v", len(mock.ensuredParticipants), mock.ensuredParticipants)
		}
	})

	t.Run("direct_with_explicit_nil_participants_zero_calls_no_panic", func(t *testing.T) {
		// A25.15 (upstream gemini-code-assist review, GoogleCloudPlatform/scion#2079):
		// WithParticipants(nil) is reachable in principle — unlike omitting the
		// option entirely (the case above), this explicitly sets
		// cfg.participants to a typed nil ParticipantEnsurer interface value.
		// The sink's existing `cfg.participants != nil` guard already prevents
		// ensureConversationParticipants from being called in this case, but
		// this test exercises the option end-to-end to prove that path stays a
		// clean no-op (no panic, no EnsureParticipant calls) rather than just
		// asserting the unexported helper in isolation.
		mock := &mockConversationUpserter{
			returnConv: &store.Conversation{ID: "c-nilopt", ExternalRef: participantsTestDMRef, Kind: "direct"},
		}
		logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

		var got *ConversationResult
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("must not panic with an explicit nil ParticipantEnsurer, got: %v", r)
				}
			}()
			got, err = ResolveOrCreateConversationByKey(context.Background(), mock, logger,
				participantsTestDMRef, "direct", nil, WithParticipants(nil))
		}()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if len(mock.ensuredParticipants) != 0 {
			t.Fatalf("expected 0 ensured participants with an explicit nil ParticipantEnsurer, got %d: %+v", len(mock.ensuredParticipants), mock.ensuredParticipants)
		}
	})

	t.Run("first_ensure_fails_second_still_attempted", func(t *testing.T) {
		// Kills m12: an early `return` after the first EnsureParticipant
		// error would leave this at 1 call / 1 ensured participant. The
		// correct G2 semantics (continue past a non-fatal failure) leave it
		// at 2 calls / 1 ensured participant (the failed one is never
		// recorded by the mock, the second succeeds and is recorded), with a
		// WARN logged for the failure.
		base := &mockConversationUpserter{
			returnConv: &store.Conversation{ID: "c-partial-fail", ExternalRef: participantsTestDMRef, Kind: "direct"},
		}
		wrapped := &failFirstEnsureParticipant{mockConversationUpserter: base}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		got, err := ResolveOrCreateConversationByKey(context.Background(), base, logger,
			participantsTestDMRef, "direct", nil, WithParticipants(wrapped))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil result — a participant-registration failure must not fail the resolve")
		}
		if wrapped.calls != 2 {
			t.Fatalf("expected EnsureParticipant to be attempted twice (both principals), got %d calls", wrapped.calls)
		}
		if len(base.ensuredParticipants) != 1 {
			t.Fatalf("expected exactly 1 successfully-recorded participant (the second), got %d: %+v", len(base.ensuredParticipants), base.ensuredParticipants)
		}
		if !strings.Contains(buf.String(), "participant registration failed") {
			t.Errorf("expected a WARN log for the failed first ensure, got: %s", buf.String())
		}
	})

	t.Run("unparseable_direct_ref_warn_no_panic", func(t *testing.T) {
		// The DB-returned ExternalRef should always be a canonical dm: key
		// for kind=="direct" in production, but the sink must not panic or
		// error if it somehow is not — it logs a WARN and still returns the
		// resolved conversation.
		badRef := "dm:not-a-valid-key"
		mock := &mockConversationUpserter{
			returnConv: &store.Conversation{ID: "c-badref", ExternalRef: badRef, Kind: "direct"},
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		var got *ConversationResult
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("must not panic on an unparseable direct external_ref, got: %v", r)
				}
			}()
			got, err = ResolveOrCreateConversationByKey(context.Background(), mock, logger,
				badRef, "direct", nil, WithParticipants(mock))
		}()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil result even when participant registration is skipped")
		}
		if got.ConversationID != "c-badref" {
			t.Errorf("expected ConversationID c-badref, got %q", got.ConversationID)
		}
		if len(mock.ensuredParticipants) != 0 {
			t.Fatalf("expected 0 ensured participants for an unparseable ref, got %d", len(mock.ensuredParticipants))
		}
		if !strings.Contains(buf.String(), "did not parse as a dm key") {
			t.Errorf("expected a WARN log about the unparseable ref, got: %s", buf.String())
		}
	})
}

// TestResolveOrCreateThreadConversation_DMPath_WithThreadParticipants kills
// m5: dropping the WithThreadParticipants -> WithParticipants forwarding in
// ResolveOrCreateThreadConversation disables participant registration for
// every thread-path call site at once (chat v2's three sites, messagebroker's
// two, and broker-inbound's Phase 5 branch) without touching
// ResolveOrCreateConversationByKey directly, so it needs its own test.
func TestResolveOrCreateThreadConversation_DMPath_WithThreadParticipants(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "c-thread-dm", ExternalRef: participantsTestDMRef, Kind: "direct"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// A "dm:"-prefixed threadID routes ResolveOrCreateThreadConversation
	// through DeriveConversationKey's Case 1, producing kind=="direct" —
	// exactly chat v2's 1:1 DM route (report-7-gteam-2a case (e)).
	got, err := ResolveOrCreateThreadConversation(context.Background(), mock, logger,
		participantsTestDMRef, "", WithThreadParticipants(mock))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if len(mock.ensuredParticipants) != 2 {
		t.Fatalf("expected 2 ensured participants via the thread-path forwarding, got %d: %+v", len(mock.ensuredParticipants), mock.ensuredParticipants)
	}
}

// TestResolveOrCreateThreadConversation_NonDMThread_NoThreadParticipants
// confirms the forwarding option is a no-op for an ordinary (non-dm:)
// thread key, even when supplied.
func TestResolveOrCreateThreadConversation_NonDMThread_NoThreadParticipants(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "c-thread-group", ExternalRef: "thread:proj-1:topic-1", Kind: "group"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got, err := ResolveOrCreateThreadConversation(context.Background(), mock, logger,
		"topic-1", "proj-1", WithThreadParticipants(mock))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if len(mock.ensuredParticipants) != 0 {
		t.Fatalf("expected 0 ensured participants for a non-dm thread key, got %d", len(mock.ensuredParticipants))
	}
}

// TestEnsureConversationParticipants_NilParticipantEnsurer_NoPanic is a
// direct unit test of the unexported helper itself (A25.15, upstream
// gemini-code-assist review on GoogleCloudPlatform/scion#2079): both current
// callers already guard against a nil ParticipantEnsurer before calling
// ensureConversationParticipants (ResolveOrCreateConversationByKey's
// `cfg.participants != nil` check in this file, and
// ResolveOrCreateDMConversation's B7 nil-pe guard in conversation.go), but
// the helper must also be safe on its own — a nil pe must cause no panic and
// no work (no EnsureParticipant call, which is trivially true here since
// there is no non-nil interface value to call a method on if this doesn't
// panic).
func TestEnsureConversationParticipants_NilParticipantEnsurer_NoPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("must not panic with a nil ParticipantEnsurer, got: %v", r)
			}
		}()
		ensureConversationParticipants(context.Background(), nil, logger, "conv-id", participantsTestDMRef)
	}()
}
