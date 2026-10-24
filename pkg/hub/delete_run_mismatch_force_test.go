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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#5016: when the broker refuses a run-scoped delete
// because it holds a different run of the agent (ptone/scion#3080), the
// delete engine deletes that reported run once, when the delete is forced
// or the reported run is one of the row's previous runs, and then
// finalizes. A second refusal is never retried, and a failure of the
// retried delete never finalizes.

// answerHeld answers a delete of the row's run run-a with the refusal
// naming held, and any other run with then(runID).
func answerHeld(t *testing.T, held string, then func(runID string) error) func(string) error {
	return func(runID string) error {
		if runID == "run-a" {
			return runMismatchEnvelope(t, runID, held, true)
		}
		return then(runID)
	}
}

func answerOK(string) error { return nil }

// requireKept checks a failed delete: status, deletion code, the row kept
// and not soft-deleted, the claim failed with code, and the prior phase
// restored.
func requireKept(t *testing.T, s store.Store, agentID string, r deleteResult, status int, code string, wantPhase state.Phase) {
	t.Helper()
	require.Equal(t, status, r.rec.Code, r.rec.Body.String())
	_, details := errorBody(t, r.rec)
	assert.Equal(t, code, details["deletionCode"])
	got := mustGetAgent(t, s, agentID)
	assert.True(t, got.DeletedAt.IsZero(), "the row is not soft-deleted")
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.Equal(t, code, got.DeletionCode)
	assert.Equal(t, string(wantPhase), got.Phase, "the prior phase is restored")
}

// A force delete refused for run-a because the broker holds run-b
// deletes run-b, then run-a as a previous run, and finalizes.
func TestForceDelete_RunMismatch_DeletesReportedRun(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-retry", state.PhaseRunning)
	f.client.answer = answerHeld(t, "run-b", answerOK)
	r := f.del(t, "?force=true")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID), "a force delete is hard")
	assert.Equal(t, []string{"run-a", "run-b", "run-a"}, f.client.deleted(),
		"the recorded run, then the reported run, then the recorded run as a previous run")
}

// The row's previous runs are sent after the recorded run, newest
// first, and the reported run is not sent twice.
func TestForceDelete_RunMismatch_PreviousRunsIncluded(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-prev", state.PhaseRunning, "run-p")
	f.client.answer = answerHeld(t, "run-b", answerOK)
	r := f.del(t, "?force=true")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
	assert.Equal(t, []string{"run-a", "run-b", "run-a", "run-p"}, f.client.deleted())
}

// The retried delete is refused because yet another run holds the
// agent now: conflict, the row kept, and no third send.
func TestForceDelete_RunMismatch_SecondMismatchRefuses(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-second", state.PhaseRunning)
	f.client.answer = answerHeld(t, "run-b", func(runID string) error {
		return runMismatchEnvelope(t, runID, "run-c", true)
	})
	r := f.del(t, "?force=true")
	requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, state.PhaseRunning)
	sent := f.client.deleted()
	require.Len(t, sent, 2, "exactly one retry: no third send (%v)", sent)
	assert.Equal(t, []string{"run-a", "run-b"}, sent)
	body := r.rec.Body.String()
	assert.Contains(t, body, "held run run-b of this agent, not run run-a")
	assert.Contains(t, body, "now holds run run-c")
	assert.NotContains(t, body, "Force delete", "a forced delete's refusal does not suggest force")

	// The next delete starts over from the row's run.
	f.client.answer = answerOK
	r = f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
}

// The retried delete is refused naming the recorded run again: no
// third send either.
func TestForceDelete_RunMismatch_SecondMismatchBackToRecorded(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-back", state.PhaseRunning)
	f.client.answer = answerHeld(t, "run-b", func(runID string) error {
		return runMismatchEnvelope(t, runID, "run-a", true)
	})
	r := f.del(t, "?force=true")
	requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, state.PhaseRunning)
	sent := f.client.deleted()
	require.Len(t, sent, 2, "exactly one retry: no third send (%v)", sent)
	assert.Equal(t, []string{"run-a", "run-b"}, sent)
}

