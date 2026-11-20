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
// evaluators such as pkg/celengine. Its direct use of celengine value types
// (EnvironmentRef.ResultType and celengine.CacheScope) is deliberate for
// Stage 1 while CEL is the sole concrete dialect.
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

// Input supplies evaluation data to a Dialect. Activation carries variable
// bindings for expression dialects such as CEL; Payload carries optional typed
// dialect input alongside Activation (ignored by CELDialect).
type Input struct {
	Activation map[string]any
	Payload    any
}

// ErrBudgetExhausted is returned when an aggregate BudgetTracker has no remaining time.
var ErrBudgetExhausted = errors.New("rule budget exhausted")

// maxEngineDeadlineMargin is the upper bound on the safety margin that
// dialects such as System One subtract from Budget.MaxEvalTime so their own
// internal deadline fires slightly before the engine's context deadline.
// Keep in sync with pkg/systemone/runtime.engineDeadlineWithMargin (unification
// tracked under ptone/scion#8033).
const maxEngineDeadlineMargin = 10 * time.Millisecond

// engineDeadlineMargin returns the deadline safety margin min(10ms, d/20) for
// an evaluation timeout d (0 when d <= 0). Keep in sync with
// pkg/systemone/runtime.engineDeadlineWithMargin (ptone/scion#8033).
func engineDeadlineMargin(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	margin := min(maxEngineDeadlineMargin, d/20)
	if margin <= 0 {
		return 1
	}
	return margin
}

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

// Exhaust marks the tracker as having spent at least its full limit so
// Exhausted() is true and Remaining() is 0. It never reduces Spent() if
// previous charges already exceeded the limit.
func (b *BudgetTracker) Exhaust() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.spent < b.limit {
		b.spent = b.limit
	}
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

// ErrEmptyConsumer is returned by AllowConsumerDialects when consumer is empty or whitespace.
var ErrEmptyConsumer = errors.New("ruleengine: empty consumer")

// DialectGrant authorizes a consumer to compile and evaluate rules in Dialect.
// AllowIO is required when the Dialect's Capabilities.PerformsIO is true.
type DialectGrant struct {
	Dialect string // normalized name ("cel", "system_one")
	AllowIO bool   // required for a dialect whose Capabilities.PerformsIO is true
}

// Grant constructs a DialectGrant for a pure (non-I/O) dialect.
func Grant(dialect string) DialectGrant {
	return DialectGrant{Dialect: dialect}
}

// GrantIO constructs a DialectGrant that also authorizes Capabilities.PerformsIO.
func GrantIO(dialect string) DialectGrant {
	return DialectGrant{Dialect: dialect, AllowIO: true}
}

// UnsupportedDialectError.Reason values returned by deny-by-default dialect admission.
const (
	ReasonEmptyConsumer         = "empty_consumer"
	ReasonConsumerNotRegistered = "consumer_not_registered"
	ReasonDialectNotAllowed     = "dialect_not_allowed"
	ReasonIONotAllowed          = "io_not_allowed"
	ReasonUnknownDialect        = "unknown_dialect"
)

// UnsupportedDialectError is returned when a dialect is unknown or disallowed
// for the requesting consumer.
type UnsupportedDialectError struct {
	Dialect  string
	Consumer string
	Reason   string // "empty_consumer" | "consumer_not_registered" | "dialect_not_allowed" | "io_not_allowed" | "unknown_dialect"
}

func (e *UnsupportedDialectError) Error() string {
	var base string
	if e.Consumer != "" {
		base = fmt.Sprintf("unsupported dialect %q for consumer %q", e.Dialect, e.Consumer)
	} else {
		base = fmt.Sprintf("unsupported dialect %q", e.Dialect)
	}
	if e.Reason != "" {
		return base + ": " + e.Reason
	}
	return base
}

// Compiled wraps a dialect-owned compiled rule representation bound to the
// consumer it was compiled for.
type Compiled struct {
	consumer string
	dialect  string
	kind     Kind
	rep      any
}

// NewCompiled constructs a Compiled handle for Dialect implementations.
func NewCompiled(consumer, dialect string, kind Kind, rep any) Compiled {
	return Compiled{
		consumer: strings.TrimSpace(consumer),
		dialect:  strings.TrimSpace(dialect),
		kind:     kind,
		rep:      rep,
	}
}

// Consumer returns the consumer name c was compiled for.
func (c Compiled) Consumer() string { return c.consumer }

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
// deny-by-default per-consumer dialect grants, and bounds evaluation time and budgets.
type Engine struct {
	mu       sync.RWMutex
	dialects map[string]Dialect
	allowed  map[string]map[string]DialectGrant // consumer -> dialect -> grant; absent = deny-by-default
}

