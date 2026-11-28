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

package ruleengine_test

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// TestC1_SEC3Probe1_ClaimThenStopRace_NoCallObservesLiveCtxAfterStopped (ptone/scion#9801, N-1):
// Races EvaluatePending(p2) against a concurrent stale EvaluatePending(p1)
// across 3000 iterations under -race. With C1 (Pending -> Dispatched with
// evalCtx minted under r.mu and canceled before releasing r.mu on terminal),
// no dialect call may observe a live (uncanceled) ctx after Result().Stopped == true.
func TestC1_SEC3Probe1_ClaimThenStopRace_NoCallObservesLiveCtxAfterStopped(t *testing.T) {
	const iters = 3000
	var startedAfterStop atomic.Int64
	var totalStep2Calls atomic.Int64

	for range iters {
		io := newCountingFakeIODialect("io_sec3_p1", time.Second)
		eng, _ := newChainTestEngine(t, "probe_sec3_p1", io)
		c1 := mustCompile(t, eng, "probe_sec3_p1", "io_sec3_p1", "a", ruleengine.KindFilter)
		c2 := mustCompile(t, eng, "probe_sec3_p1", "io_sec3_p1", "b", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{
				{Index: 1, Compiled: c1},
				{Index: 2, Compiled: c2},
			}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}

		// Step 2 handler verifies the C1 happens-before invariant:
		// failInvalidCompletion records Stopped=true and calls evalCtx's cancel
		// BEFORE releasing r.mu. Therefore, if Result().Stopped is observed as
		// true (which acquires r.mu), ctx.Err() read afterwards MUST already be
		// non-nil. Zero dialect calls may ever observe Stopped==true with a live
		// (uncanceled) ctx.
		io.setHandler("b", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			totalStep2Calls.Add(1)
			if res, _ := run.Result(); res.Stopped && ctx.Err() == nil {
				startedAfterStop.Add(1)
			}
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})

		_, p1, _, err := run.Next()
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.EvaluatePending(p1); err != nil {
			t.Fatal(err)
		}
		if _, err := run.Resume(); err != nil {
			t.Fatal(err)
		}
		_, p2, _, err := run.Next()
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = eng.EvaluatePending(p2)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = eng.EvaluatePending(p1) // stale replay
		}()
		close(start)
		wg.Wait()
	}

	if got := startedAfterStop.Load(); got != 0 {
		t.Fatalf("SEC3-L1: %d / %d step-2 dialect calls started after Result().Stopped==true (or with canceled ctx); want 0",
			got, totalStep2Calls.Load())
	}
}

// TestC1_SEC3Probe2_InFlightIOObservedCanceledOnStaleReplay (ptone/scion#9801):
// Deterministic in-flight test: while step 2's dialect call is blocked in
// Dispatched, a stale step-1 token replay terminates the run. The dialect MUST
// observe ctx.Err() != nil before returning, and Result() MUST report
// steps=[1:pass, 2:failed/invalid_completion], Unexecuted=[3], Stopped=true,
// with no resurrection on subsequent Next().
func TestC1_SEC3Probe2_InFlightIOObservedCanceledOnStaleReplay(t *testing.T) {
	io := newCountingFakeIODialect("io_sec3_p2", 5*time.Second)
	started := make(chan struct{})
	var ctxErrAtExit atomic.Value
	io.setHandler("b", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		close(started)
		select {
		case <-ctx.Done():
			ctxErrAtExit.Store(ctx.Err())
			return ruleengine.Decision{}, ctx.Err()
		case <-time.After(300 * time.Millisecond):
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		}
	})

	eng, _ := newChainTestEngine(t, "probe_sec3_p2", io)
	c1 := mustCompile(t, eng, "probe_sec3_p2", "io_sec3_p2", "a", ruleengine.KindFilter)
	c2 := mustCompile(t, eng, "probe_sec3_p2", "io_sec3_p2", "b", ruleengine.KindFilter)
	c3 := mustCompile(t, eng, "probe_sec3_p2", "io_sec3_p2", "c", ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}
	run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
		ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: c1},
			{Index: 2, Compiled: c2},
			{Index: 3, Compiled: c3},
		}},
		ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, p1, _, _ := run.Next()
	if err := eng.EvaluatePending(p1); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Resume(); err != nil {
		t.Fatal(err)
	}
	_, p2, _, _ := run.Next()

	evalDone := make(chan error, 1)
	go func() {
		evalDone <- eng.EvaluatePending(p2)
	}()
	<-started

	// Stale replay lands while step 2 is in flight (Dispatched).
	if err := eng.EvaluatePending(p1); err == nil {
		t.Fatal("stale replay unexpectedly succeeded")
	}

	if err := <-evalDone; err != nil {
		t.Fatalf("EvaluatePending: %v", err)
	}

	if v := ctxErrAtExit.Load(); v == nil {
		t.Fatalf("in-flight dialect call did NOT observe ctx.Err() != nil when run became terminal")
	}

	_, p3, done3, err3 := run.Next()
	if p3 != nil || !done3 || err3 != nil {
		t.Fatalf("post-terminal Next() = (p3=%v, done=%v, err=%v), want (nil, true, nil)", p3, done3, err3)
	}

	res, _ := run.Result()
	if !res.Stopped {
		t.Fatalf("expected Stopped=true")
	}
	if len(res.Steps) != 2 || res.Steps[0].Index != 1 || res.Steps[0].Class != ruleengine.StepClassPass ||
		res.Steps[1].Index != 2 || res.Steps[1].Class != ruleengine.StepClassFailed || res.Steps[1].Reason != ruleengine.ReasonInvalidCompletion {
		t.Fatalf("steps = %+v, want [1:pass, 2:failed/invalid_completion]", res.Steps)
	}
	if !slices.Equal(res.Unexecuted, []int{3}) {
		t.Fatalf("Unexecuted = %v, want [3]", res.Unexecuted)
	}
	if res.Through != 2 {
		t.Fatalf("Through = %d, want 2", res.Through)
	}
}

