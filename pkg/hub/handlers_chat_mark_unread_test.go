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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// POST /api/v1/chat/conversations/{key}/unread
// ---------------------------------------------------------------------------

// TestChatV2_MarkUnread_TopicMovesToPredecessor: with two or more messages,
// mark-unread must set the watermark to the message immediately before the
// latest one (by created_at, id — the same order the monotonic /read guard
// uses), and the topic must then report unread in the thread list rollup.
func TestChatV2_MarkUnread_TopicMovesToPredecessor(t *testing.T) {
	srv, s, wcs, proj, topicID := setupMutePinTest(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Minute)
	older := &store.Message{ID: tid("unread-older"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "thread:" + topicID, Msg: "first", Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: base}
	newer := &store.Message{ID: tid("unread-newer"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "thread:" + topicID, Msg: "second", Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: base.Add(time.Second)}
	if err := s.CreateMessage(ctx, older); err != nil {
		t.Fatalf("CreateMessage(older): %v", err)
	}
	if err := s.CreateMessage(ctx, newer); err != nil {
		t.Fatalf("CreateMessage(newer): %v", err)
	}
	if err := wcs.TouchTopicActivity(ctx, topicID, newer.ID); err != nil {
		t.Fatalf("TouchTopicActivity: %v", err)
	}

	// Start fully read.
	if err := wcs.SetReadState(ctx, DevUserID, topicID, newer.ID); err != nil {
		t.Fatalf("SetReadState: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/unread", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status            string `json:"status"`
		LastReadMessageID string `json:"lastReadMessageId"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LastReadMessageID != older.ID {
		t.Errorf("response lastReadMessageId = %q, want %q", resp.LastReadMessageID, older.ID)
	}

	rs, err := wcs.GetReadState(ctx, DevUserID, topicID)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil || rs.LastReadMessageID != older.ID {
		t.Fatalf("watermark not moved to predecessor: got %+v, want LastReadMessageID = %q", rs, older.ID)
	}

	// The thread list rollup must now report unread.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/threads", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for threads list, got %d: %s", rec.Code, rec.Body.String())
	}
	var threadsResp chatTopicListResponse
	if err := json.NewDecoder(rec.Body).Decode(&threadsResp); err != nil {
		t.Fatalf("decode threads: %v", err)
	}
	found := false
	for _, thread := range threadsResp.Threads {
		if thread.ID != topicID {
			continue
		}
		found = true
		if !thread.HasUnread {
			t.Errorf("expected topic %q to be unread after mark-unread", topicID)
		}
	}
	if !found {
		t.Fatalf("topic %q not found in threads list", topicID)
	}
}

// TestChatV2_MarkUnread_TieBreaksByID: two messages with an identical
// CreatedAt must resolve "which one is newer" the same way ListMessages
// (and /read's monotonic guard) does — (created_at, id) DESC, so the
// higher-ID row counts as newer and becomes the predecessor's successor,
// never the reverse. Pins the ordering claim conversationRecentMessages'
// doc comment makes, the same way
// TestChatV2_ConversationRead_MonotonicTieBreaksByID pins it for /read.
func TestChatV2_MarkUnread_TieBreaksByID(t *testing.T) {
	srv, s, wcs, proj, topicID := setupMutePinTest(t)
	ctx := context.Background()

	tied := time.Now().UTC()
	a := &store.Message{ID: tid("unread-tie-a"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "thread:" + topicID, Msg: "a", Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: tied}
	b := &store.Message{ID: tid("unread-tie-b"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "thread:" + topicID, Msg: "b", Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: tied}
	if err := s.CreateMessage(ctx, a); err != nil {
		t.Fatalf("CreateMessage(a): %v", err)
	}
	if err := s.CreateMessage(ctx, b); err != nil {
		t.Fatalf("CreateMessage(b): %v", err)
	}

	// Determine which of the two sorts later (higher ID) without assuming
	// tid()'s output order — the test must hold regardless.
	lo, hi := a, b
	if lo.ID > hi.ID {
		lo, hi = b, a
	}
	if lo.ID >= hi.ID {
		t.Fatalf("test fixture invariant broken: lo.ID (%q) must be < hi.ID (%q)", lo.ID, hi.ID)
	}

	if err := wcs.TouchTopicActivity(ctx, topicID, hi.ID); err != nil {
		t.Fatalf("TouchTopicActivity: %v", err)
	}
	if err := wcs.SetReadState(ctx, DevUserID, topicID, hi.ID); err != nil {
		t.Fatalf("SetReadState: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/unread", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rs, err := wcs.GetReadState(ctx, DevUserID, topicID)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil || rs.LastReadMessageID != lo.ID {
		t.Fatalf("watermark = %+v, want LastReadMessageID = %q (the lower-ID row, tied on created_at)", rs, lo.ID)
	}
}

// TestChatV2_MarkUnread_TopicClearsWithSingleMessage: a conversation with
// only one message has no predecessor, so mark-unread must clear the
// watermark entirely rather than leaving it pointed at some other message.
func TestChatV2_MarkUnread_TopicClearsWithSingleMessage(t *testing.T) {
	srv, s, wcs, proj, topicID := setupMutePinTest(t)
	ctx := context.Background()

	only := &store.Message{ID: tid("unread-only"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "thread:" + topicID, Msg: "solo", Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: time.Now().UTC()}
	if err := s.CreateMessage(ctx, only); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	if err := wcs.TouchTopicActivity(ctx, topicID, only.ID); err != nil {
		t.Fatalf("TouchTopicActivity: %v", err)
	}
	if err := wcs.SetReadState(ctx, DevUserID, topicID, only.ID); err != nil {
		t.Fatalf("SetReadState: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/unread", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		LastReadMessageID string `json:"lastReadMessageId"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LastReadMessageID != "" {
		t.Errorf("response lastReadMessageId = %q, want empty (cleared)", resp.LastReadMessageID)
	}

	rs, err := wcs.GetReadState(ctx, DevUserID, topicID)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil || rs.LastReadMessageID != "" {
		t.Fatalf("watermark not cleared: got %+v", rs)
	}
}

// TestChatV2_MarkUnread_NoMessagesRejected: marking an empty conversation
// unread is meaningless — there is nothing to become unread — and must be
// rejected rather than silently writing a state nobody asked for.
func TestChatV2_MarkUnread_NoMessagesRejected(t *testing.T) {
	srv, _, _, _, topicID := setupMutePinTest(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/unread", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a conversation with no messages, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestChatV2_MarkUnread_DM: mark-unread on a DM must move the caller's own
// watermark for that DM key, independent of the peer's.
func TestChatV2_MarkUnread_DM(t *testing.T) {
	srv, s, wcs, proj, _ := setupMutePinTest(t)
	ctx := context.Background()

	peerID := tid("unread-dm-peer")
	dmKey := "dm:user:" + DevUserID + ":user:" + peerID
	if err := wcs.UpsertDM(ctx, WebChatDM{ConversationKey: dmKey, ParticipantID: DevUserID, PeerID: peerID, PeerKind: "user"}); err != nil {
		t.Fatalf("UpsertDM: %v", err)
	}

	base := time.Now().UTC().Add(-time.Minute)
	older := &store.Message{ID: tid("unread-dm-older"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "user:peer", RecipientID: peerID, Msg: "hi", Type: messages.TypeChat, Channel: "web", ThreadID: dmKey, CreatedAt: base}
	newer := &store.Message{ID: tid("unread-dm-newer"), ProjectID: proj.ID, Sender: "user:peer", SenderID: peerID,
		Recipient: "user:dev", RecipientID: DevUserID, Msg: "hello back", Type: messages.TypeChat, Channel: "web", ThreadID: dmKey, CreatedAt: base.Add(time.Second)}
	if err := s.CreateMessage(ctx, older); err != nil {
		t.Fatalf("CreateMessage(older): %v", err)
	}
	if err := s.CreateMessage(ctx, newer); err != nil {
		t.Fatalf("CreateMessage(newer): %v", err)
	}

	if err := wcs.SetReadState(ctx, DevUserID, dmKey, newer.ID); err != nil {
		t.Fatalf("SetReadState: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/unread", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rs, err := wcs.GetReadState(ctx, DevUserID, dmKey)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil || rs.LastReadMessageID != older.ID {
		t.Fatalf("DM watermark not moved to predecessor: got %+v, want LastReadMessageID = %q", rs, older.ID)
	}

	// The DM list rollup must now report unread for this caller.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for dms list, got %d: %s", rec.Code, rec.Body.String())
	}
	var dmsResp chatDMListResponse
	if err := json.NewDecoder(rec.Body).Decode(&dmsResp); err != nil {
		t.Fatalf("decode dms: %v", err)
	}
	found := false
	for _, dm := range dmsResp.DMs {
		if dm.ConversationKey != dmKey {
			continue
		}
		found = true
		if !dm.HasUnread {
			t.Errorf("expected DM %q to be unread after mark-unread", dmKey)
		}
	}
	if !found {
		t.Fatalf("DM %q not found in dms list", dmKey)
	}
}

// TestChatV2_MarkUnread_DM_ExcludesMentionRowFromPredecessor: a mention
// fan-out copy sitting as the newest row in a DM's message stream must not
// count as "the latest message". If it did, the predecessor computation
// would land one message too late (on the last genuinely visible message
// instead of the one before it), and since the DM list's own hasUnread
// check (nativeDMLastMessages) already excludes mention rows when computing
// LastMessageID, the watermark would equal LastMessageID and mark-unread
// would silently do nothing.
func TestChatV2_MarkUnread_DM_ExcludesMentionRowFromPredecessor(t *testing.T) {
	srv, s, wcs, proj, _ := setupMutePinTest(t)
	ctx := context.Background()

	peerID := tid("unread-dm-mention-peer")
	dmKey := "dm:user:" + DevUserID + ":user:" + peerID
	if err := wcs.UpsertDM(ctx, WebChatDM{ConversationKey: dmKey, ParticipantID: DevUserID, PeerID: peerID, PeerKind: "user"}); err != nil {
		t.Fatalf("UpsertDM: %v", err)
	}

	base := time.Now().UTC().Add(-time.Minute)
	// Three rows, oldest to newest: a visible chat message, a second visible
	// chat message, and a mention fan-out copy newer than both. Without the
	// exclusion, the predecessor of "the newest row" would be `visible2`;
	// with it, the predecessor of "the newest VISIBLE row" (`visible2`) is
	// `visible1`.
	visible1 := &store.Message{ID: tid("unread-dm-mention-v1"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "user:peer", RecipientID: peerID, Msg: "first", Type: messages.TypeChat, Channel: "web", ThreadID: dmKey, CreatedAt: base}
	visible2 := &store.Message{ID: tid("unread-dm-mention-v2"), ProjectID: proj.ID, Sender: "user:peer", SenderID: peerID,
		Recipient: "user:dev", RecipientID: DevUserID, Msg: "second", Type: messages.TypeChat, Channel: "web", ThreadID: dmKey, CreatedAt: base.Add(time.Second)}
	mentionCopy := &store.Message{ID: tid("unread-dm-mention-copy"), ProjectID: proj.ID, Sender: "user:peer", SenderID: peerID,
		Recipient: "user:dev", RecipientID: DevUserID, Msg: "@dev fyi", Type: messages.TypeMention, Channel: "web", ThreadID: dmKey, CreatedAt: base.Add(2 * time.Second)}
	for _, m := range []*store.Message{visible1, visible2, mentionCopy} {
		if err := s.CreateMessage(ctx, m); err != nil {
			t.Fatalf("CreateMessage(%s): %v", m.ID, err)
		}
	}

	// Start fully read at the newest VISIBLE message (what a normal /read
	// after viewing the DM would have set — the mention copy is never shown,
	// so it is never read either).
	if err := wcs.SetReadState(ctx, DevUserID, dmKey, visible2.ID); err != nil {
		t.Fatalf("SetReadState: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/unread", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rs, err := wcs.GetReadState(ctx, DevUserID, dmKey)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil || rs.LastReadMessageID != visible1.ID {
		t.Fatalf("watermark = %+v, want LastReadMessageID = %q (visible1) — got the mention-inclusive "+
			"predecessor instead, meaning the mention exclusion was skipped", rs, visible1.ID)
	}

	// hasUnread must be true: if the exclusion were dropped, the watermark
	// would land on visible2, which equals nativeDMLastMessages'
	// (mention-excluding) LastMessageID, and hasUnread would be false.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for dms list, got %d: %s", rec.Code, rec.Body.String())
	}
	var dmsResp chatDMListResponse
	if err := json.NewDecoder(rec.Body).Decode(&dmsResp); err != nil {
		t.Fatalf("decode dms: %v", err)
	}
	found := false
	for _, dm := range dmsResp.DMs {
		if dm.ConversationKey != dmKey {
			continue
		}
		found = true
		if dm.LastMessageID != visible2.ID {
			t.Fatalf("dms list LastMessageID = %q, want %q (visible2, mention-excluded)", dm.LastMessageID, visible2.ID)
		}
		if !dm.HasUnread {
			t.Error("expected DM to be unread after mark-unread excluded the mention row")
		}
	}
	if !found {
		t.Fatalf("DM %q not found in dms list", dmKey)
	}
}

// TestChatV2_MarkUnread_NormalReadStaysMonotonicAfterward: once mark-unread
// has moved the watermark backward, a normal /read must still only ever
// move it forward from there — a stale/older POST is a no-op — and posting
// the conversation's actual latest message must clear the unread state.
func TestChatV2_MarkUnread_NormalReadStaysMonotonicAfterward(t *testing.T) {
	srv, s, wcs, proj, topicID := setupMutePinTest(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Minute)
	oldest := &store.Message{ID: tid("unread-mono-oldest"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "thread:" + topicID, Msg: "one", Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: base}
	middle := &store.Message{ID: tid("unread-mono-middle"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "thread:" + topicID, Msg: "two", Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: base.Add(time.Second)}
	latest := &store.Message{ID: tid("unread-mono-latest"), ProjectID: proj.ID, Sender: "user:dev", SenderID: DevUserID,
		Recipient: "thread:" + topicID, Msg: "three", Type: messages.TypeChat, Channel: "web", ThreadID: topicID, CreatedAt: base.Add(2 * time.Second)}
	for _, m := range []*store.Message{oldest, middle, latest} {
		if err := s.CreateMessage(ctx, m); err != nil {
			t.Fatalf("CreateMessage(%s): %v", m.ID, err)
		}
	}
	if err := wcs.TouchTopicActivity(ctx, topicID, latest.ID); err != nil {
		t.Fatalf("TouchTopicActivity: %v", err)
	}
	if err := wcs.SetReadState(ctx, DevUserID, topicID, latest.ID); err != nil {
		t.Fatalf("SetReadState: %v", err)
	}

	// Mark unread: watermark moves to "middle" (the predecessor of "latest").
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/unread", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mark unread: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rs, err := wcs.GetReadState(ctx, DevUserID, topicID)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil || rs.LastReadMessageID != middle.ID {
		t.Fatalf("watermark after mark-unread = %+v, want %q", rs, middle.ID)
	}

	// A stale /read for the oldest message must not roll the watermark back.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/read",
		map[string]string{"messageId": oldest.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("stale read: expected 200 (ignored, not an error), got %d: %s", rec.Code, rec.Body.String())
	}
	rs, err = wcs.GetReadState(ctx, DevUserID, topicID)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil || rs.LastReadMessageID != middle.ID {
		t.Fatalf("monotonic guard broken: watermark = %+v, want unchanged at %q", rs, middle.ID)
	}

	// A /read for the true latest message must succeed and clear the unread state.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/read",
		map[string]string{"messageId": latest.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("read latest: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rs, err = wcs.GetReadState(ctx, DevUserID, topicID)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if rs == nil || rs.LastReadMessageID != latest.ID {
		t.Fatalf("watermark after re-reading latest = %+v, want %q", rs, latest.ID)
	}
}

// TestChatV2_MarkUnread_RejectsBadRequests mirrors
// TestChatV2_Mute_RejectsBadRequests: mark-unread shares authorization and
// key-shape validation with mute/pin/read via authorizeConversationAccess.
func TestChatV2_MarkUnread_RejectsBadRequests(t *testing.T) {
	srv, _, _, _, topicID := setupMutePinTest(t)

	tests := []struct {
		name     string
		method   string
		key      string
		wantCode int
	}{
		{"get is not allowed", http.MethodGet, topicID, http.StatusMethodNotAllowed},
		{"put is not allowed", http.MethodPut, topicID, http.StatusMethodNotAllowed},
		{"delete is not allowed", http.MethodDelete, topicID, http.StatusMethodNotAllowed},
		{"unknown topic", http.MethodPost, tid("unread-missing-topic"), http.StatusNotFound},
		{"malformed DM key", http.MethodPost, "dm:user:nope", http.StatusBadRequest},
		{
			"DM the caller is not part of", http.MethodPost,
			"dm:user:" + tid("unread-stranger-a") + ":user:" + tid("unread-stranger-b"),
			http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v1/chat/conversations/" + tt.key + "/unread"
			rec := doRequest(t, srv, tt.method, path, nil)
			if rec.Code != tt.wantCode {
				t.Errorf("expected %d, got %d: %s", tt.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestChatV2_MarkUnread_Unauthenticated: same as mute/pin/read, mark-unread
// requires authentication.
func TestChatV2_MarkUnread_Unauthenticated(t *testing.T) {
	srv, _, _, _, topicID := setupMutePinTest(t)

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/unread", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestChatV2_MarkUnread_ForbiddenForNonMember: a user with no read access to
// the topic's project must not be able to mark it unread — same rationale as
// TestChatV2_MutePin_ForbiddenForNonMember, and the same authorization call.
func TestChatV2_MarkUnread_ForbiddenForNonMember(t *testing.T) {
	srv, s, wcs, project := setupChatAuthzTest(t)
	ctx := context.Background()

	topicID := tid("unread-authz-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: project.ID,
		Name:      "private",
		CreatedBy: "alice",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	outsider := &store.User{
		ID:          tid("unread-outsider"),
		Email:       "unread-outsider@test.com",
		DisplayName: "Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, outsider); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	rec := doRequestAsUser(t, srv, outsider, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/unread", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-member, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// PublishChatOwnReadStateEvent
// ---------------------------------------------------------------------------

// TestPublishChatOwnReadStateEvent_ReachesCallerOnly: mark-unread's SSE fan-out
// must reach the caller's own subject — the multi-tab sync path — and must
// not fan out to anyone else the way PublishChatReadStateEvent does for a DM
// peer.
func TestPublishChatOwnReadStateEvent_ReachesCallerOnly(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()

	caller := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	other := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	topicKey := "topic-own-read-state"

	chSelf, unsubSelf := pub.Subscribe("user." + caller + ".chat.read-state")
	defer unsubSelf()
	chOther, unsubOther := pub.Subscribe("user." + other + ".chat.read-state")
	defer unsubOther()

	pub.PublishChatOwnReadStateEvent(context.Background(), topicKey, caller, "msg-5")

	select {
	case evt := <-chSelf:
		if evt.Subject != "user."+caller+".chat.read-state" {
			t.Errorf("expected subject user.%s.chat.read-state, got %s", caller, evt.Subject)
		}
		var payload ChatReadStateEvent
		if err := json.Unmarshal(evt.Data, &payload); err != nil {
			t.Fatalf("failed to unmarshal read-state event: %v", err)
		}
		if payload.ConversationKey != topicKey {
			t.Errorf("expected conversationKey %s, got %s", topicKey, payload.ConversationKey)
		}
		if payload.UserID != caller {
			t.Errorf("expected userId %s, got %s", caller, payload.UserID)
		}
		if payload.MessageID != "msg-5" {
			t.Errorf("expected messageId msg-5, got %s", payload.MessageID)
		}
		// The client's sole discriminator for "this is mark-unread, not some
		// other self-notification" — must always be set.
		if !payload.Unread {
			t.Error("expected unread=true on a PublishChatOwnReadStateEvent payload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for own read-state event")
	}

	select {
	case evt := <-chOther:
		t.Fatalf("an unrelated user should not receive the caller's own read-state event, got %s", evt.Subject)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPublishChatOwnReadStateEvent_AllowsEmptyMessageID: the single-message
// "cleared watermark" case must still publish (with an empty messageId), so
// other tabs learn the conversation went unread even when there is no
// predecessor to point at.
func TestPublishChatOwnReadStateEvent_AllowsEmptyMessageID(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()

	caller := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	ch, unsub := pub.Subscribe("user." + caller + ".chat.read-state")
	defer unsub()

	pub.PublishChatOwnReadStateEvent(context.Background(), "dm:user:"+caller+":user:other", caller, "")

	select {
	case evt := <-ch:
		var payload ChatReadStateEvent
		if err := json.Unmarshal(evt.Data, &payload); err != nil {
			t.Fatalf("failed to unmarshal read-state event: %v", err)
		}
		if payload.MessageID != "" {
			t.Errorf("expected empty messageId, got %q", payload.MessageID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for own read-state event")
	}
}
