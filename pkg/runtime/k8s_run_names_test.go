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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	k8stesting "k8s.io/client-go/testing"
)

// Tests for per-run names of the per-agent Secrets and SecretProviderClass
// (ptone/scion#3101).

// An empty run ID keeps today's fixed names exactly.
func TestK8sAgentObjectNames_EmptyRunID_FixedNames(t *testing.T) {
	n := k8sAgentObjectNames(rsAgent, "")
	if n.Secret != "scion-agent-"+rsAgent || n.SPC != "scion-agent-"+rsAgent || n.Auth != "scion-auth-"+rsAgent {
		t.Errorf("names = %+v, want the fixed scion-agent-/scion-auth- names", n)
	}
}

// A run ID gives names that differ per run, are valid DNS-1123 subdomains
// of at most 253 characters, and never share a prefix with a fixed name.
func TestK8sAgentObjectNames_PerRun(t *testing.T) {
	a := k8sAgentObjectNames(rsAgent, rsRunA)
	b := k8sAgentObjectNames(rsAgent, rsRunB)
	if a.Secret == b.Secret || a.Auth == b.Auth || a.SPC == b.SPC {
		t.Fatalf("runs share a name: A=%+v B=%+v", a, b)
	}
	if a != k8sAgentObjectNames(rsAgent, rsRunA) {
		t.Error("names are not deterministic")
	}
	for _, name := range []string{a.Secret, a.Auth, a.SPC} {
		assertValidObjectName(t, name)
		if strings.HasPrefix(name, agentSecretPrefix) || strings.HasPrefix(name, agentAuthSecretPrefix) {
			t.Errorf("per-run name %q uses a fixed-name prefix", name)
		}
		if _, ok := podNameForAgentObject(name); ok {
			t.Errorf("per-run name %q parses as a legacy name", name)
		}
	}
}

// Run IDs that are valid label values but not DNS-1123 (capitals, "_",
// ".") still give valid names.
func TestK8sAgentObjectNames_LabelValidRunIDs(t *testing.T) {
	for _, run := range []string{"Run_A.1", "X", strings.Repeat("z", 63)} {
		n := k8sAgentObjectNames(rsAgent, run)
		assertValidObjectName(t, n.Secret)
		assertValidObjectName(t, n.Auth)
	}
}

// A long pod name is truncated with a hash: the names stay within 253
// characters and valid, and two long pod names sharing the kept prefix,
// or one long pod name under two runs, never share a name.
func TestK8sAgentObjectNames_LongPodName(t *testing.T) {
	base := strings.Repeat("a", 240)
	pod1 := base + "-one.x"
	pod2 := base + "-two.x"
	dotted := strings.Repeat("b", 218) + "." + strings.Repeat("c", 30) // truncation lands on "."
	seen := map[string]string{}
	for _, pod := range []string{pod1, pod2, dotted} {
		for _, run := range []string{rsRunA, rsRunB} {
			n := k8sAgentObjectNames(pod, run)
			for _, name := range []string{n.Secret, n.Auth} {
				assertValidObjectName(t, name)
				if prev, dup := seen[name]; dup {
					t.Errorf("name %q shared by %s and %s/%s", name, prev, pod, run)
				}
				seen[name] = pod + "/" + run
			}
		}
	}
}

func assertValidObjectName(t *testing.T, name string) {
	t.Helper()
	if len(name) > 253 {
		t.Errorf("name %q is %d characters, over 253", name, len(name))
	}
	if errs := k8svalidation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Errorf("name %q is not a DNS-1123 subdomain: %v", name, errs)
	}
}

// Run B's pre-clean, while run A has created its per-run Secrets and SPC
// but has no pod yet, leaves A's objects, and the reverse.
func TestK8sPreCleanForRun_OverlappingRuns_SpareEachOthersPerRunObjects(t *testing.T) {
	for _, tc := range []struct{ first, second string }{{rsRunA, rsRunB}, {rsRunB, rsRunA}} {
		rt, _, _, enf := newRunScopeRuntime(t)
		n := k8sAgentObjectNames(rsAgent, tc.first)
		labels := rsLabels(tc.first, "start-first")
		rsSeedSecret(t, rt, n.Secret, "sec-first", labels)
		rsSeedSecret(t, rt, n.Auth, "auth-first", labels)
		rsSeedSPCNamed(t, rt, n.SPC, "spc-first", labels)

		if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, tc.second, false, nil); err != nil {
			t.Fatalf("preCleanForRun(%s): %v", tc.second, err)
		}
		ns := rt.DefaultNamespace
		if !secretExists(t, rt, ns, n.Secret) || !secretExists(t, rt, ns, n.Auth) || !spcExists(t, rt, ns, n.SPC) {
			t.Errorf("pre-clean of run %s removed run %s's per-run objects", tc.second, tc.first)
		}
		enf.assertAllConditional(t)
	}
}

