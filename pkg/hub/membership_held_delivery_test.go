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

// Delivery to a held agent (ptone/scion#3433), through each ingress: a held
// target, whatever its phase, keeps the message but is never dispatched to.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdAgentFor places an active hold on agentID for root rootID.
func holdAgentFor(t *testing.T, s store.Store, agentID, projectID, rootID string) {
	t.Helper()
	_, err := s.CreateAgentHolds(context.Background(), []*store.AgentHold{{
		AgentID: agentID, ProjectID: projectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: rootID,
		Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "test",
	}})
	require.NoError(t, err)
}

// An agent-to-agent message to a held, running target is refused like a
// suspended target; a mention to it is kept undispatched.
func TestHeldTargetDelivery_AgentDM(t *testing.T) {
	srv, s, project, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()
	holdAgentFor(t, s, target.ID, project.ID, target.Ancestry[0])

	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "to-held"))
	require.NotNil(t, dmErr, "a send to a held target is refused")
	assert.Nil(t, result)
	assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "A hub admin can lift the hold")

	in := deliveryDMInput(sender, target, "mention-to-held")
	in.Type = messages.TypeMention
	in.SkipPhaseGate = true
	result, dmErr = srv.ExecuteAgentDM(ctx, in)
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMDeferred, result.Outcome, "a mention to a held target is kept, not dispatched")
	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDeferred, msg.DispatchState)
}

// A chat send whose primary agent is held (and running) is kept as a failed
// row and never dispatched.
func TestHeldTargetDelivery_ChatPrimary(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, a := unreachableTestSetup(t, "running", false, d)
	holdAgentFor(t, s, a.ID, a.ProjectID, DevUserID)
	code, resp, m := unreachableSend(t, srv, s, topic, "hello")
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	require.NotNil(t, m)
	assert.Equal(t, store.MessageDispatchFailed, m.DispatchState)
	require.NotNil(t, m.DispatchFailureReason)
	assert.Equal(t, "Agent unreachable (suspended)", *m.DispatchFailureReason)
	assert.Empty(t, d.getMessages(), "no dispatch to a held primary")
}

// A chat send that mentions a held secondary agent dispatches to the primary
// only.
func TestHeldTargetDelivery_ChatSecondary(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	d := &brokerMockDispatcher{}
	srv.SetDispatcher(d)
	primary := &store.Agent{ID: tid("held-chat-primary"), ProjectID: proj.ID, Name: "Primary", Slug: "held-chat-primary",
		Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	secondary := &store.Agent{ID: tid("held-chat-secondary"), ProjectID: proj.ID, Name: "Secondary", Slug: "held-chat-secondary",
		Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID}
	for _, a := range []*store.Agent{primary, secondary} {
		require.NoError(t, s.CreateAgent(ctx, a))
	}
	holdAgentFor(t, s, secondary.ID, proj.ID, DevUserID)
	topicID := tid("held-chat-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "held-chat",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: primary.Slug}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "please help @held-chat-secondary"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var slugs []string
	for _, m := range d.getMessages() {
		slugs = append(slugs, m.agentSlug)
	}
	assert.Contains(t, slugs, primary.Slug)
	assert.NotContains(t, slugs, secondary.Slug, "no dispatch to a held secondary")
}

// A broker-relayed inbound message to a held, running agent is refused like
// a suspended agent.
func TestHeldTargetDelivery_BrokerInbound(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	user := &store.User{ID: tid("held-inbound-user"), Email: "held-inbound@example.com", DisplayName: "Held Inbound",
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)
	project := &store.Project{ID: tid("held-inbound-proj"), Slug: "held-inbound-proj", Name: "Held Inbound",
		OwnerID: user.ID, CreatedBy: user.ID, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)
	agent := &store.Agent{ID: tid("held-inbound-agent"), Slug: "held-inbound-agent", Name: "Held Inbound Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
		OwnerID: user.ID, StateVersion: 1, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateAgent(ctx, agent))
	holdAgentFor(t, s, agent.ID, project.ID, user.ID)

	payload := inboundMessageRequest{
		Topic: "scion.project." + project.ID + ".agent." + agent.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339), Channel: "discord",
			Sender: "user:" + user.Email, Recipient: "agent:" + agent.Slug, Msg: "hello", Type: messages.TypeInstruction,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeAgentNotRunning, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "is suspended")
}
