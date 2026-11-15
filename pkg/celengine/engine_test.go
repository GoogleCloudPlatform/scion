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

package celengine_test

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
)

func testEnv() celengine.Environment {
	return celengine.Environment{
		Name:    "test_events",
		Version: 1,
		Variables: []celengine.Variable{
			{Name: "event", Type: celengine.Map(celengine.String(), celengine.Dyn())},
			{Name: "tags", Type: celengine.List(celengine.String())},
			{Name: "meta", Type: celengine.Map(celengine.String(), celengine.String())},
			{Name: "count", Type: celengine.Int()},
			{Name: "ratio", Type: celengine.Double()},
			{Name: "active", Type: celengine.Bool()},
			{Name: "at", Type: celengine.Timestamp()},
		},
		Extensions: []celengine.Extension{celengine.ExtensionStrings},
		Macros:     celengine.MacrosStandard,
	}
}

func newTestEngine(t *testing.T) *celengine.Engine {
	t.Helper()
	eng, err := celengine.New(testEnv(), celengine.DefaultLimits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return eng
}

// newSemanticLargeInputEngine uses DefaultLimits() with EvalTimeout raised to the
// 1s ceiling so 16-64 KiB semantic happy-path tests do not flake under -race or
// 2-vCPU contention (tests specifically exercising the 100ms default keep newTestEngine).
func newSemanticLargeInputEngine(t *testing.T) *celengine.Engine {
	t.Helper()
	lim := celengine.DefaultLimits()
	lim.EvalTimeout = celengine.MaxEvalTimeoutCeiling
	eng, err := celengine.New(testEnv(), lim, celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return eng
}

func scaleWallBound(d time.Duration) time.Duration {
	if !raceEnabled {
		return d
	}
	scaled := 10 * d
	if scaled < 2*time.Second {
		return 2 * time.Second
	}
	return scaled
}

func assertCostLimitOrRaceTimeout(t *testing.T, err error) {
	t.Helper()
	if !raceEnabled {
		assertEvalReason(t, err, celengine.ReasonCostLimit)
		return
	}
	var evalErr *celengine.EvalError
	if !errors.As(err, &evalErr) || (evalErr.Reason != celengine.ReasonCostLimit && evalErr.Reason != celengine.ReasonTimeout) {
		t.Fatalf("EvalError = %v, want cost_limit or timeout under -race", err)
	}
}

func TestNewValidation(t *testing.T) {
	validEnv := testEnv()
	validLimits := celengine.DefaultLimits()
	validCache := celengine.DefaultCacheConfig()

	tests := []struct {
		name   string
		env    celengine.Environment
		limits celengine.Limits
		cache  celengine.CacheConfig
	}{
		{
			name:   "empty env name",
			env:    celengine.Environment{Name: ""},
			limits: validLimits,
			cache:  validCache,
		},
		{
			name: "unknown variable type",
			env: celengine.Environment{
				Name:      "bad_type",
				Version:   1,
				Variables: []celengine.Variable{{Name: "x", Type: celengine.Type{}}},
			},
			limits: validLimits,
			cache:  validCache,
		},
		{
			name: "unknown list element type",
			env: celengine.Environment{
				Name:      "bad_list_type",
				Version:   1,
				Variables: []celengine.Variable{{Name: "x", Type: celengine.List(celengine.Type{})}},
			},
			limits: validLimits,
			cache:  validCache,
		},
		{
			name: "duplicate variable name",
			env: celengine.Environment{
				Name:    "dup_var",
				Version: 1,
				Variables: []celengine.Variable{
					{Name: "x", Type: celengine.Int()},
					{Name: "x", Type: celengine.String()},
				},
			},
			limits: validLimits,
			cache:  validCache,
		},
		{
			name: "unknown extension",
			env: celengine.Environment{
				Name:       "bad_ext",
				Version:    1,
				Extensions: []celengine.Extension{"http"},
			},
			limits: validLimits,
			cache:  validCache,
		},
		{
			name: "unknown macro set",
			env: celengine.Environment{
				Name:    "bad_macros",
				Version: 1,
				Macros:  celengine.MacroSet("custom"),
			},
			limits: validLimits,
			cache:  validCache,
		},
		{
			name:   "zero limits rejected",
			env:    validEnv,
			limits: celengine.Limits{},
			cache:  validCache,
		},
		{
			name: "negative max expr bytes",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:       -1,
				RecursionDepth:     32,
				CostLimit:          1000,
				EvalTimeout:        50 * time.Millisecond,
				InterruptFrequency: 100,
			},
			cache: validCache,
		},
		{
			name: "eval timeout above 1s ceiling",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:       4096,
				RecursionDepth:     32,
				CostLimit:          250_000,
				EvalTimeout:        1500 * time.Millisecond,
				InterruptFrequency: 100,
			},
			cache: validCache,
		},
		{
			name: "cost limit above 10M ceiling",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:       4096,
				RecursionDepth:     32,
				CostLimit:          10_000_001,
				EvalTimeout:        100 * time.Millisecond,
				InterruptFrequency: 100,
			},
			cache: validCache,
		},
		{
			name: "max output bytes above ceiling",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:       4096,
				RecursionDepth:     32,
				CostLimit:          250_000,
				EvalTimeout:        100 * time.Millisecond,
				InterruptFrequency: 100,
				MaxOutputBytes:     celengine.MaxOutputBytesCeiling + 1,
			},
			cache: validCache,
		},
		{
			name: "negative max output bytes",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:       4096,
				RecursionDepth:     32,
				CostLimit:          250_000,
				EvalTimeout:        100 * time.Millisecond,
				InterruptFrequency: 100,
				MaxOutputBytes:     -1,
			},
			cache: validCache,
		},
		{
			name: "max collection elements above ceiling",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:          4096,
				RecursionDepth:        32,
				CostLimit:             250_000,
				EvalTimeout:           100 * time.Millisecond,
				InterruptFrequency:    100,
				MaxCollectionElements: celengine.MaxCollectionElementsCeiling + 1,
			},
			cache: validCache,
		},
		{
			name: "max regex program size above ceiling",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:        4096,
				RecursionDepth:      32,
				CostLimit:           250_000,
				EvalTimeout:         100 * time.Millisecond,
				InterruptFrequency:  100,
				MaxRegexProgramSize: celengine.MaxRegexProgramSizeCeiling + 1,
			},
			cache: validCache,
		},
		{
			name: "negative max regex program size",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:        4096,
				RecursionDepth:      32,
				CostLimit:           250_000,
				EvalTimeout:         100 * time.Millisecond,
				InterruptFrequency:  100,
				MaxRegexProgramSize: -1,
			},
			cache: validCache,
		},
		{
			name: "max regex work above ceiling",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:       4096,
				RecursionDepth:     32,
				CostLimit:          250_000,
				EvalTimeout:        100 * time.Millisecond,
				InterruptFrequency: 100,
				MaxRegexWork:       celengine.MaxRegexWorkCeiling + 1,
			},
			cache: validCache,
		},
		{
			name: "negative max regex work",
			env:  validEnv,
			limits: celengine.Limits{
				MaxExprBytes:       4096,
				RecursionDepth:     32,
				CostLimit:          250_000,
				EvalTimeout:        100 * time.Millisecond,
				InterruptFrequency: 100,
				MaxRegexWork:       -1,
			},
			cache: validCache,
		},
		{
			name:   "negative cache size",
			env:    validEnv,
			limits: validLimits,
			cache:  celengine.CacheConfig{MaxEntries: -1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := celengine.New(tc.env, tc.limits, tc.cache); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestCompileErrorCategoriesAndPositions(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	strKind := celengine.Kind{Name: "subject", Result: celengine.String()}

	tests := []struct {
		name         string
		expr         string
		kind         celengine.Kind
		wantCategory string
		wantLine     int
		wantMinCol   int
	}{
		{
			name:         "size exceeded",
			expr:         strings.Repeat("a", celengine.DefaultMaxExprBytes+1),
			kind:         boolKind,
			wantCategory: celengine.CategorySize,
			wantLine:     1,
			wantMinCol:   1,
		},
		{
			name:         "syntax error with line and column",
			expr:         "active &&\n  == true",
			kind:         boolKind,
			wantCategory: celengine.CategorySyntax,
			wantLine:     2,
			wantMinCol:   3,
		},
		{
			name:         "parser recursion limit exceeded",
			expr:         strings.Repeat("(", 40) + "true" + strings.Repeat(")", 40),
			kind:         boolKind,
			wantCategory: celengine.CategorySyntax,
			wantLine:     1,
			wantMinCol:   1,
		},
		{
			name:         "unknown variable",
			expr:         "undeclared_var == 1",
			kind:         boolKind,
			wantCategory: celengine.CategoryUnknownVariable,
			wantLine:     1,
			wantMinCol:   1,
		},
		{
			name:         "unknown function",
			expr:         `httpGet("https://example.com") == "ok"`,
			kind:         boolKind,
			wantCategory: celengine.CategoryUnknownFunction,
			wantLine:     1,
			wantMinCol:   1,
		},
		{
			name:         "type mismatch in operator",
			expr:         `count == "not-an-int"`,
			kind:         boolKind,
			wantCategory: celengine.CategoryType,
			wantLine:     1,
			wantMinCol:   7,
		},
		{
			name:         "invalid literal regex fails at compile",
			expr:         `meta["k"].matches("[unclosed")`,
			kind:         boolKind,
			wantCategory: celengine.CategoryType,
			wantLine:     1,
			wantMinCol:   1,
		},
		{
			name:         "wrong static result type int for bool",
			expr:         `count + 1`,
			kind:         boolKind,
			wantCategory: celengine.CategoryResultType,
			wantLine:     1,
			wantMinCol:   1,
		},
		{
			name:         "wrong static result type bool for string",
			expr:         `active && count > 0`,
			kind:         strKind,
			wantCategory: celengine.CategoryResultType,
			wantLine:     1,
			wantMinCol:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Compile(tc.expr, tc.kind)
			if err == nil {
				t.Fatalf("expected compile error for %q, got nil", tc.expr)
			}
			var cerr *celengine.CompileError
			if !errors.As(err, &cerr) {
				t.Fatalf("expected *CompileError, got %T: %v", err, err)
			}
			if cerr.Category != tc.wantCategory {
				t.Errorf("Category = %q, want %q (err: %v)", cerr.Category, tc.wantCategory, cerr)
			}
			if cerr.Kind != tc.kind.Name {
				t.Errorf("Kind = %q, want %q", cerr.Kind, tc.kind.Name)
			}
			if cerr.Line != tc.wantLine {
				t.Errorf("Line = %d, want %d (err: %v)", cerr.Line, tc.wantLine, cerr)
			}
			if cerr.Column < tc.wantMinCol {
				t.Errorf("Column = %d, want >= %d (err: %v)", cerr.Column, tc.wantMinCol, cerr)
			}
		})
	}
}

func TestEvalResultTypesAndAccessors(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	strKind := celengine.Kind{Name: "subject", Result: celengine.String()}

	// Bool program + checked accessors.
	boolProg, err := eng.Compile(`active && meta["env"].startsWith("prod") && ratio > 0.5`, boolKind)
	if err != nil {
		t.Fatalf("Compile bool: %v", err)
	}
	res, stats, err := boolProg.Eval(context.Background(), map[string]any{
		"active": true,
		"meta":   map[string]string{"env": "prod-us"},
		"ratio":  0.75,
	})
	if err != nil {
		t.Fatalf("Eval bool: %v", err)
	}
	if stats.Cost == 0 {
		t.Errorf("expected non-zero cost in stats, got %+v", stats)
	}
	gotBool, err := res.Bool()
	if err != nil || !gotBool {
		t.Fatalf("res.Bool() = (%v, %v), want (true, nil)", gotBool, err)
	}
	if _, err := res.String(); err == nil {
		t.Errorf("expected error calling String() on bool Result")
	}

	// Dynamic expression compiled as string kind: succeeds when runtime value is string,
	// returns EvalError{Reason: "type"} when runtime value is non-string.
	dynStrProg, err := eng.Compile(`event.payload.service`, strKind)
	if err != nil {
		t.Fatalf("Compile dyn string: %v", err)
	}
	strRes, _, err := dynStrProg.Eval(context.Background(), map[string]any{
		"event": map[string]any{
			"payload": map[string]any{"service": "payments"},
		},
	})
	if err != nil {
		t.Fatalf("Eval dyn string: %v", err)
	}
	gotStr, err := strRes.String()
	if err != nil || gotStr != "payments" {
		t.Fatalf("strRes.String() = (%q, %v), want (\"payments\", nil)", gotStr, err)
	}
	if _, err := strRes.Bool(); err == nil {
		t.Errorf("expected error calling Bool() on string Result")
	}

	// Dynamic expression returning int at runtime for a string kind -> ReasonType.
	_, _, err = dynStrProg.Eval(context.Background(), map[string]any{
		"event": map[string]any{
			"payload": map[string]any{"service": 42},
		},
	})
	assertEvalReason(t, err, celengine.ReasonType)
}

func TestHasMacroAndRuntimeErrors(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}

	prog, err := eng.Compile(`has(event.payload) && has(event.payload.branch) && event.payload.branch == "main"`, boolKind)
	if err != nil {
		t.Fatalf("Compile has(): %v", err)
	}

	// Missing nested key guarded by has() evaluates cleanly to false (not an error).
	res, _, err := prog.Eval(context.Background(), map[string]any{
		"event": map[string]any{"payload": map[string]any{"other": "x"}},
	})
	if err != nil {
		t.Fatalf("Eval guarded has(): %v", err)
	}
	if b, err := res.Bool(); err != nil || b {
		t.Fatalf("Bool() = (%v, %v), want (false, nil)", b, err)
	}

	// Division by zero -> ReasonRuntime (and content-free error text).
	divProg, err := eng.Compile(`10 / count == 5`, boolKind)
	if err != nil {
		t.Fatalf("Compile div: %v", err)
	}
	_, _, err = divProg.Eval(context.Background(), map[string]any{"count": 0})
	assertEvalReason(t, err, celengine.ReasonRuntime)
	if strings.Contains(err.Error(), "division") {
		t.Errorf("EvalError must remain content-free, got %q", err.Error())
	}
}

