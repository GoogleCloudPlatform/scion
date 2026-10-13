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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatFixture is an inboxFixture with native chat storage, attachment
// storage and a thread in each project.
type chatFixture struct {
	*inboxFixture
	wcs    WebChatStore
	topicA string
	topicB string
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	f := &chatFixture{inboxFixture: newInboxFixture(t)}
	db, ok := f.s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store does not expose DB() *sql.DB")
	f.wcs = NewWebChatStore(db.DB(), "sqlite")
	require.NoError(t, f.wcs.Init())
	f.srv.SetWebChatStore(f.wcs)
	as, err := NewLocalDiskAttachmentStore(t.TempDir())
	require.NoError(t, err)
	f.srv.SetAttachmentStore(as)
	f.topicA = f.topic(t, f.projA)
	f.topicB = f.topic(t, f.projB)
	return f
}

func (f *chatFixture) topic(t *testing.T, projectID string) string {
	t.Helper()
	id := uuid.NewString()
	at := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, f.wcs.CreateTopic(context.Background(), WebChatTopic{
		ID: id, ProjectID: projectID, Name: "chat-" + id[:8], CreatedBy: f.admin, CreatedAt: at, LastActivityAt: at,
	}))
	return id
}

// chatMessage stores a native chat message from sender ("user:<id>" or
// "agent:<id>") in threadID and records it as the conversation's latest.
func (f *chatFixture) chatMessage(t *testing.T, sender, senderID, threadID, projectID, text string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	require.NoError(t, f.s.CreateMessage(ctx, &store.Message{
		ID: id, ProjectID: projectID, Sender: sender, SenderID: senderID, Recipient: "chat",
		Msg: text, Type: "chat", Channel: "web", ThreadID: threadID, CreatedAt: time.Now().UTC().Add(-time.Minute),
	}))
	f.srv.touchConversationActivity(ctx, threadID, id)
	return id
}

// dm returns the DM key of userID with an agent or another user and
// registers it in both participants' DM lists.
func (f *chatFixture) dm(t *testing.T, userID, peerKind, peerID string) string {
	t.Helper()
	key, err := messages.DMConversationKey("user", userID, peerKind, peerID)
	require.NoError(t, err)
	registerDMParticipants(context.Background(), f.wcs, key)
	return key
}

func chatConversationPath(key, action string) string {
	return "/api/v1/chat/conversations/" + key + "/" + action
}

// refusedOrHidden reports a refusal: 403, or 404 where the route answers a
// refused caller as a missing record.
func refusedOrHidden(code int) bool {
	return code == http.StatusForbidden || code == http.StatusNotFound
}

// TestChatPrefsToken_RequiresInboxSelectorAndAgentRead requires a token to
// carry inbox:read to read an agent thread preference and inbox:write to
// write one, both for the agent's project, on top of agent:read.
func TestChatPrefsToken_RequiresInboxSelectorAndAgentRead(t *testing.T) {
	f := newChatFixture(t)
	path := func(agent *store.Agent) string { return "/api/v1/chat/prefs?agentId=" + agent.ID }
	body := map[string]string{"visibility_mode": "verbose"}

	agentOnly := f.mint(t, f.admin, hubBoundary(), "agent:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, agentOnly, http.MethodGet, path(f.agentA), nil).Code)

	reader := f.mint(t, f.admin, hubBoundary(), "inbox:read", "agent:read")
	assert.Equal(t, http.StatusOK, f.call(t, reader, http.MethodGet, path(f.agentA), nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, reader, http.MethodPut, path(f.agentA), body).Code, "a write needs inbox:write")

	writer := f.mint(t, f.admin, hubBoundary(), "inbox:write", "agent:read")
	assert.Equal(t, http.StatusOK, f.call(t, writer, http.MethodPut, path(f.agentA), body).Code)

	inboxOnly := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, inboxOnly, http.MethodGet, path(f.agentA), nil).Code, "agent:read still applies")

	projA := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "agent:read")
	assert.Equal(t, http.StatusOK, f.call(t, projA, http.MethodGet, path(f.agentA), nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, projA, http.MethodGet, path(f.agentB), nil).Code)

	assert.Equal(t, http.StatusOK, f.session(t, f.admin, http.MethodGet, path(f.agentB), nil).Code)
}

