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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const ksaCreateGlobalSettingsYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        namespace: agents-ns
        create_service_accounts: true
`

const ksaCreateGlobalSettingsWithMappingYAML = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        namespace: agents-ns
        create_service_accounts: true
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: mapped-ksa
`

type ksaCreateRun struct {
	globalYAML        string
	projectYAML       string
	gsa               string
	hubCreateKSA      bool
	ctxHubCreateKSA   bool
	explicitKSA       string
	explicitNamespace string
	objects           []k8sruntime.Object
	createReactor     func(k8stesting.Action) (bool, k8sruntime.Object, error)
	getReactor        func(k8stesting.Action) (bool, k8sruntime.Object, error)
}

type ksaCreateResult struct {
	sc          *startContext
	sce         *startContextError
	err         error
	client      *fake.Clientset
	listCalls   int
	createCalls int
	getCalls    int
	logs        string
}

func (r ksaCreateRun) run(t *testing.T) ksaCreateResult {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.BrokerName = "broker-a"
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")

	projectYAML := r.projectYAML
	if projectYAML == "" {
		projectYAML = testKubernetesProjectSettingsYAML
	}
	projectDir := newTestProjectSettings(t, projectYAML)
	newTestGlobalSettings(t, r.globalYAML)

	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	client := fake.NewClientset(r.objects...)
	var res ksaCreateResult
	res.client = client
	client.PrependReactor("list", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.listCalls++
		return false, nil, nil
	})
	client.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.createCalls++
		if r.createReactor != nil {
			return r.createReactor(a)
		}
		return false, nil, nil
	})
	client.PrependReactor("get", "serviceaccounts", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.getCalls++
		if r.getReactor != nil {
			return r.getReactor(a)
		}
		return false, nil, nil
	})
	srv.assignKSAClientset = func(agent.Manager) (kubernetes.Interface, error) {
		return client, nil
	}

	gsa := r.gsa
	if gsa == "" {
		gsa = discoveryTestGSA
	}
	createCfg := &CreateAgentConfig{
		GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: gsa, ProjectID: "my-project"},
	}
	if r.hubCreateKSA {
		createCfg.HubAgentDefaults = &api.HubAgentDefaults{CreateKSA: true}
	}
	if r.explicitKSA != "" || r.explicitNamespace != "" {
		createCfg.Kubernetes = &api.KubernetesConfig{
			ServiceAccountName: r.explicitKSA,
			Namespace:          r.explicitNamespace,
		}
	}

	ctx := context.Background()
	if r.ctxHubCreateKSA {
		ctx = withStartHubAgentDefaults(ctx, &api.HubAgentDefaults{CreateKSA: true})
	}
	sc, err := srv.buildStartContext(ctx, startContextInputs{
		Name:        "agent-ksa-create",
		ProjectPath: projectDir,
		Config:      createCfg,
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	res.sc = sc
	res.err = err
	errors.As(err, &res.sce)
	res.logs = logBuf.String()
	return res
}

func TestBuildStartContext_KubernetesAssignCreatesConventionalKSAOnMiss(t *testing.T) {
	res := ksaCreateRun{
		globalYAML:   ksaCreateGlobalSettingsYAML,
		hubCreateKSA: true,
	}.run(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker" {
		t.Fatalf("ResolvedKubernetesServiceAccountName = %q, want agent-worker", got)
	}
	if res.listCalls != 1 || res.createCalls != 1 {
		t.Errorf("listCalls=%d createCalls=%d, want 1, 1", res.listCalls, res.createCalls)
	}
	created, err := res.client.CoreV1().ServiceAccounts(discoveryTestNamespace).Get(context.Background(), "agent-worker", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get created KSA: %v", err)
	}
	if created.Annotations[k8s.WorkloadIdentityGSAAnnotation] != discoveryTestGSA {
		t.Errorf("GSA annotation = %q, want %q", created.Annotations[k8s.WorkloadIdentityGSAAnnotation], discoveryTestGSA)
	}
	if created.Annotations[k8s.CreatedForGSAAnnotation] != discoveryTestGSA {
		t.Errorf("created-for-gsa annotation = %q, want %q", created.Annotations[k8s.CreatedForGSAAnnotation], discoveryTestGSA)
	}
	if created.Labels[k8s.ManagedByLabel] != k8s.ManagedByScion {
		t.Errorf("managed-by label = %q, want %q", created.Labels[k8s.ManagedByLabel], k8s.ManagedByScion)
	}
	if created.AutomountServiceAccountToken == nil || *created.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want false", created.AutomountServiceAccountToken)
	}
}

