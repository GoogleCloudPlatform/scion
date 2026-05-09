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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	yamlv3 "gopkg.in/yaml.v3"
)

// Well-known secret keys for GitHub App credentials stored via the secrets system.
const (
	GitHubAppSecretPrivateKey    = "GITHUB_APP_PRIVATE_KEY"
	GitHubAppSecretWebhookSecret = "GITHUB_APP_WEBHOOK_SECRET"
)

// GitHubAppConfigResponse is the API response for GitHub App configuration.
// Sensitive fields (private key, webhook secret) are never returned.
type GitHubAppConfigResponse struct {
	AppID            int64                    `json:"app_id"`
	APIBaseURL       string                   `json:"api_base_url,omitempty"`
	WebhooksEnabled  bool                     `json:"webhooks_enabled"`
	Configured       bool                     `json:"configured"`
	HasPrivateKey    bool                     `json:"has_private_key"`
	HasWebhookSecret bool                     `json:"has_webhook_secret"`
	InstallationURL  string                   `json:"installation_url,omitempty"`
	RateLimit        *githubapp.RateLimitInfo `json:"rate_limit,omitempty"`
}

// GitHubAppConfigUpdateRequest is the API request to update GitHub App configuration.
type GitHubAppConfigUpdateRequest struct {
	AppID           *int64  `json:"app_id,omitempty"`
	PrivateKey      *string `json:"private_key,omitempty"`
	WebhookSecret   *string `json:"webhook_secret,omitempty"`
	APIBaseURL      *string `json:"api_base_url,omitempty"`
	WebhooksEnabled *bool   `json:"webhooks_enabled,omitempty"`
	InstallationURL *string `json:"installation_url,omitempty"`
}

