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
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mockConversationUpserter is a test double for ConversationUpserter,
// ParticipantAdder, and ParticipantEnsurer.
type mockConversationUpserter struct {
	// lastConv captures the most recent conversation passed to Upsert.
	lastConv *store.Conversation
	// returnConv is the conversation returned by Upsert.
	returnConv *store.Conversation
	// returnErr is the error returned by Upsert.
	returnErr error
	// participants captures AddParticipant calls.
	participants []store.ConversationParticipant
	// addPartErr is the injected error for AddParticipant.
	addPartErr error
	// ensuredParticipants captures EnsureParticipant calls (tracked separately).
	ensuredParticipants []store.ConversationParticipant
	// ensurePartErr is the injected error for EnsureParticipant.
	ensurePartErr error
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

func (m *mockConversationUpserter) AddParticipant(_ context.Context, p *store.ConversationParticipant) error {
	if m.addPartErr != nil {
		return m.addPartErr
	}
	m.participants = append(m.participants, *p)
	return nil
}

func (m *mockConversationUpserter) EnsureParticipant(_ context.Context, p *store.ConversationParticipant) error {
	if m.ensurePartErr != nil {
		return m.ensurePartErr
	}
	m.ensuredParticipants = append(m.ensuredParticipants, *p)
	return nil
}

// ---------------------------------------------------------------------------
// ResolveOrCreateDMConversation tests
// ---------------------------------------------------------------------------

func TestResolveOrCreateDMConversation_HappyPath(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-abc", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger, "user", "", "agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
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

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger, "user", "550e8400-e29b-41d4-a716-446655440000", "agent", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
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

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"user", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil on upsert error, got %+v", got)
	}
	if !strings.Contains(err.Error(), "conversation upsert failed") {
		t.Errorf("expected upsert error message, got: %s", err.Error())
	}
}

func TestResolveOrCreateDMConversation_ExternalRefIsKindEncoded(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// Call with user first, agent second — ref should sort to agent:...:user:...
	_, _ = ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
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
	_, _ = ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
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

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"user", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
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

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"bot", "550e8400-e29b-41d4-a716-446655440000",
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil for invalid kind, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called with invalid kind")
	}
	if !strings.Contains(err.Error(), "invalid DM key inputs") {
		t.Errorf("expected error about invalid DM key, got: %s", err.Error())
	}
}

func TestResolveOrCreateDMConversation_RegistersBothParticipants(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-part-test", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if len(mock.ensuredParticipants) != 2 {
		t.Fatalf("expected 2 ensured participants, got %d", len(mock.ensuredParticipants))
	}
	// Sender is registered first.
	if mock.ensuredParticipants[0].PrincipalKind != "agent" || mock.ensuredParticipants[0].PrincipalID != "6ba7b810-9dad-11d1-80b4-00c04fd430c8" {
		t.Errorf("unexpected sender participant: %+v", mock.ensuredParticipants[0])
	}
	if mock.ensuredParticipants[0].Role != "member" {
		t.Errorf("expected role 'member', got %q", mock.ensuredParticipants[0].Role)
	}
	// Recipient is registered second.
	if mock.ensuredParticipants[1].PrincipalKind != "user" || mock.ensuredParticipants[1].PrincipalID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("unexpected recipient participant: %+v", mock.ensuredParticipants[1])
	}
	if mock.ensuredParticipants[1].Role != "member" {
		t.Errorf("expected role 'member', got %q", mock.ensuredParticipants[1].Role)
	}
	// Both point at the same conversation.
	if mock.ensuredParticipants[0].ConversationID != "conv-part-test" || mock.ensuredParticipants[1].ConversationID != "conv-part-test" {
		t.Error("participant conversation IDs should match the resolved conversation")
	}
}

