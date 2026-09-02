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

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeInstancesClient and friends are a purpose-built double for
// cloudrun.InstancesAPI. They record every request the runtime sends and
// replay canned responses, so the Cloud Run lifecycle can be asserted without
// GCP credentials or network access.

type fakeInstanceOperation struct {
	instance *runpb.Instance
	waitErr  error
}

func (o *fakeInstanceOperation) Wait(ctx context.Context, opts ...gax.CallOption) (*runpb.Instance, error) {
	if o.waitErr != nil {
		return nil, o.waitErr
	}
	return o.instance, nil
}

type fakeInstanceIterator struct {
	instances []*runpb.Instance
	err       error
	idx       int
}

func (it *fakeInstanceIterator) Next() (*runpb.Instance, error) {
	if it.err != nil {
		return nil, it.err
	}
	if it.idx >= len(it.instances) {
		return nil, iterator.Done
	}
	inst := it.instances[it.idx]
	it.idx++
	return inst, nil
}

type fakeInstancesClient struct {
	// Canned responses.
	getInstance *runpb.Instance
	getErr      error
	createErr   error
	startErr    error
	stopErr     error
	deleteErr   error
	waitErr     error // applied to whichever operation is returned
	listResult  []*runpb.Instance
	listErr     error

	// Recorded requests.
	getReqs    []*runpb.GetInstanceRequest
	createReqs []*runpb.CreateInstanceRequest
	startReqs  []*runpb.StartInstanceRequest
	stopReqs   []*runpb.StopInstanceRequest
	deleteReqs []*runpb.DeleteInstanceRequest
	listReqs   []*runpb.ListInstancesRequest
	closeCount int
}

func (c *fakeInstancesClient) GetInstance(ctx context.Context, req *runpb.GetInstanceRequest, opts ...gax.CallOption) (*runpb.Instance, error) {
	c.getReqs = append(c.getReqs, req)
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.getInstance, nil
}

func (c *fakeInstancesClient) CreateInstance(ctx context.Context, req *runpb.CreateInstanceRequest, opts ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.createReqs = append(c.createReqs, req)
	if c.createErr != nil {
		return nil, c.createErr
	}
	return &fakeInstanceOperation{waitErr: c.waitErr}, nil
}

func (c *fakeInstancesClient) StartInstance(ctx context.Context, req *runpb.StartInstanceRequest, opts ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.startReqs = append(c.startReqs, req)
	if c.startErr != nil {
		return nil, c.startErr
	}
	return &fakeInstanceOperation{waitErr: c.waitErr}, nil
}

func (c *fakeInstancesClient) StopInstance(ctx context.Context, req *runpb.StopInstanceRequest, opts ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.stopReqs = append(c.stopReqs, req)
	if c.stopErr != nil {
		return nil, c.stopErr
	}
	return &fakeInstanceOperation{waitErr: c.waitErr}, nil
}

func (c *fakeInstancesClient) DeleteInstance(ctx context.Context, req *runpb.DeleteInstanceRequest, opts ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.deleteReqs = append(c.deleteReqs, req)
	if c.deleteErr != nil {
		return nil, c.deleteErr
	}
	return &fakeInstanceOperation{waitErr: c.waitErr}, nil
}

func (c *fakeInstancesClient) ListInstances(ctx context.Context, req *runpb.ListInstancesRequest, opts ...gax.CallOption) cloudrun.InstanceIterator {
	c.listReqs = append(c.listReqs, req)
	return &fakeInstanceIterator{instances: c.listResult, err: c.listErr}
}

func (c *fakeInstancesClient) Close() error {
	c.closeCount++
	return nil
}

// newFakeCloudRunRuntime builds a CloudRunRuntime wired to the supplied fake.
func newFakeCloudRunRuntime(t *testing.T, fake *fakeInstancesClient) *CloudRunRuntime {
	t.Helper()
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "test-project",
		Location:  "us-central1",
	})
	if err != nil {
		t.Fatalf("NewCloudRunRuntime: %v", err)
	}
	rt.newClient = func(ctx context.Context) (cloudrun.InstancesAPI, error) {
		return fake, nil
	}
	return rt
}

