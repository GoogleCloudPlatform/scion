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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests of the chat resolvers (chat_access.go, ptone/scion#5519 Phase 1):
// chatThreadAccess and chatSpaceAccess, their _capabilities, and the send
// path that is decided by them.

// chatViewer creates a hub member holding the built-in project-viewer role
// on projectID.
func (f *chatFixture) chatViewer(t *testing.T, projectID string) string {
	t.Helper()
	id := f.plainUser(t)
	createTestUserWithProjectRole(t, f.s, id, id+"@test.com", projectID, store.ProjectRoleViewer)
	return id
}

// chatCapsBody is the part of a history response the resolver decides.
type chatCapsBody struct {
	CanPost      *bool       `json:"canPost"`
	Capabilities *ChatAccess `json:"_capabilities"`
}

func decodeChatCaps(t *testing.T, body []byte) chatCapsBody {
	t.Helper()
	var resp chatCapsBody
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

// topicMessageCount counts the stored messages of a chat thread.
func (f *chatFixture) topicMessageCount(t *testing.T, key string) int {
	t.Helper()
	rows, err := f.s.ListMessages(context.Background(), store.MessageFilter{ThreadID: key}, store.ListOptions{Limit: 200})
	require.NoError(t, err)
	return len(rows.Items)
}

// postDeniedReason returns the details.reason of a chat_post_denied answer.
func postDeniedReason(t *testing.T, body []byte) string {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	require.Equal(t, ErrCodeChatPostDenied, resp.Error.Code, string(body))
	reason, _ := resp.Error.Details["reason"].(string)
	return reason
}

// TestChatThreadAccess_ViewerReadsButCannotPost: a project-viewer reads a
// topic, its history carries no post action (reason read_only, canPost
// false), and a forced send is refused with 403 chat_post_denied reason
// read_only without storing anything.
func TestChatThreadAccess_ViewerReadsButCannotPost(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	f.chatMessage(t, "user:"+f.admin, f.admin, f.topicA, f.projA, "hello viewers")

	rec := f.session(t, viewer, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	caps := decodeChatCaps(t, rec.Body.Bytes())
	require.NotNil(t, caps.Capabilities, "a topic history carries _capabilities")
	assert.ElementsMatch(t, []string{ChatActionRead, ChatActionDeleteOwn}, caps.Capabilities.Actions)
	assert.Equal(t, ChatReasonReadOnly, caps.Capabilities.Reason)
	require.NotNil(t, caps.CanPost)
	assert.False(t, *caps.CanPost, "canPost is the post action")

	before := f.topicMessageCount(t, f.topicA)
	rec = f.session(t, viewer, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "may I?"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ChatReasonReadOnly, postDeniedReason(t, rec.Body.Bytes()))
	assert.Equal(t, before, f.topicMessageCount(t, f.topicA), "a refused send stores nothing")
}

// TestChatThreadAccess_MemberAndOwnerPost: project-member behaviour is
// unchanged (it posts), and its capabilities list every thread write.
func TestChatThreadAccess_MemberAndOwnerPost(t *testing.T) {
	f := newChatFixture(t)
	for _, user := range []string{f.member, f.owner} {
		rec := f.session(t, user, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		caps := decodeChatCaps(t, rec.Body.Bytes())
		require.NotNil(t, caps.Capabilities)
		assert.ElementsMatch(t, append([]string{ChatActionRead, ChatActionDeleteOwn}, chatThreadWriteActions...), caps.Capabilities.Actions)
		assert.Empty(t, caps.Capabilities.Reason)
		require.NotNil(t, caps.CanPost)
		assert.True(t, *caps.CanPost)

		rec = f.session(t, user, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "post"})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	// A DM's history carries neither field.
	dm := f.dm(t, f.member, "user", f.owner)
	rec := f.session(t, f.member, http.MethodGet, chatConversationPath(dm, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	caps := decodeChatCaps(t, rec.Body.Bytes())
	assert.Nil(t, caps.Capabilities)
	assert.Nil(t, caps.CanPost)
}

// countingDecisionEmitter records every decision audit record; with the
// sample rate at 1 the authz service emits exactly one per CheckAccess.
type countingDecisionEmitter struct {
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
}

func (e *countingDecisionEmitter) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, record)
}

func (e *countingDecisionEmitter) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = nil
}

// onProject counts the recorded decisions on projectID by action.
func (e *countingDecisionEmitter) onProject(projectID string) map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	counts := map[string]int{}
	for _, r := range e.records {
		if r.ResourceType == "project" && r.ResourceID == projectID {
			counts[r.Permission]++
		}
	}
	return counts
}

// TestChatHistory_ProjectDecisionCount pins the authorization cost of a
// topic history page (ptone/scion#5519 rebase review N1): project read once
// (authorizeChatThreadRead) and project.chat_post once (chatThreadAccess,
// which reuses that read decision instead of checking it again), for a
// member and for a read-only viewer.
func TestChatHistory_ProjectDecisionCount(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	emitter := &countingDecisionEmitter{}
	f.srv.authzService.DecisionAuditSampleRate = 1.0
	f.srv.authzService.SetDecisionAuditEmitter(emitter)
	for _, user := range []string{f.member, viewer} {
		emitter.reset()
		rec := f.session(t, user, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotNil(t, decodeChatCaps(t, rec.Body.Bytes()).Capabilities)
		assert.Equal(t, map[string]int{string(ActionRead): 1, string(ActionChatPost): 1}, emitter.onProject(f.projA),
			"decisions on the topic's project for user %s", user)
	}
}

// TestChatThreadAccess_ProjectAdminAndHubAdminPost: a project-admin and a
// hub-admin (no project role) read and post, and their capabilities list
// every thread write.
func TestChatThreadAccess_ProjectAdminAndHubAdminPost(t *testing.T) {
	f := newChatFixture(t)
	projectAdmin := f.plainUser(t)
	createTestUserWithProjectRole(t, f.s, projectAdmin, projectAdmin+"@test.com", f.projA, store.ProjectRoleAdmin)
	hubAdmin := uuid.NewString()
	createTestUserWithRole(t, f.s, hubAdmin, hubAdmin+"@test.com", "admin", store.SystemRoleHubAdmin)
	for name, user := range map[string]string{"project-admin": projectAdmin, "hub-admin": hubAdmin} {
		rec := f.session(t, user, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", name, rec.Body.String())
		caps := decodeChatCaps(t, rec.Body.Bytes())
		require.NotNil(t, caps.Capabilities, name)
		assert.ElementsMatch(t, append([]string{ChatActionRead, ChatActionDeleteOwn}, chatThreadWriteActions...), caps.Capabilities.Actions, name)
		assert.Empty(t, caps.Capabilities.Reason, name)

		rec = f.session(t, user, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "post from " + name})
		assert.Equal(t, http.StatusCreated, rec.Code, "%s: %s", name, rec.Body.String())
	}
}

// TestChatThreadAccess_NonReaderGetsMissingThreadAnswer: a caller without
// project read gets, byte for byte, the answer for a thread that does not
// exist, on the send and on the history, and nothing is stored.
func TestChatThreadAccess_NonReaderGetsMissingThreadAnswer(t *testing.T) {
	f := newChatFixture(t)
	outsider := f.plainUser(t)
	missing := uuid.NewString()
	before := f.topicMessageCount(t, f.topicA)

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		var body interface{}
		if method == http.MethodPost {
			body = map[string]string{"content": "hello?"}
		}
		want := f.session(t, outsider, method, chatConversationPath(missing, "messages"), body)
		got := f.session(t, outsider, method, chatConversationPath(f.topicA, "messages"), body)
		require.Equal(t, http.StatusNotFound, want.Code, want.Body.String())
		assert.Equal(t, want.Code, got.Code, "%s status", method)
		assert.Equal(t, want.Body.String(), got.Body.String(), "%s body", method)
	}
	assert.Equal(t, before, f.topicMessageCount(t, f.topicA))
}

// TestChatThreadAccess_TokenScope: a user access token that reads a topic
// but has no inbox:write for its project loses every write (reason
// token_scope, delete_own included) and a forced send is refused with 403
// chat_post_denied reason token_scope; a token that cannot read the topic
// gets the missing-thread answer. A member's token with inbox:write posts.
func TestChatThreadAccess_TokenScope(t *testing.T) {
	f := newChatFixture(t)
	before := f.topicMessageCount(t, f.topicA)

	noWrite := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "inbox:read")
	rec := f.call(t, noWrite, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	caps := decodeChatCaps(t, rec.Body.Bytes())
	require.NotNil(t, caps.Capabilities)
	assert.Equal(t, []string{ChatActionRead}, caps.Capabilities.Actions)
	assert.Equal(t, ChatReasonTokenScope, caps.Capabilities.Reason)

	rec = f.call(t, noWrite, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "token"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ChatReasonTokenScope, postDeniedReason(t, rec.Body.Bytes()))

	// A token bound to another project does not read the topic: the
	// answer is the one for a missing thread. (The admin owns both
	// projects, so it may mint a project B token.)
	otherProject := f.mint(t, f.admin, projectBoundary(f.projB), "project:read", "inbox:write")
	want := f.call(t, otherProject, http.MethodPost, chatConversationPath(uuid.NewString(), "messages"), map[string]string{"content": "x"})
	got := f.call(t, otherProject, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "x"})
	require.Equal(t, http.StatusNotFound, want.Code, want.Body.String())
	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Body.String(), got.Body.String())
	assert.Equal(t, before, f.topicMessageCount(t, f.topicA), "refused sends store nothing")

	writer := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "inbox:write")
	rec = f.call(t, writer, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "token post"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, before+1, f.topicMessageCount(t, f.topicA))
}

