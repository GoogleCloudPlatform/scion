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
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the shared, fail-closed authorization guards for hub
// handlers. Before these existed, every handler hand-wrote the same
// fetch-identity → nil-check → CheckAccess → writeError sequence, and the
// common form of that idiom silently skipped the check for any caller that was
// not a user (issue #591). Handlers should call these helpers rather than
// reproducing the idiom.

// logAuthzDenial emits the structured warning that every authorization denial
// produces. The defining property of the #591 bypass was that it was silent:
// a denial that is not logged cannot be detected, and an over-tight policy
// baseline cannot be diagnosed. The field names are part of the contract —
// operators and alerting key off them — so keep them stable.
func logAuthzDenial(r *http.Request, identity Identity, resource Resource, action Action, reason string) {
	var principalType, principalID string
	if identity != nil {
		principalType = identity.Type()
		principalID = identity.ID()
	}
	var path string
	if r != nil && r.URL != nil {
		path = r.URL.Path
	}
	slog.Warn("authorization denied",
		"principal_type", principalType,
		"principal_id", principalID,
		"resource_type", resource.Type,
		"resource_id", resource.ID,
		"action", action,
		"reason", reason,
		"path", path,
	)
}

// writeForbidden writes a 403 carrying msg, or the generic "Insufficient
// permissions" body when msg is empty.
func writeForbidden(w http.ResponseWriter, msg string) {
	if msg == "" {
		Forbidden(w)
		return
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, msg, nil)
}

// writeForbiddenStructured writes a 403 with machine-readable authorization
// detail (resource_type, denied_action) in the error envelope's details map.
// The resource ID and internal policy reason are deliberately omitted to avoid
// leaking information that aids enumeration (design §denied-detail).
func writeForbiddenStructured(w http.ResponseWriter, msg string, resourceType string, action Action) {
	if msg == "" {
		msg = "Insufficient permissions"
	}
	var details map[string]interface{}
	if resourceType != "" || action != "" {
		details = make(map[string]interface{})
		if resourceType != "" {
			details["resource_type"] = resourceType
		}
		if action != "" {
			details["denied_action"] = string(action)
		}
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, msg, details)
}

// authorize performs a fail-closed authorization check for any identity kind.
// It writes 401 for an unauthenticated caller, 403 when access is denied, and
// returns false in both cases, so callers write:
//
//	if !s.authorize(w, r, agentResource(agent), ActionDelete) { return }
//
// Unlike the pre-#591 idiom it MUST NOT be wrapped in an identity-kind guard:
// nil and non-user identities are denied here, not skipped.
//
// It takes the *http.Request rather than a context.Context so that the denial
// log can name the request path and so a future audit hook has the request.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, resource Resource, action Action) bool {
	return s.authorizeWithMessage(w, r, resource, action, "")
}

// authorizeMsg is authorize with a caller-supplied 403 body, for the handful of
// sites whose existing denial text is real user-facing guidance rather than a
// restatement of "denied". Prefer plain authorize: several of the pre-existing
// messages were actively misleading about who may pass.
func (s *Server) authorizeMsg(w http.ResponseWriter, r *http.Request, resource Resource, action Action, msg string) bool {
	return s.authorizeWithMessage(w, r, resource, action, msg)
}

// authorizeWithMessage is the single implementation behind authorize and
// authorizeMsg. An empty msg yields the generic 403 body.
func (s *Server) authorizeWithMessage(w http.ResponseWriter, r *http.Request, resource Resource, action Action, msg string) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	decision := s.authzService.CheckAccess(ctx, identity, resource, action)
	if !decision.Allowed {
		logAuthzDenial(r, identity, resource, action, decision.Reason)
		writeForbiddenStructured(w, msg, resource.Type, action)
		return false
	}
	return true
}

// authorizeRead is authorize's read-surface counterpart. On denial it writes
// a generic 404 (via NotFound) instead of a 403, so a resource the caller may
// not read is indistinguishable on the wire from one that does not exist —
// mirroring the skill fix's getSkill/writeSkillLookupError pattern
// (ptone/scion#1901) for template and harness-config read surfaces
// (ptone/scion#1916: get, download, validate, and file read/list).
//
// Only genuinely read-only checks should use this. Surfaces that also gate a
// mutation (create/update/delete) must keep using authorize/authorizeMsg: a
// write denial should read as a permission problem, not "missing", and
// authorizeRead always evaluates ActionRead regardless of what actually
// happens next, so it must never guard a non-read operation.
func (s *Server) authorizeRead(w http.ResponseWriter, r *http.Request, resource Resource, notFoundLabel string) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if s.authzService == nil {
		NotFound(w, notFoundLabel)
		return false
	}
	decision := s.authzService.CheckAccess(ctx, identity, resource, ActionRead)
	if !decision.Allowed {
		logAuthzDenial(r, identity, resource, ActionRead, decision.Reason)
		NotFound(w, notFoundLabel)
		return false
	}
	return true
}