func TestLinearRE2MatchesAndDynamicRegexError(t *testing.T) {
	eng := newTestEngine(t)
	if raceEnabled {
		eng = newSemanticLargeInputEngine(t)
	}
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}

	// Classic PCRE catastrophic-backtracking input runs in microseconds on RE2.
	reProg, err := eng.Compile(`meta["input"].matches("^(a+)+$")`, boolKind)
	if err != nil {
		t.Fatalf("Compile matches: %v", err)
	}
	start := time.Now()
	res, _, err := reProg.Eval(context.Background(), map[string]any{
		"meta": map[string]string{"input": strings.Repeat("a", 40) + "!"},
	})
	if err != nil {
		t.Fatalf("Eval matches: %v", err)
	}
	if elapsed := time.Since(start); elapsed > scaleWallBound(100*time.Millisecond) {
		t.Errorf("RE2 match took unexpectedly long: %v", elapsed)
	}
	if b, _ := res.Bool(); b {
		t.Errorf("expected false match")
	}

	// Option A: non-literal regex pattern is rejected at Compile, never reaching Eval.
	_, err = eng.Compile(`meta["input"].matches(meta["pattern"])`, boolKind)
	if err == nil {
		t.Fatalf("expected CompileError for dynamic matches pattern")
	}
	var dynCerr *celengine.CompileError
	if !errors.As(err, &dynCerr) || dynCerr.Category != celengine.CategoryRegexLiteralRequired {
		t.Fatalf("Compile dynamic matches = %v, want Category %q", err, celengine.CategoryRegexLiteralRequired)
	}
}

func TestCostLimitTimeoutAndCanceledDistinction(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}

	// Cartesian comprehension over tags: string concatenation charges cost proportional
	// to string length in CEL's CostTracker, hitting 250_000 well within 100ms even under -race.
	heavyExpr := `tags.all(a, tags.all(b, (a + b).size() >= 0))`
	prog, err := eng.Compile(heavyExpr, boolKind)
	if err != nil {
		t.Fatalf("Compile heavy comprehension: %v", err)
	}

	bigItem := strings.Repeat("x", 1024)
	items := make([]string, 80)
	for i := range items {
		items[i] = bigItem
	}

	// 1. Default CostLimit (250_000) halts comprehension with ReasonCostLimit.
	_, _, err = prog.Eval(context.Background(), map[string]any{"tags": items})
	assertEvalReason(t, err, celengine.ReasonCostLimit)

	// 2. Per-call lowered CostLimit halts a smaller input, while trying to raise above ceiling is clamped.
	smallItems := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	_, _, err = prog.Eval(context.Background(), map[string]any{"tags": smallItems}, celengine.Limits{
		CostLimit: 10,
	})
	assertEvalReason(t, err, celengine.ReasonCostLimit)

	// Trying to raise CostLimit above engine ceiling does NOT bypass the ceiling.
	_, _, err = prog.Eval(context.Background(), map[string]any{"tags": items}, celengine.Limits{
		CostLimit: 9_000_000,
	})
	assertEvalReason(t, err, celengine.ReasonCostLimit)

	// 3. Engine timeout (1ms ceiling, high CostLimit ceiling) surfaces as ReasonTimeout.
	timeoutLimits := celengine.DefaultLimits()
	timeoutLimits.CostLimit = celengine.MaxCostLimitCeiling
	timeoutLimits.EvalTimeout = 1 * time.Millisecond
	timeoutLimits.InterruptFrequency = 1
	timeoutEng, err := celengine.New(testEnv(), timeoutLimits, celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New timeoutEng: %v", err)
	}
	timeoutProg, err := timeoutEng.Compile(
		`tags.all(a, tags.all(b, tags.all(c, a.size() + b.size() + c.size() >= 0)))`,
		boolKind,
	)
	if err != nil {
		t.Fatalf("Compile timeoutProg: %v", err)
	}
	_, _, err = timeoutProg.Eval(context.Background(), map[string]any{"tags": items})
	assertEvalReason(t, err, celengine.ReasonTimeout)

	// 4. Caller context with shorter deadline / pre-canceled surfaces as ReasonCanceled, NOT ReasonTimeout.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = prog.Eval(canceledCtx, map[string]any{"tags": smallItems})
	assertEvalReason(t, err, celengine.ReasonCanceled)

	longLimits := celengine.DefaultLimits()
	longLimits.CostLimit = celengine.MaxCostLimitCeiling
	longLimits.EvalTimeout = 500 * time.Millisecond
	longLimits.InterruptFrequency = 1
	longEng, err := celengine.New(testEnv(), longLimits, celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New longEng: %v", err)
	}
	longProg, err := longEng.Compile(
		`tags.all(a, tags.all(b, tags.all(c, a.size() + b.size() + c.size() >= 0)))`,
		boolKind,
	)
	if err != nil {
		t.Fatalf("Compile longProg: %v", err)
	}
	shortCallerCtx, shortCancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer shortCancel()
	_, _, err = longProg.Eval(shortCallerCtx, map[string]any{"tags": items})
	assertEvalReason(t, err, celengine.ReasonCanceled)
}

