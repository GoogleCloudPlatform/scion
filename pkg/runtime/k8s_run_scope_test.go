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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Tests for run-scoped Kubernetes deletes (ptone/scion#2550 P2,
// k8s_run_scope.go).

const (
	rsAgent = "proj1--agent"
	rsRunA  = "11111111-1111-4111-8111-111111111111"
	rsRunB  = "22222222-2222-4222-8222-222222222222"
)

var (
	rsAgentSecret = agentSecretPrefix + rsAgent
	rsAuthSecret  = agentAuthSecretPrefix + rsAgent
	rsSPC         = agentSecretPrefix + rsAgent
)

// uidEnforcer makes the fake API servers honour delete UID preconditions
// (the fake tracker ignores them) and records every delete.
type uidEnforcer struct {
	mu            sync.Mutex
	deletes       []string // resource/name
	unconditional []string // deletes with no UID precondition
}

func enforceUIDPreconditions(cs *k8sfake.Clientset, dyn *fake.FakeDynamicClient) *uidEnforcer {
	e := &uidEnforcer{}
	check := func(tracker k8stesting.ObjectTracker) k8stesting.ReactionFunc {
		return func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			del := action.(k8stesting.DeleteAction)
			key := del.GetResource().Resource + "/" + del.GetName()
			pre := del.GetDeleteOptions().Preconditions
			e.mu.Lock()
			e.deletes = append(e.deletes, key)
			if pre == nil || pre.UID == nil {
				e.unconditional = append(e.unconditional, key)
			}
			e.mu.Unlock()
			if pre == nil || pre.UID == nil {
				return false, nil, nil
			}
			obj, err := tracker.Get(del.GetResource(), del.GetNamespace(), del.GetName())
			if err != nil {
				return false, nil, nil // the default reactor answers NotFound
			}
			acc, err := meta.Accessor(obj)
			if err != nil {
				return true, nil, err
			}
			if acc.GetUID() != *pre.UID {
				gr := schema.GroupResource{Group: del.GetResource().Group, Resource: del.GetResource().Resource}
				return true, nil, k8serrors.NewConflict(gr, del.GetName(),
					fmt.Errorf("precondition failed: UID in precondition: %v, UID in object meta: %v", *pre.UID, acc.GetUID()))
			}
			return false, nil, nil
		}
	}
	cs.PrependReactor("delete", "*", check(cs.Tracker()))
	dyn.PrependReactor("delete", "*", check(dyn.Tracker()))
	return e
}

func (e *uidEnforcer) assertAllConditional(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.unconditional) > 0 {
		t.Errorf("deletes without a UID precondition: %v", e.unconditional)
	}
}

func (e *uidEnforcer) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.deletes)
}

func newRunScopeRuntime(t *testing.T) (*KubernetesRuntime, *k8sfake.Clientset, *fake.FakeDynamicClient, *uidEnforcer) {
	t.Helper()
	rt, cs, dyn := newGKECleanupTestRuntime(t)
	return rt, cs, dyn, enforceUIDPreconditions(cs, dyn)
}

// rsLabels returns production-shaped labels with the given run and start
// IDs (either may be empty to omit it).
func rsLabels(runID, startID string) map[string]string {
	l := productionAgentLabels("agent", "proj1")
	if runID != "" {
		l[api.LabelRunID] = runID
	}
	if startID != "" {
		l[labelStartID] = startID
	}
	return l
}

func rsSeedPod(t *testing.T, rt *KubernetesRuntime, uid string, labels map[string]string, phase corev1.PodPhase) {
	t.Helper()
	_, err := rt.Client.Clientset.CoreV1().Pods(rt.DefaultNamespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: rsAgent, Namespace: rt.DefaultNamespace, UID: types.UID(uid), Labels: labels},
		Status:     corev1.PodStatus{Phase: phase},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed pod: %v", err)
	}
}

func rsSeedSecret(t *testing.T, rt *KubernetesRuntime, name, uid string, labels map[string]string) {
	t.Helper()
	_, err := rt.Client.Clientset.CoreV1().Secrets(rt.DefaultNamespace).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rt.DefaultNamespace, UID: types.UID(uid), Labels: labels},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed Secret %s: %v", name, err)
	}
}

