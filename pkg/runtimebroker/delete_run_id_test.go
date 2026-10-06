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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for the runId delete filter (ptone/scion#2550 P1): a delete that
// names a run acts only on that run's runtime entry, and never on an entry
// labelled with a different run.

func withRun(e api.AgentInfo, runID string) api.AgentInfo {
	e.RunID = runID
	labels := map[string]string{}
	for k, v := range e.Labels {
		labels[k] = v
	}
	labels[api.LabelRunID] = runID
	e.Labels = labels
	return e
}

const allDeleteParams = "&deleteFiles=true&removeBranch=true&softDelete=true&deletedAt=2026-10-03T00:00:00Z"

// Acceptance (a): a stale runId gets 404, and the newer run's container is
// neither deleted nor soft-deleted, with its files untouched.
func TestDeleteAgent_StaleRunID_404NoSideEffects(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Errorf("stale run delete reached DeleteTarget (container %q)", mgr.LastDeleteContainerID())
	}
	assertUntouched(t, scionB, "dev", infoB)
}

// Acceptance (a): the current runId deletes that run's entry, and the run
// reaches the runtime delete.
func TestDeleteAgent_CurrentRunID_Deletes(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-new&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-new" {
		t.Errorf("deleted container %q, want cid-new", mgr.LastDeleteContainerID())
	}
	if mgr.lastDeleteRunID != "run-new" {
		t.Errorf("runtime delete run = %q, want run-new", mgr.lastDeleteRunID)
	}
	if mgr.LastDeleteProjectPath() != scionB {
		t.Errorf("file deletion project path %q, want %q", mgr.LastDeleteProjectPath(), scionB)
	}
}

// Two entries for the name (a stale one still being removed and a new
// one): the runId picks its own, where a name-only delete would be
// ambiguous.
func TestDeleteAgent_RunIDDisambiguatesSameName(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{
		withRun(labelled("dev", "cid-old", scopeProjB, scionB), "run-old"),
		withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new"),
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 || mgr.LastDeleteContainerID() != "cid-old" {
		t.Errorf("got %d deletes, last %q; want exactly cid-old", mgr.DeleteCalls(), mgr.LastDeleteContainerID())
	}
}

// Acceptance (c): without a runId the delete resolves by name as before,
// labelled or not.
func TestDeleteAgent_NoRunID_ResolvesByNameAsBefore(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-new" {
		t.Errorf("deleted container %q, want cid-new", mgr.LastDeleteContainerID())
	}
}

// Acceptance (c): a legacy container (created before run labels existed)
// still matches by name when the hub sends a runId.
func TestDeleteAgent_RunIDWithLegacyUnlabelledContainer_Deletes(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-legacy", scopeProjB, scionB)}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-new")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-legacy" {
		t.Errorf("deleted container %q, want cid-legacy", mgr.LastDeleteContainerID())
	}
}

// A file-only agent (no runtime entry at all) whose files record no run
// (legacy) is deleted as before when a runId is sent. Files recorded as a
// newer run's are kept (ptone/scion#2675, delete_run_files_test.go).
func TestDeleteAgent_RunIDFileOnlyAgent_DeletesFiles(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteProjectPath() != scionB || !mgr.LastDeleteFiles() {
		t.Errorf("got path %q files=%v, want the file-only delete in %q", mgr.LastDeleteProjectPath(), mgr.LastDeleteFiles(), scionB)
	}
}

// The run filter applies within the requested project only: another
// project's entry with the requested run never matches.
func TestDeleteAgent_RunIDOtherProject_404(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-a", scopeProjA, scionA), "run-1")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-1"+allDeleteParams)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Errorf("expected no delete call, got %d", mgr.DeleteCalls())
	}
	assertUntouched(t, scionA, "dev", infoA)
}

