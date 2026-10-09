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

// identityOperations lists the catalog operations for users, groups, roles, role bindings, access constraints and tokens.
var identityOperations = []OperationSpec{
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
			// handleGroupMemberByID (handlers_groups.go) requires the
			// member type ("user", "group", or "agent") as its own path
			// segment before the member ID.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members/{memberType}/{memberId}", Method: "DELETE"},
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
		ID:          "credential.token.read",
		Domain:      "credential",
		Description: "List or read the caller's own user access tokens",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/tokens", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/tokens/{id}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "self-principal",
		BasePermission:   "user.read",
		Effects:          []SecurityEffect{EffectListScoped, EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestSessionOnlyGate_ReasonIsReported"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Token reads are authenticated-only (user reads own tokens); no per-resource permission required beyond session validity",
			Scope:  "self-token management only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
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
		Bearer: SessionOnly(ReasonCredentialManagement),
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
		Bearer: SessionOnly(ReasonCredentialManagement),
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
		ID:          "user.admin.provision",
		Domain:      "user.admin",
		Description: "Pre-register a user (status invited) through POST /api/v1/users; invitation-equivalent, shares the invite creation core; no role, no grants",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "user.invite",
		// EffectCreateResource: the invited row. EffectIssueCredential and
		// GovernanceIssuerCredential label it an admission-conferring
		// record, as for user.admin.invite: under invite_only it admits a
		// later sign-in through a configured provider.
		Effects:        []SecurityEffect{EffectCreateResource, EffectIssueCredential},
		DelegationKind: DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Pre-registration admits sign-in under invite_only, identical to user.admin.invite",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "user.admin.provision",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			AfterFields:   []string{"target_user_id", "email", "status", "display_name"},
			Atomic:        true,
		},
		// forbidden: rows 4, 4a, 5 and 8 (row 4a carries the
		// dev_auth_not_supported reason, row 5 the session-only reason);
		// user_suspended: the auth middleware (row 3); conflict: an
		// existing record (rows 15-17); role_assignment_forbidden: the
		// denial-log classification of a request that names a role (row
		// 12, wire code unprocessable). credential_insufficient is added
		// with hub token admission (rows 6-7).
		DenialCodes: []DenialCode{DenialForbidden, DenialUserSuspended, DenialConflict,
			DenialRoleAssignmentForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestHandleProvisionUser"},
		},
		// Token admission opens with the hub UAT phase of this operation.
		Bearer: SessionOnly(ReasonGovernancePending),
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
		Bearer:      SessionOnly(ReasonGovernancePending),
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
		// last_owner: the user is the last active owner of a project.
		// conflict: last super-admin, self-delete, the user still has
		// agents (owned, descendants, or started by their schedules), or
		// the user's role bindings changed concurrently during the delete.
		DenialCodes: []DenialCode{DenialForbidden, DenialLastOwner, DenialConflict},
		TestRefs:    []TestRef{{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"}},
		Bearer:      SessionOnly(ReasonGovernancePending),
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
			// handleGroupRoutes (handlers_groups.go) dispatches on
			// r.Method: PATCH, not PUT.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}", Method: "PATCH"},
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
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}/export", Method: "GET"},
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
			// handleAdminRoleBindingByID (handlers_roles.go) has no GET on
			// a bare "/role-bindings/{id}" — that route is DELETE-only.
			// The only GET is the user-scoped lookup.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/role-bindings", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/role-bindings/user/{userId}", Method: "GET"},
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
	{
		ID:          "user.session.logout",
		Domain:      "user",
		Description: "Sign-in flow logout step; the hub holds no server-side session state for it to change",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/logout", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "none",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Sign-in flow step that reads and changes no hub state; no resource permission applies",
			Scope:  "session logout",
			Waives: []WaivedObligation{WaiveBasePermission, WaiveDenialCodes},
		}},
		Bearer: NonUser(),
	},
	{
		ID:          "user.session.revoke",
		Domain:      "user",
		Description: "Revoke every cookie session of a user (platform admin only)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}/revoke-sessions", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "user-from-url",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernancePeerSuperior,
			Description: "Only an unscoped local platform admin may revoke another user's sessions",
		},
		AuthorityEval: AuthorityEvalNone,
		DenialCodes:   []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestSessionOnlyGate_ReasonIsReported"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionHubAdmin,
			Reason: "Platform-admin role check (requireAdminFor); the session generation increment is logged, not audited",
			Scope:  "user session revocation",
			Waives: []WaivedObligation{WaiveBasePermission, WaiveAuditObligation},
		}},
		Bearer: SessionOnly(ReasonSessionRecovery),
	},
	{
		ID:          "user.terminalworkspace",
		Domain:      "user",
		Description: "Read or replace the caller's own terminal workspace",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/terminal-workspace", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/terminal-workspace", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectReadOne, EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestSessionOnlyGate_ReasonIsReported"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "The path names no user; the subject is always the caller, so no resource permission applies",
			Scope:  "caller's own terminal workspace",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: SessionOnly(ReasonInteractiveState),
	},
}
