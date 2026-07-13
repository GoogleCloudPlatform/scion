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
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	yamlv3 "gopkg.in/yaml.v3"
)

// ServerConfigResponse is the API representation of the server settings file.
// It mirrors the on-disk settings.yaml structure, omitting sensitive fields.
type ServerConfigResponse struct {
	// Read-only server build info (not persisted in settings.yaml).
	ScionVersion   string `json:"scion_version,omitempty"`
	ScionCommit    string `json:"scion_commit,omitempty"`
	ScionBuildTime string `json:"scion_build_time,omitempty"`

	// SettingsTier indicates the settings backend: "db" or "file".
	SettingsTier string `json:"settings_tier,omitempty"`

	SchemaVersion        string                               `json:"schema_version"`
	ActiveProfile        string                               `json:"active_profile,omitempty"`
	DefaultTemplate      string                               `json:"default_template,omitempty"`
	DefaultHarnessConfig string                               `json:"default_harness_config,omitempty"`
	ImageRegistry        string                               `json:"image_registry,omitempty"`
	WorkspacePath        string                               `json:"workspace_path,omitempty"`
	Server               *config.V1ServerConfig               `json:"server,omitempty"`
	Telemetry            *config.V1TelemetryConfig            `json:"telemetry,omitempty"`
	Runtimes             map[string]config.V1RuntimeConfig    `json:"runtimes,omitempty"`
	HarnessConfigs       map[string]config.HarnessConfigEntry `json:"harness_configs,omitempty"`
	Profiles             map[string]config.V1ProfileConfig    `json:"profiles,omitempty"`

	// Default agent limits
	DefaultMaxTurns      int               `json:"default_max_turns,omitempty"`
	DefaultMaxModelCalls int               `json:"default_max_model_calls,omitempty"`
	DefaultMaxDuration   string            `json:"default_max_duration,omitempty"`
	DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty"`

	// EnvOverrides lists koanf keys overridden by SCION_SERVER_* env vars
	// on this node. Present in both file-mode and DB-mode responses so the
	// admin UI can show env-pinned fields regardless of settings tier.
	EnvOverrides []string `json:"env_overrides,omitempty"`
}

// ServerConfigUpdateRequest is the payload for updating settings.
type ServerConfigUpdateRequest struct {
	SchemaVersion        *string                              `json:"schema_version,omitempty"`
	ActiveProfile        *string                              `json:"active_profile,omitempty"`
	DefaultTemplate      *string                              `json:"default_template,omitempty"`
	DefaultHarnessConfig *string                              `json:"default_harness_config,omitempty"`
	ImageRegistry        *string                              `json:"image_registry,omitempty"`
	WorkspacePath        *string                              `json:"workspace_path,omitempty"`
	Server               *config.V1ServerConfig               `json:"server,omitempty"`
	Telemetry            *config.V1TelemetryConfig            `json:"telemetry,omitempty"`
	Runtimes             map[string]config.V1RuntimeConfig    `json:"runtimes,omitempty"`
	HarnessConfigs       map[string]config.HarnessConfigEntry `json:"harness_configs,omitempty"`
	Profiles             map[string]config.V1ProfileConfig    `json:"profiles,omitempty"`

	// Default agent limits
	DefaultMaxTurns      *int              `json:"default_max_turns,omitempty"`
	DefaultMaxModelCalls *int              `json:"default_max_model_calls,omitempty"`
	DefaultMaxDuration   *string           `json:"default_max_duration,omitempty"`
	DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty"`
}

