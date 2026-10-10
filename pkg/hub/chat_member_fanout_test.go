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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memberFanoutFixture is a project owned by alice with a thread whose
// conversation has these user participants:
//   - carol: project member, active participant (receives)
//   - dave: project member, left the thread (does not receive)
//   - bob: active participant row but not a project member (does not receive)
//   - sam: project member, active participant, suspended (does not receive)
//
// and erin, a project member who is not a participant (does not receive).
type memberFanoutFixture struct {
	srv                          *Server
	s                            store.Store
	wcs                          WebChatStore
	ep                           *ChannelEventPublisher
	proj                         *store.Project
	alice, bob, carol, dave, sam *store.User
	erin                         *store.User
	topicID, convID              string
}

func newMemberFanoutFixture(t *testing.T) *memberFanoutFixture {
	t.Helper()
	srv, s, alice, bob, proj := setupDemoPolicyTest(t)
	return newMemberFanoutFixtureOn(t, srv, s, alice, bob, proj)
}

func newMemberFanoutFixtureOn(t *testing.T, srv *Server, s store.Store, alice, bob *store.User, proj *store.Project) *memberFanoutFixture {
	t.Helper()
	ctx := context.Background()

	ep := NewChannelEventPublisher()
	t.Cleanup(ep.Close)
	srv.SetEventPublisher(ep)

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	member := func(name string) *store.User {
		u := &store.User{
			ID: api.NewUUID(), Email: name + "@test.com", DisplayName: name,
			Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
		addProjectMemberWithRole(t, s, proj, u.ID, store.GroupMemberRoleMember)
		return u
	}
	f := &memberFanoutFixture{
		srv: srv, s: s, wcs: wcs, ep: ep, proj: proj, alice: alice, bob: bob,
		carol: member("carol"), dave: member("dave"), sam: member("sam"), erin: member("erin"),
	}

	f.topicID = api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: f.topicID, ProjectID: proj.ID, Name: "private plans",
		CreatedBy: alice.ID, CreatedAt: time.Now().UTC(),
	}))
	f.convID = topicConversationID(t, wcs, f.topicID)
	for _, u := range []*store.User{f.carol, f.dave, f.bob, f.sam} {
		require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: f.convID, PrincipalKind: "user", PrincipalID: u.ID, Role: "member",
		}))
	}
	require.NoError(t, s.RemoveParticipant(ctx, f.convID, "user", f.dave.ID))
	f.sam.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, f.sam))
	return f
}

// threadMessage is a stored-looking web message in the fixture's thread.
func (f *memberFanoutFixture) threadMessage(content string) *store.Message {
	return &store.Message{
		ID: api.NewUUID(), ProjectID: f.proj.ID, Sender: "user:" + f.alice.Email, SenderID: f.alice.ID,
		Msg: content, Type: "chat", Channel: "web", ThreadID: f.topicID, ConversationID: f.convID,
		CreatedAt: time.Now().UTC(),
	}
}

// memberMessageRecipients returns the user IDs of the user.<id>.chat.message
// events in evts, sorted.
func memberMessageRecipients(evts []Event) []string {
	var ids []string
	for _, e := range evts {
		parts := strings.Split(e.Subject, ".")
		if len(parts) == 4 && parts[0] == "user" && parts[2] == "chat" && parts[3] == "message" {
			ids = append(ids, parts[1])
		}
	}
	sort.Strings(ids)
	return ids
}

// A thread message reaches only current members who can read the project,
// on their user chat subject, with the ordinary message payload.
func TestThreadMessageFanOut_CurrentReadableMembersOnly(t *testing.T) {
	f := newMemberFanoutFixture(t)
	events, unsub := f.ep.Subscribe("user.*.chat.message")
	defer unsub()

	msg := f.threadMessage("the secret plan")
	f.srv.fanOutThreadMessageToMembers(context.Background(), msg, nil, nil)

	evts := collectEvents(events)
	assert.Equal(t, []string{f.carol.ID}, memberMessageRecipients(evts))
	require.Len(t, evts, 1)
	var payload UserMessageEvent
	require.NoError(t, json.Unmarshal(evts[0].Data, &payload))
	assert.Equal(t, msg.ID, payload.ID)
	assert.Equal(t, f.topicID, payload.ThreadID)
}

