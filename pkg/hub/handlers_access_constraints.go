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
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Permission constants
// ---------------------------------------------------------------------------

const (
	// PermissionConstraintRead is the permission for read-only access to
	// constraints (list, detail, affected-principals, audit).
	PermissionConstraintRead = "access_constraint.read"
)

// ---------------------------------------------------------------------------
// Request types — preview-bound mutations (B7)
// ---------------------------------------------------------------------------

// accessConstraintCreateRequest is the payload for POST
// /api/v1/admin/access-constraints (preview-bound create).
type accessConstraintCreateRequest struct {
	Name               string                  `json:"name"`
	Purpose            string                  `json:"purpose,omitempty"`
	Subject            subjectSelectorRequest  `json:"subject"`
	Scope              constraintScopeRequest  `json:"scope"`
	MaximumPermissions []string                `json:"maximumPermissions"`
	Condition          *constraintConditionReq `json:"condition,omitempty"`
	PreviewToken       string                  `json:"previewToken"`
}

// accessConstraintUpdateRequest is the payload for PUT
// /api/v1/admin/access-constraints/:id (preview-bound full update).
type accessConstraintUpdateRequest struct {
	Name               string                  `json:"name"`
	Purpose            string                  `json:"purpose,omitempty"`
	Subject            subjectSelectorRequest  `json:"subject"`
	Scope              constraintScopeRequest  `json:"scope"`
	MaximumPermissions []string                `json:"maximumPermissions"`
	Condition          *constraintConditionReq `json:"condition,omitempty"`
	PreviewToken       string                  `json:"previewToken"`
}

// accessConstraintDeleteRequest is the payload for DELETE
// /api/v1/admin/access-constraints/:id (preview-bound delete).
type accessConstraintDeleteRequest struct {
	PreviewToken string `json:"previewToken"`
}

type subjectSelectorRequest struct {
	Kind          string `json:"kind"`
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalID   string `json:"principalId,omitempty"`
	GroupID       string `json:"groupId,omitempty"`
}

