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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

func newTestCELEngine(t *testing.T, name string) *celengine.Engine {
	t.Helper()
	eng, err := celengine.New(celengine.Environment{
		Name:    name,
		Version: 1,
		Variables: []celengine.Variable{
			{Name: "event", Type: celengine.Map(celengine.String(), celengine.Dyn())},
			{Name: "items", Type: celengine.List(celengine.String())},
		},
		Extensions: []celengine.Extension{celengine.ExtensionStrings},
		Macros:     celengine.MacrosStandard,
	}, celengine.DefaultLimits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	return eng
}

func TestCELDialectCompileEvaluateAndCapabilities(t *testing.T) {
	celEng := newTestCELEngine(t, "trigger")
	dialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	if dialect.Name() != ruleengine.DialectCEL {
		t.Fatalf("Name() = %q, want %q", dialect.Name(), ruleengine.DialectCEL)
	}
	caps := dialect.Declares()
	if caps.PerformsIO {
		t.Fatalf("CEL dialect must declare PerformsIO=false")
	}

	eng := ruleengine.NewEngine()
	if err := eng.Register(dialect); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := eng.AllowConsumerDialects("trigger", ruleengine.Grant(ruleengine.DialectCEL)); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}

	ctx := context.Background()
	envRef := ruleengine.EnvironmentRef{Consumer: "trigger", Name: "trigger"}

	// Filter kind -> bool Decision.
	compiledFilter, err := eng.Compile(ctx, "", `event.type == "webhook.received"`, ruleengine.KindFilter, envRef)
	if err != nil {
		t.Fatalf("Compile filter: %v", err)
	}
	dec, err := eng.Evaluate(ctx, compiledFilter, ruleengine.Input{
		Activation: map[string]any{
			"event": map[string]any{"type": "webhook.received", "subject": "svc/api"},
		},
	}, ruleengine.Budget{})
	if err != nil {
		t.Fatalf("Evaluate filter: %v", err)
	}
	if dec.Kind != ruleengine.KindFilter || !dec.Bool {
		t.Fatalf("Decision = %+v, want KindFilter Bool=true", dec)
	}

	// Subject kind -> string Decision.
	compiledSubj, err := eng.Compile(ctx, "cel", `event.subject`, ruleengine.KindSubject, ruleengine.EnvironmentRef{
		Consumer:     "trigger",
		Name:         "trigger",
		MaxRuleBytes: 1024,
	})
	if err != nil {
		t.Fatalf("Compile subject: %v", err)
	}
	subjDec, err := eng.Evaluate(ctx, compiledSubj, ruleengine.Input{
		Activation: map[string]any{
			"event": map[string]any{"type": "webhook.received", "subject": "svc/api"},
		},
	}, ruleengine.Budget{})
	if err != nil {
		t.Fatalf("Evaluate subject: %v", err)
	}
	if subjDec.Kind != ruleengine.KindSubject || subjDec.String != "svc/api" {
		t.Fatalf("Decision = %+v, want KindSubject String=\"svc/api\"", subjDec)
	}

	// Structured CompileError passes through unchanged for 400 invalid_rule formatting.
	_, err = eng.Compile(ctx, "cel", strings.Repeat("x", 1025), ruleengine.KindSubject, ruleengine.EnvironmentRef{
		Consumer:     "trigger",
		Name:         "trigger",
		MaxRuleBytes: 1024,
	})
	var cerr *celengine.CompileError
	if !errors.As(err, &cerr) || cerr.Category != celengine.CategorySize {
		t.Fatalf("expected *celengine.CompileError(size), got %T: %v", err, err)
	}
}

func TestUnsupportedAndDisallowedDialects(t *testing.T) {
	celEng := newTestCELEngine(t, "trigger")
	dialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}

	eng := ruleengine.NewEngine()
	if err := eng.Register(dialect); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Duplicate registration is rejected.
	if err := eng.Register(dialect); err == nil {
		t.Fatalf("expected error registering duplicate dialect")
	}

	// Unregistered consumer gets consumer_not_registered even for registered "cel".
	envRef := ruleengine.EnvironmentRef{Consumer: "trigger", Name: "trigger"}
	ctx := context.Background()
	_, err = eng.Compile(ctx, "cel", "true", ruleengine.KindFilter, envRef)
	var unsupported *ruleengine.UnsupportedDialectError
	if !errors.As(err, &unsupported) || unsupported.Reason != ruleengine.ReasonConsumerNotRegistered {
		t.Fatalf("expected *UnsupportedDialectError(consumer_not_registered), got %T: %v", err, err)
	}

	// Register consumer for both "cel" and a future dialect "system_one" (not yet registered on eng).
	if err := eng.AllowConsumerDialects("trigger", ruleengine.Grant(ruleengine.DialectCEL), ruleengine.GrantIO("system_one")); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}

	// Unregistered dialect that the consumer did grant -> unknown_dialect.
	_, err = eng.Compile(ctx, "system_one", "urgent issue", ruleengine.KindFilter, envRef)
	unsupported = nil
	if !errors.As(err, &unsupported) || unsupported.Reason != ruleengine.ReasonUnknownDialect {
		t.Fatalf("expected *UnsupportedDialectError(unknown_dialect), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "unsupported dialect") || !strings.HasSuffix(err.Error(), ": unknown_dialect") {
		t.Fatalf("error %q must contain 'unsupported dialect' and end with ': unknown_dialect'", err.Error())
	}

	// Consumer dialect allow-list: if consumer restricts to an empty/different set, CEL is rejected with dialect_not_allowed.
	if err := eng.AllowConsumerDialects("restricted_consumer", ruleengine.Grant("other_dialect")); err != nil {
		t.Fatalf("AllowConsumerDialects restricted: %v", err)
	}
	_, err = eng.Compile(ctx, "cel", "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "restricted_consumer",
		Name:     "trigger",
	})
	unsupported = nil
	if !errors.As(err, &unsupported) || unsupported.Reason != ruleengine.ReasonDialectNotAllowed {
		t.Fatalf("expected *UnsupportedDialectError(dialect_not_allowed) for disallowed consumer dialect, got %T: %v", err, err)
	}
}

type stubModelDialect struct {
	mode       string
	performsIO bool
	compileCnt int
	evalCnt    int
}

func (d *stubModelDialect) Name() string { return "stub_model" }
func (d *stubModelDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  d.performsIO || d.mode != "dynamic_no_io",
		MaxEvalTime: 50 * time.Millisecond,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *stubModelDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	d.compileCnt++
	return ruleengine.NewCompiled(env.Consumer, d.Name(), kind, rule), nil
}
func (d *stubModelDialect) Evaluate(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	d.evalCnt++
	switch d.mode {
	case "uncertain":
		return ruleengine.Decision{}, &ruleengine.UncertainError{Reason: "low_confidence"}
	case "failed":
		return ruleengine.Decision{}, &ruleengine.FailedError{Reason: "provider_unavailable"}
	case "hang":
		<-ctx.Done()
		return ruleengine.Decision{}, &ruleengine.FailedError{Reason: "deadline_exceeded", Cause: ctx.Err()}
	default:
		return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
	}
}

func TestUncertainVsFailedErrorsAndBudgetTracker(t *testing.T) {
	eng := ruleengine.NewEngine()
	stub := &stubModelDialect{performsIO: true}
	if err := eng.Register(stub); err != nil {
		t.Fatalf("Register stub: %v", err)
	}
	if err := eng.AllowConsumerDialects("test", ruleengine.GrantIO("stub_model")); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}
	ctx := context.Background()
	envRef := ruleengine.EnvironmentRef{Consumer: "test", Name: "test"}
	compiled, err := eng.Compile(ctx, "stub_model", "check", ruleengine.KindFilter, envRef)
	if err != nil {
		t.Fatalf("Compile stub: %v", err)
	}

	// Uncertain error is distinct from FailedError and never returns Bool=true.
	stub.mode = "uncertain"
	dec, err := eng.Evaluate(ctx, compiled, ruleengine.Input{}, ruleengine.Budget{})
	if dec.Bool {
		t.Fatalf("UncertainError must never produce Bool=true")
	}
	var uerr *ruleengine.UncertainError
	var ferr *ruleengine.FailedError
	if !errors.As(err, &uerr) || errors.As(err, &ferr) {
		t.Fatalf("expected UncertainError only, got %T: %v", err, err)
	}

	// FailedError is distinct from UncertainError and never returns Bool=true.
	stub.mode = "failed"
	dec, err = eng.Evaluate(ctx, compiled, ruleengine.Input{}, ruleengine.Budget{})
	if dec.Bool {
		t.Fatalf("FailedError must never produce Bool=true")
	}
	if !errors.As(err, &ferr) || errors.As(err, &uerr) {
		t.Fatalf("expected FailedError only, got %T: %v", err, err)
	}

	// Dialect MaxEvalTime bounds hanging dialect evaluation.
	stub.mode = "hang"
	start := time.Now()
	_, err = eng.Evaluate(ctx, compiled, ruleengine.Input{}, ruleengine.Budget{})
	if err == nil {
		t.Fatalf("expected error from hanging dialect")
	}
	maxHangElapsed := 500 * time.Millisecond
	if raceEnabled {
		maxHangElapsed = 3 * time.Second
	}
	if elapsed := time.Since(start); elapsed > maxHangElapsed {
		t.Fatalf("MaxEvalTime did not bound dialect evaluation: took %v (max %v)", elapsed, maxHangElapsed)
	}

	// Exhausted aggregate tracker rejects before invoking dialect.
	tracker := ruleengine.NewBudgetTracker(1 * time.Nanosecond)
	tracker.Charge(10 * time.Millisecond)
	stub.mode = "ok"
	_, err = eng.Evaluate(ctx, compiled, ruleengine.Input{}, ruleengine.Budget{Tracker: tracker})
	if !errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("expected ErrBudgetExhausted, got %v", err)
	}
}

