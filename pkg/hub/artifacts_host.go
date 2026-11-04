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
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// artifactHost is the hub's implementation of artifacts.Host: it answers the
// artifact service's three questions from the hub's identity context and authz
// engine, and turns hub types into the strings the service sees.
type artifactHost struct {
	server *Server
}

var (
	_ artifacts.Host                 = (*artifactHost)(nil)
	_ artifacts.ReviewGrantAuthority = (*artifactHost)(nil)
)

// newArtifactHost returns the artifacts.Host for s. The authz service is read
// per call, not captured, so a host built at route registration sees the
// server's final wiring.
func newArtifactHost(s *Server) *artifactHost {
	return &artifactHost{server: s}
}

// Principal maps the request identity to (kind, ref, homeScope). Owner refs
// are stable ids (user id, agent id), never names.
//
// Local users, scoped user access tokens and the dev user are users; what a
// scoped token allows is Permits' question. Agents are served only when
// token-backed (a validated agent token, which carries a token id) and when
// the token carries project:artifact:read: in-process agent identities the
// hub builds for its own decisions carry unfiltered role scopes and never
// reach the artifact service. Every other identity (federated principals,
// brokers, unknown types) is not served and yields ok=false.
func (h *artifactHost) Principal(ctx context.Context) (kind, ref, homeScope string, ok bool) {
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return "", "", "", false
	}
	switch id := identity.(type) {
	case *AuthenticatedUser, *ScopedUserIdentity, *DevUser:
		if id.ID() == "" {
			return "", "", "", false
		}
		return artifacts.PrincipalKindUser, id.ID(), "", true
	case *agentIdentityWrapper:
		if id.ID() == "" || id.TokenID() == "" || !agentHasAnyScope(id, []string{string(ScopeProjectArtifactRead)}) {
			return "", "", "", false
		}
		return artifacts.PrincipalKindAgent, id.ID(), id.ProjectID(), true
	default:
		return "", "", "", false
	}
}

// Permits reports whether the caller's credential allows permission on
// artifacts homed in the project scopeRef, before ownership, grants or role
// bindings are considered:
//   - a session or dev user: yes (role bindings decide in Authorize);
//   - a scoped user access token: its boundary must reach the project and
//     its ceiling must allow the permission;
//   - a token-backed agent: its token must carry one of the permission's
//     dedicated agent scopes, and its delegation chain must allow the
//     permission at use time (agentChainAllows). There is no project
//     boundary: grants to other projects are how agents collaborate across
//     projects (ptone/scion#3202);
//   - anything else: no.
//
// It fails closed on an empty scope or a permission that is not an artifact
// registry row.
func (h *artifactHost) Permits(ctx context.Context, scopeRef, permission string) bool {
	if scopeRef == "" {
		return false
	}
	perm, ok := artifactPermission(permission)
	if !ok {
		return false
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return false
	}
	switch id := identity.(type) {
	case *AuthenticatedUser, *DevUser:
		return true
	case *ScopedUserIdentity:
		return BoundaryAllows(id.Boundary(), TargetScope{Kind: TargetScopeProject, ProjectID: scopeRef}) &&
			id.Ceiling().Allows(perm.ID)
	case *agentIdentityWrapper:
		return id.TokenID() != "" && agentHasAnyScope(id, perm.AgentScopes) && h.agentChainAllows(ctx, id, perm)
	default:
		return false
	}
}

// agentChainAllows reports whether the agent's delegation chain allows perm
// right now: the chain is walked in the agent's own project, so an agent
// whose edge is gone, whose delegator no longer holds the permission, or
// whose recorded ceiling excludes it is refused for the life of its token,
// on every path including ownership and grants. It is deliberately not a
// full Authorize: a grant to read an artifact homed in another project must
// still work for an agent whose chain allows artifact reads in its own
// project.
func (h *artifactHost) agentChainAllows(ctx context.Context, agent *agentIdentityWrapper, perm permissions.Permission) bool {
	ok, err := h.agentChainAllowsErr(ctx, agent, perm)
	return err == nil && ok
}

// agentChainAllowsErr is agentChainAllows with the chain walk's lookup
// error returned instead of taken for a refusal. It is the one place the
// artifact chain walk is made: agentChainAllows (a caller's credential)
// and the send-time recipient check (recipientSubject.Permits, which must
// stay silent on a failure) both use it.
func (h *artifactHost) agentChainAllowsErr(ctx context.Context, agent *agentIdentityWrapper, perm permissions.Permission) (bool, error) {
	if h.server == nil || h.server.authzService == nil {
		return false, nil
	}
	project := agent.ProjectID()
	if project == "" || agent.ID() == "" {
		return false, nil
	}
	resource := Resource{Type: permissions.ResourceArtifact, ParentType: permissions.ResourceProject, ParentID: project}
	allowed, _, err := h.server.authzService.walkDelegationChainWithCause(ctx, resource, Action(perm.Action), perm.ID,
		agent.ID(), AncestryIsHubAttested(agent), store.RoleScopeProject, project, nil, nil)
	if err != nil {
		return false, err
	}
	return allowed, nil
}

