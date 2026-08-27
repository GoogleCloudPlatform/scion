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

// mockConversationUpserter is a test double for ConversationUpserter.
type mockConversationUpserter struct {
	// lastConv captures the most recent conversation passed to Upsert.
	lastConv *store.Conversation
	// returnConv is the conversation returned by Upsert.
	returnConv *store.Conversation
	// returnErr is the error returned by Upsert.
	returnErr error
}

func (m *mockConversationUpserter) UpsertConversationByExternalRef(
	_ context.Context, conv *store.Conversation,
) (*store.Conversation, error) {
	m.lastConv = conv
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	if m.returnConv != nil {
		return m.returnConv, nil
	}
	// Default: echo back with an ID.
	result := *conv
	result.ID = "conv-id-123"
	return &result, nil
}

// ---------------------------------------------------------------------------
// ResolveOrCreateDMConversation tests
// ---------------------------------------------------------------------------

func TestResolveOrCreateDMConversation_HappyPath(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-abc", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got := ResolveOrCreateDMConversation(context.Background(), mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.ConversationID != "conv-abc" {
		t.Errorf("expected conv-abc, got %q", got.ConversationID)
	}
	if got.ExternalRef != "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("expected ExternalRef from mock, got %q", got.ExternalRef)
	}
}

func TestResolveOrCreateDMConversation_EmptySender(t *testing.T) {
	mock := &mockConversationUpserter{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := ResolveOrCreateDMConversation(context.Background(), mock, logger, "user", "", "agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if got != nil {
		t.Errorf("expected nil for empty sender, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called with empty sender")
	}
}

func TestResolveOrCreateDMConversation_EmptyRecipient(t *testing.T) {
	mock := &mockConversationUpserter{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := ResolveOrCreateDMConversation(context.Background(), mock, logger, "user", "550e8400-e29b-41d4-a716-446655440000", "agent", "")
	if got != nil {
		t.Errorf("expected nil for empty recipient, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called with empty recipient")
	}
}

func TestResolveOrCreateDMConversation_UpsertError(t *testing.T) {
	mock := &mockConversationUpserter{
		returnErr: errors.New("db connection lost"),
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := ResolveOrCreateDMConversation(context.Background(), mock, logger,
		"user", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if got != nil {
		t.Errorf("expected nil on upsert error, got %+v", got)
	}
	output := buf.String()
	if !strings.Contains(output, "conversation resolution failed") {
		t.Errorf("expected error log, got: %s", output)
	}
}

func TestResolveOrCreateDMConversation_ExternalRefIsKindEncoded(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// Call with user first, agent second — ref should sort to agent:...:user:...
	ResolveOrCreateDMConversation(context.Background(), mock, logger,
		"user", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if mock.lastConv == nil {
		t.Fatal("expected upsert to be called")
	}
	expected := "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"
	if mock.lastConv.ExternalRef != expected {
		t.Errorf("expected external_ref %q, got %q", expected, mock.lastConv.ExternalRef)
	}
}

func TestResolveOrCreateDMConversation_ProjectIDAlwaysNil(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// DM conversations must never have ProjectID set (design 2.4.1).
	ResolveOrCreateDMConversation(context.Background(), mock, logger,
		"user", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if mock.lastConv == nil {
		t.Fatal("expected upsert to be called")
	}
	if mock.lastConv.ProjectID != nil {
		t.Errorf("expected ProjectID to be nil for DM conversations, got %v", *mock.lastConv.ProjectID)
	}
}

func TestResolveOrCreateDMConversation_ReturnsExternalRefFromDB(t *testing.T) {
	// Verify that the ExternalRef in the result comes from the DB response,
	// not reconstructed from inputs.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{
			ID:          "conv-from-db",
			ExternalRef: "dm:actual-from-db",
		},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got := ResolveOrCreateDMConversation(context.Background(), mock, logger,
		"user", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.ExternalRef != "dm:actual-from-db" {
		t.Errorf("expected ExternalRef from DB 'dm:actual-from-db', got %q", got.ExternalRef)
	}
}

func TestResolveOrCreateDMConversation_EmptyKindReturnsNil(t *testing.T) {
	// Belt-and-suspenders: even if a hub handler accidentally passes an empty
	// senderKind (the primary defense is to not call this function at all),
	// ResolveOrCreateDMConversation must reject it via DMConversationKey
	// validation, returning nil and creating no conversation row.
	mock := &mockConversationUpserter{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := ResolveOrCreateDMConversation(context.Background(), mock, logger,
		"", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if got != nil {
		t.Errorf("expected nil for empty kind, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called — no conversation row should be created")
	}
}

func TestResolveOrCreateDMConversation_InvalidKindReturnsNil(t *testing.T) {
	mock := &mockConversationUpserter{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := ResolveOrCreateDMConversation(context.Background(), mock, logger,
		"bot", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if got != nil {
		t.Errorf("expected nil for invalid kind, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called with invalid kind")
	}
	output := buf.String()
	if !strings.Contains(output, "invalid DM key inputs") {
		t.Errorf("expected warning log about invalid DM key, got: %s", output)
	}
}

// ---------------------------------------------------------------------------
// ResolveOrCreateThreadConversation tests
// ---------------------------------------------------------------------------

func TestResolveOrCreateThreadConversation_HappyPath(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{
			ID:          "conv-thread-abc",
			ExternalRef: "thread:proj1:thread-123",
		},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got := ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-123", "proj1")
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.ConversationID != "conv-thread-abc" {
		t.Errorf("expected conv-thread-abc, got %q", got.ConversationID)
	}
	if got.ExternalRef != "thread:proj1:thread-123" {
		t.Errorf("expected ExternalRef thread:proj1:thread-123, got %q", got.ExternalRef)
	}
}

func TestResolveOrCreateThreadConversation_EmptyThreadID(t *testing.T) {
	mock := &mockConversationUpserter{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := ResolveOrCreateThreadConversation(context.Background(), mock, logger, "", "proj1")
	if got != nil {
		t.Errorf("expected nil for empty threadID, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called with empty threadID")
	}
}

func TestResolveOrCreateThreadConversation_EmptyProjectID(t *testing.T) {
	mock := &mockConversationUpserter{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-123", "")
	if got != nil {
		t.Errorf("expected nil for empty projectID, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called with empty projectID")
	}
}

func TestResolveOrCreateThreadConversation_UpsertError(t *testing.T) {
	mock := &mockConversationUpserter{
		returnErr: errors.New("db error"),
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-123", "proj1")
	if got != nil {
		t.Errorf("expected nil on upsert error, got %+v", got)
	}
	output := buf.String()
	if !strings.Contains(output, "conversation resolution failed") {
		t.Errorf("expected error log, got: %s", output)
	}
}

func TestResolveOrCreateThreadConversation_ExternalRefFormat(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-ABC", "proj-42")
	if mock.lastConv == nil {
		t.Fatal("expected upsert to be called")
	}
	expected := "thread:proj-42:thread-ABC"
	if mock.lastConv.ExternalRef != expected {
		t.Errorf("expected external_ref %q, got %q", expected, mock.lastConv.ExternalRef)
	}
}

func TestResolveOrCreateThreadConversation_ProjectIDSet(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-ABC", "proj-42")
	if mock.lastConv == nil {
		t.Fatal("expected upsert to be called")
	}
	if mock.lastConv.ProjectID == nil {
		t.Fatal("expected ProjectID to be set for thread conversations")
	}
	if *mock.lastConv.ProjectID != "proj-42" {
		t.Errorf("expected ProjectID proj-42, got %q", *mock.lastConv.ProjectID)
	}
}

func TestResolveOrCreateThreadConversation_KindIsGroup(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-ABC", "proj-42")
	if mock.lastConv == nil {
		t.Fatal("expected upsert to be called")
	}
	if mock.lastConv.Kind != "group" {
		t.Errorf("expected Kind 'group', got %q", mock.lastConv.Kind)
	}
}

// ---------------------------------------------------------------------------
// AC-DEF15-5: write-then-read tests
//
// These use mockConversationStore from backfill_test.go, which implements
// both ConversationUpserter and ConversationReader with in-memory storage.
// ---------------------------------------------------------------------------

func TestWriteThenRead_DMPrefixedThreadID(t *testing.T) {
	// AC-DEF15-5: Write with dm:-prefixed ThreadID, then read with same inputs.
	// The read must find the row the write created (same ConversationID).
	cs := &mockConversationStore{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	dmKey := "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"

	writeResult := ResolveOrCreateThreadConversation(
		context.Background(), cs, logger, dmKey, "")
	if writeResult == nil {
		t.Fatal("write: expected non-nil result for dm:-prefixed ThreadID")
	}

	readResult := ResolveThreadConversationForRead(
		context.Background(), cs, logger, dmKey, "")
	if readResult == nil {
		t.Fatal("read: expected non-nil result — should find the row the write created")
	}

	if readResult.ConversationID != writeResult.ConversationID {
		t.Errorf("ConversationID mismatch: write=%q, read=%q",
			writeResult.ConversationID, readResult.ConversationID)
	}
	if readResult.ExternalRef != writeResult.ExternalRef {
		t.Errorf("ExternalRef mismatch: write=%q, read=%q",
			writeResult.ExternalRef, readResult.ExternalRef)
	}
}

func TestWriteThenRead_NonDMThreadID(t *testing.T) {
	// AC-DEF15-5: Write with a non-dm ThreadID, then read with same inputs.
	cs := &mockConversationStore{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	writeResult := ResolveOrCreateThreadConversation(
		context.Background(), cs, logger, "thread-xyz", "proj-1")
	if writeResult == nil {
		t.Fatal("write: expected non-nil result for non-dm ThreadID")
	}

	readResult := ResolveThreadConversationForRead(
		context.Background(), cs, logger, "thread-xyz", "proj-1")
	if readResult == nil {
		t.Fatal("read: expected non-nil result — should find the row the write created")
	}

	if readResult.ConversationID != writeResult.ConversationID {
		t.Errorf("ConversationID mismatch: write=%q, read=%q",
			writeResult.ConversationID, readResult.ConversationID)
	}
	if readResult.ExternalRef != writeResult.ExternalRef {
		t.Errorf("ExternalRef mismatch: write=%q, read=%q",
			writeResult.ExternalRef, readResult.ExternalRef)
	}
}

// ---------------------------------------------------------------------------
// Delegation tests — verify distinct log text (Change 5)
// ---------------------------------------------------------------------------

func TestResolveOrCreateThreadConversation_DMKeyRefusalLogsDistinctly(t *testing.T) {
	// A non-canonical dm: key must be logged as "conversation key derivation
	// refused" — distinct from "thread conversation resolution failed" so it's
	// visible on the divergence board.
	mock := &mockConversationUpserter{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Non-canonical: user before agent (canonical order is agent before user).
	nonCanonical := "dm:user:550e8400-e29b-41d4-a716-446655440000:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8"

	got := ResolveOrCreateThreadConversation(context.Background(), mock, logger,
		nonCanonical, "")
	if got != nil {
		t.Errorf("expected nil for non-canonical dm key, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called for refused key")
	}
	output := buf.String()
	if !strings.Contains(output, "conversation key derivation refused") {
		t.Errorf("expected distinct refusal log, got: %s", output)
	}
	if strings.Contains(output, "thread conversation resolution failed") {
		t.Errorf("refusal log should NOT contain the old resolution-failed text, got: %s", output)
	}
}

func TestResolveThreadConversationForRead_DMKeyWithEmptyProjectID(t *testing.T) {
	// dm:-prefixed ThreadIDs should work without projectID — the old code
	// would have returned nil for empty projectID before even trying.
	cs := &mockConversationStore{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	dmKey := "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"

	// Write first.
	writeResult := ResolveOrCreateThreadConversation(
		context.Background(), cs, logger, dmKey, "")
	if writeResult == nil {
		t.Fatal("write: expected non-nil result")
	}

	// Read with empty projectID — should still find the DM.
	readResult := ResolveThreadConversationForRead(
		context.Background(), cs, logger, dmKey, "")
	if readResult == nil {
		t.Fatal("read: expected non-nil result for dm key with empty projectID")
	}
	if readResult.ConversationID != writeResult.ConversationID {
		t.Errorf("ConversationID mismatch: write=%q, read=%q",
			writeResult.ConversationID, readResult.ConversationID)
	}
}
