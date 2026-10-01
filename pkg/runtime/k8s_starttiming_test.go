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
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestComputePodLifecycleTimings covers the start-time attribution helper
// used by waitForPodReady's ready-path log line: it must derive each
// milestone duration from the pod's CreationTimestamp when available, and
// leave a field at podTimingUnavailable (never a misleading 0) when the
// corresponding condition or container state is missing.
func TestComputePodLifecycleTimings(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	condition := func(condType corev1.PodConditionType, status corev1.ConditionStatus, offset time.Duration) corev1.PodCondition {
		return corev1.PodCondition{
			Type:               condType,
			Status:             status,
			LastTransitionTime: metav1.NewTime(created.Add(offset)),
		}
	}

	runningContainerStatus := func(name string, offset time.Duration) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name: name,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(created.Add(offset)),
				},
			},
		}
	}

	cases := []struct {
		name      string
		pod       *corev1.Pod
		container string
		want      podLifecycleTimings
	}{
		{
			name:      "nil pod",
			pod:       nil,
			container: "agent",
			want: podLifecycleTimings{
				scheduledMs:        podTimingUnavailable,
				initializedMs:      podTimingUnavailable,
				containersReadyMs:  podTimingUnavailable,
				containerStartedMs: podTimingUnavailable,
			},
		},
		{
			name: "zero creation timestamp",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						condition(corev1.PodScheduled, corev1.ConditionTrue, time.Second),
					},
				},
			},
			container: "agent",
			want: podLifecycleTimings{
				scheduledMs:        podTimingUnavailable,
				initializedMs:      podTimingUnavailable,
				containersReadyMs:  podTimingUnavailable,
				containerStartedMs: podTimingUnavailable,
			},
		},
		{
			name: "all milestones present",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						condition(corev1.PodScheduled, corev1.ConditionTrue, 1*time.Second),
						condition(corev1.PodInitialized, corev1.ConditionTrue, 2*time.Second),
						condition(corev1.ContainersReady, corev1.ConditionTrue, 3*time.Second),
					},
					ContainerStatuses: []corev1.ContainerStatus{
						runningContainerStatus("agent", 3500*time.Millisecond),
					},
				},
			},
			container: "agent",
			want: podLifecycleTimings{
				scheduledMs:        1000,
				initializedMs:      2000,
				containersReadyMs:  3000,
				containerStartedMs: 3500,
			},
		},
		{
			name: "missing conditions and no matching container",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						runningContainerStatus("sidecar", 500*time.Millisecond),
					},
				},
			},
			container: "agent",
			want: podLifecycleTimings{
				scheduledMs:        podTimingUnavailable,
				initializedMs:      podTimingUnavailable,
				containersReadyMs:  podTimingUnavailable,
				containerStartedMs: podTimingUnavailable,
			},
		},
		{
			name: "condition present but false is ignored",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						condition(corev1.PodScheduled, corev1.ConditionFalse, 1*time.Second),
					},
				},
			},
			container: "agent",
			want: podLifecycleTimings{
				scheduledMs:        podTimingUnavailable,
				initializedMs:      podTimingUnavailable,
				containersReadyMs:  podTimingUnavailable,
				containerStartedMs: podTimingUnavailable,
			},
		},
		{
			name: "container present but not yet running",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
					},
				},
			},
			container: "agent",
			want: podLifecycleTimings{
				scheduledMs:        podTimingUnavailable,
				initializedMs:      podTimingUnavailable,
				containersReadyMs:  podTimingUnavailable,
				containerStartedMs: podTimingUnavailable,
			},
		},
		{
			// Clock skew plus whole-second truncation across the apiserver,
			// scheduler, kubelet and container runtime can make a condition's
			// LastTransitionTime land before the pod's own CreationTimestamp.
			// The result must clamp to 0, not a negative duration, and must
			// not collide with the podTimingUnavailable (-1) sentinel.
			name: "condition timestamp before creation clamps to zero, not the sentinel",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						condition(corev1.PodScheduled, corev1.ConditionTrue, -1*time.Second),
						condition(corev1.PodInitialized, corev1.ConditionTrue, -1*time.Millisecond),
					},
					ContainerStatuses: []corev1.ContainerStatus{
						runningContainerStatus("agent", -500*time.Millisecond),
					},
				},
			},
			container: "agent",
			want: podLifecycleTimings{
				scheduledMs:        0,
				initializedMs:      0,
				containersReadyMs:  podTimingUnavailable,
				containerStartedMs: 0,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computePodLifecycleTimings(tc.pod, tc.container)
			if got != tc.want {
				t.Errorf("computePodLifecycleTimings() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCountingReader proves the byte counter wrapping the tar stream in
// syncToPod reports the exact number of bytes read, regardless of how many
// Read calls it takes to drain the source (a real pod exec stream reads in
// many small chunks, not one).
func TestCountingReader(t *testing.T) {
	cases := []struct {
		name      string
		data      []byte
		chunkSize int
	}{
		{name: "empty", data: []byte{}, chunkSize: 4},
		{name: "single read larger than data", data: []byte("hello world"), chunkSize: 1024},
		{name: "many small reads", data: bytes.Repeat([]byte("ab"), 1000), chunkSize: 3},
		{name: "exact chunk boundary", data: bytes.Repeat([]byte("x"), 100), chunkSize: 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &countingReader{r: strings.NewReader(string(tc.data))}
			buf := make([]byte, tc.chunkSize)
			var read []byte
			for {
				n, err := cr.Read(buf)
				read = append(read, buf[:n]...)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if n == 0 {
					t.Fatalf("Read returned 0 bytes with no error (would loop forever)")
				}
			}
			if cr.count != int64(len(tc.data)) {
				t.Errorf("countingReader.count = %d, want %d", cr.count, len(tc.data))
			}
			if !bytes.Equal(read, tc.data) {
				t.Errorf("countingReader altered the stream content: got %q, want %q", read, tc.data)
			}
		})
	}
}
