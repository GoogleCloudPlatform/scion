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
	"strings"
	"testing"
)

// --- Test helpers ---

// validSpec returns a minimal valid OperationSpec for use in tests.
func validSpec() OperationSpec {
	return OperationSpec{
		ID:               "test.read",
		Domain:           "test",
		Description:      "A test operation",
		EntryPoints:      []EntryPoint{{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test", Method: "GET"}},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "test-resolver",
		BasePermission:   "test.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestOperation"}},
	}
}

// validAuthoritySpec returns a valid spec with grant-authority effect.
func validAuthoritySpec() OperationSpec {
	return OperationSpec{
		ID:                    "test.authority",
		Domain:                "test",
		Description:           "Authority operation",
		EntryPoints:           []EntryPoint{{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/grant", Method: "POST"}},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT},
		ResourceResolver:      "project-from-url",
		BasePermission:        "project.manage",
		Effects:               []SecurityEffect{EffectGrantAuthority},
		DelegationKind:        DelegationNonAmplification,
		DelegationDescription: "Actor must hold all permissions being granted",
		AuthorityEval:         AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test.grant",
			ContextFields: []string{"actor_id", "project_id"},
			AfterFields:   []string{"target_role"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialRoleAssignmentForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub", Function: "TestGrant"}},
	}
}

// validExternalSpec returns a valid spec with emit-external-effect.
func validExternalSpec() OperationSpec {
	return OperationSpec{
		ID:               "test.external",
		Domain:           "test",
		Description:      "External effect operation",
		EntryPoints:      []EntryPoint{{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/dispatch", Method: "POST"}},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "agent.dispatch",
		Effects:          []SecurityEffect{EffectEmitExternal},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:              "agent.dispatch",
			ContextFields:          []string{"actor_id", "project_id"},
			AfterFields:            []string{"dispatch_id"},
			Atomic:                 false,
			NonAtomicJustification: "External dispatch is fire-and-forget; audit is best-effort",
		},
		ExternalPolicy: &ExternalEffectPolicy{
			DeliveryMode:   DeliveryFireAndForget,
			FailureMode:    FailureLogAndContinue,
			IdempotencyKey: "dispatch ID",
			RetryPolicy:    "no retry",
			AuthBeforeEmit: true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub", Function: "TestDispatch"}},
	}
}

func makeExemption(kind ExemptionKind, waives ...WaivedObligation) Exemption {
	return Exemption{
		Kind:   kind,
		Reason: "Test exemption",
		Scope:  "test only",
		Waives: waives,
	}
}

// --- Minimal valid specs ---

func TestValidate_MinimalValidSpec(t *testing.T) {
	s := validSpec()
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid spec, got: %v", err)
	}
}

func TestValidate_ValidAuthoritySpec(t *testing.T) {
	s := validAuthoritySpec()
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid authority spec, got: %v", err)
	}
}

func TestValidate_ValidExternalSpec(t *testing.T) {
	s := validExternalSpec()
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid external spec, got: %v", err)
	}
}

// --- G7: ID syntax validation ---

func TestValidate_IDSyntax(t *testing.T) {
	tests := []struct {
		name    string
		id      OperationID
		domain  string
		wantErr string
	}{
		{"valid two segments", "test.read", "test", ""},
		{"valid three segments", "project.membership.add", "project.membership", ""},
		{"single segment", "test", "test", "at least two dot-separated segments"},
		{"uppercase", "Test.Read", "test", "must start with a lowercase letter"},
		{"digit start", "1test.read", "1test", "must start with a lowercase letter"},
		{"hyphen", "test-op.read", "test-op", "invalid character"},
		{"underscore", "test_op.read", "test_op", "invalid character"},
		{"empty segment", "test..read", "test", "empty segment"},
		{"trailing dot", "test.read.", "test", "empty segment"},
		{"domain mismatch", "test.read", "other", "must start with domain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			s.ID = tc.id
			s.Domain = tc.domain
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected valid, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected error containing %q, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

// --- Required fields ---

func TestValidate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*OperationSpec)
		wantErr string
	}{
		{"missing ID", func(s *OperationSpec) { s.ID = "" }, "operation ID is required"},
		{"missing domain", func(s *OperationSpec) { s.Domain = "" }, "domain is required"},
		{"missing description", func(s *OperationSpec) { s.Description = "" }, "description is required"},
		{"missing entry points", func(s *OperationSpec) { s.EntryPoints = nil }, "at least one entry point is required"},
		{"missing principals", func(s *OperationSpec) { s.Principals = nil }, "at least one principal kind is required"},
		{"missing credentials", func(s *OperationSpec) { s.Credentials = nil }, "at least one credential kind is required"},
		{"missing base permission", func(s *OperationSpec) { s.BasePermission = "" }, "base permission is required"},
		{"missing resource resolver", func(s *OperationSpec) { s.ResourceResolver = "" }, "resource resolver is required"},
		{"missing effects", func(s *OperationSpec) { s.Effects = nil }, "at least one security effect is required"},
		{"missing denial codes", func(s *OperationSpec) { s.DenialCodes = nil }, "at least one denial code is required"},
		{"missing test refs", func(s *OperationSpec) { s.TestRefs = nil }, "at least one test reference is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// --- G4: Waiver-scoped exemptions ---

func TestValidate_WaiverBypassesNamedObligationsOnly(t *testing.T) {
	tests := []struct {
		name   string
		waives []WaivedObligation
		clear  func(*OperationSpec)
		valid  bool
	}{
		{
			name:   "waive entry_points bypasses entry point requirement",
			waives: []WaivedObligation{WaiveEntryPoints},
			clear:  func(s *OperationSpec) { s.EntryPoints = nil },
			valid:  true,
		},
		{
			name:   "waive principals bypasses principal requirement",
			waives: []WaivedObligation{WaivePrincipals},
			clear:  func(s *OperationSpec) { s.Principals = nil },
			valid:  true,
		},
		{
			name:   "waive credentials bypasses credential requirement",
			waives: []WaivedObligation{WaiveCredentials},
			clear:  func(s *OperationSpec) { s.Credentials = nil },
			valid:  true,
		},
		{
			name:   "waive base_permission bypasses permission requirement",
			waives: []WaivedObligation{WaiveBasePermission},
			clear:  func(s *OperationSpec) { s.BasePermission = "" },
			valid:  true,
		},
		{
			name:   "waive resource_resolver bypasses resolver requirement",
			waives: []WaivedObligation{WaiveResourceResolver},
			clear:  func(s *OperationSpec) { s.ResourceResolver = "" },
			valid:  true,
		},
		{
			name:   "waive test_refs bypasses test reference requirement",
			waives: []WaivedObligation{WaiveTestRefs},
			clear:  func(s *OperationSpec) { s.TestRefs = nil },
			valid:  true,
		},
		{
			name:   "waive denial_codes bypasses denial code requirement",
			waives: []WaivedObligation{WaiveDenialCodes},
			clear:  func(s *OperationSpec) { s.DenialCodes = nil },
			valid:  true,
		},
		{
			name:   "waive entry_points does not bypass principals",
			waives: []WaivedObligation{WaiveEntryPoints},
			clear:  func(s *OperationSpec) { s.EntryPoints = nil; s.Principals = nil },
			valid:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			s.Exemptions = []Exemption{makeExemption(ExemptionInternalOnly, tc.waives...)}
			tc.clear(&s)
			err := s.Validate()
			if tc.valid && err != nil {
				t.Errorf("expected valid, got: %v", err)
			}
			if !tc.valid && err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestValidate_ExemptionRequiresWaives(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{{
		Kind:   ExemptionTestFixture,
		Reason: "Test",
		Scope:  "test only",
		// No Waives
	}}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for exemption without waives")
	}
	if !strings.Contains(err.Error(), "at least one waived obligation is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_ExemptionRequiresKindReasonAndScope(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{{Waives: []WaivedObligation{WaiveTestRefs}}}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for empty exemption fields")
	}
	for _, want := range []string{"kind is required", "reason is required", "scope is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in error, got: %v", want, err)
		}
	}
}

func TestValidate_UnknownWaivedObligation(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{makeExemption(ExemptionTestFixture, "unknown_obligation")}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `unknown obligation "unknown_obligation"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_DuplicateWaiverInExemption(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{makeExemption(ExemptionTestFixture, WaiveTestRefs, WaiveTestRefs)}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `duplicate waiver "test_refs"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- G1: Credential separation ---

func TestValidate_CredentialKindValidation(t *testing.T) {
	s := validSpec()
	s.Credentials = []CredentialKind{"alien_token"}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for unknown credential kind")
	}
	if !strings.Contains(err.Error(), `unknown kind "alien_token"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_DuplicateCredentials(t *testing.T) {
	s := validSpec()
	s.Credentials = []CredentialKind{CredentialSessionJWT, CredentialSessionJWT}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate credentials")
	}
	if !strings.Contains(err.Error(), `duplicate credential kind "session_jwt"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_AllValidCredentialKinds(t *testing.T) {
	kinds := []CredentialKind{
		CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT,
		CredentialBrokerToken, CredentialServiceAccount,
		CredentialSystemInternal, CredentialIdentityToken,
	}
	for _, k := range kinds {
		s := validSpec()
		s.Credentials = []CredentialKind{k}
		if err := s.Validate(); err != nil {
			t.Errorf("valid credential %q caused error: %v", k, err)
		}
	}
}

// --- G2: Effect-specific delegation ---

func TestValidate_EffectDelegationRequirements(t *testing.T) {
	tests := []struct {
		name           string
		effect         SecurityEffect
		delegationKind DelegationKind
		wantErr        string
	}{
		{"grant needs non_amplification", EffectGrantAuthority, DelegationNone, "effects require delegation kind"},
		{"grant with non_amplification passes", EffectGrantAuthority, DelegationNonAmplification, ""},
		{"grant with conditional passes (subsumes)", EffectGrantAuthority, DelegationConditionalIncrease, ""},
		{"change needs conditional", EffectChangeAuthority, DelegationNonAmplification, "effects require delegation kind"},
		{"change with conditional passes", EffectChangeAuthority, DelegationConditionalIncrease, ""},
		{"revoke needs no delegation", EffectRevokeAuthority, DelegationNone, ""},
		{"ownership needs non_amplification", EffectChangeOwnership, DelegationNone, "effects require delegation kind"},
		{"read needs no delegation", EffectReadOne, DelegationNone, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			s.Effects = []SecurityEffect{tc.effect}
			s.DelegationKind = tc.delegationKind
			if tc.delegationKind != DelegationNone {
				s.DelegationDescription = "Test delegation"
			}
			// Satisfy other requirements
			if effectGovernanceRequired[tc.effect] {
				s.Governance = &GovernancePolicy{Kind: GovernancePeerSuperior, Description: "Test"}
			}
			if req, ok := effectAuthorityEvalRequirements[tc.effect]; ok {
				s.AuthorityEval = req
			}
			if tc.effect.RequiresAudit() {
				s.AuditObligation = makeAuditForEffect(tc.effect)
			}
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected valid, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected %q, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestValidate_DelegationDescriptionRequired(t *testing.T) {
	s := validAuthoritySpec()
	s.DelegationDescription = ""
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "delegation description is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- G2: Effect-specific governance ---

func TestValidate_EffectGovernanceRequirements(t *testing.T) {
	tests := []struct {
		name    string
		effect  SecurityEffect
		needGov bool
	}{
		{"revoke needs governance", EffectRevokeAuthority, true},
		{"ownership needs governance", EffectChangeOwnership, true},
		{"relax boundary needs governance", EffectRelaxBoundary, true},
		{"tighten boundary needs governance", EffectTightenBoundary, true},
		{"issue credential needs governance", EffectIssueCredential, true},
		{"mint credential needs governance", EffectMintCredential, true},
		{"assign credential needs governance", EffectAssignCredential, true},
		{"grant does NOT need governance", EffectGrantAuthority, false},
		{"read does NOT need governance", EffectReadOne, false},
		{"delete does NOT need governance", EffectDeleteResource, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			s.Effects = []SecurityEffect{tc.effect}
			s.Governance = nil
			// Satisfy delegation/eval/audit
			if req, ok := effectDelegationRequirements[tc.effect]; ok {
				s.DelegationKind = req
				s.DelegationDescription = "test"
			}
			if req, ok := effectAuthorityEvalRequirements[tc.effect]; ok {
				s.AuthorityEval = req
			}
			if tc.effect.RequiresAudit() {
				s.AuditObligation = makeAuditForEffect(tc.effect)
			}
			if tc.effect == EffectEmitExternal {
				s.ExternalPolicy = makeExternalPolicy()
			}
			err := s.Validate()
			if tc.needGov {
				if err == nil {
					t.Fatal("expected governance error")
				}
				if !strings.Contains(err.Error(), "effects require a governance policy") {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("expected valid, got: %v", err)
				}
			}
		})
	}
}

// --- G3: Authority evaluation requirements ---

func TestValidate_EffectAuthorityEvalRequirements(t *testing.T) {
	tests := []struct {
		name    string
		effect  SecurityEffect
		eval    AuthorityEvalKind
		wantErr string
	}{
		{"change-authority needs before_and_after", EffectChangeAuthority, AuthorityEvalNone, "effects require authority evaluation"},
		{"change-authority with before_and_after passes", EffectChangeAuthority, AuthorityEvalBeforeAndAfter, ""},
		{"change-ownership needs before_and_after", EffectChangeOwnership, AuthorityEvalNone, "effects require authority evaluation"},
		{"relax-boundary needs before_and_after", EffectRelaxBoundary, AuthorityEvalNone, "effects require authority evaluation"},
		{"tighten-boundary needs before_and_after", EffectTightenBoundary, AuthorityEvalNone, "effects require authority evaluation"},
		{"grant does NOT need eval", EffectGrantAuthority, AuthorityEvalNone, ""},
		{"read does NOT need eval", EffectReadOne, AuthorityEvalNone, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			s.Effects = []SecurityEffect{tc.effect}
			s.AuthorityEval = tc.eval
			// Satisfy other requirements
			if req, ok := effectDelegationRequirements[tc.effect]; ok {
				s.DelegationKind = req
				s.DelegationDescription = "test"
			}
			if effectGovernanceRequired[tc.effect] {
				s.Governance = &GovernancePolicy{Kind: GovernancePeerSuperior, Description: "test"}
			}
			if tc.effect.RequiresAudit() {
				s.AuditObligation = makeAuditForEffect(tc.effect)
			}
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected valid, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected %q, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

// --- G5: Invariant validation ---

func TestValidate_InvariantKindRequired(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{{ID: "test", Description: "test"}}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for missing invariant kind")
	}
	if !strings.Contains(err.Error(), "kind is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_SecurityInvariantMustBeFailClosed(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{{
		ID: "test", Description: "test",
		Kind: InvariantSecurity, FailClosed: false,
	}}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for non-fail-closed security invariant")
	}
	if !strings.Contains(err.Error(), "security invariants must be fail-closed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_BusinessInvariantMayBeFailOpen(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{{
		ID: "test", Description: "test",
		Kind: InvariantBusiness, FailClosed: false,
	}}
	if err := s.Validate(); err != nil {
		t.Errorf("business invariant should allow fail-open, got: %v", err)
	}
}

func TestValidate_SecurityInvariantFailClosedPasses(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{{
		ID: "test", Description: "test",
		Kind: InvariantSecurity, FailClosed: true,
	}}
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

// --- G5: Audit obligation validation ---

func TestValidate_AuditBeforeAfterFieldRequirements(t *testing.T) {
	tests := []struct {
		name    string
		effect  SecurityEffect
		before  []string
		after   []string
		wantErr string
	}{
		{"grant needs after", EffectGrantAuthority, nil, nil, "after fields are required"},
		{"grant with after passes", EffectGrantAuthority, nil, []string{"role"}, ""},
		{"revoke needs before", EffectRevokeAuthority, nil, nil, "before fields are required"},
		{"revoke with before passes", EffectRevokeAuthority, []string{"role"}, nil, ""},
		{"change needs both", EffectChangeAuthority, nil, nil, "before fields are required"},
		{"change with both passes", EffectChangeAuthority, []string{"old_role"}, []string{"new_role"}, ""},
		{"delete needs before", EffectDeleteResource, nil, nil, "before fields are required"},
		{"boundary needs both", EffectRelaxBoundary, nil, nil, "before fields are required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			s.Effects = []SecurityEffect{tc.effect}
			// Satisfy other requirements
			if req, ok := effectDelegationRequirements[tc.effect]; ok {
				s.DelegationKind = req
				s.DelegationDescription = "test"
			}
			if effectGovernanceRequired[tc.effect] {
				s.Governance = &GovernancePolicy{Kind: GovernancePeerSuperior, Description: "test"}
			}
			if req, ok := effectAuthorityEvalRequirements[tc.effect]; ok {
				s.AuthorityEval = req
			}
			if tc.effect == EffectEmitExternal {
				s.ExternalPolicy = makeExternalPolicy()
			}
			s.AuditObligation = &AuditObligation{
				EventType:     "test",
				ContextFields: []string{"actor_id"},
				BeforeFields:  tc.before,
				AfterFields:   tc.after,
				Atomic:        true,
			}
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected valid, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected %q, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestValidate_AuditRequiresContextFields(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectDeleteResource}
	s.AuditObligation = &AuditObligation{
		EventType:    "test.delete",
		BeforeFields: []string{"resource_id"},
		Atomic:       true,
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for audit obligation without context fields")
	}
	if !strings.Contains(err.Error(), "at least one context field is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_AuditWithContextFieldsPasses(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectDeleteResource}
	s.AuditObligation = &AuditObligation{
		EventType:     "test.delete",
		ContextFields: []string{"actor_id"},
		BeforeFields:  []string{"resource_id"},
		Atomic:        true,
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

func TestValidate_AuditNonAtomicRequiresJustification(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectDeleteResource}
	s.AuditObligation = &AuditObligation{
		EventType:     "test.delete",
		ContextFields: []string{"actor_id"},
		BeforeFields:  []string{"resource_id"},
		Atomic:        false,
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "non-atomic audit requires a justification") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_AuditAtomicNoJustificationNeeded(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectDeleteResource}
	s.AuditObligation = &AuditObligation{
		EventType:     "test.delete",
		ContextFields: []string{"actor_id"},
		BeforeFields:  []string{"resource_id"},
		Atomic:        true,
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

func TestValidate_AuditFieldsNonEmptyAndUnique(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectDeleteResource}
	s.AuditObligation = &AuditObligation{
		EventType:     "test",
		ContextFields: []string{"actor_id"},
		BeforeFields:  []string{"", "dup", "dup"},
		Atomic:        true,
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "empty before field") {
		t.Errorf("expected empty field error, got: %v", err)
	}
	if !strings.Contains(err.Error(), `duplicate before field "dup"`) {
		t.Errorf("expected duplicate field error, got: %v", err)
	}
}

// --- G6: External effect policy ---

func TestValidate_ExternalEffectRequiresPolicy(t *testing.T) {
	s := validExternalSpec()
	s.ExternalPolicy = nil
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "emit-external-effect requires an external effect policy") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_ExternalPolicyFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ExternalEffectPolicy)
		wantErr string
	}{
		{"unknown delivery mode", func(p *ExternalEffectPolicy) { p.DeliveryMode = "unknown" }, "unknown delivery mode"},
		{"unknown failure mode", func(p *ExternalEffectPolicy) { p.FailureMode = "unknown" }, "unknown failure mode"},
		{"missing idempotency", func(p *ExternalEffectPolicy) { p.IdempotencyKey = "" }, "idempotency key description is required"},
		{"missing retry", func(p *ExternalEffectPolicy) { p.RetryPolicy = "" }, "retry policy is required"},
		{"compensate without compensation", func(p *ExternalEffectPolicy) {
			p.FailureMode = FailureCompensate
			p.Compensation = ""
		}, "compensate failure mode requires"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validExternalSpec()
			tc.mutate(s.ExternalPolicy)
			err := s.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// --- G7: Entry point kinds ---

func TestValidate_WebSocketAndSSEEntryPoints(t *testing.T) {
	tests := []struct {
		name    string
		kind    EntryPointKind
		method  string
		wantErr string
	}{
		{"websocket with method passes", EntryPointWebSocket, "GET", ""},
		{"websocket without method fails", EntryPointWebSocket, "", "method is required for websocket"},
		{"sse with method passes", EntryPointSSE, "GET", ""},
		{"sse without method fails", EntryPointSSE, "", "method is required for sse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			s.EntryPoints = []EntryPoint{{Kind: tc.kind, Pattern: "/ws", Method: tc.method}}
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected valid, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected %q, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestValidate_NonHTTPEntryPointRejectsMethod(t *testing.T) {
	s := validSpec()
	s.EntryPoints = []EntryPoint{{Kind: EntryPointSchedulerJob, Pattern: "job", Method: "GET"}}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for non-HTTP entry point with method")
	}
	if !strings.Contains(err.Error(), "method must be empty for scheduler_job") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_NonHTTPEntryPointWithoutMethod(t *testing.T) {
	s := validSpec()
	s.EntryPoints = []EntryPoint{{Kind: EntryPointSchedulerJob, Pattern: "job"}}
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

// --- All-valid enum tests ---

func TestValidate_AllValidEntryPointKinds(t *testing.T) {
	for k := range validEntryPointKinds {
		s := validSpec()
		ep := EntryPoint{Kind: k, Pattern: "/test"}
		if httpLikeEntryPoints[k] {
			ep.Method = "GET"
		}
		s.EntryPoints = []EntryPoint{ep}
		if err := s.Validate(); err != nil {
			t.Errorf("valid entry point kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidPrincipalKinds(t *testing.T) {
	for k := range validPrincipalKinds {
		s := validSpec()
		s.Principals = []PrincipalKind{k}
		if err := s.Validate(); err != nil {
			t.Errorf("valid principal kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidGovernanceKinds(t *testing.T) {
	for k := range validGovernanceKinds {
		s := validSpec()
		s.Effects = []SecurityEffect{EffectRelaxBoundary}
		s.AuthorityEval = AuthorityEvalBeforeAndAfter
		s.AuditObligation = makeAuditForEffect(EffectRelaxBoundary)
		gov := &GovernancePolicy{Kind: k, Description: "Test"}
		if k == GovernanceDomainSpecific {
			gov.DomainCallback = "pkg.Callback"
		}
		s.Governance = gov
		if err := s.Validate(); err != nil {
			t.Errorf("valid governance kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidExemptionKinds(t *testing.T) {
	for k := range validExemptionKinds {
		s := validSpec()
		s.Exemptions = []Exemption{{Kind: k, Reason: "Test", Scope: "test", Waives: []WaivedObligation{WaiveTestRefs}}}
		if err := s.Validate(); err != nil {
			t.Errorf("valid exemption kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidDelegationKinds(t *testing.T) {
	for k := range validDelegationKinds {
		s := validSpec()
		s.DelegationKind = k
		if k != DelegationNone {
			s.DelegationDescription = "test"
		}
		if err := s.Validate(); err != nil {
			t.Errorf("valid delegation kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidAuthorityEvalKinds(t *testing.T) {
	for k := range validAuthorityEvalKinds {
		s := validSpec()
		s.AuthorityEval = k
		if err := s.Validate(); err != nil {
			t.Errorf("valid authority eval kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidInvariantKinds(t *testing.T) {
	for k := range validInvariantKinds {
		s := validSpec()
		fc := k == InvariantSecurity
		s.Invariants = []Invariant{{ID: "test", Description: "test", Kind: k, FailClosed: fc}}
		if err := s.Validate(); err != nil {
			t.Errorf("valid invariant kind %q caused error: %v", k, err)
		}
	}
}

// --- Duplicate detection ---

func TestValidate_DuplicateEntryPoints(t *testing.T) {
	s := validSpec()
	s.EntryPoints = []EntryPoint{
		{Kind: EntryPointHTTPRoute, Pattern: "/test", Method: "GET"},
		{Kind: EntryPointHTTPRoute, Pattern: "/test", Method: "GET"},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate entry point") {
		t.Errorf("expected duplicate entry point error, got: %v", err)
	}
}

func TestValidate_DuplicatePrincipals(t *testing.T) {
	s := validSpec()
	s.Principals = []PrincipalKind{PrincipalUser, PrincipalUser}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate principal kind") {
		t.Errorf("expected duplicate principal error, got: %v", err)
	}
}

func TestValidate_DuplicateEffects(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectReadOne, EffectReadOne}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate security effect") {
		t.Errorf("expected duplicate effect error, got: %v", err)
	}
}

func TestValidate_DuplicateDenialCodes(t *testing.T) {
	s := validSpec()
	s.DenialCodes = []DenialCode{DenialForbidden, DenialForbidden}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate denial code") {
		t.Errorf("expected duplicate denial code error, got: %v", err)
	}
}

func TestValidate_DuplicateTestRefs(t *testing.T) {
	s := validSpec()
	s.TestRefs = []TestRef{
		{Package: "pkg/hub", Function: "TestA"},
		{Package: "pkg/hub", Function: "TestA"},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate test ref") {
		t.Errorf("expected duplicate test ref error, got: %v", err)
	}
}

func TestValidate_DuplicateExemptions(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{
		makeExemption(ExemptionTestFixture, WaiveTestRefs),
		makeExemption(ExemptionTestFixture, WaiveTestRefs),
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate exemption kind") {
		t.Errorf("expected duplicate exemption error, got: %v", err)
	}
}

func TestValidate_DuplicateInvariantIDs(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{
		{ID: "inv", Description: "a", Kind: InvariantBusiness},
		{ID: "inv", Description: "b", Kind: InvariantBusiness},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), `duplicate ID "inv"`) {
		t.Errorf("expected duplicate invariant error, got: %v", err)
	}
}

// --- Governance validation ---

func TestValidate_GovernanceRequiresKindAndDescription(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectRelaxBoundary}
	s.AuthorityEval = AuthorityEvalBeforeAndAfter
	s.AuditObligation = makeAuditForEffect(EffectRelaxBoundary)

	t.Run("missing kind", func(t *testing.T) {
		s2 := s
		s2.Governance = &GovernancePolicy{Description: "desc"}
		err := s2.Validate()
		if err == nil || !strings.Contains(err.Error(), "kind is required") {
			t.Errorf("expected kind required, got: %v", err)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		s2 := s
		s2.Governance = &GovernancePolicy{Kind: "magic", Description: "desc"}
		err := s2.Validate()
		if err == nil || !strings.Contains(err.Error(), `unknown kind "magic"`) {
			t.Errorf("expected unknown kind, got: %v", err)
		}
	})

	t.Run("domain_specific needs callback", func(t *testing.T) {
		s2 := s
		s2.Governance = &GovernancePolicy{Kind: GovernanceDomainSpecific, Description: "desc"}
		err := s2.Validate()
		if err == nil || !strings.Contains(err.Error(), "domain_specific kind requires a non-empty domain callback") {
			t.Errorf("expected callback required, got: %v", err)
		}
	})
}

// --- Cross-spec validation ---

func TestValidateSpecs_DuplicateIDs(t *testing.T) {
	s1, s2 := validSpec(), validSpec()
	s2.EntryPoints = []EntryPoint{{Kind: EntryPointHTTPRoute, Pattern: "/other", Method: "POST"}}
	err := ValidateSpecs([]OperationSpec{s1, s2})
	if err == nil || !strings.Contains(err.Error(), "duplicate operation ID") {
		t.Errorf("expected duplicate ID error, got: %v", err)
	}
}

func TestValidateSpecs_DuplicateEntryPoints(t *testing.T) {
	s1, s2 := validSpec(), validSpec()
	s2.ID = "test.other"
	err := ValidateSpecs([]OperationSpec{s1, s2})
	if err == nil || !strings.Contains(err.Error(), "is claimed by both") {
		t.Errorf("expected cross-spec duplicate, got: %v", err)
	}
}

func TestValidateSpecs_ValidDistinct(t *testing.T) {
	s1, s2 := validSpec(), validSpec()
	s2.ID = "test.other"
	s2.EntryPoints = []EntryPoint{{Kind: EntryPointHTTPRoute, Pattern: "/other", Method: "POST"}}
	if err := ValidateSpecs([]OperationSpec{s1, s2}); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

// --- SecurityEffect method tests ---

func TestSecurityEffect_IsAuthorityEffect(t *testing.T) {
	authority := []SecurityEffect{EffectGrantAuthority, EffectChangeAuthority, EffectRevokeAuthority, EffectChangeOwnership}
	nonAuthority := []SecurityEffect{EffectReadOne, EffectListScoped, EffectCreateResource, EffectDeleteResource, EffectRelaxBoundary}
	for _, e := range authority {
		if !e.IsAuthorityEffect() {
			t.Errorf("%q should be authority effect", e)
		}
	}
	for _, e := range nonAuthority {
		if e.IsAuthorityEffect() {
			t.Errorf("%q should not be authority effect", e)
		}
	}
}

func TestSecurityEffect_IsBoundaryEffect(t *testing.T) {
	if !EffectRelaxBoundary.IsBoundaryEffect() {
		t.Error("relax should be boundary")
	}
	if !EffectTightenBoundary.IsBoundaryEffect() {
		t.Error("tighten should be boundary")
	}
	if EffectGrantAuthority.IsBoundaryEffect() {
		t.Error("grant should not be boundary")
	}
}

func TestSecurityEffect_RequiresAudit(t *testing.T) {
	if EffectReadOne.RequiresAudit() {
		t.Error("read should not require audit")
	}
	if !EffectDeleteResource.RequiresAudit() {
		t.Error("delete should require audit")
	}
	if EffectCreateResource.RequiresAudit() {
		t.Error("create should not require audit by default")
	}
}

// --- Error accumulation ---

func TestValidate_MultipleErrors(t *testing.T) {
	err := (&OperationSpec{}).Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	// Zero-value spec misses: ID, domain, description, entry points,
	// principals, credentials, base permission, resource resolver,
	// effects, denial codes, test refs = 11 minimum
	if len(ve.Errors) < 11 {
		t.Errorf("expected at least 11 errors, got %d: %v", len(ve.Errors), err)
	}
}

func TestValidationError_Format(t *testing.T) {
	err := (&OperationSpec{}).Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.HasPrefix(err.Error(), "operation spec validation failed:") {
		t.Errorf("unexpected format: %s", err.Error())
	}
}

// --- Renderer tests ---

func TestRenderMarkdown_Determinism(t *testing.T) {
	specs := []OperationSpec{validSpec(), validAuthoritySpec()}
	if RenderMarkdown(specs) != RenderMarkdown(specs) {
		t.Error("renderer not deterministic")
	}
}

func TestRenderMarkdown_SmokeSections(t *testing.T) {
	s := validAuthoritySpec()
	s.Invariants = []Invariant{{ID: "last-owner", Description: "Must not orphan", Kind: InvariantSecurity, FailClosed: true}}
	s.DenialCodes = []DenialCode{DenialLastOwner}
	out := RenderMarkdown([]OperationSpec{s})

	checks := []string{
		"# Authorization Operation Catalog",
		"**Operations:** 1",
		"## test.authority",
		"**Domain:** test",
		"### Entry Points",
		"**Principals:**",
		"**Credentials:**",
		"**Base Permission:** `project.manage`",
		"**Resource Resolver:** project-from-url",
		"`grant-authority`",
		"### Delegation",
		"`non_amplification`",
		"### Audit",
		"**Event Type:** `test.grant`",
		"**After Fields:** target_role",
		"**Atomic:** Yes",
		"`last_owner`",
		"### Tests",
		"[test.authority](#testauthority)",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output", want)
		}
	}
}

func TestRenderMarkdown_Empty(t *testing.T) {
	out := RenderMarkdown(nil)
	if !strings.Contains(out, "**Operations:** 0") {
		t.Error("empty catalog should show 0")
	}
}

func TestRenderMarkdown_PipeEscaping(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{{ID: "pipe|test", Description: "A | in desc", Kind: InvariantBusiness}}
	out := RenderMarkdown([]OperationSpec{s})
	if !strings.Contains(out, `pipe\|test`) {
		t.Error("expected escaped pipe in invariant ID")
	}
}

func TestRenderMarkdown_ExternalPolicy(t *testing.T) {
	out := RenderMarkdown([]OperationSpec{validExternalSpec()})
	if !strings.Contains(out, "### External Effect Policy") {
		t.Error("missing external policy section")
	}
	if !strings.Contains(out, "`fire_and_forget`") {
		t.Error("missing delivery mode")
	}
}

func TestRenderMarkdown_Credentials(t *testing.T) {
	out := RenderMarkdown([]OperationSpec{validSpec()})
	if !strings.Contains(out, "**Credentials:**") {
		t.Error("missing credentials section")
	}
	if !strings.Contains(out, "`session_jwt`") {
		t.Error("missing credential value")
	}
}

// --- Test helpers for effects ---

func makeAuditForEffect(eff SecurityEffect) *AuditObligation {
	req := effectAuditFieldRequirements[eff]
	a := &AuditObligation{
		EventType:     "test",
		ContextFields: []string{"actor_id"},
		Atomic:        true,
	}
	if req.NeedsBefore {
		a.BeforeFields = []string{"state"}
	}
	if req.NeedsAfter {
		a.AfterFields = []string{"state"}
	}
	return a
}

func makeExternalPolicy() *ExternalEffectPolicy {
	return &ExternalEffectPolicy{
		DeliveryMode:   DeliveryFireAndForget,
		FailureMode:    FailureLogAndContinue,
		IdempotencyKey: "dispatch ID",
		RetryPolicy:    "no retry",
		AuthBeforeEmit: true,
	}
}