// Any other failure of the retried delete is classified like a
// non-force dispatch failure: never the force finalize.
func TestForceDelete_RunMismatch_RetryFailureNotFinalized(t *testing.T) {
	for i, tc := range []struct {
		name   string
		err    func(t *testing.T) error
		status int
		code   string
	}{
		{"runtime error", func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", nil)
		}, http.StatusBadGateway, store.DeletionCodeRuntimeError},
		{"runtime unavailable", func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable, nil)
		}, http.StatusServiceUnavailable, store.DeletionCodeRuntimeUnavailable},
		{"broker conflict", func(t *testing.T) error {
			return brokerEnvelope(t, http.StatusConflict, "conflict", nil)
		}, http.StatusConflict, store.DeletionCodeConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunMismatchFixture(t, fmt.Sprintf("rmf-fail-%d", i), state.PhaseRunning)
			f.client.answer = answerHeld(t, "run-b", func(string) error { return tc.err(t) })
			r := f.del(t, "?force=true")
			requireKept(t, f.store, f.agent.ID, r, tc.status, tc.code, state.PhaseRunning)
			assert.Equal(t, []string{"run-a", "run-b"}, f.client.deleted())
		})
	}
}

// A retried delete refused as stale abandons the claim, as the
// first dispatch's would.
func TestForceDelete_RunMismatch_RetryStaleAbandons(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-stale", state.PhaseRunning)
	f.client.answer = answerHeld(t, "run-b", func(string) error { return staleDispatchErr() })
	r := f.del(t, "?force=true")
	require.NotEqual(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	_, details := errorBody(t, r.rec)
	assert.Equal(t, store.DeletionCodeAbandoned, details["deletionCode"])
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.True(t, got.DeletedAt.IsZero(), "the row is not soft-deleted")
	view := store.ComputeAgentDeletion(got, time.Now())
	require.NotNil(t, view)
	assert.Equal(t, store.DeletionCodeAbandoned, view.Code)
	assert.Equal(t, []string{"run-a", "run-b"}, f.client.deleted())
}

// A reported run that is not a valid run ID is never sent, even
// with force, and the refusal does not suggest force.
func TestForceDelete_RunMismatch_InvalidReportedRunRefuses(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-invalid", state.PhaseRunning)
	f.client.answer = answerHeld(t, "bad,run", answerOK)
	r := f.del(t, "?force=true")
	requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, state.PhaseRunning)
	assert.Equal(t, []string{"run-a"}, f.client.deleted())
	assert.NotContains(t, r.rec.Body.String(), "Force delete")
}

// A plain delete whose reported run is not one of the row's
// previous runs is still refused, sends only the recorded run, and its
// message says that force delete removes the reported run; so does a
// created row's best-effort delete.
func TestPlainDelete_RunMismatch_RefusedSuggestsForce(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseRunning, state.PhaseCreated} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRunMismatchFixture(t, "rmf-plain-"+string(phase), phase, "run-p")
			f.client.answer = answerHeld(t, "run-b", answerOK)
			r := f.del(t, "")
			requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, phase)
			assert.Equal(t, []string{"run-a"}, f.client.deleted())
			assert.Contains(t, r.rec.Body.String(), "Force delete removes run run-b that the broker holds, then the agent")
		})
	}
}

// A created row's best-effort delete with force deletes the
// reported run too.
func TestForceDelete_RunMismatch_BestEffortRetries(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-created", state.PhaseCreated)
	f.client.answer = answerHeld(t, "run-b", answerOK)
	r := f.del(t, "?force=true")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
	assert.Equal(t, []string{"run-a", "run-b", "run-a"}, f.client.deleted())
}

