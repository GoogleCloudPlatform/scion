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

// Package ruleengine provides a thin dialect registry, consumer allow-list,
// budget accounting, and common decision/error types above concrete rule
// evaluators such as pkg/celengine.
package ruleengine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
)

// DialectCEL is the built-in pure expression dialect name.
const DialectCEL = "cel"

// Kind identifies a rule projection slot.
type Kind string

const (
	// KindFilter projects a boolean match decision.
	KindFilter Kind = "filter"
	// KindSubject projects a string subject/coalescing key decision.
	KindSubject Kind = "subject"
)

// Capabilities describes static properties declared by a Dialect.
type Capabilities struct {
	PerformsIO  bool
	MaxEvalTime time.Duration
	ResultKinds []Kind
}

// EnvironmentRef identifies the consumer and schema environment a rule compiles against.
type EnvironmentRef struct {
	Consumer     string
	Name         string
	MaxRuleBytes int
	ResultType   celengine.Type
}

// Input supplies evaluation data to a Dialect.
type Input struct {
	Activation map[string]any
}

// ErrBudgetExhausted is returned when an aggregate BudgetTracker has no remaining time.
var ErrBudgetExhausted = errors.New("rule budget exhausted")

// BudgetTracker tracks aggregate evaluation wall-clock time across multiple
// candidate rules (for example the 1s per-event trigger budget).
type BudgetTracker struct {
	mu    sync.Mutex
	limit time.Duration
	spent time.Duration
}

// NewBudgetTracker creates a BudgetTracker with the given aggregate limit.
func NewBudgetTracker(limit time.Duration) *BudgetTracker {
	return &BudgetTracker{limit: limit}
}

// Remaining returns the remaining unspent duration, or 0 if exhausted.
func (b *BudgetTracker) Remaining() time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spent >= b.limit {
		return 0
	}
	return b.limit - b.spent
}

// Exhausted reports whether spent >= limit.
func (b *BudgetTracker) Exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent >= b.limit
}

// Charge records d against the aggregate budget.
func (b *BudgetTracker) Charge(d time.Duration) {
	if b == nil || d <= 0 {
		return
	}
	b.mu.Lock()
	b.spent += d
	b.mu.Unlock()
}

// Spent returns total duration charged so far.
func (b *BudgetTracker) Spent() time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// Budget bounds a single Evaluate call and optionally charges an aggregate BudgetTracker.
type Budget struct {
	MaxEvalTime time.Duration
	CostLimit   uint64
	Tracker     *BudgetTracker
}

// Decision carries a dialect's typed result beneath the Bool/String consumer projections.
type Decision struct {
	Kind   Kind
	Bool   bool
	String string
	Detail any
	Trace  string // content-free
}

// UncertainError indicates an unknown or uncertain judgment (distinct from
// provider/runtime failure). It never becomes a match.
type UncertainError struct {
	Reason string
	Cause  error
}

func (e *UncertainError) Error() string {
	if e.Reason == "" {
		return "rule uncertain"
	}
	return "rule uncertain: " + e.Reason
}

func (e *UncertainError) Unwrap() error { return e.Cause }

// FailedError indicates a runtime, evaluation, or provider failure. It never
// becomes a match.
type FailedError struct {
	Reason string
	Cause  error
}

func (e *FailedError) Error() string {
	if e.Reason == "" {
		return "rule evaluation failed"
	}
	return "rule evaluation failed: " + e.Reason
}

func (e *FailedError) Unwrap() error { return e.Cause }

// UnsupportedDialectError is returned when a dialect is unknown or disallowed
// for the requesting consumer.
type UnsupportedDialectError struct {
	Dialect  string
	Consumer string
}

func (e *UnsupportedDialectError) Error() string {
	if e.Consumer != "" {
		return fmt.Sprintf("unsupported dialect %q for consumer %q", e.Dialect, e.Consumer)
	}
	return fmt.Sprintf("unsupported dialect %q", e.Dialect)
}

// Compiled wraps a dialect-owned compiled rule representation.
type Compiled struct {
	dialect string
	kind    Kind
	rep     any
}

// NewCompiled constructs a Compiled handle for Dialect implementations.
func NewCompiled(dialect string, kind Kind, rep any) Compiled {
	return Compiled{dialect: dialect, kind: kind, rep: rep}
}

// Dialect returns the dialect name that produced c.
func (c Compiled) Dialect() string { return c.dialect }