// Authorize reports whether the caller may exercise permission on artifacts
// homed in the project scopeRef. It fails closed on an empty scope, an
// unauthenticated caller, a permission that is not an artifact registry row,
// or a missing authz service.
//
// It starts with Permits, so Authorize true implies Permits true: the
// credential's own limits always apply (for agents, a token-backed identity
// holding one of the permission's dedicated agent scopes, so a permission
// with none, artifact.delete or artifact.manage, is never agent-callable).
// Then, like every caller, it goes through AuthzService.CheckAccess against
// the project.
func (h *artifactHost) Authorize(ctx context.Context, scopeRef, permission string) bool {
	if h.server == nil || h.server.authzService == nil {
		return false
	}
	if !h.Permits(ctx, scopeRef, permission) {
		return false
	}
	identity := GetIdentityFromContext(ctx)
	perm, _ := artifactPermission(permission)
	decision := h.server.authzService.CheckAccess(ctx, identity, Resource{
		Type:       permissions.ResourceArtifact,
		ParentType: permissions.ResourceProject,
		ParentID:   scopeRef,
	}, Action(perm.Action))
	return decision.Allowed
}

// MayGrantReview implements artifacts.ReviewGrantAuthority:
// ReviewGrantClause != "".
func (h *artifactHost) MayGrantReview(ctx context.Context, ownerAgentID, homeScope, sponsorRef string) bool {
	return h.ReviewGrantClause(ctx, ownerAgentID, homeScope, sponsorRef) != ""
}

var _ artifacts.ReviewGrantClauser = (*artifactHost)(nil)

// ReviewGrantClause implements artifacts.ReviewGrantClauser (and, through
// MayGrantReview, artifacts.ReviewGrantAuthority; design D24,
// ptone/scion#4014, ptone/scion#5120): which clause, if any, lets the
// calling user give a user review access (a review grant)
// on an artifact owned by agent ownerAgentID and
// homed in project homeScope. Either is enough:
//   - the caller holds project.manage on homeScope (project owners and
//     admins, and super-admins), decided by AuthzService.CheckAccess like any
//     project administration, so a scoped token's own limits apply;
//   - the caller is the agent's delegating user. When the artifact records
//     one (sponsorRef, ptone/scion#6028), that recorded user, while active
//     and admitted to homeScope (recordedDelegator); the live chain is then
//     not consulted, so the user recorded at creation keeps the clause after
//     the agent is deleted or re-delegated, and no one else gains it. When
//     nothing is recorded (an artifact from before sponsors were recorded),
//     the user at the root of the agent's delegation chain, resolved now
//     from the recorded, active edges only (ResolveProvenanceRoot with no
//     legacy allowance: a revoked, missing, ambiguous or unrecorded edge, a
//     deleted agent on the chain, or a root user who is inactive or no
//     longer admitted to the agent's project resolves to no one). The
//     agent's stored owner or creator links are never consulted.
//
// A home project that no longer exists gives no one this authority. The
// first clause that holds is named: artifacts.ReviewClauseProjectAdmin,
// checked first, then artifacts.ReviewClauseDelegatingUser. Only users are
// answered; every other identity, a missing authz service, and every lookup
// error answer "".
func (h *artifactHost) ReviewGrantClause(ctx context.Context, ownerAgentID, homeScope, sponsorRef string) string {
	if h.server == nil || h.server.authzService == nil || ownerAgentID == "" || homeScope == "" {
		return ""
	}
	userID := artifactCallerUserID(ctx)
	if userID == "" {
		return ""
	}
	// A deleted home project gives no review authority, as it has no
	// stewards: a system role would otherwise still pass the admin clause,
	// and admission the recorded clause, for a project id that no longer
	// exists. A lookup failure answers "".
	if exists, err := h.ScopesExist(ctx, []string{homeScope}); err != nil || !exists[homeScope] {
		return ""
	}
	if h.AdministersScope(ctx, homeScope) {
		return artifacts.ReviewClauseProjectAdmin
	}
	if sponsorRef != "" {
		if h.recordedDelegator(ctx, userID, sponsorRef, homeScope) {
			return artifacts.ReviewClauseDelegatingUser
		}
		return ""
	}
	if h.liveDelegator(ctx, userID, ownerAgentID, "review grant") {
		return artifacts.ReviewClauseDelegatingUser
	}
	return ""
}

var _ artifacts.Stewardship = (*artifactHost)(nil)

