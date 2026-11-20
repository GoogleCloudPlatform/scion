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
	"errors"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleAdminEffectivePermissions serves GET/HEAD /api/v1/admin/effective-permissions.
//
// Authorization is enforced by the RouteHubAdmin declarative route guard
// (role_binding.read at hub scope on BearerTarget hub_instance).
func (s *Server) handleAdminEffectivePermissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		MethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}

	if s.authzService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"authorization service unavailable", nil)
		return
	}

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	q := r.URL.Query()
	principalType := q.Get("principalType")
	principalID := q.Get("principalId")
	projectID := q.Get("projectId")

	if principalType == "" || principalID == "" {
		BadRequest(w, "principalType and principalId are required")
		return
	}
	if principalType != store.RoleBindingPrincipalUser {
		BadRequest(w, "unsupported principal type")
		return
	}

	ctx := r.Context()
	if projectID != "" {
		if s.store == nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"store unavailable", nil)
			return
		}
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "Project")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		identity := GetIdentityFromContext(ctx)
		if identity == nil {
			NotFound(w, "Project")
			return
		}
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(identity),
			Credential: credentialContextForIdentity(identity),
			Resource:   projectResource(project),
			Action:     ActionRead,
			Permission: "project.read",
		})
		if !decision.Allowed {
			NotFound(w, "Project")
			return
		}
	}

	set, err := s.authzService.ResolveEffectivePermissions(
		ctx,
		PrincipalContext{Kind: PrincipalKindUser, ID: principalID},
		EffectiveScope{ProjectID: projectID},
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnsupportedPrincipalKind):
			BadRequest(w, "unsupported principal type")
		case errors.Is(err, store.ErrNotFound):
			NotFound(w, "Project")
		default:
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to resolve effective permissions", nil)
		}
		return
	}

	if set.StatusGate != nil && set.StatusGate.Reason == "missing" {
		NotFound(w, "User")
		return
	}

	// Redact boundary identifiers and names until the access_constraint.read
	// reveal check lands in commit 3, ensuring callers with only
	// role_binding.read never see constraint details.
	for i := range set.Removed {
		for j := range set.Removed[i].RemovedBy {
			set.Removed[i].RemovedBy[j].ID = ""
			set.Removed[i].RemovedBy[j].Name = ""
			set.Removed[i].RemovedBy[j].ScopeID = ""
			set.Removed[i].RemovedBy[j].Redacted = true
		}
	}

	writeJSON(w, http.StatusOK, set)
}
