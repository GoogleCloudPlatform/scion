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
