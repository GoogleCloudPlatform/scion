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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	discoveryTestGSA       = "agent-worker@my-project.iam.gserviceaccount.com"
	discoveryTestNamespace = "agents-ns"
)

// discoveryGlobalSettingsNoMapping pins the namespace of the "kubernetes"
// runtime entry and maps no GSA, so assign falls back to discovery.
const discoveryGlobalSettingsNoMapping = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        namespace: agents-ns
`

// discoveryGlobalSettingsWithMapping is discoveryGlobalSettingsNoMapping
// plus an explicit mapping for the test GSA.
const discoveryGlobalSettingsWithMapping = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        namespace: agents-ns
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: mapped-ksa
`

const discoveryGlobalSettingsBlock = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        namespace: agents-ns
        kubernetes_block_service_account: block-ksa
`

func annotatedKSA(namespace, name, gsa string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Annotations: map[string]string{"iam.gke.io/gcp-service-account": gsa},
		},
	}
}

// discoveryRun is one buildStartContext run for a Kubernetes GCP identity
// dispatch, with a fake clientset behind ServiceAccount discovery.
type discoveryRun struct {
	mode        string
	globalYAML  string
	explicitKSA string
	objects     []k8sruntime.Object
	listErr     error
}

type discoveryResult struct {
	sc        *startContext
	sce       *startContextError
	err       error
	listCalls int
	hookCalls int
	logs      string
}

func (r discoveryRun) run(t *testing.T) discoveryResult {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.BrokerName = "broker-a"
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, r.globalYAML)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	client := fake.NewClientset(r.objects...)
	var res discoveryResult
	client.PrependReactor("list", "serviceaccounts", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.listCalls++
		if r.listErr != nil {
			return true, nil, r.listErr
		}
		return false, nil, nil
	})
	srv.assignKSAClientset = func(agent.Manager) (kubernetes.Interface, error) {
		res.hookCalls++
		return client, nil
	}

	mode := r.mode
	if mode == "" {
		mode = "assign"
	}
	createCfg := &CreateAgentConfig{
		GCPIdentity: &GCPIdentityConfig{MetadataMode: mode, SAEmail: discoveryTestGSA, ProjectID: "my-project"},
	}
	if r.explicitKSA != "" {
		createCfg.Kubernetes = &api.KubernetesConfig{ServiceAccountName: r.explicitKSA}
	}
	if mode == "block" {
		createCfg.GCPIdentity.SAEmail = ""
		createCfg.GCPIdentity.ProjectID = ""
	}
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-ksa-discovery",
		ProjectPath: projectDir,
		Config:      createCfg,
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	res.sc = sc
	res.err = err
	res.logs = logBuf.String()
	if err != nil {
		var sce *startContextError
		if errors.As(err, &sce) {
			res.sce = sce
		}
	}
	return res
}

func TestKSADiscovery_SingleAnnotatedKSAUsed(t *testing.T) {
	res := discoveryRun{
		globalYAML: discoveryGlobalSettingsNoMapping,
		objects: []k8sruntime.Object{
			annotatedKSA(discoveryTestNamespace, "worker-ksa", discoveryTestGSA),
			annotatedKSA(discoveryTestNamespace, "other-ksa", "other@my-project.iam.gserviceaccount.com"),
		},
	}.run(t)
	if res.err != nil {
		t.Fatalf("expected the annotated ServiceAccount to be discovered, got %v", res.err)
	}
	if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "worker-ksa" {
		t.Errorf("ResolvedKubernetesServiceAccountName = %q, want %q", got, "worker-ksa")
	}
	if got := res.sc.Opts.Env["SCION_METADATA_MODE"]; got != "passthrough" {
		t.Errorf("SCION_METADATA_MODE = %q, want passthrough", got)
	}
	if res.listCalls != 1 {
		t.Errorf("list calls = %d, want 1", res.listCalls)
	}
}

func TestKSADiscovery_ExplicitMappingWins(t *testing.T) {
	res := discoveryRun{
		globalYAML: discoveryGlobalSettingsWithMapping,
		objects:    []k8sruntime.Object{annotatedKSA(discoveryTestNamespace, "worker-ksa", discoveryTestGSA)},
	}.run(t)
	if res.err != nil {
		t.Fatalf("expected the mapped ServiceAccount to be accepted, got %v", res.err)
	}
	if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "mapped-ksa" {
		t.Errorf("ResolvedKubernetesServiceAccountName = %q, want the mapped %q", got, "mapped-ksa")
	}
	if res.hookCalls != 0 || res.listCalls != 0 {
		t.Errorf("discovery ran despite an explicit mapping: client lookups %d, list calls %d", res.hookCalls, res.listCalls)
	}
}

func TestKSADiscovery_TwoAnnotatedKSAsRefused(t *testing.T) {
	res := discoveryRun{
		globalYAML: discoveryGlobalSettingsNoMapping,
		objects: []k8sruntime.Object{
			annotatedKSA(discoveryTestNamespace, "worker-b", discoveryTestGSA),
			annotatedKSA(discoveryTestNamespace, "worker-a", strings.ToUpper(discoveryTestGSA)),
		},
	}.run(t)
	if res.sce == nil {
		t.Fatalf("expected a *startContextError, got %v", res.err)
	}
	if res.sce.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.sce.Status)
	}
	for _, want := range []string{"worker-a, worker-b", discoveryTestNamespace, "kubernetes_service_account_mappings"} {
		if !strings.Contains(res.sce.Message, want) {
			t.Errorf("message %q does not contain %q", res.sce.Message, want)
		}
	}
}

func TestKSADiscovery_NoMatchKeepsNotMapped(t *testing.T) {
	res := discoveryRun{
		globalYAML: discoveryGlobalSettingsNoMapping,
		objects:    []k8sruntime.Object{annotatedKSA(discoveryTestNamespace, "other-ksa", "other@my-project.iam.gserviceaccount.com")},
	}.run(t)
	assertNotMappedWith(t, res, api.BrokerKSADiscoveryNoMatch, "annotation discovery found no ServiceAccount in namespace \"agents-ns\"")
	if strings.Contains(res.logs, "error=") {
		t.Errorf("no-match log carries an error attribute:\n%s", res.logs)
	}
	if res.listCalls != 1 {
		t.Errorf("list calls = %d, want 1", res.listCalls)
	}
}

func TestKSADiscovery_ListErrorAddsRBACHint(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "", errors.New("RBAC: access denied"))
	res := discoveryRun{
		globalYAML: discoveryGlobalSettingsNoMapping,
		objects:    []k8sruntime.Object{annotatedKSA(discoveryTestNamespace, "worker-ksa", discoveryTestGSA)},
		listErr:    forbidden,
	}.run(t)
	assertNotMappedWith(t, res, api.BrokerKSADiscoveryListFailed, "forbidden")
	if !strings.Contains(res.logs, "RBAC: access denied") {
		t.Errorf("broker log lacks the list error:\n%s", res.logs)
	}
	if !strings.Contains(res.sce.Message, `discovery needs list (read-only) access to serviceaccounts in namespace "agents-ns"`) {
		t.Errorf("message lacks the RBAC hint: %q", res.sce.Message)
	}
}

func TestKSADiscovery_OtherNamespaceIgnored(t *testing.T) {
	res := discoveryRun{
		globalYAML: discoveryGlobalSettingsNoMapping,
		objects:    []k8sruntime.Object{annotatedKSA("other-ns", "worker-ksa", discoveryTestGSA)},
	}.run(t)
	assertNotMappedWith(t, res, api.BrokerKSADiscoveryNoMatch, "found no ServiceAccount in namespace \"agents-ns\"")
}

func TestKSADiscovery_ExplicitServiceAccountConflict(t *testing.T) {
	res := discoveryRun{
		globalYAML:  discoveryGlobalSettingsNoMapping,
		explicitKSA: "someone-else",
		objects:     []k8sruntime.Object{annotatedKSA(discoveryTestNamespace, "worker-ksa", discoveryTestGSA)},
	}.run(t)
	if res.sce == nil {
		t.Fatalf("expected a *startContextError, got %v", res.err)
	}
	if res.sce.Code != ErrCodeIdentityKSAMismatch {
		t.Errorf("code = %q, want %q", res.sce.Code, ErrCodeIdentityKSAMismatch)
	}
	if !strings.Contains(res.sce.Message, `explicit Kubernetes ServiceAccount "someone-else" does not match the ServiceAccount "worker-ksa"`) {
		t.Errorf("unexpected message: %q", res.sce.Message)
	}
	if got := res.sce.Details[api.BrokerErrDetailMappedKSA]; got != "worker-ksa" {
		t.Errorf("mapped KSA detail = %v, want worker-ksa", got)
	}
	if got := res.sce.Details[api.BrokerErrDetailKSASource]; got != api.BrokerKSASourceDiscovered {
		t.Errorf("KSA source detail = %v, want %q", got, api.BrokerKSASourceDiscovered)
	}

	// A matching explicit name is accepted.
	ok := discoveryRun{
		globalYAML:  discoveryGlobalSettingsNoMapping,
		explicitKSA: "worker-ksa",
		objects:     []k8sruntime.Object{annotatedKSA(discoveryTestNamespace, "worker-ksa", discoveryTestGSA)},
	}.run(t)
	if ok.err != nil {
		t.Fatalf("expected a matching explicit serviceAccountName to be accepted, got %v", ok.err)
	}
}

func TestKSADiscovery_BlockModeUnchanged(t *testing.T) {
	res := discoveryRun{
		mode:       "block",
		globalYAML: discoveryGlobalSettingsBlock,
		objects:    []k8sruntime.Object{annotatedKSA(discoveryTestNamespace, "worker-ksa", discoveryTestGSA)},
	}.run(t)
	if res.err != nil {
		t.Fatalf("expected block to be accepted, got %v", res.err)
	}
	if res.hookCalls != 0 || res.listCalls != 0 {
		t.Errorf("block mode ran discovery: client lookups %d, list calls %d", res.hookCalls, res.listCalls)
	}
	bi := res.sc.Opts.KubernetesBlockIdentity
	if bi == nil || bi.ServiceAccountName != "block-ksa" {
		t.Errorf("KubernetesBlockIdentity = %+v, want ServiceAccountName block-ksa", bi)
	}
	if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "" {
		t.Errorf("ResolvedKubernetesServiceAccountName = %q, want empty for block", got)
	}
}

func assertNotMappedWith(t *testing.T, res discoveryResult, result, reason string) {
	t.Helper()
	if res.sce == nil {
		t.Fatalf("expected a *startContextError, got %v", res.err)
	}
	if res.sce.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.sce.Status)
	}
	if res.sce.Code != ErrCodeIdentityNotMapped {
		t.Errorf("code = %q, want %q", res.sce.Code, ErrCodeIdentityNotMapped)
	}
	if !strings.Contains(res.sce.Message, `has no Kubernetes ServiceAccount mapped for "`+discoveryTestGSA+`"`) {
		t.Errorf("message lost the not-mapped text: %q", res.sce.Message)
	}
	if !strings.Contains(res.sce.Message, reason) {
		t.Errorf("message %q does not contain %q", res.sce.Message, reason)
	}
	if got := res.sce.Details[api.BrokerErrDetailDiscovery]; got != result {
		t.Errorf("discovery detail = %v, want %q", got, result)
	}
	if got := res.sce.Details[api.BrokerErrDetailNamespace]; got != discoveryTestNamespace {
		t.Errorf("namespace detail = %v, want %q", got, discoveryTestNamespace)
	}
	// The refusal is logged at Warn with the agent, account, namespace
	// and result.
	for _, want := range []string{"level=WARN", "agent=agent-ksa-discovery", "service_account=" + discoveryTestGSA, "namespace=" + discoveryTestNamespace, "discovery=" + result} {
		if !strings.Contains(res.logs, want) {
			t.Errorf("broker log lacks %q:\n%s", want, res.logs)
		}
	}
}

// A mapped name in a mismatch carries the mapped source.
func TestKSADiscovery_MappedMismatchSource(t *testing.T) {
	res := discoveryRun{
		globalYAML:  discoveryGlobalSettingsWithMapping,
		explicitKSA: "someone-else",
	}.run(t)
	if res.sce == nil || res.sce.Code != ErrCodeIdentityKSAMismatch {
		t.Fatalf("expected an identity_ksa_mismatch refusal, got %v", res.err)
	}
	if got := res.sce.Details[api.BrokerErrDetailKSASource]; got != api.BrokerKSASourceMapped {
		t.Errorf("KSA source detail = %v, want %q", got, api.BrokerKSASourceMapped)
	}
}

// A client that cannot be obtained keeps identity_not_mapped, with the
// unavailable result.
func TestKSADiscovery_ClientUnavailable(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, discoveryGlobalSettingsNoMapping)
	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-ksa-discovery",
		ProjectPath: projectDir,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: discoveryTestGSA},
		},
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	var sce *startContextError
	if !errors.As(err, &sce) || sce.Code != ErrCodeIdentityNotMapped {
		t.Fatalf("expected an identity_not_mapped refusal, got %v", err)
	}
	if got := sce.Details[api.BrokerErrDetailDiscovery]; got != api.BrokerKSADiscoveryUnavailable {
		t.Errorf("discovery detail = %v, want %q", got, api.BrokerKSADiscoveryUnavailable)
	}
}

// The placement check runs before discovery: a project override of the
// runtime entry's namespace or context, with no mapping, is refused with
// the placement error and never lists ServiceAccounts.
func TestKSADiscovery_PlacementCheckedBeforeDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		want     string
	}{
		{name: "namespace override", override: "namespace: ns-project", want: `resolves namespace "ns-project" in the project's settings but "ns-team"`},
		{name: "context override", override: "context: ctx-project", want: `sets context "ctx-project" in the project's settings but "ctx-team"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv, dotScion := newTestServerForStartContextMultiProfile(t, cfg, "kubernetes", "team", "kubernetes")
			projectSettingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: k8s-default
    team:
        runtime: k8s-team
runtimes:
    k8s-default:
        type: kubernetes
    k8s-team:
        type: kubernetes
        ` + tc.override + "\n"
			if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(projectSettingsYAML), 0644); err != nil {
				t.Fatal(err)
			}
			newTestGlobalSettings(t, `schema_version: "1"
runtimes:
    k8s-team:
        namespace: ns-team
        context: ctx-team
`)
			client := fake.NewClientset(annotatedKSA("ns-team", "worker-ksa", discoveryTestGSA), annotatedKSA("ns-project", "worker-ksa", discoveryTestGSA))
			var hookCalls, listCalls int
			client.PrependReactor("list", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				listCalls++
				return false, nil, nil
			})
			srv.assignKSAClientset = func(agent.Manager) (kubernetes.Interface, error) {
				hookCalls++
				return client, nil
			}

			_, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-ksa-discovery-placement",
				Config: &CreateAgentConfig{
					Profile:     "team",
					GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: discoveryTestGSA},
				},
				HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			var sce *startContextError
			if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest {
				t.Fatalf("expected a 400 startContextError, got %v", err)
			}
			if sce.Code == ErrCodeIdentityNotMapped {
				t.Errorf("got identity_not_mapped, want the placement error: %q", sce.Message)
			}
			if !strings.Contains(sce.Message, tc.want) {
				t.Errorf("message %q does not contain %q", sce.Message, tc.want)
			}
			if hookCalls != 0 || listCalls != 0 {
				t.Errorf("discovery ran before the placement check: client lookups %d, list calls %d", hookCalls, listCalls)
			}
		})
	}
}

