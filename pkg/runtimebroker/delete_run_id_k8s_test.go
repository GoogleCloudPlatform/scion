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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
// Stop does nothing by default. withStopAsDelete models KubernetesRuntime
// exactly, where Stop is the same run-checked Delete
// (AgentManager.deleteResolved calls Stop(ref) before Delete(ref)).
type nameHeldRuntime struct {
	runtime.MockRuntime
	mu      sync.Mutex
	holder  string // run label of the entry holding the name; "" = legacy
	present bool
	deletes []runtime.RunRef
}

// withStopAsDelete makes Stop the run-checked delete, as Kubernetes Stop is.
func (r *nameHeldRuntime) withStopAsDelete() *nameHeldRuntime {
	r.StopFunc = r.runCheckedDelete
	return r
}

// runCheckedDelete removes the entry holding the name unless ref names a
// different run than its (non-empty) run label, as KubernetesRuntime.Delete
// does.
func (r *nameHeldRuntime) runCheckedDelete(_ context.Context, ref runtime.RunRef) error {
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

func newNameHeldRuntime(holderRun string) *nameHeldRuntime {
	r := &nameHeldRuntime{holder: holderRun, present: true}
	r.NameFunc = func() string { return "kubernetes" }
	r.DeleteFunc = r.runCheckedDelete
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

// The same stale delete with Stop modelled as Kubernetes has it since
// ptone/scion#3076 (Stop takes the RunRef and is the run-checked Delete):
// deleteResolved's Stop(ref) before Delete(ref) refuses too, so run-new's
// entry survives and the delete is a 404.
func TestDeleteAgent_RuntimeRunMismatch_RunCheckedStopLeavesNewRun(t *testing.T) {
	rt := newNameHeldRuntime("run-new").withStopAsDelete()
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&deleteFiles=true")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if !rt.stillPresent() {
		t.Error("run-new's entry was removed by a stale run-old delete's Stop")
	}
	rt.mu.Lock()
	deletes := append([]runtime.RunRef(nil), rt.deletes...)
	rt.mu.Unlock()
	if len(deletes) != 2 {
		t.Fatalf("runtime calls = %+v, want Stop then Delete", deletes)
	}
	for _, ref := range deletes {
		if ref.RunID != "run-old" {
			t.Errorf("runtime call carried run %q, want run-old", ref.RunID)
		}
	}
	assertUntouched(t, scionB, "dev", infoB)
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

	// An agent soft-deleted before keeps its earlier deletedAt across
	// restarts; the undo must bring that value back, not clear it.
	prior := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := agent.UpdateAgentDeletedAt("dev", scionB, prior); err != nil {
		t.Fatal(err)
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	st, ok := agent.GetAgentDeleteState("dev", scionB)
	if !ok {
		t.Fatal("agent-info.json unreadable")
	}
	if st.Phase != "running" || !st.DeletedAt.Equal(prior) {
		t.Errorf("soft-delete mark not undone: phase %q deletedAt %v, want running and %v", st.Phase, st.DeletedAt, prior)
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
	mgr.startErr = errIdentityRunConflict

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
	assertNoRunConflictLeak(t, w.Body.String())
}

func TestClassifyStartError_RunConflict(t *testing.T) {
	code, msg := classifyStartError(context.Background(), errIdentityRunConflict)
	if code != "name_in_use" {
		t.Fatalf("code = %q, want name_in_use", code)
	}
	assertNoRunConflictLeak(t, msg)
}

// Any other runtime failure after the soft-delete mark (nothing confirmed
// deleted) also undoes the mark; the delete answers with a runtime error.
func TestDeleteAgent_RuntimeFailure_UndoesSoftDeleteMark(t *testing.T) {
	rt := newNameHeldRuntime("run-old")
	rt.DeleteFunc = func(context.Context, runtime.RunRef) error { return fmt.Errorf("apiserver unavailable") }
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code < 500 {
		t.Fatalf("expected a 5xx, got %d: %s", rec.Code, rec.Body.String())
	}
	st, _ := agent.GetAgentDeleteState("dev", scionB)
	if st.Phase != "running" || !st.DeletedAt.IsZero() {
		t.Errorf("soft-delete mark not undone: phase %q deletedAt %v", st.Phase, st.DeletedAt)
	}
}

// The undo changes nothing unless agent-info.json still shows the mark:
// a phase a newer start wrote after the snapshot is kept.
func TestRestoreAgentDeleteState_OnlyUndoesOwnMark(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	snap := agent.AgentDeleteState{Phase: "stopped"}

	if err := agent.RestoreAgentDeleteState("dev", scionB, snap); err != nil {
		t.Fatal(err)
	}
	if st, _ := agent.GetAgentDeleteState("dev", scionB); st.Phase != "running" {
		t.Errorf("restore overwrote a newer phase: %q", st.Phase)
	}

	if err := agent.UpdateAgentConfig("dev", scionB, "deleted", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := agent.RestoreAgentDeleteState("dev", scionB, snap); err != nil {
		t.Fatal(err)
	}
	if st, _ := agent.GetAgentDeleteState("dev", scionB); st.Phase != "stopped" {
		t.Errorf("restore did not undo the mark: %q", st.Phase)
	}
}

// errIdentityRunConflict is an ErrRunConflict wrapped the way the Kubernetes
// runtime wraps it, carrying a namespace, an object name and a run ID that
// must not reach HTTP clients.
var errIdentityRunConflict = fmt.Errorf("start: %w", fmt.Errorf("%w: secretproviderclass leakns/scion-agent-leakobj belongs to run %q",
	runtime.ErrRunConflict, "run-leak-123"))

func assertNoRunConflictLeak(t *testing.T, body string) {
	t.Helper()
	for _, s := range []string{"leakns", "leakobj", "run-leak-123"} {
		if strings.Contains(body, s) {
			t.Errorf("response body leaks %q: %s", s, body)
		}
	}
	if !strings.Contains(body, runtime.ErrRunConflict.Error()) {
		t.Errorf("response body lacks the fixed conflict text: %s", body)
	}
}

func TestStartAndRestart_RunConflict_409NoLeak(t *testing.T) {
	for _, path := range []string{"/api/v1/agents/test-agent-1/start", "/api/v1/agents/test-agent-1/restart"} {
		t.Run(path, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			mgr.startErr = errIdentityRunConflict
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"runId":"run-x"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
			}
			assertNoRunConflictLeak(t, w.Body.String())
		})
	}
}

// Restart maps a container name already in use to 409, as create and start
// do.
func TestRestart_NameInUse_409(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = fmt.Errorf("start: %w", agent.ErrContainerNameInUse)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", strings.NewReader(`{"runId":"run-x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
}

// filesFailManager deletes the runtime entry successfully and then fails
// file cleanup before removing anything, as DeleteAgentFiles does on a
// registry error.
type filesFailManager struct{ k8sStyleManager }

func (m *filesFailManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	if _, err := m.k8sStyleManager.DeleteTarget(ctx, agentName, ref, false, projectPath, removeBranch); err != nil {
		return false, err
	}
	return false, errors.New("delete: FindBranchForAgent for dev: registry I/O error")
}

// Regression (P2 review round 3, B1): when the runtime delete succeeded and
// only file cleanup failed, the soft-delete mark stays: the pod is gone.
func TestDeleteAgent_FilesFailureAfterRuntimeDelete_KeepsMark(t *testing.T) {
	rt := newNameHeldRuntime("run-old")
	mgr := &filesFailManager{k8sStyleManager{real: agent.NewManager(rt)}}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&deleteFiles=true&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code < 500 {
		t.Fatalf("expected a 5xx, got %d: %s", rec.Code, rec.Body.String())
	}
	if rt.stillPresent() {
		t.Fatal("runtime entry not deleted; the test premise does not hold")
	}
	if st, _ := agent.GetAgentDeleteState("dev", scionB); st.Phase != "deleted" {
		t.Errorf("runtime delete succeeded but the soft-delete mark was undone: phase %q deletedAt %v", st.Phase, st.DeletedAt)
	}
}

// A malformed runId is a 400 validation error at the broker boundary, with
// fixed text, and nothing is resolved or deleted.
func TestDeleteAgent_InvalidRunID_400(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid", scopeProjB, scionB), "run-new")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-new,scion.agent&deleteFiles=true&softDelete=true")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ErrCodeValidationError) {
		t.Errorf("body lacks %q: %s", ErrCodeValidationError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "scion.agent") {
		t.Errorf("body echoes the run ID: %s", rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Errorf("DeleteTarget called %d times", mgr.DeleteCalls())
	}
	assertUntouched(t, scionB, "dev", infoB)
}
