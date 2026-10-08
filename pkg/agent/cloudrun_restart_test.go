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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Stop then start of a Cloud Run agent (ptone/scion#2550 P4, D1): Cloud
// Run's Run refuses to reuse an instance of another run, so a start after
// a stop relies on Start's pre-clean removing the stopped instance first.
// The entry is shaped as CloudRunRuntime.List reports it: no Phase, and
// GCP-sanitized label keys. The pre-clean must delete it with its own run
// before Run is called for the new run.
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
				Labels:          map[string]string{"scion_name": "tz-agent", "scion_run_id": "run-a", "agent_id": "agent-1"},
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
