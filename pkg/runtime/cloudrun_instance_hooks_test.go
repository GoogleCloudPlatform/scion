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
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CR-5 (ptone/scion#4761): the Cloud Run Instances runtime's async launch
// hooks (the workspace_provision, instance_start and instance_create
// checkpoints and the instance handle), the in-doubt set and
// DeleteResource. Each test names the fault check (a deliberate production
// change) it must catch, by the issue's ID. Timing variables are set to
// milliseconds; bounds on elapsed time keep wide margins.

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

const (
	ciRunA   = "run-a"
	ciParent = "projects/test-project/locations/us-central1"
)

// ciInstanceID and ciInstanceName are the instance ID and resource name
// of runConfigForTest's agent.
var (
	ciInstanceID   = cloudRunInstanceID("agent-1")
	ciInstanceName = ciParent + "/instances/" + ciInstanceID
)

// ciHandle is the handle a start of runID reports.
func ciHandle(runID string) api.ResourceHandle {
	return api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance, Namespace: ciParent, Name: ciInstanceID, UID: runID}
}

// ciInstance is the agent's instance labelled with run (none when "").
func ciInstance(run string) *runpb.Instance {
	labels := map[string]string{"agent_id": "agent-1"}
	if run != "" {
		labels[sanitizeGCPLabelKey(api.LabelRunID)] = sanitizeGCPLabelValue(run)
	}
	return &runpb.Instance{Name: ciInstanceName, Etag: "etag-1", Labels: labels}
}

// ciNotFound is a gRPC NotFound.
func ciNotFound() fakeGetResult { return fakeGetResult{err: notFoundErr()} }

// ciSetup returns a fake and a runtime on it, with the cleanup timing in
// milliseconds (budget 3 s, re-poll window 500 ms, polls 20 ms, backoff
// 20 ms) and an empty in-doubt set; both are restored when the test ends.
func ciSetup(t *testing.T) (*fakeInstancesClient, *CloudRunRuntime) {
	t.Helper()
	budget, window, interval, poll, backoff := cloudRunCleanupBudget, cloudRunInDoubtRepoll, cloudRunRepollInterval, cloudRunReconcilePoll, cloudRunRefusalBackoff
	t.Cleanup(func() {
		cloudRunCleanupBudget, cloudRunInDoubtRepoll, cloudRunRepollInterval, cloudRunReconcilePoll, cloudRunRefusalBackoff = budget, window, interval, poll, backoff
		ciResetInDoubt()
	})
	cloudRunCleanupBudget = 3 * time.Second
	cloudRunInDoubtRepoll = 500 * time.Millisecond
	cloudRunRepollInterval = 20 * time.Millisecond
	cloudRunReconcilePoll = 20 * time.Millisecond
	cloudRunRefusalBackoff = []time.Duration{20 * time.Millisecond}
	ciResetInDoubt()
	fake := &fakeInstancesClient{}
	return fake, newFakeCloudRunRuntime(t, fake)
}

func ciResetInDoubt() {
	cloudRunCreatesInDoubt.mu.Lock()
	defer cloudRunCreatesInDoubt.mu.Unlock()
	cloudRunCreatesInDoubt.marks = map[cloudRunInDoubtKey]time.Time{}
}

// ciMarked reports whether the set holds a mark for (name, runID),
// expired or not.
func ciMarked(name, runID string) bool {
	cloudRunCreatesInDoubt.mu.Lock()
	defer cloudRunCreatesInDoubt.mu.Unlock()
	_, ok := cloudRunCreatesInDoubt.marks[cloudRunInDoubtKey{name: name, runID: runID}]
	return ok
}

// ciCfg is runConfigForTest for run runID with rec's hooks installed.
func ciCfg(runID string, rec *sandboxHookRecorder) RunConfig {
	cfg := runConfigForTest()
	cfg.Labels[api.LabelRunID] = runID
	rec.install(&cfg)
	return cfg
}

