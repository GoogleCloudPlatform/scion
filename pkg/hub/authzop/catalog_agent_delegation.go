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

// agentDelegationOperations are the agent delegation operations
// (.design/agent-delegation.md §18.3): grant issuance by an interactive
// user and the bound agent's exchange of a grant for a delegated
// credential, both behind the hub.agent_delegation experiment; and grant
// list, read and revoke and delegated credential revoke (§7), which answer
// while the experiment is off.
var agentDelegationOperations = []OperationSpec{
	{
		ID:          "agent.delegation.create",
		Domain:      "agent.delegation",
		Description: "Issue an agent delegation grant binding one agent the issuer controls to a boundary, a frozen permission ceiling and an expiry",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/delegations", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser},
		Credentials:      []CredentialKind{CredentialSessionJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.delegation.create",
		Effects:          []SecurityEffect{EffectIssueCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Only the agent's owner or recorded ancestor, admitted to the agent's project, may issue; the ceiling is limited to selectors the issuer may mint and to the hub agent-delegation policy",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "agent_delegation_grant_create",
			ContextFields: []string{"actor_id", "agent_id"},
			AfterFields:   []string{"grant_id", "boundary_kind", "permissions", "expires_at"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{
			DenialForbidden, DenialScopeViolation, "credential_not_admitted", "reserved_identity",
			"issuer_not_controller", "issuer_project_access", "permission_not_delegable",
			"agent_not_found", "agent_not_eligible", "agent_reincarnating", "subdelegation_not_supported", "grant_limit_reached", "audit_failed",
		},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestAgentDelegationIssuance_IssuesGrantWithAudit"},
			{Package: "pkg/hub", Function: "TestAgentDelegationIssuance_RefusesNonSessionCredentials"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
	{
		ID:          "agent.delegation.exchange",
		Domain:      "agent.delegation",
		Description: "Exchange an agent delegation grant, with the bound agent's own agent token, for a short-lived opaque delegated credential",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/delegations/{grantId}/exchange", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalAgent},
		Credentials:      []CredentialKind{CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.delegation.exchange",
		Effects:          []SecurityEffect{EffectMintCredential},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: "Only the bound agent, whose agent credential row is live, may exchange; the issuer, grant, agent and policy are re-checked, and the credential never exceeds the grant ceiling, the grant expiry or the agent credential's expiry",
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "agent_delegation_credential_issue",
			ContextFields: []string{"actor_id", "grant_id"},
			AfterFields:   []string{"credential_id", "permissions", "expires_at"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{
			"credential_not_admitted", "agent_credential_invalid", "grant_not_found", "grant_agent_changed",
			"issuer_not_controller", "issuer_project_access", "grant_inactive", "issuer_invalid",
			"permission_not_delegable", "invalid_audience", "outside_ceiling", "audit_failed",
		},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestAgentDelegationExchange_IssuesCredentialWithAudit"},
			{Package: "pkg/hub", Function: "TestAgentDelegationExchange_RefusesUATAndSession"},
		},
		Bearer: BearerDisposition{Kind: BearerNonUser, Pin: "TestAgentDelegationExchange_RefusesUATAndSession"},
	},
}

// agentDelegationManagementControl describes who may act on a grant
// through the management operations (.design/agent-delegation.md §7).
const agentDelegationManagementControl = "Per grant: the issuer; the bound agent with its own agent token (its credential row live and unexpired, the path agent its subject); the agent's current owner; holders of the operation's permission on the hub (hub admins), for any grant; holders of it on project P (project owners and admins), for project(P)-bounded grants only. A dev session counts only as hub authority. Delegated credentials and user access tokens are refused"

// agentDelegationManagementDenials are the denial codes of the management
// operations (.design/agent-delegation.md §7, §8.4).
var agentDelegationManagementDenials = []DenialCode{
	DenialForbidden, "credential_not_admitted", "agent_credential_invalid", "grant_not_found",
}

// agentDelegationCredentialRevokeDenials add the answer for a credential
// that does not exist or belongs to another grant.
var agentDelegationCredentialRevokeDenials = append(append([]DenialCode(nil), agentDelegationManagementDenials...), "not_found", "audit_failed")

var agentDelegationManagementOperations = []OperationSpec{
	{
		ID:          "agent.delegation.revoke",
		Domain:      "agent.delegation",
		Description: "Revoke an agent delegation grant and every credential exchanged from it",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/delegations/{grantId}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.delegation.revoke",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: agentDelegationManagementControl,
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "agent_delegation_grant_revoke",
			ContextFields: []string{"actor_id", "grant_id"},
			BeforeFields:  []string{"grant_id", "revoked_credential_ids"},
			Atomic:        true,
		},
		DenialCodes: append(append([]DenialCode(nil), agentDelegationManagementDenials...), "audit_failed"),
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestAgentDelegationRevoke_CascadesWithOneAuditRow"},
			{Package: "pkg/hub", Function: "TestAgentDelegationManagement_ControlAuthority"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
	{
		ID:          "agent.delegation.credential.revoke",
		Domain:      "agent.delegation",
		Description: "Revoke one delegated credential exchanged from an agent delegation grant",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/delegations/{grantId}/credentials/{credentialId}", Method: "DELETE"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.delegation.credential.revoke",
		Effects:          []SecurityEffect{EffectRevokeAuthority},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: agentDelegationManagementControl,
		},
		AuthorityEval: AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "agent_delegation_credential_revoke",
			ContextFields: []string{"actor_id", "grant_id"},
			BeforeFields:  []string{"credential_id", "revoke_reason"},
			Atomic:        true,
		},
		DenialCodes: agentDelegationCredentialRevokeDenials,
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestAgentDelegationCredentialRevoke_RevokesOneWithAudit"},
			{Package: "pkg/hub", Function: "TestAgentDelegationManagement_ControlAuthority"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
	{
		ID:          "agent.delegation.list",
		Domain:      "agent.delegation",
		Description: "List agent delegation grants: those bound to one agent, or those the caller issued; grants outside the caller's authority are left out",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/delegations", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/delegations", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.delegation.list",
		Effects:          []SecurityEffect{EffectListScoped},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: agentDelegationManagementControl + ". The caller's own issued grants on /users/me/delegations, session only",
		},
		AuthorityEval: AuthorityEvalNone,
		DenialCodes:   append([]DenialCode(nil), agentDelegationManagementDenials...),
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestAgentDelegationList_LeavesOutGrantsOutsideAuthority"},
			{Package: "pkg/hub", Function: "TestAgentDelegationManagement_NoSecretFieldsInResponses"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
	{
		ID:          "agent.delegation.read",
		Domain:      "agent.delegation",
		Description: "Read one agent delegation grant; a grant outside the caller's authority answers as missing",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/delegations/{grantId}", Method: "GET"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.delegation.read",
		Effects:          []SecurityEffect{EffectReadOne},
		DelegationKind:   DelegationNone,
		Governance: &GovernancePolicy{
			Kind:        GovernanceIssuerCredential,
			Description: agentDelegationManagementControl,
		},
		AuthorityEval: AuthorityEvalNone,
		DenialCodes:   append([]DenialCode(nil), agentDelegationManagementDenials...),
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestAgentDelegationManagement_ControlAuthority"},
			{Package: "pkg/hub", Function: "TestAgentDelegationManagement_NoSecretFieldsInResponses"},
		},
		Bearer: SessionOnly(ReasonCredentialManagement),
	},
}
