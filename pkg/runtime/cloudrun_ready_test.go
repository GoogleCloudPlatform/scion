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
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeClockReadyBounds are the default wait bounds on a fake clock: a
// sleep only advances the clock, so a wait runs to its cap at once.
func fakeClockReadyBounds() *cloudRunReadyBounds {
	b := defaultCloudRunReadyBounds()
	clock := time.Unix(0, 0)
	b.clock = func() time.Time { return clock }
	b.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock = clock.Add(d)
		return nil
	}
	return &b
}

// newReadyTestRuntime is newFakeCloudRunRuntime with the fake-clock wait
// bounds.
func newReadyTestRuntime(t *testing.T, fake *fakeInstancesClient) *CloudRunRuntime {
	t.Helper()
	rt := newFakeCloudRunRuntime(t, fake)
	rt.readyBoundsForTest = fakeClockReadyBounds()
	return rt
}

// computedCloudRunInstance returns the instance rt's Run computes for cfg,
// named with its full resource name: Run creates it on a scratch client,
// which keeps nothing. Run's own side effects for cfg (provisioning and
// the home copy) happen once more.
func computedCloudRunInstance(t *testing.T, rt *CloudRunRuntime, cfg RunConfig) *runpb.Instance {
	t.Helper()
	scratch := &fakeInstancesClient{getErr: notFoundErr()}
	saved := rt.newClient
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) { return scratch, nil }
	defer func() { rt.newClient = saved }()
	cfg.Checkpoint, cfg.OnResourceCreated, cfg.Progress, cfg.ObserveResourceCreated = nil, nil, nil, nil
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("computing the instance: Run: %v", err)
	}
	if len(scratch.createReqs) != 1 {
		t.Fatalf("computing the instance: %d creates, want 1", len(scratch.createReqs))
	}
	req := scratch.createReqs[0]
	inst := proto.Clone(req.GetInstance()).(*runpb.Instance)
	inst.Name = req.GetParent() + "/instances/" + req.GetInstanceId()
	inst.Etag = "etag-1"
	return inst
}

const readyRunID = "run-b"

var readyInstanceName = "projects/test-project/locations/us-central1/instances/" + cloudRunInstanceID("agent-1")

func readyRunCfg() RunConfig { return runCfgForRun(readyRunID) }

// readyShape is a recorded shape (cloudrun_list_phase_test.go) labelled
// with runID.
func readyShape(shape func() *runpb.Instance, runID string) *runpb.Instance {
	inst := shape()
	inst.Name = readyInstanceName
	inst.Labels = map[string]string{"agent_id": "agent-1"}
	if runID != "" {
		inst.Labels[sanitizeGCPLabelKey(api.LabelRunID)] = sanitizeGCPLabelValue(runID)
	}
	return inst
}

// recordedFailedRetrying is the recorded stopped shape with a Retry
// condition: the container failed and the platform retries it.
func recordedFailedRetrying() *runpb.Instance {
	inst := recordedStopped()
	inst.Generation, inst.ObservedGeneration = 1, 1
	inst.Conditions = append(inst.Conditions, recordedRetry(runpb.Condition_IMMEDIATE_RETRY, "System will retry after 00:00 from lastTransitionTime for attempt 0."))
	return inst
}