func rsSeedSPC(t *testing.T, rt *KubernetesRuntime, uid string, labels map[string]string) {
	t.Helper()
	spc := &unstructured.Unstructured{}
	spc.SetGroupVersionKind(schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClass"})
	spc.SetName(rsSPC)
	spc.SetNamespace(rt.DefaultNamespace)
	spc.SetUID(types.UID(uid))
	spc.SetLabels(labels)
	if _, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(rt.DefaultNamespace).Create(context.Background(), spc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed SPC: %v", err)
	}
}

// rsSeedRun seeds the pod, both Secrets and the SPC of one run.
func rsSeedRun(t *testing.T, rt *KubernetesRuntime, labels map[string]string, phase corev1.PodPhase, uidSuffix string) {
	t.Helper()
	rsSeedPod(t, rt, "pod-"+uidSuffix, labels, phase)
	rsSeedSecret(t, rt, rsAgentSecret, "sec-"+uidSuffix, labels)
	rsSeedSecret(t, rt, rsAuthSecret, "auth-"+uidSuffix, labels)
	rsSeedSPC(t, rt, "spc-"+uidSuffix, labels)
}

func rsPod(t *testing.T, rt *KubernetesRuntime) *corev1.Pod {
	t.Helper()
	p, err := rt.Client.Clientset.CoreV1().Pods(rt.DefaultNamespace).Get(context.Background(), rsAgent, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return p
}

type rsState struct{ pod, agentSecret, authSecret, spc bool }

func rsObserve(t *testing.T, rt *KubernetesRuntime) rsState {
	t.Helper()
	ns := rt.DefaultNamespace
	return rsState{
		pod:         rsPod(t, rt) != nil,
		agentSecret: secretExists(t, rt, ns, rsAgentSecret),
		authSecret:  secretExists(t, rt, ns, rsAuthSecret),
		spc:         spcExists(t, rt, ns, rsSPC),
	}
}

func rsExpect(t *testing.T, rt *KubernetesRuntime, want rsState) {
	t.Helper()
	if got := rsObserve(t, rt); got != want {
		t.Errorf("objects present = %+v, want %+v", got, want)
	}
}

var (
	rsAllPresent = rsState{true, true, true, true}
	rsAllGone    = rsState{}
)

// --- Delete ---

func TestK8sDeleteRun_MatchingRun_DeletesPodAndItsSecrets(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	rsSeedRun(t, rt, rsLabels(rsRunA, "start-a"), corev1.PodRunning, "a")
	// Another agent's Secret of the same run label shape must survive.
	rsSeedSecret(t, rt, agentSecretPrefix+"proj1--other", "sec-other", rsLabels(rsRunA, ""))

	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rsExpect(t, rt, rsAllGone)
	if !secretExists(t, rt, rt.DefaultNamespace, agentSecretPrefix+"proj1--other") {
		t.Error("another agent's Secret was deleted")
	}
	enf.assertAllConditional(t)
}

func TestK8sDeleteRun_OtherRun_LeavesEverythingAndReportsMismatch(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	rsSeedRun(t, rt, rsLabels(rsRunB, "start-b"), corev1.PodRunning, "b")

	err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA})
	if !errors.Is(err, ErrRunMismatch) {
		t.Fatalf("Delete error = %v, want ErrRunMismatch", err)
	}
	rsExpect(t, rt, rsAllPresent)
	if n := enf.count(); n != 0 {
		t.Errorf("a mismatched delete issued %d delete calls", n)
	}
}

