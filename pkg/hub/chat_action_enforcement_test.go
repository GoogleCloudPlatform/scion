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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every thread action _capabilities reports is enforced by its route
// through chatThreadAccess (ptone/scion#5519 Phase 1). For each route: a
// project viewer reads the thread but is refused with 403
// chat_post_denied (details.chat_action names the action, details.reason
// read_only) and nothing changes; a project member and a project owner
// keep the action; a token without inbox:write is refused.

// assertChatActionDenied checks the refusal of a reader who may not take
// action: 403 chat_post_denied naming the action, reason read_only.
func assertChatActionDenied(t *testing.T, rec *httptest.ResponseRecorder, action string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details := errorCodeAndDetails(t, rec)
	assert.Equal(t, ErrCodeChatPostDenied, code)
	assert.Equal(t, action, details["chat_action"])
	assert.Equal(t, ChatReasonReadOnly, details["reason"])
}

// topicState reads a thread's stored name and default agent; ok is false
// when the thread no longer exists.
func (f *chatFixture) topicState(t *testing.T, key string) (name, defaultAgent string, ok bool) {
	t.Helper()
	topic, err := f.wcs.GetTopic(context.Background(), key)
	if err != nil || topic == nil {
		return "", "", false
	}
	return topic.Name, topic.DefaultAgent, true
}

