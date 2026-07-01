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
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// IntegrationManager is the narrow interface satisfied by *plugin.Manager.
// It lets the hub query and control broker plugins without importing the
// plugin package directly.
type IntegrationManager interface {
	ListPlugins() []string
	HasPlugin(pluginType, name string) bool
	GetPluginConfig(pluginType, name string) map[string]string
	IsSelfManaged(pluginType, name string) bool
	ConfigureBroker(name string, extra map[string]string) error
	Reconnect(pluginType, name string) error
	BrokerHealthCheck(name string) (status, message string, details map[string]string, err error)
	BrokerInfo(name string) (version, channelID string, capabilities []string, err error)
}

// --- Response types ---

// IntegrationSummary is the response element for the list endpoint.
type IntegrationSummary struct {
	Name        string             `json:"name"`
	Platform    string             `json:"platform"`
	SelfManaged bool               `json:"self_managed"`
	HasSecrets  map[string]bool    `json:"has_secrets"`
	Status      *IntegrationStatus `json:"status,omitempty"`
}

// IntegrationDetail is the response for the single-integration GET endpoint.
type IntegrationDetail struct {
	Name        string             `json:"name"`
	Platform    string             `json:"platform"`
	SelfManaged bool               `json:"self_managed"`
	Settings    map[string]string  `json:"settings"`
	HasSecrets  map[string]bool    `json:"has_secrets"`
	Status      *IntegrationStatus `json:"status,omitempty"`
}

