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
	"log/slog"
	"sync/atomic"
	"time"

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
	ActionPortAccess   Action = "port_access"
	ActionRegister     Action = "register"
	ActionAddMember    Action = "addMember"
	ActionRemoveMember Action = "removeMember"
	ActionDispatch     Action = "dispatch"
	ActionStopAll      Action = "stop_all"
	ActionVerify       Action = "verify"
	ActionMint         Action = "mint"
	// ActionAssign covers binding a resource to a principal that will act with
	// it — currently attaching a GCP service account to an agent. Declared here
	// so policies can be written against it; the assignment call sites still
	// check ActionRead and are converted separately.
	ActionAssign         Action = "assign"
	ActionInvite         Action = "invite"
	ActionSuspend        Action = "suspend"
	ActionPromote        Action = "promote"
	ActionClone          Action = "clone"
	ActionExecute        Action = "execute"
	ActionSetMessageMode Action = "set_message_mode"
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
}

// AuthzRequest carries both the acting principal and the credential caveats.
type AuthzRequest struct {
	Principal  PrincipalContext
	Credential CredentialContext
	Resource   Resource
	Action     Action
	Permission string // Canonical permission ID (e.g., "hub.settings.read"); when set, role binding evaluation uses this instead of Resource+Action.
	Explain    bool   // When true, collect step-by-step trace in Decision
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
	PolicyID       string // ID of the matched policy (if any)
	PolicyName     string // Name of the matched policy (if any)
	Scope          string // Scope level that decided (hub, project, resource)
	MatchedGrant   string // Audit-ready matched grant identifier
	MatchedPolicy  string // Audit-ready matched policy identifier
	PrincipalKind  PrincipalKind
	CredentialID   string
	CredentialType string
	CredentialKind string
	ExplainTrace   []DecisionStep `json:"explainTrace,omitempty"`
}

// EvaluationDetail provides detailed info for the evaluate endpoint.
type EvaluationDetail struct {
	Scope             string   `json:"scope"`
	PoliciesEvaluated int      `json:"policiesEvaluated"`
	Matched           bool     `json:"matched"`
	EffectiveGroups   []string `json:"effectiveGroups,omitempty"`
}

// DecisionAuditEmitter is an interface for emitting decision audit records.
type DecisionAuditEmitter interface {
	EmitDecisionAudit(ctx context.Context, record *store.DecisionAuditRecord)
}

// AuthzService provides authorization checks using the policy evaluation engine.
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
}

// NewAuthzService creates a new AuthzService.
func NewAuthzService(s store.Store, logger *slog.Logger) *AuthzService {
	return &AuthzService{
		store:                   s,
		logger:                  logger,
		DecisionAuditSampleRate: 1.0,
	}
}

// SetDecisionAuditEmitter configures the decision audit emitter.
func (a *AuthzService) SetDecisionAuditEmitter(emitter DecisionAuditEmitter) {
	a.decisionAuditEmitter = emitter
}

// CheckAccess evaluates whether the given identity is allowed to perform
// the specified action on the resource. Legacy callers that do not know a
// canonical permission ID pass an empty string; the role-binding evaluation
// step is skipped when no permission is supplied.
func (a *AuthzService) CheckAccess(ctx context.Context, identity Identity, resource Resource, action Action) Decision {
	return a.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   resource,
		Action:     action,
	})
}

