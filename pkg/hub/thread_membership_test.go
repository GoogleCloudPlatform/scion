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

// Mentioned human project members become members; an empty mention list
// adds nobody, and repeating is idempotent. Neither creates a notification
// row.
func TestRecordThreadMembers_MentionedUsers(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()

	alice := addProjectHuman(t, s, proj, "alice@example.com", "Alice Smith")
	bob := addProjectHuman(t, s, proj, "bob@example.com", "Bob")

	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "general",
		CreatedBy: alice.ID, CreatedAt: time.Now(),
	}))
	convID := topicConversationID(t, wcs, topicID)

	srv.recordThreadMembers(ctx, threadMembership{ProjectID: proj.ID, ThreadKey: topicID})
	parts, err := s.ListParticipants(ctx, convID)
	require.NoError(t, err)
	assert.Empty(t, parts, "no sender and no mentions adds nobody")

	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: topicID, MentionedUserIDs: []string{alice.ID},
	})
	assert.True(t, isUserParticipant(t, s, convID, alice.ID), "mentioned human becomes a member")
	assert.False(t, isUserParticipant(t, s, convID, bob.ID), "unmentioned human is not a member")

	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: topicID, MentionedUserIDs: []string{bob.ID, alice.ID, bob.ID},
	})
	assert.True(t, isUserParticipant(t, s, convID, bob.ID))
	parts, err = s.ListParticipants(ctx, convID)
	require.NoError(t, err)
	assert.Len(t, parts, 2, "membership writes are idempotent")

	for _, id := range []string{alice.ID, bob.ID} {
		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, id, false)
		require.NoError(t, err)
		assert.Empty(t, notifs, "mentions must not create notification rows")
	}
}

// Rows are written only for a live topic of the request's project, and only
// on the topic's own conversation.
func TestRecordThreadMembers_ProjectAndTopicChecks(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	ctx := context.Background()
	alice := addProjectHuman(t, s, proj, "alice@example.com", "Alice")

	other := &store.Project{ID: api.NewUUID(), Name: "other", Slug: "other", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, other))
	foreignTopic := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: foreignTopic, ProjectID: other.ID, Name: "foreign", CreatedBy: "x", CreatedAt: time.Now(),
	}))
	foreignConv := topicConversationID(t, wcs, foreignTopic)

	// A thread key from another project does not gain members.
	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: foreignTopic, UserID: alice.ID, MentionedUserIDs: []string{alice.ID},
	})
	assert.False(t, isUserParticipant(t, s, foreignConv, alice.ID), "cross-project thread key")

	// No project: nothing to check against, so nothing is written.
	srv.recordThreadMembers(ctx, threadMembership{ThreadKey: foreignTopic, UserID: alice.ID})
	assert.False(t, isUserParticipant(t, s, foreignConv, alice.ID), "missing project")

	keeper := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: keeper, ProjectID: proj.ID, Name: "keeper", CreatedBy: "x", CreatedAt: time.Now(),
	}))
	keeperConv := topicConversationID(t, wcs, keeper)

	// A conversation ID that is not the topic's is refused.
	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: keeper, ConversationID: foreignConv, UserID: alice.ID,
	})
	assert.False(t, isUserParticipant(t, s, foreignConv, alice.ID), "mismatched conversation")
	assert.False(t, isUserParticipant(t, s, keeperConv, alice.ID), "mismatched conversation")

	// A deleted topic does not gain members.
	doomed := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: doomed, ProjectID: proj.ID, Name: "doomed", CreatedBy: "x", CreatedAt: time.Now(),
	}))
	doomedConv := topicConversationID(t, wcs, doomed)
	require.NoError(t, wcs.DeleteTopic(ctx, doomed))
	srv.recordThreadMembers(ctx, threadMembership{ProjectID: proj.ID, ThreadKey: doomed, UserID: alice.ID})
	assert.False(t, isUserParticipant(t, s, doomedConv, alice.ID), "deleted topic")

	// The matching project and conversation write the row.
	srv.recordThreadMembers(ctx, threadMembership{
		ProjectID: proj.ID, ThreadKey: keeper, ConversationID: keeperConv, UserID: alice.ID,
	})
	assert.True(t, isUserParticipant(t, s, keeperConv, alice.ID))
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
		ProjectID: proj.ID, ThreadKey: topicID, MentionedUserIDs: []string{alice.ID},
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
