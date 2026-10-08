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
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// CloneProjectRequest is the request body for POST /api/v1/projects/{id}/clone.
type CloneProjectRequest struct {
	Name       string `json:"name"`                 // required
	Slug       string `json:"slug"`                 // optional explicit slug override
	AsTemplate bool   `json:"asTemplate,omitempty"` // mark clone as template
	GitRemote  string `json:"gitRemote,omitempty"`  // override source git remote
}

// handleProjectClone clones a project's configuration into a new project.
//
// POST /api/v1/projects/{id}/clone
//
// The clone copies settings, labels, env vars, injected skills, pre-start hook,
// and project-scoped harness configs and templates. It does NOT copy secrets,
// agents, history, or chat integrations. See .design/project-templates.md §5.3.
//
// On failure at any step, the defer-driven rollback stack ensures no orphaned
// resources remain. Every rollback closure uses context.WithoutCancel so that
// a client disconnect mid-clone does not abort cleanup.
func (s *Server) handleProjectClone(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	// ── Step 1: Load source project ──────────────────────────────────────

	src, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// ── Step 1b: Authorize ───────────────────────────────────────────────

	if err := s.authorizeProjectClone(ctx, w, src); err != nil {
		return // authorizeProjectClone writes the HTTP response
	}

	// ── Step 1c: Parse and validate request ──────────────────────────────

	var req CloneProjectRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	// A gitRemote override must be a remote git URL. Anything else (a local
	// path, a bare host) would be stored as GitRemote and turned into a bogus
	// clone-url label by ToHTTPSCloneURL. The query string and fragment are
	// dropped first: git remotes never need them and they can carry tokens
	// (?access_token=…).
	overrideRemote, cutOK := util.CutQueryAndFragment(trimRemote(req.GitRemote))
	if !cutOK {
		// A '?' or '#' inside the userinfo: cutting there would keep part of
		// the password.
		ValidationError(w, errCloneRemoteInvalid, map[string]interface{}{"field": "gitRemote"})
		return
	}
	if overrideRemote != "" {
		if msg := validateCloneGitRemote(overrideRemote); msg != "" {
			ValidationError(w, msg, map[string]interface{}{"field": "gitRemote"})
			return
		}
	}
	// Never persist credentials embedded in the override (https://user:TOKEN@…):
	// GitRemote and the git source labels are readable by project members.
	overrideRemote = dropDefaultPort(util.StripGitURLCredentials(overrideRemote))
	// overrideCanonical is the form fed to NormalizeGitRemote/ToHTTPSCloneURL,
	// which only understand the "git@" SCP login (see canonicalCloneRemote).
	overrideCanonical := canonicalCloneRemote(overrideRemote)

	// ── Step 2: Resolve name/slug ────────────────────────────────────────

	baseSlug := req.Slug
	explicitSlug := baseSlug != ""
	if !explicitSlug {
		baseSlug = api.Slugify(req.Name)
	} else if isReservedProjectSlug(baseSlug) {
		ValidationError(w, reservedProjectSlugMessage, map[string]interface{}{"field": "slug"})
		return
	} else if !requireProjectSlugFormat(w, baseSlug) {
		return
	}

	slug, err := s.nextAvailableUnreservedSlug(ctx, baseSlug)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Explicit slug that collides → 409 (never auto-disambiguate).
	if explicitSlug && slug != baseSlug {
		Conflict(w, "A project with slug \""+baseSlug+"\" already exists")
		return
	}

	displayName := req.Name
	if slug != baseSlug {
		displayName = api.DisplayNameWithSerial(req.Name, slug, baseSlug)
	}

	// ── Step 3: Build clone Project struct ────────────────────────────────

	callerID := ""
	if user := GetUserIdentityFromContext(ctx); user != nil {
		callerID = user.ID()
	}

	clone := &store.Project{
		ID:                     api.NewUUID(),
		Name:                   displayName,
		Slug:                   slug,
		GitRemote:              src.GitRemote,
		DefaultRuntimeBrokerID: src.DefaultRuntimeBrokerID,
		CreatedBy:              callerID,
		OwnerID:                callerID,
		SharedDirs:             src.SharedDirs,
		GitIdentity:            src.GitIdentity,
	}

	// Allow callers to override the git remote (e.g. creating from a template
	// with a different repository).
	remoteOverridden := false
	if overrideRemote != "" {
		clone.GitRemote = util.NormalizeGitRemote(overrideCanonical)
		remoteOverridden = clone.GitRemote != util.NormalizeGitRemote(src.GitRemote)
	}

	// Copy annotations: only keys in projectSettingKeys, preserving null semantics
	// (§6.4: unset stays unset).
	if src.Annotations != nil {
		clone.Annotations = make(map[string]string)
		for _, key := range projectSettingKeys {
			if v, ok := src.Annotations[key]; ok {
				clone.Annotations[key] = v
			}
		}
		if len(clone.Annotations) == 0 {
			clone.Annotations = nil
		}
	}

	// Skip system labels (scion.io/* prefix). Project settings annotations
	// live in project.Annotations, not Labels, and are copied separately
	// via projectSettingKeys above. scion.dev/* labels are copied.
	// workspace-mode is re-derived below, not copied raw.
	if src.Labels != nil {
		clone.Labels = make(map[string]string)
		for k, v := range src.Labels {
			if strings.HasPrefix(k, "scion.io/") {
				continue // system markers — not propagated
			}
			if k == store.LabelWorkspaceMode {
				continue // re-derived, not copied raw
			}
			if remoteOverridden && isGitSourceLabel(k) {
				continue // describe the template's repo; re-derived below
			}
			clone.Labels[k] = v
		}
		// Labels copied from the source may predate write-time validation.
		sanitizeSourceURLLabel(clone.Labels)
		sanitizeCopiedCloneURLLabel(clone.Labels)
		if len(clone.Labels) == 0 {
			clone.Labels = nil
		}
	}

	// When the git remote is overridden, the template's clone-url/source-url/
	// default-branch labels describe the wrong repository. clone-url takes
	// precedence over GitRemote at agent create and shared-workspace init
	// (resolveCloneURL), so leaving it would silently clone the template's
	// repo. Re-derive them from the override the way the web create form does.
	if remoteOverridden {
		if clone.Labels == nil {
			clone.Labels = make(map[string]string)
		}
		clone.Labels[store.LabelCloneURL] = util.ToHTTPSCloneURL(overrideCanonical)
		// Defence in depth: overrideRemote is already query-stripped, validated and credential-stripped above.
		if src := util.SanitizeGitSourceURL(overrideRemote); src != "" {
			clone.Labels[store.LabelSourceURL] = src
		}
		clone.Labels[store.LabelDefaultBranch] = "main"
	}

	// Re-derive workspace mode from the source (design #2703 §2.4).
	if mode := deriveCloneWorkspaceMode(src.Labels[store.LabelWorkspaceMode], src.GitRemote != "", clone.GitRemote != ""); mode != "" {
		if clone.Labels == nil {
			clone.Labels = make(map[string]string)
		}
		clone.Labels[store.LabelWorkspaceMode] = mode
	}

	// ── asTemplate: mark clone as a project template ─────────────────────
	if req.AsTemplate {
		// Creating templates requires project.clone permission.
		user := GetUserIdentityFromContext(ctx)
		if user == nil {
			Unauthorized(w)
			return
		}
		if !s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(user),
			Credential: credentialContextForIdentity(user),
			Resource:   Resource{Type: "project", ID: "hub"},
			Action:     Action("clone"),
			Permission: "project.clone",
		}).Allowed {
			Forbidden(w)
			return
		}
		if clone.Labels == nil {
			clone.Labels = make(map[string]string)
		}
		clone.Labels[store.LabelTemplate] = "true"
	}

	// ── Rollback stack ───────────────────────────────────────────────────

	var rollback []func()
	committed := false
	defer func() {
		if !committed {
			for i := len(rollback) - 1; i >= 0; i-- {
				rollback[i]()
			}
		}
	}()

	// ── Step 4: Create project row ───────────────────────────────────────

	if err := s.store.CreateProject(ctx, clone); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	rollback = append(rollback, func() {
		rbCtx := context.WithoutCancel(ctx)
		// Cascade-delete role bindings before the project row (PM1/XL-R1).
		if _, rbErr := s.store.DeleteRoleBindingsForScope(rbCtx, store.RoleScopeProject, clone.ID); rbErr != nil {
			slog.Warn("project clone rollback: failed to delete role bindings",
				"clone_id", clone.ID, "error", rbErr)
		}
		if delErr := s.store.DeleteProject(rbCtx, clone.ID); delErr != nil {
			slog.Warn("project clone rollback: failed to delete project",
				"clone_id", clone.ID, "error", delErr)
		}
	})

	// ── Step 5: Create owner role binding (PM1: atomic with project) ─────
	// The clone creator becomes the project owner via a direct role binding.
	// This is the canonical project membership source. The step 4 rollback
	// already cascade-deletes all project-scoped bindings.
	if callerID != "" {
		if rbErr := s.createProjectOwnerRoleBinding(ctx, clone.ID, callerID); rbErr != nil {
			slog.Error("project clone: failed to create owner role binding",
				"clone_id", clone.ID, "user_id", callerID, "error", rbErr)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to create owner role binding: "+rbErr.Error(), nil)
			return
		}
	}

	// ── Step 6: Create groups ────────────────────────────────────────────

	s.createProjectGroup(ctx, clone)

	// ── Step 7: Create members group (collaboration) ─────────────────────
	// The group exists for collaboration; authorization is via RoleBindings.
	// clone.CreatedBy is already callerID (set at construction), so the
	// creator-membership grant inside createProjectMembersGroup covers the
	// caller here; no separate caller argument is needed.
	s.createProjectMembersGroup(ctx, clone)

	// ── Step 8: Deep-copy project-scoped harness configs ─────────────────

	if err := s.cloneProjectHarnessConfigs(ctx, src.ID, clone, &rollback); err != nil {
		slog.Error("project clone: harness config copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy harness configs: "+err.Error(), nil)
		return
	}

	// ── Step 9: Deep-copy project-scoped templates ───────────────────────

	if err := s.cloneProjectTemplates(ctx, src.ID, clone, &rollback); err != nil {
		slog.Error("project clone: template copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy templates: "+err.Error(), nil)
		return
	}

	// ── Step 10: Copy non-secret env vars ────────────────────────────────

	if err := s.cloneProjectEnvVars(ctx, src.ID, clone.ID, callerID, &rollback); err != nil {
		slog.Error("project clone: env var copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy environment variables: "+err.Error(), nil)
		return
	}

	// ── Step 10b: Copy GCP service account associations ─────────────────

	if err := s.cloneProjectGCPServiceAccounts(ctx, src.ID, clone, callerID, &rollback); err != nil {
		slog.Error("project clone: GCP service account copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy GCP service accounts: "+err.Error(), nil)
		return
	}

	// ── Step 11: Copy injected skills ────────────────────────────────────

	if err := s.cloneProjectSkillInjections(ctx, src.ID, clone.ID, callerID, &rollback); err != nil {
		slog.Error("project clone: skill injection copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy injected skills: "+err.Error(), nil)
		return
	}

	// ── Step 12: Copy active pre-start hook ──────────────────────────────

	if err := s.cloneProjectPreStartHook(ctx, src.ID, clone.ID, callerID); err != nil {
		slog.Error("project clone: pre-start hook copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy pre-start hook: "+err.Error(), nil)
		return
	}
	// No explicit rollback for step 12 — hook is cleaned up transitively by
	// step 4's DeleteProject. See design doc §5.5.

	// ── Step 13: Auto-associate GitHub installation (best-effort) ────────

	if clone.GitRemote != "" && clone.GitHubInstallationID == nil {
		s.autoAssociateGitHubInstallation(ctx, clone)
	}

	// ── Step 14: Workspace init ──────────────────────────────────────────

	if clone.IsSharedWorkspace() {
		if err := s.cloneSharedWorkspaceProject(ctx, clone); err != nil {
			slog.Error("project clone: shared workspace clone failed",
				"clone_id", clone.ID, "error", err)
			if writeWorkspaceStorageUnavailable(w, err) {
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to initialize workspace: "+err.Error(), nil)
			return
		}
	} else if clone.GitRemote == "" {
		if err := s.initHubManagedProject(clone); err != nil {
			// Workspace storage did not respond: return before committed is
			// set, so the deferred rollback stack removes the clone, and
			// answer 503. Other failures stay best-effort, as before.
			if writeWorkspaceStorageUnavailable(w, err) {
				slog.Error("project clone: workspace storage did not respond, rolling back",
					"clone_id", clone.ID, "error", err)
				return
			}
			slog.Warn("project clone: failed to initialize hub-managed workspace",
				"clone_id", clone.ID, "error", err)
		}
	}

	// ── Step 14: Auto-link providers (best-effort) ───────────────────────

	s.autoLinkProviders(ctx, clone)

	// ── Step 15: Ensure the #general chat topic (best-effort) ────────────
	// Runs after every step that can fail and roll the clone back, so a
	// rolled-back clone never leaves an orphaned topic behind. Skipped for
	// template clones (asTemplate), which are not chat spaces.

	s.ensureProjectGeneralTopic(ctx, clone)

	// ── Step 16: Publish event (best-effort) ─────────────────────────────

	s.events.PublishProjectCreated(ctx, clone)

	// ── Commit ───────────────────────────────────────────────────────────

	committed = true

	writeJSON(w, http.StatusCreated, clone)
}

