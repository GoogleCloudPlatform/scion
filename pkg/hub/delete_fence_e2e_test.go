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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// End-to-end check for ptone/scion#2906, in process: the hub's delete
// engine (real claim, notAfter and dispatcher) talks over HTTP to a real
// runtimebroker.Server, through a client that holds the delete "in the
// network" until the test releases it. Both sides use fake clocks; nothing
// sleeps.

// holdingBrokerClient forwards to the real HTTP client, but holds each
// DeleteAgent until release is closed, as a slow network or a frozen hub
// would. StartAgent stands in for the broker's start handler, which the
// fake runtime cannot run: when the agent's entry still exists it keeps it
// and reports its run (the broker's "already running" answer, which the hub
// adopts); otherwise it runs a new entry labelled with the run ID the hub
// minted, as pkg/agent.Start does.
type holdingBrokerClient struct {
	RuntimeBrokerClient
	held     chan DeleteAgentOptions
	release  chan struct{}
	mgr      *runLabelManager
	project  string
	scionDir string
	starts   int
}

func (c *holdingBrokerClient) StartAgent(_ context.Context, _, _, agentID, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, extras StartExtras) (*RemoteAgentResponse, error) {
	c.starts++
	entries, _ := c.mgr.snapshot()
	for _, e := range entries {
		if e.Name == agentID {
			return &RemoteAgentResponse{Agent: &RemoteAgentInfo{Slug: agentID, ContainerID: e.ContainerID, Phase: "running", RunID: e.RunID}}, nil
		}
	}
	cid := "cid-new"
	c.mgr.run(agentID, cid, c.project, c.scionDir, extras.RunID)
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{Slug: agentID, ContainerID: cid, Phase: "running", RunID: extras.RunID}, Created: true}, nil
}

func (c *holdingBrokerClient) DeleteAgent(ctx context.Context, brokerID, endpoint, agentID, projectID string, opts DeleteAgentOptions) error {
	c.held <- opts
	<-c.release
	return c.RuntimeBrokerClient.DeleteAgent(ctx, brokerID, endpoint, agentID, projectID, opts)
}

type fenceE2E struct {
	srv       *Server
	s         store.Store
	mgr       *runLabelManager
	brokerNow *atomic.Pointer[time.Time]
	client    *holdingBrokerClient
	agent     *store.Agent
	infoPath  string
	projectID string
	scionDir  string
	t0        time.Time
}