// Across nodes, the retried delete is a second intent pinned to
// the reported run, with the recorded run among its previous runs.
func TestForceDelete_RunMismatch_DeferredRetry(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 10 * time.Second })
	f := newDeferredDeleteFixture(t, "rmf-deferred", nil)
	for _, run := range []string{"run-p", "run-a"} {
		_, err := f.store.SetAgentRunID(context.Background(), f.agent.ID, run, nil)
		require.NoError(t, err)
	}

	refusal := fmt.Errorf("dispatch delete: %w", deleteAgentError(runMismatchEnvelope(t, "run-a", "run-b", true), "run-a"))
	var (
		mu      sync.Mutex
		intents []*DeleteDispatchArgs
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := map[string]bool{}
		assert.Eventually(t, func() bool {
			all, _ := f.store.ListPendingDispatch(context.Background(), f.agent.RuntimeBrokerID)
			for _, d := range all {
				if d.AgentID != f.agent.ID || d.Op != brokerDispatchOpDelete || seen[d.ID] {
					continue
				}
				seen[d.ID] = true
				args, err := UnmarshalDeleteArgs(d.Args)
				if !assert.NoError(t, err) {
					return true
				}
				mu.Lock()
				intents = append(intents, args)
				n := len(intents)
				mu.Unlock()
				if ok, _ := f.store.ClaimBrokerDispatch(context.Background(), d.ID, "test-owner"); ok {
					if n == 1 {
						_ = f.store.FailBrokerDispatch(context.Background(), d.ID, refusal.Error(), dispatchFailureResult(refusal))
					} else {
						_ = f.store.CompleteBrokerDispatch(context.Background(), d.ID, "")
					}
				}
				f.bus.PublishDispatchDone(context.Background(), d.ID)
				if n == 2 {
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "the retried delete intent was never written")
	}()

	r := f.del(t, "?force=true")
	<-done
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, f.store, f.agent.ID))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, intents, 2)
	assert.Equal(t, "run-a", intents[0].RunID)
	assert.Equal(t, "run-b", intents[1].RunID, "the retry is pinned to the reported run")
	assert.Equal(t, []string{"run-p", "run-a"}, intents[1].PreviousRunIDs,
		"the row's previous runs in order, then the recorded run")
}

// A plain delete refused because the broker holds one of the row's
// previous runs deletes that run, then the recorded run, and soft-deletes
// the row recording the run it deleted, with the recorded run among the
// previous runs.
func TestPlainDelete_RunMismatch_PreviousRun_SoftRecordsRun(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-soft", state.PhaseRunning, "run-p")
	f.srv.config.SoftDeleteRetention = time.Hour
	f.client.answer = answerHeld(t, "run-p", answerOK)
	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.Equal(t, []string{"run-a", "run-p", "run-a"}, f.client.deleted())
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.False(t, got.DeletedAt.IsZero(), "soft-deleted")
	assert.Equal(t, "run-p", got.RunID, "the row records the run the delete removed")
	assert.Equal(t, []string{"run-a"}, got.PreviousRunIDs, "the recorded run is a previous run")
}

// A soft delete that needed no retry keeps the row's runs.
func TestPlainDelete_NoMismatch_SoftKeepsRuns(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-soft-plain", state.PhaseRunning, "run-p")
	f.srv.config.SoftDeleteRetention = time.Hour
	r := f.del(t, "")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.False(t, got.DeletedAt.IsZero(), "soft-deleted")
	assert.Equal(t, "run-a", got.RunID)
	assert.Equal(t, []string{"run-p"}, got.PreviousRunIDs)
}

// A plain delete whose reported run is one of the row's previous runs:
// the retry and its guards.
func TestPlainDelete_RunMismatch_PreviousRun(t *testing.T) {
	t.Run("member: deleted and finalized", func(t *testing.T) {
		f := newRunMismatchFixture(t, "rmf-prev-ok", state.PhaseRunning, "run-p")
		f.client.answer = answerHeld(t, "run-p", answerOK)
		r := f.del(t, "")
		require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
		assert.True(t, agentGone(t, f.store, f.agent.ID))
		assert.Equal(t, []string{"run-a", "run-p", "run-a"}, f.client.deleted())
	})
	t.Run("not a member: refused", func(t *testing.T) {
		f := newRunMismatchFixture(t, "rmf-prev-no", state.PhaseRunning, "run-p")
		f.client.answer = answerHeld(t, "run-b", answerOK)
		r := f.del(t, "")
		requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, state.PhaseRunning)
		assert.Equal(t, []string{"run-a"}, f.client.deleted())
	})
	t.Run("second mismatch: refused, no loop", func(t *testing.T) {
		f := newRunMismatchFixture(t, "rmf-prev-second", state.PhaseRunning, "run-p")
		f.client.answer = answerHeld(t, "run-p", func(runID string) error {
			return runMismatchEnvelope(t, runID, "run-c", true)
		})
		r := f.del(t, "")
		requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, state.PhaseRunning)
		sent := f.client.deleted()
		require.Len(t, sent, 2, "exactly one retry: no third send (%v)", sent)
		assert.Equal(t, []string{"run-a", "run-p"}, sent)
		body := r.rec.Body.String()
		assert.Contains(t, body, "now holds run run-c")
		assert.Contains(t, body, "Retry the delete")
		assert.NotContains(t, body, "Force delete", "the hint is not given after the retry already ran")
	})
	t.Run("invalid member: refused", func(t *testing.T) {
		f := newRunMismatchFixture(t, "rmf-prev-invalid", state.PhaseRunning, "bad,run")
		f.client.answer = answerHeld(t, "bad,run", answerOK)
		r := f.del(t, "")
		requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, state.PhaseRunning)
		assert.Equal(t, []string{"run-a"}, f.client.deleted())
		assert.NotContains(t, r.rec.Body.String(), "Force delete")
	})
}