// TestChatThreadAccess_DMTokenKeepsEarlyGate: a DM send by a token without
// inbox:write is still refused by the request-level selector check (403
// forbidden on inbox), not by the resolver.
func TestChatThreadAccess_DMTokenKeepsEarlyGate(t *testing.T) {
	f := newChatFixture(t)
	dm := f.dm(t, f.member, "user", f.owner)
	tok := f.mint(t, f.member, hubBoundary(), "inbox:read")
	rec := f.call(t, tok, http.MethodPost, chatConversationPath(dm, "messages"), map[string]string{"content": "x"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details := errorCodeAndDetails(t, rec)
	assert.Equal(t, ErrCodeForbidden, code)
	wantResource, _ := selfPermissionResourceAction(permInboxWrite)
	assert.Equal(t, wantResource, details["resource_type"])
}

// TestChatThreadAccess_AgentBranchUsesMessageMode: an agent is decided by
// its message mode alone (agentThreadPostPolicy): project and hub modes
// may post, including into another project's thread; none may not; a
// lineage agent may post only as an active participant of the thread's
// conversation, and a decision for it without the conversation id is an
// error. No agent gets read, and an unknown agent gets nothing without an
// error.
func TestChatThreadAccess_AgentBranchUsesMessageMode(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	projA, err := f.s.GetProject(ctx, f.projA)
	require.NoError(t, err)

	agent := func(mode, projectID string) *store.Agent {
		a := &store.Agent{ID: uuid.NewString(), Slug: "chat-access-" + uuid.NewString()[:8], Name: "a", ProjectID: projectID, MessageMode: mode}
		require.NoError(t, f.s.CreateAgent(ctx, a))
		return a
	}
	conv := f.group(t, f.projA)
	lineageIn := agent(store.MessageModeLineage, f.projA)
	addConvParticipant(t, f.s, conv.ID, "agent", lineageIn.ID)
	cases := []struct {
		name     string
		agent    *store.Agent
		wantPost bool
	}{
		{"project mode, same project", agent(store.MessageModeProject, f.projA), true},
		{"hub mode, other project", agent(store.MessageModeHub, f.projB), true},
		{"none mode", agent(store.MessageModeNone, f.projA), false},
		{"lineage mode, participant", lineageIn, true},
		{"lineage mode, not a participant", agent(store.MessageModeLineage, f.projA), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			access, err := f.srv.chatThreadAccess(ctx, agentIdentityFor(tc.agent.ID, tc.agent.ProjectID), chatThreadRef{Key: f.topicA, Project: projA, ConversationID: conv.ID})
			require.NoError(t, err)
			assert.Equal(t, tc.wantPost, access.Can(ChatActionPost))
			assert.False(t, access.Can(ChatActionRead), "an agent is not decided by project read")
		})
	}

	_, err = f.srv.chatThreadAccess(ctx, agentIdentityFor(lineageIn.ID, f.projA), chatThreadRef{Key: f.topicA, Project: projA})
	assert.ErrorIs(t, err, errChatThreadUnresolved, "a lineage decision needs the conversation id")

	access, err := f.srv.chatThreadAccess(ctx, agentIdentityFor(uuid.NewString(), f.projA), chatThreadRef{Key: f.topicA, Project: projA, ConversationID: conv.ID})
	require.NoError(t, err)
	assert.Empty(t, access.Actions, "an unknown agent gets nothing")
}

// TestChatThreadAccess_OtherCallerKindsGetNothing: a caller that is neither
// a user nor an agent gets no actions, and so does no caller.
func TestChatThreadAccess_OtherCallerKindsGetNothing(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	projA, err := f.s.GetProject(ctx, f.projA)
	require.NoError(t, err)
	for name, caller := range map[string]Identity{
		"broker": NewBrokerIdentity(uuid.NewString()),
		"nil":    nil,
	} {
		access, err := f.srv.chatThreadAccess(ctx, caller, chatThreadRef{Key: f.topicA, Project: projA})
		require.NoError(t, err)
		assert.Empty(t, access.Actions, name)
		assert.Empty(t, f.srv.chatSpaceAccess(ctx, caller, projA).Actions, name)
	}
}

// TestChatSpaceAccess_Actions: the space resolver lists read for every
// reader, thread and group creation with project.chat_post, and the space
// emoji with project.update.
func TestChatSpaceAccess_Actions(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	projA, err := f.s.GetProject(ctx, f.projA)
	require.NoError(t, err)
	user := func(id string) UserIdentity {
		u, err := f.s.GetUser(ctx, id)
		require.NoError(t, err)
		return userIdentityOf(u)
	}

	viewer := f.srv.chatSpaceAccess(ctx, user(f.chatViewer(t, f.projA)), projA)
	assert.Equal(t, []string{ChatActionRead}, viewer.Actions)
	assert.Equal(t, ChatReasonReadOnly, viewer.Reason)

	member := f.srv.chatSpaceAccess(ctx, user(f.member), projA)
	assert.ElementsMatch(t, []string{ChatActionRead, ChatActionCreateThread, ChatActionCreateGroup}, member.Actions)
	assert.Empty(t, member.Reason)

	owner := f.srv.chatSpaceAccess(ctx, user(f.owner), projA)
	assert.ElementsMatch(t, []string{ChatActionRead, ChatActionCreateThread, ChatActionCreateGroup, ChatActionSetEmoji}, owner.Actions)

	outsider := f.srv.chatSpaceAccess(ctx, user(f.plainUser(t)), projA)
	assert.Empty(t, outsider.Actions)
}

// TestChatTopicList_Capabilities: each thread of a space list carries the
// caller's thread capabilities.
func TestChatTopicList_Capabilities(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	list := func(user string) []chatTopicEntry {
		t.Helper()
		rec := f.session(t, user, http.MethodGet, "/api/v1/chat/spaces/"+f.projA+"/threads", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp chatTopicListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotEmpty(t, resp.Threads)
		return resp.Threads
	}
	for _, th := range list(viewer) {
		require.NotNil(t, th.Capabilities, th.ID)
		assert.False(t, th.Capabilities.Can(ChatActionPost), th.ID)
		assert.True(t, th.Capabilities.Can(ChatActionRead), th.ID)
		assert.Equal(t, ChatReasonReadOnly, th.Capabilities.Reason, th.ID)
	}
	for _, th := range list(f.member) {
		require.NotNil(t, th.Capabilities, th.ID)
		assert.True(t, th.Capabilities.Can(ChatActionPost), th.ID)
		assert.Empty(t, th.Capabilities.Reason, th.ID)
	}
}

// TestChatProjectUpload_RequiresChatPost: an upload to a space follows the
// send's decision: a viewer whose token passes the token checks is
// refused with 403 chat_post_denied reason read_only; a member uploads.
func TestChatProjectUpload_RequiresChatPost(t *testing.T) {
	f := newChatFixture(t)
	db, ok := f.s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store does not expose DB() *sql.DB")
	stored := func() int {
		t.Helper()
		var n int
		require.NoError(t, db.DB().QueryRow(`SELECT COUNT(*) FROM webchat_attachment WHERE project_id = ?`, f.projA).Scan(&n))
		return n
	}
	viewer := f.chatViewer(t, f.projA)
	rec := f.chatUpload(t, f.mint(t, viewer, projectBoundary(f.projA), "inbox:write", "project:read"), f.projA)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ChatReasonReadOnly, postDeniedReason(t, rec.Body.Bytes()))
	assert.Equal(t, 0, stored(), "a refused upload stores no attachment")

	rec = f.chatUpload(t, f.mint(t, f.member, projectBoundary(f.projA), "inbox:write", "project:read"), f.projA)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, 1, stored(), "the member's upload is stored (so the count above would have seen one)")
}