func TestResolveOrCreateDMConversation_ParticipantErrorIsNonFatal(t *testing.T) {
	mock := &mockConversationUpserter{
		returnConv:    &store.Conversation{ID: "conv-err", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
		ensurePartErr: errors.New("db error"),
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result — participant registration failure must not block resolution")
	}
	if got.ConversationID != "conv-err" {
		t.Errorf("expected conv-err, got %q", got.ConversationID)
	}
	output := buf.String()
	if !strings.Contains(output, "participant registration failed") {
		t.Errorf("expected warning log about participant failure, got: %s", output)
	}
}

func TestResolveOrCreateDMConversation_ThirdPartyGuardDocumented(t *testing.T) {
	// The D-1 guard (rejecting a third participant in a direct conversation)
	// is exercised in conversation_store_test.go
	// (TestAddParticipant_DM_ThirdPartyRejection). Here we verify that
	// ResolveOrCreateDMConversation registers exactly the two principals
	// named in the key — no more, no less.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-guard", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	_, _ = ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")

	if len(mock.ensuredParticipants) != 2 {
		t.Fatalf("expected exactly 2 ensured participants from ResolveOrCreateDMConversation, got %d", len(mock.ensuredParticipants))
	}

	// Verify the two principals match the key inputs.
	kinds := map[string]string{
		mock.ensuredParticipants[0].PrincipalID: mock.ensuredParticipants[0].PrincipalKind,
		mock.ensuredParticipants[1].PrincipalID: mock.ensuredParticipants[1].PrincipalKind,
	}
	if kinds["6ba7b810-9dad-11d1-80b4-00c04fd430c8"] != "agent" {
		t.Error("expected agent participant")
	}
	if kinds["550e8400-e29b-41d4-a716-446655440000"] != "user" {
		t.Error("expected user participant")
	}
}

func TestResolveOrCreateDMConversation_IdempotentEnsure(t *testing.T) {
	// EnsureParticipant returns nil for already-existing rows (no
	// ErrAlreadyExists to swallow). The function must succeed silently.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{ID: "conv-idem", ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"},
		// ensurePartErr is nil — simulating the "row already exists" case.
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got, err := ResolveOrCreateDMConversation(context.Background(), mock, mock, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result when EnsureParticipant returns nil")
	}
	if got.ConversationID != "conv-idem" {
		t.Errorf("expected conv-idem, got %q", got.ConversationID)
	}
	output := buf.String()
	if strings.Contains(output, "participant registration failed") {
		t.Errorf("nil return from EnsureParticipant should produce no warning log, got: %s", output)
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

	got, err := ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-123", "proj1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	got, err := ResolveOrCreateThreadConversation(context.Background(), mock, logger, "", "proj1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
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

	got, err := ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-123", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
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

	got, err := ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-123", "proj1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil on upsert error, got %+v", got)
	}
	if !strings.Contains(err.Error(), "conversation upsert failed") {
		t.Errorf("expected upsert error message, got: %s", err.Error())
	}
}

func TestResolveOrCreateThreadConversation_ExternalRefFormat(t *testing.T) {
	mock := &mockConversationUpserter{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	_, _ = ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-ABC", "proj-42")
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

	_, _ = ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-ABC", "proj-42")
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

	_, _ = ResolveOrCreateThreadConversation(context.Background(), mock, logger, "thread-ABC", "proj-42")
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

	writeResult, err := ResolveOrCreateThreadConversation(
		context.Background(), cs, logger, dmKey, "")
	if err != nil {
		t.Fatalf("write: unexpected error: %v", err)
	}
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

	writeResult, err := ResolveOrCreateThreadConversation(
		context.Background(), cs, logger, "thread-xyz", "proj-1")
	if err != nil {
		t.Fatalf("write: unexpected error: %v", err)
	}
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

	got, err := ResolveOrCreateThreadConversation(context.Background(), mock, logger,
		nonCanonical, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil for non-canonical dm key, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert should not have been called for refused key")
	}
	if !strings.Contains(err.Error(), "conversation key derivation refused") {
		t.Errorf("expected distinct refusal error, got: %s", err.Error())
	}
}

// ---------------------------------------------------------------------------
// B7 nil-pe guard test
// ---------------------------------------------------------------------------

func TestResolveOrCreateDMConversation_NilParticipantEnsurer(t *testing.T) {
	// B7: a nil ParticipantEnsurer must not panic. The function advertises
	// non-fatal semantics for participant registration, and a nil pe that
	// panics violates that contract. Assert: returns non-nil ConversationResult
	// AND no panic.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{
			ID:          "conv-nil-pe",
			ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000",
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Pass nil as pe — must not panic.
	got, err := ResolveOrCreateDMConversation(context.Background(), mock, nil, logger,
		"agent", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"user", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil ConversationResult with nil pe — conversation itself resolved")
	}
	if got.ConversationID != "conv-nil-pe" {
		t.Errorf("expected ConversationID conv-nil-pe, got %q", got.ConversationID)
	}
	if got.ExternalRef != "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("expected ExternalRef from mock, got %q", got.ExternalRef)
	}
	output := buf.String()
	if !strings.Contains(output, "nil ParticipantEnsurer") {
		t.Errorf("expected warning log about nil ParticipantEnsurer, got: %s", output)
	}
}

func TestResolveThreadConversationForRead_DMKeyWithEmptyProjectID(t *testing.T) {
	// dm:-prefixed ThreadIDs should work without projectID — the old code
	// would have returned nil for empty projectID before even trying.
	cs := &mockConversationStore{}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	dmKey := "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"

	// Write first.
	writeResult, err := ResolveOrCreateThreadConversation(
		context.Background(), cs, logger, dmKey, "")
	if err != nil {
		t.Fatalf("write: unexpected error: %v", err)
	}
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

// ---------------------------------------------------------------------------
// mockTopicLookup — test double for TopicConversationLookup
// ---------------------------------------------------------------------------

type mockTopicLookup struct {
	// topics maps topicID -> conversationID. Empty string means topic exists
	// but has no conversation_id. Missing key means topic not found.
	topics map[string]string

	// deleted is the set of topicIDs that are soft-deleted. A topic in
	// deleted must also have an entry in topics (it still exists, just
	// tombstoned). GetTopicConversationID returns ErrNotFound for these;
	// GetTopicConversationIDIncludingDeleted returns the conversation_id.
	deleted map[string]bool

	// calledMethod records which method was last called, so tests can
	// verify the sink calls the correct accessor.
	calledMethod string
}

func (m *mockTopicLookup) GetTopicConversationID(_ context.Context, topicID string) (string, error) {
	m.calledMethod = "GetTopicConversationID"
	// User-facing: hides soft-deleted topics.
	if m.deleted[topicID] {
		return "", fmt.Errorf("topic not found (deleted) %s: %w", topicID, store.ErrNotFound)
	}
	convID, ok := m.topics[topicID]
	if !ok {
		return "", fmt.Errorf("topic not found %s: %w", topicID, store.ErrNotFound)
	}
	return convID, nil
}

func (m *mockTopicLookup) GetTopicConversationIDIncludingDeleted(_ context.Context, topicID string) (string, error) {
	m.calledMethod = "GetTopicConversationIDIncludingDeleted"
	// Mint guard: sees soft-deleted topics.
	convID, ok := m.topics[topicID]
	if !ok {
		return "", fmt.Errorf("topic not found %s: %w", topicID, store.ErrNotFound)
	}
	return convID, nil
}

// mockTopicLookupWithError returns a configurable error for any topicID.
type mockTopicLookupWithError struct {
	err error
}

func (m *mockTopicLookupWithError) GetTopicConversationID(_ context.Context, _ string) (string, error) {
	return "", m.err
}

func (m *mockTopicLookupWithError) GetTopicConversationIDIncludingDeleted(_ context.Context, _ string) (string, error) {
	return "", m.err
}

// ---------------------------------------------------------------------------
// AC-U-3: The message path NEVER mints a surface=native conversation for a
// thread ID that has no topic row (with topic lookup enabled).
// ---------------------------------------------------------------------------

func TestAC_U3_NoMintForNativeTopicWithoutRow(t *testing.T) {
	// Scenario: threadID is a topic UUID that does NOT exist in webchat_topic.
	// With the sink-level guard (DEF-20 unify), store.ErrNotFound means "not a
	// native topic" and falls through to upsert. This is correct because the
	// lookup is now injected unconditionally (not just for web channels), so
	// non-native surface threads (Discord, Telegram) will naturally get
	// ErrNotFound and should proceed to upsert.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{
			ID:          "conv-minted",
			ExternalRef: "thread:proj-1:topic-uuid-nonexistent",
		},
	}
	lookup := &mockTopicLookup{topics: map[string]string{}} // empty = no topics

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// Thread ID that looks like a topic UUID but doesn't exist.
	got, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger,
		"topic-uuid-nonexistent", "proj-1",
		WithTopicLookup(lookup))

	// When topic lookup returns store.ErrNotFound, the sink falls through to
	// upsert — this is the expected behavior for non-native surface threads.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result — ErrNotFound should fall through to upsert")
	}
	if got.ConversationID != "conv-minted" {
		t.Errorf("ConversationID: got %q, want %q", got.ConversationID, "conv-minted")
	}
	if mock.lastConv == nil {
		t.Error("upsert must be called when topic is not found (ErrNotFound)")
	}
}