func chatSpaceIDs(t *testing.T, rec *httptest.ResponseRecorder) ([]string, bool) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatSpacesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := []string{}
	for _, sp := range resp.Spaces {
		ids = append(ids, sp.ProjectID)
	}
	sort.Strings(ids)
	return ids, resp.Prefs != nil
}

// TestChatSpacesToken_FilteredToBoundary requires the space list to hold
// only the projects a token can read (a project token: its boundary
// project), and the caller's rail preferences only for a token with
// inbox:read on a hub boundary.
func TestChatSpacesToken_FilteredToBoundary(t *testing.T) {
	f := newChatFixture(t)
	rec := f.session(t, f.admin, http.MethodPut, "/api/v1/chat/user-prefs", map[string]string{"spaceSortMode": "alpha"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	both := []string{f.projA, f.projB}
	sort.Strings(both)

	ids, prefs := chatSpaceIDs(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "project:read"), http.MethodGet, "/api/v1/chat/spaces", nil))
	assert.Subset(t, ids, both)
	assert.False(t, prefs, "rail prefs need inbox:read")

	ids, prefs = chatSpaceIDs(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "project:read", "inbox:read"), http.MethodGet, "/api/v1/chat/spaces", nil))
	assert.Subset(t, ids, both)
	assert.True(t, prefs)

	ids, prefs = chatSpaceIDs(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "inbox:read"), http.MethodGet, "/api/v1/chat/spaces", nil))
	assert.Equal(t, []string{f.projA}, ids)
	assert.False(t, prefs, "rail prefs have no project, so a project token does not see them")

	ids, _ = chatSpaceIDs(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "agent:read"), http.MethodGet, "/api/v1/chat/spaces", nil))
	assert.Empty(t, ids)
}

func chatUnreadCountOf(t *testing.T, rec *httptest.ResponseRecorder) chatUnreadCountResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatUnreadCountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// TestChatUnreadCountToken_FilteredToBoundary requires a token to carry
// inbox:read for the unread count, and counts for it only threads of
// projects it can read and DMs the DM rule lets it read.
func TestChatUnreadCountToken_FilteredToBoundary(t *testing.T) {
	f := newChatFixture(t)
	f.chatMessage(t, "user:"+f.member, f.member, f.topicA, f.projA, "unread in A")
	f.chatMessage(t, "user:"+f.member, f.member, f.topicB, f.projB, "unread in B")
	dmA := f.dm(t, f.admin, "agent", f.agentA.ID)
	dmB := f.dm(t, f.admin, "agent", f.agentB.ID)
	f.chatMessage(t, "agent:"+f.agentA.Slug, f.agentA.ID, dmA, f.projA, "unread DM A")
	f.chatMessage(t, "agent:"+f.agentB.Slug, f.agentB.ID, dmB, f.projB, "unread DM B")
	const path = "/api/v1/chat/unread-count"

	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, hubBoundary(), "project:read", "agent:read"), http.MethodGet, path, nil).Code)

	hub := chatUnreadCountOf(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:read", "project:read", "agent:read"), http.MethodGet, path, nil))
	assert.Equal(t, 2, hub.Threads)
	assert.Equal(t, 2, hub.DMs)

	projA := chatUnreadCountOf(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "project:read", "agent:read"), http.MethodGet, path, nil))
	assert.Equal(t, 1, projA.Threads)
	assert.Equal(t, 1, projA.DMs)

	noAgentRead := chatUnreadCountOf(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:read", "project:read"), http.MethodGet, path, nil))
	assert.Equal(t, 0, noAgentRead.DMs, "an agent DM needs agent:read on the agent")

	session := chatUnreadCountOf(t, f.session(t, f.admin, http.MethodGet, path, nil))
	assert.Equal(t, 2, session.DMs)
}

