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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"golang.org/x/sync/errgroup"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// CreateSkillRequest is the request body for creating a skill.
type CreateSkillRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Scope       string   `json:"scope"`
	ScopeID     string   `json:"scopeId,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// CreateSkillResponse is the response for skill creation.
type CreateSkillResponse struct {
	Skill *store.Skill `json:"skill"`
}

// ListSkillsResponse is the response for listing skills.
type ListSkillsResponse struct {
	Skills       []SkillWithCapabilities `json:"skills"`
	NextCursor   string                  `json:"nextCursor,omitempty"`
	TotalCount   int                     `json:"totalCount"`
	Capabilities *Capabilities           `json:"_capabilities,omitempty"`
}

// SkillWithCapabilities wraps a store.Skill with capability annotations.
type SkillWithCapabilities struct {
	store.Skill
	Cap *Capabilities `json:"_capabilities,omitempty"`
}

// PublishVersionRequest is the request body for creating a skill version.
type PublishVersionRequest struct {
	Version string              `json:"version"`
	Files   []FileUploadRequest `json:"files,omitempty"`
}

// PublishVersionResponse is the response for version creation.
type PublishVersionResponse struct {
	Version    *store.SkillVersion `json:"version"`
	UploadURLs []UploadURLInfo     `json:"uploadUrls,omitempty"`
}

// FinalizeSkillVersionRequest is the request body for finalizing a skill version.
type FinalizeSkillVersionRequest struct {
	Version  string         `json:"version"`
	Manifest *SkillManifest `json:"manifest"`
}

// SkillManifest is the manifest of uploaded skill files.
type SkillManifest struct {
	Files []store.TemplateFile `json:"files"`
}

// ResolveSkillsRequest is the request body for batch skill resolution.
type ResolveSkillsRequest struct {
	Skills    []ResolveSkillRef `json:"skills"`
	ProjectID string            `json:"projectId,omitempty"`
	UserID    string            `json:"userId,omitempty"`
}

// ResolveSkillRef is a reference to a skill to resolve.
type ResolveSkillRef struct {
	URI string `json:"uri"`
}

// ResolveSkillsResponse is the response for batch skill resolution.
type ResolveSkillsResponse struct {
	Resolved []ResolvedSkillResponse `json:"resolved"`
	Errors   []ResolveSkillError     `json:"errors,omitempty"`
}

// ResolvedSkillResponse is a single resolved skill in the batch response.
type ResolvedSkillResponse struct {
	URI                string            `json:"uri"`
	Name               string            `json:"name"`
	ResolvedVersion    string            `json:"resolvedVersion"`
	ContentHash        string            `json:"contentHash"`
	Files              []DownloadURLInfo `json:"files"`
	Deprecated         bool              `json:"deprecated,omitempty"`
	DeprecationMessage string            `json:"deprecationMessage,omitempty"`
	ReplacementURI     string            `json:"replacementUri,omitempty"`
}

// ResolveSkillError describes a resolution failure for a single skill.
type ResolveSkillError struct {
	URI     string `json:"uri"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// UpdateSkillRequest is the request body for updating a skill.
type UpdateSkillRequest struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// DeprecateVersionRequest is the request body for deprecating a skill version.
type DeprecateVersionRequest struct {
	Message        string `json:"message"`
	ReplacementURI string `json:"replacementUri,omitempty"`
}

// handleSkills dispatches /api/v1/skills (GET=list, POST=create).
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSkills(w, r)
	case http.MethodPost:
		s.createSkill(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleSkillByID dispatches /api/v1/skills/{id}[/{action}[/{subId}]].
func (s *Server) handleSkillByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/skills/")
	if path == "" {
		NotFound(w, "Skill")
		return
	}

	parts := strings.SplitN(path, "/", 3)
	skillID := parts[0]

	// Batch resolve is routed through a non-UUID path segment.
	if skillID == "resolve" {
		s.handleSkillsResolve(w, r)
		return
	}

	if len(parts) == 1 {
		s.handleSkillCRUD(w, r, skillID)
		return
	}

	action := parts[1]
	switch action {
	case "versions":
		if len(parts) == 3 {
			s.handleSkillVersionByID(w, r, skillID, parts[2])
		} else {
			s.handleSkillVersions(w, r, skillID)
		}
	case "upload":
		s.handleSkillUpload(w, r, skillID)
	case "finalize":
		s.handleSkillFinalize(w, r, skillID)
	case "download":
		s.handleSkillDownload(w, r, skillID)
	case "resolve":
		s.handleSkillResolveSingle(w, r, skillID)
	default:
		NotFound(w, "Skill action")
	}
}