func TestEvaluateTimeoutVsCallerCancellationAndTrackerRemainder(t *testing.T) {
	limits := celengine.DefaultLimits()
	limits.CostLimit = celengine.MaxCostLimitCeiling
	limits.EvalTimeout = 20 * time.Millisecond
	if raceEnabled {
		limits.EvalTimeout = 80 * time.Millisecond
	}
	limits.InterruptFrequency = 1

	env := celengine.Environment{
		Name:    "timeout_test",
		Version: 1,
		Variables: []celengine.Variable{
			{Name: "items", Type: celengine.List(celengine.String())},
		},
		Macros: celengine.MacrosStandard,
	}
	celEng, err := celengine.New(env, limits, celengine.DefaultCacheConfig())
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
	if err := eng.AllowConsumerDialects("trigger", ruleengine.Grant(ruleengine.DialectCEL)); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}

	compiled, err := eng.Compile(
		context.Background(),
		ruleengine.DialectCEL,
		`items.all(a, items.all(b, items.all(c, a.size() + b.size() + c.size() >= 0)))`,
		ruleengine.KindFilter,
		ruleengine.EnvironmentRef{Consumer: "trigger", Name: "timeout_test"},
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	listVal := make([]string, 150)
	for i := range listVal {
		listVal[i] = "elem"
	}
	input := ruleengine.Input{Activation: map[string]any{"items": listVal}}

	// 1. Dialect/engine timeout surfaces as ReasonTimeout, not ReasonCanceled.
	_, err = eng.Evaluate(context.Background(), compiled, input, ruleengine.Budget{})
	var ferr *ruleengine.FailedError
	if !errors.As(err, &ferr) || ferr.Reason != celengine.ReasonTimeout {
		t.Fatalf("engine timeout: got %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonTimeout)
	}

	// 2. Remaining tracker budget tighter than dialect MaxEvalTime surfaces ErrBudgetExhausted.
	remTracker := ruleengine.NewBudgetTracker(10 * time.Millisecond)
	_, err = eng.Evaluate(context.Background(), compiled, input, ruleengine.Budget{Tracker: remTracker})
	if !errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("tracker remainder expiry: got %T (%v), want ErrBudgetExhausted", err, err)
	}

	// 3. Caller context expiring mid-eval surfaces ReasonCanceled.
	callerCtx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err = eng.Evaluate(callerCtx, compiled, input, ruleengine.Budget{})
	ferr = nil
	if !errors.As(err, &ferr) || ferr.Reason != celengine.ReasonCanceled {
		t.Fatalf("caller deadline mid-eval: got %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonCanceled)
	}

	// 4. Pre-canceled caller context surfaces ReasonCanceled even when tracker is exhausted.
	preCanceled, preCancel := context.WithCancel(context.Background())
	preCancel()
	_, err = eng.Evaluate(preCanceled, compiled, input, ruleengine.Budget{Tracker: remTracker})
	ferr = nil
	if !errors.As(err, &ferr) || ferr.Reason != celengine.ReasonCanceled {
		t.Fatalf("pre-canceled ctx: got %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonCanceled)
	}
}

type typedPayload struct {
	Verdict string
	Score   float64
}

type payloadCapturingDialect struct {
	gotPayload any
	returnErr  func(ctx context.Context) error
}

func (d *payloadCapturingDialect) Name() string { return "typed_dialect" }
func (d *payloadCapturingDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: 25 * time.Millisecond,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *payloadCapturingDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.Name(), kind, rule), nil
}
func (d *payloadCapturingDialect) Evaluate(ctx context.Context, c ruleengine.Compiled, input ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	d.gotPayload = input.Payload
	if d.returnErr != nil {
		return ruleengine.Decision{}, d.returnErr(ctx)
	}
	tp, ok := input.Payload.(typedPayload)
	return ruleengine.Decision{
		Kind: c.Kind(),
		Bool: ok && tp.Verdict == "approve" && input.Activation["ok"] == true,
	}, nil
}

func TestInputPayloadPassedToDialectAndIgnoredByCEL(t *testing.T) {
	celEng := newTestCELEngine(t, "trigger")
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	typed := &payloadCapturingDialect{}

	eng := ruleengine.NewEngine()
	if err := eng.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}
	if err := eng.Register(typed); err != nil {
		t.Fatalf("Register typed: %v", err)
	}
	if err := eng.AllowConsumerDialects("system_one", ruleengine.GrantIO("typed_dialect")); err != nil {
		t.Fatalf("AllowConsumerDialects system_one: %v", err)
	}
	if err := eng.AllowConsumerDialects("trigger", ruleengine.Grant(ruleengine.DialectCEL)); err != nil {
		t.Fatalf("AllowConsumerDialects trigger: %v", err)
	}

	ctx := context.Background()
	wantPayload := typedPayload{Verdict: "approve", Score: 0.98}

	// 1. Typed dialect receives Input.Payload beside Input.Activation.
	compTyped, err := eng.Compile(ctx, "typed_dialect", "approve", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "system_one",
		Name:     "system_one",
	})
	if err != nil {
		t.Fatalf("Compile typed: %v", err)
	}
	dec, err := eng.Evaluate(ctx, compTyped, ruleengine.Input{
		Activation: map[string]any{"ok": true},
		Payload:    wantPayload,
	}, ruleengine.Budget{})
	if err != nil {
		t.Fatalf("Evaluate typed: %v", err)
	}
	if !dec.Bool || typed.gotPayload != wantPayload {
		t.Fatalf("got Bool=%v Payload=%#v, want true and %#v", dec.Bool, typed.gotPayload, wantPayload)
	}

	// 2. CELDialect ignores Input.Payload and evaluates Activation unchanged.
	compCEL, err := eng.Compile(ctx, ruleengine.DialectCEL, `event.kind == "deploy"`, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "trigger",
		Name:     "trigger",
	})
	if err != nil {
		t.Fatalf("Compile CEL: %v", err)
	}
	celDec, err := eng.Evaluate(ctx, compCEL, ruleengine.Input{
		Activation: map[string]any{"event": map[string]any{"kind": "deploy"}},
		Payload:    wantPayload,
	}, ruleengine.Budget{})
	if err != nil || !celDec.Bool {
		t.Fatalf("CEL Evaluate with Payload = (%+v, %v), want Bool=true, nil", celDec, err)
	}
}