// ciCutOffCreate runs an async start of run A whose CreateInstance blocks
// until the launch ctx ends, and fails the test unless Run fails and the
// create is left in doubt. The fake's Run-time settings are then cleared.
func ciCutOffCreate(t *testing.T, fake *fakeInstancesClient, rt *CloudRunRuntime) {
	t.Helper()
	fake.getErr = notFoundErr()
	fake.createBlocks = true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := rt.Run(ctx, ciCfg(ciRunA, &sandboxHookRecorder{})); err == nil {
		t.Fatal("Run with a cut-off create succeeded, want an error")
	}
	if !ciMarked(ciInstanceName, ciRunA) {
		t.Fatal("the cut-off create is not in doubt")
	}
	fake.getErr = nil
	fake.createBlocks = false
	fake.getReqs = nil
}

// ciCleanup runs DeleteResource for run A's handle on a fresh ctx and
// returns its error and duration.
func ciCleanup(rt *CloudRunRuntime) (time.Duration, error) {
	start := time.Now()
	err := rt.DeleteResource(context.Background(), ciHandle(ciRunA))
	return time.Since(start), err
}

// -----------------------------------------------------------------------
// Run: checkpoints and the handle
// -----------------------------------------------------------------------

// C1, C2, C4: an abort at each checkpoint returns the checkpoint's error
// and skips the call that follows it (NFS provisioning, CreateInstance,
// StartInstance), reporting no handle.
func TestCloudRunInstanceHooks_CheckpointAbort(t *testing.T) {
	cases := []struct {
		step           string
		setup          func(f *fakeInstancesClient, cfg *RunConfig)
		wantSteps      []string
		wantGets       int
		wantProvisions int
	}{
		{
			// Provisioning is counted below: an abort here must not
			// reach it.
			step:           CheckpointStepWorkspaceProvision,
			setup:          func(f *fakeInstancesClient, _ *RunConfig) { f.getErr = notFoundErr() },
			wantSteps:      []string{CheckpointStepWorkspaceProvision},
			wantGets:       0,
			wantProvisions: 0,
		},
		{
			step:           CheckpointStepInstanceCreate,
			setup:          func(f *fakeInstancesClient, _ *RunConfig) { f.getErr = notFoundErr() },
			wantSteps:      []string{CheckpointStepWorkspaceProvision, CheckpointStepInstanceCreate},
			wantGets:       1,
			wantProvisions: 1,
		},
		{
			step:           CheckpointStepInstanceStart,
			setup:          func(f *fakeInstancesClient, _ *RunConfig) { f.getInstance = ciInstance(ciRunA) },
			wantSteps:      []string{CheckpointStepWorkspaceProvision, CheckpointStepInstanceStart},
			wantGets:       1,
			wantProvisions: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.step, func(t *testing.T) {
			fake, rt := ciSetup(t)
			provisions := 0
			rt.provisionNFS = func(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error) {
				provisions++
				return stubCloudRunNFSProvision(ctx, cfg, agentID, uid, gid)
			}
			rec := &sandboxHookRecorder{abortAt: tc.step}
			cfg := ciCfg(ciRunA, rec)
			tc.setup(fake, &cfg)

			_, err := rt.Run(context.Background(), cfg)
			if !errors.Is(err, errTestAbort) {
				t.Fatalf("Run = %v, want the checkpoint's error", err)
			}
			if provisions != tc.wantProvisions {
				t.Errorf("NFS provisioning ran %d times, want %d", provisions, tc.wantProvisions)
			}
			if got := rec.gotSteps(); !reflect.DeepEqual(got, tc.wantSteps) {
				t.Errorf("steps = %v, want %v", got, tc.wantSteps)
			}
			if len(fake.getReqs) != tc.wantGets || len(fake.createReqs) != 0 || len(fake.startReqs) != 0 {
				t.Errorf("calls: get %d create %d start %d, want get %d and no create or start",
					len(fake.getReqs), len(fake.createReqs), len(fake.startReqs), tc.wantGets)
			}
			if h := rec.gotHandles(); len(h) != 0 {
				t.Errorf("handles = %+v, want none", h)
			}
			if ciMarked(ciInstanceName, ciRunA) {
				t.Error("an aborted start left a create in doubt")
			}
		})
	}
}

