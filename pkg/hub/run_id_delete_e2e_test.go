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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// End-to-end check for ptone/scion#2550 P1, in process: the hub dispatcher
// (real run ID mint, persist and delete query) talks over HTTP to a real
// runtimebroker.Server (real deleteAgent and resolveDeleteTarget) backed by
// a fake runtime that honours the scion.run_id label.

// runLabelManager is a broker agent.Manager whose runtime entries carry
// labels, as Docker containers do. Only the methods the broker's delete
// path uses are implemented; the embedded nil Manager makes any other call
// panic, so the test notices if the delete path starts using more.
type runLabelManager struct {
	agent.Manager
	mu      sync.Mutex
	entries []api.AgentInfo
	deletes []runtime.RunRef
	stops   []runtime.RunRef
}

func (m *runLabelManager) List(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []api.AgentInfo
	for _, e := range m.entries {
		match := true
		for k, v := range filter {
			if e.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *runLabelManager) DeleteTarget(_ context.Context, _ string, ref runtime.RunRef, _ bool, _ string, _ bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, ref)
	kept := m.entries[:0]
	for _, e := range m.entries {
		if e.ContainerID != ref.ID {
			kept = append(kept, e)
		}
	}
	m.entries = kept
	return true, nil
}

// run adds a running entry labelled with runID, as pkg/agent.Start does
// with the run ID the broker passes in StartOptions.
func (m *runLabelManager) run(name, cid, projectID, projectPath, runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, api.AgentInfo{
		Name:        name,
		ContainerID: cid,
		ProjectID:   projectID,
		ProjectPath: projectPath,
		RunID:       runID,
		Phase:       "running",
		Labels: map[string]string{
			"scion.agent":      "true",
			"scion.name":       name,
			"scion.project_id": projectID,
			api.LabelRunID:     runID,
		},
	})
}

func (m *runLabelManager) snapshot() ([]api.AgentInfo, []runtime.RunRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]api.AgentInfo(nil), m.entries...), append([]runtime.RunRef(nil), m.deletes...)
}

// e2eBrokerClient sends deletes over HTTP to the real broker. Create stands
// in for the broker's create handler, which ends in pkg/agent.Start
// labelling the new container with req.RunID: it runs the entry on the
// fake runtime with the run ID the hub sent.
type e2eBrokerClient struct {
	RuntimeBrokerClient
	mgr         *runLabelManager
	projectPath string
	cids        int
}

func (c *e2eBrokerClient) CreateAgent(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	c.cids++
	cid := "cid-" + string(rune('0'+c.cids))
	c.mgr.run(req.Slug, cid, req.ProjectID, c.projectPath, req.RunID)
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{
		ID: req.ID, Slug: req.Slug, Name: req.Name, ContainerID: cid, Phase: "running", RunID: req.RunID,
	}, Created: true}, nil
}

