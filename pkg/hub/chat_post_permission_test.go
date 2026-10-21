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
}

// TestBuiltInRoles_ChatPost pins who holds project.chat_post among the
// seeded roles: project owner, admin and member, hub admin and super
// admin. The project-visitor, hub-member and hub-viewer roles do not.
func TestBuiltInRoles_ChatPost(t *testing.T) {
	holders := map[string]bool{
		store.ProjectRoleOwner:              true,
		store.ProjectRoleAdmin:              true,
		store.ProjectRoleMember:             true,
		store.SystemRoleHubAdmin:            true,
		store.SystemRoleSuperAdmin:          true,
		store.ProjectRoleVisitor:            false,
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
