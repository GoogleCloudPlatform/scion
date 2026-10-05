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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
)

// Tests for run-scoped stop on the hub side (ptone/scion#2550 P3): the
// dispatcher sends the row's run ID on both transports, a broker 404 on a
// run-scoped stop comes back as ErrStopRunNotFound, and no caller records
// the current run as stopped because of it.

func TestRunID_DispatchAgentStopSendsRunID(t *testing.T) {
	ctx := context.Background()
	f := newRunIDFixture(t, "runid-stop")

	f.agent.RunID = "run-a"
	if err := f.dispatcher.DispatchAgentStop(ctx, f.agent); err != nil {
		t.Fatalf("DispatchAgentStop: %v", err)
	}
	if got := f.client.lastStopRunID; got != "run-a" {
		t.Errorf("stop runID = %q, want run-a", got)
	}

	f.agent.RunID = ""
	if err := f.dispatcher.DispatchAgentStop(ctx, f.agent); err != nil {
		t.Fatalf("DispatchAgentStop: %v", err)
	}
	if got := f.client.lastStopRunID; got != "" {
		t.Errorf("stop runID = %q, want none for a row without a run ID", got)
	}
}

func TestStopAgentQuery_RunID(t *testing.T) {
	q, err := url.ParseQuery(stopAgentQuery(context.Background(), "p1", "run a&b"))
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("runId") != "run a&b" || q.Get("projectId") != "p1" {
		t.Errorf("unexpected query %v", q)
	}
	q, _ = url.ParseQuery(stopAgentQuery(context.Background(), "p1", ""))
	if q.Has("runId") {
		t.Errorf("runId sent without a run ID: %v", q)
	}
	if got := stopAgentQuery(context.Background(), "", ""); got != "" {
		t.Errorf("empty stop query = %q, want empty", got)
	}
	q, _ = url.ParseQuery(stopAgentQuery(withRecordedRuntime(context.Background(), "kubernetes"), "p1", "run-1"))
	if q.Get("runId") != "run-1" || q.Get("projectId") != "p1" || len(q) != 3 {
		t.Errorf("recorded runtime query %v, want projectId, runId and the recorded runtime", q)
	}
}

// runMismatchBody is the broker's run-mismatch 404 body for a stop naming
// run-1 while run-2 holds the name (runtimebroker.StopRunMismatch).
const runMismatchBody = `{"error":{"code":"` + api.BrokerErrorCodeRunMismatch + `","message":"Agent not found for the requested run","details":{"runId":"run-1","currentRunId":"run-2"}}}`

func TestHTTPRuntimeBrokerClient_StopAgentRunID(t *testing.T) {
	var gotQuery url.Values
	status := http.StatusAccepted
	body := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client := NewHTTPRuntimeBrokerClient()

	if err := client.StopAgent(context.Background(), tid("host-1"), server.URL, "a", "p1", "run-1"); err != nil {
		t.Fatal(err)
	}
	if got := gotQuery.Get("runId"); got != "run-1" {
		t.Errorf("runId = %q, want run-1", got)
	}

	status = http.StatusNotFound
	body = runMismatchBody
	err := client.StopAgent(context.Background(), tid("host-1"), server.URL, "a", "p1", "run-1")
	if !errors.Is(err, ErrStopRunNotFound) || !isBrokerStatus(err, http.StatusNotFound) {
		t.Errorf("run-scoped 404: err = %v, want ErrStopRunNotFound wrapping the 404", err)
	}
	if current, ok := brokerStopCurrentRunID(err); !ok || current != "run-2" {
		t.Errorf("broker current run = (%q, %v), want (run-2, true)", current, ok)
	}

	// A 404 without the run-mismatch code (a proxy, an unknown route) is
	// not read as a run mismatch.
	body = `{"error":{"code":"agent_not_found","message":"Agent not found"}}`
	err = client.StopAgent(context.Background(), tid("host-1"), server.URL, "a", "p1", "run-1")
	if err == nil || errors.Is(err, ErrStopRunNotFound) {
		t.Errorf("404 without the run-mismatch code: err = %v, want a plain broker error", err)
	}

	body = runMismatchBody
	err = client.StopAgent(context.Background(), tid("host-1"), server.URL, "a", "p1", "")
	if err == nil || errors.Is(err, ErrStopRunNotFound) {
		t.Errorf("legacy 404: err = %v, want a plain broker error", err)
	}
	if gotQuery.Has("runId") {
		t.Errorf("runId sent without a run ID: %v", gotQuery)
	}
}

