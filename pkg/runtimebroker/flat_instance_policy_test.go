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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Flat instance settings policy end to end
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC1, FC5): the
// instance's runtime_target shared-dir defaults travel from its strictly
// loaded entry, through the policy the server attaches to each request,
// to the shared-dir PVC the Kubernetes runtime creates on a fake API. The
// pod create is refused (errPodCaptured), so nothing runs.

// policyInstance is one Runtime Broker server (flat or legacy) backed by
// the real agent manager and Kubernetes runtime on its own fake API.
type policyInstance struct {
	srv *Server
	mgr *agent.AgentManager
	// projectRoot is a linked project's root (it holds a .scion directory),
	// the form a dispatch's project path takes; projectID is its Hub
	// project ID, which a flat instance records each start's ownership
	// under. Each instance has its own project, so concurrent starts on two
	// instances share no project directory.
	projectRoot string
	projectID   string
	mu          sync.Mutex
	pvcs        []*corev1.PersistentVolumeClaim
	pods        []*corev1.Pod
}

// writePolicyHome points HOME at a temp dir holding settings (the global
// settings.yaml), a harness config and a default template.
func writePolicyHome(t *testing.T, settings string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	global := filepath.Join(home, ".scion")
	for path, content := range map[string]string{
		filepath.Join(global, "harness-configs", "test-harness", "config.yaml"): "harness: generic\nuser: scion\nimage: file-default:latest\n",
		filepath.Join(global, "templates", "default", "scion-agent.json"):       `{"default_harness_config": "test-harness"}`,
		filepath.Join(global, "settings.yaml"):                                  settings,
	} {
		writeFlatFixtureFile(t, path, content)
	}
}

// newPolicyInstance builds the server the host would build for inst (the
// flat instance configuration is passed to New, as the host does), or a
// legacy Runtime Broker when inst is nil.
func newPolicyInstance(t *testing.T, inst *config.V1RuntimeBrokerInstanceConfig) *policyInstance {
	t.Helper()
	p := &policyInstance{projectRoot: filepath.Join(t.TempDir(), "project"), projectID: "pid-legacy"}
	if inst != nil {
		p.projectID = "pid-" + inst.Key
	}
	if err := os.MkdirAll(filepath.Join(p.projectRoot, config.DotScion), 0o755); err != nil {
		t.Fatal(err)
	}
	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		p.mu.Lock()
		p.pvcs = append(p.pvcs, a.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim).DeepCopy())
		p.mu.Unlock()
		return false, nil, nil
	})
	clientset.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		p.mu.Lock()
		p.pods = append(p.pods, a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy())
		p.mu.Unlock()
		return true, nil, errPodCaptured
	})
	client := k8s.NewTestClient(dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme()), clientset)
	rt := scionrt.NewKubernetesRuntime(client)
	if inst != nil && inst.RuntimeTarget != nil {
		// A flat instance's runtime is built from its own target, with
		// the constructor and configuration the host uses.
		var err error
		if rt, err = scionrt.NewKubernetesRuntimeFromClient(client, inst.RuntimeTarget.KubernetesRuntimeConfig()); err != nil {
			t.Fatal(err)
		}
	}
	rt.DefaultNamespace = "agents"
	p.mgr = agent.NewManager(rt).(*agent.AgentManager)

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.ForceRuntime = "kubernetes"
	if inst != nil {
		cfg.FlatInstance = &FlatInstanceConfig{
			Identity: &brokeridentity.Identity{
				InstanceKey: inst.Key, RuntimeBrokerID: "rb-" + inst.Key,
				RuntimeTarget: api.RuntimeTargetDescriptor{ID: "target-" + inst.Key, Type: "kubernetes"},
				ExecutionScope: brokeridentity.ExecutionScope{Type: "kubernetes",
					Kubernetes: &brokeridentity.KubernetesScope{ClusterUID: "uid-" + inst.Key, Namespace: "agents"}},
			},
			Instance: *inst,
		}
	}
	p.srv = New(cfg, p.mgr, rt, config.RuntimeBuildRecord{})
	return p
}

// start serves one create through the server's request middleware (where a
// flat instance attaches its policy) and starts the agent, with one shared
// dir, through the real agent manager on the request's context. The start
// carries the project ID and a per-agent ID, as a Hub dispatch does, so a
// flat instance can record its ownership.
func (p *policyInstance) start(name string) error {
	return p.startWith(name, nil)
}

// startWith is start with the agent's start options adjusted by edit (nil
// for none), for example to give the agent its own inline configuration.
func (p *policyInstance) startWith(name string, edit func(*api.StartOptions)) error {
	var startErr error
	handler := p.srv.profileResolutionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, sce := p.srv.buildStartContext(r.Context(), startContextInputs{
			Name: name, AgentID: "agent-id-" + name, ProjectID: p.projectID,
			ProjectPath: p.projectRoot, Operation: opCreate,
			Config: &CreateAgentConfig{}, HTTPRequest: r,
		})
		if sce != nil {
			startErr = sce
			return
		}
		opts := sc.Opts
		opts.BrokerMode, opts.NoAuth = true, true
		opts.SharedDirs = []api.SharedDir{{Name: "scratch"}}
		if edit != nil {
			edit(&opts)
		}
		if _, err := p.mgr.Start(r.Context(), opts); !errors.Is(err, errPodCaptured) {
			startErr = fmt.Errorf("start %s: want the captured pod create, got %v", name, err)
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil))
	return startErr
}