func TestClassifyCloudRunInstance_RecordedShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func() *runpb.Instance
		want  cloudRunReadyVerdict
	}{
		{"creating", recordedCreating, cloudRunWait},
		{"running", recordedRunning, cloudRunReady},
		{"stopping", recordedStopping, cloudRunWait},
		{"stopped", recordedStopped, cloudRunEnded},
		{"restarting", recordedRestarting, cloudRunWait},
		{"restarted", recordedRestarted, cloudRunReady},
		{"exited zero", recordedExitedZero, cloudRunEnded},
		{"deleting", recordedDeleting, cloudRunDeleted},
		{"failed with retry", recordedFailedRetrying, cloudRunWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCloudRunInstance(readyShape(tc.shape, readyRunID), cloudRunReadyTarget{runID: readyRunID})
			if got != tc.want {
				t.Errorf("verdict = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClassifyCloudRunInstance_RunAndGeneration(t *testing.T) {
	other := readyShape(recordedRunning, "run-a")
	if got := classifyCloudRunInstance(other, cloudRunReadyTarget{runID: readyRunID}); got != cloudRunOtherRun {
		t.Errorf("another run's instance: verdict %d, want cloudRunOtherRun", got)
	}
	legacy := readyShape(recordedRunning, "")
	if got := classifyCloudRunInstance(legacy, cloudRunReadyTarget{runID: readyRunID}); got != cloudRunOtherRun {
		t.Errorf("legacy instance after a create: verdict %d, want cloudRunOtherRun", got)
	}
	if got := classifyCloudRunInstance(legacy, cloudRunReadyTarget{runID: readyRunID, legacyOK: true}); got != cloudRunReady {
		t.Errorf("legacy instance on the start path: verdict %d, want cloudRunReady", got)
	}
	// The stopped instance read before the start, at its generation, is
	// not the start's result.
	stopped := readyShape(recordedStopped, readyRunID)
	if got := classifyCloudRunInstance(stopped, cloudRunReadyTarget{runID: readyRunID, minGeneration: stopped.GetGeneration()}); got != cloudRunWait {
		t.Errorf("pre-start sample: verdict %d, want cloudRunWait", got)
	}
}

// T1 (stop then start, ptone/scion#7289): Start's pre-clean deleted the
// previous run's instance, which is still soft-deleted when Run reads it.
// Run waits until it is gone, creates this run's instance once and waits
// until that is ready. It is neither refused as another run's instance
// nor deleted again.
func TestCloudRunRun_StopThenStartWaitsForSoftDeletedInstance(t *testing.T) {
	fake := &fakeInstancesClient{
		getSeq: []fakeGetResult{
			{inst: readyShape(recordedDeleting, "run-a")}, // Run's read
			{inst: readyShape(recordedDeleting, "run-a")}, // first wait-gone read
			{err: notFoundErr()},                          // gone
			{inst: readyShape(recordedRunning, readyRunID)},
		},
		getErr:           notFoundErr(),
		createOpInstance: readyShape(recordedCreating, readyRunID),
	}
	rt := newReadyTestRuntime(t, fake)

	if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.createReqs) != 1 || len(fake.deleteReqs) != 0 || len(fake.startReqs) != 0 {
		t.Errorf("create/delete/start = %d/%d/%d, want 1/0/0", len(fake.createReqs), len(fake.deleteReqs), len(fake.startReqs))
	}
	if len(fake.getSeq) != 0 {
		t.Errorf("%d reads left unused, want the wait to read until ready", len(fake.getSeq))
	}
}

// T1, same run: this run's stopped instance is started; the start
// operation returns the instance read before the start (its generation),
// which is not taken as the result, and the wait reads until the
// restarted shape.
func TestCloudRunRun_StartExistingWaitsPastPreStartSample(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	existing := computedCloudRunInstance(t, rt, readyRunCfg())
	stopped := recordedStopped()
	existing.Generation, existing.ObservedGeneration = stopped.Generation, stopped.ObservedGeneration
	existing.TerminalCondition, existing.Conditions = stopped.TerminalCondition, stopped.Conditions
	restarted := proto.Clone(existing).(*runpb.Instance)
	r := recordedRestarted()
	restarted.Generation, restarted.ObservedGeneration = r.Generation, r.ObservedGeneration
	restarted.TerminalCondition, restarted.Conditions = r.TerminalCondition, r.Conditions
	fake.getSeq = []fakeGetResult{{inst: existing}, {inst: restarted}}
	fake.startOpInstance = existing

	if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.startReqs) != 1 || len(fake.createReqs) != 0 || len(fake.deleteReqs) != 0 {
		t.Errorf("start/create/delete = %d/%d/%d, want 1/0/0", len(fake.startReqs), len(fake.createReqs), len(fake.deleteReqs))
	}
	if len(fake.getReqs) != 2 {
		t.Errorf("GetInstance called %d times, want 2 (the read and one wait read)", len(fake.getReqs))
	}
}

// T2 (first start of a fresh agent): the create's operation returns the
// instance still creating, the first read shows the container failed with
// the platform retrying it, and the next read shows it running. Run
// returns only once it is ready, with one create and no delete.
func TestCloudRunRun_FirstStartWaitsThroughPlatformRetry(t *testing.T) {
	fake := &fakeInstancesClient{
		getSeq: []fakeGetResult{
			{err: notFoundErr()},
			{inst: readyShape(recordedFailedRetrying, readyRunID)},
			{inst: readyShape(recordedRunning, readyRunID)},
		},
		createOpInstance: readyShape(recordedCreating, readyRunID),
	}
	rt := newReadyTestRuntime(t, fake)
	var steps []string
	cfg := readyRunCfg()
	cfg.Progress = func(step, _ string) { steps = append(steps, step) }

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.getReqs) != 3 {
		t.Errorf("GetInstance called %d times, want 3 (the read and two wait reads)", len(fake.getReqs))
	}
	if len(fake.createReqs) != 1 || len(fake.deleteReqs) != 0 {
		t.Errorf("create/delete = %d/%d, want 1/0", len(fake.createReqs), len(fake.deleteReqs))
	}
	if len(steps) == 0 || steps[len(steps)-1] != api.LaunchStepContainerStart {
		t.Errorf("progress steps = %v, want the wait reported last", steps)
	}
}

// T2: a create read NotFound right after the create (not visible yet) is
// waited for.
func TestCloudRunRun_CreateNotYetVisibleIsWaitedFor(t *testing.T) {
	fake := &fakeInstancesClient{
		getSeq: []fakeGetResult{
			{err: notFoundErr()},
			{err: notFoundErr()},
			{inst: readyShape(recordedRunning, readyRunID)},
		},
		createOpInstance: readyShape(recordedCreating, readyRunID),
	}
	rt := newReadyTestRuntime(t, fake)
	if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.createReqs) != 1 {
		t.Errorf("creates = %d, want 1", len(fake.createReqs))
	}
}

// T3: a permission error is surfaced at once and never retried, from the
// create (the missing actAs permission, ptone/scion#7319) and from a wait
// read.
func TestCloudRunRun_PermissionDeniedNotRetried(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		fake := &fakeInstancesClient{getErr: notFoundErr(), createErr: status.Error(codes.PermissionDenied, "Permission 'iam.serviceaccounts.actAs' denied")}
		rt := newReadyTestRuntime(t, fake)
		_, err := rt.Run(context.Background(), readyRunCfg())
		if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "not retried") {
			t.Fatalf("Run = %v, want PermissionDenied, not retried", err)
		}
		if len(fake.createReqs) != 1 || len(fake.getReqs) != 1 || len(fake.deleteReqs) != 0 {
			t.Errorf("create/get/delete = %d/%d/%d, want 1/1/0", len(fake.createReqs), len(fake.getReqs), len(fake.deleteReqs))
		}
	})
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument} {
		t.Run("wait read "+code.String(), func(t *testing.T) {
			fake := &fakeInstancesClient{
				getSeq:           []fakeGetResult{{err: notFoundErr()}},
				getErr:           status.Error(code, "refused"),
				createOpInstance: readyShape(recordedCreating, readyRunID),
			}
			rt := newReadyTestRuntime(t, fake)
			_, err := rt.Run(context.Background(), readyRunCfg())
			if status.Code(err) != code || !strings.Contains(err.Error(), "not retried") {
				t.Fatalf("Run = %v, want %s, not retried", err, code)
			}
			if len(fake.getReqs) != 2 {
				t.Errorf("GetInstance called %d times, want 2 (the read and the refused wait read)", len(fake.getReqs))
			}
			if len(fake.createReqs) != 1 || len(fake.deleteReqs) != 0 {
				t.Errorf("create/delete = %d/%d, want 1/0", len(fake.createReqs), len(fake.deleteReqs))
			}
		})
	}
}