func TestRetryReportedRun(t *testing.T) {
	snap := &store.Agent{RunID: "run-a", PreviousRunIDs: []string{"run-p", "bad,run"}}
	for _, tc := range []struct {
		name    string
		force   bool
		current string
		retry   bool
		why     string
	}{
		{"force, valid", true, "run-b", true, retryReportedRunForce},
		{"force, invalid", true, "bad,run", false, ""},
		{"force, empty", true, "", false, ""},
		{"force, same as requested", true, "run-a", false, ""},
		{"plain, member", false, "run-p", true, retryReportedRunPrevious},
		{"plain, not a member", false, "run-b", false, ""},
		{"plain, invalid member", false, "bad,run", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, why := retryReportedRun(tc.force, snap, &DeleteRunMismatchError{RequestedRunID: "run-a", CurrentRunID: tc.current})
			assert.Equal(t, tc.retry, retry)
			assert.Equal(t, tc.why, why)
		})
	}
}

func TestEngineRunMismatchMessage(t *testing.T) {
	valid := &DeleteRunMismatchError{RequestedRunID: "run-a", CurrentRunID: "run-b"}
	invalid := &DeleteRunMismatchError{RequestedRunID: "run-a", CurrentRunID: "bad,run"}
	assert.Contains(t, engineRunMismatchMessage(valid, false), "Force delete removes run run-b")
	assert.NotContains(t, engineRunMismatchMessage(valid, true), "Force delete")
	assert.NotContains(t, engineRunMismatchMessage(invalid, false), "Force delete")
	assert.Contains(t, createCleanupRefusedMessage(valid), "Delete the agent to retry; force delete removes run run-b")
	assert.Equal(t, createCleanupRefusedPrefix+deleteRunMismatchMessage(invalid)+". Delete the agent to retry.",
		createCleanupRefusedMessage(invalid))
}

// A force retry with the previous-run list at store.MaxPreviousRunIDs
// sends every previous run, newest first, after the reported run and the
// recorded run: the retry's list is not capped.
func TestForceDelete_RunMismatch_PreviousRunsAtCap(t *testing.T) {
	prev := make([]string, store.MaxPreviousRunIDs)
	for i := range prev {
		prev[i] = fmt.Sprintf("run-p%d", i+1)
	}
	f := newRunMismatchFixture(t, "rmf-cap", state.PhaseRunning, prev...)
	f.client.answer = answerHeld(t, "run-b", answerOK)
	r := f.del(t, "?force=true")
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	want := []string{"run-a", "run-b", "run-a"}
	for i := len(prev) - 1; i >= 0; i-- {
		want = append(want, prev[i])
	}
	assert.Equal(t, want, f.client.deleted(), "every previous run is sent, the oldest included")
}

func TestRetryPreviousRunIDs(t *testing.T) {
	full := make([]string, store.MaxPreviousRunIDs)
	for i := range full {
		full[i] = fmt.Sprintf("p%d", i)
	}
	assert.Equal(t, append(append([]string{}, full...), "req"), retryPreviousRunIDs(full, "req", "cur"), "not capped")
	assert.Equal(t, []string{"p1", "req"}, retryPreviousRunIDs([]string{"p1", "cur"}, "req", "cur"), "the reported run is dropped")
	assert.Equal(t, []string{"req", "p1"}, retryPreviousRunIDs([]string{"req", "p1", ""}, "req", "cur"), "each run once, no empty run")
	assert.Equal(t, []string{"p1"}, retryPreviousRunIDs([]string{"p1"}, "", "cur"))
	in := []string{"p1", "cur"}
	_ = retryPreviousRunIDs(in, "req", "cur")
	assert.Equal(t, []string{"p1", "cur"}, in, "the snapshot's list is not modified")
}

