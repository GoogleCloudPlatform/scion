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

// validSpec returns a minimal valid OperationSpec for use in tests.
// Callers may modify specific fields before calling Validate().
func validSpec() OperationSpec {
	return OperationSpec{
		ID:          "test.operation",
		Domain:      "test",
		Description: "A test operation",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		ResourceResolver: "test-resolver",
		BasePermission:   "test.read",
		Effects:          []SecurityEffect{EffectReadOne},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestOperation"},
		},
	}
}

// validAuthoritySpec returns a valid spec with authority effects that
// requires delegation and governance policies.
func validAuthoritySpec() OperationSpec {
	s := validSpec()
	s.ID = "test.authority"
	s.Effects = []SecurityEffect{EffectGrantAuthority}
	s.Delegation = &DelegationPolicy{
		RequireNonAmplification: true,
		Description:             "Actor must hold all permissions being granted",
	}
	s.Governance = &GovernancePolicy{
		Kind:        GovernancePeerSuperior,
		Description: "Peer/superior governance check",
	}
	s.AuditObligation = &AuditObligation{
		EventType:      "test.grant",
		RequiredFields: []string{"target_role"},
	}
	return s
}

func TestValidate_MinimalValidSpec(t *testing.T) {
	s := validSpec()
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid spec, got error: %v", err)
	}
}

