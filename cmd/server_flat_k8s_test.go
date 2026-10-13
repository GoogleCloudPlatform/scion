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

//go:build !no_sqlite

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// Flat Kubernetes instances through the real runtime constructor and scope
// probe (amendment K1-K3), against controlled API server fixtures.

const k8sTestToken = "k8s-test-credential-0123456789"

// fakeKubeCluster is a test API server: version discovery plus the
// kube-system Namespace object, answered with status and UID.
type fakeKubeCluster struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	uid    string
	reads  int
}

func newFakeKubeCluster(t *testing.T, uid string) *fakeKubeCluster {
	t.Helper()
	c := &fakeKubeCluster{status: http.StatusOK, uid: uid}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_, _ = fmt.Fprint(w, `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`)
		case "/api/v1/namespaces/kube-system":
			c.mu.Lock()
			c.reads++
			status, uid := c.status, c.uid
			c.mu.Unlock()
			if status != http.StatusOK {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure",
					"reason": map[int]string{403: "Forbidden", 404: "NotFound"}[status], "code": status})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Namespace", "apiVersion": "v1",
				"metadata": map[string]any{"name": "kube-system", "uid": uid}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *fakeKubeCluster) set(status int, uid string) {
	c.mu.Lock()
	c.status, c.uid = status, uid
	c.mu.Unlock()
}

