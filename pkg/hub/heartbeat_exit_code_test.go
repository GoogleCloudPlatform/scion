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
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupHeartbeatExitCodeTest creates a server with a broker, project, and
// running agent so that heartbeat processing can be tested end-to-end.
func setupHeartbeatExitCodeTest(t *testing.T) (srv *Server, s store.Store, brokerID, projectID, agentSlug string) {
	t.Helper()

	srv, s = testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      tid("hb-broker"),
		Name:    "HB Broker",
		Slug:    "hb-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-project"),
		Slug:    "hb-project",
		Name:    "HB Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent := &store.Agent{
		ID:              tid("hb-agent"),
		Slug:            "hb-agent",
		Name:            "HB Agent",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return srv, s, broker.ID, project.ID, agent.Slug
}

// sendHeartbeat sends a broker heartbeat with the given agent heartbeat data
// and returns the HTTP status code.
func sendHeartbeat(t *testing.T, srv *Server, brokerID, projectID string, agentHB brokerAgentHeartbeat) int {
	t.Helper()
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{
			{
				ProjectID: projectID,
				Agents:    []brokerAgentHeartbeat{agentHB},
			},
		},
	}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/runtime-brokers/"+brokerID+"/heartbeat", hb)
	return rec.Code
}

// getAgentState retrieves the current agent state from the store.
func getAgentState(t *testing.T, s store.Store, slug, projectID string) *store.Agent {
	t.Helper()
	agent, err := s.GetAgentBySlug(context.Background(), projectID, slug)
	require.NoError(t, err)
	return agent
}

// TestHeartbeatExitCode_StructuredCrash verifies that a heartbeat with a
// non-zero ExitCode (structured path) promotes stopped → error and records
// the exit code and reason.
func TestHeartbeatExitCode_StructuredCrash(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	ec := 137
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "stopped",
		Activity:   "crashed",
		ExitCode:   &ec,
		ExitReason: "crashed",
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "error", got.Phase, "non-zero ExitCode should promote stopped to error")
	assert.Equal(t, "crashed", got.Activity)
	require.NotNil(t, got.ExitCode, "ExitCode should be persisted")
	assert.Equal(t, 137, *got.ExitCode)
	assert.Equal(t, "crashed", got.ExitReason)
	assert.Contains(t, got.Message, "exit code 137")
}

// TestHeartbeatExitCode_SuppressedDuringReincarnation proves a heartbeat
// reporting a crash (e.g. the OLD container dying mid-reprovision) cannot
// clobber Phase/Activity/ExitCode/ExitReason/Message while a `scion
// reincarnate` migration owns the agent: the worker is the sole authority for
// those fields until it completes or fails. ContainerStatus and the LastSeen
// bump are not worker-owned and must still apply. It runs for every in-flight
// state, and for both crash shapes: an empty activity (what live brokers
// send, which the notification dispatcher would match as ERROR) and
// "crashed".
func TestHeartbeatExitCode_SuppressedDuringReincarnation(t *testing.T) {
	states := []string{
		store.ReincarnationStatePending,
		store.ReincarnationStateStopping,
		store.ReincarnationStateProvisioning,
		store.ReincarnationStateStarting,
	}
	for _, state := range states {
		for _, activity := range []string{"", "crashed"} {
			t.Run(state+"/activity="+activity, func(t *testing.T) {
				srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

				oldLastSeen := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
				agent := getAgentState(t, s, agentSlug, projectID)
				agent.ReincarnationState = state
				agent.LastSeen = oldLastSeen
				require.NoError(t, s.UpdateAgent(context.Background(), agent))
				before := getAgentState(t, s, agentSlug, projectID)
				require.True(t, before.LastSeen.Equal(oldLastSeen), "sanity: the known old LastSeen must be stored")

				ec := 137
				code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
					Slug:            agentSlug,
					Phase:           "stopped",
					Activity:        activity,
					ExitCode:        &ec,
					ExitReason:      "crashed",
					Message:         "container exited",
					ContainerStatus: "exited (137)",
				})
				assert.Equal(t, http.StatusOK, code)

				got := getAgentState(t, s, agentSlug, projectID)
				assert.Equal(t, before.Phase, got.Phase, "phase must stay under the worker's control while reincarnating")
				assert.Equal(t, before.Activity, got.Activity, "activity must not be set from the racing container's crash")
				assert.Nil(t, got.ExitCode, "ExitCode must not be recorded from a heartbeat while reincarnating")
				assert.Equal(t, "", got.ExitReason)
				assert.Equal(t, before.Message, got.Message, "message must not be overwritten by the racing container's heartbeat")
				assert.Equal(t, "exited (137)", got.ContainerStatus, "ContainerStatus is not a worker-owned field and must still update")
				assert.True(t, got.LastSeen.After(oldLastSeen),
					"the LastSeen bump must still apply while reincarnating (was %v, now %v)", oldLastSeen, got.LastSeen)
			})
		}
	}
}

