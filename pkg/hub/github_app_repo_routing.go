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
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Primary-project routing statuses reported on GitHub App repo-routing
// responses and returned by ApplyPrimaryRouting (§4.2, §8.4).
const (
	GitHubTriggerRoutingStatusNone         = "none"
	GitHubTriggerRoutingStatusOK           = "ok"
	GitHubTriggerRoutingStatusStalePrimary = "stale_primary"
)

const (
	// ErrCodeNotACandidate is returned (HTTP 409) when PUT repo-routing names a
	// project that is not a current candidate for that repository and installation.
	ErrCodeNotACandidate = "not_a_candidate"

	githubRepoRoutingAuditMutation = "hub.github_app.repo_routing.update"
	githubRepoRoutingAuditTarget   = "github_installation"
)

// GitHubRepoRoutingProject summarizes one candidate project on a repository
// routing row (§8.4).
type GitHubRepoRoutingProject struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	HasGitHubTriggers bool   `json:"has_github_triggers"`
}

// GitHubRepoRoutingEntry describes primary-project routing for one repository
// under a GitHub App installation (§8.4).
type GitHubRepoRoutingEntry struct {
	InstallationID   int64                      `json:"installation_id"`
	Repo             string                     `json:"repo"`
	Projects         []GitHubRepoRoutingProject `json:"projects"`
	PrimaryProjectID string                     `json:"primary_project_id,omitempty"`
	Status           string                     `json:"status"`
}

// GitHubRepoRoutingPutRequest is the request body for
// PUT /api/v1/github-app/repo-routing/{owner}/{repo}.
type GitHubRepoRoutingPutRequest struct {
	InstallationID   int64  `json:"installation_id"`
	PrimaryProjectID string `json:"primary_project_id"`
}

// ApplyPrimaryRouting applies installation-level primary-project routing (§4.2
// step 4, §8.4) to the candidate project list for repo:
//   - No configured entry for repo (case-insensitive): returns all candidates,
//     nil bypassed list, and status "none".
//   - Configured primary is in candidates: returns [primary], the IDs of all
//     other candidates in candidates order as bypassed, and status "ok".
//   - Configured primary is not in candidates: fails open by returning all
//     candidates, nil bypassed list, and status "stale_primary".
func ApplyPrimaryRouting(
	candidates []store.Project,
	routing map[string]store.GitHubTriggerRoute,
	repo string,
) (selected []store.Project, bypassed []string, status string) {
	cloneCandidates := func() []store.Project {
		if len(candidates) == 0 {
			return []store.Project{}
		}
		out := make([]store.Project, len(candidates))
		copy(out, candidates)
		return out
	}

	route, ok := lookupTriggerRoute(routing, repo)
	if !ok || strings.TrimSpace(route.PrimaryProjectID) == "" {
		return cloneCandidates(), nil, GitHubTriggerRoutingStatusNone
	}

	var primary *store.Project
	for i := range candidates {
		if candidates[i].ID == route.PrimaryProjectID {
			primary = &candidates[i]
			break
		}
	}
	if primary == nil {
		return cloneCandidates(), nil, GitHubTriggerRoutingStatusStalePrimary
	}

	selected = []store.Project{*primary}
	if len(candidates) > 1 {
		bypassed = make([]string, 0, len(candidates)-1)
		for i := range candidates {
			if candidates[i].ID != primary.ID {
				bypassed = append(bypassed, candidates[i].ID)
			}
		}
	}
	return selected, bypassed, GitHubTriggerRoutingStatusOK
}

func lookupTriggerRoute(routing map[string]store.GitHubTriggerRoute, repo string) (store.GitHubTriggerRoute, bool) {
	if len(routing) == 0 {
		return store.GitHubTriggerRoute{}, false
	}
	lower := strings.ToLower(strings.TrimSpace(repo))
	if lower == "" {
		return store.GitHubTriggerRoute{}, false
	}
	if r, ok := routing[lower]; ok {
		return r, true
	}
	for k, r := range routing {
		if strings.EqualFold(strings.TrimSpace(k), lower) {
			return r, true
		}
	}
	return store.GitHubTriggerRoute{}, false
}