// StewardClause implements artifacts.Stewardship (design D25,
// ptone/scion#5144): which steward clause, if any, the calling user holds
// on an artifact owned by agent ownerAgentID and homed in project
// homeScope. In order:
//   - artifacts.StewardClauseProjectOwner: Authorize(homeScope,
//     artifact.steward), which the project-owner role and super-admins hold
//     (and any custom role an operator gives it). Authorize starts with
//     Permits, so a scoped token must reach homeScope and carry
//     artifact:steward in its ceiling;
//   - artifacts.StewardClauseDelegator: the caller reads in homeScope
//     through a role (Authorize(homeScope, artifact.read), decided from role
//     bindings only, never from the artifact's own grants), and is the
//     agent's delegating user: the user recorded on the artifact at
//     creation when there is one (sponsorRef, ptone/scion#6028), while
//     active and admitted to homeScope (recordedDelegator), with the live
//     chain not consulted; otherwise the user at the root of the agent's
//     live, recorded delegation chain (ResolveProvenanceRoot with no legacy
//     allowance, as for the review clause). The role-read check binds the
//     authority to the artifact's current home, which a move can change.
//
// A home project that no longer exists has no stewards. orphaned (the
// owner is gone) is not consulted yet. Only users are answered; every
// other identity, a missing authz service and every lookup failure
// answer "".
func (h *artifactHost) StewardClause(ctx context.Context, ownerAgentID, homeScope, sponsorRef string, _ bool) string {
	if h.server == nil || h.server.authzService == nil || ownerAgentID == "" || homeScope == "" {
		return ""
	}
	userID := artifactCallerUserID(ctx)
	if userID == "" {
		return ""
	}
	// A deleted home project has no stewards (design 4.1): a super-admin's
	// system role would otherwise still pass the project-owner clause for
	// a project id that no longer exists. Custodians retire such artifacts.
	if exists, err := h.ScopesExist(ctx, []string{homeScope}); err != nil || !exists[homeScope] {
		return ""
	}
	if h.Authorize(ctx, homeScope, artifacts.PermissionSteward) {
		return artifacts.StewardClauseProjectOwner
	}
	if !h.Authorize(ctx, homeScope, artifacts.PermissionRead) {
		return ""
	}
	if sponsorRef != "" {
		if h.recordedDelegator(ctx, userID, sponsorRef, homeScope) {
			return artifacts.StewardClauseDelegator
		}
		return ""
	}
	if h.liveDelegator(ctx, userID, ownerAgentID, "steward") {
		return artifacts.StewardClauseDelegator
	}
	return ""
}

// artifactCallerUserID returns the id of the calling user (a session, a
// scoped user access token or the dev user), or "" for any other caller.
func artifactCallerUserID(ctx context.Context) string {
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return ""
	}
	switch id := identity.(type) {
	case *AuthenticatedUser, *ScopedUserIdentity, *DevUser:
		return id.ID()
	}
	return ""
}

// liveDelegator reports whether userID is the user at the root of agent
// ownerAgentID's live, recorded delegation chain (ResolveProvenanceRoot for
// artifact.manage, no legacy allowance). It decides the delegator clauses
// only for an artifact with no recorded delegating user. Any failure
// answers false. clause names the caller in the debug log.
func (h *artifactHost) liveDelegator(ctx context.Context, userID, ownerAgentID, clause string) bool {
	root, err := h.server.authzService.ResolveProvenanceRoot(ctx, ownerAgentID, ResolveProvenanceOptions{PermissionID: artifacts.PermissionManage})
	if err != nil {
		slog.DebugContext(ctx, "artifacts: "+clause+": delegating user not resolved",
			"agent_id", ownerAgentID, "source", "live", "cause", string(DenyCauseForProvenanceError(err)))
		return false
	}
	return root.RootUser != nil && root.RootUser.ID == userID
}

// recordedDelegator reports whether userID, the caller, is the delegating
// user recorded on an artifact (sponsorRef, ptone/scion#6028) and still
// stands as one: the home project homeScope exists; the user exists and is
// active; when it is the local
// development user, local-development authority is enabled on this
// server; and it is admitted to the artifact's current home homeScope for
// artifact.manage (ProjectAdmissionForClass, evaluated for that user with
// the requester's inputs masked, as ResolveProvenanceRoot admits a root
// user to the agent's project). Every check reads the store on each call;
// nothing is cached across requests, so a suspension or a lost membership
// takes effect on the next request. Any lookup failure answers false.
//
// The recorded user is not compared with the agent's live delegation
// chain: the creation-time value stands, through the agent's deletion and
// purge and through a later re-delegation. Callers add their own
// conditions (the steward clause's role read in homeScope).
func (h *artifactHost) recordedDelegator(ctx context.Context, userID, sponsorRef, homeScope string) bool {
	if userID == "" || sponsorRef == "" || userID != sponsorRef {
		return false
	}
	authz := h.server.authzService
	if authz.store == nil {
		return false
	}
	// A deleted home project ends the clause (as it ends every steward
	// clause): admission alone does not show that the project exists (a
	// system role, or role bindings not yet removed, may still admit to a
	// deleted project's id).
	if project, err := authz.store.GetProject(ctx, homeScope); err != nil || project == nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "artifacts: home project lookup for the recorded delegating user failed", "error", err)
		}
		return false
	}
	user, err := authz.store.GetUser(ctx, sponsorRef)
	if err != nil || user == nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "artifacts: recorded delegating user lookup failed", "error", err)
		}
		return false
	}
	if user.Status != store.UserStatusActive {
		return false
	}
	if user.ID == DevUserID && !authz.devLocalAuthorityEnabled() {
		return false
	}
	res, err := authz.ProjectAdmissionForClass(maskAuthzInputs(ctx), PrincipalContext{
		Kind:     PrincipalKindUser,
		ID:       user.ID,
		Identity: NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, ""),
	}, homeScope, artifacts.PermissionManage, executionProjectClass(artifacts.PermissionManage), nil)
	if err != nil {
		slog.WarnContext(ctx, "artifacts: recorded delegating user admission failed", "error", err)
		return false
	}
	if !res.Admitted {
		slog.DebugContext(ctx, "artifacts: recorded delegating user not admitted to the home project", "source", "recorded")
		return false
	}
	return true
}

var _ artifacts.SponsorRecorder = (*artifactHost)(nil)

