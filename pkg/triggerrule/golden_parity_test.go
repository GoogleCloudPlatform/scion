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

package triggerrule_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

type goldenOutcome struct {
	matched bool
	prov    string
	err     error
	onEval  func()
}

type goldenFakeDialect struct {
	name     string
	outcomes map[string]goldenOutcome
}

func (d *goldenFakeDialect) Name() string { return d.name }

func (d *goldenFakeDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		MaxEvalTime: 100 * time.Millisecond,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}

func (d *goldenFakeDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, rule), nil
}

func (d *goldenFakeDialect) Evaluate(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	expr, _ := c.Rep().(string)
	o := d.outcomes[expr]
	if o.onEval != nil {
		o.onEval()
	}
	if o.err != nil {
		return ruleengine.Decision{Kind: c.Kind(), Trace: o.prov}, o.err
	}
	return ruleengine.Decision{Kind: c.Kind(), Bool: o.matched, Trace: o.prov}, nil
}

// normalizeTraceDuration zeroes duration_ms in trace elements so golden JSON
// byte parity is deterministic across CPU scheduling jitter while still
// pinning the exact JSON schema, field order, keys, classes, reasons,
// provenance, unexecuted indices, and budget_remaining_ms.
func normalizeTraceDuration(trace triggerrule.RuleTrace) triggerrule.RuleTrace {
	cp := triggerrule.RuleTrace{
		Elements:          make([]triggerrule.RuleTraceElement, len(trace.Elements)),
		Unexecuted:        append([]int(nil), trace.Unexecuted...),
		BudgetRemainingMs: trace.BudgetRemainingMs,
	}
	for i, el := range trace.Elements {
		el.DurationMs = 0
		cp.Elements[i] = el
	}
	return cp
}