// Start and restart attach HubAgentDefaults onto ctx via
// withStartHubAgentDefaults rather than CreateAgentConfig.HubAgentDefaults.
func TestBuildStartContext_KubernetesAssignCreateKSAFromContext(t *testing.T) {
	res := ksaCreateRun{
		globalYAML:      ksaCreateGlobalSettingsYAML,
		ctxHubCreateKSA: true,
	}.run(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker" {
		t.Fatalf("ResolvedKubernetesServiceAccountName = %q, want agent-worker", got)
	}
}

func TestBuildStartContext_KubernetesAssignCreatePrecedence(t *testing.T) {
	t.Run("explicit mapping wins without list or create", func(t *testing.T) {
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsWithMappingYAML,
			hubCreateKSA: true,
		}.run(t)
		if res.err != nil {
			t.Fatalf("unexpected error: %v", res.err)
		}
		if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "mapped-ksa" {
			t.Errorf("KSA = %q, want mapped-ksa", got)
		}
		if res.listCalls != 0 || res.createCalls != 0 {
			t.Errorf("listCalls=%d createCalls=%d, want 0, 0", res.listCalls, res.createCalls)
		}
	})

	t.Run("annotation discovery wins without create", func(t *testing.T) {
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
			objects:      []k8sruntime.Object{annotatedKSA(discoveryTestNamespace, "discovered-ksa", discoveryTestGSA)},
		}.run(t)
		if res.err != nil {
			t.Fatalf("unexpected error: %v", res.err)
		}
		if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "discovered-ksa" {
			t.Errorf("KSA = %q, want discovered-ksa", got)
		}
		if res.listCalls != 1 || res.createCalls != 0 {
			t.Errorf("listCalls=%d createCalls=%d, want 1, 0", res.listCalls, res.createCalls)
		}
	})

	t.Run("ambiguous discovery still refuses without create", func(t *testing.T) {
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
			objects: []k8sruntime.Object{
				annotatedKSA(discoveryTestNamespace, "ksa-1", discoveryTestGSA),
				annotatedKSA(discoveryTestNamespace, "ksa-2", discoveryTestGSA),
			},
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest {
			t.Fatalf("expected 400 ambiguity error, got %v", res.err)
		}
		if res.createCalls != 0 {
			t.Errorf("createCalls = %d, want 0", res.createCalls)
		}
	})
}

func TestBuildStartContext_KubernetesAssignCreateRequiresBothBrokerSettingAndHubFlag(t *testing.T) {
	for _, tc := range []struct {
		name         string
		globalYAML   string
		hubCreateKSA bool
	}{
		{
			name:         "broker setting off, hub flag on",
			globalYAML:   discoveryGlobalSettingsNoMapping,
			hubCreateKSA: true,
		},
		{
			name:         "broker setting on, hub flag off (experiment off or BYO GSA)",
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: false,
		},
		{
			name:         "both off",
			globalYAML:   discoveryGlobalSettingsNoMapping,
			hubCreateKSA: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := ksaCreateRun{
				globalYAML:   tc.globalYAML,
				hubCreateKSA: tc.hubCreateKSA,
			}.run(t)
			if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityNotMapped {
				t.Fatalf("expected 400 identity_not_mapped, got %+v (%v)", res.sce, res.err)
			}
			if res.createCalls != 0 {
				t.Errorf("createCalls = %d, want 0", res.createCalls)
			}
			if _, has := res.sce.Details[api.BrokerErrDetailMissingPermission]; has {
				t.Errorf("unexpected missingPermission detail: %+v", res.sce.Details)
			}
		})
	}
}