// The UID precondition is the real guard: the pod read by Get is run A's,
// but it is replaced (by run B) before the delete. The newer pod survives.
func TestK8sDeleteRun_PodRecreatedBetweenGetAndDelete_Survives(t *testing.T) {
	rt, cs, _, _ := newRunScopeRuntime(t)
	rsSeedPod(t, rt, "pod-a", rsLabels(rsRunA, "start-a"), corev1.PodRunning)
	// Run B's objects, already in place when its pod replaces run A's.
	rsSeedSecret(t, rt, rsAgentSecret, "sec-b", rsLabels(rsRunB, "start-b"))
	rsSeedSecret(t, rt, rsAuthSecret, "auth-b", rsLabels(rsRunB, "start-b"))
	rsSeedSPC(t, rt, "spc-b", rsLabels(rsRunB, "start-b"))

	podGVR := corev1.SchemeGroupVersion.WithResource("pods")
	var once sync.Once
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		obj, err := cs.Tracker().Get(podGVR, action.GetNamespace(), rsAgent)
		if err != nil {
			return true, nil, err
		}
		var swapped bool
		once.Do(func() {
			swapped = true
			_ = cs.Tracker().Delete(podGVR, action.GetNamespace(), rsAgent)
			_ = cs.Tracker().Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: rsAgent, Namespace: action.GetNamespace(), UID: "pod-b", Labels: rsLabels(rsRunB, "start-b"),
			}})
		})
		if swapped {
			return true, obj, nil // the caller sees run A's pod as it was
		}
		return false, nil, nil
	})

	err := rt.Delete(context.Background(), RunRef{ID: rt.DefaultNamespace + "/" + rsAgent, RunID: rsRunA})
	if !errors.Is(err, ErrRunMismatch) {
		t.Fatalf("Delete error = %v, want ErrRunMismatch", err)
	}
	p := rsPod(t, rt)
	if p == nil || p.UID != "pod-b" {
		t.Fatalf("the recreated pod was deleted (pod now %+v)", p)
	}
	rsExpect(t, rt, rsAllPresent)
}

// A legacy pod (created before run IDs, carrying only scion.start_id) is
// still deleted by a delete naming a run, with its start's Secrets.
func TestK8sDeleteRun_LegacyStartIDOnlyPod_Deleted(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	rsSeedRun(t, rt, rsLabels("", "start-legacy"), corev1.PodRunning, "legacy")

	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rsExpect(t, rt, rsAllGone)
	enf.assertAllConditional(t)
}

// A legacy pod's delete never removes a Secret labelled with a run.
func TestK8sDeleteRun_LegacyPod_LeavesRunLabelledSecrets(t *testing.T) {
	rt, _, _, _ := newRunScopeRuntime(t)
	rsSeedPod(t, rt, "pod-legacy", rsLabels("", "start-legacy"), corev1.PodRunning)
	rsSeedSecret(t, rt, rsAgentSecret, "sec-b", rsLabels(rsRunB, "start-legacy"))
	rsSeedSecret(t, rt, rsAuthSecret, "auth-legacy", rsLabels("", "start-legacy"))

	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rsExpect(t, rt, rsState{agentSecret: true})
}

// Without a run ID, Delete behaves as before: pod and per-agent objects by
// name, whatever run they carry.
func TestK8sDelete_EmptyRunID_DeletesByName(t *testing.T) {
	rt, _, _, _ := newRunScopeRuntime(t)
	rsSeedRun(t, rt, rsLabels(rsRunB, "start-b"), corev1.PodRunning, "b")

	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rsExpect(t, rt, rsAllGone)
}

// The pod is already gone: only objects labelled with the run are removed.
func TestK8sDeleteRun_PodGone_RemovesOnlySameRunSecrets(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	rsSeedSecret(t, rt, rsAgentSecret, "sec-a", rsLabels(rsRunA, ""))
	rsSeedSecret(t, rt, rsAuthSecret, "auth-b", rsLabels(rsRunB, ""))
	rsSeedSPC(t, rt, "spc-a", rsLabels(rsRunA, ""))

	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rsExpect(t, rt, rsState{authSecret: true})
	enf.assertAllConditional(t)
}

// --- Run pre-clean ---

func rsRunConfig(runID string) RunConfig {
	cfg := startCleanupConfig()
	cfg.Labels = rsLabels(runID, "")
	return cfg
}

