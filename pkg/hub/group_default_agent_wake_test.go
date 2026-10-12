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

// Default-agent delivery for agent posts into group conversations
// (ptone/scion#4605). Each test posts through the real outbound handler
// into a group conversation and asserts both whether the default agent
// received a delivery and the response's default_agent_result,
// woken_agents and no_agent_woken fields.
//
// Run: go test ./pkg/hub/ -run 'TestGroupDefaultAgentWake' -count=1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// groupWakeFixture is a group conversation created by sender, with the
// mention-fanout agents (sender, target, bystander) in one project.
type groupWakeFixture struct {
	srv        *Server
	s          store.Store
	projectID  string
	sender     *store.Agent
	target     *store.Agent
	bystander  *store.Agent
	convID     string
	dispatcher *recordingDispatcher
}

func newGroupWakeFixture(t *testing.T) *groupWakeFixture {
	t.Helper()
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	grantAgentProjectAccess(t, s, sender.ID, project.ID)

	createBytes, _ := json.Marshal(createConversationRequest{
		DisplayName: "gdw-group-conversation",
		ProjectID:   project.ID,
		Kind:        "group",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(createBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(agentContextWithScopes(sender.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	createRR := httptest.NewRecorder()
	srv.handleCreateConversation(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code, "body: %s", createRR.Body.String())
	var created conversationResponse
	require.NoError(t, json.Unmarshal(createRR.Body.Bytes(), &created))

	return &groupWakeFixture{srv: srv, s: s, projectID: project.ID, sender: sender,
		target: target, bystander: bystander, convID: created.ID, dispatcher: dispatcher}
}

func (f *groupWakeFixture) setDefault(t *testing.T, agentID string) {
	t.Helper()
	ctx := context.Background()
	conv, err := f.s.GetConversation(ctx, f.convID)
	require.NoError(t, err)
	conv.DefaultAgentID = &agentID
	require.NoError(t, f.s.UpdateConversation(ctx, conv))
}

// groupWakeResponse is the subset of the outbound response under test.
type groupWakeResponse struct {
	MessageID          string                   `json:"message_id"`
	MentionResults     []messages.MentionResult `json:"mention_results"`
	DefaultAgentResult *messages.MentionResult  `json:"default_agent_result"`
	WokenAgents        []string                 `json:"woken_agents"`
	NoAgentWoken       *bool                    `json:"no_agent_woken"`
}

func (f *groupWakeFixture) post(t *testing.T, msg, msgType string) groupWakeResponse {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Msg:             msg,
		Type:            msgType,
		ConversationRef: "conv:" + f.convID,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+f.sender.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), f.sender))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.sender.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp groupWakeResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.MessageID)
	return resp
}

func requireNoAgentWoken(t *testing.T, resp groupWakeResponse) {
	t.Helper()
	require.Empty(t, resp.WokenAgents)
	require.NotNil(t, resp.NoAgentWoken, "a group post that reached no agent must set no_agent_woken")
	require.True(t, *resp.NoAgentWoken)
}

func TestGroupDefaultAgentWake_NoMentionWakesDefault(t *testing.T) {
	f := newGroupWakeFixture(t)
	f.setDefault(t, f.target.ID)

	resp := f.post(t, "status update for the group", "")

	got := dispatchesTo(f.dispatcher, f.target.ID)
	require.Len(t, got, 1, "the default agent must receive the post exactly once")
	require.NotNil(t, got[0].StructuredMessage)
	require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)
	require.Equal(t, f.convID, got[0].StructuredMessage.ConversationID,
		"the copy must land in the group conversation")
	require.Equal(t, "default_agent", got[0].StructuredMessage.Metadata["mention_position"])
	require.Equal(t, resp.MessageID, got[0].StructuredMessage.Metadata["mention_of"])
	require.Empty(t, dispatchesTo(f.dispatcher, f.bystander.ID))

	require.NotNil(t, resp.DefaultAgentResult)
	require.Equal(t, f.target.Slug, resp.DefaultAgentResult.Slug)
	require.Equal(t, "delivered", resp.DefaultAgentResult.Status)
	require.Equal(t, []string{f.target.Slug}, resp.WokenAgents)
	require.Nil(t, resp.NoAgentWoken, "no_agent_woken must be omitted when an agent was woken")
}

