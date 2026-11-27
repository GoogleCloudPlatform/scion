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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// countingFakeIODialect is a dialect-neutral fake I/O dialect with programmable
// latency, call counting, request metadata capture, and typed provenance for
// chain guarantee tests (§3.7).
type countingFakeIODialect struct {
	name        string
	performsIO  bool
	maxEvalTime time.Duration
	traceFields []ruleengine.TraceField
	reasons     []string

	mu       sync.Mutex
	calls    atomic.Int64
	handlers map[string]func(ctx context.Context, c ruleengine.Compiled, in ruleengine.Input, b ruleengine.Budget) (ruleengine.Decision, error)
	seen     []any
}

func newCountingFakeIODialect(name string, maxEvalTime time.Duration) *countingFakeIODialect {
	return &countingFakeIODialect{
		name:        name,
		performsIO:  true,
		maxEvalTime: maxEvalTime,
		reasons:     []string{"abstain", "provider_error", "rate_limited"},
		traceFields: []ruleengine.TraceField{
			{Name: "outcome", Kind: ruleengine.TraceFieldEnum, EnumValues: []string{"ok", "abstain", "error"}},
			{Name: "cached", Kind: ruleengine.TraceFieldBool},
			{Name: "latency_ms", Kind: ruleengine.TraceFieldIntRange, Min: 0, Max: 60000},
			{Name: "instance_id", Kind: ruleengine.TraceFieldID, MaxLen: 64},
			{Name: "spec_hash", Kind: ruleengine.TraceFieldHash, HashLen: 16},
		},
		handlers: make(map[string]func(context.Context, ruleengine.Compiled, ruleengine.Input, ruleengine.Budget) (ruleengine.Decision, error)),
	}
}

func (d *countingFakeIODialect) Name() string { return d.name }

func (d *countingFakeIODialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:   d.performsIO,
		MaxEvalTime:  d.maxEvalTime,
		ResultKinds:  []ruleengine.Kind{ruleengine.KindFilter, ruleengine.KindJudgment},
		TraceReasons: d.reasons,
		TraceFields:  d.traceFields,
	}
}

func (d *countingFakeIODialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiledWithIdentity(env.Consumer, d.name, kind, "fake-prog:"+rule, rule), nil
}

func (d *countingFakeIODialect) setHandler(rule string, fn func(context.Context, ruleengine.Compiled, ruleengine.Input, ruleengine.Budget) (ruleengine.Decision, error)) {
	d.mu.Lock()
	d.handlers[rule] = fn
	d.mu.Unlock()
}

func (d *countingFakeIODialect) Evaluate(ctx context.Context, c ruleengine.Compiled, in ruleengine.Input, b ruleengine.Budget) (ruleengine.Decision, error) {
	d.calls.Add(1)
	d.mu.Lock()
	d.seen = append(d.seen, in.Payload)
	rule, _ := c.Rep().(string)
	fn := d.handlers[rule]
	d.mu.Unlock()
	if fn != nil {
		return fn(ctx, c, in, b)
	}
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

func (d *countingFakeIODialect) callCount() int64 { return d.calls.Load() }

func (d *countingFakeIODialect) seenPayloads() []any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.seen)
}

func sleepWithCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newChainTestEngine(t *testing.T, consumer string, extra ...ruleengine.Dialect) (*ruleengine.Engine, *celengine.Engine) {
	t.Helper()
	celEng, err := celengine.New(celengine.Environment{
		Name:    "chain_test",
		Version: 1,
		Variables: []celengine.Variable{
			{Name: "flag", Type: celengine.Bool()},
			{Name: "score", Type: celengine.Int()},
			{Name: "chain", Type: celengine.Map(celengine.String(), celengine.Dyn())},
			{Name: "judgment_flags", Type: celengine.Map(celengine.String(), celengine.Bool())},
			{Name: "judgment_labels", Type: celengine.Map(celengine.String(), celengine.String())},
			{Name: "judgment_scores", Type: celengine.Map(celengine.String(), celengine.Double())},
		},
		Macros: celengine.MacrosStandard,
	}, celengine.DefaultLimits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	celDial, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	eng := ruleengine.NewEngine()
	if err := eng.Register(celDial); err != nil {
		t.Fatalf("Register cel: %v", err)
	}
	grants := []ruleengine.DialectGrant{ruleengine.Grant(ruleengine.DialectCEL)}
	for _, d := range extra {
		if err := eng.Register(d); err != nil {
			t.Fatalf("Register %q: %v", d.Name(), err)
		}
		if d.Declares().PerformsIO {
			grants = append(grants, ruleengine.GrantIO(d.Name()))
		} else {
			grants = append(grants, ruleengine.Grant(d.Name()))
		}
	}
	if err := eng.AllowConsumerDialects(consumer, grants...); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}
	return eng, celEng
}

func mustCompile(t *testing.T, eng *ruleengine.Engine, consumer, dialect, expr string, kind ruleengine.Kind) ruleengine.Compiled {
	t.Helper()
	c, err := eng.Compile(context.Background(), dialect, expr, kind, ruleengine.EnvironmentRef{
		Consumer: consumer,
		Name:     "chain_test",
	})
	if err != nil {
		t.Fatalf("Compile(%q, %q): %v", dialect, expr, err)
	}
	return c
}

type funcEnricher func(step int, j ruleengine.JudgmentView) (map[string]any, error)

func (f funcEnricher) Additions(step int, j ruleengine.JudgmentView) (map[string]any, error) {
	return f(step, j)
}

// TestChain_StrictAuthorOrderAndZeroIOAfterStop covers §3.7 items 1, 2, 4 and
// v1 §4 acceptance 3: strict author order, zero I/O calls for unreached steps,
// no parallel evaluation within a run, and separation of StepClass vs Verdict.
func TestChain_StrictAuthorOrderAndZeroIOAfterStop(t *testing.T) {
	const consumer = "test_consumer"
	ioDial := newCountingFakeIODialect("fake_io", time.Second)
	ioDial.setHandler("uncertain_io", func(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		return ruleengine.Decision{}, &ruleengine.UncertainError{Reason: "abstain"}
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)

	celFalse := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == false", ruleengine.KindFilter)
	celTrue := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)
	ioRule := mustCompile(t, eng, consumer, "fake_io", "ok_io", ruleengine.KindFilter)
	ioUnc := mustCompile(t, eng, consumer, "fake_io", "uncertain_io", ruleengine.KindFilter)

	dom := ruleengine.Domain{Ctx: context.Background()}
	domains := ruleengine.ChainDomains{Inline: dom, IO: dom}

	// Case 1: [cel(false), io_fake] -> no PendingStep, 0 IO calls, element 2 unexecuted.
	run1, err := eng.NewChainRun(domains, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: celFalse},
			{Index: 2, Compiled: ioRule},
		},
	}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun: %v", err)
	}
	out, pend, done, err := run1.Next()
	if err != nil || pend != nil || !done || out == nil || out.Class != ruleengine.StepClassFail {
		t.Fatalf("Next = (%+v, %v, %v, %v), want (fail, nil, true, nil)", out, pend, done, err)
	}
	if ioDial.callCount() != 0 {
		t.Fatalf("fake_io called %d times, want 0", ioDial.callCount())
	}
	res1, tr1 := run1.Result()
	if !res1.Stopped || !slices.Equal(res1.Unexecuted, []int{2}) || !slices.Equal(tr1.Unexecuted, []int{2}) {
		t.Fatalf("Result/Trace unexecuted mismatch: res=%+v tr=%+v", res1, tr1)
	}

	// Case 2: [cel(true), io_fake(uncertain), cel] -> stops at 2 with class=uncertain
	// and consumer verdict="allow_override" kept strictly separate; element 3 skipped.
	run2, err := eng.NewChainRun(domains, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: celTrue},
			{Index: 2, Compiled: ioUnc},
			{Index: 3, Compiled: celTrue},
		},
	}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, func(o ruleengine.StepOutcome) ruleengine.Control {
		if o.Class == ruleengine.StepClassUncertain {
			return ruleengine.Control{Stop: true, Verdict: "allow_override"}
		}
		return ruleengine.DefaultANDPolicy(o)
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun 2: %v", err)
	}
	out, pend, done, err = run2.Next()
	if err != nil || pend != nil || done || out == nil || out.Class != ruleengine.StepClassPass {
		t.Fatalf("step 1 Next = (%+v, %v, %v, %v)", out, pend, done, err)
	}
	out, pend, done, err = run2.Next()
	if err != nil || out != nil || done || pend == nil || pend.Index() != 2 || pend.Dialect() != "fake_io" {
		t.Fatalf("step 2 Next = (%+v, %v, %v, %v)", out, pend, done, err)
	}
	// No parallel evaluation within a run: calling Next() while pending must error.
	if _, _, _, nextErr := run2.Next(); nextErr == nil {
		t.Fatalf("expected error calling Next() while pending step is unresolved")
	}
	if err := eng.EvaluatePending(pend); err != nil {
		t.Fatalf("EvaluatePending: %v", err)
	}
	ctrl, err := run2.Resume()
	if err != nil || !ctrl.Stop || ctrl.Verdict != "allow_override" {
		t.Fatalf("Resume = (%+v, %v), want Stop=true Verdict=allow_override", ctrl, err)
	}
	// Calling Next() after stop returns done with no pending step.
	_, pendAfter, doneAfter, err := run2.Next()
	if err != nil || pendAfter != nil || !doneAfter {
		t.Fatalf("Next after stop = (%v, %v, %v), want (nil, true, nil)", pendAfter, doneAfter, err)
	}
	res2, tr2 := run2.Result()
	if res2.Deciding.Class != ruleengine.StepClassUncertain || res2.Verdict != "allow_override" {
		t.Fatalf("Class/Verdict separation broken: deciding=%+v verdict=%q", res2.Deciding, res2.Verdict)
	}
	if len(tr2.Elements) != 2 || tr2.Elements[1].Class != ruleengine.StepClassUncertain || tr2.Elements[1].Verdict != "allow_override" {
		t.Fatalf("trace elements mismatch: %+v", tr2.Elements)
	}
	if !slices.Equal(res2.Unexecuted, []int{3}) {
		t.Fatalf("Unexecuted = %v, want [3]", res2.Unexecuted)
	}
}

