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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	_ "github.com/mattn/go-sqlite3"
)

// setupMutePinTest builds a server with a web chat store, one project and one
// topic in it, owned by nobody in particular — the dev-auth user is an admin so
// it can read the project.
func setupMutePinTest(t *testing.T) (*Server, store.Store, WebChatStore, *store.Project, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID: tid("mutepin-proj"), Name: "mutepin", Slug: "mutepin",
		Created: time.Now(), Updated: time.Now(),
	}
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

	topicID := tid("mutepin-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "mutable",
		CreatedBy: DevUserID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	return srv, s, wcs, proj, topicID
}

// setupChatAuthzTest builds a server whose project has a real members group and
// policy, so a user outside the project is genuinely refused rather than
// waved through by the dev-auth admin.
func setupChatAuthzTest(t *testing.T) (*Server, store.Store, WebChatStore, *store.Project) {
	t.Helper()
	srv, s, _, _, project := setupDemoPolicyTest(t)

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

	return srv, s, wcs, project
}

// ---------------------------------------------------------------------------
// #1029 — PUT /api/v1/chat/conversations/{key}/mute
// ---------------------------------------------------------------------------

func TestChatV2_Mute_SetAndClear(t *testing.T) {
	srv, _, wcs, _, topicID := setupMutePinTest(t)
	ctx := context.Background()

	tests := []struct {
		name  string
		muted bool
	}{
		{"mute", true},
		{"unmute", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPut,
				"/api/v1/chat/conversations/"+topicID+"/mute",
				map[string]bool{"muted": tt.muted})
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}

			var resp struct {
				Muted bool `json:"muted"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Muted != tt.muted {
				t.Errorf("response muted = %v, want %v", resp.Muted, tt.muted)
			}

			stored, err := wcs.IsConversationMuted(ctx, DevUserID, topicID)
			if err != nil {
				t.Fatalf("IsConversationMuted: %v", err)
			}
			if stored != tt.muted {
				t.Errorf("stored muted = %v, want %v", stored, tt.muted)
			}
		})
	}
}

func TestChatV2_Mute_DMParticipant(t *testing.T) {
	srv, _, wcs, _, _ := setupMutePinTest(t)
	ctx := context.Background()

	dmKey := "dm:user:" + DevUserID + ":user:" + tid("mute-dm-peer")

	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+dmKey+"/mute", map[string]bool{"muted": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	muted, err := wcs.IsConversationMuted(ctx, DevUserID, dmKey)
	if err != nil {
		t.Fatalf("IsConversationMuted: %v", err)
	}
	if !muted {
		t.Error("DM should be muted after PUT")
	}
}

func TestChatV2_Mute_RejectsBadRequests(t *testing.T) {
	srv, _, _, _, topicID := setupMutePinTest(t)

	tests := []struct {
		name     string
		method   string
		key      string
		body     interface{}
		raw      []byte
		wantCode int
	}{
		{"get is not allowed", http.MethodGet, topicID, nil, nil, http.StatusMethodNotAllowed},
		{"post is not allowed", http.MethodPost, topicID, map[string]bool{"muted": true}, nil, http.StatusMethodNotAllowed},
		{"delete is not allowed", http.MethodDelete, topicID, nil, nil, http.StatusMethodNotAllowed},
		{"malformed body", http.MethodPut, topicID, nil, []byte("{not json"), http.StatusBadRequest},
		{"missing muted field", http.MethodPut, topicID, map[string]string{"other": "x"}, nil, http.StatusBadRequest},
		{"unknown topic", http.MethodPut, tid("mutepin-missing"), map[string]bool{"muted": true}, nil, http.StatusNotFound},
		{"malformed DM key", http.MethodPut, "dm:user:nope", map[string]bool{"muted": true}, nil, http.StatusBadRequest},
		{
			"DM the caller is not part of", http.MethodPut,
			"dm:user:" + tid("stranger-a") + ":user:" + tid("stranger-b"),
			map[string]bool{"muted": true}, nil, http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v1/chat/conversations/" + tt.key + "/mute"
			var rec = func() *httptest.ResponseRecorder {
				if tt.raw != nil {
					return doRequestRaw(t, srv, tt.method, path, tt.raw, "application/json")
				}
				return doRequest(t, srv, tt.method, path, tt.body)
			}()
			if rec.Code != tt.wantCode {
				t.Errorf("expected %d, got %d: %s", tt.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestChatV2_Mute_Unauthenticated(t *testing.T) {
	srv, _, _, _, topicID := setupMutePinTest(t)

	rec := doRequestNoAuth(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+topicID+"/mute", map[string]bool{"muted": true})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A user with no read access to the topic's project must not be able to mute
// it — muting a conversation you cannot read would leak its existence and let
// a stranger write to another user's read-state row.
func TestChatV2_MutePin_ForbiddenForNonMember(t *testing.T) {
	srv, s, wcs, project := setupChatAuthzTest(t)
	ctx := context.Background()

	topicID := tid("authz-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: project.ID,
		Name:      "private",
		CreatedBy: "alice",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Deliberately not a hub member: the seeded hub-member-read-all policy
	// grants project read to every hub member, so an outsider is the user the
	// project's read authorization actually turns away.
	outsider := &store.User{
		ID:          tid("chat-outsider"),
		Email:       "outsider@test.com",
		DisplayName: "Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, outsider); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	for _, action := range []struct {
		route string
		body  map[string]bool
	}{
		{"mute", map[string]bool{"muted": true}},
		{"pin", map[string]bool{"pinned": true}},
	} {
		t.Run(action.route, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, outsider, http.MethodPut,
				"/api/v1/chat/conversations/"+topicID+"/"+action.route, action.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for non-member, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// Nothing was written for the outsider.
	muted, err := wcs.IsConversationMuted(ctx, outsider.ID, topicID)
	if err != nil {
		t.Fatalf("IsConversationMuted: %v", err)
	}
	if muted {
		t.Error("refused request must not have muted the conversation")
	}
}

// ---------------------------------------------------------------------------
// #1030 — PUT /api/v1/chat/conversations/{key}/pin
// ---------------------------------------------------------------------------

func TestChatV2_Pin_SetAndClear(t *testing.T) {
	srv, _, wcs, _, topicID := setupMutePinTest(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		pinned bool
	}{
		{"pin", true},
		{"unpin", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPut,
				"/api/v1/chat/conversations/"+topicID+"/pin",
				map[string]bool{"pinned": tt.pinned})
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}

			var resp struct {
				Pinned bool `json:"pinned"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Pinned != tt.pinned {
				t.Errorf("response pinned = %v, want %v", resp.Pinned, tt.pinned)
			}

			rs, err := wcs.GetReadState(ctx, DevUserID, topicID)
			if err != nil {
				t.Fatalf("GetReadState: %v", err)
			}
			if rs == nil {
				t.Fatal("expected a read state row after pinning")
			}
			if rs.Pinned != tt.pinned {
				t.Errorf("stored pinned = %v, want %v", rs.Pinned, tt.pinned)
			}
		})
	}
}