// handleAdminServerConfig handles GET/PUT /api/v1/admin/server-config.
// GET: Returns the current global settings.yaml contents (sensitive fields masked).
// PUT: Updates global settings.yaml and optionally reloads applicable runtime settings.
func (s *Server) handleAdminServerConfig(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	// In postgres mode, delegate to the DB-backed handlers that use
	// OperationalSettings for Layer-1 reads/writes (design §3.8).
	// File/SQLite mode keeps the exact current behavior (file read/write).
	if ops := s.GetOperationalSettings(); ops != nil && s.IsPostgres() {
		switch r.Method {
		case http.MethodGet:
			s.handleGetServerConfigDB(w, r, ops)
		case http.MethodPut:
			s.handlePutServerConfigDB(w, r, ops)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetServerConfig(w)
	case http.MethodPut:
		s.handlePutServerConfig(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminServerConfigSectionReset handles
// DELETE /api/v1/admin/server-config/sections/{name}
// Resets a managed section back to bootstrap material by deleting the DB row.
// Postgres mode only; admin-gated. Design §3.2.4.
func (s *Server) handleAdminServerConfigSectionReset(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	if r.Method != http.MethodDelete {
		MethodNotAllowed(w)
		return
	}

	ops := s.GetOperationalSettings()
	if ops == nil || !s.IsPostgres() {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"Section reset is only available in postgres mode", nil)
		return
	}

	sectionName := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/server-config/sections/")
	if sectionName == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"Section name is required", nil)
		return
	}

	sec := opsettings.SectionByName(sectionName)
	if sec == nil {
		writeError(w, http.StatusNotFound, ErrCodeNotFound,
			"Unknown section: "+sectionName, nil)
		return
	}

	if err := ops.DeleteSection(r.Context(), sectionName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeNotFound,
				"Section not found in database: "+sectionName, nil)
			return
		}
		slog.Error("Failed to reset section", "section", sectionName, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to reset section", nil)
		return
	}

	slog.Info("Section reset to bootstrap", "section", sectionName, "by", user.Email())
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reset":   true,
		"section": sectionName,
	})
}

// handleGetServerConfig reads and returns the global settings.yaml.
func (s *Server) handleGetServerConfig(w http.ResponseWriter) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to resolve settings directory", nil)
		return
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			resp := ServerConfigResponse{
				ScionVersion:   version.Short(),
				ScionCommit:    version.GetCommit(),
				ScionBuildTime: version.GetBuildTime(),
				SettingsTier:   "file",
				SchemaVersion:  "1",
			}
			envK := config.LoadEnvKoanf()
			if envOverrides := opsettings.DetectEnvOverrides(envK); len(envOverrides) > 0 {
				sort.Strings(envOverrides)
				resp.EnvOverrides = envOverrides
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read settings file", nil)
		return
	}

	var vs config.VersionedSettings
	if err := yamlv3.Unmarshal(data, &vs); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to parse settings file", nil)
		return
	}

	// Mask sensitive fields before sending to the client
	resp := ServerConfigResponse{
		ScionVersion:         version.Short(),
		ScionCommit:          version.GetCommit(),
		ScionBuildTime:       version.GetBuildTime(),
		SettingsTier:         "file",
		SchemaVersion:        vs.SchemaVersion,
		ActiveProfile:        vs.ActiveProfile,
		DefaultTemplate:      vs.DefaultTemplate,
		DefaultHarnessConfig: vs.DefaultHarnessConfig,
		ImageRegistry:        vs.ImageRegistry,
		WorkspacePath:        vs.WorkspacePath,
		Server:               vs.Server,
		Telemetry:            vs.Telemetry,
		Runtimes:             vs.Runtimes,
		HarnessConfigs:       vs.HarnessConfigs,
		Profiles:             vs.Profiles,
		DefaultMaxTurns:      vs.DefaultMaxTurns,
		DefaultMaxModelCalls: vs.DefaultMaxModelCalls,
		DefaultMaxDuration:   vs.DefaultMaxDuration,
		DefaultResources:     vs.DefaultResources,
	}

	// Env overrides — detect SCION_SERVER_* env vars so the admin UI can
	// show env-pinned fields in file mode too (H1).
	envK := config.LoadEnvKoanf()
	if envOverrides := opsettings.DetectEnvOverrides(envK); len(envOverrides) > 0 {
		sort.Strings(envOverrides)
		resp.EnvOverrides = envOverrides
	}

	maskSensitiveFields(&resp)
	writeJSON(w, http.StatusOK, resp)
}

