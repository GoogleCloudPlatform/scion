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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// User-scoped templates (/api/v1/users/me/templates)
// =============================================================================

// isUserTemplateOwner returns true if the template is user-scoped and owned by
// the given user ID (matching either OwnerID or ScopeID).
func isUserTemplateOwner(t *store.Template, uid string) bool {
	return t.Scope == store.TemplateScopeUser &&
		(t.OwnerID == uid || t.ScopeID == uid)
}

// handleUserMeTemplates routes GET/POST on /api/v1/users/me/templates.
func (s *Server) handleUserMeTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listUserTemplates(w, r)
	case http.MethodPost:
		s.createUserTemplate(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleUserMeTemplateByID routes GET/PUT/DELETE on
// /api/v1/users/me/templates/{id} and sub-actions.
func (s *Server) handleUserMeTemplateByID(w http.ResponseWriter, r *http.Request, templateID string) {
	// Check for sub-actions (upload, finalize, download, clone)
	parts := strings.SplitN(templateID, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		s.handleUserTemplateCRUD(w, r, id)
	case "upload":
		s.handleUserTemplateUpload(w, r, id)
	case "finalize":
		s.handleUserTemplateFinalize(w, r, id)
	case "download":
		s.handleUserTemplateDownload(w, r, id)
	default:
		NotFound(w, "Template action")
	}
}

// handleUserTemplateCRUD routes CRUD for a single user template.
func (s *Server) handleUserTemplateCRUD(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		s.getUserTemplate(w, r, id)
	case http.MethodPut:
		s.updateUserTemplate(w, r, id)
	case http.MethodDelete:
		s.deleteUserTemplate(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

// listUserTemplates handles GET /api/v1/users/me/templates.
func (s *Server) listUserTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	filter := store.TemplateFilter{
		Scope:   store.TemplateScopeUser,
		ScopeID: userIdent.ID(),
		Status:  store.TemplateStatusActive,
	}

	// Allow filtering by query params
	query := r.URL.Query()
	if name := query.Get("name"); name != "" {
		filter.Name = name
	}
	if harness := query.Get("harness"); harness != "" {
		filter.Harness = harness
	}
	if status := query.Get("status"); status != "" {
		filter.Status = status
	}

	result, err := s.store.ListTemplates(ctx, filter, store.ListOptions{
		Limit: 100,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	templates := make([]TemplateWithCapabilities, len(result.Items))
	for i := range result.Items {
		templates[i] = TemplateWithCapabilities{Template: result.Items[i]}
	}

	writeJSON(w, http.StatusOK, ListTemplatesResponse{
		Templates:  templates,
		TotalCount: result.TotalCount,
	})
}

// createUserTemplate handles POST /api/v1/users/me/templates.
func (s *Server) createUserTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	var req CreateTemplateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	slug := req.Slug
	if slug != "" {
		slug = api.Slugify(slug)
	}
	if slug == "" {
		slug = api.Slugify(req.Name)
	}
	if slug == "" {
		BadRequest(w, "invalid slug: name cannot be slugified")
		return
	}

	template := &store.Template{
		ID:           api.NewUUID(),
		Name:         req.Name,
		Slug:         slug,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		Harness:      req.Harness,
		Config:       req.Config,
		Scope:        store.TemplateScopeUser,
		ScopeID:      userIdent.ID(),
		OwnerID:      userIdent.ID(),
		CreatedBy:    userIdent.ID(),
		BaseTemplate: req.BaseTemplate,
		Visibility:   req.Visibility,
		Status:       store.TemplateStatusPending,
	}

	if template.Visibility == "" {
		template.Visibility = store.VisibilityPrivate
	}

	// Generate storage path and URI
	storagePath := storage.TemplateStoragePath(s.HubID(), template.Scope, template.ScopeID, template.Slug)
	template.StoragePath = storagePath

	stor := s.GetStorage()
	if stor != nil {
		template.StorageBucket = stor.Bucket()
		template.StorageURI = storage.TemplateStorageURI(s.HubID(), stor.Bucket(), template.Scope, template.ScopeID, template.Slug)
	}

	if err := s.store.CreateTemplate(ctx, template); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusConflict, "conflict", "A template with this name already exists in your user scope. Choose a different name.", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	response := CreateTemplateResponse{
		Template: template,
	}

	// Generate upload URLs if files were specified
	if len(req.Files) > 0 && stor != nil {
		uploadURLs, manifestURL, err := generateUploadURLs(ctx, stor, storagePath, req.Files)
		if err == nil || len(uploadURLs) > 0 {
			if stor.Provider() == storage.ProviderLocal {
				hubURL := requestBaseURL(r)
				uploadURLs = rewriteLocalUploadURLs(uploadURLs, hubURL, "templates", template.ID)
			}
			response.UploadURLs = uploadURLs
			response.ManifestURL = manifestURL
		}
	}

	writeJSON(w, http.StatusCreated, response)
}

// getUserTemplate handles GET /api/v1/users/me/templates/{id}.
func (s *Server) getUserTemplate(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify ownership
	if !isUserTemplateOwner(template, userIdent.ID()) {
		NotFound(w, "Template")
		return
	}

	writeJSON(w, http.StatusOK, TemplateWithCapabilities{Template: *template})
}

// updateUserTemplate handles PUT /api/v1/users/me/templates/{id}.
func (s *Server) updateUserTemplate(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify ownership
	if !isUserTemplateOwner(existing, userIdent.ID()) {
		NotFound(w, "Template")
		return
	}

	var template store.Template
	if err := readJSON(r, &template); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Preserve immutable fields
	template.ID = existing.ID
	template.Scope = store.TemplateScopeUser
	template.ScopeID = userIdent.ID()
	template.OwnerID = userIdent.ID()
	template.Created = existing.Created
	template.CreatedBy = existing.CreatedBy
	template.UpdatedBy = userIdent.ID()
	// Preserve system-managed fields that must not be zeroed by client input
	template.StoragePath = existing.StoragePath
	template.StorageBucket = existing.StorageBucket
	template.StorageURI = existing.StorageURI
	template.Files = existing.Files
	template.ContentHash = existing.ContentHash
	template.Status = existing.Status
	if template.Slug != "" {
		template.Slug = api.Slugify(template.Slug)
	}
	if template.Slug == "" {
		template.Slug = api.Slugify(template.Name)
	}

	if err := s.store.UpdateTemplate(ctx, &template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, template)
}

// deleteUserTemplate handles DELETE /api/v1/users/me/templates/{id}.
func (s *Server) deleteUserTemplate(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify ownership
	if !isUserTemplateOwner(existing, userIdent.ID()) {
		NotFound(w, "Template")
		return
	}

	// Delete files from storage if requested
	deleteFiles := r.URL.Query().Get("deleteFiles") == "true"
	if deleteFiles && existing.StoragePath != "" {
		if stor := s.GetStorage(); stor != nil {
			_ = stor.DeletePrefix(ctx, existing.StoragePath)
		}
	}

	if err := s.store.DeleteTemplate(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleUserTemplateUpload handles POST /api/v1/users/me/templates/{id}/upload.
func (s *Server) handleUserTemplateUpload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify ownership
	if !isUserTemplateOwner(template, userIdent.ID()) {
		NotFound(w, "Template")
		return
	}

	// Delegate to the shared upload handler
	s.handleTemplateUpload(w, r, id)
}

// handleUserTemplateFinalize handles POST /api/v1/users/me/templates/{id}/finalize.
func (s *Server) handleUserTemplateFinalize(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify ownership
	if !isUserTemplateOwner(template, userIdent.ID()) {
		NotFound(w, "Template")
		return
	}

	// Delegate to the shared finalize handler
	s.handleTemplateFinalize(w, r, id)
}

// handleUserTemplateDownload handles GET /api/v1/users/me/templates/{id}/download.
func (s *Server) handleUserTemplateDownload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify ownership
	if !isUserTemplateOwner(template, userIdent.ID()) {
		NotFound(w, "Template")
		return
	}

	// Delegate to the shared download handler
	s.handleTemplateDownload(w, r, id)
}