// DelegatingUser implements artifacts.SponsorRecorder (ptone/scion#6028):
// the id of the user at the root of agent agentID's recorded delegation
// chain, by the same lookup the live delegator clauses use
// (ResolveProvenanceRoot for artifact.manage, no legacy allowance), to
// record on an artifact the agent creates and in the sponsor backfill. It
// depends on nothing of the caller (the root's admission is evaluated with
// the requester's inputs masked).
//
// A provenance denial (isProvenanceDenial: no, revoked, ambiguous or
// unrecorded edge, a deleted agent on the chain, an inactive or unadmitted
// root, a source not accepted here) answers ("", nil): there is no
// delegating user to record. Any other error, a store lookup fault
// included, is returned, so the backfill stops instead of recording that
// it finished.
func (h *artifactHost) DelegatingUser(ctx context.Context, agentID string) (string, error) {
	if h.server == nil || h.server.authzService == nil {
		return "", errors.New("artifacts: delegating user: authorization service not available")
	}
	root, err := h.server.authzService.ResolveProvenanceRoot(ctx, agentID, ResolveProvenanceOptions{PermissionID: artifacts.PermissionManage})
	if err != nil {
		if isProvenanceDenial(err) {
			return "", nil
		}
		return "", err
	}
	if root.RootUser == nil {
		return "", nil
	}
	return root.RootUser.ID, nil
}

