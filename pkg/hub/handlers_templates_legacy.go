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
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type ListTemplatesResponse struct {
	Templates    []TemplateWithCapabilities `json:"templates"`
	NextCursor   string                     `json:"nextCursor,omitempty"`
	TotalCount   int                        `json:"totalCount"`
	Capabilities *Capabilities              `json:"_capabilities,omitempty"`
}

// ListHarnessConfigsResponse is the response for listing harness configs.
type ListHarnessConfigsResponse struct {
	HarnessConfigs []HarnessConfigWithCapabilities `json:"harnessConfigs"`
	NextCursor     string                          `json:"nextCursor,omitempty"`
	TotalCount     int                             `json:"totalCount"`
	Capabilities   *Capabilities                   `json:"_capabilities,omitempty"`
}

//nolint:unused // Legacy template handlers are retained for compatibility.
func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTemplates(w, r)
	case http.MethodPost:
		s.createTemplate(w, r)
	default:
		MethodNotAllowed(w)
	}
}

//nolint:unused // Legacy template handlers are retained for compatibility.
func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.TemplateFilter{
		Scope:     query.Get("scope"),
		ProjectID: query.Get("projectId"),
		Harness:   query.Get("harness"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListTemplates(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Compute per-item and scope capabilities
	identity := GetIdentityFromContext(ctx)
	templates := make([]TemplateWithCapabilities, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = templateResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "template")
		for i := range result.Items {
			templates[i] = TemplateWithCapabilities{Template: result.Items[i], Cap: caps[i]}
		}
	} else {
		for i := range result.Items {
			templates[i] = TemplateWithCapabilities{Template: result.Items[i]}
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "template")
	}

	writeJSON(w, http.StatusOK, ListTemplatesResponse{
		Templates:    templates,
		NextCursor:   result.NextCursor,
		TotalCount:   result.TotalCount,
		Capabilities: scopeCap,
	})
}

//nolint:unused // Legacy template handlers are retained for compatibility.
func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var template store.Template
	if err := readJSON(r, &template); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if template.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	template.ID = api.NewUUID()
	template.Slug = api.Slugify(template.Name)

	if template.Scope == "" {
		template.Scope = "global"
	}
	if template.Visibility == "" {
		template.Visibility = store.VisibilityPrivate
	}

	if err := s.store.CreateTemplate(ctx, &template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, template)
}

//nolint:unused // Legacy template handlers are retained for compatibility.
func (s *Server) handleTemplateByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/templates")

	if id == "" {
		NotFound(w, "Template")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getTemplate(w, r, id)
	case http.MethodPut:
		s.updateTemplate(w, r, id)
	case http.MethodDelete:
		s.deleteTemplate(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

//nolint:unused // Legacy template handlers are retained for compatibility.
func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resp := TemplateWithCapabilities{Template: *template}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, templateResource(template))
	}

	writeJSON(w, http.StatusOK, resp)
}

//nolint:unused // Legacy template handlers are retained for compatibility.
func (s *Server) updateTemplate(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var template store.Template
	if err := readJSON(r, &template); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Preserve ID and timestamps
	template.ID = existing.ID
	template.Created = existing.Created

	if template.Slug == "" {
		template.Slug = api.Slugify(template.Name)
	}

	if err := s.store.UpdateTemplate(ctx, &template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, template)
}

//nolint:unused // Legacy template handlers are retained for compatibility.
func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteTemplate(r.Context(), id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
