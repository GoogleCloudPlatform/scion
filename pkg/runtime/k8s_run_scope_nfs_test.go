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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// Run-scoped pre-clean of NFS-home agents (ptone/scion#2550 P2 merged with
// GoogleCloudPlatform/scion#2534): the run-labelled start path keeps the
// NFS-home rules of the name-based cleanupStalePod path. Each test mirrors
// one of the name-based tests in k8s_nfs_home_start_test.go with a run ID
// on the start.

// withRunID returns cfg with the scion.run_id label set, as a hub start has.
func withRunID(cfg RunConfig, runID string) RunConfig {
	labels := map[string]string{}
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels[api.LabelRunID] = runID
	cfg.Labels = labels
	return cfg
}

// seedNFSPod creates runningNFSHomePod("a") with the given run label
// ("" for a legacy pod) and phase.
func seedNFSPod(t *testing.T, rt *KubernetesRuntime, runID string, phase corev1.PodPhase, mutate func(*corev1.Pod)) {
	t.Helper()
	p := runningNFSHomePod("a")
	if runID != "" {
		p.Labels = map[string]string{api.LabelRunID: runID}
	}
	p.Status.Phase = phase
	if mutate != nil {
		mutate(p)
	}
	if _, err := rt.Client.Clientset.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func nfsHS() *HomeStorageRealization { return &HomeStorageRealization{TerminationWaitSeconds: 15} }

// Mirrors TestCleanupStalePod_WaitsForConfirmedTermination: a finished pod
// of another run is deleted gracefully, with a UID precondition, and the
// start waits until its container is confirmed stopped.
func TestPreCleanForRun_NFSHome_WaitsForConfirmedTermination(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, rsRunB, corev1.PodSucceeded, nil)
	deletes := keepPodsOnDelete(cs)
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	fc.onTick = func(now time.Time) {
		if now.Sub(start) >= 30*time.Second {
			p, _ := cs.CoreV1().Pods("default").Get(context.Background(), "a", metav1.GetOptions{})
			if p != nil && p.Status.ContainerStatuses[0].State.Terminated == nil {
				p.Status.ContainerStatuses[0].State = stTerminated
				_, _ = cs.CoreV1().Pods("default").UpdateStatus(context.Background(), p, metav1.UpdateOptions{})
			}
		}
	}
	rt.execReadyClock = fc.clock()

	if err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, nfsHS()); err != nil {
		t.Fatalf("preCleanForRun: %v", err)
	}
	if len(*deletes) != 1 || (*deletes)[0].GracePeriodSeconds != nil {
		t.Fatalf("delete options = %+v, want one delete with the pod's own grace", *deletes)
	}
	if pre := (*deletes)[0].Preconditions; pre == nil || pre.UID == nil || *pre.UID != "uid-1" {
		t.Errorf("delete precondition = %+v, want UID uid-1", pre)
	}
	if waited := fc.now.Sub(start); waited < 30*time.Second || waited > 90*time.Second {
		t.Errorf("waited %s, want the old pod's shutdown time and less than 90s", waited)
	}
}

// Mirrors TestCleanupStalePod_RefusesWhenUnconfirmed: an NFS-home pod that
// never confirms its stop refuses the start at the bound, with
// previous_pod_unconfirmed reachable through errors.Is, and the error text
// stays fixed (no pod name).
func TestPreCleanForRun_NFSHome_RefusesWhenUnconfirmed(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, "", "", nil) // legacy pod, as runningNFSHomePod
	keepPodsOnDelete(cs)
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	rt.execReadyClock = fc.clock()
	err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, nfsHS())
	if !errors.Is(err, errPreviousPodUnconfirmed) {
		t.Fatalf("err = %v, want previous_pod_unconfirmed", err)
	}
	if waited := fc.now.Sub(start); waited < 45*time.Second || waited > 47*time.Second {
		t.Errorf("refused after %s, want the 45s bound (grace 30 + wait 15)", waited)
	}
	if strings.Contains(err.Error(), `"a"`) || strings.Contains(err.Error(), "pod a") || strings.Contains(err.Error(), rsRunA) {
		t.Errorf("error text carries identity: %q", err)
	}
}

// Mirrors TestCleanupStalePod_NodeLostRefusedAtOnce.
func TestPreCleanForRun_NFSHome_NodeLostRefusedAtOnce(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, rsRunB, corev1.PodFailed, func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}}
	})
	keepPodsOnDelete(cs)
	fc := &fakeTerminationClock{now: time.Unix(1000, 0)}
	rt.execReadyClock = fc.clock()
	err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, nil)
	if !errors.Is(err, errPreviousPodUnconfirmed) || fc.sleeps != 0 {
		t.Errorf("err = %v after %d sleeps, want an immediate refusal", err, fc.sleeps)
	}
}

