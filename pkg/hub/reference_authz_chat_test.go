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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// refFixture is two projects with one owner each, a topic in each, and a
// third user who belongs to neither project. All three are hub members.
//
//	ua (alice): owner of A, no role in B
//	ub (bob):   owner of B, no role in A
//	uc (carol): no role in A or B
type refFixture struct {
	srv          *Server
	st           store.Store
	wcs          WebChatStore
	ua, ub, uc   *store.User
	projA, projB *store.Project
	topicA       string
	topicB       string
}

func newRefFixture(t *testing.T) *refFixture {
	t.Helper()
	srv, st, _, _, projA := setupDemoPolicyTest(t)
	db := openTestMemorySQLite(t, "sqlite3")
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	ctx := context.Background()

	ua, err := st.GetUser(ctx, tid("user-alice"))
	require.NoError(t, err)
	ub, err := st.GetUser(ctx, tid("user-bob"))
	require.NoError(t, err)

	uc := &store.User{
		ID:          tid("ref-user-carol"),
		Email:       "carol@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, uc))
	ensureHubMembership(ctx, st, uc.ID)

	projB := &store.Project{
		ID:        tid("ref-project-b"),
		Name:      "Project B",
		Slug:      "project-b",
		OwnerID:   ub.ID,
		CreatedBy: ub.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, projB))
	srv.seedProjectCreatorMembership(ctx, projB)

	f := &refFixture{
		srv: srv, st: st, wcs: wcs,
		ua: ua, ub: ub, uc: uc,
		projA: projA, projB: projB,
		topicA: tid("ref-topic-a"),
		topicB: tid("ref-topic-b"),
	}
	for _, tp := range []struct {
		id, project, by string
	}{{f.topicA, projA.ID, ua.ID}, {f.topicB, projB.ID, ub.ID}} {
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: tp.id, ProjectID: tp.project, Name: "topic-" + tp.id[:8],
			CreatedBy: tp.by, CreatedAt: time.Now().UTC(),
		}))
		setTopicConversationID(t, db, st, tp.id, tp.project)
	}
	return f
}

// seedMessage stores a web chat message on thread directly.
func (f *refFixture) seedMessage(t *testing.T, projectID, thread, convID, text string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, f.st.CreateMessage(context.Background(), &store.Message{
		ID: id, ProjectID: projectID,
		Sender: "user:seed", SenderID: f.ua.ID, Recipient: "thread:" + thread,
		Msg: text, Type: "instruction", Channel: "web",
		ThreadID: thread, ConversationID: convID,
		CreatedAt: time.Now().UTC(),
	}))
	return id
}

// threadMessageCount counts the web messages stored on thread.
func (f *refFixture) threadMessageCount(t *testing.T, thread string) int {
	t.Helper()
	res, err := f.st.ListMessages(context.Background(),
		store.MessageFilter{Channel: "web", ThreadID: thread}, store.ListOptions{Limit: 200})
	require.NoError(t, err)
	return len(res.Items)
}

// refAnswer is one HTTP answer: status and body.
type refAnswer struct {
	status int
	body   string
}

// requireSameAnswer asserts that two requests got exactly the same answer.
func requireSameAnswer(t *testing.T, want, got refAnswer) {
	t.Helper()
	assert.Equal(t, want.status, got.status, "status must match:\n  want %d %s\n  got  %d %s", want.status, want.body, got.status, got.body)
	assert.Equal(t, want.body, got.body, "body must match")
}

func (f *refFixture) send(t *testing.T, user *store.User, key string, body map[string]interface{}) refAnswer {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages", body)
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

func dmKeyFor(t *testing.T, kindA, idA, kindB, idB string) string {
	t.Helper()
	key, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	require.NoError(t, err)
	return key
}

func TestChatReply_TargetMustBeInSameConversation(t *testing.T) {
	f := newRefFixture(t)

	otherProjectMsg := f.seedMessage(t, f.projB.ID, f.topicB, "", "in project B")
	otherDM := dmKeyFor(t, "user", f.ub.ID, "user", f.uc.ID)
	otherDMMsg := f.seedMessage(t, f.projB.ID, otherDM, "", "in another DM")
	sameTopicMsg := f.seedMessage(t, f.projA.ID, f.topicA, "", "in topic A")

	before := f.threadMessageCount(t, f.topicA)

	missing := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": uuid.NewString()})
	require.Equal(t, http.StatusBadRequest, missing.status, missing.body)
	assert.Contains(t, missing.body, "reply_to_id does not refer to a message in this conversation")

	for name, id := range map[string]string{"other project topic": otherProjectMsg, "other DM": otherDMMsg} {
		t.Run(name, func(t *testing.T) {
			got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": id})
			requireSameAnswer(t, missing, got)
		})
	}
	assert.Equal(t, before, f.threadMessageCount(t, f.topicA), "a refused reply must not store a message")

	ok := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": sameTopicMsg})
	require.Equal(t, http.StatusCreated, ok.status, ok.body)

	hist := f.history(t, f.ua, f.topicA)
	assert.Contains(t, hist.ReplyPreviews, sameTopicMsg, "a same-conversation reply keeps its preview")
}

func TestChatReply_TargetMatchedByConversationWhenSwitchOn(t *testing.T) {
	f := newRefFixture(t)
	enableReadSwitch(t, f.srv)

	first := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "first"})
	require.Equal(t, http.StatusCreated, first.status, first.body)

	convID, err := f.srv.conversationIDForKey(context.Background(), f.wcs, f.topicA)
	require.NoError(t, err)
	require.NotEmpty(t, convID, "the topic must have a conversation once a message is sent")

	// Same conversation, stored under a different thread key.
	target := f.seedMessage(t, f.projA.ID, "legacy-thread-key", convID, "same conversation")

	got := f.send(t, f.ua, f.topicA, map[string]interface{}{"content": "re", "reply_to_id": target})
	require.Equal(t, http.StatusCreated, got.status, got.body)
}

