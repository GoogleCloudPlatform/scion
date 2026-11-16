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

package systemone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// ReasonDialectDisabled is the content-free FailedError.Reason returned when a
// rule compiled while System One was enabled is evaluated after the experiment
// has been disabled (AC-13 option (b)).
const ReasonDialectDisabled = "dialect_disabled"

var _ ruleengine.Dialect = (*Dialect)(nil)

// DialectConfig configures the System One ruleengine.Dialect.
type DialectConfig struct {
	// Runtime executes compiled System One rules. Ignored when RuntimeFunc is set.
	Runtime *runtime.Runtime
	// RuntimeFunc optionally resolves the active *runtime.Runtime on each
	// evaluation (for example Hub's s.SystemOneRuntime accessor). Takes
	// precedence over Runtime when non-nil.
	RuntimeFunc func() *runtime.Runtime
	// MaxEvalTime sets Capabilities.MaxEvalTime declared to ruleengine
	// (server.hub.system_one.max_eval_time; defaults to Runtime.MaxEvalTime() or
	// runtime.DefaultMaxEvalTime = 3s, clamped to runtime.MaxAllowedEvalTime = 30s).
	MaxEvalTime time.Duration
	// Enabled gates Compile and Evaluate on feature availability before any I/O
	// (AC-13 option (b)). When nil, the dialect is treated as enabled.
	Enabled func() bool
}

// Dialect adapts System One spec compilation and bounded runtime evaluation to
// ruleengine.Dialect under the name "system_one" (runtime-design §5, P1 brief §5).
//
// Dialect is registered unconditionally at Hub construction so feature gating
// can toggle without a Hub restart (AC-13 option (b)):
//   - Engine.Supports(consumer, "system_one") reports registration and consumer
//     allow-list membership (with AllowIO) only and stays true when Enabled
//     returns false; authoring callers must check availability via Engine.Compile.
//   - When Enabled returns false, Compile returns
//     *ruleengine.UnsupportedDialectError{Dialect: "system_one", Consumer: consumer,
//     Reason: ruleengine.ReasonUnknownDialect}, and Evaluate returns
//     *ruleengine.FailedError{Reason: "dialect_disabled", Cause:
//     *ruleengine.UnsupportedDialectError{Dialect: "system_one", Consumer: consumer,
//     Reason: ruleengine.ReasonUnknownDialect}} without making any provider call.
type Dialect struct {
	runtime     *runtime.Runtime
	runtimeFunc func() *runtime.Runtime
	maxEvalTime time.Duration
	enabled     func() bool
}

type compiledRule struct {
	compiled *spec.Compiled
	consumer string
}

// NewDialect constructs a System One ruleengine.Dialect from cfg.
func NewDialect(cfg DialectConfig) *Dialect {
	maxEval := runtime.DefaultMaxEvalTime
	if cfg.MaxEvalTime > 0 {
		maxEval = cfg.MaxEvalTime
		if maxEval > runtime.MaxAllowedEvalTime {
			maxEval = runtime.MaxAllowedEvalTime
		}
	} else if cfg.Runtime != nil {
		maxEval = cfg.Runtime.MaxEvalTime()
	}
	return &Dialect{
		runtime:     cfg.Runtime,
		runtimeFunc: cfg.RuntimeFunc,
		maxEvalTime: maxEval,
		enabled:     cfg.Enabled,
	}
}

// Name returns DialectName ("system_one").
func (d *Dialect) Name() string { return DialectName }

// Declares returns static capabilities for the "system_one" dialect.
func (d *Dialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: d.maxEvalTime,
		ResultKinds: []ruleengine.Kind{
			ruleengine.KindFilter,
			ruleengine.Kind(spec.KindRoute),
			ruleengine.Kind(spec.KindJudgmentBool),
			ruleengine.Kind(spec.KindJudgmentChoice),
			ruleengine.Kind(spec.KindJudgments),
		},
	}
}

