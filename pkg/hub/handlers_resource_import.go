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
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ImportTemplatesRequest is the request body for direct template import.
// Exactly one of SourceURL or WorkspacePath should be provided.
type ImportTemplatesRequest struct {
	SourceURL     string   `json:"sourceUrl"`
	WorkspacePath string   `json:"workspacePath"`
	Names         []string `json:"names,omitempty"`
}

// ImportTemplatesResponse is returned after a direct template import completes.
type ImportTemplatesResponse struct {
	Templates []string `json:"templates"`
	Count     int      `json:"count"`
}

// handleProjectImportTemplates imports templates directly from a remote URL into
// the project's template store without spawning a bootstrap container agent.
func (s *Server) handleProjectImportTemplates(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	// Authorize the caller
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:create", nil)
			return
		}
		if projectID != agentIdent.ProjectID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only import templates within their own project", nil)
			return
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:       "agent",
			ParentType: "project",
			ParentID:   projectID,
		}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to import templates in this project", nil)
			return
		}
	} else {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}

	var req ImportTemplatesRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body", nil)
		return
	}

	if req.SourceURL == "" && req.WorkspacePath == "" {
		// Default workspace path when neither is provided
		req.WorkspacePath = "/.scion/templates"
	}

	// Verify project exists
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Template storage is not configured", nil)
		return
	}

	run := func(progress importProgressFunc) ([]string, error) {
		if req.WorkspacePath != "" {
			return s.importFromWorkspace(ctx, project, req.WorkspacePath, store.TemplateScopeProject, s.templateImportKind(), progress, req.Names)
		}
		req.SourceURL = config.NormalizeTemplateSourceURL(req.SourceURL)
		return s.importFromRemote(ctx, projectID, req.SourceURL, store.TemplateScopeProject, s.templateImportKind(), progress, req.Names)
	}

	if importAcceptsNDJSON(r) {
		s.streamImport(w, run)
		return
	}

	imported, err := run(nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "import_failed", err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, ImportTemplatesResponse{
		Templates: imported,
		Count:     len(imported),
	})
}

// ImportHarnessConfigsRequest is the request body for direct harness-config import.
// Exactly one of SourceURL or WorkspacePath should be provided.
type ImportHarnessConfigsRequest struct {
	SourceURL     string   `json:"sourceUrl"`
	WorkspacePath string   `json:"workspacePath"`
	Names         []string `json:"names,omitempty"`
}

// ImportHarnessConfigsResponse is returned after a direct harness-config import completes.
type ImportHarnessConfigsResponse struct {
	HarnessConfigs []string `json:"harnessConfigs"`
	Count          int      `json:"count"`
}

// handleProjectImportHarnessConfigs imports harness-configs directly from a
// remote URL or workspace path into the project's harness-config store.
func (s *Server) handleProjectImportHarnessConfigs(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:create", nil)
			return
		}
		if projectID != agentIdent.ProjectID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only import harness-configs within their own project", nil)
			return
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:       "harness_config",
			ParentType: "project",
			ParentID:   projectID,
		}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to import harness-configs in this project", nil)
			return
		}
	} else {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Authentication required", nil)
		return
	}

	var req ImportHarnessConfigsRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	if req.SourceURL == "" && req.WorkspacePath == "" {
		req.WorkspacePath = "/.scion/harness-configs"
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Harness-config storage is not configured", nil)
		return
	}

	run := func(progress importProgressFunc) ([]string, error) {
		if req.WorkspacePath != "" {
			return s.importFromWorkspace(ctx, project, req.WorkspacePath, store.HarnessConfigScopeProject, s.harnessConfigImportKind(), progress, req.Names)
		}
		req.SourceURL = config.NormalizeTemplateSourceURL(req.SourceURL)
		return s.importFromRemote(ctx, projectID, req.SourceURL, store.HarnessConfigScopeProject, s.harnessConfigImportKind(), progress, req.Names)
	}

	if importAcceptsNDJSON(r) {
		s.streamImport(w, run)
		return
	}

	imported, err := run(nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "import_failed", err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, ImportHarnessConfigsResponse{
		HarnessConfigs: imported,
		Count:          len(imported),
	})
}

