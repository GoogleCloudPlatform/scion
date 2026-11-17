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
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// AnnotationGitHubTriggersPrefix guards all project annotations written by
	// /api/v1/projects/{id}/trigger-sources/github from generic project PATCH
	// writes (§8.3).
	AnnotationGitHubTriggersPrefix = "scion.dev/github-triggers."

	// AnnotationGitHubTriggersUnmatchedReply stores whether the hub posts a
	// fallback reply when a /command mention matches no trigger rule (§8.3).
	AnnotationGitHubTriggersUnmatchedReply = AnnotationGitHubTriggersPrefix + "unmatched-command-reply"

	// AnnotationGitHubTriggersUnmatchedText stores the optional custom suffix
	// appended to the unmatched-command reply (§8.3).
	AnnotationGitHubTriggersUnmatchedText = AnnotationGitHubTriggersPrefix + "unmatched-command-text"

	// AnnotationGitHubTriggersUpdatedBy records the user principal ID of the
	// last editor who configured the project's GitHub trigger source settings,
	// so downstream unmatched-command replies can attribute their audit actor
	// without scanning mutation logs (§8.3).
	AnnotationGitHubTriggersUpdatedBy = AnnotationGitHubTriggersPrefix + "updated-by"

	// MaxGitHubTriggersUnmatchedTextBytes bounds custom unmatched-command text.
	MaxGitHubTriggersUnmatchedTextBytes = 1024

	// WarningGitHubTriggersMultiProjectRepo is returned on GET/PUT trigger
	// source settings when multiple candidate projects share the repository and
	// no active primary project is configured (§8.4).
	WarningGitHubTriggersMultiProjectRepo = "multi_project_repo"

	githubTriggerSettingsAuditMutation = "project.github_trigger_settings.update"
	githubTriggerSettingsAuditTarget   = "project"
)

// ProjectGitHubTriggerPrimaryRef identifies the active primary project for a
// shared repository (§8.4).
type ProjectGitHubTriggerPrimaryRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectGitHubTriggerRoutingView is the project-scoped view of repository
// routing returned by GET/PUT /api/v1/projects/{id}/trigger-sources/github (§8.4).
type ProjectGitHubTriggerRoutingView struct {
	PrimaryProject *ProjectGitHubTriggerPrimaryRef `json:"primary_project"`
	IsPrimary      bool                            `json:"is_primary"`
	OtherProjects  []string                        `json:"other_projects"`
}

// ProjectGitHubTriggerSettingsResponse is the response payload for
// GET/PUT /api/v1/projects/{id}/trigger-sources/github (§8.3, §8.4, §10.2).
type ProjectGitHubTriggerSettingsResponse struct {
	UnmatchedCommandReply bool                            `json:"unmatched_command_reply"`
	UnmatchedCommandText  string                          `json:"unmatched_command_text"`
	Readiness             GitHubTriggersProjectReadiness  `json:"readiness"`
	Routing               ProjectGitHubTriggerRoutingView `json:"routing"`
	Warnings              []string                        `json:"warnings,omitempty"`
}

// ProjectGitHubTriggerSettingsUpdateRequest is the request body for
// PUT /api/v1/projects/{id}/trigger-sources/github (§8.3).
type ProjectGitHubTriggerSettingsUpdateRequest struct {
	UnmatchedCommandReply bool   `json:"unmatched_command_reply"`
	UnmatchedCommandText  string `json:"unmatched_command_text"`
}

func githubTriggerResource(project *store.Project) Resource {
	return Resource{
		Type:       permissions.ResourceGitHubTrigger,
		ID:         project.ID,
		ParentType: permissions.ResourceProject,
		ParentID:   project.ID,
	}
}

// changesGitHubTriggerAnnotations reports whether replacing stored annotations
// with patched adds, removes, or changes any key under
// AnnotationGitHubTriggersPrefix (§8.3).
func changesGitHubTriggerAnnotations(stored, patched map[string]string) bool {
	for k, sv := range stored {
		if !strings.HasPrefix(k, AnnotationGitHubTriggersPrefix) {
			continue
		}
		if pv, ok := patched[k]; !ok || pv != sv {
			return true
		}
	}
	for k, pv := range patched {
		if !strings.HasPrefix(k, AnnotationGitHubTriggersPrefix) {
			continue
		}
		if sv, ok := stored[k]; !ok || sv != pv {
			return true
		}
	}
	return false
}

func readProjectGitHubTriggerSettings(annotations map[string]string) (reply bool, text string) {
	if annotations == nil {
		return false, ""
	}
	return annotations[AnnotationGitHubTriggersUnmatchedReply] == "true",
		annotations[AnnotationGitHubTriggersUnmatchedText]
}