func TestAC_U3_NoMintForTopicWithoutConversationID(t *testing.T) {
	// Scenario: threadID is a webchat topic UUID that EXISTS but has no
	// conversation_id yet (not yet backfilled). The function must return nil
	// (unresolved) and MUST NOT mint a new conversation row.
	mock := &mockConversationUpserter{}
	lookup := &mockTopicLookup{
		topics: map[string]string{
			"topic-no-conv": "", // exists but no conversation_id
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger,
		"topic-no-conv", "proj-1",
		WithTopicLookup(lookup))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil for topic without conversation_id, got %+v", got)
	}
	if mock.lastConv != nil {
		t.Error("upsert MUST NOT be called — no conversation row should be minted")
	}
	if !strings.Contains(err.Error(), "topic has no conversation_id") {
		t.Errorf("expected error about missing conversation_id, got: %s", err.Error())
	}
}

func TestAC_U3_ResolveViaTopicLookup(t *testing.T) {
	// Scenario: threadID is a webchat topic UUID that EXISTS and HAS a
	// conversation_id. The function must return the linked conversation_id
	// without calling the upserter.
	mock := &mockConversationUpserter{}
	lookup := &mockTopicLookup{
		topics: map[string]string{
			"topic-with-conv": "conv-linked-123",
		},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger,
		"topic-with-conv", "proj-1",
		WithTopicLookup(lookup))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result for topic with conversation_id")
	}
	if got.ConversationID != "conv-linked-123" {
		t.Errorf("expected conversation_id conv-linked-123, got %q", got.ConversationID)
	}
	if mock.lastConv != nil {
		t.Error("upsert MUST NOT be called when topic lookup succeeds")
	}
}