// IsGitHubTriggerCandidate reports whether proj is a candidate project for
// GitHub trigger fan-out on repo under installationID, implementing design §4.2
// steps 1-3:
//  1. proj.GitHubInstallationID != nil && *proj.GitHubInstallationID == installationID
//  2. s.githubOwnerRepo(proj.GitRemote) (host-pinned) matches repo case-insensitively.
//
// Neither GitHubAppStatus (e.g. repo_not_accessible) nor the installation's
// cached Repositories list excludes a project here: §4.2 checks association
// authorization at fire time in the drainer, and GitHub may deliver events
// while the cached repository list is stale.
//
// Stage 3 G1's webhook fan-out (§4.2 steps 1-3, before ApplyPrimaryRouting)
// MUST call IsGitHubTriggerCandidate so ingress fan-out and admin repo-routing
// status / PUT candidacy validation never diverge.
func (s *Server) IsGitHubTriggerCandidate(installationID int64, proj *store.Project, repo string) bool {
	if installationID <= 0 || proj == nil || proj.GitHubInstallationID == nil || *proj.GitHubInstallationID != installationID {
		return false
	}
	target := strings.TrimSpace(repo)
	if target == "" {
		return false
	}
	ownerRepo := s.githubOwnerRepo(proj.GitRemote)
	return ownerRepo != "" && strings.EqualFold(ownerRepo, target)
}

// projectHasGitHubTriggers reports whether projectID has any GitHub-sourced
// triggers configured. Until the triggers store lands on main, this returns
// false.
func (s *Server) projectHasGitHubTriggers(_ context.Context, _ string) bool {
	return false
}

