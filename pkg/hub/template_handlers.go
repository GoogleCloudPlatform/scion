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
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SignedURLExpiry is the duration signed URLs are valid for.
const SignedURLExpiry = 15 * time.Minute

// CreateTemplateRequest is the request body for creating a template.
type CreateTemplateRequest struct {
	Name         string                `json:"name"`
	Slug         string                `json:"slug,omitempty"`
	DisplayName  string                `json:"displayName,omitempty"`
	Description  string                `json:"description,omitempty"`
	Harness      string                `json:"harness,omitempty"`
	Scope        string                `json:"scope"`
	ScopeID      string                `json:"scopeId,omitempty"`
	ProjectID      string                `json:"projectId,omitempty"` // Deprecated: use ScopeID
	Config       *store.TemplateConfig `json:"config,omitempty"`
	BaseTemplate string                `json:"baseTemplate,omitempty"`
	Visibility   string                `json:"visibility,omitempty"`
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
	Name       string `json:"name"`
	Scope      string `json:"scope"`
	ScopeID    string `json:"scopeId,omitempty"`
	ProjectID    string `json:"projectId,omitempty"` // Deprecated
	Visibility string `json:"visibility,omitempty"`
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
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.TemplateFilter{
		Name:    query.Get("name"),
		Scope:   query.Get("scope"),
		ScopeID: query.Get("scopeId"),
		ProjectID: query.Get("projectId"), // Backwards compat
		Harness: query.Get("harness"),
		Status:  query.Get("status"),
		Search:  query.Get("search"),
	}

	// Default to active templates only
	if filter.Status == "" {
		filter.Status = store.TemplateStatusActive
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
	// Resolve scope ID
	scopeID := req.ScopeID
	if scopeID == "" && req.ProjectID != "" {
		scopeID = req.ProjectID
	}

	// Generate slug from request or name
	slug := req.Slug
	if slug == "" {
		slug = api.Slugify(req.Name)
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
		ProjectID:      scopeID, // Keep for backwards compat
		BaseTemplate: req.BaseTemplate,
		Visibility:   req.Visibility,
		Status:       store.TemplateStatusPending, // Start as pending until files uploaded
	}

	if template.Scope == "" {
		template.Scope = store.TemplateScopeGlobal
	}
	if template.Visibility == "" {
		template.Visibility = store.VisibilityPrivate
	}

	// If no files provided, mark as active immediately
	if len(req.Files) == 0 {
		template.Status = store.TemplateStatusActive
	}

	// Generate storage path and URI
	storagePath := storage.TemplateStoragePath(template.Scope, template.ScopeID, template.Slug)
	template.StoragePath = storagePath

	// Get storage client if available
	stor := s.GetStorage()
	if stor != nil {
		template.StorageBucket = stor.Bucket()
		template.StorageURI = storage.TemplateStorageURI(stor.Bucket(), template.Scope, template.ScopeID, template.Slug)
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

// updateTemplateV2 replaces a template (upsert style).
func (s *Server) updateTemplateV2(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Check if template is locked
	if existing.Locked {
		ValidationError(w, "template is locked and cannot be modified", nil)
		return
	}

	var template store.Template
	if err := readJSON(r, &template); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Preserve immutable fields
	template.ID = existing.ID
	template.Created = existing.Created
	template.CreatedBy = existing.CreatedBy
	template.Locked = existing.Locked

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

	// Check if template is locked
	if existing.Locked {
		ValidationError(w, "template is locked and cannot be modified", nil)
		return
	}

	var updates struct {
		Name        string `json:"name,omitempty"`
		Slug        string `json:"slug,omitempty"`
		DisplayName string `json:"displayName,omitempty"`
		Description string `json:"description,omitempty"`
		Visibility  string `json:"visibility,omitempty"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Apply updates
	if updates.Name != "" {
		existing.Name = updates.Name
		if updates.Slug == "" {
			existing.Slug = api.Slugify(updates.Name)
		}
	}
	if updates.Slug != "" {
		existing.Slug = updates.Slug
	}
	if updates.DisplayName != "" {
		existing.DisplayName = updates.DisplayName
	}
	if updates.Description != "" {
		existing.Description = updates.Description
	}
	if updates.Visibility != "" {
		existing.Visibility = updates.Visibility
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
	force := query.Get("force") == "true"

	existing, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Check if template is locked
	if existing.Locked && !force {
		ValidationError(w, "template is locked; use force=true to delete", nil)
		return
	}

	// If deleteFiles is true and we have storage, delete the files
	if deleteFiles && existing.StoragePath != "" {
		if stor := s.GetStorage(); stor != nil {
			if err := stor.DeletePrefix(ctx, existing.StoragePath); err != nil {
				// Log but continue with database deletion
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

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	if len(template.Files) == 0 {
		ValidationError(w, "template has no files", nil)
		return
	}

	// Generate download URLs using shared helper
	downloadURLs, manifestURL, expires, _ := generateDownloadURLs(ctx, stor, template.StoragePath, template.Files)

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

// handleTemplateClone creates a copy of a template.
func (s *Server) handleTemplateClone(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	source, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
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

	// Resolve scope ID
	scopeID := req.ScopeID
	if scopeID == "" && req.ProjectID != "" {
		scopeID = req.ProjectID
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
		Scope:        req.Scope,
		ScopeID:      scopeID,
		ProjectID:      scopeID,
		BaseTemplate: source.ID, // Track the source template
		Visibility:   req.Visibility,
		Status:       store.TemplateStatusPending,
	}

	if clone.Scope == "" {
		clone.Scope = store.TemplateScopeProject
	}
	if clone.Visibility == "" {
		clone.Visibility = source.Visibility
	}

	// Generate storage path for the clone
	storagePath := storage.TemplateStoragePath(clone.Scope, clone.ScopeID, clone.Slug)
	clone.StoragePath = storagePath

	stor := s.GetStorage()
	if stor != nil {
		clone.StorageBucket = stor.Bucket()
		clone.StorageURI = storage.TemplateStorageURI(stor.Bucket(), clone.Scope, clone.ScopeID, clone.Slug)
	}

	// Copy files from source to clone location
	if stor != nil && len(source.Files) > 0 && source.StoragePath != "" {
		clonedFiles := make([]store.TemplateFile, 0, len(source.Files))
		for _, file := range source.Files {
			srcPath := source.StoragePath + "/" + file.Path
			dstPath := storagePath + "/" + file.Path

			_, err := stor.Copy(ctx, srcPath, dstPath)
			if err != nil {
				// Log but continue
				continue
			}
			clonedFiles = append(clonedFiles, file)
		}
		clone.Files = clonedFiles
		clone.ContentHash = source.ContentHash
		clone.Status = store.TemplateStatusActive
	}

	if err := s.store.CreateTemplate(ctx, clone); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, clone)
}

// computeContentHash computes a content hash from sorted file hashes.
func computeContentHash(files []store.TemplateFile) string {
	// Sort files by path for deterministic ordering
	sorted := make([]store.TemplateFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})

	// Concatenate hashes and compute final hash
	hasher := sha256.New()
	for _, file := range sorted {
		hasher.Write([]byte(file.Hash))
	}

	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}
