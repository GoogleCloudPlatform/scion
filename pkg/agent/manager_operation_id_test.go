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

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newNamespacedPodManager returns a manager over a Kubernetes runtime that
// lists all namespaces with "default" as its default namespace, where every
// request is Forbidden, and an agent pod in scion-agents, labelled with run
// runID when it is not empty.
func newNamespacedPodManager(t *testing.T, runID string) (Manager, *k8sfake.Clientset, *[]string) {
	t.Helper()
	cs := k8sfake.NewClientset()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "my-agent", Namespace: "scion-agents",
		Labels: map[string]string{"scion.name": "my-agent", "scion.agent": "true"},
	}}
	if runID != "" {
		pod.Labels[api.LabelRunID] = runID
	}
	if _, err := cs.CoreV1().Pods("scion-agents").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var defaultRequests []string
	cs.PrependReactor("*", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetNamespace() != "default" {
			return false, nil, nil
		}
		defaultRequests = append(defaultRequests, action.GetVerb()+" "+action.GetResource().Resource)
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: action.GetResource().Resource}, "", nil)
	})
	rt := runtime.NewKubernetesRuntime(k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), cs))
	rt.DefaultNamespace = "default"
	rt.ListAllNamespaces = true
	return NewManager(rt), cs, &defaultRequests
}

func TestManagerStopAndDelete_KubernetesPodAddressedByNamespace(t *testing.T) {
	for _, op := range []string{"stop", "delete"} {
		t.Run(op, func(t *testing.T) {
			mgr, cs, defaultRequests := newNamespacedPodManager(t, "")
			ctx := context.Background()
			var err error
			if op == "stop" {
				err = mgr.Stop(ctx, "my-agent", "", "")
			} else {
				_, err = mgr.Delete(ctx, "my-agent", false, "", false)
			}
			if err != nil {
				t.Fatalf("%s: %v", op, err)
			}
			if _, err := cs.CoreV1().Pods("scion-agents").Get(ctx, "my-agent", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
				t.Fatalf("pod still present after %s (err=%v)", op, err)
			}
			if len(*defaultRequests) != 0 {
				t.Fatalf("requests sent to the default namespace: %v", *defaultRequests)
			}
		})
	}
}