// NewEngine creates an empty rule Engine registry.
func NewEngine() *Engine {
	return &Engine{
		dialects: make(map[string]Dialect),
		allowed:  make(map[string]map[string]DialectGrant),
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

// DialectFor returns the registered Dialect with name when consumer is
// admitted to it, or *UnsupportedDialectError otherwise.
func (e *Engine) DialectFor(consumer, name string) (Dialect, error) {
	return e.resolveDialect(consumer, name)
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

// AllowConsumerDialects replaces consumer's dialect allow-list (idempotent).
// An empty or whitespace-only consumer returns ErrEmptyConsumer. Grants may
// name dialects registered later; admission is checked at use.
func (e *Engine) AllowConsumerDialects(consumer string, grants ...DialectGrant) error {
	c := strings.TrimSpace(consumer)
	if c == "" {
		return ErrEmptyConsumer
	}
	grantMap := make(map[string]DialectGrant, len(grants))
	for _, g := range grants {
		if n := strings.TrimSpace(g.Dialect); n != "" {
			grantMap[n] = DialectGrant{
				Dialect: n,
				AllowIO: g.AllowIO,
			}
		}
	}
	e.mu.Lock()
	e.allowed[c] = grantMap
	e.mu.Unlock()
	return nil
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
	c := strings.TrimSpace(consumer)
	if c == "" {
		return nil, &UnsupportedDialectError{
			Dialect: name,
			Reason:  ReasonEmptyConsumer,
		}
	}

	e.mu.RLock()
	grants, registered := e.allowed[c]
	if !registered {
		e.mu.RUnlock()
		return nil, &UnsupportedDialectError{
			Dialect:  name,
			Consumer: c,
			Reason:   ReasonConsumerNotRegistered,
		}
	}
	grant, allowed := grants[name]
	if !allowed {
		e.mu.RUnlock()
		return nil, &UnsupportedDialectError{
			Dialect:  name,
			Consumer: c,
			Reason:   ReasonDialectNotAllowed,
		}
	}
	d, ok := e.dialects[name]
	e.mu.RUnlock()
	if !ok {
		return nil, &UnsupportedDialectError{
			Dialect:  name,
			Consumer: c,
			Reason:   ReasonUnknownDialect,
		}
	}
	if d.Declares().PerformsIO && !grant.AllowIO {
		return nil, &UnsupportedDialectError{
			Dialect:  name,
			Consumer: c,
			Reason:   ReasonIONotAllowed,
		}
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
	env.Consumer = strings.TrimSpace(env.Consumer)
	return d.Compile(ctx, rule, kind, env)
}

// CachedCompile retrieves or compiles rule using the dialect's program cache
// when supported, falling back to Compile otherwise. An empty scope.Consumer
// defaults to env.Consumer; a non-empty scope.Consumer that differs from
// env.Consumer is rejected after admission.
func (e *Engine) CachedCompile(ctx context.Context, dialect string, scope celengine.CacheScope, rule string, kind Kind, env EnvironmentRef) (Compiled, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d, err := e.resolveDialect(env.Consumer, dialect)
	if err != nil {
		return Compiled{}, err
	}
	env.Consumer = strings.TrimSpace(env.Consumer)
	if s := strings.TrimSpace(scope.Consumer); s != "" && s != env.Consumer {
		return Compiled{}, &UnsupportedDialectError{
			Dialect:  normalizeDialect(dialect),
			Consumer: env.Consumer,
			Reason:   ReasonDialectNotAllowed,
		}
	}
	scope.Consumer = env.Consumer
	if cd, ok := d.(CachedDialect); ok {
		return cd.CachedCompile(ctx, scope, rule, kind, env)
	}
	return d.Compile(ctx, rule, kind, env)
}

// Invalidate forwards scoped cache invalidation to a CachedDialect when
// scope.Consumer is admitted to dialect.
func (e *Engine) Invalidate(dialect, envName string, scope celengine.CacheScope) {
	d, err := e.resolveDialect(scope.Consumer, dialect)
	if err != nil {
		return
	}
	scope.Consumer = strings.TrimSpace(scope.Consumer)
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
	d, err := e.resolveDialect(c.Consumer(), c.Dialect())
	if err != nil {
		return Decision{}, err
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

	// Capture start before arming evalCtx so any scheduling delay between
	// context.WithTimeoutCause and d.Evaluate is included in elapsed; both
	// tracker charging and the trackerDeadline margin check below rely on
	// elapsed >= evalTimeout whenever evalCtx expires.
	start := time.Now()

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

	dec, err := d.Evaluate(evalCtx, c, input, effBudget)
	elapsed := time.Since(start)
	if budget.Tracker != nil {
		budget.Tracker.Charge(elapsed)
	}
	if err != nil {
		if callerErr := ctx.Err(); callerErr != nil {
			return Decision{}, &FailedError{
				Reason: celengine.ReasonCanceled,
				Cause:  errors.Join(callerErr, err),
			}
		}
		trackerDeadline := errors.Is(context.Cause(evalCtx), errTrackerTimeout) ||
			elapsed >= evalTimeout-engineDeadlineMargin(evalTimeout)
		if boundFromTracker && trackerDeadline && isTimeoutOrCancelFailure(err) {
			budget.Tracker.Exhaust()
			return Decision{}, fmt.Errorf("%w: %w", ErrBudgetExhausted, err)
		}
		if errors.Is(context.Cause(evalCtx), errDialectEvalTimeout) && isTimeoutOrCancelFailure(err) {
			return Decision{}, &FailedError{
				Reason: celengine.ReasonTimeout,
				Cause:  errors.Join(&celengine.EvalError{Reason: celengine.ReasonTimeout}, err),
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