// Mirrors TestCleanupStalePod_PlainPodUnchanged: a pod without the NFS-home
// annotation is deleted immediately, with a UID precondition, and not
// waited for.
func TestPreCleanForRun_PlainPodImmediate(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, rsRunB, corev1.PodSucceeded, func(p *corev1.Pod) { p.Annotations = nil })
	deletes := keepPodsOnDelete(cs)
	fc := &fakeTerminationClock{now: time.Unix(1000, 0)}
	rt.execReadyClock = fc.clock()
	if err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, nil); err != nil {
		t.Fatal(err)
	}
	if len(*deletes) != 1 || (*deletes)[0].GracePeriodSeconds == nil || *(*deletes)[0].GracePeriodSeconds != 0 || fc.sleeps != 0 {
		t.Errorf("plain pod: deletes %+v, sleeps %d; want one immediate delete and no wait", *deletes, fc.sleeps)
	}
	if (*deletes)[0].Preconditions == nil || (*deletes)[0].Preconditions.UID == nil {
		t.Errorf("plain pod delete has no UID precondition")
	}
}

// Mirrors TestCleanupStalePod_UnreadablePodRefused: an NFS-home start never
// proceeds past a pod it could not read.
func TestPreCleanForRun_NFSHome_UnreadablePodRefused(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, rsRunB, corev1.PodSucceeded, nil)
	deletes := keepPodsOnDelete(cs)
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("connection reset")
	})
	err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, &HomeStorageRealization{})
	if !errors.Is(err, errPreviousPodUnconfirmed) {
		t.Errorf("err = %v, want previous_pod_unconfirmed wrapping the read error", err)
	}
	if len(*deletes) != 0 {
		t.Errorf("deletes = %+v, want none", *deletes)
	}
}

// A previous NFS-home pod that is already terminating is waited for, never
// force-deleted.
func TestPreCleanForRun_NFSHome_TerminatingPodWaitedFor(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	now := metav1.Now()
	seedNFSPod(t, rt, rsRunB, corev1.PodRunning, func(p *corev1.Pod) {
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"example.com/hold"}
	})
	deletes := keepPodsOnDelete(cs)
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	rt.execReadyClock = fc.clock()
	err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, nfsHS())
	if !errors.Is(err, errPreviousPodUnconfirmed) {
		t.Fatalf("err = %v, want previous_pod_unconfirmed after waiting", err)
	}
	for _, d := range *deletes {
		if d.GracePeriodSeconds != nil && *d.GracePeriodSeconds == 0 {
			t.Errorf("terminating NFS-home pod force-deleted: %+v", d)
		}
	}
	if fc.sleeps == 0 {
		t.Error("terminating NFS-home pod was not waited for")
	}
}

// A live NFS-home pod of another run still makes the start fail with
// ErrRunConflict before anything is deleted or waited for.
func TestPreCleanForRun_NFSHome_LiveOtherRunConflicts(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, rsRunB, corev1.PodRunning, nil)
	deletes := keepPodsOnDelete(cs)
	fc := &fakeTerminationClock{now: time.Unix(1000, 0)}
	rt.execReadyClock = fc.clock()
	err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, nfsHS())
	if !errors.Is(err, ErrRunConflict) {
		t.Fatalf("err = %v, want ErrRunConflict", err)
	}
	if len(*deletes) != 0 || fc.sleeps != 0 {
		t.Errorf("deletes %+v, sleeps %d; want none", *deletes, fc.sleeps)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Errorf("secrets touched (%s) on a conflict", a.GetVerb())
		}
	}
}

// A run-scoped Delete of an NFS-home pod is graceful (no force), keeps the
// UID precondition, and does not wait.
func TestDeleteRun_GracefulForNFSHomePod(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, rsRunA, corev1.PodRunning, nil)
	deletes := keepPodsOnDelete(cs)
	done := make(chan error, 1)
	go func() { done <- rt.Delete(context.Background(), RunRef{ID: "default/a", RunID: rsRunA}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete waited for the pod to terminate")
	}
	if len(*deletes) != 1 || (*deletes)[0].GracePeriodSeconds != nil {
		t.Fatalf("deletes = %+v, want one graceful delete", *deletes)
	}
	if pre := (*deletes)[0].Preconditions; pre == nil || pre.UID == nil {
		t.Errorf("delete has no UID precondition")
	}
}

// Run-scoped twin of TestRun_NFSHomeSecretsCleanedAfterPreviousPod.
func TestRun_RunScoped_NFSHomeSecretsCleanedAfterPreviousPod(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	cfg := withRunID(nfsHomeTestConfig(true), rsRunA)
	cfg.Name = "a"
	seedNFSPod(t, rt, rsRunB, corev1.PodSucceeded, nil)
	keepPodsOnDelete(cs)
	rt.execReadyClock = (&fakeTerminationClock{now: time.Unix(1000, 0)}).clock()
	_, err := rt.Run(context.Background(), cfg)
	if !errors.Is(err, errPreviousPodUnconfirmed) {
		t.Fatalf("err = %v, want previous_pod_unconfirmed", err)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "secrets" && (a.GetVerb() == "delete" || a.GetVerb() == "delete-collection" || a.GetVerb() == "list") {
			t.Errorf("secrets touched (%s) before the previous pod was confirmed stopped", a.GetVerb())
		}
	}
}