func TestValidate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*OperationSpec)
		wantErr string
	}{
		{
			name:    "missing ID",
			mutate:  func(s *OperationSpec) { s.ID = "" },
			wantErr: "operation ID is required",
		},
		{
			name:    "missing domain",
			mutate:  func(s *OperationSpec) { s.Domain = "" },
			wantErr: "domain is required",
		},
		{
			name:    "missing description",
			mutate:  func(s *OperationSpec) { s.Description = "" },
			wantErr: "description is required",
		},
		{
			name:    "missing entry points without exemption",
			mutate:  func(s *OperationSpec) { s.EntryPoints = nil },
			wantErr: "at least one entry point is required",
		},
		{
			name:    "missing principals without exemption",
			mutate:  func(s *OperationSpec) { s.Principals = nil },
			wantErr: "at least one principal kind is required",
		},
		{
			name:    "missing base permission without exemption",
			mutate:  func(s *OperationSpec) { s.BasePermission = "" },
			wantErr: "base permission is required",
		},
		{
			name:    "missing resource resolver without exemption",
			mutate:  func(s *OperationSpec) { s.ResourceResolver = "" },
			wantErr: "resource resolver is required",
		},
		{
			name:    "missing effects",
			mutate:  func(s *OperationSpec) { s.Effects = nil },
			wantErr: "at least one security effect is required",
		},
		{
			name:    "missing test refs",
			mutate:  func(s *OperationSpec) { s.TestRefs = nil },
			wantErr: "at least one test reference is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidate_ExemptionsBypassEntryPointAndPrincipalRequirements(t *testing.T) {
	s := validSpec()
	s.EntryPoints = nil
	s.Principals = nil
	s.BasePermission = ""
	s.ResourceResolver = ""
	s.Exemptions = []Exemption{
		{Kind: ExemptionInternalOnly, Reason: "Internal-only operation", Scope: "all paths"},
	}
	// Still need effects and test refs (test fixture exempts tests)
	s.TestRefs = nil
	s.Exemptions = append(s.Exemptions, Exemption{
		Kind: ExemptionTestFixture, Reason: "Test-only operation", Scope: "test only",
	})

	if err := s.Validate(); err != nil {
		t.Errorf("expected exemptions to bypass requirements, got: %v", err)
	}
}

func TestValidate_ResourceResolverExemptionBehavior(t *testing.T) {
	t.Run("missing resolver without exemption fails", func(t *testing.T) {
		s := validSpec()
		s.ResourceResolver = ""
		err := s.Validate()
		if err == nil {
			t.Fatal("expected error for missing resource resolver")
		}
		if !strings.Contains(err.Error(), "resource resolver is required") {
			t.Errorf("expected resource resolver error, got: %v", err)
		}
	})

	t.Run("missing resolver with exemption passes", func(t *testing.T) {
		s := validSpec()
		s.ResourceResolver = ""
		s.Exemptions = []Exemption{
			{Kind: ExemptionPublicEndpoint, Reason: "No resource scope needed for public endpoint", Scope: "public routes"},
		}
		if err := s.Validate(); err != nil {
			t.Errorf("expected exemption to bypass resource resolver requirement, got: %v", err)
		}
	})

	t.Run("present resolver passes", func(t *testing.T) {
		s := validSpec()
		// ResourceResolver is set by validSpec()
		if err := s.Validate(); err != nil {
			t.Errorf("expected valid spec, got: %v", err)
		}
	})
}

func TestValidate_UnknownEntryPointKind(t *testing.T) {
	s := validSpec()
	s.EntryPoints = []EntryPoint{
		{Kind: "unknown_kind", Pattern: "/test", Method: "GET"},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for unknown entry point kind")
	}
	if !strings.Contains(err.Error(), `unknown kind "unknown_kind"`) {
		t.Errorf("expected unknown kind error, got: %v", err)
	}
}

func TestValidate_HTTPRouteRequiresMethod(t *testing.T) {
	s := validSpec()
	s.EntryPoints = []EntryPoint{
		{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test"},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for HTTP route without method")
	}
	if !strings.Contains(err.Error(), "method is required for HTTP routes") {
		t.Errorf("expected method required error, got: %v", err)
	}
}

func TestValidate_DuplicateEntryPoints(t *testing.T) {
	s := validSpec()
	s.EntryPoints = []EntryPoint{
		{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test", Method: "GET"},
		{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test", Method: "GET"},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate entry points")
	}
	if !strings.Contains(err.Error(), "duplicate entry point") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

func TestValidate_UnknownPrincipalKind(t *testing.T) {
	s := validSpec()
	s.Principals = []PrincipalKind{"alien"}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for unknown principal kind")
	}
	if !strings.Contains(err.Error(), `unknown kind "alien"`) {
		t.Errorf("expected unknown kind error, got: %v", err)
	}
}

func TestValidate_UnknownSecurityEffect(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{"teleport"}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for unknown security effect")
	}
	if !strings.Contains(err.Error(), `unknown security effect "teleport"`) {
		t.Errorf("expected unknown effect error, got: %v", err)
	}
}

func TestValidate_AuthorityEffectRequiresDelegation(t *testing.T) {
	tests := []SecurityEffect{
		EffectGrantAuthority,
		EffectChangeAuthority,
		EffectRevokeAuthority,
		EffectChangeOwnership,
	}

	for _, eff := range tests {
		t.Run(string(eff), func(t *testing.T) {
			s := validSpec()
			s.Effects = []SecurityEffect{eff}
			s.Delegation = nil
			s.AuditObligation = &AuditObligation{EventType: "test"}
			err := s.Validate()
			if err == nil {
				t.Fatal("expected error for authority effect without delegation")
			}
			if !strings.Contains(err.Error(), "authority effects require a delegation policy") {
				t.Errorf("expected delegation required error, got: %v", err)
			}
		})
	}
}

func TestValidate_BoundaryEffectRequiresGovernance(t *testing.T) {
	tests := []SecurityEffect{
		EffectRelaxBoundary,
		EffectTightenBoundary,
	}

	for _, eff := range tests {
		t.Run(string(eff), func(t *testing.T) {
			s := validSpec()
			s.Effects = []SecurityEffect{eff}
			s.Governance = nil
			s.AuditObligation = &AuditObligation{EventType: "test"}
			err := s.Validate()
			if err == nil {
				t.Fatal("expected error for boundary effect without governance")
			}
			if !strings.Contains(err.Error(), "boundary effects require a governance policy") {
				t.Errorf("expected governance required error, got: %v", err)
			}
		})
	}
}

func TestValidate_AuditRequiringEffectsNeedAuditObligation(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectDeleteResource}
	s.AuditObligation = nil
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for audit-requiring effect without obligation")
	}
	if !strings.Contains(err.Error(), "effects requiring audit must have an audit obligation") {
		t.Errorf("expected audit obligation error, got: %v", err)
	}
}

func TestValidate_ReadEffectsDoNotRequireAudit(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectReadOne}
	s.AuditObligation = nil
	if err := s.Validate(); err != nil {
		t.Errorf("read-one should not require audit, got: %v", err)
	}
}

