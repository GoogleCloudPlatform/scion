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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// These tests pin that the create response, the start path without a
// project path, and the restart lookup follow the runtime an agent actually
// uses, not the broker's default runtime (ptone/scion#2633).

const (
	lifecycleK8sProfile   = "k8s"
	lifecycleBridge       = "http://host.docker.internal:9090"
	lifecycleHubEndpoint  = "http://localhost:8080"
	lifecycleBridgedHubEP = "http://host.docker.internal:8080"
)

// lifecycleFixture is a docker-default broker whose agents live in a project
// that is not the broker's working directory. That project's settings add a
// kubernetes profile (lifecycleK8sProfile); the broker's working-directory
// project only knows docker, so a runtime resolution that does not read the
// agent's own project resolves to docker.
type lifecycleFixture struct {
	srv         *Server
	defaultMgr  *filteringMockManager
	k8sRuntime  *runtime.MockRuntime
	projectPath string // the agent project's .scion directory

	mu        sync.Mutex
	k8sRunEnv []string
	k8sRuns   int
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	writeLifecycleProject(t, filepath.Join(wd, ".scion"), "schema_version: \"1\"\n"+
		"active_profile: local\n"+
		"profiles:\n"+
		"    local:\n"+
		"        runtime: docker\n"+
		"runtimes:\n"+
		"    docker:\n"+
		"        type: docker\n")

	projectPath := filepath.Join(t.TempDir(), "agent-project", ".scion")
	writeLifecycleProject(t, projectPath, "schema_version: \"1\"\n"+
		"active_profile: local\n"+
		"profiles:\n"+
		"    local:\n"+
		"        runtime: docker\n"+
		"    "+lifecycleK8sProfile+":\n"+
		"        runtime: kubernetes\n"+
		"runtimes:\n"+
		"    docker:\n"+
		"        type: docker\n"+
		"    kubernetes:\n"+
		"        type: kubernetes\n")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.StateDir = t.TempDir()
	cfg.ContainerHubEndpoint = lifecycleBridge
	// No ForceRuntime: resolution must read settings.

	f := &lifecycleFixture{
		defaultMgr:  &filteringMockManager{},
		projectPath: projectPath,
	}
	f.k8sRuntime = &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.k8sRuns++
			f.k8sRunEnv = rc.Env
			return "pod-id", nil
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	f.srv = New(cfg, f.defaultMgr, rt)
	// A real kubernetes runtime cannot be built without a cluster; hand out
	// the mock instead. resolveManagerForOpts still wraps it in a real
	// agent.Manager and registers it as an auxiliary runtime.
	f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profileFlag string) runtime.Runtime {
		return f.k8sRuntime
	}
	return f
}

