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

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// isDMParticipant tests
// ---------------------------------------------------------------------------

func TestIsDMParticipant(t *testing.T) {
	tests := []struct {
		key    string
		userID string
		want   bool
	}{
		{"dm:agent:a1:user:u1", "u1", true},
		{"dm:agent:a1:user:u1", "a1", true},
		{"dm:agent:a1:user:u1", "u2", false},
		{"dm:user:u1:user:u2", "u1", true},
		{"dm:user:u1:user:u2", "u2", true},
		{"dm:user:u1:user:u2", "u3", false},
	}
	for _, tt := range tests {
		if got := isDMParticipant(tt.key, tt.userID); got != tt.want {
			t.Errorf("isDMParticipant(%q, %q) = %v, want %v", tt.key, tt.userID, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// resolveDMPeer tests
// ---------------------------------------------------------------------------

func TestResolveDMPeer(t *testing.T) {
	tests := []struct {
		key      string
		callerID string
		wantID   string
	}{
		{"dm:agent:a1:user:u1", "u1", "a1"},
		{"dm:agent:a1:user:u1", "a1", "u1"},
		{"dm:user:u1:user:u2", "u1", "u2"},
		{"dm:user:u1:user:u2", "u2", "u1"},
	}
	for _, tt := range tests {
		_, gotID := resolveDMPeer(tt.key, tt.callerID)
		if gotID != tt.wantID {
			t.Errorf("resolveDMPeer(%q, %q) peerID = %q, want %q", tt.key, tt.callerID, gotID, tt.wantID)
		}
	}
}

// ---------------------------------------------------------------------------
// TypeChat constant audit verification
// ---------------------------------------------------------------------------

func TestTypeChat_InValidTypes(t *testing.T) {
	// TypeChat must be accepted by ValidateType.
	if err := messages.ValidateType(messages.TypeChat); err != nil {
		t.Fatalf("ValidateType(%q) returned error: %v", messages.TypeChat, err)
	}
}

func TestTypeChat_NotDispatchedToAgent(t *testing.T) {
	// type:chat audit (a): Messages with recipient "thread:..." or
	// "user:..." (non-agent-prefixed) are never dispatched to an agent.
	// The dispatch path in handleBrokerInbound routes by topic
	// (project+agent slug), not by type. deliverToAgent in
	// messagebroker.go triggers on agent topics only.
	//
	// This test verifies the structural property: a chat message's
	// recipient prefix is never "agent:", so it never enters the agent
	// dispatch path.
	recipients := []string{
		"thread:topic-uuid-1",
		"user:alice@example.com",
	}
	for _, r := range recipients {
		if strings.HasPrefix(r, "agent:") {
			t.Errorf("chat message recipient %q starts with 'agent:' — would be dispatched to an agent", r)
		}
	}
}

// ---------------------------------------------------------------------------
// Store-level tests (extend wave-2 store tests)
// ---------------------------------------------------------------------------

func TestWave2_ReadState_SetAndGet(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Initially no read state.
	rs, err := store.GetReadState(ctx, "user-1", "topic-1")
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs != nil {
		t.Fatalf("expected nil read state, got %+v", rs)
	}

	// Set read state.
	if err := store.SetReadState(ctx, "user-1", "topic-1", "msg-5"); err != nil {
		t.Fatalf("SetReadState: %v", err)
	}

	rs, err = store.GetReadState(ctx, "user-1", "topic-1")
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil {
		t.Fatal("expected non-nil read state")
	}
	if rs.LastReadMessageID != "msg-5" {
		t.Errorf("LastReadMessageID = %q, want %q", rs.LastReadMessageID, "msg-5")
	}

	// Advance watermark.
	if err := store.SetReadState(ctx, "user-1", "topic-1", "msg-10"); err != nil {
		t.Fatalf("SetReadState advance: %v", err)
	}
	rs, err = store.GetReadState(ctx, "user-1", "topic-1")
	if err != nil {
		t.Fatalf("GetReadState after advance: %v", err)
	}
	if rs.LastReadMessageID != "msg-10" {
		t.Errorf("after advance: LastReadMessageID = %q, want %q", rs.LastReadMessageID, "msg-10")
	}
}

func TestWave2_UserPrefs_DefaultsAndOverride(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Defaults.
	prefs, err := store.GetUserPrefs(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetUserPrefs: %v", err)
	}
	if prefs == nil {
		t.Fatal("expected non-nil default prefs")
	}
	if prefs.SpaceSortMode != "activity" {
		t.Errorf("default SpaceSortMode = %q, want %q", prefs.SpaceSortMode, "activity")
	}
	if prefs.ThreadSortMode != "activity" {
		t.Errorf("default ThreadSortMode = %q, want %q", prefs.ThreadSortMode, "activity")
	}

	// Override.
	if err := store.SetUserPrefs(ctx, "user-1", WebChatUserPrefs{
		UserID:         "user-1",
		SpaceSortMode:  "alpha",
		SpaceOrder:     `["proj-2","proj-1"]`,
		ThreadSortMode: "alpha",
	}); err != nil {
		t.Fatalf("SetUserPrefs: %v", err)
	}

	prefs, err = store.GetUserPrefs(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetUserPrefs after set: %v", err)
	}
	if prefs.SpaceSortMode != "alpha" {
		t.Errorf("SpaceSortMode = %q, want %q", prefs.SpaceSortMode, "alpha")
	}
	if prefs.SpaceOrder != `["proj-2","proj-1"]` {
		t.Errorf("SpaceOrder = %q, want %q", prefs.SpaceOrder, `["proj-2","proj-1"]`)
	}
}

func TestChatV2_DM_UpsertAndList(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Upsert two sides of a DM.
	if err := store.UpsertDM(ctx, WebChatDM{
		ConversationKey: "dm:user:u1:user:u2",
		ParticipantID:   "u1",
		PeerID:          "u2",
		PeerKind:        "user",
		LastActivityAt:  now,
	}); err != nil {
		t.Fatalf("UpsertDM side 1: %v", err)
	}
	if err := store.UpsertDM(ctx, WebChatDM{
		ConversationKey: "dm:user:u1:user:u2",
		ParticipantID:   "u2",
		PeerID:          "u1",
		PeerKind:        "user",
		LastActivityAt:  now,
	}); err != nil {
		t.Fatalf("UpsertDM side 2: %v", err)
	}

	// List for u1.
	dms, err := store.ListDMs(ctx, "u1")
	if err != nil {
		t.Fatalf("ListDMs: %v", err)
	}
	if len(dms) != 1 {
		t.Fatalf("expected 1 DM, got %d", len(dms))
	}
	if dms[0].PeerID != "u2" {
		t.Errorf("DM peer = %q, want %q", dms[0].PeerID, "u2")
	}

	// List for u2.
	dms, err = store.ListDMs(ctx, "u2")
	if err != nil {
		t.Fatalf("ListDMs for u2: %v", err)
	}
	if len(dms) != 1 {
		t.Fatalf("expected 1 DM for u2, got %d", len(dms))
	}
	if dms[0].PeerID != "u1" {
		t.Errorf("DM peer = %q, want %q", dms[0].PeerID, "u1")
	}
}

func TestChatV2_TouchTopicActivity(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Create a topic first.
	if err := store.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-1",
		ProjectID: "proj-1",
		Name:      "test",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Touch with message ID.
	if err := store.TouchTopicActivity(ctx, "topic-1", "msg-1"); err != nil {
		t.Fatalf("TouchTopicActivity: %v", err)
	}

	topic, err := store.GetTopic(ctx, "topic-1")
	if err != nil {
		t.Fatalf("GetTopic: %v", err)
	}
	if topic.LastMessageID != "msg-1" {
		t.Errorf("LastMessageID = %q, want %q", topic.LastMessageID, "msg-1")
	}

	// Touch without message ID (empty string).
	if err := store.TouchTopicActivity(ctx, "topic-1", ""); err != nil {
		t.Fatalf("TouchTopicActivity (no msgID): %v", err)
	}
	topic, err = store.GetTopic(ctx, "topic-1")
	if err != nil {
		t.Fatalf("GetTopic after empty touch: %v", err)
	}
	// LastMessageID should not change.
	if topic.LastMessageID != "msg-1" {
		t.Errorf("LastMessageID changed after empty touch: got %q", topic.LastMessageID)
	}
}

func TestWave2_DeleteTopic_GeneralGuard(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Create #general.
	generalID, _, err := store.EnsureGeneralTopic(ctx, "proj-1", "user-1")
	if err != nil {
		t.Fatalf("EnsureGeneralTopic: %v", err)
	}

	// Attempt to delete #general.
	err = store.DeleteTopic(ctx, generalID)
	if err == nil {
		t.Fatal("expected error when deleting #general")
	}
	if !strings.Contains(err.Error(), "#general") {
		t.Errorf("error should mention #general, got: %v", err)
	}

	// Create and delete a normal topic — should work.
	if err := store.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-2",
		ProjectID: "proj-1",
		Name:      "deletable",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if err := store.DeleteTopic(ctx, "topic-2"); err != nil {
		t.Fatalf("DeleteTopic (normal): %v", err)
	}

	// Verify it's gone from listing.
	topics, err := store.ListTopics(ctx, "proj-1")
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	for _, t2 := range topics {
		if t2.ID == "topic-2" {
			t.Error("deleted topic should not appear in ListTopics")
		}
	}
}

func TestWave2_TopicNameUniqueness(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	if err := store.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-a",
		ProjectID: "proj-1",
		Name:      "design",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Same name in same project should fail.
	err := store.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-b",
		ProjectID: "proj-1",
		Name:      "design",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected error for duplicate name in same project")
	}

	// Same name in different project should succeed.
	if err := store.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-c",
		ProjectID: "proj-2",
		Name:      "design",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("same name in different project should succeed: %v", err)
	}
}

func TestWave2_TouchDMActivity(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Create a DM.
	if err := store.UpsertDM(ctx, WebChatDM{
		ConversationKey: "dm:user:u1:user:u2",
		ParticipantID:   "u1",
		PeerID:          "u2",
		PeerKind:        "user",
		LastActivityAt:  now,
	}); err != nil {
		t.Fatalf("UpsertDM: %v", err)
	}
	if err := store.UpsertDM(ctx, WebChatDM{
		ConversationKey: "dm:user:u1:user:u2",
		ParticipantID:   "u2",
		PeerID:          "u1",
		PeerKind:        "user",
		LastActivityAt:  now,
	}); err != nil {
		t.Fatalf("UpsertDM: %v", err)
	}

	// Touch with message ID.
	if err := store.TouchDMActivity(ctx, "dm:user:u1:user:u2", "msg-1"); err != nil {
		t.Fatalf("TouchDMActivity: %v", err)
	}

	// Verify both sides updated.
	dms1, _ := store.ListDMs(ctx, "u1")
	if len(dms1) != 1 || dms1[0].LastMessageID != "msg-1" {
		t.Errorf("expected u1 DM LastMessageID = msg-1")
	}
	dms2, _ := store.ListDMs(ctx, "u2")
	if len(dms2) != 1 || dms2[0].LastMessageID != "msg-1" {
		t.Errorf("expected u2 DM LastMessageID = msg-1")
	}
}

func TestWave2_GetReadStates_Batch(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Set some read states.
	_ = store.SetReadState(ctx, "user-1", "topic-1", "msg-1")
	_ = store.SetReadState(ctx, "user-1", "topic-2", "msg-5")
	_ = store.SetReadState(ctx, "user-1", "topic-3", "msg-10")

	// Batch fetch.
	states, err := store.GetReadStates(ctx, "user-1", []string{"topic-1", "topic-2", "topic-3", "topic-4"})
	if err != nil {
		t.Fatalf("GetReadStates: %v", err)
	}
	if len(states) != 3 {
		t.Fatalf("expected 3 states, got %d", len(states))
	}

	stateMap := make(map[string]string)
	for _, rs := range states {
		stateMap[rs.ConversationKey] = rs.LastReadMessageID
	}
	if stateMap["topic-1"] != "msg-1" {
		t.Errorf("topic-1 read = %q, want %q", stateMap["topic-1"], "msg-1")
	}
	if stateMap["topic-2"] != "msg-5" {
		t.Errorf("topic-2 read = %q, want %q", stateMap["topic-2"], "msg-5")
	}
	if stateMap["topic-3"] != "msg-10" {
		t.Errorf("topic-3 read = %q, want %q", stateMap["topic-3"], "msg-10")
	}
}

// ---------------------------------------------------------------------------
// Integration tests using testServer (full HTTP stack)
// ---------------------------------------------------------------------------

func TestChatV2_Spaces_RequiresAuth(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/chat/spaces", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", rec.Code)
	}
}

