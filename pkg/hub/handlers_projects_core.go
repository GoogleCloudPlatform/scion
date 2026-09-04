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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type ListProjectsResponse struct {
	Projects     []ProjectWithCapabilities `json:"projects"`
	LegacyGroves []ProjectWithCapabilities `json:"groves,omitempty"`
	NextCursor   string                    `json:"nextCursor,omitempty"`
	TotalCount   int                       `json:"totalCount"`
	Capabilities *Capabilities             `json:"_capabilities,omitempty"`
}

type CreateProjectRequest struct {
	ID            string            `json:"id,omitempty"`
	Slug          string            `json:"slug,omitempty"`
	Name          string            `json:"name"`
	GitRemote     string            `json:"gitRemote,omitempty"`
	WorkspaceMode string            `json:"workspaceMode,omitempty"` // "shared", "worktree-per-agent", or "per-agent" (default); only meaningful when gitRemote is set
	Labels        map[string]string `json:"labels,omitempty"`
	GitHubToken   string            `json:"githubToken,omitempty"`
}

type RegisterProjectRequest struct {
	ID        string                     `json:"id,omitempty"` // Client-provided project ID
	Name      string                     `json:"name"`
	GitRemote string                     `json:"gitRemote"`
	Path      string                     `json:"path,omitempty"`
	BrokerID  string                     `json:"brokerId,omitempty"` // Link to existing broker (two-phase flow)
	Broker    *RegisterProjectBrokerInfo `json:"broker,omitempty"`   // DEPRECATED: Use BrokerID with two-phase registration
	Profiles  []string                   `json:"profiles,omitempty"`
	Labels    map[string]string          `json:"labels,omitempty"`
}

// UnmarshalJSON accepts legacy grove ID aliases at the Hub JSON adapter boundary.
func (r *RegisterProjectRequest) UnmarshalJSON(data []byte) error {
	type Alias RegisterProjectRequest
	aux := &struct {
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ID == "" {
		legacyID, err := legacyProjectIDFromJSON(data)
		if err != nil {
			return err
		}
		r.ID = legacyID
	}
	return nil
}

type RegisterProjectBrokerInfo struct {
	ID           string                    `json:"id,omitempty"`
	Name         string                    `json:"name"`
	Version      string                    `json:"version,omitempty"`
	Capabilities *store.BrokerCapabilities `json:"capabilities,omitempty"`
	Profiles     []store.BrokerProfile     `json:"profiles,omitempty"`
}

type RegisterProjectResponse struct {
	Project       *store.Project           `json:"project"`
	LegacyProject *store.Project           `json:"grove,omitempty"`
	Broker        *store.RuntimeBroker     `json:"broker,omitempty"`
	Created       bool                     `json:"created"`
	Matches       []hubclient.ProjectMatch `json:"matches,omitempty"`     // Populated when multiple projects share the same git remote
	BrokerToken   string                   `json:"brokerToken,omitempty"` // DEPRECATED: use two-phase registration
	SecretKey     string                   `json:"secretKey,omitempty"`   // DEPRECATED: secrets only from /brokers/join
}

// AddProviderRequest is the request for adding a broker as a project provider.
type AddProviderRequest struct {
	BrokerID  string `json:"brokerId"`
	LocalPath string `json:"localPath,omitempty"`
}

// AddProviderResponse is the response after adding a provider.
type AddProviderResponse struct {
	Provider *store.ProjectProvider `json:"provider"`
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listProjects(w, r)
	case http.MethodPost:
		s.createProject(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	query := r.URL.Query()

	filter := store.ProjectFilter{
		OwnerID:   query.Get("ownerId"),
		GitRemote: util.NormalizeGitRemote(query.Get("gitRemote")),
		BrokerID:  query.Get("brokerId"),
		Name:      query.Get("name"),
		Slug:      query.Get("slug"),
	}

	// Template filtering: default to excluding template projects.
	// ?isTemplate=true returns only templates; absent or false excludes them.
	switch query.Get("isTemplate") {
	case "true":
		isTemplate := true
		filter.IsTemplate = &isTemplate
	case "false", "":
		isTemplate := false
		filter.IsTemplate = &isTemplate
	default:
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"isTemplate must be 'true' or 'false'", nil)
		return
	}

	// scope=mine: projects the current user owns
	// scope=shared: projects where the user is a member/admin but not the owner
	// mine=true (legacy): projects the user owns or is a member of
	switch query.Get("scope") {
	case "mine":
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			filter.OwnerID = userIdent.ID()
		}
	case "shared":
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			if projectIDs := s.resolveUserProjectIDs(ctx, userIdent.ID()); len(projectIDs) > 0 {
				filter.MemberProjectIDs = projectIDs
				filter.ExcludeOwnerID = userIdent.ID()
			} else {
				// User has no group memberships — return empty result
				filter.MemberProjectIDs = []string{"__none__"}
			}
		}
	default:
		// Legacy mine=true support
		if query.Get("mine") == "true" {
			if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
				filter.OwnerID = userIdent.ID()
				if projectIDs := s.resolveUserProjectIDs(ctx, userIdent.ID()); len(projectIDs) > 0 {
					filter.MemberOrOwnerIDs = projectIDs
				}
			}
		}
	}

	limit := 500
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	identity, cursor := GetIdentityFromContext(ctx), query.Get("cursor")
	cursorBinding := authorizedListCursorBinding("projects", filter)
	if cursor != "" {
		if err := validateAuthorizedListCursor(cursor, cursorBinding); err != nil {
			BadRequest(w, err.Error())
			return
		}
	}

	var items []store.Project
	var nextCursor string
	var totalCount int

	// Scope-aware list authorization: resolve the caller's authorized project
	// set from role bindings instead of using a binary admin-view check on a
	// synthetic hub resource.
	if identity == nil {
		// Unauthenticated: return empty list.
		items = []store.Project{}
	} else {
		scopes := s.authzService.ResolveListScopes(ctx, identity, "project.list")
		if !scopes.IsNone() {
			// All or explicit scope set: push authorized IDs into the store
			// query so pagination and totals reflect only the visible set.
			// For All, AuthorizedProjectIDs remains nil (no filter applied).
			if !scopes.IsAll() {
				filter.AuthorizedProjectIDs = scopes.ProjectIDs()
			}
			result, err := s.store.ListProjects(ctx, filter, store.ListOptions{Limit: limit, Cursor: cursor, CursorBinding: cursorBinding})
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			items, nextCursor, totalCount = result.Items, result.NextCursor, result.TotalCount
		} else {
			// No role bindings resolved. Fall back to per-item policy filtering
			// for backward compatibility during the transition period before
			// CO1 cutover completes. After cutover, all principals will have
			// role bindings and this path will not be reached.
			result, err := authorizedList(ctx, identity, cursor, limit, func(ctx context.Context, cursor string, limit int) (authorizedCandidatePage[store.Project], error) {
				page, err := s.store.ListProjects(ctx, filter, store.ListOptions{Limit: limit, Cursor: cursor, SkipTotalCount: true, CursorBinding: cursorBinding})
				if err != nil {
					return authorizedCandidatePage[store.Project]{}, err
				}
				return authorizedCandidatePage[store.Project]{Items: page.Items, NextCursor: page.NextCursor}, nil
			}, projectResource, func(p *store.Project) string { return authorizedListCursor(p.Created, p.ID, cursorBinding) }, s.authzService.AuthorizeReadBatch)
			if err != nil {
				writeAuthorizedListError(w, err)
				return
			}
			items, nextCursor, totalCount = result.Items, result.NextCursor, result.TotalCount
		}
	}

	// Enrich owner display names
	s.enrichProjectOwnerNames(ctx, items)

	// Compute per-item and scope capabilities for authorized items
	projects := make([]ProjectWithCapabilities, 0, len(items))
	if identity == nil {
		for i := range items {
			projects = append(projects, ProjectWithCapabilities{Project: items[i]})
		}
	} else {
		resources := make([]Resource, len(items))
		for i := range items {
			resources[i] = projectResource(&items[i])
		}
		for i, cap := range s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "project") {
			projects = append(projects, ProjectWithCapabilities{Project: items[i], Cap: cap})
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "project")
	}

	writeJSON(w, http.StatusOK, ListProjectsResponse{
		Projects:     projects,
		LegacyGroves: projects,
		NextCursor:   nextCursor,
		TotalCount:   totalCount,
		Capabilities: scopeCap,
	})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateProjectRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	if req.GitHubToken != "" {
		req.GitHubToken = strings.TrimSpace(req.GitHubToken)
		if req.GitHubToken == "" {
			ValidationError(w, "GitHub token must not be blank", nil)
			return
		}
		if len(req.GitHubToken) > 500 {
			ValidationError(w, "GitHub token exceeds maximum length", nil)
			return
		}
	}

	normalizedRemote := util.NormalizeGitRemote(req.GitRemote)

	// Idempotency: if we have a client-provided ID, check for existing project
	if req.ID != "" {
		existing, err := s.store.GetProject(ctx, req.ID)
		if err == nil {
			// Project already exists — ensure associated groups exist (backfill for
			// projects created before group support was added). Pass the caller
			// so they get added as an owner of the members group.
			var callerID string
			if user := GetUserIdentityFromContext(ctx); user != nil {
				callerID = user.ID()
			}
			s.createProjectGroup(ctx, existing)
			s.createProjectMembersGroup(ctx, existing, callerID)
			writeJSON(w, http.StatusOK, existing)
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeErrorFromErr(w, err, "")
			return
		}
		// Not found — proceed to create with this ID
	}

	projectID := req.ID
	if projectID == "" {
		projectID = api.NewUUID()
	}

	baseSlug := req.Slug
	if baseSlug == "" {
		baseSlug = api.Slugify(req.Name)
	}

	slug, err := s.store.NextAvailableSlug(ctx, baseSlug)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	displayName := req.Name
	if slug != baseSlug {
		displayName = api.DisplayNameWithSerial(req.Name, slug, baseSlug)
	}

	// Apply workspace mode label for git projects with explicit workspace mode.
	if normalizedRemote != "" {
		switch req.WorkspaceMode {
		case store.WorkspaceModeShared, store.WorkspaceModePerAgent, store.WorkspaceModeWorktreePerAgent:
			if req.Labels == nil {
				req.Labels = make(map[string]string)
			}
			req.Labels[store.LabelWorkspaceMode] = req.WorkspaceMode
		}
	}

	project := &store.Project{
		ID:        projectID,
		Name:      displayName,
		Slug:      slug,
		GitRemote: normalizedRemote,
		Labels:    req.Labels,
	}

	// Set ownership from authenticated user
	if user := GetUserIdentityFromContext(ctx); user != nil {
		project.CreatedBy = user.ID()
		project.OwnerID = user.ID()
	}

	// Apply default shared dirs from hub settings.
	// Only stamp when the project has no shared dirs. Since
	// CreateProjectRequest has no SharedDirs field, this is always
	// true for direct-create — but the guard future-proofs against
	// API evolution.
	if len(project.SharedDirs) == 0 {
		project.SharedDirs = s.defaultProjectSharedDirs()
	}

	// Apply hub-level default agent roles to new projects.
	if defaults := s.hubAgentDefaults(); defaults.DefaultMaxAgentRole != "" || defaults.DefaultAgentRole != "" {
		if project.Annotations == nil {
			project.Annotations = make(map[string]string)
		}
		if defaults.DefaultMaxAgentRole != "" {
			if _, exists := project.Annotations[projectSettingMaxAgentRole]; !exists {
				project.Annotations[projectSettingMaxAgentRole] = defaults.DefaultMaxAgentRole
			}
		}
		if defaults.DefaultAgentRole != "" {
			if _, exists := project.Annotations[projectSettingDefaultAgentRole]; !exists {
				project.Annotations[projectSettingDefaultAgentRole] = defaults.DefaultAgentRole
			}
		}
	}

	// Quota enforcement: check projects-per-user limit before creation.
	if s.quotaService != nil && project.CreatedBy != "" {
		if err := s.quotaService.CheckAndReserve(ctx, "max_projects_per_user", project.CreatedBy, "system", "system", project.ID); err != nil {
			if errors.Is(err, store.ErrQuotaExceeded) {
				writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
					"quota exceeded: max_projects_per_user", nil)
				return
			}
			if errors.Is(err, ErrQuotaLockContention) {
				writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
					"quota check temporarily unavailable, please retry", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "quota check failed", nil)
			return
		}
	}

	if err := s.store.CreateProject(ctx, project); err != nil {
		if s.quotaService != nil && project.CreatedBy != "" {
			s.quotaService.Release(ctx, "max_projects_per_user", project.ID)
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Create canonical project membership as role binding (PM1).
	// The creator gets the project-owner role binding, which is the sole
	// source of truth for project membership. A project without an owner
	// binding is unusable, so failure rolls back the project creation.
	if project.CreatedBy != "" {
		if rbErr := s.createProjectOwnerRoleBinding(ctx, project.ID, project.CreatedBy); rbErr != nil {
			s.projectsLogger().Error("CRITICAL: failed to create project owner role binding — rolling back project",
				"project_id", project.ID, "user_id", project.CreatedBy, "error", rbErr)
			if delErr := s.store.DeleteProject(ctx, project.ID); delErr != nil {
				s.projectsLogger().Warn("failed to roll back project after owner binding failure",
					"project_id", project.ID, "error", delErr)
			}
			if s.quotaService != nil && project.CreatedBy != "" {
				s.quotaService.Release(ctx, "max_projects_per_user", project.ID)
			}
			writeError(w, http.StatusInternalServerError, "ROLE_BINDING_FAILED",
				"Failed to create project owner binding; project creation rolled back", nil)
			return
		}
	}

	// Create the associated project_agents group (best-effort)
	s.createProjectGroup(ctx, project)

	// Create project members group and policy (best-effort).
	// This continues to create the group for collaboration and policy bindings.
	// The role binding above is now the canonical membership source.
	s.createProjectMembersGroup(ctx, project)

	// Ensure the #general chat topic exists for this project (best-effort).
	s.ensureProjectGeneralTopic(ctx, project)

	// For git projects, try to auto-associate a GitHub App installation so that
	// clone/pull operations can mint tokens. This covers the case where the app
	// was installed before the project was created (webhook already fired).
	if project.GitRemote != "" && project.GitHubInstallationID == nil {
		s.autoAssociateGitHubInstallation(ctx, project)
	}

	// Save the GitHub token as a project secret if provided.
	// This must happen before cloneSharedWorkspaceProject so that
	// resolveCloneToken can find it during the initial clone.
	// Only applies to git-backed projects (GitRemote != "").
	if req.GitHubToken != "" && s.secretBackend != nil && project.GitRemote != "" {
		tokenInput := &secret.SetSecretInput{
			Name:          "GITHUB_TOKEN",
			Value:         req.GitHubToken,
			SecretType:    secret.TypeEnvironment,
			Target:        "GITHUB_TOKEN",
			Scope:         secret.ScopeProject,
			ScopeID:       project.ID,
			Description:   "GitHub token for repository access",
			InjectionMode: "always",
			CreatedBy:     project.CreatedBy,
			UpdatedBy:     project.CreatedBy,
		}
		if _, _, err := s.secretBackend.Set(ctx, tokenInput); err != nil {
			s.projectsLogger().Error("failed to save GitHub token as project secret",
				"project_id", project.ID, "error", err)
			// Cascade-delete role bindings before the project row to avoid
			// orphaned bindings referencing a deleted project (R1 review fix).
			if _, rbErr := s.store.DeleteRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID); rbErr != nil {
				s.projectsLogger().Warn("failed to clean up role bindings after secret save failure",
					"project_id", project.ID, "error", rbErr)
			}
			if delErr := s.store.DeleteProject(ctx, project.ID); delErr != nil {
				s.projectsLogger().Warn("failed to clean up project record after secret save failure",
					"project_id", project.ID, "error", delErr)
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to save GitHub token: "+err.Error(), nil)
			return
		}
	}

	// Initialize filesystem workspace for hub-managed projects and shared-workspace git projects.
	if project.IsSharedWorkspace() {
		// Shared-workspace git project: clone the repository into the workspace.
		// Clone failure is a creation failure — clean up the project record.
		if err := s.cloneSharedWorkspaceProject(ctx, project); err != nil {
			s.projectsLogger().Error("shared workspace clone failed, rolling back project creation",
				"project_id", project.ID, "slug", project.Slug, "error", err)
			if req.GitHubToken != "" && s.secretBackend != nil && project.GitRemote != "" {
				if delErr := s.secretBackend.Delete(ctx, "GITHUB_TOKEN", secret.ScopeProject, project.ID); delErr != nil {
					s.projectsLogger().Warn("failed to clean up project secret after clone failure",
						"project_id", project.ID, "error", delErr)
				}
			}
			// Cascade-delete role bindings before the project row to avoid
			// orphaned bindings referencing a deleted project (R1 review fix).
			if _, rbErr := s.store.DeleteRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID); rbErr != nil {
				s.projectsLogger().Warn("failed to clean up role bindings after clone failure",
					"project_id", project.ID, "error", rbErr)
			}
			if delErr := s.store.DeleteProject(ctx, project.ID); delErr != nil {
				s.projectsLogger().Warn("failed to clean up project record after clone failure",
					"project_id", project.ID, "error", delErr)
			}
			// Use appropriate HTTP status based on the error kind
			statusCode := http.StatusInternalServerError
			var details map[string]interface{}
			var gitErr *util.GitError
			if errors.As(err, &gitErr) {
				if guidance := gitErr.UserGuidance(); guidance != "" {
					details = map[string]interface{}{"guidance": guidance}
				}
				switch gitErr.Kind {
				case util.GitErrAuth:
					statusCode = http.StatusUnprocessableEntity
				case util.GitErrNotFound:
					statusCode = http.StatusUnprocessableEntity
				}
			}
			writeError(w, statusCode, ErrCodeCloneFailed,
				"Failed to clone repository for shared workspace: "+err.Error(), details)
			return
		}
	} else if project.GitRemote == "" {
		// Hub-native project (no git remote): create workspace directory.
		if err := s.initHubManagedProject(project); err != nil {
			s.projectsLogger().Warn("failed to initialize project workspace",
				"project_id", project.ID, "slug", project.Slug, "error", err)
		}
	}

	// Auto-link brokers that have auto_provide enabled (mirrors registerProject behavior).
	s.autoLinkProviders(ctx, project)

	s.events.PublishProjectCreated(ctx, project)

	writeJSON(w, http.StatusCreated, project)
}

