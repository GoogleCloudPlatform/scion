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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// TestChainDomains_P1_IOAndInlineIndependentBudgets covers Amendment A1 P1:
// [judgment(fake I/O), violation(cel)] with Inline tracker 250ms and IO ceiling 1.5s
// -> judgment pass, CEL step has ~250ms left (Inline spent = CEL elapsed only), no budget error.
func TestChainDomains_P1_IOAndInlineIndependentBudgets(t *testing.T) {
	const consumer = "a1_p1"
	ioDial := newCountingFakeIODialect("p1_io", 3*time.Second)
	ioDial.setHandler("j_slow", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		_ = sleepWithCtx(ctx, 120*time.Millisecond)
		return ruleengine.Decision{Kind: c.Kind(), Detail: map[string]any{"ok": true}}, nil
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)

	cJ := mustCompile(t, eng, consumer, "p1_io", "j_slow", ruleengine.KindJudgment)
	cCEL := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)

	inlineTracker := ruleengine.NewBudgetTracker(250 * time.Millisecond)
	domains := ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: inlineTracker}},
		IO:     ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: nil, MaxEvalTime: 1500 * time.Millisecond}},
	}

	run, err := eng.NewChainRun(domains, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: cJ},
			{Index: 2, Compiled: cCEL},
		},
	}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun: %v", err)
	}
	_, p, _, err := run.Next()
	if err != nil || p == nil {
		t.Fatalf("step 1 Next: %v", err)
	}
	if err := eng.EvaluatePending(p); err != nil {
		t.Fatalf("EvaluatePending: %v", err)
	}
	if _, err := run.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// Inline tracker must be completely untouched by the 120ms IO step.
	if inlineTracker.Spent() != 0 {
		t.Fatalf("Inline tracker charged by IO step: spent=%s", inlineTracker.Spent())
	}
	out2, _, done2, err := run.Next()
	if err != nil || !done2 || out2 == nil || out2.Class != ruleengine.StepClassPass {
		t.Fatalf("step 2 = (%+v, %v, %v), want pass", out2, done2, err)
	}
	if inlineTracker.Spent() >= 100*time.Millisecond {
		t.Fatalf("Inline spent %s unexpectedly large (IO leaked into Inline)", inlineTracker.Spent())
	}
}

// TestChainDomains_P2_TenRunsShareInlineTracker covers Amendment A1 P2:
// 10 runs of [violation(inline slow)] sharing one Inline tracker -> later runs get budget.
func TestChainDomains_P2_TenRunsShareInlineTracker(t *testing.T) {
	const consumer = "a1_p2"
	slowInline := newCountingFakeIODialect("slow_inline_p2", time.Second)
	slowInline.performsIO = false
	slowInline.setHandler("slow15", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		if err := sleepWithCtx(ctx, 15*time.Millisecond); err != nil {
			return ruleengine.Decision{}, err
		}
		return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
	})
	eng, _ := newChainTestEngine(t, consumer, slowInline)
	cSlow := mustCompile(t, eng, consumer, "slow_inline_p2", "slow15", ruleengine.KindFilter)

	inlineTracker := ruleengine.NewBudgetTracker(50 * time.Millisecond)
	domains := ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: inlineTracker}},
		IO:     ruleengine.Domain{Ctx: context.Background()},
	}

	budgetHits := 0
	for i := 0; i < 10; i++ {
		r, err := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cSlow}}}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatalf("run %d NewChainRun: %v", i, err)
		}
		out, _, _, err := r.Next()
		if err != nil {
			t.Fatalf("run %d Next: %v", i, err)
		}
		if out.Class == ruleengine.StepClassBudget {
			budgetHits++
		}
	}
	if budgetHits == 0 {
		t.Fatalf("expected later runs to hit budget on shared Inline tracker, got 0")
	}
}

