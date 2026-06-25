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
	Visibility    string            `json:"visibility,omitempty"`
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
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.ProjectFilter{
		OwnerID:    query.Get("ownerId"),
		Visibility: query.Get("visibility"),
		GitRemote:  util.NormalizeGitRemote(query.Get("gitRemote")),
		BrokerID:   query.Get("brokerId"),
		Name:       query.Get("name"),
		Slug:       query.Get("slug"),
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

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListProjects(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Enrich owner display names
	s.enrichProjectOwnerNames(ctx, result.Items)

	// Compute per-item and scope capabilities
	identity := GetIdentityFromContext(ctx)
	projects := make([]ProjectWithCapabilities, 0, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = projectResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "project")
		for i := range result.Items {
			if !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			projects = append(projects, ProjectWithCapabilities{Project: result.Items[i], Cap: caps[i]})
		}
	} else {
		for i := range result.Items {
			projects = append(projects, ProjectWithCapabilities{Project: result.Items[i]})
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "project")
	}

	totalCount := result.TotalCount
	if identity != nil {
		totalCount = len(projects)
	}

	writeJSON(w, http.StatusOK, ListProjectsResponse{
		Projects:     projects,
		LegacyGroves: projects,
		NextCursor:   result.NextCursor,
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
			s.createProjectMembersGroupAndPolicy(ctx, existing, callerID)
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
		case store.WorkspaceModeShared, store.WorkspaceModeWorktreePerAgent:
			if req.Labels == nil {
				req.Labels = make(map[string]string)
			}
			req.Labels[store.LabelWorkspaceMode] = req.WorkspaceMode
		}
	}

	project := &store.Project{
		ID:         projectID,
		Name:       displayName,
		Slug:       slug,
		GitRemote:  normalizedRemote,
		Labels:     req.Labels,
		Visibility: req.Visibility,
	}

	if project.Visibility == "" {
		project.Visibility = store.VisibilityPrivate
	}

	// Set ownership from authenticated user
	if user := GetUserIdentityFromContext(ctx); user != nil {
		project.CreatedBy = user.ID()
		project.OwnerID = user.ID()
	}

	if err := s.store.CreateProject(ctx, project); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Create the associated project_agents group (best-effort)
	s.createProjectGroup(ctx, project)

	// Create project members group and policy (best-effort)
	s.createProjectMembersGroupAndPolicy(ctx, project)

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
			InjectionMode: "as_needed",
			CreatedBy:     project.CreatedBy,
			UpdatedBy:     project.CreatedBy,
		}
		if _, _, err := s.secretBackend.Set(ctx, tokenInput); err != nil {
			slog.Error("failed to save GitHub token as project secret",
				"project_id", project.ID, "error", err)
			if delErr := s.store.DeleteProject(ctx, project.ID); delErr != nil {
				slog.Warn("failed to clean up project record after secret save failure",
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
			slog.Error("shared workspace clone failed, rolling back project creation",
				"project_id", project.ID, "slug", project.Slug, "error", err)
			if req.GitHubToken != "" && s.secretBackend != nil && project.GitRemote != "" {
				if delErr := s.secretBackend.Delete(ctx, "GITHUB_TOKEN", secret.ScopeProject, project.ID); delErr != nil {
					slog.Warn("failed to clean up project secret after clone failure",
						"project_id", project.ID, "error", delErr)
				}
			}
			if delErr := s.store.DeleteProject(ctx, project.ID); delErr != nil {
				slog.Warn("failed to clean up project record after clone failure",
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
			slog.Warn("failed to initialize project workspace",
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
	}
	if err := s.store.CreateGroup(ctx, projectGroup); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			slog.Warn("failed to create project group", "project_id", project.ID, "error", err.Error())
			return
		}
		// Slug conflict — look it up and ensure project_id is current
		existing, lookupErr := s.store.GetGroupBySlug(ctx, agentsSlug)
		if lookupErr != nil {
			slog.Warn("failed to look up existing project agents group by slug",
				"project_id", project.ID, "slug", agentsSlug, "error", lookupErr.Error())
			return
		}
		if existing.ProjectID != project.ID {
			existing.ProjectID = project.ID
			if updateErr := s.store.UpdateGroup(ctx, existing); updateErr != nil {
				slog.Warn("failed to update existing project agents group",
					"project_id", project.ID, "slug", agentsSlug, "error", updateErr.Error())
			}
		}
	}
}

// createProjectMembersGroupAndPolicy creates an explicit members group for a project
// and a policy allowing members to create agents. Best-effort; failures are logged.
// If the group already exists (e.g., project was deleted and recreated with the same
// slug), the existing group is reused and the creator is still added as a member.
// callerUserID, when non-empty, is also added as an owner of the members group
// (e.g. the user who linked the project). It is safe to pass the same value as
// project.CreatedBy — duplicate additions are handled gracefully.
func (s *Server) createProjectMembersGroupAndPolicy(ctx context.Context, project *store.Project, callerUserID ...string) {
	membersSlug := "project:" + project.Slug + ":members"

	slog.Debug("ensuring project members group",
		"project_id", project.ID, "slug", project.Slug, "membersSlug", membersSlug)

	// Create project members group, or look up the existing one
	membersGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      project.Name + " Members",
		Slug:      membersSlug,
		GroupType: store.GroupTypeExplicit,
		ProjectID: project.ID,
		OwnerID:   project.OwnerID,
		CreatedBy: project.CreatedBy,
	}
	if err := s.store.CreateGroup(ctx, membersGroup); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			slog.Warn("failed to create project members group", "project_id", project.ID, "error", err.Error())
			return
		}
		// Slug conflict — look up existing group
		existing, lookupErr := s.store.GetGroupBySlug(ctx, membersSlug)
		if lookupErr != nil {
			slog.Warn("failed to look up existing project members group by slug",
				"project_id", project.ID, "slug", membersSlug, "error", lookupErr.Error())
			return
		}
		membersGroup = existing
		// Update the project ID association or owner in case they changed (recreated project
		// or backfill for groups created before OwnerID was set).
		needsUpdate := false
		if membersGroup.ProjectID != project.ID {
			membersGroup.ProjectID = project.ID
			needsUpdate = true
		}
		if membersGroup.OwnerID == "" && project.OwnerID != "" {
			membersGroup.OwnerID = project.OwnerID
			needsUpdate = true
		}
		if needsUpdate {
			if updateErr := s.store.UpdateGroup(ctx, membersGroup); updateErr != nil {
				slog.Warn("failed to update existing project members group",
					"project_id", project.ID, "slug", membersSlug, "error", updateErr.Error())
			}
		}
	} else {
		slog.Info("created project members group",
			"project_id", project.ID, "group", membersGroup.ID, "slug", membersSlug)
	}

	// Add the creating user as an owner of the project members group
	if project.CreatedBy != "" {
		if err := s.store.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    membersGroup.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   project.CreatedBy,
			Role:       store.GroupMemberRoleOwner,
		}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			slog.Warn("failed to add creator as owner of project members group",
				"project_id", project.ID, "user", project.CreatedBy, "error", err.Error())
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
			slog.Warn("failed to add caller as owner of project members group",
				"project_id", project.ID, "user", callerUserID[0], "error", err.Error())
		}
	}

	// Backfill: if the group has exactly one member and no owners, promote
	// that member to owner. This handles projects created before ownership
	// enforcement was added, where the creator was added as "member".
	ownerCount, err := s.store.CountGroupMembersByRole(ctx, membersGroup.ID, store.GroupMemberRoleOwner)
	if err == nil && ownerCount == 0 {
		members, err := s.store.GetGroupMembers(ctx, membersGroup.ID)
		if err == nil && len(members) == 1 && members[0].MemberType == store.GroupMemberTypeUser {
			if promoteErr := s.store.UpdateGroupMemberRole(ctx, membersGroup.ID,
				members[0].MemberType, members[0].MemberID, store.GroupMemberRoleOwner); promoteErr != nil {
				slog.Warn("failed to promote sole member to owner",
					"project_id", project.ID, "group", membersGroup.ID, "user", members[0].MemberID, "error", promoteErr.Error())
			} else {
				slog.Info("promoted sole project member to owner",
					"project_id", project.ID, "group", membersGroup.ID, "user", members[0].MemberID)
			}
		}
	}

	// Create project-level policy for member agent creation and stop-all
	policyName := "project:" + project.Slug + ":member-create-agents"
	policy := &store.Policy{
		ID:           api.NewUUID(),
		Name:         policyName,
		Description:  "Allow project members to create and stop agents",
		ScopeType:    "project",
		ScopeID:      project.ID,
		ResourceType: "agent",
		Actions:      []string{"create", "stop_all"},
		Effect:       "allow",
	}
	if err := s.store.CreatePolicy(ctx, policy); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			slog.Warn("failed to create project member policy",
				"project_id", project.ID, "policy", policyName, "error", err.Error())
			return
		}
		// Policy already exists — look it up and update its scope ID in case the
		// project was recreated. Also ensure the binding to the current members group.
		existing, lookupErr := s.store.ListPolicies(ctx, store.PolicyFilter{Name: policyName}, store.ListOptions{Limit: 1})
		if lookupErr != nil || len(existing.Items) == 0 {
			slog.Warn("failed to look up existing project member policy",
				"project_id", project.ID, "policy", policyName, "error", lookupErr)
			return
		}
		policy = &existing.Items[0]
		needsUpdate := false
		if policy.ScopeID != project.ID {
			policy.ScopeID = project.ID
			needsUpdate = true
		}
		// Backfill: ensure stop_all action is present for existing projects
		hasStopAll := false
		for _, a := range policy.Actions {
			if a == "stop_all" {
				hasStopAll = true
				break
			}
		}
		if !hasStopAll {
			policy.Actions = append(policy.Actions, "stop_all")
			needsUpdate = true
		}
		if needsUpdate {
			if updateErr := s.store.UpdatePolicy(ctx, policy); updateErr != nil {
				slog.Warn("failed to update existing project member policy",
					"project_id", project.ID, "policy", policyName, "error", updateErr.Error())
			}
		}
	}

	// Bind policy to the members group
	if err := s.store.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "group",
		PrincipalID:   membersGroup.ID,
	}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Warn("failed to bind project member policy",
			"project_id", project.ID, "policy", policyName, "error", err.Error())
	}
}

