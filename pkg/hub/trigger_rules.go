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
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
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
		triggerrule.Stage1CacheConfig(),
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

// invalidateTriggerRuleCache removes cached compiled programs bound to
// (projectID, triggerID, revision) across registered dialects (3b-1 F4).
// Passing revision <= 0 invalidates every revision of triggerID.
func (s *Server) invalidateTriggerRuleCache(projectID, triggerID string, revision int64) {
	if s == nil || s.triggerRules == nil || triggerID == "" {
		return
	}
	scope := celengine.CacheScope{
		Consumer:  triggerrule.ConsumerName,
		ProjectID: projectID,
		ID:        triggerID,
		Revision:  revision,
	}
	s.invalidateTriggerScopeAllDialects(scope)
}

// invalidateTriggerRuleCacheForProject removes all cached trigger programs
// scoped to projectID (3b-1 F4 / project cascade deletion).
func (s *Server) invalidateTriggerRuleCacheForProject(projectID string) {
	if s == nil || s.triggerRules == nil || projectID == "" {
		return
	}
	s.invalidateTriggerScopeAllDialects(celengine.CacheScope{
		Consumer:  triggerrule.ConsumerName,
		ProjectID: projectID,
	})
}

func (s *Server) invalidateTriggerScopeAllDialects(scope celengine.CacheScope) {
	if s.rules != nil {
		for _, d := range s.rules.Dialects() {
			s.triggerRules.Invalidate(d, scope)
		}
		return
	}
	s.triggerRules.Invalidate(ruleengine.DialectCEL, scope)
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

func isDisallowedTriggerRuleExprRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

func validateTriggerRuleExpr(field, expr string) error {
	if !utf8.ValidString(expr) || strings.ContainsFunc(expr, isDisallowedTriggerRuleExprRune) {
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
// (mixed-rule-arrays §2.1). Normalization fails closed: if MatchRulesCorrupt is
// set, it returns errCorruptTriggerMatchRules so a corrupt column never
// degrades to a Types-only ("no rule") match. Otherwise non-empty MatchRules
// wins; else a defensive non-empty scalar MatchRule normalizes to a one-element
// slice with MatchDialect (defaulting to "cel"); else (nil, nil).
func triggerMatchRules(tr store.Trigger) ([]triggerrule.Rule, error) {
	return trigger.MatchRules(tr)
}

func hasAmbiguousScalarWithRules(dialect, expr string) bool {
	return strings.TrimSpace(dialect) != "" || strings.TrimSpace(expr) != ""
}

func writeCorruptTriggerMatchRulesError(w http.ResponseWriter) {
	const msg = "stored match_rules data is corrupt; replace match.rules before updating dispatch fields"
	writeTriggerMatchElementErrors(w, []triggerElementFailure{{
		entry: map[string]any{
			"element":  1,
			"category": "corrupt_storage",
			"message":  msg,
			"line":     1,
			"column":   1,
		},
		summary: "invalid match rule [element 1]: " + msg,
		kind:    ruleengine.KindFilter,
	}})
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

	if spec != nil && spec.Rules != nil && hasAmbiguousScalarWithRules(spec.Dialect, spec.Expr) {
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
							"element":  1,
							"category": "dialect",
							"message":  err.Error(),
							"line":     1,
							"column":   1,
						},
						summary: fmt.Sprintf("invalid match rule [element 1]: %s", err.Error()),
						kind:    ruleengine.KindFilter,
					}})
					return nil, nil, false
				}
				if err := s.compileTriggerMatchRule(ctx, scalarDialect, ""); err != nil {
					var unsup *ruleengine.UnsupportedDialectError
					if errors.As(err, &unsup) {
						msg := fmt.Sprintf("unsupported match dialect %q [element 1]", unsup.Dialect)
						if unsup.Reason != "" {
							msg += ": " + unsup.Reason
						}
						writeTriggerMatchElementErrors(w, []triggerElementFailure{{
							entry: map[string]any{
								"element": 1,
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

	normalized := make([]store.TriggerMatchRule, len(rawRules))
	for i, elem := range rawRules {
		d := strings.TrimSpace(elem.Dialect)
		if d == "" {
			d = ruleengine.DialectCEL
		}
		normalized[i] = store.TriggerMatchRule{
			Dialect: d,
			Expr:    elem.Expr,
		}
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

	var failures []triggerElementFailure

	for i, elem := range normalized {
		elemNum := i + 1
		d := elem.Dialect
		if err := validateTriggerDialectName(d); err != nil {
			failures = append(failures, triggerElementFailure{
				entry: map[string]any{
					"element":  elemNum,
					"category": "dialect",
					"message":  err.Error(),
					"line":     1,
					"column":   1,
				},
				summary: fmt.Sprintf("invalid match rule [element %d]: %s", elemNum, err.Error()),
				kind:    ruleengine.KindFilter,
			})
			continue
		}
		if err := validateTriggerRuleExpr("match.expr", elem.Expr); err != nil {
			failures = append(failures, triggerElementFailure{
				entry: map[string]any{
					"element":  elemNum,
					"category": "control_characters",
					"message":  err.Error(),
					"line":     1,
					"column":   1,
				},
				summary: fmt.Sprintf("invalid match rule [element %d]: %s", elemNum, err.Error()),
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
				msg := fmt.Sprintf("unsupported match dialect %q [element %d]", unsup.Dialect, elemNum)
				if unsup.Reason != "" {
					msg += ": " + unsup.Reason
				}
				failures = append(failures, triggerElementFailure{
					entry: map[string]any{
						"element": elemNum,
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
					"element":  elemNum,
					"category": "empty",
					"message":  msg,
					"line":     1,
					"column":   1,
				},
				summary: fmt.Sprintf("invalid match rule [element %d]: %s", elemNum, msg),
				kind:    ruleengine.KindFilter,
			})
			continue
		}
		if len(elem.Expr) > triggerrule.MaxMatchRuleBytes {
			msg := fmt.Sprintf("rule length %d exceeds maximum of %d bytes", len(elem.Expr), triggerrule.MaxMatchRuleBytes)
			failures = append(failures, triggerElementFailure{
				entry: map[string]any{
					"element":  elemNum,
					"category": "size",
					"message":  msg,
					"line":     1,
					"column":   1,
				},
				summary: fmt.Sprintf("invalid match rule [element %d] at line 1, column 1 (size): %s", elemNum, msg),
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
						"element":  elemNum,
						"category": ce.Category,
						"message":  ce.Message,
						"line":     ce.Line,
						"column":   ce.Column,
					},
					summary: fmt.Sprintf("invalid match rule [element %d] at line %d, column %d (%s): %s",
						elemNum, ce.Line, ce.Column, ce.Category, ce.Message),
					kind: ruleengine.Kind(ce.Kind),
				})
				continue
			}
			var unsup *ruleengine.UnsupportedDialectError
			if errors.As(err, &unsup) {
				msg := fmt.Sprintf("unsupported match dialect %q [element %d]", unsup.Dialect, elemNum)
				if unsup.Reason != "" {
					msg += ": " + unsup.Reason
				}
				failures = append(failures, triggerElementFailure{
					entry: map[string]any{
						"element": elemNum,
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
	}

	if len(failures) > 0 {
		writeTriggerMatchElementErrors(w, failures)
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
	case store.TriggerSubjectModeNew, store.TriggerSubjectModeSkip:
	default:
		ValidationError(w, fmt.Sprintf("unsupported subject mode %q (supported: %s, %s)", mode, store.TriggerSubjectModeNew, store.TriggerSubjectModeSkip), nil)
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