// TestChainDomains_P3_GroupedIOTrackerNilVsTracked covers Amendment A1 P3:
// 3 runs, one key; IO tracker nil vs T -> 1 call; nil charges nothing; T spent = 1 elapsed; Inline untouched.
func TestChainDomains_P3_GroupedIOTrackerNilVsTracked(t *testing.T) {
	const consumer = "a1_p3"
	for _, tracked := range []bool{false, true} {
		name := "nil_io_tracker"
		if tracked {
			name = "tracked_io_tracker"
		}
		t.Run(name, func(t *testing.T) {
			ioDial := newCountingFakeIODialect("p3_io", time.Second)
			ioDial.setHandler("J", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
				_ = sleepWithCtx(ctx, 20*time.Millisecond)
				return ruleengine.Decision{Kind: c.Kind(), Detail: map[string]any{"ok": true}}, nil
			})
			eng, _ := newChainTestEngine(t, consumer, ioDial)
			cJ := mustCompile(t, eng, consumer, "p3_io", "J", ruleengine.KindJudgment)

			inlineTr := ruleengine.NewBudgetTracker(250 * time.Millisecond)
			var ioTr *ruleengine.BudgetTracker
			if tracked {
				ioTr = ruleengine.NewBudgetTracker(time.Second)
			}
			domains := ruleengine.ChainDomains{
				Inline: ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: inlineTr}},
				IO:     ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: ioTr}},
			}
			in := ruleengine.Input{AdmittedInputKey: "p3"}

			var pends []*ruleengine.PendingStep
			var runs []*ruleengine.ChainRun
			for i := 0; i < 3; i++ {
				r, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "p3-k"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
				_, p, _, _ := r.Next()
				pends = append(pends, p)
				runs = append(runs, r)
			}
			if _, err := eng.EvaluateGroup(pends[0], pends[1:]); err != nil {
				t.Fatalf("EvaluateGroup: %v", err)
			}
			if ioDial.callCount() != 1 {
				t.Fatalf("calls = %d, want 1", ioDial.callCount())
			}
			for _, r := range runs {
				_, _ = r.Resume()
			}
			if inlineTr.Spent() != 0 {
				t.Fatalf("Inline tracker touched: %s", inlineTr.Spent())
			}
			res0, _ := runs[0].Result()
			if tracked {
				if ioTr.Spent() != res0.Steps[0].Elapsed || ioTr.Spent() == 0 {
					t.Fatalf("IO tracker spent = %s, want single elapsed %s", ioTr.Spent(), res0.Steps[0].Elapsed)
				}
			}
		})
	}
}

// TestChainDomains_P4_IOCeilingTimeoutNeverBudget covers Amendment A1 P4:
// IO ceiling 50ms, fake IO 150ms, untracked IO -> all failed/timeout, never budget.
func TestChainDomains_P4_IOCeilingTimeoutNeverBudget(t *testing.T) {
	const consumer = "a1_p4"
	ioDial := newCountingFakeIODialect("p4_io", time.Second)
	ioDial.setHandler("slow", func(ctx context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		if err := sleepWithCtx(ctx, 150*time.Millisecond); err != nil {
			return ruleengine.Decision{}, &ruleengine.FailedError{Reason: celengine.ReasonTimeout, Cause: err}
		}
		return ruleengine.Decision{Kind: ruleengine.KindJudgment}, nil
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cSlow := mustCompile(t, eng, consumer, "p4_io", "slow", ruleengine.KindJudgment)

	domains := ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: ruleengine.NewBudgetTracker(250 * time.Millisecond)}},
		IO:     ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: nil, MaxEvalTime: 50 * time.Millisecond}},
	}
	in := ruleengine.Input{AdmittedInputKey: "p4"}
	r1, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cSlow, GroupKey: "g"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
	r2, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cSlow, GroupKey: "g"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
	_, p1, _, _ := r1.Next()
	_, p2, _, _ := r2.Next()
	if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
		t.Fatalf("EvaluateGroup: %v", err)
	}
	_, _ = r1.Resume()
	_, _ = r2.Resume()
	res1, _ := r1.Result()
	res2, _ := r2.Result()
	for idx, res := range []ruleengine.ChainResult{res1, res2} {
		if res.Deciding.Class != ruleengine.StepClassFailed || res.Deciding.Reason != celengine.ReasonTimeout {
			t.Fatalf("member %d = %+v, want failed/timeout (never budget)", idx, res.Deciding)
		}
	}
}