func TestChatV2_Pin_RejectsBadRequests(t *testing.T) {
	srv, _, _, _, topicID := setupMutePinTest(t)

	tests := []struct {
		name     string
		method   string
		key      string
		body     interface{}
		raw      []byte
		wantCode int
	}{
		{"get is not allowed", http.MethodGet, topicID, nil, nil, http.StatusMethodNotAllowed},
		{"post is not allowed", http.MethodPost, topicID, map[string]bool{"pinned": true}, nil, http.StatusMethodNotAllowed},
		{"malformed body", http.MethodPut, topicID, nil, []byte("{nope"), http.StatusBadRequest},
		{"missing pinned field", http.MethodPut, topicID, map[string]string{"other": "x"}, nil, http.StatusBadRequest},
		{"unknown topic", http.MethodPut, tid("pin-missing"), map[string]bool{"pinned": true}, nil, http.StatusNotFound},
		{
			"DM the caller is not part of", http.MethodPut,
			"dm:user:" + tid("stranger-c") + ":user:" + tid("stranger-d"),
			map[string]bool{"pinned": true}, nil, http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v1/chat/conversations/" + tt.key + "/pin"
			var rec = func() *httptest.ResponseRecorder {
				if tt.raw != nil {
					return doRequestRaw(t, srv, tt.method, path, tt.raw, "application/json")
				}
				return doRequest(t, srv, tt.method, path, tt.body)
			}()
			if rec.Code != tt.wantCode {
				t.Errorf("expected %d, got %d: %s", tt.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestChatV2_Pin_Unauthenticated(t *testing.T) {
	srv, _, _, _, topicID := setupMutePinTest(t)

	rec := doRequestNoAuth(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+topicID+"/pin", map[string]bool{"pinned": true})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// API surface: the rail can only render the state it is told about.
// ---------------------------------------------------------------------------

func TestChatV2_ThreadsList_ReportsMutedAndPinned(t *testing.T) {
	srv, _, _, proj, topicID := setupMutePinTest(t)

	for _, route := range []struct {
		name string
		body map[string]bool
	}{
		{"mute", map[string]bool{"muted": true}},
		{"pin", map[string]bool{"pinned": true}},
	} {
		rec := doRequest(t, srv, http.MethodPut,
			"/api/v1/chat/conversations/"+topicID+"/"+route.name, route.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: expected 200, got %d: %s", route.name, rec.Code, rec.Body.String())
		}
	}

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/chat/spaces/"+proj.ID+"/threads", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list threads: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatTopicListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var found *chatTopicEntry
	for i := range resp.Threads {
		if resp.Threads[i].ID == topicID {
			found = &resp.Threads[i]
		}
	}
	if found == nil {
		t.Fatalf("topic %s missing from threads list", topicID)
	}
	if !found.Muted {
		t.Error("threads list should report muted=true")
	}
	if !found.Pinned {
		t.Error("threads list should report pinned=true")
	}
}

// TestChatV2_SpacesList_MutedThreadIsNotCountedUnread covers the rollup the
// rail renders as the space badge. The thread-level suppression is only half
// the feature: a space whose every unread thread is muted must stop carrying a
// number, or the parent keeps shouting about threads the user silenced.
func TestChatV2_SpacesList_MutedThreadIsNotCountedUnread(t *testing.T) {
	srv, _, wcs, proj, topicID := setupMutePinTest(t)
	ctx := context.Background()

	// Give the topic an unread message: activity the caller has never read.
	if err := wcs.TouchTopicActivity(ctx, topicID, tid("space-unread-msg")); err != nil {
		t.Fatalf("TouchTopicActivity: %v", err)
	}

	if got := spaceUnreadCount(t, srv, proj.ID); got != 1 {
		t.Fatalf("unmuted unread thread: unreadCount = %d, want 1", got)
	}

	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+topicID+"/mute", map[string]bool{"muted": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("mute: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if got := spaceUnreadCount(t, srv, proj.ID); got != 0 {
		t.Errorf("muted unread thread: unreadCount = %d, want 0", got)
	}

	// Unmuting brings the badge back — the rollup reads the flag, it does not
	// consume the unread state.
	rec = doRequest(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+topicID+"/mute", map[string]bool{"muted": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("unmute: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := spaceUnreadCount(t, srv, proj.ID); got != 1 {
		t.Errorf("unmuted again: unreadCount = %d, want 1", got)
	}
}

// spaceUnreadCount reads one space's unread rollup from GET /chat/spaces.
func spaceUnreadCount(t *testing.T, srv *Server, projectID string) int {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list spaces: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp chatSpacesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, s := range resp.Spaces {
		if s.ProjectID == projectID {
			return s.UnreadCount
		}
	}
	t.Fatalf("project %s missing from spaces list", projectID)
	return 0
}

func TestChatV2_DMList_ReportsMuted(t *testing.T) {
	srv, _, wcs, _, _ := setupMutePinTest(t)
	ctx := context.Background()

	peerID := tid("dm-mute-peer")
	dmKey := "dm:user:" + DevUserID + ":user:" + peerID
	if err := wcs.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   DevUserID,
		PeerID:          peerID,
		PeerKind:        "user",
		LastActivityAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertDM: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/chat/conversations/"+dmKey+"/mute", map[string]bool{"muted": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("mute DM: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list DMs: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatDMListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.DMs) != 1 {
		t.Fatalf("expected 1 DM, got %d", len(resp.DMs))
	}
	if !resp.DMs[0].Muted {
		t.Error("DM list should report muted=true")
	}
}
