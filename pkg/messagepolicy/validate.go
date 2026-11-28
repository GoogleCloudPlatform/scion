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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	s1spec "github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// Authoring validation error codes (brief §§1, 1a, 3, 5; SP1 §1.2).
const (
	ErrCodeValidation           = "validation_error"
	ErrCodeNotYetSupported      = "not_yet_supported"
	ErrCodeSurfaceNotCovered    = "surface_not_covered"
	ErrCodeInvalidRule          = "invalid_rule"
	ErrCodeUnsupportedDialect   = "unsupported_dialect"
	ErrCodeRoleDialectMismatch  = "role_dialect_mismatch"
	ErrCodeChainHasNoVerdict    = "chain_has_no_verdict"
	ErrCodeForwardReference     = "forward_reference"
	ErrCodeDynamicStepReference = "dynamic_step_reference"
	ErrCodeReservedName         = "reserved_name"
)

var (
	chainStepIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// reservedLegacyJudgmentNames are unavailable in new chain CEL steps (A1 §5).
	reservedLegacyJudgmentNames = []string{
		"judgment_flags",
		"judgment_labels",
		"judgment_scores",
	}
)

// IsValidChainStepID reports whether id matches ^[a-z][a-z0-9_]{0,31}$.
func IsValidChainStepID(id string) bool {
	return chainStepIDPattern.MatchString(id)
}

// IsP1ChainSurface reports whether surface is supported for chain-native
// policies in P1 (E1/E2 origin surfaces only: agent_to_user, agent_to_conversation).
func IsP1ChainSurface(surface string) bool {
	return surface == SurfaceAgentToUser || surface == SurfaceAgentToConversation
}

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

// Coverage returns the active enforcement coverage declaration (H2 + M3 Slice B
// agent-recipient egress: agent_dm and mention_delivery).
func Coverage() CoverageInfo {
	return CoverageInfo{
		EnforcedSurfaces: []string{
			SurfaceAgentToUser,
			SurfaceAgentToConversation,
			SurfaceAgentDM,
			SurfaceMentionDelivery,
		},
		UncoveredAgentPaths: []string{
			SurfaceGroupPost,
			SurfaceGroupSet,
			SurfaceMentionFanout,
			"broadcast",
		},
	}
}

// IsCoveredSurface reports whether surface has an active enforcement hook.
func IsCoveredSurface(surface string) bool {
	switch surface {
	case SurfaceAgentToUser, SurfaceAgentToConversation, SurfaceAgentDM, SurfaceMentionDelivery:
		return true
	default:
		return false
	}
}

func surfaceRecipientKind(surface string) string {
	switch surface {
	case SurfaceAgentToUser:
		return "user"
	case SurfaceAgentToConversation:
		return "conversation"
	case SurfaceAgentDM, SurfaceMentionDelivery:
		return "agent"
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

// knownCELFailureReasons is the M1 closed failure-reason set accepted by
// on_failed_reasons for CEL-only policies (strict M1 parity).
var knownCELFailureReasons = func() []string {
	out := []string{
		celengine.ReasonCanceled,
		celengine.ReasonCostLimit,
		celengine.ReasonMissingInput,
		celengine.ReasonRuntime,
		celengine.ReasonTimeout,
		celengine.ReasonType,
		ReasonBodyTruncated,
		ReasonBudgetExhausted,
		ReasonChainDisabled,
		ReasonChainUnsupported,
		ReasonCompileError,
		ReasonFactsUnavailable,
		ReasonInvalidCompiledProgram,
		ReasonPolicyCapExceeded,
		ReasonSystemOneCapExceeded,
		ReasonUninitializedProgram,
		ReasonUnsupportedDialect,
		ruleengine.ReasonConsumerNotRegistered,
		ruleengine.ReasonDialectNotAllowed,
		ruleengine.ReasonEmptyConsumer,
		ruleengine.ReasonIONotAllowed,
		ruleengine.ReasonUnknownDialect,
	}
	slices.Sort(out)
	return out
}()

var knownCELFailureReasonSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(knownCELFailureReasons))
	for _, r := range knownCELFailureReasons {
		m[r] = struct{}{}
	}
	return m
}()

