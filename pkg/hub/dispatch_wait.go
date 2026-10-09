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
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ErrDispatchFailed is returned when a lifecycle dispatch rolling timeout
// expires without receiving any status update within the window — the broker
// went silent and the operation is considered failed (design §6.4).
var ErrDispatchFailed = errors.New("dispatch failed: rolling timeout expired with no status update")

// dispatchRollingTimeout is the default rolling window for
// waitForAgentTransition. Each status event (phase/activity/detail change)
// resets this timer. If no event arrives within the window, the dispatch is
// considered failed. Single tunable per design §6.4.
const dispatchRollingTimeout = 90 * time.Second

// dispatchDeleteTimeout is a shorter rolling timeout for delete operations.
// Deletes are lightweight broker-side operations and should not block the
// caller for the full 90-second window — a 15-second silence is sufficient
// to conclude the broker is unreachable.
const dispatchDeleteTimeout = 15 * time.Second

// deleteWaitBudgetKey carries an explicit deferred-delete wait budget on a
// context (see withDeleteWaitBudget).
type deleteWaitBudgetKey struct{}

// withDeleteWaitBudget returns ctx carrying d as the wait budget for a
// cross-node (deferred) delete dispatched under it. Only the delete engine
// sets it, to the time remaining on its own dispatch budget, so the wait can
// never fire before that budget (design ptone/scion#2483 §2.3.1). Every
// other DispatchAgentDelete caller (project deletion, env-gather recreate,
// reconcile, create-failure cleanup) passes no budget and keeps
// dispatchDeleteTimeout, whatever its ctx deadline.
func withDeleteWaitBudget(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, deleteWaitBudgetKey{}, d)
}

