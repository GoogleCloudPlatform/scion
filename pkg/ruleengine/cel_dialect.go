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

package ruleengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
)

// CELDialect wraps one or more consumer-scoped *celengine.Engine instances as
// the pure, no-I/O "cel" Dialect in a ruleengine.Engine.
type CELDialect struct {
	mu      sync.RWMutex
	engines map[string]*celengine.Engine
	maxTime time.Duration
}

// NewCELDialect creates a CELDialect and registers each provided *celengine.Engine
// under its Environment().Name.
func NewCELDialect(engines ...*celengine.Engine) (*CELDialect, error) {
	d := &CELDialect{
		engines: make(map[string]*celengine.Engine, len(engines)),
	}
	for _, eng := range engines {
		if err := d.RegisterEnvironment(eng); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// RegisterEnvironment binds a *celengine.Engine by its Environment().Name.
func (d *CELDialect) RegisterEnvironment(eng *celengine.Engine) error {
	if eng == nil {
		return errors.New("ruleengine: nil celengine.Engine")
	}
	name := eng.Environment().Name
	if name == "" {
		return errors.New("ruleengine: celengine.Engine has empty Environment.Name")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.engines[name]; exists {
		return fmt.Errorf("ruleengine: CEL environment %q is already registered", name)
	}
	d.engines[name] = eng
	if t := eng.Limits().EvalTimeout; t > d.maxTime {
		d.maxTime = t
	}
	return nil
}

// CELEngine returns the *celengine.Engine registered for envName, if present.
func (d *CELDialect) CELEngine(envName string) (*celengine.Engine, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	eng, ok := d.engines[envName]
	return eng, ok
}

// Name returns DialectCEL ("cel").
func (d *CELDialect) Name() string { return DialectCEL }

// Declares returns CELDialect's static capabilities (PerformsIO: false).
func (d *CELDialect) Declares() Capabilities {
	d.mu.RLock()
	maxTime := d.maxTime
	d.mu.RUnlock()
	if maxTime == 0 {
		maxTime = celengine.DefaultEvalTimeout
	}
	return Capabilities{
		PerformsIO:  false,
		MaxEvalTime: maxTime,
		ResultKinds: []Kind{KindFilter, KindSubject},
	}
}

func (d *CELDialect) lookupEngine(env EnvironmentRef) (*celengine.Engine, []celengine.Option, error) {
	envName := env.Name
	if envName == "" {
		envName = env.Consumer
	}
	d.mu.RLock()
	eng, ok := d.engines[envName]
	d.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("ruleengine: unknown CEL environment %q", envName)
	}

	var opts []celengine.Option
	if env.MaxRuleBytes > 0 {
		opts = append(opts, celengine.WithMaxExprBytes(env.MaxRuleBytes))
	}
	return eng, opts, nil
}

func celKindFor(kind Kind, env EnvironmentRef) (celengine.Kind, error) {
	if env.ResultType.Valid() {
		return celengine.Kind{Name: string(kind), Result: env.ResultType}, nil
	}
	switch kind {
	case KindFilter:
		return celengine.Kind{Name: string(kind), Result: celengine.Bool()}, nil
	case KindSubject:
		return celengine.Kind{Name: string(kind), Result: celengine.String()}, nil
	default:
		return celengine.Kind{}, fmt.Errorf("ruleengine: unsupported kind %q without explicit ResultType", kind)
	}
}

// Compile validates and compiles rule without populating the runtime LRU cache.
func (d *CELDialect) Compile(ctx context.Context, rule string, kind Kind, env EnvironmentRef) (Compiled, error) {
	if ctx != nil && ctx.Err() != nil {
		return Compiled{}, ctx.Err()
	}
	eng, opts, err := d.lookupEngine(env)
	if err != nil {
		return Compiled{}, err
	}
	celKind, err := celKindFor(kind, env)
	if err != nil {
		return Compiled{}, err
	}
	prog, err := eng.CompileWithOptions(rule, celKind, opts...)
	if err != nil {
		return Compiled{}, err
	}
	return NewCompiled(env.Consumer, DialectCEL, kind, prog), nil
}

// CachedCompile retrieves or compiles rule in the environment's content-addressed LRU cache.
func (d *CELDialect) CachedCompile(ctx context.Context, scope celengine.CacheScope, rule string, kind Kind, env EnvironmentRef) (Compiled, error) {
	eng, opts, err := d.lookupEngine(env)
	if err != nil {
		return Compiled{}, err
	}
	celKind, err := celKindFor(kind, env)
	if err != nil {
		return Compiled{}, err
	}
	prog, err := eng.Cached(ctx, scope, rule, celKind, opts...)
	if err != nil {
		return Compiled{}, err
	}
	return NewCompiled(env.Consumer, DialectCEL, kind, prog), nil
}

// Invalidate removes scope bindings in the named CEL environment's program cache.
func (d *CELDialect) Invalidate(envName string, scope celengine.CacheScope) {
	if eng, ok := d.CELEngine(envName); ok {
		eng.Invalidate(scope)
	}
}

// Evaluate runs a compiled CEL program and projects its Result into a Decision.
func (d *CELDialect) Evaluate(ctx context.Context, c Compiled, input Input, budget Budget) (Decision, error) {
	prog, ok := c.Rep().(celengine.Program)
	if !ok || prog.IsZero() {
		return Decision{}, &FailedError{Reason: "invalid_compiled_program"}
	}

	var overrides []celengine.Limits
	if budget.MaxEvalTime > 0 || budget.CostLimit > 0 {
		overrides = append(overrides, celengine.Limits{
			EvalTimeout: budget.MaxEvalTime,
			CostLimit:   budget.CostLimit,
		})
	}

	res, stats, err := prog.Eval(ctx, input.Activation, overrides...)
	if err != nil {
		var evalErr *celengine.EvalError
		if errors.As(err, &evalErr) {
			return Decision{}, &FailedError{
				Reason: evalErr.Reason,
				Cause:  evalErr,
			}
		}
		return Decision{}, &FailedError{
			Reason: celengine.ReasonRuntime,
			Cause:  err,
		}
	}

	dec := Decision{
		Kind:   c.Kind(),
		Detail: stats,
	}
	if b, bErr := res.Bool(); bErr == nil {
		dec.Bool = b
		return dec, nil
	}
	if s, sErr := res.String(); sErr == nil {
		dec.String = s
		return dec, nil
	}
	return Decision{}, &FailedError{Reason: celengine.ReasonType}
}
