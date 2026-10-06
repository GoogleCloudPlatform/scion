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
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Every caller that records a stop leaves a row that moved to a newer run
// untouched: no status write, the reservation held, nothing published
// (ptone/scion#2550). The restart and suspend cases assert final row and
// credential state.

// runSwapStopDispatcher answers a stop with success after moving the row to
// run-new, running, as when a newer run is minted while the stop for the
// older run is in flight.
type runSwapStopDispatcher struct {
	quotaLifecycleDispatcher
	s store.Store
}

func (d *runSwapStopDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	d.stopCount.Add(1)
	if _, err := d.s.SetAgentRunID(ctx, agent.ID, "run-new"); err != nil {
		return err
	}
	return d.s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning), ContainerStatus: "running"})
}

// runSwapQuotaAgent is a running agent on run-old holding one broker
// reservation, behind a dispatcher whose stop moves the row to run-new.
func runSwapQuotaAgent(t *testing.T, name string) (*Server, store.Store, *store.RuntimeBroker, *store.Agent, *trackingEventPublisher) {
	t.Helper()
	srv, s := testServer(t)
	setBrokerAgentCeiling(t, s, 5)
	broker, project := newQuotaTestBrokerAndProject(t, s, name)
	a := newQuotaTestAgent(t, s, broker, project, name, state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, a.ID)
	if _, err := s.SetAgentRunID(context.Background(), a.ID, "run-old"); err != nil {
		t.Fatal(err)
	}
	a.RunID = "run-old"
	srv.SetDispatcher(&runSwapStopDispatcher{s: s})
	ep := &trackingEventPublisher{}
	srv.events = ep
	return srv, s, broker, a, ep
}

func assertNewerRunUntouched(t *testing.T, s store.Store, broker *store.RuntimeBroker, a *store.Agent, ep *trackingEventPublisher) {
	t.Helper()
	got := mustGetAgent(t, s, a.ID)
	if got.RunID != "run-new" || got.Phase != string(state.PhaseRunning) || got.ContainerStatus != "running" {
		t.Errorf("row: run=%q phase=%q container=%q; want run-new running", got.RunID, got.Phase, got.ContainerStatus)
	}
	if n := brokerReservationCount(t, s, broker.ID); n != 1 {
		t.Errorf("reservations = %d, want 1 (run-new keeps its slot)", n)
	}
	for _, p := range ep.publishedAgents() {
		if p.ID == a.ID && (p.Phase == string(state.PhaseStopped) || p.Phase == string(state.PhaseSuspended)) {
			t.Errorf("published %s for a run-changed stop", p.Phase)
		}
	}
}

