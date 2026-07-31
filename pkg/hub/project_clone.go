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
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CloneProjectRequest is the request body for POST /api/v1/projects/{id}/clone.
type CloneProjectRequest struct {
	Name string `json:"name"` // required
	Slug string `json:"slug"` // optional explicit slug override
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
		MethodNotAllowed(w)
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

	// ── Step 2: Resolve name/slug ────────────────────────────────────────

	baseSlug := req.Slug
	explicitSlug := baseSlug != ""
	if !explicitSlug {
		baseSlug = api.Slugify(req.Name)
	}

	slug, err := s.store.NextAvailableSlug(ctx, baseSlug)
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
		Visibility:             store.VisibilityPrivate,
		CreatedBy:              callerID,
		OwnerID:                callerID,
		SharedDirs:             src.SharedDirs,
		GitIdentity:            src.GitIdentity,
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
			clone.Labels[k] = v
		}
		if len(clone.Labels) == 0 {
			clone.Labels = nil
		}
	}

	// Re-derive workspace mode from the source (like createProject does).
	if src.GitRemote != "" {
		srcMode := ""
		if src.Labels != nil {
			srcMode = src.Labels[store.LabelWorkspaceMode]
		}
		switch srcMode {
		case store.WorkspaceModeShared, store.WorkspaceModeWorktreePerAgent:
			if clone.Labels == nil {
				clone.Labels = make(map[string]string)
			}
			clone.Labels[store.LabelWorkspaceMode] = srcMode
		}
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
		if delErr := s.store.DeleteProject(rbCtx, clone.ID); delErr != nil {
			slog.Warn("project clone rollback: failed to delete project",
				"clone_id", clone.ID, "error", delErr)
		}
	})

	// ── Step 5: Create groups ────────────────────────────────────────────

	s.createProjectGroup(ctx, clone)

	// ── Step 6: Create members group and policy ──────────────────────────

	s.createProjectMembersGroupAndPolicy(ctx, clone, callerID)

	// ── Step 7: Deep-copy project-scoped harness configs ─────────────────

	if err := s.cloneProjectHarnessConfigs(ctx, src.ID, clone, &rollback); err != nil {
		slog.Error("project clone: harness config copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy harness configs: "+err.Error(), nil)
		return
	}

	// ── Step 8: Deep-copy project-scoped templates ───────────────────────

	if err := s.cloneProjectTemplates(ctx, src.ID, clone, &rollback); err != nil {
		slog.Error("project clone: template copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy templates: "+err.Error(), nil)
		return
	}

	// ── Step 9: Copy non-secret env vars ─────────────────────────────────

	if err := s.cloneProjectEnvVars(ctx, src.ID, clone.ID, callerID, &rollback); err != nil {
		slog.Error("project clone: env var copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy environment variables: "+err.Error(), nil)
		return
	}

	// ── Step 10: Copy injected skills ────────────────────────────────────

	if err := s.cloneProjectSkillInjections(ctx, src.ID, clone.ID, callerID, &rollback); err != nil {
		slog.Error("project clone: skill injection copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy injected skills: "+err.Error(), nil)
		return
	}

	// ── Step 11: Copy active pre-start hook ──────────────────────────────

	if err := s.cloneProjectPreStartHook(ctx, src.ID, clone.ID, callerID); err != nil {
		slog.Error("project clone: pre-start hook copy failed",
			"source_id", src.ID, "clone_id", clone.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to copy pre-start hook: "+err.Error(), nil)
		return
	}
	// No explicit rollback for step 11 — hook is cleaned up transitively by
	// step 4's DeleteProject. See design doc §5.5.

	// ── Step 12: Auto-associate GitHub installation (best-effort) ────────

	if clone.GitRemote != "" && clone.GitHubInstallationID == nil {
		s.autoAssociateGitHubInstallation(ctx, clone)
	}

	// ── Step 13: Workspace init ──────────────────────────────────────────

	if clone.IsSharedWorkspace() {
		if err := s.cloneSharedWorkspaceProject(ctx, clone); err != nil {
			slog.Error("project clone: shared workspace clone failed",
				"clone_id", clone.ID, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to initialize workspace: "+err.Error(), nil)
			return
		}
	} else if clone.GitRemote == "" {
		if err := s.initHubManagedProject(clone); err != nil {
			slog.Warn("project clone: failed to initialize hub-managed workspace",
				"clone_id", clone.ID, "error", err)
		}
	}

	// ── Step 14: Auto-link providers (best-effort) ───────────────────────

	s.autoLinkProviders(ctx, clone)

	// ── Step 15: Publish event (best-effort) ─────────────────────────────

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
			Visibility:  srcHC.Visibility,
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
					_ = stor.DeletePrefix(ctx, storagePath)
					return err
				}
			}
		}

		if err := s.store.CreateHarnessConfig(ctx, newHC); err != nil {
			if stor != nil {
				_ = stor.DeletePrefix(ctx, storagePath)
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
			_ = stor.DeletePrefix(rbCtx, prefix)
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
			Visibility:   srcTmpl.Visibility,
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
					_ = stor.DeletePrefix(ctx, storagePath)
					return err
				}
			}
		}

		if err := s.store.CreateTemplate(ctx, newTmpl); err != nil {
			if stor != nil {
				_ = stor.DeletePrefix(ctx, storagePath)
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
			_ = stor.DeletePrefix(rbCtx, prefix)
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