// authorizeProjectClone checks that the caller can read the source project and
// create projects at hub scope. It writes the HTTP error response on failure and
// returns a non-nil error; on success it returns nil.
func (s *Server) authorizeProjectClone(ctx context.Context, w http.ResponseWriter, src *store.Project) error {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return errors.New("unauthenticated")
	}

	userIdent, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return errors.New("non-user identity")
	}

	// 1. Caller must be able to read the source project.
	decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
		Type:    "project",
		ID:      src.ID,
		OwnerID: src.OwnerID,
	}, ActionRead)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Insufficient permission to read source project", nil)
		return errors.New("forbidden: cannot read source")
	}

	// 2. Caller must be able to create projects at hub scope.
	caps := s.authzService.ComputeScopeCapabilities(ctx, userIdent, "", "", "project")
	if !slices.Contains(caps.Actions, string(ActionCreate)) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Insufficient permission to create projects", nil)
		return errors.New("forbidden: cannot create projects")
	}

	return nil
}

// cloneProjectHarnessConfigs deep-copies all project-scoped harness configs from
// the source project to the clone. Each config gets a fresh UUID but keeps the
// same slug — the slug is scoped to the project, and the clone's
// default-harness-config annotation refers to it by slug.
func (s *Server) cloneProjectHarnessConfigs(ctx context.Context, srcProjectID string, clone *store.Project, rollback *[]func()) error {
	result, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: srcProjectID,
	}, store.ListOptions{Limit: 500})
	if err != nil {
		return err
	}

	if len(result.Items) == 0 {
		return nil
	}

	stor := s.GetStorage()

	for _, srcHC := range result.Items {
		newHC := &store.HarnessConfig{
			ID:          api.NewUUID(),
			Name:        srcHC.Name,
			Slug:        srcHC.Slug, // SAME slug — critical for annotation references
			DisplayName: srcHC.DisplayName,
			Description: srcHC.Description,
			Harness:     srcHC.Harness,
			Config:      srcHC.Config,
			Scope:       store.HarnessConfigScopeProject,
			ScopeID:     clone.ID,
			Status:      srcHC.Status,
			Files:       srcHC.Files,
			ContentHash: srcHC.ContentHash,
		}

		storagePath := storage.HarnessConfigStoragePath(s.HubID(), newHC.Scope, newHC.ScopeID, newHC.Slug)
		newHC.StoragePath = storagePath

		if stor != nil {
			newHC.StorageBucket = stor.Bucket()
			newHC.StorageURI = storage.HarnessConfigStorageURI(s.HubID(), stor.Bucket(), newHC.Scope, newHC.ScopeID, newHC.Slug)
		}

		// Copy storage files
		if stor != nil && len(srcHC.Files) > 0 && srcHC.StoragePath != "" {
			for _, file := range srcHC.Files {
				srcPath := srcHC.StoragePath + "/" + file.Path
				dstPath := storagePath + "/" + file.Path
				if _, err := stor.Copy(ctx, srcPath, dstPath); err != nil {
					_ = stor.DeletePrefix(ctx, storage.DirPrefix(storagePath))
					return err
				}
			}
		}

		if err := s.store.CreateHarnessConfig(ctx, newHC); err != nil {
			if stor != nil {
				_ = stor.DeletePrefix(ctx, storage.DirPrefix(storagePath))
			}
			return err
		}
	}

	// Add rollback for all harness configs at once
	*rollback = append(*rollback, func() {
		rbCtx := context.WithoutCancel(ctx)
		stor := s.GetStorage()
		if stor != nil {
			prefix := storage.HarnessConfigStoragePath(s.HubID(), store.HarnessConfigScopeProject, clone.ID, "")
			_ = stor.DeletePrefix(rbCtx, storage.DirPrefix(prefix))
		}
		if _, err := s.store.DeleteHarnessConfigsByScope(rbCtx, store.HarnessConfigScopeProject, clone.ID); err != nil {
			slog.Warn("project clone rollback: failed to delete harness configs",
				"clone_id", clone.ID, "error", err)
		}
	})

	return nil
}

