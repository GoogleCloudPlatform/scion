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
		// Record the installation and match to groves
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

		// Auto-match groves by repo
		s.matchGrovesToInstallation(ctx, installation)

	case "deleted":
		// Mark installation as deleted, update affected groves
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

		// Set affected groves to error state
		s.updateGrovesForInstallation(ctx, installationID, store.GitHubAppStateError,
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

		s.updateGrovesForInstallation(ctx, installationID, store.GitHubAppStateError,
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
		s.updateGrovesForInstallation(ctx, installationID, store.GitHubAppStateUnchecked, "", "")
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

		// Check if any existing groves now match newly added repos
		s.matchGrovesToInstallation(ctx, existing)

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

		// Check if any groves using this installation lost their repo
		s.checkGrovesForRemovedRepos(ctx, installationID, removedSet)
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

	// Auto-match groves
	matchedGroves := s.matchGrovesToInstallation(ctx, installation)

	// Redirect to the GitHub App setup page so the user can see their groves
	// and configure installations. Pass the installation ID for context.
	redirectURL := fmt.Sprintf("/github-app/installed?installation_id=%d", installationID)
	http.Redirect(w, r, redirectURL, http.StatusFound)

	_ = matchedGroves // consumed by matchGrovesToInstallation side effects
}

// handleGitHubAppDiscover handles POST /api/v1/github-app/installations/discover.
// It queries the GitHub API for all installations and syncs them to the store,
// then auto-matches installations to groves.
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

		matchedGroves := s.matchGrovesToInstallation(ctx, installation)

		discovered = append(discovered, map[string]interface{}{
			"installation_id": ghInst.ID,
			"account":         ghInst.Account.Login,
			"repositories":    repoNames,
			"matched_groves":  matchedGroves,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"installations": discovered,
		"total":         len(discovered),
	})
}

// matchGrovesToInstallation finds groves whose git remote matches repos in the
// installation and auto-associates them. Returns the list of matched grove IDs.
func (s *Server) matchGrovesToInstallation(ctx context.Context, installation *store.GitHubInstallation) []string {
	if len(installation.Repositories) == 0 {
		return nil
	}

	// Build a set of normalized repo full names (owner/repo) from the installation
	repoSet := make(map[string]bool, len(installation.Repositories))
	for _, r := range installation.Repositories {
		repoSet[strings.ToLower(r)] = true
	}

	// List all groves and check their git remote against the installation repos
	groves, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		slog.Error("Failed to list groves for matching", "error", err)
		return nil
	}

	var matched []string
	for _, grove := range groves.Items {
		if grove.GitRemote == "" {
			continue
		}

		// Extract owner/repo from the git remote URL
		ownerRepo := extractOwnerRepo(grove.GitRemote)
		if ownerRepo == "" {
			continue
		}

		if !repoSet[strings.ToLower(ownerRepo)] {
			continue
		}

		// Only auto-associate if the grove doesn't already have an installation
		if grove.GitHubInstallationID != nil {
			continue
		}

		// Associate the grove with this installation
		grove.GitHubInstallationID = &installation.InstallationID
		grove.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:       store.GitHubAppStateUnchecked,
			LastChecked: timeNow(),
		}

		if err := s.store.UpdateProject(ctx, &grove); err != nil {
			slog.Error("Failed to associate grove with installation",
				"grove_id", grove.ID, "installation_id", installation.InstallationID, "error", err)
			continue
		}
		s.events.PublishGroveUpdated(ctx, &grove)

		slog.Info("Auto-associated grove with GitHub App installation",
			"grove_id", grove.ID, "grove_name", grove.Name,
			"installation_id", installation.InstallationID, "account", installation.AccountLogin)
		matched = append(matched, grove.ID)
	}

	return matched
}

// updateGrovesForInstallation updates the GitHub App status for all groves
// associated with the given installation.
func (s *Server) updateGrovesForInstallation(ctx context.Context, installationID int64, state, errorCode, errorMessage string) {
	groves, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		slog.Error("Failed to list groves for status update", "error", err)
		return
	}

	now := timeNow()
	for _, grove := range groves.Items {
		if grove.GitHubInstallationID == nil || *grove.GitHubInstallationID != installationID {
			continue
		}

		// Preserve the existing LastTokenMint before overwriting
		var lastTokenMint *time.Time
		if grove.GitHubAppStatus != nil {
			lastTokenMint = grove.GitHubAppStatus.LastTokenMint
		}

		grove.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:         state,
			ErrorCode:     errorCode,
			ErrorMessage:  errorMessage,
			LastChecked:   now,
			LastTokenMint: lastTokenMint,
		}
		if state == store.GitHubAppStateError {
			grove.GitHubAppStatus.LastError = &now
		}

		if err := s.store.UpdateProject(ctx, &grove); err != nil {
			slog.Error("Failed to update grove GitHub App status",
				"grove_id", grove.ID, "error", err)
		} else {
			s.events.PublishGroveUpdated(ctx, &grove)
		}
	}
}