func TestProgramCacheContentIdentityAndInvalidation(t *testing.T) {
	eng, err := celengine.New(testEnv(), celengine.DefaultLimits(), celengine.CacheConfig{
		MaxEntries:     4,
		MaxPerConsumer: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()

	// Two distinct rules with identical expression share one compiled program.
	scope1 := celengine.CacheScope{Consumer: "trigger", ID: "trig-1", Revision: 1}
	scope2 := celengine.CacheScope{Consumer: "trigger", ID: "trig-2", Revision: 1}

	_, err = eng.Cached(ctx, scope1, `active && count > 0`, boolKind)
	if err != nil {
		t.Fatalf("Cached 1: %v", err)
	}
	_, err = eng.Cached(ctx, scope2, `active && count > 0`, boolKind)
	if err != nil {
		t.Fatalf("Cached 2: %v", err)
	}
	stats := eng.CacheStats()
	if stats.Entries != 1 || stats.Misses != 1 || stats.Hits != 1 {
		t.Fatalf("expected shared entry (Entries=1, Misses=1, Hits=1), got %+v", stats)
	}

	// Invalidating trig-1 keeps the shared program alive for trig-2.
	eng.Invalidate(scope1)
	if got := eng.CacheStats().Entries; got != 1 {
		t.Fatalf("Entries after invalidating 1 of 2 sharers = %d, want 1", got)
	}

	// Editing trig-2 (revision bump + new expression) detaches trig-2 from old expression,
	// evicting the now-unreferenced old expression and caching the new one.
	scope2Rev2 := celengine.CacheScope{Consumer: "trigger", ID: "trig-2", Revision: 2}
	_, err = eng.Cached(ctx, scope2Rev2, `active && count > 10`, boolKind)
	if err != nil {
		t.Fatalf("Cached rev 2: %v", err)
	}
	if got := eng.CacheStats().Entries; got != 1 {
		t.Fatalf("Entries after sole sharer revision edit = %d, want 1", got)
	}

	// Invalidating trig-2 removes the remaining entry.
	eng.Invalidate(scope2Rev2)
	if got := eng.CacheStats().Entries; got != 0 {
		t.Fatalf("Entries after invalidating last sharer = %d, want 0", got)
	}

	// MaxPerConsumer (2) bounds entries for a single consumer.
	for i, expr := range []string{`count == 1`, `count == 2`, `count == 3`} {
		sc := celengine.CacheScope{Consumer: "trigger", ID: string(rune('a' + i)), Revision: 1}
		if _, err := eng.Cached(ctx, sc, expr, boolKind); err != nil {
			t.Fatalf("Cached %s: %v", expr, err)
		}
	}
	if got := eng.CacheStats().Entries; got > 2 {
		t.Fatalf("MaxPerConsumer=2 exceeded: Entries = %d", got)
	}
}

func TestProgramCacheMultiKindSameScope(t *testing.T) {
	eng, err := celengine.New(testEnv(), celengine.DefaultLimits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	filterKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	subjectKind := celengine.Kind{Name: "subject", Result: celengine.String()}
	ctx := context.Background()

	scope := celengine.CacheScope{Consumer: "trigger", ID: "trig-multi", Revision: 1}
	for i := 0; i < 5; i++ {
		if _, err := eng.Cached(ctx, scope, `active && count > 0`, filterKind); err != nil {
			t.Fatalf("iter %d Cached filter: %v", i, err)
		}
		if _, err := eng.Cached(ctx, scope, `"subj:" + meta["label"]`, subjectKind); err != nil {
			t.Fatalf("iter %d Cached subject: %v", i, err)
		}
	}

	stats := eng.CacheStats()
	if stats.Entries != 2 || stats.Misses != 2 || stats.Hits != 8 {
		t.Fatalf("expected multi-kind scope retention (Entries=2, Misses=2, Hits=8), got %+v", stats)
	}

	// Bumping revision and compiling only filter retains the unchanged filter hit
	// while evicting the unreferenced prior-revision subject entry.
	scopeRev2 := celengine.CacheScope{Consumer: "trigger", ID: "trig-multi", Revision: 2}
	if _, err := eng.Cached(ctx, scopeRev2, `active && count > 0`, filterKind); err != nil {
		t.Fatalf("rev2 Cached filter: %v", err)
	}
	stats = eng.CacheStats()
	if stats.Entries != 1 || stats.Hits != 9 || stats.Misses != 2 {
		t.Fatalf("expected rev2 filter hit + stale subject eviction (Entries=1, Hits=9, Misses=2), got %+v", stats)
	}

	// Re-bind both kinds at rev2 and verify Invalidate drops all kinds for the scope.
	if _, err := eng.Cached(ctx, scopeRev2, `"v2:" + meta["label"]`, subjectKind); err != nil {
		t.Fatalf("rev2 Cached subject: %v", err)
	}
	if got := eng.CacheStats().Entries; got != 2 {
		t.Fatalf("Entries with both rev2 kinds = %d, want 2", got)
	}

	// Stale rev1 invalidation is a no-op.
	eng.Invalidate(scope)
	if got := eng.CacheStats().Entries; got != 2 {
		t.Fatalf("Entries after stale rev1 Invalidate = %d, want 2", got)
	}

	eng.Invalidate(scopeRev2)
	if got := eng.CacheStats().Entries; got != 0 {
		t.Fatalf("Entries after rev2 Invalidate = %d, want 0", got)
	}
}

func TestFunctionsListAndMacrosNone(t *testing.T) {
	eng := newTestEngine(t)
	fns := eng.Functions()
	if !slices.IsSorted(fns) {
		t.Errorf("Functions() must return sorted list, got %v", fns)
	}
	for _, want := range []string{"has", "all", "exists", "matches", "startsWith", "endsWith", "trim"} {
		if !slices.Contains(fns, want) {
			t.Errorf("Functions() missing expected %q in %v", want, fns)
		}
	}

	// MacrosNone disables comprehension macros like has() and all().
	noMacroEnv := testEnv()
	noMacroEnv.Macros = celengine.MacrosNone
	noMacroEng, err := celengine.New(noMacroEnv, celengine.DefaultLimits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New noMacroEng: %v", err)
	}
	if slices.Contains(noMacroEng.Functions(), "has") {
		t.Errorf("MacrosNone environment should not list 'has' macro")
	}
	_, err = noMacroEng.Compile(`has(event.payload)`, celengine.Kind{Name: "filter", Result: celengine.Bool()})
	if err == nil {
		t.Fatalf("expected compile error for has() when MacrosNone is configured")
	}
}

func assertEvalReason(t *testing.T, err error, wantReason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected EvalError with reason %q, got nil", wantReason)
	}
	var eerr *celengine.EvalError
	if !errors.As(err, &eerr) {
		t.Fatalf("expected *EvalError, got %T: %v", err, err)
	}
	if eerr.Reason != wantReason {
		t.Fatalf("EvalError.Reason = %q, want %q", eerr.Reason, wantReason)
	}
}

func TestMaxOutputBytesLimitEnforced(t *testing.T) {
	lim := celengine.DefaultLimits()
	lim.MaxOutputBytes = 4096
	lim.MaxCollectionElements = 256

	eng, err := celengine.New(testEnv(), lim, celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()

	s1k := strings.Repeat("a", 1024)
	quotes2k := strings.Repeat(`"`, 2500)
	act := map[string]any{
		"event": map[string]any{
			"s":      s1k,
			"quotes": quotes2k,
		},
	}

	cases := []struct {
		name string
		expr string
	}{
		{name: "split_join", expr: `event.s.split("").join(event.s).size() > 0`},
		{name: "replace_all", expr: `event.s.replace("a", event.s).size() > 0`},
		{name: "replace_empty_old", expr: `event.s.replace("", event.s).size() > 0`},
		{name: "replace_n", expr: `event.s.replace("a", event.s, 10).size() > 0`},
		{name: "split_elements", expr: `event.s.split("").size() > 0`},
		{name: "format_args", expr: `"%s%s%s%s%s".format([event.s, event.s, event.s, event.s, event.s]).size() > 0`},
		{name: "format_nested_list", expr: `"%s".format([[event.s, event.s, event.s, event.s, event.s]]).size() > 0`},
		{name: "format_hex_double", expr: `"%x%x%x".format([event.s, event.s, event.s]).size() > 0`},
		{name: "strings_quote", expr: `strings.quote(event.quotes).size() > 0`},
		{name: "add_string", expr: `[event.s].map(a, [a + a].map(b, [b + b].map(c, c + c)))[0][0][0].size() > 4096`},
		{name: "add_bytes", expr: `[bytes(event.s)].map(a, [a + a].map(b, [b + b].map(c, c + c)))[0][0][0].size() > 4096`},
		{name: "add_list", expr: `[[1, 2, 3, 4, 5]].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, [d + d].map(e, [e + e].map(f, f + f))))))[0][0][0][0][0][0].size() > 0`},
		{name: "non_strict_or_cannot_swallow", expr: `event.s.replace("a", event.s) == "" || true`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := eng.Compile(tc.expr, boolKind)
			if err != nil {
				t.Fatalf("Compile(%s): %v", tc.expr, err)
			}
			res, _, evalErr := prog.Eval(ctx, act)
			if res != nil {
				t.Fatalf("expected nil Result on output limit violation, got %v", res)
			}
			assertEvalReason(t, evalErr, celengine.ReasonCostLimit)
		})
	}
}

func TestAmplifyingFunctionsBoundedOn64KiBPayload(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()

	s60k := strings.Repeat("a", 60*1024)
	quotes60k := strings.Repeat("\n", 60*1024)
	act := map[string]any{
		"event": map[string]any{
			"s":       s60k,
			"quotes":  quotes60k,
			"oversiz": strings.Repeat("z", 2<<20),
		},
	}

	// 60 KiB split("") = 61,440 elements; l + l = 122,880 > MaxCollectionElements (65,536).
	nestedListConcat := `[event.s.split("")].map(l, (l + l).size() > 0)[0]`

	// Nested DAG of 8-element lists (each list has Size()==8, 6 levels => 8^6 = 262,144 leaves,
	// well under ParserRecursionLimit(32) while exceeding the container walk bound).
	buildDAG := func(seed string) string {
		cur := "[[" + seed + "]]"
		for i := 0; i < 6; i++ {
			v := "d" + string(rune('a'+i))
			cur += ".map(" + v + ", [[" + v + ", " + v + ", " + v + ", " + v + ", " + v + ", " + v + ", " + v + ", " + v + "]])[0]"
		}
		return cur
	}
	dagEqExpr := buildDAG("1") + " == " + buildDAG("1")
	dagNeExpr := buildDAG("1") + " != " + buildDAG("2")
	dagInExpr := buildDAG("2") + " in [" + buildDAG("1") + "]"

	cases := []struct {
		name string
		expr string
	}{
		{
			name: "split_empty_join_60k",
			expr: `event.s.split("").join(event.s).size() > 0`,
		},
		{
			name: "replace_all_60k",
			expr: `event.s.replace("a", event.s).size() > 0`,
		},
		{
			name: "replace_empty_old_60k",
			expr: `event.s.replace("", event.s).size() > 0`,
		},
		{
			name: "replace_n_60k",
			expr: `event.s.replace("a", event.s, 20).size() > 0`,
		},
		{
			name: "format_repeated_60k",
			expr: `"%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s".format([event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s]).size() > 0`,
		},
		{
			name: "format_nested_list_60k",
			expr: `"%s".format([[event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s]]).size() > 0`,
		},
		{
			name: "format_hex_doubling_60k",
			expr: `"%x%x%x%x%x%x%x%x%x%x".format([event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s, event.s]).size() > 0`,
		},
		{
			name: "strings_quote_doubling",
			expr: `[event.quotes].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, strings.quote(d + d)))))[0][0][0][0].size() > 0`,
		},
		{
			name: "string_add_doubling",
			expr: `[event.s].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, [d + d].map(e, e + e)))))[0][0][0][0][0].size() > 0`,
		},
		{
			name: "bytes_add_doubling",
			expr: `[bytes(event.s)].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, [d + d].map(e, e + e)))))[0][0][0][0][0].size() > 0`,
		},
		{
			name: "split_over_max_collection_elements",
			expr: `[event.s].map(a, [a + a].map(b, b.split("")))[0][0].size() > 0`,
		},
		{
			name: "concat_list_over_max_collection_elements",
			expr: nestedListConcat,
		},
		{
			name: "nested_dag_equality",
			expr: dagEqExpr,
		},
		{
			name: "nested_dag_inequality",
			expr: dagNeExpr,
		},
		{
			name: "nested_dag_in",
			expr: dagInExpr,
		},
		{
			name: "oversized_activation_lowerAscii",
			expr: `event.oversiz.lowerAscii().size() > 0`,
		},
		{
			name: "oversized_activation_reverse",
			expr: `event.oversiz.reverse().size() > 0`,
		},
		{
			name: "oversized_activation_substring",
			expr: `event.oversiz.substring(0, 10).size() > 0`,
		},
		{
			name: "oversized_activation_charAt",
			expr: `event.oversiz.charAt(0) == "z"`,
		},
		{
			name: "oversized_activation_indexOf",
			expr: `event.oversiz.indexOf("z") >= 0`,
		},
	}

	maxElapsed := scaleWallBound(2 * celengine.DefaultLimits().EvalTimeout)
	const maxAllocDelta = 64 << 20 // 64 MiB

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := eng.Compile(tc.expr, boolKind)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			var mBefore, mAfter runtime.MemStats
			runtime.ReadMemStats(&mBefore)
			start := time.Now()
			res, _, evalErr := prog.Eval(ctx, act)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&mAfter)

			if res != nil {
				t.Fatalf("expected nil Result, got %v", res)
			}
			assertCostLimitOrRaceTimeout(t, evalErr)
			if elapsed > maxElapsed {
				t.Errorf("elapsed %s exceeds wall-clock bound (%s)", elapsed, maxElapsed)
			}
			allocDelta := mAfter.TotalAlloc - mBefore.TotalAlloc
			if allocDelta > maxAllocDelta {
				t.Errorf("TotalAlloc delta %d bytes (%.2f MiB) exceeds 64 MiB bound", allocDelta, float64(allocDelta)/(1<<20))
			}
		})
	}

	// Verify linear-time KMP indexOf / lastIndexOf on worst-case "aaa...a" vs "aaa...ab"
	// within the 1 MiB output bound completes fast instead of O(N*M) CPU stall.
	t.Run("linear_time_indexOf_and_lastIndexOf", func(t *testing.T) {
		haystack := strings.Repeat("a", 200*1024)
		needle := strings.Repeat("a", 100*1024) + "b"
		searchAct := map[string]any{
			"event": map[string]any{
				"h": haystack,
				"n": needle,
			},
		}
		// Raise CostLimit slightly so stringSearchCostModel (which charges N*M*0.1) does
		// not mask CPU time, or test under a 10M CostLimit engine to prove the search itself
		// runs in O(N+M) milliseconds rather than O(N*M) seconds.
		highCostLim := celengine.DefaultLimits()
		highCostLim.CostLimit = celengine.MaxCostLimitCeiling
		if raceEnabled {
			highCostLim.EvalTimeout = celengine.MaxEvalTimeoutCeiling
		}
		searchEng, err := celengine.New(testEnv(), highCostLim, celengine.DefaultCacheConfig())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for _, expr := range []string{`event.h.indexOf(event.n) == -1`, `event.h.lastIndexOf(event.n) == -1`} {
			prog, err := searchEng.Compile(expr, boolKind)
			if err != nil {
				t.Fatalf("Compile(%s): %v", expr, err)
			}
			start := time.Now()
			res, _, evalErr := prog.Eval(ctx, searchAct)
			elapsed := time.Since(start)
			// Under stringSearchCostModel 200k*100k*0.1 > 10M, CostTracker charges AFTER the call:
			// either it returns true or ReasonCostLimit (or ReasonTimeout under -race), and critically
			// the call itself MUST finish quickly instead of ~5s of O(N*M) rune loops!
			if evalErr != nil {
				assertCostLimitOrRaceTimeout(t, evalErr)
			} else if got, _ := res.Bool(); !got {
				t.Fatalf("%s = false, want true", expr)
			}
			if elapsed > maxElapsed {
				t.Errorf("%s took %s, want <= %s (must use linear-time search)", expr, elapsed, maxElapsed)
			}
		}
	})
}