// isProvenanceDenial reports whether err, from ResolveProvenanceRoot, is a
// decision that the agent has no delegating user rather than a failure to
// find out: every provenance sentinel except ErrProvenanceRequest.
func isProvenanceDenial(err error) bool {
	for _, target := range []error{
		ErrProvenanceInactive, ErrProvenanceMissing, ErrProvenanceAmbiguous, ErrProvenanceChain,
		ErrProvenanceUnrecorded, ErrProvenanceMismatch, ErrProvenanceNotAdmitted, ErrProvenanceSourceNotAllowed,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// MemberScopes returns the projects the caller belongs to: for a user (a
// session, a scoped user access token or the dev user), every project it
// holds an active project-scoped binding of a role that confers membership
// in, directly or through a group; for a token-backed agent, its own project. It reads live bindings
// on every call, so a user who has left a project stops matching that
// project's scope grants at once. The artifact service uses the result only
// to bound list candidates; every candidate is re-checked like a GET, so a
// token's boundary or ceiling still applies through Permits.
func (h *artifactHost) MemberScopes(ctx context.Context) ([]string, error) {
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return nil, nil
	}
	switch id := identity.(type) {
	case *agentIdentityWrapper:
		if p := id.ProjectID(); p != "" {
			return []string{p}, nil
		}
		return nil, nil
	case *AuthenticatedUser, *ScopedUserIdentity, *DevUser:
	default:
		return nil, nil
	}
	if h.server == nil || h.server.authzService == nil {
		return nil, nil
	}
	in := h.server.authzService.inputsFor(ctx, identity)
	if _, err := in.Principals(); err != nil {
		return nil, err
	}
	bindings, err := in.Bindings()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var active []*store.RoleBinding
	for _, rb := range bindings {
		if rb == nil || rb.ScopeType != store.RoleScopeProject || rb.ScopeID == "" {
			continue
		}
		if (rb.NotBefore != nil && now.Before(*rb.NotBefore)) || (rb.ExpiresAt != nil && !now.Before(*rb.ExpiresAt)) {
			continue
		}
		active = append(active, rb)
	}
	// Only a role that confers membership makes a member scope: a
	// project-visitor binding does not.
	active, err = membershipBindings(active, func([]string) (map[string]*RolePermissions, error) { return in.RoleDefs() })
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, rb := range active {
		if seen[rb.ScopeID] {
			continue
		}
		seen[rb.ScopeID] = true
		out = append(out, rb.ScopeID)
	}
	return out, nil
}

// artifactListEndpoint names the artifact list in cursor bindings.
const artifactListEndpoint = "artifacts.list"

// SealCursor seals a list position with the hub's list-cursor key, bound
// to the caller's credential and to binding, in the same format the hub's
// other authorized lists use, so the cursor reveals nothing about the row
// it points after.
func (h *artifactHost) SealCursor(ctx context.Context, position, binding string) (string, error) {
	if h.server == nil {
		return "", errListCursorSealerUnavailable
	}
	b := scopedCursorBinding(artifactListEndpoint, binding, GetIdentityFromContext(ctx))
	inner := base64.URLEncoding.EncodeToString([]byte(position + "," + b))
	return h.server.listCursorSealer.Seal(inner, b)
}

// OpenCursor reverses SealCursor. Any failure is errInvalidCursor.
func (h *artifactHost) OpenCursor(ctx context.Context, cursor, binding string) (string, error) {
	if h.server == nil {
		return "", errInvalidCursor
	}
	b := scopedCursorBinding(artifactListEndpoint, binding, GetIdentityFromContext(ctx))
	inner, err := openAndValidateListCursor(h.server.listCursorSealer, cursor, b)
	if err != nil {
		return "", errInvalidCursor
	}
	raw, err := base64.URLEncoding.DecodeString(inner)
	if err != nil {
		return "", errInvalidCursor
	}
	position, ok := strings.CutSuffix(string(raw), ","+b)
	if !ok {
		return "", errInvalidCursor
	}
	return position, nil
}

var _ artifacts.CrossScopeSharing = (*artifactHost)(nil)

// CrossScopeSharingAllowed implements artifacts.CrossScopeSharing:
// sharing an artifact with another project follows the hub's
// messaging.cross_project_messaging_enabled setting, and is off when the
// hub has no operational settings (fail closed).
func (h *artifactHost) CrossScopeSharingAllowed(context.Context) bool {
	if h.server == nil {
		return false
	}
	ops := h.server.GetOperationalSettings()
	return ops != nil && ops.CrossProjectMessagingEnabled()
}

var _ artifacts.ScopeChecker = (*artifactHost)(nil)

// ScopesExist implements artifacts.ScopeChecker: a project exists while the
// store has it. The hub deletes projects outright, so a missing one is
// gone. One store query covers the page: refs holds at most one id per
// row of a list page (artifacts.MaxListLimit), well under the store's
// project list limit. A store error fails the call.
//
// A ref made by artifacts.MessageScopeRef names a message instead: it
// exists while the store has it and it is not soft-deleted
// (messagesExist). The maintenance pass asks about at most one page of
// message refs at a time.
//
// A host without a server or a store answers an error for any non-empty
// refs, never a panic; callers treat it as a failed lookup.
func (h *artifactHost) ScopesExist(ctx context.Context, refs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	// No server or no store: fail closed with an error, never a nil
	// dereference. Every caller treats an error as "cannot tell", and the
	// steward and review clauses deny on it.
	if h.server == nil || h.server.store == nil {
		slog.WarnContext(ctx, "artifacts: scope existence check without a store")
		return nil, errors.New("artifacts: scope existence check: no store")
	}
	var projects, messages []string
	for _, ref := range refs {
		if id, ok := artifacts.ParseMessageScopeRef(ref); ok {
			messages = append(messages, id)
		} else {
			projects = append(projects, ref)
		}
	}
	if len(projects) > 0 {
		res, err := h.server.store.ListProjectSummaries(ctx, store.ProjectFilter{MemberProjectIDs: projects},
			store.ListOptions{Limit: len(projects), SkipTotalCount: true})
		if err != nil {
			return nil, err
		}
		found := make(map[string]bool, len(res.Items))
		for _, p := range res.Items {
			found[strings.ToLower(p.ID)] = true
		}
		for _, ref := range projects {
			out[ref] = found[strings.ToLower(ref)]
		}
	}
	if len(messages) > 0 {
		live, err := h.messagesExist(ctx, messages)
		if err != nil {
			return nil, err
		}
		for _, id := range messages {
			out[artifacts.MessageScopeRef(id)] = live[id]
		}
	}
	return out, nil
}

// messagesExist reports, for each id, whether the message is stored and
// not soft-deleted on the web-chat channel: one message lookup and one
// extension lookup for all of them. A store error fails the call.
func (h *artifactHost) messagesExist(ctx context.Context, ids []string) (map[string]bool, error) {
	s := h.server
	msgs, err := s.store.GetMessagesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	var exts map[string]*WebChatMessageExt
	if wcs != nil {
		if exts, err = wcs.GetMessageExts(ctx, ids); err != nil {
			return nil, err
		}
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		m := msgs[id]
		ext := exts[id]
		out[id] = m != nil && (ext == nil || ext.DeletedAt == nil)
	}
	return out, nil
}

var _ artifacts.ScopeExplainer = (*artifactHost)(nil)

// MissingScope implements artifacts.ScopeExplainer: the first scope
// MissingScopes reports, or "".
func (h *artifactHost) MissingScope(ctx context.Context, permission string) string {
	if missing := h.MissingScopes(ctx, permission); len(missing) > 0 {
		return missing[0]
	}
	return ""
}

var _ artifacts.ScopesExplainer = (*artifactHost)(nil)

// MissingScopes implements artifacts.ScopesExplainer. Only an agent that
// presented a real token (one with a token id) gets an answer: every scope
// it lacks among the scope that Principal requires before serving it and
// the token scopes the permission maps to, in that order (ptone/scion#4571).
// Users and unauthenticated callers get nil.
func (h *artifactHost) MissingScopes(ctx context.Context, permission string) []string {
	perm, ok := artifactPermission(permission)
	if !ok {
		return nil
	}
	agent, ok := GetIdentityFromContext(ctx).(*agentIdentityWrapper)
	if !ok || agent == nil || agent.AgentTokenClaims == nil || agent.ID() == "" || agent.TokenID() == "" {
		return nil
	}
	var missing []string
	if !agentHasAnyScope(agent, []string{string(ScopeProjectArtifactRead)}) {
		missing = append(missing, string(ScopeProjectArtifactRead))
	}
	if len(perm.AgentScopes) > 0 && !agentHasAnyScope(agent, perm.AgentScopes) && !slices.Contains(missing, perm.AgentScopes[0]) {
		missing = append(missing, perm.AgentScopes[0])
	}
	return missing
}

// artifactPermission returns the registry row for id when it is an artifact
// permission.
func artifactPermission(id string) (permissions.Permission, bool) {
	for _, p := range permissions.Registry {
		if p.ID == id {
			return p, p.Resource == permissions.ResourceArtifact
		}
	}
	return permissions.Permission{}, false
}

// agentHasAnyScope reports whether the agent's effective token scopes
// include one of scopes. An empty scopes list is never satisfied.
func agentHasAnyScope(agent AgentIdentity, scopes []string) bool {
	have := effectiveAgentScopes(agent)
	for _, s := range scopes {
		if slices.Contains(have, AgentTokenScope(s)) {
			return true
		}
	}
	return false
}

// artifactsGuard is the artifacts.Guard the hub mounts the artifact service
// with. The hub.artifacts experiment is checked first, per request, so every
// artifact route (including the share-link route) answers 404 while it is
// off, before any authentication outcome is visible. The artifacts settings
// section's enabled switch (false also when the section is malformed) closes
// the routes the same way. Then the route goes
// through the declarative route guard for its routeMetadataTable row.
//
// The experiment check is per request rather than requireExperiment because
// requireExperiment panics at registration when the server's registry lacks
// the name, which breaks every server built over a test registry; hub.conduit
// and the gcs/object route gate the same way for the same reason.
func (s *Server) artifactsGuard(pattern string, handler http.Handler) http.Handler {
	guarded := s.guarded(pattern, handler.ServeHTTP)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.experimentEnabled(experiments.Artifacts) || !s.artifactsConfig().Enabled {
			NotFound(w, "route")
			return
		}
		guarded(w, r)
	})
}

