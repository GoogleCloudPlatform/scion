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
	"net/http"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
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
	ctx := context.Background()
	if r != nil {
		if r.URL != nil {
			path = r.URL.Path
		}
		ctx = r.Context()
	}
	// E.2a (plan §3.1(5)): every one of this function's ~56 call sites now
	// also carries the credential kind/ID and the request's correlation ID,
	// through this single edit at the field-list boundary.
	credential := GetCredentialContextFromContext(ctx)
	slog.Warn("authorization denied",
		"principal_type", principalType,
		"principal_id", principalID,
		"resource_type", resource.Type,
		"resource_id", resource.ID,
		"action", action,
		"reason", reason,
		"path", path,
		"credential_kind", string(credential.Kind),
		"credential_id", credential.ID,
		"request_id", logging.RequestIDFromContext(ctx),
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
	writeForbiddenStructuredDenial(w, msg, resourceType, action, "")
}

// writeForbiddenStructuredDenial is writeForbiddenStructured for a denial
// with a known deciding stage. A delegation-ceiling denial adds
// details.denied_by; no other stage adds anything.
func writeForbiddenStructuredDenial(w http.ResponseWriter, msg string, resourceType string, action Action, deniedBy DeniedBy) {
	writeForbiddenStructuredDenialCause(w, msg, resourceType, action, deniedBy, "")
}

// Additive details on a ceiling_unrecorded denial. They name the recovery
// route only; no edge or ancestor ID is returned to the caller. The message
// text is unchanged.
const (
	detailDenyCause       = "deny_cause"
	detailRemediation     = "remediation"
	detailRemediationPath = "remediation_path"

	remediationDelegationProvenanceAdoption = "delegation_provenance_adoption"
)

// addCeilingUnrecordedDetails adds the delegation-provenance adoption keys
// to details when cause is DenyCauseCeilingUnrecorded, allocating details
// when needed. Any other cause returns details unchanged.
func addCeilingUnrecordedDetails(details map[string]interface{}, cause DenyCause) map[string]interface{} {
	if cause != DenyCauseCeilingUnrecorded {
		return details
	}
	if details == nil {
		details = make(map[string]interface{}, 3)
	}
	details[detailDenyCause] = string(DenyCauseCeilingUnrecorded)
	details[detailRemediation] = remediationDelegationProvenanceAdoption
	details[detailRemediationPath] = delegationAdoptionPath
	return details
}

// writeForbiddenStructuredDenialCause is writeForbiddenStructuredDenial
// with the decision's DenyCause; a ceiling_unrecorded cause adds the
// delegation-provenance adoption details.
func writeForbiddenStructuredDenialCause(w http.ResponseWriter, msg string, resourceType string, action Action, deniedBy DeniedBy, cause DenyCause) {
	if msg == "" {
		msg = "Insufficient permissions"
	}
	var details map[string]interface{}
	if resourceType != "" || action != "" || deniedBy == DeniedByDelegationCeiling {
		details = make(map[string]interface{})
		if resourceType != "" {
			details["resource_type"] = resourceType
		}
		if action != "" {
			details["denied_action"] = string(action)
		}
		if deniedBy == DeniedByDelegationCeiling {
			details["denied_by"] = string(DeniedByDelegationCeiling)
		}
	}
	details = addCeilingUnrecordedDetails(details, cause)
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
		writeForbiddenStructuredDenialCause(w, msg, resource.Type, action, decision.DeniedBy, decision.adoptionDetailsCause())
		return false
	}
	return true
}

// authorizeWithEvidence is authorize with server-built target evidence for
// a collection-level request (CheckAccessWithEvidence). evidence must come
// from hubCollectionEvidence or projectCollectionEvidence in the handler
// that knows which operation it runs, never from a request field.
func (s *Server) authorizeWithEvidence(w http.ResponseWriter, r *http.Request, resource Resource, action Action, evidence TargetScopeEvidence) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	decision := s.authzService.CheckAccessWithEvidence(ctx, identity, resource, action, evidence)
	if !decision.Allowed {
		logAuthzDenial(r, identity, resource, action, decision.Reason)
		writeForbiddenStructuredDenial(w, "", resource.Type, action, decision.DeniedBy)
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
	// isNilIdentity, not broker == nil: BrokerIdentity embeds Identity, so a
	// typed-nil concrete broker identity (see isNilIdentity) is a non-nil
	// interface value and would otherwise reach broker.BrokerID() below, or
	// fall through to the global-scope case and be granted access outright.
	if isNilIdentity(broker) {
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

// agentCreateDenyMessage is the client message for a denied agent creation.
const agentCreateDenyMessage = "You don't have permission to create agents in this project"

// agentCreateDecision decides whether identity may create an agent in
// projectID: the exact agent.create permission on the project's agent
// collection through Decide, which applies the caller's roles, token
// restrictions and, for an agent caller, the delegation ceiling of every
// live ancestor.
func (s *Server) agentCreateDecision(ctx context.Context, identity Identity, projectID string) Decision {
	return s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   agentCreateResource(projectID),
		Action:     ActionCreate,
		Permission: "agent.create",
	})
}