// TestChatWriteToken_RequiresInboxWrite requires a token to carry
// inbox:write for every chat write on a thread or space, on top of
// project:read on its project, and to hold the project inside its
// boundary. Reads keep project:read alone.
func TestChatWriteToken_RequiresInboxWrite(t *testing.T) {
	f := newChatFixture(t)
	topicDel := f.topic(t, f.projA)
	m := f.chatMessage(t, "user:"+f.admin, f.admin, f.topicA, f.projA, "admin message")
	editMsg := f.chatMessage(t, "user:"+f.admin, f.admin, f.topicA, f.projA, "admin edit")

	type req struct {
		method, path string
		body         interface{}
	}
	writes := []req{
		{http.MethodPost, "/api/v1/chat/spaces/" + f.projA + "/threads", map[string]string{"name": "token thread"}},
		{http.MethodPost, "/api/v1/chat/spaces/" + f.projA + "/read", nil},
		{http.MethodPatch, "/api/v1/chat/topics/" + f.topicA, map[string]string{}},
		{http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "from a token"}},
		{http.MethodPost, chatConversationPath(f.topicA, "read"), map[string]string{"messageId": m}},
		{http.MethodPost, chatConversationPath(f.topicA, "unread"), nil},
		{http.MethodPut, chatConversationPath(f.topicA, "mute"), map[string]bool{"muted": true}},
		{http.MethodPut, chatConversationPath(f.topicA, "pin"), map[string]bool{"pinned": true}},
		{http.MethodPost, chatConversationPath(f.topicA, "typing"), nil},
		{http.MethodPut, chatConversationPath(f.topicA, "messages/"+editMsg), map[string]string{"content": "edited"}},
		{http.MethodDelete, "/api/v1/chat/topics/" + topicDel, nil},
	}

	readOnly := f.mint(t, f.admin, hubBoundary(), "project:read")
	for _, w := range writes {
		rec := f.call(t, readOnly, w.method, w.path, w.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s without inbox:write: %s", w.method, w.path, rec.Body.String())
	}

	otherProject := f.mint(t, f.admin, projectBoundary(f.projB), "project:read", "inbox:write")
	for _, w := range writes {
		rec := f.call(t, otherProject, w.method, w.path, w.body)
		assert.True(t, refusedOrHidden(rec.Code), "%s %s with a project B token: got %d %s", w.method, w.path, rec.Code, rec.Body.String())
	}

	writer := f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "inbox:write")
	for _, w := range writes {
		rec := f.call(t, writer, w.method, w.path, w.body)
		assert.True(t, rec.Code >= 200 && rec.Code < 300, "%s %s with inbox:write: got %d %s", w.method, w.path, rec.Code, rec.Body.String())
	}

	for _, p := range []string{
		"/api/v1/chat/spaces/" + f.projA + "/threads",
		"/api/v1/chat/topics/" + f.topicA,
		chatConversationPath(f.topicA, "messages"),
		chatConversationPath(f.topicA, "read"),
		"/api/v1/chat/spaces/" + f.projA + "/members",
	} {
		rec := f.call(t, readOnly, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "GET %s with project:read: %s", p, rec.Body.String())
	}
}