// TestChainDomains_P5_CancelIOInFlightAndAtBarrierAndInlineCascade covers Amendment A1 P5.
func TestChainDomains_P5_CancelIOInFlightAndAtBarrierAndInlineCascade(t *testing.T) {
	const consumer = "a1_p5"
	ioDial := newCountingFakeIODialect("p5_io", time.Second)
	ioDial.setHandler("wait", func(ctx context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		<-ctx.Done()
		return ruleengine.Decision{}, ctx.Err()
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cW := mustCompile(t, eng, consumer, "p5_io", "wait", ruleengine.KindJudgment)

	// (a) Cancel IO ctx in flight.
	inlineCtx := context.Background()
	ioCtx, cancelIO := context.WithCancel(inlineCtx)
	r1, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: inlineCtx},
		IO:     ruleengine.Domain{Ctx: ioCtx},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cW}}}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	_, p1, _, _ := r1.Next()
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancelIO()
	}()
	_ = eng.EvaluatePending(p1)
	_, _ = r1.Resume()
	if res, _ := r1.Result(); res.Deciding.Class != ruleengine.StepClassCanceled {
		t.Fatalf("cancel IO in flight = %+v, want canceled", res.Deciding)
	}

	// (b) Cancel IO ctx at barrier -> 0 calls.
	before := ioDial.callCount()
	ioCtx2, cancelIO2 := context.WithCancel(inlineCtx)
	r2, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: inlineCtx},
		IO:     ruleengine.Domain{Ctx: ioCtx2},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cW}}}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	_, p2, _, _ := r2.Next()
	cancelIO2()
	_ = eng.EvaluatePending(p2)
	_, _ = r2.Resume()
	if ioDial.callCount() != before {
		t.Fatalf("cancel IO at barrier made IO call")
	}
	if res, _ := r2.Result(); res.Deciding.Class != ruleengine.StepClassCanceled {
		t.Fatalf("cancel IO at barrier = %+v, want canceled", res.Deciding)
	}

	// (c) Cancel Inline ctx cascades to child IO ctx.
	parentCtx, cancelParent := context.WithCancel(context.Background())
	childIO, cancelChild := context.WithCancel(parentCtx)
	defer cancelChild()
	r3, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: parentCtx},
		IO:     ruleengine.Domain{Ctx: childIO},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cW}}}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	_, p3, _, _ := r3.Next()
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancelParent()
	}()
	_ = eng.EvaluatePending(p3)
	_, _ = r3.Resume()
	if res, _ := r3.Result(); res.Deciding.Class != ruleengine.StepClassCanceled {
		t.Fatalf("Inline cancel cascade = %+v, want canceled", res.Deciding)
	}
}

// TestChainDomains_P6_And_P13_GroupCompatibilityByIOOnly covers Amendment A1 P6 & P13:
// differing IO ceiling/tracker/ctx -> separate groups (incompatible if grouped);
// differing Inline tracker/ctx only -> one group, 1 call.
func TestChainDomains_P6_And_P13_GroupCompatibilityByIOOnly(t *testing.T) {
	const consumer = "a1_p6_p13"
	ioDial := newCountingFakeIODialect("p6_io", time.Second)
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cJ := mustCompile(t, eng, consumer, "p6_io", "J", ruleengine.KindJudgment)

	sharedIOCtx := context.Background()
	sharedIOTr := ruleengine.NewBudgetTracker(time.Second)
	in := ruleengine.Input{AdmittedInputKey: "p6"}

	// P13 / P6 positive: same IO tracker/ceiling/ctx, DIFFERENT Inline trackers -> 1 group, 1 call.
	r1, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: sharedIOCtx, Budget: ruleengine.Budget{Tracker: ruleengine.NewBudgetTracker(100 * time.Millisecond)}},
		IO:     ruleengine.Domain{Ctx: sharedIOCtx, Budget: ruleengine.Budget{Tracker: sharedIOTr, MaxEvalTime: 500 * time.Millisecond}},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "gk"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
	r2, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: sharedIOCtx, Budget: ruleengine.Budget{Tracker: ruleengine.NewBudgetTracker(200 * time.Millisecond)}},
		IO:     ruleengine.Domain{Ctx: sharedIOCtx, Budget: ruleengine.Budget{Tracker: sharedIOTr, MaxEvalTime: 500 * time.Millisecond}},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "gk"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)

	_, p1, _, _ := r1.Next()
	_, p2, _, _ := r2.Next()
	if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
		t.Fatalf("P13 positive grouping failed: %v", err)
	}
	if ioDial.callCount() != 1 {
		t.Fatalf("P13 calls = %d, want 1", ioDial.callCount())
	}

	// P6 negative: IO tracker nil vs T -> incompatible.
	rNil, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: sharedIOCtx},
		IO:     ruleengine.Domain{Ctx: sharedIOCtx, Budget: ruleengine.Budget{Tracker: nil, MaxEvalTime: 500 * time.Millisecond}},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "gk"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
	rTr, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: sharedIOCtx},
		IO:     ruleengine.Domain{Ctx: sharedIOCtx, Budget: ruleengine.Budget{Tracker: sharedIOTr, MaxEvalTime: 500 * time.Millisecond}},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "gk"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
	_, pNil, _, _ := rNil.Next()
	_, pTr, _, _ := rTr.Next()
	if _, err := eng.EvaluateGroup(pNil, []*ruleengine.PendingStep{pTr}); err == nil {
		t.Fatalf("P6 nil vs tracked IO tracker should be incompatible")
	}
}