// TestChain_SharedTrackerClampBudgetAndCancel covers §3.7 item 3 and v1 §4 acceptance 4.
func TestChain_SharedTrackerClampBudgetAndCancel(t *testing.T) {
	const consumer = "budget_consumer"
	ioDial := newCountingFakeIODialect("slow_io", time.Second)
	var secondObservedRemaining time.Duration
	ioDial.setHandler("step_40ms", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, b ruleengine.Budget) (ruleengine.Decision, error) {
		if err := sleepWithCtx(ctx, 40*time.Millisecond); err != nil {
			return ruleengine.Decision{}, err
		}
		return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
	})
	ioDial.setHandler("step_block", func(ctx context.Context, _ ruleengine.Compiled, _ ruleengine.Input, b ruleengine.Budget) (ruleengine.Decision, error) {
		secondObservedRemaining = b.MaxEvalTime
		<-ctx.Done()
		return ruleengine.Decision{}, &ruleengine.FailedError{Reason: celengine.ReasonTimeout, Cause: ctx.Err()}
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)

	c40 := mustCompile(t, eng, consumer, "slow_io", "step_40ms", ruleengine.KindFilter)
	cBlk := mustCompile(t, eng, consumer, "slow_io", "step_block", ruleengine.KindFilter)

	tracker := ruleengine.NewBudgetTracker(70 * time.Millisecond)
	dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: tracker}}
	domains := ruleengine.ChainDomains{Inline: dom, IO: dom}

	run1, err := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c40}}}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun 1: %v", err)
	}
	_, p1, _, err := run1.Next()
	if err != nil || p1 == nil {
		t.Fatalf("run1.Next: p=%v err=%v", p1, err)
	}
	if err := eng.EvaluatePending(p1); err != nil {
		t.Fatalf("EvaluatePending 1: %v", err)
	}
	if _, err := run1.Resume(); err != nil {
		t.Fatalf("Resume 1: %v", err)
	}

	// Second run on the same tracker must have its deadline clamped to remainder (~30ms) and yield budget.
	run2, err := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cBlk}}}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun 2: %v", err)
	}
	_, p2, _, err := run2.Next()
	if err != nil || p2 == nil {
		t.Fatalf("run2.Next: p=%v err=%v", p2, err)
	}
	if err := eng.EvaluatePending(p2); err != nil {
		t.Fatalf("EvaluatePending 2: %v", err)
	}
	if _, err := run2.Resume(); err != nil {
		t.Fatalf("Resume 2: %v", err)
	}
	res2, _ := run2.Result()
	if res2.Deciding.Class != ruleengine.StepClassBudget {
		t.Fatalf("run2 deciding = %+v, want budget", res2.Deciding)
	}
	if secondObservedRemaining >= 70*time.Millisecond {
		t.Fatalf("second run deadline was not clamped to remainder: got %s", secondObservedRemaining)
	}

	// Cancellation mid-run yields canceled and no further element.
	ctx, cancel := context.WithCancel(context.Background())
	cCancel := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)
	cDom := ruleengine.Domain{Ctx: ctx}
	run3, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: cDom, IO: cDom}, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: cCancel},
			{Index: 2, Compiled: c40},
		},
	}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun 3: %v", err)
	}
	out1, _, done1, err := run3.Next()
	if err != nil || done1 || out1.Class != ruleengine.StepClassPass {
		t.Fatalf("run3 step 1 = (%+v, %v, %v)", out1, done1, err)
	}
	cancel()
	callsBefore := ioDial.callCount()
	out2, p3, done2, err := run3.Next()
	if err != nil || p3 != nil || !done2 || out2 == nil || out2.Class != ruleengine.StepClassCanceled {
		t.Fatalf("run3 step 2 after cancel = (%+v, %v, %v, %v), want canceled+done", out2, p3, done2, err)
	}
	if ioDial.callCount() != callsBefore {
		t.Fatalf("IO dialect called after cancel")
	}
}

// TestChain_AdmissionPerElementAndPerMemberAtEvaluation covers §3.7 item 5.
func TestChain_AdmissionPerElementAndPerMemberAtEvaluation(t *testing.T) {
	const consumer = "adm_consumer"
	ioDial := newCountingFakeIODialect("io_adm", time.Second)
	eng, _ := newChainTestEngine(t, consumer, ioDial)

	c1 := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)
	c2 := mustCompile(t, eng, consumer, "io_adm", "r1", ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}

	run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: c1},
			{Index: 2, Compiled: c2},
		},
	}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun: %v", err)
	}
	if _, _, _, err := run.Next(); err != nil {
		t.Fatalf("step 1 Next: %v", err)
	}
	// Revoke io_adm before step 2 is reached.
	if err := eng.AllowConsumerDialects(consumer, ruleengine.Grant(ruleengine.DialectCEL)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out, pend, done, err := run.Next()
	if err != nil || pend != nil || !done || out == nil || out.Class != ruleengine.StepClassFailed || out.Reason != ruleengine.ReasonDialectNotAllowed {
		t.Fatalf("revoked step 2 = (%+v, %v, %v, %v), want failed/dialect_not_allowed", out, pend, done, err)
	}
	if ioDial.callCount() != 0 {
		t.Fatalf("revoked IO dialect was called")
	}
}