// Each run's pod references its own per-run objects, and only the secret
// references differ from a start without a run ID: volume, mount and env
// var names stay fixed. Plain, GKE secrets and NFS-home pods.
func TestBuildPod_RunID_OnlySecretReferencesChange(t *testing.T) {
	plain := func() RunConfig {
		cfg := rsRunConfig("")
		cfg.ResolvedSecrets = []api.ResolvedSecret{
			{Name: "TOKEN", Type: "environment", Target: "TOKEN", Value: "v"},
			{Name: "cfg", Type: "file", Target: "/home/scion/cfg", Value: "dg=="},
		}
		cfg.ResolvedAuth = &api.ResolvedAuth{Files: []api.FileMapping{{SourcePath: "/dev/null", ContainerPath: "/home/scion/.auth"}}}
		return cfg
	}
	gke := func() RunConfig {
		cfg := plain()
		cfg.ResolvedSecrets = []api.ResolvedSecret{
			{Name: "TOKEN", Type: "environment", Target: "TOKEN", Value: "v", Ref: "projects/p/secrets/t"},
			{Name: "cfg", Type: "file", Target: "/home/scion/cfg", Value: "dg==", Ref: "projects/p/secrets/c"},
		}
		return cfg
	}
	nfs := func() RunConfig {
		cfg := nfsHomeTestConfig(true)
		cfg.ResolvedAuth = &api.ResolvedAuth{Files: []api.FileMapping{{SourcePath: "/dev/null", ContainerPath: "/home/scion/.auth"}}}
		return cfg
	}
	for _, tc := range []struct {
		name string
		gke  bool
		cfg  func() RunConfig
	}{{"plain", false, plain}, {"gke", true, gke}, {"nfs-home", false, nfs}} {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _ := newGKECleanupTestRuntime(t)
			rt.GKEMode = tc.gke
			cfg := tc.cfg()
			legacy, err := rt.buildPod(rt.DefaultNamespace, cfg)
			if err != nil {
				t.Fatalf("buildPod (no run): %v", err)
			}
			labels := map[string]string{}
			for k, v := range cfg.Labels {
				labels[k] = v
			}
			labels[api.LabelRunID] = rsRunA
			cfg.Labels = labels
			perRun, err := rt.buildPod(rt.DefaultNamespace, cfg)
			if err != nil {
				t.Fatalf("buildPod (run A): %v", err)
			}
			want := k8sAgentObjectNames(cfg.Name, rsRunA)
			refs := podSecretRefs(perRun)
			if len(refs) == 0 {
				t.Fatal("pod references no Secret")
			}
			sawAuth := false
			for _, ref := range refs {
				if ref != want.Secret && ref != want.Auth && ref != want.SPC {
					t.Errorf("run A's pod references %q, not one of its own objects %+v", ref, want)
				}
				sawAuth = sawAuth || ref == want.Auth
			}
			if !sawAuth {
				t.Errorf("run A's pod does not reference its auth Secret %q (refs %v)", want.Auth, refs)
			}
			fixed := k8sAgentObjectNames(cfg.Name, "")
			for _, ref := range podSecretRefs(legacy) {
				if ref != fixed.Secret && ref != fixed.Auth && ref != fixed.SPC {
					t.Errorf("no-run pod references %q, not a fixed name", ref)
				}
			}
			if got, want := podShapeNames(perRun), podShapeNames(legacy); got != want {
				t.Errorf("volume/env names differ with a run ID:\n got  %s\n want %s", got, want)
			}
		})
	}
}

// podSecretRefs returns every Secret and SecretProviderClass name the pod
// references.
func podSecretRefs(pod *corev1.Pod) []string {
	var refs []string
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil {
			refs = append(refs, v.Secret.SecretName)
		}
		if v.CSI != nil {
			refs = append(refs, v.CSI.VolumeAttributes["secretProviderClass"])
		}
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.Secret != nil {
					refs = append(refs, s.Secret.Name)
				}
			}
		}
	}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				refs = append(refs, e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	return refs
}

// podShapeNames joins the pod's volume, mount and env var names.
func podShapeNames(pod *corev1.Pod) string {
	var b strings.Builder
	for _, v := range pod.Spec.Volumes {
		b.WriteString("vol:" + v.Name + " ")
	}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, m := range c.VolumeMounts {
			b.WriteString("mnt:" + c.Name + ":" + m.Name + ":" + m.MountPath + ":" + m.SubPath + " ")
		}
		for _, e := range c.Env {
			b.WriteString("env:" + c.Name + ":" + e.Name + " ")
		}
	}
	return b.String()
}

