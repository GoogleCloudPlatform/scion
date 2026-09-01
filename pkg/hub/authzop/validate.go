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

package authzop

import (
	"errors"
	"fmt"
	"strings"
)

// Validate performs deterministic validation of an OperationSpec. It returns
// a non-nil error describing every violation found. Validation is fail-closed:
// a spec that fails Validate() must not be accepted into any catalog or
// enforcement path.
func (s *OperationSpec) Validate() error {
	var errs []error

	// --- Required fields ---
	errs = append(errs, s.validateID()...)

	if s.Domain == "" {
		errs = append(errs, errors.New("domain is required"))
	}
	if s.Description == "" {
		errs = append(errs, errors.New("description is required"))
	}
	if len(s.EntryPoints) == 0 && !s.waives(WaiveEntryPoints) {
		errs = append(errs, errors.New("at least one entry point is required (or waive entry_points)"))
	}
	if len(s.Principals) == 0 && !s.waives(WaivePrincipals) {
		errs = append(errs, errors.New("at least one principal kind is required (or waive principals)"))
	}
	if len(s.Credentials) == 0 && !s.waives(WaiveCredentials) {
		errs = append(errs, errors.New("at least one credential kind is required (or waive credentials)"))
	}
	if s.BasePermission == "" && !s.waives(WaiveBasePermission) {
		errs = append(errs, errors.New("base permission is required (or waive base_permission)"))
	}
	if s.ResourceResolver == "" && !s.waives(WaiveResourceResolver) {
		errs = append(errs, errors.New("resource resolver is required (or waive resource_resolver)"))
	}
	if len(s.Effects) == 0 {
		errs = append(errs, errors.New("at least one security effect is required"))
	}
	if len(s.DenialCodes) == 0 && !s.waives(WaiveDenialCodes) {
		errs = append(errs, errors.New("at least one denial code is required (or waive denial_codes)"))
	}

	// --- Entry point validation ---
	epSeen := make(map[string]bool)
	for i, ep := range s.EntryPoints {
		if ep.Kind == "" {
			errs = append(errs, fmt.Errorf("entry point [%d]: kind is required", i))
		} else if !validEntryPointKinds[ep.Kind] {
			errs = append(errs, fmt.Errorf("entry point [%d]: unknown kind %q", i, ep.Kind))
		}
		if ep.Pattern == "" {
			errs = append(errs, fmt.Errorf("entry point [%d]: pattern is required", i))
		}
		if httpLikeEntryPoints[ep.Kind] && ep.Method == "" {
			errs = append(errs, fmt.Errorf("entry point [%d]: method is required for %s entry points", i, ep.Kind))
		}
		if !httpLikeEntryPoints[ep.Kind] && ep.Method != "" {
			errs = append(errs, fmt.Errorf("entry point [%d]: method must be empty for %s entry points", i, ep.Kind))
		}
		key := string(ep.Kind) + ":" + ep.Method + ":" + ep.Pattern
		if epSeen[key] {
			errs = append(errs, fmt.Errorf("entry point [%d]: duplicate entry point %s", i, key))
		}
		epSeen[key] = true
	}

	// --- Principal kind validation ---
	pkSeen := make(map[PrincipalKind]bool)
	for i, pk := range s.Principals {
		if !validPrincipalKinds[pk] {
			errs = append(errs, fmt.Errorf("principal [%d]: unknown kind %q", i, pk))
		}
		if pkSeen[pk] {
			errs = append(errs, fmt.Errorf("principal [%d]: duplicate principal kind %q", i, pk))
		}
		pkSeen[pk] = true
	}

	// --- Credential kind validation ---
	ckSeen := make(map[CredentialKind]bool)
	for i, ck := range s.Credentials {
		if !validCredentialKinds[ck] {
			errs = append(errs, fmt.Errorf("credential [%d]: unknown kind %q", i, ck))
		}
		if ckSeen[ck] {
			errs = append(errs, fmt.Errorf("credential [%d]: duplicate credential kind %q", i, ck))
		}
		ckSeen[ck] = true
	}

	// --- Security effect validation and obligation collection ---
	requiredDelegation := DelegationNone
	requiresGovernance := false
	requiredAuthorityEval := AuthorityEvalNone
	hasAuditRequiredEffect := false
	needsBeforeFields := false
	needsAfterFields := false
	needsExternalPolicy := false

	effSeen := make(map[SecurityEffect]bool)
	for i, eff := range s.Effects {
		if !validSecurityEffects[eff] {
			errs = append(errs, fmt.Errorf("effect [%d]: unknown security effect %q", i, eff))
		}
		if effSeen[eff] {
			errs = append(errs, fmt.Errorf("effect [%d]: duplicate security effect %q", i, eff))
		}
		effSeen[eff] = true

		// Collect delegation requirement.
		if req, ok := effectDelegationRequirements[eff]; ok {
			if delegationStrength[req] > delegationStrength[requiredDelegation] {
				requiredDelegation = req
			}
		}

		// Collect governance requirement.
		if effectGovernanceRequired[eff] {
			requiresGovernance = true
		}

		// Collect authority evaluation requirement.
		if req, ok := effectAuthorityEvalRequirements[eff]; ok {
			if authorityEvalStrength[req] > authorityEvalStrength[requiredAuthorityEval] {
				requiredAuthorityEval = req
			}
		}

		// Collect audit requirements.
		if eff.RequiresAudit() {
			hasAuditRequiredEffect = true
			if afr, ok := effectAuditFieldRequirements[eff]; ok {
				if afr.NeedsBefore {
					needsBeforeFields = true
				}
				if afr.NeedsAfter {
					needsAfterFields = true
				}
			}
		}

		// External effect policy requirement.
		if eff == EffectEmitExternal {
			needsExternalPolicy = true
		}
	}

	// --- Delegation validation ---
	if !validDelegationKinds[s.DelegationKind] {
		errs = append(errs, fmt.Errorf("unknown delegation kind %q", s.DelegationKind))
	}
	if delegationStrength[s.DelegationKind] < delegationStrength[requiredDelegation] {
		errs = append(errs, fmt.Errorf("effects require delegation kind %q but spec declares %q", requiredDelegation, s.DelegationKind))
	}
	if s.DelegationKind != DelegationNone && s.DelegationDescription == "" {
		errs = append(errs, errors.New("delegation description is required when delegation kind is not none"))
	}

	// --- Governance validation ---
	if requiresGovernance && s.Governance == nil {
		errs = append(errs, errors.New("effects require a governance policy"))
	}
	if s.Governance != nil {
		if s.Governance.Kind == "" {
			errs = append(errs, errors.New("governance policy: kind is required"))
		} else if !validGovernanceKinds[s.Governance.Kind] {
			errs = append(errs, fmt.Errorf("governance policy: unknown kind %q", s.Governance.Kind))
		}
		if s.Governance.Description == "" {
			errs = append(errs, errors.New("governance policy: description is required"))
		}
		if s.Governance.Kind == GovernanceDomainSpecific && s.Governance.DomainCallback == "" {
			errs = append(errs, errors.New("governance policy: domain_specific kind requires a non-empty domain callback"))
		}
	}

	// --- Authority evaluation validation ---
	if !validAuthorityEvalKinds[s.AuthorityEval] {
		errs = append(errs, fmt.Errorf("unknown authority evaluation kind %q", s.AuthorityEval))
	}
	if authorityEvalStrength[s.AuthorityEval] < authorityEvalStrength[requiredAuthorityEval] {
		errs = append(errs, fmt.Errorf("effects require authority evaluation %q but spec declares %q", requiredAuthorityEval, s.AuthorityEval))
	}

	// --- Audit obligation validation ---
	if hasAuditRequiredEffect && s.AuditObligation == nil && !s.waives(WaiveAuditObligation) {
		errs = append(errs, errors.New("effects requiring audit must have an audit obligation (or waive audit_obligation)"))
	}
	if s.AuditObligation != nil {
		if s.AuditObligation.EventType == "" {
			errs = append(errs, errors.New("audit obligation: event type is required"))
		}
		if len(s.AuditObligation.ContextFields) == 0 {
			errs = append(errs, errors.New("audit obligation: at least one context field is required"))
		}
		// Validate before/after fields against effect requirements.
		if needsBeforeFields && len(s.AuditObligation.BeforeFields) == 0 {
			errs = append(errs, errors.New("audit obligation: before fields are required by declared effects"))
		}
		if needsAfterFields && len(s.AuditObligation.AfterFields) == 0 {
			errs = append(errs, errors.New("audit obligation: after fields are required by declared effects"))
		}
		// Validate non-empty/unique fields.
		errs = append(errs, validateAuditFields("context", s.AuditObligation.ContextFields)...)
		errs = append(errs, validateAuditFields("before", s.AuditObligation.BeforeFields)...)
		errs = append(errs, validateAuditFields("after", s.AuditObligation.AfterFields)...)
		// Atomicity justification.
		if !s.AuditObligation.Atomic && s.AuditObligation.NonAtomicJustification == "" {
			errs = append(errs, errors.New("audit obligation: non-atomic audit requires a justification"))
		}
	}

	// --- External effect policy validation ---
	if needsExternalPolicy && s.ExternalPolicy == nil {
		errs = append(errs, errors.New("emit-external-effect requires an external effect policy"))
	}
	if s.ExternalPolicy != nil {
		if !validExternalDeliveryModes[s.ExternalPolicy.DeliveryMode] {
			errs = append(errs, fmt.Errorf("external policy: unknown delivery mode %q", s.ExternalPolicy.DeliveryMode))
		}
		if !validExternalFailureModes[s.ExternalPolicy.FailureMode] {
			errs = append(errs, fmt.Errorf("external policy: unknown failure mode %q", s.ExternalPolicy.FailureMode))
		}
		if s.ExternalPolicy.IdempotencyKey == "" {
			errs = append(errs, errors.New("external policy: idempotency key description is required"))
		}
		if s.ExternalPolicy.RetryPolicy == "" {
			errs = append(errs, errors.New("external policy: retry policy is required"))
		}
		if s.ExternalPolicy.FailureMode == FailureCompensate && s.ExternalPolicy.Compensation == "" {
			errs = append(errs, errors.New("external policy: compensate failure mode requires a non-empty compensation description"))
		}
	}

	// --- Invariant validation ---
	invSeen := make(map[string]bool)
	for i, inv := range s.Invariants {
		if inv.ID == "" {
			errs = append(errs, fmt.Errorf("invariant [%d]: ID is required", i))
		}
		if inv.Description == "" {
			errs = append(errs, fmt.Errorf("invariant [%d]: description is required", i))
		}
		if inv.Kind == "" {
			errs = append(errs, fmt.Errorf("invariant [%d]: kind is required", i))
		} else if !validInvariantKinds[inv.Kind] {
			errs = append(errs, fmt.Errorf("invariant [%d]: unknown kind %q", i, inv.Kind))
		}
		if inv.Kind == InvariantSecurity && !inv.FailClosed {
			errs = append(errs, fmt.Errorf("invariant [%d]: security invariants must be fail-closed", i))
		}
		if invSeen[inv.ID] {
			errs = append(errs, fmt.Errorf("invariant [%d]: duplicate ID %q", i, inv.ID))
		}
		invSeen[inv.ID] = true
	}

	// --- Denial code validation ---
	dcSeen := make(map[DenialCode]bool)
	for i, dc := range s.DenialCodes {
		if dc == "" {
			errs = append(errs, fmt.Errorf("denial code [%d]: empty denial code", i))
		}
		if dcSeen[dc] {
			errs = append(errs, fmt.Errorf("denial code [%d]: duplicate denial code %q", i, dc))
		}
		dcSeen[dc] = true
	}

	// --- Test reference validation ---
	if len(s.TestRefs) == 0 && !s.waives(WaiveTestRefs) {
		errs = append(errs, errors.New("at least one test reference is required (or waive test_refs)"))
	}
	trSeen := make(map[string]bool)
	for i, tr := range s.TestRefs {
		if tr.Package == "" {
			errs = append(errs, fmt.Errorf("test ref [%d]: package is required", i))
		}
		if tr.Function == "" {
			errs = append(errs, fmt.Errorf("test ref [%d]: function is required", i))
		}
		trKey := tr.Package + ":" + tr.Function
		if trSeen[trKey] {
			errs = append(errs, fmt.Errorf("test ref [%d]: duplicate test ref %s", i, trKey))
		}
		trSeen[trKey] = true
	}

	// --- Exemption validation ---
	errs = append(errs, s.validateExemptions()...)

	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}