// TestChain_ImmutabilityJudgmentsAndEnricher covers §3.7 item 6 and v1 §4 acceptance 6.
func TestChain_ImmutabilityJudgmentsAndEnricher(t *testing.T) {
	const consumer = "imm_consumer"
	ioDial := newCountingFakeIODialect("judge_io", time.Second)
	dialectOwnedMap := map[string]any{"verdict": "flagged", "nested": map[string]any{"k": "v1"}}
	ioDial.setHandler("j1", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		return ruleengine.Decision{Kind: c.Kind(), Detail: dialectOwnedMap}, nil
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)

	cJ := mustCompile(t, eng, consumer, "judge_io", "j1", ruleengine.KindJudgment)
	cCheck := mustCompile(t, eng, consumer, ruleengine.DialectCEL, `flag == true && chain.s1.verdict == "flagged"`, ruleengine.KindFilter)

	baseAct := map[string]any{"flag": true}
	dom := ruleengine.Domain{Ctx: context.Background()}

	enr := funcEnricher(func(step int, j ruleengine.JudgmentView) (map[string]any, error) {
		if step == 1 {
			if len(j.Steps()) != 0 {
				return nil, fmt.Errorf("step 1 should see 0 earlier judgments, got %v", j.Steps())
			}
			return nil, nil
		}
		got, ok := j.Get(1)
		if !ok {
			return nil, errors.New("missing step 1 judgment")
		}
		// Mutate the returned copy; subsequent Get must still see the original.
		m := got.Detail.(map[string]any)
		m["verdict"] = "tampered"
		again, _ := j.Get(1)
		if again.Detail.(map[string]any)["verdict"] != "flagged" {
			return nil, errors.New("JudgmentView did not return defensive copy")
		}
		return map[string]any{"chain": map[string]any{"s1": again.Detail}}, nil
	})

	run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: cJ},
			{Index: 2, Compiled: cCheck},
		},
	}, ruleengine.Input{Activation: baseAct}, ruleengine.ChainLimits{}, nil, enr, nil)
	if err != nil {
		t.Fatalf("NewChainRun: %v", err)
	}
	// Mutate caller's baseAct after NewChainRun -> must NOT affect run.
	baseAct["flag"] = false

	_, pend, _, err := run.Next()
	if err != nil || pend == nil {
		t.Fatalf("step 1 Next: %v", err)
	}
	if err := eng.EvaluatePending(pend); err != nil {
		t.Fatalf("EvaluatePending: %v", err)
	}
	// Mutate dialect's own map after evaluation -> must NOT affect stored judgment.
	dialectOwnedMap["verdict"] = "mutated_by_dialect"

	if _, err := run.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	out2, _, done2, err := run.Next()
	if err != nil || !done2 || out2 == nil || out2.Class != ruleengine.StepClassPass {
		t.Fatalf("step 2 = (%+v, %v, %v), want pass", out2, done2, err)
	}

	// Enricher attempting to overwrite a base key or inject outside chain.* fails with invalid_enrichment.
	badEnr := funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
		return map[string]any{"flag": false}, nil
	})
	badRun, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{{Index: 1, Compiled: mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)}},
	}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, badEnr, nil)
	if err != nil {
		t.Fatalf("NewChainRun badEnr: %v", err)
	}
	badOut, _, _, err := badRun.Next()
	if err != nil || badOut == nil || badOut.Class != ruleengine.StepClassFailed || badOut.Reason != ruleengine.ReasonInvalidEnrichment {
		t.Fatalf("badEnr out = (%+v, %v), want failed/invalid_enrichment", badOut, err)
	}

	// Two concurrent runs never observe each other (-race).
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{
				Elements: []ruleengine.ChainElement{{Index: 1, Compiled: mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)}},
			}, ruleengine.Input{Activation: map[string]any{"flag": i%2 == 0}}, ruleengine.ChainLimits{}, nil, nil, nil)
			if err != nil {
				t.Errorf("concurrent NewChainRun: %v", err)
				return
			}
			o, _, _, err := r.Next()
			if err != nil {
				t.Errorf("concurrent Next: %v", err)
				return
			}
			want := ruleengine.StepClassPass
			if i%2 != 0 {
				want = ruleengine.StepClassFail
			}
			if o.Class != want {
				t.Errorf("concurrent run %d class = %s, want %s", i, o.Class, want)
			}
		}(i)
	}
	wg.Wait()
}

// TestChain_CompletionTokenSecurity covers §3.7 item 7 and v1 §4 acceptance 9:
// single-use replay, foreign engine token, post-stop completion, and terminal
// recording of expired pending steps.
func TestChain_CompletionTokenSecurity(t *testing.T) {
	const consumer = "tok_consumer"
	ioDial := newCountingFakeIODialect("tok_io", time.Second)
	eng1, _ := newChainTestEngine(t, consumer, ioDial)
	eng2, _ := newChainTestEngine(t, consumer, newCountingFakeIODialect("tok_io", time.Second))

	cIO := mustCompile(t, eng1, consumer, "tok_io", "r1", ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}

	run, err := eng1.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO}},
	}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun: %v", err)
	}
	_, p, _, err := run.Next()
	if err != nil || p == nil {
		t.Fatalf("Next: %v", err)
	}

	// Foreign engine token rejected.
	if err := eng2.EvaluatePending(p); err == nil {
		t.Fatalf("expected foreign engine EvaluatePending to fail")
	}

	// First completion succeeds.
	if err := eng1.EvaluatePending(p); err != nil {
		t.Fatalf("EvaluatePending: %v", err)
	}
	// Replay of same token rejected.
	errReplay := eng1.EvaluatePending(p)
	var fErr *ruleengine.FailedError
	if !errors.As(errReplay, &fErr) || fErr.Reason != ruleengine.ReasonInvalidCompletion {
		t.Fatalf("replay err = %v, want *FailedError{Reason: invalid_completion}", errReplay)
	}

	// Terminal completion on Resume when ctx expired before EvaluatePending.
	ctx, cancel := context.WithCancel(context.Background())
	cDom := ruleengine.Domain{Ctx: ctx}
	runExp, err := eng1.NewChainRun(ruleengine.ChainDomains{Inline: cDom, IO: cDom}, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO}},
	}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewChainRun exp: %v", err)
	}
	_, pExp, _, err := runExp.Next()
	if err != nil || pExp == nil {
		t.Fatalf("Next exp: %v", err)
	}
	cancel()
	ctrl, err := runExp.Resume()
	if err != nil || !ctrl.Stop {
		t.Fatalf("Resume on canceled pending = (%+v, %v), want Stop=true", ctrl, err)
	}
	resExp, _ := runExp.Result()
	if resExp.Deciding.Class != ruleengine.StepClassCanceled {
		t.Fatalf("expired pending deciding = %+v, want canceled", resExp.Deciding)
	}
	// Post-stop completion of pExp rejected.
	if err := eng1.EvaluatePending(pExp); err == nil {
		t.Fatalf("expected post-stop completion to be rejected")
	}
}

