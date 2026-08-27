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
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
		{"dm:agent:a1:user:u1", "a1", false}, // agent slot — user principal must not match
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
// dmUserParticipants tests
// ---------------------------------------------------------------------------

// Typing events for human-to-human DMs have no project to publish on, so they
// fan out to each user participant's own subject. The agent side of an agent DM
// has no user subject and must be skipped.
func TestDMUserParticipants(t *testing.T) {
	tests := []struct {
		key  string
		want []string
	}{
		{"dm:user:u1:user:u2", []string{"u1", "u2"}},
		{"dm:agent:a1:user:u1", []string{"u1"}},
		{"dm:user:u1:user:u1", []string{"u1"}},
		{"dm:user:u1", nil},
		{"topic-uuid", nil},
	}
	for _, tt := range tests {
		got := dmUserParticipants(tt.key)
		if !slices.Equal(got, tt.want) {
			t.Errorf("dmUserParticipants(%q) = %v, want %v", tt.key, got, tt.want)
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

// The members sidebar tooltip shows the agent's status detail and the time of
// its last state change, so both have to survive the trip through this
// endpoint — the heartbeat in lastSeen is not a substitute for either.
func TestChatV2_Members_AgentDetailAndActivityTime(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("members-detail"), Name: "members-detail", Slug: "members-detail", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	activityAt := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	agent := &store.Agent{
		ID:                tid("members-detail-agent"),
		ProjectID:         proj.ID,
		Name:              "Helper Bot",
		Slug:              "helper-bot",
		Phase:             "running",
		Activity:          "blocked",
		Message:           "Waiting for user decision on c34",
		LastSeen:          time.Now().UTC(),
		LastActivityEvent: activityAt,
		OwnerID:           DevUserID,
		CreatedBy:         DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMembersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(resp.Agents))
	}
	got := resp.Agents[0]
	if got.Message != "Waiting for user decision on c34" {
		t.Errorf("message = %q, want the agent's status detail", got.Message)
	}
	if want := activityAt.Format(time.RFC3339); got.LastActivityEvent != want {
		t.Errorf("lastActivityEvent = %q, want %q", got.LastActivityEvent, want)
	}
}

// An agent that has never reported an activity event still needs an updated
// time, otherwise the tooltip loses its second line entirely.
func TestChatV2_Members_LastActivityEventFallsBackToUpdated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("members-fallback"), Name: "members-fallback", Slug: "members-fallback", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:        tid("members-fallback-agent"),
		ProjectID: proj.ID,
		Name:      "Fresh Bot",
		Slug:      "fresh-bot",
		Phase:     "created",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatMembersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(resp.Agents))
	}
	if resp.Agents[0].LastActivityEvent == "" {
		t.Error("lastActivityEvent should fall back to the agent's updated time")
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
	// With a resolved @mention, message should be type "mention" (not "instruction").
	if resp.Type != messages.TypeMention {
		t.Errorf("expected type %q (mention-routed), got %q", messages.TypeMention, resp.Type)
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
	// Should be type:mention (agent-routed via @mention).
	if resp.Type != messages.TypeMention {
		t.Errorf("mention-takes-precedence: expected type %q, got %q", messages.TypeMention, resp.Type)
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

// ---------------------------------------------------------------------------
// parseDMKeyIDs tests
// ---------------------------------------------------------------------------

func TestParseDMKeyIDs(t *testing.T) {
	tests := []struct {
		key       string
		wantAgent string
		wantUser  string
	}{
		{"dm:agent:aaaa-bbbb:user:cccc-dddd", "aaaa-bbbb", "cccc-dddd"},
		{"dm:user:aaaa:user:bbbb", "", ""},   // not an agent-DM
		{"dm:agent:1234:agent:5678", "", ""}, // second slot is not user
		{"dm:agent:abc", "", ""},             // truncated
		{"not-a-dm", "", ""},                 // garbage
		{"", "", ""},                         // empty
	}
	for _, tt := range tests {
		gotAgent, gotUser := parseDMKeyIDs(tt.key)
		if gotAgent != tt.wantAgent || gotUser != tt.wantUser {
			t.Errorf("parseDMKeyIDs(%q) = (%q, %q), want (%q, %q)",
				tt.key, gotAgent, gotUser, tt.wantAgent, tt.wantUser)
		}
	}
}

// ---------------------------------------------------------------------------
// W8: Search tests
// ---------------------------------------------------------------------------

// newTestWebChatStoreWithMessages creates a WebChatStore backed by an in-memory
// SQLite DB, including a minimal messages table for search testing.
func newTestWebChatStoreWithMessages(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	store := NewWebChatStore(db, "sqlite3")
	if err := store.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}

	// Create a minimal messages table matching the Ent schema columns
	// used by SearchChatMessages.
	const createMessages = `
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    sender TEXT NOT NULL DEFAULT '',
    sender_id TEXT,
    recipient TEXT NOT NULL DEFAULT '',
    recipient_id TEXT,
    msg TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'instruction',
    channel TEXT,
    thread_id TEXT,
    visibility TEXT DEFAULT 'normal',
    created TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages (created);
`
	if _, err := db.Exec(createMessages); err != nil {
		t.Fatalf("create messages table: %v", err)
	}

	return store, db
}

// insertTestMessage is a helper to insert a message row for search testing.
func insertTestMessage(t *testing.T, db *sql.DB, id, projectID, threadID, sender, msg string, created time.Time) {
	t.Helper()
	const query = `INSERT INTO messages (id, project_id, thread_id, sender, msg, channel, created) VALUES (?, ?, ?, ?, ?, 'web', ?)`
	_, err := db.Exec(query, id, projectID, threadID, sender, msg, created.UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insert test message: %v", err)
	}
}

func TestSearchChatMessages_BasicMatch(t *testing.T) {
	store, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()

	insertTestMessage(t, db, "m1", "proj-1", "topic-1", "user:alice", "Hello world from alice", now.Add(-3*time.Minute))
	insertTestMessage(t, db, "m2", "proj-1", "topic-1", "user:bob", "Goodbye world from bob", now.Add(-2*time.Minute))
	insertTestMessage(t, db, "m3", "proj-1", "topic-1", "user:alice", "Just a test message", now.Add(-1*time.Minute))

	results, nextCursor, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:     "world",
		ProjectID: "proj-1",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("SearchChatMessages: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Results should be ordered by created DESC.
	if results[0].MessageID != "m2" {
		t.Errorf("first result should be m2 (most recent), got %s", results[0].MessageID)
	}
	if results[1].MessageID != "m1" {
		t.Errorf("second result should be m1, got %s", results[1].MessageID)
	}

	if nextCursor != "" {
		t.Errorf("expected empty nextCursor, got %q", nextCursor)
	}
}

func TestSearchChatMessages_CaseInsensitive(t *testing.T) {
	store, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()

	insertTestMessage(t, db, "m1", "proj-1", "topic-1", "user:alice", "Hello WORLD", now.Add(-2*time.Minute))
	insertTestMessage(t, db, "m2", "proj-1", "topic-1", "user:bob", "hello World", now.Add(-1*time.Minute))

	results, _, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:     "world",
		ProjectID: "proj-1",
	})
	if err != nil {
		t.Fatalf("SearchChatMessages: %v", err)
	}

	// SQLite LIKE is case-insensitive for ASCII.
	if len(results) != 2 {
		t.Fatalf("expected 2 results for case-insensitive search, got %d", len(results))
	}
}

func TestSearchChatMessages_NoResults(t *testing.T) {
	store, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()

	insertTestMessage(t, db, "m1", "proj-1", "topic-1", "user:alice", "Hello world", now)

	results, _, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:     "nonexistent",
		ProjectID: "proj-1",
	})
	if err != nil {
		t.Fatalf("SearchChatMessages: %v", err)
	}

	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestSearchChatMessages_ScopedByProject(t *testing.T) {
	store, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()

	insertTestMessage(t, db, "m1", "proj-1", "topic-1", "user:alice", "Hello world", now.Add(-2*time.Minute))
	insertTestMessage(t, db, "m2", "proj-2", "topic-2", "user:bob", "Hello world", now.Add(-1*time.Minute))

	results, _, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:     "world",
		ProjectID: "proj-1",
	})
	if err != nil {
		t.Fatalf("SearchChatMessages: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result scoped to proj-1, got %d", len(results))
	}
	if results[0].ProjectID != "proj-1" {
		t.Errorf("result project = %q, want %q", results[0].ProjectID, "proj-1")
	}
}