var knownFailureReasons = func() []string {
	seen := make(map[string]struct{}, len(knownCELFailureReasons)+len(provider.AllFailReasons())+1)
	out := make([]string, 0, len(knownCELFailureReasons)+len(provider.AllFailReasons())+1)
	for _, r := range knownCELFailureReasons {
		seen[r] = struct{}{}
		out = append(out, r)
	}
	if _, dup := seen[systemone.ReasonDialectDisabled]; !dup {
		seen[systemone.ReasonDialectDisabled] = struct{}{}
		out = append(out, systemone.ReasonDialectDisabled)
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
// accepted by on_failed_reasons for CEL-only policies (M1 parity; consumer
// design §5.3).
func KnownFailureReasons() []string {
	out := slices.Clone(knownCELFailureReasons)
	slices.Sort(out)
	return out
}

// KnownSystemOneFailureReasons returns a sorted copy of the closed
// failure-reason set accepted by on_failed_reasons when system_one_rule is set
// (CEL + System One provider failure reasons + dialect_disabled).
func KnownSystemOneFailureReasons() []string {
	out := slices.Clone(knownFailureReasons)
	slices.Sort(out)
	return out
}

// IsValidFailureReason reports whether reason belongs to the closed M1 CEL-only
// failure reason set accepted as an on_failed_reasons key.
func IsValidFailureReason(reason string) bool {
	_, ok := knownCELFailureReasonSet[reason]
	return ok
}

// IsValidSystemOneFailureReason reports whether reason belongs to the closed
// System One failure reason set (CEL + System One provider reasons +
// dialect_disabled).
func IsValidSystemOneFailureReason(reason string) bool {
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

// ChainStepSpec mirrors one authored chain step for authoring validation
// without importing pkg/store.
type ChainStepSpec struct {
	ID                string          `json:"id"`
	Role              string          `json:"role"`
	Dialect           string          `json:"dialect"`
	Expr              string          `json:"expr,omitempty"`
	SystemOne         json.RawMessage `json:"system_one,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	Remediation       string          `json:"remediation,omitempty"`
	HasVerdictMapping bool            `json:"-"`
}

// AuthoringSpec carries the author-supplied fields of a message policy for
// validation and compilation at create/update/test time.
type AuthoringSpec struct {
	Name              string
	Mode              string
	Scope             Scope
	ViolationRule     string
	SystemOneRule     string
	HasSteps          bool
	Steps             []ChainStepSpec
	OnUncertain       string
	OnFailed          string
	OnFailedReasons   map[string]string
	RejectReason      string
	RejectRemediation string

	// SkipSystemOneBindingValidation skips runtime binding validation
	// (Dialect.Compile / ValidateCompiled and the hub.system_one experiment
	// check), running only syntax/schema/environment compilation via
	// s1spec.Compile. Used strictly when PATCH results in a disabled policy
	// with system_one_rule unchanged so operators can always disable a policy
	// whose provider binding is broken or while hub.system_one is off.
	SkipSystemOneBindingValidation bool
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

	if spec.HasSteps && (strings.TrimSpace(spec.ViolationRule) != "" || strings.TrimSpace(spec.SystemOneRule) != "") {
		return "", &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "steps",
			Message: "steps cannot be combined with violation_rule or system_one_rule",
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
				Message: fmt.Sprintf("surface %q is not covered (covered surfaces: %s, %s, %s, %s)", s, SurfaceAgentToUser, SurfaceAgentToConversation, SurfaceAgentDM, SurfaceMentionDelivery),
			}
		}
	}

	for _, k := range spec.Scope.Recipient.Kinds {
		if k != "user" && k != "conversation" && k != "agent" {
			return "", &ValidationError{
				Code:    ErrCodeValidation,
				Field:   "scope.recipient.kinds",
				Message: fmt.Sprintf("invalid recipient kind %q (must be \"user\", \"conversation\", or \"agent\")", k),
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
	if len(spec.Scope.Recipient.AgentIDs) > 0 {
		for _, id := range spec.Scope.Recipient.AgentIDs {
			if strings.TrimSpace(id) == "" {
				return "", &ValidationError{
					Code:    ErrCodeValidation,
					Field:   "scope.recipient.agent_ids",
					Message: "scope.recipient.agent_ids entries must not be empty",
				}
			}
		}
		agentSurface := false
		for _, s := range spec.Scope.Surfaces {
			if surfaceRecipientKind(s) == "agent" {
				agentSurface = true
				break
			}
		}
		if !agentSurface || (len(spec.Scope.Recipient.Kinds) > 0 && !slices.Contains(spec.Scope.Recipient.Kinds, "agent")) {
			return "", &ValidationError{
				Code:    ErrCodeValidation,
				Field:   "scope.recipient.agent_ids",
				Message: "scope.recipient.agent_ids requires an agent-recipient surface and must not exclude recipient kind \"agent\"",
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
		validReason := IsValidFailureReason(reason)
		if hasSystemOne {
			validReason = IsValidSystemOneFailureReason(reason)
		}
		if !validReason {
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

	if spec.HasSteps {
		if err := e.validateChainAuthoring(ctx, mode, spec); err != nil {
			return "", err
		}
		return mode, nil
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
		var s1Err error
		if spec.SkipSystemOneBindingValidation {
			contentEnv := SystemOneContentEnv()
			_, s1Err = s1spec.Compile([]byte(spec.SystemOneRule), s1spec.CompileOptions{
				Kind:        s1spec.Kind(KindJudgments),
				Environment: &contentEnv,
			})
		} else {
			_, s1Err = e.CompileSystemOne(ctx, spec.SystemOneRule)
		}
		if s1Err != nil {
			if compErr, ok := s1spec.AsCompileError(s1Err); ok {
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
					Cause:    s1Err,
				}
			}
			var unsupErr *ruleengine.UnsupportedDialectError
			if errors.As(s1Err, &unsupErr) {
				return "", &ValidationError{
					Code:     ErrCodeUnsupportedDialect,
					Field:    "system_one_rule",
					Message:  unsupErr.Error(),
					Category: unsupErr.Reason,
					Cause:    s1Err,
				}
			}
			return "", &ValidationError{
				Code:    ErrCodeInvalidRule,
				Field:   "system_one_rule",
				Message: s1Err.Error(),
				Cause:   s1Err,
			}
		}
	}

	return mode, nil
}

func (e *Evaluator) validateChainAuthoring(ctx context.Context, mode string, spec AuthoringSpec) error {
	if strings.TrimSpace(spec.ViolationRule) != "" || strings.TrimSpace(spec.SystemOneRule) != "" {
		return &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "steps",
			Message: "steps cannot be combined with violation_rule or system_one_rule",
		}
	}
	if len(spec.Steps) == 0 || len(spec.Steps) > MaxChainSteps {
		return &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "steps",
			Message: fmt.Sprintf("steps must contain between 1 and %d elements", MaxChainSteps),
		}
	}

	// P1 phase acceptance invariant (SP1 §1.1, policy-chain-plan §5):
	// enforce mode and E1/E2 origin surfaces only; CEL steps only.
	if mode != ModeEnforce {
		return &ValidationError{
			Code:    ErrCodeNotYetSupported,
			Field:   "mode",
			Message: fmt.Sprintf("chain policies only support mode %q in this phase (got %q)", ModeEnforce, mode),
		}
	}
	for _, s := range spec.Scope.Surfaces {
		if !IsP1ChainSurface(s) {
			return &ValidationError{
				Code:    ErrCodeNotYetSupported,
				Field:   "scope.surfaces",
				Message: fmt.Sprintf("chain policies do not yet support surface %q (supported in P1: %s, %s)", s, SurfaceAgentToUser, SurfaceAgentToConversation),
			}
		}
	}

	// Bound total canonical chain JSON size (<= 64 KiB).
	if raw, err := json.Marshal(struct {
		Version           int             `json:"v"`
		Steps             []ChainStepSpec `json:"steps"`
		RejectReason      string          `json:"reject_reason,omitempty"`
		RejectRemediation string          `json:"reject_remediation,omitempty"`
	}{
		Version:           ChainVersion,
		Steps:             spec.Steps,
		RejectReason:      spec.RejectReason,
		RejectRemediation: spec.RejectRemediation,
	}); err == nil && len(raw) > MaxChainBytes {
		return &ValidationError{
			Code:    ErrCodeValidation,
			Field:   "steps",
			Message: fmt.Sprintf("chain document size %d exceeds maximum %d bytes", len(raw), MaxChainBytes),
		}
	}

	seenIDs := make(map[string]struct{}, len(spec.Steps))
	priorJudgments := make(map[string]struct{})
	judgmentCount := 0
	verdictCapable := 0

	for i, st := range spec.Steps {
		fieldPrefix := fmt.Sprintf("steps[%d]", i)
		if !IsValidChainStepID(st.ID) {
			return &ValidationError{
				Code:    ErrCodeValidation,
				Field:   fieldPrefix + ".id",
				Message: fmt.Sprintf("invalid step id %q (must match ^[a-z][a-z0-9_]{0,31}$)", st.ID),
			}
		}
		if _, dup := seenIDs[st.ID]; dup {
			return &ValidationError{
				Code:    ErrCodeValidation,
				Field:   fieldPrefix + ".id",
				Message: fmt.Sprintf("duplicate step id %q", st.ID),
			}
		}
		seenIDs[st.ID] = struct{}{}

		switch st.Role {
		case StepRoleGuard, StepRoleViolation, StepRoleJudgment:
		default:
			return &ValidationError{
				Code:    ErrCodeValidation,
				Field:   fieldPrefix + ".role",
				Message: fmt.Sprintf("invalid step role %q (must be guard, violation, or judgment)", st.Role),
			}
		}
		switch st.Dialect {
		case StepDialectCEL, StepDialectSystemOne:
		default:
			return &ValidationError{
				Code:    ErrCodeValidation,
				Field:   fieldPrefix + ".dialect",
				Message: fmt.Sprintf("invalid step dialect %q (must be cel or system_one)", st.Dialect),
			}
		}

		// Role/dialect pairing (SP1 §1.2): guard->cel, violation->cel, judgment->system_one.
		expectedDialect := StepDialectCEL
		if st.Role == StepRoleJudgment {
			expectedDialect = StepDialectSystemOne
		}
		if st.Dialect != expectedDialect {
			return &ValidationError{
				Code:    ErrCodeRoleDialectMismatch,
				Field:   fieldPrefix + ".dialect",
				Message: fmt.Sprintf("step role %q requires dialect %q (got %q)", st.Role, expectedDialect, st.Dialect),
			}
		}

		if st.Role == StepRoleJudgment {
			judgmentCount++
			if judgmentCount > MaxChainJudgmentSteps {
				return &ValidationError{
					Code:    ErrCodeValidation,
					Field:   fieldPrefix + ".role",
					Message: fmt.Sprintf("chain may contain at most %d judgment steps", MaxChainJudgmentSteps),
				}
			}
			// P1/2a boundary: reject judgment steps at authoring (SP1 §1.1 option (ii)).
			return &ValidationError{
				Code:    ErrCodeNotYetSupported,
				Field:   fieldPrefix + ".role",
				Message: "judgment steps are not yet supported in this phase",
			}
		}

		if len(st.SystemOne) > 0 {
			return &ValidationError{
				Code:    ErrCodeValidation,
				Field:   fieldPrefix + ".system_one",
				Message: "system_one is only valid on judgment steps",
			}
		}
		if st.HasVerdictMapping {
			return &ValidationError{
				Code:    ErrCodeValidation,
				Field:   fieldPrefix + ".verdict_mapping",
				Message: "verdict_mapping is only valid on judgment steps",
			}
		}

		if err := ValidateTemplate(fieldPrefix+".reason", st.Reason); err != nil {
			return err
		}
		if err := ValidateTemplate(fieldPrefix+".remediation", st.Remediation); err != nil {
			return err
		}

		if strings.TrimSpace(st.Expr) == "" {
			return &ValidationError{
				Code:    ErrCodeInvalidRule,
				Field:   fieldPrefix + ".expr",
				Message: "expr is required for cel steps",
			}
		}

		compiled, err := e.CompileChainStep(ctx, st.Expr)
		if err != nil {
			var compErr *celengine.CompileError
			if errors.As(err, &compErr) {
				if reserved := matchReservedLegacyName(compErr.Message); reserved != "" {
					return &ValidationError{
						Code:     ErrCodeReservedName,
						Field:    fieldPrefix + ".expr",
						Message:  fmt.Sprintf("reserved legacy variable %q is unavailable in chain steps; reference chain.<step_id> instead", reserved),
						Line:     compErr.Line,
						Column:   compErr.Column,
						Category: compErr.Category,
						Cause:    err,
					}
				}
				return &ValidationError{
					Code:     ErrCodeInvalidRule,
					Field:    fieldPrefix + ".expr",
					Message:  compErr.Message,
					Line:     compErr.Line,
					Column:   compErr.Column,
					Category: compErr.Category,
					Cause:    err,
				}
			}
			var unsupErr *ruleengine.UnsupportedDialectError
			if errors.As(err, &unsupErr) {
				return &ValidationError{
					Code:     ErrCodeUnsupportedDialect,
					Field:    fieldPrefix + ".expr",
					Message:  unsupErr.Error(),
					Category: unsupErr.Reason,
					Cause:    err,
				}
			}
			return &ValidationError{
				Code:    ErrCodeInvalidRule,
				Field:   fieldPrefix + ".expr",
				Message: err.Error(),
				Cause:   err,
			}
		}

		prog, ok := compiled.Rep().(celengine.Program)
		if !ok || prog.IsZero() {
			return &ValidationError{
				Code:    ErrCodeInvalidRule,
				Field:   fieldPrefix + ".expr",
				Message: "invalid compiled CEL program",
			}
		}
		refs, dynamic, refErr := prog.MapKeyAccesses("chain")
		if refErr != nil {
			return &ValidationError{
				Code:    ErrCodeInvalidRule,
				Field:   fieldPrefix + ".expr",
				Message: refErr.Error(),
				Cause:   refErr,
			}
		}
		if dynamic {
			return &ValidationError{
				Code:    ErrCodeDynamicStepReference,
				Field:   fieldPrefix + ".expr",
				Message: "chain must only be indexed with static literal step ids (e.g. chain.<step_id> or chain[\"<step_id>\"])",
			}
		}
		for _, refID := range refs {
			if _, ok := priorJudgments[refID]; !ok {
				return &ValidationError{
					Code:    ErrCodeForwardReference,
					Field:   fieldPrefix + ".expr",
					Message: fmt.Sprintf("chain.%s does not reference an earlier judgment step", refID),
				}
			}
		}

		if st.Role == StepRoleViolation {
			verdictCapable++
		}
	}

	if verdictCapable == 0 {
		return &ValidationError{
			Code:    ErrCodeChainHasNoVerdict,
			Field:   "steps",
			Message: "chain must contain at least one verdict-capable step (violation or judgment with verdict_mapping)",
		}
	}
	return nil
}

func matchReservedLegacyName(msg string) string {
	for _, name := range reservedLegacyJudgmentNames {
		if strings.Contains(msg, "'"+name+"'") || strings.Contains(msg, "`"+name+"`") {
			return name
		}
	}
	return ""
}