// isArtifactViewRequest reports whether r is a read of the artifact view
// route (artifacts.RouteView); see isArtifactCapabilityRequest.
func isArtifactViewRequest(r *http.Request) bool {
	return isArtifactCapabilityRequest(r, artifacts.RouteView)
}

// isArtifactSharedRequest reports whether r is a read of the share-link
// route (artifacts.RouteShared); see isArtifactCapabilityRequest.
func isArtifactSharedRequest(r *http.Request) bool {
	return isArtifactCapabilityRequest(r, artifacts.RouteShared)
}

// isArtifactCapabilityRequest reports whether r is a read under route, an
// artifact route that authenticates by a capability (a view capability or
// a share-link token) in the path segment after route. Both the decoded and
// the escaped path must be clean and under the route, the escaped path may
// not encode a slash, dot, backslash or NUL (in any letter case), and the
// capability segment may not be escaped at all,
// so the request this check admits is the one the mux routes to the route.
// Only the request shape is checked here; the artifact service verifies
// the capability.
func isArtifactCapabilityRequest(r *http.Request, route string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	esc := r.URL.EscapedPath()
	if !strings.HasPrefix(r.URL.Path, route) || !strings.HasPrefix(esc, route) {
		return false
	}
	if path.Clean(r.URL.Path) != r.URL.Path || path.Clean(esc) != esc {
		return false
	}
	lower := strings.ToLower(esc)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%2e") || strings.Contains(lower, "%5c") ||
		strings.Contains(lower, "%00") || strings.IndexByte(r.URL.Path, 0) >= 0 {
		return false
	}
	capability, _, _ := strings.Cut(esc[len(route):], "/")
	return capability != "" && !strings.Contains(capability, "%")
}

var _ artifacts.PrincipalChecker = (*artifactHost)(nil)

// PrincipalsExist implements artifacts.PrincipalChecker for the owner
// liveness reconciler. An agent is live while its row exists and is not
// soft-deleted; a user is live while its row exists with status active.
// Each call costs at most two store queries whatever the number of refs:
// one agent list by id (soft-deleted rows excluded) and one user lookup by
// id. A ref of another kind, or whose id is not a UUID (store ids always
// are), is left out of the answer, so it counts as live. A store error
// fails the call, and the reconciler then changes nothing.
func (h *artifactHost) PrincipalsExist(ctx context.Context, refs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(refs))
	if h.server == nil || h.server.store == nil || len(refs) == 0 {
		return out, nil
	}
	var agentIDs, userIDs []string
	for _, ref := range refs {
		kind, id, ok := strings.Cut(ref, ":")
		if _, err := uuid.Parse(id); !ok || err != nil {
			continue
		}
		switch kind {
		case artifacts.PrincipalKindAgent:
			agentIDs = append(agentIDs, id)
		case artifacts.PrincipalKindUser:
			userIDs = append(userIDs, id)
		}
	}
	if len(agentIDs) > 0 {
		res, err := h.server.store.ListAgents(ctx, store.AgentFilter{IDs: agentIDs},
			store.ListOptions{Limit: len(agentIDs), SkipTotalCount: true})
		if err != nil {
			return nil, err
		}
		live := make(map[string]bool, len(res.Items))
		for _, a := range res.Items {
			if a.DeletedAt.IsZero() {
				live[strings.ToLower(a.ID)] = true
			}
		}
		for _, id := range agentIDs {
			out[artifacts.PrincipalRef(artifacts.PrincipalKindAgent, id)] = live[strings.ToLower(id)]
		}
	}
	if len(userIDs) > 0 {
		users, err := h.server.store.GetUsersByIDs(ctx, userIDs)
		if err != nil {
			return nil, err
		}
		live := make(map[string]bool, len(users))
		for _, u := range users {
			if u != nil && u.Status == store.UserStatusActive {
				live[strings.ToLower(u.ID)] = true
			}
		}
		for _, id := range userIDs {
			out[artifacts.PrincipalRef(artifacts.PrincipalKindUser, id)] = live[strings.ToLower(id)]
		}
	}
	return out, nil
}

var _ artifacts.ScopeAdministrator = (*artifactHost)(nil)