// Upgrade mid-run: a pod created by an older broker with fixed-name
// objects keeps them while it exists, whatever another run's cleanup does,
// and they are removed once its own run is deleted (or, for a pod with no
// run label, by a no-run delete), and the leftover cleanup after the pod is
// gone removes nothing more of it and fails nothing.
func TestUpgradeMidRun_FixedNamePodKeepsObjectsUntilStopped(t *testing.T) {
	ctx := context.Background()
	t.Run("run-labelled fixed-name pod", func(t *testing.T) {
		rt, _, _, enf := newRunScopeRuntime(t)
		rsSeedRun(t, rt, rsLabels(rsRunA, "start-a"), corev1.PodRunning, "a")

		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", rsRunB); err != nil {
			t.Fatalf("CleanupAgentResources(B): %v", err)
		}
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", ""); err != nil {
			t.Fatalf("CleanupAgentResources(no run): %v", err)
		}
		if err := rt.preCleanForRun(ctx, rt.DefaultNamespace, rsAgent, rsRunB, false, nil); err == nil {
			t.Fatal("pre-clean of run B succeeded against run A's live pod")
		}
		rsExpect(t, rt, rsAllPresent)

		if err := rt.Delete(ctx, RunRef{ID: rsAgent, RunID: rsRunA}); err != nil {
			t.Fatalf("Delete(A): %v", err)
		}
		rsExpect(t, rt, rsAllGone)
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", ""); err != nil {
			t.Fatalf("CleanupAgentResources(no run) after the pod is gone: %v", err)
		}
		enf.assertAllConditional(t)
	})
	t.Run("unlabelled fixed-name pod, objects left after the pod is gone", func(t *testing.T) {
		rt, cs, _, _ := newRunScopeRuntime(t)
		rsSeedRun(t, rt, rsLabels("", "start-old"), corev1.PodRunning, "old")

		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", rsRunB); err != nil {
			t.Fatalf("CleanupAgentResources(B): %v", err)
		}
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", ""); err != nil {
			t.Fatalf("CleanupAgentResources(no run): %v", err)
		}
		rsExpect(t, rt, rsAllPresent)

		// The pod goes away outside scion: its fixed-name objects are left
		// until the leftover cleanup (no run) removes them.
		if err := cs.CoreV1().Pods(rt.DefaultNamespace).Delete(ctx, rsAgent, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", ""); err != nil {
			t.Fatalf("CleanupAgentResources(no run) after the pod is gone: %v", err)
		}
		rsExpect(t, rt, rsAllGone)
	})
}

// --- helpers ---

// prSeed creates a per-agent Secret (kind "Secret") or SecretProviderClass
// (kind "SPC") with the given name, UID, labels, annotations and creation
// time in rt's default namespace.
func prSeed(t *testing.T, rt *KubernetesRuntime, kind, name, uid string, labels, ann map[string]string, created time.Time) {
	t.Helper()
	ctx := context.Background()
	ns := rt.DefaultNamespace
	meta := metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Labels: labels, Annotations: ann,
		CreationTimestamp: metav1.NewTime(created)}
	switch kind {
	case "Secret":
		if _, err := rt.Client.Clientset.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: meta}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed Secret %s: %v", name, err)
		}
	case "SPC":
		spc := &unstructured.Unstructured{}
		spc.SetGroupVersionKind(schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClass"})
		spc.SetName(name)
		spc.SetNamespace(ns)
		spc.SetUID(types.UID(uid))
		spc.SetLabels(labels)
		spc.SetAnnotations(ann)
		spc.SetCreationTimestamp(metav1.NewTime(created))
		if _, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(ns).Create(ctx, spc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed SPC %s: %v", name, err)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
}

// prAnn returns per-run annotations for pod rsAgent with the given deadline
// offset value ("" for none at all).
func prAnn(offset string) map[string]string {
	ann := map[string]string{annotationPodName: rsAgent}
	if offset != "" {
		ann[annotationStartDeadlineOffset] = offset
	}
	return ann
}

// prSeedRunObjects seeds run's per-run Secret, auth Secret and SPC for pod
// rsAgent, created at created with the deadline offset value offset.
func prSeedRunObjects(t *testing.T, rt *KubernetesRuntime, run, offset string, created time.Time) k8sObjectNames {
	t.Helper()
	n := k8sAgentObjectNames(rsAgent, run)
	labels := rsLabels(run, "start-"+run[:4])
	prSeed(t, rt, "Secret", n.Secret, "sec-"+run[:4], labels, prAnn(offset), created)
	prSeed(t, rt, "Secret", n.Auth, "auth-"+run[:4], labels, prAnn(offset), created)
	prSeed(t, rt, "SPC", n.SPC, "spc-"+run[:4], labels, prAnn(offset), created)
	return n
}

// prPresent reports which of n's objects exist.
func prPresent(t *testing.T, rt *KubernetesRuntime, n k8sObjectNames) [3]bool {
	t.Helper()
	ns := rt.DefaultNamespace
	return [3]bool{secretExists(t, rt, ns, n.Secret), secretExists(t, rt, ns, n.Auth), spcExists(t, rt, ns, n.SPC)}
}

var (
	prAll  = [3]bool{true, true, true}
	prNone = [3]bool{}
)

// prClock pins rt's wall clock at now.
func prClock(rt *KubernetesRuntime, now time.Time) {
	rt.nowFn = func() time.Time { return now }
}

var prNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// --- overlap and start cleanup ---

// Run B's start runs to its pod while run A's per-run objects exist with
// no pod: A's objects survive, and B's pod references only B's objects.
func TestK8sRun_OverlappingRuns_NewRunLeavesOtherRunsPerRunObjects(t *testing.T) {
	rt, cs, _, enf := newRunScopeRuntime(t)
	prClock(rt, prNow)
	a := prSeedRunObjects(t, rt, rsRunA, startDeadlineNone, prNow.Add(-time.Minute))

	pod := runUntilPodSubmittedLate(t, rt, cs, rsRunConfig(rsRunB))
	if got := prPresent(t, rt, a); got != prAll {
		t.Errorf("run A's per-run objects after run B's start = %v, want all present", got)
	}
	b := k8sAgentObjectNames(rsAgent, rsRunB)
	for _, ref := range podSecretRefs(pod) {
		if ref != b.Secret && ref != b.Auth && ref != b.SPC {
			t.Errorf("run B's pod references %q, not one of its own objects %+v", ref, b)
		}
	}
	if got := prPresent(t, rt, b); got != prAll {
		t.Errorf("run B's per-run objects = %v, want all created", got)
	}
	enf.assertAllConditional(t)
}

// The cleanup of run B's incomplete start (GoogleCloudPlatform/scion#2318)
// removes only B's per-run objects, never A's, and the reverse.
func TestK8sCleanupStartResources_PerRun_RemovesOnlyThisStartsObjects(t *testing.T) {
	for _, tc := range []struct{ mine, other string }{{rsRunB, rsRunA}, {rsRunA, rsRunB}} {
		rt, _, _, enf := newRunScopeRuntime(t)
		mine := prSeedRunObjects(t, rt, tc.mine, "300", prNow)
		other := prSeedRunObjects(t, rt, tc.other, "300", prNow)
		rt.cleanupStartResources(context.Background(), rt.DefaultNamespace, rsAgent, "start-"+tc.mine[:4], true)
		if got := prPresent(t, rt, mine); got != prNone {
			t.Errorf("run %s's own objects after its cleanup = %v, want none", tc.mine, got)
		}
		if got := prPresent(t, rt, other); got != prAll {
			t.Errorf("run %s's objects after run %s's cleanup = %v, want all", tc.other, tc.mine, got)
		}
		enf.assertAllConditional(t)
	}
}

// A start whose context ends at the pod-create checkpoint
// (GoogleCloudPlatform/scion#2318) removes its per-run objects on a
// detached context and creates no pod.
func TestRun_DeadlineAtPodCreateCheckpoint_RemovesPerRunObjectsNoPod(t *testing.T) {
	rt, cs, _, enf := newRunScopeRuntime(t)
	var podCreates int
	var mu sync.Mutex
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		podCreates++
		mu.Unlock()
		return false, nil, nil
	})
	cfg := rsRunConfig(rsRunA)
	cfg.Checkpoint = func(ctx context.Context, step string) error {
		if step != CheckpointStepPodCreate {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := rt.Run(ctx, cfg); err == nil {
		t.Fatal("Run succeeded past an expired deadline")
	}
	if got := prPresent(t, rt, k8sAgentObjectNames(rsAgent, rsRunA)); got != prNone {
		t.Errorf("per-run objects after the abandoned start = %v, want none", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if podCreates != 0 {
		t.Errorf("pod creates = %d, want 0", podCreates)
	}
	enf.assertAllConditional(t)
}

// --- verify before the pod create ---

// prVerifyRuntime returns a GKE-mode runtime whose fake clients assign
// UIDs on create, with a counter of the field-selector lists
// verifyStartObjects issues and of pod creates.
func prVerifyRuntime(t *testing.T) (*KubernetesRuntime, *verifyCounter) {
	t.Helper()
	rt, cs, dyn := newGKECleanupTestRuntime(t)
	var fx uidFixture
	fx.install(t, cs)
	fx.install(t, dyn)
	c := &verifyCounter{}
	count := func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if l, ok := action.(k8stesting.ListAction); ok && !l.GetListRestrictions().Fields.Empty() {
			c.mu.Lock()
			c.lists++
			c.mu.Unlock()
		}
		return false, nil, nil
	}
	cs.PrependReactor("list", "secrets", count)
	dyn.PrependReactor("list", "secretproviderclasses", count)
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		c.mu.Lock()
		c.podCreates++
		c.mu.Unlock()
		return true, nil, errors.New("stop after the pod create")
	})
	return rt, c
}

