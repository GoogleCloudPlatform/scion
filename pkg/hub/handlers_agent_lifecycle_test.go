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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuardAgentPhaseTransition_ReincarnationInFlightSuppressesStatus is the
// status-POST-path half of design §3.4 Amendment A11 item 2: a `sciontool
// /status` report racing an in-flight `scion reincarnate` migration (e.g. the
// OLD container's dying-gasp crash report, sent just as the worker tears it
// down to reprovision) must not clobber the fields the reincarnation worker
// owns — Phase, Activity, ExitCode, ExitReason, and Message — exactly like
// the existing suspended-sticky guard (Guard 0) already does for suspension.
func TestGuardAgentPhaseTransition_ReincarnationInFlightSuppressesStatus(t *testing.T) {
	for _, inFlightState := range []string{
		store.ReincarnationStatePending,
		store.ReincarnationStateStopping,
		store.ReincarnationStateProvisioning,
		store.ReincarnationStateStarting,
	} {
		t.Run(inFlightState, func(t *testing.T) {
			agent := &store.Agent{
				Phase:              "starting", // what the worker itself set
				Activity:           "",
				ReincarnationState: inFlightState,
			}
			ec := 137
			status := &store.AgentStatusUpdate{
				Phase:      "error",
				Activity:   "crashed",
				ExitCode:   &ec,
				ExitReason: "crashed",
				Message:    "container exited unexpectedly",
			}

			guardAgentPhaseTransition(agent, status)

			assert.Equal(t, "", status.Phase, "phase must be suppressed while a reincarnation is in flight")
			assert.Equal(t, "", status.Activity, "activity must be suppressed while a reincarnation is in flight")
			assert.Nil(t, status.ExitCode, "ExitCode must be suppressed while a reincarnation is in flight")
			assert.Equal(t, "", status.ExitReason, "ExitReason must be suppressed while a reincarnation is in flight")
			assert.Equal(t, "", status.Message, "Message must be suppressed while a reincarnation is in flight")
		})
	}
}

// TestGuardAgentPhaseTransition_ReincarnationNotInFlightAllowsStatus is the
// regression counterpart: neither ReincarnationStateNone (never migrated, or
// the previous migration already completed) nor ReincarnationStateFailed (a
// migration ended and the agent is independently owned again) should
// suppress a status update — a crash report must be processed completely
// normally in both cases, exactly as it always has been.
func TestGuardAgentPhaseTransition_ReincarnationNotInFlightAllowsStatus(t *testing.T) {
	for _, notInFlightState := range []string{
		store.ReincarnationStateNone,
		store.ReincarnationStateFailed,
	} {
		t.Run("state="+notInFlightState, func(t *testing.T) {
			agent := &store.Agent{
				Phase:              "running",
				Activity:           "working",
				ReincarnationState: notInFlightState,
			}
			ec := 137
			status := &store.AgentStatusUpdate{
				Phase:      "error",
				Activity:   "crashed",
				ExitCode:   &ec,
				ExitReason: "crashed",
				Message:    "container exited unexpectedly",
			}

			guardAgentPhaseTransition(agent, status)

			assert.Equal(t, "error", status.Phase)
			assert.Equal(t, "crashed", status.Activity)
			if assert.NotNil(t, status.ExitCode) {
				assert.Equal(t, 137, *status.ExitCode)
			}
			assert.Equal(t, "crashed", status.ExitReason)
			assert.Equal(t, "container exited unexpectedly", status.Message)
		})
	}
}

// TestGuardAgentPhaseTransition_SuspendedStillSuppressesStatus is a
// regression test for the pre-existing Guard 0 (suspended-sticky), pinning
// its behavior now that Guard 0b (reincarnation-sticky) sits next to it:
// suspension must still suppress Phase/Activity exactly as before.
func TestGuardAgentPhaseTransition_SuspendedStillSuppressesStatus(t *testing.T) {
	agent := &store.Agent{Phase: "suspended"}
	status := &store.AgentStatusUpdate{Phase: "stopped", Activity: "crashed"}

	guardAgentPhaseTransition(agent, status)

	assert.Equal(t, "", status.Phase)
	assert.Equal(t, "", status.Activity)
}

// TestUpdateAgentStatus_ReincarnationInFlight_PostedMessageDiscarded is the
// end-to-end half of Guard 0b's reincarnation-sticky rule (design Amendment
// A26.8): a self-reported status update carrying Phase and Activity while a
// migration is in flight must be entirely discarded by the real HTTP
// handler, not just by guardAgentPhaseTransition in isolation — the agent
// row must keep whatever the reincarnation worker itself wrote (e.g.
// "migrating to generation N"), never whatever a racing status POST tried to
// write.
func TestUpdateAgentStatus_ReincarnationInFlight_PostedMessageDiscarded(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = "starting"
	})
	// ReincarnationState is not a CreateAgent field (only a real reincarnate
	// or a direct UpdateAgent sets it), so it is set here the same way
	// TestReincarnateAgent_BackstopResetsOrphanAgentState does.
	agent.Message = "migrating to generation 2"
	agent.ReincarnationState = store.ReincarnationStateProvisioning
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	// Simulate the OLD container's dying-gasp crash report racing the
	// migration — the exact scenario Guard 0b exists for.
	body, err := json.Marshal(store.AgentStatusUpdate{
		Phase: "error", Activity: "crashed", Message: "container exited unexpectedly",
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/status", bytes.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), agentIdentityFor(agent.ID, project.ID, ScopeAgentStatusUpdate)))
	rec := httptest.NewRecorder()
	srv.updateAgentStatus(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "migrating to generation 2", final.Message,
		"Guard 0b must block the status POST's Message from landing while a reincarnation is in flight")
	assert.Equal(t, "starting", final.Phase, "Phase must also stay blocked")
}