// AdministersScope implements artifacts.ScopeAdministrator: the calling
// user holds project.manage on the project scopeRef (the project owners
// and admins of the artifact's home project, and super-admins; hub-admins
// do not hold it), decided by AuthzService.CheckAccess like any
// project administration, so a scoped token's own limits apply. Only users
// are answered; every other identity and a missing authz service answer
// false. It is the first custodian clause of the delete route and the
// project-admin half of ReviewGrantClause.
func (h *artifactHost) AdministersScope(ctx context.Context, scopeRef string) bool {
	if h.server == nil || h.server.authzService == nil || scopeRef == "" {
		return false
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return false
	}
	switch id := identity.(type) {
	case *AuthenticatedUser, *ScopedUserIdentity, *DevUser:
		if id.ID() == "" {
			return false
		}
	default:
		return false
	}
	return h.server.authzService.CheckAccess(ctx, identity, Resource{Type: permissions.ResourceProject, ID: scopeRef}, ActionManage).Allowed
}

var _ artifacts.MessageScopeHost = (*artifactHost)(nil)

// PermitsMessageRead implements artifacts.MessageScopeHost: whether the
// caller's credential allows reading attachments stored as artifacts at
// all, classified the way the inbox and conversation routes classify it
// (inboxCredentialOf):
//   - a session (the session allowlist) of a user or the dev user: yes;
//   - an agent: only while it is in good standing (agentStanding,
//     ptone/scion#3433). The check sits here, not in MayReadMessage, so it
//     covers every agent read, the upload scope included; a lookup fault
//     refuses. Callers that ask twice for one request install
//     withStandingMemo. Agents have no attachment read route: they receive
//     attachments by delivery, which asks this decision for the recipient
//     (ptone/scion#4596);
//   - a user access token: not yet (ptone/scion#4596, section 9);
//   - federated users and visitors, brokers acting for a user, and every
//     other credential: no.
func (h *artifactHost) PermitsMessageRead(ctx context.Context) bool {
	cred, _ := inboxCredentialOf(ctx)
	switch cred {
	case inboxCredentialSession:
		switch id := GetIdentityFromContext(ctx).(type) {
		case *AuthenticatedUser, *DevUser:
			return id.ID() != ""
		}
		return false
	case inboxCredentialAgent:
		agent := GetAgentIdentityFromContext(ctx)
		if agent == nil || agent.ID() == "" || h.server == nil {
			return false
		}
		return h.server.agentStanding(ctx, agent.ID()) == nil
	case inboxCredentialToken:
		// Clause 1 of the token rule (ptone/scion#4596, section 9): the
		// token carries inbox:read, eligible for its boundary, and a
		// project-boundary holder is a current member of that project.
		token := chatToken(ctx)
		if token == nil || h.server == nil {
			return false
		}
		return h.server.newSelfScopeCheck(ctx, token, permInboxRead).allows(tokenBoundaryProject(token))
	default:
		return false
	}
}

// MayReadMessage implements artifacts.MessageScopeHost: whether the caller
// may read message messageID now. The message must exist and not be
// deleted; then, by credential class:
//   - a session user reads it through its conversation or thread
//     (canReadNativeMessage: DM participants, project readers for a
//     project thread, active participants of a group without a project),
//     or is one of its user addressees;
//   - an agent reads it through its conversation (canUserReadMessage: a
//     DM party by key; a group of its own project), or is one of its
//     addressees (attachmentAgentAddressee).
//
// messageProjectID is not read by these classes. It answers only for the
// identity of the request (as PermitsMessageRead, which it asks first),
// and every lookup failure answers false.
func (h *artifactHost) MayReadMessage(ctx context.Context, messageID string, messageProjectID *string) bool {
	if h.server == nil || messageID == "" || !h.PermitsMessageRead(ctx) {
		return false
	}
	s := h.server
	msg, ok := s.liveMessageForAttachments(ctx, messageID)
	if !ok {
		return false
	}
	cred, _ := inboxCredentialOf(ctx)
	switch cred {
	case inboxCredentialSession:
		user := GetUserIdentityFromContext(ctx)
		if user == nil || user.ID() == "" {
			return false
		}
		return s.canReadNativeMessage(ctx, user, msg) || s.attachmentUserAddressee(ctx, messageID, user.ID())
	case inboxCredentialAgent:
		agent := GetAgentIdentityFromContext(ctx)
		if agent == nil || agent.ID() == "" {
			return false
		}
		if s.canUserReadMessage(ctx, agent, msg) {
			return true
		}
		return s.attachmentAgentAddressee(ctx, msg, agent.ID())
	case inboxCredentialToken:
		return s.tokenMayReadAttachmentMessage(ctx, msg, messageProjectID)
	default:
		return false
	}
}

// liveMessageForAttachments returns message messageID when it exists and
// is not deleted on the web chat channel. Any lookup failure answers false.
func (s *Server) liveMessageForAttachments(ctx context.Context, messageID string) (*store.Message, bool) {
	msg, err := s.store.GetMessage(ctx, messageID)
	if err != nil || msg == nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "artifacts: attachment message lookup failed", "message", messageID, "error", err)
		}
		return nil, false
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs != nil {
		ext, err := wcs.GetMessageExt(ctx, messageID)
		if err != nil {
			slog.WarnContext(ctx, "artifacts: attachment message state lookup failed", "message", messageID, "error", err)
			return nil, false
		}
		if ext != nil && ext.DeletedAt != nil {
			return nil, false
		}
	}
	return msg, true
}

