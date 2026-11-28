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
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// TestReviewFix2_RV2_2_ConcurrentNextNoParallelEvaluation (RV2-2, v1 §3.7 #1):
// Concurrent Next() on one run must reject re-entry with an error without
// evaluating the same element twice, charging twice, or dropping element 2.
func TestReviewFix2_RV2_2_ConcurrentNextNoParallelEvaluation(t *testing.T) {
	inl := newCountingFakeIODialect("slow_inline_rv2", time.Second)
	inl.performsIO = false
	inl.setHandler("slow", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		_ = sleepWithCtx(ctx, 25*time.Millisecond)
		return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
	})
	inl.setHandler("deny", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		return ruleengine.Decision{Kind: c.Kind(), Bool: false}, nil
	})
	eng, _ := newChainTestEngine(t, "probe_rv2", inl)
	c1 := mustCompile(t, eng, "probe_rv2", "slow_inline_rv2", "slow", ruleengine.KindFilter)
	c2 := mustCompile(t, eng, "probe_rv2", "slow_inline_rv2", "deny", ruleengine.KindFilter)

	tr := ruleengine.NewBudgetTracker(10 * time.Second)
	dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: tr}}
	run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
		ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c1}, {Index: 2, Compiled: c2}}},
		ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, errs[i] = run.Next()
		}(i)
	}
	wg.Wait()

	res, _ := run.Result()
	idx := make([]int, 0, len(res.Steps))
	for _, s := range res.Steps {
		idx = append(idx, s.Index)
	}
	for i, s := range res.Steps {
		if s.Index != i+1 {
			t.Fatalf("steps out of author order / duplicated / skipped: %v (calls=%d)", idx, inl.callCount())
		}
	}
	if inl.callCount() != int64(len(res.Steps)) {
		t.Fatalf("dialect calls (%d) != recorded steps (%d)", inl.callCount(), len(res.Steps))
	}
	if len(res.Steps) == 1 {
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("expected exactly one concurrent Next to error, got errs=%v", errs)
		}
		if !slices.Equal(res.Unexecuted, []int{2}) {
			t.Fatalf("Unexecuted = %v, want [2]", res.Unexecuted)
		}
		if tr.Spent() != res.Steps[0].Elapsed {
			t.Fatalf("double charge: spent=%s != step.Elapsed=%s", tr.Spent(), res.Steps[0].Elapsed)
		}
	}
}

// TestReviewFix2_RV2_1_ResumeVsCompletionNoDeadlock (RV2-1):
// Concurrent Resume (terminal-cancel branch) racing EvaluatePending and
// EvaluateGroup (including isLive) on a canceled context must never deadlock.
func TestReviewFix2_RV2_1_ResumeVsCompletionNoDeadlock(t *testing.T) {
	io := newCountingFakeIODialect("io_dl_rv2", time.Second)
	eng, _ := newChainTestEngine(t, "probe_dl", io)
	c := mustCompile(t, eng, "probe_dl", "io_dl_rv2", "x", ruleengine.KindFilter)

	t.Run("Resume_vs_EvaluatePending", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		dom := ruleengine.Domain{Ctx: ctx}
		run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, p, _, err := run.Next()
		if err != nil || p == nil {
			t.Fatalf("Next: p=%v err=%v", p, err)
		}
		cancel()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = run.Resume() }()
		go func() { defer wg.Done(); _ = eng.EvaluatePending(p) }()

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("DEADLOCK Resume vs EvaluatePending:\n%s", buf[:n])
		}
		res, _ := run.Result()
		if len(res.Steps) == 0 || res.Steps[0].Class != ruleengine.StepClassCanceled {
			t.Fatalf("expected canceled step recorded, got %+v", res.Steps)
		}
	})

	t.Run("Resume_vs_EvaluateGroup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		dom := ruleengine.Domain{Ctx: ctx}
		in := ruleengine.Input{AdmittedInputKey: "dl-grp"}
		r1, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c, GroupKey: "g"}}},
			in, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		r2, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c, GroupKey: "g"}}},
			in, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		cancel()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = r1.Resume() }()
		go func() { defer wg.Done(); _, _ = eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}) }()

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("DEADLOCK Resume vs EvaluateGroup:\n%s", buf[:n])
		}
	})
}

