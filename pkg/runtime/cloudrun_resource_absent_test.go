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
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Exact absence of a Cloud Run instance a launch recorded: the run ID label
// against the handle UID, never the name alone.

const absentTestParent = "projects/test-project/locations/us-central1"

func absentTestHandle() api.ResourceHandle {
	return api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance, Namespace: absentTestParent,
		Name: "agent-1-abcdef0123", UID: "0f1e2d3c-4b5a-4968-8776-655443322110"}
}

// absentTestInstance is the instance of absentTestHandle as Run stores it,
// with runID under the GCP-sanitized run ID label key.
func absentTestInstance(runID string) *runpb.Instance {
	return &runpb.Instance{
		Name:   absentTestParent + "/instances/agent-1-abcdef0123",
		Labels: map[string]string{sanitizeGCPLabelKey(api.LabelRunID): runID, "scion_agent": "true"},
	}
}

func TestCloudRunResourceAbsent_NotFound(t *testing.T) {
	fake := &fakeInstancesClient{getErr: status.Error(codes.NotFound, "instance does not exist")}
	rt := newFakeCloudRunRuntime(t, fake)
	gone, err := rt.ResourceAbsent(context.Background(), absentTestHandle())
	if err != nil || !gone {
		t.Fatalf("NotFound: gone=%v err=%v, want absent", gone, err)
	}
	if len(fake.getReqs) != 1 || fake.getReqs[0].GetName() != absentTestParent+"/instances/agent-1-abcdef0123" {
		t.Fatalf("GetInstance requests = %v, want one for the recorded name", fake.getReqs)
	}
}

func TestCloudRunResourceAbsent_RunIDMismatchAbsent(t *testing.T) {
	h := absentTestHandle()
	for name, inst := range map[string]*runpb.Instance{
		"another run holds the name": absentTestInstance("99999999-0000-4000-8000-000000000000"),
		// A missing run label is absent for this run, never "present and
		// ours": the recorded instance always carried its run label.
		"run label missing": {Name: absentTestParent + "/instances/agent-1-abcdef0123"},
		"run label empty": {Name: absentTestParent + "/instances/agent-1-abcdef0123",
			Labels: map[string]string{sanitizeGCPLabelKey(api.LabelRunID): ""}},
	} {
		t.Run(name, func(t *testing.T) {
			rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{getInstance: inst})
			gone, err := rt.ResourceAbsent(context.Background(), h)
			if err != nil || !gone {
				t.Fatalf("gone=%v err=%v, want absent (a different object holds the name)", gone, err)
			}
		})
	}
	// The same name with the recorded run ID is present.
	rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{getInstance: absentTestInstance(h.UID)})
	gone, err := rt.ResourceAbsent(context.Background(), h)
	if err != nil || gone {
		t.Fatalf("same run: gone=%v err=%v, want present", gone, err)
	}
}

func TestCloudRunResourceAbsent_SoftDeletedPresent(t *testing.T) {
	inst := absentTestInstance(absentTestHandle().UID)
	inst.DeleteTime = timestamppb.New(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	inst.Reconciling = true
	rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{getInstance: inst})
	gone, err := rt.ResourceAbsent(context.Background(), absentTestHandle())
	if err != nil || gone {
		t.Fatalf("soft-deleted instance: gone=%v err=%v, want present until NotFound", gone, err)
	}
}

func TestCloudRunResourceAbsent_TransportErrorIsError(t *testing.T) {
	for name, getErr := range map[string]error{
		"unavailable":       status.Error(codes.Unavailable, "connection refused"),
		"permission denied": status.Error(codes.PermissionDenied, "denied"),
		"plain error":       errors.New("dial tcp: i/o timeout"),
	} {
		t.Run(name, func(t *testing.T) {
			rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{getErr: getErr})
			gone, err := rt.ResourceAbsent(context.Background(), absentTestHandle())
			if err == nil || gone {
				t.Fatalf("gone=%v err=%v, want an error and never absence", gone, err)
			}
		})
	}
	// A nil instance with no error is an error too.
	rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{})
	if gone, err := rt.ResourceAbsent(context.Background(), absentTestHandle()); err == nil || gone {
		t.Fatalf("nil instance: gone=%v err=%v, want an error", gone, err)
	}
}

func TestCloudRunResourceAbsent_WrongKindIsError(t *testing.T) {
	for name, mutate := range map[string]func(h *api.ResourceHandle){
		"container kind": func(h *api.ResourceHandle) { h.Kind = api.ResourceKindContainer },
		"pod kind":       func(h *api.ResourceHandle) { h.Kind = api.ResourceKindPod },
		"no UID":         func(h *api.ResourceHandle) { h.UID = "" },
		"no name":        func(h *api.ResourceHandle) { h.Name = "" },
		"no parent":      func(h *api.ResourceHandle) { h.Namespace = "" },
		"other parent":   func(h *api.ResourceHandle) { h.Namespace = "projects/other-project/locations/us-central1" },
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: status.Error(codes.NotFound, "instance does not exist")}
			rt := newFakeCloudRunRuntime(t, fake)
			h := absentTestHandle()
			mutate(&h)
			gone, err := rt.ResourceAbsent(context.Background(), h)
			if err == nil || gone {
				t.Fatalf("gone=%v err=%v, want an error and never absence", gone, err)
			}
			if len(fake.getReqs) != 0 {
				t.Fatalf("GetInstance called %d times for a refused handle", len(fake.getReqs))
			}
		})
	}
}