// TestChatPromote_ViewerRefused: promoting a DM into a thread needs the
// space's create_thread action; a viewer gets 403 chat_post_denied.
func TestChatPromote_ViewerRefused(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	dm := f.dm(t, viewer, "agent", f.agentA.ID)
	f.chatMessage(t, "user:"+viewer, viewer, dm, f.projA, "promote me")
	topics := func() int {
		t.Helper()
		list, err := f.wcs.ListTopics(context.Background(), f.projA)
		require.NoError(t, err)
		return len(list)
	}
	before := topics()
	rec := f.session(t, viewer, http.MethodPost, chatConversationPath(dm, "promote"), map[string]string{"name": "promoted"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ChatReasonReadOnly, postDeniedReason(t, rec.Body.Bytes()))
	assert.Equal(t, before, topics(), "a refused promotion creates no topic")
}

// TestProjectMembers_AssignProjectViewer: project-viewer is assignable
// through the membership API like project-member, by an owner and by an
// admin, and it is a membership role: a principal holds at most one
// built-in membership role, so adding another one never leaves two.
func TestProjectMembers_AssignProjectViewer(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	viewerRD, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
	require.NoError(t, err)
	memberRD, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	membersPath := "/api/v1/projects/" + f.projA + "/members"
	add := func(actor, principal, roleID string) int {
		t.Helper()
		rec := f.session(t, actor, http.MethodPost, membersPath, addProjectMemberRequest{
			RoleDefinitionID: roleID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      principal,
		})
		return rec.Code
	}
	builtInBindings := func(principal string) []string {
		t.Helper()
		bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, principal)
		require.NoError(t, err)
		var names []string
		for _, b := range bindings {
			if b.ScopeType != store.RoleScopeProject || b.ScopeID != f.projA {
				continue
			}
			rd, err := f.s.GetRoleDefinition(ctx, b.RoleDefinitionID)
			require.NoError(t, err)
			if store.IsBuiltInProjectMembershipRole(rd.Name) {
				names = append(names, rd.Name)
			}
		}
		return names
	}

	byOwner := f.plainUser(t)
	assert.Equal(t, http.StatusCreated, add(f.owner, byOwner, viewerRD.ID))
	assert.Equal(t, []string{store.ProjectRoleViewer}, builtInBindings(byOwner))

	// The viewer is a project member and reads the project's chat.
	member := f.srv.CheckEffectiveMembership(ctx, byOwner, f.projA)
	require.NoError(t, member.Err)
	assert.True(t, member.IsMember)
	assert.Equal(t, store.ProjectRoleViewer, member.Role)
	rec := f.session(t, byOwner, http.MethodGet, chatConversationPath(f.topicA, "messages"), nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Adding project-member for the same principal never leaves two
	// built-in membership roles.
	add(f.owner, byOwner, memberRD.ID)
	assert.Len(t, builtInBindings(byOwner), 1, "one built-in membership role per principal")

	// An admin manages viewers as it manages members.
	admin := f.plainUser(t)
	createTestUserWithProjectRole(t, f.s, admin, admin+"@test.com", f.projA, store.ProjectRoleAdmin)
	byAdmin := f.plainUser(t)
	assert.Equal(t, http.StatusCreated, add(admin, byAdmin, viewerRD.ID))
	assert.Equal(t, []string{store.ProjectRoleViewer}, builtInBindings(byAdmin))

	// The role picker lists it among the built-in roles.
	rec = f.session(t, f.owner, http.MethodGet, membersPath+"/assignable-roles", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"name":"project-viewer"`)
}

// TestAgentMessageThread_ViewerGetsChatPostDenied: a user post by thread key
// on the agent message route is decided by chatThreadAccess, as a chat send
// is (ptone/scion#5519). A project-viewer who may message and resume
// the agent reads the project, so it gets 403 chat_post_denied reason
// read_only, before any wake and without writing a conversation or a
// message.
func TestAgentMessageThread_ViewerGetsChatPostDenied(t *testing.T) {
	f := acceptanceSetup(t)
	ctx := context.Background()
	// Bound the readiness wait, so a post that wrongly wakes the agent
	// fails fast instead of waiting for it to start.
	prevReady := wakeReadyTimeout
	wakeReadyTimeout = 2 * time.Second
	t.Cleanup(func() { wakeReadyTimeout = prevReady })
	viewerID := tid("chat-access-thread-viewer")
	createTestUserWithProjectRole(t, f.store, viewerID, "chat-access-thread-viewer@test.com", f.projectB, store.ProjectRoleViewer)
	grantMessageAndLifecycle(t, f.store, viewerID, f.projectB)
	viewerUser, err := f.store.GetUser(ctx, viewerID)
	require.NoError(t, err)
	viewer := userIdentityOf(viewerUser)

	target := agentInStateB(t, f, "viewer-wake", "suspended", f.hubAgentB.RuntimeBrokerID)
	require.True(t, f.srv.canReadProject(ctx, viewer, f.projectB), "precondition: the viewer reads project B")
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, viewer, target, false)
	require.True(t, allowed, "precondition: the viewer may message the agent: %s", reason)

	threadID := "thread-chat-access-viewer"
	req := threadRequest(target, threadID)
	req.Wake = true
	before := agentMessageCount(t, f.store, target.ID)
	f.dispatcher.reset()
	got := postThreadRequestAs(t, f.srv, viewer, target, req)
	require.Equal(t, http.StatusForbidden, got.status, got.body)
	assert.Equal(t, ChatReasonReadOnly, postDeniedReason(t, []byte(got.body)))
	assert.Equal(t, 0, startCallsFor(f, target.ID), "no resume is attempted")
	stored, err := f.store.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, "suspended", stored.Phase, "the agent stays suspended")
	threadConversationAbsent(t, f.store, f.projectB, threadID)
	assert.Equal(t, before, agentMessageCount(t, f.store, target.ID), "no message row is written")
}

// TestAgentMessageThread_TokenBehaviourUnchanged pins a user access token's
// behaviour on the thread-key route in Phase 1: the resolver's token-scope
// step is skipped there (design section 8.3 item 4), so a project owner's
// token without inbox:write still posts by thread key as before.
func TestAgentMessageThread_TokenBehaviourUnchanged(t *testing.T) {
	f := acceptanceSetup(t)
	token := NewScopedUserIdentity(authUser(f.ownerB), f.projectB, []string{"project:read", "agent:read", "agent:message"})
	threadID := "thread-chat-access-token"

	got := postThreadMessageAs(t, f.srv, token, f.hubAgentB, threadID)
	require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, got.status, got.body)
	threadConversationPresent(t, f.store, f.projectB, threadID)
}

// TestProjectViewer_NotMemberForMembersInboundPolicy: a project-viewer
// cannot post messages, so it does not count as a member for the cross-project
// "members" inbound policy (ptone/scion#5519 lead ruling); a project-member
// origin user does.
func TestProjectViewer_NotMemberForMembersInboundPolicy(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()
	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require.NoError(t, err)
	target := msgAuthzAgent(t, f.store, "cp-viewer-target", f.projectB, store.MessageModeProject, []string{f.ownerB.ID})

	// Each origin user is a project-member of the sender's project A, so the
	// sender agent is in good standing and the decision reaches project B's
	// "members" inbound policy.
	viewerID := tid("cp-viewer-origin")
	createTestUserWithProjectRole(t, f.store, viewerID, "cp-viewer-origin@test.com", f.projectA, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, f.store, viewerID, "cp-viewer-origin@test.com", f.projectB, store.ProjectRoleViewer)
	m := f.srv.CheckEffectiveMembership(ctx, viewerID, f.projectB)
	require.NoError(t, m.Err)
	require.True(t, m.IsMember, "precondition: the viewer is a project member")
	require.True(t, m.ViewerOnly)
	viewerSender := msgAuthzAgent(t, f.store, "cp-viewer-sender", f.projectA, store.MessageModeHub, []string{viewerID})
	decision := f.srv.EvaluateAgentMessage(ctx, msgAuthzAgentIdentity(viewerSender.ID, f.projectA, viewerSender.Ancestry), target)
	assert.False(t, decision.Allowed)
	assert.Equal(t, MessageDenialCrossProjectNotMember, decision.Code, decision.Reason)

	memberID := tid("cp-member-origin")
	createTestUserWithProjectRole(t, f.store, memberID, "cp-member-origin@test.com", f.projectA, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, f.store, memberID, "cp-member-origin@test.com", f.projectB, store.ProjectRoleMember)
	memberSender := msgAuthzAgent(t, f.store, "cp-member-sender", f.projectA, store.MessageModeHub, []string{memberID})
	decision = f.srv.EvaluateAgentMessage(ctx, msgAuthzAgentIdentity(memberSender.ID, f.projectA, memberSender.Ancestry), target)
	assert.True(t, decision.Allowed, "a project-member origin user is admitted: %s", decision.Reason)
}

// TestProjectViewer_DoesNotAdmitMaterialRuntime: material runtime admission
// (check 5) refuses a root user whose only membership is project-viewer;
// the same user as project-member is admitted.
func TestProjectViewer_DoesNotAdmitMaterialRuntime(t *testing.T) {
	f := newMaterialFixture(t, "viewer-only")
	ctx := context.Background()
	bindings, err := f.Store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.UserID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.ProjectID {
			require.NoError(t, f.Store.DeleteRoleBinding(ctx, b.ID))
		}
	}
	bind := func(roleName string) string {
		t.Helper()
		rd, err := f.Store.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		b, err := f.Store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.UserID,
			ScopeType: store.RoleScopeProject, ScopeID: f.ProjectID, CreatedBy: "test",
		})
		require.NoError(t, err)
		return b.ID
	}
	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})

	viewerBinding := bind(store.ProjectRoleViewer)
	_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, ReasonMembershipRequired, reason)

	require.NoError(t, f.Store.DeleteRoleBinding(ctx, viewerBinding))
	bind(store.ProjectRoleMember)
	_, reason, status = f.Server.materialRuntimePrecheck(ctx, ident)
	assert.NotEqual(t, ReasonMembershipRequired, reason, "a project-member root user passes check 5 (status %d)", status)
}

// TestTestAdminRootScopes_ViewerGetsNoProjectSecrets: an agent rooted at an
// admin-kind test identity gets project-scope secrets only in a project
// where the root is a member beyond the project-viewer role
// (ptone/scion#5519): a viewer-only root gets user scope alone, a
// project-member root gets project and user scope.
func TestTestAdminRootScopes_ViewerGetsNoProjectSecrets(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-viewer-iss", store.SystemRoleSuperAdmin)
	root := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin).Identity.ID
	bindIn := func(projectID, roleName string) {
		t.Helper()
		require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: "p " + projectID[:8], Slug: "p-" + projectID[:8], CreatedBy: issuer.ID}))
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: root,
			ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
		})
		require.NoError(t, err)
	}
	viewerProject := tid("tsa-viewer-proj")
	bindIn(viewerProject, store.ProjectRoleViewer)
	memberProject := tid("tsa-member-proj")
	bindIn(memberProject, store.ProjectRoleMember)

	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, nil)
	d.SetHubID("test-hub")
	d.SetAuthzService(srv.authzService)
	agentIn := func(projectID string) *store.Agent {
		return &store.Agent{ID: generateID(), Name: "tsa-v", Slug: "tsa-v-" + generateID()[:6], ProjectID: projectID,
			OwnerID: root, Ancestry: []string{root}, RuntimeBrokerID: "broker-x", AppliedConfig: &store.AgentAppliedConfig{}}
	}

	restricted, allowed, _ := d.testAdminRootScopes(ctx, agentIn(viewerProject))
	require.True(t, restricted, "precondition: the root is an admin-kind test identity")
	assert.Equal(t, map[string]bool{store.ScopeUser: true}, allowed, "a viewer-only root gets no project scope")

	restricted, allowed, _ = d.testAdminRootScopes(ctx, agentIn(memberProject))
	require.True(t, restricted)
	assert.Equal(t, map[string]bool{store.ScopeUser: true, store.ScopeProject: true}, allowed, "a project-member root gets project scope")
}

// TestProjectViewer_RootAgentKeepsStanding pins Phase 1 behaviour (lead
// ruling; Phase 2 revisits standing): an agent whose root user is demoted
// to project-viewer keeps good standing, but material is refused to it.
func TestProjectViewer_RootAgentKeepsStanding(t *testing.T) {
	f := newMaterialFixture(t, "viewer-standing")
	ctx := context.Background()
	bindings, err := f.Store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.UserID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.ProjectID {
			require.NoError(t, f.Store.DeleteRoleBinding(ctx, b.ID))
		}
	}
	createTestUserWithProjectRole(t, f.Store, f.UserID, f.UserID+"@test.com", f.ProjectID, store.ProjectRoleViewer)

	assert.NoError(t, f.Server.agentStanding(ctx, f.AgentID), "the agent keeps standing (Phase 1)")
	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, ReasonMembershipRequired, reason, "material is refused to it")
}

// TestProjectViewer_KeepsOwnAgentRelationshipGrant pins Phase 1 behaviour
// (lead ruling; Phase 2 revisits it): a user demoted from project-member to
// project-viewer keeps the owner relationship grant on its own agent, so it
// still attaches to it (as a demotion to a custom read-only role does).
func TestProjectViewer_KeepsOwnAgentRelationshipGrant(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("viewer-rel-project")
	ownerID := tid("viewer-rel-owner")
	memberID := tid("viewer-rel-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)

	uatpDeleteProjectBinding(t, s, memberID, projectID)
	createTestUserWithProjectRole(t, s, memberID, memberID+"@test.com", projectID, store.ProjectRoleViewer)
	memberUser, err := s.GetUser(context.Background(), memberID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, memberUser, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	requireAuthorizedPTY(t, rec, "a viewer-only owner still attaches to its own agent (Phase 1)")
}

// TestProjectViewer_GroupDerivedViewerOnly covers the group-derived branch
// of the membership check: a user whose only project membership is a
// project-viewer binding of a group is a member but viewer-only; a group
// binding of project-member makes the same user a member beyond viewer.
func TestProjectViewer_GroupDerivedViewerOnly(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	userID := f.plainUser(t)
	bindGroup := func(roleName string) {
		t.Helper()
		g := &store.Group{ID: uuid.NewString(), Name: "g-" + roleName, Slug: "g-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
		require.NoError(t, f.s.CreateGroup(ctx, g))
		require.NoError(t, f.s.AddGroupMember(ctx, &store.GroupMember{GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: store.GroupMemberRoleMember}))
		rd, err := f.s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: g.ID,
			ScopeType: store.RoleScopeProject, ScopeID: f.projA, CreatedBy: "test",
		})
		require.NoError(t, err)
	}

	bindGroup(store.ProjectRoleViewer)
	m := f.srv.CheckEffectiveMembership(ctx, userID, f.projA)
	require.NoError(t, m.Err)
	assert.True(t, m.IsMember)
	assert.True(t, m.ViewerOnly, "membership only through a group's project-viewer binding is viewer-only")
	assert.False(t, m.AdmitsBeyondViewer())

	bindGroup(store.ProjectRoleMember)
	m = f.srv.CheckEffectiveMembership(ctx, userID, f.projA)
	require.NoError(t, m.Err)
	assert.False(t, m.ViewerOnly)
	assert.True(t, m.AdmitsBeyondViewer(), "a group's project-member binding admits beyond viewer")
}

// TestProjectViewer_OwnAgentGrantsKept pins Phase 1 behaviour (lead ruling,
// own-resource grants kept; Phase 2 revisits them): a user demoted from
// project-owner to project-viewer is still admitted at the gates of the
// agents it already controls, each a ProjectAdmissionForClass or
// ProjectTargetAdmission call that counts any membership:
//   - artifact review of its agents' artifacts (ResolveProvenanceRoot,
//     authz_provenance_root.go);
//   - delegation to its own agent: create and exchange
//     (handlers_agent_delegation.go) and the chain recheck at use
//     (authz_agent_delegation.go) share the issuer admission call below;
//   - messaging its own agent through ancestry (messageAncestorProjectAccess).
func TestProjectViewer_OwnAgentGrantsKept(t *testing.T) {
	f := newProvenanceFixture(t, "viewer-own", false)
	ctx := context.Background()
	ag := f.userChild(t, "viewer-own", ceilPrincip)

	// Demote the owner to project-viewer.
	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.projectID {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
		}
	}
	createTestUserWithProjectRole(t, f.store, f.userID, f.userID+"@test.com", f.projectID, store.ProjectRoleViewer)
	m := checkEffectiveMembership(ctx, f.store, f.a, f.userID, f.projectID)
	require.NoError(t, m.Err)
	require.True(t, m.ViewerOnly, "precondition: the user is now viewer-only")
	user, err := f.store.GetUser(ctx, f.userID)
	require.NoError(t, err)

	t.Run("artifact review: provenance root admitted", func(t *testing.T) {
		_, err := f.a.ResolveProvenanceRoot(ctx, ag.ID, ResolveProvenanceOptions{PermissionID: artifacts.PermissionManage})
		assert.NoError(t, err)
	})
	// Delegation create, exchange and the chain recheck all make this one
	// call (the issuer's admission to the agent's project), so one case
	// covers the three call sites.
	t.Run("delegation create, exchange and chain recheck: issuer admitted", func(t *testing.T) {
		adm, err := f.a.ProjectTargetAdmission(ctx, issuerPrincipal(user), f.projectID, agentDelegationCreatePermission, agentResource(ag), nil)
		require.NoError(t, err)
		assert.True(t, adm.Admitted)
	})
	t.Run("messaging ancestry: project access admitted", func(t *testing.T) {
		sender := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
		admitted, fault := f.a.messageAncestorProjectAccess(ctx, sender, f.projectID, agentResource(ag))
		assert.False(t, fault)
		assert.True(t, admitted)
	})
}
