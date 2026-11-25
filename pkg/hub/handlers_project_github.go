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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ProjectGitHubResponse is the read model for GET /api/v1/projects/{id}/github.
type ProjectGitHubResponse struct {
	ProjectID   string                        `json:"project_id"`
	App         ProjectGitHubAppInfo          `json:"app"`
	Remote      ProjectGitHubRemoteInfo       `json:"remote"`
	Association ProjectGitHubAssociationInfo  `json:"association"`
	Status      *store.GitHubAppProjectStatus `json:"status"`
	Permissions ProjectGitHubPermissionsInfo  `json:"permissions"`
	Can         ProjectGitHubCapabilities     `json:"can"`
}

// ProjectGitHubAppInfo reports non-sensitive GitHub App configuration state.
type ProjectGitHubAppInfo struct {
	Configured bool   `json:"configured"`
	Slug       string `json:"slug"`
	WebHost    string `json:"web_host"`
}

// ProjectGitHubRemoteInfo reports the project's parsed GitHub remote.
type ProjectGitHubRemoteInfo struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	OnAppHost bool   `json:"on_app_host"`
}

// ProjectGitHubAssociationInfo reports the project's GitHub App installation
// association and authorization status from cached/local state.
type ProjectGitHubAssociationInfo struct {
	InstallationID *int64  `json:"installation_id"`
	AccountLogin   string  `json:"account_login"`
	Basis          *string `json:"basis"`
	WriteAllowed   bool    `json:"write_allowed"`
	BoundBy        string  `json:"bound_by,omitempty"`
	BoundAt        string  `json:"bound_at,omitempty"`
	Authorized     bool    `json:"authorized"`
	ErrorCode      string  `json:"error_code"`
	Reason         string  `json:"reason"`
}

// ProjectGitHubPermissionsInfo reports requested, App-granted, and effective
// token permissions plus any caps applied during token minting.
type ProjectGitHubPermissionsInfo struct {
	Mode       string            `json:"mode"`
	Requested  map[string]string `json:"requested"`
	AppGranted map[string]string `json:"app_granted"`
	Effective  map[string]string `json:"effective"`
	Capped     []string          `json:"capped"`
	CapReasons map[string]string `json:"cap_reasons"`
}

// ProjectGitHubCapabilities reports the caller's server-computed capabilities
// on this project's GitHub integration.
type ProjectGitHubCapabilities struct {
	Bind            bool `json:"bind"`
	EditPermissions bool `json:"edit_permissions"`
	EditExtraRepos  bool `json:"edit_extra_repos"`
	Discover        bool `json:"discover"`
}

// projectGitHubPermissionKeys defines the canonical order of the six known
// GitHub App installation token permissions.
var projectGitHubPermissionKeys = []string{
	"contents",
	"pull_requests",
	"issues",
	"metadata",
	"checks",
	"actions",
}

// handleProjectGitHub handles GET /api/v1/projects/{id}/github.
// It returns a read-only overview of the project's GitHub App integration
// without making any outbound GitHub API calls.
func (s *Server) handleProjectGitHub(w http.ResponseWriter, r *http.Request, project *store.Project) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()

	cfg, keyStatus := s.githubAppPrivateKeyStatus(ctx)
	webHost := githubapp.WebHostForAPIBaseURL(cfg.APIBaseURL)
	slug, _, _ := s.peekGitHubAppMentionIdentities()

	appGranted := make(map[string]string)
	if cfg.AppID > 0 {
		if id, _, _ := s.githubAppIdentity.snapshot(cfg.AppID); id != nil {
			for k, v := range id.Permissions {
				appGranted[k] = v
			}
		}
	}

	owner, repo, onAppHost := githubapp.RemoteOwnerRepo(project.GitRemote, webHost)
	assoc := s.resolveProjectGitHubAssociationCached(ctx, project, webHost, owner, repo, onAppHost)
	perms := computeProjectGitHubPermissions(project, assoc, owner, repo, appGranted)
	caps := s.computeProjectGitHubCapabilities(r, project)

	writeJSON(w, http.StatusOK, ProjectGitHubResponse{
		ProjectID: project.ID,
		App: ProjectGitHubAppInfo{
			Configured: cfg.AppID != 0 && keyStatus.hasPrivateKey,
			Slug:       slug,
			WebHost:    webHost,
		},
		Remote: ProjectGitHubRemoteInfo{
			Owner:     owner,
			Repo:      repo,
			OnAppHost: onAppHost,
		},
		Association: assoc,
		Status:      project.GitHubAppStatus,
		Permissions: perms,
		Can:         caps,
	})
}

