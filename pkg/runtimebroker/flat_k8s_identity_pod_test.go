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

// These tests drive a flat Kubernetes instance's production identity path
// end to end inside the process: the instance's start context
// (buildStartContext: its own mapping, block account and namespace), the
// agent manager's start (the RunConfig it builds from the start options),
// and the Kubernetes runtime's pod builder, against a fake Kubernetes API.
// The pod is captured when the runtime creates it; the fake API refuses the
// create, so nothing runs.
//
// Simulated boundaries: there is no cluster, no node, no Workload Identity
// and no GCP. The tests show the pod configuration the broker would submit
// (ServiceAccount, token automount, node selector, namespace, env). They
// are not evidence of cloud identity behaviour or an end-to-end proof.

// errPodCaptured ends a start at the pod create.
var errPodCaptured = errors.New("pod captured by the test")

// flatPodFixture is a flat Kubernetes instance (key k8s-a, namespace
// agents) backed by the real Kubernetes runtime on a fake API.
type flatPodFixture struct {
	srv        *Server
	mgr        *agent.AgentManager
	projectDir string
	mu         sync.Mutex
	pods       []*corev1.Pod
}

func newFlatPodFixture(t *testing.T, key string, target config.V1RuntimeTargetConfig) *flatPodFixture {
	t.Helper()
	return newPodFixture(t, key, target, true)
}

// newPodFixture is newFlatPodFixture, or with flat false the same server as
// a legacy Runtime Broker (no flat instance), which reads the global
// settings' identity policy.
func newPodFixture(t *testing.T, key string, target config.V1RuntimeTargetConfig, flat bool) *flatPodFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	global := filepath.Join(home, ".scion")
	for path, content := range map[string]string{
		filepath.Join(global, "harness-configs", "test-harness", "config.yaml"): "harness: generic\nuser: scion\nimage: file-default:latest\n",
		filepath.Join(global, "templates", "default", "scion-agent.json"):       `{"default_harness_config": "test-harness"}`,
		filepath.Join(global, "settings.yaml"):                                  globalPolicyThatMustNotApply,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &flatPodFixture{projectDir: filepath.Join(t.TempDir(), "project", ".scion")}
	if err := os.MkdirAll(f.projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		f.mu.Lock()
		f.pods = append(f.pods, a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy())
		f.mu.Unlock()
		return true, nil, errPodCaptured
	})
	rt := scionrt.NewKubernetesRuntime(k8s.NewTestClient(dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme()), clientset))
	target.Type = "kubernetes"
	if target.Namespace == "" {
		target.Namespace = "agents"
	}
	rt.DefaultNamespace = target.Namespace
	if !flat {
		// A legacy broker's namespace comes from its runtime entry, which
		// sets none here: the runtime default.
		rt.DefaultNamespace = "default"
	}
	f.mgr = agent.NewManager(rt).(*agent.AgentManager)

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.ForceRuntime = "kubernetes"
	f.srv = New(cfg, f.mgr, rt)
	if !flat {
		return f
	}
	f.srv.config.FlatInstance = &FlatInstanceConfig{
		Identity: &brokeridentity.Identity{
			InstanceKey: key, RuntimeBrokerID: "rb-" + key,
			RuntimeTarget: api.RuntimeTargetDescriptor{ID: "target-" + key, Type: "kubernetes"},
			ExecutionScope: brokeridentity.ExecutionScope{Type: "kubernetes",
				Kubernetes: &brokeridentity.KubernetesScope{ClusterUID: "uid-1", Namespace: target.Namespace}},
		},
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: key, Name: key, RuntimeTarget: &target},
	}
	f.srv.flatK8sIdentity = newFlatKubernetesIdentityPolicy(f.srv.config.FlatInstance)
	return f
}

// start builds the instance's start context for gcp and starts the agent
// through the real agent manager, returning the pod the runtime submitted.
func (f *flatPodFixture) start(t *testing.T, name string, gcp *GCPIdentityConfig, k8sCfg *api.KubernetesConfig) (*corev1.Pod, error) {
	t.Helper()
	sc, sce := f.srv.buildStartContext(context.Background(), startContextInputs{
		Name: name, ProjectPath: f.projectDir, Operation: opCreate,
		Config:      &CreateAgentConfig{GCPIdentity: gcp, Kubernetes: k8sCfg},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
	})
	if sce != nil {
		return nil, sce
	}
	opts := sc.Opts
	opts.BrokerMode, opts.NoAuth = true, true
	_, err := f.mgr.Start(context.Background(), opts)
	if !errors.Is(err, errPodCaptured) {
		t.Fatalf("start: want the captured pod create, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pods) == 0 {
		t.Fatal("no pod was submitted")
	}
	return f.pods[len(f.pods)-1], nil
}

func podEnv(pod *corev1.Pod) map[string]string {
	env := map[string]string{}
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
	}
	return env
}