type verifyCounter struct {
	mu                sync.Mutex
	lists, podCreates int
}

func (c *verifyCounter) get() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists, c.podCreates
}

// A fast start makes no verification calls.
func TestRun_FastStart_NoVerificationCalls(t *testing.T) {
	rt, c := prVerifyRuntime(t)
	_, _ = rt.Run(context.Background(), rsRunConfig(rsRunA))
	lists, pods := c.get()
	if lists != 0 {
		t.Errorf("verification lists = %d, want 0", lists)
	}
	if pods != 1 {
		t.Errorf("pod creates = %d, want 1", pods)
	}
}

// A start past the verify trigger whose Secret was removed, or replaced
// with a new UID, fails with the fixed error and creates no pod.
func TestRun_PerRunObjectSweptBeforePodCreate_FailsWithoutPod(t *testing.T) {
	for _, mode := range []string{"removed", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			rt, c := prVerifyRuntime(t)
			rt.sinceFn = func(time.Time) time.Duration { return minStaleRunObjectAge - staleRunObjectMargin }
			cfg := rsRunConfig(rsRunA)
			name := k8sAgentObjectNames(rsAgent, rsRunA).Secret
			cfg.Checkpoint = func(ctx context.Context, step string) error {
				if step != CheckpointStepPodCreate {
					return nil
				}
				secrets := rt.Client.Clientset.CoreV1().Secrets(rt.DefaultNamespace)
				old, err := secrets.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
					t.Fatalf("delete: %v", err)
				}
				if mode == "replaced" {
					repl := old.DeepCopy()
					repl.ResourceVersion = ""
					if _, err := secrets.Create(ctx, repl, metav1.CreateOptions{}); err != nil {
						t.Fatalf("recreate: %v", err)
					}
				}
				return nil
			}
			_, err := rt.Run(context.Background(), cfg)
			if err == nil || err.Error() != errStartObjectsGone {
				t.Fatalf("Run error = %v, want %q", err, errStartObjectsGone)
			}
			if _, pods := c.get(); pods != 0 {
				t.Errorf("pod creates = %d, want 0", pods)
			}
		})
	}
}

