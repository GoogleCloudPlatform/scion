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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Stop and restart requests that carry no project ID (solo/CLI mode) must
// not fall back to acting on the bare slug after a lookup error
// (ptone/scion#2549). A listing failure is a 503 and an ambiguous match is a
// 500, exactly as with a project ID, and neither stops nor starts anything.

func twoProjectSameSlugAgents() []api.AgentInfo {
	return []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
		{
			ContainerID: "container-B",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-B"},
		},
	}
}

func assertErrorCode(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d (%s)", w.Code, wantStatus, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
	}
	if resp.Error.Code != wantCode {
		t.Errorf("error code = %q, want %q", resp.Error.Code, wantCode)
	}
}

func TestStopAgent_NoProjectID_ListUnavailableReturns503(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectSameSlugAgents()[:1]
	mgr.listErr = fmt.Errorf("docker ps failed against https://10.0.0.5:2376")
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	assertErrorCode(t, w, http.StatusServiceUnavailable, ErrCodeRuntimeUnavailable)
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s) (target %q); a listing failure must not fall back to the bare slug",
			mgr.StopCalls(), mgr.LastStopAgentID())
	}
	if strings.Contains(w.Body.String(), "10.0.0.5") {
		t.Errorf("response body leaked the raw runtime-listing error: %s", w.Body.String())
	}
}

func TestStopAgent_NoProjectID_AmbiguousMatchAbortsWithoutStop(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectSameSlugAgents()
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	assertErrorCode(t, w, http.StatusInternalServerError, ErrCodeRuntimeError)
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s) (target %q); an ambiguous match must not fall back to the bare slug",
			mgr.StopCalls(), mgr.LastStopAgentID())
	}
}

func TestStopAgent_NoProjectID_SingleMatchStopsContainer(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectSameSlugAgents()[:1]
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%s)", w.Code, http.StatusAccepted, w.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.LastStopAgentID() != "container-A" {
		t.Errorf("Stop calls = %d, target = %q; want 1 call on container-A", mgr.StopCalls(), mgr.LastStopAgentID())
	}
}

func TestStopAgent_NoProjectID_NotFoundStopsBareSlug(t *testing.T) {
	// A genuine not-found with no project ID keeps the solo/CLI behaviour:
	// the stop is passed through with the bare slug.
	mgr := &filteringMockManager{}
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%s)", w.Code, http.StatusAccepted, w.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.LastStopAgentID() != "coordinator" {
		t.Errorf("Stop calls = %d, target = %q; want 1 call on the bare slug", mgr.StopCalls(), mgr.LastStopAgentID())
	}
}

func TestRestartAgent_NoProjectID_ListUnavailableReturns503(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectSameSlugAgents()[:1]
	mgr.listErr = fmt.Errorf("docker ps failed against https://10.0.0.5:2376")
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	assertErrorCode(t, w, http.StatusServiceUnavailable, ErrCodeRuntimeUnavailable)
	if mgr.StopCalls() != 0 || mgr.StartCalls() != 0 {
		t.Errorf("Stop calls = %d, Start calls = %d; a listing failure must abort the restart",
			mgr.StopCalls(), mgr.StartCalls())
	}
	if strings.Contains(w.Body.String(), "10.0.0.5") {
		t.Errorf("response body leaked the raw runtime-listing error: %s", w.Body.String())
	}
}

func TestRestartAgent_NoProjectID_AmbiguousMatchAbortsWithoutStart(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectSameSlugAgents()
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	assertErrorCode(t, w, http.StatusInternalServerError, ErrCodeRuntimeError)
	if mgr.StopCalls() != 0 || mgr.StartCalls() != 0 {
		t.Errorf("Stop calls = %d, Start calls = %d; an ambiguous match must abort the restart",
			mgr.StopCalls(), mgr.StartCalls())
	}
}

func TestRestartAgent_NoProjectID_SingleMatchStopsThenStarts(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectSameSlugAgents()[:1]
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%s)", w.Code, http.StatusAccepted, w.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.LastStopAgentID() != "container-A" {
		t.Errorf("Stop calls = %d, target = %q; want 1 call on container-A", mgr.StopCalls(), mgr.LastStopAgentID())
	}
	if mgr.StartCalls() != 1 {
		t.Errorf("Start calls = %d, want 1", mgr.StartCalls())
	}
}

func TestRestartAgent_NoProjectID_NotFoundStopsBareSlugThenStarts(t *testing.T) {
	// A genuine not-found with no project ID keeps the solo/CLI behaviour:
	// the restart stops the bare slug and then starts the agent.
	mgr := &filteringMockManager{}
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%s)", w.Code, http.StatusAccepted, w.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.LastStopAgentID() != "coordinator" {
		t.Errorf("Stop calls = %d, target = %q; want 1 call on the bare slug", mgr.StopCalls(), mgr.LastStopAgentID())
	}
	if mgr.StartCalls() != 1 {
		t.Errorf("Start calls = %d, want 1", mgr.StartCalls())
	}
}