func TestFilterDeleteCandidatesByRun(t *testing.T) {
	entry := func(cid, run string) api.AgentInfo { return api.AgentInfo{ContainerID: cid, RunID: run} }
	id := func(e api.AgentInfo) api.AgentInfo { return e }
	cids := func(es []api.AgentInfo) []string {
		out := []string{}
		for _, e := range es {
			out = append(out, e.ContainerID)
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		in        []api.AgentInfo
		want      []string
		wantOther bool
	}{
		{"match", []api.AgentInfo{entry("c1", "r1")}, []string{"c1"}, false},
		{"other run only", []api.AgentInfo{entry("c2", "r2")}, []string{}, true},
		{"match beside other run", []api.AgentInfo{entry("c2", "r2"), entry("c1", "r1")}, []string{"c1"}, true},
		{"legacy container", []api.AgentInfo{entry("c0", "")}, []string{"c0"}, false},
		{"legacy containers", []api.AgentInfo{entry("c0", ""), entry("c9", "")}, []string{"c0", "c9"}, false},
		{"exact match beats legacy container", []api.AgentInfo{entry("c0", ""), entry("c1", "r1")}, []string{"c1"}, false},
		{"exact match beats file-only", []api.AgentInfo{entry("", ""), entry("c1", "r1")}, []string{"c1"}, false},
		{"two exact matches stay ambiguous", []api.AgentInfo{entry("c1", "r1"), entry("c0", ""), entry("c3", "r1")}, []string{"c1", "c3"}, false},
		{"legacy beside other run", []api.AgentInfo{entry("c0", ""), entry("c2", "r2")}, []string{"c0"}, true},
		{"file-only", []api.AgentInfo{entry("", "")}, []string{""}, false},
		{"file-only beside other run", []api.AgentInfo{entry("", ""), entry("c2", "r2")}, []string{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, other := filterDeleteCandidatesByRun(tc.in, "r1", id)
			if g := cids(got); !slices.Equal(g, tc.want) {
				t.Errorf("kept %q, want %q", g, tc.want)
			}
			if other != tc.wantOther {
				t.Errorf("otherRun = %v, want %v", other, tc.wantOther)
			}
		})
	}
}

// A runId that matches one entry exactly resolves a name it shares with a
// legacy unlabelled container, where a name-only delete is ambiguous.
func TestDeleteAgent_RunIDPrefersExactOverLegacy(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{
		labelled("dev", "cid-legacy", scopeProjB, scionB),
		withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new"),
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-new")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 || mgr.LastDeleteContainerID() != "cid-new" {
		t.Errorf("got %d deletes, last %q; want exactly cid-new", mgr.DeleteCalls(), mgr.LastDeleteContainerID())
	}
}

// A run mismatch must not run the leftover-resource cleanup, which on k8s
// deletes the agent's secrets by name, and so would hit the live run's.
// A plain not-found still runs it once.
func TestDeleteAgent_RunMismatchSkipsLeftoverCleanup(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		mgr := &cleanupRecordingManager{}
		srv, home := newCleanupTestServer(t, mgr)
		scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
		mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}

		rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
		if mgr.DeleteCalls() != 0 {
			t.Errorf("expected no delete call, got %d", mgr.DeleteCalls())
		}
		assertCleanupCalls(t, mgr.cleanupCalls())
		assertUntouched(t, scionB, "dev", infoB)
	})
	t.Run("plain not found", func(t *testing.T) {
		mgr := &cleanupRecordingManager{}
		srv, _ := newCleanupTestServer(t, mgr)

		rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
		assertCleanupCalls(t, mgr.cleanupCalls(), cleanupCall{"dev", scopeProjB})
	})
}