// C3: the handle is reported before CreateInstance is issued, so a create
// cut off mid-request (blocking until the launch ctx ends) still leaves
// the handle, and the create stays in doubt.
func TestCloudRunInstanceHooks_HandleBeforeCreate(t *testing.T) {
	fake, rt := ciSetup(t)
	rec := &sandboxHookRecorder{}
	var atCreate []api.ResourceHandle
	fake.onCreate = func() { atCreate = rec.gotHandles() }
	fake.getErr = notFoundErr()
	fake.createBlocks = true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if _, err := rt.Run(ctx, ciCfg(ciRunA, rec)); err == nil {
		t.Fatal("Run with a cut-off create succeeded, want an error")
	}
	if want := []api.ResourceHandle{ciHandle(ciRunA)}; !reflect.DeepEqual(atCreate, want) {
		t.Fatalf("handles when CreateInstance was called = %+v, want %+v", atCreate, want)
	}
	if got, want := rec.gotHandles(), []api.ResourceHandle{ciHandle(ciRunA)}; !reflect.DeepEqual(got, want) {
		t.Errorf("handles = %+v, want %+v", got, want)
	}
	if !ciMarked(ciInstanceName, ciRunA) {
		t.Error("the cut-off create is not in doubt")
	}
}

// C5: on the existing-instance path the handle is reported only for this
// run's own instance; a legacy instance with no run label is started but
// not reported.
func TestCloudRunInstanceHooks_StartHandleOwnRunOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label string
		want  []api.ResourceHandle
	}{
		{"own_run", ciRunA, []api.ResourceHandle{ciHandle(ciRunA)}},
		{"legacy", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, rt := ciSetup(t)
			fake.getInstance = ciInstance(tc.label)
			rec := &sandboxHookRecorder{}

			if _, err := rt.Run(context.Background(), ciCfg(ciRunA, rec)); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(fake.startReqs) != 1 {
				t.Fatalf("StartInstance called %d times, want 1", len(fake.startReqs))
			}
			if got := rec.gotHandles(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("handles = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// An async create whose operation succeeds is observed: its mark is
// cleared, so a later cleanup's NotFound is not re-polled.
func TestCloudRunInstanceHooks_CreateOKClearsMark(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getErr = notFoundErr()

	if _, err := rt.Run(context.Background(), ciCfg(ciRunA, &sandboxHookRecorder{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
	}
	if ciMarked(ciInstanceName, ciRunA) {
		t.Error("a create whose operation succeeded is still in doubt")
	}
}

// The synchronous path (no hooks) marks nothing in doubt, even when the
// create's wait fails.
func TestCloudRunInstanceHooks_SyncPathNoMark(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getErr = notFoundErr()
	fake.createWaitErr = context.Canceled
	cfg := runConfigForTest()
	cfg.Labels[api.LabelRunID] = ciRunA

	if _, err := rt.Run(context.Background(), cfg); err == nil {
		t.Fatal("Run with a failed create wait succeeded, want an error")
	}
	if ciMarked(ciInstanceName, ciRunA) {
		t.Error("a synchronous create was marked in doubt")
	}
}

// -----------------------------------------------------------------------
// DeleteResource
// -----------------------------------------------------------------------

// This run's instance is deleted with the read's etag.
func TestCloudRunInstanceDeleteResource_DeletesOwnInstance(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getInstance = ciInstance(ciRunA)

	if _, err := ciCleanup(rt); err != nil {
		t.Fatalf("DeleteResource = %v, want nil", err)
	}
	if len(fake.deleteReqs) != 1 {
		t.Fatalf("DeleteInstance called %d times, want 1", len(fake.deleteReqs))
	}
	if r := fake.deleteReqs[0]; r.Name != ciInstanceName || r.Etag != "etag-1" {
		t.Errorf("DeleteInstance request = %+v, want name %s etag etag-1", r, ciInstanceName)
	}
}

// C6: NotFound at the read, at the delete and at the delete's wait, for a
// create not in doubt, is success.
func TestCloudRunInstanceDeleteResource_NotFoundNotInDoubt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fakeInstancesClient)
	}{
		{"get", func(f *fakeInstancesClient) { f.getErr = notFoundErr() }},
		{"delete", func(f *fakeInstancesClient) { f.getInstance = ciInstance(ciRunA); f.deleteErr = notFoundErr() }},
		{"delete_wait", func(f *fakeInstancesClient) { f.getInstance = ciInstance(ciRunA); f.deleteWaitErr = notFoundErr() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, rt := ciSetup(t)
			tc.setup(fake)
			if _, err := ciCleanup(rt); err != nil {
				t.Fatalf("DeleteResource = %v, want nil", err)
			}
		})
	}
}

// C6b: a cut-off create is in doubt, so NotFound is re-polled; the
// instance that appears on the third read is deleted.
func TestCloudRunInstanceDeleteResource_CutOffCreateRepolled(t *testing.T) {
	fake, rt := ciSetup(t)
	ciCutOffCreate(t, fake, rt)
	cloudRunInDoubtRepoll = 2 * time.Second
	fake.getSeq = []fakeGetResult{ciNotFound(), ciNotFound(), {inst: ciInstance(ciRunA)}}

	if _, err := ciCleanup(rt); err != nil {
		t.Fatalf("DeleteResource = %v, want nil", err)
	}
	if len(fake.deleteReqs) != 1 {
		t.Fatalf("DeleteInstance called %d times, want 1 (the instance appeared after two NotFounds)", len(fake.deleteReqs))
	}
	if ciMarked(ciInstanceName, ciRunA) {
		t.Error("the create is still in doubt after its instance was deleted")
	}
}

// C6c: the re-poll is bounded: an instance that never appears is taken as
// not created once the window from the first NotFound has passed, well
// before the budget.
func TestCloudRunInstanceDeleteResource_RepollBounded(t *testing.T) {
	fake, rt := ciSetup(t)
	ciCutOffCreate(t, fake, rt)
	cloudRunInDoubtRepoll = 300 * time.Millisecond
	cloudRunCleanupBudget = 5 * time.Second
	fake.getErr = notFoundErr()

	elapsed, err := ciCleanup(rt)
	if err != nil {
		t.Fatalf("DeleteResource = %v after %s, want nil once the re-poll window passed", err, elapsed)
	}
	if elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("DeleteResource took %s, want the 300 ms window (and well under the 5 s budget)", elapsed)
	}
	if len(fake.getReqs) < 2 {
		t.Errorf("GetInstance called %d times, want re-polls", len(fake.getReqs))
	}
}

// C6d: a create the server accepted whose wait was cancelled stays in
// doubt, so a NotFound is re-polled and the instance that then appears is
// deleted.
func TestCloudRunInstanceDeleteResource_AcceptedCreateWaitCancelled(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getErr = notFoundErr()
	fake.createWaitErr = context.Canceled
	if _, err := rt.Run(context.Background(), ciCfg(ciRunA, &sandboxHookRecorder{})); err == nil {
		t.Fatal("Run with a cancelled create wait succeeded, want an error")
	}
	fake.getErr = nil
	fake.getSeq = []fakeGetResult{ciNotFound(), {inst: ciInstance(ciRunA)}}
	cloudRunInDoubtRepoll = 2 * time.Second

	if _, err := ciCleanup(rt); err != nil {
		t.Fatalf("DeleteResource = %v, want nil", err)
	}
	if len(fake.deleteReqs) != 1 {
		t.Fatalf("DeleteInstance called %d times, want 1 (the accepted create appeared after one NotFound)", len(fake.deleteReqs))
	}
}

// C6e: a create the server rejected (InvalidArgument) is not in doubt:
// the cleanup's NotFound returns nil at once, with no re-poll.
func TestCloudRunInstanceDeleteResource_ServerRejectedNotRepolled(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getErr = notFoundErr()
	fake.createErr = status.Error(codes.InvalidArgument, "bad instance")
	if _, err := rt.Run(context.Background(), ciCfg(ciRunA, &sandboxHookRecorder{})); err == nil {
		t.Fatal("Run with a rejected create succeeded, want an error")
	}
	fake.getReqs = nil
	cloudRunInDoubtRepoll = 2 * time.Second

	elapsed, err := ciCleanup(rt)
	if err != nil {
		t.Fatalf("DeleteResource = %v, want nil", err)
	}
	if len(fake.getReqs) != 1 {
		t.Fatalf("GetInstance called %d times in %s, want 1 (no re-poll)", len(fake.getReqs), elapsed)
	}
}

// C7: an unlabelled (legacy) instance is not this launch's: no delete.
func TestCloudRunInstanceDeleteResource_Unlabelled(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getInstance = ciInstance("")

	if _, err := ciCleanup(rt); err != nil {
		t.Fatalf("DeleteResource = %v, want nil", err)
	}
	if len(fake.deleteReqs) != 0 {
		t.Fatalf("DeleteInstance called %d times for an unlabelled instance, want 0", len(fake.deleteReqs))
	}
}

// C8: another run's instance is left untouched.
func TestCloudRunInstanceDeleteResource_OtherRun(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getInstance = ciInstance("run-b")

	if _, err := ciCleanup(rt); err != nil {
		t.Fatalf("DeleteResource = %v, want nil", err)
	}
	if len(fake.deleteReqs) != 0 {
		t.Fatalf("DeleteInstance called %d times for run B's instance, want 0", len(fake.deleteReqs))
	}
}

// C9: FailedPrecondition or Aborted refusals (the etag unchanged, the
// instance not reconciling) are retried with backoff until the delete is
// accepted.
func TestCloudRunInstanceDeleteResource_RefusedThenDeleted(t *testing.T) {
	for _, code := range []codes.Code{codes.FailedPrecondition, codes.Aborted} {
		t.Run(code.String(), func(t *testing.T) {
			fake, rt := ciSetup(t)
			fake.getInstance = ciInstance(ciRunA)
			refused := status.Error(code, "refused")
			fake.deleteErrSeq = []error{refused, refused, nil}

			if _, err := ciCleanup(rt); err != nil {
				t.Fatalf("DeleteResource = %v, want nil after two refusals", err)
			}
			if len(fake.deleteReqs) != 3 {
				t.Errorf("DeleteInstance called %d times, want 3", len(fake.deleteReqs))
			}
		})
	}
}

// C9b: a delete refused while the instance reconciles waits (polling)
// for the reconcile to clear, then retries. The backoff is an hour, so
// only the reconciling wait can retry within the budget.
func TestCloudRunInstanceDeleteResource_WaitsForReconcile(t *testing.T) {
	fake, rt := ciSetup(t)
	cloudRunRefusalBackoff = []time.Duration{time.Hour}
	fake.getInstance = ciInstance(ciRunA)
	fake.reconcilingReads = 5 // the run-checked read and re-read, then 3 polls

	elapsed, err := ciCleanup(rt)
	if err != nil {
		t.Fatalf("DeleteResource = %v after %s, want nil once the reconcile cleared", err, elapsed)
	}
	if len(fake.deleteReqs) != 2 {
		t.Errorf("DeleteInstance called %d times, want 2 (refused while reconciling, then accepted)", len(fake.deleteReqs))
	}
}

// C10: refusals are retried only within the budget: with every delete
// refused and a 300 ms budget, DeleteResource fails at about 300 ms.
func TestCloudRunInstanceDeleteResource_BudgetBoundsRefusals(t *testing.T) {
	fake, rt := ciSetup(t)
	cloudRunCleanupBudget = 300 * time.Millisecond
	fake.getInstance = ciInstance(ciRunA)
	fake.deleteErr = status.Error(codes.FailedPrecondition, "precondition failed")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := rt.DeleteResource(ctx, ciHandle(ciRunA))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("DeleteResource = nil, want an error when every delete is refused")
	}
	if elapsed < 250*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("DeleteResource returned after %s, want about the 300 ms budget", elapsed)
	}
}