// No fan-out when the message's project is not the topic's, the conversation
// is not the topic's, the topic is deleted, or the message is not a web
// thread message.
func TestThreadMessageFanOut_ScopeChecks(t *testing.T) {
	f := newMemberFanoutFixture(t)
	ctx := context.Background()
	events, unsub := f.ep.Subscribe("user.*.chat.message")
	defer unsub()

	other := &store.Project{ID: api.NewUUID(), Name: "other", Slug: "other", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.s.CreateProject(ctx, other))

	wrongProject := f.threadMessage("x")
	wrongProject.ProjectID = other.ID
	f.srv.fanOutThreadMessageToMembers(ctx, wrongProject, nil, nil)

	wrongConv := f.threadMessage("x")
	wrongConv.ConversationID = api.NewUUID()
	f.srv.fanOutThreadMessageToMembers(ctx, wrongConv, nil, nil)

	notWeb := f.threadMessage("x")
	notWeb.Channel = "telegram"
	f.srv.fanOutThreadMessageToMembers(ctx, notWeb, nil, nil)

	dm := f.threadMessage("x")
	dm.ThreadID = "dm:user:" + f.alice.ID + ":user:" + f.carol.ID
	f.srv.fanOutThreadMessageToMembers(ctx, dm, nil, nil)

	assert.Empty(t, memberMessageRecipients(collectEvents(events)))

	// A deleted topic (the project keeps another, so it can be deleted).
	require.NoError(t, f.wcs.CreateTopic(ctx, WebChatTopic{
		ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "keeper", CreatedBy: f.alice.ID, CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, f.wcs.DeleteTopic(ctx, f.topicID))
	f.srv.fanOutThreadMessageToMembers(ctx, f.threadMessage("x"), nil, nil)
	assert.Empty(t, memberMessageRecipients(collectEvents(events)))
}

// Posting in a thread over HTTP fans the message out to members.
func TestThreadMessageFanOut_SendPath(t *testing.T) {
	f := newMemberFanoutFixture(t)
	carolEvents, unsubCarol := f.ep.Subscribe("user." + f.carol.ID + ".chat.message")
	defer unsubCarol()
	others, unsubOthers := f.ep.Subscribe(
		"user."+f.bob.ID+".chat.message", "user."+f.dave.ID+".chat.message",
		"user."+f.erin.ID+".chat.message", "user."+f.sam.ID+".chat.message")
	defer unsubOthers()

	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost,
		"/api/v1/chat/conversations/"+f.topicID+"/messages", map[string]string{"content": "hello members"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	select {
	case evt := <-carolEvents:
		var payload UserMessageEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		assert.Equal(t, "hello members", payload.Msg)
	case <-time.After(5 * time.Second):
		t.Fatal("member did not receive the thread message")
	}
	assert.Empty(t, collectEvents(others), "non-members, leavers and suspended users get nothing")
}

// Reading a conversation, and muting or unmuting it, tell the caller's own
// sessions on their read-state subject, never as a mark-unread.
func TestOwnStateEvents_ReadAndMute(t *testing.T) {
	f := newMemberFanoutFixture(t)
	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost,
		"/api/v1/chat/conversations/"+f.topicID+"/messages", map[string]string{"content": "first"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var sent struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sent))
	require.NotEmpty(t, sent.ID)

	// carol is a member and reads alice's message.
	own, unsubOwn := f.ep.Subscribe("user." + f.carol.ID + ".chat.read-state")
	defer unsubOwn()
	others, unsubOthers := f.ep.Subscribe("user." + f.alice.ID + ".chat.read-state")
	defer unsubOthers()

	rec = doRequestAsUser(t, f.srv, f.carol, http.MethodPost,
		"/api/v1/chat/conversations/"+f.topicID+"/read", map[string]string{"messageId": sent.ID})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.carol, http.MethodPut,
		"/api/v1/chat/conversations/"+f.topicID+"/mute", map[string]bool{"muted": true})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.carol, http.MethodPut,
		"/api/v1/chat/conversations/"+f.topicID+"/mute", map[string]bool{"muted": false})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Pinning does not change the count and publishes nothing.
	rec = doRequestAsUser(t, f.srv, f.carol, http.MethodPut,
		"/api/v1/chat/conversations/"+f.topicID+"/pin", map[string]bool{"pinned": true})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	evts := collectEvents(own)
	require.Len(t, evts, 3)
	var got []ChatReadStateEvent
	for _, e := range evts {
		var rs ChatReadStateEvent
		require.NoError(t, json.Unmarshal(e.Data, &rs))
		assert.Equal(t, f.topicID, rs.ConversationKey)
		assert.Equal(t, f.carol.ID, rs.UserID)
		assert.False(t, rs.Unread, "own read and mute events are never a mark-unread")
		got = append(got, rs)
	}
	assert.Equal(t, sent.ID, got[0].MessageID)
	assert.Nil(t, got[0].Muted)
	require.NotNil(t, got[1].Muted)
	assert.True(t, *got[1].Muted)
	require.NotNil(t, got[2].Muted)
	assert.False(t, *got[2].Muted)

	assert.Empty(t, collectEvents(others), "a thread read is not published to other users")
}