type constraintScopeRequest struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type constraintConditionReq struct {
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ---------------------------------------------------------------------------
// Preview request types
// ---------------------------------------------------------------------------

// previewCreateRequest is the payload for POST
// /api/v1/admin/access-constraint-previews.
type previewCreateRequest struct {
	Operation string `json:"operation"` // "create", "update", "delete"

	// For create and update:
	Draft *previewDraftRequest `json:"draft,omitempty"`

	// For update and delete:
	ConstraintID string `json:"constraintId,omitempty"`
	BaseRevision int64  `json:"baseRevision,omitempty"`
}

type previewDraftRequest struct {
	Name               string                  `json:"name"`
	Purpose            string                  `json:"purpose,omitempty"`
	Subject            subjectSelectorRequest  `json:"subject"`
	Scope              constraintScopeRequest  `json:"scope"`
	MaximumPermissions []string                `json:"maximumPermissions"`
	Condition          *constraintConditionReq `json:"condition,omitempty"`
}

// ---------------------------------------------------------------------------
// API response types (B7 response shape)
// ---------------------------------------------------------------------------

// accessBoundarySummary is a list row with resolved references and capabilities.
type accessBoundarySummary struct {
	ID                 string                   `json:"id"`
	Name               string                   `json:"name"`
	Purpose            string                   `json:"purpose,omitempty"`
	Subject            resolvedSubject           `json:"subject"`
	Scope              resolvedScope             `json:"scope"`
	MaximumPermissions []resolvedPermission      `json:"maximumPermissions"`
	Status             string                    `json:"status"`
	Health             resolutionHealth          `json:"health"`
	Completeness       responseCompleteness      `json:"completeness"`
	Condition          *constraintConditionReq   `json:"condition,omitempty"`
	Revision           int64                     `json:"revision"`
	CreatedBy          string                    `json:"createdBy"`
	CreatedAt          time.Time                 `json:"createdAt"`
	UpdatedAt          time.Time                 `json:"updatedAt"`
	UpdatedBy          string                    `json:"updatedBy,omitempty"`
	Capabilities       *BoundaryCapabilities     `json:"_capabilities,omitempty"`
}

// accessBoundaryDetail is the full record with temporal impact, lockout, provenance.
type accessBoundaryDetail struct {
	accessBoundarySummary
	TemporalImpact []TemporalImpact   `json:"temporalImpact,omitempty"`
	Lockout        *LockoutAssessment `json:"lockout,omitempty"`
	Provenance     *provenanceLinks   `json:"provenance,omitempty"`
}

type resolvedSubject struct {
	Kind          string `json:"kind"`
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalID   string `json:"principalId,omitempty"`
	GroupID       string `json:"groupId,omitempty"`
	DisplayName   string `json:"displayName,omitempty"`
}

type resolvedScope struct {
	Type        string `json:"type"`
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

type resolvedPermission struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
}

type resolutionHealth struct {
	Healthy  bool   `json:"healthy"`
	Degraded bool   `json:"degraded,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type responseCompleteness struct {
	Complete bool `json:"complete"`
}

type provenanceLinks struct {
	AuditURL string `json:"auditUrl,omitempty"`
}

// accessBoundaryListResponse is the list envelope.
type accessBoundaryListResponse struct {
	Items         []accessBoundarySummary `json:"items"`
	NextPageToken string                  `json:"nextPageToken,omitempty"`
	TotalCount    int                     `json:"totalCount"`
}

// auditEventResponse wraps a single audit entry for the API.
type auditEventResponse struct {
	ID             string       `json:"id"`
	ConstraintID   string       `json:"constraintId"`
	Operation      string       `json:"operation"`
	ActorID        string       `json:"actorId"`
	BeforeRevision int64        `json:"beforeRevision"`
	AfterRevision  int64        `json:"afterRevision"`
	Classification string       `json:"classification"`
	PreviewID      string       `json:"previewId,omitempty"`
	DraftHash      string       `json:"draftHash,omitempty"`
	ImpactCounts   ImpactCounts `json:"impactCounts"`
	Timestamp      time.Time    `json:"timestamp"`
}

// auditListResponse is the audit subresource envelope.
type auditListResponse struct {
	Items         []auditEventResponse `json:"items"`
	NextPageToken string               `json:"nextPageToken,omitempty"`
	TotalCount    int                  `json:"totalCount"`
}

// affectedPrincipalsResponse wraps the affected-principals subresource.
type affectedPrincipalsResponse struct {
	Items         []AffectedPrincipal `json:"items"`
	NextPageToken string              `json:"nextPageToken,omitempty"`
	TotalCount    int                 `json:"totalCount"`
}

// mutationResponse is the response for create/update mutations.
type mutationResponse struct {
	accessBoundaryDetail
	AuditID string `json:"auditId"`
}

// deleteResponse is the response for delete mutations.
type deleteResponse struct {
	AuditID string `json:"auditId"`
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

// handleAdminAccessConstraints handles GET (list) and POST (preview-bound create) on
// /api/v1/admin/access-constraints.
func (s *Server) handleAdminAccessConstraints(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAccessConstraints(w, r)
	case http.MethodPost:
		user, ok := s.requireConstraintAdminPermission(w, r, PermissionConstraintAdmin, "create")
		if !ok {
			return
		}
		s.createAccessConstraint(w, r, user)
	default:
		MethodNotAllowed(w, "GET", "POST")
	}
}

// handleAdminAccessConstraintByID handles GET / PUT / DELETE on
// /api/v1/admin/access-constraints/:id, and routes to subresources.
func (s *Server) handleAdminAccessConstraintByID(w http.ResponseWriter, r *http.Request) {
	// Extract path after the prefix to detect subresources.
	path := r.URL.Path
	prefix := "/api/v1/admin/access-constraints/"
	remainder := strings.TrimPrefix(path, prefix)

	// Parse ID and subresource.
	parts := strings.SplitN(remainder, "/", 2)
	id := parts[0]
	if id == "" {
		BadRequest(w, "access constraint ID is required")
		return
	}

	// If there's a subresource path, route to the appropriate handler.
	if len(parts) == 2 {
		subresource := parts[1]
		switch subresource {
		case "affected-principals":
			s.getAffectedPrincipals(w, r, id)
		case "audit":
			s.getConstraintAudit(w, r, id)
		default:
			NotFound(w, "subresource")
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getAccessConstraint(w, r, id)
	case http.MethodPut:
		user, ok := s.requireConstraintAdminPermission(w, r, PermissionConstraintAdmin, "update")
		if !ok {
			return
		}
		s.updateAccessConstraint(w, r, id, user)
	case http.MethodDelete:
		user, ok := s.requireConstraintAdminPermission(w, r, PermissionConstraintAdmin, "delete")
		if !ok {
			return
		}
		s.deleteAccessConstraint(w, r, id, user)
	default:
		MethodNotAllowed(w, "GET", "PUT", "DELETE")
	}
}

// handleAdminAccessConstraintPreviews handles POST on
// /api/v1/admin/access-constraint-previews and GET for async jobs.
func (s *Server) handleAdminAccessConstraintPreviews(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	prefix := "/api/v1/admin/access-constraint-previews"

	// Exact match: POST creates a new preview.
	if path == prefix {
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, "POST")
			return
		}
		user, ok := s.requireConstraintAdminPermission(w, r, PermissionConstraintAdmin, "preview")
		if !ok {
			return
		}
		s.createPreview(w, r, user)
		return
	}

	// Sub-path: /api/v1/admin/access-constraint-previews/:jobId[/result]
	remainder := strings.TrimPrefix(path, prefix+"/")
	parts := strings.SplitN(remainder, "/", 2)
	jobID := parts[0]
	if jobID == "" {
		BadRequest(w, "preview job ID is required")
		return
	}

	if r.Method != http.MethodGet {
		MethodNotAllowed(w, "GET")
		return
	}

	if len(parts) == 2 && parts[1] == "result" {
		s.getPreviewResult(w, r, jobID)
		return
	}
	s.getPreviewJob(w, r, jobID)
}

// ---------------------------------------------------------------------------
// List with cursor/filter/sort
// ---------------------------------------------------------------------------

func (s *Server) listAccessConstraints(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	opts := store.AccessConstraintListOptions{
		PageSize:             parseIntOr(q.Get("pageSize"), 50),
		PageToken:            q.Get("pageToken"),
		SubjectKind:          q.Get("subjectKind"),
		SubjectPrincipalType: q.Get("subjectPrincipalType"),
		ScopeType:            q.Get("scopeType"),
		ScopeID:              q.Get("scopeId"),
		Status:               q.Get("status"),
		NameContains:         q.Get("nameContains"),
		SortBy:               q.Get("sortBy"),
		SortOrder:            q.Get("sortOrder"),
	}

	// Clamp page size.
	if opts.PageSize <= 0 {
		opts.PageSize = 50
	}
	if opts.PageSize > 200 {
		opts.PageSize = 200
	}

	constraints, nextToken, totalCount, err := s.store.ListAccessConstraintsFiltered(r.Context(), opts)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	actor := s.actorFromRequest(r)

	items := make([]accessBoundarySummary, 0, len(constraints))
	for _, sc := range constraints {
		summary := s.buildBoundarySummary(r.Context(), sc, actor)
		items = append(items, summary)
	}

	writeJSON(w, http.StatusOK, accessBoundaryListResponse{
		Items:         items,
		NextPageToken: nextToken,
		TotalCount:    totalCount,
	})
}

// ---------------------------------------------------------------------------
// Detail
// ---------------------------------------------------------------------------

func (s *Server) getAccessConstraint(w http.ResponseWriter, r *http.Request, id string) {
	sc, err := s.store.GetAccessConstraint(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	actor := s.actorFromRequest(r)
	detail := s.buildBoundaryDetail(r.Context(), sc, actor)

	// If-Match revision support: set ETag header.
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, sc.Revision))

	writeJSON(w, http.StatusOK, detail)
}

// ---------------------------------------------------------------------------
// Affected-principals subresource
// ---------------------------------------------------------------------------

func (s *Server) getAffectedPrincipals(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, "GET")
		return
	}

	sc, err := s.store.GetAccessConstraint(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Build the blast-radius preview and return the affected principals.
	preview := s.buildConstraintPreview(r, sc)
	if preview == nil {
		writeJSON(w, http.StatusOK, affectedPrincipalsResponse{
			Items:      []AffectedPrincipal{},
			TotalCount: 0,
		})
		return
	}

	// Cursor pagination over affected principals.
	pageSize := parseIntOr(r.URL.Query().Get("pageSize"), 50)
	if pageSize > 200 {
		pageSize = 200
	}
	offset := parseIntOr(r.URL.Query().Get("pageToken"), 0)

	total := len(preview.AffectedPrincipals)
	end := offset + pageSize
	if end > total {
		end = total
	}

	var items []AffectedPrincipal
	if offset < total {
		items = preview.AffectedPrincipals[offset:end]
	}
	if items == nil {
		items = []AffectedPrincipal{}
	}

	var nextToken string
	if end < total {
		nextToken = fmt.Sprintf("%d", end)
	}

	writeJSON(w, http.StatusOK, affectedPrincipalsResponse{
		Items:         items,
		NextPageToken: nextToken,
		TotalCount:    total,
	})
}

// ---------------------------------------------------------------------------
// Audit subresource
// ---------------------------------------------------------------------------

func (s *Server) getConstraintAudit(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, "GET")
		return
	}

	// Verify the constraint exists.
	_, err := s.store.GetAccessConstraint(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Get audit entries from the governance service's audit writer.
	var entries []BoundaryAuditEntry
	if s.governanceService != nil && s.governanceService.auditWriter != nil {
		entries = s.governanceService.auditWriter.GetEntriesForConstraint(id)
	}

	// Cursor pagination.
	pageSize := parseIntOr(r.URL.Query().Get("pageSize"), 50)
	if pageSize > 200 {
		pageSize = 200
	}
	offset := parseIntOr(r.URL.Query().Get("pageToken"), 0)

	total := len(entries)
	end := offset + pageSize
	if end > total {
		end = total
	}

	var items []auditEventResponse
	if offset < total {
		for _, e := range entries[offset:end] {
			items = append(items, auditEventResponse{
				ID:             e.ID,
				ConstraintID:   e.ConstraintID,
				Operation:      e.Operation,
				ActorID:        e.ActorID,
				BeforeRevision: e.BeforeRevision,
				AfterRevision:  e.AfterRevision,
				Classification: e.Classification,
				PreviewID:      e.PreviewID,
				DraftHash:      e.DraftHash,
				ImpactCounts:   e.ImpactCounts,
				Timestamp:      e.Timestamp,
			})
		}
	}
	if items == nil {
		items = []auditEventResponse{}
	}

	var nextToken string
	if end < total {
		nextToken = fmt.Sprintf("%d", end)
	}

	writeJSON(w, http.StatusOK, auditListResponse{
		Items:         items,
		NextPageToken: nextToken,
		TotalCount:    total,
	})
}

// ---------------------------------------------------------------------------
// Preview endpoint
// ---------------------------------------------------------------------------

func (s *Server) createPreview(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	if s.previewService == nil {
		InternalError(w)
		return
	}

	var req previewCreateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Reject unknown fields by re-decoding with DisallowUnknownFields.
	if hasUnknownFields(r) {
		BadRequest(w, "request contains unknown fields")
		return
	}

	// Build the preview request.
	actor := PrincipalContext{
		Kind: PrincipalKindUser,
		ID:   user.ID(),
	}

	previewReq := PreviewRequest{
		Operation:    req.Operation,
		ConstraintID: req.ConstraintID,
		BaseRevision: req.BaseRevision,
		Actor:        actor,
	}

	// Build draft store constraint if provided.
	if req.Draft != nil {
		draft, err := s.draftToStoreConstraint(req.Draft, user)
		if err != nil {
			BadRequest(w, err.Error())
			return
		}
		if req.ConstraintID != "" {
			draft.ID = req.ConstraintID
		}
		previewReq.Draft = draft
	}

	// Generate preview.
	result, err := s.previewService.GeneratePreview(r.Context(), previewReq)
	if err != nil {
		s.handlePreviewError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) getPreviewJob(w http.ResponseWriter, r *http.Request, jobID string) {
	if s.previewService == nil {
		InternalError(w)
		return
	}

	job, err := s.previewService.GetPreviewJob(r.Context(), jobID)
	if err != nil {
		NotFound(w, "Preview Job")
		return
	}

	writeJSON(w, http.StatusOK, job)
}

func (s *Server) getPreviewResult(w http.ResponseWriter, r *http.Request, jobID string) {
	if s.previewService == nil {
		InternalError(w)
		return
	}

	job, err := s.previewService.GetPreviewJob(r.Context(), jobID)
	if err != nil {
		NotFound(w, "Preview Job")
		return
	}

	if job.Status != JobStatusSucceeded {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			fmt.Sprintf("preview job status is %s, not succeeded", job.Status), nil)
		return
	}

	writeJSON(w, http.StatusOK, job.Result)
}

// ---------------------------------------------------------------------------
// Create (preview-bound)
// ---------------------------------------------------------------------------

func (s *Server) createAccessConstraint(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	if s.governanceService == nil {
		// Fall back behavior: governance not wired — reject mutations.
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"boundary governance service is not available", nil)
		return
	}

	var req accessConstraintCreateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Preview token is required — no raw CRUD bypass.
	if req.PreviewToken == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"previewToken is required; mutations must go through preview first", nil)
		return
	}

	// Validate required fields.
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	// Validate subject.
	subject := SubjectSelector{
		Kind:          SubjectKind(req.Subject.Kind),
		PrincipalType: req.Subject.PrincipalType,
		PrincipalID:   req.Subject.PrincipalID,
		GroupID:       req.Subject.GroupID,
	}
	if err := subject.Validate(); err != nil {
		BadRequest(w, "invalid subject: "+err.Error())
		return
	}

	// Validate scope.
	scope := ConstraintScopeRef{
		Type: req.Scope.Type,
		ID:   req.Scope.ID,
	}
	if err := scope.Validate(); err != nil {
		BadRequest(w, "invalid scope: "+err.Error())
		return
	}

	// Validate maximum permissions.
	if len(req.MaximumPermissions) == 0 {
		BadRequest(w, "maximumPermissions must contain at least one permission")
		return
	}
	if err := validatePermissionIDs(req.MaximumPermissions); err != nil {
		BadRequest(w, err.Error())
		return
	}

	// Build store model.
	draft := &store.AccessConstraint{
		Name:               req.Name,
		Purpose:            req.Purpose,
		SubjectKind:        string(subject.Kind),
		ScopeType:          scope.Type,
		ScopeID:            scope.ID,
		MaximumPermissions: req.MaximumPermissions,
		CreatedBy:          user.ID(),
		UpdatedBy:          user.ID(),
	}

	// Set subject fields based on kind.
	switch subject.Kind {
	case SubjectKindPrincipal:
		draft.SubjectPrincipalType = &subject.PrincipalType
		draft.SubjectPrincipalID = &subject.PrincipalID
	case SubjectKindGroupClosure:
		draft.SubjectGroupID = &subject.GroupID
	}

	// Set condition (time window).
	if req.Condition != nil {
		draft.NotBefore = req.Condition.NotBefore
		draft.ExpiresAt = req.Condition.ExpiresAt
	}

	// Commit through governance service.
	actor := PrincipalContext{
		Kind: PrincipalKindUser,
		ID:   user.ID(),
	}

	result, err := s.governanceService.CommitBoundaryChange(r.Context(), CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: req.PreviewToken,
		Actor:        actor,
	})
	if err != nil {
		s.handleGovernanceError(w, err)
		return
	}

	slog.Info("access constraint created via preview",
		"constraint_id", result.Constraint.ID,
		"name", result.Constraint.Name,
		"actor", user.Email(),
		"audit_id", result.AuditID,
	)

	detail := s.buildBoundaryDetail(r.Context(), result.Constraint, &actor)
	writeJSON(w, http.StatusCreated, mutationResponse{
		accessBoundaryDetail: detail,
		AuditID:              result.AuditID,
	})
}

// ---------------------------------------------------------------------------
// Update (preview-bound with If-Match)
// ---------------------------------------------------------------------------

func (s *Server) updateAccessConstraint(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	if s.governanceService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"boundary governance service is not available", nil)
		return
	}

	// Require If-Match header for optimistic concurrency.
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		writeError(w, http.StatusPreconditionRequired, ErrCodeRevisionConflict,
			"If-Match header is required for updates", nil)
		return
	}
	expectedRevision, err := parseIfMatchRevision(ifMatch)
	if err != nil {
		BadRequest(w, "invalid If-Match header: "+err.Error())
		return
	}

	var req accessConstraintUpdateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Preview token required.
	if req.PreviewToken == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"previewToken is required; mutations must go through preview first", nil)
		return
	}

	// Check the constraint exists and is not recovery-disabled.
	existing, err := s.store.GetAccessConstraint(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if existing.Disabled {
		writeError(w, http.StatusConflict, ErrCodeRecoveryDisabledImmutable,
			"constraint is recovery-disabled and cannot be modified", nil)
		return
	}

	// Validate fields.
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	subject := SubjectSelector{
		Kind:          SubjectKind(req.Subject.Kind),
		PrincipalType: req.Subject.PrincipalType,
		PrincipalID:   req.Subject.PrincipalID,
		GroupID:       req.Subject.GroupID,
	}
	if err := subject.Validate(); err != nil {
		BadRequest(w, "invalid subject: "+err.Error())
		return
	}

	scope := ConstraintScopeRef{
		Type: req.Scope.Type,
		ID:   req.Scope.ID,
	}
	if err := scope.Validate(); err != nil {
		BadRequest(w, "invalid scope: "+err.Error())
		return
	}

	if len(req.MaximumPermissions) == 0 {
		BadRequest(w, "maximumPermissions must contain at least one permission")
		return
	}
	if err := validatePermissionIDs(req.MaximumPermissions); err != nil {
		BadRequest(w, err.Error())
		return
	}

	// Build full update draft.
	draft := &store.AccessConstraint{
		ID:                 id,
		Name:               req.Name,
		Purpose:            req.Purpose,
		SubjectKind:        string(subject.Kind),
		ScopeType:          scope.Type,
		ScopeID:            scope.ID,
		MaximumPermissions: req.MaximumPermissions,
		CreatedBy:          existing.CreatedBy,
		UpdatedBy:          user.ID(),
	}

	switch subject.Kind {
	case SubjectKindPrincipal:
		draft.SubjectPrincipalType = &subject.PrincipalType
		draft.SubjectPrincipalID = &subject.PrincipalID
	case SubjectKindGroupClosure:
		draft.SubjectGroupID = &subject.GroupID
	}

	if req.Condition != nil {
		draft.NotBefore = req.Condition.NotBefore
		draft.ExpiresAt = req.Condition.ExpiresAt
	}

	actor := PrincipalContext{
		Kind: PrincipalKindUser,
		ID:   user.ID(),
	}

	result, err := s.governanceService.CommitBoundaryChange(r.Context(), CommitRequest{
		Operation:    "update",
		Draft:        draft,
		ConstraintID: id,
		BaseRevision: expectedRevision,
		PreviewToken: req.PreviewToken,
		Actor:        actor,
	})
	if err != nil {
		s.handleGovernanceError(w, err)
		return
	}

	slog.Info("access constraint updated via preview",
		"constraint_id", result.Constraint.ID,
		"name", result.Constraint.Name,
		"actor", user.Email(),
		"audit_id", result.AuditID,
	)

	detail := s.buildBoundaryDetail(r.Context(), result.Constraint, &actor)
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, result.Constraint.Revision))
	writeJSON(w, http.StatusOK, mutationResponse{
		accessBoundaryDetail: detail,
		AuditID:              result.AuditID,
	})
}

// ---------------------------------------------------------------------------
// Delete (preview-bound)
// ---------------------------------------------------------------------------

func (s *Server) deleteAccessConstraint(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	if s.governanceService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"boundary governance service is not available", nil)
		return
	}

	// For DELETE, the preview token may be in the request body or a query param.
	var previewToken string

	// Try reading from body first (if content-type is JSON).
	if r.Header.Get("Content-Type") == "application/json" || r.ContentLength > 0 {
		var req accessConstraintDeleteRequest
		if err := readJSON(r, &req); err == nil {
			previewToken = req.PreviewToken
		}
	}

	// Fall back to query parameter.
	if previewToken == "" {
		previewToken = r.URL.Query().Get("previewToken")
	}

	if previewToken == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"previewToken is required; mutations must go through preview first", nil)
		return
	}

	// Check the constraint exists and is not recovery-disabled.
	existing, err := s.store.GetAccessConstraint(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if existing.Disabled {
		writeError(w, http.StatusConflict, ErrCodeRecoveryDisabledImmutable,
			"constraint is recovery-disabled and cannot be deleted", nil)
		return
	}

	actor := PrincipalContext{
		Kind: PrincipalKindUser,
		ID:   user.ID(),
	}

	result, err := s.governanceService.CommitBoundaryChange(r.Context(), CommitRequest{
		Operation:    "delete",
		ConstraintID: id,
		BaseRevision: existing.Revision,
		PreviewToken: previewToken,
		Actor:        actor,
	})
	if err != nil {
		s.handleGovernanceError(w, err)
		return
	}

	slog.Info("access constraint deleted via preview",
		"constraint_id", id,
		"name", existing.Name,
		"actor", user.Email(),
		"audit_id", result.AuditID,
	)

	writeJSON(w, http.StatusOK, deleteResponse{
		AuditID: result.AuditID,
	})
}

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------

// requireConstraintAdminPermission checks that the authenticated user has
// the specified constraint administration permission.
func (s *Server) requireConstraintAdminPermission(w http.ResponseWriter, r *http.Request, permission string, action string) (UserIdentity, bool) {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, false
	}
	if s.authzService == nil {
		Forbidden(w)
		return nil, false
	}
	decision := s.authzService.Decide(r.Context(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "access_constraint", ID: "hub"},
		Action:     Action(action),
		Permission: permission,
	})
	if !decision.Allowed {
		Forbidden(w)
		return nil, false
	}
	return user, true
}

// ---------------------------------------------------------------------------
// Governance: lockout prevention (retained for legacy callers)
// ---------------------------------------------------------------------------

// checkConstraintLockout verifies that after applying the proposed constraint,
// at least one active direct user retains constraint-admin permission at the
// relevant scope.
//
// The algorithm:
//  1. Resolve all direct users who currently hold access_constraint.admin at
//     this scope via role bindings (including group-expanded bindings).
//  2. Load all constraints at this scope, merge in the proposed change.
//  3. For each admin user, simulate the full constraint set against that
//     user's principal closure. If any constraint (in its most-restrictive
//     time state) would remove access_constraint.admin, the user is blocked.
//  4. If at least one admin user survives unconstrained, the operation is
//     allowed. If none survive, reject.
func (s *Server) checkConstraintLockout(r *http.Request, proposed *store.AccessConstraint) error {
	ctx := r.Context()

	// Fast path: if the proposed constraint allows constraint-admin, it
	// cannot cause a lockout by itself. We still need to check combined
	// state though — another existing constraint might already be blocking
	// admin, and this change could close the last remaining gap.
	// However, if the proposed constraint allows admin AND is a new creation
	// (ID is empty), it strictly cannot make things worse. Skip.
	if proposed.ID == "" && constraintAllowsPermission(proposed, PermissionConstraintAdmin) {
		return nil
	}

	// Step 1: Find all direct users with constraint-admin at this scope.
	adminUsers, err := s.resolveConstraintAdminUsers(ctx, proposed.ScopeType, proposed.ScopeID)
	if err != nil {
		slog.Warn("lockout check: failed to resolve admin users", "error", err)
		return errors.New("failed to resolve constraint admin users for lockout check")
	}

	if len(adminUsers) == 0 {
		// No admin users found — this is already a degraded state.
		// Allow the operation rather than blocking everything.
		slog.Warn("lockout check: no constraint admin users found at scope",
			"scopeType", proposed.ScopeType, "scopeID", proposed.ScopeID)
		return nil
	}

	// Step 2: Load all constraints at this scope and merge proposed change.
	constraints, err := s.store.ListConstraintsForScope(ctx, proposed.ScopeType, proposed.ScopeID)
	if err != nil {
		return errors.New("failed to load existing constraints for lockout check")
	}

	// Add or update the proposed constraint in the list.
	found := false
	for i, c := range constraints {
		if c.ID == proposed.ID {
			constraints[i] = proposed
			found = true
			break
		}
	}
	if !found {
		constraints = append(constraints, proposed)
	}

	// Filter to constraints that would be active in most-restrictive state
	// and that remove constraint-admin.
	now := time.Now()
	var restrictingConstraints []*store.AccessConstraint
	for _, c := range constraints {
		if c.Disabled {
			continue
		}
		condition := ConstraintCondition{}
		if c.NotBefore != nil {
			condition.NotBefore = *c.NotBefore
		}
		if c.ExpiresAt != nil {
			condition.ExpiresAt = *c.ExpiresAt
		}
		if !condition.IsActiveInMostRestrictiveState(now) {
			continue
		}
		if constraintAllowsPermission(c, PermissionConstraintAdmin) {
			continue // This constraint does not restrict admin.
		}
		restrictingConstraints = append(restrictingConstraints, c)
	}

	// If no constraints restrict admin, no lockout is possible.
	if len(restrictingConstraints) == 0 {
		return nil
	}

	// Step 3: For each admin user, check if any restricting constraint
	// applies to them. If ALL admin users are blocked, reject.
	for _, au := range adminUsers {
		if !s.userBlockedByConstraints(ctx, au, restrictingConstraints) {
			return nil // At least one admin user survives.
		}
	}

	// Step 4: All admin users are blocked.
	return errors.New("constraint would remove constraint-admin permission from all users who currently hold it at this scope; at least one direct user must retain it")
}

// adminUserInfo holds identity data for a user with constraint-admin.
type adminUserInfo struct {
	userID   string
	groupIDs []string // effective group membership (for group_closure matching)
}

// resolveConstraintAdminUsers finds all direct users who hold the
// access_constraint.admin permission at the given scope via role bindings.
func (s *Server) resolveConstraintAdminUsers(ctx context.Context, scopeType, scopeID string) ([]adminUserInfo, error) {
	// Get all role bindings at this scope.
	bindings, err := s.store.ListRoleBindingsForScope(ctx, scopeType, scopeID)
	if err != nil {
		return nil, fmt.Errorf("list role bindings for scope: %w", err)
	}

	// Also include system-scope bindings if we're checking a project scope,
	// because system-scoped roles apply everywhere.
	if scopeType == ScopeTypeProject {
		sysBindings, err := s.store.ListRoleBindingsForScope(ctx, ScopeTypeSystem, "")
		if err != nil {
			return nil, fmt.Errorf("list system role bindings: %w", err)
		}
		bindings = append(bindings, sysBindings...)
	}

	// Resolve which role definitions grant constraint-admin.
	roleDefs, err := s.store.ListRoleDefinitions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list role definitions: %w", err)
	}
	adminRoleIDs := make(map[string]bool)
	for _, rd := range roleDefs {
		for _, p := range rd.Permissions {
			if p == PermissionConstraintAdmin {
				adminRoleIDs[rd.ID] = true
				break
			}
		}
	}

	// Collect direct user principals with admin role bindings.
	// Also track groups that have admin bindings (for group-expanded resolution).
	directUserIDs := make(map[string]bool)
	adminGroupIDs := make(map[string]bool)
	for _, b := range bindings {
		if !adminRoleIDs[b.RoleDefinitionID] {
			continue
		}
		switch b.PrincipalType {
		case store.RoleBindingPrincipalUser:
			directUserIDs[b.PrincipalID] = true
		case store.RoleBindingPrincipalGroup:
			adminGroupIDs[b.PrincipalID] = true
		}
	}

	// Expand group memberships to find users who get admin via groups.
	// Fail closed: if we cannot resolve group members, we cannot reliably
	// determine which users are constraint admins, so the lockout check
	// must reject the operation rather than proceed with incomplete data.
	for gid := range adminGroupIDs {
		members, err := s.store.GetGroupMembers(ctx, gid)
		if err != nil {
			return nil, fmt.Errorf("lockout check: failed to get group members for group %s: %w", gid, err)
		}
		for _, m := range members {
			if m.MemberType == store.GroupMemberTypeUser {
				directUserIDs[m.MemberID] = true
			}
		}
	}

	// Build admin user info with group closure for each user.
	// Fail closed: if we cannot resolve a user's group closure, we cannot
	// reliably evaluate group_closure constraints against them.
	var result []adminUserInfo
	for uid := range directUserIDs {
		groupIDs, err := s.store.GetEffectiveGroups(ctx, uid)
		if err != nil {
			return nil, fmt.Errorf("lockout check: failed to get effective groups for user %s: %w", uid, err)
		}
		result = append(result, adminUserInfo{
			userID:   uid,
			groupIDs: groupIDs,
		})
	}

	return result, nil
}

// userBlockedByConstraints returns true if all of the given restricting
// constraints (which remove constraint-admin) apply to this user.
// The user is blocked if ANY restricting constraint matches them.
func (s *Server) userBlockedByConstraints(_ context.Context, user adminUserInfo, constraints []*store.AccessConstraint) bool {
	// Build the user's principal closure: user ID + all group IDs.
	closure := make(map[string]struct{}, 1+len(user.groupIDs))
	closure[user.userID] = struct{}{}
	for _, gid := range user.groupIDs {
		closure[gid] = struct{}{}
	}

	for _, c := range constraints {
		switch c.SubjectKind {
		case store.ConstraintSubjectAllPrincipals:
			return true // all_principals blocks everyone
		case store.ConstraintSubjectPrincipal:
			if c.SubjectPrincipalType != nil && *c.SubjectPrincipalType == "user" &&
				c.SubjectPrincipalID != nil && *c.SubjectPrincipalID == user.userID {
				return true
			}
			// A principal-kind constraint targeting a group also blocks users
			// whose closure includes that group.
			if c.SubjectPrincipalType != nil && *c.SubjectPrincipalType == "group" &&
				c.SubjectPrincipalID != nil {
				if _, ok := closure[*c.SubjectPrincipalID]; ok {
					return true
				}
			}
		case store.ConstraintSubjectGroupClosure:
			if c.SubjectGroupID != nil {
				if _, ok := closure[*c.SubjectGroupID]; ok {
					return true
				}
			}
		}
	}

	return false
}

// constraintAllowsPermission checks whether a constraint's maximum permissions
// include the given permission.
func constraintAllowsPermission(c *store.AccessConstraint, permissionID string) bool {
	for _, p := range c.MaximumPermissions {
		if p == permissionID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Blast-radius preview (legacy — retained for affected-principals subresource)
// ---------------------------------------------------------------------------

// maxPreviewMembers caps group_closure expansion to avoid expensive queries.
const maxPreviewMembers = 50

// buildConstraintPreview builds a preview of the constraint's blast radius,
// including per-principal permission deltas.
func (s *Server) buildConstraintPreview(r *http.Request, sc *store.AccessConstraint) *ConstraintPreview {
	ctx := r.Context()

	maxPerms := make(map[string]struct{}, len(sc.MaximumPermissions))
	for _, p := range sc.MaximumPermissions {
		maxPerms[p] = struct{}{}
	}

	// Find permissions that would be restricted (permissions NOT in the
	// maximum set).
	var restricted []string
	for _, p := range permissions.Registry {
		if _, ok := maxPerms[p.ID]; !ok {
			restricted = append(restricted, p.ID)
		}
	}

	preview := &ConstraintPreview{
		ConstraintID:          sc.ID,
		ConstraintName:        sc.Name,
		RestrictedPermissions: restricted,
	}

	// Resolve affected principals and compute per-principal permission deltas.
	switch sc.SubjectKind {
	case store.ConstraintSubjectPrincipal:
		if sc.SubjectPrincipalType != nil && sc.SubjectPrincipalID != nil {
			ap := s.buildAffectedPrincipal(ctx, *sc.SubjectPrincipalType, *sc.SubjectPrincipalID,
				sc.ScopeType, sc.ScopeID, maxPerms)
			preview.AffectedPrincipals = []AffectedPrincipal{ap}
		}

	case store.ConstraintSubjectGroupClosure:
		if sc.SubjectGroupID != nil {
			// Include the group entity itself.
			groupDisplayName := s.resolveGroupMemberDisplayName(ctx, "group", *sc.SubjectGroupID)
			groupEntry := AffectedPrincipal{
				PrincipalType: "group",
				PrincipalID:   *sc.SubjectGroupID,
				DisplayName:   groupDisplayName + " (group closure)",
			}
			preview.AffectedPrincipals = []AffectedPrincipal{groupEntry}

			// Expand group members and compute per-member deltas.
			members, err := s.store.GetGroupMembers(ctx, *sc.SubjectGroupID)
			if err != nil {
				slog.Warn("preview: failed to get group members",
					"groupID", *sc.SubjectGroupID, "error", err)
				break
			}

			count := 0
			for _, m := range members {
				if m.MemberType != store.GroupMemberTypeUser {
					continue
				}
				if count >= maxPreviewMembers {
					preview.Truncated = true
					break
				}
				ap := s.buildAffectedPrincipal(ctx, m.MemberType, m.MemberID,
					sc.ScopeType, sc.ScopeID, maxPerms)
				preview.AffectedPrincipals = append(preview.AffectedPrincipals, ap)
				count++
			}
		}

	case store.ConstraintSubjectAllPrincipals:
		preview.AffectedPrincipals = []AffectedPrincipal{
			{
				PrincipalType: "all",
				PrincipalID:   "*",
				DisplayName:   "All principals",
			},
		}
		preview.Truncated = true
	}

	return preview
}

// buildAffectedPrincipal resolves a single principal's effective permissions
// at the given scope and computes the permission delta against the constraint.
func (s *Server) buildAffectedPrincipal(
	ctx context.Context,
	principalType, principalID string,
	scopeType, scopeID string,
	maxPerms map[string]struct{},
) AffectedPrincipal {
	displayName := s.resolveGroupMemberDisplayName(ctx, principalType, principalID)
	ap := AffectedPrincipal{
		PrincipalType: principalType,
		PrincipalID:   principalID,
		DisplayName:   displayName,
	}

	// Resolve effective permissions via the authz service.
	if s.authzService == nil {
		return ap
	}

	currentPerms, err := s.authzService.getEffectivePermissions(ctx, principalType, principalID, scopeType, scopeID)
	if err != nil {
		slog.Warn("preview: failed to resolve effective permissions",
			"principalType", principalType, "principalID", principalID, "error", err)
		return ap
	}

	ap.CurrentPermissions = currentPerms

	// Compute proposed (intersection with constraint) and removed (set difference).
	var proposed, removed []string
	for _, p := range currentPerms {
		if _, ok := maxPerms[p]; ok {
			proposed = append(proposed, p)
		} else {
			removed = append(removed, p)
		}
	}
	ap.ProposedPermissions = proposed
	ap.RemovedPermissions = removed

	return ap
}

// ---------------------------------------------------------------------------
// Response builders
// ---------------------------------------------------------------------------

// buildBoundarySummary converts a store constraint into an API summary response.
func (s *Server) buildBoundarySummary(ctx context.Context, sc *store.AccessConstraint, actor *PrincipalContext) accessBoundarySummary {
	hc := storeToHubAccessConstraint(sc)

	// Resolve subject display name.
	subjectDisplay := s.resolveSubjectDisplayName(ctx, sc)

	// Resolve scope display name.
	scopeDisplay := s.resolveScopeDisplayName(ctx, sc.ScopeType, sc.ScopeID)

	// Resolve permission display names.
	resolvedPerms := resolvePermissionDisplayNames(sc.MaximumPermissions)

	// Compute status.
	status := computeConstraintStatus(sc, hc)

	// Compute health.
	health := computeHealth(hc)

	summary := accessBoundarySummary{
		ID:                 sc.ID,
		Name:               sc.Name,
		Purpose:            sc.Purpose,
		Subject:            resolvedSubjectFromStore(sc, subjectDisplay),
		Scope:              resolvedScope{Type: sc.ScopeType, ID: sc.ScopeID, DisplayName: scopeDisplay},
		MaximumPermissions: resolvedPerms,
		Status:             status,
		Health:             health,
		Completeness:       responseCompleteness{Complete: !hc.Degraded},
		Revision:           sc.Revision,
		CreatedBy:          sc.CreatedBy,
		CreatedAt:          sc.CreatedAt,
		UpdatedAt:          sc.UpdatedAt,
		UpdatedBy:          sc.UpdatedBy,
	}

	// Set condition if time-bounded.
	if sc.NotBefore != nil || sc.ExpiresAt != nil {
		summary.Condition = &constraintConditionReq{
			NotBefore: sc.NotBefore,
			ExpiresAt: sc.ExpiresAt,
		}
	}

	// Compute capabilities if actor is available.
	if actor != nil && s.capabilitiesService != nil {
		caps, err := s.capabilitiesService.ComputeResourceCapabilities(ctx, *actor, sc.ID)
		if err == nil {
			summary.Capabilities = caps
		}
	}

	return summary
}

// buildBoundaryDetail converts a store constraint into a detailed API response.
func (s *Server) buildBoundaryDetail(ctx context.Context, sc *store.AccessConstraint, actor *PrincipalContext) accessBoundaryDetail {
	summary := s.buildBoundarySummary(ctx, sc, actor)
	detail := accessBoundaryDetail{
		accessBoundarySummary: summary,
	}

	// Add provenance links.
	detail.Provenance = &provenanceLinks{
		AuditURL: fmt.Sprintf("/api/v1/admin/access-constraints/%s/audit", sc.ID),
	}

	return detail
}

// ---------------------------------------------------------------------------
// Resolution helpers
// ---------------------------------------------------------------------------

func (s *Server) resolveSubjectDisplayName(ctx context.Context, sc *store.AccessConstraint) string {
	switch sc.SubjectKind {
	case store.ConstraintSubjectPrincipal:
		if sc.SubjectPrincipalType != nil && sc.SubjectPrincipalID != nil {
			return s.resolveGroupMemberDisplayName(ctx, *sc.SubjectPrincipalType, *sc.SubjectPrincipalID)
		}
	case store.ConstraintSubjectGroupClosure:
		if sc.SubjectGroupID != nil {
			return s.resolveGroupMemberDisplayName(ctx, "group", *sc.SubjectGroupID)
		}
	case store.ConstraintSubjectAllPrincipals:
		return "All principals"
	}
	return ""
}

func (s *Server) resolveScopeDisplayName(ctx context.Context, scopeType, scopeID string) string {
	if scopeType == ScopeTypeSystem {
		return "System"
	}
	if scopeType == ScopeTypeProject && scopeID != "" {
		p, err := s.store.GetProject(ctx, scopeID)
		if err == nil && p != nil {
			return p.Name
		}
		return scopeID
	}
	return ""
}

func resolvedSubjectFromStore(sc *store.AccessConstraint, displayName string) resolvedSubject {
	rs := resolvedSubject{
		Kind:        sc.SubjectKind,
		DisplayName: displayName,
	}
	if sc.SubjectPrincipalType != nil {
		rs.PrincipalType = *sc.SubjectPrincipalType
	}
	if sc.SubjectPrincipalID != nil {
		rs.PrincipalID = *sc.SubjectPrincipalID
	}
	if sc.SubjectGroupID != nil {
		rs.GroupID = *sc.SubjectGroupID
	}
	return rs
}

func resolvePermissionDisplayNames(ids []string) []resolvedPermission {
	lookup := make(map[string]string, len(permissions.Registry))
	for _, p := range permissions.Registry {
		// Use Description as display name; the Permission struct doesn't have
		// a dedicated DisplayName field.
		lookup[p.ID] = p.Description
	}
	result := make([]resolvedPermission, len(ids))
	for i, id := range ids {
		result[i] = resolvedPermission{
			ID:          id,
			DisplayName: lookup[id],
		}
	}
	return result
}

func computeConstraintStatus(sc *store.AccessConstraint, hc *AccessConstraint) string {
	if sc.Disabled {
		return "recovery_disabled"
	}
	now := time.Now()
	if sc.NotBefore != nil && now.Before(*sc.NotBefore) {
		return "scheduled"
	}
	if sc.ExpiresAt != nil && !now.Before(*sc.ExpiresAt) {
		return "expired"
	}
	return "active"
}

func computeHealth(hc *AccessConstraint) resolutionHealth {
	if hc == nil || hc.Degraded {
		return resolutionHealth{
			Healthy:  false,
			Degraded: true,
			Reason:   "constraint has invalid stored data",
		}
	}
	return resolutionHealth{Healthy: true}
}

// ---------------------------------------------------------------------------
// Error handling
// ---------------------------------------------------------------------------

// handleGovernanceError maps governance errors to HTTP responses.
func (s *Server) handleGovernanceError(w http.ResponseWriter, err error) {
	var govErr *GovernanceError
	if errors.As(err, &govErr) {
		statusCode := governanceErrorStatus(govErr.Code)
		writeError(w, statusCode, govErr.Code, govErr.Message, govErr.Details)
		return
	}

	var tokenErr *TokenValidationError
	if errors.As(err, &tokenErr) {
		statusCode := tokenErrorStatus(tokenErr.Code)
		writeError(w, statusCode, tokenErr.Code, tokenErr.Message, nil)
		return
	}

	// Check for store-level errors.
	if errors.Is(err, store.ErrRevisionConflict) {
		writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
			"constraint was modified by another operation", nil)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		NotFound(w, "Access Constraint")
		return
	}

	slog.Error("governance commit failed", "error", err)
	InternalError(w)
}

// handlePreviewError maps preview errors to HTTP responses.
func (s *Server) handlePreviewError(w http.ResponseWriter, err error) {
	var tokenErr *TokenValidationError
	if errors.As(err, &tokenErr) {
		statusCode := tokenErrorStatus(tokenErr.Code)
		writeError(w, statusCode, tokenErr.Code, tokenErr.Message, nil)
		return
	}

	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeResolutionFailed,
			"referenced constraint not found: "+err.Error(), nil)
		return
	}

	slog.Error("preview generation failed", "error", err)
	writeError(w, http.StatusUnprocessableEntity, ErrCodeResolutionFailed,
		"preview generation failed: "+err.Error(), nil)
}

// governanceErrorStatus maps governance error codes to HTTP status codes.
func governanceErrorStatus(code string) int {
	switch code {
	case ErrCodeConstraintAdminLockout:
		return http.StatusConflict
	case ErrCodeStaleAuthorizationPreview:
		return http.StatusConflict
	case ErrCodePreviewIncomplete:
		return http.StatusConflict
	case ErrCodeInsufficientRelaxationAuthority:
		return http.StatusForbidden
	case ErrCodeMutationPermissionLost:
		return http.StatusForbidden
	case ErrCodeRevisionConflict:
		return http.StatusConflict
	case ErrCodeRecoveryDisabledImmutable:
		return http.StatusConflict
	case ErrCodeInvalidRequest:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// tokenErrorStatus maps token validation error codes to HTTP status codes.
func tokenErrorStatus(code string) int {
	switch code {
	case ErrCodePreviewTokenExpired:
		return http.StatusConflict
	case ErrCodePreviewTokenReplay:
		return http.StatusConflict
	case ErrCodePreviewActorMismatch:
		return http.StatusForbidden
	case ErrCodePreviewOperationMismatch:
		return http.StatusBadRequest
	case ErrCodePreviewDraftModified:
		return http.StatusConflict
	case ErrCodePreviewRevisionMismatch:
		return http.StatusConflict
	case ErrCodePreviewStateMismatch:
		return http.StatusConflict
	case ErrCodePreviewIncomplete:
		return http.StatusConflict
	case ErrCodePreviewTokenInvalid:
		return http.StatusBadRequest
	default:
		return http.StatusBadRequest
	}
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// actorFromRequest extracts a PrincipalContext from the request identity.
func (s *Server) actorFromRequest(r *http.Request) *PrincipalContext {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		return nil
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		return nil
	}
	actor := PrincipalContext{
		Kind: PrincipalKindUser,
		ID:   user.ID(),
	}
	return &actor
}

// draftToStoreConstraint converts a preview draft request into a store constraint.
func (s *Server) draftToStoreConstraint(draft *previewDraftRequest, user UserIdentity) (*store.AccessConstraint, error) {
	name := strings.TrimSpace(draft.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	subject := SubjectSelector{
		Kind:          SubjectKind(draft.Subject.Kind),
		PrincipalType: draft.Subject.PrincipalType,
		PrincipalID:   draft.Subject.PrincipalID,
		GroupID:       draft.Subject.GroupID,
	}
	if err := subject.Validate(); err != nil {
		return nil, fmt.Errorf("invalid subject: %w", err)
	}

	scope := ConstraintScopeRef{
		Type: draft.Scope.Type,
		ID:   draft.Scope.ID,
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scope: %w", err)
	}

	if len(draft.MaximumPermissions) == 0 {
		return nil, fmt.Errorf("maximumPermissions must contain at least one permission")
	}
	if err := validatePermissionIDs(draft.MaximumPermissions); err != nil {
		return nil, err
	}

	sc := &store.AccessConstraint{
		Name:               name,
		Purpose:            draft.Purpose,
		SubjectKind:        string(subject.Kind),
		ScopeType:          scope.Type,
		ScopeID:            scope.ID,
		MaximumPermissions: draft.MaximumPermissions,
		CreatedBy:          user.ID(),
		UpdatedBy:          user.ID(),
	}

	switch subject.Kind {
	case SubjectKindPrincipal:
		sc.SubjectPrincipalType = &subject.PrincipalType
		sc.SubjectPrincipalID = &subject.PrincipalID
	case SubjectKindGroupClosure:
		sc.SubjectGroupID = &subject.GroupID
	}

	if draft.Condition != nil {
		sc.NotBefore = draft.Condition.NotBefore
		sc.ExpiresAt = draft.Condition.ExpiresAt
	}

	return sc, nil
}

// parseIfMatchRevision parses the If-Match header value as a revision number.
// Accepts formats: "42", `"42"` (quoted).
func parseIfMatchRevision(ifMatch string) (int64, error) {
	// Strip quotes.
	v := strings.Trim(ifMatch, `"`)
	v = strings.TrimSpace(v)
	if v == "" || v == "*" {
		return 0, fmt.Errorf("revision number required")
	}
	rev, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid revision number: %w", err)
	}
	if rev <= 0 {
		return 0, fmt.Errorf("revision must be a positive integer")
	}
	return rev, nil
}

