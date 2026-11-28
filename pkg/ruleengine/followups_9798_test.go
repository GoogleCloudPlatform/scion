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
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

type unexpChainAddition struct {
	secret string
}

// TestSecS2L1_ChainAdditionsRejectNonJSONShapedAndDistinguishBytesFromBase64
// (ptone/scion#9798, SEC-S2-L1):
//   - Struct with unexported fields in chain.* is rejected as invalid_enrichment
//     (never false-grouped under a shared AdmittedInputKey, 0 IO calls, 0 false pass).
//   - []byte{1, 2} vs base64 string "AQI=" in chain.* must NOT hash identically
//     and must fail closed (incompatible_group, 0 IO calls, 0 false pass).
func TestSecS2L1_ChainAdditionsRejectNonJSONShapedAndDistinguishBytesFromBase64(t *testing.T) {
	t.Run("struct_with_unexported_fields_rejected_no_false_pass", func(t *testing.T) {
		cons := "sec_s2_l1_struct"
		io := newCountingFakeIODialect("io_s2_l1_struct", time.Second)
		eng, _ := newChainTestEngine(t, cons, io)
		cIO := mustCompile(t, eng, cons, "io_s2_l1_struct", "x", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		d := ruleengine.ChainDomains{Inline: dom, IO: dom}

		io.setHandler("x", func(_ context.Context, _ ruleengine.Compiled, in ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			ch, _ := in.Activation["chain"].(map[string]any)
			u, _ := ch["v"].(unexpChainAddition)
			return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: u.secret == "allow"}, nil
		})

		mk := func(secret string) *ruleengine.ChainRun {
			enr := funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
				return map[string]any{"chain.v": unexpChainAddition{secret: secret}}, nil
			})
			r, err := eng.NewChainRun(d,
				ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO, GroupKey: "g"}}},
				ruleengine.Input{Activation: map[string]any{"flag": true}, AdmittedInputKey: "same"},
				ruleengine.ChainLimits{}, nil, enr, nil)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}

		r1, r2 := mk("allow"), mk("deny")
		out1, p1, done1, err1 := r1.Next()
		out2, p2, done2, err2 := r2.Next()

		// Non-JSON-shaped struct in chain.* must fail closed at Next() with
		// invalid_enrichment (never yielding a PendingStep to EvaluateGroup).
		if err1 != nil || err2 != nil {
			t.Fatalf("Next returned unexpected errors: err1=%v err2=%v", err1, err2)
		}
		if p1 != nil || p2 != nil {
			// If it unexpectedly yielded pending steps, prove whether EvaluateGroup false-passes.
			gid, gerr := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
			_, _ = r1.Resume()
			_, _ = r2.Resume()
			res2, _ := r2.Result()
			t.Fatalf("SEC-S2-L1: non-JSON-shaped struct in chain.* was not rejected at enrichment; EvaluateGroup gid=%q err=%v calls=%d r2=%+v",
				gid, gerr, io.callCount(), res2.Steps)
		}
		if !done1 || !done2 || out1 == nil || out2 == nil ||
			out1.Class != ruleengine.StepClassFailed || out1.Reason != ruleengine.ReasonInvalidEnrichment ||
			out2.Class != ruleengine.StepClassFailed || out2.Reason != ruleengine.ReasonInvalidEnrichment {
			t.Fatalf("expected failed/invalid_enrichment for both runs; out1=%+v done1=%v out2=%+v done2=%v", out1, done1, out2, done2)
		}
		if io.callCount() != 0 {
			t.Fatalf("expected 0 IO calls, got %d", io.callCount())
		}
	})

	t.Run("reserved_chain_map_with_unexported_struct_rejected", func(t *testing.T) {
		cons := "sec_s2_l1_chain_map"
		io := newCountingFakeIODialect("io_s2_l1_chain_map", time.Second)
		eng, _ := newChainTestEngine(t, cons, io)
		cIO := mustCompile(t, eng, cons, "io_s2_l1_chain_map", "x", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		enr := funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
			return map[string]any{"chain": map[string]any{"v": unexpChainAddition{secret: "x"}}}, nil
		})
		r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO, GroupKey: "g"}}},
			ruleengine.Input{Activation: map[string]any{"flag": true}, AdmittedInputKey: "same"},
			ruleengine.ChainLimits{}, nil, enr, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, p, done, err := r.Next()
		if err != nil || p != nil || !done || out == nil ||
			out.Class != ruleengine.StepClassFailed || out.Reason != ruleengine.ReasonInvalidEnrichment {
			t.Fatalf("expected failed/invalid_enrichment for reserved chain map with unexported struct; out=%+v p=%v done=%v err=%v", out, p, done, err)
		}
		if io.callCount() != 0 {
			t.Fatalf("expected 0 IO calls, got %d", io.callCount())
		}
	})

	t.Run("bytes_vs_base64_string_no_false_grouping", func(t *testing.T) {
		cons := "sec_s2_l1_bytes"
		io := newCountingFakeIODialect("io_s2_l1_bytes", time.Second)
		eng, _ := newChainTestEngine(t, cons, io)
		cIO := mustCompile(t, eng, cons, "io_s2_l1_bytes", "x", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		d := ruleengine.ChainDomains{Inline: dom, IO: dom}

		io.setHandler("x", func(_ context.Context, _ ruleengine.Compiled, in ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			ch, _ := in.Activation["chain"].(map[string]any)
			_, isString := ch["v"].(string)
			return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: isString}, nil
		})

		mk := func(val any) *ruleengine.ChainRun {
			enr := funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
				return map[string]any{"chain.v": val}, nil
			})
			r, err := eng.NewChainRun(d,
				ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO, GroupKey: "g"}}},
				ruleengine.Input{Activation: map[string]any{"flag": true}, AdmittedInputKey: "same"},
				ruleengine.ChainLimits{}, nil, enr, nil)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}

		// "AQI=" is base64([]byte{1, 2}); json.Marshal produces identical bytes for both.
		r1, r2 := mk("AQI="), mk([]byte{1, 2})
		_, p1, _, err1 := r1.Next()
		_, p2, _, err2 := r2.Next()
		if err1 != nil || err2 != nil {
			t.Fatalf("Next: err1=%v err2=%v", err1, err2)
		}
		if p1 == nil || p2 == nil {
			// If []byte is rejected as invalid_enrichment, that is also fail-closed (0 IO calls).
			if io.callCount() != 0 {
				t.Fatalf("expected 0 IO calls, got %d", io.callCount())
			}
			return
		}
		gid, gerr := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res2, _ := r2.Result()
		if gerr == nil {
			t.Fatalf("SEC-S2-L1: []byte{1,2} and base64 string \"AQI=\" grouped under same AdmittedInputKey (gid=%q calls=%d r2=%+v)",
				gid, io.callCount(), res2.Steps)
		}
		var fe *ruleengine.FailedError
		if !errors.As(gerr, &fe) || fe.Reason != ruleengine.ReasonIncompatibleGroup {
			t.Fatalf("expected incompatible_group error, got %v", gerr)
		}
		if io.callCount() != 0 {
			t.Fatalf("expected 0 IO calls, got %d", io.callCount())
		}
		if len(res2.Steps) != 1 || res2.Steps[0].Class != ruleengine.StepClassFailed || res2.Steps[0].Reason != ruleengine.ReasonIncompatibleGroup {
			t.Fatalf("r2 must fail closed with incompatible_group, got %+v", res2.Steps)
		}
	})
}

