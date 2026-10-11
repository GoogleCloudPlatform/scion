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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitAutoSuspendIdle waits until the auto-suspend worker has processed
// everything handed to it.
func (s *Server) waitAutoSuspendIdle() {
	s.autoSuspend.wg.Wait()
}

// autoSuspendQueueState returns the IDs of the agents waiting for the
// auto-suspend worker and the size of its queued set.
func autoSuspendQueueState(s *Server) (pending []string, queued int) {
	w := &s.autoSuspend
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.pending {
		pending = append(pending, w.pending[i].ID)
	}
	return pending, len(w.queued)
}

// requireAutoSuspendWorkerIdle checks that the worker has stopped running
// and holds no agent.
func requireAutoSuspendWorkerIdle(t *testing.T, s *Server) {
	t.Helper()
	pending, queued := autoSuspendQueueState(s)
	require.Empty(t, pending, "nothing is pending")
	require.Zero(t, queued, "the queued set is empty")
	s.autoSuspend.mu.Lock()
	running := s.autoSuspend.running
	s.autoSuspend.mu.Unlock()
	require.False(t, running, "the worker is not running")
}

// setAutoSuspendWorkerExitHook sets autoSuspendWorkerExitHook for one test.
func setAutoSuspendWorkerExitHook(t *testing.T, hook func()) {
	t.Helper()
	prev := autoSuspendWorkerExitHook
	autoSuspendWorkerExitHook = hook
	t.Cleanup(func() { autoSuspendWorkerExitHook = prev })
}

// ptone/scion#4387 round 2 (R-1): a panic in an auto-suspend batch is
// recovered on the worker goroutine. The hub keeps running, the batch's
// agents leave the queued set, the agent's lifecycle op is ended, and a
// later hand-off is still processed.
func TestAutoSuspend_WorkerRecoversFromPanic(t *testing.T) {
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	disp.panicFirst = true
	close(disp.release)
	srv.SetDispatcher(disp)
	a := newStalledBrokerAgent(t, s, "as-panic-a")
	b := newStalledBrokerAgent(t, s, "as-panic-b")

	srv.enqueueAutoSuspend([]store.Agent{*a})
	srv.waitAutoSuspendIdle()
	requireAutoSuspendWorkerIdle(t, srv)
	assert.False(t, srv.lifecycleOps.active(a.ID), "the panicked auto-suspend's lifecycle op is ended")

	srv.enqueueAutoSuspend([]store.Agent{*b})
	srv.waitAutoSuspendIdle()
	requireAutoSuspendWorkerIdle(t, srv)

	assert.Equal(t, []string{a.ID, b.ID}, disp.stopped(), "the later hand-off is processed")
	got := requireRunIntent(t, s, b.ID, store.RunIntentStopped)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
}

// ptone/scion#4387 round 2 (O-3): a hand-off that arrives just as the worker
// exits is still processed. The exit hook hands an agent off right after
// the worker has decided to exit and released the lock: the worker must
// have cleared running by then (under the lock), so the hand-off starts a
// new worker rather than leaving the agent queued with no worker.
func TestAutoSuspend_WorkerHandOffAtExitIsProcessed(t *testing.T) {
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	close(disp.release)
	srv.SetDispatcher(disp)
	a := newStalledBrokerAgent(t, s, "as-exit-a")
	b := newStalledBrokerAgent(t, s, "as-exit-b")

	var once sync.Once
	handedOff := false
	setAutoSuspendWorkerExitHook(t, func() {
		once.Do(func() {
			handedOff = true
			srv.enqueueAutoSuspend([]store.Agent{*b})
		})
	})

	srv.enqueueAutoSuspend([]store.Agent{*a})
	srv.waitAutoSuspendIdle()

	require.True(t, handedOff, "fixture check: the hand-off ran at worker exit")
	assert.Equal(t, []string{a.ID, b.ID}, disp.stopped(), "the hand-off at exit is processed")
	requireAutoSuspendWorkerIdle(t, srv)
	got := requireRunIntent(t, s, b.ID, store.RunIntentStopped)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
}

// ptone/scion#4387 round 2 (O-3): a batch's agents leave the queued set
// when the batch ends, so a later tick can hand the same agent off again.
// The first stop fails, so the agent stays running and stalled and the
// second hand-off dispatches it again.
func TestAutoSuspend_WorkerClearsQueuedAfterBatch(t *testing.T) {
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	disp.failFirst = true
	close(disp.release)
	srv.SetDispatcher(disp)
	a := newStalledBrokerAgent(t, s, "as-requeue")

	srv.enqueueAutoSuspend([]store.Agent{*a})
	srv.waitAutoSuspendIdle()
	requireAutoSuspendWorkerIdle(t, srv)

	srv.enqueueAutoSuspend([]store.Agent{*a})
	srv.waitAutoSuspendIdle()

	assert.Equal(t, []string{a.ID, a.ID}, disp.stopped(), "the second hand-off is processed")
	got := requireRunIntent(t, s, a.ID, store.RunIntentStopped)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
}