func TestStopMissLeavesNewerRun_NoWriteNoQuotaNoPublish(t *testing.T) {
	for _, action := range []string{"stop", "suspend"} {
		t.Run(action, func(t *testing.T) {
			srv, s, broker, a, ep := runSwapQuotaAgent(t, "miss-"+action)
			jti := "jti-" + a.ID
			insertTestAgentCredential(t, s, a.ID, a.ProjectID, jti)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/"+action, nil)
			if rec.Code >= 300 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			assertNewerRunUntouched(t, s, broker, a, ep)
			// The response is the current row (run-new's, container
			// running), not the pre-stop copy, which has no container
			// status. The run ID itself is not serialised.
			var resp struct {
				Agent           *store.Agent `json:"agent"`
				ContainerStatus string       `json:"containerStatus"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			gotStatus := resp.ContainerStatus
			if resp.Agent != nil {
				gotStatus = resp.Agent.ContainerStatus
			}
			if gotStatus != "running" {
				t.Errorf("%s response containerStatus = %q, want run-new's running: %s", action, gotStatus, rec.Body.String())
			}
			// A newer run keeps its credentials.
			if cred := getTestAgentCredential(t, s, jti); cred.RevokedAt != nil {
				t.Errorf("%s revoked the newer run's credentials", action)
			}
		})
	}
	t.Run("stop-all", func(t *testing.T) {
		srv, s, broker, a, ep := runSwapQuotaAgent(t, "miss-stopall")
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/stop-all", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp StopAllAgentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range resp.Results {
			if r.ID == a.ID {
				found = true
				if r.Status != stopAllStatusRunChanged {
					t.Errorf("stop-all result status = %q, want %q", r.Status, stopAllStatusRunChanged)
				}
			}
		}
		if !found {
			t.Errorf("no stop-all result for the agent: %+v", resp)
		}
		assertNewerRunUntouched(t, s, broker, a, ep)
	})
	t.Run("auto-suspend", func(t *testing.T) {
		srv, s, broker, a, ep := runSwapQuotaAgent(t, "miss-autosusp")
		srv.autoSuspendStalledAgents(context.Background(), []store.Agent{*a})
		assertNewerRunUntouched(t, s, broker, a, ep)
	})
	t.Run("queued stop", func(t *testing.T) {
		srv, s, broker, a, ep := runSwapQuotaAgent(t, "miss-queued")
		ctx := context.Background()
		intentAt, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
		if err != nil {
			t.Fatal(err)
		}
		args, _ := MarshalDispatchArgs(StopDispatchArgs{IntentAt: &intentAt, RunID: "run-old"})
		if _, err := srv.execDispatchStop(ctx, store.BrokerDispatch{AgentID: a.ID, Op: "stop", Args: args}); err != nil {
			t.Fatal(err)
		}
		assertNewerRunUntouched(t, s, broker, a, ep)
	})
}

// runLandingStartClient fails the start leg after another caller's run has
// landed on the row.
type runLandingStartClient struct {
	*mockRuntimeBrokerClient
	s       store.Store
	agentID string
}

func (c runLandingStartClient) StartAgent(ctx context.Context, _, _, _, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, _ StartExtras) (*RemoteAgentResponse, error) {
	c.startCalled = true
	if _, err := c.s.SetAgentRunID(ctx, c.agentID, "run-other"); err != nil {
		return nil, err
	}
	return nil, errors.New("connection reset by peer")
}

// restartQuotaAgent is a running agent on run-x holding one broker
// reservation.
func restartQuotaAgent(t *testing.T, name string) (*Server, store.Store, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)
	setBrokerAgentCeiling(t, s, 5)
	agent := setupBrokerAgentInPhase(t, s, name, state.PhaseRunning)
	broker, err := s.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		t.Fatal(err)
	}
	reserveBrokerSlot(t, s, broker, agent.ID)
	if _, err := s.SetAgentRunID(ctx, agent.ID, "run-x"); err != nil {
		t.Fatal(err)
	}
	return srv, s, agent
}

// A restart whose stop leg succeeds and whose start leg fails while
// keeping the run it minted (the broker attempted the start, or a transport
// error) records the agent stopped under that run and releases its
// reservation.
func TestRestartStartLegFailureKeepingMintedRunRecordsStopped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startErr func(t *testing.T) error
	}{
		{"start attempted", func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttempted(""))
		}},
		{"transport error", func(*testing.T) error { return errors.New("connection reset by peer") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, agent := restartQuotaAgent(t, "restart-minted-"+tc.name)
			mockClient := &mockRuntimeBrokerClient{}
			d := NewHTTPAgentDispatcherWithClient(s, startErrClient{mockClient, tc.startErr(t)}, false, slog.Default())
			d.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
			srv.SetDispatcher(d)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
			if rec.Code < 400 {
				t.Fatalf("restart: status %d, want a failure: %s", rec.Code, rec.Body.String())
			}
			if !mockClient.stopCalled {
				t.Fatal("the stop leg was not dispatched")
			}
			got := mustGetAgent(t, s, agent.ID)
			if got.Phase != string(state.PhaseStopped) || got.ContainerStatus != "stopped" {
				t.Errorf("phase=%q container=%q, want stopped", got.Phase, got.ContainerStatus)
			}
			if got.RunID == "" || got.RunID == "run-x" {
				t.Errorf("run = %q, want the run the failed start leg minted", got.RunID)
			}
			if n := brokerReservationCount(t, s, agent.RuntimeBrokerID); n != 0 {
				t.Errorf("reservations = %d, want 0 (released)", n)
			}
		})
	}
}

// Variant: another caller's run lands on the row during the failed
// start leg. The restart records nothing for it and keeps the reservation.
func TestRestartStartLegFailureWithOtherRunLandedRecordsNothing(t *testing.T) {
	srv, s, agent := restartQuotaAgent(t, "restart-other-run")
	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, runLandingStartClient{mockClient, s, agent.ID}, false, slog.Default())
	d.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	srv.SetDispatcher(d)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
	if rec.Code < 400 {
		t.Fatalf("restart: status %d, want a failure: %s", rec.Code, rec.Body.String())
	}
	got := mustGetAgent(t, s, agent.ID)
	if got.RunID != "run-other" {
		t.Fatalf("run = %q, want run-other", got.RunID)
	}
	if got.Phase == string(state.PhaseStopped) || got.ContainerStatus == "stopped" {
		t.Errorf("phase=%q container=%q: the other run was recorded stopped", got.Phase, got.ContainerStatus)
	}
	if n := brokerReservationCount(t, s, agent.RuntimeBrokerID); n != 1 {
		t.Errorf("reservations = %d, want 1 (held for the other run)", n)
	}
}

// failSuspendWriteStore fails the suspended status write with a plain error,
// optionally after moving the row to run-new.
type failSuspendWriteStore struct {
	store.Store
	swapRun bool
}

func (f *failSuspendWriteStore) UpdateAgentStatus(ctx context.Context, id string, upd store.AgentStatusUpdate) error {
	if upd.Phase == string(state.PhaseSuspended) {
		if f.swapRun {
			if _, err := f.SetAgentRunID(ctx, id, "run-new"); err != nil {
				return err
			}
		}
		return errors.New("disk full")
	}
	return f.Store.UpdateAgentStatus(ctx, id, upd)
}

// When the suspended status write fails, the stopped container's
// credentials are still revoked while the row holds the stopped run, and
// left alone when the row moved to a newer run.
func TestSuspendWriteFailureRevokesOnlyForTheStoppedRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		swapRun     bool
		wantRevoked bool
	}{
		{"stopped run still current", false, true},
		{"row moved to a newer run", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, a, _ := runSwapQuotaAgent(t, "suspend-fail-"+map[bool]string{true: "swap", false: "same"}[tc.swapRun])
			srv.SetDispatcher(&quotaLifecycleDispatcher{})
			insertTestAgentCredential(t, s, a.ID, a.ProjectID, "jti-"+a.ID)
			srv.store = &failSuspendWriteStore{Store: s, swapRun: tc.swapRun}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
			if rec.Code < 400 {
				t.Fatalf("suspend: status %d, want a failure: %s", rec.Code, rec.Body.String())
			}
			cred := getTestAgentCredential(t, s, "jti-"+a.ID)
			if revoked := cred.RevokedAt != nil; revoked != tc.wantRevoked {
				t.Errorf("credential revoked = %v, want %v", revoked, tc.wantRevoked)
			}
		})
	}
}