// brokerMayReadCatalogResource reports whether an authenticated runtime
// broker may read a template or harness-config record with the given scope
// and scope ID. Brokers read these during agent creation (hydration) over
// HMAC auth, not as user principals, so the authorization kernel cannot
// evaluate them directly — but "the caller is an authenticated broker" is
// not itself authority to read every record in the catalog. Mirrors
// canUseProjectGitHubToken (skill_handlers.go): a broker may act on a
// project's resources only when it is a registered provider for that
// project (store.GetProjectProvider). Every broker exemption for a
// template/harness-config read surface (get, list, download, files) must
// call this instead of admitting any authenticated broker unconditionally.
//
//   - Global (hub-wide) scope: always allowed — no confidentiality boundary,
//     the same rule filterHubWideTemplateGrants/filterHubWideHarnessConfigGrants
//     encode for the curated hub-member/hub-viewer grant.
//   - Project scope: allowed only when the broker is a registered provider
//     for that project.
//   - User scope, or any other/unrecognized scope value: never — a broker
//     has no legitimate reason to read a user's private catalog entry, and
//     an unrecognized scope must fail closed rather than default-allow.
func (s *Server) brokerMayReadCatalogResource(ctx context.Context, broker BrokerIdentity, scope, scopeID string) bool {
	if broker == nil {
		return false
	}
	switch scope {
	case store.TemplateScopeGlobal: // == store.HarnessConfigScopeGlobal ("global")
		return true
	case store.TemplateScopeProject: // == store.HarnessConfigScopeProject ("project")
		if s.store == nil || scopeID == "" {
			return false
		}
		_, err := s.store.GetProjectProvider(ctx, scopeID, broker.BrokerID())
		return err == nil
	default:
		return false
	}
}

// authorizeAgentCreate gates agent creation for every caller kind. Exhaustive
// and fail-closed. Replaces the caller-kind branch in createAgent, which had no
// else clause, and supplies the gate createProjectAgent never had.
//
// The agent path is scope-gated rather than policy-gated: sub-agent creation is
// a template-administered capability (ScopeAgentCreate), constrained to the
// calling agent's own project.
func (s *Server) authorizeAgentCreate(w http.ResponseWriter, r *http.Request, projectID string) bool {
	ctx := r.Context()
	resource := Resource{
		Type:       "agent",
		ParentType: "project",
		ParentID:   projectID,
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}

	// A switch with a terminating default, not a chain of ifs: an unhandled
	// caller kind falling through the guard is precisely the #591 bug.
	switch identity.Type() {
	case "agent":
		agentIdent, ok := identity.(AgentIdentity)
		if !ok {
			logAuthzDenial(r, identity, resource, ActionCreate, "invalid agent identity")
			writeForbidden(w, "")
			return false
		}
		if !agentIdent.HasScope(ScopeAgentCreate) {
			logAuthzDenial(r, identity, resource, ActionCreate,
				"missing scope "+string(ScopeAgentCreate))
			writeForbidden(w, "Missing required scope: "+string(ScopeAgentCreate))
			return false
		}
		if agentIdent.ProjectID() != projectID {
			logAuthzDenial(r, identity, resource, ActionCreate, "agent project mismatch")
			writeForbidden(w, "Agents can only create sub-agents within their own project")
			return false
		}
		return true

	case "user", "dev":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			logAuthzDenial(r, identity, resource, ActionCreate, "invalid user identity")
			writeForbidden(w, "")
			return false
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, resource, ActionCreate)
		if !decision.Allowed {
			logAuthzDenial(r, identity, resource, ActionCreate, decision.Reason)
			writeForbidden(w, "You don't have permission to create agents in this project")
			return false
		}
		return true

	default:
		logAuthzDenial(r, identity, resource, ActionCreate,
			"identity type may not create agents")
		writeForbidden(w, "")
		return false
	}
}