// Decide evaluates an authorization request. Credential caveats are applied
// before legacy principal baselines, so a scoped credential can never inherit
// an admin or super-admin bypass from its principal.
func (a *AuthzService) Decide(ctx context.Context, request AuthzRequest) Decision {
	principal := request.Principal
	if principal.Identity == nil {
		return decorateDecision(Decision{Allowed: false, Reason: "missing principal"}, principal, request.Credential)
	}
	derivedPrincipal := principalContextForIdentity(principal.Identity)
	if principal.Kind != "" && principal.Kind != derivedPrincipal.Kind {
		return decorateDecision(Decision{Allowed: false, Reason: "principal kind does not match identity"}, derivedPrincipal, request.Credential)
	}
	principal.Kind = derivedPrincipal.Kind
	if principal.ID == "" {
		principal.ID = derivedPrincipal.ID
	}

	credential := request.Credential
	if credential.Kind == "" {
		credential = credentialContextForIdentity(principal.Identity)
	}

	// Initialize explain trace if requested
	var trace []DecisionStep
	if request.Explain {
		trace = make([]DecisionStep, 0)
		trace = append(trace, DecisionStep{Step: "principal_resolved", Detail: string(principal.Kind) + ":" + principal.ID})
		credDetail := string(credential.Kind) + " credential"
		if credential.ID != "" {
			credDetail += ", id=" + credential.ID
		}
		trace = append(trace, DecisionStep{Step: "credential_checked", Detail: credDetail})
	}

	var decision Decision
	switch principal.Kind {
	case PrincipalKindUser, PrincipalKindDev, PrincipalKindFederatedUser:
		user, ok := principal.Identity.(UserIdentity)
		if !ok {
			decision = Decision{Allowed: false, Reason: "invalid user identity"}
			break
		}
		if credential.Kind == CredentialKindUAT {
			user = NewScopedUserIdentity(user, credential.ProjectID, credential.Scopes)
		}
		decision = a.checkAccessForUser(ctx, user, request.Resource, request.Action, request.Permission)
	case PrincipalKindAgent, PrincipalKindFederatedAgent:
		agent, ok := principal.Identity.(AgentIdentity)
		if !ok {
			decision = Decision{Allowed: false, Reason: "invalid agent identity"}
			break
		}
		// Ensure delegation ceiling cache is available for this request.
		if getDelegationCeilingCache(ctx) == nil {
			ctx = contextWithDelegationCeilingCache(ctx)
		}
		decision = a.checkAccessForAgent(ctx, agent, request.Resource, request.Action)

		// Step 2.5: Live delegation ceiling (Phase 1G).
		// If the agent was allowed by policy/role, verify the delegation chain
		// still supports it. The ceiling intersects with, not replaces, the
		// existing permission determination.
		if decision.Allowed {
			var ceilingExplain *[]DecisionStep
			if request.Explain && trace != nil {
				ceilingExplain = &trace
			}
			ceilingAllowed, ceilingReason, ceilingErr := a.checkDelegationCeiling(ctx, request, agent.ID(), ceilingExplain)
			if ceilingErr != nil {
				// Fail-closed for all non-read-only operations
				if !isReadOnlyOperation(request.Action) {
					decision.Allowed = false
					decision.Reason = "delegation ceiling check failed (fail-closed): " + ceilingErr.Error()
				}
				// For read-only operations (read, list, verify), log and allow
			} else if !ceilingAllowed {
				decision.Allowed = false
				decision.Reason = ceilingReason
			}
		}
	case PrincipalKindFederatedService:
		decision = Decision{Allowed: false, Reason: "federated service identities are not supported"}
	case PrincipalKindBroker:
		decision = Decision{Allowed: false, Reason: "broker identities are not supported by policy authorization"}
	default:
		decision = Decision{Allowed: false, Reason: "unknown identity type"}
	}

	// Attach explain trace
	if request.Explain && trace != nil {
		// Check admin bypass
		adminDetail := "not admin, skipped"
		if user, ok := principal.Identity.(UserIdentity); ok && IsUnscopedLocalPlatformAdmin(user) {
			adminDetail = "admin bypass active"
		}
		trace = append(trace, DecisionStep{Step: "admin_bypass_checked", Detail: adminDetail})

		resultDetail := "denied: " + decision.Reason
		if decision.Allowed {
			resultDetail = "allowed by " + decision.Reason
			if decision.PolicyName != "" {
				resultDetail += " (policy: " + decision.PolicyName + ")"
			}
		}
		trace = append(trace, DecisionStep{Step: "decision", Detail: resultDetail})
		decision.ExplainTrace = trace
	}

	result := decorateDecision(decision, principal, credential)

	// Emit decision audit if emitter is configured
	if a.decisionAuditEmitter != nil {
		a.emitDecisionAudit(ctx, request, result)
	}

	return result
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
	if user, ok := identity.(UserIdentity); ok && IsUnscopedLocalPlatformAdmin(user) {
		return makeAllowed(len(resources)), nil
	}

	principals, err := a.authorizationPrincipals(ctx, identity)
	if err != nil {
		return nil, err
	}
	if _, err := a.store.GetPoliciesForPrincipals(ctx, principals); err != nil {
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
		if decision.Reason == "policy lookup error" {
			return nil, errors.New("authorization policy lookup failed")
		}
		allowed[i] = decision.Allowed
	}
	return allowed, nil
}

