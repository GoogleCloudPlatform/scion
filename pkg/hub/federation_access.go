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
	"fmt"
	"net/http"
)

// scopeChecker is implemented by identity types that support scope-based access control.
type scopeChecker interface {
	HasScope(scope AgentTokenScope) bool
}

// RequireFederationAccess returns a middleware that requires the caller to be
// any identity with scope-based access (local agent, federated agent,
// federated service account, or federated user) and to have the specified scope.
func RequireFederationAccess(scope AgentTokenScope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity := GetIdentityFromContext(r.Context())
			if identity == nil {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"authentication required", nil)
				return
			}

			sc, ok := identity.(scopeChecker)
			if !ok {
				// Identity type does not support scope checking (e.g. UserIdentity from IAP).
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					fmt.Sprintf("identity type %q does not support scope-based access", identity.Type()), nil)
				return
			}

			if !sc.HasScope(scope) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					fmt.Sprintf("missing required scope: %s", scope), nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
