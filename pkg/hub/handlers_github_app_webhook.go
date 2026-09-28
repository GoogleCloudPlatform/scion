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
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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

	if s.config.Debug {
		slog.Debug("GitHub webhook received", "event", eventType, "delivery_id", deliveryID)
	}

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

	case "issues":
		s.handleIssuesWebhook(w, r, body)
		return

	case "issue_comment":
		s.handleIssueCommentWebhook(w, r, body)
		return

	case "pull_request_review_comment":
		s.handlePullRequestReviewCommentWebhook(w, r, body)
		return

	case "pull_request_review":
		s.handlePullRequestReviewWebhook(w, r, body)
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

type webhookIssueCommentEvent struct {
	Action string `json:"action"` // created, edited, deleted
	Issue  struct {
		Number      int64  `json:"number"`
		HTMLURL     string `json:"html_url"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type webhookPullRequestReviewCommentEvent struct {
	Action      string `json:"action"` // created, edited, deleted
	PullRequest struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type webhookPullRequestReviewEvent struct {
	Action      string `json:"action"` // submitted, edited, dismissed
	PullRequest struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
	Review struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"review"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type webhookIssuesEvent struct {
	Action string `json:"action"` // opened, edited, etc.
	Issue  struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// handleIssuesWebhook handles "issues" events.
func (s *Server) handleIssuesWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookIssuesEvent
	if err := json.Unmarshal(body, &event); err != nil {
		if s.config.Debug {
			slog.Debug("handleIssuesWebhook: failed to unmarshal", "error", err.Error())
		}
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	if event.Action != "opened" {
		if s.config.Debug {
			slog.Debug("handleIssuesWebhook: ignoring action", "action", event.Action)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "not opened action"})
		return
	}

	if s.config.Debug {
		slog.Debug("handleIssuesWebhook: processing issue", "repo", event.Repository.FullName, "issue", event.Issue.Number)
	}
	s.processComment(r.Context(), "issues", event.Repository.FullName, event.Issue.Number, 0, event.Issue.Body, event.Sender.Login, event.Installation.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleIssueCommentWebhook handles "issue_comment" events.
func (s *Server) handleIssueCommentWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookIssueCommentEvent
	if err := json.Unmarshal(body, &event); err != nil {
		if s.config.Debug {
			slog.Debug("handleIssueCommentWebhook: failed to unmarshal", "error", err)
		}
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	isPlanOrImplementComment := strings.Contains(strings.ToLower(event.Comment.Body), "@scion") &&
		(strings.Contains(strings.ToLower(event.Comment.Body), "/plan") || strings.Contains(strings.ToLower(event.Comment.Body), "/implement"))

	if (event.Issue.PullRequest == nil && !isPlanOrImplementComment) || event.Action != "created" {
		if s.config.Debug {
			slog.Debug("handleIssueCommentWebhook: ignoring event", "action", event.Action, "is_pr", event.Issue.PullRequest != nil, "is_plan_or_implement", isPlanOrImplementComment)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "not a new PR comment or /plan /implement issue comment"})
		return
	}

	if s.config.Debug {
		slog.Debug("handleIssueCommentWebhook: processing comment", "repo", event.Repository.FullName, "pr", event.Issue.Number)
	}
	s.processComment(r.Context(), "issue_comment", event.Repository.FullName, event.Issue.Number, event.Comment.ID, event.Comment.Body, event.Sender.Login, event.Installation.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePullRequestReviewCommentWebhook handles "pull_request_review_comment" events.
func (s *Server) handlePullRequestReviewCommentWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookPullRequestReviewCommentEvent
	if err := json.Unmarshal(body, &event); err != nil {
		if s.config.Debug {
			slog.Debug("handlePullRequestReviewCommentWebhook: failed to unmarshal", "error", err)
		}
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	if event.Action != "created" {
		if s.config.Debug {
			slog.Debug("handlePullRequestReviewCommentWebhook: ignoring event", "action", event.Action)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "non-created PR review comment"})
		return
	}

	if s.config.Debug {
		slog.Debug("handlePullRequestReviewCommentWebhook: processing comment", "repo", event.Repository.FullName, "pr", event.PullRequest.Number)
	}
	s.processComment(r.Context(), "pull_request_review_comment", event.Repository.FullName, event.PullRequest.Number, event.Comment.ID, event.Comment.Body, event.Sender.Login, event.Installation.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePullRequestReviewWebhook handles "pull_request_review" events.
func (s *Server) handlePullRequestReviewWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookPullRequestReviewEvent
	if err := json.Unmarshal(body, &event); err != nil {
		if s.config.Debug {
			slog.Debug("handlePullRequestReviewWebhook: failed to unmarshal", "error", err)
		}
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	if event.Action != "submitted" || event.Review.Body == "" {
		if s.config.Debug {
			slog.Debug("handlePullRequestReviewWebhook: ignoring event", "action", event.Action, "has_body", event.Review.Body != "")
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "review not submitted or empty body"})
		return
	}

	if s.config.Debug {
		slog.Debug("handlePullRequestReviewWebhook: processing comment", "repo", event.Repository.FullName, "pr", event.PullRequest.Number)
	}
	s.processComment(r.Context(), "pull_request_review", event.Repository.FullName, event.PullRequest.Number, event.Review.ID, event.Review.Body, event.Sender.Login, event.Installation.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// parseCommand extracts commands starting with "/" from a comment body.
func parseCommand(body string) string {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "/review") {
		return "/review"
	}
	if strings.Contains(lower, "/validate") {
		return "/validate"
	}
	if strings.Contains(lower, "/fix") {
		return "/fix"
	}
	if strings.Contains(lower, "/plan") {
		return "/plan"
	}
	if strings.Contains(lower, "/implement") {
		return "/implement"
	}
	return ""
}

// extractTextAfterCommand returns the substring of body that comes after the given command.
func extractTextAfterCommand(body, command string) string {
	idx := strings.Index(strings.ToLower(body), command)
	if idx == -1 {
		return ""
	}
	text := body[idx+len(command):]
	return strings.TrimSpace(text)
}

// processComment checks for mentions of "@scion" and resolves corresponding projects.
func (s *Server) processComment(ctx context.Context, eventType, repoFullName string, prNumber, commentID int64, body string, senderLogin string, installationID int64) {
	if !strings.Contains(strings.ToLower(body), "@scion") {
		if s.config.Debug {
			slog.Debug("No @scion mention in comment", "repo", repoFullName, "pr", prNumber, "comment_id", commentID)
		}
		return
	}

	cmd := parseCommand(body)
	if s.config.Debug {
		slog.Debug("processComment: detected @scion mention", "event_type", eventType, "repo", repoFullName, "command", cmd)
	}

	slog.Info("Detected @scion mention in comment!",
		"event_type", eventType,
		"repo", repoFullName,
		"pr", prNumber,
		"comment_id", commentID,
		"sender", senderLogin,
	)

	// Resolve project associated with repo
	projects, err := s.findProjectsForRepository(ctx, repoFullName)
	if err != nil {
		if s.config.Debug {
			slog.Debug("processComment: failed to find projects for repo", "repo", repoFullName, "error", err)
		}
		slog.Error("Failed to find projects for repository", "repo", repoFullName, "error", err)
		return
	}

	if len(projects) == 0 {
		if s.config.Debug {
			slog.Debug("processComment: no projects matched for repo", "repo", repoFullName, "installation_id", installationID)
		}
		slog.Warn("No project matched with the repository", "repo", repoFullName)
		if installationID != 0 {
			client, err := s.getGitHubAppClient()
			if err != nil {
				slog.Error("Failed to get GitHub App client to post unmatched project comment", "error", err)
				return
			}
			parts := strings.SplitN(repoFullName, "/", 2)
			if len(parts) == 2 {
				owner, repo := parts[0], parts[1]
				var errMsg string
				if cmd == "/review" || cmd == "/validate" || cmd == "/fix" {
					errMsg = fmt.Sprintf("No appropriate Scion project/grove could be found to execute your `%s` command.\n\nTo run this command, you must have either:\n1. A **branch-specific project** (a project that is currently serving the PR's target branch), or\n2. A **project with isolated workspaces** available for this repository.", cmd)
				} else {
					errMsg = "No matching project or grove was found in Scion configured with this repository's Git remote. Please ensure the repository is linked to a Scion project."
				}
				if s.config.Debug {
					slog.Debug("processComment: posting unmatched project comment", "owner", owner, "repo", repo)
				}
				if err := s.postPRCommentWithFallback(ctx, client, installationID, owner, repo, prNumber, errMsg); err != nil {
					slog.Error("Failed to post unmatched project comment to GitHub", "repo", repoFullName, "pr", prNumber, "error", err.Error())
				} else {
					slog.Info("Posted unmatched project comment to GitHub", "repo", repoFullName, "pr", prNumber)
				}
			}
		}
		return
	}

	// Try to resolve the head branch first from any available project
	var prBranch string
	for _, p := range projects {
		if p.GitHubInstallationID != nil {
			branch, err := s.fetchPRHeadBranch(ctx, p, repoFullName, prNumber)
			if err == nil && branch != "" {
				prBranch = branch
				break
			}
		}
	}

	// For /review, /validate, and /fix, ensure we have an appropriate project.
	// An appropriate project is either branch-specific (has agents matching prBranch)
	// or is a project with isolated workspaces.
	if cmd == "/review" || cmd == "/validate" || cmd == "/fix" {
		hasAppropriateProject := false

		// 1. Check for branch-specific project (branchMatchCandidates)
		if prBranch != "" {
			for _, proj := range projects {
				agents, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: proj.ID}, store.ListOptions{Limit: 1000})
				if err == nil {
					for _, agent := range agents.Items {
						if agent.AppliedConfig != nil && agent.AppliedConfig.Branch == prBranch {
							hasAppropriateProject = true
							break
						}
					}
				}
				if hasAppropriateProject {
					break
				}
			}
		}

		// 2. Check for project with isolated workspaces
		if !hasAppropriateProject {
			if s.selectByIsolatedWorkspace(projects) != nil {
				hasAppropriateProject = true
			}
		}

		if !hasAppropriateProject {
			slog.Warn("No appropriate project found for command", "command", cmd, "repo", repoFullName, "pr", prNumber)
			if installationID != 0 {
				client, err := s.getGitHubAppClient()
				if err == nil {
					parts := strings.SplitN(repoFullName, "/", 2)
					if len(parts) == 2 {
						owner, repo := parts[0], parts[1]
						guidanceMsg := fmt.Sprintf("No appropriate Scion project/grove could be found to execute your `%s` command.\n\nTo run this command, you must have either:\n1. A **branch-specific project** (a project that is currently serving the branch `%s`), or\n2. A **project with isolated workspaces** available for this repository.", cmd, prBranch)
						if err := s.postPRCommentWithFallback(ctx, client, installationID, owner, repo, prNumber, guidanceMsg); err != nil {
							slog.Error("Failed to post project fallback guidance comment to GitHub", "repo", repoFullName, "pr", prNumber, "error", err.Error())
						} else {
							slog.Info("Posted project fallback guidance comment to GitHub", "repo", repoFullName, "pr", prNumber)
						}
					}
				} else {
					slog.Error("Failed to get GitHub App client to post guidance comment", "error", err.Error())
				}
			}
			return
		}
	}

	// Use our prioritisation helper to select the single best project
	p := s.resolveBestProjectForPR(ctx, projects, prBranch)

	if s.config.Debug {
		slog.Debug("processComment: matched project", "id", p.ID, "name", p.Name, "git_remote", p.GitRemote)
	}
	slog.Info("Matched comment mention to Scion Project",
		"project_id", p.ID,
		"project_name", p.Name,
		"git_remote", p.GitRemote,
	)

	// Resolve the target template based on environment variables or Scion hierarchy
	var targetTemplate string
	if cmd == "/review" || cmd == "/validate" || cmd == "/fix" || cmd == "/plan" || cmd == "/implement" {
		var defaultTemplateFromSettings string
		if settings, _, err := config.LoadEffectiveSettings(""); err == nil && settings != nil {
			defaultTemplateFromSettings = settings.DefaultTemplate
			profileName := settings.ActiveProfile
			if profileName == "" {
				profileName = "local"
			}
			if profile, ok := settings.Profiles[profileName]; ok {
				if profile.DefaultTemplate != "" {
					defaultTemplateFromSettings = profile.DefaultTemplate
				}
			}
		}

		// Load environment variables defined in the Hub database (scope project or hub)
		dbEnv := make(map[string]string)
		if projEnvVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeProject, ScopeID: p.ID}); err == nil {
			for _, ev := range projEnvVars {
				if ev.Value != "" {
					dbEnv[ev.Key] = ev.Value
				}
			}
		}
		if s.hubID != "" {
			if hubEnvVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeHub, ScopeID: s.hubID}); err == nil {
				for _, ev := range hubEnvVars {
					if _, ok := dbEnv[ev.Key]; !ok && ev.Value != "" {
						dbEnv[ev.Key] = ev.Value
					}
				}
			}
		}
		if hubEnvVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeHub, ScopeID: "hub"}); err == nil {
			for _, ev := range hubEnvVars {
				if _, ok := dbEnv[ev.Key]; !ok && ev.Value != "" {
					dbEnv[ev.Key] = ev.Value
				}
			}
		}

		lookup := func(key string) string {
			// 1. Try project-level annotations first
			if p.Annotations != nil {
				if val, ok := p.Annotations[key]; ok && val != "" {
					return val
				}
				lowerKey := strings.ToLower(strings.ReplaceAll(key, "_", "-"))
				if val, ok := p.Annotations[lowerKey]; ok && val != "" {
					return val
				}
			}
			// 2. Try project-level labels
			if p.Labels != nil {
				if val, ok := p.Labels[key]; ok && val != "" {
					return val
				}
				lowerKey := strings.ToLower(strings.ReplaceAll(key, "_", "-"))
				if val, ok := p.Labels[lowerKey]; ok && val != "" {
					return val
				}
			}
			// 3. Try database-defined environment variables (project and hub scope)
			if val, ok := dbEnv[key]; ok && val != "" {
				return val
			}
			// 4. Fall back to process environment variables
			return os.Getenv(key)
		}

		if cmd == "/review" {
			targetTemplate = lookup("SCION_REVIEW_TEMPLATE")
		} else if cmd == "/validate" {
			targetTemplate = lookup("SCION_VALIDATE_TEMPLATE")
		} else if cmd == "/fix" {
			targetTemplate = lookup("SCION_FIX_TEMPLATE")
		} else if cmd == "/plan" {
			targetTemplate = lookup("SCION_PLAN_TEMPLATE")
		} else if cmd == "/implement" {
			targetTemplate = lookup("SCION_IMPLEMENT_TEMPLATE")
		}
		if targetTemplate == "" {
			targetTemplate = defaultTemplateFromSettings
		}
		if targetTemplate == "" {
			targetTemplate = "default"
		}
	}

	// 1. Look for an active (running) agent labeled with this PR (unless command is /review, /validate, or /implement)
	var activeAgent *store.Agent
	if cmd != "/review" && cmd != "/validate" && cmd != "/implement" {
		var err error
		activeAgent, err = s.findActiveAgentForPR(ctx, p.ID, prNumber, repoFullName)
		if err != nil {
			slog.Error("Failed to query active agents for PR", "project", p.ID, "pr", prNumber, "error", err)
			return
		}

		// Only route /fix and /plan to an active agent if its template matches the targetTemplate
		if (cmd == "/fix" || cmd == "/plan") && activeAgent != nil {
			if activeAgent.Template != targetTemplate {
				slog.Info("Active agent template mismatch for command. Spawning a new agent with matching template instead.",
					"command", cmd,
					"active_agent_id", activeAgent.ID,
					"active_agent_template", activeAgent.Template,
					"required_template", targetTemplate,
				)
				activeAgent = nil
			}
		}
	}

	if activeAgent != nil {
		// --- PATH A: Route to existing active agent ---
		slog.Info("Routing GitHub mention to active agent", "agent_id", activeAgent.ID, "pr", prNumber)

		msgText := body
		if cmd == "/fix" {
			fixText := extractTextAfterCommand(body, "/fix")
			if fixText != "" {
				msgText = fixText + " Please also comment in the GitHub PR explaining the changes made for a human reviewer."
			} else {
				msgText = "Please implement a fix as requested and comment in the GitHub PR explaining the changes made for a human reviewer."
			}
			slog.Info("Routing /fix command to active agent in project/grove",
				"command", cmd,
				"project_id", p.ID,
				"project_name", p.Name,
				"agent_id", activeAgent.ID,
				"fix_instruction", msgText,
			)
		} else if cmd == "/plan" {
			planText := extractTextAfterCommand(body, "/plan")
			if planText != "" {
				msgText = fmt.Sprintf("Please refine/revise the plan based on the following feedback: %s", planText)
			} else {
				msgText = "Please refine/revise the plan based on the discussion above."
			}
			slog.Info("Routing /plan command to active agent in project/grove",
				"command", cmd,
				"project_id", p.ID,
				"project_name", p.Name,
				"agent_id", activeAgent.ID,
				"plan_instruction", msgText,
			)
		}

		msg := &messages.StructuredMessage{
			Version:     messages.Version,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Sender:      "user:github-" + senderLogin,
			Recipient:   "agent:" + activeAgent.Slug,
			RecipientID: activeAgent.ID,
			Msg:         msgText,
			Type:        messages.TypeInstruction,
		}

		// Publish to the message broker proxy
		if s.messageBrokerProxy != nil {
			if err := s.messageBrokerProxy.PublishMessage(ctx, p.ID, msg); err != nil {
				slog.Error("Failed to publish PR comment to agent", "agent_id", activeAgent.ID, "error", err)
			}
		} else {
			slog.Info("Message broker proxy not initialized, falling back to direct dispatch", "agent_id", activeAgent.ID)

			// 1. Create and persist message
			storeMsg := &store.Message{
				ID:          api.NewUUID(),
				ProjectID:   p.ID,
				Sender:      msg.Sender,
				SenderID:    msg.SenderID,
				Recipient:   msg.Recipient,
				RecipientID: msg.RecipientID,
				Msg:         msg.Msg,
				Type:        msg.Type,
				AgentID:     activeAgent.ID,
				CreatedAt:   time.Now(),
			}
			if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
				slog.Error("Failed to persist fallback message", "error", err)
			}

			// 2. Publish SSE event
			s.events.PublishUserMessage(ctx, storeMsg)

			// 3. Dispatch to runtime broker via agent dispatcher
			dispatcher := s.GetDispatcher()
			if dispatcher != nil && activeAgent.RuntimeBrokerID != "" {
				if err := dispatcher.DispatchAgentMessage(ctx, activeAgent, msgText, false, msg); err != nil {
					slog.Error("Failed to dispatch PR comment to runtime broker", "agent_id", activeAgent.ID, "error", err)
				} else {
					slog.Info("Successfully dispatched PR comment directly to agent", "agent_id", activeAgent.ID)
				}
			} else {
				slog.Error("Direct dispatch failed: dispatcher or runtime broker ID not available", "agent_id", activeAgent.ID)
			}
		}
	} else {
		// --- PATH B: Spawn a new agent ---
		slog.Info("Spawning new agent in project", "project", p.ID, "pr", prNumber, "command", cmd)

		// Resolve branch ref in a background goroutine so we don't block the webhook response
		go func(proj store.Project, prNum int64, repoFull, sender string, prompt string, command string, template string, installID int64) {
			bgCtx := context.Background()

			// A: Fetch the head branch name from GitHub if we don't already have it
			branch := prBranch
			var err error
			if branch == "" && command != "/plan" && command != "/implement" {
				branch, err = s.fetchPRHeadBranch(bgCtx, proj, repoFull, prNum)
				if err != nil {
					slog.Error("Failed to fetch branch ref from GitHub", "repo", repoFull, "pr", prNum, "error", err)
					return
				}
			}

			taskDesc := prompt
			labels := map[string]string{
				"github-pr":    strconv.FormatInt(prNum, 10),
				"github-issue": strconv.FormatInt(prNum, 10),
				"github-repo":  repoFull,
			}
			if command == "/review" {
				taskDesc = fmt.Sprintf("Perform a complete code review for Pull Request #%d on repository %s. Inspect the changes on branch %s, identify bugs, style issues, or architectural improvements, and post review comments back to GitHub.", prNum, repoFull, branch)
				labels["github-action"] = "review"
			} else if command == "/validate" {
				taskDesc = fmt.Sprintf("Validate the changes for Pull Request #%d on repository %s and report the validation results back to GitHub.", prNum, repoFull)
				labels["github-action"] = "validate"
			} else if command == "/fix" {
				fixText := extractTextAfterCommand(prompt, "/fix")
				if fixText != "" {
					taskDesc = fmt.Sprintf("Implement the following fix for Pull Request #%d on repository %s: %s Please also comment in the GitHub PR explaining the changes made for a human reviewer.", prNum, repoFull, fixText)
				} else {
					taskDesc = fmt.Sprintf("Implement a fix for Pull Request #%d on repository %s and comment in the GitHub PR explaining the changes made for a human reviewer.", prNum, repoFull)
				}
				labels["github-action"] = "fix"
			} else if command == "/plan" {
				planText := extractTextAfterCommand(prompt, "/plan")
				if planText == "" {
					planText = prompt
				}
				taskDesc = fmt.Sprintf("You are working in a local workspace directory that is a checkout of the repository %s. You must ONLY plan the changes requested based on the codebase in this workspace. Do NOT execute or apply any code changes. To allow revising or refining any existing plans based on user feedback, use the GitHub CLI 'gh' or the GitHub API to fetch and inspect the full description of Issue #%d as well as all of its comments. If an existing plan is already present in the comments, incorporate any feedback or requests from the comments and write a revised, complete design and implementation plan, then post it back to GitHub Issue #%d using a comment. The requested planning instruction is: %s", repoFull, prNum, prNum, planText)
				labels["github-action"] = "plan"
			} else if command == "/implement" {
				implementText := extractTextAfterCommand(prompt, "/implement")
				if implementText == "" {
					implementText = prompt
				}
				taskDesc = fmt.Sprintf("You are working in a local workspace directory that is a checkout of the repository %s. Implement the plan referenced in GitHub Issue #%d for this repository. To locate the plan, use the GitHub CLI 'gh' or the GitHub API to fetch and inspect the full description of Issue #%d as well as all of its comments. If the plan has been updated or revised in subsequent comments, make sure to use the most recently posted (latest revised) version of the plan for execution. Once you find and parse the correct plan details, implement them in this workspace. When you are finished and everything is verified, create a new GitHub Pull Request containing your implementation, and make sure to include a detailed summary of the plan, the work done, and a reference back to the original Issue #%d. The requested implementation instructions are: %s", repoFull, prNum, prNum, prNum, implementText)
				labels["github-action"] = "implement"
			}

			if command == "/review" || command == "/validate" || command == "/fix" || command == "/plan" || command == "/implement" {
				slog.Info("Launching agent for command",
					"command", command,
					"project_id", proj.ID,
					"project_name", proj.Name,
					"template_used", template,
				)
			}

			agentName := fmt.Sprintf("pr-%d-agent-%d", prNum, time.Now().UnixNano()/1e6)
			if command == "/plan" {
				agentName = fmt.Sprintf("issue-%d-agent-planner", prNum)
			} else if command == "/implement" {
				agentName = fmt.Sprintf("issue-%d-agent-implementer", prNum)
			}

			// C: Construct a new agent creation request
			req := CreateAgentRequest{
				Name:      agentName,
				ProjectID: proj.ID,
				Branch:    branch,
				Task:      taskDesc,
				Labels:    labels,
				Template:  template,
			}

			// C: Provision and start the agent
			slog.Info("Starting dynamic agent dispatch", "name", req.Name, "branch", req.Branch)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)

			// Ent's created_by column requires a valid RFC 4122 UUID.
			// "6550700c-b26a-54cf-bc7b-ec81c1be486a" is the deterministic UUID v5 for "system:github-webhook".
			createdBy := "6550700c-b26a-54cf-bc7b-ec81c1be486a"
			creatorName := "GitHub Webhook"
			notifySubscriberType := store.SubscriberTypeUser
			notifySubscriberID := ""
			ancestry := []string{"system:github-webhook"}

			webhookUser := NewAuthenticatedUser(createdBy, "webhook@system.scion", creatorName, "admin", "webhook")
			r = r.WithContext(contextWithIdentity(r.Context(), webhookUser))

			s.createAgentInProject(w, r, req, proj.ID, createdBy, creatorName, ancestry, notifySubscriberType, notifySubscriberID)

			if w.Code >= 400 {
				slog.Error("Failed to spawn dynamic agent from webhook", "project_id", proj.ID, "status", w.Code, "body", w.Body.String())
			} else {
				slog.Info("Successfully spawned dynamic agent from webhook", "project_id", proj.ID, "agent_name", req.Name)

				var agentID string
				var createResp CreateAgentResponse
				if err := json.Unmarshal(w.Body.Bytes(), &createResp); err == nil && createResp.Agent != nil {
					agentID = createResp.Agent.ID
				}

				if command == "/implement" || command == "/plan" {
					client, err := s.getGitHubAppClient()
					if err == nil {
						parts := strings.SplitN(repoFull, "/", 2)
						if len(parts) == 2 {
							owner, repo := parts[0], parts[1]
							var confirmMsg string
							if command == "/plan" {
								confirmMsg = fmt.Sprintf("🤖 Scion has successfully received your request and started planning the changes for Issue #%d.", prNum)
							} else {
								confirmMsg = fmt.Sprintf("🤖 Scion has successfully received your request and started implementing the plan for Issue #%d.", prNum)
							}

							if agentID != "" && s.config.HubEndpoint != "" {
								link := fmt.Sprintf("%s/agents/%s", strings.TrimSuffix(s.config.HubEndpoint, "/"), agentID)
								confirmMsg += fmt.Sprintf("\n\nYou can view the agent's progress [here](%s).", link)
							}

							if err := s.postPRCommentWithFallback(bgCtx, client, installID, owner, repo, prNum, confirmMsg); err != nil {
								slog.Error("Failed to post starting confirmation to GitHub", "command", command, "repo", repoFull, "pr", prNum, "error", err)
							} else {
								slog.Info("Posted starting confirmation to GitHub", "command", command, "repo", repoFull, "pr", prNum)
							}
						}
					}
				}
			}
		}(p, prNumber, repoFullName, senderLogin, body, cmd, targetTemplate, installationID)
	}
}

// resolveBestProjectForPR implements a multi-tier prioritisation strategy to select the single best project for a PR comment webhook.
func (s *Server) resolveBestProjectForPR(ctx context.Context, projects []store.Project, prBranch string) store.Project {
	if len(projects) == 0 {
		return store.Project{}
	}

	// Priority 1: Branch Match (Tier 1)
	if prBranch != "" {
		var branchMatchCandidates []store.Project
		for _, p := range projects {
			// Query agents for this project
			agents, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: p.ID}, store.ListOptions{Limit: 1000})
			if err != nil {
				slog.Error("resolveBestProjectForPR: failed to list agents for project", "project_id", p.ID, "error", err)
				continue
			}
			for _, agent := range agents.Items {
				if agent.AppliedConfig != nil && agent.AppliedConfig.Branch == prBranch {
					branchMatchCandidates = append(branchMatchCandidates, p)
					break
				}
			}
		}
		if len(branchMatchCandidates) > 0 {
			if s.config.Debug {
				slog.Debug("resolveBestProjectForPR: found branch match candidates", "branch", prBranch, "count", len(branchMatchCandidates))
			}
			// Prefer isolated workspaces within branch match candidates if there are multiple, otherwise fall back to the first.
			best := s.selectByIsolatedWorkspace(branchMatchCandidates)
			if best != nil {
				return *best
			}
			return branchMatchCandidates[0]
		}
	}

	// Priority 2: Isolated Workspaces (Tier 2)
	best := s.selectByIsolatedWorkspace(projects)
	if best != nil {
		if s.config.Debug {
			slog.Debug("resolveBestProjectForPR: selected project by isolated workspace", "project_id", best.ID, "name", best.Name)
		}
		return *best
	}

	// Priority 3: Fallback (first candidate)
	if s.config.Debug {
		slog.Debug("resolveBestProjectForPR: fallback to first matching project", "project_id", projects[0].ID, "name", projects[0].Name)
	}
	return projects[0]
}

func (s *Server) selectByIsolatedWorkspace(projects []store.Project) *store.Project {
	for i := range projects {
		if !projects[i].IsSharedWorkspace() {
			return &projects[i]
		}
	}
	return nil
}

// findActiveAgentForPR queries the store for running agents matching the PR number and repo labels.
func (s *Server) findActiveAgentForPR(ctx context.Context, projectID string, prNumber int64, repoFullName string) (*store.Agent, error) {
	result, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: projectID, Phase: "running"}, store.ListOptions{Limit: 1000})
	if err != nil {
		return nil, err
	}

	prStr := strconv.FormatInt(prNumber, 10)
	repoLower := strings.ToLower(repoFullName)

	for i := range result.Items {
		agent := &result.Items[i]
		if agent.Labels != nil {
			agentPR := agent.Labels["github-pr"]
			agentRepo := strings.ToLower(agent.Labels["github-repo"])
			if agentPR == prStr && agentRepo == repoLower {
				return agent, nil
			}
		}
	}

	return nil, nil
}

// fetchPRHeadBranch calls the internal getGitHubAppClient to fetch the PR metadata and retrieve the head branch name.
func (s *Server) fetchPRHeadBranch(ctx context.Context, project store.Project, repoFullName string, prNumber int64) (string, error) {
	if project.GitHubInstallationID == nil {
		return "", fmt.Errorf("project %s has no GitHub App installation ID", project.ID)
	}

	client, err := s.getGitHubAppClient()
	if err != nil {
		return "", fmt.Errorf("failed to get GitHub App client: %w", err)
	}

	parts := strings.SplitN(repoFullName, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid repository full name: %s", repoFullName)
	}
	owner, repo := parts[0], parts[1]

	pr, err := client.GetPullRequest(ctx, *project.GitHubInstallationID, owner, repo, prNumber)
	if err != nil {
		return "", err
	}

	if pr.Head.Ref == "" {
		return "", fmt.Errorf("empty head branch ref in pull request response")
	}

	return pr.Head.Ref, nil
}

// findProjectsForRepository looks up projects in the store matching the given repository.
func (s *Server) findProjectsForRepository(ctx context.Context, repoFullName string) ([]store.Project, error) {
	projects, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		return nil, err
	}

	var matched []store.Project
	repoLower := strings.ToLower(repoFullName)
	for _, project := range projects.Items {
		if project.GitRemote == "" {
			continue
		}
		ownerRepo := extractOwnerRepo(project.GitRemote)
		if strings.ToLower(ownerRepo) == repoLower {
			matched = append(matched, project)
		}
	}
	return matched, nil
}

// postPRCommentWithFallback attempts to post a PR comment using the client's PostIssueComment (Issues: write permission).
// If that fails (e.g. because Issues permission is not granted), it falls back to posting a top-level Pull Request Review
// of type COMMENT (PullRequests: write permission, which is always granted to Scion).
func (s *Server) postPRCommentWithFallback(ctx context.Context, client *githubapp.Client, installationID int64, owner, repo string, prNumber int64, body string) error {
	// 1. Try standard Issue Comment
	err := client.PostIssueComment(ctx, installationID, owner, repo, prNumber, body)
	if err == nil {
		return nil
	}

	slog.Info("PostIssueComment failed, attempting fallback to PR review comment",
		"owner", owner,
		"repo", repo,
		"pr", prNumber,
		"error", err.Error(),
	)

	// 2. Mint installation token with PullRequests write permission
	token, mintErr := client.MintInstallationToken(ctx, installationID, []string{repo}, githubapp.TokenPermissions{PullRequests: "write"})
	if mintErr != nil {
		return fmt.Errorf("PostIssueComment failed (%s) and fallback token mint failed: %w", err.Error(), mintErr)
	}

	// 3. Resolve API base URL
	s.mu.RLock()
	apiBaseURL := s.config.GitHubAppConfig.APIBaseURL
	s.mu.RUnlock()
	if apiBaseURL == "" {
		apiBaseURL = "https://api.github.com"
	}
	apiBaseURL = strings.TrimRight(apiBaseURL, "/")

	// 4. Construct PR Review request
	reqBody := map[string]string{
		"body":  body,
		"event": "COMMENT",
	}
	bodyBytes, marshalErr := json.Marshal(reqBody)
	if marshalErr != nil {
		return marshalErr
	}

	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews", apiBaseURL, owner, repo, prNumber)
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyBytes)))
	if reqErr != nil {
		return reqErr
	}

	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, respErr := httpClient.Do(req)
	if respErr != nil {
		return fmt.Errorf("PostIssueComment failed (%s) and fallback HTTP request failed: %w", err.Error(), respErr)
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("failed to read fallback response: %w", readErr)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("PostIssueComment failed (%s) and fallback post PR review comment failed (status %d): %s", err.Error(), resp.StatusCode, string(respBody))
	}

	return nil
}
