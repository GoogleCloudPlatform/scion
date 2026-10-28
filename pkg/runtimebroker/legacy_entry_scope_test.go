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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// These tests pin that stop, restart, exec and attach (LookupAgent) resolve
// a runtime entry with no project identity by the rule delete uses
// (legacyEntryInProject): such an entry stands for an agent of the
// requested project only if it is a container whose recorded project path
// identifies as that project.

// legacyProjectPath returns a fresh .scion directory whose project-id file
// records projectID, for use as an entry's recorded project path.
func legacyProjectPath(t *testing.T, projectID string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "project", config.DotScion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(dir, projectID); err != nil {
		t.Fatal(err)
	}
	return dir
}

// execIDRecorder records the container IDs a runtime was asked to exec in.
type execIDRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *execIDRecorder) exec(_ context.Context, id string, _ []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return "", nil
}

func (r *execIDRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// legacyScopeServer is newTestServerWithManager with a default runtime
// that records every exec.
func legacyScopeServer(t *testing.T, mgr *filteringMockManager) (*Server, *execIDRecorder) {
	t.Helper()
	setupTestScionEnv(t)
	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	rec := &execIDRecorder{}
	srv := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "mock" }, ExecFunc: rec.exec}, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)
	return srv, rec
}

// twoProjectLegacyDev returns two unlabelled "dev" containers: cid-a whose
// recorded path identifies as scopeProjA and cid-b whose path identifies as
// scopeProjB.
func twoProjectLegacyDev(t *testing.T) []api.AgentInfo {
	t.Helper()
	return []api.AgentInfo{
		legacyEntry("dev", "cid-a", legacyProjectPath(t, scopeProjA)),
		legacyEntry("dev", "cid-b", legacyProjectPath(t, scopeProjB)),
	}
}

// otherProjectLegacyDev returns unlabelled "dev" containers none of which
// belongs to scopeProjB: one whose path identifies as scopeProjA, and one
// with no recorded path.
func otherProjectLegacyDev(t *testing.T) []api.AgentInfo {
	t.Helper()
	return []api.AgentInfo{
		legacyEntry("dev", "cid-a", legacyProjectPath(t, scopeProjA)),
		legacyEntry("dev", "cid-nopath", ""),
	}
}

