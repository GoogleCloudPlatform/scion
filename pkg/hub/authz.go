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

package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Action represents an authorization action.
type Action string

// Action constants for authorization checks.
const (
	ActionCreate     Action = "create"
	ActionRead       Action = "read"
	ActionUpdate     Action = "update"
	ActionDelete     Action = "delete"
	ActionList       Action = "list"
	ActionManage     Action = "manage"
	ActionStart      Action = "start"
	ActionStop       Action = "stop"
	ActionMessage    Action = "message"
	ActionAttach     Action = "attach"
	ActionLifecycle  Action = "lifecycle"
	ActionPortAccess Action = "port_access"
	// ActionTunnel gates the Conduit `scion tunnel` / `scion ssh` request
	// path (not a stream kind). It is not a registered permission of its
	// own: it is granted wherever agent.port_access is (see
	// conduitAuthzFor), so every role, relationship and token scope that
	// carries port access carries it.
	ActionTunnel       Action = "tunnel"
	ActionRegister     Action = "register"
	ActionAddMember    Action = "addMember"
	ActionRemoveMember Action = "removeMember"
	ActionDispatch     Action = "dispatch"
	ActionStopAll      Action = "stop_all"
	ActionVerify       Action = "verify"
	ActionMint         Action = "mint"
	// ActionAssign covers binding a resource to a principal that will act with
	// it — currently attaching a GCP service account to an agent.
	ActionAssign         Action = "assign"
	ActionInvite         Action = "invite"
	ActionSuspend        Action = "suspend"
	ActionPromote        Action = "promote"
	ActionClone          Action = "clone"
	ActionExecute        Action = "execute"
	ActionSetMessageMode Action = "set_message_mode"

	// Global-catalog write actions — see design doc §3.1.
	// These distinguish hub-catalog mutation from project-scoped CRUD,
	// so a system-scoped binding carrying only these IDs does not
	// silently grant project-level authority.
	ActionCreateGlobal Action = "create_global"

	// ActionDeliver and ActionUse distinguish launch-time material delivery
	// from an agent's own runtime retrieval or token-mint request. Neither
	// is listed in isReadOnlyOperation: a material decision always runs the
	// delegation ceiling.
	ActionDeliver Action = "deliver"
	ActionUse     Action = "use"
)

// Resource represents the target of an authorization check.
type Resource struct {
	Type       string            // e.g. "agent", "project", "policy", "group"
	ID         string            // Resource ID
	OwnerID    string            // Owner user ID
	ParentType string            // e.g. "project" for an agent
	ParentID   string            // Parent resource ID
	Labels     map[string]string // Resource labels for condition matching
	Ancestry   []string          // Ordered ancestor chain [root, ..., parent] for transitive access

	// ScopeKind is the resource's own scope classification for resource
	// types whose records are themselves partitioned by scope: "skill"
	// (store.SkillScopeGlobal/Core/Project/User), "template"
	// (store.TemplateScopeGlobal/Project/User), and "harness_config"
	// (store.HarnessConfigScopeGlobal/Project/User). It is distinct from
	// ParentType/ParentID, which describe project containment for the
	// kernel's project-scoped binding check. ScopeKind instead lets a
	// resource-type-specific check (see filterHubWideSkillGrants,
	// filterHubWideTemplateGrants, filterHubWideHarnessConfigGrants) tell a
	// genuinely hub-scoped record apart from a user- or project-scoped one
	// that merely happens to have no ParentType set. Left empty for resource
	// types that don't need it. For "skill", "template", and
	// "harness_config" resources specifically, build this through the
	// resource's canonical constructor (skillResource/skillScopeResource,
	// templateResource/templateScopeResource,
	// harnessConfigResource/harnessConfigScopeResource) rather than a
	// hand-built literal: each filter fails closed on an empty or
	// unrecognized ScopeKind (ptone/scion#1901 finding F4; ptone/scion#1916
	// applies the same rule to template and harness_config), so only those
	// constructors are guaranteed to set it correctly.
	ScopeKind string

	// ScopeUserID is the owning user of a user-scoped skill
	// (store.Skill.ScopeID when ScopeKind is store.SkillScopeUser), set only
	// by skillScopeResource/skillResource. It lets the personal-skill progeny
	// grant (skillProgenyAdapter, authz_skill_progeny.go) match the same
	// column the skill list predicate filters on. Empty otherwise.
	ScopeUserID string

	// launchedTarget is the agent record the single-agent GET handlers
	// read from the store, set only by agentStatusReadResource. The
	// launcher status-read rule (authz_launcher_read.go) decides from it
	// alone. It is unexported so no decoded request can carry it.
	launchedTarget *store.Agent
}

// PrincipalKind describes the authenticated actor evaluated by an authorization request.
type PrincipalKind string

const (
	PrincipalKindUser             PrincipalKind = "user"
	PrincipalKindAgent            PrincipalKind = "agent"
	PrincipalKindFederatedUser    PrincipalKind = "federated_user"
	PrincipalKindFederatedAgent   PrincipalKind = "federated_agent"
	PrincipalKindFederatedService PrincipalKind = "federated_service"
	PrincipalKindBroker           PrincipalKind = "broker"
	PrincipalKindDev              PrincipalKind = "dev"
)

// CredentialKind describes the authentication material that established a principal.
// Credential constraints are caveats: they may narrow authority but never grant it.
type CredentialKind = credentialmeta.Kind

const (
	CredentialKindInteractive = credentialmeta.KindInteractive
	CredentialKindUAT         = credentialmeta.KindUAT
	CredentialKindAgentJWT    = credentialmeta.KindAgentJWT
	CredentialKindFederation  = credentialmeta.KindFederation
	CredentialKindBroker      = credentialmeta.KindBroker
	CredentialKindDev         = credentialmeta.KindDev

	// CredentialKindHubDelivery is the internal credential a hub-side
	// material delivery caller presents (ptone/scion#2228 part 2). It is
	// produced only by the unexported newHubDeliveryIdentity constructor
	// (authz_delivery_credential.go); no request context, token or header
	// can carry it.
	CredentialKindHubDelivery CredentialKind = "hub_delivery"
)

// PrincipalContext identifies the authenticated actor for an authorization request.
// Identity remains available during the migration so existing policy evaluation can
// use the established identity interfaces.
type PrincipalContext struct {
	Kind     PrincipalKind
	ID       string
	Identity Identity
}

// CredentialContext records the credential used for an authorization request.
// ProjectID and Scopes are caveats for scoped bearer credentials. Ceiling is
// the UAT's normalized permission ceiling — the single source every
// credential-scope restriction evaluates through, for every ceiling
// version.
type CredentialContext struct {
	Kind      CredentialKind
	ID        string
	Type      string
	ProjectID string
	// Boundary carries the credential-side boundary (project or hub) for a
	// UAT credential; nil for every other credential kind. ProjectID is
	// filled from the same boundary for a project-scoped UAT, so a caller
	// that reads only ProjectID sees a value consistent with Boundary. A
	// boundary-aware caller should read Boundary directly, never infer
	// hub-vs-project from an empty ProjectID, which is never a positive
	// claim of hub scope by itself.
	Boundary *TokenBoundary
	Scopes   []string
	Ceiling  permissions.FrozenPermissionCeiling

	// Descriptive credential metadata. Decoration is additive,
	// server-derived attribution (token name/boundary/purpose/labels) for
	// logs and audit. It is never read by authorization decisions — see
	// TestCredentialDecorationNotReadByAuthzCode.
	Decoration *CredentialDecoration
}

// AuthzRequest carries both the acting principal and the credential caveats.
type AuthzRequest struct {
	Principal  PrincipalContext
	Credential CredentialContext
	Resource   Resource
	Action     Action
	Permission string // Canonical permission ID (e.g., "hub.settings.read"); when set, role binding evaluation uses this instead of Resource+Action.
	Explain    bool   // When true, collect step-by-step trace in Decision

	// TargetEvidence is the collection-level classification the bearer
	// gate (step 1) uses to resolve the target scope of a request that
	// names no existing resource instance. Trusted server-side operation
	// code constructs it; it is never taken from a client-supplied field.
	// The zero value classifies the request from Resource alone. Evidence
	// with IsCollectionLevel set must name the permission this request
	// evaluates; the gate denies evidence that names any other permission.
	// It is read only by the bearer gate and grants nothing by itself.
	TargetEvidence TargetScopeEvidence

	// Actor and Purpose describe who initiated the operation and why, when
	// that differs from Principal (for example a delivery performed for a
	// target agent). They are recorded on every decision (and in its
	// provenance when present) for audit and never contribute to the
	// decision.
	Actor   *DecisionActor
	Purpose string

	// bearerRun carries the bearer gate's request-scoped memo and outcome
	// for EvaluateBearerCeiling. It is set only by in-package callers,
	// grants nothing, and callers of Decide leave it nil.
	bearerRun *bearerGateRun

	// AlwaysAudit forces Decide's single audit exit to emit a decision audit
	// record for this request regardless of the allow-sampling rate
	// (AuthzService.DecisionAuditSampleRate). Deny decisions are always
	// audited already; this exists for callers that know in advance they
	// need an allow decision audited unconditionally too. See also
	// Decision.AlwaysAudit, its counterpart for a branch inside decide's
	// body that only learns this partway through evaluation. It never
	// changes the authorization result.
	AlwaysAudit bool

	// ListRow marks an agent read decided to choose the rows of an agent
	// list (AuthorizeListReadBatch). It changes only the UAT ceiling check
	// for agent.read (see listRowReadCeiling); every other stage of the
	// decision runs unchanged.
	ListRow bool
}

// listRowReadCeiling returns ceiling widened with agent.read when request
// is an agent-list row read and the token's scopes allow listing that
// agent: agent.list, or project.read on a token bound to the agent's
// project (ptone/scion#3346). Otherwise it returns ceiling unchanged. Only
// the token-scope part of the decision changes; bindings, relationships,
// constraints and the project access check still decide every row.
func listRowReadCeiling(request AuthzRequest, permissionID string, boundary *TokenBoundary, ceiling permissions.FrozenPermissionCeiling) permissions.FrozenPermissionCeiling {
	if !request.ListRow || request.Action != ActionRead || request.Resource.Type != "agent" ||
		permissionID != "agent.read" || ceiling.Allows("agent.read") {
		return ceiling
	}
	projectBound := boundary != nil && boundary.Kind == BoundaryKindProject &&
		boundary.ProjectID != "" && boundary.ProjectID == request.Resource.ParentID
	listScope := ceiling.Allows("agent.list") || (projectBound && ceiling.Allows("project.read"))
	if !listScope {
		return ceiling
	}
	ceiling.PermissionIDs = append(append([]string{}, ceiling.PermissionIDs...), "agent.read")
	return ceiling
}

// listRowReadReasonMarker is appended to the Reason of an allowed
// decision that passed the UAT ceiling only through listRowReadCeiling,
// so the audit record shows that agent.read was allowed as an agent-list
// row read. It is a fixed string and carries no token or ceiling value.
const listRowReadReasonMarker = "agent-list row read"

// listRowReadWidened reports whether listRowReadCeiling adds agent.read
// to ceiling for request.
func listRowReadWidened(request AuthzRequest, permissionID string, boundary *TokenBoundary, ceiling permissions.FrozenPermissionCeiling) bool {
	return !ceiling.Allows("agent.read") &&
		listRowReadCeiling(request, permissionID, boundary, ceiling).Allows("agent.read")
}

// DecisionActor identifies the initiator of an operation. Audit-only.
type DecisionActor struct {
	Kind PrincipalKind `json:"kind,omitempty"`
	ID   string        `json:"id"`
}

// AuthzRequestFromContext builds a request from authentication middleware
// context. Legacy callers that supplied only an identity receive a derived
// credential context, keeping the compatibility adapter safe during migration.
func AuthzRequestFromContext(ctx context.Context, resource Resource, action Action) AuthzRequest {
	identity := GetIdentityFromContext(ctx)
	credential := GetCredentialContextFromContext(ctx)
	if credential.Kind == "" {
		credential = credentialContextForIdentity(identity)
	}
	return AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credential,
		Resource:   resource,
		Action:     action,
	}
}

// DecisionStep represents a single step in the authorization explain trace.
type DecisionStep struct {
	Step   string `json:"step"`
	Detail string `json:"detail"`
}