// runConfigForTest returns a RunConfig that does not exercise NFS provisioning,
// so Run reaches the Instances API without touching the filesystem.
func runConfigForTest() RunConfig {
	return RunConfig{
		Name:   "test-agent",
		Image:  "docker.io/scion/agent:latest",
		Labels: map[string]string{"agent_id": "agent-1"},
	}
}

func notFoundErr() error {
	return status.Error(codes.NotFound, "instance does not exist")
}

func TestCloudRunRun_CreatesInstanceWhenAbsent(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	cfg := runConfigForTest()

	id, err := rt.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantID := cloudRunInstanceID("agent-1")
	if id != wantID {
		t.Errorf("Run returned id %q, want %q", id, wantID)
	}

	// Existence is probed before creating.
	if len(fake.getReqs) != 1 {
		t.Fatalf("GetInstance called %d times, want 1", len(fake.getReqs))
	}
	wantName := "projects/test-project/locations/us-central1/instances/" + wantID
	if fake.getReqs[0].Name != wantName {
		t.Errorf("GetInstance name = %q, want %q", fake.getReqs[0].Name, wantName)
	}

	// A missing instance is created, not started.
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
	}
	if len(fake.startReqs) != 0 {
		t.Errorf("StartInstance called %d times, want 0", len(fake.startReqs))
	}

	req := fake.createReqs[0]
	if got, want := req.Parent, "projects/test-project/locations/us-central1"; got != want {
		t.Errorf("CreateInstance parent = %q, want %q", got, want)
	}
	if req.InstanceId != wantID {
		t.Errorf("CreateInstance InstanceId = %q, want %q", req.InstanceId, wantID)
	}
	if req.Instance == nil {
		t.Fatal("CreateInstance Instance is nil")
	}
	if len(req.Instance.Containers) != 1 {
		t.Fatalf("Instance has %d containers, want 1", len(req.Instance.Containers))
	}
	if got := req.Instance.Containers[0].Image; got != cfg.Image {
		t.Errorf("container image = %q, want %q", got, cfg.Image)
	}
	if got := req.Instance.Labels["agent_id"]; got != "agent-1" {
		t.Errorf("instance label agent_id = %q, want %q", got, "agent-1")
	}

	if fake.closeCount != 1 {
		t.Errorf("client Close called %d times, want 1", fake.closeCount)
	}
}

