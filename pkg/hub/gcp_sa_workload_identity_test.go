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

package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// wiStubAdmin is a GCPServiceAccountAdmin that records AddRoleMembers calls
// and fails them with fail (nil succeeds). It refuses every other call: the
// Workload Identity grant makes no other IAM call.
type wiStubAdmin struct {
	calls [][]string
	roles []string
	fail  func(call int, members []string) error
}

func (a *wiStubAdmin) CreateServiceAccount(context.Context, string, string, string, string) (string, string, error) {
	return "", "", errors.New("unexpected CreateServiceAccount")
}

func (a *wiStubAdmin) DeleteServiceAccount(context.Context, string) error {
	return errors.New("unexpected DeleteServiceAccount")
}

func (a *wiStubAdmin) SetIAMPolicy(context.Context, string, string, string) error {
	return errors.New("unexpected SetIAMPolicy")
}

func (a *wiStubAdmin) AddRoleMembers(_ context.Context, _, role string, members []string) error {
	a.calls = append(a.calls, append([]string(nil), members...))
	a.roles = append(a.roles, role)
	if a.fail != nil {
		return a.fail(len(a.calls), members)
	}
	return nil
}

const wiTestGSA = "scion-foo@hub-proj.iam.gserviceaccount.com"

func wiLegacyBroker(name string, caps bool, targets []store.WorkloadIdentityTarget, profiles ...store.BrokerProfile) *store.RuntimeBroker {
	b := &store.RuntimeBroker{ID: name, Name: name, Profiles: profiles, WorkloadIdentityTargets: targets}
	if caps {
		b.Capabilities = &store.BrokerCapabilities{WITargetsV1: true}
	}
	return b
}

func wiK8sProfile(name string) store.BrokerProfile {
	return store.BrokerProfile{Name: name, Type: "kubernetes"}
}

func wiFlatBroker(name, typ string, targets []store.WorkloadIdentityTarget) *store.RuntimeBroker {
	return &store.RuntimeBroker{
		ID: name, Name: name, WorkloadIdentityTargets: targets,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-" + name, Type: typ},
		Capabilities:  &store.BrokerCapabilities{WITargetsV1: true},
	}
}

// The plan merges equal (pool, namespace) pairs across legacy and flat
// rows, lists Kubernetes targets with no pool per broker (too old or not
// configured), and ignores non-Kubernetes profiles and instances.
func TestWorkloadIdentityMintPlanFor(t *testing.T) {
	plan := workloadIdentityMintPlanFor([]*store.RuntimeBroker{
		wiLegacyBroker("a", true, []store.WorkloadIdentityTarget{
			{Profile: "gke", Pool: "p1.svc.id.goog", Namespace: "agents"},
			{Profile: "gke-dev", Pool: "p1.svc.id.goog", Namespace: "dev"},
		}, wiK8sProfile("gke"), wiK8sProfile("gke-dev"), wiK8sProfile("gke-nopool"),
			store.BrokerProfile{Name: "local", Type: "docker"},
			store.BrokerProfile{Name: "cr", Type: "cloudrun"}),
		wiLegacyBroker("old", false, nil, wiK8sProfile("k8s")),
		wiLegacyBroker("docker-only", false, nil, store.BrokerProfile{Name: "local", Type: "docker"}),
		wiFlatBroker("flat", "kubernetes", []store.WorkloadIdentityTarget{{Pool: "p1.svc.id.goog", Namespace: "agents"}}),
		wiFlatBroker("flat-nopool", "kubernetes", []store.WorkloadIdentityTarget{}),
		wiFlatBroker("flat-docker", "docker", nil),
		nil,
	})
	require.Len(t, plan.targets, 2)
	assert.Equal(t, workloadIdentityMintTarget{pool: "p1.svc.id.goog", namespace: "agents", profiles: []string{"a/gke", "flat"}}, plan.targets[0])
	assert.Equal(t, workloadIdentityMintTarget{pool: "p1.svc.id.goog", namespace: "dev", profiles: []string{"a/gke-dev"}}, plan.targets[1])
	assert.Equal(t, []workloadIdentityMissing{
		{profiles: []string{"a/gke-nopool"}},
		{profiles: []string{"flat-nopool"}},
		{profiles: []string{"old/k8s"}, tooOld: true},
	}, plan.missing)

	assert.True(t, workloadIdentityMintPlanFor([]*store.RuntimeBroker{
		wiLegacyBroker("docker-only", true, []store.WorkloadIdentityTarget{}, store.BrokerProfile{Name: "local", Type: "docker"}),
	}).empty(), "no Kubernetes target: nothing to report")
}