// Decision represents the result of an authorization check.
type Decision struct {
	Allowed        bool   // Whether access is allowed
	Reason         string // Human-readable explanation
	BindingID      string // ID of the matched role binding (if any)
	RoleName       string // Name of the matched role (if any)
	Scope          string // Scope level that decided (hub, project, resource)
	MatchedGrant   string // Audit-ready matched grant identifier
	MatchedPolicy  string // Audit-ready matched policy identifier
	PrincipalKind  PrincipalKind
	PrincipalID    string
	CredentialID   string
	CredentialType string
	CredentialKind string
	ExplainTrace   []DecisionStep `json:"explainTrace,omitempty"`

	// principalDecorated is set by decorateDecision, the single function that
	// derives PrincipalID from the principal Decide evaluated. It is
	// unexported and untagged so it carries no wire representation.
	// BuildDecisionAuditRecord reads it to tell an empty derived PrincipalID
	// apart from a Decision decorateDecision never touched, instead of
	// treating an empty string as "derivation ran and found nothing" in both
	// cases.
	principalDecorated bool

	// Actor and Purpose echo AuthzRequest.Actor/Purpose on every decision.
	// Audit-only: they never contribute to Allowed.
	Actor   *DecisionActor `json:"actor,omitempty"`
	Purpose string         `json:"purpose,omitempty"`

	// DeniedBy names the pipeline stage that produced a deny, as a stable
	// snake_case value. It is empty on allow and on a deny that is not
	// attributed to a named stage. Reason is independent of it.
	DeniedBy DeniedBy `json:"deniedBy,omitempty"`

	// PermissionID is the caller-supplied AuthzRequest.Permission, recorded
	// only when it is a canonical ID present in the permissions registry.
	// It is never derived from Resource/Action, and never an unregistered
	// string: an unset or unrecognized Permission leaves this empty. See
	// auditPermissionID, the single function that computes it.
	PermissionID string `json:"permissionId,omitempty"`

	// AlwaysAudit forces Decide's single audit exit to emit a decision audit
	// record for this decision regardless of the allow-sampling rate, the
	// same as AuthzRequest.AlwaysAudit — but settable from inside decide's
	// body, for a branch that determines only partway through evaluation
	// that this decision must not be sampled away (for example a delegated-
	// agent branch routed on identity kind after principal/credential
	// derivation, which the caller building AuthzRequest cannot know to flag
	// in advance). The single audit exit ORs this with the request-level
	// flag. Authorization-neutral: it never changes Allowed or Reason.
	AlwaysAudit bool `json:"-"`

	// DenyCause classifies certain deny decisions structurally, so callers
	// can react to *why* access was denied without parsing or matching
	// substrings of Reason (which is prose, for logs and explain, and is
	// free to change wording). Set by Step 10 (the agent delegation
	// ceiling) for a SUBSET of ceiling denials only — see the DenyCause
	// constants for which ones. Other ceiling denials (e.g. max depth, no
	// edge, duplicate active edges), and all non-ceiling denials, leave it
	// at its zero value.
	DenyCause DenyCause `json:"denyCause,omitempty"`

	// adoptionRemediable is set with a ceiling_unrecorded DenyCause when the
	// denying hop is an unrecorded row that delegation-provenance adoption
	// can address (not a hop with an unsupported provenance version). It
	// only selects the additive response details; see adoptionDetailsCause.
	adoptionRemediable bool

	// Provenance contains the full decision provenance when Explain=true.
	// For non-explain requests, this is populated with minimal data
	// (matched grant and deny reason).
	Provenance *DecisionProvenance `json:"provenance,omitempty"`
}

// adoptionDetailsCause returns the cause that selects the
// delegation-provenance adoption response details: ceiling_unrecorded when
// adoption can address the denying hop, and "" otherwise, including a hop
// denied only because its provenance version is not understood.
func (d Decision) adoptionDetailsCause() DenyCause {
	if d.DenyCause == DenyCauseCeilingUnrecorded && d.adoptionRemediable {
		return d.DenyCause
	}
	return ""
}

// DeniedBy is the stable identifier of the stage that denied a decision.
type DeniedBy string

const (
	// DeniedByDelegationCeiling: the agent's delegation chain does not
	// supply the permission (a non-live delegator, a delegator that does
	// not hold the permission, a missing or ambiguous edge, or a failed
	// lookup).
	DeniedByDelegationCeiling DeniedBy = "delegation_ceiling"
)

// DenyCause is a structural tag for a subset of deny reasons that callers
// need to distinguish without string-matching Reason. It is deliberately
// small and closed: only the causes a caller actually branches on get a
// value here, everything else is the zero value ("").
type DenyCause string

const (
	// DenyCauseCeilingOrphaned marks a delegation-ceiling deny where the
	// delegator (the principal that created the agent, directly or
	// transitively) does not resolve, is deleted (including a retained
	// soft-deleted agent), or is the migration sentinel.
	DenyCauseCeilingOrphaned DenyCause = "ceiling_orphaned"

	// DenyCauseCeilingDelegatorLacksPermission marks a delegation-ceiling
	// deny where the delegator exists but does not hold the permission
	// being exercised, including a user that is not active (for example
	// suspended or invited), super-admin or not.
	DenyCauseCeilingDelegatorLacksPermission DenyCause = "ceiling_delegator_lacks_permission"

	// DenyCauseCeilingError marks a delegation-ceiling deny caused by a
	// transient or internal fault (e.g. a store error) rather than a
	// policy fact about the delegator. Callers should treat this like an
	// ordinary denial, not surface it as a specific reason.
	DenyCauseCeilingError DenyCause = "ceiling_error"

	// DenyCauseResolutionError marks a deny caused by a store or resolution
	// fault rather than a policy fact. decide() sets it on four paths:
	// principal resolution (Step 2), role-binding resolution (Step 3),
	// role-definition resolution (Step 4), and access-constraint load
	// failure (Step 7c, detected after Step 9 because the failure there
	// is folded into a deny-all restriction rather than an early return).
	// The bearer gate (evaluateBearerGate) also sets it when the live
	// project access lookup for a user access token fails on a store fault.
	DenyCauseResolutionError DenyCause = "resolution_error"

	// DenyCauseCeilingUnrecorded marks a deny where the source credential's
	// ceiling version is not one this binary interprets, or a hop whose
	// provenance is unrecorded or of an unknown version meets a permission
	// that requires recorded provenance. An unknown ceiling version on a
	// bounded hop in the walk is reported as DenyCauseCeilingEffectExceeded.
	DenyCauseCeilingUnrecorded DenyCause = "ceiling_unrecorded"

	// DenyCauseCeilingEffectExceeded marks a deny where the permission or
	// requested role lies outside a frozen effect ceiling on the chain.
	DenyCauseCeilingEffectExceeded DenyCause = "ceiling_effect_exceeded"

	// DenyCauseCeilingResourceMissing marks a deny where a resource a
	// frozen ceiling refers to does not resolve. Reserved for the
	// service-account parent-ceiling evaluator: no code path in this
	// package emits it.
	DenyCauseCeilingResourceMissing DenyCause = "ceiling_resource_missing"

	// DenyCauseCeilingSourceNotAllowed marks a deny where the source
	// credential is not accepted as an authority source on this server.
	DenyCauseCeilingSourceNotAllowed DenyCause = "ceiling_source_not_allowed"
)

// IsIndeterminate reports whether this deny was caused by a store or
// resolution fault on the tagged paths, rather than a policy fact — the
// access check could not be decided. Tagged: principal, role-binding,
// role-definition and access-constraint resolution in decide(), the
// user-access-token live project access lookup (evaluateBearerGate), and
// the delegation-ceiling error. Not yet tagged: relationship-fact and
// source-active lookup failures (isCurrentHubMember, relationshipSourceActive,
// progenySourceFor). A false result for those candidates does not prove a
// policy deny.
func (d Decision) IsIndeterminate() bool {
	return !d.Allowed && (d.DenyCause == DenyCauseResolutionError || d.DenyCause == DenyCauseCeilingError)
}

// EvaluationDetail provides detailed info for the evaluate endpoint.
type EvaluationDetail struct {
	Scope           string   `json:"scope"`
	Matched         bool     `json:"matched"`
	EffectiveGroups []string `json:"effectiveGroups,omitempty"`
}

// DecisionAuditEmitter is an interface for emitting decision audit records.
type DecisionAuditEmitter interface {
	EmitDecisionAudit(ctx context.Context, record *store.DecisionAuditRecord)
}

// AuthzService provides authorization checks using the AK1 kernel.
type AuthzService struct {
	store                   store.Store
	logger                  *slog.Logger
	decisionAuditEmitter    DecisionAuditEmitter
	DecisionAuditSampleRate float64 // 1.0 = audit everything, <1.0 = sample allow decisions

	// backfillDone caches the delegation edge backfill completion check.
	// The marker is write-once: once latched to true it never reverts.
	// Uses atomic.Bool for thread safety — only the false→true transition
	// is cached; a false result is re-queried on every call so that the
	// latch catches up as soon as the backfill completes.
	backfillDone atomic.Bool

	// relationshipResolver handles progeny relationship grants. Lazily
	// initialized on first use.
	relationshipResolver *RelationshipGrantResolver

	// progenyAdapters holds progeny sharing-source adapters registered
	// through RegisterProgenyAdapter.
	progenyAdapters progenyAdapterRegistry

	// sourceResolver identifies an agent's authoritative source user for
	// the execution-project relationship stage. Nil selects the stored
	// agent row and typed delegation edges.
	sourceResolver ExecutionSourceResolver

	// devLocalEnabled backs devLocalAuthorityEnabled (devauth.go,
	// ptone/scion#2342 B.3 R6). Set once at server construction via
	// setDevLocalAuthorityEnabled, from the same ServerConfig.DevAuthToken
	// != "" condition that gates dev-token acceptance and DevUserID
	// seeding. Zero value false: an AuthzService built without going
	// through that wiring (e.g. a bare &AuthzService{} in a test) fails
	// closed.
	devLocalEnabled bool

	// mintDevAuthOverride mirrors the agent-token mint's dev-auth role
	// override: when set, mintCandidateScopes raises a role below full to
	// full before applying the ceiling filter. Set once at server
	// construction from ServerConfig.DevAuthToken != "". It is separate
	// from devLocalEnabled and is read only by mintCandidateScopes.
	mintDevAuthOverride bool
}

// NewAuthzService creates a new AuthzService.
func NewAuthzService(s store.Store, logger *slog.Logger) *AuthzService {
	svc := &AuthzService{
		store:                   s,
		logger:                  logger,
		DecisionAuditSampleRate: 1.0,
		relationshipResolver:    NewRelationshipGrantResolver(s),
	}
	// ptone/scion#2128: personal (user-scoped) skills are a progeny sharing
	// source keyed on the owning user's bucket (see authz_skill_progeny.go).
	// Registration only fails for a programming error, so a failure here is
	// logged, not fatal. Without a registered adapter, progenyAdapter returns
	// none for "skill" (it is not a built-in store-adapter kind), and the
	// progeny candidate's fact stage rejects it ("no sharing-source adapter")
	// — fail closed, never open.
	if err := svc.RegisterProgenyAdapter(skillProgenyAdapter{}); err != nil {
		if logger != nil {
			logger.Error("failed to register skill progeny adapter", "error", err)
		}
	}
	return svc
}

// SetDecisionAuditEmitter configures the decision audit emitter.
func (a *AuthzService) SetDecisionAuditEmitter(emitter DecisionAuditEmitter) {
	a.decisionAuditEmitter = emitter
}

// CheckAccess evaluates whether the given identity is allowed to perform
// the specified action on the resource. All authorization decisions route
// through the AK1 kernel — there are no privileged early-allow bypasses.
func (a *AuthzService) CheckAccess(ctx context.Context, identity Identity, resource Resource, action Action) Decision {
	return a.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   resource,
		Action:     action,
	})
}

// Decide evaluates an authorization request through the AK1 kernel and emits
// exactly one decision audit record for the outcome, whichever internal
// return path inside decide produced it. This is a structural guarantee: no
// return path inside decide can skip the audit, because decide itself never
// emits — only this wrapper does, once, after decide returns.
func (a *AuthzService) Decide(ctx context.Context, request AuthzRequest) Decision {
	decision := a.decide(ctx, request)
	if a.decisionAuditEmitter != nil {
		a.emitDecisionAudit(ctx, request, decision)
	}
	return decision
}