// TestChainDomains_P7a_And_P7b_TerminalityPerReachedElement covers Amendment A1 P7a & P7b.
func TestChainDomains_P7a_And_P7b_TerminalityPerReachedElement(t *testing.T) {
	const consumer = "a1_p7"
	ioDial := newCountingFakeIODialect("p7_io", time.Second)
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cJ := mustCompile(t, eng, consumer, "p7_io", "J", ruleengine.KindJudgment)
	cCEL := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)

	// P7a: Inline exhausted by siblings, then [io, cel] -> 1 IO call (pass), then CEL budget (no global precheck).
	exhaustedInline := ruleengine.NewBudgetTracker(10 * time.Millisecond)
	exhaustedInline.Exhaust()
	domains := ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: exhaustedInline}},
		IO:     ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: nil, MaxEvalTime: time.Second}},
	}
	rA, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
		{Index: 1, Compiled: cJ},
		{Index: 2, Compiled: cCEL},
	}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
	_, pA, _, err := rA.Next()
	if err != nil || pA == nil {
		t.Fatalf("P7a step 1 Next = (%v, %v), want pending IO", pA, err)
	}
	if err := eng.EvaluatePending(pA); err != nil {
		t.Fatalf("P7a EvaluatePending: %v", err)
	}
	if _, err := rA.Resume(); err != nil {
		t.Fatalf("P7a Resume: %v", err)
	}
	if ioDial.callCount() != 1 {
		t.Fatalf("P7a IO calls = %d, want 1", ioDial.callCount())
	}
	out2, _, done2, err := rA.Next()
	if err != nil || !done2 || out2 == nil || out2.Class != ruleengine.StepClassBudget {
		t.Fatalf("P7a step 2 = (%+v, %v, %v), want budget", out2, done2, err)
	}

	// P7b: [cel (hits budget), io] -> budget terminal, io skipped, 0 IO calls.
	before := ioDial.callCount()
	rB, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
		{Index: 1, Compiled: cCEL},
		{Index: 2, Compiled: cJ},
	}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
	outB, pB, doneB, err := rB.Next()
	if err != nil || pB != nil || !doneB || outB == nil || outB.Class != ruleengine.StepClassBudget {
		t.Fatalf("P7b step 1 = (%+v, %v, %v, %v), want budget+done", outB, pB, doneB, err)
	}
	if ioDial.callCount() != before {
		t.Fatalf("P7b made IO call after Inline budget exhaustion")
	}
}