// createProjectGroup creates the implicit project_agents group for a project.
// This is a best-effort operation; failures are logged but don't fail the caller.
// If the group already exists (e.g., project was deleted and recreated with the same
// slug), the existing group is reused and its project ID association is updated.
func (s *Server) createProjectGroup(ctx context.Context, project *store.Project) {
	agentsSlug := "project:" + project.Slug + ":agents"
	projectGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      project.Name + " Agents",
		Slug:      agentsSlug,
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: project.ID,
		CreatedBy: project.CreatedBy,
		Annotations: map[string]string{
			systemProjectAgentsGroupAnnotation: "true",
		},
	}
	if err := s.store.CreateGroup(ctx, projectGroup); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			s.projectsLogger().Warn("failed to create project group", "project_id", project.ID, "error", err.Error())
			return
		}
		// Slug conflict — look it up and validate before adopting.
		existing, lookupErr := s.store.GetGroupBySlug(ctx, agentsSlug)
		if lookupErr != nil {
			s.projectsLogger().Warn("failed to look up existing project agents group by slug",
				"project_id", project.ID, "slug", agentsSlug, "error", lookupErr.Error())
			return
		}
		if !isSystemProjectAgentsGroup(existing, project.ID) {
			s.projectsLogger().Warn("refusing to adopt colliding project agents group",
				"project_id", project.ID, "slug", agentsSlug, "group", existing.ID,
				"group_type", existing.GroupType, "existing_project_id", existing.ProjectID)
			return
		}
		if existing.GroupType != store.GroupTypeProjectAgents {
			s.projectsLogger().Warn("refusing to adopt project agents group with wrong group type",
				"project_id", project.ID, "slug", agentsSlug, "group", existing.ID,
				"expected_type", store.GroupTypeProjectAgents, "actual_type", existing.GroupType)
			return
		}
	}
}

// ensureProjectGeneralTopic creates the #general chat topic for a project if
// the webchat store is configured. Best-effort: failures are logged but do not
// block project creation.
func (s *Server) ensureProjectGeneralTopic(ctx context.Context, project *store.Project) {
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		return
	}
	createdBy := project.CreatedBy
	if createdBy == "" {
		createdBy = "system"
	}
	topicID, created, err := wcs.EnsureGeneralTopic(ctx, project.ID, createdBy)
	if err != nil {
		s.projectsLogger().Warn("failed to create #general topic for project",
			"project_id", project.ID, "error", err)
		return
	}

	// Only publish the created event when a new topic was actually inserted.
	// EnsureGeneralTopic is idempotent (ON CONFLICT DO NOTHING), so
	// re-calling it for an existing project must not emit a spurious event.
	if !created {
		return
	}
	topic, err := wcs.GetTopic(ctx, topicID)
	if err != nil || topic == nil {
		return
	}
	s.events.PublishChatTopicEvent(ctx, project.ID, "created", *topic)
}

// createProjectOwnerRoleBinding creates a project-owner role binding for the
// given user in the given project. This is the canonical project membership
// source (Phase 1E). The role binding is scoped to the project ID, not the
// project slug, so it survives project renames.
func (s *Server) createProjectOwnerRoleBinding(ctx context.Context, projectID, userID string) error {
	ownerRoleDef, err := s.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		return fmt.Errorf("lookup project-owner role definition: %w", err)
	}
	_, err = s.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        userID,
	})
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil // idempotent
		}
		return fmt.Errorf("create project-owner role binding: %w", err)
	}

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: "project_member_add",
		TargetType:   "project_membership",
		TargetID:     projectID,
		AfterSummary: `{"userId":"` + userID + `","role":"owner"}`,
	})

	return nil
}

// createProjectRoleBinding creates a project-scoped role binding for any
// principal (user, agent, or group). This is the canonical way to grant
// project membership. Returns nil on duplicate (idempotent).
func (s *Server) createProjectRoleBinding(ctx context.Context, projectID, principalType, principalID, roleName, createdBy string) error {
	roleDef, err := s.store.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	if err != nil {
		return fmt.Errorf("lookup %s role definition: %w", roleName, err)
	}
	_, err = s.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: roleDef.ID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        createdBy,
	})
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil // idempotent
		}
		return fmt.Errorf("create %s role binding: %w", roleName, err)
	}
	return nil
}

// ensureHubMembersProjectVisibility creates a project-member RoleBinding for
// the hub-members group, making the project visible to all hub members.
// This replaces the old pattern of adding hub-members as a nested group member
// of the project's members group. Best-effort; failures are logged.
//
// TODO(PM1): Wire into handleCreateProject. The legacy
// ensureProjectMemberReadPolicy bridge has been removed (CO1 cutover);
// this function provides the RoleBinding-based replacement.
func (s *Server) ensureHubMembersProjectVisibility(ctx context.Context, project *store.Project) {
	group, err := s.store.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		s.projectsLogger().Debug("hub-members group not found, skipping project visibility binding",
			"project_id", project.ID, "error", err)
		return
	}
	if err := s.createProjectRoleBinding(ctx, project.ID, store.RoleBindingPrincipalGroup,
		group.ID, store.ProjectRoleMember, "system"); err != nil {
		s.projectsLogger().Warn("failed to create hub-members project visibility binding",
			"project_id", project.ID, "error", err)
	}
}

// countDirectOwnerBindings returns the number of direct-user project-owner
// role bindings for a project.
//
// Known limitation (O3): This count includes all matching bindings regardless
// of activation conditions (NotBefore, ExpiresAt). An expired or not-yet-active
// binding still counts toward the minimum owner threshold. Currently moot
// because owner bindings are created unconditionally without time bounds, but
// this will need to filter by activation state if time-bounded ownership
// bindings are introduced.
func (s *Server) countDirectOwnerBindings(ctx context.Context, projectID string) (int, error) {
	bindings, err := s.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	if err != nil {
		return 0, err
	}
	ownerRoleDef, err := s.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, b := range bindings {
		if b.PrincipalType == store.RoleBindingPrincipalUser && b.RoleDefinitionID == ownerRoleDef.ID {
			count++
		}
	}
	return count, nil
}

const systemProjectMembersGroupAnnotation = "scion.io/project-members-group"
const systemProjectAgentsGroupAnnotation = "scion.io/project-agents-group"

func projectMembersGroupSlug(projectSlug string) string {
	return "project:" + projectSlug + ":members"
}

func isSystemProjectMembersGroup(group *store.Group, projectID string) bool {
	return group != nil &&
		group.ProjectID == projectID &&
		group.Annotations != nil &&
		group.Annotations[systemProjectMembersGroupAnnotation] == "true"
}

func isSystemProjectAgentsGroup(group *store.Group, projectID string) bool {
	return group != nil &&
		group.ProjectID == projectID &&
		group.Annotations != nil &&
		group.Annotations[systemProjectAgentsGroupAnnotation] == "true"
}

