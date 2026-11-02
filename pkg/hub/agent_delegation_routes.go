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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// delegatedRouteID names one exact route resolution the delegated-credential
// admission table can match: an agent sub-route RouteID (agent_routes.go),
// or one of the exact non-agent resolutions below.
type delegatedRouteID string

// projectResourceRouteID is the exact resolution of the project resource
// path /api/v1/projects/{id} (no sub-path, no trailing slash). The mux
// pattern "projects.byId" covers every path under /api/v1/projects/, so the
// admission table never matches that coarse pattern; it matches only this
// resolution (.design/agent-delegation.md §11.1).
const projectResourceRouteID delegatedRouteID = "projects.byId.resource"

// opProjectRead is the catalog operation of GET /api/v1/projects/{id}.
const opProjectRead authzop.OperationID = "project.read"

// agentsCollectionRouteID is the exact resolution of the hub agent list path
// /api/v1/agents (no sub-path, no trailing slash). The admission table
// matches only this resolution of the "agents.list" mux pattern.
const agentsCollectionRouteID delegatedRouteID = "agents.list.collection"

// opAgentList is the catalog operation of GET /api/v1/agents.
const opAgentList authzop.OperationID = "agent.list"

// agentCollectionTargetType is the route target type of a list route: the
// collection the list reads, never one of its items and never a resource
// type. Capabilities computed for a list item never match it.
const agentCollectionTargetType = "agent_collection"

// hubAgentCollectionTarget is the route target of the hub agent list: the
// hub's agent collection.
var hubAgentCollectionTarget = delegatedRouteTarget{Type: agentCollectionTargetType, ID: "hub"}

// delegatedRouteTarget is the resource a delegated request's route resolves
// to: the agent or project named in the path.
type delegatedRouteTarget struct {
	Type string
	// ID is the path segment that names the target. On the project form of
	// an agent sub-route it is the agent's ID or its slug in ProjectID.
	ID string
	// ProjectID is the project of a project-form agent sub-route, resolved
	// the way handleProjectAgents resolves it; empty on every other route.
	ProjectID string
}

// namesAgent reports whether the route target names agent: by ID on any
// agent route, or by slug inside the agent's own project on the project
// form. A project-form target also requires the agent's project.
func (t delegatedRouteTarget) namesAgent(agent *store.Agent) bool {
	if t.Type != "agent" || t.ID == "" || agent == nil || agent.ID == "" {
		return false
	}
	if t.ProjectID == "" {
		return t.ID == agent.ID
	}
	if t.ProjectID != agent.ProjectID {
		return false
	}
	return t.ID == agent.ID || (agent.Slug != "" && t.ID == agent.Slug)
}

// delegatedRoute is the single server-owned resolution of a request that
// the admission table is matched against.
type delegatedRoute struct {
	RouteID     delegatedRouteID
	OperationID authzop.OperationID
	Target      delegatedRouteTarget
}

// agentDelegationAdmission is one entry of the delegated-credential route
// admission table (.design/agent-delegation.md §11.1).
type agentDelegationAdmission struct {
	// Method and RouteID name one exact route resolution, never a prefix.
	Method  string
	RouteID delegatedRouteID
	// Operation is the catalog operation the table supplies for the route.
	// It must equal the resolved route's operation; an empty resolved
	// operation never admits.
	Operation authzop.OperationID
	// Secondary lists extra permissions a handler on this route may check
	// with a second Decide. Listing confers nothing: each check still
	// needs its own permission in the ceiling and the issuer's live
	// authority.
	Secondary []string
	// ItemFilter is the per-item permission of a list route: every item
	// the list could return is decided for it, and only allowed items are
	// returned and counted. It is accepted only by the list's item filter
	// (delegatedListItemRead), never by Decide: a request that asks Decide
	// for ItemFilter on this route is still refused. Empty on every
	// route that is not a list.
	ItemFilter string
}

// agentDelegationAdmittedRoutes is the code-owned, deny-by-default list of
// routes that admit an agent delegated credential. A delegated request on
// any other route answers 403 credential_not_admitted in routeGuard.
// TestAgentDelegationAdmission_MatchesCatalog pins it against the catalog in
// both directions, and TestAgentDelegationHandlerBranches_MatchCatalog pins
// it against the handlers' explicit delegated branches.
var agentDelegationAdmittedRoutes = []agentDelegationAdmission{
	{Method: http.MethodGet, RouteID: delegatedRouteID(AgentRouteRoot), Operation: opAgentRead},
	{Method: http.MethodGet, RouteID: projectResourceRouteID, Operation: opProjectRead},
	{Method: http.MethodPost, RouteID: delegatedRouteID(AgentRouteActionMessage), Operation: opAgentActionMessage},
	{Method: http.MethodPost, RouteID: delegatedRouteID(ProjectAgentRouteActionMessage), Operation: opAgentActionMessage},
	// The PTY WebSocket handshake. The lifecycle operations that share
	// agent.attach (exec, env, resetauth, keys) are not admitted.
	{Method: http.MethodGet, RouteID: delegatedRouteID(AgentRoutePTY), Operation: opAgentAttach},
	// The hub agent list. Each listed agent is decided for agent.read.
	{Method: http.MethodGet, RouteID: agentsCollectionRouteID, Operation: opAgentList, ItemFilter: "agent.read"},
}

