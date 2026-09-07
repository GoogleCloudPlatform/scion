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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mockConversationReader is a test double for ConversationReader.
type mockConversationReader struct {
	conv *store.Conversation
	err  error
}

func (m *mockConversationReader) GetConversationByExternalRef(_ context.Context, _, _ string) (*store.Conversation, error) {
	return m.conv, m.err
}

// ---------------------------------------------------------------------------
// DEF-127a: ResolveDMConversationForRead error separation
// ---------------------------------------------------------------------------

func TestResolveDMConversationForRead_NotFound_ReturnsNilNil(t *testing.T) {
	// When the conversation row does not exist, the function must return
	// (nil, nil) — not an error. This is the normal first-use state for DMs.
	reader := &mockConversationReader{
		conv: nil,
		err:  store.ErrNotFound,
	}
	log := slog.Default()
	result, err := ResolveDMConversationForRead(
		context.Background(), reader, log,
		"agent", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"user", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
	)
	if err != nil {
		t.Fatalf("expected nil error for ErrNotFound, got: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result for ErrNotFound, got: %+v", result)
	}
}

func TestResolveDMConversationForRead_InfraError_ReturnsError(t *testing.T) {
	// DEF-127a: a database failure must NOT be collapsed into nil.
	// The function must return a non-nil error so the caller can serve 500.
	infraErr := fmt.Errorf("connection refused")
	reader := &mockConversationReader{
		conv: nil,
		err:  infraErr,
	}
	log := slog.Default()
	result, err := ResolveDMConversationForRead(
		context.Background(), reader, log,
		"agent", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"user", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
	)
	if err == nil {
		t.Fatalf("expected non-nil error for infrastructure failure, got nil")
	}
	if result != nil {
		t.Fatalf("expected nil result on error, got: %+v", result)
	}
	// The original error must be wrapped, not swallowed.
	if !errors.Is(err, infraErr) {
		t.Errorf("expected wrapped infrastructure error, got: %v", err)
	}
}

func TestResolveDMConversationForRead_Found_ReturnsResult(t *testing.T) {
	// When the conversation exists, the result is returned with no error.
	conv := &store.Conversation{
		ID:          "conv-123",
		ExternalRef: "dm:agent:aaa:user:bbb",
		Kind:        "direct",
		Surface:     "native",
	}
	reader := &mockConversationReader{
		conv: conv,
		err:  nil,
	}
	log := slog.Default()
	result, err := ResolveDMConversationForRead(
		context.Background(), reader, log,
		"agent", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"user", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ConversationID != "conv-123" {
		t.Errorf("expected ConversationID %q, got %q", "conv-123", result.ConversationID)
	}
}

func TestResolveDMConversationForRead_EmptyIDs_ReturnsNilNil(t *testing.T) {
	// Empty IDs are a no-op, not an error.
	reader := &mockConversationReader{
		conv: nil,
		err:  fmt.Errorf("should not be called"),
	}
	log := slog.Default()
	result, err := ResolveDMConversationForRead(
		context.Background(), reader, log,
		"agent", "",
		"user", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
	)
	if err != nil {
		t.Fatalf("expected nil error for empty IDs, got: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result for empty IDs, got: %+v", result)
	}
}