func validateUnmatchedCommandText(text string) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("unmatched_command_text must be valid UTF-8")
	}
	if strings.ContainsRune(text, 0) {
		return fmt.Errorf("unmatched_command_text must not contain NUL bytes")
	}
	if len(text) > MaxGitHubTriggersUnmatchedTextBytes {
		return fmt.Errorf("unmatched_command_text must be at most %d bytes", MaxGitHubTriggersUnmatchedTextBytes)
	}
	return nil
}

func formatGitHubTriggerSettingsAuditSummary(reply bool, textBytes int) string {
	return fmt.Sprintf("unmatched_command_reply=%t,unmatched_command_text_bytes=%d", reply, textBytes)
}

func newGitHubTriggerSettingsAuditRecord(
	ctx context.Context,
	projectID string,
	beforeReply bool,
	beforeTextBytes int,
	afterReply bool,
	afterTextBytes int,
) *store.MutationAuditRecord {
	rec := &store.MutationAuditRecord{
		Timestamp:     timeNow().UTC(),
		MutationType:  githubTriggerSettingsAuditMutation,
		TargetType:    githubTriggerSettingsAuditTarget,
		TargetID:      projectID,
		BeforeSummary: formatGitHubTriggerSettingsAuditSummary(beforeReply, beforeTextBytes),
		AfterSummary:  formatGitHubTriggerSettingsAuditSummary(afterReply, afterTextBytes),
	}
	auditActorFromContext(ctx).ApplyActor(rec)
	if rec.ActorPrincipalKind == "" {
		rec.ActorPrincipalKind = "system"
	}
	if rec.ActorPrincipalID == "" {
		rec.ActorPrincipalID = "unknown"
	}
	return rec
}

// handleProjectTriggerSourcesRoutes dispatches /api/v1/projects/{id}/trigger-sources/...
func (s *Server) handleProjectTriggerSourcesRoutes(w http.ResponseWriter, r *http.Request, projectID, subPath string) {
	if subPath != "trigger-sources/github" {
		NotFound(w, "Trigger source")
		return
	}
	s.handleProjectGitHubTriggerSettings(w, r, projectID)
}

// handleProjectGitHubTriggerSettings handles GET and PUT
// /api/v1/projects/{id}/trigger-sources/github.
func (s *Server) handleProjectGitHubTriggerSettings(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet, http.MethodPut:
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
		return
	}

	s.requireGitHubTriggers(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if GetAgentIdentityFromContext(ctx) != nil {
			Forbidden(w)
			return
		}
		if isVisitorIdentity(GetIdentityFromContext(ctx)) {
			writeVisitorRefusal(w)
			return
		}

		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "Project")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}

		switch r.Method {
		case http.MethodGet:
			s.handleGetProjectGitHubTriggerSettings(w, r, project)
		case http.MethodPut:
			s.handlePutProjectGitHubTriggerSettings(w, r, project)
		}
	})(w, r)
}