// attachmentAgentAddressee reports whether agent agentID is an addressee of
// msg for its attachments (ptone/scion#4596, design section 6.4): the
// message row's own recipient, or a message_addressees row for the agent
// whose delivery state is pending or delivered. A failed row grants
// nothing. A lookup failure answers false.
func (s *Server) attachmentAgentAddressee(ctx context.Context, msg *store.Message, agentID string) bool {
	if kind, ok := messages.PrincipalKindFromAddress(msg.Recipient); ok && kind == artifacts.PrincipalKindAgent && msg.RecipientID == agentID {
		return true
	}
	addressees, err := s.store.ListAddressees(ctx, msg.ID)
	if err != nil {
		slog.WarnContext(ctx, "artifacts: attachment addressee lookup failed", "message", msg.ID, "error", err)
		return false
	}
	for _, a := range addressees {
		if a.PrincipalKind != artifacts.PrincipalKindAgent || a.PrincipalID != agentID {
			continue
		}
		if a.DeliveryState == addresseePending || a.DeliveryState == addresseeDelivered {
			return true
		}
	}
	return false
}

// Delivery states of a message_addressees row.
const (
	addresseePending   = "pending"
	addresseeDelivered = "delivered"
	addresseeFailed    = "failed"
)

// attachmentUserAddressee reports whether user userID is one of the user
// addressees of message messageID. A lookup failure answers false.
func (s *Server) attachmentUserAddressee(ctx context.Context, messageID, userID string) bool {
	addressees, err := s.store.ListAddressees(ctx, messageID)
	if err != nil {
		slog.WarnContext(ctx, "artifacts: attachment addressee lookup failed", "message", messageID, "error", err)
		return false
	}
	for _, a := range addressees {
		if a.PrincipalKind == artifacts.PrincipalKindUser && a.PrincipalID == userID {
			return true
		}
	}
	return false
}

// tokenMayReadAttachmentMessage is the token branch of MayReadMessage
// (ptone/scion#4596, section 9). Clause 1 ran in PermitsMessageRead and
// clause 2 (a live message) in MayReadMessage; then every one of these
// must hold:
//  3. the holder reads the message: through its conversation
//     (canReadNativeMessage with the token identity, so the token's
//     ceiling applies), or as one of its user addressees;
//  4. the attachments record the message's project (not nil), and the
//     token's inbox:read self-scope check allows it ("" needs a hub
//     boundary);
//  5. for a direct conversation, agent:read on every agent party of its
//     key (dmAgentParties), whichever party the holder is; a missing agent
//     row refuses.
//
// It is not tokenMayUseConversation: the project is the one stored at
// bind, and the agent requirement does not depend on the caller.
func (s *Server) tokenMayReadAttachmentMessage(ctx context.Context, msg *store.Message, messageProjectID *string) bool {
	token := chatToken(ctx)
	if token == nil || token.ID() == "" {
		return false
	}
	if !s.canReadNativeMessage(ctx, token, msg) && !s.attachmentUserAddressee(ctx, msg.ID, token.ID()) {
		return false
	}
	if messageProjectID == nil || !s.newSelfScopeCheck(ctx, token, permInboxRead).allows(*messageProjectID) {
		return false
	}
	conv, err := s.attachmentMessageConversation(ctx, msg)
	if err != nil {
		slog.WarnContext(ctx, "artifacts: attachment conversation lookup failed", "message", msg.ID, "error", err)
		return false
	}
	if conv == nil || conv.Kind != "direct" {
		return true
	}
	parties, err := dmAgentParties(conv)
	if err != nil {
		return false
	}
	for _, id := range parties {
		agent, err := s.store.GetAgent(ctx, id)
		if err != nil || agent == nil {
			return false
		}
		if !s.authzService.CheckAccess(ctx, token, agentResource(agent), ActionRead).Allowed {
			return false
		}
	}
	return true
}

// attachmentMessageConversation returns the conversation of msg: its
// stored conversation, or for a message without one in a DM thread, a
// direct conversation of the thread's key. It returns nil when neither
// applies.
func (s *Server) attachmentMessageConversation(ctx context.Context, msg *store.Message) (*store.Conversation, error) {
	if msg.ConversationID != "" {
		return s.store.GetConversation(ctx, msg.ConversationID)
	}
	if strings.HasPrefix(msg.ThreadID, "dm:") {
		return &store.Conversation{Kind: "direct", ExternalRef: msg.ThreadID}, nil
	}
	return nil, nil
}

// PermitsUploadRead implements artifacts.MessageScopeHost (ptone/scion#4596,
// section 9, clause 0): whether the uploader's credential reads its own
// unsent upload. Sessions and agents in standing do; a user access token
// does only when its inbox:read self-scope check allows a record with no
// project (a hub boundary), as for a project-less upload in the
// AttachmentStore.
func (h *artifactHost) PermitsUploadRead(ctx context.Context) bool {
	cred, _ := inboxCredentialOf(ctx)
	switch cred {
	case inboxCredentialSession, inboxCredentialAgent:
		return h.PermitsMessageRead(ctx)
	case inboxCredentialToken:
		token := chatToken(ctx)
		if token == nil || h.server == nil {
			return false
		}
		return h.PermitsMessageRead(ctx) && h.server.newSelfScopeCheck(ctx, token, permInboxRead).allows("")
	}
	return false
}
