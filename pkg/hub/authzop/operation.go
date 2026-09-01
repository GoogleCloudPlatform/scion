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
	// ID is a stable, unique operation identifier. Format:
	// lowercase dot-separated segments (e.g., "project.membership.add").
	// Must start with Domain as a prefix. Once assigned, an ID must not
	// change or be reused.
	ID OperationID

	// Domain identifies the product domain that owns this operation
	// (e.g., "project.membership", "agent", "secret", "constraint").
	Domain string

	// Description is a human-readable summary of the operation's purpose.
	Description string

	// EntryPoints enumerates every externally reachable path that
	// dispatches this operation.
	EntryPoints []EntryPoint

	// Principals lists the authenticated principal kinds admitted to
	// attempt this operation. Credentials are declared separately.
	Principals []PrincipalKind

	// Credentials lists the credential kinds admitted for this
	// operation. Separated from Principals to express distinctions
	// like "user via session_jwt but not scoped_uat".
	Credentials []CredentialKind

	// ResourceResolver names the authoritative resource and scope
	// resolver for this operation (e.g., "project-from-url",
	// "agent-owner-project").
	ResourceResolver string

	// BasePermission is the canonical permission ID required for this
	// operation (e.g., "project.manage", "agent.create", "secret.read").
	BasePermission string

	// Effects enumerates the typed security effects this operation may
	// produce. Effects select obligation requirements beyond the base
	// permission.
	Effects []SecurityEffect

	// DelegationKind specifies the delegation check required by this
	// operation's effects. Validation enforces that the declared kind
	// satisfies all effect requirements.
	DelegationKind DelegationKind

	// DelegationDescription explains the delegation semantics for
	// reviewers. Required when DelegationKind is not "none".
	DelegationDescription string

	// Governance specifies target-governance rules. Required by effects
	// that involve managing targets (revocation, boundary changes,
	// credential effects). Nil means no governance check is required.
	Governance *GovernancePolicy

	// AuthorityEval specifies the authority-delta evaluation required
	// by this operation. Effects like change-authority, ownership
	// change, and boundary changes require before-and-after evaluation.
	AuthorityEval AuthorityEvalKind

	// Invariants lists post-state invariants that must hold after the
	// operation commits.
	Invariants []Invariant

	// AuditObligation specifies audit requirements including before/after
	// state fields and atomicity. Required by effects that require audit.
	AuditObligation *AuditObligation

	// ExternalPolicy specifies failure/retry semantics for operations
	// with emit-external-effect. Required when the operation includes
	// EffectEmitExternal.
	ExternalPolicy *ExternalEffectPolicy

	// DenialCodes lists the stable public denial codes this operation
	// may return. At least one is required unless explicitly waived.
	DenialCodes []DenialCode

	// TestRefs lists the executable tests that prove this contract.
	TestRefs []TestRef

	// Exemptions documents explicit exemptions from normal contract
	// requirements. Each exemption must declare which specific
	// obligations it waives.
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
	EntryPointWebSocket        EntryPointKind = "websocket"
	EntryPointSSE              EntryPointKind = "sse"
	EntryPointBrokerCall       EntryPointKind = "broker_call"
	EntryPointSchedulerJob     EntryPointKind = "scheduler_job"
	EntryPointCLICommand       EntryPointKind = "cli_command"
	EntryPointBackgroundJob    EntryPointKind = "background_job"
	EntryPointInternalDispatch EntryPointKind = "internal_dispatch"
)

// validEntryPointKinds is the closed set of recognized entry point kinds.
var validEntryPointKinds = map[EntryPointKind]bool{
	EntryPointHTTPRoute:        true,
	EntryPointWebSocket:        true,
	EntryPointSSE:              true,
	EntryPointBrokerCall:       true,
	EntryPointSchedulerJob:     true,
	EntryPointCLICommand:       true,
	EntryPointBackgroundJob:    true,
	EntryPointInternalDispatch: true,
}

// httpLikeEntryPoints are entry points that require an HTTP method.
var httpLikeEntryPoints = map[EntryPointKind]bool{
	EntryPointHTTPRoute: true,
	EntryPointWebSocket: true,
	EntryPointSSE:       true,
}

// PrincipalKind identifies a class of authenticated principal.
// Credential types are declared separately via CredentialKind.
type PrincipalKind string

const (
	PrincipalUser           PrincipalKind = "user"
	PrincipalAgent          PrincipalKind = "agent"
	PrincipalBroker         PrincipalKind = "broker"
	PrincipalServiceAccount PrincipalKind = "service_account"
	PrincipalSystem         PrincipalKind = "system"
)

// validPrincipalKinds is the closed set of recognized principal kinds.
var validPrincipalKinds = map[PrincipalKind]bool{
	PrincipalUser:           true,
	PrincipalAgent:          true,
	PrincipalBroker:         true,
	PrincipalServiceAccount: true,
	PrincipalSystem:         true,
}

