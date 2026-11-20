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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

type cutShortTimeoutDialect struct {
	name        string
	maxEvalTime time.Duration
}

func (d *cutShortTimeoutDialect) Name() string { return d.name }
func (d *cutShortTimeoutDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		MaxEvalTime: d.maxEvalTime,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *cutShortTimeoutDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, rule), nil
}
func (d *cutShortTimeoutDialect) Evaluate(_ context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	// Return a timeout immediately (no sleep) to deterministically simulate an
	// evaluation cut short by the tracker deadline while elapsed < tracker.Remaining().
	return ruleengine.Decision{}, &ruleengine.FailedError{
		Reason: celengine.ReasonTimeout,
		Cause:  context.DeadlineExceeded,
	}
}

func TestCutShortBudgetExhaustionSkipsLaterBenignRuleWithoutTouchingCELCache(t *testing.T) {
	ctx := context.Background()

	celEng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	cutShort := &cutShortTimeoutDialect{name: "cut_short", maxEvalTime: 100 * time.Millisecond}

	re := ruleengine.NewEngine()
	if err := re.Register(celDialect); err != nil {
		t.Fatalf("Register cel: %v", err)
	}
	if err := re.Register(cutShort); err != nil {
		t.Fatalf("Register cut_short: %v", err)
	}
	if err := re.AllowConsumerDialects(
		triggerrule.ConsumerName,
		ruleengine.Grant(ruleengine.DialectCEL),
		ruleengine.Grant("cut_short"),
	); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}
	rules, err := triggerrule.New(re)
	if err != nil {
		t.Fatalf("triggerrule.New: %v", err)
	}

	ev := sampleFireEvent(`{"env":"prod"}`)
	baseAct, err := triggerrule.NewEventActivation(ev)
	if err != nil {
		t.Fatalf("NewEventActivation: %v", err)
	}

	// Tracker limit (50ms) < cutShort MaxEvalTime (100ms) => boundFromTracker is true.
	budget := ruleengine.NewBudgetTracker(50 * time.Millisecond)

	// 1. Candidate 1 is cut short by the tracker budget and returns ErrBudgetExhausted.
	act1 := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: "trig-cut-short", Name: "cut-short"})
	scope1 := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-cut-short", Revision: 1}
	matched, evalErr := rules.Match(ctx, scope1, triggerrule.Rule{
		Dialect: "cut_short",
		Expr:    "slow",
	}, act1, triggerrule.WithBudget(budget))
	if matched {
		t.Fatalf("cut-short trigger unexpectedly matched")
	}
	if !errors.Is(evalErr, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("cut-short err = %v, want ErrBudgetExhausted", evalErr)
	}
	if reason, ok := triggerrule.SkipReason(evalErr); !ok || reason != triggerrule.SkipReasonBudgetExhausted {
		t.Fatalf("cut-short SkipReason = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonBudgetExhausted)
	}
	if !budget.Exhausted() || budget.Remaining() != 0 {
		t.Fatalf("budget must be exhausted after cut-short ErrBudgetExhausted: Exhausted=%v Remaining=%s", budget.Exhausted(), budget.Remaining())
	}

	// 2. Later benign CEL candidate via Match must skip fail-closed without touching CEL cache.
	cacheBefore := celEng.CacheStats()
	act2 := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: "trig-benign-match", Name: "benign-match"})
	scope2 := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-benign-match", Revision: 1}
	matched, evalErr = rules.Match(ctx, scope2, triggerrule.Rule{
		Expr: `event.payload.env == "prod"`,
	}, act2, triggerrule.WithBudget(budget))
	if matched {
		t.Fatalf("benign Match candidate after budget exhaustion unexpectedly matched")
	}
	if !errors.Is(evalErr, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("benign Match err = %v, want ErrBudgetExhausted", evalErr)
	}
	if reason, ok := triggerrule.SkipReason(evalErr); !ok || reason != triggerrule.SkipReasonBudgetExhausted {
		t.Fatalf("benign Match SkipReason = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonBudgetExhausted)
	}
	if got := celEng.CacheStats(); got != cacheBefore {
		t.Fatalf("CEL cache touched after cut-short budget exhaustion (Match): got %+v, want %+v", got, cacheBefore)
	}

	// 3. Later benign CEL candidate via MatchAll must also skip fail-closed without touching CEL cache.
	act3 := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: "trig-benign-matchall", Name: "benign-matchall"})
	scope3 := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-benign-matchall", Revision: 1}
	res, evalErr := rules.MatchAll(ctx, scope3, []triggerrule.Rule{
		{Expr: `event.payload.env == "prod"`},
	}, act3, triggerrule.WithBudget(budget))
	if res.Matched {
		t.Fatalf("benign MatchAll candidate after budget exhaustion unexpectedly matched")
	}
	if res.Class != triggerrule.MatchClassBudget || res.Reason != triggerrule.SkipReasonBudgetExhausted {
		t.Fatalf("benign MatchAll result = %+v, want Class=%q Reason=%q", res, triggerrule.MatchClassBudget, triggerrule.SkipReasonBudgetExhausted)
	}
	if !errors.Is(evalErr, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("benign MatchAll err = %v, want ErrBudgetExhausted", evalErr)
	}
	if got := celEng.CacheStats(); got != cacheBefore {
		t.Fatalf("CEL cache touched after cut-short budget exhaustion (MatchAll): got %+v, want %+v", got, cacheBefore)
	}
}
