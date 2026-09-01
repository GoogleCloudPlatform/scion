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

// OperationSpec declares the complete authorization contract for one
// security-meaningful operation. Every externally reachable entry point
// and every security-relevant mutation must map to exactly one
// OperationSpec or a documented exemption.
//
// Validation is deterministic and fail-closed: a spec that fails
// Validate() must not be accepted into a catalog or enforcement path.
type OperationSpec struct {
	// ID is a stable, unique operation identifier. Format: domain.verb
	// (e.g., "project.membership.add", "agent.create", "secret.read").
	// Once assigned, an ID must not change or be reused.
	ID OperationID

	// Domain identifies the product domain that owns this operation
	// (e.g., "project.membership", "agent", "secret", "constraint").
	Domain string

	// Description is a human-readable summary of the operation's purpose.
	Description string

	// EntryPoints enumerates every externally reachable path that
	// dispatches this operation: HTTP method+route, broker call,
	// scheduler callback, CLI command, background job, or internal
	// dispatcher.
	EntryPoints []EntryPoint

	// Principals lists the authenticated principal and credential kinds
	// admitted to attempt this operation.
	Principals []PrincipalKind

	// ResourceResolver names the authoritative resource and scope
	// resolver for this operation (e.g., "project-from-url",
	// "agent-owner-project"). The resolver determines which project
	// or system scope the base permission is evaluated against.
	ResourceResolver string

	// BasePermission is the canonical permission ID required for this
	// operation (e.g., "project.manage", "agent.create", "secret.read").
	BasePermission string

	// Effects enumerates the typed security effects this operation may
	// produce. Effects select additional checks and invariants beyond
	// the base permission.
	Effects []SecurityEffect

	// Delegation specifies delegation requirements for
	// authority-increasing effects. Nil means no delegation check
	// is required.
	Delegation *DelegationPolicy

	// Governance specifies target-governance rules for peer/superior
	// or protected-principal changes. Nil means no governance check
	// is required.
	Governance *GovernancePolicy

	// Invariants lists post-state invariants that must hold after the
	// operation commits. Each invariant is evaluated against the
	// proposed post-state within the same transaction.
	Invariants []Invariant

	// AuditObligation specifies the audit event type and required fields.
	// Nil means no audit record is required (must be justified via an
	// exemption).
	AuditObligation *AuditObligation

	// DenialCodes lists the stable public denial codes this operation
	// may return. These are product-level reasons, not internal evaluator
	// details.
	DenialCodes []DenialCode

	// TestRefs lists the executable tests that prove this contract. Each
	// reference is a package path + test function name or pattern.
	TestRefs []TestRef

	// Exemptions documents explicit exemptions from normal contract
	// requirements, such as offline recovery, deterministic seeding,
	// migrations, or test fixture construction.
	Exemptions []Exemption
}

// OperationID is a stable, unique operation identifier.
type OperationID string

// EntryPoint describes one externally reachable path that dispatches an
// operation.
type EntryPoint struct {
	// Kind classifies the entry point type.
	Kind EntryPointKind

	// Pattern is the entry-point-specific identifier: an HTTP route
	// pattern, broker call name, scheduler callback, CLI command, etc.
	Pattern string

	// Method is the HTTP method for HTTPRoute entry points. Empty for
	// non-HTTP entry points.
	Method string
}

// EntryPointKind classifies entry point types.
type EntryPointKind string

const (
	EntryPointHTTPRoute        EntryPointKind = "http_route"
	EntryPointBrokerCall       EntryPointKind = "broker_call"
	EntryPointSchedulerJob     EntryPointKind = "scheduler_job"
	EntryPointCLICommand       EntryPointKind = "cli_command"
	EntryPointBackgroundJob    EntryPointKind = "background_job"
	EntryPointInternalDispatch EntryPointKind = "internal_dispatch"
)

// validEntryPointKinds is the closed set of recognized entry point kinds.
var validEntryPointKinds = map[EntryPointKind]bool{
	EntryPointHTTPRoute:        true,
	EntryPointBrokerCall:       true,
	EntryPointSchedulerJob:     true,
	EntryPointCLICommand:       true,
	EntryPointBackgroundJob:    true,
	EntryPointInternalDispatch: true,
}

// PrincipalKind identifies a class of authenticated principal or credential.
type PrincipalKind string

const (
	PrincipalUser           PrincipalKind = "user"
	PrincipalAgent          PrincipalKind = "agent"
	PrincipalScopedUAT      PrincipalKind = "scoped_uat"
	PrincipalBroker         PrincipalKind = "broker"
	PrincipalServiceAccount PrincipalKind = "service_account"
	PrincipalSystem         PrincipalKind = "system"
)