// TestC1_GroupInterestWithdrawal (ptone/scion#9801):
//   - Subtest 1 (one_run_replays_stale_other_completes_normally): two runs share
//     one EvaluateGroup call; run 1 replays a stale token while the group call is
//     in flight. The group ctx MUST NOT cancel, run 2 completes normally, the
//     shared tracker is charged ONCE, and run 1 records invalid_completion.
//   - Subtest 2 (both_runs_terminal_cancels_group_ctx): both runs replay stale
//     tokens while the group call is in flight. On the second (last) withdrawal,
//     the shared group ctx MUST cancel immediately.
func TestC1_GroupInterestWithdrawal(t *testing.T) {
	t.Run("one_run_replays_stale_other_completes_normally", func(t *testing.T) {
		io := newCountingFakeIODialect("io_grp_c1_a", 5*time.Second)
		started := make(chan struct{})
		release := make(chan struct{})
		var sawCtxErr atomic.Bool
		io.setHandler("b", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			close(started)
			select {
			case <-ctx.Done():
				sawCtxErr.Store(true)
				return ruleengine.Decision{}, ctx.Err()
			case <-release:
			}
			time.Sleep(15 * time.Millisecond)
			if ctx.Err() != nil {
				sawCtxErr.Store(true)
				return ruleengine.Decision{}, ctx.Err()
			}
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})

		eng, _ := newChainTestEngine(t, "probe_grp_c1_a", io)
		c1 := mustCompile(t, eng, "probe_grp_c1_a", "io_grp_c1_a", "a", ruleengine.KindFilter)
		c2 := mustCompile(t, eng, "probe_grp_c1_a", "io_grp_c1_a", "b", ruleengine.KindFilter)

		ioTracker := ruleengine.NewBudgetTracker(5 * time.Second)
		ioDom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: 5 * time.Second, Tracker: ioTracker}}
		inDom := ruleengine.Domain{Ctx: context.Background()}

		mkRun := func() (*ruleengine.ChainRun, *ruleengine.PendingStep, *ruleengine.PendingStep) {
			r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: inDom, IO: ioDom},
				ruleengine.Chain{Elements: []ruleengine.ChainElement{
					{Index: 1, Compiled: c1},
					{Index: 2, Compiled: c2, GroupKey: "gk"},
				}},
				ruleengine.Input{Activation: map[string]any{"x": 1}, AdmittedInputKey: "k1"},
				ruleengine.ChainLimits{}, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, p1, _, _ := r.Next()
			if err := eng.EvaluatePending(p1); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Resume(); err != nil {
				t.Fatal(err)
			}
			_, p2, _, _ := r.Next()
			return r, p1, p2
		}

		r1, r1p1, r1p2 := mkRun()
		r2, _, r2p2 := mkRun()
		callsBeforeGroup := io.callCount()
		spentBeforeGroup := ioTracker.Spent()

		grpDone := make(chan error, 1)
		go func() {
			_, err := eng.EvaluateGroup(r1p2, []*ruleengine.PendingStep{r2p2})
			grpDone <- err
		}()
		<-started

		// Terminate r1 via stale replay while group call is in flight.
		if err := eng.EvaluatePending(r1p1); err == nil {
			t.Fatal("stale replay on r1 unexpectedly succeeded")
		}
		close(release)

		if err := <-grpDone; err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		if sawCtxErr.Load() {
			t.Fatalf("group evalCtx was canceled when only 1 of 2 members terminated")
		}
		if got := io.callCount() - callsBeforeGroup; got != 1 {
			t.Fatalf("group IO calls = %d, want 1", got)
		}
		if _, err := r2.Resume(); err != nil {
			t.Fatalf("r2.Resume: %v", err)
		}

		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if !res1.Stopped || len(res1.Steps) != 2 || res1.Steps[1].Reason != ruleengine.ReasonInvalidCompletion {
			t.Fatalf("r1 unexpected result: %+v", res1)
		}
		if len(res2.Steps) != 2 || res2.Steps[1].Class != ruleengine.StepClassPass {
			t.Fatalf("r2 should have passed uncancelled; got %+v", res2)
		}
		// Shared tracker charged once, and surviving member r2 carries the charged Elapsed.
		groupCharge := ioTracker.Spent() - spentBeforeGroup
		if groupCharge <= 0 || res2.Steps[1].Elapsed != groupCharge {
			t.Fatalf("group charge mismatch: tracker delta=%s, r2.Elapsed=%s", groupCharge, res2.Steps[1].Elapsed)
		}
	})

	t.Run("both_runs_terminal_cancels_group_ctx", func(t *testing.T) {
		io := newCountingFakeIODialect("io_grp_c1_b", 5*time.Second)
		started := make(chan struct{})
		var canceled atomic.Bool
		io.setHandler("b", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			close(started)
			select {
			case <-ctx.Done():
				canceled.Store(true)
				return ruleengine.Decision{}, ctx.Err()
			case <-time.After(300 * time.Millisecond):
				return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
			}
		})

		eng, _ := newChainTestEngine(t, "probe_grp_c1_b", io)
		c1 := mustCompile(t, eng, "probe_grp_c1_b", "io_grp_c1_b", "a", ruleengine.KindFilter)
		c2 := mustCompile(t, eng, "probe_grp_c1_b", "io_grp_c1_b", "b", ruleengine.KindFilter)

		ioTracker := ruleengine.NewBudgetTracker(5 * time.Second)
		ioDom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: 5 * time.Second, Tracker: ioTracker}}
		inDom := ruleengine.Domain{Ctx: context.Background()}

		mkRun := func() (*ruleengine.ChainRun, *ruleengine.PendingStep, *ruleengine.PendingStep) {
			r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: inDom, IO: ioDom},
				ruleengine.Chain{Elements: []ruleengine.ChainElement{
					{Index: 1, Compiled: c1},
					{Index: 2, Compiled: c2, GroupKey: "gk"},
				}},
				ruleengine.Input{Activation: map[string]any{"x": 1}, AdmittedInputKey: "k1"},
				ruleengine.ChainLimits{}, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, p1, _, _ := r.Next()
			_ = eng.EvaluatePending(p1)
			_, _ = r.Resume()
			_, p2, _, _ := r.Next()
			return r, p1, p2
		}

		r1, r1p1, r1p2 := mkRun()
		r2, r2p1, r2p2 := mkRun()

		grpDone := make(chan error, 1)
		go func() {
			_, err := eng.EvaluateGroup(r1p2, []*ruleengine.PendingStep{r2p2})
			grpDone <- err
		}()
		<-started

		// Terminate BOTH r1 and r2 while group call is in flight.
		if err := eng.EvaluatePending(r1p1); err == nil {
			t.Fatal("stale replay on r1 unexpectedly succeeded")
		}
		if err := eng.EvaluatePending(r2p1); err == nil {
			t.Fatal("stale replay on r2 unexpectedly succeeded")
		}

		if err := <-grpDone; err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		if !canceled.Load() {
			t.Fatalf("group evalCtx was NOT canceled after ALL participating runs became terminal")
		}
		for name, r := range map[string]*ruleengine.ChainRun{"r1": r1, "r2": r2} {
			res, _ := r.Result()
			if !res.Stopped || len(res.Steps) != 2 || res.Steps[1].Reason != ruleengine.ReasonInvalidCompletion {
				t.Fatalf("%s unexpected result: %+v", name, res)
			}
		}
	})
}