// handleSkillCRUD handles basic skill CRUD operations.
func (s *Server) handleSkillCRUD(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		s.getSkill(w, r, id)
	case http.MethodPatch:
		s.updateSkill(w, r, id)
	case http.MethodDelete:
		s.deleteSkill(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

// listSkills lists skills with filtering.
func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.SkillFilter{
		Name:    query.Get("name"),
		Scope:   query.Get("scope"),
		ScopeID: query.Get("scopeId"),
		OwnerID: query.Get("ownerId"),
		Status:  query.Get("status"),
		Search:  query.Get("search"),
	}
	if tagsParam := query.Get("tags"); tagsParam != "" {
		filter.Tags = strings.Split(tagsParam, ",")
	}

	if filter.Status == "" {
		filter.Status = "active"
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListSkills(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	skills := make([]SkillWithCapabilities, 0, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = skillResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "skill")
		for i := range result.Items {
			if !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			skills = append(skills, SkillWithCapabilities{Skill: result.Items[i], Cap: caps[i]})
		}
	} else {
		for i := range result.Items {
			skills = append(skills, SkillWithCapabilities{Skill: result.Items[i]})
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "skill")
	}

	totalCount := result.TotalCount
	if identity != nil {
		totalCount = len(skills)
	}

	writeJSON(w, http.StatusOK, ListSkillsResponse{
		Skills:       skills,
		NextCursor:   result.NextCursor,
		TotalCount:   totalCount,
		Capabilities: scopeCap,
	})
}

// createSkill creates a new skill record.
func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateSkillRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	if err := api.ValidateSkillName(req.Name); err != nil {
		ValidationError(w, fmt.Sprintf("invalid skill name: %v", err), nil)
		return
	}

	// Validate scope
	scope := req.Scope
	if scope == "" {
		scope = store.SkillScopeGlobal
	}
	switch scope {
	case store.SkillScopeGlobal, store.SkillScopeProject, store.SkillScopeUser, store.SkillScopeCore:
	default:
		ValidationError(w, fmt.Sprintf("invalid scope %q: must be one of global, project, user, core", scope), nil)
		return
	}

	// Authorize
	switch scope {
	case store.SkillScopeGlobal, store.SkillScopeCore:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{Type: "skill"}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create global skills", nil)
			return
		}
	case store.SkillScopeProject:
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			if !agentIdent.HasScope(ScopeAgentCreate) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope", nil)
				return
			}
			if req.ScopeID != agentIdent.ProjectID() {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only manage resources within their own project", nil)
				return
			}
		} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type: "skill", ParentType: "project", ParentID: req.ScopeID,
			}, ActionCreate)
			if !decision.Allowed {
				writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create skills in this project", nil)
				return
			}
		} else {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
	case store.SkillScopeUser:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "User authentication required for user-scoped skills", nil)
			return
		}
		req.ScopeID = userIdent.ID()
	}

	slug := api.Slugify(req.Name)

	skill := &store.Skill{
		ID:          api.NewUUID(),
		Name:        req.Name,
		Slug:        slug,
		Description: req.Description,
		Tags:        req.Tags,
		Scope:       scope,
		ScopeID:     req.ScopeID,
		Visibility:  req.Visibility,
		Status:      "active",
	}
	if skill.Visibility == "" {
		skill.Visibility = store.VisibilityPrivate
	}

	// Set owner from identity
	if identity := GetIdentityFromContext(ctx); identity != nil {
		skill.OwnerID = identity.ID()
		skill.CreatedBy = identity.ID()
		skill.UpdatedBy = identity.ID()
	}

	// Generate storage path and URI
	storagePath := storage.SkillStoragePath(skill.Scope, skill.ScopeID, skill.Slug)
	skill.StoragePath = storagePath

	stor := s.GetStorage()
	if stor != nil {
		skill.StorageBucket = stor.Bucket()
		skill.StorageURI = storage.SkillStorageURI(stor.Bucket(), skill.Scope, skill.ScopeID, skill.Slug)
	}

	if err := s.store.CreateSkill(ctx, skill); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusConflict, "conflict", "A skill with this slug already exists in the target scope", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, CreateSkillResponse{Skill: skill})
}

