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
		Description: "Create a custom role definition (create, import, duplicate). A token needs role:create and role:read on a hub boundary and the holder's live role.create authority; the holder must hold every permission of the role within the token's selectors. A project-scoped definition can be bound in every project, so it is judged on the holder's system-scope authority; a system-scoped definition is refused to every token",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/import", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}/duplicate", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestCustomRoleCreateToken_ProjectScopeJudgedOnHubAuthority"},
			{Package: "pkg/hub", Function: "TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "audit obligation waived; full operation contract deferred to AH1",
			Scope:  "AF1 catalog only",
			Waives: []WaivedObligation{WaiveAuditObligation},
		}},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "role.definition.update",
		Domain:      "role",
		Description: "Update a custom role definition. A token needs role:update and role:read on a hub boundary and the holder's live role.update authority; the holder must hold every permission of the updated role within the token's selectors, judged as for create",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "role.definition.delete",
		Domain:      "role",
		Description: "Delete a custom role definition. A token needs role:delete and role:read on a hub boundary and the holder's live role.delete authority; system roles and roles that are still bound are kept",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},

	// =====================================================================
	// Domain: role.binding — role binding management
	// =====================================================================
	{
		ID:          "role.binding.create",
		Domain:      "role.binding",
		Description: "Create a role binding (grant authority to a principal) for a custom project role or at system scope. A token needs role_binding:create on a hub boundary and the holder's live role_binding.create authority. A system-scoped binding is refused to every token. For a custom project role the holder must also have current project.manage access to the binding's project and hold every permission of the role in that project within the token's selectors",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/role-bindings", Method: "POST"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver:      "hub-scoped",
		BasePermission:        "role_binding.create",
		Effects:               []SecurityEffect{EffectGrantAuthority},
		DelegationKind:        DelegationNonAmplification,
		DelegationDescription: "Actor must hold all permissions in the bound role (CanDelegate)",
		AuthorityEval:         AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "role_binding_create",
			ContextFields: []string{"actor_id"},
			AfterFields:   []string{"principal_id", "role_name", "scope"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialRoleAssignmentForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestRoleBindingCreateToken_SystemScopeAlwaysDenied"},
			{Package: "pkg/hub", Function: "TestCustomRoleBindingToken_NeedsCreateSelectorAndProjectAccess"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "role.binding.delete",
		Domain:      "role.binding",
		Description: "Delete a system-scoped role binding (revoke authority from a principal). A token needs role_binding:delete on a hub boundary and the holder's live role_binding.delete authority; the constraint-admin lockout guard applies. Deleting a super-admin binding is refused to every token (session only)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/role-bindings/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
			EventType:     "role_binding_delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"principal_id", "role_name", "scope"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialRoleAssignmentForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestRoleBindingDeleteToken_SuperAdminBindingDenied"},
			{Package: "pkg/hub", Function: "TestRoleBindingDeleteToken_SystemBindingNeedsDeleteSelector"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},

	// =====================================================================
	// Domain: group — group and membership management
	// =====================================================================
	{
		ID:          "group.member.add",
		Domain:      "group",
		Description: "Add a member to a group. A user access token needs group:addMember on a hub boundary, and the group's closure may carry no authority beyond the token's own",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members", Method: "POST"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver:      "group-from-url",
		BasePermission:        "group.addMember",
		Effects:               []SecurityEffect{EffectGrantAuthority},
		DelegationKind:        DelegationNonAmplification,
		DelegationDescription: "Adding a member to a role-bearing group effectively grants authority; actor must hold the group's role permissions. A user access token is refused when the closure carries a system-scoped binding, and must hold, in each project the closure binds, every permission those bindings carry",
		AuthorityEval:         AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "group_member_add",
			ContextFields: []string{"actor_id", "group_id"},
			AfterFields:   []string{"member_principal_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestGroupMemberAddToken_ProjectBindingsOnClosureRequireCallerAuthority"},
			{Package: "pkg/hub", Function: "TestGroupMemberAddToken_SystemBindingsOnClosureDenied"},
			{Package: "pkg/hub", Function: "TestGroupChangeToken_SelectorAndBoundaryPerRoute"},
		},
		Bearer: AdmitOn(BearerTargetGroupRecord, BearerBoundaryHub),
	},
	{
		ID:          "group.member.remove",
		Domain:      "group",
		Description: "Remove a member from a group. A user access token needs group:removeMember on a hub boundary. Removing a member from a group that is the subject of an access constraint is session only",
		EntryPoints: []EntryPoint{
			// handleGroupMemberByID (handlers_groups.go) requires the
			// member type ("user", "group", or "agent") as its own path
			// segment before the member ID.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members/{memberType}/{memberId}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
			EventType:     "group_member_remove",
			ContextFields: []string{"actor_id", "group_id"},
			BeforeFields:  []string{"member_principal_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestGroupChangeToken_SelectorAndBoundaryPerRoute"},
			{Package: "pkg/hub", Function: "TestConstraintSubjectGroupToken_MemberRemoveSessionOnly"},
			{Package: "pkg/hub", Function: "TestConstraintSubjectGroupSession_MemberRemoveAndDeleteAdmitted"},
		},
		Bearer: AdmitOn(BearerTargetGroupRecord, BearerBoundaryHub),
	},
	{
		ID:          "group.delete",
		Domain:      "group",
		Description: "Delete a group. A user access token needs group:delete on a hub boundary. Deleting a group that is the subject of an access constraint is session only",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "group-from-url",
		BasePermission:   "group.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "group_delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"group_id", "group_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestGroupChangeToken_SelectorAndBoundaryPerRoute"},
			{Package: "pkg/hub", Function: "TestConstraintSubjectGroupToken_DeleteSessionOnly"},
			{Package: "pkg/hub", Function: "TestConstraintSubjectGroupSession_MemberRemoveAndDeleteAdmitted"},
		},
		Bearer: AdmitOn(BearerTargetGroupRecord, BearerBoundaryHub),
	},

	// =====================================================================
	// Domain: access.constraint — access constraint management
	// =====================================================================
	{
		ID:          "access.constraint.create",
		Domain:      "access.constraint",
		Description: "Create an access constraint (tighten boundary). A token needs access_constraint:admin and access_constraint:read on a hub boundary and the holder's live access_constraint.admin authority; the preview token and governance checks apply, and the commit re-checks judge the holder's authority within the token's selectors",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestConstraintAdminToken_RequiresConstraintAdminSelector"},
			{Package: "pkg/hub", Function: "TestConstraintCommit_AppliesTokenSelectors"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "access.constraint.update",
		Domain:      "access.constraint",
		Description: "Update an access constraint (may relax or tighten boundary). A token needs access_constraint:admin and access_constraint:read on a hub boundary and the holder's live access_constraint.admin authority, and may only tighten: an update that relaxes the boundary (adds permissions, changes subject or scope, or narrows or adds its time window) is session only. The commit re-checks judge the holder's authority within the token's selectors",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestConstraintAdminToken_RequiresConstraintAdminSelector"},
			{Package: "pkg/hub", Function: "TestConstraintRelaxToken_SessionOnly"},
			{Package: "pkg/hub", Function: "TestConstraintCommit_AppliesTokenSelectors"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "access.constraint.delete",
		Domain:      "access.constraint",
		Description: "Delete an access constraint (relax boundary). Session only: deleting a boundary relaxes it, and a user access token is refused before the preview token is used",
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestConstraintRelaxToken_SessionOnly"},
		},
		Bearer: SessionOnly(ReasonGovernancePending),
	},
	{
		ID:          "access.constraint.preview",
		Domain:      "access.constraint",
		Description: "Generate an access constraint preview and read a preview job and its result. A token needs access_constraint:admin on a hub boundary and the holder's live access_constraint.admin authority",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraint-previews", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraint-previews/{jobId}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraint-previews/{jobId}/result", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "access_constraint.admin",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestConstraintAdminToken_RequiresConstraintAdminSelector"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
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
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/tokens", Method: "POST", Response: ResponseCredential},
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
	{
		ID:          "federation.token.mint",
		Domain:      "federation",
		Description: "Obtain a short-lived user access credential for the caller, signed with the hub's OIDC key, for one peer hub audience listed in server.oidc.user_token_audiences. Only a local human user with status active; a user access token needs federation_token:mint on a hub boundary. 404 while federation.project_visitors is off",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/federation-token", Method: "POST", Response: ResponseCredential},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		BasePermission:   "federation_token.mint",
		Effects:          []SecurityEffect{EffectMintCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "A user obtains a credential for themselves only, for an audience the hub admin allows, with a lifetime capped at one hour; it carries no project or agent claims",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "federation_token_issued",
			ContextFields: []string{"actor_id", "credential_kind"},
			AfterFields:   []string{"audience", "jti", "expires_at"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestFederationToken_RefusedCallers"},
			{Package: "pkg/hub", Function: "TestFederationToken_UserAccessTokenNeedsMintScope"},
			{Package: "pkg/hub", Function: "TestFederationToken_AudienceAllowList"},
			{Package: "pkg/hub", Function: "TestFederationToken_AuditWritten"},
			{Package: "pkg/hub", Function: "TestFederationToken_AuditFailureIssuesNothing"},
			{Package: "pkg/hub", Function: "TestFederationToken_RefusedCredentialKinds"},
		},
		Bearer: AdmitOn(BearerTargetSelfRecord, BearerBoundaryHub),
	},

	// =====================================================================
	// Domain: user.admin — user administration
	// =====================================================================
	{
		ID:          "user.admin.suspend",
		Domain:      "user.admin",
		Description: "Suspend or reactivate a user account (dispatched from PATCH /api/v1/users/{id} when status field is present). A token needs user:suspend on a hub boundary, checked on the hub-level user record. The caller's own status and activating an invited user are refused for every credential",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointInternalDispatch, Pattern: "updateUser:status-field"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestUserPatchToken_StatusRequiresSuspendSelector"},
			{Package: "pkg/hub", Function: "TestUserPatchToken_SelfStatusGuardStillApplies"},
		},
		Bearer: BearerDisposition{Kind: BearerAdmit, Target: BearerTargetHubInstance, Boundaries: []BearerBoundary{BearerBoundaryHub}, Pin: "TestUserPatchToken_StatusRequiresSuspendSelector"},
	},

	// =====================================================================
	// Domain: user.admin — user administration (continued, HIGH-RISK)
	// =====================================================================
	{
		ID:          "user.admin.invite",
		Domain:      "user.admin",
		Description: "Invite users to the platform (single or bulk) and manage invitation codes (list, create, read stats, read, revoke, delete). The invited record has status invited, the placeholder role member and no role binding or group membership; an invitation code names no role. A token needs user:invite on the hub boundary and the holder's live user.invite authority",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/users/invite", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/users/invite/bulk", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites", Method: "POST", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites/stats", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites/{id}", Method: "GET"},
			// Revoke is listed before delete: the live checks address one
			// invitation fixture, which the delete removes.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites/{id}/revoke", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestInviteToken_RequiresInviteSelectorAndHubBoundary"},
			{Package: "pkg/hub", Function: "TestInvitationToken_CreatesNoRoleBindingOrGroupMembership"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "user.admin.provision",
		Domain:      "user.admin",
		Description: "Pre-register a user (status invited) through POST /api/v1/users; invitation-equivalent, shares the invite creation core; no role, no grants. A token needs user:invite on the hub boundary and the holder's live user.invite authority, checked at the hub user collection; the user_provision audit records the token as the credential. Refused for every caller on a hub running with dev authentication",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
		// forbidden: rows 4, 4a and 8 (row 4a carries the
		// dev_auth_not_supported reason; a credential kind other than a
		// session or a user access token gets the session-only reason);
		// credential_insufficient: the details.reason of a token without
		// user:invite on a hub boundary (rows 6-7, wire code forbidden);
		// user_suspended: the auth middleware (row 3); conflict: an
		// existing record (rows 15-17); role_assignment_forbidden: the
		// denial-log classification of a request that names a role (row
		// 12, wire code unprocessable).
		DenialCodes: []DenialCode{DenialForbidden, DenialCredentialInsufficient, DenialUserSuspended, DenialConflict,
			DenialRoleAssignmentForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestHandleProvisionUser"},
			{Package: "pkg/hub", Function: "TestProvisionToken_RequiresInviteSelectorAndHubBoundary"},
			{Package: "pkg/hub", Function: "TestProvisionToken_DetailViewNeedsReadSelector"},
			{Package: "pkg/hub", Function: "TestProvisionToken_RecordOutlivesTokenAndHolderAuthority"},
			{Package: "pkg/hub", Function: "TestInvitationToken_CreatesNoRoleBindingOrGroupMembership"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
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
		// user_delete_blocked: the user has agents (owned, descendants,
		// or started by their schedules) or active delegations to agents
		// that exist.
		// conflict: last super-admin, self-delete, or the user's role
		// bindings changed concurrently during the delete.
		// With force=true (hub.user_delete_force_cascade): the caller
		// also needs agent.delete on every blocking agent
		// (user_delete_force_forbidden otherwise); the agents may not
		// change before the transaction (user_delete_blockers_changed).
		// user_deletion_in_progress: a forced deletion of the user is in
		// progress (a plain delete is refused; a forced one joins it).
		DenialCodes: []DenialCode{DenialForbidden, DenialLastOwner, DenialUserDeleteBlocked, DenialConflict,
			DenialUserDeleteForceForbidden, DenialUserDeleteBlockersChanged, DenialUserDeletionInProgress},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestDeleteUser_GuardOrder"},
			{Package: "pkg/hub", Function: "TestForceDeleteUser_ForbiddenWritesNothing"},
		},
		Bearer: SessionOnly(ReasonGovernancePending),
	},
	{
		ID:          "user.admin.deleteblockers",
		Domain:      "user.admin",
		Description: "List what blocks deleting a user (agents and active delegations), without deleting",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}/delete-blockers", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "user-from-url",
		BasePermission:   "user.delete",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestUserDeleteBlockers_RequiresUserDelete"},
			{Package: "pkg/hub", Function: "TestSessionOnlyGate_ReasonIsReported"},
		},
		Bearer: SessionOnly(ReasonGovernancePending),
	},
	{
		ID:          "user.admin.deletion",
		Domain:      "user.admin",
		Description: "Read the forced deletion of a user: its state and the outcome of each of the user's agents",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}/deletion", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "user-from-url",
		BasePermission:   "user.delete",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		// not_found: the user has no deletion, or
		// hub.user_delete_force_cascade is off.
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestUserDeletion_RequiresUserDelete"},
		},
		Bearer: SessionOnly(ReasonGovernancePending),
	},

	// =====================================================================
	// Domain: group — group read, list, create, update
	// =====================================================================
	{
		ID:          "group.read",
		Domain:      "group",
		Description: "Read group details",
		EntryPoints: []EntryPoint{
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
		ID:          "group.list",
		Domain:      "group",
		Description: "List groups. Each row is shown only when the caller may read it (group.read); a user access token needs group:read on a hub boundary, and a group with no project resolves to the hub",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "group-per-row",
		BasePermission:   "group.read",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestExplicitGroupToken_ResolvesHubTarget"},
		},
		Bearer: AdmitOn(BearerTargetGroupRecord, BearerBoundaryHub),
	},
	{
		ID:          "group.create",
		Domain:      "group",
		Description: "Create a new group. A user access token needs group:create on a hub boundary; the target is the hub's group collection. Creating under a parent group runs the member-add delegation check on the parent",
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestGroupCreateToken_RequiresCreateSelectorOnHubBoundary"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
	{
		ID:          "group.update",
		Domain:      "group",
		Description: "Update group metadata. A user access token that changes the owner needs group:update and live update access on the group, and for the new owner it must pass the delegation check of a member add on the group's role bindings; group:addMember and the group role hierarchy do not apply. A refused owner change returns one fixed 403, including for a group that does not exist",
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
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestGroupChangeToken_SelectorAndBoundaryPerRoute"},
			{Package: "pkg/hub", Function: "TestGroupOwnerChangeToken_RequiresDelegationForNewOwner"},
		},
		Bearer: AdmitOn(BearerTargetGroupRecord, BearerBoundaryHub),
	},

	// =====================================================================
	// Domain: user — user read and update
	// =====================================================================
	{
		ID:          "user.read",
		Domain:      "user",
		Description: "Read a user profile",
		EntryPoints: []EntryPoint{
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
		ID:          "user.list",
		Domain:      "user",
		Description: "List users. A session sees the users it may read. A token needs user:list at the hub collection on a hub boundary, and then sees only the users it may read, each checked as a hub-level user record, so it also needs user:read",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "user.list",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestUserListToken_RequiresUserListAndHubBoundary"}},
		Bearer:           AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
	{
		ID:          "user.update",
		Domain:      "user",
		Description: "Update another user's profile (display name and preferences). The same PATCH dispatches user.admin.suspend for a status field and user.admin.promote for a role field, and every field's check passes before any write. A token needs user:update on a hub boundary, checked on the hub-level user record; a token PATCH that names no status field is checked as a profile update",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}", Method: "PATCH", Variant: "cross_user"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "user-from-url",
		BasePermission:   "user.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestUserPatchToken_CrossUserProfileRequiresUpdateSelector"},
			{Package: "pkg/hub", Function: "TestUserPatchToken_MixedFieldsAllOrNothing"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "user.self.update",
		Domain:      "user",
		Description: "Update the caller's own profile (display name and preferences) through PATCH /api/v1/users/{id} naming the caller's own record. A status field on the caller's own record is refused by the self-status guard",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/{id}", Method: "PATCH", Variant: "self"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestUserPatchToken_SelfProfileSessionOnly"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "The path names the caller's own record, so no resource permission applies to the caller's own profile",
			Scope:  "caller's own profile fields",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: SessionOnly(ReasonInteractiveState),
	},

	// =====================================================================
	// Domain: role — role definition and binding reads
	// =====================================================================
	{
		ID:          "role.read",
		Domain:      "role",
		Description: "Read role definitions and the permission registry. A token needs role:read on a hub boundary; the write methods on the same routes also need their own selector",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/export", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/export", Method: "HEAD"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}/export", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}/export", Method: "HEAD"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/permissions", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestRoleReadToken_DoesNotAdmitRoleWrites"},
			{Package: "pkg/hub", Function: "TestRoleRoutesToken_WriteMethodsNeedWriteSelectors"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
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
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "role_binding.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestRoleReadToken_DoesNotAdmitRoleWrites"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "access.constraint.read",
		Domain:      "access.constraint",
		Description: "Read access constraint definitions and the principals a constraint affects. A token needs access_constraint:read on a hub boundary; the write methods on the same routes also need access_constraint:admin",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}/affected-principals", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "access_constraint.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestConstraintReadToken_DoesNotAdmitConstraintAdmin"},
			{Package: "pkg/hub", Function: "TestConstraintWriteRoutesToken_WriteMethodsNeedAdminSelector"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "hub.audit.constraint",
		Domain:      "hub",
		Description: "Read one access constraint's audit history. hub.audit.read is a hub-level permission: a token needs hub_audit:read on a hub boundary and is checked against the hub-level constraint record; a denied caller gets the same not-found answer as for a missing constraint",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}/audit", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.audit.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestConstraintAuditToken_RequiresHubAuditReadOnHubBoundary"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
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
	{
		ID:          "user.skillinjection.update",
		Domain:      "user.skillinjection",
		Description: "Add, replace or remove the skills injected into the caller's own agents. A user access token needs user_skill_injection:update on a hub boundary",
		EntryPoints: []EntryPoint{
			// DELETE is listed first: the live-inventory and bearer matrix
			// tests run entry points in this order, and the PUT probe
			// replaces the caller's list, including the seeded entry the
			// DELETE probe removes.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/injected-skills/{id}", Method: "DELETE"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/injected-skills", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/injected-skills", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		BasePermission:   "user_skill_injection.update",
		Effects:          []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "user.skillinjection.update",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"skill_injection_id"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestUserInjectedSkillsWrite_TokenNeedsUpdateScope"},
			{Package: "pkg/hub", Function: "TestUserInjectedSkillsWrite_ProjectBoundaryTokenDenied"},
			{Package: "pkg/hub", Function: "TestUserInjectedSkillsWrite_SessionAndDevUnchanged"},
			{Package: "pkg/hub", Function: "TestUserInjectedSkillsWrite_FederatedUserDenied"},
		},
		Bearer: AdmitOn(BearerTargetSelfRecord, BearerBoundaryHub),
	},
	{
		ID:          "user.skillinjection.read",
		Domain:      "user.skillinjection",
		Description: "List the skills injected into the caller's own agents. Any token of the holder reads its own entries; the entries belong to no project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/injected-skills", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestUserInjectedSkillsList_AnyTokenReadsOwnEntries"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "The path names no user; the subject is always the caller, and the list carries only the caller's own entries",
			Scope:  "caller's own user-scope injected skills",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: BearerDisposition{Kind: BearerAdmitSelf, SelfFilter: BearerSelfFilterNone, Pin: "TestUserInjectedSkillsList_AnyTokenReadsOwnEntries"},
	},
	{
		ID:          "template.user.read",
		Domain:      "template",
		Description: "List, read or download the caller's own user-scope templates. A user access token needs template:read on a hub boundary",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}/download", Method: "GET", Response: ResponseCredential},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		BasePermission:   "template.read",
		Effects:          []SecurityEffect{EffectReadOne, EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestUserTemplateWrite_RequiresTemplateSelector"},
		},
		Bearer: AdmitOn(BearerTargetCatalogRecord, BearerBoundaryHub),
	},
	{
		ID:          "template.user.create",
		Domain:      "template",
		Description: "Create a template in the caller's own user scope; the scope and owner are the caller. A user access token needs template:create on a hub boundary",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates", Method: "POST", Response: ResponseCredential},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		BasePermission:   "template.create",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestUserTemplateWrite_RequiresTemplateSelector"},
			{Package: "pkg/hub", Function: "TestUserTemplateWrites_FederatedUserRefused"},
		},
		Bearer: AdmitOn(BearerTargetCatalogRecord, BearerBoundaryHub),
	},
	{
		ID:          "template.user.update",
		Domain:      "template",
		Description: "Update, upload to or finalize one of the caller's own user-scope templates. A user access token needs template:update on a hub boundary",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}/upload", Method: "POST", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}/finalize", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		BasePermission:   "template.update",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestUserTemplateWrite_RequiresTemplateSelector"},
			{Package: "pkg/hub", Function: "TestUserTemplateWrites_FederatedUserRefused"},
		},
		Bearer: AdmitOn(BearerTargetCatalogRecord, BearerBoundaryHub),
	},
	{
		ID:          "template.user.delete",
		Domain:      "template",
		Description: "Delete one of the caller's own user-scope templates, through /api/v1/users/me/templates/{id} or /api/v1/templates/{id}. A user access token needs template:delete on a hub boundary",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}", Method: "DELETE"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}", Method: "DELETE", Variant: "scope=user"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		BasePermission:   "template.delete",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "template.delete",
			ContextFields: []string{"actor_id"},
			BeforeFields:  []string{"template_id", "template_name"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestUserTemplateWrite_RequiresTemplateSelector"},
			{Package: "pkg/hub", Function: "TestUserTemplateWrites_FederatedUserRefused"},
		},
		Bearer: AdmitOn(BearerTargetCatalogRecord, BearerBoundaryHub),
	},

	// =====================================================================
	// Domain: hub — hub audit reads; user — the caller's own explain
	// =====================================================================
	{
		ID:          "hub.audit.effectiveaccess",
		Domain:      "hub",
		Description: "Read any principal's effective access composition at the system scope. A token needs hub_audit:read on a hub boundary; HEAD answers whether the caller may read it",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/effective-access", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/effective-access", Method: "HEAD"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.audit.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestHubAuditToken_EffectiveAccessAndCrossPrincipalExplain"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "hub.audit.explain",
		Domain:      "hub",
		Description: "Explain another principal's authorization, or compare two principals' effective permissions. A token needs hub_audit:read on a hub boundary; grant details of the other principal are redacted",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/authz/explain", Method: "POST", Variant: "cross_principal"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.audit.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestHubAuditToken_EffectiveAccessAndCrossPrincipalExplain"},
		},
		Bearer: AdmitOn(BearerTargetHubInstance, BearerBoundaryHub),
	},
	{
		ID:          "user.self.explain",
		Domain:      "user",
		Description: "Explain the caller's own authorization. A token's decisions apply its ceiling and boundary, and its effective permission list holds only the permissions it may use; a project-boundary token explains only targets in its boundary project, while its holder is a current member of that project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/authz/explain", Method: "POST", Variant: "self"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestExplainSelfToken_ProjectBoundaryLimitedToItsProject"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Explains the caller's own authorization; every explained decision runs with the caller's own credential",
			Scope:  "caller's own authorization only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: BearerDisposition{Kind: BearerAdmitSelf, SelfFilter: BearerSelfFilterBoundaryProject, Pin: "TestExplainSelfToken_ProjectBoundaryLimitedToItsProject"},
	},

	// =====================================================================
	// Domain: user — the caller's own identity reads
	// =====================================================================
	{
		ID:          "user.self.profile",
		Domain:      "user",
		Description: "Read the caller's own profile and preferences. Any token reads its holder's own profile. A visitor reads its own federated account; an unlinked federated user is refused",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/me", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT, CredentialFederatedUser},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestSelfIdentityReadsToken_AnyTokenReads"},
			{Package: "pkg/hub", Function: "TestSelfReadCredential_VisitorIdentityOnly"},
			{Package: "pkg/hub", Function: "TestVisitorRoute_AuthMeReturnsVisitorOwnProfile"},
			{Package: "pkg/hub", Function: "TestVisitorRoute_UnlinkedFederatedUserUnchanged"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Reads the caller's own profile; carries no authority",
			Scope:  "caller's own profile only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: BearerDisposition{Kind: BearerAdmitSelf, SelfFilter: BearerSelfFilterNone, Pin: "TestSelfIdentityReadsToken_AnyTokenReads"},
	},
	{
		ID:          "user.self.scopes",
		Domain:      "user",
		Description: "List the token selectors and, for a requested boundary, the caller's own mint eligibility for each. Any token reads it",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/scopes", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestSelfIdentityReadsToken_AnyTokenReads"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Lists the selector catalog and the caller's own mint eligibility; carries no authority",
			Scope:  "selector catalog and caller's own eligibility only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: BearerDisposition{Kind: BearerAdmitSelf, SelfFilter: BearerSelfFilterNone, Pin: "TestSelfIdentityReadsToken_AnyTokenReads"},
	},
	{
		ID:          "user.self.adminstatus",
		Domain:      "user",
		Description: "Read the caller's own admin status and system-scope permissions. For a token the permission list holds only the permissions its ceiling allows that are eligible for its boundary kind",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/admin-status", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestAdminStatusToken_ReportsCeilingIntersection"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Reads the caller's own admin status; a token's permission list is limited to its ceiling and boundary",
			Scope:  "caller's own admin status only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: BearerDisposition{Kind: BearerAdmitSelf, SelfFilter: BearerSelfFilterNone, Pin: "TestAdminStatusToken_ReportsCeilingIntersection"},
	},
	{
		ID:          "user.self.groups",
		Domain:      "user",
		Description: "List the caller's own effective groups. A project-boundary token lists groups that belong to no project and the groups of its boundary project, the latter only while its holder is a current member of that project",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/groups", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestMyGroupsToken_ProjectBoundaryFiltersProjectGroups"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Lists the caller's own group memberships; a project token sees only its boundary project's groups and groups with no project",
			Scope:  "caller's own groups only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: BearerDisposition{Kind: BearerAdmitSelf, SelfFilter: BearerSelfFilterBoundaryProject, Pin: "TestMyGroupsToken_ProjectBoundaryFiltersProjectGroups"},
	},

	// =====================================================================
	// Domain: user — identity binding (session-only)
	// =====================================================================
	{
		ID:          "user.invite.redeem",
		Domain:      "user",
		Description: "Redeem an invitation code for the caller's identity. Session-only: a token does not bind an invitation to its holder",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/invite/redeem", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestInviteRedeem_SessionOnly"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Redeems an invitation for the caller's own identity; the handler requires an interactive session and refuses a federated caller",
			Scope:  "caller's own identity only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: SessionOnly(ReasonIdentityBinding),
	},
	{
		ID:          "user.chatlink.verify",
		Domain:      "user",
		Description: "Link a pending chat account to the caller's identity with a verification code. Session-only: a token does not bind an external account to its holder",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/telegram/link/verify", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/discord/link/verify", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/teams/link/verify", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "self-principal",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestChatLinkVerify_SessionOnly"},
		},
		Exemptions: []Exemption{{
			Kind:   ExemptionAuthenticationOnly,
			Reason: "Links an external chat account to the caller's own identity; the handler requires an interactive session and refuses a federated caller",
			Scope:  "caller's own identity only",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: SessionOnly(ReasonIdentityBinding),
	},
}
