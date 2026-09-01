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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
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
// WP0-conformant capabilities serialization (R1)
// ---------------------------------------------------------------------------

// wpCapabilities is the WP0 wire shape for capabilities. The B6
// BoundaryCapabilities struct uses boolean fields; the WP0 contract
// (type-shapes.ts §8) specifies an actions array. This wrapper lives
// in B7 (serialization layer), not B6.
type wpCapabilities struct {
	Actions []string `json:"actions"`
}

// toWPCapabilities maps B6 boolean BoundaryCapabilities to the WP0
// actions array format. Action names match type-shapes.ts §8
// AccessBoundaryCapabilityAction.
func toWPCapabilities(bc *BoundaryCapabilities) *wpCapabilities {
	if bc == nil {
		return &wpCapabilities{Actions: []string{}}
	}
	// Always include "read" — if the actor can see the resource, they have read.
	actions := []string{"read"}
	if bc.CanPreview {
		actions = append(actions, "previewCreate", "previewTighten", "previewRelax")
	}
	if bc.CanCreate {
		actions = append(actions, "commit")
	}
	if bc.CanUpdate {
		// commit covers create+update; only add if not already present from CanCreate.
		if !bc.CanCreate {
			actions = append(actions, "commit")
		}
	}
	if bc.CanDelete {
		actions = append(actions, "delete")
	}
	if bc.IsAdmin {
		actions = append(actions, "readAudit")
	}
	return &wpCapabilities{Actions: actions}
}

// ---------------------------------------------------------------------------
// Strict JSON decoding (R2)
// ---------------------------------------------------------------------------

