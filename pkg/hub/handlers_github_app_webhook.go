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
		MethodNotAllowed(w)
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
	if webhookSecret == "" {
		if sec, err := s.loadGitHubAppSecret(r.Context(), GitHubAppSecretWebhookSecret); err == nil {
			webhookSecret = sec
		}
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
		// Ignore unhandled event types
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": eventType})
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
		// Remove repos from the installation's repo list
		removedSet := make(map[string]bool)
		for _, repo := range event.RepositoriesRemoved {
			removedSet[repo.FullName] = true
		}

		filtered := existing.Repositories[:0]
		for _, r := range existing.Repositories {
			if !removedSet[r] {
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
		MethodNotAllowed(w)
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
		slog.Warn("Failed to list installation repos", "error", err, "installation_id", installationID)
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
		MethodNotAllowed(w)
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
			slog.Warn("Failed to list repos for installation", "installation_id", ghInst.ID, "error", err)
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
			"installation_id": ghInst.ID,
			"account":         ghInst.Account.Login,
			"repositories":    repoNames,
			"matched_projects": matchedProjects,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"installations": discovered,
		"total":         len(discovered),
	})
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

	// List all projects and check their git remote against the installation repos
	projects, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		slog.Error("Failed to list projects for matching", "error", err)
		return nil
	}

	var matched []string
	for _, project := range projects.Items {
		if project.GitRemote == "" {
			continue
		}

		// Extract owner/repo from the git remote URL
		ownerRepo := extractOwnerRepo(project.GitRemote)
		if ownerRepo == "" {
			continue
		}

		if !repoSet[strings.ToLower(ownerRepo)] {
			continue
		}

		// Only auto-associate if the project doesn't already have an installation
		if project.GitHubInstallationID != nil {
			continue
		}

		// Associate the project with this installation
		project.GitHubInstallationID = &installation.InstallationID
		project.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:       store.GitHubAppStateUnchecked,
			LastChecked: timeNow(),
		}

		if err := s.store.UpdateProject(ctx, &project); err != nil {
			slog.Error("Failed to associate project with installation",
				"project_id", project.ID, "installation_id", installation.InstallationID, "error", err)
			continue
		}
		s.events.PublishProjectUpdated(ctx, &project)

		slog.Info("Auto-associated project with GitHub App installation",
			"project_id", project.ID, "project_name", project.Name,
			"installation_id", installation.InstallationID, "account", installation.AccountLogin)
		matched = append(matched, project.ID)
	}

	return matched
}

// updateProjectsForInstallation updates the GitHub App status for all projects
// associated with the given installation.
func (s *Server) updateProjectsForInstallation(ctx context.Context, installationID int64, state, errorCode, errorMessage string) {
	projects, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		slog.Error("Failed to list projects for status update", "error", err)
		return
	}

	now := timeNow()
	for _, project := range projects.Items {
		if project.GitHubInstallationID == nil || *project.GitHubInstallationID != installationID {
			continue
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

		if err := s.store.UpdateProject(ctx, &project); err != nil {
			slog.Error("Failed to update project GitHub App status",
				"project_id", project.ID, "error", err)
		} else {
			s.events.PublishProjectUpdated(ctx, &project)
		}
	}
}

// checkProjectsForRemovedRepos checks if any projects using the given installation
// have lost access to their repository.
func (s *Server) checkProjectsForRemovedRepos(ctx context.Context, installationID int64, removedRepos map[string]bool) {
	projects, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		slog.Error("Failed to list projects for repo removal check", "error", err)
		return
	}

	now := timeNow()
	for _, project := range projects.Items {
		if project.GitHubInstallationID == nil || *project.GitHubInstallationID != installationID {
			continue
		}

		if project.GitRemote == "" {
			continue
		}

		ownerRepo := extractOwnerRepo(project.GitRemote)
		if ownerRepo == "" || !removedRepos[ownerRepo] {
			continue
		}

		project.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:        store.GitHubAppStateError,
			ErrorCode:    githubapp.ErrCodeRepoNotAccessible,
			ErrorMessage: "Target repo was removed from the GitHub App installation. Add the repo back to the installation on GitHub.",
			LastChecked:  now,
			LastError:    &now,
		}

		if err := s.store.UpdateProject(ctx, &project); err != nil {
			slog.Error("Failed to update project after repo removal",
				"project_id", project.ID, "error", err)
		} else {
			s.events.PublishProjectUpdated(ctx, &project)
		}
	}
}

