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
	// (AC-13 option (b)). When nil, the dialect fails closed (disabled).
	Enabled func() bool
	// Environments optionally supplies a shared ContentEnvironment registry.
	// When nil, NewDialect creates a fresh spec.NewEnvironmentRegistry() with
	// the built-in "probe" environment pre-registered.
	Environments *spec.EnvironmentRegistry
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
	runtime      *runtime.Runtime
	runtimeFunc  func() *runtime.Runtime
	maxEvalTime  time.Duration
	enabled      func() bool
	environments *spec.EnvironmentRegistry
}

type compiledRule struct {
	compiled *spec.Compiled
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
	envs := cfg.Environments
	if envs == nil {
		envs = spec.NewEnvironmentRegistry()
	}
	return &Dialect{
		runtime:      cfg.Runtime,
		runtimeFunc:  cfg.RuntimeFunc,
		maxEvalTime:  maxEval,
		enabled:      cfg.Enabled,
		environments: envs,
	}
}

// Environment returns a defensive copy of the registered ContentEnvironment
// named name.
func (d *Dialect) Environment(name string) (spec.ContentEnvironment, bool) {
	if d == nil || d.environments == nil {
		return spec.ContentEnvironment{}, false
	}
	return d.environments.Get(name)
}

// Environments returns d's ContentEnvironment registry.
func (d *Dialect) Environments() *spec.EnvironmentRegistry {
	if d == nil {
		return nil
	}
	return d.environments
}

// RegisterConsumerEnvironment binds env onto the "system_one" Dialect
// registered in eng after verifying that consumer holds an active system_one
// grant (including AllowIO) via eng.DialectFor (runtime-design §4.4).
func RegisterConsumerEnvironment(eng *ruleengine.Engine, consumer string, env spec.ContentEnvironment) error {
	if eng == nil {
		return fmt.Errorf("system_one: cannot register environment on nil ruleengine.Engine")
	}
	d, err := eng.DialectFor(consumer, DialectName)
	if err != nil {
		return err
	}
	soDialect, ok := d.(*Dialect)
	if !ok {
		return fmt.Errorf("system_one: registered dialect %q has unexpected type %T", DialectName, d)
	}
	if soDialect == nil || soDialect.environments == nil {
		return &spec.CompileError{
			Path:     "environment",
			Category: spec.CategoryInput,
			Message:  "system_one dialect environment registry is not initialized",
		}
	}
	return soDialect.environments.RegisterForConsumer(consumer, env)
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
// EnvironmentRef.Name selects the registered content environment (owned by
// EnvironmentRef.Consumer).
//
// Caller obligation: consumer authoring paths that compile rules in a project
// context MUST attach the project ID to ctx via WithProjectID(ctx, projectID)
// so authoring-time allowlist validation runs against that project's allowlist.
// Evaluation independently enforces RequireProject: true regardless of compile
// context.
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
	contentEnv, ok := d.Environment(envName)
	if !ok {
		return ruleengine.Compiled{}, &spec.CompileError{
			Path:     "environment",
			Category: spec.CategoryInput,
			Message:  fmt.Sprintf("unknown content environment %q", envName),
		}
	}
	consumer := strings.TrimSpace(env.Consumer)
	if owner, hasOwner := d.environments.Owner(envName); hasOwner && owner != consumer {
		return ruleengine.Compiled{}, &spec.CompileError{
			Path:     "environment",
			Category: spec.CategoryInput,
			Message:  fmt.Sprintf("content environment %q is owned by consumer %q, not %q", envName, owner, consumer),
		}
	}

	compiled, err := spec.Compile([]byte(rule), spec.CompileOptions{
		Kind:        spec.Kind(kind),
		Environment: &contentEnv,
	})
	if err != nil {
		return ruleengine.Compiled{}, err
	}

	if rt := d.resolveRuntime(); rt != nil {
		if err := rt.ValidateCompiled(ctx, compiled); err != nil {
			return ruleengine.Compiled{}, err
		}
	}

	return ruleengine.NewCompiled(strings.TrimSpace(env.Consumer), DialectName, kind, &compiledRule{
		compiled: compiled,
	}), nil
}