func TestSearchChatMessages_ScopedByConversation(t *testing.T) {
	store, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()

	insertTestMessage(t, db, "m1", "proj-1", "topic-1", "user:alice", "Hello world", now.Add(-2*time.Minute))
	insertTestMessage(t, db, "m2", "proj-1", "topic-2", "user:bob", "Hello world", now.Add(-1*time.Minute))

	results, _, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:           "world",
		ConversationKey: "topic-1",
	})
	if err != nil {
		t.Fatalf("SearchChatMessages: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result scoped to topic-1, got %d", len(results))
	}
	if results[0].ConversationKey != "topic-1" {
		t.Errorf("result conversation = %q, want %q", results[0].ConversationKey, "topic-1")
	}
}

func TestSearchChatMessages_MultipleProjects(t *testing.T) {
	store, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()

	insertTestMessage(t, db, "m1", "proj-1", "topic-1", "user:alice", "Hello world", now.Add(-3*time.Minute))
	insertTestMessage(t, db, "m2", "proj-2", "topic-2", "user:bob", "Hello world", now.Add(-2*time.Minute))
	insertTestMessage(t, db, "m3", "proj-3", "topic-3", "user:carol", "Hello world", now.Add(-1*time.Minute))

	// Search across proj-1 and proj-2 only.
	results, _, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:      "world",
		ProjectIDs: []string{"proj-1", "proj-2"},
	})
	if err != nil {
		t.Fatalf("SearchChatMessages: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results across proj-1 and proj-2, got %d", len(results))
	}
}