// handlePutServerConfig updates the global settings.yaml.
func (s *Server) handlePutServerConfig(w http.ResponseWriter, r *http.Request) {
	var req ServerConfigUpdateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to resolve settings directory", nil)
		return
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")

	// Load existing settings to merge with updates
	var raw map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := yamlv3.Unmarshal(data, &raw); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to parse existing settings", nil)
			return
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	// Apply updates by marshaling the request fields and merging
	applySettingsUpdates(raw, &req)

	// Ensure schema_version is set
	if _, ok := raw["schema_version"]; !ok {
		raw["schema_version"] = "1"
	}

	newData, err := yamlv3.Marshal(raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal settings", nil)
		return
	}

	if err := os.WriteFile(settingsPath, newData, 0644); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to write settings file", nil)
		return
	}

	slog.Info("Server config updated via admin API",
		"user", user(GetUserIdentityFromContext(r.Context())),
	)

	// Attempt to reload applicable runtime settings
	reloadResults := s.reloadSettings()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "saved",
		"reload": reloadResults,
	})
}

// reloadSettings re-reads the settings file and applies runtime-changeable values.
// Returns a summary of what was reloaded and what requires a restart.
//
// This is the file-mode path: it loads GlobalConfig from settings.yaml,
// builds a Layer1Snapshot, and delegates to applySnapshot. In postgres mode,
// the OperationalSettings service provides the snapshot instead.
func (s *Server) reloadSettings() map[string]interface{} {
	results := map[string]interface{}{
		"applied":          []string{},
		"requires_restart": []string{},
	}

	gc, err := config.LoadGlobalConfig("")
	if err != nil {
		slog.Error("Failed to reload global config", "error", err)
		results["error"] = err.Error()
		return results
	}

	snap := BuildLayer1SnapshotFromFile(gc)
	results = ApplySnapshot(s, snap)

	// Log level is a Layer-0 setting (per design §3.1) — only applied in
	// file mode via reloadSettings, not through OperationalSettings.
	if gc.LogLevel != "" {
		applySnapshotLogLevel(gc.LogLevel)
		applied := results["applied"].([]string)
		applied = append(applied, "log_level")
		results["applied"] = applied
	}

	return results
}

// applySettingsUpdates merges the update request into the raw settings map.
func applySettingsUpdates(raw map[string]interface{}, req *ServerConfigUpdateRequest) {
	if req.SchemaVersion != nil {
		raw["schema_version"] = *req.SchemaVersion
	}
	if req.ActiveProfile != nil {
		raw["active_profile"] = *req.ActiveProfile
	}
	if req.DefaultTemplate != nil {
		raw["default_template"] = *req.DefaultTemplate
	}
	if req.DefaultHarnessConfig != nil {
		raw["default_harness_config"] = *req.DefaultHarnessConfig
	}
	if req.ImageRegistry != nil {
		raw["image_registry"] = *req.ImageRegistry
	}
	if req.WorkspacePath != nil {
		raw["workspace_path"] = *req.WorkspacePath
	}

	if req.Server != nil {
		newServer := marshalToMap(req.Server)
		// Merge into existing server section to preserve keys not present in the
		// update (e.g. github_app managed via its own endpoint).
		if existing, ok := raw["server"]; ok {
			if existingMap, ok := existing.(map[string]interface{}); ok {
				if newMap, ok := newServer.(map[string]interface{}); ok {
					for k, v := range newMap {
						existingMap[k] = v
					}
					newServer = existingMap
				}
			}
		}
		raw["server"] = newServer
	}
	if req.Telemetry != nil {
		raw["telemetry"] = marshalToMap(req.Telemetry)
	}
	if req.Runtimes != nil {
		raw["runtimes"] = marshalToMap(req.Runtimes)
	}
	if req.HarnessConfigs != nil {
		raw["harness_configs"] = marshalToMap(req.HarnessConfigs)
	}
	if req.Profiles != nil {
		raw["profiles"] = marshalToMap(req.Profiles)
	}

	if req.DefaultMaxTurns != nil {
		if *req.DefaultMaxTurns > 0 {
			raw["default_max_turns"] = *req.DefaultMaxTurns
		} else {
			delete(raw, "default_max_turns")
		}
	}
	if req.DefaultMaxModelCalls != nil {
		if *req.DefaultMaxModelCalls > 0 {
			raw["default_max_model_calls"] = *req.DefaultMaxModelCalls
		} else {
			delete(raw, "default_max_model_calls")
		}
	}
	if req.DefaultMaxDuration != nil {
		if *req.DefaultMaxDuration != "" {
			raw["default_max_duration"] = *req.DefaultMaxDuration
		} else {
			delete(raw, "default_max_duration")
		}
	}
	if req.DefaultResources != nil {
		raw["default_resources"] = marshalToMap(req.DefaultResources)
	}
}

