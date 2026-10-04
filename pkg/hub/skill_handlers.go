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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Masterminds/semver/v3"
	"golang.org/x/sync/errgroup"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

const (
	githubAPIBase = "https://api.github.com"
	githubRawBase = "https://raw.githubusercontent.com"
)

// CreateSkillRequest is the request body for creating a skill.
type CreateSkillRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Scope       string   `json:"scope"`
	ScopeID     string   `json:"scopeId,omitempty"`
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
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
	case "files":
		// parts is split into at most 3 segments, so parts[2] carries the
		// full (possibly nested) file path.
		filePath := ""
		if len(parts) == 3 {
			filePath = parts[2]
		}
		s.handleSkillFiles(w, r, skillID, filePath)
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
	}
}

// listSkills lists skills with filtering.
func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	if !checkAgentReadScope(w, r) {
		return
	}

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

	identity := GetIdentityFromContext(ctx)

	// ptone/scion#1901 (pagination follow-up): resolve the caller's project
	// memberships and push the whole read boundary (hub scope, own user
	// scope, member projects) into the store query, ahead of COUNT and
	// LIMIT. "project.list" is the right proxy permission here: every
	// project role that carries skill.list also carries project.list, and
	// only the elevated hub-admin/super-admin roles resolve to an
	// unrestricted (IsAll) scope — exactly the pair of facts (my projects;
	// am I unrestricted) this predicate needs. See skillAccessScopePredicate
	// in pkg/store/entadapter/skill_store.go. Visibility no longer widens
	// this boundary (ptone/scion#1903): an anonymous caller gets an
	// AccessScope with no matching terms, which authorizes nothing.
	scopeResult, err := s.authzService.ResolveListScopes(ctx, identity, "project.list")
	resolutionFailed := err != nil
	if resolutionFailed {
		// ptone/scion#1954: a principal-resolution error here (e.g. a
		// federated identity whose ID isn't a bare UUID, which trips
		// parseUUID in GetEffectiveGroups/GetEffectiveGroupsForAgent) must
		// not take the whole endpoint down for every caller. Logged with
		// principal type only: never the raw principal ID.
		principalType := "anonymous"
		if identity != nil {
			principalType = identity.Type()
		}
		slog.WarnContext(ctx, "listSkills: failed to resolve list scopes; failing closed",
			"principal_type", principalType, "error", err)
	}
	switch {
	case identity == nil:
		// ptone/scion#1903 removed the public-visibility bypass: an
		// anonymous caller now gets a scope with no matching terms, which
		// authorizes nothing.
		filter.AccessScope = &store.SkillAccessScope{}
	case resolutionFailed:
		// ptone/scion#1954: the same principal-resolution error that failed
		// ResolveListScopes also makes Decide() fail closed on every row's
		// per-row capability check below (see authz.go), so this principal
		// can never actually read anything regardless of what the query
		// predicate offers. Giving it the ordinary no-grant-user floor
		// (IncludeHubScope + CallerID + ProjectIDs) would only produce a
		// nonzero totalCount over an empty page — exactly the count/page
		// mismatch this fix closes for agents below — and would needlessly
		// disclose the hub-wide skill count to a principal that can't read
		// any of them. Match what Decide() actually grants: nothing.
		filter.AccessScope = &store.SkillAccessScope{}
	case isAgentIdentity(identity):
		// ptone/scion#1968: agents read exactly their granted set — the hub
		// catalog (global/core), their own project's skills, and their
		// creator's own user-scoped skills, each gated
		// by the agent JWT scope restriction and the delegation ceiling. See
		// agentSkillAccessScope for why per-bucket probes equal the per-row
		// decision, which keeps totalCount and pages consistent with point
		// reads (the ptone/scion#1954 count/page mismatch).
		//
		// This case must stay before the IsAll() check below
		// (ptone/scion#1954): an agent always gets an explicit, bounded
		// predicate, never an unfiltered query.
		filter.AccessScope = s.agentSkillAccessScope(ctx, identity.(AgentIdentity))
	case scopeResult.Scopes.IsAll():
		// identity holds an unrestricted (hub-admin/super-admin) scope —
		// leave filter.AccessScope nil so the query is unfiltered.
	default:
		filter.AccessScope = &store.SkillAccessScope{
			IncludeHubScope: true,
			CallerID:        identity.ID(),
			ProjectIDs:      scopeResult.Scopes.ProjectIDs(),
		}
	}

	result, err := s.store.ListSkills(ctx, filter, listOptionsFromQuery(query))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// The store query above already applies the caller's read boundary, so
	// result.Items only contains rows the caller may see and result.TotalCount
	// only counts those rows — pagination cannot crowd them out. The
	// capability computation below is defense in depth (it also derives the
	// per-row "_capabilities" the response returns), not the access decision.
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
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.skillListCapabilities(ctx, filter.Scope, filter.ScopeID, scopeResult.Scopes)
	}

	writeJSON(w, http.StatusOK, ListSkillsResponse{
		Skills:       skills,
		NextCursor:   result.NextCursor,
		TotalCount:   result.TotalCount,
		Capabilities: scopeCap,
	})
}

// skillListCapabilities returns the list-level capabilities for a skills
// list request. "create" is reported when the caller can create a skill in
// at least one scope the request covers: any scope when no scope filter is
// given, otherwise only the filtered scope (and, for project and user
// scopes, the filtered scopeId when one is set). Each scope is checked the
// same way createSkill authorizes a create in it. Listing is not reported
// as a capability: a 200 response already means the caller may list.
//
// projects is the caller's project list scope from ResolveListScopes; it
// supplies the candidate projects for a project-scope check without a
// scopeId.
func (s *Server) skillListCapabilities(ctx context.Context, scopeFilter, scopeIDFilter string, projects ScopeSet) *Capabilities {
	scopes := []string{scopeFilter}
	if scopeFilter == "" {
		scopes = []string{store.SkillScopeUser, store.SkillScopeProject, store.SkillScopeGlobal, store.SkillScopeCore}
	}
	for _, scope := range scopes {
		if s.canCreateSkillInScope(ctx, scope, scopeIDFilter, projects) {
			return &Capabilities{Actions: []string{string(ActionCreate)}}
		}
	}
	return &Capabilities{Actions: []string{}}
}

