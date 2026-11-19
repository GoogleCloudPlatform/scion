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
	"slices"
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

// Stage 1 trigger rule limits (§7.2, §4.2, §4.4, §8.1; mixed-rule-arrays §2.1, §6).
const (
	MaxMatchRuleBytes      = 4096
	MaxMatchRules          = 8
	MaxMatchRulesJSONBytes = 16 * 1024
	MaxRuleDialectBytes    = 32
	MaxSubjectRuleBytes    = 1024
	MaxDerivedSubjectBytes = 256
	MaxPayloadBytes        = 64 * 1024
	// MaxAttributesBytes is the Stage 1 8 KiB serialized attributes ceiling (§7.2,
	// §8.1). Event ingress enforces this bound on serialized attribute bytes before
	// persisting the event; NewEventActivation accepts the already-admitted map.
	MaxAttributesBytes = 8 * 1024
	MaxRuleTraceBytes  = 4 * 1024
	EventRuleBudget    = time.Second

	Stage1CacheMaxEntries         = 4096
	Stage1CacheMaxBytes           = 4 << 20 // 4 MiB across active projects
	Stage1CacheMaxPerProject      = 400     // 2x MaxTriggersPerProject (200)
	Stage1CacheMaxBytesPerProject = 1 << 20 // 1 MiB per project
)

// MatchResult outcome classes (§3.1).
const (
	MatchClassMatched   = "matched"
	MatchClassFalse     = "false"
	MatchClassUncertain = "uncertain"
	MatchClassFailed    = "failed"
	MatchClassBudget    = "budget"
)

// Closed content-free MatchResult.Reason codes (separate from MatchResult.Class
// and from the human-facing SkipReason strings stored in trigger_fires.error).
const (
	MatchReasonBudgetExhausted    = "budget_exhausted"
	MatchReasonCorruptRules       = "corrupt_rules"
	MatchReasonUncertain          = "uncertain"
	MatchReasonEvalFailed         = "eval_failed"
	MatchReasonCompileError       = "compile_error"
	MatchReasonUnsupportedDialect = "unsupported_dialect"
)

var allowedMatchReasons = map[string]bool{
	celengine.ReasonMissingInput:           true,
	celengine.ReasonCostLimit:              true,
	celengine.ReasonTimeout:                true,
	celengine.ReasonCanceled:               true,
	celengine.ReasonRuntime:                true,
	celengine.ReasonType:                   true,
	celengine.CategorySize:                 true,
	celengine.CategorySyntax:               true,
	celengine.CategoryUnknownVariable:      true,
	celengine.CategoryUnknownFunction:      true,
	celengine.CategoryResultType:           true,
	celengine.CategoryRegexLiteralRequired: true,
	ruleengine.ReasonEmptyConsumer:         true,
	ruleengine.ReasonConsumerNotRegistered: true,
	ruleengine.ReasonDialectNotAllowed:     true,
	ruleengine.ReasonIONotAllowed:          true,
	ruleengine.ReasonUnknownDialect:        true,
	MatchReasonBudgetExhausted:             true,
	MatchReasonCorruptRules:                true,
	MatchReasonUncertain:                   true,
	MatchReasonEvalFailed:                  true,
	MatchReasonCompileError:                true,
	MatchReasonUnsupportedDialect:          true,
	"uninitialized_program":                true,
	"invalid_compiled_program":             true,
	"abstain":                              true,
	"tie":                                  true,
	"provider_error":                       true,
	"provider_unavailable":                 true,
	"rate_limited":                         true,
}

func sanitizeMatchReason(code, fallback string) string {
	if allowedMatchReasons[code] {
		return code
	}
	return fallback
}

// SafeDialectName validates that d matches ^[a-z][a-z0-9_]*$ within
// MaxRuleDialectBytes (32B), returning "unknown" otherwise so corrupt stored
// dialect strings never overflow columns or metric cardinality.
func SafeDialectName(d string) string {
	d = strings.TrimSpace(d)
	if len(d) == 0 || len(d) > MaxRuleDialectBytes {
		return "unknown"
	}
	for i := 0; i < len(d); i++ {
		b := d[i]
		if i == 0 {
			if b < 'a' || b > 'z' {
				return "unknown"
			}
			continue
		}
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '_' {
			return "unknown"
		}
	}
	return d
}

