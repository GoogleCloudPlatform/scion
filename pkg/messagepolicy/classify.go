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

package messagepolicy

import (
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	s1spec "github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// Classification describes the outcome class and reason of evaluating one
// message policy rule (consumer design §§4.3, 5.5).
type Classification struct {
	// Class is one of ClassPass, ClassViolation, ClassUncertain, or ClassFailed.
	Class string

	// Reason carries the failure or uncertainty reason when Class is ClassFailed
	// or ClassUncertain (e.g. "budget_exhausted", "missing_input", "cost_limit",
	// "timeout", "canceled", "runtime", "type", or an admission reason). Empty
	// when Class is ClassPass or ClassViolation.
	Reason string

	// AdmissionError reports whether the failure came from ruleengine consumer
	// admission (*ruleengine.UnsupportedDialectError), indicating a server
	// misconfiguration at evaluation time (brief §1a).
	AdmissionError bool

	// Stats holds content-free CEL evaluation metrics when available.
	Stats celengine.EvalStats

	// Evaluation holds the content-free System One evaluation record when a
	// policy evaluated a system_one_rule (consumer design §5.8).
	Evaluation *systemone.Evaluation

	// Judgments holds the typed flags/labels extracted from a successful
	// System One evaluation for template substitution (consumer design §5.3).
	Judgments Judgments
}

// ClassifyDecision classifies a ruleengine evaluation result or error.
func ClassifyDecision(matched bool, stats celengine.EvalStats, err error) Classification {
	if err != nil {
		c := ClassifyError(err)
		c.Stats = stats
		return c
	}
	if matched {
		return Classification{
			Class: ClassViolation,
			Stats: stats,
		}
	}
	return Classification{
		Class: ClassPass,
		Stats: stats,
	}
}

// ClassifyError maps a non-nil evaluation or compilation error to its
// Classification using errors.Is and errors.As so wrapped ruleengine errors
// (fmt.Errorf("%w: %w", ...) and errors.Join) are always unpacked (brief §1,
// target 4c).
func ClassifyError(err error) Classification {
	if err == nil {
		return Classification{Class: ClassPass}
	}
	eval, _ := systemone.EvaluationFrom(err)
	if errors.Is(err, ruleengine.ErrBudgetExhausted) {
		return Classification{
			Class:      ClassFailed,
			Reason:     ReasonBudgetExhausted,
			Evaluation: eval,
		}
	}

	var uncertainErr *ruleengine.UncertainError
	if errors.As(err, &uncertainErr) {
		reason := uncertainErr.Reason
		if reason == "" {
			reason = "uncertain"
		}
		return Classification{
			Class:      ClassUncertain,
			Reason:     reason,
			Evaluation: eval,
		}
	}

	var failedErr *ruleengine.FailedError
	if errors.As(err, &failedErr) {
		reason := failedErr.Reason
		if reason == "" {
			reason = celengine.ReasonRuntime
		}
		return Classification{
			Class:      ClassFailed,
			Reason:     reason,
			Evaluation: eval,
		}
	}

	var evalErr *celengine.EvalError
	if errors.As(err, &evalErr) {
		reason := evalErr.Reason
		if reason == "" {
			reason = celengine.ReasonRuntime
		}
		return Classification{
			Class:      ClassFailed,
			Reason:     reason,
			Evaluation: eval,
		}
	}

	var unsupErr *ruleengine.UnsupportedDialectError
	if errors.As(err, &unsupErr) {
		reason := unsupErr.Reason
		if reason == "" {
			reason = ReasonUnsupportedDialect
		}
		return Classification{
			Class:          ClassFailed,
			Reason:         reason,
			AdmissionError: true,
			Evaluation:     eval,
		}
	}

	var compErr *celengine.CompileError
	if errors.As(err, &compErr) {
		return Classification{
			Class:      ClassFailed,
			Reason:     ReasonCompileError,
			Evaluation: eval,
		}
	}

	var s1CompErr *s1spec.CompileError
	if errors.As(err, &s1CompErr) {
		reason := ReasonCompileError
		if s1CompErr.Category == s1spec.CategoryBinding {
			reason = string(systemoneprovider.FailNoEligibleInstance)
		}
		return Classification{
			Class:      ClassFailed,
			Reason:     reason,
			Evaluation: eval,
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return Classification{
			Class:      ClassFailed,
			Reason:     celengine.ReasonTimeout,
			Evaluation: eval,
		}
	}
	if errors.Is(err, context.Canceled) {
		return Classification{
			Class:      ClassFailed,
			Reason:     celengine.ReasonCanceled,
			Evaluation: eval,
		}
	}
	return Classification{
		Class:      ClassFailed,
		Reason:     celengine.ReasonRuntime,
		Evaluation: eval,
	}
}

// ResolveOutcome maps a Classification to OutcomeAdmit or OutcomeReject using
// only the policy's configured onUncertain, onFailed, and onFailedReasons
// overrides (consumer design §§4.4, 5.5; target 4).
func ResolveOutcome(class, reason, onUncertain, onFailed string, onFailedReasons map[string]string) string {
	switch class {
	case ClassPass:
		return OutcomeAdmit
	case ClassViolation:
		return OutcomeReject
	case ClassUncertain:
		if onUncertain == OutcomeAdmit || onUncertain == OutcomeReject {
			return onUncertain
		}
		return OutcomeReject
	default:
		if reason != "" && len(onFailedReasons) > 0 {
			if ov, ok := onFailedReasons[reason]; ok && (ov == OutcomeAdmit || ov == OutcomeReject) {
				return ov
			}
		}
		if onFailed == OutcomeAdmit || onFailed == OutcomeReject {
			return onFailed
		}
		return OutcomeReject
	}
}
