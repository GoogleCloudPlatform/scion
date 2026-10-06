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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#3080: a run-scoped delete that finds the name held
// by another run answers the run-mismatch 404 naming that run
// (currentRunId), so the hub does not finalize its row; with nothing of
// any run left it answers the plain 404.

// deleteErrBody decodes a delete's error body.
func deleteErrBody(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var b struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return b.Error
}

// assertDeleteRunMismatch checks rec is the run-mismatch 404 for run-old,
// naming current (omitted when current is "").
func assertDeleteRunMismatch(t *testing.T, rec *httptest.ResponseRecorder, current string) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	e := deleteErrBody(t, rec)
	if e.Code != api.BrokerErrorCodeRunMismatch {
		t.Fatalf("code = %q, want %q (body %s)", e.Code, api.BrokerErrorCodeRunMismatch, rec.Body.String())
	}
	if got := e.Details[api.BrokerErrorDetailRunID]; got != "run-old" {
		t.Errorf("details runId = %v, want run-old", got)
	}
	got, ok := e.Details[api.BrokerErrorDetailCurrentRunID]
	switch {
	case current == "" && ok:
		t.Errorf("details currentRunId = %v, want it omitted", got)
	case current != "" && got != current:
		t.Errorf("details currentRunId = %v, want %q (details %v)", got, current, e.Details)
	}
}

// assertPlainDeleteNotFound checks rec is the plain agent-not-found 404,
// with no run-mismatch code and no current run.
func assertPlainDeleteNotFound(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	e := deleteErrBody(t, rec)
	if e.Code != ErrCodeAgentNotFound {
		t.Errorf("code = %q, want %q", e.Code, ErrCodeAgentNotFound)
	}
	if got, ok := e.Details[api.BrokerErrorDetailCurrentRunID]; ok {
		t.Errorf("details currentRunId = %v, want none", got)
	}
}

// A delete of run-old while run-new's container holds the name: the
// run-mismatch 404 names run-new, and nothing is deleted.
func TestDeleteAgent_OtherRunHoldsName_ReportsCurrentRun(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
	assertDeleteRunMismatch(t, rec, "run-new")
	if mgr.DeleteCalls() != 0 {
		t.Errorf("delete reached DeleteTarget (container %q)", mgr.LastDeleteContainerID())
	}
	assertUntouched(t, scionB, "dev", infoB)
}

// Containers of two other runs hold the name: no single current run, so
// the run-mismatch 404 names none (the hub keeps today's handling).
func TestDeleteAgent_TwoOtherRunsHoldName_NoCurrentRun(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{
		withRun(labelled("dev", "cid-b", scopeProjB, scionB), "run-b"),
		withRun(labelled("dev", "cid-c", scopeProjB, scionB), "run-c"),
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old")
	assertDeleteRunMismatch(t, rec, "")
	if mgr.DeleteCalls() != 0 {
		t.Errorf("delete reached DeleteTarget (container %q)", mgr.LastDeleteContainerID())
	}
}

// No entry of any run, nothing in flight: the plain 404.
func TestDeleteAgent_NoEntry_PlainNotFound(t *testing.T) {
	mgr := &cleanupRecordingManager{}
	srv, _ := newCleanupTestServer(t, mgr)

	assertPlainDeleteNotFound(t, doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"))
}

// No entry, but a start of run-new is in flight here: run-new holds the
// name, as for a run-scoped stop.
func TestDeleteAgent_NoEntryOtherRunInFlight_ReportsCurrentRun(t *testing.T) {
	mgr := &cleanupRecordingManager{}
	srv, _ := newCleanupTestServer(t, mgr)
	lr := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() {})
	lr.RunID = "run-new"
	srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, lr)

	assertDeleteRunMismatch(t, doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"), "run-new")
}

// Files only, recorded as run-new's, nothing in flight: no container of
// run-new, so the run-mismatch 404 names no current run (files alone are
// not a running agent).
func TestDeleteAgent_FilesOfOtherRunOnly_NoCurrentRun(t *testing.T) {
	mgr := &cleanupRecordingManager{}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	recordRun(t, scionB, "dev", "run-new")

	assertDeleteRunMismatch(t, doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"), "")
}

// relistingManager lists run-old's entry for resolution, then, once
// DeleteTarget is called (the runtime found the name taken by run-new and
// refused), lists run-new's entry, as the runtime holds it now.
type relistingManager struct {
	filteringMockManager
	after []api.AgentInfo
}

func (m *relistingManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	_, _ = m.filteringMockManager.DeleteTarget(ctx, agentName, ref, deleteFiles, projectPath, removeBranch)
	m.mu.Lock()
	m.agents = m.after
	m.mu.Unlock()
	return false, runtime.ErrRunMismatch
}

// The entry was replaced between resolution and the runtime delete: the
// broker re-lists and names the run that holds the name now. A delete
// naming no run still answers the plain 404.
func TestDeleteAgent_RuntimeRunMismatch_ReportsCurrentRun(t *testing.T) {
	setup := func(t *testing.T) *Server {
		mgr := &relistingManager{}
		srv, home := newCleanupTestServer(t, mgr)
		scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
		mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-old", scopeProjB, scionB), "run-old")}
		mgr.after = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}
		return srv
	}
	t.Run("run-scoped", func(t *testing.T) {
		assertDeleteRunMismatch(t, doDelete(t, setup(t), "dev", "projectId="+scopeProjB+"&runId=run-old"), "run-new")
	})
	t.Run("no run", func(t *testing.T) {
		assertPlainDeleteNotFound(t, doDelete(t, setup(t), "dev", "projectId="+scopeProjB))
	})
}

func TestOtherRunContainerID(t *testing.T) {
	c := func(cid, run string) agentCandidate {
		return agentCandidate{entry: api.AgentInfo{ContainerID: cid, RunID: run}}
	}
	for _, tc := range []struct {
		name  string
		cands []agentCandidate
		want  string
	}{
		{"one other run", []agentCandidate{c("a", "run-b")}, "run-b"},
		{"two containers of one other run", []agentCandidate{c("a", "run-b"), c("b", "run-b")}, "run-b"},
		{"two other runs", []agentCandidate{c("a", "run-b"), c("b", "run-c")}, ""},
		{"file-only entry does not count", []agentCandidate{c("", "run-b")}, ""},
		{"unlabelled container does not count", []agentCandidate{c("a", ""), c("b", "run-b")}, "run-b"},
		{"the requested run does not count", []agentCandidate{c("a", "run-a"), c("b", "run-b")}, "run-b"},
		{"none", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := otherRunContainerID(tc.cands, "run-a"); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