// --- the stale sweep ---

func TestStaleRunObjectAge(t *testing.T) {
	for _, tc := range []struct {
		value string
		age   time.Duration
		ok    bool
	}{
		{startDeadlineNone, time.Hour, true},
		{"60", time.Hour, true},
		{"3600", time.Hour + 15*time.Minute, true},
		{"0", time.Hour, true},
		{"", 0, false},
		{"-5", 0, false},
		{"+5", 0, false},
		{"05", 0, false},
		{"1.5", 0, false},
		{"soon", 0, false},
		{"99999999999999", 0, false},
	} {
		age, ok := staleRunObjectAge(tc.value)
		if age != tc.age || ok != tc.ok {
			t.Errorf("staleRunObjectAge(%q) = %v, %v; want %v, %v", tc.value, age, ok, tc.age, tc.ok)
		}
	}
	if got := verifyStartObjectsAfter(startDeadlineNone); got != 45*time.Minute {
		t.Errorf("verify trigger with no deadline = %v, want 45m", got)
	}
	if got := verifyStartObjectsAfter("7200"); got != 2*time.Hour {
		t.Errorf("verify trigger with a 2h deadline = %v, want 2h", got)
	}
	if got := verifyStartObjectsAfter("600"); got != 45*time.Minute {
		t.Errorf("verify trigger with a 10m deadline = %v, want 45m", got)
	}
}

// Run A's pre-clean sweeps run B's per-run objects (no pod) only once they
// are stale; each case is one age rule.
func TestK8sPreCleanForRun_StaleSweep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset string
		age    time.Duration
		swept  bool
	}{
		{"young orphan, no deadline", startDeadlineNone, 30 * time.Minute, false},
		{"old orphan, no deadline", startDeadlineNone, 61 * time.Minute, true},
		{"deadline unexpired though older than 1h", "10800", 2 * time.Hour, false},
		{"deadline expired, older than 1h", "60", 2 * time.Hour, true},
		{"60s deadline passed but under the 1h floor", "60", 30 * time.Minute, false},
		{"just before deadline plus margin", "3600", time.Hour + 15*time.Minute - time.Second, false},
		{"just after deadline plus margin", "3600", time.Hour + 15*time.Minute + time.Second, true},
		{"exactly at deadline plus margin", "3600", time.Hour + 15*time.Minute, false},
		{"no deadline, exactly 1h", startDeadlineNone, time.Hour, false},
		{"malformed offset", "soon", 30 * 24 * time.Hour, false},
		{"no annotation", "", 30 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _, enf := newRunScopeRuntime(t)
			prClock(rt, prNow)
			b := prSeedRunObjects(t, rt, rsRunB, tc.offset, prNow.Add(-tc.age))
			if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA, false, nil); err != nil {
				t.Fatalf("preCleanForRun: %v", err)
			}
			want := prAll
			if tc.swept {
				want = prNone
			}
			if got := prPresent(t, rt, b); got != want {
				t.Errorf("run B's objects = %v, want %v", got, want)
			}
			enf.assertAllConditional(t)
		})
	}
}

// The sweep never removes an object recreated under the same name since
// the list (new UID: the delete's precondition fails).
func TestK8sPreCleanForRun_StaleSweep_RecreatedObjectSurvives(t *testing.T) {
	rt, cs, _, _ := newRunScopeRuntime(t)
	prClock(rt, prNow)
	b := prSeedRunObjects(t, rt, rsRunB, startDeadlineNone, prNow.Add(-2*time.Hour))
	// The list reports a stale UID: the stored object was recreated.
	cs.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		s, err := cs.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, rt.DefaultNamespace, b.Secret)
		if err != nil {
			return false, nil, nil
		}
		old := s.(*corev1.Secret).DeepCopy()
		old.UID = "uid-before-recreate"
		return true, &corev1.SecretList{Items: []corev1.Secret{*old}}, nil
	})
	if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA, false, nil); err != nil {
		t.Fatalf("preCleanForRun: %v", err)
	}
	if !secretExists(t, rt, rt.DefaultNamespace, b.Secret) {
		t.Error("a recreated object was removed by the sweep")
	}
}

// Pre-clean removes the per-run objects of the previous pod's run along
// with that (finished) pod, whatever their age.
func TestK8sPreCleanForRun_PreviousPodsPerRunObjectsRemoved(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	prClock(rt, prNow)
	rsSeedPod(t, rt, "pod-b", rsLabels(rsRunB, "start-b"), corev1.PodSucceeded)
	b := prSeedRunObjects(t, rt, rsRunB, "300", prNow)
	if err := rt.preCleanForRun(context.Background(), rt.DefaultNamespace, rsAgent, rsRunA, false, nil); err != nil {
		t.Fatalf("preCleanForRun: %v", err)
	}
	if got := prPresent(t, rt, b); got != prNone {
		t.Errorf("previous pod's run objects = %v, want none", got)
	}
	enf.assertAllConditional(t)
}