// createProjectMembersGroup creates the project's collaboration
// members group and ensures project membership via RoleBindings. The group
// exists for collaboration (chat, agent co-ownership) but carries NO
// authorization meaning — all authorization flows through project-scoped
// RoleBindings.
//
// PM1 contract: project membership IS the set of project-scoped role bindings.
// The special project:<slug>:members group no longer has authorization meaning.
//
// callerUserID, when non-empty, also receives a project-owner RoleBinding
// (e.g. the user who linked the project). It is safe to pass the same value as
// project.CreatedBy — duplicate bindings are handled gracefully.
func (s *Server) createProjectMembersGroup(ctx context.Context, project *store.Project, callerUserID ...string) {
	membersSlug := projectMembersGroupSlug(project.Slug)

	s.projectsLogger().Debug("ensuring project members group",
		"project_id", project.ID, "slug", project.Slug, "membersSlug", membersSlug)

	// Create project members group, or look up the existing one.
	// The group exists for collaboration purposes (chat, agent management)
	// but has no authorization meaning — membership is defined by RoleBindings.
	membersGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      project.Name + " Members",
		Slug:      membersSlug,
		GroupType: store.GroupTypeExplicit,
		ProjectID: project.ID,
		OwnerID:   project.OwnerID,
		CreatedBy: project.CreatedBy,
		Annotations: map[string]string{
			systemProjectMembersGroupAnnotation: "true",
		},
	}
	createErr := s.store.CreateGroup(ctx, membersGroup)
	if createErr != nil && errors.Is(createErr, store.ErrInvalidInput) && membersGroup.OwnerID != "" {
		// FK violation: the owner user does not exist in the store. Retry
		// without OwnerID so the group is still created for collaboration.
		s.projectsLogger().Warn("project members group owner not found, retrying without owner",
			"project_id", project.ID, "owner_id", membersGroup.OwnerID, "error", createErr.Error())
		membersGroup.OwnerID = ""
		createErr = s.store.CreateGroup(ctx, membersGroup)
	}
	if createErr != nil {
		if !errors.Is(createErr, store.ErrAlreadyExists) {
			s.projectsLogger().Warn("failed to create project members group", "project_id", project.ID, "error", createErr.Error())
			// Group creation failed with a non-duplicate error. The group was
			// never persisted, so its UUID is a phantom — skip all downstream
			// operations that reference the group ID to avoid creating dangling
			// group memberships or legacy policy bindings (R2 review fix).
			return
		}
		if errors.Is(createErr, store.ErrAlreadyExists) {
			// Slug conflict — look up existing group
			existing, lookupErr := s.store.GetGroupBySlug(ctx, membersSlug)
			if lookupErr != nil {
				s.projectsLogger().Warn("failed to look up existing project members group by slug",
					"project_id", project.ID, "slug", membersSlug, "error", lookupErr.Error())
				return
			} else if !isSystemProjectMembersGroup(existing, project.ID) {
				s.projectsLogger().Warn("refusing to adopt colliding project members group",
					"project_id", project.ID, "slug", membersSlug, "group", existing.ID)
				return
			} else {
				membersGroup = existing
				// Update the owner in case it changed.
				if membersGroup.OwnerID == "" && project.OwnerID != "" {
					membersGroup.OwnerID = project.OwnerID
					if updateErr := s.store.UpdateGroup(ctx, membersGroup); updateErr != nil {
						s.projectsLogger().Warn("failed to update existing project members group",
							"project_id", project.ID, "slug", membersSlug, "error", updateErr.Error())
					}
				}
			}
		}
	} else {
		s.projectsLogger().Info("created project members group",
			"project_id", project.ID, "group", membersGroup.ID, "slug", membersSlug)
	}

	// Add the creating user to the collaboration group (best-effort).
	if project.CreatedBy != "" {
		if err := s.store.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    membersGroup.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   project.CreatedBy,
			Role:       store.GroupMemberRoleOwner,
		}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			s.projectsLogger().Warn("failed to add creator to project members group",
				"project_id", project.ID, "user", project.CreatedBy, "error", err.Error())
		}

		// Ensure a project-owner role binding exists for the creator. In
		// production, the createProject handler creates this via
		// createProjectOwnerRoleBinding BEFORE calling us. But this function
		// is also called from backfill and sync paths where the role binding
		// may not exist. Best-effort; errors logged.
		if rbErr := s.createProjectOwnerRoleBinding(ctx, project.ID, project.CreatedBy); rbErr != nil {
			s.projectsLogger().Debug("project owner role binding already exists or failed",
				"project_id", project.ID, "user", project.CreatedBy, "error", rbErr.Error())
		}
	}

	// Add the caller (e.g. the user who linked the project) as an owner too.
	// This is a no-op when callerUserID matches project.CreatedBy.
	if len(callerUserID) > 0 && callerUserID[0] != "" && callerUserID[0] != project.CreatedBy {
		if err := s.store.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    membersGroup.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   callerUserID[0],
			Role:       store.GroupMemberRoleOwner,
		}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			s.projectsLogger().Warn("failed to add caller to project members group",
				"project_id", project.ID, "user", callerUserID[0], "error", err.Error())
		}

		// Ensure role binding for caller too.
		if rbErr := s.createProjectOwnerRoleBinding(ctx, project.ID, callerUserID[0]); rbErr != nil {
			s.projectsLogger().Debug("caller project owner role binding already exists or failed",
				"project_id", project.ID, "user", callerUserID[0], "error", rbErr.Error())
		}
	}

	// ── Legacy policy bridge (pre-CO1) ──────────────────────────────────
	// CO1 cutover: legacy project policies are no longer needed.
	// All authorization routes through AK1 kernel using RoleBindings.
}

// hubManagedProjectPath returns the filesystem path for a hub-managed project workspace.
// It prefers projects/<slug> and falls back to groves/<slug> for backward compatibility
// with workspaces created before the grove-to-project rename.
//
// When the server has a workspace storage config with backend "nfs",
// "cloudrun-volume" or "gke-shared-volume", the durable volume-backed path is
// returned instead. A backward-compatible fallback checks the durable path
// first, then the legacy local path, so existing local deployments continue to
// work when durable storage is first configured.
func (s *Server) hubManagedProjectPath(slug string) (string, error) {
	if err := validateProjectSlug(slug); err != nil {
		return "", err
	}

	wsCfg := s.config.WorkspaceStorageConfig

	// --- NFS backend ---
	if wsCfg != nil && wsCfg.Backend == "nfs" && wsCfg.NFS != nil && len(wsCfg.NFS.Shares) > 0 {
		nfsPath := filepath.Join(workspaceMountRoot(wsCfg), "hub-projects", slug)
		if hasWorkspaceContent(nfsPath) {
			return nfsPath, nil
		}
		// Fallback: check legacy local path for backward compatibility
		if localPath, err := localProjectPath(slug); err == nil && hasWorkspaceContent(localPath) {
			return localPath, nil
		}
		// Neither has content — return NFS path (new projects go to NFS)
		return nfsPath, nil
	}

	// --- Cloud Run volume backend ---
	// Unlike the GKE branch below, this one does not require a non-empty
	// volume name: guarding it would change where existing Cloud Run
	// deployments look for content, which needs its own migration (#1073).
	if wsCfg != nil && wsCfg.Backend == "cloudrun-volume" && wsCfg.CloudRunVolume != nil {
		subPathRoot := wsCfg.CloudRunVolume.SubPathRoot
		if subPathRoot == "" {
			subPathRoot = "projects"
		}
		crPath := filepath.Join(volumeMountBase, wsCfg.CloudRunVolume.VolumeName, subPathRoot, "hub-projects", slug)
		if hasWorkspaceContent(crPath) {
			return crPath, nil
		}
		// Fallback: check legacy local path
		if localPath, err := localProjectPath(slug); err == nil && hasWorkspaceContent(localPath) {
			return localPath, nil
		}
		return crPath, nil
	}

	// --- GKE shared volume backend ---
	// The mount root comes from workspaceMountRoot, the same resolver
	// checkWorkspaceStorageHealth probes for readiness, so the two cannot
	// drift. An empty root means no volume name was configured: there is no
	// mount point to build a path from, so the config is treated as unset and
	// this falls through to the local path. A deployment in that state fails
	// its readiness check and never serves.
	if wsCfg != nil && wsCfg.Backend == "gke-shared-volume" && wsCfg.GKESharedVolume != nil {
		if mountRoot := workspaceMountRoot(wsCfg); mountRoot != "" {
			subPathRoot := wsCfg.GKESharedVolume.SubPathRoot
			if subPathRoot == "" {
				subPathRoot = "projects"
			}
			gkePath := filepath.Join(mountRoot, subPathRoot, "hub-projects", slug)
			if hasWorkspaceContent(gkePath) {
				return gkePath, nil
			}
			// Fallback: check legacy local path
			if localPath, err := localProjectPath(slug); err == nil && hasWorkspaceContent(localPath) {
				// Worth saying out loud: on GKE the local path is always pod
				// ephemeral storage, so this content disappears on the next
				// reschedule and the project silently moves to the volume.
				s.warnEphemeralProjectPath(slug, localPath, gkePath)
				return localPath, nil
			}
			return gkePath, nil
		}
	}

	// --- Default: local ephemeral path (existing behavior) ---
	return localProjectPath(slug)
}

// hubManagedProjectPath is the package-level backward-compatible wrapper used by
// callers that don't have access to a Server (e.g. resolveHubProjectSharedDirPath).
// It always returns the local path. Server-method callers should prefer
// s.hubManagedProjectPath for durable-storage awareness.
func hubManagedProjectPath(slug string) (string, error) {
	return localProjectPath(slug)
}

// validateProjectSlug rejects empty slugs and slugs containing path-traversal
// characters (/, \, ..) to prevent directory-traversal attacks.
func validateProjectSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("project slug must not be empty")
	}
	if strings.Contains(slug, "/") || strings.Contains(slug, "\\") || strings.Contains(slug, "..") {
		return fmt.Errorf("project slug contains invalid characters")
	}
	return nil
}

// localProjectPath returns the legacy local filesystem path for a hub-managed
// project workspace under ~/.scion/projects/<slug>, with groves/<slug> fallback.
func localProjectPath(slug string) (string, error) {
	if err := validateProjectSlug(slug); err != nil {
		return "", err
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", fmt.Errorf("failed to get global dir: %w", err)
	}
	projectsPath := filepath.Join(globalDir, "projects", slug)
	if hasWorkspaceContent(projectsPath) {
		return projectsPath, nil
	}
	grovesPath := filepath.Join(globalDir, "groves", slug)
	if hasWorkspaceContent(grovesPath) {
		return grovesPath, nil
	}
	return projectsPath, nil
}

// hasWorkspaceContent returns true if dir exists and contains meaningful
// workspace files beyond just infrastructure directories.
func hasWorkspaceContent(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		switch e.Name() {
		case "shared-dirs", ".scion":
			continue
		default:
			return true
		}
	}
	return false
}

// initHubManagedProject initializes the filesystem workspace for a hub-managed project.
// It creates the workspace directory and seeds the .scion project structure with
// hub connection settings. Unlike regular projects, hub-managed projects store
// settings directly in the .scion directory (no split storage or marker files).
func (s *Server) initHubManagedProject(project *store.Project) error {
	workspacePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(workspacePath, 0755); err != nil {
		return fmt.Errorf("failed to create project workspace directory: %w", err)
	}

	scionDir := filepath.Join(workspacePath, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		return fmt.Errorf("failed to create .scion directory: %w", err)
	}

	// Seed default settings.yaml directly in scionDir. Hub-native projects
	// bypass InitProject (which uses split storage for git repos) and keep
	// all configuration in-place.
	settingsPath := filepath.Join(scionDir, "settings.yaml")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		defaultSettings, err := config.GetProjectDefaultSettingsYAML()
		if err != nil {
			return fmt.Errorf("failed to read default project settings: %w", err)
		}
		if err := os.WriteFile(settingsPath, defaultSettings, 0644); err != nil {
			return fmt.Errorf("failed to seed settings.yaml: %w", err)
		}
	}

	// Write hub connection settings into the seeded settings file.
	settingsUpdates := map[string]string{
		"hub.enabled":   "true",
		"hub.endpoint":  s.config.HubEndpoint,
		"hub.projectId": project.ID,
		"project_id":    project.ID,
	}
	for key, value := range settingsUpdates {
		if err := config.UpdateSetting(scionDir, key, value, false); err != nil {
			s.projectsLogger().Warn("failed to update hub-managed project setting",
				"project_id", project.ID, "key", key, "error", err.Error())
		}
	}

	return nil
}

