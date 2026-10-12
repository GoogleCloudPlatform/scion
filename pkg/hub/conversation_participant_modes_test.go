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

//go:build !no_sqlite && (!hubshard || hubshard_5)

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createModeTestAgent creates an agent with the given message mode in projectID.
func createModeTestAgent(t *testing.T, s store.Store, projectID, slug, mode string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:          api.NewUUID(),
		Name:        slug,
		Slug:        slug,
		ProjectID:   projectID,
		Phase:       "running",
		MessageMode: mode,
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

func createModeTestProject(t *testing.T, s store.Store, slug string) *store.Project {
	t.Helper()
	p := &store.Project{ID: api.NewUUID(), Name: slug, Slug: slug}
	require.NoError(t, s.CreateProject(context.Background(), p))
	return p
}

// participantsByID decodes a GET conversation body into raw participant
// objects keyed by principalId, so a test can tell an omitted messageMode
// from an empty one.
func participantsByID(t *testing.T, body []byte) map[string]map[string]interface{} {
	t.Helper()
	var raw struct {
		Participants []map[string]interface{} `json:"participants"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	out := map[string]map[string]interface{}{}
	for _, p := range raw.Participants {
		id, _ := p["principalId"].(string)
		out[id] = p
	}
	return out
}

// TestGetConversation_ParticipantMessageModes checks that the conversation
// GET reports each in-project agent participant's message mode, and omits
// it for users and for agents in another project (ptone/scion#4411).
func TestGetConversation_ParticipantMessageModes(t *testing.T) {
	srv, s := testServer(t)
	project, caller, conv := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, caller.ID, project.ID)

	peer := createModeTestAgent(t, s, project.ID, "branch-peer", store.MessageModeBranch)
	other := createModeTestProject(t, s, "other-project")
	foreign := createModeTestAgent(t, s, other.ID, "foreign-agent", store.MessageModeHub)

	addConvParticipant(t, s, conv.ID, "agent", caller.ID)
	addConvParticipant(t, s, conv.ID, "agent", peer.ID)
	addConvParticipant(t, s, conv.ID, "agent", foreign.ID)
	addConvParticipant(t, s, conv.ID, "user", "user-1")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContextWithScopes(caller.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := participantsByID(t, rr.Body.Bytes())
	require.Len(t, got, 4)
	assert.Equal(t, store.MessageModeProject, got[caller.ID]["messageMode"],
		"an agent created without a mode is stored as project")
	assert.Equal(t, store.MessageModeBranch, got[peer.ID]["messageMode"])
	assert.NotContains(t, got[foreign.ID], "messageMode", "cross-project agent must not expose its mode")
	assert.NotContains(t, got["user-1"], "messageMode", "users have no message mode")
}

// TestConversationParticipantViews_DirectConversations covers conversations
// without a project (DMs), where only an agent caller's own project is in
// scope.
func TestConversationParticipantViews_DirectConversations(t *testing.T) {
	srv, s := testServer(t)
	projA := createModeTestProject(t, s, "proj-a")
	projB := createModeTestProject(t, s, "proj-b")
	agentA := createModeTestAgent(t, s, projA.ID, "agent-a", store.MessageModeHub)
	agentA2 := createModeTestAgent(t, s, projA.ID, "agent-a2", store.MessageModeLineage)
	agentB := createModeTestAgent(t, s, projB.ID, "agent-b", store.MessageModeProject)

	dm := &store.Conversation{ID: api.NewUUID(), Kind: "direct", Surface: "native", CreatedAt: time.Now()}
	part := func(kind, id string) store.ConversationParticipant {
		return store.ConversationParticipant{ConversationID: dm.ID, PrincipalKind: kind, PrincipalID: id}
	}
	modes := func(views []conversationParticipantView) map[string]string {
		m := map[string]string{}
		for _, v := range views {
			m[v.PrincipalID] = v.MessageMode
		}
		return m
	}

	t.Run("agent caller, cross-project peer hidden", func(t *testing.T) {
		ctx := agentContext(agentA.ID, projA.ID)
		got := modes(srv.conversationParticipantViews(ctx, dm, []store.ConversationParticipant{
			part("agent", agentA.ID), part("agent", agentB.ID),
		}))
		assert.Equal(t, store.MessageModeHub, got[agentA.ID])
		assert.Empty(t, got[agentB.ID])
	})

	t.Run("agent caller, same-project peer shown", func(t *testing.T) {
		ctx := agentContext(agentA.ID, projA.ID)
		got := modes(srv.conversationParticipantViews(ctx, dm, []store.ConversationParticipant{
			part("agent", agentA.ID), part("agent", agentA2.ID),
		}))
		assert.Equal(t, store.MessageModeHub, got[agentA.ID])
		assert.Equal(t, store.MessageModeLineage, got[agentA2.ID])
	})

	t.Run("user caller, DM agent mode hidden", func(t *testing.T) {
		// A non-agent caller of a projectless conversation gets no
		// modes: it may not be able to read the agent's project.
		got := modes(srv.conversationParticipantViews(context.Background(), dm, []store.ConversationParticipant{
			part("user", "user-1"), part("agent", agentB.ID),
		}))
		assert.Empty(t, got[agentB.ID])
		assert.Empty(t, got["user-1"])
	})

	t.Run("user caller, agents in two projects, nothing shown", func(t *testing.T) {
		got := modes(srv.conversationParticipantViews(context.Background(), dm, []store.ConversationParticipant{
			part("agent", agentA.ID), part("agent", agentB.ID),
		}))
		assert.Empty(t, got[agentA.ID])
		assert.Empty(t, got[agentB.ID])
	})

	t.Run("unknown agent leaves mode empty", func(t *testing.T) {
		ctx := agentContext(agentA.ID, projA.ID)
		got := modes(srv.conversationParticipantViews(ctx, dm, []store.ConversationParticipant{
			part("agent", "missing-agent"),
		}))
		assert.Empty(t, got["missing-agent"])
	})
}
