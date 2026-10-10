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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// tombstoneClock is a settable clock for the runtime's nowFn seam.
type tombstoneClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tombstoneClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *tombstoneClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTombstoneTestRuntime(t *testing.T) (*KubernetesRuntime, *k8sfake.Clientset, *tombstoneClock) {
	t.Helper()
	rt, clientset, _ := newTestK8sRuntime()
	rt.DefaultNamespace = "default"
	clock := &tombstoneClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	rt.nowFn = clock.now
	return rt, clientset, clock
}

func tombstoneTestPod(agentName, uid string, recoverable bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName,
			Namespace: "default",
			UID:       types.UID(uid),
			Labels: map[string]string{
				"scion.name":               agentName,
				projectkeys.LabelProjectID: "proj-1",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: "test:latest"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  agentContainerName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	if recoverable {
		pod.Spec.Volumes = []corev1.Volume{{
			Name:         "workspace",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "ws"}},
		}}
	}
	return pod
}

func createPod(t *testing.T, cs *k8sfake.Clientset, pod *corev1.Pod) {
	t.Helper()
	if _, err := cs.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
}

func deletePod(t *testing.T, cs *k8sfake.Clientset, pod *corev1.Pod) {
	t.Helper()
	if err := cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
}

func createPodEvent(t *testing.T, cs *k8sfake.Clientset, pod *corev1.Pod, name, reason string) {
	t.Helper()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pod.Namespace},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
		},
		Reason: reason,
		Type:   corev1.EventTypeNormal,
	}
	if _, err := cs.CoreV1().Events(pod.Namespace).Create(context.Background(), ev, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create event: %v", err)
	}
}

func mustList(t *testing.T, rt *KubernetesRuntime, filter map[string]string) []api.AgentInfo {
	t.Helper()
	agents, err := rt.List(context.Background(), filter)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return agents
}

func TestList_Tombstone_PreemptedPodVanishedBeforeTerminalList(t *testing.T) {
	for _, tc := range []struct {
		name        string
		recoverable bool
		wantPhase   state.Phase
	}{
		{"emptydir workspace", false, state.PhaseError},
		{"persistent workspace", true, state.PhaseStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, cs, _ := newTombstoneTestRuntime(t)
			pod := tombstoneTestPod("agent-a", "uid-1", tc.recoverable)
			createPod(t, cs, pod)
			if got := mustList(t, rt, nil); len(got) != 1 || got[0].ExitReason != "" {
				t.Fatalf("first List: got %+v, want the running pod with no reason", got)
			}

			// Preempted and removed between two Lists.
			createPodEvent(t, cs, pod, "ev-1", "Preempted")
			deletePod(t, cs, pod)

			for i := 0; i < 2; i++ {
				got := mustList(t, rt, nil)
				if len(got) != 1 {
					t.Fatalf("List %d: got %d agents, want 1 tombstone", i, len(got))
				}
				ts := got[0]
				if ts.Name != "agent-a" || ts.ProjectID != "proj-1" {
					t.Errorf("tombstone identity = %q/%q", ts.Name, ts.ProjectID)
				}
				if ts.ExitReason != string(state.ExitReasonPreempted) {
					t.Errorf("tombstone ExitReason = %q, want preempted", ts.ExitReason)
				}
				if ts.Phase != string(tc.wantPhase) {
					t.Errorf("tombstone Phase = %q, want %q", ts.Phase, tc.wantPhase)
				}
				if ts.ExitCode != nil {
					t.Errorf("tombstone ExitCode = %v, want nil", *ts.ExitCode)
				}
				if ts.Runtime != "kubernetes" || ts.Kubernetes == nil || ts.Kubernetes.UID != "uid-1" {
					t.Errorf("tombstone runtime metadata = %q %+v", ts.Runtime, ts.Kubernetes)
				}
			}
		})
	}
}

func TestList_Tombstone_EvictedEvent(t *testing.T) {
	rt, cs, _ := newTombstoneTestRuntime(t)
	pod := tombstoneTestPod("agent-e", "uid-e", false)
	createPod(t, cs, pod)
	mustList(t, rt, nil)
	createPodEvent(t, cs, pod, "ev-e", "Evicted")
	deletePod(t, cs, pod)

	got := mustList(t, rt, nil)
	if len(got) != 1 || got[0].ExitReason != string(state.ExitReasonEvicted) {
		t.Fatalf("got %+v, want one evicted tombstone", got)
	}
}

func TestList_Tombstone_NoDisruptionEvent_NoTombstone(t *testing.T) {
	rt, cs, _ := newTombstoneTestRuntime(t)
	pod := tombstoneTestPod("agent-b", "uid-2", true)
	createPod(t, cs, pod)
	mustList(t, rt, nil)

	// An explicit stop or delete: the pod goes away with only ordinary
	// events, or a Preempted event recorded on a different pod UID.
	createPodEvent(t, cs, pod, "ev-kill", "Killing")
	other := tombstoneTestPod("agent-b", "uid-older", true)
	createPodEvent(t, cs, other, "ev-old", "Preempted")
	deletePod(t, cs, pod)

	if got := mustList(t, rt, nil); len(got) != 0 {
		t.Fatalf("got %+v, want no agents (no disruption evidence)", got)
	}
}