func TestGrantWorkloadIdentityBestEffort_AllBoundInOneWrite(t *testing.T) {
	admin := &wiStubAdmin{}
	s := &Server{gcpIAMAdmin: admin}
	plan := workloadIdentityMintPlan{
		targets: []workloadIdentityMintTarget{
			{pool: "p1.svc.id.goog", namespace: "agents", profiles: []string{"a/gke"}},
			{pool: "p2.svc.id.goog", namespace: "team", profiles: []string{"flat"}},
		},
		missing: []workloadIdentityMissing{{profiles: []string{"old/k8s"}, tooOld: true}},
	}
	got := s.grantWorkloadIdentityBestEffort(context.Background(), wiTestGSA, plan)

	require.Equal(t, [][]string{{
		"serviceAccount:p1.svc.id.goog[agents/scion-foo]",
		"serviceAccount:p2.svc.id.goog[team/scion-foo]",
	}}, admin.calls, "one policy write with every member")
	assert.Equal(t, []string{"roles/iam.workloadIdentityUser"}, admin.roles)
	require.Len(t, got, 3)
	assert.Equal(t, api.WorkloadIdentityBinding{
		Pool: "p1.svc.id.goog", Namespace: "agents", KSA: "scion-foo",
		Member: "serviceAccount:p1.svc.id.goog[agents/scion-foo]", Profiles: []string{"a/gke"},
		State: api.WorkloadIdentityBindingBound,
	}, got[0])
	assert.Equal(t, api.WorkloadIdentityBindingBound, got[1].State)
	assert.Equal(t, api.WorkloadIdentityBindingSkipped, got[2].State)
	assert.Equal(t, api.WorkloadIdentityReasonNoPool, got[2].Reason)
	assert.Contains(t, got[2].Detail, "does not report")
	assert.Equal(t, []string{"old/k8s"}, got[2].Profiles)
	assert.Equal(t, `gcloud iam service-accounts add-iam-policy-binding `+wiTestGSA+` --role roles/iam.workloadIdentityUser --member "serviceAccount:<CLUSTER_PROJECT>.svc.id.goog[<NAMESPACE>/scion-foo]"`, got[2].ManualStep)
}

// Only Kubernetes targets with no pool: no IAM call at all.
func TestGrantWorkloadIdentityBestEffort_NoTargetsNoIAMCall(t *testing.T) {
	admin := &wiStubAdmin{}
	s := &Server{gcpIAMAdmin: admin}
	got := s.grantWorkloadIdentityBestEffort(context.Background(), wiTestGSA, workloadIdentityMintPlan{
		missing: []workloadIdentityMissing{{profiles: []string{"a/gke"}}},
	})
	assert.Empty(t, admin.calls)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Detail, "no workload_identity_pool is configured")
}

func TestGrantWorkloadIdentityBestEffort_PermissionDeniedSkipsAll(t *testing.T) {
	admin := &wiStubAdmin{fail: func(int, []string) error {
		return fmt.Errorf("getting IAM policy: %w", &googleapi.Error{Code: http.StatusForbidden, Message: "Permission 'iam.serviceAccounts.getIamPolicy' denied"})
	}}
	s := &Server{gcpIAMAdmin: admin}
	got := s.grantWorkloadIdentityBestEffort(context.Background(), wiTestGSA, workloadIdentityMintPlan{targets: []workloadIdentityMintTarget{
		{pool: "p1.svc.id.goog", namespace: "agents"}, {pool: "p2.svc.id.goog", namespace: "agents"},
	}})
	assert.Len(t, admin.calls, 1, "no per-member fallback when permission is denied")
	for _, b := range got {
		assert.Equal(t, api.WorkloadIdentityBindingSkipped, b.State)
		assert.Equal(t, api.WorkloadIdentityReasonPermissionDenied, b.Reason)
		assert.Equal(t, workloadIdentityManualStep(wiTestGSA, b.Member), b.ManualStep)
	}
}

// One pool that does not exist fails the batch; each member is then granted
// on its own, so the good target is still bound. The pool error is not
// retried as eventual consistency.
func TestGrantWorkloadIdentityBestEffort_PoolNotFoundFallsBackPerMember(t *testing.T) {
	poolErr := &googleapi.Error{Code: http.StatusBadRequest, Message: "Identity Pool does not exist (nope-project.svc.id.goog). Please check that you specified a valid resource name as returned in the `name` attribute in the configuration API."}
	admin := &wiStubAdmin{fail: func(_ int, members []string) error {
		for _, m := range members {
			if strings.Contains(m, "nope-project") {
				return fmt.Errorf("setting IAM policy: %w", poolErr)
			}
		}
		return nil
	}}
	s := &Server{gcpIAMAdmin: admin}
	start := time.Now()
	got := s.grantWorkloadIdentityBestEffort(context.Background(), wiTestGSA, workloadIdentityMintPlan{targets: []workloadIdentityMintTarget{
		{pool: "good-project.svc.id.goog", namespace: "agents"}, {pool: "nope-project.svc.id.goog", namespace: "agents"},
	}})
	assert.Less(t, time.Since(start), 900*time.Millisecond, "a missing pool is not retried")
	assert.Len(t, admin.calls, 3, "the batch, then one call per member")
	require.Len(t, got, 2)
	assert.Equal(t, api.WorkloadIdentityBindingBound, got[0].State)
	assert.Equal(t, api.WorkloadIdentityBindingSkipped, got[1].State)
	assert.Equal(t, api.WorkloadIdentityReasonPoolNotFound, got[1].Reason)
	assert.Contains(t, got[1].Detail, "nope-project.svc.id.goog")
	assert.Contains(t, got[1].ManualStep, `--member "serviceAccount:nope-project.svc.id.goog[agents/scion-foo]"`)
}