// Kind returns the Kind c was compiled for.
func (c Compiled) Kind() Kind { return c.kind }

// Rep returns the dialect's internal compiled representation.
func (c Compiled) Rep() any { return c.rep }

// IsZero reports whether c is uninitialized.
func (c Compiled) IsZero() bool { return c.rep == nil }

// Dialect is implemented by concrete rule evaluators registered on an Engine.
type Dialect interface {
	Name() string
	Compile(ctx context.Context, rule string, kind Kind, env EnvironmentRef) (Compiled, error)
	Evaluate(ctx context.Context, c Compiled, input Input, budget Budget) (Decision, error)
	Declares() Capabilities
}

// CachedDialect is an optional extension implemented by dialects with a
// content-addressed program cache (such as CELDialect).
type CachedDialect interface {
	Dialect
	CachedCompile(ctx context.Context, scope celengine.CacheScope, rule string, kind Kind, env EnvironmentRef) (Compiled, error)
	Invalidate(envName string, scope celengine.CacheScope)
}

// Engine dispatches rule compilation and evaluation by dialect, enforces
// per-consumer dialect allow-lists, and bounds evaluation time and budgets.
type Engine struct {
	mu       sync.RWMutex
	dialects map[string]Dialect
	allowed  map[string]map[string]bool // consumer -> set of allowed dialects; absent = any registered
}

// NewEngine creates an empty rule Engine registry.
func NewEngine() *Engine {
	return &Engine{
		dialects: make(map[string]Dialect),
		allowed:  make(map[string]map[string]bool),
	}
}

