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

// delegatedRouteTarget is the resource a delegated request's route resolves
// to: the agent or project named in the path.
type delegatedRouteTarget struct {
	Type string
	ID   string
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

// resolveDelegatedRoute returns the single resolution of a delegated request
// on the mux route meta: the agent sub-route resolution (the one dispatch
// uses) on the agent sub-route patterns, the exact project resource route
// on the project pattern, and nothing anywhere else.
func resolveDelegatedRoute(meta RouteMetadata, method, escapedPath string) (delegatedRoute, *AgentSubRoute, bool) {
	if agentSubRouteGuardedRoutes[meta.RouteID] && isAgentSubRoutePath(escapedPath) {
		route, ok := ResolveAgentSubRoute(method, escapedPath)
		if !ok {
			return delegatedRoute{}, nil, false
		}
		return delegatedRoute{
			RouteID:     delegatedRouteID(route.RouteID),
			OperationID: route.OperationID,
			Target:      delegatedRouteTarget{Type: "agent", ID: route.AgentID},
		}, &route, true
	}
	if meta.RouteID == "projects.byId" {
		route, ok := resolveProjectResourceRoute(method, escapedPath)
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
		writeError(w, http.StatusForbidden, agentDelegationCodeCredentialNotAdmitted,
			"this route does not accept a delegated credential", nil)
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
	return r.WithContext(ctx), true
}

// delegatedRouteTargetKey holds the route target routeGuard resolved for an
// admitted delegated request. Only routeGuard sets it.
type delegatedRouteTargetKey struct{}

func contextWithDelegatedTarget(ctx context.Context, target delegatedRouteTarget) context.Context {
	return context.WithValue(ctx, delegatedRouteTargetKey{}, target)
}

// delegatedTargetFromContext returns the route target of an admitted
// delegated request. A context with an agent sub-route and no explicit
// target uses the sub-route's agent.
func delegatedTargetFromContext(ctx context.Context) (delegatedRouteTarget, bool) {
	if target, ok := ctx.Value(delegatedRouteTargetKey{}).(delegatedRouteTarget); ok && target.ID != "" {
		return target, true
	}
	if route, ok := agentSubRouteFromContext(ctx); ok && route.AgentID != "" {
		return delegatedRouteTarget{Type: "agent", ID: route.AgentID}, true
	}
	return delegatedRouteTarget{}, false
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
	if !admitted || entry.Operation != op || !hasTarget ||
		target.Type != resource.Type || target.ID == "" || target.ID != resource.ID {
		writeError(w, http.StatusForbidden, agentDelegationCodeCredentialNotAdmitted,
			"this route does not accept a delegated credential", nil)
		return false
	}
	return true
}