// associationEntriesForRouting reads associated project entries from the
// association report cache (§8.4), falling back to a direct project scan only
// if a cache rebuild is rate-limited.
func (s *Server) associationEntriesForRouting(ctx context.Context) ([]githubAssociationReportEntry, error) {
	report, _, err := s.githubAssociationReport(ctx, false)
	if err == nil {
		return report.Entries, nil
	}
	if !errors.Is(err, errGitHubAssociationReportRateLimited) {
		return nil, err
	}

	var entries []githubAssociationReportEntry
	cursor := ""
	for {
		page, listErr := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:          githubAssociationReportPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if listErr != nil {
			return nil, listErr
		}
		for i := range page.Items {
			p := &page.Items[i]
			if p.GitHubInstallationID == nil {
				continue
			}
			entries = append(entries, githubAssociationReportEntry{
				ProjectID:      p.ID,
				ProjectName:    p.Name,
				GitRemote:      p.GitRemote,
				InstallationID: *p.GitHubInstallationID,
			})
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return entries, nil
}

func (s *Server) buildRepoRoutingEntries(
	ctx context.Context,
	installations []store.GitHubInstallation,
	assocEntries []githubAssociationReportEntry,
) []GitHubRepoRoutingEntry {
	instByID := make(map[int64]*store.GitHubInstallation, len(installations))
	for i := range installations {
		inst := &installations[i]
		instByID[inst.InstallationID] = inst
	}

	type instRepoKey struct {
		instID int64
		repo   string
	}
	candidatesByKey := make(map[instRepoKey][]GitHubRepoRoutingProject)
	seenKey := make(map[instRepoKey]struct{})

	for _, e := range assocEntries {
		inst, ok := instByID[e.InstallationID]
		if !ok {
			continue
		}
		ownerRepo := s.githubOwnerRepo(e.GitRemote)
		if ownerRepo == "" {
			continue
		}
		lowerRepo := strings.ToLower(ownerRepo)
		projInstID := e.InstallationID
		candProj := store.Project{
			ID:                   e.ProjectID,
			Name:                 e.ProjectName,
			GitRemote:            e.GitRemote,
			GitHubInstallationID: &projInstID,
		}
		if !s.IsGitHubTriggerCandidate(inst.InstallationID, &candProj, lowerRepo) {
			continue
		}
		key := instRepoKey{instID: inst.InstallationID, repo: lowerRepo}
		seenKey[key] = struct{}{}
		candidatesByKey[key] = append(candidatesByKey[key], GitHubRepoRoutingProject{
			ID:                e.ProjectID,
			Name:              e.ProjectName,
			HasGitHubTriggers: s.projectHasGitHubTriggers(ctx, e.ProjectID),
		})
	}

	// Include configured primary routes even when their candidate list has
	// dropped to zero so admins see stale_primary rows and can clean them up.
	for i := range installations {
		inst := &installations[i]
		for repoKey, route := range inst.TriggerRouting {
			lowerRepo := strings.ToLower(strings.TrimSpace(repoKey))
			if lowerRepo == "" || strings.TrimSpace(route.PrimaryProjectID) == "" {
				continue
			}
			seenKey[instRepoKey{instID: inst.InstallationID, repo: lowerRepo}] = struct{}{}
		}
	}

	out := make([]GitHubRepoRoutingEntry, 0, len(seenKey))
	for key := range seenKey {
		inst := instByID[key.instID]
		projects := candidatesByKey[key]
		if projects == nil {
			projects = []GitHubRepoRoutingProject{}
		}
		sort.Slice(projects, func(i, j int) bool {
			if projects[i].Name != projects[j].Name {
				return projects[i].Name < projects[j].Name
			}
			return projects[i].ID < projects[j].ID
		})

		var primaryID string
		if route, ok := lookupTriggerRoute(inst.TriggerRouting, key.repo); ok {
			primaryID = strings.TrimSpace(route.PrimaryProjectID)
		}

		status := GitHubTriggerRoutingStatusNone
		if primaryID != "" {
			status = GitHubTriggerRoutingStatusStalePrimary
			for _, p := range projects {
				if p.ID == primaryID {
					status = GitHubTriggerRoutingStatusOK
					break
				}
			}
		}

		out = append(out, GitHubRepoRoutingEntry{
			InstallationID:   key.instID,
			Repo:             key.repo,
			Projects:         projects,
			PrimaryProjectID: primaryID,
			Status:           status,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].InstallationID != out[j].InstallationID {
			return out[i].InstallationID < out[j].InstallationID
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

func parseInstallationQueryFilter(r *http.Request) (*int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("installation"))
	if raw == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("invalid installation parameter %q", raw)
	}
	return &id, nil
}

func parseOwnerRepoPath(r *http.Request) (string, error) {
	owner := strings.TrimSpace(r.PathValue("owner"))
	repo := strings.TrimSpace(r.PathValue("repo"))
	if owner == "" || repo == "" || strings.Contains(owner, "/") || strings.Contains(repo, "/") {
		return "", fmt.Errorf("owner and repo path segments are required")
	}
	return strings.ToLower(owner + "/" + repo), nil
}

func formatRepoRoutingAuditSummary(lowerRepo, primaryProjectID string) string {
	return fmt.Sprintf("repo=%s,primary_project_id=%s", lowerRepo, primaryProjectID)
}

func newRepoRoutingAuditRecord(ctx context.Context, installationID int64, lowerRepo, beforePrimary, afterPrimary string) *store.MutationAuditRecord {
	rec := &store.MutationAuditRecord{
		Timestamp:     timeNow().UTC(),
		MutationType:  githubRepoRoutingAuditMutation,
		TargetType:    githubRepoRoutingAuditTarget,
		TargetID:      strconv.FormatInt(installationID, 10),
		BeforeSummary: formatRepoRoutingAuditSummary(lowerRepo, beforePrimary),
		AfterSummary:  formatRepoRoutingAuditSummary(lowerRepo, afterPrimary),
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

// handleListGitHubAppRepoRouting handles GET /api/v1/github-app/repo-routing.
func (s *Server) handleListGitHubAppRepoRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	instFilter, err := parseInstallationQueryFilter(r)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	ctx := r.Context()
	installations, err := s.store.ListGitHubInstallations(ctx, store.GitHubInstallationFilter{})
	if err != nil {
		InternalError(w)
		return
	}
	if instFilter != nil {
		filtered := make([]store.GitHubInstallation, 0, 1)
		for i := range installations {
			if installations[i].InstallationID == *instFilter {
				filtered = append(filtered, installations[i])
			}
		}
		installations = filtered
	}
	if len(installations) == 0 {
		writeJSON(w, http.StatusOK, []GitHubRepoRoutingEntry{})
		return
	}

	assocEntries, err := s.associationEntriesForRouting(ctx)
	if err != nil {
		InternalError(w)
		return
	}

	entries := s.buildRepoRoutingEntries(ctx, installations, assocEntries)
	writeJSON(w, http.StatusOK, entries)
}

// handlePutGitHubAppRepoRouting handles PUT /api/v1/github-app/repo-routing/{owner}/{repo}.
func (s *Server) handlePutGitHubAppRepoRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		MethodNotAllowed(w, http.MethodPut)
		return
	}

	lowerRepo, err := parseOwnerRepoPath(r)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	var req GitHubRepoRoutingPutRequest
	if err := readJSON(r, &req); err != nil {
		ValidationError(w, "invalid request body: "+err.Error(), nil)
		return
	}
	req.PrimaryProjectID = strings.TrimSpace(req.PrimaryProjectID)
	if req.InstallationID <= 0 {
		ValidationError(w, "installation_id must be a positive integer", nil)
		return
	}
	if req.PrimaryProjectID == "" {
		ValidationError(w, "primary_project_id is required", nil)
		return
	}

	ctx := r.Context()
	inst, err := s.store.GetGitHubInstallation(ctx, req.InstallationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GitHub installation")
			return
		}
		InternalError(w)
		return
	}

	proj, err := s.store.GetProject(ctx, req.PrimaryProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, ErrCodeNotACandidate,
				"project is not a current candidate for this repository under this installation", nil)
			return
		}
		InternalError(w)
		return
	}

	if !s.IsGitHubTriggerCandidate(inst.InstallationID, proj, lowerRepo) {
		writeError(w, http.StatusConflict, ErrCodeNotACandidate,
			"project is not a current candidate for this repository under this installation", nil)
		return
	}

	actor := auditActorFromContext(ctx)
	now := timeNow().UTC()
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		curInst, txErr := tx.GetGitHubInstallation(ctx, req.InstallationID)
		if txErr != nil {
			return txErr
		}
		var beforePrimary string
		if prev, ok := lookupTriggerRoute(curInst.TriggerRouting, lowerRepo); ok {
			beforePrimary = prev.PrimaryProjectID
		}
		if txErr := tx.SetGitHubInstallationTriggerRoute(ctx, req.InstallationID, lowerRepo, store.GitHubTriggerRoute{
			PrimaryProjectID: req.PrimaryProjectID,
			SetBy:            actor.PrincipalID,
			SetAt:            now,
		}); txErr != nil {
			return txErr
		}
		rec := newRepoRoutingAuditRecord(ctx, req.InstallationID, lowerRepo, beforePrimary, req.PrimaryProjectID)
		return tx.CreateMutationAudit(ctx, rec)
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "GitHub installation")
			return
		}
		InternalError(w)
		return
	}

	s.githubAssociationReportCache.invalidateAll()

	updatedInst, err := s.store.GetGitHubInstallation(ctx, req.InstallationID)
	if err != nil {
		InternalError(w)
		return
	}
	assocEntries, err := s.associationEntriesForRouting(ctx)
	if err != nil {
		InternalError(w)
		return
	}
	entries := s.buildRepoRoutingEntries(ctx, []store.GitHubInstallation{*updatedInst}, assocEntries)
	for i := range entries {
		if entries[i].Repo == lowerRepo {
			writeJSON(w, http.StatusOK, entries[i])
			return
		}
	}
	writeJSON(w, http.StatusOK, GitHubRepoRoutingEntry{
		InstallationID:   req.InstallationID,
		Repo:             lowerRepo,
		Projects:         []GitHubRepoRoutingProject{},
		PrimaryProjectID: req.PrimaryProjectID,
		Status:           GitHubTriggerRoutingStatusStalePrimary,
	})
}

