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

package hub

// DEF-100 regression tests: production-writer → read-switch integration.
//
// These tests exercise the full producer/consumer key contract: the topic
// is created through the real CreateTopic path (production writer), which
// writes external_ref = '' on the conversations row and stores conversation_id
// on the webchat_topic row. The read path must resolve via the topic lookup
// intercept, not via external_ref.
//
// A test that seeds its own conversation row with a well-formed external_ref
// (like the pre-DEF-100 tests) cannot detect the mismatch that caused every
// native web thread to return 409.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// DEF-100 T1 — Production writer → read resolver integration
//
// Creates a topic via the real CreateTopic path, then resolves it through
// ResolveThreadConversationForRead with WithReadTopicLookup. This is the
// minimal unit that proves the fix: the resolver finds the conversation_id
// via the topic lookup even though external_ref is empty.
// ---------------------------------------------------------------------------

func TestDEF100_T1_ProductionWriter_ReadResolver(t *testing.T) {
	wcs, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	topicID := uuid.New().String()
	projectID := "proj-def100-t1"

	// Step 1: create topic via the production writer. This auto-generates a
	// conversation_id and writes external_ref = '' on the conversations row.
	err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: projectID,
		Name:      "DEF-100 test topic",
		CreatedBy: "test-user",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Verify preconditions: topic has a conversation_id, conversation row
	// has external_ref = ''.
	convID := getTopicConvID(t, db, topicID)
	if convID == "" {
		t.Fatal("precondition: topic should have auto-generated conversation_id")
	}

	var extRef string
	err = db.QueryRow("SELECT external_ref FROM conversations WHERE id = ?", convID).Scan(&extRef)
	if err != nil {
		t.Fatalf("precondition: conversations row query: %v", err)
	}
	if extRef != "" {
		t.Fatalf("precondition: expected external_ref='', got %q — "+
			"the production writer is supposed to write empty external_ref for native topics", extRef)
	}

	// Step 2: resolve via the read path WITH topic lookup.
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// The ConversationReader is not used when topic lookup succeeds, but
	// we still need to pass one. Use a nil-safe stub.
	cr := &stubConversationReader{}

	result := messaging.ResolveThreadConversationForRead(
		ctx, cr, logger,
		topicID, projectID,
		messaging.WithReadTopicLookup(wcs))

	if result == nil {
		t.Fatal("DEF-100: ResolveThreadConversationForRead returned nil — " +
			"the topic lookup intercept is not working")
	}
	if result.ConversationID != convID {
		t.Errorf("DEF-100: expected conversation_id %q, got %q",
			convID, result.ConversationID)
	}

	// Step 3: verify the OLD path (without topic lookup) would FAIL.
	// This proves the test is not vacuous.
	resultOld := messaging.ResolveThreadConversationForRead(
		ctx, cr, logger,
		topicID, projectID) // no WithReadTopicLookup

	if resultOld != nil {
		t.Errorf("DEF-100 control: without topic lookup, the resolver should "+
			"return nil (external_ref is empty), got %+v — this means the test "+
			"is vacuous or external_ref was unexpectedly populated", resultOld)
	}
}

// stubConversationReader is a ConversationReader that always returns nil.
// Used when the topic lookup intercept is expected to resolve without
// falling through to the external_ref path.
type stubConversationReader struct{}

func (s *stubConversationReader) GetConversationByExternalRef(_ context.Context, _, _ string) (*store.Conversation, error) {
	return nil, fmt.Errorf("no conversation found (stub)")
}

// ---------------------------------------------------------------------------
// DEF-100 T2 — Full HTTP handler integration (S1 site)
//
// Creates a topic via the real CreateTopic path, seeds a message with the
// topic's conversation_id, enables the read switch, and hits the S1 endpoint.
// Asserts 200 and that the message is returned.
// ---------------------------------------------------------------------------