// A project member @mentioned into a thread they are not in receives the
// message on their user subject: membership is written before the fan-out
// reads the participants. Repeated to catch an ordering race.
func TestThreadMessageFanOut_MentionedNonParticipantReceives(t *testing.T) {
	f := newMemberFanoutFixture(t)
	for i := 0; i < 5; i++ {
		mia := addProjectHuman(t, f.s, f.proj, fmt.Sprintf("mia%d@test.com", i), fmt.Sprintf("Mia%d", i))
		require.False(t, isUserParticipant(t, f.s, f.convID, mia.ID))
		events, unsub := f.ep.Subscribe("user." + mia.ID + ".chat.message")

		rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost,
			"/api/v1/chat/conversations/"+f.topicID+"/messages",
			map[string]string{"content": fmt.Sprintf("hi @mia%d", i)})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		select {
		case evt := <-events:
			var payload UserMessageEvent
			require.NoError(t, json.Unmarshal(evt.Data, &payload))
			assert.Equal(t, f.topicID, payload.ThreadID)
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the mentioned user did not receive the message", i)
		}
		unsub()
		assert.True(t, isUserParticipant(t, f.s, f.convID, mia.ID))
	}
}

// The recipient bound counts accepted recipients only.
func TestThreadMessageFanOut_RecipientBound(t *testing.T) {
	f := newMemberFanoutFixture(t)
	ctx := context.Background()
	// Two more eligible members besides carol.
	for _, email := range []string{"uma@test.com", "vic@test.com"} {
		u := addProjectHuman(t, f.s, f.proj, email, strings.TrimSuffix(email, "@test.com"))
		require.NoError(t, f.s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: f.convID, PrincipalKind: "user", PrincipalID: u.ID, Role: "member",
		}))
	}
	events, unsub := f.ep.Subscribe("user.*.chat.message")
	defer unsub()

	f.srv.fanOutThreadMessageToMembers(ctx, f.threadMessage("all"), nil, nil)
	assert.Len(t, memberMessageRecipients(collectEvents(events)), 3, "default bound")

	f.srv.chatMemberFanout = chatMemberFanoutLimits{maxRecipients: 1}
	f.srv.fanOutThreadMessageToMembers(ctx, f.threadMessage("one"), nil, nil)
	assert.Len(t, memberMessageRecipients(collectEvents(events)), 1, "bound of 1")
}

