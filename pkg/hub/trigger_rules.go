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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

var (
	errTriggerRuleEngineNotInitialized = errors.New("trigger rule engine is not initialized")
	ruleDialectNameRE                  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Trigger rule registration, authoring validation, and engine seam
// (Stage 1 §7.0, §7.0a, §7.1, §7.2; ptone/scion#8030).
//
// Security L1 seam note (ptone/scion#8030): ruleengine.Evaluate derives a context
// deadline for the dialect call and budget tracker, but does not itself discard a
// Decision returned after that deadline if a dialect ignores context cancellation.
// Consumer "trigger" is admitted exclusively to the pure, no-I/O "cel" dialect,
// which is safe because celengine.Program.Eval enforces a post-return wall-clock
// backstop (Elapsed > EvalTimeout returns EvalError{Reason: "timeout"}, never a
// decision). Stage 1 3b-2 dispatch relies on CEL's backstop; admitting any second
// dialect to "trigger" requires either a dialect-level post-return deadline
// backstop or an engine-level post-return check in ruleengine.Evaluate.

// registerTriggerRules builds the Stage 1 trigger CEL environment over Stage 1
// limits and default cache config, registers it on s.celDialect, grants consumer
// "trigger" pure CEL admission on s.rules, and attaches the shared
// *triggerrule.Rules wrapper to s.triggerRules.
//
// Registration performs no network I/O and reads neither the database nor the
// operational settings store.
func (s *Server) registerTriggerRules() error {
	if s.rules == nil || s.celDialect == nil {
		return fmt.Errorf("triggers: cannot register rules before rule engine and CEL dialect are initialized")
	}
	celEng, err := celengine.New(
		triggerrule.Env(),
		triggerrule.Stage1Limits(),
		celengine.DefaultCacheConfig(),
	)
	if err != nil {
		return fmt.Errorf("create trigger CEL engine: %w", err)
	}
	if err := s.celDialect.RegisterEnvironment(celEng); err != nil {
		return fmt.Errorf("register trigger CEL environment: %w", err)
	}
	if err := s.rules.AllowConsumerDialects(
		triggerrule.ConsumerName,
		ruleengine.Grant(ruleengine.DialectCEL),
	); err != nil {
		return fmt.Errorf("grant trigger CEL dialect: %w", err)
	}
	trRules, err := triggerrule.New(s.rules)
	if err != nil {
		return fmt.Errorf("create trigger rules wrapper: %w", err)
	}
	s.triggerRules = trRules
	return nil
}

// TriggerRules returns the Hub Server's shared *triggerrule.Rules wrapper.
func (s *Server) TriggerRules() *triggerrule.Rules {
	if s == nil {
		return nil
	}
	return s.triggerRules
}

func (s *Server) compileTriggerMatchRule(ctx context.Context, dialect, expr string) error {
	if s != nil && s.triggerRules != nil {
		return s.triggerRules.CompileMatch(ctx, triggerrule.Rule{
			Dialect: dialect,
			Expr:    expr,
		})
	}
	if s != nil && s.rules != nil {
		d := strings.TrimSpace(dialect)
		if d == "" {
			d = ruleengine.DialectCEL
		}
		if _, err := s.rules.DialectFor(triggerrule.ConsumerName, d); err != nil {
			return err
		}
	}
	return errTriggerRuleEngineNotInitialized
}

func (s *Server) compileTriggerSubjectRule(ctx context.Context, dialect, expr string) error {
	if s != nil && s.triggerRules != nil {
		return s.triggerRules.CompileSubject(ctx, triggerrule.Rule{
			Dialect: dialect,
			Expr:    expr,
		})
	}
	if s != nil && s.rules != nil {
		d := strings.TrimSpace(dialect)
		if d == "" {
			d = ruleengine.DialectCEL
		}
		if _, err := s.rules.DialectFor(triggerrule.ConsumerName, d); err != nil {
			return err
		}
	}
	return errTriggerRuleEngineNotInitialized
}

func validateTriggerRuleExpr(field, expr string) error {
	if !utf8.ValidString(expr) || strings.ContainsFunc(expr, isDisallowedTriggerDescriptionRune) {
		return fmt.Errorf("%s must not contain control or format characters", field)
	}
	return nil
}

func validateTriggerDialectName(dialect string) error {
	if len(dialect) > triggerrule.MaxRuleDialectBytes || !ruleDialectNameRE.MatchString(dialect) {
		return fmt.Errorf("invalid dialect name (must match ^[a-z][a-z0-9_]*$ and be at most %d bytes)", triggerrule.MaxRuleDialectBytes)
	}
	return nil
}

// triggerMatchRules normalizes a stored Trigger's match rules on read
// (mixed-rule-arrays §2.1): non-empty MatchRules wins; else a defensive
// non-empty scalar MatchRule normalizes to a one-element slice with
// MatchDialect (defaulting to "cel"); else nil (Types-only match).
func triggerMatchRules(tr store.Trigger) []triggerrule.Rule {
	if len(tr.MatchRules) > 0 {
		out := make([]triggerrule.Rule, len(tr.MatchRules))
		for i, r := range tr.MatchRules {
			d := strings.TrimSpace(r.Dialect)
			if d == "" {
				d = ruleengine.DialectCEL
			}
			out[i] = triggerrule.Rule{
				Dialect: d,
				Expr:    r.Expr,
			}
		}
		return out
	}
	if tr.MatchRule != "" {
		d := strings.TrimSpace(tr.MatchDialect)
		if d == "" {
			d = ruleengine.DialectCEL
		}
		return []triggerrule.Rule{{
			Dialect: d,
			Expr:    tr.MatchRule,
		}}
	}
	return nil
}

func triggerRulesToSpecs(rules []triggerrule.Rule) []TriggerMatchRuleSpec {
	out := make([]TriggerMatchRuleSpec, len(rules))
	for i, r := range rules {
		out[i] = TriggerMatchRuleSpec{
			Dialect: r.Dialect,
			Expr:    r.Expr,
		}
	}
	return out
}

func writeAmbiguousTriggerMatchError(w http.ResponseWriter) {
	writeError(
		w,
		http.StatusBadRequest,
		ErrCodeInvalidRule,
		"match cannot specify both rules and scalar dialect/expr",
		map[string]any{
			"field":    "match",
			"category": "ambiguous_match",
		},
	)
}

type triggerElementFailure struct {
	entry    map[string]any
	summary  string
	kind     ruleengine.Kind
	consumer string
}

func writeTriggerMatchElementErrors(w http.ResponseWriter, failures []triggerElementFailure) {
	elements := make([]map[string]any, len(failures))
	summaries := make([]string, len(failures))
	for i, f := range failures {
		elements[i] = f.entry
		summaries[i] = f.summary
	}
	first := failures[0]
	details := map[string]any{
		"field":    "match",
		"elements": elements,
	}
	if cat, hasCat := first.entry["category"]; hasCat {
		if first.kind != "" {
			details["kind"] = first.kind
		}
		details["category"] = cat
		details["message"] = first.entry["message"]
		details["line"] = first.entry["line"]
		details["column"] = first.entry["column"]
	} else if dialect, hasDialect := first.entry["dialect"]; hasDialect {
		details["dialect"] = dialect
		if first.consumer != "" {
			details["consumer"] = first.consumer
		}
		if reason, ok := first.entry["reason"]; ok && reason != "" {
			details["reason"] = reason
		}
	}
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRule, strings.Join(summaries, "; "), details)
}