// cloneProjectTemplates deep-copies all project-scoped templates from the source
// project to the clone. Identical pattern to cloneProjectHarnessConfigs.
func (s *Server) cloneProjectTemplates(ctx context.Context, srcProjectID string, clone *store.Project, rollback *[]func()) error {
	result, err := s.store.ListTemplates(ctx, store.TemplateFilter{
		Scope:   store.TemplateScopeProject,
		ScopeID: srcProjectID,
	}, store.ListOptions{Limit: 500})
	if err != nil {
		return err
	}

	if len(result.Items) == 0 {
		return nil
	}

	stor := s.GetStorage()

	for _, srcTmpl := range result.Items {
		newTmpl := &store.Template{
			ID:           api.NewUUID(),
			Name:         srcTmpl.Name,
			Slug:         srcTmpl.Slug, // SAME slug — critical for annotation references
			DisplayName:  srcTmpl.DisplayName,
			Description:  srcTmpl.Description,
			Harness:      srcTmpl.Harness,
			Config:       srcTmpl.Config,
			Scope:        store.TemplateScopeProject,
			ScopeID:      clone.ID,
			Status:       srcTmpl.Status,
			Files:        srcTmpl.Files,
			ContentHash:  srcTmpl.ContentHash,
			BaseTemplate: srcTmpl.BaseTemplate,
		}

		storagePath := storage.TemplateStoragePath(s.HubID(), newTmpl.Scope, newTmpl.ScopeID, newTmpl.Slug)
		newTmpl.StoragePath = storagePath

		if stor != nil {
			newTmpl.StorageBucket = stor.Bucket()
			newTmpl.StorageURI = storage.TemplateStorageURI(s.HubID(), stor.Bucket(), newTmpl.Scope, newTmpl.ScopeID, newTmpl.Slug)
		}

		// Copy storage files
		if stor != nil && len(srcTmpl.Files) > 0 && srcTmpl.StoragePath != "" {
			for _, file := range srcTmpl.Files {
				srcPath := srcTmpl.StoragePath + "/" + file.Path
				dstPath := storagePath + "/" + file.Path
				if _, err := stor.Copy(ctx, srcPath, dstPath); err != nil {
					_ = stor.DeletePrefix(ctx, storage.DirPrefix(storagePath))
					return err
				}
			}
		}

		if err := s.store.CreateTemplate(ctx, newTmpl); err != nil {
			if stor != nil {
				_ = stor.DeletePrefix(ctx, storage.DirPrefix(storagePath))
			}
			return err
		}
	}

	// Add rollback for all templates at once
	*rollback = append(*rollback, func() {
		rbCtx := context.WithoutCancel(ctx)
		stor := s.GetStorage()
		if stor != nil {
			prefix := storage.TemplateStoragePath(s.HubID(), store.TemplateScopeProject, clone.ID, "")
			_ = stor.DeletePrefix(rbCtx, storage.DirPrefix(prefix))
		}
		if _, err := s.store.DeleteTemplatesByScope(rbCtx, store.TemplateScopeProject, clone.ID); err != nil {
			slog.Warn("project clone rollback: failed to delete templates",
				"clone_id", clone.ID, "error", err)
		}
	})

	return nil
}

