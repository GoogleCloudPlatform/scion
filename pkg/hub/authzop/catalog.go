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

// EntryPointExemption documents a route or entry point that does not map to an
// authorization operation. Each exemption is typed, scoped, owned, and
// rationalized. Invalid or stale exemptions fail CI.
type EntryPointExemption struct {
	// Pattern is the route pattern or entry point identifier (matches
	// routeMetadataTable keys or non-HTTP entry point names).
	Pattern string

	// Kind classifies the exemption.
	Kind ExemptionKind

	// Reason explains why this entry point is exempt from operation mapping.
	Reason string

	// Owner identifies the reviewer or team responsible for this exemption.
	Owner string
}

// MutationClassification maps a discovered security-relevant mutation call
// site to its cataloged operation or reviewed exemption.
type MutationClassification struct {
	// File is the source file path relative to the project root.
	File string

	// Function is the enclosing function name.
	Function string

	// Symbol is the mutation method/function name.
	Symbol string

	// OperationID maps to a Catalog operation (empty if Exemption is set).
	OperationID OperationID

	// Exemption documents why this site is exempt from operation mapping.
	// Exactly one of OperationID or Exemption must be set.
	Exemption *MutationExemption
}

// MutationExemption documents why a specific mutation call site is exempt.
type MutationExemption struct {
	Kind   ExemptionKind
	Reason string
	Scope  string
}

// SecurityMutationSymbols is the closed set of method/function names that
// represent security-relevant mutations. The authorization audit scanner
// discovers call sites matching these symbols and requires each to be
// classified.
var SecurityMutationSymbols = map[string]string{
	// Authority mutations (RoleBinding)
	"CreateRoleBinding": "grant-authority",
	"DeleteRoleBinding": "revoke-authority",
	"UpdateRoleBinding": "change-authority",

	// Role definition mutations
	"CreateRoleDefinition": "create-resource",
	"UpdateRoleDefinition": "update-resource",
	"DeleteRoleDefinition": "delete-resource",

	// Group membership mutations
	"AddGroupMember":    "grant-authority",
	"RemoveGroupMember": "revoke-authority",

	// Access constraint mutations
	"CreateAccessConstraint": "tighten-boundary",
	"UpdateAccessConstraint": "change-authority",
	"DeleteAccessConstraint": "relax-boundary",

	// Principal status mutations
	"SuspendUser":    "change-principal-status",
	"ReactivateUser": "change-principal-status",

	// Credential/token mutations
	"CreateToken":     "mint-credential",
	"RevokeToken":     "revoke-authority",
	"RevokeAllTokens": "revoke-authority",

	// GCP service account mutations
	"CreateGCPServiceAccount": "assign-credential",
	"DeleteGCPServiceAccount": "delete-resource",
	"VerifyGCPServiceAccount": "assign-credential",

	// Project ownership/lifecycle
	"DeleteProject": "delete-resource",

	// Agent lifecycle
	"DeleteAgent": "delete-resource",
}

