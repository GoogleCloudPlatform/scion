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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// TestUpdateAgentStatus_DispatchReadyAndHarnessReadyTiming is a table-driven
// regression test for the two real sources of activity=working, plus the
// adjacent startup_ms metadata parsing (see ptone/scion#2519). A no-auth /
// drop-to-shell agent never runs a harness session, so the SessionStart hook
// never fires and "Session started" never reaches this handler — the only
// phase=running/activity=working report such an agent ever sends is
// sciontool init's own "Agent started" status, carrying
// Metadata["startup_ms"] (see cmd/sciontool/commands/init.go). The cases
// cover both message sources, plus malformed and absent startup_ms, so a
// broken match or an unparseable value reaching the log fails the test.
func TestUpdateAgentStatus_DispatchReadyAndHarnessReadyTiming(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	srv, s := testServer(t)
	ctx := context.Background()

	newAgent := func(t *testing.T, suffix string) *store.Agent {
		t.Helper()
		project := &store.Project{
			ID: tid("project-dr-" + suffix), Name: "Dispatch Ready Project " + suffix,
			Slug: "dispatch-ready-project-" + suffix,
		}
		require.NoError(t, s.CreateProject(ctx, project))
		agent := &store.Agent{
			ID: tid("agent-dr-" + suffix), Slug: "agent-dr-slug-" + suffix,
			Name: "Agent Dispatch Ready " + suffix, ProjectID: project.ID,
			Phase: string(state.PhaseStarting),
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
		return agent
	}

	post := func(t *testing.T, agentID string, status store.AgentStatusUpdate) {
		t.Helper()
		body, err := json.Marshal(status)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/status", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	t.Run("Agent started with valid startup_ms logs dispatch-ready and startup timing", func(t *testing.T) {
		logBuf.Reset()
		agent := newAgent(t, "started")

		// Real wire shape of sciontool init's initial running report,
		// confirmed against the live hybval int2 journal.
		post(t, agent.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: string(state.ActivityWorking),
			Message:  "Agent started",
			Metadata: map[string]string{"startup_ms": "358"},
		})

		logged := logBuf.String()
		assert.Contains(t, logged, "agent reported startup timing", "startup_ms line must fire")
		assert.Contains(t, logged, "startup_ms=358")
		assert.Contains(t, logged, "dispatch ready: Agent started status received",
			"the dispatch-ready since_create_ms line must fire on the Agent started report")
		assert.Contains(t, logged, "since_create_ms=")
		assert.NotContains(t, logged, "harness ready",
			"the harness-ready (SessionStart) line must not fire for an Agent started report")
	})

	t.Run("Session started logs harness-ready, not dispatch-ready", func(t *testing.T) {
		logBuf.Reset()
		agent := newAgent(t, "session")

		// Real wire shape of the harness SessionStart hook (see
		// TestHubHandler_SessionStart_WirePayloadShape in
		// pkg/sciontool/hooks/handlers/hub_test.go for the sending side).
		post(t, agent.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: string(state.ActivityWorking),
			Message:  "Session started",
		})

		logged := logBuf.String()
		assert.Contains(t, logged, "harness ready: SessionStart status received",
			"the harness-ready since_create_ms line must fire on a Session started report")
		assert.Contains(t, logged, "since_create_ms=")
		assert.NotContains(t, logged, "dispatch ready",
			"the dispatch-ready (Agent started) line must not fire for a Session started report")
		assert.NotContains(t, logged, "agent reported startup timing")
	})

	for i, malformed := range []string{"abc", "", "12x"} {
		t.Run(fmt.Sprintf("malformed startup_ms %q never logs the raw value", malformed), func(t *testing.T) {
			logBuf.Reset()
			agent := newAgent(t, fmt.Sprintf("malformed-%d", i))

			post(t, agent.ID, store.AgentStatusUpdate{
				Metadata: map[string]string{"startup_ms": malformed},
			})

			logged := logBuf.String()
			assert.NotContains(t, logged, "agent reported startup timing",
				"an unparseable startup_ms must never produce a startup-timing line")
			if malformed != "" {
				assert.NotContains(t, logged, malformed,
					"the unparseable raw startup_ms value must never be logged")
			}
		})
	}

	t.Run("absent metadata logs nothing startup- or dispatch-related", func(t *testing.T) {
		logBuf.Reset()
		agent := newAgent(t, "nometa")

		post(t, agent.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: string(state.ActivityWorking),
		})

		logged := logBuf.String()
		assert.NotContains(t, logged, "agent reported startup timing")
		assert.NotContains(t, logged, "dispatch ready")
		assert.NotContains(t, logged, "harness ready")
	})
}

// TestStatusUpdateIsEmpty_EveryFieldCounts catches drift between
// store.AgentStatusUpdate and statusUpdateIsEmpty: a field added to the
// struct but not to the predicate would let a report carrying only that
// field be dropped as a guarded no-op ({"applied":false}) during a delete
// or reincarnation. Each field is set to a non-zero value in turn.
func TestStatusUpdateIsEmpty_EveryFieldCounts(t *testing.T) {
	require.True(t, statusUpdateIsEmpty(store.AgentStatusUpdate{}), "the zero value is empty")

	typ := reflect.TypeOf(store.AgentStatusUpdate{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			var su store.AgentStatusUpdate
			v := reflect.ValueOf(&su).Elem().Field(i)
			switch v.Kind() {
			case reflect.String:
				v.SetString("x")
			case reflect.Bool:
				v.SetBool(true)
			case reflect.Pointer:
				elem := reflect.New(v.Type().Elem())
				v.Set(elem)
			case reflect.Map:
				m := reflect.MakeMap(v.Type())
				m.SetMapIndex(reflect.ValueOf("k").Convert(v.Type().Key()), reflect.Zero(v.Type().Elem()))
				v.Set(m)
			default:
				t.Fatalf("unhandled kind %s for AgentStatusUpdate.%s: extend this test and statusUpdateIsEmpty", v.Kind(), field.Name)
			}
			require.False(t, reflect.ValueOf(su).IsZero(), "sanity: the field must be non-zero")
			assert.False(t, statusUpdateIsEmpty(su),
				"statusUpdateIsEmpty must report false when AgentStatusUpdate.%s is set", field.Name)
		})
	}
}