// decide is Decide's body: the AK1 kernel evaluation itself.
// All grants are traced to either a RoleBinding or a named relationship grant.
// All reductions are traced to a named restriction. No undocumented bypasses.
//
// request.bearerRun, when non-nil, supplies the request-scoped
// project-admission memo to the bearer gate (step 1) and receives the gate's
// outcome; nil uses no memo and records nothing. It never changes the
// authorization result.
func (a *AuthzService) decide(ctx context.Context, request AuthzRequest) Decision {
	bearer := request.bearerRun
	// admissionMemo is the request-scoped project-admission memo shared by
	// the bearer gate (step 1) and the relationship project-access stage
	// (step 9), so a request reads project access from the store once.
	admissionMemo := bearer.memoOrNil()
	if admissionMemo == nil {
		admissionMemo = &ProjectAdmissionCache{}
	}
	derivedPrincipal := principalContextForIdentity(request.Principal.Identity)
	derivedCredential := credentialContextForIdentity(request.Principal.Identity)

	// ── One rejection block: fail closed on unrecognized or mismatched
	// classification (ptone/scion#2123) ─────────────────────────────────
	// Evaluated once, after both derivations and before any candidate
	// gathering, so an unrecognized or misrepresented identity never reaches
	// step 1 or the kernel below. Every deny here decorates the decision
	// with the DERIVED principal and credential — never the caller's
	// rejected claim. Decide's single audit exit records it.
	//
	// A nil Principal.Identity denies first, with reason "missing principal";
	// a typed-nil concrete identity (see isNilIdentity) takes the same path,
	// since it carries no usable principal either. Every other case below
	// assumes at least a non-nil identity was supplied, even one of an
	// unrecognized concrete type.
	//
	// A request with an omitted Principal.Kind/Credential.Kind/Principal.ID
	// derives it from the identity via the adapter below; an omitted value
	// never reaches the matching check.
	//
	// A supplied Principal.ID is checked against the derived principal's own
	// ID the same way a supplied Principal.Kind is checked against the
	// derived kind, immediately after it: the caller may name the principal
	// it means, but never a different one than the identity resolves to.
	//
	// A supplied Credential is independent of Principal.Identity in the
	// directions suppliedCredentialCompatible admits: a narrower
	// CredentialContext may be layered onto a local user's own
	// classification (a UAT-shaped Scopes caveat, or an authenticated broker
	// acting on that user's behalf) to exercise restriction logic that keys
	// on Credential.Kind/Scopes/ID alone — the kernel's credential_scope
	// restriction and the UAT/broker on-behalf-of gates work this way. Each
	// admitted direction only ever adds a restriction or binds an
	// independently-verified actor; it never grants the caller's chosen
	// kind on its own say-so. Every other mismatch denies, including a
	// recognized-looking supplied kind on an identity whose own derived kind
	// is empty: an unrecognized identity can never be upgraded into a
	// recognized one by supplied context.
	var denyReason string
	switch {
	case isNilIdentity(request.Principal.Identity):
		denyReason = "missing principal"
	case request.Principal.Kind != "" && request.Principal.Kind != derivedPrincipal.Kind:
		denyReason = "principal kind does not match identity"
	case request.Principal.ID != "" && request.Principal.ID != derivedPrincipal.ID:
		denyReason = "principal id does not match identity"
	case !isRecognizedPrincipalKind(derivedPrincipal.Kind) || !isRecognizedCredentialKind(derivedCredential.Kind):
		if !isRecognizedPrincipalKind(derivedPrincipal.Kind) {
			denyReason = "unrecognized principal kind"
		} else {
			denyReason = "unrecognized credential kind"
		}
	case request.Credential.Kind != "" && !isRecognizedCredentialKind(request.Credential.Kind):
		denyReason = "unrecognized credential kind"
	case request.Credential.Kind != "" && !suppliedCredentialCompatible(ctx, derivedPrincipal, derivedCredential, request.Credential):
		denyReason = "credential kind does not match identity"
	}
	if denyReason != "" {
		return decorateDecision(Decision{Allowed: false, Reason: denyReason}, request, derivedPrincipal, derivedCredential, auditPermissionID(request))
	}

	principal := request.Principal
	principal.Kind = derivedPrincipal.Kind
	if principal.ID == "" {
		principal.ID = derivedPrincipal.ID
	}
	// credential keeps the caller-supplied value when one was given: both
	// the narrowing-UAT and the broker-on-behalf-of cases admitted above
	// carry their own restrictions/effective credential ID forward exactly
	// as supplied. An omitted kind falls back to the identity's own
	// derivation.
	credential := request.Credential
	if credential.Kind == "" {
		credential = derivedCredential
	}
	// A hub_delivery identity always uses its own derived credential, even
	// when the caller supplied one of the same kind: the lines above keep a
	// caller-supplied CredentialContext whenever its kind was admitted, but
	// for hub_delivery that context is replaced by the derived one here, so
	// no caller-supplied Scopes, Ceiling or ID travels with it. The
	// credential's authority is fixed by the constructor, not by request
	// data.
	if derivedCredential.Kind == CredentialKindHubDelivery {
		credential = derivedCredential
	}

	// Unsupported principal kinds — fail closed.
	switch principal.Kind {
	case PrincipalKindFederatedService:
		return decorateDecision(Decision{Allowed: false, Reason: "federated service identities are not supported"}, request, principal, credential, auditPermissionID(request))
	case PrincipalKindBroker:
		return decorateDecision(Decision{Allowed: false, Reason: "broker identities are not supported by authorization"}, request, principal, credential, auditPermissionID(request))
	}

	// Resolve permission ID. When the caller provides an explicit permission,
	// use it; otherwise resolve it from resource type + action. A pair that
	// does not name exactly one permission is denied.
	permissionID := request.Permission
	if permissionID == "" {
		resolved, err := resolveResourcePermission(request.Resource.Type, request.Action)
		if err != nil {
			a.logger.Warn("authorization request has no resolvable permission",
				"resource_type", request.Resource.Type, "action", string(request.Action), "error", err)
			d := Decision{Allowed: false, Reason: unresolvablePermissionReason}
			if request.Explain {
				d.Provenance = &DecisionProvenance{
					Errors:          []string{err.Error()},
					DenyReasons:     []string{unresolvablePermissionReason},
					Grants:          []GrantDetail{},
					InactiveGrants:  []GrantDetail{},
					Restrictions:    []RestrictionProvenance{},
					MembershipPaths: []MembershipPathDetail{},
				}
			}
			return decorateDecision(d, request, principal, credential, auditPermissionID(request))
		}
		permissionID = resolved
	}

	// ── Step 0: Delivery credential gate (ptone/scion#2228) ───────────
	// Deliver permissions are admitted only for a delivery credential kind.
	// The gate precedes every grant stage, so a role binding, synthetic
	// agent binding or relationship grant cannot admit deliver for any
	// other credential kind. Passing the gate is necessary, not
	// sufficient: see authz_delivery_gate.go for the contract a delivery
	// credential kind must meet before it joins the set.
	if !deliveryCredentialAdmitted(permissionID, request.Action, credential.Kind) {
		d := Decision{Allowed: false, Reason: deliveryGateReason}
		if request.Explain {
			d.Provenance = &DecisionProvenance{
				Permission:      permissionID,
				DenyReasons:     []string{deliveryGateReason},
				Grants:          []GrantDetail{},
				InactiveGrants:  []GrantDetail{},
				Restrictions:    []RestrictionProvenance{},
				MembershipPaths: []MembershipPathDetail{},
			}
		}
		return decorateDecision(d, request, principal, credential, auditPermissionID(request))
	}

	// ── Step 0b: Delivery credential bound-agent and permission gate
	// (ptone/scion#2228 part 2) ────────────────────────────────────────
	// A hub_delivery principal may only ever request its own three deliver
	// permissions (hubDeliveryPermissionIDs), for the agent it is bound to.
	// This runs once, here, for every request that reaches it; the step-10
	// gate arm (authz_delegation_ceiling.go, checkHubDeliveryCeiling)
	// enforces the same two rules again on its own, independently of this
	// step, because a direct checkDelegationCeiling call (as in
	// TestHubDelivery_Step10ArmDeniesWithoutStep0b) never reaches Decide's
	// entry block at all.
	if credential.Kind == CredentialKindHubDelivery {
		h, _ := principal.Identity.(*hubDeliveryIdentity)
		var reason string
		switch {
		case h == nil:
			reason = "delivery credential is missing"
		case request.Action != ActionDeliver:
			reason = "delivery credential is limited to deliver permissions"
		default:
			if _, ok := hubDeliveryPermissionIDs[permissionID]; !ok {
				reason = "delivery credential is limited to deliver permissions"
			} else if h.boundAgentID == "" || h.boundAgentID != principal.ID {
				reason = "delivery credential is bound to a different agent"
			}
		}
		if reason != "" {
			d := Decision{Allowed: false, Reason: reason}
			if request.Explain {
				d.Provenance = &DecisionProvenance{
					Permission:      permissionID,
					DenyReasons:     []string{reason},
					Grants:          []GrantDetail{},
					InactiveGrants:  []GrantDetail{},
					Restrictions:    []RestrictionProvenance{},
					MembershipPaths: []MembershipPathDetail{},
				}
			}
			return decorateDecision(d, request, principal, credential, auditPermissionID(request))
		}
	}

	// ── Step 1: Bearer credential gate (pre-kernel) ───────────────────
	// A UAT-kind credential is confined to its boundary and permission
	// ceiling, and a project target additionally requires the holder's
	// current access to that project (evaluateBearerGate,
	// authz_bearer.go). This is a credential constraint, not an allow path: it
	// can only narrow, never widen. It runs before the kernel and before
	// relationship grants, so neither can reach a target outside the
	// boundary or a project the holder cannot currently access.
	if credential.Kind == CredentialKindUAT {
		if in, ok := bearerGateInputsFor(principal, credential); ok {
			in.ceiling = listRowReadCeiling(request, permissionID, &in.boundary, in.ceiling)
			if denied := a.evaluateBearerGate(ctx, principal, in, request.Resource, request.TargetEvidence, request.Action, permissionID, admissionMemo, bearer.traceOrNil()); denied != nil {
				return decorateDecision(*denied, request, principal, credential, auditPermissionID(request))
			}
		}
	}

	// Track resolution errors for explain provenance.
	var resolutionErrors []string

	// ── Step 2: Resolve principal closure ─────────────────────────────
	authzIn := a.inputsFor(ctx, principal.Identity)
	principals, err := authzIn.Principals()
	if err != nil {
		a.logger.Warn("failed to resolve authorization principals",
			"principal_id", principal.ID, "error", err)
		errMsg := "principal resolution error: " + err.Error()
		if request.Explain {
			resolutionErrors = append(resolutionErrors, errMsg)
		}
		d := Decision{Allowed: false, Reason: "principal resolution error (fail-closed)", DenyCause: DenyCauseResolutionError}
		if request.Explain {
			d.Provenance = &DecisionProvenance{
				Permission:      permissionID,
				Errors:          resolutionErrors,
				DenyReasons:     []string{"principal resolution error (fail-closed)"},
				Grants:          []GrantDetail{},
				InactiveGrants:  []GrantDetail{},
				Restrictions:    []RestrictionProvenance{},
				MembershipPaths: []MembershipPathDetail{},
			}
		}
		return decorateDecision(d, request, principal, credential, auditPermissionID(request))
	}

	// Build typed principal closure map (O2: type:id composite keys).
	closure := make(map[string]struct{}, len(principals))
	membershipPaths := make(map[string][]string, len(principals))
	for _, p := range principals {
		key := p.Type + ":" + p.ID
		closure[key] = struct{}{}
		// Default: single-element path (overridden below for groups).
		membershipPaths[key] = []string{p.ID}
	}

	// Build real membership path chains for group principals when
	// Explain=true. For non-explain requests, single-element paths are
	// sufficient (performance optimization).
	directKey := principals[0].Type + ":" + principals[0].ID
	membershipPaths[directKey] = []string{principals[0].ID}
	if request.Explain {
		a.buildMembershipPathChains(ctx, principals[0], principals, membershipPaths)
	}

	// ── Step 3: Load active role bindings (batched) ───────────────────
	bindings, err := authzIn.Bindings()
	if err != nil {
		a.logger.Warn("failed to load role bindings",
			"principal_id", principal.ID, "error", err)
		errMsg := "binding resolution error: " + err.Error()
		if request.Explain {
			resolutionErrors = append(resolutionErrors, errMsg)
		}
		d := Decision{Allowed: false, Reason: "binding resolution error (fail-closed)", DenyCause: DenyCauseResolutionError}
		if request.Explain {
			d.Provenance = &DecisionProvenance{
				Permission:      permissionID,
				Errors:          resolutionErrors,
				DenyReasons:     []string{"binding resolution error (fail-closed)"},
				Grants:          []GrantDetail{},
				InactiveGrants:  []GrantDetail{},
				Restrictions:    []RestrictionProvenance{},
				MembershipPaths: []MembershipPathDetail{},
			}
		}
		return decorateDecision(d, request, principal, credential, auditPermissionID(request))
	}

	// ── Step 4: Load role definitions ─────────────────────────────────
	roleDefs, err := authzIn.RoleDefs()
	if err != nil {
		a.logger.Warn("failed to load role definitions",
			"principal_id", principal.ID, "error", err)
		errMsg := "role resolution error: " + err.Error()
		if request.Explain {
			resolutionErrors = append(resolutionErrors, errMsg)
		}
		d := Decision{Allowed: false, Reason: "role resolution error (fail-closed)", DenyCause: DenyCauseResolutionError}
		if request.Explain {
			d.Provenance = &DecisionProvenance{
				Permission:      permissionID,
				Errors:          resolutionErrors,
				DenyReasons:     []string{"role resolution error (fail-closed)"},
				Grants:          []GrantDetail{},
				InactiveGrants:  []GrantDetail{},
				Restrictions:    []RestrictionProvenance{},
				MembershipPaths: []MembershipPathDetail{},
			}
		}
		return decorateDecision(d, request, principal, credential, auditPermissionID(request))
	}

	// ── Step 5: Convert to CandidateBindings ──────────────────────────
	candidates := toCandidateBindings(bindings)

	// ── Step 5b: Agent synthetic bindings from JWT scopes ─────────────
	// Agents derive project-scoped permissions from their JWT token scopes.
	// This creates a synthetic project-scoped binding so the kernel can
	// evaluate agent permissions through the standard pipeline.
	//
	// Deliberately NOT given a hub-wide-catalog carve-out here: this kernel
	// function is shared by every resource type, including ones (broker,
	// group, user, github_app — see TestAuthz_AgentProjectReadBaseline_NoProjectDenied)
	// where a parentless resource must stay unconditionally denied to an
	// agent. The template/harness_config global-catalog exception for agents
	// (ptone/scion#1916 follow-up) is instead applied at the two call sites
	// that need it — catalogListReadBatch (authorized_list.go) and
	// authorizeTemplateReadRoute/authorizeHarnessConfigRoute — the same way
	// this function already carves out brokers outside the kernel rather
	// than inside it, so the exception cannot leak into an unrelated
	// resource type's evaluation.
	// A hub_delivery principal is skipped here, before the AgentIdentity
	// assertion: its Scopes() is always nil, so buildAgentSyntheticBindings
	// would add nothing — Step 0b already restricts it to ActionDeliver on
	// the three deliver permissions, which this synthetic-binding step never
	// grants. The skip states the rule explicitly rather than relying on
	// nil scopes producing no candidates.
	if isAgentPrincipal(principal.Kind) {
		if _, ok := principal.Identity.(*hubDeliveryIdentity); !ok {
			if agent, ok := principal.Identity.(AgentIdentity); ok && agent.ProjectID() != "" {
				synthCandidates, synthRoles := a.buildAgentSyntheticBindings(agent)
				candidates = append(candidates, synthCandidates...)
				for k, v := range synthRoles {
					roleDefs[k] = v
				}
			}
		}
	}

	// ── Step 5b2: Agent hub skill catalog (ptone/scion#1968) ──────────
	// Agents may read the hub-wide (global/core) skill catalog. The
	// project-scoped JWT binding above cannot express that (a hub-scoped
	// skill has no ProjectID, so scopeApplies rejects it), so add a
	// synthetic system-scoped skill.read/skill.list binding. Step 5c strips
	// it again for any skill that is not global/core, and the agent JWT
	// restriction (7b) and delegation ceiling (10) still apply.
	// A hub_delivery principal is skipped here too, before the AgentIdentity
	// assertion: Step 0b already restricts it to ActionDeliver, so a
	// skill.read/skill.list synthetic binding could never be exercised by
	// this type.
	if request.Resource.Type == "skill" && isAgentPrincipal(principal.Kind) {
		if _, ok := principal.Identity.(*hubDeliveryIdentity); !ok {
			if agent, ok := principal.Identity.(AgentIdentity); ok {
				cb, role := agentSkillCatalogBinding(agent)
				candidates = append(candidates, cb)
				roleDefs[cb.RoleDefinitionID] = role
			}
		}
	}

	// ── Step 5b3: Request-local gcp_service_account.use grant
	// (ptone/scion#2129) ────────────────────────────────────────────────
	// See gcpServiceAccountUseBinding's doc comment. Unlike Step 5b this is
	// not gated on agent.ProjectID() != "": the decided service account can
	// be hub-, project- or user-scoped, and the exact per-instance scope
	// match is what authorizes it, not the agent's own project association.
	if isAgentPrincipal(principal.Kind) {
		if agent, ok := principal.Identity.(AgentIdentity); ok {
			if cb, role := gcpServiceAccountUseBinding(agent, permissionID, request.Resource); cb != nil {
				candidates = append(candidates, *cb)
				roleDefs[role.RoleID] = role
			}
		}
	}

	// ── Step 5c/5d/5e: Hub-wide catalog scope containment (ptone/scion#1901,
	// #1916) ───────────────────────────────────────────────────────────
	// The curated hub-member/hub-viewer roles carry skill/template/
	// harness_config read/list at system scope purely so every hub member
	// can browse the hub-wide (global/core) catalog. That grant must not
	// leak into user- or project-scoped records. applyHubWideScopeFilters
	// (authz_boundary.go) is the single shared dispatcher for this rule —
	// ptone/scion#2117's SystemAuthorityProof/MintTimeSystemGrant call the
	// exact same function, so the two paths cannot drift apart.
	candidates = applyHubWideScopeFilters(candidates, roleDefs, ProjectTargetClass{ResourceType: request.Resource.Type, ScopeKind: request.Resource.ScopeKind})

	// ── Step 6: Build resource context ────────────────────────────────
	resourceCtx := ResourceContext{
		ResourceType: request.Resource.Type,
		ResourceID:   request.Resource.ID,
		OwnerID:      request.Resource.OwnerID,
		ProjectID:    projectIDForResource(request.Resource),
		Ancestry:     request.Resource.Ancestry,
	}

	// ── Step 7: Build restrictions ────────────────────────────────────
	var restrictions []Restriction

	// 7a. UAT credential permission ceiling restriction. Every UAT request
	// applies this restriction, including one whose ceiling has no
	// permission IDs at all: an empty or malformed ceiling denies every
	// permission rather than lifting the restriction.
	if credential.Kind == CredentialKindUAT {
		restrictions = append(restrictions, ceilingRestriction(listRowReadCeiling(request, permissionID, credential.Boundary, credential.Ceiling)))
	}

	// 7b. Agent JWT scope restriction. A hub_delivery principal gets the
	// delivery_credential restriction instead, checked before the
	// AgentIdentity assertion: hubDeliveryIdentity.Scopes() is always nil,
	// and agentScopeRestriction denies everything when scopes are empty, but
	// the delivery credential's authority is hubDeliveryPermissionIDs, not a
	// JWT scope grant.
	if isAgentPrincipal(principal.Kind) {
		if _, ok := principal.Identity.(*hubDeliveryIdentity); ok {
			restrictions = append(restrictions, deliveryCredentialRestriction())
		} else if agent, ok := principal.Identity.(AgentIdentity); ok {
			restrictions = append(restrictions, agentScopeRestriction(agent, request.Resource))
		}
	}

	// 7c. Access constraints (AC1). Calls the error-returning form directly
	// (rather than loadAccessConstraintRestrictions) so a load failure can be
	// tagged as DenyCauseResolutionError below, instead of looking like an
	// ordinary policy deny. Behaviour on error is unchanged: the same
	// deny-all "access_constraint_error" restriction is appended.
	var constraintLoadFailed bool
	acRestrictions, acErr := a.accessConstraintRestrictions(ctx, closure, resourceCtx)
	if acErr != nil {
		a.logger.Warn("failed to load access constraints (fail-closed)", "error", acErr)
		acRestrictions = []Restriction{{
			Kind:        "access_constraint_error",
			Description: "constraint loading failed (fail-closed)",
			// nil Check denies everything.
		}}
		constraintLoadFailed = true
	}
	restrictions = append(restrictions, acRestrictions...)

	// ── Step 8: Evaluate via AK1 kernel ───────────────────────────────
	kernelReq := KernelRequest{
		Permission:        permissionID,
		PrincipalClosure:  closure,
		MembershipPaths:   membershipPaths,
		Resource:          resourceCtx,
		CandidateBindings: candidates,
		RoleDefinitions:   roleDefs,
		Restrictions:      restrictions,
		Now:               time.Now(),
	}
	kernelResult := Evaluate(kernelReq)

	// ── Step 8b: delivery_item_grant ──────────────────────────────────
	// A kernel grant never satisfies a deliver request on its own: the
	// association, progeny or skill-default grant for the selected item is
	// required instead. This runs for every credential kind, not only the
	// internal delivery credential, and it is not a Restriction:
	// Restriction.Check sees only the permission ID and cannot tell a role
	// grant from a relationship grant, and the same restrictions are
	// re-applied to relationship candidates at stage 5, where excluding the
	// permission outright would also reject the progeny candidate.
	decision := kernelDecisionToDecision(kernelResult, permissionID)
	kernelAdmits := kernelResult.Allowed
	if kernelAdmits && isDeliverRequest(permissionID, request.Action) {
		excludeKernelGrantForDeliver(&decision)
		kernelAdmits = false
	}

	// ── Step 9: Relationship candidates ───────────────────────────────
	// On a kernel deny, named relationships (owner, ancestor, launcher,
	// progeny, hub-member assign) are evaluated as typed candidates
	// through the common stages in authz_relationship_rules.go:
	// relationship policy, hub-attested ancestry, relationship fact, source
	// activity, and the same restrictions the kernel applied (7a/7b/7c).
	// With Explain, candidates are also evaluated on a kernel allow so the
	// provenance lists them.
	//
	// Candidates run with both memo keys masked. Their stages reach
	// lookups made on behalf of a principal other than the requester (the
	// progeny source user's system authority and project admission, and the
	// source agent's delegation chain), and those must always read the
	// store, never the requester's memoized principals, constraints or
	// edges. The restrictions passed in were already resolved above.
	var relationshipFault bool
	if !kernelAdmits || request.Explain {
		rel := a.evaluateRelationshipCandidates(maskAllAuthzMemo(ctx), principal, request.Resource, request.Action, permissionID, restrictions, !request.Explain,
			&relationshipProjectAccess{memo: admissionMemo, requestCtx: ctx})
		// A project-access lookup fault (stage 2c) that leaves the request
		// denied is a resolution error, not a policy fact.
		if !kernelAdmits && rel.accepted == nil && rel.projectAccessFault {
			relationshipFault = true
		}
		if !kernelAdmits {
			if rel.accepted != nil {
				kernelProvenance := decision.Provenance
				decision = *rel.accepted
				if request.Explain && kernelProvenance != nil {
					kernelProvenance.DenyReasons = nil
					decision.Provenance = kernelProvenance
				}
			} else if rel.restrictedBy != "" {
				decision.Reason = "relationship grant restricted by " + rel.restrictedBy
				if decision.Provenance != nil {
					decision.Provenance.DenyReasons = append([]string{decision.Reason}, decision.Provenance.DenyReasons...)
				}
			}
		}
		if request.Explain && decision.Provenance != nil {
			decision.Provenance.Relationships = rel.results
		}
	}

	// If the access-constraint load failed (7c), and the outcome is a deny,
	// tag it so callers can distinguish a store fault from a policy fact.
	// This runs regardless of which path produced the deny (kernel or
	// relationship candidates), because the deny-all restriction from 7c
	// applies to both. Step 10 below only overwrites DenyCause on decisions
	// that were allowed at this point, so it cannot clobber this tag.
	if !decision.Allowed && (constraintLoadFailed || relationshipFault) {
		decision.DenyCause = DenyCauseResolutionError
	}

	// ── Step 10: Agent delegation ceiling (post-decision) ────────────
	// Applies to every allowed decision for an agent principal, whatever
	// the grant source (kernel or relationship grant). The ceiling
	// evaluates the exact permission resolved above. A failed lookup
	// denies.
	if decision.Allowed && isAgentPrincipal(principal.Kind) {
		if agent, ok := principal.Identity.(AgentIdentity); ok {
			if getDelegationCeilingCache(ctx) == nil {
				ctx = contextWithDelegationCeilingCache(ctx)
			}
			ceilingReq := request
			ceilingReq.bearerRun = nil
			ceilingReq.Principal = principal
			ceilingReq.Permission = permissionID
			var ceilingCause DenyCause
			// The ceiling runs on a masked ctx: checkDelegationCeiling and
			// everything it reaches for the delegator side (walkDelegationChainWithCause,
			// resolveUserDelegatorAuthority/evaluateUserDelegatorAuthority/
			// userRelationshipAuthority, migrationSentinelCeiling,
			// checkAgentHoldsPermission, IsSystemAdmin/hasActiveSystemRole,
			// getEffectivePermissions and its constraint loads, GetUser) sees
			// no principal/constraint memo, so the delegator side is
			// genuinely unaffected by this memo (see authz_request_inputs.go,
			// maskAuthzInputs). Only delegation edges are still shared,
			// through the untouched edges key — safe because edges are keyed
			// by delegate, not by the requesting principal.
			//
			// The chain walk also applies maskAuthzInputs to its own ctx at
			// entry, so its other caller (relationshipSourceDelegationHolds,
			// reached from the relationship candidates in step 9, which
			// already run with both memo keys masked) and any future caller
			// get the same guarantee without relying on this call site.
			var hopNote unrecordedHopNote
			ceilingAllowed, ceilingReason, ceilingErr := a.checkDelegationCeilingWithNote(maskAuthzInputs(ctx), ceilingReq, permissionID, agent.ID(), nil, &ceilingCause, &hopNote)
			if ceilingErr != nil {
				decision.Allowed = false
				decision.Reason = "delegation ceiling check failed (fail-closed): " + ceilingErr.Error()
				decision.DeniedBy = DeniedByDelegationCeiling
				decision.DenyCause = DenyCauseCeilingError
			} else if !ceilingAllowed {
				decision.Allowed = false
				decision.Reason = ceilingReason
				decision.DeniedBy = DeniedByDelegationCeiling
				decision.DenyCause = ceilingCause
				decision.adoptionRemediable = ceilingCause == DenyCauseCeilingUnrecorded && hopNote.adoptable
			}
		}
	}

	// ── Step 11: Finalize provenance ─────────────────────────────────
	// When Explain=true, include resolution errors and full provenance.
	// When Explain=false, strip provenance to minimal data for performance.
	if decision.Provenance != nil {
		if request.Explain {
			decision.Provenance.Errors = append(decision.Provenance.Errors, resolutionErrors...)
		} else {
			// Non-explain: keep only the minimal provenance (matched grant,
			// deny reason) to avoid serializing large structures.
			decision.Provenance = &DecisionProvenance{
				Permission:      decision.Provenance.Permission,
				Grants:          decision.Provenance.Grants,
				DenyReasons:     decision.Provenance.DenyReasons,
				Restrictions:    []RestrictionProvenance{},
				InactiveGrants:  []GrantDetail{},
				MembershipPaths: []MembershipPathDetail{},
			}
		}
	}

	if decision.Allowed && credential.Kind == CredentialKindUAT &&
		listRowReadWidened(request, permissionID, credential.Boundary, credential.Ceiling) {
		if decision.Reason == "" {
			decision.Reason = listRowReadReasonMarker
		} else {
			decision.Reason += " (" + listRowReadReasonMarker + ")"
		}
	}

	return decorateDecision(decision, request, principal, credential, auditPermissionID(request))
}

