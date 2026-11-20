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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

const (
	flatKSACreateGSA = "scion-worker@hub-proj.iam.gserviceaccount.com"
	flatKSACreateKSA = "scion-worker"
)

type flatKSACreateRun struct {
	target            config.V1RuntimeTargetConfig
	globalYAML        string
	gsa               string
	hubCreateKSA      bool
	ctxHubCreateKSA   bool
	explicitKSA       string
	explicitNamespace string
	objects           []k8sruntime.Object
	getReactor        func(k8stesting.Action) (bool, k8sruntime.Object, error)
	createReactor     func(k8stesting.Action) (bool, k8sruntime.Object, error)
}

type flatKSACreateResult struct {
	sc            *startContext
	sce           *startContextError
	err           error
	client        *fake.Clientset
	clientsetUses int
	listCalls     int
	getCalls      int
	getNames      []string
	createCalls   int
	updateCalls   int
	patchCalls    int
	deleteCalls   int
	logs          string
}

func (r flatKSACreateRun) run(t *testing.T) flatKSACreateResult {
	t.Helper()
	srv := newFlatKubernetesStartServer(t, r.target)
	srv.config.BrokerName = "flat-broker-a"

	if r.globalYAML != "" {
		newTestGlobalSettings(t, r.globalYAML)
	}

	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	client := fake.NewClientset(r.objects...)
	var res flatKSACreateResult
	res.client = client

	client.PrependReactor("list", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.listCalls++
		return false, nil, nil
	})
	client.PrependReactor("get", "serviceaccounts", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.getCalls++
		if ga, ok := a.(k8stesting.GetAction); ok {
			res.getNames = append(res.getNames, ga.GetName())
		}
		if r.getReactor != nil {
			return r.getReactor(a)
		}
		return false, nil, nil
	})
	client.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.createCalls++
		if r.createReactor != nil {
			return r.createReactor(a)
		}
		return false, nil, nil
	})
	client.PrependReactor("update", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.updateCalls++
		return false, nil, nil
	})
	client.PrependReactor("patch", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.patchCalls++
		return false, nil, nil
	})
	client.PrependReactor("delete", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		res.deleteCalls++
		return false, nil, nil
	})

	srv.assignKSAClientset = func(agent.Manager) (kubernetes.Interface, error) {
		res.clientsetUses++
		return client, nil
	}

	gsa := r.gsa
	if gsa == "" {
		gsa = flatKSACreateGSA
	}
	createCfg := &CreateAgentConfig{
		GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: gsa, ProjectID: "hub-proj"},
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
	sc, err := srv.buildStartContext(ctx, flatStartInputs("flat-ksa-create-agent", startContextInputs{
		Config:    createCfg,
		Operation: opCreate,
	}))
	res.sc = sc
	res.err = err
	errors.As(err, &res.sce)
	res.logs = logBuf.String()

	if res.listCalls != 0 {
		t.Fatalf("flat assign must never list serviceaccounts, got %d list call(s)", res.listCalls)
	}
	if res.updateCalls != 0 || res.patchCalls != 0 || res.deleteCalls != 0 {
		t.Fatalf("flat assign must never modify or delete existing serviceaccounts (update=%d patch=%d delete=%d)",
			res.updateCalls, res.patchCalls, res.deleteCalls)
	}
	return res
}

func scionManagedKSA(namespace, name, gsaEmail string) *corev1.ServiceAccount {
	automount := false
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				k8s.ManagedByLabel: k8s.ManagedByScion,
			},
			Annotations: map[string]string{
				k8s.WorkloadIdentityGSAAnnotation: gsaEmail,
				k8s.CreatedForGSAAnnotation:       gsaEmail,
			},
		},
		AutomountServiceAccountToken: &automount,
	}
}