func agentOp(t *testing.T, srv *Server, op, projectID, body string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/agents/dev/" + op + "?projectId=" + projectID
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// T1: same-named unlabelled containers in two projects. Each verb acts on
// the requested project's container only.

func TestStopAgent_UnlabelledSameNameInTwoProjects_StopsRequestedProject(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectLegacyDev(t)
	srv, _ := legacyScopeServer(t, mgr)

	w := agentOp(t, srv, "stop", scopeProjB, "")

	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.LastStopAgentID() != "cid-b" {
		t.Fatalf("Stop calls=%d target=%q, want 1 call on cid-b", mgr.StopCalls(), mgr.LastStopAgentID())
	}
}

func TestRestartAgent_UnlabelledSameNameInTwoProjects_StopsRequestedProject(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectLegacyDev(t)
	srv, _ := legacyScopeServer(t, mgr)

	w := agentOp(t, srv, "restart", scopeProjB, "")

	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.LastStopAgentID() != "cid-b" {
		t.Fatalf("Stop calls=%d target=%q, want 1 call on cid-b", mgr.StopCalls(), mgr.LastStopAgentID())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("Start calls=%d, want 1", mgr.StartCalls())
	}
}

func TestExecCommand_UnlabelledSameNameInTwoProjects_ExecsInRequestedProject(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectLegacyDev(t)
	srv, rec := legacyScopeServer(t, mgr)

	w := agentOp(t, srv, "exec", scopeProjB, `{"command":["echo","hi"]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got := rec.got(); len(got) != 1 || got[0] != "cid-b" {
		t.Fatalf("exec targets=%v, want [cid-b]", got)
	}
}

func TestLookupAgent_UnlabelledSameNameInTwoProjects_ResolvesRequestedProject(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = twoProjectLegacyDev(t)
	srv, _ := legacyScopeServer(t, mgr)

	res, err := srv.LookupAgent(context.Background(), "dev", scopeProjB)

	if err != nil {
		t.Fatalf("LookupAgent: %v, want cid-b", err)
	}
	if res.ContainerID != "cid-b" {
		t.Fatalf("LookupAgent container=%q, want cid-b", res.ContainerID)
	}
}

// T2: an unlabelled container whose path identifies as the requested
// project is still found, on the default runtime, on an auxiliary runtime,
// and in the agent's own runtime.

func TestLookupAgentMatch_UnlabelledInRequestedProject_FoundOnEveryStage(t *testing.T) {
	entry := func(t *testing.T) api.AgentInfo {
		return legacyEntry("dev", "cid-b", legacyProjectPath(t, scopeProjB))
	}
	cases := []struct {
		name  string
		setup func(t *testing.T) (*Server, context.Context)
	}{
		{"default runtime", func(t *testing.T) (*Server, context.Context) {
			mgr := &filteringMockManager{}
			mgr.agents = []api.AgentInfo{entry(t)}
			return New(DefaultServerConfig(), mgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{}), context.Background()
		}},
		{"auxiliary runtime", func(t *testing.T) (*Server, context.Context) {
			srv := New(DefaultServerConfig(), &filteringMockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
			auxMgr := &filteringMockManager{}
			auxMgr.agents = []api.AgentInfo{entry(t)}
			srv.auxiliaryRuntimesMu.Lock()
			srv.auxiliaryRuntimes["aux-a"] = auxiliaryRuntime{Runtime: &runtime.MockRuntime{NameFunc: func() string { return "aux-a" }}, Manager: auxMgr}
			srv.auxiliaryRuntimesMu.Unlock()
			return srv, context.Background()
		}},
		{"own runtime", func(t *testing.T) (*Server, context.Context) {
			srv := New(DefaultServerConfig(), &filteringMockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
			ownMgr := &filteringMockManager{}
			ownMgr.agents = []api.AgentInfo{entry(t)}
			own := &agentOwnRuntime{mgr: ownMgr, rt: &runtime.MockRuntime{NameFunc: func() string { return "own" }}}
			return srv, context.WithValue(context.Background(), agentOwnRuntimeKey{}, own)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ctx := tc.setup(t)
			m, err := srv.lookupAgentMatch(ctx, "dev", scopeProjB)
			if err != nil || m.containerID != "cid-b" {
				t.Fatalf("lookupAgentMatch = (%q, %v), want (cid-b, nil)", m.containerID, err)
			}
			res, err := srv.LookupAgent(ctx, "dev", scopeProjB)
			if err != nil || res.ContainerID != "cid-b" {
				t.Fatalf("LookupAgent = (%+v, %v), want cid-b", res, err)
			}
		})
	}
}

// T3: unlabelled containers that do not belong to the requested project
// (another project's path, or no path) are not acted on.

// A stop that finds nothing in the project is a no-op (202), as
// TestStopAgent_NotFoundInProjectIsNoOp pins; what matters is that no
// container is stopped.
func TestStopAgent_UnlabelledOnlyInOtherProject_NothingStopped(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = otherProjectLegacyDev(t)
	srv, _ := legacyScopeServer(t, mgr)

	w := agentOp(t, srv, "stop", scopeProjB, "")

	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Fatalf("Stop calls=%d target=%q, want none", mgr.StopCalls(), mgr.LastStopAgentID())
	}
}

func TestRestartAgent_UnlabelledOnlyInOtherProject_SkipsStop(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = otherProjectLegacyDev(t)
	srv, _ := legacyScopeServer(t, mgr)

	w := agentOp(t, srv, "restart", scopeProjB, "")

	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Fatalf("Stop calls=%d target=%q, want none", mgr.StopCalls(), mgr.LastStopAgentID())
	}
}

func TestExecCommand_UnlabelledOnlyInOtherProject_NotFound(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = otherProjectLegacyDev(t)
	srv, rec := legacyScopeServer(t, mgr)

	w := agentOp(t, srv, "exec", scopeProjB, `{"command":["echo","hi"]}`)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", w.Code, w.Body.String())
	}
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("exec targets=%v, want none", got)
	}
}

func TestLookupAgent_UnlabelledOnlyInOtherProject_NotFound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry func(t *testing.T) api.AgentInfo
	}{
		{"other project's path", func(t *testing.T) api.AgentInfo {
			return legacyEntry("dev", "cid-a", legacyProjectPath(t, scopeProjA))
		}},
		{"no path", func(t *testing.T) api.AgentInfo { return legacyEntry("dev", "cid-nopath", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			mgr.agents = []api.AgentInfo{tc.entry(t)}
			srv, _ := legacyScopeServer(t, mgr)

			res, err := srv.LookupAgent(context.Background(), "dev", scopeProjB)
			if err == nil {
				t.Fatalf("LookupAgent resolved %q, want not found", res.ContainerID)
			}
			if _, err := srv.lookupAgentMatch(context.Background(), "dev", scopeProjB); !errors.Is(err, ErrAgentNotFound) {
				t.Fatalf("lookupAgentMatch err=%v, want ErrAgentNotFound", err)
			}
		})
	}
}

// T4: an entry with no container ID is not accepted, even when its path
// identifies as the requested project.
func TestLookupAgent_UnlabelledWithoutContainerID_NotAccepted(t *testing.T) {
	entry := legacyEntry("dev", "", legacyProjectPath(t, scopeProjB))
	entry.ID = "id-only"
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{entry}
	srv, _ := legacyScopeServer(t, mgr)

	if target, _, _, err := srv.lookupAgentTarget(context.Background(), "dev", scopeProjB); target != "" || !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("lookupAgentTarget = (%q, %v), want (\"\", ErrAgentNotFound)", target, err)
	}
	if res, err := srv.LookupAgent(context.Background(), "dev", scopeProjB); err == nil {
		t.Fatalf("LookupAgent resolved %q, want not found", res.ContainerID)
	}
}

// T5: for the same entries, delete and stop pick the same container.
func TestDeleteAndStop_UnlabelledEntries_PickSameContainer(t *testing.T) {
	entries := func(t *testing.T) []api.AgentInfo {
		return append(twoProjectLegacyDev(t), legacyEntry("dev", "cid-nopath", ""))
	}

	stopMgr := &filteringMockManager{}
	stopMgr.agents = entries(t)
	stopSrv, _ := legacyScopeServer(t, stopMgr)
	if w := agentOp(t, stopSrv, "stop", scopeProjB, ""); w.Code != http.StatusAccepted {
		t.Fatalf("stop status=%d body=%s, want 202", w.Code, w.Body.String())
	}

	delMgr := &filteringMockManager{}
	delMgr.agents = entries(t)
	delSrv, _ := newScopeTestServer(t, delMgr)
	if w := doDelete(t, delSrv, "dev", "projectId="+scopeProjB); w.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s, want 204", w.Code, w.Body.String())
	}

	if stopMgr.LastStopAgentID() != "cid-b" || delMgr.LastDeleteContainerID() != "cid-b" {
		t.Fatalf("stop target=%q delete target=%q, want both cid-b", stopMgr.LastStopAgentID(), delMgr.LastDeleteContainerID())
	}
}

// Per-runtime entry shapes: an unlabelled entry resolves only when it is a
// container whose recorded path identifies as the project. Cloud Run
// entries record no project path, so only labelled ones resolve.
func TestStopAgent_LegacyEntryShapesPerRuntime(t *testing.T) {
	pathB := func(t *testing.T) string { return legacyProjectPath(t, scopeProjB) }
	pathA := func(t *testing.T) string { return legacyProjectPath(t, scopeProjA) }
	docker := func(path string) api.AgentInfo {
		return api.AgentInfo{Name: "dev", ContainerID: "cid-docker", ProjectPath: path,
			Labels: map[string]string{"scion.agent": "true", "scion.name": "dev", "scion.project_path": path}}
	}
	k8s := func(path string) api.AgentInfo {
		return api.AgentInfo{Name: "dev", ContainerID: "dev-pod", ProjectPath: path,
			Labels:      map[string]string{"scion.agent": "true", "scion.name": "dev"},
			Annotations: map[string]string{"scion.project_path": path},
			Kubernetes:  &api.AgentK8sMetadata{Namespace: "scion-ns", PodName: "dev-pod"}}
	}
	cloudRun := func(projectID string) api.AgentInfo {
		labels := map[string]string{"scion_agent": "true", "scion_name": "dev"}
		if projectID != "" {
			labels["scion_project_id"] = projectID
		}
		return api.AgentInfo{Name: "dev", ContainerID: "agent-dev-abc", ProjectID: projectID, Labels: labels}
	}
	cases := []struct {
		name  string
		entry func(t *testing.T) api.AgentInfo
		found bool
	}{
		{"docker in project", func(t *testing.T) api.AgentInfo { return docker(pathB(t)) }, true},
		{"docker other project", func(t *testing.T) api.AgentInfo { return docker(pathA(t)) }, false},
		{"docker no path", func(t *testing.T) api.AgentInfo { return docker("") }, false},
		{"kubernetes in project", func(t *testing.T) api.AgentInfo { return k8s(pathB(t)) }, true},
		{"kubernetes other project", func(t *testing.T) api.AgentInfo { return k8s(pathA(t)) }, false},
		{"cloud run labelled", func(*testing.T) api.AgentInfo { return cloudRun(scopeProjB) }, true},
		{"cloud run unlabelled", func(*testing.T) api.AgentInfo { return cloudRun("") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.entry(t)
			rt := &runtime.MockRuntime{
				NameFunc: func() string { return "docker" },
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{e}, nil
				},
			}
			mgr := agent.NewManager(rt)
			t.Cleanup(mgr.Close)
			srv := New(DefaultServerConfig(), mgr, rt, config.RuntimeBuildRecord{})

			m, err := srv.lookupAgentMatch(context.Background(), "dev", scopeProjB)
			if !tc.found {
				if !errors.Is(err, ErrAgentNotFound) {
					t.Fatalf("lookupAgentMatch = (%q, %v), want ErrAgentNotFound", m.containerID, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("lookupAgentMatch: %v, want a match", err)
			}
			if got := m.entry.ContainerID; got != e.ContainerID {
				t.Fatalf("matched container=%q, want %q", got, e.ContainerID)
			}
		})
	}
}

// Get, logs and has-prompt: with the other project's unlabelled entry
// listed first, each reads the requested project's entry, and an entry of
// another project alone is not found.

// readEntriesTwoProjects returns unlabelled "dev" containers of scopeProjA
// (listed first) and scopeProjB, each with its own agent.log, and a
// non-empty prompt.md for scopeProjB's only.
func readEntriesTwoProjects(t *testing.T) []api.AgentInfo {
	t.Helper()
	pathA, pathB := legacyProjectPath(t, scopeProjA), legacyProjectPath(t, scopeProjB)
	for path, text := range map[string]string{pathA: "log of A", pathB: "log of B"} {
		home := config.GetAgentHomePath(path, "dev")
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "agent.log"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	agentDirB := config.ResolveAgentDir(pathB, "dev")
	if err := os.MkdirAll(agentDirB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDirB, "prompt.md"), []byte("do the task"), 0o644); err != nil {
		t.Fatal(err)
	}
	return []api.AgentInfo{legacyEntry("dev", "cid-a", pathA), legacyEntry("dev", "cid-b", pathB)}
}

func doRead(srv *Server, op string, projectID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dev", nil)
	switch op {
	case "get":
		srv.getAgent(w, r, "dev", projectID)
	case "logs":
		srv.getLogs(w, r, "dev", projectID)
	case "has-prompt":
		srv.checkAgentPrompt(w, r, "dev", projectID)
	}
	return w
}

func TestGetAgent_UnlabelledSameNameInTwoProjects_ReturnsRequestedProject(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := legacyScopeServer(t, mgr) // sets HOME, which the agent home path depends on
	mgr.agents = readEntriesTwoProjects(t)

	w := doRead(srv, "get", scopeProjB)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"cid-b"`) {
		t.Fatalf("status=%d body=%s, want 200 with cid-b", w.Code, w.Body.String())
	}
}

func TestGetLogs_UnlabelledSameNameInTwoProjects_ReadsRequestedProject(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := legacyScopeServer(t, mgr) // sets HOME, which the agent home path depends on
	mgr.agents = readEntriesTwoProjects(t)

	w := doRead(srv, "logs", scopeProjB)

	if w.Code != http.StatusOK || w.Body.String() != "log of B" {
		t.Fatalf("status=%d body=%q, want 200 \"log of B\"", w.Code, w.Body.String())
	}
}

func TestCheckAgentPrompt_UnlabelledSameNameInTwoProjects_ReadsRequestedProject(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := legacyScopeServer(t, mgr) // sets HOME, which the agent home path depends on
	mgr.agents = readEntriesTwoProjects(t)

	w := doRead(srv, "has-prompt", scopeProjB)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"hasPrompt":true`) {
		t.Fatalf("status=%d body=%s, want 200 with hasPrompt true", w.Code, w.Body.String())
	}
}

func TestReadVerbs_UnlabelledOnlyInOtherProject_NotFound(t *testing.T) {
	for _, op := range []string{"get", "logs", "has-prompt"} {
		t.Run(op, func(t *testing.T) {
			mgr := &filteringMockManager{}
			srv, _ := legacyScopeServer(t, mgr)
			mgr.agents = readEntriesTwoProjects(t)[:1] // scopeProjA's only

			w := doRead(srv, op, scopeProjB)

			if w.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s, want 404", w.Code, w.Body.String())
			}
		})
	}
}

// Runtime selection for these verbs follows the same rule: the default
// runtime holds another project's unlabelled entry, an auxiliary runtime
// holds the requested project's.
func TestResolveAgentRuntimeTarget_UnlabelledOtherProjectOnDefault_PicksRequestedProjectsRuntime(t *testing.T) {
	defMgr := &filteringMockManager{}
	defMgr.agents = []api.AgentInfo{legacyEntry("dev", "cid-a", legacyProjectPath(t, scopeProjA))}
	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{legacyEntry("dev", "cid-b", legacyProjectPath(t, scopeProjB))}
	srv := New(DefaultServerConfig(), defMgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	auxRT := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRT, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	mgr, rt := srv.resolveAgentRuntimeTarget(context.Background(), "dev", scopeProjB)

	if mgr != auxMgr || rt != auxRT {
		t.Fatalf("resolveAgentRuntimeTarget picked %T/%v, want the auxiliary runtime holding the requested project's entry", mgr, rt)
	}
}