// TestHeartbeatExitCode_CrashStillWorksAfterFailedReincarnation is the
// counterpart regression test to
// TestHeartbeatExitCode_SuppressedDuringReincarnation: a FAILED reincarnation
// is terminal, not in flight (design §3.4 Amendment A11 item 2 — see
// reincarnationInFlight), so a heartbeat crash report must be processed
// completely normally once a reincarnation has already failed, exactly as it
// would be for an agent that never attempted one.
func TestHeartbeatExitCode_CrashStillWorksAfterFailedReincarnation(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	agent := getAgentState(t, s, agentSlug, projectID)
	agent.ReincarnationState = store.ReincarnationStateFailed
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	ec := 137
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "stopped",
		Activity:   "crashed",
		ExitCode:   &ec,
		ExitReason: "crashed",
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "error", got.Phase, "a failed (terminal) reincarnation must not suppress normal crash handling")
	assert.Equal(t, "crashed", got.Activity)
	require.NotNil(t, got.ExitCode)
	assert.Equal(t, 137, *got.ExitCode)
}

// TestHeartbeatExitCode_StructuredCleanExit verifies that ExitCode==0 keeps
// PhaseStopped (clean exit) and still records the exit code.
func TestHeartbeatExitCode_StructuredCleanExit(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	ec := 0
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:     agentSlug,
		Phase:    "stopped",
		Activity: "completed",
		ExitCode: &ec,
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "stopped", got.Phase, "ExitCode 0 should keep stopped phase")
	require.NotNil(t, got.ExitCode, "ExitCode 0 should be persisted")
	assert.Equal(t, 0, *got.ExitCode)
}

// TestHeartbeatExitCode_PreemptedEvictedMessage verifies that a Kubernetes
// pod disruption gets its own default Message instead of being reported as
// a generic crash, in both the non-zero-exit-code path (promotes stopped to
// error) and the zero-exit-code path (stays stopped).
func TestHeartbeatExitCode_PreemptedEvictedMessage(t *testing.T) {
	t.Run("preempted with a non-zero exit code promotes to error", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		ec := 137
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "stopped",
			ExitCode:   &ec,
			ExitReason: "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "error", got.Phase, "non-zero ExitCode should still promote stopped to error")
		assert.Equal(t, "preempted", got.ExitReason)
		assert.Equal(t, "Agent pod was preempted, exit code 137", got.Message)
	})

	t.Run("evicted with a zero exit code keeps stopped and gets no crash wording", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		ec := 0
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "stopped",
			ExitCode:   &ec,
			ExitReason: "evicted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "stopped", got.Phase, "ExitCode 0 should keep stopped phase")
		assert.Equal(t, "evicted", got.ExitReason)
		assert.Equal(t, "Agent pod was evicted", got.Message, "no exit code suffix expected when the exit code is not meaningful")
	})

	t.Run("ordinary crash keeps the existing wording unchanged", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		ec := 137
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "stopped",
			ExitCode:   &ec,
			ExitReason: "crashed",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "error", got.Phase)
		assert.Equal(t, "Agent crashed with exit code 137", got.Message, "regression: non-disruption crash wording must not change")
	})
}

