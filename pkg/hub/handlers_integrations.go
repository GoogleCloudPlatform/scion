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
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/integrationupdate"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/secretmigration"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reconfigureRuntimeKeys lists keys that are injected at runtime/wiring time
// and must be carried over during reconfigure (they are not in config files).
var pluginBrokerNS = uuid.MustParse("5c104390-a1d0-5e9a-9b1e-5c104390a1d0")

var hubKeys = map[string]bool{
	"hub_url": true, "broker_id": true, "hmac_key": true,
	"plugin_name": true, "project_slug_map": true,
	"database_driver": true, "database_url": true,
}

var reconfigureRuntimeKeys = map[string]bool{
	"config_file":      true,
	"hub_url":          true,
	"hmac_key":         true,
	"broker_id":        true,
	"plugin_name":      true,
	"project_slug_map": true,
	"database_url":     true,
	"database_driver":  true,
	"bot_id":           true,
	"mode":             true,
	"path":             true,
	"address":          true,
	"tls_cert_file":    true,
	"tls_key_file":     true,
	"tls_ca_file":      true,
	"tls_skip_verify":  true,
}

// IntegrationManager is the narrow interface satisfied by *plugin.Manager.
// It lets the hub query and control broker plugins without importing the
// plugin package directly.
type IntegrationManager interface {
	ListPlugins() []string
	HasPlugin(pluginType, name string) bool
	GetPluginConfig(pluginType, name string) map[string]string
	GetPluginConfigFile(pluginType, name string) string
	IsSelfManaged(pluginType, name string) bool
	GetDeploymentMode(pluginType, name string) plugin.DeploymentMode
	ConfigureBroker(name string, extra map[string]string) error
	ReplaceBrokerConfig(name string, cfg map[string]string) error
	RestartBrokerPlugin(name string, cfg map[string]string) error
	Reconnect(pluginType, name string) error
	BrokerHealthCheck(name string) (status, message string, details map[string]string, err error)
	BrokerInfo(name string) (version, channelID string, capabilities []string, err error)
	UpdatePlugin(name string, repoPath string) error
	InstallPlugin(name, repoPath, pluginsDir, configFile string) error
	LoadOne(pluginType, name string, entry plugin.PluginEntry, pluginsDir string) error
	GetBroker(name string) (eventbus.EventBus, error)
	GetGRPCBrokerAdapter(name string) plugin.GRPCBrokerClient
}

// --- Response types ---

// IntegrationSummary is the response element for the list endpoint.
type IntegrationSummary struct {
	Name           string             `json:"name"`
	Platform       string             `json:"platform"`
	SelfManaged    bool               `json:"self_managed"`
	DeploymentMode string             `json:"deployment_mode"`
	HasSecrets     map[string]bool    `json:"has_secrets"`
	Status         *IntegrationStatus `json:"status,omitempty"`
}

// IntegrationDetail is the response for the single-integration GET endpoint.
type IntegrationDetail struct {
	Name           string             `json:"name"`
	Platform       string             `json:"platform"`
	SelfManaged    bool               `json:"self_managed"`
	DeploymentMode string             `json:"deployment_mode"`
	Settings       map[string]string  `json:"settings"`
	HasSecrets     map[string]bool    `json:"has_secrets"`
	Status         *IntegrationStatus `json:"status,omitempty"`
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
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Description string `json:"description,omitempty"`
}