// C10b: the budget applies inside the delete call: a DeleteInstance that
// blocks until its ctx ends returns at the budget, not at the outer ctx.
func TestCloudRunInstanceDeleteResource_BudgetInsideDeleteCall(t *testing.T) {
	fake, rt := ciSetup(t)
	cloudRunCleanupBudget = 300 * time.Millisecond
	fake.getInstance = ciInstance(ciRunA)
	fake.deleteBlocks = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := rt.DeleteResource(ctx, ciHandle(ciRunA))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("DeleteResource = nil, want an error when the delete call blocks")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("DeleteResource returned after %s, want about the 300 ms budget", elapsed)
	}
}

// C13: a handle whose namespace is not the runtime's parent is refused
// before any call.
func TestCloudRunInstanceDeleteResource_NamespaceChecked(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getInstance = ciInstance(ciRunA)
	h := ciHandle(ciRunA)
	h.Namespace = "projects/other-project/locations/us-central1"

	if err := rt.DeleteResource(context.Background(), h); err == nil {
		t.Fatal("DeleteResource in another namespace = nil, want an error")
	}
	if len(fake.getReqs) != 0 || len(fake.deleteReqs) != 0 {
		t.Errorf("calls: get %d delete %d, want none", len(fake.getReqs), len(fake.deleteReqs))
	}
}

