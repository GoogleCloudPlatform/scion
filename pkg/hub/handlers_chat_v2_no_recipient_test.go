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

// Tests for the no-recipient dispatch state: a thread message that resolves
// no agent recipient (no default agent, no reply-to agent, no agent
// @mention) reaches no agent, so it must not be recorded as "dispatched".
// Routing itself is unchanged: these tests pin that no agent receives it,
// even when an agent has already posted in and joined the thread.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// bindProjectMember gives userID the project member role so
// resolveProjectHumanMembers finds it.
func bindProjectMember(t *testing.T, s store.Store, projectID, userID string) {
	t.Helper()
	ctx := t.Context()
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	if err != nil {
		t.Fatalf("GetRoleDefinitionByName: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	}); err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}
}

// addHumanMember creates a user and makes it a project member.
func addHumanMember(t *testing.T, s store.Store, projectID, email, name string) *store.User {
	t.Helper()
	u := &store.User{ID: api.NewUUID(), Email: email, DisplayName: name,
		Role: "member", Status: "active", Created: time.Now()}
	if err := s.CreateUser(t.Context(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bindProjectMember(t, s, projectID, u.ID)
	return u
}

// noRecipientSetup creates a topic with no default agent and an idle agent
// that has already posted in the thread and is a group participant.
func noRecipientSetup(t *testing.T) (*Server, store.Store, string, *store.Agent, *brokerMockDispatcher) {
	srv, s, topicID, a, d, _ := noRecipientSetupProject(t)
	return srv, s, topicID, a, d
}

func noRecipientSetupProject(t *testing.T) (*Server, store.Store, string, *store.Agent, *brokerMockDispatcher, string) {
	t.Helper()
	srv, s, wcs, proj, db := setupSendTest(t)
	d := &brokerMockDispatcher{}
	srv.SetDispatcher(d)
	ctx := t.Context()

	a := &store.Agent{ID: tid("norcpt-agent"), ProjectID: proj.ID, Name: "Poster", Slug: "norcpt-agent",
		Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	topicID := tid("norcpt-topic")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "norcpt",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	topic, err := wcs.GetTopic(ctx, topicID)
	if err != nil || topic == nil || topic.ConversationID == "" {
		t.Fatalf("GetTopic: %v %+v", err, topic)
	}
	srv.ensureGroupParticipants(ctx, topic.ConversationID, []*store.Agent{a})
	seedAgentMessage(t, s, proj, topicID, a, "agent was here")
	return srv, s, topicID, a, d, proj.ID
}

// An un-mentioned, untargeted thread reply reaches no agent (not even the
// agent that last posted and is a participant) and is recorded, returned
// and published as no_recipient rather than dispatched.
func TestNoRecipient_UnmentionedThreadReply(t *testing.T) {
	srv, s, topicID, _, d := noRecipientSetup(t)
	code, resp, m := unreachableSend(t, srv, s, topicID, "thanks, sounds good")

	if code != 201 {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
	if m == nil {
		t.Fatal("expected message to be persisted")
	}
	if m.Type != messages.TypeChat || m.Recipient != "thread:"+topicID || m.AgentID != "" {
		t.Fatalf("expected a thread chat row with no agent, got %+v", m)
	}
	if m.DispatchState != store.MessageDispatchNoRecipient {
		t.Fatalf("expected row dispatchState=%q, got %q",
			store.MessageDispatchNoRecipient, m.DispatchState)
	}
	if resp["dispatchState"] != store.MessageDispatchNoRecipient {
		t.Fatalf("expected response dispatchState=%q, got %v",
			store.MessageDispatchNoRecipient, resp["dispatchState"])
	}
}

// A leading @mention of the agent in the same thread still dispatches and
// reads dispatched.
func TestNoRecipient_MentionedReplyStillDispatched(t *testing.T) {
	srv, s, topicID, a, d := noRecipientSetup(t)
	code, resp, m := unreachableSend(t, srv, s, topicID, "@"+a.Slug+" please check")

	if code != 201 {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if n := len(d.getMessages()); n != 1 {
		t.Fatalf("expected one dispatch, got %d", n)
	}
	if m == nil || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected row dispatched, got %+v", m)
	}
	if resp["dispatchState"] != store.MessageDispatchDispatched {
		t.Fatalf("expected response dispatchState=dispatched, got %v", resp["dispatchState"])
	}
}

// A topic default agent is an explicit target: an un-mentioned message
// dispatches to it and reads dispatched.
func TestNoRecipient_DefaultAgentStillDispatched(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topicID, _ := unreachableTestSetup(t, "running", false, d)
	_, resp, m := unreachableSend(t, srv, s, topicID, "hello")

	if n := len(d.getMessages()); n != 1 {
		t.Fatalf("expected one dispatch, got %d", n)
	}
	if m == nil || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected row dispatched, got %+v", m)
	}
	if resp["dispatchState"] != store.MessageDispatchDispatched {
		t.Fatalf("expected response dispatchState=dispatched, got %v", resp["dispatchState"])
	}
}

// A thread message that @mentions a project human and no agent is addressed
// to that project member: no "not delivered to any agent" warning. It keeps
// the dispatched state main records today.
func TestNoRecipient_HumanOnlyMentionKeepsDispatched(t *testing.T) {
	srv, s, topicID, _, d, projectID := noRecipientSetupProject(t)
	addHumanMember(t, s, projectID, "alice@example.com", "Alice Smith")

	for _, content := range []string{"@alice-smith can you look", "thanks @alice"} {
		code, resp, m := unreachableSend(t, srv, s, topicID, content)
		if code != 201 {
			t.Fatalf("%q: expected 201, got %d (body=%v)", content, code, resp)
		}
		if m == nil || m.DispatchState != store.MessageDispatchDispatched {
			t.Fatalf("%q: expected row dispatched, got %+v", content, m)
		}
		if v, ok := resp["dispatchState"]; ok && v != store.MessageDispatchDispatched {
			t.Fatalf("%q: expected no no_recipient in response, got %v", content, v)
		}
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
}

// Mentions that resolve to no project human (an unknown name, or only the
// sender) do not count: still no_recipient.
func TestNoRecipient_UnresolvedOrSelfMentionIsNoRecipient(t *testing.T) {
	srv, s, topicID, _, _, projectID := noRecipientSetupProject(t)
	addHumanMember(t, s, projectID, "alice@example.com", "Alice")
	dev, err := s.GetUser(t.Context(), DevUserID)
	if err != nil || dev == nil {
		t.Fatalf("GetUser(dev): %v", err)
	}
	bindProjectMember(t, s, projectID, DevUserID)
	self := dev.Email
	if at := strings.IndexByte(self, '@'); at > 0 {
		self = self[:at]
	}

	for _, content := range []string{"@nobody are you there", "note to self @" + self} {
		_, resp, m := unreachableSend(t, srv, s, topicID, content)
		if m == nil || m.DispatchState != store.MessageDispatchNoRecipient {
			t.Fatalf("%q: expected row no_recipient, got %+v", content, m)
		}
		if resp["dispatchState"] != store.MessageDispatchNoRecipient {
			t.Fatalf("%q: expected response no_recipient, got %v", content, resp["dispatchState"])
		}
	}
}

func TestMentionMatchesMember(t *testing.T) {
	m := chatMemberEntry{ID: "u1", Kind: "user", DisplayName: "Alice Smith", Email: "alice@example.com"}
	for name, want := range map[string]bool{
		"Alice-Smith":       true,
		"alice smith":       true,
		"ALICE@example.com": true,
		"alice":             true,
		"smith":             false,
		"nobody":            false,
		"":                  false,
	} {
		if got := mentionMatchesMember(name, m); got != want {
			t.Errorf("mentionMatchesMember(%q) = %v, want %v", name, got, want)
		}
	}
	if mentionMatchesMember("", chatMemberEntry{ID: "u2"}) {
		t.Error("a member with no name or email must not match")
	}
}

// requireDispatchedNotNoRecipient asserts the row kept dispatched and the
// response carries no no_recipient state.
func requireDispatchedNotNoRecipient(t *testing.T, label string, resp map[string]any, m *store.Message) {
	t.Helper()
	if m == nil || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("%s: expected row dispatched, got %+v", label, m)
	}
	if v, ok := resp["dispatchState"]; ok && v != store.MessageDispatchDispatched {
		t.Fatalf("%s: expected no no_recipient in response, got %v", label, v)
	}
}

// DMs are never no_recipient: a user-to-user DM is addressed to its peer.
func TestNoRecipient_UserDMStaysDispatched(t *testing.T) {
	srv, s, _, _, _ := setupSendTest(t)
	peer := &store.User{ID: api.NewUUID(), Email: "peer@example.com", DisplayName: "Peer",
		Role: "member", Status: "active", Created: time.Now()}
	if err := s.CreateUser(t.Context(), peer); err != nil {
		t.Fatal(err)
	}
	key := "dm:user:" + peer.ID + ":user:" + DevUserID
	setDMConversationID(t, s, key, "")

	code, resp, m := unreachableSend(t, srv, s, key, "hello there")
	if code != 201 {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	requireDispatchedNotNoRecipient(t, "user DM", resp, m)
}

// An agent DM whose agent no longer exists falls through to the
// human-to-human path as a DM; the DM guard keeps it out of no_recipient.
func TestNoRecipient_AgentDMFallthroughNotNoRecipient(t *testing.T) {
	srv, s, _, _, _ := setupSendTest(t)
	srv.SetDispatcher(&brokerMockDispatcher{})
	key := "dm:agent:" + api.NewUUID() + ":user:" + DevUserID
	setDMConversationID(t, s, key, "")

	code, resp, m := unreachableSend(t, srv, s, key, "hello")
	if code != 201 {
		t.Fatalf("expected 201, got %d (body=%v)", code, resp)
	}
	if m == nil || m.DispatchState == store.MessageDispatchNoRecipient {
		t.Fatalf("expected a persisted row that is not no_recipient, got %+v", m)
	}
	if resp["dispatchState"] == store.MessageDispatchNoRecipient {
		t.Fatalf("expected no no_recipient in response, got %v", resp)
	}
}

// A transient routing-plan error means the message cannot be proven
// agentless: keep the previous state instead of a permanent no_recipient.
func TestNoRecipient_RoutingPlanErrorKeepsPreviousState(t *testing.T) {
	srv, s, topicID, _, d := noRecipientSetup(t)
	srv.store = &errListAgentsStore{Store: s, err: errors.New("list agents: connection reset by peer")}

	_, resp, m := unreachableSend(t, srv, s, topicID, "thanks")
	requireDispatchedNotNoRecipient(t, "plan error", resp, m)
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
}

// A transient error looking up the topic default agent likewise keeps the
// previous state.
func TestNoRecipient_TransientDefaultLookupKeepsPreviousState(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	srv.SetDispatcher(&brokerMockDispatcher{})
	topicID := tid("norcpt-transient")
	if err := wcs.CreateTopic(t.Context(), WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "transient",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: "ghost-agent"}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	srv.store = &transientAgentLookupStore{Store: s, failSlug: "ghost-agent", err: errors.New("connection reset by peer")}

	_, resp, m := unreachableSend(t, srv, s, topicID, "hello")
	requireDispatchedNotNoRecipient(t, "transient default lookup", resp, m)
}

// replySend posts a quote-reply to replyToID in topicID.
func replySend(t *testing.T, srv *Server, s store.Store, topicID, content, replyToID string) (map[string]any, *store.Message) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": content, "reply_to_id": replyToID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := resp["id"].(string)
	m, _ := s.GetMessage(t.Context(), id)
	return resp, m
}

// A quote-reply to another person's message in the thread is addressed to
// that person: it keeps dispatched. A quote-reply to your own message does
// not count and is no_recipient.
func TestNoRecipient_QuoteReplyToHuman(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	d := &brokerMockDispatcher{}
	srv.SetDispatcher(d)
	topicID := tid("norcpt-quote")
	if err := wcs.CreateTopic(t.Context(), WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "quote",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	otherMsg := seedHumanMessage(t, s, proj, topicID, api.NewUUID(), "from someone else")
	resp, m := replySend(t, srv, s, topicID, "agreed", otherMsg)
	requireDispatchedNotNoRecipient(t, "reply to other human", resp, m)

	ownMsg := seedHumanMessage(t, s, proj, topicID, DevUserID, "my earlier note")
	resp, m = replySend(t, srv, s, topicID, "following up", ownMsg)
	if m == nil || m.DispatchState != store.MessageDispatchNoRecipient {
		t.Fatalf("reply to own message: expected row no_recipient, got %+v", m)
	}
	if resp["dispatchState"] != store.MessageDispatchNoRecipient {
		t.Fatalf("reply to own message: expected response no_recipient, got %v", resp["dispatchState"])
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
}

// errReplyLookupStore fails message lookups by ID.
type errReplyLookupStore struct {
	store.Store
}

func (e *errReplyLookupStore) GetMessagesByIDs(ctx context.Context, ids []string) (map[string]*store.Message, error) {
	return nil, errors.New("connection reset by peer")
}

// A failed lookup of the quoted message keeps the previous state.
func TestNoRecipient_ReplyLookupErrorKeepsPreviousState(t *testing.T) {
	srv, s, topicID, _, _ := noRecipientSetup(t)
	srv.store = &errReplyLookupStore{Store: s}

	resp, m := replySend(t, srv, s, topicID, "re", api.NewUUID())
	requireDispatchedNotNoRecipient(t, "reply lookup error", resp, m)
}

// errMembersStore fails project member listing, fails the batch user
// lookup when it includes one user, or omits another user as not found.
type errMembersStore struct {
	store.Store
	failList     bool
	failGetUser  string
	notFoundUser string
}

func (e *errMembersStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	if e.failList {
		return nil, errors.New("list members: connection reset by peer")
	}
	return e.Store.ListProjectMembers(ctx, projectID)
}

func (e *errMembersStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*store.User, error) {
	for _, id := range ids {
		if id == e.failGetUser {
			return nil, errors.New("get users: connection reset by peer")
		}
	}
	users, err := e.Store.GetUsersByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	delete(users, e.notFoundUser)
	return users, nil
}

// A failed member lookup means a mention of a real member cannot be ruled
// out: keep dispatched rather than a terminal no_recipient.
func TestNoRecipient_MemberLookupErrorKeepsDispatched(t *testing.T) {
	for _, tc := range []string{"list members fails", "get user fails"} {
		t.Run(tc, func(t *testing.T) {
			srv, s, topicID, _, _, projectID := noRecipientSetupProject(t)
			alice := addHumanMember(t, s, projectID, "alice@example.com", "Alice")
			es := &errMembersStore{Store: s}
			if tc == "list members fails" {
				es.failList = true
			} else {
				es.failGetUser = alice.ID
			}
			srv.store = es

			_, resp, m := unreachableSend(t, srv, s, topicID, "@alice can you look")
			requireDispatchedNotNoRecipient(t, tc, resp, m)
		})
	}
}

// seedRefMessage persists a message in threadID with the given sender
// fields, as a quote-reply target.
func seedRefMessage(t *testing.T, s store.Store, projectID, threadID, sender, senderID string) string {
	t.Helper()
	id := api.NewUUID()
	if err := s.CreateMessage(t.Context(), &store.Message{
		ID: id, ProjectID: projectID, Sender: sender, SenderID: senderID,
		Recipient: "thread:" + threadID, Msg: "quoted", Type: "chat",
		ThreadID: threadID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedRefMessage: %v", err)
	}
	return id
}

// Only a quote-reply to the sender's own user message, in any thread, is
// unaddressed (and then mentions decide). Any other reference, including
// one in another thread or one that cannot be found, keeps the previous
// state.
func TestNoRecipient_QuoteReplyRefKinds(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	d := &brokerMockDispatcher{}
	srv.SetDispatcher(d)
	ctx := t.Context()
	newTopic := func(name string) string {
		id := tid("norcpt-ref-" + name)
		if err := wcs.CreateTopic(ctx, WebChatTopic{ID: id, ProjectID: proj.ID, Name: name,
			CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		setTopicConversationID(t, db, s, id, proj.ID)
		return id
	}
	topicID := newTopic("main")
	otherTopic := newTopic("other")
	addHumanMember(t, s, proj.ID, "alice@example.com", "Alice")

	// An agent from another project: the reply-to agent override does not
	// apply, so the reply reaches this path.
	other := &store.Project{ID: tid("norcpt-other-proj"), Name: "other", Slug: "norcpt-other",
		Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(ctx, other); err != nil {
		t.Fatal(err)
	}
	foreign := &store.Agent{ID: tid("norcpt-foreign"), ProjectID: other.ID, Name: "Foreign",
		Slug: "norcpt-foreign", Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		ref     string
		content string
		want    string
	}{
		{"agent ref", seedRefMessage(t, s, proj.ID, topicID, "agent:"+foreign.Slug, foreign.ID),
			"ok", store.MessageDispatchDispatched},
		{"user ref without user ID", seedRefMessage(t, s, proj.ID, topicID, "user:telegram-bob", ""),
			"ok", store.MessageDispatchDispatched},
		{"other sender kind", seedRefMessage(t, s, proj.ID, topicID, "channel:telegram", DevUserID),
			"ok", store.MessageDispatchDispatched},
		{"own user ref", seedRefMessage(t, s, proj.ID, topicID, "user:dev@localhost", DevUserID),
			"ok", store.MessageDispatchNoRecipient},
		{"another person in another thread", seedRefMessage(t, s, proj.ID, otherTopic, "user:x@example.com", api.NewUUID()),
			"ok", store.MessageDispatchDispatched},
		{"agent in another thread", seedRefMessage(t, s, proj.ID, otherTopic, "agent:"+foreign.Slug, foreign.ID),
			"ok", store.MessageDispatchDispatched},
		{"missing ref", api.NewUUID(), "ok", store.MessageDispatchDispatched},
		{"own ref in another thread", seedRefMessage(t, s, proj.ID, otherTopic, "user:dev@localhost", DevUserID),
			"ok", store.MessageDispatchNoRecipient},
		{"own ref in another thread, human mention", seedRefMessage(t, s, proj.ID, otherTopic, "user:dev@localhost", DevUserID),
			"ok @alice", store.MessageDispatchDispatched},
	}
	for _, c := range cases {
		resp, m := replySend(t, srv, s, topicID, c.content, c.ref)
		if m == nil || m.DispatchState != c.want {
			t.Errorf("%s: expected row %q, got %+v", c.name, c.want, m)
			continue
		}
		if c.want == store.MessageDispatchNoRecipient && resp["dispatchState"] != c.want {
			t.Errorf("%s: expected response %q, got %v", c.name, c.want, resp["dispatchState"])
		}
		if c.want == store.MessageDispatchDispatched {
			if v, ok := resp["dispatchState"]; ok && v != c.want {
				t.Errorf("%s: expected no no_recipient in response, got %v", c.name, v)
			}
		}
	}
	if n := len(d.getMessages()); n != 0 {
		t.Fatalf("expected no agent dispatch, got %d", n)
	}
}

// A member whose user record is gone cannot be addressed: it is skipped,
// not treated as a lookup failure, so an unresolved mention stays
// no_recipient.
func TestNoRecipient_MissingMemberUserSkipped(t *testing.T) {
	srv, s, topicID, _, _, projectID := noRecipientSetupProject(t)
	gone := addHumanMember(t, s, projectID, "gone@example.com", "Gone")
	srv.store = &errMembersStore{Store: s, notFoundUser: gone.ID}

	_, resp, m := unreachableSend(t, srv, s, topicID, "@nobody hello")
	if m == nil || m.DispatchState != store.MessageDispatchNoRecipient {
		t.Fatalf("expected row no_recipient, got %+v", m)
	}
	if resp["dispatchState"] != store.MessageDispatchNoRecipient {
		t.Fatalf("expected response no_recipient, got %v", resp["dispatchState"])
	}
}

// Two members share a display name and the sender is one of them: a
// mention of that name still addresses the other member, whatever order the
// members are listed in.
func TestNoRecipient_SharedNameWithSenderStillAddressed(t *testing.T) {
	for _, senderFirst := range []bool{true, false} {
		name := "sender listed last"
		if senderFirst {
			name = "sender listed first"
		}
		t.Run(name, func(t *testing.T) {
			srv, s, topicID, _, _, projectID := noRecipientSetupProject(t)
			ctx := t.Context()
			dev, err := s.GetUser(ctx, DevUserID)
			if err != nil {
				t.Fatalf("GetUser(dev): %v", err)
			}
			dev.DisplayName = "Alex"
			if err := s.UpdateUser(ctx, dev); err != nil {
				t.Fatalf("UpdateUser(dev): %v", err)
			}
			if senderFirst {
				bindProjectMember(t, s, projectID, DevUserID)
				addHumanMember(t, s, projectID, "alex2@example.com", "Alex")
			} else {
				addHumanMember(t, s, projectID, "alex2@example.com", "Alex")
				bindProjectMember(t, s, projectID, DevUserID)
			}

			_, resp, m := unreachableSend(t, srv, s, topicID, "@alex can you look")
			requireDispatchedNotNoRecipient(t, name, resp, m)
		})
	}
}