// extractOwnerRepo extracts the "owner/repo" from a git remote URL.
// Supports HTTPS, SSH, and shorthand formats:
//   - https://github.com/owner/repo.git → owner/repo
//   - git@github.com:owner/repo.git → owner/repo
//   - github.com/owner/repo → owner/repo
func extractOwnerRepo(remote string) string {
	remote = strings.TrimSpace(remote)

	// Handle SSH format: git@github.com:owner/repo.git
	if strings.Contains(remote, ":") && strings.Contains(remote, "@") {
		parts := strings.SplitN(remote, ":", 2)
		if len(parts) == 2 {
			path := strings.TrimSuffix(parts[1], ".git")
			path = strings.TrimPrefix(path, "/")
			if isValidOwnerRepo(path) {
				return path
			}
		}
	}

	// Handle HTTPS format: https://github.com/owner/repo.git
	remote = strings.TrimPrefix(remote, "https://")
	remote = strings.TrimPrefix(remote, "http://")

	// Remove host prefix (e.g., "github.com/")
	parts := strings.SplitN(remote, "/", 2)
	if len(parts) < 2 {
		return ""
	}

	// If the first part looks like a hostname, skip it
	if strings.Contains(parts[0], ".") {
		path := strings.TrimSuffix(parts[1], ".git")
		path = strings.TrimSuffix(path, "/")
		if isValidOwnerRepo(path) {
			return path
		}
		return ""
	}

	return ""
}

// isValidOwnerRepo checks if a string is in "owner/repo" format.
func isValidOwnerRepo(s string) bool {
	parts := strings.Split(s, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
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
	} else if cfg.PrivateKeyPath != "" {
		keyData, err = os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read private key file %q: %w", cfg.PrivateKeyPath, err)
		}
		keySource = "file:" + cfg.PrivateKeyPath
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

// mintGitHubAppToken mints a GitHub App installation token for a project.
// It handles error classification and updates the project's GitHub App status.
// Returns the token string and expiry, or an error.
func (s *Server) mintGitHubAppToken(ctx context.Context, project *store.Project) (string, string, error) {
	if project.GitHubInstallationID == nil {
		return "", "", fmt.Errorf("project has no GitHub App installation")
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
		s.updateProjectGitHubAppStatus(ctx, project, store.GitHubAppStateError,
			errorCode, err.Error())
		return "", "", err
	}

	installationID := *project.GitHubInstallationID

	// Determine permissions to request
	perms := githubapp.DefaultTokenPermissions()
	if project.GitHubPermissions != nil {
		perms = githubapp.TokenPermissions{
			Contents:     project.GitHubPermissions.Contents,
			PullRequests: project.GitHubPermissions.PullRequests,
			Issues:       project.GitHubPermissions.Issues,
			Metadata:     project.GitHubPermissions.Metadata,
			Checks:       project.GitHubPermissions.Checks,
			Actions:      project.GitHubPermissions.Actions,
		}
	}

	// Extract repo name from git remote (just the repo name, not owner/repo)
	var repos []string
	if project.GitRemote != "" {
		ownerRepo := extractOwnerRepo(project.GitRemote)
		if ownerRepo != "" {
			// GitHub API expects just the repo name, not owner/repo
			parts := strings.SplitN(ownerRepo, "/", 2)
			if len(parts) == 2 {
				repos = []string{parts[1]}
			}
		}
	}

	token, err := client.MintInstallationToken(ctx, installationID, repos, perms)
	if err != nil {
		// Classify the error and update project status
		var mintErr *githubapp.TokenMintError
		errorCode := githubapp.ErrCodeTokenMintFailed
		errorMessage := err.Error()
		if ok := isTokenMintError(err, &mintErr); ok {
			errorCode = mintErr.ErrorCode
			errorMessage = mintErr.Message
		}

		state := store.GitHubAppStateError
		if errorCode == githubapp.ErrCodePermissionDenied {
			state = store.GitHubAppStateDegraded
		}

		s.updateProjectGitHubAppStatus(ctx, project, state, errorCode, errorMessage)
		return "", "", err
	}

	// Cache rate limit info
	if rl := client.GetRateLimit(); rl != nil {
		s.mu.Lock()
		s.githubAppRateLimit = rl
		s.mu.Unlock()
	}

	// Success — update project status
	now := timeNow()
	project.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:         store.GitHubAppStateOK,
		LastTokenMint: &now,
		LastChecked:   now,
	}
	if err := s.store.UpdateProject(ctx, project); err != nil {
		slog.Warn("Failed to update project status after successful token mint", "error", err)
	} else {
		s.events.PublishProjectUpdated(ctx, project)
	}

	return token.Token, token.ExpiresAt.Format("2006-01-02T15:04:05Z"), nil
}

// updateProjectGitHubAppStatus is a helper to update a project's GitHub App status.
func (s *Server) updateProjectGitHubAppStatus(ctx context.Context, project *store.Project, state, errorCode, errorMessage string) {
	now := timeNow()
	project.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:        state,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		LastChecked:  now,
		LastError:    &now,
	}
	if err := s.store.UpdateProject(ctx, project); err != nil {
		slog.Warn("Failed to update project GitHub App status", "project_id", project.ID, "error", err)
	} else {
		s.events.PublishProjectUpdated(ctx, project)
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
