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

// A file-only agent (no runtime entry at all) is deleted as before when a
// runId is sent: there is no other run's entry to protect. (The window
// where a newer run has files but no container yet is ptone/scion#2675.)
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