func newFenceE2E(t *testing.T) *fenceE2E {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	t0 := fenceNow(t)

	const slug, name = "fence-e2e", "dev"
	projectID := tid("project-fence-e2e")
	scionDir := filepath.Join(home, ".scion", "projects", slug, ".scion")
	agentHome := config.GetAgentHomePath(scionDir, name)
	require.NoError(t, os.MkdirAll(agentHome, 0o755))
	require.NoError(t, config.WriteProjectID(scionDir, projectID))
	infoPath := filepath.Join(agentHome, "agent-info.json")
	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"dev","phase":"running"}`), 0o644))

	mgr := &runLabelManager{}
	cfg := runtimebroker.DefaultServerConfig()
	cfg.BrokerID = "fence-broker"
	cfg.BrokerName = "fence-broker"
	brokerNow := &atomic.Pointer[time.Time]{}
	brokerNow.Store(&t0)
	cfg.DeleteClock = func() time.Time { return *brokerNow.Load() }
	broker := runtimebroker.New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	httpSrv := httptest.NewServer(broker.Handler())
	t.Cleanup(httpSrv.Close)

	srv, s := testServer(t)
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: slug, Slug: slug}))
	brokerID := tid("broker-fence-e2e")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "fence-broker", Slug: "fence-broker", Endpoint: httpSrv.URL, Status: store.BrokerStatusOnline,
	}))
	client := &holdingBrokerClient{
		RuntimeBrokerClient: NewHTTPRuntimeBrokerClient(),
		held:                make(chan DeleteAgentOptions, 1),
		release:             make(chan struct{}),
		mgr:                 mgr,
		project:             projectID,
		scionDir:            scionDir,
	}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

	a := &store.Agent{
		ID: tid("agent-fence-e2e"), Name: name, Slug: name, ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: string(state.PhaseRunning), AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, a))
	_, err := s.SetAgentRunID(ctx, a.ID, "run-a")
	require.NoError(t, err)
	mgr.run(name, "cid-a", projectID, scionDir, "run-a")

	return &fenceE2E{srv: srv, s: s, mgr: mgr, brokerNow: brokerNow, client: client, agent: a,
		infoPath: infoPath, projectID: projectID, scionDir: scionDir, t0: t0}
}

// startDelete claims and starts the engine, and waits until its dispatch
// is held in the "network".
func (f *fenceE2E) startDelete(t *testing.T) (<-chan deletionOutcome, *agentDeletionPlan, DeleteAgentOptions) {
	t.Helper()
	ctx := context.Background()
	plan, err := f.srv.claimAgentDeletion(ctx, f.agent.ID, agentDeleteParams{deleteFiles: true})
	require.NoError(t, err)
	require.NotNil(t, plan)
	done := f.srv.runAgentDeletion(ctx, plan)
	select {
	case opts := <-f.client.held:
		return done, plan, opts
	case <-time.After(10 * time.Second):
		t.Fatal("the engine never dispatched")
	}
	return nil, nil, DeleteAgentOptions{}
}

// lapse makes the claim's lease lapse, as when the engine froze: the row
// reads failed/abandoned and a start is allowed again.
func (f *fenceE2E) lapse(t *testing.T, plan *agentDeletionPlan) {
	t.Helper()
	past := time.Now().Add(-time.Minute)
	claim := plan.claim
	n, err := f.s.UpdateAgentDeletion(context.Background(), f.agent.ID,
		store.DeletionPredicate{Claim: &claim}, store.DeletionFields{LeaseAt: &past})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got := mustGetAgent(t, f.s, f.agent.ID)
	view := store.ComputeAgentDeletion(got, time.Now())
	require.NotNil(t, view)
	require.Equal(t, store.DeletionCodeAbandoned, view.Code)
}

// setBrokerNow sets the broker's delete clock.
func (f *fenceE2E) setBrokerNow(t time.Time) { f.brokerNow.Store(&t) }

// start drives a real start through the hub's start handler (start gate,
// clearing the abandoned delete marker, run ID mint and adoption).
func (f *fenceE2E) start(t *testing.T) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/start", nil)
	require.Less(t, rec.Code, 300, "start: %d %s", rec.Code, rec.Body.String())
	require.Equal(t, 1, f.client.starts, "the start did not reach the broker")
	got := mustGetAgent(t, f.s, f.agent.ID)
	require.Equal(t, store.DeletionStateNone, got.DeletionState, "the start did not clear the abandoned delete")
}

func (f *fenceE2E) wait(t *testing.T, done <-chan deletionOutcome) deletionOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(10 * time.Second):
		t.Fatal("the engine never finished")
	}
	return deletionOutcome{}
}

func (f *fenceE2E) requireSurvived(t *testing.T, out deletionOutcome, wantRun string) {
	t.Helper()
	assert.Equal(t, deletionOutcomeFailed, out.kind, "outcome %+v", out)
	assert.Equal(t, store.DeletionCodeAbandoned, out.code)
	entries, deletes := f.mgr.snapshot()
	assert.Empty(t, deletes, "the late delete reached DeleteTarget")
	require.Len(t, entries, 1)
	assert.Equal(t, wantRun, entries[0].RunID)
	data, err := os.ReadFile(f.infoPath)
	require.NoError(t, err, "the agent's files were removed")
	assert.False(t, strings.Contains(string(data), "deleted"), "soft-delete marked: %s", data)
	got, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err, "the hub row was removed")
	assert.True(t, got.DeletedAt.IsZero(), "the hub row was soft-deleted")
}

func TestDeleteFence_E2E_LateDeleteAfterAbandonment(t *testing.T) {
	// The delete sent under claim N is delivered long after it was sent;
	// by then the claim lapsed. Each case is a step the user may have
	// taken in between.
	for _, tc := range []struct {
		name    string
		between func(t *testing.T, f *fenceE2E)
		wantRun string
	}{
		{
			// Same run, no restart: the hub abandoned the delete and the
			// user kept using run A.
			name:    "abandoned, nothing since",
			between: func(*testing.T, *fenceE2E) {},
			wantRun: "run-a",
		},
		{
			// ii2 step 9b: run A survived the freeze, the user's start
			// adopted it (the hub records the broker's run-a again), then
			// DELETE(run A) arrived. run_id cannot fence this.
			name: "start adopted the surviving run",
			between: func(t *testing.T, f *fenceE2E) {
				f.start(t)
				require.Equal(t, "run-a", mustGetAgent(t, f.s, f.agent.ID).RunID, "the start did not adopt run-a")
			},
			wantRun: "run-a",
		},
		{
			// Run N's delete delivered after the run N+1 start: run A's
			// container went away meanwhile and the start created a new
			// run. The broker's run_id check would also spare it (404);
			// the deadline refuses the delete first.
			name: "started as a new run",
			between: func(t *testing.T, f *fenceE2E) {
				f.mgr.mu.Lock()
				f.mgr.entries = nil
				f.mgr.mu.Unlock()
				f.start(t)
			},
			wantRun: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceE2E(t)
			done, plan, opts := f.startDelete(t)
			require.Equal(t, "run-a", opts.RunID)
			require.False(t, opts.NotAfter.IsZero(), "the engine sent no notAfter")

			f.lapse(t, plan)
			tc.between(t, f)
			// The delete reaches the broker 75s after it was sent. notAfter
			// is t0+55s (the 60s lease less the 5s margin), so the broker's
			// last accepted instant is notAfter + 5s = t0+60s, the lease
			// expiry.
			f.setBrokerNow(f.t0.Add(75 * time.Second))
			close(f.client.release)

			wantRun := tc.wantRun
			if wantRun == "" {
				wantRun = mustGetAgent(t, f.s, f.agent.ID).RunID
				require.NotEqual(t, "run-a", wantRun)
			}
			out := f.wait(t, done)
			assert.Contains(t, out.message, "after its deadline")
			f.requireSurvived(t, out, wantRun)
		})
	}
}

// Control: the same delivery within the deadline deletes the agent, so the
// refusal above is the deadline and nothing else.
func TestDeleteFence_E2E_InTimeDeleteProceeds(t *testing.T) {
	f := newFenceE2E(t)
	done, _, _ := f.startDelete(t)
	f.setBrokerNow(f.t0.Add(10 * time.Second))
	close(f.client.release)

	out := f.wait(t, done)
	assert.Equal(t, deletionOutcomeDeleted, out.kind, "outcome %+v", out)
	entries, deletes := f.mgr.snapshot()
	assert.Empty(t, entries)
	require.Len(t, deletes, 1)
	assert.Equal(t, "run-a", deletes[0].RunID)
}

// The margin's boundary: notAfter is t0+55s and the broker accepts until
// t0+60s, the lease expiry. A delete reaching the broker at t0+62s, after
// the lease expired, is refused; without the hub's margin (notAfter =
// t0+60s, accepted until t0+65s) it would have been accepted. One reaching
// it at t0+58s, while the lease is still live, is accepted.
func TestDeleteFence_E2E_MarginBoundary(t *testing.T) {
	t.Run("after the lease expiry", func(t *testing.T) {
		f := newFenceE2E(t)
		done, plan, opts := f.startDelete(t)
		require.True(t, opts.NotAfter.Equal(f.t0.Add(55*time.Second)), "notAfter = %v, want t0+55s", opts.NotAfter)
		f.lapse(t, plan)
		f.setBrokerNow(f.t0.Add(62 * time.Second))
		close(f.client.release)
		f.requireSurvived(t, f.wait(t, done), "run-a")
	})
	t.Run("before the lease expiry", func(t *testing.T) {
		f := newFenceE2E(t)
		done, _, _ := f.startDelete(t)
		f.setBrokerNow(f.t0.Add(58 * time.Second))
		close(f.client.release)
		out := f.wait(t, done)
		assert.Equal(t, deletionOutcomeDeleted, out.kind, "outcome %+v", out)
		entries, _ := f.mgr.snapshot()
		assert.Empty(t, entries)
	})
}