func TestGuardedFunctionsNormalSemanticsPreserved(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	strKind := celengine.Kind{Name: "subject", Result: celengine.String()}
	ctx := context.Background()

	act := map[string]any{
		"event": map[string]any{
			"s":    "hello mellow",
			"uni":  "héllo 🌍 héllo",
			"bad":  string([]byte{'a', 0xff, 'b'}),
			"nums": []any{int64(1), int64(2), int64(3)},
			"mix":  []any{"a", int64(2)},
			"dynN": "not-an-int",
		},
		"tags":  []string{"alpha", "beta", "gamma"},
		"meta":  map[string]string{"k": "v"},
		"count": int64(42),
		"ratio": 3.14,
		"at":    time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC),
	}

	trueExprs := []string{
		`event.s.replace("he", "we") == "wello mellow"`,
		`"hello hello".replace("he", "we", 1) == "wello hello"`,
		`"hello hello".replace("he", "we", 0) == "hello hello"`,
		`"hello hello".replace("he", "we", -1) == "wello wello"`,
		`"ab".replace("", "_") == "_a_b_"`,
		`"ab".replace("", "_", 2) == "_a_b"`,
		`tags.join() == "alphabetagamma"`,
		`tags.join("-") == "alpha-beta-gamma"`,
		`[].join("/") == ""`,
		`"a,b,c".split(",") == ["a", "b", "c"]`,
		`"a,b,c".split(",", 0) == []`,
		`"a,b,c".split(",", 1) == ["a,b,c"]`,
		`"a,b,c".split(",", 2) == ["a", "b,c"]`,
		`"a,b,c".split(",", -1) == ["a", "b", "c"]`,
		`"hé".split("") == ["h", "é"]`,
		`"%s:%d:%.2f:%x:%X:%o:%b:%%".format(["ok", count, ratio, "ab", 26, 8, true]) == "ok:42:3.14:6162:1A:10:1:%"`,
		`"%s".format([[1, 2, {"a": "x"}]]) == "[1, 2, {a: x}]"`,
		`strings.quote("hi \"there\"\n\t\\") == "\"hi \\\"there\\\"\\n\\t\\\\\""`,
		`strings.quote(event.bad) == "\"a\ufffdb\""`,
		`event.s.indexOf("") == 0`,
		`event.s.indexOf("", 5) == 5`,
		`event.s.indexOf("", 99) == 12`,
		`event.s.indexOf("ello") == 1`,
		`event.s.indexOf("ello", 2) == 7`,
		`event.s.indexOf("ello", 20) == -1`,
		`event.uni.indexOf("🌍") == 6`,
		`event.uni.indexOf("héllo", 1) == 8`,
		`event.s.lastIndexOf("") == 12`,
		`event.s.lastIndexOf("", 4) == 4`,
		`event.s.lastIndexOf("", 99) == 12`,
		`event.s.lastIndexOf("ello") == 7`,
		`event.s.lastIndexOf("ello", 6) == 1`,
		`event.s.lastIndexOf("ello", 20) == -1`,
		`event.uni.lastIndexOf("héllo") == 8`,
		`event.uni.lastIndexOf("héllo", 7) == 0`,
		`event.uni.charAt(6) == "🌍"`,
		`"hello".charAt(5) == ""`,
		`"TacoCÆt".lowerAscii() == "tacocÆt"`,
		`"TacoCÆt".upperAscii() == "TACOCÆT"`,
		`"héllo 🌍".reverse() == "🌍 olléh"`,
		`event.uni.substring(6) == "🌍 héllo"`,
		`event.uni.substring(6, 7) == "🌍"`,
		`count + 8 == 50`,
		`ratio + 0.86 == 4.0`,
		`"foo" + "bar" == "foobar"`,
		`bytes("ab") + bytes("cd") == bytes("abcd")`,
		`[1, 2] + [3, 4] == [1, 2, 3, 4]`,
		`tags.map(t, t.upperAscii()) == ["ALPHA", "BETA", "GAMMA"]`,
		`at + duration("10s") > at`,
		`"beta" in tags`,
		`!("delta" in tags)`,
		`"k" in meta`,
	}

	for _, expr := range trueExprs {
		prog, err := eng.Compile(expr, boolKind)
		if err != nil {
			t.Fatalf("Compile(%s): %v", expr, err)
		}
		res, _, err := prog.Eval(ctx, act)
		if err != nil {
			t.Fatalf("Eval(%s) unexpected error: %v", expr, err)
		}
		got, err := res.Bool()
		if err != nil || !got {
			t.Fatalf("Eval(%s) = (%v, %v), want (true, nil)", expr, got, err)
		}
	}

	// Verify runtime errors on invalid indices/types still surface as ReasonRuntime (and can be short-circuited by || when non-cost errors).
	runtimeErrExprs := []string{
		`"hello".charAt(-1)`,
		`"hello".charAt(6)`,
		`"hello".substring(-1)`,
		`"hello".substring(3, 2)`,
		`"hello".substring(0, 9)`,
		`"%d".format([event.dynN])`,
		`event.mix.join(",")`,
	}
	for _, expr := range runtimeErrExprs {
		prog, err := eng.Compile(expr, strKind)
		if err != nil {
			t.Fatalf("Compile(%s): %v", expr, err)
		}
		_, _, evalErr := prog.Eval(ctx, act)
		assertEvalReason(t, evalErr, celengine.ReasonRuntime)
	}
	for _, expr := range []string{`"hello".indexOf("e", -1) == 0`, `"hello".lastIndexOf("e", -1) == 0`} {
		prog, err := eng.Compile(expr, boolKind)
		if err != nil {
			t.Fatalf("Compile(%s): %v", expr, err)
		}
		_, _, evalErr := prog.Eval(ctx, act)
		assertEvalReason(t, evalErr, celengine.ReasonRuntime)
	}
}

func runNoPanic(t *testing.T, label string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s escaped panic: %v", label, r)
		}
	}()
	fn()
}

func TestPlanTimeBoundRecovery(t *testing.T) {
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()

	t.Run("eval_replan_const_unary_exceeds_lowered_max_output_bytes", func(t *testing.T) {
		eng := newTestEngine(t)
		prog, err := eng.Compile(`string(b"0123456789abcdef0123456789") == "x"`, boolKind)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		runNoPanic(t, "Eval lowered MaxOutputBytes", func() {
			res, _, evalErr := prog.Eval(ctx, nil, celengine.Limits{MaxOutputBytes: 8})
			if res != nil {
				t.Fatalf("expected nil Result, got %v", res)
			}
			assertEvalReason(t, evalErr, celengine.ReasonCostLimit)
		})
	})

	t.Run("compile_const_unary_exceeds_engine_max_output_bytes", func(t *testing.T) {
		lim := celengine.DefaultLimits()
		lim.MaxOutputBytes = 1024
		eng, err := celengine.New(testEnv(), lim, celengine.DefaultCacheConfig())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		expr := `string(b"` + strings.Repeat("a", 2000) + `") == "x"`
		runNoPanic(t, "Compile lowered MaxOutputBytes", func() {
			_, compErr := eng.Compile(expr, boolKind)
			if compErr == nil {
				t.Fatalf("expected CompileError, got nil")
			}
			var cerr *celengine.CompileError
			if !errors.As(compErr, &cerr) {
				t.Fatalf("expected *CompileError, got %T: %v", compErr, compErr)
			}
			if cerr.Category != celengine.CategorySize {
				t.Fatalf("CompileError.Category = %q, want %q (err: %v)", cerr.Category, celengine.CategorySize, cerr)
			}
		})
	})
}

