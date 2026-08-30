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

// Tests for the Phase 8 conversation read-switch across three sites:
//
//   S1 — handleConversationHistory  (GET /api/v1/chat/conversations/{key}/messages)
//   S2 — handleMessages             (GET /api/v1/messages?agent={id})
//   S3 — handleAgentMessages        (GET /api/v1/agents/{id}/messages)
//
// All divergence counter assertions are delta-based: we capture
// messaging.DivergenceMetrics.Fallbacks() before and after each test handler
// invocation, then assert on the difference. This avoids coupling to other
// writers of the global counter.
//
// These tests are NOT safe for t.Parallel() against each other — the
// divergence counter is a package-level atomic with no reset and multiple
// concurrent writers would invalidate delta assertions.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// enableReadSwitch configures OperationalSettings on the server with the
// ConversationReadSwitch flag ON. After this call, handlers that check
// s.GetOperationalSettings().ConversationReadSwitch() will enter the
// Phase 8 conversation-resolution branch.
func enableReadSwitch(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_read_switch":true}`))
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("ops.Refresh failed: %v", err)
	}
	srv.SetOperationalSettings(ops)
	// Canary: verify the switch is actually on. Without this, a silent
	// failure in enableReadSwitch makes every delta==0 assertion pass
	// trivially — the handler never enters the read-switch block at all.
	if !srv.GetOperationalSettings().ConversationReadSwitch() {
		t.Fatalf("enableReadSwitch: ConversationReadSwitch() is still false after setup — " +
			"every FlagOn test in this file is vacuous without this guard")
	}
}

// seedConversation creates a conversation in the store with the given
// surface and externalRef, returning its auto-assigned ID. The caller uses
// this to set up the "conversation resolves" precondition.
func seedConversation(t *testing.T, s store.Store, surface, externalRef, kind string) string {
	t.Helper()
	conv := &store.Conversation{
		Surface:     surface,
		ExternalRef: externalRef,
		Kind:        kind,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(context.Background(), conv)
	if err != nil {
		t.Fatalf("seedConversation(%s, %s): %v", surface, externalRef, err)
	}
	return created.ID
}

// rsAgent creates an agent in the store with the given name and projectID.
// Prefixed "rs" (read-switch) to avoid collision with seedAgent in other
// test files in the same package.
func rsAgent(t *testing.T, s store.Store, agentName, projectID string) string {
	t.Helper()
	agentID := tid(agentName)
	agent := &store.Agent{
		ID:        agentID,
		Slug:      agentName,
		Name:      agentName,
		ProjectID: projectID,
	}
	if err := s.CreateAgent(context.Background(), agent); err != nil {
		t.Fatalf("rsAgent(%s): %v", agentName, err)
	}
	return agentID
}

// rsProject creates a project in the store with required fields populated.
func rsProject(t *testing.T, s store.Store, projectName string) string {
	t.Helper()
	projectID := tid(projectName)
	project := &store.Project{
		ID:      projectID,
		Name:    projectName,
		Slug:    projectName,
		OwnerID: DevUserID,
	}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("rsProject(%s): %v", projectName, err)
	}
	return projectID
}

// fallbackDelta captures the divergence fallback counter before calling fn,
// then returns the delta after fn completes. This is the only safe way to
// assert on the global counter given concurrent writers.
func fallbackDelta(fn func()) int64 {
	before := messaging.DivergenceMetrics.Fallbacks()
	fn()
	return messaging.DivergenceMetrics.Fallbacks() - before
}

// rsWebChatStore is a minimal WebChatStore that returns a fixed topic for
// GetTopic and no-ops everything else. The fixture is load-bearing for S1
// thread tests: without it, webChatStore is nil and the flag-ON thread
// branch is never entered, producing a false-green test.
type rsWebChatStore struct {
	topics map[string]*WebChatTopic
}

func (s *rsWebChatStore) GetTopic(_ context.Context, topicID string) (*WebChatTopic, error) {
	if t, ok := s.topics[topicID]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("topic not found: %s", topicID)
}

