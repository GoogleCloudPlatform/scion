//go:build !no_sqlite

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

package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// starFixture is a chatFixture with the starred-messages experiment set
// and an event publisher.
type starFixture struct {
	*chatFixture
	ep    *ChannelEventPublisher
	stars MessageStarStore
	db    *sql.DB
}

// setStarsExperiment sets web.chat_starred_messages and the conversation
// read switch through operational settings.
func setStarsExperiment(t *testing.T, srv *Server, enabled, readSwitch bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.ChatStarredMessages, enabled)))
	fakeStore.seed("messaging", json.RawMessage(fmt.Sprintf(`{"conversation_envelope_switch":%t}`, readSwitch)))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	if ops := srv.GetOperationalSettings(); ops != nil {
		require.Equal(t, readSwitch, ops.ConversationEnvelopeSwitch())
	}
	require.Equal(t, enabled, srv.experimentEnabled(experiments.ChatStarredMessages))
}

func newStarFixture(t *testing.T, readSwitch bool) *starFixture {
	t.Helper()
	f := &starFixture{chatFixture: newChatFixture(t)}
	f.ep = NewChannelEventPublisher()
	t.Cleanup(f.ep.Close)
	f.srv.SetEventPublisher(f.ep)
	f.stars = messageStarStoreFrom(f.wcs)
	require.NotNil(t, f.stars, "both real chat stores implement MessageStarStore")
	db, ok := f.s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	f.db = db.DB()
	setStarsExperiment(t, f.srv, true, readSwitch)
	return f
}

// threadMsg stores a native web message in topic (with its conversation
// ID, so it is in history under both read-switch modes) created at.
func (f *starFixture) threadMsg(t *testing.T, topic, projectID, text string, at time.Time) string {
	t.Helper()
	return f.threadMsgOn(t, topic, projectID, text, "web", at)
}

func (f *starFixture) threadMsgOn(t *testing.T, topic, projectID, text, channel string, at time.Time) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, f.s.CreateMessage(context.Background(), &store.Message{
		ID: id, ProjectID: projectID, Sender: "user:" + f.admin + "@test.com", SenderID: f.admin, Recipient: "chat",
		Msg: text, Type: "chat", Channel: channel, ThreadID: topic, ConversationID: topicConversationID(t, f.wcs, topic),
		CreatedAt: at.UTC(),
	}))
	return id
}

func starPath(id string) string { return "/api/v1/chat/stars/" + id }

func starListPath(key string, extra ...string) string {
	p := "/api/v1/chat/stars?conversationKey=" + url.QueryEscape(key)
	for _, e := range extra {
		p += "&" + e
	}
	return p
}

func starStatePath(ids ...string) string {
	q := url.Values{}
	for _, id := range ids {
		q.Add("messageId", id)
	}
	return "/api/v1/chat/stars/state?" + q.Encode()
}

func (f *starFixture) put(t *testing.T, userID, msgID, key string) *httptest.ResponseRecorder {
	t.Helper()
	return f.session(t, userID, http.MethodPut, starPath(msgID), map[string]string{"conversationKey": key})
}

func decodeStarPut(t *testing.T, rec *httptest.ResponseRecorder) starPutResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp starPutResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func decodeStarPage(t *testing.T, rec *httptest.ResponseRecorder) starPageResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp starPageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func decodeStarState(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp starStateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.StarredMessageIDs
}

func starPageIDs(p starPageResponse) []string {
	ids := []string{}
	for _, it := range p.Items {
		ids = append(ids, it.MessageID)
	}
	return ids
}

// starEvents returns the subjects of the star events received on ch.
func starEvents(t *testing.T, ch <-chan Event) []string {
	t.Helper()
	var subjects []string
	for _, e := range collectEvents(ch) {
		var evt map[string]any
		require.NoError(t, json.Unmarshal(e.Data, &evt))
		assert.Equal(t, map[string]any{"action": "changed"}, evt, "the payload is content-free")
		subjects = append(subjects, e.Subject)
	}
	return subjects
}