// A live (Running or Pending) pod of another run makes Run fail before it
// deletes or creates anything.
func TestK8sRun_PreClean_LiveOtherRunPod_ConflictsWithoutDeleting(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodPending} {
		t.Run(string(phase), func(t *testing.T) {
			rt, cs, _, enf := newRunScopeRuntime(t)
			rsSeedRun(t, rt, rsLabels(rsRunB, "start-b"), phase, "b")
			created := 0
			cs.PrependReactor("create", "*", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				created++
				return false, nil, nil
			})

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := rt.Run(ctx, rsRunConfig(rsRunA))
			if !errors.Is(err, ErrRunConflict) {
				t.Fatalf("Run error = %v, want ErrRunConflict", err)
			}
			rsExpect(t, rt, rsAllPresent)
			if p := rsPod(t, rt); p.UID != "pod-b" {
				t.Errorf("pod replaced: uid %s", p.UID)
			}
			if n := enf.count(); n != 0 {
				t.Errorf("Run issued %d deletes despite the conflict", n)
			}
			if created != 0 {
				t.Errorf("Run created %d objects despite the conflict", created)
			}
		})
	}
}

// A finished pod of another run, its objects, and a legacy pod are stale:
// pre-clean removes them, each with a UID precondition.
func TestK8sPreCleanForRun_RemovesStaleObjects(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		phase  corev1.PodPhase
	}{
		{"finished other run", rsLabels(rsRunB, "start-b"), corev1.PodSucceeded},
		{"failed other run", rsLabels(rsRunB, "start-b"), corev1.PodFailed},
		{"legacy live pod", rsLabels("", "start-legacy"), corev1.PodRunning},
		{"same run retry", rsLabels(rsRunA, "start-a0"), corev1.PodRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _, enf := newRunScopeRuntime(t)
			rsSeedRun(t, rt, tc.labels, tc.phase, "old")
			if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA); err != nil {
				t.Fatalf("preCleanForRun: %v", err)
			}
			want := rsAllGone
			if tc.labels[api.LabelRunID] == rsRunA {
				// Pre-clean leaves the run's own objects; the creates
				// replace them (replaceExistingAgentObject).
				want = rsState{agentSecret: true, authSecret: true, spc: true}
			}
			rsExpect(t, rt, want)
			enf.assertAllConditional(t)
		})
	}
}

// Leftover Secrets of a non-live other run are removed even with no pod.
func TestK8sPreCleanForRun_NoPod_RemovesOtherRunSecrets(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	rsSeedSecret(t, rt, rsAgentSecret, "sec-b", rsLabels(rsRunB, ""))
	rsSeedSecret(t, rt, rsAuthSecret, "auth-legacy", rsLabels("", ""))
	rsSeedSPC(t, rt, "spc-b", rsLabels(rsRunB, ""))

	if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA); err != nil {
		t.Fatalf("preCleanForRun: %v", err)
	}
	rsExpect(t, rt, rsAllGone)
	enf.assertAllConditional(t)
}

// A pod read failure fails the start rather than deleting blind.
func TestK8sPreCleanForRun_PodReadError_FailsClosed(t *testing.T) {
	rt, cs, _, enf := newRunScopeRuntime(t)
	rsSeedRun(t, rt, rsLabels(rsRunB, "start-b"), corev1.PodSucceeded, "b")
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, fmt.Errorf("simulated API failure")
	})
	if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA); err == nil {
		t.Fatal("preCleanForRun succeeded despite the pod read failure")
	}
	if n := enf.count(); n != 0 {
		t.Errorf("issued %d deletes after a failed pod read", n)
	}
}

// A start without a run ID keeps the name-based pre-clean.
func TestK8sRun_NoRunID_PreCleansByName(t *testing.T) {
	rt, cs, _, enf := newRunScopeRuntime(t)
	rsSeedRun(t, rt, rsLabels(rsRunB, "start-b"), corev1.PodRunning, "b")
	pod := runUntilPodSubmitted(t, rt, cs, startCleanupConfig())
	if pod.Labels[api.LabelRunID] != "" {
		t.Fatalf("unexpected run label on the new pod")
	}
	enf.mu.Lock()
	defer enf.mu.Unlock()
	if len(enf.unconditional) == 0 {
		t.Error("expected the name-based pre-clean deletes")
	}
}

// --- AlreadyExists on the three create sites ---

type rsCreateSite struct {
	name   string
	create func(rt *KubernetesRuntime, labels map[string]string) error
	seed   func(t *testing.T, rt *KubernetesRuntime, uid string, labels map[string]string)
	exists func(t *testing.T, rt *KubernetesRuntime) (bool, map[string]string)
}