// ImportResourcesRequest is the body for the unified import endpoint
// (POST /api/v1/resources/import). It imports a single kind of resource from a
// remote source URL into the given scope.
type ImportResourcesRequest struct {
	// Kind is the resource kind: "template" or "harness-config".
	Kind string `json:"kind"`
	// Scope is "global" (hub-level) or "project".
	Scope string `json:"scope"`
	// ScopeID is the project id for project scope; empty for global scope.
	ScopeID string `json:"scopeId"`
	// SourceURL is the remote URL to import from. Workspace-path import is not
	// available on this endpoint (see the per-project endpoints for that).
	SourceURL string `json:"sourceUrl"`
	// Names optionally restricts which discovered resources to import.
	Names []string `json:"names,omitempty"`
}

// ImportResourcesResponse reports the result of a unified import.
type ImportResourcesResponse struct {
	Kind     string   `json:"kind"`
	Imported []string `json:"imported"`
	Count    int      `json:"count"`
}

// handleResourcesImport handles POST /api/v1/resources/import: a single,
// kind/scope-generic import endpoint sitting over the shared import driver
// (resource_import.go). Global-scope import requires hub-admin; project-scope
// import requires create access in the target project. URL is the only source
// (no workspace mode) — matching the hub-level import design.
func (s *Server) handleResourcesImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	var req ImportResourcesRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body", nil)
		return
	}

	// Resolve the kind knobs and the authz resource type for the kind.
	var kind resourceImportKind
	var authzType string
	switch storage.ResourceKind(req.Kind) {
	case storage.ResourceKindTemplate:
		kind = s.templateImportKind()
		authzType = "template"
	case storage.ResourceKindHarnessConfig:
		kind = s.harnessConfigImportKind()
		authzType = "harness_config"
	default:
		writeError(w, http.StatusBadRequest, "invalid_request",
			"kind must be 'template' or 'harness-config'", nil)
		return
	}

	if req.SourceURL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "sourceUrl is required", nil)
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable",
			"Resource storage is not configured", nil)
		return
	}

	sourceURL := config.NormalizeTemplateSourceURL(req.SourceURL)

	// Resolve scope-specific authz and bind the import call. All pre-flight
	// checks (authz, project existence) run here, before any response is
	// committed, so they can still return proper HTTP status codes even on the
	// streaming path.
	var projectID, scope string
	switch req.Scope {
	case "global", "":
		// Global import is hub-admin only. CheckAccess on an ownerless,
		// parentless global resource grants only on admin bypass (or an explicit
		// hub-wide policy), which is exactly the hub-admin gate we want.
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{Type: authzType}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to import global "+kind.noun, nil)
			return
		}
		projectID, scope = "", "global"

	case "project":
		if req.ScopeID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request",
				"scopeId (project id) is required for project scope", nil)
			return
		}
		if !s.authorizeProjectImport(ctx, w, req.ScopeID, kind.noun) {
			return
		}
		// Verify project exists before fetching.
		if _, perr := s.store.GetProject(ctx, req.ScopeID); perr != nil {
			if perr == store.ErrNotFound {
				NotFound(w, "Project")
				return
			}
			writeErrorFromErr(w, perr, "")
			return
		}
		projectID, scope = req.ScopeID, "project"

	default:
		writeError(w, http.StatusBadRequest, "invalid_request",
			"scope must be 'global' or 'project'", nil)
		return
	}

	run := func(progress importProgressFunc) ([]string, error) {
		return s.importFromRemote(ctx, projectID, sourceURL, scope, kind, progress, req.Names)
	}

	if importAcceptsNDJSON(r) {
		s.streamImport(w, run)
		return
	}

	imported, err := run(nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "import_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, ImportResourcesResponse{
		Kind:     req.Kind,
		Imported: imported,
		Count:    len(imported),
	})
}

// importAcceptsNDJSON reports whether the client opted into a streaming
// per-resource progress response via the Accept header.
func importAcceptsNDJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/x-ndjson")
}

