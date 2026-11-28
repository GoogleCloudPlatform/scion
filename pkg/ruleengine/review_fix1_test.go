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
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// opaqueRep mirrors dialects whose Compiled rep is a pointer to a struct with
// only unexported fields and no declared ProgramID.
type opaqueRep struct{ rule string }

type opaqueIODialect struct {
	name  string
	calls atomic.Int64
	delay time.Duration
}

func (d *opaqueIODialect) Name() string { return d.name }

func (d *opaqueIODialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: time.Second,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter, ruleengine.KindJudgment},
	}
}

func (d *opaqueIODialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, &opaqueRep{rule: rule}), nil
}

func (d *opaqueIODialect) Evaluate(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	d.calls.Add(1)
	if d.delay > 0 {
		t := time.NewTimer(d.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ruleengine.Decision{}, ctx.Err()
		}
	}
	rep := c.Rep().(*opaqueRep)
	return ruleengine.Decision{Kind: c.Kind(), Bool: rep.rule == "allow_A"}, nil
}

func probeEngine(t *testing.T, consumer string, d ruleengine.Dialect) *ruleengine.Engine {
	t.Helper()
	eng := ruleengine.NewEngine()
	if err := eng.Register(d); err != nil {
		t.Fatal(err)
	}
	if err := eng.AllowConsumerDialects(consumer, ruleengine.GrantIO(d.Name())); err != nil {
		t.Fatal(err)
	}
	return eng
}

func oneElemRun(t *testing.T, eng *ruleengine.Engine, d ruleengine.ChainDomains, c ruleengine.Compiled, gk string, in ruleengine.Input) (*ruleengine.ChainRun, *ruleengine.PendingStep) {
	t.Helper()
	r, err := eng.NewChainRun(d, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c, GroupKey: gk}}}, in, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, p, _, err := r.Next()
	if err != nil || p == nil {
		t.Fatalf("Next: p=%v err=%v", p, err)
	}
	return r, p
}

// TestReviewFix1_R1_SafetyFloor_OpaqueRepDistinctPrograms (R-1):
// Two distinct opaque-rep programs without ProgramID under the same GroupKey
// must fail closed with incompatible_group and 0 calls; the exact same rep
// pointer still groups.
func TestReviewFix1_R1_SafetyFloor_OpaqueRepDistinctPrograms(t *testing.T) {
	const consumer = "probe"
	d := &opaqueIODialect{name: "opaque_io"}
	eng := probeEngine(t, consumer, d)
	env := ruleengine.EnvironmentRef{Consumer: consumer, Name: "x"}
	cA, _ := eng.Compile(context.Background(), "opaque_io", "allow_A", ruleengine.KindFilter, env)
	cB, _ := eng.Compile(context.Background(), "opaque_io", "deny_B", ruleengine.KindFilter, env)
	dom := ruleengine.Domain{Ctx: context.Background()}
	ds := ruleengine.ChainDomains{Inline: dom, IO: dom}
	in := ruleengine.Input{AdmittedInputKey: "same"}

	// Distinct opaque-rep programs must fail closed.
	rA, pA := oneElemRun(t, eng, ds, cA, "gk", in)
	rB, pB := oneElemRun(t, eng, ds, cB, "gk", in)
	_, err := eng.EvaluateGroup(pA, []*ruleengine.PendingStep{pB})
	_, _ = rA.Resume()
	_, _ = rB.Resume()
	resB, _ := rB.Result()
	if err == nil || d.calls.Load() != 0 || resB.Deciding.Reason != ruleengine.ReasonIncompatibleGroup {
		t.Fatalf("safety floor accepted two different opaque-rep programs: err=%v calls=%d memberB=%s/%s",
			err, d.calls.Load(), resB.Deciding.Class, resB.Deciding.Reason)
	}

	// Identical rep pointer still groups with 1 call.
	rSame1, pSame1 := oneElemRun(t, eng, ds, cA, "gk", in)
	rSame2, pSame2 := oneElemRun(t, eng, ds, cA, "gk", in)
	if _, err := eng.EvaluateGroup(pSame1, []*ruleengine.PendingStep{pSame2}); err != nil {
		t.Fatalf("identical rep pointer failed to group: %v", err)
	}
	_, _ = rSame1.Resume()
	_, _ = rSame2.Resume()
	if d.calls.Load() != 1 {
		t.Fatalf("identical rep pointer calls = %d, want 1", d.calls.Load())
	}
}