// TestReviewFix2_Oa_LeaderCanceledAttributesChargeToFirstLiveMember (O-a):
// When the leader is canceled at distribution while a member is live, the
// leader gets canceled with Elapsed=0 and the first live member receives the
// charged Elapsed so tracker.Spent() == Σ StepOutcome.Elapsed.
func TestReviewFix2_Oa_LeaderCanceledAttributesChargeToFirstLiveMember(t *testing.T) {
	io := newCountingFakeIODialect("io_oa", time.Second)
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	io.setHandler("j", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		cancelLeader()
		_ = sleepWithCtx(context.Background(), 20*time.Millisecond)
		return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
	})
	eng, _ := newChainTestEngine(t, "probe_oa", io)
	c := mustCompile(t, eng, "probe_oa", "io_oa", "j", ruleengine.KindFilter)

	tr := ruleengine.NewBudgetTracker(10 * time.Second)
	ioDom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{Tracker: tr}}
	mk := func(inl context.Context) *ruleengine.ChainRun {
		r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: inl}, IO: ioDom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c, GroupKey: "g"}}},
			ruleengine.Input{AdmittedInputKey: "k"}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	rl, rm1, rm2 := mk(leaderCtx), mk(context.Background()), mk(context.Background())
	_, pl, _, _ := rl.Next()
	_, pm1, _, _ := rm1.Next()
	_, pm2, _, _ := rm2.Next()
	if _, err := eng.EvaluateGroup(pl, []*ruleengine.PendingStep{pm1, pm2}); err != nil {
		t.Fatal(err)
	}
	_, _ = rl.Resume()
	_, _ = rm1.Resume()
	_, _ = rm2.Resume()

	resL, _ := rl.Result()
	resM1, _ := rm1.Result()
	resM2, _ := rm2.Result()
	if resL.Deciding.Class != ruleengine.StepClassCanceled || resL.Deciding.Elapsed != 0 {
		t.Fatalf("canceled leader = %+v, want canceled with Elapsed=0", resL.Deciding)
	}
	if resM1.Deciding.Class != ruleengine.StepClassPass || resM1.Deciding.Elapsed <= 0 {
		t.Fatalf("first live member = %+v, want pass with Elapsed > 0", resM1.Deciding)
	}
	if resM2.Deciding.Class != ruleengine.StepClassPass || resM2.Deciding.Elapsed != 0 {
		t.Fatalf("second live member = %+v, want pass with Elapsed=0", resM2.Deciding)
	}
	totalElapsed := resL.Deciding.Elapsed + resM1.Deciding.Elapsed + resM2.Deciding.Elapsed
	if tr.Spent() != totalElapsed {
		t.Fatalf("tracker.Spent() (%s) != sum of Elapsed (%s)", tr.Spent(), totalElapsed)
	}
}

// TestReviewFix2_Ob_BaseActivationRejectsNonCopyableContainers (O-b):
// NewChainRun rejects non-JSON-shaped / non-copyable container kinds in
// base.Activation (such as []map[string]any, map[string]int, pointers, structs).
func TestReviewFix2_Ob_BaseActivationRejectsNonCopyableContainers(t *testing.T) {
	eng, _ := newChainTestEngine(t, "probe_ob")
	c := mustCompile(t, eng, "probe_ob", ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}
	ds := ruleengine.ChainDomains{Inline: dom, IO: dom}

	type customStruct struct{ X int }
	badActivations := []map[string]any{
		{"xs": []map[string]any{{"a": "orig"}}},
		{"m": map[string]int{"k": 1}},
		{"p": new(int)},
		{"s": customStruct{X: 1}},
	}
	for i, act := range badActivations {
		if _, err := eng.NewChainRun(ds, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{Activation: act}, ruleengine.ChainLimits{}, nil, nil, nil); err == nil {
			t.Fatalf("case %d: expected NewChainRun to reject non-copyable base Activation %#v", i, act)
		}
	}
}