// TestC1_StateTransitionTable exercises the full C1 state × event matrix
// (Ready, Evaluating, Pending, Dispatched, PendingCompleted, Done ×
// Next, Resume, Claim/EvaluatePending, Complete, StaleReplay).
func TestC1_StateTransitionTable(t *testing.T) {
	t.Run("Ready_Resume_errors_no_transition", func(t *testing.T) {
		io := newCountingFakeIODialect("io_sm1", time.Second)
		eng, _ := newChainTestEngine(t, "sm1", io)
		c := mustCompile(t, eng, "sm1", "io_sm1", "a", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Resume(); err == nil {
			t.Fatal("Resume in Ready must error")
		}
		// Still Ready: Next() succeeds and yields PendingStep.
		_, p, done, err := r.Next()
		if err != nil || done || p == nil {
			t.Fatalf("Next after rejected Resume = (%v, %v, %v), want pending", p, done, err)
		}
	})

	t.Run("Pending_Next_and_live_Resume_error_no_transition", func(t *testing.T) {
		io := newCountingFakeIODialect("io_sm2", time.Second)
		eng, _ := newChainTestEngine(t, "sm2", io)
		c := mustCompile(t, eng, "sm2", "io_sm2", "a", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		r, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p, _, err := r.Next()
		if err != nil || p == nil {
			t.Fatalf("Next: %v %v", p, err)
		}
		if _, _, _, err := r.Next(); err == nil {
			t.Fatal("Next in Pending must error")
		}
		if _, err := r.Resume(); err == nil {
			t.Fatal("Resume in Pending with live ctx must error")
		}
		// Still Pending: EvaluatePending succeeds.
		if err := eng.EvaluatePending(p); err != nil {
			t.Fatalf("EvaluatePending after rejected Next/Resume: %v", err)
		}
	})

	t.Run("Pending_Resume_with_canceled_ctx_becomes_Done", func(t *testing.T) {
		io := newCountingFakeIODialect("io_sm3", time.Second)
		eng, _ := newChainTestEngine(t, "sm3", io)
		c := mustCompile(t, eng, "sm3", "io_sm3", "a", ruleengine.KindFilter)
		ctx, cancel := context.WithCancel(context.Background())
		dom := ruleengine.Domain{Ctx: ctx}
		r, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p, _, _ := r.Next()
		cancel()
		ctrl, err := r.Resume()
		if err != nil || !ctrl.Stop {
			t.Fatalf("Resume in Pending with canceled ctx: ctrl=%+v err=%v", ctrl, err)
		}
		// Token is now consumed / run is Done; EvaluatePending must fail with 0 calls.
		if err := eng.EvaluatePending(p); err == nil {
			t.Fatal("EvaluatePending after canceled Resume must fail")
		}
		if io.callCount() != 0 {
			t.Fatalf("expected 0 IO calls, got %d", io.callCount())
		}
	})

	t.Run("Dispatched_Next_Resume_and_double_claim_error_no_transition", func(t *testing.T) {
		io := newCountingFakeIODialect("io_sm4", 5*time.Second)
		started := make(chan struct{})
		release := make(chan struct{})
		io.setHandler("a", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			close(started)
			<-release
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})
		eng, _ := newChainTestEngine(t, "sm4", io)
		c := mustCompile(t, eng, "sm4", "io_sm4", "a", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		r, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p, _, _ := r.Next()

		evalDone := make(chan error, 1)
		go func() { evalDone <- eng.EvaluatePending(p) }()
		<-started

		// In Dispatched: Next and Resume must error with NO transition.
		if _, _, _, err := r.Next(); err == nil {
			t.Fatal("Next during Dispatched must error")
		}
		if _, err := r.Resume(); err == nil {
			t.Fatal("Resume during Dispatched must error")
		}

		// Still Dispatched: releasing allows completion -> PendingCompleted -> Resume succeeds.
		close(release)
		if err := <-evalDone; err != nil {
			t.Fatalf("EvaluatePending: %v", err)
		}
		if _, err := r.Resume(); err != nil {
			t.Fatalf("Resume after Dispatched completion: %v", err)
		}
		res, _ := r.Result()
		if len(res.Steps) != 1 || res.Steps[0].Class != ruleengine.StepClassPass {
			t.Fatalf("unexpected steps after normal Dispatched->PendingCompleted->Done: %+v", res.Steps)
		}
	})

	t.Run("PendingCompleted_Next_errors_no_transition", func(t *testing.T) {
		io := newCountingFakeIODialect("io_sm5", time.Second)
		eng, _ := newChainTestEngine(t, "sm5", io)
		c := mustCompile(t, eng, "sm5", "io_sm5", "a", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		r, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p, _, _ := r.Next()
		if err := eng.EvaluatePending(p); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := r.Next(); err == nil {
			t.Fatal("Next in PendingCompleted must error")
		}
		// Still PendingCompleted: Resume succeeds.
		if _, err := r.Resume(); err != nil {
			t.Fatalf("Resume after rejected Next: %v", err)
		}
	})

	t.Run("Done_is_absorbing_across_Next_Resume_and_stale_claim", func(t *testing.T) {
		io := newCountingFakeIODialect("io_sm6", time.Second)
		eng, _ := newChainTestEngine(t, "sm6", io)
		c := mustCompile(t, eng, "sm6", "io_sm6", "a", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		r, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p, _, _ := r.Next()
		_ = eng.EvaluatePending(p)
		_, _ = r.Resume()

		out, pend, done, err := r.Next()
		if out != nil || pend != nil || !done || err != nil {
			t.Fatalf("Next in Done = (%v,%v,%v,%v), want (nil,nil,true,nil)", out, pend, done, err)
		}
		if _, err := r.Resume(); err == nil {
			t.Fatal("Resume in Done must error")
		}
		if err := eng.EvaluatePending(p); err == nil {
			t.Fatal("stale claim in Done must error")
		}
		var fe *ruleengine.FailedError
		if err := eng.EvaluatePending(p); !errors.As(err, &fe) || fe.Reason != ruleengine.ReasonInvalidCompletion {
			t.Fatalf("expected invalid_completion in Done, got %v", err)
		}
		res, _ := r.Result()
		if len(res.Steps) != 1 {
			t.Fatalf("Done must be absorbing (no extra steps), got %+v", res.Steps)
		}
	})
}

// TestProbe9801_GroupEarlyWithdrawBeforeSecondClaimMustNotCancelSurvivor (R-1):
// EvaluateGroup(r1p2, [r2p2]). After r1 is claimed (Dispatched) but BEFORE r2 is
// claimed, r1 is terminated by a stale replay. r1's interest withdrawal must not
// cancel the shared group evalCtx, because the dispatcher holds an interest
// across the claim loop and r2 (a distinct, live run) is about to join.
func TestProbe9801_GroupEarlyWithdrawBeforeSecondClaimMustNotCancelSurvivor(t *testing.T) {
	const iters = 5
	hits, defects := 0, 0
	for range iters {
		io := newCountingFakeIODialect("io_pr_r1", 5*time.Second)
		eng, _ := newChainTestEngine(t, "pr_r1", io)
		c1 := mustCompile(t, eng, "pr_r1", "io_pr_r1", "a", ruleengine.KindFilter)
		c2 := mustCompile(t, eng, "pr_r1", "io_pr_r1", "b", ruleengine.KindFilter)
		tr := ruleengine.NewBudgetTracker(5 * time.Second)
		ioDom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: 5 * time.Second, Tracker: tr}}
		inCtx1, cancel1 := context.WithCancel(context.Background())
		inCtx2, cancel2 := context.WithCancel(context.Background())
		mk := func(inCtx context.Context) (*ruleengine.ChainRun, *ruleengine.PendingStep, *ruleengine.PendingStep) {
			r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: inCtx}, IO: ioDom},
				ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c1}, {Index: 2, Compiled: c2, GroupKey: "gk"}}},
				ruleengine.Input{Activation: map[string]any{"x": 1}, AdmittedInputKey: "k1"}, ruleengine.ChainLimits{}, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, p1, _, _ := r.Next()
			if err := eng.EvaluatePending(p1); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Resume(); err != nil {
				t.Fatal(err)
			}
			_, p2, _, _ := r.Next()
			return r, p1, p2
		}
		r1, r1p1, r1p2 := mk(inCtx1)
		r2, _, r2p2 := mk(inCtx2)
		var sawCanceledCtx atomic.Bool
		io.setHandler("b", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			if ctx.Err() != nil {
				sawCanceledCtx.Store(true)
				return ruleengine.Decision{}, ctx.Err()
			}
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})
		calls0 := io.callCount()

		done := make(chan error, 1)
		go func() { _, err := eng.EvaluateGroup(r1p2, []*ruleengine.PendingStep{r2p2}); done <- err }()

		hit := false
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			unlock := ruleengine.ProbeLockRun(r2)
			if ruleengine.ProbeState(r1) == ruleengine.ProbeStateDispatched {
				if ruleengine.ProbeStateLocked(r2) == ruleengine.ProbeStatePending {
					hit = true
					_ = eng.EvaluatePending(r1p1) // stale replay terminates r1 (locks r1 only)
				}
				unlock()
				break
			}
			unlock()
			runtime.Gosched()
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("EvaluateGroup: %v", err)
			}
		case <-time.After(5 * time.Second):
			buf := make([]byte, 1<<20)
			t.Fatalf("deadlock: %s", buf[:runtime.Stack(buf, true)])
		}
		if !hit {
			cancel1()
			cancel2()
			continue
		}
		hits++
		if _, err := r2.Resume(); err != nil {
			t.Fatalf("r2.Resume: %v", err)
		}
		res2, _ := r2.Result()
		res1, _ := r1.Result()
		if !res1.Stopped || res1.Steps[len(res1.Steps)-1].Reason != ruleengine.ReasonInvalidCompletion {
			t.Fatalf("r1 should be terminal invalid_completion: %+v", res1)
		}
		calls := io.callCount() - calls0
		if len(res2.Steps) != 2 || res2.Steps[1].Class != ruleengine.StepClassPass || sawCanceledCtx.Load() || calls != 1 {
			defects++
			if defects == 1 {
				t.Logf("first defect: r2 step2 = %+v (Err=%v); dialect calls=%d sawCanceledCtx=%v; inCtx2.Err=%v",
					res2.Steps[len(res2.Steps)-1], res2.Steps[len(res2.Steps)-1].Err, calls, sawCanceledCtx.Load(), inCtx2.Err())
			}
		}
		cancel1()
		cancel2()
	}
	if hits == 0 {
		t.Skip("interleaving never forced")
	}
	if defects > 0 {
		t.Fatalf("cross-run cancellation: %d/%d forced interleavings canceled live run r2's evaluation after r1 alone withdrew", defects, hits)
	}
}