// Register adds d to the Engine. Registering nil, an empty dialect name, or a
// duplicate dialect name returns an error.
func (e *Engine) Register(d Dialect) error {
	if d == nil {
		return errors.New("ruleengine: cannot register nil Dialect")
	}
	name := strings.TrimSpace(d.Name())
	if name == "" {
		return errors.New("ruleengine: Dialect.Name must not be empty")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.dialects[name]; exists {
		return fmt.Errorf("ruleengine: dialect %q is already registered", name)
	}
	e.dialects[name] = d
	return nil
}

// Dialect returns the registered Dialect with name, if present.
func (e *Engine) Dialect(name string) (Dialect, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d, ok := e.dialects[normalizeDialect(name)]
	return d, ok
}

// Dialects returns the sorted names of all registered dialects.
func (e *Engine) Dialects() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.dialects))
	for name := range e.dialects {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// AllowConsumerDialects restricts consumer to the given dialect names.
func (e *Engine) AllowConsumerDialects(consumer string, dialects ...string) {
	if consumer == "" {
		return
	}
	set := make(map[string]bool, len(dialects))
	for _, d := range dialects {
		if n := strings.TrimSpace(d); n != "" {
			set[n] = true
		}
	}
	e.mu.Lock()
	e.allowed[consumer] = set
	e.mu.Unlock()
}

// Supports reports whether dialect is registered and permitted for consumer.
func (e *Engine) Supports(consumer, dialect string) bool {
	_, err := e.resolveDialect(consumer, dialect)
	return err == nil
}

func normalizeDialect(dialect string) string {
	d := strings.TrimSpace(dialect)
	if d == "" {
		return DialectCEL
	}
	return d
}

func (e *Engine) resolveDialect(consumer, dialect string) (Dialect, error) {
	name := normalizeDialect(dialect)
	e.mu.RLock()
	defer e.mu.RUnlock()
	if allowSet, restricted := e.allowed[consumer]; restricted && !allowSet[name] {
		return nil, &UnsupportedDialectError{Dialect: name, Consumer: consumer}
	}
	d, ok := e.dialects[name]
	if !ok {
		return nil, &UnsupportedDialectError{Dialect: name, Consumer: consumer}
	}
	return d, nil
}

// Compile validates and compiles rule for authoring-time checks. An empty
// dialect defaults to "cel".
func (e *Engine) Compile(ctx context.Context, dialect, rule string, kind Kind, env EnvironmentRef) (Compiled, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d, err := e.resolveDialect(env.Consumer, dialect)
	if err != nil {
		return Compiled{}, err
	}
	return d.Compile(ctx, rule, kind, env)
}

// CachedCompile retrieves or compiles rule using the dialect's program cache
// when supported, falling back to Compile otherwise.
func (e *Engine) CachedCompile(ctx context.Context, dialect string, scope celengine.CacheScope, rule string, kind Kind, env EnvironmentRef) (Compiled, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d, err := e.resolveDialect(env.Consumer, dialect)
	if err != nil {
		return Compiled{}, err
	}
	if cd, ok := d.(CachedDialect); ok {
		return cd.CachedCompile(ctx, scope, rule, kind, env)
	}
	return d.Compile(ctx, rule, kind, env)
}

// Invalidate forwards scoped cache invalidation to a CachedDialect if registered.
func (e *Engine) Invalidate(dialect, envName string, scope celengine.CacheScope) {
	d, ok := e.Dialect(dialect)
	if !ok {
		return
	}
	if cd, ok := d.(CachedDialect); ok {
		cd.Invalidate(envName, scope)
	}
}

var (
	errDialectEvalTimeout = errors.New("ruleengine: dialect eval timeout exceeded")
	errTrackerTimeout     = errors.New("ruleengine: aggregate budget tracker exhausted")
)

// Evaluate executes a compiled rule under the dialect's declared MaxEvalTime
// and the caller's Budget. Non-nil errors always return a zero Decision.
func (e *Engine) Evaluate(ctx context.Context, c Compiled, input Input, budget Budget) (Decision, error) {
	if c.IsZero() {
		return Decision{}, &FailedError{Reason: "uninitialized_program"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, &FailedError{Reason: celengine.ReasonCanceled, Cause: err}
	}
	if budget.Tracker != nil && budget.Tracker.Exhausted() {
		return Decision{}, ErrBudgetExhausted
	}

	e.mu.RLock()
	d, ok := e.dialects[c.Dialect()]
	e.mu.RUnlock()
	if !ok {
		return Decision{}, &UnsupportedDialectError{Dialect: c.Dialect()}
	}

	evalTimeout := d.Declares().MaxEvalTime
	boundFromTracker := false
	if budget.MaxEvalTime > 0 && (evalTimeout == 0 || budget.MaxEvalTime < evalTimeout) {
		evalTimeout = budget.MaxEvalTime
	}
	if budget.Tracker != nil {
		rem := budget.Tracker.Remaining()
		if rem <= 0 {
			return Decision{}, ErrBudgetExhausted
		}
		if evalTimeout == 0 || rem < evalTimeout {
			evalTimeout = rem
			boundFromTracker = true
		}
	}

	effBudget := budget
	if evalTimeout > 0 {
		effBudget.MaxEvalTime = evalTimeout
	}

	evalCtx := ctx
	var cancel context.CancelFunc
	if evalTimeout > 0 {
		cause := errDialectEvalTimeout
		if boundFromTracker {
			cause = errTrackerTimeout
		}
		evalCtx, cancel = context.WithTimeoutCause(ctx, evalTimeout, cause)
		defer cancel()
	}

	start := time.Now()
	dec, err := d.Evaluate(evalCtx, c, input, effBudget)
	elapsed := time.Since(start)
	if budget.Tracker != nil {
		budget.Tracker.Charge(elapsed)
	}
	if err != nil {
		if callerErr := ctx.Err(); callerErr != nil {
			return Decision{}, &FailedError{Reason: celengine.ReasonCanceled, Cause: callerErr}
		}
		if boundFromTracker && (isTimeoutFailure(err) || (errors.Is(context.Cause(evalCtx), errTrackerTimeout) && isTimeoutOrCancelFailure(err))) {
			return Decision{}, ErrBudgetExhausted
		}
		if errors.Is(context.Cause(evalCtx), errDialectEvalTimeout) && isTimeoutOrCancelFailure(err) {
			return Decision{}, &FailedError{
				Reason: celengine.ReasonTimeout,
				Cause:  &celengine.EvalError{Reason: celengine.ReasonTimeout},
			}
		}
		return Decision{}, err
	}
	return dec, nil
}

func isTimeoutFailure(err error) bool {
	var failedErr *FailedError
	if errors.As(err, &failedErr) && failedErr.Reason == celengine.ReasonTimeout {
		return true
	}
	var evalErr *celengine.EvalError
	return errors.As(err, &evalErr) && evalErr.Reason == celengine.ReasonTimeout
}

func isTimeoutOrCancelFailure(err error) bool {
	if isTimeoutFailure(err) {
		return true
	}
	var failedErr *FailedError
	if errors.As(err, &failedErr) && failedErr.Reason == celengine.ReasonCanceled {
		return true
	}
	var evalErr *celengine.EvalError
	if errors.As(err, &evalErr) && evalErr.Reason == celengine.ReasonCanceled {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
