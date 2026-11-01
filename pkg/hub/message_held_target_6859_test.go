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

// The hold check applies to every send to an agent (ptone/scion#6859): a
// user send and an agent send, with or without wake, to a held target is
// refused with the held-target refusal (409 agent_not_running) before any
// wake, dispatch or write; the same send to a target that is not held still
// goes through.

import (
	"context"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldSendFixture is a sender and target (in targetPhase) with a dispatcher
// that records starts and message dispatches; the target is held when held
// is set.
func heldSendFixture(t *testing.T, targetPhase string, held bool) (*Server, store.Store, *store.Agent, *store.Agent, *wakeTrackingDispatcher) {
	t.Helper()
	srv, s, sender, target := createWakeDMFixtures(t, targetPhase)
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	if held {
		holdAgentFor(t, s, target.ID, target.ProjectID, target.Ancestry[0])
	}
	return srv, s, sender, target, disp
}

// assertNothingHappened checks that a refused send resumed, dispatched and
// stored nothing, and left the target's phase alone.
func assertNothingHappened(t *testing.T, s store.Store, target *store.Agent, disp *wakeTrackingDispatcher) {
	t.Helper()
	assert.Empty(t, disp.getStartCalls(), "no resume may be dispatched")
	assert.Empty(t, disp.getMessageCalls(), "no message may be dispatched")
	assert.Zero(t, storedMessagesFor(t, s, target.ID), "no message may be stored")
	got, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, target.Phase, got.Phase)
}

func TestHeldTargetSend_User(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		wake  bool
	}{
		{"noWakeRunning", string(state.PhaseRunning), false},
		{"wakeRunning", string(state.PhaseRunning), true},
		// Before ptone/scion#6859 this reached the wake and was refused by
		// the start gate as a 409 "conflict".
		{"wakeSuspended", string(state.PhaseSuspended), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, target, disp := heldSendFixture(t, tc.phase, true)
			u := wakeLifecycleUser(t, s, "held-user-"+tc.name, target.ProjectID, "agent.message", "agent.read", "agent.lifecycle")

			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
				map[string]interface{}{"message": "to held", "wake": tc.wake})

			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			apiErr := decodeTargetAPIError(t, rec)
			assert.Equal(t, ErrCodeAgentNotRunning, apiErr.Code)
			assert.Equal(t, heldTargetDMError(target).Message, apiErr.Message)
			assertNothingHappened(t, s, target, disp)
		})
	}

	// Control: the same sends to a running target that is not held go
	// through.
	for _, wake := range []bool{false, true} {
		srv, s, _, target, disp := heldSendFixture(t, string(state.PhaseRunning), false)
		u := wakeLifecycleUser(t, s, "unheld-user", target.ProjectID, "agent.message", "agent.read", "agent.lifecycle")
		rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
			map[string]interface{}{"message": "to unheld", "wake": wake})
		require.Equal(t, http.StatusOK, rec.Code, "wake=%v: %s", wake, rec.Body.String())
		require.Len(t, disp.getMessageCalls(), 1, "wake=%v", wake)
		assert.Equal(t, 1, storedMessagesFor(t, s, target.ID), "wake=%v", wake)
	}
}

func TestHeldTargetSend_Agent(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		wake  bool
	}{
		{"noWakeRunning", string(state.PhaseRunning), false},
		// Before ptone/scion#6859 the hold check was skipped when wake was
		// requested: a held running target was dispatched to.
		{"wakeRunning", string(state.PhaseRunning), true},
		{"wakeSuspended", string(state.PhaseSuspended), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, sender, target, disp := heldSendFixture(t, tc.phase, true)
			wakeStarted := false
			result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
				SenderAgent:    sender,
				SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle),
				TargetAgent:    target,
				Msg:            "to held",
				Type:           "instruction",
				ProjectID:      sender.ProjectID,
				Wake:           tc.wake,
				BeforeWake:     func() { wakeStarted = true },
			})

			assert.Nil(t, result)
			require.NotNil(t, dmErr)
			assert.Equal(t, heldTargetDMError(target), dmErr)
			assert.False(t, wakeStarted, "a held target is never woken")
			assertNothingHappened(t, s, target, disp)
		})
	}

	// Control: the same sends to a running target that is not held go
	// through.
	for _, wake := range []bool{false, true} {
		srv, s, sender, target, disp := heldSendFixture(t, string(state.PhaseRunning), false)
		result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
			SenderAgent:    sender,
			SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle),
			TargetAgent:    target,
			Msg:            "to unheld",
			Type:           "instruction",
			ProjectID:      sender.ProjectID,
			Wake:           wake,
		})
		require.Nil(t, dmErr, "wake=%v", wake)
		require.NotNil(t, result)
		assert.Equal(t, AgentDMAccepted, result.Outcome, "wake=%v", wake)
		require.Len(t, disp.getMessageCalls(), 1, "wake=%v", wake)
	}
}