// writeAndValidateTriggerMatch normalizes and compiles a trigger's match block
// at authoring time (create, PATCH, or resume; mixed-rule-arrays §2, §4).
// Scalar dialect/expr is accepted as sugar for a one-element list; mixing both
// rules and scalar dialect/expr returns 400 invalid_rule (ambiguous_match).
// Every element is validated and compiled, and ALL failing elements are
// reported together in details.elements[].
func (s *Server) writeAndValidateTriggerMatch(
	w http.ResponseWriter,
	ctx context.Context,
	spec *TriggerMatchSpec,
	defaultTypes, explicitScalarDialect bool,
) ([]string, []store.TriggerMatchRule, bool) {
	var types []string
	if spec != nil {
		types = spec.Types
	}
	if len(types) == 0 {
		if !defaultTypes {
			ValidationError(w, "match.types must contain at least 1 entry", nil)
			return nil, nil, false
		}
		types = []string{defaultWebhookMatchType}
	}
	if err := triggerrule.ValidateMatchTypes(types); err != nil {
		ValidationError(w, err.Error(), nil)
		return nil, nil, false
	}

	if spec != nil && spec.Rules != nil && (spec.Expr != "" || strings.TrimSpace(spec.Dialect) != "") {
		writeAmbiguousTriggerMatchError(w)
		return nil, nil, false
	}

	var rawRules []TriggerMatchRuleSpec
	switch {
	case spec == nil:
		rawRules = nil
	case spec.Rules != nil:
		rawRules = spec.Rules
	default:
		scalarDialect := strings.TrimSpace(spec.Dialect)
		if spec.Expr == "" {
			if explicitScalarDialect {
				if err := validateTriggerDialectName(scalarDialect); err != nil {
					writeTriggerMatchElementErrors(w, []triggerElementFailure{{
						entry: map[string]any{
							"element":  0,
							"category": "dialect",
							"message":  err.Error(),
							"line":     1,
							"column":   1,
						},
						summary: fmt.Sprintf("invalid match rule [element 0]: %s", err.Error()),
						kind:    ruleengine.KindFilter,
					}})
					return nil, nil, false
				}
				if err := s.compileTriggerMatchRule(ctx, scalarDialect, ""); err != nil {
					var unsup *ruleengine.UnsupportedDialectError
					if errors.As(err, &unsup) {
						msg := fmt.Sprintf("unsupported match dialect %q", unsup.Dialect)
						if unsup.Reason != "" {
							msg += ": " + unsup.Reason
						}
						writeTriggerMatchElementErrors(w, []triggerElementFailure{{
							entry: map[string]any{
								"element": 0,
								"dialect": unsup.Dialect,
								"reason":  unsup.Reason,
							},
							summary:  msg,
							consumer: unsup.Consumer,
						}})
						return nil, nil, false
					}
					writeTriggerRuleError(w, "match", err)
					return nil, nil, false
				}
				ValidationError(w, "match.dialect requires a non-empty match.expr", nil)
				return nil, nil, false
			}
			rawRules = nil
		} else {
			rawRules = []TriggerMatchRuleSpec{{
				Dialect: spec.Dialect,
				Expr:    spec.Expr,
			}}
		}
	}

	copiedTypes := make([]string, len(types))
	copy(copiedTypes, types)

	if len(rawRules) == 0 {
		// Verify the trigger rule engine is initialized and consumer "trigger" is admitted.
		if err := s.compileTriggerMatchRule(ctx, ruleengine.DialectCEL, ""); err != nil {
			writeTriggerRuleError(w, "match", err)
			return nil, nil, false
		}
		return copiedTypes, nil, true
	}

	if len(rawRules) > triggerrule.MaxMatchRules {
		writeError(
			w,
			http.StatusBadRequest,
			ErrCodeInvalidRule,
			fmt.Sprintf("match.rules exceeds maximum of %d elements (got %d)", triggerrule.MaxMatchRules, len(rawRules)),
			map[string]any{
				"field":        "match",
				"category":     "size",
				"max_elements": triggerrule.MaxMatchRules,
				"count":        len(rawRules),
			},
		)
		return nil, nil, false
	}

	normalized := make([]store.TriggerMatchRule, 0, len(rawRules))
	var failures []triggerElementFailure

	for i, elem := range rawRules {
		d := strings.TrimSpace(elem.Dialect)
		if d == "" {
			d = ruleengine.DialectCEL
		}
		if err := validateTriggerDialectName(d); err != nil {
			failures = append(failures, triggerElementFailure{
				entry: map[string]any{
					"element":  i,
					"category": "dialect",
					"message":  err.Error(),
					"line":     1,
					"column":   1,
				},
				summary: fmt.Sprintf("invalid match rule [element %d]: %s", i, err.Error()),
				kind:    ruleengine.KindFilter,
			})
			continue
		}
		if err := validateTriggerRuleExpr("match.expr", elem.Expr); err != nil {
			failures = append(failures, triggerElementFailure{
				entry: map[string]any{
					"element":  i,
					"category": "control_characters",
					"message":  err.Error(),
					"line":     1,
					"column":   1,
				},
				summary: fmt.Sprintf("invalid match rule [element %d]: %s", i, err.Error()),
				kind:    ruleengine.KindFilter,
			})
			continue
		}

		// Check dialect admission and engine initialization first so ungranted
		// dialects surface dialect_not_allowed even when expr is empty or oversize,
		// and an uninitialized engine returns HTTP 500 without leaking details.
		if err := s.compileTriggerMatchRule(ctx, d, ""); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				writeErrorFromErr(w, err, "")
				return nil, nil, false
			}
			var unsup *ruleengine.UnsupportedDialectError
			if errors.As(err, &unsup) {
				msg := fmt.Sprintf("unsupported match dialect %q [element %d]", unsup.Dialect, i)
				if unsup.Reason != "" {
					msg += ": " + unsup.Reason
				}
				failures = append(failures, triggerElementFailure{
					entry: map[string]any{
						"element": i,
						"dialect": unsup.Dialect,
						"reason":  unsup.Reason,
					},
					summary:  msg,
					consumer: unsup.Consumer,
				})
				continue
			}
			writeErrorFromErr(w, err, "")
			return nil, nil, false
		}

		if elem.Expr == "" {
			msg := "match rule expression must not be empty"
			failures = append(failures, triggerElementFailure{
				entry: map[string]any{
					"element":  i,
					"category": "empty",
					"message":  msg,
					"line":     1,
					"column":   1,
				},
				summary: fmt.Sprintf("invalid match rule [element %d]: %s", i, msg),
				kind:    ruleengine.KindFilter,
			})
			continue
		}
		if len(elem.Expr) > triggerrule.MaxMatchRuleBytes {
			msg := fmt.Sprintf("rule length %d exceeds maximum of %d bytes", len(elem.Expr), triggerrule.MaxMatchRuleBytes)
			failures = append(failures, triggerElementFailure{
				entry: map[string]any{
					"element":  i,
					"category": "size",
					"message":  msg,
					"line":     1,
					"column":   1,
				},
				summary: fmt.Sprintf("invalid match rule [element %d] at line 1, column 1 (size): %s", i, msg),
				kind:    ruleengine.KindFilter,
			})
			continue
		}

		if err := s.compileTriggerMatchRule(ctx, d, elem.Expr); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				writeErrorFromErr(w, err, "")
				return nil, nil, false
			}
			var ce *celengine.CompileError
			if errors.As(err, &ce) {
				failures = append(failures, triggerElementFailure{
					entry: map[string]any{
						"element":  i,
						"category": ce.Category,
						"message":  ce.Message,
						"line":     ce.Line,
						"column":   ce.Column,
					},
					summary: fmt.Sprintf("invalid match rule [element %d] at line %d, column %d (%s): %s",
						i, ce.Line, ce.Column, ce.Category, ce.Message),
					kind: ruleengine.Kind(ce.Kind),
				})
				continue
			}
			var unsup *ruleengine.UnsupportedDialectError
			if errors.As(err, &unsup) {
				msg := fmt.Sprintf("unsupported match dialect %q [element %d]", unsup.Dialect, i)
				if unsup.Reason != "" {
					msg += ": " + unsup.Reason
				}
				failures = append(failures, triggerElementFailure{
					entry: map[string]any{
						"element": i,
						"dialect": unsup.Dialect,
						"reason":  unsup.Reason,
					},
					summary:  msg,
					consumer: unsup.Consumer,
				})
				continue
			}
			writeErrorFromErr(w, err, "")
			return nil, nil, false
		}

		normalized = append(normalized, store.TriggerMatchRule{
			Dialect: d,
			Expr:    elem.Expr,
		})
	}

	if len(failures) > 0 {
		writeTriggerMatchElementErrors(w, failures)
		return nil, nil, false
	}

	encoded, err := json.Marshal(normalized)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return nil, nil, false
	}
	if len(encoded) > triggerrule.MaxMatchRulesJSONBytes {
		writeError(
			w,
			http.StatusBadRequest,
			ErrCodeInvalidRule,
			fmt.Sprintf("encoded match.rules length %d exceeds maximum of %d bytes", len(encoded), triggerrule.MaxMatchRulesJSONBytes),
			map[string]any{
				"field":     "match",
				"category":  "size",
				"max_bytes": triggerrule.MaxMatchRulesJSONBytes,
				"bytes":     len(encoded),
			},
		)
		return nil, nil, false
	}

	return copiedTypes, normalized, true
}