// A placement refusal of the retried delete is a conflict with the
// refusal's message, not a classified runtime error.
func TestForceDelete_RunMismatch_RetryPlacementRefusal(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-placement", state.PhaseRunning)
	refusal := &RuntimeTargetRefusal{Code: ErrCodeRuntimeTargetMismatch, Status: http.StatusConflict, Message: "placement refusal for the test"}
	f.client.answer = answerHeld(t, "run-b", func(string) error { return refusal })
	r := f.del(t, "?force=true")
	requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, state.PhaseRunning)
	assert.Contains(t, r.rec.Body.String(), "placement refusal for the test")
	assert.Equal(t, []string{"run-a", "run-b"}, f.client.deleted())
}

// A claim lost while the retried delete is in flight ends the engine as
// lost, without classifying the retry's error.
func TestForceDelete_RunMismatch_RetryLostClaim(t *testing.T) {
	setDeleteKnob(t, &deleteLeaseRenewInterval, 20*time.Millisecond)
	setDeleteKnob(t, &deleteSyncWait, 2*time.Second)
	f := newRunMismatchFixture(t, "rmf-lost", state.PhaseRunning)
	var logs bytes.Buffer
	var logMu sync.Mutex
	f.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&syncedLogWriter{w: &logs, mu: &logMu}, nil))
	var stolen atomic.Bool
	f.client.answerCtx = func(ctx context.Context, runID string) error {
		if runID == "run-a" {
			return runMismatchEnvelope(t, runID, "run-b", true)
		}
		// Another delete takes the claim; the engine's renewal finds
		// it gone and cancels the engine.
		_, err := f.store.UpdateAgentDeletion(context.Background(), f.agent.ID, store.DeletionPredicate{}, store.DeletionFields{BumpClaim: true})
		assert.NoError(t, err)
		stolen.Store(err == nil)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return fmt.Errorf("the engine was not cancelled")
		}
	}
	r := f.del(t, "?force=true")
	require.True(t, stolen.Load())
	assert.NotEqual(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.Equal(t, []string{"run-a", "run-b"}, f.client.deleted())
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.True(t, got.DeletedAt.IsZero(), "the row is not soft-deleted")
	logMu.Lock()
	defer logMu.Unlock()
	assert.Contains(t, logs.String(), "delete engine lost its claim")
	assert.NotContains(t, logs.String(), "Failed to delete the run the broker held", "the lost claim is not classified")
}

