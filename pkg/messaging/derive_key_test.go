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

// ---------------------------------------------------------------------------
// DeriveConversationKey — golden vector tests (AC-DEF15-2)
// ---------------------------------------------------------------------------

func TestDeriveConversationKey_GoldenVectors(t *testing.T) {
	const (
		agentUUID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
		userUUID  = "550e8400-e29b-41d4-a716-446655440000"
	)

	canonicalDMKey := "dm:agent:" + agentUUID + ":user:" + userUUID

	type want struct {
		extRef    string
		kind      string
		projectID *string // nil means no project
		wantErr   bool
	}

	pstr := func(s string) *string { return &s }

	tests := []struct {
		name  string
		input KeyInputs
		want  want
	}{
		// ---------------------------------------------------------------
		// Success vectors
		// ---------------------------------------------------------------
		{
			name: "case1: dm-prefixed canonical key returns verbatim",
			input: KeyInputs{
				ThreadID: canonicalDMKey,
			},
			want: want{
				extRef:    canonicalDMKey,
				kind:      "direct",
				projectID: nil,
			},
		},
		{
			name: "case2: non-dm ThreadID with ProjectID returns thread key",
			input: KeyInputs{
				ThreadID:  "my-thread-123",
				ProjectID: "proj-42",
			},
			want: want{
				extRef:    "thread:proj-42:my-thread-123",
				kind:      "group",
				projectID: pstr("proj-42"),
			},
		},
		{
			name: "case3: empty ThreadID derives from principal pair",
			input: KeyInputs{
				SenderKind:    "user",
				SenderID:      userUUID,
				RecipientKind: "agent",
				RecipientID:   agentUUID,
			},
			want: want{
				extRef:    canonicalDMKey, // sorted: agent < user
				kind:      "direct",
				projectID: nil,
			},
		},

		// ---------------------------------------------------------------
		// Error vectors — every error branch gets a vector
		// ---------------------------------------------------------------
		{
			name: "error1: dm prefix + malformed (wrong segment count)",
			input: KeyInputs{
				ThreadID: "dm:agent:" + agentUUID,
			},
			want: want{wantErr: true},
		},
		{
			name: "error2: dm prefix + unknown kind",
			input: KeyInputs{
				ThreadID: "dm:bot:" + agentUUID + ":user:" + userUUID,
			},
			want: want{wantErr: true},
		},
		{
			name: "error3: dm prefix + invalid UUID",
			input: KeyInputs{
				ThreadID: "dm:agent:not-a-uuid:user:" + userUUID,
			},
			want: want{wantErr: true},
		},
		{
			name: "error4: dm prefix + non-canonical UUID (uppercase hex)",
			input: KeyInputs{
				ThreadID: "dm:agent:6BA7B810-9DAD-11D1-80B4-00C04FD430C8:user:" + userUUID,
			},
			want: want{wantErr: true},
		},
		{
			name: "error5: dm prefix + non-canonical token order (user before agent)",
			input: KeyInputs{
				ThreadID: "dm:user:" + userUUID + ":agent:" + agentUUID,
			},
			want: want{wantErr: true},
		},
		{
			name: "error6: dm prefix + unhyphenated UUID",
			input: KeyInputs{
				// uuid.Parse accepts unhyphenated, but DMConversationKey produces hyphenated
				ThreadID: "dm:agent:6ba7b8109dad11d180b400c04fd430c8:user:" + userUUID,
			},
			want: want{wantErr: true},
		},
		{
			name: "error7: non-dm ThreadID + empty ProjectID",
			input: KeyInputs{
				ThreadID:  "my-thread",
				ProjectID: "",
			},
			want: want{wantErr: true},
		},
		{
			name: "error8: empty ThreadID + empty SenderID",
			input: KeyInputs{
				SenderKind:    "user",
				SenderID:      "",
				RecipientKind: "agent",
				RecipientID:   agentUUID,
			},
			want: want{wantErr: true},
		},
		{
			name: "error9: empty ThreadID + unknown SenderKind",
			input: KeyInputs{
				SenderKind:    "bot",
				SenderID:      userUUID,
				RecipientKind: "agent",
				RecipientID:   agentUUID,
			},
			want: want{wantErr: true},
		},
		{
			name: "error10: empty ThreadID + invalid SenderID UUID",
			input: KeyInputs{
				SenderKind:    "user",
				SenderID:      "not-a-uuid",
				RecipientKind: "agent",
				RecipientID:   agentUUID,
			},
			want: want{wantErr: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extRef, kind, projectID, err := DeriveConversationKey(tt.input)

			if tt.want.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (extRef=%q, kind=%q)", extRef, kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if extRef != tt.want.extRef {
				t.Errorf("extRef: got %q, want %q", extRef, tt.want.extRef)
			}
			if kind != tt.want.kind {
				t.Errorf("kind: got %q, want %q", kind, tt.want.kind)
			}

			// Compare projectID.
			switch {
			case tt.want.projectID == nil && projectID != nil:
				t.Errorf("projectID: got %q, want nil", *projectID)
			case tt.want.projectID != nil && projectID == nil:
				t.Errorf("projectID: got nil, want %q", *tt.want.projectID)
			case tt.want.projectID != nil && projectID != nil && *projectID != *tt.want.projectID:
				t.Errorf("projectID: got %q, want %q", *projectID, *tt.want.projectID)
			}
		})
	}
}