func TestMatchesRegexSizeAndWorkBounds(t *testing.T) {
	eng := newTestEngine(t)
	semEng := newSemanticLargeInputEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()

	// 1. Literal oversized counted-repetition regex fails at Compile with CategorySize.
	t.Run("literal_oversized_regex_compile_error", func(t *testing.T) {
		_, err := eng.Compile(`event.s.matches("[a-z]{0,1000}x")`, boolKind)
		if err == nil {
			t.Fatalf("expected CompileError for oversized literal regex")
		}
		var cerr *celengine.CompileError
		if !errors.As(err, &cerr) {
			t.Fatalf("expected *CompileError, got %T: %v", err, err)
		}
		if cerr.Category != celengine.CategorySize {
			t.Fatalf("Category = %q, want %q (err: %v)", cerr.Category, celengine.CategorySize, cerr)
		}
	})

	// 2. Literal regex within MaxRegexProgramSize (244 <= 256) that exceeds MaxRegexWork
	// on a 60 KiB input MUST still be caught at Eval even with cel-go's CompileRegexConstants enabled.
	t.Run("literal_in_size_limit_exceeds_work_bound_survives_optimizer", func(t *testing.T) {
		pat := strings.Repeat("a?", 80) + strings.Repeat("a", 80) + "x"
		prog, err := eng.Compile(`event.s.matches("`+pat+`")`, boolKind)
		if err != nil {
			t.Fatalf("Compile in-limit regex: %v", err)
		}
		start := time.Now()
		res, _, evalErr := prog.Eval(ctx, map[string]any{
			"event": map[string]any{"s": strings.Repeat("a", 60*1024)},
		})
		elapsed := time.Since(start)
		if res != nil {
			t.Fatalf("expected nil Result, got %v", res)
		}
		assertCostLimitOrRaceTimeout(t, evalErr)
		if elapsed > scaleWallBound(50*time.Millisecond) {
			t.Errorf("pre-call work check took %s, want <= %s", elapsed, scaleWallBound(50*time.Millisecond))
		}
	})

	// 3. Dynamic regex patterns are rejected at Compile with CategoryRegexLiteralRequired (Option A).
	t.Run("dynamic_patterns_rejected_at_compile", func(t *testing.T) {
		for _, expr := range []string{
			`event.s.matches(event.p) || true`,
			`matches(string(event.s), string(event.p))`,
		} {
			_, err := eng.Compile(expr, boolKind)
			if err == nil {
				t.Fatalf("Compile(%s): expected CompileError, got nil", expr)
			}
			var cerr *celengine.CompileError
			if !errors.As(err, &cerr) {
				t.Fatalf("Compile(%s): expected *CompileError, got %T: %v", expr, err, err)
			}
			if cerr.Category != celengine.CategoryRegexLiteralRequired {
				t.Fatalf("Compile(%s): Category = %q, want %q", expr, cerr.Category, celengine.CategoryRegexLiteralRequired)
			}
		}
	})

	// 4. Normal literal matches on 60 KiB inputs within work bound succeed.
	t.Run("normal_literal_matches_on_60k", func(t *testing.T) {
		s60k := "webhook.deploy." + strings.Repeat("a", 60*1024)
		litProg, err := semEng.Compile(`event.s.matches("^webhook\\.deploy\\..*$")`, boolKind)
		if err != nil {
			t.Fatalf("Compile normal literal: %v", err)
		}
		res, _, err := litProg.Eval(ctx, map[string]any{"event": map[string]any{"s": s60k}})
		if err != nil {
			t.Fatalf("Eval normal literal: %v", err)
		}
		if b, _ := res.Bool(); !b {
			t.Fatalf("expected true match")
		}
	})

	// 5. Realistic keyword and conventional-commit patterns succeed on full 64 KiB
	// payloads under DefaultMaxRegexWork = 8 MiB, while repeated counted-repetition
	// matches calls charge progSize to CostTracker and abort on cost_limit.
	t.Run("realistic_patterns_on_64k_and_counted_rep_cost_charged", func(t *testing.T) {
		s64k := strings.Repeat("ok ", 21840) + "panic: boom" // ~65,531 bytes <= 64 KiB
		commit64k := "feat(triggers): " + strings.Repeat("x", 65500)

		kwProg, err := semEng.Compile(`event.s.matches("(?i)\\b(error|failed|panic)\\b")`, boolKind)
		if err != nil {
			t.Fatalf("Compile keyword regex: %v", err)
		}
		res, _, err := kwProg.Eval(ctx, map[string]any{"event": map[string]any{"s": s64k}})
		if err != nil {
			t.Fatalf("Eval keyword regex on 64k: %v", err)
		}
		if b, _ := res.Bool(); !b {
			t.Fatalf("expected keyword regex match on 64k")
		}

		ccProg, err := semEng.Compile(`event.s.matches("^(feat|fix|chore)(\\([a-z0-9_-]+\\))?!?: .+")`, boolKind)
		if err != nil {
			t.Fatalf("Compile conventional-commit regex: %v", err)
		}
		res, _, err = ccProg.Eval(ctx, map[string]any{"event": map[string]any{"s": commit64k}})
		if err != nil {
			t.Fatalf("Eval conventional-commit regex on 64k: %v", err)
		}
		if b, _ := res.Bool(); !b {
			t.Fatalf("expected conventional-commit regex match on 64k")
		}

		// "a{1,120}b" has len=9 but progSize=244: upstream len-based cost would charge
		// only ~303 on 1 KiB (allowing >800 calls), whereas progSize-based charging
		// charges 6,161 per call and aborts repeated matches on CostLimit (or
		// ReasonTimeout via CheckInterrupt under -race instrumentation).
		s1kA := strings.Repeat("a", 1_000)
		singleRepProg, err := semEng.Compile(`event.s.matches("a{1,120}b")`, boolKind)
		if err != nil {
			t.Fatalf("Compile single counted-rep: %v", err)
		}
		_, singleStats, err := singleRepProg.Eval(ctx, map[string]any{"event": map[string]any{"s": s1kA}})
		if err != nil {
			t.Fatalf("Eval single counted-rep: %v", err)
		}
		if singleStats.Cost < 6_000 {
			t.Fatalf("single counted-rep Cost = %d, want >= 6000 (progSize-proportional)", singleStats.Cost)
		}

		repLoopProg, err := eng.Compile(`event.l.exists(x, event.s.matches("a{1,120}b"))`, boolKind)
		if err != nil {
			t.Fatalf("Compile counted-repetition loop: %v", err)
		}
		loopIdx := make([]any, 200)
		for i := range loopIdx {
			loopIdx[i] = int64(i)
		}
		start := time.Now()
		res, _, evalErr := repLoopProg.Eval(ctx, map[string]any{"event": map[string]any{"s": s1kA, "l": loopIdx}})
		elapsed := time.Since(start)
		if res != nil {
			t.Fatalf("expected nil Result on counted-repetition matches loop, got %v", res)
		}
		var repEvalErr *celengine.EvalError
		if !errors.As(evalErr, &repEvalErr) || (repEvalErr.Reason != celengine.ReasonCostLimit && repEvalErr.Reason != celengine.ReasonTimeout) {
			t.Fatalf("EvalError = %v, want cost_limit or timeout", evalErr)
		}
		maxLoopElapsed := scaleWallBound(2 * celengine.DefaultEvalTimeout)
		if elapsed > maxLoopElapsed {
			t.Errorf("counted-repetition loop took %s, want <= %s", elapsed, maxLoopElapsed)
		}
	})
}

func TestEvalTimeoutBackstopNeverReturnsDecision(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	prog, err := eng.Compile(`event.s.contains("needle")`, boolKind)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// Single host call completes with err == nil before CEL's periodic interrupt check,
	// while wall-clock Elapsed exceeds the 1ns per-call EvalTimeout.
	res, stats, evalErr := prog.Eval(
		context.Background(),
		map[string]any{"event": map[string]any{"s": strings.Repeat("a", 60*1024) + "needle"}},
		celengine.Limits{EvalTimeout: time.Nanosecond},
	)
	if res != nil {
		t.Fatalf("expected nil Result when Elapsed (%s) exceeds EvalTimeout, got %v", stats.Elapsed, res)
	}
	assertEvalReason(t, evalErr, celengine.ReasonTimeout)
}

func TestComprehensionContainerWalkAndFormatBounded(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()

	list12k := make([]any, 12_000)
	for i := range list12k {
		list12k[i] = int64(0)
	}
	s20k := strings.Repeat("a", 20_000)
	nestedDyn := []any{[]any{list12k, list12k, list12k, int64(-1)}}

	act := map[string]any{
		"event": map[string]any{
			"s":  s20k,
			"l":  list12k,
			"ll": nestedDyn,
		},
	}

	buildDisjunction := func(termFmt string, count int) string {
		terms := make([]string, count)
		for i := range terms {
			terms[i] = fmt.Sprintf(termFmt, i)
		}
		return "event.l.exists(x, " + strings.Join(terms, " || ") + ")"
	}

	probes := []struct {
		name string
		expr string
	}{
		{
			name: "eq_25_terms_in_exists",
			expr: buildDisjunction("[event.l,event.l,event.l,event.l,event.l] == [event.l,event.l,event.l,event.l,%d]", 25),
		},
		{
			name: "in_list_25_terms_in_exists",
			expr: buildDisjunction("[event.l,event.l,event.l,event.l] in [[event.l,event.l,event.l,%d]]", 25),
		},
		{
			name: "in_dyn_25_terms_in_exists",
			expr: buildDisjunction("[event.l,event.l,event.l,%d] in event.ll", 25),
		},
		{
			name: "format_31_terms_in_exists",
			expr: buildDisjunction(`"%%s".format([event.l]) == "b%d"`, 31),
		},
	}

	maxElapsed := scaleWallBound(2 * celengine.DefaultEvalTimeout)
	const maxAllocDelta = 64 << 20 // 64 MiB

	for _, tc := range probes {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.expr) > celengine.DefaultMaxExprBytes {
				t.Fatalf("probe len %d exceeds DefaultMaxExprBytes %d", len(tc.expr), celengine.DefaultMaxExprBytes)
			}
			prog, err := eng.Compile(tc.expr, boolKind)
			if err != nil {
				t.Fatalf("Compile(%s, %dB): %v", tc.name, len(tc.expr), err)
			}

			var mBefore, mAfter runtime.MemStats
			runtime.ReadMemStats(&mBefore)
			start := time.Now()
			res, stats, evalErr := prog.Eval(ctx, act)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&mAfter)

			allocDelta := mAfter.TotalAlloc - mBefore.TotalAlloc
			t.Logf("probe=%s bytes=%d elapsed=%s alloc=%.2fMiB cost=%d err=%v",
				tc.name, len(tc.expr), elapsed, float64(allocDelta)/(1<<20), stats.Cost, evalErr)

			if res != nil {
				t.Fatalf("expected nil Result on bounded abort, got %v", res)
			}
			if evalErr == nil {
				t.Fatalf("expected non-nil EvalError, got nil")
			}
			var eerr *celengine.EvalError
			if !errors.As(evalErr, &eerr) || (eerr.Reason != celengine.ReasonCostLimit && eerr.Reason != celengine.ReasonTimeout) {
				t.Fatalf("EvalError = %v, want cost_limit or timeout", evalErr)
			}
			if elapsed > maxElapsed {
				t.Errorf("elapsed %s exceeds wall-clock bound (%s)", elapsed, maxElapsed)
			}
			if allocDelta > maxAllocDelta {
				t.Errorf("TotalAlloc delta %.2f MiB exceeds 64 MiB", float64(allocDelta)/(1<<20))
			}

			// Re-evaluating the same shared Program on a small input must use a fresh
			// per-evaluation cost tracker (never poisoned by the prior cost_limit abort).
			benignAct := map[string]any{
				"event": map[string]any{
					"s":  "a",
					"l":  []any{int64(0)},
					"ll": []any{[]any{[]any{int64(0)}, []any{int64(0)}, []any{int64(0)}, int64(-1)}},
				},
			}
			res2, _, err2 := prog.Eval(ctx, benignAct)
			if err2 != nil {
				t.Fatalf("subsequent benign Eval on shared Program failed: %v", err2)
			}
			if b, boolErr := res2.Bool(); boolErr != nil || b {
				t.Fatalf("subsequent benign Eval = (%v, %v), want (false, nil)", b, boolErr)
			}
		})
	}

	t.Run("default_interrupt_frequency_bounds_high_cost_ceiling", func(t *testing.T) {
		if celengine.DefaultInterruptFrequency != 1 {
			t.Fatalf("DefaultInterruptFrequency = %d, want 1", celengine.DefaultInterruptFrequency)
		}
		lim := celengine.DefaultLimits()
		lim.CostLimit = celengine.MaxCostLimitCeiling
		hiEng, err := celengine.New(testEnv(), lim, celengine.DefaultCacheConfig())
		if err != nil {
			t.Fatalf("New high-cost engine: %v", err)
		}
		expr := "event.l.exists(x, [event.l] == [event.l] && x == -1)"
		prog, err := hiEng.Compile(expr, boolKind)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		start := time.Now()
		res, _, evalErr := prog.Eval(ctx, act)
		elapsed := time.Since(start)
		if res != nil {
			t.Fatalf("expected nil Result, got %v", res)
		}
		var eerr *celengine.EvalError
		if !errors.As(evalErr, &eerr) || (eerr.Reason != celengine.ReasonTimeout && eerr.Reason != celengine.ReasonCostLimit) {
			t.Fatalf("EvalError = %v, want timeout or cost_limit", evalErr)
		}
		if elapsed > maxElapsed {
			t.Errorf("elapsed %s under MaxCostLimitCeiling exceeds wall-clock bound (%s)", elapsed, maxElapsed)
		}
	})
}

func TestOptionalCeilingValidationErrorMessages(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*celengine.Limits)
		field  string
	}{
		{
			name:   "negative_max_output_bytes",
			mutate: func(l *celengine.Limits) { l.MaxOutputBytes = -1 },
			field:  "MaxOutputBytes must be 0 (default) or 1..",
		},
		{
			name:   "above_ceiling_max_collection_elements",
			mutate: func(l *celengine.Limits) { l.MaxCollectionElements = celengine.MaxCollectionElementsCeiling + 1 },
			field:  "MaxCollectionElements must be 0 (default) or 1..",
		},
		{
			name:   "negative_max_regex_program_size",
			mutate: func(l *celengine.Limits) { l.MaxRegexProgramSize = -1 },
			field:  "MaxRegexProgramSize must be 0 (default) or 1..",
		},
		{
			name:   "above_ceiling_max_regex_work",
			mutate: func(l *celengine.Limits) { l.MaxRegexWork = celengine.MaxRegexWorkCeiling + 1 },
			field:  "MaxRegexWork must be 0 (default) or 1..",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lim := celengine.DefaultLimits()
			tc.mutate(&lim)
			_, err := celengine.New(testEnv(), lim, celengine.DefaultCacheConfig())
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.field)
			}
		})
	}
}

