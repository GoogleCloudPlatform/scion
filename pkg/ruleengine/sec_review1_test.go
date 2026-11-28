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
	"math"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

type unexpPayload struct{ secret string }

// TestSecProbe_AdmittedKeyMasksEnrichment (SEC-S1-M1 regression test):
// When Input.AdmittedInputKey is set and Enricher supplies non-empty additions
// (chain.* or reserved names), the grouping safety floor MUST mix a canonical
// hash of those additions into the keyed hash so runs with differing additions
// never group (zero false pass), identical additions still group, and
// non-canonically-hashable additions fail closed with incompatible_group.
func TestSecProbe_AdmittedKeyMasksEnrichment(t *testing.T) {
	t.Run("differing_additions_fail_closed_zero_false_pass", func(t *testing.T) {
		cons := "probe_m1_diff"
		io := newCountingFakeIODialect("probe_io_m1_diff", time.Second)
		eng, _ := newChainTestEngine(t, cons, io)
		cIO := mustCompile(t, eng, cons, "probe_io_m1_diff", "x", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		d := ruleengine.ChainDomains{Inline: dom, IO: dom}

		io.setHandler("x", func(_ context.Context, _ ruleengine.Compiled, in ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			ch, _ := in.Activation["chain"].(map[string]any)
			return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: ch["v"] == "allow"}, nil
		})

		mk := func(v string) *ruleengine.ChainRun {
			enr := funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
				return map[string]any{"chain.v": v}, nil
			})
			r, err := eng.NewChainRun(d, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO, GroupKey: "g"}}},
				ruleengine.Input{Activation: map[string]any{"flag": true}, AdmittedInputKey: "same"}, ruleengine.ChainLimits{}, nil, enr, nil)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}

		r1, r2 := mk("allow"), mk("deny")
		_, p1, _, err := r1.Next()
		if err != nil {
			t.Fatalf("r1.Next: %v", err)
		}
		_, p2, _, err := r2.Next()
		if err != nil {
			t.Fatalf("r2.Next: %v", err)
		}

		gid, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
		if err == nil {
			t.Fatalf("expected incompatible_group error when AdmittedInputKey matches but Enricher additions differ; got gid=%q calls=%d", gid, io.callCount())
		}
		var fe *ruleengine.FailedError
		if !errors.As(err, &fe) || fe.Reason != ruleengine.ReasonIncompatibleGroup {
			t.Fatalf("expected FailedError{Reason: incompatible_group}, got %v", err)
		}
		if io.callCount() != 0 {
			t.Fatalf("expected 0 IO calls on incompatible group, got %d", io.callCount())
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res2, _ := r2.Result()
		if len(res2.Steps) != 1 || res2.Steps[0].Class != ruleengine.StepClassFailed || res2.Steps[0].Reason != ruleengine.ReasonIncompatibleGroup {
			t.Fatalf("r2 must fail closed with incompatible_group (no false pass), got %+v", res2.Steps)
		}
	})

	t.Run("identical_additions_still_group", func(t *testing.T) {
		cons := "probe_m1_same"
		io := newCountingFakeIODialect("probe_io_m1_same", time.Second)
		eng, _ := newChainTestEngine(t, cons, io)
		cIO := mustCompile(t, eng, cons, "probe_io_m1_same", "x", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		d := ruleengine.ChainDomains{Inline: dom, IO: dom}

		io.setHandler("x", func(_ context.Context, _ ruleengine.Compiled, in ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			ch, _ := in.Activation["chain"].(map[string]any)
			return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: ch["v"] == "allow"}, nil
		})

		mk := func() *ruleengine.ChainRun {
			enr := funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
				return map[string]any{"chain.v": "allow"}, nil
			})
			r, err := eng.NewChainRun(d, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO, GroupKey: "g"}}},
				ruleengine.Input{Activation: map[string]any{"flag": true}, AdmittedInputKey: "same"}, ruleengine.ChainLimits{}, nil, enr, nil)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}

		r1, r2 := mk(), mk()
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		gid, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
		if err != nil || gid == "" {
			t.Fatalf("identical key + identical additions should group: gid=%q err=%v", gid, err)
		}
		if io.callCount() != 1 {
			t.Fatalf("expected 1 grouped IO call, got %d", io.callCount())
		}
		_, _ = r1.Resume()
		_, _ = r2.Resume()
		res1, _ := r1.Result()
		res2, _ := r2.Result()
		if res1.Steps[0].Class != ruleengine.StepClassPass || res2.Steps[0].Class != ruleengine.StepClassPass {
			t.Fatalf("expected pass for both grouped members, got r1=%s r2=%s", res1.Steps[0].Class, res2.Steps[0].Class)
		}
	})

	t.Run("unhashable_additions_fail_closed", func(t *testing.T) {
		cons := "probe_m1_unhashable"
		io := newCountingFakeIODialect("probe_io_m1_unhashable", time.Second)
		eng, _ := newChainTestEngine(t, cons, io)
		cIO := mustCompile(t, eng, cons, "probe_io_m1_unhashable", "x", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		d := ruleengine.ChainDomains{Inline: dom, IO: dom}

		mk := func() *ruleengine.ChainRun {
			enr := funcEnricher(func(int, ruleengine.JudgmentView) (map[string]any, error) {
				// math.NaN() cannot be canonically JSON-marshaled.
				return map[string]any{"chain.bad": math.NaN()}, nil
			})
			r, err := eng.NewChainRun(d, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO, GroupKey: "g"}}},
				ruleengine.Input{Activation: map[string]any{"flag": true}, AdmittedInputKey: "same"}, ruleengine.ChainLimits{}, nil, enr, nil)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}

		r1, r2 := mk(), mk()
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		_, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
		var fe *ruleengine.FailedError
		if !errors.As(err, &fe) || fe.Reason != ruleengine.ReasonIncompatibleGroup {
			t.Fatalf("expected incompatible_group for unhashable additions, got %v", err)
		}
		if io.callCount() != 0 {
			t.Fatalf("expected 0 IO calls on unhashable additions, got %d", io.callCount())
		}
	})
}

