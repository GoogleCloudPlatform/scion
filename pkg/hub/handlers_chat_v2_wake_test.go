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

// Tests for wake-on-send in chat v2: a send to a suspended primary can ask
// the hub to offer a wake (offer_wake: 409 instead of a failed row) or to
// wake the agent and deliver the message as its first input (wake).
package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chatWakeFixture struct {
	srv   *Server
	s     store.Store
	proj  *store.Project
	topic string
	agent *store.Agent
	disp  *wakeTrackingDispatcher
}

// chatWakeSetup creates a project with an online broker, an agent in the
// given phase on that broker (owned by the dev user), and a topic whose
// default agent is that agent.
func chatWakeSetup(t *testing.T, phase string) *chatWakeFixture {
	t.Helper()
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := t.Context()
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	broker := &store.RuntimeBroker{
		ID: tid("chat-wake-broker-" + phase), Name: "Chat Wake Broker",
		Slug: "chat-wake-broker-" + phase, Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		Status: store.BrokerStatusOnline,
	}))

	a := &store.Agent{
		ID: tid("chat-wake-" + phase), ProjectID: proj.ID, Name: "Sleepy",
		Slug: "chat-wake-" + phase, Phase: phase, RuntimeBrokerID: broker.ID,
		OwnerID: DevUserID, CreatedBy: DevUserID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, a))

	topicID := tid("chat-wake-topic-" + phase)
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "chat-wake-" + phase,
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: a.Slug,
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)
	return &chatWakeFixture{srv: srv, s: s, proj: proj, topic: topicID, agent: a, disp: disp}
}

// memberWithoutLifecycle creates a project member who may message the
// fixture's agent (agent.message granted) but does not own it, so the
// lifecycle permission the start route requires is not held.
func (f *chatWakeFixture) memberWithoutLifecycle(t *testing.T) *store.User {
	t.Helper()
	ctx := t.Context()
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	u := &store.User{
		ID: tid("chat-wake-member"), Email: "chat-wake-member@example.com",
		DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.s.CreateUser(ctx, u))
	msgAuthzAddProjectMember(t, f.s, u.ID, f.proj.ID, f.proj.Slug, store.GroupMemberRoleMember)
	msgAuthzGrantAgentMessage(t, f.s, u.ID, f.proj.ID)
	require.False(t, f.srv.agentLifecycleAllowed(ctx, userIdentityFor(u), f.agent),
		"fixture member must not hold the lifecycle permission")
	return u
}

func userIdentityFor(u *store.User) UserIdentity {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
}

func (f *chatWakeFixture) path() string {
	return "/api/v1/chat/conversations/" + f.topic + "/messages"
}

func decodeWakeResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	return resp
}

// countThreadMessages returns how many rows the topic thread holds.
func (f *chatWakeFixture) countThreadMessages(t *testing.T) int {
	t.Helper()
	res, err := f.s.ListMessages(t.Context(), store.MessageFilter{ThreadID: f.topic}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return len(res.Items)
}

// markReadySoon simulates the resumed harness reporting its first activity.
func (f *chatWakeFixture) markReadySoon() {
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = f.s.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseRunning), Activity: "idle",
		})
	}()
}

// offer_wake to a suspended primary the caller may wake answers 409 with
// canWake=true and persists nothing, so the client can ask the user.
func TestChatV2Wake_OfferWake_Suspended_Conflict(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "offer_wake": true})
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())

	resp := decodeWakeResp(t, rec)
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, ErrCodeAgentNotRunning, errObj["code"])
	details, _ := errObj["details"].(map[string]any)
	require.NotNil(t, details)
	assert.Equal(t, true, details["canWake"])
	assert.Equal(t, "suspended", details["phase"])
	assert.Equal(t, f.agent.Slug, details["agentSlug"])

	assert.Equal(t, 0, f.countThreadMessages(t), "no row may be persisted")
	assert.Empty(t, f.disp.getStartCalls())
	assert.Empty(t, f.disp.getMessageCalls())
}

// offer_wake does not change a stopped primary: it is not wakeable, so the
// ordinary failed "Agent unreachable (stopped)" row is kept.
func TestChatV2Wake_OfferWake_Stopped_FailedRow(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseStopped))
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "offer_wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decodeWakeResp(t, rec)
	assert.Equal(t, "failed", resp["dispatchState"])
	assert.Equal(t, "Agent unreachable (stopped)", resp["dispatchFailureReason"])
	assert.Empty(t, f.disp.getStartCalls())
}

// offer_wake without the lifecycle permission keeps the non-wake error: a
// failed row, no 409, no resume.
func TestChatV2Wake_OfferWake_NoPermission_FailedRow(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	member := f.memberWithoutLifecycle(t)
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "offer_wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decodeWakeResp(t, rec)
	assert.Equal(t, "failed", resp["dispatchState"])
	assert.Equal(t, "agent_unreachable", resp["dispatchFailureCode"])
	assert.Empty(t, f.disp.getStartCalls())
}

// wake resumes the suspended primary (continue=true), waits for readiness
// and then delivers the message as its first input.
func TestChatV2Wake_Wake_Suspended_ResumesThenDelivers(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	f.markReadySoon()
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "wake up please", "wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	resp := decodeWakeResp(t, rec)
	assert.Equal(t, "dispatched", resp["dispatchState"])

	starts := f.disp.getStartCalls()
	require.Len(t, starts, 1, "the agent must be resumed exactly once")
	assert.True(t, starts[0].Continue, "wake must resume the previous session")
	msgs := f.disp.getMessageCalls()
	require.Len(t, msgs, 1)
	assert.Equal(t, f.agent.ID, msgs[0].AgentID)

	got, err := f.s.GetAgent(t.Context(), f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	m, err := f.s.GetMessage(t.Context(), resp["id"].(string))
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, m.DispatchState)
}

// wake without the lifecycle permission is refused with 403 before any
// resume, and nothing is persisted.
func TestChatV2Wake_Wake_NoPermission_Forbidden(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	member := f.memberWithoutLifecycle(t)
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true})
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, f.disp.getStartCalls())
	assert.Empty(t, f.disp.getMessageCalls())
	assert.Equal(t, 0, f.countThreadMessages(t))
}

// A failed resume returns the wake error and persists nothing, so the
// client keeps the draft.
func TestChatV2Wake_Wake_ResumeFails_NoRow(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseSuspended))
	f.disp.startReturnErr = assert.AnError
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true})
	require.Equal(t, http.StatusBadGateway, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Failed to wake agent")
	assert.Empty(t, f.disp.getMessageCalls())
	assert.Equal(t, 0, f.countThreadMessages(t))
}

// wake to a running primary is a no-op: no resume, ordinary delivery.
func TestChatV2Wake_Wake_Running_NoResume(t *testing.T) {
	f := chatWakeSetup(t, string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, f.path(),
		map[string]any{"content": "hello", "wake": true})
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, f.disp.getStartCalls())
	assert.Len(t, f.disp.getMessageCalls(), 1)
}