func TestCloudRunRun_StartsExistingInstance(t *testing.T) {
	wantID := cloudRunInstanceID("agent-1")
	wantName := "projects/test-project/locations/us-central1/instances/" + wantID
	fake := &fakeInstancesClient{getInstance: &runpb.Instance{Name: wantName}}
	rt := newFakeCloudRunRuntime(t, fake)

	id, err := rt.Run(context.Background(), runConfigForTest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != wantID {
		t.Errorf("Run returned id %q, want %q", id, wantID)
	}

	// An existing instance is started, never re-created.
	if len(fake.startReqs) != 1 {
		t.Fatalf("StartInstance called %d times, want 1", len(fake.startReqs))
	}
	if fake.startReqs[0].Name != wantName {
		t.Errorf("StartInstance name = %q, want %q", fake.startReqs[0].Name, wantName)
	}
	if len(fake.createReqs) != 0 {
		t.Errorf("CreateInstance called %d times, want 0", len(fake.createReqs))
	}
}

func TestCloudRunRun_ErrorPaths(t *testing.T) {
	tests := []struct {
		name    string
		fake    *fakeInstancesClient
		wantErr string
		// assertions on what must NOT have happened
		wantNoCreate bool
		wantNoStart  bool
	}{
		{
			name:         "get returns non-NotFound",
			fake:         &fakeInstancesClient{getErr: status.Error(codes.PermissionDenied, "denied")},
			wantErr:      "failed to get instance",
			wantNoCreate: true,
			wantNoStart:  true,
		},
		{
			name:        "create call fails",
			fake:        &fakeInstancesClient{getErr: notFoundErr(), createErr: errors.New("boom")},
			wantErr:     "failed to create instance",
			wantNoStart: true,
		},
		{
			name:        "create operation wait fails",
			fake:        &fakeInstancesClient{getErr: notFoundErr(), waitErr: errors.New("lro failed")},
			wantErr:     "wait for create operation failed",
			wantNoStart: true,
		},
		{
			name:         "start call fails",
			fake:         &fakeInstancesClient{getInstance: &runpb.Instance{}, startErr: errors.New("boom")},
			wantErr:      "failed to start existing instance",
			wantNoCreate: true,
		},
		{
			name:         "start operation wait fails",
			fake:         &fakeInstancesClient{getInstance: &runpb.Instance{}, waitErr: errors.New("lro failed")},
			wantErr:      "wait for start operation failed",
			wantNoCreate: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeCloudRunRuntime(t, tc.fake)
			id, err := rt.Run(context.Background(), runConfigForTest())
			if err == nil {
				t.Fatalf("Run returned id %q, want error", id)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Run error = %q, want it to contain %q", err, tc.wantErr)
			}
			if tc.wantNoCreate && len(tc.fake.createReqs) != 0 {
				t.Errorf("CreateInstance called %d times, want 0", len(tc.fake.createReqs))
			}
			if tc.wantNoStart && len(tc.fake.startReqs) != 0 {
				t.Errorf("StartInstance called %d times, want 0", len(tc.fake.startReqs))
			}
			// The client is closed even when the lifecycle call fails.
			if tc.fake.closeCount != 1 {
				t.Errorf("client Close called %d times, want 1", tc.fake.closeCount)
			}
		})
	}
}

// TestCloudRunTeardown_SendsQualifiedName covers Stop and Delete: both must
// address exactly one well-formed instance resource name.
func TestCloudRunTeardown_SendsQualifiedName(t *testing.T) {
	const id = "agent-foo-0123456789"
	wantName := "projects/test-project/locations/us-central1/instances/" + id

	tests := []struct {
		name    string
		call    func(*CloudRunRuntime) error
		reqName func(*fakeInstancesClient) (string, int)
	}{
		{
			name: "Stop",
			call: func(rt *CloudRunRuntime) error { return rt.Stop(context.Background(), id) },
			reqName: func(f *fakeInstancesClient) (string, int) {
				if len(f.stopReqs) == 0 {
					return "", 0
				}
				return f.stopReqs[0].Name, len(f.stopReqs)
			},
		},
		{
			name: "Delete",
			call: func(rt *CloudRunRuntime) error { return rt.Delete(context.Background(), id) },
			reqName: func(f *fakeInstancesClient) (string, int) {
				if len(f.deleteReqs) == 0 {
					return "", 0
				}
				return f.deleteReqs[0].Name, len(f.deleteReqs)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{}
			rt := newFakeCloudRunRuntime(t, fake)

			if err := tc.call(rt); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			got, calls := tc.reqName(fake)
			if calls != 1 {
				t.Fatalf("%sInstance called %d times, want 1", tc.name, calls)
			}
			if got != wantName {
				t.Errorf("%sInstance name = %q, want %q", tc.name, got, wantName)
			}
			if fake.closeCount != 1 {
				t.Errorf("client Close called %d times, want 1", fake.closeCount)
			}
		})
	}
}

