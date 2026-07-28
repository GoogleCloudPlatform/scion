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
	ActionAssign Action = "assign"
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

// Decision represents the result of an authorization check.
type Decision struct {
	Allowed    bool   // Whether access is allowed
	Reason     string // Human-readable explanation
	PolicyID   string // ID of the matched policy (if any)
	PolicyName string // Name of the matched policy (if any)
	Scope      string // Scope level that decided (hub, project, resource)
}

// EvaluationDetail provides detailed info for the evaluate endpoint.
type EvaluationDetail struct {
	Scope             string   `json:"scope"`
	PoliciesEvaluated int      `json:"policiesEvaluated"`
	Matched           bool     `json:"matched"`
	EffectiveGroups   []string `json:"effectiveGroups,omitempty"`
}

// AuthzService provides authorization checks using the policy evaluation engine.
type AuthzService struct {
	store  store.Store
	logger *slog.Logger
}

// NewAuthzService creates a new AuthzService.
func NewAuthzService(s store.Store, logger *slog.Logger) *AuthzService {
	return &AuthzService{
		store:  s,
		logger: logger,
	}
}

// CheckAccess evaluates whether the given identity is allowed to perform
// the specified action on the resource.
func (a *AuthzService) CheckAccess(ctx context.Context, identity Identity, resource Resource, action Action) Decision {
	switch identity.Type() {
	case "user", "dev":
		if user, ok := identity.(UserIdentity); ok {
			return a.checkAccessForUser(ctx, user, resource, action)
		}
		return Decision{Allowed: false, Reason: "invalid user identity"}
	case "agent":
		if agent, ok := identity.(AgentIdentity); ok {
			return a.checkAccessForAgent(ctx, agent, resource, action)
		}
		return Decision{Allowed: false, Reason: "invalid agent identity"}
	default:
		return Decision{Allowed: false, Reason: "unknown identity type"}
	}
}

// checkAccessForUser evaluates access for a user principal.
func (a *AuthzService) checkAccessForUser(ctx context.Context, user UserIdentity, resource Resource, action Action) Decision {
	// 0. If the identity is scoped (UAT), enforce project + scope constraints first.
	if scoped, ok := user.(*ScopedUserIdentity); ok {
		if denied := a.enforceUATConstraints(scoped, resource, action); denied != nil {
			return *denied
		}
	}

	// 1. Admin bypass
	if user.Role() == "admin" {
		return Decision{
			Allowed: true,
			Reason:  "admin bypass",
		}
	}

	// 2. Owner bypass
	if resource.OwnerID != "" && resource.OwnerID == user.ID() {
		return Decision{
			Allowed: true,
			Reason:  "resource owner",
		}
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

	// 3. Build principal refs: direct user + effective groups
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

	// 4. Fetch and evaluate policies
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
	// ⚠️ This grant is safe ONLY while service-account assignment is confined
	// to the caller's own project. Two things enforce that today: the
	// sa.ScopeID != projectID check in createAgentInProject, and
	// gcpServiceAccountResource, which gives a project parent only to
	// project-scoped accounts — so a hub-scoped account yields pid == "" here
	// and this arm cannot fire for it.
	//
	// The human half of this baseline is the per-project assign policy in
	// seed.go (projectAssignPolicyName), and it is confined by the SAME guard
	// read the other way: this arm by `pid != ""` immediately below,
	// that policy by `pid == ""` in matchesResource, which refuses to match a
	// project-scoped policy against a parentless resource (#595). One
	// discipline applied in two places, not two unrelated accidents. Neither
	// side needs a code-side revocation to stay confined, and neither should
	// grow one.
	//
	// Goal 2 relaxes that confinement to make accounts hub-scoped and
	// assignable across projects. At that moment neither guard holds: this arm
	// stops being a restatement of existing reach and becomes the only thing
	// standing between any agent and any service account on the hub. The
	// coupling is escalated to ptone as task #19; the relaxation must not land
	// before it is ruled. If you are here to relax scope, this arm is part of
	// your change.
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
		if !allowed && policy.Conditions.DelegatedFrom != nil {
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
// Policies are expected to be ordered by scope_type ASC, priority ASC.
// Lower scope overrides higher scope; higher priority overrides lower within scope.
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

// isProjectOwnerOrAdmin reports whether the user is recorded with role=owner
// or role=admin in any explicit group that belongs to the project (typically
// the "project:<slug>:members" group). These users get the same access as the
// project's creator-owner.
func (a *AuthzService) isProjectOwnerOrAdmin(ctx context.Context, userID, projectID string) bool {
	if userID == "" || projectID == "" {
		return false
	}
	groups, err := a.store.ListGroups(ctx, store.GroupFilter{
		ProjectID: projectID,
		GroupType: store.GroupTypeExplicit,
	}, store.ListOptions{Limit: 10})
	if err != nil || len(groups.Items) == 0 {
		return false
	}
	for _, g := range groups.Items {
		membership, err := a.store.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, userID)
		if err != nil {
			continue
		}
		if membership.Role == store.GroupMemberRoleOwner || membership.Role == store.GroupMemberRoleAdmin {
			return true
		}
	}
	return false
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
