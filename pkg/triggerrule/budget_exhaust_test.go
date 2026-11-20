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

type triggerBudgetProbeDialect struct {
	name        string
	maxEvalTime time.Duration
	waitCtxDone bool
}

func (d *triggerBudgetProbeDialect) Name() string { return d.name }
func (d *triggerBudgetProbeDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		MaxEvalTime: d.maxEvalTime,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *triggerBudgetProbeDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, rule), nil
}
func (d *triggerBudgetProbeDialect) Evaluate(ctx context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	if d.waitCtxDone {
		<-ctx.Done()
		return ruleengine.Decision{}, &ruleengine.FailedError{
			Reason: celengine.ReasonTimeout,
			Cause:  ctx.Err(),
		}
	}
	return ruleengine.Decision{}, &ruleengine.FailedError{
		Reason: celengine.ReasonTimeout,
		Cause:  context.DeadlineExceeded,
	}
}

func TestTrackerDeadlineVsPerRuleTimeoutOnSharedEventBudget(t *testing.T) {
	ctx := context.Background()

	celEng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	waitDialect := &triggerBudgetProbeDialect{name: "wait_deadline", maxEvalTime: 100 * time.Millisecond, waitCtxDone: true}
	fastTimeout := &triggerBudgetProbeDialect{name: "fast_timeout", maxEvalTime: 100 * time.Millisecond, waitCtxDone: false}

	re := ruleengine.NewEngine()
	if err := re.Register(celDialect); err != nil {
		t.Fatalf("Register cel: %v", err)
	}
	if err := re.Register(waitDialect); err != nil {
		t.Fatalf("Register wait_deadline: %v", err)
	}
	if err := re.Register(fastTimeout); err != nil {
		t.Fatalf("Register fast_timeout: %v", err)
	}
	if err := re.AllowConsumerDialects(
		triggerrule.ConsumerName,
		ruleengine.Grant(ruleengine.DialectCEL),
		ruleengine.Grant("wait_deadline"),
		ruleengine.Grant("fast_timeout"),
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

	t.Run("tracker_deadline_skips_later_benign_rule_without_touching_CEL_cache", func(t *testing.T) {
		// Tracker limit (10ms) < waitDialect MaxEvalTime (100ms) => boundFromTracker is true.
		budget := ruleengine.NewBudgetTracker(10 * time.Millisecond)

		act1 := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: "trig-wait-deadline", Name: "wait-deadline"})
		scope1 := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-wait-deadline", Revision: 1}
		matched, evalErr := rules.Match(ctx, scope1, triggerrule.Rule{
			Dialect: "wait_deadline",
			Expr:    "slow",
		}, act1, triggerrule.WithBudget(budget))
		if matched {
			t.Fatalf("tracker-deadline trigger unexpectedly matched")
		}
		if !errors.Is(evalErr, ruleengine.ErrBudgetExhausted) {
			t.Fatalf("tracker-deadline err = %v, want ErrBudgetExhausted", evalErr)
		}
		if !errors.Is(evalErr, context.DeadlineExceeded) {
			t.Fatalf("tracker-deadline err = %v, want errors.Is(context.DeadlineExceeded)", evalErr)
		}
		if reason, ok := triggerrule.SkipReason(evalErr); !ok || reason != triggerrule.SkipReasonBudgetExhausted {
			t.Fatalf("tracker-deadline SkipReason = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonBudgetExhausted)
		}
		if !budget.Exhausted() || budget.Remaining() != 0 {
			t.Fatalf("budget must be exhausted after tracker deadline: Exhausted=%v Remaining=%s", budget.Exhausted(), budget.Remaining())
		}

		// Later benign CEL candidate via Match must skip fail-closed without touching CEL cache.
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
			t.Fatalf("CEL cache touched after tracker-deadline budget exhaustion (Match): got %+v, want %+v", got, cacheBefore)
		}

		// Later benign CEL candidate via MatchAll must also skip fail-closed without touching CEL cache.
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
			t.Fatalf("CEL cache touched after tracker-deadline budget exhaustion (MatchAll): got %+v, want %+v", got, cacheBefore)
		}
	})

	t.Run("shorter_per_rule_timeout_does_not_exhaust_shared_budget", func(t *testing.T) {
		// Tracker limit (50ms) < fastTimeout MaxEvalTime (100ms) => boundFromTracker is true,
		// but the dialect returns ReasonTimeout immediately with budget left.
		budget := ruleengine.NewBudgetTracker(50 * time.Millisecond)

		act1 := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: "trig-fast-timeout", Name: "fast-timeout"})
		scope1 := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-fast-timeout", Revision: 1}
		matched, evalErr := rules.Match(ctx, scope1, triggerrule.Rule{
			Dialect: "fast_timeout",
			Expr:    "fast",
		}, act1, triggerrule.WithBudget(budget))
		if matched {
			t.Fatalf("fast-timeout trigger unexpectedly matched")
		}
		if errors.Is(evalErr, ruleengine.ErrBudgetExhausted) {
			t.Fatalf("shorter per-rule timeout unexpectedly returned ErrBudgetExhausted: %v", evalErr)
		}
		var failedErr *ruleengine.FailedError
		if !errors.As(evalErr, &failedErr) || failedErr.Reason != celengine.ReasonTimeout {
			t.Fatalf("shorter per-rule timeout err = %T (%v), want *FailedError{Reason: %q}", evalErr, evalErr, celengine.ReasonTimeout)
		}
		if reason, ok := triggerrule.SkipReason(evalErr); !ok || reason != triggerrule.SkipReasonEvalFailed {
			t.Fatalf("shorter per-rule timeout SkipReason = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonEvalFailed)
		}
		if budget.Exhausted() || budget.Remaining() <= 0 {
			t.Fatalf("shorter per-rule timeout unexpectedly exhausted shared budget: Exhausted=%v Remaining=%s", budget.Exhausted(), budget.Remaining())
		}

		// Later benign CEL candidate on the same event must still evaluate and match.
		act2 := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: "trig-benign-after-per-rule", Name: "benign-after-per-rule"})
		scope2 := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-benign-after-per-rule", Revision: 1}
		matched, evalErr = rules.Match(ctx, scope2, triggerrule.Rule{
			Expr: `event.payload.env == "prod"`,
		}, act2, triggerrule.WithBudget(budget))
		if evalErr != nil || !matched {
			t.Fatalf("benign candidate after shorter per-rule timeout = (%v, %v), want (true, nil)", matched, evalErr)
		}
	})
}