// agentCreateResource is the authorization target of agent creation in
// projectID.
func agentCreateResource(projectID string) Resource {
	return Resource{Type: "agent", ParentType: "project", ParentID: projectID}
}

// authorizeAgentCreate gates agent creation for every caller kind, fail
// closed, with a terminating default for unknown kinds.
//
//   - An agent caller needs ScopeAgentCreate (template-administered) and must
//     create within its own project (both project IDs non-empty).
//   - Every caller then needs agentCreateDecision.
func (s *Server) authorizeAgentCreate(w http.ResponseWriter, r *http.Request, projectID string) bool {
	ctx := r.Context()
	resource := agentCreateResource(projectID)

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}

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
		if projectID == "" || agentIdent.ProjectID() != projectID {
			logAuthzDenial(r, identity, resource, ActionCreate, "agent project mismatch")
			writeForbidden(w, "Agents can only create sub-agents within their own project")
			return false
		}

	case "user", "dev":
		if _, ok := identity.(UserIdentity); !ok {
			logAuthzDenial(r, identity, resource, ActionCreate, "invalid user identity")
			writeForbidden(w, "")
			return false
		}

	default:
		logAuthzDenial(r, identity, resource, ActionCreate,
			"identity type may not create agents")
		writeForbidden(w, "")
		return false
	}

	decision := s.agentCreateDecision(ctx, identity, projectID)
	if !decision.Allowed {
		logAuthzDenial(r, identity, resource, ActionCreate, decision.Reason)
		writeForbiddenDenialCause(w, agentCreateDenyMessage, decision.DeniedBy, decision.adoptionDetailsCause())
		return false
	}
	return true
}

// agentTargetDenyMessage is the client message for a denied action on an
// existing agent.
const agentTargetDenyMessage = "insufficient permission for this agent action"

// agentTargetPermission is the exact registered permission for an authz
// action on an existing agent. Any other action has none and is denied.
func agentTargetPermission(action Action) string {
	switch action {
	case ActionLifecycle:
		return "agent.lifecycle"
	case ActionAttach:
		return "agent.attach"
	case ActionDelete:
		return "agent.delete"
	default:
		return ""
	}
}

// agentTargetDenial describes a denied action on an existing agent.
type agentTargetDenial struct {
	// status is the HTTP status (401 or 403).
	status int
	// message is the client message; empty selects the default 403 text.
	message string
	// reason is the internal reason, for logs only.
	reason string
	// deniedBy is the decision stage that denied, when attributed.
	deniedBy DeniedBy
	// cause is the decision's adoptionDetailsCause: ceiling_unrecorded when
	// delegation-provenance adoption can address the denial, else empty.
	cause DenyCause
}