func rsCreateSites() []rsCreateSite {
	secrets := []api.ResolvedSecret{{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "v", Source: "user", Ref: "projects/p/secrets/k"}}
	ctx := context.Background()
	secretState := func(name string) func(t *testing.T, rt *KubernetesRuntime) (bool, map[string]string) {
		return func(t *testing.T, rt *KubernetesRuntime) (bool, map[string]string) {
			s, err := rt.Client.Clientset.CoreV1().Secrets(rt.DefaultNamespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return true, s.Labels
		}
	}
	return []rsCreateSite{
		{
			name: "agent secret",
			create: func(rt *KubernetesRuntime, labels map[string]string) error {
				_, err := rt.createAgentSecret(ctx, rt.DefaultNamespace, rsAgent, secrets, labels)
				return err
			},
			seed: func(t *testing.T, rt *KubernetesRuntime, uid string, labels map[string]string) {
				rsSeedSecret(t, rt, rsAgentSecret, uid, labels)
			},
			exists: secretState(rsAgentSecret),
		},
		{
			name: "auth secret",
			create: func(rt *KubernetesRuntime, labels map[string]string) error {
				return rt.createAuthFileSecret(ctx, rt.DefaultNamespace, rsAgent, nil, labels)
			},
			seed: func(t *testing.T, rt *KubernetesRuntime, uid string, labels map[string]string) {
				rsSeedSecret(t, rt, rsAuthSecret, uid, labels)
			},
			exists: secretState(rsAuthSecret),
		},
		{
			name: "secret provider class",
			create: func(rt *KubernetesRuntime, labels map[string]string) error {
				_, err := rt.createSecretProviderClass(ctx, rt.DefaultNamespace, rsAgent, secrets, labels)
				return err
			},
			seed: func(t *testing.T, rt *KubernetesRuntime, uid string, labels map[string]string) {
				rsSeedSPC(t, rt, uid, labels)
			},
			exists: func(t *testing.T, rt *KubernetesRuntime) (bool, map[string]string) {
				o, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(rt.DefaultNamespace).Get(ctx, rsSPC, metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				return true, o.GetLabels()
			},
		},
	}
}

// An existing object of another run (created by a concurrent start after
// this start's pre-clean) is never deleted: the create fails with
// ErrRunConflict.
func TestK8sCreate_AlreadyExistsOtherRun_Conflicts(t *testing.T) {
	for _, site := range rsCreateSites() {
		t.Run(site.name, func(t *testing.T) {
			rt, _, _, enf := newRunScopeRuntime(t)
			site.seed(t, rt, "uid-b", rsLabels(rsRunB, ""))
			err := site.create(rt, rsLabels(rsRunA, ""))
			if !errors.Is(err, ErrRunConflict) {
				t.Fatalf("create error = %v, want ErrRunConflict", err)
			}
			ok, labels := site.exists(t, rt)
			if !ok || labels[api.LabelRunID] != rsRunB {
				t.Errorf("run B's object was replaced (present=%v labels=%v)", ok, labels)
			}
			if n := enf.count(); n != 0 {
				t.Errorf("issued %d deletes", n)
			}
		})
	}
}

// An existing object of the same run or a legacy one is replaced, with a
// UID precondition.
func TestK8sCreate_AlreadyExistsSameRunOrLegacy_Replaced(t *testing.T) {
	for _, site := range rsCreateSites() {
		for _, existingRun := range []string{rsRunA, ""} {
			t.Run(site.name+"/existing run "+existingRun, func(t *testing.T) {
				rt, _, _, enf := newRunScopeRuntime(t)
				site.seed(t, rt, "uid-old", rsLabels(existingRun, "old-start"))
				if err := site.create(rt, rsLabels(rsRunA, "new-start")); err != nil {
					t.Fatalf("create: %v", err)
				}
				ok, labels := site.exists(t, rt)
				if !ok || labels[labelStartID] != "new-start" {
					t.Errorf("object not replaced (present=%v labels=%v)", ok, labels)
				}
				enf.assertAllConditional(t)
			})
		}
	}
}

// Without a run ID the AlreadyExists path deletes by name, as before.
func TestK8sCreate_AlreadyExistsNoRunID_ReplacesByName(t *testing.T) {
	for _, site := range rsCreateSites() {
		t.Run(site.name, func(t *testing.T) {
			rt, _, _, _ := newRunScopeRuntime(t)
			site.seed(t, rt, "uid-b", rsLabels(rsRunB, ""))
			if err := site.create(rt, rsLabels("", "new-start")); err != nil {
				t.Fatalf("create: %v", err)
			}
			ok, labels := site.exists(t, rt)
			if !ok || labels[labelStartID] != "new-start" {
				t.Errorf("object not replaced (present=%v labels=%v)", ok, labels)
			}
		})
	}
}

func TestK8sRunMatches(t *testing.T) {
	cases := []struct {
		obj, run string
		want     bool
	}{
		{rsRunA, rsRunA, true},
		{"", rsRunA, true},
		{rsRunB, rsRunA, false},
	}
	for _, tc := range cases {
		if got := k8sRunMatches(tc.obj, tc.run); got != tc.want {
			t.Errorf("k8sRunMatches(%q, %q) = %v, want %v", tc.obj, tc.run, got, tc.want)
		}
	}
}

// End to end through Run: a finished pod of another run and its objects are
// cleared, and the new run's pod is submitted with the new run's label.
func TestK8sRun_PreClean_FinishedOtherRun_StartsNewRun(t *testing.T) {
	rt, cs, _, enf := newRunScopeRuntime(t)
	rsSeedRun(t, rt, rsLabels(rsRunB, "start-b"), corev1.PodSucceeded, "b")

	var mu sync.Mutex
	var submitted *corev1.Pod
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if submitted != nil {
			// Fail readiness once the pod exists (see failPodReadiness).
			return true, nil, fmt.Errorf("simulated readiness failure")
		}
		return false, nil, nil
	})
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		submitted = action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		mu.Unlock()
		return false, nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = rt.Run(ctx, rsRunConfig(rsRunA))

	mu.Lock()
	defer mu.Unlock()
	if submitted == nil {
		t.Fatal("Run did not submit a pod")
	}
	if got := submitted.Labels[api.LabelRunID]; got != rsRunA {
		t.Errorf("new pod run label = %q, want %q", got, rsRunA)
	}
	for _, name := range []string{rsAgentSecret, rsAuthSecret} {
		s, err := cs.CoreV1().Secrets(rt.DefaultNamespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil || s.Labels[api.LabelRunID] != rsRunA {
			t.Errorf("Secret %s not recreated for run A (err=%v)", name, err)
		}
	}
	enf.assertAllConditional(t)
}

// A very old pod with neither a run nor a start label: its unlabelled
// objects are removed, but never one labelled with a run.
func TestK8sDeleteRun_LegacyPodWithoutStartID_LeavesRunLabelledSecrets(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	rsSeedPod(t, rt, "pod-old", rsLabels("", ""), corev1.PodRunning)
	rsSeedSecret(t, rt, rsAgentSecret, "sec-b", rsLabels(rsRunB, ""))
	rsSeedSecret(t, rt, rsAuthSecret, "auth-old", rsLabels("", ""))
	rsSeedSPC(t, rt, "spc-old", rsLabels("", ""))

	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rsExpect(t, rt, rsState{agentSecret: true})
	enf.assertAllConditional(t)
}

// A terminating pod of another run is not live: pre-clean force-deletes it,
// with a UID precondition (today's behaviour for a terminating pod).
func TestK8sPreCleanForRun_TerminatingOtherRunPod_ForceDeleted(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	now := metav1.Now()
	_, err := rt.Client.Clientset.CoreV1().Pods(rt.DefaultNamespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: rsAgent, Namespace: rt.DefaultNamespace, UID: "pod-b",
			Labels: rsLabels(rsRunB, "start-b"), DeletionTimestamp: &now, Finalizers: []string{"example.com/hold"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed pod: %v", err)
	}
	if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA); err != nil {
		t.Fatalf("preCleanForRun: %v", err)
	}
	if rsPod(t, rt) != nil {
		t.Error("terminating pod of another run was not deleted")
	}
	enf.assertAllConditional(t)
}

// A pre-clean pod delete that fails its UID precondition (the pod was
// recreated by a concurrent start) fails the start with ErrRunConflict.
func TestK8sPreCleanForRun_PodDeleteConflict_RunConflict(t *testing.T) {
	rt, cs, _, _ := newRunScopeRuntime(t)
	rsSeedPod(t, rt, "pod-b", rsLabels(rsRunB, "start-b"), corev1.PodSucceeded)
	cs.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewConflict(schema.GroupResource{Resource: "pods"}, rsAgent, fmt.Errorf("precondition failed"))
	})
	err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA)
	if !errors.Is(err, ErrRunConflict) {
		t.Fatalf("preCleanForRun error = %v, want ErrRunConflict", err)
	}
}