// The hub's runId reaches StartOptions.RunID on create, start and restart,
// and the create response reports the entry's run.
func TestRunID_ThreadedIntoStartOptions(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"create", http.MethodPost, "/api/v1/agents", `{"name":"run-agent","config":{"template":"claude"},"runId":"run-create"}`},
		{"start", http.MethodPost, "/api/v1/agents/test-agent-1/start", `{"runId":"run-start"}`},
		{"restart", http.MethodPost, "/api/v1/agents/test-agent-1/restart", `{"runId":"run-restart"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code >= 300 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			want := "run-" + tc.name
			if got := mgr.LastStartOpts().RunID; got != want {
				t.Errorf("StartOptions.RunID = %q, want %q", got, want)
			}
			if tc.name == "create" {
				var resp CreateAgentResponse
				if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
					t.Fatal(err)
				}
				if resp.Agent == nil || resp.Agent.RunID != want {
					t.Errorf("response agent = %+v, want runId %q", resp.Agent, want)
				}
			}
		})
	}
}

func TestAgentInfoToResponse_RunID(t *testing.T) {
	if got := AgentInfoToResponse(api.AgentInfo{Name: "a", RunID: "r1"}).RunID; got != "r1" {
		t.Errorf("RunID = %q, want r1", got)
	}
}

// A failure from inside Manager.Start (start or restart) carries the
// startAttempted marker and the run ID, so the hub knows the broker acted
// and keeps the run ID it minted. A rejection before Manager.Start does
// not carry it.
func TestStartFailure_MarksStartAttempted(t *testing.T) {
	type errBody struct {
		Error APIError `json:"error"`
	}
	decode := func(t *testing.T, w *httptest.ResponseRecorder) APIError {
		t.Helper()
		var b errBody
		if err := json.NewDecoder(w.Body).Decode(&b); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
		return b.Error
	}
	for _, tc := range []struct {
		name, path, body string
		startErr         error
		wantStatus       int
		wantMarker       bool
	}{
		{"start runtime failure", "/api/v1/agents/test-agent-1/start", `{"runId":"run-x"}`,
			errors.New("docker run failed"), http.StatusInternalServerError, true},
		{"start name in use", "/api/v1/agents/test-agent-1/start", `{"runId":"run-x"}`,
			fmt.Errorf("start: %w", agent.ErrContainerNameInUse), http.StatusConflict, true},
		{"start run conflict", "/api/v1/agents/test-agent-1/start", `{"runId":"run-x"}`,
			fmt.Errorf("start: %w", runtime.ErrRunConflict), http.StatusConflict, true},
		{"restart run conflict", "/api/v1/agents/test-agent-1/restart", `{"runId":"run-x"}`,
			fmt.Errorf("start: %w", runtime.ErrRunConflict), http.StatusConflict, true},
		{"restart runtime failure", "/api/v1/agents/test-agent-1/restart", `{"runId":"run-x"}`,
			errors.New("docker run failed"), http.StatusInternalServerError, true},
		{"restart not found in Start", "/api/v1/agents/test-agent-1/restart", `{"runId":"run-x"}`,
			errors.New("image not found"), http.StatusNotFound, true},
		{"start rejected before Manager.Start", "/api/v1/agents/test-agent-1/start?projectId=..", `{"runId":"run-x"}`,
			errors.New("unreachable"), http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			mgr.startErr = tc.startErr
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			e := decode(t, w)
			attempted, _ := e.Details[api.BrokerErrorDetailStartAttempted].(bool)
			if attempted != tc.wantMarker {
				t.Errorf("startAttempted = %v, want %v (details %v)", attempted, tc.wantMarker, e.Details)
			}
			if tc.wantMarker && e.Details[api.BrokerErrorDetailRunID] != "run-x" {
				t.Errorf("runId detail = %v, want run-x", e.Details[api.BrokerErrorDetailRunID])
			}
			if !tc.wantMarker && mgr.StartCalls() != 0 {
				t.Errorf("Manager.Start called %d times on a pre-Start rejection", mgr.StartCalls())
			}
		})
	}
}

// A delete naming a run cancels an in-flight local start only if that
// start is for the same run (or carries no run): a stale delete for an
// earlier run must not cancel the start of the agent recreated under the
// same name.
func TestDeleteAgent_RunIDCancelsOnlyItsOwnInFlightStart(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}

	cancels := 0
	rec := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() { cancels++ })
	rec.RunID = "run-new"
	srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, rec)

	if code := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old").Code; code != http.StatusNotFound {
		t.Fatalf("stale delete: expected 404, got %d", code)
	}
	if cancels != 0 {
		t.Fatalf("stale delete cancelled the newer run's start (%d cancels)", cancels)
	}
	if code := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-new").Code; code != http.StatusNoContent {
		t.Fatalf("current delete: expected 204, got %d", code)
	}
	if cancels == 0 {
		t.Fatal("the delete for the start's own run did not cancel it")
	}
}

func TestLaunchRegistry_CancelLocalForRun(t *testing.T) {
	key := launchKey{ProjectID: "p1", Slug: "a"}
	for _, tc := range []struct {
		name, recRun, deleteRun string
		wantCancel              bool
	}{
		{"same run", "r1", "r1", true},
		{"other run", "r1", "r2", false},
		{"delete without run", "r1", "", true},
		{"launch without run", "", "r2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLaunchRegistry()
			cancelled := false
			rec := newLaunchRecord("L1", "a", "create", "", time.Time{}, func() { cancelled = true })
			rec.RunID = tc.recRun
			r.Begin(key, rec)
			r.CancelLocalForRun(key, tc.deleteRun)
			if cancelled != tc.wantCancel {
				t.Errorf("cancelled = %v, want %v", cancelled, tc.wantCancel)
			}
		})
	}
}

// afterStartManager is a mockManager whose List, once Start has been
// called, answers with afterStart (or afterErr): the runtime as the
// failed start left it.
type afterStartManager struct {
	*mockManager
	afterStart []api.AgentInfo
	afterErr   error
}

func (m *afterStartManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if m.StartCalls() == 0 {
		return m.mockManager.List(ctx, filter)
	}
	m.mu.Lock()
	m.lastListFilter = filter
	m.mu.Unlock()
	if m.afterErr != nil {
		return nil, m.afterErr
	}
	return m.afterStart, nil
}

func runEntry(name, cid, runID string) api.AgentInfo {
	e := api.AgentInfo{Name: name, ContainerID: cid, Phase: "stopped",
		Labels: map[string]string{"scion.agent": "true", "scion.name": name}}
	if runID != "" {
		e = withRun(e, runID)
	}
	return e
}

// DN1 (ptone/scion#2550 P1 round 2, revised in round 3): a failure inside
// Manager.Start reports the run the runtime holds afterwards, from one
// re-list of every runtime, so the hub records what exists rather than
// guessing. Start can fail before it removes the previous entry (that
// entry's run is reported, even from another runtime) or after creating
// the new one (the minted run). Several runs, or an unlabelled entry,
// report "" (by-name delete). No container entry anywhere, or a failed
// re-list, omits the detail, so the hub keeps the run it minted.
func TestStartFailure_ReportsCurrentRunID(t *testing.T) {
	const agentName = "test-agent-1"
	for _, tc := range []struct {
		name, op   string
		after      []api.AgentInfo
		afterErr   error
		aux        []api.AgentInfo // entries on an auxiliary runtime
		wantOK     bool
		wantRunID  string
		wantStatus int
	}{
		{"start fails before removing the previous entry", "start",
			[]api.AgentInfo{runEntry(agentName, "cid-old", "run-old")}, nil, nil, true, "run-old", http.StatusInternalServerError},
		{"start fails after creating the new entry", "start",
			[]api.AgentInfo{runEntry(agentName, "cid-new", "run-x")}, nil, nil, true, "run-x", http.StatusInternalServerError},
		{"start leaves no entry", "start", nil, nil, nil, false, "", http.StatusInternalServerError},
		{"start leaves entries of two runs", "start",
			[]api.AgentInfo{runEntry(agentName, "cid-old", "run-old"), runEntry(agentName, "cid-new", "run-x")}, nil, nil, true, "", http.StatusInternalServerError},
		{"start leaves a legacy unlabelled entry", "start",
			[]api.AgentInfo{runEntry(agentName, "cid-legacy", "")}, nil, nil, true, "", http.StatusInternalServerError},
		{"file-only and other-name entries are not entries", "start",
			[]api.AgentInfo{runEntry(agentName, "", "run-files"), runEntry("other-agent", "cid-o", "run-o")}, nil, nil, false, "", http.StatusInternalServerError},
		{"start re-list fails", "start", nil, errors.New("runtime down"), nil, false, "", http.StatusInternalServerError},
		{"the previous entry is on an auxiliary runtime", "start",
			nil, nil, []api.AgentInfo{runEntry(agentName, "cid-aux", "run-aux")}, true, "run-aux", http.StatusInternalServerError},
		{"entries on the default and an auxiliary runtime of two runs", "start",
			[]api.AgentInfo{runEntry(agentName, "cid-new", "run-x")}, nil, []api.AgentInfo{runEntry(agentName, "cid-aux", "run-aux")}, true, "", http.StatusInternalServerError},
		{"restart fails before removing the previous entry", "restart",
			[]api.AgentInfo{runEntry(agentName, "cid-old", "run-old")}, nil, nil, true, "run-old", http.StatusInternalServerError},
		{"restart leaves no entry", "restart", nil, nil, nil, false, "", http.StatusInternalServerError},
		{"restart's previous entry is on an auxiliary runtime", "restart",
			nil, nil, []api.AgentInfo{runEntry(agentName, "cid-aux", "run-aux")}, true, "run-aux", http.StatusInternalServerError},
		{"restart re-list fails", "restart", nil, errors.New("runtime down"), nil, false, "", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newTestServer(t).manager.(*mockManager)
			base.startErr = errors.New("docker run failed")
			mgr := &afterStartManager{mockManager: base, afterStart: tc.after, afterErr: tc.afterErr}
			srv := newTestServerWithManager(t, mgr)
			if tc.aux != nil {
				addAuxManager(srv, "aux-test", &mockManager{agents: tc.aux})
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/"+tc.op, strings.NewReader(`{"runId":"run-x"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			var b struct {
				Error APIError `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&b); err != nil {
				t.Fatal(err)
			}
			if attempted, _ := b.Error.Details[api.BrokerErrorDetailStartAttempted].(bool); !attempted {
				t.Errorf("startAttempted missing: %v", b.Error.Details)
			}
			got, ok := b.Error.Details[api.BrokerErrorDetailCurrentRunID]
			if ok != tc.wantOK {
				t.Fatalf("currentRunId present = %v, want %v (details %v)", ok, tc.wantOK, b.Error.Details)
			}
			if ok && got != tc.wantRunID {
				t.Errorf("currentRunId = %v, want %q", got, tc.wantRunID)
			}
		})
	}
}

// currentRunID scopes the re-list to the request's project: another
// project's same-name entry is not this agent's run, even from a runtime
// whose List ignores the project label filter (mockManager honours only
// scion.name).
func TestCurrentRunID_ProjectScoped(t *testing.T) {
	mgr := &mockManager{}
	srv := newTestServerWithManager(t, mgr)
	mgr.agents = []api.AgentInfo{
		withRun(labelled("dev", "cid-a", scopeProjA, ""), "run-a"),
		withRun(labelled("dev", "cid-b", scopeProjB, ""), "run-b"),
	}
	if got, ok := srv.currentRunID(context.Background(), mgr, "dev", scopeProjB); !ok || got != "run-b" {
		t.Errorf("project B: got %q, %v; want run-b", got, ok)
	}
	mgr.agents = mgr.agents[:1]
	if got, ok := srv.currentRunID(context.Background(), mgr, "dev", scopeProjB); ok {
		t.Errorf("only project A's entry: got %q, %v; want no report (no entry in project B)", got, ok)
	}
}

// A List failure on any runtime, here an auxiliary one, omits the current
// run (round 3): the unlisted runtime may hold an entry.
func TestStartFailure_AuxListErrorOmitsCurrentRunID(t *testing.T) {
	mgr := &mockManager{agents: []api.AgentInfo{runEntry("dev", "cid-x", "run-x")}}
	srv := newTestServerWithManager(t, mgr)
	aux := &mockManager{}
	aux.listErr = errors.New("aux runtime unreachable")
	addAuxManager(srv, "aux-test", aux)
	if got, ok := srv.currentRunID(context.Background(), mgr, "dev", ""); ok {
		t.Errorf("got %q, reported; want no report on a list error", got)
	}
}

// N2 (round 3): the start and restart handlers scope the re-list to the
// request's projectId, so another project's same-name run is never
// reported (the hub would adopt it, and its next delete would 404 and
// leak this project's entry).
func TestStartFailure_CurrentRunIDIsProjectScoped(t *testing.T) {
	const agentName = "dev"
	entries := []api.AgentInfo{
		withRun(labelled(agentName, "cid-b", scopeProjB, ""), "run-b"),
		withRun(labelled(agentName, "cid-a", scopeProjA, ""), "run-a"),
	}
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			base := &mockManager{agents: entries, startErr: errors.New("docker run failed")}
			mgr := &afterStartManager{mockManager: base, afterStart: entries}
			srv := newTestServerWithManager(t, mgr)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/"+op+"?projectId="+scopeProjA, strings.NewReader(`{"runId":"run-x"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if mgr.StartCalls() == 0 {
				t.Fatalf("Manager.Start not reached: %d %s", w.Code, w.Body.String())
			}
			var b struct {
				Error APIError `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&b); err != nil {
				t.Fatal(err)
			}
			if got := b.Error.Details[api.BrokerErrorDetailCurrentRunID]; got != "run-a" {
				t.Errorf("currentRunId = %v, want run-a (details %v)", got, b.Error.Details)
			}
		})
	}
}

// B1 (round 2): when a runtime cannot be listed, a run mismatch on the
// others is not proof the requested run is gone (the unlisted runtime may
// hold it), so the delete fails closed instead of answering 404.
func TestDeleteAgent_RunMismatchWithListErrorFailsClosed(t *testing.T) {
	mgr := &cleanupRecordingManager{}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}
	auxMgr := &filteringMockManager{}
	auxMgr.listErr = errors.New("aux runtime unreachable")
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["substrate-eu"] = auxiliaryRuntime{
		Runtime: &runtime.MockRuntime{NameFunc: func() string { return "substrate" }},
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
	if rec.Code == http.StatusNotFound || rec.Code < 400 {
		t.Fatalf("expected a failure status, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 || auxMgr.DeleteCalls() != 0 {
		t.Errorf("expected no delete calls, got default=%d aux=%d", mgr.DeleteCalls(), auxMgr.DeleteCalls())
	}
	assertCleanupCalls(t, mgr.cleanupCalls())
	assertUntouched(t, scionB, "dev", infoB)
}

// registeredRunID reads the run ID of the launch record held for key.
func registeredRunID(srv *Server, key launchKey) (string, bool) {
	srv.launchRegistry.mu.Lock()
	defer srv.launchRegistry.mu.Unlock()
	rec, ok := srv.launchRegistry.records[key]
	if !ok {
		return "", false
	}
	return rec.RunID, true
}

// N1 (round 2): a real synchronous create records the hub's run on its
// launch record, so a stale-run delete leaves the in-flight start running
// and a delete of its own run cancels it.
func TestSyncCreate_LaunchRecordCarriesRunID(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)

	started := make(chan struct{})
	ctxErr := make(chan error, 1)
	var startCtx context.Context
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		startCtx = ctx
		close(started)
		<-ctx.Done()
		ctxErr <- ctx.Err()
		return nil, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		body := fmt.Sprintf(`{"id":"agent-a-id","name":"same-name","projectPath":%q,"runId":"run-new","config":{"task":"t"}}`, projectPath)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	waitSignal(t, started, "the create's start")

	if got, ok := registeredRunID(srv, launchKey{Slug: "same-name"}); !ok || got != "run-new" {
		t.Fatalf("launch record run = %q (registered %v), want run-new", got, ok)
	}
	del := func(runID string) {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/same-name?runId="+runID, nil))
	}
	del("run-old")
	// The delete handler cancels synchronously, so a wrongful cancel is
	// visible on the Start ctx as soon as del returns.
	if err := startCtx.Err(); err != nil {
		t.Fatalf("a stale-run delete cancelled the start: %v", err)
	}
	del("run-new")
	select {
	case <-ctxErr:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("the delete for the start's own run did not cancel it")
	}
	<-done
}

// N1 (round 2): an async admission records the hub's run on its launch
// record.
func TestAsyncCreate_LaunchRecordCarriesRunID(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	srv, _ := newAsyncTestServer(t, mgr)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-1", "asyncLaunch": true, "launchId": "L-1", "runId": "run-new",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got, ok := registeredRunID(srv, launchKey{Slug: "agent-1"}); !ok || got != "run-new" {
		t.Fatalf("launch record run = %q (registered %v), want run-new", got, ok)
	}
}

// n2 (round 2): the cancel after resolution is keyed by the resolved
// entry's project, which the early cancel cannot know when the delete
// carries no projectId. It is run-aware too: resolving a legacy entry for
// a stale run must not cancel another run's in-flight start under that
// project.
func TestDeleteAgent_PostResolutionCancelIsRunAware(t *testing.T) {
	for _, tc := range []struct {
		name, deleteRun string
		wantCancel      bool
	}{
		{"stale run", "run-old", false},
		{"launch's own run", "run-new", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			srv, home := newScopeTestServer(t, mgr)
			scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			// A legacy container (no run label) matches any runId by name.
			mgr.agents = []api.AgentInfo{labelled("dev", "cid-legacy", scopeProjB, scionB)}

			cancels := 0
			rec := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() { cancels++ })
			rec.RunID = "run-new"
			srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, rec)

			if code := doDelete(t, srv, "dev", "runId="+tc.deleteRun).Code; code != http.StatusNoContent {
				t.Fatalf("delete: expected 204, got %d", code)
			}
			if got := cancels > 0; got != tc.wantCancel {
				t.Errorf("cancelled = %v, want %v", got, tc.wantCancel)
			}
		})
	}
}

// The recorded-runtime restriction (GoogleCloudPlatform/scion#2423) applies
// before the run filter: a run that lives only on a runtime outside the
// recorded type is not found, so the delete answers 404 with no side effects
// on either runtime rather than reaching across to the other runtime.
func TestDeleteAgent_RecordedRuntimeRestrictsBeforeRunFilter(t *testing.T) {
	srv, dockerMgr, k8sMgr := newRecordedRuntimeServer(t, true)
	dockerMgr.agents = []api.AgentInfo{withRun(rrAgentInfo("docker-container"), "run-a")}
	k8sMgr.agents = []api.AgentInfo{withRun(rrAgentInfo("k8s-pod"), "run-b")}

	w := serveRR(srv, http.MethodDelete, "/api/v1/agents/"+rrAgent+rrQuery("docker")+"&runId=run-b", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
	if dockerMgr.acted() != 0 || k8sMgr.acted() != 0 {
		t.Errorf("acted: docker=%d kubernetes=%d, want none", dockerMgr.acted(), k8sMgr.acted())
	}
	if n := k8sMgr.lists.Load(); n != 0 {
		t.Errorf("kubernetes runtime listed %d times, want 0", n)
	}

	// The same run under its own recorded type is deleted there only.
	w = serveRR(srv, http.MethodDelete, "/api/v1/agents/"+rrAgent+rrQuery("kubernetes")+"&runId=run-b", "")
	if w.Code >= 300 {
		t.Fatalf("status = %d, want success; body = %s", w.Code, w.Body.String())
	}
	if k8sMgr.acted() != 1 || dockerMgr.acted() != 0 {
		t.Errorf("acted: docker=%d kubernetes=%d, want kubernetes only", dockerMgr.acted(), k8sMgr.acted())
	}
}

// panickingManager panics in Stop or Start, after recording the call,
// and, with listPanicsAfterStart, in List once Start has been called.
type panickingManager struct {
	*mockManager
	panicIn              string // "stop" or "start"
	listPanicsAfterStart bool
}

func (m *panickingManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if m.listPanicsAfterStart && m.StartCalls() > 0 {
		panic("list panicked")
	}
	return m.mockManager.List(ctx, filter)
}

func (m *panickingManager) Stop(ctx context.Context, agentID, projectPath string) error {
	err := m.mockManager.Stop(ctx, agentID, projectPath)
	if m.panicIn == "stop" {
		panic("stop panicked")
	}
	return err
}

func (m *panickingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	info, err := m.mockManager.Start(ctx, opts)
	if m.panicIn == "start" {
		panic("start panicked")
	}
	return info, err
}

// A panic once a start or restart has reached the runtime (restart's stop,
// or Manager.Start) is answered with the startAttempted marker and the run
// ID, like a failed Manager.Start, not with the recovery middleware's
// generic error.
func TestStartPanic_MarksStartAttempted(t *testing.T) {
	for _, tc := range []struct {
		name, path, panicIn string
		listPanics          bool // the re-list for the current run panics too
	}{
		{name: "start panics in Manager.Start", path: "/api/v1/agents/test-agent-1/start", panicIn: "start"},
		{name: "restart panics in Stop", path: "/api/v1/agents/test-agent-1/restart", panicIn: "stop"},
		{name: "restart panics in Manager.Start", path: "/api/v1/agents/test-agent-1/restart", panicIn: "start"},
		{name: "start panics, then the re-list panics", path: "/api/v1/agents/test-agent-1/start", panicIn: "start", listPanics: true},
		{name: "restart panics, then the re-list panics", path: "/api/v1/agents/test-agent-1/restart", panicIn: "start", listPanics: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockManager{agents: []api.AgentInfo{{ID: "container-1", Name: "test-agent-1", Phase: "running"}}}
			srv := newTestServerWithManager(t, &panickingManager{mockManager: mock, panicIn: tc.panicIn, listPanicsAfterStart: tc.listPanics})
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"runId":"run-x"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
			}
			var b struct {
				Error APIError `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&b); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if b.Error.Code != ErrCodeRuntimeError {
				t.Errorf("code = %q, want %q", b.Error.Code, ErrCodeRuntimeError)
			}
			if attempted, _ := b.Error.Details[api.BrokerErrorDetailStartAttempted].(bool); !attempted {
				t.Errorf("startAttempted missing: %v", b.Error.Details)
			}
			if b.Error.Details[api.BrokerErrorDetailRunID] != "run-x" {
				t.Errorf("runId detail = %v, want run-x", b.Error.Details[api.BrokerErrorDetailRunID])
			}
			if tc.panicIn == "stop" && mock.StartCalls() != 0 {
				t.Errorf("Manager.Start called after the stop panicked")
			}
		})
	}
}
