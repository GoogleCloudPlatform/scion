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
	"net/http"
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// artifactHost is the hub's implementation of artifacts.Host: it answers the
// artifact service's three questions from the hub's identity context and authz
// engine, and turns hub types into the strings the service sees.
type artifactHost struct {
	server *Server
}

var _ artifacts.Host = (*artifactHost)(nil)

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
	if h.server == nil || h.server.authzService == nil {
		return false
	}
	project := agent.ProjectID()
	if project == "" || agent.ID() == "" {
		return false
	}
	resource := Resource{Type: permissions.ResourceArtifact, ParentType: permissions.ResourceProject, ParentID: project}
	allowed, _, err := h.server.authzService.walkDelegationChainWithCause(ctx, resource, Action(perm.Action), perm.ID,
		agent.ID(), AncestryIsHubAttested(agent), store.RoleScopeProject, project, nil, nil)
	return err == nil && allowed
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

// artifactPermission returns the registry row for id when it is an artifact
// permission.
var _ artifacts.ScopeExplainer = (*artifactHost)(nil)

// MissingScope implements artifacts.ScopeExplainer. Only an agent that
// presented a real token (one with a token id) gets an answer: the scope
// that Principal requires before serving it, then the token scopes the
// permission maps to. Users and unauthenticated callers get "".
func (h *artifactHost) MissingScope(ctx context.Context, permission string) string {
	perm, ok := artifactPermission(permission)
	if !ok {
		return ""
	}
	agent, ok := GetIdentityFromContext(ctx).(*agentIdentityWrapper)
	if !ok || agent == nil || agent.AgentTokenClaims == nil || agent.ID() == "" || agent.TokenID() == "" {
		return ""
	}
	if !agentHasAnyScope(agent, []string{string(ScopeProjectArtifactRead)}) {
		return string(ScopeProjectArtifactRead)
	}
	if len(perm.AgentScopes) > 0 && !agentHasAnyScope(agent, perm.AgentScopes) {
		return perm.AgentScopes[0]
	}
	return ""
}

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