// A run-scoped Manager.Stop addresses the pod by its namespace-qualified
// operation ID too (manager.go's run-scoped branch): the
// pod of the requested run is stopped and nothing goes to "default". A stop
// naming another run finds no target and leaves the pod alone.
func TestManagerStop_RunScoped_KubernetesPodAddressedByNamespace(t *testing.T) {
	t.Run("own run", func(t *testing.T) {
		mgr, cs, defaultRequests := newNamespacedPodManager(t, "run-1")
		ctx := context.Background()
		if err := mgr.Stop(ctx, "my-agent", "", "run-1"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if _, err := cs.CoreV1().Pods("scion-agents").Get(ctx, "my-agent", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
			t.Fatalf("pod still present after stop (err=%v)", err)
		}
		if len(*defaultRequests) != 0 {
			t.Fatalf("requests sent to the default namespace: %v", *defaultRequests)
		}
	})
	t.Run("other run", func(t *testing.T) {
		mgr, cs, defaultRequests := newNamespacedPodManager(t, "run-1")
		ctx := context.Background()
		if err := mgr.Stop(ctx, "my-agent", "", "run-0"); !errors.Is(err, ErrStopRunNotFound) {
			t.Fatalf("stop: err = %v, want ErrStopRunNotFound", err)
		}
		if _, err := cs.CoreV1().Pods("scion-agents").Get(ctx, "my-agent", metav1.GetOptions{}); err != nil {
			t.Fatalf("pod of run-1 removed by a run-0 stop (err=%v)", err)
		}
		if len(*defaultRequests) != 0 {
			t.Fatalf("requests sent to the default namespace: %v", *defaultRequests)
		}
	})
}

// addPodIn adds a same-named agent pod "my-agent" in namespace ns, labelled
// with runID when it is not empty.
func addPodIn(t *testing.T, cs *k8sfake.Clientset, ns, runID string) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "my-agent", Namespace: ns,
		Labels: map[string]string{"scion.name": "my-agent", "scion.agent": "true"},
	}}
	if runID != "" {
		pod.Labels[api.LabelRunID] = runID
	}
	if _, err := cs.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func podPresent(t *testing.T, cs *k8sfake.Clientset, ns string) bool {
	t.Helper()
	_, err := cs.CoreV1().Pods(ns).Get(context.Background(), "my-agent", metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// Same-named pods in two namespaces are two containers to Manager.Stop and
// Manager.Delete (DedupeByContainerID keys on the operation ID): a
// project-blind legacy stop or delete is ambiguous and removes neither,
// and a run-scoped stop removes only its own run's pod.
func TestManagerStopAndDelete_SameNamedPodsInTwoNamespaces(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"stop", "delete"} {
		t.Run("legacy "+op+" is ambiguous", func(t *testing.T) {
			mgr, cs, _ := newNamespacedPodManager(t, "run-1")
			addPodIn(t, cs, "ns-b", "run-2")
			var err error
			if op == "stop" {
				err = mgr.Stop(ctx, "my-agent", "", "")
			} else {
				_, err = mgr.Delete(ctx, "my-agent", false, "", false)
			}
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("%s: err = %v, want ambiguous", op, err)
			}
			if !podPresent(t, cs, "scion-agents") || !podPresent(t, cs, "ns-b") {
				t.Fatalf("an ambiguous %s removed a pod", op)
			}
		})
	}
	t.Run("run-scoped stop finds its own run's pod", func(t *testing.T) {
		mgr, cs, defaultRequests := newNamespacedPodManager(t, "run-1")
		addPodIn(t, cs, "ns-b", "run-2")
		if err := mgr.Stop(ctx, "my-agent", "", "run-2"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if podPresent(t, cs, "ns-b") {
			t.Error("run-2's pod in ns-b still present")
		}
		if !podPresent(t, cs, "scion-agents") {
			t.Error("run-1's pod was removed by a stop of run-2")
		}
		if len(*defaultRequests) != 0 {
			t.Fatalf("requests sent to the default namespace: %v", *defaultRequests)
		}
	})
}

// DedupeByContainerID keys on the operation ID: same-named Kubernetes pods
// in two namespaces stay distinct, while the same pod listed twice, or the
// same Docker/Podman/Apple container ID listed twice (no Kubernetes
// metadata, so the key is the container ID as before), collapses to one.
func TestDedupeByContainerID_OperationID(t *testing.T) {
	pod := func(ns string) api.AgentInfo {
		return api.AgentInfo{Name: "dev", ContainerID: "dev", Kubernetes: &api.AgentK8sMetadata{Namespace: ns, PodName: "dev"}}
	}
	docker := func(id string) api.AgentInfo { return api.AgentInfo{Name: "dev", ContainerID: id, Runtime: "docker"} }
	cases := []struct {
		name string
		in   []api.AgentInfo
		want int
	}{
		{"pods in two namespaces stay distinct", []api.AgentInfo{pod("ns-a"), pod("ns-b")}, 2},
		{"the same pod listed twice collapses", []api.AgentInfo{pod("ns-a"), pod("ns-a")}, 1},
		{"the same docker container listed twice collapses", []api.AgentInfo{docker("3f2a"), docker("3f2a")}, 1},
		{"two docker containers stay distinct", []api.AgentInfo{docker("3f2a"), docker("9b1c")}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(DedupeByContainerID(tc.in)); got != tc.want {
				t.Errorf("len(DedupeByContainerID) = %d, want %d", got, tc.want)
			}
		})
	}
}

// A run-scoped Manager.Stop of a legacy entry with no run label sends the
// requested run to the runtime (as the broker's run-scoped stop and delete
// do), so a run-checking runtime still refuses a pod another run recreated;
// a labelled entry keeps its own run.
func TestManagerStop_RunScoped_LegacyEntryCarriesRequestedRun(t *testing.T) {
	for _, tc := range []struct{ name, entryRun, want string }{
		{"legacy entry", "", "run-req"},
		{"labelled entry", "run-req", "run-req"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []runtime.RunRef
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{{Name: "dev", ContainerID: "cid-1", RunID: tc.entryRun}}, nil
				},
				StopFunc: func(_ context.Context, ref runtime.RunRef) error {
					got = append(got, ref)
					return nil
				},
			}
			if err := NewManager(rt).Stop(context.Background(), "dev", "", "run-req"); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if len(got) != 1 || got[0] != (runtime.RunRef{ID: "cid-1", RunID: tc.want}) {
				t.Errorf("runtime Stop refs = %+v, want one {cid-1 %s}", got, tc.want)
			}
		})
	}
}