func TestChatTopicRename_RequiresRenameAction(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	path := "/api/v1/chat/topics/" + f.topicA
	before, _, _ := f.topicState(t, f.topicA)

	rec := f.session(t, viewer, http.MethodPatch, path, map[string]string{"name": "viewer-rename"})
	assertChatActionDenied(t, rec, ChatActionRename)
	name, _, ok := f.topicState(t, f.topicA)
	require.True(t, ok)
	assert.Equal(t, before, name, "a refused rename leaves the name unchanged")

	// A viewer's token with inbox:write passes the token checks and is
	// refused by the thread decision.
	viewerToken := f.mint(t, viewer, projectBoundary(f.projA), "project:read", "inbox:write")
	assertChatActionDenied(t, f.call(t, viewerToken, http.MethodPatch, path, map[string]string{"name": "viewer-token-rename"}), ChatActionRename)

	// A token without inbox:write is refused.
	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPatch, path, map[string]string{"name": "token-rename"}).Code)
	name, _, _ = f.topicState(t, f.topicA)
	assert.Equal(t, before, name)

	rec = f.session(t, f.member, http.MethodPatch, path, map[string]string{"name": "member-rename"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	name, _, _ = f.topicState(t, f.topicA)
	assert.Equal(t, "member-rename", name)

	rec = f.session(t, f.owner, http.MethodPatch, path, map[string]string{"name": "owner-rename"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	name, _, _ = f.topicState(t, f.topicA)
	assert.Equal(t, "owner-rename", name)
}

func TestChatTopicDefaultAgent_RequiresSetDefaultAgentAction(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	path := "/api/v1/chat/topics/" + f.topicA

	rec := f.session(t, viewer, http.MethodPatch, path, map[string]string{"defaultAgent": f.agentA.Slug})
	assertChatActionDenied(t, rec, ChatActionSetDefaultAgent)
	_, da, ok := f.topicState(t, f.topicA)
	require.True(t, ok)
	assert.Empty(t, da, "a refused PATCH sets no default agent")

	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPatch, path, map[string]string{"defaultAgent": f.agentA.Slug}).Code)
	_, da, _ = f.topicState(t, f.topicA)
	assert.Empty(t, da)

	rec = f.session(t, f.member, http.MethodPatch, path, map[string]string{"defaultAgent": f.agentA.Slug})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, da, _ = f.topicState(t, f.topicA)
	assert.Equal(t, f.agentA.Slug, da)

	rec = f.session(t, f.owner, http.MethodPatch, path, map[string]string{"defaultAgent": ""})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, da, _ = f.topicState(t, f.topicA)
	assert.Empty(t, da)
}

func TestChatTopicDelete_RequiresDeleteAction(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	forMember, forOwner := f.topic(t, f.projA), f.topic(t, f.projA)

	rec := f.session(t, viewer, http.MethodDelete, "/api/v1/chat/topics/"+forMember, nil)
	assertChatActionDenied(t, rec, ChatActionDelete)
	_, _, ok := f.topicState(t, forMember)
	assert.True(t, ok, "a refused delete leaves the thread in place")

	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodDelete, "/api/v1/chat/topics/"+forMember, nil).Code)
	_, _, ok = f.topicState(t, forMember)
	assert.True(t, ok)

	rec = f.session(t, f.member, http.MethodDelete, "/api/v1/chat/topics/"+forMember, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, _, ok = f.topicState(t, forMember)
	assert.False(t, ok)

	rec = f.session(t, f.owner, http.MethodDelete, "/api/v1/chat/topics/"+forOwner, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, _, ok = f.topicState(t, forOwner)
	assert.False(t, ok)
}

func TestChatConversationDefaultAgent_RequiresSetDefaultAgentAction(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	viewer := f.chatViewer(t, f.projA)
	conv := f.group(t, f.projA, viewer, f.member, f.owner)
	path := "/api/v1/conversations/" + conv.ID + "/default-agent"
	body := map[string]string{"agentId": f.agentA.ID}
	unset := func() bool {
		t.Helper()
		got, err := f.s.GetConversation(ctx, conv.ID)
		require.NoError(t, err)
		return got.DefaultAgentID == nil || *got.DefaultAgentID == ""
	}

	assertChatActionDenied(t, f.session(t, viewer, http.MethodPut, path, body), ChatActionSetDefaultAgent)
	assert.True(t, unset(), "a refused request sets no default agent")

	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPut, path, body).Code)
	assert.True(t, unset())

	rec := f.session(t, f.member, http.MethodPut, path, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, unset())

	rec = f.session(t, f.owner, http.MethodPut, path, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestChatConversationAddParticipant_RequiresAddParticipantAction(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	viewer := f.chatViewer(t, f.projA)
	other := f.chatViewer(t, f.projA)
	conv := f.group(t, f.projA, viewer, f.member, f.owner)
	path := "/api/v1/conversations/" + conv.ID + "/participants"
	add := func(id string) map[string]string {
		return map[string]string{"principalKind": "user", "principalId": id}
	}
	isParticipant := func(id string) bool {
		t.Helper()
		ok, err := isConversationParticipant(ctx, f.s, conv.ID, "user", id)
		require.NoError(t, err)
		return ok
	}

	assertChatActionDenied(t, f.session(t, viewer, http.MethodPost, path, add(other)), ChatActionAddParticipant)
	assert.False(t, isParticipant(other), "a refused request adds no participant")

	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPost, path, add(other)).Code)
	assert.False(t, isParticipant(other))

	rec := f.session(t, f.member, http.MethodPost, path, add(other))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.True(t, isParticipant(other))

	rec = f.session(t, f.owner, http.MethodPost, path, add(f.admin))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.True(t, isParticipant(f.admin))
}

