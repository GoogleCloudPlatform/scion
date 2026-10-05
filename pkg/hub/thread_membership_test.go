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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// Thread membership: who becomes a member of a thread
// ---------------------------------------------------------------------------

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

// Mentioning an agent slug makes nobody a member; mentioning a human project
// member makes that human a member. Neither creates a notification row.
func TestRecordThreadMembers_Mentions(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID: api.NewUUID(), Slug: "scout", Name: "Scout",
		Template: "claude", ProjectID: proj.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	alice := addProjectHuman(t, s, proj, "alice@example.com", "Alice Smith")
	bob := addProjectHuman(t, s, proj, "bob@example.com", "Bob")

	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "general",
		CreatedBy: alice.ID, CreatedAt: time.Now(),
	}))
	convID := topicConversationID(t, wcs, topicID)

	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: topicID, MentionNames: []string{"scout"},
	})
	parts, err := s.ListParticipants(ctx, convID)
	require.NoError(t, err)
	assert.Empty(t, parts, "an agent mention must not add a user member")

	// The autocomplete slug form of a multi-word display name resolves.
	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: topicID, MentionNames: []string{"scout", "alice-smith"},
	})
	assert.True(t, isUserParticipant(t, s, convID, alice.ID), "mentioned human becomes a member")
	assert.False(t, isUserParticipant(t, s, convID, bob.ID), "unmentioned human is not a member")

	// Email local part resolves; repeating is idempotent.
	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: topicID, MentionNames: []string{"bob", "Alice Smith"},
	})
	assert.True(t, isUserParticipant(t, s, convID, bob.ID))
	parts, err = s.ListParticipants(ctx, convID)
	require.NoError(t, err)
	assert.Len(t, parts, 2, "membership writes are idempotent")

	for _, id := range []string{alice.ID, bob.ID, agent.ID} {
		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, id, false)
		require.NoError(t, err)
		assert.Empty(t, notifs, "mentions must not create notification rows")
	}
}

// A user who left a thread is not re-added by a later mention.
func TestRecordThreadMembers_LeftStaysLeft(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()
	alice := addProjectHuman(t, s, proj, "alice@example.com", "Alice")

	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "general",
		CreatedBy: alice.ID, CreatedAt: time.Now(),
	}))
	convID := topicConversationID(t, wcs, topicID)

	srv.recordThreadMembers(ctx, threadMembership{ProjectID: proj.ID, ThreadKey: topicID, UserID: alice.ID})
	require.True(t, isUserParticipant(t, s, convID, alice.ID))
	require.NoError(t, s.RemoveParticipant(ctx, convID, "user", alice.ID))

	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: topicID, MentionNames: []string{"alice"},
	})
	assert.False(t, isUserParticipant(t, s, convID, alice.ID), "leaving is sticky")
}

// DM keys and topics with no linked conversation are ignored without error.
func TestRecordThreadMembers_IgnoresDMAndUnlinked(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()

	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: "dm:user:" + api.NewUUID() + ":user:" + api.NewUUID(), UserID: api.NewUUID(),
	})
	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "unlinked", CreatedBy: "x", CreatedAt: time.Now(),
	}))
	srv.recordThreadMembers(ctx, threadMembership{ProjectID: proj.ID, ThreadKey: topicID, UserID: api.NewUUID()})
	_ = s
}

// Creating a thread on the web makes the creator a member.
func TestCreateThread_CreatorBecomesMember(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/spaces/"+proj.ID+"/threads",
		map[string]string{"name": "member-thread"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var topic WebChatTopic
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &topic))

	convID := topicConversationID(t, wcs, topic.ID)
	waitUserParticipant(t, s, convID, topic.CreatedBy)
}

// Posting in a thread makes the sender a member; a mentioned human project
// member becomes one too, and no notification row is created.
func TestSendInThread_SenderAndMentionedBecomeMembers(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()
	alice := addProjectHuman(t, s, proj, "alice@example.com", "Alice")

	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "chatter", CreatedBy: "x", CreatedAt: time.Now(),
	}))
	convID := topicConversationID(t, wcs, topicID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "hi @alice"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	waitUserParticipant(t, s, convID, DevUserID)
	waitUserParticipant(t, s, convID, alice.ID)
	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, alice.ID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs, "a mention must not create a notification row")
}

// Sending a DM creates no DM_RECEIVED notification row for the peer and no
// thread membership.
func TestSendDM_NoNotificationRow(t *testing.T) {
	srv, s, _, _ := setupSharedChatTest(t)
	ctx := context.Background()
	peer := &store.User{
		ID: api.NewUUID(), Email: "peer@example.com", DisplayName: "Peer",
		Role: "member", Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, peer))
	key, err := messages.DMConversationKey("user", DevUserID, "user", peer.ID)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages",
		map[string]string{"content": "hello there"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	time.Sleep(200 * time.Millisecond)
	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, peer.ID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs, "a DM must not create a notification row")
}

// ---------------------------------------------------------------------------
// Tests: IsConversationMuted (store method)
// ---------------------------------------------------------------------------

func TestIsConversationMuted_SQLite(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())

	ctx := context.Background()
	userID := api.NewUUID()
	key := api.NewUUID()

	// No row → not muted.
	muted, err := wcs.IsConversationMuted(ctx, userID, key)
	require.NoError(t, err)
	assert.False(t, muted, "should not be muted when no row exists")

	// Set muted.
	require.NoError(t, wcs.SetMuted(ctx, userID, key, true))
	muted, err = wcs.IsConversationMuted(ctx, userID, key)
	require.NoError(t, err)
	assert.True(t, muted, "should be muted after SetMuted(true)")

	// Unmute.
	require.NoError(t, wcs.SetMuted(ctx, userID, key, false))
	muted, err = wcs.IsConversationMuted(ctx, userID, key)
	require.NoError(t, err)
	assert.False(t, muted, "should not be muted after SetMuted(false)")
}

// A presence manager that has never seen a heartbeat for the user, and a nil
// manager (no InitPresenceManager, e.g. in tests or a trimmed deployment),
// must both report absent so notifications keep flowing.
func TestPresenceManager_IsUserActive(t *testing.T) {
	var nilPM *PresenceManager
	assert.False(t, nilPM.IsUserActive("user-1"))

	pm := NewPresenceManager(NewChannelEventPublisher(), nil)
	t.Cleanup(pm.Stop)

	assert.False(t, pm.IsUserActive("user-1"))
	pm.Heartbeat(context.Background(), "user-1", "User One", nil)
	assert.True(t, pm.IsUserActive("user-1"))
	assert.False(t, pm.IsUserActive("user-2"))
}