// Catalog is the authoritative operation catalog. Every externally reachable
// high-risk entry point maps to exactly one OperationSpec here or to an
// EntryPointExemption. The catalog is validated by TestCatalogValidation.
//
// Operations are organized by domain. Each spec uses the frozen
// OperationSpec vocabulary from CT1.
var Catalog = []OperationSpec{
	// =====================================================================
	// Domain: project.membership — project member management
	// Governance appendix: authorization-governance-project-membership.md
	// =====================================================================
	{
		ID:          "project.membership.add",
		Domain:      "project.membership",
		Description: "Add a member to a project with a specified role",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/members", Method: "POST"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT},
		ResourceResolver:      "project-from-url",
		BasePermission:        "project.manage",
		Effects:               []SecurityEffect{EffectGrantAuthority},
		DelegationKind:        DelegationNonAmplification,
		DelegationDescription: "Actor must hold all permissions in the target role (CanDelegate non-amplification)",
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "C0 containment: only project-owner may add members. CT1 D5 approved governance matrix applies in RS1.",
		},
		AuthorityEval: AuthorityEvalNone,
		Invariants: []Invariant{
			{ID: "direct-user-only-owner", Description: "project-owner role is direct-user-only", Kind: InvariantSecurity, FailClosed: true},
			{ID: "single-binding-per-principal", Description: "CT1 D4: one direct binding per principal per project", Kind: InvariantBusiness, FailClosed: false},
		},
		AuditObligation: &AuditObligation{
			EventType:     "project.membership.add",
			ContextFields: []string{"actor_id", "project_id"},
			AfterFields:   []string{"target_principal_id", "target_role"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialRoleAssignmentForbidden, DenialTargetRoleProtected, DenialPrincipalIneligible},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "project.membership.update",
		Domain:      "project.membership",
		Description: "Change a project member's role",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/members/{memberId}", Method: "PATCH"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT},
		ResourceResolver:      "project-from-url",
		BasePermission:        "project.manage",
		Effects:               []SecurityEffect{EffectChangeAuthority},
		DelegationKind:        DelegationConditionalIncrease,
		DelegationDescription: "CanDelegate checked when new role has more permissions than old role",
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "C0 containment: only project-owner may change roles. CT1 D5 governance matrix applies in RS1.",
		},
		AuthorityEval: AuthorityEvalBeforeAndAfter,
		Invariants: []Invariant{
			{ID: "direct-user-only-owner", Description: "project-owner role is direct-user-only", Kind: InvariantSecurity, FailClosed: true},
			{ID: "last-owner-guard", Description: "Cannot demote the last active direct owner", Kind: InvariantSecurity, FailClosed: true},
		},
		AuditObligation: &AuditObligation{
			EventType:     "project.membership.update",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"target_principal_id", "old_role"},
			AfterFields:   []string{"new_role"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialRoleAssignmentForbidden, DenialTargetRoleProtected, DenialLastOwner},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "project.membership.remove",
		Domain:      "project.membership",
		Description: "Remove a member from a project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/members/{memberId}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.manage",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "C0 containment: only project-owner may remove members. CT1 D1 allows self-removal when another active direct owner remains.",
		},
		AuthorityEval: AuthorityEvalProposedPost,
		Invariants: []Invariant{
			{ID: "last-owner-guard", Description: "Cannot remove the last active direct owner", Kind: InvariantSecurity, FailClosed: true},
		},
		AuditObligation: &AuditObligation{
			EventType:     "project.membership.remove",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"target_principal_id", "target_role"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialRoleAssignmentForbidden, DenialTargetRoleProtected, DenialLastOwner},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "project.membership.list",
		Domain:      "project.membership",
		Description: "List project members and their roles",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/members", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.read",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: role — role definition management
	// =====================================================================
	{
		ID:          "role.definition.create",
		Domain:      "role",
		Description: "Create a custom role definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "role.definition.create",
			ContextFields: []string{"actor_id"},
			AfterFields:   []string{"role_name", "permissions"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Role CRUD currently requires hub-admin via route guard; full operation contract deferred to AH1",
			Scope:  "AF1 catalog only",
			Waives: []WaivedObligation{WaiveAuditObligation},
		}},
	},
	{
		ID:          "role.definition.update",
		Domain:      "role",
		Description: "Update a custom role definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "role.definition.delete",
		Domain:      "role",
		Description: "Delete a custom role definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "role.definition.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"role_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: role.binding — role binding management
	// =====================================================================
	{
		ID:          "role.binding.create",
		Domain:      "role.binding",
		Description: "Create a role binding (grant authority to a principal)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/role-bindings", Method: "POST"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT},
		ResourceResolver:      "hub-scoped",
		BasePermission:        "role_binding.create",
		Effects:               []SecurityEffect{EffectGrantAuthority},
		DelegationKind:        DelegationNonAmplification,
		DelegationDescription: "Actor must hold all permissions in the bound role (CanDelegate)",
		AuthorityEval:         AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "role.binding.create",
			ContextFields: []string{"actor_id"},
			AfterFields:   []string{"principal_id", "role_name", "scope"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialRoleAssignmentForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "role.binding.delete",
		Domain:      "role.binding",
		Description: "Delete a role binding (revoke authority from a principal)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/role-bindings/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role_binding.delete",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "Revoking authority from a peer or superior principal requires governance review",
		},
		AuthorityEval: AuthorityEvalProposedPost,
		AuditObligation: &AuditObligation{
			EventType:     "role.binding.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"principal_id", "role_name", "scope"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialRoleAssignmentForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: group — group and membership management
	// =====================================================================
	{
		ID:          "group.member.add",
		Domain:      "group",
		Description: "Add a member to a group",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members", Method: "POST"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT},
		ResourceResolver:      "group-from-url",
		BasePermission:        "group.addMember",
		Effects:               []SecurityEffect{EffectGrantAuthority},
		DelegationKind:        DelegationNonAmplification,
		DelegationDescription: "Adding a member to a role-bearing group effectively grants authority; actor must hold the group's role permissions",
		AuthorityEval:         AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "group.member.add",
			ContextFields: []string{"actor_id", "group_id"},
			AfterFields:   []string{"member_principal_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "group.member.remove",
		Domain:      "group",
		Description: "Remove a member from a group",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members/{memberId}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "group-from-url",
		BasePermission:   "group.removeMember",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "Removing from a constraint-bearing group may change effective authority; governed by group role hierarchy",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "group.member.remove",
			ContextFields: []string{"actor_id", "group_id"},
			BeforeFields:  []string{"member_principal_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "group.delete",
		Domain:      "group",
		Description: "Delete a group",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "group-from-url",
		BasePermission:   "group.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "group.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"group_id", "group_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: access.constraint — access constraint management
	// =====================================================================
	{
		ID:          "access.constraint.create",
		Domain:      "access.constraint",
		Description: "Create an access constraint (tighten boundary)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "access_constraint.admin",
		Effects:          []SecurityEffect{EffectTightenBoundary},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceConstraintAdmin,
			Description: "Constraint creation requires constraint admin authority",
		},
		AuthorityEval: AuthorityEvalBeforeAndAfter,
		AuditObligation: &AuditObligation{
			EventType:     "access.constraint.create",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"effective_authority_before"},
			AfterFields:   []string{"constraint_id", "constraint_type", "target_scope"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "access.constraint.update",
		Domain:      "access.constraint",
		Description: "Update an access constraint (may relax or tighten boundary)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "access_constraint.admin",
		Effects:          []SecurityEffect{EffectRelaxBoundary, EffectTightenBoundary},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceConstraintAdmin,
			Description: "Constraint modification requires constraint admin authority; relaxation has higher governance bar",
		},
		AuthorityEval: AuthorityEvalBeforeAndAfter,
		AuditObligation: &AuditObligation{
			EventType:     "access.constraint.update",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"constraint_id", "old_scope"},
			AfterFields:   []string{"new_scope"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "access.constraint.delete",
		Domain:      "access.constraint",
		Description: "Delete an access constraint (relax boundary)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "access_constraint.admin",
		Effects:          []SecurityEffect{EffectRelaxBoundary},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceConstraintAdmin,
			Description: "Constraint deletion relaxes boundary and requires constraint admin authority",
		},
		AuthorityEval: AuthorityEvalBeforeAndAfter,
		AuditObligation: &AuditObligation{
			EventType:     "access.constraint.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"constraint_id", "constraint_type", "target_scope"},
			AfterFields:   []string{"effective_authority_after"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: credential — token and credential management
	// =====================================================================
	{
		ID:          "credential.token.create",
		Domain:      "credential",
		Description: "Create a user access token (UAT)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/tokens", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "self-principal",
		BasePermission:   "user.read",
		Effects:          []SecurityEffect{EffectMintCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "User mints tokens for self; token scopes cannot exceed session authority",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "credential.token.create",
			ContextFields: []string{"actor_id"},
			AfterFields:   []string{"token_id", "scopes"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialScopeViolation},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Token creation is authenticated-only (user manages own tokens); no per-resource permission required beyond session validity",
			Scope:  "self-token management only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
	},
	{
		ID:          "credential.token.revoke",
		Domain:      "credential",
		Description: "Revoke a user access token",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/tokens/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "self-principal",
		BasePermission:   "user.read",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "User may revoke own tokens; admin may revoke via hub-admin path",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "credential.token.revoke",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"token_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Token revocation is authenticated-only (user manages own tokens)",
			Scope:  "self-token management only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
	},

	// =====================================================================
	// Domain: gcp.identity — GCP service account management
	// =====================================================================
	{
		ID:          "gcp.identity.create",
		Domain:      "gcp.identity",
		Description: "Create a GCP service account binding",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-body",
		BasePermission:   "gcp_service_account.create",
		Effects:          []SecurityEffect{EffectAssignCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Service account creation assigns a credential to project scope",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "gcp.identity.create",
			ContextFields: []string{"actor_id", "project_id"},
			AfterFields:   []string{"service_account_email"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "gcp.identity.delete",
		Domain:      "gcp.identity",
		Description: "Delete a GCP service account binding",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "gcp-service-account-from-url",
		BasePermission:   "gcp_service_account.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "gcp.identity.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"service_account_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "gcp.identity.assign",
		Domain:      "gcp.identity",
		Description: "Assign a GCP service account to an agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts/{id}/assign", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "gcp-service-account-from-url",
		BasePermission:   "gcp_service_account.assign",
		Effects:          []SecurityEffect{EffectAssignCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Assigning a service account to an agent grants the agent access to the service account's identity",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "gcp.identity.assign",
			ContextFields: []string{"actor_id"},
			AfterFields:   []string{"service_account_id", "agent_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "gcp.identity.mint",
		Domain:      "gcp.identity",
		Description: "Mint a GCP access token for a service account",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agent/gcp-token", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalAgent},
		Credentials:      []CredentialKind{CredentialAgentJWT},
		ResourceResolver: "agent-gcp-service-account",
		BasePermission:   "gcp_service_account.mint",
		Effects:          []SecurityEffect{EffectMintCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Agent mints GCP tokens scoped to its assigned service account",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:              "gcp.identity.mint",
			ContextFields:          []string{"agent_id"},
			AfterFields:            []string{"service_account_email", "token_scopes"},
			Atomic:                 false,
			NonAtomicJustification: "Token minting calls external GCP API; audit recorded before external call",
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: agent — agent lifecycle
	// =====================================================================
	{
		ID:          "agent.lifecycle.create",
		Domain:      "agent",
		Description: "Create an agent in a project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "project-from-body",
		BasePermission:   "agent.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "agent.lifecycle.delete",
		Domain:      "agent",
		Description: "Delete an agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "agent.lifecycle.delete",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"agent_id", "agent_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: project — project lifecycle
	// =====================================================================
	{
		ID:          "project.lifecycle.create",
		Domain:      "project",
		Description: "Create a new project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "project.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "project.lifecycle.delete",
		Domain:      "project",
		Description: "Delete a project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "project.lifecycle.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"project_id", "project_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: agent.message — agent messaging (external effect)
	// =====================================================================
	{
		ID:          "agent.message.send",
		Domain:      "agent.message",
		Description: "Send a message to an agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/threads/{id}/messages", Method: "POST"},
			{Kind: EntryPointBrokerCall, Pattern: "broker.inbound"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent, PrincipalBroker},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT, CredentialBrokerToken},
		ResourceResolver: "agent-from-thread",
		BasePermission:   "agent.message",
		Effects:          []SecurityEffect{EffectEmitExternal},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		ExternalPolicy: &ExternalEffectPolicy{
			DeliveryMode:   DeliveryFireAndForget,
			FailureMode:    FailureLogAndContinue,
			IdempotencyKey: "message ID",
			RetryPolicy:    "no retry for user-sent messages",
			AuthBeforeEmit: true,
		},
		AuditObligation: &AuditObligation{
			EventType:              "agent.message.send",
			ContextFields:          []string{"actor_id", "project_id"},
			AfterFields:            []string{"message_id", "target_agent_id"},
			Atomic:                 false,
			NonAtomicJustification: "Message dispatch is fire-and-forget; audit recorded before dispatch",
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: user.admin — user administration
	// =====================================================================
	{
		ID:          "user.admin.suspend",
		Domain:      "user.admin",
		Description: "Suspend a user account",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}/suspend", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "user-from-url",
		BasePermission:   "user.suspend",
		Effects:          []SecurityEffect{EffectChangePrincipalStatus},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "user.admin.suspend",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"target_user_id", "old_status"},
			AfterFields:   []string{"new_status"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
}

// EntryPointExemptions documents routes and entry points that do not map to
// authorization operations. Each exemption is typed, scoped, owned, and
// rationalized. The CI gate validates that every exemption references a real
// route and that no route is both cataloged and exempted.
var EntryPointExemptions = []EntryPointExemption{
	// Public endpoints — no authorization required
	{Pattern: "/healthz", Kind: ExemptionPublicEndpoint, Reason: "Liveness probe, no authorization", Owner: "route_metadata.go"},
	{Pattern: "/readyz", Kind: ExemptionPublicEndpoint, Reason: "Readiness probe, no authorization", Owner: "route_metadata.go"},
	{Pattern: "/metrics", Kind: ExemptionPublicEndpoint, Reason: "Prometheus metrics, no authorization", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/login", Kind: ExemptionPublicEndpoint, Reason: "Auth login flow, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/token", Kind: ExemptionPublicEndpoint, Reason: "Auth token exchange, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/refresh", Kind: ExemptionPublicEndpoint, Reason: "Auth token refresh, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/validate", Kind: ExemptionPublicEndpoint, Reason: "Token validation, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/providers", Kind: ExemptionPublicEndpoint, Reason: "Auth provider list, public configuration", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/invite/redeem", Kind: ExemptionPublicEndpoint, Reason: "Invite redemption, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/cli/authorize", Kind: ExemptionPublicEndpoint, Reason: "CLI auth flow, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/cli/token", Kind: ExemptionPublicEndpoint, Reason: "CLI token exchange, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/cli/device", Kind: ExemptionPublicEndpoint, Reason: "CLI device auth flow, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/cli/device/token", Kind: ExemptionPublicEndpoint, Reason: "CLI device token exchange, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/settings/public", Kind: ExemptionPublicEndpoint, Reason: "Public settings, no secrets", Owner: "route_metadata.go"},
	{Pattern: "/github-app/setup", Kind: ExemptionPublicEndpoint, Reason: "GitHub App setup callback, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "GET /.well-known/openid-configuration", Kind: ExemptionPublicEndpoint, Reason: "OIDC discovery, public standard", Owner: "route_metadata.go"},
	{Pattern: "GET /.well-known/jwks.json", Kind: ExemptionPublicEndpoint, Reason: "OIDC JWKS, public standard", Owner: "route_metadata.go"},

	// Authentication-only endpoints — identity required but no resource-level authorization
	{Pattern: "/api/v1/auth/logout", Kind: ExemptionAuthenticationOnly, Reason: "Session termination, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/me", Kind: ExemptionAuthenticationOnly, Reason: "Read own identity, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/admin-status", Kind: ExemptionAuthenticationOnly, Reason: "Check own admin status, self-service", Owner: "route_metadata.go"},
	// /api/v1/auth/tokens: GET is self-service (exempted); POST is cataloged as credential.token.create.
	// /api/v1/auth/tokens/: GET is self-service (exempted); DELETE is cataloged as credential.token.revoke.
	// The catalog uses method-specific entry points; the route metadata uses the base pattern for both.
	{Pattern: "/api/v1/auth/scopes", Kind: ExemptionAuthenticationOnly, Reason: "List available scopes, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/metrics/session/", Kind: ExemptionAuthenticationOnly, Reason: "Session metrics, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/groups", Kind: ExemptionAuthenticationOnly, Reason: "List own group memberships, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/principals/", Kind: ExemptionAuthenticationOnly, Reason: "Resolve principal display name, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/injected-skills", Kind: ExemptionAuthenticationOnly, Reason: "Manage own injected skills, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/injected-skills/", Kind: ExemptionAuthenticationOnly, Reason: "Manage own injected skill by ID, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/templates", Kind: ExemptionAuthenticationOnly, Reason: "Manage own templates, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/templates/", Kind: ExemptionAuthenticationOnly, Reason: "Manage own template by ID, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/notifications", Kind: ExemptionAuthenticationOnly, Reason: "List own notifications, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/notifications/", Kind: ExemptionAuthenticationOnly, Reason: "Manage own notification by ID, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/messages", Kind: ExemptionAuthenticationOnly, Reason: "List own messages, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/messages/", Kind: ExemptionAuthenticationOnly, Reason: "Manage own message by ID, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/message-channels", Kind: ExemptionAuthenticationOnly, Reason: "List own message channels, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/chat/user-prefs", Kind: ExemptionAuthenticationOnly, Reason: "Chat preferences, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/chat/presence", Kind: ExemptionAuthenticationOnly, Reason: "Chat presence, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/telegram/link", Kind: ExemptionAuthenticationOnly, Reason: "Account linking, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/telegram/link/verify", Kind: ExemptionAuthenticationOnly, Reason: "Account linking verification, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/telegram/link/status", Kind: ExemptionAuthenticationOnly, Reason: "Account linking status, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/discord/link", Kind: ExemptionAuthenticationOnly, Reason: "Account linking, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/discord/link/verify", Kind: ExemptionAuthenticationOnly, Reason: "Account linking verification, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/discord/link/status", Kind: ExemptionAuthenticationOnly, Reason: "Account linking status, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/teams/link", Kind: ExemptionAuthenticationOnly, Reason: "Account linking, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/teams/link/verify", Kind: ExemptionAuthenticationOnly, Reason: "Account linking verification, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/teams/link/status", Kind: ExemptionAuthenticationOnly, Reason: "Account linking status, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/policies", Kind: ExemptionAuthenticationOnly, Reason: "Deprecated (410 Gone), authenticated for error reporting", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/policies/", Kind: ExemptionAuthenticationOnly, Reason: "Deprecated (410 Gone), authenticated for error reporting", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/authz/explain", Kind: ExemptionAuthenticationOnly, Reason: "Authorization explain for self, self-service diagnostic", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/hub/settings/injected-skills", Kind: ExemptionAuthenticationOnly, Reason: "Hub injected skills; GET is public, PUT requires admin (enforced in handler)", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/pre-start-hooks", Kind: ExemptionAuthenticationOnly, Reason: "Pre-start hooks; GET is open, POST/PUT/DELETE requires admin (enforced in handler)", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/pre-start-hooks/", Kind: ExemptionAuthenticationOnly, Reason: "Pre-start hooks by ID; admin enforcement in handler", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/usage/me", Kind: ExemptionAuthenticationOnly, Reason: "Own usage statistics, self-service", Owner: "route_metadata.go"},

	// Workstation endpoints — workstation token authentication
	{Pattern: "/api/v1/system/identity", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/status", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/check", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/runtime", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/init", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/images/pull", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/images/build", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/apple-dns", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/registry", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/workstation-settings", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/fs/list", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/fs/mkdir", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/fs/validate-path", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},

	// Broker HMAC endpoints — broker authentication
	{Pattern: "/api/v1/brokers", Kind: ExemptionInternalOnly, Reason: "Broker registration, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/brokers/join", Kind: ExemptionInternalOnly, Reason: "Broker join, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/brokers/", Kind: ExemptionInternalOnly, Reason: "Broker by ID, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/broker/inbound", Kind: ExemptionInternalOnly, Reason: "Broker message inbound, broker-HMAC auth; per-message authz via authorizeAgentMessage", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/broker/projects", Kind: ExemptionInternalOnly, Reason: "Broker project list, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/runtime-brokers/connect", Kind: ExemptionInternalOnly, Reason: "Runtime broker WebSocket connect, broker-HMAC auth", Owner: "route_metadata.go"},

	// Agent token endpoints — agent JWT authentication
	// /api/v1/agent/gcp-token: cataloged as gcp.identity.mint (agent-JWT entry point).
	{Pattern: "/api/v1/agent/gcp-identity-token", Kind: ExemptionInternalOnly, Reason: "Agent GCP identity token, agent-JWT auth", Owner: "route_metadata.go"},
	{Pattern: "POST /api/v1/agent/identity-token", Kind: ExemptionInternalOnly, Reason: "Agent OIDC identity token, agent-JWT auth", Owner: "route_metadata.go"},

	// Webhook endpoints — signature verification
	{Pattern: "/api/v1/webhooks/github", Kind: ExemptionInternalOnly, Reason: "GitHub webhook, signature-verified", Owner: "route_metadata.go"},

	// GitHub App admin endpoints — hub-admin without declared permission
	{Pattern: "/api/v1/github-app", Kind: ExemptionAuthenticationOnly, Reason: "GitHub App config, hub-admin via requireAdmin fallback", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/github-app/installations", Kind: ExemptionAuthenticationOnly, Reason: "GitHub App installations, hub-admin via requireAdmin fallback", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/github-app/installations/", Kind: ExemptionAuthenticationOnly, Reason: "GitHub App installation by ID, hub-admin via requireAdmin fallback", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/github-app/installations/discover", Kind: ExemptionAuthenticationOnly, Reason: "GitHub App installation discover, hub-admin via requireAdmin fallback", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/github-app/sync-permissions", Kind: ExemptionAuthenticationOnly, Reason: "GitHub App permission sync, hub-admin via requireAdmin fallback", Owner: "route_metadata.go"},
}

// CatalogOperationIDs returns the set of all operation IDs in the catalog.
func CatalogOperationIDs() map[OperationID]bool {
	ids := make(map[OperationID]bool, len(Catalog))
	for _, spec := range Catalog {
		ids[spec.ID] = true
	}
	return ids
}

// CatalogBasePermissions returns the set of all base permissions referenced
// by catalog operations.
func CatalogBasePermissions() map[string][]OperationID {
	perms := make(map[string][]OperationID)
	for _, spec := range Catalog {
		if spec.BasePermission != "" {
			perms[spec.BasePermission] = append(perms[spec.BasePermission], spec.ID)
		}
	}
	return perms
}