// CleanupAgentResources: the stale sweep with and without a run; never
// the named run, never a legacy fixed name by age, never a young object.
func TestCleanupAgentResources_StaleSweep(t *testing.T) {
	ctx := context.Background()
	t.Run("stale other run with a live pod of another run is removed", func(t *testing.T) {
		rt, _, _, enf := newRunScopeRuntime(t)
		prClock(rt, prNow)
		rsSeedPod(t, rt, "pod-a", rsLabels(rsRunA, "start-a"), corev1.PodRunning)
		b := prSeedRunObjects(t, rt, rsRunB, startDeadlineNone, prNow.Add(-2*time.Hour))
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", rsRunA); err != nil {
			t.Fatal(err)
		}
		if got := prPresent(t, rt, b); got != prNone {
			t.Errorf("stale run B objects = %v, want none", got)
		}
		enf.assertAllConditional(t)
	})
	t.Run("young other run is kept", func(t *testing.T) {
		rt, _, _, _ := newRunScopeRuntime(t)
		prClock(rt, prNow)
		b := prSeedRunObjects(t, rt, rsRunB, startDeadlineNone, prNow.Add(-10*time.Minute))
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", rsRunA); err != nil {
			t.Fatal(err)
		}
		if got := prPresent(t, rt, b); got != prAll {
			t.Errorf("young run B objects = %v, want all", got)
		}
	})
	t.Run("the named run's objects with a live pod of their run are kept", func(t *testing.T) {
		rt, _, _, _ := newRunScopeRuntime(t)
		prClock(rt, prNow)
		rsSeedPod(t, rt, "pod-a", rsLabels(rsRunA, "start-a"), corev1.PodRunning)
		a := prSeedRunObjects(t, rt, rsRunA, startDeadlineNone, prNow.Add(-48*time.Hour))
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", rsRunB); err != nil {
			t.Fatal(err)
		}
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", ""); err != nil {
			t.Fatal(err)
		}
		if got := prPresent(t, rt, a); got != prAll {
			t.Errorf("live run A objects = %v, want all", got)
		}
	})
	t.Run("the named run's stale objects with a pod of another run are kept", func(t *testing.T) {
		rt, _, _, _ := newRunScopeRuntime(t)
		prClock(rt, prNow)
		rsSeedPod(t, rt, "pod-b", rsLabels(rsRunB, "start-b"), corev1.PodRunning)
		a := prSeedRunObjects(t, rt, rsRunA, startDeadlineNone, prNow.Add(-48*time.Hour))
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", rsRunA); err != nil {
			t.Fatal(err)
		}
		if got := prPresent(t, rt, a); got != prAll {
			t.Errorf("named run A objects = %v, want all (never swept as the current run)", got)
		}
	})
	t.Run("legacy fixed names of another run with a pod gone are never swept by age", func(t *testing.T) {
		rt, _, _, _ := newRunScopeRuntime(t)
		prClock(rt, prNow)
		old := prNow.Add(-30 * 24 * time.Hour)
		prSeed(t, rt, "Secret", rsAgentSecret, "sec-legacy", rsLabels(rsRunB, ""), prAnn(startDeadlineNone), old)
		prSeed(t, rt, "SPC", rsSPC, "spc-legacy", rsLabels(rsRunB, ""), prAnn(startDeadlineNone), old)
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", rsRunA); err != nil {
			t.Fatal(err)
		}
		if !secretExists(t, rt, rt.DefaultNamespace, rsAgentSecret) || !spcExists(t, rt, rt.DefaultNamespace, rsSPC) {
			t.Error("a legacy fixed-name object of another run was swept by age")
		}
	})
	t.Run("legacy fixed name of another run is never swept by age", func(t *testing.T) {
		rt, _, _, _ := newRunScopeRuntime(t)
		prClock(rt, prNow)
		prSeed(t, rt, "Secret", rsAgentSecret, "sec-legacy", rsLabels(rsRunB, ""), prAnn(startDeadlineNone), prNow.Add(-30*24*time.Hour))
		if err := rt.CleanupAgentResources(ctx, "agent", "proj1", rsRunA); err != nil {
			t.Fatal(err)
		}
		if !secretExists(t, rt, rt.DefaultNamespace, rsAgentSecret) {
			t.Error("a legacy fixed-name object of another run was swept by age")
		}
	})
}

// --- no-run paths (an older hub) ---

// A delete with no run after the pod is gone reaches the per-run objects
// through CleanupAgentResources with no run (the broker's leftover cleanup):
// the pod is found through the annotation, whatever the object's age.
func TestCleanupAgentResources_NoRun_RemovesPerRunObjectsOfGonePod(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	prClock(rt, prNow)
	a := prSeedRunObjects(t, rt, rsRunA, "300", prNow)
	if err := rt.CleanupAgentResources(context.Background(), "agent", "proj1", ""); err != nil {
		t.Fatal(err)
	}
	if got := prPresent(t, rt, a); got != prNone {
		t.Errorf("per-run objects after a no-run leftover cleanup = %v, want none", got)
	}
	enf.assertAllConditional(t)
}