// getSkill retrieves a skill with capabilities.
func (s *Server) getSkill(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	skill, err := s.store.GetSkill(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity != nil {
		decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
		if !decision.Allowed {
			NotFound(w, "Skill")
			return
		}
	}

	resp := SkillWithCapabilities{Skill: *skill}
	if identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, skillResource(skill))
	}

	writeJSON(w, http.StatusOK, resp)
}

// updateSkill updates specific skill fields.
func (s *Server) updateSkill(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetSkill(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(existing), ActionUpdate)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to update this skill", nil)
		return
	}

	var updates UpdateSkillRequest
	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if updates.Name != "" {
		existing.Name = updates.Name
		existing.Slug = api.Slugify(updates.Name)
	}
	if updates.Description != "" {
		existing.Description = updates.Description
	}
	if updates.Visibility != "" {
		existing.Visibility = updates.Visibility
	}
	if updates.Tags != nil {
		existing.Tags = updates.Tags
	}

	if identity := GetIdentityFromContext(ctx); identity != nil {
		existing.UpdatedBy = identity.ID()
	}

	if err := s.store.UpdateSkill(ctx, existing); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

// deleteSkill soft-deletes a skill by setting status to archived.
func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	existing, err := s.store.GetSkill(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(existing), ActionDelete)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to delete this skill", nil)
		return
	}

	if err := s.store.DeleteSkill(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleSkillVersions handles /api/v1/skills/{id}/versions (GET=list, POST=create).
func (s *Server) handleSkillVersions(w http.ResponseWriter, r *http.Request, skillID string) {
	switch r.Method {
	case http.MethodGet:
		s.listSkillVersions(w, r, skillID)
	case http.MethodPost:
		s.publishSkillVersion(w, r, skillID)
	default:
		MethodNotAllowed(w)
	}
}

// handleSkillVersionByID handles /api/v1/skills/{id}/versions/{versionId}[/deprecate].
func (s *Server) handleSkillVersionByID(w http.ResponseWriter, r *http.Request, skillID, versionID string) {
	if strings.HasSuffix(versionID, "/deprecate") {
		vid := strings.TrimSuffix(versionID, "/deprecate")
		s.deprecateSkillVersion(w, r, skillID, vid)
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	s.getSkillVersion(w, r, skillID, versionID)
}

// listSkillVersions lists versions for a skill.
func (s *Server) listSkillVersions(w http.ResponseWriter, r *http.Request, skillID string) {
	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity != nil {
		decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
		if !decision.Allowed {
			NotFound(w, "Skill")
			return
		}
	}

	result, err := s.store.ListSkillVersions(ctx, skillID, store.ListOptions{
		Limit: 100,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// getSkillVersion retrieves a specific skill version.
func (s *Server) getSkillVersion(w http.ResponseWriter, r *http.Request, skillID, versionID string) {
	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity != nil {
		decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
		if !decision.Allowed {
			NotFound(w, "Skill")
			return
		}
	}

	sv, err := s.store.GetSkillVersion(ctx, versionID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if sv.SkillID != skillID {
		NotFound(w, "SkillVersion")
		return
	}

	writeJSON(w, http.StatusOK, sv)
}

// deprecateSkillVersion marks a published skill version as deprecated.
func (s *Server) deprecateSkillVersion(w http.ResponseWriter, r *http.Request, skillID, versionID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionUpdate)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to deprecate versions of this skill", nil)
		return
	}

	var req DeprecateVersionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if req.Message == "" {
		ValidationError(w, "message is required for deprecation", nil)
		return
	}

	sv, err := s.store.GetSkillVersion(ctx, versionID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if sv.SkillID != skillID {
		NotFound(w, "SkillVersion")
		return
	}
	if sv.Status != store.SkillVersionStatusPublished {
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("only published versions can be deprecated (current status: %s)", sv.Status), nil)
		return
	}

	sv.Status = store.SkillVersionStatusDeprecated
	sv.DeprecationMessage = req.Message
	sv.ReplacementURI = req.ReplacementURI

	if err := s.store.UpdateSkillVersion(ctx, sv); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, sv)
}

// publishSkillVersion creates a new draft version and returns upload URLs.
func (s *Server) publishSkillVersion(w http.ResponseWriter, r *http.Request, skillID string) {
	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize: publishing a version is an update on the skill
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionUpdate)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to publish versions for this skill", nil)
		return
	}

	// Dispatch based on content type: multipart uploads are handled inline,
	// while JSON requests go through the existing two-phase upload flow.
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
		s.publishSkillVersionMultipart(w, r, skill)
		return
	}

	var req PublishVersionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Version == "" {
		ValidationError(w, "version is required", nil)
		return
	}

	// Validate semver
	if _, err := semver.NewVersion(req.Version); err != nil {
		ValidationError(w, fmt.Sprintf("invalid semver version %q: %s", req.Version, err.Error()), nil)
		return
	}

	// Check for an existing version with this number.
	// - Draft: reuse it (idempotent retry of an incomplete publish).
	// - Published/deprecated/archived: reject as conflict.
	var sv *store.SkillVersion
	existing, err := s.store.GetSkillVersionByNumber(ctx, skillID, req.Version)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErrorFromErr(w, err, "")
		return
	}
	if err == nil {
		switch existing.Status {
		case store.SkillVersionStatusDraft:
			sv = existing
		case store.SkillVersionStatusPublished:
			writeError(w, http.StatusConflict, "conflict",
				fmt.Sprintf("version %s is already published and immutable; publish a new version instead", req.Version), nil)
			return
		default:
			writeError(w, http.StatusConflict, "conflict",
				fmt.Sprintf("version %s already exists with status %q", req.Version, existing.Status), nil)
			return
		}
	}

	if sv == nil {
		// Create new draft version
		sv = &store.SkillVersion{
			ID:      api.NewUUID(),
			SkillID: skillID,
			Version: req.Version,
			Status:  store.SkillVersionStatusDraft,
		}

		if identity := GetIdentityFromContext(ctx); identity != nil {
			sv.PublisherID = identity.ID()
		}

		if err := s.store.CreateSkillVersion(ctx, sv); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				writeError(w, http.StatusConflict, "conflict",
					fmt.Sprintf("version %s already exists for this skill", req.Version), nil)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
	}

	response := PublishVersionResponse{
		Version: sv,
	}

	// Generate upload URLs if files were specified and storage is available
	if len(req.Files) > 0 {
		stor := s.GetStorage()
		if stor != nil {
			versionPath := skill.StoragePath + "/" + req.Version
			uploadURLs, _, err := generateUploadURLs(ctx, stor, versionPath, req.Files)
			if err == nil && len(uploadURLs) > 0 {
				if stor.Provider() == storage.ProviderLocal {
					hubURL := requestBaseURL(r)
					uploadURLs = rewriteLocalUploadURLs(uploadURLs, hubURL, "skills", skillID)
				}
				response.UploadURLs = uploadURLs
			}
		}
	}

	writeJSON(w, http.StatusCreated, response)
}

