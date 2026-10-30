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
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// crStopInstances is a minimal cloudrun.InstancesAPI double: it lists and
// gets a fixed set of instances and records the stop requests.
type crStopInstances struct {
	instances []*runpb.Instance
	stopped   []string
}

type crStopOp struct{}

func (crStopOp) Wait(context.Context, ...gax.CallOption) (*runpb.Instance, error) { return nil, nil }

type crStopIter struct{ instances []*runpb.Instance }

func (it *crStopIter) Next() (*runpb.Instance, error) {
	if len(it.instances) == 0 {
		return nil, iterator.Done
	}
	inst := it.instances[0]
	it.instances = it.instances[1:]
	return inst, nil
}

func (c *crStopInstances) GetInstance(_ context.Context, req *runpb.GetInstanceRequest, _ ...gax.CallOption) (*runpb.Instance, error) {
	for _, inst := range c.instances {
		if inst.GetName() == req.GetName() {
			return inst, nil
		}
	}
	return nil, status.Error(codes.NotFound, "instance does not exist")
}

func (c *crStopInstances) CreateInstance(context.Context, *runpb.CreateInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected CreateInstance")
}

func (c *crStopInstances) StartInstance(context.Context, *runpb.StartInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected StartInstance")
}

func (c *crStopInstances) StopInstance(_ context.Context, req *runpb.StopInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.stopped = append(c.stopped, req.GetName())
	return crStopOp{}, nil
}

func (c *crStopInstances) DeleteInstance(context.Context, *runpb.DeleteInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected DeleteInstance")
}

func (c *crStopInstances) ListInstances(context.Context, *runpb.ListInstancesRequest, ...gax.CallOption) cloudrun.InstanceIterator {
	return &crStopIter{instances: append([]*runpb.Instance(nil), c.instances...)}
}

func (c *crStopInstances) Close() error { return nil }

// TestAgentManagerStop_CloudRun_AgentName: stopping a Cloud Run agent by its
// agent name stops the agent's instance, which is named by a hashed
// instance ID and carries the agent name only in its GCP-sanitized
// scion_name label, instead of passing the bare name on as an instance ID
// (ptone/scion#5268).
func TestAgentManagerStop_CloudRun_AgentName(t *testing.T) {
	const instance = "projects/test-project/locations/us-central1/instances/agent-dev-0123456789"
	fake := &crStopInstances{instances: []*runpb.Instance{{
		Name: instance,
		Labels: map[string]string{
			"agent_id":     "hub-agent-id",
			"scion_agent":  "true",
			"scion_name":   "dev",
			"scion_run_id": "run-1",
		},
	}}}
	rt, err := runtime.NewCloudRunRuntimeForTest(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	mgr := &AgentManager{Runtime: rt}

	if err := mgr.Stop(context.Background(), "dev", "", "", ""); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if len(fake.stopped) != 1 || fake.stopped[0] != instance {
		t.Errorf("StopInstance calls = %q, want exactly one, for %s", fake.stopped, instance)
	}
}