// TestCloudRunListedObjectHandle_MatchesLaunch: the handle built for a
// listed instance is the one Run records at launch (kind, parent, instance
// ID, run ID), so a reconstructed ownership record equals the launched one.
func TestCloudRunListedObjectHandle_MatchesLaunch(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	cfg := runConfigForTest()
	const runID = "0f1e2d3c-4b5a-4968-8776-655443322110"
	cfg.Labels[api.LabelRunID] = runID
	var launched []api.ResourceHandle
	cfg.OnResourceCreated = func(h api.ResourceHandle) { launched = append(launched, h) }
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(launched) != 1 || len(fake.createReqs) != 1 {
		t.Fatalf("launch handles %v, creates %d", launched, len(fake.createReqs))
	}
	created := fake.createReqs[0]
	fake.listResult = []*runpb.Instance{{Name: created.GetParent() + "/instances/" + created.GetInstanceId(), Labels: created.GetInstance().GetLabels()}}
	listed, err := rt.List(context.Background(), nil)
	if err != nil || len(listed) != 1 {
		t.Fatalf("List = %v, %v", listed, err)
	}
	if got := rt.ListedObjectHandle(listed[0]); got != launched[0] {
		t.Fatalf("listed handle %+v, launched %+v", got, launched[0])
	}
}

// TestFlatCloudRunCreate_LongProjectIDRefused: an owned Cloud Run launch
// whose owner, project, agent or name label value the GCP label rewrite
// would change is refused before anything is provisioned and before any
// Instances API call. Rows: a long-form (<uuid>__<slug>) project ID, an
// upper-case value and an over-length value. Canonical values at their
// bounds (lower-case UUIDs, a 63-character slug) are stored exactly as
// given.
func TestFlatCloudRunCreate_LongProjectIDRefused(t *testing.T) {
	const (
		owner   = "0b7c6f1e-3d2a-4c5b-9e8f-1a2b3c4d5e6f"
		project = "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
		agentID = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	)
	ownerKey := LabelKey(&CloudRunRuntime{}, api.LabelRuntimeBrokerID)
	longForm := project + "__" + strings.Repeat("p", 40)
	over := strings.Repeat("a", 64)
	for name, tc := range map[string]struct{ key, value string }{
		"project long form":  {"scion.project_id", longForm},
		"project upper case": {"scion.project_id", strings.ToUpper(project)},
		"project too long":   {"scion.project_id", over},
		"owner upper case":   {ownerKey, strings.ToUpper(owner)},
		"owner too long":     {ownerKey, over},
		"agent upper case":   {"agent_id", strings.ToUpper(agentID)},
		"agent too long":     {"agent_id", over},
		"name upper case":    {"scion.name", "Worker"},
		"name too long":      {"scion.name", over},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newFakeCloudRunRuntime(t, fake)
			provisioned := false
			rt.provisionNFS = func(context.Context, RunConfig, string, int, int) (*cloudRunNFSProvisionPaths, error) {
				provisioned = true
				return nil, errors.New("not reached")
			}
			cfg := runConfigForTest()
			cfg.Labels = map[string]string{ownerKey: owner, "scion.project_id": project, "agent_id": agentID, "scion.name": "worker"}
			cfg.Labels[tc.key] = tc.value
			_, err := rt.Run(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), "would be rewritten") {
				t.Fatalf("Run error = %v, want a refusal", err)
			}
			if provisioned || len(fake.getReqs) != 0 || len(fake.createReqs) != 0 {
				t.Fatalf("provisioned=%v, get %d, create %d: want nothing before the refusal", provisioned, len(fake.getReqs), len(fake.createReqs))
			}
		})
	}

	// Hub-dispatched values (lower-case UUIDs, a canonical slug at
	// api.MaxSlugLength) are stored exactly as given.
	slug := strings.Repeat("w", api.MaxSlugLength-2) + "-1"
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	cfg := runConfigForTest()
	cfg.Labels = map[string]string{ownerKey: owner, "scion.project_id": project, "agent_id": agentID, "scion.name": slug}
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run with canonical values: %v", err)
	}
	stored := fake.createReqs[0].GetInstance().GetLabels()
	for key, want := range map[string]string{ownerKey: owner, LabelKey(rt, "scion.project_id"): project,
		LabelKey(rt, "agent_id"): agentID, LabelKey(rt, "scion.name"): slug} {
		if stored[key] != want {
			t.Errorf("stored %s = %q, want exactly %q", key, stored[key], want)
		}
	}
}