// publishSkillVersionMultipart handles multipart/form-data skill version
// publishing. Files are uploaded inline in the request body rather than via
// the two-phase signed-URL flow. The caller has already authenticated and
// authorized the request.
func (s *Server) publishSkillVersionMultipart(w http.ResponseWriter, r *http.Request, skill *store.Skill) {
	ctx := r.Context()

	// a) Size limit: 50 MB max request body.
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)

	// b) Parse multipart form: 10 MB in memory, rest spills to disk.
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds 50MB limit", nil)
			return
		}
		BadRequest(w, "Failed to parse multipart form: "+err.Error())
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	// c) Extract and validate version.
	version := r.FormValue("version")
	if version == "" {
		ValidationError(w, "version is required", nil)
		return
	}
	if _, err := semver.NewVersion(version); err != nil {
		ValidationError(w, fmt.Sprintf("invalid semver version %q: %s", version, err.Error()), nil)
		return
	}

	// d) Check for existing published version (immutability).
	existing, err := s.store.GetSkillVersionByNumber(ctx, skill.ID, version)
	if err == nil && existing.Status == store.SkillVersionStatusPublished {
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("version %s is already published and immutable; publish a new version instead", version), nil)
		return
	}

	// e) Extract and validate files.
	fileHeaders := r.MultipartForm.File["file"]
	if len(fileHeaders) == 0 {
		ValidationError(w, "at least one file is required", nil)
		return
	}
	if len(fileHeaders) > 50 {
		ValidationError(w, "too many files (max 50)", nil)
		return
	}

	hasSkillMD := false
	seenFilenames := make(map[string]struct{}, len(fileHeaders))
	for _, fh := range fileHeaders {
		name := fh.Filename

		// Security: reject dangerous filenames.
		if name == "" {
			ValidationError(w, "file has empty filename", nil)
			return
		}
		if strings.Contains(name, "..") {
			ValidationError(w, fmt.Sprintf("filename %q contains path traversal sequence", name), nil)
			return
		}
		if strings.HasPrefix(name, "/") {
			ValidationError(w, fmt.Sprintf("filename %q must not start with /", name), nil)
			return
		}
		if strings.ContainsRune(name, 0) {
			ValidationError(w, fmt.Sprintf("filename %q contains null byte", name), nil)
			return
		}

		// Duplicate filename check.
		if _, dup := seenFilenames[name]; dup {
			ValidationError(w, fmt.Sprintf("duplicate filename %q", name), nil)
			return
		}
		seenFilenames[name] = struct{}{}

		// Per-file size limit: 10 MB.
		if fh.Size > 10*1024*1024 {
			ValidationError(w, fmt.Sprintf("file %q exceeds 10MB limit", name), nil)
			return
		}

		if name == "SKILL.md" {
			hasSkillMD = true
		}
	}
	if !hasSkillMD {
		ValidationError(w, "SKILL.md is required", nil)
		return
	}

	// f) Create draft version.
	sv := &store.SkillVersion{
		ID:      api.NewUUID(),
		SkillID: skill.ID,
		Version: version,
		Status:  store.SkillVersionStatusDraft,
	}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		sv.PublisherID = identity.ID()
	}
	if err := s.store.CreateSkillVersion(ctx, sv); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusConflict, "conflict",
				fmt.Sprintf("version %s already exists for this skill", version), nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// g) Upload files to storage and compute hashes.
	stor := s.GetStorage()
	if stor == nil {
		_ = s.store.DeleteSkillVersion(ctx, sv.ID)
		RuntimeError(w, "Storage not configured")
		return
	}

	type uploadResult struct {
		file store.TemplateFile
	}
	results := make([]uploadResult, len(fileHeaders))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fileUploadConcurrency)
	for i, fh := range fileHeaders {
		i, fh := i, fh
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}

			f, err := fh.Open()
			if err != nil {
				return fmt.Errorf("failed to open file %s: %w", fh.Filename, err)
			}
			defer func() { _ = f.Close() }()

			data, err := io.ReadAll(f)
			if err != nil {
				return fmt.Errorf("failed to read file %s: %w", fh.Filename, err)
			}

			hash := transfer.HashBytes(data)
			objectPath := skill.StoragePath + "/" + version + "/" + fh.Filename

			if _, err := stor.Upload(gctx, objectPath, bytes.NewReader(data), storage.UploadOptions{}); err != nil {
				return fmt.Errorf("failed to upload file %s: %w", fh.Filename, err)
			}

			results[i] = uploadResult{
				file: store.TemplateFile{
					Path: fh.Filename,
					Size: int64(len(data)),
					Hash: hash,
				},
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		_ = s.store.DeleteSkillVersion(ctx, sv.ID)
		RuntimeError(w, "Failed to upload files: "+err.Error())
		return
	}

	// Build manifest from results.
	manifest := make([]store.TemplateFile, len(results))
	for i, r := range results {
		manifest[i] = r.file
	}

	// i) Compute content hash.
	contentHash := computeContentHash(manifest)

	// j) Update version to published.
	sv.Status = store.SkillVersionStatusPublished
	sv.Files = manifest
	sv.ContentHash = contentHash
	if err := s.store.UpdateSkillVersion(ctx, sv); err != nil {
		_ = s.store.DeleteSkillVersion(ctx, sv.ID)
		writeErrorFromErr(w, err, "")
		return
	}

	// k) Return response.
	writeJSON(w, http.StatusCreated, PublishVersionResponse{Version: sv})
}

