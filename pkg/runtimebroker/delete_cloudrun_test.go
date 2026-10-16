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

package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// crInstances is a minimal cloudrun.InstancesAPI double: it lists and gets
// a fixed set of instances and records the stop and delete requests.
type crInstances struct {
	mu        sync.Mutex
	instances []*runpb.Instance
	deleted   []string
}

type crOp struct{}

func (crOp) Wait(context.Context, ...gax.CallOption) (*runpb.Instance, error) { return nil, nil }

type crIter struct{ instances []*runpb.Instance }

func (it *crIter) Next() (*runpb.Instance, error) {
	if len(it.instances) == 0 {
		return nil, iterator.Done
	}
	inst := it.instances[0]
	it.instances = it.instances[1:]
	return inst, nil
}

func (c *crInstances) GetInstance(_ context.Context, req *runpb.GetInstanceRequest, _ ...gax.CallOption) (*runpb.Instance, error) {
	for _, inst := range c.instances {
		if inst.GetName() == req.GetName() {
			return inst, nil
		}
	}
	return nil, status.Error(codes.NotFound, "instance does not exist")
}

func (c *crInstances) CreateInstance(context.Context, *runpb.CreateInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected CreateInstance")
}

func (c *crInstances) StartInstance(context.Context, *runpb.StartInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected StartInstance")
}

func (c *crInstances) StopInstance(context.Context, *runpb.StopInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected StopInstance")
}

func (c *crInstances) DeleteInstance(_ context.Context, req *runpb.DeleteInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, req.GetName())
	return crOp{}, nil
}

func (c *crInstances) ListInstances(context.Context, *runpb.ListInstancesRequest, ...gax.CallOption) cloudrun.InstanceIterator {
	return &crIter{instances: append([]*runpb.Instance(nil), c.instances...)}
}

func (c *crInstances) Close() error { return nil }

// crInstance is the Cloud Run instance of agent "dev" in projectID, with the
// GCP-sanitized labels CloudRunRuntime.Run gives it.
func crInstance(instanceID, projectID, runID string) *runpb.Instance {
	return &runpb.Instance{
		Name: "projects/test-project/locations/us-central1/instances/" + instanceID,
		Labels: map[string]string{
			"agent_id":         instanceID + "-hub-id",
			"scion_agent":      "true",
			"scion_name":       "dev",
			"scion_project_id": projectID,
			"scion_run_id":     runID,
		},
	}
}

// TestDeleteAgent_CloudRun_HubNameDeletesInstance: a hub delete names the
// agent ("dev"), while the Cloud Run instance is named by a hashed instance
// ID and carries the agent name only in its GCP-sanitized scion_name label.
// The broker's delete resolution must still find the instance and delete
// it, rather than deleting only the agent's files and leaving the instance
// behind (ptone/scion#5268). A same-named agent of another project must
// keep its instance and files.
func TestDeleteAgent_CloudRun_HubNameDeletesInstance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	fake := &crInstances{instances: []*runpb.Instance{
		crInstance("agent-dev-aaaaaaaaaa", scopeProjA, "run-a"),
		crInstance("agent-dev-bbbbbbbbbb", scopeProjB, "run-b"),
	}}
	rt, err := runtime.NewCloudRunRuntimeForTest(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	srv := New(cfg, agent.NewManager(rt), rt)
	// The agents' files exist, so a resolution that misses the instance
	// still answers 204 (a files-only delete).
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	makeHubProject(t, home, "proj-b", scopeProjB, "dev")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	deleted := append([]string(nil), fake.deleted...)
	fake.mu.Unlock()
	want := "projects/test-project/locations/us-central1/instances/agent-dev-bbbbbbbbbb"
	if len(deleted) != 1 || deleted[0] != want {
		t.Errorf("DeleteInstance calls = %q, want exactly one, for %s", deleted, want)
	}
	assertUntouched(t, scionA, "dev", infoA)
}