// authorizeAgentTargetAction decides whether identity may perform action on
// the existing agent target. It is the single rule for lifecycle, attach and
// delete on an agent, for every caller kind, and returns nil when allowed.
//
//   - An agent caller needs ScopeAgentLifecycle and must be in the target's
//     project (both project IDs non-empty).
//   - Every caller then needs the exact permission for action on the target
//     through Decide, which applies the caller's roles, named relationships,
//     token restrictions and, for an agent caller, the delegation ceiling of
//     every live ancestor.
//
// Unknown caller kinds and unmapped actions are denied.
func (s *Server) authorizeAgentTargetAction(ctx context.Context, identity Identity, target *store.Agent, action Action) *agentTargetDenial {
	if identity == nil {
		return &agentTargetDenial{status: http.StatusUnauthorized, reason: "unauthenticated"}
	}
	if target == nil {
		return &agentTargetDenial{status: http.StatusForbidden, reason: "nil agent"}
	}
	permissionID := agentTargetPermission(action)
	if permissionID == "" {
		return &agentTargetDenial{status: http.StatusForbidden, reason: "no permission for action " + string(action)}
	}

	switch identity.Type() {
	case "agent":
		agentIdent, ok := identity.(AgentIdentity)
		if !ok {
			return &agentTargetDenial{status: http.StatusForbidden, reason: "invalid agent identity"}
		}
		if !agentIdent.HasScope(ScopeAgentLifecycle) {
			return &agentTargetDenial{
				status:  http.StatusForbidden,
				message: "Missing required scope: " + string(ScopeAgentLifecycle),
				reason:  "missing scope " + string(ScopeAgentLifecycle),
			}
		}
		if target.ProjectID == "" || agentIdent.ProjectID() == "" || agentIdent.ProjectID() != target.ProjectID {
			return &agentTargetDenial{
				status:  http.StatusForbidden,
				message: "Agents can only manage agents within their own project",
				reason:  "agent project mismatch",
			}
		}
	case "user", "dev":
		if _, ok := identity.(UserIdentity); !ok {
			return &agentTargetDenial{status: http.StatusForbidden, reason: "invalid user identity"}
		}
	default:
		return &agentTargetDenial{status: http.StatusForbidden, reason: "identity type may not act on agents"}
	}

	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   agentResource(target),
		Action:     action,
		Permission: permissionID,
	})
	if !decision.Allowed {
		return &agentTargetDenial{
			status:   http.StatusForbidden,
			message:  agentTargetDenyMessage,
			reason:   decision.Reason,
			deniedBy: decision.DeniedBy,
			cause:    decision.adoptionDetailsCause(),
		}
	}
	return nil
}

// writeAgentTargetDenial logs and writes a denial from
// authorizeAgentTargetAction. A delegation-ceiling denial carries the single
// detail denied_by=delegation_ceiling and nothing else.
func writeAgentTargetDenial(w http.ResponseWriter, r *http.Request, identity Identity, target *store.Agent, action Action, denial *agentTargetDenial) {
	if denial.status == http.StatusUnauthorized {
		Unauthorized(w)
		return
	}
	resource := Resource{Type: "agent"}
	if target != nil {
		resource = agentResource(target)
	}
	logAuthzDenial(r, identity, resource, action, denial.reason)
	writeForbiddenDenialCause(w, denial.message, denial.deniedBy, denial.cause)
}

// writeForbiddenDenial writes a 403 with message (default text when empty).
// A delegation-ceiling denial adds details.denied_by and no other detail.
func writeForbiddenDenial(w http.ResponseWriter, message string, deniedBy DeniedBy) {
	writeForbiddenDenialCause(w, message, deniedBy, "")
}

// writeForbiddenDenialCause is writeForbiddenDenial with the decision's
// DenyCause; a ceiling_unrecorded cause adds the delegation-provenance
// adoption details.
func writeForbiddenDenialCause(w http.ResponseWriter, message string, deniedBy DeniedBy, cause DenyCause) {
	var details map[string]interface{}
	if deniedBy == DeniedByDelegationCeiling {
		details = map[string]interface{}{"denied_by": string(DeniedByDelegationCeiling)}
	}
	details = addCeilingUnrecordedDetails(details, cause)
	if details == nil {
		writeForbidden(w, message)
		return
	}
	if message == "" {
		message = "Insufficient permissions"
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, message, details)
}

// authorizeAgentLifecycle gates operations on an existing agent, for every
// caller kind, through authorizeAgentTargetAction. action selects the
// permission (see agentActionPermission):
//   - ActionLifecycle for management (start/stop/suspend/restart/restore)
//   - ActionAttach for observation (terminal, exec, env, reset-auth)
//
// Project owners/admins hold agent.lifecycle through their role but not
// agent.attach, so they can manage members' agents without being able to
// observe them (miller79/scion#88).
//
// An agent caller needs ScopeAgentLifecycle within its own project and the
// exact permission on the target through Decide, which includes the
// delegation ceiling of every live ancestor.
func (s *Server) authorizeAgentLifecycle(w http.ResponseWriter, r *http.Request, agent *store.Agent, action Action) bool {
	identity := GetIdentityFromContext(r.Context())
	if denial := s.authorizeAgentTargetAction(r.Context(), identity, agent, action); denial != nil {
		writeAgentTargetDenial(w, r, identity, agent, action, denial)
		return false
	}
	return true
}

