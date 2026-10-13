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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestListAgents(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp ListAgentsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(resp.Agents))
	}

	if resp.TotalCount != 2 {
		t.Errorf("expected totalCount 2, got %d", resp.TotalCount)
	}
}

// TestListAgents_GroveIDQueryParamNotHonoured verifies that a groveId-only
// query no longer scopes the agent list: the filter built for the manager
// must carry no scion.project_id key, so the list falls back to unscoped
// (matching every agent) rather than silently re-honouring the legacy alias.
func TestListAgents_GroveIDQueryParamNotHonoured(t *testing.T) {
	mgr := &mockManager{
		agents: []api.AgentInfo{
			{ID: "container-1", Name: "test-agent-1", Phase: "running"},
		},
	}
	srv := newTestServerWithManager(t, mgr)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents?groveId=p1", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	if v, ok := mgr.LastListFilter()["scion.project_id"]; ok {
		t.Errorf("groveId-only query must not scope the list filter, got scion.project_id=%q", v)
	}
}

// TestListAgents_StatusFiltersByPhase verifies that ?status= keeps only the
// agents whose Phase matches (case-insensitively), on the default and the
// auxiliary runtimes, and that status is not passed to the runtimes as a
// label filter: containers carry no status label, so a label match would
// return nothing (ptone/scion#3020).
func TestListAgents_StatusFiltersByPhase(t *testing.T) {
	mgr := &mockManager{
		agents: []api.AgentInfo{
			{ID: "c-1", Name: "running-1", Phase: "running"},
			{ID: "c-2", Name: "stopped-1", Phase: "stopped"},
			{ID: "c-3", Name: "running-2", Phase: "Running"},
			{ID: "c-4", Name: "error-1", Phase: "error"},
		},
	}
	srv := newTestServerWithManager(t, mgr)

	auxMgr := &mockManager{
		agents: []api.AgentInfo{
			{ID: "k8s-1", Name: "aux-running", Phase: "running", Runtime: "kubernetes"},
			{ID: "k8s-2", Name: "aux-stopped", Phase: "stopped", Runtime: "kubernetes"},
		},
	}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents?status=running", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp ListAgentsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	got := make(map[string]bool)
	for _, ag := range resp.Agents {
		got[ag.Name] = true
	}
	want := []string{"running-1", "running-2", "aux-running"}
	if len(resp.Agents) != len(want) {
		t.Errorf("expected %d agents, got %d: %v", len(want), len(resp.Agents), got)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("expected agent %q in the status=running list, got %v", name, got)
		}
	}
	if resp.TotalCount != len(want) {
		t.Errorf("expected totalCount %d, got %d", len(want), resp.TotalCount)
	}

	if v, ok := mgr.LastListFilter()["status"]; ok {
		t.Errorf("status must not be passed to the runtime label filter, got status=%q", v)
	}
	if v, ok := auxMgr.LastListFilter()["status"]; ok {
		t.Errorf("status must not be passed to the auxiliary runtime label filter, got status=%q", v)
	}
}

func TestListAgentsIncludesAuxiliaryRuntimes(t *testing.T) {
	srv := newTestServer(t)

	// Add an auxiliary runtime with a K8s agent not on the default runtime
	auxMgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:              "k8s-pod-1",
				Name:            "k8s-agent",
				Phase:           "running",
				ContainerStatus: "Running",
				Runtime:         "kubernetes",
			},
		},
	}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp ListAgentsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should include 2 default + 1 auxiliary = 3
	if resp.TotalCount != 3 {
		t.Errorf("expected totalCount 3, got %d", resp.TotalCount)
	}

	// Verify the K8s agent is included
	found := false
	for _, ag := range resp.Agents {
		if ag.Name == "k8s-agent" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected k8s-agent from auxiliary runtime to be in list")
	}
}

func TestListAgentsDeduplicatesAcrossRuntimes(t *testing.T) {
	srv := newTestServer(t)

	// Add an auxiliary runtime that has an agent with the same name as one on the default runtime
	auxMgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:              "k8s-pod-1",
				Name:            "test-agent-1", // same name as default
				Phase:           "running",
				ContainerStatus: "Running",
			},
		},
	}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	var resp ListAgentsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should still be 2, not 3, because test-agent-1 is deduplicated
	if resp.TotalCount != 2 {
		t.Errorf("expected totalCount 2 (deduplicated), got %d", resp.TotalCount)
	}
}

func TestGetAgent(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/test-agent-1", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp AgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Name != "test-agent-1" {
		t.Errorf("expected name 'test-agent-1', got '%s'", resp.Name)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/nonexistent", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}