// hubManagedProjectPath returns the filesystem path for a hub-managed project workspace.
// It prefers projects/<slug> and falls back to groves/<slug> for backward compatibility
// with workspaces created before the grove-to-project rename.
func hubManagedProjectPath(slug string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("project slug must not be empty")
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
	// Neither has content — return projects path (will be created on demand)
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
	workspacePath, err := hubManagedProjectPath(project.Slug)
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
			slog.Warn("failed to update hub-managed project setting",
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
	workspacePath, err := hubManagedProjectPath(project.Slug)
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
		slog.Warn("failed to initialize .scion in cloned workspace",
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
			slog.Warn("failed to update shared-workspace project setting",
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
		slog.Warn("failed to list GitHub App installations for auto-association",
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
					slog.Warn("failed to persist GitHub App installation association",
						"project_id", project.ID, "installation_id", installID, "error", err)
				} else {
					slog.Info("auto-associated project with GitHub App installation at creation time",
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
			slog.Warn("failed to mint GitHub App token for clone, trying secrets",
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
				slog.Info("using creator's GitHub token for project clone",
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

	storagePath := storage.ProjectWorkspaceStoragePath(project.ID)

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
	workspacePath, err := hubManagedProjectPath(project.Slug)
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
			ID:         projectID,
			Name:       displayName,
			Slug:       slug,
			GitRemote:  normalizedRemote,
			Labels:     req.Labels,
			Visibility: store.VisibilityPrivate,
		}

		// Set ownership from authenticated user
		if user := GetUserIdentityFromContext(ctx); user != nil {
			project.CreatedBy = user.ID()
			project.OwnerID = user.ID()
		}

		if err := s.store.CreateProject(ctx, project); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		created = true

		// Create the associated project_agents group (best-effort)
		s.createProjectGroup(ctx, project)

		// Create project members group and policy (best-effort)
		s.createProjectMembersGroupAndPolicy(ctx, project)

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
		slog.Debug("ensuring groups for existing project during register",
			"project_id", project.ID, "slug", project.Slug, "caller", callerID)
		s.createProjectGroup(ctx, project)
		s.createProjectMembersGroupAndPolicy(ctx, project, callerID)
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

	// Check for nested /metrics path (project-scoped metrics dashboard)
	if subPath == "metrics" || strings.HasPrefix(subPath, "metrics/") {
		metricsPath := strings.TrimPrefix(subPath, "metrics")
		metricsPath = strings.TrimPrefix(metricsPath, "/")
		s.handleProjectMetricsDashboard(w, r, projectID, metricsPath)
		return
	}

	// Check for nested /settings path
	if subPath == "settings" {
		s.handleProjectSettings(w, r, projectID)
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
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.AgentFilter{
		ProjectID:       projectID,
		RuntimeBrokerID: query.Get("runtimeBrokerId"),
		Phase:           query.Get("phase"),
		IncludeDeleted:  query.Get("includeDeleted") == "true",
	}

	limit := 50
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

	// Resolve caller identity for creator tracking
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

	// For interactive actions, enforce policy-based authorization (owner or admin only)
	switch action {
	case api.AgentActionStart, api.AgentActionStop, api.AgentActionSuspend, api.AgentActionRestart, api.AgentActionMessage, api.AgentActionExec:
		if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionAttach)
			if !decision.Allowed {
				slog.Warn("agent authz check failed",
					"agent_id", agent.ID,
					"agent_slug", agent.Slug,
					"agent_owner_id", agent.OwnerID,
					"agent_created_by", agent.CreatedBy,
					"user_id", userIdent.ID(),
					"user_email", userIdent.Email(),
					"user_role", userIdent.Role(),
					"action", action,
					"decision_reason", decision.Reason,
				)
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"Only the agent's creator can interact with it", nil)
				return
			}
		}
	}

	switch action {
	case api.AgentActionStatus:
		s.updateAgentStatus(w, r, agent.ID)
	case api.AgentActionStart, api.AgentActionStop, api.AgentActionSuspend, api.AgentActionRestart:
		s.handleAgentLifecycle(w, r, agent.ID, action)
	case api.AgentActionMessage:
		s.handleAgentMessage(w, r, agent.ID)
	case api.AgentActionExec:
		s.handleAgentExec(w, r, agent.ID)
	case api.AgentActionEnv:
		s.submitAgentEnv(w, r, projectID, agentID)
	case api.AgentActionRestore:
		s.restoreAgent(w, r, agent.ID)
	case api.AgentActionOutboundMessage:
		s.handleAgentOutboundMessage(w, r, agent.ID)
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
	ctx := r.Context()
	project, err := s.store.GetProject(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Ensure associated groups exist (backfill for projects created before
	// group support was added). These calls are idempotent.
	s.createProjectGroup(ctx, project)
	s.createProjectMembersGroupAndPolicy(ctx, project)

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

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, projectResource(project), ActionUpdate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You do not have permission to update this project", nil)
			return
		}
	}

	var updates struct {
		Name                   string            `json:"name,omitempty"`
		Slug                   string            `json:"slug,omitempty"`
		Labels                 map[string]string `json:"labels,omitempty"`
		Visibility             string            `json:"visibility,omitempty"`
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
	if updates.Visibility != "" {
		project.Visibility = updates.Visibility
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
			slog.Warn("failed to migrate project agents group slug",
				"project_id", project.ID, "old_slug", oldAgentsSlug, "new_slug", newAgentsSlug, "error", err)
		}
	} else if err != store.ErrNotFound {
		slog.Warn("failed to retrieve project agents group for migration",
			"project_id", project.ID, "old_slug", oldAgentsSlug, "error", err)
	}

	// Migrate the project members group slug.
	oldMembersSlug := "project:" + oldSlug + ":members"
	newMembersSlug := "project:" + newSlug + ":members"
	if group, err := s.store.GetGroupBySlug(ctx, oldMembersSlug); err == nil {
		group.Slug = newMembersSlug
		group.Name = project.Name + " Members"
		if err := s.store.UpdateGroup(ctx, group); err != nil {
			slog.Warn("failed to migrate project members group slug",
				"project_id", project.ID, "old_slug", oldMembersSlug, "new_slug", newMembersSlug, "error", err)
		}
	} else if err != store.ErrNotFound {
		slog.Warn("failed to retrieve project members group for migration",
			"project_id", project.ID, "old_slug", oldMembersSlug, "error", err)
	}

	// Migrate the project member policy name.
	oldPolicyName := "project:" + oldSlug + ":member-create-agents"
	newPolicyName := "project:" + newSlug + ":member-create-agents"
	if policies, err := s.store.ListPolicies(ctx, store.PolicyFilter{Name: oldPolicyName}, store.ListOptions{Limit: 1}); err == nil && len(policies.Items) > 0 {
		policy := &policies.Items[0]
		policy.Name = newPolicyName
		if err := s.store.UpdatePolicy(ctx, policy); err != nil {
			slog.Warn("failed to migrate project member policy name",
				"project_id", project.ID, "old_policy", oldPolicyName, "new_policy", newPolicyName, "error", err)
		}
	} else if err != nil {
		slog.Warn("failed to retrieve project member policy for migration",
			"project_id", project.ID, "old_policy", oldPolicyName, "error", err)
	}

	// Migrate hub-managed project filesystem paths (best-effort).
	// Derive newPath from oldPath's parent to preserve the directory type (groves/ vs projects/).
	if oldPath, err := hubManagedProjectPath(oldSlug); err == nil {
		if _, statErr := os.Stat(oldPath); statErr == nil {
			newPath := filepath.Join(filepath.Dir(oldPath), newSlug)
			if _, statErr := os.Stat(newPath); os.IsNotExist(statErr) {
				if err := os.Rename(oldPath, newPath); err != nil {
					slog.Warn("failed to rename project workspace directory",
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
							slog.Warn("failed to rename project config directory",
								"project_id", project.ID, "old_path", oldConfigDir, "new_path", newConfigDir, "error", err)
						}
					}
				}
			}
		}
	}
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Fetch the project record before deletion so we can clean up the filesystem.
	project, err := s.store.GetProject(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, projectResource(project), ActionDelete)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You do not have permission to delete this project", nil)
			return
		}
	}

	// Dispatch agent deletions to runtime brokers so containers are stopped
	// and agent files are cleaned up. The DB cascade will remove agent records,
	// but we need the broker to tear down the actual resources first.
	s.deleteProjectAgents(ctx, project)

	// Clean up all groups associated with the project (agents group, members group, etc.)
	if projectGroups, err := s.store.ListGroups(ctx, store.GroupFilter{ProjectID: id}, store.ListOptions{Limit: 100}); err == nil {
		for _, g := range projectGroups.Items {
			if delErr := s.store.DeleteGroup(ctx, g.ID); delErr != nil {
				slog.Warn("failed to delete project group", "project_id", id, "group", g.ID, "slug", g.Slug, "error", delErr.Error())
			}
		}
	}

	// Clean up project-scoped policies (best-effort)
	if projectPolicies, err := s.store.ListPolicies(ctx, store.PolicyFilter{ScopeType: "project", ScopeID: id}, store.ListOptions{Limit: 100}); err == nil {
		for _, p := range projectPolicies.Items {
			if delErr := s.store.DeletePolicy(ctx, p.ID); delErr != nil {
				slog.Warn("failed to delete project policy", "project_id", id, "policy", p.ID, "name", p.Name, "error", delErr.Error())
			}
		}
	}

	// Clean up project-scoped env vars (best-effort).
	// These use scope/scope_id without FK cascade.
	if n, err := s.store.DeleteEnvVarsByScope(ctx, store.ScopeProject, id); err != nil {
		slog.Warn("failed to delete project env vars", "project_id", id, "error", err)
	} else if n > 0 {
		slog.Info("deleted project env vars", "project_id", id, "count", n)
	}

	// Clean up project-scoped secrets (best-effort).
	if n, err := s.store.DeleteSecretsByScope(ctx, store.ScopeProject, id); err != nil {
		slog.Warn("failed to delete project secrets", "project_id", id, "error", err)
	} else if n > 0 {
		slog.Info("deleted project secrets", "project_id", id, "count", n)
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
				slog.Warn("failed to delete project GCP service account registration",
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

	// For hub-native and shared-workspace projects, remove the filesystem directory.
	if (project.GitRemote == "" || project.IsSharedWorkspace()) && project.Slug != "" {
		if projectPath, err := hubManagedProjectPath(project.Slug); err == nil {
			if err := util.RemoveAllSafe(projectPath); err != nil {
				slog.Warn("failed to remove hub-managed project directory",
					"project_id", id, "slug", project.Slug, "path", projectPath, "error", err)
			}
		}
	}

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
				slog.Warn("failed to remove project config directory",
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
		slog.Warn("failed to list project templates for deletion", "project_id", projectID, "error", err)
	} else if stor := s.GetStorage(); stor != nil {
		for _, tmpl := range templates.Items {
			if tmpl.StoragePath != "" {
				if err := stor.DeletePrefix(ctx, tmpl.StoragePath); err != nil {
					slog.Warn("failed to delete template storage files",
						"project_id", projectID, "template", tmpl.ID, "path", tmpl.StoragePath, "error", err)
				}
			}
		}
	}

	if n, err := s.store.DeleteTemplatesByScope(ctx, store.ScopeProject, projectID); err != nil {
		slog.Warn("failed to delete project templates", "project_id", projectID, "error", err)
	} else if n > 0 {
		slog.Info("deleted project templates", "project_id", projectID, "count", n)
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
		slog.Warn("failed to list managed GCP SAs for project deletion warning",
			"project_id", projectID, "error", err)
		return
	}
	for _, sa := range sas {
		slog.Warn("project deletion: managed GCP service account retained in GCP — manual cleanup may be required",
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
		slog.Warn("failed to list project harness configs for deletion", "project_id", projectID, "error", err)
	} else if stor := s.GetStorage(); stor != nil {
		for _, hc := range configs.Items {
			if hc.StoragePath != "" {
				if err := stor.DeletePrefix(ctx, hc.StoragePath); err != nil {
					slog.Warn("failed to delete harness config storage files",
						"project_id", projectID, "harnessConfig", hc.ID, "path", hc.StoragePath, "error", err)
				}
			}
		}
	}

	if n, err := s.store.DeleteHarnessConfigsByScope(ctx, store.ScopeProject, projectID); err != nil {
		slog.Warn("failed to delete project harness configs", "project_id", projectID, "error", err)
	} else if n > 0 {
		slog.Info("deleted project harness configs", "project_id", projectID, "count", n)
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
		slog.Warn("failed to get project providers for cleanup", "project_id", project.ID, "error", err)
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
		slog.Warn("no RuntimeBrokerClient available for project cleanup dispatch", "project_id", project.ID)
		return
	}

	for _, provider := range providers {
		// Skip the embedded broker — the hub already cleans up its own copy.
		if s.isEmbeddedBroker(provider.BrokerID) {
			continue
		}

		broker, err := s.store.GetRuntimeBroker(ctx, provider.BrokerID)
		if err != nil {
			slog.Warn("failed to get broker for project cleanup",
				"project_id", project.ID, "broker", provider.BrokerID, "error", err)
			continue
		}

		if err := client.CleanupProject(ctx, provider.BrokerID, broker.Endpoint, project.Slug, project.ID); err != nil {
			slog.Warn("failed to cleanup project on broker",
				"project_id", project.ID, "slug", project.Slug,
				"broker", provider.BrokerID, "endpoint", broker.Endpoint, "error", err)
		}
	}
}