// Stubs for the remaining WebChatStore interface methods — none are called
// by the read-switch code path under test.
func (s *rsWebChatStore) Init() error { return nil }
func (s *rsWebChatStore) TouchThread(_ context.Context, _, _, _, _ string, _ time.Time) error {
	return nil
}
func (s *rsWebChatStore) RecordChannel(_ context.Context, _, _, _, _ string, _ time.Time) error {
	return nil
}
func (s *rsWebChatStore) GetLastChannel(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (s *rsWebChatStore) GetThreadPrefs(context.Context, string, string, string) (ThreadPrefs, error) {
	return ThreadPrefs{}, nil
}
func (s *rsWebChatStore) SetThreadPrefs(context.Context, string, string, string, ThreadPrefs) error {
	return nil
}
func (s *rsWebChatStore) GetThreads(context.Context, string, string, int) ([]WebChatThread, error) {
	return nil, nil
}
func (s *rsWebChatStore) MarkThreadRead(context.Context, string, string, string) error { return nil }
func (s *rsWebChatStore) GetTopicConversationID(context.Context, string) (string, error) {
	return "", nil
}
func (s *rsWebChatStore) GetTopicConversationIDIncludingDeleted(context.Context, string) (string, error) {
	return "", nil
}
func (s *rsWebChatStore) CreateTopic(context.Context, WebChatTopic) error { return nil }
func (s *rsWebChatStore) ListTopics(context.Context, string) ([]WebChatTopic, error) {
	return nil, nil
}
func (s *rsWebChatStore) UpdateTopic(context.Context, string, TopicUpdate) error { return nil }
func (s *rsWebChatStore) DeleteTopic(context.Context, string) error              { return nil }
func (s *rsWebChatStore) TouchTopicActivity(_ context.Context, _, _ string) error {
	return nil
}
func (s *rsWebChatStore) EnsureGeneralTopic(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (s *rsWebChatStore) GetReadState(context.Context, string, string) (*WebChatReadState, error) {
	return nil, nil
}
func (s *rsWebChatStore) SetReadState(context.Context, string, string, string) error { return nil }
func (s *rsWebChatStore) GetReadStates(context.Context, string, []string) ([]WebChatReadState, error) {
	return nil, nil
}
func (s *rsWebChatStore) SetPinned(context.Context, string, string, bool) error { return nil }
func (s *rsWebChatStore) SetMuted(context.Context, string, string, bool) error  { return nil }
func (s *rsWebChatStore) IsConversationMuted(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *rsWebChatStore) GetUserPrefs(context.Context, string) (*WebChatUserPrefs, error) {
	return nil, nil
}
func (s *rsWebChatStore) SetUserPrefs(context.Context, string, WebChatUserPrefs) error { return nil }
func (s *rsWebChatStore) UpsertDM(context.Context, WebChatDM) error                    { return nil }
func (s *rsWebChatStore) ListDMs(context.Context, string) ([]WebChatDM, error)         { return nil, nil }
func (s *rsWebChatStore) TouchDMActivity(context.Context, string, string) error        { return nil }
func (s *rsWebChatStore) SearchChatMessages(context.Context, ChatSearchFilter) ([]ChatSearchResult, string, error) {
	return nil, "", nil
}
func (s *rsWebChatStore) CreateAttachment(context.Context, AttachmentMeta) error { return nil }
func (s *rsWebChatStore) GetAttachment(context.Context, string) (*AttachmentMeta, error) {
	return nil, nil
}
func (s *rsWebChatStore) DeleteAttachment(context.Context, string) error { return nil }
func (s *rsWebChatStore) GetAttachmentsByMessage(context.Context, string) ([]AttachmentMeta, error) {
	return nil, nil
}
func (s *rsWebChatStore) GetAttachmentsByMessages(context.Context, []string) (map[string][]AttachmentMeta, error) {
	return nil, nil
}
func (s *rsWebChatStore) LinkAttachmentToMessage(context.Context, string, string) error { return nil }
func (s *rsWebChatStore) SetMessageReplyTo(context.Context, string, string) error       { return nil }
func (s *rsWebChatStore) GetMessageExt(context.Context, string) (*WebChatMessageExt, error) {
	return nil, nil
}
func (s *rsWebChatStore) GetMessageExts(context.Context, []string) (map[string]*WebChatMessageExt, error) {
	return nil, nil
}
func (s *rsWebChatStore) SetMessageEdited(_ context.Context, _ string, _ time.Time) error {
	return nil
}
func (s *rsWebChatStore) SetMessageDeleted(_ context.Context, _ string, _ time.Time) error {
	return nil
}
func (s *rsWebChatStore) UpdateMessageContent(context.Context, string, string) error { return nil }
func (s *rsWebChatStore) PromoteDM(context.Context, WebChatTopic, string) (*WebChatTopic, error) {
	return nil, nil
}
func (s *rsWebChatStore) UpdateThreadID(context.Context, string, string) (int, error) { return 0, nil }
func (s *rsWebChatStore) DeleteDM(context.Context, string) error                      { return nil }
func (s *rsWebChatStore) MigrateReadState(context.Context, string, string) error      { return nil }
func (s *rsWebChatStore) CountPendingMessages(context.Context, string) (int, error)   { return 0, nil }
func (s *rsWebChatStore) CountMessages(context.Context, string) (int, error)          { return 0, nil }

// ==========================================================================
// S1 — handleConversationHistory
// Route: GET /api/v1/chat/conversations/{key}/messages
// ==========================================================================

// makeDMKey builds a valid 5-part DM key for the dev user and the given agent UUID.
// Format: dm:agent:<agentUUID>:user:<userUUID> (sorted lexicographically by token).
func makeDMKey(agentUUID, userUUID string) string {
	// "agent:" < "user:" lexicographically, so agent token comes first.
	return fmt.Sprintf("dm:agent:%s:user:%s", agentUUID, userUUID)
}

func TestReadSwitch_S1_DM_FlagOff(t *testing.T) {
	srv, _ := testServer(t)
	// No OperationalSettings → flag OFF (nil ops → legacy path).
	agentUUID := tid("s1-agent-off")
	key := makeDMKey(agentUUID, DevUserID)

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag OFF: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S1_DM_FlagOn_ConversationResolved(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	agentUUID := tid("s1-agent-resolved")
	key := makeDMKey(agentUUID, DevUserID)

	// Seed a conversation that ResolveDMConversationForRead will find.
	// DMConversationKey(agent, agentUUID, user, DevUserID) produces the external ref.
	seedConversation(t, s, "native", key, "direct")

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag ON + resolved: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S1_DM_FlagOn_ConversationNotFound(t *testing.T) {
	// G3: with fallback removed, an unresolvable conversation returns 409
	// with code "conversation_not_resolved" instead of falling back to the
	// legacy channel+thread filter. (AC-G3-2)
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	agentUUID := tid("s1-agent-notfound")
	key := makeDMKey(agentUUID, DevUserID)
	// No conversation seeded → resolve returns nil → typed error.

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeConversationNotResolved {
		t.Errorf("expected error code %q, got %q", ErrCodeConversationNotResolved, errResp.Error.Code)
	}
}

func TestReadSwitch_S1_DM_SevenPartKey_FlagOn(t *testing.T) {
	// A 7-part DM key must NOT derive a conversation from its first 5.
	// validDMKey's regex rejects keys with more than 5 colon-separated parts
	// (the $ anchor enforces exactly 5), so the handler returns 400 before
	// reaching the read-switch block.
	//
	// This is an access-control invariant: after the read-switch the DM key
	// IS the ACL, so a tolerant parse that silently drops trailing parts
	// would be a security defect (see handlers_chat_v2.go ~1786-1788).
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	agentUUID := tid("s1-agent-7part")
	// Build a 7-part key: dm:agent:<uuid>:user:<uuid>:extra:data
	key := makeDMKey(agentUUID, DevUserID) + ":extra:data"

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	// validDMKey rejects the 7-part key → 400 Bad Request.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReadSwitch_S1_Thread_FlagOff(t *testing.T) {
	srv, s := testServer(t)
	// No OperationalSettings → flag OFF.

	projectID := rsProject(t, s, "s1-thread-off-project")
	threadKey := "thread-key-off-" + tid("s1-thread-off")

	// Set up webChatStore with a topic so the handler can look it up for authz.
	wcs := &rsWebChatStore{topics: map[string]*WebChatTopic{
		threadKey: {ID: threadKey, ProjectID: projectID, Name: "test-thread"},
	}}
	srv.SetWebChatStore(wcs)

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+threadKey+"/messages", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag OFF thread: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S1_Thread_FlagOn_ConversationResolved(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s1-thread-resolved-project")
	threadKey := "thread-key-resolved-" + tid("s1-thread-resolved")

	// webChatStore fixture is load-bearing: without it the flag-ON thread
	// branch is never entered (wcs == nil guard), producing a false-green.
	wcs := &rsWebChatStore{topics: map[string]*WebChatTopic{
		threadKey: {ID: threadKey, ProjectID: projectID, Name: "test-thread"},
	}}
	srv.SetWebChatStore(wcs)

	// Seed the thread conversation. ResolveThreadConversationForRead uses
	// DeriveConversationKey({ThreadID: threadKey, ProjectID: projectID})
	// which produces "thread:{projectID}:{threadKey}".
	extRef := fmt.Sprintf("thread:%s:%s", projectID, threadKey)
	seedConversation(t, s, "native", extRef, "group")

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+threadKey+"/messages", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag ON thread resolved: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S1_Thread_FlagOn_ConversationNotFound(t *testing.T) {
	// G3: with fallback removed, an unresolvable thread conversation returns
	// 409 with code "conversation_not_resolved". (AC-G3-2)
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s1-thread-notfound-project")
	threadKey := "thread-key-notfound-" + tid("s1-thread-notfound")

	// webChatStore fixture is load-bearing: see TestReadSwitch_S1_Thread_FlagOn_WcsNil.
	wcs := &rsWebChatStore{topics: map[string]*WebChatTopic{
		threadKey: {ID: threadKey, ProjectID: projectID, Name: "test-thread"},
	}}
	srv.SetWebChatStore(wcs)
	// No conversation seeded → resolve returns nil → typed error.

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+threadKey+"/messages", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeConversationNotResolved {
		t.Errorf("expected error code %q, got %q", ErrCodeConversationNotResolved, errResp.Error.Code)
	}
}

func TestReadSwitch_S1_Thread_FlagOn_WcsNil(t *testing.T) {
	// When webChatStore is nil, the thread branch at S1 ~1793 is never
	// entered: the `if wcs != nil` guard fails and convResult stays nil.
	// The handler falls back to the legacy filter (Channel="web",
	// ThreadID=key) and calls IncFallback().
	//
	// This is the same "skip without IncFallback" shape as S2's R-9
	// discipline, except S2 documents it as intentional and S1 is silent.
	//
	// FIXTURE NOTE: webChatStore is load-bearing for flag-ON thread tests.
	// Without it, every flag-ON thread test silently takes the legacy path
	// and appears green without ever exercising the conversation-resolve
	// branch. Deleting a webChatStore fixture that "looks inert" will rot
	// the test from the inside.
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)
	// Deliberately do NOT set webChatStore.

	threadKey := "thread-key-wcsnil-" + tid("s1-thread-wcsnil")

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+threadKey+"/messages", nil)
		// Without webChatStore, the handler checks `if wcs == nil` for non-DM
		// keys and returns an empty result (200) before reaching the read-switch.
		// Check the actual handler behaviour.
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// The handler returns early with an empty chatHistoryResponse at ~1752
	// when wcs is nil for a non-DM key (before reaching the read-switch
	// block at ~1779). Therefore no IncFallback is called.
	if delta != 0 {
		t.Errorf("wcs nil: expected fallback delta 0, got %d", delta)
	}
}

// ==========================================================================
// S2 — handleMessages
// Route: GET /api/v1/messages?agent={id}
// ==========================================================================

func TestReadSwitch_S2_FlagOff(t *testing.T) {
	srv, s := testServer(t)
	// No OperationalSettings → flag OFF.
	projectID := rsProject(t, s, "s2-off-project")
	agentID := rsAgent(t, s, "s2-agent-off", projectID)

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent="+agentID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag OFF: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S2_FlagOn_ConversationResolved(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s2-resolved-project")
	agentID := rsAgent(t, s, "s2-agent-resolved", projectID)

	// Seed a DM conversation between this agent and the dev user.
	// The handler calls ResolveDMConversationForRead(ctx, s.store, log,
	//   "agent", resolvedAgent.ID, "user", user.ID())
	// which derives the DM key and looks it up.
	key := makeDMKey(agentID, DevUserID)
	seedConversation(t, s, "native", key, "direct")

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent="+agentID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag ON + resolved: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S2_FlagOn_ConversationNotFound(t *testing.T) {
	// G3: with fallback removed, an unresolvable conversation returns 409
	// with code "conversation_not_resolved". (AC-G3-2)
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s2-notfound-project")
	agentID := rsAgent(t, s, "s2-agent-notfound", projectID)
	// No conversation seeded → resolve returns nil → typed error.

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent="+agentID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeConversationNotResolved {
		t.Errorf("expected error code %q, got %q", ErrCodeConversationNotResolved, errResp.Error.Code)
	}
}

func TestReadSwitch_S2_FlagOn_AgentLookupFails(t *testing.T) {
	// R-9 discipline: when GetAgent fails (unknown agent), the code skips
	// the conversation path WITHOUT calling IncFallback(). A bad client
	// reference is not a migration gap; conflating the two corrupts the
	// readiness metric that gates Tranche G.
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	// Use a valid UUID that does not exist in the store.
	fakeAgentID := tid("s2-agent-nonexistent")

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent="+fakeAgentID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// The key assertion: IncFallback must NOT be called when the agent
	// lookup fails. This is one line of code and trivially regressed.
	if delta != 0 {
		t.Errorf("R-9: agent lookup failure must NOT increment fallback counter, got delta %d", delta)
	}
}

func TestReadSwitch_S2_FlagOn_SlugAgentParam(t *testing.T) {
	// Pin finding: GetAgent → parseGetID → uuid.Parse rejects slugs.
	// A slug input causes ErrNotFound, which means lookupErr != nil,
	// so the conversation path is silently skipped with no metric signal.
	//
	// Consequence: once the read-switch is ON, every slug-based agent query
	// silently keeps legacy routing and records nothing in the metric — an
	// invisible no-op in the exact path Tranche G is meant to switch.
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent=my-agent-slug", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// Slugs silently skip the conversation path. No IncFallback call.
	if delta != 0 {
		t.Errorf("slug param: expected fallback delta 0 (silent skip), got %d", delta)
	}
}

func TestReadSwitch_S2_FlagOff_SlugReturnsEmpty(t *testing.T) {
	// Pin: with the read-switch OFF, a slug-valued `agent` query parameter
	// yields NO messages, even when messages for that agent exist and the
	// caller is their recipient.
	//
	// The chain: handleMessages sets filter.AgentID = agentID (the raw query
	// param). The store layer's ListMessages uses AgentIDEQ(filter.AgentID)
	// to match against the messages.agent_id column, which only ever
	// receives UUID values. A slug never matches.
	//
	// This test uses a UUID-valued control to prove that the message IS
	// retrievable — ruling out "empty store" as the explanation for the
	// empty result with the slug.
	srv, s := testServer(t)
	// No OperationalSettings → flag OFF.

	projectID := rsProject(t, s, "s2-slug-empty-project")
	agentID := rsAgent(t, s, "s2-agent-slug-empty", projectID)

	// Create a message from the agent to the dev user.
	msg := &store.Message{
		ID:          tid("s2-slug-msg"),
		ProjectID:   projectID,
		Sender:      "agent:" + agentID,
		SenderID:    agentID,
		Recipient:   "user:" + DevUserID,
		RecipientID: DevUserID,
		AgentID:     agentID,
		Msg:         "test message for slug filter test",
		Type:        "output",
		Channel:     "web",
	}
	if err := s.CreateMessage(context.Background(), msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// Control: UUID-valued filter returns the message.
	recUUID := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent="+agentID, nil)
	if recUUID.Code != http.StatusOK {
		t.Fatalf("UUID query: expected 200, got %d: %s", recUUID.Code, recUUID.Body.String())
	}
	var resultUUID store.ListResult[store.Message]
	if err := json.Unmarshal(recUUID.Body.Bytes(), &resultUUID); err != nil {
		t.Fatalf("UUID query: unmarshal: %v", err)
	}
	if len(resultUUID.Items) == 0 {
		t.Fatal("UUID query: expected at least 1 message, got 0 — control failed")
	}

	// Test: slug-valued filter returns nothing.
	recSlug := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent=s2-agent-slug-empty", nil)
	if recSlug.Code != http.StatusOK {
		t.Fatalf("slug query: expected 200, got %d: %s", recSlug.Code, recSlug.Body.String())
	}
	var resultSlug store.ListResult[store.Message]
	if err := json.Unmarshal(recSlug.Body.Bytes(), &resultSlug); err != nil {
		t.Fatalf("slug query: unmarshal: %v", err)
	}
	if len(resultSlug.Items) != 0 {
		t.Errorf("slug query: expected 0 messages (slug never matches agent_id column), got %d", len(resultSlug.Items))
	}
}

func TestReadSwitch_S2_FlagOn_NoAgentParam(t *testing.T) {
	// When no agent query param is provided, the agentID=="" guard at the
	// start of the read-switch block is false → entire block skipped.
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("no agent param: expected fallback delta 0, got %d", delta)
	}
}

// ==========================================================================
// S3 — handleAgentMessages
// Route: GET /api/v1/agents/{id}/messages
// ==========================================================================

func TestReadSwitch_S3_FlagOff(t *testing.T) {
	srv, s := testServer(t)
	// No OperationalSettings → flag OFF.
	projectID := rsProject(t, s, "s3-off-project")
	agentID := rsAgent(t, s, "s3-agent-off", projectID)

	delta := fallbackDelta(func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/messages", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag OFF: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S3_FlagOn_ThreadID_ConversationResolved(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s3-thread-resolved-project")
	agentID := rsAgent(t, s, "s3-agent-thread-resolved", projectID)

	threadID := "s3-thread-" + tid("s3-thread-resolved")
	// Seed the thread conversation. ResolveThreadConversationForRead
	// derives "thread:{projectID}:{threadID}" as external_ref.
	extRef := fmt.Sprintf("thread:%s:%s", projectID, threadID)
	seedConversation(t, s, "native", extRef, "group")

	delta := fallbackDelta(func() {
		url := fmt.Sprintf("/api/v1/agents/%s/messages?thread_id=%s", agentID, threadID)
		rec := doRequest(t, srv, http.MethodGet, url, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag ON thread resolved: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S3_FlagOn_ThreadID_ConversationNotFound(t *testing.T) {
	// G3: with fallback removed, an unresolvable thread conversation returns
	// 409 with code "conversation_not_resolved". (AC-G3-2)
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s3-thread-notfound-project")
	agentID := rsAgent(t, s, "s3-agent-thread-notfound", projectID)

	threadID := "s3-thread-" + tid("s3-thread-notfound")
	// No conversation seeded → resolve returns nil → typed error.

	url := fmt.Sprintf("/api/v1/agents/%s/messages?thread_id=%s", agentID, threadID)
	rec := doRequest(t, srv, http.MethodGet, url, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeConversationNotResolved {
		t.Errorf("expected error code %q, got %q", ErrCodeConversationNotResolved, errResp.Error.Code)
	}
}

func TestReadSwitch_S3_FlagOn_DMDefault_ConversationResolved(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s3-dm-resolved-project")
	agentID := rsAgent(t, s, "s3-agent-dm-resolved", projectID)

	// The DM default fires when channel is "web" or "" and no thread_id.
	// The handler uses agent.ID (UUID, not slug — R-1) and user.ID().
	key := makeDMKey(agentID, DevUserID)
	seedConversation(t, s, "native", key, "direct")

	delta := fallbackDelta(func() {
		// channel="" (empty) triggers the DM default path.
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/messages", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 0 {
		t.Errorf("flag ON DM resolved: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S3_FlagOn_DMDefault_ConversationNotFound(t *testing.T) {
	// G3: with fallback removed, an unresolvable DM conversation returns
	// 409 with code "conversation_not_resolved". (AC-G3-2)
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s3-dm-notfound-project")
	agentID := rsAgent(t, s, "s3-agent-dm-notfound", projectID)
	// No conversation seeded → resolve returns nil → typed error.

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/messages", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeConversationNotResolved {
		t.Errorf("expected error code %q, got %q", ErrCodeConversationNotResolved, errResp.Error.Code)
	}
}

func TestReadSwitch_S3_FlagOn_NonWebChannel(t *testing.T) {
	// Non-web channels (discord, telegram, etc.) do not use the conversation
	// model. When channel is explicitly set to a non-web value and there's
	// no thread_id, neither the thread branch nor the DM default fires.
	// The read-switch is a silent no-op — probably correct, but untested
	// until now.
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s3-nonweb-project")
	agentID := rsAgent(t, s, "s3-agent-nonweb", projectID)

	delta := fallbackDelta(func() {
		url := fmt.Sprintf("/api/v1/agents/%s/messages?channel=discord", agentID)
		rec := doRequest(t, srv, http.MethodGet, url, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// Neither path entered → no IncFallback.
	if delta != 0 {
		t.Errorf("non-web channel: expected fallback delta 0, got %d", delta)
	}
}

func TestReadSwitch_S3_FlagOn_Manager_WithExistingDM_LosesVisibility(t *testing.T) {
	// Pin: the S3 read-switch block (handlers_messages.go:259-279) has NO
	// canManage guard. When a manager calls GET /agents/{id}/messages with
	// no thread_id and channel web/"", ResolveDMConversationForRead resolves
	// the manager's OWN DM with the agent and ANDs ConversationID into the
	// filter. This narrows a filter that was deliberately unscoped
	// ({AgentID} → {AgentID, ConversationID: <manager's own DM>}).
	//
	// The manager silently stops seeing every message that is not in their
	// personal DM with that agent.
	//
	// Three properties make this defect nasty:
	// 1. The divergence metric cannot see it — resolution SUCCEEDS and
	//    narrows wrongly. No fallback, no mismatch, no signal.
	// 2. It is intermittent by caller — it only bites managers who already
	//    have a DM with the agent. See the NoDM sibling test below.
	// 3. No test covered manager + switch ON until now.
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s3-mgr-dm-project")
	agentID := rsAgent(t, s, "s3-agent-mgr-dm", projectID)

	// Create another user who also messages this agent.
	otherUserID := tid("s3-other-user")
	if err := s.CreateUser(context.Background(), &store.User{
		ID: otherUserID, Email: "other@test.com", DisplayName: "Other User",
		Role: "member", Status: "active",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Create a message from the other user to this agent — visible to
	// a manager with the old filter ({AgentID only}).
	otherMsg := &store.Message{
		ID:          tid("s3-mgr-other-msg"),
		ProjectID:   projectID,
		Sender:      "user:" + otherUserID,
		SenderID:    otherUserID,
		Recipient:   "agent:" + agentID,
		RecipientID: agentID,
		AgentID:     agentID,
		Msg:         "message from other user",
		Type:        "instruction",
		Channel:     "web",
	}
	if err := s.CreateMessage(context.Background(), otherMsg); err != nil {
		t.Fatalf("CreateMessage (other): %v", err)
	}

	// Positive control: verify the dev user actually has manage on this
	// agent. If this fails, the visibility assertions below are testing a
	// non-manager path and proving nothing about DEF-64. An unrelated
	// change to admin role resolution would silently convert this test
	// into a non-manager test without any assertion failing — this guard
	// prevents that.
	agent, err := s.GetAgent(context.Background(), agentID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	devUser := NewDevUser(DevUserConfig{})
	manageDecision := srv.authzService.CheckAccess(context.Background(), devUser, agentResource(agent), ActionManage)
	if !manageDecision.Allowed {
		t.Fatalf("precondition failed: dev user does not have manage on agent — "+
			"this test requires a manager caller to exercise DEF-64 (reason: %s)", manageDecision.Reason)
	}

	// Seed the manager's (dev user's) DM conversation with this agent.
	// This is the trigger: without this row, the resolve returns nil and
	// falls back to the correct (unscoped) behaviour.
	managerDMKey := makeDMKey(agentID, DevUserID)
	seedConversation(t, s, "native", managerDMKey, "direct")

	// Request as the admin/manager (dev user).
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result store.ListResult[store.Message]
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// DEF-64 characterisation: pin the KNOWN DEFECT. With the defect
	// present, the manager only sees messages in their own DM. The other
	// user's message is NOT in the manager's DM conversation, so it is
	// filtered out. A failure here likely means DEF-64 was FIXED — update
	// this test to assert the corrected behaviour rather than reverting
	// the fix.
	//
	// Note: fallbackDelta is deliberately absent from this test. The
	// defect succeeds silently — resolution returns a valid conversation
	// and narrows the filter without triggering IncFallback. There is no
	// fallback to count, so a delta assertion here can never fire.
	found := false
	for _, m := range result.Items {
		if m.ID == otherMsg.ID {
			found = true
			break
		}
	}
	if found {
		t.Errorf("DEF-64 (known defect, pinned): manager with an existing DM " +
			"should currently NOT see other users' messages due to missing " +
			"canManage guard. If this now passes, DEF-64 may be FIXED — " +
			"update this test rather than reverting the fix")
	}
}

func TestReadSwitch_S3_FlagOn_Manager_NoDM_Returns409(t *testing.T) {
	// G3 update of the original NoDM control. With fallback removed, a
	// manager who has never chatted with the agent (no DM conversation row)
	// now gets a 409 error instead of silently falling back to the legacy
	// filter. This is the intended G3 behaviour: the fallback is gone, so
	// BOTH the "with DM" and "without DM" cases surface an explicit signal
	// rather than returning potentially wrong results. (AC-G3-2)
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "s3-mgr-nodm-project")
	agentID := rsAgent(t, s, "s3-agent-mgr-nodm", projectID)

	// Positive control: verify the dev user actually has manage on this
	// agent — same guard as the sibling test.
	agent, err := s.GetAgent(context.Background(), agentID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	devUser := NewDevUser(DevUserConfig{})
	manageDecision := srv.authzService.CheckAccess(context.Background(), devUser, agentResource(agent), ActionManage)
	if !manageDecision.Allowed {
		t.Fatalf("precondition failed: dev user does not have manage on agent — "+
			"this test requires a manager caller (reason: %s)", manageDecision.Reason)
	}

	// Do NOT seed a DM conversation for the manager. G3: no DM → nil
	// resolution → typed 409 error (no more fallback to legacy filter).

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/messages", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeConversationNotResolved {
		t.Errorf("expected error code %q, got %q", ErrCodeConversationNotResolved, errResp.Error.Code)
	}
}

// ==========================================================================
// G3 Acceptance-Criteria Tests
// ==========================================================================

// AC-G3-1 — Regression: switch ON + resolvable conversation returns messages.
// The per-site "Resolved" tests above already pin this for all three sites.
// This dedicated test creates a message, resolves the conversation, and
// verifies the message is returned unchanged.
func TestG3_AC1_Regression_SwitchOn_Resolvable(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "g3-ac1-project")
	agentID := rsAgent(t, s, "g3-ac1-agent", projectID)

	// Seed a DM conversation and a message in it.
	key := makeDMKey(agentID, DevUserID)
	convID := seedConversation(t, s, "native", key, "direct")

	msg := &store.Message{
		ID:             tid("g3-ac1-msg"),
		ProjectID:      projectID,
		Sender:         "agent:" + agentID,
		SenderID:       agentID,
		Recipient:      "user:" + DevUserID,
		RecipientID:    DevUserID,
		AgentID:        agentID,
		Msg:            "regression test message",
		Type:           "output",
		Channel:        "web",
		ThreadID:       key,
		ConversationID: convID,
	}
	if err := s.CreateMessage(context.Background(), msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// S1: conversation history
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S1: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var histResp chatHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &histResp); err != nil {
		t.Fatalf("S1: unmarshal: %v", err)
	}
	if len(histResp.Messages) == 0 {
		t.Error("S1: expected at least 1 message, got 0")
	}

	// S2: messages by agent
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent="+agentID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S2: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// S3: agent messages
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S3: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// AC-G3-3 — Switch OFF: behaviour is byte-for-byte unchanged.
// With no OperationalSettings, all three sites use the legacy filter.
func TestG3_AC3_SwitchOff_Unchanged(t *testing.T) {
	srv, s := testServer(t)
	// No enableReadSwitch → flag OFF.

	projectID := rsProject(t, s, "g3-ac3-project")
	agentID := rsAgent(t, s, "g3-ac3-agent", projectID)

	// Create a message with the old-style filter fields.
	key := makeDMKey(agentID, DevUserID)
	msg := &store.Message{
		ID:          tid("g3-ac3-msg"),
		ProjectID:   projectID,
		Sender:      "agent:" + agentID,
		SenderID:    agentID,
		Recipient:   "user:" + DevUserID,
		RecipientID: DevUserID,
		AgentID:     agentID,
		Msg:         "switch-off test message",
		Type:        "output",
		Channel:     "web",
		ThreadID:    key,
	}
	if err := s.CreateMessage(context.Background(), msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// S1: conversation history — old path (Channel=web, ThreadID=key).
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S1 flag OFF: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var histResp chatHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &histResp); err != nil {
		t.Fatalf("S1: unmarshal: %v", err)
	}
	found := false
	for _, m := range histResp.Messages {
		if m.ID == msg.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("S1 flag OFF: expected to find the test message via legacy filter")
	}

	// S2: messages by agent — old path (no ConversationID in filter).
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent="+agentID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S2 flag OFF: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// S3: agent messages — old path.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S3 flag OFF: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// AC-G3-4 — DM key with a part count other than 5 produces an explicit error
// distinguishable from "no such conversation". validDMKey's regex catches
// non-5-part keys at the HTTP layer (400 Bad Request), which is distinguishable
// from the 409 conversation_not_resolved error. Test both too-few and too-many.
func TestG3_AC4_DMKey_TooFewParts(t *testing.T) {
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	// 3-part key: dm:agent:<uuid> — missing the second participant.
	key := "dm:agent:" + tid("g3-ac4-few")

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("too-few parts: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	// Verify the error is NOT conversation_not_resolved — it's a different
	// failure mode (parse, not lookup).
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errResp.Error.Code == ErrCodeConversationNotResolved {
		t.Errorf("too-few parts: error code should NOT be %q — this is a parse failure, not a lookup miss",
			ErrCodeConversationNotResolved)
	}
}

func TestG3_AC4_DMKey_TooManyParts(t *testing.T) {
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	// 7-part key: dm:agent:<uuid>:user:<uuid>:extra:data
	key := makeDMKey(tid("g3-ac4-many"), DevUserID) + ":extra:data"

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("too-many parts: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	// Verify the error is NOT conversation_not_resolved.
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errResp.Error.Code == ErrCodeConversationNotResolved {
		t.Errorf("too-many parts: error code should NOT be %q — this is a parse failure, not a lookup miss",
			ErrCodeConversationNotResolved)
	}
}

// G3-d — the switch-on filter at S1 (handleConversationHistory) must
// preserve Channel:"web" so that messages from other surfaces sharing the
// same conversation_id are NOT returned. Widening is not recoverable.
func TestG3_D_ChannelConstraintPreserved(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "g3-d-project")
	agentID := rsAgent(t, s, "g3-d-agent", projectID)

	// Seed a DM conversation.
	key := makeDMKey(agentID, DevUserID)
	convID := seedConversation(t, s, "native", key, "direct")

	// Create a web message — should be visible.
	webMsg := &store.Message{
		ID:             tid("g3-d-web-msg"),
		ProjectID:      projectID,
		Sender:         "agent:" + agentID,
		SenderID:       agentID,
		Recipient:      "user:" + DevUserID,
		RecipientID:    DevUserID,
		AgentID:        agentID,
		Msg:            "web channel message",
		Type:           "output",
		Channel:        "web",
		ThreadID:       key,
		ConversationID: convID,
	}
	if err := s.CreateMessage(context.Background(), webMsg); err != nil {
		t.Fatalf("CreateMessage (web): %v", err)
	}

	// Create a discord message in the SAME conversation — must NOT be visible.
	discordMsg := &store.Message{
		ID:             tid("g3-d-discord-msg"),
		ProjectID:      projectID,
		Sender:         "agent:" + agentID,
		SenderID:       agentID,
		Recipient:      "user:" + DevUserID,
		RecipientID:    DevUserID,
		AgentID:        agentID,
		Msg:            "discord channel message",
		Type:           "output",
		Channel:        "discord",
		ThreadID:       key,
		ConversationID: convID,
	}
	if err := s.CreateMessage(context.Background(), discordMsg); err != nil {
		t.Fatalf("CreateMessage (discord): %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var histResp chatHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &histResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The web message must appear; the discord message must not.
	var foundWeb, foundDiscord bool
	for _, m := range histResp.Messages {
		if m.ID == webMsg.ID {
			foundWeb = true
		}
		if m.ID == discordMsg.ID {
			foundDiscord = true
		}
	}
	if !foundWeb {
		t.Error("G3-d: web message should be visible but was not returned")
	}
	if foundDiscord {
		t.Error("G3-d: discord message in the same conversation must NOT be " +
			"visible — Channel:\"web\" constraint was dropped by the switch-on filter")
	}
}

// ==========================================================================
// G3-e — SwitchBypassCounter tests
// ==========================================================================

// bypassDelta captures a specific SwitchBypassMetrics accessor before calling
// fn, then returns the delta. Same pattern as fallbackDelta.
func bypassDelta(accessor func() int64, fn func()) int64 {
	before := accessor()
	fn()
	return accessor() - before
}

func TestG3_E_Bypass_SlugParam(t *testing.T) {
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	delta := bypassDelta(messaging.SwitchBypassMetrics.SlugParam, func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent=my-agent-slug", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 1 {
		t.Errorf("slug_param: expected bypass delta 1, got %d", delta)
	}
}

func TestG3_E_Bypass_AgentNotFound(t *testing.T) {
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	// Valid UUID that does not exist in the store.
	fakeAgentID := tid("g3-e-agent-notfound")

	delta := bypassDelta(messaging.SwitchBypassMetrics.AgentNotFound, func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages?agent="+fakeAgentID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 1 {
		t.Errorf("agent_not_found: expected bypass delta 1, got %d", delta)
	}
}

func TestG3_E_Bypass_NonDMKey(t *testing.T) {
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	// No agent param → no DM key to derive.
	delta := bypassDelta(messaging.SwitchBypassMetrics.NonDMKey, func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/messages", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 1 {
		t.Errorf("non_dm_key: expected bypass delta 1, got %d", delta)
	}
}

func TestG3_E_Bypass_WcsNil(t *testing.T) {
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)
	// Deliberately do NOT set webChatStore.

	threadKey := "thread-key-wcsnil-" + tid("g3-e-wcsnil")

	delta := bypassDelta(messaging.SwitchBypassMetrics.WcsNil, func() {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+threadKey+"/messages", nil)
		// wcs nil → returns empty 200 before reaching switch block.
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 1 {
		t.Errorf("wcs_nil: expected bypass delta 1, got %d", delta)
	}
}

func TestG3_E_Bypass_NonWebChannel(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "g3-e-nonweb-project")
	agentID := rsAgent(t, s, "g3-e-agent-nonweb", projectID)

	delta := bypassDelta(messaging.SwitchBypassMetrics.NonWebChannel, func() {
		url := fmt.Sprintf("/api/v1/agents/%s/messages?channel=discord", agentID)
		rec := doRequest(t, srv, http.MethodGet, url, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	if delta != 1 {
		t.Errorf("non_web_channel: expected bypass delta 1, got %d", delta)
	}
}