// TestHeartbeatExitCode_CommittedDisruptionWhileRunning covers the other
// ordering for ptone/scion#2528: scheduler preemption and the eviction API
// usually delete the pod object outright once termination completes, often
// before any heartbeat ever sees a terminal phase. List() reports the
// disruption reason ahead of that, while the pod still has Phase=running
// (a deletionTimestamp plus a live DisruptionTarget condition). The hub
// must record that reason immediately, without treating the agent as
// stopped, so it is already on record once a later terminal report lands.
func TestHeartbeatExitCode_CommittedDisruptionWhileRunning(t *testing.T) {
	t.Run("records the reason and it survives a later stop report", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "running",
			ExitReason: "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		mid := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "running", mid.Phase, "phase must not change while the agent has not stopped yet")
		assert.Equal(t, "preempted", mid.ExitReason, "the reason must be recorded ahead of the pod actually stopping")

		// sciontool's own direct stop report lands later — a graceful SIGTERM
		// deletion usually lets sciontool report a plain stop before the
		// broker's next heartbeat even runs.
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+mid.ID+"/status",
			map[string]string{"phase": "stopped", "message": "Agent stopped"})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		final := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "stopped", final.Phase)
		assert.Equal(t, "preempted", final.ExitReason, "the reason recorded while running must survive the terminal transition")
	})

	t.Run("an already-stored reason is not overwritten by a later running heartbeat", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.ExitReason = "evicted"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "running",
			ExitReason: "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "evicted", got.ExitReason, "a stored reason must win over a later running heartbeat's reason")
	})

	t.Run("a non-disruption reason is not recorded while running", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "running",
			ExitReason: "crashed",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "running", got.Phase, "a non-disruption reason must not move the phase either")
		assert.Equal(t, "", got.ExitReason, "only preempted/evicted are recorded while running")
	})

	t.Run("the message is not touched by the running-phase backfill", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.Message = "custom operator note"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "running",
			ExitReason: "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "preempted", got.ExitReason)
		assert.Equal(t, "custom operator note", got.Message, "the running-phase backfill only ever sets ExitReason")
	})
}

// TestHeartbeatExitCode_RestartClearsRunningPhaseReason pins that a
// disruption reason recorded on a still-running agent (for example because
// the pod that triggered it disappeared without sciontool ever reporting a
// stop) does not survive a restart or start dispatched while the agent was
// already running: ExitReason/ExitCode describe the prior generation, not
// the one the restart/start is bringing up, so they must be cleared
// regardless of the current phase.
func TestHeartbeatExitCode_RestartClearsRunningPhaseReason(t *testing.T) {
	t.Run("restart", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "running",
			ExitReason: "preempted",
		})
		require.Equal(t, http.StatusOK, code)
		mid := getAgentState(t, s, agentSlug, projectID)
		require.Equal(t, "preempted", mid.ExitReason, "sanity: the reason must be recorded before the restart")

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+mid.ID+"/restart", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "running", got.Phase)
		assert.Equal(t, "", got.ExitReason, "a restart must clear a reason recorded against the prior generation")
	})

	t.Run("start", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "running",
			ExitReason: "evicted",
		})
		require.Equal(t, http.StatusOK, code)
		mid := getAgentState(t, s, agentSlug, projectID)
		require.Equal(t, "evicted", mid.ExitReason, "sanity: the reason must be recorded before start")

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+mid.ID+"/start", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "running", got.Phase)
		assert.Equal(t, "", got.ExitReason, "start on an already-running agent must also clear a stale reason")
	})
}