// handleSkillUpload handles requests for upload URLs for a skill.
func (s *Server) handleSkillUpload(w http.ResponseWriter, r *http.Request, skillID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionUpdate)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to upload files for this skill", nil)
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	var req struct {
		Version string              `json:"version"`
		Files   []FileUploadRequest `json:"files"`
	}
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Version == "" {
		ValidationError(w, "version is required", nil)
		return
	}
	if len(req.Files) == 0 {
		ValidationError(w, "at least one file is required", nil)
		return
	}

	versionPath := skill.StoragePath + "/" + req.Version
	uploadURLs, manifestURL, err := generateUploadURLs(ctx, stor, versionPath, req.Files)
	if err != nil {
		RuntimeError(w, "Failed to generate upload URLs: "+err.Error())
		return
	}

	if stor.Provider() == storage.ProviderLocal {
		hubURL := requestBaseURL(r)
		uploadURLs = rewriteLocalUploadURLs(uploadURLs, hubURL, "skills", skillID)
	}

	writeJSON(w, http.StatusOK, UploadResponse{
		UploadURLs:  uploadURLs,
		ManifestURL: manifestURL,
	})
}

// handleSkillFinalize finalizes a skill version after file upload.
func (s *Server) handleSkillFinalize(w http.ResponseWriter, r *http.Request, skillID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Authorize
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionUpdate)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to finalize versions for this skill", nil)
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	var req FinalizeSkillVersionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Version == "" {
		ValidationError(w, "version is required", nil)
		return
	}
	if req.Manifest == nil || len(req.Manifest.Files) == 0 {
		ValidationError(w, "manifest with files is required", nil)
		return
	}

	// Validate SKILL.md is present
	hasSkillMD := false
	for _, f := range req.Manifest.Files {
		if f.Path == "SKILL.md" {
			hasSkillMD = true
			break
		}
	}
	if !hasSkillMD {
		ValidationError(w, "SKILL.md is required in the manifest", nil)
		return
	}

	// Validate file count and sizes
	if len(req.Manifest.Files) > 50 {
		ValidationError(w, "too many files (max 50)", nil)
		return
	}
	var totalSize int64
	for _, f := range req.Manifest.Files {
		if f.Size > 10*1024*1024 {
			ValidationError(w, fmt.Sprintf("file %q exceeds 10MB limit", f.Path), nil)
			return
		}
		totalSize += f.Size
	}
	if totalSize > 50*1024*1024 {
		ValidationError(w, "total file size exceeds 50MB limit", nil)
		return
	}

	// Look up the draft version
	sv, err := s.store.GetSkillVersionByNumber(ctx, skillID, req.Version)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if sv.Status == store.SkillVersionStatusPublished {
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("version %s is already published and immutable", req.Version), nil)
		return
	}

	// Verify files exist in storage and compute content hash
	versionPath := skill.StoragePath + "/" + req.Version
	contentHash, err := verifyAndFinalizeFiles(ctx, stor, versionPath, req.Manifest.Files)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	// Update version to published
	sv.Files = req.Manifest.Files
	sv.ContentHash = contentHash
	sv.Status = store.SkillVersionStatusPublished

	if err := s.store.UpdateSkillVersion(ctx, sv); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, sv)
}