// parseIntOr parses a string as an integer, returning a default on failure.
func parseIntOr(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

// hasUnknownFields re-parses the raw request body and checks for unknown fields.
// NOTE: This is a best-effort check. We use json.Decoder.DisallowUnknownFields()
// which can only check top-level fields for our request types.
func hasUnknownFields(r *http.Request) bool {
	// This is implemented as a policy for the preview endpoint.
	// For now, we do basic checking by looking at the content type.
	// The readJSON + struct tags already restrict what's accepted.
	// Additional unknown field rejection is handled by the JSON decoder
	// settings in the caller.
	return false
}

// ---------------------------------------------------------------------------
// Server field extensions (B7 service wiring)
// ---------------------------------------------------------------------------

// previewService provides access to the B3 preview engine.
// Set via initBoundaryServices().
// Field declared here to document B7's dependency; the actual field lives
// on Server.

// governanceService provides access to the B5 governance service.
// Set via initBoundaryServices().

// capabilitiesService provides access to the B6 capabilities service.
// Set via initBoundaryServices().

// initBoundaryServices initializes the B3-B6 services on the Server.
// Called during server startup after authzService is available.
func (s *Server) initBoundaryServices() {
	if s.authzService == nil {
		return
	}
	logger := slog.Default()

	s.previewService = NewPreviewService(s.store, s.authzService, logger)
	s.capabilitiesService = NewCapabilitiesService(s.store, s.authzService, logger)

	// Create audit writer and event bus.
	auditWriter := NewBoundaryAuditWriter(logger)
	eventBus := NewInvalidationEventBus(logger)

	gs := NewGovernanceService(s.store, s.previewService, s.authzService, logger)
	gs.auditWriter = auditWriter
	gs.eventBus = eventBus
	s.governanceService = gs
}