// TestChain_GroupedExecution_E1_E2_E3_AndWorkedExample covers §3.7 item 8,
// §3.3 worked example, E1/E2/E3a/E3b/E3c-*, exhausted at dispatch, legacy N->1,
// later-round same key, failed/incompatible_group, and leader-designation (6a).
func TestChain_GroupedExecution_E1_E2_E3_AndWorkedExample(t *testing.T) {
	const consumer = "grp_consumer"

	t.Run("WorkedExample_Exactly3Executions", func(t *testing.T) {
		ioDial := newCountingFakeIODialect("s1_io", time.Second)
		ioDial.setHandler("J", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{Kind: c.Kind(), Detail: map[string]any{"j": true}}, nil
		})
		ioDial.setHandler("K", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{Kind: c.Kind(), Detail: map[string]any{"k": true}}, nil
		})
		eng, _ := newChainTestEngine(t, consumer, ioDial)

		celPass := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)
		celFail := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == false", ruleengine.KindFilter)
		cJ := mustCompile(t, eng, consumer, "s1_io", "J", ruleengine.KindJudgment)
		cK := mustCompile(t, eng, consumer, "s1_io", "K", ruleengine.KindJudgment)

		tracker := ruleengine.NewBudgetTracker(5 * time.Second)
		dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: tracker}}
		domains := ruleengine.ChainDomains{Inline: dom, IO: dom}
		in := ruleengine.Input{Activation: map[string]any{"flag": true}, AdmittedInputKey: "msg-1"}

		// P1 = [g1, J, v1, K, w1]
		p1, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: celPass},
			{Index: 2, Compiled: cJ, GroupKey: "key-J"},
			{Index: 3, Compiled: celPass},
			{Index: 4, Compiled: cK, GroupKey: "key-K"},
			{Index: 5, Compiled: celPass},
		}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		// P2 = [g2, J, v2]
		p2, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: celPass},
			{Index: 2, Compiled: cJ, GroupKey: "key-J"},
			{Index: 3, Compiled: celPass},
		}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		// P3 = [g3, K]
		p3, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: celPass},
			{Index: 2, Compiled: cK, GroupKey: "key-K"},
		}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		// P4 = [g4(false), J]
		p4, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: celFail},
			{Index: 2, Compiled: cJ, GroupKey: "key-J"},
		}}, in, ruleengine.ChainLimits{}, nil, nil, nil)

		runs := []*ruleengine.ChainRun{p1, p2, p3, p4}
		// Fake round-based grouping scheduler (§3.3).
		for round := 0; round < 5; round++ {
			groups := map[string][]*ruleengine.PendingStep{}
			var keyOrder []string
			var pendingRuns []*ruleengine.ChainRun
			for _, r := range runs {
				for {
					_, pend, done, err := r.Next()
					if err != nil {
						t.Fatalf("round %d Next: %v", round, err)
					}
					if done {
						break
					}
					if pend != nil {
						k := pend.GroupKey()
						if _, ok := groups[k]; !ok {
							keyOrder = append(keyOrder, k)
						}
						groups[k] = append(groups[k], pend)
						pendingRuns = append(pendingRuns, r)
						break
					}
				}
			}
			if len(pendingRuns) == 0 {
				break
			}
			for _, k := range keyOrder {
				steps := groups[k]
				if _, err := eng.EvaluateGroup(steps[0], steps[1:]); err != nil {
					t.Fatalf("round %d EvaluateGroup(%s): %v", round, k, err)
				}
			}
			for _, r := range pendingRuns {
				if _, err := r.Resume(); err != nil {
					t.Fatalf("round %d Resume: %v", round, err)
				}
			}
		}

		if got := ioDial.callCount(); got != 3 {
			t.Fatalf("worked example executions = %d, want exactly 3", got)
		}
		res4, _ := p4.Result()
		if !res4.Stopped || res4.Through != 1 || !slices.Equal(res4.Unexecuted, []int{2}) {
			t.Fatalf("P4 result = %+v, want stopped at g4 with J unexecuted", res4)
		}
	})

	t.Run("E1_SharedSuccess_OneCall_AndIndependentCopies", func(t *testing.T) {
		ioDial := newCountingFakeIODialect("e1_io", time.Second)
		ioDial.setHandler("J", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{Kind: c.Kind(), Detail: map[string]any{"label": "safe"}}, nil
		})
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		cJ := mustCompile(t, eng, consumer, "e1_io", "J", ruleengine.KindJudgment)
		cRead := mustCompile(t, eng, consumer, ruleengine.DialectCEL, `chain.s1.label == "safe"`, ruleengine.KindFilter)

		dom := ruleengine.Domain{Ctx: context.Background()}
		domains := ruleengine.ChainDomains{Inline: dom, IO: dom}
		in := ruleengine.Input{AdmittedInputKey: "e1-msg"}

		mutatingEnr := funcEnricher(func(step int, j ruleengine.JudgmentView) (map[string]any, error) {
			if step == 1 {
				return nil, nil
			}
			g, _ := j.Get(1)
			m := g.Detail.(map[string]any)
			cp := map[string]any{"label": m["label"]}
			m["label"] = "mutated_by_member_1"
			return map[string]any{"chain": map[string]any{"s1": cp}}, nil
		})
		readingEnr := funcEnricher(func(step int, j ruleengine.JudgmentView) (map[string]any, error) {
			if step == 1 {
				return nil, nil
			}
			g, _ := j.Get(1)
			return map[string]any{"chain": map[string]any{"s1": g.Detail}}, nil
		})

		r1, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: cJ, GroupKey: "gk1"},
			{Index: 2, Compiled: cRead},
		}}, in, ruleengine.ChainLimits{}, nil, mutatingEnr, nil)
		r2, _ := eng.NewChainRun(domains, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: cJ, GroupKey: "gk1"},
			{Index: 2, Compiled: cRead},
		}}, in, ruleengine.ChainLimits{}, nil, readingEnr, nil)

		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		gid, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
		if err != nil || gid == "" {
			t.Fatalf("EvaluateGroup: gid=%q err=%v", gid, err)
		}
		if ioDial.callCount() != 1 {
			t.Fatalf("E1 calls = %d, want 1", ioDial.callCount())
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		o1, _, _, _ := r1.Next()
		o2, _, _, _ := r2.Next()
		if o1.Class != ruleengine.StepClassPass || o2.Class != ruleengine.StepClassPass {
			t.Fatalf("E1 member steps = (%s, %s), want (pass, pass)", o1.Class, o2.Class)
		}
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if res1.Steps[0].GroupID != gid || res2.Steps[0].GroupID != gid || res2.Steps[0].Elapsed != 0 {
			t.Fatalf("group attribution mismatch: r1=%+v r2=%+v", res1.Steps[0], res2.Steps[0])
		}
	})

	t.Run("E2_StoppedRunNeverPending", func(t *testing.T) {
		ioDial := newCountingFakeIODialect("e2_io", time.Second)
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		celFail := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == false", ruleengine.KindFilter)
		cJ := mustCompile(t, eng, consumer, "e2_io", "J", ruleengine.KindJudgment)
		dom := ruleengine.Domain{Ctx: context.Background()}

		r, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: celFail},
			{Index: 2, Compiled: cJ, GroupKey: "gk"},
		}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p, done, _ := r.Next()
		if p != nil || !done || ioDial.callCount() != 0 {
			t.Fatalf("E2 violated: p=%v done=%v calls=%d", p, done, ioDial.callCount())
		}
	})

	t.Run("E3a_SharedFailure_OneCall_PerMemberVerdict", func(t *testing.T) {
		ioDial := newCountingFakeIODialect("e3a_io", time.Second)
		ioDial.setHandler("fail_prov", func(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{}, &ruleengine.FailedError{Reason: "provider_error"}
		})
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		cF := mustCompile(t, eng, consumer, "e3a_io", "fail_prov", ruleengine.KindJudgment)
		dom := ruleengine.Domain{Ctx: context.Background()}
		in := ruleengine.Input{AdmittedInputKey: "e3a"}

		r1, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cF, GroupKey: "g"}}}, in, ruleengine.ChainLimits{},
			func(o ruleengine.StepOutcome) ruleengine.Control {
				return ruleengine.Control{Stop: true, Verdict: "reject_fail_closed"}
			}, nil, nil)
		r2, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cF, GroupKey: "g"}}}, in, ruleengine.ChainLimits{},
			func(o ruleengine.StepOutcome) ruleengine.Control {
				return ruleengine.Control{Stop: true, Verdict: "allow_fail_open"}
			}, nil, nil)

		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		if ioDial.callCount() != 1 {
			t.Fatalf("E3a calls = %d, want 1", ioDial.callCount())
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if res1.Deciding.Class != ruleengine.StepClassFailed || res1.Deciding.Reason != "provider_error" || res1.Verdict != "reject_fail_closed" {
			t.Fatalf("r1 = %+v", res1)
		}
		if res2.Deciding.Class != ruleengine.StepClassFailed || res2.Deciding.Reason != "provider_error" || res2.Verdict != "allow_fail_open" {
			t.Fatalf("r2 = %+v", res2)
		}
	})

	t.Run("E3b_CancelInFlight_AllCanceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ioDial := newCountingFakeIODialect("e3b_io", time.Second)
		ioDial.setHandler("block", func(c context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			go func() {
				time.Sleep(15 * time.Millisecond)
				cancel()
			}()
			<-c.Done()
			return ruleengine.Decision{}, c.Err()
		})
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		cB := mustCompile(t, eng, consumer, "e3b_io", "block", ruleengine.KindJudgment)
		dom := ruleengine.Domain{Ctx: ctx}
		in := ruleengine.Input{AdmittedInputKey: "e3b"}

		r1, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cB, GroupKey: "g"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		r2, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cB, GroupKey: "g"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if res1.Deciding.Class != ruleengine.StepClassCanceled || res2.Deciding.Class != ruleengine.StepClassCanceled {
			t.Fatalf("E3b classes = (%s, %s), want (canceled, canceled)", res1.Deciding.Class, res2.Deciding.Class)
		}
	})

	t.Run("E3c_CancelAtBarrier_ZeroCalls", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ioDial := newCountingFakeIODialect("e3cb_io", time.Second)
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		cJ := mustCompile(t, eng, consumer, "e3cb_io", "J", ruleengine.KindJudgment)
		dom := ruleengine.Domain{Ctx: ctx}
		in := ruleengine.Input{AdmittedInputKey: "e3cb"}

		r1, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "g"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		r2, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "g"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		cancel()
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		if ioDial.callCount() != 0 {
			t.Fatalf("E3c-cancel-at-barrier calls = %d, want 0", ioDial.callCount())
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if res1.Deciding.Class != ruleengine.StepClassCanceled || res2.Deciding.Class != ruleengine.StepClassCanceled {
			t.Fatalf("E3c-cancel-at-barrier classes = (%s, %s), want (canceled, canceled)", res1.Deciding.Class, res2.Deciding.Class)
		}
	})

	t.Run("E3c_Skew_SlowSiblingChargesSharedTracker_NoBarrierCharge", func(t *testing.T) {
		slowInline := newCountingFakeIODialect("slow_inline", time.Second)
		slowInline.performsIO = false
		slowInline.setHandler("slow30", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			_ = sleepWithCtx(ctx, 30*time.Millisecond)
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})
		ioDial := newCountingFakeIODialect("skew_io", time.Second)
		var observedBudget time.Duration
		ioDial.setHandler("io20", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, b ruleengine.Budget) (ruleengine.Decision, error) {
			observedBudget = b.MaxEvalTime
			_ = sleepWithCtx(ctx, 20*time.Millisecond)
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})
		eng, _ := newChainTestEngine(t, consumer, slowInline, ioDial)
		cSlow := mustCompile(t, eng, consumer, "slow_inline", "slow30", ruleengine.KindFilter)
		cIO := mustCompile(t, eng, consumer, "skew_io", "io20", ruleengine.KindJudgment)

		tracker := ruleengine.NewBudgetTracker(500 * time.Millisecond)
		dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: tracker}}
		in := ruleengine.Input{AdmittedInputKey: "skew"}

		r1, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: cIO, GroupKey: "g"},
		}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		r2, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: cSlow},
			{Index: 2, Compiled: cIO, GroupKey: "g"},
		}}, in, ruleengine.ChainLimits{}, nil, nil, nil)

		// r1 reaches barrier immediately; wait 40ms at barrier before r2 runs slow inline (30ms).
		_, p1, _, _ := r1.Next()
		time.Sleep(40 * time.Millisecond)
		_, _, _, _ = r2.Next() // runs slow30 (~30ms)
		_, p2, _, _ := r2.Next()

		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		// Bound re-read at dispatch must reflect slow30's charge (< 480ms), and total spent
		// must NOT include the 40ms barrier wait (~50ms eval total, well below 150ms).
		if observedBudget >= 480*time.Millisecond {
			t.Fatalf("bound at dispatch not reduced by sibling slow inline: %s", observedBudget)
		}
		if spent := tracker.Spent(); spent < 40*time.Millisecond || spent > 250*time.Millisecond {
			t.Fatalf("tracker spent = %s, want ~50ms (no barrier wait charge)", spent)
		}
	})

	t.Run("E3c_Incompatible_And_FailClosed_ZeroCalls", func(t *testing.T) {
		ioDial := newCountingFakeIODialect("inc_io", time.Second)
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		cJ1 := mustCompile(t, eng, consumer, "inc_io", "J1", ruleengine.KindJudgment)
		cJ2 := mustCompile(t, eng, consumer, "inc_io", "J2", ruleengine.KindJudgment)

		ctx1, cancel1 := context.WithCancel(context.Background())
		defer cancel1()
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		tr1 := ruleengine.NewBudgetTracker(time.Second)
		tr2 := ruleengine.NewBudgetTracker(time.Second)

		checkIncompatible := func(name string, d1, d2 ruleengine.ChainDomains, e1, e2 ruleengine.ChainElement, in1, in2 ruleengine.Input) {
			t.Helper()
			t.Run(name, func(t *testing.T) {
				before := ioDial.callCount()
				r1, err := eng.NewChainRun(d1, ruleengine.Chain{Elements: []ruleengine.ChainElement{e1}}, in1, ruleengine.ChainLimits{}, nil, nil, nil)
				if err != nil {
					t.Fatalf("r1: %v", err)
				}
				r2, err := eng.NewChainRun(d2, ruleengine.Chain{Elements: []ruleengine.ChainElement{e2}}, in2, ruleengine.ChainLimits{}, nil, nil, nil)
				if err != nil {
					t.Fatalf("r2: %v", err)
				}
				_, p1, _, _ := r1.Next()
				_, p2, _, _ := r2.Next()
				_, err = eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
				var fErr *ruleengine.FailedError
				if !errors.As(err, &fErr) || fErr.Reason != ruleengine.ReasonIncompatibleGroup {
					t.Fatalf("EvaluateGroup err = %v, want *FailedError{Reason: incompatible_group}", err)
				}
				if ioDial.callCount() != before {
					t.Fatalf("incompatible group executed IO call")
				}
				_, _ = r1.Resume()
				_, _ = r2.Resume()
				res1, _ := r1.Result()
				res2, _ := r2.Result()
				if res1.Deciding.Class != ruleengine.StepClassFailed || res1.Deciding.Reason != ruleengine.ReasonIncompatibleGroup ||
					res2.Deciding.Class != ruleengine.StepClassFailed || res2.Deciding.Reason != ruleengine.ReasonIncompatibleGroup {
					t.Fatalf("fail-closed outcomes wrong: r1=%+v r2=%+v", res1.Deciding, res2.Deciding)
				}
			})
		}

		baseDom := ruleengine.Domain{Ctx: ctx1, Budget: ruleengine.Budget{Tracker: tr1, MaxEvalTime: 500 * time.Millisecond}}
		baseD := ruleengine.ChainDomains{Inline: baseDom, IO: baseDom}
		baseEl := ruleengine.ChainElement{Index: 1, Compiled: cJ1, GroupKey: "gk"}
		baseIn := ruleengine.Input{AdmittedInputKey: "in-1"}

		checkIncompatible("different_tracker", baseD, ruleengine.ChainDomains{Inline: baseDom, IO: ruleengine.Domain{Ctx: ctx1, Budget: ruleengine.Budget{Tracker: tr2, MaxEvalTime: 500 * time.Millisecond}}}, baseEl, baseEl, baseIn, baseIn)
		checkIncompatible("different_ceiling", baseD, ruleengine.ChainDomains{Inline: baseDom, IO: ruleengine.Domain{Ctx: ctx1, Budget: ruleengine.Budget{Tracker: tr1, MaxEvalTime: 300 * time.Millisecond}}}, baseEl, baseEl, baseIn, baseIn)
		checkIncompatible("different_io_ctx", baseD, ruleengine.ChainDomains{Inline: baseDom, IO: ruleengine.Domain{Ctx: ctx2, Budget: baseDom.Budget}}, baseEl, baseEl, baseIn, baseIn)
		checkIncompatible("different_group_key", baseD, baseD, baseEl, ruleengine.ChainElement{Index: 1, Compiled: cJ1, GroupKey: "gk-other"}, baseIn, baseIn)
		checkIncompatible("different_program_id", baseD, baseD, baseEl, ruleengine.ChainElement{Index: 1, Compiled: cJ2, GroupKey: "gk"}, baseIn, baseIn)
		checkIncompatible("different_admitted_input", baseD, baseD, baseEl, baseEl, baseIn, ruleengine.Input{AdmittedInputKey: "in-2"})
		checkIncompatible("empty_group_key", baseD, baseD, ruleengine.ChainElement{Index: 1, Compiled: cJ1, GroupKey: ""}, ruleengine.ChainElement{Index: 1, Compiled: cJ1, GroupKey: ""}, baseIn, baseIn)
	})

	t.Run("ExhaustedAtDispatch_ZeroCalls", func(t *testing.T) {
		ioDial := newCountingFakeIODialect("exh_io", time.Second)
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		cJ := mustCompile(t, eng, consumer, "exh_io", "J", ruleengine.KindJudgment)
		tr := ruleengine.NewBudgetTracker(10 * time.Millisecond)
		dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: tr}}
		in := ruleengine.Input{AdmittedInputKey: "exh"}

		r1, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "g"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		r2, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "g"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		tr.Exhaust()
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		if ioDial.callCount() != 0 {
			t.Fatalf("exhausted at dispatch made %d calls, want 0", ioDial.callCount())
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if res1.Deciding.Class != ruleengine.StepClassBudget || res2.Deciding.Class != ruleengine.StepClassBudget {
			t.Fatalf("exhausted classes = (%s, %s), want (budget, budget)", res1.Deciding.Class, res2.Deciding.Class)
		}
	})

	t.Run("LegacyNTo1_And_LaterRoundSameKey_SecondExecution", func(t *testing.T) {
		ioDial := newCountingFakeIODialect("n1_io", time.Second)
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		cJ := mustCompile(t, eng, consumer, "n1_io", "J", ruleengine.KindJudgment)
		dom := ruleengine.Domain{Ctx: context.Background()}
		in := ruleengine.Input{AdmittedInputKey: "n1"}

		// 5 legacy policies pending in round 1 with same key -> 1 call.
		var pends []*ruleengine.PendingStep
		var runs []*ruleengine.ChainRun
		for i := 0; i < 5; i++ {
			r, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "legacy-k"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
			_, p, _, _ := r.Next()
			pends = append(pends, p)
			runs = append(runs, r)
		}
		if _, err := eng.EvaluateGroup(pends[0], pends[1:]); err != nil {
			t.Fatalf("EvaluateGroup N->1: %v", err)
		}
		if ioDial.callCount() != 1 {
			t.Fatalf("legacy N->1 calls = %d, want 1", ioDial.callCount())
		}
		for _, r := range runs {
			_, _ = r.Resume()
		}

		// Later round with the exact same key -> second execution (never reused/joined).
		rLater, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "legacy-k"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
		_, pLater, _, _ := rLater.Next()
		if _, err := eng.EvaluateGroup(pLater, nil); err != nil {
			t.Fatalf("EvaluateGroup round 2: %v", err)
		}
		if ioDial.callCount() != 2 {
			t.Fatalf("later-round same key calls = %d, want 2", ioDial.callCount())
		}
	})

	t.Run("LeaderDesignation_6a_DifferentRuleIDAndRevision", func(t *testing.T) {
		type policyMeta struct {
			RuleID   string
			Revision int
		}
		ioDial := newCountingFakeIODialect("lead_io", time.Second)
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		// Two separately compiled handles with same semantic ProgramID (not pointer identity).
		c1 := mustCompile(t, eng, consumer, "lead_io", "same_spec", ruleengine.KindJudgment)
		c2 := mustCompile(t, eng, consumer, "lead_io", "same_spec", ruleengine.KindJudgment)
		dom := ruleengine.Domain{Ctx: context.Background()}

		rPrimary, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: c1, GroupKey: "a2-key"},
		}}, ruleengine.Input{Payload: policyMeta{RuleID: "pol-alpha", Revision: 3}, AdmittedInputKey: "same-admitted-input"}, ruleengine.ChainLimits{}, nil, nil, nil)

		rSecondary, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: c2, GroupKey: "a2-key"},
		}}, ruleengine.Input{Payload: policyMeta{RuleID: "pol-beta", Revision: 7}, AdmittedInputKey: "same-admitted-input"}, ruleengine.ChainLimits{}, nil, nil, nil)

		_, pPrimary, _, _ := rPrimary.Next()
		_, pSecondary, _, _ := rSecondary.Next()
		if _, err := eng.EvaluateGroup(pPrimary, []*ruleengine.PendingStep{pSecondary}); err != nil {
			t.Fatalf("EvaluateGroup 6a: %v", err)
		}
		if ioDial.callCount() != 1 {
			t.Fatalf("leader designation calls = %d, want 1", ioDial.callCount())
		}
		seen := ioDial.seenPayloads()
		if len(seen) != 1 || seen[0] != (policyMeta{RuleID: "pol-alpha", Revision: 3}) {
			t.Fatalf("leader payload = %+v, want pol-alpha rev 3", seen)
		}
	})
}