// TestReviewFix1_R1_SafetyFloor_AdmittedInputHashMarshalFailure (R-1):
// An unmarshalable activation (e.g. NaN) with differing values must fail closed
// with incompatible_group and 0 calls.
func TestReviewFix1_R1_SafetyFloor_AdmittedInputHashMarshalFailure(t *testing.T) {
	const consumer = "probe"
	d := &opaqueIODialect{name: "opaque_io2"}
	eng := probeEngine(t, consumer, d)
	env := ruleengine.EnvironmentRef{Consumer: consumer, Name: "x"}
	c, _ := eng.Compile(context.Background(), "opaque_io2", "allow_A", ruleengine.KindFilter, env)
	dom := ruleengine.Domain{Ctx: context.Background()}
	ds := ruleengine.ChainDomains{Inline: dom, IO: dom}
	r1, p1 := oneElemRun(t, eng, ds, c, "gk", ruleengine.Input{Activation: map[string]any{"nan": math.NaN(), "recipient": "alice"}})
	r2, p2 := oneElemRun(t, eng, ds, c, "gk", ruleengine.Input{Activation: map[string]any{"nan": math.NaN(), "recipient": "mallory"}})
	_, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
	_, _ = r1.Resume()
	_, _ = r2.Resume()
	if err == nil || d.calls.Load() != 0 {
		t.Fatalf("safety floor accepted different unmarshalable admitted inputs: err=%v calls=%d", err, d.calls.Load())
	}
}

// TestReviewFix1_R2_GroupInlineCancelPerMember (R-2):
// Canceling one member's Inline ctx mid-flight or at the barrier cancels only
// that member while the other member receives the shared outcome (1 call);
// canceling ALL members' Inline ctxs cancels the group (0 calls at barrier).
func TestReviewFix1_R2_GroupInlineCancelPerMember(t *testing.T) {
	const consumer = "probe"
	d := &opaqueIODialect{name: "opaque_io3", delay: 120 * time.Millisecond}
	eng := probeEngine(t, consumer, d)
	env := ruleengine.EnvironmentRef{Consumer: consumer, Name: "x"}
	c, _ := eng.Compile(context.Background(), "opaque_io3", "allow_A", ruleengine.KindFilter, env)
	ioCtx := context.Background()
	io := ruleengine.Domain{Ctx: ioCtx}

	t.Run("cancel_one_mid_flight", func(t *testing.T) {
		before := d.calls.Load()
		inl1, cancel1 := context.WithCancel(context.Background())
		defer cancel1()
		r1, p1 := oneElemRun(t, eng, ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: inl1}, IO: io}, c, "gk", ruleengine.Input{AdmittedInputKey: "k"})
		r2, p2 := oneElemRun(t, eng, ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: context.Background()}, IO: io}, c, "gk", ruleengine.Input{AdmittedInputKey: "k"})
		go func() { time.Sleep(25 * time.Millisecond); cancel1() }()
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatal(err)
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if d.calls.Load()-before != 1 {
			t.Fatalf("calls = %d, want 1", d.calls.Load()-before)
		}
		if res2.Deciding.Class != ruleengine.StepClassPass {
			t.Fatalf("run 2 (live Inline and IO ctx) got %q, want pass", res2.Deciding.Class)
		}
		if res1.Deciding.Class != ruleengine.StepClassCanceled {
			t.Fatalf("run 1 (Inline canceled) got %q, want canceled", res1.Deciding.Class)
		}
	})

	t.Run("cancel_one_at_barrier", func(t *testing.T) {
		before := d.calls.Load()
		inl1, cancel1 := context.WithCancel(context.Background())
		r1, p1 := oneElemRun(t, eng, ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: inl1}, IO: io}, c, "gk", ruleengine.Input{AdmittedInputKey: "k"})
		r2, p2 := oneElemRun(t, eng, ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: context.Background()}, IO: io}, c, "gk", ruleengine.Input{AdmittedInputKey: "k"})
		cancel1()
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatal(err)
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if d.calls.Load()-before != 1 {
			t.Fatalf("calls = %d, want 1", d.calls.Load()-before)
		}
		if res2.Deciding.Class != ruleengine.StepClassPass {
			t.Fatalf("run 2 (live) got %q, want pass", res2.Deciding.Class)
		}
		if res1.Deciding.Class != ruleengine.StepClassCanceled {
			t.Fatalf("run 1 (canceled at barrier) got %q, want canceled", res1.Deciding.Class)
		}
	})

	t.Run("cancel_all_mid_flight_cascades_to_eval", func(t *testing.T) {
		inl1, cancel1 := context.WithCancel(context.Background())
		inl2, cancel2 := context.WithCancel(context.Background())
		defer cancel1()
		defer cancel2()
		r1, p1 := oneElemRun(t, eng, ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: inl1}, IO: io}, c, "gk", ruleengine.Input{AdmittedInputKey: "k"})
		r2, p2 := oneElemRun(t, eng, ruleengine.ChainDomains{Inline: ruleengine.Domain{Ctx: inl2}, IO: io}, c, "gk", ruleengine.Input{AdmittedInputKey: "k"})
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel1()
			cancel2()
		}()
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatal(err)
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if res1.Deciding.Class != ruleengine.StepClassCanceled || res2.Deciding.Class != ruleengine.StepClassCanceled {
			t.Fatalf("all-canceled mid-flight = (%s, %s), want (canceled, canceled)", res1.Deciding.Class, res2.Deciding.Class)
		}
	})
}

