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

package messagepolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
)

// Evaluator compiles, caches, invalidates, and evaluates message_policy CEL
// rules over a shared *ruleengine.Engine. It owns no cel.Env or program cache
// of its own.
type Evaluator struct {
	engine *ruleengine.Engine
}

// Engine aliases Evaluator for callers referencing *messagepolicy.Engine.
type Engine = Evaluator

// New wraps a shared *ruleengine.Engine on which ConsumerName ("message_policy")
// is admitted to DialectCEL and the "message_policy" CEL environment is registered.
func New(engine *ruleengine.Engine) (*Evaluator, error) {
	if engine == nil {
		return nil, errors.New("messagepolicy: nil ruleengine.Engine")
	}
	d, err := engine.DialectFor(ConsumerName, ruleengine.DialectCEL)
	if err != nil {
		return nil, fmt.Errorf("messagepolicy: engine does not support dialect %q for consumer %q: %w", ruleengine.DialectCEL, ConsumerName, err)
	}
	if cd, isCEL := d.(*ruleengine.CELDialect); isCEL {
		if _, hasEnv := cd.CELEngine(EnvironmentName); !hasEnv {
			return nil, fmt.Errorf("messagepolicy: CEL dialect is missing environment %q", EnvironmentName)
		}
	}
	return &Evaluator{engine: engine}, nil
}

// NewSendBudget creates a 250ms aggregate per-send BudgetTracker (brief §1).
func NewSendBudget() *ruleengine.BudgetTracker {
	return ruleengine.NewBudgetTracker(DefaultSendBudget)
}

// ScopeForPolicy builds the CacheScope for a persisted message policy at revision.
func ScopeForPolicy(policyID string, revision int64) celengine.CacheScope {
	return celengine.CacheScope{
		Consumer: ConsumerName,
		ID:       policyID,
		Revision: revision,
	}
}

// CompileViolation validates and compiles expr for authoring-time checks without
// populating the runtime program LRU cache.
func (e *Evaluator) CompileViolation(ctx context.Context, expr string) error {
	if strings.TrimSpace(expr) == "" {
		return errors.New("messagepolicy: violation_rule must not be empty")
	}
	_, err := e.engine.Compile(ctx, ruleengine.DialectCEL, expr, KindViolation, ViolationEnvironmentRef())
	return err
}

// CompileSystemOne validates and compiles a System One judgments rule JSON
// against the message_policy v1 ContentEnvironment via ruleengine.Engine.
func (e *Evaluator) CompileSystemOne(ctx context.Context, rawRule string) (ruleengine.Compiled, error) {
	if strings.TrimSpace(rawRule) == "" {
		return ruleengine.Compiled{}, errors.New("messagepolicy: system_one_rule must not be empty")
	}
	return e.engine.Compile(ctx, systemone.DialectName, rawRule, KindJudgments, SystemOneEnvironmentRef())
}

// EvaluateSystemOne compiles and evaluates a System One judgments rule against
// req (which carries the consumer-authorized ProjectID and typed Input) via
// ruleengine.Engine. On Success it returns ClassPass with Evaluation and
// extracted Judgments (Flags/Labels only); on Uncertain or Failed it returns
// the classified error with Evaluation attached.
func (e *Evaluator) EvaluateSystemOne(
	ctx context.Context,
	rawRule string,
	req systemone.Request,
	opts ...EvalOption,
) (*systemone.Evaluation, Classification) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, ClassifyError(&ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err})
		}
	}
	cfg := applyEvalOpts(opts)
	compiled, err := e.CompileSystemOne(ctx, rawRule)
	if err != nil {
		return nil, ClassifyError(err)
	}
	dec, err := e.engine.Evaluate(ctx, compiled, ruleengine.Input{
		Payload: req,
	}, ruleengine.Budget{
		MaxEvalTime: cfg.maxEvalTime,
		Tracker:     cfg.tracker,
	})
	if err != nil {
		cls := ClassifyError(err)
		return cls.Evaluation, cls
	}
	eval, _ := dec.Detail.(*systemone.Evaluation)
	judgments := ExtractJudgments(eval)
	return eval, Classification{
		Class:      ClassPass,
		Evaluation: eval,
		Judgments:  judgments,
	}
}

// EvaluatePolicy evaluates a message policy (optional system_one_rule followed
// by required CEL violation_rule) for policyID@revision. When systemOneRule is
// non-empty, System One runs first with the consumer-authorized projectID;
// Uncertain and Failed results short-circuit before CEL (consumer design §5.5).
// On System One Success, extracted judgment_flags and judgment_labels are fed
// into the CEL activation (judgment_scores stays empty until M2b).
func (e *Evaluator) EvaluatePolicy(
	ctx context.Context,
	policyID string,
	revision int64,
	violationRule string,
	systemOneRule string,
	projectID string,
	facts Facts,
	celOpts []EvalOption,
	soOpts []EvalOption,
) Classification {
	return e.evaluatePolicy(ctx, policyID, revision, violationRule, systemOneRule, projectID, facts, true, celOpts, soOpts)
}

// EvaluatePolicyTest evaluates a message policy without caching the compiled
// CEL program, for the policy test endpoint. When systemOneRule is non-empty,
// this performs a live System One evaluation against the configured provider
// (sending selected fields, potentially incurring provider cost, with no
// automatic replay or retry).
func (e *Evaluator) EvaluatePolicyTest(
	ctx context.Context,
	policyID string,
	revision int64,
	violationRule string,
	systemOneRule string,
	projectID string,
	facts Facts,
	celOpts []EvalOption,
	soOpts []EvalOption,
) Classification {
	return e.evaluatePolicy(ctx, policyID, revision, violationRule, systemOneRule, projectID, facts, false, celOpts, soOpts)
}