// RuleTraceElement records the content-free evaluation outcome of a single
// match_rules element (§5).
type RuleTraceElement struct {
	Index      int           `json:"i"`
	Dialect    string        `json:"dialect"`
	Class      string        `json:"class"`
	Reason     string        `json:"reason,omitempty"`
	DurationMs int64         `json:"duration_ms"`
	Provenance string        `json:"provenance,omitempty"`
	Elapsed    time.Duration `json:"-"`
}

// RuleTrace records the content-free per-element trace and unexecuted element
// indices for a MatchAll evaluation (§5, §9b).
type RuleTrace struct {
	Elements          []RuleTraceElement `json:"elements"`
	Unexecuted        []int              `json:"unexecuted,omitempty"`
	BudgetRemainingMs *int64             `json:"budget_remaining_ms,omitempty"`
}

func sanitizeTraceProvenance(s string) string {
	if s == "" {
		return ""
	}
	const maxProvenanceBytes = 256
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || !unicode.IsPrint(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxProvenanceBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// EncodeRuleTrace serializes trace to bounded content-free JSON (<= 4 KiB, §5).
// Returns "" when trace carries no elements, unexecuted indices, or budget metadata.
func EncodeRuleTrace(trace RuleTrace) string {
	if len(trace.Elements) == 0 && len(trace.Unexecuted) == 0 && trace.BudgetRemainingMs == nil {
		return ""
	}
	work := RuleTrace{
		Elements:          make([]RuleTraceElement, len(trace.Elements)),
		Unexecuted:        append([]int(nil), trace.Unexecuted...),
		BudgetRemainingMs: trace.BudgetRemainingMs,
	}
	for i, el := range trace.Elements {
		el.Dialect = SafeDialectName(el.Dialect)
		el.Provenance = sanitizeTraceProvenance(el.Provenance)
		if el.Reason != "" && !allowedMatchReasons[el.Reason] {
			if el.Class == MatchClassUncertain {
				el.Reason = MatchReasonUncertain
			} else {
				el.Reason = MatchReasonEvalFailed
			}
		}
		work.Elements[i] = el
	}
	if raw, err := json.Marshal(work); err == nil && len(raw) <= MaxRuleTraceBytes {
		return string(raw)
	}
	for i := range work.Elements {
		work.Elements[i].Provenance = ""
		if raw, err := json.Marshal(work); err == nil && len(raw) <= MaxRuleTraceBytes {
			return string(raw)
		}
	}
	for len(work.Elements) > 1 {
		work.Elements = work.Elements[1:]
		if raw, err := json.Marshal(work); err == nil && len(raw) <= MaxRuleTraceBytes {
			return string(raw)
		}
	}
	return `{"elements":[]}`
}

// MatchResult describes the outcome of evaluating an ordered match-rule list
// with short-circuit AND semantics (§3.1).
type MatchResult struct {
	Matched bool
	Through int    // elements evaluated (1-based index of the deciding element; == len(rules) on a full match)
	Dialect string // dialect of the deciding element
	Class   string // "matched" | "false" | "uncertain" | "failed" | "budget"
	Reason  string // closed content-free reason token (empty on matched/false)
}

// Generic history reasons recorded on skipped trigger fires (§6.3, §7.0a, §7.2).
const (
	SkipReasonEvalFailed       = "rule evaluation failed"
	SkipReasonRuleUncertain    = "rule uncertain"
	SkipReasonNoLongerCompiles = "rule no longer compiles"
	SkipReasonBudgetExhausted  = "rule budget exhausted"
)

// ErrCorruptMatchRules is the sentinel error returned when a stored trigger's
// match_rules column cannot be parsed as a JSON rule array or violates stored
// size/count bounds. Normalization fails closed with this error (never
// degrading to an empty Types-only rule list), and SkipReason maps it to
// SkipReasonEvalFailed ("rule evaluation failed") for 3b-2 dispatch history
// recording.
var ErrCorruptMatchRules = errors.New("triggerrule: stored match_rules is corrupt")

type corruptRuleSizeError struct {
	detail string
}

func (e *corruptRuleSizeError) Error() string { return ErrCorruptMatchRules.Error() + ": " + e.detail }
func (e *corruptRuleSizeError) Unwrap() error { return ErrCorruptMatchRules }

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

// Stage1CacheConfig returns the Stage 1 compiled-program cache configuration
// for triggers, including per-project residency bounds (3a security F1/F2,
// ptone/scion#8030).
func Stage1CacheConfig() celengine.CacheConfig {
	return celengine.CacheConfig{
		MaxEntries:         Stage1CacheMaxEntries,
		MaxBytes:           Stage1CacheMaxBytes,
		MaxPerProject:      Stage1CacheMaxPerProject,
		MaxBytesPerProject: Stage1CacheMaxBytesPerProject,
	}
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
	d, err := engine.DialectFor(ConsumerName, ruleengine.DialectCEL)
	if err != nil {
		return nil, fmt.Errorf("triggerrule: engine does not support dialect %q for consumer %q: %w", ruleengine.DialectCEL, ConsumerName, err)
	}
	if cd, isCEL := d.(*ruleengine.CELDialect); isCEL {
		if _, hasEnv := cd.CELEngine(EnvironmentName); !hasEnv {
			return nil, fmt.Errorf("triggerrule: CEL dialect is missing environment %q", EnvironmentName)
		}
	}
	return &Rules{engine: engine}, nil
}

// NewDefault builds a standalone *Rules backed by a new *ruleengine.Engine with
// the Stage 1 CEL dialect registered over Env(), Stage1Limits(), and
// Stage1CacheConfig().
func NewDefault() (*Rules, error) {
	celEng, err := celengine.New(Env(), Stage1Limits(), Stage1CacheConfig())
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
	if err := re.AllowConsumerDialects(ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)); err != nil {
		return nil, err
	}
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
	dialect := rule.normalizedDialect()
	if _, err := r.engine.DialectFor(ConsumerName, dialect); err != nil {
		return err
	}
	if rule.Expr == "" {
		return nil
	}
	_, err := r.engine.Compile(ctx, dialect, rule.Expr, ruleengine.KindFilter, matchEnvRef())
	return err
}

// CompileSubject validates rule for the trigger subject slot (<= 1024 bytes, string).
// An empty Expr with a supported dialect is valid (Subject returns ("", nil) and
// DeriveSubject substitutes the fallback event.subject).
func (r *Rules) CompileSubject(ctx context.Context, rule Rule) error {
	dialect := rule.normalizedDialect()
	if _, err := r.engine.DialectFor(ConsumerName, dialect); err != nil {
		return err
	}
	if rule.Expr == "" {
		return nil
	}
	_, err := r.engine.Compile(ctx, dialect, rule.Expr, ruleengine.KindSubject, subjectEnvRef())
	return err
}

// Invalidate removes cached compiled programs for scope in dialect.
func (r *Rules) Invalidate(dialect string, scope celengine.CacheScope) {
	if scope.Consumer == "" {
		scope.Consumer = ConsumerName
	}
	r.engine.Invalidate(dialect, EnvironmentName, scope)
}

// IsRegisteredDialect reports whether dialect is registered on the underlying
// ruleengine.Engine, allowing metrics callers to restrict dialect labels to the
// finite server-registered dialect set.
func (r *Rules) IsRegisteredDialect(dialect string) bool {
	if r == nil || r.engine == nil {
		return false
	}
	return slices.Contains(r.engine.Dialects(), strings.TrimSpace(dialect))
}

// Activation holds the pre-parsed event map so multiple candidate trigger rules
// share one JSON parse per event (§6.3, F5).
type Activation struct {
	eventID   string
	projectID string
	eventMap  map[string]any
	trigMap   map[string]any
}

// NewEventActivation parses ev.Payload once (when non-empty valid JSON) and
// constructs a base Activation that can be rebound per candidate trigger via
// WithTrigger. Per §7.1, if ev.Payload is empty or is not valid JSON,
// event.payload is omitted from the activation so has(event.payload) is false.
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
		if parsedPayload, err := parseJSONPayload(trimmed); err == nil {
			eventMap["payload"] = parsedPayload
		}
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

func normalizeTriggerScope(scope celengine.CacheScope, projectID string) celengine.CacheScope {
	if scope.Consumer == "" {
		scope.Consumer = ConsumerName
	}
	if scope.ProjectID == "" && scope.ID != "" && projectID != "" {
		scope.ProjectID = projectID
	}
	return scope
}

// Match evaluates rule for the trigger filter slot using the engine's program
// cache. An empty rule.Expr returns (true, nil). Compile time on a cache miss
// is never charged to the shared per-event BudgetTracker (O2 / 3a F2).
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
	if _, err := r.engine.DialectFor(ConsumerName, dialect); err != nil {
		return false, err
	}
	if rule.Expr == "" {
		return true, nil
	}
	scope = normalizeTriggerScope(scope, act.projectID)

	compiled, err := r.engine.CachedCompile(ctx, dialect, scope, rule.Expr, ruleengine.KindFilter, matchEnvRef())
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

// MatchAll evaluates rules in strict author order with short-circuit AND
// semantics (§3.1). See MatchAllWithTrace for per-element trace output.
func (r *Rules) MatchAll(
	ctx context.Context,
	scope celengine.CacheScope,
	rules []Rule,
	act Activation,
	opts ...EvalOption,
) (MatchResult, error) {
	res, _, err := r.MatchAllWithTrace(ctx, scope, rules, act, opts...)
	return res, err
}

func unexecutedIndices(after, total int) []int {
	if after >= total {
		return nil
	}
	out := make([]int, 0, total-after)
	for k := after + 1; k <= total; k++ {
		out = append(out, k)
	}
	return out
}

func budgetRemainingMsPtr(tracker *ruleengine.BudgetTracker) *int64 {
	if tracker == nil {
		return nil
	}
	ms := tracker.Remaining().Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return &ms
}

// MatchAllWithTrace evaluates rules in strict author order with short-circuit
// AND semantics (§3.1, §5), returning both the summary MatchResult and the
// content-free per-element RuleTrace.
//
// Security L1 seam note (ptone/scion#8030): Stage 1 grants consumer "trigger"
// only pure, no-I/O Grant("cel"), where celengine.Program.Eval enforces a
// post-return wall-clock backstop. Admitting any second dialect to "trigger"
// requires a post-return deadline check in ruleengine.Evaluate or that dialect.
func (r *Rules) MatchAllWithTrace(
	ctx context.Context,
	scope celengine.CacheScope,
	rules []Rule,
	act Activation,
	opts ...EvalOption,
) (MatchResult, RuleTrace, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			fErr := &ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err}
			dialect := ""
			through := 0
			if len(rules) > 0 {
				dialect = SafeDialectName(rules[0].normalizedDialect())
				through = 1
			}
			return MatchResult{
				Matched: false,
				Through: through,
				Dialect: dialect,
				Class:   MatchClassFailed,
				Reason:  celengine.ReasonCanceled,
			}, RuleTrace{}, fErr
		}
	}
	if len(rules) == 0 {
		return MatchResult{
			Matched: true,
			Through: 0,
			Class:   MatchClassMatched,
		}, RuleTrace{}, nil
	}

	cfg := applyEvalOpts(opts)

	// Evaluation-time list count cap (SEC-3b1rw-01 defence in depth).
	if len(rules) > MaxMatchRules {
		dialect := SafeDialectName(rules[0].normalizedDialect())
		err := &corruptRuleSizeError{detail: fmt.Sprintf("rule list count %d exceeds maximum %d", len(rules), MaxMatchRules)}
		trace := RuleTrace{
			Elements: []RuleTraceElement{{
				Index:   1,
				Dialect: dialect,
				Class:   MatchClassFailed,
				Reason:  celengine.CategorySize,
			}},
			BudgetRemainingMs: budgetRemainingMsPtr(cfg.tracker),
		}
		return MatchResult{
			Matched: false,
			Through: 1,
			Dialect: dialect,
			Class:   MatchClassFailed,
			Reason:  celengine.CategorySize,
		}, trace, err
	}

	scope = normalizeTriggerScope(scope, act.projectID)
	input := ruleengine.Input{Activation: act.toMap()}
	lastDialect := ""
	trace := RuleTrace{
		Elements: make([]RuleTraceElement, 0, len(rules)),
	}

	finishEarly := func(through int, dialect, cls, reason string, elapsed time.Duration, prov string, err error) (MatchResult, RuleTrace, error) {
		safeDialect := SafeDialectName(dialect)
		trace.Elements = append(trace.Elements, RuleTraceElement{
			Index:      through,
			Dialect:    safeDialect,
			Class:      cls,
			Reason:     reason,
			DurationMs: max(elapsed.Milliseconds(), int64(0)),
			Provenance: sanitizeTraceProvenance(prov),
			Elapsed:    max(elapsed, time.Duration(0)),
		})
		trace.Unexecuted = unexecutedIndices(through, len(rules))
		trace.BudgetRemainingMs = budgetRemainingMsPtr(cfg.tracker)
		return MatchResult{
			Matched: false,
			Through: through,
			Dialect: safeDialect,
			Class:   cls,
			Reason:  reason,
		}, trace, err
	}

	for i, elem := range rules {
		through := i + 1
		rawDialect := elem.normalizedDialect()
		dialect := SafeDialectName(rawDialect)

		if ctx != nil {
			if err := ctx.Err(); err != nil {
				fErr := &ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err}
				return finishEarly(through, dialect, MatchClassFailed, celengine.ReasonCanceled, 0, "", fErr)
			}
		}
		if cfg.tracker != nil && cfg.tracker.Exhausted() {
			return finishEarly(through, dialect, MatchClassBudget, MatchReasonBudgetExhausted, 0, "", ruleengine.ErrBudgetExhausted)
		}

		if strings.TrimSpace(elem.Expr) == "" {
			return finishEarly(through, dialect, MatchClassFailed, MatchReasonCorruptRules, 0, "", ErrCorruptMatchRules)
		}
		if len(elem.Expr) > MaxMatchRuleBytes {
			err := &corruptRuleSizeError{detail: fmt.Sprintf("element %d length %d exceeds maximum %d", through, len(elem.Expr), MaxMatchRuleBytes)}
			return finishEarly(through, dialect, MatchClassFailed, celengine.CategorySize, 0, "", err)
		}

		d, err := r.engine.DialectFor(ConsumerName, rawDialect)
		if err != nil {
			cls, reason := classifyMatchError(err)
			return finishEarly(through, dialect, cls, reason, 0, "", err)
		}

		// O2 / 3a F2: compile time on a cache miss is NOT charged to cfg.tracker.
		compiled, err := r.engine.CachedCompile(ctx, rawDialect, scope, elem.Expr, ruleengine.KindFilter, matchEnvRef())
		if err != nil {
			cls, reason := classifyMatchError(err)
			return finishEarly(through, dialect, cls, reason, 0, "", err)
		}

		evalStart := time.Now()
		dec, err := r.engine.Evaluate(ctx, compiled, input, ruleengine.Budget{
			MaxEvalTime: d.Declares().MaxEvalTime,
			Tracker:     cfg.tracker,
		})
		evalElapsed := max(time.Since(evalStart), time.Duration(0))
		if err != nil {
			cls, reason := classifyMatchError(err)
			return finishEarly(through, dialect, cls, reason, evalElapsed, dec.Trace, err)
		}
		if !dec.Bool {
			return finishEarly(through, dialect, MatchClassFalse, "", evalElapsed, dec.Trace, nil)
		}
		trace.Elements = append(trace.Elements, RuleTraceElement{
			Index:      through,
			Dialect:    dialect,
			Class:      MatchClassMatched,
			DurationMs: max(evalElapsed.Milliseconds(), int64(0)),
			Provenance: sanitizeTraceProvenance(dec.Trace),
			Elapsed:    evalElapsed,
		})
		lastDialect = dialect
	}

	trace.BudgetRemainingMs = budgetRemainingMsPtr(cfg.tracker)
	return MatchResult{
		Matched: true,
		Through: len(rules),
		Dialect: lastDialect,
		Class:   MatchClassMatched,
	}, trace, nil
}