// handleGitHubApp handles GET and PUT /api/v1/github-app.
func (s *Server) handleGitHubApp(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetGitHubApp(w, r)
	case http.MethodPut:
		s.handleUpdateGitHubApp(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleGetGitHubApp(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.config.GitHubAppConfig
	rateLimit := s.githubAppRateLimit
	s.mu.RUnlock()

	hasPrivateKey := cfg.PrivateKey != "" || cfg.PrivateKeyPath != ""
	hasWebhookSecret := cfg.WebhookSecret != ""

	// Also check secrets backend for stored credentials
	if !hasPrivateKey || !hasWebhookSecret {
		if s.secretBackend != nil {
			if !hasPrivateKey {
				if meta, err := s.secretBackend.GetMeta(r.Context(), GitHubAppSecretPrivateKey, store.ScopeHub, s.hubID); err == nil && meta != nil {
					hasPrivateKey = true
				}
			}
			if !hasWebhookSecret {
				if meta, err := s.secretBackend.GetMeta(r.Context(), GitHubAppSecretWebhookSecret, store.ScopeHub, s.hubID); err == nil && meta != nil {
					hasWebhookSecret = true
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, GitHubAppConfigResponse{
		AppID:            cfg.AppID,
		APIBaseURL:       cfg.APIBaseURL,
		WebhooksEnabled:  cfg.WebhooksEnabled,
		Configured:       cfg.AppID != 0 && hasPrivateKey,
		HasPrivateKey:    hasPrivateKey,
		HasWebhookSecret: hasWebhookSecret,
		InstallationURL:  cfg.InstallationURL,
		RateLimit:        rateLimit,
	})
}

func (s *Server) handleUpdateGitHubApp(w http.ResponseWriter, r *http.Request) {
	var req GitHubAppConfigUpdateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	ctx := r.Context()
	user := GetUserIdentityFromContext(ctx)
	userID := ""
	if user != nil {
		userID = user.ID()
	}

	// Store sensitive fields via secrets backend
	if req.PrivateKey != nil && *req.PrivateKey != "" {
		if err := s.setGitHubAppSecret(ctx, GitHubAppSecretPrivateKey, *req.PrivateKey, "GitHub App private key (PEM)", userID); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, fmt.Sprintf("failed to store private key: %v", err), nil)
			return
		}
		// Update in-memory config so it's immediately available
		s.mu.Lock()
		s.config.GitHubAppConfig.PrivateKey = *req.PrivateKey
		s.mu.Unlock()
	}

	if req.WebhookSecret != nil && *req.WebhookSecret != "" {
		if err := s.setGitHubAppSecret(ctx, GitHubAppSecretWebhookSecret, *req.WebhookSecret, "GitHub App webhook secret", userID); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, fmt.Sprintf("failed to store webhook secret: %v", err), nil)
			return
		}
		s.mu.Lock()
		s.config.GitHubAppConfig.WebhookSecret = *req.WebhookSecret
		s.mu.Unlock()
	}

	// Update non-sensitive fields in-memory
	s.mu.Lock()
	if req.AppID != nil {
		s.config.GitHubAppConfig.AppID = *req.AppID
	}
	if req.APIBaseURL != nil {
		s.config.GitHubAppConfig.APIBaseURL = *req.APIBaseURL
	}
	if req.WebhooksEnabled != nil {
		s.config.GitHubAppConfig.WebhooksEnabled = *req.WebhooksEnabled
	}
	if req.InstallationURL != nil {
		s.config.GitHubAppConfig.InstallationURL = *req.InstallationURL
	}
	cfg := s.config.GitHubAppConfig
	s.mu.Unlock()

	// Persist non-sensitive config to settings.yaml (best-effort — in-memory and secrets are already saved)
	if err := s.persistGitHubAppConfig(cfg); err != nil {
		slog.Warn("Failed to persist GitHub App config to settings.yaml (in-memory config updated successfully)", "error", err)
	}

	hasPrivateKey := cfg.PrivateKey != "" || cfg.PrivateKeyPath != ""
	hasWebhookSecret := cfg.WebhookSecret != ""
	// Check secrets backend too
	if !hasPrivateKey && s.secretBackend != nil {
		if meta, err := s.secretBackend.GetMeta(ctx, GitHubAppSecretPrivateKey, store.ScopeHub, s.hubID); err == nil && meta != nil {
			hasPrivateKey = true
		}
	}
	if !hasWebhookSecret && s.secretBackend != nil {
		if meta, err := s.secretBackend.GetMeta(ctx, GitHubAppSecretWebhookSecret, store.ScopeHub, s.hubID); err == nil && meta != nil {
			hasWebhookSecret = true
		}
	}

	slog.Info("GitHub App configuration updated via admin API", "user", userID, "app_id", cfg.AppID)

	writeJSON(w, http.StatusOK, GitHubAppConfigResponse{
		AppID:            cfg.AppID,
		APIBaseURL:       cfg.APIBaseURL,
		WebhooksEnabled:  cfg.WebhooksEnabled,
		Configured:       cfg.AppID != 0 && hasPrivateKey,
		HasPrivateKey:    hasPrivateKey,
		HasWebhookSecret: hasWebhookSecret,
		InstallationURL:  cfg.InstallationURL,
	})
}

// setGitHubAppSecret stores a GitHub App secret via the secrets backend,
// falling back to direct store if the backend is unavailable.
func (s *Server) setGitHubAppSecret(ctx context.Context, name, value, description, userID string) error {
	if s.secretBackend != nil {
		_, _, err := s.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name:          name,
			Value:         value,
			SecretType:    secret.TypeVariable,
			Scope:         store.ScopeHub,
			ScopeID:       s.hubID,
			Description:   description,
			InjectionMode: "as_needed",
			CreatedBy:     userID,
			UpdatedBy:     userID,
		})
		return err
	}

	// Fallback: store directly in the database (same pattern as ensureSigningKey)
	sec := &store.Secret{
		ID:             fmt.Sprintf("hub-ghapp-%s", strings.ToLower(strings.ReplaceAll(name, "_", "-"))),
		Key:            name,
		EncryptedValue: value,
		Scope:          store.ScopeHub,
		ScopeID:        s.hubID,
		SecretType:     store.SecretTypeVariable,
		Description:    description,
		Version:        1,
		CreatedBy:      userID,
		UpdatedBy:      userID,
	}
	_, err := s.store.UpsertSecret(ctx, sec)
	return err
}