// handleSkillDownload returns signed URLs for downloading skill version files.
func (s *Server) handleSkillDownload(w http.ResponseWriter, r *http.Request, skillID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()
	query := r.URL.Query()
	version := query.Get("version")
	if version == "" {
		version = "latest"
	}

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity != nil {
		decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
		if !decision.Allowed {
			NotFound(w, "Skill")
			return
		}
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Resolve version
	sv, err := s.store.ResolveSkillVersion(ctx, skillID, version)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if len(sv.Files) == 0 {
		ValidationError(w, "version has no files", nil)
		return
	}

	versionPath := skill.StoragePath + "/" + sv.Version
	downloadURLs, manifestURL, expires, _ := generateDownloadURLs(ctx, stor, versionPath, sv.Files)

	if stor.Provider() == storage.ProviderLocal {
		hubURL := requestBaseURL(r)
		downloadURLs = rewriteLocalDownloadURLs(downloadURLs, hubURL, "skills", skillID)
	}

	writeJSON(w, http.StatusOK, DownloadResponse{
		Files:       downloadURLs,
		ManifestURL: manifestURL,
		Expires:     expires,
	})
}

// handleSkillResolveSingle resolves a single skill version (for debug/test).
func (s *Server) handleSkillResolveSingle(w http.ResponseWriter, r *http.Request, skillID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity != nil {
		decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
		if !decision.Allowed {
			NotFound(w, "Skill")
			return
		}
	}

	version := r.URL.Query().Get("version")
	if version == "" {
		version = "latest"
	}

	sv, err := s.store.ResolveSkillVersion(ctx, skillID, version)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, sv)
}