func TestValidate_GovernanceRequiresKindAndDescription(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectRelaxBoundary}
	s.AuditObligation = &AuditObligation{EventType: "test"}

	t.Run("missing kind", func(t *testing.T) {
		s2 := s
		s2.Governance = &GovernancePolicy{Description: "desc"}
		err := s2.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "governance policy: kind is required") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		s2 := s
		s2.Governance = &GovernancePolicy{Kind: "magic", Description: "desc"}
		err := s2.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), `unknown kind "magic"`) {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("missing description", func(t *testing.T) {
		s2 := s
		s2.Governance = &GovernancePolicy{Kind: GovernancePeerSuperior}
		err := s2.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "governance policy: description is required") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestValidate_DuplicateInvariantIDs(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{
		{ID: "inv-1", Description: "First"},
		{ID: "inv-1", Description: "Duplicate"},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate invariant IDs")
	}
	if !strings.Contains(err.Error(), `duplicate ID "inv-1"`) {
		t.Errorf("expected duplicate ID error, got: %v", err)
	}
}

func TestValidate_InvariantRequiresIDAndDescription(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{{}}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for empty invariant")
	}
	if !strings.Contains(err.Error(), "ID is required") {
		t.Errorf("expected ID required error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "description is required") {
		t.Errorf("expected description required error, got: %v", err)
	}
}

func TestValidate_EmptyDenialCode(t *testing.T) {
	s := validSpec()
	s.DenialCodes = []DenialCode{""}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for empty denial code")
	}
	if !strings.Contains(err.Error(), "empty denial code") {
		t.Errorf("expected empty denial code error, got: %v", err)
	}
}

func TestValidate_TestRefRequiresPackageAndFunction(t *testing.T) {
	s := validSpec()
	s.TestRefs = []TestRef{{}}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for empty test ref")
	}
	if !strings.Contains(err.Error(), "package is required") {
		t.Errorf("expected package required error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "function is required") {
		t.Errorf("expected function required error, got: %v", err)
	}
}

func TestValidate_ExemptionRequiresKindReasonAndScope(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{{}}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for empty exemption")
	}
	if !strings.Contains(err.Error(), "kind is required") {
		t.Errorf("expected kind required error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "reason is required") {
		t.Errorf("expected reason required error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "scope is required") {
		t.Errorf("expected scope required error, got: %v", err)
	}
}

func TestValidate_UnknownExemptionKind(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{
		{Kind: "wishful_thinking", Reason: "Because", Scope: "all"},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for unknown exemption kind")
	}
	if !strings.Contains(err.Error(), `unknown kind "wishful_thinking"`) {
		t.Errorf("expected unknown kind error, got: %v", err)
	}
}

func TestValidate_AuditObligationRequiresEventType(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectDeleteResource}
	s.AuditObligation = &AuditObligation{}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for empty audit event type")
	}
	if !strings.Contains(err.Error(), "event type is required") {
		t.Errorf("expected event type error, got: %v", err)
	}
}