// customDoneCtx is a non-cancelCtx context wrapper with a real Done() channel
// so context.WithCancelCause / AfterFunc spawns a propagateCancel goroutine
// that exits ONLY when the child cancel/stop function is invoked.
type customDoneCtx struct {
	context.Context
	done chan struct{}
}

func (c *customDoneCtx) Done() <-chan struct{} { return c.done }
func (c *customDoneCtx) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// TestC1_R1_AllMembersTerminateDuringClaimCancelsGroupCtx (R-1 probe ii):
// Every claimed member terminates before dispatch (before releaseDispatcher):
// releasing the claim-phase guard drops remaining to 0, evalCtx cancels, and
// 0 dialect calls are made.
func TestC1_R1_AllMembersTerminateDuringClaimCancelsGroupCtx(t *testing.T) {
	const maxAttempts = 40
	for range maxAttempts {
		io := newCountingFakeIODialect("io_r1_all", 5*time.Second)
		var dialectStarted atomic.Bool
		io.setHandler("b", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			dialectStarted.Store(true)
			select {
			case <-ctx.Done():
				return ruleengine.Decision{}, ctx.Err()
			case <-time.After(300 * time.Millisecond):
				return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
			}
		})
		eng, _ := newChainTestEngine(t, "r1_all", io)
		c1 := mustCompile(t, eng, "r1_all", "io_r1_all", "a", ruleengine.KindFilter)
		c2 := mustCompile(t, eng, "r1_all", "io_r1_all", "b", ruleengine.KindFilter)
		ioDom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: 5 * time.Second}}
		inDom := ruleengine.Domain{Ctx: context.Background()}

		mk := func() (*ruleengine.ChainRun, *ruleengine.PendingStep, *ruleengine.PendingStep) {
			r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: inDom, IO: ioDom},
				ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c1}, {Index: 2, Compiled: c2, GroupKey: "gk"}}},
				ruleengine.Input{Activation: map[string]any{"x": 1}, AdmittedInputKey: "k1"}, ruleengine.ChainLimits{}, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, p1, _, _ := r.Next()
			_ = eng.EvaluatePending(p1)
			_, _ = r.Resume()
			_, p2, _, _ := r.Next()
			return r, p1, p2
		}
		r1, r1p1, r1p2 := mk()
		r2, r2p1, r2p2 := mk()
		calls0 := io.callCount()

		done := make(chan error, 1)
		go func() { _, err := eng.EvaluateGroup(r1p2, []*ruleengine.PendingStep{r2p2}); done <- err }()

		hit := false
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			unlockR2 := ruleengine.ProbeLockRun(r2)
			if ruleengine.ProbeState(r1) == ruleengine.ProbeStateDispatched && ruleengine.ProbeStateLocked(r2) == ruleengine.ProbeStatePending {
				// r1 is claimed, r2 not yet claimed: terminate r1 first.
				_ = eng.EvaluatePending(r1p1)
				// Now hold r1.mu so EvaluateGroup pauses in Step 1b right after
				// claiming r2 (before releaseDispatcher).
				unlockR1 := ruleengine.ProbeLockRun(r1)
				unlockR2()
				for time.Now().Before(deadline) {
					if ruleengine.ProbeState(r2) == ruleengine.ProbeStateDispatched {
						_ = eng.EvaluatePending(r2p1)
						hit = true
						break
					}
					runtime.Gosched()
				}
				unlockR1()
				break
			}
			unlockR2()
			runtime.Gosched()
		}
		if err := <-done; err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		if !hit {
			continue
		}
		if dialectStarted.Load() || io.callCount() != calls0 {
			t.Fatalf("expected 0 dialect calls when all members terminated before dispatch; started=%v calls=%d",
				dialectStarted.Load(), io.callCount()-calls0)
		}
		for name, r := range map[string]*ruleengine.ChainRun{"r1": r1, "r2": r2} {
			if ruleengine.ProbeHasInflight(r) {
				t.Fatalf("%s still has inflight after terminal", name)
			}
			res, _ := r.Result()
			if !res.Stopped || res.Steps[len(res.Steps)-1].Reason != ruleengine.ReasonInvalidCompletion {
				t.Fatalf("%s expected terminal invalid_completion, got %+v", name, res)
			}
		}
		return
	}
	t.Fatal("failed to force all-members-terminate-before-dispatch interleaving")
}