func TestSystemOneModeBenignProseRegexAndPredicateProbe(t *testing.T) {
	eng := newTestEngine(t)
	if raceEnabled {
		eng = newSemanticLargeInputEngine(t)
	}
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}

	prose16k := strings.Repeat("Reviewing candidate change notes and rollout telemetry. ", 295) +
		"Decision: please ESCALATE to the release lead before rollout."
	if len(prose16k) < 16*1024 {
		t.Fatalf("prose16k len = %d, want >= 16 KiB", len(prose16k))
	}

	expr := `active && count >= 1 && ratio < 1.0 && size(tags) == 0 && ` +
		`meta["tier"] == "prod" && ` +
		`event.prose.matches("(?i)\\b(approve|reject|defer|escalate|hold)\\b")`
	prog, err := eng.Compile(expr, boolKind)
	if err != nil {
		t.Fatalf("Compile benign prose regex rule: %v", err)
	}

	act := map[string]any{
		"active": true,
		"count":  int64(3),
		"ratio":  0.42,
		"tags":   []string{},
		"meta":   map[string]string{"tier": "prod"},
		"event": map[string]any{
			"prose": prose16k,
		},
	}

	res, stats, err := prog.Eval(context.Background(), act)
	t.Logf("system_one_benign_probe: prose_bytes=%d elapsed=%s cost=%d", len(prose16k), stats.Elapsed, stats.Cost)
	if err != nil {
		t.Fatalf("Eval benign prose regex rule failed under DefaultLimits: %v (stats=%+v)", err, stats)
	}
	matched, err := res.Bool()
	if err != nil || !matched {
		t.Fatalf("res.Bool() = (%v, %v), want (true, nil)", matched, err)
	}
	if stats.Cost >= celengine.DefaultCostLimit {
		t.Fatalf("stats.Cost = %d, want < DefaultCostLimit (%d)", stats.Cost, celengine.DefaultCostLimit)
	}
}

func TestConcatListTraversalChargedAndBounded(t *testing.T) {
	eng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	strKind := celengine.Kind{Name: "subject", Result: celengine.String()}
	ctx := context.Background()

	buildConcatChain := func(varExpr string, depth int) string {
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = varExpr
		}
		return "(" + strings.Join(parts, " + ") + ")"
	}

	// 1. Depth-30 left-nested concat of payload lists across ==, !=, in, join, and %s
	// must charge result size across the chain (stats.Cost >= elements * depth) and
	// abort with ReasonCostLimit without building O(depth) lazy concatList traversal.
	const depth = 30
	const elems = 2184
	strPayload := make([]any, elems)
	for i := range strPayload {
		strPayload[i] = "x"
	}
	act := map[string]any{
		"event": map[string]any{
			"a": strPayload,
		},
	}

	chain30 := buildConcatChain("event.a", depth)
	consumers := []struct {
		name string
		expr string
	}{
		{name: "eq", expr: chain30 + " == " + chain30},
		{name: "ne", expr: chain30 + " != " + chain30},
		{name: "in", expr: `"z" in ` + chain30},
		{name: "join", expr: fmt.Sprintf(`[dyn(%s)].map(c, c.map(x, string(x)).join("") == "")[0]`, chain30)},
		{name: "format_s", expr: `"%s".format([` + chain30 + `]) == ""`},
		{name: "chained_or_ne_eq", expr: `[` + chain30 + `].map(c, c != c || c != c || c == c)[0]`},
	}

	// Also exercise direct list<string> `tags + tags + ...` (static AddList overload, no map wrapper) for join.
	tagsPayload := make([]string, elems)
	for i := range tagsPayload {
		tagsPayload[i] = "x"
	}
	act["tags"] = tagsPayload
	tagsChain30 := buildConcatChain("tags", depth)
	consumers = append(consumers, struct {
		name string
		expr string
	}{
		name: "join_typed_tags",
		expr: tagsChain30 + `.join("") == ""`,
	})

	minExpectedCost := uint64(elems * depth)
	maxElapsed := scaleWallBound(2 * celengine.DefaultEvalTimeout)

	for _, tc := range consumers {
		t.Run("depth30_"+tc.name, func(t *testing.T) {
			prog, err := eng.Compile(tc.expr, boolKind)
			if err != nil {
				t.Fatalf("Compile(%s): %v", tc.name, err)
			}
			start := time.Now()
			res, stats, evalErr := prog.Eval(ctx, act)
			elapsed := time.Since(start)
			if res != nil {
				t.Fatalf("%s returned decision %v (elapsed=%s, cost=%d), want nil Result", tc.name, res, elapsed, stats.Cost)
			}
			if !raceEnabled {
				assertEvalReason(t, evalErr, celengine.ReasonCostLimit)
			} else {
				assertCostLimitOrRaceTimeout(t, evalErr)
			}
			if stats.Cost < minExpectedCost {
				t.Errorf("%s stats.Cost = %d, want >= %d (elements*depth = %d*%d)", tc.name, stats.Cost, minExpectedCost, elems, depth)
			}
			if elapsed > maxElapsed {
				t.Errorf("%s elapsed %s exceeds wall-clock bound %s", tc.name, elapsed, maxElapsed)
			}
		})
	}

	// 2. Benign small concat chain preserves exact semantics across all 5 consumers
	// while still charging >= elements * depth.
	t.Run("benign_depth10_semantics_and_cost", func(t *testing.T) {
		smallAct := map[string]any{
			"tags": []string{"a", "b", "c"},
		}
		chain10 := buildConcatChain("tags", 10)
		wantMinCost := uint64(3 * 10)

		for _, expr := range []string{
			chain10 + " == " + chain10,
			`!(` + chain10 + " != " + chain10 + `)`,
			`"c" in ` + chain10,
			`!("z" in ` + chain10 + `)`,
		} {
			prog, err := eng.Compile(expr, boolKind)
			if err != nil {
				t.Fatalf("Compile(%s): %v", expr, err)
			}
			res, stats, err := prog.Eval(ctx, smallAct)
			if err != nil {
				t.Fatalf("Eval(%s): %v", expr, err)
			}
			if b, bErr := res.Bool(); bErr != nil || !b {
				t.Fatalf("Eval(%s) = (%v, %v), want (true, nil)", expr, b, bErr)
			}
			if stats.Cost < wantMinCost {
				t.Errorf("Eval(%s) Cost = %d, want >= %d", expr, stats.Cost, wantMinCost)
			}
		}

		joinProg, err := eng.Compile(chain10+`.join("")`, strKind)
		if err != nil {
			t.Fatalf("Compile join: %v", err)
		}
		joinRes, joinStats, err := joinProg.Eval(ctx, smallAct)
		if err != nil {
			t.Fatalf("Eval join: %v", err)
		}
		if s, sErr := joinRes.String(); sErr != nil || s != strings.Repeat("abc", 10) {
			t.Fatalf("join = (%q, %v), want %q", s, sErr, strings.Repeat("abc", 10))
		}
		if joinStats.Cost < wantMinCost {
			t.Errorf("join Cost = %d, want >= %d", joinStats.Cost, wantMinCost)
		}

		fmtProg, err := eng.Compile(`"%s".format([`+chain10+`])`, strKind)
		if err != nil {
			t.Fatalf("Compile format: %v", err)
		}
		fmtRes, fmtStats, err := fmtProg.Eval(ctx, smallAct)
		if err != nil {
			t.Fatalf("Eval format: %v", err)
		}
		if s, sErr := fmtRes.String(); sErr != nil || !strings.HasPrefix(s, "[a, b, c, a,") {
			t.Fatalf("format = (%q, %v), want prefix '[a, b, c, a,'", s, sErr)
		}
		if fmtStats.Cost < wantMinCost {
			t.Errorf("format Cost = %d, want >= %d", fmtStats.Cost, wantMinCost)
		}
	})
}

