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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CreateHarnessConfigRequest is the request body for creating a harness config.
type CreateHarnessConfigRequest struct {
	Name        string                   `json:"name"`
	Slug        string                   `json:"slug,omitempty"`
	DisplayName string                   `json:"displayName,omitempty"`
	Description string                   `json:"description,omitempty"`
	Harness     string                   `json:"harness"`
	Scope       string                   `json:"scope"`
	ScopeID     string                   `json:"scopeId,omitempty"`
	Config      *store.HarnessConfigData `json:"config,omitempty"`
	Visibility  string                   `json:"visibility,omitempty"`
	Files       []FileUploadRequest      `json:"files,omitempty"`
}

// CreateHarnessConfigResponse is the response for harness config creation.
type CreateHarnessConfigResponse struct {
	HarnessConfig *store.HarnessConfig `json:"harnessConfig"`
	UploadURLs    []UploadURLInfo      `json:"uploadUrls,omitempty"`
	ManifestURL   string               `json:"manifestUrl,omitempty"`
}

// HarnessConfigManifest is the manifest of uploaded harness config files.
type HarnessConfigManifest struct {
	Version string               `json:"version"`
	Harness string               `json:"harness,omitempty"`
	Files   []store.TemplateFile `json:"files"`
}

// handleHarnessConfigs handles the /api/v1/harness-configs endpoint.
func (s *Server) handleHarnessConfigs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listHarnessConfigs(w, r)
	case http.MethodPost:
		s.createHarnessConfig(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// listHarnessConfigs lists harness configs with filtering.
func (s *Server) listHarnessConfigs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.HarnessConfigFilter{
		Name:      query.Get("name"),
		Scope:     query.Get("scope"),
		ScopeID:   query.Get("scopeId"),
		ProjectID: query.Get("projectId"),
		Harness:   query.Get("harness"),
		Status:    query.Get("status"),
		Search:    query.Get("search"),
	}

	// Default to active harness configs only
	if filter.Status == "" {
		filter.Status = store.HarnessConfigStatusActive
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListHarnessConfigs(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Compute per-item and scope capabilities (mirrors listTemplatesV2).
	identity := GetIdentityFromContext(ctx)
	items := make([]HarnessConfigWithCapabilities, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = harnessConfigResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "harness_config")
		for i := range result.Items {
			items[i] = HarnessConfigWithCapabilities{HarnessConfig: result.Items[i], Cap: caps[i]}
		}
	} else {
		for i := range result.Items {
			items[i] = HarnessConfigWithCapabilities{HarnessConfig: result.Items[i]}
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "harness_config")
	}

	writeJSON(w, http.StatusOK, ListHarnessConfigsResponse{
		HarnessConfigs: items,
		NextCursor:     result.NextCursor,
		TotalCount:     result.TotalCount,
		Capabilities:   scopeCap,
	})
}

// createHarnessConfig creates a harness config with optional file upload URLs.
func (s *Server) createHarnessConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateHarnessConfigRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if req.Harness == "" {
		ValidationError(w, "harness is required", nil)
		return
	}

	slug := req.Slug
	if slug == "" {
		slug = api.Slugify(req.Name)
	}

	hc := &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        req.Name,
		Slug:        slug,
		DisplayName: req.DisplayName,
		Description: req.Description,
		Harness:     req.Harness,
		Config:      req.Config,
		Scope:       req.Scope,
		ScopeID:     req.ScopeID,
		Visibility:  req.Visibility,
		Status:      store.HarnessConfigStatusPending,
	}

	if hc.Scope == "" {
		hc.Scope = store.HarnessConfigScopeGlobal
	}
	if hc.Visibility == "" {
		hc.Visibility = store.VisibilityPrivate
	}

	// If no files provided, mark as active immediately
	if len(req.Files) == 0 {
		hc.Status = store.HarnessConfigStatusActive
	}

	// Generate storage path and URI
	storagePath := storage.HarnessConfigStoragePath(hc.Scope, hc.ScopeID, hc.Slug)
	hc.StoragePath = storagePath

	stor := s.GetStorage()
	if stor != nil {
		hc.StorageBucket = stor.Bucket()
		hc.StorageURI = storage.HarnessConfigStorageURI(stor.Bucket(), hc.Scope, hc.ScopeID, hc.Slug)
	}

	if err := s.store.CreateHarnessConfig(ctx, hc); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	response := CreateHarnessConfigResponse{
		HarnessConfig: hc,
	}

	// Generate upload URLs if files were specified and storage is available
	if len(req.Files) > 0 && stor != nil {
		uploadURLs, manifestURL, err := generateUploadURLs(ctx, stor, storagePath, req.Files)
		if err == nil || len(uploadURLs) > 0 {
			response.UploadURLs = uploadURLs
			response.ManifestURL = manifestURL
		}
	}

	writeJSON(w, http.StatusCreated, response)
}

