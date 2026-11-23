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
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	s1spec "github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// Authoring validation error codes (brief §§1, 1a, 3, 5).
const (
	ErrCodeValidation         = "validation_error"
	ErrCodeNotYetSupported    = "not_yet_supported"
	ErrCodeSurfaceNotCovered  = "surface_not_covered"
	ErrCodeInvalidRule        = "invalid_rule"
	ErrCodeUnsupportedDialect = "unsupported_dialect"
)

// ValidationError carries structured authoring-time validation details so HTTP
// handlers can return precise 400 error payloads (including CEL compile line,
// column, and category; targets 4a, 5, 6).
type ValidationError struct {
	Code     string
	Field    string
	Path     string
	Message  string
	Line     int
	Column   int
	Category string
	Cause    error
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Field != "" && e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Field, e.Message)
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// CoverageInfo reports which surfaces are enforced in the current build and
// which agent-authored messaging paths bypass message policies in M1
// (consumer design §§5.2, 11; target 6).
type CoverageInfo struct {
	EnforcedSurfaces    []string `json:"enforced_surfaces"`
	UncoveredAgentPaths []string `json:"uncovered_agent_paths"`
}

// Coverage returns the M1 enforcement coverage declaration (H2 only).
func Coverage() CoverageInfo {
	return CoverageInfo{
		EnforcedSurfaces: []string{
			SurfaceAgentToUser,
			SurfaceAgentToConversation,
		},
		UncoveredAgentPaths: []string{
			SurfaceAgentDM,
			SurfaceGroupPost,
			SurfaceGroupSet,
			SurfaceMentionFanout,
			SurfaceMentionDelivery,
			"broadcast",
		},
	}
}

// IsCoveredSurface reports whether surface has an active enforcement hook in M1.
func IsCoveredSurface(surface string) bool {
	return surface == SurfaceAgentToUser || surface == SurfaceAgentToConversation
}

func surfaceRecipientKind(surface string) string {
	switch surface {
	case SurfaceAgentToUser:
		return "user"
	case SurfaceAgentToConversation:
		return "conversation"
	default:
		return ""
	}
}

// IsCanonicalSlug reports whether s is a canonical lowercase ASCII slug of
// length 1..MaxPolicyNameLen (`^[a-z0-9]+(-[a-z0-9]+)*$`).
func IsCanonicalSlug(s string) bool {
	if s == "" || len(s) > MaxPolicyNameLen {
		return false
	}
	prevHyphen := true
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '-' {
			if prevHyphen || i == len(s)-1 {
				return false
			}
			prevHyphen = true
			continue
		}
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') {
			prevHyphen = false
			continue
		}
		return false
	}
	return !prevHyphen
}