// Evaluate executes a compiled System One rule against a typed systemone.Input
// or systemone.Request read from ruleengine.Input.Payload. Budget.CostLimit is
// ignored; Budget.MaxEvalTime is forwarded to Runtime alongside
// ruleengine.EngineDeadlineMargin.
//
// When Payload is a systemone.Request, its consumer-authorized ProjectID,
// RuleID, Revision, and optional ElementIndex are forwarded into
// runtime.EvalOptions (and ProjectID is attached to the request context via
// runtime.WithEvalProjectID) so Evaluation.ProjectID, Evaluation.Rule, and
// project-scoped named secrets resolve cleanly on the dialect path.
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
	consumer := c.Consumer()
	prog, ok := c.Rep().(*compiledRule)
	if !ok || prog == nil || prog.compiled == nil {
		return failedInternalDecision(nil, consumer, c.Kind(), start, evalContextMeta{})
	}

	var (
		soInput Input
		meta    evalContextMeta
	)
	switch p := input.Payload.(type) {
	case Input:
		soInput = p
	case Request:
		soInput = p.Input
		meta = evalContextMeta{
			projectID: strings.TrimSpace(p.ProjectID),
			ruleID:    strings.TrimSpace(p.RuleID),
			revision:  p.Revision,
		}
		if p.ElementIndex != nil {
			idx := *p.ElementIndex
			meta.elementIndex = &idx
		}
	default:
		return failedInternalDecision(prog, consumer, c.Kind(), start, evalContextMeta{})
	}

	rt := d.resolveRuntime()
	if rt == nil {
		return failedInternalDecision(prog, consumer, c.Kind(), start, meta)
	}

	evalCtx := ctx
	if meta.projectID != "" {
		evalCtx = runtime.WithEvalProjectID(ctx, meta.projectID)
	}
	// Every dialect evaluation is a consumer path and requires an authorized
	// ProjectID for D1 per-project allowlist enforcement (fail-closed when
	// ProjectID is missing or empty, regardless of Input vs Request or
	// environment name). Admin probes invoke Runtime.Evaluate directly.
	dec, err := rt.Evaluate(evalCtx, prog.compiled, soInput, runtime.EvalOptions{
		Consumer:             consumer,
		RuleID:               meta.ruleID,
		Revision:             meta.revision,
		ElementIndex:         meta.elementIndex,
		ProjectID:            meta.projectID,
		RequireProject:       true,
		EngineMaxEvalTime:    budget.MaxEvalTime,
		EngineDeadlineMargin: ruleengine.EngineDeadlineMargin(budget.MaxEvalTime),
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
		return failedInternalDecision(prog, consumer, c.Kind(), start, meta)
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
		return false
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

type evalContextMeta struct {
	projectID    string
	ruleID       string
	revision     int64
	elementIndex *int
}

func failedInternalDecision(prog *compiledRule, consumer string, kind ruleengine.Kind, start time.Time, meta evalContextMeta) (ruleengine.Decision, error) {
	var elemIdx *int
	if meta.elementIndex != nil {
		v := *meta.elementIndex
		elemIdx = &v
	}
	eval := &Evaluation{
		EvalID: newDialectEvalID(),
		Rule: RuleRef{
			Dialect:      DialectName,
			Consumer:     consumer,
			RuleID:       meta.ruleID,
			Revision:     meta.revision,
			ElementIndex: elemIdx,
		},
		ProjectID: meta.projectID,
		Class:     ClassFailed,
		Reason:    string(provider.FailInternal),
		Elapsed:   time.Since(start),
	}
	if prog != nil && prog.compiled != nil {
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