// deleteWaitTimeoutFn computes the deferred-delete wait timeout for ctx: the
// budget set by withDeleteWaitBudget when present and positive, else
// dispatchDeleteTimeout. A package-level seam so tests can make the wait's
// timer fire before the engine's ctx.
var deleteWaitTimeoutFn = func(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(deleteWaitBudgetKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return dispatchDeleteTimeout
}

// Lifecycle wait timings. Package variables so tests can shorten them.
var (
	// lifecycleRollingTimeout is waitForLifecycleOutcome's rolling window:
	// any non-terminal agent status event resets it.
	lifecycleRollingTimeout = dispatchRollingTimeout
	// lifecycleRowPollInterval is how often waitForLifecycleOutcome re-reads
	// the dispatch row regardless of events. Agent status events keep
	// resetting the rolling window, so without this a missed done event
	// would go unnoticed until the caller's deadline.
	lifecycleRowPollInterval = 10 * time.Second
	// lifecycleErrorPhaseGrace is how long waitForLifecycleOutcome waits for
	// the dispatch row to fail after an error phase arrives first, so the
	// broker's typed error wins over the generic error-phase error.
	lifecycleErrorPhaseGrace = 5 * time.Second
)

// waitForLifecycleOutcome waits for the outcome of a cross-node start, stop
// or restart. The caller subscribes to BOTH agent.<id>.status and
// broker.dispatch.<dispatchID>.done before writing the dispatch row, and
// passes the subscription channel and unsubscribe function here.
//
// The dispatch row is authoritative for failure: whenever it reads failed,
// the wait returns dispatchFailureError, which carries the broker's typed
// error when the executing node recorded one. Success is still the op's
// success phase (running for start/restart, stopped for stop); a done row
// alone does not end the wait.
//
//   - success phase: return nil.
//   - error phase: return the row's failure if it is failed. While the row
//     is still pending or in_progress, wait up to lifecycleErrorPhaseGrace
//     for it to fail, then return the generic error-phase error. For any
//     other row state (done, or unreadable) return that error at once.
//   - done event: read the row; return its failure if it is failed. If an
//     error phase is waiting out its grace and the row is now finished
//     without failing, return the error-phase error. If the row is done and
//     its result reports that a delete won after the broker start landed
//     (LifecycleDispatchResult.DeleteWon), return nil: the success phase
//     will not come, and the caller answers from its own re-read
//     (ptone/scion#3456). Otherwise keep waiting (the executor's write may
//     have lost its CAS). The rolling window is not reset.
//   - other status events reset the rolling window.
//   - every lifecycleRowPollInterval the row is read as on a done event, and
//     when the rolling window expires it is read for its failure, in case
//     the done event was missed. Rolling window
//     expiry otherwise returns ErrDispatchFailed.
//   - ctx cancellation returns ctx.Err(), including when a row read in any
//     of the cases above fails because ctx ended.
func waitForLifecycleOutcome(
	ctx context.Context,
	events <-chan Event,
	unsub func(),
	st store.BrokerDispatchStore,
	dispatchID, op string,
	terminal func(phase string) bool,
) error {
	defer unsub()

	doneSubject := "broker.dispatch." + dispatchID + ".done"
	errorPhaseErr := fmt.Errorf("agent entered error phase during %s", op)

	// readRow returns the dispatch row's state and, when it is failed, its
	// failure. A row that cannot be read reports state "" (a later read
	// retries), unless the read failed because ctx ended: then ctx.Err() is
	// returned as the outcome, as the ctx.Done case would.
	//
	// deleteWon is set when the row is done and its result reports that a
	// delete won after the broker start landed (LifecycleDispatchResult).
	deleteWon := false
	readRow := func() (string, error) {
		d, err := st.GetBrokerDispatch(ctx, dispatchID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			return "", nil
		}
		if d.State != store.DispatchStateFailed {
			deleteWon = d.State == store.DispatchStateDone && decodeLifecycleResult(d.Result).DeleteWon
			return d.State, nil
		}
		return d.State, dispatchFailureError(d)
	}
	rowFailure := func() error {
		_, err := readRow()
		return err
	}
	// rowEnd is rowFailure for a closed event channel: done with the row's
	// failure, or
	// done with nil when the row is done and reports DeleteWon (see
	// rowOutcome); else not done.
	rowEnd := func() (done bool, err error) {
		if _, err := readRow(); err != nil {
			return true, err
		}
		return deleteWon, nil
	}

	timer := time.NewTimer(lifecycleRollingTimeout)
	defer timer.Stop()
	poll := time.NewTicker(lifecycleRowPollInterval)
	defer poll.Stop()
	var graceTimer *time.Timer
	var grace <-chan time.Time // set once an error phase has arrived
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()
	// rowOutcome re-reads the row on a done event or a poll: its failure if
	// it is failed, the error-phase error if an error phase is waiting out
	// its grace and the executor has since finished the row without failing
	// it, done=true with no error if the row is done and reports that a
	// delete won after the broker start landed (the success phase will not
	// come: the delete holds the row, and the caller answers from its own
	// re-read, ptone/scion#3456), else nil (keep waiting).
	rowOutcome := func() (done bool, err error) {
		rowState, err := readRow()
		if err != nil {
			return true, err
		}
		if grace != nil && rowState != "" && rowState != store.DispatchStatePending && rowState != store.DispatchStateInProgress {
			return true, errorPhaseErr
		}
		return deleteWon, nil
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if done, err := rowEnd(); done {
					return err
				}
				return ErrDispatchFailed
			}
			if ev.Subject == doneSubject {
				if done, err := rowOutcome(); done {
					return err
				}
				continue
			}
			var status AgentStatusEvent
			if err := json.Unmarshal(ev.Data, &status); err != nil {
				continue
			}
			if !terminal(status.Phase) {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(lifecycleRollingTimeout)
				continue
			}
			if status.Phase != "error" {
				return nil
			}
			rowState, err := readRow()
			if err != nil {
				return err
			}
			if rowState != store.DispatchStatePending && rowState != store.DispatchStateInProgress {
				// The executor has finished without failing the row (the
				// broker accepted the op), or the row cannot be read: the
				// error phase is the outcome.
				return errorPhaseErr
			}
			if grace == nil {
				graceTimer = time.NewTimer(lifecycleErrorPhaseGrace)
				grace = graceTimer.C
			}

		case <-grace:
			if err := rowFailure(); err != nil {
				return err
			}
			return errorPhaseErr

		case <-poll.C:
			if done, err := rowOutcome(); done {
				return err
			}

		case <-timer.C:
			// rowOutcome, as the poll does: an error phase waiting out its
			// grace keeps the error-phase answer, else a done row
			// reporting DeleteWon ends with nil.
			if done, err := rowOutcome(); done {
				return err
			}
			if grace != nil {
				return errorPhaseErr
			}
			return ErrDispatchFailed

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waitForDispatchDone waits for a broker_dispatch row to reach terminal state.
// The caller subscribes to broker.dispatch.<id>.done BEFORE writing intent and
// passes the channel + unsub here. On event arrival (or timeout), the row is
// read from the store — the DB row is authoritative (design §6.3), so a missed
// event is recoverable.
//
// timeoutOverride, if non-zero, replaces the default dispatchRollingTimeout.
func waitForDispatchDone(
	ctx context.Context,
	events <-chan Event,
	unsub func(),
	st store.BrokerDispatchStore,
	dispatchID string,
	timeoutOverride ...time.Duration,
) (*store.BrokerDispatch, error) {
	defer unsub()

	timeout := dispatchRollingTimeout
	if len(timeoutOverride) > 0 && timeoutOverride[0] > 0 {
		timeout = timeoutOverride[0]
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case _, ok := <-events:
			if !ok {
				return nil, ErrDispatchFailed
			}
			d, err := st.GetBrokerDispatch(ctx, dispatchID)
			if err != nil {
				return nil, fmt.Errorf("read dispatch result: %w", err)
			}
			if d.State == store.DispatchStateDone || d.State == store.DispatchStateFailed {
				return d, nil
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)

		case <-timer.C:
			// Bounded re-read: the event may have been missed (design §6.3).
			d, err := st.GetBrokerDispatch(ctx, dispatchID)
			if err != nil {
				return nil, fmt.Errorf("read dispatch result on timeout: %w", err)
			}
			if d.State == store.DispatchStateDone || d.State == store.DispatchStateFailed {
				return d, nil
			}
			return nil, ErrDispatchFailed

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
