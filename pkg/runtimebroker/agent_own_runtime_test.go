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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	ownRTAgent     = "my-agent"
	ownRTProjectID = "11111111-2222-3333-4444-555555555555"
	ownRTProfileNS = "scion-agents"
)

// ownRTFixture is a broker whose default runtime is Kubernetes in the
// namespace "default", where every request is Forbidden, and a hub-managed
// project whose "agents" profile runs Kubernetes agents in scion-agents.
type ownRTFixture struct {
	srv        *Server
	cs         *k8sfake.Clientset
	projectDir string

	mu            sync.Mutex
	nsRequests    map[string][]string // namespace -> "verb resource"
	forbiddenNS   map[string]bool
	resolverCalls atomic.Int32
}

func (f *ownRTFixture) requests(ns string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.nsRequests[ns]...)
}

func (f *ownRTFixture) forbid(ns string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forbiddenNS[ns] = true
}

// newOwnRTFixture builds the fixture. savedProfile is written to the agent's
// agent-info.json ("" leaves it empty, as for an agent started without a
// profile). withPod creates the agent pod in scion-agents.
func newOwnRTFixture(t *testing.T, savedProfile string, withPod bool) *ownRTFixture {
	t.Helper()
	clearSCIONEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig"))

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	projectDir := filepath.Join(home, ".scion", "projects", "proj", ".scion")
	if err := os.MkdirAll(filepath.Join(projectDir, "agents", ownRTAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(projectDir, ownRTProjectID); err != nil {
		t.Fatal(err)
	}
	settings := "schema_version: \"1\"\n" +
		"active_profile: base\n" +
		"profiles:\n" +
		"    base:\n" +
		"        runtime: k8s-base\n" +
		"    agents:\n" +
		"        runtime: k8s-agents\n" +
		"runtimes:\n" +
		"    k8s-base:\n" +
		"        type: kubernetes\n" +
		"        namespace: default\n" +
		"    k8s-agents:\n" +
		"        type: kubernetes\n" +
		"        context: agents-ctx\n" +
		"        namespace: " + ownRTProfileNS + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	agentHome := config.GetAgentHomePath(projectDir, ownRTAgent)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(api.AgentInfo{Name: ownRTAgent, Profile: savedProfile})
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), info, 0o644); err != nil {
		t.Fatal(err)
	}

	f := &ownRTFixture{
		cs:          k8sfake.NewClientset(),
		projectDir:  projectDir,
		nsRequests:  map[string][]string{},
		forbiddenNS: map[string]bool{"default": true},
	}
	if withPod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ownRTAgent,
				Namespace: ownRTProfileNS,
				Labels: map[string]string{
					"scion.name":               ownRTAgent,
					"scion.agent":              "true",
					projectkeys.LabelProjectID: ownRTProjectID,
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if _, err := f.cs.CoreV1().Pods(ownRTProfileNS).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f.cs.PrependReactor("*", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		ns := action.GetNamespace()
		f.mu.Lock()
		f.nsRequests[ns] = append(f.nsRequests[ns], action.GetVerb()+" "+action.GetResource().Resource)
		forbidden := f.forbiddenNS[ns]
		f.mu.Unlock()
		if forbidden {
			return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: action.GetResource().Resource}, "", nil)
		}
		return false, nil, nil
	})

	client := k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), f.cs)
	defaultRT := runtime.NewKubernetesRuntime(client)
	defaultRT.DefaultNamespace = "default"

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	f.srv = New(cfg, agent.NewManager(defaultRT), defaultRT)
	f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profile string) runtime.Runtime {
		f.resolverCalls.Add(1)
		rt := runtime.NewKubernetesRuntime(client)
		rt.DefaultNamespace = ownRTProfileNS
		return rt
	}
	return f
}