type panickingSourceDialect struct {
	name           string
	panicOnCompile bool
	panicOnEval    bool
	delay          time.Duration
}

func (d *panickingSourceDialect) Name() string { return d.name }
func (d *panickingSourceDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  false,
		MaxEvalTime: time.Second,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *panickingSourceDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	if d.delay > 0 {
		time.Sleep(d.delay)
	}
	if d.panicOnCompile {
		panic("SECRET_COMPILE_PANIC_TEXT")
	}
	return ruleengine.NewCompiledWithIdentity(env.Consumer, d.name, kind, "id:"+rule, rule), nil
}
func (d *panickingSourceDialect) Evaluate(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	if d.delay > 0 {
		time.Sleep(d.delay)
	}
	if d.panicOnEval {
		panic("SECRET_EVAL_PANIC_TEXT")
	}
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

// TestA1A2_SourceFormInlinePanicRecoveredChargedAndNeverStuckEvaluating
// (ptone/scion#9798, A-1 & A-2):
//   - A panic in Source-form inline Compile or Evaluate is recovered inside
//     Next(), records StepClassFailed / ReasonEvalFailed, transitions the run
//     to Done (never stuck Evaluating), never leaks panic text into ChainTrace,
//     and charges the elapsed time to Inline.Budget.Tracker.
//   - A panic in EvaluatePending charges the elapsed time to IO.Budget.Tracker.
func TestA1A2_SourceFormInlinePanicRecoveredChargedAndNeverStuckEvaluating(t *testing.T) {
	for _, tc := range []struct {
		name           string
		panicOnCompile bool
		panicOnEval    bool
		secret         string
	}{
		{name: "source_compile_panic", panicOnCompile: true, secret: "SECRET_COMPILE_PANIC_TEXT"},
		{name: "source_eval_panic", panicOnEval: true, secret: "SECRET_EVAL_PANIC_TEXT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &panickingSourceDialect{
				name:           "src_panic_" + tc.name,
				panicOnCompile: tc.panicOnCompile,
				panicOnEval:    tc.panicOnEval,
				delay:          5 * time.Millisecond,
			}
			eng, _ := newChainTestEngine(t, "c_a1_"+tc.name, d)
			tr := ruleengine.NewBudgetTracker(5 * time.Second)
			dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second, Tracker: tr}}
			run, err := eng.NewChainRun(
				ruleengine.ChainDomains{Inline: dom, IO: dom},
				ruleengine.Chain{Elements: []ruleengine.ChainElement{{
					Index:   1,
					Dialect: d.name,
					Kind:    ruleengine.KindFilter,
					Source: &ruleengine.ChainElementSource{
						Expr:   "true",
						Scope:  celengine.CacheScope{Consumer: "c_a1_" + tc.name, ID: "r1"},
						Kind:   ruleengine.KindFilter,
						EnvRef: ruleengine.EnvironmentRef{Consumer: "c_a1_" + tc.name, Name: "e"},
					},
				}}},
				ruleengine.Input{Activation: map[string]any{}},
				ruleengine.ChainLimits{MaxElements: 4, MaxTraceBytes: 4096},
				nil, nil, nil,
			)
			if err != nil {
				t.Fatalf("NewChainRun: %v", err)
			}

			out, p, done, err := run.Next()
			if err != nil || p != nil || !done || out == nil {
				t.Fatalf("Next() = (out=%+v, p=%v, done=%v, err=%v), want failed/eval_failed & done=true", out, p, done, err)
			}
			if out.Class != ruleengine.StepClassFailed || out.Reason != ruleengine.ReasonEvalFailed {
				t.Fatalf("outcome = %+v, want failed/eval_failed", out)
			}
			// Run must be Done (never stuck Evaluating): follow-up Next returns done=true, err=nil.
			if _, _, done2, err2 := run.Next(); !done2 || err2 != nil {
				t.Fatalf("run stuck after source panic: Next() = (done=%v, err=%v)", done2, err2)
			}
			// A-2: elapsed time before panic must be charged to tracker.
			if tr.Spent() < 3*time.Millisecond {
				t.Fatalf("panic elapsed not charged to Inline tracker: spent=%s", tr.Spent())
			}
			res, trace := run.Result()
			if !res.Stopped || len(res.Steps) != 1 || res.Steps[0].Elapsed < 3*time.Millisecond {
				t.Fatalf("unexpected result after source panic: %+v", res)
			}
			raw, err := ruleengine.EncodeChainTrace(trace, map[string]ruleengine.Capabilities{d.name: d.Declares()}, 4096)
			if err != nil {
				t.Fatalf("EncodeChainTrace: %v", err)
			}
			if strings.Contains(raw, tc.secret) {
				t.Fatalf("EncodeChainTrace leaked panic text: %s", raw)
			}
		})
	}

	t.Run("evaluate_pending_panic_charges_io_tracker", func(t *testing.T) {
		io := newCountingFakeIODialect("io_a2_charge", time.Second)
		io.setHandler("boom", func(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			time.Sleep(5 * time.Millisecond)
			panic("SECRET_IO_PANIC_TEXT")
		})
		eng, _ := newChainTestEngine(t, "c_a2_io", io)
		c := mustCompile(t, eng, "c_a2_io", "io_a2_charge", "boom", ruleengine.KindFilter)
		ioTr := ruleengine.NewBudgetTracker(5 * time.Second)
		ioDom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second, Tracker: ioTr}}
		inDom := ruleengine.Domain{Ctx: context.Background()}
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
		if ioTr.Spent() < 3*time.Millisecond {
			t.Fatalf("IO panic elapsed not charged to IO tracker: spent=%s", ioTr.Spent())
		}
		res, trace := r.Result()
		if len(res.Steps) != 1 || res.Steps[0].Elapsed < 3*time.Millisecond {
			t.Fatalf("unexpected step elapsed on IO panic: %+v", res.Steps)
		}
		raw, err := ruleengine.EncodeChainTrace(trace, map[string]ruleengine.Capabilities{io.Name(): io.Declares()}, 4096)
		if err != nil {
			t.Fatalf("EncodeChainTrace: %v", err)
		}
		if strings.Contains(raw, "SECRET_IO_PANIC_TEXT") {
			t.Fatalf("EncodeChainTrace leaked IO panic text: %s", raw)
		}
	})
}