// CredentialKind identifies a class of authentication credential
// admitted for an operation. Separated from PrincipalKind to express
// distinctions like "user via session JWT but not scoped UAT."
type CredentialKind string

const (
	CredentialSessionJWT     CredentialKind = "session_jwt"
	CredentialScopedUAT      CredentialKind = "scoped_uat"
	CredentialAgentJWT       CredentialKind = "agent_jwt"
	CredentialBrokerToken    CredentialKind = "broker_token"
	CredentialServiceAccount CredentialKind = "service_account_key"
	CredentialSystemInternal CredentialKind = "system_internal"
	CredentialIdentityToken  CredentialKind = "identity_token"
)

// validCredentialKinds is the closed set of recognized credential kinds.
var validCredentialKinds = map[CredentialKind]bool{
	CredentialSessionJWT:     true,
	CredentialScopedUAT:      true,
	CredentialAgentJWT:       true,
	CredentialBrokerToken:    true,
	CredentialServiceAccount: true,
	CredentialSystemInternal: true,
	CredentialIdentityToken:  true,
}

// SecurityEffect classifies the security-meaningful consequence of an
// operation. Effects select obligation requirements beyond the base
// permission.
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

// DelegationKind classifies delegation check requirements.
type DelegationKind string

const (
	// DelegationNone means no delegation check is required.
	DelegationNone DelegationKind = "none"

	// DelegationNonAmplification means the actor must hold every
	// permission being granted (CanDelegate / non-amplification).
	DelegationNonAmplification DelegationKind = "non_amplification"

	// DelegationConditionalIncrease means delegation is checked only
	// when the proposed change increases authority (before/after
	// comparison shows increased effective permissions).
	DelegationConditionalIncrease DelegationKind = "conditional_on_increase"
)

// validDelegationKinds is the closed set of recognized delegation kinds.
var validDelegationKinds = map[DelegationKind]bool{
	DelegationNone:                true,
	DelegationNonAmplification:    true,
	DelegationConditionalIncrease: true,
}

// delegationStrength orders delegation kinds for subsumption checks.
// A higher value satisfies all lower requirements.
var delegationStrength = map[DelegationKind]int{
	DelegationNone:                0,
	DelegationNonAmplification:    1,
	DelegationConditionalIncrease: 2,
}

// effectDelegationRequirements maps effects to their minimum required
// delegation kind. Effects not in this map require DelegationNone.
var effectDelegationRequirements = map[SecurityEffect]DelegationKind{
	EffectGrantAuthority:  DelegationNonAmplification,
	EffectChangeAuthority: DelegationConditionalIncrease,
	EffectChangeOwnership: DelegationNonAmplification,
}

// effectGovernanceRequired maps effects that require a governance policy.
var effectGovernanceRequired = map[SecurityEffect]bool{
	EffectRevokeAuthority:  true,
	EffectChangeOwnership:  true,
	EffectRelaxBoundary:    true,
	EffectTightenBoundary:  true,
	EffectIssueCredential:  true,
	EffectMintCredential:   true,
	EffectAssignCredential: true,
}

// AuthorityEvalKind classifies authority-delta evaluation requirements.
type AuthorityEvalKind string

const (
	// AuthorityEvalNone means no before/after evaluation is required.
	AuthorityEvalNone AuthorityEvalKind = "none"

	// AuthorityEvalProposedPost evaluates the proposed post-state
	// authority to verify invariants (e.g., last-owner guard).
	AuthorityEvalProposedPost AuthorityEvalKind = "proposed_post_state"

	// AuthorityEvalBeforeAndAfter evaluates both before and after
	// effective authority to detect increases, decreases, and boundary
	// changes.
	AuthorityEvalBeforeAndAfter AuthorityEvalKind = "before_and_after"
)

// validAuthorityEvalKinds is the closed set of recognized authority
// evaluation kinds.
var validAuthorityEvalKinds = map[AuthorityEvalKind]bool{
	AuthorityEvalNone:           true,
	AuthorityEvalProposedPost:   true,
	AuthorityEvalBeforeAndAfter: true,
}

// authorityEvalStrength orders evaluation kinds for subsumption.
var authorityEvalStrength = map[AuthorityEvalKind]int{
	AuthorityEvalNone:           0,
	AuthorityEvalProposedPost:   1,
	AuthorityEvalBeforeAndAfter: 2,
}