// validateID checks operation ID syntax and domain-prefix consistency.
func (s *OperationSpec) validateID() []error {
	var errs []error
	if s.ID == "" {
		errs = append(errs, errors.New("operation ID is required"))
		return errs
	}

	id := string(s.ID)

	// Check lowercase dot-separated segments: each segment starts with
	// a letter and contains only lowercase letters and digits.
	segments := strings.Split(id, ".")
	if len(segments) < 2 {
		errs = append(errs, fmt.Errorf("operation ID %q must have at least two dot-separated segments", id))
	}
	for si, seg := range segments {
		if seg == "" {
			errs = append(errs, fmt.Errorf("operation ID %q: empty segment at position %d", id, si))
			continue
		}
		if seg[0] < 'a' || seg[0] > 'z' {
			errs = append(errs, fmt.Errorf("operation ID %q: segment %q must start with a lowercase letter", id, seg))
			continue
		}
		for _, ch := range seg {
			if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
				errs = append(errs, fmt.Errorf("operation ID %q: invalid character %q (only lowercase letters and digits allowed)", id, string(ch)))
				break
			}
		}
	}

	// Domain-prefix consistency.
	if s.Domain != "" && !strings.HasPrefix(id, s.Domain+".") && id != s.Domain {
		errs = append(errs, fmt.Errorf("operation ID %q must start with domain %q", id, s.Domain))
	}

	return errs
}