func TestControlChannelBrokerClient_StopAgentRunID(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true, status: http.StatusAccepted}
	client := &ControlChannelBrokerClient{manager: tunnel}
	if err := client.StopAgent(context.Background(), "broker-1", "unused", "agent-1", "proj-1", "run-1"); err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(tunnel.lastRequest.Query)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("runId") != "run-1" || q.Get("projectId") != "proj-1" {
		t.Errorf("query = %v, want runId run-1 and projectId proj-1", q)
	}

	tunnel.status = http.StatusNotFound
	tunnel.body = []byte(runMismatchBody)
	err = client.StopAgent(context.Background(), "broker-1", "unused", "agent-1", "proj-1", "run-1")
	if !errors.Is(err, ErrStopRunNotFound) || !isBrokerStatus(err, http.StatusNotFound) {
		t.Errorf("run-scoped 404: err = %v, want ErrStopRunNotFound wrapping the 404", err)
	}
	if current, ok := brokerStopCurrentRunID(err); !ok || current != "run-2" {
		t.Errorf("broker current run = (%q, %v), want (run-2, true)", current, ok)
	}
	tunnel.body = nil
	err = client.StopAgent(context.Background(), "broker-1", "unused", "agent-1", "proj-1", "run-1")
	if err == nil || errors.Is(err, ErrStopRunNotFound) {
		t.Errorf("404 without the run-mismatch code: err = %v, want a plain broker error", err)
	}
	tunnel.body = []byte(runMismatchBody)
	err = client.StopAgent(context.Background(), "broker-1", "unused", "agent-1", "proj-1", "")
	if err == nil || errors.Is(err, ErrStopRunNotFound) {
		t.Errorf("legacy 404: err = %v, want a plain broker error", err)
	}
}

// A queued stop pins the run it was dispatched for: the owning node sends
// that run, not the row's current one.
func TestExecDispatchStop_UsesIntentRunID(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	_, _, agent := setupOnlineBrokerAgent(t, s, "stop-intent-run")
	if _, err := s.SetAgentRunID(ctx, agent.ID, "run-new"); err != nil {
		t.Fatal(err)
	}

	args, err := MarshalDispatchArgs(StopDispatchArgs{RunID: "run-old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.execDispatchStop(ctx, store.BrokerDispatch{AgentID: agent.ID, Op: "stop", Args: args}); err != nil {
		t.Fatalf("execDispatchStop: %v", err)
	}
	if client.lastStopRunID != "run-old" {
		t.Errorf("stop runID = %q, want the intent's run-old", client.lastStopRunID)
	}

	// A row queued without a run ID stops with the row's run, as before.
	if _, err := srv.execDispatchStop(ctx, store.BrokerDispatch{AgentID: agent.ID, Op: "stop"}); err != nil {
		t.Fatalf("execDispatchStop: %v", err)
	}
	if client.lastStopRunID != "run-new" {
		t.Errorf("stop runID = %q, want the row's run-new", client.lastStopRunID)
	}
}

func TestStopDispatchArgs_RunIDRoundTrip(t *testing.T) {
	raw, err := MarshalDispatchArgs(StopDispatchArgs{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalStopArgs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", got.RunID)
	}
}

// runLabelManager.StopTarget is the broker's stop of a resolved entry on
// the fake labelled runtime: it records the stop and marks the entry
// stopped.
func (m *runLabelManager) StopTarget(_ context.Context, ref runtime.RunRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops = append(m.stops, ref)
	for i := range m.entries {
		if m.entries[i].ContainerID == ref.ID {
			m.entries[i].Phase = "stopped"
		}
	}
	return nil
}

func (m *runLabelManager) stopSnapshot() ([]api.AgentInfo, []runtime.RunRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]api.AgentInfo(nil), m.entries...), append([]runtime.RunRef(nil), m.stops...)
}