func TestValidate_FullAuthoritySpec(t *testing.T) {
	s := validAuthoritySpec()
	if err := s.Validate(); err != nil {
		t.Errorf("expected valid authority spec, got: %v", err)
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	s := OperationSpec{} // totally empty
	err := s.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	// Should have at least: ID, domain, description, entry points,
	// principals, base permission, resource resolver, effects, test refs
	if len(ve.Errors) < 9 {
		t.Errorf("expected at least 9 errors, got %d: %v", len(ve.Errors), err)
	}
}

// --- ValidateSpecs cross-spec tests ---

func TestValidateSpecs_DuplicateIDs(t *testing.T) {
	s1 := validSpec()
	s2 := validSpec()
	s2.EntryPoints = []EntryPoint{
		{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/other", Method: "POST"},
	}
	err := ValidateSpecs([]OperationSpec{s1, s2})
	if err == nil {
		t.Fatal("expected error for duplicate operation IDs")
	}
	if !strings.Contains(err.Error(), "duplicate operation ID") {
		t.Errorf("expected duplicate ID error, got: %v", err)
	}
}

func TestValidateSpecs_DuplicateEntryPointsAcrossSpecs(t *testing.T) {
	s1 := validSpec()
	s2 := validSpec()
	s2.ID = "test.other"
	// Same entry point as s1
	err := ValidateSpecs([]OperationSpec{s1, s2})
	if err == nil {
		t.Fatal("expected error for duplicate entry points across specs")
	}
	if !strings.Contains(err.Error(), "is claimed by both") {
		t.Errorf("expected cross-spec duplicate error, got: %v", err)
	}
}

func TestValidateSpecs_ValidDistinctSpecs(t *testing.T) {
	s1 := validSpec()
	s2 := validSpec()
	s2.ID = "test.other"
	s2.EntryPoints = []EntryPoint{
		{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/other", Method: "POST"},
	}
	if err := ValidateSpecs([]OperationSpec{s1, s2}); err != nil {
		t.Errorf("expected valid specs, got: %v", err)
	}
}

// --- SecurityEffect method tests ---

func TestSecurityEffect_IsAuthorityEffect(t *testing.T) {
	authority := []SecurityEffect{
		EffectGrantAuthority, EffectChangeAuthority,
		EffectRevokeAuthority, EffectChangeOwnership,
	}
	nonAuthority := []SecurityEffect{
		EffectReadOne, EffectListScoped, EffectCreateResource,
		EffectDeleteResource, EffectRelaxBoundary,
	}

	for _, e := range authority {
		if !e.IsAuthorityEffect() {
			t.Errorf("%q should be an authority effect", e)
		}
	}
	for _, e := range nonAuthority {
		if e.IsAuthorityEffect() {
			t.Errorf("%q should not be an authority effect", e)
		}
	}
}

func TestSecurityEffect_IsBoundaryEffect(t *testing.T) {
	if !EffectRelaxBoundary.IsBoundaryEffect() {
		t.Error("relax-boundary should be a boundary effect")
	}
	if !EffectTightenBoundary.IsBoundaryEffect() {
		t.Error("tighten-boundary should be a boundary effect")
	}
	if EffectGrantAuthority.IsBoundaryEffect() {
		t.Error("grant-authority should not be a boundary effect")
	}
}

func TestSecurityEffect_RequiresAudit(t *testing.T) {
	// Read effects should not require audit.
	if EffectReadOne.RequiresAudit() {
		t.Error("read-one should not require audit")
	}
	if EffectListScoped.RequiresAudit() {
		t.Error("list-scoped should not require audit")
	}
	// Mutations should require audit.
	if !EffectDeleteResource.RequiresAudit() {
		t.Error("delete-resource should require audit")
	}
	if !EffectGrantAuthority.RequiresAudit() {
		t.Error("grant-authority should require audit")
	}
	// Create and update resource are not in auditRequiredEffects by default.
	if EffectCreateResource.RequiresAudit() {
		t.Error("create-resource should not require audit by default")
	}
}

func TestValidationError_ErrorMessage(t *testing.T) {
	s := OperationSpec{}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "operation spec validation failed:") {
		t.Errorf("unexpected error format: %s", msg)
	}
}

func TestValidate_NonHTTPEntryPointDoesNotRequireMethod(t *testing.T) {
	s := validSpec()
	s.EntryPoints = []EntryPoint{
		{Kind: EntryPointSchedulerJob, Pattern: "agent-heartbeat-timeout"},
	}
	if err := s.Validate(); err != nil {
		t.Errorf("scheduler job should not require method, got: %v", err)
	}
}

func TestValidate_AllValidEntryPointKinds(t *testing.T) {
	kinds := []EntryPointKind{
		EntryPointHTTPRoute, EntryPointBrokerCall,
		EntryPointSchedulerJob, EntryPointCLICommand,
		EntryPointBackgroundJob, EntryPointInternalDispatch,
	}
	for _, k := range kinds {
		s := validSpec()
		ep := EntryPoint{Kind: k, Pattern: "/test"}
		if k == EntryPointHTTPRoute {
			ep.Method = "GET"
		}
		s.EntryPoints = []EntryPoint{ep}
		if err := s.Validate(); err != nil {
			t.Errorf("valid entry point kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidPrincipalKinds(t *testing.T) {
	kinds := []PrincipalKind{
		PrincipalUser, PrincipalAgent, PrincipalScopedUAT,
		PrincipalBroker, PrincipalServiceAccount, PrincipalSystem,
	}
	for _, k := range kinds {
		s := validSpec()
		s.Principals = []PrincipalKind{k}
		if err := s.Validate(); err != nil {
			t.Errorf("valid principal kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidGovernanceKinds(t *testing.T) {
	kinds := []GovernanceKind{
		GovernancePeerSuperior, GovernanceOwnershipAncestry,
		GovernanceProtectedPrincipal, GovernanceConstraintAdmin,
		GovernanceDomainSpecific,
	}
	for _, k := range kinds {
		s := validSpec()
		s.Effects = []SecurityEffect{EffectRelaxBoundary}
		s.AuditObligation = &AuditObligation{EventType: "test"}
		gov := &GovernancePolicy{Kind: k, Description: "Test"}
		if k == GovernanceDomainSpecific {
			gov.DomainCallback = "pkg.TestCallback"
		}
		s.Governance = gov
		if err := s.Validate(); err != nil {
			t.Errorf("valid governance kind %q caused error: %v", k, err)
		}
	}
}

func TestValidate_AllValidExemptionKinds(t *testing.T) {
	kinds := []ExemptionKind{
		ExemptionOfflineRecovery, ExemptionDeterministicSeed,
		ExemptionMigration, ExemptionTestFixture,
		ExemptionAuthenticationOnly, ExemptionPublicEndpoint,
		ExemptionInternalOnly,
	}
	for _, k := range kinds {
		s := validSpec()
		s.Exemptions = []Exemption{{Kind: k, Reason: "Test reason", Scope: "test only"}}
		if err := s.Validate(); err != nil {
			t.Errorf("valid exemption kind %q caused error: %v", k, err)
		}
	}
}

// --- N1: Duplicate detection tests ---

func TestValidate_DuplicatePrincipals(t *testing.T) {
	s := validSpec()
	s.Principals = []PrincipalKind{PrincipalUser, PrincipalUser}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate principals")
	}
	if !strings.Contains(err.Error(), `duplicate principal kind "user"`) {
		t.Errorf("expected duplicate principal error, got: %v", err)
	}
}

func TestValidate_DuplicateEffects(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectReadOne, EffectReadOne}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate effects")
	}
	if !strings.Contains(err.Error(), `duplicate security effect "read-one"`) {
		t.Errorf("expected duplicate effect error, got: %v", err)
	}
}

func TestValidate_DuplicateDenialCodes(t *testing.T) {
	s := validSpec()
	s.DenialCodes = []DenialCode{DenialForbidden, DenialForbidden}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate denial codes")
	}
	if !strings.Contains(err.Error(), `duplicate denial code "forbidden"`) {
		t.Errorf("expected duplicate denial code error, got: %v", err)
	}
}

func TestValidate_DuplicateTestRefs(t *testing.T) {
	s := validSpec()
	s.TestRefs = []TestRef{
		{Package: "pkg/hub", Function: "TestFoo"},
		{Package: "pkg/hub", Function: "TestFoo"},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate test refs")
	}
	if !strings.Contains(err.Error(), "duplicate test ref") {
		t.Errorf("expected duplicate test ref error, got: %v", err)
	}
}

func TestValidate_DistinctDuplicatesPass(t *testing.T) {
	s := validSpec()
	s.Principals = []PrincipalKind{PrincipalUser, PrincipalAgent}
	s.Effects = []SecurityEffect{EffectReadOne, EffectListScoped}
	s.DenialCodes = []DenialCode{DenialForbidden, DenialLastOwner}
	s.TestRefs = []TestRef{
		{Package: "pkg/hub", Function: "TestA"},
		{Package: "pkg/hub", Function: "TestB"},
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected distinct values to pass, got: %v", err)
	}
}

// --- N5: RenderMarkdown tests ---

func TestRenderMarkdown_Determinism(t *testing.T) {
	specs := []OperationSpec{validSpec(), validAuthoritySpec()}
	out1 := RenderMarkdown(specs)
	out2 := RenderMarkdown(specs)
	if out1 != out2 {
		t.Error("RenderMarkdown is not deterministic: two calls with same input produced different output")
	}
}

func TestRenderMarkdown_SmokeSections(t *testing.T) {
	s := validAuthoritySpec()
	s.Invariants = []Invariant{
		{ID: "last-owner", Description: "Must not orphan project", FailClosed: true},
	}
	s.DenialCodes = []DenialCode{DenialLastOwner}
	out := RenderMarkdown([]OperationSpec{s})

	checks := []struct {
		label string
		want  string
	}{
		{"catalog heading", "# Authorization Operation Catalog"},
		{"operation count", "**Operations:** 1"},
		{"operation ID heading", "## test.authority"},
		{"domain", "**Domain:** test"},
		{"entry points table", "### Entry Points"},
		{"principals", "**Principals:**"},
		{"base permission", "**Base Permission:** `test.read`"},
		{"resource resolver", "**Resource Resolver:** test-resolver"},
		{"effects", "`grant-authority`"},
		{"delegation section", "### Delegation"},
		{"governance section", "### Governance"},
		{"invariants table", "### Invariants"},
		{"invariant ID", "last-owner"},
		{"audit section", "### Audit"},
		{"denial codes", "`LAST_OWNER`"},
		{"test refs", "### Tests"},
		{"toc link", "[test.authority](#testauthority)"},
	}

	for _, c := range checks {
		if !strings.Contains(out, c.want) {
			t.Errorf("missing %s: expected output to contain %q", c.label, c.want)
		}
	}
}

func TestRenderMarkdown_Empty(t *testing.T) {
	out := RenderMarkdown(nil)
	if !strings.Contains(out, "**Operations:** 0") {
		t.Error("empty catalog should show 0 operations")
	}
}

func TestRenderMarkdown_PipeEscaping(t *testing.T) {
	s := validSpec()
	s.Invariants = []Invariant{
		{ID: "pipe|test", Description: "A | in description", FailClosed: false},
	}
	out := RenderMarkdown([]OperationSpec{s})
	if strings.Contains(out, "| pipe|test |") {
		t.Error("pipe character in invariant ID should be escaped")
	}
	if !strings.Contains(out, `pipe\|test`) {
		t.Error("expected escaped pipe in invariant ID")
	}
}

// --- O1: Exemption.Scope validation tests ---

func TestValidate_ExemptionMissingScopeFails(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{
		{Kind: ExemptionTestFixture, Reason: "Test fixture", Scope: ""},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for exemption without scope")
	}
	if !strings.Contains(err.Error(), "scope is required") {
		t.Errorf("expected scope required error, got: %v", err)
	}
}

func TestValidate_ExemptionWithScopePasses(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{
		{Kind: ExemptionTestFixture, Reason: "Test fixture", Scope: "test only"},
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected exemption with scope to pass, got: %v", err)
	}
}

// --- O2: Duplicate exemption detection tests ---

func TestValidate_DuplicateExemptions(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{
		{Kind: ExemptionTestFixture, Reason: "First", Scope: "test only"},
		{Kind: ExemptionTestFixture, Reason: "Second", Scope: "test only"},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate exemptions")
	}
	if !strings.Contains(err.Error(), `duplicate exemption kind "test_fixture"`) {
		t.Errorf("expected duplicate exemption error, got: %v", err)
	}
}

func TestValidate_DistinctExemptionsPasses(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{
		{Kind: ExemptionTestFixture, Reason: "Test fixture", Scope: "test only"},
		{Kind: ExemptionInternalOnly, Reason: "Internal operation", Scope: "all paths"},
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected distinct exemptions to pass, got: %v", err)
	}
}

func TestValidate_SameKindDifferentScopePasses(t *testing.T) {
	s := validSpec()
	s.Exemptions = []Exemption{
		{Kind: ExemptionTestFixture, Reason: "Unit tests", Scope: "unit tests"},
		{Kind: ExemptionTestFixture, Reason: "Integration tests", Scope: "integration tests"},
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected same kind with different scopes to pass, got: %v", err)
	}
}

// --- O3: GovernanceDomainSpecific requires DomainCallback ---

func TestValidate_DomainSpecificGovernanceRequiresCallback(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectRelaxBoundary}
	s.AuditObligation = &AuditObligation{EventType: "test"}
	s.Governance = &GovernancePolicy{
		Kind:        GovernanceDomainSpecific,
		Description: "Custom governance",
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected error for domain_specific governance without callback")
	}
	if !strings.Contains(err.Error(), "domain_specific kind requires a non-empty domain callback") {
		t.Errorf("expected domain callback error, got: %v", err)
	}
}

func TestValidate_DomainSpecificGovernanceWithCallbackPasses(t *testing.T) {
	s := validSpec()
	s.Effects = []SecurityEffect{EffectRelaxBoundary}
	s.AuditObligation = &AuditObligation{EventType: "test"}
	s.Governance = &GovernancePolicy{
		Kind:           GovernanceDomainSpecific,
		Description:    "Custom governance",
		DomainCallback: "authz.CheckCustomGovernance",
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected domain_specific with callback to pass, got: %v", err)
	}
}

func TestValidate_NonDomainSpecificGovernanceIgnoresCallback(t *testing.T) {
	// peer_superior governance should pass without a DomainCallback
	s := validSpec()
	s.Effects = []SecurityEffect{EffectRelaxBoundary}
	s.AuditObligation = &AuditObligation{EventType: "test"}
	s.Governance = &GovernancePolicy{
		Kind:        GovernancePeerSuperior,
		Description: "Peer/superior check",
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected non-domain-specific governance to pass without callback, got: %v", err)
	}
}
