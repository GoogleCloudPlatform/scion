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

// waitForAgentTransition waits for an agent's phase to reach a terminal state,
// using a rolling timeout that resets on ANY AgentStatusEvent (phase, activity,
// or detail change). The caller must subscribe to the agent's status events
// BEFORE writing the durable intent, and pass the subscription channel +
// unsubscribe function here.
//
// Parameters:
//   - events: the subscription channel from EventPublisher.Subscribe("agent.<id>.status")
//   - unsub:  the unsubscribe function returned by Subscribe (called on return)
//   - terminal: returns true when the agent's phase indicates the op is done
//     (e.g. "running" or "error" for start; "stopped" or "error" for stop)
//
// Returns the terminal phase on success, or ErrDispatchFailed on rolling
// timeout, or ctx.Err() on context cancellation.
func waitForAgentTransition(
	ctx context.Context,
	events <-chan Event,
	unsub func(),
	terminal func(phase string) bool,
) (string, error) {
	defer unsub()

	timeout := dispatchRollingTimeout
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return "", ErrDispatchFailed
			}
			var status AgentStatusEvent
			if err := json.Unmarshal(ev.Data, &status); err != nil {
				continue
			}
			if terminal(status.Phase) {
				return status.Phase, nil
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)

		case <-timer.C:
			return "", ErrDispatchFailed

		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// waitForDispatchDone waits for a broker_dispatch row to reach terminal state.
// The caller subscribes to broker.dispatch.<id>.done BEFORE writing intent and
// passes the channel + unsub here. On event arrival (or timeout), the row is
// read from the store — the DB row is authoritative (design §6.3), so a missed
// event is recoverable.
func waitForDispatchDone(
	ctx context.Context,
	events <-chan Event,
	unsub func(),
	st store.BrokerDispatchStore,
	dispatchID string,
) (*store.BrokerDispatch, error) {
	defer unsub()

	timeout := dispatchRollingTimeout
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
