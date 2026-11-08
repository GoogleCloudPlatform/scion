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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// listedAgentStatus returns the phase and containerStatus of agentID in the
// project's agent list JSON.
func listedAgentStatus(t *testing.T, srv *Server, projectID, agentID string) (phase, containerStatus string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?projectId="+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct {
		Agents []struct {
			ID              string `json:"id"`
			Phase           string `json:"phase"`
			ContainerStatus string `json:"containerStatus"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	for _, a := range list.Agents {
		if a.ID == agentID {
			return a.Phase, a.ContainerStatus
		}
	}
	t.Fatalf("agent %s not in the list: %s", agentID, rec.Body.String())
	return "", ""
}

// ptone/scion#3607: after a stop, a heartbeat that still reports the
// container running (its phase is held) must not leave the agent list
// showing phase stopped next to a live containerStatus.
func TestHeartbeatContainerStatus_AgreesWithHeldPhase(t *testing.T) {
	cases := []struct {
		name string
		// intent is the run intent set before the stop (default stopped).
		intent store.RunIntent
		// stop is the write that stops the agent.
		stop store.AgentStatusUpdate
		// hb is the heartbeat entry that arrives after it (Slug is filled in).
		hb            brokerAgentHeartbeat
		wantPhase     string
		wantContainer string
	}{
		{
			name:          "hub stop, then structured running heartbeat",
			stop:          store.AgentStatusUpdate{Phase: string(state.PhaseStopped), ContainerStatus: "stopped"},
			hb:            brokerAgentHeartbeat{Phase: string(state.PhaseRunning), Activity: "idle", ContainerStatus: "running"},
			wantContainer: "stopped",
		},
		{
			// sciontool's final report carries no container status: the
			// phase change clears the old live one.
			name:          "agent's own stop report, then legacy Up heartbeat",
			stop:          store.AgentStatusUpdate{Phase: string(state.PhaseStopped), ReportSource: store.StatusReportSourceAgent},
			hb:            brokerAgentHeartbeat{ContainerStatus: "Up 5 minutes"},
			wantContainer: "",
		},
		{
			name:          "a heartbeat confirming the stop still updates the status",
			stop:          store.AgentStatusUpdate{Phase: string(state.PhaseStopped), ContainerStatus: "stopped"},
			hb:            brokerAgentHeartbeat{Phase: string(state.PhaseStopped), ContainerStatus: "exited (0)"},
			wantContainer: "exited (0)",
		},
		{
			// A start is under way: the live container is the intended
			// one, so its status applies (the phase stays held).
			name:          "run intent running, then structured running heartbeat",
			intent:        store.RunIntentRunning,
			stop:          store.AgentStatusUpdate{Phase: string(state.PhaseStopped), ContainerStatus: "stopped"},
			hb:            brokerAgentHeartbeat{Phase: string(state.PhaseRunning), Activity: "idle", ContainerStatus: "running"},
			wantContainer: "running",
		},
		{
			name:          "suspended, then structured running heartbeat",
			stop:          store.AgentStatusUpdate{Phase: string(state.PhaseSuspended), ContainerStatus: "stopped"},
			hb:            brokerAgentHeartbeat{Phase: string(state.PhaseRunning), Activity: "idle", ContainerStatus: "running"},
			wantPhase:     string(state.PhaseSuspended),
			wantContainer: "stopped",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			grantDevUserRuntimeBrokerAccess(t, s)
			ctx := context.Background()
			suffix := fmt.Sprint(i)

			project := &store.Project{ID: tid("project-cs-phase-" + suffix), Name: "CS Phase " + suffix, Slug: "cs-phase-" + suffix}
			require.NoError(t, s.CreateProject(ctx, project))
			broker := &store.RuntimeBroker{
				ID: tid("broker-cs-phase-" + suffix), Name: "CS Phase Broker " + suffix, Slug: "cs-phase-broker-" + suffix,
				Status: store.BrokerStatusOnline,
			}
			require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
			agent := &store.Agent{
				ID: tid("agent-cs-phase-" + suffix), Slug: "cs-phase-agent-" + suffix, Name: "CS Phase Agent " + suffix,
				ProjectID: project.ID, RuntimeBrokerID: broker.ID,
				Phase: string(state.PhaseRunning), ContainerStatus: "Up 5 minutes",
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			intent := tc.intent
			if intent == "" {
				intent = store.RunIntentStopped
			}
			_, err := s.SetRunIntent(ctx, agent.ID, intent)
			require.NoError(t, err)
			require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, tc.stop))

			hb := tc.hb
			hb.Slug = agent.Slug
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", brokerHeartbeatRequest{
				Status:   "online",
				Projects: []brokerProjectHeartbeat{{ProjectID: project.ID, AgentCount: 1, Agents: []brokerAgentHeartbeat{hb}}},
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			wantPhase := tc.wantPhase
			if wantPhase == "" {
				wantPhase = string(state.PhaseStopped)
			}
			phase, containerStatus := listedAgentStatus(t, srv, project.ID, agent.ID)
			assert.Equal(t, wantPhase, phase)
			assert.Equal(t, tc.wantContainer, containerStatus)
		})
	}
}

func TestHeartbeatContainerStatusHeld(t *testing.T) {
	cases := []struct {
		name      string
		agentHeld bool
		hbPhase   string
		hbStatus  string
		want      bool
	}{
		{"agent not held", false, "running", "running", false},
		{"structured running", true, "running", "running", true},
		{"structured starting", true, "starting", "", true},
		{"structured stopped confirms", true, "stopped", "exited (0)", false},
		{"structured error", true, "error", "exited (1)", false},
		{"legacy Up", true, "", "Up 3 minutes", true},
		{"legacy running", true, "", "running", true},
		{"legacy exited", true, "", "Exited (0) 1 minute ago", false},
		{"legacy restarting is not held", true, "", "Restarting (1) 5 seconds ago", false},
		{"legacy empty", true, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, heartbeatContainerStatusHeld(tc.agentHeld, tc.hbPhase, tc.hbStatus))
		})
	}
}

func TestHeartbeatAgentHeld(t *testing.T) {
	cases := []struct {
		phase  string
		intent store.RunIntent
		want   bool
	}{
		{string(state.PhaseStopped), store.RunIntentStopped, true},
		{string(state.PhaseError), store.RunIntentStopped, true},
		{string(state.PhaseHalted), "", true},
		{string(state.PhaseSuspended), store.RunIntentStopped, true},
		{string(state.PhaseStopped), store.RunIntentRunning, false},
		{string(state.PhaseSuspended), store.RunIntentRunning, false},
		{string(state.PhaseRunning), store.RunIntentStopped, false},
		{string(state.PhaseStarting), store.RunIntentRunning, false},
	}
	for _, tc := range cases {
		t.Run(tc.phase+"/"+string(tc.intent), func(t *testing.T) {
			agent := &store.Agent{Phase: tc.phase, RunIntent: tc.intent}
			assert.Equal(t, tc.want, heartbeatAgentHeld(agent))
		})
	}
}

// ptone/scion#3607: holding a stopped agent's live container status does
// not hide a stop that did not take effect. The heartbeat still records the
// container as observed running, so the intent-stop backstop stops the
// agent again, while the stored container status keeps agreeing with the
// stopped phase.
func TestHeartbeatContainerStatus_HeldAgentStillObservedRunning(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()
	_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStopped), ContainerStatus: "stopped"}))

	f.send(brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory(),
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{{
			Slug: a.Slug, Phase: string(state.PhaseRunning), Activity: "working", ContainerStatus: "running", RuntimeTarget: "docker",
		}}}},
	})

	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, "stopped", got.ContainerStatus, "the stored status agrees with the held phase")
	require.Eventually(t, func() bool { return d.stops.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the agent observed running with intent stopped is stopped again")
}