// validPrincipalKinds is the closed set of recognized principal kinds.
var validPrincipalKinds = map[PrincipalKind]bool{
	PrincipalUser:           true,
	PrincipalAgent:          true,
	PrincipalScopedUAT:      true,
	PrincipalBroker:         true,
	PrincipalServiceAccount: true,
	PrincipalSystem:         true,
}

// SecurityEffect classifies the security-meaningful consequence of an
// operation. Effects do not replace permissions; they select additional
// checks and invariants an otherwise authorized operation must satisfy.
type SecurityEffect string

const (
	EffectReadOne               SecurityEffect = "read-one"
	EffectListScoped            SecurityEffect = "list-scoped"
	EffectCreateResource        SecurityEffect = "create-resource"
	EffectUpdateResource        SecurityEffect = "update-resource"
	EffectDeleteResource        SecurityEffect = "delete-resource"
	EffectGrantAuthority        SecurityEffect = "grant-authority"
	EffectChangeAuthority       SecurityEffect = "change-authority"
	EffectRevokeAuthority       SecurityEffect = "revoke-authority"
	EffectTightenBoundary       SecurityEffect = "tighten-boundary"
	EffectRelaxBoundary         SecurityEffect = "relax-boundary"
	EffectChangePrincipalStatus SecurityEffect = "change-principal-status"
	EffectIssueCredential       SecurityEffect = "issue-credential"
	EffectReadSecret            SecurityEffect = "read-secret"
	EffectMintCredential        SecurityEffect = "mint-credential"
	EffectAssignCredential      SecurityEffect = "assign-credential"
	EffectEmitExternal          SecurityEffect = "emit-external-effect"
	EffectChangeOwnership       SecurityEffect = "change-ownership"
)

// validSecurityEffects is the closed set of recognized security effects.
var validSecurityEffects = map[SecurityEffect]bool{
	EffectReadOne:               true,
	EffectListScoped:            true,
	EffectCreateResource:        true,
	EffectUpdateResource:        true,
	EffectDeleteResource:        true,
	EffectGrantAuthority:        true,
	EffectChangeAuthority:       true,
	EffectRevokeAuthority:       true,
	EffectTightenBoundary:       true,
	EffectRelaxBoundary:         true,
	EffectChangePrincipalStatus: true,
	EffectIssueCredential:       true,
	EffectReadSecret:            true,
	EffectMintCredential:        true,
	EffectAssignCredential:      true,
	EffectEmitExternal:          true,
	EffectChangeOwnership:       true,
}

// authorityEffects are effects that require delegation checks.
var authorityEffects = map[SecurityEffect]bool{
	EffectGrantAuthority:  true,
	EffectChangeAuthority: true,
	EffectRevokeAuthority: true,
	EffectChangeOwnership: true,
}

// boundaryEffects are effects that require before/after effective-authority
// calculation.
var boundaryEffects = map[SecurityEffect]bool{
	EffectRelaxBoundary:   true,
	EffectTightenBoundary: true,
}

// auditRequiredEffects are effects that require audit records.
var auditRequiredEffects = map[SecurityEffect]bool{
	EffectGrantAuthority:        true,
	EffectChangeAuthority:       true,
	EffectRevokeAuthority:       true,
	EffectChangeOwnership:       true,
	EffectRelaxBoundary:         true,
	EffectTightenBoundary:       true,
	EffectChangePrincipalStatus: true,
	EffectDeleteResource:        true,
	EffectMintCredential:        true,
	EffectIssueCredential:       true,
	EffectAssignCredential:      true,
	EffectReadSecret:            true,
	EffectEmitExternal:          true,
}

// IsAuthorityEffect reports whether the effect involves creating,
// modifying, or revoking authority.
func (e SecurityEffect) IsAuthorityEffect() bool {
	return authorityEffects[e]
}

// IsBoundaryEffect reports whether the effect involves boundary
// relaxation or tightening.
func (e SecurityEffect) IsBoundaryEffect() bool {
	return boundaryEffects[e]
}

// RequiresAudit reports whether the effect requires an audit record.
func (e SecurityEffect) RequiresAudit() bool {
	return auditRequiredEffects[e]
}

// DelegationPolicy specifies how delegation is checked for
// authority-increasing effects.
type DelegationPolicy struct {
	// RequireNonAmplification means the actor must hold every
	// permission being granted (CanDelegate check).
	RequireNonAmplification bool

	// Description explains the delegation semantics for reviewers.
	Description string
}