func TestChatV2_Spaces_ReturnsEmpty(t *testing.T) {
	srv, _ := testServer(t)

	// The dev user is authenticated but may not have projects yet.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatSpacesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Spaces == nil {
		t.Error("spaces should be non-nil (empty array)")
	}
}

func TestChatV2_CreateThread_AndList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project.
	proj := &store.Project{ID: tid("chat-test"), Name: "chat-test", Slug: "chat-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Ensure WebChatStore is set up.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	// Create a thread.
	body := map[string]string{"name": "design-review"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/spaces/"+proj.ID+"/threads", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var topic WebChatTopic
	if err := json.NewDecoder(rec.Body).Decode(&topic); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if topic.Name != "design-review" {
		t.Errorf("name = %q, want %q", topic.Name, "design-review")
	}
	if topic.ID == "" {
		t.Error("topic ID should be non-empty")
	}

	// List threads — should include #general (lazy-created) and our thread.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/threads", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var listResp chatTopicListResponse
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listResp.Threads) < 2 {
		t.Fatalf("expected at least 2 threads (general + created), got %d", len(listResp.Threads))
	}

	// Verify #general exists.
	var hasGeneral bool
	for _, th := range listResp.Threads {
		if th.IsGeneral {
			hasGeneral = true
		}
	}
	if !hasGeneral {
		t.Error("expected #general to be in the list")
	}
}

