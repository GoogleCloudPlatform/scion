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
	"encoding/json"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SetTemplateRequest is the request body for POST /api/v1/projects/{id}/set-template.
type SetTemplateRequest struct {
	IsTemplate bool `json:"isTemplate"`
}

// handleSetTemplate marks or unmarks a project as a template.
// POST /api/v1/projects/{id}/set-template
// Requires admin role + ActionUpdate on the project.
func (s *Server) handleSetTemplate(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Require project.clone permission for template management (user identity only).
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}
	if !s.authzService.Decide(r.Context(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "project", ID: "hub"},
		Action:     Action("clone"),
		Permission: "project.clone",
	}).Allowed {
		Forbidden(w)
		return
	}

	var req SetTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	// Load the project
	project, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Check ActionUpdate permission on the project
	decision := s.authzService.CheckAccess(r.Context(), user, projectResource(project), ActionUpdate)
	if !decision.Allowed {
		Forbidden(w)
		return
	}

	if req.IsTemplate {
		// Check for existing agents before marking as template
		agentResult, err := s.store.ListAgents(r.Context(), store.AgentFilter{ProjectID: project.ID}, store.ListOptions{Limit: 1})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if len(agentResult.Items) > 0 {
			writeError(w, http.StatusConflict, ErrCodeConflict, "cannot mark project as template while agents exist", nil)
			return
		}

		// Set template label
		if project.Labels == nil {
			project.Labels = make(map[string]string)
		}
		project.Labels[store.LabelTemplate] = "true"
	} else {
		// Remove template label
		delete(project.Labels, store.LabelTemplate)
	}

	if err := s.store.UpdateProject(r.Context(), project); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, project)
}
