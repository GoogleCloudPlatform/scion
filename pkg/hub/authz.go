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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Action represents an authorization action.
type Action string

// Action constants for authorization checks.
const (
	ActionCreate       Action = "create"
	ActionRead         Action = "read"
	ActionUpdate       Action = "update"
	ActionDelete       Action = "delete"
	ActionList         Action = "list"
	ActionManage       Action = "manage"
	ActionStart        Action = "start"
	ActionStop         Action = "stop"
	ActionMessage      Action = "message"
	ActionAttach       Action = "attach"
	ActionLifecycle    Action = "lifecycle"
	ActionPortAccess   Action = "port_access"
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
	// by skillScopeResource/skillResource. It lets the agent creator
	// user-skill relationship grant (agentCreatorUserSkillGrant) match the
	// same column the skill list predicate filters on. Empty otherwise.
	ScopeUserID string
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
type CredentialKind string

const (
	CredentialKindInteractive CredentialKind = "interactive"
	CredentialKindUAT         CredentialKind = "uat"
	CredentialKindAgentJWT    CredentialKind = "agent_jwt"
	CredentialKindFederation  CredentialKind = "federation"
	CredentialKindBroker      CredentialKind = "broker"
	CredentialKindDev         CredentialKind = "dev"
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
// ProjectID and Scopes are caveats for scoped bearer credentials.
type CredentialContext struct {
	Kind      CredentialKind
	ID        string
	Type      string
	ProjectID string
	Scopes    []string

	// E.1 descriptive credential metadata. Decoration is additive,
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

	// Actor and Purpose describe who initiated the operation and why, when
	// that differs from Principal (for example a delivery performed for a
	// target agent). They are recorded on every decision (and in its
	// provenance when present) for audit and never contribute to the
	// decision.
	Actor   *DecisionActor
	Purpose string

	// AlwaysAudit forces Decide's single audit exit to emit a decision audit
	// record for this request regardless of the allow-sampling rate
	// (AuthzService.DecisionAuditSampleRate). Deny decisions are always
	// audited already; this exists for callers that know in advance they
	// need an allow decision audited unconditionally too. See also
	// Decision.AlwaysAudit, its counterpart for a branch inside decide's
	// body that only learns this partway through evaluation. It never
	// changes the authorization result.
	AlwaysAudit bool
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
	CredentialID   string
	CredentialType string
	CredentialKind string
	ExplainTrace   []DecisionStep `json:"explainTrace,omitempty"`

	// Actor and Purpose echo AuthzRequest.Actor/Purpose on every decision.
	// Audit-only: they never contribute to Allowed.
	Actor   *DecisionActor `json:"actor,omitempty"`
	Purpose string         `json:"purpose,omitempty"`

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

	// Provenance contains the full decision provenance when Explain=true.
	// For non-explain requests, this is populated with minimal data
	// (matched grant and deny reason).
	Provenance *DecisionProvenance `json:"provenance,omitempty"`
}

// DenyCause is a structural tag for a subset of deny reasons that callers
// need to distinguish without string-matching Reason. It is deliberately
// small and closed: only the causes a caller actually branches on get a
// value here, everything else is the zero value ("").
type DenyCause string

const (
	// DenyCauseCeilingOrphaned marks a delegation-ceiling deny where the
	// delegator (the principal that created the agent, directly or
	// transitively) no longer resolves at all — e.g. its user was deleted.
	DenyCauseCeilingOrphaned DenyCause = "ceiling_orphaned"

	// DenyCauseCeilingDelegatorLacksPermission marks a delegation-ceiling
	// deny where the delegator still exists but no longer holds the
	// permission being exercised.
	DenyCauseCeilingDelegatorLacksPermission DenyCause = "ceiling_delegator_lacks_permission"

	// DenyCauseCeilingError marks a delegation-ceiling deny caused by a
	// transient or internal fault (e.g. a store error) rather than a
	// policy fact about the delegator. Callers should treat this like an
	// ordinary denial, not surface it as a specific reason.
	DenyCauseCeilingError DenyCause = "ceiling_error"
)

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
}

// NewAuthzService creates a new AuthzService.
func NewAuthzService(s store.Store, logger *slog.Logger) *AuthzService {
	return &AuthzService{
		store:                   s,
		logger:                  logger,
		DecisionAuditSampleRate: 1.0,
		relationshipResolver:    NewRelationshipGrantResolver(s),
	}
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
func (a *AuthzService) decide(ctx context.Context, request AuthzRequest) Decision {
	principal := request.Principal
	if principal.Identity == nil {
		return decorateDecision(Decision{Allowed: false, Reason: "missing principal"}, request, principal, request.Credential, auditPermissionID(request))
	}
	derivedPrincipal := principalContextForIdentity(principal.Identity)
	if principal.Kind != "" && principal.Kind != derivedPrincipal.Kind {
		return decorateDecision(Decision{Allowed: false, Reason: "principal kind does not match identity"}, request, derivedPrincipal, request.Credential, auditPermissionID(request))
	}
	principal.Kind = derivedPrincipal.Kind
	if principal.ID == "" {
		principal.ID = derivedPrincipal.ID
	}

	credential := request.Credential
	if credential.Kind == "" {
		credential = credentialContextForIdentity(principal.Identity)
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

	// ── Step 1: UAT project constraint (pre-kernel gate) ──────────────
	// UAT tokens are project-scoped. Resources outside the token's project
	// are denied before kernel evaluation. This is a credential constraint,
	// not a bypass — it can only narrow, never widen.
	if credential.Kind == CredentialKindUAT {
		if user, ok := principal.Identity.(UserIdentity); ok {
			if scoped, ok := user.(*ScopedUserIdentity); ok {
				if denied := a.enforceUATConstraints(scoped, request.Resource, request.Action); denied != nil {
					return decorateDecision(*denied, request, principal, credential, auditPermissionID(request))
				}
			}
		}
	}

	// Track resolution errors for explain provenance.
	var resolutionErrors []string

	// ── Step 2: Resolve principal closure ─────────────────────────────
	principals, err := a.authorizationPrincipals(ctx, principal.Identity)
	if err != nil {
		a.logger.Warn("failed to resolve authorization principals",
			"principal_id", principal.ID, "error", err)
		errMsg := "principal resolution error: " + err.Error()
		if request.Explain {
			resolutionErrors = append(resolutionErrors, errMsg)
		}
		d := Decision{Allowed: false, Reason: "principal resolution error (fail-closed)"}
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
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		a.logger.Warn("failed to load role bindings",
			"principal_id", principal.ID, "error", err)
		errMsg := "binding resolution error: " + err.Error()
		if request.Explain {
			resolutionErrors = append(resolutionErrors, errMsg)
		}
		d := Decision{Allowed: false, Reason: "binding resolution error (fail-closed)"}
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
	roleDefIDs := collectRoleDefinitionIDs(bindings)
	roleDefs, err := a.loadRoleDefinitions(ctx, roleDefIDs)
	if err != nil {
		a.logger.Warn("failed to load role definitions",
			"principal_id", principal.ID, "error", err)
		errMsg := "role resolution error: " + err.Error()
		if request.Explain {
			resolutionErrors = append(resolutionErrors, errMsg)
		}
		d := Decision{Allowed: false, Reason: "role resolution error (fail-closed)"}
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
	if isAgentPrincipal(principal.Kind) {
		if agent, ok := principal.Identity.(AgentIdentity); ok && agent.ProjectID() != "" {
			synthCandidates, synthRoles := a.buildAgentSyntheticBindings(agent)
			candidates = append(candidates, synthCandidates...)
			for k, v := range synthRoles {
				roleDefs[k] = v
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
	if request.Resource.Type == "skill" && isAgentPrincipal(principal.Kind) {
		if agent, ok := principal.Identity.(AgentIdentity); ok {
			cb, role := agentSkillCatalogBinding(agent)
			candidates = append(candidates, cb)
			roleDefs[cb.RoleDefinitionID] = role
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

	// 7a. UAT credential scope restriction.
	if credential.Kind == CredentialKindUAT && len(credential.Scopes) > 0 {
		restrictions = append(restrictions, uatScopeRestriction(credential.Scopes))
	}

	// 7b. Agent JWT scope restriction.
	if isAgentPrincipal(principal.Kind) {
		if agent, ok := principal.Identity.(AgentIdentity); ok {
			restrictions = append(restrictions, agentScopeRestriction(agent))
		}
	}

	// 7c. Access constraints (AC1).
	acRestrictions := a.loadAccessConstraintRestrictions(ctx, closure, resourceCtx)
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

	// ── Step 9: Relationship candidates ───────────────────────────────
	// On a kernel deny, named relationships (owner, ancestor, progeny,
	// hub-member assign, creator user skill) are evaluated as typed
	// candidates through the common stages in authz_relationship_rules.go:
	// relationship policy, hub-attested ancestry, relationship fact, source
	// activity, and the same restrictions the kernel applied (7a/7b/7c).
	// With Explain, candidates are also evaluated on a kernel allow so the
	// provenance lists them.
	decision := kernelDecisionToDecision(kernelResult, permissionID)
	if !kernelResult.Allowed || request.Explain {
		rel := a.evaluateRelationshipCandidates(ctx, principal, request.Resource, request.Action, permissionID, restrictions, !request.Explain)
		if !kernelResult.Allowed {
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

	// ── Step 10: Agent delegation ceiling (post-decision) ────────────
	// Applies to ALL allowed decisions regardless of grant source
	// (kernel or relationship grant). C-1 fix: previously only ran on
	// kernel-allowed decisions because Step 9 returned early.
	if decision.Allowed && isAgentPrincipal(principal.Kind) {
		if agent, ok := principal.Identity.(AgentIdentity); ok {
			if getDelegationCeilingCache(ctx) == nil {
				ctx = contextWithDelegationCeilingCache(ctx)
			}
			var ceilingCause DenyCause
			ceilingAllowed, ceilingReason, ceilingErr := a.checkDelegationCeiling(ctx, request, agent.ID(), nil, &ceilingCause)
			if ceilingErr != nil {
				if !isReadOnlyOperation(request.Action) {
					decision.Allowed = false
					decision.Reason = "delegation ceiling check failed (fail-closed): " + ceilingErr.Error()
					decision.DenyCause = DenyCauseCeilingError
				}
			} else if !ceilingAllowed {
				decision.Allowed = false
				decision.Reason = ceilingReason
				decision.DenyCause = ceilingCause
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if GetIdentityFromContext(ctx) != identity {
		ctx = contextWithIdentity(ctx, identity)
	}
	allowed := make([]bool, len(resources))
	for i := range resources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		decision := a.DecideFromContext(ctx, resources[i], ActionRead)
		allowed[i] = decision.Allowed
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
	scopes := agent.Scopes()
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

// agentScopesToPermissionIDs maps agent JWT token scopes to canonical permission IDs
// by looking up each scope in the permissions registry.
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

// uatScopeRestriction builds a kernel Restriction from UAT credential scopes.
// Only permissions whose agent scope strings match the UAT scopes are allowed.
func uatScopeRestriction(scopes []string) Restriction {
	// UAT scopes are in "resource:action" format. Map them to permission IDs.
	scopeSet := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		scopeSet[s] = true
		for _, implied := range permissions.LegacyUATScopeImplications[s] {
			scopeSet[implied] = true
		}
	}
	// Build the set of allowed permission IDs from the scopes.
	allowed := make(map[string]struct{})
	for _, p := range permissions.Registry {
		scopeKey := p.Resource + ":" + p.Action
		if scopeSet[scopeKey] {
			allowed[p.ID] = struct{}{}
		}
	}
	return Restriction{
		Kind:        "credential_scope",
		Description: "UAT credential scope restriction",
		Check: func(permissionID string) bool {
			_, ok := allowed[permissionID]
			return ok
		},
	}
}

// agentScopeRestriction builds a kernel Restriction from agent JWT token scopes.
// Only permissions that map to the agent's declared scopes are allowed.
func agentScopeRestriction(agent AgentIdentity) Restriction {
	scopes := agent.Scopes()
	if len(scopes) == 0 {
		// No scopes: deny everything (fail closed).
		return Restriction{
			Kind:        "credential_scope",
			Description: "agent JWT has no scopes (fail closed)",
		}
	}
	scopeSet := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		scopeSet[string(s)] = true
	}
	allowed := make(map[string]struct{})
	for _, p := range permissions.Registry {
		for _, s := range p.AgentScopes {
			if scopeSet[s] {
				allowed[p.ID] = struct{}{}
				break
			}
		}
	}
	return Restriction{
		Kind:        "credential_scope",
		Description: "agent JWT scope restriction",
		Check: func(permissionID string) bool {
			_, ok := allowed[permissionID]
			return ok
		},
	}
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

func principalContextForIdentity(identity Identity) PrincipalContext {
	if identity == nil {
		return PrincipalContext{}
	}
	principal := PrincipalContext{ID: identity.ID(), Identity: identity}
	switch identity.Type() {
	case "user":
		principal.Kind = PrincipalKindUser
	case "agent":
		principal.Kind = PrincipalKindAgent
	case "federated_user":
		principal.Kind = PrincipalKindFederatedUser
	case "federated_agent":
		principal.Kind = PrincipalKindFederatedAgent
	case "federated_service":
		principal.Kind = PrincipalKindFederatedService
	case "broker":
		principal.Kind = PrincipalKindBroker
	case "dev":
		principal.Kind = PrincipalKindDev
	}
	return principal
}

func credentialContextForIdentity(identity Identity) CredentialContext {
	if identity == nil {
		return CredentialContext{}
	}
	if scoped, ok := identity.(*ScopedUserIdentity); ok {
		cc := CredentialContext{Kind: CredentialKindUAT, ID: scoped.CredentialID(), ProjectID: scoped.ScopedProjectID(), Scopes: scoped.ScopedScopes()}
		// E.1: carry the descriptive decoration, if ValidateToken attached
		// one, through to the credential context. This is the single copy
		// point named in the E.1 design (plan §2.2); decoration is never
		// otherwise derived here. Decoration() already returns a deep copy,
		// so this assignment cannot alias the identity's stored value.
		cc.Decoration = scoped.Decoration()
		return cc
	}
	switch identity.Type() {
	case "agent":
		credential := CredentialContext{Kind: CredentialKindAgentJWT}
		if agent, ok := identity.(*agentIdentityWrapper); ok && agent.AgentTokenClaims != nil {
			credential.ID = agent.Claims.ID
		}
		return credential
	case "federated_user", "federated_agent", "federated_service":
		return CredentialContext{Kind: CredentialKindFederation, Type: identity.Type()}
	case "broker":
		return CredentialContext{Kind: CredentialKindBroker}
	case "dev":
		return CredentialContext{Kind: CredentialKindDev}
	default:
		return CredentialContext{Kind: CredentialKindInteractive, Type: identity.Type()}
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

// enforceUATConstraints checks the project and scope restrictions carried by a
// ScopedUserIdentity (produced from a UAT). Returns a deny Decision if the
// request falls outside the token's allowed project or scopes, nil otherwise.
func (a *AuthzService) enforceUATConstraints(scoped *ScopedUserIdentity, resource Resource, action Action) *Decision {
	// Enforce project constraint: the resource must belong to the token's project.
	projectID := scoped.ScopedProjectID()
	if resource.Type == "project" {
		if resource.ID != projectID {
			return &Decision{Allowed: false, Reason: "token not scoped for this project"}
		}
	} else if resource.ParentType == "project" && resource.ParentID != projectID {
		return &Decision{Allowed: false, Reason: "token not scoped for this project"}
	} else if resource.Type != "" && resource.Type != "project" && resource.ParentType != "project" {
		// Resource has no project association (hub-level).
		// UATs are project-scoped and must not access hub-level resources.
		return &Decision{Allowed: false, Reason: "token not scoped for hub-level resources"}
	}

	// Enforce scope constraint: the resource:action must be in the token's scopes.
	scope := resource.Type + ":" + string(action)
	if !scoped.HasScope(scope) {
		return &Decision{Allowed: false, Reason: "token does not have scope: " + scope}
	}

	return nil
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
// scoped bindings are excluded. This is the A2 issuer-ceiling resolver: hub
// or system authority must not enlarge a project-scoped token.
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