// T4: an instance that never becomes ready ends the wait at the cap, and
// the error names the last state read, not a bare timeout. This run's
// instance is then deleted, run-checked.
func TestCloudRunRun_WaitCapNamesLastState(t *testing.T) {
	pending := readyShape(recordedCreating, readyRunID)
	fake := &fakeInstancesClient{
		getSeq:           []fakeGetResult{{err: notFoundErr()}},
		getInstance:      pending,
		createOpInstance: pending,
	}
	rt := newReadyTestRuntime(t, fake)
	_, err := rt.Run(context.Background(), readyRunCfg())
	if !errors.Is(err, errCloudRunReadyCap) {
		t.Fatalf("Run = %v, want the wait cap", err)
	}
	for _, want := range []string{"condition=CONDITION_PENDING", "reconciling=true", readyInstanceName[strings.LastIndex(readyInstanceName, "/")+1:]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	// The read, at most maxPolls wait reads, and the delete's run check.
	if n := len(fake.getReqs); n < 3 || n > 1+cloudRunReadyMaxPolls+1 {
		t.Errorf("GetInstance called %d times, want within the poll bound", n)
	}
	if len(fake.createReqs) != 1 || len(fake.deleteReqs) != 1 {
		t.Errorf("create/delete = %d/%d, want 1/1", len(fake.createReqs), len(fake.deleteReqs))
	}
}

// T4: when only retried API errors were read, the cap names the last
// error code.
func TestCloudRunRun_WaitCapNamesLastError(t *testing.T) {
	fake := &fakeInstancesClient{
		getSeq:           []fakeGetResult{{err: notFoundErr()}},
		getErr:           status.Error(codes.Unavailable, "try again"),
		createOpInstance: readyShape(recordedCreating, readyRunID),
	}
	rt := newReadyTestRuntime(t, fake)
	_, err := rt.Run(context.Background(), readyRunCfg())
	if !errors.Is(err, errCloudRunReadyCap) || !strings.Contains(err.Error(), "last error: Unavailable") {
		t.Fatalf("Run = %v, want the cap naming the last error", err)
	}
	if len(fake.createReqs) != 1 {
		t.Errorf("creates = %d, want 1", len(fake.createReqs))
	}
}

// T5: CreateInstance is issued at most once per Run: an instance that
// ends before it is ready fails the start and is deleted run-checked, and
// is never created again.
func TestCloudRunRun_EndedInstanceNotRecreated(t *testing.T) {
	ended := readyShape(recordedExitedZero, readyRunID)
	fake := &fakeInstancesClient{
		getSeq:           []fakeGetResult{{err: notFoundErr()}},
		getInstance:      ended,
		createOpInstance: readyShape(recordedCreating, readyRunID),
	}
	rt := newReadyTestRuntime(t, fake)
	_, err := rt.Run(context.Background(), readyRunCfg())
	if !errors.Is(err, errCloudRunInstanceEnded) || !strings.Contains(err.Error(), "condition=CONDITION_FAILED") {
		t.Fatalf("Run = %v, want the instance-ended error with its state", err)
	}
	if len(fake.createReqs) != 1 || len(fake.startReqs) != 0 {
		t.Errorf("create/start = %d/%d, want 1/0", len(fake.createReqs), len(fake.startReqs))
	}
	if len(fake.deleteReqs) != 1 {
		t.Errorf("deletes = %d, want 1 (this run's instance)", len(fake.deleteReqs))
	}
}

// T5: a wait that reads another run's instance under the ID (a racing
// start) fails with ErrRunConflict and deletes nothing.
func TestCloudRunRun_WaitReadsOtherRunDeletesNothing(t *testing.T) {
	fake := &fakeInstancesClient{
		getSeq:           []fakeGetResult{{err: notFoundErr()}},
		getInstance:      readyShape(recordedRunning, "run-a"),
		createOpInstance: readyShape(recordedCreating, readyRunID),
	}
	rt := newReadyTestRuntime(t, fake)
	_, err := rt.Run(context.Background(), readyRunCfg())
	if !errors.Is(err, ErrRunConflict) {
		t.Fatalf("Run = %v, want ErrRunConflict for another run's instance", err)
	}
	if len(fake.deleteReqs) != 0 || len(fake.createReqs) != 1 {
		t.Errorf("delete/create = %d/%d, want 0/1", len(fake.deleteReqs), len(fake.createReqs))
	}
}

// T6 (S3): a create that finds the instance present adopts it only when
// it is this run's and matches the computed spec; any other is refused
// and left untouched. The create is never re-issued.
func TestCloudRunRun_AlreadyExistsAdoptionRules(t *testing.T) {
	alreadyExists := status.Error(codes.AlreadyExists, "instance exists")
	for _, tc := range []struct {
		name      string
		mutate    func(inst *runpb.Instance)
		wantErr   error
		wantInErr string
	}{
		{name: "own run matching adopted"},
		{name: "another run refused", mutate: func(inst *runpb.Instance) {
			inst.Labels[sanitizeGCPLabelKey(api.LabelRunID)] = "run-a"
		}, wantErr: ErrRunConflict},
		{name: "legacy refused", mutate: func(inst *runpb.Instance) {
			delete(inst.Labels, sanitizeGCPLabelKey(api.LabelRunID))
		}, wantErr: ErrRunConflict},
		{name: "own run different args refused", mutate: func(inst *runpb.Instance) {
			inst.Containers[0].Args = []string{"/bin/sh", "-c", "a different launch command"}
		}, wantInErr: "different args"},
		{name: "own run being deleted refused", mutate: func(inst *runpb.Instance) {
			inst.DeleteTime = timestamppb.New(time.Unix(1, 0))
		}, wantInErr: "being deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{}
			rt := newReadyTestRuntime(t, fake)
			found := computedCloudRunInstance(t, rt, readyRunCfg())
			found.TerminalCondition = &runpb.Condition{Type: "Running", State: runpb.Condition_CONDITION_SUCCEEDED}
			if tc.mutate != nil {
				tc.mutate(found)
			}
			fake.getSeq = []fakeGetResult{{err: notFoundErr()}, {inst: found}}
			fake.createErr = alreadyExists

			_, err := rt.Run(context.Background(), readyRunCfg())
			adopt := tc.wantErr == nil && tc.wantInErr == ""
			switch {
			case adopt && err != nil:
				t.Fatalf("Run = %v, want the instance adopted", err)
			case !adopt && err == nil:
				t.Fatal("Run adopted the instance, want a refusal")
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Errorf("Run = %v, want %v", err, tc.wantErr)
			case tc.wantInErr != "" && !strings.Contains(err.Error(), tc.wantInErr):
				t.Errorf("Run = %v, want it to name %q", err, tc.wantInErr)
			}
			if !adopt && status.Code(err) != codes.AlreadyExists {
				t.Errorf("refusal code = %s, want AlreadyExists", status.Code(err))
			}
			if len(fake.createReqs) != 1 || len(fake.startReqs) != 0 || len(fake.deleteReqs) != 0 {
				t.Errorf("create/start/delete = %d/%d/%d, want 1/0/0", len(fake.createReqs), len(fake.startReqs), len(fake.deleteReqs))
			}
		})
	}
}

