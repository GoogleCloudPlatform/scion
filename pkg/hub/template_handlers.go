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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// SignedURLExpiry is the duration signed URLs are valid for.
const SignedURLExpiry = 15 * time.Minute

// isValidTemplateScope reports whether scope is an accepted template scope.
// An empty scope is valid: callers default it (to "global" for create, to
// "project" for clone). Any other value — including removed legacy scope
// names — is rejected outright rather than stored as-is.
func isValidTemplateScope(scope string) bool {
	switch scope {
	case "", store.TemplateScopeGlobal, store.TemplateScopeProject, store.TemplateScopeUser:
		return true
	default:
		return false
	}
}

// CreateTemplateRequest is the request body for creating a template.
type CreateTemplateRequest struct {
	Name         string                `json:"name"`
	Slug         string                `json:"slug,omitempty"`
	DisplayName  string                `json:"displayName,omitempty"`
	Description  string                `json:"description,omitempty"`
	Harness      string                `json:"harness,omitempty"`
	Scope        string                `json:"scope"`
	ScopeID      string                `json:"scopeId,omitempty"`
	ProjectID    string                `json:"projectId,omitempty"` // Deprecated: use ScopeID
	Config       *store.TemplateConfig `json:"config,omitempty"`
	BaseTemplate string                `json:"baseTemplate,omitempty"`
	Files        []FileUploadRequest   `json:"files,omitempty"`
}

// FileUploadRequest describes a file to upload.
type FileUploadRequest struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// CreateTemplateResponse is the response for template creation.
type CreateTemplateResponse struct {
	Template    *store.Template `json:"template"`
	UploadURLs  []UploadURLInfo `json:"uploadUrls,omitempty"`
	ManifestURL string          `json:"manifestUrl,omitempty"`
}

type ListTemplatesResponse struct {
	Templates  []TemplateWithCapabilities `json:"templates"`
	NextCursor string                     `json:"nextCursor,omitempty"`
	TotalCount int                        `json:"totalCount"`
	// TotalCountApproximate marks TotalCount as a lower bound rather than an
	// exact count (ptone/scion#1916 follow-up, C3): set when the hub-wide
	// candidate pool crossed authorizedListMaxCandidates before the count
	// scan finished. Never affects Templates or NextCursor, which are always
	// exactly the caller's authorized page regardless of this flag.
	TotalCountApproximate bool          `json:"totalCountApproximate,omitempty"`
	Capabilities          *Capabilities `json:"_capabilities,omitempty"`
}