// DecideFromContext evaluates a request using the authenticated principal and
// credential metadata established by middleware.
func (a *AuthzService) DecideFromContext(ctx context.Context, resource Resource, action Action) Decision {
	return a.Decide(ctx, AuthzRequestFromContext(ctx, resource, action))
}

// AuthorizeReadBatch evaluates read enforcement decisions without converting
// authorization-store failures into denials. Capability projections retain
// their best-effort behavior; list enforcement must fail closed instead.
func (a *AuthzService) AuthorizeReadBatch(ctx context.Context, identity Identity, resources []Resource) ([]bool, error) {
	return a.authorizeReadBatch(ctx, identity, resources, false)
}

// AuthorizeListReadBatch is AuthorizeReadBatch for choosing the rows of an
// agent list: each decision is marked ListRow (see listRowReadCeiling) and
// still runs the full evaluation path, one decision per resource.
func (a *AuthzService) AuthorizeListReadBatch(ctx context.Context, identity Identity, resources []Resource) ([]bool, error) {
	return a.authorizeReadBatch(ctx, identity, resources, true)
}

func (a *AuthzService) authorizeReadBatch(ctx context.Context, identity Identity, resources []Resource, listRow bool) ([]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The memo caches successful loads only, so a store failure still
	// reaches the decision that observed it and fails closed.
	ctx = withAuthzInputMemo(ctx)
	if GetIdentityFromContext(ctx) != identity {
		ctx = contextWithIdentity(ctx, identity)
	}
	allowed := make([]bool, len(resources))
	for i := range resources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request := AuthzRequestFromContext(ctx, resources[i], ActionRead)
		request.ListRow = listRow
		allowed[i] = a.Decide(ctx, request).Allowed
	}
	return allowed, nil
}