func TestAC_U3_DMPrefixFallsThrough(t *testing.T) {
	// Scenario: threadID is a dm:-prefixed key. Even with topic lookup enabled,
	// dm: keys should fall through to the existing DeriveConversationKey path
	// because they are not native topics.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{
			ID:          "conv-dm-fallthrough",
			ExternalRef: "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000",
		},
	}
	lookup := &mockTopicLookup{topics: map[string]string{}} // no topics
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	dmKey := "dm:agent:6ba7b810-9dad-11d1-80b4-00c04fd430c8:user:550e8400-e29b-41d4-a716-446655440000"
	got, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger,
		dmKey, "",
		WithTopicLookup(lookup))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result — dm: keys must fall through to upsert")
	}
	if got.ConversationID != "conv-dm-fallthrough" {
		t.Errorf("expected conv-dm-fallthrough, got %q", got.ConversationID)
	}
}

func TestAC_U3_WithoutTopicLookup_StillMints(t *testing.T) {
	// Backwards compatibility: without the WithTopicLookup option,
	// the function must still mint conversations as before.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{
			ID:          "conv-minted",
			ExternalRef: "thread:proj-1:topic-uuid",
		},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	got, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger,
		"topic-uuid", "proj-1") // no WithTopicLookup

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result — without lookup, must mint as before")
	}
	if got.ConversationID != "conv-minted" {
		t.Errorf("expected conv-minted, got %q", got.ConversationID)
	}
	if mock.lastConv == nil {
		t.Error("upsert must have been called (backwards compat)")
	}
}

// ---------------------------------------------------------------------------
// DEF-21 regression: infrastructure error must NOT fall through to upsert
// ---------------------------------------------------------------------------

func TestDEF21_InfraErrorMustNotMint(t *testing.T) {
	// DEF-21: When GetTopicConversationID returns an infrastructure error
	// (e.g. DB connection lost — NOT store.ErrNotFound), the function must
	// NOT fall through to the upsert path and mint a spurious conversation.
	// It must return nil (non-fatal contract) without calling the upserter.
	mock := &mockConversationUpserter{
		returnConv: &store.Conversation{
			ID:          "conv-spurious",
			ExternalRef: "thread:proj-1:some-topic",
		},
	}
	lookup := &mockTopicLookupWithError{
		err: errors.New("connection refused"), // infra error, NOT ErrNotFound
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got, err := ResolveOrCreateThreadConversation(
		context.Background(), mock, logger,
		"some-topic", "proj-1",
		WithTopicLookup(lookup))

	// After the DEF-21 fix:
	// - err must be non-nil (infra error propagated)
	// - got must be nil (no spurious conversation minted)
	// - mock.lastConv must be nil (upserter must NOT be called)
	if err == nil {
		t.Fatal("DEF-21: expected error on infra error, got nil")
	}
	if got != nil {
		t.Errorf("DEF-21: expected nil on infra error, got %+v (spurious mint!)", got)
	}
	if mock.lastConv != nil {
		t.Errorf("DEF-21: upserter was called on infra error — spurious conversation minted")
	}
}