// TestDeriveConversationKey_Case1VerbatimReturn explicitly verifies that
// a canonical dm: key is returned byte-for-byte from the input, not
// reconstructed from parsed components.
func TestDeriveConversationKey_Case1VerbatimReturn(t *testing.T) {
	input := "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"

	extRef, kind, projectID, err := DeriveConversationKey(KeyInputs{ThreadID: input})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The pointer comparison ensures we are returning the input string, not
	// a reconstructed copy (though Go string interning may make this hold
	// even for copies — the value check is the real assertion).
	if extRef != input {
		t.Errorf("extRef not verbatim: got %q, want %q", extRef, input)
	}
	if kind != "direct" {
		t.Errorf("kind: got %q, want %q", kind, "direct")
	}
	if projectID != nil {
		t.Errorf("projectID: got %q, want nil", *projectID)
	}
}

// ---------------------------------------------------------------------------
// ResolveOrCreateConversationByKey tests
// ---------------------------------------------------------------------------

func TestResolveOrCreateConversationByKey_HappyPath(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-key-1", ExternalRef: "thread:proj:t1"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	pid := "proj"

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:t1", "group", &pid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.ConversationID != "conv-key-1" {
		t.Errorf("ConversationID: got %q, want %q", got.ConversationID, "conv-key-1")
	}
	if got.ExternalRef != "thread:proj:t1" {
		t.Errorf("ExternalRef: got %q, want %q", got.ExternalRef, "thread:proj:t1")
	}
}

func TestResolveOrCreateConversationByKey_UpsertError(t *testing.T) {
	mock := &mockConversationUpserter{
		returnErr: errors.New("db connection lost"),
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:t1", "group", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil on upsert error, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Sink-level topic lookup tests (DEF-20 unify)
// ---------------------------------------------------------------------------

func TestResolveOrCreateConversationByKey_SinkTopicLookup_Resolves(t *testing.T) {
	// When WithKeyTopicLookup is provided and extRef is "thread:proj:topicID",
	// and the lookup returns a conversation_id, the function returns it
	// WITHOUT calling UpsertConversationByExternalRef.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "should-not-be-used"},
	}
	lookup := &mockTopicLookup{
		topics: map[string]string{
			"topicID": "conv-from-topic",
		},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	pid := "proj"

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:topicID", "group", &pid, WithKeyTopicLookup(lookup))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result from topic lookup")
	}
	if got.ConversationID != "conv-from-topic" {
		t.Errorf("ConversationID: got %q, want %q", got.ConversationID, "conv-from-topic")
	}
	if mock.lastConv != nil {
		t.Error("UpsertConversationByExternalRef must NOT be called when topic lookup succeeds")
	}
}

func TestResolveOrCreateConversationByKey_SinkTopicLookup_ErrNotFound_FallsThrough(t *testing.T) {
	// When the lookup returns store.ErrNotFound, it falls through to upsert.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-upserted", ExternalRef: "thread:proj:nonTopic"},
	}
	lookup := &mockTopicLookup{topics: map[string]string{}} // empty = not found
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	pid := "proj"

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:nonTopic", "group", &pid, WithKeyTopicLookup(lookup))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result from upsert fallthrough")
	}
	if got.ConversationID != "conv-upserted" {
		t.Errorf("ConversationID: got %q, want %q", got.ConversationID, "conv-upserted")
	}
	if mock.lastConv == nil {
		t.Error("UpsertConversationByExternalRef must be called when topic not found")
	}
}

func TestResolveOrCreateConversationByKey_SinkTopicLookup_InfraError_ReturnsNil(t *testing.T) {
	// When the lookup returns an infra error (not ErrNotFound), return nil.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "should-not-be-used"},
	}
	lookup := &mockTopicLookupWithError{err: errors.New("connection refused")}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pid := "proj"

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:topicID", "group", &pid, WithKeyTopicLookup(lookup))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil on infra error, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("UpsertConversationByExternalRef must NOT be called on infra error")
	}
}

func TestResolveOrCreateConversationByKey_SinkTopicLookup_NoConvID_ReturnsNil(t *testing.T) {
	// Topic exists but has no conversation_id (not yet backfilled) — return nil.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "should-not-be-used"},
	}
	lookup := &mockTopicLookup{
		topics: map[string]string{
			"topicID": "", // exists but empty conversation_id
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pid := "proj"

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:topicID", "group", &pid, WithKeyTopicLookup(lookup))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil for topic without conversation_id, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("UpsertConversationByExternalRef must NOT be called for unbackfilled topic")
	}
}