// resolveProjectGitHubAssociationCached resolves association status from the
// cached association report entry and local/in-memory state only, never making
// an outbound GitHub API call.
func (s *Server) resolveProjectGitHubAssociationCached(
	ctx context.Context,
	project *store.Project,
	webHost, owner, repo string,
	onAppHost bool,
) ProjectGitHubAssociationInfo {
	if project == nil || project.GitHubInstallationID == nil {
		return ProjectGitHubAssociationInfo{}
	}

	instID := *project.GitHubInstallationID
	info := ProjectGitHubAssociationInfo{
		InstallationID: &instID,
	}

	inst, instErr := s.store.GetGitHubInstallation(ctx, instID)
	if instErr == nil && inst != nil {
		info.AccountLogin = inst.AccountLogin
	}

	// Prefer a fresh, non-dirty entry from the association report cache.
	if entry, ok := s.githubAssociationReportCache.lookupFreshProject(project.ID, instID, timeNow()); ok {
		info.Authorized = entry.Authorized
		info.WriteAllowed = entry.WriteAllowed
		info.ErrorCode = entry.ErrorCode
		info.Reason = entry.Reason
		if entry.Basis != "" {
			basis := entry.Basis
			info.Basis = &basis
		}
		populateAdminBindingDetails(project, instID, &info)
		return info
	}

	// Report cache is cold or dirty for this project: evaluate local checks and
	// in-memory access cache without making any outbound GitHub HTTP call.
	if !onAppHost {
		info.ErrorCode = githubAppErrAssociationUnauthorized
		info.Reason = fmt.Sprintf("the project's git remote is not an owner/repo on the configured GitHub host %q", webHost)
		return info
	}

	if instErr != nil {
		if errors.Is(instErr, store.ErrNotFound) {
			info.ErrorCode = githubAppErrAssociationUnauthorized
			info.Reason = fmt.Sprintf("installation %d not found", instID)
		} else {
			info.ErrorCode = githubAppErrAssociationCheckFailed
			info.Reason = "installation lookup failed"
		}
		return info
	}
	if inst == nil || inst.Status != store.GitHubInstallationStatusActive {
		status := "missing"
		if inst != nil {
			status = inst.Status
		}
		info.ErrorCode = githubAppErrAssociationUnauthorized
		info.Reason = fmt.Sprintf("installation %d is %s", instID, status)
		return info
	}
	if inst.AccountLogin == "" || !strings.EqualFold(inst.AccountLogin, owner) {
		info.ErrorCode = githubAppErrAssociationUnauthorized
		info.Reason = fmt.Sprintf("installation %d belongs to account %q, not the remote owner %q", instID, inst.AccountLogin, owner)
		return info
	}

	bound, err := s.githubAdminBindingValid(ctx, project, instID)
	if err != nil {
		info.ErrorCode = githubAppErrAssociationCheckFailed
		info.Reason = "binding admin lookup failed"
		return info
	}
	if bound {
		basis := githubAssociationBasisAdminBinding
		info.Authorized = true
		info.Basis = &basis
		info.WriteAllowed = true
		populateAdminBindingDetails(project, instID, &info)
		return info
	}

	creatorToken, hasToken, err := s.lookupCreatorGitHubToken(ctx, project)
	if err != nil {
		info.ErrorCode = githubAppErrAssociationCheckFailed
		info.Reason = "creator GitHub token lookup failed"
		return info
	}
	if !hasToken {
		info.ErrorCode = githubAppErrAssociationUnauthorized
		info.Reason = "no admin binding and the project creator has no GitHub token to verify repository access"
		return info
	}

	key := githubAccessCacheKey(project.ID, instID, s.githubAppAPIBase(), owner, repo, project.CreatedBy, creatorToken)
	if access, denied, cached := s.githubAccessCache.get(key); cached {
		switch {
		case denied:
			info.ErrorCode = githubAppErrAssociationUnauthorized
			info.Reason = fmt.Sprintf("the project creator's GitHub identity has no access to %s/%s", owner, repo)
		case access.Push:
			basis := githubAssociationBasisCreatorPush
			info.Authorized = true
			info.Basis = &basis
			info.WriteAllowed = true
		case access.Pull:
			basis := githubAssociationBasisCreatorPull
			info.Authorized = true
			info.Basis = &basis
			info.WriteAllowed = false
		default:
			info.ErrorCode = githubAppErrAssociationUnauthorized
			info.Reason = fmt.Sprintf("the project creator's GitHub identity has no access to %s/%s", owner, repo)
		}
		return info
	}

	if project.GitHubAppStatus != nil &&
		project.GitHubAppStatus.State == store.GitHubAppStateError &&
		strings.HasPrefix(project.GitHubAppStatus.ErrorCode, "association_") {
		info.ErrorCode = project.GitHubAppStatus.ErrorCode
		info.Reason = project.GitHubAppStatus.ErrorMessage
		return info
	}

	info.Reason = "creator repository access has not been verified yet"
	return info
}