// cancelOnUserLookupStore cancels a context the first time, while armed,
// that GetUser is asked for one of the watched users, and records which
// watched users were looked up.
type cancelOnUserLookupStore struct {
	store.Store
	fault *storeFaultSwitch

	mu     sync.Mutex
	watch  map[string]bool
	looked map[string]bool
	cancel context.CancelFunc
}

func (w *cancelOnUserLookupStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if w.fault.Active() {
		w.mu.Lock()
		if w.watch[id] {
			w.looked[id] = true
			if w.cancel != nil {
				w.cancel()
			}
		}
		w.mu.Unlock()
	}
	return w.Store.GetUser(ctx, id)
}

// A deadline that passes mid-loop stops the fan-out at the next member
// rather than looking up every remaining member with a dead context.
func TestThreadMessageFanOut_DeadlineStopsMidLoop(t *testing.T) {
	srv, s, alice, bob, proj, wrapped, fault := setupDemoPolicyTestWithFault(t,
		func(inner store.Store, f *storeFaultSwitch) *cancelOnUserLookupStore {
			return &cancelOnUserLookupStore{Store: inner, fault: f, looked: map[string]bool{}}
		})
	f := newMemberFanoutFixtureOn(t, srv, s, alice, bob, proj)
	events, unsub := f.ep.Subscribe("user.*.chat.message")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped.mu.Lock()
	// The fixture's active participants: carol, bob and sam.
	wrapped.watch = map[string]bool{f.carol.ID: true, f.bob.ID: true, f.sam.ID: true}
	wrapped.cancel = cancel
	wrapped.mu.Unlock()
	fault.Arm()

	f.srv.fanOutThreadMessageToMembers(ctx, f.threadMessage("late"), nil, nil)

	wrapped.mu.Lock()
	looked := len(wrapped.looked)
	wrapped.mu.Unlock()
	assert.Equal(t, 1, looked, "the loop must stop at the first member after the deadline passed")
	// The first member's lookups run on the cancelled context, so whether
	// they succeed depends on the store; only the loop stopping is pinned.
	assert.LessOrEqual(t, len(memberMessageRecipients(collectEvents(events))), 1)
}

// The same through the agent-routed send path: in a topic with a default
// agent, a human @mentioned by the sender receives the message on their
// user subject.
func TestThreadMessageFanOut_MentionedNonParticipantReceives_AgentRouted(t *testing.T) {
	f := newMemberFanoutFixture(t)
	ctx := context.Background()
	f.srv.SetDispatcher(&brokerMockDispatcher{})
	agent := &store.Agent{ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "Helper", Slug: "fanout-helper",
		Phase: "running", OwnerID: f.alice.ID, CreatedBy: f.alice.ID}
	require.NoError(t, f.s.CreateAgent(ctx, agent))
	topicID := api.NewUUID()
	require.NoError(t, f.wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: f.proj.ID, Name: "with agent", DefaultAgent: agent.Slug,
		CreatedBy: f.alice.ID, CreatedAt: time.Now().UTC(),
	}))
	convID := topicConversationID(t, f.wcs, topicID)

	nia := addProjectHuman(t, f.s, f.proj, "nia@test.com", "Nia")
	events, unsub := f.ep.Subscribe("user." + nia.ID + ".chat.message")
	defer unsub()

	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", map[string]string{"content": "please look, @nia"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	select {
	case evt := <-events:
		var payload UserMessageEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		assert.Equal(t, topicID, payload.ThreadID)
	case <-time.After(5 * time.Second):
		t.Fatal("the mentioned user did not receive the agent-routed message")
	}
	assert.True(t, isUserParticipant(t, f.s, convID, nia.ID))
}