// TestChatDirectMessageToken_PeerAgentInsideBoundary requires a token to
// carry inbox:read to read a DM with an agent and inbox:write to act on
// it, with the agent's project inside its boundary; reads and read-state
// changes also need agent:read on the agent. A send needs agent:message as
// for every caller.
func TestChatDirectMessageToken_PeerAgentInsideBoundary(t *testing.T) {
	f := newChatFixture(t)
	dmA := f.dm(t, f.admin, "agent", f.agentA.ID)
	dmB := f.dm(t, f.admin, "agent", f.agentB.ID)
	f.chatMessage(t, "agent:"+f.agentA.Slug, f.agentA.ID, dmA, f.projA, "hello from A")
	f.chatMessage(t, "agent:"+f.agentB.Slug, f.agentB.ID, dmB, f.projB, "hello from B")

	projAReader := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "agent:read")
	assert.Equal(t, http.StatusOK, f.call(t, projAReader, http.MethodGet, chatConversationPath(dmA, "messages"), nil).Code)
	assert.Equal(t, http.StatusOK, f.call(t, projAReader, http.MethodGet, chatConversationPath(dmA, "read"), nil).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, projAReader, http.MethodGet, chatConversationPath(dmB, "messages"), nil).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, projAReader, http.MethodGet, chatConversationPath(dmB, "read"), nil).Code)
	assert.Equal(t, http.StatusOK, f.call(t, projAReader, http.MethodGet, "/api/v1/chat/search?q=hello&key="+dmA, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, projAReader, http.MethodGet, "/api/v1/chat/search?q=hello&key="+dmB, nil).Code)

	noAgentRead := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, noAgentRead, http.MethodGet, chatConversationPath(dmA, "messages"), nil).Code)

	noInbox := f.mint(t, f.admin, hubBoundary(), "agent:read", "agent:message")
	assert.Equal(t, http.StatusForbidden, f.call(t, noInbox, http.MethodGet, chatConversationPath(dmA, "messages"), nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, noInbox, http.MethodPost, chatConversationPath(dmA, "messages"), map[string]string{"content": "x"}).Code)

	projAWriter := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write", "agent:read", "agent:message")
	assert.Equal(t, http.StatusOK, f.call(t, projAWriter, http.MethodPut, chatConversationPath(dmA, "mute"), map[string]bool{"muted": true}).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, projAWriter, http.MethodPut, chatConversationPath(dmB, "mute"), map[string]bool{"muted": true}).Code)
	assert.Equal(t, http.StatusOK, f.call(t, projAWriter, http.MethodPost, chatConversationPath(dmA, "typing"), nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, projAWriter, http.MethodPost, chatConversationPath(dmB, "typing"), nil).Code)
	rec := f.call(t, projAWriter, http.MethodPost, chatConversationPath(dmA, "messages"), map[string]string{"content": "to agent A"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusNotFound, f.call(t, projAWriter, http.MethodPost, chatConversationPath(dmB, "messages"), map[string]string{"content": "to agent B"}).Code)

	// A hub token holds every agent's project inside its boundary.
	hubSender := f.mint(t, f.admin, hubBoundary(), "inbox:write", "agent:message")
	rec = f.call(t, hubSender, http.MethodPost, chatConversationPath(dmB, "messages"), map[string]string{"content": "to agent B"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Inter-agent history keeps agent:attach.
	attach := f.mint(t, f.admin, projectBoundary(f.projA), "agent:attach")
	assert.Equal(t, http.StatusOK, f.call(t, attach, http.MethodGet, chatConversationPath(dmA, "interagent"), nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, attach, http.MethodGet, chatConversationPath(dmB, "interagent"), nil).Code)
}

// TestChatHumanDirectMessage_RequiresHubBoundary requires a token to hold a
// hub boundary for a DM between two users: the DM has no project.
func TestChatHumanDirectMessage_RequiresHubBoundary(t *testing.T) {
	f := newChatFixture(t)
	key := f.dm(t, f.admin, "user", f.member)
	f.chatMessage(t, "user:"+f.member, f.member, key, uuid.Nil.String(), "hello admin")

	projA := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "inbox:write")
	assert.Equal(t, http.StatusNotFound, f.call(t, projA, http.MethodGet, chatConversationPath(key, "messages"), nil).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, projA, http.MethodPut, chatConversationPath(key, "pin"), map[string]bool{"pinned": true}).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, projA, http.MethodPost, chatConversationPath(key, "messages"), map[string]string{"content": "x"}).Code)

	hubReader := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, http.StatusOK, f.call(t, hubReader, http.MethodGet, chatConversationPath(key, "messages"), nil).Code)

	hubWriter := f.mint(t, f.admin, hubBoundary(), "inbox:write", "user:read")
	assert.Equal(t, http.StatusOK, f.call(t, hubWriter, http.MethodPut, chatConversationPath(key, "pin"), map[string]bool{"pinned": true}).Code)
	rec := f.call(t, hubWriter, http.MethodPost, chatConversationPath(key, "messages"), map[string]string{"content": "hello member"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	hubNoInbox := f.mint(t, f.admin, hubBoundary(), "user:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, hubNoInbox, http.MethodPost, chatConversationPath(key, "messages"), map[string]string{"content": "x"}).Code)
}

// TestChatMessageEdit_RechecksTargetConversation requires a token editing
// or deleting one of its holder's messages to pass the conversation check
// again: project:read and inbox:write for a thread's project, the DM rule
// for a DM. The authorship rule applies to every caller.
func TestChatMessageEdit_RechecksTargetConversation(t *testing.T) {
	f := newChatFixture(t)
	inThread := f.chatMessage(t, "user:"+f.admin, f.admin, f.topicA, f.projA, "thread message")
	dmB := f.dm(t, f.admin, "agent", f.agentB.ID)
	inDM := f.chatMessage(t, "user:"+f.admin, f.admin, dmB, f.projB, "dm message")
	notMine := f.chatMessage(t, "user:"+f.member, f.member, f.topicA, f.projA, "member message")
	edit := map[string]string{"content": "edited"}
	path := func(key, id string) string { return chatConversationPath(key, "messages/"+id) }

	noProjectRead := f.mint(t, f.admin, hubBoundary(), "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, noProjectRead, http.MethodPut, path(f.topicA, inThread), edit).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, noProjectRead, http.MethodDelete, path(f.topicA, inThread), nil).Code)

	projB := f.mint(t, f.admin, projectBoundary(f.projB), "inbox:write", "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, projB, http.MethodPut, path(f.topicA, inThread), edit).Code)

	projAThread := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write", "project:read", "agent:read")
	assert.Equal(t, http.StatusOK, f.call(t, projAThread, http.MethodPut, path(f.topicA, inThread), edit).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, projAThread, http.MethodPut, path(dmB, inDM), edit).Code, "the DM peer agent is outside the boundary")
	assert.Equal(t, http.StatusForbidden, f.call(t, projAThread, http.MethodPut, path(f.topicA, notMine), edit).Code, "only the author edits")

	hubDM := f.mint(t, f.admin, hubBoundary(), "inbox:write", "agent:read")
	assert.Equal(t, http.StatusOK, f.call(t, hubDM, http.MethodPut, path(dmB, inDM), edit).Code)
	assert.Equal(t, http.StatusOK, f.call(t, hubDM, http.MethodDelete, path(dmB, inDM), nil).Code)

	rec := f.session(t, f.admin, http.MethodDelete, path(f.topicA, inThread), nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func chatDMKeys(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatDMListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	keys := []string{}
	for _, dm := range resp.DMs {
		keys = append(keys, dm.ConversationKey)
	}
	sort.Strings(keys)
	return keys
}

// TestChatDMListToken_FilteredToBoundary requires a token to carry
// inbox:read to list DMs, and lists for it only the DMs the DM rule lets
// it read.
func TestChatDMListToken_FilteredToBoundary(t *testing.T) {
	f := newChatFixture(t)
	dmA := f.dm(t, f.admin, "agent", f.agentA.ID)
	dmB := f.dm(t, f.admin, "agent", f.agentB.ID)
	dmU := f.dm(t, f.admin, "user", f.member)
	all := []string{dmA, dmB, dmU}
	sort.Strings(all)
	const path = "/api/v1/chat/dms"

	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, hubBoundary(), "agent:read"), http.MethodGet, path, nil).Code)
	assert.Equal(t, all, chatDMKeys(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:read", "agent:read"), http.MethodGet, path, nil)))
	assert.Equal(t, []string{dmA}, chatDMKeys(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "agent:read"), http.MethodGet, path, nil)))
	assert.Equal(t, []string{dmU}, chatDMKeys(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:read"), http.MethodGet, path, nil)), "agent DMs need agent:read")
	assert.Equal(t, all, chatDMKeys(t, f.session(t, f.admin, http.MethodGet, path, nil)))
}

// TestChatUserPrefsToken_RequiresHubBoundary requires a token to carry
// inbox:read to read the rail preferences and inbox:write to write them,
// on a hub boundary: the record has no project.
func TestChatUserPrefsToken_RequiresHubBoundary(t *testing.T) {
	f := newChatFixture(t)
	const path = "/api/v1/chat/user-prefs"
	body := map[string]string{"spaceSortMode": "alpha"}

	assert.Equal(t, http.StatusOK, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:read"), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read"), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, hubBoundary(), "project:read"), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:read"), http.MethodPut, path, body).Code)
	assert.Equal(t, http.StatusOK, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:write"), http.MethodPut, path, body).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write"), http.MethodPut, path, body).Code)
}

// TestChatPresence_EveryProjectRequiresProjectRead requires every caller
// to pass project:read on every project its presence names (a missing
// project is refused the same way), and a token to carry inbox:write.
func TestChatPresence_EveryProjectRequiresProjectRead(t *testing.T) {
	f := newChatFixture(t)
	const path = "/api/v1/chat/presence"
	body := func(ids ...string) map[string][]string { return map[string][]string{"projectIds": ids} }

	assert.Equal(t, http.StatusOK, f.session(t, f.owner, http.MethodPost, path, body(f.projA)).Code)
	assert.Equal(t, http.StatusForbidden, f.session(t, f.owner, http.MethodPost, path, body(f.projA, f.projB)).Code)
	assert.Equal(t, http.StatusForbidden, f.session(t, f.owner, http.MethodPost, path, body(uuid.NewString())).Code)

	assert.Equal(t, http.StatusOK, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:write", "project:read"), http.MethodPost, path, body(f.projA, f.projB)).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write", "project:read"), http.MethodPost, path, body(f.projB)).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, hubBoundary(), "project:read"), http.MethodPost, path, body(f.projA)).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, hubBoundary(), "inbox:write"), http.MethodPost, path, body(f.projA)).Code)
}

