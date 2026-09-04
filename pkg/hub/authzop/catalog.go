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
	"CreateRoleBinding":              "grant-authority",
	"DeleteRoleBinding":              "revoke-authority",
	"DeleteRoleBindingsForPrincipal": "revoke-authority",
	"DeleteRoleBindingsForScope":     "revoke-authority",

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
	"CreateAgentCredential":         "mint-credential",
	"RevokeAgentCredential":         "revoke-authority",
	"RevokeAgentCredentialsByAgent": "revoke-authority",

	// Secret operations
	"CreateSecret":         "create-resource",
	"UpdateSecret":         "update-resource",
	"UpsertSecret":         "create-resource",
	"DeleteSecret":         "delete-resource",
	"DeleteSecretsByScope": "delete-resource",
	"GetSecretValue":       "read-secret",

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
		Credentials:           []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver:      "project-from-url",
		BasePermission:        "project.manage",
		Effects:               []SecurityEffect{EffectGrantAuthority},
		DelegationKind:        DelegationNonAmplification,
		DelegationDescription: "Actor must hold all permissions in the target role (CanDelegate non-amplification)",
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. Enforced by ProjectMembershipService.checkGovernance.",
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
		Credentials:           []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver:      "project-from-url",
		BasePermission:        "project.manage",
		Effects:               []SecurityEffect{EffectChangeAuthority},
		DelegationKind:        DelegationConditionalIncrease,
		DelegationDescription: "CanDelegate checked when new role has more permissions than old role",
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. Both old and new target roles are governed. Enforced by ProjectMembershipService.checkGovernance.",
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
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.manage",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. CT1 D1 allows self-removal when another active direct owner remains. Enforced by ProjectMembershipService.checkGovernance.",
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
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.read",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	{
		ID:          "project.membership.transfer",
		Domain:      "project.membership",
		Description: "Atomically transfer project ownership from the actor to another user",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/transfer-ownership", Method: "POST"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver:      "project-from-url",
		BasePermission:        "project.manage",
		Effects:               []SecurityEffect{EffectChangeAuthority},
		DelegationKind:        DelegationConditionalIncrease,
		DelegationDescription: "Actor must be a direct project owner; target is promoted to owner, actor is downgraded to member — conditional-on-increase applies to the target's authority change",
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "RS1 governance: only active direct project owners may transfer ownership. Actor-must-be-direct-owner is enforced by the ProjectMembershipService.",
		},
		AuthorityEval: AuthorityEvalBeforeAndAfter,
		Invariants: []Invariant{
			{ID: "direct-user-only-owner", Description: "project-owner role is direct-user-only", Kind: InvariantSecurity, FailClosed: true},
			{ID: "last-owner-guard", Description: "Post-state: at least one active direct owner must remain", Kind: InvariantSecurity, FailClosed: true},
			{ID: "single-binding-per-principal", Description: "CT1 D4: one direct binding per principal per project; atomic replacement for both actor and target", Kind: InvariantBusiness, FailClosed: false},
		},
		AuditObligation: &AuditObligation{
			EventType:     "project.membership.transfer",
			ContextFields: []string{"actor_id", "project_id"},
			BeforeFields:  []string{"old_owner_id"},
			AfterFields:   []string{"new_owner_id", "old_owner_role", "new_owner_role"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialRoleAssignmentForbidden, DenialPrincipalIneligible, DenialLastOwner},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
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
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/import", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}/duplicate", Method: "POST"},
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestRS4_IssuerAuthority"},
			{Package: "pkg/hub", Function: "TestRS4_TargetScope"},
			{Package: "pkg/hub", Function: "TestRS4_Audit_Mint"},
		},
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
		Description: "Revoke or delete a user access token",
		EntryPoints: []EntryPoint{
			// RS4/G6: Both soft-revoke and hard-delete share one operation ID (A5).
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/tokens/{id}/revoke", Method: "POST"},
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
			BeforeFields:  []string{"token_id", "action"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestRS4_Audit_Revoke"},
			{Package: "pkg/hub", Function: "TestRS4_Audit_Delete"},
		},
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
		Description: "Delete a project with cascading security state cleanup and atomic audit",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.delete",
		Effects:          []SecurityEffect{EffectDeleteResource, EffectEmitExternal},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceOwnershipAncestry,
			Description: "RS3 governance: direct project owner or super-admin. Hub-admin lacks project.delete and is denied at base permission. Group-derived ownership does not confer deletion authority. Stale Project.OwnerID is not consulted. Enforced by ProjectDeletionService.checkDeletionGovernance.",
		},
		AuthorityEval: AuthorityEvalNone,
		Invariants: []Invariant{
			{ID: "target-exists", Description: "Project must exist and not be already deleted", Kind: InvariantBusiness, FailClosed: true},
		},
		ExternalPolicy: &ExternalEffectPolicy{
			DeliveryMode:   DeliveryFireAndForget,
			FailureMode:    FailureLogAndContinue,
			IdempotencyKey: "project ID (single deletion per project)",
			RetryPolicy:    "no retry — cascading deletes are best-effort; DB cascade is authoritative",
			AuthBeforeEmit: true,
		},
		AuditObligation: &AuditObligation{
			EventType:     "project.lifecycle.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"project_id", "project_name", "project_slug", "owner_id"},
			AfterFields:   []string{"cascade_summary"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialUserSuspended, DenialCredentialInsufficient, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestRS3_ProjectDeleteOwnerPositiveControl"},
			{Package: "pkg/hub", Function: "TestRS3_ProjectDeleteGovernanceMatrix"},
			{Package: "pkg/hub", Function: "TestRS3_ProjectDeleteAtomicAudit"},
		},
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
		Description: "Suspend or reactivate a user account (dispatched from PATCH /api/v1/users/{id} when status field is present)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointInternalDispatch, Pattern: "updateUser:status-field"},
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
		ID:          "user.admin.promote",
		Domain:      "user.admin",
		Description: "Promote or demote a user's administrative level (dispatched from PATCH /api/v1/users/{id} when role field is present)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointInternalDispatch, Pattern: "updateUser:role-field"},
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

	{
		ID:          "user.admin.delete",
		Domain:      "user.admin",
		Description: "Delete a user account",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "user-from-url",
		BasePermission:   "user.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "user.admin.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"target_user_id", "email", "role", "status"},
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
		ID:          "hub.config.read",
		Domain:      "hub",
		Description: "Read server configuration and schema",
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
		ID:          "hub.config.update",
		Domain:      "hub",
		Description: "Update server configuration sections",
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
		ID:          "hub.maintenance.execute",
		Domain:      "hub",
		Description: "Execute maintenance operations including migrations and restarts",
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
		ID:          "hub.adminmode.update",
		Domain:      "hub",
		Description: "Toggle admin/maintenance mode",
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
		ID:          "hub.allowlist.update",
		Domain:      "hub",
		Description: "Manage the platform email allow list",
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
		ID:          "hub.health.read",
		Domain:      "hub",
		Description: "Read platform health summary and GCP quota status",
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
		ID:          "hub.diagnostics.read",
		Domain:      "hub",
		Description: "Read diagnostic logs and messaging divergence data",
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
		ID:          "hub.scheduler.read",
		Domain:      "hub",
		Description: "Read scheduler status and configuration",
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
		ID:          "hub.projectdefaults.read",
		Domain:      "hub",
		Description: "Read project default settings",
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
		ID:          "hub.lifecyclehooks.read",
		Domain:      "hub",
		Description: "Read lifecycle hook definitions",
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
		ID:          "hub.validate.execute",
		Domain:      "hub",
		Description: "Validate resource definitions against schema",
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
		ID:          "hub.integrations.read",
		Domain:      "hub",
		Description: "Read integration configurations",
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
		ID:          "hub.teamsmanifest.read",
		Domain:      "hub",
		Description: "Read Teams integration manifest",
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
		ID:          "hub.metrics.read",
		Domain:      "hub",
		Description: "Read metrics dashboard data",
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
		ID:          "agent.read",
		Domain:      "agent",
		Description: "Read a single agent's metadata by ID",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	// RS2: agent.list — split from agent.read because the list operation
	// has distinct authorization semantics: scope-based resolution with
	// store-pushed intersection, Mine/Shared classification via project
	// ownership (not agent creator), slug oracle prevention, and cursor
	// binding that includes the authorization context.
	{
		ID:          "agent.list",
		Domain:      "agent",
		Description: "List agents within the caller's authorized project scope",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "list-scope-resolver",
		BasePermission:   "agent.list",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		Invariants: []Invariant{
			{ID: "scope-pushed-query", Description: "Rows, totalCount, and nextCursor come from the same SQL predicate that includes the authorization scope", Kind: InvariantSecurity, FailClosed: true},
			{ID: "cursor-scope-binding", Description: "Cursor binding includes endpoint, caller filters, authorization scope, and principal/credential context", Kind: InvariantSecurity, FailClosed: true},
			{ID: "no-broad-query-on-none", Description: "ScopeSetNone produces empty list without issuing any resource query", Kind: InvariantSecurity, FailClosed: true},
			{ID: "slug-not-oracle", Description: "Project slug lookup for agent list filter must not distinguish unauthorized from nonexistent", Kind: InvariantSecurity, FailClosed: true},
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialCredentialInsufficient, DenialUserSuspended},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestRS2_AgentListScopePushed"},
			{Package: "pkg/hub", Function: "TestRS2_AgentListMineSharedClassification"},
			{Package: "pkg/hub", Function: "TestRS2_AgentListSlugOracle"},
			{Package: "pkg/hub", Function: "TestRS2_AgentListMultiPageInterleaved"},
			{Package: "pkg/hub", Function: "TestRS2_FailureInjection_PrincipalGroupClosure"},
			{Package: "pkg/hub", Function: "TestRS2_FailureInjection_StoreListCount"},
			{Package: "pkg/hub", Function: "TestRS2_CursorReplayAfterGrantRemoval"},
			{Package: "pkg/hub", Function: "TestRS2_CursorReplayAfterBindingExpiry"},
			{Package: "pkg/hub", Function: "TestRS2_AllPlusConstraint_EndToEnd"},
			{Package: "pkg/hub", Function: "TestRS2_ProductionAgentJWT"},
			{Package: "pkg/hub", Function: "TestRS2_SystemAllSharedSemantics"},
			{Package: "pkg/hub", Function: "TestRS2_GroupChangeCursorReplay"},
			{Package: "pkg/hub", Function: "TestRS2_ConstraintChangeCursorReplay"},
			{Package: "pkg/hub", Function: "TestRS2_TransitiveGroupAccess"},
			{Package: "pkg/hub", Function: "TestRS2_FilterCompositionMatrix"},
		},
	},
	{
		ID:          "agent.update",
		Domain:      "agent",
		Description: "Update agent configuration or metadata",
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
		ID:          "agent.attach",
		Domain:      "agent",
		Description: "Attach to an agent session via WebSocket",
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
		ID:          "agent.portaccess",
		Domain:      "agent",
		Description: "Access forwarded ports on an agent",
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
		ID:          "agent.stopall",
		Domain:      "agent",
		Description: "Stop all running agents in a project",
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
		ID:          "agent.setmessagemode",
		Domain:      "agent",
		Description: "Change an agent's message mode",
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
		ID:          "project.read",
		Domain:      "project",
		Description: "Read a single project's metadata by ID or slug",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groves/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "project-from-url",
		BasePermission:   "project.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
	},
	// RS2: project.list — split from project.read because the list operation
	// has distinct authorization semantics: scope-based resolution with
	// store-pushed intersection, Mine/Shared classification, and cursor binding
	// that includes the authorization context.
	{
		ID:          "project.list",
		Domain:      "project",
		Description: "List projects within the caller's authorized scope",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groves", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "list-scope-resolver",
		BasePermission:   "project.list",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		Invariants: []Invariant{
			{ID: "scope-pushed-query", Description: "Rows, totalCount, and nextCursor come from the same SQL predicate that includes the authorization scope", Kind: InvariantSecurity, FailClosed: true},
			{ID: "cursor-scope-binding", Description: "Cursor binding includes endpoint, caller filters, authorization scope, and principal/credential context", Kind: InvariantSecurity, FailClosed: true},
			{ID: "no-broad-query-on-none", Description: "ScopeSetNone produces empty list without issuing any resource query", Kind: InvariantSecurity, FailClosed: true},
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialCredentialInsufficient, DenialUserSuspended},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestRS2_ProjectListScopePushed"},
			{Package: "pkg/hub", Function: "TestRS2_ProjectListMineSharedClassification"},
			{Package: "pkg/hub", Function: "TestRS2_ProjectListCursorBinding"},
			{Package: "pkg/hub", Function: "TestRS2_ProjectListMultiPageInterleaved"},
			{Package: "pkg/hub", Function: "TestRS2_ProjectListInterleavedWithCallerFilter"},
			{Package: "pkg/hub", Function: "TestRS2_FailureInjection_PrincipalGroupClosure"},
			{Package: "pkg/hub", Function: "TestRS2_FailureInjection_StoreListCount"},
			{Package: "pkg/hub", Function: "TestRS2_CursorReplayAfterGrantRemoval"},
			{Package: "pkg/hub", Function: "TestRS2_CursorReplayAfterBindingExpiry"},
			{Package: "pkg/hub", Function: "TestRS2_AllPlusConstraint_EndToEnd"},
			{Package: "pkg/hub", Function: "TestRS2_MalformedConstraintExclusionHTTP"},
			{Package: "pkg/hub", Function: "TestRS2_SystemAllSharedSemantics"},
			{Package: "pkg/hub", Function: "TestRS2_GroupChangeCursorReplay"},
			{Package: "pkg/hub", Function: "TestRS2_ConstraintChangeCursorReplay"},
			{Package: "pkg/hub", Function: "TestRS2_SuspensionCursorReplay"},
			{Package: "pkg/hub", Function: "TestRS2_CredentialChangeCursorReplay"},
			{Package: "pkg/hub", Function: "TestRS2_TransferredOwnership"},
			{Package: "pkg/hub", Function: "TestRS2_TransitiveGroupAccess"},
			{Package: "pkg/hub", Function: "TestRS2_FilterCompositionMatrix"},
		},
	},
	{
		ID:          "project.update",
		Domain:      "project",
		Description: "Update project settings and metadata",
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
		ID:          "project.register",
		Domain:      "project",
		Description: "Register a project or grove from an external source",
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
		ID:          "skill.read",
		Domain:      "skill",
		Description: "Read skill definitions or list/discover skills",
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
		ID:          "skill.create",
		Domain:      "skill",
		Description: "Create a new skill definition",
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
		ID:          "skill.update",
		Domain:      "skill",
		Description: "Update an existing skill definition",
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
		ID:          "skill.delete",
		Domain:      "skill",
		Description: "Delete a skill definition",
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
		ID:          "skill.register",
		Domain:      "skill",
		Description: "Register skills in a skill registry",
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
		ID:          "template.read",
		Domain:      "template",
		Description: "Read template definitions or discover available templates",
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
		ID:          "template.create",
		Domain:      "template",
		Description: "Create a new template or import resources",
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
		ID:          "template.update",
		Domain:      "template",
		Description: "Update an existing template definition",
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
		ID:          "template.delete",
		Domain:      "template",
		Description: "Delete a template definition",
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
		ID:          "harnessconfig.read",
		Domain:      "harnessconfig",
		Description: "Read harness configurations or list available configs",
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
		ID:          "harnessconfig.create",
		Domain:      "harnessconfig",
		Description: "Create a new harness configuration",
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
		ID:          "harnessconfig.update",
		Domain:      "harnessconfig",
		Description: "Update a harness configuration",
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
		ID:          "harnessconfig.delete",
		Domain:      "harnessconfig",
		Description: "Delete a harness configuration",
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
		ID:          "group.read",
		Domain:      "group",
		Description: "Read group details or list groups",
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
		ID:          "group.create",
		Domain:      "group",
		Description: "Create a new group",
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
		ID:          "group.update",
		Domain:      "group",
		Description: "Update group metadata",
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
		ID:          "user.read",
		Domain:      "user",
		Description: "Read user profile or list users",
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
		ID:          "user.update",
		Domain:      "user",
		Description: "Update user profile or settings (PATCH may also dispatch user.admin.suspend/promote per field)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}", Method: "PATCH"},
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
		ID:          "broker.read",
		Domain:      "broker",
		Description: "Read runtime broker status or list brokers",
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
		ID:          "gcp.identity.read",
		Domain:      "gcp.identity",
		Description: "Read GCP service account details or list accounts",
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
		ID:          "role.read",
		Domain:      "role",
		Description: "Read role definitions and permission registry",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/export", Method: "GET"},
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
		ID:          "role.binding.read",
		Domain:      "role.binding",
		Description: "Read role binding assignments",
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
		ID:          "access.constraint.read",
		Domain:      "access.constraint",
		Description: "Read access constraint definitions",
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
		ID:          "quota.read",
		Domain:      "quota",
		Description: "Read limit definitions, entitlements, and usage",
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
		ID:          "quota.create",
		Domain:      "quota",
		Description: "Create limit definitions and entitlement bindings",
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
		ID:          "quota.update",
		Domain:      "quota",
		Description: "Update limit definitions and entitlement bindings",
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
		ID:          "quota.delete",
		Domain:      "quota",
		Description: "Delete limit definitions and entitlement bindings",
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
		ID:          "schedule.event.read",
		Domain:      "schedule",
		Description: "Read scheduled events or list events in a project",
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
		ID:          "schedule.event.create",
		Domain:      "schedule",
		Description: "Create a scheduled event or recurring schedule",
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
		ID:          "schedule.event.update",
		Domain:      "schedule",
		Description: "Update a recurring schedule",
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
		ID:          "schedule.event.delete",
		Domain:      "schedule",
		Description: "Cancel a scheduled event or delete a recurring schedule",
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
		ID:          "chat.access",
		Domain:      "chat",
		Description: "Access chat threads, spaces, topics, and messages within a project",
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
		ID:          "env.read",
		Domain:      "env",
		Description: "Read project environment variables",
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
	// /api/v1/auth/tokens/: GET is self-service (exempted); POST {id}/revoke and DELETE {id} are both
	// cataloged as credential.token.revoke (RS4/A5: one operation, two entry points).
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

	// Access constraint preview endpoints — hub-admin, access_constraint.admin permission
	{Pattern: "/api/v1/admin/access-constraint-previews", Kind: ExemptionHubAdmin, Reason: "Access constraint previews, hub-admin with access_constraint.admin; PR #1445 B5 governance", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/admin/access-constraint-previews/", Kind: ExemptionHubAdmin, Reason: "Access constraint preview by ID, hub-admin with access_constraint.admin; PR #1445 B5 governance", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/admin/effective-access", Kind: ExemptionAuthenticationOnly, Reason: "Admin effective-access composition, inline hub.audit.read check", Owner: "route_metadata.go"},
}

