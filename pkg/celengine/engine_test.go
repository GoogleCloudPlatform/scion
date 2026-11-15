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
	"context"
	"errors"
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
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("RE2 match took unexpectedly long: %v", elapsed)
	}
	if b, _ := res.Bool(); b {
		t.Errorf("expected false match")
	}

	// Dynamic invalid regex fails at Eval with ReasonRuntime.
	dynReProg, err := eng.Compile(`meta["input"].matches(meta["pattern"])`, boolKind)
	if err != nil {
		t.Fatalf("Compile dynamic matches: %v", err)
	}
	_, _, err = dynReProg.Eval(context.Background(), map[string]any{
		"meta": map[string]string{"input": "abc", "pattern": "[invalid"},
	})
	assertEvalReason(t, err, celengine.ReasonRuntime)
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