func (s *Server) handleGetProjectGitHubTriggerSettings(w http.ResponseWriter, r *http.Request, project *store.Project) {
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}
	resp, err := s.buildProjectGitHubTriggerSettingsResponse(r.Context(), project)
	if err != nil {
		InternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePutProjectGitHubTriggerSettings(w http.ResponseWriter, r *http.Request, project *store.Project) {
	if !s.authorize(w, r, githubTriggerResource(project), Action(permissions.ActionConfigure)) {
		return
	}

	var req ProjectGitHubTriggerSettingsUpdateRequest
	if err := readJSON(r, &req); err != nil {
		ValidationError(w, "invalid request body: "+err.Error(), nil)
		return
	}
	if err := validateUnmatchedCommandText(req.UnmatchedCommandText); err != nil {
		ValidationError(w, err.Error(), map[string]interface{}{"field": "unmatched_command_text"})
		return
	}

	ctx := r.Context()
	actor := auditActorFromContext(ctx)
	var updated *store.Project
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		cur, txErr := tx.GetProject(ctx, project.ID)
		if txErr != nil {
			return txErr
		}
		beforeReply, beforeText := readProjectGitHubTriggerSettings(cur.Annotations)

		nextAnnotations := maps.Clone(cur.Annotations)
		if nextAnnotations == nil {
			nextAnnotations = make(map[string]string, 3)
		}
		nextAnnotations[AnnotationGitHubTriggersUnmatchedReply] = strconv.FormatBool(req.UnmatchedCommandReply)
		if req.UnmatchedCommandText == "" {
			delete(nextAnnotations, AnnotationGitHubTriggersUnmatchedText)
		} else {
			nextAnnotations[AnnotationGitHubTriggersUnmatchedText] = req.UnmatchedCommandText
		}
		if actor.PrincipalID != "" {
			nextAnnotations[AnnotationGitHubTriggersUpdatedBy] = actor.PrincipalID
		}
		cur.Annotations = nextAnnotations

		if txErr := tx.UpdateProject(ctx, cur); txErr != nil {
			return txErr
		}
		rec := newGitHubTriggerSettingsAuditRecord(
			ctx,
			cur.ID,
			beforeReply,
			len(beforeText),
			req.UnmatchedCommandReply,
			len(req.UnmatchedCommandText),
		)
		if txErr := tx.CreateMutationAudit(ctx, rec); txErr != nil {
			return txErr
		}
		updated = cur
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		InternalError(w)
		return
	}

	s.events.PublishProjectUpdated(ctx, updated)

	resp, err := s.buildProjectGitHubTriggerSettingsResponse(ctx, updated)
	if err != nil {
		InternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) buildProjectGitHubTriggerSettingsResponse(
	ctx context.Context,
	project *store.Project,
) (ProjectGitHubTriggerSettingsResponse, error) {
	reply, text := readProjectGitHubTriggerSettings(project.Annotations)
	readiness := s.GitHubTriggersProjectReadiness(ctx)
	routing, warnings, err := s.projectGitHubTriggerRoutingView(ctx, project)
	if err != nil {
		return ProjectGitHubTriggerSettingsResponse{}, err
	}
	return ProjectGitHubTriggerSettingsResponse{
		UnmatchedCommandReply: reply,
		UnmatchedCommandText:  text,
		Readiness:             readiness,
		Routing:               routing,
		Warnings:              warnings,
	}, nil
}

const githubTriggerRoutingCandidatePageSize = 500

// projectGitHubTriggerRoutingView derives the project-scoped routing card view
// directly for this project's installation and repository (§4.2, §8.4) without
// triggering a hub-wide GitHub association report build.
func (s *Server) projectGitHubTriggerRoutingView(
	ctx context.Context,
	project *store.Project,
) (ProjectGitHubTriggerRoutingView, []string, error) {
	empty := ProjectGitHubTriggerRoutingView{OtherProjects: []string{}}
	if project == nil || project.GitHubInstallationID == nil || *project.GitHubInstallationID <= 0 {
		return empty, nil, nil
	}
	ownerRepo := strings.ToLower(strings.TrimSpace(s.githubOwnerRepo(project.GitRemote)))
	if !s.IsGitHubTriggerCandidate(*project.GitHubInstallationID, project, ownerRepo) {
		return empty, nil, nil
	}

	inst, err := s.store.GetGitHubInstallation(ctx, *project.GitHubInstallationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return empty, nil, nil
		}
		return empty, nil, err
	}

	instID := inst.InstallationID
	var candidates []store.Project
	seenTarget := false
	cursor := ""
	for {
		page, listErr := s.store.ListProjects(ctx, store.ProjectFilter{
			GitHubInstallationID: &instID,
		}, store.ListOptions{
			Limit:          githubTriggerRoutingCandidatePageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if listErr != nil {
			return empty, nil, listErr
		}
		for i := range page.Items {
			p := page.Items[i]
			if p.ID == project.ID {
				p = *project
				seenTarget = true
			}
			if s.IsGitHubTriggerCandidate(instID, &p, ownerRepo) {
				candidates = append(candidates, p)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !seenTarget && s.IsGitHubTriggerCandidate(instID, project, ownerRepo) {
		candidates = append(candidates, *project)
	}
	if len(candidates) == 0 {
		return empty, nil, nil
	}

	selected, _, status := ApplyPrimaryRouting(candidates, inst.TriggerRouting, ownerRepo)
	var activePrimary *ProjectGitHubTriggerPrimaryRef
	if status == GitHubTriggerRoutingStatusOK && len(selected) == 1 {
		activePrimary = &ProjectGitHubTriggerPrimaryRef{
			ID:   selected[0].ID,
			Name: selected[0].Name,
		}
	}

	otherProjects := make([]string, 0, len(candidates)-1)
	for i := range candidates {
		if candidates[i].ID == project.ID {
			continue
		}
		name := candidates[i].Name
		if name == "" {
			name = candidates[i].ID
		}
		otherProjects = append(otherProjects, name)
	}
	sort.Strings(otherProjects)

	view := ProjectGitHubTriggerRoutingView{
		PrimaryProject: activePrimary,
		IsPrimary:      activePrimary != nil && activePrimary.ID == project.ID,
		OtherProjects:  otherProjects,
	}
	var warnings []string
	if len(otherProjects) > 0 && activePrimary == nil {
		warnings = []string{WarningGitHubTriggersMultiProjectRepo}
	}
	return view, warnings, nil
}