// TestChainDomains_P9_E1ContextSplit covers Amendment A1 P9 (E1 closure):
// slow IO step times out soCtx, while a CEL-only run bound to requestCtx evaluates normally.
func TestChainDomains_P9_E1ContextSplit(t *testing.T) {
	const consumer = "a1_p9"
	ioDial := newCountingFakeIODialect("p9_io", time.Second)
	ioDial.setHandler("slow", func(ctx context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		<-ctx.Done()
		return ruleengine.Decision{}, ctx.Err()
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cSlow := mustCompile(t, eng, consumer, "p9_io", "slow", ruleengine.KindJudgment)
	cCEL := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)

	reqCtx := context.Background()
	soCtx, cancelSO := context.WithTimeout(reqCtx, 30*time.Millisecond)
	defer cancelSO()
	celTracker := ruleengine.NewBudgetTracker(250 * time.Millisecond)

	// System One policy run bound to soCtx.
	soRun, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: soCtx, Budget: ruleengine.Budget{Tracker: celTracker}},
		IO:     ruleengine.Domain{Ctx: soCtx, Budget: ruleengine.Budget{MaxEvalTime: time.Second}},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cSlow}}}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)

	// CEL-only policy run bound to reqCtx (per A1 §4 table).
	celRun, _ := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: reqCtx, Budget: ruleengine.Budget{Tracker: celTracker}},
		IO:     ruleengine.Domain{Ctx: reqCtx},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cCEL}}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)

	_, pSO, _, _ := soRun.Next()
	_ = eng.EvaluatePending(pSO)
	_, _ = soRun.Resume()
	if res, _ := soRun.Result(); res.Deciding.Class != ruleengine.StepClassCanceled {
		t.Fatalf("soRun deciding = %+v, want canceled", res.Deciding)
	}

	// Even after soCtx expired, celRun under reqCtx must succeed normally.
	outCEL, _, doneCEL, err := celRun.Next()
	if err != nil || !doneCEL || outCEL == nil || outCEL.Class != ruleengine.StepClassPass {
		t.Fatalf("P9 CEL-only run after soCtx expiry = (%+v, %v, %v), want pass", outCEL, doneCEL, err)
	}
}

// TestChainDomains_P10_And_P15_ReservedNames covers Amendment A1 P10 & P15 (E3 closure).
func TestChainDomains_P10_And_P15_ReservedNames(t *testing.T) {
	const consumer = "a1_p10"
	ioDial := newCountingFakeIODialect("p10_io", time.Second)
	ioDial.setHandler("judge", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		return ruleengine.Decision{
			Kind: c.Kind(),
			Detail: map[string]any{
				"flags":  map[string]bool{"pii": true},
				"labels": map[string]string{"tier": "high"},
			},
		}, nil
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)

	cJ := mustCompile(t, eng, consumer, "p10_io", "judge", ruleengine.KindJudgment)
	cViol := mustCompile(t, eng, consumer, ruleengine.DialectCEL, `judgment_flags["pii"] == true && judgment_labels["tier"] == "high" && size(judgment_scores) == 0`, ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}
	domains := ruleengine.ChainDomains{Inline: dom, IO: dom}
	reserved := []string{"judgment_flags", "judgment_labels", "judgment_scores"}

	// Legacy-mapped [judgment, violation] reading judgment_flags/labels/scores.
	legacyEnr := funcEnricher(func(step int, j ruleengine.JudgmentView) (map[string]any, error) {
		if step == 1 {
			return nil, nil
		}
		g, ok := j.Get(1)
		if !ok {
			return nil, errors.New("missing judgment")
		}
		m := g.Detail.(map[string]any)
		return map[string]any{
			"judgment_flags":  m["flags"],
			"judgment_labels": m["labels"],
			"judgment_scores": map[string]float64{},
		}, nil
	})
	run, err := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
		{Index: 1, Compiled: cJ},
		{Index: 2, Compiled: cViol},
	}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, legacyEnr, reserved)
	if err != nil {
		t.Fatalf("P10 NewChainRun: %v", err)
	}
	_, p, _, _ := run.Next()
	_ = eng.EvaluatePending(p)
	_, _ = run.Resume()
	out2, _, _, err := run.Next()
	if err != nil || out2 == nil || out2.Class != ruleengine.StepClassPass {
		t.Fatalf("P10 legacy violation = (%+v, %v), want pass", out2, err)
	}

	// Base input containing a reserved name -> construction fails.
	if _, err := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cViol}}},
		ruleengine.Input{Activation: map[string]any{"judgment_flags": map[string]bool{}}}, ruleengine.ChainLimits{}, nil, nil, reserved); err == nil {
		t.Fatalf("P10 expected construction error when base contains reserved name")
	}

	// Undeclared top-level addition -> failed/invalid_enrichment.
	undeclRun, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cViol}}},
		ruleengine.Input{}, ruleengine.ChainLimits{}, nil, funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
			return map[string]any{"sneaky_var": true}, nil
		}), reserved)
	if o, _, _, _ := undeclRun.Next(); o == nil || o.Class != ruleengine.StepClassFailed || o.Reason != ruleengine.ReasonInvalidEnrichment {
		t.Fatalf("P10 undeclared top-level addition = %+v, want failed/invalid_enrichment", o)
	}

	// Reserved name supplied with non-conforming type -> failed/invalid_enrichment.
	badTypeRun, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cViol}}},
		ruleengine.Input{}, ruleengine.ChainLimits{}, nil, funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
			return map[string]any{"judgment_flags": "not-a-map"}, nil
		}), reserved)
	if o, _, _, _ := badTypeRun.Next(); o == nil || o.Class != ruleengine.StepClassFailed || o.Reason != ruleengine.ReasonInvalidEnrichment {
		t.Fatalf("P10 non-conforming reserved type = %+v, want failed/invalid_enrichment", o)
	}

	// New chain with no reserved names attempting to supply judgment_flags -> invalid_enrichment.
	newChainRun, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cViol}}},
		ruleengine.Input{}, ruleengine.ChainLimits{}, nil, funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
			return map[string]any{"judgment_flags": map[string]bool{"pii": true}}, nil
		}), nil)
	if o, _, _, _ := newChainRun.Next(); o == nil || o.Class != ruleengine.StepClassFailed || o.Reason != ruleengine.ReasonInvalidEnrichment {
		t.Fatalf("P10 new chain injecting reserved name = %+v, want failed/invalid_enrichment", o)
	}

	// P15: reserved name is an engine root or not an identifier -> construction error.
	for _, badName := range []string{"event", "trigger", "chain", "", "123bad", "has-dash", "a.b"} {
		if _, err := eng.NewChainRun(domains, ruleengine.Chain{}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, []string{badName}); err == nil {
			t.Fatalf("P15 expected construction error for reserved name %q", badName)
		}
	}
}

