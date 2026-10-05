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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Delete dispatch fencing (ptone/scion#2906).
//
// A delete the engine sends can reach the broker after the engine's claim
// has lapsed (queued in the network, a slow broker, a frozen hub that
// resumed). By then the row reads failed/abandoned and the user may have
// started the agent again, and run_id cannot tell a same-run start (one that
// adopted the surviving run) from the run the delete was for. So the engine
// sends a deadline, notAfter = min(its lease expiry, now + its dispatch
// budget), and the broker refuses a delete that arrives after it with 409
// stale_dispatch and no side effects. notAfter is a wire field only: nothing
// stores it. A delete from any other caller carries none, and the broker
// then does not check.
//
// The margin points toward refusal: the hub allows a start as soon as the
// lease expires, and the broker accepts up to deleteNotAfterMargin past
// notAfter (its own skew allowance), so the hub subtracts the same margin
// from the lease. With synchronised clocks the broker's last accepted
// instant is then the lease expiry. A broker clock further ahead of the
// hub's refuses in-time deletes (safe: the hub retries); one further behind
// accepts late deletes, past the lease expiry by the skew beyond the margin.
//
// A cross-node (deferred) delete carries the engine's claim instead, in
// DeleteDispatchArgs. The executing node re-reads the row, drops the intent
// unless that claim is still the current one and the intent may still run
// (see deferredDeleteDeadline), and computes notAfter when it actually
// sends. The dispatch table already blocks a start while a delete intent is
// outstanding (deleteBlocksStart), so the re-check mostly drops superseded
// or abandoned intents; the deadline it adds closes the remaining gap, where
// the intent fails on a timeout while the broker is still working.

// errStaleDeleteDispatch is the error for a delete that was not acted on
// because it was stale: the broker answered 409 stale_dispatch, or a
// deferred intent was dropped because its claim was no longer live.
var errStaleDeleteDispatch = errors.New(staleDeleteDispatchPrefix + " delete dispatch was stale; nothing was done")

// staleDeleteDispatchPrefix starts the error text of a stale delete, so a
// deferred delete's failure, which reaches the originating node only as the
// dispatch row's error text, is still recognised there.
const staleDeleteDispatchPrefix = "stale_dispatch:"

// brokerCodeStaleDispatch is the broker's error code for a delete refused
// because it arrived after its notAfter.
const brokerCodeStaleDispatch = "stale_dispatch"

// deleteDispatchFence is what the engine attaches to its dispatch context.
type deleteDispatchFence struct {
	claim    int64
	notAfter time.Time
}

type deleteDispatchFenceKey struct{}

// withDeleteDispatchFence returns ctx carrying the engine's fence. It is
// carried on the context, as the wait budget and the project path are, so
// the AgentDispatcher interface and its other callers stay unchanged.
func withDeleteDispatchFence(ctx context.Context, f deleteDispatchFence) context.Context {
	return context.WithValue(ctx, deleteDispatchFenceKey{}, f)
}

func deleteDispatchFenceFrom(ctx context.Context) (deleteDispatchFence, bool) {
	f, ok := ctx.Value(deleteDispatchFenceKey{}).(deleteDispatchFence)
	return f, ok
}

// deleteNotAfterMargin is subtracted from the lease expiry when computing
// notAfter. It equals the broker's skew allowance (deleteNotAfterSkew in
// pkg/runtimebroker), so a broker with a synchronised clock stops accepting
// the delete no later than the lease expiry.
const deleteNotAfterMargin = 5 * time.Second

// deleteNotAfter is the deadline for a delete sent at now under a lease
// that expires at leaseUntil: the earlier of the lease expiry less
// deleteNotAfterMargin and the end of the dispatch budget.
func deleteNotAfter(now, leaseUntil time.Time) time.Time {
	budgetEnd := now.Add(deleteDispatchBudget)
	if leaseUntil.IsZero() {
		return budgetEnd
	}
	if bound := leaseUntil.Add(-deleteNotAfterMargin); bound.Before(budgetEnd) {
		return bound
	}
	return budgetEnd
}

// isStaleDeleteDispatch reports whether err is a delete that was refused as
// stale, directly (the broker's 409 stale_dispatch) or across nodes.
func isStaleDeleteDispatch(err error) bool {
	if errors.Is(err, errStaleDeleteDispatch) {
		return true
	}
	var se *brokerStatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusConflict {
		return false
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal([]byte(se.Body), &body) == nil && body.Error.Code == brokerCodeStaleDispatch
}

// staleDeleteDispatchFromText maps a failed deferred delete's error text
// back to errStaleDeleteDispatch.
func staleDeleteDispatchFromText(text string) bool {
	return strings.Contains(text, staleDeleteDispatchPrefix) || strings.Contains(text, `"code":"`+brokerCodeStaleDispatch+`"`)
}

// deferredDeleteDeadline decides whether a deferred delete intent created
// under claim may still be sent, re-reading row at now, and returns the
// notAfter to send it with. ok is false when the intent must be dropped.
//
//   - claim still live (same claim, deleting, lease not expired): notAfter =
//     min(lease - margin, now + budget), as the engine computes it;
//   - claim failed in_doubt (the engine's wait ended with this intent still
//     outstanding): the intent still runs, as design ptone/scion#2483
//     §2.3.1 and its follow-up 3 expect ("teardown may still complete on
//     the broker"). The outstanding intent blocks start, so there is no
//     lease to bound it: notAfter = min(now + budget, ctx's deadline) less
//     deleteNotAfterMargin, which covers the intent failing on a timeout
//     while the broker still works (the margin offsets the broker's skew
//     allowance, as for the lease bound);
//   - anything else (a newer claim, a lapsed lease, any other failure,
//     finalizing, soft-deleted): dropped.
func deferredDeleteDeadline(ctx context.Context, row *store.Agent, claim int64, now time.Time) (time.Time, bool) {
	if !row.DeletedAt.IsZero() || row.DeletionClaim != claim {
		return time.Time{}, false
	}
	switch {
	case row.DeletionState == store.DeletionStateDeleting &&
		row.DeletionLeaseAt != nil && row.DeletionLeaseAt.After(now):
		return deleteNotAfter(now, *row.DeletionLeaseAt), true
	case row.DeletionState == store.DeletionStateFailed && row.DeletionCode == store.DeletionCodeInDoubt:
		end := now.Add(deleteDispatchBudget)
		if dl, ok := ctx.Deadline(); ok && dl.Before(end) {
			end = dl
		}
		return end.Add(-deleteNotAfterMargin), true
	}
	return time.Time{}, false
}