// TestC1_R1_DispatcherInterestReleasedOnAllPaths (R-1 variant):
// Verifies that EvaluateGroup releases its dispatcher interest and evalCtx
// resources with no goroutine leak across success, incompatible_group, and
// claim-race paths.
func TestC1_R1_DispatcherInterestReleasedOnAllPaths(t *testing.T) {
	parent := &customDoneCtx{Context: context.Background(), done: make(chan struct{})}
	defer close(parent.done)

	io := newCountingFakeIODialect("io_r1_leak", time.Second)
	eng, _ := newChainTestEngine(t, "r1_leak", io)
	c := mustCompile(t, eng, "r1_leak", "io_r1_leak", "a", ruleengine.KindFilter)
	ioDom := ruleengine.Domain{Ctx: parent, Budget: ruleengine.Budget{MaxEvalTime: time.Second}}
	inDom := ruleengine.Domain{Ctx: context.Background()}

	mk := func(gk, key string) (*ruleengine.ChainRun, *ruleengine.PendingStep) {
		r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: inDom, IO: ioDom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c, GroupKey: gk}}},
			ruleengine.Input{AdmittedInputKey: key}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, p, _, _ := r.Next()
		return r, p
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	const iters = 30
	for range iters {
		// 1. Success path
		r1, p1 := mk("gk", "k")
		r2, p2 := mk("gk", "k")
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatalf("success EvaluateGroup: %v", err)
		}
		if ruleengine.ProbeHasInflight(r1) || ruleengine.ProbeHasInflight(r2) {
			t.Fatal("inflight leaked on success path")
		}

		// 2. incompatible_group path
		r3, p3 := mk("gk", "k1")
		r4, p4 := mk("gk", "k2")
		if _, err := eng.EvaluateGroup(p3, []*ruleengine.PendingStep{p4}); err == nil {
			t.Fatal("expected incompatible_group")
		}
		if ruleengine.ProbeHasInflight(r3) || ruleengine.ProbeHasInflight(r4) {
			t.Fatal("inflight leaked on incompatible_group path")
		}

		// 3. Stale-token claim failure path
		r5, p5 := mk("gk", "k")
		_ = eng.EvaluatePending(p5)
		r6, p6 := mk("gk", "k")
		if _, err := eng.EvaluateGroup(p6, []*ruleengine.PendingStep{p5}); err == nil {
			t.Fatal("expected invalid_completion")
		}
		if ruleengine.ProbeHasInflight(r5) || ruleengine.ProbeHasInflight(r6) {
			t.Fatal("inflight leaked on claim-failure path")
		}
	}

	// Poll briefly for propagateCancel goroutines to exit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		if runtime.NumGoroutine() <= before+10 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak in EvaluateGroup: before=%d after=%d (iters=%d)", before, runtime.NumGoroutine(), iters)
}

