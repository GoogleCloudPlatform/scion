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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
		{
			ContainerID: "container-B",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-B"},
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

	// Exec scoped to project-A must target container-A.
	respA, codeA := doExec("project-A")
	if codeA != http.StatusOK {
		t.Fatalf("project-A exec: expected 200, got %d (%s)", codeA, respA)
	}
	if execedID != "container-A" {
		t.Errorf("project-A exec targeted %q, want container-A", execedID)
	}

	// Exec scoped to project-B must target container-B — not whichever the
	// slug-only lookup happened to find first.
	respB, codeB := doExec("project-B")
	if codeB != http.StatusOK {
		t.Fatalf("project-B exec: expected 200, got %d (%s)", codeB, respB)
	}
	if execedID != "container-B" {
		t.Errorf("project-B exec targeted %q, want container-B (cross-project slug collision)", execedID)
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
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
		{
			ContainerID: "container-B",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-B"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop?projectId=project-B", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d (%s)", w.Code, w.Body.String())
	}
	if mgr.LastStopAgentID() != "container-B" {
		t.Errorf("stop targeted %q, want container-B (cross-project slug collision)", mgr.LastStopAgentID())
	}
}

// TestExecCommand_NotFoundWhenOnlyInOtherProject verifies that exec does NOT
// fall back to a same-slug agent in a different project. Asking to exec
// "coordinator" in project-B when only project-A has one must 404 and never invoke
// the runtime — the core cross-project collision the review feedback targeted.
func TestExecCommand_NotFoundWhenOnlyInOtherProject(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
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
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/exec?projectId=project-B", bytes.NewReader(body))
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
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop?projectId=project-B", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (idempotent no-op), got %d (%s)", w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s); must not stop a same-slug agent in another project", mgr.StopCalls())
	}
}

// TestStopAgent_LookupErrorReturns5xx is a regression test for #1985: when
// resolving the project-scoped stop target fails for a reason other than
// genuine "not found" (here, the runtime listing itself errors), stopAgent
// must surface a 5xx rather than reporting the idempotent-no-op 202 that a
// real "not found" gets. Before the fix, projectScopedTarget mapped every
// lookup failure to "" and stopAgent treated "" as success.
//
// A runtime listing failure is specifically an ErrAgentListUnavailable case:
// the response is 503 (matching the PTY attach handler's precedent for the
// same error), and the raw listing error's own text — which can carry a
// runtime's connection target, namespace, or identity — must never reach the
// body, only the server's log.
func TestStopAgent_LookupErrorReturns5xx(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	mgr.listErr = fmt.Errorf("docker ps failed against https://10.0.0.5:2376 (namespace scion-prod)")
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop?projectId=project-A", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code < 500 {
		t.Fatalf("expected a 5xx status when the lookup fails, got %d (%s)", w.Code, w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected %d for a runtime-listing failure, got %d (%s)", http.StatusServiceUnavailable, w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s); a lookup failure must not be treated as a successful stop", mgr.StopCalls())
	}
	if strings.Contains(w.Body.String(), "10.0.0.5") || strings.Contains(w.Body.String(), "scion-prod") {
		t.Errorf("response body leaked the raw runtime-listing error: %s", w.Body.String())
	}
}

// TestStopAgent_AmbiguousMatchAbortsWithoutStop is a regression test for
// #1985: when the project-scoped lookup finds more than one distinct
// container matching the slug (uniqueAgentEntry's ambiguous case), that is a
// real lookup failure, not a "not found," so stopAgent must abort with a 5xx
// and must NOT call Stop — a lookup that can't tell which container to stop
// must not guess and stop one of them anyway. Mirrors
// TestRestartAgent_AmbiguousMatchAbortsWithoutStart.
//
// The status and code are pinned exactly (500 runtime_error, not just "some
// 5xx") because an ambiguous match is NOT ErrAgentListUnavailable: the
// runtime answered fine, it just returned two entries. A mutation that
// widens the list-unavailable branch to catch every lookup error (turning
// this into a 503 runtime_unavailable) must fail this test.
func TestStopAgent_AmbiguousMatchAbortsWithoutStop(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
		{
			ContainerID: "container-A2",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop?projectId=project-A", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected exactly 500 for an ambiguous match, got %d (%s)", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
	}
	if resp.Error.Code != ErrCodeRuntimeError {
		t.Errorf("expected error code %q, got %q", ErrCodeRuntimeError, resp.Error.Code)
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s); an ambiguous match must abort before stopping", mgr.StopCalls())
	}
	if strings.Contains(w.Body.String(), "ambiguous") || strings.Contains(w.Body.String(), "container-A2") {
		t.Errorf("response body leaked the raw lookup error: %s", w.Body.String())
	}
}