// TestCloudRunTeardown_ErrorPaths covers Stop and Delete failing at both the
// call layer and the long-running-operation wait layer.
func TestCloudRunTeardown_ErrorPaths(t *testing.T) {
	stop := func(rt *CloudRunRuntime) error { return rt.Stop(context.Background(), "agent-x") }
	del := func(rt *CloudRunRuntime) error { return rt.Delete(context.Background(), "agent-x") }

	tests := []struct {
		name    string
		fake    *fakeInstancesClient
		call    func(*CloudRunRuntime) error
		wantErr string
	}{
		{"stop call fails", &fakeInstancesClient{stopErr: errors.New("boom")}, stop, "failed to stop instance"},
		{"stop wait fails", &fakeInstancesClient{waitErr: errors.New("lro failed")}, stop, "wait for stop operation failed"},
		{"delete call fails", &fakeInstancesClient{deleteErr: errors.New("boom")}, del, "failed to delete instance"},
		{"delete wait fails", &fakeInstancesClient{waitErr: errors.New("lro failed")}, del, "wait for delete operation failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeCloudRunRuntime(t, tc.fake)
			err := tc.call(rt)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			// The client is closed even when teardown fails.
			if tc.fake.closeCount != 1 {
				t.Errorf("client Close called %d times, want 1", tc.fake.closeCount)
			}
		})
	}
}

