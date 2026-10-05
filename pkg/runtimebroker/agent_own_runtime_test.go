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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// ownListAll makes the resolved profile runtime list all namespaces
	// with "default" as its default namespace.
	ownListAll bool
	// wrapOwn, when set, wraps each resolved profile runtime.
	wrapOwn func(*runtime.KubernetesRuntime) runtime.Runtime

	logs syncBuffer
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logRecords returns the JSON log records whose message contains msg.
func (f *ownRTFixture) logRecords(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(f.logs.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if m, _ := rec["msg"].(string); strings.Contains(m, msg) {
			out = append(out, rec)
		}
	}
	return out
}

type ownRTOptions struct {
	savedProfile string
	withPod      bool
	// linked places the project outside the hub-managed projects
	// directory, as a linked project the broker reaches only through the
	// hub's project path hint.
	linked bool
	// podRunID labels the pod with this run ID.
	podRunID string
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
	return newOwnRTFixtureWith(t, ownRTOptions{savedProfile: savedProfile, withPod: withPod})
}

func newOwnRTFixtureWith(t *testing.T, opts ownRTOptions) *ownRTFixture {
	t.Helper()
	savedProfile, withPod := opts.savedProfile, opts.withPod
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
	if opts.linked {
		projectDir = filepath.Join(t.TempDir(), "linked-repo", ".scion")
	}
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
				Annotations: map[string]string{projectkeys.LabelProjectPath: projectDir},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if opts.podRunID != "" {
			pod.Labels[api.LabelRunID] = opts.podRunID
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
	f.srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&f.logs, nil))
	// The profile names its own kubeconfig context, as in the settings above.
	profileClient := k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), f.cs)
	profileClient.CurrentContext = "agents-ctx"
	f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profile string) runtime.Runtime {
		f.resolverCalls.Add(1)
		rt := runtime.NewKubernetesRuntime(profileClient)
		rt.DefaultNamespace = ownRTProfileNS
		if f.ownListAll {
			rt.DefaultNamespace = "default"
			rt.ListAllNamespaces = true
		}
		if f.wrapOwn != nil {
			return f.wrapOwn(rt)
		}
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

// A profile runtime that lists all namespaces finds the pod outside its own
// default namespace; the delete addresses the pod by its namespace and sends
// nothing to the default namespace, where it has no access.
func TestDeleteAgent_ProfileRuntimeListsAllNamespaces_DeletesByPodNamespace(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	f.ownListAll = true

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

// Stop through a profile runtime that lists all namespaces addresses the pod
// by its namespace and sends nothing to the default namespace.
func TestStopAgent_ProfileRuntimeListsAllNamespaces_StopsByPodNamespace(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	f.ownListAll = true

	w := f.do(t, http.MethodPost, "/api/v1/agents/"+ownRTAgent+"/stop?projectId="+ownRTProjectID+"&runtime=kubernetes")
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
// delete of what remains (the agent's files), not a failure. The best-effort
// search of the other runtimes meets a Forbidden default namespace, which is
// logged and ignored. The warning names what was checked.
func TestDeleteAgent_OwnNamespaceNotFound_FileOnlyDelete(t *testing.T) {
	f := newOwnRTFixture(t, "agents", false)

	w := f.do(t, http.MethodDelete, ownRTDeletePath+"&deleteFiles=true")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %s; want 204", w.Code, w.Body.String())
	}
	recs := f.logRecords(t, "no container found in the agent's own runtime")
	if len(recs) != 1 {
		t.Fatalf("got %d no-container warnings, want 1; logs:\n%s", len(recs), f.logs.String())
	}
	for k, want := range map[string]string{
		"agent_id": ownRTAgent, "project_id": ownRTProjectID, "profile": "agents",
		"runtime": "kubernetes", "namespace": ownRTProfileNS, "level": "WARN",
	} {
		if got, _ := recs[0][k].(string); got != want {
			t.Errorf("warning %s = %q, want %q", k, got, want)
		}
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

// The hub's project path hint adds a linked project only when that project's
// recorded identity is the requested project. A hint naming a directory of
// another project, or no project, is ignored and the hub-managed directory
// is used.
func TestKnownAgentProjectDir_HintOutsideKnownDirsIgnored(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)

	other := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(filepath.Join(other, "agents", ownRTAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(other, "99999999-0000-0000-0000-000000000000"); err != nil {
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

	// An agent whose files exist only under a hinted directory of another
	// project has no directory: the hint does not supply one.
	if err := os.MkdirAll(filepath.Join(other, "agents", "hint-only"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.knownAgentProjectDir("hint-only", ownRTProjectID, other); got != "" {
		t.Errorf("hint of another project: knownAgentProjectDir = %q, want empty", got)
	}

	// A hinted linked project that identifies as the requested project is
	// used.
	linked := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(filepath.Join(linked, "agents", "linked-only"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(linked, ownRTProjectID); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.knownAgentProjectDir("linked-only", ownRTProjectID, filepath.Dir(linked)); got != linked {
		t.Errorf("linked hint: knownAgentProjectDir = %q, want %q", got, linked)
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

// A linked project outside the hub-managed projects directory is found
// through the hub's project path hint after a broker restart, and its agent
// is deleted in the profile namespace.
func TestDeleteAgent_LinkedProject_AfterBrokerRestart(t *testing.T) {
	f := newOwnRTFixtureWith(t, ownRTOptions{savedProfile: "agents", withPod: true, linked: true})

	w := f.do(t, http.MethodDelete, ownRTDeletePath+"&projectPath="+filepath.Dir(f.projectDir))
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
}

// Attach looks the agent up through LookupAgent. After a broker restart it
// finds the agent in its profile namespace without listing the default
// namespace.
func TestLookupAgent_ProfileNamespace_DefaultNamespaceForbidden(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)

	res, err := f.srv.LookupAgent(context.Background(), ownRTAgent, ownRTProjectID)
	if err != nil {
		t.Fatalf("LookupAgent: %v", err)
	}
	if res.ContainerID != ownRTAgent || res.RuntimeName != "kubernetes" {
		t.Fatalf("LookupAgent = container %q runtime %q; want %q kubernetes", res.ContainerID, res.RuntimeName, ownRTAgent)
	}
	k, ok := res.Runtime.(*runtime.KubernetesRuntime)
	if !ok || k.DefaultNamespace != ownRTProfileNS {
		t.Fatalf("LookupAgent runtime = %#v; want the profile runtime in %s", res.Runtime, ownRTProfileNS)
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}

// GET of an agent whose own runtime holds no pod is a 404, and the default
// namespace is not listed.
func TestGetAgent_OwnRuntimeNoPod_NotFound(t *testing.T) {
	f := newOwnRTFixture(t, "agents", false)

	w := f.do(t, http.MethodGet, "/api/v1/agents/"+ownRTAgent+"?projectId="+ownRTProjectID+"&runtime=kubernetes")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get status = %d, body %s; want 404", w.Code, w.Body.String())
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}

// startRecordingRuntime is a Kubernetes runtime whose Run only records the
// start, so a restart can be followed without provisioning a pod.
type startRecordingRuntime struct {
	*runtime.KubernetesRuntime
	runs *atomic.Int32
}

func (r *startRecordingRuntime) Run(ctx context.Context, cfg runtime.RunConfig) (string, error) {
	r.runs.Add(1)
	return cfg.Name, nil
}

// Restart in the issue's setup (profile namespace, default namespace
// Forbidden, broker restarted) stops the pod in its namespace and starts the
// agent again in the same runtime, without listing the default namespace.
func TestRestartAgent_ProfileNamespace_AfterBrokerRestart(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	writeRestartTemplates(t, f.projectDir)
	var runs atomic.Int32
	f.wrapOwn = func(k *runtime.KubernetesRuntime) runtime.Runtime {
		return &startRecordingRuntime{KubernetesRuntime: k, runs: &runs}
	}

	w := f.do(t, http.MethodPost, "/api/v1/agents/"+ownRTAgent+"/restart?projectId="+ownRTProjectID+"&runtime=kubernetes")
	if w.Code >= 300 {
		t.Fatalf("restart status = %d, body %s; want success; logs:\n%s", w.Code, w.Body.String(), f.logs.String())
	}
	if f.podExists(t) {
		t.Fatal("old pod still present after restart")
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("starts in the profile runtime = %d, want 1", n)
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}

// writeRestartTemplates writes the template and harness config a restart's
// start provisions from.
func writeRestartTemplates(t *testing.T, projectDir string) {
	t.Helper()
	tplDir := filepath.Join(projectDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness_config: default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hcDir := filepath.Join(projectDir, "harness-configs", "default")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: default\nimage: test-image:default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// When the saved profile now selects another runtime that does not list the
// agent, restart still stops the container in the runtime it runs in (here
// the broker's default) before starting it in the new one.
func TestRestartAgent_OwnRuntimeNoMatch_StopsInPreviousRuntime(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForSavedProfileRemap(t, "test-agent-1", "other")
	mgr.agents[0].ContainerID = "old-container"
	if own := agentOwnRuntimeFrom(srv.ensureAgentOwnRuntime(context.Background(), "test-agent-1", "", "")); own == nil || own.rt.Name() != "other" {
		t.Fatalf("own runtime = %+v, want the saved profile's runtime", own)
	}
	var runs atomic.Int32
	remapRuntime.RunFunc = func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
		runs.Add(1)
		return "new-id", nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code >= 300 {
		t.Fatalf("restart status = %d, body %s; want success", w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.LastStopAgentID() != "old-container" {
		t.Fatalf("stop on the previous runtime: calls=%d target=%q; want 1 old-container", mgr.StopCalls(), mgr.LastStopAgentID())
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("starts in the new runtime = %d, want 1", n)
	}
}

// A delete with no recorded runtime type, for an agent whose saved profile
// now selects another runtime that does not list it, finds the container in
// the runtime it runs in and deletes it there.
func TestDeleteAgent_RuntimeTypeChanged_NoRecordedType(t *testing.T) {
	srv, mgr, _ := newTestServerForSavedProfileRemap(t, "test-agent-1", "other")
	mgr.agents[0].ContainerID = "old-container"
	if own := agentOwnRuntimeFrom(srv.ensureAgentOwnRuntime(context.Background(), "test-agent-1", "", "")); own == nil || own.rt.Name() != "other" {
		t.Fatalf("own runtime = %+v, want the saved profile's runtime", own)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/test-agent-1", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %s; want 204", w.Code, w.Body.String())
	}
	if mgr.lastDeleteContainerID != "old-container" {
		t.Fatalf("deleted container = %q, want old-container", mgr.lastDeleteContainerID)
	}
}

// Editing the profile's namespace between two requests resolves the profile
// again and replaces the memoised entry, keeping one entry per project dir
// and profile.
func TestEnsureAgentOwnRuntime_ProfileEditedResolvesAgain(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true)
	var namespaces []string
	inner := f.srv.resolveAuxiliaryRuntime
	f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profile string) runtime.Runtime {
		vs, _, err := config.LoadEffectiveSettings(projectPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}
		rc, _, err := vs.ResolveRuntime(profile)
		if err != nil {
			t.Fatalf("resolve runtime: %v", err)
		}
		namespaces = append(namespaces, rc.Namespace)
		rt := inner(projectPath, agentName, profile).(*runtime.KubernetesRuntime)
		rt.DefaultNamespace = rc.Namespace
		return rt
	}

	first := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, ""))
	again := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, ""))
	if first == nil || again != first {
		t.Fatalf("unchanged settings did not reuse the memoised entry: %p %p", first, again)
	}

	settingsPath := filepath.Join(f.projectDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "namespace: "+ownRTProfileNS, "namespace: scion-agents-2", 1)
	if edited == string(data) {
		t.Fatal("settings edit did not apply")
	}
	if err := os.WriteFile(settingsPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	second := agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), ownRTAgent, ownRTProjectID, ""))
	if second == nil || second == first {
		t.Fatalf("edited settings did not resolve a new entry: %p %p", first, second)
	}
	if ns := ownRuntimeNamespace(second.rt); ns != "scion-agents-2" {
		t.Fatalf("new entry namespace = %q, want scion-agents-2", ns)
	}
	if want := []string{ownRTProfileNS, "scion-agents-2"}; strings.Join(namespaces, ",") != strings.Join(want, ",") {
		t.Fatalf("resolved namespaces = %v, want %v", namespaces, want)
	}
	entries := 0
	f.srv.agentOwnRuntimes.Range(func(any, any) bool { entries++; return true })
	if entries != 1 {
		t.Fatalf("memo entries = %d, want 1", entries)
	}
}

// Restart of an agent whose pod is already gone, in the issue's setup: the
// best-effort search of the other runtimes meets the Forbidden default
// namespace, which is logged and skipped (no 503 lookup failure); the
// restart goes on to start the agent.
func TestRestartAgent_ProfileNamespace_PodGone_ProceedsToStart(t *testing.T) {
	f := newOwnRTFixture(t, "agents", false)

	w := f.do(t, http.MethodPost, "/api/v1/agents/"+ownRTAgent+"/restart?projectId="+ownRTProjectID+"&runtime=kubernetes")
	// The start that follows fails: with the pod gone, restart has no
	// project path for the agent (unchanged behaviour), so it answers 500,
	// not the 503 a failed lookup would give.
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "Failed to restart agent") {
		t.Fatalf("restart status = %d, body %s; want 500 from the start", w.Code, w.Body.String())
	}
	if recs := f.logRecords(t, "runtime list failed while searching the other runtimes"); len(recs) == 0 {
		t.Fatalf("no skipped-runtime warning; logs:\n%s", f.logs.String())
	}
	if recs := f.logRecords(t, "agent not found in project, proceeding with start"); len(recs) != 1 {
		t.Fatalf("restart did not proceed to start; logs:\n%s", f.logs.String())
	}
}

// A delete carrying a run ID resolves the pod in the agent's own runtime and
// applies the run filter there: another run's ID leaves the pod untouched
// (404), the pod's own run ID deletes it in its namespace. The default
// namespace is not queried either way.
func TestDeleteAgent_ProfileNamespace_RunIDFilter(t *testing.T) {
	f := newOwnRTFixtureWith(t, ownRTOptions{savedProfile: "agents", withPod: true, podRunID: "run-1"})

	w := f.do(t, http.MethodDelete, ownRTDeletePath+"&runId=run-2")
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete of another run: status = %d, body %s; want 404", w.Code, w.Body.String())
	}
	if !f.podExists(t) {
		t.Fatal("pod of another run was deleted")
	}

	w = f.do(t, http.MethodDelete, ownRTDeletePath+"&runId=run-1")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete of the pod's run: status = %d, body %s; want 204", w.Code, w.Body.String())
	}
	if f.podExists(t) {
		t.Fatal("pod still present in the profile namespace")
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}

// useSettingsNamespace makes the profile runtime resolver read the profile's
// namespace from the project settings, as runtime.GetRuntime does.
func (f *ownRTFixture) useSettingsNamespace(t *testing.T) {
	t.Helper()
	inner := f.srv.resolveAuxiliaryRuntime
	f.srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profile string) runtime.Runtime {
		vs, _, err := config.LoadEffectiveSettings(projectPath)
		if err != nil {
			t.Errorf("load settings: %v", err)
			return inner(projectPath, agentName, profile)
		}
		rc, _, err := vs.ResolveRuntime(profile)
		if err != nil {
			t.Errorf("resolve runtime: %v", err)
		}
		saved := f.wrapOwn
		f.wrapOwn = nil
		rt := inner(projectPath, agentName, profile).(*runtime.KubernetesRuntime)
		f.wrapOwn = saved
		rt.DefaultNamespace = rc.Namespace
		if f.wrapOwn != nil {
			return f.wrapOwn(rt)
		}
		return rt
	}
}

// setProfileNamespace edits the agents profile's namespace in settings.
func (f *ownRTFixture) setProfileNamespace(t *testing.T, ns string) {
	t.Helper()
	p := filepath.Join(f.projectDir, "settings.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "namespace: "+ownRTProfileNS, "namespace: "+ns, 1)
	if edited == string(data) {
		t.Fatal("settings edit did not apply")
	}
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
}

// registerOldRuntime registers a Kubernetes runtime for namespace ns as an
// auxiliary runtime, as a broker does for a profile's earlier settings.
func (f *ownRTFixture) registerOldRuntime(ns string) {
	client := k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), f.cs)
	client.CurrentContext = "old-ctx"
	rt := runtime.NewKubernetesRuntime(client)
	rt.DefaultNamespace = ns
	f.srv.auxiliaryRuntimesMu.Lock()
	f.srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rt)] = auxiliaryRuntime{Runtime: rt, Manager: agent.NewManager(rt)}
	f.srv.auxiliaryRuntimesMu.Unlock()
}

func (f *ownRTFixture) createPod(t *testing.T, ns string, labels, annotations map[string]string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: ownRTAgent, Namespace: ns, Labels: labels, Annotations: annotations},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if _, err := f.cs.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (f *ownRTFixture) podExistsIn(t *testing.T, ns string) bool {
	t.Helper()
	_, err := f.cs.CoreV1().Pods(ns).Get(context.Background(), ownRTAgent, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		t.Fatalf("get pod: %v", err)
	}
	return err == nil
}

// Restart after the profile's namespace changed: the agent's own runtime
// (the new namespace) holds no pod, the broker's default namespace cannot
// be listed, and the old pod runs in a third runtime (the old namespace).
// The search skips the runtime it cannot list, stops the old pod, and the
// agent is started once in the new namespace.
func TestRestartAgent_NamespaceChanged_SkipsUnlistableRuntime_StopsOldPod(t *testing.T) {
	f := newOwnRTFixture(t, "agents", true) // old pod in scion-agents
	writeRestartTemplates(t, f.projectDir)
	f.registerOldRuntime(ownRTProfileNS)
	f.setProfileNamespace(t, "scion-agents-2")
	var runs atomic.Int32
	f.wrapOwn = func(k *runtime.KubernetesRuntime) runtime.Runtime {
		return &startRecordingRuntime{KubernetesRuntime: k, runs: &runs}
	}
	f.useSettingsNamespace(t)

	w := f.do(t, http.MethodPost, "/api/v1/agents/"+ownRTAgent+"/restart?projectId="+ownRTProjectID+"&runtime=kubernetes")
	if w.Code >= 300 {
		t.Fatalf("restart status = %d, body %s; want success; logs:\n%s", w.Code, w.Body.String(), f.logs.String())
	}
	if f.podExistsIn(t, ownRTProfileNS) {
		t.Fatal("old pod still running beside the new one")
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("starts = %d, want 1", n)
	}
	if recs := f.logRecords(t, "runtime list failed while searching the other runtimes"); len(recs) == 0 {
		t.Fatalf("the unlistable runtime was not skipped with a warning; logs:\n%s", f.logs.String())
	}
}

// The restart search applies delete's legacy-container rule: a container
// with no project label is stopped only when its recorded project path
// identifies as the requested project.
func TestRestartAgent_LegacyContainerInOtherRuntime_PathIdentityChecked(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ownPath     bool
		wantStopped bool
	}{
		{name: "path of this project", ownPath: true, wantStopped: true},
		{name: "path of no project", ownPath: false, wantStopped: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOwnRTFixture(t, "agents", false)
			writeRestartTemplates(t, f.projectDir)
			const legacyNS = "scion-legacy"
			f.registerOldRuntime(legacyNS)
			path := filepath.Join(t.TempDir(), "elsewhere", ".scion")
			if tc.ownPath {
				path = f.projectDir
			}
			f.createPod(t, legacyNS,
				map[string]string{"scion.name": ownRTAgent, "scion.agent": "true"},
				map[string]string{projectkeys.LabelProjectPath: path})
			var runs atomic.Int32
			f.wrapOwn = func(k *runtime.KubernetesRuntime) runtime.Runtime {
				return &startRecordingRuntime{KubernetesRuntime: k, runs: &runs}
			}

			_ = f.do(t, http.MethodPost, "/api/v1/agents/"+ownRTAgent+"/restart?projectId="+ownRTProjectID+"&runtime=kubernetes")
			if stopped := !f.podExistsIn(t, legacyNS); stopped != tc.wantStopped {
				t.Fatalf("legacy pod stopped = %v, want %v; logs:\n%s", stopped, tc.wantStopped, f.logs.String())
			}
		})
	}
}