func (a *AuthzService) authorizationPrincipals(ctx context.Context, identity Identity) ([]store.PrincipalRef, error) {
	principals := []store.PrincipalRef{{Type: identity.Type(), ID: identity.ID()}}
	var (
		groups []string
		err    error
	)
	switch identity.Type() {
	case "user", "dev", "federated_user":
		principals[0].Type = "user"
		groups, err = a.store.GetEffectiveGroups(ctx, identity.ID())
	case "agent", "federated_agent":
		principals[0].Type = "agent"
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

func makeAllowed(n int) []bool {
	allowed := make([]bool, n)
	for i := range allowed {
		allowed[i] = true
	}
	return allowed
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
		return CredentialContext{Kind: CredentialKindUAT, ID: scoped.CredentialID(), ProjectID: scoped.ScopedProjectID(), Scopes: scoped.ScopedScopes()}
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

func decorateDecision(decision Decision, principal PrincipalContext, credential CredentialContext) Decision {
	decision.PrincipalKind = principal.Kind
	decision.CredentialID = credential.ID
	decision.CredentialType = credential.Type
	decision.CredentialKind = string(credential.Kind)
	if decision.MatchedPolicy == "" {
		decision.MatchedPolicy = decision.PolicyID
	}
	if decision.MatchedGrant == "" {
		decision.MatchedGrant = decision.PolicyName
	}
	return decision
}

// checkAccessForUser evaluates access for a user principal.
func (a *AuthzService) checkAccessForUser(ctx context.Context, user UserIdentity, resource Resource, action Action, permissionID string) Decision {
	// 0. If the identity is scoped (UAT), enforce project + scope constraints first.
	if scopedIdentity, ok := user.(*ScopedUserIdentity); ok {
		if denied := a.enforceUATConstraints(scopedIdentity, resource, action); denied != nil {
			return *denied
		}
	}

	// 1. Admin bypass. Scoped UAT credentials must not inherit hub-admin
	// semantics from the underlying user after their project/scope constraints
	// pass; they continue through owner, project membership, and policy grants.
	if IsUnscopedLocalPlatformAdmin(user) {
		return Decision{
			Allowed: true,
			Reason:  "admin bypass",
		}
	}

	// 2. Owner bypass
	//
	// ⚠️ D7 exception: the OwnerID lever must NOT confer assignment of a
	// hub-scoped service account. A former hub member who created the SA must
	// not be able to assign it solely via OwnerID; current hub membership is
	// required (step 2.7). The owner bypass is suppressed for ActionAssign on
	// parentless gcp_service_account resources (parentless == hub-scoped,
	// because gcpServiceAccountResource sets ParentType/ParentID only for
	// project-scoped SAs).
	//
	// Other actions (read, delete, verify) on owned resources are unaffected:
	// the creator keeps those rights. Only assignment requires the additional
	// hub membership check. Admin bypass (step 1) is not affected.
	if resource.OwnerID != "" && resource.OwnerID == user.ID() {
		if action != ActionAssign || resource.Type != "gcp_service_account" ||
			resource.ParentType != "" || resource.ParentID != "" {
			return Decision{
				Allowed: true,
				Reason:  "resource owner",
			}
		}
		// Fall through to step 2.7, which checks current hub membership.
	}

	// 2.5. Ancestry-based transitive access
	if canAccessAsAncestor(user.ID(), resource) {
		return Decision{
			Allowed: true,
			Reason:  "ancestor access",
		}
	}

	// 2.6. Project owner/admin bypass: any user with role=owner or role=admin
	// in the project's members group has the same access as the project's
	// creator-owner. This applies to the project resource itself and to all
	// resources scoped to the project (agents, members group, etc.).
	if projectID := projectIDForResource(resource); projectID != "" {
		if a.isProjectOwnerOrAdmin(ctx, user.ID(), projectID) {
			return Decision{
				Allowed: true,
				Reason:  "project owner/admin",
			}
		}
	}

	// 2.7. Hub-scoped service-account assign baseline (D5).
	//
	// Current hub members may assign hub-scoped SAs. This is the Option B
	// code baseline ruled by ptone: a narrow code path for current hub members,
	// not a seed policy, because the policy engine has no hub-scope resource arm
	// and a hub-scoped policy would over-match.
	//
	// Four properties are load-bearing:
	//
	//   1. Position. This runs AFTER the owner and project-owner baselines
	//      (which already allowed the creator and project admins) and BEFORE
	//      policy evaluation. It is therefore revocable by an explicit deny
	//      policy, and it does not shadow any baseline that already applied.
	//   2. The parentless guard. gcpServiceAccountResource gives a project
	//      parent only to project-scoped SAs; hub-scoped SAs are parentless.
	//      This arm fires only for parentless resources, which means it cannot
	//      match project-scoped SAs — they have a parent and are handled by
	//      the per-project assign policy in seed.go.
	//   3. Current hub membership. The user must be a current member of the
	//      hub-members group. OwnerID (CreatedBy) alone is NOT sufficient:
	//      a former hub member who created the SA loses assign when removed
	//      from the group. This is D7's OwnerID lever constraint.
	//   4. Action + type. Only ActionAssign on gcp_service_account.
	if action == ActionAssign && resource.Type == "gcp_service_account" &&
		resource.ParentType == "" && resource.ParentID == "" {
		if a.isCurrentHubMember(ctx, user.ID()) {
			return Decision{
				Allowed: true,
				Reason:  "hub member hub-scoped assign baseline",
				Scope:   "hub",
			}
		}
	}

	// 3. Role binding permission check (Phase 2 D4 second-path).
	// When a canonical permission ID is supplied, check whether the user
	// holds that permission through any system-scoped role binding. This is
	// the path that enables scoped admin: a non-super-admin user with the
	// hub-admin role binding can access hub operations they have permissions
	// for, without holding the super-admin bypass.
	if permissionID != "" {
		effectivePerms, err := a.getEffectivePermissions(ctx, "user", user.ID(), store.RoleScopeSystem, "")
		if err != nil {
			a.logger.Warn("failed to get effective permissions for user", "userID", user.ID(), "error", err.Error())
		} else {
			for _, p := range effectivePerms {
				if p == permissionID {
					return Decision{
						Allowed:       true,
						Reason:        "role binding grant",
						MatchedGrant:  permissionID,
						PrincipalKind: PrincipalKindUser,
					}
				}
			}
		}
	}

	// 4. Build principal refs: direct user + effective groups
	principals := []store.PrincipalRef{
		{Type: "user", ID: user.ID()},
	}

	groupIDs, err := a.store.GetEffectiveGroups(ctx, user.ID())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.logger.Warn("failed to get effective groups for user", "userID", user.ID(), "error", err.Error())
	}
	for _, gid := range groupIDs {
		principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
	}

	// 5. Fetch and evaluate policies
	policies, err := a.store.GetPoliciesForPrincipals(ctx, principals)
	if err != nil {
		a.logger.Warn("failed to get policies for principals", "error", err)
		return Decision{Allowed: false, Reason: "policy lookup error"}
	}

	return a.evaluatePolicies(policies, resource, action)
}

// checkAccessForAgent evaluates access for an agent principal.
func (a *AuthzService) checkAccessForAgent(ctx context.Context, agent AgentIdentity, resource Resource, action Action) Decision {
	// 0. Ancestry-based transitive access
	if canAccessAsAncestor(agent.ID(), resource) {
		return Decision{
			Allowed: true,
			Reason:  "ancestor access",
		}
	}

	// 1. Build principal refs: direct agent + effective groups
	principals := []store.PrincipalRef{
		{Type: "agent", ID: agent.ID()},
	}

	groupIDs, err := a.store.GetEffectiveGroupsForAgent(ctx, agent.ID())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.logger.Warn("failed to get effective groups for agent", "agent_id", agent.ID(), "error", err.Error())
	}
	for _, gid := range groupIDs {
		principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
	}

	// 2. Fetch and evaluate policies
	policies, err := a.store.GetPoliciesForPrincipals(ctx, principals)
	if err != nil {
		a.logger.Warn("failed to get policies for agent principals", "error", err)
		return Decision{Allowed: false, Reason: "policy lookup error"}
	}

	decision := a.evaluatePolicies(policies, resource, action)
	if decision.PolicyID != "" {
		return decision
	}

	// 3. Project-scoped read baseline.
	//
	// An agent may perform read-class actions on resources in its own project.
	// This codifies the project-isolation gate these paths already relied on
	// before #591; it grants nothing that was not already reachable — it
	// consolidates the hand-rolled per-handler isolation checks into one
	// enforced location.
	//
	// Four properties are load-bearing; do not change them casually:
	//
	//   1. Position. This runs *after* policy evaluation, which returns any
	//      matched policy — allow or deny — before we get here. That makes the
	//      baseline revocable: an admin can bind an explicit deny policy to the
	//      project's implicit "project:<slug>:agents" group and it wins. Moving
	//      this block earlier would make the baseline unconditional.
	//   2. The pid != "" guard. Resources with no project (broker, template,
	//      the GitHub App config, hub-scoped resources) yield "" from
	//      projectIDForResource. Without the guard, "" would equal an agent's
	//      empty ProjectID and the baseline would allow everything.
	//   3. Read-class is ActionRead and ActionList only. Deliberately not
	//      ActionAttach (PTY/exec/message mutate a running agent), not
	//      ActionCreate (gated by token scope in authorizeAgentCreate), and no
	//      mutating action.
	//   4. It grants nothing new relative to pre-#591 behaviour.
	if isReadClassAction(action) {
		if pid := projectIDForResource(resource); pid != "" && pid == agent.ProjectID() {
			return Decision{
				Allowed: true,
				Reason:  "agent project read baseline",
				Scope:   "project",
			}
		}
	}

	// 3b. Project-scoped service-account assign baseline.
	//
	// An agent may assign a GCP service account that lives in its own project.
	// Today the assignment gate checks ActionRead, which step 3 above already
	// allows for exactly this set of service accounts. svc-accnt P3 converts
	// that check to ActionAssign so the IAM actAs gate has a resource-shaped
	// place to hang. Without this arm the conversion would deny every agent
	// caller hub-wide, because checkAccessForAgent has no admin or owner
	// bypass and no seeded policy grants assign. The security in that change
	// comes from the GCP actAs check, not from narrowing the Hub policy layer.
	//
	// It is a separate arm rather than an addition to isReadClassAction on
	// purpose: read-class is deliberately narrow, and widening it would grant
	// assign on every resource type instead of this one.
	//
	// The four properties documented on step 3 apply here unchanged — in
	// particular the position after policy evaluation, which keeps this
	// revocable by an explicit deny bound to the project's implicit
	// "project:<slug>:agents" group, and the pid != "" guard. A fifth is
	// specific to this arm:
	//
	//   5. It preserves the step 3 project-baseline path exactly, and only
	//      that path. Under ActionRead an agent could also reach :483 via a
	//      hand-authored read policy (step 2) or a delegation condition
	//      (step 4), and this arm deliberately does not preserve either,
	//      because a grant to read a service account is not a grant to assign
	//      one — reproducing them would over-grant on the very surface this
	//      change exists to gate. Both are empty for agents on
	//      gcp_service_account in default configuration, so nothing breaks out
	//      of the box; an operator who wrote one loses agent assign and must
	//      grant assign explicitly. Do not cite this arm as
	//      "reachability-preserving" without that qualification.
	//
	// ⚠️ WHAT CONFINES THIS ARM, and read the next paragraph before citing
	// anything else. The confinement is `pid != ""` in the predicate
	// immediately below, combined with gcpServiceAccountResource giving a
	// project parent only to project-scoped accounts. A hub-scoped account is
	// parentless, so projectIDForResource yields "" and this arm cannot fire
	// for it. That is a property of the authorization engine and of the
	// resource builder. It does not depend on any handler.
	//
	// Do NOT justify this arm by the scope check in createAgentInProject. An
	// earlier version of this comment did exactly that, naming the
	// `sa.ScopeID != projectID` equality as an enforcing mechanism. That check
	// was replaced by sa.ReachableFromProject in a44b2950, which admits
	// hub-scoped accounts from every project — so the stated justification
	// became false while the arm it justified stayed correct for a reason the
	// comment had not recorded. A confinement argument that names a call site
	// can be invalidated by a commit to that call site, silently. Name engine
	// properties only.
	//
	// The human half of this baseline is the per-project assign policy in
	// seed.go (projectAssignPolicyName), confined by the SAME engine property
	// read the other way: this arm by `pid != ""` here, that policy by
	// `pid == ""` in matchesResource, which refuses to match a project-scoped
	// policy against a parentless resource (#595). One discipline in two
	// places, not two unrelated accidents. Neither side needs a code-side
	// revocation to stay confined, and neither should grow one.
	//
	// Goal 2 makes hub-scoped accounts assignable across projects. That does
	// not breach this arm — a hub-scoped account stays parentless, so this arm
	// still cannot fire for it, which is the fail-closed outcome §8.2 rules
	// correct: hub-scoped accounts are assignable by hub admins and the
	// account's creator and nobody else. If you are here to make hub-scoped
	// accounts broadly assignable, that is task #19 and this arm is NOT the
	// place to do it; widening `pid != ""` would grant every agent every
	// service account on the hub.
	if action == ActionAssign && resource.Type == "gcp_service_account" {
		if pid := projectIDForResource(resource); pid != "" && pid == agent.ProjectID() {
			return Decision{
				Allowed: true,
				Reason:  "agent project service-account assign baseline",
				Scope:   "project",
			}
		}
	}

	// 4. Delegation fallback: check policies with delegation conditions
	return a.checkDelegation(ctx, agent, resource, action, policies)
}

// isReadClassAction reports whether an action is read-class for the purposes of
// the agent project baseline. Read-class is deliberately narrow: read and list
// only. See checkAccessForAgent for why.
func isReadClassAction(a Action) bool {
	return a == ActionRead || a == ActionList
}

// checkDelegation handles the delegation fallback for agents.
func (a *AuthzService) checkDelegation(ctx context.Context, agent AgentIdentity, resource Resource, action Action, _ []store.Policy) Decision {
	// Find policies with delegation conditions that match the resource
	// We look at all policies that have delegation conditions
	allPolicies, err := a.store.ListPolicies(ctx, store.PolicyFilter{}, store.ListOptions{Limit: 200})
	if err != nil {
		a.logger.Warn("failed to list policies for delegation check", "error", err)
		return Decision{Allowed: false, Reason: "default deny"}
	}

	for _, policy := range allPolicies.Items {
		if policy.Conditions == nil {
			continue
		}
		if policy.Conditions.DelegatedFrom == nil && policy.Conditions.DelegatedFromGroup == "" {
			continue
		}

		// Check if the policy matches the resource and action
		if !matchesResource(policy, resource) || !matchesAction(policy, action) {
			continue
		}

		// Check if the policy's time conditions are valid
		if !evaluateTimeConditions(policy.Conditions) {
			continue
		}

		// Check delegation access via the store (verifies creator, enabled flag, etc.)
		allowed, err := a.store.CheckDelegatedAccess(ctx, agent.ID(), policy.Conditions)
		if err != nil {
			a.logger.Warn("delegation check failed", "agent_id", agent.ID(), "policyID", policy.ID, "error", err)
			continue
		}

		// If store-level delegation didn't match, check ancestry chain.
		// This supports progeny access: the DelegatedFrom principal may be
		// an ancestor (not the direct creator) of this agent.
		//
		// Phase 1G security fix 3: the ancestry fallback is gated on
		// AncestryIsHubAttested. For federated agents, the ancestry array
		// is a remote claim and must not be used for local delegation
		// matching. Federated ancestry was already validated at auth time
		// (fixes 1 and 2), but fix 3 is the load-bearing fix — it does
		// not depend on optional AllowedRootUsers config.
		if !allowed && policy.Conditions.DelegatedFrom != nil && AncestryIsHubAttested(agent) {
			ancestry := agent.Ancestry()
			for _, ancestorID := range ancestry {
				if policy.Conditions.DelegatedFrom.PrincipalID == ancestorID {
					allowed = true
					break
				}
			}
		}

		if allowed && policy.Effect == "allow" {
			return Decision{
				Allowed:    true,
				Reason:     "delegated access",
				PolicyID:   policy.ID,
				PolicyName: policy.Name,
				Scope:      policy.ScopeType,
			}
		}
	}

	return Decision{Allowed: false, Reason: "default deny"}
}

// evaluatePolicies applies the policy evaluation loop against a set of policies.
// Policies are expected to be ordered by a deterministic total order (ascending):
//
//  1. scope_type: hub < project < resource (more specific scope wins)
//  2. priority: lower number < higher number (higher priority wins)
//  3. policy_kind: default < explicit (explicit wins over default at same priority)
//  4. created: earlier < later (later-created wins as tiebreaker)
//  5. id: lexicographic order (stable final tiebreaker)
//
// The evaluation uses "last match wins at same or higher scope level": as we iterate
// through the sorted policies, each matching policy overwrites the previous match if
// it is at the same scope level or a more specific (higher) scope level. This means:
//
//   - Resource-scoped policy overrides project-scoped and hub-scoped policies (local override)
//   - Project-scoped policy overrides hub-scoped policies (local override)
//   - Within the same scope, higher priority wins (comes later in sort)
//   - At same scope and priority, explicit kind wins over default kind (comes later)
//   - At same scope, priority, and kind, later-created wins (comes later)
//
// This preserves local override behavior: a project admin can override hub-level
// policies for their project, and resource-specific policies override broader scopes.
func (a *AuthzService) evaluatePolicies(policies []store.Policy, resource Resource, action Action) Decision {
	var matched *Decision

	for _, policy := range policies {
		if !matchesResource(policy, resource) {
			continue
		}
		if !matchesAction(policy, action) {
			continue
		}
		if !evaluateConditions(policy, resource) {
			continue
		}

		d := Decision{
			Allowed:    policy.Effect == "allow",
			Reason:     "policy match",
			PolicyID:   policy.ID,
			PolicyName: policy.Name,
			Scope:      policy.ScopeType,
		}

		if matched == nil {
			matched = &d
			continue
		}

		// Compare scope levels: resource > project > hub
		matchedLevel := scopeLevel(matched.Scope)
		newLevel := scopeLevel(d.Scope)

		if newLevel > matchedLevel {
			// Lower scope (more specific) overrides
			matched = &d
		} else if newLevel == matchedLevel {
			// Same scope: later policy (higher priority number) overrides
			matched = &d
		}
	}

	if matched != nil {
		return *matched
	}

	return Decision{Allowed: false, Reason: "default deny"}
}

// scopeLevel returns a numeric level for scope ordering (higher = more specific).
func scopeLevel(scope string) int {
	switch scope {
	case "hub":
		return 0
	case "project":
		return 1
	case "resource":
		return 2
	default:
		return -1
	}
}

// matchesAction checks if a policy's actions include the requested action.
// Supports wildcard "*".
func matchesAction(policy store.Policy, action Action) bool {
	for _, a := range policy.Actions {
		if a == "*" || Action(a) == action {
			return true
		}
	}
	return false
}

// matchesResource checks if a policy applies to the given resource.
func matchesResource(policy store.Policy, resource Resource) bool {
	// Resource type must match or be wildcard
	if policy.ResourceType != "*" && policy.ResourceType != resource.Type {
		return false
	}

	// If policy specifies a resource ID, it must match
	if policy.ResourceID != "" && policy.ResourceID != resource.ID {
		return false
	}

	// Scope matching
	switch policy.ScopeType {
	case "project":
		// A project-scoped policy applies only to resources that resolve to
		// that project. Parentless / hub-scoped resources resolve to "" and
		// must NOT match — fail closed rather than falling through (#595).
		//
		// There is deliberately no outer `policy.ScopeID != ""` guard: a
		// project-scoped policy with an empty ScopeID must match nothing, not
		// everything. pid == "" is already rejected, and a non-empty pid can
		// never equal an empty ScopeID, so the two clauses cover it.
		if pid := projectIDForResource(resource); pid == "" || pid != policy.ScopeID {
			return false
		}
	case "resource":
		// Policy scoped to a specific resource
		if policy.ScopeID != "" && resource.ID != policy.ScopeID {
			return false
		}
	}

	return true
}

// evaluateConditions checks policy conditions against the resource.
func evaluateConditions(policy store.Policy, resource Resource) bool {
	if policy.Conditions == nil {
		return true
	}

	// Label conditions: all must match (AND semantics)
	if len(policy.Conditions.Labels) > 0 {
		for k, v := range policy.Conditions.Labels {
			if resource.Labels[k] != v {
				return false
			}
		}
	}

	// Time conditions
	if !evaluateTimeConditions(policy.Conditions) {
		return false
	}

	return true
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

// isProjectOwnerOrAdmin reports whether the user has project-owner or
// project-admin role in the given project. Uses role bindings as the sole
// source of truth (Phase 1F: legacy group-based fallback removed).
func (a *AuthzService) isProjectOwnerOrAdmin(ctx context.Context, userID, projectID string) bool {
	if userID == "" || projectID == "" {
		return false
	}

	membership, err := a.store.GetProjectMembership(ctx, projectID, userID)
	if err != nil || membership == nil {
		return false
	}
	return membership.Role == store.ProjectRoleOwner || membership.Role == store.ProjectRoleAdmin
}

// getEffectivePermissions resolves the set of permission IDs granted to a
// principal via role bindings. It collects all bindings for the principal,
// filters by scope, resolves each binding's role definition, and returns a
// deduplicated list of permission IDs.
func (a *AuthzService) getEffectivePermissions(ctx context.Context, principalType, principalID string, scopeType, scopeID string) ([]string, error) {
	bindings, err := a.store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var result []string

	for _, b := range bindings {
		// Filter by scope: system bindings always apply; project bindings
		// apply only if the scope matches.
		if b.ScopeType == store.RoleScopeProject {
			if scopeType != store.RoleScopeProject || b.ScopeID != scopeID {
				continue
			}
		}

		// Resolve role definition to get permissions
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

	return result, nil
}

// IsSystemAdmin checks whether the given user has a system-scoped super-admin
// role binding. This is the role-binding-based authority check (Phase 1F),
// complementing the fast-path IsUnscopedLocalPlatformAdmin which uses
// User.Role == "admin". The startup reconciliation ensures these always agree.
func (a *AuthzService) IsSystemAdmin(ctx context.Context, userID string) bool {
	if userID == "" {
		return false
	}
	// Check if the user has a super-admin role binding directly.
	bindings, err := a.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return false
	}
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeSystem {
			continue
		}
		rd, err := a.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		if rd.Name == store.SystemRoleSuperAdmin {
			return true
		}
	}
	return false
}

// hubMembersSlug is the slug of the seeded hub-members group. It is the same
// value seed.go uses when creating the group; kept as a constant so tests and
// production code agree on the lookup key.
const hubMembersSlug = "hub-members"

// isCurrentHubMember reports whether the user is a current member of the
// hub-members group. "Current" means an active membership record exists; a
// former member who was removed returns false regardless of OwnerID on any
// resource they created. This is the D7 OwnerID lever constraint: the SA
// creator is not sufficient to assign, only current hub membership is.
func (a *AuthzService) isCurrentHubMember(ctx context.Context, userID string) bool {
	if userID == "" {
		return false
	}
	group, err := a.store.GetGroupBySlug(ctx, hubMembersSlug)
	if err != nil {
		// Group does not exist or lookup failed: not a member.
		return false
	}
	_, err = a.store.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	// Any role (member, admin, owner) counts as current membership.
	return err == nil
}

// evaluateTimeConditions checks time-based conditions.
func evaluateTimeConditions(conditions *store.PolicyConditions) bool {
	if conditions == nil {
		return true
	}
	now := time.Now()
	if conditions.ValidFrom != nil && now.Before(*conditions.ValidFrom) {
		return false
	}
	if conditions.ValidUntil != nil && now.After(*conditions.ValidUntil) {
		return false
	}
	return true
}