// TestChain_ConcurrentDisjointGroups_RaceSafe covers §3.7 item 1 & §3.10 Addendum A:
// two concurrent EvaluateGroup calls on disjoint groups sharing one tracker under -race.
func TestChain_ConcurrentDisjointGroups_RaceSafe(t *testing.T) {
	const consumer = "race_consumer"
	ioDial := newCountingFakeIODialect("race_io", time.Second)
	ioDial.setHandler("G1", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		_ = sleepWithCtx(ctx, 25*time.Millisecond)
		return ruleengine.Decision{Kind: c.Kind(), Detail: map[string]any{"g": 1}}, nil
	})
	ioDial.setHandler("G2", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		_ = sleepWithCtx(ctx, 35*time.Millisecond)
		return ruleengine.Decision{Kind: c.Kind(), Detail: map[string]any{"g": 2}}, nil
	})
	eng, _ := newChainTestEngine(t, consumer, ioDial)
	cG1 := mustCompile(t, eng, consumer, "race_io", "G1", ruleengine.KindJudgment)
	cG2 := mustCompile(t, eng, consumer, "race_io", "G2", ruleengine.KindJudgment)

	sharedTracker := ruleengine.NewBudgetTracker(5 * time.Second)
	dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: sharedTracker}}

	r1a, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cG1, GroupKey: "g1"}}}, ruleengine.Input{AdmittedInputKey: "a"}, ruleengine.ChainLimits{}, nil, nil, nil)
	r1b, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cG1, GroupKey: "g1"}}}, ruleengine.Input{AdmittedInputKey: "a"}, ruleengine.ChainLimits{}, nil, nil, nil)
	r2a, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cG2, GroupKey: "g2"}}}, ruleengine.Input{AdmittedInputKey: "b"}, ruleengine.ChainLimits{}, nil, nil, nil)
	r2b, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cG2, GroupKey: "g2"}}}, ruleengine.Input{AdmittedInputKey: "b"}, ruleengine.ChainLimits{}, nil, nil, nil)

	_, p1a, _, _ := r1a.Next()
	_, p1b, _, _ := r1b.Next()
	_, p2a, _, _ := r2a.Next()
	_, p2b, _, _ := r2b.Next()

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = eng.EvaluateGroup(p1a, []*ruleengine.PendingStep{p1b})
	}()
	go func() {
		defer wg.Done()
		_, err2 = eng.EvaluateGroup(p2a, []*ruleengine.PendingStep{p2b})
	}()
	wg.Wait()
	if err1 != nil || err2 != nil {
		t.Fatalf("concurrent EvaluateGroup errs = (%v, %v)", err1, err2)
	}
	if ioDial.callCount() != 2 {
		t.Fatalf("concurrent disjoint groups calls = %d, want 2", ioDial.callCount())
	}
	for _, r := range []*ruleengine.ChainRun{r1a, r1b, r2a, r2b} {
		if _, err := r.Resume(); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	res1a, _ := r1a.Result()
	res2a, _ := r2a.Result()
	wantSum := res1a.Steps[0].Elapsed + res2a.Steps[0].Elapsed
	if sharedTracker.Spent() != wantSum {
		t.Fatalf("sharedTracker.Spent() = %s, want exact sum %s", sharedTracker.Spent(), wantSum)
	}
}