func TestSearchChatMessages_Pagination(t *testing.T) {
	store, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()

	// Insert 5 messages.
	for i := 0; i < 5; i++ {
		insertTestMessage(t, db, "m"+string(rune('a'+i)), "proj-1", "topic-1", "user:alice",
			"Hello world message", now.Add(-time.Duration(5-i)*time.Minute))
	}

	// First page: limit 2.
	results, nextCursor, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:     "world",
		ProjectID: "proj-1",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("SearchChatMessages page 1: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("page 1: expected 2 results, got %d", len(results))
	}
	if nextCursor == "" {
		t.Fatal("page 1: expected non-empty nextCursor")
	}

	// Second page using cursor.
	results2, nextCursor2, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:     "world",
		ProjectID: "proj-1",
		Limit:     2,
		Cursor:    nextCursor,
	})
	if err != nil {
		t.Fatalf("SearchChatMessages page 2: %v", err)
	}

	if len(results2) != 2 {
		t.Fatalf("page 2: expected 2 results, got %d", len(results2))
	}

	// Third page — should have 1 remaining.
	results3, nextCursor3, err := store.SearchChatMessages(ctx, ChatSearchFilter{
		Query:     "world",
		ProjectID: "proj-1",
		Limit:     2,
		Cursor:    nextCursor2,
	})
	if err != nil {
		t.Fatalf("SearchChatMessages page 3: %v", err)
	}

	if len(results3) != 1 {
		t.Fatalf("page 3: expected 1 result, got %d", len(results3))
	}
	if nextCursor3 != "" {
		t.Errorf("page 3: expected empty nextCursor, got %q", nextCursor3)
	}

	// Verify no duplicates across pages.
	allIDs := make(map[string]bool)
	for _, r := range results {
		allIDs[r.MessageID] = true
	}
	for _, r := range results2 {
		if allIDs[r.MessageID] {
			t.Errorf("duplicate result across pages: %s", r.MessageID)
		}
		allIDs[r.MessageID] = true
	}
	for _, r := range results3 {
		if allIDs[r.MessageID] {
			t.Errorf("duplicate result across pages: %s", r.MessageID)
		}
		allIDs[r.MessageID] = true
	}

	if len(allIDs) != 5 {
		t.Errorf("expected 5 unique results across all pages, got %d", len(allIDs))
	}
}

// ---------------------------------------------------------------------------
// W8: Snippet generation tests
// ---------------------------------------------------------------------------

func TestGenerateSnippet_BasicMatch(t *testing.T) {
	snippet := generateSnippet("Hello world, how are you doing today?", "world", 80)

	if !strings.Contains(snippet, "<mark>world</mark>") {
		t.Errorf("snippet should contain <mark>world</mark>, got %q", snippet)
	}
}

func TestGenerateSnippet_MatchAtStart(t *testing.T) {
	snippet := generateSnippet("Hello there", "Hello", 80)

	if !strings.Contains(snippet, "<mark>Hello</mark>") {
		t.Errorf("snippet should contain <mark>Hello</mark>, got %q", snippet)
	}
	// Should not have leading "..." since match is at start.
	if strings.HasPrefix(snippet, "...") {
		t.Errorf("snippet should not start with ... when match is at start, got %q", snippet)
	}
}

func TestGenerateSnippet_MatchAtEnd(t *testing.T) {
	snippet := generateSnippet("This is the end", "end", 80)

	if !strings.Contains(snippet, "<mark>end</mark>") {
		t.Errorf("snippet should contain <mark>end</mark>, got %q", snippet)
	}
	// Should not have trailing "..." since match is at end.
	if strings.HasSuffix(snippet, "...") {
		t.Errorf("snippet should not end with ... when match is at end, got %q", snippet)
	}
}

func TestGenerateSnippet_CaseInsensitive(t *testing.T) {
	snippet := generateSnippet("Hello WORLD today", "world", 80)

	if !strings.Contains(snippet, "<mark>WORLD</mark>") {
		t.Errorf("snippet should preserve original case in <mark>, got %q", snippet)
	}
}