func (a *AuthzService) authorizationPrincipals(ctx context.Context, identity Identity) ([]store.PrincipalRef, error) {
	// Normalize the identity type to canonical form (dev→user,
	// federated_agent→agent, etc.) using the single canonical normalization.
	normalizedType := NormalizePrincipalType(identity.Type())
	principals := []store.PrincipalRef{{Type: normalizedType, ID: identity.ID()}}
	var (
		groups []string
		err    error
	)
	switch normalizedType {
	case "user":
		groups, err = a.store.GetEffectiveGroups(ctx, identity.ID())
	case "agent":
		groups, err = a.store.GetEffectiveGroupsForAgent(ctx, identity.ID())
	default:
		return principals, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	for _, groupID := range groups {
		principals = append(principals, store.PrincipalRef{Type: "group", ID: groupID})
	}
	return principals, nil
}

// buildMembershipPathChains builds real membership path chains for group
// principals. For each group in the principal closure, it computes the chain
// from the requesting principal through intermediate groups to the target
// group. This replaces the single-element stub paths.
//
// The resulting paths are stored in membershipPaths using typed composite keys.
func (a *AuthzService) buildMembershipPathChains(
	ctx context.Context,
	directPrincipal store.PrincipalRef,
	principals []store.PrincipalRef,
	membershipPaths map[string][]string,
) {
	// Collect all group IDs in the closure.
	groupIDs := make(map[string]bool)
	for _, p := range principals {
		if p.Type == "group" {
			groupIDs[p.ID] = true
		}
	}
	if len(groupIDs) == 0 {
		return
	}

	// Build a child→parent adjacency from the group hierarchy.
	// For each group, get its parent groups and build the reverse mapping.
	childToParents := make(map[string][]string)
	for gid := range groupIDs {
		parents, err := a.store.GetParentGroups(ctx, gid)
		if err != nil {
			// Best-effort: on error, keep the single-element path.
			continue
		}
		for _, parentID := range parents {
			if groupIDs[parentID] {
				childToParents[gid] = append(childToParents[gid], parentID)
			}
		}
	}

	// Identify the principal's direct groups (groups that directly contain
	// the principal, not via other groups).
	directGroups := make(map[string]bool)
	switch directPrincipal.Type {
	case "user", "dev", "federated_user":
		members, err := a.store.GetUserGroups(ctx, directPrincipal.ID)
		if err == nil {
			for _, m := range members {
				if groupIDs[m.GroupID] {
					directGroups[m.GroupID] = true
				}
			}
		}
	case "agent", "federated_agent":
		// For agents, direct groups come from GetEffectiveGroupsForAgent.
		// We cannot distinguish direct vs transitive from the flat list,
		// so we use GetGroupMembership to check direct membership.
		for gid := range groupIDs {
			_, err := a.store.GetGroupMembership(ctx, gid, "agent", directPrincipal.ID)
			if err == nil {
				directGroups[gid] = true
			}
		}
	}

	// For each group, build a path from the direct principal through
	// intermediate groups. Uses BFS to find shortest path.
	principalKey := directPrincipal.Type + ":" + directPrincipal.ID
	for gid := range groupIDs {
		key := "group:" + gid
		if directGroups[gid] {
			// Directly a member: path is [principal, group].
			membershipPaths[key] = []string{principalKey, key}
			continue
		}

		// BFS from direct groups to find a path to gid.
		path := bfsGroupPath(directGroups, gid, childToParents)
		if len(path) > 0 {
			// Prepend the principal.
			fullPath := make([]string, 0, len(path)+1)
			fullPath = append(fullPath, principalKey)
			for _, p := range path {
				fullPath = append(fullPath, "group:"+p)
			}
			membershipPaths[key] = fullPath
		}
		// Otherwise keep the single-element fallback from the closure builder.
	}
}

// bfsGroupPath finds a path from any direct group to the target group using BFS
// over the child→parent adjacency. Returns the path as group IDs (without the
// principal), or nil if no path is found.
func bfsGroupPath(directGroups map[string]bool, target string, childToParents map[string][]string) []string {
	if directGroups[target] {
		return []string{target}
	}

	// Reverse the child→parent map to parent→child for BFS from target.
	parentToChildren := make(map[string][]string)
	for child, parents := range childToParents {
		for _, parent := range parents {
			parentToChildren[parent] = append(parentToChildren[parent], child)
		}
	}

	// BFS from the target backwards through the hierarchy to find a direct group.
	type queueItem struct {
		id   string
		path []string
	}
	visited := map[string]bool{target: true}
	queue := []queueItem{{id: target, path: []string{target}}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, child := range parentToChildren[current.id] {
			if visited[child] {
				continue
			}
			visited[child] = true
			newPath := make([]string, len(current.path)+1)
			copy(newPath, current.path)
			newPath[len(current.path)] = child

			if directGroups[child] {
				// Found a path. Reverse it to go from direct group to target.
				reversed := make([]string, len(newPath))
				for i, j := 0, len(newPath)-1; i < len(newPath); i, j = i+1, j-1 {
					reversed[i] = newPath[j]
				}
				return reversed
			}
			queue = append(queue, queueItem{id: child, path: newPath})
		}
	}

	return nil
}

// =============================================================================
// Kernel result → Decision conversion
// =============================================================================

// kernelDecisionToDecision converts a KernelDecision to the external Decision type.
func kernelDecisionToDecision(kd KernelDecision, permissionID string) Decision {
	d := Decision{
		Allowed: kd.Allowed,
	}
	if kd.Allowed {
		// R-4 fix: select a granting binding whose role actually contains
		// the requested permission (ContainsRequested==true). Previously,
		// GrantingBindings[0] was used unconditionally, which could name a
		// binding whose role does not contain the granted permission.
		if len(kd.Provenance.GrantingBindings) > 0 {
			gb := kd.Provenance.GrantingBindings[0]
			// Prefer a binding that contains the requested permission.
			for _, candidate := range kd.Provenance.GrantingBindings {
				if candidate.ContainsRequested {
					gb = candidate
					break
				}
			}
			d.Reason = "role binding grant"
			d.MatchedGrant = gb.RoleName
			d.RoleName = gb.RoleName
			d.BindingID = gb.BindingID
			d.Scope = gb.ScopeType
		} else {
			d.Reason = "kernel allow"
		}
	} else {
		if len(kd.Provenance.DenyReasons) > 0 {
			d.Reason = kd.Provenance.DenyReasons[0]
		} else {
			d.Reason = "default deny"
		}
	}

	// Populate DecisionProvenance from the kernel provenance.
	d.Provenance = buildDecisionProvenance(kd.Provenance)

	return d
}

// buildDecisionProvenance converts KernelProvenance to the external
// DecisionProvenance type.
func buildDecisionProvenance(kp KernelProvenance) *DecisionProvenance {
	dp := &DecisionProvenance{
		Permission:           kp.Permission,
		EffectivePermissions: kp.EffectivePermissions,
		DenyReasons:          kp.DenyReasons,
	}

	// Map granting (active) bindings.
	for _, gb := range kp.GrantingBindings {
		dp.Grants = append(dp.Grants, grantProvenanceToDetail(gb))
	}

	// Map rejected (inactive) bindings.
	for _, rb := range kp.RejectedCandidates {
		detail := grantProvenanceToDetail(rb)
		if len(rb.RejectReasons) > 0 {
			detail.InactiveReason = rb.RejectReasons[0]
		}
		dp.InactiveGrants = append(dp.InactiveGrants, detail)
	}

	// Map restrictions with boundary metadata.
	for _, rr := range kp.Restrictions {
		rp := RestrictionProvenance{
			Kind:          rr.Kind,
			Description:   rr.Description,
			Applied:       rr.Applied,
			Detail:        rr.Detail,
			BoundaryName:  rr.BoundaryName,
			BoundaryID:    rr.BoundaryID,
			BoundaryScope: formatBoundaryScope(rr.BoundaryScopeType, rr.BoundaryScopeID),
		}
		// Separate credential/status restrictions from boundary restrictions.
		if rr.Kind == "credential_scope" || rr.Kind == "delegation_ceiling" || rr.Kind == "suspension" {
			dp.StatusRestrictions = append(dp.StatusRestrictions, rp)
		} else {
			dp.Restrictions = append(dp.Restrictions, rp)
		}
	}

	// Collect unique membership paths from all evaluated bindings.
	seenPaths := make(map[string]bool)
	collectPath := func(gp GrantProvenance) {
		if len(gp.MembershipPath) == 0 {
			return
		}
		targetKey := gp.PrincipalType + ":" + gp.PrincipalID
		if seenPaths[targetKey] {
			return
		}
		seenPaths[targetKey] = true

		kind := "direct"
		if gp.PrincipalType == "group" {
			if len(gp.MembershipPath) > 2 {
				kind = "group_closure"
			} else {
				kind = "group_membership"
			}
		}
		dp.MembershipPaths = append(dp.MembershipPaths, MembershipPathDetail{
			TargetID: targetKey,
			Path:     gp.MembershipPath,
			Kind:     kind,
		})
	}
	for _, gb := range kp.GrantingBindings {
		collectPath(gb)
	}
	for _, rb := range kp.RejectedCandidates {
		collectPath(rb)
	}

	// Ensure non-nil slices for JSON serialization.
	if dp.Grants == nil {
		dp.Grants = []GrantDetail{}
	}
	if dp.InactiveGrants == nil {
		dp.InactiveGrants = []GrantDetail{}
	}
	if dp.Restrictions == nil {
		dp.Restrictions = []RestrictionProvenance{}
	}
	if dp.MembershipPaths == nil {
		dp.MembershipPaths = []MembershipPathDetail{}
	}

	return dp
}

// grantProvenanceToDetail converts a kernel GrantProvenance to a GrantDetail.
func grantProvenanceToDetail(gp GrantProvenance) GrantDetail {
	return GrantDetail{
		BindingID:         gp.BindingID,
		RoleID:            gp.RoleID,
		RoleName:          gp.RoleName,
		ScopeType:         gp.ScopeType,
		ScopeID:           gp.ScopeID,
		PrincipalType:     gp.PrincipalType,
		PrincipalID:       gp.PrincipalID,
		ContainsRequested: gp.ContainsRequested,
		MembershipPath:    gp.MembershipPath,
		Permissions:       gp.Permissions,
		RejectReasons:     gp.RejectReasons,
	}
}

// formatBoundaryScope formats scope type and ID into a human-readable string.
func formatBoundaryScope(scopeType, scopeID string) string {
	if scopeType == "" {
		return ""
	}
	if scopeID == "" {
		return scopeType
	}
	return scopeType + ":" + scopeID
}

// =============================================================================
// Relationship grants (replacing legacy bypasses)
// =============================================================================

// Relationship grants are evaluated by evaluateRelationshipCandidates
// (authz_relationship_rules.go).

// =============================================================================
// Agent synthetic binding construction
// =============================================================================

// buildAgentSyntheticBindings creates synthetic project-scoped CandidateBindings
// from an agent's JWT token scopes. This translates the agent's token-based
// authority into kernel-evaluable bindings so the agent's project-scoped
// permissions flow through the standard evaluation pipeline.
func (a *AuthzService) buildAgentSyntheticBindings(agent AgentIdentity) ([]CandidateBinding, map[string]*RolePermissions) {
	scopes := effectiveAgentScopes(agent)
	if len(scopes) == 0 {
		return nil, nil
	}

	// Map scopes to permission IDs.
	permIDs := agentScopesToPermissionIDs(scopes)
	if len(permIDs) == 0 {
		return nil, nil
	}

	synthRoleID := "synthetic:agent-jwt:" + agent.ID()
	permSet := make(map[string]struct{}, len(permIDs))
	for _, id := range permIDs {
		permSet[id] = struct{}{}
	}

	roleDefs := map[string]*RolePermissions{
		synthRoleID: {
			RoleID:      synthRoleID,
			RoleName:    "agent-jwt-scope",
			ScopeType:   ScopeTypeProject,
			Permissions: permSet,
		},
	}

	candidates := []CandidateBinding{{
		BindingID:        "synthetic:agent-project:" + agent.ID(),
		RoleDefinitionID: synthRoleID,
		PrincipalType:    "agent",
		PrincipalID:      agent.ID(),
		ScopeType:        ScopeTypeProject,
		ScopeID:          agent.ProjectID(),
	}}

	return candidates, roleDefs
}

// isLegacyPreSplitAgentJWT reports whether identity is a genuine pre-split
// hub-signed agent JWT: one that ValidateAgentToken actually verified and
// found carried no scope_schema claim on the wire (AgentTokenClaims.
// legacyScopeSchema; see CurrentAgentScopeSchema's doc comment for why that
// flag, not a zero ScopeSchema value, is the signal). Direct construction of
// an AgentTokenClaims literal never sets legacyScopeSchema, so every
// in-process identity built that way — a stored-record wrapper, a
// scheduled-dispatch creator identity, a synthetic secret-resolution
// identity, and so on — reports false here regardless of which scopes it
// carries. A non-hub-JWT AgentIdentity (federated, or any future kind) also
// reports false: this compatibility exists only to grandfather a token this
// hub's own signer actually minted before the split, and an identity that
// never went through that signer was never granted anything under the
// pre-split combined scope to begin with.
func isLegacyPreSplitAgentJWT(identity AgentIdentity) bool {
	wrapped, ok := identity.(*agentIdentityWrapper)
	return ok && wrapped.AgentTokenClaims != nil && wrapped.legacyScopeSchema
}

// effectiveAgentScopes is the single place the ptone/scion#2339
// compatibility rule applies, so every scope-level consumer agrees on what a
// legacy token means. For a genuine pre-split hub JWT
// (isLegacyPreSplitAgentJWT) that holds ScopeAgentCreate but not
// ScopeAgentSAAssign, it appends ScopeAgentSAAssign — the same single
// combined scope granted both permissions when the token was minted, so the
// token keeps authorizing what it authorized then. Every other identity's
// scopes pass through unchanged, including a current-schema token that
// deliberately holds ScopeAgentCreate without ScopeAgentSAAssign (a future
// per-scope-ceiling-filtered mint must not be widened back here).
//
// Every scope-level consumer — the synthetic project-scoped grant
// (buildAgentSyntheticBindings), the credential-scope restriction
// (agentScopeRestriction), and agent-to-agent delegation admission
// (canAgentDelegateToAgent) — must call this instead of agent.Scopes()
// directly, so a permission one of them grants can never be taken back by
// another reading a different, stale scope list.
func effectiveAgentScopes(agent AgentIdentity) []AgentTokenScope {
	scopes := agent.Scopes()
	if !isLegacyPreSplitAgentJWT(agent) {
		return scopes
	}
	hasCreate, hasAssign := false, false
	for _, s := range scopes {
		switch s {
		case ScopeAgentCreate:
			hasCreate = true
		case ScopeAgentSAAssign:
			hasAssign = true
		}
	}
	if !hasCreate || hasAssign {
		return scopes
	}
	extended := make([]AgentTokenScope, len(scopes), len(scopes)+1)
	copy(extended, scopes)
	return append(extended, ScopeAgentSAAssign)
}

// agentScopesToPermissionIDs maps agent JWT token scopes to canonical
// permission IDs by looking up each scope in the permissions registry. It is
// a pure registry lookup with no compatibility logic of its own — callers
// evaluating an agent's actual authority must pass effectiveAgentScopes(agent)
// rather than agent.Scopes() so the ptone/scion#2339 compatibility rule is
// applied exactly once, upstream of this function.
func agentScopesToPermissionIDs(scopes []AgentTokenScope) []string {
	scopeSet := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		scopeSet[string(s)] = true
	}
	var ids []string
	for _, p := range permissions.Registry {
		for _, s := range p.AgentScopes {
			if scopeSet[s] {
				ids = append(ids, p.ID)
				break
			}
		}
	}
	return ids
}

