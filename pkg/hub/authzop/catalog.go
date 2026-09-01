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
// represent security-relevant mutations and external effects. The
// authorization audit scanner discovers call sites matching these symbols
// and requires each to be classified.
//
// Scanner boundary: the scanner walks all .go files (excluding _test.go)
// under pkg/hub/ and pkg/store/ recursively. Methods in extras/, pkg/ent/,
// and pkg/k8s/ are outside the trust boundary.
var SecurityMutationSymbols = map[string]string{
	// Authority mutations — role bindings
	"CreateRoleBinding":            "grant-authority",
	"DeleteRoleBinding":            "revoke-authority",
	"DeleteRoleBindingsForPrincipal": "revoke-authority",
	"DeleteRoleBindingsForScope":   "revoke-authority",

	// Role definition mutations
	"CreateRoleDefinition":                  "create-resource",
	"UpdateRoleDefinition":                  "update-resource",
	"DeleteRoleDefinition":                  "delete-resource",
	"UpdateSystemRoleDefinitionPermissions": "change-authority",

	// Group mutations
	"CreateGroup":           "create-resource",
	"UpdateGroup":           "update-resource",
	"DeleteGroup":           "delete-resource",
	"AddGroupMember":        "grant-authority",
	"RemoveGroupMember":     "revoke-authority",
	"UpdateGroupMemberRole": "change-authority",

	// Access constraint mutations
	"CreateAccessConstraint": "tighten-boundary",
	"UpdateAccessConstraint": "change-authority",
	"DeleteAccessConstraint": "relax-boundary",

	// User lifecycle mutations
	"CreateUser": "create-resource",
	"UpdateUser": "change-principal-status",
	"DeleteUser": "delete-resource",

	// Credential/token mutations — user access tokens
	"CreateUserAccessToken": "mint-credential",
	"RevokeUserAccessToken": "revoke-authority",
	"DeleteUserAccessToken": "revoke-authority",

	// Credential mutations — agent credentials
	"CreateAgentCredential":        "mint-credential",
	"RevokeAgentCredential":        "revoke-authority",
	"RevokeAgentCredentialsByAgent": "revoke-authority",

	// Secret operations
	"CreateSecret":       "create-resource",
	"UpdateSecret":       "update-resource",
	"UpsertSecret":       "create-resource",
	"DeleteSecret":       "delete-resource",
	"DeleteSecretsByScope": "delete-resource",
	"GetSecretValue":     "read-secret",

	// Broker secret operations
	"CreateBrokerSecret": "create-resource",
	"UpdateBrokerSecret": "update-resource",
	"DeleteBrokerSecret": "delete-resource",
	"CreateJoinToken":    "mint-credential",

	// Invite code operations
	"CreateInviteCode": "mint-credential",
	"RevokeInviteCode": "revoke-authority",
	"DeleteInviteCode": "delete-resource",

	// GCP service account mutations (store-level)
	"CreateGCPServiceAccount": "assign-credential",
	"DeleteGCPServiceAccount": "delete-resource",
	"UpdateGCPServiceAccount": "update-resource",

	// GCP IAM external effects
	"GenerateAccessToken":  "mint-credential",
	"CreateServiceAccount": "emit-external",
	"DeleteServiceAccount": "emit-external",
	"SetIAMPolicy":         "emit-external",

	// Project lifecycle
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

	// =====================================================================
	// Domain: secret — project secret management (HIGH-RISK)
	// =====================================================================
	{
		ID:          "secret.read",
		Domain:      "secret",
		Description: "Read project secrets or environment variables containing secrets",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.read",
		Effects:          []SecurityEffect{EffectReadSecret},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "secret.read",
			ContextFields: []string{"actor_id", "project_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "secret.write",
		Domain:      "secret",
		Description: "Create or update project secrets",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.update",
		Effects:          []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "secret.write",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"secret_key"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: user.admin — user administration (continued, HIGH-RISK)
	// =====================================================================
	{
		ID:          "user.admin.invite",
		Domain:      "user.admin",
		Description: "Invite a user to the platform",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/users/invite", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/users/invite/bulk", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "user.invite",
		Effects:          []SecurityEffect{EffectIssueCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Invitation issues a credential granting platform access",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "user.admin.invite",
			ContextFields: []string{"actor_id"},
			AfterFields:   []string{"invite_email", "invite_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:                    "user.admin.promote",
		Domain:                "user.admin",
		Description:           "Promote or demote a user's administrative level",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}/promote", Method: "POST"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT},
		ResourceResolver:      "user-from-url",
		BasePermission:        "user.promote",
		Effects:               []SecurityEffect{EffectChangeAuthority},
		DelegationKind:        DelegationConditionalIncrease,
		DelegationDescription: "Promotion delegation checked only when effective authority increases",
		AuthorityEval:         AuthorityEvalBeforeAndAfter,
		AuditObligation: &AuditObligation{
			EventType:     "user.admin.promote",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"target_user_id", "old_level"},
			AfterFields:   []string{"new_level"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: hub — hub administration (HIGH-RISK)
	// =====================================================================
	{
		ID:          "hub.authreset",
		Domain:      "hub",
		Description: "Reset all agent authentication credentials (emergency action)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/agents/reset-auth-all", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.auth_reset.execute",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "Mass auth reset is a drastic authority revocation requiring hub admin governance",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "hub.authreset",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"agent_count"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: hub — hub admin reads and configuration
	// =====================================================================
	{
		ID:               "hub.config.read",
		Domain:           "hub",
		Description:      "Read server configuration and schema",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/server-config", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/server-config/schema", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.config.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.config.update",
		Domain:           "hub",
		Description:      "Update server configuration sections",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/server-config/sections/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.config.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.maintenance.execute",
		Domain:           "hub",
		Description:      "Execute maintenance operations including migrations and restarts",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/operations", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/operations", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/operations/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/restart", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/check-updates", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/migrations/{id}", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.maintenance.execute",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.adminmode.update",
		Domain:           "hub",
		Description:      "Toggle admin/maintenance mode",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.admin_mode.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.allowlist.update",
		Domain:           "hub",
		Description:      "Manage the platform email allow list",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/allow-list", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/allow-list", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/allow-list/{email}", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/allow-list/{email}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.allow_list.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.health.read",
		Domain:           "hub",
		Description:      "Read platform health summary and GCP quota status",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/health/summary", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/gcp-quota", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.health.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.diagnostics.read",
		Domain:           "hub",
		Description:      "Read diagnostic logs and messaging divergence data",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/diagnostics/logs", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/diagnostics/logs/stream", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/messaging/divergence", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.diagnostics.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.scheduler.read",
		Domain:           "hub",
		Description:      "Read scheduler status and configuration",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/scheduler", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.scheduler.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.projectdefaults.read",
		Domain:           "hub",
		Description:      "Read project default settings",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/project-defaults", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.project_defaults.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.lifecyclehooks.read",
		Domain:           "hub",
		Description:      "Read lifecycle hook definitions",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/lifecycle-hooks", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/lifecycle-hooks/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.lifecycle_hooks.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.validate.execute",
		Domain:           "hub",
		Description:      "Validate resource definitions against schema",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/validate-resources", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.validate.execute",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.integrations.read",
		Domain:           "hub",
		Description:      "Read integration configurations",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/{name}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.integrations.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.teamsmanifest.read",
		Domain:           "hub",
		Description:      "Read Teams integration manifest",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/teams/manifest", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.teams_manifest.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "hub.metrics.read",
		Domain:           "hub",
		Description:      "Read metrics dashboard data",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/metrics/{name}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/metrics-dashboard", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.metrics.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: agent — agent read, update, and special operations
	// =====================================================================
	{
		ID:               "agent.read",
		Domain:           "agent",
		Description:      "Read agent metadata or list agents in a project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "agent.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "agent.update",
		Domain:           "agent",
		Description:      "Update agent configuration or metadata",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "agent.attach",
		Domain:           "agent",
		Description:      "Attach to an agent session via WebSocket",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointWebSocket, Pattern: "/api/v1/agents/{id}/attach", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.attach",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "agent.portaccess",
		Domain:           "agent",
		Description:      "Access forwarded ports on an agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.port_access",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "agent.stopall",
		Domain:           "agent",
		Description:      "Stop all running agents in a project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/stop-all", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "agent.stop_all",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "agent.setmessagemode",
		Domain:           "agent",
		Description:      "Change an agent's message mode",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/message-mode", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.set_message_mode",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: project — project read, update, and register
	// =====================================================================
	{
		ID:               "project.read",
		Domain:           "project",
		Description:      "Read project metadata or list projects",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groves", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groves/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "project.update",
		Domain:           "project",
		Description:      "Update project settings and metadata",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groves/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "project.register",
		Domain:           "project",
		Description:      "Register a project or grove from an external source",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/register", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groves/register", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-body",
		BasePermission:   "project.register",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: skill — skill CRUD
	// =====================================================================
	{
		ID:               "skill.read",
		Domain:           "skill",
		Description:      "Read skill definitions or list/discover skills",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/discover-directory", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "skill.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "skill.create",
		Domain:           "skill",
		Description:      "Create a new skill definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-body",
		BasePermission:   "skill.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "skill.update",
		Domain:           "skill",
		Description:      "Update an existing skill definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "skill-from-url",
		BasePermission:   "skill.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "skill.delete",
		Domain:           "skill",
		Description:      "Delete a skill definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "skill-from-url",
		BasePermission:   "skill.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "skill.delete",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"skill_id", "skill_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "skill.register",
		Domain:           "skill",
		Description:      "Register skills in a skill registry",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "skill.register",
		Effects:          []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "skill.register",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"registry_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: template — template CRUD
	// =====================================================================
	{
		ID:               "template.read",
		Domain:           "template",
		Description:      "Read template definitions or discover available templates",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/resources/discover", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "template.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "template.create",
		Domain:           "template",
		Description:      "Create a new template or import resources",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/resources/import", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-body",
		BasePermission:   "template.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "template.update",
		Domain:           "template",
		Description:      "Update an existing template definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "template-from-url",
		BasePermission:   "template.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "template.delete",
		Domain:           "template",
		Description:      "Delete a template definition",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "template-from-url",
		BasePermission:   "template.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "template.delete",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"template_id", "template_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: harnessconfig — harness configuration CRUD
	// =====================================================================
	{
		ID:               "harnessconfig.read",
		Domain:           "harnessconfig",
		Description:      "Read harness configurations or list available configs",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "harness_config.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "harnessconfig.create",
		Domain:           "harnessconfig",
		Description:      "Create a new harness configuration",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-body",
		BasePermission:   "harness_config.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "harnessconfig.update",
		Domain:           "harnessconfig",
		Description:      "Update a harness configuration",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "harnessconfig-from-url",
		BasePermission:   "harness_config.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "harnessconfig.delete",
		Domain:           "harnessconfig",
		Description:      "Delete a harness configuration",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "harnessconfig-from-url",
		BasePermission:   "harness_config.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "harnessconfig.delete",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"config_id", "config_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: group — group read, create, update
	// =====================================================================
	{
		ID:               "group.read",
		Domain:           "group",
		Description:      "Read group details or list groups",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "group-from-url",
		BasePermission:   "group.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "group.create",
		Domain:           "group",
		Description:      "Create a new group",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "group.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "group.update",
		Domain:           "group",
		Description:      "Update group metadata",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "group-from-url",
		BasePermission:   "group.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: user — user read and update
	// =====================================================================
	{
		ID:               "user.read",
		Domain:           "user",
		Description:      "Read user profile or list users",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "user-from-url",
		BasePermission:   "user.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "user.update",
		Domain:           "user",
		Description:      "Update user profile or settings",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "user-from-url",
		BasePermission:   "user.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: broker — runtime broker read
	// =====================================================================
	{
		ID:               "broker.read",
		Domain:           "broker",
		Description:      "Read runtime broker status or list brokers",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "broker.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: gcp.identity — GCP identity read and verify
	// =====================================================================
	{
		ID:               "gcp.identity.read",
		Domain:           "gcp.identity",
		Description:      "Read GCP service account details or list accounts",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "gcp_service_account.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "gcp.identity.verify",
		Domain:      "gcp.identity",
		Description: "Verify a GCP service account's IAM configuration",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts/{id}/verify", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "gcp-identity-from-url",
		BasePermission:   "gcp_service_account.verify",
		Effects:          []SecurityEffect{EffectAssignCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Verification may re-bind IAM credentials",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "gcp.identity.verify",
			ContextFields: []string{"actor_id", "project_id"},
			AfterFields:   []string{"service_account_id", "verification_status"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: role — role definition and binding reads
	// =====================================================================
	{
		ID:               "role.read",
		Domain:           "role",
		Description:      "Read role definitions and permission registry",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/permissions", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "role.binding.read",
		Domain:           "role.binding",
		Description:      "Read role binding assignments",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/role-bindings", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/role-bindings/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role_binding.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "access.constraint.read",
		Domain:           "access.constraint",
		Description:      "Read access constraint definitions",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "access_constraint.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: quota — quota/limits management
	// =====================================================================
	{
		ID:               "quota.read",
		Domain:           "quota",
		Description:      "Read limit definitions, entitlements, and usage",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/limits", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/limits/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/entitlements/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/usage", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/usage/{limit}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "quota.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "quota.create",
		Domain:           "quota",
		Description:      "Create limit definitions and entitlement bindings",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/limits", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/entitlements/{id}", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "quota.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "quota.update",
		Domain:           "quota",
		Description:      "Update limit definitions and entitlement bindings",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/limits/{id}", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/entitlements/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "quota.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "quota.delete",
		Domain:           "quota",
		Description:      "Delete limit definitions and entitlement bindings",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/limits/{id}", Method: "DELETE"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/entitlements/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "quota.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "quota.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"limit_id", "limit_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: schedule — scheduled event management
	// =====================================================================
	{
		ID:               "schedule.event.read",
		Domain:           "schedule",
		Description:      "Read scheduled events or list events in a project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/scheduled-events", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/scheduled-events/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/schedules", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/schedules/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "scheduled_event.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "schedule.event.create",
		Domain:           "schedule",
		Description:      "Create a scheduled event or recurring schedule",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/scheduled-events", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/schedules", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "scheduled_event.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "schedule.event.update",
		Domain:           "schedule",
		Description:      "Update a recurring schedule",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/schedules/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "scheduled_event.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:               "schedule.event.delete",
		Domain:           "schedule",
		Description:      "Cancel a scheduled event or delete a recurring schedule",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/scheduled-events/{id}", Method: "DELETE"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/schedules/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "scheduled_event.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "schedule.event.delete",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"event_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: chat — chat access (project-scoped)
	// =====================================================================
	{
		ID:               "chat.access",
		Domain:           "chat",
		Description:      "Access chat threads, spaces, topics, and messages within a project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/prefs", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/prefs", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/threads", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/threads/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/spaces", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/spaces/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/topics/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/dms", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/search", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/attachments", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/attachments/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},

	// =====================================================================
	// Domain: env — project environment variable access
	// =====================================================================
	{
		ID:               "env.read",
		Domain:           "env",
		Description:      "Read project environment variables",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
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
	{Pattern: "/api/v1/hub/settings/injected-skills", Kind: ExemptionHubAdmin, Reason: "Hub injected skills; GET is open, PUT requires hub-admin (enforced in handler via requireAdmin). Admin mutation — operation contract deferred to AH1.", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/pre-start-hooks", Kind: ExemptionHubAdmin, Reason: "Pre-start hooks; GET is open, POST/PUT/DELETE require hub-admin (enforced in handler via requireAdmin). Admin mutation — operation contract deferred to AH1.", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/pre-start-hooks/", Kind: ExemptionHubAdmin, Reason: "Pre-start hooks by ID; admin enforcement in handler via requireAdmin. Admin mutation — operation contract deferred to AH1.", Owner: "route_metadata.go"},
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
	{Pattern: "/api/v1/github-app", Kind: ExemptionHubAdmin, Reason: "GitHub App config, hub-admin via requireAdmin fallback; no declared permission yet", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/github-app/installations", Kind: ExemptionHubAdmin, Reason: "GitHub App installations, hub-admin via requireAdmin fallback; no declared permission yet", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/github-app/installations/", Kind: ExemptionHubAdmin, Reason: "GitHub App installation by ID, hub-admin via requireAdmin fallback; no declared permission yet", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/github-app/installations/discover", Kind: ExemptionHubAdmin, Reason: "GitHub App installation discover, hub-admin via requireAdmin fallback; no declared permission yet", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/github-app/sync-permissions", Kind: ExemptionHubAdmin, Reason: "GitHub App permission sync, hub-admin via requireAdmin fallback; no declared permission yet", Owner: "route_metadata.go"},
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
