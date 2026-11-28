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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	ateapipb "github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
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

	cs := fake.NewClientset(pod)
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

func TestSubstrateRuntime_RunDeadlineLabel(t *testing.T) {
	deadline := time.Date(2026, 11, 26, 22, 0, 0, 0, time.UTC)
	callerLabels := map[string]string{
		"scion.name":   "sub-agent",
		api.LabelRunID: "run-sub-1",
	}

	got := substrateRecordLabels(RunConfig{
		Name:        "sub-agent",
		RunDeadline: deadline,
		Labels:      callerLabels,
	})
	if got[api.LabelRunDeadline] != deadline.Format(time.RFC3339) {
		t.Fatalf("substrateRecordLabels[%s] = %q, want %q", api.LabelRunDeadline, got[api.LabelRunDeadline], deadline.Format(time.RFC3339))
	}
	if _, mutated := callerLabels[api.LabelRunDeadline]; mutated {
		t.Errorf("substrateRecordLabels must not mutate caller cfg.Labels")
	}

	// Zero deadline must leave label unset.
	gotZero := substrateRecordLabels(RunConfig{
		Name:   "sub-agent",
		Labels: callerLabels,
	})
	if _, ok := gotZero[api.LabelRunDeadline]; ok {
		t.Errorf("unexpected %s label when RunDeadline is zero", api.LabelRunDeadline)
	}

	// Verify SubstrateRuntime.List with the sweeper's scion.agent=true filter
	// surfaces the stamped scion.dev/run-deadline label from substrateAgentRecords.
	rec := &callRecorder{}
	rt, fc, _, closeServer := newTestSubstrateHarness(t, rec)
	defer closeServer()

	substrateAgentStateMu.Lock()
	substrateAgentRecords["uid-deadline"] = &substrateAgentRecord{
		Labels:  got,
		Project: "proj-sub",
	}
	substrateAgentStateMu.Unlock()
	t.Cleanup(func() {
		substrateAgentStateMu.Lock()
		delete(substrateAgentRecords, "uid-deadline")
		substrateAgentStateMu.Unlock()
	})

	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{
			Actors: []*ateapipb.Actor{
				{
					Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-proj", Name: "sub-agent", Uid: "uid-deadline"},
					Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
				},
			},
		}, nil
	}

	listed, err := rt.List(context.Background(), map[string]string{"scion.agent": "true"})
	if err != nil {
		t.Fatalf("SubstrateRuntime.List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len(listed) = %d, want 1", len(listed))
	}
	if listed[0].Labels["scion.agent"] != "true" {
		t.Errorf("listed[0].Labels[scion.agent] = %q, want \"true\"", listed[0].Labels["scion.agent"])
	}
	if listed[0].Labels[api.LabelRunDeadline] != deadline.Format(time.RFC3339) {
		t.Errorf("listed[0].Labels[%s] = %q, want %q", api.LabelRunDeadline, listed[0].Labels[api.LabelRunDeadline], deadline.Format(time.RFC3339))
	}
}

func TestParseDockerListLabels_DuplicateRunDeadlineFailsClosed(t *testing.T) {
	earlier := "2026-11-26T18:00:00Z"
	later := "2099-01-01T00:00:00Z"
	expired := "1970-01-01T00:00:00Z"

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "single deadline preserved",
			raw:  "scion.agent=true," + api.LabelRunDeadline + "=" + earlier,
			want: earlier,
		},
		{
			name: "real earlier deadline followed by spoofed future deadline keeps earlier",
			raw:  api.LabelRunDeadline + "=" + earlier + ",other=x," + api.LabelRunDeadline + "=" + later,
			want: earlier,
		},
		{
			name: "spoofed future deadline followed by real earlier deadline keeps earlier",
			raw:  "other=x," + api.LabelRunDeadline + "=" + later + "," + api.LabelRunDeadline + "=" + earlier,
			want: earlier,
		},
		{
			name: "real deadline followed by unparsable duplicate fails closed to expired",
			raw:  api.LabelRunDeadline + "=" + earlier + "," + api.LabelRunDeadline + "=not-a-timestamp",
			want: expired,
		},
		{
			name: "unparsable duplicate followed by real deadline fails closed to expired",
			raw:  api.LabelRunDeadline + "=not-a-timestamp," + api.LabelRunDeadline + "=" + earlier,
			want: expired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDockerListLabels(tc.raw)
			if got[api.LabelRunDeadline] != tc.want {
				t.Errorf("parseDockerListLabels(%q)[%s] = %q, want %q", tc.raw, api.LabelRunDeadline, got[api.LabelRunDeadline], tc.want)
			}
		})
	}

	// Residual R1: a comma-injected duplicate scion.agent=false must not drop
	// a Scion-managed container from the scion.agent=true sweep in either order.
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"true then false keeps true", "scion.agent=true,other=x,scion.agent=false"},
		{"false then true keeps true", "other=x,scion.agent=false,scion.agent=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDockerListLabels(tc.raw)
			if got["scion.agent"] != "true" {
				t.Errorf("parseDockerListLabels(%q)[scion.agent] = %q, want \"true\"", tc.raw, got["scion.agent"])
			}
		})
	}
}

