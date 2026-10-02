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

// Tests that @mention and target resolution find an agent that sits beyond
// the first page of a project's agent listing. The store lists agents newest
// first, so the agent created before a large batch of fillers is the last
// one in store order. Each test first proves that a single-page read misses
// it, then asserts the handler still resolves it.
//
// Run: go test ./pkg/hub/ -run 'TestMentionPagination|TestMessagingTargetsPagination|TestConversationResolvePagination' -count=1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// createFillerAgents adds n agents to projectID, all created after any agent
// already in the project, so earlier agents sort after them in store order.
func createFillerAgents(t *testing.T, s store.Store, projectID string, ancestry []string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("filler-%04d", i)
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID:          tid(projectID + "-" + slug),
			Name:        slug,
			Slug:        slug,
			ProjectID:   projectID,
			MessageMode: store.MessageModeProject,
			Ancestry:    ancestry,
		}))
	}
}

// requireMissingFromFirstPage asserts that a single ListAgents page with opts
// does not contain agentID, i.e. that the fixture really places the agent
// past the first page.
func requireMissingFromFirstPage(t *testing.T, s store.Store, projectID, agentID string, opts store.ListOptions) {
	t.Helper()
	page, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: projectID}, opts)
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor, "fixture must span more than one page")
	for _, a := range page.Items {
		require.NotEqual(t, agentID, a.ID, "fixture must place the agent past the first page")
	}
}

// processMentions used to read a single 200-agent page, so an agent past the
// 200th in a project could not be @mentioned.
func TestMentionPagination_ProcessMentions_AgentPastFirstPage(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	// Bystander, target and sender already exist; 447 fillers give 450
	// agents in total, with the bystander among the oldest.
	createFillerAgents(t, s, project.ID, sender.Ancestry, 447)
	requireMissingFromFirstPage(t, s, project.ID, bystander.ID, store.ListOptions{Limit: 200})

	owner, err := s.GetUser(ctx, project.OwnerID)
	require.NoError(t, err)

	sm := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Recipient: "agent:" + target.Slug,
		Msg:       "hey @" + mentionBystanderSlug + " can you take a look?",
	}
	reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm, Mentions: []string{mentionBystanderSlug}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "member", "cli")))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1, "an agent past the first page must still receive its mention")
	require.NotNil(t, got[0].StructuredMessage)
	require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)

	// The reported result must say delivered too (it said not_found under
	// the single-page read).
	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.MentionResults, 1)
	require.Equal(t, mentionBystanderSlug, resp.MentionResults[0].Slug)
	require.Equal(t, "delivered", resp.MentionResults[0].Status)
}

// The store's default page holds 500 agents, so these fixtures need more than
// 500 agents for a single-page read to miss the target.
const pastDefaultPageFillers = 520

// handleMessagingTargetsResolve used to read a single default page, so an
// agent past it was reported as a nonexistent target (404).
func TestMessagingTargetsPagination_AgentPastFirstPage(t *testing.T) {
	srv, s, _, projectB, _, ownerB, _, agentB := cpmSetup(t)

	createFillerAgents(t, s, projectB, []string{ownerB.ID}, pastDefaultPageFillers)
	requireMissingFromFirstPage(t, s, projectB, agentB.ID, store.ListOptions{})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/messaging/targets/resolve?project=project-b&agent=agent-beta", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))
	rr := httptest.NewRecorder()
	srv.handleMessagingTargetsResolve(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "an agent past the first page must resolve: %s", rr.Body.String())
	var resp targetResolveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.Equal(t, "agent-beta", resp.Agent.Slug)
}

// handleConversationResolve's @agent path used to read a single default
// page, so an agent past it came back as an unknown peer.
func TestConversationResolvePagination_AgentPastFirstPage(t *testing.T) {
	srv, s, _, projectB, _, ownerB, _, agentB := cpmSetup(t)

	createFillerAgents(t, s, projectB, []string{ownerB.ID}, pastDefaultPageFillers)
	requireMissingFromFirstPage(t, s, projectB, agentB.ID, store.ListOptions{})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/resolve?reference=@agent-beta&project_id="+projectB, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))
	rr := httptest.NewRecorder()
	srv.handleConversationResolve(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp conversationResolveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.PeerAgent, "an agent past the first page must resolve as the peer")
	require.Equal(t, "agent-beta", resp.PeerAgent.Slug)
}