func TestDEF100_T2_S1_ProductionWriter_HTTPEndpoint(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	ctx := context.Background()
	projectID := rsProject(t, s, "def100-t2-project")

	// Create a real webchat store backed by an in-memory SQLite DB.
	wcs, wcsDB := newTestWebChatStoreWithConversations(t)
	defer wcsDB.Close() //nolint:errcheck
	srv.SetWebChatStore(wcs)

	topicID := uuid.New().String()
	err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: projectID,
		Name:      "DEF-100 HTTP test",
		CreatedBy: DevUserID,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Get the auto-generated conversation_id.
	convID := getTopicConvID(t, wcsDB, topicID)
	if convID == "" {
		t.Fatal("precondition: topic should have auto-generated conversation_id")
	}

	// Seed a message in the main store with the topic's conversation_id.
	agentID := rsAgent(t, s, "def100-t2-agent", projectID)
	msg := &store.Message{
		ID:             tid("def100-t2-msg"),
		ProjectID:      projectID,
		Sender:         "agent:" + agentID,
		SenderID:       agentID,
		Recipient:      "user:" + DevUserID,
		RecipientID:    DevUserID,
		AgentID:        agentID,
		Msg:            "DEF-100 regression test message",
		Type:           "output",
		Channel:        "web",
		ThreadID:       topicID,
		ConversationID: convID,
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// Hit the S1 endpoint with the read switch ON.
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/chat/conversations/"+topicID+"/messages", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("DEF-100: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the message is in the response.
	var histResp chatHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &histResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	found := false
	for _, m := range histResp.Messages {
		if m.ID == msg.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DEF-100: expected to find message %s in response, got %d messages",
			msg.ID, len(histResp.Messages))
	}
}

// ---------------------------------------------------------------------------
// DEF-100 T3 — Backfilled topic shape
//
// Simulates a topic created before DEF-89 (no auto-generated conversation_id)
// that was later backfilled. After backfill, the topic has a conversation_id
// and the conversations row has external_ref = ''. The read path must still
// resolve via topic lookup.
// ---------------------------------------------------------------------------