// TestC1_O1_DialectPanicCompletesAsFailedNeverStuckDispatched (O-1):
// A dialect panic inside EvaluatePending or EvaluateGroup is recovered into a
// closed StepClassFailed / ReasonEvalFailed outcome so the run transitions
// cleanly Dispatched -> PendingCompleted -> Done and is never stuck Dispatched.
func TestC1_O1_DialectPanicCompletesAsFailedNeverStuckDispatched(t *testing.T) {
	t.Run("EvaluatePending", func(t *testing.T) {
		io := newCountingFakeIODialect("io_panic_single", time.Second)
		io.setHandler("boom", func(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			panic("dialect exploded")
		})
		eng, _ := newChainTestEngine(t, "o1_single", io)
		c := mustCompile(t, eng, "o1_single", "io_panic_single", "boom", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, p, _, _ := r.Next()
		if err := eng.EvaluatePending(p); err != nil {
			t.Fatalf("EvaluatePending should recover panic into outcome, got err=%v", err)
		}
		if ruleengine.ProbeHasInflight(r) {
			t.Fatal("inflight not cleared after dialect panic")
		}
		ctrl, err := r.Resume()
		if err != nil || !ctrl.Stop {
			t.Fatalf("Resume after dialect panic: ctrl=%+v err=%v", ctrl, err)
		}
		res, tr := r.Result()
		if !res.Stopped || len(res.Steps) != 1 || res.Steps[0].Class != ruleengine.StepClassFailed || res.Steps[0].Reason != ruleengine.ReasonEvalFailed {
			t.Fatalf("unexpected result after dialect panic: %+v", res)
		}
		if _, err := ruleengine.EncodeChainTrace(tr, map[string]ruleengine.Capabilities{io.Name(): io.Declares()}, 4096); err != nil {
			t.Fatalf("EncodeChainTrace rejected recovered panic trace: %v", err)
		}
	})

	t.Run("EvaluateGroup", func(t *testing.T) {
		io := newCountingFakeIODialect("io_panic_grp", time.Second)
		io.setHandler("boom", func(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			panic("group dialect exploded")
		})
		eng, _ := newChainTestEngine(t, "o1_grp", io)
		c := mustCompile(t, eng, "o1_grp", "io_panic_grp", "boom", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		mk := func() (*ruleengine.ChainRun, *ruleengine.PendingStep) {
			r, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
				ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c, GroupKey: "gk"}}},
				ruleengine.Input{AdmittedInputKey: "k"}, ruleengine.ChainLimits{}, nil, nil, nil)
			_, p, _, _ := r.Next()
			return r, p
		}
		r1, p1 := mk()
		r2, p2 := mk()
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatalf("EvaluateGroup should recover panic into outcome, got err=%v", err)
		}
		for name, r := range map[string]*ruleengine.ChainRun{"r1": r1, "r2": r2} {
			if ruleengine.ProbeHasInflight(r) {
				t.Fatalf("%s inflight not cleared after group panic", name)
			}
			ctrl, err := r.Resume()
			if err != nil || !ctrl.Stop {
				t.Fatalf("%s Resume after group panic: ctrl=%+v err=%v", name, ctrl, err)
			}
			res, _ := r.Result()
			if !res.Stopped || len(res.Steps) != 1 || res.Steps[0].Class != ruleengine.StepClassFailed || res.Steps[0].Reason != ruleengine.ReasonEvalFailed {
				t.Fatalf("%s unexpected result after group panic: %+v", name, res)
			}
		}
	})
}