// streamImport runs an import that may emit progress events, streaming them to
// the client as newline-delimited JSON (NDJSON). It writes a 200 and the stream
// headers up front, so per-resource and fetch errors are reported as an `error`
// event in-band rather than via HTTP status (the caller must do all pre-flight
// validation/authz before calling this). Events are serialized through a mutex
// so they remain correct once the per-resource loop is parallelized (Phase 4).
func (s *Server) streamImport(w http.ResponseWriter, run func(progress importProgressFunc) ([]string, error)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming not supported", nil)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var mu sync.Mutex
	enc := json.NewEncoder(w)
	progress := func(ev ResourceImportEvent) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(ev) // Encode appends a newline → NDJSON framing.
		flusher.Flush()
	}

	if _, err := run(progress); err != nil {
		// The import failed before reaching the per-resource phase (e.g. fetch
		// failure or nothing found); report it in-band since the status line is
		// already committed.
		progress(ResourceImportEvent{Type: ImportEventError, Reason: err.Error()})
	}
}

// authorizeProjectImport checks that the caller may import resources into the
// given project, mirroring the per-project import handlers. It writes the error
// response and returns false when access is denied.
func (s *Server) authorizeProjectImport(ctx context.Context, w http.ResponseWriter, projectID, noun string) bool {
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:create", nil)
			return false
		}
		if projectID != agentIdent.ProjectID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only import "+noun+" within their own project", nil)
			return false
		}
		return true
	}
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:       "agent",
			ParentType: "project",
			ParentID:   projectID,
		}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to import "+noun+" in this project", nil)
			return false
		}
		return true
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
	return false
}

// DiscoverResourcesRequest is the body for discover endpoints. Exactly one of
// SourceURL or WorkspacePath should be provided.
type DiscoverResourcesRequest struct {
	SourceURL     string `json:"sourceUrl"`
	WorkspacePath string `json:"workspacePath"`
}

// DiscoverResourcesResponse lists the resources found at the given source.
type DiscoverResourcesResponse struct {
	Resources []string `json:"resources"`
	Skipped   []string `json:"skipped,omitempty"`
	Count     int      `json:"count"`
}

// handleProjectDiscoverTemplates handles POST /api/v1/projects/{id}/discover-templates:
// discovers templates at a remote URL or workspace path without importing them.
func (s *Server) handleProjectDiscoverTemplates(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:create", nil)
			return
		}
		if projectID != agentIdent.ProjectID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only discover templates within their own project", nil)
			return
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:       "agent",
			ParentType: "project",
			ParentID:   projectID,
		}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to discover templates in this project", nil)
			return
		}
	} else {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Authentication required", nil)
		return
	}

	var req DiscoverResourcesRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	if req.SourceURL == "" && req.WorkspacePath == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "sourceUrl or workspacePath is required", nil)
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Template storage is not configured", nil)
		return
	}

	var names, skipped []string
	if req.WorkspacePath != "" {
		names, skipped, err = s.discoverFromWorkspace(ctx, project, req.WorkspacePath, s.templateImportKind())
	} else {
		req.SourceURL = config.NormalizeTemplateSourceURL(req.SourceURL)
		names, skipped, err = s.discoverFromRemote(ctx, projectID, req.SourceURL, s.templateImportKind())
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "discover_failed", err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, DiscoverResourcesResponse{
		Resources: names,
		Skipped:   skipped,
		Count:     len(names),
	})
}