// canCreateSkillInScope reports whether the caller in ctx could create a
// skill in scope (restricted to scopeID when it is set), following the
// authorization branches of createSkill.
func (s *Server) canCreateSkillInScope(ctx context.Context, scope, scopeID string, projects ScopeSet) bool {
	switch scope {
	case store.SkillScopeUser:
		// createSkill always places a user-scoped skill in the caller's own
		// user scope, so another user's scope is never creatable.
		userIdent := GetUserIdentityFromContext(ctx)
		return userIdent != nil && (scopeID == "" || scopeID == userIdent.ID())
	case store.SkillScopeGlobal, store.SkillScopeCore:
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			return false
		}
		return s.authzService.CheckAccess(ctx, userIdent, skillScopeResource(scope, ""), globalWriteAction(scope, ActionCreate)).Allowed
	case store.SkillScopeProject:
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			own := agentIdent.ProjectID()
			return own != "" && agentIdent.HasScope(ScopeAgentCreate) && (scopeID == "" || scopeID == own)
		}
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			return false
		}
		candidates := projects.ProjectIDs()
		switch {
		case scopeID != "":
			candidates = []string{scopeID}
		case projects.IsAll():
			// An unrestricted caller's project list is not enumerated; ask
			// whether it may create in project scope at all.
			candidates = []string{""}
		}
		for _, projectID := range candidates {
			if s.authzService.CheckAccess(ctx, userIdent, skillScopeResource(store.SkillScopeProject, projectID), ActionCreate).Allowed {
				return true
			}
		}
	}
	return false
}

// agentSkillAccessScope derives an agent's list predicate from the same
// authorization decisions its point reads get (ptone/scion#1968).
//
// For an agent principal every term of Decide depends on a skill row only
// through its bucket: (scope kind, project) for hub and project skills, and
// (scope kind, owning user) for user skills. The project-scoped JWT binding,
// the synthetic agent-skill-catalog binding (Step 5b/5b2, global/core only
// after Step 5c), the personal-skill progeny relationship grant (Step 9, user
// skills owned by the agent's origin user only, with that user's admission to
// the agent's execution project, a per-agent fact), the JWT scope restriction
// and access constraints (Step 7), and the delegation ceiling (Step 10,
// permission-level at the agent's project) all read nothing else from the
// row. So one probe per bucket equals the per-row outcome for every row in
// that bucket, and the store predicate built from the probes returns exactly
// the rows the per-row check allows.
//
// global and core share one probe: the store predicate groups them
// (ScopeIn(global, core)) and every Decide input treats them identically.
// The only user bucket that can be granted is the origin user's, so that is
// the only one probed; every other user's skills stay out of the predicate.
func (s *Server) agentSkillAccessScope(ctx context.Context, agent AgentIdentity) *store.SkillAccessScope {
	scope := &store.SkillAccessScope{}
	scope.IncludeHubScope = s.authzService.CheckAccess(ctx, agent,
		skillScopeResource(store.SkillScopeGlobal, ""), ActionRead).Allowed
	if projectID := agent.ProjectID(); projectID != "" {
		if s.authzService.CheckAccess(ctx, agent,
			skillScopeResource(store.SkillScopeProject, projectID), ActionRead).Allowed {
			scope.ProjectIDs = []string{projectID}
		}
	}
	if origin := agent.OriginUserID(); origin != "" {
		if s.authzService.CheckAccess(ctx, agent,
			skillScopeResource(store.SkillScopeUser, origin), ActionRead).Allowed {
			scope.CallerID = origin
		}
	}
	return scope
}

// isAgentIdentity reports whether identity is a local or federated agent.
// Both agentIdentityWrapper and FederatedAgentIdentity implement
// AgentIdentity; BrokerIdentity and UserIdentity implementations
// deliberately do not (see brokerIdentityImpl's doc comment, DEF-58).
func isAgentIdentity(identity Identity) bool {
	_, ok := identity.(AgentIdentity)
	return ok
}

// readSkillWriteBody decodes a create or update skill request body into v.
// Skills no longer carry a visibility setting (access follows the skill's
// scope), so a body that still sends one is rejected with 400 rather than
// having the field silently dropped. The body must be a single JSON value:
// trailing data is rejected, so the visibility check always covers exactly
// the value decoded into v. The body is limited by readRawBody (413 when
// exceeded). On failure it writes the error response and returns false.
func readSkillWriteBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	body, err := readRawBody(w, r)
	if err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body too large", nil)
			return false
		}
		BadRequest(w, "Invalid request body: "+err.Error())
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		BadRequest(w, "Invalid request body: unexpected data after the JSON value")
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(value, &fields) == nil {
		for name := range fields {
			// encoding/json matches struct fields case-insensitively, so
			// treat the key the same way.
			if strings.EqualFold(name, "visibility") {
				ValidationError(w, "visibility is not supported: access to a skill is determined by its scope",
					map[string]interface{}{"field": "visibility"})
				return false
			}
		}
	}
	if err := json.Unmarshal(value, v); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return false
	}
	return true
}