// TestSecProbe_PayloadUnexportedFields (SEC-S1-L1 pinned contract test):
// Pins the documented contract on Input.Payload:
//  1. When AdmittedInputKey == "", the fallback admitted-input hash uses
//     encoding/json, which skips unexported / json:"-" struct fields; consumers
//     must ensure Payload is JSON-complete (or supply AdmittedInputKey).
//  2. When Payload is a JSON-shaped value (e.g. map[string]any), NewChainRun
//     defensively deep-copies it so post-construction caller mutations do not
//     leak into the run.
func TestSecProbe_PayloadUnexportedFields(t *testing.T) {
	t.Run("unexported_fields_ignored_by_json_fallback_pinned", func(t *testing.T) {
		cons := "probe_l1_unexp"
		io := newCountingFakeIODialect("probe_io_l1_unexp", time.Second)
		eng, _ := newChainTestEngine(t, cons, io)
		cIO := mustCompile(t, eng, cons, "probe_io_l1_unexp", "x", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		d := ruleengine.ChainDomains{Inline: dom, IO: dom}

		mk := func(p any) *ruleengine.ChainRun {
			r, err := eng.NewChainRun(d, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: cIO, GroupKey: "g"}}},
				ruleengine.Input{Payload: p}, ruleengine.ChainLimits{}, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		r1, r2 := mk(unexpPayload{"a"}), mk(unexpPayload{"b"})
		_, p1, _, _ := r1.Next()
		_, p2, _, _ := r2.Next()
		gid, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2})
		if err != nil || gid == "" || io.callCount() != 1 {
			t.Fatalf("pinned JSON-fallback behavior changed: gid=%q err=%v calls=%d", gid, err, io.callCount())
		}
	})

	t.Run("json_shaped_payload_is_defensively_deep_copied", func(t *testing.T) {
		cons := "probe_l1_copy"
		io := newCountingFakeIODialect("probe_io_l1_copy", time.Second)
		io.performsIO = false
		var seenPayload map[string]any
		io.setHandler("x", func(_ context.Context, c ruleengine.Compiled, in ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			seenPayload, _ = in.Payload.(map[string]any)
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})
		eng, _ := newChainTestEngine(t, cons, io)
		c := mustCompile(t, eng, cons, "probe_io_l1_copy", "x", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		d := ruleengine.ChainDomains{Inline: dom, IO: dom}

		payload := map[string]any{"project_id": "proj-1", "tags": []string{"a"}}
		r, err := eng.NewChainRun(d, ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Compiled: c}}},
			ruleengine.Input{Payload: payload}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		payload["project_id"] = "MUTATED"
		payload["tags"].([]string)[0] = "MUTATED"

		if _, _, _, err := r.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
		if seenPayload["project_id"] != "proj-1" || seenPayload["tags"].([]string)[0] != "a" {
			t.Fatalf("JSON-shaped Payload aliased caller state: %+v", seenPayload)
		}
	})
}
