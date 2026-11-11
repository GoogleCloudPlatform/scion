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
	"log/slog"
	"testing"
	"time"

	runapi "cloud.google.com/go/run/apiv2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ptone/scion#5154: a Cloud Run delete can take longer than the launch
// cleanup budget (72-76 s seen live, budget 45 s). DeleteResource counts a
// delete the API accepted, whose operation wait the budget's deadline cuts
// off, as success, logged as not confirmed gone; every other failure keeps
// its handling. Each test names the fault check (a deliberate production
// change) it must catch, T1-T5 as agreed for the issue.

// The Cloud Run client's delete operation exposes its name, which
// cloudRunOperationName reads through an optional Name method.
var _ interface{ Name() string } = (*runapi.DeleteInstanceOperation)(nil)

// dtNotConfirmedMsg is the warning DeleteResource logs for an accepted
// delete not confirmed gone within the budget.
const dtNotConfirmedMsg = "Launch cleanup: Cloud Run accepted the delete of an instance, but it was not confirmed gone within the budget"

// T1: an accepted delete whose operation is still running when the budget
// ends is success, at about the budget, with one delete call, the in-doubt
// mark cleared and a warning naming the instance, the run and the
// operation; the wait may end with the ctx error or with a gRPC
// DeadlineExceeded (an operation poll cut off mid-call). Fault checks:
// drop the accepted branch in DeleteResource (it returns the over-budget
// error); drop its clearCloudRunCreateInDoubt (the mark stays); drop the
// gRPC DeadlineExceeded arm of cloudRunWaitCutOffByDeadline (the
// grpc_deadline case returns the over-budget error).
func TestCloudRunInstanceDeleteResource_AcceptedDeleteCutOffIsDone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		waitErr error // returned by the blocked wait when its ctx ends; nil for the ctx error
	}{
		{"ctx_error", nil},
		{"grpc_deadline", status.Error(codes.DeadlineExceeded, "operation poll deadline exceeded")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, rt := ciSetup(t)
			cloudRunCleanupBudget = 300 * time.Millisecond
			fake.getInstance = ciInstance(ciRunA)
			fake.deleteWaitBlocks = true
			fake.deleteWaitBlockErr = tc.waitErr
			markCtx, cancelMark := context.WithTimeout(context.Background(), time.Hour)
			defer cancelMark()
			markCloudRunCreateInDoubt(markCtx, ciInstanceName, ciRunA)
			logs := newRecordingHandler(slog.LevelDebug)
			setRuntimeLogHandler(t, logs)

			elapsed, err := ciCleanup(rt)
			if err != nil {
				t.Fatalf("DeleteResource = %v after %s, want nil for an accepted delete not yet finished", err, elapsed)
			}
			if elapsed < 250*time.Millisecond || elapsed > 4*time.Second {
				t.Errorf("DeleteResource returned after %s, want about the 300 ms budget", elapsed)
			}
			if len(fake.deleteReqs) != 1 {
				t.Errorf("DeleteInstance called %d times, want 1", len(fake.deleteReqs))
			}
			if ciMarked(ciInstanceName, ciRunA) {
				t.Error("the create is still in doubt after the delete was accepted, want the mark cleared")
			}
			rec, ok := logs.findRecord(dtNotConfirmedMsg)
			if !ok {
				t.Fatalf("no %q warning logged; records: %+v", dtNotConfirmedMsg, logs.records())
			}
			if rec.level != slog.LevelWarn || rec.attrs["instance"] != ciInstanceID || rec.attrs["run_id"] != ciRunA || rec.attrs["operation"] != fakeDeleteOpName {
				t.Errorf("warning = %+v, want level WARN, instance %q, run_id %q, operation %q", rec, ciInstanceID, ciRunA, fakeDeleteOpName)
			}
		})
	}
}

// T1b: the wait on an accepted delete ended by a cancellation of the
// caller's ctx (not the budget's deadline) stays an error. Fault check:
// accept any end of the budget after acceptance (skip
// cloudRunWaitCutOffByDeadline in DeleteResource).
func TestCloudRunInstanceDeleteResource_AcceptedDeleteCancelledIsError(t *testing.T) {
	fake, rt := ciSetup(t)
	fake.getInstance = ciInstance(ciRunA)
	fake.deleteWaitBlocks = true
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	defer cancel()

	err := rt.DeleteResource(ctx, ciHandle(ciRunA))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteResource = %v, want an error wrapping context.Canceled", err)
	}
}

// T2: an operation failure reported after the delete was accepted stays an
// error, whether it comes at once or as the budget ends. Fault checks:
// count any wait error after acceptance as success (immediate case); in
// cloudRunWaitCutOffByDeadline, ignore the kind of the wait error and look
// only at the budget (at-deadline case).
func TestCloudRunInstanceDeleteResource_AcceptedDeleteOperationFailedIsError(t *testing.T) {
	opFailed := status.Error(codes.Internal, "delete operation failed")
	for _, tc := range []struct {
		name  string
		setup func(f *fakeInstancesClient)
	}{
		{"immediate", func(f *fakeInstancesClient) { f.deleteWaitErr = opFailed }},
		{"at_deadline", func(f *fakeInstancesClient) {
			f.deleteWaitBlocks = true
			f.deleteWaitBlockErr = opFailed
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, rt := ciSetup(t)
			cloudRunCleanupBudget = 300 * time.Millisecond
			fake.getInstance = ciInstance(ciRunA)
			tc.setup(fake)

			elapsed, err := ciCleanup(rt)
			if err == nil || status.Code(err) != codes.Internal {
				t.Fatalf("DeleteResource = %v after %s, want the operation's Internal error", err, elapsed)
			}
		})
	}
}

// T3: a delete call that the budget cuts off before the API answers was
// never accepted: the error wraps context.DeadlineExceeded. Fault check:
// set accepted in DeleteResource's deleteCall before DeleteInstance
// returns (it then returns nil).
func TestCloudRunInstanceDeleteResource_DeleteNotAcceptedIsError(t *testing.T) {
	fake, rt := ciSetup(t)
	cloudRunCleanupBudget = 300 * time.Millisecond
	fake.getInstance = ciInstance(ciRunA)
	fake.deleteBlocks = true

	elapsed, err := ciCleanup(rt)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DeleteResource = %v after %s, want an error wrapping context.DeadlineExceeded", err, elapsed)
	}
	if len(fake.deleteReqs) != 1 {
		t.Errorf("DeleteInstance called %d times, want 1", len(fake.deleteReqs))
	}
}

// T5: the API leaves reconciling out of a response when false; an
// instance read with it unset is not reconciling, so
// waitCloudRunReconciled returns at once after one read. Fault check: take
// an unset reconciling as reconciling (drop the negation in its check; it
// then polls until the ctx ends).
func TestWaitCloudRunReconciled_ReconcilingUnsetIsNotReconciling(t *testing.T) {
	fake, _ := ciSetup(t)
	fake.getInstance = ciInstance(ciRunA) // Reconciling unset
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	waited, err := waitCloudRunReconciled(ctx, fake, ciInstanceName)
	if err != nil || waited {
		t.Fatalf("waitCloudRunReconciled = (%v, %v), want (false, nil)", waited, err)
	}
	if len(fake.getReqs) != 1 {
		t.Errorf("GetInstance called %d times, want 1", len(fake.getReqs))
	}
}