// marshalToMap converts a struct to a map[string]interface{} via YAML round-trip.
func marshalToMap(v interface{}) interface{} {
	data, err := yamlv3.Marshal(v)
	if err != nil {
		return v
	}
	var m interface{}
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		return v
	}
	return m
}

// maskSensitiveFields redacts secrets from the response before sending to the client.
func maskSensitiveFields(resp *ServerConfigResponse) {
	if resp.Server == nil {
		return
	}

	// Mask OAuth client secrets
	if resp.Server.OAuth != nil {
		maskOAuthClient(resp.Server.OAuth.Web)
		maskOAuthClient(resp.Server.OAuth.CLI)
		maskOAuthClient(resp.Server.OAuth.Device)
	}

	// Mask auth tokens
	if resp.Server.Auth != nil {
		if resp.Server.Auth.DevToken != "" {
			resp.Server.Auth.DevToken = "********"
		}
	}

	// Mask broker token
	if resp.Server.Broker != nil {
		if resp.Server.Broker.BrokerToken != "" {
			resp.Server.Broker.BrokerToken = "********"
		}
	}

	// Mask database URL (may contain credentials)
	if resp.Server.Database != nil {
		if resp.Server.Database.URL != "" {
			resp.Server.Database.URL = "********"
		}
	}

	// Mask secrets backend credentials
	if resp.Server.Secrets != nil {
		if resp.Server.Secrets.GCPCredentials != "" {
			resp.Server.Secrets.GCPCredentials = "********"
		}
	}

	// N1: Mask GitHubApp private key and webhook secret (pre-existing gap,
	// applies to both DB-mode and file-mode GET paths).
	if resp.Server.GitHubApp != nil {
		if resp.Server.GitHubApp.PrivateKey != "" {
			resp.Server.GitHubApp.PrivateKey = "********"
		}
		if resp.Server.GitHubApp.WebhookSecret != "" {
			resp.Server.GitHubApp.WebhookSecret = "********"
		}
	}

	// Mask notification channel params (may contain webhook URLs/tokens)
	for i := range resp.Server.NotificationChannels {
		for k := range resp.Server.NotificationChannels[i].Params {
			resp.Server.NotificationChannels[i].Params[k] = "********"
		}
	}
}

// maskOAuthClient masks OAuth client secrets in the response.
func maskOAuthClient(c *config.V1OAuthClientConfig) {
	if c == nil {
		return
	}
	if c.Google != nil && c.Google.ClientSecret != "" {
		c.Google.ClientSecret = "********"
	}
	if c.GitHub != nil && c.GitHub.ClientSecret != "" {
		c.GitHub.ClientSecret = "********"
	}
}

// user returns the email or ID string for logging purposes.
func user(u UserIdentity) string {
	if u == nil {
		return "unknown"
	}
	return u.Email()
}