func TestChatStars_ExperimentOffIs404(t *testing.T) {
	f := newStarFixture(t, true)
	msg := f.threadMsg(t, f.topicA, f.projA, "hello", time.Now())
	setStarsExperiment(t, f.srv, false, true)
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, starPath(msg)},
		{http.MethodDelete, starPath(msg)},
		{http.MethodGet, starPath(msg)},
		{http.MethodGet, starStatePath(msg)},
		{http.MethodGet, starListPath(f.topicA)},
		{http.MethodPost, "/api/v1/chat/stars"},
	} {
		rec := f.session(t, f.admin, c.method, c.path, map[string]string{"conversationKey": f.topicA})
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
	}
	// Stored stars are kept while it is off.
	_, _, err := f.stars.AddMessageStar(context.Background(), f.admin, StarTarget{MessageID: msg, ProjectID: f.projA,
		Selector: StarSelector{ConversationKey: f.topicA, Channel: "web", ThreadID: f.topicA}})
	require.NoError(t, err)
	setStarsExperiment(t, f.srv, true, true)
	assert.Equal(t, []string{msg}, decodeStarState(t, f.session(t, f.admin, http.MethodGet, starStatePath(msg), nil)))
}

func TestChatStars_NoCapableStoreIs503(t *testing.T) {
	f := newStarFixture(t, true)
	f.srv.SetWebChatStore(starlessWebChatStore{f.wcs})
	assert.NotEmpty(t, f.srv.messageStarStoreProblem())
	rec := f.session(t, f.admin, http.MethodGet, starListPath(f.topicA), nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	f.srv.SetWebChatStore(f.wcs)
	assert.Empty(t, f.srv.messageStarStoreProblem())
}

// starlessWebChatStore is a chat store double that does not opt in to
// MessageStarStore.
type starlessWebChatStore struct{ WebChatStore }

func TestChatStars_PutIdempotentAndPrivate(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarFixture(t, readSwitch)
			ch, unsub := f.ep.Subscribe("user.*.chat.stars")
			defer unsub()

			// An old message, far outside the latest history page.
			old := f.threadMsg(t, f.topicA, f.projA, "old message", time.Now().Add(-48*time.Hour))
			for i := 0; i < 60; i++ {
				f.threadMsg(t, f.topicA, f.projA, fmt.Sprintf("filler %d", i), time.Now().Add(-time.Duration(60-i)*time.Minute))
			}

			first := decodeStarPut(t, f.put(t, f.admin, old, f.topicA))
			assert.True(t, first.Starred)
			assert.Equal(t, []string{"user." + f.admin + ".chat.stars"}, starEvents(t, ch), "one invalidation, to the owner only")

			again := decodeStarPut(t, f.put(t, f.admin, old, f.topicA))
			assert.True(t, again.StarredAt.Equal(first.StarredAt), "a repeated PUT keeps the save time")
			assert.Empty(t, starEvents(t, ch), "a no-op PUT publishes nothing")

			page := decodeStarPage(t, f.session(t, f.admin, http.MethodGet, starListPath(f.topicA), nil))
			assert.Equal(t, []string{old}, starPageIDs(page), "one association")

			// The other user (a member of the same project) has no star.
			assert.Empty(t, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(old), nil)))
			assert.Empty(t, decodeStarPage(t, f.session(t, f.member, http.MethodGet, starListPath(f.topicA), nil)).Items)
			assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodGet, starPath(old), nil).Code)
			assert.Equal(t, []string{old}, decodeStarState(t, f.session(t, f.admin, http.MethodGet, starStatePath(old), nil)))

			// The other user's DELETE never touches the owner's star.
			assert.Equal(t, http.StatusNoContent, f.session(t, f.member, http.MethodDelete, starPath(old), nil).Code)
			assert.Empty(t, starEvents(t, ch))
			assert.Equal(t, []string{old}, decodeStarState(t, f.session(t, f.admin, http.MethodGet, starStatePath(old), nil)))

			// Repeated DELETE stays 204; only the first publishes.
			assert.Equal(t, http.StatusNoContent, f.session(t, f.admin, http.MethodDelete, starPath(old), nil).Code)
			assert.Equal(t, []string{"user." + f.admin + ".chat.stars"}, starEvents(t, ch))
			assert.Equal(t, http.StatusNoContent, f.session(t, f.admin, http.MethodDelete, starPath(old), nil).Code)
			assert.Empty(t, starEvents(t, ch))
			assert.Empty(t, decodeStarState(t, f.session(t, f.admin, http.MethodGet, starStatePath(old), nil)))

			// Unstar then re-star creates a fresh save time.
			time.Sleep(2 * time.Millisecond)
			restar := decodeStarPut(t, f.put(t, f.admin, old, f.topicA))
			assert.True(t, restar.StarredAt.After(first.StarredAt))
		})
	}
}