func (f *ownRTFixture) do(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

func (f *ownRTFixture) podExists(t *testing.T) bool {
	t.Helper()
	_, err := f.cs.CoreV1().Pods(ownRTProfileNS).Get(context.Background(), ownRTAgent, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		t.Fatalf("get pod: %v", err)
	}
	return err == nil
}

const ownRTDeletePath = "/api/v1/agents/" + ownRTAgent + "?projectId=" + ownRTProjectID + "&runtime=kubernetes"

// A delete of an agent in a profile namespace, with the profile runtime
// already registered (as after a start on this broker process), deletes the
// pod in its namespace and sends nothing to the default namespace.
func TestDeleteAgent_ProfileNamespace_DefaultNamespaceForbidden(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	// Register the profile runtime the way a start does.
	if _, name := f.srv.resolveManagerForOpts(api.StartOptions{Name: ownRTAgent, ProjectPath: f.projectDir, Profile: "agents"}); name != "kubernetes" {
		t.Fatalf("profile runtime = %q, want kubernetes", name)
	}

	w := f.do(t, http.MethodDelete, ownRTDeletePath)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %s; want 204", w.Code, w.Body.String())
	}
	if f.podExists(t) {
		t.Fatal("pod still present in the profile namespace")
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}

// After a broker restart no profile runtime is registered. The agent's saved
// profile resolves and registers it, once, and the delete removes the pod in
// its namespace without querying the default namespace.
func TestDeleteAgent_ProfileNamespace_AfterBrokerRestart(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	if n := len(f.srv.sortedAuxiliaryRuntimes()); n != 0 {
		t.Fatalf("fresh server has %d auxiliary runtimes, want 0", n)
	}

	w := f.do(t, http.MethodDelete, ownRTDeletePath)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %s; want 204", w.Code, w.Body.String())
	}
	if f.podExists(t) {
		t.Fatal("pod still present in the profile namespace")
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
	if n := f.resolverCalls.Load(); n != 1 {
		t.Fatalf("profile runtime resolved %d times, want 1", n)
	}
	if n := len(f.srv.sortedAuxiliaryRuntimes()); n != 1 {
		t.Fatalf("auxiliary runtimes after delete = %d, want 1", n)
	}
}

// Stop and status after a restart also use the agent's own runtime.
func TestStopAndGetAgent_ProfileNamespace_AfterBrokerRestart(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)

	w := f.do(t, http.MethodGet, "/api/v1/agents/"+ownRTAgent+"?projectId="+ownRTProjectID+"&runtime=kubernetes")
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s; want 200", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/api/v1/agents/"+ownRTAgent+"/stop?projectId="+ownRTProjectID+"&runtime=kubernetes")
	if w.Code >= 300 {
		t.Fatalf("stop status = %d, body %s; want success", w.Code, w.Body.String())
	}
	if f.podExists(t) {
		t.Fatal("pod still present after stop")
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}

// An agent with no saved profile keeps the previous behaviour: every
// registered runtime is searched. With only the default runtime registered,
// its Forbidden list makes the target unknown (a runtime error), not a false
// success.
func TestDeleteAgent_LegacyAgentWithoutProfile_SearchesAllRuntimes(t *testing.T) {
	f := newOwnRTFixture(t, "", true)

	w := f.do(t, http.MethodDelete, ownRTDeletePath)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, body %s; want 500", w.Code, w.Body.String())
	}
	if got := f.requests("default"); len(got) == 0 {
		t.Fatal("legacy delete did not search the default runtime")
	}
	if !f.podExists(t) {
		t.Fatal("pod removed although the target was unknown")
	}

	// With the profile runtime registered, the walk finds the pod there and
	// the delete removes it in its own namespace.
	if _, name := f.srv.resolveManagerForOpts(api.StartOptions{Name: ownRTAgent, ProjectPath: f.projectDir, Profile: "agents"}); name != "kubernetes" {
		t.Fatalf("profile runtime = %q, want kubernetes", name)
	}
	w = f.do(t, http.MethodDelete, ownRTDeletePath)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %s; want 204", w.Code, w.Body.String())
	}
	if f.podExists(t) {
		t.Fatal("pod still present in the profile namespace")
	}
}

// Forbidden in the agent's own namespace is a real failure: the target is
// unknown (a runtime error, not a 404 the hub would take as done) and
// nothing is deleted.
func TestDeleteAgent_OwnNamespaceForbidden_TargetUnknown(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	f.forbid(ownRTProfileNS)

	w := f.do(t, http.MethodDelete, ownRTDeletePath)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, body %s; want 500", w.Code, w.Body.String())
	}
	f.mu.Lock()
	f.forbiddenNS[ownRTProfileNS] = false
	f.mu.Unlock()
	if !f.podExists(t) {
		t.Fatal("pod removed although its namespace could not be listed")
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}

// No pod in the agent's own namespace (listed without error) is a completed
// delete of what remains (the agent's files), not a failure, even though the
// default namespace is Forbidden.
func TestDeleteAgent_OwnNamespaceNotFound_FileOnlyDelete(t *testing.T) {
	f := newOwnRTFixture(t, "agents", false)

	w := f.do(t, http.MethodDelete, ownRTDeletePath+"&deleteFiles=true")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %s; want 204", w.Code, w.Body.String())
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.projectDir, "agents", ownRTAgent)); !os.IsNotExist(err) {
		t.Fatalf("agent files not deleted (stat err=%v)", err)
	}
}

