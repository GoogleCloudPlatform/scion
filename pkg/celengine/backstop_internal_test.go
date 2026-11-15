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

package celengine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestEvalWallClockBackstopDeterministic exercises the post-evaluation
// `elapsed > eff.EvalTimeout` wall-clock backstop in Program.Eval on an
// evaluation whose ContextEval returns a valid boolean decision with nil error
// and live context. Removing or bypassing `if elapsed > eff.EvalTimeout` causes
// Program.Eval to return (true, nil) and fails this test deterministically.
func TestEvalWallClockBackstopDeterministic(t *testing.T) {
	env := Environment{
		Name:    "backstop_internal",
		Version: 1,
		Variables: []Variable{
			{Name: "active", Type: Bool()},
		},
		Macros: MacrosStandard,
	}
	lim := DefaultLimits()
	lim.EvalTimeout = 100 * time.Millisecond

	eng, err := New(env, lim, DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prog, err := eng.Compile(`active == true`, Kind{Name: "filter", Result: Bool()})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	origNow := nowFunc
	t.Cleanup(func() {
		nowFunc = origNow
	})

	base := time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC)
	calls := 0
	nowFunc = func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		// Simulate the final uninterruptible host call finishing 5 ms past
		// eff.EvalTimeout while ContextEval itself returned (true, nil).
		return base.Add(lim.EvalTimeout + 5*time.Millisecond)
	}

	res, stats, evalErr := prog.Eval(context.Background(), map[string]any{"active": true})
	if res != nil {
		t.Fatalf("expected nil Result when elapsed (%s) > EvalTimeout (%s), got %v", stats.Elapsed, lim.EvalTimeout, res)
	}
	var eerr *EvalError
	if !errors.As(evalErr, &eerr) {
		t.Fatalf("Eval error = %T (%v), want *EvalError", evalErr, evalErr)
	}
	if eerr.Reason != ReasonTimeout {
		t.Fatalf("EvalError.Reason = %q, want %q", eerr.Reason, ReasonTimeout)
	}
	if wantElapsed := lim.EvalTimeout + 5*time.Millisecond; stats.Elapsed != wantElapsed {
		t.Errorf("stats.Elapsed = %s, want %s", stats.Elapsed, wantElapsed)
	}
}