func assertBlockPod(t *testing.T, pod *corev1.Pod, wantSA string) {
	t.Helper()
	if pod.Namespace != "agents" {
		t.Errorf("namespace %q, want the instance namespace agents", pod.Namespace)
	}
	if pod.Spec.ServiceAccountName != wantSA {
		t.Errorf("ServiceAccountName %q, want %q", pod.Spec.ServiceAccountName, wantSA)
	}
	if a := pod.Spec.AutomountServiceAccountToken; a == nil || *a {
		t.Errorf("automountServiceAccountToken = %v, want false", a)
	}
	if pod.Spec.NodeSelector[scionrt.KubernetesWorkloadIdentityNodeLabel] != "true" {
		t.Errorf("node selector %v lacks %s=true", pod.Spec.NodeSelector, scionrt.KubernetesWorkloadIdentityNodeLabel)
	}
	env := podEnv(pod)
	if env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("SCION_METADATA_MODE = %q, want block", env["SCION_METADATA_MODE"])
	}
	for _, k := range []string{"SCION_METADATA_SA_EMAIL", "SCION_METADATA_PROJECT_ID"} {
		if _, ok := env[k]; ok {
			t.Errorf("block pod carries %s", k)
		}
	}
}

// assertAssignPod is assertBlockPod for GCP identity mode assign: the pod
// runs as the mapped KSA (Workload Identity) in the instance namespace, so
// the mode is passthrough with the GSA and project as informational env, no
// metadata emulator redirect, and none of block's token or node-selector
// settings.
func assertAssignPod(t *testing.T, pod *corev1.Pod, wantKSA string) {
	t.Helper()
	if pod.Namespace != "agents" {
		t.Errorf("namespace %q, want the instance namespace agents", pod.Namespace)
	}
	if pod.Spec.ServiceAccountName != wantKSA {
		t.Errorf("ServiceAccountName %q, want %q", pod.Spec.ServiceAccountName, wantKSA)
	}
	if a := pod.Spec.AutomountServiceAccountToken; a != nil && !*a {
		t.Error("an assign pod must not get block's automountServiceAccountToken false")
	}
	if _, ok := pod.Spec.NodeSelector[scionrt.KubernetesWorkloadIdentityNodeLabel]; ok {
		t.Error("an assign pod must not get block's node selector")
	}
	env := podEnv(pod)
	for k, want := range map[string]string{
		"SCION_METADATA_MODE":       "passthrough",
		"SCION_METADATA_SA_EMAIL":   flatTestGSA,
		"SCION_METADATA_PROJECT_ID": "proj",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["GCE_METADATA_HOST"]; ok {
		t.Error("an assign pod must not be redirected to the metadata emulator")
	}
}

// TestFlatKubernetesPod_BlockWithInstanceServiceAccount: the submitted pod
// runs as the instance's block ServiceAccount in the instance namespace,
// with the token not mounted, the Workload Identity node selector and the
// block env. The global settings' block account never applies.
func TestFlatKubernetesPod_BlockWithInstanceServiceAccount(t *testing.T) {
	f := newFlatPodFixture(t, "k8s-a", config.V1RuntimeTargetConfig{KubernetesBlockServiceAccount: "instance-block"})
	pod, err := f.start(t, "pod-block", &GCPIdentityConfig{MetadataMode: "block"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertBlockPod(t, pod, "instance-block")
}

// TestFlatKubernetesPod_BlockOmittedUsesNamespaceDefault: with no instance
// block ServiceAccount the pod names none (the namespace's default), with
// the same token and node-selector restrictions.
func TestFlatKubernetesPod_BlockOmittedUsesNamespaceDefault(t *testing.T) {
	f := newFlatPodFixture(t, "k8s-a", config.V1RuntimeTargetConfig{})
	pod, err := f.start(t, "pod-block-default", &GCPIdentityConfig{MetadataMode: "block"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertBlockPod(t, pod, "")
}

// TestFlatKubernetesPod_AssignUsesInstanceMapping: the submitted pod runs
// as the KSA the instance maps the GSA to (not the global mapping), in the
// instance namespace, without the block token and node-selector settings.
func TestFlatKubernetesPod_AssignUsesInstanceMapping(t *testing.T) {
	f := newFlatPodFixture(t, "k8s-a", config.V1RuntimeTargetConfig{
		KubernetesServiceAccountMappings: map[string]string{flatTestGSA: "instance-ksa"}})
	pod, err := f.start(t, "pod-assign", &GCPIdentityConfig{MetadataMode: "assign", SAEmail: flatTestGSA, ProjectID: "proj"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertAssignPod(t, pod, "instance-ksa")
}

// TestFlatKubernetesPod_AssignRefusalsSubmitNoPod: an unmapped GSA, an
// explicit ServiceAccount other than the mapped one and an explicit
// namespace other than the instance's are refused before any pod.
func TestFlatKubernetesPod_AssignRefusalsSubmitNoPod(t *testing.T) {
	mapped := config.V1RuntimeTargetConfig{KubernetesServiceAccountMappings: map[string]string{flatTestGSA: "instance-ksa"}}
	assign := &GCPIdentityConfig{MetadataMode: "assign", SAEmail: flatTestGSA, ProjectID: "proj"}
	for name, tc := range map[string]struct {
		target   config.V1RuntimeTargetConfig
		k8s      *api.KubernetesConfig
		wantCode string // the refusal's code ("" when it has none)
		wantText string // a fragment of the refusal message
	}{
		"unmapped":           {config.V1RuntimeTargetConfig{}, nil, ErrCodeIdentityNotMapped, "this flat Runtime Broker instance's runtime_target"},
		"explicit KSA":       {mapped, &api.KubernetesConfig{ServiceAccountName: "other-ksa"}, ErrCodeIdentityKSAMismatch, "this instance's kubernetes_service_account_mappings"},
		"explicit namespace": {mapped, &api.KubernetesConfig{Namespace: "elsewhere"}, "", "this flat Runtime Broker instance's namespace \"agents\""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFlatPodFixture(t, "k8s-a", tc.target)
			_, err := f.start(t, "pod-refused", assign, tc.k8s)
			var sce *startContextError
			if !errors.As(err, &sce) {
				t.Fatalf("want a start context refusal before any pod, got %v", err)
			}
			if sce.Status != http.StatusBadRequest || sce.Code != tc.wantCode || !strings.Contains(sce.Message, tc.wantText) {
				t.Fatalf("refusal = %d %q %q, want 400 %q containing %q", sce.Status, sce.Code, sce.Message, tc.wantCode, tc.wantText)
			}
			if len(f.pods) != 0 {
				t.Fatalf("a pod was submitted: %+v", f.pods[0].Spec.ServiceAccountName)
			}
		})
	}
}

// TestFlatKubernetesPod_SiblingInstancesKeepTheirOwnIdentity: two instances
// on one cluster with different policies submit pods with their own
// ServiceAccounts.
func TestFlatKubernetesPod_SiblingInstancesKeepTheirOwnIdentity(t *testing.T) {
	for key, want := range map[string]string{"k8s-a": "ksa-a", "k8s-b": "ksa-b"} {
		f := newFlatPodFixture(t, key, config.V1RuntimeTargetConfig{
			KubernetesServiceAccountMappings: map[string]string{flatTestGSA: want}})
		pod, err := f.start(t, "pod-"+key, &GCPIdentityConfig{MetadataMode: "assign", SAEmail: flatTestGSA, ProjectID: "proj"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertAssignPod(t, pod, want)
	}
}

// TestFlatKubernetesPod_LegacyBrokerUnchanged: the same server without a
// flat instance keeps the legacy resolution: the global settings' mapping
// and block account apply (control for the contrast above).
func TestFlatKubernetesPod_LegacyBrokerUnchanged(t *testing.T) {
	f := newPodFixture(t, "legacy", config.V1RuntimeTargetConfig{}, false)
	pod, err := f.start(t, "pod-legacy-assign", &GCPIdentityConfig{MetadataMode: "assign", SAEmail: flatTestGSA, ProjectID: "proj"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.ServiceAccountName != "global-ksa" {
		t.Errorf("legacy assign ServiceAccountName %q, want the global mapping global-ksa", pod.Spec.ServiceAccountName)
	}
	pod, err = f.start(t, "pod-legacy-block", &GCPIdentityConfig{MetadataMode: "block"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.ServiceAccountName != "global-block" {
		t.Errorf("legacy block ServiceAccountName %q, want global-block", pod.Spec.ServiceAccountName)
	}
}
