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

// Package triggerrule defines the Stage 1 trigger rule environment (§7.1),
// once-per-event JSON activation builder, match/subject rule compilation and
// evaluation via pkg/ruleengine (§7.2), subject derivation bounds (§4.4/F6),
// outcome skip-reason mapping (§6.3), and match_types prefiltering (§7.3).
package triggerrule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// Trigger rule consumer and environment identifiers.
const (
	ConsumerName       = "trigger"
	EnvironmentName    = "trigger"
	EnvironmentVersion = 1
)

// Stage 1 trigger rule limits (§7.2, §4.2, §4.4).
const (
	MaxMatchRuleBytes      = 4096
	MaxSubjectRuleBytes    = 1024
	MaxDerivedSubjectBytes = 256
	MaxPayloadBytes        = 64 * 1024
	MaxAttributesBytes     = 8 * 1024
	EventRuleBudget        = time.Second
)

// Generic history reasons recorded on skipped trigger fires (§6.3, §7.0a, §7.2).
const (
	SkipReasonEvalFailed       = "rule evaluation failed"
	SkipReasonRuleUncertain    = "rule uncertain"
	SkipReasonNoLongerCompiles = "rule no longer compiles"
	SkipReasonBudgetExhausted  = "rule budget exhausted"
)

// Rule pairs a dialect tag (empty defaults to "cel") with a rule expression.
type Rule struct {
	Dialect string
	Expr    string
}

func (r Rule) normalizedDialect() string {
	d := strings.TrimSpace(r.Dialect)
	if d == "" {
		return ruleengine.DialectCEL
	}
	return d
}

// Env returns the load-bearing Stage 1 trigger CEL Environment (Version 1, §7.1).
func Env() celengine.Environment {
	return celengine.Environment{
		Name:    EnvironmentName,
		Version: EnvironmentVersion,
		Variables: []celengine.Variable{
			{Name: "event", Type: celengine.Map(celengine.String(), celengine.Dyn())},
			{Name: "trigger", Type: celengine.Map(celengine.String(), celengine.Dyn())},
		},
		Extensions: []celengine.Extension{celengine.ExtensionStrings},
		Macros:     celengine.MacrosStandard,
	}
}

// Stage1Limits returns the Stage 1 CEL engine ceiling Limits for triggers.
func Stage1Limits() celengine.Limits {
	return celengine.DefaultLimits()
}

// Rules evaluates and validates trigger match and subject rules over a shared
// *ruleengine.Engine. It holds no cel.Env or program cache of its own.
type Rules struct {
	engine *ruleengine.Engine
}

// Engine aliases Rules for callers referencing *triggerrule.Engine (§11).
type Engine = Rules

// New wraps a shared *ruleengine.Engine. The CEL dialect (with the "trigger"
// environment registered when backed by *ruleengine.CELDialect) must be
// permitted for ConsumerName ("trigger") on engine.
func New(engine *ruleengine.Engine) (*Rules, error) {
	if engine == nil {
		return nil, errors.New("triggerrule: nil ruleengine.Engine")
	}
	if !engine.Supports(ConsumerName, ruleengine.DialectCEL) {
		return nil, fmt.Errorf("triggerrule: engine does not support dialect %q for consumer %q", ruleengine.DialectCEL, ConsumerName)
	}
	if d, ok := engine.Dialect(ruleengine.DialectCEL); ok {
		if cd, isCEL := d.(*ruleengine.CELDialect); isCEL {
			if _, hasEnv := cd.CELEngine(EnvironmentName); !hasEnv {
				return nil, fmt.Errorf("triggerrule: CEL dialect is missing environment %q", EnvironmentName)
			}
		}
	}
	return &Rules{engine: engine}, nil
}

// NewDefault builds a standalone *Rules backed by a new *ruleengine.Engine with
// the Stage 1 CEL dialect registered over Env() and Stage1Limits().
func NewDefault() (*Rules, error) {
	celEng, err := celengine.New(Env(), Stage1Limits(), celengine.DefaultCacheConfig())
	if err != nil {
		return nil, err
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		return nil, err
	}
	re := ruleengine.NewEngine()
	if err := re.Register(celDialect); err != nil {
		return nil, err
	}
	re.AllowConsumerDialects(ConsumerName, ruleengine.DialectCEL)
	return New(re)
}