// NB1: CleanupAgentResources trusts the scion.pod_name annotation only when
// the object's name is that pod's per-run name for the object's run, and
// the pod is this agent's. Otherwise the object is kept, even though the
// pod it names is absent.
func TestCleanupAgentResources_MismatchedPodAnnotation_Kept(t *testing.T) {
	ownA := k8sAgentObjectNames(rsAgent, rsRunA)
	for _, tc := range []struct {
		name      string
		secret    string // Secret name
		spc       string // SPC name
		annotated string
	}{
		{"annotation names a different, absent pod", ownA.Secret, ownA.SPC, "proj9--agent"},
		{"annotation names another agent's pod", k8sAgentObjectNames("proj1--other", rsRunA).Secret, k8sAgentObjectNames("proj1--other", rsRunA).SPC, "proj1--other"},
		{"mismatched name (another pod) with a matching annotation", k8sAgentObjectNames("proj9--agent", rsRunA).Secret, k8sAgentObjectNames("proj9--agent", rsRunA).SPC, rsAgent},
		{"mismatched name (another run's token) with a matching annotation", k8sAgentObjectNames(rsAgent, rsRunB).Secret, k8sAgentObjectNames(rsAgent, rsRunB).SPC, rsAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _, _ := newRunScopeRuntime(t)
			prClock(rt, prNow)
			ann := map[string]string{annotationPodName: tc.annotated, annotationStartDeadlineOffset: "300"}
			prSeed(t, rt, "Secret", tc.secret, "sec-a", rsLabels(rsRunA, ""), ann, prNow)
			prSeed(t, rt, "SPC", tc.spc, "spc-a", rsLabels(rsRunA, ""), ann, prNow)
			for _, run := range []string{"", rsRunA} {
				if err := rt.CleanupAgentResources(context.Background(), "agent", "proj1", run); err != nil {
					t.Fatal(err)
				}
			}
			if !secretExists(t, rt, rt.DefaultNamespace, tc.secret) || !spcExists(t, rt, rt.DefaultNamespace, tc.spc) {
				t.Error("an object whose name and pod annotation disagree was removed")
			}
		})
	}
	// Control: a consistent object of the same shape is removed.
	rt, _, _, _ := newRunScopeRuntime(t)
	a := prSeedRunObjects(t, rt, rsRunA, "300", prNow)
	if err := rt.CleanupAgentResources(context.Background(), "agent", "proj1", rsRunA); err != nil {
		t.Fatal(err)
	}
	if got := prPresent(t, rt, a); got != prNone {
		t.Errorf("consistent per-run objects = %v, want none", got)
	}
}

// A delete with no run while the pod exists removes the per-run objects of
// the pod's run, and the pod.
func TestK8sDelete_NoRun_RemovesPodsPerRunObjects(t *testing.T) {
	rt, _, _, _ := newRunScopeRuntime(t)
	rsSeedPod(t, rt, "pod-a", rsLabels(rsRunA, "start-a"), corev1.PodRunning)
	a := prSeedRunObjects(t, rt, rsRunA, "300", prNow)
	b := prSeedRunObjects(t, rt, rsRunB, "300", prNow)
	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent}); err != nil {
		t.Fatal(err)
	}
	if got := prPresent(t, rt, a); got != prNone {
		t.Errorf("pod's run objects = %v, want none", got)
	}
	if got := prPresent(t, rt, b); got != prAll {
		t.Errorf("another run's objects = %v, want all", got)
	}
	if rsPod(t, rt) != nil {
		t.Error("pod not deleted")
	}
}

// --- run-scoped Delete ---

// Delete naming run A removes A's per-run objects with its pod; with the
// pod gone, it removes them too; B's are left either way.
func TestK8sDeleteRun_PerRunObjects(t *testing.T) {
	for _, withPod := range []bool{true, false} {
		rt, _, _, enf := newRunScopeRuntime(t)
		if withPod {
			rsSeedPod(t, rt, "pod-a", rsLabels(rsRunA, "start-a"), corev1.PodRunning)
		}
		a := prSeedRunObjects(t, rt, rsRunA, "300", prNow)
		b := prSeedRunObjects(t, rt, rsRunB, "300", prNow)
		if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA}); err != nil {
			t.Fatal(err)
		}
		if got := prPresent(t, rt, a); got != prNone {
			t.Errorf("withPod=%v: run A objects = %v, want none", withPod, got)
		}
		if got := prPresent(t, rt, b); got != prAll {
			t.Errorf("withPod=%v: run B objects = %v, want all", withPod, got)
		}
		enf.assertAllConditional(t)
	}
}

// Delete in a saved profile's namespace (GoogleCloudPlatform/scion#2481):
// the runtime's namespace is the profile's, and the per-run objects there
// are found; same-named objects in another namespace are left.
func TestK8sDeleteRun_ProfileNamespace_RemovesPerRunObjects(t *testing.T) {
	rt, _, _, enf := newRunScopeRuntime(t)
	decoy := prSeedRunObjects(t, rt, rsRunA, "300", prNow) // in "default"
	rt.DefaultNamespace = "team-a"
	a := prSeedRunObjects(t, rt, rsRunA, "300", prNow)
	if err := rt.Delete(context.Background(), RunRef{ID: rsAgent, RunID: rsRunA}); err != nil {
		t.Fatal(err)
	}
	if got := prPresent(t, rt, a); got != prNone {
		t.Errorf("profile namespace objects = %v, want none", got)
	}
	rt.DefaultNamespace = "default"
	if got := prPresent(t, rt, decoy); got != prAll {
		t.Errorf("objects in another namespace = %v, want all", got)
	}
	enf.assertAllConditional(t)
}

// --- async launch and NFS home (GoogleCloudPlatform/scion#2551) ---