func TestFlatKubernetesIdentity_CreateServiceAccounts_CreatesOnMissWithoutListing(t *testing.T) {
	for _, viaCtx := range []bool{false, true} {
		name := "create_request"
		if viaCtx {
			name = "start_context"
		}
		t.Run(name, func(t *testing.T) {
			res := flatKSACreateRun{
				target:          config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
				hubCreateKSA:    !viaCtx,
				ctxHubCreateKSA: viaCtx,
				// Another annotated KSA in the namespace must not be listed or adopted.
				objects: []k8sruntime.Object{annotatedKSA("agents", "other-annotated", flatKSACreateGSA)},
			}.run(t)
			if res.err != nil {
				t.Fatalf("unexpected error: %v", res.err)
			}
			if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != flatKSACreateKSA {
				t.Fatalf("ResolvedKubernetesServiceAccountName = %q, want %s", got, flatKSACreateKSA)
			}
			if res.sc.AssignSelection == nil || *res.sc.AssignSelection != wantFlatSelection() {
				t.Errorf("AssignSelection = %+v, want %+v", res.sc.AssignSelection, wantFlatSelection())
			}
			if res.getCalls != 1 || len(res.getNames) != 1 || res.getNames[0] != flatKSACreateKSA {
				t.Errorf("getCalls=%d getNames=%v, want 1 get for %s", res.getCalls, res.getNames, flatKSACreateKSA)
			}
			if res.createCalls != 1 {
				t.Errorf("createCalls=%d, want 1", res.createCalls)
			}
			created, err := res.client.CoreV1().ServiceAccounts("agents").Get(context.Background(), flatKSACreateKSA, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Get created KSA: %v", err)
			}
			if created.Labels[k8s.ManagedByLabel] != k8s.ManagedByScion {
				t.Errorf("managed-by label = %q, want %q", created.Labels[k8s.ManagedByLabel], k8s.ManagedByScion)
			}
			if created.Annotations[k8s.WorkloadIdentityGSAAnnotation] != flatKSACreateGSA {
				t.Errorf("GSA annotation = %q, want %q", created.Annotations[k8s.WorkloadIdentityGSAAnnotation], flatKSACreateGSA)
			}
			if created.Annotations[k8s.CreatedForGSAAnnotation] != flatKSACreateGSA {
				t.Errorf("created-for-gsa annotation = %q, want %q", created.Annotations[k8s.CreatedForGSAAnnotation], flatKSACreateGSA)
			}
			if created.AutomountServiceAccountToken == nil || *created.AutomountServiceAccountToken {
				t.Errorf("AutomountServiceAccountToken = %v, want false", created.AutomountServiceAccountToken)
			}
		})
	}
}

func TestFlatKubernetesIdentity_CreateServiceAccounts_ReusesScionLabelledKSAWithOneGet(t *testing.T) {
	res := flatKSACreateRun{
		target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
		hubCreateKSA: true,
		objects:      []k8sruntime.Object{scionManagedKSA("agents", flatKSACreateKSA, flatKSACreateGSA)},
	}.run(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != flatKSACreateKSA {
		t.Fatalf("ResolvedKubernetesServiceAccountName = %q, want %s", got, flatKSACreateKSA)
	}
	if res.getCalls != 1 || res.createCalls != 0 {
		t.Errorf("getCalls=%d createCalls=%d, want 1 get and 0 creates", res.getCalls, res.createCalls)
	}
}

func TestFlatKubernetesIdentity_CreateServiceAccounts_RefusesNonScionOrConflictingKSA(t *testing.T) {
	cases := map[string]*corev1.ServiceAccount{
		// Critical §3.10 rule: annotated for this GSA, but lacks app.kubernetes.io/managed-by=scion.
		"annotated_only_without_scion_label": annotatedKSA("agents", flatKSACreateKSA, flatKSACreateGSA),
		"unannotated": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      flatKSACreateKSA,
				Namespace: "agents",
				Labels:    map[string]string{"sentinel": "keep"},
			},
		},
		"annotated_for_different_gsa":      annotatedKSA("agents", flatKSACreateKSA, "other@hub-proj.iam.gserviceaccount.com"),
		"scion_labelled_for_different_gsa": scionManagedKSA("agents", flatKSACreateKSA, "other@hub-proj.iam.gserviceaccount.com"),
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			orig := existing.DeepCopy()
			res := flatKSACreateRun{
				target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
				hubCreateKSA: true,
				objects:      []k8sruntime.Object{existing},
			}.run(t)
			if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityKSAConflict {
				t.Fatalf("want 400 %s, got sce=%+v err=%v", ErrCodeIdentityKSAConflict, res.sce, res.err)
			}
			if res.sce.Details[api.BrokerErrDetailConflictingKSA] != flatKSACreateKSA {
				t.Errorf("conflictingServiceAccount = %v, want %s", res.sce.Details[api.BrokerErrDetailConflictingKSA], flatKSACreateKSA)
			}
			if res.sce.Details[api.BrokerErrDetailNamespace] != "agents" {
				t.Errorf("namespace = %v, want agents", res.sce.Details[api.BrokerErrDetailNamespace])
			}
			if res.getCalls != 1 || res.createCalls != 0 {
				t.Errorf("getCalls=%d createCalls=%d, want 1 get and 0 creates", res.getCalls, res.createCalls)
			}
			after, err := res.client.CoreV1().ServiceAccounts("agents").Get(context.Background(), flatKSACreateKSA, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("existing KSA missing after conflict: %v", err)
			}
			if after.Labels["sentinel"] != orig.Labels["sentinel"] ||
				after.Labels[k8s.ManagedByLabel] != orig.Labels[k8s.ManagedByLabel] ||
				after.Annotations[k8s.WorkloadIdentityGSAAnnotation] != orig.Annotations[k8s.WorkloadIdentityGSAAnnotation] {
				t.Errorf("existing KSA was modified: before=%+v after=%+v", orig.ObjectMeta, after.ObjectMeta)
			}
		})
	}
}