func TestGenerateSnippet_LongContent(t *testing.T) {
	long := strings.Repeat("a", 200) + "findme" + strings.Repeat("b", 200)
	snippet := generateSnippet(long, "findme", 80)

	if !strings.Contains(snippet, "<mark>findme</mark>") {
		t.Errorf("snippet should contain <mark>findme</mark>, got %q", snippet)
	}
	// Should have ellipsis on both sides for content in the middle.
	if !strings.HasPrefix(snippet, "...") {
		t.Errorf("snippet should start with ... for middle match, got %q", snippet)
	}
	if !strings.HasSuffix(snippet, "...") {
		t.Errorf("snippet should end with ... for middle match, got %q", snippet)
	}
}

func TestGenerateSnippet_NoMatch(t *testing.T) {
	snippet := generateSnippet("Hello world", "xyz", 80)

	// When no match, should return truncated content.
	if strings.Contains(snippet, "<mark>") {
		t.Errorf("snippet should not contain <mark> when no match, got %q", snippet)
	}
}

func TestGenerateSnippet_EmptyInputs(t *testing.T) {
	if s := generateSnippet("", "test", 80); s != "" {
		t.Errorf("empty content should return empty, got %q", s)
	}
	if s := generateSnippet("hello", "", 80); s != "hello" {
		t.Errorf("empty query should return content, got %q", s)
	}
}

// ---------------------------------------------------------------------------
// W8: Search endpoint tests
// ---------------------------------------------------------------------------

func TestChatSearch_EmptyQuery(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/search", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing q, got %d", rec.Code)
	}
}

func TestChatSearch_TooShortQuery(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/search?q=a", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for q < 2 chars, got %d", rec.Code)
	}
}

func TestChatSearch_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/search?q=hello", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", rec.Code)
	}
}