// TestReviewFix2_Od_EncodeChainTraceValidatesClassKindVerdict (O-d):
// EncodeChainTrace rejects invalid class, invalid kind, and hostile verdict tokens.
func TestReviewFix2_Od_EncodeChainTraceValidatesClassKindVerdict(t *testing.T) {
	cases := []struct {
		name string
		el   ruleengine.ChainTraceElement
	}{
		{"invalid_class", ruleengine.ChainTraceElement{Index: 1, Dialect: "cel", Class: ruleengine.StepClass("bogus")}},
		{"invalid_kind", ruleengine.ChainTraceElement{Index: 1, Dialect: "cel", Kind: ruleengine.Kind("bogus"), Class: ruleengine.StepClassPass}},
		{"hostile_verdict_spaces", ruleengine.ChainTraceElement{Index: 1, Dialect: "cel", Class: ruleengine.StepClassFail, Verdict: "reject: secret body"}},
		{"hostile_verdict_newline", ruleengine.ChainTraceElement{Index: 1, Dialect: "cel", Class: ruleengine.StepClassFail, Verdict: "reject\nline2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ruleengine.EncodeChainTrace(ruleengine.ChainTrace{Elements: []ruleengine.ChainTraceElement{tc.el}}, nil, 4096); err == nil {
				t.Fatalf("expected EncodeChainTrace to reject %s", tc.name)
			}
		})
	}
}

// TestReviewFix2_Oe_RuntimeProvenanceFilterDropsInvalidInMemory (O-e, kills m19):
// filterValidProvenance drops undeclared and invalid provenance fields at
// runtime before they enter StepOutcome.Provenance or Result()'s in-memory trace.
func TestReviewFix2_Oe_RuntimeProvenanceFilterDropsInvalidInMemory(t *testing.T) {
	io := newCountingFakeIODialect("io_oe", time.Second)
	io.setHandler("prov", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		return ruleengine.Decision{
			Kind: c.Kind(),
			Bool: true,
			Provenance: ruleengine.NewTraceProvenance(map[string]any{
				"outcome":     "ok",                   // declared valid enum
				"secret_msg":  "hunter2 user content", // undeclared key
				"instance_id": "bad id with spaces\n", // declared key, invalid value
			}),
		}, nil
	})
	eng, _ := newChainTestEngine(t, "probe_oe", io)
	c := mustCompile(t, eng, "probe_oe", "io_oe", "prov", ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}
	run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
		ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
		ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, p, _, _ := run.Next()
	if err := eng.EvaluatePending(p); err != nil {
		t.Fatal(err)
	}
	_, _ = run.Resume()
	res, tr := run.Result()
	if len(res.Steps) != 1 || len(tr.Elements) != 1 {
		t.Fatalf("unexpected steps/trace len: %d / %d", len(res.Steps), len(tr.Elements))
	}
	for _, prov := range []map[string]any{res.Steps[0].Provenance, tr.Elements[0].Provenance} {
		if len(prov) != 1 || prov["outcome"] != "ok" {
			t.Fatalf("in-memory provenance leaked undeclared/invalid fields: %#v", prov)
		}
	}
}

// TestReviewFix2_Na_StaleTokenReplayRecordsPendingIndex (N-a):
// Replaying step-1's token while the run is pending on step 2 records
// failed/invalid_completion against step 2 (the currently pending index),
// leaving no duplicate index 1 and no stale index 2 in Unexecuted.
func TestReviewFix2_Na_StaleTokenReplayRecordsPendingIndex(t *testing.T) {
	io := newCountingFakeIODialect("io_na", time.Second)
	eng, _ := newChainTestEngine(t, "probe_na", io)
	c := mustCompile(t, eng, "probe_na", "io_na", "x", ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}
	run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
		ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}, {Index: 2, Compiled: c}}},
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
	if err := eng.EvaluatePending(p1); err == nil {
		t.Fatal("stale token replay accepted")
	}
	before := io.callCount()
	if err := eng.EvaluatePending(p2); err == nil {
		t.Fatal("completion after terminal accepted")
	}
	if io.callCount() != before {
		t.Fatal("I/O after terminal")
	}
	res, _ := run.Result()
	if len(res.Steps) != 2 || res.Steps[0].Index != 1 || res.Steps[1].Index != 2 || res.Steps[1].Reason != ruleengine.ReasonInvalidCompletion {
		t.Fatalf("stale replay steps = %+v, want [1 pass, 2 failed/invalid_completion]", res.Steps)
	}
	if len(res.Unexecuted) != 0 || !res.Stopped {
		t.Fatalf("stale replay unexecuted=%v stopped=%v, want [] / true", res.Unexecuted, res.Stopped)
	}
}