func TestTimezoneEmbeddedLRUCacheAndCost(t *testing.T) {
	semEng := newSemanticLargeInputEngine(t)
	semEng2 := newSemanticLargeInputEngine(t)
	defaultEng := newTestEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()
	refTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	// 1. Valid IANA zone: cold miss pre-charges >= 250, warm hit on same Engine charges ~1,
	// and a second Engine has an isolated LRU (cold miss >= 250).
	t.Run("valid_iana_cold_miss_warm_hit_and_per_engine_isolation", func(t *testing.T) {
		prog1, err := semEng.Compile(`at.getHours(meta["tz"]) == 1`, boolKind) // Auckland in Jan = UTC+13 -> 01:00 next day
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		act := map[string]any{
			"at":   refTime,
			"meta": map[string]string{"tz": "Pacific/Auckland"},
		}
		res, coldStats, err := prog1.Eval(ctx, act)
		if err != nil {
			t.Fatalf("cold Eval Pacific/Auckland: %v", err)
		}
		if b, _ := res.Bool(); !b {
			t.Fatalf("expected Pacific/Auckland hour == 1")
		}
		if coldStats.Cost < 250 {
			t.Errorf("cold miss stats.Cost = %d, want >= 250", coldStats.Cost)
		}

		res, warmStats, err := prog1.Eval(ctx, act)
		if err != nil {
			t.Fatalf("warm Eval Pacific/Auckland: %v", err)
		}
		if b, _ := res.Bool(); !b {
			t.Fatalf("expected warm Pacific/Auckland hour == 1")
		}
		if warmStats.Cost >= 20 {
			t.Errorf("warm hit stats.Cost = %d, want < 20 (1 for cached tz lookup)", warmStats.Cost)
		}

		// Independent Engine must not share semEng's timezone cache.
		prog2, err := semEng2.Compile(`at.getHours(meta["tz"]) == 1`, boolKind)
		if err != nil {
			t.Fatalf("Compile eng2: %v", err)
		}
		_, coldStats2, err := prog2.Eval(ctx, act)
		if err != nil {
			t.Fatalf("cold Eval on eng2: %v", err)
		}
		if coldStats2.Cost < 250 {
			t.Errorf("eng2 cold miss stats.Cost = %d, want >= 250 (per-Engine cache isolation)", coldStats2.Cost)
		}
	})

	// 2. Negative / invalid and > 64B timezone names charge >= 250, cache negative entries,
	// and abort repeated invalid-timezone loops with ReasonCostLimit.
	t.Run("negative_cached_and_invalid_loop_cost_bounded", func(t *testing.T) {
		prog, err := semEng.Compile(`at.getHours(meta["tz"]) == 0`, boolKind)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		for _, badTZ := range []string{
			"NoSuchArea/NoSuchCity",
			"NoSuchArea/NoSuchCity", // warm negative LRU hit
			strings.Repeat("A", 80), // > 64B rejected in memory without fallback
			"../America/New_York",
			"America//New_York",
			"+25:00",
			"00:99",
		} {
			res, stats, evalErr := prog.Eval(ctx, map[string]any{
				"at":   refTime,
				"meta": map[string]string{"tz": badTZ},
			})
			if res != nil {
				t.Fatalf("badTZ %q returned decision %v", badTZ, res)
			}
			assertEvalReason(t, evalErr, celengine.ReasonRuntime)
			if stats.Cost < 250 {
				t.Errorf("badTZ %q stats.Cost = %d, want >= 250", badTZ, stats.Cost)
			}
		}

		// 300 distinct invalid zone names x 5 iterations = 1500 lookups * 250 > 250,000 CostLimit.
		badZones := make([]string, 300)
		for i := range badZones {
			badZones[i] = fmt.Sprintf("InvalidZone/City_%03d", i)
		}
		loopExpr := `[0, 1, 2, 3, 4].exists(r, tags.exists(z, at.getHours(z) == -99 || false))`
		loopProg, err := defaultEng.Compile(loopExpr, boolKind)
		if err != nil {
			t.Fatalf("Compile invalid tz loop: %v", err)
		}
		res, _, evalErr := loopProg.Eval(ctx, map[string]any{
			"at":   refTime,
			"tags": badZones,
		})
		if res != nil {
			t.Fatalf("expected nil Result on invalid timezone flood, got %v", res)
		}
		assertCostLimitOrRaceTimeout(t, evalErr)
	})

	// 3. Fixed numeric offsets, UTC, empty string, and Local preserve CEL semantics at cost ~1.
	t.Run("fixed_offsets_and_builtin_zones", func(t *testing.T) {
		exprs := []string{
			`at.getHours("") == 12`,
			`at.getHours("UTC") == 12`,
			`at.getHours("+05:30") == 17 && at.getMinutes("+05:30") == 30`,
			`at.getHours("-00:30") == 11 && at.getMinutes("-00:30") == 30`,
			`at.getHours("05:30") == 17`,
			`at.getHours("Local") >= 0`,
		}
		for _, expr := range exprs {
			prog, err := semEng.Compile(expr, boolKind)
			if err != nil {
				t.Fatalf("Compile(%s): %v", expr, err)
			}
			res, stats, err := prog.Eval(ctx, map[string]any{"at": refTime})
			if err != nil {
				t.Fatalf("Eval(%s): %v", expr, err)
			}
			if b, _ := res.Bool(); !b {
				t.Fatalf("Eval(%s) = false, want true", expr)
			}
			if stats.Cost >= 50 {
				t.Errorf("Eval(%s) stats.Cost = %d, want < 50", expr, stats.Cost)
			}
		}
	})

	// 4. P5-O1 regression: timezone resolution NEVER reads $ZONEINFO or disk.
	// Planting a valid TZif file under $ZONEINFO/Fake/Zone succeeds under stdlib
	// time.LoadLocation, but celengine MUST reject it with ReasonRuntime.
	t.Run("planted_ZONEINFO_directory_never_consulted_no_disk_io", func(t *testing.T) {
		zr, err := zip.OpenReader("tzdata/zoneinfo.zip")
		if err != nil {
			t.Fatalf("open embedded zoneinfo.zip fixture: %v", err)
		}
		defer func() { _ = zr.Close() }()

		var tokyoBytes []byte
		for _, f := range zr.File {
			if f.Name == "Asia/Tokyo" {
				rc, err := f.Open()
				if err != nil {
					t.Fatalf("open Asia/Tokyo: %v", err)
				}
				tokyoBytes, err = io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					t.Fatalf("read Asia/Tokyo: %v", err)
				}
				break
			}
		}
		if len(tokyoBytes) == 0 {
			t.Fatalf("Asia/Tokyo not found in tzdata/zoneinfo.zip")
		}

		tmpDir := t.TempDir()
		fakeZonePath := filepath.Join(tmpDir, "Fake", "Zone")
		if err := os.MkdirAll(filepath.Dir(fakeZonePath), 0o755); err != nil {
			t.Fatalf("mkdir Fake: %v", err)
		}
		if err := os.WriteFile(fakeZonePath, tokyoBytes, 0o644); err != nil {
			t.Fatalf("write Fake/Zone: %v", err)
		}

		// Run in a fresh subprocess so Go's process-wide stdlib zoneinfoOnce
		// latches ZONEINFO=tmpDir cleanly on every -count=N iteration.
		cmd := exec.Command(os.Args[0], "-test.run=^TestPlantedZoneinfoSubprocess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"ZONEINFO="+tmpDir,
			"CELENGINE_TEST_PLANTED_ZONEINFO=1",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("TestPlantedZoneinfoSubprocess failed: %v\n%s", err, string(out))
		}
	})
}

func TestPlantedZoneinfoSubprocess(t *testing.T) {
	if os.Getenv("CELENGINE_TEST_PLANTED_ZONEINFO") != "1" {
		return
	}
	zoneDir := os.Getenv("ZONEINFO")
	// Confirm stdlib time.LoadLocation WOULD resolve Fake/Zone from $ZONEINFO.
	if _, stdErr := time.LoadLocation("Fake/Zone"); stdErr != nil {
		t.Fatalf("expected stdlib time.LoadLocation(Fake/Zone) to succeed with ZONEINFO=%s: %v", zoneDir, stdErr)
	}

	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	refTime := time.Date(2026, 11, 15, 12, 34, 56, 0, time.UTC)
	freshEng := newTestEngine(t)
	for _, expr := range []string{
		`at.getHours("Fake/Zone") == 21`,
		`at.getHours(meta["tz"]) == 21`,
	} {
		prog, err := freshEng.Compile(expr, boolKind)
		if err != nil {
			t.Fatalf("Compile(%s): %v", expr, err)
		}
		res, stats, evalErr := prog.Eval(context.Background(), map[string]any{
			"at":   refTime,
			"meta": map[string]string{"tz": "Fake/Zone"},
		})
		if res != nil {
			t.Fatalf("Eval(%s) resolved planted disk zone Fake/Zone: %v", expr, res)
		}
		assertEvalReason(t, evalErr, celengine.ReasonRuntime)
		if stats.Cost < 250 {
			t.Errorf("Eval(%s) stats.Cost = %d, want >= 250", expr, stats.Cost)
		}
	}
}