func writeLifecycleProject(t *testing.T, dotScion, settings string) {
	t.Helper()
	if err := os.MkdirAll(dotScion, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"default", "claude"} {
		tplDir := filepath.Join(dotScion, "templates", name)
		if err := os.MkdirAll(tplDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness_config: "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		hcDir := filepath.Join(dotScion, "harness-configs", name)
		if err := os.MkdirAll(hcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: "+name+"\nimage: test-image:"+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// registerK8sAgents registers an auxiliary kubernetes runtime whose manager
// lists agents, as the broker does once it has dispatched to that runtime.
func (f *lifecycleFixture) registerK8sAgents(agents ...api.AgentInfo) *filteringMockManager {
	mgr := &filteringMockManager{}
	mgr.agents = agents
	f.srv.auxiliaryRuntimesMu.Lock()
	f.srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: f.k8sRuntime, Manager: mgr}
	f.srv.auxiliaryRuntimesMu.Unlock()
	return mgr
}

func (f *lifecycleFixture) k8sRun() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.k8sRuns, f.k8sRunEnv
}

// lifecycleAgent is a listed agent entry carrying the labels the broker's
// project-scoped lookup filters on (scion.name, and the project label when
// projectID is set). projectPath is the project's .scion directory, as a
// container records it.
func lifecycleAgent(name, projectPath, projectID string) api.AgentInfo {
	labels := map[string]string{"scion.name": name}
	if projectID != "" {
		labels[projectkeys.LabelProjectID] = projectID
	}
	return api.AgentInfo{ID: name, Name: name, ContainerID: "ctr-" + name, ProjectPath: projectPath, Labels: labels}
}

// newDockerProject writes a second agent project that only knows docker and
// returns its .scion directory.
func newDockerProject(t *testing.T) string {
	t.Helper()
	dotScion := filepath.Join(t.TempDir(), "docker-project", ".scion")
	writeLifecycleProject(t, dotScion, "schema_version: \"1\"\n"+
		"active_profile: local\n"+
		"profiles:\n"+
		"    local:\n"+
		"        runtime: docker\n"+
		"runtimes:\n"+
		"    docker:\n"+
		"        type: docker\n")
	return dotScion
}

func lifecyclePost(t *testing.T, srv *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestCreateAgentProvisionOnly_ReportsDispatchRuntime: on a docker-default
// broker, a provision-only create reports the runtime of the profile it was
// provisioned on, which the hub records as the agent's runtime.
func TestCreateAgentProvisionOnly_ReportsDispatchRuntime(t *testing.T) {
	for _, tc := range []struct {
		name        string
		profile     string
		wantRuntime string
	}{
		{name: "kubernetes profile", profile: lifecycleK8sProfile, wantRuntime: "kubernetes"},
		{name: "default profile", profile: "", wantRuntime: "docker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
				"name":          "prov-agent",
				"slug":          "prov-agent",
				"id":            "agent-uuid-prov",
				"projectPath":   f.projectPath,
				"provisionOnly": true,
				"config":        map[string]any{"template": "claude", "profile": tc.profile},
			})
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
			}
			var raw struct {
				Agent map[string]any `json:"agent"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got := raw.Agent["runtime"]; got != tc.wantRuntime {
				t.Errorf("response agent.runtime = %v, want %q", got, tc.wantRuntime)
			}
		})
	}
}

// TestStartAgent_NoProjectPathResolvesFromAgentProject: a start request that
// names no project recovers the project path from the agent's container
// before the start context is built, so the hub endpoint is resolved for the
// runtime the agent's saved profile names (kubernetes: no docker-bridge
// rewrite), and the agent starts on that runtime.
func TestStartAgent_NoProjectPathResolvesFromAgentProject(t *testing.T) {
	for _, tc := range []struct {
		name  string
		onK8s bool // the container is listed by the kubernetes runtime rather than the default
	}{
		{name: "listed on default runtime"},
		{name: "listed on kubernetes runtime", onK8s: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			const name = "start-agent"
			writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
			listed := lifecycleAgent(name, f.projectPath, "")
			if tc.onK8s {
				f.registerK8sAgents(listed)
			} else {
				f.defaultMgr.agents = []api.AgentInfo{listed}
			}

			w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
				"hubEndpoint": lifecycleHubEndpoint,
			})
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
			}
			runs, env := f.k8sRun()
			if runs != 1 {
				t.Fatalf("kubernetes runtime runs = %d, want 1", runs)
			}
			if f.defaultMgr.StartCalls() != 0 {
				t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
			}
			if !slices.Contains(env, "SCION_HUB_ENDPOINT="+lifecycleHubEndpoint) {
				t.Errorf("want SCION_HUB_ENDPOINT=%s (no docker-bridge rewrite), got env %v", lifecycleHubEndpoint, env)
			}
			if slices.Contains(env, "SCION_HUB_ENDPOINT="+lifecycleBridgedHubEP) {
				t.Errorf("hub endpoint was rewritten for docker: %v", env)
			}
		})
	}
}

// TestRestartAgent_FindsAgentOnNonDefaultRuntime: an agent whose container
// is on the kubernetes runtime of a docker-default broker is found there, so
// its project path and saved kubernetes profile are read and it is restarted
// on kubernetes, with the hub endpoint resolved for kubernetes.
func TestRestartAgent_FindsAgentOnNonDefaultRuntime(t *testing.T) {
	f := newLifecycleFixture(t)
	const name = "restart-agent"
	writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
	f.registerK8sAgents(lifecycleAgent(name, f.projectPath, ""))

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	runs, env := f.k8sRun()
	if runs != 1 {
		t.Fatalf("kubernetes runtime runs = %d, want 1", runs)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
	if !slices.Contains(env, "SCION_HUB_ENDPOINT="+lifecycleHubEndpoint) {
		t.Errorf("want SCION_HUB_ENDPOINT=%s (no docker-bridge rewrite), got env %v", lifecycleHubEndpoint, env)
	}
}

// TestStartAgent_AuxiliaryListErrorDoesNotFailStart: when the default
// runtime lists no match and an auxiliary runtime fails to list, a start
// without a project path proceeds on the default resolution instead of
// failing.
func TestStartAgent_AuxiliaryListErrorDoesNotFailStart(t *testing.T) {
	f := newLifecycleFixture(t)
	k8sMgr := f.registerK8sAgents()
	k8sMgr.listErr = errors.New("cluster unreachable")

	w := lifecyclePost(t, f.srv, "/api/v1/agents/plain-agent/start", map[string]any{})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if f.defaultMgr.StartCalls() != 1 {
		t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
	}
}

// TestStartAgent_DefaultListErrorFailsStart: a start without a project path
// fails as unavailable when the default runtime cannot list agents, rather
// than starting without the agent's project.
func TestStartAgent_DefaultListErrorFailsStart(t *testing.T) {
	f := newLifecycleFixture(t)
	f.registerK8sAgents()
	const rawListErr = "docker unavailable: dial unix /var/run/docker.sock"
	f.defaultMgr.listErr = errors.New(rawListErr)

	w := lifecyclePost(t, f.srv, "/api/v1/agents/plain-agent/start", map[string]any{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, rawListErr) || strings.Contains(body, "docker.sock") {
		t.Errorf("response body leaks the raw runtime list error: %s", body)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
}

// TestRestartAgent_NotFoundOnAnyRuntime: an agent no runtime lists still
// starts through the default resolution, and a not-found start error is
// still reported as 404.
func TestRestartAgent_NotFoundOnAnyRuntime(t *testing.T) {
	f := newLifecycleFixture(t)
	k8sMgr := f.registerK8sAgents()
	f.defaultMgr.startErr = errors.New("agent 'ghost' not found")

	w := lifecyclePost(t, f.srv, "/api/v1/agents/ghost/restart", map[string]any{})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if f.defaultMgr.StartCalls() != 1 {
		t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
	}
	if k8sMgr.StartCalls() != 0 || k8sMgr.stopCalls != 0 {
		t.Errorf("kubernetes runtime was used: start=%d stop=%d", k8sMgr.StartCalls(), k8sMgr.stopCalls)
	}
}

// dirEntries lists the names directly under dir (nil when it does not exist).
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestStartAgent_RecoveredProjectPathWritesNoProjectMarker: a start with a
// projectId but no project path or slug recovers the agent's .scion
// directory from its container and uses it for settings only. It does not
// initialize that directory as a hub-managed project root: no .scion marker
// is written inside it and no project-configs directory is created. The
// start still lands on the runtime of the agent's saved profile.
func TestStartAgent_RecoveredProjectPathWritesNoProjectMarker(t *testing.T) {
	f := newLifecycleFixture(t)
	const (
		name      = "marker-agent"
		projectID = "11111111-2222-3333-4444-555555555555"
	)
	writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
	f.registerK8sAgents(lifecycleAgent(name, f.projectPath, projectID))
	before := dirEntries(t, f.projectPath)

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start?projectId="+projectID, map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if runs, _ := f.k8sRun(); runs != 1 {
		t.Fatalf("kubernetes runtime runs = %d, want 1", runs)
	}
	if _, err := os.Lstat(filepath.Join(f.projectPath, ".scion")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a .scion entry was created inside the agent's .scion directory (err = %v)", err)
	}
	if after := dirEntries(t, f.projectPath); !slices.Equal(before, after) {
		t.Errorf("agent's .scion directory entries changed: before %v, after %v", before, after)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := dirEntries(t, filepath.Join(home, ".scion", "project-configs")); len(got) != 0 {
		t.Errorf("project-configs entries created: %v", got)
	}
}

// TestProjectScopedLookup_SameNameInTwoProjects: two agents share a name,
// one in a docker project listed on the default runtime and one in a
// kubernetes project listed on the kubernetes runtime. A start or restart
// scoped to the kubernetes agent's project uses that agent's project path
// and saved profile, and a restart stops it on the kubernetes runtime.
func TestProjectScopedLookup_SameNameInTwoProjects(t *testing.T) {
	const (
		name       = "twin-agent"
		dockerProj = "aaaaaaaa-0000-0000-0000-000000000001"
		k8sProj    = "bbbbbbbb-0000-0000-0000-000000000002"
	)
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			f := newLifecycleFixture(t)
			dockerPath := newDockerProject(t)
			writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
			f.defaultMgr.agents = []api.AgentInfo{lifecycleAgent(name, dockerPath, dockerProj)}
			k8sMgr := f.registerK8sAgents(lifecycleAgent(name, f.projectPath, k8sProj))

			w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/"+op+"?projectId="+k8sProj, map[string]any{
				"hubEndpoint": lifecycleHubEndpoint,
			})
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
			}
			runs, env := f.k8sRun()
			if runs != 1 {
				t.Fatalf("kubernetes runtime runs = %d, want 1", runs)
			}
			if f.defaultMgr.StartCalls() != 0 {
				t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
			}
			if !slices.Contains(env, "SCION_HUB_ENDPOINT="+lifecycleHubEndpoint) {
				t.Errorf("want SCION_HUB_ENDPOINT=%s (no docker-bridge rewrite), got env %v", lifecycleHubEndpoint, env)
			}
			if op == "restart" {
				if f.defaultMgr.stopCalls != 0 {
					t.Errorf("default runtime Stop calls = %d, want 0", f.defaultMgr.stopCalls)
				}
				if k8sMgr.stopCalls != 1 {
					t.Errorf("kubernetes runtime Stop calls = %d, want 1", k8sMgr.stopCalls)
				}
			}
		})
	}
}

// TestStartAgent_ProvidedProjectPathUsedAsIs: a start that names a project
// path uses it as given, even when the agent's listed container records a
// different project.
func TestStartAgent_ProvidedProjectPathUsedAsIs(t *testing.T) {
	f := newLifecycleFixture(t)
	const name = "pinned-agent"
	dockerPath := newDockerProject(t)
	writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
	f.defaultMgr.agents = []api.AgentInfo{lifecycleAgent(name, dockerPath, "")}

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
		"projectPath": f.projectPath,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if runs, _ := f.k8sRun(); runs != 1 {
		t.Fatalf("kubernetes runtime runs = %d, want 1", runs)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
}

// TestTryProvisionWorktree_RecoveredProjectPathNotUsedAsRoot: a project path
// recovered from the agent's container is a .scion directory, not a project
// root, so worktree-per-agent provisioning does not create a worktree base
// under it; the start falls back to clone-per-agent and the directory is
// left as it was.
func TestTryProvisionWorktree_RecoveredProjectPathNotUsedAsRoot(t *testing.T) {
	f := newLifecycleFixture(t)
	dotScion := newDockerProject(t)
	before := dirEntries(t, dotScion)

	opts := api.StartOptions{}
	provisioned, _, err := f.srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:                     "wt-agent",
		AgentID:                  "wt-agent-id",
		ProjectID:                "wt-project-id",
		ProjectPath:              dotScion,
		ProjectPathFromContainer: true,
		WorkspaceMode:            store.WorkspaceModeWorktreePerAgent,
		Config:                   &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: "file://" + filepath.Join(t.TempDir(), "no-such-repo")}},
		Operation:                opHTTPStart,
	}, &opts, map[string]string{}, "docker")
	if err != nil {
		t.Fatalf("tryProvisionWorktree: %v", err)
	}
	if provisioned {
		t.Fatal("worktree provisioned under a recovered .scion directory")
	}
	if after := dirEntries(t, dotScion); !slices.Equal(before, after) {
		t.Errorf(".scion directory entries changed: before %v, after %v", before, after)
	}
}

// TestStartAgent_RecoversPathFromEntryWithoutContainerID: a listed entry
// that matches but carries no container identifier still supplies the
// agent's project path, so the start runs on the runtime of the agent's
// saved profile.
func TestStartAgent_RecoversPathFromEntryWithoutContainerID(t *testing.T) {
	f := newLifecycleFixture(t)
	const name = "no-ctr-agent"
	writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
	listed := lifecycleAgent(name, f.projectPath, "")
	listed.ID = ""
	listed.ContainerID = ""
	f.registerK8sAgents(listed)

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if runs, _ := f.k8sRun(); runs != 1 {
		t.Fatalf("kubernetes runtime runs = %d, want 1", runs)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
}

// TestStartAgent_AmbiguousMatchRecoversNoPath: when two distinct containers
// of the same name and project are listed, neither is taken as the agent's
// project; the start proceeds with the default resolution instead of
// failing.
func TestStartAgent_AmbiguousMatchRecoversNoPath(t *testing.T) {
	f := newLifecycleFixture(t)
	const (
		name      = "dup-agent"
		projectID = "cccccccc-0000-0000-0000-000000000003"
	)
	// Were either entry's path recovered, its saved kubernetes profile would
	// send the start to kubernetes.
	writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
	first := lifecycleAgent(name, f.projectPath, projectID)
	second := lifecycleAgent(name, f.projectPath, projectID)
	second.ID = name + "-2"
	second.ContainerID = "ctr-" + name + "-2"
	f.defaultMgr.agents = []api.AgentInfo{first, second}

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start?projectId="+projectID, map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if f.defaultMgr.StartCalls() != 1 {
		t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
	}
	if runs, _ := f.k8sRun(); runs != 0 {
		t.Errorf("kubernetes runtime runs = %d, want 0", runs)
	}
}