func chatSearchKeys(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatSearchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	keys := []string{}
	for _, r := range resp.Results {
		keys = append(keys, r.ConversationKey)
	}
	sort.Strings(keys)
	return keys
}

func chatSortedKeys(in ...string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestChatSearchToken_DMRowsNeedInboxRead requires a project or unscoped
// search by a token to keep a DM row only when the DM rule lets the token
// read the DM under inbox:read. A token without inbox:read gets no DM rows
// and no refusal. A DM-scoped search needs inbox:read.
func TestChatSearchToken_DMRowsNeedInboxRead(t *testing.T) {
	f := newChatFixture(t)
	dmA := f.dm(t, f.admin, "agent", f.agentA.ID)
	dmB := f.dm(t, f.admin, "agent", f.agentB.ID)
	f.chatMessage(t, "user:"+f.admin, f.admin, f.topicA, f.projA, "needle thread A")
	f.chatMessage(t, "user:"+f.admin, f.admin, f.topicB, f.projB, "needle thread B")
	f.chatMessage(t, "user:"+f.admin, f.admin, dmA, f.projA, "needle dm A")
	f.chatMessage(t, "user:"+f.admin, f.admin, dmB, f.projB, "needle dm B")
	project := "/api/v1/chat/search?q=needle&projectId=" + f.projA
	unscoped := "/api/v1/chat/search?q=needle"

	noInbox := f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "agent:read")
	assert.Equal(t, []string{f.topicA}, chatSearchKeys(t, f.call(t, noInbox, http.MethodGet, project, nil)))
	assert.Equal(t, []string{f.topicA}, chatSearchKeys(t, f.call(t, noInbox, http.MethodGet, unscoped, nil)))

	projA := f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "agent:read", "inbox:read")
	assert.Equal(t, chatSortedKeys(f.topicA, dmA), chatSearchKeys(t, f.call(t, projA, http.MethodGet, project, nil)))
	assert.Equal(t, chatSortedKeys(f.topicA, dmA), chatSearchKeys(t, f.call(t, projA, http.MethodGet, unscoped, nil)))

	hub := f.mint(t, f.admin, hubBoundary(), "project:read", "agent:read", "inbox:read")
	assert.Equal(t, chatSortedKeys(f.topicA, f.topicB, dmA, dmB), chatSearchKeys(t, f.call(t, hub, http.MethodGet, unscoped, nil)))

	hubNoAgentRead := f.mint(t, f.admin, hubBoundary(), "project:read", "inbox:read")
	assert.Equal(t, chatSortedKeys(f.topicA, f.topicB), chatSearchKeys(t, f.call(t, hubNoAgentRead, http.MethodGet, unscoped, nil)))

	assert.Equal(t, http.StatusForbidden, f.call(t, noInbox, http.MethodGet, "/api/v1/chat/search?q=needle&key="+dmA, nil).Code)
	assert.Equal(t, chatSortedKeys(f.topicA, f.topicB, dmA, dmB), chatSearchKeys(t, f.session(t, f.admin, http.MethodGet, unscoped, nil)))
}