func TestEvaluateWrapsDialectErrorAcrossTimeoutBudgetAndCancel(t *testing.T) {
	errProviderRootCause := errors.New("provider stream aborted")

	eng := ruleengine.NewEngine()
	dialect := &payloadCapturingDialect{}
	if err := eng.Register(dialect); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := eng.AllowConsumerDialects("system_one", ruleengine.GrantIO("typed_dialect")); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}
	comp, err := eng.Compile(context.Background(), "typed_dialect", "rule", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "system_one",
		Name:     "system_one",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// 1. Dialect timeout replaces outer classification with ReasonTimeout while wrapping
	// the dialect's UncertainError and its Cause.
	dialect.returnErr = func(ctx context.Context) error {
		<-ctx.Done()
		return &ruleengine.UncertainError{
			Reason: "partial_judgment",
			Cause:  errors.Join(ctx.Err(), errProviderRootCause),
		}
	}
	_, err = eng.Evaluate(context.Background(), comp, ruleengine.Input{}, ruleengine.Budget{})
	var outerFailed *ruleengine.FailedError
	if !errors.As(err, &outerFailed) || outerFailed.Reason != celengine.ReasonTimeout {
		t.Fatalf("timeout outer error = %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonTimeout)
	}
	var wrappedUncertain *ruleengine.UncertainError
	if !errors.As(err, &wrappedUncertain) || wrappedUncertain.Reason != "partial_judgment" {
		t.Fatalf("errors.As(*UncertainError) failed on wrapped timeout error: %v", err)
	}
	if !errors.Is(err, errProviderRootCause) {
		t.Fatalf("errors.Is(errProviderRootCause) = false on wrapped timeout error: %v", err)
	}

	// 2. Tracker budget exhaustion returns ErrBudgetExhausted while wrapping the dialect's FailedError and Cause.
	dialect.returnErr = func(ctx context.Context) error {
		<-ctx.Done()
		return &ruleengine.FailedError{
			Reason: "provider_deadline",
			Cause:  errors.Join(ctx.Err(), errProviderRootCause),
		}
	}
	tracker := ruleengine.NewBudgetTracker(10 * time.Millisecond)
	_, err = eng.Evaluate(context.Background(), comp, ruleengine.Input{}, ruleengine.Budget{Tracker: tracker})
	if !errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("budget exhausted error = %v, want errors.Is(ErrBudgetExhausted)", err)
	}
	var wrappedFailed *ruleengine.FailedError
	if !errors.As(err, &wrappedFailed) || wrappedFailed.Reason != "provider_deadline" {
		t.Fatalf("errors.As(*FailedError) failed on wrapped budget error: got %+v (%v)", wrappedFailed, err)
	}
	if !errors.Is(err, errProviderRootCause) {
		t.Fatalf("errors.Is(errProviderRootCause) = false on wrapped budget error: %v", err)
	}

	// 3. Caller cancellation returns outer FailedError{Reason: "canceled"} while wrapping
	// the dialect's UncertainError and its Cause.
	dialect.returnErr = func(ctx context.Context) error {
		<-ctx.Done()
		return &ruleengine.UncertainError{
			Reason: "caller_aborted_stream",
			Cause:  errProviderRootCause,
		}
	}
	callerCtx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err = eng.Evaluate(callerCtx, comp, ruleengine.Input{}, ruleengine.Budget{})
	outerFailed = nil
	if !errors.As(err, &outerFailed) || outerFailed.Reason != celengine.ReasonCanceled {
		t.Fatalf("caller cancel outer error = %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonCanceled)
	}
	wrappedUncertain = nil
	if !errors.As(err, &wrappedUncertain) || wrappedUncertain.Reason != "caller_aborted_stream" {
		t.Fatalf("errors.As(*UncertainError) failed on wrapped caller-cancel error: %v", err)
	}
	if !errors.Is(err, errProviderRootCause) {
		t.Fatalf("errors.Is(errProviderRootCause) = false on wrapped caller-cancel error: %v", err)
	}
}

type countingCachedDialect struct {
	name            string
	performsIO      bool
	compileCnt      int
	cachedCompCnt   int
	invalidateCnt   int
	evalCnt         int
	lastCachedScope celengine.CacheScope
	lastInvalScope  celengine.CacheScope
}

func (d *countingCachedDialect) Name() string { return d.name }
func (d *countingCachedDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  d.performsIO,
		MaxEvalTime: 50 * time.Millisecond,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *countingCachedDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	d.compileCnt++
	return ruleengine.NewCompiled(env.Consumer, d.Name(), kind, rule), nil
}
func (d *countingCachedDialect) CachedCompile(_ context.Context, scope celengine.CacheScope, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	d.cachedCompCnt++
	d.lastCachedScope = scope
	return ruleengine.NewCompiled(env.Consumer, d.Name(), kind, rule), nil
}
func (d *countingCachedDialect) Invalidate(_ string, scope celengine.CacheScope) {
	d.invalidateCnt++
	d.lastInvalScope = scope
}
func (d *countingCachedDialect) Evaluate(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	d.evalCnt++
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

func assertUnsupportedReason(t *testing.T, err error, wantDialect, wantConsumer, wantReason, wantMsg string) {
	t.Helper()
	var unsup *ruleengine.UnsupportedDialectError
	if !errors.As(err, &unsup) {
		t.Fatalf("expected *UnsupportedDialectError, got %T: %v", err, err)
	}
	var ferr *ruleengine.FailedError
	if errors.As(err, &ferr) {
		t.Fatalf("denied admission must never wrap or return *FailedError, got: %v", err)
	}
	if unsup.Dialect != wantDialect || unsup.Consumer != wantConsumer || unsup.Reason != wantReason {
		t.Fatalf("UnsupportedDialectError = %+v, want Dialect=%q Consumer=%q Reason=%q", unsup, wantDialect, wantConsumer, wantReason)
	}
	if err.Error() != wantMsg {
		t.Fatalf("Error() = %q, want %q", err.Error(), wantMsg)
	}
}

func TestDenyByDefault_FiveAdmissionReasonsInOrder(t *testing.T) {
	celEng := newTestCELEngine(t, "trigger")
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	ioDialect := &countingCachedDialect{name: "io_model", performsIO: true}

	eng := ruleengine.NewEngine()
	if err := eng.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}
	if err := eng.Register(ioDialect); err != nil {
		t.Fatalf("Register io_model: %v", err)
	}

	ctx := context.Background()
	scope := celengine.CacheScope{Consumer: "placeholder", ID: "r1", Revision: 1}

	// Additive Reason: zero Reason preserves exact base Error() text.
	plainWithConsumer := (&ruleengine.UnsupportedDialectError{Dialect: "cel", Consumer: "trigger"}).Error()
	if plainWithConsumer != `unsupported dialect "cel" for consumer "trigger"` {
		t.Fatalf("plain Error() with consumer = %q", plainWithConsumer)
	}
	plainNoConsumer := (&ruleengine.UnsupportedDialectError{Dialect: "cel"}).Error()
	if plainNoConsumer != `unsupported dialect "cel"` {
		t.Fatalf("plain Error() without consumer = %q", plainNoConsumer)
	}

	// (1) empty_consumer wins first (even when dialect is also unregistered).
	for _, emptyCons := range []string{"", "   \t\n "} {
		if eng.Supports(emptyCons, "unregistered_dialect") {
			t.Fatalf("Supports(%q, ...) = true, want false", emptyCons)
		}
		_, err := eng.Compile(ctx, "unregistered_dialect", "x", ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: emptyCons,
			Name:     "trigger",
		})
		assertUnsupportedReason(t, err, "unregistered_dialect", "", ruleengine.ReasonEmptyConsumer,
			`unsupported dialect "unregistered_dialect": empty_consumer`)

		_, err = eng.CachedCompile(ctx, "", scope, "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: emptyCons,
			Name:     "trigger",
		})
		assertUnsupportedReason(t, err, "cel", "", ruleengine.ReasonEmptyConsumer,
			`unsupported dialect "cel": empty_consumer`)
	}

	// (2) consumer_not_registered (deny-by-default, even for registered "cel", and before unknown_dialect).
	if eng.Supports("unregistered_consumer", "cel") {
		t.Fatalf("Supports must be false for consumer with no allow-list")
	}
	_, err = eng.Compile(ctx, "cel", "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "unregistered_consumer",
		Name:     "trigger",
	})
	assertUnsupportedReason(t, err, "cel", "unregistered_consumer", ruleengine.ReasonConsumerNotRegistered,
		`unsupported dialect "cel" for consumer "unregistered_consumer": consumer_not_registered`)

	_, err = eng.CachedCompile(ctx, "missing_dialect", scope, "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: " unregistered_consumer ",
		Name:     "trigger",
	})
	assertUnsupportedReason(t, err, "missing_dialect", "unregistered_consumer", ruleengine.ReasonConsumerNotRegistered,
		`unsupported dialect "missing_dialect" for consumer "unregistered_consumer": consumer_not_registered`)

	// (3) dialect_not_allowed (consumer registered, dialect not in grants; wins before unknown_dialect).
	if err := eng.AllowConsumerDialects("cel_only", ruleengine.Grant("cel")); err != nil {
		t.Fatalf("AllowConsumerDialects cel_only: %v", err)
	}
	if eng.Supports("cel_only", "io_model") || eng.Supports("cel_only", "missing_dialect") {
		t.Fatalf("Supports must be false for ungranted dialects")
	}
	_, err = eng.Compile(ctx, "io_model", "check", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "cel_only",
		Name:     "trigger",
	})
	assertUnsupportedReason(t, err, "io_model", "cel_only", ruleengine.ReasonDialectNotAllowed,
		`unsupported dialect "io_model" for consumer "cel_only": dialect_not_allowed`)

	_, err = eng.CachedCompile(ctx, "missing_dialect", scope, "check", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "cel_only",
		Name:     "trigger",
	})
	assertUnsupportedReason(t, err, "missing_dialect", "cel_only", ruleengine.ReasonDialectNotAllowed,
		`unsupported dialect "missing_dialect" for consumer "cel_only": dialect_not_allowed`)

	// (4) unknown_dialect (grant registered ahead of dialect registration; admitted once registered).
	if err := eng.AllowConsumerDialects("forward_consumer", ruleengine.Grant("future_pure")); err != nil {
		t.Fatalf("AllowConsumerDialects forward_consumer: %v", err)
	}
	if eng.Supports("forward_consumer", "future_pure") {
		t.Fatalf("Supports must be false before future_pure dialect is registered")
	}
	_, err = eng.Compile(ctx, "future_pure", "rule", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "forward_consumer",
		Name:     "forward",
	})
	assertUnsupportedReason(t, err, "future_pure", "forward_consumer", ruleengine.ReasonUnknownDialect,
		`unsupported dialect "future_pure" for consumer "forward_consumer": unknown_dialect`)

	futureDialect := &countingCachedDialect{name: "future_pure", performsIO: false}
	if err := eng.Register(futureDialect); err != nil {
		t.Fatalf("Register future_pure: %v", err)
	}
	if !eng.Supports("forward_consumer", "future_pure") {
		t.Fatalf("Supports must become true once pre-granted pure dialect is registered")
	}
	compFuture, err := eng.Compile(ctx, "future_pure", "rule", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "forward_consumer",
		Name:     "forward",
	})
	if err != nil {
		t.Fatalf("Compile future_pure after Register: %v", err)
	}
	if compFuture.Consumer() != "forward_consumer" {
		t.Fatalf("Compiled.Consumer() = %q, want %q", compFuture.Consumer(), "forward_consumer")
	}

	// (5) io_not_allowed (PerformsIO dialect granted via Grant without AllowIO -> denied; GrantIO -> admitted).
	if err := eng.AllowConsumerDialects("no_io_opt_in", ruleengine.Grant("io_model")); err != nil {
		t.Fatalf("AllowConsumerDialects no_io_opt_in: %v", err)
	}
	if eng.Supports("no_io_opt_in", "io_model") {
		t.Fatalf("Supports must be false when PerformsIO dialect lacks AllowIO")
	}
	_, err = eng.Compile(ctx, "io_model", "rule", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "no_io_opt_in",
		Name:     "io",
	})
	assertUnsupportedReason(t, err, "io_model", "no_io_opt_in", ruleengine.ReasonIONotAllowed,
		`unsupported dialect "io_model" for consumer "no_io_opt_in": io_not_allowed`)
	_, err = eng.CachedCompile(ctx, "io_model", scope, "rule", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "no_io_opt_in",
		Name:     "io",
	})
	assertUnsupportedReason(t, err, "io_model", "no_io_opt_in", ruleengine.ReasonIONotAllowed,
		`unsupported dialect "io_model" for consumer "no_io_opt_in": io_not_allowed`)
	if ioDialect.compileCnt != 0 || ioDialect.cachedCompCnt != 0 || ioDialect.evalCnt != 0 {
		t.Fatalf("denied PerformsIO dialect must never be invoked: %+v", ioDialect)
	}

	// Admitted once GrantIO is granted.
	if err := eng.AllowConsumerDialects("with_io_opt_in", ruleengine.GrantIO("io_model")); err != nil {
		t.Fatalf("AllowConsumerDialects with_io_opt_in: %v", err)
	}
	if !eng.Supports("with_io_opt_in", "io_model") {
		t.Fatalf("Supports must be true when PerformsIO dialect has GrantIO")
	}
	compIO, err := eng.Compile(ctx, "io_model", "rule", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "with_io_opt_in",
		Name:     "io",
	})
	if err != nil {
		t.Fatalf("Compile with GrantIO: %v", err)
	}
	decIO, err := eng.Evaluate(ctx, compIO, ruleengine.Input{}, ruleengine.Budget{})
	if err != nil || !decIO.Bool {
		t.Fatalf("Evaluate with GrantIO = (%+v, %v), want Bool=true, nil", decIO, err)
	}
}