// cloneProjectEnvVars copies non-secret environment variables from the source
// project to the clone. Secret env vars (Secret == true) are excluded.
// Sensitive env vars (Sensitive == true, not secrets) ARE copied.
func (s *Server) cloneProjectEnvVars(ctx context.Context, srcProjectID, cloneProjectID, callerID string, rollback *[]func()) error {
	envVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{
		Scope:   store.ScopeProject,
		ScopeID: srcProjectID,
	})
	if err != nil {
		return err
	}

	copied := false
	for _, ev := range envVars {
		// Skip secret-backed env vars
		if ev.Secret {
			continue
		}

		newEV := &store.EnvVar{
			ID:            api.NewUUID(),
			Key:           ev.Key,
			Value:         ev.Value,
			Scope:         store.ScopeProject,
			ScopeID:       cloneProjectID,
			Description:   ev.Description,
			Sensitive:     ev.Sensitive, // Sensitive rows ARE copied
			InjectionMode: ev.InjectionMode,
			CreatedBy:     callerID,
		}

		if err := s.store.CreateEnvVar(ctx, newEV); err != nil {
			return err
		}
		copied = true
	}

	if copied {
		*rollback = append(*rollback, func() {
			rbCtx := context.WithoutCancel(ctx)
			if _, err := s.store.DeleteEnvVarsByScope(rbCtx, store.ScopeProject, cloneProjectID); err != nil {
				slog.Warn("project clone rollback: failed to delete env vars",
					"clone_id", cloneProjectID, "error", err)
			}
		})
	}

	return nil
}