// validateExemptions validates exemption fields and waiver consistency.
func (s *OperationSpec) validateExemptions() []error {
	var errs []error
	exSeen := make(map[string]bool)
	for i, ex := range s.Exemptions {
		if ex.Kind == "" {
			errs = append(errs, fmt.Errorf("exemption [%d]: kind is required", i))
		} else if !validExemptionKinds[ex.Kind] {
			errs = append(errs, fmt.Errorf("exemption [%d]: unknown kind %q", i, ex.Kind))
		}
		if ex.Reason == "" {
			errs = append(errs, fmt.Errorf("exemption [%d]: reason is required", i))
		}
		if ex.Scope == "" {
			errs = append(errs, fmt.Errorf("exemption [%d]: scope is required", i))
		}
		if len(ex.Waives) == 0 {
			errs = append(errs, fmt.Errorf("exemption [%d]: at least one waived obligation is required", i))
		}
		wSeen := make(map[WaivedObligation]bool)
		for j, w := range ex.Waives {
			if !validWaivedObligations[w] {
				errs = append(errs, fmt.Errorf("exemption [%d] waiver [%d]: unknown obligation %q", i, j, w))
			}
			if wSeen[w] {
				errs = append(errs, fmt.Errorf("exemption [%d] waiver [%d]: duplicate waiver %q", i, j, w))
			}
			wSeen[w] = true
		}
		exKey := string(ex.Kind) + ":" + ex.Scope
		if exSeen[exKey] {
			errs = append(errs, fmt.Errorf("exemption [%d]: duplicate exemption kind %q with scope %q", i, ex.Kind, ex.Scope))
		}
		exSeen[exKey] = true
	}
	return errs
}