func TestDenyByDefault_EmptyConsumerRejectedAtRegistrationAndBlankGrantIgnored(t *testing.T) {
	celEng := newTestCELEngine(t, "trigger")
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	eng := ruleengine.NewEngine()
	if err := eng.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}

	for _, badConsumer := range []string{"", " ", "\t\n"} {
		err := eng.AllowConsumerDialects(badConsumer, ruleengine.Grant("cel"))
		if !errors.Is(err, ruleengine.ErrEmptyConsumer) {
			t.Fatalf("AllowConsumerDialects(%q) = %v, want ErrEmptyConsumer", badConsumer, err)
		}
	}

	// Blank dialect name inside DialectGrant must not implicitly grant "cel".
	if err := eng.AllowConsumerDialects("blank_grant_consumer", ruleengine.Grant(""), ruleengine.Grant("   ")); err != nil {
		t.Fatalf("AllowConsumerDialects blank_grant_consumer: %v", err)
	}
	if eng.Supports("blank_grant_consumer", "cel") {
		t.Fatalf("Grant(\"\") must not grant default CEL dialect")
	}
	_, err = eng.Compile(context.Background(), "", "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "blank_grant_consumer",
		Name:     "trigger",
	})
	assertUnsupportedReason(t, err, "cel", "blank_grant_consumer", ruleengine.ReasonDialectNotAllowed,
		`unsupported dialect "cel" for consumer "blank_grant_consumer": dialect_not_allowed`)
}

func TestDenyByDefault_ReregistrationReplacesAndIntrospectionUnchanged(t *testing.T) {
	celEng := newTestCELEngine(t, "trigger")
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	ioDialect := &countingCachedDialect{name: "system_one", performsIO: true}

	eng := ruleengine.NewEngine()
	if err := eng.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}
	if err := eng.Register(ioDialect); err != nil {
		t.Fatalf("Register system_one: %v", err)
	}

	// Names-only introspection works identically before and after consumer registrations,
	// whereas DialectFor enforces deny-by-default admission before any grants.
	if got := eng.Dialects(); !slices.Equal(got, []string{"cel", "system_one"}) {
		t.Fatalf("Dialects() before grants = %v, want [cel system_one]", got)
	}
	if d, err := eng.DialectFor("trigger", "cel"); d != nil || err == nil {
		t.Fatalf("DialectFor before grants = (%v, %v), want nil, consumer_not_registered", d, err)
	}

	// Initial grant allows both "cel" and "system_one".
	if err := eng.AllowConsumerDialects("trigger", ruleengine.Grant("cel"), ruleengine.GrantIO("system_one")); err != nil {
		t.Fatalf("AllowConsumerDialects initial: %v", err)
	}
	// Idempotent repeat.
	if err := eng.AllowConsumerDialects(" trigger ", ruleengine.Grant(" cel "), ruleengine.GrantIO(" system_one ")); err != nil {
		t.Fatalf("AllowConsumerDialects idempotent: %v", err)
	}
	if !eng.Supports("trigger", "cel") || !eng.Supports("trigger", "system_one") {
		t.Fatalf("expected both cel and system_one supported after initial grant")
	}
	if d, err := eng.DialectFor("trigger", "cel"); err != nil || d != celDialect {
		t.Fatalf("DialectFor(trigger, cel) = (%v, %v), want celDialect, nil", d, err)
	}
	if d, err := eng.DialectFor(" trigger ", ""); err != nil || d != celDialect {
		t.Fatalf("DialectFor(trigger, \"\") = (%v, %v), want celDialect, nil", d, err)
	}
	if d, err := eng.DialectFor("trigger", "system_one"); err != nil || d != ioDialect {
		t.Fatalf("DialectFor(trigger, system_one) = (%v, %v), want ioDialect, nil", d, err)
	}

	// Re-registration REPLACES (does not union): drop system_one.
	if err := eng.AllowConsumerDialects("trigger", ruleengine.Grant("cel")); err != nil {
		t.Fatalf("AllowConsumerDialects replace: %v", err)
	}
	if !eng.Supports("trigger", "cel") {
		t.Fatalf("expected cel still supported after replacement")
	}
	if eng.Supports("trigger", "system_one") {
		t.Fatalf("expected system_one revoked after replacement")
	}
	if d, err := eng.DialectFor("trigger", "system_one"); d != nil {
		t.Fatalf("DialectFor after revoke returned non-nil handle: %v", d)
	} else {
		assertUnsupportedReason(t, err, "system_one", "trigger", ruleengine.ReasonDialectNotAllowed,
			`unsupported dialect "system_one" for consumer "trigger": dialect_not_allowed`)
	}
	_, err = eng.Compile(context.Background(), "system_one", "r", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "trigger",
		Name:     "trigger",
	})
	assertUnsupportedReason(t, err, "system_one", "trigger", ruleengine.ReasonDialectNotAllowed,
		`unsupported dialect "system_one" for consumer "trigger": dialect_not_allowed`)

	// Names-only introspection remains unchanged.
	if got := eng.Dialects(); !slices.Equal(got, []string{"cel", "system_one"}) {
		t.Fatalf("Dialects() after replacement = %v, want [cel system_one]", got)
	}
}