// checkGrovesForRemovedRepos checks if any groves using the given installation
// have lost access to their repository.
func (s *Server) checkGrovesForRemovedRepos(ctx context.Context, installationID int64, removedRepos map[string]bool) {
	groves, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		slog.Error("Failed to list groves for repo removal check", "error", err)
		return
	}

	now := timeNow()
	for _, grove := range groves.Items {
		if grove.GitHubInstallationID == nil || *grove.GitHubInstallationID != installationID {
			continue
		}

		if grove.GitRemote == "" {
			continue
		}

		ownerRepo := extractOwnerRepo(grove.GitRemote)
		if ownerRepo == "" || !removedRepos[ownerRepo] {
			continue
		}

		grove.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:        store.GitHubAppStateError,
			ErrorCode:    githubapp.ErrCodeRepoNotAccessible,
			ErrorMessage: "Target repo was removed from the GitHub App installation. Add the repo back to the installation on GitHub.",
			LastChecked:  now,
			LastError:    &now,
		}

		if err := s.store.UpdateProject(ctx, &grove); err != nil {
			slog.Error("Failed to update grove after repo removal",
				"grove_id", grove.ID, "error", err)
		} else {
			s.events.PublishGroveUpdated(ctx, &grove)
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

// mintGitHubAppToken mints a GitHub App installation token for a grove.
// It handles error classification and updates the grove's GitHub App status.
// Returns the token string and expiry, or an error.
func (s *Server) mintGitHubAppToken(ctx context.Context, grove *store.Project) (string, string, error) {
	if grove.GitHubInstallationID == nil {
		return "", "", fmt.Errorf("grove has no GitHub App installation")
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
		s.updateGroveGitHubAppStatus(ctx, grove, store.GitHubAppStateError,
			errorCode, err.Error())
		return "", "", err
	}

	installationID := *grove.GitHubInstallationID

	// Determine permissions to request
	perms := githubapp.DefaultTokenPermissions()
	if grove.GitHubPermissions != nil {
		perms = githubapp.TokenPermissions{
			Contents:     grove.GitHubPermissions.Contents,
			PullRequests: grove.GitHubPermissions.PullRequests,
			Issues:       grove.GitHubPermissions.Issues,
			Metadata:     grove.GitHubPermissions.Metadata,
			Checks:       grove.GitHubPermissions.Checks,
			Actions:      grove.GitHubPermissions.Actions,
		}
	}

	// Extract repo name from git remote (just the repo name, not owner/repo)
	var repos []string
	if grove.GitRemote != "" {
		ownerRepo := extractOwnerRepo(grove.GitRemote)
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
		// Classify the error and update grove status
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

		s.updateGroveGitHubAppStatus(ctx, grove, state, errorCode, errorMessage)
		return "", "", err
	}

	// Cache rate limit info
	if rl := client.GetRateLimit(); rl != nil {
		s.mu.Lock()
		s.githubAppRateLimit = rl
		s.mu.Unlock()
	}

	// Success — update grove status
	now := timeNow()
	grove.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:         store.GitHubAppStateOK,
		LastTokenMint: &now,
		LastChecked:   now,
	}
	if err := s.store.UpdateProject(ctx, grove); err != nil {
		slog.Warn("Failed to update grove status after successful token mint", "error", err)
	} else {
		s.events.PublishGroveUpdated(ctx, grove)
	}

	return token.Token, token.ExpiresAt.Format("2006-01-02T15:04:05Z"), nil
}

// updateGroveGitHubAppStatus is a helper to update a grove's GitHub App status.
func (s *Server) updateGroveGitHubAppStatus(ctx context.Context, grove *store.Project, state, errorCode, errorMessage string) {
	now := timeNow()
	grove.GitHubAppStatus = &store.GitHubAppProjectStatus{
		State:        state,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		LastChecked:  now,
		LastError:    &now,
	}
	if err := s.store.UpdateProject(ctx, grove); err != nil {
		slog.Warn("Failed to update grove GitHub App status", "grove_id", grove.ID, "error", err)
	} else {
		s.events.PublishGroveUpdated(ctx, grove)
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

// MintGitHubAppTokenForGrove implements GitHubAppTokenMinter.
// It mints a GitHub App installation token for the given grove.
func (s *Server) MintGitHubAppTokenForGrove(ctx context.Context, grove *store.Project) (string, string, error) {
	if grove.GitHubInstallationID == nil {
		return "", "", nil
	}

	// Check if the app is configured
	s.mu.RLock()
	appConfigured := s.config.GitHubAppConfig.AppID != 0
	s.mu.RUnlock()

	if !appConfigured {
		return "", "", nil
	}

	return s.mintGitHubAppToken(ctx, grove)
}