// waives reports whether any exemption waives the given obligation.
func (s *OperationSpec) waives(obl WaivedObligation) bool {
	for _, ex := range s.Exemptions {
		for _, w := range ex.Waives {
			if w == obl {
				return true
			}
		}
	}
	return false
}

// validateAuditFields checks that audit field lists contain no empty
// or duplicate values.
func validateAuditFields(category string, fields []string) []error {
	var errs []error
	seen := make(map[string]bool)
	for i, f := range fields {
		if f == "" {
			errs = append(errs, fmt.Errorf("audit obligation: empty %s field at index %d", category, i))
		}
		if seen[f] {
			errs = append(errs, fmt.Errorf("audit obligation: duplicate %s field %q", category, f))
		}
		seen[f] = true
	}
	return errs
}

// ValidationError collects all validation errors for an OperationSpec.
type ValidationError struct {
	Errors []error
}

// Error returns a newline-separated list of all validation errors.
func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, err := range e.Errors {
		msgs[i] = err.Error()
	}
	return "operation spec validation failed:\n  " + strings.Join(msgs, "\n  ")
}

// ValidateSpecs validates a slice of OperationSpecs and checks for
// duplicate IDs and entry points across all specs.
func ValidateSpecs(specs []OperationSpec) error {
	var errs []error

	idSeen := make(map[OperationID]bool)
	epSeen := make(map[string]OperationID)

	for i := range specs {
		spec := &specs[i]

		if err := spec.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("spec %q: %w", spec.ID, err))
		}

		if idSeen[spec.ID] {
			errs = append(errs, fmt.Errorf("duplicate operation ID %q", spec.ID))
		}
		idSeen[spec.ID] = true

		for _, ep := range spec.EntryPoints {
			key := string(ep.Kind) + ":" + ep.Method + ":" + ep.Pattern
			if owner, ok := epSeen[key]; ok {
				errs = append(errs, fmt.Errorf("entry point %s is claimed by both %q and %q", key, owner, spec.ID))
			}
			epSeen[key] = spec.ID
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}