// An AlreadyExists replace whose delete fails its UID precondition (the
// object was recreated by a concurrent start) fails with ErrRunConflict,
// on all three create sites.
func TestK8sCreate_AlreadyExistsReplaceConflict_RunConflict(t *testing.T) {
	conflict := func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewConflict(schema.GroupResource{Resource: action.GetResource().Resource}, "x", fmt.Errorf("precondition failed"))
	}
	for _, site := range rsCreateSites() {
		t.Run(site.name, func(t *testing.T) {
			rt, cs, dyn, _ := newRunScopeRuntime(t)
			site.seed(t, rt, "uid-old", rsLabels(rsRunA, "old-start"))
			cs.PrependReactor("delete", "secrets", conflict)
			dyn.PrependReactor("delete", "secretproviderclasses", conflict)
			err := site.create(rt, rsLabels(rsRunA, "new-start"))
			if !errors.Is(err, ErrRunConflict) {
				t.Fatalf("create error = %v, want ErrRunConflict", err)
			}
		})
	}
}

// Regression (P2 review round 2, B1): stale run-labelled objects of the same
// name but an old project ID (a project recreated under the same name) and
// no pod are removed by pre-clean, and the start goes on to create its pod.
// Skipping them would make every retry fail with ErrRunConflict.
func TestK8sRun_PreClean_StaleOtherProjectRunObjects_Cleaned(t *testing.T) {
	rt, cs, _, enf := newRunScopeRuntime(t)
	stale := map[string]string{
		"scion.agent": "true", "scion.name": "agent", "scion.project_id": "proj-old", api.LabelRunID: rsRunB,
	}
	rsSeedSecret(t, rt, rsAgentSecret, "sec-stale", stale)
	rsSeedSecret(t, rt, rsAuthSecret, "auth-stale", stale)
	rsSeedSPC(t, rt, "spc-stale", stale)
	pod := runUntilPodSubmittedLate(t, rt, cs, rsRunConfig(rsRunA))
	if got := pod.Labels[api.LabelRunID]; got != rsRunA {
		t.Errorf("new pod run label = %q, want %q", got, rsRunA)
	}
	for _, name := range []string{rsAgentSecret, rsAuthSecret} {
		s, err := cs.CoreV1().Secrets(rt.DefaultNamespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil || s.Labels[api.LabelRunID] != rsRunA {
			t.Errorf("Secret %s not recreated for run A (err=%v)", name, err)
		}
	}
	enf.assertAllConditional(t)
}

// runUntilPodSubmittedLate drives Run until it submits its pod, failing
// pod reads only after that (pre-clean's own pod read must succeed, unlike
// failPodReadiness). Returns the submitted pod.
func runUntilPodSubmittedLate(t *testing.T, rt *KubernetesRuntime, cs *k8sfake.Clientset, cfg RunConfig) *corev1.Pod {
	t.Helper()
	var mu sync.Mutex
	var submitted *corev1.Pod
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if submitted != nil {
			return true, nil, fmt.Errorf("simulated readiness failure")
		}
		return false, nil, nil
	})
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		submitted = action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		mu.Unlock()
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = rt.Run(ctx, cfg)
	mu.Lock()
	defer mu.Unlock()
	if submitted == nil {
		t.Fatal("Run did not submit a pod")
	}
	return submitted
}