// S4 (ptone/scion#7297): this run's existing instance that differs from
// the computed spec in any compared field is deleted and recreated with
// the computed spec, never started. Only field names reach the log.
func TestCloudRunRun_ExistingInstanceSpecDiffRecreated(t *testing.T) {
	const canary = "spec-diff-canary-value"
	secretCfg := func(value string) RunConfig {
		cfg := readyRunCfg()
		cfg.ResolvedSecrets = []api.ResolvedSecret{
			{Name: "token", Type: "environment", Target: "EXAMPLE_TOKEN", Value: value},
		}
		return cfg
	}
	stagedCfg := func(value string) RunConfig {
		cfg := readyRunCfg()
		cfg.ResolvedSecrets = []api.ResolvedSecret{
			{Name: "region", Type: "variable", Target: "EXAMPLE_REGION", Value: value},
		}
		return cfg
	}
	for _, tc := range []struct {
		field string
		// existingCfg computes the existing instance; runCfg is the start.
		existingCfg, runCfg RunConfig
		mutate              func(inst *runpb.Instance)
		check               func(t *testing.T, created *runpb.Instance)
	}{
		{field: "args", existingCfg: readyRunCfg(), runCfg: readyRunCfg(),
			mutate: func(inst *runpb.Instance) {
				inst.Containers[0].Args = []string{"/bin/sh", "-c", "a launch command of an earlier start"}
			}},
		{field: "image", existingCfg: readyRunCfg(), runCfg: readyRunCfg(),
			mutate: func(inst *runpb.Instance) { inst.Containers[0].Image = "docker.io/scion/agent:earlier" }},
		{field: "service_account", existingCfg: readyRunCfg(), runCfg: readyRunCfg(),
			mutate: func(inst *runpb.Instance) { inst.ServiceAccount = "other-sa@test-project.iam.gserviceaccount.com" },
			check: func(t *testing.T, created *runpb.Instance) {
				if created.GetServiceAccount() != "agent-sa@test-project.iam.gserviceaccount.com" {
					t.Errorf("created service account = %q, want the configured one", created.GetServiceAccount())
				}
			}},
		{field: "secret_env", existingCfg: secretCfg("earlier"), runCfg: secretCfg(canary)},
		{field: "secret_env", existingCfg: stagedCfg("earlier"), runCfg: stagedCfg(canary)},
		{field: "volumes", existingCfg: readyRunCfg(), runCfg: readyRunCfg(),
			mutate: func(inst *runpb.Instance) {
				inst.Volumes[0].GetNfs().Path = "/ws/projects/other/workspace"
			}},
	} {
		t.Run(tc.field+"/"+tc.existingCfg.Labels[api.LabelRunID], func(t *testing.T) {
			fake := &fakeInstancesClient{}
			rt := newReadyTestRuntime(t, fake)
			rt.config.ServiceAccount = "agent-sa@test-project.iam.gserviceaccount.com"
			existing := computedCloudRunInstance(t, rt, tc.existingCfg)
			if tc.mutate != nil {
				tc.mutate(existing)
			}
			// Run's read, the delete's run-checked read, then gone.
			fake.getSeq = []fakeGetResult{{inst: existing}, {inst: existing}, {err: notFoundErr()}}
			fake.getErr = notFoundErr()
			logs := captureRuntimeLog(t)

			if _, err := rt.Run(context.Background(), tc.runCfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(fake.deleteReqs) != 1 || len(fake.createReqs) != 1 || len(fake.startReqs) != 0 {
				t.Fatalf("delete/create/start = %d/%d/%d, want 1/1/0", len(fake.deleteReqs), len(fake.createReqs), len(fake.startReqs))
			}
			created := fake.createReqs[0].GetInstance()
			if diff := cloudRunSpecDiff(created, existing, map[string]bool{stagedsecrets.EnvVar: true, "EXAMPLE_TOKEN": true}); len(diff) == 0 {
				t.Error("created instance equals the existing one, want the computed spec")
			}
			if tc.check != nil {
				tc.check(t, created)
			}
			out := logs.String()
			if !strings.Contains(out, "changed_fields="+tc.field) {
				t.Errorf("log does not name changed field %s (log length %d)", tc.field, len(out))
			}
			if strings.Contains(out, canary) {
				t.Error("log carries a secret value")
			}
		})
	}
}

// S4: this run's existing instance that matches the computed spec is
// started, not recreated (the control for the cases above).
func TestCloudRunRun_ExistingInstanceMatchingSpecStarted(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	fake.getInstance = computedCloudRunInstance(t, rt, readyRunCfg())
	if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.startReqs) != 1 || len(fake.createReqs) != 0 || len(fake.deleteReqs) != 0 {
		t.Errorf("start/create/delete = %d/%d/%d, want 1/0/0", len(fake.startReqs), len(fake.createReqs), len(fake.deleteReqs))
	}
}

