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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// IntegrationManager is the narrow interface satisfied by *plugin.Manager.
// It lets the hub query and control broker plugins without importing the
// plugin package directly.
type IntegrationManager interface {
	ListPlugins() []string
	HasPlugin(pluginType, name string) bool
	IsPluginActive(pluginType, name string) bool
	GetPluginConfig(pluginType, name string) map[string]string
	GetPluginConfigFile(pluginType, name string) string
	UpdatePluginConfig(pluginType, name string, config map[string]string)
	IsSelfManaged(pluginType, name string) bool
	ConfigureBroker(name string, extra map[string]string) error
	StartPlugin(pluginType, name string, cfg map[string]string) error
	Reconnect(pluginType, name string) error
	BrokerHealthCheck(name string) (status, message string, details map[string]string, err error)
	BrokerInfo(name string) (version, channelID string, capabilities []string, err error)
	GetBrokerEventBus(name string) (eventbus.EventBus, error)
	UpdatePlugin(name string, repoPath string) error
	InstallPlugin(name, repoPath, pluginsDir string) error
	RegisterPlugin(pluginType, name, path string, cfg map[string]string, configFile string)
	StopPlugin(pluginType, name string) error
	UnregisterPlugin(pluginType, name string) error
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

// AvailableIntegration represents a plugin that could be installed.
type AvailableIntegration struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

// knownPlugins is the list of plugins that can be discovered for installation.
var knownPlugins = []string{"telegram", "discord"}

var knownPluginSet = func() map[string]bool {
	s := make(map[string]bool, len(knownPlugins))
	for _, n := range knownPlugins {
		s[n] = true
	}
	return s
}()

// settingsWriteMu guards concurrent writes to settings.yaml.
var settingsWriteMu sync.Mutex

// pluginBuildMu guards concurrent build operations per plugin name.
var pluginBuildMu sync.Map

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

	// Special-case: "available" as a name with no action is the available-integrations list.
	if name == "available" && action == "" && r.Method == http.MethodGet {
		s.handleListAvailableIntegrations(w, r)
		return
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
	case "update":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.handleUpdateIntegration(w, r, name)
	case "install":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.handleInstallIntegration(w, r, name)
	case "uninstall":
		if r.Method != http.MethodDelete {
			MethodNotAllowed(w)
			return
		}
		s.handleUninstallIntegration(w, r, name)
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
				slog.Error("Failed to store integration secret", "plugin", name, "key", configKey, "error", err.Error())
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
	}

	// Write non-sensitive settings to YAML config file.
	if len(req.Settings) > 0 {
		configFile := mgr.GetPluginConfigFile("broker", name)

		if configFile == "" {
			BadRequest(w, "integration has no config file configured")
			return
		}

		provider, err := config.NewYAMLConfigProvider(configFile)
		if err != nil {
			slog.Error("Failed to create config provider", "plugin", name, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		existing, err := provider.Load()
		if err != nil {
			slog.Error("Failed to load existing config", "plugin", name, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
			slog.Error("Failed to save config", "plugin", name, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	// Reconfigure the running integration with updated config.
	if err := s.reconfigureIntegration(r.Context(), mgr, name); err != nil {
		if !mgr.IsPluginActive("broker", name) {
			writeJSON(w, http.StatusOK, map[string]string{
				"status":  "ok",
				"message": "Configuration saved. Restart to activate.",
			})
			return
		}
		slog.Error("Failed to reconfigure integration after config update", "plugin", name, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	updatedCfg := mgr.GetPluginConfig("broker", name)
	if updatedCfg == nil {
		updatedCfg = make(map[string]string)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"settings": filterSensitiveConfig(name, updatedCfg),
	})
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
		slog.Error("Failed to restart integration", "plugin", name, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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

func (s *Server) handleUpdateIntegration(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil || !mgr.HasPlugin("broker", name) {
		NotFound(w, "integration")
		return
	}

	if mgr.IsSelfManaged("broker", name) {
		BadRequest(w, "cannot update a self-managed integration")
		return
	}

	repoPath := s.config.MaintenanceConfig.RepoPath
	if repoPath == "" {
		slog.Error("No repository path configured for plugin update")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no repository path configured for plugin update"})
		return
	}

	mu := acquirePluginBuildLock(name)
	if mu == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "a build is already in progress for this integration",
		})
		return
	}
	defer releasePluginBuildLock(name)

	if err := mgr.UpdatePlugin(name, repoPath); err != nil {
		slog.Error("Failed to update integration", "plugin", name, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := s.reconfigureIntegration(r.Context(), mgr, name); err != nil {
		slog.Warn("Plugin rebuilt but reconfigure failed", "plugin", name, "error", err.Error())
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleInstallIntegration(w http.ResponseWriter, r *http.Request, name string) {
	if !knownPluginSet[name] {
		BadRequest(w, "unknown integration: "+name)
		return
	}

	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil {
		slog.Error("Plugin manager not initialized")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not initialized"})
		return
	}

	if mgr.HasPlugin("broker", name) {
		BadRequest(w, "integration is already installed")
		return
	}

	repoPath := s.config.MaintenanceConfig.RepoPath
	if repoPath == "" {
		slog.Error("No repository path configured for plugin install")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no repository path configured for plugin install"})
		return
	}

	sourceDir := filepath.Join(repoPath, "extras", "scion-"+name)
	if _, err := os.Stat(sourceDir); err != nil {
		NotFound(w, "plugin source")
		return
	}

	mu := acquirePluginBuildLock(name)
	if mu == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "a build is already in progress for this integration",
		})
		return
	}
	defer releasePluginBuildLock(name)

	pluginsDir, err := plugin.DefaultPluginsDir()
	if err != nil {
		slog.Error("Failed to resolve plugins directory", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := mgr.InstallPlugin(name, repoPath, pluginsDir); err != nil {
		slog.Error("Failed to install integration", "plugin", name, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	configFilePath := "~/.scion/scion-" + name + ".yaml"
	if err := config.CreatePluginConfigFile(name, configFilePath); err != nil {
		slog.Error("Failed to create plugin config file", "plugin", name, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	settingsWriteMu.Lock()
	err = config.AddPluginToSettings(name, configFilePath)
	settingsWriteMu.Unlock()
	if err != nil {
		slog.Error("Failed to add plugin to settings.yaml", "plugin", name, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	binaryPath := filepath.Join(pluginsDir, plugin.PluginTypeBroker, plugin.PluginBinaryPrefix+name)
	mgr.RegisterPlugin(plugin.PluginTypeBroker, name, binaryPath, nil, configFilePath)

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "installed",
		"message": "Plugin built and registered. Set secrets and restart to activate.",
	})
}

func (s *Server) handleUninstallIntegration(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "plugin manager not initialized"})
		return
	}

	if !mgr.HasPlugin("broker", name) {
		NotFound(w, "integration")
		return
	}

	pluginCfg := mgr.GetPluginConfig("broker", name)

	if mgr.IsPluginActive("broker", name) {
		if err := mgr.StopPlugin("broker", name); err != nil {
			slog.Error("Failed to stop plugin during uninstall", "plugin", name, "error", err.Error())
		}
	}

	if s.fanOutBus != nil {
		if err := s.fanOutBus.RemoveSpoke(name); err != nil {
			slog.Warn("Failed to remove spoke during uninstall", "plugin", name, "error", err.Error())
		}
	}

	ctx := r.Context()
	for _, m := range config.PluginSecretKeyMap[name] {
		if err := s.DeleteChatIntegrationSecret(ctx, m.SecretKey); err != nil {
			slog.Warn("Failed to delete secret during uninstall", "plugin", name, "key", m.SecretKey, "error", err.Error())
		}
	}

	if cfgFile := pluginCfg["config_file"]; cfgFile != "" {
		resolved := cfgFile
		if strings.HasPrefix(resolved, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				resolved = filepath.Join(home, resolved[2:])
			}
		}
		resolved = filepath.Clean(resolved)
		if err := os.Remove(resolved); err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to delete config file during uninstall", "plugin", name, "path", resolved, "error", err.Error())
		}
	}

	if dbPath := pluginCfg["db_path"]; dbPath != "" {
		resolved := dbPath
		if strings.HasPrefix(resolved, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				resolved = filepath.Join(home, resolved[2:])
			}
		}
		resolved = filepath.Clean(resolved)
		if home, err := os.UserHomeDir(); err == nil {
			scionDir := filepath.Join(home, ".scion") + string(filepath.Separator)
			if strings.HasPrefix(resolved, scionDir) {
				if err := os.Remove(resolved); err != nil && !os.IsNotExist(err) {
					slog.Warn("Failed to delete database during uninstall", "plugin", name, "path", resolved, "error", err.Error())
				}
			}
		}
	}

	if binPath := pluginCfg["path"]; binPath != "" {
		if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to delete binary during uninstall", "plugin", name, "path", binPath, "error", err.Error())
		}
	}

	settingsWriteMu.Lock()
	err := config.RemovePluginFromSettings(name)
	settingsWriteMu.Unlock()
	if err != nil {
		slog.Warn("Failed to remove plugin from settings.yaml", "plugin", name, "error", err.Error())
	}

	if err := mgr.UnregisterPlugin("broker", name); err != nil {
		slog.Error("Failed to unregister plugin", "plugin", name, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	slog.Info("Integration uninstalled", "plugin", name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "uninstalled"})
}

func (s *Server) handleListAvailableIntegrations(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	repoPath := s.config.MaintenanceConfig.RepoPath

	var available []AvailableIntegration
	for _, name := range knownPlugins {
		if mgr != nil && mgr.HasPlugin("broker", name) {
			continue
		}
		if repoPath != "" {
			sourceDir := filepath.Join(repoPath, "extras", "scion-"+name)
			if _, err := os.Stat(sourceDir); err != nil {
				continue
			}
		} else {
			continue
		}
		available = append(available, AvailableIntegration{
			Name:     name,
			Platform: resolvePlatform(name),
		})
	}

	if available == nil {
		available = []AvailableIntegration{}
	}
	writeJSON(w, http.StatusOK, available)
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
		"hub_url":          true,
		"hmac_key":         true,
		"broker_id":        true,
		"bot_id":           true,
		"config_file":      true,
		"_host_callbacks":  true,
		"plugin_name":      true,
		"project_slug_map": true,
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

// addBrokerSpoke adds a spoke to the fan-out event bus for a broker plugin
// that was just started. This mirrors the startup path in server_foreground.go.
func (s *Server) addBrokerSpoke(mgr IntegrationManager, name string) {
	s.mu.RLock()
	fanout := s.fanOutBus
	s.mu.RUnlock()

	if fanout == nil {
		return
	}

	bus, err := mgr.GetBrokerEventBus(name)
	if err != nil {
		slog.Error("Failed to get broker event bus for spoke", "plugin", name, "error", err)
		return
	}

	_, channelID, capabilities, err := mgr.BrokerInfo(name)
	if err != nil {
		slog.Error("Failed to get broker info for spoke", "plugin", name, "error", err.Error())
		return
	}
	observer := false
	for _, cap := range capabilities {
		if strings.EqualFold(cap, "observer") {
			observer = true
			break
		}
	}

	spoke := eventbus.NamedEventBus{
		Name:      name,
		Bus:       bus,
		Observer:  observer,
		ChannelID: channelID,
	}

	if err := fanout.AddSpoke(spoke); err != nil {
		if replaceErr := fanout.ReplaceSpoke(name, spoke); replaceErr != nil {
			slog.Error("Failed to add/replace broker spoke", "plugin", name,
				"addErr", err, "replaceErr", replaceErr)
			return
		}
		slog.Info("Replaced existing message broker spoke on restart", "name", name)
	}

	slog.Info("Message broker spoke added", "name", name, "channel_id", channelID, "observer", observer)
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

// acquirePluginBuildLock tries to acquire a per-plugin build lock. Returns a
// non-nil *sync.Mutex if acquired, nil if another build is already in progress.
func acquirePluginBuildLock(name string) *sync.Mutex {
	mu := &sync.Mutex{}
	mu.Lock()
	actual, loaded := pluginBuildMu.LoadOrStore(name, mu)
	if loaded {
		existing := actual.(*sync.Mutex)
		if !existing.TryLock() {
			return nil
		}
		return existing
	}
	return mu
}

func releasePluginBuildLock(name string) {
	if actual, ok := pluginBuildMu.Load(name); ok {
		actual.(*sync.Mutex).Unlock()
	}
}

// pluginBrokerNS is the deterministic UUIDv5 namespace for plugin broker IDs.
// Must match the namespace used in cmd/server_foreground.go so that broker
// entities created at startup and at reconfigure time share the same ID.
var pluginBrokerNS = uuid.MustParse("5c104390-a1d0-5e9a-9b1e-5c104390a1d0")

// getPluginHubCreds generates hub runtime credentials for a broker plugin.
// This mirrors the credential injection in cmd/server_foreground.go so that
// plugins installed after startup receive the same hub_url, broker_id, and
// hmac_key that plugins loaded at boot time get via ConfigureBroker.
func (s *Server) getPluginHubCreds(ctx context.Context, name string) map[string]string {
	authSvc := s.GetBrokerAuthService()
	if authSvc == nil {
		slog.Warn("BrokerAuthService not available, cannot generate hub credentials", "plugin", name)
		return nil
	}

	legacyID := "plugin-broker-" + name
	brokerID := uuid.NewSHA1(pluginBrokerNS, []byte(legacyID)).String()

	if _, err := s.store.GetRuntimeBroker(ctx, brokerID); err != nil {
		pluginBroker := &store.RuntimeBroker{
			ID:              brokerID,
			Name:            "plugin-" + name,
			Slug:            api.Slugify("plugin-" + name),
			Version:         "0.1.0",
			Status:          store.BrokerStatusOnline,
			ConnectionState: "embedded",
			Labels:          map[string]string{"scion.io/plugin": name},
			Created:         time.Now(),
			Updated:         time.Now(),
		}
		if createErr := s.store.CreateRuntimeBroker(ctx, pluginBroker); createErr != nil {
			slog.Warn("Failed to register broker entity for plugin", "plugin", name, "error", createErr)
		}
	}

	secretKey, err := authSvc.GenerateAndStoreSecret(ctx, brokerID)
	if err != nil {
		slog.Error("Failed to generate secret for plugin", "plugin", name, "error", err)
		return nil
	}

	creds := map[string]string{
		"hub_url":     s.config.HubEndpoint,
		"hmac_key":    secretKey,
		"broker_id":   brokerID,
		"plugin_name": name,
	}

	if projects, listErr := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500}); listErr == nil {
		slugMap := make(map[string]string, len(projects.Items))
		for _, p := range projects.Items {
			if p.Slug != "" {
				slugMap[p.ID] = p.Slug
			} else {
				slugMap[p.ID] = p.Name
			}
		}
		if jsonBytes, jsonErr := json.Marshal(slugMap); jsonErr == nil {
			creds["project_slug_map"] = string(jsonBytes)
		}
	}

	return creds
}

// reconfigureIntegration reloads config for a plugin and calls ConfigureBroker.
// For self-managed plugins, it falls back to Reconnect on ConfigureBroker failure.
func (s *Server) reconfigureIntegration(ctx context.Context, mgr IntegrationManager, name string) error {
	pluginCfg := mgr.GetPluginConfig("broker", name)

	// Re-read config file if one is configured.
	configFile := mgr.GetPluginConfigFile("broker", name)

	merged := make(map[string]string)
	if configFile != "" {
		fileMerged, err := config.LoadPluginConfigFile(configFile, nil)
		if err != nil {
			slog.Error("Failed to reload config file for reconfigure", "plugin", name, "error", err.Error())
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
		if existing, hasKey := merged[m.ConfigKey]; existing != "" || hasKey {
			continue
		}
		val, err := s.LoadChatIntegrationSecret(ctx, m.SecretKey)
		if err != nil {
			slog.Error("Failed to load integration secret", "plugin", name, "key", m.SecretKey, "error", err.Error())
			continue
		}
		if val == "" {
			slog.Warn("Integration secret is empty", "plugin", name, "key", m.SecretKey)
			continue
		}
		merged[m.ConfigKey] = val
	}

	// Inject hub runtime credentials (hub_url, broker_id, hmac_key) when
	// not already present. Plugins loaded at startup receive these via the
	// ConfigureBroker call in server_foreground.go; plugins installed
	// post-startup need them injected here.
	if !mgr.IsSelfManaged("broker", name) && merged["hub_url"] == "" {
		if hubCreds := s.getPluginHubCreds(ctx, name); hubCreds != nil {
			for k, v := range hubCreds {
				merged[k] = v
			}
			slog.Info("Injected hub credentials into plugin at reconfigure time",
				"plugin", name, "broker_id", hubCreds["broker_id"])
		}
	}

	for k, v := range merged {
		if strings.HasPrefix(v, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				merged[k] = filepath.Join(home, v[2:])
			}
		}
	}

	if !mgr.IsPluginActive("broker", name) {
		if err := mgr.StartPlugin("broker", name, merged); err != nil {
			return err
		}
		mgr.UpdatePluginConfig("broker", name, merged)
		s.addBrokerSpoke(mgr, name)
		return nil
	}

	if err := mgr.ConfigureBroker(name, merged); err != nil {
		if mgr.IsSelfManaged("broker", name) {
			slog.Warn("ConfigureBroker failed for self-managed plugin, trying Reconnect",
				"plugin", name, "error", err.Error())
			return mgr.Reconnect("broker", name)
		}
		return err
	}

	mgr.UpdatePluginConfig("broker", name, merged)
	return nil
}