func TestChatStars_PutValidation(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	msg := f.threadMsg(t, f.topicA, f.projA, "in A", time.Now())
	msgB := f.threadMsg(t, f.topicB, f.projB, "in B", time.Now())
	other := f.threadMsgOn(t, f.topicA, f.projA, "from another channel", "telegram", time.Now())
	deleted := f.threadMsg(t, f.topicA, f.projA, "deleted", time.Now())
	require.NoError(t, f.wcs.SetMessageDeleted(ctx, deleted, time.Now()))

	cases := []struct {
		name string
		user string
		path string
		body interface{}
		want int
	}{
		{"bad message id", f.admin, starPath("not-a-uuid"), map[string]string{"conversationKey": f.topicA}, http.StatusBadRequest},
		{"missing key", f.admin, starPath(msg), map[string]string{}, http.StatusBadRequest},
		{"unknown field", f.admin, starPath(msg), json.RawMessage(`{"conversationKey":"` + f.topicA + `","userId":"x"}`), http.StatusBadRequest},
		{"not an object", f.admin, starPath(msg), "text", http.StatusBadRequest},
		{"unknown query", f.admin, starPath(msg) + "?x=1", map[string]string{"conversationKey": f.topicA}, http.StatusBadRequest},
		{"DM key (phase 1)", f.admin, starPath(msg), map[string]string{"conversationKey": "dm:user:" + f.admin + ":agent:" + f.agentA.ID}, http.StatusNotFound},
		{"missing thread", f.admin, starPath(msg), map[string]string{"conversationKey": uuid.NewString()}, http.StatusNotFound},
		{"wrong conversation", f.admin, starPath(msgB), map[string]string{"conversationKey": f.topicA}, http.StatusNotFound},
		{"missing message", f.admin, starPath(uuid.NewString()), map[string]string{"conversationKey": f.topicA}, http.StatusNotFound},
		{"other channel", f.admin, starPath(other), map[string]string{"conversationKey": f.topicA}, http.StatusNotFound},
		{"deleted message", f.admin, starPath(deleted), map[string]string{"conversationKey": f.topicA}, http.StatusNotFound},
		{"unreadable thread", f.member, starPath(msgB), map[string]string{"conversationKey": f.topicB}, http.StatusNotFound},
	}
	for _, c := range cases {
		rec := f.session(t, c.user, http.MethodPut, c.path, c.body)
		assert.Equal(t, c.want, rec.Code, "%s: %s", c.name, rec.Body.String())
	}
	rec := f.session(t, f.admin, http.MethodPost, starPath(msg), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	rec = f.session(t, f.admin, http.MethodPut, "/api/v1/chat/stars", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	rec = f.session(t, f.admin, http.MethodPost, "/api/v1/chat/stars/state", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	// A deleted topic is gone for stars too.
	f.topic(t, f.projA) // a project keeps at least one thread
	require.NoError(t, f.wcs.DeleteTopic(ctx, f.topicA))
	assert.Equal(t, http.StatusNotFound, f.put(t, f.admin, msg, f.topicA).Code)
	assert.Empty(t, decodeStarState(t, f.session(t, f.admin, http.MethodGet, starStatePath(msg, other, deleted, msgB), nil)))
}

func TestChatStars_DeleteAlways204(t *testing.T) {
	f := newStarFixture(t, true)
	assert.Equal(t, http.StatusNoContent, f.session(t, f.admin, http.MethodDelete, starPath(uuid.NewString()), nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.session(t, f.admin, http.MethodDelete, starPath("nope"), nil).Code)

	// Removing works after the message is deleted and after access is lost.
	ctx := context.Background()
	msg := f.threadMsg(t, f.topicA, f.projA, "will go", time.Now())
	decodeStarPut(t, f.put(t, f.member, msg, f.topicA))
	require.NoError(t, f.wcs.SetMessageDeleted(ctx, msg, time.Now()))
	assert.Equal(t, http.StatusNoContent, f.session(t, f.member, http.MethodDelete, starPath(msg), nil).Code)
	rec, err := f.stars.GetMessageStar(ctx, f.member, msg)
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestChatStars_ListPaginates(t *testing.T) {
	f := newStarFixture(t, true)
	var ids []string
	for i := 0; i < 5; i++ {
		id := f.threadMsg(t, f.topicA, f.projA, fmt.Sprintf("  message\n number   %d  ", i), time.Now().Add(-time.Duration(10-i)*time.Hour))
		decodeStarPut(t, f.put(t, f.admin, id, f.topicA))
		ids = append(ids, id)
		time.Sleep(time.Millisecond)
	}
	// Ties on starred_at order by message ID.
	_, err := f.db.Exec(`UPDATE webchat_message_star SET starred_at = ? WHERE message_id IN (?, ?)`,
		sqliteStarTime(time.Now().Add(time.Hour)), ids[0], ids[1])
	require.NoError(t, err)
	tied := []string{ids[0], ids[1]}
	if tied[0] < tied[1] {
		tied[0], tied[1] = tied[1], tied[0]
	}
	want := []string{tied[0], tied[1], ids[4], ids[3], ids[2]}

	var got []string
	cursor := ""
	for page := 0; page < 5; page++ {
		path := starListPath(f.topicA, "limit=2")
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		p := decodeStarPage(t, f.session(t, f.admin, http.MethodGet, path, nil))
		assert.LessOrEqual(t, len(p.Items), 2)
		got = append(got, starPageIDs(p)...)
		if p.NextCursor == "" {
			break
		}
		assert.True(t, strings.HasPrefix(p.NextCursor, listCursorPrefix), "sealed cursor")
		cursor = p.NextCursor
	}
	assert.Equal(t, want, got, "reverse save order, keyset over ties, no duplicates")

	first := decodeStarPage(t, f.session(t, f.admin, http.MethodGet, starListPath(f.topicA), nil))
	require.Len(t, first.Items, 5)
	assert.Empty(t, first.NextCursor)
	it := first.Items[2]
	assert.Equal(t, ids[4], it.MessageID)
	assert.Equal(t, "message number 4", it.Excerpt, "whitespace collapsed")
	assert.Equal(t, "thread", it.ConversationKind)
	assert.Equal(t, f.topicA, it.ConversationKey)
	assert.Equal(t, f.projA, it.ProjectID)
	assert.Equal(t, "Inbox A", it.ProjectName)
	assert.NotEmpty(t, it.ConversationName)
	assert.NotEmpty(t, it.SenderName)
	assert.False(t, it.Edited)
	assert.Equal(t, "/chat/space/"+f.projA+"/thread/"+f.topicA+"#msg-"+ids[4], it.Permalink)

	// Cursor binding: another caller, another conversation, a tampered
	// cursor and a plaintext cursor are all invalid.
	p := decodeStarPage(t, f.session(t, f.admin, http.MethodGet, starListPath(f.topicA, "limit=1"), nil))
	require.NotEmpty(t, p.NextCursor)
	c := url.QueryEscape(p.NextCursor)
	assert.Equal(t, http.StatusBadRequest, f.session(t, f.owner, http.MethodGet, starListPath(f.topicA, "cursor="+c), nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.session(t, f.admin, http.MethodGet, starListPath(f.topicB, "cursor="+c), nil).Code)
	tampered := p.NextCursor[:len(p.NextCursor)-2] + "AA"
	assert.Equal(t, http.StatusBadRequest, f.session(t, f.admin, http.MethodGet, starListPath(f.topicA, "cursor="+url.QueryEscape(tampered)), nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.session(t, f.admin, http.MethodGet, starListPath(f.topicA, "cursor=abc"), nil).Code)
	// The page size may change between pages.
	assert.Equal(t, http.StatusOK, f.session(t, f.admin, http.MethodGet, starListPath(f.topicA, "limit=3", "cursor="+c), nil).Code)

	for _, bad := range []string{
		"/api/v1/chat/stars",
		starListPath(f.topicA, "limit=0"),
		starListPath(f.topicA, "limit=101"),
		starListPath(f.topicA, "limit=x"),
		starListPath(f.topicA, "projectId="+f.projA),
		starListPath(f.topicA, "conversationKey="+f.topicB),
		starListPath(f.topicA, "limit=1", "limit=2"),
	} {
		assert.Equal(t, http.StatusBadRequest, f.session(t, f.admin, http.MethodGet, bad, nil).Code, bad)
	}
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodGet, starListPath(f.topicB), nil).Code, "unreadable thread")
	assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodGet, starListPath("dm:user:a:user:b"), nil).Code)
}

func TestChatStars_StateFiltersByAccess(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	a1 := f.threadMsg(t, f.topicA, f.projA, "a1", time.Now())
	a2 := f.threadMsg(t, f.topicA, f.projA, "a2", time.Now())
	b1 := f.threadMsg(t, f.topicB, f.projB, "b1", time.Now())
	decodeStarPut(t, f.put(t, f.member, a1, f.topicA))

	// A retained association in a thread the member cannot read (stored
	// directly: the API would refuse it) is hidden from every read.
	_, _, err := f.stars.AddMessageStar(ctx, f.member, StarTarget{MessageID: b1, ProjectID: f.projB,
		Selector: StarSelector{ConversationKey: f.topicB, Channel: "web", ThreadID: f.topicB}})
	require.NoError(t, err)

	assert.Equal(t, []string{a1}, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(a2, b1, a1), nil)))
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodGet, starPath(b1), nil).Code)

	// Regaining access shows the retained association again.
	createTestUserWithProjectRole(t, f.s, f.member, f.member+"@test.com", f.projB, store.ProjectRoleMember)
	assert.ElementsMatch(t, []string{a1, b1}, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(a2, b1, a1), nil)))

	// A deleted message disappears from state at once.
	require.NoError(t, f.wcs.SetMessageDeleted(ctx, a1, time.Now()))
	assert.Equal(t, []string{b1}, decodeStarState(t, f.session(t, f.member, http.MethodGet, starStatePath(a1, b1), nil)))

	many := make([]string, 201)
	for i := range many {
		many[i] = uuid.NewString()
	}
	for _, bad := range []string{
		"/api/v1/chat/stars/state",
		starStatePath(many...),
		starStatePath(a1, a1),
		starStatePath("nope"),
		starStatePath(a1) + "&conversationKey=" + f.topicA,
	} {
		assert.Equal(t, http.StatusBadRequest, f.session(t, f.member, http.MethodGet, bad, nil).Code, bad)
	}
	assert.Equal(t, http.StatusOK, f.session(t, f.member, http.MethodGet, starStatePath(many[:200]...), nil).Code)
}