// cloneProjectGCPServiceAccounts copies project-scoped GCP service account
// associations from the source project to the clone, preserving each SA's
// verified state. If the clone project's default-SA annotation references a
// source SA, it is remapped to the corresponding cloned SA and persisted.
func (s *Server) cloneProjectGCPServiceAccounts(ctx context.Context, srcProjectID string, clone *store.Project, callerID string, rollback *[]func()) error {
	accounts, err := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: srcProjectID,
	})
	if err != nil {
		return err
	}

	if len(accounts) == 0 {
		return nil
	}

	var clonedIDs []string

	// Register rollback BEFORE the loop so partial creates are cleaned up
	// if CreateGCPServiceAccount fails mid-loop.
	*rollback = append(*rollback, func() {
		rbCtx := context.WithoutCancel(ctx)
		for _, id := range clonedIDs {
			if err := s.store.DeleteGCPServiceAccount(rbCtx, id); err != nil {
				slog.Warn("project clone rollback: failed to delete GCP service account",
					"clone_id", clone.ID, "sa_id", id, "error", err)
			}
		}
	})

	for _, sa := range accounts {
		newSA := &store.GCPServiceAccount{
			ID:            api.NewUUID(),
			Scope:         store.ScopeProject,
			ScopeID:       clone.ID,
			Email:         sa.Email,
			ProjectID:     sa.ProjectID,
			DisplayName:   sa.DisplayName,
			DefaultScopes: append([]string(nil), sa.DefaultScopes...),
			Verified:      sa.Verified,
			CreatedBy:     callerID,
			Managed:       sa.Managed,
			ManagedBy:     sa.ManagedBy,
		}

		if err := s.store.CreateGCPServiceAccount(ctx, newSA); err != nil {
			return err
		}
		clonedIDs = append(clonedIDs, newSA.ID)
	}

	// Remap the per-profile default SA map's entries that reference a
	// source SA. Entries naming other (hub-scoped) accounts are kept.
	if byProfile := profileDefaultSAIDsFromAnnotations(clone.Annotations); len(byProfile) > 0 {
		changed := false
		for profile, saID := range byProfile {
			for i, srcSA := range accounts {
				if srcSA.ID == saID {
					byProfile[profile] = clonedIDs[i]
					changed = true
					break
				}
			}
		}
		if changed {
			setProfileDefaultSAIDsAnnotation(clone.Annotations, byProfile)
			if err := s.store.UpdateProject(ctx, clone); err != nil {
				return fmt.Errorf("remap per-profile default SA annotation: %w", err)
			}
		}
	}

	// Remap default SA annotation if it references a source SA.
	defaultSAID, ok := clone.Annotations[projectSettingDefaultGCPIdentitySAID]
	if ok && defaultSAID != "" {
		for i, srcSA := range accounts {
			if srcSA.ID == defaultSAID {
				clone.Annotations[projectSettingDefaultGCPIdentitySAID] = clonedIDs[i]
				// Update the persisted project row.
				if err := s.store.UpdateProject(ctx, clone); err != nil {
					return fmt.Errorf("remap default SA annotation: %w", err)
				}
				break
			}
		}
	}

	return nil
}

// cloneProjectSkillInjections copies skill injections from the source project
// to the clone using a single atomic SetSkillInjections call.
func (s *Server) cloneProjectSkillInjections(ctx context.Context, srcProjectID, cloneProjectID, callerID string, rollback *[]func()) error {
	injections, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeProject, srcProjectID)
	if err != nil {
		return err
	}

	if len(injections) == 0 {
		return nil
	}

	// Build the list for the clone — SetSkillInjections generates fresh IDs
	cloneInjections := make([]store.SkillInjection, len(injections))
	for i, si := range injections {
		cloneInjections[i] = store.SkillInjection{
			SkillURI:  si.SkillURI,
			SkillAs:   si.SkillAs,
			Optional:  si.Optional,
			SortOrder: si.SortOrder,
		}
	}

	if err := s.store.SetSkillInjections(ctx, store.SkillInjectionScopeProject, cloneProjectID, cloneInjections, callerID); err != nil {
		return err
	}

	*rollback = append(*rollback, func() {
		rbCtx := context.WithoutCancel(ctx)
		if _, err := s.store.DeleteSkillInjectionsByScope(rbCtx, store.SkillInjectionScopeProject, cloneProjectID); err != nil {
			slog.Warn("project clone rollback: failed to delete skill injections",
				"clone_id", cloneProjectID, "error", err)
		}
	})

	return nil
}

// cloneProjectPreStartHook copies the active pre-start hook from the source
// project to the clone. Archived hooks are not copied.
func (s *Server) cloneProjectPreStartHook(ctx context.Context, srcProjectID, cloneProjectID, callerID string) error {
	srcHook, err := s.store.GetActiveProjectPreStartHook(ctx, srcProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // no active hook — nothing to copy
		}
		return err
	}

	newHook := &store.ProjectPreStartHook{
		ID:          api.NewUUID(),
		Scope:       store.PreStartHookScopeProject,
		ProjectID:   cloneProjectID,
		Name:        srcHook.Name,
		Slug:        api.Slugify(srcHook.Name),
		Description: srcHook.Description,
		Script:      srcHook.Script,
		Status:      store.ProjectPreStartHookStatusActive,
		CreatedBy:   callerID,
	}

	if _, err := s.store.CreateProjectPreStartHook(ctx, newHook); err != nil {
		return err
	}

	// Belt-and-braces: ensure it is active
	if _, err := s.store.ActivateProjectPreStartHook(ctx, newHook.ID, cloneProjectID); err != nil {
		slog.Warn("project clone: failed to activate pre-start hook (may already be active)",
			"clone_id", cloneProjectID, "hook_id", newHook.ID, "error", err)
	}

	return nil
}