// handleProjectDiscoverHarnessConfigs handles POST /api/v1/projects/{id}/discover-harness-configs:
// discovers harness-configs at a remote URL or workspace path without importing them.
func (s *Server) handleProjectDiscoverHarnessConfigs(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentCreate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: project:agent:create", nil)
			return
		}
		if projectID != agentIdent.ProjectID() {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only discover harness-configs within their own project", nil)
			return
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:       "harness_config",
			ParentType: "project",
			ParentID:   projectID,
		}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to discover harness-configs in this project", nil)
			return
		}
	} else {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Authentication required", nil)
		return
	}

	var req DiscoverResourcesRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	if req.SourceURL == "" && req.WorkspacePath == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "sourceUrl or workspacePath is required", nil)
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "Harness-config storage is not configured", nil)
		return
	}

	var names, skipped []string
	if req.WorkspacePath != "" {
		names, skipped, err = s.discoverFromWorkspace(ctx, project, req.WorkspacePath, s.harnessConfigImportKind())
	} else {
		req.SourceURL = config.NormalizeTemplateSourceURL(req.SourceURL)
		names, skipped, err = s.discoverFromRemote(ctx, projectID, req.SourceURL, s.harnessConfigImportKind())
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "discover_failed", err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, DiscoverResourcesResponse{
		Resources: names,
		Skipped:   skipped,
		Count:     len(names),
	})
}

// DiscoverResourcesUnifiedRequest is the body for the unified discover endpoint
// (POST /api/v1/resources/discover).
type DiscoverResourcesUnifiedRequest struct {
	Kind      string `json:"kind"`
	Scope     string `json:"scope"`
	ScopeID   string `json:"scopeId"`
	SourceURL string `json:"sourceUrl"`
}

// handleResourcesDiscover handles POST /api/v1/resources/discover: a unified,
// kind/scope-generic discover endpoint that returns discovered resource names
// without importing them.
func (s *Server) handleResourcesDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	var req DiscoverResourcesUnifiedRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body", nil)
		return
	}

	var kind resourceImportKind
	var authzType string
	switch storage.ResourceKind(req.Kind) {
	case storage.ResourceKindTemplate:
		kind = s.templateImportKind()
		authzType = "template"
	case storage.ResourceKindHarnessConfig:
		kind = s.harnessConfigImportKind()
		authzType = "harness_config"
	default:
		writeError(w, http.StatusBadRequest, "invalid_request",
			"kind must be 'template' or 'harness-config'", nil)
		return
	}

	if req.SourceURL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "sourceUrl is required", nil)
		return
	}

	if s.GetStorage() == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable",
			"Resource storage is not configured", nil)
		return
	}

	sourceURL := config.NormalizeTemplateSourceURL(req.SourceURL)

	var projectID string
	switch req.Scope {
	case "global", "":
		userIdent := GetUserIdentityFromContext(ctx)
		if userIdent == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
			return
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{Type: authzType}, ActionCreate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You don't have permission to discover global "+kind.noun, nil)
			return
		}
		projectID = ""

	case "project":
		if req.ScopeID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request",
				"scopeId (project id) is required for project scope", nil)
			return
		}
		if !s.authorizeProjectImport(ctx, w, req.ScopeID, kind.noun) {
			return
		}
		if _, perr := s.store.GetProject(ctx, req.ScopeID); perr != nil {
			if perr == store.ErrNotFound {
				NotFound(w, "Project")
				return
			}
			writeErrorFromErr(w, perr, "")
			return
		}
		projectID = req.ScopeID

	default:
		writeError(w, http.StatusBadRequest, "invalid_request",
			"scope must be 'global' or 'project'", nil)
		return
	}

	names, skipped, err := s.discoverFromRemote(ctx, projectID, sourceURL, kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, "discover_failed", err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, DiscoverResourcesResponse{
		Resources: names,
		Skipped:   skipped,
		Count:     len(names),
	})
}

// handleMessageChannels handles GET /api/v1/message-channels.
//
//nolint:unused // Kept for legacy message channel route compatibility.
func (s *Server) handleMessageChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", nil)
		return
	}

	type channelInfo struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Observer bool   `json:"observer,omitempty"`
	}

	bp := s.GetMessageBrokerProxy()
	if bp == nil {
		writeJSON(w, http.StatusOK, map[string]any{"channels": []channelInfo{}})
		return
	}

	channels := bp.ListChannels()
	result := make([]channelInfo, 0, len(channels))
	for _, ch := range channels {
		result = append(result, channelInfo{
			Name:     ch.Name,
			Status:   "registered",
			Observer: ch.Observer,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"channels": result})
}