// chatUpload posts one text file to the upload route with a bearer key
// (a token key, or testDevToken), for projectID when it is not empty.
func (f *chatFixture) chatUpload(t *testing.T, key, projectID string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if projectID != "" {
		require.NoError(t, mw.WriteField("project_id", projectID))
	}
	part, err := mw.CreatePart(map[string][]string{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="files"; filename=%q`, "note.txt")},
		"Content-Type":        {"text/plain"},
	})
	require.NoError(t, err)
	_, err = part.Write([]byte("chat token upload"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestChatProjectUpload_RequiresInboxWriteAndProjectRead requires a token
// uploading to a space to carry inbox:write and project:read, with the
// space's project inside its boundary.
func TestChatProjectUpload_RequiresInboxWriteAndProjectRead(t *testing.T) {
	f := newChatFixture(t)
	assert.Equal(t, http.StatusForbidden, f.chatUpload(t, f.mint(t, f.admin, hubBoundary(), "project:read"), f.projA).Code)
	assert.Equal(t, http.StatusForbidden, f.chatUpload(t, f.mint(t, f.admin, hubBoundary(), "inbox:write"), f.projA).Code)
	assert.Equal(t, http.StatusForbidden, f.chatUpload(t, f.mint(t, f.admin, projectBoundary(f.projB), "inbox:write", "project:read"), f.projA).Code)
	rec := f.chatUpload(t, f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write", "project:read"), f.projA)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestChatProjectlessUpload_RequiresHubBoundary requires a token uploading
// a project-less attachment to carry inbox:write on a hub boundary.
func TestChatProjectlessUpload_RequiresHubBoundary(t *testing.T) {
	f := newChatFixture(t)
	assert.Equal(t, http.StatusForbidden, f.chatUpload(t, f.mint(t, f.admin, hubBoundary(), "project:read"), "").Code)
	assert.Equal(t, http.StatusForbidden, f.chatUpload(t, f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write"), "").Code)
	rec := f.chatUpload(t, f.mint(t, f.admin, hubBoundary(), "inbox:write"), "")
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestChatAttachmentDownloadToken_ProjectFileNeedsProjectRead requires a
// token to pass project:read on the project of a project attachment,
// whoever uploaded it.
func TestChatAttachmentDownloadToken_ProjectFileNeedsProjectRead(t *testing.T) {
	f := newChatFixture(t)
	rec := f.chatUpload(t, f.mint(t, f.admin, hubBoundary(), "inbox:write", "project:read"), f.projA)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp struct {
		Attachments []attachmentUploadResult `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Attachments, 1)
	path := "/api/v1/chat/attachments/" + resp.Attachments[0].ID

	assert.Equal(t, http.StatusOK, f.call(t, f.mint(t, f.admin, hubBoundary(), "project:read"), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusOK, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "project:read"), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, f.mint(t, f.admin, projectBoundary(f.projB), "project:read"), http.MethodGet, path, nil).Code, "the uploader's own token is bounded too")
	assert.Equal(t, http.StatusNotFound, f.call(t, f.mint(t, f.admin, hubBoundary(), "agent:read"), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusOK, f.session(t, f.admin, http.MethodGet, path, nil).Code)
}

// TestChatToken_RefusedCredentialKinds requires the chat routes to refuse
// every credential other than a session, a dev login, a federated user and
// a user access token: a broker acting for a user, hub delivery, an agent
// credential and an unknown kind get 403 before the handler runs.
func TestChatToken_RefusedCredentialKinds(t *testing.T) {
	f := newChatFixture(t)
	user := NewAuthenticatedUser(f.admin, f.admin+"@test.com", "Admin", "admin", string(ClientTypeWeb))
	fed := NewFederatedUserIdentity("https://issuer.chat.test", "chat-fed", "chat-fed@test.com", "Fed", "member", nil)
	routes := []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/chat/user-prefs", f.srv.handleChatUserPrefs},
		{http.MethodGet, "/api/v1/chat/dms", f.srv.handleChatDMs},
		{http.MethodPost, "/api/v1/chat/presence", f.srv.handleChatPresence},
	}
	run := func(method, path string, handler http.HandlerFunc, identity Identity, cred *CredentialContext) int {
		req := httptest.NewRequest(method, path, nil)
		ctx := contextWithIdentity(req.Context(), identity)
		if cred != nil {
			ctx = contextWithCredentialContext(ctx, *cred)
		}
		rec := httptest.NewRecorder()
		f.srv.chatRoute(handler)(rec, req.WithContext(ctx))
		return rec.Code
	}
	for _, r := range routes {
		for _, kind := range []CredentialKind{CredentialKindBroker, CredentialKindHubDelivery, CredentialKindAgentJWT, CredentialKindFederation, CredentialKind("unknown-kind")} {
			assert.Equal(t, http.StatusForbidden, run(r.method, r.path, r.handler, user, &CredentialContext{Kind: kind}), "%s %s with %q", r.method, r.path, kind)
		}
		assert.Equal(t, http.StatusOK, run(r.method, r.path, r.handler, user, &CredentialContext{Kind: CredentialKindInteractive}), "%s %s with a session", r.method, r.path)
		assert.Equal(t, http.StatusOK, run(r.method, r.path, r.handler, user, nil), "%s %s with an identity and no credential record", r.method, r.path)
	}
	// A federated user keeps the chat reads; its profile writes stay
	// refused (TestChatProfileWrites_FederatedUserRefused).
	assert.Equal(t, http.StatusOK, run(http.MethodGet, "/api/v1/chat/dms", f.srv.handleChatDMs, fed, &CredentialContext{Kind: CredentialKindFederation}))
	assert.Equal(t, http.StatusOK, run(http.MethodGet, "/api/v1/chat/dms", f.srv.handleChatDMs, fed, nil))
}