func classifyMatchError(err error) (class, reason string) {
	if cls, rsn, matched := classifyMatchErrorTopDown(err); matched {
		return cls, rsn
	}
	if errors.Is(err, context.Canceled) {
		return MatchClassFailed, celengine.ReasonCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return MatchClassFailed, celengine.ReasonTimeout
	}
	return MatchClassFailed, MatchReasonEvalFailed
}

func classifyMatchErrorTopDown(err error) (class, reason string, matched bool) {
	if err == nil {
		return "", "", false
	}
	if err == ruleengine.ErrBudgetExhausted {
		return MatchClassBudget, MatchReasonBudgetExhausted, true
	}
	if _, isSize := err.(*corruptRuleSizeError); isSize {
		return MatchClassFailed, celengine.CategorySize, true
	}
	if err == ErrCorruptMatchRules {
		return MatchClassFailed, MatchReasonCorruptRules, true
	}
	switch e := err.(type) {
	case *ruleengine.UncertainError:
		return MatchClassUncertain, sanitizeMatchReason(e.Reason, MatchReasonUncertain), true
	case *ruleengine.FailedError:
		return MatchClassFailed, sanitizeMatchReason(e.Reason, MatchReasonEvalFailed), true
	case *celengine.EvalError:
		return MatchClassFailed, sanitizeMatchReason(e.Reason, MatchReasonEvalFailed), true
	case *celengine.CompileError:
		return MatchClassFailed, sanitizeMatchReason(e.Category, MatchReasonCompileError), true
	case *ruleengine.UnsupportedDialectError:
		return MatchClassFailed, sanitizeMatchReason(e.Reason, MatchReasonUnsupportedDialect), true
	}

	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, sub := range multi.Unwrap() {
			if c, r, m := classifyMatchErrorTopDown(sub); m {
				return c, r, true
			}
		}
		return "", "", false
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return classifyMatchErrorTopDown(single.Unwrap())
	}
	return "", "", false
}

