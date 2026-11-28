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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

type fit2aDetail struct {
	Verdict string
	Tags    []string
	Meta    map[string]string
}

func copyFit2aDetail(v any) any {
	d, ok := v.(*fit2aDetail)
	if !ok || d == nil {
		return nil
	}
	cp := &fit2aDetail{
		Verdict: d.Verdict,
	}
	if d.Tags != nil {
		cp.Tags = append([]string(nil), d.Tags...)
	}
	if d.Meta != nil {
		cp.Meta = make(map[string]string, len(d.Meta))
		for k, val := range d.Meta {
			cp.Meta[k] = val
		}
	}
	return cp
}

// TestFit2a_StepOutcomeJudgment_PolicyStopMutationIsolationAndTraceExclusion verifies:
//   - StepPolicy sees the current step's Judgment for passing KindJudgment steps
//     and can stop the chain based on it without re-entering the ChainRun;
//   - mutating out.Judgment.Detail inside StepPolicy (or on Next()/Result().Steps)
//     does not mutate Judgments() or other grouped members;
//   - out.Judgment is nil for KindFilter and for non-pass KindJudgment outcomes;
//   - EncodeChainTrace never serializes any Judgment content.
func TestFit2a_StepOutcomeJudgment_PolicyStopMutationIsolationAndTraceExclusion(t *testing.T) {
	t.Run("inline_policy_stops_on_judgment_and_mutation_isolated_and_trace_clean", func(t *testing.T) {
		const secretSentinel = "SECRET_JUDGMENT_PAYLOAD_DO_NOT_LEAK"
		dial := newCountingFakeIODialect("inline_judger_2a", time.Second)
		dial.performsIO = false
		dial.copyDetailFn = copyFit2aDetail
		dial.setHandler("f1", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})
		dial.setHandler("j1", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{
				Kind: c.Kind(),
				Detail: &fit2aDetail{
					Verdict: "block",
					Tags:    []string{secretSentinel},
					Meta:    map[string]string{"note": secretSentinel},
				},
			}, nil
		})
		dial.setHandler("f2", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})

		eng, _ := newChainTestEngine(t, "c_fit2a_inline", dial)
		cFilter := mustCompile(t, eng, "c_fit2a_inline", "inline_judger_2a", "f1", ruleengine.KindFilter)
		cJudg := mustCompile(t, eng, "c_fit2a_inline", "inline_judger_2a", "j1", ruleengine.KindJudgment)
		cAfter := mustCompile(t, eng, "c_fit2a_inline", "inline_judger_2a", "f2", ruleengine.KindFilter)

		var policySawFilterNil bool
		var policySawVerdict string
		policy := func(out ruleengine.StepOutcome) ruleengine.Control {
			if out.Kind == ruleengine.KindFilter {
				policySawFilterNil = (out.Judgment == nil)
				return ruleengine.Control{Stop: false}
			}
			if out.Judgment == nil {
				t.Errorf("expected non-nil out.Judgment on passing KindJudgment step")
				return ruleengine.Control{Stop: true}
			}
			d, ok := out.Judgment.Detail.(*fit2aDetail)
			if !ok {
				t.Errorf("expected *fit2aDetail, got %T", out.Judgment.Detail)
				return ruleengine.Control{Stop: true}
			}
			policySawVerdict = d.Verdict
			// Mutate inside StepPolicy to verify isolation from stored state.
			d.Verdict = "MUTATED_BY_POLICY"
			d.Tags[0] = "MUTATED_TAG"
			d.Meta["note"] = "MUTATED_META"
			if policySawVerdict == "block" {
				return ruleengine.Control{Stop: true, Verdict: "blocked_by_judgment"}
			}
			return ruleengine.Control{Stop: false}
		}

		dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second}}
		run, err := eng.NewChainRun(
			ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{
				{Index: 1, Dialect: "inline_judger_2a", Kind: ruleengine.KindFilter, Compiled: cFilter},
				{Index: 2, Dialect: "inline_judger_2a", Kind: ruleengine.KindJudgment, Compiled: cJudg},
				{Index: 3, Dialect: "inline_judger_2a", Kind: ruleengine.KindFilter, Compiled: cAfter},
			}},
			ruleengine.Input{Activation: map[string]any{}},
			ruleengine.ChainLimits{MaxElements: 8, MaxTraceBytes: 8192},
			policy,
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("NewChainRun: %v", err)
		}

		out1, _, done1, err := run.Next()
		if err != nil || done1 || out1 == nil {
			t.Fatalf("step 1: out=%v done=%v err=%v", out1, done1, err)
		}
		if out1.Judgment != nil {
			t.Fatalf("step 1 KindFilter expected nil Judgment, got %+v", out1.Judgment)
		}

		out2, _, done2, err := run.Next()
		if err != nil || !done2 || out2 == nil {
			t.Fatalf("step 2: out=%v done=%v err=%v", out2, done2, err)
		}
		if !policySawFilterNil {
			t.Fatalf("StepPolicy saw non-nil Judgment on KindFilter step")
		}
		if policySawVerdict != "block" {
			t.Fatalf("StepPolicy verdict = %q, want block", policySawVerdict)
		}
		if out2.Judgment == nil {
			t.Fatalf("Next() returned nil Judgment on passing KindJudgment")
		}
		// Mutate Next()'s returned copy as well.
		out2.Judgment.Detail.(*fit2aDetail).Verdict = "MUTATED_BY_NEXT"

		// Judgments() must still hold the unmutated original.
		j, ok := run.Judgments().Get(2)
		if !ok {
			t.Fatalf("Judgments().Get(2) missing")
		}
		jd := j.Detail.(*fit2aDetail)
		if jd.Verdict != "block" || jd.Tags[0] != secretSentinel || jd.Meta["note"] != secretSentinel {
			t.Fatalf("stored judgment mutated: %+v", jd)
		}

		// Result().Steps must have nil for step 1, unmutated copy for step 2, and step 3 unexecuted.
		res, tr := run.Result()
		if !res.Stopped || res.Verdict != "blocked_by_judgment" || len(res.Steps) != 2 || len(res.Unexecuted) != 1 || res.Unexecuted[0] != 3 {
			t.Fatalf("unexpected result: %+v", res)
		}
		if res.Steps[0].Judgment != nil {
			t.Fatalf("Result().Steps[0].Judgment want nil, got %+v", res.Steps[0].Judgment)
		}
		if res.Steps[1].Judgment == nil {
			t.Fatalf("Result().Steps[1].Judgment want non-nil")
		}
		rsd := res.Steps[1].Judgment.Detail.(*fit2aDetail)
		if rsd.Verdict != "block" || rsd.Tags[0] != secretSentinel {
			t.Fatalf("Result().Steps[1].Judgment mutated: %+v", rsd)
		}
		// Mutating Result().Steps[1].Judgment must not affect subsequent Result() calls.
		rsd.Verdict = "MUTATED_BY_RESULT"
		res2, _ := run.Result()
		if res2.Steps[1].Judgment.Detail.(*fit2aDetail).Verdict != "block" {
			t.Fatalf("subsequent Result() saw mutation from earlier Result()")
		}

		// EncodeChainTrace must never serialize Judgment content.
		raw, err := ruleengine.EncodeChainTrace(tr, map[string]ruleengine.Capabilities{dial.Name(): dial.Declares()}, 8192)
		if err != nil {
			t.Fatalf("EncodeChainTrace: %v", err)
		}
		if strings.Contains(raw, secretSentinel) || strings.Contains(raw, `"judgment":`) {
			t.Fatalf("EncodeChainTrace leaked judgment content: %s", raw)
		}
	})

	t.Run("non_pass_judgment_has_nil_judgment", func(t *testing.T) {
		dial := newCountingFakeIODialect("uncertain_judger_2a", time.Second)
		dial.performsIO = false
		dial.copyDetailFn = copyFit2aDetail
		dial.setHandler("j1", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{Kind: c.Kind(), Detail: &fit2aDetail{Verdict: "should_not_surface"}}, &ruleengine.UncertainError{Reason: ruleengine.ReasonUncertain}
		})
		eng, _ := newChainTestEngine(t, "c_fit2a_nonpass", dial)
		cJudg := mustCompile(t, eng, "c_fit2a_nonpass", "uncertain_judger_2a", "j1", ruleengine.KindJudgment)

		var sawNil bool
		dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second}}
		run, err := eng.NewChainRun(
			ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Dialect: "uncertain_judger_2a", Kind: ruleengine.KindJudgment, Compiled: cJudg}}},
			ruleengine.Input{Activation: map[string]any{}},
			ruleengine.ChainLimits{MaxElements: 4, MaxTraceBytes: 4096},
			func(out ruleengine.StepOutcome) ruleengine.Control {
				sawNil = (out.Judgment == nil)
				return ruleengine.Control{Stop: true}
			},
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("NewChainRun: %v", err)
		}
		out, _, _, err := run.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !sawNil || out.Judgment != nil || out.Class != ruleengine.StepClassUncertain {
			t.Fatalf("non-pass judgment must have nil Judgment: sawNil=%v out=%+v", sawNil, out)
		}
		res, _ := run.Result()
		if res.Steps[0].Judgment != nil {
			t.Fatalf("Result().Steps[0].Judgment want nil on non-pass, got %+v", res.Steps[0].Judgment)
		}
	})

	t.Run("grouped_members_get_independent_judgment_copies", func(t *testing.T) {
		const secretSentinel = "GROUPED_SECRET_JUDGMENT"
		dial := newCountingFakeIODialect("io_judger_2a", time.Second)
		dial.performsIO = true
		dial.copyDetailFn = copyFit2aDetail
		dial.setHandler("j_grp", func(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			return ruleengine.Decision{
				Kind: c.Kind(),
				Detail: &fit2aDetail{
					Verdict: "escalate",
					Tags:    []string{secretSentinel},
					Meta:    map[string]string{"k": secretSentinel},
				},
			}, nil
		})
		eng, _ := newChainTestEngine(t, "c_fit2a_grp", dial)
		cJudg := mustCompile(t, eng, "c_fit2a_grp", "io_judger_2a", "j_grp", ruleengine.KindJudgment)

		ioTracker := ruleengine.NewBudgetTracker(time.Second)
		ioDom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second, Tracker: ioTracker}}
		inDom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second}}
		in := ruleengine.Input{Activation: map[string]any{"x": "same"}, AdmittedInputKey: "k-grp"}

		makeRun := func(mutate bool) *ruleengine.ChainRun {
			r, err := eng.NewChainRun(
				ruleengine.ChainDomains{Inline: inDom, IO: ioDom},
				ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Dialect: "io_judger_2a", Kind: ruleengine.KindJudgment, Compiled: cJudg, GroupKey: "gk1"}}},
				in,
				ruleengine.ChainLimits{MaxElements: 4, MaxTraceBytes: 4096},
				func(out ruleengine.StepOutcome) ruleengine.Control {
					if out.Judgment == nil {
						t.Errorf("grouped member saw nil Judgment")
						return ruleengine.Control{Stop: true}
					}
					d := out.Judgment.Detail.(*fit2aDetail)
					if d.Verdict != "escalate" || d.Tags[0] != secretSentinel {
						t.Errorf("grouped member saw corrupted judgment: %+v", d)
					}
					verdict := d.Verdict
					if mutate {
						d.Verdict = "LEADER_MUTATED"
						d.Tags[0] = "LEADER_MUTATED_TAG"
						d.Meta["k"] = "LEADER_MUTATED_META"
					}
					return ruleengine.Control{Stop: true, Verdict: verdict}
				},
				nil,
				nil,
			)
			if err != nil {
				t.Fatalf("NewChainRun: %v", err)
			}
			return r
		}

		r1 := makeRun(true)
		r2 := makeRun(false)
		_, p1, _, err := r1.Next()
		if err != nil {
			t.Fatalf("r1.Next: %v", err)
		}
		_, p2, _, err := r2.Next()
		if err != nil {
			t.Fatalf("r2.Next: %v", err)
		}
		if _, err := eng.EvaluateGroup(p1, []*ruleengine.PendingStep{p2}); err != nil {
			t.Fatalf("EvaluateGroup: %v", err)
		}
		if _, err := r1.Resume(); err != nil {
			t.Fatalf("r1.Resume: %v", err)
		}
		if _, err := r2.Resume(); err != nil {
			t.Fatalf("r2.Resume: %v", err)
		}

		// Both r1 and r2 stored judgments and Result().Steps must remain unmutated.
		for name, r := range map[string]*ruleengine.ChainRun{"r1": r1, "r2": r2} {
			j, ok := r.Judgments().Get(1)
			if !ok {
				t.Fatalf("%s: Judgments().Get(1) missing", name)
			}
			jd := j.Detail.(*fit2aDetail)
			if jd.Verdict != "escalate" || jd.Tags[0] != secretSentinel || jd.Meta["k"] != secretSentinel {
				t.Fatalf("%s: stored judgment mutated: %+v", name, jd)
			}
			res, tr := r.Result()
			if res.Steps[0].Judgment == nil || res.Steps[0].Judgment.Detail.(*fit2aDetail).Verdict != "escalate" {
				t.Fatalf("%s: Result().Steps[0].Judgment invalid: %+v", name, res.Steps[0].Judgment)
			}
			raw, err := ruleengine.EncodeChainTrace(tr, map[string]ruleengine.Capabilities{dial.Name(): dial.Declares()}, 4096)
			if err != nil {
				t.Fatalf("%s: EncodeChainTrace: %v", name, err)
			}
			if strings.Contains(raw, secretSentinel) || strings.Contains(raw, `"judgment":`) {
				t.Fatalf("%s: EncodeChainTrace leaked judgment: %s", name, raw)
			}
		}
	})
}