// =============================================================================
// Restriction builders
// =============================================================================

// ceilingRestriction builds a kernel Restriction from a UAT's permission
// ceiling. It defers entirely to FrozenPermissionCeiling.Allows: an empty,
// zero-value, or unknown-version ceiling denies every permission rather
// than lifting the restriction, and no scope ever implies another.
func ceilingRestriction(ceiling permissions.FrozenPermissionCeiling) Restriction {
	return Restriction{
		Kind:        "credential_scope",
		Description: "UAT credential permission ceiling",
		Check: func(permissionID string) bool {
			return ceiling.Allows(permissionID)
		},
	}
}

// agentScopeRestriction builds a kernel Restriction from agent JWT token
// scopes. Only permissions the agent's effective scopes grant are allowed —
// computed via effectiveAgentScopes + agentScopesToPermissionIDs, the exact
// pipeline step 5b's synthetic binding uses, so the two always agree on what
// a legacy pre-split token (ptone/scion#2339) authorizes — with one
// exception: permissions.PermissionGCPServiceAccountUse is decided against
// the specific resource this restriction was built for
// (agentGCPServiceAccountUseScopeMatch), never against effectiveAgentScopes's
// map below -- see gcpServiceAccountUseBinding's doc comment for why a static
// list cannot express a per-instance token scope. Every other permission is
// decided by that map alone.
func agentScopeRestriction(agent AgentIdentity, resource Resource) Restriction {
	scopes := effectiveAgentScopes(agent)
	if len(scopes) == 0 {
		// No scopes: deny everything (fail closed).
		return Restriction{
			Kind:        "credential_scope",
			Description: "agent JWT has no scopes (fail closed)",
		}
	}
	allowed := make(map[string]struct{})
	for _, id := range agentScopesToPermissionIDs(scopes) {
		allowed[id] = struct{}{}
	}
	return Restriction{
		Kind:        "credential_scope",
		Description: "agent JWT scope restriction",
		Check: func(permissionID string) bool {
			if permissionID == permissions.PermissionGCPServiceAccountUse {
				return agentGCPServiceAccountUseScopeMatch(agent, permissionID, resource)
			}
			// agent.read has no agent scope. The launcher status read
			// (authz_launcher_read.go) admits it for one stored target.
			if permissionID == permissionAgentRead && launcherStatusReadHolds(agent, resource, ActionRead, permissionID) {
				return true
			}
			_, ok := allowed[permissionID]
			return ok
		},
	}
}

// agentGCPServiceAccountUseScopeMatch reports whether agent's JWT carries the
// exact per-instance token scope that the
// gcp_service_account.use / gcp_service_account pair requires. It is the one
// place that pair is decided, consulted by both the request-local synthetic
// grant (gcpServiceAccountUseBinding) and the agent-scope restriction above,
// so the two cannot drift apart.
//
// It denies -- never falls back to a broader test -- when permissionID is not
// permissions.PermissionGCPServiceAccountUse, when resource.Type is not
// permissions.ResourceGCPServiceAccount, when resource.ID is empty, or when
// the JWT lacks the exact scope for that ID. Matching is agent.HasScope
// (exact string equality) only: a prefix, wildcard or another account's scope
// does not satisfy it, and the scope string itself is never re-derived --
// GCPTokenScopeForSA (agenttoken.go) is the single source of that format.
func agentGCPServiceAccountUseScopeMatch(agent AgentIdentity, permissionID string, resource Resource) bool {
	if agent == nil || permissionID != permissions.PermissionGCPServiceAccountUse {
		return false
	}
	if resource.Type != permissions.ResourceGCPServiceAccount || resource.ID == "" {
		return false
	}
	return agent.HasScope(GCPTokenScopeForSA(resource.ID))
}

// gcpServiceAccountUseBinding returns a request-local CandidateBinding and its
// RolePermissions when agentGCPServiceAccountUseScopeMatch holds for this
// exact request; otherwise (nil, nil).
//
// The binding is scoped ScopeTypeSystem so it applies regardless of whether
// the decided gcp_service_account row is itself hub-, project- or
// user-scoped (scopeApplies always admits system scope) -- scope containment
// here is not the point, the per-request resource-ID match already is.
//
// It is built fresh inside decide() on every call: never cached, and never added
// to the target-agnostic agentScopesToPermissionIDs (Step 5b's project-scoped
// binding) or to the static AgentScopes set that agentScopeRestriction builds.
// That static set also drives intersectCredentialCaveats/CanDelegate, which
// decides what an agent may delegate to something it creates -- a grant scoped to
// one resource ID must never be read there as general, delegable
// gcp_service_account.use authority. CanDelegate calls agentScopeRestriction with
// a zero Resource, so agentGCPServiceAccountUseScopeMatch always denies there and
// gcp_service_account.use is never in an agent's delegable set.
func gcpServiceAccountUseBinding(agent AgentIdentity, permissionID string, resource Resource) (*CandidateBinding, *RolePermissions) {
	if !agentGCPServiceAccountUseScopeMatch(agent, permissionID, resource) {
		return nil, nil
	}
	id := "synthetic:agent-gcp-service-account-use:" + agent.ID() + ":" + resource.ID
	role := &RolePermissions{
		RoleID:      id,
		RoleName:    "agent-jwt-gcp-service-account-use",
		ScopeType:   ScopeTypeSystem,
		Permissions: map[string]struct{}{permissions.PermissionGCPServiceAccountUse: {}},
	}
	cb := &CandidateBinding{
		BindingID:        id,
		RoleDefinitionID: id,
		PrincipalType:    "agent",
		PrincipalID:      agent.ID(),
		ScopeType:        ScopeTypeSystem,
	}
	return cb, role
}

// loadAccessConstraintRestrictions loads active access constraints from the
// store and converts them to kernel restrictions. On a load error it fails
// closed by returning a deny-all restriction. Decide (no error return),
// getEffectivePermissions and getProjectScopedPermissions call this form and
// keep that behaviour. A caller with its own error return that needs a load
// failure to surface as an error instead should call
// accessConstraintRestrictions directly; see SystemAuthorityProof and
// CanMintSelector's mint-time helpers.
func (a *AuthzService) loadAccessConstraintRestrictions(
	ctx context.Context,
	closure map[string]struct{},
	resource ResourceContext,
) []Restriction {
	restrictions, err := a.accessConstraintRestrictions(ctx, closure, resource)
	if err != nil {
		// R-1 fix: deny (fail closed) when constraint loading errors.
		// The design is explicit: "Store or group resolution errors fail
		// closed." Returning a deny-all restriction ensures no over-grant.
		a.logger.Warn("failed to load access constraints (fail-closed)", "error", err)
		return []Restriction{{
			Kind:        "access_constraint_error",
			Description: "constraint loading failed (fail-closed)",
			// nil Check denies everything.
		}}
	}
	return restrictions
}

// accessConstraintRestrictions is loadAccessConstraintRestrictions's
// error-returning form: it loads active access constraints from the store
// and converts them to kernel restrictions, but returns a load error to the
// caller instead of converting it into a deny-all restriction. Use when the
// caller has its own error return and a load failure must surface as an
// error rather than an ordinary denial, as on the ProjectAdmissionForClass
// and CanMintSelector paths. Every other caller should use
// loadAccessConstraintRestrictions.
func (a *AuthzService) accessConstraintRestrictions(
	ctx context.Context,
	closure map[string]struct{},
	resource ResourceContext,
) ([]Restriction, error) {
	// R-1 fix: page through all constraints instead of capping at 200.
	constraints, err := a.loadAllAccessConstraints(ctx)
	if err != nil {
		return nil, err
	}
	if len(constraints) == 0 {
		return nil, nil
	}

	// Convert store constraints to hub AccessConstraint and filter.
	var hubConstraints []*AccessConstraint
	for _, sc := range constraints {
		hc := storeToHubAccessConstraint(sc)
		if hc != nil {
			hubConstraints = append(hubConstraints, hc)
		}
	}

	// Normalize all closure keys so that dev/federated variants match
	// the canonical "user"/"agent" types used in constraint subjects.
	// The closure already uses typed "type:id" keys; normalization ensures
	// consistency regardless of how the closure was built.
	normalizedClosure := normalizeClosureTypes(closure)

	scopeType := ""
	scopeID := ""
	if resource.ProjectID != "" {
		scopeType = ScopeTypeProject
		scopeID = resource.ProjectID
	} else {
		scopeType = ScopeTypeSystem
	}

	applicable := FilterApplicableConstraints(
		hubConstraints, normalizedClosure,
		scopeType, scopeID,
	)

	// R1 fix: capture time once to avoid TOCTOU between ConstraintsToRestrictions
	// and the enrichment loop. Two separate time.Now() calls could diverge at
	// a constraint's active-window boundary, breaking the positional 1:1
	// correspondence.
	now := time.Now()
	restrictions := ConstraintsToRestrictions(applicable, now)

	// Enrich restrictions with boundary metadata. ConstraintsToRestrictions
	// builds the Description with constraint name/ID but does not populate the
	// structured boundary fields added for provenance explain. We match each
	// restriction back to its source constraint by position (1:1 correspondence
	// with the applicable list, skipping nil/inactive which ConstraintsToRestrictions
	// also skips — guaranteed identical because we use the same `now` value).
	ri := 0
	for _, c := range applicable {
		if c == nil || !c.IsActive(now) {
			continue
		}
		if ri < len(restrictions) {
			restrictions[ri].BoundaryName = c.Name
			restrictions[ri].BoundaryID = c.ID
			restrictions[ri].BoundaryScopeType = c.Scope.Type
			restrictions[ri].BoundaryScopeID = c.Scope.ID
		}
		ri++
	}

	return restrictions, nil
}

// loadAllAccessConstraints loads all access constraints by paging through
// the store. R-1 fix: the previous call used a fixed limit of 200 which
// silently truncated constraints beyond that threshold.
func (a *AuthzService) loadAllAccessConstraints(ctx context.Context) ([]*store.AccessConstraint, error) {
	if cache := mintEligibilityCacheFromContext(ctx); cache != nil {
		cache.mu.Lock()
		if cache.constraintsLoaded {
			all, err := cache.constraints, cache.constraintsErr
			cache.mu.Unlock()
			return all, err
		}
		cache.mu.Unlock()

		all, err := a.loadAllAccessConstraintsUncached(ctx)

		cache.mu.Lock()
		cache.constraintsLoaded = true
		cache.constraints, cache.constraintsErr = all, err
		cache.mu.Unlock()
		return all, err
	}

	// Within an install phase (see authz_request_inputs.go), the constraint
	// slot is consumed only by decide's own access-constraint restriction
	// (accessConstraintRestrictions, called from decide) and by
	// ResolveListScopes (applyListScopeConstraints). Every other consumer is
	// reached only on a masked ctx:
	//   - decide's relationship candidates run under maskAllAuthzMemo, which
	//     covers executionProjectAdmission -> ProjectAdmissionForClass ->
	//     SystemAuthorityProof and relationshipSourceDelegationHolds -> the
	//     delegation chain walk. The one exception is the project-access
	//     stage (relationshipProjectAccessStage, stage 2c): it evaluates the
	//     requester's own project access, so decide passes it the request
	//     context, and its ProjectTargetAdmission -> ProjectMembershipEvidence
	//     and SystemAuthorityProof -> accessConstraintRestrictions read the
	//     requester's memoized principals, bindings and constraint slot;
	//   - the delegation ceiling call site and the chain walk itself run
	//     under maskAuthzInputs, which covers getEffectivePermissions and
	//     userRelationshipAuthority;
	//   - CanMintSelector runs under maskAllAuthzMemo.
	// The access-constraint impact computation (computePrincipalImpact), the
	// material runtime's SystemAuthorityProof call and the handlers that call
	// getEffectivePermissions are outside every install site. Adding a new
	// consumer requires updating this comment and the store-call recorder
	// test that asserts no masked call observes a memo.
	//
	// On a done ctx the memo is bypassed entirely: today's uncached call
	// is made with today's ctx and its result returned verbatim, and
	// nothing is stored, so post-cancellation behaviour is today's by
	// construction.
	if m := authzInputMemoFromContext(ctx); m != nil && ctx.Err() == nil {
		m.mu.Lock()
		if m.constraints != nil {
			all := *m.constraints
			m.mu.Unlock()
			return all, nil
		}
		m.mu.Unlock()

		all, err := a.loadAllAccessConstraintsUncached(ctx)
		if err != nil {
			// Errors are never memoized: a failed load is returned to the
			// caller and not stored, so the next decision retries.
			return nil, err
		}

		m.mu.Lock()
		if m.constraints == nil {
			m.constraints = &all
		}
		stored := *m.constraints
		m.mu.Unlock()
		return stored, nil
	}

	return a.loadAllAccessConstraintsUncached(ctx)
}

