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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Restart-specific consequences of the strict agent target lookup
// (lookupAgentTarget): restart aborts when it cannot determine the stop
// target, and stops through the manager whose runtime reported it.

const restartLookupProject = "project-restart-lookup"

func restartLookupAuxAgent(containerID string) api.AgentInfo {
	return api.AgentInfo{
		Name:        "dev",
		ContainerID: containerID,
		Labels:      map[string]string{"scion.name": "dev", projectkeys.LabelProjectID: restartLookupProject},
	}
}

func addAuxManager(srv *Server, name string, mgr *mockManager) {
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes[name] = auxiliaryRuntime{
		Runtime: &runtime.MockRuntime{NameFunc: func() string { return name }},
		Manager: mgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()
}

func doRestart(srv *Server) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/restart?projectId="+restartLookupProject, nil))
	return w
}

// An auxiliary runtime that cannot list, with no match anywhere, means the
// restart cannot determine whether the agent is running: it must abort with
// a 5xx and start nothing, rather than treating it as not-found and
// starting a second container.
func TestRestart_AuxListErrorWithNoMatch_AbortsWithoutStart(t *testing.T) {
	defaultMgr := &mockManager{}
	srv := newTestServerWithManager(t, defaultMgr)
	auxMgr := &mockManager{listErr: errors.New("apiserver: connection refused")}
	addAuxManager(srv, "kubernetes", auxMgr)

	w := doRestart(srv)

	if w.Code < 500 {
		t.Fatalf("status=%d body=%s, want 5xx (an unlistable runtime may hold the agent)", w.Code, w.Body.String())
	}
	if defaultMgr.StartCalls() != 0 {
		t.Errorf("Start called %d time(s); restart must not start when the target cannot be determined", defaultMgr.StartCalls())
	}
	if defaultMgr.StopCalls() != 0 || auxMgr.StopCalls() != 0 {
		t.Errorf("Stop calls default=%d aux=%d, want none", defaultMgr.StopCalls(), auxMgr.StopCalls())
	}
}

// A failing default-runtime List in the unlabelled-fallback stage is also
// "cannot determine" for restart: the error is not dropped and restart does
// not proceed.
func TestRestart_FallbackStageListError_AbortsWithoutStart(t *testing.T) {
	mgr := &scopedThenFailManager{failErr: errors.New("docker ps: connection reset")}
	srv := newTestServerWithManager(t, mgr)

	w := doRestart(srv)

	if w.Code < 500 {
		t.Fatalf("status=%d body=%s, want 5xx", w.Code, w.Body.String())
	}
	if mgr.StartCalls() != 0 {
		t.Errorf("Start called %d time(s); want 0", mgr.StartCalls())
	}
}

// Restart's stop is dispatched through the manager whose runtime reported
// the target container ID — never a container ID from one runtime sent to
// another runtime's manager, as a second, independent map-ordered lookup
// could do.
func TestRestart_TwoAuxMatches_StopTargetAndManagerAgree(t *testing.T) {
	for i := 0; i < 30; i++ {
		defaultMgr := &mockManager{}
		srv := newTestServerWithManager(t, defaultMgr)
		mgrA := &mockManager{agents: []api.AgentInfo{restartLookupAuxAgent("c-a")}}
		mgrB := &mockManager{agents: []api.AgentInfo{restartLookupAuxAgent("c-b")}}
		addAuxManager(srv, "aux-b", mgrB)
		addAuxManager(srv, "aux-a", mgrA)

		w := doRestart(srv)

		if w.Code != http.StatusAccepted {
			t.Fatalf("iteration %d: status=%d body=%s, want 202", i, w.Code, w.Body.String())
		}
		if mgrA.StopCalls() != 1 || mgrA.LastStopAgentID() != "c-a" || mgrB.StopCalls() != 0 || defaultMgr.StopCalls() != 0 {
			t.Fatalf("iteration %d: stops aux-a=%d(%q) aux-b=%d(%q) default=%d(%q), want only aux-a stopping c-a",
				i, mgrA.StopCalls(), mgrA.LastStopAgentID(), mgrB.StopCalls(), mgrB.LastStopAgentID(), defaultMgr.StopCalls(), defaultMgr.LastStopAgentID())
		}
	}
}