// cloneSharedWorkspaceProject performs the host-side git clone for a shared-workspace
// git project. It clones the repository into the hub-native workspace path and
// seeds the .scion project structure on top. If the clone fails, the workspace
// directory is cleaned up and an error is returned.
func (s *Server) cloneSharedWorkspaceProject(ctx context.Context, project *store.Project) error {
	workspacePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		return err
	}

	// Build clone URL from the project's git remote.
	// The clone-url label may be an explicit override (e.g. local path for testing).
	// Only convert to HTTPS if the URL looks like a remote git URL.
	cloneURL := resolveCloneURL(project.Labels["scion.dev/clone-url"], project.GitRemote)

	defaultBranch := project.Labels["scion.dev/default-branch"]
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	// Resolve a token for authentication.
	token := s.resolveCloneToken(ctx, project)

	// Perform the clone
	if err := util.CloneSharedWorkspace(workspacePath, cloneURL, defaultBranch, token); err != nil {
		// Clean up the workspace directory on failure — return to pre-creation state
		_ = util.RemoveAllSafe(workspacePath)
		return fmt.Errorf("shared workspace clone failed: %w", err)
	}

	// Seed the .scion project on top of the cloned workspace
	scionDir := filepath.Join(workspacePath, ".scion")
	if err := config.InitProject(scionDir, nil, config.InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		s.projectsLogger().Warn("failed to initialize .scion in cloned workspace",
			"project_id", project.ID, "error", err.Error())
	}

	// Write hub connection settings
	settingsUpdates := map[string]string{
		"hub.enabled":   "true",
		"hub.endpoint":  s.config.HubEndpoint,
		"hub.projectId": project.ID,
		"project_id":    project.ID,
	}
	for key, value := range settingsUpdates {
		if err := config.UpdateSetting(scionDir, key, value, false); err != nil {
			s.projectsLogger().Warn("failed to update shared-workspace project setting",
				"project_id", project.ID, "key", key, "error", err.Error())
		}
	}

	return nil
}

// autoAssociateGitHubInstallation searches active GitHub App installations for one
// that covers the project's repository. If found, it sets GitHubInstallationID on the
// project and persists the association. This handles the case where a GitHub App was
// installed (and its webhook processed) before the project was created.
func (s *Server) autoAssociateGitHubInstallation(ctx context.Context, project *store.Project) {
	ownerRepo := extractOwnerRepo(project.GitRemote)
	if ownerRepo == "" {
		return
	}

	installations, err := s.store.ListGitHubInstallations(ctx, store.GitHubInstallationFilter{
		Status: store.GitHubInstallationStatusActive,
	})
	if err != nil {
		s.projectsLogger().Warn("failed to list GitHub App installations for auto-association",
			"project_id", project.ID, "error", err)
		return
	}

	ownerRepoLower := strings.ToLower(ownerRepo)
	for _, inst := range installations {
		for _, repo := range inst.Repositories {
			if strings.ToLower(repo) == ownerRepoLower {
				installID := inst.InstallationID
				project.GitHubInstallationID = &installID
				project.GitHubAppStatus = &store.GitHubAppProjectStatus{
					State:       store.GitHubAppStateUnchecked,
					LastChecked: timeNow(),
				}
				if err := s.store.UpdateProject(ctx, project); err != nil {
					s.projectsLogger().Warn("failed to persist GitHub App installation association",
						"project_id", project.ID, "installation_id", installID, "error", err)
				} else {
					s.projectsLogger().Info("auto-associated project with GitHub App installation at creation time",
						"project_id", project.ID, "project_name", project.Name,
						"installation_id", installID, "account", inst.AccountLogin)
					s.events.PublishProjectUpdated(ctx, project)
				}
				return
			}
		}
	}
}

// resolveCloneToken resolves a GitHub token for cloning a project's repository.
// It tries GitHub App installation tokens first, then project secrets, then the
// creating user's profile-level GitHub token as a final fallback. This last
// fallback solves the bootstrap problem where a new project linked to a private
// repo has no project-level credentials yet.
func (s *Server) resolveCloneToken(ctx context.Context, project *store.Project) string {
	// Try GitHub App token first
	if project.GitHubInstallationID != nil {
		token, _, err := s.MintGitHubAppTokenForProject(ctx, project)
		if err == nil && token != "" {
			return token
		}
		if err != nil {
			s.projectsLogger().Warn("failed to mint GitHub App token for clone, trying secrets",
				"project_id", project.ID, "error", err.Error())
		}
	}

	if s.secretBackend != nil {
		// Fall back to GITHUB_TOKEN from project secrets
		sv, err := s.secretBackend.Get(ctx, "GITHUB_TOKEN", "project", project.ID)
		if err == nil && sv != nil && sv.Value != "" {
			return sv.Value
		}

		// Fall back to the creating user's profile-level GITHUB_TOKEN
		if project.CreatedBy != "" {
			sv, err = s.secretBackend.Get(ctx, "GITHUB_TOKEN", "user", project.CreatedBy)
			if err == nil && sv != nil && sv.Value != "" {
				s.projectsLogger().Info("using creator's GitHub token for project clone",
					"project_id", project.ID, "user_id", project.CreatedBy)
				return sv.Value
			}
		}
	}

	return ""
}

// syncWorkspaceOnStop triggers a best-effort workspace sync-back for hub-managed projects
// on remote brokers before the agent is stopped. It uploads the workspace from the
// broker to GCS via the control channel, then downloads from GCS to the Hub filesystem.
func (s *Server) syncWorkspaceOnStop(ctx context.Context, agent *store.Agent) {
	if agent.ProjectID == "" || agent.RuntimeBrokerID == "" {
		return
	}

	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if err != nil || (project.GitRemote != "" && !project.IsSharedWorkspace()) {
		return // Not hub-native/shared-workspace or project not found
	}

	// Check if broker is co-located (embedded or has local path)
	if s.isEmbeddedBroker(agent.RuntimeBrokerID) {
		return // Embedded broker, no sync needed
	}
	provider, err := s.store.GetProjectProvider(ctx, project.ID, agent.RuntimeBrokerID)
	if err == nil && provider.LocalPath != "" {
		return // Colocated broker, no sync needed
	}

	stor := s.GetStorage()
	cc := s.GetControlChannelManager()
	if stor == nil || cc == nil {
		return
	}

	storagePath := storage.ProjectWorkspaceStoragePath(s.HubID(), project.ID)

	// Tunnel upload request to the broker
	uploadReq := RuntimeBrokerWorkspaceUploadRequest{
		Slug:        agent.Slug,
		StoragePath: storagePath,
	}
	var uploadResp RuntimeBrokerWorkspaceUploadResponse
	if err := tunnelWorkspaceRequest(ctx, cc, agent.RuntimeBrokerID, "POST", "/api/v1/workspace/upload", uploadReq, &uploadResp); err != nil {
		s.agentLifecycleLog.Warn("syncWorkspaceOnStop: failed to upload workspace from broker",
			"agent_id", agent.ID,
			"agent", agent.Name, "project_id", project.ID, "error", err)
		return
	}

	// Download from GCS to Hub filesystem
	workspacePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		s.agentLifecycleLog.Warn("syncWorkspaceOnStop: failed to get project path", "agent_id", agent.ID, "error", err)
		return
	}

	if err := gcp.SyncFromGCS(ctx, stor.Bucket(), storagePath+"/files", workspacePath); err != nil {
		s.agentLifecycleLog.Warn("syncWorkspaceOnStop: GCS download failed",
			"agent_id", agent.ID,
			"project_id", project.ID, "error", err)
	} else {
		s.agentLifecycleLog.Info("syncWorkspaceOnStop: workspace synced back to Hub",
			"agent_id", agent.ID,
			"project_id", project.ID, "path", workspacePath)
	}
}