func TestFlatKubernetesIdentity_CreateServiceAccounts_AlreadyExistsRace(t *testing.T) {
	t.Run("concurrent Scion-managed create wins race and is reused", func(t *testing.T) {
		winner := scionManagedKSA("agents", flatKSACreateKSA, flatKSACreateGSA)
		createdInRace := false
		res := flatKSACreateRun{
			target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
			hubCreateKSA: true,
			createReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				createdInRace = true
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "serviceaccounts"}, flatKSACreateKSA)
			},
			getReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				if !createdInRace {
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, flatKSACreateKSA)
				}
				return true, winner.DeepCopy(), nil
			},
		}.run(t)
		if res.err != nil {
			t.Fatalf("unexpected error: %v", res.err)
		}
		if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != flatKSACreateKSA {
			t.Errorf("ResolvedKubernetesServiceAccountName = %q, want %s", got, flatKSACreateKSA)
		}
		if res.getCalls != 2 || res.createCalls != 1 {
			t.Errorf("getCalls=%d createCalls=%d, want 2 gets and 1 create", res.getCalls, res.createCalls)
		}
	})

	t.Run("concurrent annotated-only KSA without Scion label is refused on race", func(t *testing.T) {
		annotatedOnly := annotatedKSA("agents", flatKSACreateKSA, flatKSACreateGSA)
		createdInRace := false
		res := flatKSACreateRun{
			target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
			hubCreateKSA: true,
			createReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				createdInRace = true
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "serviceaccounts"}, flatKSACreateKSA)
			},
			getReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				if !createdInRace {
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, flatKSACreateKSA)
				}
				return true, annotatedOnly.DeepCopy(), nil
			},
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityKSAConflict {
			t.Fatalf("want 400 %s on annotated-only race KSA, got sce=%+v err=%v", ErrCodeIdentityKSAConflict, res.sce, res.err)
		}
	})
}