// syncedLogWriter serializes writes to w (its own type: pkg/hub already has a
// lockedWriter in conduit_relay_ops_test.go).
type syncedLogWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *syncedLogWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A plain delete's retry of one of the row's previous runs whose
// cross-node intent outlives the wait goes in_doubt; when the intent later
// succeeds, the re-claimed soft finish records the run the intent deleted,
// as the engine would have.
func TestPlainDelete_RunMismatch_PreviousRun_InDoubtSoftRecordsRun(t *testing.T) {
	var waits atomic.Int32
	setDeleteWaitTimeout(t, func(context.Context) time.Duration {
		if waits.Add(1) == 1 {
			return 10 * time.Second // the first intent: failed by the test below
		}
		return 100 * time.Millisecond // the retry's intent: left outstanding
	})
	f := newDeferredDeleteFixture(t, "rmf-indoubt-soft", nil)
	f.srv.config.SoftDeleteRetention = time.Hour
	ctx := context.Background()
	for _, run := range []string{"run-p", "run-a"} {
		_, err := f.store.SetAgentRunID(ctx, f.agent.ID, run, nil)
		require.NoError(t, err)
	}

	refusal := fmt.Errorf("dispatch delete: %w", deleteAgentError(runMismatchEnvelope(t, "run-a", "run-p", true), "run-a"))
	go func() {
		assert.Eventually(t, func() bool {
			all, _ := f.store.ListPendingDispatch(ctx, f.agent.RuntimeBrokerID)
			for _, d := range all {
				if d.AgentID != f.agent.ID || d.Op != brokerDispatchOpDelete {
					continue
				}
				if ok, _ := f.store.ClaimBrokerDispatch(ctx, d.ID, "test-owner"); ok {
					_ = f.store.FailBrokerDispatch(ctx, d.ID, refusal.Error(), dispatchFailureResult(refusal))
				}
				f.bus.PublishDispatchDone(ctx, d.ID)
				return true
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "the first delete intent was never written")
	}()

	requireInDoubt(t, f, f.del(t, ""))
	intents := f.pendingDeleteIntents(t)
	require.Len(t, intents, 1, "the retry's intent is outstanding")
	args, err := UnmarshalDeleteArgs(intents[0].Args)
	require.NoError(t, err)
	require.Equal(t, "run-p", args.RunID)

	f.drainOK(t)

	got := mustGetAgent(t, f.store, f.agent.ID)
	require.False(t, got.DeletedAt.IsZero(), "soft-deleted")
	assert.Equal(t, "run-p", got.RunID, "the row records the run the intent deleted")
	assert.Equal(t, []string{"run-a"}, got.PreviousRunIDs)
}

// The re-claim of an in_doubt delete whose intent succeeded records the
// intent's runs on the soft finish only for an intent shaped like a retry
// of the run the broker held: another run than the row records, with the
// row's run among its previous runs. Any other run difference keeps the
// row's runs.
func TestFinalizeInDoubtDelete_SeedsRetriedRunOnlyForRetryIntent(t *testing.T) {
	for i, tc := range []struct {
		name     string
		runID    string
		prev     []string
		wantRun  string
		wantPrev []string
	}{
		{"retry intent: recorded", "run-p", []string{"run-a"}, "run-p", []string{"run-a"}},
		// Each case below fails one clause of the guard and passes the
		// others, so each clause is pinned on its own.
		{"other run without the row's run among its previous runs: kept", "run-x", []string{"run-old"}, "run-a", []string{"run-p"}},
		{"same run as the row, listing it too: kept", "run-a", []string{"run-p", "run-a"}, "run-a", []string{"run-p"}},
		{"no run in the intent: kept", "", []string{"run-a"}, "run-a", []string{"run-p"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, claim := newInDoubtFixture(t, fmt.Sprintf("rmf-seed-%d", i), func(f *deferredDeleteFixture) {
				f.srv.config.SoftDeleteRetention = time.Hour
				for _, run := range []string{"run-p", "run-a"} {
					_, err := f.store.SetAgentRunID(context.Background(), f.agent.ID, run, nil)
					require.NoError(t, err)
				}
			})
			req := mustGetAgent(t, f.store, f.agent.ID).ParseDeletionRequest()
			require.True(t, req.Soft)
			f.srv.finalizeInDoubtDelete(context.Background(), f.agent.ID, &DeleteDispatchArgs{
				Claim:          claim,
				SoftDelete:     req.Soft,
				DeleteFiles:    req.DeleteFiles,
				RemoveBranch:   req.RemoveBranch,
				RunID:          tc.runID,
				PreviousRunIDs: tc.prev,
			})
			got := mustGetAgent(t, f.store, f.agent.ID)
			require.False(t, got.DeletedAt.IsZero(), "soft-deleted")
			assert.Equal(t, tc.wantRun, got.RunID)
			assert.Equal(t, tc.wantPrev, got.PreviousRunIDs)
		})
	}
}

// The retried delete runs under the stopped intent the engine recorded for
// the delete before its first dispatch, and records none of its own: when
// the broker refuses it too, the rollback restores the agent's prior intent.
func TestForceDelete_RunMismatch_RetryRefusalRestoresRunIntent(t *testing.T) {
	f := newRunMismatchFixture(t, "rmf-intent", state.PhaseRunning)
	_, err := f.store.SetRunIntent(context.Background(), f.agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	f.client.answer = answerHeld(t, "run-b", func(runID string) error {
		return runMismatchEnvelope(t, runID, "run-c", true)
	})
	r := f.del(t, "?force=true")
	requireKept(t, f.store, f.agent.ID, r, http.StatusConflict, store.DeletionCodeConflict, state.PhaseRunning)
	assert.Equal(t, []string{"run-a", "run-b"}, f.client.deleted())
	requireRunIntent(t, f.store, f.agent.ID, store.RunIntentRunning)
}