func TestList_Tombstone_DroppedWhenNewPodAppears(t *testing.T) {
	rt, cs, _ := newTombstoneTestRuntime(t)
	pod := tombstoneTestPod("agent-c", "uid-3", false)
	createPod(t, cs, pod)
	mustList(t, rt, nil)
	createPodEvent(t, cs, pod, "ev-3", "Preempted")
	deletePod(t, cs, pod)
	if got := mustList(t, rt, nil); len(got) != 1 || got[0].ExitReason != string(state.ExitReasonPreempted) {
		t.Fatalf("got %+v, want a preempted tombstone", got)
	}

	newer := tombstoneTestPod("agent-c", "uid-4", false)
	createPod(t, cs, newer)
	for i := 0; i < 2; i++ {
		got := mustList(t, rt, nil)
		if len(got) != 1 {
			t.Fatalf("List %d: got %d agents, want only the new pod", i, len(got))
		}
		if got[0].Kubernetes.UID != "uid-4" || got[0].ExitReason != "" {
			t.Errorf("List %d: got %+v, want the new running pod", i, got[0])
		}
	}
}

func TestList_Tombstone_DroppedAfterTTL(t *testing.T) {
	rt, cs, clock := newTombstoneTestRuntime(t)
	pod := tombstoneTestPod("agent-d", "uid-5", false)
	createPod(t, cs, pod)
	mustList(t, rt, nil)
	createPodEvent(t, cs, pod, "ev-5", "Preempted")
	deletePod(t, cs, pod)
	if got := mustList(t, rt, nil); len(got) != 1 {
		t.Fatalf("got %d agents, want a tombstone", len(got))
	}

	clock.advance(podTombstoneTTL - time.Second)
	if got := mustList(t, rt, nil); len(got) != 1 {
		t.Fatalf("before TTL: got %d agents, want the tombstone", len(got))
	}
	clock.advance(2 * time.Second)
	if got := mustList(t, rt, nil); len(got) != 0 {
		t.Fatalf("after TTL: got %+v, want none", got)
	}
}

func TestList_Tombstone_PodCreatedByRunNeverListed(t *testing.T) {
	// Run() remembers the pod it created, so a pod preempted and removed
	// before the first List() is still reported.
	rt, cs, _ := newTombstoneTestRuntime(t)
	pod := tombstoneTestPod("agent-f", "uid-6", false)
	rt.trackCreatedPod(pod)
	createPodEvent(t, cs, pod, "ev-6", "Preempted")

	got := mustList(t, rt, nil)
	if len(got) != 1 || got[0].ExitReason != string(state.ExitReasonPreempted) {
		t.Fatalf("got %+v, want a preempted tombstone", got)
	}
}

func TestList_Tombstone_DroppedByStart(t *testing.T) {
	rt, cs, _ := newTombstoneTestRuntime(t)
	pod := tombstoneTestPod("agent-g", "uid-7", false)
	createPod(t, cs, pod)
	mustList(t, rt, nil)
	createPodEvent(t, cs, pod, "ev-7", "Preempted")
	deletePod(t, cs, pod)
	if got := mustList(t, rt, nil); len(got) != 1 {
		t.Fatalf("got %d agents, want a tombstone", len(got))
	}

	rt.noteAgentStart("default", "agent-g")
	if got := mustList(t, rt, nil); len(got) != 0 {
		t.Fatalf("got %+v, want none after a start began", got)
	}
}

func TestList_Tombstone_FilteredListDoesNotTreatOtherPodsAsVanished(t *testing.T) {
	rt, cs, _ := newTombstoneTestRuntime(t)
	pod := tombstoneTestPod("agent-h", "uid-8", false)
	createPod(t, cs, pod)
	mustList(t, rt, nil)

	// A List scoped to another agent must not consume agent-h's entry.
	if got := mustList(t, rt, map[string]string{"scion.name": "someone-else"}); len(got) != 0 {
		t.Fatalf("filtered List: got %+v", got)
	}

	createPodEvent(t, cs, pod, "ev-8", "Preempted")
	deletePod(t, cs, pod)
	// Out-of-scope filtered List: still no tombstone shown for agent-h.
	if got := mustList(t, rt, map[string]string{"scion.name": "someone-else"}); len(got) != 0 {
		t.Fatalf("filtered List after delete: got %+v", got)
	}
	got := mustList(t, rt, nil)
	if len(got) != 1 || got[0].ExitReason != string(state.ExitReasonPreempted) {
		t.Fatalf("got %+v, want a preempted tombstone", got)
	}
}

func TestList_Tombstone_ConcurrentListAndTrack(t *testing.T) {
	// List and Run's tracking may run concurrently; run under -race
	// locally when memory allows.
	rt, cs, _ := newTombstoneTestRuntime(t)
	createPod(t, cs, tombstoneTestPod("agent-i", "uid-9", false))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = rt.List(context.Background(), nil)
			}
		}()
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rt.trackCreatedPod(tombstoneTestPod("agent-j", "uid-j", false))
				rt.noteAgentStart("default", "agent-j")
			}
		}(i)
	}
	wg.Wait()
}