// TestStopAgent_NoContainerIDIsNoOp: a matching agent record with no
// resolvable container id (no "scion.container.id" label, no ContainerID,
// no ID) has nothing to stop. LookupContainerID's "no container ID" result
// is classified as ErrAgentNotFound, so stopAgent must treat it like a
// genuine not-found: return 202 as an idempotent no-op rather than aborting
// with a 5xx, and must NOT call Stop. Mirrors
// TestRestartAgent_NoContainerIDProceedsWithStart.
func TestStopAgent_NoContainerIDIsNoOp(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			Name:   "coordinator",
			Labels: map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop?projectId=project-A", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s); a no-container agent has nothing to stop", mgr.StopCalls())
	}
}

// TestStopAgent_AuxiliaryListErrorAbortsWithout202 is a knock-on-effect
// regression test for ptone/scion#2176: before the fix, an auxiliary
// runtime's List failure inside LookupContainerID was folded into
// agentNotFoundError (ErrAgentNotFound), so projectScopedTarget
// treated it as "not found in this project" and stopAgent answered 202
// "Agent stopped (not found in project)" — a false "stopped" for what was
// actually a transient listing failure, not via a 404 but via a 202. Now
// that the failure surfaces as ErrAgentListUnavailable, projectScopedTarget's
// existing "surface anything that isn't ErrAgentNotFound" check (handlers.go)
// must reject it, and stopAgent must report 503 runtime_unavailable instead
// of a false-success 202, and must never call Stop.
func TestStopAgent_AuxiliaryListErrorAbortsWithout202(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &mockManager{listErr: fmt.Errorf("docker ps: connection refused")}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/stop?projectId=project-A", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (not the idempotent 202 no-op, nor a bare 500) when an auxiliary runtime's list fails, got %d (%s)", w.Code, w.Body.String())
	}
	if defaultMgr.StopCalls() != 0 || auxMgr.StopCalls() != 0 {
		t.Error("Stop must not be called when the auxiliary list failure aborts the lookup")
	}
	if strings.Contains(w.Body.String(), "docker ps: connection refused") {
		t.Errorf("response body must not leak the raw runtime error text: %s", w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
	}
	if resp.Error.Code != ErrCodeRuntimeUnavailable {
		t.Errorf("expected error code %q, got %q", ErrCodeRuntimeUnavailable, resp.Error.Code)
	}
	if want := agentLookupUnavailableMessage("coordinator", ""); resp.Error.Message != want {
		t.Errorf("expected message %q, got %q", want, resp.Error.Message)
	}
}