// handleSkillsResolve handles batch skill resolution: POST /api/v1/skills/resolve.
func (s *Server) handleSkillsResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	var req ResolveSkillsRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Skills) == 0 {
		ValidationError(w, "at least one skill reference is required", nil)
		return
	}

	const maxResolveItems = 50
	if len(req.Skills) > maxResolveItems {
		ValidationError(w, fmt.Sprintf("too many skills in request (max %d)", maxResolveItems), nil)
		return
	}

	stor := s.GetStorage()

	var resolved []ResolvedSkillResponse
	var resolveErrors []ResolveSkillError

	for _, skillRef := range req.Skills {
		uri, err := api.ParseSkillURI(skillRef.URI)
		if err != nil {
			resolveErrors = append(resolveErrors, ResolveSkillError{
				URI: skillRef.URI, Code: "invalid_uri", Message: err.Error(),
			})
			continue
		}

		// Federation: non-scion registry → proxy to external
		if uri.Registry != "scion" && uri.Registry != "" {
			fedResolved, resolveErr := s.federateResolve(ctx, uri.Registry, skillRef)
			if resolveErr != nil {
				resolveErrors = append(resolveErrors, *resolveErr)
			} else {
				resolved = append(resolved, *fedResolved)
			}
			continue
		}

		// Expand scope aliases from request context
		expandScopeAliases(uri, req.ProjectID, req.UserID)

		skill, sv, err := s.resolveSkill(ctx, uri, req.ProjectID)
		if err != nil {
			resolveErrors = append(resolveErrors, ResolveSkillError{
				URI: skillRef.URI, Code: "not_found", Message: err.Error(),
			})
			continue
		}

		identity := GetIdentityFromContext(ctx)
		if identity != nil {
			decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
			if !decision.Allowed {
				resolveErrors = append(resolveErrors, ResolveSkillError{
					URI: skillRef.URI, Code: "forbidden",
					Message: "you do not have permission to access this skill",
				})
				continue
			}
		}

		entry := ResolvedSkillResponse{
			URI:             skillRef.URI,
			Name:            skill.Name,
			ResolvedVersion: sv.Version,
			ContentHash:     sv.ContentHash,
		}

		if sv.Status == store.SkillVersionStatusDeprecated {
			entry.Deprecated = true
			entry.DeprecationMessage = sv.DeprecationMessage
			entry.ReplacementURI = sv.ReplacementURI
		}

		// Generate download URLs for the resolved version's files
		if stor != nil && len(sv.Files) > 0 {
			versionPath := skill.StoragePath + "/" + sv.Version
			downloadURLs, _, _, dlErr := generateDownloadURLs(ctx, stor, versionPath, sv.Files)
			if dlErr == nil {
				if stor.Provider() == storage.ProviderLocal {
					hubURL := requestBaseURL(r)
					downloadURLs = rewriteLocalDownloadURLs(downloadURLs, hubURL, "skills", skill.ID)
				}
				entry.Files = downloadURLs
			}
		}

		go func(versionID string) {
			_ = s.store.IncrementSkillVersionDownloadCount(context.Background(), versionID)
		}(sv.ID)

		resolved = append(resolved, entry)
	}

	writeJSON(w, http.StatusOK, ResolveSkillsResponse{
		Resolved: resolved,
		Errors:   resolveErrors,
	})
}