// Subject evaluates rule for the trigger subject slot using the engine's
// program cache. An empty rule.Expr returns ("", nil); callers that apply the
// fallback event.subject and Stage 1 subject output bounds use DeriveSubject.
// Compile time on a cache miss is never charged to cfg.tracker (O2 / 3a F2).
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
	if _, err := r.engine.DialectFor(ConsumerName, dialect); err != nil {
		return "", err
	}
	if rule.Expr == "" {
		return "", nil
	}
	scope = normalizeTriggerScope(scope, act.projectID)

	compiled, err := r.engine.CachedCompile(ctx, dialect, scope, rule.Expr, ruleengine.KindSubject, subjectEnvRef())
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
	if reason, ok, matched := classifyRuleErrorTopDown(err); matched {
		return reason, ok
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	return "", false
}

func classifyRuleErrorTopDown(err error) (reason string, isSkip bool, matched bool) {
	if err == nil {
		return "", false, false
	}
	// Check direct sentinel equality on the current node only — do NOT use
	// errors.Is(err, ruleengine.ErrBudgetExhausted) here. errors.Is walks the
	// entire Unwrap tree at the root call, which would let a deeper
	// ErrBudgetExhausted override an outer classification in the switch below
	// (for example &ruleengine.FailedError{Reason: ReasonCanceled, Cause: ...}
	// when caller cancellation wraps a dialect error). Wrapped sentinels are
	// still reached in outermost-first order by the Unwrap recursion below.
	if err == ruleengine.ErrBudgetExhausted {
		return SkipReasonBudgetExhausted, true, true
	}
	if err == ErrCorruptMatchRules {
		return SkipReasonEvalFailed, true, true
	}
	switch e := err.(type) {
	case *ruleengine.FailedError:
		if e.Reason == celengine.ReasonCanceled {
			return "", false, true
		}
		return SkipReasonEvalFailed, true, true
	case *ruleengine.UncertainError:
		return SkipReasonRuleUncertain, true, true
	case *celengine.CompileError:
		return SkipReasonNoLongerCompiles, true, true
	case *ruleengine.UnsupportedDialectError:
		return SkipReasonEvalFailed, true, true
	case *celengine.EvalError:
		if e.Reason == celengine.ReasonCanceled {
			return "", false, true
		}
		return SkipReasonEvalFailed, true, true
	}

	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, sub := range multi.Unwrap() {
			if r, s, m := classifyRuleErrorTopDown(sub); m {
				return r, s, true
			}
		}
		return "", false, false
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return classifyRuleErrorTopDown(single.Unwrap())
	}
	return "", false, false
}