func (s *Server) handleProjectRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	var req RegisterProjectRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	normalizedRemote := util.NormalizeGitRemote(req.GitRemote)

	// Try to find existing project
	var project *store.Project
	var created bool

	// First, try to look up by client-provided project ID
	if req.ID != "" {
		existingProject, err := s.store.GetProject(ctx, req.ID)
		if err == nil {
			project = existingProject
		} else if err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// If not found by ID, try git remote lookup
	var gitRemoteMatches []*store.Project
	if project == nil && normalizedRemote != "" {
		// For projects with git remote, look up by git remote (may return multiple)
		matchingProjects, err := s.store.GetProjectsByGitRemote(ctx, normalizedRemote)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if len(matchingProjects) == 1 {
			// Backward compatible: single match auto-links
			project = matchingProjects[0]
		} else if len(matchingProjects) > 1 {
			// Multiple matches — return the list for client-side disambiguation.
			gitRemoteMatches = matchingProjects
		}
	}

	// If still not found and no git remote, try by slug (for global projects)
	if project == nil && normalizedRemote == "" {
		// For projects without git remote (like global projects), look up by slug (case-insensitive)
		slug := api.Slugify(req.Name)
		existingProject, err := s.store.GetProjectBySlugCaseInsensitive(ctx, slug)
		if err == nil {
			project = existingProject
		} else if err != store.ErrNotFound {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Create new project if not found
	if project == nil {
		// Use client-provided ID if available; fall back to random UUID.
		projectID := req.ID
		if projectID == "" {
			projectID = api.NewUUID()
		}

		baseSlug := api.Slugify(req.Name)
		slug, err := s.store.NextAvailableSlug(ctx, baseSlug)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		displayName := req.Name
		if slug != baseSlug {
			displayName = api.DisplayNameWithSerial(req.Name, slug, baseSlug)
		}

		project = &store.Project{
			ID:        projectID,
			Name:      displayName,
			Slug:      slug,
			GitRemote: normalizedRemote,
			Labels:    req.Labels,
		}

		// Set ownership from authenticated user
		if user := GetUserIdentityFromContext(ctx); user != nil {
			project.CreatedBy = user.ID()
			project.OwnerID = user.ID()
		}

		// Apply default shared dirs from hub settings (new projects only).
		if len(project.SharedDirs) == 0 {
			project.SharedDirs = s.defaultProjectSharedDirs()
		}

		// Apply hub-level default agent roles to new projects.
		if defaults := s.hubAgentDefaults(); defaults.DefaultMaxAgentRole != "" || defaults.DefaultAgentRole != "" {
			if project.Annotations == nil {
				project.Annotations = make(map[string]string)
			}
			if defaults.DefaultMaxAgentRole != "" {
				if _, exists := project.Annotations[projectSettingMaxAgentRole]; !exists {
					project.Annotations[projectSettingMaxAgentRole] = defaults.DefaultMaxAgentRole
				}
			}
			if defaults.DefaultAgentRole != "" {
				if _, exists := project.Annotations[projectSettingDefaultAgentRole]; !exists {
					project.Annotations[projectSettingDefaultAgentRole] = defaults.DefaultAgentRole
				}
			}
		}

		// Quota enforcement: check projects-per-user limit before creation.
		if s.quotaService != nil && project.CreatedBy != "" {
			if err := s.quotaService.CheckAndReserve(ctx, "max_projects_per_user", project.CreatedBy, "system", "system", project.ID); err != nil {
				if errors.Is(err, store.ErrQuotaExceeded) {
					writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
						"quota exceeded: max_projects_per_user", nil)
					return
				}
				if errors.Is(err, ErrQuotaLockContention) {
					writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
						"quota check temporarily unavailable, please retry", nil)
					return
				}
				writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "quota check failed", nil)
				return
			}
		}

		if err := s.store.CreateProject(ctx, project); err != nil {
			if s.quotaService != nil && project.CreatedBy != "" {
				s.quotaService.Release(ctx, "max_projects_per_user", project.ID)
			}
			writeErrorFromErr(w, err, "")
			return
		}
		created = true

		// Create owner role binding (PM1: atomic with project creation).
		if project.CreatedBy != "" {
			if rbErr := s.createProjectOwnerRoleBinding(ctx, project.ID, project.CreatedBy); rbErr != nil {
				s.projectsLogger().Error("CRITICAL: failed to create project owner role binding during register — rolling back",
					"project_id", project.ID, "user_id", project.CreatedBy, "error", rbErr)
				if delErr := s.store.DeleteProject(ctx, project.ID); delErr != nil {
					s.projectsLogger().Warn("failed to roll back project after owner binding failure",
						"project_id", project.ID, "error", delErr)
				}
				if s.quotaService != nil {
					s.quotaService.Release(ctx, "max_projects_per_user", project.ID)
				}
				writeError(w, http.StatusInternalServerError, "ROLE_BINDING_FAILED",
					"Failed to create project owner binding; project creation rolled back", nil)
				return
			}
		}

		// Create the associated project_agents group (best-effort)
		s.createProjectGroup(ctx, project)

		// Create project members group (best-effort, for collaboration)
		s.createProjectMembersGroup(ctx, project)

		// Ensure the #general chat topic exists for this project (best-effort).
		s.ensureProjectGeneralTopic(ctx, project)

		// Auto-link brokers that have auto_provide enabled
		s.autoLinkProviders(ctx, project)
	} else {
		// Existing project — ensure associated groups exist (backfill for
		// projects created before group support was added). Pass the
		// authenticated user so they are added as owner of the members
		// group (the person linking deserves membership).
		var callerID string
		if user := GetUserIdentityFromContext(ctx); user != nil {
			callerID = user.ID()
		}
		s.projectsLogger().Debug("ensuring groups for existing project during register",
			"project_id", project.ID, "slug", project.Slug, "caller", callerID)
		s.createProjectGroup(ctx, project)
		s.createProjectMembersGroup(ctx, project, callerID)
	}

	// Handle broker linking - two paths:
	// 1. New flow (preferred): BrokerID provided - link to existing broker (no secret generation)
	// 2. Deprecated flow: Broker object provided - create/update broker AND generate secret
	var broker *store.RuntimeBroker
	var brokerToken string
	var secretKey string

	if req.BrokerID != "" {
		// NEW FLOW: Link to existing broker registered via two-phase /brokers + /brokers/join
		existingBroker, err := s.store.GetRuntimeBroker(ctx, req.BrokerID)
		if err != nil {
			if err == store.ErrNotFound {
				ValidationError(w, "brokerId not found: broker must be registered via POST /brokers and /brokers/join first", map[string]interface{}{
					"field":    "brokerId",
					"brokerId": req.BrokerID,
				})
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		broker = existingBroker

		// Add as project provider. When the project already existed and the
		// broker is already a provider, preserve the existing localPath to
		// avoid converting a hub-native git project into a linked project.
		localPath := req.Path
		if !created {
			if existingProvider, err := s.store.GetProjectProvider(ctx, project.ID, broker.ID); err == nil {
				localPath = existingProvider.LocalPath
			}
		}
		provider := &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LocalPath:  localPath,
			Status:     broker.Status,
		}

		if err := s.store.AddProjectProvider(ctx, provider); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// For linked projects (local directory), initialize the .scion
		// directory structure so agents and templates directories exist.
		if localPath != "" {
			scionDir := filepath.Join(localPath, ".scion")
			if err := config.InitProject(scionDir, nil, config.InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
				s.projectsLogger().Warn("failed to initialize .scion in linked project",
					"project_id", project.ID, "localPath", localPath, "error", err.Error())
			}
		}

		// Set as default runtime broker if project doesn't have one
		if project.DefaultRuntimeBrokerID == "" {
			project.DefaultRuntimeBrokerID = broker.ID
			if err := s.store.UpdateProject(ctx, project); err != nil {
				util.Debugf("Warning: failed to set default runtime broker: %v", err)
			}
		}

		// No secret returned - broker already has credentials from /brokers/join
	} else if req.Broker != nil {
		// DEPRECATED FLOW: Embedded broker registration (creates broker and generates secret)
		util.Debugf("Warning: embedded Broker field in project registration is deprecated. Use two-phase registration: POST /brokers + POST /brokers/join, then pass brokerId")

		brokerID := req.Broker.ID

		// Try to find existing broker by ID first, then by name
		var existingBroker *store.RuntimeBroker
		var err error

		if brokerID != "" {
			existingBroker, err = s.store.GetRuntimeBroker(ctx, brokerID)
			if err != nil && err != store.ErrNotFound {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		// If not found by ID, try to find by name (prevents duplicate brokers with same hostname)
		if existingBroker == nil && req.Broker.Name != "" {
			existingBroker, err = s.store.GetRuntimeBrokerByName(ctx, req.Broker.Name)
			if err != nil && err != store.ErrNotFound {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		if existingBroker != nil {
			// Update existing broker
			broker = existingBroker
			broker.Name = req.Broker.Name
			broker.Slug = api.Slugify(req.Broker.Name)
			broker.Version = req.Broker.Version
			broker.Status = store.BrokerStatusOnline
			broker.ConnectionState = "connected"
			broker.Capabilities = req.Broker.Capabilities
			broker.Profiles = req.Broker.Profiles

			if err := s.store.UpdateRuntimeBroker(ctx, broker); err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			// Create new broker
			if brokerID == "" {
				brokerID = api.NewUUID()
			}

			broker = &store.RuntimeBroker{
				ID:              brokerID,
				Name:            req.Broker.Name,
				Slug:            api.Slugify(req.Broker.Name),
				Version:         req.Broker.Version,
				Status:          store.BrokerStatusOnline,
				ConnectionState: "connected",
				Capabilities:    req.Broker.Capabilities,
				Profiles:        req.Broker.Profiles,
			}

			if err := s.store.CreateRuntimeBroker(ctx, broker); err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		// Add as project provider. When the project already existed and the
		// broker is already a provider, preserve the existing localPath to
		// avoid converting a hub-native git project into a linked project.
		localPath := req.Path
		if !created {
			if existingProvider, err := s.store.GetProjectProvider(ctx, project.ID, broker.ID); err == nil {
				localPath = existingProvider.LocalPath
			}
		}
		provider := &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LocalPath:  localPath,
			Status:     store.BrokerStatusOnline,
		}

		if err := s.store.AddProjectProvider(ctx, provider); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Set as default runtime broker if project doesn't have one
		// (first broker to register becomes the default)
		if project.DefaultRuntimeBrokerID == "" {
			project.DefaultRuntimeBrokerID = broker.ID
			if err := s.store.UpdateProject(ctx, project); err != nil {
				// Log but don't fail - the broker is registered, default can be set later
				util.Debugf("Warning: failed to set default runtime broker: %v", err)
			}
		}

		// Generate HMAC credentials for the broker if broker auth service is available
		// (deprecated flow only - new flow gets secrets from /brokers/join)
		if s.brokerAuthService != nil {
			var err error
			secretKey, err = s.brokerAuthService.GenerateAndStoreSecret(ctx, broker.ID)
			if err != nil {
				// Log but don't fail - broker is registered, can complete join later
				util.Debugf("Warning: failed to generate broker secret: %v", err)
				// Fall back to simple token for backward compatibility
				brokerToken = "broker_" + api.NewShortID() + "_" + api.NewShortID()
			}
		} else {
			// No broker auth service - use simple token
			brokerToken = "broker_" + api.NewShortID() + "_" + api.NewShortID()
		}
	}

	// Build match list for client-side disambiguation when multiple
	// projects share the same git remote.
	var matches []hubclient.ProjectMatch
	if len(gitRemoteMatches) > 0 {
		matches = make([]hubclient.ProjectMatch, len(gitRemoteMatches))
		for i, g := range gitRemoteMatches {
			matches[i] = hubclient.ProjectMatch{
				ID:   g.ID,
				Name: g.Name,
				Slug: g.Slug,
			}
		}
	}

	writeJSON(w, http.StatusOK, RegisterProjectResponse{
		Project:       project,
		LegacyProject: project,
		Broker:        broker,
		Created:       created,
		Matches:       matches,
		BrokerToken:   brokerToken,
		SecretKey:     secretKey,
	})
}

// handleProjectRoutes routes requests under /api/v1/projects/{projectId}/... or /api/v1/projects/{projectId}/...
// It supports both the project resource endpoints and nested agent endpoints.
func (s *Server) handleProjectRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract project ID and remaining path
	var path string
	if strings.HasPrefix(r.URL.Path, "/api/v1/projects/") {
		path = strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
	} else {
		path = strings.TrimPrefix(r.URL.Path, "/api/v1/groves/")
	}

	if path == "" {
		NotFound(w, "Project")
		return
	}

	// Parse the project ID (supports both UUID and {uuid}__{slug} format)
	// The project ID may contain "__" so we need to find the first "/"
	parts := strings.SplitN(path, "/", 2)
	projectIDRaw := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Skip the register endpoint - it's handled separately
	if projectIDRaw == "register" {
		NotFound(w, "Project")
		return
	}

	// Parse project ID to extract UUID (supports {uuid}__{slug} format)
	projectID := resolveProjectID(projectIDRaw)

	// Check for nested /agents path
	if strings.HasPrefix(subPath, "agents") {
		agentPath := strings.TrimPrefix(subPath, "agents")
		agentPath = strings.TrimPrefix(agentPath, "/")
		s.handleProjectAgents(w, r, projectID, agentPath)
		return
	}

	// Check for nested /env path
	if strings.HasPrefix(subPath, "env") {
		envPath := strings.TrimPrefix(subPath, "env")
		envPath = strings.TrimPrefix(envPath, "/")
		if envPath == "" {
			s.handleProjectEnvVars(w, r, projectID)
		} else {
			s.handleProjectEnvVarByKey(w, r, projectID, envPath)
		}
		return
	}

	// Check for nested /secrets path
	if strings.HasPrefix(subPath, "secrets") {
		secretPath := strings.TrimPrefix(subPath, "secrets")
		secretPath = strings.TrimPrefix(secretPath, "/")
		if secretPath == "" {
			s.handleProjectSecrets(w, r, projectID)
		} else {
			s.handleProjectSecretByKey(w, r, projectID, secretPath)
		}
		return
	}

	// Check for nested /providers path
	if strings.HasPrefix(subPath, "providers") {
		providerPath := strings.TrimPrefix(subPath, "providers")
		providerPath = strings.TrimPrefix(providerPath, "/")
		s.handleProjectProviders(w, r, projectID, providerPath)
		return
	}

	// Check for nested /shared-dirs path
	if strings.HasPrefix(subPath, "shared-dirs") {
		sdPath := strings.TrimPrefix(subPath, "shared-dirs")
		sdPath = strings.TrimPrefix(sdPath, "/")
		if sdPath == "" {
			s.handleProjectSharedDirs(w, r, projectID)
		} else {
			// Split into name and optional sub-path (e.g. "my-dir/files/some/path")
			parts := strings.SplitN(sdPath, "/", 2)
			name := parts[0]
			rest := ""
			if len(parts) > 1 {
				rest = parts[1]
			}
			if rest == "archive" {
				s.handleProjectSharedDirArchive(w, r, projectID, name)
			} else if strings.HasPrefix(rest, "files") {
				filePath := strings.TrimPrefix(rest, "files")
				filePath = strings.TrimPrefix(filePath, "/")
				s.handleSharedDirFiles(w, r, projectID, name, filePath)
			} else if rest == "" {
				s.handleProjectSharedDirByName(w, r, projectID, name)
			} else {
				NotFound(w, "Resource")
			}
		}
		return
	}

	// Check for nested /injected-skills path
	if strings.HasPrefix(subPath, "injected-skills") {
		siPath := strings.TrimPrefix(subPath, "injected-skills")
		siPath = strings.TrimPrefix(siPath, "/")
		siPath = strings.TrimSuffix(siPath, "/")
		if siPath == "" {
			s.handleProjectInjectedSkills(w, r, projectID)
		} else {
			s.handleProjectInjectedSkillByID(w, r, projectID, siPath)
		}
		return
	}

	// Check for nested /gcp-service-accounts path
	if strings.HasPrefix(subPath, "gcp-service-accounts") {
		saPath := strings.TrimPrefix(subPath, "gcp-service-accounts")
		saPath = strings.TrimPrefix(saPath, "/")
		if saPath == "" {
			s.handleProjectGCPServiceAccounts(w, r, projectID)
		} else {
			s.handleProjectGCPServiceAccountByID(w, r, projectID, saPath)
		}
		return
	}

	// Check for nested /message-logs path (project-level message audit log)
	if subPath == api.AgentActionMessageLogs {
		s.handleProjectMessageLogs(w, r, projectID)
		return
	}
	if subPath == api.AgentActionMessageLogsStream {
		s.handleProjectMessageLogsStream(w, r, projectID)
		return
	}

	// Check for nested /broadcast path (message broker broadcast)
	if subPath == "broadcast" {
		s.handleProjectBroadcast(w, r, projectID)
		return
	}

	// Check for nested /scheduled-events path
	if strings.HasPrefix(subPath, "scheduled-events") {
		eventPath := strings.TrimPrefix(subPath, "scheduled-events")
		eventPath = strings.TrimPrefix(eventPath, "/")
		s.handleScheduledEvents(w, r, projectID, eventPath)
		return
	}

	// Check for nested /schedules path (recurring schedules)
	if strings.HasPrefix(subPath, "schedules") {
		schedulePath := strings.TrimPrefix(subPath, "schedules")
		schedulePath = strings.TrimPrefix(schedulePath, "/")
		s.handleSchedules(w, r, projectID, schedulePath)
		return
	}

	// Check for nested /metrics-summary path (lightweight project metrics summary)
	if subPath == "metrics-summary" {
		s.handleProjectMetricsSummary(w, r, projectID)
		return
	}

	// Check for nested /metrics/summary path (DB-backed session metrics summary)
	if subPath == "metrics/summary" {
		s.handleProjectSessionMetricsSummary(w, r, projectID)
		return
	}

	// Check for nested /metrics path (project-scoped metrics dashboard)
	if subPath == "metrics" || strings.HasPrefix(subPath, "metrics/") {
		metricsPath := strings.TrimPrefix(subPath, "metrics")
		metricsPath = strings.TrimPrefix(metricsPath, "/")
		s.handleProjectMetricsDashboard(w, r, projectID, metricsPath)
		return
	}

	// Check for nested /pre-start-hooks path
	if subPath == "pre-start-hooks" || strings.HasPrefix(subPath, "pre-start-hooks/") {
		pshPath := strings.TrimPrefix(subPath, "pre-start-hooks")
		pshPath = strings.TrimPrefix(pshPath, "/")
		pshPath = strings.TrimSuffix(pshPath, "/")
		if pshPath == "" {
			s.handleProjectPreStartHooks(w, r, projectID)
		} else {
			s.handleProjectPreStartHookByID(w, r, projectID, pshPath)
		}
		return
	}

	// Check for nested /settings path
	if subPath == "settings" {
		s.handleProjectSettings(w, r, projectID)
		return
	}

	// Check for nested /settings/resolved path. Must be tested separately from
	// "settings" above rather than as a prefix: this is a distinct read-only
	// sub-resource with no PUT, not a mode of the settings endpoint.
	if subPath == "settings/resolved" {
		s.handleProjectSettingsResolved(w, r, projectID)
		return
	}

	// Check for nested /set-template path
	if subPath == "set-template" {
		s.handleSetTemplate(w, r, projectID)
		return
	}

	// Check for nested /clone path
	if subPath == "clone" {
		s.handleProjectClone(w, r, projectID)
		return
	}

	// Check for nested /discover-templates path
	if subPath == "discover-templates" {
		s.handleProjectDiscoverTemplates(w, r, projectID)
		return
	}

	// Check for nested /discover-harness-configs path
	if subPath == "discover-harness-configs" {
		s.handleProjectDiscoverHarnessConfigs(w, r, projectID)
		return
	}

	// Check for nested /import-templates path
	if subPath == "import-templates" {
		s.handleProjectImportTemplates(w, r, projectID)
		return
	}

	// Check for nested /import-harness-configs path
	if subPath == "import-harness-configs" {
		s.handleProjectImportHarnessConfigs(w, r, projectID)
		return
	}

	// Check for nested /dav/ path (WebDAV endpoint for project workspace sync)
	if strings.HasPrefix(subPath, "dav") {
		davPath := strings.TrimPrefix(subPath, "dav")
		davPath = strings.TrimPrefix(davPath, "/")
		s.handleProjectWebDAV(w, r, projectID, davPath)
		return
	}

	// Check for nested /sync/status path (sync metadata)
	if subPath == "sync/status" {
		s.handleProjectSyncStatus(w, r, projectID)
		return
	}

	// Check for nested /workspace/cache/ paths (linked project cache management)
	if subPath == "workspace/cache/refresh" {
		s.handleProjectCacheRefresh(w, r, projectID)
		return
	}
	if subPath == "workspace/cache/status" {
		s.handleProjectCacheStatus(w, r, projectID)
		return
	}
	if subPath == "workspace/cache/notify" {
		s.handleProjectCacheNotify(w, r, projectID)
		return
	}

	// Check for nested /workspace/pull path (git pull for shared-workspace projects)
	if subPath == "workspace/pull" {
		s.handleProjectWorkspacePull(w, r, projectID)
		return
	}

	// Check for nested /workspace/archive path (download workspace as zip)
	if subPath == "workspace/archive" {
		s.handleProjectWorkspaceArchive(w, r, projectID)
		return
	}

	// Check for nested /workspace/files path
	if strings.HasPrefix(subPath, "workspace/files") {
		filePath := strings.TrimPrefix(subPath, "workspace/files")
		filePath = strings.TrimPrefix(filePath, "/")
		s.handleProjectWorkspace(w, r, projectID, filePath)
		return
	}

	// Check for nested /github-installation path
	if subPath == "github-installation" {
		s.handleProjectGitHubInstallation(w, r, projectID)
		return
	}

	// Check for nested /github-status path
	if subPath == "github-status" {
		s.handleProjectGitHubStatus(w, r, projectID)
		return
	}

	// Check for nested /github-permissions path
	if subPath == "github-permissions" {
		s.handleProjectGitHubPermissions(w, r, projectID)
		return
	}

	// Check for nested /git-identity path
	if subPath == "git-identity" {
		s.handleProjectGitIdentity(w, r, projectID)
		return
	}

	// Otherwise handle as project resource
	s.handleProjectByIDInternal(w, r, projectID, subPath)
}

// handleProjectByIDInternal handles project resource operations
func (s *Server) handleProjectByIDInternal(w http.ResponseWriter, r *http.Request, projectID, subPath string) {
	// Only handle if no subpath (direct project resource)
	if subPath != "" {
		NotFound(w, "Project resource")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getProject(w, r, projectID)
	case http.MethodPatch:
		s.updateProject(w, r, projectID)
	case http.MethodDelete:
		s.deleteProject(w, r, projectID)
	default:
		MethodNotAllowed(w)
	}
}

// handleProjectAgents handles agent operations scoped to a project
// Path: /api/v1/projects/{projectId}/agents[/{agentId}[/{action}]]
func (s *Server) handleProjectAgents(w http.ResponseWriter, r *http.Request, projectID, agentPath string) {
	ctx := r.Context()

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

	// Handle stop-all (POST /api/v1/projects/{projectId}/agents/stop-all)
	if agentPath == "stop-all" {
		s.handleStopAllAgents(w, r, project.ID)
		return
	}

	// No agent ID - list or create agents in this project
	if agentPath == "" {
		switch r.Method {
		case http.MethodGet:
			s.listProjectAgents(w, r, project.ID)
		case http.MethodPost:
			s.createProjectAgent(w, r, project.ID)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	// Parse agent ID and action
	parts := strings.SplitN(agentPath, "/", 2)
	agentIDRaw := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	// Handle actions
	if action != "" {
		s.handleProjectAgentAction(w, r, project.ID, agentIDRaw, action)
		return
	}

	// Handle agent by ID within project
	switch r.Method {
	case http.MethodGet:
		s.getProjectAgent(w, r, project.ID, agentIDRaw)
	case http.MethodPatch:
		s.updateProjectAgent(w, r, project.ID, agentIDRaw)
	case http.MethodDelete:
		s.deleteProjectAgent(w, r, project.ID, agentIDRaw)
	default:
		MethodNotAllowed(w)
	}
}

// listProjectAgents lists agents within a specific project
func (s *Server) listProjectAgents(w http.ResponseWriter, r *http.Request, projectID string) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	query := r.URL.Query()

	filter := store.AgentFilter{
		ProjectID:       projectID,
		RuntimeBrokerID: query.Get("runtimeBrokerId"),
		Phase:           query.Get("phase"),
		IncludeDeleted:  query.Get("includeDeleted") == "true",
	}

	if labelParams := query["label"]; len(labelParams) > 0 {
		parsed, err := parseLabelFilters(labelParams)
		if err != nil {
			BadRequest(w, err.Error())
			return
		}
		filter.Labels = parsed
	}

	limit := 500
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListAgents(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Enrich agents with project and broker names
	s.enrichAgents(ctx, result.Items)

	// Compute per-item and scope capabilities
	identity := GetIdentityFromContext(ctx)
	agents := make([]AgentWithCapabilities, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = agentResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "agent")
		for i := range result.Items {
			agents[i] = AgentWithCapabilities{Agent: result.Items[i], Cap: caps[i]}
		}
	} else {
		for i := range result.Items {
			agents[i] = AgentWithCapabilities{Agent: result.Items[i]}
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "project", projectID, "agent")
	}

	writeJSON(w, http.StatusOK, ListAgentsResponse{
		Agents:       agents,
		NextCursor:   result.NextCursor,
		TotalCount:   result.TotalCount,
		ServerTime:   time.Now().UTC(),
		Capabilities: scopeCap,
	})
}

// createProjectAgent creates an agent within a specific project
func (s *Server) createProjectAgent(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	var req CreateAgentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if req.CleanupMode != "" && req.CleanupMode != "strict" && req.CleanupMode != "force" {
		ValidationError(w, "cleanupMode must be 'strict' or 'force'", nil)
		return
	}
	if err := labels.Validate(req.Labels); err != nil {
		ValidationError(w, "Invalid labels: "+err.Error(), nil)
		return
	}

	// Authorization for every caller kind. This route had no gate at all before
	// #591: any agent token could create an agent in any project, and it is the
	// route the CLI uses for create/start/sync. The URL project implies no
	// isolation by itself.
	if !s.authorizeAgentCreate(w, r, projectID) {
		return
	}

	// Resolve caller identity for creator tracking (attribution only — the
	// authorization decision is made above).
	var createdBy string
	var creatorName string
	var ancestry []string
	var notifySubscriberType, notifySubscriberID string
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		createdBy = agentIdent.ID()
		if creatorAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
			creatorName = creatorAgent.Name
			notifySubscriberType = store.SubscriberTypeAgent
			notifySubscriberID = creatorAgent.Slug
			// Build ancestry: creator's ancestry + creator's ID
			ancestry = append(ancestry, creatorAgent.Ancestry...)
			ancestry = append(ancestry, creatorAgent.ID)
		}
	} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		createdBy = userIdent.ID()
		creatorName = userIdent.Email()
		notifySubscriberType = store.SubscriberTypeUser
		notifySubscriberID = userIdent.ID()
		// User-created agents: ancestry is [userID]
		ancestry = []string{userIdent.ID()}
	}
	s.createAgentInProject(w, r, req, projectID, createdBy, creatorName, ancestry, notifySubscriberType, notifySubscriberID)
}

// getProjectAgent gets an agent by ID within a specific project
func (s *Server) getProjectAgent(w http.ResponseWriter, r *http.Request, projectID, agentID string) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()

	// Try to get by slug first (more common case)
	agent, err := s.store.GetAgentBySlug(ctx, projectID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			// Try by UUID
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			// Verify it belongs to this project
			if agent.ProjectID != projectID {
				NotFound(w, "Agent")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Enrich agent with project and broker names
	s.enrichAgent(ctx, agent, nil, nil)

	writeJSON(w, http.StatusOK, agent)
}

// updateProjectAgent updates an agent within a specific project
func (s *Server) updateProjectAgent(w http.ResponseWriter, r *http.Request, projectID, agentID string) {
	ctx := r.Context()

	// Try to get by slug first
	agent, err := s.store.GetAgentBySlug(ctx, projectID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			// Try by UUID
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if agent.ProjectID != projectID {
				NotFound(w, "Agent")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	var updates struct {
		Name         string            `json:"name,omitempty"`
		Labels       map[string]string `json:"labels,omitempty"`
		Annotations  map[string]string `json:"annotations,omitempty"`
		TaskSummary  string            `json:"taskSummary,omitempty"`
		StateVersion int64             `json:"stateVersion"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Check version for optimistic locking
	if updates.StateVersion != 0 && updates.StateVersion != agent.StateVersion {
		Conflict(w, "Version conflict - resource was modified")
		return
	}

	// Apply updates
	if updates.Name != "" {
		agent.Name = updates.Name
	}
	if updates.Labels != nil {
		if err := labels.Validate(updates.Labels); err != nil {
			ValidationError(w, "Invalid labels: "+err.Error(), nil)
			return
		}
		agent.Labels = updates.Labels
	}
	if updates.Annotations != nil {
		agent.Annotations = updates.Annotations
	}
	if updates.TaskSummary != "" {
		agent.TaskSummary = updates.TaskSummary
	}

	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, agent)
}

// deleteProjectAgent deletes an agent within a specific project
func (s *Server) deleteProjectAgent(w http.ResponseWriter, r *http.Request, projectID, agentID string) {
	ctx := r.Context()

	// Try to get by slug first to verify project membership
	agent, err := s.store.GetAgentBySlug(ctx, projectID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			// Try by UUID
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if agent.ProjectID != projectID {
				NotFound(w, "Agent")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	s.performAgentDelete(w, r, agent)
}

// handleProjectAgentAction handles actions on agents within a project
func (s *Server) handleProjectAgentAction(w http.ResponseWriter, r *http.Request, projectID, agentID, action string) {
	// Agent logs relay (GET, proxied to broker); handle before the POST-only gate.
	if action == "logs" {
		resolvedAgent, err := s.resolveProjectAgent(r.Context(), projectID, agentID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		s.handleAgentLogs(w, r, resolvedAgent.ID)
		return
	}

	// Cloud-logs actions are GET endpoints; handle before the POST-only gate.
	if action == "cloud-logs" || action == "cloud-logs/stream" {
		resolvedAgent, err := s.resolveProjectAgent(r.Context(), projectID, agentID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if action == "cloud-logs" {
			s.handleAgentCloudLogs(w, r, resolvedAgent.ID)
		} else {
			s.handleAgentCloudLogsStream(w, r, resolvedAgent.ID)
		}
		return
	}

	// Message-logs actions are GET endpoints; handle before the POST-only gate.
	if action == api.AgentActionMessageLogs || action == api.AgentActionMessageLogsStream {
		resolvedAgent, err := s.resolveProjectAgent(r.Context(), projectID, agentID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if action == api.AgentActionMessageLogs {
			s.handleAgentMessageLogs(w, r, resolvedAgent.ID)
		} else {
			s.handleAgentMessageLogsStream(w, r, resolvedAgent.ID)
		}
		return
	}

	// NOTE: messages/stream is intentionally NOT routed here. The project-
	// scoped path only serves message-logs endpoints (Cloud Logging).
	// The hub-store-backed messages/stream is agent-scoped only
	// (/api/v1/agents/{id}/messages/stream), matching handleAgentByID.

	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	// Resolve agent ID
	agent, err := s.store.GetAgentBySlug(ctx, projectID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				writeError(w, http.StatusNotFound, ErrCodeAgentNotFound,
					fmt.Sprintf("Agent %q not found in project", agentID),
					map[string]interface{}{"agent_slug": agentID, "project_id": projectID})
				return
			}
			if agent.ProjectID != projectID {
				writeError(w, http.StatusNotFound, ErrCodeAgentNotFound,
					fmt.Sprintf("Agent %q not found in project", agentID),
					map[string]interface{}{"agent_slug": agentID, "project_id": projectID})
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// set_message_mode action: own permission model (D7).
	if action == api.AgentActionSetMessageMode {
		s.handleSetMessageMode(w, r, agent.ID)
		return
	}

	// Message action: route through authorizeAgentMessage (D1/D8).
	if action == api.AgentActionMessage {
		identity := GetIdentityFromContext(r.Context())
		if identity == nil {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"This action requires user or agent authentication", nil)
			return
		}
		isSystemPlane := false
		allowed, reason := s.authorizeAgentMessage(r.Context(), identity, agent, isSystemPlane)
		if !allowed {
			slog.Warn("message authorization denied",
				"sender_type", identity.Type(),
				"sender_id", identity.ID(),
				"target_agent", agent.ID,
				"reason", reason,
			)
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Message delivery denied", nil)
			return
		}
		// Skip lifecycle authorization for messages — dispatch directly.
		s.handleAgentMessage(w, r, agent.ID)
		return
	}

	// For interactive actions, enforce lifecycle authorization for every caller
	// kind: users via policy, agents via ScopeAgentLifecycle within their own
	// project, everything else denied. authorizeAgentLifecycle logs the denial.
	switch action {
	case api.AgentActionStart, api.AgentActionStop, api.AgentActionSuspend, api.AgentActionRestart, api.AgentActionExec, api.AgentActionResetAuth, api.AgentActionEnv, api.AgentActionRestore:
		if !s.authorizeAgentLifecycle(w, r, agent) {
			return
		}
	}

	switch action {
	case api.AgentActionStatus:
		s.updateAgentStatus(w, r, agent.ID)
	case api.AgentActionStart, api.AgentActionStop, api.AgentActionSuspend, api.AgentActionRestart:
		s.handleAgentLifecycle(w, r, agent.ID, action)
	case api.AgentActionExec:
		s.handleAgentExec(w, r, agent.ID)
	case api.AgentActionEnv:
		s.submitAgentEnv(w, r, projectID, agentID)
	case api.AgentActionRestore:
		s.restoreAgent(w, r, agent.ID)
	case api.AgentActionOutboundMessage:
		s.handleAgentOutboundMessage(w, r, agent.ID)
	case api.AgentActionResetAuth:
		s.handleAgentResetAuth(w, r, agent.ID)
	default:
		NotFound(w, "Action")
	}
}

// resolveProjectID extracts the UUID from a project ID that may be in {uuid}__{slug} format
func resolveProjectID(projectIDRaw string) string {
	id, _, ok := api.ParseProjectID(projectIDRaw)
	if ok {
		return id
	}
	// Not in hosted format - return as-is (may be just a UUID or slug)
	return projectIDRaw
}

// handleProjectByID is deprecated - use handleProjectRoutes instead
//
//nolint:unused // Kept for legacy route compatibility.
func (s *Server) handleProjectByID(w http.ResponseWriter, r *http.Request) {
	var id string
	if strings.HasPrefix(r.URL.Path, "/api/v1/projects") {
		id = extractID(r, "/api/v1/projects")
	} else {
		id = extractID(r, "/api/v1/groves")
	}

	if id == "" || id == "register" {
		// Handled by handleProjectRegister
		NotFound(w, "Project")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getProject(w, r, id)
	case http.MethodPatch:
		s.updateProject(w, r, id)
	case http.MethodDelete:
		s.deleteProject(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, id string) {
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	project, err := s.store.GetProject(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Ensure associated groups exist (backfill for projects created before
	// group support was added). These calls are idempotent.
	s.createProjectGroup(ctx, project)

	// SECURITY-GATE: CheckAccess — verify read access to individual project
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		NotFound(w, "Project")
		return
	}
	if decision := s.authzService.CheckAccess(ctx, identity, projectResource(project), ActionRead); !decision.Allowed {
		NotFound(w, "Project")
		return
	}
	s.createProjectMembersGroup(ctx, project)

	// Enrich owner display name
	if project.OwnerID != "" {
		if user, err := s.store.GetUser(ctx, project.OwnerID); err == nil {
			if user.DisplayName != "" {
				project.OwnerName = user.DisplayName
			} else {
				project.OwnerName = user.Email
			}
		}
	}

	resp := ProjectWithCapabilities{Project: *project, CloudLogging: s.logQueryService != nil}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, projectResource(project))
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if !s.authorize(w, r, projectResource(project), ActionUpdate) {
		return
	}

	var updates struct {
		Name                   string            `json:"name,omitempty"`
		Slug                   string            `json:"slug,omitempty"`
		Labels                 map[string]string `json:"labels,omitempty"`
		DefaultRuntimeBrokerID string            `json:"defaultRuntimeBrokerId,omitempty"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	oldSlug := project.Slug

	if updates.Name != "" {
		project.Name = updates.Name
	}
	if updates.Slug != "" {
		newSlug := api.Slugify(updates.Slug)
		if newSlug == "" {
			BadRequest(w, "Invalid slug: must contain at least one alphanumeric character")
			return
		}
		if newSlug != oldSlug {
			existing, err := s.store.GetProjectBySlug(ctx, newSlug)
			if err != nil && err != store.ErrNotFound {
				writeErrorFromErr(w, err, "")
				return
			}
			if err == nil && existing.ID != project.ID {
				writeError(w, http.StatusConflict, ErrCodeConflict,
					fmt.Sprintf("A project with slug %q already exists", newSlug), nil)
				return
			}
			project.Slug = newSlug
		}
	}
	if updates.Labels != nil {
		project.Labels = updates.Labels
	}
	if updates.DefaultRuntimeBrokerID != "" {
		project.DefaultRuntimeBrokerID = updates.DefaultRuntimeBrokerID
	}

	if err := s.store.UpdateProject(ctx, project); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// If the slug changed, update associated group slugs and filesystem paths.
	if project.Slug != oldSlug {
		s.migrateProjectSlug(ctx, project, oldSlug)
	}

	s.events.PublishProjectUpdated(ctx, project)

	writeJSON(w, http.StatusOK, project)
}

// migrateProjectSlug updates group slugs and filesystem paths after a project slug change.
// This is best-effort: failures are logged but don't roll back the rename.
func (s *Server) migrateProjectSlug(ctx context.Context, project *store.Project, oldSlug string) {
	newSlug := project.Slug

	// Migrate the project agents group slug.
	oldAgentsSlug := "project:" + oldSlug + ":agents"
	newAgentsSlug := "project:" + newSlug + ":agents"
	if group, err := s.store.GetGroupBySlug(ctx, oldAgentsSlug); err == nil {
		group.Slug = newAgentsSlug
		group.Name = project.Name + " Agents"
		if err := s.store.UpdateGroup(ctx, group); err != nil {
			s.projectsLogger().Warn("failed to migrate project agents group slug",
				"project_id", project.ID, "old_slug", oldAgentsSlug, "new_slug", newAgentsSlug, "error", err)
		}
	} else if err != store.ErrNotFound {
		s.projectsLogger().Warn("failed to retrieve project agents group for migration",
			"project_id", project.ID, "old_slug", oldAgentsSlug, "error", err)
	}

	// Migrate the project members group slug.
	oldMembersSlug := "project:" + oldSlug + ":members"
	newMembersSlug := "project:" + newSlug + ":members"
	if group, err := s.store.GetGroupBySlug(ctx, oldMembersSlug); err == nil {
		group.Slug = newMembersSlug
		group.Name = project.Name + " Members"
		if err := s.store.UpdateGroup(ctx, group); err != nil {
			s.projectsLogger().Warn("failed to migrate project members group slug",
				"project_id", project.ID, "old_slug", oldMembersSlug, "new_slug", newMembersSlug, "error", err)
		}
	} else if err != store.ErrNotFound {
		s.projectsLogger().Warn("failed to retrieve project members group for migration",
			"project_id", project.ID, "old_slug", oldMembersSlug, "error", err)
	}

	// CO1: Legacy policy migration removed. Policies are no longer the
	// source of authority; project-scoped RoleBindings are scope-keyed by
	// project ID (not slug), so no rename is needed.

	// Migrate hub-managed project filesystem paths (best-effort).
	// Derive newPath from oldPath's parent to preserve the directory type (groves/ vs projects/).
	if oldPath, err := s.hubManagedProjectPath(oldSlug); err == nil {
		if _, statErr := os.Stat(oldPath); statErr == nil {
			newPath := filepath.Join(filepath.Dir(oldPath), newSlug)
			if _, statErr := os.Stat(newPath); os.IsNotExist(statErr) {
				if err := os.Rename(oldPath, newPath); err != nil {
					s.projectsLogger().Warn("failed to rename project workspace directory",
						"project_id", project.ID, "old_path", oldPath, "new_path", newPath, "error", err)
				}
			}
		}
	}

	// Migrate the project config directory (~/.scion/project-configs/<slug>__<short-uuid>/).
	oldMarker := &config.ProjectMarker{
		ProjectID:   project.ID,
		ProjectSlug: oldSlug,
	}
	newMarker := &config.ProjectMarker{
		ProjectID:   project.ID,
		ProjectSlug: newSlug,
	}
	if oldConfigPath, err := oldMarker.ExternalProjectPath(); err == nil {
		if newConfigPath, err := newMarker.ExternalProjectPath(); err == nil {
			oldConfigDir := filepath.Dir(oldConfigPath)
			newConfigDir := filepath.Dir(newConfigPath)
			if _, statErr := os.Stat(oldConfigDir); statErr == nil {
				if _, statErr := os.Stat(newConfigDir); os.IsNotExist(statErr) {
					if err := os.MkdirAll(filepath.Dir(newConfigDir), 0755); err == nil {
						if err := os.Rename(oldConfigDir, newConfigDir); err != nil {
							s.projectsLogger().Warn("failed to rename project config directory",
								"project_id", project.ID, "old_path", oldConfigDir, "new_path", newConfigDir, "error", err)
						}
					}
				}
			}
		}
	}

	// The old slug is now unreachable, so its ephemeral-path warning
	// suppression can never be consulted again — drop it, or a later project
	// reusing the slug inherits the suppression and never warns.
	//
	// As in deleteProject, the invariant is that this follows the last
	// hubManagedProjectPath(oldSlug) call, not that it follows the last read
	// of oldSlug — reads that resolve no path cannot re-record. That call
	// above re-records it on gke-shared-volume, where the resolution takes
	// the warning path, so an eviction placed before it is undone by it.
	// Nothing reads oldSlug after this point today, but that is incidental;
	// the resolution is the thing to order against. Best-effort even here: a
	// resolution already in flight on another goroutine can re-insert it.
	s.warnedEphemeralProjects.Delete(oldSlug)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Fetch the project record before deletion so we can clean up the filesystem.
	project, err := s.store.GetProject(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if !s.authorize(w, r, projectResource(project), ActionDelete) {
		return
	}

	// Dispatch agent deletions to runtime brokers so containers are stopped
	// and agent files are cleaned up. The DB cascade will remove agent records,
	// but we need the broker to tear down the actual resources first.
	s.deleteProjectAgents(ctx, project)

	// Clean up all groups associated with the project (agents group, members group, etc.)
	if projectGroups, err := s.store.ListGroups(ctx, store.GroupFilter{ProjectID: id}, store.ListOptions{Limit: 100}); err == nil {
		for _, g := range projectGroups.Items {
			if delErr := s.store.DeleteGroup(ctx, g.ID); delErr != nil {
				s.projectsLogger().Warn("failed to delete project group", "project_id", id, "group", g.ID, "slug", g.Slug, "error", delErr.Error())
			}
		}
	}

	// CO1: Legacy policy cleanup removed. Policies are dead data.

	// Cascade-delete all project-scoped role bindings (XL review R1).
	// This removes all membership (owner/admin/member) bindings and the
	// hub-members visibility binding for this project. Must run before
	// DeleteProject so the scope reference is still valid for audit.
	if n, err := s.store.DeleteRoleBindingsForScope(ctx, store.RoleScopeProject, id); err != nil {
		s.projectsLogger().Warn("failed to cascade-delete project role bindings", "project_id", id, "error", err)
	} else if n > 0 {
		s.projectsLogger().Info("cascade-deleted project role bindings", "project_id", id, "count", n)
	}

	// Clean up project-scoped env vars (best-effort).
	// These use scope/scope_id without FK cascade.
	if n, err := s.store.DeleteEnvVarsByScope(ctx, store.ScopeProject, id); err != nil {
		s.projectsLogger().Warn("failed to delete project env vars", "project_id", id, "error", err)
	} else if n > 0 {
		s.projectsLogger().Info("deleted project env vars", "project_id", id, "count", n)
	}

	// Clean up project-scoped secrets (best-effort).
	if n, err := s.store.DeleteSecretsByScope(ctx, store.ScopeProject, id); err != nil {
		s.projectsLogger().Warn("failed to delete project secrets", "project_id", id, "error", err)
	} else if n > 0 {
		s.projectsLogger().Info("deleted project secrets", "project_id", id, "count", n)
	}

	// Clean up project-scoped skill injections (best-effort).
	// These use scope/scope_id without FK cascade.
	if n, err := s.store.DeleteSkillInjectionsByScope(ctx, store.SkillInjectionScopeProject, id); err != nil {
		s.projectsLogger().Warn("failed to delete project skill injections", "project_id", id, "error", err)
	} else if n > 0 {
		s.projectsLogger().Info("deleted project skill injections", "project_id", id, "count", n)
	}

	// Warn about retained managed GCP service accounts (best-effort).
	// Managed SAs are NOT deleted from GCP — only unlinked from the project.
	s.warnManagedGCPServiceAccounts(ctx, id)

	// Clean up project-scoped GCP service account registrations (best-effort).
	if sas, err := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: id,
	}); err == nil {
		for _, sa := range sas {
			if delErr := s.store.DeleteGCPServiceAccount(ctx, sa.ID); delErr != nil {
				s.projectsLogger().Warn("failed to delete project GCP service account registration",
					"project_id", id, "sa_id", sa.ID, "email", sa.Email, "error", delErr.Error())
			}
		}
	}

	// Clean up project-scoped templates (best-effort), including storage files.
	s.deleteProjectTemplates(ctx, id)

	// Clean up project-scoped harness configs (best-effort), including storage files.
	s.deleteProjectHarnessConfigs(ctx, id)

	// For hub-native and shared-workspace projects, notify provider brokers to clean up
	// their local project directories. This must run before DeleteProject because
	// the cascade deletes the project_providers we need to enumerate.
	if project.GitRemote == "" || project.IsSharedWorkspace() {
		s.cleanupBrokerProjectDirectories(ctx, project)
	}

	if err := s.store.DeleteProject(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Release quota reservation for the deleted project (best-effort).
	if s.quotaService != nil {
		s.quotaService.Release(ctx, "max_projects_per_user", id)
	}

	// For hub-native and shared-workspace projects, remove the filesystem directory
	// and clean up the per-project WebDAV lock store to prevent memory leaks.
	if (project.GitRemote == "" || project.IsSharedWorkspace()) && project.Slug != "" {
		if projectPath, err := s.hubManagedProjectPath(project.Slug); err == nil {
			if err := util.RemoveAllSafe(projectPath); err != nil {
				s.projectsLogger().Warn("failed to remove hub-managed project directory",
					"project_id", id, "slug", project.Slug, "path", projectPath, "error", err)
			}
		}
	}
	s.webdavLocks.Delete(id)
	// Same reason, keyed by slug rather than ID: drop the once-per-project
	// ephemeral-path warning suppression so a slug that is deleted and later
	// recreated warns again instead of inheriting the old suppression.
	//
	// Order matters, and not marginally. The hubManagedProjectPath call above
	// can re-record this slug: on gke-shared-volume it takes the legacy-local
	// fallback and warns, and a project served from that fallback is exactly
	// the population that has an entry here — so evicting before that call
	// would reliably reinstate the entry it just removed, not occasionally.
	// The invariant is about resolutions, not reads: the eviction has to
	// follow the last hubManagedProjectPath call for this slug. The
	// project-configs cleanup below reads the slug three times after it —
	// its guard, the marker, and the failure log — and all three are
	// harmless, because none of them calls hubManagedProjectPath, and only
	// that resolution re-records.
	s.warnedEphemeralProjects.Delete(project.Slug)

	// Clean up the project-configs directory (~/.scion/project-configs/<slug>__<short-uuid>/).
	// This stores external settings, templates, and agent homes for both
	// git-backed linked projects and non-git external projects.
	if project.Slug != "" && project.ID != "" {
		marker := &config.ProjectMarker{
			ProjectID:   project.ID,
			ProjectSlug: project.Slug,
		}
		if configPath, err := marker.ExternalProjectPath(); err == nil {
			// ExternalProjectPath returns <project-configs>/<slug__uuid>/.scion —
			// remove the parent (<slug__uuid>) directory.
			projectConfigDir := filepath.Dir(configPath)
			if err := config.RemoveProjectConfig(projectConfigDir); err != nil && !os.IsNotExist(err) {
				s.projectsLogger().Warn("failed to remove project config directory",
					"project_id", id, "slug", project.Slug, "path", projectConfigDir, "error", err)
			}
		}
	}

	s.events.PublishProjectDeleted(ctx, id)

	w.WriteHeader(http.StatusNoContent)
}

// deleteProjectAgents dispatches deletion of all agents in a project to their
// runtime brokers. This is best-effort: failures are logged but do not block
// project deletion. The database cascade will remove agent records regardless.
func (s *Server) deleteProjectAgents(ctx context.Context, project *store.Project) {
	dispatcher := s.GetDispatcher()

	result, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{Limit: 1000})
	if err != nil {
		s.agentLifecycleLog.Warn("failed to list agents for project deletion", "project_id", project.ID, "error", err)
		return
	}

	now := time.Now()
	for _, agent := range result.Items {
		if !agent.DeletedAt.IsZero() {
			continue
		}
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			if err := dispatcher.DispatchAgentDelete(ctx, &agent, true, true, false, now); err != nil {
				s.agentLifecycleLog.Warn("failed to dispatch agent delete during project deletion",
					"agent_id", agent.ID, "broker", agent.RuntimeBrokerID, "error", err)
			}
		}
		s.events.PublishAgentDeleted(ctx, agent.ID, agent.ProjectID)
	}
}

// deleteProjectTemplates deletes all project-scoped templates including their
// storage files (GCS/local). This is best-effort: failures are logged but
// do not block project deletion.
func (s *Server) deleteProjectTemplates(ctx context.Context, projectID string) {
	// List all project-scoped templates so we can clean up their storage files.
	templates, err := s.store.ListTemplates(ctx, store.TemplateFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		s.projectsLogger().Warn("failed to list project templates for deletion", "project_id", projectID, "error", err)
	} else if stor := s.GetStorage(); stor != nil {
		for _, tmpl := range templates.Items {
			if tmpl.StoragePath != "" {
				if err := stor.DeletePrefix(ctx, tmpl.StoragePath); err != nil {
					s.projectsLogger().Warn("failed to delete template storage files",
						"project_id", projectID, "template", tmpl.ID, "path", tmpl.StoragePath, "error", err)
				}
			}
		}
	}

	if n, err := s.store.DeleteTemplatesByScope(ctx, store.ScopeProject, projectID); err != nil {
		s.projectsLogger().Warn("failed to delete project templates", "project_id", projectID, "error", err)
	} else if n > 0 {
		s.projectsLogger().Info("deleted project templates", "project_id", projectID, "count", n)
	}
}

// warnManagedGCPServiceAccounts logs a warning for any hub-minted GCP service
// accounts that will be retained in GCP when a project is deleted.
func (s *Server) warnManagedGCPServiceAccounts(ctx context.Context, projectID string) {
	managed := true
	sas, err := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Managed: &managed,
	})
	if err != nil {
		s.projectsLogger().Warn("failed to list managed GCP SAs for project deletion warning",
			"project_id", projectID, "error", err)
		return
	}
	for _, sa := range sas {
		s.projectsLogger().Warn("project deletion: managed GCP service account retained in GCP — manual cleanup may be required",
			"project_id", projectID, "sa_email", sa.Email, "sa_id", sa.ID, "project_id", sa.ProjectID)
	}
}

// deleteProjectHarnessConfigs deletes all project-scoped harness configs including
// their storage files (GCS/local). This is best-effort: failures are logged
// but do not block project deletion.
func (s *Server) deleteProjectHarnessConfigs(ctx context.Context, projectID string) {
	// List all project-scoped harness configs so we can clean up their storage files.
	configs, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
	}, store.ListOptions{Limit: 1000})
	if err != nil {
		s.projectsLogger().Warn("failed to list project harness configs for deletion", "project_id", projectID, "error", err)
	} else if stor := s.GetStorage(); stor != nil {
		for _, hc := range configs.Items {
			if hc.StoragePath != "" {
				if err := stor.DeletePrefix(ctx, hc.StoragePath); err != nil {
					s.projectsLogger().Warn("failed to delete harness config storage files",
						"project_id", projectID, "harnessConfig", hc.ID, "path", hc.StoragePath, "error", err)
				}
			}
		}
	}

	if n, err := s.store.DeleteHarnessConfigsByScope(ctx, store.ScopeProject, projectID); err != nil {
		s.projectsLogger().Warn("failed to delete project harness configs", "project_id", projectID, "error", err)
	} else if n > 0 {
		s.projectsLogger().Info("deleted project harness configs", "project_id", projectID, "count", n)
	}
}

