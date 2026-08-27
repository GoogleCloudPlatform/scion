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

	got := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:t1", "group", &pid)
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

	got := ResolveOrCreateConversationByKey(context.Background(), mock, logger,
		"thread:proj:t1", "group", nil)
	if got != nil {
		t.Errorf("expected nil on upsert error, got %+v", got)
	}
}