// TestExecCommand_ListUnavailableReturns503 is a regression test for
// ptone/scion#2165: when the container runtime itself fails to answer the
// agent lookup (LookupContainerID wraps that in ErrAgentListUnavailable),
// execCommand must report a retryable 503 rather than a terminal 404 — the
// two mean very different things to a caller deciding whether to retry.
func TestExecCommand_ListUnavailableReturns503(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.listErr = fmt.Errorf("docker ps failed: exit status 1")
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
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/exec?projectId=project-A", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the runtime listing is unavailable, got %d (%s)", w.Code, w.Body.String())
	}
	if execCalled {
		t.Error("exec must not run when the agent lookup itself failed")
	}
	if strings.Contains(w.Body.String(), "docker ps failed") {
		t.Errorf("response body must not leak the raw runtime error text: %s", w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
	}
	if resp.Error.Code != ErrCodeRuntimeUnavailable {
		t.Errorf("expected error code %q, got %q", ErrCodeRuntimeUnavailable, resp.Error.Code)
	}
	if want := agentLookupUnavailableMessage("coordinator", ""); resp.Error.Message != want {
		t.Errorf("expected message %q, got %q", want, resp.Error.Message)
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
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, error) { return "", nil },
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	body, _ := json.Marshal(map[string]any{"command": []string{"echo", "hi"}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/ghost/exec?projectId=project-A", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing agent, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestExecCommand_AmbiguousMatchReturns500 is a regression test for
// GoogleCloudPlatform/scion#2098: when LookupContainerID finds more than one
// distinct container matching the slug (uniqueAgentEntry's ambiguous case),
// that is a real lookup failure, not a "not found," so execCommand must
// return a 500 runtime_error and must NOT exec — a lookup that can't tell
// which container to target must not guess and run against one of them
// anyway. Mirrors TestStopAgent_AmbiguousMatchAbortsWithoutStop.
//
// The status and code are pinned exactly (500 runtime_error, not just "some
// 5xx") because an ambiguous match is NOT ErrAgentListUnavailable: the
// runtime answered fine, it just returned two entries. A mutation that widens
// the list-unavailable branch to catch every lookup error (turning this into
// a 503 runtime_unavailable) must fail this test.
func TestExecCommand_AmbiguousMatchReturns500(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
		{
			ContainerID: "container-A2",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
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
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/exec?projectId=project-A", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected exactly 500 for an ambiguous match, got %d (%s)", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
	}
	if resp.Error.Code != ErrCodeRuntimeError {
		t.Errorf("expected error code %q, got %q", ErrCodeRuntimeError, resp.Error.Code)
	}
	// The body must carry a fixed, generic message — never the underlying
	// lookup error's own text (which can name container IDs or a match
	// count): that detail is logged server-side only (see execCommand's
	// agentLifecycleLog.Warn call), consistent with AgentLookupUnavailable's
	// same no-error-text-in-body contract for the 503 path.
	const wantMessage = "Failed to execute command"
	if resp.Error.Message != wantMessage {
		t.Errorf("expected generic body message %q, got %q (leaking the lookup error's own text)", wantMessage, resp.Error.Message)
	}
	if execCalled {
		t.Error("exec must not run when the lookup found an ambiguous match")
	}
}

// TestRestartAgent_LookupErrorAbortsWithoutStart is a regression test for
// #1985: when resolving the project-scoped stop target during a restart
// fails for a reason other than genuine "not found" (here, the runtime
// listing itself errors, which lookupAgentTarget wraps as
// ErrAgentListUnavailable), restartAgent must abort with the exact 503
// runtime_unavailable response and must NOT call Start — otherwise a runtime
// hiccup during the lookup would leave a second container running alongside
// whatever the first lookup couldn't see. The status is pinned exactly
// (rather than just "some 5xx") so a mutation that removes or narrows the
// ErrAgentListUnavailable branch (falling back to a 500 runtime_error) is
// caught.
func TestRestartAgent_LookupErrorAbortsWithoutStart(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	mgr.listErr = fmt.Errorf("docker ps failed against https://10.0.0.5:2376 (namespace scion-prod)")
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart?projectId=project-A", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected exactly 503 when the runtime listing itself fails, got %d (%s)", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
	}
	if resp.Error.Code != ErrCodeRuntimeUnavailable {
		t.Errorf("expected error code %q, got %q", ErrCodeRuntimeUnavailable, resp.Error.Code)
	}
	if want := agentLookupUnavailableMessage("coordinator", ""); resp.Error.Message != want {
		t.Errorf("expected message %q, got %q", want, resp.Error.Message)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected %d for a runtime-listing failure, got %d (%s)", http.StatusServiceUnavailable, w.Code, w.Body.String())
	}
	if mgr.StartCalls() != 0 {
		t.Errorf("Start was called %d time(s); a lookup failure during restart must not start a second container", mgr.StartCalls())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s); a lookup failure must abort before stopping", mgr.StopCalls())
	}
	if strings.Contains(w.Body.String(), "10.0.0.5") || strings.Contains(w.Body.String(), "scion-prod") {
		t.Errorf("response body leaked the raw runtime-listing error: %s", w.Body.String())
	}
}

// TestRestartAgent_AuxiliaryListErrorAbortsWithoutStart is the restart
// counterpart of TestStopAgent_AuxiliaryListErrorAbortsWithout202: an
// auxiliary runtime's List failure inside LookupContainerID must abort the
// restart with a 503 runtime_unavailable, not proceed to Start as if the
// agent were simply absent from this project — otherwise a runtime hiccup
// during the stop-target lookup would leave a second container running.
func TestRestartAgent_AuxiliaryListErrorAbortsWithoutStart(t *testing.T) {
	defaultMgr := &filteringMockManager{}
	defaultMgr.agents = []api.AgentInfo{}

	auxMgr := &mockManager{listErr: fmt.Errorf("docker ps: connection refused")}

	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(DefaultServerConfig(), defaultMgr, rt)

	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart?projectId=project-A", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (not proceed-with-start) when an auxiliary runtime's list fails, got %d (%s)", w.Code, w.Body.String())
	}
	if defaultMgr.StartCalls() != 0 || auxMgr.StartCalls() != 0 {
		t.Error("Start must not be called when the auxiliary list failure aborts the lookup")
	}
	if defaultMgr.StopCalls() != 0 || auxMgr.StopCalls() != 0 {
		t.Error("Stop must not be called when the auxiliary list failure aborts the lookup")
	}
	if strings.Contains(w.Body.String(), "docker ps: connection refused") {
		t.Errorf("response body must not leak the raw runtime error text: %s", w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
	}
	if resp.Error.Code != ErrCodeRuntimeUnavailable {
		t.Errorf("expected error code %q, got %q", ErrCodeRuntimeUnavailable, resp.Error.Code)
	}
	if want := agentLookupUnavailableMessage("coordinator", ""); resp.Error.Message != want {
		t.Errorf("expected message %q, got %q", want, resp.Error.Message)
	}
}

