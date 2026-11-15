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
