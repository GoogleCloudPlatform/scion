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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestLabelKey_PerRuntime(t *testing.T) {
	cr := &CloudRunRuntime{}
	for key, want := range map[string]string{
		api.LabelRuntimeBrokerID: "scion_runtime_broker_id",
		"scion.project_id":       "scion_project_id",
		"scion.name":             "scion_name",
		"agent_id":               "agent_id",
	} {
		if got := LabelKey(cr, key); got != want {
			t.Errorf("Cloud Run key for %q = %q, want %q", key, got, want)
		}
	}
	for name, rt := range map[string]Runtime{
		"mock":             &MockRuntime{},
		"docker":           &DockerRuntime{},
		"kubernetes":       &KubernetesRuntime{},
		"cloudrun-sandbox": &CloudRunSandboxRuntime{},
	} {
		for _, key := range []string{api.LabelRuntimeBrokerID, "scion.project_id", "scion.name"} {
			if got := LabelKey(rt, key); got != key {
				t.Errorf("%s key for %q = %q, want it unchanged", name, key, got)
			}
		}
	}
}

// TestCloudRunRun_StoresOwnerLabelUnderItsKey: the owner label is stored
// under the key LabelKey names, with its value unchanged, so List
// returns it where the ownership filter reads it.
func TestCloudRunRun_StoresOwnerLabelUnderItsKey(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	cfg := runConfigForTest()
	key := LabelKey(rt, api.LabelRuntimeBrokerID)
	const brokerID = "0b7c6f1e-3d2a-4c5b-9e8f-1a2b3c4d5e6f"
	cfg.Labels[key] = brokerID

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
	}
	if got := fake.createReqs[0].GetInstance().GetLabels()[key]; got != brokerID {
		t.Errorf("stored owner label %q = %q, want %q", key, got, brokerID)
	}
}

// TestCloudRunRun_RefusesOwnerLabelItCannotStoreExactly: a launch whose
// owner label would be rewritten, or overwritten by another label landing
// on the same stored key, is refused before any instance is created.
func TestCloudRunRun_RefusesOwnerLabelItCannotStoreExactly(t *testing.T) {
	key := LabelKey(&CloudRunRuntime{}, api.LabelRuntimeBrokerID)
	for name, tc := range map[string]struct {
		labels  map[string]string
		wantErr string
	}{
		"value rewritten (uppercase)": {
			labels:  map[string]string{key: "Broker-A"},
			wantErr: "owner label value",
		},
		"value rewritten (dot)": {
			labels:  map[string]string{key: "broker.a"},
			wantErr: "owner label value",
		},
		"another key lands on the owner key": {
			labels:  map[string]string{key: "broker-a", api.LabelRuntimeBrokerID: "broker-b"},
			wantErr: "would be stored under the owner label key",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newFakeCloudRunRuntime(t, fake)
			cfg := runConfigForTest()
			for k, v := range tc.labels {
				cfg.Labels[k] = v
			}
			_, err := rt.Run(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Run error = %v, want one containing %q", err, tc.wantErr)
			}
			if len(fake.createReqs) != 0 || len(fake.getReqs) != 0 {
				t.Errorf("Instances API called (get %d, create %d), want no calls", len(fake.getReqs), len(fake.createReqs))
			}
		})
	}
}

// TestCloudRunRun_OwnedLaunchNeedsExactIdentityLabels: an owned launch
// (owner label set) whose project ID, agent ID or name value Run would
// rewrite is refused before any instance is created, since ownership
// checks match those stored values exactly. A launch without an owner
// label is not affected.
func TestCloudRunRun_OwnedLaunchNeedsExactIdentityLabels(t *testing.T) {
	key := LabelKey(&CloudRunRuntime{}, api.LabelRuntimeBrokerID)
	for name, labels := range map[string]map[string]string{
		"project ID": {key: "broker-a", "scion.project_id": "Proj-1", "scion.name": "worker"},
		"agent ID":   {key: "broker-a", "agent_id": "Agent.1", "scion.name": "worker"},
		"name":       {key: "broker-a", "scion.name": "-worker-"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newFakeCloudRunRuntime(t, fake)
			cfg := runConfigForTest()
			for k, v := range labels {
				cfg.Labels[k] = v
			}
			_, err := rt.Run(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), "would be rewritten") {
				t.Fatalf("owned launch with a rewritten %s: Run error = %v, want a refusal", name, err)
			}
			if len(fake.createReqs) != 0 || len(fake.getReqs) != 0 {
				t.Errorf("Instances API called (get %d, create %d), want no calls", len(fake.getReqs), len(fake.createReqs))
			}
		})
	}

	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	cfg := runConfigForTest()
	cfg.Labels["scion.project_id"] = "Proj-1"
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("a launch without an owner label is refused: %v", err)
	}
}
