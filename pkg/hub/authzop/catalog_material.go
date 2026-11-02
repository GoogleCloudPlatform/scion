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
		Description: "Mint a GCP access or identity token for the agent's assigned service account",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agent/gcp-token", Method: "POST", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agent/gcp-identity-token", Method: "POST", Response: ResponseCredential},
		},
		Principals:       []PrincipalKind{PrincipalAgent},
		Credentials:      []CredentialKind{CredentialAgentJWT},
		ResourceResolver: "agent-gcp-service-account",
		// Both endpoints decide gcp_service_account.use on every request
		// (authorizeAgentGCPMint). gcp_service_account.mint is the
		// permission to create Hub-minted service accounts, not this.
		BasePermission: "gcp_service_account.use",
		Effects:        []SecurityEffect{EffectMintCredential},
		DelegationKind: DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Agent mints GCP tokens for its assigned service account while the principal recorded as the assignment's source holds gcp_service_account.assign on it (and passes actAs in enforce mode)",
		},
		// No before/after authority evaluation: a token mint changes no
		// authority. The per-request authority check is the use decision
		// named by BasePermission.
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
		Description: "Read project or broker secrets (scope=project or runtime_broker). User and hub scope are the session-only operations secret.user.read and secret.hub.read",
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
		Description: "Create, update or delete project or broker secrets (scope=project or runtime_broker). User and hub scope are the session-only operations secret.user.write and secret.hub.write",
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
		Description: "Read GCP service account details or list accounts. A project's list requires project.read on it (members, and agents of that project only); the hub-scoped list requires gcp_service_account.list at hub scope (hub members)",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts/{id}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
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
		Description: "Read project or broker environment variables (scope=project or runtime_broker). User and hub scope are the session-only operations env.user.read, env.hub.read and env.hub.list",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env", Method: "GET", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "GET", Response: ResponseCredential},
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
	{
		ID:          "env.hub.list",
		Domain:      "env",
		Description: "List hub-level environment variables (scope=hub). Session only: an unscoped admin lists every entry, and a session holder of hub.env_vars.read lists them without secret entries; every user access token is refused",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env", Method: "GET", Variant: "scope=hub", Response: ResponseCredential},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "hub-scoped",
		BasePermission:   "hub.env_vars.read",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestHubEnvSecret_ProjectBoundaryTokenRefused"},
			{Package: "pkg/hub", Function: "TestHubEnvSecret_HubBoundaryTokenWithoutHubMaterialScopeRefused"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},

	// =====================================================================
	// User- and hub-level env and secret operations (session only). They
	// share routes with the project and broker operations above and are
	// told apart by the scope query parameter or body field.
	// =====================================================================
	envSecretSessionOnlyOp("env.user.read", "env", "Read the caller's own user-level environment variables (scope=user, the default). A federated caller reads its own (retained prior behaviour)",
		[]EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env", Method: "GET", Variant: "scope=user", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "GET", Variant: "scope=user", Response: ResponseCredential},
		}, []SecurityEffect{EffectReadOne, EffectListScoped}, false),
	envSecretSessionOnlyOp("env.user.write", "env", "Create, update or delete the caller's own user-level environment variables (scope=user, the default). A federated caller is refused (requireProfileWriter)",
		[]EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "PUT", Variant: "scope=user", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "DELETE", Variant: "scope=user"},
		}, []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource}, false),
	envSecretSessionOnlyOp("secret.user.read", "secret", "Read the caller's own user-level secrets (scope=user, the default). A federated caller reads its own (retained prior behaviour)",
		[]EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets", Method: "GET", Variant: "scope=user"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "GET", Variant: "scope=user"},
		}, []SecurityEffect{EffectReadSecret}, false),
	envSecretSessionOnlyOp("secret.user.write", "secret", "Create, update or delete the caller's own user-level secrets (scope=user, the default). A federated caller is refused (requireProfileWriter)",
		[]EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "PUT", Variant: "scope=user"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "PATCH", Variant: "scope=user"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "DELETE", Variant: "scope=user"},
		}, []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource}, false),
	envSecretSessionOnlyOp("env.hub.read", "env", "Read one hub-level environment variable (scope=hub). Agents may read; a user must be an unscoped admin",
		[]EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "GET", Variant: "scope=hub", Response: ResponseCredential},
		}, []SecurityEffect{EffectReadOne}, true),
	envSecretSessionOnlyOp("env.hub.write", "env", "Create, update or delete hub-level environment variables (scope=hub). A user must be an unscoped admin",
		[]EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "PUT", Variant: "scope=hub", Response: ResponseCredential},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "DELETE", Variant: "scope=hub"},
		}, []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource}, true),
	envSecretSessionOnlyOp("secret.hub.read", "secret", "Read hub-level secrets (scope=hub). Agents may read; a user must be an unscoped admin",
		[]EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets", Method: "GET", Variant: "scope=hub"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "GET", Variant: "scope=hub"},
		}, []SecurityEffect{EffectReadSecret}, true),
	envSecretSessionOnlyOp("secret.hub.write", "secret", "Create, update or delete hub-level secrets (scope=hub). A user must be an unscoped admin",
		[]EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "PUT", Variant: "scope=hub"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "PATCH", Variant: "scope=hub"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "DELETE", Variant: "scope=hub"},
		}, []SecurityEffect{EffectCreateResource, EffectUpdateResource, EffectDeleteResource}, true),
}