func TestDockerRuntime_List_AuthoritativeLabelsPreventCommaInjection(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	argsFile := filepath.Join(tmpDir, "args")

	realDeadline := "2026-11-26T18:00:00Z"
	// Simulate Docker ps output where a user-controlled label value contains
	// commas attempting to spoof scion.dev/run-deadline, scion.agent,
	// scion.run_id, scion.name, agent_id, and scion.project_id, while Docker's
	// direct `.Label "<key>"` extractions carry the true container label values.
	line1 := `{"ID":"ctr-1","Names":"proj--real-agent","Status":"Up 5 minutes","Image":"scion:latest","Labels":"scion.agent=true,scion.name=real-agent,scion.run_id=real-run,agent_id=real-id,scion.project_id=real-proj,` + api.LabelRunDeadline + `=` + realDeadline + `,user.note=x,scion.agent=false,scion.name=spoof,scion.run_id=spoof-run,agent_id=spoof-id,scion.project_id=spoof-proj,` + api.LabelRunDeadline + `=2099-01-01T00:00:00Z","RunDeadline":"` + realDeadline + `","ScionAgent":"true","RunID":"real-run","ScionName":"real-agent","AgentID":"real-id","ProjectID":"real-proj"}`
	// Second container has no run-deadline label at all (RunDeadline="") even
	// though a comma-injected value appears in .Labels.
	line2 := `{"ID":"ctr-2","Names":"proj--no-deadline","Status":"Up 5 minutes","Image":"scion:latest","Labels":"scion.agent=true,scion.name=no-deadline,user.note=x,` + api.LabelRunDeadline + `=2099-01-01T00:00:00Z","RunDeadline":"","ScionAgent":"true","RunID":"run-2","ScionName":"no-deadline","AgentID":"id-2","ProjectID":"proj-2"}`

	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\nprintf '%s\\n%s\\n' '" + line1 + "' '" + line2 + "'\n"
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("write mock docker: %v", err)
	}

	rt := &DockerRuntime{Command: mockDocker}
	agents, err := rt.List(context.Background(), map[string]string{"scion.agent": "true"})
	if err != nil {
		t.Fatalf("DockerRuntime.List: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("len(agents) = %d, want 2", len(agents))
	}

	a1 := agents[0]
	if a1.Labels[api.LabelRunDeadline] != realDeadline {
		t.Errorf("a1 deadline = %q, want %q", a1.Labels[api.LabelRunDeadline], realDeadline)
	}
	if a1.Labels["scion.agent"] != "true" {
		t.Errorf("a1 scion.agent = %q, want \"true\"", a1.Labels["scion.agent"])
	}
	if a1.RunID != "real-run" || a1.Labels[api.LabelRunID] != "real-run" {
		t.Errorf("a1 RunID = %q (label %q), want real-run", a1.RunID, a1.Labels[api.LabelRunID])
	}
	if a1.Name != "real-agent" || a1.Labels["scion.name"] != "real-agent" {
		t.Errorf("a1 Name = %q (label %q), want real-agent", a1.Name, a1.Labels["scion.name"])
	}
	if a1.Labels["agent_id"] != "real-id" {
		t.Errorf("a1 agent_id = %q, want real-id", a1.Labels["agent_id"])
	}
	if a1.ProjectID != "real-proj" {
		t.Errorf("a1 ProjectID = %q, want real-proj", a1.ProjectID)
	}

	a2 := agents[1]
	if v, ok := a2.Labels[api.LabelRunDeadline]; ok {
		t.Errorf("a2 should have no %s label when authoritative .Label is empty, got %q", api.LabelRunDeadline, v)
	}

	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(rawArgs)
	if !strings.Contains(args, `.Label "`+api.LabelRunDeadline+`"`) {
		t.Errorf("docker ps format must extract %s via .Label; got %q", api.LabelRunDeadline, args)
	}
	if strings.Contains(args, ".Size") {
		t.Errorf("docker ps format must not reference .Size; got %q", args)
	}
}