func TestCloudRunList_FiltersByLabelsAndMapsAgentInfo(t *testing.T) {
	const wanted = "projects/test-project/locations/us-central1/instances/agent-wanted-0000000000"

	// The "scion.name" key here is the label the runtime emits today
	// (pkg/agent/run.go). It is a fixture, not a contract: if label-key
	// sanitization is introduced for GCP resource labels, these fixtures and
	// the filter key below need updating to the sanitized form.
	newFake := func() *fakeInstancesClient {
		return &fakeInstancesClient{
			listResult: []*runpb.Instance{
				{
					Name:              wanted,
					Labels:            map[string]string{"scion.name": "wanted", "agent_id": "agent-1"},
					TerminalCondition: &runpb.Condition{State: runpb.Condition_CONDITION_SUCCEEDED},
				},
				{
					Name:   "projects/test-project/locations/us-central1/instances/agent-other-1111111111",
					Labels: map[string]string{"scion.name": "other", "agent_id": "agent-2"},
				},
			},
		}
	}

	t.Run("label filter selects one instance", func(t *testing.T) {
		fake := newFake()
		rt := newFakeCloudRunRuntime(t, fake)

		agents, err := rt.List(context.Background(), map[string]string{"scion.name": "wanted"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		if len(fake.listReqs) != 1 {
			t.Fatalf("ListInstances called %d times, want 1", len(fake.listReqs))
		}
		if got, want := fake.listReqs[0].Parent, "projects/test-project/locations/us-central1"; got != want {
			t.Errorf("ListInstances parent = %q, want %q", got, want)
		}

		if len(agents) != 1 {
			t.Fatalf("List returned %d agents, want 1 (label filter not applied?)", len(agents))
		}
		a := agents[0]
		if a.ID != "agent-1" {
			t.Errorf("AgentInfo.ID = %q, want %q", a.ID, "agent-1")
		}
		if a.ContainerID != "agent-wanted-0000000000" {
			t.Errorf("AgentInfo.ContainerID = %q, want the short instance ID", a.ContainerID)
		}
		// Characterization, not a normalization contract: List currently
		// surfaces the raw Cloud Run condition string. A future status/Phase
		// mapping fix is expected to change this assertion.
		if a.ContainerStatus != runpb.Condition_CONDITION_SUCCEEDED.String() {
			t.Errorf("AgentInfo.ContainerStatus = %q, want %q", a.ContainerStatus, runpb.Condition_CONDITION_SUCCEEDED.String())
		}
		if a.Labels["scion.name"] != "wanted" {
			t.Errorf("AgentInfo.Labels = %v, want the instance labels", a.Labels)
		}
		if fake.closeCount != 1 {
			t.Errorf("client Close called %d times, want 1", fake.closeCount)
		}
	})

	t.Run("nil filter returns all instances", func(t *testing.T) {
		fake := newFake()
		rt := newFakeCloudRunRuntime(t, fake)

		agents, err := rt.List(context.Background(), nil)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(agents) != 2 {
			t.Fatalf("List returned %d agents, want 2", len(agents))
		}
		if fake.closeCount != 1 {
			t.Errorf("client Close called %d times, want 1", fake.closeCount)
		}
	})
}

func TestCloudRunList_IteratorErrorPropagates(t *testing.T) {
	fake := &fakeInstancesClient{listErr: errors.New("boom")}
	rt := newFakeCloudRunRuntime(t, fake)

	_, err := rt.List(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "error listing instances") {
		t.Errorf("List error = %v, want 'error listing instances'", err)
	}
	// The client is closed even when the iterator fails.
	if fake.closeCount != 1 {
		t.Errorf("client Close called %d times, want 1", fake.closeCount)
	}
}

// TestCloudRunLifecycle_RunListStopDelete exercises the full lifecycle against
// the fake: the ID Run returns must be the ID List reports, and that ID must
// address the same instance when handed back to Stop and Delete.
func TestCloudRunLifecycle_RunListStopDelete(t *testing.T) {
	ctx := context.Background()

	createFake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, createFake)

	runID, err := rt.Run(ctx, runConfigForTest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	created := createFake.createReqs[0]
	instanceName := created.Parent + "/instances/" + created.InstanceId

	// The API reports the instance under its full resource name.
	listFake := &fakeInstancesClient{
		listResult: []*runpb.Instance{{
			Name:              instanceName,
			Labels:            created.Instance.Labels,
			TerminalCondition: &runpb.Condition{State: runpb.Condition_CONDITION_SUCCEEDED},
		}},
	}
	rt.newClient = func(ctx context.Context) (cloudrun.InstancesAPI, error) { return listFake, nil }

	agents, err := rt.List(ctx, map[string]string{"agent_id": "agent-1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("List returned %d agents, want 1", len(agents))
	}
	containerID := agents[0].ContainerID
	if containerID != runID {
		t.Fatalf("List ContainerID = %q, want the ID Run returned (%q)", containerID, runID)
	}

	// Stop and Delete accept that ContainerID and address the same instance.
	stopFake := &fakeInstancesClient{}
	rt.newClient = func(ctx context.Context) (cloudrun.InstancesAPI, error) { return stopFake, nil }
	if err := rt.Stop(ctx, containerID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := stopFake.stopReqs[0].Name; got != instanceName {
		t.Errorf("StopInstance name = %q, want %q", got, instanceName)
	}

	deleteFake := &fakeInstancesClient{}
	rt.newClient = func(ctx context.Context) (cloudrun.InstancesAPI, error) { return deleteFake, nil }
	if err := rt.Delete(ctx, containerID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := deleteFake.deleteReqs[0].Name; got != instanceName {
		t.Errorf("DeleteInstance name = %q, want %q", got, instanceName)
	}
}

// TestCloudRunRuntime_ClientFactoryError verifies the runtime surfaces a
// client construction failure instead of panicking.
func TestCloudRunRuntime_ClientFactoryError(t *testing.T) {
	rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{})
	rt.newClient = func(ctx context.Context) (cloudrun.InstancesAPI, error) {
		return nil, errors.New("no credentials")
	}

	if _, err := rt.Run(context.Background(), runConfigForTest()); err == nil || !strings.Contains(err.Error(), "failed to create client") {
		t.Errorf("Run error = %v, want 'failed to create client'", err)
	}
	if err := rt.Stop(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "failed to create client") {
		t.Errorf("Stop error = %v, want 'failed to create client'", err)
	}
	if err := rt.Delete(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "failed to create client") {
		t.Errorf("Delete error = %v, want 'failed to create client'", err)
	}
	if _, err := rt.List(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "failed to create client") {
		t.Errorf("List error = %v, want 'failed to create client'", err)
	}
}
