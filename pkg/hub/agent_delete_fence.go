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
// A cross-node (deferred) delete carries the engine's claim instead, in
// DeleteDispatchArgs. The executing node re-reads the row, drops the intent
// unless that claim is still the current, live one, and computes notAfter
// from the row's lease when it actually sends.

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

// deleteNotAfter is the deadline for a delete sent at now under a lease
// that expires at leaseUntil: the earlier of the lease expiry and the end of
// the dispatch budget.
func deleteNotAfter(now, leaseUntil time.Time) time.Time {
	budgetEnd := now.Add(deleteDispatchBudget)
	if !leaseUntil.IsZero() && leaseUntil.Before(budgetEnd) {
		return leaseUntil
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

// deleteClaimLive reports whether row is still held by delete claim at now:
// the same claim, still deleting, not soft-deleted, and its lease not yet
// expired.
func deleteClaimLive(row *store.Agent, claim int64, now time.Time) bool {
	return row.DeletedAt.IsZero() &&
		row.DeletionClaim == claim &&
		row.DeletionState == store.DeletionStateDeleting &&
		row.DeletionLeaseAt != nil && row.DeletionLeaseAt.After(now)
}
