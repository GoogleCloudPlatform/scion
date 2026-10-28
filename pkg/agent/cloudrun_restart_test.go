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
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
)

// Stop then start of a Cloud Run agent (ptone/scion#2550 P4, D1): Cloud
// Run's Run refuses to reuse an instance of another run, so a start after
// a stop relies on Start's pre-clean removing the stopped instance first.
// The entry has no Phase and GCP-sanitized label keys; the next test feeds
// the pre-clean the entry CloudRunRuntime.List itself reports. The
// pre-clean must delete it with its own run before Run is called for the
// new run.
func TestStart_CloudRunStoppedInstancePreCleanedWithItsRun(t *testing.T) {
	projectScionDir, _ := startTZFixture(t, "", `""`)
	var calls []string
	var deleted runtime.RunRef
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			if len(calls) > 0 {
				return nil, nil // after the pre-clean and the Run
			}
			return []api.AgentInfo{{
				ID:              "agent-1",
				ContainerID:     "agent-tz-agent-0123456789",
				RunID:           "run-a",
				Name:            "projects/p/locations/l/instances/agent-tz-agent-0123456789",
				ContainerStatus: "CONDITION_SUCCEEDED",
				ProjectID:       identityProjB,
				Labels:          map[string]string{"scion_name": "tz-agent", "scion_run_id": "run-a", "agent_id": "agent-1", "scion_project_id": identityProjB},
			}}, nil
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			calls = append(calls, "delete")
			deleted = ref
			return nil
		},
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			calls = append(calls, "run:"+cfg.Labels[api.LabelRunID])
			return "agent-tz-agent-0123456789", nil
		},
	}
	if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-b",
		Env: map[string]string{"SCION_PROJECT_ID": identityProjB},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(calls) < 2 || calls[0] != "delete" || calls[1] != "run:run-b" {
		t.Fatalf("runtime calls = %v, want delete then run:run-b", calls)
	}
	if deleted.ID != "agent-tz-agent-0123456789" || deleted.RunID != "run-a" {
		t.Errorf("pre-clean deleted %+v, want the stopped instance with run-a", deleted)
	}
}

// Stop then start with the entry CloudRunRuntime.List reports for a
// stopped instance, in the shape recorded after StopInstance
// (ptone/scion#5295): List reports it as stopped, so Start's pre-clean
// must delete it with its own run, then Run the new run.
func TestStart_CloudRunStoppedInstanceFromListPreCleaned(t *testing.T) {
	projectScionDir, _ := startTZFixture(t, "", `""`)
	const instance = "projects/test-project/locations/us-central1/instances/agent-tz-agent-0123456789"
	fake := &crStopInstances{instances: []*runpb.Instance{{
		Name: instance,
		Labels: map[string]string{
			"agent_id":     "agent-1",
			"scion_agent":  "true",
			"scion_name":   "tz-agent",
			"scion_run_id": "run-a",
			// The project ID a hub-dispatched instance carries, under its
			// GCP-sanitized key; List reports it as ProjectID.
			"scion_project_id": identityProjB,
		},
		TerminalCondition: &runpb.Condition{Type: "Running", State: runpb.Condition_CONDITION_FAILED, Message: "Instance stopped."},
		Conditions: []*runpb.Condition{
			{Type: "ContainerReady", State: runpb.Condition_CONDITION_SUCCEEDED, Message: "Imported container image in 2.66s."},
			{Type: "ResourcesAvailable", State: runpb.Condition_CONDITION_SUCCEEDED, Message: "Provisioned imported containers."},
		},
	}}}
	cr, err := runtime.NewCloudRunRuntimeForTest(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}

	var calls []string
	var deleted runtime.RunRef
	rt := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			if len(calls) > 0 {
				return nil, nil // after the pre-clean and the Run
			}
			return cr.List(ctx, filter)
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			calls = append(calls, "delete")
			deleted = ref
			return nil
		},
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			calls = append(calls, "run:"+cfg.Labels[api.LabelRunID])
			return "agent-tz-agent-0123456789", nil
		},
	}
	if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-b",
		Env: map[string]string{"SCION_PROJECT_ID": identityProjB},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(calls) < 2 || calls[0] != "delete" || calls[1] != "run:run-b" {
		t.Fatalf("runtime calls = %v, want delete then run:run-b", calls)
	}
	if deleted.ID != "agent-tz-agent-0123456789" || deleted.RunID != "run-a" {
		t.Errorf("pre-clean deleted %+v, want the listed instance with run-a", deleted)
	}
}

// Start with no task, for an agent whose Cloud Run instance is running, in
// the shape recorded for a started instance (ptone/scion#5295): List
// reports it as running, so Start returns it as is, with no delete and no
// new Run. Before, List reported a running instance as provisioning and
// Start deleted and recreated it.
func TestStart_CloudRunRunningInstanceFromListKept(t *testing.T) {
	projectScionDir, _ := startTZFixture(t, "", `""`)
	const instance = "projects/test-project/locations/us-central1/instances/agent-tz-agent-0123456789"
	fake := &crStopInstances{instances: []*runpb.Instance{{
		Name: instance,
		Labels: map[string]string{
			"agent_id":     "agent-1",
			"scion_agent":  "true",
			"scion_name":   "tz-agent",
			"scion_run_id": "run-a",
			// The project ID a hub-dispatched instance carries, under its
			// GCP-sanitized key; List reports it as ProjectID. Start's
			// pre-clean keeps only entries of its project (matchAgentProject).
			"scion_project_id": identityProjB,
		},
		TerminalCondition: &runpb.Condition{Type: "Running", State: runpb.Condition_CONDITION_SUCCEEDED, Message: "Started instance in 2.54s."},
		Conditions: []*runpb.Condition{
			{Type: "Retry", State: runpb.Condition_CONDITION_SUCCEEDED, Reasons: &runpb.Condition_Reason{Reason: runpb.Condition_WAITING_FOR_OPERATION}, Message: "System will retry after 01:00 from lastTransitionTime for polling interval", Severity: runpb.Condition_INFO},
			{Type: "ContainerReady", State: runpb.Condition_CONDITION_SUCCEEDED, Message: "Imported container image in 2.66s."},
			{Type: "ResourcesAvailable", State: runpb.Condition_CONDITION_SUCCEEDED, Message: "Provisioned imported containers."},
		},
	}}}
	cr, err := runtime.NewCloudRunRuntimeForTest(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}

	var calls []string
	rt := &runtime.MockRuntime{
		ListFunc: cr.List,
		DeleteFunc: func(context.Context, runtime.RunRef) error {
			calls = append(calls, "delete")
			return nil
		},
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			calls = append(calls, "run:"+cfg.Labels[api.LabelRunID])
			return "agent-tz-agent-0123456789", nil
		},
	}
	got, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-b",
		Env: map[string]string{"SCION_PROJECT_ID": identityProjB},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("runtime calls = %v, want none: the running instance is kept", calls)
	}
	if got.ContainerID != "agent-tz-agent-0123456789" || got.RunID != "run-a" {
		t.Errorf("Start returned ContainerID=%q RunID=%q, want the running instance with run-a", got.ContainerID, got.RunID)
	}
}