// Delete honours the namespace in a namespace/pod ref: only the objects in
// that namespace are removed, never same-named ones in the default
// namespace.
func TestK8sDeleteRun_NamespacedRef_DeletesOnlyInThatNamespace(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	const otherNS = "other-ns"
	ctx := context.Background()
	labels := rsLabels(rsRunA, "start-a")
	// The same run's objects exist under the same names in both namespaces.
	rsSeedRun(t, rt, labels, corev1.PodRunning, "default")
	if _, err := rt.Client.Clientset.CoreV1().Pods(otherNS).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: rsAgent, Namespace: otherNS, UID: "pod-other", Labels: labels},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{rsAgentSecret, rsAuthSecret} {
		if _, err := rt.Client.Clientset.CoreV1().Secrets(otherNS).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: otherNS, UID: types.UID("other-" + n), Labels: labels},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	spc := &unstructured.Unstructured{}
	spc.SetGroupVersionKind(schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClass"})
	spc.SetName(rsSPC)
	spc.SetNamespace(otherNS)
	spc.SetUID("spc-other")
	spc.SetLabels(labels)
	if _, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(otherNS).Create(ctx, spc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := rt.Delete(ctx, RunRef{ID: otherNS + "/" + rsAgent, RunID: rsRunA}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := rt.Client.Clientset.CoreV1().Pods(otherNS).Get(ctx, rsAgent, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("pod in %s not deleted (err=%v)", otherNS, err)
	}
	for _, n := range []string{rsAgentSecret, rsAuthSecret} {
		if secretExists(t, rt, otherNS, n) {
			t.Errorf("Secret %s/%s not deleted", otherNS, n)
		}
	}
	if spcExists(t, rt, otherNS, rsSPC) {
		t.Errorf("SPC in %s not deleted", otherNS)
	}
	rsExpect(t, rt, rsAllPresent) // default namespace untouched
	enf.assertAllConditional(t)
}

// A malformed run ID never reaches a label selector: Delete and pre-clean
// refuse it and delete nothing.
func TestK8sRunScope_InvalidRunID_Refused(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	rsSeedRun(t, rt, rsLabels(rsRunB, "start-b"), corev1.PodSucceeded, "b")
	bad := rsRunA + ",scion.agent"
	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: bad}); err == nil {
		t.Error("Delete accepted a malformed run ID")
	}
	if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, bad); err == nil {
		t.Error("preCleanForRun accepted a malformed run ID")
	}
	rsExpect(t, rt, rsAllPresent)
	if n := enf.count(); n != 0 {
		t.Errorf("issued %d deletes for a malformed run ID", n)
	}
}