// Error messages for a rejected clone gitRemote override.
const (
	errCloneRemoteInvalid = "gitRemote must be a remote git URL (https://, ssh://, git://, " +
		"user@host:org/repo or host[:port]/org/repo)"
	errCloneRemoteSSHPort = "gitRemote: ssh URLs with a port are not supported yet; use the https URL"
	// errCloneRemoteTLSPort is returned for git:// with any port and http://
	// with a port other than 80: ToHTTPSCloneURL keeps the port, so the
	// clone-url would speak TLS to a plain-text port.
	errCloneRemoteTLSPort = "gitRemote: git:// URLs with a port, http:// URLs with a port other than 80 and host:80/... remotes are not supported; use the https URL"
)

// trimRemote trims ASCII whitespace only. Unicode spaces such as U+0085 or
// U+FEFF are left in place, so the printable-ASCII check rejects them
// (the web create form trims the same set).
func trimRemote(remote string) string {
	return strings.Trim(remote, " \t\n\v\f\r")
}

// validRemotePath reports whether a decoded remote path (without the leading
// '/' of a scheme URL) has only non-empty segments that are neither "." nor
// "..", and no '@' (ambiguous with userinfo), '\\' or control characters. A single trailing '/' is allowed. Dot
// segments would make GitRemote name a different repository than the one
// git clones (libcurl removes them); empty segments ("org//repo") are junk.
func validRemotePath(path string) bool {
	for _, seg := range strings.Split(strings.TrimSuffix(path, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "@\\") {
			return false
		}
		for i := 0; i < len(seg); i++ {
			if seg[i] < 0x20 || seg[i] == 0x7f {
				return false
			}
		}
	}
	return true
}

// isRemotePathChar reports whether c may appear in a raw remote path: RFC 3986
// unreserved and sub-delims, ':', '/' and '%' (escapes are checked
// separately). '@', '\\', quotes, brackets and the like are rejected.
func isRemotePathChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:/%", c) >= 0
}

// validRawRemotePath reports whether a raw (still escaped) remote path uses
// only isRemotePathChar characters and no %2F, which some servers decode to
// '/' so that GitRemote and the cloned repository would differ.
func validRawRemotePath(path string) bool {
	for i := 0; i < len(path); i++ {
		if !isRemotePathChar(path[i]) {
			return false
		}
	}
	return !strings.Contains(strings.ToUpper(path), "%2F")
}

// validEscapedRemotePath is validRemotePath for a path that may still hold
// percent-escapes (SCP and scheme-less forms). The raw path is checked too,
// so neither "org/%2e%2e/repo" nor a literal "org/../repo" is accepted.
func validEscapedRemotePath(path string) bool {
	decoded, err := url.PathUnescape(path)
	return err == nil && validRawRemotePath(path) && validRemotePath(path) && validRemotePath(decoded)
}

// scpLogin matches the login of an SCP-style remote (user@host:path).
var scpLogin = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// splitSCPRemote splits an SCP-style remote "user@host:path" into its parts.
// ok is false when remote is not in that form (it has a scheme, no login, no
// ':' after the host, or a host containing '/').
func splitSCPRemote(remote string) (login, host, path string, ok bool) {
	if strings.Contains(remote, "://") {
		return "", "", "", false
	}
	login, rest, found := strings.Cut(remote, "@")
	if !found {
		return "", "", "", false
	}
	host, path, found = strings.Cut(rest, ":")
	if !found || strings.Contains(host, "/") {
		return "", "", "", false
	}
	return login, host, path, true
}

// validateCloneGitRemote returns "" when a clone's gitRemote override names a
// remote repository, and otherwise the 400 message to return. Accepted forms:
//
//   - scheme URLs (https://, http://, ssh://, git://) that util.IsGitURL
//     accepts and that net/url parses to a valid host, except ssh:// with a
//     port, which NormalizeGitRemote/ToHTTPSCloneURL cannot yet represent
//     (the port would become a path segment). The host after
//     util.StripGitURLCredentials must match the parsed host, so an
//     ambiguous userinfo can never survive stripping or be mistaken for it;
//   - SCP style user@host:org/repo, with any login (not only "git") and any
//     host, including a single-label one such as "gitserver";
//   - scheme-less host[:port]/org/repo with a dotted host, which is how
//     GitRemote is stored (see util.NormalizeGitRemote) and what the web
//     create form already accepts.
//
// Local paths ("/x", "./x", "~/x"), bare names, bare hosts, drive paths and
// SCP without a login (host:org/repo) are rejected, as is, in every form,
// anything but printable ASCII, a malformed %-escape, or a path with '@'
// (raw or %40), "." / ".." or empty segments. git:// with a port and
// http:// with a port other than 80 get errCloneRemoteTLSPort.
func validateCloneGitRemote(remote string) string {
	if !util.IsPrintableASCII(remote) {
		return errCloneRemoteInvalid
	}
	if strings.Contains(remote, "://") {
		return validateCloneSchemeRemote(remote)
	}

	if login, host, path, ok := splitSCPRemote(remote); ok {
		if scpLogin.MatchString(login) && isHostLabels(host) && !strings.Contains(path, "@") &&
			path != "" && !strings.HasPrefix(path, "/") && strings.Contains(strings.Trim(path, "/"), "/") &&
			validEscapedRemotePath(path) {
			return ""
		}
		return errCloneRemoteInvalid
	}

	hostPort, path, _ := strings.Cut(remote, "/")
	host, port, hasPort := strings.Cut(hostPort, ":")
	if !isHostname(host) || (hasPort && !isPort(port)) {
		return errCloneRemoteInvalid
	}
	if port == "80" {
		return errCloneRemoteTLSPort // the clone-url is https, so :80 would be TLS to plain text
	}
	if !strings.Contains(strings.Trim(path, "/"), "/") || strings.Contains(path, "@") ||
		!validEscapedRemotePath(path) {
		return errCloneRemoteInvalid // need at least org/repo, no '@', no dot or empty segments
	}
	return ""
}