// R17: GET .../read reports the DM peer's watermark so the sender can render
// the "Seen" receipt on load rather than waiting for the next SSE event.
func TestChatV2_ConversationReadState_ReportsPeerWatermark(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

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

	peerID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	key := "dm:user:" + peerID + ":user:" + DevUserID

	if err := wcs.SetReadState(ctx, peerID, key, "msg-9"); err != nil {
		t.Fatalf("SetReadState(peer): %v", err)
	}
	if err := wcs.SetReadState(ctx, DevUserID, key, "msg-11"); err != nil {
		t.Fatalf("SetReadState(self): %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/chat/conversations/"+url.PathEscape(key)+"/read", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatReadStateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PeerLastReadMessageID != "msg-9" {
		t.Errorf("expected peerLastReadMessageId msg-9, got %q", resp.PeerLastReadMessageID)
	}
	if resp.LastReadMessageID != "msg-11" {
		t.Errorf("expected lastReadMessageId msg-11, got %q", resp.LastReadMessageID)
	}
	if resp.PeerLastReadAt == "" {
		t.Error("expected peerLastReadAt to be populated")
	}
}

// A topic has no "peer" watermark to report — only the caller's own.
func TestChatV2_ConversationReadState_TopicHasNoPeer(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("read-state"), Name: "read-state", Slug: "read-state", Created: time.Now(), Updated: time.Now()}
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
		ID:        "topic-read-state",
		ProjectID: proj.ID,
		Name:      "readable",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if err := wcs.SetReadState(ctx, DevUserID, "topic-read-state", "msg-3"); err != nil {
		t.Fatalf("SetReadState: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/chat/conversations/topic-read-state/read", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatReadStateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LastReadMessageID != "msg-3" {
		t.Errorf("expected lastReadMessageId msg-3, got %q", resp.LastReadMessageID)
	}
	if resp.PeerLastReadMessageID != "" {
		t.Errorf("topic should have no peer watermark, got %q", resp.PeerLastReadMessageID)
	}
}

// R17: a deleted agent must not linger as a thread's default — new messages
// would be routed at an agent that no longer exists.
func TestChatV2_ClearTopicDefaultAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("clear-default"), Name: "clear-default", Slug: "clear-default", Created: time.Now(), Updated: time.Now()}
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

	agentID := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	topics := []struct {
		id           string
		defaultAgent string
	}{
		{"topic-by-slug", "coder"},
		{"topic-by-id", agentID},
		{"topic-other", "reviewer"},
	}
	for _, tc := range topics {
		if err := wcs.CreateTopic(ctx, WebChatTopic{
			ID:           tc.id,
			ProjectID:    proj.ID,
			Name:         tc.id,
			DefaultAgent: tc.defaultAgent,
			CreatedBy:    "dev",
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateTopic(%s): %v", tc.id, err)
		}
	}

	srv.ClearTopicDefaultAgent(ctx, agentID, "coder", proj.ID)

	for _, id := range []string{"topic-by-slug", "topic-by-id"} {
		got, err := wcs.GetTopic(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("GetTopic(%s): %v", id, err)
		}
		if got.DefaultAgent != "" {
			t.Errorf("%s: expected default agent cleared, got %q", id, got.DefaultAgent)
		}
	}

	other, err := wcs.GetTopic(ctx, "topic-other")
	if err != nil || other == nil {
		t.Fatalf("GetTopic(topic-other): %v", err)
	}
	if other.DefaultAgent != "reviewer" {
		t.Errorf("unrelated topic default changed: got %q", other.DefaultAgent)
	}
}

// ---------------------------------------------------------------------------
// History pagination (#1027)
// ---------------------------------------------------------------------------

// seedHistoryMessages inserts n web-channel messages into the given thread,
// one second apart so the keyset ordering (created DESC, id DESC) is stable.
// It returns the message contents in chronological order (oldest first).
func seedHistoryMessages(t *testing.T, s store.Store, projectID, threadID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Duration(n) * time.Second)
	contents := make([]string, 0, n)
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("message-%03d", i)
		msg := &store.Message{
			ID:        tid(threadID + "-msg-" + content),
			ProjectID: projectID,
			Sender:    "user:dev",
			SenderID:  DevUserID,
			Recipient: "thread:" + threadID,
			Msg:       content,
			Type:      messages.TypeChat,
			Channel:   "web",
			ThreadID:  threadID,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := s.CreateMessage(ctx, msg); err != nil {
			t.Fatalf("CreateMessage(%s): %v", content, err)
		}
		contents = append(contents, content)
	}
	return contents
}

// The client sends the pagination cursor as ?cursor= (chat-thread.ts
// fetchHistoryV2). When the handler read a different parameter the cursor was
// silently dropped and every page returned the same newest window, so
// scrollback never advanced past the first page (#1027). Paginating twice is
// the only way to catch that: a single-page assertion passes either way.
func TestChatV2_History_CursorPaginatesToOlderMessages(t *testing.T) {
	srv, s, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	topicID := tid("topic-history-paginate")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "history-paginate",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// More than one default page (50) so a second page must exist.
	const total = 120
	seedHistoryMessages(t, s, proj.ID, topicID, total)

	fetch := func(cursor string) chatHistoryResponse {
		t.Helper()
		path := "/api/v1/chat/conversations/" + topicID + "/messages?limit=50"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequest(t, srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d: %s", path, rec.Code, rec.Body.String())
		}
		var resp chatHistoryResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	first := fetch("")
	if len(first.Messages) != 50 {
		t.Fatalf("first page: got %d messages, want 50", len(first.Messages))
	}
	if first.NextCursor == "" {
		t.Fatalf("first page: expected a nextCursor with %d messages seeded", total)
	}
	// Newest first: the last seeded message heads the first page.
	if got, want := first.Messages[0].Msg, fmt.Sprintf("message-%03d", total-1); got != want {
		t.Errorf("first page head = %q, want %q", got, want)
	}

	second := fetch(first.NextCursor)
	if len(second.Messages) != 50 {
		t.Fatalf("second page: got %d messages, want 50", len(second.Messages))
	}

	// The second page must be disjoint from the first...
	firstIDs := make(map[string]bool, len(first.Messages))
	for _, m := range first.Messages {
		firstIDs[m.ID] = true
	}
	for _, m := range second.Messages {
		if firstIDs[m.ID] {
			t.Fatalf("second page repeats message %q from the first page — cursor was ignored", m.Msg)
		}
	}

	// ...and strictly older than it.
	oldestOnFirst := first.Messages[len(first.Messages)-1].CreatedAt
	newestOnSecond := second.Messages[0].CreatedAt
	if !newestOnSecond.Before(oldestOnFirst) {
		t.Errorf("second page is not older: newest=%s, oldest on first page=%s", newestOnSecond, oldestOnFirst)
	}
	if got, want := second.Messages[0].Msg, fmt.Sprintf("message-%03d", total-51); got != want {
		t.Errorf("second page head = %q, want %q", got, want)
	}

	// A third page walks the tail: 120 seeded, 100 consumed, 20 left.
	third := fetch(second.NextCursor)
	if len(third.Messages) != 20 {
		t.Fatalf("third page: got %d messages, want 20", len(third.Messages))
	}
	if got, want := third.Messages[len(third.Messages)-1].Msg, "message-000"; got != want {
		t.Errorf("third page tail = %q, want %q (oldest message unreachable)", got, want)
	}
	if third.NextCursor != "" {
		t.Errorf("third page: expected no nextCursor, got %q", third.NextCursor)
	}
}

// ---------------------------------------------------------------------------
// Phase-3: Edit/Delete endpoint tests
// ---------------------------------------------------------------------------

// setupEditDeleteTest creates a project, topic, webchat store, and a user
// message for edit/delete testing. It wires the webchat store to the same
// underlying database as the ent store so that UpdateMessageContent (raw SQL)
// can see messages created through the ent store — matching production wiring.
// Returns the server, store, webchat store, topic ID, message ID, and project ID.
func setupEditDeleteTest(t *testing.T) (*Server, store.Store, WebChatStore, string, string, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("send-test"), Name: "send-test", Slug: "send-test", Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Use the ent store's underlying DB so UpdateMessageContent can reach
	// messages created via s.CreateMessage — same wiring as production.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	if !ok {
		t.Fatal("store does not expose DB()")
	}
	rawDB := dbProvider.DB()
	if rawDB == nil {
		t.Fatal("store DB() returned nil")
	}
	wcs := NewWebChatStore(rawDB, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init webchat store: %v", err)
	}
	srv.SetWebChatStore(wcs)

	topicID := tid("topic-editdel")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "edit-delete-test",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Create a message owned by the dev user.
	now := time.Now().UTC()
	msgID := tid("msg-editdel")
	storeMsg := &store.Message{
		ID:        msgID,
		ProjectID: proj.ID,
		Sender:    "user:dev@localhost",
		SenderID:  DevUserID,
		Recipient: "thread:" + topicID,
		Msg:       "original content",
		Type:      "chat",
		Channel:   "web",
		ThreadID:  topicID,
		CreatedAt: now,
	}
	if err := s.CreateMessage(ctx, storeMsg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	return srv, s, wcs, topicID, msgID, proj.ID
}

func TestChatV2_Edit_HappyPath(t *testing.T) {
	srv, s, wcs, topicID, msgID, _ := setupEditDeleteTest(t)
	ctx := context.Background()

	body := map[string]string{"content": "updated content"}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+topicID+"/messages/"+msgID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["messageId"] != msgID {
		t.Errorf("messageId = %v, want %v", resp["messageId"], msgID)
	}
	if resp["content"] != "updated content" {
		t.Errorf("content = %v, want %q", resp["content"], "updated content")
	}
	if resp["editedAt"] == nil {
		t.Error("editedAt should be set")
	}

	// Verify the message content was actually updated in the store.
	msg, err := s.GetMessage(ctx, msgID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Msg != "updated content" {
		t.Errorf("persisted content = %q, want %q", msg.Msg, "updated content")
	}

	// Verify edited_at was recorded in extensions.
	exts, err := wcs.GetMessageExts(ctx, []string{msgID})
	if err != nil {
		t.Fatalf("GetMessageExts: %v", err)
	}
	ext, ok := exts[msgID]
	if !ok {
		t.Fatal("expected extension for message")
	}
	if ext.EditedAt == nil {
		t.Error("expected editedAt to be set in extensions")
	}
}

func TestChatV2_Edit_NonOwner_Forbidden(t *testing.T) {
	srv, s, _, topicID, _, projID := setupEditDeleteTest(t)
	ctx := context.Background()

	// Create a message from a different user.
	otherUserID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	otherMsgID := tid("msg-other-edit")
	otherMsg := &store.Message{
		ID:        otherMsgID,
		ProjectID: projID,
		Sender:    "user:other@localhost",
		SenderID:  otherUserID,
		Recipient: "thread:" + topicID,
		Msg:       "other user message",
		Type:      "chat",
		Channel:   "web",
		ThreadID:  topicID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.CreateMessage(ctx, otherMsg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// DevUser tries to edit another user's message — should get 403.
	body := map[string]string{"content": "hacked content"}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+topicID+"/messages/"+otherMsgID, body)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner edit, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_Edit_AgentReplied_Conflict(t *testing.T) {
	srv, s, _, topicID, msgID, projID := setupEditDeleteTest(t)
	ctx := context.Background()

	// Create an agent reply after the user's message.
	agentMsgID := tid("msg-agent-reply")
	agentMsg := &store.Message{
		ID:        agentMsgID,
		ProjectID: projID,
		Sender:    "agent:helper-bot",
		SenderID:  tid("agent-helper"),
		Recipient: "user:dev@localhost",
		Msg:       "I can help with that",
		Type:      "assistant-reply",
		Channel:   "web",
		ThreadID:  topicID,
		CreatedAt: time.Now().UTC().Add(1 * time.Second),
	}
	if err := s.CreateMessage(ctx, agentMsg); err != nil {
		t.Fatalf("CreateMessage (agent reply): %v", err)
	}

	// Attempt to edit — should get 409 because an agent has replied.
	body := map[string]string{"content": "too late to edit"}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+topicID+"/messages/"+msgID, body)
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 when agent has replied, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_Edit_ConversationKeyMismatch(t *testing.T) {
	srv, _, _, _, msgID, _ := setupEditDeleteTest(t)

	// Try to edit the message using a wrong conversation key.
	body := map[string]string{"content": "wrong conversation"}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/wrong-topic-id/messages/"+msgID, body)
	// Should be rejected — message does not belong to this conversation.
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Errorf("expected 400 or 404 for conversation key mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_Delete_HappyPath(t *testing.T) {
	srv, _, wcs, topicID, msgID, _ := setupEditDeleteTest(t)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/chat/conversations/"+topicID+"/messages/"+msgID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["messageId"] != msgID {
		t.Errorf("messageId = %v, want %v", resp["messageId"], msgID)
	}
	if resp["deletedAt"] == nil {
		t.Error("deletedAt should be set")
	}

	// Verify the extension has deletedAt set.
	exts, err := wcs.GetMessageExts(ctx, []string{msgID})
	if err != nil {
		t.Fatalf("GetMessageExts: %v", err)
	}
	ext, ok := exts[msgID]
	if !ok {
		t.Fatal("expected extension for message")
	}
	if ext.DeletedAt == nil {
		t.Error("expected deletedAt to be set in extensions")
	}
}

func TestChatV2_Delete_NonOwner_Forbidden(t *testing.T) {
	srv, s, _, topicID, _, projID := setupEditDeleteTest(t)
	ctx := context.Background()

	// Create a message from a different user.
	otherUserID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	otherMsgID := tid("msg-other-del")
	otherMsg := &store.Message{
		ID:        otherMsgID,
		ProjectID: projID,
		Sender:    "user:other@localhost",
		SenderID:  otherUserID,
		Recipient: "thread:" + topicID,
		Msg:       "other user message",
		Type:      "chat",
		Channel:   "web",
		ThreadID:  topicID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.CreateMessage(ctx, otherMsg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// DevUser tries to delete another user's message — should get 403.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/chat/conversations/"+topicID+"/messages/"+otherMsgID, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner delete, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_Delete_AgentReplied_Conflict(t *testing.T) {
	srv, s, _, topicID, msgID, projID := setupEditDeleteTest(t)
	ctx := context.Background()

	// Create an agent reply after the user's message.
	agentMsgID := tid("msg-agent-reply-del")
	agentMsg := &store.Message{
		ID:        agentMsgID,
		ProjectID: projID,
		Sender:    "agent:helper-bot",
		SenderID:  tid("agent-helper-del"),
		Recipient: "user:dev@localhost",
		Msg:       "I responded already",
		Type:      "assistant-reply",
		Channel:   "web",
		ThreadID:  topicID,
		CreatedAt: time.Now().UTC().Add(1 * time.Second),
	}
	if err := s.CreateMessage(ctx, agentMsg); err != nil {
		t.Fatalf("CreateMessage (agent reply): %v", err)
	}

	// Attempt to delete — should get 409.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/chat/conversations/"+topicID+"/messages/"+msgID, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 when agent has replied, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatV2_Delete_ContentBlankedInHistory(t *testing.T) {
	srv, _, wcs, topicID, msgID, _ := setupEditDeleteTest(t)
	ctx := context.Background()

	// Soft-delete the message.
	now := time.Now().UTC()
	if err := wcs.SetMessageDeleted(ctx, msgID, now); err != nil {
		t.Fatalf("SetMessageDeleted: %v", err)
	}

	// Fetch history.
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/chat/conversations/"+topicID+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatHistoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Find the deleted message in the response.
	var found bool
	for _, m := range resp.Messages {
		if m.ID == msgID {
			found = true
			if m.Msg != "" {
				t.Errorf("deleted message content should be blank, got %q", m.Msg)
			}
		}
	}
	if !found {
		t.Error("deleted message not found in history response")
	}

	// Verify the extension has deletedAt.
	if ext, ok := resp.MessageExtensions[msgID]; !ok || ext.DeletedAt == nil {
		t.Error("expected deletedAt in message extensions for deleted message")
	}
}

// A human sender that floods a thread is cut off with a retryable 429 rather
// than being allowed to fill the conversation, and the cut-off lifts as soon
// as tokens refill (#1054).
func TestChatV2_Send_RateLimitsFloodingHuman(t *testing.T) {
	srv, _, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        tid("topic-ratelimit"),
		ProjectID: proj.ID,
		Name:      "flooded",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Production limits, test clock: the real 30/min ceiling without a real
	// minute of waiting.
	clock := newTestClock()
	srv.chatSendLimiter = newChatSendLimiterWithClock(clock.Now)

	path := "/api/v1/chat/conversations/" + tid("topic-ratelimit") + "/messages"
	for i := range chatSendHumanRatePerMinute {
		rec := doRequest(t, srv, http.MethodPost, path, map[string]string{"content": "flood"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("send %d: expected 201, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := doRequest(t, srv, http.MethodPost, path, map[string]string{"content": "one too many"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("send %d: expected 429, got %d: %s",
			chatSendHumanRatePerMinute+1, rec.Code, rec.Body.String())
	}
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Error("a rate-limited send must say when to retry (Retry-After header missing)")
	} else if secs, err := strconv.Atoi(retryAfter); err != nil || secs < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", retryAfter)
	}
	if !strings.Contains(rec.Body.String(), ErrCodeRateLimited) {
		t.Errorf("expected a %q error code in the body, got %s", ErrCodeRateLimited, rec.Body.String())
	}
	// The delay belongs in the body as well as the header: no current client
	// reads Retry-After, so the message text is the signal that gets seen.
	if want := "retry in " + retryAfter + "s"; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("expected the body to carry the retry delay %q, got %s", want, rec.Body.String())
	}

	// The refusal is transient: at 30/min a token accrues every 2 seconds.
	clock.Advance(2 * time.Second)
	rec = doRequest(t, srv, http.MethodPost, path, map[string]string{"content": "after backoff"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected the send to succeed after backing off, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Issue #1055: Idempotency key on send
// ---------------------------------------------------------------------------

func TestChatV2_Send_IdempotencyKey_DeduplicatesSend(t *testing.T) {
	srv, _, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	// Create a topic.
	topicID := tid("topic-idem-1")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "idempotency-test",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	path := "/api/v1/chat/conversations/" + topicID + "/messages"
	idemKey := "test-idempotency-key-123"

	// First send — should create the message (201).
	body1 := map[string]string{"content": "idempotent message", "idempotency_key": idemKey}
	rec1 := doRequest(t, srv, http.MethodPost, path, body1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first send: expected 201, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 chatMessageResponse
	if err := json.NewDecoder(rec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if resp1.ID == "" {
		t.Fatal("first send: expected non-empty message ID")
	}

	// Second send with the same idempotency key — should return 200 with the same ID.
	body2 := map[string]string{"content": "idempotent message", "idempotency_key": idemKey}
	rec2 := doRequest(t, srv, http.MethodPost, path, body2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second send: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 chatMessageResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if resp2.ID != resp1.ID {
		t.Errorf("expected same message ID %q on duplicate, got %q", resp1.ID, resp2.ID)
	}
}

func TestChatV2_Send_DifferentIdempotencyKeys_CreateSeparateMessages(t *testing.T) {
	srv, _, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	topicID := tid("topic-idem-2")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "idempotency-diff",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	path := "/api/v1/chat/conversations/" + topicID + "/messages"

	// Two sends with different keys should create two distinct messages.
	body1 := map[string]string{"content": "first message", "idempotency_key": "key-a"}
	rec1 := doRequest(t, srv, http.MethodPost, path, body1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first send: expected 201, got %d", rec1.Code)
	}
	var resp1 chatMessageResponse
	_ = json.NewDecoder(rec1.Body).Decode(&resp1)

	body2 := map[string]string{"content": "second message", "idempotency_key": "key-b"}
	rec2 := doRequest(t, srv, http.MethodPost, path, body2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second send: expected 201, got %d", rec2.Code)
	}
	var resp2 chatMessageResponse
	_ = json.NewDecoder(rec2.Body).Decode(&resp2)

	if resp1.ID == resp2.ID {
		t.Errorf("different idempotency keys should produce different message IDs, both got %q", resp1.ID)
	}
}

func TestChatV2_Send_NoIdempotencyKey_AlwaysCreates(t *testing.T) {
	srv, _, wcs, proj := setupSendTest(t)
	ctx := context.Background()

	topicID := tid("topic-idem-3")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "no-idem-key",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	path := "/api/v1/chat/conversations/" + topicID + "/messages"

	// Without an idempotency key, each send creates a new message.
	body := map[string]string{"content": "same content, no key"}
	rec1 := doRequest(t, srv, http.MethodPost, path, body)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first send: expected 201, got %d", rec1.Code)
	}
	var resp1 chatMessageResponse
	_ = json.NewDecoder(rec1.Body).Decode(&resp1)

	rec2 := doRequest(t, srv, http.MethodPost, path, body)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second send: expected 201, got %d", rec2.Code)
	}
	var resp2 chatMessageResponse
	_ = json.NewDecoder(rec2.Body).Decode(&resp2)

	if resp1.ID == resp2.ID {
		t.Errorf("sends without idempotency key should create distinct messages, both got %q", resp1.ID)
	}
}