// authorizeAgentLifecycle gates operations on an existing agent, for every
// caller kind. Exhaustive and fail-closed. action selects the permission a
// user caller must hold (see agentActionPermission):
//   - ActionLifecycle for management (start/stop/suspend/restart/restore)
//   - ActionAttach for observation (terminal, exec, env, reset-auth)
//
// Project owners/admins hold agent.lifecycle through their role but not
// agent.attach, so they can manage members' agents without being able to
// observe them (miller79/scion#88).
//
// An agent caller passes on ScopeAgentLifecycle within its own project, which
// includes project peers as well as its own descendants. That breadth is
// deliberate (design Q3): the scope is template-administered rather than
// ambient, and handleProjectBroadcast already reads it as conferring exactly
// this authority. Narrowing it later is a change to this one function.
func (s *Server) authorizeAgentLifecycle(w http.ResponseWriter, r *http.Request, agent *store.Agent, action Action) bool {
	ctx := r.Context()

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if agent == nil {
		// Caller bug rather than a policy outcome; deny rather than panic.
		logAuthzDenial(r, identity, Resource{Type: "agent"}, action, "nil agent")
		writeForbidden(w, "")
		return false
	}
	resource := agentResource(agent)

	switch identity.Type() {
	case "agent":
		agentIdent, ok := identity.(AgentIdentity)
		if !ok {
			logAuthzDenial(r, identity, resource, action, "invalid agent identity")
			writeForbidden(w, "")
			return false
		}
		if !agentIdent.HasScope(ScopeAgentLifecycle) {
			logAuthzDenial(r, identity, resource, action,
				"missing scope "+string(ScopeAgentLifecycle))
			writeForbidden(w, "Missing required scope: "+string(ScopeAgentLifecycle))
			return false
		}
		if agentIdent.ProjectID() != agent.ProjectID {
			logAuthzDenial(r, identity, resource, action, "agent project mismatch")
			writeForbidden(w, "Agents can only manage agents within their own project")
			return false
		}
		return true

	case "user", "dev":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			logAuthzDenial(r, identity, resource, action, "invalid user identity")
			writeForbidden(w, "")
			return false
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, resource, action)
		if !decision.Allowed {
			logAuthzDenial(r, identity, resource, action, decision.Reason)
			writeForbidden(w, "")
			return false
		}
		return true

	default:
		logAuthzDenial(r, identity, resource, action,
			"identity type may not act on agent lifecycle")
		writeForbidden(w, "")
		return false
	}
}

// agentActionPermission maps an agent action name (api.AgentAction*) to the
// authz action a user caller must hold. Management actions map to
// ActionLifecycle; everything else (exec, env, reset-auth, message history,
// terminal) maps to ActionAttach, which carries the owner's secrets exposure.
func agentActionPermission(action string) Action {
	switch action {
	case api.AgentActionStart, api.AgentActionStop, api.AgentActionSuspend,
		api.AgentActionRestart, api.AgentActionRestore:
		return ActionLifecycle
	default:
		return ActionAttach
	}
}

// Deprecated: requireAdmin is the legacy admin check. New handlers should use
// permission-based route metadata (RouteMetadata.Permission) which the routeGuard
// evaluates via Decide. This function remains only as a fallback for routes not
// yet converted to the permission model. Do not use in new code.
//
// requireAdmin returns the calling user identity if it is a hub admin, writing
// 401 for an unauthenticated caller and 403 for any authenticated caller that
// is not an admin user, and returning false in both cases.
//
// It resolves the identity with GetIdentityFromContext rather than
// GetUserIdentityFromContext. The latter returns nil for agent and broker
// callers, which conflates "nobody is authenticated" with "the authenticated
// caller is not a user" — the same conflation that produced #591. Here it
// merely produced a wrong status code (an authenticated agent was told
// "Authentication required"), but the distinction is worth keeping honest:
// this helper gates the hub's admin endpoints.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (UserIdentity, bool) {
	// Synthetic resource: requireAdmin is a role check on the hub itself
	// rather than a policy check on an addressable resource.
	resource := Resource{Type: "hub", ID: r.URL.Path}

	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return nil, false
	}
	// The assertion, rather than a switch on Type(), is deliberate: the question
	// this helper asks is "can this caller answer Role()". It admits both "user"
	// and dev-auth "dev" identities (DevUser implements UserIdentity) and denies
	// everything else, including a "user"-typed identity too degenerate to have
	// a role.
	user, ok := identity.(UserIdentity)
	if !ok {
		logAuthzDenial(r, identity, resource, ActionManage, "non-user identity")
		Forbidden(w)
		return nil, false
	}
	if !IsUnscopedLocalPlatformAdmin(user) {
		reason := "not an admin"
		if IsScopedUserIdentity(user) {
			reason = "scoped user access token"
		} else if _, federated := user.(FederatedIdentity); federated {
			reason = "federated identity is not a local platform admin"
		}
		logAuthzDenial(r, identity, resource, ActionManage, reason)
		Forbidden(w)
		return nil, false
	}
	return user, true
}