// TestReviewFix1_O1_ReplayBeforeResumeKeepsRecordedOutcome (O-1):
// A replayed token after completion (before Resume) must be rejected without
// suppressing the engine-recorded outcome.
func TestReviewFix1_O1_ReplayBeforeResumeKeepsRecordedOutcome(t *testing.T) {
	const consumer = "probe"
	d := &opaqueIODialect{name: "opaque_io4", delay: 200 * time.Millisecond}
	eng := probeEngine(t, consumer, d)
	env := ruleengine.EnvironmentRef{Consumer: consumer, Name: "x"}
	c, _ := eng.Compile(context.Background(), "opaque_io4", "allow_A", ruleengine.KindFilter, env)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	dom := ruleengine.Domain{Ctx: ctx}
	r, p := oneElemRun(t, eng, ruleengine.ChainDomains{Inline: dom, IO: dom}, c, "", ruleengine.Input{})
	if err := eng.EvaluatePending(p); err != nil {
		t.Fatal(err)
	}
	if err := eng.EvaluatePending(p); err == nil {
		t.Fatal("replay accepted")
	}
	_, _ = r.Resume()
	res, _ := r.Result()
	for _, s := range res.Steps {
		if s.Class == ruleengine.StepClassCanceled {
			return
		}
	}
	t.Fatalf("recorded canceled outcome was suppressed by the replay; steps=%+v", res.Steps)
}

type detailWithPrivate struct {
	Public  map[string]string
	private map[string]string
	self    *detailWithPrivate
}

type judgNoCopierDialect struct{ name string }

func (d *judgNoCopierDialect) Name() string { return d.name }
func (d *judgNoCopierDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: time.Second,
		ResultKinds: []ruleengine.Kind{ruleengine.KindJudgment},
	}
}
func (d *judgNoCopierDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiledWithIdentity(env.Consumer, d.name, kind, "id:"+rule, rule), nil
}
func (d *judgNoCopierDialect) Evaluate(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	return ruleengine.Decision{Kind: c.Kind(), Detail: &detailWithPrivate{
		Public: map[string]string{"k": "v"}, private: map[string]string{"k": "v"},
	}}, nil
}

// TestReviewFix1_R3_KindJudgmentRequiresCopyDetail (R-3 option b):
// NewChainRun must reject a KindJudgment element whose dialect declares no
// Capabilities.CopyDetail.
func TestReviewFix1_R3_KindJudgmentRequiresCopyDetail(t *testing.T) {
	const consumer = "probe"
	d := &judgNoCopierDialect{name: "judg_no_copier"}
	eng := probeEngine(t, consumer, d)
	env := ruleengine.EnvironmentRef{Consumer: consumer, Name: "x"}
	c, _ := eng.Compile(context.Background(), "judg_no_copier", "J", ruleengine.KindJudgment, env)
	dom := ruleengine.Domain{Ctx: context.Background()}
	_, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom}, ruleengine.Chain{
		Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}},
	}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err == nil {
		t.Fatalf("expected NewChainRun to fail closed when KindJudgment dialect declares no CopyDetail")
	}
}

type judgWithCopierDialect struct {
	name         string
	selfRefCycle bool
}

func (d *judgWithCopierDialect) Name() string { return d.name }
func (d *judgWithCopierDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: time.Second,
		ResultKinds: []ruleengine.Kind{ruleengine.KindJudgment},
		CopyDetail: func(v any) any {
			src := v.(*detailWithPrivate)
			pub := make(map[string]string, len(src.Public))
			for k, val := range src.Public {
				pub[k] = val
			}
			priv := make(map[string]string, len(src.private))
			for k, val := range src.private {
				priv[k] = val
			}
			cp := &detailWithPrivate{Public: pub, private: priv}
			if src.self == src {
				cp.self = cp // preserve self-referential cycle without recursion
			}
			return cp
		},
	}
}
func (d *judgWithCopierDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiledWithIdentity(env.Consumer, d.name, kind, "id:"+rule, rule), nil
}
func (d *judgWithCopierDialect) Evaluate(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	det := &detailWithPrivate{
		Public:  map[string]string{"k": "v"},
		private: map[string]string{"k": "v"},
	}
	if d.selfRefCycle {
		det.self = det
	}
	return ruleengine.Decision{Kind: c.Kind(), Detail: det}, nil
}