// TestRev9798Probe_P1_TimeZoneFalseGrouping (ptone/scion#9798, R-1):
// Same instant in different time zones (or same offset with different zone
// names) in chain.* under a shared AdmittedInputKey must NOT group.
func TestRev9798Probe_P1_TimeZoneFalseGrouping(t *testing.T) {
	const cons = "rev9798_tz"
	io := newCountingFakeIODialect("io_rev9798_tz", time.Second)
	eng, _ := newChainTestEngine(t, cons, io)
	cIO := mustCompile(t, eng, cons, "io_rev9798_tz", "x", ruleengine.KindFilter)
	io.setHandler("x", func(_ context.Context, _ ruleengine.Compiled, in ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		ch, _ := in.Activation["chain"].(map[string]any)
		ts, _ := ch["ts"].(time.Time)
		return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: ts.Hour() < 3}, nil
	})
	dom := ruleengine.Domain{Ctx: context.Background()}
	instant := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(loc *time.Location) *ruleengine.ChainRun {
		enr := funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
			return map[string]any{"chain.ts": instant.In(loc)}, nil
		})
		r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO, GroupKey: "g"}}},
			ruleengine.Input{Activation: map[string]any{"flag": true}, AdmittedInputKey: "same"},
			ruleengine.ChainLimits{}, nil, enr, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("different_offset_same_instant", func(t *testing.T) {
		calls0 := io.callCount()
		r1, r2 := mk(time.UTC), mk(time.FixedZone("plus5", 5*3600))
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		if p1 == nil || p2 == nil {
			t.Fatalf("expected pending steps")
		}
		_, gerr := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res2, _ := r2.Result()
		if gerr == nil {
			t.Fatalf("FALSE GROUPING: same-instant/different-zone chain.ts grouped; calls=%d r2 steps=%+v", io.callCount()-calls0, res2.Steps)
		}
		var fe *ruleengine.FailedError
		if !errors.As(gerr, &fe) || fe.Reason != ruleengine.ReasonIncompatibleGroup {
			t.Fatalf("expected incompatible_group, got %v", gerr)
		}
		if io.callCount() != calls0 {
			t.Fatalf("expected 0 IO calls, got %d", io.callCount()-calls0)
		}
	})

	t.Run("same_offset_different_zone_name", func(t *testing.T) {
		calls0 := io.callCount()
		r1, r2 := mk(time.FixedZone("ZoneA", 3600)), mk(time.FixedZone("ZoneB", 3600))
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		_, gerr := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		var fe *ruleengine.FailedError
		if !errors.As(gerr, &fe) || fe.Reason != ruleengine.ReasonIncompatibleGroup {
			t.Fatalf("expected incompatible_group for distinct zone names with same offset, got %v", gerr)
		}
		if io.callCount() != calls0 {
			t.Fatalf("expected 0 IO calls, got %d", io.callCount()-calls0)
		}
	})
}