// Terminal attach passes the hub's projectPath hint, so an agent of a
// linked project outside the hub-managed directory is found in its profile
// namespace without listing the default namespace.
func TestAttachLookup_LinkedProject_UsesProjectPathHint(t *testing.T) {
	f := newOwnRTFixtureWith(t, ownRTOptions{savedProfile: "agents", withPod: true, linked: true})

	if _, err := f.srv.LookupAgent(context.Background(), ownRTAgent, ownRTProjectID); err == nil {
		t.Fatal("lookup without the hint found the agent; the test needs the hint to matter")
	}
	f.mu.Lock()
	f.nsRequests = map[string][]string{}
	f.mu.Unlock()

	ctx := withProjectPathHint(context.Background(), filepath.Dir(f.projectDir))
	res, err := f.srv.LookupAgent(ctx, ownRTAgent, ownRTProjectID)
	if err != nil {
		t.Fatalf("LookupAgent with hint: %v", err)
	}
	if k, ok := res.Runtime.(*runtime.KubernetesRuntime); !ok || k.DefaultNamespace != ownRTProfileNS {
		t.Fatalf("LookupAgent runtime = %#v; want the profile runtime in %s", res.Runtime, ownRTProfileNS)
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}

// The HTTP attach handler reads projectPath from the request and hands it to
// the lookup.
func TestHandleAgentAttach_PassesProjectPathHint(t *testing.T) {
	f := newOwnRTFixtureWith(t, ownRTOptions{savedProfile: "agents", withPod: true, linked: true})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+ownRTAgent+"/attach?projectId="+ownRTProjectID+"&projectPath="+filepath.Dir(f.projectDir), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	f.srv.handleAgentAttach(w, req)

	if n := f.resolverCalls.Load(); n != 1 {
		t.Fatalf("profile runtime resolved %d times, want 1 (hint not passed); status %d body %s", n, w.Code, w.Body.String())
	}
	if got := f.requests("default"); len(got) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", got)
	}
}