// TestReviewFix1_R3_WithCopyDetail_UnexportedFieldsAndCycle (R-3):
// With a dialect-declared CopyDetail, unexported fields and self-referential
// details copy cleanly without crashing or aliasing across runs/members/views.
func TestReviewFix1_R3_WithCopyDetail_UnexportedFieldsAndCycle(t *testing.T) {
	const consumer = "probe"
	d := &judgWithCopierDialect{name: "judg_with_copier", selfRefCycle: true}
	eng := probeEngine(t, consumer, d)
	env := ruleengine.EnvironmentRef{Consumer: consumer, Name: "x"}
	c, _ := eng.Compile(context.Background(), "judg_with_copier", "J", ruleengine.KindJudgment, env)
	dom := ruleengine.Domain{Ctx: context.Background()}
	ds := ruleengine.ChainDomains{Inline: dom, IO: dom}

	r1, p1 := oneElemRun(t, eng, ds, c, "gk", ruleengine.Input{AdmittedInputKey: "k"})
	r2, p2 := oneElemRun(t, eng, ds, c, "gk", ruleengine.Input{AdmittedInputKey: "k"})
	if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
		t.Fatal(err)
	}
	_, _ = r1.Resume()
	_, _ = r2.Resume()

	j1, _ := r1.Judgments().Get(1)
	j2, _ := r2.Judgments().Get(1)
	d1 := j1.Detail.(*detailWithPrivate)
	d2 := j2.Detail.(*detailWithPrivate)
	if d1.self != d1 || d2.self != d2 || d1 == d2 {
		t.Fatalf("self-referential copy broken or aliased")
	}
	d1.Public["k"] = "mutated"
	d1.private["k"] = "mutated"

	j1b, _ := r1.Judgments().Get(1)
	j2b, _ := r2.Judgments().Get(1)
	if j1b.Detail.(*detailWithPrivate).private["k"] != "v" || j2b.Detail.(*detailWithPrivate).private["k"] != "v" {
		t.Fatalf("unexported map aliased across JudgmentView/runs")
	}
}

// TestReviewFix1_O4_EncodeChainTrace_ErrorsWhenMinimalExceedsMaxBytes (O-4):
// EncodeChainTrace must return an error instead of dropping elements when even
// minimal {i, dialect, class} metadata cannot fit in maxBytes.
func TestReviewFix1_O4_EncodeChainTrace_ErrorsWhenMinimalExceedsMaxBytes(t *testing.T) {
	tr := ruleengine.ChainTrace{
		Elements: []ruleengine.ChainTraceElement{
			{Index: 1, Dialect: "cel", Class: ruleengine.StepClassPass},
			{Index: 2, Dialect: "cel", Class: ruleengine.StepClassFail},
		},
	}
	if _, err := ruleengine.EncodeChainTrace(tr, nil, 20); err == nil {
		t.Fatalf("expected error when minimal trace cannot fit in maxBytes=20")
	}
}

// TestReviewFix1_N2_StrictIncreasingIndexes (N-2):
// NewChainRun rejects duplicate, non-increasing, or negative element indexes.
func TestReviewFix1_N2_StrictIncreasingIndexes(t *testing.T) {
	const consumer = "probe_n2"
	eng, _ := newChainTestEngine(t, consumer)
	c := mustCompile(t, eng, consumer, ruleengine.DialectCEL, "flag == true", ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}
	ds := ruleengine.ChainDomains{Inline: dom, IO: dom}

	badChains := [][]ruleengine.ChainElement{
		{{Index: -1, Compiled: c}},
		{{Index: 1, Compiled: c}, {Index: 1, Compiled: c}},
		{{Index: 2, Compiled: c}, {Index: 1, Compiled: c}},
		{{Index: 0, Compiled: c}, {Index: 0, Compiled: c}},
	}
	for i, elems := range badChains {
		if _, err := eng.NewChainRun(ds, ruleengine.Chain{Elements: elems}, ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil); err == nil {
			t.Fatalf("case %d: expected NewChainRun to reject non-strictly-increasing/negative indexes", i)
		}
	}
}