// A handle of another kind, or with no UID, is refused before any call.
func TestCloudRunInstanceDeleteResource_KindAndUIDRefused(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getInstance = ciInstance("")
	other := ciHandle(ciRunA)
	other.Kind = api.ResourceKindSandbox
	noUID := ciHandle("")
	for _, h := range []api.ResourceHandle{other, noUID} {
		if err := rt.DeleteResource(context.Background(), h); err == nil {
			t.Errorf("DeleteResource(%+v) = nil, want an error", h)
		}
	}
	if len(fake.getReqs) != 0 || len(fake.deleteReqs) != 0 {
		t.Errorf("calls: get %d delete %d, want none", len(fake.getReqs), len(fake.deleteReqs))
	}
}

// C14: in-doubt marks expire at the shared cleanup horizon
// (AsyncCleanupHorizon of the launch ctx's deadline). With the horizon
// terms in milliseconds, a mark for run A is swept when a later mark is
// added after the horizon, so A's NotFound returns nil with no re-poll.
func TestCloudRunInstanceDeleteResource_InDoubtMarkExpires(t *testing.T) {
	fake, rt := ciSetup(t)
	skew, ttl, budget, slack := asyncLaunchAdmissionSkew, asyncLaunchReportTTL, asyncLaunchCleanupBudget, asyncCleanupSlack
	t.Cleanup(func() {
		asyncLaunchAdmissionSkew, asyncLaunchReportTTL, asyncLaunchCleanupBudget, asyncCleanupSlack = skew, ttl, budget, slack
	})
	asyncLaunchAdmissionSkew, asyncLaunchReportTTL, asyncLaunchCleanupBudget, asyncCleanupSlack =
		10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond

	ctxA, cancelA := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelA()
	dl, _ := ctxA.Deadline()
	markCloudRunCreateInDoubt(ctxA, ciInstanceName, ciRunA)
	if !ciMarked(ciInstanceName, ciRunA) {
		t.Fatal("run A's create was not marked")
	}
	// Wait well past A's horizon (deadline + 40 ms).
	time.Sleep(time.Until(AsyncCleanupHorizon(dl).Add(300 * time.Millisecond)))
	ctxB, cancelB := context.WithTimeout(context.Background(), time.Minute)
	defer cancelB()
	markCloudRunCreateInDoubt(ctxB, ciParent+"/instances/other", "run-b")

	if ciMarked(ciInstanceName, ciRunA) {
		t.Fatal("run A's mark is still in the set after its horizon passed")
	}
	cloudRunInDoubtRepoll = 2 * time.Second
	fake.getErr = notFoundErr()
	if _, err := ciCleanup(rt); err != nil {
		t.Fatalf("DeleteResource = %v, want nil", err)
	}
	if len(fake.getReqs) != 1 {
		t.Errorf("GetInstance called %d times, want 1 (no re-poll)", len(fake.getReqs))
	}
}

