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

// testIdentityOperations are the hub test identity operations
// (ptone/scion#4240). The routes answer 404 unless the hub runs with
// --test-mode=member or above. Each requires test_identity.issue at hub scope
// and admits a hub-boundary user access token carrying test_identity:issue,
// and a workload-bound issuer delegation (phase X2), which acts for its
// human grantor within its single scope and manages only the identities
// issued through it. The delegation operations themselves (create, list,
// revoke) admit an interactive session only.
var testIdentityOperations = []OperationSpec{
	{
		ID:          "testidentity.create",
		Domain:      "testidentity",
		Description: "Issue a short-lived synthetic member or viewer test identity and one access token for it (no refresh token, no cookie)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identities", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialWorkloadDelegation},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue",
		Effects:          []SecurityEffect{EffectCreateResource, EffectMintCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "The role is member or viewer only; live identities are capped per issuer and per hub, and issuance is rate limited per issuer",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test_identity_issue",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			AfterFields:   []string{"user_id", "role", "issued_by", "purpose", "expires_at", "token_ttl_seconds", "workload_delegation"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestIdentity_IssuerAuthorization"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
	{
		ID:          "testidentity.list",
		Domain:      "testidentity",
		Description: "List test identities: the caller's own, or every identity for an unscoped platform admin session",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identities", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialWorkloadDelegation},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestTestIdentity_ListIsolation"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
	{
		ID:          "testidentity.token.issue",
		Domain:      "testidentity",
		Description: "Re-issue one access token for a live test identity, for its issuer or an unscoped platform admin session",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identities/{id}/token", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialWorkloadDelegation},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue",
		Effects:          []SecurityEffect{EffectMintCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Only the identity's issuer (or an unscoped platform admin session) re-issues; the token never outlives the identity",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test_identity_token_issue",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			AfterFields:   []string{"user_id", "role", "issued_by", "token_ttl_seconds", "workload_delegation"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound, DenialConflict},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestIdentity_TokenReissue"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
	{
		ID:          "testidentity.webcode.issue",
		Domain:      "testidentity",
		Description: "Mint a single-use, five-minute web-login code for a live test identity (kind test_fixture), for its issuer or an unscoped platform admin session; only on hubs where test identity web sessions are available",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identities/{id}/web-login-code", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue",
		Effects:          []SecurityEffect{EffectMintCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Only the identity's issuer (or an unscoped platform admin session) mints a code; at most one unredeemed code per identity, redeemable once, never past the identity's expiry",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test_identity_web_code_issue",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			AfterFields:   []string{"user_id", "code_id", "code_expires_at", "issued_by"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound, DenialConflict},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestIdentityWeb_Mint"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
	{
		ID:          "testidentity.delete",
		Domain:      "testidentity",
		Description: "Delete a test identity (its role bindings, group memberships and user-scope data go with it), for its issuer, an unscoped platform admin session, or a holder of test_identity.issue and user.delete; refused with 409 while it owns agents or is a project's last owner. With ?purge=true it also removes the agents, schedules, groups and projects stamped as created under the identity (the identity ID is the only input), keeps stamped projects that have another owner or other resources, and reports unstamped agents as unproven",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identities/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialWorkloadDelegation},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue",
		Effects:          []SecurityEffect{EffectDeleteResource},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Only a kind=test_fixture user is deleted; any other user ID answers 404, even for an admin (with ?purge=true: the same 200 empty report for any ID the caller may not purge)",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test_identity_delete",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			BeforeFields:  []string{"user_id", "role", "issued_by", "purpose", "expires_at"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound, DenialConflict},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestIdentity_Delete"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
	{
		ID:          "testidentity.admin.create",
		Domain:      "testidentity",
		Description: "Issue a short-lived test identity holding an expiring hub-admin role (on a hub started with --test-mode=admin or above) or super-admin role (--test-mode=super-admin), and one access token for it (no refresh token, no cookie)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identities/admin", Method: "POST"},
		},
		Principals:            []PrincipalKind{PrincipalUser},
		Credentials:           []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
		ResourceResolver:      "hub-scoped",
		BasePermission:        "test_identity.issue_admin",
		Effects:               []SecurityEffect{EffectCreateResource, EffectGrantAuthority, EffectMintCredential},
		DelegationKind:        DelegationNonAmplification,
		DelegationDescription: "The issued hub-admin or super-admin authority never exceeds the issuer's: the issuer must be a live super-admin through its own current bindings (canIssueTestAdmin), and an access token issuer's frozen ceiling must carry the requested kind's capability. The binding expires with the identity",
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "The issuer is a human, live super-admin through its own current bindings; an access token issuer presents a hub-boundary token with exactly the requested kind's scope (test_identity:issue_admin or test_identity:issue_super_admin), at most 8 h old, loaded and checked on every issuance. One live admin-kind identity per issuer, two per hub of which at most one test super-admin; issuance is rate limited per issuer (6/h hub-admin, 2/h super-admin)",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test_admin_issue",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			AfterFields:   []string{"user_id", "kind", "role", "issued_by", "issuer_credential_id", "issuance_id", "purpose", "expires_at", "token_ttl_seconds"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestAdmin_IssuanceMatrix"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
	{
		ID:          "testidentity.admin.revokeall",
		Domain:      "testidentity",
		Description: "Revoke every admin-kind test identity: delete each (or, when the user delete refuses it, suspend it and remove its role bindings), for a human super-admin session",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identities/admin/revoke-all", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue_admin",
		Effects:          []SecurityEffect{EffectDeleteResource, EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Only a human caller holding an active super-admin binding, on an interactive session; only admin-kind rows are touched",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test_admin_revoke",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			BeforeFields:  []string{"user_id", "role", "issued_by", "purpose", "expires_at"},
			AfterFields:   []string{"outcome"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestAdmin_RevokeAll"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
	{
		ID:          "testidentity.delegation.create",
		Domain:      "testidentity",
		Description: "Delegate member-tier test identity issuance to one pinned workload identity (a subject in the server.hub.test_identities.workload_issuers allowlist) with one scope, a member cap and a lifetime of at most 7 days",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identity-delegations", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue",
		Effects:          []SecurityEffect{EffectIssueCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Only a live human user's interactive session may delegate; the scope is exactly test_identity:issue, the subject must be allow-listed for its tier, and every mint re-checks the grantor's live bindings, the delegation and the allowlist",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test_identity_delegation_create",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			AfterFields:   []string{"delegation_id", "grantor", "workload_sub", "scope", "member_cap", "label", "expires_at"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound, "validation_error"},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestIdentityDelegation_CreateAuthorization"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
	{
		ID:          "testidentity.delegation.list",
		Domain:      "testidentity",
		Description: "List test identity issuer delegations: the caller's own, or every delegation for an unscoped platform admin session",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identity-delegations", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub", Function: "TestTestIdentityDelegation_ListAndRevoke"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
	{
		ID:          "testidentity.delegation.revoke",
		Domain:      "testidentity",
		Description: "Revoke a test identity issuer delegation, for its grantor or an unscoped platform admin session; the next mint through it is refused",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identity-delegations/{id}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "test_identity.issue",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Only the grantor or an unscoped platform admin session revokes; any other caller gets 404",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "test_identity_delegation_revoke",
			ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
			BeforeFields:  []string{"delegation_id", "grantor", "workload_sub", "scope"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestIdentityDelegation_ListAndRevoke"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
}
