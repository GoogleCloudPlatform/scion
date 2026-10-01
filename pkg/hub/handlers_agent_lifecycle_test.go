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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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

// TestUpdateAgentStatus_DispatchReadyLogsOnAgentStartedReport is a regression
// test for the real path that actually sets activity=working, confirmed live
// on hybval int2 (agent 4ef7f4c9, ptone/scion#2519 r1): a no-auth /
// drop-to-shell agent never runs a harness session, so the SessionStart hook
// never fires and "Session started" never reaches this handler. The only
// phase=running/activity=working report such an agent ever sends is
// sciontool init's own "Agent started" status, carrying Metadata
// ["startup_ms"] (see cmd/sciontool/commands/init.go). This pins the real
// wire shape of that report and asserts both the pre-existing startup_ms log
// and the new dispatch-ready since_create_ms log fire for it, while the
// harness-ready (SessionStart) line does not.
func TestUpdateAgentStatus_DispatchReadyLogsOnAgentStartedReport(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("project-dr"), Name: "Dispatch Ready Project", Slug: "dispatch-ready-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("agent-dr"),
		Slug:      "agent-dr-slug",
		Name:      "Agent Dispatch Ready",
		ProjectID: project.ID,
		Phase:     string(state.PhaseStarting),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Real wire shape of sciontool init's initial running report, confirmed
	// against the live hybval int2 journal.
	status := store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityWorking),
		Message:  "Agent started",
		Metadata: map[string]string{"startup_ms": "358"},
	}
	body, err := json.Marshal(status)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/status", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	logged := logBuf.String()
	assert.Contains(t, logged, "agent reported startup timing", "startup_ms line must still fire")
	assert.Contains(t, logged, "startup_ms=358")
	assert.Contains(t, logged, "dispatch ready: Agent started status received",
		"the dispatch-ready since_create_ms line must fire on the Agent started report")
	assert.Contains(t, logged, "since_create_ms=")
	assert.NotContains(t, logged, "harness ready",
		"the harness-ready (SessionStart) line must not fire for an Agent started report")
}