// agentDelegationAdmissionFor returns the admission entry for method and
// routeID.
func agentDelegationAdmissionFor(method string, routeID delegatedRouteID) (agentDelegationAdmission, bool) {
	for _, entry := range agentDelegationAdmittedRoutes {
		if entry.Method == method && entry.RouteID == routeID {
			return entry, true
		}
	}
	return agentDelegationAdmission{}, false
}

// resolveProjectResourceRoute resolves method and escapedPath (the request's
// URL.EscapedPath()) to the exact project resource route: exactly
// /api/v1/projects/{id}, one acceptable segment, no sub-path and no
// trailing slash. Only GET has an operation. The project ID is resolved the
// way handleProjectRoutes resolves it ({uuid}__{slug} → uuid). It performs
// no authorization and no store reads.
func resolveProjectResourceRoute(method, escapedPath string) (delegatedRoute, bool) {
	raw, ok := strings.CutPrefix(escapedPath, "/api/v1/projects/")
	if !ok || strings.Contains(raw, "/") {
		return delegatedRoute{}, false
	}
	decoded, ok := decodeAgentRouteSegment(raw)
	if !ok || decoded == "register" {
		return delegatedRoute{}, false
	}
	route := delegatedRoute{
		RouteID: projectResourceRouteID,
		Target:  delegatedRouteTarget{Type: "project", ID: resolveProjectID(decoded)},
	}
	if method == http.MethodGet {
		route.OperationID = opProjectRead
	}
	return route, true
}

// resolveAgentsCollectionRoute resolves method and escapedPath to the exact
// hub agent list route: exactly /api/v1/agents. Only GET has an operation.
// The target is the hub's agent collection. It performs no authorization
// and no store reads.
func resolveAgentsCollectionRoute(method, escapedPath string) (delegatedRoute, bool) {
	if escapedPath != "/api/v1/agents" {
		return delegatedRoute{}, false
	}
	route := delegatedRoute{
		RouteID: agentsCollectionRouteID,
		Target:  hubAgentCollectionTarget,
	}
	if method == http.MethodGet {
		route.OperationID = opAgentList
	}
	return route, true
}

// resolveDelegatedRoute returns the single resolution of a delegated request
// on the mux route meta: the agent sub-route resolution (the one dispatch
// uses) on the agent sub-route patterns, the exact project resource route
// on the project pattern, the exact hub agent list route on the agent list
// pattern, and nothing anywhere else.
func resolveDelegatedRoute(meta RouteMetadata, method, escapedPath string) (delegatedRoute, *AgentSubRoute, bool) {
	if agentSubRouteGuardedRoutes[meta.RouteID] && isAgentSubRoutePath(escapedPath) {
		route, ok := ResolveAgentSubRoute(method, escapedPath)
		if !ok {
			return delegatedRoute{}, nil, false
		}
		target := delegatedRouteTarget{Type: "agent", ID: route.AgentID}
		if strings.HasPrefix(escapedPath, "/api/v1/projects/") {
			if route.ProjectID == "" {
				return delegatedRoute{}, nil, false
			}
			target.ProjectID = resolveProjectID(route.ProjectID)
		}
		return delegatedRoute{
			RouteID:     delegatedRouteID(route.RouteID),
			OperationID: route.OperationID,
			Target:      target,
		}, &route, true
	}
	if meta.RouteID == "projects.byId" {
		route, ok := resolveProjectResourceRoute(method, escapedPath)
		return route, nil, ok
	}
	if meta.RouteID == "agents.list" {
		route, ok := resolveAgentsCollectionRoute(method, escapedPath)
		return route, nil, ok
	}
	return delegatedRoute{}, nil, false
}

// admitDelegatedRequest is routeGuard's admission check for a request whose
// identity is an agent delegated credential. It resolves the request once,
// looks the resolution up in the admission table and, on a match, stores
// the table entry and the route target in the request context. Any other
// route, an unresolved route, or a resolution whose operation is empty or
// differs from the table's answers 403 credential_not_admitted. It reports
// whether the request may continue.
func (s *Server) admitDelegatedRequest(w http.ResponseWriter, r *http.Request, meta RouteMetadata) (*http.Request, bool) {
	refuse := func() (*http.Request, bool) {
		writeDelegatedNotAdmitted(w)
		return nil, false
	}
	route, agentRoute, ok := resolveDelegatedRoute(meta, r.Method, r.URL.EscapedPath())
	if !ok || route.OperationID == "" || route.Target.ID == "" {
		return refuse()
	}
	entry, ok := agentDelegationAdmissionFor(r.Method, route.RouteID)
	if !ok || entry.Operation != route.OperationID {
		return refuse()
	}
	ctx := r.Context()
	if agentRoute != nil {
		ctx = withAgentSubRoute(ctx, *agentRoute)
	}
	ctx = contextWithDelegatedTarget(contextWithDelegatedAdmission(ctx, entry), route.Target)
	if entry.ItemFilter != "" {
		ctx = contextWithListFilterCollector(ctx, newListFilterCollector(entry.ItemFilter))
	}
	return r.WithContext(ctx), true
}