// loadAllAccessConstraintsUncached is loadAllAccessConstraints's body, split
// out so the mint-eligibility cache wrapper above never duplicates this
// logic. Every caller outside a CanMintSelector call (e.g. the ordinary
// Decide path) reaches this directly, with no cache in context, and behaves
// exactly as before.
func (a *AuthzService) loadAllAccessConstraintsUncached(ctx context.Context) ([]*store.AccessConstraint, error) {
	const pageSize = 500
	var all []*store.AccessConstraint
	offset := 0
	for {
		page, err := a.store.ListAccessConstraints(ctx, pageSize, offset)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		offset += len(page)
	}
	return all, nil
}

// normalizeClosureTypes normalizes the principal types in a typed closure map.
// It maps dev/federated variants to their canonical types (user/agent) so
// constraint subjects using "user" or "agent" match all equivalent types.
//
// Keys are expected in "type:id" format. Keys without a colon are passed
// through unchanged.
func normalizeClosureTypes(closure map[string]struct{}) map[string]struct{} {
	normalized := make(map[string]struct{}, len(closure))
	for key := range closure {
		if idx := strings.IndexByte(key, ':'); idx >= 0 {
			keyType := key[:idx]
			keyID := key[idx+1:]
			normType := NormalizePrincipalType(keyType)
			normalized[normType+":"+keyID] = struct{}{}
		} else {
			normalized[key] = struct{}{}
		}
	}
	return normalized
}

// =============================================================================
// Permission resolution
// =============================================================================

// Permission resolution for requests without an explicit permission lives
// in authz_permission_resolver.go (resolveResourcePermission).

// =============================================================================
// Helper functions
// =============================================================================

func isAgentPrincipal(kind PrincipalKind) bool {
	return kind == PrincipalKindAgent || kind == PrincipalKindFederatedAgent
}

func isUserPrincipal(kind PrincipalKind) bool {
	return kind == PrincipalKindUser || kind == PrincipalKindDev || kind == PrincipalKindFederatedUser
}

// isRecognizedPrincipalKind reports whether kind is one of the classifications
// principalContextForIdentity can produce for a known identity type. An empty
// kind (a nil identity, or an identity of an unrecognized concrete type) is
// not recognized, and neither is any string a caller might supply that isn't
// one of these constants.
func isRecognizedPrincipalKind(kind PrincipalKind) bool {
	switch kind {
	case PrincipalKindUser, PrincipalKindAgent, PrincipalKindFederatedUser,
		PrincipalKindFederatedAgent, PrincipalKindFederatedService, PrincipalKindBroker, PrincipalKindDev:
		return true
	default:
		return false
	}
}

// isRecognizedCredentialKind reports whether kind is one of the
// classifications credentialContextForIdentity can produce for a known
// identity type. An empty kind (a nil identity, or an identity of an
// unrecognized concrete type) is not recognized.
func isRecognizedCredentialKind(kind CredentialKind) bool {
	switch kind {
	case CredentialKindInteractive, CredentialKindUAT, CredentialKindAgentJWT,
		CredentialKindFederation, CredentialKindBroker, CredentialKindDev,
		CredentialKindHubDelivery:
		return true
	default:
		return false
	}
}

// suppliedCredentialCompatible reports whether a caller-supplied
// Credential.Kind may stand in for an identity's own derived classification.
// It is admitted only when:
//
//  1. supplied equals derived; or
//  2. principal.Kind is PrincipalKindUser, derived is CredentialKindInteractive
//     (a plain local user, not a *ScopedUserIdentity — that derives UAT and so
//     only ever reaches case 1), and supplied is CredentialKindUAT — a
//     narrowing overlay used by recorded/reconstructed UAT evaluation. Every
//     UAT scope, boundary, and live-authority check still applies to the
//     supplied credential itself; this predicate only admits it past this
//     gate; or
//  3. the same principal/derived precondition as case 2, supplied is
//     CredentialKindBroker, and ctx proves BrokerAuthMiddleware (or its
//     audited variant) resolved this exact principal on behalf of the
//     authenticated broker named by supplied — see
//     brokerOnBehalfOfAuthorizes. The broker credential remains effective for
//     the rest of Decide, so route/credential restrictions still apply.
//
// Nothing else passes: dev is not in the UAT/broker exception (a used UAT or
// broker-on-behalf-of grant represents its local user owner, not dev's
// token-issuing power), CredentialKindHubDelivery is compatible by equality
// only (case 1) — it is not in the user/interactive UAT overlay and not in
// broker on-behalf-of — and an unrecognized derived identity never reaches
// this predicate — it denies earlier, on the unrecognized-kind check. This
// consumes ctx provenance for case 3, not just the two kinds, so a caller
// cannot admit the broker exception by supplying CredentialKindBroker alone.
func suppliedCredentialCompatible(ctx context.Context, principal PrincipalContext, derived, supplied CredentialContext) bool {
	if supplied.Kind == derived.Kind {
		return true
	}
	if principal.Kind != PrincipalKindUser || derived.Kind != CredentialKindInteractive {
		return false
	}
	switch supplied.Kind {
	case CredentialKindUAT:
		return true
	case CredentialKindBroker:
		return brokerOnBehalfOfAuthorizes(ctx, principal, supplied)
	default:
		return false
	}
}

// brokerOnBehalfOfAuthorizes reports whether ctx proves that the authenticated
// broker acted on behalf of exactly the principal being evaluated, naming
// exactly the broker identified by supplied. It requires ALL of:
//   - the broker identity marker (contextWithBrokerIdentity), installed for
//     every HMAC-authenticated broker request;
//   - the dedicated BrokerOnBehalfOf marker, installed only after HMAC
//     verification and a successful resolveOnBehalfOf — never by a bare or
//     invalid header;
//   - supplied.ID equal to both the marker's BrokerID and the ctx broker
//     identity's own ID;
//   - the ctx broker identity's Type() equal to the expected broker
//     credential type ("broker"), and supplied.Type equal to it too;
//   - the ctx effective identity (GetIdentityFromContext) has the same ID as
//     principal — the on-behalf-of substitution named exactly this principal,
//     not merely some local user.
//
// A header alone, a fabricated CredentialKindBroker with no marker, or a
// context authenticated for a different principal never qualifies: this reads
// ctx provenance the middleware sets, not the supplied kind by itself.
func brokerOnBehalfOfAuthorizes(ctx context.Context, principal PrincipalContext, supplied CredentialContext) bool {
	broker := GetBrokerIdentityFromContext(ctx)
	// isNilIdentity, not broker == nil: BrokerIdentity embeds Identity, so a
	// typed-nil concrete broker identity (see isNilIdentity) is a non-nil
	// interface value and would otherwise reach broker.Type() below.
	if isNilIdentity(broker) || broker.Type() != "broker" || supplied.Type != "broker" {
		return false
	}
	obo, ok := BrokerOnBehalfOfFromContext(ctx)
	if !ok || obo.BrokerID == "" {
		return false
	}
	// isNilIdentity, not obo.Broker == nil, for the same reason: obo.Broker is
	// a BrokerIdentity and a typed-nil value here would otherwise reach
	// obo.Broker.ID() below.
	if obo.BrokerID != broker.ID() || isNilIdentity(obo.Broker) || obo.Broker.ID() != broker.ID() {
		return false
	}
	effective := GetIdentityFromContext(ctx)
	if effective == nil || principal.ID == "" || effective.ID() != principal.ID {
		return false
	}
	return supplied.ID == broker.ID()
}

// principalContextForIdentity classifies identity into its PrincipalKind by
// concrete type — a type assertion switch, never identity.Type(). Type() is
// informational only (see its doc comment): any concrete type, including one
// this package has not classified, is free to return "user", "agent", or any
// other string, and must not thereby be admitted as if it were the classified
// type that string names. Every known concrete production identity type has
// an explicit arm. The default arm covers everything else: a nil identity,
// an unrecognized concrete type, and a package-hub test fake that has not
// opted into explicitIdentityClassification. It leaves Kind empty, which
// Decide's fail-closed classification check denies rather than letting it
// fall through to any implicit default. A typed-nil concrete identity (see
// isNilIdentity) takes the same empty-context path as a nil interface: this
// check runs before identity.ID() or the type switch below touch it, since a
// nil concrete pointer panics on either.
func principalContextForIdentity(identity Identity) PrincipalContext {
	if isNilIdentity(identity) {
		return PrincipalContext{}
	}
	principal := PrincipalContext{ID: identity.ID(), Identity: identity}
	switch identity.(type) {
	case *AuthenticatedUser, *ScopedUserIdentity:
		principal.Kind = PrincipalKindUser
	case *DevUser:
		principal.Kind = PrincipalKindDev
	case *agentIdentityWrapper, *storedAgentIdentity, *peerAgentIdentity, *explainAgentIdentity, *hubDeliveryIdentity:
		principal.Kind = PrincipalKindAgent
	case *FederatedUserIdentity:
		principal.Kind = PrincipalKindFederatedUser
	case *FederatedAgentIdentity:
		principal.Kind = PrincipalKindFederatedAgent
	case *FederatedServiceIdentity:
		principal.Kind = PrincipalKindFederatedService
	case *brokerIdentityImpl:
		principal.Kind = PrincipalKindBroker
	default:
		if c, ok := identity.(explicitIdentityClassification); ok {
			principal.Kind, _ = c.authzClassification()
		}
	}
	return principal
}

// credentialContextForIdentity classifies identity into its CredentialKind by
// concrete type, for the same reason principalContextForIdentity does: a
// caller-defined or otherwise unclassified type's Type() string must never
// stand in for classification. The *ScopedUserIdentity check stays first: any
// UAT-backed identity is CredentialKindUAT regardless of what its underlying
// UserIdentity's concrete type is. Every other known concrete identity type
// has its own explicit arm, including *AuthenticatedUser for a plain
// interactive session. The default arm covers a nil identity, an
// unrecognized concrete type, and a package-hub test fake that has not opted
// into explicitIdentityClassification: it returns an empty Kind rather than
// CredentialKindInteractive, so Decide's fail-closed classification check
// denies it instead of treating an unknown identity as an ordinary
// interactive session. A typed-nil concrete identity (see isNilIdentity)
// takes this same empty-context path, checked before the type switch below
// touches it; see the *ScopedUserIdentity case below for the one exception.
func credentialContextForIdentity(identity Identity) CredentialContext {
	// A typed-nil *ScopedUserIdentity returns Kind == CredentialKindUAT with a
	// zero Ceiling, not the empty context the other typed-nil types get. A
	// caller may supply this CredentialContext independently of
	// Principal.Identity, so Decide's missing-principal check does not cover
	// it; an empty Kind would skip Decide's
	// suppliedCredentialCompatible/ceiling check, while a UAT Kind with a
	// zero Ceiling denies every permission (ptone/scion#2143).
	if v, ok := identity.(*ScopedUserIdentity); ok && v == nil {
		return CredentialContext{Kind: CredentialKindUAT}
	}
	if isNilIdentity(identity) {
		return CredentialContext{}
	}
	switch v := identity.(type) {
	case *ScopedUserIdentity:
		// The typed-nil case is handled by the guard above, before this
		// switch is reached, so v is guaranteed non-nil here and
		// v.Boundary() cannot dereference a nil receiver.
		boundary := v.Boundary()
		cc := CredentialContext{Kind: CredentialKindUAT, ID: v.CredentialID(), ProjectID: boundary.ProjectID, Boundary: &boundary, Scopes: v.ScopedScopes(), Ceiling: v.Ceiling()}
		// Carry the descriptive decoration, if ValidateToken attached one,
		// through to the credential context. This is the single copy point;
		// decoration is never otherwise derived here. Decoration() already
		// returns a deep copy, so this assignment cannot alias the
		// identity's stored value.
		cc.Decoration = v.Decoration()
		return cc
	case *AuthenticatedUser:
		return CredentialContext{Kind: CredentialKindInteractive, Type: identity.Type()}
	case *DevUser:
		return CredentialContext{Kind: CredentialKindDev}
	case *agentIdentityWrapper:
		credential := CredentialContext{Kind: CredentialKindAgentJWT}
		if v.AgentTokenClaims != nil {
			credential.ID = v.Claims.ID
		}
		return credential
	case *storedAgentIdentity, *peerAgentIdentity, *explainAgentIdentity:
		return CredentialContext{Kind: CredentialKindAgentJWT}
	case *hubDeliveryIdentity:
		// No ID: a delivery credential carries no JTI.
		return CredentialContext{Kind: CredentialKindHubDelivery}
	case *FederatedUserIdentity, *FederatedAgentIdentity, *FederatedServiceIdentity:
		return CredentialContext{Kind: CredentialKindFederation, Type: identity.Type()}
	case *brokerIdentityImpl:
		return CredentialContext{Kind: CredentialKindBroker}
	default:
		if c, ok := identity.(explicitIdentityClassification); ok {
			_, kind := c.authzClassification()
			return CredentialContext{Kind: kind, Type: identity.Type()}
		}
		return CredentialContext{}
	}
}

// decorateDecision finalizes a Decision with principal/credential attribution,
// the audit-recorded permission ID, and the request's Actor and Purpose.
// Every decide return path goes through it, so Actor and Purpose are
// recorded on every decision (and on its provenance when present). permID
// is a value from auditPermissionID(request), never independently derived
// here.
func decorateDecision(decision Decision, request AuthzRequest, principal PrincipalContext, credential CredentialContext, permID string) Decision {
	decision.Actor = request.Actor
	decision.Purpose = request.Purpose
	if decision.Provenance != nil {
		decision.Provenance.Actor = request.Actor
		decision.Provenance.Purpose = request.Purpose
	}
	decision.PrincipalKind = principal.Kind
	decision.PrincipalID = principal.ID
	decision.principalDecorated = true
	decision.CredentialID = credential.ID
	decision.CredentialType = credential.Type
	decision.CredentialKind = string(credential.Kind)
	decision.PermissionID = permID
	if decision.MatchedPolicy == "" {
		decision.MatchedPolicy = decision.BindingID
	}
	if decision.MatchedGrant == "" {
		decision.MatchedGrant = decision.RoleName
	}
	return decision
}

