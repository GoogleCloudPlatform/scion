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

func TestMatchHumanMentionIDs(t *testing.T) {
	members := []chatMemberEntry{
		{ID: "u1", Kind: "user", DisplayName: "Alice Smith", Email: "alice@example.com"},
		{ID: "u2", Kind: "user", DisplayName: "Bob", Email: "bob@example.com"},
	}
	cases := []struct {
		names []string
		want  []string
	}{
		{nil, nil},
		{[]string{"nobody"}, nil},
		{[]string{"Alice-Smith"}, []string{"u1"}},
		{[]string{"alice smith"}, []string{"u1"}},
		{[]string{"bob@example.com", "alice", "bob"}, []string{"u2", "u1"}},
	}
	for _, c := range cases {
		got := matchHumanMentionIDs(members, c.names)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("matchHumanMentionIDs(%v) = %v, want %v", c.names, got, c.want)
		}
	}
	if got := matchHumanMentionIDs(nil, []string{"alice"}); got != nil {
		t.Errorf("no members: got %v, want nil", got)
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