// agentLifecycleAllowed reports whether identity may manage target (start,
// resume, or restart it), applying the identical rule
// authorizeAgentLifecycle enforces for ActionLifecycle -- the same authority
// the /start route requires -- but without writing an HTTP response.
//
// It exists for callers like handleExistingAgent that need the boolean
// because a denial there must not surface as authorizeAgentLifecycle's 403
// (which would confirm to the caller that a specific agent exists and is
// somebody else's): the caller folds a false result into a generic
// name-conflict response instead, disclosing nothing about the agent it was
// denied against.
func (s *Server) agentLifecycleAllowed(ctx context.Context, identity Identity, target *store.Agent) bool {
	return s.authorizeAgentTargetAction(ctx, identity, target, ActionLifecycle) == nil
}

// agentFullHistoryDecision decides whether identity sees an agent's full
// message and log history: the exact agent.attach permission on the agent
// through Decide. A caller without it sees only history it participates in,
// subject to agent.read.
func (s *Server) agentFullHistoryDecision(ctx context.Context, identity Identity, agent *store.Agent) Decision {
	return s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   agentResource(agent),
		Action:     ActionAttach,
		Permission: "agent.attach",
	})
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
	case api.AgentActionKeys:
		// Explicit, not a fallthrough to default: the agent-keys contract
		// (.design/agent-keys-contract.md, decision 2 / ptone/scion#2191)
		// requires that api.AgentActionKeys map to the existing attach
		// permission and its credential ceilings by a visible, auditable
		// registration rather than by accidentally landing in this
		// function's default branch. The route action itself is not a new
		// independently granted permission. Task 2.1 (ptone/scion#2195) gave
		// this action its own early branch on both action-dispatch
		// functions (handlers_agents_core.go's handleAgentAction,
		// handlers_projects_core.go's handleProjectAgentAction), calling
		// authorizeAgentKeys directly — so this case is no longer reached by
		// any live HTTP request; TestAgentActionPermission_KeysMapsToAttach
		// pins the mapped value directly instead. Neither switch has a
		// dispatch case that calls a real keys handler yet (task 2.2 adds
		// one), so an authorized call still 404s via each switch's own
		// default branch, not because it was denied.
		return ActionAttach
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
	return s.requireAdminFor(w, r, "")
}

// requireAdminFor is requireAdmin for a session-only admin operation. A
// scoped user access token is refused with the same 403 as requireAdmin,
// plus the session-only reason details (session_only_gate.go). An empty
// reason writes the plain 403.
func (s *Server) requireAdminFor(w http.ResponseWriter, r *http.Request, reason authzop.SessionOnlyReason) (UserIdentity, bool) {
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
		denial := "not an admin"
		scoped := IsScopedUserIdentity(user)
		if scoped {
			denial = "scoped user access token"
		} else if _, federated := user.(FederatedIdentity); federated {
			denial = "federated identity is not a local platform admin"
		}
		logAuthzDenial(r, identity, resource, ActionManage, denial)
		if scoped && reason != "" {
			writeSessionOnlyDenial(w, ErrCodeForbidden, "Insufficient permissions", reason)
		} else {
			Forbidden(w)
		}
		return nil, false
	}
	return user, true
}

// Self-scoped authorization.
//
// A self permission (permissions.IsSelfPermission) acts only on the
// holder's own records: inbox items, direct messages, user-scope skill
// injections. Those records have no project or hub target that a role
// binding could authorize, so these checks replace Decide for them. The
// caller has already confirmed that the record belongs to the holder.

// Self-scope deny reasons.
const (
	selfScopeReasonNotSelfPermission = "permission is not a self-scoped permission"
	selfScopeReasonCredential        = "credential cannot act on self-scoped records"
	selfScopeReasonCeiling           = "token does not have scope for this self-scoped permission"
	selfScopeReasonBoundary          = bearerReasonBoundaryIneligible
	selfScopeReasonOutsideProject    = bearerReasonOutsideProject
)