// S4: when Start's listing fails, its pre-clean does not run
// (pkg/agent/run.go), and Run reads the agent's instance itself. A legacy
// instance, or this run's own from an earlier Run, carrying the Args of an
// earlier start is recreated, never started with those Args.
func TestCloudRunRun_ListFailedPreCleanStaleArgsRecreated(t *testing.T) {
	for _, label := range []string{"", readyRunID} {
		t.Run("label="+label, func(t *testing.T) {
			fake := &fakeInstancesClient{listErr: status.Error(codes.Unavailable, "list unavailable")}
			rt := newReadyTestRuntime(t, fake)
			if _, err := rt.List(context.Background(), map[string]string{"scion.name": "test-agent"}); err == nil {
				t.Fatal("List succeeded, want the listing failure Start sees")
			}
			stale := computedCloudRunInstance(t, rt, readyRunCfg())
			stale.Containers[0].Args = []string{"/bin/sh", "-c", "a launch command of an earlier start"}
			if label == "" {
				delete(stale.Labels, sanitizeGCPLabelKey(api.LabelRunID))
			}
			fake.getSeq = []fakeGetResult{{inst: stale}, {inst: stale}, {err: notFoundErr()}}
			fake.getErr = notFoundErr()

			if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(fake.startReqs) != 0 || len(fake.deleteReqs) != 1 || len(fake.createReqs) != 1 {
				t.Fatalf("start/delete/create = %d/%d/%d, want 0/1/1", len(fake.startReqs), len(fake.deleteReqs), len(fake.createReqs))
			}
			if got := fake.createReqs[0].GetInstance().GetContainers()[0].GetArgs(); len(got) > 0 && strings.Contains(strings.Join(got, " "), "earlier start") {
				t.Error("the recreated instance carries the stale Args")
			}
		})
	}
}

func TestCloudRunPollRetryable(t *testing.T) {
	for code, want := range map[codes.Code]bool{
		codes.Unavailable:        true,
		codes.ResourceExhausted:  true,
		codes.DeadlineExceeded:   true,
		codes.Aborted:            true,
		codes.Internal:           true,
		codes.PermissionDenied:   false,
		codes.Unauthenticated:    false,
		codes.InvalidArgument:    false,
		codes.FailedPrecondition: false,
		codes.NotFound:           false,
	} {
		if got := cloudRunPollRetryable(status.Error(code, "x")); got != want {
			t.Errorf("cloudRunPollRetryable(%s) = %v, want %v", code, got, want)
		}
	}
}

// settledShapes returns this run's existing instance (the spec rt
// computes) in the recorded stopping shape, its stopped shape after the
// stop settles, and its restarted shape after the start.
func settledShapes(t *testing.T, rt *CloudRunRuntime) (stopping, stopped, restarted *runpb.Instance) {
	t.Helper()
	base := computedCloudRunInstance(t, rt, readyRunCfg())
	shape := func(src *runpb.Instance, etag string) *runpb.Instance {
		inst := proto.Clone(base).(*runpb.Instance)
		inst.Generation, inst.ObservedGeneration, inst.Reconciling = src.Generation, src.ObservedGeneration, src.Reconciling
		inst.TerminalCondition, inst.Conditions = src.TerminalCondition, src.Conditions
		inst.Etag = etag
		return inst
	}
	return shape(recordedStopping(), "etag-1"), shape(recordedStopped(), "etag-2"), shape(recordedRestarted(), "etag-3")
}

// (c): a start right after a stop reaches the instance while the stop is
// still settling; StartInstance is refused with FailedPrecondition and the
// re-read still reports reconciling. The start is retried with the
// re-read's etag and succeeds: one more start, no create, no delete.
func TestCloudRunRun_StartRetriedWhileInstanceSettles(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	stopping, _, restarted := settledShapes(t, rt)
	stillStopping := proto.Clone(stopping).(*runpb.Instance)
	stillStopping.Etag = "etag-2"
	fake.getSeq = []fakeGetResult{{inst: stopping}, {inst: stillStopping}}
	fake.startErrSeq = []error{status.Error(codes.FailedPrecondition, "instance is being stopped"), nil}
	fake.startOpInstance = restarted

	if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.startReqs) != 2 || len(fake.createReqs) != 0 || len(fake.deleteReqs) != 0 {
		t.Fatalf("start/create/delete = %d/%d/%d, want 2/0/0", len(fake.startReqs), len(fake.createReqs), len(fake.deleteReqs))
	}
	if got := fake.startReqs[1].GetEtag(); got != "etag-2" {
		t.Errorf("retried start etag = %q, want the re-read's etag-2", got)
	}
}

// (c): a permission refusal of StartInstance (as the missing actAs
// permission, ptone/scion#7319) fails at once, naming the instance and its
// state, never the platform's message text, and is not retried.
func TestCloudRunRun_StartPermissionDeniedNotRetried(t *testing.T) {
	const platformText = "Permission 'iam.serviceaccounts.actAs' denied on service account"
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	_, stopped, _ := settledShapes(t, rt)
	fake.getInstance = stopped
	fake.startErr = status.Error(codes.PermissionDenied, platformText)

	_, err := rt.Run(context.Background(), readyRunCfg())
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Run = %v, want PermissionDenied", err)
	}
	short := readyInstanceName[strings.LastIndex(readyInstanceName, "/")+1:]
	for _, want := range []string{short, "not retried", "condition=CONDITION_FAILED"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), platformText) {
		t.Error("error carries the platform's message text")
	}
	if len(fake.startReqs) != 1 || len(fake.getReqs) != 1 {
		t.Errorf("start/get = %d/%d, want 1/1 (no retry, no re-read)", len(fake.startReqs), len(fake.getReqs))
	}
}

// (c): an Aborted start (the etag changed) whose re-read is not
// reconciling is not retried: a changed instance is not started blindly.
func TestCloudRunRun_StartAbortedNotSettlingNotRetried(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	_, stopped, _ := settledShapes(t, rt)
	fake.getInstance = stopped
	fake.startErr = status.Error(codes.Aborted, "etag mismatch")

	if _, err := rt.Run(context.Background(), readyRunCfg()); status.Code(err) != codes.Aborted {
		t.Fatalf("Run = %v, want Aborted", err)
	}
	if len(fake.startReqs) != 1 {
		t.Errorf("starts = %d, want 1", len(fake.startReqs))
	}
}