// TestHeartbeatExitCode_GracefulPreemptionAfterPlainStop covers the ordering
// in ptone/scion#2528: preemption and the eviction API delete the pod
// gracefully (SIGTERM), so sciontool reports a plain clean stop directly to the Hub
// before the broker's next heartbeat can observe the pod's disruption
// signal. By the time that heartbeat arrives the agent is already in a
// terminal phase (the agentInTerminalPhase branch), which must still
// persist ExitReason/ExitCode/Message rather than leave them empty.
func TestHeartbeatExitCode_GracefulPreemptionAfterPlainStop(t *testing.T) {
	t.Run("a generic stop message and empty ExitReason are backfilled", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.Phase = "stopped"
		agent.Message = "Agent stopped"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		ec := 0
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "stopped",
			ExitCode:   &ec,
			ExitReason: "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "stopped", got.Phase, "phase must stay untouched")
		assert.Equal(t, "preempted", got.ExitReason)
		assert.Equal(t, "Agent pod was preempted", got.Message)
		require.NotNil(t, got.ExitCode, "ExitCode must be backfilled, not just ExitReason")
		assert.Equal(t, 0, *got.ExitCode)
	})

	t.Run("a heartbeat phase differing from the stored phase does not move it", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.Phase = "stopped"
		agent.Message = "Agent stopped"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		ec := 137
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "error",
			ExitCode:   &ec,
			ExitReason: "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "stopped", got.Phase, "the stored phase must win even though the heartbeat reports a different one")
		assert.Equal(t, "preempted", got.ExitReason)
		require.NotNil(t, got.ExitCode)
		assert.Equal(t, 137, *got.ExitCode)
	})

	t.Run("a non-disruption reason is not backfilled", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.Phase = "stopped"
		agent.Message = "Agent stopped"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		ec := 0
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "stopped",
			ExitCode:   &ec,
			ExitReason: "crashed",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "", got.ExitReason, "only preempted/evicted are backfilled through this path")
		assert.Equal(t, "Agent stopped", got.Message, "an unrelated reason must not touch the generic message either")
	})

	t.Run("a non-generic stored message is preserved while ExitReason still backfills", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.Phase = "stopped"
		agent.Message = "custom operator note"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		ec := 0
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "stopped",
			ExitCode:   &ec,
			ExitReason: "evicted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "evicted", got.ExitReason, "ExitReason still backfills regardless of the message")
		assert.Equal(t, "custom operator note", got.Message, "a non-generic message must not be overwritten")
	})

	t.Run("an already-stored ExitReason is not overwritten", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.Phase = "error"
		agent.ExitReason = "crashed"
		agent.Message = "Agent crashed with exit code 1"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		ec := 0
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "error",
			ExitCode:   &ec,
			ExitReason: "evicted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "crashed", got.ExitReason, "a stored reason must win over a later heartbeat's reason")
		assert.Equal(t, "Agent crashed with exit code 1", got.Message)
	})

	t.Run("a nil heartbeat ExitCode falls back to the stored one in the message", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.Phase = "stopped"
		agent.Message = "Agent stopped"
		storedCode := 137
		agent.ExitCode = &storedCode
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		// The heartbeat carries the disruption reason but no structured exit
		// code (for example the pod never got far enough to report one).
		// statusUpdate.ExitCode = nil leaves the already-stored code
		// untouched, so the message should reflect that stored code rather
		// than reading as if no exit code were known at all.
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:       agentSlug,
			Phase:      "stopped",
			ExitCode:   nil,
			ExitReason: "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "preempted", got.ExitReason)
		require.NotNil(t, got.ExitCode, "the previously stored exit code must survive a nil heartbeat ExitCode")
		assert.Equal(t, 137, *got.ExitCode)
		assert.Equal(t, "Agent pod was preempted, exit code 137", got.Message, "the message must use the stored exit code, not read as if none were known")
	})
}

// TestHeartbeatExitCode_LegacyFallback verifies that when ExitCode is nil
// (old broker), the hub falls back to parsing the ContainerStatus string.
func TestHeartbeatExitCode_LegacyFallback(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:            agentSlug,
		Phase:           "stopped",
		ContainerStatus: "Exited (1) 5 minutes ago",
		// No ExitCode field — simulating an old broker
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "error", got.Phase, "legacy fallback should promote stopped to error")
	require.NotNil(t, got.ExitCode, "ExitCode should be derived from ContainerStatus")
	assert.Equal(t, 1, *got.ExitCode)
	assert.Contains(t, got.Message, "exit code 1")
}

// TestHeartbeatExitCode_LegacyCleanExit verifies that when ExitCode is nil
// and ContainerStatus shows exit code 0, the phase stays stopped.
func TestHeartbeatExitCode_LegacyCleanExit(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:            agentSlug,
		Phase:           "stopped",
		ContainerStatus: "Exited (0) 5 minutes ago",
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "stopped", got.Phase, "legacy clean exit should keep stopped phase")
}