// selfScopedDecision applies the self-scope rule for one record:
//
//   - permissionID must be a self permission;
//   - an interactive session or a dev credential passes;
//   - a user access token passes only if its ceiling allows permissionID,
//     permissionID is eligible for the token's boundary kind, and, for a
//     project boundary, rowProjectID equals the boundary project. A record
//     with no project (rowProjectID empty) needs a hub boundary;
//   - every other credential is denied.
//
// ok is false with a stable reason on denial.
func selfScopedDecision(identity Identity, permissionID, rowProjectID string) (ok bool, reason string) {
	if !permissions.IsSelfPermission(permissionID) {
		return false, selfScopeReasonNotSelfPermission
	}
	switch v := identity.(type) {
	case *AuthenticatedUser:
		if v == nil {
			return false, selfScopeReasonCredential
		}
		return true, ""
	case *DevUser:
		if v == nil {
			return false, selfScopeReasonCredential
		}
		return true, ""
	case *ScopedUserIdentity:
		if v == nil {
			return false, selfScopeReasonCredential
		}
		if !v.Ceiling().Allows(permissionID) {
			return false, selfScopeReasonCeiling
		}
		boundary := v.Boundary()
		if !boundary.Valid() {
			return false, bearerReasonBoundaryInvalid
		}
		if !permissionEligibleForBoundary(permissionID, boundary.Kind) {
			return false, selfScopeReasonBoundary
		}
		if boundary.Kind == BoundaryKindProject && (rowProjectID == "" || rowProjectID != boundary.ProjectID) {
			return false, selfScopeReasonOutsideProject
		}
		return true, ""
	default:
		return false, selfScopeReasonCredential
	}
}

// authorizeSelfScoped authorizes the caller to apply permissionID to one of
// its own records whose project is rowProjectID (empty for a record with no
// project, such as a direct message between two users). It writes 401 when
// no identity is present and 403 on denial; see selfScopedDecision for the
// rule.
func (s *Server) authorizeSelfScoped(w http.ResponseWriter, r *http.Request, permissionID string, rowProjectID string) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return false
	}
	ok, reason := selfScopedDecision(identity, permissionID, rowProjectID)
	if ok {
		return true
	}
	resourceType, action := selfPermissionResourceAction(permissionID)
	logAuthzDenial(r, identity, Resource{Type: resourceType}, action, reason)
	writeForbiddenStructured(w, "", resourceType, action)
	return false
}

// selfPermissionResourceAction returns the registry resource type and
// action of permissionID, or empty values for an unknown ID.
func selfPermissionResourceAction(permissionID string) (string, Action) {
	for _, p := range permissions.Registry {
		if p.ID == permissionID {
			return p.Resource, Action(p.Action)
		}
	}
	return "", ""
}

// filterSelfScopedRows keeps the rows of the caller's own records that the
// caller may see with permissionID: projectOf returns a row's project (empty
// for a row with no project), and each row is checked with the
// authorizeSelfScoped rule. A caller that may not use permissionID at all
// gets no rows. List handlers filter the full result first and compute
// totals and cursors from the filtered rows (pageSelfScopedRows), so neither
// counts a row the caller cannot see.
func filterSelfScopedRows[T any](identity Identity, permissionID string, rows []T, projectOf func(T) string) []T {
	out := make([]T, 0, len(rows))
	for _, row := range rows {
		if ok, _ := selfScopedDecision(identity, permissionID, projectOf(row)); ok {
			out = append(out, row)
		}
	}
	return out
}

// errInvalidSelfScopedCursor reports a cursor pageSelfScopedRows did not
// issue.
var errInvalidSelfScopedCursor = errors.New("invalid cursor")

// pageSelfScopedRows filters rows with filterSelfScopedRows and then pages
// the filtered rows: totalCount is the number of visible rows, and cursor
// and nextCursor are offsets into the visible rows. An empty cursor starts
// at the first row; nextCursor is empty on the last page. limit must be
// positive.
func pageSelfScopedRows[T any](identity Identity, permissionID string, rows []T, projectOf func(T) string, cursor string, limit int) (items []T, totalCount int, nextCursor string, err error) {
	if limit <= 0 {
		return nil, 0, "", errors.New("limit must be positive")
	}
	visible := filterSelfScopedRows(identity, permissionID, rows, projectOf)
	start := 0
	if cursor != "" {
		start, err = strconv.Atoi(cursor)
		if err != nil || start < 0 || start > len(visible) {
			return nil, 0, "", errInvalidSelfScopedCursor
		}
	}
	end := start + limit
	if end >= len(visible) {
		end = len(visible)
	} else {
		nextCursor = strconv.Itoa(end)
	}
	return visible[start:end], len(visible), nextCursor, nil
}