// Compile validates and compiles raw JSON rule text via pkg/systemone/spec.
// EnvironmentRef.Name selects the content environment ("probe" only in P1);
// EnvironmentRef.ResultType is ignored.
func (d *Dialect) Compile(ctx context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	if !d.isEnabled() {
		return ruleengine.Compiled{}, &ruleengine.UnsupportedDialectError{
			Dialect:  DialectName,
			Consumer: strings.TrimSpace(env.Consumer),
			Reason:   ruleengine.ReasonUnknownDialect,
		}
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return ruleengine.Compiled{}, err
		}
	}
	if env.MaxRuleBytes > 0 && len(rule) > env.MaxRuleBytes {
		return ruleengine.Compiled{}, &spec.CompileError{
			Category: spec.CategoryLimit,
			Message:  fmt.Sprintf("rule length %d exceeds maximum %d bytes", len(rule), env.MaxRuleBytes),
		}
	}

	envName := strings.TrimSpace(env.Name)
	if envName != spec.ProbeEnvironmentName {
		return ruleengine.Compiled{}, &spec.CompileError{
			Path:     "environment",
			Category: spec.CategoryInput,
			Message:  fmt.Sprintf("unsupported content environment %q (only %q is supported in P1)", envName, spec.ProbeEnvironmentName),
		}
	}

	compiled, err := spec.Compile([]byte(rule), spec.CompileOptions{
		Kind: spec.Kind(kind),
	})
	if err != nil {
		return ruleengine.Compiled{}, err
	}

	consumer := strings.TrimSpace(env.Consumer)
	return ruleengine.NewCompiled(consumer, DialectName, kind, &compiledRule{
		compiled: compiled,
		consumer: consumer,
	}), nil
}

// Evaluate executes a compiled System One rule against a typed systemone.Input
// read from ruleengine.Input.Payload. Budget.CostLimit is ignored;
// Budget.MaxEvalTime is forwarded to Runtime with the engine deadline margin.
func (d *Dialect) Evaluate(ctx context.Context, c ruleengine.Compiled, input ruleengine.Input, budget ruleengine.Budget) (ruleengine.Decision, error) {
	if !d.isEnabled() {
		return ruleengine.Decision{}, &ruleengine.FailedError{
			Reason: ReasonDialectDisabled,
			Cause: &ruleengine.UnsupportedDialectError{
				Dialect:  DialectName,
				Consumer: c.Consumer(),
				Reason:   ruleengine.ReasonUnknownDialect,
			},
		}
	}

	start := time.Now()
	prog, ok := c.Rep().(*compiledRule)
	if !ok || prog == nil || prog.compiled == nil {
		return failedInternalDecision(nil, c.Kind(), start)
	}

	soInput, ok := input.Payload.(Input)
	if !ok {
		return failedInternalDecision(prog, c.Kind(), start)
	}

	rt := d.resolveRuntime()
	if rt == nil {
		return failedInternalDecision(prog, c.Kind(), start)
	}

	dec, err := rt.Evaluate(ctx, prog.compiled, soInput, runtime.EvalOptions{
		Consumer:          prog.consumer,
		EngineMaxEvalTime: budget.MaxEvalTime,
	})
	if err == nil {
		return ruleengine.Decision{
			Kind:   c.Kind(),
			Bool:   dec.Bool,
			String: dec.String,
			Detail: dec.Eval,
		}, nil
	}

	eval, hasEval := EvaluationFrom(err)
	if !hasEval || eval == nil {
		return failedInternalDecision(prog, c.Kind(), start)
	}
	evalErr := &EvaluationError{Eval: eval}

	if eval.Class == ClassUncertain {
		return ruleengine.Decision{
				Kind:   c.Kind(),
				Detail: eval,
			}, &ruleengine.UncertainError{
				Reason: eval.Reason,
				Cause:  evalErr,
			}
	}

	return ruleengine.Decision{
			Kind:   c.Kind(),
			Detail: eval,
		}, &ruleengine.FailedError{
			Reason: eval.Reason,
			Cause:  evalErr,
		}
}

func (d *Dialect) isEnabled() bool {
	if d == nil || d.enabled == nil {
		return true
	}
	return d.enabled()
}

func (d *Dialect) resolveRuntime() *runtime.Runtime {
	if d == nil {
		return nil
	}
	if d.runtimeFunc != nil {
		return d.runtimeFunc()
	}
	return d.runtime
}

func failedInternalDecision(prog *compiledRule, kind ruleengine.Kind, start time.Time) (ruleengine.Decision, error) {
	eval := &Evaluation{
		EvalID:  newDialectEvalID(),
		Rule:    RuleRef{Dialect: DialectName},
		Class:   ClassFailed,
		Reason:  string(provider.FailInternal),
		Elapsed: time.Since(start),
	}
	if prog != nil && prog.compiled != nil {
		eval.Rule.Consumer = prog.consumer
		eval.Rule.ContentHash = prog.compiled.ContentHash
		eval.Rule.Native = prog.compiled.Spec.Form == spec.FormNative
		eval.Requested = prog.compiled.Spec.Binding
	}
	evalErr := &EvaluationError{Eval: eval}
	return ruleengine.Decision{
			Kind:   kind,
			Detail: eval,
		}, &ruleengine.FailedError{
			Reason: string(provider.FailInternal),
			Cause:  evalErr,
		}
}

func newDialectEvalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "eval-fallback"
	}
	return "eval-" + hex.EncodeToString(b[:])
}
