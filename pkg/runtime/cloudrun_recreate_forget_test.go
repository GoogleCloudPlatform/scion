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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Every Run path that removes the agent's instance, or waits for its
// removal, before an instance of the same name is created drops the exec
// key cached for the removed one, as Stop and Delete do (ptone/scion#7289
// with the signed-key cache of #4564). The drop happens also when the
// delete fails. A start that reuses the instance, or finds none, keeps it.
func TestCloudRunRun_ReplacedInstanceForgetsExecKey(t *testing.T) {
	want := "test-project/us-central1/" + cloudRunInstanceID("agent-1")
	unavailable := status.Error(codes.Unavailable, "delete unavailable")
	specDiff := func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient) {
		existing := computedCloudRunInstance(t, rt, readyRunCfg())
		existing.Containers[0].Args = []string{"/bin/sh", "-c", "a launch command of an earlier start"}
		// Run's read, the delete's run-checked read, then gone.
		fake.getSeq = []fakeGetResult{{inst: existing}, {inst: existing}, {err: notFoundErr()}}
		fake.getErr = notFoundErr()
	}
	beingDeleted := func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient) {
		existing := computedCloudRunInstance(t, rt, readyRunCfg())
		existing.DeleteTime = timestamppb.New(time.Unix(1, 0))
		fake.getSeq = []fakeGetResult{{inst: existing}}
		fake.getErr = notFoundErr()
	}
	endedAfterCreate := func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient) {
		fake.getSeq = []fakeGetResult{{err: notFoundErr()}}
		fake.getInstance = readyShape(recordedExitedZero, readyRunID)
		fake.createOpInstance = readyShape(recordedCreating, readyRunID)
	}
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient)
		deleteErr error
		deletes   int
		forgotten int
	}{
		{name: "spec diff recreate", setup: specDiff, deletes: 1, forgotten: 1},
		{name: "spec diff recreate, delete fails", setup: specDiff, deleteErr: unavailable, deletes: 1, forgotten: 1},
		{name: "being deleted, waited for", setup: beingDeleted, deletes: 0, forgotten: 1},
		{name: "ended after create", setup: endedAfterCreate, deletes: 1, forgotten: 1},
		{name: "ended after create, delete fails", setup: endedAfterCreate, deleteErr: unavailable, deletes: 1, forgotten: 1},
		{name: "matching instance started", setup: func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient) {
			fake.getInstance = computedCloudRunInstance(t, rt, readyRunCfg())
		}, deletes: 0, forgotten: 0},
		{name: "no instance, created", setup: func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient) {
			fake.getErr = notFoundErr()
		}, deletes: 0, forgotten: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{}
			rt := newReadyTestRuntime(t, fake)
			tc.setup(t, rt, fake)
			fake.deleteErr = tc.deleteErr
			conn := &forgettingExecConnector{}
			rt.exec = conn

			_, _ = rt.Run(context.Background(), readyRunCfg())

			if len(fake.deleteReqs) != tc.deletes {
				t.Errorf("deletes = %d, want %d", len(fake.deleteReqs), tc.deletes)
			}
			if len(conn.forgotten) != tc.forgotten {
				t.Fatalf("Forget calls = %q, want %d", conn.forgotten, tc.forgotten)
			}
			for _, got := range conn.forgotten {
				if got != want {
					t.Errorf("Forget call = %q, want %q", got, want)
				}
			}
		})
	}
}

// This run's instance computed before resolved auth was delivered on Cloud
// Run (#4566: Cloud Run ignored RunConfig.ResolvedAuth, so the instance is
// the one computed without it) differs from the computed spec, so the next
// start recreates it, naming env_names. The instance it creates matches the
// spec, so the start after that reuses it: the recreate happens once.
func TestCloudRunRun_InstanceWithoutResolvedAuthRecreatedOnce(t *testing.T) {
	withAuth := func() RunConfig {
		cfg := readyRunCfg()
		cfg.ResolvedAuth = &api.ResolvedAuth{EnvVars: map[string]string{
			"CLOUD_ML_REGION":           "us-east5",
			"EXAMPLE_AUTH_CONFIG_VALUE": "example-config-value",
		}}
		return cfg
	}

	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	before := computedCloudRunInstance(t, rt, readyRunCfg())
	// Run's read, the delete's run-checked read, then gone.
	fake.getSeq = []fakeGetResult{{inst: before}, {inst: before}, {err: notFoundErr()}}
	fake.getErr = notFoundErr()
	logs := captureRuntimeLog(t)

	if _, err := rt.Run(context.Background(), withAuth()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(fake.deleteReqs) != 1 || len(fake.createReqs) != 1 || len(fake.startReqs) != 0 {
		t.Fatalf("first Run delete/create/start = %d/%d/%d, want 1/1/0", len(fake.deleteReqs), len(fake.createReqs), len(fake.startReqs))
	}
	if out := logs.String(); !strings.Contains(out, "changed_fields=") || !strings.Contains(out, "env_names") {
		t.Errorf("log does not name env_names among the changed fields (log length %d)", len(out))
	}

	next := &fakeInstancesClient{getInstance: computedCloudRunInstance(t, rt, withAuth())}
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) { return next, nil }
	if _, err := rt.Run(context.Background(), withAuth()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(next.startReqs) != 1 || len(next.createReqs) != 0 || len(next.deleteReqs) != 0 {
		t.Errorf("second Run start/create/delete = %d/%d/%d, want 1/0/0", len(next.startReqs), len(next.createReqs), len(next.deleteReqs))
	}
}
