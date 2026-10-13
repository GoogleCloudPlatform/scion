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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// resolveFederatedVisitor is the auth middleware's federated visitor
// lookup (AuthConfig.ResolveFederatedVisitor). After the federation
// authenticator has verified a token, it returns the VisitorIdentity of the
// federated account linked to the token's (issuer, subject) when all of
// these hold:
//   - the federation.project_visitors experiment is enabled now;
//   - the verified identity is a *FederatedUserIdentity;
//   - the pair is linked (store.Store.GetFederatedUserBySubject) to a
//     federated account whose status is active.
//
// In every other case (experiment off, another federated identity type, no
// link, a pending or suspended account) it returns (nil, nil), so the
// request keeps the verified identity exactly as before. A store error other
// than not-found is returned; the middleware answers it with 503.
func (s *Server) resolveFederatedVisitor(ctx context.Context, identity FederatedIdentity) (*VisitorIdentity, error) {
	fu, ok := identity.(*FederatedUserIdentity)
	if !ok || fu == nil || s.store == nil {
		return nil, nil
	}
	if !s.experimentEnabled(experiments.FederationProjectVisitors) {
		return nil, nil
	}
	u, err := s.store.GetFederatedUserBySubject(ctx, fu.IssuerURL(), fu.Subject())
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !u.IsFederated() || u.Status != store.UserStatusActive {
		return nil, nil
	}
	email := fu.Email()
	if u.FederatedEmail != nil {
		email = *u.FederatedEmail
	}
	return NewVisitorIdentity(u.ID, fu.IssuerURL(), fu.Subject(), email, fu.DisplayName()), nil
}

// visitorRouteRefusalMessage is the one response message for every route a
// visitor is refused, so the response does not distinguish routes.
const visitorRouteRefusalMessage = "You don't have access to this resource"

// refuseVisitorRequest is routeGuard's visitor check. Until per-route visitor
// admission exists, it refuses every request whose identity is a
// *VisitorIdentity with the same 403, whatever the route. It writes the
// refusal and returns true when it refused.
func refuseVisitorRequest(w http.ResponseWriter, r *http.Request) bool {
	if !isVisitorIdentity(GetIdentityFromContext(r.Context())) {
		return false
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, visitorRouteRefusalMessage, nil)
	return true
}