func TestChatMessageEdit_RequiresEditOwnAction(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	viewer := f.chatViewer(t, f.projA)
	content := func(id string) string {
		t.Helper()
		m, err := f.s.GetMessage(ctx, id)
		require.NoError(t, err)
		return m.Msg
	}
	path := func(id string) string { return chatConversationPath(f.topicA, "messages/"+id) }

	mine := f.chatMessage(t, "user:"+viewer, viewer, f.topicA, f.projA, "viewer said")
	assertChatActionDenied(t, f.session(t, viewer, http.MethodPut, path(mine), map[string]string{"content": "edited"}), ChatActionEditOwn)
	assert.Equal(t, "viewer said", content(mine), "a refused edit leaves the message unchanged")

	memberMsg := f.chatMessage(t, "user:"+f.member, f.member, f.topicA, f.projA, "member said")
	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPut, path(memberMsg), map[string]string{"content": "token edit"}).Code)
	assert.Equal(t, "member said", content(memberMsg))

	rec := f.session(t, f.member, http.MethodPut, path(memberMsg), map[string]string{"content": "member edited"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "member edited", content(memberMsg))

	ownerMsg := f.chatMessage(t, "user:"+f.owner, f.owner, f.topicA, f.projA, "owner said")
	rec = f.session(t, f.owner, http.MethodPut, path(ownerMsg), map[string]string{"content": "owner edited"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "owner edited", content(ownerMsg))
}

// TestChatMessageDelete_DeleteOwnAction: delete_own is a reader's action,
// so a viewer still retracts its own message; a token without inbox:write
// is refused and the message is unchanged.
func TestChatMessageDelete_DeleteOwnAction(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	viewer := f.chatViewer(t, f.projA)
	content := func(id string) string {
		t.Helper()
		m, err := f.s.GetMessage(ctx, id)
		require.NoError(t, err)
		return m.Msg
	}
	path := func(id string) string { return chatConversationPath(f.topicA, "messages/"+id) }

	memberMsg := f.chatMessage(t, "user:"+f.member, f.member, f.topicA, f.projA, "member said")
	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodDelete, path(memberMsg), nil).Code)
	assert.Equal(t, "member said", content(memberMsg))

	for _, who := range []string{viewer, f.member, f.owner} {
		id := f.chatMessage(t, "user:"+who, who, f.topicA, f.projA, "to retract")
		rec := f.session(t, who, http.MethodDelete, path(id), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Empty(t, content(id), "a retracted message is redacted")
	}
}

// formerPoster creates a user who may read and post in projectID through
// one role binding, and returns it with a function that deletes the
// binding (the user is removed from the project).
func (f *chatFixture) formerPoster(t *testing.T, projectID string) (string, func()) {
	t.Helper()
	ctx := context.Background()
	id := f.plainUser(t)
	rd, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "chat-poster-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read", "project.list", "project.chat_post"},
	})
	require.NoError(t, err)
	rb, err := f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      id,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return id, func() { require.NoError(t, f.s.DeleteRoleBinding(ctx, rb.ID)) }
}

