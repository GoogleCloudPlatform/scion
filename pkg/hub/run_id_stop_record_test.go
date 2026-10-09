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
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The hub records a stop only against the run it was dispatched for
// (ptone/scion#2550): a broker 202 for run X never marks a newer run Y
// stopped, suspended, or releases Y's reservation.

func TestRecordStopStatus_OnlyForTheStoppedRun(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	_, _, agent := setupOnlineBrokerAgent(t, s, "record-stop")
	if _, err := s.SetAgentRunID(ctx, agent.ID, "run-new", nil); err != nil {
		t.Fatal(err)
	}
	upd := store.AgentStatusUpdate{Phase: string(state.PhaseStopped), ContainerStatus: "stopped"}

	recorded, err := srv.recordStopStatus(ctx, agent.ID, "run-old", "stop", upd)
	if err != nil || recorded {
		t.Fatalf("stale run: recorded=%v err=%v, want false, nil", recorded, err)
	}
	if got := mustGetAgent(t, s, agent.ID); got.Phase != string(state.PhaseRunning) {
		t.Fatalf("stale run wrote phase %q", got.Phase)
	}

	recorded, err = srv.recordStopStatus(ctx, agent.ID, "run-new", "stop", upd)
	if err != nil || !recorded {
		t.Fatalf("current run: recorded=%v err=%v, want true, nil", recorded, err)
	}
	if got := mustGetAgent(t, s, agent.ID); got.Phase != string(state.PhaseStopped) {
		t.Fatalf("current run: phase %q, want stopped", got.Phase)
	}

	// A row from before run IDs (no run sent) records as before.
	recorded, err = srv.recordStopStatus(ctx, agent.ID, "", "stop", store.AgentStatusUpdate{Phase: string(state.PhaseRunning)})
	if err != nil || !recorded {
		t.Fatalf("no run: recorded=%v err=%v, want true, nil", recorded, err)
	}
}

func TestStopRunStillCurrent(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	_, _, agent := setupOnlineBrokerAgent(t, s, "stop-run-current")
	if _, err := s.SetAgentRunID(ctx, agent.ID, "run-new", nil); err != nil {
		t.Fatal(err)
	}
	if srv.stopRunStillCurrent(ctx, agent.ID, "run-old", "queued stop") {
		t.Error("run-old reported current while the row holds run-new")
	}
	if !srv.stopRunStillCurrent(ctx, agent.ID, "run-new", "queued stop") {
		t.Error("run-new not reported current")
	}
	if !srv.stopRunStillCurrent(ctx, agent.ID, "", "queued stop") {
		t.Error("an empty run must count as current")
	}
}

// runSwapStopClient answers a stop with success after a newer run has been
// minted and reported running, as when the row moves on while the stop for
// the older run is in flight.
type runSwapStopClient struct {
	mockRuntimeBrokerClient
	s       store.Store
	agentID string
}

func (c *runSwapStopClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, runID string) error {
	c.lastStopRunID = runID
	if _, err := c.s.SetAgentRunID(ctx, c.agentID, "run-new", nil); err != nil {
		return err
	}
	return c.s.UpdateAgentStatus(ctx, c.agentID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning), ContainerStatus: "running"})
}

// The review probe, through the HTTP stop and suspend handlers: the stop for
// run-old succeeds while the row moves to run-new; the row stays running.
func TestStop202ForOldRunDoesNotStopNewerRun(t *testing.T) {
	for _, action := range []string{"stop", "suspend"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			_, _, agent := setupOnlineBrokerAgent(t, s, "runswap-"+action)
			if _, err := s.SetAgentRunID(ctx, agent.ID, "run-old", nil); err != nil {
				t.Fatal(err)
			}
			client := &runSwapStopClient{s: s, agentID: agent.ID}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			if rec.Code >= 300 {
				t.Fatalf("%s: status %d: %s", action, rec.Code, rec.Body.String())
			}
			if client.lastStopRunID != "run-old" {
				t.Fatalf("stop dispatched for %q, want run-old", client.lastStopRunID)
			}
			got := mustGetAgent(t, s, agent.ID)
			if got.RunID != "run-new" || got.Phase != string(state.PhaseRunning) || got.ContainerStatus != "running" {
				t.Errorf("after the %s for run-old: run=%q phase=%q container=%q; want run-new left running",
					action, got.RunID, got.Phase, got.ContainerStatus)
			}
		})
	}
}

// The queued-stop variant: a stop queued for run-old drains after the row
// moved to run-new. The stop_queued status is left for run-new to settle,
// and nothing is recorded as stopped.
func TestExecDispatchStop_QueuedForOldRunLeavesNewerRun(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	_, _, agent := setupOnlineBrokerAgent(t, s, "queued-old-run")
	intentAt, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentStopped)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{ContainerStatus: containerStatusStopQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAgentRunID(ctx, agent.ID, "run-new", nil); err != nil {
		t.Fatal(err)
	}

	args, err := MarshalDispatchArgs(StopDispatchArgs{IntentAt: &intentAt, RunID: "run-old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.execDispatchStop(ctx, store.BrokerDispatch{AgentID: agent.ID, Op: "stop", Args: args}); err != nil {
		t.Fatalf("execDispatchStop: %v", err)
	}
	if client.lastStopRunID != "run-old" {
		t.Fatalf("stop dispatched for %q, want run-old", client.lastStopRunID)
	}
	if got := mustGetAgent(t, s, agent.ID); got.ContainerStatus != containerStatusStopQueued {
		t.Errorf("container status = %q, want %q left for run-new", got.ContainerStatus, containerStatusStopQueued)
	}

	// The same queued stop for the row's own run settles it.
	args, _ = MarshalDispatchArgs(StopDispatchArgs{IntentAt: &intentAt, RunID: "run-new"})
	if _, err := srv.execDispatchStop(ctx, store.BrokerDispatch{AgentID: agent.ID, Op: "stop", Args: args}); err != nil {
		t.Fatalf("execDispatchStop: %v", err)
	}
	if got := mustGetAgent(t, s, agent.ID); got.ContainerStatus != "stopped" {
		t.Errorf("own-run queued stop: container status = %q, want stopped", got.ContainerStatus)
	}
}