// TestChatToken_ProjectMembershipRecheckedOnEveryRequest requires a
// project-boundary token's holder to be a current member of the boundary
// project for every chat route that takes an inbox selector: after the
// holder's project binding is removed, the same token is refused.
func TestChatToken_ProjectMembershipRecheckedOnEveryRequest(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	dmA := f.dm(t, f.owner, "agent", f.agentA.ID)
	f.chatMessage(t, "agent:"+f.agentA.Slug, f.agentA.ID, dmA, f.projA, "hello owner")
	tok := f.mint(t, f.owner, projectBoundary(f.projA), "inbox:read", "inbox:write", "agent:read", "project:read")

	type req struct {
		method, path string
		body         interface{}
	}
	reqs := []req{
		{http.MethodGet, chatConversationPath(dmA, "messages"), nil},
		{http.MethodGet, "/api/v1/chat/dms", nil},
		{http.MethodGet, "/api/v1/chat/unread-count", nil},
		{http.MethodPut, chatConversationPath(dmA, "mute"), map[string]bool{"muted": true}},
		{http.MethodPost, chatConversationPath(f.topicA, "typing"), nil},
		{http.MethodGet, "/api/v1/chat/prefs?agentId=" + f.agentA.ID, nil},
	}
	for _, r := range reqs {
		rec := f.call(t, tok, r.method, r.path, r.body)
		assert.Equal(t, http.StatusOK, rec.Code, "before removal: %s %s: %s", r.method, r.path, rec.Body.String())
	}

	bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.owner)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.projA {
			require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.Equal(t, 1, removed)

	for _, r := range reqs {
		rec := f.call(t, tok, r.method, r.path, r.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "after removal: %s %s: %s", r.method, r.path, rec.Body.String())
	}
}