// validateCloneSchemeRemote validates a gitRemote override with a scheme.
func validateCloneSchemeRemote(remote string) string {
	u, err := url.Parse(remote)
	if err != nil {
		// Report an ssh port even when the rest fails to parse.
		if scheme, rest, _ := strings.Cut(remote, "://"); strings.EqualFold(scheme, "ssh") {
			authority, _, _ := strings.Cut(rest, "/")
			if at := strings.LastIndex(authority, "@"); at >= 0 {
				authority = authority[at+1:]
			}
			if _, port, ok := strings.Cut(authority, ":"); ok && isPort(port) {
				return errCloneRemoteSSHPort
			}
		}
		return errCloneRemoteInvalid
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http", "ssh", "git":
	default:
		return errCloneRemoteInvalid
	}
	host := u.Hostname()
	if !isURLHost(host) {
		return errCloneRemoteInvalid
	}
	if strings.EqualFold(u.Scheme, "ssh") && (u.Port() != "" || strings.HasSuffix(u.Host, ":")) {
		return errCloneRemoteSSHPort
	}
	// A port must be 1-65535 without leading zeros; a bare ':' is rejected.
	if strings.HasSuffix(u.Host, ":") || (u.Port() != "" && !isPort(u.Port())) {
		return errCloneRemoteInvalid
	}
	switch strings.ToLower(u.Scheme) {
	case "git":
		if u.Port() != "" {
			return errCloneRemoteTLSPort
		}
	case "http":
		if u.Port() != "" && u.Port() != "80" {
			return errCloneRemoteTLSPort
		}
	}
	// Path characters, %2F, dot and empty segments, checked on the raw path
	// as written (net/url's EscapedPath may re-escape it) and decoded (u.Path).
	_, rest, _ := strings.Cut(remote, "://")
	rawPath := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rawPath = rest[i+1:]
	}
	if !validRawRemotePath(rawPath) || !validRemotePath(rawPath) ||
		!validRemotePath(strings.TrimPrefix(u.Path, "/")) {
		return errCloneRemoteInvalid
	}
	// '@' in the path would be ambiguous with userinfo (and could make a
	// lenient parser clone a different repository).
	if strings.Contains(u.EscapedPath(), "@") || strings.Contains(u.Path, "@") {
		return errCloneRemoteInvalid
	}
	if !util.IsGitURL(remote) {
		return errCloneRemoteInvalid
	}
	// Removing credentials must change nothing but the userinfo (ssh keeps
	// the login): fail closed if the host, port or path would differ.
	stripped, err := url.Parse(util.StripGitURLCredentials(remote))
	if err != nil {
		return errCloneRemoteInvalid
	}
	want := *u
	want.User = nil
	if strings.EqualFold(u.Scheme, "ssh") && u.User != nil && u.User.Username() != "" {
		want.User = url.User(u.User.Username())
	}
	if stripped.String() != want.String() {
		return errCloneRemoteInvalid
	}
	return ""
}

// dropDefaultPort removes an explicit default port (:443 for https and for
// the scheme-less form, :80 for http) from a validated, credential-free remote, so that
// https://github.com:443/org/repo names the same repository as
// https://github.com/org/repo. Other inputs are returned unchanged.
func dropDefaultPort(remote string) string {
	scheme, rest, ok := strings.Cut(remote, "://")
	if !ok {
		// Scheme-less host:443/org/repo: the clone-url is https, so :443 is
		// the default port. SCP remotes have no port.
		if _, _, _, scp := splitSCPRemote(remote); scp {
			return remote
		}
		if hostPort, path, found := strings.Cut(remote, "/"); found && strings.HasSuffix(hostPort, ":443") {
			return strings.TrimSuffix(hostPort, ":443") + "/" + path
		}
		return remote
	}
	var port string
	switch strings.ToLower(scheme) {
	case "https":
		port = ":443"
	case "http":
		port = ":80"
	default:
		return remote
	}
	authority, path, hasPath := strings.Cut(rest, "/")
	if strings.Contains(authority, "@") || !strings.HasSuffix(authority, port) {
		return remote
	}
	authority = strings.TrimSuffix(authority, port)
	if hasPath {
		return scheme + "://" + authority + "/" + path
	}
	return scheme + "://" + authority
}

// isHostname reports whether s looks like a DNS hostname with a dot
// ("github.com", "git.example.co"), which rules out local paths, "~" and
// drive letters in scheme-less remotes.
func isHostname(s string) bool {
	return hostnamePattern.MatchString(s)
}