// TestChatMessageEdit_SenderWithoutProjectRead_NotFound: editing one's own
// thread message needs the thread to be readable now. A sender removed from
// the project, and a message of a deleted thread, get the missing-thread
// answer (404), and the message is unchanged.
func TestChatMessageEdit_SenderWithoutProjectRead_NotFound(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	content := func(id string) string {
		t.Helper()
		m, err := f.s.GetMessage(ctx, id)
		require.NoError(t, err)
		return m.Msg
	}
	poster, remove := f.formerPoster(t, f.projA)
	// Control: before the removal the poster edits its message.
	control := f.chatMessage(t, "user:"+poster, poster, f.topicA, f.projA, "control")
	rec := f.session(t, poster, http.MethodPut, chatConversationPath(f.topicA, "messages/"+control), map[string]string{"content": "control edited"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	mine := f.chatMessage(t, "user:"+poster, poster, f.topicA, f.projA, "before removal")
	remove()
	rec = f.session(t, poster, http.MethodPut, chatConversationPath(f.topicA, "messages/"+mine), map[string]string{"content": "after removal"})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Thread not found")
	assert.Equal(t, "before removal", content(mine), "a refused edit leaves the message unchanged")

	gone := f.topic(t, f.projA)
	memberMsg := f.chatMessage(t, "user:"+f.member, f.member, gone, f.projA, "in a deleted thread")
	require.NoError(t, f.wcs.DeleteTopic(ctx, gone))
	rec = f.session(t, f.member, http.MethodPut, chatConversationPath(gone, "messages/"+memberMsg), map[string]string{"content": "edited"})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, "in a deleted thread", content(memberMsg))
}

// TestChatMessageDelete_SenderWithoutProjectRead_NotFound: deleting one's
// own thread message (delete_own) needs the thread to be readable now. A
// sender removed from the project, and a message of a deleted thread, get
// the missing-thread answer (404), and the message is not redacted.
func TestChatMessageDelete_SenderWithoutProjectRead_NotFound(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	content := func(id string) string {
		t.Helper()
		m, err := f.s.GetMessage(ctx, id)
		require.NoError(t, err)
		return m.Msg
	}
	poster, remove := f.formerPoster(t, f.projA)
	// Control: before the removal the poster deletes a message of its own.
	control := f.chatMessage(t, "user:"+poster, poster, f.topicA, f.projA, "control")
	rec := f.session(t, poster, http.MethodDelete, chatConversationPath(f.topicA, "messages/"+control), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	mine := f.chatMessage(t, "user:"+poster, poster, f.topicA, f.projA, "before removal")
	remove()
	rec = f.session(t, poster, http.MethodDelete, chatConversationPath(f.topicA, "messages/"+mine), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Thread not found")
	assert.Equal(t, "before removal", content(mine), "a refused delete leaves the message in place")

	gone := f.topic(t, f.projA)
	memberMsg := f.chatMessage(t, "user:"+f.member, f.member, gone, f.projA, "in a deleted thread")
	require.NoError(t, f.wcs.DeleteTopic(ctx, gone))
	rec = f.session(t, f.member, http.MethodDelete, chatConversationPath(gone, "messages/"+memberMsg), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, "in a deleted thread", content(memberMsg))
}

// assertPlainTokenForbidden checks a token refusal from a route's own token
// check: 403 forbidden, not the resolver's chat_post_denied.
func assertPlainTokenForbidden(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeForbidden, errorCode(t, rec), "the route's token check answers first, not the resolver")
}

// topicCount counts the live threads of a project's chat space (a group
// conversation mints one too).
func (f *chatFixture) topicCount(t *testing.T, projectID string) int {
	t.Helper()
	topics, err := f.wcs.ListTopics(context.Background(), projectID)
	require.NoError(t, err)
	return len(topics)
}

// TestChatCreateThread_RequiresCreateThreadAction: creating a thread is the
// space's create_thread action. A viewer is refused with 403
// chat_post_denied naming create_thread and no thread is created; a token
// without inbox:write is refused; a member and an owner create threads.
func TestChatCreateThread_RequiresCreateThreadAction(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	path := "/api/v1/chat/spaces/" + f.projA + "/threads"
	before := f.topicCount(t, f.projA)

	assertChatActionDenied(t, f.session(t, viewer, http.MethodPost, path, map[string]string{"name": "viewer-thread"}), ChatActionCreateThread)
	assert.Equal(t, before, f.topicCount(t, f.projA), "a refused create writes no thread")

	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assertPlainTokenForbidden(t, f.call(t, readOnly, http.MethodPost, path, map[string]string{"name": "token-thread"}))
	assert.Equal(t, before, f.topicCount(t, f.projA))
	// The token check runs before the space decision: a viewer's token
	// without inbox:write gets the plain forbidden, not chat_post_denied.
	viewerReadOnly := f.mint(t, viewer, projectBoundary(f.projA), "project:read")
	assertPlainTokenForbidden(t, f.call(t, viewerReadOnly, http.MethodPost, path, map[string]string{"name": "viewer-ro-thread"}))
	assert.Equal(t, before, f.topicCount(t, f.projA))

	// A viewer's token with inbox:write passes the token checks and is
	// refused by the space decision.
	viewerToken := f.mint(t, viewer, projectBoundary(f.projA), "project:read", "inbox:write")
	assertChatActionDenied(t, f.call(t, viewerToken, http.MethodPost, path, map[string]string{"name": "viewer-token-thread"}), ChatActionCreateThread)
	assert.Equal(t, before, f.topicCount(t, f.projA))

	rec := f.session(t, f.member, http.MethodPost, path, map[string]string{"name": "member-thread"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = f.session(t, f.owner, http.MethodPost, path, map[string]string{"name": "owner-thread"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, before+2, f.topicCount(t, f.projA))
}

// TestChatCreateThread_DefaultAgentNeedsSetDefaultAgent: a default agent on
// create is decided as set_default_agent, before create_thread, so a caller
// who could not set it afterwards is refused naming set_default_agent and no
// thread is created. A member creates a thread with a default agent.
func TestChatCreateThread_DefaultAgentNeedsSetDefaultAgent(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	path := "/api/v1/chat/spaces/" + f.projA + "/threads"
	before := f.topicCount(t, f.projA)

	rec := f.session(t, viewer, http.MethodPost, path, map[string]string{"name": "viewer-da", "defaultAgent": f.agentA.Slug})
	assertChatActionDenied(t, rec, ChatActionSetDefaultAgent)
	assert.Equal(t, before, f.topicCount(t, f.projA), "a refused create writes no thread")

	// Decided before validation: an unknown agent gives the viewer the
	// same refusal, not a validation error that would reveal the agent
	// does not exist.
	rec = f.session(t, viewer, http.MethodPost, path, map[string]string{"name": "viewer-da-unknown", "defaultAgent": "no-such-agent"})
	assertChatActionDenied(t, rec, ChatActionSetDefaultAgent)
	assert.Equal(t, before, f.topicCount(t, f.projA))

	rec = f.session(t, f.member, http.MethodPost, path, map[string]string{"name": "member-da", "defaultAgent": f.agentA.Slug})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created WebChatTopic
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	_, da, ok := f.topicState(t, created.ID)
	require.True(t, ok)
	assert.Equal(t, f.agentA.Slug, da)
}

// TestChatCreateConversation_RequiresCreateGroupAction: creating a group
// conversation is the space's create_group action. A viewer is refused with
// 403 chat_post_denied naming create_group and nothing is minted (no
// conversation thread); a token without inbox:write is refused; a member
// and an owner create groups.
func TestChatCreateConversation_RequiresCreateGroupAction(t *testing.T) {
	f := newChatFixture(t)
	viewer := f.chatViewer(t, f.projA)
	path := "/api/v1/conversations"
	body := func(name string) map[string]string {
		return map[string]string{"displayName": name, "projectId": f.projA}
	}
	before := f.topicCount(t, f.projA)

	assertChatActionDenied(t, f.session(t, viewer, http.MethodPost, path, body("viewer-group")), ChatActionCreateGroup)
	assert.Equal(t, before, f.topicCount(t, f.projA), "a refused create mints nothing")

	readOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	assertPlainTokenForbidden(t, f.call(t, readOnly, http.MethodPost, path, body("token-group")))
	assert.Equal(t, before, f.topicCount(t, f.projA))
	// The token check runs before the space decision: a viewer's token
	// without inbox:write gets the plain forbidden, not chat_post_denied.
	viewerReadOnly := f.mint(t, viewer, projectBoundary(f.projA), "project:read")
	assertPlainTokenForbidden(t, f.call(t, viewerReadOnly, http.MethodPost, path, body("viewer-ro-group")))
	assert.Equal(t, before, f.topicCount(t, f.projA))

	// A viewer's token with inbox:write passes the token checks and is
	// refused by the space decision.
	viewerToken := f.mint(t, viewer, projectBoundary(f.projA), "project:read", "inbox:write")
	assertChatActionDenied(t, f.call(t, viewerToken, http.MethodPost, path, body("viewer-token-group")), ChatActionCreateGroup)
	assert.Equal(t, before, f.topicCount(t, f.projA))

	rec := f.session(t, f.member, http.MethodPost, path, body("member-group"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = f.session(t, f.owner, http.MethodPost, path, body("owner-group"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, before+2, f.topicCount(t, f.projA))
}
