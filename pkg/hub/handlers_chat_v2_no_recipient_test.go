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

// A thread message that @mentions a project human and no agent was meant
// for that person, who is notified: no "not delivered to any agent"
// warning. It keeps the dispatched state main records today.
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
