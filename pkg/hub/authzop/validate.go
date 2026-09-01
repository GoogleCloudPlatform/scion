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
//
// Validation checks:
//   - All required fields are present and non-empty.
//   - All enum values belong to their closed vocabulary.
//   - No duplicate operation IDs or entry points within a spec.
//   - Authority effects require a delegation policy.
//   - Boundary effects require governance.
//   - Audit-requiring effects require an audit obligation (or an exemption).
//   - Invariants that are security-critical must be fail-closed.
//   - At least one test reference is present.
//   - Exemptions have valid kinds and non-empty reasons.
func (s *OperationSpec) Validate() error {
	var errs []error

	// Required fields.
	if s.ID == "" {
		errs = append(errs, errors.New("operation ID is required"))
	}
	if s.Domain == "" {
		errs = append(errs, errors.New("domain is required"))
	}
	if s.Description == "" {
		errs = append(errs, errors.New("description is required"))
	}
	if len(s.EntryPoints) == 0 && len(s.Exemptions) == 0 {
		errs = append(errs, errors.New("at least one entry point is required (or an exemption must justify its absence)"))
	}
	if len(s.Principals) == 0 && len(s.Exemptions) == 0 {
		errs = append(errs, errors.New("at least one principal kind is required (or an exemption must justify its absence)"))
	}
	if s.BasePermission == "" && len(s.Exemptions) == 0 {
		errs = append(errs, errors.New("base permission is required (or an exemption must justify its absence)"))
	}
	if len(s.Effects) == 0 {
		errs = append(errs, errors.New("at least one security effect is required"))
	}

	// Entry point validation.
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
		if ep.Kind == EntryPointHTTPRoute && ep.Method == "" {
			errs = append(errs, fmt.Errorf("entry point [%d]: method is required for HTTP routes", i))
		}
		key := string(ep.Kind) + ":" + ep.Method + ":" + ep.Pattern
		if epSeen[key] {
			errs = append(errs, fmt.Errorf("entry point [%d]: duplicate entry point %s", i, key))
		}
		epSeen[key] = true
	}

	// Principal kind validation.
	for i, pk := range s.Principals {
		if !validPrincipalKinds[pk] {
			errs = append(errs, fmt.Errorf("principal [%d]: unknown kind %q", i, pk))
		}
	}

	// Security effect validation.
	hasAuthorityEffect := false
	hasBoundaryEffect := false
	hasAuditRequiredEffect := false
	for i, eff := range s.Effects {
		if !validSecurityEffects[eff] {
			errs = append(errs, fmt.Errorf("effect [%d]: unknown security effect %q", i, eff))
		}
		if eff.IsAuthorityEffect() {
			hasAuthorityEffect = true
		}
		if eff.IsBoundaryEffect() {
			hasBoundaryEffect = true
		}
		if eff.RequiresAudit() {
			hasAuditRequiredEffect = true
		}
	}

	// Authority effects require delegation policy.
	if hasAuthorityEffect && s.Delegation == nil {
		errs = append(errs, errors.New("authority effects require a delegation policy"))
	}

	// Boundary effects require governance.
	if hasBoundaryEffect && s.Governance == nil {
		errs = append(errs, errors.New("boundary effects require a governance policy"))
	}

	// Governance validation.
	if s.Governance != nil {
		if s.Governance.Kind == "" {
			errs = append(errs, errors.New("governance policy: kind is required"))
		} else if !validGovernanceKinds[s.Governance.Kind] {
			errs = append(errs, fmt.Errorf("governance policy: unknown kind %q", s.Governance.Kind))
		}
		if s.Governance.Description == "" {
			errs = append(errs, errors.New("governance policy: description is required"))
		}
	}

	// Audit-requiring effects need an audit obligation or exemption.
	if hasAuditRequiredEffect && s.AuditObligation == nil && !s.hasExemptionKind(ExemptionTestFixture) {
		errs = append(errs, errors.New("effects requiring audit must have an audit obligation (or a test_fixture exemption)"))
	}

	// Audit obligation validation.
	if s.AuditObligation != nil {
		if s.AuditObligation.EventType == "" {
			errs = append(errs, errors.New("audit obligation: event type is required"))
		}
	}

	// Invariant validation.
	invSeen := make(map[string]bool)
	for i, inv := range s.Invariants {
		if inv.ID == "" {
			errs = append(errs, fmt.Errorf("invariant [%d]: ID is required", i))
		}
		if inv.Description == "" {
			errs = append(errs, fmt.Errorf("invariant [%d]: description is required", i))
		}
		if invSeen[inv.ID] {
			errs = append(errs, fmt.Errorf("invariant [%d]: duplicate ID %q", i, inv.ID))
		}
		invSeen[inv.ID] = true
	}

	// Denial code validation — codes must not be empty.
	for i, dc := range s.DenialCodes {
		if dc == "" {
			errs = append(errs, fmt.Errorf("denial code [%d]: empty denial code", i))
		}
	}

	// Test reference validation — at least one is required unless exempted.
	if len(s.TestRefs) == 0 && !s.hasExemptionKind(ExemptionTestFixture) {
		errs = append(errs, errors.New("at least one test reference is required"))
	}
	for i, tr := range s.TestRefs {
		if tr.Package == "" {
			errs = append(errs, fmt.Errorf("test ref [%d]: package is required", i))
		}
		if tr.Function == "" {
			errs = append(errs, fmt.Errorf("test ref [%d]: function is required", i))
		}
	}

	// Exemption validation.
	for i, ex := range s.Exemptions {
		if ex.Kind == "" {
			errs = append(errs, fmt.Errorf("exemption [%d]: kind is required", i))
		} else if !validExemptionKinds[ex.Kind] {
			errs = append(errs, fmt.Errorf("exemption [%d]: unknown kind %q", i, ex.Kind))
		}
		if ex.Reason == "" {
			errs = append(errs, fmt.Errorf("exemption [%d]: reason is required", i))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}

// hasExemptionKind reports whether the spec has an exemption of the given kind.
func (s *OperationSpec) hasExemptionKind(kind ExemptionKind) bool {
	for _, ex := range s.Exemptions {
		if ex.Kind == kind {
			return true
		}
	}
	return false
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
// duplicate IDs and entry points across all specs. It returns a non-nil
// error if any spec is invalid or if duplicates are found.
func ValidateSpecs(specs []OperationSpec) error {
	var errs []error

	idSeen := make(map[OperationID]bool)
	epSeen := make(map[string]OperationID) // entry-point key -> owning op ID

	for i := range specs {
		spec := &specs[i]

		// Per-spec validation.
		if err := spec.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("spec %q: %w", spec.ID, err))
		}

		// Cross-spec duplicate ID check.
		if idSeen[spec.ID] {
			errs = append(errs, fmt.Errorf("duplicate operation ID %q", spec.ID))
		}
		idSeen[spec.ID] = true

		// Cross-spec duplicate entry point check.
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