func TestGroupDefaultAgentWake_AgentMentionLeavesDefaultAlone(t *testing.T) {
	f := newGroupWakeFixture(t)
	f.setDefault(t, f.target.ID)

	resp := f.post(t, "@"+mentionBystanderSlug+" please take this", "")

	require.Len(t, dispatchesTo(f.dispatcher, f.bystander.ID), 1)
	require.Empty(t, dispatchesTo(f.dispatcher, f.target.ID),
		"a post that @mentions an agent must not also reach the default agent")

	require.Nil(t, resp.DefaultAgentResult)
	require.Equal(t, []string{mentionBystanderSlug}, resp.WokenAgents)
	require.Nil(t, resp.NoAgentWoken)
}

func TestGroupDefaultAgentWake_FailedAgentMentionLeavesDefaultAlone(t *testing.T) {
	f := newGroupWakeFixture(t)
	f.setDefault(t, f.target.ID)

	resp := f.post(t, "@gdw-no-such-agent are you there", "")

	require.Empty(t, dispatchesTo(f.dispatcher, f.target.ID),
		"a post whose agent mention failed must not reach the default agent")
	require.Len(t, resp.MentionResults, 1)
	require.Equal(t, "not_found", resp.MentionResults[0].Status)
	require.Nil(t, resp.DefaultAgentResult)
	requireNoAgentWoken(t, resp)
}

func TestGroupDefaultAgentWake_NoDefaultReportsNoAgentWoken(t *testing.T) {
	f := newGroupWakeFixture(t)

	resp := f.post(t, "anyone around", "")

	require.Empty(t, dispatchesTo(f.dispatcher, f.target.ID))
	require.Empty(t, dispatchesTo(f.dispatcher, f.bystander.ID))
	require.Nil(t, resp.DefaultAgentResult)
	requireNoAgentWoken(t, resp)
}

func TestGroupDefaultAgentWake_DefaultDeniedPairwise(t *testing.T) {
	f := newGroupWakeFixture(t)
	ctx := context.Background()
	target, err := f.s.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	target.MessageMode = store.MessageModeNone
	require.NoError(t, f.s.UpdateAgent(ctx, target))
	f.setDefault(t, f.target.ID)

	resp := f.post(t, "status update for the group", "")

	require.Empty(t, dispatchesTo(f.dispatcher, f.target.ID),
		"a default agent the sender may not message must not receive the post")
	require.NotNil(t, resp.DefaultAgentResult)
	require.Equal(t, f.target.Slug, resp.DefaultAgentResult.Slug)
	require.Equal(t, "unauthorized", resp.DefaultAgentResult.Status)
	require.Equal(t, "message delivery denied", resp.DefaultAgentResult.Error,
		"the pairwise check must refuse before delivery is attempted")
	requireNoAgentWoken(t, resp)

	participants, err := f.s.ListParticipants(ctx, f.convID)
	require.NoError(t, err)
	for _, p := range participants {
		require.False(t, p.PrincipalKind == "agent" && p.PrincipalID == f.target.ID,
			"a refused default agent must not be registered as a participant")
	}
}

func TestGroupDefaultAgentWake_SelfPostDoesNotWakeSelf(t *testing.T) {
	f := newGroupWakeFixture(t)
	f.setDefault(t, f.sender.ID)

	resp := f.post(t, "my own update", "")

	require.Empty(t, dispatchesTo(f.dispatcher, f.sender.ID),
		"the default agent's own post must not be delivered back to it")
	require.NotNil(t, resp.DefaultAgentResult)
	require.Equal(t, f.sender.Slug, resp.DefaultAgentResult.Slug)
	require.Equal(t, "self", resp.DefaultAgentResult.Status)
	requireNoAgentWoken(t, resp)
}

func TestGroupDefaultAgentWake_ExcludedTypeDoesNotWake(t *testing.T) {
	f := newGroupWakeFixture(t)
	f.setDefault(t, f.target.ID)

	resp := f.post(t, "phase changed", messages.TypeStateChange)

	require.Empty(t, dispatchesTo(f.dispatcher, f.target.ID))
	require.Nil(t, resp.DefaultAgentResult)
	requireNoAgentWoken(t, resp)
}