type cancelOnDeclaresDialect struct {
	name          string
	cancelFn      context.CancelFunc
	declaresCalls atomic.Int32
	evalCalls     atomic.Int32
}

func (d *cancelOnDeclaresDialect) Name() string { return d.name }
func (d *cancelOnDeclaresDialect) Declares() ruleengine.Capabilities {
	// ResolveDialect calls Declares() once (AllowIO check); evaluateCore calls
	// Declares() a second time AFTER the top-of-function ctx.Err() check and
	// BEFORE the post-arm ctx.Err() check (engine.go:680). Canceling here
	// deterministically tests the post-arm guard (kills mutant c10).
	if d.declaresCalls.Add(1) == 2 && d.cancelFn != nil {
		d.cancelFn()
	}
	return ruleengine.Capabilities{
		PerformsIO:  false,
		MaxEvalTime: time.Second,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *cancelOnDeclaresDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiledWithIdentity(env.Consumer, d.name, kind, "id:"+rule, rule), nil
}
func (d *cancelOnDeclaresDialect) Evaluate(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	d.evalCalls.Add(1)
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

// TestC1_O2_PostArmCtxCancelSkipsDialectCall (O-2, kills mutant c10):
// Verifies that when caller ctx cancels between evaluateCore's entry check and
// d.Evaluate, the post-arm ctx.Err() guard prevents invoking d.Evaluate.
func TestC1_O2_PostArmCtxCancelSkipsDialectCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &cancelOnDeclaresDialect{name: "c10_dial"}
	eng, _ := newChainTestEngine(t, "o2_c10", d)
	c, err := eng.Compile(context.Background(), "c10_dial", "x", ruleengine.KindFilter, ruleengine.EnvironmentRef{Consumer: "o2_c10", Name: "e"})
	if err != nil {
		t.Fatal(err)
	}
	d.declaresCalls.Store(0)
	d.cancelFn = cancel
	_, err = eng.Evaluate(ctx, c, ruleengine.Input{}, ruleengine.Budget{MaxEvalTime: time.Second})
	if err == nil {
		t.Fatal("expected canceled error when ctx canceled before d.Evaluate")
	}
	if got := d.evalCalls.Load(); got != 0 {
		t.Fatalf("d.Evaluate was called %d times despite ctx canceled before call (mutant c10)", got)
	}
}

// TestC1_O2_EvaluatePendingReleasesStopCtxNoGoroutineLeak (O-2, kills mutant c11):
// Uses a custom non-cancelCtx parent with a Done() channel so each
// EvaluatePending call spawns a context.WithCancelCause propagation goroutine
// that exits ONLY when defer stopCtx() runs.
func TestC1_O2_EvaluatePendingReleasesStopCtxNoGoroutineLeak(t *testing.T) {
	parent := &customDoneCtx{Context: context.Background(), done: make(chan struct{})}
	defer close(parent.done)

	io := newCountingFakeIODialect("io_c11", time.Second)
	eng, _ := newChainTestEngine(t, "o2_c11", io)
	c := mustCompile(t, eng, "o2_c11", "io_c11", "a", ruleengine.KindFilter)
	ioDom := ruleengine.Domain{Ctx: parent, Budget: ruleengine.Budget{MaxEvalTime: time.Second}}
	inDom := ruleengine.Domain{Ctx: context.Background()}

	runtime.GC()
	before := runtime.NumGoroutine()

	const iters = 50
	for range iters {
		r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: inDom, IO: ioDom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, p, _, _ := r.Next()
		if err := eng.EvaluatePending(p); err != nil {
			t.Fatalf("EvaluatePending: %v", err)
		}
		if _, err := r.Resume(); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		if runtime.NumGoroutine() <= before+10 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak in EvaluatePending (mutant c11): before=%d after=%d (iters=%d)", before, runtime.NumGoroutine(), iters)
}