// TestChainDomains_P11_EngineLevel_ContextExpiresBetweenIOAndInline covers the
// engine half of Amendment A1 P11: IO/judgment step passes, then ctx expires
// before next inline step -> inline step is canceled.
func TestChainDomains_P11_EngineLevel_ContextExpiresBetweenIOAndInline(t *testing.T) {
	const consumer = "a1_p11"
	ioDial := newCountingFakeIODialect("p11_io", time.Second)
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cJ := mustCompile(t, eng, consumer, "p11_io", "J", ruleengine.KindJudgment)
	cCEL := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)

	soCtx, cancelSO := context.WithCancel(context.Background())
	domains := ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: soCtx, Budget: ruleengine.Budget{Tracker: ruleengine.NewBudgetTracker(250 * time.Millisecond)}},
		IO:     ruleengine.Domain{Ctx: soCtx, Budget: ruleengine.Budget{MaxEvalTime: time.Second}},
	}
	run, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
		{Index: 1, Compiled: cJ},
		{Index: 2, Compiled: cCEL},
	}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)

	_, p, _, _ := run.Next()
	if err := eng.EvaluatePending(p); err != nil {
		t.Fatalf("EvaluatePending: %v", err)
	}
	if _, err := run.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// Context expires between IO success and next Inline step.
	cancelSO()
	out2, _, done2, err := run.Next()
	if err != nil || !done2 || out2 == nil || out2.Class != ruleengine.StepClassCanceled {
		t.Fatalf("P11 step 2 after ctx expiry = (%+v, %v, %v), want canceled", out2, done2, err)
	}
}

