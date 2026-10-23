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
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// syncBuffer is a bytes.Buffer safe for the log handler and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// F1 (log part): the unschedulable log lines name the pod's effective node
// selector and its toleration keys, on the failing path and on the GKE
// waiting path; the error text is unchanged.
func TestWaitForPodReady_UnschedulableLogNamesPlacement_F1(t *testing.T) {
	spec := corev1.PodSpec{
		NodeSelector: map[string]string{"cloud.google.com/gke-nodepool": "scion-agents"},
		Tolerations: []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual,
			Value: "toleration-value", Effect: corev1.TaintEffectNoSchedule}},
	}
	status := corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "0/3 nodes are available"}}}

	for _, gke := range []bool{false, true} {
		buf := &syncBuffer{}
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

		rt, clientset := fastPodReadyRuntime(gke)
		rt.podReadyTimeout = 200 * time.Millisecond // the GKE path waits until its budget ends
		clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: action.(k8stesting.GetAction).GetName(), Namespace: action.GetNamespace()},
				Spec:       *spec.DeepCopy(),
				Status:     *status.DeepCopy(),
			}, nil
		})
		err := rt.waitForPodReady(context.Background(), "default", "p", launchHooks{})
		slog.SetDefault(old)
		if err == nil {
			t.Fatalf("gke=%v: waitForPodReady succeeded, want a failure", gke)
		}
		if !gke && !strings.HasPrefix(err.Error(), `pod "p" cannot be scheduled: 0/3 nodes are available`) {
			t.Errorf("error text changed: %s", err)
		}
		out := buf.String()
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "Pod unschedulable") {
				line = l
				break
			}
		}
		if line == "" {
			t.Fatalf("gke=%v: no unschedulable log line in:\n%s", gke, out)
		}
		for _, want := range []string{"nodeSelector=", "cloud.google.com/gke-nodepool:scion-agents", "tolerationKeys=[dedicated]"} {
			if !strings.Contains(line, want) {
				t.Errorf("gke=%v: log line lacks %q: %s", gke, want, line)
			}
		}
		if strings.Contains(line, "toleration-value") {
			t.Errorf("gke=%v: log line carries a toleration value: %s", gke, line)
		}
	}
}
