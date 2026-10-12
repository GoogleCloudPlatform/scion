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

//go:build !no_sqlite && (!hubshard || hubshard_3)

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The phase gate shared by direct messages, group sends and the message
// broker proxy accepts an initializing agent: the message is delivered into
// the broker buffer, which waits for harness readiness. No new rejection.
func TestValidateAgentDeliverable_InitializingDelivers(t *testing.T) {
	for _, activity := range []string{"initializing", "working", ""} {
		a := &store.Agent{ID: "a1", Slug: "a1", Phase: string(state.PhaseRunning), Activity: activity}
		assert.Nil(t, validateAgentDeliverable(a), "activity %q", activity)
		assert.Nil(t, validateGroupMemberDeliverable(a), "activity %q", activity)
	}
}

// A broadcast targets an initializing agent like any running agent (the
// message is dispatched and buffered by the broker) and counts it in the
// additive "starting" field. An interrupt to it is delivered as an ordinary
// message; the working peer keeps the interrupt.
func TestBroadcast_InitializingAgentTargetedAndCounted(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	disp := &brokerMockDispatcher{}
	srv.SetDispatcher(disp)

	project := &store.Project{ID: api.NewUUID(), Name: "init-bcast", Slug: "init-bcast"}
	require.NoError(t, s.CreateProject(ctx, project))
	mk := func(slug, activity string) *store.Agent {
		a := &store.Agent{
			ID: api.NewUUID(), Name: slug, Slug: slug, ProjectID: project.ID,
			Phase: string(state.PhaseRunning), RuntimeBrokerID: "test-broker",
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		if activity != "" {
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Activity: activity}))
		}
		return a
	}
	sender := mk("bcast-sender", "working")
	mk("bcast-starting", "initializing")
	mk("bcast-ready", "working")

	body, _ := json.Marshal(BroadcastMessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      messages.TypeInstruction,
			Sender:    "agent:" + sender.Slug,
			Msg:       "hello project",
		},
		Interrupt: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: project.ID,
		Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
	}}))
	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())

	var resp BroadcastAcceptedResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Targeted)
	assert.Equal(t, 1, resp.Starting)
	assert.Equal(t, 0, resp.Skipped)

	got := map[string]bool{}
	for _, m := range disp.getMessages() {
		got[m.agentSlug] = m.interrupt
	}
	require.Contains(t, got, "bcast-starting", "the initializing agent must receive the broadcast")
	require.Contains(t, got, "bcast-ready")
	assert.False(t, got["bcast-starting"], "interrupt downgraded to a buffered delivery")
	assert.True(t, got["bcast-ready"], "a ready agent keeps the interrupt")
}

// Auto-suspend skips an agent whose stall began while it was initializing,
// and still suspends one that stalled while working.
func TestAutoSuspend_SkipsStalledWhileInitializing(t *testing.T) {
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	close(disp.release)
	srv.SetDispatcher(disp)
	ctx := context.Background()

	initAgent := newStalledBrokerAgent(t, s, "as-init")
	workAgent := newStalledBrokerAgent(t, s, "as-work")
	db := s.(*entadapter.CompositeStore).DB()
	for id, from := range map[string]string{initAgent.ID: "initializing", workAgent.ID: "working"} {
		_, err := db.ExecContext(ctx, "UPDATE agents SET stalled_from_activity = ? WHERE id = ?", from, id)
		require.NoError(t, err)
	}
	a, err := s.GetAgent(ctx, initAgent.ID)
	require.NoError(t, err)
	require.Equal(t, "initializing", a.StalledFromActivity)
	b, err := s.GetAgent(ctx, workAgent.ID)
	require.NoError(t, err)

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*a, *b})

	assert.Equal(t, []string{workAgent.ID}, disp.stopped())
	gotInit, err := s.GetAgent(ctx, initAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotInit.Phase, "stalled-while-initializing agent stays running")
}

// A wake whose agent is still initializing when the wake wait ends must not
// paste an interrupt into the starting harness: the caller's agent records
// the woken run's activity, so the interrupt is delivered as an ordinary
// buffered message. An agent that reports working keeps the interrupt.
func TestExecuteAgentDM_WakeInterrupt_InitializingIsBuffered(t *testing.T) {
	orig := wakeReadyTimeout
	wakeReadyTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { wakeReadyTimeout = orig })

	for _, tc := range []struct {
		activity      string
		wantInterrupt bool
	}{
		{"initializing", false},
		{"working", true},
	} {
		t.Run(tc.activity, func(t *testing.T) {
			srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
			disp := &wakeTrackingDispatcher{}
			srv.SetDispatcher(disp)

			// The resumed container reports running with the activity.
			go func() {
				time.Sleep(200 * time.Millisecond)
				_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
					Phase:    string(state.PhaseRunning),
					Activity: tc.activity,
				})
			}()

			result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
				SenderAgent:    sender,
				SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle),
				TargetAgent:    target,
				Msg:            "stop and look",
				Type:           "instruction",
				ProjectID:      sender.ProjectID,
				Wake:           true,
				Interrupt:      true,
			})
			require.Nil(t, dmErr)
			require.NotNil(t, result)
			assert.Equal(t, AgentDMAccepted, result.Outcome)

			msgs := disp.getMessageCalls()
			require.Len(t, msgs, 1, "the message is delivered")
			assert.Equal(t, tc.wantInterrupt, msgs[0].Interrupt)
		})
	}
}