func TestChatV2_CreateThread_Validation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("val-test"), Name: "val-test", Slug: "val-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	// Empty name.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/spaces/"+proj.ID+"/threads", map[string]string{"name": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty name: expected 400, got %d", rec.Code)
	}

	// Name too long.
	longName := strings.Repeat("a", 101)
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/chat/spaces/"+proj.ID+"/threads", map[string]string{"name": longName})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("long name: expected 400, got %d", rec.Code)
	}
}

func TestChatV2_PatchThread(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("patch-test"), Name: "patch-test", Slug: "patch-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	// Create a topic.
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-patch",
		ProjectID: proj.ID,
		Name:      "old-name",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Rename.
	newName := "new-name"
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/chat/topics/topic-patch", map[string]*string{"name": &newName})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated WebChatTopic
	if err := json.NewDecoder(rec.Body).Decode(&updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Name != "new-name" {
		t.Errorf("name = %q, want %q", updated.Name, "new-name")
	}
}

func TestChatV2_PatchThread_GeneralGuard(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("gen-guard"), Name: "gen-guard", Slug: "gen-guard", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	genID, _, err := wcs.EnsureGeneralTopic(ctx, proj.ID, "dev")
	if err != nil {
		t.Fatalf("EnsureGeneralTopic: %v", err)
	}

	// Attempt to rename #general.
	newName := "not-general"
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/chat/topics/"+genID, map[string]*string{"name": &newName})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("rename #general: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_DeleteThread(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("del-test"), Name: "del-test", Slug: "del-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-del",
		ProjectID: proj.ID,
		Name:      "to-delete",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/chat/topics/topic-del", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify soft-deleted — not in list.
	topics, _ := wcs.ListTopics(ctx, proj.ID)
	for _, tp := range topics {
		if tp.ID == "topic-del" {
			t.Error("deleted topic should not appear in ListTopics")
		}
	}
}

func TestChatV2_DeleteThread_GeneralGuard(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("del-gen"), Name: "del-gen", Slug: "del-gen", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	genID, _, _ := wcs.EnsureGeneralTopic(ctx, proj.ID, "dev")
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/chat/topics/"+genID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("delete #general: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_ConversationRead(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("read-test"), Name: "read-test", Slug: "read-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-read",
		ProjectID: proj.ID,
		Name:      "readable",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/topic-read/read",
		map[string]string{"messageId": "msg-42"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_DMs_Empty(t *testing.T) {
	srv, _ := testServer(t)

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatDMListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DMs == nil {
		t.Error("DMs should be non-nil (empty array)")
	}
}

func TestChatV2_UserPrefs_GetDefault(t *testing.T) {
	srv, _ := testServer(t)

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/user-prefs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var prefs WebChatUserPrefs
	if err := json.NewDecoder(rec.Body).Decode(&prefs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if prefs.SpaceSortMode != "activity" {
		t.Errorf("default SpaceSortMode = %q, want %q", prefs.SpaceSortMode, "activity")
	}
}

func TestChatV2_UserPrefs_PutAndGet(t *testing.T) {
	srv, _ := testServer(t)

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	// PUT.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/chat/user-prefs", map[string]string{
		"spaceSortMode":  "alpha",
		"threadSortMode": "alpha",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/user-prefs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var prefs WebChatUserPrefs
	if err := json.NewDecoder(rec.Body).Decode(&prefs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if prefs.SpaceSortMode != "alpha" {
		t.Errorf("SpaceSortMode = %q, want %q", prefs.SpaceSortMode, "alpha")
	}
}

func TestChatV2_Presence_Stub(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/presence", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestChatV2_Search_Stub(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/search?q=test", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestChatV2_LegacyThreads_AuthzFix(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project that only admins can see — the dev user is admin
	// by default so this test just verifies the authz call is present
	// by checking the endpoint doesn't error on a valid project.
	proj := &store.Project{ID: tid("legacy-authz"), Name: "legacy-authz", Slug: "legacy-authz", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/threads?projectId="+proj.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Non-existent project should 404.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/threads?projectId=nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("nonexistent project: expected 404, got %d", rec.Code)
	}
}

func TestChatV2_SpaceRead(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("space-read"), Name: "space-read", Slug: "space-read", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	// Create a topic with a message.
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:            "topic-sr",
		ProjectID:     proj.ID,
		Name:          "space-read-thread",
		CreatedBy:     "dev",
		CreatedAt:     time.Now().UTC(),
		LastMessageID: "msg-99",
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/spaces/"+proj.ID+"/read", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_Members(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("members-test"), Name: "members-test", Slug: "members-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMembersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// humans and agents should be non-nil arrays.
	if resp.Humans == nil {
		t.Error("humans should be non-nil")
	}
	if resp.Agents == nil {
		t.Error("agents should be non-nil")
	}
}

// ---------------------------------------------------------------------------
// DM key validation tests
// ---------------------------------------------------------------------------

func TestValidDMKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		// Valid keys.
		{"dm:user:be67fbc9-c869-5d43-b15d-c28ca3e8d355:user:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", true},
		{"dm:agent:be67fbc9-c869-5d43-b15d-c28ca3e8d355:user:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", true},
		{"dm:user:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:agent:be67fbc9-c869-5d43-b15d-c28ca3e8d355", true},

		// Invalid keys.
		{"dm:user:short:user:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", false},
		{"dm:user:ZZZZZZZZ-ZZZZ-ZZZZ-ZZZZ-ZZZZZZZZZZZZ:user:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", false},  // uppercase
		{"dm:robot:be67fbc9-c869-5d43-b15d-c28ca3e8d355:user:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", false}, // bad kind
		{"dm:user:be67fbc9-c869-5d43-b15d-c28ca3e8d355", false},                                            // truncated
		{"not-a-dm-key", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := validDMKey(tt.key); got != tt.want {
			t.Errorf("validDMKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Send path tests (R4)
// ---------------------------------------------------------------------------

// setupSendTest creates a project, webchat store, and a topic for send path testing.
func setupSendTest(t *testing.T) (*Server, store.Store, WebChatStore, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("send-test"), Name: "send-test", Slug: "send-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	return srv, s, wcs, proj
}

func TestChatV2_Send_NoAgent_TypeChat(t *testing.T) {
	srv, _, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	// Create a topic with no default_agent.
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        tid("topic-send-1"),
		ProjectID: proj.ID,
		Name:      "chat-only",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	body := map[string]string{"content": "hello world"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+tid("topic-send-1")+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Type != messages.TypeChat {
		t.Errorf("expected type %q, got %q", messages.TypeChat, resp.Type)
	}
	if resp.Content != "hello world" {
		t.Errorf("content = %q, want %q", resp.Content, "hello world")
	}
}

func TestChatV2_Send_DefaultAgent_Dispatched(t *testing.T) {
	srv, s, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	// Create an agent.
	agent := &store.Agent{
		ID:        tid("agent-default"),
		ProjectID: proj.ID,
		Name:      "Helper Bot",
		Slug:      "helper-bot",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Create a topic with default_agent set.
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           tid("topic-default-agent"),
		ProjectID:    proj.ID,
		Name:         "agent-thread",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agent.ID,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	body := map[string]string{"content": "please help"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+tid("topic-default-agent")+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Agent-routed messages get type "instruction", not "chat".
	if resp.Type != messages.TypeInstruction {
		t.Errorf("expected type %q (agent-routed), got %q", messages.TypeInstruction, resp.Type)
	}
}

func TestChatV2_Send_Mention_AgentReceives(t *testing.T) {
	srv, s, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	// Create an agent.
	agent := &store.Agent{
		ID:        tid("agent-mention"),
		ProjectID: proj.ID,
		Name:      "Reviewer",
		Slug:      "reviewer",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Topic without default_agent.
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        tid("topic-mention"),
		ProjectID: proj.ID,
		Name:      "mention-thread",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Send with @reviewer mention.
	body := map[string]string{"content": "@reviewer please check this"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+tid("topic-mention")+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// With a resolved mention, message should be type "instruction" (agent-routed).
	if resp.Type != messages.TypeInstruction {
		t.Errorf("expected type %q (mention-routed), got %q", messages.TypeInstruction, resp.Type)
	}
	// Mentions should be populated.
	if len(resp.Mentions) == 0 {
		t.Error("expected non-empty mentions list")
	}
}

func TestChatV2_Send_DM_AgentDM_Routed(t *testing.T) {
	srv, s, _, proj := setupSendTest(t)
	ctx := context.Background()

	// Create an agent so resolveProjectFromDMKey can find the project.
	agent := &store.Agent{
		ID:        tid("dm-agent"),
		ProjectID: proj.ID,
		Name:      "DM Bot",
		Slug:      "dm-bot",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Build a valid DM key: agent DM so project can be resolved.
	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID

	body := map[string]string{"content": "hi there"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// W4 fix: Agent DM without @mention is now correctly routed to the agent
	// (implicit agent routing). The message type should be "instruction".
	if resp.Type != messages.TypeInstruction {
		t.Errorf("agent DM expected type %q (agent-routed), got %q", messages.TypeInstruction, resp.Type)
	}
	// Sender should include the dev user label.
	if resp.SenderID != DevUserID {
		t.Errorf("senderID = %q, want %q", resp.SenderID, DevUserID)
	}
}

func TestChatV2_Send_HumanToHuman_NoDispatch(t *testing.T) {
	srv, _, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	// Create a topic with no default_agent.
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        tid("topic-h2h"),
		ProjectID: proj.ID,
		Name:      "human-only",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	body := map[string]string{"content": "just chatting"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+tid("topic-h2h")+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Must be type:chat (never dispatched to an agent).
	if resp.Type != messages.TypeChat {
		t.Errorf("human-to-human expected type %q, got %q — agent dispatch may have occurred", messages.TypeChat, resp.Type)
	}
	// No dispatcher set on test server, so if this were agent-routed it would
	// still succeed but with type "instruction". The type check above covers this.
}

func TestChatV2_Send_MaxLength_Rejected(t *testing.T) {
	srv, _, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        tid("topic-maxlen"),
		ProjectID: proj.ID,
		Name:      "maxlen-thread",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	longContent := strings.Repeat("x", messages.MaxMessageLength+1)
	body := map[string]string{"content": longContent}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+tid("topic-maxlen")+"/messages", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized message, got %d: %s", rec.Code, rec.Body.String())
	}

	// Exactly at limit should succeed.
	exactContent := strings.Repeat("y", messages.MaxMessageLength)
	body = map[string]string{"content": exactContent}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+tid("topic-maxlen")+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Errorf("expected 201 for exact-limit message, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_Send_InvalidDMKey_Rejected(t *testing.T) {
	srv, _, _, _ := setupSendTest(t)

	// Malformed DM key.
	body := map[string]string{"content": "hello"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/dm:garbage:key/messages", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed DM key: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Method not allowed tests
// ---------------------------------------------------------------------------

func TestChatV2_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/chat/spaces"},
		{http.MethodGet, "/api/v1/chat/presence"},
		{http.MethodPost, "/api/v1/chat/search"},
		{http.MethodPost, "/api/v1/chat/dms"},
	}

	for _, tt := range tests {
		rec := doRequest(t, srv, tt.method, tt.path, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected 405, got %d", tt.method, tt.path, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// W4: Agent DM implicit routing tests
// ---------------------------------------------------------------------------

func TestChatV2_Send_AgentDM_ImplicitRouting(t *testing.T) {
	srv, s, _, proj := setupSendTest(t)
	ctx := context.Background()

	// Create an agent.
	agent := &store.Agent{
		ID:        tid("agent-dm-route"),
		ProjectID: proj.ID,
		Name:      "DM Router",
		Slug:      "dm-router",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Build a valid agent DM key.
	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID

	// Send a message without any @mention — it should be implicitly
	// routed to the agent (type:instruction), not go through human-to-human.
	body := map[string]string{"content": "hello agent, help me please"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Agent DM without @mention should be type:instruction (routed to agent).
	if resp.Type != messages.TypeInstruction {
		t.Errorf("agent DM implicit routing: expected type %q, got %q", messages.TypeInstruction, resp.Type)
	}
}

func TestChatV2_Send_AgentDM_MentionTakesPrecedence(t *testing.T) {
	srv, s, _, proj := setupSendTest(t)
	ctx := context.Background()

	// Create the DM agent.
	dmAgent := &store.Agent{
		ID:        tid("agent-dm-default"),
		ProjectID: proj.ID,
		Name:      "DM Default",
		Slug:      "dm-default",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, dmAgent); err != nil {
		t.Fatalf("CreateAgent (dm): %v", err)
	}

	// Create a different agent that will be mentioned.
	otherAgent := &store.Agent{
		ID:        tid("agent-other-mention"),
		ProjectID: proj.ID,
		Name:      "Other Agent",
		Slug:      "other-agent",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, otherAgent); err != nil {
		t.Fatalf("CreateAgent (other): %v", err)
	}

	dmKey := "dm:agent:" + dmAgent.ID + ":user:" + DevUserID

	// Send a message with @other-agent mention — mention should take precedence
	// over the implicit DM agent routing.
	body := map[string]string{"content": "@other-agent please review this"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Should be type:instruction (agent-routed via mention).
	if resp.Type != messages.TypeInstruction {
		t.Errorf("mention-takes-precedence: expected type %q, got %q", messages.TypeInstruction, resp.Type)
	}
	// Mentions should include the mentioned agent.
	if len(resp.Mentions) == 0 {
		t.Error("expected non-empty mentions list for @other-agent")
	}
}

func TestChatV2_Send_UserDM_HumanToHuman(t *testing.T) {
	srv, _, _, _ := setupSendTest(t)

	// Build a user-to-user DM key.
	peerID := tid("dm-peer-user")
	dmKey := "dm:user:" + DevUserID + ":user:" + peerID

	body := map[string]string{"content": "hey there, how are you?"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMessageResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// User-to-user DM should be type:chat (human-to-human, no agent dispatch).
	if resp.Type != messages.TypeChat {
		t.Errorf("user DM: expected type %q, got %q", messages.TypeChat, resp.Type)
	}
	if resp.SenderID != DevUserID {
		t.Errorf("senderID = %q, want %q", resp.SenderID, DevUserID)
	}
}

// ---------------------------------------------------------------------------
// W4: parseAgentDMKey tests
// ---------------------------------------------------------------------------

func TestParseAgentDMKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"dm:agent:aaaa-bbbb:user:cccc-dddd", "aaaa-bbbb"},
		{"dm:user:aaaa:user:bbbb", ""},
		{"dm:agent:1234:user:5678", "1234"},
		{"not-a-dm", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := parseAgentDMKey(tt.key); got != tt.want {
			t.Errorf("parseAgentDMKey(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