// TestRestartAgent_NotFoundInProjectProceedsWithStart verifies the other side
// of the #1985 fix: a genuine "not found in this project" result (no lookup
// error, just no match) must keep the existing idempotent behavior — restart
// skips the stop and proceeds to start, exactly as it did before the fix.
func TestRestartAgent_NotFoundInProjectProceedsWithStart(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	srv := newTestServerWithManager(t, mgr)

	// project-B has no "coordinator" agent — a genuine not-found, not a lookup error.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart?projectId=project-B", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s); must not stop a same-slug agent in another project", mgr.StopCalls())
	}
	if mgr.StartCalls() != 1 {
		t.Errorf("expected Start to be called once, got %d", mgr.StartCalls())
	}
}

// TestRestartAgent_AmbiguousMatchAbortsWithoutStart is a regression test for
// #1985: when the project-scoped lookup finds more than one distinct
// container matching the slug (uniqueAgentEntry's ambiguous case), that is a
// real lookup failure, not a "not found," so restartAgent must abort with a
// 5xx and must NOT call Start — a lookup that can't tell which container to
// stop must not just start a second one anyway.
//
// The status and code are pinned exactly (500 runtime_error, not just "some
// 5xx") because an ambiguous match is NOT ErrAgentListUnavailable: the
// runtime answered fine, it just returned two entries. A mutation that
// widens the list-unavailable branch to catch every lookup error (turning
// this into a 503 runtime_unavailable) must fail this test.
func TestRestartAgent_AmbiguousMatchAbortsWithoutStart(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
		{
			ContainerID: "container-A2",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart?projectId=project-A", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected exactly 500 for an ambiguous match, got %d (%s)", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
	}
	if resp.Error.Code != ErrCodeRuntimeError {
		t.Errorf("expected error code %q, got %q", ErrCodeRuntimeError, resp.Error.Code)
	}
	if mgr.StartCalls() != 0 {
		t.Errorf("Start was called %d time(s); an ambiguous match must not start a second container", mgr.StartCalls())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s); an ambiguous match must abort before stopping", mgr.StopCalls())
	}
	if strings.Contains(w.Body.String(), "ambiguous") || strings.Contains(w.Body.String(), "container-A2") {
		t.Errorf("response body leaked the raw lookup error: %s", w.Body.String())
	}
}

// TestRestartAgent_NoContainerIDProceedsWithStart: a matching agent record
// with no resolvable container id (no "scion.container.id" label, no
// ContainerID, no ID — e.g. a malformed or partial runtime entry) has
// nothing addressable to stop. LookupContainerID's "no container ID" result
// is classified as ErrAgentNotFound, so restartAgent must treat it like a
// genuine not-found: skip the stop and proceed to start, rather than
// aborting with a 5xx.
func TestRestartAgent_NoContainerIDProceedsWithStart(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			Name:   "coordinator",
			Labels: map[string]string{"scion.name": "coordinator", "scion.project_id": "project-A"},
		},
	}
	srv := newTestServerWithManager(t, mgr)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/coordinator/restart?projectId=project-A", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("Stop was called %d time(s); a no-container agent has nothing to stop", mgr.StopCalls())
	}
	if mgr.StartCalls() != 1 {
		t.Errorf("expected Start to be called once, got %d", mgr.StartCalls())
	}
}