func TestMatchesLiteralOnlyPreScanAndCommonPatterns(t *testing.T) {
	eng := newTestEngine(t)
	semEng := newSemanticLargeInputEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()

	// 1. Non-literal pattern arguments (receiver, global, leading-dot global,
	// payload field, attribute, concatenation, comprehension join, map/list index,
	// conditional) fail at Compile with CategoryRegexLiteralRequired + position.
	t.Run("non_literal_patterns_rejected_with_position_and_category", func(t *testing.T) {
		exprBuiltPass6 := `"a".matches("(?i)" + [0,1,2].map(i, [0,1,2].map(j, r"[\x{41}-\x{1e942}]").join()).join())`
		cases := []struct {
			name       string
			expr       string
			wantLine   int
			wantMinCol int
		}{
			{
				name:       "receiver_payload_field",
				expr:       `"a".matches(event.payload.p)`,
				wantLine:   1,
				wantMinCol: 13,
			},
			{
				name:       "global_payload_field",
				expr:       `matches("a", event.payload.p)`,
				wantLine:   1,
				wantMinCol: 14,
			},
			{
				name:       "leading_dot_global_payload_field",
				expr:       `.matches("a", event.payload.p)`,
				wantLine:   1,
				wantMinCol: 15,
			},
			{
				name:       "attribute_pattern",
				expr:       `"a".matches(meta["pat"])`,
				wantLine:   1,
				wantMinCol: 13,
			},
			{
				name:       "concatenation_pattern",
				expr:       `"a".matches("(?i)" + "[a-z]+")`,
				wantLine:   1,
				wantMinCol: 13,
			},
			{
				name:       "pass6_expression_built_map_join_pattern",
				expr:       exprBuiltPass6,
				wantLine:   1,
				wantMinCol: 13,
			},
			{
				name:       "list_indexed_pattern",
				expr:       `"a".matches(["^a$"][0])`,
				wantLine:   1,
				wantMinCol: 13,
			},
			{
				name:       "map_indexed_pattern",
				expr:       `"a".matches({"p": "^a$"}["p"])`,
				wantLine:   1,
				wantMinCol: 13,
			},
			{
				name:       "conditional_pattern",
				expr:       `"a".matches(active ? "^a$" : "^b$")`,
				wantLine:   1,
				wantMinCol: 13,
			},
			{
				name:       "nested_in_comprehension",
				expr:       `tags.exists(x, x.matches(meta["pat"]))`,
				wantLine:   1,
				wantMinCol: 26,
			},
			{
				name:       "has_wrapped_receiver_non_literal",
				expr:       `has(event.payload.p) && "a".matches(event.payload.p)`,
				wantLine:   1,
				wantMinCol: 37,
			},
			{
				name:       "has_wrapped_global_non_literal",
				expr:       `has(event.payload.p) ? matches("a", event.payload.p) : false`,
				wantLine:   1,
				wantMinCol: 37,
			},
			{
				name:       "multiline_position",
				expr:       "true &&\n  \"a\".matches(event.payload.p)",
				wantLine:   2,
				wantMinCol: 15,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				start := time.Now()
				_, err := eng.Compile(tc.expr, boolKind)
				elapsed := time.Since(start)
				if err == nil {
					t.Fatalf("Compile(%s) succeeded, want CompileError", tc.expr)
				}
				var cerr *celengine.CompileError
				if !errors.As(err, &cerr) {
					t.Fatalf("Compile(%s) err = %T (%v), want *CompileError", tc.expr, err, err)
				}
				if cerr.Category != celengine.CategoryRegexLiteralRequired {
					t.Errorf("Category = %q, want %q (err=%v)", cerr.Category, celengine.CategoryRegexLiteralRequired, cerr)
				}
				if cerr.Line != tc.wantLine || cerr.Column < tc.wantMinCol {
					t.Errorf("pos = %d:%d, want line %d col >= %d", cerr.Line, cerr.Column, tc.wantLine, tc.wantMinCol)
				}
				if elapsed > scaleWallBound(50*time.Millisecond) {
					t.Errorf("Compile(%s) took %s, want <= %s", tc.name, elapsed, scaleWallBound(50*time.Millisecond))
				}
			})
		}
	})

	// 2. Linear authoring pre-scan rejects over-long literal patterns and
	// case-folded character-class range bombs BEFORE regexp/syntax parses them.
	t.Run("prescan_rejects_folded_ranges_and_overlong_patterns_fast", func(t *testing.T) {
		// Pass-6 4,022-byte literal pattern (250 folded ranges; previously 2.8 s Compile).
		pass6Literal := `"a".matches(r"(?i)[` + strings.Repeat(`\x{41}-\x{1e942}`, 250) + `]")`

		// Multi-call cumulative folded-range span across one rule (10 calls * 320 runes = 3,200 > 2,048).
		tenCalls := make([]string, 10)
		for i := range tenCalls {
			tenCalls[i] = `"a".matches(r"(?i)[\x{41}-\x{180}]")`
		}
		cumulativeExpr := strings.Join(tenCalls, " || ")

		cases := []struct {
			name string
			expr string
		}{
			{name: "pass6_4k_literal_250_folded_ranges", expr: pass6Literal},
			{name: "single_short_folded_range_hex_braces", expr: `"a".matches(r"(?i)[\x{41}-\x{1e942}]")`},
			{name: "global_matches_short_folded_range", expr: `matches("a", r"(?i)[\x{41}-\x{1e942}]")`},
			{name: "leading_dot_global_short_folded_range", expr: `.matches("a", r"(?i)[\x{41}-\x{1e942}]")`},
			{name: "inline_flag_group_ims_folded_range", expr: `"a".matches(r"(?ims:[\x{41}-\x{1000}])")`},
			{name: "late_inline_flag_folded_range", expr: `"a".matches(r"(?:foo)(?i)[\x{41}-\x{1000}]")`},
			{name: "unclosed_posix_lookalike_folded_range", expr: `"a".matches(r"(?i)[[:\x{41}-\x{1000}]")`},
			{name: "escaped_backslash_before_class_folded_range", expr: `"a".matches(r"(?i)\\[\x{41}-\x{1000}]")`},
			{name: "utf8_literal_endpoints_folded_range", expr: `"a".matches("(?i)[A-\u1000]")`},
			{name: "cumulative_multi_call_folded_ranges", expr: cumulativeExpr},
			{name: "overlong_513_byte_pattern", expr: `"a".matches("` + strings.Repeat("a", celengine.MaxRegexPatternBytes+1) + `")`},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				start := time.Now()
				_, err := eng.Compile(tc.expr, boolKind)
				elapsed := time.Since(start)
				if err == nil {
					t.Fatalf("Compile(%s) succeeded, want CategorySize CompileError", tc.name)
				}
				var cerr *celengine.CompileError
				if !errors.As(err, &cerr) {
					t.Fatalf("Compile(%s) err = %T (%v), want *CompileError", tc.name, err, err)
				}
				if cerr.Category != celengine.CategorySize {
					t.Errorf("Category = %q, want %q (err=%v)", cerr.Category, celengine.CategorySize, cerr)
				}
				if elapsed > scaleWallBound(50*time.Millisecond) {
					t.Errorf("Compile(%s) took %s, want <= %s (pre-scan must run before syntax.Parse)", tc.name, elapsed, scaleWallBound(50*time.Millisecond))
				}
			})
		}
	})

	// 3. Cached evaluation of allowed patterns containing '{' (counted repetitions
	// and \x{..}) never re-parses: cost is identical across evaluations and fast.
	t.Run("cached_literal_with_braces_never_reparses_and_cost_is_stable", func(t *testing.T) {
		scope := celengine.CacheScope{Consumer: "test", ID: "brace-rule", Revision: 1}
		expr := `meta["sha"].matches("^[0-9a-fA-F]{64}$") && meta["tag"].matches("(?i)^[\\x{41}-\\x{5a}]{4}-[0-9]{2}$")`
		act := map[string]any{
			"meta": map[string]string{
				"sha": strings.Repeat("deadbeef", 8),
				"tag": "AbCd-42",
			},
		}

		prog1, err := eng.Cached(ctx, scope, expr, boolKind)
		if err != nil {
			t.Fatalf("Cached miss: %v", err)
		}
		res1, stats1, err := prog1.Eval(ctx, act)
		if err != nil {
			t.Fatalf("first Eval: %v", err)
		}
		if b, _ := res1.Bool(); !b {
			t.Fatalf("expected true match on first Eval")
		}

		start := time.Now()
		for i := 0; i < 100; i++ {
			progHit, err := eng.Cached(ctx, scope, expr, boolKind)
			if err != nil {
				t.Fatalf("Cached hit %d: %v", i, err)
			}
			res, stats, err := progHit.Eval(ctx, act)
			if err != nil {
				t.Fatalf("Eval %d: %v", i, err)
			}
			if b, _ := res.Bool(); !b {
				t.Fatalf("Eval %d = false, want true", i)
			}
			if stats.Cost != stats1.Cost {
				t.Fatalf("Eval %d Cost = %d, want stable %d", i, stats.Cost, stats1.Cost)
			}
		}
		if elapsed := time.Since(start); elapsed > scaleWallBound(100*time.Millisecond) {
			t.Errorf("100 cached brace-pattern evaluations took %s, want <= %s", elapsed, scaleWallBound(100*time.Millisecond))
		}
	})

	// 4. Common patterns (semver 2.0, SHA-256, UUID, conventional-commit, and
	// 14-keyword (?i) alternation over 60 KiB) compile and evaluate within limits.
	t.Run("common_patterns_and_14_keyword_60k_alternation_succeed", func(t *testing.T) {
		commonCases := []struct {
			name  string
			pat   string
			valid string
			bad   string
		}{
			{
				name:  "semver_2_0",
				pat:   `^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`,
				valid: "v1.24.3-rc.1+build.20261115",
				bad:   "v01.2.3",
			},
			{
				name:  "sha256_hex",
				pat:   `^[0-9a-fA-F]{64}$`,
				valid: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				bad:   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85g",
			},
			{
				name:  "uuid_v4",
				pat:   `^(?i)[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
				valid: "550e8400-e29b-41d4-a716-446655440000",
				bad:   "550e8400-e29b-11d4-a716-446655440000",
			},
			{
				name:  "conventional_commit",
				pat:   `^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([a-zA-Z0-9_./-]+\))?!?: .+$`,
				valid: "feat(triggers/cel)!: restrict matches to literal patterns",
				bad:   "random commit message",
			},
		}

		for _, tc := range commonCases {
			t.Run(tc.name, func(t *testing.T) {
				escaped := strings.ReplaceAll(tc.pat, `\`, `\\`)
				prog, err := semEng.Compile(`meta["v"].matches("`+escaped+`")`, boolKind)
				if err != nil {
					t.Fatalf("Compile(%s): %v", tc.name, err)
				}
				res, _, err := prog.Eval(ctx, map[string]any{"meta": map[string]string{"v": tc.valid}})
				if err != nil {
					t.Fatalf("Eval valid(%s): %v", tc.name, err)
				}
				if b, _ := res.Bool(); !b {
					t.Errorf("expected %q to match %s", tc.valid, tc.name)
				}
				res, _, err = prog.Eval(ctx, map[string]any{"meta": map[string]string{"v": tc.bad}})
				if err != nil {
					t.Fatalf("Eval bad(%s): %v", tc.name, err)
				}
				if b, _ := res.Bool(); b {
					t.Errorf("expected %q NOT to match %s", tc.bad, tc.name)
				}
			})
		}

		// 14-keyword (?i) alternation over a 60 KiB body field under DefaultLimits
		// (scaled only for -race instrumentation overhead).
		ordEng := eng
		if raceEnabled {
			ordEng = semEng
		}
		kw14Expr := `event.body.matches("(?i)\\b(deploy|rollback|incident|outage|hotfix|release|canary|promote|degraded|critical|escalate|mitigate|resolved|postmortem)\\b")`
		kw14Prog, err := ordEng.Compile(kw14Expr, boolKind)
		if err != nil {
			t.Fatalf("Compile 14-keyword (?i) alternation: %v", err)
		}
		body60k := strings.Repeat("status=healthy check=green ", 2270) + "incident detected in us-central1"
		res, _, err := kw14Prog.Eval(ctx, map[string]any{"event": map[string]any{"body": body60k}})
		if err != nil {
			t.Fatalf("Eval 14-keyword (?i) alternation on 60 KiB: %v", err)
		}
		if b, _ := res.Bool(); !b {
			t.Fatalf("expected 14-keyword (?i) alternation to match 60 KiB body")
		}

		// has()-wrapped receiver and global literal forms succeed cleanly.
		hasExpr := `has(event.body) && event.body.matches("^ok$") && matches(string(event.body), "^ok$")`
		hasProg, err := semEng.Compile(hasExpr, boolKind)
		if err != nil {
			t.Fatalf("Compile has()-wrapped literal matches: %v", err)
		}
		res, _, err = hasProg.Eval(ctx, map[string]any{"event": map[string]any{"body": "ok"}})
		if err != nil {
			t.Fatalf("Eval has()-wrapped literal matches: %v", err)
		}
		if b, _ := res.Bool(); !b {
			t.Fatalf("expected has()-wrapped literal matches to evaluate true")
		}
	})
}

func TestMapStringAndBytesKeyHashingCost(t *testing.T) {
	eng := newTestEngine(t)
	semEng := newSemanticLargeInputEngine(t)
	boolKind := celengine.Kind{Name: "filter", Result: celengine.Bool()}
	ctx := context.Background()

	// 1. Direct per-call cost checks on map literal construction, map index, and `in` map
	// with a 60 KiB string key (byteScanCost(61440) = 6,144).
	s60k := strings.Repeat("k", 60*1024)
	const wantMinKeyCost uint64 = 6_000

	t.Run("per_call_map_construct_index_and_in_charge_key_bytes", func(t *testing.T) {
		act := map[string]any{
			"meta": map[string]string{s60k: "ok", "ptr": s60k},
			"event": map[string]any{
				"k":    s60k,
				"kb":   []byte(s60k),
				"dmap": map[any]any{"x": 1},
			},
		}

		for _, tc := range []struct {
			name string
			expr string
			want bool
		}{
			{name: "map_literal_construct", expr: `size({string(event.k): 1}) == 1`, want: true},
			{name: "map_index_string_key", expr: `meta[string(event.k)] == "ok"`, want: true},
			{name: "nested_attr_map_index_string_key", expr: `meta[meta["ptr"]] == "ok"`, want: true},
			{name: "in_map_string_key", expr: `string(event.k) in meta`, want: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				prog, err := semEng.Compile(tc.expr, boolKind)
				if err != nil {
					t.Fatalf("Compile(%s): %v", tc.expr, err)
				}
				res, stats, err := prog.Eval(ctx, act)
				if err != nil {
					t.Fatalf("Eval(%s): %v", tc.expr, err)
				}
				if b, _ := res.Bool(); b != tc.want {
					t.Fatalf("Eval(%s) = %v, want %v", tc.expr, b, tc.want)
				}
				if stats.Cost < wantMinKeyCost {
					t.Errorf("%s Cost = %d, want >= %d (key-length proportional)", tc.name, stats.Cost, wantMinKeyCost)
				}
			})
		}

		// Dynamic `bytes in dynMap` rejects unsupported bytes map key at runtime in CEL,
		// after boundedInCall pre-charges the bytes key length.
		t.Run("in_map_bytes_key_precharged", func(t *testing.T) {
			prog, err := semEng.Compile(`bytes(event.kb) in event.dmap`, boolKind)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			_, stats, err := prog.Eval(ctx, act)
			if err == nil {
				t.Fatal("expected ReasonRuntime for bytes map key")
			}
			if stats.Cost < wantMinKeyCost {
				t.Errorf("bytes in map pre-charge Cost = %d, want >= %d", stats.Cost, wantMinKeyCost)
			}
		})
	})

	// 2. Pass-6 Consider probe: `{big: 1}[big] == 1` inside a 1,500-iteration loop
	// with a 960 KiB `big` string MUST hit ReasonCostLimit rapidly instead of
	// returning a decision after 1,500 uncharged 1-MiB hashes.
	t.Run("pass6_consider_probe_960k_map_construct_and_index_loop_aborts", func(t *testing.T) {
		loop := make([]int64, 1500)
		for i := range loop {
			loop[i] = int64(i)
		}
		s16 := make([]string, 16)
		for i := range s16 {
			s16[i] = s60k
		}
		expr := `[event.s16.join("")].all(b, event.loop.all(i, {b: 1}[b] == 1))`
		prog, err := eng.Compile(expr, boolKind)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		start := time.Now()
		res, _, evalErr := prog.Eval(ctx, map[string]any{
			"event": map[string]any{"s16": s16, "loop": loop},
		})
		elapsed := time.Since(start)
		if res != nil {
			t.Fatalf("expected nil Result on 960 KiB map construct+index loop, got decision %v", res)
		}
		assertCostLimitOrRaceTimeout(t, evalErr)
		if elapsed > scaleWallBound(2*celengine.DefaultEvalTimeout) {
			t.Errorf("elapsed = %s, want <= %s", elapsed, scaleWallBound(2*celengine.DefaultEvalTimeout))
		}
	})
}