// writeTestKubeconfigFile writes a kubeconfig whose context alias is always
// "shared" (so selection is proven per file, not per alias).
func writeTestKubeconfigFile(t *testing.T, dir, name, server string) string {
	t.Helper()
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
users:
- name: u
  user:
    token: %s
contexts:
- name: shared
  context:
    cluster: c
    user: u
current-context: shared
`, server, k8sTestToken)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func k8sInstance(key, kubeconfig, namespace string) config.V1RuntimeBrokerInstanceConfig {
	return config.V1RuntimeBrokerInstanceConfig{Key: key, Name: key,
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "kubernetes", Context: "shared", Namespace: namespace, Kubeconfig: kubeconfig}}
}

type recordingFlatActivator struct {
	mu        sync.Mutex
	activated map[string]*brokeridentity.Identity
	runtimes  map[string]runtime.Runtime
	refused   map[string]error
}

func (a *recordingFlatActivator) Activate(_ context.Context, c brokerhost.Candidate) (*brokerhost.Activation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activated == nil {
		a.activated, a.runtimes = map[string]*brokeridentity.Identity{}, map[string]runtime.Runtime{}
	}
	a.activated[c.Instance.Key] = c.Identity
	a.runtimes[c.Instance.Key] = c.Runtime
	return &brokerhost.Activation{}, nil
}

func (a *recordingFlatActivator) Refused(inst config.V1RuntimeBrokerInstanceConfig, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refused == nil {
		a.refused = map[string]error{}
	}
	a.refused[inst.Key] = err
}

// prepareK8sHost runs the production runtime factory and scope probe for
// the instances through a host with a recording activator.
func prepareK8sHost(t *testing.T, globalDir string, insts ...config.V1RuntimeBrokerInstanceConfig) (*brokerhost.Host, *recordingFlatActivator) {
	t.Helper()
	act := &recordingFlatActivator{}
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir:  globalDir,
		Instances:  insts,
		Mode:       brokerhost.ModeRemote,
		NewRuntime: newFlatInstanceRuntime,
		ProbeScope: probeFlatInstanceScope,
		Activator:  act,
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = ic.Identity.RuntimeBrokerID
			cfg.StateDir = filepath.Join(globalDir, "state", ic.Identity.RuntimeBrokerID)
			cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
			return runtimebroker.New(cfg, ic.Manager, ic.Runtime), nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	return h, act
}

func statusOf(h *brokerhost.Host, key string) brokerhost.InstanceStatus {
	for _, st := range h.Status() {
		if st.Key == key {
			return st
		}
	}
	return brokerhost.InstanceStatus{}
}

func clearK8sEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"KUBECONFIG", "SCION_K8S_NAMESPACE", "POD_NAMESPACE"} {
		t.Setenv(k, "")
	}
}

func TestFlatKubernetes_DistinctKubeconfigsSelectDistinctClusters(t *testing.T) {
	clearK8sEnv(t)
	dir := t.TempDir()
	a := newFakeKubeCluster(t, "uid-cluster-a")
	b := newFakeKubeCluster(t, "uid-cluster-b")
	kcA := writeTestKubeconfigFile(t, dir, "a.kubeconfig", a.URL)
	kcB := writeTestKubeconfigFile(t, dir, "b.kubeconfig", b.URL)
	globalDir := t.TempDir()

	h, act := prepareK8sHost(t, globalDir, k8sInstance("k8s-a", kcA, "agents"), k8sInstance("k8s-b", kcB, "agents"))
	require.Equal(t, brokerhost.StateActive, statusOf(h, "k8s-a").State, statusOf(h, "k8s-a").Error)
	require.Equal(t, brokerhost.StateActive, statusOf(h, "k8s-b").State, statusOf(h, "k8s-b").Error)

	idA, idB := act.activated["k8s-a"], act.activated["k8s-b"]
	assert.Equal(t, "uid-cluster-a", readScope(t, globalDir, "k8s-a").Kubernetes.ClusterUID)
	assert.Equal(t, "uid-cluster-b", readScope(t, globalDir, "k8s-b").Kubernetes.ClusterUID)
	assert.NotEqual(t, idA.RuntimeBrokerID, idB.RuntimeBrokerID)
	assert.Equal(t, 1, a.reads)
	assert.Equal(t, 1, b.reads)

	// The probe's namespace is the runtime's namespace.
	rtA := act.runtimes["k8s-a"].(*runtime.KubernetesRuntime)
	assert.Equal(t, "agents", rtA.DefaultNamespace)
	assert.Equal(t, rtA.DefaultNamespace, readScope(t, globalDir, "k8s-a").Kubernetes.Namespace)

	// Neither the kubeconfig path nor the credential is in the identity
	// record.
	for _, key := range []string{"k8s-a", "k8s-b"} {
		data, err := os.ReadFile(filepath.Join(brokeridentity.InstanceDir(globalDir, key), brokeridentity.IdentityFileName))
		require.NoError(t, err)
		assert.NotContains(t, string(data), "kubeconfig")
		assert.NotContains(t, string(data), k8sTestToken)
	}
}

func readScope(t *testing.T, globalDir, key string) brokeridentity.ExecutionScope {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(brokeridentity.InstanceDir(globalDir, key), brokeridentity.IdentityFileName))
	require.NoError(t, err)
	var id brokeridentity.Identity
	require.NoError(t, json.Unmarshal(data, &id))
	return id.ExecutionScope
}

func TestFlatKubernetes_SameClusterAndNamespaceActivatesEveryInstance(t *testing.T) {
	clearK8sEnv(t)
	dir := t.TempDir()
	c := newFakeKubeCluster(t, "uid-shared")
	kc1 := writeTestKubeconfigFile(t, dir, "one.kubeconfig", c.URL)
	kc2 := writeTestKubeconfigFile(t, dir, "two.kubeconfig", c.URL)

	h, act := prepareK8sHost(t, t.TempDir(), k8sInstance("k8s-a", kc1, "agents"), k8sInstance("k8s-b", kc2, "agents"), k8sInstance("k8s-c", kc1, "other"))
	for _, k := range []string{"k8s-a", "k8s-b", "k8s-c"} {
		assert.Equal(t, brokerhost.StateActive, statusOf(h, k).State, k)
		assert.Contains(t, act.activated, k)
	}
	assert.NotEqual(t, act.activated["k8s-a"].RuntimeBrokerID, act.activated["k8s-b"].RuntimeBrokerID, "one identity per instance")
}

func TestFlatKubernetes_UnidentifiedScopeRefusesOnlyThatInstance(t *testing.T) {
	clearK8sEnv(t)
	for name, tc := range map[string]struct {
		status int
		uid    string
		want   string
	}{
		"forbidden": {http.StatusForbidden, "", "access denied reading Namespace kube-system; this flat Runtime Broker instance requires get permission on namespaces/kube-system"},
		"not found": {http.StatusNotFound, "", "the Namespace object kube-system does not exist"},
		"empty uid": {http.StatusOK, "", "Namespace kube-system has no UID"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bad := newFakeKubeCluster(t, "uid-bad")
			bad.set(tc.status, tc.uid)
			good := newFakeKubeCluster(t, "uid-good")
			kcBad := writeTestKubeconfigFile(t, dir, "bad.kubeconfig", bad.URL)
			kcGood := writeTestKubeconfigFile(t, dir, "good.kubeconfig", good.URL)
			globalDir := t.TempDir()

			h, act := prepareK8sHost(t, globalDir, k8sInstance("k8s-bad", kcBad, "agents"), k8sInstance("k8s-good", kcGood, "agents"))
			st := statusOf(h, "k8s-bad")
			assert.Equal(t, brokerhost.StateRefused, st.State)
			assert.Contains(t, st.Error, tc.want)
			assert.True(t, errors.Is(act.refused["k8s-bad"], brokeridentity.ErrExecutionScopeUnidentified))
			assert.NotContains(t, st.Error, k8sTestToken, "credentials never appear in diagnostics")
			assert.NotContains(t, act.activated, "k8s-bad", "not activated: no registration, heartbeat or control channel")
			_, err := os.Stat(filepath.Join(brokeridentity.InstanceDir(globalDir, "k8s-bad"), brokeridentity.IdentityFileName))
			assert.True(t, errors.Is(err, os.ErrNotExist), "no identity is minted for an unidentified scope")
			assert.Equal(t, brokerhost.StateActive, statusOf(h, "k8s-good").State, "a healthy sibling activates")
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		dir := t.TempDir()
		gone := newFakeKubeCluster(t, "uid")
		url := gone.URL
		gone.Close()
		kc := writeTestKubeconfigFile(t, dir, "gone.kubeconfig", url)
		h, act := prepareK8sHost(t, t.TempDir(), k8sInstance("k8s-gone", kc, "agents"))
		st := statusOf(h, "k8s-gone")
		assert.Equal(t, brokerhost.StateRefused, st.State)
		assert.True(t, errors.Is(act.refused["k8s-gone"], brokeridentity.ErrExecutionScopeUnidentified), "unreachable is an unidentified scope: %v", act.refused["k8s-gone"])
		assert.Contains(t, st.Error, "the API server is unreachable or the request failed")
		assert.NotContains(t, st.Error, k8sTestToken)
		assert.NotContains(t, act.activated, "k8s-gone")
	})
}

func TestFlatKubernetes_RestartReidentifiesAndRefusesRetarget(t *testing.T) {
	clearK8sEnv(t)
	dir := t.TempDir()
	c := newFakeKubeCluster(t, "uid-1")
	kc := writeTestKubeconfigFile(t, dir, "a.kubeconfig", c.URL)
	globalDir := t.TempDir()

	h, act := prepareK8sHost(t, globalDir, k8sInstance("k8s-a", kc, "agents"))
	require.Equal(t, brokerhost.StateActive, statusOf(h, "k8s-a").State)
	first := act.activated["k8s-a"]

	// A renamed credential file pointing at the same cluster and namespace
	// keeps the identity.
	moved := writeTestKubeconfigFile(t, dir, "renamed.kubeconfig", c.URL)
	h, act = prepareK8sHost(t, globalDir, k8sInstance("k8s-a", moved, "agents"))
	require.Equal(t, brokerhost.StateActive, statusOf(h, "k8s-a").State)
	assert.Equal(t, first.RuntimeBrokerID, act.activated["k8s-a"].RuntimeBrokerID)
	assert.Equal(t, 2, c.reads, "every start identifies the scope again")

	// The kube-system read now fails: the restart is refused.
	c.set(http.StatusForbidden, "")
	h, _ = prepareK8sHost(t, globalDir, k8sInstance("k8s-a", kc, "agents"))
	assert.Equal(t, brokerhost.StateRefused, statusOf(h, "k8s-a").State)

	// Another cluster behind the same configuration is a retarget: refused.
	c.set(http.StatusOK, "uid-2")
	h, act = prepareK8sHost(t, globalDir, k8sInstance("k8s-a", kc, "agents"))
	assert.Equal(t, brokerhost.StateRefused, statusOf(h, "k8s-a").State)
	assert.True(t, errors.Is(act.refused["k8s-a"], brokeridentity.ErrExecutionScopeChanged))

	// So is another namespace.
	c.set(http.StatusOK, "uid-1")
	h, act = prepareK8sHost(t, globalDir, k8sInstance("k8s-a", kc, "elsewhere"))
	assert.Equal(t, brokerhost.StateRefused, statusOf(h, "k8s-a").State)
	assert.True(t, errors.Is(act.refused["k8s-a"], brokeridentity.ErrExecutionScopeChanged))
}

func TestFlatKubernetes_ExplicitKubeconfigFailureHasNoFallback(t *testing.T) {
	clearK8sEnv(t)
	dir := t.TempDir()
	good := newFakeKubeCluster(t, "uid-good")
	// A valid default source is available through KUBECONFIG.
	t.Setenv("KUBECONFIG", writeTestKubeconfigFile(t, dir, "default.kubeconfig", good.URL))
	malformed := filepath.Join(dir, "malformed.kubeconfig")
	require.NoError(t, os.WriteFile(malformed, []byte("{not: yaml: ["), 0o600))
	unreadable := filepath.Join(dir, "unreadable.kubeconfig")
	require.NoError(t, os.WriteFile(unreadable, []byte("x"), 0o000))
	validFile := writeTestKubeconfigFile(t, dir, "valid.kubeconfig", good.URL)

	missingCtx := k8sInstance("k8s-ctx", validFile, "agents")
	missingCtx.RuntimeTarget.Context = "no-such-context"
	h, act := prepareK8sHost(t, t.TempDir(),
		k8sInstance("k8s-missing", filepath.Join(dir, "missing.kubeconfig"), "ns-1"),
		k8sInstance("k8s-malformed", malformed, "ns-2"),
		k8sInstance("k8s-unreadable", unreadable, "ns-3"),
		missingCtx,
		k8sInstance("k8s-default", "", "ns-5"),
	)
	for _, key := range []string{"k8s-missing", "k8s-malformed", "k8s-unreadable", "k8s-ctx"} {
		if key == "k8s-unreadable" && os.Geteuid() == 0 {
			continue // root reads a 0000 file
		}
		st := statusOf(h, key)
		assert.Equal(t, brokerhost.StateRefused, st.State, "%s must be refused, not served from another source", key)
		assert.NotContains(t, act.activated, key)
		assert.NotContains(t, st.Error, k8sTestToken)
	}
	// The omitted field uses the default loading rules (here KUBECONFIG).
	assert.Equal(t, brokerhost.StateActive, statusOf(h, "k8s-default").State, statusOf(h, "k8s-default").Error)
	assert.Equal(t, filepath.Join(dir, "default.kubeconfig"), os.Getenv("KUBECONFIG"), "the process environment is unchanged")
}

func TestFlatKubernetes_DefaultLoadingKeepsKubeconfigPathList(t *testing.T) {
	clearK8sEnv(t)
	dir := t.TempDir()
	c := newFakeKubeCluster(t, "uid-list")
	kc := writeTestKubeconfigFile(t, dir, "real.kubeconfig", c.URL)
	list := filepath.Join(dir, "absent.kubeconfig") + string(filepath.ListSeparator) + kc
	t.Setenv("KUBECONFIG", list)

	h, _ := prepareK8sHost(t, t.TempDir(), k8sInstance("k8s-list", "", "agents"))
	assert.Equal(t, brokerhost.StateActive, statusOf(h, "k8s-list").State, statusOf(h, "k8s-list").Error)
	assert.Equal(t, list, os.Getenv("KUBECONFIG"))
}

func TestFlatKubernetes_NamespaceFallbackChainUnchanged(t *testing.T) {
	clearK8sEnv(t)
	dir := t.TempDir()
	c := newFakeKubeCluster(t, "uid-ns")
	kc := writeTestKubeconfigFile(t, dir, "a.kubeconfig", c.URL)
	t.Setenv("SCION_K8S_NAMESPACE", "from-env")

	rt, err := newFlatInstanceRuntime(context.Background(), k8sInstance("k8s-a", kc, ""))
	require.NoError(t, err)
	assert.Equal(t, "from-env", rt.(*runtime.KubernetesRuntime).DefaultNamespace, "no explicit namespace: the runtime's chain")
	scope, err := probeFlatInstanceScope(context.Background(), k8sInstance("k8s-a", kc, ""), rt)
	require.NoError(t, err)
	assert.Equal(t, "from-env", scope.Kubernetes.Namespace)
	assert.True(t, strings.HasPrefix(scope.Kubernetes.APIServer, "http://127.0.0.1:"), "API server recorded (informational): %s", scope.Kubernetes.APIServer)

	rt, err = newFlatInstanceRuntime(context.Background(), k8sInstance("k8s-a", kc, "explicit"))
	require.NoError(t, err)
	assert.Equal(t, "explicit", rt.(*runtime.KubernetesRuntime).DefaultNamespace, "an explicit namespace wins")
}

// TestFlatKubernetes_RegistrationCarriesNoKubeconfig: registering a
// Kubernetes instance sends neither its kubeconfig path nor credentials.
func TestFlatKubernetes_RegistrationCarriesNoKubeconfig(t *testing.T) {
	hub := newRegistrationHub(t)
	globalDir := setupRegisterInstanceTest(t, hub.URL)
	clearK8sEnv(t)
	cluster := newFakeKubeCluster(t, "uid-reg")
	kc := writeTestKubeconfigFile(t, t.TempDir(), "reg.kubeconfig", cluster.URL)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(fmt.Sprintf(
		"schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n    instances:\n      - key: k8s-reg\n        name: example-k8s\n        runtime_target:\n          type: kubernetes\n          context: shared\n          namespace: agents\n          kubeconfig: %s\n", kc)), 0o644))
	flatScopeProber = probeFlatInstanceScope // the real probe against the fixture
	brokerRegisterInstance = "k8s-reg"

	require.NoError(t, runBrokerRegister(brokerRegisterCmd, nil))

	for path, body := range hub.bodies {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		assert.NotContains(t, string(data), kc, "%s carries the kubeconfig path", path)
		assert.NotContains(t, string(data), "kubeconfig", "%s", path)
		assert.NotContains(t, string(data), k8sTestToken, "%s carries a credential", path)
		assert.NotContains(t, string(data), cluster.URL, "%s carries the API server", path)
	}
	target := hub.bodies["/api/v1/brokers"]["runtimeTarget"].(map[string]any)
	assert.Equal(t, "kubernetes", target["type"])
}

// TestFlatKubernetes_ExplicitFileExecFailureRefusesOnlyThatInstance: an
// instance whose explicit kubeconfig's exec credential plugin fails is
// refused (no ADC substitution, even on GCE) while a healthy sibling
// activates.
func TestFlatKubernetes_ExplicitFileExecFailureRefusesOnlyThatInstance(t *testing.T) {
	clearK8sEnv(t)
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1") // on GCE: the legacy path would try ADC
	dir := t.TempDir()
	// The exec plugin is used for a TLS server (client-go sends exec
	// credentials only over TLS).
	bad := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(bad.Close)
	good := newFakeKubeCluster(t, "uid-good")
	kcBad := filepath.Join(dir, "exec.kubeconfig")
	require.NoError(t, os.WriteFile(kcBad, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
    insecure-skip-tls-verify: true
users:
- name: u
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: /bin/false
      interactiveMode: Never
contexts:
- name: shared
  context:
    cluster: c
    user: u
current-context: shared
`, bad.URL)), 0o600))
	kcGood := writeTestKubeconfigFile(t, dir, "good.kubeconfig", good.URL)

	h, act := prepareK8sHost(t, t.TempDir(), k8sInstance("k8s-exec", kcBad, "agents"), k8sInstance("k8s-good", kcGood, "agents"))
	st := statusOf(h, "k8s-exec")
	assert.Equal(t, brokerhost.StateRefused, st.State)
	assert.True(t, errors.Is(act.refused["k8s-exec"], brokeridentity.ErrExecutionScopeUnidentified), "%v", act.refused["k8s-exec"])
	assert.Contains(t, st.Error, "no other credential source is used for this kubeconfig")
	assert.NotContains(t, act.activated, "k8s-exec")
	assert.Equal(t, brokerhost.StateActive, statusOf(h, "k8s-good").State, "a healthy sibling activates")
}