// handleDeleteGitHubAppRepoRouting handles DELETE /api/v1/github-app/repo-routing/{owner}/{repo}.
func (s *Server) handleDeleteGitHubAppRepoRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		MethodNotAllowed(w, http.MethodDelete)
		return
	}

	lowerRepo, err := parseOwnerRepoPath(r)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	instFilter, err := parseInstallationQueryFilter(r)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	ctx := r.Context()
	var targetInstIDs []int64
	if instFilter != nil {
		if _, err := s.store.GetGitHubInstallation(ctx, *instFilter); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "GitHub installation")
				return
			}
			InternalError(w)
			return
		}
		targetInstIDs = []int64{*instFilter}
	} else {
		installations, err := s.store.ListGitHubInstallations(ctx, store.GitHubInstallationFilter{})
		if err != nil {
			InternalError(w)
			return
		}
		owner := strings.SplitN(lowerRepo, "/", 2)[0]
		var ownerFallback []int64
		for i := range installations {
			inst := &installations[i]
			if _, ok := lookupTriggerRoute(inst.TriggerRouting, lowerRepo); ok {
				targetInstIDs = append(targetInstIDs, inst.InstallationID)
			} else if strings.EqualFold(inst.AccountLogin, owner) || installationListsRepo(*inst, lowerRepo) {
				ownerFallback = append(ownerFallback, inst.InstallationID)
			}
		}
		if len(targetInstIDs) == 0 {
			targetInstIDs = ownerFallback
		}
	}

	var anyRemoved bool
	if len(targetInstIDs) > 0 {
		err = s.store.WithTx(ctx, func(tx store.Store) error {
			for _, instID := range targetInstIDs {
				curInst, txErr := tx.GetGitHubInstallation(ctx, instID)
				if txErr != nil {
					if errors.Is(txErr, store.ErrNotFound) && instFilter == nil {
						continue
					}
					return txErr
				}
				var beforePrimary string
				if prev, ok := lookupTriggerRoute(curInst.TriggerRouting, lowerRepo); ok {
					beforePrimary = prev.PrimaryProjectID
				}
				removed, txErr := tx.DeleteGitHubInstallationTriggerRoute(ctx, instID, lowerRepo)
				if txErr != nil {
					return txErr
				}
				if !removed {
					continue
				}
				anyRemoved = true
				rec := newRepoRoutingAuditRecord(ctx, instID, lowerRepo, beforePrimary, "")
				if txErr := tx.CreateMutationAudit(ctx, rec); txErr != nil {
					return txErr
				}
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "GitHub installation")
				return
			}
			InternalError(w)
			return
		}
	}

	if anyRemoved {
		s.githubAssociationReportCache.invalidateAll()
	}
	w.WriteHeader(http.StatusNoContent)
}