func matchEnvRef() ruleengine.EnvironmentRef {
	return ruleengine.EnvironmentRef{
		Consumer:     ConsumerName,
		Name:         EnvironmentName,
		MaxRuleBytes: MaxMatchRuleBytes,
	}
}

func subjectEnvRef() ruleengine.EnvironmentRef {
	return ruleengine.EnvironmentRef{
		Consumer:     ConsumerName,
		Name:         EnvironmentName,
		MaxRuleBytes: MaxSubjectRuleBytes,
	}
}

// CompileMatch validates rule for the trigger match slot (<= 4096 bytes, bool).
// An empty Expr with a supported dialect is valid (unconditional match).
func (r *Rules) CompileMatch(ctx context.Context, rule Rule) error {
	if !r.engine.Supports(ConsumerName, rule.normalizedDialect()) {
		return &ruleengine.UnsupportedDialectError{
			Dialect:  rule.normalizedDialect(),
			Consumer: ConsumerName,
		}
	}
	if rule.Expr == "" {
		return nil
	}
	_, err := r.engine.Compile(ctx, rule.normalizedDialect(), rule.Expr, ruleengine.KindFilter, matchEnvRef())
	return err
}

// CompileSubject validates rule for the trigger subject slot (<= 1024 bytes, string).
// An empty Expr with a supported dialect is valid (uses event.subject).
func (r *Rules) CompileSubject(ctx context.Context, rule Rule) error {
	if !r.engine.Supports(ConsumerName, rule.normalizedDialect()) {
		return &ruleengine.UnsupportedDialectError{
			Dialect:  rule.normalizedDialect(),
			Consumer: ConsumerName,
		}
	}
	if rule.Expr == "" {
		return nil
	}
	_, err := r.engine.Compile(ctx, rule.normalizedDialect(), rule.Expr, ruleengine.KindSubject, subjectEnvRef())
	return err
}

// Invalidate removes cached compiled programs for scope in dialect.
func (r *Rules) Invalidate(dialect string, scope celengine.CacheScope) {
	if scope.Consumer == "" {
		scope.Consumer = ConsumerName
	}
	r.engine.Invalidate(dialect, EnvironmentName, scope)
}

// Activation holds the pre-parsed event map so multiple candidate trigger rules
// share one JSON parse per event (§6.3, F5).
type Activation struct {
	eventID   string
	projectID string
	eventMap  map[string]any
	trigMap   map[string]any
}

// NewEventActivation parses ev.Payload once (when non-empty) and constructs a
// base Activation that can be rebound per candidate trigger via WithTrigger.
func NewEventActivation(ev hubfire.FireEvent) (Activation, error) {
	if len(ev.Payload) > MaxPayloadBytes {
		return Activation{}, fmt.Errorf("triggerrule: payload length %d exceeds maximum %d bytes", len(ev.Payload), MaxPayloadBytes)
	}

	attrs := make(map[string]string, len(ev.Attributes))
	maps.Copy(attrs, ev.Attributes)

	eventMap := map[string]any{
		"id":   ev.ID,
		"type": ev.Type,
		"source": map[string]any{
			"kind": ev.SourceKind,
			"id":   ev.SourceID,
		},
		"subject":     ev.Subject,
		"occurred_at": ev.OccurredAt.UTC(),
		"received_at": ev.ReceivedAt.UTC(),
		"attributes":  attrs,
	}

	if trimmed := bytes.TrimSpace(ev.Payload); len(trimmed) > 0 {
		parsedPayload, err := parseJSONPayload(trimmed)
		if err != nil {
			return Activation{}, fmt.Errorf("triggerrule: decode event payload: %w", err)
		}
		eventMap["payload"] = parsedPayload
	}

	var tr hubfire.FireTriggerRef
	if ev.Trigger != nil {
		tr = *ev.Trigger
	}
	return Activation{
		eventID:   ev.ID,
		projectID: ev.ProjectID,
		eventMap:  eventMap,
	}.WithTrigger(tr), nil
}

// NewActivation parses ev.Payload once and binds trigger metadata tr. To reuse
// the parsed payload across multiple candidate triggers on the same event, call
// act.WithTrigger(nextTrigger).
func NewActivation(ev hubfire.FireEvent, tr hubfire.FireTriggerRef) (Activation, error) {
	base, err := NewEventActivation(ev)
	if err != nil {
		return Activation{}, err
	}
	return base.WithTrigger(tr), nil
}