// TestO1_CanonicalAdditionEncodingTable (ptone/scion#9798, O-1):
// Directly pins encodeCanonicalAdditionValue invariants: type tags (bytes vs
// string, int64 vs int), deterministic map key ordering, string length-prefix
// injection immunity, sub-second time precision, and full time zone identity.
func TestO1_CanonicalAdditionEncodingTable(t *testing.T) {
	mustEncode := func(t *testing.T, v any) []byte {
		t.Helper()
		b, err := ruleengine.HookCanonicalAdditions(v)
		if err != nil {
			t.Fatalf("HookCanonicalAdditions(%#v): %v", v, err)
		}
		return b
	}

	distinctCases := []struct {
		name string
		a, b any
	}{
		{
			name: "bytes_vs_identical_string_k10",
			a:    []byte("AQI="),
			b:    "AQI=",
		},
		{
			name: "bytes_vs_base64_string_k10",
			a:    []byte{1, 2},
			b:    "AQI=",
		},
		{
			name: "string_length_prefix_injection_k13",
			a:    []any{"a;s:b", "c"},
			b:    []any{"a", "b;s:c"},
		},
		{
			name: "string_length_prefix_embedded_tag_k13",
			a:    []any{"a;s:1:b", "c"},
			b:    []any{"a", "b;s:1:c"},
		},
		{
			name: "time_subsecond_precision_k16",
			a:    time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC),
			b:    time.Date(2026, 1, 1, 0, 0, 0, 2, time.UTC),
		},
		{
			name: "int64_vs_int_tag_k17",
			a:    int64(42),
			b:    int(42),
		},
		{
			name: "time_same_instant_different_offset_r1",
			a:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			b:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(time.FixedZone("plus5", 5*3600)),
		},
		{
			name: "time_same_offset_different_zone_name_r1",
			a:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(time.FixedZone("ZoneA", 3600)),
			b:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(time.FixedZone("ZoneB", 3600)),
		},
	}

	for _, tc := range distinctCases {
		t.Run(tc.name, func(t *testing.T) {
			ea := mustEncode(t, tc.a)
			eb := mustEncode(t, tc.b)
			if bytes.Equal(ea, eb) {
				t.Fatalf("expected distinct canonical encodings for %#v vs %#v, both encoded to %q", tc.a, tc.b, ea)
			}
		})
	}

	t.Run("map_key_order_deterministic_k12", func(t *testing.T) {
		m := map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8}
		ref := mustEncode(t, m)
		for i := range 50 {
			got := mustEncode(t, m)
			if !bytes.Equal(got, ref) {
				t.Fatalf("non-deterministic map encoding at iter %d:\ngot:  %q\nwant: %q", i, got, ref)
			}
		}
	})

	t.Run("same_instant_and_zone_identical", func(t *testing.T) {
		loc := time.FixedZone("ZoneA", 3600)
		t1 := time.Date(2026, 1, 1, 1, 0, 0, 123, loc)
		t2 := time.Date(2026, 1, 1, 0, 0, 0, 123, time.UTC).In(loc)
		if !bytes.Equal(mustEncode(t, t1), mustEncode(t, t2)) {
			t.Fatalf("same instant in identical zone should encode identically")
		}
	})
}