// readJSONStrict decodes JSON from the request body into v, rejecting unknown
// fields. Used for mutation endpoints (create, update, preview) to enforce the
// "unknown mutation fields are rejected" contract.
func readJSONStrict(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("empty request body")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Permission display name cache (N3)
// ---------------------------------------------------------------------------

var (
	permissionDisplayNameCache     map[string]string
	permissionDisplayNameCacheOnce sync.Once
)

// getPermissionDisplayNameLookup returns a lazily-built, cached lookup map
// from permission ID to display name. The registry is static for the process
// lifetime, so the map is built once and shared.
func getPermissionDisplayNameLookup() map[string]string {
	permissionDisplayNameCacheOnce.Do(func() {
		m := make(map[string]string, len(permissions.Registry))
		for _, p := range permissions.Registry {
			m[p.ID] = p.Description
		}
		permissionDisplayNameCache = m
	})
	return permissionDisplayNameCache
}

// ---------------------------------------------------------------------------
// Request types — preview-bound mutations (B7)
// ---------------------------------------------------------------------------

// accessConstraintCreateRequest is the payload for POST
// /api/v1/admin/access-constraints (preview-bound create).
type accessConstraintCreateRequest struct {
	Name               string                  `json:"name"`
	Purpose            string                  `json:"purpose"`
	Subject            subjectSelectorRequest  `json:"subject"`
	Scope              constraintScopeRequest  `json:"scope"`
	MaximumPermissions []string                `json:"maximumPermissions"`
	AppliesWhen        *constraintConditionReq `json:"appliesWhen,omitempty"`
	PreviewToken       string                  `json:"previewToken"`
}

// accessConstraintUpdateRequest is the payload for PUT
// /api/v1/admin/access-constraints/:id (preview-bound full update).
type accessConstraintUpdateRequest struct {
	Name               string                  `json:"name"`
	Purpose            string                  `json:"purpose"`
	Subject            subjectSelectorRequest  `json:"subject"`
	Scope              constraintScopeRequest  `json:"scope"`
	MaximumPermissions []string                `json:"maximumPermissions"`
	AppliesWhen        *constraintConditionReq `json:"appliesWhen,omitempty"`
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
	// BaseRevision is an opaque string on the wire (N4, WP0 contract).
	// Parsed to int64 internally.
	BaseRevision string `json:"baseRevision,omitempty"`
}

type previewDraftRequest struct {
	Name               string                  `json:"name"`
	Purpose            string                  `json:"purpose"`
	Subject            subjectSelectorRequest  `json:"subject"`
	Scope              constraintScopeRequest  `json:"scope"`
	MaximumPermissions []string                `json:"maximumPermissions"`
	AppliesWhen        *constraintConditionReq `json:"appliesWhen,omitempty"`
}

// ---------------------------------------------------------------------------
// API response types (B7 response shape)
// ---------------------------------------------------------------------------

// principalRef is the WP0 wire shape for createdBy/updatedBy (R4.3).
type principalRef struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// appliesWhenResponse is the WP0 wire shape for time conditions (R4.6).
type appliesWhenResponse struct {
	NotBefore *time.Time `json:"notBefore"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// accessBoundarySummary is a list row with resolved references and capabilities.
// Field shapes conform to the frozen WP0 contract (type-shapes.ts §8).
type accessBoundarySummary struct {
	ID                     string              `json:"id"`
	Name                   string              `json:"name"`
	Purpose                string              `json:"purpose"`
	Subject                resolvedSubject     `json:"subject"`
	SubjectDisplay         subjectDisplayResp  `json:"subjectDisplay"`
	Scope                  resolvedScope       `json:"scope"`
	ScopeDisplay           scopeDisplayResp    `json:"scopeDisplay"`
	MaxPermissionCount     int                 `json:"maximumPermissionCount"`
	AffectedPrincipalCnt   int                 `json:"affectedPrincipalCount"`
	AffectedPrincipalExact bool                `json:"affectedPrincipalCountExact"`
	Status                 string              `json:"status"`
	Risk                   []string            `json:"risk"`
	Health                 wpResolutionHealth  `json:"health"`
	AppliesWhen            appliesWhenResponse `json:"appliesWhen"`
	Revision               string              `json:"revision"`
	CreatedBy              *principalRef       `json:"createdBy"`
	CreatedAt              time.Time           `json:"createdAt"`
	UpdatedBy              *principalRef       `json:"updatedBy"`
	UpdatedAt              time.Time           `json:"updatedAt"`
	Capabilities           *wpCapabilities     `json:"_capabilities"`
}

// accessBoundaryDetail is the full record with temporal impact, lockout, provenance.
// MaximumPermissions lives here (not on summary) per WP0 contract:
// the summary carries only maximumPermissionCount, while the detail
// includes the full resolved permission array.
type accessBoundaryDetail struct {
	accessBoundarySummary
	MaximumPermissions []resolvedPermission `json:"maximumPermissions"`
	TemporalImpact     []TemporalImpact     `json:"temporalImpact,omitempty"`
	Lockout            *LockoutAssessment   `json:"lockout,omitempty"`
	Provenance         *provenanceLinks     `json:"provenance,omitempty"`
}

type resolvedSubject struct {
	Kind          string `json:"kind"`
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalID   string `json:"principalId,omitempty"`
	GroupID       string `json:"groupId,omitempty"`
}

// subjectDisplayResp is the WP0 ConstraintSubjectDisplay shape.
type subjectDisplayResp struct {
	Kind          string `json:"kind"`
	Label         string `json:"label"`
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalName string `json:"principalName,omitempty"`
	GroupName     string `json:"groupName,omitempty"`
	Resolved      bool   `json:"resolved"`
}

type resolvedScope struct {
	Type      string `json:"type"`
	ProjectID string `json:"projectId,omitempty"`
}

// scopeDisplayResp is the WP0 ConstraintScopeDisplay shape.
type scopeDisplayResp struct {
	Type        string  `json:"type"`
	Label       string  `json:"label"`
	ProjectName *string `json:"projectName"`
	Resolved    bool    `json:"resolved"`
}

type resolvedPermission struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
}

// wpResolutionHealth is the WP0 ResolutionHealth wire shape (R4.2).
// Maps from the old {healthy, degraded, reason} to {state, unresolvedReferences}.
type wpResolutionHealth struct {
	State                string                `json:"state"`
	UnresolvedReferences []unresolvedReference `json:"unresolvedReferences"`
}

// unresolvedReference identifies a reference that could not be resolved.
type unresolvedReference struct {
	Field         string `json:"field"`
	ReferenceType string `json:"referenceType"`
	ReferenceID   string `json:"referenceId"`
	Reason        string `json:"reason"`
}

type provenanceLinks struct {
	AuditURL string `json:"auditUrl,omitempty"`
}

// accessBoundaryListResponse is the list envelope.
// Includes collection-level _capabilities (R4.7).
type accessBoundaryListResponse struct {
	Items         []accessBoundarySummary `json:"items"`
	NextPageToken string                  `json:"nextPageToken,omitempty"`
	TotalCount    int                     `json:"totalCount"`
	Capabilities  *wpCapabilities         `json:"_capabilities"`
}

// auditEventResponse wraps a single audit entry for the API.
type auditEventResponse struct {
	ID             string       `json:"id"`
	ConstraintID   string       `json:"constraintId"`
	Operation      string       `json:"operation"`
	ActorID        string       `json:"actorId"`
	BeforeRevision string       `json:"beforeRevision"`
	AfterRevision  string       `json:"afterRevision"`
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

	// Collection-level _capabilities (R4.7): gates "New boundary" control.
	// Default to empty actions array so _capabilities is always present (WP0 contract).
	collectionCaps := &wpCapabilities{Actions: []string{}}
	if actor != nil && s.capabilitiesService != nil {
		bc, err := s.capabilitiesService.ComputeCapabilities(r.Context(), *actor, ScopeTypeSystem, "")
		if err == nil {
			collectionCaps = toWPCapabilities(bc)
		}
	}

	writeJSON(w, http.StatusOK, accessBoundaryListResponse{
		Items:         items,
		NextPageToken: nextToken,
		TotalCount:    totalCount,
		Capabilities:  collectionCaps,
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

	// If-Match revision support: set ETag header (opaque string per WP0).
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, strconv.FormatInt(sc.Revision, 10)))

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

	// Keyset pagination based on principal ID (R5).
	pageSize := parseIntOr(r.URL.Query().Get("pageSize"), 50)
	if pageSize > 200 {
		pageSize = 200
	}

	allPrincipals := preview.AffectedPrincipals
	total := len(allPrincipals)

	// Sort by PrincipalID for deterministic keyset ordering.
	sort.Slice(allPrincipals, func(i, j int) bool {
		return allPrincipals[i].PrincipalID < allPrincipals[j].PrincipalID
	})

	// Decode cursor: base64-encoded last-seen principal ID.
	var cursorID string
	if rawToken := r.URL.Query().Get("pageToken"); rawToken != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(rawToken)
		if err != nil {
			BadRequest(w, "invalid pageToken")
			return
		}
		cursorID = string(decoded)
	}

	// Filter: skip all items with PrincipalID <= cursorID.
	startIdx := 0
	if cursorID != "" {
		for i, ap := range allPrincipals {
			if ap.PrincipalID > cursorID {
				startIdx = i
				break
			}
			if i == len(allPrincipals)-1 {
				startIdx = len(allPrincipals) // all items consumed
			}
		}
	}

	end := startIdx + pageSize
	if end > total {
		end = total
	}

	var items []AffectedPrincipal
	if startIdx < total {
		items = allPrincipals[startIdx:end]
	}
	if items == nil {
		items = []AffectedPrincipal{}
	}

	var nextToken string
	if end < total && len(items) > 0 {
		lastID := items[len(items)-1].PrincipalID
		nextToken = base64.RawURLEncoding.EncodeToString([]byte(lastID))
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

	// Keyset pagination based on audit entry ID (R5).
	// Audit entries are append-only, so ID-based keyset is stable.
	pageSize := parseIntOr(r.URL.Query().Get("pageSize"), 50)
	if pageSize > 200 {
		pageSize = 200
	}

	// Sort entries by ID for deterministic ordering.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})

	total := len(entries)

	// Decode cursor: base64-encoded last-seen audit entry ID.
	var cursorID string
	if rawToken := r.URL.Query().Get("pageToken"); rawToken != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(rawToken)
		if err != nil {
			BadRequest(w, "invalid pageToken")
			return
		}
		cursorID = string(decoded)
	}

	// Filter: skip all entries with ID <= cursorID.
	startIdx := 0
	if cursorID != "" {
		for i, e := range entries {
			if e.ID > cursorID {
				startIdx = i
				break
			}
			if i == len(entries)-1 {
				startIdx = len(entries) // all items consumed
			}
		}
	}

	end := startIdx + pageSize
	if end > total {
		end = total
	}

	var items []auditEventResponse
	if startIdx < total {
		for _, e := range entries[startIdx:end] {
			items = append(items, auditEventResponse{
				ID:             e.ID,
				ConstraintID:   e.ConstraintID,
				Operation:      e.Operation,
				ActorID:        e.ActorID,
				BeforeRevision: strconv.FormatInt(e.BeforeRevision, 10),
				AfterRevision:  strconv.FormatInt(e.AfterRevision, 10),
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
	if end < total && len(items) > 0 {
		lastID := items[len(items)-1].ID
		nextToken = base64.RawURLEncoding.EncodeToString([]byte(lastID))
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
	if err := readJSONStrict(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Build the preview request.
	actor := PrincipalContext{
		Kind: PrincipalKindUser,
		ID:   user.ID(),
	}

	// Parse BaseRevision from opaque string to int64 (N4).
	var baseRevision int64
	if req.BaseRevision != "" {
		var parseErr error
		baseRevision, parseErr = strconv.ParseInt(req.BaseRevision, 10, 64)
		if parseErr != nil {
			BadRequest(w, "invalid baseRevision: must be a numeric string")
			return
		}
	}

	previewReq := PreviewRequest{
		Operation:    req.Operation,
		ConstraintID: req.ConstraintID,
		BaseRevision: baseRevision,
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
	if err := readJSONStrict(r, &req); err != nil {
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

	// N1: purpose is required by the WP0 contract.
	req.Purpose = strings.TrimSpace(req.Purpose)
	if req.Purpose == "" {
		BadRequest(w, "purpose is required")
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

	// Set time window (appliesWhen).
	if req.AppliesWhen != nil {
		draft.NotBefore = req.AppliesWhen.NotBefore
		draft.ExpiresAt = req.AppliesWhen.ExpiresAt
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
	if err := readJSONStrict(r, &req); err != nil {
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

	// N1: purpose is required by the WP0 contract.
	req.Purpose = strings.TrimSpace(req.Purpose)
	if req.Purpose == "" {
		BadRequest(w, "purpose is required")
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

	if req.AppliesWhen != nil {
		draft.NotBefore = req.AppliesWhen.NotBefore
		draft.ExpiresAt = req.AppliesWhen.ExpiresAt
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
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, strconv.FormatInt(result.Constraint.Revision, 10)))
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

	// R3: Accept preview token ONLY from request body (JSON) or
	// X-Preview-Token header. Never from query parameters — tokens are
	// HMAC credentials and must not appear in URLs.
	var previewToken string

	// Try reading from body first (if content-type is JSON).
	if r.Header.Get("Content-Type") == "application/json" || r.ContentLength > 0 {
		var req accessConstraintDeleteRequest
		if err := readJSONStrict(r, &req); err == nil {
			previewToken = req.PreviewToken
		}
	}

	// Fall back to X-Preview-Token header.
	if previewToken == "" {
		previewToken = r.Header.Get("X-Preview-Token")
	}

	if previewToken == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"previewToken is required; provide via request body or X-Preview-Token header", nil)
		return
	}

	// N2: This pre-fetch checks the Disabled flag before calling
	// CommitBoundaryChange. The governance service will re-fetch internally,
	// but we need the early check to return the correct error code for
	// recovery-disabled constraints without consuming the preview token.
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
// Response shape conforms to the frozen WP0 contract (type-shapes.ts §8).
func (s *Server) buildBoundarySummary(ctx context.Context, sc *store.AccessConstraint, actor *PrincipalContext) accessBoundarySummary {
	hc := storeToHubAccessConstraint(sc)

	// Resolve subject display name.
	subjectDisplayName := s.resolveSubjectDisplayName(ctx, sc)

	// Resolve scope display name.
	scopeDisplayName := s.resolveScopeDisplayName(ctx, sc.ScopeType, sc.ScopeID)

	// Compute status.
	status := computeConstraintStatus(sc, hc)

	// Compute health (WP0 shape).
	health := computeWPHealth(hc)

	// Build subject display (WP0 ConstraintSubjectDisplay shape).
	subjectDisp := buildSubjectDisplay(sc, subjectDisplayName)

	// Build scope display (WP0 ConstraintScopeDisplay shape).
	scopeDisp := buildScopeDisplay(sc, scopeDisplayName)

	// R4.3: createdBy/updatedBy as PrincipalRef objects.
	createdByRef := s.resolvePrincipalRef(ctx, sc.CreatedBy)
	updatedByRef := s.resolvePrincipalRef(ctx, sc.UpdatedBy)

	summary := accessBoundarySummary{
		ID:                     sc.ID,
		Name:                   sc.Name,
		Purpose:                sc.Purpose,
		Subject:                resolvedSubjectFromStore(sc),
		SubjectDisplay:         subjectDisp,
		Scope:                  resolvedScopeFromStore(sc),
		ScopeDisplay:           scopeDisp,
		MaxPermissionCount:     len(sc.MaximumPermissions),
		AffectedPrincipalCnt:   0, // R4.5: populated from cached count; TODO: compute from preview cache.
		AffectedPrincipalExact: false,
		Status:                 status,
		Risk:                   []string{}, // R4.4: empty array; risk assessment not yet implemented.
		Health:                 health,
		AppliesWhen: appliesWhenResponse{
			NotBefore: sc.NotBefore,
			ExpiresAt: sc.ExpiresAt,
		},
		Revision:  strconv.FormatInt(sc.Revision, 10),
		CreatedBy: createdByRef,
		CreatedAt: sc.CreatedAt,
		UpdatedBy: updatedByRef,
		UpdatedAt: sc.UpdatedAt,
	}

	// Compute capabilities if actor is available (R1: WP0 actions array).
	// Default to empty actions array so _capabilities is always present (WP0 contract).
	summary.Capabilities = &wpCapabilities{Actions: []string{}}
	if actor != nil && s.capabilitiesService != nil {
		caps, err := s.capabilitiesService.ComputeResourceCapabilities(ctx, *actor, sc.ID)
		if err == nil {
			summary.Capabilities = toWPCapabilities(caps)
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

	// MaximumPermissions lives on detail only (WP0 contract: summary has count only).
	detail.MaximumPermissions = resolvePermissionDisplayNames(sc.MaximumPermissions)

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

func resolvedSubjectFromStore(sc *store.AccessConstraint) resolvedSubject {
	rs := resolvedSubject{
		Kind: sc.SubjectKind,
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

func resolvedScopeFromStore(sc *store.AccessConstraint) resolvedScope {
	rs := resolvedScope{Type: sc.ScopeType}
	if sc.ScopeType == ScopeTypeProject {
		rs.ProjectID = sc.ScopeID
	}
	return rs
}

// buildSubjectDisplay creates the WP0 ConstraintSubjectDisplay shape.
func buildSubjectDisplay(sc *store.AccessConstraint, displayName string) subjectDisplayResp {
	d := subjectDisplayResp{
		Kind:     sc.SubjectKind,
		Label:    displayName,
		Resolved: displayName != "",
	}
	switch sc.SubjectKind {
	case store.ConstraintSubjectPrincipal:
		if sc.SubjectPrincipalType != nil {
			d.PrincipalType = *sc.SubjectPrincipalType
		}
		d.PrincipalName = displayName
	case store.ConstraintSubjectGroupClosure:
		d.GroupName = displayName
	case store.ConstraintSubjectAllPrincipals:
		d.Resolved = true
	}
	return d
}

// buildScopeDisplay creates the WP0 ConstraintScopeDisplay shape.
func buildScopeDisplay(sc *store.AccessConstraint, displayName string) scopeDisplayResp {
	d := scopeDisplayResp{
		Type:     sc.ScopeType,
		Label:    displayName,
		Resolved: displayName != "",
	}
	if sc.ScopeType == ScopeTypeProject && displayName != "" && displayName != sc.ScopeID {
		d.ProjectName = &displayName
	}
	return d
}

// resolvePrincipalRef resolves a bare user ID to a PrincipalRef (R4.3).
// Falls back to showing the ID as display name if unresolvable.
func (s *Server) resolvePrincipalRef(_ context.Context, userID string) *principalRef {
	if userID == "" {
		return nil
	}
	// Default: use ID as display name. A full resolution would look up
	// the user from the store, but we use the ID as the fallback to avoid
	// extra queries on every list/detail call.
	return &principalRef{
		Type:        "user",
		ID:          userID,
		DisplayName: userID,
	}
}

// N3: Uses cached lookup map instead of rebuilding per call.
func resolvePermissionDisplayNames(ids []string) []resolvedPermission {
	lookup := getPermissionDisplayNameLookup()
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

// computeWPHealth converts the internal health to the WP0 ResolutionHealth shape (R4.2).
func computeWPHealth(hc *AccessConstraint) wpResolutionHealth {
	if hc == nil || hc.Degraded {
		return wpResolutionHealth{
			State: "degraded",
			UnresolvedReferences: []unresolvedReference{
				{
					Field:         "stored_data",
					ReferenceType: "unknown",
					ReferenceID:   "",
					Reason:        "resolution_failed",
				},
			},
		}
	}
	return wpResolutionHealth{
		State:                "healthy",
		UnresolvedReferences: []unresolvedReference{},
	}
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

	if draft.AppliesWhen != nil {
		sc.NotBefore = draft.AppliesWhen.NotBefore
		sc.ExpiresAt = draft.AppliesWhen.ExpiresAt
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

// hasUnknownFields was removed (R2): replaced by readJSONStrict which uses
// json.NewDecoder.DisallowUnknownFields() as the primary decode path for
// all mutation and preview endpoints.

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