// An async launch reports the per-run names in its handles, and
// DeleteResource removes the objects by them.
func TestLaunchHooks_PerRunHandleNames_DeleteResourceRemovesThem(t *testing.T) {
	rt, cs, dyn := newGKECleanupTestRuntime(t)
	var fx uidFixture
	fx.install(t, cs)
	fx.install(t, dyn)
	rec := &hookRecorder{}
	cfg := rsRunConfig(rsRunA)
	rec.apply(&cfg)
	_ = runWithHooks(t, rt, cs, cfg)
	_, handles := rec.snapshot()
	n := k8sAgentObjectNames(rsAgent, rsRunA)
	seen := map[string]bool{}
	for _, h := range handles {
		if h.Kind == api.ResourceKindPod {
			continue
		}
		seen[h.Kind+"/"+h.Name] = true
		if err := rt.DeleteResource(context.Background(), h); err != nil {
			t.Fatalf("DeleteResource(%+v): %v", h, err)
		}
	}
	for _, want := range []string{api.ResourceKindSecret + "/" + n.Secret, api.ResourceKindSecret + "/" + n.Auth, api.ResourceKindSecretProviderClass + "/" + n.SPC} {
		if !seen[want] {
			t.Errorf("no handle for %s (handles %v)", want, handles)
		}
	}
	if got := prPresent(t, rt, n); got != prNone {
		t.Errorf("objects after DeleteResource = %v, want none", got)
	}
}

// An NFS-home start removes the previous pod's per-run objects only after
// that pod is confirmed stopped.
func TestPreCleanForRun_NFSHome_PerRunSecretsOfPreviousPodAfterStop(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, rsRunB, corev1.PodSucceeded, nil)
	prev := k8sAgentObjectNames("a", rsRunB)
	labels := map[string]string{"scion.agent": "true", api.LabelRunID: rsRunB}
	for _, name := range []string{prev.Secret, prev.Auth} {
		if _, err := cs.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID("u-" + name[:12]), Labels: labels}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	keepPodsOnDelete(cs)
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	stopped := false
	secretsAtStop := 0
	fc.onTick = func(now time.Time) {
		if stopped || now.Sub(start) < 30*time.Second {
			return
		}
		list, _ := cs.CoreV1().Secrets("default").List(context.Background(), metav1.ListOptions{})
		secretsAtStop = len(list.Items)
		p, _ := cs.CoreV1().Pods("default").Get(context.Background(), "a", metav1.GetOptions{})
		p.Status.ContainerStatuses[0].State = stTerminated
		_, _ = cs.CoreV1().Pods("default").UpdateStatus(context.Background(), p, metav1.UpdateOptions{})
		stopped = true
	}
	rt.execReadyClock = fc.clock()
	if err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, nfsHS()); err != nil {
		t.Fatalf("preCleanForRun: %v", err)
	}
	if secretsAtStop != 2 {
		t.Errorf("Secrets present while the previous pod was stopping = %d, want 2", secretsAtStop)
	}
	for _, name := range []string{prev.Secret, prev.Auth} {
		if secretExists(t, rt, "default", name) {
			t.Errorf("previous pod's per-run Secret %s not removed after its stop", name)
		}
	}
}

// Per-run objects carry the pod-name and deadline-offset annotations; a
// start without a run ID writes neither.
func TestK8sCreate_PerRunAnnotations(t *testing.T) {
	rt, _, _ := newGKECleanupTestRuntime(t)
	prClock(rt, prNow)
	secrets := []api.ResolvedSecret{{Name: "K", Type: "environment", Target: "K", Value: "v", Ref: "projects/p/secrets/k"}}
	ctx, cancel := context.WithDeadline(context.Background(), prNow.Add(90*time.Second))
	defer cancel()
	name, err := rt.createAgentSecret(ctx, rt.DefaultNamespace, rsAgent, secrets, rsLabels(rsRunA, ""))
	if err != nil {
		t.Fatal(err)
	}
	s, err := rt.Client.Clientset.CoreV1().Secrets(rt.DefaultNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Annotations[annotationPodName] != rsAgent || s.Annotations[annotationStartDeadlineOffset] != "90" {
		t.Errorf("annotations = %v, want pod name and offset 90", s.Annotations)
	}
	spcName, err := rt.createSecretProviderClass(context.Background(), rt.DefaultNamespace, rsAgent, secrets, rsLabels(rsRunA, ""))
	if err != nil {
		t.Fatal(err)
	}
	spc, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(rt.DefaultNamespace).Get(context.Background(), spcName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if spc.GetAnnotations()[annotationStartDeadlineOffset] != startDeadlineNone {
		t.Errorf("SPC annotations = %v, want offset none", spc.GetAnnotations())
	}
	legacy, err := rt.createAgentSecret(context.Background(), rt.DefaultNamespace, "other", secrets, rsLabels("", ""))
	if err != nil {
		t.Fatal(err)
	}
	ls, _ := rt.Client.Clientset.CoreV1().Secrets(rt.DefaultNamespace).Get(context.Background(), legacy, metav1.GetOptions{})
	if legacy != agentSecretPrefix+"other" || len(ls.Annotations) != 0 {
		t.Errorf("no-run Secret = %s %v, want the fixed name and no annotations", legacy, ls.Annotations)
	}
}
