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
	"strings"
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

// relistingManager lists run-old's entry for resolution, then, on the
// first DeleteTarget (the runtime found the name taken by another run and
// refused), lists after, as the runtime holds it now. Later DeleteTarget
// calls (a retry) delete as filteringMockManager does.
type relistingManager struct {
	filteringMockManager
	after   []api.AgentInfo
	refused bool
}

func (m *relistingManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.mu.Lock()
	first := !m.refused
	m.refused = true
	m.mu.Unlock()
	if !first {
		return m.filteringMockManager.DeleteTarget(ctx, agentName, ref, deleteFiles, projectPath, removeBranch)
	}
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
		{"unlabelled container does not count, listed last", []agentCandidate{c("b", "run-b"), c("a", "")}, "run-b"},
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

// Finding 6 (review round 1): the main path and the DeleteTarget race path
// apply one selection (selectDeleteCandidates) to the same runtime state,
// so the hub gets the same outcome either way. The main path lists state
// at resolution; the race path resolves run-old's entry, is refused by the
// runtime, then re-lists state. A container the delete may target on the
// race path (a legacy unlabelled one) gets a retryable 409, and the retry,
// resolving state afresh, deletes it as the main path does.
func TestDeleteAgent_MainAndRacePathsAgree(t *testing.T) {
	type outcome struct {
		code    int
		current string // run_mismatch currentRunId; "" omitted
		deleted string // container the final delete removed; "" none
	}
	for _, tc := range []struct {
		name  string
		state func(scionB string) []api.AgentInfo
		main  outcome
		race  outcome // first answer
		retry outcome // the hub's retry after the race answer
	}{
		{
			name: "other run alone",
			state: func(scionB string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-b", scopeProjB, scionB), "run-b")}
			},
			main:  outcome{code: http.StatusNotFound, current: "run-b"},
			race:  outcome{code: http.StatusNotFound, current: "run-b"},
			retry: outcome{code: http.StatusNotFound, current: "run-b"},
		},
		{
			name: "unlabelled legacy container beside the other run",
			state: func(scionB string) []api.AgentInfo {
				return []api.AgentInfo{
					labelled("dev", "cid-legacy", scopeProjB, scionB),
					withRun(labelled("dev", "cid-b", scopeProjB, scionB), "run-b"),
				}
			},
			main:  outcome{code: http.StatusNoContent, deleted: "cid-legacy"},
			race:  outcome{code: http.StatusConflict},
			retry: outcome{code: http.StatusNoContent, deleted: "cid-legacy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(t *testing.T, step string, rec *httptest.ResponseRecorder, want outcome, deletedID string) {
				t.Helper()
				if rec.Code != want.code {
					t.Fatalf("%s: status %d, want %d: %s", step, rec.Code, want.code, rec.Body.String())
				}
				switch rec.Code {
				case http.StatusNotFound:
					assertDeleteRunMismatch(t, rec, want.current)
				case http.StatusConflict:
					e := deleteErrBody(t, rec)
					if e.Code != ErrCodeConflict {
						t.Errorf("%s: code = %q, want %q", step, e.Code, ErrCodeConflict)
					}
					if strings.Contains(rec.Body.String(), "run-b") {
						t.Errorf("%s: the retry 409 names the other run: %s", step, rec.Body.String())
					}
				}
				if want.deleted != "" && deletedID != want.deleted {
					t.Errorf("%s: deleted %q, want %q", step, deletedID, want.deleted)
				}
			}
			t.Run("main", func(t *testing.T) {
				mgr := &filteringMockManager{}
				srv, home := newScopeTestServer(t, mgr)
				scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
				mgr.agents = tc.state(scionB)
				rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old")
				check(t, "main", rec, tc.main, mgr.LastDeleteContainerID())
			})
			t.Run("race", func(t *testing.T) {
				mgr := &relistingManager{}
				srv, home := newCleanupTestServer(t, mgr)
				scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
				mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-old", scopeProjB, scionB), "run-old")}
				mgr.after = tc.state(scionB)
				rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old")
				check(t, "race", rec, tc.race, "")
				rec = doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old")
				check(t, "retry", rec, tc.retry, mgr.LastDeleteContainerID())
			})
		})
	}
}
