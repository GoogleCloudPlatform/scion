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
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildCommonRunArgs_RunDeadlineLabel(t *testing.T) {
	deadline := time.Date(2026, 11, 26, 20, 15, 0, 0, time.UTC)

	t.Run("emits label when RunDeadline is set and ignores spoofed labels", func(t *testing.T) {
		cfg := RunConfig{
			Name:                "deadline-agent",
			Image:               "example/image:latest",
			HomeDir:             t.TempDir(),
			Workspace:           t.TempDir(),
			NoAuth:              true,
			RunDeadline:         deadline,
			RunDeadlineDuration: 7 * time.Minute,
			Labels: map[string]string{
				"scion.name":         "deadline-agent",
				api.LabelRunDeadline: "2099-01-01T00:00:00Z", // must be ignored
			},
			Annotations: map[string]string{
				api.LabelRunDeadline: "2099-01-01T00:00:00Z", // must be ignored
			},
		}
		args, err := buildCommonRunArgs(cfg)
		if err != nil {
			t.Fatalf("buildCommonRunArgs: %v", err)
		}
		var found []string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--label" && strings.HasPrefix(args[i+1], api.LabelRunDeadline+"=") {
				found = append(found, args[i+1])
			}
		}
		want := api.LabelRunDeadline + "=" + deadline.Format(time.RFC3339)
		if len(found) != 1 || found[0] != want {
			t.Fatalf("run-deadline labels = %v, want [%s]", found, want)
		}
	})

	t.Run("omits label when RunDeadline is zero", func(t *testing.T) {
		cfg := RunConfig{
			Name:      "no-deadline-agent",
			Image:     "example/image:latest",
			HomeDir:   t.TempDir(),
			Workspace: t.TempDir(),
			NoAuth:    true,
			Labels: map[string]string{
				api.LabelRunDeadline: "2099-01-01T00:00:00Z", // must be ignored
			},
		}
		args, err := buildCommonRunArgs(cfg)
		if err != nil {
			t.Fatalf("buildCommonRunArgs: %v", err)
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--label" && strings.HasPrefix(args[i+1], api.LabelRunDeadline+"=") {
				t.Fatalf("unexpected run-deadline label: %s", args[i+1])
			}
		}
	})
}

func TestKubernetesRuntime_RunDeadlineAnnotationAndActiveDeadlineSeconds(t *testing.T) {
	deadline := time.Date(2026, 11, 26, 20, 30, 0, 0, time.UTC)
	rt, _, _ := newTestK8sRuntime()

	cfg := RunConfig{
		Name:                "k8s-deadline-agent",
		Image:               "example/image:latest",
		UnixUsername:        "scion",
		RunDeadline:         deadline,
		RunDeadlineDuration: 12 * time.Minute,
		Labels: map[string]string{
			"scion.name":   "k8s-deadline-agent",
			api.LabelRunID: "run-k8s-1",
		},
	}

	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if got := pod.Annotations[api.LabelRunDeadline]; got != deadline.Format(time.RFC3339) {
		t.Errorf("pod annotation %s = %q, want %q", api.LabelRunDeadline, got, deadline.Format(time.RFC3339))
	}
	if _, inLabels := pod.Labels[api.LabelRunDeadline]; inLabels {
		t.Errorf("pod labels must not contain %s (colons are invalid in k8s label values)", api.LabelRunDeadline)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil {
		t.Fatalf("pod.Spec.ActiveDeadlineSeconds is nil, want 720")
	}
	if *pod.Spec.ActiveDeadlineSeconds != 720 {
		t.Errorf("pod.Spec.ActiveDeadlineSeconds = %d, want 720", *pod.Spec.ActiveDeadlineSeconds)
	}

	// Zero deadline must leave annotation and ActiveDeadlineSeconds unset.
	cfgZero := cfg
	cfgZero.RunDeadline = time.Time{}
	cfgZero.RunDeadlineDuration = 0
	podZero, err := rt.buildPod("default", cfgZero)
	if err != nil {
		t.Fatalf("buildPod (zero): %v", err)
	}
	if _, ok := podZero.Annotations[api.LabelRunDeadline]; ok {
		t.Errorf("unexpected %s annotation when RunDeadline is zero", api.LabelRunDeadline)
	}
	if podZero.Spec.ActiveDeadlineSeconds != nil {
		t.Errorf("ActiveDeadlineSeconds = %d, want nil when RunDeadline is zero", *podZero.Spec.ActiveDeadlineSeconds)
	}
}

func TestKubernetesRuntime_List_DeadlineExceededMapsToLimitsExceeded(t *testing.T) {
	deadline := time.Date(2026, 11, 26, 20, 30, 0, 0, time.UTC)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "k8s-expired-agent",
			Namespace: "default",
			Labels: map[string]string{
				"scion.name":   "k8s-expired-agent",
				api.LabelRunID: "run-k8s-expired",
			},
			Annotations: map[string]string{
				api.LabelRunDeadline: deadline.Format(time.RFC3339),
			},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodFailed,
			Reason: "DeadlineExceeded",
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: agentContainerName,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137,
							Reason:   "Error",
						},
					},
				},
			},
		},
	}

	cs := fake.NewSimpleClientset(pod)
	rt := NewKubernetesRuntime(&k8s.Client{Clientset: cs})
	rt.DefaultNamespace = "default"

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("len(agents) = %d, want 1", len(agents))
	}
	got := agents[0]
	if got.Phase != string(state.PhaseStopped) {
		t.Errorf("Phase = %q, want %q", got.Phase, state.PhaseStopped)
	}
	if got.Activity != string(state.ActivityLimitsExceeded) {
		t.Errorf("Activity = %q, want %q", got.Activity, state.ActivityLimitsExceeded)
	}
	if got.ExitReason != string(state.ExitReasonLimitsExceeded) {
		t.Errorf("ExitReason = %q, want %q", got.ExitReason, state.ExitReasonLimitsExceeded)
	}
	if got.Detail == nil || got.Detail.Message != api.ReasonMaxDurationBackstop {
		t.Errorf("Detail = %+v, want Message=%q", got.Detail, api.ReasonMaxDurationBackstop)
	}
	if got.Annotations[api.LabelRunDeadline] != deadline.Format(time.RFC3339) {
		t.Errorf("Annotations[%s] = %q, want %q", api.LabelRunDeadline, got.Annotations[api.LabelRunDeadline], deadline.Format(time.RFC3339))
	}
}

