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

package runtimebroker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Broker-level tests for ptone/scion#2550 P2: a runtime whose handle is the
// agent name (Kubernetes) re-checks the run at delete time. These model the
// window where resolveDeleteTarget listed one run's entry but, by the time
// the runtime delete runs, the name is held by a newer run.

// nameHeldRuntime is a MockRuntime with Kubernetes' shape: the handle is
// the agent name, and Delete honours RunRef the way KubernetesRuntime does
// (exact run or unlabelled legacy entry matches; another run returns
// ErrRunMismatch and deletes nothing).
//
// Stop does nothing by default: these tests model Stop as it is after
// ptone/scion#3076, which passes the RunRef to Stop so that Kubernetes Stop
// (a Delete) gets the same run check. withStopByName models upstream main
// today, where AgentManager.deleteResolved calls Stop(id) before Delete and
// Kubernetes Stop deletes by name.
type nameHeldRuntime struct {
	runtime.MockRuntime
	mu      sync.Mutex
	holder  string // run label of the entry holding the name; "" = legacy
	present bool
	deletes []runtime.RunRef
}

// withStopByName makes Stop remove whatever entry holds the name, as
// Kubernetes Stop does before ptone/scion#3076.
func (r *nameHeldRuntime) withStopByName() *nameHeldRuntime {
	r.StopFunc = func(context.Context, string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.present = false
		return nil
	}
	return r
}

func newNameHeldRuntime(holderRun string) *nameHeldRuntime {
	r := &nameHeldRuntime{holder: holderRun, present: true}
	r.NameFunc = func() string { return "kubernetes" }
	r.DeleteFunc = func(_ context.Context, ref runtime.RunRef) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.deletes = append(r.deletes, ref)
		if !r.present {
			return nil
		}
		if ref.RunID != "" && r.holder != "" && r.holder != ref.RunID {
			return runtime.ErrRunMismatch
		}
		r.present = false
		return nil
	}
	return r
}

func (r *nameHeldRuntime) stillPresent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.present
}

// k8sStyleManager lists a stale snapshot (filteringMockManager.agents) but
// deletes through a real AgentManager over nameHeldRuntime, so the
// runtime's own run check decides.
type k8sStyleManager struct {
	cleanupRecordingManager
	real agent.Manager
}

func (m *k8sStyleManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	_, _ = m.mockManager.DeleteTarget(ctx, agentName, ref, deleteFiles, projectPath, removeBranch)
	return m.real.DeleteTarget(ctx, agentName, ref, deleteFiles, projectPath, removeBranch)
}

// A delete for run-old resolves run-old's entry from a stale list, but the
// name is now held by run-new. The runtime refuses, and the broker answers
// 404 with no file deletion and no leftover-object cleanup: run-new's entry
// and files survive.
func TestDeleteAgent_RuntimeRunMismatch_404LeavesNewRun(t *testing.T) {
	rt := newNameHeldRuntime("run-new")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&deleteFiles=true&removeBranch=true")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if !rt.stillPresent() {
		t.Error("run-new's entry was deleted by a stale run-old delete")
	}
	if got := mgr.lastDeleteRunID; got != "run-old" {
		t.Errorf("runtime delete carried run %q, want run-old", got)
	}
	assertCleanupCalls(t, mgr.cleanupCalls())
	assertUntouched(t, scionB, "dev", infoB)
}

// Upstream main today: deleteResolved's Stop(id) runs before the run-aware
// Delete, and Kubernetes Stop deletes by name, so a stale delete still
// removes the newer run's entry. P2 closes this only together with
// ptone/scion#3076.
// TODO(ptone/scion#3076): once Stop takes a RunRef, this entry must
// survive; flip the assertion (or drop this test for the one above).
func TestDeleteAgent_RuntimeRunMismatch_StopByNameStillRemovesNewRun(t *testing.T) {
	rt := newNameHeldRuntime("run-new").withStopByName()
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	_ = doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old")
	if rt.stillPresent() {
		t.Error("Stop no longer deletes by name: ptone/scion#3076 has landed; update this test")
	}
}