// UploadURLInfo contains a signed URL for uploading a file.
type UploadURLInfo struct {
	Path    string            `json:"path"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Expires time.Time         `json:"expires"`
}

// UploadRequest is the request body for requesting upload URLs.
type UploadRequest struct {
	Files []FileUploadRequest `json:"files"`
}

// UploadResponse is the response containing signed upload URLs.
type UploadResponse struct {
	UploadURLs  []UploadURLInfo `json:"uploadUrls"`
	ManifestURL string          `json:"manifestUrl,omitempty"`
}

// FinalizeRequest is the request body for finalizing a template upload.
type FinalizeRequest struct {
	Manifest *TemplateManifest `json:"manifest"`
}

// TemplateManifest is the manifest of uploaded template files.
type TemplateManifest struct {
	Version string               `json:"version"`
	Harness string               `json:"harness,omitempty"`
	Files   []store.TemplateFile `json:"files"`
}

// DownloadResponse contains signed URLs for downloading template files.
type DownloadResponse struct {
	ManifestURL string            `json:"manifestUrl,omitempty"`
	Files       []DownloadURLInfo `json:"files"`
	Expires     time.Time         `json:"expires"`
}

// DownloadURLInfo contains info for downloading a file.
type DownloadURLInfo struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
	Hash string `json:"hash,omitempty"`
}

// CloneTemplateRequest is the request for cloning a template.
type CloneTemplateRequest struct {
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	ScopeID   string `json:"scopeId,omitempty"`
	ProjectID string `json:"projectId,omitempty"` // Deprecated
}

// handleTemplatesV2 handles the /api/v1/templates endpoint with storage support.
func (s *Server) handleTemplatesV2(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTemplatesV2(w, r)
	case http.MethodPost:
		s.createTemplateV2(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// listTemplatesV2 lists templates with extended filtering.
func (s *Server) listTemplatesV2(w http.ResponseWriter, r *http.Request) {
	if !checkAgentReadScope(w, r) {
		return
	}
	ctx, query := r.Context(), r.URL.Query()
	filter := store.TemplateFilter{Name: query.Get("name"), Scope: query.Get("scope"), ScopeID: query.Get("scopeId"), ProjectID: query.Get("projectId"), Harness: query.Get("harness"), Status: query.Get("status"), Search: query.Get("search")}
	if filter.Status == "" {
		filter.Status = store.TemplateStatusActive
	}

	// When listing without explicit scope, include user-scoped templates
	// for the authenticated user in the resolution results.
	switch filter.Scope {
	case "":
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			filter.UserID = userIdent.ID()
		}
	case store.TemplateScopeUser:
		// When listing user-scoped templates, always force ScopeID to the
		// authenticated user's ID to prevent cross-user template enumeration.
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			Unauthorized(w)
			return
		}
		filter.ScopeID = userIdent.ID()
	}

	limit, err := parseAuthorizedListLimit(query.Get("limit"))
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	identity, cursor := GetIdentityFromContext(ctx), query.Get("cursor")
	cursorBinding := authorizedListCursorBinding("templates", filter)
	if cursor != "" {
		if err := validateAuthorizedListCursor(cursor, cursorBinding); err != nil {
			BadRequest(w, err.Error())
			return
		}
	}
	wideAccess := s.hasCatalogWideListAccess(ctx, identity, "template", "template.list")
	authorizeEach := identity != nil && !wideAccess
	result, err := listAuthorizedOrAll(
		ctx, identity, cursor, limit, cursorBinding, authorizeEach,
		func(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.Template], error) {
			return s.store.ListTemplates(ctx, filter, opts)
		},
		templateResource,
		func(t *store.Template) string { return authorizedListCursor(t.Created, t.ID, cursorBinding) },
		s.catalogListReadBatch(identity),
	)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	items, nextCursor, totalCount, totalApprox := result.Items, result.NextCursor, result.TotalCount, result.TotalCountApproximate
	templates := make([]TemplateWithCapabilities, 0, len(items))
	if identity == nil {
		for i := range items {
			templates = append(templates, TemplateWithCapabilities{Template: items[i]})
		}
	} else {
		resources := make([]Resource, len(items))
		for i := range items {
			resources[i] = templateResource(&items[i])
		}
		for i, cap := range s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "template") {
			templates = append(templates, TemplateWithCapabilities{Template: items[i], Cap: cap})
		}
	}
	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "template")
	}
	writeJSON(w, http.StatusOK, ListTemplatesResponse{Templates: templates, NextCursor: nextCursor, TotalCount: totalCount, TotalCountApproximate: totalApprox, Capabilities: scopeCap})
}

// createTemplateV2 creates a template with optional file upload URLs.
func (s *Server) createTemplateV2(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateTemplateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if !isValidTemplateScope(req.Scope) {
		ValidationError(w, fmt.Sprintf("invalid scope %q: must be \"global\", \"project\" or \"user\"", req.Scope), nil)
		return
	}
	// Resolve scope ID
	scopeID := req.ScopeID
	if scopeID == "" && req.ProjectID != "" {
		scopeID = req.ProjectID
	}

	// SECURITY-GATE: require template.create permission before any mutation.
	// Scope-aware: project-scoped requests authorize against the project parent
	// so that project-level role bindings (owner/admin/member) grant access.
	//
	// User-scoped requests never authorize against a caller-supplied scopeID
	// at all: templateUserScopeResource takes the authenticated UserIdentity
	// directly rather than a bare string, so there is no request field this
	// gate could be tricked into authorizing against (e.g. another user's
	// ID) — an attacker-supplied scopeID is simply never consulted. scopeID
	// is still forced to the caller's own ID here, ahead of the redundant
	// forcing on the template record below, so the persisted record agrees
	// with what was authorized.
	createScope := req.Scope
	if createScope == "" {
		createScope = store.TemplateScopeGlobal
	}
	if createScope == store.TemplateScopeUser {
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			Unauthorized(w)
			return
		}
		scopeID = userIdent.ID()
		if !s.authorize(w, r, templateUserScopeResource(userIdent), ActionCreate) {
			return
		}
	} else if !s.authorize(w, r, templateScopeResource(createScope, scopeID), ActionCreate) {
		return
	}

	// Generate slug from request or name — always sanitize caller-supplied
	// slugs through Slugify to prevent path-traversal via crafted values.
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

	// Validate MessageMode in template config if specified.
	if req.Config != nil && req.Config.MessageMode != "" {
		if !store.IsValidMessageMode(req.Config.MessageMode) {
			ValidationError(w, "invalid template message mode: "+req.Config.MessageMode, nil)
			return
		}
	}

	// Create template record
	template := &store.Template{
		ID:           api.NewUUID(),
		Name:         req.Name,
		Slug:         slug,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		Harness:      req.Harness,
		Config:       req.Config,
		Scope:        req.Scope,
		ScopeID:      scopeID,
		ProjectID:    scopeID, // Keep for backwards compat
		BaseTemplate: req.BaseTemplate,
		Status:       store.TemplateStatusPending, // Start as pending until files uploaded
	}

	if template.Scope == "" {
		template.Scope = store.TemplateScopeGlobal
	}

	// For user-scoped templates, always set the owner and scope ID from the
	// authenticated user. ScopeID is forced unconditionally to prevent callers
	// from injecting another user's ID.
	if template.Scope == store.TemplateScopeUser {
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			Unauthorized(w)
			return
		}
		template.OwnerID = userIdent.ID()
		template.CreatedBy = userIdent.ID()
		template.ScopeID = userIdent.ID()
	}

	// If no files provided, keep the template in 'pending' status so it
	// cannot be used for agent dispatch until files are uploaded via
	// "scion template sync". This prevents template_error failures when
	// agents are dispatched to file-less templates.
	// Templates with files are also created as 'pending' and promoted to
	// 'active' during finalize (handleTemplateFinalize).

	// Generate storage path and URI
	storagePath := storage.TemplateStoragePath(s.HubID(), template.Scope, template.ScopeID, template.Slug)
	template.StoragePath = storagePath

	// Get storage client if available
	stor := s.GetStorage()
	if stor != nil {
		template.StorageBucket = stor.Bucket()
		template.StorageURI = storage.TemplateStorageURI(s.HubID(), stor.Bucket(), template.Scope, template.ScopeID, template.Slug)
	}

	// Create the template record
	if err := s.store.CreateTemplate(ctx, template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	response := CreateTemplateResponse{
		Template: template,
	}

	// Generate upload URLs if files were specified and storage is available
	if len(req.Files) > 0 && stor != nil {
		uploadURLs, manifestURL, err := generateUploadURLs(ctx, stor, storagePath, req.Files)
		if err == nil || len(uploadURLs) > 0 {
			// For local storage, rewrite file:// URLs to HTTP proxy URLs
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

// handleTemplateByIDV2 handles individual template operations with storage support.
func (s *Server) handleTemplateByIDV2(w http.ResponseWriter, r *http.Request) {
	// Extract template ID and action
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/templates/")
	if path == "" {
		NotFound(w, "Template")
		return
	}

	parts := strings.SplitN(path, "/", 2)
	templateID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	// Handle actions
	switch action {
	case "":
		s.handleTemplateCRUD(w, r, templateID)
	case "upload":
		s.handleTemplateUpload(w, r, templateID)
	case "finalize":
		s.handleTemplateFinalize(w, r, templateID)
	case "download":
		s.handleTemplateDownload(w, r, templateID)
	case "clone":
		s.handleTemplateClone(w, r, templateID)
	case "validate":
		s.handleTemplateValidate(w, r, templateID)
	case "files":
		s.handleTemplateFiles(w, r, templateID, "")
	default:
		if strings.HasPrefix(action, "files/") {
			filePath := strings.TrimPrefix(action, "files/")
			s.handleTemplateFiles(w, r, templateID, filePath)
			return
		}
		NotFound(w, "Template action")
	}
}

// handleTemplateCRUD handles basic template CRUD operations.
func (s *Server) handleTemplateCRUD(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		s.getTemplateV2(w, r, id)
	case http.MethodPut:
		s.updateTemplateV2(w, r, id)
	case http.MethodPatch:
		s.patchTemplateV2(w, r, id)
	case http.MethodDelete:
		s.deleteTemplateV2(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

// getTemplateV2 retrieves a template with full metadata.
func (s *Server) getTemplateV2(w http.ResponseWriter, r *http.Request, id string) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: authorize read access to this specific template.
	// The list endpoint filters via AuthorizeReadBatch; without this check
	// a caller could bypass list filtering by addressing the template by ID.
	// A denial reads as 404 (ptone/scion#1916), matching the skill fix: a
	// template a caller may not read must not be distinguishable from one
	// that does not exist.
	if !s.authorizeTemplateReadRoute(w, r, template) {
		return
	}

	resp := TemplateWithCapabilities{Template: *template}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, templateResource(template))
	}

	writeJSON(w, http.StatusOK, resp)
}

// updateTemplateV2 replaces a template (upsert style).
func (s *Server) updateTemplateV2(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: authorize update access to this specific template.
	if !s.authorize(w, r, templateResource(existing), ActionUpdate) {
		return
	}

	var template store.Template
	if err := readJSON(r, &template); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate MessageMode in template config if specified.
	if template.Config != nil && template.Config.MessageMode != "" {
		if !store.IsValidMessageMode(template.Config.MessageMode) {
			ValidationError(w, "invalid template message mode: "+template.Config.MessageMode, nil)
			return
		}
	}

	// Preserve immutable fields from the existing record. The store's
	// UpdateTemplate unconditionally Set()s every column, so any field not
	// pinned here is overwritten with the request body's value — or its
	// zero value if the body omits it.
	//
	// Group 1 — identity and authz state: without these a caller could
	// reparent a template to a different scope or claim ownership.
	// Scope reparenting requires its own endpoint with dual-scope
	// authorization; it is not supported through the update body.
	template.ID = existing.ID
	template.Created = existing.Created
	template.CreatedBy = existing.CreatedBy
	template.Scope = existing.Scope
	template.ScopeID = existing.ScopeID
	template.OwnerID = existing.OwnerID
	// Group 2 — content and storage state: these are managed by the
	// upload/finalize workflow and must not be writable through the
	// update body.
	template.ProjectID = existing.ProjectID
	template.StoragePath = existing.StoragePath
	template.StorageBucket = existing.StorageBucket
	template.StorageURI = existing.StorageURI
	template.Files = existing.Files
	template.ContentHash = existing.ContentHash
	template.Status = existing.Status
	template.BaseTemplate = existing.BaseTemplate
	template.SourceURL = existing.SourceURL
	// Group 3 — audit trail: derived from the authenticated caller,
	// not trusted from the request body. The deref is safe: authorize
	// (line 497) returns false on nil identity, so reaching here
	// guarantees GetIdentityFromContext(ctx) != nil.
	template.UpdatedBy = GetIdentityFromContext(ctx).ID()
	if template.Slug == "" {
		template.Slug = api.Slugify(template.Name)
	}

	if err := s.store.UpdateTemplate(ctx, &template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, template)
}

// patchTemplateV2 updates specific template fields.
func (s *Server) patchTemplateV2(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: authorize update access to this specific template.
	if !s.authorize(w, r, templateResource(existing), ActionUpdate) {
		return
	}

	if !applyResourceMetadataPatch(w, r, resourceMetadataFields{
		Name:        &existing.Name,
		Slug:        &existing.Slug,
		DisplayName: &existing.DisplayName,
		Description: &existing.Description,
	}) {
		return
	}

	if err := s.store.UpdateTemplate(ctx, existing); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

// deleteTemplateV2 deletes a template.
func (s *Server) deleteTemplateV2(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize: check source scope for ActionDelete
	switch existing.Scope {
	case store.TemplateScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, templateScopeResource(store.TemplateScopeGlobal, ""), ActionDelete)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to delete global resources", nil)
			return
		}
	case store.TemplateScopeProject:
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			if !agentIdent.HasScope(ScopeAgentCreate) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope", nil)
				return
			}
			if existing.ScopeID != agentIdent.ProjectID() {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only manage resources within their own project", nil)
				return
			}
		} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			decision := s.authzService.CheckAccess(ctx, userIdent,
				templateScopeResource(store.TemplateScopeProject, existing.ScopeID), ActionDelete)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to delete resources in this project", nil)
				return
			}
		} else {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
	case store.TemplateScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		if existing.OwnerID != userIdent.ID() && existing.ScopeID != userIdent.ID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You can only delete your own user-scoped templates", nil)
			return
		}
	}

	// If deleteFiles is true and we have storage, delete the files
	if deleteFiles && existing.StoragePath != "" {
		if stor := s.GetStorage(); stor != nil {
			if err := stor.DeletePrefix(ctx, existing.StoragePath); err != nil {
				slog.Warn("failed to delete template files", "template_id", id, "storage_path", existing.StoragePath, "error", err)
			}
		}
	}

	// Delete from database
	if err := s.store.DeleteTemplate(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleTemplateUpload handles requests for upload URLs.
func (s *Server) handleTemplateUpload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: authorize update access to this template (upload mutates content).
	if !s.authorize(w, r, templateResource(template), ActionUpdate) {
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	var req UploadRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Files) == 0 {
		ValidationError(w, "at least one file is required", nil)
		return
	}

	// Check that template has a valid storage path
	if template.StoragePath == "" {
		RuntimeError(w, "Template storage path not configured (template ID: "+id+", scope: "+template.Scope+", scopeID: "+template.ScopeID+")")
		return
	}

	// Generate upload URLs using shared helper
	uploadURLs, manifestURL, err := generateUploadURLs(ctx, stor, template.StoragePath, req.Files)
	if err != nil {
		RuntimeError(w, "Failed to generate upload URLs: "+err.Error())
		return
	}
	if len(uploadURLs) == 0 && len(req.Files) > 0 {
		RuntimeError(w, "Failed to generate upload URLs")
		return
	}

	// For local storage, rewrite file:// URLs to HTTP proxy URLs
	if stor.Provider() == storage.ProviderLocal {
		hubURL := requestBaseURL(r)
		uploadURLs = rewriteLocalUploadURLs(uploadURLs, hubURL, "templates", id)
	}

	response := UploadResponse{
		UploadURLs:  uploadURLs,
		ManifestURL: manifestURL,
	}

	writeJSON(w, http.StatusOK, response)
}

// handleTemplateFinalize finalizes a template after file upload.
func (s *Server) handleTemplateFinalize(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: authorize update access to this template (finalize mutates state).
	if !s.authorize(w, r, templateResource(template), ActionUpdate) {
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	var req FinalizeRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Manifest == nil || len(req.Manifest.Files) == 0 {
		ValidationError(w, "manifest with files is required", nil)
		return
	}

	// Verify files exist in storage and compute content hash using shared helper
	contentHash, err := verifyAndFinalizeFiles(ctx, stor, template.StoragePath, req.Manifest.Files)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	// Update template with manifest and mark as active
	template.Files = req.Manifest.Files
	template.ContentHash = contentHash
	template.Status = store.TemplateStatusActive

	if err := s.store.UpdateTemplate(ctx, template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, template)
}

// handleTemplateDownload returns signed URLs for downloading template files.
func (s *Server) handleTemplateDownload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: authorize read access to this template's files. A
	// denial reads as 404 (ptone/scion#1916), matching getTemplateV2.
	if !s.authorizeTemplateReadRoute(w, r, template) {
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	if len(template.Files) == 0 {
		name := template.Slug
		if name == "" {
			name = template.Name
		}
		ValidationError(w, "template "+name+" ("+template.ID+") has no files — sync template files first with: scion template sync "+name, nil)
		return
	}

	// Generate download URLs using shared helper
	downloadURLs, manifestURL, expires, err := generateDownloadURLs(ctx, stor, template.StoragePath, s.legacyFallbackPath(template.StoragePath), template.Files)
	if err != nil {
		RuntimeError(w, fmt.Sprintf("template %q: %s — run 'scion template validate %s' to diagnose", template.Name, err, template.Name))
		return
	}

	// For local storage, rewrite file:// URLs to HTTP proxy URLs
	if stor.Provider() == storage.ProviderLocal {
		hubURL := requestBaseURL(r)
		downloadURLs = rewriteLocalDownloadURLs(downloadURLs, hubURL, "templates", id)
	}

	response := DownloadResponse{
		Files:       downloadURLs,
		ManifestURL: manifestURL,
		Expires:     expires,
	}

	writeJSON(w, http.StatusOK, response)
}

// handleTemplateValidate validates a template's storage consistency.
func (s *Server) handleTemplateValidate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()
	template, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// SECURITY-GATE: authorize read access to this template's validation
	// report. A denial reads as 404 (ptone/scion#1916), matching getTemplateV2.
	if !s.authorizeRead(w, r, templateResource(template), "Template") {
		return
	}

	rec := templateToRecord(template)
	rs := s.templateStore()
	report, err := rs.ValidateStorage(ctx, rec)
	if err != nil {
		RuntimeError(w, fmt.Sprintf("validation failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, report)
}

// handleTemplateClone creates a copy of a template.
func (s *Server) handleTemplateClone(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	source, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		// A genuinely missing source uses the same "Template not found"
		// message authorizeRead below writes on denial (ptone/scion#1916),
		// so the two outcomes cannot be told apart by message text either.
		if err == store.ErrNotFound {
			NotFound(w, "Template")
		} else {
			writeErrorFromErr(w, err, "")
		}
		return
	}

	// SECURITY-GATE: authorize read access to the source template before its
	// content is copied into the clone. The destination-scope check below
	// only authorizes ActionCreate at the destination; without this gate
	// that check alone would let any caller who may create a template
	// somewhere copy the contents of a source template they cannot
	// otherwise read. Reading the clone source is a read like any other
	// (ptone/scion#1916): use authorizeRead so a forbidden source template
	// reads as the same 404 as a nonexistent one, rather than a 403 that
	// confirms it exists.
	if !s.authorizeRead(w, r, templateResource(source), "Template") {
		return
	}

	var req CloneTemplateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if !isValidTemplateScope(req.Scope) {
		ValidationError(w, fmt.Sprintf("invalid scope %q: must be \"global\", \"project\" or \"user\"", req.Scope), nil)
		return
	}

	// Resolve scope ID
	scopeID := req.ScopeID
	if scopeID == "" && req.ProjectID != "" {
		scopeID = req.ProjectID
	}

	// Authorize: check destination scope for ActionCreate. Legacy scope
	// names are normalized here — in the one place shared by this switch,
	// the clone record below, and the storage path resolution further
	// down — so all three always agree on which scope a clone targets
	// (ptone/scion#1977).
	destScope := req.Scope
	if destScope == "" {
		destScope = store.TemplateScopeProject
	}
	destScope = projectcompat.CanonicalResourceScope(destScope)
	switch destScope {
	case store.TemplateScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, templateScopeResource(store.TemplateScopeGlobal, ""), ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create global resources", nil)
			return
		}
	case store.TemplateScopeProject:
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			if !agentIdent.HasScope(ScopeAgentCreate) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope", nil)
				return
			}
			if scopeID != agentIdent.ProjectID() {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only manage resources within their own project", nil)
				return
			}
		} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			decision := s.authzService.CheckAccess(ctx, userIdent,
				templateScopeResource(store.TemplateScopeProject, scopeID), ActionCreate)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create resources in this project", nil)
				return
			}
		} else {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
	case store.TemplateScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		// User scope: scopeID must match the authenticated user
		if scopeID == "" {
			scopeID = userIdent.ID()
		} else if scopeID != userIdent.ID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You can only clone templates into your own user scope", nil)
			return
		}
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Cloning into this resource scope is not supported", nil)
		return
	}

	// Create new template based on source
	clone := &store.Template{
		ID:           api.NewUUID(),
		Name:         req.Name,
		Slug:         api.Slugify(req.Name),
		DisplayName:  source.DisplayName,
		Description:  source.Description,
		Harness:      source.Harness,
		Image:        source.Image,
		Config:       source.Config,
		Scope:        destScope,
		ScopeID:      scopeID,
		ProjectID:    scopeID,
		BaseTemplate: source.ID, // Track the source template
		Status:       store.TemplateStatusPending,
	}

	// For user-scoped clones, set the owner from the authenticated user
	if clone.Scope == store.TemplateScopeUser {
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			clone.OwnerID = userIdent.ID()
			clone.CreatedBy = userIdent.ID()
		}
	}

	// Detect a name collision at the destination BEFORE any storage write.
	// The clone below copies files to a path derived from (scope, scopeID,
	// slug); if a record already occupies that path, copying into it first
	// and only checking for the collision at CreateTemplate time would
	// silently overwrite the existing record's files ahead of ever reporting
	// the conflict — and the failure-path cleanup a few lines down would
	// then delete a prefix this request never owned.
	if existing, err := s.store.GetTemplateBySlug(ctx, clone.Slug, clone.Scope, clone.ScopeID); err != nil && err != store.ErrNotFound {
		writeErrorFromErr(w, err, "")
		return
	} else if existing != nil {
		writeError(w, http.StatusConflict, "conflict", "A resource with this slug already exists in the target scope. Choose a different name.", nil)
		return
	}

	// Generate storage path for the clone
	storagePath := storage.TemplateStoragePath(s.HubID(), clone.Scope, clone.ScopeID, clone.Slug)
	clone.StoragePath = storagePath

	stor := s.GetStorage()
	if stor != nil {
		clone.StorageBucket = stor.Bucket()
		clone.StorageURI = storage.TemplateStorageURI(s.HubID(), stor.Bucket(), clone.Scope, clone.ScopeID, clone.Slug)
	}

	// Copy files from source to clone location
	if stor != nil && len(source.Files) > 0 && source.StoragePath != "" {
		for _, file := range source.Files {
			srcPath := source.StoragePath + "/" + file.Path
			dstPath := storagePath + "/" + file.Path
			if _, err := stor.Copy(ctx, srcPath, dstPath); err != nil {
				_ = stor.DeletePrefix(ctx, storagePath)
				RuntimeError(w, "Failed to copy files: "+err.Error())
				return
			}
		}
		clone.Files = source.Files
		clone.ContentHash = source.ContentHash
		clone.Status = store.TemplateStatusActive
	}

	if err := s.store.CreateTemplate(ctx, clone); err != nil {
		if stor != nil {
			_ = stor.DeletePrefix(ctx, storagePath)
		}
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusConflict, "conflict", "A resource with this slug already exists in the target scope. Choose a different name.", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, clone)
}

// templateScopeResource builds an ad hoc "template" Resource for
// authorization checks that have a project or global scope to evaluate but
// no concrete store.Template record (e.g. authorizing a create or clone
// destination before the new record exists). scope should be
// store.TemplateScopeProject or store.TemplateScopeGlobal; scopeID is the
// owning project ID for project scope, and is ignored for global scope.
//
// User scope is deliberately not handled here — use
// templateUserScopeResource, which takes the authenticated UserIdentity
// directly instead of a bare scopeID string. See that function's doc for
// why the distinction matters (ptone/scion#2015).
//
// ptone/scion#1916: every ad hoc "template" Resource literal must set
// ScopeKind through this constructor (or templateResource, for a real
// record, or templateUserScopeResource, for user scope), never by hand —
// filterHubWideTemplateGrants only narrows the curated hub-member/hub-viewer
// grant when ScopeKind is populated, so a hand-built literal that forgets it
// would fall through unfiltered. See
// TestTemplateResourceLiterals_AllUseCanonicalConstructor.
func templateScopeResource(scope, scopeID string) Resource {
	r := Resource{Type: "template", ScopeKind: scope}
	if scope == store.TemplateScopeProject && scopeID != "" {
		r.ParentType = "project"
		r.ParentID = scopeID
	}
	return r
}

// templateUserScopeResource builds the ad hoc "template" Resource for the
// user-scope authorization check in template create (createTemplateV2), the
// user-scope counterpart to templateScopeResource.
//
// It takes the authenticated UserIdentity directly, never a bare scopeID
// string (ptone/scion#2015): Resource.OwnerID drives the kernel's
// "relationship grant: resource owner" check (authz.go), so whatever lands
// there gets ownership access to that scope. Taking UserIdentity makes
// sourcing OwnerID from an unvalidated request field a type error rather
// than a doc-comment violation.
//
// filterHubWideTemplateGrants (ptone/scion#1916) narrows the hub-member/
// hub-viewer system-scope grant to global-scope records only, so without
// OwnerID set here, a user-scope create would have no candidate binding.
func templateUserScopeResource(userIdent UserIdentity) Resource {
	return Resource{Type: "template", ScopeKind: store.TemplateScopeUser, OwnerID: userIdent.ID()}
}

// authorizeTemplateReadRoute is the shared read gate for every template read
// surface that must also admit a runtime broker: get, download, and files
// (template_file_handlers.go). A denial reads as 404, matching authorizeRead.
//
// An authenticated broker is scoped by brokerMayReadCatalogResource rather
// than admitted unconditionally: brokers read templates during agent
// creation (hydration) over HMAC auth and are not user principals, so the
// authorization kernel cannot evaluate them, but that is authority to
// hydrate the projects the broker serves plus the hub-wide catalog, not
// every project's or user's private template.
//
// A global-scope template is likewise allowed outright for an agent
// identity, ahead of the ordinary authorizeRead/CheckAccess call: the
// kernel's agent synthetic binding is project-scoped by design (see the
// comment on Decide's step 5b, authz.go) and deliberately does not carry a
// hub-wide-catalog exception, because that would leak into every other
// parentless resource type the kernel evaluates for agents. Applying the
// exception here instead — the same way catalogListReadBatch
// (authorized_list.go) applies it to list — keeps list and this per-ID read
// in agreement without widening what CheckAccess itself grants an agent.
func (s *Server) authorizeTemplateReadRoute(w http.ResponseWriter, r *http.Request, template *store.Template) bool {
	ctx := r.Context()
	if broker := GetBrokerIdentityFromContext(ctx); broker != nil {
		if s.brokerMayReadCatalogResource(ctx, broker, template.Scope, template.ScopeID) {
			return true
		}
		NotFound(w, "Template")
		return false
	}
	if template.Scope == store.TemplateScopeGlobal {
		if agent := GetAgentIdentityFromContext(ctx); agent != nil {
			return true
		}
	}
	return s.authorizeRead(w, r, templateResource(template), "Template")
}

// computeContentHash computes the aggregate content hash for a set of resource
// files. It is a thin adapter over transfer.ComputeContentHash — the single
// canonical implementation also used by the broker-side hydrator, the transfer
// collector, and the hubclient manifest builder. Routing every hub call site
// through the same implementation guarantees the hub and the broker can never
// compute divergent hashes for identical content (which would silently break
// cache lookups). Per the resource-storage refactor (§7.3/§9), transfer owns
// the canonical hash until a ResourceStore abstraction subsumes it.
func computeContentHash(files []store.TemplateFile) string {
	fileInfos := make([]transfer.FileInfo, len(files))
	for i, f := range files {
		fileInfos[i] = transfer.FileInfo{
			Path: f.Path,
			Size: f.Size,
			Hash: f.Hash,
			Mode: f.Mode,
		}
	}
	return transfer.ComputeContentHash(fileInfos)
}