func TestResolveOrCreateConversationByKey_SinkTopicLookup_MalformedThreadRef_Refuses(t *testing.T) {
	// F2: "thread:abc" (2 parts) has the thread: prefix but is malformed.
	// Must refuse (return nil), not fall through to upsert.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "should-not-be-used"},
	}
	lookup := &mockTopicLookup{topics: map[string]string{}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pid := "proj"

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:abc", "group", &pid, WithKeyTopicLookup(lookup))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil for malformed thread: ref, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("UpsertConversationByExternalRef must NOT be called for malformed thread: ref")
	}
	if !strings.Contains(err.Error(), "malformed thread: ref") {
		t.Error("expected error about malformed thread: ref")
	}
}

func TestResolveOrCreateConversationByKey_SinkTopicLookup_WellFormedRef_Resolves(t *testing.T) {
	// Paired positive for F2: a well-formed 3-part thread:proj:topicID still
	// resolves via topic lookup as before.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "should-not-be-used"},
	}
	lookup := &mockTopicLookup{
		topics: map[string]string{"topicID": "conv-from-topic"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	pid := "proj"

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:topicID", "group", &pid, WithKeyTopicLookup(lookup))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result from well-formed thread ref")
	}
	if got.ConversationID != "conv-from-topic" {
		t.Errorf("ConversationID: got %q, want %q", got.ConversationID, "conv-from-topic")
	}
	if mock.lastConv != nil {
		t.Error("UpsertConversationByExternalRef must NOT be called when topic lookup succeeds")
	}
}

func TestResolveOrCreateConversationByKey_SinkTopicLookup_SkipsNonGroupKind(t *testing.T) {
	// Topic lookup should only intercept kind=="group" with "thread:" prefix.
	// For "direct" kind, it should fall through to upsert even with a lookup.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-direct", ExternalRef: "dm:agent:x:user:y"},
	}
	lookup := &mockTopicLookup{
		topics: map[string]string{
			"some-id": "should-not-be-used",
		},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"dm:agent:x:user:y", "direct", nil, WithKeyTopicLookup(lookup))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result for direct kind")
	}
	if got.ConversationID != "conv-direct" {
		t.Errorf("ConversationID: got %q, want %q", got.ConversationID, "conv-direct")
	}
}

func TestResolveOrCreateConversationByKey_SinkTopicLookup_SoftDeletedTopic_DoesNotMint(t *testing.T) {
	// DEF-27 unit-level: a soft-deleted topic must resolve via the
	// including-deleted accessor, NOT the user-facing one. The sink must
	// call GetTopicConversationIDIncludingDeleted, which sees tombstoned
	// topics, and return the existing conversation_id without minting.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "shadow-should-not-appear"},
	}
	lookup := &mockTopicLookup{
		topics:  map[string]string{"deletedTopic": "conv-existing"},
		deleted: map[string]bool{"deletedTopic": true},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	pid := "proj"

	got, err := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:deletedTopic", "group", &pid, WithKeyTopicLookup(lookup))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result for soft-deleted topic with linked conversation")
	}
	if got.ConversationID != "conv-existing" {
		t.Errorf("ConversationID: got %q, want %q — sink minted instead of resolving via tombstoned topic",
			got.ConversationID, "conv-existing")
	}
	if mock.lastConv != nil {
		t.Error("UpsertConversationByExternalRef must NOT be called for soft-deleted native topic — would mint a shadow conversation")
	}
	if lookup.calledMethod != "GetTopicConversationIDIncludingDeleted" {
		t.Errorf("sink must call GetTopicConversationIDIncludingDeleted, called %q", lookup.calledMethod)
	}
}

func TestResolveOrCreateConversationByKey_WithSurfaceAndParentRef(t *testing.T) {
	// Verify WithSurface and WithParentRef are applied to the conversation.
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	pid := "proj"
	agentID := "agent-123"

	_, _ = ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"ext-ref-1", "group", &pid,
		WithSurface("discord"),
		WithParentRef("parent-ref-1"),
		WithDefaultAgentID(&agentID))

	if mock.lastConv == nil {
		t.Fatal("expected upsert to be called")
	}
	if mock.lastConv.Surface != "discord" {
		t.Errorf("Surface: got %q, want %q", mock.lastConv.Surface, "discord")
	}
	if mock.lastConv.ParentRef != "parent-ref-1" {
		t.Errorf("ParentRef: got %q, want %q", mock.lastConv.ParentRef, "parent-ref-1")
	}
	if mock.lastConv.DefaultAgentID == nil || *mock.lastConv.DefaultAgentID != "agent-123" {
		t.Errorf("DefaultAgentID: got %v, want %q", mock.lastConv.DefaultAgentID, "agent-123")
	}
}

func TestResolveOrCreateConversationByKey_DefaultSurfaceIsNative(t *testing.T) {
	// Without WithSurface, surface should default to "native".
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	pid := "proj"

	_, _ = ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:t1", "group", &pid)

	if mock.lastConv == nil {
		t.Fatal("expected upsert to be called")
	}
	if mock.lastConv.Surface != "native" {
		t.Errorf("Surface: got %q, want %q", mock.lastConv.Surface, "native")
	}
}