// TestChainDomains_P12_SourceFormColdCompileAndIdentity covers Amendment A1 P12 & §4:
// cold-cache CEL compile in Source form compiles on reach via CachedCompile, is
// charged to Inline tracker inside the per-element bound, and has stable identity before compile.
func TestChainDomains_P12_SourceFormColdCompileAndIdentity(t *testing.T) {
	const consumer = "a1_p12"
	eng, celEng := newChainTestEngine(t, consumer)

	src := &ruleengine.ChainElementSource{
		Expr:       `flag == true && score > 10`,
		Scope:      celengine.CacheScope{Consumer: consumer, ID: "pol-1", Revision: 1},
		Kind:       ruleengine.KindFilter,
		EnvRef:     ruleengine.EnvironmentRef{Consumer: consumer, Name: "chain_test"},
		EnvVersion: 1,
	}
	el := ruleengine.ChainElement{Index: 1, Dialect: ruleengine.DialectCEL, Kind: ruleengine.KindFilter, Source: src}

	// Stable program identity before compilation.
	id1 := el.ProgramID()
	id2 := ruleengine.ComputeSourceProgramID(ruleengine.DialectCEL, *src)
	if id1 == "" || id1 != id2 || len(id1) != 64 {
		t.Fatalf("stable ProgramID before compile = %q / %q", id1, id2)
	}

	inlineTracker := ruleengine.NewBudgetTracker(250 * time.Millisecond)
	dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: inlineTracker}}
	beforeStats := celEng.CacheStats()

	run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{el}},
		ruleengine.Input{Activation: map[string]any{"flag": true, "score": int64(42)}}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun Source: %v", err)
	}
	// Must NOT compile at construction (compiles on reach).
	if got := celEng.CacheStats(); got != beforeStats {
		t.Fatalf("Source element compiled eagerly at construction: %+v != %+v", got, beforeStats)
	}

	out, _, done, err := run.Next()
	if err != nil || !done || out == nil || out.Class != ruleengine.StepClassPass {
		t.Fatalf("P12 Source Next = (%+v, %v, %v), want pass", out, done, err)
	}
	if afterStats := celEng.CacheStats(); afterStats.Entries <= beforeStats.Entries {
		t.Fatalf("expected CachedCompile to populate CEL cache: before=%+v after=%+v", beforeStats, afterStats)
	}
	if inlineTracker.Spent() <= 0 {
		t.Fatalf("expected cold Source compile+eval to charge Inline tracker, spent=%s", inlineTracker.Spent())
	}
}

// TestChainDomains_P14_NilDomainCtxConstructionError covers Amendment A1 P14.
func TestChainDomains_P14_NilDomainCtxConstructionError(t *testing.T) {
	eng, _ := newChainTestEngine(t, "a1_p14")
	if _, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: nil}, IO: ruleengine.Domain{Ctx: context.Background()}}, ruleengine.Chain{}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil); err == nil {
		t.Fatalf("expected error for nil Inline.Ctx")
	}
	if _, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: context.Background()}, IO: ruleengine.Domain{Ctx: nil}}, ruleengine.Chain{}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil); err == nil {
		t.Fatalf("expected error for nil IO.Ctx")
	}
}

// TestChainDomains_P16_InlineCancelCascadesToIndependentIOCtx covers Amendment A1 P16 (D2):
// Inline ctx canceled while IO ctx is independently constructed -> run canceled, pending
// step completed terminally, no further elements.
func TestChainDomains_P16_InlineCancelCascadesToIndependentIOCtx(t *testing.T) {
	const consumer = "a1_p16"
	ioDial := newCountingFakeIODialect("p16_io", time.Second)
	ioDial.setHandler("block", func(ctx context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		<-ctx.Done()
		return ruleengine.Decision{}, ctx.Err()
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cBlock := mustCompile(t, eng, consumer, "p16_io", "block", ruleengine.KindJudgment)
	cAfter := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)

	inlineCtx, cancelInline := context.WithCancel(context.Background())
	independentIOCtx := context.Background() // NOT a child of inlineCtx

	run, err := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: ruleengine.Domain{Ctx: inlineCtx},
		IO:     ruleengine.Domain{Ctx: independentIOCtx},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{
		{Index: 1, Compiled: cBlock},
		{Index: 2, Compiled: cAfter},
	}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun: %v", err)
	}
	_, p, _, err := run.Next()
	if err != nil || p == nil {
		t.Fatalf("Next: %v", err)
	}
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancelInline()
	}()
	if err := eng.EvaluatePending(p); err != nil {
		t.Fatalf("EvaluatePending: %v", err)
	}
	ctrl, err := run.Resume()
	if err != nil || !ctrl.Stop {
		t.Fatalf("Resume = (%+v, %v), want Stop=true", ctrl, err)
	}
	res, _ := run.Result()
	if res.Deciding.Class != ruleengine.StepClassCanceled || len(res.Unexecuted) != 1 || res.Unexecuted[0] != 2 {
		t.Fatalf("P16 result = %+v, want canceled with step 2 unexecuted", res)
	}
}