// cleanupBrokerProjectDirectories notifies provider brokers to remove their local
// copies of a hub-managed project directory. This is best-effort: failures are
// logged but do not block project deletion. The embedded broker is skipped
// because the hub already cleans up its own filesystem copy.
func (s *Server) cleanupBrokerProjectDirectories(ctx context.Context, project *store.Project) {
	if project.Slug == "" {
		return
	}

	providers, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		s.projectsLogger().Warn("failed to get project providers for cleanup", "project_id", project.ID, "error", err)
		return
	}

	if len(providers) == 0 {
		return
	}

	// Get the RuntimeBrokerClient from the dispatcher.
	var client RuntimeBrokerClient
	if disp := s.GetDispatcher(); disp != nil {
		if httpDisp, ok := disp.(*HTTPAgentDispatcher); ok {
			client = httpDisp.GetClient()
		}
	}
	if client == nil {
		s.projectsLogger().Warn("no RuntimeBrokerClient available for project cleanup dispatch", "project_id", project.ID)
		return
	}

	for _, provider := range providers {
		// Skip the embedded broker — the hub already cleans up its own copy.
		if s.isEmbeddedBroker(provider.BrokerID) {
			continue
		}

		broker, err := s.store.GetRuntimeBroker(ctx, provider.BrokerID)
		if err != nil {
			s.projectsLogger().Warn("failed to get broker for project cleanup",
				"project_id", project.ID, "broker", provider.BrokerID, "error", err)
			continue
		}

		if err := client.CleanupProject(ctx, provider.BrokerID, broker.Endpoint, project.Slug, project.ID); err != nil {
			s.projectsLogger().Warn("failed to cleanup project on broker",
				"project_id", project.ID, "slug", project.Slug,
				"broker", provider.BrokerID, "endpoint", broker.Endpoint, "error", err)
		}
	}
}