// handleHarnessConfigByID handles individual harness config operations.
func (s *Server) handleHarnessConfigByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/harness-configs/")
	if path == "" {
		NotFound(w, "HarnessConfig")
		return
	}

	parts := strings.SplitN(path, "/", 2)
	hcID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		s.handleHarnessConfigCRUD(w, r, hcID)
	case "upload":
		s.handleHarnessConfigUpload(w, r, hcID)
	case "finalize":
		s.handleHarnessConfigFinalize(w, r, hcID)
	case "download":
		s.handleHarnessConfigDownload(w, r, hcID)
	case "clone":
		s.handleHarnessConfigClone(w, r, hcID)
	case "reimport":
		s.handleHarnessConfigReimport(w, r, hcID)
	case "files":
		s.handleHarnessConfigFiles(w, r, hcID, "")
	default:
		if strings.HasPrefix(action, "files/") {
			filePath := strings.TrimPrefix(action, "files/")
			s.handleHarnessConfigFiles(w, r, hcID, filePath)
			return
		}
		NotFound(w, "HarnessConfig action")
	}
}

// handleHarnessConfigCRUD handles basic harness config CRUD operations.
func (s *Server) handleHarnessConfigCRUD(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		s.getHarnessConfig(w, r, id)
	case http.MethodPut:
		s.updateHarnessConfig(w, r, id)
	case http.MethodPatch:
		s.patchHarnessConfig(w, r, id)
	case http.MethodDelete:
		s.deleteHarnessConfig(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getHarnessConfig(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resp := HarnessConfigWithCapabilities{HarnessConfig: *hc}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, harnessConfigResource(hc))
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateHarnessConfig(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var hc store.HarnessConfig
	if err := readJSON(r, &hc); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Preserve immutable fields
	hc.ID = existing.ID
	hc.Created = existing.Created
	hc.CreatedBy = existing.CreatedBy
	if hc.Slug == "" {
		hc.Slug = api.Slugify(hc.Name)
	}

	if err := s.store.UpdateHarnessConfig(ctx, &hc); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, hc)
}

func (s *Server) patchHarnessConfig(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
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

	if err := s.store.UpdateHarnessConfig(ctx, existing); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) deleteHarnessConfig(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"

	existing, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize: check source scope for ActionDelete
	switch existing.Scope {
	case store.HarnessConfigScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{Type: "harness_config"}, ActionDelete)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to delete global resources", nil)
			return
		}
	case store.HarnessConfigScopeProject:
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
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "harness_config", ParentType: "project", ParentID: existing.ScopeID,
			}, ActionDelete)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to delete resources in this project", nil)
				return
			}
		} else {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
	case store.HarnessConfigScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		if existing.OwnerID != userIdent.ID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You do not have permission to delete another user's harness config", nil)
			return
		}
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Delete is not supported for this resource scope", nil)
		return
	}

	if deleteFiles && existing.StoragePath != "" {
		if stor := s.GetStorage(); stor != nil {
			_ = stor.DeletePrefix(ctx, existing.StoragePath)
		}
	}

	if err := s.store.DeleteHarnessConfig(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleHarnessConfigUpload handles requests for upload URLs.
func (s *Server) handleHarnessConfigUpload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
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

	if hc.StoragePath == "" {
		RuntimeError(w, "Harness config storage path not configured (id: "+id+")")
		return
	}

	uploadURLs, manifestURL, err := generateUploadURLs(ctx, stor, hc.StoragePath, req.Files)
	if err != nil {
		RuntimeError(w, "Failed to generate upload URLs: "+err.Error())
		return
	}
	if len(uploadURLs) == 0 && len(req.Files) > 0 {
		RuntimeError(w, "Failed to generate upload URLs")
		return
	}

	writeJSON(w, http.StatusOK, UploadResponse{
		UploadURLs:  uploadURLs,
		ManifestURL: manifestURL,
	})
}

// handleHarnessConfigFinalize finalizes a harness config after file upload.
func (s *Server) handleHarnessConfigFinalize(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	var req struct {
		Manifest *HarnessConfigManifest `json:"manifest"`
	}
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Manifest == nil || len(req.Manifest.Files) == 0 {
		ValidationError(w, "manifest with files is required", nil)
		return
	}

	contentHash, err := verifyAndFinalizeFiles(ctx, stor, hc.StoragePath, req.Manifest.Files)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	hc.Files = req.Manifest.Files
	hc.ContentHash = contentHash
	hc.Status = store.HarnessConfigStatusActive

	if err := s.store.UpdateHarnessConfig(ctx, hc); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, hc)
}

