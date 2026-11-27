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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// lateSuccessFakeDialect ignores evalCtx expiration / cancellation and returns
// a nil-error Decision after sleeping for delay (or after running onEval).
// This characterises how Engine.Evaluate handles a dialect that returns a
// decision with err == nil after the dispatch-time bound has passed (§3.4).
type lateSuccessFakeDialect struct {
	name        string
	maxEvalTime time.Duration
	delay       time.Duration
	onEval      func()
}

func (d *lateSuccessFakeDialect) Name() string { return d.name }

func (d *lateSuccessFakeDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: d.maxEvalTime,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}

func (d *lateSuccessFakeDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, rule), nil
}

func (d *lateSuccessFakeDialect) Evaluate(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	if d.onEval != nil {
		d.onEval()
	}
	if d.delay > 0 {
		time.Sleep(d.delay)
	}
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

// TestEvaluate_LateSuccessCharacterisation pins the pre-fix behaviour of
// Engine.Evaluate on late nil-error decisions across all three bounding causes
// (caller cancel, per-call/dialect timeout, aggregate tracker timeout) plus the
// margin-preserved in-time case, and confirms CEL's existing post-return
// backstop (shared-rule-chain-design v1 §3.4, §4 acceptance 5).
//
// In Step 3 (late-success fix), the three late-success subtests below will be
// updated to assert that late nil-error decisions are discarded and classified
// by the bounding cause (canceled / failed/timeout / budget).
func TestEvaluate_LateSuccessCharacterisation(t *testing.T) {
	newEngineWith := func(t *testing.T, d ruleengine.Dialect) (*ruleengine.Engine, ruleengine.Compiled) {
		t.Helper()
		eng := ruleengine.NewEngine()
		if err := eng.Register(d); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := eng.AllowConsumerDialects("char_consumer", ruleengine.GrantIO(d.Name())); err != nil {
			t.Fatalf("AllowConsumerDialects: %v", err)
		}
		comp, err := eng.Compile(context.Background(), d.Name(), "rule", ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: "char_consumer",
			Name:     "test",
		})
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		return eng, comp
	}

	t.Run("caller_canceled_during_eval_nil_err_currently_succeeds", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		d := &lateSuccessFakeDialect{
			name:        "late_cancel",
			maxEvalTime: time.Second,
			onEval:      cancel,
		}
		eng, comp := newEngineWith(t, d)

		dec, err := eng.Evaluate(ctx, comp, ruleengine.Input{}, ruleengine.Budget{})
		// Pre-fix baseline: Engine.Evaluate only checks ctx.Err() when err != nil,
		// so a nil-error decision returned after caller cancel currently slips through.
		if err != nil || !dec.Bool {
			t.Fatalf("pre-fix baseline expected (true, nil), got (%+v, %v)", dec, err)
		}
	})

	t.Run("per_call_timeout_exceeded_nil_err_currently_succeeds", func(t *testing.T) {
		d := &lateSuccessFakeDialect{
			name:        "late_timeout",
			maxEvalTime: 25 * time.Millisecond,
			delay:       60 * time.Millisecond,
		}
		eng, comp := newEngineWith(t, d)

		dec, err := eng.Evaluate(context.Background(), comp, ruleengine.Input{}, ruleengine.Budget{})
		// Pre-fix baseline: Engine.Evaluate does not check evalCtx / elapsed on err == nil.
		if err != nil || !dec.Bool {
			t.Fatalf("pre-fix baseline expected (true, nil), got (%+v, %v)", dec, err)
		}
	})

	t.Run("tracker_timeout_exceeded_nil_err_currently_succeeds", func(t *testing.T) {
		d := &lateSuccessFakeDialect{
			name:        "late_tracker",
			maxEvalTime: time.Second,
			delay:       60 * time.Millisecond,
		}
		eng, comp := newEngineWith(t, d)
		tracker := ruleengine.NewBudgetTracker(25 * time.Millisecond)

		dec, err := eng.Evaluate(context.Background(), comp, ruleengine.Input{}, ruleengine.Budget{Tracker: tracker})
		// Pre-fix baseline: Engine.Evaluate charges the tracker but still returns (true, nil).
		if err != nil || !dec.Bool {
			t.Fatalf("pre-fix baseline expected (true, nil), got (%+v, %v)", dec, err)
		}
		if !tracker.Exhausted() {
			t.Fatalf("expected tracker to be exhausted after 60ms charge on 25ms limit")
		}
	})

	t.Run("in_time_within_bound_succeeds", func(t *testing.T) {
		d := &lateSuccessFakeDialect{
			name:        "in_time",
			maxEvalTime: 500 * time.Millisecond,
			delay:       5 * time.Millisecond,
		}
		eng, comp := newEngineWith(t, d)
		tracker := ruleengine.NewBudgetTracker(500 * time.Millisecond)

		dec, err := eng.Evaluate(context.Background(), comp, ruleengine.Input{}, ruleengine.Budget{Tracker: tracker})
		if err != nil || !dec.Bool {
			t.Fatalf("in-time eval got (%+v, %v), want (true, nil)", dec, err)
		}
	})

	t.Run("cel_existing_post_return_backstop_rejects_canceled_ctx", func(t *testing.T) {
		celEng, err := celengine.New(celengine.Environment{
			Name:    "char_cel",
			Version: 1,
			Variables: []celengine.Variable{
				{Name: "x", Type: celengine.Bool()},
			},
		}, celengine.DefaultLimits(), celengine.DefaultCacheConfig())
		if err != nil {
			t.Fatalf("celengine.New: %v", err)
		}
		celDialect, err := ruleengine.NewCELDialect(celEng)
		if err != nil {
			t.Fatalf("NewCELDialect: %v", err)
		}
		eng := ruleengine.NewEngine()
		if err := eng.Register(celDialect); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := eng.AllowConsumerDialects("char_consumer", ruleengine.Grant(ruleengine.DialectCEL)); err != nil {
			t.Fatalf("AllowConsumerDialects: %v", err)
		}
		comp, err := eng.Compile(context.Background(), ruleengine.DialectCEL, "x == true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: "char_consumer",
			Name:     "char_cel",
		})
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = eng.Evaluate(ctx, comp, ruleengine.Input{Activation: map[string]any{"x": true}}, ruleengine.Budget{})
		var failedErr *ruleengine.FailedError
		if !errors.As(err, &failedErr) || failedErr.Reason != celengine.ReasonCanceled {
			t.Fatalf("CEL canceled eval err = %T (%v), want *FailedError{Reason: canceled}", err, err)
		}
	})
}
