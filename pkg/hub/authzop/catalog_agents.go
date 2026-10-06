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

// agentOperations lists the catalog operations for agent lifecycle, read, update and special operations.
var agentOperations = []OperationSpec{
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
		DelegationKind:   DelegationNonAmplification,
		DelegationDescription: "Actor must hold the role and scopes delegated to the new agent (CanDelegate non-amplification); " +
			"an agent actor is also evaluated against the delegation ceiling of its live delegation chain for agent.create on the target project",
		AuthorityEval: AuthorityEvalNone,
		// conflict: the user or agent the agent belongs to no longer exists
		// (deleted while the create ran), or its slug's identity key is taken.
		DenialCodes: []DenialCode{DenialForbidden, DenialConflict},
		TestRefs: []TestRef{
			{Package: "pkg/hub/authzop", Function: "TestCatalogValidation"},
			{Package: "pkg/hub", Function: "TestAgentCreate_ExplicitRoleAboveParentDenied"},
			{Package: "pkg/hub", Function: "TestAgentCreate_RequiresLiveDelegator"},
		},
		Bearer: AdmitOn(BearerTargetProjectBody, BearerBoundaryProject, BearerBoundaryHub),
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
		Bearer:      AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
	},

	// Agent actions on the by-id and project alias forms. Every entry point
	// resolves through ResolveAgentSubRoute (pkg/hub/agent_routes.go);
	// TestAgentSubRoute_CatalogDrift checks these against that table.
	{
		ID:          "agent.lifecycle.control",
		Domain:      "agent",
		Description: "Start, stop, suspend or restart an agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/start", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/stop", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/suspend", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/restart", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/start", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/stop", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/suspend", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/restart", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.lifecycle",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestAgentSubRoute_CatalogDrift"}},
		Bearer:           AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "agent.lifecycle.restore",
		Domain:      "agent",
		Description: "Restore a soft-deleted agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/restore", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/restore", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.lifecycle",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		// conflict: the user or agent the agent belongs to no longer exists, its
		// identity key is now taken by another agent, a delegator of its
		// restored edges is not live, or one of those edges conflicts.
		DenialCodes: []DenialCode{DenialForbidden, DenialConflict},
		TestRefs:    []TestRef{{Package: "pkg/hub", Function: "TestAgentSubRoute_CatalogDrift"}},
		Bearer:      AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "agent.lifecycle.exec",
		Domain:      "agent",
		Description: "Run a command in an agent's container",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/exec", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/exec", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.attach",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestAgentSubRoute_CatalogDrift"}},
		Bearer:           AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "agent.lifecycle.env",
		Domain:      "agent",
		Description: "Submit environment values to an agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/env", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/env", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.attach",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestAgentSubRoute_CatalogDrift"}},
		Bearer:           AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "agent.lifecycle.resetauth",
		Domain:      "agent",
		Description: "Reset an agent's harness authentication",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/reset-auth", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/reset-auth", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.attach",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestAgentSubRoute_CatalogDrift"}},
		Bearer:           AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "agent.lifecycle.reincarnate",
		Domain:      "agent",
		Description: "Reincarnate an agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/reincarnate", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/reincarnate", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
		ResourceResolver: "agent-from-url",
		BasePermission:   "agent.lifecycle",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestAgentSubRoute_CatalogDrift"}},
		Bearer:           AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
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
		Bearer:           AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
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
			// handleAgentByID (handlers_agents_core.go) dispatches the
			// no-action, no-sub-resource case on r.Method: PATCH, not PUT.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}", Method: "PATCH"},
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
			// The live route dispatches on /pty (pkg/hub/pty_handlers.go
			// handleAgentPTY, invoked from handlers_agents_core.go's
			// action == "pty" branch), not /attach.
			// TestAgentAttachCatalogMatchesRoute (authzop/drift_test.go)
			// pins this entry point to the live route.
			{Kind: EntryPointWebSocket, Pattern: "/api/v1/agents/{id}/pty", Method: "GET"},
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
		Bearer:           AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub),
	},
	{
		ID:          "agent.portaccess",
		Domain:      "agent",
		Description: "Access forwarded ports on an agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports", Method: "GET"},
			// port_forward_handlers.go's proxyAgentPort (invoked from
			// handleAgentPorts for the "{port}/proxy" and "{port}/proxy/*"
			// suffixes) authorizes via authorizePortAccess for EVERY HTTP
			// method and any subpath after "/proxy" — there is no
			// method-based routing before that authorization check. Entry
			// points are representative, not exhaustive: the schema has no
			// wildcard method or pattern, and this route accepts every
			// HTTP method on ".../proxy" and any subpath. GET/POST/PUT/
			// DELETE and one subpath are listed as representatives.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy", Method: "DELETE"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}", Method: "GET"},
		},
		// authorizePortAccess (port_forward_handlers.go) also admits an agent
		// identity directly, without calling CheckAccess, when the agent
		// matches the target agent's own ID/project (self-access) — a second
		// principal/credential this route genuinely accepts, distinct from
		// the CheckAccess-gated user path BasePermission describes.
		Principals:       []PrincipalKind{PrincipalUser, PrincipalAgent},
		Credentials:      []CredentialKind{CredentialSessionJWT, CredentialScopedUAT, CredentialAgentJWT},
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
			// The set_message_mode action is dispatched through the
			// generic, POST-only agent-action gate (handleAgentAction,
			// handlers_agents_core.go), not a PUT on a dedicated
			// "message-mode" sub-resource. Both the agent-scoped and
			// project-scoped forms reach handleSetMessageMode.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/set_message_mode", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/set_message_mode", Method: "POST"},
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
	{
		ID:          "agent.token.refresh",
		Domain:      "agent",
		Description: "Refresh the calling agent's own hub token",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/token/refresh", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/refresh-token", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalAgent},
		Credentials:      []CredentialKind{CredentialAgentJWT},
		ResourceResolver: "agent-self",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "The agent authenticates with its own agent JWT for its own record; no user permission applies",
			Scope:  "agent self access",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: NonUser(),
	},
	{
		ID:          "agent.outbound.message",
		Domain:      "agent",
		Description: "Deliver an outbound message from the calling agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/outbound-message", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/agents/{id}/outbound-message", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalAgent},
		Credentials:      []CredentialKind{CredentialAgentJWT},
		ResourceResolver: "agent-self",
		Effects:          []SecurityEffect{EffectCreateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "The agent authenticates with its own agent JWT for its own record; no user permission applies",
			Scope:  "agent self access",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: NonUser(),
	},
	{
		ID:          "agent.metrics.report",
		Domain:      "agent",
		Description: "Report runtime metrics for the calling agent",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/metrics", Method: "POST"},
		},
		Principals:       []PrincipalKind{PrincipalAgent},
		Credentials:      []CredentialKind{CredentialAgentJWT},
		ResourceResolver: "agent-self",
		Effects:          []SecurityEffect{EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		DenialCodes:      []DenialCode{DenialForbidden},
		TestRefs:         []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "The agent authenticates with its own agent JWT for its own record; no user permission applies",
			Scope:  "agent self access",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: NonUser(),
	},
	{
		ID:          "agent.secrets.access",
		Domain:      "agent",
		Description: "List, read and write the secrets available to the calling agent: its project's and its creating user's",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/secrets", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/secrets/{key}", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/secrets/{key}", Method: "PUT"},
		},
		Principals:       []PrincipalKind{PrincipalAgent},
		Credentials:      []CredentialKind{CredentialAgentJWT},
		ResourceResolver: "agent-self",
		Effects:          []SecurityEffect{EffectReadSecret, EffectUpdateResource},
		DelegationKind:   DelegationNone,
		AuthorityEval:    AuthorityEvalNone,
		AuditObligation: &AuditObligation{
			EventType:     "agent.secrets.access",
			ContextFields: []string{"actor_id", "project_id", "scope"},
			BeforeFields:  []string{"secret_key"},
			Atomic:        true,
		},
		DenialCodes: []DenialCode{DenialForbidden},
		TestRefs:    []TestRef{{Package: "pkg/hub", Function: "TestBearerDisposition_EveryRoutePatternCovered"}},
		Exemptions: []Exemption{{
			Kind:   ExemptionInternalOnly,
			Reason: "The agent authenticates with its own agent JWT, whose subject must match the agent ID in the path; no user permission applies; the project and user scope IDs come from the token",
			Scope:  "agent self access",
			Waives: []WaivedObligation{WaiveBasePermission},
		}},
		Bearer: NonUser(),
	},
}
