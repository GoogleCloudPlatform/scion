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

//go:build !no_sqlite

package hub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errStopNeverCut is returned by autoSuspendBlockingStopDispatcher when its ctx was
// not done in time: the dispatch had no bound.
var errStopNeverCut = errors.New("stop dispatch ctx never done")

// autoSuspendBlockingStopDispatcher's stop dispatch waits for its ctx to end and
// reports the ctx error and whether the ctx had a deadline.
type autoSuspendBlockingStopDispatcher struct {
	createAgentDispatcher
	stopErr     chan error
	hadDeadline chan bool
}

func (d *autoSuspendBlockingStopDispatcher) DispatchAgentStop(ctx context.Context, _ *store.Agent) error {
	_, ok := ctx.Deadline()
	d.hadDeadline <- ok
	select {
	case <-ctx.Done():
		d.stopErr <- ctx.Err()
		return ctx.Err()
	case <-time.After(10 * time.Second):
		d.stopErr <- errStopNeverCut
		return errStopNeverCut
	}
}

// ptone/scion#4247: the auto-suspend scheduler's stop dispatch runs under
// syncDispatch, as single stop and suspend do, so it is cut at its own
// bound even when the scheduler's ctx has none. A dispatch cut by the
// bound is handled as any failed dispatch: the running intent is put back.
func TestAutoSuspend_StopDispatchHonoursSyncDispatchTimeout(t *testing.T) {
	const bound = 200 * time.Millisecond
	shortenSyncDispatchTimeout(t, bound)
	ctx := context.Background() // no deadline: only syncDispatch bounds the dispatch.
	srv, s := testServer(t)
	disp := &autoSuspendBlockingStopDispatcher{stopErr: make(chan error, 1), hadDeadline: make(chan bool, 1)}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "as-sync-dispatch")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	markAgentStalled(t, s, agent.ID)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	start := time.Now()
	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})
	elapsed := time.Since(start)

	select {
	case ok := <-disp.hadDeadline:
		assert.True(t, ok, "the stop dispatch ctx carries the syncDispatch deadline")
	default:
		t.Fatal("fixture check: the stop was never dispatched")
	}
	require.ErrorIs(t, <-disp.stopErr, context.DeadlineExceeded, "the dispatch is cut at its own bound")
	assert.Less(t, elapsed, 5*time.Second, "auto-suspend returns soon after the dispatch bound")
	got := requireRunIntent(t, s, agent.ID, store.RunIntentRunning)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "a cut dispatch records no suspension")
}