// createSkill creates a new skill record.
func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateSkillRequest
	if !readSkillWriteBody(w, r, &req) {
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
		decision := s.authzService.CheckAccess(ctx, userIdent, skillScopeResource(scope, ""), globalWriteAction(scope, ActionCreate))
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to create global skills", nil)
			return
		}
	case store.SkillScopeProject:
		if req.ScopeID == "" {
			writeError(w, http.StatusBadRequest, "scope_id_required",
				"scopeId is required for project-scoped skills", nil)
			return
		}
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
			decision := s.authzService.CheckAccess(ctx, userIdent,
				skillScopeResource(store.SkillScopeProject, req.ScopeID), ActionCreate)
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
		Status:      "active",
	}

	// Set owner from identity
	if identity := GetIdentityFromContext(ctx); identity != nil {
		skill.OwnerID = identity.ID()
		skill.CreatedBy = identity.ID()
		skill.UpdatedBy = identity.ID()
	}

	// Generate storage path and URI
	storagePath := storage.SkillStoragePath(s.HubID(), skill.Scope, skill.ScopeID, skill.Slug)
	skill.StoragePath = storagePath

	stor := s.GetStorage()
	if stor != nil {
		skill.StorageBucket = stor.Bucket()
		skill.StorageURI = storage.SkillStorageURI(s.HubID(), stor.Bucket(), skill.Scope, skill.ScopeID, skill.Slug)
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
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	skill, err := s.store.GetSkill(ctx, id)
	if err != nil {
		writeSkillLookupError(w, err)
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		NotFound(w, "Skill")
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
	if !decision.Allowed {
		NotFound(w, "Skill")
		return
	}

	resp := SkillWithCapabilities{Skill: *skill}
	resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, skillResource(skill))

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
	if !readSkillWriteBody(w, r, &updates) {
		return
	}

	if updates.Name != "" {
		existing.Name = updates.Name
		existing.Slug = api.Slugify(updates.Name)
	}
	if updates.Description != "" {
		existing.Description = updates.Description
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
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
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.getSkillVersion(w, r, skillID, versionID)
}

// listSkillVersions lists versions for a skill.
func (s *Server) listSkillVersions(w http.ResponseWriter, r *http.Request, skillID string) {
	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeSkillLookupError(w, err)
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		NotFound(w, "Skill")
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
	if !decision.Allowed {
		NotFound(w, "Skill")
		return
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
		writeSkillLookupError(w, err)
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		NotFound(w, "Skill")
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
	if !decision.Allowed {
		NotFound(w, "Skill")
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

	writeJSON(w, http.StatusOK, sv)
}

// deprecateSkillVersion marks a published skill version as deprecated.
func (s *Server) deprecateSkillVersion(w http.ResponseWriter, r *http.Request, skillID, versionID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
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
					uploadURLs = withSkillVersionUploadQuery(uploadURLs, req.Version)
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
		MethodNotAllowed(w, http.MethodPost)
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
		uploadURLs = withSkillVersionUploadQuery(uploadURLs, req.Version)
	}

	writeJSON(w, http.StatusOK, UploadResponse{
		UploadURLs:  uploadURLs,
		ManifestURL: manifestURL,
	})
}

// handleSkillFinalize finalizes a skill version after file upload.
func (s *Server) handleSkillFinalize(w http.ResponseWriter, r *http.Request, skillID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
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
		MethodNotAllowed(w, http.MethodGet)
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
		writeSkillLookupError(w, err)
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		NotFound(w, "Skill")
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
	if !decision.Allowed {
		NotFound(w, "Skill")
		return
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
	downloadURLs, manifestURL, expires, err := generateDownloadURLs(ctx, stor, versionPath, s.legacyFallbackPath(versionPath), sv.Files)
	if err != nil {
		RuntimeError(w, fmt.Sprintf("skill %q version %q: %s", skill.Name, sv.Version, err))
		return
	}

	if stor.Provider() == storage.ProviderLocal {
		hubURL := requestBaseURL(r)
		downloadURLs = rewriteLocalDownloadURLs(downloadURLs, hubURL, "skills", skillID)
		downloadURLs = withSkillVersionDownloadQuery(downloadURLs, sv.Version)
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
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeSkillLookupError(w, err)
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		NotFound(w, "Skill")
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
	if !decision.Allowed {
		NotFound(w, "Skill")
		return
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
		MethodNotAllowed(w, http.MethodPost)
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

	// Resolving a gh:// URI with a non-empty ProjectID makes the Hub mint that
	// project's GitHub App token, so the caller must be authorized for the
	// project — otherwise anyone could borrow another project's installation to
	// read its private repositories. An empty ProjectID resolves anonymously
	// (no token minted) and needs no check. Evaluated once, up front, and only
	// when the request actually contains a gh:// URI.
	ghProjectAllowed := true
	if req.ProjectID != "" && hasGitHubSkillRef(req.Skills) {
		ghProjectAllowed = s.canUseProjectGitHubToken(ctx, req.ProjectID)
	}

	// Per-request memo for (owner,repo,ref,tokenScope) → commitSHA.
	// URIs sharing the same tuple — the common case for a skill bundle in one
	// repo — perform a single ref→SHA lookup instead of one per URI.
	refSHAMemo := newGHSHAMemo()

	for _, skillRef := range req.Skills {
		// GitHub skill resolution: gh:// URIs are handled by the Hub's GitHub resolution cache
		if strings.HasPrefix(skillRef.URI, "gh://") {
			if !ghProjectAllowed {
				resolveErrors = append(resolveErrors, ResolveSkillError{
					URI: skillRef.URI, Code: agent.SkillErrCodeForbidden,
					Message: "you do not have permission to resolve GitHub skills for this project",
				})
				continue
			}
			ghResolved, err := s.resolveGitHubSkill(ctx, skillRef.URI, req.ProjectID, refSHAMemo)
			if err != nil {
				code := "resolve_failed"
				var rl *agent.GitHubRateLimitError
				if errors.As(err, &rl) {
					code = agent.GitHubRateLimitedCode
				} else if isGHNotFound(err) {
					code = agent.SkillErrCodeNotFound
				}
				resolveErrors = append(resolveErrors, ResolveSkillError{
					URI: skillRef.URI, Code: code, Message: err.Error(),
				})
			} else {
				resolved = append(resolved, *ghResolved)
			}
			continue
		}

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

		baseURL := ""
		if stor != nil && stor.Provider() == storage.ProviderLocal {
			baseURL = requestBaseURL(r)
		}
		entry, resolveErr := s.resolveRegistrySkillRef(ctx, GetIdentityFromContext(ctx), skillRef.URI, uri,
			req.ProjectID, req.UserID, baseURL)
		if resolveErr != nil {
			resolveErrors = append(resolveErrors, *resolveErr)
			continue
		}
		resolved = append(resolved, *entry)
	}

	writeJSON(w, http.StatusOK, ResolveSkillsResponse{
		Resolved: resolved,
		Errors:   resolveErrors,
	})
}

// resolveSkill finds a skill and version by URI, searching scopes in
// priority order, on behalf of identity (nil for an unauthenticated caller).
//
// ptone/scion#1901 finding F2: authorization runs here, per candidate,
// before any version-specific detail is computed — and a candidate the
// caller cannot read (identity is nil and the skill isn't public, or
// CheckAccess denies) is skipped exactly like a scope with no matching slug
// at all, continuing the search rather than stopping to report a
// distinguishable reason. Two things this closes:
//   - a batch/single resolve of a skill name the caller cannot read now
//     produces the exact same "not found" outcome as guessing a nonexistent
//     name, instead of a distinguishable "forbidden";
//   - "found but version could not be resolved" (below) can now only ever
//     describe a candidate the caller was already authorized to read, so it
//     can no longer confirm the existence of a skill version to a caller who
//     cannot read the skill itself.
func (s *Server) resolveSkill(ctx context.Context, identity Identity, uri *api.SkillURI, projectID string) (*store.Skill, *store.SkillVersion, error) {
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

		if identity == nil {
			continue
		}
		decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
		if !decision.Allowed {
			slog.WarnContext(ctx, "skill resolve candidate denied, continuing scope search",
				"skill_id", skill.ID, "scope", sc.scope, "identity_type", identity.Type(), "reason", decision.Reason)
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

// globalWriteAction maps a CRUD action to its global-catalog twin for records
// that live in the hub catalog. Project- and user-scoped records are unchanged.
// Key off the record's stored scope, not parentlessness — user-scoped records
// are also parentless but must NOT require global permissions. See design §3.1.
func globalWriteAction(scope string, a Action) Action {
	switch scope {
	case store.SkillScopeGlobal, store.SkillScopeCore:
		switch a {
		case ActionCreate:
			return ActionCreateGlobal
		}
	}
	return a
}

// skillResource constructs a Resource from a store.Skill for capability computation.
func skillResource(s *store.Skill) Resource {
	if s == nil {
		return Resource{Type: "skill"}
	}
	r := skillScopeResource(s.Scope, s.ScopeID)
	r.ID = s.ID
	r.OwnerID = s.OwnerID
	return r
}

// writeSkillLookupError writes the response for a failed store.GetSkill
// lookup on a read surface. ptone/scion#1901 finding F2a: a missing skill
// and a skill that exists but the caller cannot read must be byte-for-byte
// indistinguishable, so both use NotFound(w, "Skill") — the same call the
// CheckAccess-denied branch a few lines below already uses. Before this fix,
// a missing skill instead went through writeErrorFromErr, which emits the
// generic {"code":"not_found","message":"Resource not found"} — same status
// and code, but different message text than the denied-access body — letting
// an unauthorized caller distinguish "doesn't exist" from "exists, denied"
// by string-comparing responses. Errors other than store.ErrNotFound (a
// genuine backend failure) still go through writeErrorFromErr unchanged.
func writeSkillLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		NotFound(w, "Skill")
		return
	}
	writeErrorFromErr(w, err, "")
}

// skillScopeResource builds an ad hoc "skill" Resource for authorization
// checks that have a scope to evaluate but no concrete store.Skill record
// (e.g. minting a project's GitHub App token before any specific skill is
// resolved — see canUseProjectGitHubToken). scope should be one of the
// store.SkillScope* constants; scopeID is the owning user or project ID and
// is ignored for global/core scope.
//
// ptone/scion#1901 finding F3/F4: every ad hoc skill Resource literal must
// set ScopeKind through this constructor (or skillResource, for a real
// record), never by hand — filterHubWideSkillGrants only narrows the
// curated hub-member/hub-viewer grant when ScopeKind is populated, so a
// hand-built literal that forgets it silently reopens the #1901 leak. See
// TestSkillResourceLiterals_AllUseCanonicalConstructor.
func skillScopeResource(scope, scopeID string) Resource {
	r := Resource{Type: "skill", ScopeKind: scope}
	if scope == store.SkillScopeProject && scopeID != "" {
		r.ParentType = "project"
		r.ParentID = scopeID
	}
	if scope == store.SkillScopeUser {
		r.ScopeUserID = scopeID
	}
	return r
}

// hasGitHubSkillRef reports whether any reference in the batch is a gh:// URI.
func hasGitHubSkillRef(refs []ResolveSkillRef) bool {
	for _, ref := range refs {
		if strings.HasPrefix(ref.URI, "gh://") {
			return true
		}
	}
	return false
}

// canUseProjectGitHubToken reports whether the caller in ctx is permitted to have
// the Hub mint projectID's GitHub App token on their behalf. Agents are confined
// to their own project; users must hold read access on the project's skills;
// brokers must be registered as a provider for the project.
// Unauthenticated callers are always denied.
func (s *Server) canUseProjectGitHubToken(ctx context.Context, projectID string) bool {
	// A BrokerIdentity is NOT a global privilege: broker join is unauthenticated,
	// so anyone can mint one. A broker may only borrow the token of a project it
	// is registered to serve, which GetProjectProvider confirms.
	if brokerIdent := GetBrokerIdentityFromContext(ctx); brokerIdent != nil {
		if s.store == nil {
			return false
		}
		_, err := s.store.GetProjectProvider(ctx, projectID, brokerIdent.BrokerID())
		return err == nil
	}
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		return agentIdent.ProjectID() == projectID
	}
	// Last arm, and the only one left: anything that is not a broker or an agent
	// must be a user holding read on the project's skills. The nil test is
	// written as an explicit deny rather than as a guard around the CheckAccess
	// call, because a guarded call is the #591 shape — there the deny is implied
	// by the absence of an else, and an edit that adds code after the block
	// silently turns it into an allow.
	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		return false
	}
	if s.authzService == nil {
		return false
	}
	return s.authzService.CheckAccess(ctx, userIdent, skillScopeResource(store.SkillScopeProject, projectID), ActionRead).Allowed
}

// resolveGitHubToken determines the GitHub token scope and mints a token if needed.
// Returns (installID, token, error).
// - installID is the GitHub App installation ID (as string) or "public" for unauthenticated.
// - token is the minted GitHub App token (or empty for public).
func (s *Server) resolveGitHubToken(ctx context.Context, projectID string) (installID, token string, err error) {
	if projectID == "" {
		return "public", "", nil
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return "", "", fmt.Errorf("failed to get project: %w", err)
	}
	if project == nil {
		return "", "", fmt.Errorf("project %s not found", projectID)
	}

	if project.GitHubInstallationID == nil {
		return "public", "", nil
	}

	mintedToken, _, err := s.mintGitHubAppToken(ctx, project)
	if err != nil {
		return "", "", fmt.Errorf("failed to mint GitHub App token: %w", err)
	}

	installID = strconv.FormatInt(*project.GitHubInstallationID, 10)
	return installID, mintedToken, nil
}

// hubGitHubRefreshTimeout bounds a background cache refresh kicked off by a
// stale hit (see resolveGitHubSkill), once detached from the request that
// triggered it. Generous enough for a commit lookup plus a contents listing
// — the Hub never downloads file bytes itself — without hanging forever if
// upstream is unresponsive.
const hubGitHubRefreshTimeout = 2 * time.Minute

// ghRefreshFailureBackoff bounds how often a background stale-refresh is
// retried for the same cache key after it fails. Without this, a
// persistently failing ref (rate limit, outage) would start a brand new
// refresh attempt on every single stale hit, while silently continuing to
// serve the stale value regardless.
const ghRefreshFailureBackoff = 1 * time.Minute

// recentGHRefreshFailure reports whether a background refresh for cacheKey
// failed within the last ghRefreshFailureBackoff.
func (s *Server) recentGHRefreshFailure(cacheKey string) bool {
	s.ghRefreshFailMu.Lock()
	defer s.ghRefreshFailMu.Unlock()
	t, ok := s.ghLastRefreshFailure[cacheKey]
	return ok && time.Since(t) < ghRefreshFailureBackoff
}

func (s *Server) recordGHRefreshFailure(cacheKey string) {
	s.ghRefreshFailMu.Lock()
	defer s.ghRefreshFailMu.Unlock()
	if s.ghLastRefreshFailure == nil {
		s.ghLastRefreshFailure = make(map[string]time.Time)
	}
	s.ghLastRefreshFailure[cacheKey] = time.Now()
}

func (s *Server) clearGHRefreshFailure(cacheKey string) {
	s.ghRefreshFailMu.Lock()
	defer s.ghRefreshFailMu.Unlock()
	delete(s.ghLastRefreshFailure, cacheKey)
}

// ghSHAMemo is a mutex-guarded (owner,repo,ref,tokenScope) → commitSHA memo,
// shared across every gh:// URI in one handleSkillsResolve call (see
// resolveGitHubSkill and fetchAndCacheGitHubSkill) to avoid redundant
// commits/{ref} lookups for URIs that share the same tuple.
//
// It must tolerate concurrent access even though handleSkillsResolve's loop
// itself calls resolveGitHubSkill one URI at a time: a flight is detached
// (see fetchAndCacheGitHubSkill's caller) and so can still be running on its
// own goroutine after its own caller's ctx has ended and resolveGitHubSkill
// has already moved on — at which point resolveGitHubSkill's own ctx.Err()
// guard stops any *new* flight for that request from starting, but it cannot
// retroactively stop one that is already in flight from finishing. A plain
// map here would then be one flight's in-progress write racing nothing
// *else* under correct code, but a mutex costs nothing on the hot path and
// removes any dependence on that guard alone being sufficient — including
// against a future change that calls resolveGitHubSkill for several URIs in
// parallel.
//
// A nil *ghSHAMemo is valid and disables memoisation (treated as always-miss
// on get, and set is a no-op), exactly like a nil map did before.
type ghSHAMemo struct {
	mu sync.Mutex
	m  map[string]string
}

func newGHSHAMemo() *ghSHAMemo {
	return &ghSHAMemo{m: make(map[string]string)}
}

func (g *ghSHAMemo) get(key string) (string, bool) {
	if g == nil {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.m[key]
	return v, ok
}

func (g *ghSHAMemo) set(key, value string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.m[key] = value
}

// ghFlightJoinHook, when non-nil, is called immediately before every caller —
// leader and followers alike — calls ghResolveFlight.DoChan for cacheKey.
// Tests use it to know precisely when a second (or later) caller has reached
// the point of joining an in-flight resolution, without polling or sleeping:
// the first invocation for a key is the caller that will become the flight
// leader; any later invocation for the same key, made while that leader's
// call is still outstanding, is a caller that will join it as a follower.
//
// Held in an atomic.Pointer, not a plain var, for the same reason as the
// broker's flightJoinHook (github_resolution_cache.go): a background refresh
// goroutine started by one test can still be running when that test returns
// and a later test installs its own hook, and reading/writing a plain var
// across those two goroutines with no synchronization is a data race.
var ghFlightJoinHook atomic.Pointer[func(string)]

func injectGHFlightJoin(cacheKey string) {
	if hook := ghFlightJoinHook.Load(); hook != nil {
		(*hook)(cacheKey)
	}
}

// ghStaleServeHook, when non-nil, is called synchronously each time
// resolveGitHubSkill serves a stale entry, with the cache key and whether a
// background refresh was started. Tests use it to assert that no refresh was
// started without waiting for one. Atomic for the same reason as
// ghFlightJoinHook.
var ghStaleServeHook atomic.Pointer[func(cacheKey string, refreshStarted bool)]

func injectGHStaleServe(cacheKey string, refreshStarted bool) {
	if hook := ghStaleServeHook.Load(); hook != nil {
		(*hook)(cacheKey, refreshStarted)
	}
}

// githubCooldown returns the rate-limit cooldown tracker for gh://
// resolution: s.ghCooldown when set (tests), else the process-wide tracker
// shared with the broker-side resolver.
func (s *Server) githubCooldown() *agent.GitHubCooldown {
	if s.ghCooldown != nil {
		return s.ghCooldown
	}
	return agent.SharedGitHubCooldown()
}

// resolveGitHubSkill resolves a gh:// skill URI via the Hub's GitHub resolution cache.
// This method is called by handleSkillsResolve for gh:// URIs. It:
//  1. Parses the gh:// URI
//  2. Determines the token scope (GitHub App installation ID or "public")
//  3. Checks the DB-backed resolution cache
//  4. On a fresh hit, returns it directly; on a stale hit (branch ref, past
//     TTL but within agent.MaxResolutionStaleAge), returns the stale value and
//     refreshes in the background
//  5. Otherwise calls the GitHub API to resolve commit SHA and file list,
//     coalescing concurrent callers for the same cache key into one call
//  6. Stores the result in the cache and returns it
//
// While the credential identity (the GitHub App installation, or anonymous)
// is in a rate-limit cooldown (see agent.GitHubCooldown), a fresh or stale
// cache entry is still served, but no background refresh is started, and a
// miss fails at once with an *agent.GitHubRateLimitError naming the ref
// instead of sending a request.
//
// refSHAMemo is a per-request memo keyed by "(owner)/(repo)@(ref):(tokenScope)"
// that is shared across all URIs in one handleSkillsResolve call. It prevents
// redundant commits/{ref} API lookups for URIs that share the same tuple. Pass
// a non-nil *ghSHAMemo to enable memoisation; nil disables it (treated as
// always-miss).
func (s *Server) resolveGitHubSkill(ctx context.Context, rawURI, projectID string, refSHAMemo *ghSHAMemo) (*ResolvedSkillResponse, error) {
	// A caller whose context has already ended must not start a new flight:
	// handleSkillsResolve's loop keeps going to the next gh:// URI after a
	// per-URI error (including this one), on the same ctx and the same
	// refSHAMemo, regardless of why the previous URI failed. Without this
	// check, a request cancelled partway through a batch could start a fresh
	// detached flight — up to the full ceiling — for every URI still left in
	// the batch, for a caller that has already gone.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 1. Parse gh:// URI
	ghRef, err := agent.ParseGitHubSkillURI(rawURI)
	if err != nil {
		return nil, fmt.Errorf("invalid gh:// URI: %w", err)
	}

	// gh:// URIs with ?token= name a ProvisionCredentials secret that lives on
	// the broker, not the Hub. Resolving here would silently substitute the
	// project's GitHub App token and hand back raw.githubusercontent.com URLs
	// the broker cannot authenticate. Return an error so the per-URI fallback
	// routes these to the local resolver, which looks up the named secret.
	if ghRef.TokenSecretName != "" {
		return nil, fmt.Errorf("gh:// URI with ?token= must be resolved by the local resolver")
	}

	// Default an omitted ref to HEAD, matching the local resolver
	// (github_skill_resolver.go resolveCommitSHA). Doing this before the cache
	// key is computed also means gh://o/r/p and gh://o/r/p@HEAD share one
	// entry rather than each missing the other's.
	if ghRef.Ref == "" {
		ghRef.Ref = "HEAD"
	}
	// Commit-SHA refs are immutable, so staleness (4, below) has no meaning
	// for them: they are only ever served fresh or re-resolved.
	isBranchRef := !isFullCommitSHA(ghRef.Ref)

	// 2. Determine token scope
	installID, token, err := s.resolveGitHubToken(ctx, projectID)
	if err != nil {
		return nil, err
	}

	// 3. Compute cache key
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, installID)
	cooldownID := agent.GitHubCooldownIdentityForInstallation(installID)

	// 4. Check cache
	if s.ghResolutionStore != nil {
		entry, hit, err := s.ghResolutionStore.Get(ctx, cacheKey)
		if err != nil {
			slog.WarnContext(ctx, "github_resolution_cache: cache lookup failed",
				"uri", rawURI, "error", err)
		} else if hit {
			slog.InfoContext(ctx, "github_resolution_cache: cache hit",
				"uri", rawURI, "commit_sha", safeShortSHA(entry.CommitSHA), "cache_hit", true)
			return buildResolvedSkillResponse(ghRef, entry), nil
		} else if isBranchRef {
			stale, ok, staleErr := s.ghResolutionStore.GetStale(ctx, cacheKey, agent.DefaultResolutionCacheTTL, agent.MaxResolutionStaleAge)
			if staleErr != nil {
				slog.WarnContext(ctx, "github_resolution_cache: stale lookup failed",
					"uri", rawURI, "error", staleErr)
			} else if ok {
				refreshStarted := false
				if _, cooling := s.githubCooldown().Active(cooldownID); cooling {
					slog.WarnContext(ctx, "github_resolution_cache: serving stale entry, skipping refresh during a rate-limit cooldown",
						"uri", rawURI, "commit_sha", safeShortSHA(stale.CommitSHA))
				} else if s.recentGHRefreshFailure(cacheKey) {
					slog.WarnContext(ctx, "github_resolution_cache: serving stale entry, skipping refresh after a recent failure",
						"uri", rawURI, "commit_sha", safeShortSHA(stale.CommitSHA))
				} else {
					slog.InfoContext(ctx, "github_resolution_cache: serving stale entry, refreshing in background",
						"uri", rawURI, "commit_sha", safeShortSHA(stale.CommitSHA))
					refreshStarted = true
					go s.refreshGitHubSkillInBackground(cacheKey, rawURI, ghRef, token, installID, isBranchRef)
				}
				injectGHStaleServe(cacheKey, refreshStarted)
				return buildResolvedSkillResponse(ghRef, stale), nil
			}
		}
	}

	// A ref GitHub reported as not found for this cache key within the last
	// ghFailureCacheTTL fails again now, without asking GitHub. A fresh or
	// stale entry above still wins.
	if ferr := s.ghFailures.recent(cacheKey); ferr != nil {
		slog.DebugContext(ctx, "github_resolution_cache: returning remembered not found", "uri", rawURI)
		return nil, ferr
	}

	// A miss during a rate-limit cooldown fails now, without starting a
	// flight: no request could be sent for this identity anyway.
	if retryAt, cooling := s.githubCooldown().Active(cooldownID); cooling {
		return nil, &agent.GitHubRateLimitError{Ref: rawURI, RetryAt: retryAt, Unauthenticated: agent.GitHubCooldownIdentityIsAnonymous(cooldownID)}
	}

	// 5. Cache miss, with no usable stale entry: coalesce concurrent misses
	// for this exact cache key into a single mint+commits+contents+Put
	// sequence, so a burst hitting a cold or just-expired-past-staleness
	// entry for the same ref does not send one request per caller to GitHub.
	//
	// Every caller — leader and followers alike — waits via DoChan and a
	// select on its own ctx: a caller whose own context is done returns
	// ctx.Err() immediately rather than blocking for the whole flight. The
	// flight itself runs on a context detached from any one caller's
	// cancellation (so the leader's own context ending does not fail the
	// others, or skip the cache write), bounded only by the fixed
	// hubGitHubRefreshTimeout ceiling — see the comment at that bound below
	// for why it is not derived from any one caller's deadline.
	injectGHFlightJoin(cacheKey)
	resultCh := s.ghResolveFlight.DoChan(cacheKey, func() (result interface{}, ferr error) {
		// DoChan always runs this function in a goroutine it spawns itself,
		// never the calling goroutine (see golang.org/x/sync/singleflight) —
		// unlike Do, there is no caller stack frame to recover a panic in. A
		// panic here otherwise crashes the process outright (singleflight
		// deliberately makes it unrecoverable once there is a channel
		// waiter). Recovering here, inside the function singleflight runs,
		// converts it into a normal error instead, delivered to every waiter
		// through resultCh like any other failure.
		defer func() {
			if r := recover(); r != nil {
				ferr = fmt.Errorf("panic during GitHub skill resolution for %s: %v", cacheKey, r)
			}
		}()

		// Bounded by the fixed ceiling only, not by the leader's own
		// deadline: every waiter (including the leader) already returns on
		// its own ctx.Done() via the select below, so no caller can wait
		// past its own deadline regardless of this bound. Deriving the bound
		// from the leader's deadline instead would fail every waiter with
		// that leader's own "context deadline exceeded" the moment it
		// expired — including waiters with no deadline, or a later one — the
		// exact starvation this flight exists to prevent.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubGitHubRefreshTimeout)
		defer cancel()

		// Re-check: a concurrent flight for this exact key may have already
		// landed while this call waited to become the flight leader. Uses
		// flightCtx, not the leader's own ctx: DoChan can run this closure
		// some time after the call that started the flight, and if that
		// caller's own ctx had already ended by then, a Get keyed to it would
		// fail outright and fall through to a redundant fetch — the detached
		// flightCtx exists precisely so this flight never depends on any one
		// caller's own context staying alive.
		if s.ghResolutionStore != nil {
			if entry, hit, gerr := s.ghResolutionStore.Get(flightCtx, cacheKey); gerr == nil && hit {
				return entry, nil
			}
		}
		if ferr := s.ghFailures.recent(cacheKey); ferr != nil {
			slog.DebugContext(ctx, "github_resolution_cache: returning remembered not found", "uri", rawURI)
			return nil, ferr
		}

		return s.fetchAndCacheGitHubSkill(flightCtx, cacheKey, rawURI, ghRef, token, installID, isBranchRef, refSHAMemo)
	})

	select {
	case res := <-resultCh:
		if res.Err != nil {
			var rl *agent.GitHubRateLimitError
			if errors.As(res.Err, &rl) {
				return nil, rl.WithRef(rawURI)
			}
			return nil, res.Err
		}
		// Build the response from this caller's own ghRef, not whichever
		// caller happened to lead or already have the result cached: the
		// flight (and the cache re-check above) share one *GitHubCacheEntry
		// across every caller keyed to cacheKey, but two callers can reach
		// the same cacheKey with different raw URI text (a bare ref vs
		// "@HEAD", or different owner/repo letter case) — the entry carries
		// none of that, so each caller supplies it from its own parsed ghRef.
		return buildResolvedSkillResponse(ghRef, res.Val.(*GitHubCacheEntry)), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// refreshGitHubSkillInBackground re-resolves ghRef and updates the cache on
// behalf of a caller that was already served a stale value (see
// resolveGitHubSkill, step 4). It runs detached from any specific request —
// the stale caller has already returned — on a bounded timeout, and shares
// ghResolveFlight's key with the synchronous miss path so a burst of stale
// hits for the same ref collapses into one refresh. installID is the same
// value resolveGitHubSkill resolved for this request, passed through so the
// refreshed entry's TokenScope is preserved rather than overwritten with "".
func (s *Server) refreshGitHubSkillInBackground(cacheKey, rawURI string, ghRef *agent.GitHubSkillRef, token, installID string, isBranchRef bool) {
	injectGHFlightJoin(cacheKey)
	ctx, cancel := context.WithTimeout(context.Background(), hubGitHubRefreshTimeout)
	defer cancel()

	// A panic in fetchAndCacheGitHubSkill is recovered inside the DoChan
	// closure below (see its comment), so this goroutine itself cannot panic
	// from that; no recover needed at this level.
	resultCh := s.ghResolveFlight.DoChan(cacheKey, func() (result interface{}, ferr error) {
		defer func() {
			if r := recover(); r != nil {
				ferr = fmt.Errorf("panic during background GitHub skill refresh for %s: %v", cacheKey, r)
			}
		}()
		if entry, hit, gerr := s.ghResolutionStore.Get(ctx, cacheKey); gerr == nil && hit {
			return entry, nil
		}
		return s.fetchAndCacheGitHubSkill(ctx, cacheKey, rawURI, ghRef, token, installID, isBranchRef, nil)
	})

	select {
	case res := <-resultCh:
		if res.Err != nil {
			s.recordGHRefreshFailure(cacheKey)
			slog.WarnContext(ctx, "github_resolution_cache: background refresh failed",
				"uri", rawURI, "error", res.Err)
		} else {
			s.clearGHRefreshFailure(cacheKey)
		}
	case <-ctx.Done():
		s.recordGHRefreshFailure(cacheKey)
		slog.WarnContext(ctx, "github_resolution_cache: background refresh timed out",
			"uri", rawURI, "error", ctx.Err())
	}
}

// fetchAndCacheGitHubSkill resolves ghRef against the GitHub API (commit SHA,
// then directory contents), stores the result in the resolution cache under
// cacheKey, and returns the stored entry. installID is recorded on the cache
// entry's TokenScope and must be the same value the triggering request
// resolved via resolveGitHubToken — both the synchronous path and a
// background refresh pass it through explicitly, so a refresh can never
// overwrite an existing row's TokenScope with an empty value.
//
// The return value is the cache entry, not a *ResolvedSkillResponse: a
// response is built from a specific caller's own ghRef (see
// buildResolvedSkillResponse and its callers), and this result is shared, via
// the flight, by every caller sharing cacheKey — which can include callers
// whose raw URI text differs (a bare ref vs "@HEAD", or owner/repo letter
// case) even though they compute the same cacheKey.
//
// Called from within s.ghResolveFlight.DoChan, so concurrent callers sharing
// cacheKey share one execution — but refSHAMemo is also shared by every
// flight in one handleSkillsResolve batch (one per distinct cacheKey), and a
// detached flight can still be running after its own caller's ctx has ended
// and resolveGitHubSkill has moved on to the next URI, so refSHAMemo must
// tolerate concurrent access from more than one flight's goroutine — see
// ghSHAMemo.
func (s *Server) fetchAndCacheGitHubSkill(
	ctx context.Context,
	cacheKey, rawURI string,
	ghRef *agent.GitHubSkillRef,
	token, installID string,
	isBranchRef bool,
	refSHAMemo *ghSHAMemo,
) (*GitHubCacheEntry, error) {
	apiBase := githubAPIBase
	if s.config.GitHubAppConfig.APIBaseURL != "" {
		apiBase = s.config.GitHubAppConfig.APIBaseURL
	}
	rawBase := githubRawBase
	if s.config.GitHubAppConfig.RawBaseURL != "" {
		rawBase = s.config.GitHubAppConfig.RawBaseURL
	}

	// Resolve ref → commit SHA, reusing a prior lookup if this (owner, repo,
	// ref, tokenScope) tuple was already resolved within this request.
	// The separators "/" "@" ":" are safe for GitHub names: owner/repo names
	// cannot contain "@" or ":", and Git ref names cannot contain ":". The
	// installID suffix is the same for every URI in one request (shared
	// projectID → shared installation) but is included for forward-safety.
	cooldown := s.githubCooldown()
	cooldownID := agent.GitHubCooldownIdentityForInstallation(installID)

	memoKey := strings.ToLower(ghRef.Owner) + "/" + strings.ToLower(ghRef.Repo) + "@" + ghRef.Ref + ":" + installID
	commitSHA, seen := refSHAMemo.get(memoKey)
	if !seen {
		var err error
		commitSHA, err = ghResolveCommitSHA(ctx, cooldown, cooldownID, apiBase, ghRef.Owner, ghRef.Repo, ghRef.Ref, token)
		if err != nil {
			err = fmt.Errorf("failed to resolve commit SHA: %w", err)
			s.rememberGHNotFound(cacheKey, err)
			return nil, err
		}
		refSHAMemo.set(memoKey, commitSHA)
	}

	fileEntries, err := ghListContents(ctx, cooldown, cooldownID, apiBase, rawBase, ghRef.Owner, ghRef.Repo, ghRef.SkillPath, commitSHA, token)
	if err != nil {
		err = fmt.Errorf("failed to list contents: %w", err)
		s.rememberGHNotFound(cacheKey, err)
		return nil, err
	}

	if len(fileEntries) == 0 {
		return nil, fmt.Errorf("no files found at %s in %s/%s", ghRef.SkillPath, ghRef.Owner, ghRef.Repo)
	}

	// Compute bundle hash
	bundleHash := computeBundleHash(fileEntries)

	// Determine TTL based on ref type
	var ttl time.Duration
	if isBranchRef {
		ttl = agent.DefaultResolutionCacheTTL
	} else {
		ttl = agent.DefaultSHAResolutionCacheTTL
	}

	// Store in cache. The TTL is jittered (see agent.JitteredTTL) so a burst
	// of creates that all populate the cache at once — the common case this
	// cache exists to absorb — do not all expire at exactly the same instant
	// and stampede GitHub again together.
	entry := GitHubCacheEntry{
		CommitSHA:   commitSHA,
		FileEntries: fileEntries,
		BundleHash:  bundleHash,
		TokenScope:  installID,
		ExpiresAt:   time.Now().Add(agent.JitteredTTL(ttl, rand.Float64)),
		OriginalURI: rawURI,
	}

	// A successful resolution replaces any remembered not found for this
	// key, whether or not the store write below succeeds.
	s.ghFailures.clear(cacheKey)
	if s.ghResolutionStore != nil {
		if err := s.ghResolutionStore.Put(ctx, cacheKey, entry); err != nil {
			slog.WarnContext(ctx, "github_resolution_cache: failed to store entry",
				"uri", rawURI, "error", err)
		}
	}

	slog.InfoContext(ctx, "github_resolution_cache: cache miss, resolved via API",
		"uri", rawURI, "commit_sha", safeShortSHA(commitSHA), "files", len(fileEntries), "cache_hit", false)

	return &entry, nil
}

// buildResolvedSkillResponse constructs a ResolvedSkillResponse from a cache entry.
//
// Hash values here are git blob object IDs (bare 40-char hex), not the
// "sha256:<hex>" digests used elsewhere in the API: the Hub resolves gh://
// skills from GitHub metadata alone and never downloads the bytes, so a
// sha256 digest is not available to it. The client recognises the format and
// verifies accordingly (see hashFileAs in pkg/agent/skill_resolver.go).
// ContentHash is likewise a digest over the git blob IDs.
func buildResolvedSkillResponse(ghRef *agent.GitHubSkillRef, entry *GitHubCacheEntry) *ResolvedSkillResponse {
	files := make([]DownloadURLInfo, len(entry.FileEntries))
	for i, f := range entry.FileEntries {
		files[i] = DownloadURLInfo{
			Path: f.Path,
			URL:  f.URL,
			Size: f.Size,
			Hash: f.Hash,
		}
	}

	return &ResolvedSkillResponse{
		URI:             ghRef.Raw,
		Name:            ghRef.SkillName,
		ResolvedVersion: safeShortSHA(entry.CommitSHA), // Short SHA for display
		ContentHash:     entry.BundleHash,
		Files:           files,
	}
}

// safeShortSHA returns the first 12 characters of a SHA string, or the full
// string if it is shorter than 12 characters (e.g. in tests or after DB
// corruption). Prevents a panic on bare sha[:12] slicing.
func safeShortSHA(sha string) string {
	if len(sha) < 12 {
		return sha
	}
	return sha[:12]
}

// computeBundleHash computes the content hash from file entries.
func computeBundleHash(files []GitHubFileEntry) string {
	// Convert to transfer.FileInfo for hash computation
	fileInfos := make([]transfer.FileInfo, len(files))
	for i, f := range files {
		fileInfos[i] = transfer.FileInfo{
			Path: f.Path,
			Hash: f.Hash,
		}
	}
	return transfer.ComputeContentHash(fileInfos)
}
