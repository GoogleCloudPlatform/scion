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
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleGitHubWebhook handles POST /api/v1/webhooks/github.
// This endpoint receives GitHub webhook events for the GitHub App integration.
// It validates the webhook signature using the configured webhook secret and
// processes installation lifecycle events idempotently.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	// Read the raw body for signature verification
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "failed to read request body", nil)
		return
	}

	// Verify webhook signature — check in-memory config first, then secrets backend
	s.mu.RLock()
	webhookSecret := s.config.GitHubAppConfig.WebhookSecret
	s.mu.RUnlock()
	var secretLookupErr error
	if webhookSecret == "" {
		if sec, err := s.loadGitHubAppSecret(r.Context(), GitHubAppSecretWebhookSecret); err == nil {
			webhookSecret = sec
		} else if !errors.Is(err, store.ErrNotFound) {
			secretLookupErr = err
		}
	}

	// SECURITY-GATE: webhook signature — unauthenticated route; reject unless a webhook secret resolves and the signature verifies.
	if webhookSecret == "" {
		if secretLookupErr != nil {
			slog.Warn("GitHub webhook secret lookup failed", "error", secretLookupErr)
		}
		s.githubWebhookNoSecretWarnOnce.Do(func() {
			slog.Warn("GitHub webhook received but no webhook secret is configured; rejecting")
		})
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "GitHub webhook is not configured", nil)
		return
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	if webhookSecret != "" {
		if !githubapp.VerifyWebhookSignature(body, signature, webhookSecret) {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "invalid webhook signature", nil)
			return
		}
	}

	// Parse the event type
	eventType := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")

	slog.Info("GitHub webhook received",
		"event", eventType,
		"delivery_id", deliveryID,
	)

	switch eventType {
	case "ping":
		// GitHub sends a ping event when the webhook is first configured
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return

	case "installation":
		s.handleInstallationWebhook(w, r, body)
		return

	case "installation_repositories":
		s.handleInstallationRepositoriesWebhook(w, r, body)
		return

	default:
		if !s.githubTriggersEnabled() {
			// Ignore unhandled event types unchanged when triggers or github_triggers is off (§4.1 step 0).
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": eventType})
			return
		}
		s.handleGitHubContentWebhook(w, r, eventType, deliveryID, body)
		return
	}
}