// writeAndValidateTriggerSubject normalizes and compiles a trigger's subject
// block at authoring time (create, PATCH, or resume). When explicitDialect is
// true, an empty expr is refused as a malformed request.
func (s *Server) writeAndValidateTriggerSubject(w http.ResponseWriter, ctx context.Context, spec *TriggerSubjectSpec, explicitDialect bool) (string, string, string, bool) {
	dialect := ruleengine.DialectCEL
	expr := ""
	mode := store.TriggerSubjectModeNew
	if spec != nil {
		if d := strings.TrimSpace(spec.Dialect); d != "" {
			dialect = d
		}
		expr = spec.Expr
		if m := strings.TrimSpace(spec.Mode); m != "" {
			mode = m
		}
	}
	switch mode {
	case store.TriggerSubjectModeNew:
	case store.TriggerSubjectModeSkip:
		ValidationError(w, `subject_mode "skip" is not yet supported`, nil)
		return "", "", "", false
	default:
		ValidationError(w, fmt.Sprintf("unsupported subject mode %q", mode), nil)
		return "", "", "", false
	}
	if err := validateTriggerDialectName(dialect); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRule, "invalid subject dialect: "+err.Error(), map[string]any{
			"field":    "subject",
			"category": "dialect",
			"message":  err.Error(),
			"line":     1,
			"column":   1,
		})
		return "", "", "", false
	}
	if err := validateTriggerRuleExpr("subject.expr", expr); err != nil {
		ValidationError(w, err.Error(), nil)
		return "", "", "", false
	}
	if err := s.compileTriggerSubjectRule(ctx, dialect, expr); err != nil {
		writeTriggerRuleError(w, "subject", err)
		return "", "", "", false
	}
	if explicitDialect && expr == "" {
		ValidationError(w, "subject.dialect requires a non-empty subject.expr", nil)
		return "", "", "", false
	}
	return dialect, expr, mode, true
}