type fakeCloudRunListAPI struct {
	cloudrun.InstancesAPI
	instances []*runpb.Instance
}

type fakeCloudRunIter struct {
	items []*runpb.Instance
	idx   int
}

func (it *fakeCloudRunIter) Next() (*runpb.Instance, error) {
	if it.idx >= len(it.items) {
		return nil, iterator.Done
	}
	v := it.items[it.idx]
	it.idx++
	return v, nil
}

func (f *fakeCloudRunListAPI) ListInstances(context.Context, *runpb.ListInstancesRequest, ...gax.CallOption) cloudrun.InstanceIterator {
	return &fakeCloudRunIter{items: f.instances}
}

func (f *fakeCloudRunListAPI) Close() error { return nil }

func TestCloudRunRuntime_RunDeadlineAnnotationAndList(t *testing.T) {
	deadline := time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID:      "test-proj",
		Location:       "us-central1",
		ServiceAccount: "sa@test-proj.iam.gserviceaccount.com",
	})
	if err != nil {
		t.Fatalf("NewCloudRunRuntime: %v", err)
	}

	cfg := goldenInstanceCfg()
	cfg.RunDeadline = deadline
	inst, err := rt.buildCloudRunInstance(cfg, 1000, 1000, goldenInstancePaths())
	if err != nil {
		t.Fatalf("buildCloudRunInstance: %v", err)
	}
	if got := inst.Annotations[api.LabelRunDeadline]; got != deadline.Format(time.RFC3339) {
		t.Fatalf("inst.Annotations[%s] = %q, want %q", api.LabelRunDeadline, got, deadline.Format(time.RFC3339))
	}
	if _, inLabels := inst.Labels[sanitizeGCPLabelKey(api.LabelRunDeadline)]; inLabels {
		t.Errorf("inst.Labels must not carry %s (sanitization would mangle RFC3339)", api.LabelRunDeadline)
	}

	inst.Name = "projects/test-proj/locations/us-central1/instances/cr-deadline-agent"
	inst.TerminalCondition = &runpb.Condition{State: runpb.Condition_CONDITION_SUCCEEDED}
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) {
		return &fakeCloudRunListAPI{instances: []*runpb.Instance{inst}}, nil
	}
	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("CloudRunRuntime.List: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("len(agents) = %d, want 1", len(agents))
	}
	if got := agents[0].Annotations[api.LabelRunDeadline]; got != deadline.Format(time.RFC3339) {
		t.Errorf("AgentInfo.Annotations[%s] = %q, want %q", api.LabelRunDeadline, got, deadline.Format(time.RFC3339))
	}
}