func TestGrantWorkloadIdentityBestEffort_OtherErrorIsFailed(t *testing.T) {
	admin := &wiStubAdmin{fail: func(int, []string) error {
		return &googleapi.Error{Code: http.StatusInternalServerError, Message: "backend error"}
	}}
	s := &Server{gcpIAMAdmin: admin}
	got := s.grantWorkloadIdentityBestEffort(context.Background(), wiTestGSA, workloadIdentityMintPlan{targets: []workloadIdentityMintTarget{
		{pool: "p1.svc.id.goog", namespace: "agents"},
	}})
	require.Len(t, got, 1)
	assert.Equal(t, api.WorkloadIdentityBindingFailed, got[0].State)
	assert.Equal(t, api.WorkloadIdentityReasonIAMError, got[0].Reason)
	assert.NotEmpty(t, got[0].ManualStep)
}

// An etag conflict on the policy write is retried once.
func TestGrantWorkloadIdentityBestEffort_EtagConflictRetried(t *testing.T) {
	for _, code := range []int{http.StatusConflict, http.StatusPreconditionFailed} {
		admin := &wiStubAdmin{fail: func(call int, _ []string) error {
			if call == 1 {
				return fmt.Errorf("setting IAM policy: %w", &googleapi.Error{Code: code, Message: "There were concurrent policy changes."})
			}
			return nil
		}}
		s := &Server{gcpIAMAdmin: admin}
		got := s.grantWorkloadIdentityBestEffort(context.Background(), wiTestGSA, workloadIdentityMintPlan{targets: []workloadIdentityMintTarget{
			{pool: "p1.svc.id.goog", namespace: "agents"}, {pool: "p2.svc.id.goog", namespace: "agents"},
		}})
		assert.Len(t, admin.calls, 2, "code %d", code)
		for _, b := range got {
			assert.Equal(t, api.WorkloadIdentityBindingBound, b.State, "code %d", code)
		}
	}
}

func TestIsEventualConsistencyError_PoolNotFoundIsPermanent(t *testing.T) {
	assert.True(t, isEventualConsistencyError(errors.New("googleapi: Error 400: Service account x does not exist., badRequest")))
	assert.False(t, isEventualConsistencyError(errors.New("googleapi: Error 400: Identity Pool does not exist (p.svc.id.goog). Please check ..., badRequest")))
}

func TestAddPolicyRoleMembers(t *testing.T) {
	policy := &iamv1.Policy{Bindings: []*iamv1.Binding{
		{Role: roleWorkloadIdentityUser, Members: []string{"serviceAccount:p.svc.id.goog[a/x]"}, Condition: &iamv1.Expr{Expression: "true"}},
		{Role: roleWorkloadIdentityUser, Members: []string{"serviceAccount:p.svc.id.goog[a/y]"}},
		{Role: "roles/iam.serviceAccountTokenCreator", Members: []string{"serviceAccount:hub@x"}},
	}}
	changed := addPolicyRoleMembers(policy, roleWorkloadIdentityUser, []string{"serviceAccount:p.svc.id.goog[a/y]", "serviceAccount:p.svc.id.goog[b/y]"})
	assert.True(t, changed)
	assert.Equal(t, []string{"serviceAccount:p.svc.id.goog[a/x]"}, policy.Bindings[0].Members, "a conditional binding is never extended")
	assert.Equal(t, []string{"serviceAccount:p.svc.id.goog[a/y]", "serviceAccount:p.svc.id.goog[b/y]"}, policy.Bindings[1].Members)
	assert.False(t, addPolicyRoleMembers(policy, roleWorkloadIdentityUser, []string{"serviceAccount:p.svc.id.goog[a/y]"}), "already granted: no write")

	fresh := &iamv1.Policy{}
	assert.True(t, addPolicyRoleMembers(fresh, roleWorkloadIdentityUser, []string{"m1", "m2"}))
	require.Len(t, fresh.Bindings, 1)
	assert.True(t, slices.Equal([]string{"m1", "m2"}, fresh.Bindings[0].Members))
}

func TestNormalizeWorkloadIdentityTargets(t *testing.T) {
	in := []store.WorkloadIdentityTarget{
		{Profile: " gke ", Pool: " p.svc.id.goog ", Namespace: " agents "},
		{Profile: "x", Pool: "  "},
	}
	got, dropped := normalizeWorkloadIdentityTargets(in, false)
	assert.False(t, dropped)
	assert.Equal(t, []store.WorkloadIdentityTarget{{Profile: "gke", Pool: "p.svc.id.goog", Namespace: "agents"}}, got)

	got, dropped = normalizeWorkloadIdentityTargets(in, true)
	assert.True(t, dropped, "a flat row has no profiles")
	assert.Equal(t, []store.WorkloadIdentityTarget{{Pool: "p.svc.id.goog", Namespace: "agents"}}, got)

	empty, _ := normalizeWorkloadIdentityTargets(nil, false)
	assert.NotNil(t, empty, "an empty report is stored as an empty list")
}