// WithTrigger returns a shallow copy of a sharing the already-parsed event map
// while binding trigger.{id, name, project_id} to tr.
func (a Activation) WithTrigger(tr hubfire.FireTriggerRef) Activation {
	return Activation{
		eventID:   a.eventID,
		projectID: a.projectID,
		eventMap:  a.eventMap,
		trigMap: map[string]any{
			"id":         tr.ID,
			"name":       tr.Name,
			"project_id": a.projectID,
		},
	}
}

func (a Activation) toMap() map[string]any {
	return map[string]any{
		"event":   a.eventMap,
		"trigger": a.trigMap,
	}
}

func parseJSONPayload(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return normalizeJSONNumbers(val)
}

func normalizeJSONNumbers(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			if i, err := x.Int64(); err == nil {
				return i, nil
			}
		}
		f, err := x.Float64()
		if err != nil {
			return nil, err
		}
		return f, nil
	case map[string]any:
		for k, elem := range x {
			nv, err := normalizeJSONNumbers(elem)
			if err != nil {
				return nil, err
			}
			x[k] = nv
		}
		return x, nil
	case []any:
		for i, elem := range x {
			nv, err := normalizeJSONNumbers(elem)
			if err != nil {
				return nil, err
			}
			x[i] = nv
		}
		return x, nil
	default:
		return v, nil
	}
}

// EvalOption configures per-call options on Match, Subject, and DeriveSubject.
type EvalOption func(*evalConfig)

type evalConfig struct {
	tracker *ruleengine.BudgetTracker
}

// WithBudget attaches an aggregate per-event BudgetTracker (see NewEventBudget).
func WithBudget(tracker *ruleengine.BudgetTracker) EvalOption {
	return func(c *evalConfig) {
		c.tracker = tracker
	}
}

// NewEventBudget creates a 1s aggregate per-event rule budget tracker (§6.3, F5).
func NewEventBudget() *ruleengine.BudgetTracker {
	return ruleengine.NewBudgetTracker(EventRuleBudget)
}

func applyEvalOpts(opts []EvalOption) evalConfig {
	var cfg evalConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// Match evaluates rule for the trigger filter slot using the engine's program
// cache. An empty rule.Expr returns (true, nil).
func (r *Rules) Match(ctx context.Context, scope celengine.CacheScope, rule Rule, act Activation, opts ...EvalOption) (bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return false, &ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err}
		}
	}
	cfg := applyEvalOpts(opts)
	if cfg.tracker != nil && cfg.tracker.Exhausted() {
		return false, ruleengine.ErrBudgetExhausted
	}
	dialect := rule.normalizedDialect()
	if !r.engine.Supports(ConsumerName, dialect) {
		return false, &ruleengine.UnsupportedDialectError{Dialect: dialect, Consumer: ConsumerName}
	}
	if rule.Expr == "" {
		return true, nil
	}
	if scope.Consumer == "" {
		scope.Consumer = ConsumerName
	}

	compileStart := time.Now()
	compiled, err := r.engine.CachedCompile(ctx, dialect, scope, rule.Expr, ruleengine.KindFilter, matchEnvRef())
	if cfg.tracker != nil {
		cfg.tracker.Charge(time.Since(compileStart))
	}
	if err != nil {
		return false, err
	}

	dec, err := r.engine.Evaluate(ctx, compiled, ruleengine.Input{Activation: act.toMap()}, ruleengine.Budget{
		Tracker: cfg.tracker,
	})
	if err != nil {
		return false, err
	}
	return dec.Bool, nil
}

