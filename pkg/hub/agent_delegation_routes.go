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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// agentDelegationAdmission is one entry of the delegated-credential route
// admission table (.design/agent-delegation.md §11.1).
type agentDelegationAdmission struct {
	// Method and RouteID name one exact route resolution: the agent
	// sub-route resolver's RouteID for the request, never a prefix.
	Method  string
	RouteID AgentSubRouteID
	// Operation is the catalog operation the table supplies for the route.
	// It must equal the resolved AgentSubRoute.OperationID; an empty
	// resolved operation never admits.
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
// both directions.
var agentDelegationAdmittedRoutes = []agentDelegationAdmission{
	{Method: http.MethodGet, RouteID: AgentRouteRoot, Operation: opAgentRead},
}

// agentDelegationAdmissionFor returns the admission entry for method and
// routeID.
func agentDelegationAdmissionFor(method string, routeID AgentSubRouteID) (agentDelegationAdmission, bool) {
	for _, entry := range agentDelegationAdmittedRoutes {
		if entry.Method == method && entry.RouteID == routeID {
			return entry, true
		}
	}
	return agentDelegationAdmission{}, false
}

// admitDelegatedRequest is routeGuard's admission check for a request whose
// identity is an agent delegated credential. It resolves the agent
// sub-route (the same single resolution dispatch uses), looks it up in the
// admission table and, on a match, stores the table entry in the request
// context. Any other route, an unresolved route, or a resolution whose
// operation is empty or differs from the table's answers 403
// credential_not_admitted. It reports whether the request may continue.
func (s *Server) admitDelegatedRequest(w http.ResponseWriter, r *http.Request, meta RouteMetadata) (*http.Request, bool) {
	refuse := func() (*http.Request, bool) {
		writeError(w, http.StatusForbidden, agentDelegationCodeCredentialNotAdmitted,
			"this route does not accept a delegated credential", nil)
		return nil, false
	}
	if !agentSubRouteGuardedRoutes[meta.RouteID] || !isAgentSubRoutePath(r.URL.EscapedPath()) {
		return refuse()
	}
	route, ok := ResolveAgentSubRoute(r.Method, r.URL.EscapedPath())
	if !ok || route.OperationID == "" {
		return refuse()
	}
	entry, ok := agentDelegationAdmissionFor(r.Method, route.RouteID)
	if !ok || entry.Operation != route.OperationID {
		return refuse()
	}
	ctx := withAgentSubRoute(r.Context(), route)
	return r.WithContext(contextWithDelegatedAdmission(ctx, entry)), true
}