// hostnamePattern matches two or more dot-separated DNS labels (a label may
// not start or end with '-').
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$`)

// isHostLabels reports whether s is one or more dot-separated DNS labels
// ("gitserver", "github.com").
func isHostLabels(s string) bool {
	return hostLabelsPattern.MatchString(s)
}

// hostLabelsPattern matches one or more dot-separated DNS labels (a label may
// not start or end with '-').
var hostLabelsPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// isURLHost reports whether s (a url.URL Hostname) is a DNS name or an IP
// address.
func isURLHost(s string) bool {
	return isHostLabels(s) || net.ParseIP(s) != nil
}

// isPort reports whether s is a decimal TCP port.
func isPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == s
}

// canonicalCloneRemote rewrites an SCP remote with a non-"git" login
// (alice@host:org/repo) to the "git@host:org/repo" form, because
// util.NormalizeGitRemote and util.ToHTTPSCloneURL only treat the "git@"
// login as SCP syntax. The login does not affect either result (GitRemote is
// login-free and clone-url is https). Other forms are returned unchanged.
func canonicalCloneRemote(remote string) string {
	if login, host, path, ok := splitSCPRemote(remote); ok && login != "git" {
		return "git@" + host + ":" + path
	}
	return remote
}

// isGitSourceLabel reports whether k is one of the labels that describe a
// project's git source repository and must track Project.GitRemote.
func isGitSourceLabel(k string) bool {
	switch k {
	case store.LabelCloneURL, store.LabelSourceURL, store.LabelDefaultBranch:
		return true
	}
	return false
}

// validateCloneURLLabelValue checks the clone-url label in labels (if any)
// and returns a user-facing message when it is not a plain repository URL,
// or "" when it is acceptable. Every hub path that writes project labels from
// a request calls it, so credentials, query strings and fragments never reach
// the stored label. Clone authentication belongs in project secrets or the
// GitHub App instead.
func validateCloneURLLabelValue(labels map[string]string) string {
	v, ok := labels[store.LabelCloneURL]
	if !ok {
		return ""
	}
	if err := util.ValidateCloneURLLabel(v); err != nil {
		return cloneURLRefusalMessage(err)
	}
	return ""
}

// cloneURLRefusalMessage is the constant user-facing 400 message for a
// refused clone URL. It names what to remove but never echoes the value.
func cloneURLRefusalMessage(err error) string {
	var problem string
	switch {
	case errors.Is(err, util.ErrCloneURLInvalid):
		problem = "remove whitespace and control or non-ASCII characters from the URL"
	case errors.Is(err, util.ErrCloneURLUserinfo):
		problem = "remove the username, password or token from the URL ('@' is allowed only in an ssh or scp-style login)"
	case errors.Is(err, util.ErrCloneURLQuery):
		problem = "remove the query string (?...) from the URL"
	case errors.Is(err, util.ErrCloneURLFragment):
		problem = "remove the fragment (#...) from the URL"
	default:
		problem = "use a plain repository URL"
	}
	return "Invalid " + store.LabelCloneURL + " label: " + problem +
		". The label must be a plain repository URL; configure clone authentication with project secrets or the GitHub App instead"
}

// normalizeRequestGitRemote returns the normalized form of a git remote from a
// create or register request, or a constant 400 message. The query and
// fragment are dropped first (as the clone path does): git remotes never need
// them and they can carry tokens. A remote whose dropped part contains '@'
// (a '?' or '#' inside the password) or whose normalized form still contains
// '@' is refused rather than repaired.
func normalizeRequestGitRemote(raw string) (normalized, msg string) {
	rest, ok := util.CutQueryAndFragment(raw)
	if !ok {
		return "", cloneURLRefusalMessage(util.ErrCloneURLUserinfo)
	}
	normalized = util.NormalizeGitRemote(rest)
	return normalized, validateNormalizedGitRemote(normalized)
}

// validateNormalizedGitRemote refuses a git remote whose normalized form
// (util.NormalizeGitRemote, which drops ordinary userinfo) still contains
// '@': the input carried a password or an extra '@' in scp form
// (git@user:PASS@host:org/repo), and storing it would keep the credential in
// Project.GitRemote. The value is not repaired. The message is the same
// constant as the clone-url refusal and never echoes the value.
func validateNormalizedGitRemote(normalized string) string {
	if strings.Contains(normalized, "@") {
		return cloneURLRefusalMessage(util.ErrCloneURLUserinfo)
	}
	return ""
}

// sanitizeSourceURLLabel rewrites the source-url label in labels (if any) to
// its credential-, query- and fragment-free form, mirroring how the clone
// path above sanitizes a git remote override before storing it. The label is
// readable by project members, so it never keeps what was stripped; a value
// that cannot be sanitized unambiguously is removed. labels is modified in
// place.
func sanitizeSourceURLLabel(labels map[string]string) {
	v, ok := labels[store.LabelSourceURL]
	if !ok {
		return
	}
	if clean := util.SanitizeGitSourceURL(v); clean != "" {
		labels[store.LabelSourceURL] = clean
	} else {
		delete(labels, store.LabelSourceURL)
	}
}

// sanitizeCopiedCloneURLLabel makes a clone-url label copied from another
// project (which may predate write-time validation) acceptable to
// validateCloneURLLabelValue: a value that fails validation is replaced by its
// sanitized form, or removed when that is still not a plain repository URL.
// labels is modified in place.
func sanitizeCopiedCloneURLLabel(labels map[string]string) {
	v, ok := labels[store.LabelCloneURL]
	if !ok || util.ValidateCloneURLLabel(v) == nil {
		return
	}
	if clean := util.SanitizeGitSourceURL(v); clean != "" && util.ValidateCloneURLLabel(clean) == nil {
		labels[store.LabelCloneURL] = clean
	} else {
		delete(labels, store.LabelCloneURL)
	}
}

// cloneURLLabelErrorDetails is the details payload for a rejected clone-url
// label. It names the field but never echoes the value.
func cloneURLLabelErrorDetails() map[string]interface{} {
	return map[string]interface{}{"field": "labels." + store.LabelCloneURL}
}