// Subject evaluates rule for the trigger subject slot using the engine's
// program cache.
func (r *Rules) Subject(ctx context.Context, scope celengine.CacheScope, rule Rule, act Activation, opts ...EvalOption) (string, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return "", &ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err}
		}
	}
	cfg := applyEvalOpts(opts)
	if cfg.tracker != nil && cfg.tracker.Exhausted() {
		return "", ruleengine.ErrBudgetExhausted
	}
	dialect := rule.normalizedDialect()
	if !r.engine.Supports(ConsumerName, dialect) {
		return "", &ruleengine.UnsupportedDialectError{Dialect: dialect, Consumer: ConsumerName}
	}
	if rule.Expr == "" {
		return "", nil
	}
	if scope.Consumer == "" {
		scope.Consumer = ConsumerName
	}

	compileStart := time.Now()
	compiled, err := r.engine.CachedCompile(ctx, dialect, scope, rule.Expr, ruleengine.KindSubject, subjectEnvRef())
	if cfg.tracker != nil {
		cfg.tracker.Charge(time.Since(compileStart))
	}
	if err != nil {
		return "", err
	}

	dec, err := r.engine.Evaluate(ctx, compiled, ruleengine.Input{Activation: act.toMap()}, ruleengine.Budget{
		Tracker: cfg.tracker,
	})
	if err != nil {
		return "", err
	}
	return dec.String, nil
}

// DeriveSubject evaluates rule when non-empty and validates the derived string
// against Stage 1 bounds (non-empty, <= 256 bytes, printable UTF-8; §4.4, §6.3,
// F6). On empty rule, evaluation error, or invalid derived output, it returns
// fallback (logging a content-free warning via logger when non-nil on failure).
func (r *Rules) DeriveSubject(
	ctx context.Context,
	scope celengine.CacheScope,
	rule Rule,
	act Activation,
	fallback string,
	logger *slog.Logger,
	opts ...EvalOption,
) string {
	if rule.Expr == "" {
		return fallback
	}
	derived, err := r.Subject(ctx, scope, rule, act, opts...)
	if err != nil {
		if logger != nil {
			reason, ok := SkipReason(err)
			if !ok {
				reason = celengine.ReasonCanceled
			}
			logger.Warn("trigger subject_rule failed; falling back to event subject",
				"trigger_id", scope.ID,
				"event_id", act.eventID,
				"reason", reason,
			)
		}
		return fallback
	}
	if vErr := ValidateDerivedSubject(derived); vErr != nil {
		if logger != nil {
			logger.Warn("trigger subject_rule output invalid; falling back to event subject",
				"trigger_id", scope.ID,
				"event_id", act.eventID,
				"reason", vErr.Error(),
			)
		}
		return fallback
	}
	return derived
}

// ValidateDerivedSubject checks that s is non-empty, at most 256 bytes, valid
// UTF-8, and consists solely of printable runes (F6). Errors are content-free.
func ValidateDerivedSubject(s string) error {
	if s == "" {
		return errors.New("derived subject is empty")
	}
	if len(s) > MaxDerivedSubjectBytes {
		return fmt.Errorf("derived subject length %d exceeds %d bytes", len(s), MaxDerivedSubjectBytes)
	}
	if !IsPrintableSubject(s) {
		return errors.New("derived subject contains non-printable or invalid UTF-8 characters")
	}
	return nil
}

// IsPrintableSubject reports whether s is valid UTF-8 and every rune satisfies unicode.IsPrint.
func IsPrintableSubject(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// SkipReason maps a terminal Match/Subject rule error to the generic history
// reason recorded on a skipped trigger_fires row (§6.3, §7.0a, §7.2). It returns
// ("", false) when err is nil or when evaluation was aborted by caller context
// cancellation or deadline (for example hub shutdown or lost lease), so callers
// do not record transient infrastructure aborts as terminal rule skips.
func SkipReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var failedErr *ruleengine.FailedError
	if errors.As(err, &failedErr) && failedErr.Reason == celengine.ReasonCanceled {
		return "", false
	}
	var evalErr *celengine.EvalError
	if errors.As(err, &evalErr) && evalErr.Reason == celengine.ReasonCanceled {
		return "", false
	}
	if !errors.As(err, &failedErr) && !errors.As(err, &evalErr) &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return "", false
	}
	if errors.Is(err, ruleengine.ErrBudgetExhausted) {
		return SkipReasonBudgetExhausted, true
	}
	var compileErr *celengine.CompileError
	var unsupportedErr *ruleengine.UnsupportedDialectError
	if errors.As(err, &compileErr) || errors.As(err, &unsupportedErr) {
		return SkipReasonNoLongerCompiles, true
	}
	var uncertainErr *ruleengine.UncertainError
	if errors.As(err, &uncertainErr) {
		return SkipReasonRuleUncertain, true
	}
	if errors.As(err, &failedErr) || errors.As(err, &evalErr) {
		return SkipReasonEvalFailed, true
	}
	return "", false
}