// Run-scoped twin of TestRun_NFSHomeSecretDeleteBetweenConfirmAndCreate:
// the Secrets are deleted after the last read of the previous pod and
// before the new pod is created.
func TestRun_RunScoped_NFSHomeSecretDeleteBetweenConfirmAndCreate(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	cfg := withRunID(nfsHomeTestConfig(true), rsRunA)
	cfg.Name = "a"
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ResolvedAuth.Files[0].SourcePath = adc
	seedNFSPod(t, rt, rsRunB, corev1.PodSucceeded, nil)
	// A stale Secret of run B, which pre-clean removes after the wait.
	if _, err := cs.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: agentSecretPrefix + "a", Namespace: "default", UID: "sec-b",
			Labels: map[string]string{"scion.agent": "true", api.LabelRunID: rsRunB}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	keepPodsOnDelete(cs)
	start := time.Unix(1000, 0)
	fc := &fakeTerminationClock{now: start}
	gone := false
	fc.onTick = func(now time.Time) {
		if !gone && now.Sub(start) >= 20*time.Second {
			gone = true
			_ = cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "default", "a")
		}
	}
	rt.execReadyClock = fc.clock()
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("stop here")
	})
	if _, err := rt.Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("Run: %v", err)
	}
	lastPodGet, secretDelete, podCreate := -1, -1, -1
	for i, a := range cs.Actions() {
		switch {
		case a.GetResource().Resource == "pods" && a.GetVerb() == "get":
			lastPodGet = i
		case a.GetResource().Resource == "secrets" && a.GetVerb() == "delete" && secretDelete < 0:
			secretDelete = i
		case a.GetResource().Resource == "pods" && a.GetVerb() == "create":
			podCreate = i
		}
	}
	if secretDelete < 0 || lastPodGet >= secretDelete || secretDelete >= podCreate {
		t.Errorf("order: last pod read %d, secret delete %d, pod create %d", lastPodGet, secretDelete, podCreate)
	}
}

// Run-scoped twin of TestRun_NFSHomeStartLockReleasedOnce ("previous pod
// unconfirmed"), plus the run-scoped conflict: the start lock is taken
// once and released once.
func TestRun_RunScoped_NFSHomeStartLockReleasedOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase corev1.PodPhase
		want  error
	}{
		{"previous pod unconfirmed", corev1.PodSucceeded, errPreviousPodUnconfirmed},
		{"live other run", corev1.PodRunning, ErrRunConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, cs, _ := newTestK8sRuntime()
			cfg := withRunID(nfsHomeTestConfig(true), rsRunA)
			cfg.Name = "a"
			l := &countingLocker{acquire: true}
			cfg.Locker = l
			seedNFSPod(t, rt, rsRunB, tc.phase, nil)
			keepPodsOnDelete(cs)
			rt.execReadyClock = (&fakeTerminationClock{now: time.Unix(1000, 0)}).clock()
			if _, err := rt.Run(context.Background(), cfg); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if l.acquires != 1 || l.releases != 1 {
				t.Errorf("acquires %d, releases %d; want 1 and 1", l.acquires, l.releases)
			}
		})
	}
}

// The home start lock is held while the run-scoped pre-clean removes the
// previous pod (it is released only after the start ends or the pod
// exists), so no other start can run between the delete and the wait.
func TestRun_RunScoped_NFSHomeLockHeldDuringPreClean(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	cfg := withRunID(nfsHomeTestConfig(true), rsRunA)
	cfg.Name = "a"
	l := &countingLocker{acquire: true}
	cfg.Locker = l
	seedNFSPod(t, rt, rsRunB, corev1.PodSucceeded, nil)
	sawDelete := false
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		sawDelete = true
		if l.acquires != 1 || l.releases != 0 {
			t.Errorf("pod deleted with acquires %d, releases %d; want the lock held", l.acquires, l.releases)
		}
		return true, nil, nil
	})
	rt.execReadyClock = (&fakeTerminationClock{now: time.Unix(1000, 0)}).clock()
	_, _ = rt.Run(context.Background(), cfg)
	if !sawDelete {
		t.Fatal("pre-clean did not delete the previous pod")
	}
}

// A failed delete of a previous NFS-home pod refuses the start with
// previous_pod_unconfirmed (retryable), not a bare API error.
func TestPreCleanForRun_NFSHome_DeleteErrorUnconfirmed(t *testing.T) {
	rt, cs, _ := newTestK8sRuntime()
	seedNFSPod(t, rt, rsRunB, corev1.PodSucceeded, nil)
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	err := rt.preCleanForRun(context.Background(), "default", "a", rsRunA, true, nfsHS())
	if !errors.Is(err, errPreviousPodUnconfirmed) {
		t.Errorf("err = %v, want previous_pod_unconfirmed", err)
	}
}