// (c): a start refused for as long as the instance keeps settling ends at
// the wait cap, naming the last state read.
func TestCloudRunRun_StartRefusedUntilCapNamesLastState(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	stopping, _, _ := settledShapes(t, rt)
	fake.getInstance = stopping
	fake.startErr = status.Error(codes.FailedPrecondition, "instance is being stopped")

	_, err := rt.Run(context.Background(), readyRunCfg())
	if !errors.Is(err, errCloudRunReadyCap) || status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Run = %v, want the wait cap with the FailedPrecondition refusal", err)
	}
	for _, want := range []string{"condition=CONDITION_RECONCILING", "reconciling=true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if n := len(fake.startReqs); n < 2 || n > cloudRunReadyMaxPolls {
		t.Errorf("starts = %d, want retries within the poll bound", n)
	}
	if len(fake.createReqs) != 0 || len(fake.deleteReqs) != 0 {
		t.Errorf("create/delete = %d/%d, want 0/0", len(fake.createReqs), len(fake.deleteReqs))
	}
}

// (e): the wait's cap stays below the hub's default launch budget, read
// from the hub's DefaultServerConfig source (LaunchTimeout), so a change
// to either cannot let the launch deadline end the wait first.
func TestCloudRunReadyCapBelowHubLaunchBudget(t *testing.T) {
	budget := hubDefaultLaunchTimeout(t)
	if cloudRunReadyTimeout >= budget {
		t.Fatalf("ready cap %s is not below the hub's launch budget %s", cloudRunReadyTimeout, budget)
	}
}

// hubDefaultLaunchTimeout returns the LaunchTimeout value of
// DefaultServerConfig in pkg/hub/server.go, which must be written as
// <integer> * time.<Minute|Second>.
func hubDefaultLaunchTimeout(t *testing.T) time.Duration {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "hub", "server.go"), nil, 0)
	if err != nil {
		t.Fatalf("parsing the hub's server.go: %v", err)
	}
	var found []time.Duration
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "DefaultServerConfig" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "LaunchTimeout" {
				return true
			}
			bin, ok := kv.Value.(*ast.BinaryExpr)
			if !ok || bin.Op != token.MUL {
				t.Fatalf("DefaultServerConfig LaunchTimeout is not <integer> * time.<unit>")
			}
			lit, ok1 := bin.X.(*ast.BasicLit)
			sel, ok2 := bin.Y.(*ast.SelectorExpr)
			if !ok1 || !ok2 || lit.Kind != token.INT {
				t.Fatalf("DefaultServerConfig LaunchTimeout is not <integer> * time.<unit>")
			}
			n64, err := strconv.ParseInt(lit.Value, 10, 64)
			if err != nil {
				t.Fatalf("DefaultServerConfig LaunchTimeout: %v", err)
			}
			unit := map[string]time.Duration{"Minute": time.Minute, "Second": time.Second}[sel.Sel.Name]
			if unit == 0 {
				t.Fatalf("DefaultServerConfig LaunchTimeout unit %q is not Minute or Second", sel.Sel.Name)
			}
			found = append(found, time.Duration(n64)*unit)
			return false
		})
		return false
	})
	if len(found) != 1 {
		t.Fatalf("found %d LaunchTimeout values in DefaultServerConfig, want 1", len(found))
	}
	return found[0]
}

// (e): when the launch deadline (ctx) ends the wait, the error names the
// instance and the last state read, and nothing is deleted. The instance
// left is this run's: a later start of the run reuses it when it matches
// (S4), and a start of another run removes it in Start's pre-clean.
func TestCloudRunRun_LaunchDeadlineEndsWaitNamingState(t *testing.T) {
	pending := readyShape(recordedCreating, readyRunID)
	fake := &fakeInstancesClient{
		getSeq:           []fakeGetResult{{err: notFoundErr()}},
		getInstance:      pending,
		createOpInstance: pending,
	}
	rt := newReadyTestRuntime(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := 0
	rt.readyBoundsForTest.sleep = func(c context.Context, d time.Duration) error {
		sleeps++
		if sleeps == 2 {
			cancel()
		}
		return c.Err()
	}

	_, err := rt.Run(ctx, readyRunCfg())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the launch context's end", err)
	}
	short := readyInstanceName[strings.LastIndex(readyInstanceName, "/")+1:]
	for _, want := range []string{short, "condition=CONDITION_PENDING"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if len(fake.deleteReqs) != 0 || len(fake.createReqs) != 1 {
		t.Fatalf("delete/create = %d/%d, want 0/1", len(fake.deleteReqs), len(fake.createReqs))
	}

	// A later start of the same run finds the instance, which matches
	// its spec, and starts it: nothing is left behind or recreated.
	rt.readyBoundsForTest = fakeClockReadyBounds()
	left := computedCloudRunInstance(t, rt, readyRunCfg())
	fake.getInstance = left
	if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
		t.Fatalf("later Run: %v", err)
	}
	if len(fake.startReqs) != 1 || len(fake.createReqs) != 1 || len(fake.deleteReqs) != 0 {
		t.Errorf("start/create/delete = %d/%d/%d, want 1/1/0", len(fake.startReqs), len(fake.createReqs), len(fake.deleteReqs))
	}
}

// (d): a start with no run ID cannot run-check a delete, so a labelled
// instance whose spec differs is refused with ErrRunConflict and never
// deleted or started.
func TestCloudRunRun_NoRunIDNeverDeletesLabelledInstance(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	labelled := computedCloudRunInstance(t, rt, runConfigForTest())
	labelled.Labels[sanitizeGCPLabelKey(api.LabelRunID)] = "run-a"
	labelled.Containers[0].Image = "docker.io/scion/agent:earlier"
	fake.getInstance = labelled
	fake.getErr = nil

	_, err := rt.Run(context.Background(), runConfigForTest())
	if !errors.Is(err, ErrRunConflict) {
		t.Fatalf("Run = %v, want ErrRunConflict", err)
	}
	if len(fake.deleteReqs) != 0 || len(fake.startReqs) != 0 || len(fake.createReqs) != 0 {
		t.Errorf("delete/start/create = %d/%d/%d, want 0/0/0", len(fake.deleteReqs), len(fake.startReqs), len(fake.createReqs))
	}
}