// loadGitHubAppSecret loads a GitHub App secret from the secrets backend,
// falling back to direct store lookup.
func (s *Server) loadGitHubAppSecret(ctx context.Context, name string) (string, error) {
	if s.secretBackend != nil {
		sv, err := s.secretBackend.Get(ctx, name, store.ScopeHub, s.hubID)
		if err != nil {
			return "", err
		}
		return sv.Value, nil
	}

	// Fallback: read directly from the database
	return s.store.GetSecretValue(ctx, name, store.ScopeHub, s.hubID)
}

// persistGitHubAppConfig writes the non-sensitive GitHub App configuration
// fields to settings.yaml. Sensitive values (private key, webhook secret) are
// stored via the secrets system and are NOT written to settings.yaml.
func (s *Server) persistGitHubAppConfig(cfg GitHubAppServerConfig) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to resolve settings directory: %w", err)
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")

	var raw map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := yamlv3.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("failed to parse existing settings: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	// Build the github_app section with only non-sensitive fields.
	// This must be nested under "server" to match the V1ServerConfig schema
	// that loadServerFromSettingsFile expects.
	ghApp := map[string]interface{}{}
	if cfg.AppID != 0 {
		ghApp["app_id"] = cfg.AppID
	}
	if cfg.APIBaseURL != "" {
		ghApp["api_base_url"] = cfg.APIBaseURL
	}
	ghApp["webhooks_enabled"] = cfg.WebhooksEnabled
	// Preserve existing private_key_path if it was set via settings.yaml directly
	if cfg.PrivateKeyPath != "" {
		ghApp["private_key_path"] = cfg.PrivateKeyPath
	}
	if cfg.InstallationURL != "" {
		ghApp["installation_url"] = cfg.InstallationURL
	}

	if len(ghApp) > 0 {
		serverRaw, _ := raw["server"].(map[string]interface{})
		if serverRaw == nil {
			serverRaw = make(map[string]interface{})
		}
		serverRaw["github_app"] = ghApp
		raw["server"] = serverRaw
	}

	// Clean up any stale top-level github_app key from older versions
	delete(raw, "github_app")

	// Ensure schema_version is set
	if _, ok := raw["schema_version"]; !ok {
		raw["schema_version"] = "1"
	}

	newData, err := yamlv3.Marshal(raw)
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	return os.WriteFile(settingsPath, newData, 0644)
}

// handleGitHubAppInstallations handles GET and POST /api/v1/github-app/installations.
func (s *Server) handleGitHubAppInstallations(w http.ResponseWriter, r *http.Request) {
	// Check if this is a sub-route (e.g., /api/v1/github-app/installations/{id})
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/github-app/installations")
	if path != "" && path != "/" {
		subPath := strings.TrimPrefix(path, "/")
		subPath = strings.TrimSuffix(subPath, "/")

		// Handle /discover sub-route
		if subPath == "discover" {
			s.handleGitHubAppDiscover(w, r)
			return
		}

		// Sub-route: /api/v1/github-app/installations/{id}
		installationID, err := strconv.ParseInt(subPath, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid installation ID", nil)
			return
		}
		s.handleGitHubAppInstallationByID(w, r, installationID)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleListGitHubAppInstallations(w, r)
	case http.MethodPost:
		s.handleCreateGitHubAppInstallation(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleListGitHubAppInstallations(w http.ResponseWriter, r *http.Request) {
	filter := store.GitHubInstallationFilter{
		AccountLogin: r.URL.Query().Get("account_login"),
		Status:       r.URL.Query().Get("status"),
	}

	installations, err := s.store.ListGitHubInstallations(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to list installations", nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"installations": installations,
		"total":         len(installations),
	})
}

func (s *Server) handleCreateGitHubAppInstallation(w http.ResponseWriter, r *http.Request) {
	var installation store.GitHubInstallation
	if err := readJSON(r, &installation); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if installation.InstallationID == 0 {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "installation_id is required", nil)
		return
	}
	if installation.AccountLogin == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "account_login is required", nil)
		return
	}

	// Set app_id from server config if not provided
	if installation.AppID == 0 {
		s.mu.RLock()
		installation.AppID = s.config.GitHubAppConfig.AppID
		s.mu.RUnlock()
	}

	if err := s.store.CreateGitHubInstallation(r.Context(), &installation); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to create installation", nil)
		return
	}

	writeJSON(w, http.StatusCreated, installation)
}