// TestChainTrace_TypedValidationAndTruncation covers §3.7 item 9 and v1 §4 acceptance 7.
func TestChainTrace_TypedValidationAndTruncation(t *testing.T) {
	caps := map[string]ruleengine.Capabilities{
		"s1": {
			TraceReasons: []string{"abstain"},
			TraceFields: []ruleengine.TraceField{
				{Name: "outcome", Kind: ruleengine.TraceFieldEnum, EnumValues: []string{"ok", "abstain"}},
				{Name: "cached", Kind: ruleengine.TraceFieldBool},
				{Name: "latency_ms", Kind: ruleengine.TraceFieldIntRange, Min: 0, Max: 10000},
				{Name: "instance_id", Kind: ruleengine.TraceFieldID, MaxLen: 32},
				{Name: "spec_hash", Kind: ruleengine.TraceFieldHash, HashLen: 16},
			},
		},
	}

	validTrace := ruleengine.ChainTrace{
		Elements: []ruleengine.ChainTraceElement{{
			Index:   1,
			Dialect: "s1",
			Kind:    ruleengine.KindJudgment,
			Class:   ruleengine.StepClassUncertain,
			Reason:  "abstain",
			Domain:  ruleengine.DomainIO,
			Provenance: map[string]any{
				"outcome":     "abstain",
				"cached":      false,
				"latency_ms":  42,
				"instance_id": "inst-01_a:b",
				"spec_hash":   "0123456789abcdef",
			},
		}},
	}
	encoded, err := ruleengine.EncodeChainTrace(validTrace, caps, 4096)
	if err != nil {
		t.Fatalf("valid trace rejected: %v", err)
	}
	if !strings.Contains(encoded, `"instance_id":"inst-01_a:b"`) {
		t.Fatalf("encoded trace missing valid provenance: %s", encoded)
	}

	// Reject undeclared reason.
	badReason := validTrace
	badReason.Elements = []ruleengine.ChainTraceElement{{Index: 1, Dialect: "s1", Class: ruleengine.StepClassFailed, Reason: "undeclared_secret_reason"}}
	if _, err := ruleengine.EncodeChainTrace(badReason, caps, 4096); err == nil {
		t.Fatalf("expected undeclared reason to be rejected")
	}

	// Reject hostile / message-derived / unknown provenance fields.
	hostileCases := []struct {
		name string
		prov map[string]any
	}{
		{"unknown_key", map[string]any{"user_message": "hello"}},
		{"enum_not_in_set", map[string]any{"outcome": "user_controlled_text"}},
		{"bool_wrong_type", map[string]any{"cached": "true"}},
		{"int_out_of_range", map[string]any{"latency_ms": 99999}},
		{"id_with_spaces_injection", map[string]any{"instance_id": "inst 1; DROP TABLE"}},
		{"id_with_control_chars", map[string]any{"instance_id": "inst\n1"}},
		{"id_over_long", map[string]any{"instance_id": strings.Repeat("a", 33)}},
		{"hash_wrong_len", map[string]any{"spec_hash": "deadbeef"}},
		{"hash_non_hex", map[string]any{"spec_hash": "0123456789abcdeg"}},
	}
	for _, hc := range hostileCases {
		t.Run(hc.name, func(t *testing.T) {
			tr := ruleengine.ChainTrace{
				Elements: []ruleengine.ChainTraceElement{{
					Index:      1,
					Dialect:    "s1",
					Class:      ruleengine.StepClassPass,
					Provenance: hc.prov,
				}},
			}
			if _, err := ruleengine.EncodeChainTrace(tr, caps, 4096); err == nil {
				t.Fatalf("expected hostile provenance %s to be rejected", hc.name)
			}
		})
	}

	// Reject unsupported free-string TraceFieldKind (no free-string fallback).
	freeCaps := map[string]ruleengine.Capabilities{
		"s1": {TraceFields: []ruleengine.TraceField{{Name: "free", Kind: ruleengine.TraceFieldKind("string")}}},
	}
	if _, err := ruleengine.EncodeChainTrace(ruleengine.ChainTrace{
		Elements: []ruleengine.ChainTraceElement{{Index: 1, Dialect: "s1", Class: ruleengine.StepClassPass, Provenance: map[string]any{"free": "hi"}}},
	}, freeCaps, 4096); err == nil {
		t.Fatalf("expected unsupported free-string TraceFieldKind to be rejected")
	}

	// Deterministic truncation: provenance dropped earliest-first, {i, dialect, class} kept.
	truncTrace := ruleengine.ChainTrace{
		Elements: []ruleengine.ChainTraceElement{
			{Index: 1, Dialect: "s1", Class: ruleengine.StepClassPass, Provenance: map[string]any{"instance_id": "inst-1", "spec_hash": "0123456789abcdef"}},
			{Index: 2, Dialect: "s1", Class: ruleengine.StepClassPass, Provenance: map[string]any{"instance_id": "inst-2", "spec_hash": "0123456789abcdef"}},
		},
	}
	truncOut, err := ruleengine.EncodeChainTrace(truncTrace, caps, 220)
	if err != nil {
		t.Fatalf("EncodeChainTrace truncation: %v", err)
	}
	if len(truncOut) > 220 {
		t.Fatalf("truncated len %d > 220: %s", len(truncOut), truncOut)
	}
	var decoded ruleengine.ChainTrace
	if err := json.Unmarshal([]byte(truncOut), &decoded); err != nil {
		t.Fatalf("Unmarshal truncated trace: %v", err)
	}
	if len(decoded.Elements) != 2 || decoded.Elements[0].Provenance != nil || decoded.Elements[1].Provenance == nil {
		t.Fatalf("expected earliest-first provenance drop, got %s", truncOut)
	}
}