func TestRunID_E2E_StaleDeleteSparesRecreatedAgent(t *testing.T) {
	ctx := context.Background()

	// Isolate HOME and CWD: the broker resolves the hub project's
	// directory under ~/.scion/projects.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	const slug, name = "e2e-proj", "dev"
	projectID := tid("project-e2e")
	scionDir := filepath.Join(home, ".scion", "projects", slug, ".scion")
	agentHome := config.GetAgentHomePath(scionDir, name)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(scionDir, projectID); err != nil {
		t.Fatal(err)
	}
	infoPath := filepath.Join(agentHome, "agent-info.json")
	if err := os.WriteFile(infoPath, []byte(`{"name":"dev","phase":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Real broker over HTTP.
	mgr := &runLabelManager{}
	cfg := runtimebroker.DefaultServerConfig()
	cfg.BrokerID = "e2e-broker"
	cfg.BrokerName = "e2e-broker"
	brokerSrv := runtimebroker.New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	httpSrv := httptest.NewServer(brokerSrv.Handler())
	defer httpSrv.Close()

	// Hub store and dispatcher.
	s := createTestStore(t)
	if err := s.CreateProject(ctx, &store.Project{ID: projectID, Name: slug, Slug: slug}); err != nil {
		t.Fatal(err)
	}
	brokerID := tid("broker-e2e")
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "e2e-broker", Slug: "e2e-broker", Endpoint: httpSrv.URL, Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	client := &e2eBrokerClient{RuntimeBrokerClient: NewHTTPRuntimeBrokerClient(), mgr: mgr, projectPath: scionDir}
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

	// Run A: create, then delete.
	agentA := newAgent("agent-e2e-a")
	if _, err := d.DispatchAgentCreate(ctx, agentA); err != nil {
		t.Fatalf("create A: %v", err)
	}
	runA := agentA.RunID
	requireUUID(t, "run A", runA)
	if err := d.DispatchAgentDelete(ctx, agentA, false, false, false, time.Time{}); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	entries, deletes := mgr.snapshot()
	if len(entries) != 0 || len(deletes) != 1 || deletes[0].RunID != runA {
		t.Fatalf("delete A: entries=%v deletes=%v, want A's entry deleted by run %s", entries, deletes, runA)
	}
	if err := s.DeleteAgent(ctx, agentA.ID); err != nil {
		t.Fatal(err)
	}

	// Run B: recreate the same name.
	agentB := newAgent("agent-e2e-b")
	if _, err := d.DispatchAgentCreate(ctx, agentB); err != nil {
		t.Fatalf("create B: %v", err)
	}
	runB := agentB.RunID
	requireUUID(t, "run B", runB)
	if runB == runA {
		t.Fatal("recreate reused run A's ID")
	}

	// A late delete for run A (e.g. a retried request still carrying A's
	// row) must leave B alone: 404 on the broker, which the hub client
	// treats as an idempotent success.
	if err := d.DispatchAgentDelete(ctx, agentA, true, true, true, time.Now()); err != nil {
		t.Fatalf("stale delete for run A: %v", err)
	}
	entries, deletes = mgr.snapshot()
	if len(deletes) != 1 {
		t.Fatalf("stale delete reached DeleteTarget: %v", deletes)
	}
	if len(entries) != 1 || entries[0].RunID != runB {
		t.Fatalf("B's entry was touched: %v", entries)
	}
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatalf("B's files were removed: %v", err)
	}
	if strings.Contains(string(data), "deleted") {
		t.Fatalf("B was soft-delete marked: %s", data)
	}

	// The delete for run B removes it.
	if err := d.DispatchAgentDelete(ctx, agentB, false, false, false, time.Time{}); err != nil {
		t.Fatalf("delete B: %v", err)
	}
	entries, deletes = mgr.snapshot()
	if len(entries) != 0 || len(deletes) != 2 || deletes[1].RunID != runB {
		t.Fatalf("delete B: entries=%v deletes=%v, want B's entry deleted by run %s", entries, deletes, runB)
	}
}

// The 1a-2 deletion engine (agent_delete_engine.go) hands the row it read
// after the claim to DispatchAgentDelete, so an HTTP DELETE reaches the
// broker client with the row's run ID. Engine → real dispatcher → client.
func TestRunID_DeleteEngineSendsRowRunID(t *testing.T) {
	srv, s := testServer(t)
	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	agent := setupBrokerAgentInPhase(t, s, "runid-engine", state.PhaseRunning)
	if _, err := s.SetAgentRunID(context.Background(), agent.ID, "run-current"); err != nil {
		t.Fatal(err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status %d: %s", rec.Code, rec.Body.String())
	}
	if !client.deleteCalled {
		t.Fatal("broker DeleteAgent was not called")
	}
	if got := client.lastDeleteOpts.runID; got != "run-current" {
		t.Errorf("broker delete runId = %q, want run-current", got)
	}
}

// claimFirstStore runs claim just before the first SetAgentRunID: a delete
// claim landing after the start gate passed but before the start's
// beginRun writes its run ID.
type claimFirstStore struct {
	store.Store
	once  sync.Once
	claim func()
}

func (c *claimFirstStore) SetAgentRunID(ctx context.Context, agentID, runID string) (string, error) {
	c.once.Do(c.claim)
	return c.Store.SetAgentRunID(ctx, agentID, runID)
}

// N3 (round 3): a delete that claims between the start gate and beginRun
// snapshots the old run ID. The start's run-ID write is then refused, so
// the start fails closed with 409 delete_in_progress before reaching the
// broker, and the delete's runId still names what exists. Without the
// refusal the start would replace run-0 with a new run, and the delete
// (carrying run-0) would 404 and leak it.
func TestRunID_DeleteClaimBeforeBeginRunFailsStartClosed(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			client := &mockRuntimeBrokerClient{}
			phase := state.PhaseStopped
			if op == "restart" {
				phase = state.PhaseRunning
			}
			agent := setupBrokerAgentInPhase(t, s, "runid-claim-"+op, phase)
			if _, err := s.SetAgentRunID(ctx, agent.ID, "run-0"); err != nil {
				t.Fatal(err)
			}
			var plan *agentDeletionPlan
			cs := &claimFirstStore{Store: s, claim: func() {
				var err error
				if plan, err = srv.claimAgentDeletion(ctx, agent.ID, agentDeleteParams{}); err != nil || plan == nil {
					t.Errorf("claim: plan %v, err %v", plan, err)
				}
			}}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(cs, client, false, slog.Default()))

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+op, nil)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrCodeDeleteInProgress) {
				t.Fatalf("%s: status %d, want 409 %s: %s", op, rec.Code, ErrCodeDeleteInProgress, rec.Body.String())
			}
			if plan == nil {
				t.Fatal("the delete claim did not run")
			}
			if plan.snapshot.RunID != "run-0" {
				t.Errorf("delete snapshot run = %q, want run-0", plan.snapshot.RunID)
			}
			if client.startCalled || client.restartCalled {
				t.Error("the start reached the broker")
			}
			got, err := s.GetAgent(ctx, agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.RunID != "run-0" {
				t.Errorf("row run = %q, want run-0 (the refused start writes nothing)", got.RunID)
			}
		})
	}
}

// finalize-env mints a run (beginRun); a delete that holds the row refuses
// the write, and the handler answers 409 delete_in_progress with no broker
// call (ptone/scion#2550 P1 round 4, N-1).
func TestRunID_FinalizeEnvUnderDeleteIs409(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	agent := setupBrokerAgentInPhase(t, s, "runid-finalize-del", state.PhaseProvisioning)
	if _, err := s.SetAgentRunID(ctx, agent.ID, "run-0"); err != nil {
		t.Fatal(err)
	}
	seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/env",
		map[string]interface{}{"env": map[string]string{"FOO": "bar"}})
	requireDeleteInProgress(t, rec)
	if client.createCalled {
		t.Error("finalize-env reached the broker")
	}
	got, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != "run-0" {
		t.Errorf("row run = %q, want run-0", got.RunID)
	}
}

// POST /agents on an existing stopped agent: a delete claim landing between
// the start gate and beginRun fails the start closed with 409
// delete_in_progress and no broker call (round 4, N-2).
func TestRunID_DeleteClaimBeforeBeginRun_CreateExisting(t *testing.T) {
	ctx := context.Background()
	f := handleExistingAgentAuthzSetup(t)
	client := &mockRuntimeBrokerClient{}
	agent := f.agent(t, "runid-claim-create", string(state.PhaseStopped))
	var plan *agentDeletionPlan
	cs := &claimFirstStore{Store: f.store, claim: func() {
		var err error
		if plan, err = f.srv.claimAgentDeletion(ctx, agent.ID, agentDeleteParams{}); err != nil || plan == nil {
			t.Errorf("claim: plan %v, err %v", plan, err)
		}
	}}
	f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(cs, client, false, slog.Default()))

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": agent.Slug, "projectId": f.project.ID, "resume": true,
	})
	requireDeleteInProgress(t, rec)
	if plan == nil {
		t.Fatal("the delete claim did not run")
	}
	if client.startCalled || client.createCalled || client.deleteCalled {
		t.Error("the create-existing start reached the broker")
	}
}

// wakeAgentForDM on a suspended agent: the same claim race answers
// AgentDMError delete_in_progress with no broker call and no quota held
// (round 4, N-2).
func TestRunID_DeleteClaimBeforeBeginRun_DMWake(t *testing.T) {
	ctx := context.Background()
	u := newWakeQuotaFixture(t, "runid-claim-wake", 1)
	client := &mockRuntimeBrokerClient{}
	var plan *agentDeletionPlan
	cs := &claimFirstStore{Store: u.s, claim: func() {
		var err error
		if plan, err = u.srv.claimAgentDeletion(ctx, u.target.ID, agentDeleteParams{}); err != nil || plan == nil {
			t.Errorf("claim: plan %v, err %v", plan, err)
		}
	}}
	u.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(cs, client, false, slog.Default()))
	target, err := u.s.GetAgent(ctx, u.target.ID)
	if err != nil {
		t.Fatal(err)
	}

	res, dmErr := u.srv.wakeAgentForDM(ctx, target)
	if res != nil || dmErr == nil {
		t.Fatalf("wake: res %+v, dmErr %+v; want a delete_in_progress error", res, dmErr)
	}
	if dmErr.HTTPStatus != http.StatusConflict || dmErr.Code != ErrCodeDeleteInProgress {
		t.Errorf("dmErr = %d %s, want 409 %s", dmErr.HTTPStatus, dmErr.Code, ErrCodeDeleteInProgress)
	}
	if plan == nil {
		t.Fatal("the delete claim did not run")
	}
	if client.startCalled {
		t.Error("the wake reached the broker")
	}
	if n := u.count(t); n != 0 {
		t.Errorf("broker quota held = %d, want 0", n)
	}
}

// Start, restart and DM wake of a soft-deleted agent answer 409 "agent is
// deleted; restore it first" from the start gate, before quota or beginRun
// (ptone/scion#2550 P1 round 4, n-b).
func TestStartGate_SoftDeletedAgentRefused(t *testing.T) {
	softDelete := func(t *testing.T, s store.Store, id string) {
		t.Helper()
		now := time.Now()
		n, err := s.UpdateAgentDeletion(context.Background(), id, store.DeletionPredicate{}, store.DeletionFields{DeletedAt: &now})
		if err != nil || n != 1 {
			t.Fatalf("soft delete: n %d, err %v", n, err)
		}
	}
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			srv, s := testServer(t)
			client := &mockRuntimeBrokerClient{}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
			agent := setupBrokerAgentInPhase(t, s, "softdel-"+op, state.PhaseStopped)
			softDelete(t, s, agent.ID)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+op, nil)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "agent is deleted; restore it first") {
				t.Fatalf("status %d, want 409 restore-first: %s", rec.Code, rec.Body.String())
			}
			if client.startCalled || client.restartCalled {
				t.Error("the start reached the broker")
			}
			if n := brokerReservationCount(t, s, agent.RuntimeBrokerID); n != 0 {
				t.Errorf("broker quota held = %d, want 0", n)
			}
		})
	}
	t.Run("wake", func(t *testing.T) {
		u := newWakeQuotaFixture(t, "softdel-wake", 1)
		client := &mockRuntimeBrokerClient{}
		u.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(u.s, client, false, slog.Default()))
		softDelete(t, u.s, u.target.ID)
		target, err := u.s.GetAgent(context.Background(), u.target.ID)
		if err != nil {
			t.Fatal(err)
		}
		res, dmErr := u.srv.wakeAgentForDM(context.Background(), target)
		if res != nil || dmErr == nil || dmErr.HTTPStatus != http.StatusConflict || dmErr.Message != "agent is deleted; restore it first" {
			t.Fatalf("wake: res %+v, dmErr %+v; want 409 restore-first", res, dmErr)
		}
		if client.startCalled {
			t.Error("the wake reached the broker")
		}
		if n := u.count(t); n != 0 {
			t.Errorf("broker quota held = %d, want 0", n)
		}
	})
}