var knownFailureReasons = func() []string {
	base := []string{
		celengine.ReasonCanceled,
		celengine.ReasonCostLimit,
		celengine.ReasonMissingInput,
		celengine.ReasonRuntime,
		celengine.ReasonTimeout,
		celengine.ReasonType,
		ReasonBodyTruncated,
		ReasonBudgetExhausted,
		ReasonCompileError,
		ReasonFactsUnavailable,
		ReasonInvalidCompiledProgram,
		ReasonPolicyCapExceeded,
		ReasonUninitializedProgram,
		ReasonUnsupportedDialect,
		ruleengine.ReasonConsumerNotRegistered,
		ruleengine.ReasonDialectNotAllowed,
		ruleengine.ReasonEmptyConsumer,
		ruleengine.ReasonIONotAllowed,
		ruleengine.ReasonUnknownDialect,
		systemone.ReasonDialectDisabled,
	}
	seen := make(map[string]struct{}, len(base)+len(provider.AllFailReasons()))
	out := make([]string, 0, len(base)+len(provider.AllFailReasons()))
	for _, r := range base {
		if _, dup := seen[r]; !dup {
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}
	for _, fr := range provider.AllFailReasons() {
		r := string(fr)
		if _, dup := seen[r]; !dup {
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}()

var knownFailureReasonSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(knownFailureReasons))
	for _, r := range knownFailureReasons {
		m[r] = struct{}{}
	}
	return m
}()

// KnownFailureReasons returns a sorted copy of the closed failure-reason set
// accepted by on_failed_reasons in M1 (CEL evaluation, ruleengine program and
// consumer admission, and message_policy consumer failure reasons; consumer
// design §5.3).
func KnownFailureReasons() []string {
	out := slices.Clone(knownFailureReasons)
	slices.Sort(out)
	return out
}

// IsValidFailureReason reports whether reason belongs to the closed M1 failure
// reason set accepted as an on_failed_reasons key.
func IsValidFailureReason(reason string) bool {
	_, ok := knownFailureReasonSet[reason]
	return ok
}

// SenderSelector mirrors policy sender scope for authoring validation.
type SenderSelector struct {
	AgentIDs  []string
	AllAgents bool
}

// RecipientSelector mirrors policy recipient scope for authoring validation.
type RecipientSelector struct {
	Kinds    []string
	AgentIDs []string
}

// Scope mirrors policy scope for authoring validation.
type Scope struct {
	Surfaces  []string
	Sender    SenderSelector
	Recipient RecipientSelector
}

// AuthoringSpec carries the author-supplied fields of a message policy for
// validation and compilation at create/update/test time.
type AuthoringSpec struct {
	Name              string
	Mode              string
	Scope             Scope
	ViolationRule     string
	SystemOneRule     string
	OnUncertain       string
	OnFailed          string
	OnFailedReasons   map[string]string
	RejectReason      string
	RejectRemediation string
}

// ValidateAuthoring validates spec against M1 authoring rules and compiles
// spec.ViolationRule against the message_policy v1 CEL environment, returning
// the normalized Mode ("shadow" when spec.Mode is empty) or a *ValidationError.
func (e *Evaluator) ValidateAuthoring(ctx context.Context, spec AuthoringSpec) (string, error) {
	if !IsCanonicalSlug(spec.Name) {
		return "", &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "name",
			Message: fmt.Sprintf("name must be a canonical slug of 1..%d lowercase letters, digits, or single hyphens", MaxPolicyNameLen),
		}
	}

	mode := strings.TrimSpace(spec.Mode)
	if mode == "" {
		mode = ModeShadow
	}
	if mode != ModeShadow && mode != ModeEnforce {
		return "", &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "mode",
			Message: fmt.Sprintf("mode must be %q or %q", ModeShadow, ModeEnforce),
		}
	}

	hasSystemOne := strings.TrimSpace(spec.SystemOneRule) != ""
	if hasSystemOne && len(spec.SystemOneRule) > MaxSystemOneRuleBytes {
		return "", &ValidationError{
			Code:     ErrCodeInvalidRule,
			Field:    "system_one_rule",
			Category: string(s1spec.CategoryLimit),
			Message:  fmt.Sprintf("system_one_rule length %d exceeds maximum %d bytes", len(spec.SystemOneRule), MaxSystemOneRuleBytes),
		}
	}

	if len(spec.Scope.Surfaces) == 0 {
		return "", &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "scope.surfaces",
			Message: "at least one surface is required",
		}
	}
	for _, s := range spec.Scope.Surfaces {
		if !IsCoveredSurface(s) {
			return "", &ValidationError{
				Code:    ErrCodeSurfaceNotCovered,
				Field:   "scope.surfaces",
				Message: fmt.Sprintf("surface %q is not covered in M1 (covered surfaces: %s, %s)", s, SurfaceAgentToUser, SurfaceAgentToConversation),
			}
		}
	}

	if len(spec.Scope.Recipient.AgentIDs) > 0 {
		return "", &ValidationError{
			Code:    ErrCodeSurfaceNotCovered,
			Field:   "scope.recipient.agent_ids",
			Message: "agent recipient selectors are not covered in M1",
		}
	}
	for _, k := range spec.Scope.Recipient.Kinds {
		if k == "agent" {
			return "", &ValidationError{
				Code:    ErrCodeSurfaceNotCovered,
				Field:   "scope.recipient.kinds",
				Message: `recipient kind "agent" is not covered in M1`,
			}
		}
		if k != "user" && k != "conversation" {
			return "", &ValidationError{
				Code:    ErrCodeValidation,
				Field:   "scope.recipient.kinds",
				Message: fmt.Sprintf("invalid recipient kind %q (must be \"user\" or \"conversation\")", k),
			}
		}
	}
	if len(spec.Scope.Recipient.Kinds) > 0 {
		reachable := false
		for _, s := range spec.Scope.Surfaces {
			if rk := surfaceRecipientKind(s); rk != "" && slices.Contains(spec.Scope.Recipient.Kinds, rk) {
				reachable = true
				break
			}
		}
		if !reachable {
			return "", &ValidationError{
				Code:  ErrCodeValidation,
				Field: "scope.recipient.kinds",
				Message: fmt.Sprintf("none of scope.recipient.kinds (%s) can match scope.surfaces (%s)",
					strings.Join(spec.Scope.Recipient.Kinds, ", "),
					strings.Join(spec.Scope.Surfaces, ", ")),
			}
		}
	}

	if spec.OnFailed != OutcomeAdmit && spec.OnFailed != OutcomeReject {
		return "", &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "on_failed",
			Message: fmt.Sprintf("on_failed is required and must be %q or %q", OutcomeAdmit, OutcomeReject),
		}
	}
	if hasSystemOne {
		if spec.OnUncertain != OutcomeAdmit && spec.OnUncertain != OutcomeReject {
			return "", &ValidationError{
				Code:    ErrCodeValidation,
				Field:   "on_uncertain",
				Message: fmt.Sprintf("on_uncertain is required when system_one_rule is set and must be %q or %q", OutcomeAdmit, OutcomeReject),
			}
		}
	} else if spec.OnUncertain != "" && spec.OnUncertain != OutcomeAdmit && spec.OnUncertain != OutcomeReject {
		return "", &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "on_uncertain",
			Message: fmt.Sprintf("on_uncertain must be %q or %q when specified", OutcomeAdmit, OutcomeReject),
		}
	}
	for _, reason := range slices.Sorted(maps.Keys(spec.OnFailedReasons)) {
		outcome := spec.OnFailedReasons[reason]
		if !IsValidFailureReason(reason) {
			return "", &ValidationError{
				Code:    ErrCodeValidation,
				Field:   "on_failed_reasons",
				Message: fmt.Sprintf("unknown on_failed_reasons key %q", reason),
			}
		}
		if outcome != OutcomeAdmit && outcome != OutcomeReject {
			return "", &ValidationError{
				Code:    ErrCodeValidation,
				Field:   "on_failed_reasons",
				Message: fmt.Sprintf("on_failed_reasons entry %q must map to %q or %q", reason, OutcomeAdmit, OutcomeReject),
			}
		}
	}

	if err := ValidateTemplate("reject_reason", spec.RejectReason); err != nil {
		return "", err
	}
	if err := ValidateTemplate("reject_remediation", spec.RejectRemediation); err != nil {
		return "", err
	}

	if strings.TrimSpace(spec.ViolationRule) == "" {
		return "", &ValidationError{
			Code:    ErrCodeInvalidRule,
			Field:   "violation_rule",
			Message: "violation_rule is required",
		}
	}
	if err := e.CompileViolation(ctx, spec.ViolationRule); err != nil {
		var compErr *celengine.CompileError
		if errors.As(err, &compErr) {
			return "", &ValidationError{
				Code:     ErrCodeInvalidRule,
				Field:    "violation_rule",
				Message:  compErr.Message,
				Line:     compErr.Line,
				Column:   compErr.Column,
				Category: compErr.Category,
				Cause:    err,
			}
		}
		var unsupErr *ruleengine.UnsupportedDialectError
		if errors.As(err, &unsupErr) {
			return "", &ValidationError{
				Code:     ErrCodeUnsupportedDialect,
				Field:    "violation_rule",
				Message:  unsupErr.Error(),
				Category: unsupErr.Reason,
				Cause:    err,
			}
		}
		return "", &ValidationError{
			Code:    ErrCodeInvalidRule,
			Field:   "violation_rule",
			Message: err.Error(),
			Cause:   err,
		}
	}

	if hasSystemOne {
		if _, err := e.CompileSystemOne(ctx, spec.SystemOneRule); err != nil {
			if compErr, ok := s1spec.AsCompileError(err); ok {
				code := ErrCodeInvalidRule
				if compErr.Category == s1spec.CategoryCapability {
					code = ErrCodeNotYetSupported
				}
				return "", &ValidationError{
					Code:     code,
					Field:    "system_one_rule",
					Path:     compErr.Path,
					Message:  compErr.Message,
					Category: string(compErr.Category),
					Cause:    err,
				}
			}
			var unsupErr *ruleengine.UnsupportedDialectError
			if errors.As(err, &unsupErr) {
				return "", &ValidationError{
					Code:     ErrCodeUnsupportedDialect,
					Field:    "system_one_rule",
					Message:  unsupErr.Error(),
					Category: unsupErr.Reason,
					Cause:    err,
				}
			}
			return "", &ValidationError{
				Code:    ErrCodeInvalidRule,
				Field:   "system_one_rule",
				Message: err.Error(),
				Cause:   err,
			}
		}
	}

	return mode, nil
}