// GovernancePolicy specifies target-governance rules.
type GovernancePolicy struct {
	// Kind identifies the governance model used.
	Kind GovernanceKind

	// Description explains the governance semantics for reviewers.
	Description string

	// DomainCallback names the domain-specific governance function
	// that evaluates target-governance rules. The callback receives
	// actor and target context and returns an allow/deny decision.
	// Format: "package.FunctionName" or a symbolic reference.
	DomainCallback string
}

// GovernanceKind classifies governance models.
type GovernanceKind string

const (
	// GovernancePeerSuperior compares actor and target roles to
	// determine whether the actor may manage the target.
	GovernancePeerSuperior GovernanceKind = "peer_superior"

	// GovernanceOwnershipAncestry evaluates ownership or ancestry
	// relationships between actor and target.
	GovernanceOwnershipAncestry GovernanceKind = "ownership_ancestry"

	// GovernanceProtectedPrincipal identifies protected principal
	// classes that require elevated authority to manage.
	GovernanceProtectedPrincipal GovernanceKind = "protected_principal"

	// GovernanceConstraintAdmin governs constraint administration
	// and issuer/credential relationships.
	GovernanceConstraintAdmin GovernanceKind = "constraint_admin"

	// GovernanceDomainSpecific uses a domain callback for governance
	// decisions that do not fit the above categories.
	GovernanceDomainSpecific GovernanceKind = "domain_specific"
)

// validGovernanceKinds is the closed set of recognized governance kinds.
var validGovernanceKinds = map[GovernanceKind]bool{
	GovernancePeerSuperior:       true,
	GovernanceOwnershipAncestry:  true,
	GovernanceProtectedPrincipal: true,
	GovernanceConstraintAdmin:    true,
	GovernanceDomainSpecific:     true,
}

// Invariant describes a post-state invariant that must hold after the
// operation commits.
type Invariant struct {
	// ID is a stable identifier for the invariant.
	ID string

	// Description explains the invariant for reviewers.
	Description string

	// FailClosed specifies the behavior when the invariant cannot
	// be evaluated (e.g., store error). Must be true for security
	// invariants.
	FailClosed bool
}

// AuditObligation specifies audit requirements for an operation.
type AuditObligation struct {
	// EventType is the audit event type (e.g., "membership.add",
	// "constraint.relax").
	EventType string

	// RequiredFields lists the before/after fields that must be
	// present in the audit record.
	RequiredFields []string
}

// DenialCode is a stable public denial code.
type DenialCode string

// Well-known denial codes. Domain appendices may define additional codes.
const (
	DenialForbidden               DenialCode = "forbidden"
	DenialRoleAssignmentForbidden DenialCode = "role_assignment_forbidden"
	DenialTargetRoleProtected     DenialCode = "target_role_protected"
	DenialLastOwner               DenialCode = "LAST_OWNER"
	DenialInsufficientPermissions DenialCode = "insufficient_permissions"
	DenialScopeViolation          DenialCode = "scope_violation"
	DenialPrincipalIneligible     DenialCode = "principal_ineligible"
	DenialCredentialInsufficient  DenialCode = "credential_insufficient"
	DenialUserSuspended           DenialCode = "user_suspended"
	DenialResourceNotFound        DenialCode = "not_found"
)

// TestRef references an executable test that proves part of the contract.
type TestRef struct {
	// Package is the Go package path containing the test.
	Package string

	// Function is the test function name or pattern.
	Function string
}

// Exemption documents an explicit exemption from normal contract
// requirements.
type Exemption struct {
	// Kind classifies the exemption.
	Kind ExemptionKind

	// Reason explains why the exemption is necessary.
	Reason string

	// Scope limits the exemption (e.g., "seeding only",
	// "offline recovery only").
	Scope string
}

// ExemptionKind classifies exemption types.
type ExemptionKind string

const (
	ExemptionOfflineRecovery    ExemptionKind = "offline_recovery"
	ExemptionDeterministicSeed  ExemptionKind = "deterministic_seed"
	ExemptionMigration          ExemptionKind = "migration"
	ExemptionTestFixture        ExemptionKind = "test_fixture"
	ExemptionAuthenticationOnly ExemptionKind = "authentication_only"
	ExemptionPublicEndpoint     ExemptionKind = "public_endpoint"
	ExemptionInternalOnly       ExemptionKind = "internal_only"
)

// validExemptionKinds is the closed set of recognized exemption kinds.
var validExemptionKinds = map[ExemptionKind]bool{
	ExemptionOfflineRecovery:    true,
	ExemptionDeterministicSeed:  true,
	ExemptionMigration:          true,
	ExemptionTestFixture:        true,
	ExemptionAuthenticationOnly: true,
	ExemptionPublicEndpoint:     true,
	ExemptionInternalOnly:       true,
}
