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
	Configured      bool   `json:"configured"`
	Slug            string `json:"slug"`
	WebHost         string `json:"web_host"`
	InstallationURL string `json:"installation_url,omitempty"`
}

// ProjectGitHubRemoteInfo reports the project's parsed GitHub remote.
type ProjectGitHubRemoteInfo struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	OnAppHost bool   `json:"on_app_host"`
}

// ProjectGitHubAssociationInfo reports the project's GitHub App installation
// association and authorization status from bounded/cached evaluation.
type ProjectGitHubAssociationInfo struct {
	InstallationID *int64  `json:"installation_id"`
	AccountLogin   string  `json:"account_login"`
	Basis          *string `json:"basis"`
	WriteAllowed   bool    `json:"write_allowed"`
	BoundBy        string  `json:"bound_by,omitempty"`
	BoundByName    string  `json:"bound_by_name,omitempty"`
	BoundAt        string  `json:"bound_at,omitempty"`
	Authorized     bool    `json:"authorized"`
	Verified       bool    `json:"verified"`
	ErrorCode      string  `json:"error_code"`
	Reason         string  `json:"reason"`
}

// ProjectGitHubPermissionsInfo reports requested, App-granted, and effective
// token permissions plus any caps applied during token minting.
type ProjectGitHubPermissionsInfo struct {
	Mode          string            `json:"mode"`
	AppGrantKnown bool              `json:"app_grant_known"`
	Requested     map[string]string `json:"requested"`
	AppGranted    map[string]string `json:"app_granted"`
	Effective     map[string]string `json:"effective"`
	Capped        []string          `json:"capped"`
	CapReasons    map[string]string `json:"cap_reasons"`
}

// ProjectGitHubCapabilities reports the caller's server-computed capabilities
// on this project's GitHub integration.
type ProjectGitHubCapabilities struct {
	Bind            bool `json:"bind"`
	Unbind          bool `json:"unbind"`
	Check           bool `json:"check"`
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
// It resolves App identity through ensureGitHubAppIdentity (at most one
// GET /app per process lifetime with failure backoff) and association status
// through evaluateGitHubAssociationCached (bounded by the report and access
// caches).
func (s *Server) handleProjectGitHub(w http.ResponseWriter, r *http.Request, project *store.Project) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()

	cfg, keyStatus := s.githubAppPrivateKeyStatus(ctx)
	webHost := githubapp.WebHostForAPIBaseURL(cfg.APIBaseURL)
	configured := cfg.AppID != 0 && keyStatus.hasPrivateKey

	// Resolve App identity through the same bounded path minting and admin
	// GET /github-app use (at most one GET /app per process with backoff).
	var (
		appGranted    map[string]string
		appGrantKnown bool
		slug          = normalizeGitHubMentionHandle(cfg.AppSlug)
	)
	if id, hasID := s.GitHubAppIdentity(ctx); hasID && id != nil {
		appGrantKnown = true
		appGranted = make(map[string]string, len(id.Permissions))
		for k, v := range id.Permissions {
			appGranted[k] = v
		}
		if slug == "" {
			slug = id.Slug
		}
	}

	owner, repo, onAppHost := githubapp.RemoteOwnerRepo(project.GitRemote, webHost)
	assoc := s.resolveProjectGitHubAssociation(ctx, project)
	perms := computeProjectGitHubPermissions(project, assoc, owner, repo, appGranted, appGrantKnown)
	caps := s.computeProjectGitHubCapabilities(r, project)

	appInfo := ProjectGitHubAppInfo{
		Configured: configured,
		Slug:       slug,
		WebHost:    webHost,
	}
	if caps.Discover {
		appInfo.InstallationURL = cfg.InstallationURL
	}

	writeJSON(w, http.StatusOK, ProjectGitHubResponse{
		ProjectID:   project.ID,
		App:         appInfo,
		Remote:      ProjectGitHubRemoteInfo{Owner: owner, Repo: repo, OnAppHost: onAppHost},
		Association: assoc,
		Status:      project.GitHubAppStatus,
		Permissions: perms,
		Can:         caps,
	})
}

// resolveProjectGitHubAssociation builds ProjectGitHubAssociationInfo from the
// shared cached association evaluator.
func (s *Server) resolveProjectGitHubAssociation(ctx context.Context, project *store.Project) ProjectGitHubAssociationInfo {
	eval, _ := s.evaluateGitHubAssociationCached(ctx, project, true)
	info := ProjectGitHubAssociationInfo{
		InstallationID: eval.InstallationID,
		AccountLogin:   eval.AccountLogin,
		WriteAllowed:   eval.WriteAllowed,
		Authorized:     eval.Authorized,
		Verified:       eval.Verified,
		ErrorCode:      eval.ErrorCode,
		Reason:         eval.Reason,
	}
	if eval.Basis != "" {
		basis := eval.Basis
		info.Basis = &basis
	}
	if eval.InstallationID != nil {
		s.populateAdminBindingDetails(ctx, project, *eval.InstallationID, &info)
	}
	return info
}

// populateAdminBindingDetails fills BoundBy, BoundByName, and BoundAt when the
// association basis is admin_binding.
func (s *Server) populateAdminBindingDetails(ctx context.Context, project *store.Project, instID int64, info *ProjectGitHubAssociationInfo) {
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
	if b.BoundBy != "" {
		if u, err := s.store.GetUser(ctx, b.BoundBy); err == nil && u != nil {
			if u.DisplayName != "" {
				info.BoundByName = u.DisplayName
			} else if u.Email != "" {
				info.BoundByName = u.Email
			}
		}
	}
}

func computeProjectGitHubPermissions(
	project *store.Project,
	assoc ProjectGitHubAssociationInfo,
	owner, repo string,
	appGranted map[string]string,
	appGrantKnown bool,
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
		// When default mode is active and App identity is not yet known, do not
		// report false app_grant caps; leave effective/capped empty and let
		// app_grant_known=false explain why.
		if mode == "custom" || appGrantKnown {
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
	}

	return ProjectGitHubPermissionsInfo{
		Mode:          mode,
		AppGrantKnown: appGrantKnown,
		Requested:     requestedMap,
		AppGranted:    appGranted,
		Effective:     effectiveMap,
		Capped:        capped,
		CapReasons:    capReasons,
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
	// project.github.installation.bind and hub.github_app.update admit hub-boundary
	// credentials (and interactive sessions), never project-scoped UATs.
	cred := credentialContextForIdentity(identity)
	hubBoundaryAdmitted := cred.Boundary == nil || cred.Boundary.Kind == "" || cred.Boundary.Kind == BoundaryKindHub

	return ProjectGitHubCapabilities{
		Bind:            canUpdateProject && canHubGitHubAppUpdate && hubBoundaryAdmitted,
		Unbind:          canUpdateProject,
		Check:           canUpdateProject,
		EditPermissions: canUpdateProject,
		EditExtraRepos:  canUpdateProject,
		Discover:        canHubGitHubAppUpdate && hubBoundaryAdmitted,
	}
}