// TestMatchAllWithTrace_GoldenByteParity pins the exact byte-level output of
// MatchAllWithTrace (MatchResult, SkipReason, matched_through, decided_dialect,
// and EncodeRuleTrace JSON) across every outcome class and cap on unmodified
// pre-chain behaviour (shared-rule-chain-design v1 §3.8, §4 acceptance 2, A1 P8).
//
// After migrating MatchAllWithTrace onto pkg/ruleengine ChainRun, these golden
// expectations must continue to pass UNCHANGED.
func TestMatchAllWithTrace_GoldenByteParity(t *testing.T) {
	var cancelMid context.CancelFunc
	fake := &goldenFakeDialect{
		name: "golden_fake",
		outcomes: map[string]goldenOutcome{
			"pass_with_prov": {matched: true, prov: "p1=ok"},
			"pass_and_cancel": {
				matched: true,
				prov:    "p1=ok",
				onEval: func() {
					if cancelMid != nil {
						cancelMid()
					}
				},
			},
			"uncertain_abstain": {
				err: &ruleengine.UncertainError{Reason: "abstain"},
			},
			"failed_provider": {
				err: &ruleengine.FailedError{Reason: "provider_error"},
			},
			"failed_tainted": {
				err: &ruleengine.FailedError{Reason: "secret payload hunter2"},
			},
		},
	}

	_, rules := newTriggerRulesWithExtraDialects(t, fake)
	ev := sampleFireEvent(`{"env":"prod"}`)
	act, err := triggerrule.NewActivation(ev, hubfire.FireTriggerRef{ID: "trig-golden", Name: "golden"})
	if err != nil {
		t.Fatalf("NewActivation: %v", err)
	}

	type goldenCase struct {
		name               string
		ctx                func() context.Context
		rules              []triggerrule.Rule
		opts               func() []triggerrule.EvalOption
		wantRes            triggerrule.MatchResult
		wantMatchedThrough string
		wantDecidedDialect string
		wantErr            bool
		wantSkipReason     string
		wantIsSkip         bool
		wantTraceJSON      string
	}

	exhaustedTracker := func() *ruleengine.BudgetTracker {
		tr := ruleengine.NewBudgetTracker(10 * time.Millisecond)
		tr.Exhaust()
		return tr
	}

	nineRules := make([]triggerrule.Rule, triggerrule.MaxMatchRules+1)
	for i := range nineRules {
		nineRules[i] = triggerrule.Rule{Dialect: "golden_fake", Expr: "pass_with_prov"}
	}
	fiveOversizedTotal := make([]triggerrule.Rule, 5)
	for i := range fiveOversizedTotal {
		fiveOversizedTotal[i] = triggerrule.Rule{
			Dialect: "golden_fake",
			Expr:    fmt.Sprintf("elem_%d_%s", i, strings.Repeat("q", 3500)),
		}
	}

	cases := []goldenCase{
		{
			name:               "empty_list",
			rules:              nil,
			wantRes:            triggerrule.MatchResult{Matched: true, Through: 0, Dialect: "", Class: triggerrule.MatchClassMatched, Reason: ""},
			wantMatchedThrough: "0/0",
			wantDecidedDialect: "",
			wantErr:            false,
			wantSkipReason:     "",
			wantIsSkip:         false,
			wantTraceJSON:      "",
		},
		{
			name: "matched_single_cel",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
			},
			wantRes:            triggerrule.MatchResult{Matched: true, Through: 1, Dialect: "cel", Class: triggerrule.MatchClassMatched, Reason: ""},
			wantMatchedThrough: "1/1",
			wantDecidedDialect: "cel",
			wantErr:            false,
			wantSkipReason:     "",
			wantIsSkip:         false,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"matched","duration_ms":0}]}`,
		},
		{
			name: "matched_multi_cel_and_fake",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: "golden_fake", Expr: "pass_with_prov"},
			},
			wantRes:            triggerrule.MatchResult{Matched: true, Through: 2, Dialect: "golden_fake", Class: triggerrule.MatchClassMatched, Reason: ""},
			wantMatchedThrough: "2/2",
			wantDecidedDialect: "golden_fake",
			wantErr:            false,
			wantSkipReason:     "",
			wantIsSkip:         false,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"matched","duration_ms":0},{"i":2,"dialect":"golden_fake","class":"matched","duration_ms":0,"provenance":"p1=ok"}]}`,
		},
		{
			name: "false_short_circuit",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "staging"`},
				{Dialect: "golden_fake", Expr: "pass_with_prov"},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 2, Dialect: "cel", Class: triggerrule.MatchClassFalse, Reason: ""},
			wantMatchedThrough: "2/3",
			wantDecidedDialect: "cel",
			wantErr:            false,
			wantSkipReason:     "",
			wantIsSkip:         false,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"matched","duration_ms":0},{"i":2,"dialect":"cel","class":"false","duration_ms":0}],"unexecuted":[3]}`,
		},
		{
			name: "uncertain_short_circuit",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: "golden_fake", Expr: "uncertain_abstain"},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 2, Dialect: "golden_fake", Class: triggerrule.MatchClassUncertain, Reason: "abstain"},
			wantMatchedThrough: "2/3",
			wantDecidedDialect: "golden_fake",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonRuleUncertain,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"matched","duration_ms":0},{"i":2,"dialect":"golden_fake","class":"uncertain","reason":"abstain","duration_ms":0}],"unexecuted":[3]}`,
		},
		{
			name: "failed_runtime_missing_input",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.nonexistent == "x"`},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 2, Dialect: "cel", Class: triggerrule.MatchClassFailed, Reason: celengine.ReasonMissingInput},
			wantMatchedThrough: "2/3",
			wantDecidedDialect: "cel",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonEvalFailed,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"matched","duration_ms":0},{"i":2,"dialect":"cel","class":"failed","reason":"missing_input","duration_ms":0}],"unexecuted":[3]}`,
		},
		{
			name: "failed_compile_syntax",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env === "prod"`},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 2, Dialect: "cel", Class: triggerrule.MatchClassFailed, Reason: celengine.CategorySyntax},
			wantMatchedThrough: "2/3",
			wantDecidedDialect: "cel",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonNoLongerCompiles,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"matched","duration_ms":0},{"i":2,"dialect":"cel","class":"failed","reason":"syntax","duration_ms":0}],"unexecuted":[3]}`,
		},
		{
			name: "failed_unsupported_dialect",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: "ungranted_dialect", Expr: `anything`},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 2, Dialect: "ungranted_dialect", Class: triggerrule.MatchClassFailed, Reason: ruleengine.ReasonDialectNotAllowed},
			wantMatchedThrough: "2/3",
			wantDecidedDialect: "ungranted_dialect",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonEvalFailed,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"matched","duration_ms":0},{"i":2,"dialect":"ungranted_dialect","class":"failed","reason":"dialect_not_allowed","duration_ms":0}],"unexecuted":[3]}`,
		},
		{
			name: "failed_tainted_reason_scrubbed",
			rules: []triggerrule.Rule{
				{Dialect: "golden_fake", Expr: "failed_tainted"},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 1, Dialect: "golden_fake", Class: triggerrule.MatchClassFailed, Reason: triggerrule.MatchReasonEvalFailed},
			wantMatchedThrough: "1/1",
			wantDecidedDialect: "golden_fake",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonEvalFailed,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"golden_fake","class":"failed","reason":"eval_failed","duration_ms":0}]}`,
		},
		{
			name: "failed_corrupt_empty_expr",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: ruleengine.DialectCEL, Expr: "   "},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 2, Dialect: "cel", Class: triggerrule.MatchClassFailed, Reason: triggerrule.MatchReasonCorruptRules},
			wantMatchedThrough: "2/3",
			wantDecidedDialect: "cel",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonEvalFailed,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"matched","duration_ms":0},{"i":2,"dialect":"cel","class":"failed","reason":"corrupt_rules","duration_ms":0}],"unexecuted":[3]}`,
		},
		{
			name: "failed_oversized_element",
			rules: []triggerrule.Rule{
				{Dialect: "golden_fake", Expr: "pass_with_prov"},
				{Dialect: "golden_fake", Expr: strings.Repeat("x", triggerrule.MaxMatchRuleBytes+1)},
				{Dialect: "golden_fake", Expr: "pass_with_prov"},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 2, Dialect: "golden_fake", Class: triggerrule.MatchClassFailed, Reason: celengine.CategorySize},
			wantMatchedThrough: "2/3",
			wantDecidedDialect: "golden_fake",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonEvalFailed,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"golden_fake","class":"matched","duration_ms":0,"provenance":"p1=ok"},{"i":2,"dialect":"golden_fake","class":"failed","reason":"size","duration_ms":0}],"unexecuted":[3]}`,
		},
		{
			name: "budget_exhausted_preflight",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			opts: func() []triggerrule.EvalOption {
				return []triggerrule.EvalOption{triggerrule.WithBudget(exhaustedTracker())}
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 1, Dialect: "cel", Class: triggerrule.MatchClassBudget, Reason: triggerrule.MatchReasonBudgetExhausted},
			wantMatchedThrough: "1/2",
			wantDecidedDialect: "cel",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonBudgetExhausted,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"budget","reason":"budget_exhausted","duration_ms":0}],"unexecuted":[2],"budget_remaining_ms":0}`,
		},
		{
			name: "compile_budget_exhausted",
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "cold_compile_budget_case"`},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			opts: func() []triggerrule.EvalOption {
				return []triggerrule.EvalOption{triggerrule.WithCompileBudget(exhaustedTracker())}
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 1, Dialect: "cel", Class: triggerrule.MatchClassBudget, Reason: triggerrule.MatchReasonCompileBudgetExhausted},
			wantMatchedThrough: "1/2",
			wantDecidedDialect: "cel",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonCompileBudgetExhausted,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"cel","class":"budget","reason":"compile_budget_exhausted","duration_ms":0}],"unexecuted":[2]}`,
		},
		{
			name: "canceled_preflight",
			ctx: func() context.Context {
				c, cancel := context.WithCancel(context.Background())
				cancel()
				return c
			},
			rules: []triggerrule.Rule{
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "prod"`},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 1, Dialect: "cel", Class: triggerrule.MatchClassFailed, Reason: celengine.ReasonCanceled},
			wantMatchedThrough: "1/2",
			wantDecidedDialect: "cel",
			wantErr:            true,
			wantSkipReason:     "",
			wantIsSkip:         false,
			wantTraceJSON:      "",
		},
		{
			// With Step 3's late-success fix (§3.4), a dialect that cancels ctx
			// during element 1 and returns (true, nil) is discarded as canceled
			// at element 1 itself rather than slipping through to element 2.
			name: "canceled_during_element_1",
			ctx: func() context.Context {
				c, cancel := context.WithCancel(context.Background())
				cancelMid = cancel
				return c
			},
			rules: []triggerrule.Rule{
				{Dialect: "golden_fake", Expr: "pass_and_cancel"},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 1, Dialect: "golden_fake", Class: triggerrule.MatchClassFailed, Reason: celengine.ReasonCanceled},
			wantMatchedThrough: "1/3",
			wantDecidedDialect: "golden_fake",
			wantErr:            true,
			wantSkipReason:     "",
			wantIsSkip:         false,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"golden_fake","class":"failed","reason":"canceled","duration_ms":0}],"unexecuted":[2,3]}`,
		},
		{
			// Context canceled between element 1 (pass) and element 2 (via compile
			// miss observer on element 2) stops at element 2 as failed/canceled.
			name: "canceled_mid_list_before_element_2",
			ctx: func() context.Context {
				c, cancel := context.WithCancel(context.Background())
				cancelMid = cancel
				return c
			},
			rules: []triggerrule.Rule{
				{Dialect: "golden_fake", Expr: "pass_with_prov"},
				{Dialect: ruleengine.DialectCEL, Expr: `event.payload.env == "cancel_before_el2"`},
				{Dialect: ruleengine.DialectCEL, Expr: `true`},
			},
			opts: func() []triggerrule.EvalOption {
				return []triggerrule.EvalOption{
					triggerrule.WithCompileMissObserver(func(dialect string) {
						if dialect == ruleengine.DialectCEL && cancelMid != nil {
							cancelMid()
						}
					}),
				}
			},
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 2, Dialect: "cel", Class: triggerrule.MatchClassFailed, Reason: celengine.ReasonCanceled},
			wantMatchedThrough: "2/3",
			wantDecidedDialect: "cel",
			wantErr:            true,
			wantSkipReason:     "",
			wantIsSkip:         false,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"golden_fake","class":"matched","duration_ms":0,"provenance":"p1=ok"},{"i":2,"dialect":"cel","class":"failed","reason":"canceled","duration_ms":0}],"unexecuted":[3]}`,
		},
		{
			name:               "cap_max_match_rules",
			rules:              nineRules,
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 1, Dialect: "golden_fake", Class: triggerrule.MatchClassFailed, Reason: celengine.CategorySize},
			wantMatchedThrough: "1/9",
			wantDecidedDialect: "golden_fake",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonEvalFailed,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"golden_fake","class":"failed","reason":"size","duration_ms":0}]}`,
		},
		{
			name:               "cap_max_match_rules_json_bytes",
			rules:              fiveOversizedTotal,
			wantRes:            triggerrule.MatchResult{Matched: false, Through: 1, Dialect: "golden_fake", Class: triggerrule.MatchClassFailed, Reason: celengine.CategorySize},
			wantMatchedThrough: "1/5",
			wantDecidedDialect: "golden_fake",
			wantErr:            true,
			wantSkipReason:     triggerrule.SkipReasonEvalFailed,
			wantIsSkip:         true,
			wantTraceJSON:      `{"elements":[{"i":1,"dialect":"golden_fake","class":"failed","reason":"size","duration_ms":0}]}`,
		},
	}

	for idx, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cancelMid = nil
			ctx := context.Background()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			var opts []triggerrule.EvalOption
			if tc.opts != nil {
				opts = tc.opts()
			}
			scope := celengine.CacheScope{
				Consumer: triggerrule.ConsumerName,
				ID:       fmt.Sprintf("trig-golden-%d", idx),
				Revision: 1,
			}

			res, trace, evalErr := rules.MatchAllWithTrace(ctx, scope, tc.rules, act, opts...)
			if (evalErr != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", evalErr, tc.wantErr)
			}
			if res != tc.wantRes {
				t.Fatalf("MatchResult = %+v, want %+v", res, tc.wantRes)
			}
			gotThrough := fmt.Sprintf("%d/%d", res.Through, len(tc.rules))
			if gotThrough != tc.wantMatchedThrough {
				t.Fatalf("matched_through = %q, want %q", gotThrough, tc.wantMatchedThrough)
			}
			if res.Dialect != tc.wantDecidedDialect {
				t.Fatalf("decided_dialect = %q, want %q", res.Dialect, tc.wantDecidedDialect)
			}
			gotSkip, gotIsSkip := triggerrule.SkipReason(evalErr)
			if gotSkip != tc.wantSkipReason || gotIsSkip != tc.wantIsSkip {
				t.Fatalf("SkipReason = (%q, %v), want (%q, %v)", gotSkip, gotIsSkip, tc.wantSkipReason, tc.wantIsSkip)
			}

			gotJSON := triggerrule.EncodeRuleTrace(normalizeTraceDuration(trace))
			if gotJSON != tc.wantTraceJSON {
				t.Fatalf("EncodeRuleTrace =\n  %s\nwant:\n  %s", gotJSON, tc.wantTraceJSON)
			}
		})
	}
}