// effectAuthorityEvalRequirements maps effects to their minimum
// authority evaluation requirement. Effects not in this map require
// AuthorityEvalNone.
var effectAuthorityEvalRequirements = map[SecurityEffect]AuthorityEvalKind{
	EffectChangeAuthority: AuthorityEvalBeforeAndAfter,
	EffectChangeOwnership: AuthorityEvalBeforeAndAfter,
	EffectRelaxBoundary:   AuthorityEvalBeforeAndAfter,
	EffectTightenBoundary: AuthorityEvalBeforeAndAfter,
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

// AuditFieldReq describes which audit state fields an effect requires.
type AuditFieldReq struct {
	NeedsBefore bool
	NeedsAfter  bool
}

// effectAuditFieldRequirements maps audit-requiring effects to whether
// they need before-state and/or after-state fields in the audit record.
var effectAuditFieldRequirements = map[SecurityEffect]AuditFieldReq{
	EffectGrantAuthority:        {NeedsBefore: false, NeedsAfter: true},
	EffectChangeAuthority:       {NeedsBefore: true, NeedsAfter: true},
	EffectRevokeAuthority:       {NeedsBefore: true, NeedsAfter: false},
	EffectChangeOwnership:       {NeedsBefore: true, NeedsAfter: true},
	EffectDeleteResource:        {NeedsBefore: true, NeedsAfter: false},
	EffectRelaxBoundary:         {NeedsBefore: true, NeedsAfter: true},
	EffectTightenBoundary:       {NeedsBefore: true, NeedsAfter: true},
	EffectChangePrincipalStatus: {NeedsBefore: true, NeedsAfter: true},
	EffectIssueCredential:       {NeedsBefore: false, NeedsAfter: true},
	EffectMintCredential:        {NeedsBefore: false, NeedsAfter: true},
	EffectAssignCredential:      {NeedsBefore: false, NeedsAfter: true},
	EffectReadSecret:            {NeedsBefore: false, NeedsAfter: false},
	EffectEmitExternal:          {NeedsBefore: false, NeedsAfter: true},
}

// IsAuthorityEffect reports whether the effect involves creating,
// modifying, or revoking authority.
func (e SecurityEffect) IsAuthorityEffect() bool {
	switch e {
	case EffectGrantAuthority, EffectChangeAuthority,
		EffectRevokeAuthority, EffectChangeOwnership:
		return true
	}
	return false
}

// IsBoundaryEffect reports whether the effect involves boundary
// relaxation or tightening.
func (e SecurityEffect) IsBoundaryEffect() bool {
	return e == EffectRelaxBoundary || e == EffectTightenBoundary
}

// RequiresAudit reports whether the effect requires an audit record.
func (e SecurityEffect) RequiresAudit() bool {
	return auditRequiredEffects[e]
}

// GovernancePolicy specifies target-governance rules.
type GovernancePolicy struct {
	// Kind identifies the governance model used.
	Kind GovernanceKind

	// Description explains the governance semantics for reviewers.
	Description string

	// DomainCallback names the domain-specific governance function.
	// Required when Kind is GovernanceDomainSpecific.
	DomainCallback string
}

// GovernanceKind classifies governance models.
type GovernanceKind string

const (
	GovernancePeerSuperior       GovernanceKind = "peer_superior"
	GovernanceOwnershipAncestry  GovernanceKind = "ownership_ancestry"
	GovernanceProtectedPrincipal GovernanceKind = "protected_principal"
	GovernanceConstraintAdmin    GovernanceKind = "constraint_admin"
	GovernanceIssuerCredential   GovernanceKind = "issuer_credential"
	GovernanceDomainSpecific     GovernanceKind = "domain_specific"
)

// validGovernanceKinds is the closed set of recognized governance kinds.
var validGovernanceKinds = map[GovernanceKind]bool{
	GovernancePeerSuperior:       true,
	GovernanceOwnershipAncestry:  true,
	GovernanceProtectedPrincipal: true,
	GovernanceConstraintAdmin:    true,
	GovernanceIssuerCredential:   true,
	GovernanceDomainSpecific:     true,
}

// Invariant describes a post-state invariant that must hold after the
// operation commits.
type Invariant struct {
	// ID is a stable identifier for the invariant.
	ID string

	// Description explains the invariant for reviewers.
	Description string

	// Kind classifies the invariant severity.
	Kind InvariantKind

	// FailClosed specifies the behavior when the invariant cannot
	// be evaluated (e.g., store error). Must be true for security
	// invariants.
	FailClosed bool
}

// InvariantKind classifies invariant severity.
type InvariantKind string

const (
	// InvariantSecurity is a security invariant that must be
	// fail-closed. Violations indicate a security defect.
	InvariantSecurity InvariantKind = "security"

	// InvariantBusiness is a business-logic invariant. May be
	// fail-closed or fail-open depending on business requirements.
	InvariantBusiness InvariantKind = "business"
)

// validInvariantKinds is the closed set of recognized invariant kinds.
var validInvariantKinds = map[InvariantKind]bool{
	InvariantSecurity: true,
	InvariantBusiness: true,
}

// AuditObligation specifies audit requirements for an operation.
type AuditObligation struct {
	// EventType is the audit event type (e.g., "membership.add",
	// "constraint.relax").
	EventType string

	// ContextFields lists context fields always required in the
	// audit record (e.g., "actor_id", "project_id").
	ContextFields []string

	// BeforeFields lists pre-mutation state fields required in the
	// audit record. Required by effects that destroy or change state.
	BeforeFields []string

	// AfterFields lists post-mutation state fields required in the
	// audit record. Required by effects that create or change state.
	AfterFields []string

	// Atomic indicates the audit record is written in the same
	// transaction as the mutation.
	Atomic bool

	// NonAtomicJustification is required when Atomic is false.
	// Explains why atomic audit is not feasible and what mitigations
	// are in place.
	NonAtomicJustification string
}

// ExternalEffectPolicy documents the failure/retry contract for
// operations that emit external effects.
type ExternalEffectPolicy struct {
	// DeliveryMode classifies the delivery guarantee.
	DeliveryMode ExternalDeliveryMode

	// FailureMode classifies how failures are handled.
	FailureMode ExternalFailureMode

	// IdempotencyKey describes how idempotency is ensured (e.g.,
	// "dispatch ID", "event correlation ID").
	IdempotencyKey string

	// RetryPolicy describes retry semantics (e.g., "exponential
	// backoff with 3 retries", "no retry").
	RetryPolicy string

	// Compensation describes rollback/compensation on failure.
	Compensation string

	// AuthBeforeEmit indicates authorization is checked before the
	// external effect is emitted.
	AuthBeforeEmit bool
}

// ExternalDeliveryMode classifies delivery guarantees.
type ExternalDeliveryMode string

const (
	DeliveryFireAndForget ExternalDeliveryMode = "fire_and_forget"
	DeliveryAtLeastOnce   ExternalDeliveryMode = "at_least_once"
	DeliveryExactlyOnce   ExternalDeliveryMode = "exactly_once"
)

// validExternalDeliveryModes is the closed set of delivery modes.
var validExternalDeliveryModes = map[ExternalDeliveryMode]bool{
	DeliveryFireAndForget: true,
	DeliveryAtLeastOnce:   true,
	DeliveryExactlyOnce:   true,
}

// ExternalFailureMode classifies failure handling.
type ExternalFailureMode string

const (
	FailureLogAndContinue ExternalFailureMode = "log_and_continue"
	FailureFailOperation  ExternalFailureMode = "fail_operation"
	FailureCompensate     ExternalFailureMode = "compensate"
)

// validExternalFailureModes is the closed set of failure modes.
var validExternalFailureModes = map[ExternalFailureMode]bool{
	FailureLogAndContinue: true,
	FailureFailOperation:  true,
	FailureCompensate:     true,
}

// DenialCode is a stable public denial code.
type DenialCode string

// Well-known denial codes.
const (
	DenialForbidden               DenialCode = "forbidden"
	DenialRoleAssignmentForbidden DenialCode = "role_assignment_forbidden"
	DenialTargetRoleProtected     DenialCode = "target_role_protected"
	DenialLastOwner               DenialCode = "last_owner"
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
// requirements. Each exemption must declare which specific obligations
// it waives via the Waives field.
type Exemption struct {
	// Kind classifies the exemption.
	Kind ExemptionKind

	// Reason explains why the exemption is necessary.
	Reason string

	// Scope limits the exemption (e.g., "seeding only",
	// "offline recovery only").
	Scope string

	// Waives lists the specific obligations this exemption waives.
	// Validation only bypasses the named requirements; un-waived
	// requirements are still enforced.
	Waives []WaivedObligation
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

// WaivedObligation identifies a specific requirement an exemption waives.
type WaivedObligation string

const (
	WaiveEntryPoints      WaivedObligation = "entry_points"
	WaivePrincipals       WaivedObligation = "principals"
	WaiveCredentials      WaivedObligation = "credentials"
	WaiveBasePermission   WaivedObligation = "base_permission"
	WaiveResourceResolver WaivedObligation = "resource_resolver"
	WaiveTestRefs         WaivedObligation = "test_refs"
	WaiveDenialCodes      WaivedObligation = "denial_codes"
	WaiveAuditObligation  WaivedObligation = "audit_obligation"
)

// validWaivedObligations is the closed set of waivable obligations.
var validWaivedObligations = map[WaivedObligation]bool{
	WaiveEntryPoints:      true,
	WaivePrincipals:       true,
	WaiveCredentials:      true,
	WaiveBasePermission:   true,
	WaiveResourceResolver: true,
	WaiveTestRefs:         true,
	WaiveDenialCodes:      true,
	WaiveAuditObligation:  true,
}