// (c), O2: a settled instance (not reconciling) that still refuses
// StartInstance with FailedPrecondition fails at once, naming the
// instance and its state, rather than retrying to the cap.
func TestCloudRunRun_StartSettledFailedPreconditionNotRetried(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	_, stopped, _ := settledShapes(t, rt)
	fake.getInstance = stopped
	fake.startErr = status.Error(codes.FailedPrecondition, "precondition not met")

	_, err := rt.Run(context.Background(), readyRunCfg())
	if status.Code(err) != codes.FailedPrecondition || errors.Is(err, errCloudRunReadyCap) {
		t.Fatalf("Run = %v, want FailedPrecondition at once, not the cap", err)
	}
	short := readyInstanceName[strings.LastIndex(readyInstanceName, "/")+1:]
	for _, want := range []string{short, "not retried", "condition=CONDITION_FAILED", "reconciling=false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "precondition not met") {
		t.Error("error carries the platform's message text")
	}
	if len(fake.startReqs) != 1 {
		t.Errorf("starts = %d, want 1", len(fake.startReqs))
	}
}

// O4: when the caller's context ends during a wait read, the error still
// names the last state read before it.
func TestCloudRunRun_ContextEndMidReadNamesLastState(t *testing.T) {
	pending := readyShape(recordedCreating, readyRunID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeInstancesClient{
		getSeq:           []fakeGetResult{{err: notFoundErr()}, {inst: pending}, {err: context.Canceled}},
		createOpInstance: pending,
	}
	fake.beforeGet = func(n int) {
		if n == 3 {
			cancel() // the context ends during the third read
		}
	}
	rt := newReadyTestRuntime(t, fake)

	_, err := rt.Run(ctx, readyRunCfg())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the context's end", err)
	}
	short := readyInstanceName[strings.LastIndex(readyInstanceName, "/")+1:]
	for _, want := range []string{short, "condition=CONDITION_PENDING", "reconciling=true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if len(fake.deleteReqs) != 0 {
		t.Errorf("deletes = %d, want 0", len(fake.deleteReqs))
	}
}

// R1: a start with no run ID that finds an instance labelled with a run
// and matching the computed spec reuses it, and says so in an Info log
// naming the instance, its run and state, with no env value.
func TestCloudRunRun_NoRunIDReuseOfLabelledInstanceLogged(t *testing.T) {
	const canary = "no-run-id-reuse-canary-value"
	cfg := runConfigForTest()
	cfg.ResolvedSecrets = []api.ResolvedSecret{{Name: "token", Type: "environment", Target: "EXAMPLE_TOKEN", Value: canary}}
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	labelled := computedCloudRunInstance(t, rt, cfg)
	labelled.Labels[sanitizeGCPLabelKey(api.LabelRunID)] = "run-a"
	fake.getInstance = labelled
	logs := captureRuntimeLog(t)

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.startReqs) != 1 || len(fake.deleteReqs) != 0 || len(fake.createReqs) != 0 {
		t.Fatalf("start/delete/create = %d/%d/%d, want 1/0/0", len(fake.startReqs), len(fake.deleteReqs), len(fake.createReqs))
	}
	out := logs.String()
	for _, want := range []string{"Cloud Run start with no run ID: reusing a matching instance labelled with a run", "instance_run_id=run-a", "state="} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not contain %q (log length %d)", want, len(out))
		}
	}
	if strings.Contains(out, canary) {
		t.Error("log carries a secret value")
	}
}

// O5: an existing instance whose env var of the same name is backed by a
// secret reference (a value source), where this start sets a literal, is
// recreated: the secret-backed env differs.
func TestCloudRunRun_SecretRefValueSourceDiffRecreated(t *testing.T) {
	cfg := readyRunCfg()
	cfg.Env = []string{"EXAMPLE_SETTING=plain"}
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	existing := computedCloudRunInstance(t, rt, cfg)
	for _, ev := range existing.Containers[0].Env {
		if ev.GetName() == "EXAMPLE_SETTING" {
			ev.Values = &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{SecretKeyRef: &runpb.SecretKeySelector{Secret: "example-secret", Version: "1"}}}
		}
	}
	fake.getSeq = []fakeGetResult{{inst: existing}, {inst: existing}, {err: notFoundErr()}}
	fake.getErr = notFoundErr()
	logs := captureRuntimeLog(t)

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.deleteReqs) != 1 || len(fake.createReqs) != 1 || len(fake.startReqs) != 0 {
		t.Fatalf("delete/create/start = %d/%d/%d, want 1/1/0", len(fake.deleteReqs), len(fake.createReqs), len(fake.startReqs))
	}
	if out := logs.String(); !strings.Contains(out, "changed_fields=secret_env") {
		t.Errorf("log does not name secret_env (log length %d)", len(out))
	}
}

// O5: two otherwise equal instances whose secret reference differs only in
// its version differ in their secret-backed env.
func TestCloudRunRun_SpecDiffSecretRefVersion(t *testing.T) {
	inst := func(version string) *runpb.Instance {
		return &runpb.Instance{Containers: []*runpb.Container{{Env: []*runpb.EnvVar{{
			Name:   "EXAMPLE_KEY",
			Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{SecretKeyRef: &runpb.SecretKeySelector{Secret: "example-secret", Version: version}}},
		}}}}}
	}
	if diff := cloudRunSpecDiff(inst("1"), inst("2"), nil); !slices.Equal(diff, []string{"secret_env"}) {
		t.Errorf("diff = %v, want [secret_env]", diff)
	}
	if diff := cloudRunSpecDiff(inst("1"), inst("1"), nil); len(diff) != 0 {
		t.Errorf("diff of equal instances = %v, want none", diff)
	}
}