// resolveSkill finds a skill and version by URI, searching scopes in priority order.
func (s *Server) resolveSkill(ctx context.Context, uri *api.SkillURI, projectID string) (*store.Skill, *store.SkillVersion, error) {
	scopes := determineScopeSearchOrder(uri, projectID)

	var versionErr error
	for _, sc := range scopes {
		// Skip scoped lookups that require a scopeID when none is available
		if sc.scopeID == "" && (sc.scope == store.SkillScopeProject || sc.scope == store.SkillScopeUser) {
			continue
		}

		skill, err := s.store.GetSkillBySlug(ctx, uri.Name, sc.scope, sc.scopeID)
		if err != nil {
			continue
		}

		sv, err := s.store.ResolveSkillVersion(ctx, skill.ID, uri.Version)
		if err != nil {
			versionErr = err
			continue
		}

		return skill, sv, nil
	}
	if versionErr != nil {
		return nil, nil, fmt.Errorf("skill %q found but version %q could not be resolved: %w", uri.Name, uri.Version, versionErr)
	}
	return nil, nil, fmt.Errorf("skill %q not found in any scope", uri.Name)
}

type scopeEntry struct {
	scope   string
	scopeID string
}

// determineScopeSearchOrder returns the scope search order for skill resolution.
func determineScopeSearchOrder(uri *api.SkillURI, projectID string) []scopeEntry {
	// If explicit scope is set, search only that scope.
	if uri.Scope != "" {
		return []scopeEntry{{scope: uri.Scope, scopeID: uri.ScopeID}}
	}

	// Default search order: user > project > global > core
	var order []scopeEntry
	if uri.ScopeID != "" {
		order = append(order, scopeEntry{scope: store.SkillScopeUser, scopeID: uri.ScopeID})
	}
	if projectID != "" {
		order = append(order, scopeEntry{scope: store.SkillScopeProject, scopeID: projectID})
	}
	order = append(order,
		scopeEntry{scope: store.SkillScopeGlobal},
		scopeEntry{scope: store.SkillScopeCore},
	)
	return order
}

// expandScopeAliases fills in scope IDs from request context.
func expandScopeAliases(uri *api.SkillURI, projectID, userID string) {
	if uri.Scope == store.SkillScopeProject && uri.ScopeID == "" && projectID != "" {
		uri.ScopeID = projectID
	}
	if uri.Scope == store.SkillScopeUser && uri.ScopeID == "" && userID != "" {
		uri.ScopeID = userID
	}
}

// skillResource constructs a Resource from a store.Skill for capability computation.
func skillResource(s *store.Skill) Resource {
	r := Resource{
		Type:    "skill",
		ID:      s.ID,
		OwnerID: s.OwnerID,
	}
	if s.Scope == store.SkillScopeProject && s.ScopeID != "" {
		r.ParentType = "project"
		r.ParentID = s.ScopeID
	}
	return r
}