// handleHarnessConfigDownload returns signed URLs for downloading harness config files.
func (s *Server) handleHarnessConfigDownload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	if len(hc.Files) == 0 {
		ValidationError(w, "harness config has no files", nil)
		return
	}

	downloadURLs, manifestURL, expires, _ := generateDownloadURLs(ctx, stor, hc.StoragePath, hc.Files)

	writeJSON(w, http.StatusOK, DownloadResponse{
		Files:       downloadURLs,
		ManifestURL: manifestURL,
		Expires:     expires,
	})
}

// handleHarnessConfigClone creates a copy of a harness config.
func (s *Server) handleHarnessConfigClone(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	source, err := s.store.GetHarnessConfig(ctx, id)
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

	// Authorize: check destination scope for ActionCreate
	destScope := req.Scope
	if destScope == "" {
		destScope = source.Scope
	}
	if destScope == "" {
		destScope = store.HarnessConfigScopeGlobal
	}
	switch destScope {
	case store.HarnessConfigScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{Type: "harness_config"}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create global resources", nil)
			return
		}
	case store.HarnessConfigScopeProject:
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
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "harness_config", ParentType: "project", ParentID: scopeID,
			}, ActionCreate)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create resources in this project", nil)
				return
			}
		} else {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
	}

	clone := &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        req.Name,
		Slug:        api.Slugify(req.Name),
		DisplayName: source.DisplayName,
		Description: source.Description,
		Harness:     source.Harness,
		Config:      source.Config,
		Scope:       destScope,
		ScopeID:     scopeID,
		Visibility:  req.Visibility,
		Status:      store.HarnessConfigStatusPending,
	}

	if clone.Visibility == "" {
		clone.Visibility = source.Visibility
	}

	storagePath := storage.HarnessConfigStoragePath(clone.Scope, clone.ScopeID, clone.Slug)
	clone.StoragePath = storagePath

	stor := s.GetStorage()
	if stor != nil {
		clone.StorageBucket = stor.Bucket()
		clone.StorageURI = storage.HarnessConfigStorageURI(stor.Bucket(), clone.Scope, clone.ScopeID, clone.Slug)
	}

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
		clone.Status = store.HarnessConfigStatusActive
	}

	if err := s.store.CreateHarnessConfig(ctx, clone); err != nil {
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

// ReimportHarnessConfigRequest is the optional request body for the reimport endpoint.
type ReimportHarnessConfigRequest struct {
	SourceURL string `json:"sourceUrl,omitempty"`
}

// handleHarnessConfigReimport re-imports a harness-config from its stored
// source_url (or an override URL). POST /api/v1/harness-configs/{id}/reimport
func (s *Server) handleHarnessConfigReimport(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if hc == nil {
		NotFound(w, "HarnessConfig")
		return
	}

	var req ReimportHarnessConfigRequest
	if r.Body != nil && r.Body != http.NoBody {
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
	}

	sourceURL := req.SourceURL
	if sourceURL == "" {
		sourceURL = hc.SourceURL
	}
	if sourceURL == "" {
		writeError(w, http.StatusBadRequest, "no_source_url",
			"No source URL stored and none provided. Use the sourceUrl field to specify one.", nil)
		return
	}

	sourceURL = config.NormalizeTemplateSourceURL(sourceURL)

	// Authorize: same as import — harness_config:create on the owning scope.
	switch hc.Scope {
	case store.HarnessConfigScopeGlobal:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{Type: "harness_config"}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to reimport global resources", nil)
			return
		}
	case store.HarnessConfigScopeProject:
		if !s.authorizeProjectImport(ctx, w, hc.ScopeID, "harness-configs") {
			return
		}
	case store.HarnessConfigScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		if hc.OwnerID != userIdent.ID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to reimport another user's harness config", nil)
			return
		}
	default:
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Reimport is not supported for this resource scope", nil)
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Storage is not configured", nil)
		return
	}

	kind := s.harnessConfigImportKind()
	run := func(progress importProgressFunc) ([]string, error) {
		return s.importFromRemote(ctx, hc.ScopeID, sourceURL, hc.Scope, kind, progress, nil)
	}

	if importAcceptsNDJSON(r) {
		s.streamImport(w, run)
		return
	}

	imported, err := run(nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reimport_failed", err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, ImportHarnessConfigsResponse{
		HarnessConfigs: imported,
		Count:          len(imported),
	})
}