// TestChainDomains_P17_EngineLevel_IOPassThenBrokenInlineSource covers the engine half
// of Amendment A1 P17: [io(pass), inline-Source(broken CEL)] -> 1 provider call,
// then failed/compile_error with the judgment present in JudgmentView.
func TestChainDomains_P17_EngineLevel_IOPassThenBrokenInlineSource(t *testing.T) {
	const consumer = "a1_p17"
	ioDial := newCountingFakeIODialect("p17_io", time.Second)
	ioDial.setHandler("J", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		return ruleengine.Decision{Kind: c.Kind(), Detail: map[string]any{"eval_id": "ev-17"}}, nil
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cJ := mustCompile(t, eng, consumer, "p17_io", "J", ruleengine.KindJudgment)

	var sawJudgment bool
	enr := funcEnricher(func(step int, j ruleengine.JudgmentView) (map[string]any, error) {
		if step == 2 {
			if g, ok := j.Get(1); ok && g.Detail.(map[string]any)["eval_id"] == "ev-17" {
				sawJudgment = true
			}
		}
		return nil, nil
	})

	dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: ruleengine.NewBudgetTracker(250 * time.Millisecond)}}
	run, err := eng.NewChainRun(ruleengine.ChainDomains{
		Inline: dom,
		IO:     ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second}},
	}, ruleengine.Chain{Elements: []ruleengine.ChainElement{
		{Index: 1, Compiled: cJ},
		{
			Index:   2,
			Dialect: ruleengine.DialectCEL,
			Kind:    ruleengine.KindFilter,
			Source: &ruleengine.ChainElementSource{
				Expr:   `flag === broken_syntax`,
				Scope:  celengine.CacheScope{Consumer: consumer, ID: "pol-17", Revision: 1},
				Kind:   ruleengine.KindFilter,
				EnvRef: ruleengine.EnvironmentRef{Consumer: consumer, Name: "chain_test"},
			},
		},
	}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, enr, nil)
	if err != nil {
		t.Fatalf("P17 NewChainRun: %v", err)
	}
	_, p, _, err := run.Next()
	if err != nil || p == nil {
		t.Fatalf("P17 step 1 Next: %v", err)
	}
	if err := eng.EvaluatePending(p); err != nil {
		t.Fatalf("P17 EvaluatePending: %v", err)
	}
	if _, err := run.Resume(); err != nil {
		t.Fatalf("P17 Resume: %v", err)
	}
	if ioDial.callCount() != 1 {
		t.Fatalf("P17 IO calls = %d, want 1", ioDial.callCount())
	}
	out2, _, done2, err := run.Next()
	if err != nil || !done2 || out2 == nil || out2.Class != ruleengine.StepClassFailed || out2.Reason != ruleengine.ReasonCompileError {
		t.Fatalf("P17 step 2 = (%+v, %v, %v), want failed/compile_error", out2, done2, err)
	}
	if !sawJudgment {
		t.Fatalf("P17 expected step 1 judgment to be present when step 2 failed compilation")
	}
	// Earlier judgment stays recorded on the run after step 2 compile_error.
	if g, ok := run.Judgments().Get(1); !ok || g.Detail.(map[string]any)["eval_id"] != "ev-17" {
		t.Fatalf("P17 run.Judgments().Get(1) = (%+v, %v), want eval_id=ev-17", g, ok)
	}
	var ce *celengine.CompileError
	if !errors.As(out2.Err, &ce) {
		t.Fatalf("P17 step 2 Err = %v, want *celengine.CompileError", out2.Err)
	}
}

// TestChainDomains_P18_IOElementWithSourceConstructionError covers Amendment A1 P18:
// an IO element carrying Source is a construction error.
func TestChainDomains_P18_IOElementWithSourceConstructionError(t *testing.T) {
	const consumer = "a1_p18"
	ioDial := newCountingFakeIODialect("p18_io", time.Second)
	eng, _ := newChainTestEngine(t, consumer, ioDial)

	dom := ruleengine.Domain{Ctx: context.Background()}
	_, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{{
			Index:   1,
			Dialect: "p18_io",
			Kind:    ruleengine.KindJudgment,
			Source: &ruleengine.ChainElementSource{
				Expr:   "spec",
				Kind:   ruleengine.KindJudgment,
				EnvRef: ruleengine.EnvironmentRef{Consumer: consumer, Name: "chain_test"},
			},
		}},
	}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "performs IO") {
		t.Fatalf("P18 err = %v, want construction error for IO element with Source", err)
	}
}
