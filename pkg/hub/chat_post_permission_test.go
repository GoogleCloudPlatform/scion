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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatReader creates a hub member whose only access to project is a custom
// project role holding project.read and project.list: it reads the
// project's threads but holds no project.chat_post.
func (f *chatFixture) chatReader(t *testing.T, projectID string) string {
	t.Helper()
	ctx := context.Background()
	id := f.plainUser(t)
	rd, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "chat-reader-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read", "project.list"},
	})
	require.NoError(t, err)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      id,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return id
}

// historyCanPost reads a thread's history as userID and returns the
// response's canPost (nil when absent).
func (f *chatFixture) historyCanPost(t *testing.T, userID, key string) *bool {
	t.Helper()
	rec := f.session(t, userID, http.MethodGet, chatConversationPath(key, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, "history: %s", rec.Body.String())
	var resp struct {
		CanPost *bool `json:"canPost"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.CanPost
}

// TestChatSendTopic_RequiresChatPost: posting into a project thread needs
// project.chat_post on the project. A caller that only reads the project
// still reads the thread (history, canPost false) and gets 403
// chat_post_denied on a send; the built-in project-member role keeps
// posting (canPost true, 201). A caller that cannot read the project still
// gets "Thread not found".
func TestChatSendTopic_RequiresChatPost(t *testing.T) {
	f := newChatFixture(t)
	reader := f.chatReader(t, f.projA)
	f.chatMessage(t, "user:"+f.admin, f.admin, f.topicA, f.projA, "hello readers")

	// The reader reads the thread, and is told it cannot post.
	canPost := f.historyCanPost(t, reader, f.topicA)
	require.NotNil(t, canPost, "a thread's history carries canPost")
	assert.False(t, *canPost, "a project.read-only caller cannot post")

	rec := f.session(t, reader, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "can I post?"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeChatPostDenied, errorCode(t, rec))

	// A thread of a project the reader cannot read stays hidden.
	rec = f.session(t, reader, http.MethodPost, chatConversationPath(f.topicB, "messages"), map[string]string{"content": "x"})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// The built-in project-member role posts.
	canPost = f.historyCanPost(t, f.member, f.topicA)
	require.NotNil(t, canPost)
	assert.True(t, *canPost, "project-member holds project.chat_post")
	rec = f.session(t, f.member, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "member post"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// So does a project owner.
	rec = f.session(t, f.owner, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "owner post"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// A DM's history carries no canPost.
	dm := f.dm(t, f.member, "user", f.owner)
	assert.Nil(t, f.historyCanPost(t, f.member, dm), "a DM's history leaves canPost out")
}

// TestChatSendTopic_ChatPostCheckedOnTokenHolder: a user access token still
// needs only inbox:write to post, and project.chat_post is decided on its
// holder's account: a reader's token with inbox:write is refused with 403
// chat_post_denied, a member's token posts.
func TestChatSendTopic_ChatPostCheckedOnTokenHolder(t *testing.T) {
	f := newChatFixture(t)
	reader := f.chatReader(t, f.projA)

	readerToken := f.mint(t, reader, projectBoundary(f.projA), "project:read", "inbox:write")
	rec := f.call(t, readerToken, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "token post"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeChatPostDenied, errorCode(t, rec))

	memberToken := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "inbox:write")
	rec = f.call(t, memberToken, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "token post"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestChatSendTopic_ChatPostSelectorDoesNotPost: the project:chat_post
// selector only keeps project.chat_post in a token's ceiling for
// delegation; it does not let a token post. A member's token with it but
// without inbox:write reads the thread, so it is refused by the resolver's
// token-scope step: 403 chat_post_denied with reason token_scope
// (ptone/scion#5519).
func TestChatSendTopic_ChatPostSelectorDoesNotPost(t *testing.T) {
	f := newChatFixture(t)
	topicMessages := func() int {
		t.Helper()
		rows, err := f.s.ListMessages(context.Background(), store.MessageFilter{ThreadID: f.topicA}, store.ListOptions{Limit: 100})
		require.NoError(t, err)
		return len(rows.Items)
	}
	before := topicMessages()

	selectorOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "project:chat_post")
	rec := f.call(t, selectorOnly, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "selector only"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details := errorCodeAndDetails(t, rec)
	assert.Equal(t, ErrCodeChatPostDenied, code, "refused by the token-scope step")
	assert.Equal(t, ChatReasonTokenScope, details["reason"])
	assert.Equal(t, before, topicMessages(), "a refused send writes no message")

	// With inbox:write as well the same member posts, and the message is
	// written (so the count above would have seen a write).
	both := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "project:chat_post", "inbox:write")
	rec = f.call(t, both, http.MethodPost, chatConversationPath(f.topicA, "messages"), map[string]string{"content": "selector and inbox"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, before+1, topicMessages())
}

// TestChatPromote_RequiresChatPost: promoting an agent DM moves its
// messages into a thread of the agent's project, so it needs
// project.chat_post there as a send does.
func TestChatPromote_RequiresChatPost(t *testing.T) {
	f := newChatFixture(t)
	reader := f.chatReader(t, f.projA)
	dm := f.dm(t, reader, "agent", f.agentA.ID)
	f.chatMessage(t, "user:"+reader, reader, dm, f.projA, "promote me")

	rec := f.session(t, reader, http.MethodPost, chatConversationPath(dm, "promote"), map[string]string{"name": "promoted"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeChatPostDenied, errorCode(t, rec))

	// A token's own refusal comes before the post check: the reader's
	// token with inbox:write but without agent:read fails the DM rule, and
	// is not told about project.chat_post.
	readerToken := f.mint(t, reader, projectBoundary(f.projA), "project:read", "inbox:write")
	rec = f.call(t, readerToken, http.MethodPost, chatConversationPath(dm, "promote"), map[string]string{"name": "promoted"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details := errorCodeAndDetails(t, rec)
	assert.Equal(t, ErrCodeForbidden, code, "the token refusal comes first, not chat_post_denied")
	wantResource, wantAction := selfPermissionResourceAction(permInboxWrite)
	assert.Equal(t, wantResource, details["resource_type"], "refused by the token's inbox.write DM rule")
	assert.Equal(t, string(wantAction), details["denied_action"])

	// The built-in project-member role promotes its own agent DM. Its
	// message is already dispatched, so no reply is in flight (a pending
	// one would be refused with 409 IN_FLIGHT_MESSAGES).
	memberDM := f.dm(t, f.member, "agent", f.agentA.ID)
	require.NoError(t, f.s.CreateMessage(context.Background(), &store.Message{
		ID: uuid.NewString(), ProjectID: f.projA, Sender: "user:" + f.member, SenderID: f.member, Recipient: "chat",
		Msg: "promote me too", Type: "chat", Channel: "web", ThreadID: memberDM, CreatedAt: time.Now().UTC().Add(-time.Minute),
		DispatchState: store.MessageDispatchDispatched,
	}))
	rec = f.session(t, f.member, http.MethodPost, chatConversationPath(memberDM, "promote"), map[string]string{"name": "member promoted"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// makeChatReader turns user, an ordinary project member of the fixture,
// into a reader: it leaves the project's members group, loses its role
// bindings (the project-member binding addProjectMemberWithRole created)
// and gets a custom project role holding only project.read and
// project.list, so it still reads the topic but holds no project.chat_post.
func (f *scheduledSendFixture) makeChatReader(t *testing.T, user *store.User) {
	t.Helper()
	ctx := context.Background()
	membersGroup, err := f.store.GetGroupBySlug(ctx, "project:"+f.project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, f.store.RemoveGroupMember(ctx, membersGroup.ID, store.GroupMemberTypeUser, user.ID))
	_, err = f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)
	rd, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "sched-reader-" + uuid.NewString()[:8], ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read", "project.list"},
	})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// TestScheduledSend_RequiresChatPost: scheduling a message into a thread,
// and asking again for a missed one (send-now), need project.chat_post as
// a live send does. A reader gets 403 chat_post_denied and nothing
// changes; it still lists its own rows (read access is enough).
func TestScheduledSend_RequiresChatPost(t *testing.T) {
	f := newScheduledSendFixture(t)
	now := time.Now().UTC()
	missed := f.insertScheduled(t, f.bob, "cp-missed", now.Add(-2*time.Hour))
	failRow(t, f.sms, missed.ID, ScheduledFailureMissed, now)
	f.insertScheduled(t, f.bob, "cp-pending", now.Add(time.Hour))
	f.makeChatReader(t, f.bob)

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.scheduledPath(), map[string]interface{}{
		"content":         "reader schedules",
		"fire_at":         now.Add(time.Hour).Format(time.RFC3339),
		"idempotency_key": "idem-reader-schedules",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeChatPostDenied, errorCode(t, rec))

	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(missed.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeChatPostDenied, errorCode(t, rec))
	row := f.row(t, f.bob, missed.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status, "send-now refused: the row is unchanged")
	assert.Equal(t, ScheduledFailureMissed, row.FailureReason)

	// The list answers 200 with the reader's own two rows, and no row was added.
	assert.Len(t, f.list(t, f.bob), 2)
	assert.Empty(t, f.topicMessages(t))
	assert.Empty(t, f.dispatcher.getMessages())
}

// TestScheduledSend_SenderLosesChatPost_FailsNoAccess: a message a member
// scheduled fails as no_access at fire time when the sender has become a
// reader meanwhile. The sender still reads the thread, so this is the
// post check, not the read check.
func TestScheduledSend_SenderLosesChatPost_FailsNoAccess(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	fireAt := time.Now().Add(2 * time.Minute)
	sm := f.schedule(t, f.bob, "after losing chat_post", fireAt)

	f.makeChatReader(t, f.bob)
	require.Len(t, f.list(t, f.bob), 1, "the sender still reads the thread")

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, fireAt.Add(time.Second)))
	row := f.row(t, f.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureNoAccess, row.FailureReason)
	assert.Empty(t, f.topicMessages(t), "nothing posted")
	assert.Empty(t, f.dispatcher.getMessages())
}

// TestBuiltInRoles_ChatPost pins who holds project.chat_post among the
// seeded roles: project owner, admin and member, hub admin and super
// admin. The project-visitor, project-viewer, hub-member and hub-viewer
// roles do not.
func TestBuiltInRoles_ChatPost(t *testing.T) {
	holders := map[string]bool{
		store.ProjectRoleOwner:              true,
		store.ProjectRoleAdmin:              true,
		store.ProjectRoleMember:             true,
		store.SystemRoleHubAdmin:            true,
		store.SystemRoleSuperAdmin:          true,
		store.ProjectRoleVisitor:            false,
		store.ProjectRoleViewer:             false,
		store.SystemRoleHubMember:           false,
		store.SystemRoleHubViewer:           false,
		store.SystemRoleGlobalCatalogAuthor: false,
	}
	for name, want := range holders {
		role := builtInRoleByName(t, name)
		if want {
			assert.Contains(t, role.Permissions, "project.chat_post", "%s holds project.chat_post", name)
		} else {
			assert.NotContains(t, role.Permissions, "project.chat_post", "%s must not hold project.chat_post", name)
		}
	}
}