func TestChatHistory_ReplyPreviewsOnlyFromSameConversation(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	foreign := f.seedMessage(t, f.projB.ID, f.topicB, "", "text of project B")
	local := f.seedMessage(t, f.projA.ID, f.topicA, "", "text of topic A")
	replyForeign := f.seedMessage(t, f.projA.ID, f.topicA, "", "reply 1")
	replyLocal := f.seedMessage(t, f.projA.ID, f.topicA, "", "reply 2")
	// Written directly, as a row stored before the send-time check.
	require.NoError(t, f.wcs.SetMessageReplyTo(ctx, replyForeign, foreign))
	require.NoError(t, f.wcs.SetMessageReplyTo(ctx, replyLocal, local))

	hist := f.history(t, f.ua, f.topicA)
	assert.NotContains(t, hist.ReplyPreviews, foreign, "a message of another conversation is never previewed")
	assert.Contains(t, hist.ReplyPreviews, local, "a same-conversation preview is still shown")
}

func (f *refFixture) history(t *testing.T, user *store.User, key string) chatHistoryResponse {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestChatSendRefusal_ReasonLoggedNotReturned(t *testing.T) {
	logs := captureSlog(t)
	f := newRefFixture(t)

	unknown := f.send(t, f.ub, uuid.NewString(), map[string]interface{}{"content": "hi"})
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)

	// bob is a hub member with no role in project A.
	logs.Reset()
	got := f.send(t, f.ub, f.topicA, map[string]interface{}{"content": "hi"})
	requireSameAnswer(t, unknown, got)
	logged := logs.String()
	assert.Contains(t, logged, "authorization denied", "the denial must be logged")
	assert.Contains(t, logged, "chat send refused as not found")
	assert.Contains(t, logged, f.ub.ID, "the log names the caller")
	for _, word := range []string{"denied", "permission", "resource_type", "reason"} {
		assert.NotContains(t, got.body, word, "the response carries no reason")
	}

	// carol is not a participant of the alice/bob DM.
	logs.Reset()
	dm := dmKeyFor(t, "user", f.ua.ID, "user", f.ub.ID)
	got = f.send(t, f.uc, dm, map[string]interface{}{"content": "hi"})
	requireSameAnswer(t, unknown, got)
	assert.Contains(t, logs.String(), "not a participant of this DM")

	// History answers the same way.
	getAnswer := func(key string) refAnswer {
		rec := doRequestAsUser(t, f.srv, f.ub, http.MethodGet, "/api/v1/chat/conversations/"+key+"/messages", nil)
		return refAnswer{status: rec.Code, body: rec.Body.String()}
	}
	missingHist := getAnswer(uuid.NewString())
	require.Equal(t, http.StatusNotFound, missingHist.status, missingHist.body)
	requireSameAnswer(t, missingHist, getAnswer(f.topicA))
	requireSameAnswer(t, missingHist, getAnswer(dmKeyFor(t, "user", f.ua.ID, "user", f.uc.ID)))
}

func TestChatDMHistory_StaysReadableAfterProjectAccessEnds(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	// An agent of project B, which carol cannot read (any more).
	agent := &store.Agent{ID: tid("ref-agent-b"), ProjectID: f.projB.ID, Name: "ab", Slug: "ab",
		Phase: "running", OwnerID: f.ub.ID, CreatedBy: f.ub.ID}
	require.NoError(t, f.st.CreateAgent(ctx, agent))
	require.False(t, f.srv.canReadProject(ctx, NewAuthenticatedUser(f.uc.ID, f.uc.Email, f.uc.DisplayName, f.uc.Role, string(ClientTypeWeb)), f.projB.ID))

	dm := dmKeyFor(t, "agent", agent.ID, "user", f.uc.ID)
	f.seedMessage(t, f.projB.ID, dm, "", "earlier DM message")

	hist := f.history(t, f.uc, dm)
	assert.NotEmpty(t, hist.Messages, "the user's own DM history with the agent stays readable")
}