// TestEncodeRuleTrace_GoldenTruncationAndSanitization pins the 4 KiB cap,
// deterministic truncation order, and provenance sanitization of EncodeRuleTrace.
func TestEncodeRuleTrace_GoldenTruncationAndSanitization(t *testing.T) {
	// Sanitization strips control characters and caps provenance at 256 bytes.
	sanitized := triggerrule.EncodeRuleTrace(triggerrule.RuleTrace{
		Elements: []triggerrule.RuleTraceElement{{
			Index:      1,
			Dialect:    "cel",
			Class:      triggerrule.MatchClassMatched,
			Provenance: "ok\x00\n\t" + strings.Repeat("a", 300),
		}},
	})
	var parsed triggerrule.RuleTrace
	if err := json.Unmarshal([]byte(sanitized), &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(parsed.Elements) != 1 || len(parsed.Elements[0].Provenance) != 256 {
		t.Fatalf("sanitized provenance len = %d, want 256 (json=%s)", len(parsed.Elements[0].Provenance), sanitized)
	}
	if strings.ContainsAny(parsed.Elements[0].Provenance, "\x00\n\t") {
		t.Fatalf("control chars survived sanitization: %q", parsed.Elements[0].Provenance)
	}

	// Deterministic 4 KiB cap: provenance is stripped earliest-first, then earliest
	// elements dropped, while staying <= MaxRuleTraceBytes.
	many := make([]triggerrule.RuleTraceElement, 30)
	for i := range many {
		many[i] = triggerrule.RuleTraceElement{
			Index:      i + 1,
			Dialect:    "golden_fake",
			Class:      triggerrule.MatchClassMatched,
			Provenance: strings.Repeat("p", 200),
		}
	}
	encoded := triggerrule.EncodeRuleTrace(triggerrule.RuleTrace{Elements: many})
	if len(encoded) > triggerrule.MaxRuleTraceBytes {
		t.Fatalf("encoded len %d exceeds MaxRuleTraceBytes (%d)", len(encoded), triggerrule.MaxRuleTraceBytes)
	}
	if !strings.Contains(encoded, `"i":30`) {
		t.Fatalf("deciding/last element missing after truncation: %s", encoded)
	}
}
