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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		var out []string
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
		{"other run only", []api.AgentInfo{entry("c2", "r2")}, nil, true},
		{"match beside other run", []api.AgentInfo{entry("c2", "r2"), entry("c1", "r1")}, []string{"c1"}, true},
		{"legacy container", []api.AgentInfo{entry("c0", "")}, []string{"c0"}, false},
		{"file-only", []api.AgentInfo{entry("", "")}, []string{""}, false},
		{"file-only beside other run", []api.AgentInfo{entry("", ""), entry("c2", "r2")}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, other := filterDeleteCandidatesByRun(tc.in, "r1", id)
			if g := cids(got); len(g) != len(tc.want) || (len(g) > 0 && g[0] != tc.want[0]) {
				t.Errorf("kept %v, want %v", g, tc.want)
			}
			if other != tc.wantOther {
				t.Errorf("otherRun = %v, want %v", other, tc.wantOther)
			}
		})
	}
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