// stopE2EBroker starts a real runtimebroker.Server over HTTP on the fake
// labelled runtime.
func stopE2EBroker(t *testing.T) (*runLabelManager, string) {
	t.Helper()
	mgr := &runLabelManager{}
	cfg := runtimebroker.DefaultServerConfig()
	cfg.BrokerID = "e2e-broker"
	cfg.BrokerName = "e2e-broker"
	brokerSrv := runtimebroker.New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	httpSrv := httptest.NewServer(brokerSrv.Handler())
	t.Cleanup(httpSrv.Close)
	return mgr, httpSrv.URL
}

// Plan acceptance (a) for stop on Docker, in process: the hub dispatcher
// talks over HTTP to a real runtimebroker.Server. A stop carrying run A's
// ID after run B started under the same name gets 404, and B keeps
// running; the stop for run B stops it.
func TestRunID_E2E_StaleStopSparesRecreatedAgent(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	const slug, name = "e2e-stop-proj", "dev"
	projectID := tid("project-e2e-stop")
	mgr, endpoint := stopE2EBroker(t)

	s := createTestStore(t)
	if err := s.CreateProject(ctx, &store.Project{ID: projectID, Name: slug, Slug: slug}); err != nil {
		t.Fatal(err)
	}
	brokerID := tid("broker-e2e-stop")
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "e2e-broker", Slug: "e2e-broker", Endpoint: endpoint, Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	client := &e2eBrokerClient{RuntimeBrokerClient: NewHTTPRuntimeBrokerClient(), mgr: mgr}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	newAgent := func(id string) *store.Agent {
		a := &store.Agent{
			ID: tid(id), Name: name, Slug: name, ProjectID: projectID, RuntimeBrokerID: brokerID,
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
		}
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
		return a
	}

	// Run A, whose container is then removed (deleted or replaced).
	agentA := newAgent("agent-e2e-stop-a")
	if _, err := d.DispatchAgentCreate(ctx, agentA); err != nil {
		t.Fatalf("create A: %v", err)
	}
	runA := agentA.RunID
	requireUUID(t, "run A", runA)
	mgr.mu.Lock()
	mgr.entries = nil
	mgr.mu.Unlock()
	if err := s.DeleteAgent(ctx, agentA.ID); err != nil {
		t.Fatal(err)
	}

	// Run B under the same name.
	agentB := newAgent("agent-e2e-stop-b")
	if _, err := d.DispatchAgentCreate(ctx, agentB); err != nil {
		t.Fatalf("create B: %v", err)
	}
	runB := agentB.RunID
	if runB == runA {
		t.Fatal("recreate reused run A's ID")
	}

	// A late stop for run A must leave B running.
	err := d.DispatchAgentStop(ctx, agentA)
	if !errors.Is(err, ErrStopRunNotFound) {
		t.Fatalf("stale stop for run A: err = %v, want ErrStopRunNotFound", err)
	}
	if current, ok := brokerStopCurrentRunID(err); !ok || current != runB {
		t.Errorf("broker current run = (%q, %v), want (%s, true)", current, ok, runB)
	}
	entries, stops := mgr.stopSnapshot()
	if len(stops) != 0 {
		t.Fatalf("stale stop reached the runtime: %v", stops)
	}
	if len(entries) != 1 || entries[0].RunID != runB || entries[0].Phase != "running" {
		t.Fatalf("B's entry was touched: %v", entries)
	}

	// The stop for run B stops it, naming its run.
	if err := d.DispatchAgentStop(ctx, agentB); err != nil {
		t.Fatalf("stop B: %v", err)
	}
	entries, stops = mgr.stopSnapshot()
	if len(stops) != 1 || stops[0].RunID != runB || entries[0].Phase != "stopped" {
		t.Fatalf("stop B: entries=%v stops=%v, want B stopped by run %s", entries, stops, runB)
	}
}