// IntegrationStatus holds runtime status information from a broker plugin.
type IntegrationStatus struct {
	Connected    bool              `json:"connected"`
	Version      string            `json:"version,omitempty"`
	ChannelID    string            `json:"channel_id,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Health       string            `json:"health,omitempty"`
	Message      string            `json:"message,omitempty"`
	Details      map[string]string `json:"details,omitempty"`
}

// IntegrationConfigUpdateRequest is the request body for the PUT config endpoint.
type IntegrationConfigUpdateRequest struct {
	Settings map[string]string `json:"settings"`
	Secrets  map[string]string `json:"secrets"`
}

// --- Route dispatchers ---

// handleAdminIntegrations dispatches GET /api/v1/admin/integrations.
func (s *Server) handleAdminIntegrations(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleListIntegrations(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminIntegrationByName dispatches requests under
// /api/v1/admin/integrations/{name}[/config|/restart|/health].
func (s *Server) handleAdminIntegrationByName(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	// Parse: /api/v1/admin/integrations/{name}[/{action}]
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/integrations/")
	path = strings.TrimSuffix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]
	if name == "" {
		NotFound(w, "integration")
		return
	}

	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		s.handleGetIntegration(w, r, name)
	case "config":
		if r.Method != http.MethodPut {
			MethodNotAllowed(w)
			return
		}
		s.handleUpdateIntegrationConfig(w, r, name)
	case "restart":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.handleRestartIntegration(w, r, name)
	case "health":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		s.handleIntegrationHealth(w, r, name)
	default:
		NotFound(w, "integration endpoint")
	}
}

// --- Handlers ---

func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil {
		writeJSON(w, http.StatusOK, []IntegrationSummary{})
		return
	}

	plugins := mgr.ListPlugins()
	summaries := make([]IntegrationSummary, 0, len(plugins))
	for _, key := range plugins {
		name := pluginNameFromKey(key)
		if name == "" {
			continue
		}

		summary := IntegrationSummary{
			Name:        name,
			Platform:    resolvePlatform(name),
			SelfManaged: mgr.IsSelfManaged("broker", name),
			HasSecrets:  s.checkIntegrationSecrets(r.Context(), name),
			Status:      getIntegrationStatus(mgr, name),
		}
		summaries = append(summaries, summary)
	}

	writeJSON(w, http.StatusOK, summaries)
}

func (s *Server) handleGetIntegration(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil || !mgr.HasPlugin("broker", name) {
		NotFound(w, "integration")
		return
	}

	cfg := mgr.GetPluginConfig("broker", name)
	if cfg == nil {
		cfg = make(map[string]string)
	}

	detail := IntegrationDetail{
		Name:        name,
		Platform:    resolvePlatform(name),
		SelfManaged: mgr.IsSelfManaged("broker", name),
		Settings:    filterSensitiveConfig(name, cfg),
		HasSecrets:  s.checkIntegrationSecrets(r.Context(), name),
		Status:      getIntegrationStatus(mgr, name),
	}

	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleUpdateIntegrationConfig(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil || !mgr.HasPlugin("broker", name) {
		NotFound(w, "integration")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req IntegrationConfigUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	ctx := r.Context()
	user := GetUserIdentityFromContext(ctx)
	userID := ""
	if user != nil {
		userID = user.ID()
	}

	// Store secrets via secret backend (never written to YAML).
	if len(req.Secrets) > 0 {
		mappings := config.PluginSecretKeyMap[name]
		allowedSecrets := make(map[string]string, len(mappings))
		for _, m := range mappings {
			allowedSecrets[m.ConfigKey] = m.SecretKey
		}

		for configKey, value := range req.Secrets {
			secretKey, ok := allowedSecrets[configKey]
			if !ok {
				BadRequest(w, "unknown secret key: "+configKey)
				return
			}
			if err := s.SetChatIntegrationSecret(ctx, secretKey, value, ChatSecretDescription(secretKey), userID); err != nil {
				slog.Error("Failed to store integration secret", "plugin", name, "key", configKey, "error", err)
				InternalError(w)
				return
			}
		}
	}

	// Write non-sensitive settings to YAML config file.
	if len(req.Settings) > 0 {
		pluginCfg := mgr.GetPluginConfig("broker", name)
		configFile := ""
		if pluginCfg != nil {
			configFile = pluginCfg["config_file"]
		}

		if configFile == "" {
			BadRequest(w, "integration has no config file configured")
			return
		}

		provider, err := config.NewYAMLConfigProvider(configFile)
		if err != nil {
			slog.Error("Failed to create config provider", "plugin", name, "error", err)
			InternalError(w)
			return
		}

		existing, err := provider.Load()
		if err != nil {
			slog.Error("Failed to load existing config", "plugin", name, "error", err)
			InternalError(w)
			return
		}

		// Merge new settings into existing, filtering out any secret keys.
		secretKeys := allSecretConfigKeys(name)
		for k, v := range req.Settings {
			if secretKeys[k] {
				continue
			}
			existing[k] = v
		}

		if err := provider.Save(existing); err != nil {
			slog.Error("Failed to save config", "plugin", name, "error", err)
			InternalError(w)
			return
		}
	}

	// Reconfigure the running integration with updated config.
	if err := s.reconfigureIntegration(r.Context(), mgr, name); err != nil {
		slog.Error("Failed to reconfigure integration after config update", "plugin", name, "error", err)
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRestartIntegration(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil || !mgr.HasPlugin("broker", name) {
		NotFound(w, "integration")
		return
	}

	if err := s.reconfigureIntegration(r.Context(), mgr, name); err != nil {
		slog.Error("Failed to restart integration", "plugin", name, "error", err)
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIntegrationHealth(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil || !mgr.HasPlugin("broker", name) {
		NotFound(w, "integration")
		return
	}

	status := getIntegrationStatus(mgr, name)
	if status == nil {
		status = &IntegrationStatus{Health: "unknown", Message: "unable to query plugin status"}
	}

	writeJSON(w, http.StatusOK, status)
}

// --- Helpers ---

// pluginNameFromKey extracts the plugin name from a "type:name" key,
// returning only broker plugin names.
func pluginNameFromKey(key string) string {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 || parts[0] != "broker" {
		return ""
	}
	return parts[1]
}

// resolvePlatform maps a plugin name to its user-facing platform name.
func resolvePlatform(name string) string {
	switch name {
	case "telegram":
		return "telegram"
	case "discord":
		return "discord"
	case "chat-app":
		return "gchat"
	default:
		return name
	}
}

// checkIntegrationSecrets returns a map of config_key → bool indicating
// whether each expected secret for the integration is present.
func (s *Server) checkIntegrationSecrets(ctx context.Context, name string) map[string]bool {
	mappings := config.PluginSecretKeyMap[name]
	result := make(map[string]bool, len(mappings))
	for _, m := range mappings {
		result[m.ConfigKey] = s.HasChatIntegrationSecret(ctx, m.SecretKey)
	}
	return result
}

// filterSensitiveConfig returns a copy of the config map with secret values
// and internal runtime keys removed.
func filterSensitiveConfig(name string, cfg map[string]string) map[string]string {
	filtered := make(map[string]string, len(cfg))

	secretKeys := allSecretConfigKeys(name)
	internalKeys := map[string]bool{
		"hub_url":     true,
		"hmac_key":    true,
		"broker_id":   true,
		"config_file": true,
	}

	for k, v := range cfg {
		if secretKeys[k] || internalKeys[k] {
			continue
		}
		filtered[k] = v
	}
	return filtered
}

// allSecretConfigKeys returns the set of config keys that correspond to
// secrets for the named plugin.
func allSecretConfigKeys(name string) map[string]bool {
	mappings := config.PluginSecretKeyMap[name]
	keys := make(map[string]bool, len(mappings))
	for _, m := range mappings {
		keys[m.ConfigKey] = true
	}
	return keys
}

// getIntegrationStatus queries health and info from the plugin manager.
func getIntegrationStatus(mgr IntegrationManager, name string) *IntegrationStatus {
	status := &IntegrationStatus{}

	version, channelID, capabilities, err := mgr.BrokerInfo(name)
	if err != nil {
		status.Health = "unknown"
		status.Message = "failed to query plugin info"
		return status
	}
	status.Version = version
	status.ChannelID = channelID
	status.Capabilities = capabilities
	status.Connected = true

	health, message, details, err := mgr.BrokerHealthCheck(name)
	if err != nil {
		status.Health = "unknown"
		status.Message = "failed to query health"
		return status
	}
	status.Health = health
	status.Message = message
	status.Details = details

	if health == "unhealthy" {
		status.Connected = false
	}

	return status
}

// reconfigureIntegration reloads config for a plugin and calls ConfigureBroker.
// For self-managed plugins, it falls back to Reconnect on ConfigureBroker failure.
func (s *Server) reconfigureIntegration(ctx context.Context, mgr IntegrationManager, name string) error {
	pluginCfg := mgr.GetPluginConfig("broker", name)

	// Re-read config file if one is configured.
	configFile := ""
	if pluginCfg != nil {
		configFile = pluginCfg["config_file"]
	}

	merged := make(map[string]string)
	if configFile != "" {
		fileMerged, err := config.LoadPluginConfigFile(configFile, nil)
		if err != nil {
			slog.Error("Failed to reload config file for reconfigure", "plugin", name, "error", err)
			for k, v := range pluginCfg {
				merged[k] = v
			}
		} else {
			merged = fileMerged
			// Carry over runtime/internal keys from the old config that
			// are not present in the file (e.g. hub_url, hmac_key).
			for k, v := range pluginCfg {
				if _, ok := merged[k]; !ok {
					merged[k] = v
				}
			}
		}
	} else {
		for k, v := range pluginCfg {
			merged[k] = v
		}
	}

	// Inject secrets from the secret backend.
	mappings := config.PluginSecretKeyMap[name]
	for _, m := range mappings {
		if existing := merged[m.ConfigKey]; existing != "" {
			continue
		}
		val, err := s.LoadChatIntegrationSecret(ctx, m.SecretKey)
		if err != nil || val == "" {
			continue
		}
		merged[m.ConfigKey] = val
	}

	if err := mgr.ConfigureBroker(name, merged); err != nil {
		if mgr.IsSelfManaged("broker", name) {
			slog.Warn("ConfigureBroker failed for self-managed plugin, trying Reconnect",
				"plugin", name, "error", err)
			return mgr.Reconnect("broker", name)
		}
		return err
	}

	return nil
}