// IntegrationUpdateResponse is returned by POST update for HA integrations (202)
// and by GET update/{id}.
type IntegrationUpdateResponse struct {
	ID          string `json:"id"`
	Integration string `json:"integration"`
	State       string `json:"state"`
	Detail      string `json:"detail,omitempty"`
	NewVersion  string `json:"new_version,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// KnownPlugin describes a plugin that can be discovered for installation.
// The catalog replaces the former knownPlugins string list so that per-plugin
// metadata (binary name, source directory, self-managed flag) is explicit
// rather than derived from naming conventions.
type KnownPlugin struct {
	Name        string // settings.yaml key: "a2a-bridge"
	Platform    string // platform identifier: "a2a"
	BinaryName  string // binary name, default "scion-plugin-<name>"
	SourceDir   string // source directory, default "extras/scion-<name>"
	SelfManaged bool   // true = register+instruct, never build/launch
	Description string // human-readable description for the available-integrations list
}

var knownPluginCatalog = []KnownPlugin{
	{Name: "telegram", Platform: "telegram", BinaryName: "scion-plugin-telegram", SourceDir: "extras/scion-telegram", Description: "Chat integration — built and managed by the Hub"},
	{Name: "discord", Platform: "discord", BinaryName: "scion-plugin-discord", SourceDir: "extras/scion-discord", Description: "Chat integration — built and managed by the Hub"},
	{Name: "slack", Platform: "slack", BinaryName: "scion-plugin-slack", SourceDir: "extras/scion-slack", Description: "Chat integration — built and managed by the Hub"},
	{Name: "a2a-bridge", Platform: "a2a", BinaryName: "scion-a2a-bridge", SourceDir: "extras/scion-a2a-bridge", SelfManaged: true, Description: "External service — installed separately, managed via admin UI"},
	{Name: "chat-app", Platform: "gchat", BinaryName: "scion-chat-app", SourceDir: "extras/scion-chat-app", SelfManaged: true, Description: "Google Chat integration — installed separately, managed via admin UI"},
	{Name: "teams", Platform: "teams", BinaryName: "scion-plugin-teams", SourceDir: "extras/scion-teams", Description: "Chat integration — built and managed by the Hub"},
}

var knownPluginSet = func() map[string]bool {
	s := make(map[string]bool, len(knownPluginCatalog))
	for _, p := range knownPluginCatalog {
		s[p.Name] = true
	}
	return s
}()

// lookupKnownPlugin returns the catalog entry for the named plugin, or nil.
func lookupKnownPlugin(name string) *KnownPlugin {
	for i := range knownPluginCatalog {
		if knownPluginCatalog[i].Name == name {
			return &knownPluginCatalog[i]
		}
	}
	return nil
}

// SettingsWriteMu guards concurrent writes to settings.yaml.
var SettingsWriteMu sync.Mutex

// pluginBuildMu guards concurrent build operations per plugin name.
var pluginBuildMu sync.Map

// --- Route dispatchers ---

// handleAdminIntegrations dispatches GET /api/v1/admin/integrations.
// Authorization: route guard checks hub.integrations.read for GET.
func (s *Server) handleAdminIntegrations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListIntegrations(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminIntegrationByName dispatches requests under
// /api/v1/admin/integrations/{name}[/config|/restart|/health].
// Authorization: route guard checks hub.integrations.read for GET.
// Write operations (PUT/POST/DELETE) require hub.integrations.update via inline Decide.
func (s *Server) handleAdminIntegrationByName(w http.ResponseWriter, r *http.Request) {
	// Inline authorization for write operations — the route guard only
	// checks the read permission. Writes need hub.integrations.update.
	switch r.Method {
	case http.MethodPut, http.MethodPost, http.MethodDelete:
		if _, ok := s.requireWritePermission(w, r, "hub.integrations.update"); !ok {
			return
		}
	}

	// Parse: /api/v1/admin/integrations/{name}[/{action}[/{sub}]]
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/integrations/")
	path = strings.TrimSuffix(path, "/")
	parts := strings.SplitN(path, "/", 3)
	name := parts[0]
	if name == "" {
		NotFound(w, "integration")
		return
	}

	action := ""
	if len(parts) >= 2 {
		action = parts[1]
	}
	actionSub := ""
	if len(parts) >= 3 {
		actionSub = parts[2]
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
		if actionSub != "" && r.Method == http.MethodGet {
			s.handleGetUpdateStatus(w, r, name, actionSub)
			return
		}
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
	seen := make(map[string]bool, len(plugins))
	for _, key := range plugins {
		name := pluginNameFromKey(key)
		if name == "" {
			continue
		}
		seen[name] = true

		summary := IntegrationSummary{
			Name:           name,
			Platform:       resolvePlatform(name),
			SelfManaged:    mgr.IsSelfManaged("broker", name),
			DeploymentMode: string(mgr.GetDeploymentMode("broker", name)),
			HasSecrets:     s.checkIntegrationSecrets(r.Context(), name),
			Status:         getIntegrationStatus(mgr, name),
		}
		summaries = append(summaries, summary)
	}

	// Union-merge installed-but-unconfigured plugins from settings.yaml.
	// When LoadOne fails (e.g. bot_token missing at first install), the plugin
	// is never in mgr.clients and won't appear above — but it IS in
	// settings.yaml and should still be listed so the user can configure it.
	globalDir, err := config.GetGlobalDir()
	if err == nil {
		if vs, err := config.LoadSingleFileVersioned(globalDir); err == nil {
			if vs.Server != nil && vs.Server.Plugins != nil {
				for name := range vs.Server.Plugins.Broker {
					if seen[name] {
						continue
					}
					summaries = append(summaries, IntegrationSummary{
						Name:     name,
						Platform: resolvePlatform(name),
						Status: &IntegrationStatus{
							Connected: false,
							Message:   "Plugin installed — configure bot_token to activate",
						},
						HasSecrets: s.checkIntegrationSecrets(r.Context(), name),
					})
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, summaries)
}

func (s *Server) handleGetIntegration(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil || !mgr.HasPlugin("broker", name) {
		// Fallback: plugin may be in settings.yaml but not loaded (e.g. bot_token
		// was missing at startup so LoadOne failed). Return a stub detail so the
		// UI can show the configuration form instead of an error toast.
		if installedPluginSettingsEntry(name) != nil {
			writeJSON(w, http.StatusOK, IntegrationDetail{
				Name:     name,
				Platform: resolvePlatform(name),
				Settings: map[string]string{},
				Status: &IntegrationStatus{
					Connected: false,
					Message:   "Plugin installed — configure required fields to activate",
				},
			})
			return
		}
		NotFound(w, "integration")
		return
	}

	runtimeCfg := mgr.GetPluginConfig("broker", name)
	if runtimeCfg == nil {
		runtimeCfg = make(map[string]string)
	}

	// Resolve settings from the same provider that PUT writes to, so
	// GET reflects the latest saved state rather than the boot-time map.
	cfg := s.resolveIntegrationSettings(r.Context(), mgr, name, runtimeCfg)

	detail := IntegrationDetail{
		Name:           name,
		Platform:       resolvePlatform(name),
		SelfManaged:    mgr.IsSelfManaged("broker", name),
		DeploymentMode: string(mgr.GetDeploymentMode("broker", name)),
		Settings:       filterSensitiveConfig(name, cfg),
		HasSecrets:     s.checkIntegrationSecrets(r.Context(), name),
		Status:         getIntegrationStatus(mgr, name),
	}

	writeJSON(w, http.StatusOK, detail)
}

// resolveIntegrationSettings returns the current settings for a plugin by
// reading from the appropriate config provider (Postgres for HA, YAML file for
// non-HA). Runtime/wiring keys from the manager map are merged as an underlay.
func (s *Server) resolveIntegrationSettings(ctx context.Context, mgr IntegrationManager, name string, runtimeCfg map[string]string) map[string]string {
	if s.isHAIntegration(mgr, name) {
		if s.entClient != nil {
			provider := config.NewPostgresConfigProvider(s.entClient, name)
			if settings, err := provider.Load(ctx); err == nil {
				// Merge runtime keys as underlay — provider values win.
				for k, v := range runtimeCfg {
					if _, ok := settings[k]; !ok {
						settings[k] = v
					}
				}
				return settings
			}
			slog.Warn("Failed to load HA config for GET, falling back to manager map", "plugin", name)
		}
		return runtimeCfg
	}

	configFile := mgr.GetPluginConfigFile("broker", name)
	if configFile == "" {
		configFile = runtimeCfg["config_file"]
	}
	if configFile != "" {
		if settings, err := config.ResolvePluginConfig(configFile, nil); err == nil {
			for k, v := range runtimeCfg {
				if _, ok := settings[k]; !ok {
					settings[k] = v
				}
			}
			return settings
		}
		slog.Warn("Failed to reload config file for GET, falling back to manager map", "plugin", name)
	}

	return runtimeCfg
}

func (s *Server) handleUpdateIntegrationConfig(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil {
		NotFound(w, "integration")
		return
	}

	loaded := mgr.HasPlugin("broker", name)
	var settingsEntry *config.V1PluginEntry
	if !loaded {
		// Mirror handleGetIntegration's fallback: the plugin may be registered
		// in settings.yaml but not loaded — on a fresh install LoadOne fails
		// because required fields (e.g. bot_token) don't exist yet. GET already
		// returns a stub so the UI can show the config form; PUT must accept
		// that form's submission, otherwise the required fields can never be
		// saved and the integration can never activate.
		settingsEntry = installedPluginSettingsEntry(name)
		if settingsEntry == nil {
			NotFound(w, "integration")
			return
		}
	}

	isHA := false
	if loaded {
		isHA = s.isHAIntegration(mgr, name)
	} else {
		pe := plugin.PluginEntry{SelfManaged: settingsEntry.SelfManaged, Mode: settingsEntry.Mode}
		isHA = pe.ResolvedDeploymentMode() == plugin.DeploymentModeHA
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

	// Write non-sensitive settings: HA mode uses PostgresConfigProvider,
	// non-HA mode uses YAML config file.
	if len(req.Settings) > 0 {
		var provider config.IntegrationConfigProvider
		var haConfigTx *ent.Tx

		if isHA {
			if !s.requirePostgres(w) {
				return
			}
			if s.entClient == nil {
				InternalError(w)
				return
			}
			haTx, txErr := s.entClient.Tx(ctx)
			if txErr != nil {
				slog.Error("Failed to begin transaction for config update", "plugin", name, "error", txErr)
				InternalError(w)
				return
			}
			defer func() { _ = haTx.Rollback() }()
			pgProvider := config.NewPostgresConfigProvider(haTx.Client(), name)
			pgProvider.SetUpdatedBy(userID)
			provider = pgProvider
			haConfigTx = haTx
		} else {
			configFile := ""
			if loaded {
				configFile = mgr.GetPluginConfigFile("broker", name)
				if configFile == "" {
					pluginCfg := mgr.GetPluginConfig("broker", name)
					if pluginCfg != nil {
						configFile = pluginCfg["config_file"]
					}
				}
			} else {
				configFile = settingsEntry.ConfigFile
			}

			if configFile == "" {
				BadRequest(w, "integration has no config file configured")
				return
			}

			yamlProvider, err := config.NewYAMLConfigProvider(configFile)
			if err != nil {
				slog.Error("Failed to create config provider", "plugin", name, "error", err)
				InternalError(w)
				return
			}
			provider = yamlProvider
		}

		existing, err := provider.Load(ctx)
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

		if err := provider.Save(ctx, existing); err != nil {
			slog.Error("Failed to save config", "plugin", name, "error", err)
			InternalError(w)
			return
		}

		// For HA mode, NOTIFY within the same transaction as the config write.
		if haConfigTx != nil {
			signal := AdminSignal{
				Integration: name,
				Kind:        "config",
			}
			if err := publishAdminSignalTx(ctx, haConfigTx, signal); err != nil {
				slog.Warn("Failed to NOTIFY config change", "integration", name, "error", err)
			}
			if err := haConfigTx.Commit(); err != nil {
				slog.Error("Failed to commit config update transaction", "plugin", name, "error", err)
				InternalError(w)
				return
			}
		}
	}

	// Ensure the plugin is listed in message_broker.types on disk so it
	// survives future restarts/rebuilds that re-read settings.yaml.
	SettingsWriteMu.Lock()
	if err := config.AddPluginToMessageBrokerTypes(name); err != nil {
		slog.Warn("Failed to ensure plugin in message_broker.types", "plugin", name, "error", err)
		// Non-fatal — continue with the config update.
	}
	SettingsWriteMu.Unlock()

	// Reconfigure the running integration with updated config.
	// For HA integrations the DB write + NOTIFY is the reconfigure path —
	// pushing a hub-side merge over gRPC would race with the DB-backed reload.
	if loaded {
		if !isHA {
			if err := s.reconfigureIntegration(r.Context(), mgr, name); err != nil {
				slog.Error("Failed to reconfigure integration after config update", "plugin", name, "error", err)
				InternalError(w)
				return
			}
			// The plugin may have been installed mid-session and never wired
			// into the FanOut at startup. Ensure it has a spoke now.
			s.ensureBrokerSpoke(mgr, name)
		}
	} else {
		// The plugin was installed but never loaded (required fields were
		// missing at install/startup). Now that config and secrets are saved,
		// try to activate it. Failure is non-fatal: the config is persisted,
		// and the plugin will load on the next server restart once all its
		// required fields are present.
		if err := s.activateInstalledIntegration(ctx, mgr, name, settingsEntry); err != nil {
			slog.Warn("Config saved but plugin activation failed (likely still missing required fields)",
				"plugin", name, "error", err)
		} else {
			s.refreshBrokerSpoke(mgr, name)
			slog.Info("Plugin activated after config update", "plugin", name)
		}
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

	// Ensure the plugin is listed in message_broker.types on disk so it
	// survives future restarts/rebuilds that re-read settings.yaml.
	SettingsWriteMu.Lock()
	if err := config.AddPluginToMessageBrokerTypes(name); err != nil {
		slog.Warn("Failed to ensure plugin in message_broker.types", "plugin", name, "error", err)
		// Non-fatal — continue with the restart.
	}
	SettingsWriteMu.Unlock()

	// Resolve the latest config from file + secrets + hub wiring creds.
	merged := s.resolveIntegrationMergedConfig(r.Context(), mgr, name)

	// Full process restart: kill the old plugin process and start a new one
	// with the resolved config. This ensures a fresh go-plugin handshake and
	// new MuxBroker connections for host callbacks, fixing stale callback
	// issues that arise when a plugin was initially started with incomplete
	// config (e.g. first-time install before the user configures bot_token).
	if err := mgr.RestartBrokerPlugin(name, merged); err != nil {
		slog.Error("Failed to restart integration", "plugin", name, "error", err)
		InternalError(w)
		return
	}

	// Replace the FanOut spoke with a fresh RPC connection to the restarted
	// plugin process. The old spoke wraps a dead connection from the killed
	// process. refreshBrokerSpoke replaces an existing spoke or adds a new
	// one if none existed (e.g. first install before a full hub restart).
	s.refreshBrokerSpoke(mgr, name)

	// Post-restart validation: check that the plugin is wired into the FanOut.
	warnings := s.validateIntegrationWiring(name)

	response := map[string]interface{}{
		"status": "ok",
	}
	if len(warnings) > 0 {
		response["warnings"] = warnings
	}

	writeJSON(w, http.StatusOK, response)
}

// validateIntegrationWiring checks that the named plugin is wired into the
// FanOut event bus as a spoke. Returns warnings for any issues found.
func (s *Server) validateIntegrationWiring(name string) []string {
	var warnings []string

	proxy := s.GetMessageBrokerProxy()
	if proxy == nil {
		warnings = append(warnings, "message broker not initialized")
		return warnings
	}
	fanout, ok := proxy.bus.(*eventbus.FanOutEventBus)
	if !ok {
		return warnings
	}
	if !fanout.HasSpoke(name) {
		warnings = append(warnings, fmt.Sprintf("plugin %q is not wired into the FanOut event bus — messages will not be routed", name))
	}

	return warnings
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

	// Self-managed plugins: in dev mode (RepoPath set), offer a binary rebuild
	// but do NOT restart the bridge process. Otherwise reject with guidance.
	if kp := lookupKnownPlugin(name); kp != nil && kp.SelfManaged {
		repoPath := s.config.MaintenanceConfig.RepoPath
		if repoPath == "" {
			BadRequest(w, "self-managed integrations cannot be updated via the Hub — update the binary manually and click Reconnect")
			return
		}
		s.handleRebuildSelfManaged(w, r, kp, repoPath)
		return
	}

	// HA (Mode 3) integrations: insert an update request row + NOTIFY.
	if s.isHAIntegration(mgr, name) {
		s.handleUpdateIntegrationHA(w, r, name)
		return
	}

	repoPath := s.config.MaintenanceConfig.RepoPath
	if repoPath == "" {
		slog.Error("No repository path configured for plugin update")
		InternalError(w)
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

	// Ensure the plugin is listed in message_broker.types on disk so it
	// survives future restarts/rebuilds that re-read settings.yaml.
	SettingsWriteMu.Lock()
	if err := config.AddPluginToMessageBrokerTypes(name); err != nil {
		slog.Warn("Failed to ensure plugin in message_broker.types", "plugin", name, "error", err)
		// Non-fatal — continue with the update.
	}
	SettingsWriteMu.Unlock()

	if err := mgr.UpdatePlugin(name, repoPath); err != nil {
		slog.Error("Failed to update integration", "plugin", name, "error", err)
		InternalError(w)
		return
	}

	if err := s.reconfigureIntegration(r.Context(), mgr, name); err != nil {
		slog.Warn("Plugin rebuilt but reconfigure failed", "plugin", name, "error", err)
	}

	s.refreshBrokerSpoke(mgr, name)

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
		InternalError(w)
		return
	}

	if mgr.HasPlugin("broker", name) {
		BadRequest(w, "integration is already installed")
		return
	}

	// Look up the catalog entry for this plugin.
	kp := lookupKnownPlugin(name)
	if kp == nil {
		BadRequest(w, "unknown integration: "+name)
		return
	}

	// Self-managed plugins have a distinct install flow: register + scaffold
	// + instruct, no build or process launch.
	if kp.SelfManaged {
		s.handleInstallSelfManaged(w, r, mgr, kp)
		return
	}

	repoPath := s.config.MaintenanceConfig.RepoPath
	binaryName := kp.BinaryName
	_, lookPathErr := exec.LookPath(binaryName)
	binaryOnPath := lookPathErr == nil

	if repoPath == "" && !binaryOnPath {
		slog.Error("No repository path configured and plugin binary not found on PATH",
			"plugin", name, "binary", binaryName)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Plugin installation requires either a repository path or the plugin binary on PATH", nil)
		return
	}

	// Source-dir check and build lock only apply when building from source.
	if repoPath != "" {
		sourceDir := filepath.Join(repoPath, kp.SourceDir)
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
	}

	pluginsDir, err := plugin.DefaultPluginsDir()
	if err != nil {
		slog.Error("Failed to resolve plugins directory", "error", err)
		InternalError(w)
		return
	}

	configFilePath := "~/.scion/scion-" + name + ".yaml"
	resolvedConfigPath, err := resolveTilde(configFilePath)
	if err != nil {
		slog.Error("Failed to resolve config file path", "plugin", name, "error", err)
		InternalError(w)
		return
	}
	if _, err := os.Stat(resolvedConfigPath); err != nil {
		if !os.IsNotExist(err) {
			slog.Error("Failed to check plugin config file", "plugin", name, "path", resolvedConfigPath, "error", err)
			InternalError(w)
			return
		}
		if err := config.CreatePluginConfigFile(name, configFilePath); err != nil {
			slog.Error("Failed to create plugin config file", "plugin", name, "error", err)
			InternalError(w)
			return
		}
	} else {
		slog.Info("Plugin config file already exists, preserving", "plugin", name, "path", resolvedConfigPath)
	}

	SettingsWriteMu.Lock()
	err = config.AddPluginToSettings(name, configFilePath)
	if err == nil {
		err = config.AddPluginToMessageBrokerTypes(name)
	}
	SettingsWriteMu.Unlock()
	if err != nil {
		slog.Error("Failed to add plugin to settings.yaml", "plugin", name, "error", err)
		InternalError(w)
		return
	}

	if repoPath != "" {
		// Build from source (dev mode).
		if err := mgr.InstallPlugin(name, repoPath, pluginsDir, configFilePath); err != nil {
			slog.Error("Failed to install integration", "plugin", name, "error", err)
			// Use a sanitized message — raw error may contain compiler output and host paths.
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Plugin installation failed — check server logs for details", nil)
			return
		}
	} else {
		// Binary is already on PATH (Homebrew/package-manager install).
		// Config file and settings.yaml were written above — no build needed.
		slog.Info("Plugin binary found on PATH, skipping build", "plugin", name, "binary", binaryName)

		binaryPath, _ := exec.LookPath(binaryName)
		if err := mgr.LoadOne(plugin.PluginTypeBroker, name, plugin.PluginEntry{Path: binaryPath, ConfigFile: configFilePath}, pluginsDir); err != nil {
			// LoadOne failing due to missing required config (e.g. bot_token) is
			// expected on first install — the plugin is installed but not yet
			// configured. Log a warning and continue; the operator must configure
			// the plugin via the admin UI before it becomes active.
			slog.Warn("Plugin installed but not yet configured (LoadOne failed, likely missing required fields)",
				"plugin", name, "error", err)
		}
	}

	if err := s.reconfigureIntegration(r.Context(), mgr, name); err != nil {
		// Plugin is already written to disk and registered in settings.yaml.
		// Returning 500 would leave an inconsistent state (installed on disk,
		// error reported to client). Treat as a non-fatal warning; the operator
		// can reconfigure via the admin UI.
		slog.Warn("Plugin installed but initial reconfigure failed", "plugin", name, "error", err)
	}

	// Wire the newly installed plugin into the FanOut so it receives
	// Subscribe() calls (and can start its gateway).
	s.ensureBrokerSpoke(mgr, name)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleInstallSelfManaged implements the install flow for self-managed plugins
// (e.g. A2A bridge): create the Hub-side admin config file, register in
// settings.yaml with self_managed/mode/address/config_file, attempt LoadOne
// (non-fatal if the bridge process is not running), and return setup
// instructions.
func (s *Server) handleInstallSelfManaged(w http.ResponseWriter, r *http.Request, mgr IntegrationManager, kp *KnownPlugin) {
	name := kp.Name

	// Also reject if already registered in settings.yaml but not loaded.
	if entry := installedPluginSettingsEntry(name); entry != nil {
		BadRequest(w, "integration is already installed")
		return
	}

	// 1. Create the Hub-side flat admin config file (if absent).
	adminConfigPath := "~/.scion/scion-" + name + "-admin.yaml"
	resolvedAdminPath, err := resolveTilde(adminConfigPath)
	if err != nil {
		slog.Error("Failed to resolve admin config path", "plugin", name, "error", err)
		InternalError(w)
		return
	}
	if _, err := os.Stat(resolvedAdminPath); err != nil {
		if !os.IsNotExist(err) {
			slog.Error("Failed to check admin config file", "plugin", name, "path", resolvedAdminPath, "error", err)
			InternalError(w)
			return
		}
		if err := createSelfManagedAdminConfig(name, adminConfigPath); err != nil {
			slog.Error("Failed to create admin config file", "plugin", name, "error", err)
			InternalError(w)
			return
		}
	} else {
		slog.Info("Admin config file already exists, preserving", "plugin", name, "path", resolvedAdminPath)
	}

	// 2. Create bridge bootstrap config template (if absent).
	bridgeConfigPath := "~/.scion/scion-" + name + ".yaml"
	resolvedBridgePath, err := resolveTilde(bridgeConfigPath)
	if err != nil {
		slog.Error("Failed to resolve bridge config path", "plugin", name, "error", err)
		InternalError(w)
		return
	}
	if _, err := os.Stat(resolvedBridgePath); err != nil {
		if !os.IsNotExist(err) {
			slog.Error("Failed to check bridge config file", "plugin", name, "path", resolvedBridgePath, "error", err)
			InternalError(w)
			return
		}
		if err := createBridgeConfigTemplate(name, bridgeConfigPath, s.config.HubEndpoint); err != nil {
			slog.Error("Failed to create bridge config template", "plugin", name, "error", err)
			InternalError(w)
			return
		}
	} else {
		slog.Info("Bridge config file already exists, preserving", "plugin", name, "path", resolvedBridgePath)
	}

	// 3. Register in settings.yaml with self-managed fields and broker type
	//    in a single read-modify-write cycle.
	SettingsWriteMu.Lock()
	err = config.AddSelfManagedPluginWithBrokerType(config.SelfManagedPluginEntry{
		Name:       name,
		Address:    "localhost:9090",
		ConfigFile: adminConfigPath,
	})
	SettingsWriteMu.Unlock()
	if err != nil {
		slog.Error("Failed to add self-managed plugin to settings.yaml", "plugin", name, "error", err)
		InternalError(w)
		return
	}

	// 4. Attempt LoadOne — non-fatal if the bridge is not running.
	pluginsDir, err := plugin.DefaultPluginsDir()
	if err != nil {
		slog.Warn("Failed to resolve plugins directory, continuing without it", "plugin", name, "error", err)
	}
	if err := mgr.LoadOne(plugin.PluginTypeBroker, name, plugin.PluginEntry{
		SelfManaged: true,
		Mode:        "self-managed",
		Address:     "localhost:9090",
		ConfigFile:  adminConfigPath,
	}, pluginsDir); err != nil {
		slog.Warn("Self-managed plugin installed but bridge not reachable (expected for fresh installs)",
			"plugin", name, "error", err)
	} else {
		// If LoadOne succeeded, wire up the spoke and reconfigure.
		if err := s.reconfigureIntegration(r.Context(), mgr, name); err != nil {
			slog.Warn("Self-managed plugin reconfigure failed after LoadOne", "plugin", name, "error", err)
		}
		s.ensureBrokerSpoke(mgr, name)
	}

	// 5. Return setup instructions.
	configFile := "~/.scion/scion-" + name + ".yaml"
	startCommand := kp.BinaryName + " -config " + configFile

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "installed",
		"setup": map[string]string{
			"binary":        kp.BinaryName,
			"config_file":   configFile,
			"start_command": startCommand,
			"notes":         "Start the bridge process, then click Reconnect to activate.",
		},
	})
}

// createSelfManagedAdminConfig creates a default Hub-side flat admin config
// file for a self-managed plugin.
func createSelfManagedAdminConfig(pluginName, configFilePath string) error {
	resolved, err := resolveTilde(configFilePath)
	if err != nil {
		return err
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	content := "# Scion admin configuration for " + pluginName + "\n"
	switch pluginName {
	case "a2a-bridge":
		content += "external_url: \"\"\n"
		content += "auth_scheme: \"none\"\n"
		content += "uat_cache_ttl: \"60s\"\n"
		content += "rate_limit_enabled: \"false\"\n"
		content += "rate_limit_rps: \"10\"\n"
		content += "rate_limit_burst: \"20\"\n"
		content += "send_message_timeout: \"120s\"\n"
		content += "sse_keepalive: \"30s\"\n"
		content += "push_retry_max: \"3\"\n"
		content += "provider_org: \"\"\n"
		content += "provider_url: \"\"\n"
		content += "projects_json: \"[]\"\n"
	case "chat-app":
		content += "project_id: \"\"\n"
		content += "credentials: \"\"\n"
		content += "listen_address: \":8443\"\n"
		content += "external_url: \"\"\n"
		content += "service_account_email: \"\"\n"
	}

	return os.WriteFile(resolved, []byte(content), 0600)
}

// createBridgeConfigTemplate creates a bridge bootstrap config template file
// pre-filled with the Hub endpoint and default plugin listen address. The
// template gives operators a working starting point for the bridge's own
// nested YAML configuration.
func createBridgeConfigTemplate(name, configFilePath, hubEndpoint string) error {
	resolved, err := resolveTilde(configFilePath)
	if err != nil {
		return err
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	var content string
	switch name {
	case "chat-app":
		content = "# Scion Google Chat app bootstrap configuration\n"
		content += "# This file is operator-managed. Edit it to configure listen addresses,\n"
		content += "# GCP credentials, and state database settings.\n"
		content += "hub:\n"
		content += "  endpoint: \"" + hubEndpoint + "\"\n"
		content += "plugin:\n"
		content += "  listen_address: \"localhost:9090\"\n"
		content += "platforms:\n"
		content += "  google_chat:\n"
		content += "    enabled: true\n"
		content += "    project_id: \"\"\n"
		content += "    credentials: \"\"\n"
		content += "    listen_address: \":8443\"\n"
		content += "    external_url: \"\"\n"
		content += "    service_account_email: \"\"\n"
		content += "# state:\n"
		content += "#   database: \"~/.scion/scion-chat-app.db\"\n"
		content += "# logging:\n"
		content += "#   level: \"info\"\n"
	default:
		content = "# Scion A2A bridge bootstrap configuration\n"
		content += "# This file is operator-managed. Edit it to configure listen addresses,\n"
		content += "# TLS, state database, and signing key settings.\n"
		content += "hub:\n"
		content += "  endpoint: \"" + hubEndpoint + "\"\n"
		content += "plugin:\n"
		content += "  listen_address: \"localhost:9090\"\n"
		content += "# bridge:\n"
		content += "#   listen_address: \":8081\"\n"
		content += "# state:\n"
		content += "#   database: \"~/.scion/scion-a2a-bridge.db\"\n"
		content += "# logging:\n"
		content += "#   level: \"info\"\n"
	}

	return os.WriteFile(resolved, []byte(content), 0600)
}

// handleRebuildSelfManaged builds a self-managed plugin binary from source
// (dev mode only). Unlike regular plugin updates, this does NOT restart the
// bridge process — the response instructs the operator to restart manually.
func (s *Server) handleRebuildSelfManaged(w http.ResponseWriter, _ *http.Request, kp *KnownPlugin, repoPath string) {
	sourceDir := filepath.Join(repoPath, kp.SourceDir)
	if _, err := os.Stat(sourceDir); err != nil {
		NotFound(w, "plugin source directory")
		return
	}

	mu := acquirePluginBuildLock(kp.Name)
	if mu == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "a build is already in progress for this integration",
		})
		return
	}
	defer releasePluginBuildLock(kp.Name)

	// Build the binary into the repo's bin/ directory using the same pattern
	// as the Makefile target: go build -o bin/<binary> ./<source>/cmd/<binary>/
	binDir := filepath.Join(repoPath, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		slog.Error("Failed to create bin directory", "path", binDir, "error", err)
		InternalError(w)
		return
	}

	binaryPath := filepath.Join(binDir, kp.BinaryName)
	buildPkg := "./" + kp.SourceDir + "/cmd/" + kp.BinaryName + "/"

	slog.Info("Rebuilding self-managed plugin binary",
		"plugin", kp.Name, "source", sourceDir, "binary", binaryPath)

	buildCmd := exec.Command("go", "build", "-o", binaryPath, buildPkg)
	buildCmd.Dir = repoPath
	output, err := buildCmd.CombinedOutput()
	if err != nil {
		slog.Error("Build failed for self-managed plugin", "plugin", kp.Name, "error", err, "output", string(output))
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Build failed — check server logs for details", nil)
		return
	}

	slog.Info("Self-managed plugin binary rebuilt successfully",
		"plugin", kp.Name, "binary", binaryPath)

	// Ensure the plugin is listed in message_broker.types on disk so it
	// survives future restarts/rebuilds that re-read settings.yaml.
	SettingsWriteMu.Lock()
	if err := config.AddPluginToMessageBrokerTypes(kp.Name); err != nil {
		slog.Warn("Failed to ensure plugin in message_broker.types", "plugin", kp.Name, "error", err)
		// Non-fatal — continue with the rebuild response.
	}
	SettingsWriteMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "rebuilt",
		"binary_path": binaryPath,
		"notes":       "Binary rebuilt successfully. Restart the bridge process to use the new binary.",
	})
}

func (s *Server) handleListAvailableIntegrations(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	repoPath := s.config.MaintenanceConfig.RepoPath

	// Load settings.yaml once so we can skip plugins that are registered there
	// (installed but unconfigured — LoadOne failed so they're not in mgr).
	var settingsBroker map[string]struct{}
	if globalDir, err := config.GetGlobalDir(); err == nil {
		if vs, err := config.LoadSingleFileVersioned(globalDir); err == nil {
			if vs.Server != nil && vs.Server.Plugins != nil {
				settingsBroker = make(map[string]struct{}, len(vs.Server.Plugins.Broker))
				for k := range vs.Server.Plugins.Broker {
					settingsBroker[k] = struct{}{}
				}
			}
		}
	}

	var available []AvailableIntegration
	for _, kp := range knownPluginCatalog {
		if mgr != nil && mgr.HasPlugin("broker", kp.Name) {
			continue
		}
		// Also skip if registered in settings.yaml (installed but not yet loaded).
		if _, ok := settingsBroker[kp.Name]; ok {
			continue
		}

		// Check 1: binary is on $PATH (covers Homebrew / package-manager installs).
		_, onPathErr := exec.LookPath(kp.BinaryName)

		// Check 2: source checkout exists (covers development installs).
		inSourceCheckout := false
		if repoPath != "" {
			sourceDir := filepath.Join(repoPath, kp.SourceDir)
			if _, err := os.Stat(sourceDir); err == nil {
				inSourceCheckout = true
			}
		}

		if onPathErr != nil && !inSourceCheckout {
			continue // neither binary on PATH nor source checkout found
		}

		available = append(available, AvailableIntegration{
			Name:        kp.Name,
			Platform:    kp.Platform,
			Description: kp.Description,
		})
	}

	if available == nil {
		available = []AvailableIntegration{}
	}
	writeJSON(w, http.StatusOK, available)
}

// --- Helpers ---

// resolveTilde expands a leading "~/" in path to the user's home directory.
// If the path does not start with "~/", it is returned unchanged.
func resolveTilde(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, path[2:]), nil
}

// installedPluginSettingsEntry returns the settings.yaml broker entry for the
// named plugin, or nil if the plugin is not registered there. Used as the
// fallback identity check for plugins that are installed (present in
// settings.yaml) but not loaded into the manager (LoadOne failed because
// required fields were missing).
func installedPluginSettingsEntry(name string) *config.V1PluginEntry {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil
	}
	vs, err := config.LoadSingleFileVersioned(globalDir)
	if err != nil || vs.Server == nil || vs.Server.Plugins == nil {
		return nil
	}
	entry, ok := vs.Server.Plugins.Broker[name]
	if !ok {
		return nil
	}
	return &entry
}

// activateInstalledIntegration loads a plugin that is registered in
// settings.yaml but not yet running. It builds the same fully-resolved config
// map that server startup builds — file settings, secret-backend secrets, and
// hub wiring credentials — so the plugin's Configure call succeeds on load.
func (s *Server) activateInstalledIntegration(ctx context.Context, mgr IntegrationManager, name string, entry *config.V1PluginEntry) error {
	// Every current caller checks entry before calling, but the signature
	// accepts a pointer, so fail loudly rather than panicking deep inside the
	// activation sequence if a future one forgets.
	if entry == nil {
		return fmt.Errorf("cannot activate integration %q: entry is nil", name)
	}

	// Migrate raw credentials into the secret backend before
	// ResolvePluginConfig strips them, mirroring what the server boot path
	// does in initPluginManager. Without this, a plugin installed and
	// activated without a restart loses any secret its operator put in
	// settings.yaml. A nil backend is a no-op.
	secretmigration.MigratePluginSecrets(ctx, s.GetSecretBackend(), name, entry.Config, entry.ConfigFile)

	merged, err := config.ResolvePluginConfig(entry.ConfigFile, entry.Config)
	if err != nil {
		slog.Warn("Failed to resolve config file for plugin activation", "plugin", name, "error", err)
		merged = make(map[string]string)
	}

	for _, m := range config.PluginSecretKeyMap[name] {
		if merged[m.ConfigKey] != "" {
			continue
		}
		if val, err := s.LoadChatIntegrationSecret(ctx, m.SecretKey); err == nil && val != "" {
			merged[m.ConfigKey] = val
		}
	}

	for k, v := range s.getPluginHubCreds(ctx, name) {
		if v != "" {
			merged[k] = v
		}
	}

	pluginsDir, err := plugin.DefaultPluginsDir()
	if err != nil {
		return err
	}

	return mgr.LoadOne(plugin.PluginTypeBroker, name, plugin.PluginEntry{
		Path:          entry.Path,
		Config:        merged,
		ConfigFile:    entry.ConfigFile,
		SelfManaged:   entry.SelfManaged,
		Mode:          entry.Mode,
		Address:       entry.Address,
		TLSCertFile:   entry.TLSCertFile,
		TLSKeyFile:    entry.TLSKeyFile,
		TLSCAFile:     entry.TLSCAFile,
		TLSSkipVerify: entry.TLSSkipVerify,
	}, pluginsDir)
}

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
	case "slack":
		return "slack"
	case "chat-app":
		return "gchat"
	case "a2a-bridge":
		return "a2a"
	case "teams":
		return "teams"
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
		"bot_id":      true,
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

// getPluginHubCreds reconstructs the hub wiring credentials for a broker
// plugin from live, authoritative sources — the same values that
// server_foreground.go injects via ConfigureBroker at startup. This avoids
// relying on the plugin manager's config map, which may be stale or empty
// after a process restart.
func (s *Server) getPluginHubCreds(ctx context.Context, name string) map[string]string {
	creds := make(map[string]string)

	// hub_url: from the server config (same value that startup resolves).
	if s.config.HubEndpoint != "" {
		creds["hub_url"] = s.config.HubEndpoint
	}

	// broker_id: deterministic UUIDv5 — same namespace and seed as startup.
	legacyID := "plugin-broker-" + name
	brokerID := uuid.NewSHA1(pluginBrokerNS, []byte(legacyID)).String()
	creds["broker_id"] = brokerID

	// hmac_key: from BrokerAuthService (idempotent — returns existing key).
	if authSvc := s.GetBrokerAuthService(); authSvc != nil {
		if secretKey, err := authSvc.GenerateAndStoreSecret(ctx, brokerID); err != nil {
			slog.Error("Failed to generate or retrieve HMAC key", "plugin", name, "error", err)
		} else {
			creds["hmac_key"] = secretKey
		}
	}

	// plugin_name: the plugin's own name.
	creds["plugin_name"] = name

	// project_slug_map: from the store.
	if s.store != nil {
		if projects, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500}); err != nil {
			slog.Warn("Failed to list projects for plugin slug map", "plugin", name, "error", err)
		} else {
			slugMap := make(map[string]string, len(projects.Items))
			for _, p := range projects.Items {
				if p.Slug != "" {
					slugMap[p.ID] = p.Slug
				} else {
					slugMap[p.ID] = p.Name
				}
			}
			if jsonBytes, err := json.Marshal(slugMap); err == nil {
				creds["project_slug_map"] = string(jsonBytes)
			}
		}
	}

	// database_driver + database_url: only for Postgres backends.
	if s.dbDriver != "" && s.dbDriver != "sqlite" {
		creds["database_driver"] = s.dbDriver
		creds["database_url"] = s.databaseDSN
	}

	return creds
}

// resolveIntegrationMergedConfig builds the full merged config map for a plugin
// by reading the config file, injecting secrets from the secret backend,
// carrying over runtime keys, and applying hub wiring credentials. The returned
// map is ready to be pushed to the plugin via ReplaceBrokerConfig or
// RestartBrokerPlugin.
func (s *Server) resolveIntegrationMergedConfig(ctx context.Context, mgr IntegrationManager, name string) map[string]string {
	pluginCfg := mgr.GetPluginConfig("broker", name)

	// Re-read config file if one is configured. Prefer the immutable
	// configFiles store over the mutable runtime config map.
	configFile := mgr.GetPluginConfigFile("broker", name)
	if configFile == "" && pluginCfg != nil {
		configFile = pluginCfg["config_file"]
	}

	// When a config file is set, resolve from file only — do NOT pass the
	// manager's boot-resolved map as "inline" config, because it contains
	// the file's own keys and would trigger spurious deprecation warnings (B2).
	// When no config file exists, pass the manager map as inline config so
	// that inline-only deployments retain their configuration keys.
	var inlineToPass map[string]string
	if configFile == "" {
		inlineToPass = pluginCfg
	}
	merged, err := config.ResolvePluginConfig(configFile, inlineToPass)
	if err != nil {
		slog.Error("Failed to resolve config for integration", "plugin", name, "error", err)
		merged = make(map[string]string)
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

	// Carry over non-hub runtime keys (config_file, mode, path, address,
	// TLS) from the manager's cached config as underlay.
	for k, v := range pluginCfg {
		if reconfigureRuntimeKeys[k] && !hubKeys[k] && v != "" {
			if merged[k] == "" {
				merged[k] = v
			}
		}
	}

	// Reconstruct hub wiring credentials from authoritative live sources
	// and apply as OVERRIDE. The manager's cached config may be stale or
	// empty (e.g. after process restart), so we recompute these values the
	// same way server_foreground.go does at startup.
	hubCreds := s.getPluginHubCreds(ctx, name)
	for k, v := range hubCreds {
		if v != "" {
			merged[k] = v
		}
	}

	// Preserve the config_file path in the merged map so that
	// ReplaceBrokerConfig doesn't overwrite dp.Config with a map that
	// lacks the key, which would cause subsequent reloads to lose it.
	if configFile != "" {
		merged["config_file"] = configFile
	}

	return merged
}

// reconfigureIntegration reloads config for a plugin and calls ReplaceBrokerConfig
// to push new config to the running process without restarting it.
// For self-managed plugins, it falls back to Reconnect on failure.
func (s *Server) reconfigureIntegration(ctx context.Context, mgr IntegrationManager, name string) error {
	merged := s.resolveIntegrationMergedConfig(ctx, mgr, name)

	if err := mgr.ReplaceBrokerConfig(name, merged); err != nil {
		if mgr.IsSelfManaged("broker", name) {
			slog.Warn("ReplaceBrokerConfig failed for self-managed plugin, trying Reconnect",
				"plugin", name, "error", err)
			return mgr.Reconnect("broker", name)
		}
		return err
	}

	return nil
}

// ensureBrokerSpoke adds the plugin as a FanOut spoke if it is not already
// registered. Unlike refreshBrokerSpoke it never calls ReplaceSpoke, so it
// is safe to call on a running plugin — existing connections are preserved.
// This fills the gap where handleInstallIntegration, handleRestartIntegration,
// and handleUpdateIntegrationConfig (loaded-branch) all call
// reconfigureIntegration but never wire the plugin into the FanOut, causing
// Subscribe() (and therefore startGateway()) to never fire.
func (s *Server) ensureBrokerSpoke(mgr IntegrationManager, name string) {
	proxy := s.GetMessageBrokerProxy()
	if proxy == nil {
		return
	}
	fanout, ok := proxy.bus.(*eventbus.FanOutEventBus)
	if !ok {
		return
	}
	if fanout.HasSpoke(name) {
		return // already wired — nothing to do
	}

	newBus, err := mgr.GetBroker(name)
	if err != nil {
		slog.Warn("ensureBrokerSpoke: cannot get broker, skipping spoke add",
			"plugin", name, "error", err)
		return
	}
	if newBus == nil {
		slog.Warn("ensureBrokerSpoke: broker is nil, skipping spoke add", "plugin", name)
		return
	}

	var observer bool
	var channelID string
	if _, cID, caps, infoErr := mgr.BrokerInfo(name); infoErr == nil {
		channelID = cID
		for _, cap := range caps {
			if strings.EqualFold(cap, "observer") {
				observer = true
				break
			}
		}
	}

	spoke := eventbus.NamedEventBus{
		Name:      name,
		Bus:       newBus,
		Observer:  observer,
		ChannelID: channelID,
	}
	if err := fanout.AddSpoke(spoke); err != nil {
		slog.Error("ensureBrokerSpoke: failed to add spoke", "plugin", name, "error", err)
	} else {
		slog.Info("Wired broker plugin as FanOut spoke after admin operation", "plugin", name)
	}
}

// refreshBrokerSpoke replaces the stale fan-out eventbus spoke for a broker
// plugin after its process has been restarted (e.g. UpdatePlugin). The old
// spoke wraps a dead RPC connection; this swaps in a fresh one.
func (s *Server) refreshBrokerSpoke(mgr IntegrationManager, name string) {
	proxy := s.GetMessageBrokerProxy()
	if proxy == nil {
		return
	}
	fanout, ok := proxy.bus.(*eventbus.FanOutEventBus)
	if !ok {
		return
	}

	newBus, err := mgr.GetBroker(name)
	if err != nil {
		slog.Error("Failed to get broker for spoke refresh", "plugin", name, "error", err)
		return
	}

	var observer bool
	var channelID string
	if _, cID, caps, infoErr := mgr.BrokerInfo(name); infoErr == nil {
		channelID = cID
		for _, cap := range caps {
			if strings.EqualFold(cap, "observer") {
				observer = true
				break
			}
		}
	}

	spoke := eventbus.NamedEventBus{
		Name:      name,
		Bus:       newBus,
		Observer:  observer,
		ChannelID: channelID,
	}
	if err := fanout.ReplaceSpoke(name, spoke); err != nil {
		// No existing spoke — the plugin was activated after startup (e.g. it
		// failed to load at boot and was configured via the admin UI). Add it.
		if addErr := fanout.AddSpoke(spoke); addErr != nil {
			slog.Error("Failed to replace or add broker spoke", "plugin", name,
				"replace_error", err, "add_error", addErr)
		} else {
			slog.Info("Added broker spoke for newly activated plugin", "plugin", name)
		}
	} else {
		slog.Info("Replaced broker spoke after plugin restart", "plugin", name)
	}
}

// requirePostgres is a feature gate for Mode 3 (HA) endpoints that require
// Postgres. Returns true if the check passed and the handler can proceed.
// Returns false if the response has been written (409).
func (s *Server) requirePostgres(w http.ResponseWriter) bool {
	if s.IsPostgres() {
		return true
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": "Mode 3 (HA) features require PostgreSQL. Configure database.driver: postgres in settings.",
	})
	return false
}

// isHAIntegration reports whether the named integration is running in HA
// (Mode 3) deployment mode.
func (s *Server) isHAIntegration(mgr IntegrationManager, name string) bool {
	return mgr.GetDeploymentMode("broker", name) == plugin.DeploymentModeHA
}

// handleGetUpdateStatus handles GET .../update/{id} and GET .../update/latest.
func (s *Server) handleGetUpdateStatus(w http.ResponseWriter, r *http.Request, name, updateRef string) {
	if !s.requirePostgres(w) {
		return
	}

	client := s.entClient
	if client == nil {
		InternalError(w)
		return
	}

	ctx := r.Context()
	var row *ent.IntegrationUpdate
	var err error

	if updateRef == "latest" {
		row, err = client.IntegrationUpdate.
			Query().
			Where(integrationupdate.IntegrationEQ(name)).
			Order(integrationupdate.ByCreateTime(entsql.OrderDesc())).
			First(ctx)
	} else {
		uid, parseErr := uuid.Parse(updateRef)
		if parseErr != nil {
			BadRequest(w, "invalid update id")
			return
		}
		row, err = client.IntegrationUpdate.
			Query().
			Where(
				integrationupdate.IDEQ(uid),
				integrationupdate.IntegrationEQ(name),
			).
			Only(ctx)
	}

	if err != nil {
		if ent.IsNotFound(err) {
			NotFound(w, "update")
			return
		}
		slog.Error("Failed to query integration update", "integration", name, "ref", updateRef, "error", err)
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, integrationUpdateToResponse(row))
}

func integrationUpdateToResponse(row *ent.IntegrationUpdate) IntegrationUpdateResponse {
	return IntegrationUpdateResponse{
		ID:          row.ID.String(),
		Integration: row.Integration,
		State:       string(row.State),
		Detail:      row.Detail,
		NewVersion:  row.NewVersion,
		RequestedBy: row.RequestedBy,
		CreatedAt:   row.CreateTime.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   row.UpdateTime.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// handleUpdateIntegrationHA handles POST .../update for HA integrations.
// It inserts an integration_updates row in "requested" state, fires a NOTIFY,
// and returns 202 with the update_id for polling.
func (s *Server) handleUpdateIntegrationHA(w http.ResponseWriter, r *http.Request, name string) {
	if !s.requirePostgres(w) {
		return
	}

	client := s.entClient
	if client == nil {
		InternalError(w)
		return
	}

	// Reject if a non-terminal update already exists for this integration.
	ctx := r.Context()
	existingCount, err := client.IntegrationUpdate.Query().
		Where(
			integrationupdate.IntegrationEQ(name),
			integrationupdate.StateNotIn(
				integrationupdate.StateCompleted,
				integrationupdate.StateFailed,
			),
		).
		Count(ctx)
	if err != nil {
		slog.Error("Failed to check for pending updates", "integration", name, "error", err)
		InternalError(w)
		return
	}
	if existingCount > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "An update is already in progress for this integration",
		})
		return
	}

	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	// Capture pre-update version so completion detection can compare.
	preUpdateVersion := ""
	if mgr != nil {
		if version, _, _, err := mgr.BrokerInfo(name); err == nil {
			preUpdateVersion = version
		}
	}

	user := GetUserIdentityFromContext(ctx)
	requestedBy := ""
	if user != nil {
		requestedBy = user.ID()
	}

	tx, err := client.Tx(ctx)
	if err != nil {
		slog.Error("Failed to begin transaction for integration update", "integration", name, "error", err)
		InternalError(w)
		return
	}
	defer func() { _ = tx.Rollback() }()

	create := tx.IntegrationUpdate.
		Create().
		SetIntegration(name).
		SetState(integrationupdate.StateRequested).
		SetRequestedBy(requestedBy)
	if preUpdateVersion != "" {
		create = create.SetDetail("pre_update_version=" + preUpdateVersion)
	}

	row, err := create.Save(ctx)
	if err != nil {
		slog.Error("Failed to create integration update request", "integration", name, "error", err)
		InternalError(w)
		return
	}

	signal := AdminSignal{
		Integration: name,
		Kind:        "update",
		ID:          row.ID.String(),
	}
	if err := publishAdminSignalTx(ctx, tx, signal); err != nil {
		slog.Warn("Failed to NOTIFY integration update", "integration", name, "error", err)
	}

	if err := tx.Commit(); err != nil {
		slog.Error("Failed to commit integration update transaction", "integration", name, "error", err)
		InternalError(w)
		return
	}

	// Start poll-based completion detection.
	s.startUpdateTracking(name, row.ID.String(), preUpdateVersion)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"update_id": row.ID.String(),
	})
}