// envSecretSessionOnlyOp builds a session-only user- or hub-level env or
// secret operation. Every user access token is refused with
// CREDENTIAL_MANAGEMENT (handlers_env_secrets.go, resolveEnvSecretAccess and
// authorizeHubEnvVarList). User scope is the caller's own record; hub scope
// needs an unscoped local platform admin; agents may also read hub-level
// material (agentRead), as resolveEnvSecretAccess allows.
func envSecretSessionOnlyOp(id OperationID, domain, description string, eps []EntryPoint, effects []SecurityEffect, hub bool) OperationSpec {
	resolver := "self-principal"
	exemption := Exemption{
		Kind:   ExemptionAuthenticationOnly,
		Reason: "User-level env and secret material is the caller's own record, so no resource permission applies; a session credential is required",
		Scope:  "caller's own user-level environment variables and secrets",
		Waives: []WaivedObligation{WaiveBasePermission},
	}
	if hub {
		resolver = "hub-scoped"
		exemption = Exemption{
			Kind:   ExemptionHubAdmin,
			Reason: "Unscoped local platform admin on a session credential (IsUnscopedLocalPlatformAdmin); no registered permission covers hub-level env and secret material",
			Scope:  "hub-level environment variables and secrets",
			Waives: []WaivedObligation{WaiveBasePermission},
		}
	}
	refs := []TestRef{
		{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
		{Package: "pkg/hub", Function: "TestUserEnvSecret_ProjectBoundaryTokenRefused"},
		{Package: "pkg/hub", Function: "TestUserEnvSecretWrites_SessionAllowedTokenRefused"},
		{Package: "pkg/hub", Function: "TestUserEnvSecretWrites_FederatedUserRefused"},
	}
	if hub {
		refs = []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestHubEnvSecret_ProjectBoundaryTokenRefused"},
			{Package: "pkg/hub", Function: "TestHubEnvSecret_HubBoundaryTokenWithoutHubMaterialScopeRefused"},
			{Package: "pkg/hub", Function: "TestHubEnvSecret_SessionAdminAllowed"},
			{Package: "pkg/hub", Function: "TestHubEnvSecret_NonAdminSessionRefused"},
			{Package: "pkg/hub", Function: "TestHubEnvSecret_TestAdminSessionRefused"},
		}
	}
	// Writes and secret reads carry an audit obligation, as the project
	// operations secret.read and secret.write do.
	var audit *AuditObligation
	for _, eff := range effects {
		if !eff.RequiresAudit() {
			continue
		}
		audit = &AuditObligation{EventType: domain + ".write", ContextFields: []string{"actor_id"}, Atomic: true}
		if eff == EffectReadSecret {
			audit.EventType = domain + ".read"
		} else {
			audit.BeforeFields = []string{domain + "_key"}
		}
		break
	}
	principals := []PrincipalKind{PrincipalUser}
	credentials := []CredentialKind{CredentialSessionJWT}
	if hub && allGET(eps) {
		// Agents read hub-level material with their agent token.
		principals = append(principals, PrincipalAgent)
		credentials = append(credentials, CredentialAgentJWT)
	}
	if !hub && allGET(eps) {
		// A federated caller reads its own user-level material (retained
		// prior behaviour; writes are refused by requireProfileWriter).
		credentials = append(credentials, CredentialFederatedUser)
		refs = append(refs, TestRef{Package: "pkg/hub", Function: "TestUserEnvSecretReads_FederatedOwnScopeAllowed"})
	}
	return OperationSpec{
		ID:               id,
		Domain:           domain,
		Description:      description,
		EntryPoints:      eps,
		Principals:       principals,
		Credentials:      credentials,
		ResourceResolver: resolver,
		Effects:          effects,
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         refs,
		AuditObligation:  audit,
		Exemptions:       []Exemption{exemption},
		Bearer:           SessionOnly(ReasonCredentialManagement),
	}
}

// allGET reports whether every entry point is an HTTP GET.
func allGET(eps []EntryPoint) bool {
	for _, ep := range eps {
		if ep.Method != "GET" {
			return false
		}
	}
	return len(eps) > 0
}
