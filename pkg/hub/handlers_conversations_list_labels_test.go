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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestListConversations_LabelsNativeConversations covers ptone/scion#3499:
// native DMs and message-path threads carry no display name, so the list
// response must carry the DM peer (relative to the caller, with its name)
// and the linked webchat topic name.
func TestListConversations_LabelsNativeConversations(t *testing.T) {
	srv, s, wcs, _ := setupGroupConvTopicTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	project := &store.Project{ID: api.NewUUID(), Name: "labels-project", Slug: "labels-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{ID: api.NewUUID(), Name: "reviewer", Slug: "reviewer", ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, agent))
	user := &store.User{ID: api.NewUUID(), Email: "dev@example.com", DisplayName: "Dev User", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, user))

	// Native DM between the user and the agent, minted without a display name.
	dmKey, err := messages.DMConversationKey("user", user.ID, "agent", agent.ID)
	require.NoError(t, err)
	dm := &store.Conversation{ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: dmKey,
		DriftState: "active", LastActivityAt: now, CreatedAt: now}
	require.NoError(t, s.CreateConversation(ctx, dm))

	// Native thread minted on the message path (no display name), later
	// linked to a webchat topic that carries the name.
	topicID := api.NewUUID()
	threadRef, err := messaging.ThreadConversationExternalRef(project.ID, topicID)
	require.NoError(t, err)
	thread := &store.Conversation{ID: api.NewUUID(), ProjectID: &project.ID, Kind: "group", Surface: "native",
		ExternalRef: threadRef, DriftState: "active", LastActivityAt: now.Add(-time.Minute), CreatedAt: now}
	require.NoError(t, s.CreateConversation(ctx, thread))
	// The fresh ConversationID is deliberate: CreateTopic finds the
	// conversation already on the thread external_ref and relinks the topic
	// to it, which is the message-path-first scenario under test.
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: project.ID, Name: "design-chat",
		ConversationID: api.NewUUID(), CreatedBy: "dev", CreatedAt: now,
	}))

	// A named native thread linked to a topic keeps its display name and
	// gets no threadName.
	namedTopicID := api.NewUUID()
	namedRef, err := messaging.ThreadConversationExternalRef(project.ID, namedTopicID)
	require.NoError(t, err)
	named := &store.Conversation{ID: api.NewUUID(), ProjectID: &project.ID, Kind: "group", Surface: "native",
		DisplayName: "release-room", ExternalRef: namedRef, DriftState: "active",
		LastActivityAt: now.Add(-2 * time.Minute), CreatedAt: now}
	require.NoError(t, s.CreateConversation(ctx, named))
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: namedTopicID, ProjectID: project.ID, Name: "topic-name",
		ConversationID: named.ID, CreatedBy: "dev", CreatedAt: now,
	}))

	for _, id := range []string{dm.ID, thread.ID, named.ID} {
		addConvParticipant(t, s, id, "user", user.ID)
		addConvParticipant(t, s, id, "agent", agent.ID)
	}

	// Peer name fallbacks: a user with no display name is labelled by email,
	// and an agent with no name by its slug.
	quietUser := &store.User{ID: api.NewUUID(), Email: "quiet@example.com", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, quietUser))
	namelessAgent := &store.Agent{ID: api.NewUUID(), Name: "placeholder", Slug: "nameless-slug",
		ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, namelessAgent))
	// The store rejects an empty agent name on write, so blank it directly
	// to model a row that has none.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB() for this test")
	rawDB := dbProvider.DB()
	res, err := rawDB.ExecContext(ctx, `UPDATE agents SET name = '' WHERE id = ?`, namelessAgent.ID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	userDMKey, err := messages.DMConversationKey("user", user.ID, "user", quietUser.ID)
	require.NoError(t, err)
	userDM := &store.Conversation{ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: userDMKey,
		DriftState: "active", LastActivityAt: now.Add(-3 * time.Minute), CreatedAt: now}
	require.NoError(t, s.CreateConversation(ctx, userDM))
	addConvParticipant(t, s, userDM.ID, "user", user.ID)
	addConvParticipant(t, s, userDM.ID, "user", quietUser.ID)

	slugDMKey, err := messages.DMConversationKey("user", user.ID, "agent", namelessAgent.ID)
	require.NoError(t, err)
	slugDM := &store.Conversation{ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: slugDMKey,
		DriftState: "active", LastActivityAt: now.Add(-4 * time.Minute), CreatedAt: now}
	require.NoError(t, s.CreateConversation(ctx, slugDM))
	addConvParticipant(t, s, slugDM.ID, "user", user.ID)
	addConvParticipant(t, s, slugDM.ID, "agent", namelessAgent.ID)

	list := func(ctx context.Context) map[string]conversationResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		srv.handleListConversations(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		var result conversationListResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
		byID := map[string]conversationResponse{}
		for _, c := range result.Conversations {
			byID[c.ID] = c
		}
		return byID
	}

	// The user sees the agent as the DM peer, and the thread's topic name.
	asUser := list(contextWithIdentity(ctx, NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "user", "web")))
	require.Contains(t, asUser, dm.ID)
	require.Equal(t, &conversationPeer{Kind: "agent", ID: agent.ID, Name: "reviewer"}, asUser[dm.ID].DMPeer)
	require.Empty(t, asUser[dm.ID].ThreadName)
	require.Contains(t, asUser, thread.ID)
	require.Equal(t, "design-chat", asUser[thread.ID].ThreadName)
	require.Nil(t, asUser[thread.ID].DMPeer)
	require.Contains(t, asUser, named.ID)
	require.Equal(t, "release-room", asUser[named.ID].DisplayName)
	require.Empty(t, asUser[named.ID].ThreadName)
	require.Contains(t, asUser, userDM.ID)
	require.Equal(t, &conversationPeer{Kind: "user", ID: quietUser.ID, Name: "quiet@example.com"}, asUser[userDM.ID].DMPeer)
	require.Contains(t, asUser, slugDM.ID)
	require.Equal(t, &conversationPeer{Kind: "agent", ID: namelessAgent.ID, Name: "nameless-slug"}, asUser[slugDM.ID].DMPeer)

	// The agent sees the user as the DM peer.
	asAgent := list(agentContext(agent.ID, project.ID))
	require.Contains(t, asAgent, dm.ID)
	require.Equal(t, &conversationPeer{Kind: "user", ID: user.ID, Name: "Dev User"}, asAgent[dm.ID].DMPeer)
}