// auditPermissionID returns the permission ID decision audit records: the
// exact caller-supplied AuthzRequest.Permission, and only when it is a
// canonical ID in the permissions registry. It is never derived from
// Resource/Action, and never an unregistered string — ruling Q7 forbids
// certifying an ID that does not exist in the catalog. Empty when the
// caller supplied no Permission, or supplied one the registry does not
// recognize.
func auditPermissionID(request AuthzRequest) string {
	if request.Permission != "" && isKnownPermission(request.Permission) {
		return request.Permission
	}
	return ""
}

// enforceUATConstraints applies the bearer gate (evaluateBearerGate) to a
// request presented with a ScopedUserIdentity, using the identity's own
// boundary and ceiling and no collection-level evidence. It returns a deny
// Decision when the request falls outside the token's boundary, ceiling, or
// the holder's current project access, and nil otherwise. permissionID is
// the canonical permission Decide resolved for the request.
func (a *AuthzService) enforceUATConstraints(ctx context.Context, principal PrincipalContext, scoped *ScopedUserIdentity, resource Resource, action Action, permissionID string) *Decision {
	in := bearerGateInputs{missing: true}
	if scoped != nil {
		in = bearerGateInputs{boundary: scoped.Boundary(), ceiling: scoped.Ceiling()}
	}
	return a.evaluateBearerGate(ctx, principal, in, resource, TargetScopeEvidence{}, action, permissionID, nil, nil)
}

// canAccessAsAncestor checks if the principal appears in the resource's ancestry chain.
// This provides transitive access: any ancestor (human or agent) in the creation
// chain can access the resource.
func canAccessAsAncestor(principalID string, resource Resource) bool {
	for _, id := range resource.Ancestry {
		if id == principalID {
			return true
		}
	}
	return false
}

// projectIDForResource returns the project ID a resource belongs to, or "" if the
// resource is not project-scoped. A project resource maps to its own ID; any
// resource with ParentType="project" maps to its ParentID.
func projectIDForResource(r Resource) string {
	if r.Type == "project" {
		return r.ID
	}
	if r.ParentType == "project" {
		return r.ParentID
	}
	return ""
}

// IsSystemAdmin checks whether the given user has a system-scoped super-admin
// role binding. Uses the batched query path.
func (a *AuthzService) IsSystemAdmin(ctx context.Context, userID string) bool {
	return a.hasActiveSystemRole(ctx, userID, store.SystemRoleSuperAdmin)
}

// IsHubAdmin checks whether the given user has a system-scoped hub-admin
// role binding.
func (a *AuthzService) IsHubAdmin(ctx context.Context, userID string) bool {
	return a.hasActiveSystemRole(ctx, userID, store.SystemRoleHubAdmin)
}

func (a *AuthzService) hasActiveSystemRole(ctx context.Context, userID, roleName string) bool {
	if userID == "" {
		return false
	}
	now := time.Now()
	principals := []store.PrincipalRef{{Type: "user", ID: userID}}
	groups, err := a.store.GetEffectiveGroups(ctx, userID)
	if err == nil {
		for _, gid := range groups {
			principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
		}
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		return false
	}
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeSystem {
			continue
		}
		if !isBindingActive(b, now) {
			continue
		}
		rd, err := a.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		if rd.Name == roleName {
			return true
		}
	}
	return false
}

// isBindingActive checks whether a store RoleBinding is currently active
// based on its notBefore/expiresAt fields. R-2 fix.
func isBindingActive(b *store.RoleBinding, now time.Time) bool {
	if b.NotBefore != nil && now.Before(*b.NotBefore) {
		return false
	}
	if b.ExpiresAt != nil && now.After(*b.ExpiresAt) {
		return false
	}
	return true
}

// hubMembersSlug is the slug of the seeded hub-members group.
const hubMembersSlug = "hub-members"

// isCurrentHubMember reports whether the user is a current member of the
// hub-members group.
func (a *AuthzService) isCurrentHubMember(ctx context.Context, userID string) bool {
	if userID == "" {
		return false
	}
	group, err := a.store.GetGroupBySlug(ctx, hubMembersSlug)
	if err != nil {
		return false
	}
	_, err = a.store.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	return err == nil
}

// storeToHubAccessConstraint converts a store AccessConstraint to a hub
// AccessConstraint. Returns nil if the store constraint is nil.
// Runs Validate() on subject and scope and marks the constraint as degraded
// (with a warning log) if validation fails on stored records, rather than
// silently discarding them.
func storeToHubAccessConstraint(sc *store.AccessConstraint) *AccessConstraint {
	if sc == nil {
		return nil
	}
	hc := &AccessConstraint{
		ID:                 sc.ID,
		Name:               sc.Name,
		MaximumPermissions: sc.MaximumPermissions,
		Disabled:           sc.Disabled,
		Revision:           sc.Revision,
		Purpose:            sc.Purpose,
		UpdatedBy:          sc.UpdatedBy,
		CreatedBy:          sc.CreatedBy,
		CreatedAt:          sc.CreatedAt,
		UpdatedAt:          sc.UpdatedAt,
	}
	// Map subject.
	hc.Subject = SubjectSelector{
		Kind: SubjectKind(sc.SubjectKind),
	}
	if sc.SubjectPrincipalType != nil {
		hc.Subject.PrincipalType = *sc.SubjectPrincipalType
	}
	if sc.SubjectPrincipalID != nil {
		hc.Subject.PrincipalID = *sc.SubjectPrincipalID
	}
	if sc.SubjectGroupID != nil {
		hc.Subject.GroupID = *sc.SubjectGroupID
	}
	// Map scope.
	hc.Scope = ConstraintScopeRef{
		Type: sc.ScopeType,
		ID:   sc.ScopeID,
	}
	// Map condition/time window.
	if sc.NotBefore != nil {
		hc.Condition.NotBefore = *sc.NotBefore
	}
	if sc.ExpiresAt != nil {
		hc.Condition.ExpiresAt = *sc.ExpiresAt
	}

	// Validate converted subject and scope. Invalid stored records are
	// marked as degraded for B7's ResolutionHealth, not dropped — this
	// preserves record inclusion (does not silently drop) while surfacing
	// data quality issues via the Degraded flag.
	if err := hc.Subject.Validate(); err != nil {
		slog.Warn("degraded access constraint: invalid stored subject",
			"constraint_id", sc.ID, "constraint_name", sc.Name, "error", err)
		hc.Degraded = true
	}
	if err := hc.Scope.Validate(); err != nil {
		slog.Warn("degraded access constraint: invalid stored scope",
			"constraint_id", sc.ID, "constraint_name", sc.Name, "error", err)
		hc.Degraded = true
	}

	return hc
}

// isProjectOwner checks whether the user has an active, direct project-owner
// RoleBinding in the given project. Group-derived bindings are excluded per
// the "direct user only" design invariant for ownership roles, and activation
// lifecycle (NotBefore/ExpiresAt) is enforced to match the Mine resolver's
// semantics.
//
// C0-CONTAINMENT: F-QA-02 — this function restricts to owner-only. The
// pre-existing isProjectOwnerOrAdmin allowed admins to manage membership,
// which the C0 exit gate disallows. Contract decision to relax: Phase 1
// governance matrix.
func (a *AuthzService) isProjectOwner(ctx context.Context, userID, projectID string) bool {
	if userID == "" || projectID == "" {
		return false
	}

	// Query direct-user-only bindings (no group expansion).
	bindings, err := a.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil || len(bindings) == 0 {
		return false
	}

	// Resolve the project-owner role definition once per call.
	ownerRoleDef, err := a.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil || ownerRoleDef == nil {
		return false
	}

	now := time.Now()
	for _, rb := range bindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID {
			continue
		}
		if rb.RoleDefinitionID != ownerRoleDef.ID {
			continue
		}
		// Activation lifecycle: binding must be currently active.
		if rb.NotBefore != nil && now.Before(*rb.NotBefore) {
			continue
		}
		if rb.ExpiresAt != nil && now.After(*rb.ExpiresAt) {
			continue
		}
		return true
	}
	return false
}

// isProjectOwnerOrAdmin reports whether the user has project-owner or
// project-admin role in the given project. Uses the batched query path.
func (a *AuthzService) isProjectOwnerOrAdmin(ctx context.Context, userID, projectID string) bool {
	if userID == "" || projectID == "" {
		return false
	}

	// 1. Direct user membership (existing behavior).
	membership, err := a.store.GetProjectMembership(ctx, projectID, userID)
	if err == nil && membership != nil {
		if membership.Role == store.ProjectRoleOwner || membership.Role == store.ProjectRoleAdmin {
			return true
		}
	}

	// 2. Group-expanded: check if any of the user's groups have owner/admin
	//    role binding on this project.
	groupIDs, err := a.store.GetEffectiveGroups(ctx, userID)
	if err != nil || len(groupIDs) == 0 {
		return false
	}

	// Build principals for batched query.
	var principals []store.PrincipalRef
	for _, gid := range groupIDs {
		principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		return false
	}
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeProject || b.ScopeID != projectID {
			continue
		}
		rd, err := a.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		if rd.Name == store.ProjectRoleOwner || rd.Name == store.ProjectRoleAdmin {
			return true
		}
	}
	return false
}

// getEffectivePermissions resolves the set of permission IDs granted to a
// principal via role bindings. Uses the batched query path.
func (a *AuthzService) getEffectivePermissions(ctx context.Context, principalType, principalID string, scopeType, scopeID string) ([]string, error) {
	// Normalize principal type: dev/federated variants resolve groups
	// through the same paths as user/agent and must be treated identically
	// for constraint matching and group expansion.
	normalizedType := NormalizePrincipalType(principalType)

	// Build principals: direct principal + group-expanded.
	principals := []store.PrincipalRef{{Type: normalizedType, ID: principalID}}
	var groupIDs []string
	var err error
	switch normalizedType {
	case store.RoleBindingPrincipalUser:
		groupIDs, err = a.store.GetEffectiveGroups(ctx, principalID)
	case store.RoleBindingPrincipalAgent:
		groupIDs, err = a.store.GetEffectiveGroupsForAgent(ctx, principalID)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// Fail closed: group resolution failure means we cannot evaluate
		// group_closure constraints correctly. Return empty permissions
		// rather than silently skipping group constraints.
		a.logger.Warn("failed to get effective groups for permission expansion (fail-closed)",
			"principalType", principalType, "principalID", principalID, "error", err)
		return nil, fmt.Errorf("group resolution failed (fail-closed): %w", err)
	}
	for _, gid := range groupIDs {
		principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
	}

	// Use batched query.
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		return nil, err
	}

	// R-2 fix: filter bindings through activation checks (notBefore/expiresAt)
	// and apply AccessConstraint restrictions. Previously, expired/future
	// bindings were counted and constraints were never intersected.
	now := time.Now()
	seen := make(map[string]bool)
	var result []string
	for _, b := range bindings {
		// Filter by scope.
		if b.ScopeType == store.RoleScopeProject {
			if scopeType != store.RoleScopeProject || b.ScopeID != scopeID {
				continue
			}
		}
		// R-2: Check activation — skip expired and not-yet-active bindings.
		cb := &CandidateBinding{
			BindingID: b.ID,
		}
		if b.NotBefore != nil {
			cb.NotBefore = *b.NotBefore
		}
		if b.ExpiresAt != nil {
			cb.ExpiresAt = *b.ExpiresAt
		}
		activation := evaluateActivation(cb, now)
		if !activation.Active {
			continue
		}

		rd, err := a.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			a.logger.Warn("failed to resolve role definition for binding",
				"binding_id", b.ID, "role_definition_id", b.RoleDefinitionID, "error", err)
			continue
		}
		for _, permID := range rd.Permissions {
			if !seen[permID] {
				seen[permID] = true
				result = append(result, permID)
			}
		}
	}

	// R-2: Apply AccessConstraint intersection. Load constraints and remove
	// permissions that are excluded by any applicable constraint.
	if len(result) > 0 {
		closure := make(map[string]struct{}, len(principals))
		for _, p := range principals {
			closure[p.Type+":"+p.ID] = struct{}{}
		}
		resourceCtx := ResourceContext{}
		if scopeType == store.RoleScopeProject {
			resourceCtx.ProjectID = scopeID
		}
		restrictions := a.loadAccessConstraintRestrictions(ctx, closure, resourceCtx)
		if len(restrictions) > 0 {
			var filtered []string
			for _, permID := range result {
				blocked := false
				for _, r := range restrictions {
					if r.Check == nil || !r.Check(permID) {
						blocked = true
						break
					}
				}
				if !blocked {
					filtered = append(filtered, permID)
				}
			}
			result = filtered
		}
	}

	return result, nil
}

// getProjectScopedPermissions returns only the permissions that the principal
// holds through project-scoped role bindings for the given project. System-
// scoped bindings are excluded: hub or system authority must not enlarge a
// project-scoped token. Not called from production code; retained because
// authz_boundary_test.go pins its binding-resolution parity with
// projectScopedPermissionsStrict.
//
// The method retains group-expanded principals, activation-window filtering,
// and AccessConstraint reduction — exactly as getEffectivePermissions does —
// but only considers bindings where ScopeType == "project" && ScopeID == projectID.
// Shares its group/binding resolution (projectScopedGrants, authz_boundary.go)
// with projectScopedPermissionsStrict, CanMintSelector's flat-role mint path's
// error-returning counterpart; this function keeps a deny-all restriction on
// a constraint-table load failure rather than returning an error.
func (a *AuthzService) getProjectScopedPermissions(ctx context.Context, principalType, principalID, projectID string) ([]string, error) {
	perms, closure, err := a.projectScopedGrants(ctx, principalType, principalID, projectID)
	if err != nil {
		return nil, err
	}
	if len(perms) == 0 {
		return perms, nil
	}
	restrictions := a.loadAccessConstraintRestrictions(ctx, closure, ResourceContext{ProjectID: projectID})
	return applyRestrictions(perms, restrictions), nil
}
