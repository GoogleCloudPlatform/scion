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

// materialOperations lists the catalog operations for environment variables, secrets and GCP service accounts.
var materialOperations = []OperationSpec{
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
			// There is no standalone "/assign" HTTP route: handleGCPServiceAccountByID
			// (handlers_gcp_identity_scoped.go) only recognizes the "verify"
			// action; any other action, including "assign", returns 404.
			// The assign check (authorizeSAAssignment, ActionAssign on the
			// gcp_service_account resource) is dispatched inline, from the
			// GCPMetadataModeAssign branch of agent create and agent update,
			// when the request body sets metadata_mode: "assign".
			{Kind: EntryPointInternalDispatch, Pattern: "createAgentInProject:gcp-identity-assign"},
			{Kind: EntryPointInternalDispatch, Pattern: "applyAgentUpdate:gcp-identity-assign"},
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
			// handleSecrets (handlers_env_secrets.go) is GET-only (list);
			// create-or-update and delete are both on the by-key route.
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