// O5: an existing instance carrying an env-type secret this start no
// longer sets is recreated (the env name set differs), so the removed
// secret is never kept.
func TestCloudRunRun_RemovedSecretEnvNameRecreated(t *testing.T) {
	withSecret := readyRunCfg()
	withSecret.ResolvedSecrets = []api.ResolvedSecret{{Name: "token", Type: "environment", Target: "EXAMPLE_TOKEN", Value: "removed-secret-canary"}}
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	existing := computedCloudRunInstance(t, rt, withSecret)
	fake.getSeq = []fakeGetResult{{inst: existing}, {inst: existing}, {err: notFoundErr()}}
	fake.getErr = notFoundErr()
	logs := captureRuntimeLog(t)

	if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.deleteReqs) != 1 || len(fake.createReqs) != 1 || len(fake.startReqs) != 0 {
		t.Fatalf("delete/create/start = %d/%d/%d, want 1/1/0", len(fake.deleteReqs), len(fake.createReqs), len(fake.startReqs))
	}
	for _, ev := range fake.createReqs[0].GetInstance().GetContainers()[0].GetEnv() {
		if ev.GetName() == "EXAMPLE_TOKEN" {
			t.Error("the recreated instance still carries the removed secret's env var")
		}
	}
	out := logs.String()
	if !strings.Contains(out, "changed_fields=env_names") {
		t.Errorf("log does not name env_names (log length %d)", len(out))
	}
	if strings.Contains(out, "removed-secret-canary") {
		t.Error("log carries a secret value")
	}
}

// O-1: the stop settles between the refused start and the re-read: the
// settled instance carries a new etag and was never asked to start, so it
// is started once more with that etag and the start succeeds.
func TestCloudRunRun_StartSettledAfterRefusalRetriedOnce(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	stopping, stopped, restarted := settledShapes(t, rt)
	fake.getSeq = []fakeGetResult{{inst: stopping}, {inst: stopped}}
	fake.startErrSeq = []error{status.Error(codes.FailedPrecondition, "instance is being stopped"), nil}
	fake.startOpInstance = restarted

	if _, err := rt.Run(context.Background(), readyRunCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.startReqs) != 2 || len(fake.createReqs) != 0 || len(fake.deleteReqs) != 0 {
		t.Fatalf("start/create/delete = %d/%d/%d, want 2/0/0", len(fake.startReqs), len(fake.createReqs), len(fake.deleteReqs))
	}
	if got := fake.startReqs[1].GetEtag(); got != "etag-2" {
		t.Errorf("retried start etag = %q, want the settled instance's etag-2", got)
	}
}

// O-1: the settled instance is started once more with its new etag; when
// it refuses that too, the start fails at once (one retry only, not the
// cap), naming the instance and its state. The second settled re-read
// carries yet another etag, so only the once guard stops a further retry.
func TestCloudRunRun_StartSettledRetriedOnlyOnce(t *testing.T) {
	fake := &fakeInstancesClient{}
	rt := newReadyTestRuntime(t, fake)
	stopping, stopped, _ := settledShapes(t, rt)
	stoppedAgain := proto.Clone(stopped).(*runpb.Instance)
	stoppedAgain.Etag = "etag-3"
	fake.getSeq = []fakeGetResult{{inst: stopping}, {inst: stopped}, {inst: stoppedAgain}}
	fake.getInstance = stoppedAgain
	fake.startErr = status.Error(codes.FailedPrecondition, "precondition not met")

	_, err := rt.Run(context.Background(), readyRunCfg())
	if status.Code(err) != codes.FailedPrecondition || errors.Is(err, errCloudRunReadyCap) {
		t.Fatalf("Run = %v, want FailedPrecondition after one retry, not the cap", err)
	}
	if !strings.Contains(err.Error(), "not retried again") || !strings.Contains(err.Error(), "condition=CONDITION_FAILED") {
		t.Errorf("error %q does not name the refusal after the retry and the state", err)
	}
	if len(fake.startReqs) != 2 {
		t.Errorf("starts = %d, want 2", len(fake.startReqs))
	}
}

// O-3 (and O-2): at every point where the caller's context can end
// during a call (a wait read, a gone-wait read, StartInstance and its
// re-read) the error is the context's end and names the last state read.
// The wait-read case follows a retried error read, so its last state is
// the instance read before that error (O-2).
func TestCloudRunRun_ContextEndAtEverySiteNamesLastState(t *testing.T) {
	short := readyInstanceName[strings.LastIndex(readyInstanceName, "/")+1:]
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient, cancel context.CancelFunc)
		want  string
	}{
		{"wait read after an error read", func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient, cancel context.CancelFunc) {
			pending := readyShape(recordedCreating, readyRunID)
			fake.createOpInstance = pending
			fake.getSeq = []fakeGetResult{{err: notFoundErr()}, {err: status.Error(codes.Unavailable, "try again")}, {err: context.Canceled}}
			fake.beforeGet = func(n int) {
				if n == 3 {
					cancel()
				}
			}
		}, "condition=CONDITION_PENDING"},
		{"gone-wait read", func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient, cancel context.CancelFunc) {
			deleting := readyShape(recordedDeleting, "run-a")
			fake.getSeq = []fakeGetResult{{inst: deleting}, {inst: deleting}, {err: context.Canceled}}
			fake.beforeGet = func(n int) {
				if n == 3 {
					cancel()
				}
			}
		}, "deleted=true"},
		{"StartInstance", func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient, cancel context.CancelFunc) {
			_, stopped, _ := settledShapes(t, rt)
			fake.getInstance = stopped
			fake.startErr = context.Canceled
			fake.beforeStart = func(int) { cancel() }
		}, "condition=CONDITION_FAILED"},
		{"start re-read", func(t *testing.T, rt *CloudRunRuntime, fake *fakeInstancesClient, cancel context.CancelFunc) {
			_, stopped, _ := settledShapes(t, rt)
			fake.getSeq = []fakeGetResult{{inst: stopped}, {err: context.Canceled}}
			fake.startErr = status.Error(codes.FailedPrecondition, "instance is being stopped")
			fake.beforeGet = func(n int) {
				if n == 2 {
					cancel()
				}
			}
		}, "condition=CONDITION_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstancesClient{}
			rt := newReadyTestRuntime(t, fake)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.setup(t, rt, fake, cancel)

			_, err := rt.Run(ctx, readyRunCfg())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run = %v, want the context's end", err)
			}
			for _, want := range []string{short, tc.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if len(fake.deleteReqs) != 0 {
				t.Errorf("deletes = %d, want 0", len(fake.deleteReqs))
			}
		})
	}
}