func TestDenyByDefault_CachedCompileCarriesConsumerAndInvalidateEnforced(t *testing.T) {
	celEng := newTestCELEngine(t, "shared_env")
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	cachedIO := &countingCachedDialect{name: "cached_io", performsIO: true}

	eng := ruleengine.NewEngine()
	if err := eng.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}
	if err := eng.Register(cachedIO); err != nil {
		t.Fatalf("Register cached_io: %v", err)
	}
	if err := eng.AllowConsumerDialects("consumer_a", ruleengine.Grant("cel")); err != nil {
		t.Fatalf("AllowConsumerDialects consumer_a: %v", err)
	}
	if err := eng.AllowConsumerDialects("consumer_b", ruleengine.Grant("cel"), ruleengine.Grant("cached_io")); err != nil {
		t.Fatalf("AllowConsumerDialects consumer_b: %v", err)
	}

	ctx := context.Background()
	const sharedExpr = `event.type == "push"`
	compA, err := eng.CachedCompile(ctx, "cel",
		celengine.CacheScope{Consumer: "consumer_a", ID: "id-a", Revision: 1},
		sharedExpr, ruleengine.KindFilter,
		ruleengine.EnvironmentRef{Consumer: "consumer_a", Name: "shared_env"},
	)
	if err != nil {
		t.Fatalf("CachedCompile consumer_a: %v", err)
	}
	compB, err := eng.CachedCompile(ctx, "cel",
		celengine.CacheScope{Consumer: "consumer_b", ID: "id-b", Revision: 1},
		sharedExpr, ruleengine.KindFilter,
		ruleengine.EnvironmentRef{Consumer: "consumer_b", Name: "shared_env"},
	)
	if err != nil {
		t.Fatalf("CachedCompile consumer_b: %v", err)
	}
	if celEng.CacheStats().Hits != 1 {
		t.Fatalf("expected shared CEL program cache hit, got %+v", celEng.CacheStats())
	}
	if compA.Consumer() != "consumer_a" || compB.Consumer() != "consumer_b" {
		t.Fatalf("cached Compiled handles leaked consumer identity: A=%q B=%q", compA.Consumer(), compB.Consumer())
	}

	// Invalidate denied for empty consumer, unregistered consumer, ungranted dialect, and io_not_allowed.
	eng.Invalidate("cached_io", "shared_env", celengine.CacheScope{Consumer: "", ID: "x", Revision: 1})
	eng.Invalidate("cached_io", "shared_env", celengine.CacheScope{Consumer: "unregistered", ID: "x", Revision: 1})
	eng.Invalidate("cached_io", "shared_env", celengine.CacheScope{Consumer: "consumer_a", ID: "x", Revision: 1})
	eng.Invalidate("cached_io", "shared_env", celengine.CacheScope{Consumer: "consumer_b", ID: "x", Revision: 1})
	if cachedIO.invalidateCnt != 0 {
		t.Fatalf("Invalidate forwarded despite denied admission: invalidateCnt=%d", cachedIO.invalidateCnt)
	}

	// GrantIO to consumer_b -> Invalidate now forwards.
	if err := eng.AllowConsumerDialects("consumer_b", ruleengine.Grant("cel"), ruleengine.GrantIO("cached_io")); err != nil {
		t.Fatalf("AllowConsumerDialects GrantIO: %v", err)
	}
	eng.Invalidate("cached_io", "shared_env", celengine.CacheScope{Consumer: "consumer_b", ID: "x", Revision: 1})
	if cachedIO.invalidateCnt != 1 {
		t.Fatalf("Invalidate after GrantIO: invalidateCnt=%d, want 1", cachedIO.invalidateCnt)
	}
}

func TestDenyByDefault_EvaluateEnforcesAdmissionBeforeCtxAndBudget(t *testing.T) {
	ctx := context.Background()

	// 1. Zero Compiled{} still returns FailedError{Reason: "uninitialized_program"}.
	emptyEng := ruleengine.NewEngine()
	_, err := emptyEng.Evaluate(ctx, ruleengine.Compiled{}, ruleengine.Input{}, ruleengine.Budget{})
	var ferr *ruleengine.FailedError
	if !errors.As(err, &ferr) || ferr.Reason != "uninitialized_program" {
		t.Fatalf("zero Compiled{} Evaluate = %T (%v), want *FailedError{uninitialized_program}", err, err)
	}

	// 2. Compiled from consumer A evaluated on an engine that only admits consumer B is denied.
	celEngA := newTestCELEngine(t, "env_shared")
	celDialectA, err := ruleengine.NewCELDialect(celEngA)
	if err != nil {
		t.Fatalf("NewCELDialect A: %v", err)
	}
	engA := ruleengine.NewEngine()
	if err := engA.Register(celDialectA); err != nil {
		t.Fatalf("Register A: %v", err)
	}
	if err := engA.AllowConsumerDialects("consumer_a", ruleengine.Grant("cel")); err != nil {
		t.Fatalf("AllowConsumerDialects A: %v", err)
	}
	compFromA, err := engA.Compile(ctx, "cel", "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "consumer_a",
		Name:     "env_shared",
	})
	if err != nil {
		t.Fatalf("Compile on engA: %v", err)
	}
	if compFromA.Consumer() != "consumer_a" {
		t.Fatalf("compFromA.Consumer() = %q, want consumer_a", compFromA.Consumer())
	}

	celEngB := newTestCELEngine(t, "env_shared")
	celDialectB, err := ruleengine.NewCELDialect(celEngB)
	if err != nil {
		t.Fatalf("NewCELDialect B: %v", err)
	}
	engB := ruleengine.NewEngine()
	if err := engB.Register(celDialectB); err != nil {
		t.Fatalf("Register B: %v", err)
	}
	if err := engB.AllowConsumerDialects("consumer_b", ruleengine.Grant("cel")); err != nil {
		t.Fatalf("AllowConsumerDialects B: %v", err)
	}
	_, err = engB.Evaluate(ctx, compFromA, ruleengine.Input{}, ruleengine.Budget{})
	assertUnsupportedReason(t, err, "cel", "consumer_a", ruleengine.ReasonConsumerNotRegistered,
		`unsupported dialect "cel" for consumer "consumer_a": consumer_not_registered`)

	// 3. Revoking consumer_a on engA causes Evaluate(compFromA) to fail with *UnsupportedDialectError
	// (never *FailedError), even when caller ctx is pre-canceled and BudgetTracker is exhausted.
	if err := engA.AllowConsumerDialects("consumer_a"); err != nil {
		t.Fatalf("revoke consumer_a: %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	exhausted := ruleengine.NewBudgetTracker(time.Nanosecond)
	exhausted.Charge(time.Second)

	_, err = engA.Evaluate(canceledCtx, compFromA, ruleengine.Input{}, ruleengine.Budget{Tracker: exhausted})
	assertUnsupportedReason(t, err, "cel", "consumer_a", ruleengine.ReasonDialectNotAllowed,
		`unsupported dialect "cel" for consumer "consumer_a": dialect_not_allowed`)

	// 4. Mirror: Compiled while PerformsIO was false (with pure Grant), then dialect's PerformsIO
	// is true at Evaluate without AllowIO -> io_not_allowed and zero dialect Evaluate calls.
	dynStub := &stubModelDialect{mode: "dynamic_no_io", performsIO: false}
	engDyn := ruleengine.NewEngine()
	if err := engDyn.Register(dynStub); err != nil {
		t.Fatalf("Register dynStub: %v", err)
	}
	if err := engDyn.AllowConsumerDialects("consumer_a", ruleengine.Grant("stub_model")); err != nil {
		t.Fatalf("AllowConsumerDialects dyn: %v", err)
	}
	compDyn, err := engDyn.Compile(ctx, "stub_model", "rule", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "consumer_a",
		Name:     "dyn",
	})
	if err != nil {
		t.Fatalf("Compile dyn: %v", err)
	}
	dynStub.performsIO = true
	dynStub.mode = "ok"
	_, err = engDyn.Evaluate(ctx, compDyn, ruleengine.Input{}, ruleengine.Budget{})
	assertUnsupportedReason(t, err, "stub_model", "consumer_a", ruleengine.ReasonIONotAllowed,
		`unsupported dialect "stub_model" for consumer "consumer_a": io_not_allowed`)
	if dynStub.evalCnt != 0 {
		t.Fatalf("d.Evaluate invoked %d times despite io_not_allowed", dynStub.evalCnt)
	}

	// 5. Hand-built Compiled with empty consumer (NewCompiled("", ...)) is denied with empty_consumer.
	forgedEmptyConsumer := ruleengine.NewCompiled("", "cel", ruleengine.KindFilter, compFromA.Rep())
	_, err = engB.Evaluate(ctx, forgedEmptyConsumer, ruleengine.Input{}, ruleengine.Budget{})
	assertUnsupportedReason(t, err, "cel", "", ruleengine.ReasonEmptyConsumer,
		`unsupported dialect "cel": empty_consumer`)
}