func (s *Server) handleGitHubAppInstallationByID(w http.ResponseWriter, r *http.Request, installationID int64) {
	switch r.Method {
	case http.MethodGet:
		installation, err := s.store.GetGitHubInstallation(r.Context(), installationID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "installation not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get installation", nil)
			return
		}
		writeJSON(w, http.StatusOK, installation)

	case http.MethodPut:
		var installation store.GitHubInstallation
		if err := readJSON(r, &installation); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
			return
		}
		installation.InstallationID = installationID

		if err := s.store.UpdateGitHubInstallation(r.Context(), &installation); err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "installation not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to update installation", nil)
			return
		}
		writeJSON(w, http.StatusOK, installation)

	case http.MethodDelete:
		if err := s.store.DeleteGitHubInstallation(r.Context(), installationID); err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "installation not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to delete installation", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

// handleProjectGitHubInstallation handles PUT and DELETE /api/v1/groves/{id}/github-installation.
func (s *Server) handleProjectGitHubInstallation(w http.ResponseWriter, r *http.Request, groveID string) {
	switch r.Method {
	case http.MethodPut:
		var req struct {
			InstallationID int64 `json:"installation_id"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
			return
		}
		if req.InstallationID == 0 {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError, "installation_id is required", nil)
			return
		}

		// Verify installation exists
		if _, err := s.store.GetGitHubInstallation(r.Context(), req.InstallationID); err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "installation not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to verify installation", nil)
			return
		}

		grove, err := s.store.GetProject(r.Context(), groveID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
			return
		}

		grove.GitHubInstallationID = &req.InstallationID
		grove.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:       store.GitHubAppStateUnchecked,
			LastChecked: timeNow(),
		}

		if err := s.store.UpdateProject(r.Context(), grove); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to update grove", nil)
			return
		}
		s.events.PublishProjectUpdated(r.Context(), grove)

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"grove_id":        groveID,
			"installation_id": req.InstallationID,
			"status":          "associated",
		})

	case http.MethodDelete:
		grove, err := s.store.GetProject(r.Context(), groveID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
			return
		}

		grove.GitHubInstallationID = nil
		grove.GitHubAppStatus = nil

		if err := s.store.UpdateProject(r.Context(), grove); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to update grove", nil)
			return
		}
		s.events.PublishProjectUpdated(r.Context(), grove)

		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

// handleProjectGitHubStatus handles GET and POST /api/v1/groves/{id}/github-status.
// GET returns the current status. POST actively verifies the installation by
// checking with GitHub and attempting a token mint, then returns the updated status.
func (s *Server) handleProjectGitHubStatus(w http.ResponseWriter, r *http.Request, groveID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetProjectGitHubStatus(w, r, groveID)
	case http.MethodPost:
		s.handleCheckProjectGitHubStatus(w, r, groveID)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleGetProjectGitHubStatus(w http.ResponseWriter, r *http.Request, groveID string) {
	grove, err := s.store.GetProject(r.Context(), groveID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
		return
	}

	resp := map[string]interface{}{
		"grove_id":        groveID,
		"installation_id": grove.GitHubInstallationID,
		"status":          grove.GitHubAppStatus,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCheckGroveGitHubStatus actively verifies the grove's GitHub App
// installation by checking the installation on GitHub and attempting to mint
// a token. The grove's status is updated to reflect the result.
func (s *Server) handleCheckProjectGitHubStatus(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
		return
	}

	if project.GitHubInstallationID == nil {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "grove has no GitHub App installation", nil)
		return
	}

	// Try minting a token — this validates the installation, permissions, and
	// repo access in one shot, and updates the grove's status accordingly.
	_, _, mintErr := s.mintGitHubAppToken(ctx, project)

	// Re-read the project to get the updated status (mintGitHubAppToken updates it)
	project, err = s.store.GetProject(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to re-read grove after check", nil)
		return
	}

	resp := map[string]interface{}{
		"project_id":      projectID,
		"installation_id": project.GitHubInstallationID,
		"status":          project.GitHubAppStatus,
		"permissions":     project.GitHubPermissions,
	}
	if mintErr != nil {
		resp["check_error"] = mintErr.Error()
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleProjectGitHubPermissions handles GET, PUT, DELETE /api/v1/groves/{id}/github-permissions.
func (s *Server) handleProjectGitHubPermissions(w http.ResponseWriter, r *http.Request, groveID string) {
	switch r.Method {
	case http.MethodGet:
		grove, err := s.store.GetProject(r.Context(), groveID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
			return
		}

		perms := grove.GitHubPermissions
		if perms == nil {
			// Return defaults
			perms = &store.GitHubTokenPermissions{
				Contents:     "write",
				PullRequests: "write",
				Metadata:     "read",
			}
		}
		writeJSON(w, http.StatusOK, perms)

	case http.MethodPut:
		var perms store.GitHubTokenPermissions
		if err := readJSON(r, &perms); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
			return
		}

		grove, err := s.store.GetProject(r.Context(), groveID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
			return
		}

		grove.GitHubPermissions = &perms
		if err := s.store.UpdateProject(r.Context(), grove); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to update grove", nil)
			return
		}

		writeJSON(w, http.StatusOK, perms)

	case http.MethodDelete:
		grove, err := s.store.GetProject(r.Context(), groveID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
			return
		}

		grove.GitHubPermissions = nil
		if err := s.store.UpdateProject(r.Context(), grove); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to update grove", nil)
			return
		}

		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

// handleProjectGitIdentity handles GET, PUT, DELETE /api/v1/groves/{id}/git-identity.
func (s *Server) handleProjectGitIdentity(w http.ResponseWriter, r *http.Request, groveID string) {
	switch r.Method {
	case http.MethodGet:
		grove, err := s.store.GetProject(r.Context(), groveID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
			return
		}
		identity := grove.GitIdentity
		if identity == nil {
			identity = &store.GitIdentityConfig{Mode: "bot"}
		}
		writeJSON(w, http.StatusOK, identity)

	case http.MethodPut:
		var identity store.GitIdentityConfig
		if err := readJSON(r, &identity); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
			return
		}
		switch identity.Mode {
		case "bot", "custom", "co-authored":
		default:
			writeError(w, http.StatusBadRequest, ErrCodeValidationError, "mode must be 'bot', 'custom', or 'co-authored'", nil)
			return
		}
		if identity.Mode == "custom" && (identity.Name == "" || identity.Email == "") {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError, "name and email are required when mode is 'custom'", nil)
			return
		}
		grove, err := s.store.GetProject(r.Context(), groveID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
			return
		}
		grove.GitIdentity = &identity
		if err := s.store.UpdateProject(r.Context(), grove); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to update grove", nil)
			return
		}
		writeJSON(w, http.StatusOK, identity)

	case http.MethodDelete:
		grove, err := s.store.GetProject(r.Context(), groveID)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "grove not found", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to get grove", nil)
			return
		}
		grove.GitIdentity = nil
		if err := s.store.UpdateProject(r.Context(), grove); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to update grove", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}

// timeNow is a helper that returns the current time. Can be overridden in tests.
var timeNow = func() time.Time { return time.Now() }

// handleGitHubAppSyncPermissions handles POST /api/v1/github-app/sync-permissions.
// It fetches the GitHub App's current permissions and compares them against each
// grove's requested permissions, marking groves as degraded if they request
// permissions the app no longer has.
func (s *Server) handleGitHubAppSyncPermissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	appPermissions, affectedGroves, err := s.syncAppPermissions(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, ErrCodeInternalError, err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"app_permissions": appPermissions,
		"affected_groves": affectedGroves,
		"affected_count":  len(affectedGroves),
	})
}

// syncAppPermissions fetches the GitHub App's current permissions from the API
// and compares them against each grove's requested permissions. Groves requesting
// permissions the app no longer has are set to degraded state.
// Returns the app's current permissions and a list of affected groves.
func (s *Server) syncAppPermissions(ctx context.Context) (map[string]string, []map[string]interface{}, error) {
	client, err := s.getGitHubAppClient()
	if err != nil {
		return nil, nil, fmt.Errorf("GitHub App not configured: %v", err)
	}

	appInfo, err := client.GetApp(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch app info from GitHub: %v", err)
	}

	// Extract permissions map from the app response
	appPermissions := make(map[string]string)
	if permsRaw, ok := appInfo["permissions"]; ok {
		if permsMap, ok := permsRaw.(map[string]interface{}); ok {
			for k, v := range permsMap {
				if vs, ok := v.(string); ok {
					appPermissions[k] = vs
				}
			}
		}
	}

	slog.Info("Synced GitHub App permissions",
		"permission_count", len(appPermissions),
	)

	// List all groves and check their requested permissions against the app's permissions
	groves, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		return appPermissions, nil, fmt.Errorf("failed to list groves: %v", err)
	}

	var affectedGroves []map[string]interface{}
	now := timeNow()

	for _, grove := range groves.Items {
		if grove.GitHubInstallationID == nil || grove.GitHubPermissions == nil {
			continue
		}

		// Compare each grove's requested permissions against the app's permissions
		missingPerms := comparePermissions(grove.GitHubPermissions, appPermissions)
		if len(missingPerms) == 0 {
			continue
		}

		// Grove requests permissions the app doesn't have — mark as degraded
		msg := fmt.Sprintf("App is missing permissions requested by this grove: %s. Update the GitHub App's permissions in the app settings.", strings.Join(missingPerms, ", "))

		grove.GitHubAppStatus = &store.GitHubAppProjectStatus{
			State:        store.GitHubAppStateDegraded,
			ErrorCode:    githubapp.ErrCodePermissionDenied,
			ErrorMessage: msg,
			LastChecked:  now,
			LastError:    &now,
		}

		if err := s.store.UpdateProject(ctx, &grove); err != nil {
			slog.Error("Failed to update grove after permission sync",
				"grove_id", grove.ID, "error", err)
			continue
		}

		affectedGroves = append(affectedGroves, map[string]interface{}{
			"grove_id":            grove.ID,
			"grove_name":          grove.Name,
			"missing_permissions": missingPerms,
		})

		slog.Warn("Grove marked as degraded due to missing app permissions",
			"grove_id", grove.ID, "grove_name", grove.Name,
			"missing_permissions", missingPerms,
		)
	}

	return appPermissions, affectedGroves, nil
}

// comparePermissions checks each non-empty permission in the grove's requested
// permissions against the app's available permissions. Returns a list of
// permission names that the grove requests but the app does not have (or has
// at a lower level).
func comparePermissions(grovePerms *store.GitHubTokenPermissions, appPerms map[string]string) []string {
	var missing []string

	checks := []struct {
		name  string
		level string
	}{
		{"contents", grovePerms.Contents},
		{"pull_requests", grovePerms.PullRequests},
		{"issues", grovePerms.Issues},
		{"metadata", grovePerms.Metadata},
		{"checks", grovePerms.Checks},
		{"actions", grovePerms.Actions},
	}

	for _, check := range checks {
		if check.level == "" {
			continue
		}

		appLevel, ok := appPerms[check.name]
		if !ok {
			// App doesn't have this permission at all
			missing = append(missing, fmt.Sprintf("%s:%s", check.name, check.level))
			continue
		}

		// Check if the app's level is sufficient
		// "write" satisfies "read", but "read" does not satisfy "write"
		if check.level == "write" && appLevel == "read" {
			missing = append(missing, fmt.Sprintf("%s:%s (app has %s)", check.name, check.level, appLevel))
		}
	}

	return missing
}

// githubAppHealthCheckHandler returns a function suitable for RegisterRecurring
// that performs periodic health checks on GitHub App installations and syncs
// app-level permissions.
func (s *Server) githubAppHealthCheckHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		slog.Info("GitHub App health check starting")

		client, err := s.getGitHubAppClient()
		if err != nil {
			slog.Error("GitHub App health check: client not available", "error", err)
			return
		}

		// Step 1: Check all active installations
		installations, err := s.store.ListGitHubInstallations(ctx, store.GitHubInstallationFilter{
			Status: store.GitHubInstallationStatusActive,
		})
		if err != nil {
			slog.Error("GitHub App health check: failed to list installations", "error", err)
			return
		}

		var checked, deleted, suspended int
		for _, inst := range installations {
			ghInst, err := client.GetInstallation(ctx, inst.InstallationID)
			if err != nil {
				// Check if this is a classified GitHub error
				if mintErr, ok := err.(*githubapp.TokenMintError); ok {
					switch mintErr.ErrorCode {
					case githubapp.ErrCodeInstallationRevoked:
						// Installation no longer exists on GitHub
						inst.Status = store.GitHubInstallationStatusDeleted
						if updateErr := s.store.UpdateGitHubInstallation(ctx, &inst); updateErr != nil {
							slog.Error("GitHub App health check: failed to mark installation as deleted",
								"installation_id", inst.InstallationID, "error", updateErr)
						}
						s.updateGrovesForInstallation(ctx, inst.InstallationID, store.GitHubAppStateError,
							githubapp.ErrCodeInstallationRevoked,
							"Installation was revoked on GitHub. Reinstall the GitHub App for this org/account.")
						deleted++
						slog.Warn("GitHub App health check: installation revoked",
							"installation_id", inst.InstallationID,
							"account", inst.AccountLogin,
						)

					case githubapp.ErrCodeInstallationSuspended:
						inst.Status = store.GitHubInstallationStatusSuspended
						if updateErr := s.store.UpdateGitHubInstallation(ctx, &inst); updateErr != nil {
							slog.Error("GitHub App health check: failed to mark installation as suspended",
								"installation_id", inst.InstallationID, "error", updateErr)
						}
						s.updateGrovesForInstallation(ctx, inst.InstallationID, store.GitHubAppStateError,
							githubapp.ErrCodeInstallationSuspended,
							"Installation is suspended. Contact org admin to unsuspend.")
						suspended++
						slog.Warn("GitHub App health check: installation suspended",
							"installation_id", inst.InstallationID,
							"account", inst.AccountLogin,
						)

					default:
						slog.Warn("GitHub App health check: failed to verify installation",
							"installation_id", inst.InstallationID,
							"error", err,
						)
					}
				} else {
					slog.Warn("GitHub App health check: failed to verify installation",
						"installation_id", inst.InstallationID,
						"error", err,
					)
				}
				continue
			}

			// Installation exists — check if it became suspended
			if ghInst.SuspendedAt != nil {
				inst.Status = store.GitHubInstallationStatusSuspended
				if updateErr := s.store.UpdateGitHubInstallation(ctx, &inst); updateErr != nil {
					slog.Error("GitHub App health check: failed to update suspended installation",
						"installation_id", inst.InstallationID, "error", updateErr)
				}
				s.updateGrovesForInstallation(ctx, inst.InstallationID, store.GitHubAppStateError,
					githubapp.ErrCodeInstallationSuspended,
					"Installation is suspended. Contact org admin to unsuspend.")
				suspended++
			}

			checked++
		}

		slog.Info("GitHub App health check: installations verified",
			"checked", checked, "deleted", deleted, "suspended", suspended,
		)

		// Step 2: Sync app-level permissions
		_, affectedGroves, err := s.syncAppPermissions(ctx)
		if err != nil {
			slog.Error("GitHub App health check: permission sync failed", "error", err)
			return
		}

		slog.Info("GitHub App health check completed",
			"installations_checked", checked+deleted+suspended,
			"installations_deleted", deleted,
			"installations_suspended", suspended,
			"permission_affected_groves", len(affectedGroves),
		)
	}
}