func TestFlatKubernetesIdentity_CreateServiceAccounts_PrecedenceAndGates(t *testing.T) {
	t.Run("explicit instance mapping wins with zero client calls", func(t *testing.T) {
		res := flatKSACreateRun{
			target: config.V1RuntimeTargetConfig{
				Namespace:                        "agents",
				CreateServiceAccounts:            true,
				KubernetesServiceAccountMappings: map[string]string{flatKSACreateGSA: "explicit-ksa"},
			},
			hubCreateKSA: true,
		}.run(t)
		if res.err != nil {
			t.Fatalf("unexpected error: %v", res.err)
		}
		if got := res.sc.Opts.ResolvedKubernetesServiceAccountName; got != "explicit-ksa" {
			t.Errorf("ResolvedKubernetesServiceAccountName = %q, want explicit-ksa", got)
		}
		if res.clientsetUses != 0 || res.getCalls != 0 || res.createCalls != 0 {
			t.Errorf("clientsetUses=%d getCalls=%d createCalls=%d, want 0", res.clientsetUses, res.getCalls, res.createCalls)
		}
	})

	t.Run("setting off keeps identity_not_mapped with zero client calls even when global settings enable create", func(t *testing.T) {
		res := flatKSACreateRun{
			target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: false},
			globalYAML:   ksaCreateGlobalSettingsYAML,
			hubCreateKSA: true,
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityNotMapped {
			t.Fatalf("want 400 %s, got sce=%+v err=%v", ErrCodeIdentityNotMapped, res.sce, res.err)
		}
		if res.clientsetUses != 0 || res.getCalls != 0 || res.createCalls != 0 {
			t.Errorf("clientsetUses=%d getCalls=%d createCalls=%d, want 0", res.clientsetUses, res.getCalls, res.createCalls)
		}
	})

	t.Run("hub CreateKSA false keeps identity_not_mapped with zero client calls", func(t *testing.T) {
		res := flatKSACreateRun{
			target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
			hubCreateKSA: false,
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityNotMapped {
			t.Fatalf("want 400 %s, got sce=%+v err=%v", ErrCodeIdentityNotMapped, res.sce, res.err)
		}
		if res.clientsetUses != 0 || res.getCalls != 0 || res.createCalls != 0 {
			t.Errorf("clientsetUses=%d getCalls=%d createCalls=%d, want 0", res.clientsetUses, res.getCalls, res.createCalls)
		}
	})

	t.Run("explicit request KSA or namespace mismatch refused before any Get or Create", func(t *testing.T) {
		ksaMismatch := flatKSACreateRun{
			target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
			hubCreateKSA: true,
			explicitKSA:  "wrong-ksa",
		}.run(t)
		if ksaMismatch.sce == nil || ksaMismatch.sce.Status != http.StatusBadRequest || ksaMismatch.sce.Code != ErrCodeIdentityKSAMismatch {
			t.Fatalf("want 400 %s, got sce=%+v err=%v", ErrCodeIdentityKSAMismatch, ksaMismatch.sce, ksaMismatch.err)
		}
		if ksaMismatch.sce.Details[api.BrokerErrDetailKSASource] != api.BrokerKSASourceCreated {
			t.Errorf("ksaSource = %v, want %s", ksaMismatch.sce.Details[api.BrokerErrDetailKSASource], api.BrokerKSASourceCreated)
		}
		if ksaMismatch.clientsetUses != 0 || ksaMismatch.getCalls != 0 || ksaMismatch.createCalls != 0 {
			t.Errorf("clientsetUses=%d getCalls=%d createCalls=%d, want 0", ksaMismatch.clientsetUses, ksaMismatch.getCalls, ksaMismatch.createCalls)
		}

		nsMismatch := flatKSACreateRun{
			target:            config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
			hubCreateKSA:      true,
			explicitNamespace: "other-ns",
		}.run(t)
		if nsMismatch.sce == nil || nsMismatch.sce.Status != http.StatusBadRequest {
			t.Fatalf("want 400 namespace mismatch, got sce=%+v err=%v", nsMismatch.sce, nsMismatch.err)
		}
		if nsMismatch.clientsetUses != 0 || nsMismatch.getCalls != 0 || nsMismatch.createCalls != 0 {
			t.Errorf("clientsetUses=%d getCalls=%d createCalls=%d, want 0", nsMismatch.clientsetUses, nsMismatch.getCalls, nsMismatch.createCalls)
		}
	})
}

func TestFlatKubernetesIdentity_CreateServiceAccounts_Forbidden(t *testing.T) {
	t.Run("forbidden on initial Get returns identity_not_mapped with get permission and no discovery detail", func(t *testing.T) {
		res := flatKSACreateRun{
			target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
			hubCreateKSA: true,
			getReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, flatKSACreateKSA, errors.New("denied"))
			},
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityNotMapped {
			t.Fatalf("want 400 %s, got sce=%+v err=%v", ErrCodeIdentityNotMapped, res.sce, res.err)
		}
		if res.sce.Details[api.BrokerErrDetailMissingPermission] != api.BrokerKSAPermissionGet {
			t.Errorf("missingPermission = %v, want %s", res.sce.Details[api.BrokerErrDetailMissingPermission], api.BrokerKSAPermissionGet)
		}
		if _, hasDiscovery := res.sce.Details[api.BrokerErrDetailDiscovery]; hasDiscovery {
			t.Errorf("flat permission refusal must not set discovery detail: %+v", res.sce.Details)
		}
	})

	t.Run("forbidden on Create returns identity_not_mapped with create permission and no discovery detail", func(t *testing.T) {
		res := flatKSACreateRun{
			target:       config.V1RuntimeTargetConfig{Namespace: "agents", CreateServiceAccounts: true},
			hubCreateKSA: true,
			createReactor: func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, flatKSACreateKSA, errors.New("denied"))
			},
		}.run(t)
		if res.sce == nil || res.sce.Status != http.StatusBadRequest || res.sce.Code != ErrCodeIdentityNotMapped {
			t.Fatalf("want 400 %s, got sce=%+v err=%v", ErrCodeIdentityNotMapped, res.sce, res.err)
		}
		if res.sce.Details[api.BrokerErrDetailMissingPermission] != api.BrokerKSAPermissionCreate {
			t.Errorf("missingPermission = %v, want %s", res.sce.Details[api.BrokerErrDetailMissingPermission], api.BrokerKSAPermissionCreate)
		}
		if _, hasDiscovery := res.sce.Details[api.BrokerErrDetailDiscovery]; hasDiscovery {
			t.Errorf("flat permission refusal must not set discovery detail: %+v", res.sce.Details)
		}
	})
}