// A soft delete that loses the race: the mark written before the runtime
// call is undone when the runtime reports a run mismatch, so the newer
// run's agent-info.json keeps its phase and has no deletedAt.
func TestDeleteAgent_RuntimeRunMismatch_UndoesSoftDeleteMark(t *testing.T) {
	rt := newNameHeldRuntime("run-new")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	st, ok := agent.GetAgentDeleteState("dev", scionB)
	if !ok {
		t.Fatal("agent-info.json unreadable")
	}
	if st.Phase != "running" || !st.DeletedAt.IsZero() {
		t.Errorf("soft-delete mark not undone: phase %q deletedAt %v", st.Phase, st.DeletedAt)
	}
	if !rt.stillPresent() {
		t.Error("run-new's entry was deleted")
	}
}

// A legacy (unlabelled) entry is listed for a delete naming run-old, but by
// delete time the name is held by run-new. The broker passes the requested
// run (deleteRunRef) so the runtime can refuse, rather than an empty run,
// which a name-handle runtime would honour by name.
func TestDeleteAgent_LegacyEntryReplaced_PassesRequestedRun(t *testing.T) {
	rt := newNameHeldRuntime("run-new")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "dev", scopeProjB, scionB)}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&deleteFiles=true")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if !rt.stillPresent() {
		t.Error("run-new's entry was deleted via a legacy entry's name")
	}
	assertUntouched(t, scionB, "dev", infoB)
}

// A legacy entry that is still the one holding the name is deleted, with
// the requested run passed through (the runtime's legacy match applies).
func TestDeleteAgent_LegacyEntry_DeletedWithRequestedRun(t *testing.T) {
	rt := newNameHeldRuntime("")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "dev", scopeProjB, scionB)}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if rt.stillPresent() {
		t.Error("legacy entry was not deleted")
	}
	if got := mgr.lastDeleteRunID; got != "run-old" {
		t.Errorf("runtime delete carried run %q, want the requested run-old", got)
	}
	if _, err := os.Stat(filepath.Join(scionB, "agents", "dev")); !os.IsNotExist(err) {
		t.Errorf("agent files not deleted: %v", err)
	}
}

func TestDeleteRunRef(t *testing.T) {
	cases := []struct {
		name    string
		target  deleteTarget
		request string
		want    runtime.RunRef
	}{
		{"labelled entry keeps its run", deleteTarget{containerID: "c", runID: "r1"}, "r1", runtime.RunRef{ID: "c", RunID: "r1"}},
		{"legacy entry gets requested run", deleteTarget{containerID: "c"}, "r1", runtime.RunRef{ID: "c", RunID: "r1"}},
		{"legacy entry, no requested run", deleteTarget{containerID: "c"}, "", runtime.RunRef{ID: "c"}},
		{"file-only target passes no run", deleteTarget{}, "r1", runtime.RunRef{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deleteRunRef(&tc.target, tc.request); got != tc.want {
				t.Errorf("deleteRunRef = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A start the runtime refused because another live run holds the name
// (runtime.ErrRunConflict) is a 409 conflict on create, and name_in_use on
// an async launch, not a runtime error.
func TestCreateAgent_RunConflictIs409(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = fmt.Errorf("start: %w", runtime.ErrRunConflict)

	body := `{"name": "new-agent", "config": {"template": "claude"}, "runId": "run-x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), ErrCodeConflict) {
		t.Errorf("body %s lacks code %q", w.Body.String(), ErrCodeConflict)
	}
}

func TestClassifyStartError_RunConflict(t *testing.T) {
	code, _ := classifyStartError(context.Background(), fmt.Errorf("start: %w", runtime.ErrRunConflict))
	if code != "name_in_use" {
		t.Fatalf("code = %q, want name_in_use", code)
	}
}