// One Instances client serves the whole cleanup: two refusals, the
// reconciling reads and the accepted delete all go through the client
// opened once at the start, which is closed on return.
func TestCloudRunInstanceDeleteResource_OneClientAcrossRetries(t *testing.T) {
	fake, rt := ciSetup(t)
	opens := 0
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) {
		opens++
		return fake, nil
	}
	fake.getInstance = ciInstance(ciRunA)
	refused := status.Error(codes.FailedPrecondition, "refused")
	fake.deleteErrSeq = []error{refused, refused, nil}

	if _, err := ciCleanup(rt); err != nil {
		t.Fatalf("DeleteResource = %v, want nil after two refusals", err)
	}
	if len(fake.deleteReqs) != 3 {
		t.Fatalf("DeleteInstance called %d times, want 3", len(fake.deleteReqs))
	}
	if opens != 1 || fake.closeCount != 1 {
		t.Errorf("Instances clients opened %d and closed %d times, want 1 and 1", opens, fake.closeCount)
	}
}

// When the budget runs out while waiting (the backoff after a refusal,
// the in-doubt re-poll, or the reconciling wait), the error wraps both
// context.DeadlineExceeded and the last API error.
func TestCloudRunInstanceDeleteResource_BudgetErrorWrapsBoth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, f *fakeInstancesClient, rt *CloudRunRuntime)
		wantCode codes.Code
	}{
		{"backoff", func(_ *testing.T, f *fakeInstancesClient, _ *CloudRunRuntime) {
			cloudRunRefusalBackoff = []time.Duration{time.Hour}
			f.getInstance = ciInstance(ciRunA)
			f.deleteErr = status.Error(codes.FailedPrecondition, "refused")
		}, codes.FailedPrecondition},
		{"repoll", func(t *testing.T, f *fakeInstancesClient, rt *CloudRunRuntime) {
			ciCutOffCreate(t, f, rt)
			cloudRunInDoubtRepoll = time.Hour
			f.getErr = notFoundErr()
		}, codes.NotFound},
		{"reconcile", func(_ *testing.T, f *fakeInstancesClient, _ *CloudRunRuntime) {
			f.getInstance = ciInstance(ciRunA)
			f.reconcilingReads = 1 << 30
		}, codes.FailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, rt := ciSetup(t)
			tc.setup(t, fake, rt)
			cloudRunCleanupBudget = 300 * time.Millisecond

			elapsed, err := ciCleanup(rt)
			if !errors.Is(err, context.DeadlineExceeded) || status.Code(err) != tc.wantCode {
				t.Fatalf("DeleteResource = %v after %s, want it to wrap context.DeadlineExceeded and a gRPC %s", err, elapsed, tc.wantCode)
			}
		})
	}
}