type fakeTypedDetail struct {
	ID    string
	Flags map[string]bool
	Tags  []string
}

type copyDetailDialect struct {
	*countingFakeIODialect
	copyFn func(any) any
}

func (d *copyDetailDialect) Declares() ruleengine.Capabilities {
	caps := d.countingFakeIODialect.Declares()
	caps.CopyDetail = d.copyFn
	return caps
}

// TestChain_FitCheck_R1_R2_R3_AndCompileBudgetPrecedence covers so-p1-em's
// checkpoint fit-check additive requests:
//   - R1: StepOutcome.GroupElapsed exposed to every member while Elapsed stays 0 for non-leaders.
//   - R2: Capabilities.CopyDetail (and reflection fallback) preserve concrete Go pointer/struct types without aliasing.
//   - R3: KindJudgment exists and JudgmentView stores ONLY KindJudgment steps whose class is pass.
//   - CompileBudget condition: non-nil CompileBudget never alters cancel > eval-budget > compile-budget precedence,
//     and OnCompileMiss receives only the dialect name.
func TestChain_FitCheck_R1_R2_R3_AndCompileBudgetPrecedence(t *testing.T) {
	const consumer = "fit_consumer"

	t.Run("R1_GroupElapsed_And_R2_ConcreteTypePreserved", func(t *testing.T) {
		var customCopierCalls atomic.Int64
		origDetail := &fakeTypedDetail{
			ID:    "eval-42",
			Flags: map[string]bool{"pii": true},
			Tags:  []string{"a", "b"},
		}

		for _, withCustomCopier := range []bool{true, false} {
			mode := "fallback_reflect_copier"
			if withCustomCopier {
				mode = "dialect_CopyDetail"
			}
			t.Run(mode, func(t *testing.T) {
				customCopierCalls.Store(0)
				base := newCountingFakeIODialect("fit_io", time.Second)
				base.setHandler("J", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
					_ = sleepWithCtx(ctx, 15*time.Millisecond)
					return ruleengine.Decision{Kind: c.Kind(), Detail: origDetail}, nil
				})
				var copyFn func(any) any
				if withCustomCopier {
					copyFn = func(v any) any {
						customCopierCalls.Add(1)
						src := v.(*fakeTypedDetail)
						cp := &fakeTypedDetail{
							ID:    src.ID,
							Flags: make(map[string]bool, len(src.Flags)),
							Tags:  slices.Clone(src.Tags),
						}
						for k, b := range src.Flags {
							cp.Flags[k] = b
						}
						return cp
					}
				}
				dial := &copyDetailDialect{countingFakeIODialect: base, copyFn: copyFn}
				eng, _ := newChainTestEngine(t, consumer, dial)
				cJ := mustCompile(t, eng, consumer, "fit_io", "J", ruleengine.KindJudgment)

				tracker := ruleengine.NewBudgetTracker(time.Second)
				dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: tracker}}
				in := ruleengine.Input{AdmittedInputKey: "fit-r1-r2"}

				r1, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "gk"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
				r2, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cJ, GroupKey: "gk"}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)

				_, p1, _, _ := r1.Next()
				_, p2, _, _ := r2.Next()
				if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
					t.Fatalf("EvaluateGroup: %v", err)
				}
				_, _ = r1.Resume()
				_, _ = r2.Resume()

				// R1: Leader has Elapsed == GroupElapsed > 0; non-leader has Elapsed == 0 and GroupElapsed == leader.GroupElapsed.
				res1, _ := r1.Result()
				res2, _ := r2.Result()
				if res1.Steps[0].Elapsed <= 0 || res1.Steps[0].GroupElapsed != res1.Steps[0].Elapsed {
					t.Fatalf("leader timing = %+v, want Elapsed == GroupElapsed > 0", res1.Steps[0])
				}
				if res2.Steps[0].Elapsed != 0 || res2.Steps[0].GroupElapsed != res1.Steps[0].GroupElapsed {
					t.Fatalf("member timing = %+v, want Elapsed=0 and GroupElapsed=%s", res2.Steps[0], res1.Steps[0].GroupElapsed)
				}
				if tracker.Spent() != res1.Steps[0].Elapsed {
					t.Fatalf("tracker charged %s, want single charge %s", tracker.Spent(), res1.Steps[0].Elapsed)
				}

				// R2: Concrete pointer type *fakeTypedDetail preserved and no aliasing across runs.
				j1, ok1 := r1.Judgments().Get(1)
				j2, ok2 := r2.Judgments().Get(1)
				if !ok1 || !ok2 {
					t.Fatalf("missing judgments: ok1=%v ok2=%v", ok1, ok2)
				}
				d1, okType1 := j1.Detail.(*fakeTypedDetail)
				d2, okType2 := j2.Detail.(*fakeTypedDetail)
				if !okType1 || !okType2 {
					t.Fatalf("concrete type not preserved: %T / %T", j1.Detail, j2.Detail)
				}
				if d1 == origDetail || d2 == origDetail || d1 == d2 {
					t.Fatalf("pointer aliased across dialect/members")
				}
				d1.Flags["pii"] = false
				d1.Tags[0] = "tampered"
				if !d2.Flags["pii"] || d2.Tags[0] != "a" {
					t.Fatalf("member 1 mutation leaked to member 2: %+v", d2)
				}
				if withCustomCopier && customCopierCalls.Load() == 0 {
					t.Fatalf("expected Capabilities.CopyDetail to be invoked")
				}
			})
		}
	})

	t.Run("R3_JudgmentView_StoresOnlyPassKindJudgment", func(t *testing.T) {
		ioDial := newCountingFakeIODialect("r3_io", time.Second)
		ioDial.setHandler("unc", func(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{Kind: ruleengine.KindJudgment, Detail: map[string]any{"should_not_store": true}}, &ruleengine.UncertainError{Reason: "abstain"}
		})
		eng, _ := newChainTestEngine(t, consumer, ioDial)
		cFilter := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)
		cUnc := mustCompile(t, eng, consumer, "r3_io", "unc", ruleengine.KindJudgment)

		dom := ruleengine.Domain{Ctx: context.Background()}
		run, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: cFilter}, // KindFilter pass (has CEL EvalStats in Detail) -> must NOT be in JudgmentView
			{Index: 2, Compiled: cUnc},    // KindJudgment uncertain -> must NOT be in JudgmentView
		}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)

		_, _, _, _ = run.Next()
		_, p, _, _ := run.Next()
		_ = eng.EvaluatePending(p)
		_, _ = run.Resume()
		if got := run.Judgments().Steps(); len(got) != 0 {
			t.Fatalf("JudgmentView stored non-pass or non-KindJudgment steps: %v", got)
		}
	})

	t.Run("CompileBudget_PreservesCancelPrecedence_And_OnCompileMissDialectOnly", func(t *testing.T) {
		eng, _ := newChainTestEngine(t, consumer)
		exhCompile := ruleengine.NewBudgetTracker(time.Millisecond)
		exhCompile.Exhaust()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var observedMiss []string
		dom := ruleengine.Domain{Ctx: ctx}
		run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{
			Index:   1,
			Dialect: ruleengine.DialectCEL,
			Kind:    ruleengine.KindFilter,
			Source: &ruleengine.ChainElementSource{
				Expr:          "flag == true",
				Scope:         celengine.CacheScope{Consumer: consumer, ID: "cb-1", Revision: 1},
				Kind:          ruleengine.KindFilter,
				EnvRef:        ruleengine.EnvironmentRef{Consumer: consumer, Name: "chain_test"},
				CompileBudget: exhCompile,
				OnCompileMiss: func(d string) { observedMiss = append(observedMiss, d) },
			},
		}}}, ruleengine.Input{Activation: map[string]any{"flag": true}}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatalf("NewChainRun: %v", err)
		}
		out, _, done, err := run.Next()
		if err != nil || !done || out == nil || out.Class != ruleengine.StepClassCanceled {
			t.Fatalf("canceled ctx with exhausted CompileBudget = (%+v, %v, %v), want canceled (not compile_budget)", out, done, err)
		}

		// Live ctx with non-exhausted CompileBudget invokes OnCompileMiss with dialect name only.
		liveDom := ruleengine.Domain{Ctx: context.Background()}
		run2, _ := eng.NewChainRun(ruleengine.ChainDomains{Inline: liveDom, IO: liveDom}, ruleengine.Chain{Elements: []ruleengine.ChainElement{{
			Index:   1,
			Dialect: ruleengine.DialectCEL,
			Kind:    ruleengine.KindFilter,
			Source: &ruleengine.ChainElementSource{
				Expr:          "flag == true && score == 7",
				Scope:         celengine.CacheScope{Consumer: consumer, ID: "cb-2", Revision: 1},
				Kind:          ruleengine.KindFilter,
				EnvRef:        ruleengine.EnvironmentRef{Consumer: consumer, Name: "chain_test"},
				CompileBudget: ruleengine.NewBudgetTracker(time.Second),
				OnCompileMiss: func(d string) { observedMiss = append(observedMiss, d) },
			},
		}}}, ruleengine.Input{Activation: map[string]any{"flag": true, "score": int64(7)}}, ruleengine.ChainLimits{}, nil, nil, nil)
		out2, _, _, _ := run2.Next()
		if out2 == nil || out2.Class != ruleengine.StepClassPass {
			t.Fatalf("live CompileBudget run = %+v, want pass", out2)
		}
		if !slices.Equal(observedMiss, []string{ruleengine.DialectCEL}) {
			t.Fatalf("OnCompileMiss observed = %v, want [cel]", observedMiss)
		}
	})
}
