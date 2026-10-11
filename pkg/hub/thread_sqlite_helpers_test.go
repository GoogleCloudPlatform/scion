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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// addProjectHuman creates a user bound to the project-member role on proj, so
// resolveProjectHumanMembers (and so mention resolution) can find them.
func addProjectHuman(t *testing.T, s store.Store, proj *store.Project, email, displayName string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: api.NewUUID(), Email: email, DisplayName: displayName,
		Role: "member", Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, u))
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err, "project-member role definition must exist")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return u
}

// isUserParticipant reports whether userID holds an active user participant
// row on conversationID.
func isUserParticipant(t *testing.T, s store.Store, conversationID, userID string) bool {
	t.Helper()
	parts, err := s.ListParticipants(context.Background(), conversationID)
	require.NoError(t, err)
	for _, p := range parts {
		if p.PrincipalKind == "user" && p.PrincipalID == userID && p.LeftAt == nil {
			return true
		}
	}
	return false
}

// waitUserParticipant polls for an async membership write.
func waitUserParticipant(t *testing.T, s store.Store, conversationID, userID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return isUserParticipant(t, s, conversationID, userID)
	}, 5*time.Second, 10*time.Millisecond, "user %s should become a member", userID)
}

// setupSharedChatTest is setupSendTest with the webchat store on the hub
// store's own database, so CreateTopic mints the linked conversation and
// participant rows and topics can be read together.
func setupSharedChatTest(t *testing.T) (*Server, store.Store, WebChatStore, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: api.NewUUID(), Name: "member-test", Slug: "member-test", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store does not expose DB()")
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return srv, s, wcs, proj
}

// topicConversationID returns the conversation linked to a topic.
func topicConversationID(t *testing.T, wcs WebChatStore, topicID string) string {
	t.Helper()
	id, err := wcs.GetTopicConversationID(context.Background(), topicID)
	require.NoError(t, err)
	require.NotEmpty(t, id, "topic must have a linked conversation")
	return id
}