func TestChatStars_DetailAndListHideUnreadable(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	msg := f.threadMsg(t, f.topicA, f.projA, "detail me", time.Now().Add(-time.Hour))
	put := decodeStarPut(t, f.put(t, f.admin, msg, f.topicA))

	rec := f.session(t, f.admin, http.MethodGet, starPath(msg), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var item starItemResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &item))
	assert.Equal(t, msg, item.MessageID)
	assert.True(t, item.StarredAt.Equal(put.StarredAt))
	assert.Equal(t, "detail me", item.Excerpt)

	// An edit keeps the star and marks the item edited.
	require.NoError(t, f.wcs.SetMessageEdited(ctx, msg, time.Now()))
	rec = f.session(t, f.admin, http.MethodGet, starPath(msg), nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &item))
	assert.True(t, item.Edited)

	unstarred := f.threadMsg(t, f.topicA, f.projA, "never starred", time.Now())
	assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodGet, starPath(unstarred), nil).Code)

	require.NoError(t, f.wcs.SetMessageDeleted(ctx, msg, time.Now()))
	assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodGet, starPath(msg), nil).Code)
	assert.Empty(t, decodeStarPage(t, f.session(t, f.admin, http.MethodGet, starListPath(f.topicA), nil)).Items)
}

func TestChatStars_TokenSelectors(t *testing.T) {
	f := newStarFixture(t, true)
	ctx := context.Background()
	msg := f.threadMsg(t, f.topicA, f.projA, "token", time.Now())
	body := map[string]string{"conversationKey": f.topicA}

	readOnly := f.mint(t, f.admin, hubBoundary(), "inbox:read", "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPut, starPath(msg), body).Code, "PUT needs inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodDelete, starPath(msg), nil).Code, "DELETE needs inbox:write")

	noProjectRead := f.mint(t, f.admin, hubBoundary(), "inbox:write")
	assert.Equal(t, http.StatusNotFound, f.call(t, noProjectRead, http.MethodPut, starPath(msg), body).Code, "the thread read check applies")

	writer := f.mint(t, f.admin, hubBoundary(), "inbox:write", "project:read")
	decodeStarPut(t, f.call(t, writer, http.MethodPut, starPath(msg), body))

	writeOnly := f.mint(t, f.admin, hubBoundary(), "inbox:write", "project:read")
	for _, p := range []string{starPath(msg), starStatePath(msg), starListPath(f.topicA)} {
		assert.Equal(t, http.StatusForbidden, f.call(t, writeOnly, http.MethodGet, p, nil).Code, "reads need inbox:read: %s", p)
	}
	for _, p := range []string{starPath(msg), starStatePath(msg), starListPath(f.topicA)} {
		assert.Equal(t, http.StatusOK, f.call(t, readOnly, http.MethodGet, p, nil).Code, p)
	}

	// A project token for B does not see or add A's stars, and its DELETE
	// leaves the A star in place with the same empty 204.
	readerB := f.mint(t, f.admin, projectBoundary(f.projB), "inbox:read", "project:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, readerB, http.MethodGet, starPath(msg), nil).Code)
	assert.Equal(t, http.StatusNotFound, f.call(t, readerB, http.MethodGet, starListPath(f.topicA), nil).Code)
	assert.Empty(t, decodeStarState(t, f.call(t, readerB, http.MethodGet, starStatePath(msg), nil)))
	writerB := f.mint(t, f.admin, projectBoundary(f.projB), "inbox:write", "project:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, writerB, http.MethodPut, starPath(msg), body).Code)
	assert.Equal(t, http.StatusNoContent, f.call(t, writerB, http.MethodDelete, starPath(msg), nil).Code)
	rec, err := f.stars.GetMessageStar(ctx, f.admin, msg)
	require.NoError(t, err)
	assert.NotNil(t, rec, "out-of-boundary DELETE removes nothing")

	writerA := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write", "project:read")
	assert.Equal(t, http.StatusNoContent, f.call(t, writerA, http.MethodDelete, starPath(msg), nil).Code)
	rec, err = f.stars.GetMessageStar(ctx, f.admin, msg)
	require.NoError(t, err)
	assert.Nil(t, rec)
}

// TestChatHistory_ReadTargetResolverRegression pins the history semantics
// the shared read-target resolver must keep: missing and unreadable
// threads are "Thread not found", the channel restriction and the
// selector for both read-switch modes, and the around anchor check.
func TestChatHistory_ReadTargetResolverRegression(t *testing.T) {
	for _, readSwitch := range []bool{true, false} {
		t.Run(fmt.Sprintf("readSwitch=%t", readSwitch), func(t *testing.T) {
			f := newStarFixture(t, readSwitch)
			web := f.threadMsg(t, f.topicA, f.projA, "web", time.Now().Add(-time.Minute))
			other := f.threadMsgOn(t, f.topicA, f.projA, "telegram", "telegram", time.Now())
			inB := f.threadMsg(t, f.topicB, f.projB, "b", time.Now())
			path := chatConversationPath(f.topicA, "messages")

			rec := f.session(t, f.admin, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp chatHistoryResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			var got []string
			for _, m := range resp.Messages {
				got = append(got, m.ID)
			}
			assert.Equal(t, []string{web}, got, "web channel only")

			for _, c := range []struct {
				user, path string
			}{
				{f.member, chatConversationPath(f.topicB, "messages")},
				{f.admin, chatConversationPath(uuid.NewString(), "messages")},
				{f.admin, path + "?around=" + other},
				{f.admin, path + "?around=" + inB},
			} {
				rec := f.session(t, c.user, http.MethodGet, c.path, nil)
				assert.Equal(t, http.StatusNotFound, rec.Code, c.path)
				if strings.Contains(c.path, "around") {
					assert.Contains(t, rec.Body.String(), "Message")
				} else {
					assert.Contains(t, rec.Body.String(), "Thread")
				}
			}
			rec = f.session(t, f.admin, http.MethodGet, path+"?around="+web, nil)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}
