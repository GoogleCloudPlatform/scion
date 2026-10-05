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

// projectOperations lists the catalog operations for project lifecycle, membership and schedules.
var projectOperations = []OperationSpec{
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
	// Domain: project — project read, update, and register
	// =====================================================================
	{
		ID:          "project.read",
		Domain:      "project",
		Description: "Read a single project's metadata by ID or slug",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}", Method: "GET"},
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
			// handleProjectByIDInternal (handlers_projects_core.go)
			// dispatches on r.Method: PATCH, not PUT.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}", Method: "PATCH"},
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
		Description: "Register a project from an external source",
		EntryPoints: []EntryPoint{
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/register", Method: "POST"},
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
			// handleSchedules (handlers_schedules.go) dispatches on
			// r.Method: PATCH, not PUT.
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{projectId}/schedules/{id}", Method: "PATCH"},
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
}
