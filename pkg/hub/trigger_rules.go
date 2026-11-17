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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

var errTriggerRuleEngineNotInitialized = errors.New("trigger rule engine is not initialized")

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

// writeAndValidateTriggerMatch normalizes and compiles a trigger's match block
// at authoring time (create, PATCH, or resume). When defaultTypes is true and
// spec omits types, types defaults to ["webhook.received"]. When
// explicitDialect is true, an empty expr is refused as a malformed request
// (a non-empty dialect requires a rule expression).
func (s *Server) writeAndValidateTriggerMatch(w http.ResponseWriter, ctx context.Context, spec *TriggerMatchSpec, defaultTypes, explicitDialect bool) ([]string, string, string, bool) {
	var types []string
	dialect := ruleengine.DialectCEL
	expr := ""
	if spec != nil {
		types = spec.Types
		if d := strings.TrimSpace(spec.Dialect); d != "" {
			dialect = d
		}
		expr = spec.Expr
	}
	if len(types) == 0 {
		if !defaultTypes {
			ValidationError(w, "match.types must contain at least 1 entry", nil)
			return nil, "", "", false
		}
		types = []string{defaultWebhookMatchType}
	}
	if err := triggerrule.ValidateMatchTypes(types); err != nil {
		ValidationError(w, err.Error(), nil)
		return nil, "", "", false
	}
	if err := validateTriggerRuleExpr("match.expr", expr); err != nil {
		ValidationError(w, err.Error(), nil)
		return nil, "", "", false
	}
	if err := s.compileTriggerMatchRule(ctx, dialect, expr); err != nil {
		writeTriggerRuleError(w, "match", err)
		return nil, "", "", false
	}
	if explicitDialect && expr == "" {
		ValidationError(w, "match.dialect requires a non-empty match.expr", nil)
		return nil, "", "", false
	}
	copied := make([]string, len(types))
	copy(copied, types)
	return copied, dialect, expr, true
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