// TestHeartbeatExitCode_NilExitCodeStillRecordsDisruption covers the legacy
// nil-ExitCode path: a terminal pod can carry a disruption signal (preempted/evicted) with no
// structured ExitCode and no ContainerStatus string that parses to a
// non-zero code — for example a pod evicted or preempted before its agent
// container ever started, or one with no container statuses at all. The
// legacy-fallback branch must still persist ExitReason and set a message,
// not silently drop both because its ContainerStatus parse found nothing.
func TestHeartbeatExitCode_NilExitCodeStillRecordsDisruption(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "stopped",
		ExitReason: "evicted",
		// No ExitCode and no parseable ContainerStatus — simulating a pod
		// that never got a container exit code.
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "stopped", got.Phase, "no non-zero exit code found, so phase stays stopped")
	assert.Equal(t, "evicted", got.ExitReason, "ExitReason must not be dropped just because no exit code parsed")
	assert.Equal(t, "Agent pod was evicted", got.Message)
}

// TestHeartbeatExitCode_InvalidReasonDropped verifies that an invalid
// ExitReason (non-terminal activity) is silently dropped.
func TestHeartbeatExitCode_InvalidReasonDropped(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	ec := 1
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "stopped",
		ExitCode:   &ec,
		ExitReason: "working", // invalid — not a terminal activity
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "error", got.Phase, "non-zero ExitCode should still promote to error")
	require.NotNil(t, got.ExitCode)
	assert.Equal(t, 1, *got.ExitCode)
	assert.Equal(t, "", got.ExitReason, "invalid exit reason must be dropped")
}

// TestHeartbeatExitCode_LimitsExceeded verifies that "limits_exceeded" is
// accepted as a valid ExitReason.
func TestHeartbeatExitCode_LimitsExceeded(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	ec := 1
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "stopped",
		ExitCode:   &ec,
		ExitReason: "limits_exceeded",
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "error", got.Phase)
	assert.Equal(t, "limits_exceeded", got.ExitReason)
}

// TestHeartbeatExitCode_CleanExitWithReason verifies that a clean exit
// (ExitCode 0) with a valid reason records the reason.
func TestHeartbeatExitCode_CleanExitWithReason(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	ec := 0
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "stopped",
		ExitCode:   &ec,
		ExitReason: "limits_exceeded",
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "stopped", got.Phase, "ExitCode 0 keeps stopped")
	assert.Equal(t, "limits_exceeded", got.ExitReason, "valid reason on clean exit should be recorded")
}

// TestHeartbeatExitCode_WireCompat_OldBrokerNoExitCode verifies backward
// compatibility: a heartbeat with no ExitCode field (old broker) that has
// a running container status doesn't erroneously set exit info.
func TestHeartbeatExitCode_WireCompat_OldBrokerNoExitCode(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	// Old broker sending running status — no ExitCode
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:            agentSlug,
		Phase:           "running",
		Activity:        "thinking",
		ContainerStatus: "Up 5 minutes",
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "running", got.Phase)
	assert.Nil(t, got.ExitCode, "running agent should not have ExitCode set")
	assert.Equal(t, "", got.ExitReason)
}