// delegatedRouteTargetKey holds the route target routeGuard resolved for an
// admitted delegated request. Only routeGuard sets it.
type delegatedRouteTargetKey struct{}

func contextWithDelegatedTarget(ctx context.Context, target delegatedRouteTarget) context.Context {
	return context.WithValue(ctx, delegatedRouteTargetKey{}, target)
}

// delegatedTargetFromContext returns the route target routeGuard stored for
// an admitted delegated request.
func delegatedTargetFromContext(ctx context.Context) (delegatedRouteTarget, bool) {
	target, ok := ctx.Value(delegatedRouteTargetKey{}).(delegatedRouteTarget)
	return target, ok && target.ID != "" && target.Type != ""
}

// admitDelegatedHandler is the explicit delegated branch of every handler
// that serves an admitted operation. For any caller other than an agent
// delegated credential it does nothing and returns true. For a delegated
// caller it returns true only when routeGuard admitted this request for
// exactly op, on a route whose target is resource; otherwise it answers
// 403 credential_not_admitted and returns false.
//
// Every call names its operation as a constant, and
// TestAgentDelegationHandlerBranches_MatchCatalog requires the set of
// operations named in calls to equal the set of catalog operations that
// admit the delegated pair: no handler admits the delegated kind without a
// catalog entry, and no catalog entry lacks a handler branch.
func admitDelegatedHandler(w http.ResponseWriter, r *http.Request, op authzop.OperationID, resource Resource) bool {
	ctx := r.Context()
	if !isDelegatedAgentIdentity(GetIdentityFromContext(ctx)) {
		return true
	}
	entry, admitted := delegatedAdmissionFromContext(ctx)
	target, hasTarget := delegatedTargetFromContext(ctx)
	if !admitted || entry.Operation != op || !hasTarget || target.ProjectID != "" ||
		target.Type != resource.Type || target.ID == "" || target.ID != resource.ID {
		writeDelegatedNotAdmitted(w)
		return false
	}
	return true
}

// admitDelegatedListHandler is admitDelegatedHandler for a list handler,
// whose target is a collection rather than a resource. For a delegated
// caller it returns true only when routeGuard admitted this request for
// exactly op, on a list route (an entry with an item filter) whose target
// is collection; otherwise it answers 403 credential_not_admitted. The
// handler-branch drift test counts its calls with admitDelegatedHandler's.
func admitDelegatedListHandler(w http.ResponseWriter, r *http.Request, op authzop.OperationID, collection delegatedRouteTarget) bool {
	ctx := r.Context()
	if !isDelegatedAgentIdentity(GetIdentityFromContext(ctx)) {
		return true
	}
	entry, admitted := delegatedAdmissionFromContext(ctx)
	target, hasTarget := delegatedTargetFromContext(ctx)
	if !admitted || entry.Operation != op || entry.ItemFilter == "" || !hasTarget || target != collection {
		writeDelegatedNotAdmitted(w)
		return false
	}
	return true
}

// admitDelegatedAgentHandler is admitDelegatedHandler for a handler whose
// target is an agent it loaded by the path's agent segment, which on the
// project form may be the agent's slug. For a delegated caller it returns
// true only when routeGuard admitted this request for exactly op and the
// route target names agent (delegatedRouteTarget.namesAgent); otherwise it
// answers 403 credential_not_admitted. The handler-branch drift test counts
// its calls with admitDelegatedHandler's.
func admitDelegatedAgentHandler(w http.ResponseWriter, r *http.Request, op authzop.OperationID, agent *store.Agent) bool {
	ctx := r.Context()
	if !isDelegatedAgentIdentity(GetIdentityFromContext(ctx)) {
		return true
	}
	if !delegatedRequestAdmittedFor(ctx, op, agent) {
		writeDelegatedNotAdmitted(w)
		return false
	}
	return true
}

// delegatedRequestAdmittedFor reports whether routeGuard admitted the
// request in ctx for exactly op, on a route whose target names agent.
func delegatedRequestAdmittedFor(ctx context.Context, op authzop.OperationID, agent *store.Agent) bool {
	entry, admitted := delegatedAdmissionFromContext(ctx)
	target, hasTarget := delegatedTargetFromContext(ctx)
	return admitted && entry.Operation == op && hasTarget && target.namesAgent(agent)
}

func writeDelegatedNotAdmitted(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, agentDelegationCodeCredentialNotAdmitted,
		"this route does not accept a delegated credential", nil)
}