// Two concurrent requests for one agent resolve its profile runtime once and
// register one auxiliary runtime.
func TestEnsureAgentOwnRuntime_ConcurrentRequestsRegisterOnce(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	inner := f.srv.resolveAuxiliaryRuntime
	f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profile string) runtime.Runtime {
		time.Sleep(50 * time.Millisecond) // hold the resolution open so the second request overlaps it
		return inner(projectPath, agentName, profile)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	owns := make([]*agentOwnRuntime, 2)
	for i := range owns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx := f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, "")
			owns[i] = agentOwnRuntimeFrom(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	if n := f.resolverCalls.Load(); n != 1 {
		t.Fatalf("profile runtime resolved %d times, want 1", n)
	}
	if n := len(f.srv.sortedAuxiliaryRuntimes()); n != 1 {
		t.Fatalf("auxiliary runtimes = %d, want 1", n)
	}
	if owns[0] == nil || owns[0] != owns[1] {
		t.Fatalf("requests did not share one own runtime: %p %p", owns[0], owns[1])
	}
}

// A failed resolution is not stored: the next request resolves again.
func TestEnsureAgentOwnRuntime_FailureNotCached(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	inner := f.srv.resolveAuxiliaryRuntime
	var calls atomic.Int32
	f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profile string) runtime.Runtime {
		if calls.Add(1) == 1 {
			return &runtime.MockRuntime{NameFunc: func() string { return "error" }}
		}
		return inner(projectPath, agentName, profile)
	}

	if own := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, "")); own != nil {
		t.Fatal("failed resolution produced an own runtime")
	}
	if own := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, "")); own == nil {
		t.Fatal("second request did not resolve the own runtime")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("resolver calls = %d, want 2", n)
	}
}

// The hub's project path hint only selects among project directories the
// broker already knows. A hint naming another directory, even one holding
// a same-named agent with the same project ID, is ignored.
func TestKnownAgentProjectDir_HintOutsideKnownDirsIgnored(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)

	other := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(filepath.Join(other, "agents", ownRTAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(other, ownRTProjectID); err != nil {
		t.Fatal(err)
	}

	for _, hint := range []string{"", other, filepath.Dir(other), "/nonexistent/path"} {
		if got := f.srv.knownAgentProjectDir(ownRTAgent, ownRTProjectID, hint); got != f.projectDir {
			t.Errorf("hint %q: knownAgentProjectDir = %q, want %q", hint, got, f.projectDir)
		}
	}
	if got := f.srv.knownAgentProjectDir(ownRTAgent, ownRTProjectID, f.projectDir); got != f.projectDir {
		t.Errorf("hint naming the known dir: got %q, want %q", got, f.projectDir)
	}

	// An agent whose files exist only under the hinted directory has no
	// known directory: the hint does not supply one.
	if err := os.MkdirAll(filepath.Join(other, "agents", "hint-only"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.knownAgentProjectDir("hint-only", ownRTProjectID, other); got != "" {
		t.Errorf("no known dir: knownAgentProjectDir = %q, want empty", got)
	}
}

// A docker agent whose saved profile resolves to the broker's default
// runtime is deleted by its unchanged container ID.
func TestDeleteAgent_DockerOwnRuntime_ContainerIDUnchanged(t *testing.T) {
	clearSCIONEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	origWd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	projectDir := filepath.Join(home, ".scion", "projects", "proj", ".scion")
	if err := os.MkdirAll(filepath.Join(projectDir, "agents", ownRTAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(projectDir, ownRTProjectID); err != nil {
		t.Fatal(err)
	}
	settings := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n    local:\n        runtime: docker\nruntimes:\n    docker:\n        type: docker\n"
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	agentHome := config.GetAgentHomePath(projectDir, ownRTAgent)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(api.AgentInfo{Name: ownRTAgent, Profile: "local"})
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), info, 0o644); err != nil {
		t.Fatal(err)
	}

	const containerID = "3f2a9c1d0b7e5a4f"
	mgr := &mockManager{agents: []api.AgentInfo{{
		Name: ownRTAgent, ContainerID: containerID, ProjectID: ownRTProjectID,
		Labels: map[string]string{projectkeys.LabelProjectID: ownRTProjectID},
	}}}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	srv := New(cfg, mgr, rt)
	srv.resolveAuxiliaryRuntime = func(string, string, string) runtime.Runtime {
		t.Fatal("docker profile matching the default runtime must not be resolved again")
		return nil
	}

	if own := agentOwnRuntimeFrom(srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, "")); own == nil || own.mgr != srv.manager {
		t.Fatalf("own runtime = %+v, want the default manager", own)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+ownRTAgent+"?projectId="+ownRTProjectID, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %s; want 204", w.Code, w.Body.String())
	}
	if mgr.lastDeleteContainerID != containerID {
		t.Fatalf("deleted container ID = %q, want %q", mgr.lastDeleteContainerID, containerID)
	}
}