// TestHeartbeatExitCode_JSONWireFormat verifies that the ExitCode and
// ExitReason fields serialize/deserialize correctly in the heartbeat JSON,
// including the omitempty behavior for nil ExitCode.
func TestHeartbeatExitCode_JSONWireFormat(t *testing.T) {
	t.Run("nil ExitCode omitted from JSON", func(t *testing.T) {
		hb := brokerAgentHeartbeat{
			Slug:  "test-agent",
			Phase: "running",
		}
		data, err := json.Marshal(hb)
		require.NoError(t, err)

		var m map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &m))
		_, hasExitCode := m["exitCode"]
		assert.False(t, hasExitCode, "nil ExitCode should be omitted from JSON")
	})

	t.Run("non-nil ExitCode present in JSON", func(t *testing.T) {
		ec := 42
		hb := brokerAgentHeartbeat{
			Slug:       "test-agent",
			Phase:      "stopped",
			ExitCode:   &ec,
			ExitReason: "crashed",
		}
		data, err := json.Marshal(hb)
		require.NoError(t, err)

		var m map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &m))
		exitCode, ok := m["exitCode"]
		assert.True(t, ok, "non-nil ExitCode should be present in JSON")
		assert.Equal(t, float64(42), exitCode)
		assert.Equal(t, "crashed", m["exitReason"])
	})

	t.Run("ExitCode zero present in JSON", func(t *testing.T) {
		ec := 0
		hb := brokerAgentHeartbeat{
			Slug:     "test-agent",
			Phase:    "stopped",
			ExitCode: &ec,
		}
		data, err := json.Marshal(hb)
		require.NoError(t, err)

		var m map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &m))
		exitCode, ok := m["exitCode"]
		assert.True(t, ok, "ExitCode 0 should be present in JSON (not omitted)")
		assert.Equal(t, float64(0), exitCode)
	})

	t.Run("round-trip through JSON preserves nil", func(t *testing.T) {
		original := brokerAgentHeartbeat{
			Slug:  "test-agent",
			Phase: "running",
		}
		data, err := json.Marshal(original)
		require.NoError(t, err)

		var decoded brokerAgentHeartbeat
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.Nil(t, decoded.ExitCode, "nil ExitCode should round-trip as nil")
		assert.Equal(t, "", decoded.ExitReason)
	})
}

// TestHeartbeatExitCode_LegacyPhaseRunningRecordsDisruption covers the
// legacy (no structured Phase) branch: List() reports Phase="" for a
// non-terminal pod by design (represented via ContainerStatus instead), so
// a committed-disruption heartbeat for a still-running pod can land here
// too, not only in the structured non-terminal branch, whenever the
// broker's own phase tracking has nothing more specific to report. The
// reason must still be recorded.
func TestHeartbeatExitCode_LegacyPhaseRunningRecordsDisruption(t *testing.T) {
	t.Run("records the reason", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug: agentSlug,
			// No structured Phase — only ContainerStatus, like a non-terminal
			// List() result.
			ContainerStatus: "Running",
			ExitReason:      "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "running", got.Phase, "ContainerStatus must still derive the running phase")
		assert.Equal(t, "preempted", got.ExitReason, "the reason must not be dropped just because Phase was empty")
	})

	t.Run("an already-stored reason is not overwritten", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		agent := getAgentState(t, s, agentSlug, projectID)
		agent.ExitReason = "evicted"
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:            agentSlug,
			ContainerStatus: "Running",
			ExitReason:      "preempted",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "evicted", got.ExitReason, "a stored reason must win over a later legacy-path heartbeat's reason")
	})

	t.Run("a non-disruption reason is not recorded", func(t *testing.T) {
		srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug:            agentSlug,
			ContainerStatus: "Running",
			ExitReason:      "crashed",
		})
		assert.Equal(t, http.StatusOK, code)

		got := getAgentState(t, s, agentSlug, projectID)
		assert.Equal(t, "", got.ExitReason, "only preempted/evicted are recorded by the legacy-path branch")
	})
}

// TestStatusEndpoint_ClearExitNotSettableViaJSON pins ClearExit as internal
// to the lifecycle start/restart dispatch path: the status endpoint decodes
// its request body straight into store.AgentStatusUpdate, so a clearExit
// field reachable from JSON would let any status reporter erase a stored
// exit reason and code without going through start/restart at all.
func TestStatusEndpoint_ClearExitNotSettableViaJSON(t *testing.T) {
	srv, s, _, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	agent := getAgentState(t, s, agentSlug, projectID)
	agent.ExitReason = "preempted"
	ec := 137
	agent.ExitCode = &ec
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/status",
		map[string]interface{}{"clearExit": true})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "preempted", got.ExitReason, "clearExit must not be settable through the status endpoint's JSON body")
	require.NotNil(t, got.ExitCode)
	assert.Equal(t, 137, *got.ExitCode)
}