func TestDenyByDefault_CachedCompileScopeConsumerBoundary(t *testing.T) {
	celEng := newTestCELEngine(t, "shared_env")
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	cachedPure := &countingCachedDialect{name: "cached_pure", performsIO: false}

	eng := ruleengine.NewEngine()
	if err := eng.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}
	if err := eng.Register(cachedPure); err != nil {
		t.Fatalf("Register cached_pure: %v", err)
	}
	if err := eng.AllowConsumerDialects("consumer_a", ruleengine.Grant("cel"), ruleengine.Grant("cached_pure")); err != nil {
		t.Fatalf("AllowConsumerDialects consumer_a: %v", err)
	}
	if err := eng.AllowConsumerDialects("consumer_b", ruleengine.Grant("cel")); err != nil {
		t.Fatalf("AllowConsumerDialects consumer_b: %v", err)
	}

	ctx := context.Background()

	// 1. Mismatched non-empty scope.Consumer (both registered and unregistered) is rejected
	// after admission with UnsupportedDialectError{Dialect, Consumer: env.Consumer, Reason: dialect_not_allowed}
	// without touching the cache or invoking CachedDialect.
	for _, otherScopeConsumer := range []string{"consumer_b", "unregistered_other"} {
		_, err := eng.CachedCompile(ctx, "",
			celengine.CacheScope{Consumer: otherScopeConsumer, ID: "id-1", Revision: 1},
			`event.type == "push"`, ruleengine.KindFilter,
			ruleengine.EnvironmentRef{Consumer: " consumer_a ", Name: "shared_env"},
		)
		assertUnsupportedReason(t, err, "cel", "consumer_a", ruleengine.ReasonDialectNotAllowed,
			`unsupported dialect "cel" for consumer "consumer_a": dialect_not_allowed`)

		_, err = eng.CachedCompile(ctx, "cached_pure",
			celengine.CacheScope{Consumer: otherScopeConsumer, ID: "id-1", Revision: 1},
			"rule", ruleengine.KindFilter,
			ruleengine.EnvironmentRef{Consumer: "consumer_a", Name: "shared_env"},
		)
		assertUnsupportedReason(t, err, "cached_pure", "consumer_a", ruleengine.ReasonDialectNotAllowed,
			`unsupported dialect "cached_pure" for consumer "consumer_a": dialect_not_allowed`)
	}
	if celEng.CacheStats().Entries != 0 || cachedPure.cachedCompCnt != 0 {
		t.Fatalf("mismatched scope.Consumer touched cache: cel=%+v cachedPure=%+v", celEng.CacheStats(), cachedPure)
	}

	// 2. Empty (or whitespace) scope.Consumer defaults to normalized env.Consumer.
	compCEL, err := eng.CachedCompile(ctx, "cel",
		celengine.CacheScope{Consumer: "", ID: "id-default", Revision: 1},
		`event.type == "push"`, ruleengine.KindFilter,
		ruleengine.EnvironmentRef{Consumer: " consumer_a ", Name: "shared_env"},
	)
	if err != nil {
		t.Fatalf("CachedCompile empty scope.Consumer on CEL: %v", err)
	}
	if compCEL.Consumer() != "consumer_a" {
		t.Fatalf("compCEL.Consumer() = %q, want consumer_a", compCEL.Consumer())
	}
	if celEng.CacheStats().Entries != 1 {
		t.Fatalf("Entries = %d, want 1", celEng.CacheStats().Entries)
	}
	// Proves celEng bound the entry under consumer_a: invalidating consumer_a evicts it.
	eng.Invalidate("cel", "shared_env", celengine.CacheScope{Consumer: " consumer_a ", ID: "id-default", Revision: 1})
	if celEng.CacheStats().Entries != 0 {
		t.Fatalf("Entries after Invalidate(consumer_a) = %d, want 0", celEng.CacheStats().Entries)
	}

	compCustom, err := eng.CachedCompile(ctx, "cached_pure",
		celengine.CacheScope{Consumer: "   ", ID: "id-ws", Revision: 2},
		"rule", ruleengine.KindFilter,
		ruleengine.EnvironmentRef{Consumer: "consumer_a", Name: "shared_env"},
	)
	if err != nil {
		t.Fatalf("CachedCompile whitespace scope.Consumer on cached_pure: %v", err)
	}
	if compCustom.Consumer() != "consumer_a" || cachedPure.lastCachedScope.Consumer != "consumer_a" {
		t.Fatalf("got compiled.Consumer=%q scope.Consumer=%q, want consumer_a", compCustom.Consumer(), cachedPure.lastCachedScope.Consumer)
	}
}

type concurrentIODialect struct {
	compiles atomic.Int64
	evals    atomic.Int64
}

func (d *concurrentIODialect) Name() string { return "concurrent_io" }
func (d *concurrentIODialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: time.Second,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *concurrentIODialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	d.compiles.Add(1)
	return ruleengine.NewCompiled(env.Consumer, d.Name(), kind, rule), nil
}
func (d *concurrentIODialect) Evaluate(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	d.evals.Add(1)
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

func TestDenyByDefault_ConcurrentAllowListReplacementRace(t *testing.T) {
	// Use the maximum celengine EvalTimeout (1s) so heavy -race / CPU load
	// cannot trigger spurious 100ms wall-clock eval timeouts during concurrent
	// allow-list contention; this test verifies admission-race safety, not eval timeouts.
	limits := celengine.DefaultLimits()
	limits.EvalTimeout = time.Second
	celEng, err := celengine.New(celengine.Environment{
		Name:    "race_env",
		Version: 1,
		Variables: []celengine.Variable{
			{Name: "event", Type: celengine.Map(celengine.String(), celengine.Dyn())},
			{Name: "items", Type: celengine.List(celengine.String())},
		},
		Extensions: []celengine.Extension{celengine.ExtensionStrings},
		Macros:     celengine.MacrosStandard,
	}, limits, celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	ioDialect := &concurrentIODialect{}

	eng := ruleengine.NewEngine()
	if err := eng.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}
	if err := eng.Register(ioDialect); err != nil {
		t.Fatalf("Register concurrent_io: %v", err)
	}

	if err := eng.AllowConsumerDialects("stable", ruleengine.Grant("cel")); err != nil {
		t.Fatalf("AllowConsumerDialects stable: %v", err)
	}
	if err := eng.AllowConsumerDialects("flapping", ruleengine.Grant("cel"), ruleengine.GrantIO("concurrent_io")); err != nil {
		t.Fatalf("AllowConsumerDialects flapping: %v", err)
	}

	ctx := context.Background()
	const expr = `event.type == "push"`
	actInput := ruleengine.Input{
		Activation: map[string]any{"event": map[string]any{"type": "push"}},
	}

	compStable, err := eng.Compile(ctx, "cel", expr, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "stable",
		Name:     "race_env",
	})
	if err != nil {
		t.Fatalf("Compile stable: %v", err)
	}
	compFlapCEL, err := eng.Compile(ctx, "cel", expr, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "flapping",
		Name:     "race_env",
	})
	if err != nil {
		t.Fatalf("Compile flapping CEL: %v", err)
	}
	compFlapIO, err := eng.Compile(ctx, "concurrent_io", "check", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "flapping",
		Name:     "race_env",
	})
	if err != nil {
		t.Fatalf("Compile flapping IO: %v", err)
	}

	checkFlappingErr := func(err error, wantDialect string) {
		if err == nil {
			return
		}
		var unsup *ruleengine.UnsupportedDialectError
		if !errors.As(err, &unsup) {
			t.Errorf("unexpected non-UnsupportedDialectError for %s: %T (%v)", wantDialect, err, err)
			return
		}
		var ferr *ruleengine.FailedError
		if errors.As(err, &ferr) {
			t.Errorf("admission failure wrapped in FailedError: %v", err)
			return
		}
		if unsup.Consumer != "flapping" || unsup.Dialect != wantDialect {
			t.Errorf("UnsupportedDialectError = %+v, want Consumer=flapping Dialect=%s", unsup, wantDialect)
		}
		switch unsup.Reason {
		case ruleengine.ReasonDialectNotAllowed:
		case ruleengine.ReasonIONotAllowed:
			if wantDialect != "concurrent_io" {
				t.Errorf("unexpected io_not_allowed for pure dialect %s", wantDialect)
			}
		default:
			t.Errorf("unexpected UnsupportedDialectError.Reason = %q for %s", unsup.Reason, wantDialect)
		}
	}

	const iterations = 250
	start := make(chan struct{})
	var wg sync.WaitGroup

	// 2 writer goroutines toggling flapping's allow-list between granted, pure-only, and empty.
	for w := range 2 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := range iterations {
				var err error
				switch (i + worker) % 3 {
				case 0:
					err = eng.AllowConsumerDialects("flapping", ruleengine.Grant("cel"), ruleengine.GrantIO("concurrent_io"))
				case 1:
					err = eng.AllowConsumerDialects("flapping")
				case 2:
					err = eng.AllowConsumerDialects("flapping", ruleengine.Grant("concurrent_io"))
				}
				if err != nil {
					t.Errorf("AllowConsumerDialects: %v", err)
					return
				}
			}
		}(w)
	}

	// Concurrent Supports / DialectFor readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			_ = eng.Supports("flapping", "cel")
			_ = eng.Supports("flapping", "concurrent_io")
			if d, err := eng.DialectFor("flapping", "cel"); err != nil {
				if d != nil {
					t.Errorf("non-nil Dialect handle on denied CEL DialectFor: %v", d)
				}
				checkFlappingErr(err, "cel")
			} else if d != celDialect {
				t.Errorf("DialectFor(flapping, cel) = %v, want celDialect", d)
			}
			if d, err := eng.DialectFor("flapping", "concurrent_io"); err != nil {
				if d != nil {
					t.Errorf("non-nil Dialect handle on denied IO DialectFor: %v", d)
				}
				checkFlappingErr(err, "concurrent_io")
			} else if d != ioDialect {
				t.Errorf("DialectFor(flapping, concurrent_io) = %v, want ioDialect", d)
			}
			if !eng.Supports("stable", "cel") {
				t.Errorf("stable consumer unexpectedly lost cel grant")
				return
			}
			if d, err := eng.DialectFor("stable", "cel"); err != nil || d != celDialect {
				t.Errorf("stable DialectFor = (%v, %v), want celDialect, nil", d, err)
				return
			}
		}
	}()

	// Concurrent Compile / CachedCompile / Invalidate readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range iterations {
			c, err := eng.Compile(ctx, "cel", expr, ruleengine.KindFilter, ruleengine.EnvironmentRef{
				Consumer: "flapping",
				Name:     "race_env",
			})
			if err != nil {
				checkFlappingErr(err, "cel")
			} else if c.Consumer() != "flapping" {
				t.Errorf("Compiled.Consumer() = %q, want flapping", c.Consumer())
			}

			cc, err := eng.CachedCompile(ctx, "cel",
				celengine.CacheScope{ID: "race-slot", Revision: int64(i + 1)},
				expr, ruleengine.KindFilter,
				ruleengine.EnvironmentRef{Consumer: "flapping", Name: "race_env"},
			)
			if err != nil {
				checkFlappingErr(err, "cel")
			} else if cc.Consumer() != "flapping" {
				t.Errorf("CachedCompile Compiled.Consumer() = %q, want flapping", cc.Consumer())
			}

			ci, err := eng.Compile(ctx, "concurrent_io", "check", ruleengine.KindFilter, ruleengine.EnvironmentRef{
				Consumer: "flapping",
				Name:     "race_env",
			})
			if err != nil {
				checkFlappingErr(err, "concurrent_io")
			} else if ci.Consumer() != "flapping" {
				t.Errorf("IO Compiled.Consumer() = %q, want flapping", ci.Consumer())
			}

			eng.Invalidate("cel", "race_env", celengine.CacheScope{Consumer: "flapping", ID: "race-slot", Revision: int64(i + 1)})
		}
	}()

	// Concurrent Evaluate readers (2 goroutines).
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				decCEL, err := eng.Evaluate(ctx, compFlapCEL, actInput, ruleengine.Budget{})
				if err != nil {
					if decCEL != (ruleengine.Decision{}) {
						t.Errorf("non-zero Decision on denied CEL Evaluate: %+v", decCEL)
					}
					checkFlappingErr(err, "cel")
				} else if !decCEL.Bool {
					t.Errorf("admitted CEL Evaluate returned Bool=false")
				}

				decIO, err := eng.Evaluate(ctx, compFlapIO, actInput, ruleengine.Budget{})
				if err != nil {
					if decIO != (ruleengine.Decision{}) {
						t.Errorf("non-zero Decision on denied IO Evaluate: %+v", decIO)
					}
					checkFlappingErr(err, "concurrent_io")
				} else if !decIO.Bool {
					t.Errorf("admitted IO Evaluate returned Bool=false")
				}

				decStable, err := eng.Evaluate(ctx, compStable, actInput, ruleengine.Budget{})
				if err != nil || !decStable.Bool {
					t.Errorf("stable Evaluate = (%+v, %v), want Bool=true, nil", decStable, err)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}