// populateAdminBindingDetails fills BoundBy and BoundAt when the association
// basis is admin_binding.
func populateAdminBindingDetails(project *store.Project, instID int64, info *ProjectGitHubAssociationInfo) {
	if info.Basis == nil || *info.Basis != githubAssociationBasisAdminBinding {
		return
	}
	raw := project.Annotations[githubInstallationBindingAnnotation]
	if raw == "" {
		return
	}
	var b githubInstallationBinding
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return
	}
	if b.InstallationID != instID {
		return
	}
	info.BoundBy = b.BoundBy
	if !b.BoundAt.IsZero() {
		info.BoundAt = b.BoundAt.UTC().Format(time.RFC3339)
	}
}

func computeProjectGitHubPermissions(
	project *store.Project,
	assoc ProjectGitHubAssociationInfo,
	owner, repo string,
	appGranted map[string]string,
) ProjectGitHubPermissionsInfo {
	mode := "default"
	var requestedPerms githubapp.TokenPermissions
	if project != nil && hasUsableStoredGitHubPermissions(project.GitHubPermissions) {
		mode = "custom"
		requestedPerms = githubapp.TokenPermissions(*project.GitHubPermissions)
	} else {
		requestedPerms = githubapp.DefaultTokenPermissions()
	}

	requestedMap := githubapp.TokenPermissionsToMap(requestedPerms)
	effectiveMap := make(map[string]string)
	capped := make([]string, 0)
	capReasons := make(map[string]string)

	if assoc.Authorized {
		basis := ""
		if assoc.Basis != nil {
			basis = *assoc.Basis
		}
		grant := githubAssociationGrant{
			Owner:        owner,
			Repo:         repo,
			WriteAllowed: assoc.WriteAllowed,
			Basis:        basis,
		}
		effPerms, usedDefaults, ok := effectiveGitHubTokenPermissions(project, grant, appGranted)
		if ok {
			effectiveMap = githubapp.TokenPermissionsToMap(effPerms)
		}

		for _, key := range projectGitHubPermissionKeys {
			reqLevel := requestedMap[key]
			if reqLevel == "" {
				continue
			}
			effLevel := effectiveMap[key]
			if githubPermLevelRank(effLevel) < githubPermLevelRank(reqLevel) {
				capped = append(capped, key)
				if usedDefaults {
					if githubPermLevelRank(appGranted[key]) < githubPermLevelRank(reqLevel) {
						capReasons[key] = "app_grant"
					} else if !grant.WriteAllowed {
						capReasons[key] = "association_read_only"
					} else {
						capReasons[key] = "app_grant"
					}
				} else {
					capReasons[key] = "association_read_only"
				}
			}
		}
	}

	return ProjectGitHubPermissionsInfo{
		Mode:       mode,
		Requested:  requestedMap,
		AppGranted: appGranted,
		Effective:  effectiveMap,
		Capped:     capped,
		CapReasons: capReasons,
	}
}

func githubPermLevelRank(level string) int {
	switch level {
	case "read":
		return 1
	case "write", "admin":
		return 2
	default:
		return 0
	}
}

func (s *Server) computeProjectGitHubCapabilities(r *http.Request, project *store.Project) ProjectGitHubCapabilities {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil || s.authzService == nil {
		return ProjectGitHubCapabilities{}
	}

	canUpdateProject := s.authzService.CheckAccess(ctx, identity, projectResource(project), ActionUpdate).Allowed
	canHubGitHubAppUpdate := s.routePermissionDecision(r, githubInstallationBindPermission).outcome == routePermissionAllowed

	return ProjectGitHubCapabilities{
		Bind:            canUpdateProject && canHubGitHubAppUpdate,
		EditPermissions: canUpdateProject,
		EditExtraRepos:  canUpdateProject,
		Discover:        canHubGitHubAppUpdate,
	}
}