// webhookInstallationEvent represents the payload for installation webhook events.
type webhookInstallationEvent struct {
	Action       string `json:"action"` // created, deleted, suspend, unsuspend
	Installation struct {
		ID      int64 `json:"id"`
		AppID   int64 `json:"app_id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		RepositorySelection string `json:"repository_selection"` // all, selected
	} `json:"installation"`
	Repositories []struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Name     string `json:"name"`
	} `json:"repositories"`
}

func (s *Server) handleInstallationWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookInstallationEvent
	if err := json.Unmarshal(body, &event); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	ctx := r.Context()
	installationID := event.Installation.ID

	switch event.Action {
	case "created":
		// Record the installation and match to projects
		repos := make([]string, len(event.Repositories))
		for i, r := range event.Repositories {
			repos[i] = r.FullName
		}

		installation := &store.GitHubInstallation{
			InstallationID: installationID,
			AccountLogin:   event.Installation.Account.Login,
			AccountType:    event.Installation.Account.Type,
			AppID:          event.Installation.AppID,
			Repositories:   repos,
			Status:         store.GitHubInstallationStatusActive,
		}

		if err := s.store.CreateGitHubInstallation(ctx, installation); err != nil {
			// Idempotent — if already exists, just log and continue
			slog.Info("Installation already exists (idempotent)", "installation_id", installationID)
		}

		// Auto-match projects by repo
		s.matchProjectsToInstallation(ctx, installation)

	case "deleted":
		// Mark installation as deleted, update affected projects
		existing, err := s.store.GetGitHubInstallation(ctx, installationID)
		if err != nil {
			slog.Warn("Installation not found for deletion webhook", "installation_id", installationID)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}

		existing.Status = store.GitHubInstallationStatusDeleted
		if err := s.store.UpdateGitHubInstallation(ctx, existing); err != nil {
			slog.Error("Failed to update installation status", "error", err)
		}

		// Set affected projects to error state
		s.updateProjectsForInstallation(ctx, installationID, store.GitHubAppStateError,
			githubapp.ErrCodeInstallationRevoked, "Installation was revoked on GitHub. Reinstall the GitHub App for this org/account.")

	case "suspend":
		existing, err := s.store.GetGitHubInstallation(ctx, installationID)
		if err != nil {
			slog.Warn("Installation not found for suspend webhook", "installation_id", installationID)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}

		existing.Status = store.GitHubInstallationStatusSuspended
		if err := s.store.UpdateGitHubInstallation(ctx, existing); err != nil {
			slog.Error("Failed to update installation status", "error", err)
		}

		s.updateProjectsForInstallation(ctx, installationID, store.GitHubAppStateError,
			githubapp.ErrCodeInstallationSuspended, "Installation is suspended. Contact org admin to unsuspend.")

	case "unsuspend":
		existing, err := s.store.GetGitHubInstallation(ctx, installationID)
		if err != nil {
			slog.Warn("Installation not found for unsuspend webhook", "installation_id", installationID)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}

		existing.Status = store.GitHubInstallationStatusActive
		if err := s.store.UpdateGitHubInstallation(ctx, existing); err != nil {
			slog.Error("Failed to update installation status", "error", err)
		}

		// Clear error state — will be validated on next token mint
		s.updateProjectsForInstallation(ctx, installationID, store.GitHubAppStateUnchecked, "", "")

	case "new_permissions_accepted":
		if s.githubTriggersEnabled() {
			// Refresh cached App identity (events + permissions) and reset
			// degraded/permission_denied projects on this installation back to
			// unchecked so the next mint validates the newly accepted permissions
			// (§10.3, §11 G12).
			if _, err := s.refreshGitHubAppIdentity(ctx); err != nil {
				slog.Warn("Failed to refresh GitHub App identity after new_permissions_accepted",
					"installation_id", installationID, "error", upstreamText(err.Error()))
			}
			s.resetPermissionDeniedProjectsForInstallation(ctx, installationID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// webhookInstallationRepositoriesEvent represents the payload for installation_repositories events.
type webhookInstallationRepositoriesEvent struct {
	Action       string `json:"action"` // added, removed
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	RepositoriesAdded []struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Name     string `json:"name"`
	} `json:"repositories_added"`
	RepositoriesRemoved []struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Name     string `json:"name"`
	} `json:"repositories_removed"`
	RepositorySelection string `json:"repository_selection"`
}

func (s *Server) handleInstallationRepositoriesWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookInstallationRepositoriesEvent
	if err := json.Unmarshal(body, &event); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	ctx := r.Context()
	installationID := event.Installation.ID

	existing, err := s.store.GetGitHubInstallation(ctx, installationID)
	if err != nil {
		slog.Warn("Installation not found for repos webhook", "installation_id", installationID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	switch event.Action {
	case "added":
		// Add new repos to the installation's repo list
		for _, repo := range event.RepositoriesAdded {
			found := false
			for _, existing := range existing.Repositories {
				if existing == repo.FullName {
					found = true
					break
				}
			}
			if !found {
				existing.Repositories = append(existing.Repositories, repo.FullName)
			}
		}
		if err := s.store.UpdateGitHubInstallation(ctx, existing); err != nil {
			slog.Error("Failed to update installation repos", "error", err)
		}

		// Check if any existing projects now match newly added repos
		s.matchProjectsToInstallation(ctx, existing)

	case "removed":
		// Remove repos from the installation's repo list. GitHub owner and
		// repo names are case-insensitive, so the set is keyed by the
		// lowercased owner/repo.
		removedSet := make(map[string]bool)
		for _, repo := range event.RepositoriesRemoved {
			removedSet[strings.ToLower(repo.FullName)] = true
		}

		filtered := existing.Repositories[:0]
		for _, r := range existing.Repositories {
			if !removedSet[strings.ToLower(r)] {
				filtered = append(filtered, r)
			}
		}
		existing.Repositories = filtered
		if err := s.store.UpdateGitHubInstallation(ctx, existing); err != nil {
			slog.Error("Failed to update installation repos", "error", err)
		}

		// Check if any projects using this installation lost their repo
		s.checkProjectsForRemovedRepos(ctx, installationID, removedSet)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleGitHubAppSetup handles GET /github-app/setup.
// This is the post-installation callback URL configured on the GitHub App.
// GitHub redirects here after a user installs or configures the app.
func (s *Server) handleGitHubAppSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	installationIDStr := r.URL.Query().Get("installation_id")
	setupAction := r.URL.Query().Get("setup_action")

	if installationIDStr == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "missing installation_id parameter", nil)
		return
	}

	installationID, err := strconv.ParseInt(installationIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid installation_id", nil)
		return
	}

	ctx := r.Context()

	slog.Info("GitHub App setup callback",
		"installation_id", installationID,
		"setup_action", setupAction,
	)

	// Get the GitHub App client to look up installation details
	client, err := s.getGitHubAppClient()
	if err != nil {
		slog.Error("GitHub App not configured", "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "GitHub App not configured", nil)
		return
	}

	// Fetch installation details from GitHub
	ghInstallation, err := client.GetInstallation(ctx, installationID)
	if err != nil {
		slog.Error("Failed to fetch installation from GitHub", "error", err, "installation_id", installationID)
		writeError(w, http.StatusBadGateway, ErrCodeInternalError, "failed to fetch installation details from GitHub", nil)
		return
	}

	// List repos for this installation
	repos, err := client.ListInstallationRepos(ctx, installationID)
	if err != nil {
		slog.Warn("Failed to list installation repos", "error", upstreamText(err.Error()), "installation_id", installationID)
		// Continue without repos — we can still record the installation
	}

	repoNames := make([]string, len(repos))
	for i, repo := range repos {
		repoNames[i] = repo.FullName
	}

	// Record the installation (idempotent)
	installation := &store.GitHubInstallation{
		InstallationID: installationID,
		AccountLogin:   ghInstallation.Account.Login,
		AccountType:    ghInstallation.Account.Type,
		AppID:          ghInstallation.AppID,
		Repositories:   repoNames,
		Status:         store.GitHubInstallationStatusActive,
	}

	if ghInstallation.SuspendedAt != nil {
		installation.Status = store.GitHubInstallationStatusSuspended
	}

	if err := s.store.CreateGitHubInstallation(ctx, installation); err != nil {
		// Idempotent — update if already exists
		if existing, getErr := s.store.GetGitHubInstallation(ctx, installationID); getErr == nil {
			existing.AccountLogin = installation.AccountLogin
			existing.AccountType = installation.AccountType
			existing.Repositories = installation.Repositories
			existing.Status = installation.Status
			if updateErr := s.store.UpdateGitHubInstallation(ctx, existing); updateErr != nil {
				slog.Error("Failed to update existing installation", "error", updateErr)
			}
		}
	}

	// Auto-match projects
	matchedProjects := s.matchProjectsToInstallation(ctx, installation)

	// Redirect to the GitHub App setup page so the user can see their projects
	// and configure installations. Pass the installation ID for context.
	redirectURL := fmt.Sprintf("/github-app/installed?installation_id=%d", installationID)
	http.Redirect(w, r, redirectURL, http.StatusFound)

	_ = matchedProjects // consumed by matchProjectsToInstallation side effects
}

// handleGitHubAppDiscover handles POST /api/v1/github-app/installations/discover.
// It queries the GitHub API for all installations and syncs them to the store,
// then auto-matches installations to projects.
func (s *Server) handleGitHubAppDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	client, err := s.getGitHubAppClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "GitHub App not configured", nil)
		return
	}

	// List all installations from GitHub
	ghInstallations, err := client.ListInstallations(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, ErrCodeInternalError,
			fmt.Sprintf("failed to list installations from GitHub: %v", err), nil)
		return
	}

	var discovered []map[string]interface{}
	for _, ghInst := range ghInstallations {
		// Try to list repos for each installation
		repos, err := client.ListInstallationRepos(ctx, ghInst.ID)
		if err != nil {
			slog.Warn("Failed to list repos for installation", "installation_id", ghInst.ID, "error", upstreamText(err.Error()))
		}

		repoNames := make([]string, len(repos))
		for i, r := range repos {
			repoNames[i] = r.FullName
		}

		status := store.GitHubInstallationStatusActive
		if ghInst.SuspendedAt != nil {
			status = store.GitHubInstallationStatusSuspended
		}

		installation := &store.GitHubInstallation{
			InstallationID: ghInst.ID,
			AccountLogin:   ghInst.Account.Login,
			AccountType:    ghInst.Account.Type,
			AppID:          ghInst.AppID,
			Repositories:   repoNames,
			Status:         status,
		}

		if err := s.store.CreateGitHubInstallation(ctx, installation); err != nil {
			// Update existing
			if existing, getErr := s.store.GetGitHubInstallation(ctx, ghInst.ID); getErr == nil {
				existing.Repositories = repoNames
				existing.Status = status
				_ = s.store.UpdateGitHubInstallation(ctx, existing)
			}
		}

		matchedProjects := s.matchProjectsToInstallation(ctx, installation)

		discovered = append(discovered, map[string]interface{}{
			"installation_id":  ghInst.ID,
			"account":          ghInst.Account.Login,
			"repositories":     repoNames,
			"matched_projects": matchedProjects,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"installations": discovered,
		"total":         len(discovered),
	})
}

// forEachGitHubAppProject pages through every project in the store via
// ListProjects cursor pagination and calls visit on each project. The store
// caps a single ListProjects page at 1000 rows, so callers must follow
// NextCursor until exhausted rather than relying on a single large Limit.
func (s *Server) forEachGitHubAppProject(ctx context.Context, visit func(project *store.Project)) error {
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:          githubAssociationReportPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return err
		}
		if page == nil {
			break
		}
		for i := range page.Items {
			visit(&page.Items[i])
		}
		if page.NextCursor == "" || page.NextCursor == cursor || len(page.Items) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	return nil
}

// matchProjectsToInstallation finds projects whose git remote matches repos in the
// installation and auto-associates them. Returns the list of matched project IDs.
func (s *Server) matchProjectsToInstallation(ctx context.Context, installation *store.GitHubInstallation) []string {
	if len(installation.Repositories) == 0 {
		return nil
	}

	// Build a set of normalized repo full names (owner/repo) from the installation
	repoSet := make(map[string]bool, len(installation.Repositories))
	for _, r := range installation.Repositories {
		repoSet[strings.ToLower(r)] = true
	}

	// Page through all projects and check their git remote against the installation repos
	var matched []string
	if err := s.forEachGitHubAppProject(ctx, func(project *store.Project) {
		if project.GitRemote == "" {
			return
		}

		// Extract owner/repo from the git remote URL; a remote on any host
		// other than the configured GitHub host yields "".
		ownerRepo := s.githubOwnerRepo(project.GitRemote)
		if ownerRepo == "" {
			return
		}

		if !repoSet[strings.ToLower(ownerRepo)] {
			return
		}

		// Only auto-associate if the project doesn't already have an installation
		if project.GitHubInstallationID != nil {
			return
		}

		// A repo-name match only selects the candidate: associate only an
		// authorized association (host, installation account, and the
		// creator's own GitHub access; see authorizeGitHubAppAssociation).
		if _, authzErr := s.authorizeGitHubAppAssociation(ctx, project, installation.InstallationID); authzErr != nil {
			slog.Info("Not auto-associating project with GitHub App installation: association not authorized",
				"project_id", project.ID, "installation_id", installation.InstallationID, "error", authzErr)
			return
		}

		// Associate the project with this installation
		s.setGitHubInstallationAssociation(project, installation.InstallationID)

		if err := s.store.UpdateProject(ctx, project); err != nil {
			slog.Error("Failed to associate project with installation",
				"project_id", project.ID, "installation_id", installation.InstallationID, "error", err)
			return
		}
		s.invalidateGitHubAssociation(project.ID)
		s.events.PublishProjectUpdated(ctx, project)

		slog.Info("Auto-associated project with GitHub App installation",
			"project_id", project.ID, "project_name", project.Name,
			"installation_id", installation.InstallationID, "account", installation.AccountLogin)
		matched = append(matched, project.ID)
	}); err != nil {
		slog.Error("Failed to list projects for matching", "error", err)
	}

	return matched
}

// updateProjectsForInstallation updates the GitHub App status for all projects
// associated with the given installation.
func (s *Server) updateProjectsForInstallation(ctx context.Context, installationID int64, state, errorCode, errorMessage string) {
	now := timeNow()
	if err := s.forEachGitHubAppProject(ctx, func(project *store.Project) {
		if project.GitHubInstallationID == nil || *project.GitHubInstallationID != installationID {
			return
		}

		// Preserve the existing LastTokenMint before overwriting
		var lastTokenMint *time.Time
		if project.GitHubAppStatus != nil {
			lastTokenMint = project.GitHubAppStatus.LastTokenMint
		}

		project.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:         state,
			ErrorCode:     errorCode,
			ErrorMessage:  errorMessage,
			LastChecked:   now,
			LastTokenMint: lastTokenMint,
		}
		if state == store.GitHubAppStateError {
			project.GitHubAppStatus.LastError = &now
		}

		if err := s.store.UpdateProjectGitHubAppStatus(ctx, project.ID, project.GitHubAppStatus); err != nil {
			slog.ErrorContext(ctx, "Failed to update project GitHub App status",
				"project_id", project.ID, "error", err)
		} else if s.events != nil {
			s.events.PublishProjectUpdated(ctx, project)
		}
	}); err != nil {
		slog.Error("Failed to list projects for status update", "error", err)
	}
}

// resetPermissionDeniedProjectsPageSize is the ListProjects page size used by
// resetPermissionDeniedProjectsForInstallation (a variable so tests can force
// multi-page traversal without inserting thousands of rows).
var resetPermissionDeniedProjectsPageSize = githubAssociationReportPageSize

// resetPermissionDeniedProjectsForInstallation clears degraded or
// permission_denied GitHubAppStatus back to unchecked for projects associated
// with installationID (§10.3). Other error states (for example suspended or
// repo_not_accessible) and active projects are left untouched.
func (s *Server) resetPermissionDeniedProjectsForInstallation(ctx context.Context, installationID int64) {
	pageSize := resetPermissionDeniedProjectsPageSize
	if pageSize <= 0 {
		pageSize = githubAssociationReportPageSize
	}

	now := timeNow()
	cursor := ""
	for {
		page, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:          pageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			slog.Error("Failed to list projects for permission acceptance reset", "error", err)
			return
		}

		for i := range page.Items {
			project := &page.Items[i]
			if project.GitHubInstallationID == nil || *project.GitHubInstallationID != installationID {
				continue
			}
			if project.GitHubAppStatus == nil {
				continue
			}
			if project.GitHubAppStatus.State != store.GitHubAppStateDegraded &&
				project.GitHubAppStatus.ErrorCode != githubapp.ErrCodePermissionDenied {
				continue
			}

			lastTokenMint := project.GitHubAppStatus.LastTokenMint
			project.GitHubAppStatus = &store.GitHubAppProjectStatus{
				State:         store.GitHubAppStateUnchecked,
				LastChecked:   now,
				LastTokenMint: lastTokenMint,
			}

			if err := s.store.UpdateProjectGitHubAppStatus(ctx, project.ID, project.GitHubAppStatus); err != nil {
				slog.ErrorContext(ctx, "Failed to reset project GitHub App status after new_permissions_accepted",
					"project_id", project.ID, "error", err)
				continue
			}
			s.invalidateGitHubAssociation(project.ID)
			if s.events != nil {
				s.events.PublishProjectUpdated(ctx, project)
			}
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
}

// checkProjectsForRemovedRepos checks if any projects using the given installation
// have lost access to their repository. removedRepos is keyed by lowercased
// owner/repo. For each affected project, it marks GitHubAppStatus as
// repo_not_accessible and pauses its GitHub-sourced triggers with
// status_reason = source_deleted (§10.3). Installation trigger_routing entries
// are intentionally left untouched so they surface as stale_primary (§8.4).
func (s *Server) checkProjectsForRemovedRepos(ctx context.Context, installationID int64, removedRepos map[string]bool) {
	if len(removedRepos) == 0 {
		return
	}

	now := timeNow()
	if err := s.forEachGitHubAppProject(ctx, func(project *store.Project) {
		if project.GitHubInstallationID == nil || *project.GitHubInstallationID != installationID {
			return
		}

		if project.GitRemote == "" {
			return
		}

		ownerRepo := s.githubOwnerRepo(project.GitRemote)
		if ownerRepo == "" || !removedRepos[strings.ToLower(ownerRepo)] {
			return
		}

		project.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:        store.GitHubAppStateError,
			ErrorCode:    githubapp.ErrCodeRepoNotAccessible,
			ErrorMessage: "Target repo was removed from the GitHub App installation. Add the repo back to the installation on GitHub.",
			LastChecked:  now,
			LastError:    &now,
		}

		if err := s.store.UpdateProjectGitHubAppStatus(ctx, project.ID, project.GitHubAppStatus); err != nil {
			slog.ErrorContext(ctx, "Failed to update project after repo removal",
				"project_id", project.ID, "error", err)
		} else {
			s.invalidateGitHubAssociation(project.ID)
			if s.events != nil {
				s.events.PublishProjectUpdated(ctx, project)
			}
		}

		s.pauseGitHubTriggersForRemovedRepo(ctx, project, installationID, ownerRepo)
	}); err != nil {
		slog.Error("Failed to list projects for repo removal check", "error", err)
	}
}

// pauseGitHubTriggersForRemovedRepo pauses every active (or reason-less paused)
// GitHub-sourced trigger in project with status_reason = source_deleted and
// writes a mutation audit record for each paused trigger (§10.3). Triggers
// already paused with a system reason (including source_deleted on redelivery)
// are skipped so the operation is idempotent.
func (s *Server) pauseGitHubTriggersForRemovedRepo(ctx context.Context, project *store.Project, installationID int64, ownerRepo string) {
	if project == nil || project.ID == "" {
		return
	}

	// TODO(ptone/scion#8058): release this project's GitHub trigger subject
	// claims with release_reason = "association_lost" (§10.3) once Stage 2's
	// trigger_subject_claims table is merged on main.

	var matches []store.Trigger
	opts := store.ListOptions{Limit: scheduleCreatorPausePageSize, SkipTotalCount: true}
	for {
		page, err := s.store.ListTriggers(ctx, store.TriggerFilter{
			ProjectID:  project.ID,
			SourceKind: store.TriggerSourceKindGitHub,
		}, opts)
		if err != nil {
			slog.Warn("Failed to list GitHub triggers after repo removal",
				"project_id", project.ID, "installation_id", installationID, "error", err)
			return
		}
		for _, tr := range page.Items {
			if tr.Status == store.TriggerStatusActive ||
				(tr.Status == store.TriggerStatusPaused && tr.StatusReason == store.TriggerStatusReasonNone) {
				matches = append(matches, tr)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}

	lowerRepo := strings.ToLower(strings.TrimSpace(ownerRepo))
	paused := 0
	for i, tr := range matches {
		if err := ctx.Err(); err != nil {
			slog.Warn("Stopped pausing GitHub triggers after repo removal: context done",
				"project_id", project.ID, "unpaused", len(matches)-i, "error", err)
			break
		}

		audit := newTriggerAudit(ctx, mutationTypeTriggerPause, tr.ID)
		afterBytes, _ := json.Marshal(map[string]any{
			"project_id":      project.ID,
			"trigger_id":      tr.ID,
			"name":            tr.Name,
			"status":          store.TriggerStatusPaused,
			"status_reason":   store.TriggerStatusReasonSourceDeleted,
			"installation_id": installationID,
			"repo":            lowerRepo,
			"cause":           "installation_repositories.removed",
		})
		audit.AfterSummary = string(afterBytes)

		err := s.store.WithTx(ctx, func(tx store.Store) error {
			if pErr := s.pauseTriggerWithReason(ctx, tx, tr, store.TriggerStatusReasonSourceDeleted); pErr != nil {
				return pErr
			}
			return tx.CreateMutationAudit(ctx, audit)
		})
		switch {
		case err == nil:
			paused++
		case errors.Is(err, store.ErrRevisionConflict), errors.Is(err, store.ErrNotFound):
			// Edited, resumed, or deleted since it was listed.
		default:
			slog.Warn("Failed to pause GitHub trigger after repo removal",
				"project_id", project.ID, "trigger_id", tr.ID, "error", err)
		}
	}
	if paused > 0 {
		slog.Info("Paused GitHub triggers after repository removal from installation",
			"project_id", project.ID,
			"installation_id", installationID,
			"repo", lowerRepo,
			"paused", paused,
			"status_reason", store.TriggerStatusReasonSourceDeleted,
		)
	}
}

// requireGitHubInstallationHasRepo checks that project's repository is still
// present (case-insensitively) in its GitHub App installation's repository
// list, enforcing §10.3 ("un-paused after re-association") when resuming a
// source_deleted GitHub trigger.
func (s *Server) requireGitHubInstallationHasRepo(w http.ResponseWriter, r *http.Request, project *store.Project) bool {
	if project == nil || project.GitHubInstallationID == nil || *project.GitHubInstallationID <= 0 {
		writeError(w, http.StatusConflict, ErrCodeGitHubNotAssociated,
			"project does not have a GitHub App installation", nil)
		return false
	}
	ownerRepo := s.githubOwnerRepo(project.GitRemote)
	if ownerRepo == "" {
		writeError(w, http.StatusConflict, ErrCodeGitHubNotAssociated,
			"project does not have a GitHub repository remote", nil)
		return false
	}
	inst, err := s.store.GetGitHubInstallation(r.Context(), *project.GitHubInstallationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, ErrCodeGitHubNotAssociated,
				"project's GitHub App installation was not found", nil)
			return false
		}
		slog.ErrorContext(r.Context(), "Failed to load GitHub installation for trigger resume",
			"project_id", project.ID, "installation_id", *project.GitHubInstallationID, "error", err)
		InternalError(w)
		return false
	}
	for _, repo := range inst.Repositories {
		if strings.EqualFold(repo, ownerRepo) {
			return true
		}
	}
	writeError(w, http.StatusConflict, githubapp.ErrCodeRepoNotAccessible,
		"project repository is not in the GitHub App installation's repository list; re-add the repository to the installation (or discover/sync installations) before resuming this trigger", nil)
	return false
}

// getGitHubAppClient creates a GitHub App client from the server's configuration.
// It resolves the private key from: 1) in-memory config, 2) private key file path,
// 3) secrets backend (hub-scoped GITHUB_APP_PRIVATE_KEY secret).
func (s *Server) getGitHubAppClient() (*githubapp.Client, error) {
	s.mu.RLock()
	cfg := s.config.GitHubAppConfig
	s.mu.RUnlock()

	if cfg.AppID == 0 {
		return nil, fmt.Errorf("github app not configured: missing app_id")
	}

	var keyData []byte
	var keySource string
	var err error

	if cfg.PrivateKey != "" {
		keyData = []byte(cfg.PrivateKey)
		keySource = "in-memory config"
		s.warnGitHubAppKeyOverride(context.Background())
	} else if cfg.PrivateKeyPath != "" {
		keyData, err = os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read private key file %q: %w", cfg.PrivateKeyPath, err)
		}
		keySource = "file:" + cfg.PrivateKeyPath
		s.warnGitHubAppKeyOverride(context.Background())
	} else {
		// Try loading from secrets backend
		keyStr, secretErr := s.loadGitHubAppSecret(context.Background(), GitHubAppSecretPrivateKey)
		if secretErr != nil || keyStr == "" {
			return nil, fmt.Errorf("github app not configured: no private key found (checked in-memory config, file path, and secrets backend)")
		}
		keyData = []byte(keyStr)
		keySource = "secrets backend"
	}

	slog.Debug("Loading GitHub App private key", "app_id", cfg.AppID, "key_source", keySource, "key_bytes", len(keyData))

	return githubapp.NewClient(githubapp.Config{
		AppID:      cfg.AppID,
		PrivateKey: string(keyData),
		APIBaseURL: cfg.APIBaseURL,
	}, keyData)
}

// mintGitHubAppToken mints a fresh GitHub App installation token for a project
// (agent delivery/refresh, status check, resource import, skills). It is
// intentionally uncached so agent tokens retain their full lifetime and
// status checks always contact GitHub.
func (s *Server) mintGitHubAppToken(ctx context.Context, project *store.Project) (string, string, error) {
	return s.mintGitHubAppTokenInternal(ctx, project, nil, false)
}

// MintGitHubAppTokenWithPermissions mints (or returns a cached) repository-scoped
// GitHub App installation token for hub-internal Stage 3 callers (trigger
// enrichment and github.comment) with an explicit permission set (§7.3).
// Association authorization runs on every call; if requested contains any
// "write" permission level under a read-only grant (such as a pull-only creator
// proof), it refuses with ErrGitHubWriteNotPermitted rather than downgrading.
func (s *Server) MintGitHubAppTokenWithPermissions(
	ctx context.Context,
	project *store.Project,
	requested githubapp.TokenPermissions,
) (string, string, error) {
	return s.mintGitHubAppTokenInternal(ctx, project, &requested, true)
}

func (s *Server) mintGitHubAppTokenInternal(
	ctx context.Context,
	project *store.Project,
	requested *githubapp.TokenPermissions,
	useCache bool,
) (string, string, error) {
	if project == nil || project.GitHubInstallationID == nil {
		return "", "", fmt.Errorf("project has no GitHub App installation")
	}

	// Single choke for every token delivery leg (hub-host clone and pull,
	// agent env at create and refresh, the agent token-refresh endpoint,
	// resource import, skills, the status check, trigger enrichment and
	// github.comment): the association must be authorized NOW, whatever was
	// recorded before. See github_app_association_authz.go. A refusal,
	// definitive or inconclusive, mints nothing and is surfaced on the
	// project status.
	grant, authzErr := s.authorizeGitHubAppAssociation(ctx, project, *project.GitHubInstallationID)
	if authzErr != nil {
		slog.WarnContext(ctx, "GitHub App token refused: installation association not authorized",
			"project_id", project.ID, "installation_id", *project.GitHubInstallationID, "error", authzErr)
		errCode := githubAssociationErrorCode(authzErr)
		s.updateProjectGitHubAppStatus(ctx, project, store.GitHubAppStateError,
			errCode, githubAppStatusMessage(errCode))
		return "", "", authzErr
	}

	var perms githubapp.TokenPermissions
	var usedDefaults bool
	var appPermissions map[string]string
	if requested != nil {
		var permErr error
		perms, permErr = scopeRequestedGitHubTokenPermissions(*requested, project.GitHubPermissions, grant)
		if permErr != nil {
			return "", "", permErr
		}
	} else if hasUsableStoredGitHubPermissions(project.GitHubPermissions) {
		// Explicit stored project permissions are requested unchanged (still
		// capped by the association's proof).
		perms, _, _ = githubTokenPermissionsForGrant(project.GitHubPermissions, grant, nil)
	} else {
		// No usable stored permissions: intersect DefaultTokenPermissions with
		// the permissions actually granted to the GitHub App so read-only and
		// git-only Apps can mint scoped tokens without exceeding their grant.
		// Fail closed and visibly if the App's permissions are unavailable or
		// have no overlap with the defaults.
		id, hasID := s.GitHubAppIdentity(ctx)
		if !hasID || id == nil {
			const msg = "Unable to verify GitHub App permissions. See the hub log for details."
			slog.WarnContext(ctx, "GitHub App token refused: app permissions unavailable",
				"project_id", project.ID, "installation_id", *project.GitHubInstallationID)
			s.updateProjectGitHubAppStatus(ctx, project, store.GitHubAppStateError,
				githubapp.ErrCodeTokenMintFailed, msg)
			return "", "", &githubapp.TokenMintError{
				ErrorCode: githubapp.ErrCodeTokenMintFailed,
				Message:   msg,
			}
		}
		var ok bool
		perms, usedDefaults, ok = githubTokenPermissionsForGrant(project.GitHubPermissions, grant, id.Permissions)
		if !ok {
			const msg = "GitHub App does not have any of the default permissions required for this project. Update the GitHub App's permissions or configure project token permissions."
			slog.WarnContext(ctx, "GitHub App token refused: default permissions intersection with app grant is empty",
				"project_id", project.ID, "installation_id", *project.GitHubInstallationID)
			s.updateProjectGitHubAppStatus(ctx, project, store.GitHubAppStateDegraded,
				githubapp.ErrCodePermissionDenied, msg)
			return "", "", &githubapp.TokenMintError{
				ErrorCode: githubapp.ErrCodePermissionDenied,
				Message:   msg,
			}
		}
		appPermissions = id.Permissions
	}

	installationID := *project.GitHubInstallationID
	ownerRepo := grant.Owner + "/" + grant.Repo
	appID := s.githubAppConfiguredID()
	apiBase := s.githubAppAPIBase()
	permKey := canonicalGitHubTokenPermKey(perms)

	if useCache {
		if cachedToken, cachedExp, ok := s.githubTokenCache.get(project.ID, permKey, appID, apiBase, installationID, ownerRepo); ok {
			return cachedToken, cachedExp.UTC().Format("2006-01-02T15:04:05Z"), nil
		}
	}

	client, err := s.getGitHubAppClient()
	if err != nil {
		// Classify the client-creation error: it could be a missing app_id,
		// a missing/corrupt private key, or a file-read failure.
		errorCode := githubapp.ErrCodeTokenMintFailed
		if mintErr, ok := err.(*githubapp.TokenMintError); ok {
			errorCode = mintErr.ErrorCode
		} else if strings.Contains(err.Error(), "private key") || strings.Contains(err.Error(), "PEM") {
			errorCode = githubapp.ErrCodePrivateKeyInvalid
		}
		// The stored status is shown to users: fixed text for the code; the
		// error itself reaches only the log, rendered (ptone/scion#7498).
		slog.ErrorContext(ctx, "GitHub App client unavailable", "project_id", project.ID,
			"error_code", errorCode, "error", upstreamText(err.Error()))
		s.updateProjectGitHubAppStatus(ctx, project, store.GitHubAppStateError,
			errorCode, githubAppStatusMessage(errorCode))
		return "", "", err
	}

	// Always repository-scoped, never installation-wide: the API takes the
	// bare repo name, and the coverage check above pinned the installation
	// account to the remote owner.
	repos := []string{grant.Repo}

	token, err := client.MintInstallationToken(ctx, installationID, repos, perms)
	if err != nil {
		// Classify the error and update project status
		var mintErr *githubapp.TokenMintError
		errorCode := githubapp.ErrCodeTokenMintFailed
		errorMessage := githubAppStatusMessage(errorCode)
		upstream := ""
		if ok := isTokenMintError(err, &mintErr); ok {
			errorCode = mintErr.ErrorCode
			errorMessage = mintErr.Message
			upstream = mintErr.UpstreamDetail()
		}
		// The stored status is shown to users: Message is fixed per code;
		// GitHub's text and the error reach only the log, rendered
		// (ptone/scion#7498).
		slog.ErrorContext(ctx, "GitHub App token mint failed", "project_id", project.ID,
			"error_code", errorCode, "error", upstreamText(err.Error()), "upstream", upstreamText(upstream))

		state := store.GitHubAppStateError
		if errorCode == githubapp.ErrCodePermissionDenied {
			state = store.GitHubAppStateDegraded
		}

		s.updateProjectGitHubAppStatus(ctx, project, state, errorCode, errorMessage)
		return "", "", err
	}

	if useCache {
		s.githubTokenCache.put(project.ID, permKey, appID, apiBase, installationID, ownerRepo, token.Token, token.ExpiresAt)
	}

	// Cache rate limit info
	if rl := client.GetRateLimit(); rl != nil {
		s.mu.Lock()
		s.githubAppRateLimit = rl
		s.mu.Unlock()
	}

	// Success — update project status. If default permissions were narrowed
	// because the App lacks part of DefaultTokenPermissions, mark the project
	// degraded rather than silently dropping permissions.
	now := timeNow()
	if usedDefaults {
		defaults := store.GitHubTokenPermissions(githubapp.DefaultTokenPermissions())
		if missingPerms := comparePermissions(&defaults, appPermissions); len(missingPerms) > 0 {
			project.GitHubAppStatus = &store.GitHubAppProjectStatus{
				State:         store.GitHubAppStateDegraded,
				ErrorCode:     githubapp.ErrCodePermissionDenied,
				ErrorMessage:  formatMissingPermissionsMessage(missingPerms),
				LastTokenMint: &now,
				LastChecked:   now,
				LastError:     &now,
			}
		} else {
			project.GitHubAppStatus = &store.GitHubAppProjectStatus{
				State:         store.GitHubAppStateOK,
				LastTokenMint: &now,
				LastChecked:   now,
			}
		}
	} else {
		project.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:         store.GitHubAppStateOK,
			LastTokenMint: &now,
			LastChecked:   now,
		}
	}
	if err := s.store.UpdateProjectGitHubAppStatus(ctx, project.ID, project.GitHubAppStatus); err != nil {
		slog.WarnContext(ctx, "Failed to update project status after successful token mint", "error", err)
	} else if s.events != nil {
		s.events.PublishProjectUpdated(ctx, project)
	}

	return token.Token, token.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"), nil
}

// updateProjectGitHubAppStatus is a helper to update a project's GitHub App status
// via a narrow status-only store write so concurrent project updates are never reverted.
func (s *Server) updateProjectGitHubAppStatus(ctx context.Context, project *store.Project, state, errorCode, errorMessage string) {
	now := timeNow()
	project.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:        state,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		LastChecked:  now,
		LastError:    &now,
	}
	if err := s.store.UpdateProjectGitHubAppStatus(ctx, project.ID, project.GitHubAppStatus); err != nil {
		slog.WarnContext(ctx, "Failed to update project GitHub App status", "project_id", project.ID, "error", err)
	} else if s.events != nil {
		s.events.PublishProjectUpdated(ctx, project)
	}
}

// githubAppStatusMessage is the fixed project-status text stored for a
// GitHub App failure that is not a TokenMintError (whose Message is already
// fixed). The status is shown to users, so it never carries error or
// upstream text (ptone/scion#7498).
func githubAppStatusMessage(errorCode string) string {
	switch errorCode {
	case githubapp.ErrCodePrivateKeyInvalid:
		return "The GitHub App private key is missing or invalid. Re-upload it in Admin > Server Config."
	case githubapp.ErrCodeAppNotFound:
		return "GitHub App not found — verify the App ID in Admin > Server Config > GitHub App."
	case githubAppErrAssociationUnauthorized:
		return "The project's GitHub App installation association is not authorized. Re-bind or re-verify the installation."
	case githubAppErrAssociationCheckFailed:
		return "Could not verify the project's GitHub App installation association. See the hub log for details."
	case githubapp.ErrCodePermissionDenied:
		return "The GitHub App does not have the required permissions for this repository. Update the App's permissions on GitHub."
	default:
		return "The GitHub App could not provide a token. See the hub log for details."
	}
}

// isTokenMintError checks if the error is a TokenMintError and assigns it.
func isTokenMintError(err error, target **githubapp.TokenMintError) bool {
	if tme, ok := err.(*githubapp.TokenMintError); ok {
		*target = tme
		return true
	}
	return false
}

// MintGitHubAppTokenForProject implements GitHubAppTokenMinter.
// It mints a GitHub App installation token for the given project.
func (s *Server) MintGitHubAppTokenForProject(ctx context.Context, project *store.Project) (string, string, error) {
	if project.GitHubInstallationID == nil {
		return "", "", nil
	}

	// Check if the app is configured
	s.mu.RLock()
	appConfigured := s.config.GitHubAppConfig.AppID != 0
	s.mu.RUnlock()

	if !appConfigured {
		return "", "", nil
	}

	return s.mintGitHubAppToken(ctx, project)
}