// TestFit2a_NewChainRun_AcceptsMessagePolicyActivationShapes verifies that
// NewChainRun accepts all activation value shapes produced by
// pkg/messagepolicy/activation.go (map[string]any containing map[string]string,
// map[string]bool, map[string]float64, []string, int64, bool, string) and
// defensively deep-copies every container so caller mutations never leak.
func TestFit2a_NewChainRun_AcceptsMessagePolicyActivationShapes(t *testing.T) {
	var seenActivation map[string]any
	dial := newCountingFakeIODialect("mp_probe_2a", time.Second)
	dial.performsIO = false
	dial.setHandler("true", func(_ context.Context, c ruleengine.Compiled, in ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
		seenActivation = in.Activation
		return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
	})
	eng, _ := newChainTestEngine(t, "c_mp_shapes", dial)
	c := mustCompile(t, eng, "c_mp_shapes", "mp_probe_2a", "true", ruleengine.KindFilter)

	// Mirrors pkg/messagepolicy/activation.go NewActivation exact Go types.
	sender := map[string]string{"kind": "agent", "id": "a1", "slug": "dev", "project_id": "p1"}
	recipient := map[string]string{"kind": "user", "id": "u1", "slug": "alice"}
	mentionedAgents := []string{"a2"}
	mentionedUsers := []string{"u1"}
	convAgents := []string{"a1", "a2"}
	convUsers := []string{"u1"}
	flags := map[string]bool{"urgent_flag": true}
	labels := map[string]string{"severity": "high"}
	scores := map[string]float64{"risk": 0.75}

	act := map[string]any{
		"surface":   "web",
		"sender":    sender,
		"recipient": recipient,
		"message": map[string]any{
			"type":             "instruction",
			"body":             "hello",
			"body_truncated":   false,
			"bytes":            int64(5),
			"has_choices":      true,
			"urgent":           false,
			"attachment_count": int64(2),
		},
		"mentioned_agent_ids": mentionedAgents,
		"mentioned_user_ids":  mentionedUsers,
		"conversation": map[string]any{
			"kind":                  "direct",
			"agent_participant_ids": convAgents,
			"user_participant_ids":  convUsers,
		},
		"judgment_flags":  flags,
		"judgment_labels": labels,
		"judgment_scores": scores,
	}

	dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second}}
	run, err := eng.NewChainRun(
		ruleengine.ChainDomains{Inline: dom, IO: dom},
		ruleengine.Chain{Elements: []ruleengine.ChainElement{{Index: 1, Dialect: "mp_probe_2a", Kind: ruleengine.KindFilter, Compiled: c}}},
		ruleengine.Input{Activation: act},
		ruleengine.ChainLimits{MaxElements: 4, MaxTraceBytes: 4096},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewChainRun rejected message-policy activation shapes: %v", err)
	}

	// Mutate every caller container after construction to verify deep copy.
	sender["slug"] = "MUTATED"
	mentionedAgents[0] = "MUTATED"
	convAgents[0] = "MUTATED"
	flags["urgent_flag"] = false
	labels["severity"] = "MUTATED"
	scores["risk"] = 99.0

	if _, _, _, err := run.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if seenActivation["sender"].(map[string]string)["slug"] != "dev" {
		t.Fatalf("sender map aliased caller state")
	}
	if seenActivation["mentioned_agent_ids"].([]string)[0] != "a2" {
		t.Fatalf("mentioned_agent_ids slice aliased caller state")
	}
	if seenActivation["conversation"].(map[string]any)["agent_participant_ids"].([]string)[0] != "a1" {
		t.Fatalf("nested conversation slice aliased caller state")
	}
	if !seenActivation["judgment_flags"].(map[string]bool)["urgent_flag"] {
		t.Fatalf("judgment_flags map aliased caller state")
	}
	if seenActivation["judgment_labels"].(map[string]string)["severity"] != "high" {
		t.Fatalf("judgment_labels map aliased caller state")
	}
	if seenActivation["judgment_scores"].(map[string]float64)["risk"] != 0.75 {
		t.Fatalf("judgment_scores map aliased caller state")
	}
	if seenActivation["message"].(map[string]any)["bytes"].(int64) != 5 {
		t.Fatalf("message.bytes int64 mismatch")
	}
}