// writeTriggerRuleError maps authoring-time rule compilation and dialect-admission
// errors to HTTP 400 ErrCodeInvalidRule responses carrying structured CompileError
// or UnsupportedDialectError details. Unexpected internal errors (such as an
// uninitialized rule engine) fall through to writeErrorFromErr, which logs the
// cause and returns a generic HTTP 500 without echoing internal error text.
func writeTriggerRuleError(w http.ResponseWriter, slot string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeErrorFromErr(w, err, "")
		return
	}

	var ce *celengine.CompileError
	if errors.As(err, &ce) {
		msg := fmt.Sprintf("invalid %s rule at line %d, column %d (%s): %s", slot, ce.Line, ce.Column, ce.Category, ce.Message)
		details := map[string]any{
			"field":    slot,
			"kind":     ce.Kind,
			"category": ce.Category,
			"message":  ce.Message,
			"line":     ce.Line,
			"column":   ce.Column,
		}
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRule, msg, details)
		return
	}

	var unsup *ruleengine.UnsupportedDialectError
	if errors.As(err, &unsup) {
		details := map[string]any{
			"field":   slot,
			"dialect": unsup.Dialect,
		}
		if unsup.Consumer != "" {
			details["consumer"] = unsup.Consumer
		}
		if unsup.Reason != "" {
			details["reason"] = unsup.Reason
		}
		msg := fmt.Sprintf("unsupported %s dialect %q", slot, unsup.Dialect)
		if unsup.Reason != "" {
			msg += ": " + unsup.Reason
		}
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRule, msg, details)
		return
	}

	writeErrorFromErr(w, err, "")
}