// Through the hub's own stop and suspend handlers, to the real broker: when
// the row names a run that no longer holds the name, the broker's 404 fails
// the action and the row is not recorded stopped or suspended, while the
// running entry is untouched. With the row naming the live run, the same
// stop succeeds and records stopped.
func TestRunID_E2E_StaleStopDoesNotMarkRowStopped(t *testing.T) {
	for _, action := range []string{"stop", "suspend"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("HOME", t.TempDir())
			t.Chdir(t.TempDir())
			mgr, endpoint := stopE2EBroker(t)

			srv, s := testServer(t)
			_, broker, agent := setupOnlineBrokerAgent(t, s, "e2e-stale-"+action)
			broker.Endpoint = endpoint
			if err := s.UpdateRuntimeBroker(ctx, broker); err != nil {
				t.Fatal(err)
			}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, NewHTTPRuntimeBrokerClient(), false, slog.Default()))
			mgr.run(agent.Slug, "cid-b", agent.ProjectID, "", "run-b")
			if _, err := s.SetAgentRunID(ctx, agent.ID, "run-a"); err != nil {
				t.Fatal(err)
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			if rec.Code < 400 {
				t.Fatalf("stale %s: status %d, want a failure: %s", action, rec.Code, rec.Body.String())
			}
			got, err := s.GetAgent(ctx, agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Phase != string(state.PhaseRunning) || got.ContainerStatus == "stopped" {
				t.Errorf("stale %s recorded the row as phase=%q container=%q, want running", action, got.Phase, got.ContainerStatus)
			}
			entries, stops := mgr.stopSnapshot()
			if len(stops) != 0 || entries[0].Phase != "running" {
				t.Fatalf("stale %s touched the live run: entries=%v stops=%v", action, entries, stops)
			}

			if action != "stop" {
				return
			}
			if _, err := s.SetAgentRunID(ctx, agent.ID, "run-b"); err != nil {
				t.Fatal(err)
			}
			rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("current stop: status %d: %s", rec.Code, rec.Body.String())
			}
			got, err = s.GetAgent(ctx, agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Phase != string(state.PhaseStopped) {
				t.Errorf("current stop: phase = %q, want stopped", got.Phase)
			}
			if _, stops = mgr.stopSnapshot(); len(stops) != 1 || stops[0].RunID != "run-b" {
				t.Errorf("current stop: stops = %v, want one stop of run-b", stops)
			}
		})
	}
}

// B1 (review round 1): the producers of queued stop intents write the run.
// An offline-broker stop queues a dispatch row whose args carry the row's
// run ID.
func TestQueueOfflineStop_IntentCarriesRunID(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetDispatcher(&runIntentDispatcher{})
	srv.commandBus = &recordingCommandBus{}
	_, broker, agent := setupOfflineBrokerAgent(t, s, "stop-q-run")
	if _, err := s.SetAgentRunID(ctx, agent.ID, "run-q"); err != nil {
		t.Fatal(err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("offline stop: status %d: %s", rec.Code, rec.Body.String())
	}
	assertStopIntentRunID(t, s, broker.ID, "run-q")
}

// A cross-node stop (DispatchAgentStop → ErrLifecycleDeferred) writes a
// stop dispatch row whose args carry the row's run ID.
func TestDeferredStop_IntentCarriesRunID(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	remoteBroker := uuid.NewString()
	events := NewChannelEventPublisher()
	defer events.Close()
	dispatcher := NewHTTPAgentDispatcherWithClient(cs, &deferredTestClient{localBroker: "local-broker"}, false, slog.Default())
	dispatcher.SetCrossNodeDeps(events, NoopCommandBus{})

	agent := seedAgentWithBrokerID(t, cs, remoteBroker)
	if _, err := cs.SetAgentRunID(ctx, agent.ID, "run-d"); err != nil {
		t.Fatal(err)
	}
	agent.RunID = "run-d"

	// Publish the terminal event only once the intent row is visible: the
	// dispatcher subscribes before it writes the row, so the event cannot
	// arrive before the subscription.
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if pending, err := cs.ListPendingDispatch(ctx, remoteBroker); err == nil && len(pending) > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		stopped := *agent
		stopped.Phase = "stopped"
		events.PublishAgentStatus(ctx, &stopped)
	}()
	if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
		t.Fatalf("deferred stop: %v", err)
	}
	assertStopIntentRunID(t, cs, remoteBroker, "run-d")
}

func assertStopIntentRunID(t *testing.T, s store.Store, brokerID, want string) {
	t.Helper()
	pending, err := s.ListPendingDispatch(context.Background(), brokerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Op != "stop" {
		t.Fatalf("pending dispatch rows = %+v, want one stop", pending)
	}
	args, err := UnmarshalStopArgs(pending[0].Args)
	if err != nil {
		t.Fatal(err)
	}
	if args.RunID != want {
		t.Errorf("stop intent runId = %q, want %q", args.RunID, want)
	}
}