func TestDenyByDefault_DialectForEnforcesAllFiveReasons(t *testing.T) {
	celEng := newTestCELEngine(t, "trigger")
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	ioDialect := &countingCachedDialect{name: "io_model", performsIO: true}

	eng := ruleengine.NewEngine()
	if err := eng.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}
	if err := eng.Register(ioDialect); err != nil {
		t.Fatalf("Register io_model: %v", err)
	}

	// (1) empty_consumer (empty or whitespace; empty name normalizes to "cel").
	for _, emptyCons := range []string{"", " \t\n "} {
		d, err := eng.DialectFor(emptyCons, "")
		if d != nil {
			t.Fatalf("DialectFor(%q, \"\") returned non-nil handle %v", emptyCons, d)
		}
		assertUnsupportedReason(t, err, "cel", "", ruleengine.ReasonEmptyConsumer,
			`unsupported dialect "cel": empty_consumer`)
	}

	// (2) consumer_not_registered.
	if d, err := eng.DialectFor("unregistered", "io_model"); d != nil {
		t.Fatalf("DialectFor unregistered returned handle %v", d)
	} else {
		assertUnsupportedReason(t, err, "io_model", "unregistered", ruleengine.ReasonConsumerNotRegistered,
			`unsupported dialect "io_model" for consumer "unregistered": consumer_not_registered`)
	}

	// (3) dialect_not_allowed (wins ahead of unknown_dialect).
	if err := eng.AllowConsumerDialects("cel_only", ruleengine.Grant("cel")); err != nil {
		t.Fatalf("AllowConsumerDialects cel_only: %v", err)
	}
	if d, err := eng.DialectFor("cel_only", "io_model"); d != nil {
		t.Fatalf("DialectFor ungranted io_model returned handle %v", d)
	} else {
		assertUnsupportedReason(t, err, "io_model", "cel_only", ruleengine.ReasonDialectNotAllowed,
			`unsupported dialect "io_model" for consumer "cel_only": dialect_not_allowed`)
	}
	if d, err := eng.DialectFor("cel_only", "missing_dialect"); d != nil {
		t.Fatalf("DialectFor ungranted missing_dialect returned handle %v", d)
	} else {
		assertUnsupportedReason(t, err, "missing_dialect", "cel_only", ruleengine.ReasonDialectNotAllowed,
			`unsupported dialect "missing_dialect" for consumer "cel_only": dialect_not_allowed`)
	}

	// (4) unknown_dialect (granted, not yet registered).
	if err := eng.AllowConsumerDialects("forward_consumer", ruleengine.Grant("future_pure")); err != nil {
		t.Fatalf("AllowConsumerDialects forward_consumer: %v", err)
	}
	if d, err := eng.DialectFor("forward_consumer", "future_pure"); d != nil {
		t.Fatalf("DialectFor unregistered future_pure returned handle %v", d)
	} else {
		assertUnsupportedReason(t, err, "future_pure", "forward_consumer", ruleengine.ReasonUnknownDialect,
			`unsupported dialect "future_pure" for consumer "forward_consumer": unknown_dialect`)
	}

	// (5) io_not_allowed (pure Grant on PerformsIO dialect -> denied; GrantIO -> returns handle).
	if err := eng.AllowConsumerDialects("pure_grant_consumer", ruleengine.Grant("io_model")); err != nil {
		t.Fatalf("AllowConsumerDialects pure_grant_consumer: %v", err)
	}
	if d, err := eng.DialectFor("pure_grant_consumer", "io_model"); d != nil {
		t.Fatalf("DialectFor io_model without AllowIO returned handle %v", d)
	} else {
		assertUnsupportedReason(t, err, "io_model", "pure_grant_consumer", ruleengine.ReasonIONotAllowed,
			`unsupported dialect "io_model" for consumer "pure_grant_consumer": io_not_allowed`)
	}

	if err := eng.AllowConsumerDialects("io_grant_consumer", ruleengine.Grant("cel"), ruleengine.GrantIO("io_model")); err != nil {
		t.Fatalf("AllowConsumerDialects io_grant_consumer: %v", err)
	}
	gotCEL, err := eng.DialectFor(" io_grant_consumer ", "")
	if err != nil || gotCEL != celDialect {
		t.Fatalf("DialectFor admitted CEL = (%v, %v), want celDialect, nil", gotCEL, err)
	}
	gotIO, err := eng.DialectFor("io_grant_consumer", "io_model")
	if err != nil || gotIO != ioDialect {
		t.Fatalf("DialectFor admitted io_model = (%v, %v), want ioDialect, nil", gotIO, err)
	}
}

type budgetProbeDialect struct {
	name                 string
	maxEvalTime          time.Duration
	waitCtxDone          bool
	returnBeforeDeadline time.Duration
	evalCalls            atomic.Int32
}

func (d *budgetProbeDialect) Name() string { return d.name }
func (d *budgetProbeDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		MaxEvalTime: d.maxEvalTime,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *budgetProbeDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, rule), nil
}
func (d *budgetProbeDialect) Evaluate(ctx context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	d.evalCalls.Add(1)
	if d.returnBeforeDeadline > 0 {
		if deadline, ok := ctx.Deadline(); ok {
			if wait := time.Until(deadline) - d.returnBeforeDeadline; wait > 0 {
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
				}
			}
		}
		return ruleengine.Decision{}, &ruleengine.FailedError{
			Reason: celengine.ReasonTimeout,
			Cause:  context.DeadlineExceeded,
		}
	}
	if d.waitCtxDone {
		<-ctx.Done()
		return ruleengine.Decision{}, &ruleengine.FailedError{
			Reason: celengine.ReasonTimeout,
			Cause:  ctx.Err(),
		}
	}
	// Immediate per-rule timeout well before the shared tracker deadline.
	return ruleengine.Decision{}, &ruleengine.FailedError{
		Reason: celengine.ReasonTimeout,
		Cause:  context.DeadlineExceeded,
	}
}