// onlyPVC returns the single shared-dir PVC the instance created.
func (p *policyInstance) onlyPVC(t *testing.T) *corev1.PersistentVolumeClaim {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pvcs) != 1 {
		t.Fatalf("want exactly one shared-dir PVC, got %d", len(p.pvcs))
	}
	return p.pvcs[0]
}

func pvcSize(pvc *corev1.PersistentVolumeClaim) string {
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	return q.String()
}

func pvcClass(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

func readPolicyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "config", "testdata", "profile_conversion", "flat_40gi", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestFlatPolicy_Flat40Gi: the flat_40gi example. Legacy settings give
// profile batch a 40Gi shared-dir default over its runtime entry's 20Gi;
// the flat instance that replaces it declares runtime_target.shared_dir_size
// 40Gi. Loaded strictly and served by the flat instance, the agent's
// shared-dir PVC is 40Gi, as the legacy profile's is (the runtime default
// would be 10Gi).
func TestFlatPolicy_Flat40Gi(t *testing.T) {
	writePolicyHome(t, readPolicyFixture(t, "input.yaml")+readPolicyFixture(t, "want_instances.yaml"))
	instances, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("want one instance, got %+v", instances)
	}

	flat := newPolicyInstance(t, &instances[0])
	if err := flat.start("flat-40gi"); err != nil {
		t.Fatal(err)
	}
	if got := pvcSize(flat.onlyPVC(t)); got != "40Gi" {
		t.Fatalf("flat instance shared-dir PVC is %s, want 40Gi", got)
	}

	// Control: the legacy Runtime Broker resolves the same settings
	// through the active profile and also gets 40Gi.
	legacy := newPolicyInstance(t, nil)
	if err := legacy.start("legacy-40gi"); err != nil {
		t.Fatal(err)
	}
	if got := pvcSize(legacy.onlyPVC(t)); got != "40Gi" {
		t.Fatalf("legacy profile shared-dir PVC is %s, want 40Gi", got)
	}
}

// TestFlatPolicy_TwoInstancesOneProcess: two flat Kubernetes instances in
// one process, built from one settings file whose active profile carries
// its own shared-dir defaults. Creates on the two run concurrently; each
// agent's PVC has its own instance's defaults, and neither has the active
// profile's. Run under -race.
func TestFlatPolicy_TwoInstancesOneProcess(t *testing.T) {
	writePolicyHome(t, `schema_version: "1"
active_profile: batch
runtimes:
  cluster:
    type: kubernetes
    shared_dir_storage_class: profile-rwx
    shared_dir_size: 30Gi
profiles:
  batch:
    runtime: cluster
server:
  broker:
    instances:
      - key: k8s-a
        name: k8s-a
        runtime_target:
          type: kubernetes
          namespace: agents
          shared_dir_storage_class: a-rwx
          shared_dir_size: 40Gi
      - key: k8s-b
        name: k8s-b
        runtime_target:
          type: kubernetes
          namespace: agents
          shared_dir_size: 20Gi
`)
	instances, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("want two instances, got %+v", instances)
	}
	a := newPolicyInstance(t, &instances[0])
	b := newPolicyInstance(t, &instances[1])

	// The two instances serve concurrently; each one's own starts are
	// sequential (they share one project-scoped PVC, whose get-or-create
	// is not meant for concurrent first creates).
	const starts = 3
	errs := make(chan error, 2*starts)
	var wg sync.WaitGroup
	for name, inst := range map[string]*policyInstance{"a": a, "b": b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < starts; i++ {
				errs <- inst.start(fmt.Sprintf("agent-%s-%d", name, i))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// Shared-dir PVCs are project-scoped: each instance creates one for its
	// project and its later starts reuse it.
	if pvc := a.onlyPVC(t); pvcSize(pvc) != "40Gi" || pvcClass(pvc) != "a-rwx" {
		t.Errorf("instance k8s-a PVC: size %s class %q, want 40Gi a-rwx", pvcSize(pvc), pvcClass(pvc))
	}
	if pvc := b.onlyPVC(t); pvcSize(pvc) != "20Gi" || pvcClass(pvc) != "" {
		t.Errorf("instance k8s-b PVC: size %s class %q, want 20Gi and no class", pvcSize(pvc), pvcClass(pvc))
	}

	// Each server's own decisions read its own policy too.
	vs := &config.VersionedSettings{ActiveProfile: "batch"}
	for inst, want := range map[*policyInstance]string{a: "40Gi", b: "20Gi"} {
		if _, size := inst.srv.settingsView(vs).ResolveSharedDirDefaults(""); size != want {
			t.Errorf("settingsView size %q, want %q", size, want)
		}
	}
}

// A flat instance never serves a request without its policy, even when its
// flat configuration was set after the server was built.
func TestFlatPolicy_FlatServerAlwaysAttachesPolicy(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	var got *config.FlatInstancePolicy
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = config.FlatInstancePolicyFrom(r.Context())
	})
	f.srv.applyMiddleware(probe).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got == nil || got.InstanceKey != flatInstanceKey {
		t.Fatalf("attached policy %+v, want the instance's own (%s)", got, flatInstanceKey)
	}

	late := newTestServer(t)
	late.config.FlatInstance = f.srv.config.FlatInstance
	got = nil
	late.applyMiddleware(probe).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got == nil || got.InstanceKey != flatInstanceKey {
		t.Fatalf("a flat configuration set after New must still attach its policy, got %+v", got)
	}
}