// fakeMockManager is an agent.Manager that is not an *agent.AgentManager.
type fakeMockManager struct{ agent.Manager }

func TestAssignDiscoveryClientset(t *testing.T) {
	cs := fake.NewClientset()
	srv := &Server{}
	for _, tc := range []struct {
		name    string
		mgr     agent.Manager
		wantErr bool
	}{
		{name: "nil manager", mgr: nil, wantErr: true},
		{name: "non-Kubernetes runtime", mgr: &agent.AgentManager{Runtime: &scionrt.MockRuntime{}}, wantErr: true},
		{name: "nil runtime", mgr: &agent.AgentManager{}, wantErr: true},
		{name: "nil client", mgr: &agent.AgentManager{Runtime: &scionrt.KubernetesRuntime{}}, wantErr: true},
		{name: "typed-nil Kubernetes runtime", mgr: &agent.AgentManager{Runtime: (*scionrt.KubernetesRuntime)(nil)}, wantErr: true},
		{name: "nil clientset", mgr: &agent.AgentManager{Runtime: &scionrt.KubernetesRuntime{Client: &k8s.Client{}}}, wantErr: true},
		{name: "other manager type", mgr: fakeMockManager{}, wantErr: true},
		{name: "Kubernetes runtime", mgr: &agent.AgentManager{Runtime: &scionrt.KubernetesRuntime{Client: &k8s.Client{Clientset: cs}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := srv.assignDiscoveryClientset(tc.mgr)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected an error, got clientset %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != cs {
				t.Errorf("got clientset %v, want the runtime's", got)
			}
		})
	}

	// The broker's own default manager falls back to the default runtime.
	defaultMgr := fakeMockManager{}
	srvDefault := &Server{manager: &defaultMgr, runtime: &scionrt.KubernetesRuntime{Client: &k8s.Client{Clientset: cs}}}
	if got, err := srvDefault.assignDiscoveryClientset(&defaultMgr); err != nil || got != cs {
		t.Errorf("default manager: got %v, %v; want the default runtime's clientset", got, err)
	}
}