func TestBudgetTracker_Exhaust(t *testing.T) {
	var nilTracker *ruleengine.BudgetTracker
	nilTracker.Exhaust() // must not panic

	tr := ruleengine.NewBudgetTracker(100 * time.Millisecond)
	tr.Charge(10 * time.Millisecond)
	if tr.Exhausted() || tr.Remaining() != 90*time.Millisecond {
		t.Fatalf("before Exhaust: Exhausted=%v Remaining=%s", tr.Exhausted(), tr.Remaining())
	}
	tr.Exhaust()
	if !tr.Exhausted() || tr.Remaining() != 0 || tr.Spent() != 100*time.Millisecond {
		t.Fatalf("after Exhaust: Exhausted=%v Remaining=%s Spent=%s", tr.Exhausted(), tr.Remaining(), tr.Spent())
	}

	// Already-overspent tracker must not have Spent() reduced by Exhaust().
	over := ruleengine.NewBudgetTracker(50 * time.Millisecond)
	over.Charge(80 * time.Millisecond)
	over.Exhaust()
	if !over.Exhausted() || over.Remaining() != 0 || over.Spent() != 80*time.Millisecond {
		t.Fatalf("overspent after Exhaust: Exhausted=%v Remaining=%s Spent=%s", over.Exhausted(), over.Remaining(), over.Spent())
	}

	// Concurrent Charge/Exhaust/Remaining/Exhausted must be race-free.
	conc := ruleengine.NewBudgetTracker(50 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				conc.Charge(time.Millisecond)
			} else {
				conc.Exhaust()
			}
			_ = conc.Exhausted()
			_ = conc.Remaining()
			_ = conc.Spent()
		}(i)
	}
	wg.Wait()
	if !conc.Exhausted() || conc.Remaining() != 0 {
		t.Fatalf("concurrent Exhaust: Exhausted=%v Remaining=%s", conc.Exhausted(), conc.Remaining())
	}
}

func TestEvaluate_BoundFromTrackerTimeoutExhaustsTracker(t *testing.T) {
	eng := ruleengine.NewEngine()
	waitDialect := &budgetProbeDialect{name: "wait_deadline", maxEvalTime: 100 * time.Millisecond, waitCtxDone: true}
	fastTimeoutDialect := &budgetProbeDialect{name: "fast_timeout", maxEvalTime: 10 * time.Second, waitCtxDone: false}
	if err := eng.Register(waitDialect); err != nil {
		t.Fatalf("Register wait_deadline: %v", err)
	}
	if err := eng.Register(fastTimeoutDialect); err != nil {
		t.Fatalf("Register fast_timeout: %v", err)
	}
	if err := eng.AllowConsumerDialects(
		"triggers",
		ruleengine.Grant("wait_deadline"),
		ruleengine.Grant("fast_timeout"),
	); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}
	waitComp, err := eng.Compile(context.Background(), "wait_deadline", "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "triggers",
		Name:     "test",
	})
	if err != nil {
		t.Fatalf("Compile wait_deadline: %v", err)
	}
	fastComp, err := eng.Compile(context.Background(), "fast_timeout", "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "triggers",
		Name:     "test",
	})
	if err != nil {
		t.Fatalf("Compile fast_timeout: %v", err)
	}

	// (a) Tracker-deadline case: small tracker (10ms < 100ms dialect MaxEvalTime)
	// cuts short a dialect blocking on <-ctx.Done(). Evaluate returns
	// ErrBudgetExhausted wrapping DeadlineExceeded, exhausts the tracker, and
	// short-circuits subsequent Evaluate calls without invoking the dialect.
	tracker := ruleengine.NewBudgetTracker(10 * time.Millisecond)
	_, err = eng.Evaluate(context.Background(), waitComp, ruleengine.Input{}, ruleengine.Budget{Tracker: tracker})
	if !errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("tracker-deadline err = %v, want ErrBudgetExhausted", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tracker-deadline err = %v, want errors.Is(context.DeadlineExceeded)", err)
	}
	if !tracker.Exhausted() || tracker.Remaining() != 0 {
		t.Fatalf("tracker not exhausted after tracker deadline: Exhausted=%v Remaining=%s", tracker.Exhausted(), tracker.Remaining())
	}
	if got := waitDialect.evalCalls.Load(); got != 1 {
		t.Fatalf("evalCalls = %d, want 1", got)
	}

	_, err = eng.Evaluate(context.Background(), waitComp, ruleengine.Input{}, ruleengine.Budget{Tracker: tracker})
	if !errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("subsequent Evaluate err = %v, want ErrBudgetExhausted", err)
	}
	if got := waitDialect.evalCalls.Load(); got != 1 {
		t.Fatalf("dialect invoked after budget exhaustion: evalCalls = %d, want 1", got)
	}

	// (b) Shorter per-rule timeout while boundFromTracker is true (2s tracker < 10s
	// dialect MaxEvalTime, immediate ReasonTimeout with plenty of budget remaining):
	// must return per-rule FailedError{timeout}, NOT ErrBudgetExhausted, and must NOT exhaust tracker.
	perRuleTracker := ruleengine.NewBudgetTracker(2 * time.Second)
	_, err = eng.Evaluate(context.Background(), fastComp, ruleengine.Input{}, ruleengine.Budget{Tracker: perRuleTracker})
	if errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("shorter per-rule timeout unexpectedly returned ErrBudgetExhausted: %v", err)
	}
	var failedErr *ruleengine.FailedError
	if !errors.As(err, &failedErr) || failedErr.Reason != celengine.ReasonTimeout {
		t.Fatalf("shorter per-rule timeout err = %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonTimeout)
	}
	if perRuleTracker.Exhausted() || perRuleTracker.Remaining() <= 0 {
		t.Fatalf("shorter per-rule timeout unexpectedly exhausted tracker: Exhausted=%v Remaining=%s", perRuleTracker.Exhausted(), perRuleTracker.Remaining())
	}

	// (c) Non-boundFromTracker case (20s tracker > 10s dialect MaxEvalTime):
	// per-rule dialect timeout must NOT exhaust the aggregate tracker.
	wideTracker := ruleengine.NewBudgetTracker(20 * time.Second)
	_, err = eng.Evaluate(context.Background(), fastComp, ruleengine.Input{}, ruleengine.Budget{Tracker: wideTracker})
	if errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("non-boundFromTracker timeout unexpectedly returned ErrBudgetExhausted: %v", err)
	}
	failedErr = nil
	if !errors.As(err, &failedErr) || failedErr.Reason != celengine.ReasonTimeout {
		t.Fatalf("non-boundFromTracker timeout err = %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonTimeout)
	}
	if wideTracker.Exhausted() || wideTracker.Remaining() <= 0 {
		t.Fatalf("wideTracker unexpectedly exhausted on dialect timeout: Exhausted=%v Remaining=%s", wideTracker.Exhausted(), wideTracker.Remaining())
	}
}

func TestEvaluate_MarginWindowTimeoutExhaustsTracker(t *testing.T) {
	eng := ruleengine.NewEngine()
	// Tracker limit 200ms (< 500ms dialect MaxEvalTime => boundFromTracker is true).
	// engineDeadlineMargin(200ms) is 10ms, so returning 5ms before evalCtx.Deadline()
	// lands inside [190ms, 200ms): elapsed >= evalTimeout - margin while elapsed < evalTimeout.
	// Charge(elapsed) alone would leave ~5ms remaining, so this test directly guards
	// budget.Tracker.Exhaust() on the margin-window exhaustion path.
	const (
		trackerLimit = 200 * time.Millisecond
		margin       = 10 * time.Millisecond
	)
	marginDialect := &budgetProbeDialect{
		name:                 "margin_timeout",
		maxEvalTime:          500 * time.Millisecond,
		returnBeforeDeadline: margin / 2,
	}
	if err := eng.Register(marginDialect); err != nil {
		t.Fatalf("Register margin_timeout: %v", err)
	}
	if err := eng.AllowConsumerDialects("triggers", ruleengine.Grant("margin_timeout")); err != nil {
		t.Fatalf("AllowConsumerDialects: %v", err)
	}
	comp, err := eng.Compile(context.Background(), "margin_timeout", "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "triggers",
		Name:     "test",
	})
	if err != nil {
		t.Fatalf("Compile margin_timeout: %v", err)
	}

	tracker := ruleengine.NewBudgetTracker(trackerLimit)
	_, err = eng.Evaluate(context.Background(), comp, ruleengine.Input{}, ruleengine.Budget{Tracker: tracker})
	if !errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("margin-window timeout err = %v, want ErrBudgetExhausted", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("margin-window timeout err = %v, want errors.Is(context.DeadlineExceeded)", err)
	}
	if !tracker.Exhausted() || tracker.Remaining() != 0 {
		t.Fatalf("tracker must be exhausted on margin-window timeout: Exhausted=%v Remaining=%s", tracker.Exhausted(), tracker.Remaining())
	}
	if got := marginDialect.evalCalls.Load(); got != 1 {
		t.Fatalf("evalCalls = %d, want 1", got)
	}

	// Subsequent call on the same tracker must short-circuit without invoking the dialect.
	_, err = eng.Evaluate(context.Background(), comp, ruleengine.Input{}, ruleengine.Budget{Tracker: tracker})
	if !errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("subsequent Evaluate err = %v, want ErrBudgetExhausted", err)
	}
	if got := marginDialect.evalCalls.Load(); got != 1 {
		t.Fatalf("dialect invoked after margin-window exhaustion: evalCalls = %d, want 1", got)
	}
}