// With the pod gone, Delete's selector would be built from the run ID
// alone: a malformed one must be refused before any list or delete.
func TestK8sDeleteRun_PodGone_InvalidRunID_Refused(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	rsSeedSecret(t, rt, rsAgentSecret, "sec-a", rsLabels(rsRunA, ""))
	rsSeedSecret(t, rt, rsAuthSecret, "auth-a", rsLabels(rsRunA, ""))
	rsSeedSPC(t, rt, "spc-a", rsLabels(rsRunA, ""))
	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA + ",scion.agent"}); err == nil {
		t.Error("Delete accepted a malformed run ID")
	}
	rsExpect(t, rt, rsState{agentSecret: true, authSecret: true, spc: true})
	if n := enf.count(); n != 0 {
		t.Errorf("issued %d deletes for a malformed run ID", n)
	}
}

// A pre-clean pod delete that fails for any reason other than NotFound or
// Conflict (here Forbidden) fails the start.
func TestK8sPreCleanForRun_PodDeleteError_FailsStart(t *testing.T) {
	rt, cs, _, _ := newRunScopeRuntime(t)
	rsSeedPod(t, rt, "pod-b", rsLabels(rsRunB, "start-b"), corev1.PodSucceeded)
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, rsAgent, fmt.Errorf("rbac"))
	})
	err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA)
	if err == nil || !k8serrors.IsForbidden(err) {
		t.Fatalf("preCleanForRun error = %v, want the Forbidden error", err)
	}
	if errors.Is(err, ErrRunConflict) {
		t.Errorf("a delete failure is not a run conflict: %v", err)
	}
}