func TestDEF100_T3_BackfilledTopic(t *testing.T) {
	wcs, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	topicID := uuid.New().String()
	projectID := "proj-def100-t3"

	// Step 1: create a topic the pre-DEF-89 way — no conversations table
	// initially, so the topic is created without a conversation_id.
	// We simulate this by directly inserting a topic without conversation_id.
	_, err := db.ExecContext(ctx,
		`INSERT INTO webchat_topic (id, project_id, name, is_general, created_by, created_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		topicID, projectID, "Backfilled topic", "test-user",
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insert legacy topic: %v", err)
	}

	// Step 2: simulate backfill — create a conversation row and link it.
	backfilledConvID := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.ExecContext(ctx,
		`INSERT INTO conversations (id, project_id, kind, surface, external_ref, parent_ref, display_name, drift_state, last_activity_at, created_at)
		 VALUES (?, ?, 'group', 'native', '', '', ?, 'active', ?, ?)`,
		backfilledConvID, projectID, "Backfilled topic", now, now)
	if err != nil {
		t.Fatalf("insert backfilled conversation: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`UPDATE webchat_topic SET conversation_id = ? WHERE id = ?`,
		backfilledConvID, topicID)
	if err != nil {
		t.Fatalf("link backfilled conversation to topic: %v", err)
	}

	// Step 3: resolve via the read path with topic lookup.
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	cr := &stubConversationReader{}

	result := messaging.ResolveThreadConversationForRead(
		ctx, cr, logger,
		topicID, projectID,
		messaging.WithReadTopicLookup(wcs))

	if result == nil {
		t.Fatal("DEF-100 T3: ResolveThreadConversationForRead returned nil for backfilled topic")
	}
	if result.ConversationID != backfilledConvID {
		t.Errorf("DEF-100 T3: expected conversation_id %q, got %q",
			backfilledConvID, result.ConversationID)
	}
}

// ---------------------------------------------------------------------------
// DEF-100 T4 — Non-native thread resolves via external_ref
//
// A non-native surface thread (e.g. Discord) is NOT a webchat topic — the
// topic lookup returns store.ErrNotFound and the resolver falls through to
// the external_ref lookup. The 2 live rows with non-empty external_ref prove
// this path is real and working.
// ---------------------------------------------------------------------------

func TestDEF100_T4_NonNativeThread_ExternalRef(t *testing.T) {
	wcs, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	threadID := "discord-thread-" + uuid.New().String()
	projectID := "proj-def100-t4"

	// Seed a non-native conversation with a well-formed external_ref.
	// This simulates a Discord/Telegram thread that went through the write
	// path's external_ref upsert (not the topic lookup).
	extRef := fmt.Sprintf("thread:%s:%s", projectID, threadID)
	convID := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx,
		`INSERT INTO conversations (id, project_id, kind, surface, external_ref, parent_ref, display_name, drift_state, last_activity_at, created_at)
		 VALUES (?, ?, 'group', 'native', ?, '', '', 'active', ?, ?)`,
		convID, projectID, extRef, now, now)
	if err != nil {
		t.Fatalf("seed non-native conversation: %v", err)
	}

	// Use a ConversationReader that queries the real conversations table.
	cr := &sqliteConversationReader{db: db}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// Resolve with topic lookup enabled — the thread is NOT a topic, so
	// GetTopicConversationIDIncludingDeleted returns ErrNotFound and the
	// resolver falls through to external_ref lookup.
	result := messaging.ResolveThreadConversationForRead(
		ctx, cr, logger,
		threadID, projectID,
		messaging.WithReadTopicLookup(wcs))

	if result == nil {
		t.Fatal("DEF-100 T4: non-native thread should resolve via external_ref fallthrough")
	}
	if result.ConversationID != convID {
		t.Errorf("DEF-100 T4: expected conversation_id %q, got %q",
			convID, result.ConversationID)
	}
}

// ---------------------------------------------------------------------------
// DEF-100 T5 — Full HTTP handler integration (S3 site / handleAgentMessages)
//
// The S3 call site (handlers_messages.go:306) had NO topic lookup at all
// before this fix. This test proves it now resolves native topics correctly.
// ---------------------------------------------------------------------------

func TestDEF100_T5_S3_ProductionWriter_HTTPEndpoint(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	ctx := context.Background()
	projectID := rsProject(t, s, "def100-t5-project")
	agentID := rsAgent(t, s, "def100-t5-agent", projectID)

	// Create a real webchat store.
	wcs, wcsDB := newTestWebChatStoreWithConversations(t)
	defer wcsDB.Close() //nolint:errcheck
	srv.SetWebChatStore(wcs)

	topicID := uuid.New().String()
	err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: projectID,
		Name:      "DEF-100 S3 test",
		CreatedBy: DevUserID,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	convID := getTopicConvID(t, wcsDB, topicID)
	if convID == "" {
		t.Fatal("precondition: topic should have auto-generated conversation_id")
	}

	// Seed a message.
	msg := &store.Message{
		ID:             tid("def100-t5-msg"),
		ProjectID:      projectID,
		Sender:         "agent:" + agentID,
		SenderID:       agentID,
		Recipient:      "user:" + DevUserID,
		RecipientID:    DevUserID,
		AgentID:        agentID,
		Msg:            "DEF-100 S3 regression test message",
		Type:           "output",
		Channel:        "web",
		ThreadID:       topicID,
		ConversationID: convID,
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// Hit the S3 endpoint with thread_id.
	url := fmt.Sprintf("/api/v1/agents/%s/messages?thread_id=%s", agentID, topicID)
	rec := doRequest(t, srv, http.MethodGet, url, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("DEF-100 S3: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result store.ListResult[store.Message]
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	found := false
	for _, m := range result.Items {
		if m.ID == msg.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DEF-100 S3: expected to find message %s in response, got %d items",
			msg.ID, len(result.Items))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// sqliteConversationReader implements messaging.ConversationReader by querying
// the conversations table directly. Used in DEF-100 tests where the fixture
// is created in an in-memory SQLite DB that isn't part of the main Ent store.
type sqliteConversationReader struct {
	db *sql.DB
}

func (r *sqliteConversationReader) GetConversationByExternalRef(_ context.Context, surface, externalRef string) (*store.Conversation, error) {
	var conv store.Conversation
	err := r.db.QueryRow(
		`SELECT id, COALESCE(external_ref,''), COALESCE(kind,''), COALESCE(surface,'')
		   FROM conversations WHERE surface = ? AND external_ref = ?`,
		surface, externalRef).
		Scan(&conv.ID, &conv.ExternalRef, &conv.Kind, &conv.Surface)
	if err != nil {
		return nil, fmt.Errorf("conversation not found: %w", err)
	}
	return &conv, nil
}