// TestO2O3_SeparateCompileBudgetPanicAndSentinelCause (ptone/scion#9798, O-2 & O-3):
//   - O-2: A compile panic on a Source-form element with CompileBudget / OnCompileMiss
//     charges CompileBudget, invokes OnCompileMiss, does NOT charge Inline.Budget.Tracker,
//     and transitions the run to Done with StepClassFailed / ReasonEvalFailed.
//   - O-2: An eval panic after compile succeeds on that same path still charges
//     Inline.Budget.Tracker.
//   - O-3: Recovered panics attach the fixed sentinel cause ("dialect panic")
//     without embedding the panic payload in FailedError.Cause.
func TestO2O3_SeparateCompileBudgetPanicAndSentinelCause(t *testing.T) {
	t.Run("source_compile_panic_charges_compile_budget_not_inline_tracker", func(t *testing.T) {
		d := &panickingSourceDialect{
			name:           "src_o2_compile_panic",
			panicOnCompile: true,
			delay:          5 * time.Millisecond,
		}
		eng, _ := newChainTestEngine(t, "c_o2_compile", d)
		inlineTr := ruleengine.NewBudgetTracker(5 * time.Second)
		compileTr := ruleengine.NewBudgetTracker(5 * time.Second)
		var missCount atomic.Int32
		dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second, Tracker: inlineTr}}
		run, err := eng.NewChainRun(
			ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{
				Index:   1,
				Dialect: d.name,
				Kind:    ruleengine.KindFilter,
				Source: &ruleengine.ChainElementSource{
					Expr:          "true",
					Scope:         celengine.CacheScope{Consumer: "c_o2_compile", ID: "r1"},
					Kind:          ruleengine.KindFilter,
					EnvRef:        ruleengine.EnvironmentRef{Consumer: "c_o2_compile", Name: "e"},
					CompileBudget: compileTr,
					OnCompileMiss: func(string) { missCount.Add(1) },
				},
			}}},
			ruleengine.Input{Activation: map[string]any{}},
			ruleengine.ChainLimits{MaxElements: 4, MaxTraceBytes: 4096},
			nil, nil, nil,
		)
		if err != nil {
			t.Fatalf("NewChainRun: %v", err)
		}

		out, p, done, err := run.Next()
		if err != nil || p != nil || !done || out == nil {
			t.Fatalf("Next() = (out=%+v, p=%v, done=%v, err=%v), want failed/eval_failed & done=true", out, p, done, err)
		}
		if out.Class != ruleengine.StepClassFailed || out.Reason != ruleengine.ReasonEvalFailed {
			t.Fatalf("outcome = %+v, want failed/eval_failed", out)
		}
		if compileTr.Spent() < 3*time.Millisecond {
			t.Fatalf("compile panic not charged to CompileBudget: spent=%s", compileTr.Spent())
		}
		if inlineTr.Spent() != 0 {
			t.Fatalf("compile panic must not charge Inline.Budget.Tracker when CompileBudget is set: spent=%s", inlineTr.Spent())
		}
		if missCount.Load() != 1 {
			t.Fatalf("expected OnCompileMiss called once on compile panic, got %d", missCount.Load())
		}
		var fe *ruleengine.FailedError
		if !errors.As(out.Err, &fe) || fe.Cause == nil || fe.Cause.Error() != "dialect panic" {
			t.Fatalf("O-3: expected sentinel cause 'dialect panic', got err=%v", out.Err)
		}
		if strings.Contains(out.Err.Error(), "SECRET_COMPILE_PANIC_TEXT") {
			t.Fatalf("O-3: panic payload leaked into FailedError: %v", out.Err)
		}
	})

	t.Run("source_eval_panic_with_compile_budget_charges_inline_tracker_and_sentinel", func(t *testing.T) {
		d := &panickingSourceDialect{
			name:        "src_o2_eval_panic",
			panicOnEval: true,
			delay:       5 * time.Millisecond,
		}
		eng, _ := newChainTestEngine(t, "c_o2_eval", d)
		inlineTr := ruleengine.NewBudgetTracker(5 * time.Second)
		compileTr := ruleengine.NewBudgetTracker(5 * time.Second)
		var missCount atomic.Int32
		dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second, Tracker: inlineTr}}
		run, err := eng.NewChainRun(
			ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{
				Index:   1,
				Dialect: d.name,
				Kind:    ruleengine.KindFilter,
				Source: &ruleengine.ChainElementSource{
					Expr:          "true",
					Scope:         celengine.CacheScope{Consumer: "c_o2_eval", ID: "r1"},
					Kind:          ruleengine.KindFilter,
					EnvRef:        ruleengine.EnvironmentRef{Consumer: "c_o2_eval", Name: "e"},
					CompileBudget: compileTr,
					OnCompileMiss: func(string) { missCount.Add(1) },
				},
			}}},
			ruleengine.Input{Activation: map[string]any{}},
			ruleengine.ChainLimits{MaxElements: 4, MaxTraceBytes: 4096},
			nil, nil, nil,
		)
		if err != nil {
			t.Fatalf("NewChainRun: %v", err)
		}
		out, _, done, err := run.Next()
		if err != nil || !done || out == nil || out.Class != ruleengine.StepClassFailed || out.Reason != ruleengine.ReasonEvalFailed {
			t.Fatalf("Next() = (out=%+v, done=%v, err=%v)", out, done, err)
		}
		if missCount.Load() != 1 {
			t.Fatalf("expected OnCompileMiss called once, got %d", missCount.Load())
		}
		if inlineTr.Spent() < 3*time.Millisecond {
			t.Fatalf("eval panic not charged to Inline.Budget.Tracker: spent=%s", inlineTr.Spent())
		}
		var fe *ruleengine.FailedError
		if !errors.As(out.Err, &fe) || fe.Cause == nil || fe.Cause.Error() != "dialect panic" {
			t.Fatalf("O-3: expected sentinel cause 'dialect panic', got err=%v", out.Err)
		}
		if strings.Contains(out.Err.Error(), "SECRET_EVAL_PANIC_TEXT") {
			t.Fatalf("O-3: panic payload leaked into FailedError: %v", out.Err)
		}
	})

	t.Run("io_panic_uses_sentinel_cause", func(t *testing.T) {
		io := newCountingFakeIODialect("io_o3_sentinel", time.Second)
		io.setHandler("boom", func(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			panic("SECRET_IO_SENTINEL_TEXT")
		})
		eng, _ := newChainTestEngine(t, "c_o3_io", io)
		c := mustCompile(t, eng, "c_o3_io", "io_o3_sentinel", "boom", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		r, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
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
		res, _ := r.Result()
		var fe *ruleengine.FailedError
		if len(res.Steps) != 1 || !errors.As(res.Steps[0].Err, &fe) || fe.Cause == nil || fe.Cause.Error() != "dialect panic" {
			t.Fatalf("O-3: expected sentinel cause 'dialect panic', got steps=%+v", res.Steps)
		}
		if strings.Contains(res.Steps[0].Err.Error(), "SECRET_IO_SENTINEL_TEXT") {
			t.Fatalf("O-3: panic payload leaked into IO FailedError: %v", res.Steps[0].Err)
		}
	})
}
