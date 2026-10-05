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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unreadFixture seeds threads and DMs for the unread-count tests.
type unreadFixture struct {
	t    *testing.T
	s    store.Store
	wcs  WebChatStore
	proj *store.Project
	seq  int
}

// thread creates a topic in f.proj. When lastMsg is true it gets a latest
// message; the message ID is returned ("" otherwise). member adds userID as
// an active participant of the topic's conversation.
func (f *unreadFixture) thread(name, userID string, member, lastMsg bool) (topicID, msgID string) {
	f.t.Helper()
	ctx := context.Background()
	topicID = api.NewUUID()
	require.NoError(f.t, f.wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: f.proj.ID, Name: name,
		CreatedBy: "someone", CreatedAt: time.Now().UTC(),
	}))
	convID := topicConversationID(f.t, f.wcs, topicID)
	if member {
		require.NoError(f.t, f.s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: convID, PrincipalKind: "user", PrincipalID: userID, Role: "member",
		}))
	}
	if lastMsg {
		msgID = api.NewUUID()
		require.NoError(f.t, f.wcs.TouchTopicActivity(ctx, topicID, msgID))
	}
	return topicID, msgID
}

// dm registers a DM between userID and a fresh user peer with one message
// from the peer, and returns its key and that message's ID.
func (f *unreadFixture) dm(userID string) (key, msgID string) {
	f.t.Helper()
	ctx := context.Background()
	peer := api.NewUUID()
	key, err := messages.DMConversationKey("user", peer, "user", userID)
	require.NoError(f.t, err)
	convID := seedConversation(f.t, f.s, "native", key, "direct")
	registerDMParticipants(ctx, f.wcs, key)
	f.seq++
	m := &store.Message{
		ID: api.NewUUID(), ProjectID: f.proj.ID, Sender: "user:peer", SenderID: peer,
		Recipient: "user:me", RecipientID: userID, Msg: "hello",
		Type: messages.TypeInstruction, Channel: "web", ThreadID: key, ConversationID: convID,
		CreatedAt: time.Now().UTC().Add(time.Duration(f.seq) * time.Second),
	}
	require.NoError(f.t, f.s.CreateMessage(ctx, m))
	return key, m.ID
}

func getUnreadCount(t *testing.T, rec *httptest.ResponseRecorder) chatUnreadCountResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp chatUnreadCountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// The count includes only unmuted member threads and DMs whose latest
// message is past the read watermark.
func TestChatUnreadCount_MemberThreadsAndDMs(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	me := DevUserID

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{}, getUnreadCount(t, rec), "empty to start")

	f.thread("member-unread", me, true, true)            // counts
	f.thread("non-member-unread", me, false, true)       // not a member
	muted, _ := f.thread("member-muted", me, true, true) // muted
	require.NoError(t, wcs.SetMuted(ctx, me, muted, true))
	read, readMsg := f.thread("member-read", me, true, true) // read to latest
	require.NoError(t, wcs.SetReadState(ctx, me, read, readMsg))
	f.thread("member-empty", me, true, false) // no messages
	stale, _ := f.thread("member-stale-watermark", me, true, true)
	require.NoError(t, wcs.SetReadState(ctx, me, stale, api.NewUUID())) // counts

	f.dm(me) // counts
	mutedDM, _ := f.dm(me)
	require.NoError(t, wcs.SetMuted(ctx, me, mutedDM, true))
	readDM, readDMMsg := f.dm(me)
	require.NoError(t, wcs.SetReadState(ctx, me, readDM, readDMMsg))

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 3, Threads: 2, DMs: 1}, getUnreadCount(t, rec))

	// Leaving a thread drops it from the count.
	convs, err := s.GetConversationsForPrincipal(ctx, "user", me)
	require.NoError(t, err)
	for _, c := range convs {
		require.NoError(t, s.RemoveParticipant(ctx, c.ID, "user", me))
	}
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 1, Threads: 0, DMs: 1}, getUnreadCount(t, rec))
}

// A deleted thread does not count, even with a participant row.
func TestChatUnreadCount_DeletedThreadExcluded(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	f.thread("keeper", DevUserID, false, false) // the last thread cannot be deleted
	topicID, _ := f.thread("doomed", DevUserID, true, true)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	require.Equal(t, 1, getUnreadCount(t, rec).Threads)

	require.NoError(t, wcs.DeleteTopic(context.Background(), topicID))
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, 0, getUnreadCount(t, rec).Threads)
}

// Each caller sees only their own membership and read state, and a
// participant row does not count without project read access: the row is a
// listing index, not authorization.
func TestChatUnreadCount_CallerScopedAndReadGated(t *testing.T) {
	srv, s, alice, bob, proj := setupDemoPolicyTest(t)
	ctx := context.Background()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}

	// alice (project member) is a member of an unread thread; bob (not a
	// project member) holds a stale participant row on another thread.
	f.thread("alice-thread", alice.ID, true, true)
	bobThread, _ := f.thread("bob-thread", bob.ID, true, true)
	f.dm(bob.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 1, Threads: 1, DMs: 0}, getUnreadCount(t, rec),
		"alice sees only her own thread")

	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 1, Threads: 0, DMs: 1}, getUnreadCount(t, rec),
		"bob's thread is in a project he cannot read; only his DM counts")

	// alice reading bob's thread changes nothing for bob.
	require.NoError(t, wcs.SetReadState(ctx, alice.ID, bobThread, "anything"))
	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, 1, getUnreadCount(t, rec).Conversations)
}

func TestChatUnreadCount_MethodNotAllowed(t *testing.T) {
	srv, _, _, _ := setupSharedChatTest(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// The count is a constant number of read-state queries however many
// conversations the caller has, batched at the configured size.
func TestChatUnreadCount_BatchesReadStates(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	srv.chatSpacesBatch = chatSpacesBatchSizes{topics: 1, readStates: 2}
	f := &unreadFixture{t: t, s: s, wcs: wcs, proj: proj}
	for i := 0; i < 3; i++ {
		f.thread("t"+string(rune('a'+i)), DevUserID, true, true)
	}
	f.dm(DevUserID)
	f.dm(DevUserID)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/unread-count", nil)
	assert.Equal(t, chatUnreadCountResponse{Conversations: 5, Threads: 3, DMs: 2}, getUnreadCount(t, rec))
}