// MutationClassifications maps every discovered security-relevant mutation
// call site to either a catalog operation or a reviewed exemption. This table
// is the R3 exit-gate artifact: every scanner-discovered site must appear
// here, and every entry here must still be discoverable by the scanner.
//
// Organized by source file for reviewability.
var MutationClassifications = []MutationClassification{
	// -----------------------------------------------------------------------
	// pkg/hub/project_membership_service.go — RS1 bounded domain service
	// Mutations moved from handlers to the service in RS1. Handlers now
	// delegate to the service and never directly mutate RoleBindings.
	// -----------------------------------------------------------------------
	{File: "pkg/hub/project_membership_service.go", Function: "AddMember", Symbol: "CreateRoleBinding", OperationID: "project.membership.add"},
	{File: "pkg/hub/project_membership_service.go", Function: "UpdateMemberRole", Symbol: "CreateRoleBinding", OperationID: "project.membership.update"},
	{File: "pkg/hub/project_membership_service.go", Function: "UpdateMemberRole", Symbol: "DeleteRoleBinding", OperationID: "project.membership.update"},
	{File: "pkg/hub/project_membership_service.go", Function: "RemoveMember", Symbol: "DeleteRoleBinding", OperationID: "project.membership.remove"},
	{File: "pkg/hub/project_membership_service.go", Function: "TransferOwnership", Symbol: "CreateRoleBinding", OperationID: "project.membership.transfer"},
	{File: "pkg/hub/project_membership_service.go", Function: "replaceBindingTx", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "RS1 one-binding invariant: atomic binding replacement used by AddMember/UpdateMemberRole/TransferOwnership; always called from a governed service method", Scope: "pkg/hub/project_membership_service.go"}},
	{File: "pkg/hub/project_membership_service.go", Function: "replaceBindingTx", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "RS1 one-binding invariant: atomic binding replacement cleanup; always called from a governed service method", Scope: "pkg/hub/project_membership_service.go"}},
	{File: "pkg/hub/project_membership_service.go", Function: "MigrateMultiRoleBindings", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "RS1 R-3 pre-constraint migration: removes duplicate bindings keeping highest authority; idempotent, admin-only, runs within transaction", Scope: "pkg/hub/project_membership_service.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_roles.go — role/binding CRUD
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_roles.go", Function: "createRoleBinding", Symbol: "CreateRoleBinding", OperationID: "role.binding.create"},
	{File: "pkg/hub/handlers_roles.go", Function: "deleteRoleBinding", Symbol: "DeleteRoleBinding", OperationID: "role.binding.delete"},
	{File: "pkg/hub/handlers_roles.go", Function: "createRoleDefinition", Symbol: "CreateRoleDefinition", OperationID: "role.definition.create"},
	{File: "pkg/hub/handlers_roles.go", Function: "importRoleDefinitions", Symbol: "CreateRoleDefinition", OperationID: "role.definition.create"},
	{File: "pkg/hub/handlers_roles.go", Function: "duplicateRoleDefinition", Symbol: "CreateRoleDefinition", OperationID: "role.definition.create"},
	{File: "pkg/hub/handlers_roles.go", Function: "updateRoleDefinition", Symbol: "UpdateRoleDefinition", OperationID: "role.definition.update"},
	{File: "pkg/hub/handlers_roles.go", Function: "deleteRoleDefinition", Symbol: "DeleteRoleDefinition", OperationID: "role.definition.delete"},

	// -----------------------------------------------------------------------
	// pkg/hub/access_constraint_governance.go — B5 transactional governance
	// PR #1445 moved store mutations from handlers to the governance layer:
	// CommitBoundaryChange, compensateAuditFailure, and ReplaceRoleBinding.
	// -----------------------------------------------------------------------
	{File: "pkg/hub/access_constraint_governance.go", Function: "CommitBoundaryChange", Symbol: "CreateAccessConstraint", OperationID: "access.constraint.create"},
	{File: "pkg/hub/access_constraint_governance.go", Function: "CommitBoundaryChange", Symbol: "UpdateAccessConstraint", OperationID: "access.constraint.update"},
	{File: "pkg/hub/access_constraint_governance.go", Function: "CommitBoundaryChange", Symbol: "DeleteAccessConstraint", OperationID: "access.constraint.delete"},
	{File: "pkg/hub/access_constraint_governance.go", Function: "compensateAuditFailure", Symbol: "CreateAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Governance compensating action: restores constraint after audit failure", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "compensateAuditFailure", Symbol: "UpdateAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Governance compensating action: restores constraint after audit failure", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "compensateAuditFailure", Symbol: "DeleteAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Governance compensating action: restores constraint after audit failure", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "ReplaceRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "B5 §5 atomic role binding replacement; exported GovernanceService method reachable only from access_constraint.admin-guarded handler paths; enforces lockout invariant independently on admin-role downgrades", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "ReplaceRoleBinding", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "B5 §5 atomic role binding replacement; exported GovernanceService method reachable only from access_constraint.admin-guarded handler paths; enforces lockout invariant independently on admin-role downgrades", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "ReplaceRoleBinding", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "B5 §5 atomic role binding replacement compensating delete: rolls back newly created binding when old-binding deletion fails, preventing dual-active-binding inconsistency", Scope: "pkg/hub/access_constraint_governance.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/access_constraint_recovery.go — constraint recovery operations
	// -----------------------------------------------------------------------
	{File: "pkg/hub/access_constraint_recovery.go", Function: "RecoverAll", Symbol: "UpdateAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Constraint recovery: re-enables constraints disabled by audit failure", Scope: "pkg/hub/access_constraint_recovery.go"}},
	{File: "pkg/hub/access_constraint_recovery.go", Function: "rollbackDisable", Symbol: "UpdateAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Constraint recovery rollback: restores constraint on disable failure", Scope: "pkg/hub/access_constraint_recovery.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_groups.go — group CRUD and membership
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_groups.go", Function: "createGroup", Symbol: "CreateGroup", OperationID: "group.create"},
	{File: "pkg/hub/handlers_groups.go", Function: "createGroup", Symbol: "AddGroupMember", OperationID: "group.create"},
	{File: "pkg/hub/handlers_groups.go", Function: "updateGroup", Symbol: "UpdateGroup", OperationID: "group.update"},
	{File: "pkg/hub/handlers_groups.go", Function: "deleteGroup", Symbol: "DeleteGroup", OperationID: "group.delete"},
	{File: "pkg/hub/handlers_groups.go", Function: "addGroupMember", Symbol: "AddGroupMember", OperationID: "group.member.add"},
	{File: "pkg/hub/handlers_groups.go", Function: "removeGroupMember", Symbol: "RemoveGroupMember", OperationID: "group.member.remove"},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_gcp_identity.go — GCP service account operations
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "createGCPServiceAccount", Symbol: "CreateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "createGCPServiceAccount", Symbol: "UpdateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "createGCPServiceAccount", Symbol: "UpdateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "deleteGCPServiceAccount", Symbol: "DeleteGCPServiceAccount", OperationID: "gcp.identity.delete"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "handleAgentGCPToken", Symbol: "GenerateAccessToken", OperationID: "gcp.identity.mint"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "CreateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "CreateServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "DeleteServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "DeleteServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "DeleteServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "SetIAMPolicy", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "SetIAMPolicy", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "runGCPServiceAccountVerification", Symbol: "UpdateGCPServiceAccount", OperationID: "gcp.identity.verify"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "runGCPServiceAccountVerification", Symbol: "UpdateGCPServiceAccount", OperationID: "gcp.identity.verify"},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_gcp_identity_scoped.go — hub-scoped GCP identity
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "createHubScopedGCPServiceAccount", Symbol: "CreateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "createHubScopedGCPServiceAccount", Symbol: "UpdateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "deleteGCPServiceAccountByID", Symbol: "DeleteGCPServiceAccount", OperationID: "gcp.identity.delete"},

	// -----------------------------------------------------------------------
	// pkg/hub/useraccesstoken.go — user access token CRUD
	// -----------------------------------------------------------------------
	{File: "pkg/hub/useraccesstoken.go", Function: "CreateToken", Symbol: "CreateUserAccessToken", OperationID: "credential.token.create"},
	{File: "pkg/hub/useraccesstoken.go", Function: "RevokeToken", Symbol: "RevokeUserAccessToken", OperationID: "credential.token.revoke"},
	{File: "pkg/hub/useraccesstoken.go", Function: "DeleteToken", Symbol: "DeleteUserAccessToken", OperationID: "credential.token.revoke"},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_users_core.go — user management
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_users_core.go", Function: "deleteUser", Symbol: "DeleteUser", OperationID: "user.admin.delete"},
	{File: "pkg/hub/handlers_users_core.go", Function: "updateUser", Symbol: "UpdateUser", OperationID: "user.update"},
	{File: "pkg/hub/handlers_users_core.go", Function: "executeRoleTransition", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Atomic User.Role update inside role-transition transaction; caller updateUserRole checks CanDelegate", Scope: "pkg/hub/handlers_users_core.go"}},
	{File: "pkg/hub/handlers_users_core.go", Function: "createSuperAdminBindingTx", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Super-admin binding creation inside role-transition transaction; caller updateUserRole checks CanDelegate; uses SystemReconcileCreatedBy sentinel", Scope: "pkg/hub/handlers_users_core.go"}},
	{File: "pkg/hub/handlers_users_core.go", Function: "deleteSuperAdminBindingTx", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Super-admin binding deletion inside role-transition transaction; caller updateUserRole checks CanDelegate; guarded by checkLastSuperAdminTx", Scope: "pkg/hub/handlers_users_core.go"}},
	{File: "pkg/hub/handlers_users_core.go", Function: "ensureHubMemberBindingTx", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Hub-member binding creation during demotion; non-privileged role, ensures basic directory access; uses SystemReconcileCreatedBy sentinel", Scope: "pkg/hub/handlers_users_core.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_agents_core.go — agent lifecycle
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_agents_core.go", Function: "performAgentDelete", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent delete handler, route-guarded by agent.update permission", Scope: "pkg/hub/handlers_agents_core.go"}},
	{File: "pkg/hub/handlers_agents_core.go", Function: "performAgentDelete", Symbol: "RevokeAgentCredentialsByAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent delete cleanup, revokes credentials as part of delete", Scope: "pkg/hub/handlers_agents_core.go"}},
	{File: "pkg/hub/handlers_agents_core.go", Function: "createAgentInProject", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_agents_core.go"}},
	{File: "pkg/hub/handlers_agents_core.go", Function: "createAgentInProject", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_agents_core.go"}},
	{File: "pkg/hub/handlers_agents_core.go", Function: "createAgentInProject", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_agents_core.go"}},
	{File: "pkg/hub/handlers_agents_core.go", Function: "createAgentInProject", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_agents_core.go"}},
	{File: "pkg/hub/handlers_agents_core.go", Function: "handleAgentTokenRefresh", Symbol: "RevokeAgentCredential", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Agent token refresh, agent-JWT auth; old credential revoked on refresh", Scope: "pkg/hub/handlers_agents_core.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_agent_create_helpers.go
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_agent_create_helpers.go", Function: "handleExistingAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Existing agent cleanup during create, route-guarded by agent.create path", Scope: "pkg/hub/handlers_agent_create_helpers.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_agent_lifecycle.go
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_agent_lifecycle.go", Function: "suspendAgent", Symbol: "RevokeAgentCredentialsByAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent suspend revokes credentials, route-guarded by agent.update permission", Scope: "pkg/hub/handlers_agent_lifecycle.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_projects_core.go — project lifecycle
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteRoleBindingsForScope", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, cleans up bindings on failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteRoleBindingsForScope", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, cleans up bindings on failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectGroup", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: creates project groups", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectMembersGroup", Symbol: "AddGroupMember", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: adds creator to members group", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectMembersGroup", Symbol: "AddGroupMember", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: adds creator to members group", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectMembersGroup", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: creates members group", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectMembersGroup", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: creates members group", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectMembersGroup", Symbol: "UpdateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: updates members group", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectOwnerRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: creates owner role binding", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: creates project role binding", Scope: "pkg/hub/handlers_projects_core.go"}},
	// RS3: deleteProject handler now delegates to ProjectDeletionService.
	// Cascade mutations are in the service's cascadeSecurityState method.
	{File: "pkg/hub/project_deletion_service.go", Function: "cascadeSecurityState", Symbol: "DeleteRoleBindingsForScope", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/project_deletion_service.go", Function: "cascadeSecurityState", Symbol: "DeleteGroup", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/project_deletion_service.go", Function: "cascadeSecurityState", Symbol: "DeleteSecretsByScope", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/project_deletion_service.go", Function: "cascadeSecurityState", Symbol: "DeleteGCPServiceAccount", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/project_deletion_service.go", Function: "Delete", Symbol: "DeleteProject", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/handlers_projects_core.go", Function: "handleProjectRegister", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project register rollback, deletes on failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "migrateProjectSlug", Symbol: "UpdateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project slug migration, updates group names", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "migrateProjectSlug", Symbol: "UpdateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project slug migration, updates group names", Scope: "pkg/hub/handlers_projects_core.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/project_clone.go
	// -----------------------------------------------------------------------
	{File: "pkg/hub/project_clone.go", Function: "handleProjectClone", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project clone rollback, deletes on failure", Scope: "pkg/hub/project_clone.go"}},
	{File: "pkg/hub/project_clone.go", Function: "handleProjectClone", Symbol: "DeleteRoleBindingsForScope", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project clone rollback, cleans up bindings on failure", Scope: "pkg/hub/project_clone.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_auth.go — auth flow user provisioning
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_auth.go", Function: "provisionUser", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "User provisioning during auth login flow, pre-authorization", Scope: "pkg/hub/handlers_auth.go"}},
	{File: "pkg/hub/handlers_auth.go", Function: "provisionUser", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "User record update during auth login flow", Scope: "pkg/hub/handlers_auth.go"}},
	{File: "pkg/hub/handlers_auth.go", Function: "ensureSuperAdminBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Idempotent super-admin binding during authorized user provisioning", Scope: "pkg/hub/handlers_auth.go"}},
	{File: "pkg/hub/handlers_auth.go", Function: "handleAuthRefresh", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "User last-login update during token refresh", Scope: "pkg/hub/handlers_auth.go"}},
	{File: "pkg/hub/handlers_auth.go", Function: "deleteSuperAdminBinding", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Super-admin self-demotion, hub-admin operation", Scope: "pkg/hub/handlers_auth.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/web.go — OAuth/session middleware
	// -----------------------------------------------------------------------
	{File: "pkg/hub/web.go", Function: "handleOAuthCallback", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "OAuth callback user provisioning", Scope: "pkg/hub/web.go"}},
	{File: "pkg/hub/web.go", Function: "handleOAuthCallback", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "OAuth callback user record update", Scope: "pkg/hub/web.go"}},
	{File: "pkg/hub/web.go", Function: "proxyAuthMiddleware", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Proxy auth user provisioning", Scope: "pkg/hub/web.go"}},
	{File: "pkg/hub/web.go", Function: "proxyAuthMiddleware", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Proxy auth user record update", Scope: "pkg/hub/web.go"}},
	{File: "pkg/hub/web.go", Function: "sessionToBearerMiddleware", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Session-to-bearer token conversion middleware", Scope: "pkg/hub/web.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_test_login.go — dev/test login
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_test_login.go", Function: "handleTestLogin", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Development-only test login endpoint", Scope: "pkg/hub/handlers_test_login.go"}},
	{File: "pkg/hub/handlers_test_login.go", Function: "handleTestLogin", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Development-only test login endpoint", Scope: "pkg/hub/handlers_test_login.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/admin_allow_list.go — hub admin: allow-list management
	// -----------------------------------------------------------------------
	{File: "pkg/hub/admin_allow_list.go", Function: "handleAdminAllowListAdd", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Admin allow-list add, hub-admin operation", Scope: "pkg/hub/admin_allow_list.go"}},
	{File: "pkg/hub/admin_allow_list.go", Function: "handleAdminAllowListByEmail", Symbol: "DeleteUser", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Admin allow-list remove, hub-admin operation", Scope: "pkg/hub/admin_allow_list.go"}},
	{File: "pkg/hub/admin_allow_list.go", Function: "handleAdminAllowListImport", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Admin allow-list import, hub-admin operation", Scope: "pkg/hub/admin_allow_list.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/admin_invites.go — hub admin: invite management
	// -----------------------------------------------------------------------
	{File: "pkg/hub/admin_invites.go", Function: "handleAdminInviteDelete", Symbol: "DeleteInviteCode", OperationID: "user.admin.invite"},
	{File: "pkg/hub/admin_invites.go", Function: "handleAdminInviteRevoke", Symbol: "RevokeInviteCode", OperationID: "user.admin.invite"},

	// -----------------------------------------------------------------------
	// pkg/hub/admin_user_invite.go — hub admin: user invite
	// -----------------------------------------------------------------------
	{File: "pkg/hub/admin_user_invite.go", Function: "handleAdminUserInvite", Symbol: "CreateUser", OperationID: "user.admin.invite"},
	{File: "pkg/hub/admin_user_invite.go", Function: "handleAdminUserInviteBulk", Symbol: "CreateUser", OperationID: "user.admin.invite"},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_chat_secrets.go — chat integration secrets
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_chat_secrets.go", Function: "HasChatIntegrationSecret", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Chat integration secret check, route-guarded by hub admin", Scope: "pkg/hub/handlers_chat_secrets.go"}},
	{File: "pkg/hub/handlers_chat_secrets.go", Function: "LoadChatIntegrationSecret", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Chat integration secret load, route-guarded by hub admin", Scope: "pkg/hub/handlers_chat_secrets.go"}},
	{File: "pkg/hub/handlers_chat_secrets.go", Function: "SetChatIntegrationSecret", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Chat integration secret write, route-guarded by hub admin", Scope: "pkg/hub/handlers_chat_secrets.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_github_app.go — GitHub App admin
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_github_app.go", Function: "loadGitHubAppSecret", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "GitHub App secret read, hub-admin operation", Scope: "pkg/hub/handlers_github_app.go"}},
	{File: "pkg/hub/handlers_github_app.go", Function: "setGitHubAppSecret", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "GitHub App secret write, hub-admin operation", Scope: "pkg/hub/handlers_github_app.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/invite_service.go — invite code creation
	// -----------------------------------------------------------------------
	{File: "pkg/hub/invite_service.go", Function: "CreateInvite", Symbol: "CreateInviteCode", OperationID: "user.admin.invite"},

	// -----------------------------------------------------------------------
	// pkg/hub/seed.go — server startup seed/reconciliation
	// -----------------------------------------------------------------------
	{File: "pkg/hub/seed.go", Function: "ReconcileSuperAdminBindings", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: reconcile super-admin role bindings", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ReconcileSuperAdminBindings", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: reconcile super-admin role bindings", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ReconcileSuperAdminBindings", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: promote/demote super-admin users", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ReconcileSuperAdminBindings", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: promote/demote super-admin users", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "backfillProjectOwnerRoleBindings", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: backfill project owner role bindings", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "backfillUserRoleBindings", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: backfill user role bindings", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ensureDevUserRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: ensure dev user role binding", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ensureHubMembership", Symbol: "AddGroupMember", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: ensure hub membership for user", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "reconcileBuiltInRole", Symbol: "CreateRoleDefinition", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: create built-in role definition", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "reconcileBuiltInRole", Symbol: "UpdateSystemRoleDefinitionPermissions", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: reconcile built-in role permissions", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "seedDefaultGroupsAndBindings", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: seed default groups", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "seedDevUser", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: seed dev user", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "seedHubMemberRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: seed hub member role binding", Scope: "pkg/hub/seed.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/server.go — server infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/server.go", Function: "RecordAgentCredential", Symbol: "CreateAgentCredential", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Agent credential provisioning during agent create, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "a2aBridgeSweepHandler", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Background job: A2A bridge sweep generates GCP tokens", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "backupSigningKeyToStore", Symbol: "UpdateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key backup, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "backupSigningKeyToStore", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key backup, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "ensureSigningKey", Symbol: "DeleteSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key rotation, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "ensureSigningKey", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key load, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "ensureSigningKey", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key load, server infrastructure", Scope: "pkg/hub/server.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/oidckeys.go — OIDC key infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/oidckeys.go", Function: "backupKeyToStore", Symbol: "UpdateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset backup, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "backupKeyToStore", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset backup, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "casCreateKeyInStore", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset CAS create, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "casCreateKeyInStore", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset CAS read, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "loadKeysetFromDB", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset load, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "loadOrCreateKey", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset load-or-create, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "saveKeysetToDB", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset save, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/lifecycle_hook_executor.go — pre-start hook execution
	// -----------------------------------------------------------------------
	{File: "pkg/hub/lifecycle_hook_executor.go", Function: "resolveIdentityAndToken", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Pre-start hook: generates GCP token for hook execution", Scope: "pkg/hub/lifecycle_hook_executor.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/maintenance_executors.go — maintenance operations
	// -----------------------------------------------------------------------
	{File: "pkg/hub/maintenance_executors.go", Function: "Run", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Maintenance executor reads secrets, hub-admin operation", Scope: "pkg/hub/maintenance_executors.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/system_identity.go
	// -----------------------------------------------------------------------
	{File: "pkg/hub/system_identity.go", Function: "updateDevUserRecord", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Dev mode user record update, system identity management", Scope: "pkg/hub/system_identity.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/gcp_token_cache.go / gcp_token_iam.go — GCP token infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/gcp_token_cache.go", Function: "GenerateAccessToken", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "GCP token cache, delegates to IAM GenerateAccessToken", Scope: "pkg/hub/gcp_token_cache.go"}},
	{File: "pkg/hub/gcp_token_iam.go", Function: "GenerateAccessToken", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "GCP IAM token generation, infrastructure implementation", Scope: "pkg/hub/gcp_token_iam.go"}},
	{File: "pkg/hub/gcp_token_iam.go", Function: "VerifyImpersonation", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "GCP impersonation verification, infrastructure implementation", Scope: "pkg/hub/gcp_token_iam.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/brokerauth.go — broker authentication infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/brokerauth.go", Function: "CompleteBrokerJoin", Symbol: "CreateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker join completion, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "CompleteBrokerJoin", Symbol: "DeleteBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker join completion, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "CreateBrokerRegistration", Symbol: "CreateJoinToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker registration, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "GenerateAndStoreSecret", Symbol: "CreateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker secret generation, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "RotateBrokerSecret", Symbol: "UpdateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker secret rotation, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/brokerclient.go / controlchannel_client.go / httpdispatcher.go
	// — agent delete dispatch infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/brokerclient.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker client agent delete dispatch, infrastructure adapter", Scope: "pkg/hub/brokerclient.go"}},
	{File: "pkg/hub/controlchannel_client.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Control channel agent delete dispatch, infrastructure adapter", Scope: "pkg/hub/controlchannel_client.go"}},
	{File: "pkg/hub/controlchannel_client.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Control channel agent delete dispatch, infrastructure adapter", Scope: "pkg/hub/controlchannel_client.go"}},
	{File: "pkg/hub/httpdispatcher.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "HTTP dispatcher agent delete, infrastructure adapter", Scope: "pkg/hub/httpdispatcher.go"}},
	{File: "pkg/hub/httpdispatcher.go", Function: "DispatchAgentDelete", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "HTTP dispatcher agent delete dispatch, infrastructure adapter", Scope: "pkg/hub/httpdispatcher.go"}},

	// -----------------------------------------------------------------------
	// pkg/store/entadapter/ — store layer implementation
	// -----------------------------------------------------------------------
	{File: "pkg/store/entadapter/composite.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: composite DeleteAgent implementation", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/composite.go", Function: "DeleteProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: composite DeleteProject implementation", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/secret_store.go", Function: "UpsertSecret", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: UpsertSecret delegates to CreateSecret", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/secret_store.go", Function: "UpsertSecret", Symbol: "UpdateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: UpsertSecret delegates to UpdateSecret", Scope: "pkg/store/entadapter"}},

	// -----------------------------------------------------------------------
	// pkg/store/storetest/ — store interface conformance test fixtures
	// -----------------------------------------------------------------------
	{File: "pkg/store/storetest/domains.go", Function: "AgentDomain", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: agent domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "DeleteGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "UpdateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "DeleteGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "UpdateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "seedGCPScopeMix", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP scope seeding", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerJoinTokenDomain", Symbol: "CreateJoinToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker join token domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerSecretDomain", Symbol: "CreateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker secret domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerSecretDomain", Symbol: "DeleteBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker secret domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerSecretDomain", Symbol: "UpdateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker secret domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "ProjectDomain", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: project domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "DeleteSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "UpdateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "InviteCodeDomain", Symbol: "CreateInviteCode", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: invite code domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "InviteCodeDomain", Symbol: "DeleteInviteCode", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: invite code domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "DeleteUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain update", Scope: "pkg/store/storetest"}},
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