func (e *Evaluator) evaluatePolicy(
	ctx context.Context,
	policyID string,
	revision int64,
	violationRule string,
	systemOneRule string,
	projectID string,
	facts Facts,
	useCELCache bool,
	celOpts []EvalOption,
	soOpts []EvalOption,
) Classification {
	if strings.TrimSpace(systemOneRule) == "" {
		act := NewActivation(facts)
		if useCELCache {
			return e.EvaluateViolation(ctx, policyID, revision, violationRule, act, celOpts...)
		}
		return e.EvaluateExpr(ctx, violationRule, act, celOpts...)
	}

	req := systemone.Request{
		Input:     BuildSystemOneInput(facts),
		ProjectID: projectID,
		RuleID:    policyID,
		Revision:  revision,
	}
	eval, soCls := e.EvaluateSystemOne(ctx, systemOneRule, req, soOpts...)
	if soCls.Class != ClassPass {
		return soCls
	}

	factsWithJudgments := facts
	factsWithJudgments.Judgments = Judgments{
		Flags:  soCls.Judgments.Flags,
		Labels: soCls.Judgments.Labels,
	}
	act := NewActivation(factsWithJudgments)
	var celCls Classification
	if useCELCache {
		celCls = e.EvaluateViolation(ctx, policyID, revision, violationRule, act, celOpts...)
	} else {
		celCls = e.EvaluateExpr(ctx, violationRule, act, celOpts...)
	}
	celCls.Evaluation = eval
	celCls.Judgments = soCls.Judgments
	return celCls
}

// Invalidate removes cached compiled program bindings for policyID at or below
// revision, always populating Consumer: "message_policy" so ruleengine's
// consumer-admitted Invalidate takes effect (brief §1a, target 5).
func (e *Evaluator) Invalidate(policyID string, revision int64) {
	e.InvalidateScope(ScopeForPolicy(policyID, revision))
}

// InvalidateScope removes cached compiled program bindings for scope, ensuring
// scope.Consumer is ConsumerName ("message_policy").
func (e *Evaluator) InvalidateScope(scope celengine.CacheScope) {
	scope.Consumer = ConsumerName
	e.engine.Invalidate(ruleengine.DialectCEL, EnvironmentName, scope)
}

// EvalOption configures per-evaluation bounds on EvaluateViolation / EvaluateExpr.
type EvalOption func(*evalConfig)

type evalConfig struct {
	tracker     *ruleengine.BudgetTracker
	maxEvalTime time.Duration
}

// WithBudget attaches an aggregate per-send BudgetTracker (see NewSendBudget).
func WithBudget(tracker *ruleengine.BudgetTracker) EvalOption {
	return func(c *evalConfig) {
		c.tracker = tracker
	}
}

// WithMaxEvalTime lowers the single-rule evaluation timeout ceiling for this call.
func WithMaxEvalTime(d time.Duration) EvalOption {
	return func(c *evalConfig) {
		c.maxEvalTime = d
	}
}

func applyEvalOpts(opts []EvalOption) evalConfig {
	var cfg evalConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// EvaluateViolation compiles (via the engine's content-addressed cache) and
// evaluates expr for policyID@revision against act, returning its Classification.
func (e *Evaluator) EvaluateViolation(
	ctx context.Context,
	policyID string,
	revision int64,
	expr string,
	act Activation,
	opts ...EvalOption,
) Classification {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return ClassifyError(&ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err})
		}
	}
	cfg := applyEvalOpts(opts)
	if cfg.tracker != nil && cfg.tracker.Exhausted() {
		return ClassifyError(ruleengine.ErrBudgetExhausted)
	}

	compileStart := time.Now()
	compiled, err := e.engine.CachedCompile(
		ctx,
		ruleengine.DialectCEL,
		ScopeForPolicy(policyID, revision),
		expr,
		KindViolation,
		ViolationEnvironmentRef(),
	)
	if cfg.tracker != nil {
		cfg.tracker.Charge(time.Since(compileStart))
	}
	if err != nil {
		return ClassifyError(err)
	}
	return e.evalCompiled(ctx, compiled, act, cfg)
}

// EvaluateExpr compiles expr without caching and evaluates it against act.
// Used by the policy test endpoint so transient authoring tests do not churn
// the runtime LRU cache.
func (e *Evaluator) EvaluateExpr(
	ctx context.Context,
	expr string,
	act Activation,
	opts ...EvalOption,
) Classification {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return ClassifyError(&ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err})
		}
	}
	cfg := applyEvalOpts(opts)
	if cfg.tracker != nil && cfg.tracker.Exhausted() {
		return ClassifyError(ruleengine.ErrBudgetExhausted)
	}

	compileStart := time.Now()
	compiled, err := e.engine.Compile(
		ctx,
		ruleengine.DialectCEL,
		expr,
		KindViolation,
		ViolationEnvironmentRef(),
	)
	if cfg.tracker != nil {
		cfg.tracker.Charge(time.Since(compileStart))
	}
	if err != nil {
		return ClassifyError(err)
	}
	return e.evalCompiled(ctx, compiled, act, cfg)
}

func (e *Evaluator) evalCompiled(
	ctx context.Context,
	compiled ruleengine.Compiled,
	act Activation,
	cfg evalConfig,
) Classification {
	dec, err := e.engine.Evaluate(ctx, compiled, ruleengine.Input{
		Activation: act.Map(),
	}, ruleengine.Budget{
		MaxEvalTime: cfg.maxEvalTime,
		Tracker:     cfg.tracker,
	})
	var stats celengine.EvalStats
	if s, ok := dec.Detail.(celengine.EvalStats); ok {
		stats = s
	}
	return ClassifyDecision(dec.Bool, stats, err)
}
