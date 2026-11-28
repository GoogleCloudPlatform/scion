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
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// TestC1_SEC3Probe1_ClaimThenStopRace_ZeroCallsAfterStopped (ptone/scion#9801):
// Races EvaluatePending(p2) against a concurrent stale EvaluatePending(p1)
// across 3000 iterations under -race. With C1 (Pending -> Dispatched with
// evalCtx minted under r.mu and canceled before releasing r.mu on terminal),
// ZERO step-2 dialect calls may start after Result().Stopped == true.
func TestC1_SEC3Probe1_ClaimThenStopRace_ZeroCallsAfterStopped(t *testing.T) {
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