func TestBuildStartContext_KubernetesAssignProjectCreateServiceAccountsIgnoredWithWarning(t *testing.T) {
	const projectWithCreate = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
        namespace: agents-ns
        create_service_accounts: true
`
	res := ksaCreateRun{
		globalYAML:   discoveryGlobalSettingsNoMapping,
		projectYAML:  projectWithCreate,
		hubCreateKSA: true,
	}.run(t)
	if res.sce == nil || res.sce.Code != ErrCodeIdentityNotMapped {
		t.Fatalf("expected 400 identity_not_mapped when only project sets create_service_accounts, got %+v (%v)", res.sce, res.err)
	}
	if res.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", res.createCalls)
	}
	if !strings.Contains(res.logs, "create_service_accounts is set in a project's own settings.yaml") {
		t.Errorf("expected project settings warning in logs, got %q", res.logs)
	}
}

func TestBuildStartContext_KubernetesAssignAlreadyExistsRaceAndConflict(t *testing.T) {
	gr := schema.GroupResource{Resource: "serviceaccounts"}

	t.Run("concurrent scion create won race after discovery missed", func(t *testing.T) {
		winner := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   discoveryTestNamespace,
				Name:        "agent-worker",
				Labels:      map[string]string{k8s.ManagedByLabel: k8s.ManagedByScion},
				Annotations: map[string]string{k8s.WorkloadIdentityGSAAnnotation: discoveryTestGSA},
			},
		}
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
			createReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, apierrors.NewAlreadyExists(gr, "agent-worker")
			},
			getReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, winner.DeepCopy(), nil
			},
		}.run(t)
		if res.err != nil {
			t.Fatalf("unexpected error: %v", res.err)
		}
		if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "agent-worker" {
			t.Errorf("KSA = %q, want agent-worker", got)
		}
	})

	t.Run("unannotated existing KSA at conventional name returns identity_ksa_conflict without modifying it", func(t *testing.T) {
		unannotated := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: discoveryTestNamespace,
				Name:      "agent-worker",
				Labels:    map[string]string{"owner": "team-sec"},
			},
		}
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
			objects:      []k8sruntime.Object{unannotated},
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityKSAConflict {
			t.Fatalf("expected 400 identity_ksa_conflict, got %+v (%v)", res.sce, res.err)
		}
		if got := res.sce.Details[api.BrokerErrDetailConflictingKSA]; got != "agent-worker" {
			t.Errorf("conflictingKubernetesServiceAccount = %v, want agent-worker", got)
		}
		if got := res.sce.Details[api.BrokerErrDetailNamespace]; got != discoveryTestNamespace {
			t.Errorf("namespace = %v, want %s", got, discoveryTestNamespace)
		}
		for _, action := range res.client.Actions() {
			v := action.GetVerb()
			if v == "update" || v == "patch" || v == "delete" {
				t.Errorf("existing KSA was modified via %s", v)
			}
		}
	})

	t.Run("existing KSA annotated for different GSA returns identity_ksa_conflict", func(t *testing.T) {
		other := annotatedKSA(discoveryTestNamespace, "agent-worker", "other-sa@my-project.iam.gserviceaccount.com")
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
			objects:      []k8sruntime.Object{other},
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityKSAConflict {
			t.Fatalf("expected 400 identity_ksa_conflict, got %+v (%v)", res.sce, res.err)
		}
	})
}

func TestBuildStartContext_KubernetesAssignCreateForbidden(t *testing.T) {
	gr := schema.GroupResource{Resource: "serviceaccounts"}

	t.Run("create forbidden returns identity_not_mapped with missingPermission create serviceaccounts", func(t *testing.T) {
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
			createReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, apierrors.NewForbidden(gr, "agent-worker", errors.New("raw-apiserver-secret"))
			},
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityNotMapped {
			t.Fatalf("expected 400 identity_not_mapped, got %+v (%v)", res.sce, res.err)
		}
		if got := res.sce.Details[api.BrokerErrDetailMissingPermission]; got != api.BrokerKSAPermissionCreate {
			t.Errorf("missingPermission = %v, want %q", got, api.BrokerKSAPermissionCreate)
		}
		if got := res.sce.Details[api.BrokerErrDetailNamespace]; got != discoveryTestNamespace {
			t.Errorf("namespace = %v, want %q", got, discoveryTestNamespace)
		}
		if strings.Contains(res.sce.Message, "raw-apiserver-secret") {
			t.Errorf("client message leaked raw API server error: %q", res.sce.Message)
		}
		if !strings.Contains(res.logs, "raw-apiserver-secret") {
			t.Errorf("server log should record raw API server error: %q", res.logs)
		}
	})

	t.Run("already exists then get forbidden returns identity_not_mapped with missingPermission get serviceaccounts", func(t *testing.T) {
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
			objects: []k8sruntime.Object{
				&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: discoveryTestNamespace, Name: "agent-worker"}},
			},
			getReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, apierrors.NewForbidden(gr, "agent-worker", errors.New("get-denied-secret"))
			},
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityNotMapped {
			t.Fatalf("expected 400 identity_not_mapped, got %+v (%v)", res.sce, res.err)
		}
		if got := res.sce.Details[api.BrokerErrDetailMissingPermission]; got != api.BrokerKSAPermissionGet {
			t.Errorf("missingPermission = %v, want %q", got, api.BrokerKSAPermissionGet)
		}
		if strings.Contains(res.sce.Message, "get-denied-secret") {
			t.Errorf("client message leaked raw API server error: %q", res.sce.Message)
		}
	})
}

func TestBuildStartContext_KubernetesAssignRefusedExplicitFieldsDoNotCreateKSA(t *testing.T) {
	t.Run("explicit serviceAccountName mismatch refuses before creating KSA", func(t *testing.T) {
		res := ksaCreateRun{
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
			explicitKSA:  "other-ksa",
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityKSAMismatch {
			t.Fatalf("expected 400 identity_ksa_mismatch, got %+v (%v)", res.sce, res.err)
		}
		if res.createCalls != 0 {
			t.Errorf("createCalls = %d, want 0 (must not leave an orphan KSA on refused request)", res.createCalls)
		}
		if got := res.sce.Details[api.BrokerErrDetailKSASource]; got != api.BrokerKSASourceCreated {
			t.Errorf("kubernetesServiceAccountSource = %v, want %q", got, api.BrokerKSASourceCreated)
		}
		if got := res.sce.Details[api.BrokerErrDetailMappedKSA]; got != "agent-worker" {
			t.Errorf("mappedKubernetesServiceAccount = %v, want agent-worker", got)
		}
	})

	t.Run("explicit namespace mismatch refuses before creating KSA", func(t *testing.T) {
		res := ksaCreateRun{
			globalYAML:        ksaCreateGlobalSettingsYAML,
			hubCreateKSA:      true,
			explicitNamespace: "wrong-ns",
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest {
			t.Fatalf("expected 400 namespace mismatch, got %+v (%v)", res.sce, res.err)
		}
		if res.createCalls != 0 {
			t.Errorf("createCalls = %d, want 0", res.createCalls)
		}
	})
}

// Direct test of createOrReuseAssignKSA with a flat selection (no legacy
// discovery dependency), pinning the reuse contract required by P3b.
func TestCreateOrReuseAssignKSA_ReusableWithoutDiscovery(t *testing.T) {
	srv := &Server{
		config:            ServerConfig{BrokerName: "flat-k8s"},
		agentLifecycleLog: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	flatSel := dispatchProfileSelection{InstanceKey: "k8s-1", RuntimeTargetID: "rt-1"}
	client := fake.NewClientset()

	got, sce := srv.createOrReuseAssignKSA(context.Background(), client, "agent-1", "agents-ns", "agent-worker", discoveryTestGSA, flatSel)
	if sce != nil {
		t.Fatalf("unexpected error: %+v", sce)
	}
	if got != "agent-worker" {
		t.Errorf("got %q, want agent-worker", got)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "list" {
			t.Error("createOrReuseAssignKSA must not list serviceaccounts")
		}
	}
}
