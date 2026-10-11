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
// --enable-test-identities. Each requires test_identity.issue at hub scope
// and admits a hub-boundary user access token carrying test_identity:issue.
var testIdentityOperations = []OperationSpec{
	{
		ID:          "testidentity.create",
		Domain:      "testidentity",
		Description: "Issue a short-lived synthetic member or viewer test identity and one access token for it (no refresh token, no cookie)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/test-identities", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
			AfterFields:   []string{"user_id", "role", "issued_by", "purpose", "expires_at", "token_ttl_seconds"},
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
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT},
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
			AfterFields:   []string{"user_id", "role", "issued_by", "token_ttl_seconds"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden, DenialResourceNotFound, DenialConflict},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestTestIdentity_TokenReissue"},
		},
		Bearer: AdmitOn(BearerTargetHubCollection, BearerBoundaryHub),
	},
}
