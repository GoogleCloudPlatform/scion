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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestExecCommand_ProjectScopedDisambiguation is a regression test for the
// cross-project slug collision in "scion look"/"scion exec". Two agents in
// different projects share the slug "coordinator"; the exec must target the
// container in the project named by the projectId query param. Before the fix,
// execCommand ignored projectId and resolved the slug across all projects,
// so "scion look coordinator" in one project could show another project's
// terminal output.
func TestExecCommand_ProjectScopedDisambiguation(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-A"},
		},
		{
			ContainerID: "container-B",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-B"},
		},
	}

	var execedID string
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ExecFunc: func(_ context.Context, id string, _ []string) (string, error) {
			execedID = id
			return "output-from-" + id, nil
		},
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	doExec := func(projectID string) (string, int) {
		execedID = ""
		body, _ := json.Marshal(map[string]any{"command": []string{"tmux", "capture-pane", "-p"}})
		url := "/api/v1/agents/coordinator/exec"
		if projectID != "" {
			url += "?projectId=" + projectID
		}
		r := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleAgentByID(w, r)
		return w.Body.String(), w.Code
	}

	// Exec scoped to grove-A must target container-A.
	respA, codeA := doExec("grove-A")
	if codeA != http.StatusOK {
		t.Fatalf("grove-A exec: expected 200, got %d (%s)", codeA, respA)
	}
	if execedID != "container-A" {
		t.Errorf("grove-A exec targeted %q, want container-A", execedID)
	}

	// Exec scoped to grove-B must target container-B — not whichever the
	// slug-only lookup happened to find first.
	respB, codeB := doExec("grove-B")
	if codeB != http.StatusOK {
		t.Fatalf("grove-B exec: expected 200, got %d (%s)", codeB, respB)
	}
	if execedID != "container-B" {
		t.Errorf("grove-B exec targeted %q, want container-B (cross-project slug collision)", execedID)
	}
}

// TestStopAgent_ProjectScopedDisambiguation verifies that "scion stop" targets
// the container in the requested project when two projects share an agent slug,
// rather than stopping whichever the slug matches first.
func TestStopAgent_ProjectScopedDisambiguation(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-A"},
		},
		{
			ContainerID: "container-B",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-B"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop?projectId=grove-B", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d (%s)", w.Code, w.Body.String())
	}
	if mgr.lastStopAgentID != "container-B" {
		t.Errorf("stop targeted %q, want container-B (cross-project slug collision)", mgr.lastStopAgentID)
	}
}

// TestExecCommand_NotFoundWhenOnlyInOtherProject verifies that exec does NOT
// fall back to a same-slug agent in a different project. Asking to exec
// "coordinator" in grove-B when only grove-A has one must 404 and never invoke
// the runtime — the core cross-project collision the review feedback targeted.
func TestExecCommand_NotFoundWhenOnlyInOtherProject(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-A"},
		},
	}
	execCalled := false
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, error) {
			execCalled = true
			return "", nil
		},
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	body, _ := json.Marshal(map[string]any{"command": []string{"echo", "hi"}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/exec?projectId=grove-B", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d (%s)", w.Code, w.Body.String())
	}
	if execCalled {
		t.Error("exec must not run against a same-slug agent in a different project")
	}
}

// TestStopAgent_NotFoundInProjectIsNoOp verifies that stopping a slug not
// present in the requested project is an idempotent no-op (202) and does NOT
// stop a same-slug agent that exists in a different project.
func TestStopAgent_NotFoundInProjectIsNoOp(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-A"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop?projectId=grove-B", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (idempotent no-op), got %d (%s)", w.Code, w.Body.String())
	}
	if mgr.stopCalls != 0 {
		t.Errorf("Stop was called %d time(s); must not stop a same-slug agent in another project", mgr.stopCalls)
	}
}

// TestExecCommand_NotFoundInProject verifies that exec returns 404 when the
// slug does not resolve to any agent in the requested project (and there is no
// legacy unlabeled container to fall back to).
func TestExecCommand_NotFoundInProject(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-A"},
		},
	}
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, error) { return "", nil },
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	body, _ := json.Marshal(map[string]any{"command": []string{"echo", "hi"}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/ghost/exec?projectId=grove-A", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing agent, got %d (%s)", w.Code, w.Body.String())
	}
}
