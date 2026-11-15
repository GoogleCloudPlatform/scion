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
	"strings"
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
	eng.AllowConsumerDialects("trigger", ruleengine.DialectCEL)

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

	envRef := ruleengine.EnvironmentRef{Consumer: "trigger", Name: "trigger"}
	ctx := context.Background()

	// Unregistered dialect (e.g. "system_one" in Stage 1) -> unsupported dialect error.
	_, err = eng.Compile(ctx, "system_one", "urgent issue", ruleengine.KindFilter, envRef)
	var unsupported *ruleengine.UnsupportedDialectError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected *UnsupportedDialectError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "unsupported dialect") {
		t.Fatalf("error %q must contain 'unsupported dialect'", err.Error())
	}

	// Consumer dialect allow-list: if consumer restricts to an empty/different set, CEL is rejected too.
	eng.AllowConsumerDialects("restricted_consumer", "other_dialect")
	_, err = eng.Compile(ctx, "cel", "true", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "restricted_consumer",
		Name:     "trigger",
	})
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected *UnsupportedDialectError for disallowed consumer dialect, got %T: %v", err, err)
	}
}

type stubModelDialect struct {
	mode string
}

func (d *stubModelDialect) Name() string { return "stub_model" }
func (d *stubModelDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: 50 * time.Millisecond,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *stubModelDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, _ ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(d.Name(), kind, rule), nil
}
func (d *stubModelDialect) Evaluate(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
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
	stub := &stubModelDialect{}
	if err := eng.Register(stub); err != nil {
		t.Fatalf("Register stub: %v", err)
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
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("MaxEvalTime did not bound dialect evaluation: took %v", elapsed)
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
func (d *payloadCapturingDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, _ ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(d.Name(), kind, rule), nil
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
