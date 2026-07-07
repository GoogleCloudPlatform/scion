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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/v2"
)

// sectionState holds the cached value and revision for a single section.
type sectionState struct {
	Value    json.RawMessage
	Revision int64
}

// Layer1Snapshot is an immutable merged view of all Layer-1 operational settings.
// It carries everything that reloadSettings() currently derives from the config
// file, so that applySnapshot can populate ServerConfig + MaintenanceState
// without re-reading any external source.
type Layer1Snapshot struct {
	// Access
	AdminEmails       []string
	UserAccessMode    string
	AuthorizedDomains []string

	// Lifecycle
	AutoSuspendStalled    bool
	SoftDeleteRetention   string
	SoftDeleteRetainFiles bool

	// Maintenance
	AdminMode          bool
	MaintenanceMessage string

	// Telemetry
	TelemetryEnabled *bool
	TelemetryConfig  *config.V1TelemetryConfig

	// Agent defaults
	DefaultTemplate      string
	DefaultHarnessConfig string
	DefaultMaxTurns      int
	DefaultMaxModelCalls int
	DefaultMaxDuration   string
	DefaultResources     *api.ResourceSpec

	// Endpoints
	PublicURL     string
	ImageRegistry string

	// GitHub App (non-secret fields only)
	GitHubAppID           int64
	GitHubAPIBaseURL      string
	GitHubWebhooksEnabled bool
	GitHubInstallationURL string
	GitHubPrivateKeyPath  string

	// Notifications
	NotificationChannels []config.V1NotificationChannelConfig

	// EnvOverrides lists Layer-1 koanf keys that are overridden by env vars
	// on this node — used for drift warnings.
	EnvOverrides []string
}

// OperationalSettings is the runtime component that merges file, DB, and env
// sources into a single Layer-1 view per §3.5 of the settings-db design.
//
// It is owned by the Server and used only when database.driver == "postgres".
// In file/SQLite mode the legacy reloadSettings path is used instead.
type OperationalSettings struct {
	store        store.HubSettingStore
	fileFallback *koanf.Koanf    // Layer-1 keys from settings.yaml + defaults (file values only, never env)
	envOverrides map[string]bool // Layer-1 koanf keys satisfied by env
	envKoanf     *koanf.Koanf    // env-only koanf for merge
	mu           sync.RWMutex
	cache        map[string]sectionState // section name → cached value + revision
}

// NewOperationalSettings creates a new OperationalSettings service.
//
// fileFallback is a koanf instance loaded from settings.yaml (file values only,
// no env overlay) representing the Layer-1 fallback when a section is absent
// from the DB. envKoanf is a koanf instance containing only SCION_SERVER_*
// environment variable keys for the precedence merge.
func NewOperationalSettings(
	st store.HubSettingStore,
	fileFallback *koanf.Koanf,
	envKoanf *koanf.Koanf,
) *OperationalSettings {
	envOverrides := make(map[string]bool)
	for _, key := range opsettings.DetectEnvOverrides(envKoanf) {
		envOverrides[key] = true
	}

	return &OperationalSettings{
		store:        st,
		fileFallback: fileFallback,
		envOverrides: envOverrides,
		envKoanf:     envKoanf,
		cache:        make(map[string]sectionState),
	}
}

// Refresh re-reads all hub_settings rows from the store, diffs revisions
// against the cache, and returns the names of sections that changed.
func (o *OperationalSettings) Refresh(ctx context.Context) ([]string, error) {
	rows, err := o.store.ListHubSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("operational settings refresh: %w", err)
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	var changed []string
	seen := make(map[string]bool, len(rows))

	for _, row := range rows {
		if row.Section == "_meta" {
			continue
		}
		seen[row.Section] = true

		prev, exists := o.cache[row.Section]
		if !exists || prev.Revision != row.Revision {
			changed = append(changed, row.Section)
		}
		o.cache[row.Section] = sectionState{
			Value:    row.Value,
			Revision: row.Revision,
		}
	}

	// Detect deleted sections (in cache but not in DB).
	for name := range o.cache {
		if !seen[name] {
			changed = append(changed, name)
			delete(o.cache, name)
		}
	}

	return changed, nil
}

// Snapshot returns an immutable merged Layer-1 view.
//
// Precedence (per §3.4):
//
//	env > DB > file > defaults
//
// Absent-vs-empty: a DB row present for a section fully owns that section
// (its omitted fields fall to compiled defaults, not to the file). A deleted
// row restores file fallback for that section.
func (o *OperationalSettings) Snapshot() Layer1Snapshot {
	o.mu.RLock()
	dbSections := make(map[string]json.RawMessage, len(o.cache))
	for name, ss := range o.cache {
		dbSections[name] = ss.Value
	}
	o.mu.RUnlock()

	// Build the merged koanf: start with file fallback, overlay DB sections,
	// then overlay env.
	merged := koanf.New(".")

	// Layer: file fallback (lowest precedence for Layer-1 keys).
	// Only load keys for sections NOT present in DB.
	for _, sec := range opsettings.Registry {
		if _, inDB := dbSections[sec.Name]; inDB {
			continue // DB fully owns this section
		}
		if len(sec.KoanfPaths) == 0 {
			continue
		}
		// Extract this section from the file fallback and load it.
		doc, err := opsettings.ExtractSectionFromKoanf(o.fileFallback, sec.Name)
		if err != nil {
			continue
		}
		_ = loadSectionDocIntoKoanf(merged, sec.Name, doc)
	}

	// Layer: DB sections.
	for name, doc := range dbSections {
		_ = loadSectionDocIntoKoanf(merged, name, doc)
	}

	// Layer: env overrides (highest precedence).
	if o.envKoanf != nil {
		for _, key := range o.envKoanf.Keys() {
			if opsettings.IsLayer1Key(key) {
				_ = merged.Set(key, o.envKoanf.Get(key))
			}
		}
	}

	snap := buildSnapshotFromKoanf(merged)

	// Populate env overrides list.
	overrides := make([]string, 0, len(o.envOverrides))
	for key := range o.envOverrides {
		overrides = append(overrides, key)
	}
	snap.EnvOverrides = overrides

	// For maintenance, pull from DB section directly (not koanf — it has no
	// koanf paths). If no DB row exists, compiled defaults apply.
	snap.AdminMode, snap.MaintenanceMessage = o.maintenanceFromCache(dbSections)

	return snap
}

// maintenanceFromCache extracts maintenance settings from the DB section
// document, falling back to compiled defaults if absent.
func (o *OperationalSettings) maintenanceFromCache(dbSections map[string]json.RawMessage) (adminMode bool, message string) {
	doc, ok := dbSections["maintenance"]
	if !ok {
		return false, ""
	}
	var ms opsettings.MaintenanceSettings
	if err := json.Unmarshal(doc, &ms); err != nil {
		return false, ""
	}
	return ms.AdminMode, ms.MaintenanceMessage
}

// Update validates the section document, upserts it via the store, and
// refreshes the local cache. Returns the new revision.
func (o *OperationalSettings) Update(
	ctx context.Context,
	section string,
	doc json.RawMessage,
	updatedBy string,
	expectedRevision int64,
) (int64, error) {
	// Validate via opsettings registry.
	if errs := opsettings.Validate(section, doc); len(errs) > 0 {
		return 0, fmt.Errorf("validation failed for section %q: %v", section, errs)
	}

	result, err := o.store.UpsertHubSetting(ctx, section, doc, updatedBy, expectedRevision)
	if err != nil {
		return 0, err
	}

	// Update local cache.
	o.mu.Lock()
	o.cache[section] = sectionState{
		Value:    result.Value,
		Revision: result.Revision,
	}
	o.mu.Unlock()

	// TODO(phase4): publish admin.settings.updated event with {section, revision}
	// to propagate the change to other replicas via PostgresEventPublisher.

	return result.Revision, nil
}

// EnvOverriddenKeys returns the list of Layer-1 koanf keys that are overridden
// by environment variables on this node.
func (o *OperationalSettings) EnvOverriddenKeys() []string {
	keys := make([]string, 0, len(o.envOverrides))
	for key := range o.envOverrides {
		keys = append(keys, key)
	}
	return keys
}

// loadSectionDocIntoKoanf loads a single section's JSON document into the
// koanf instance using the opsettings koanf integration.
func loadSectionDocIntoKoanf(k *koanf.Koanf, sectionName string, doc json.RawMessage) error {
	sections := map[string]json.RawMessage{sectionName: doc}
	return opsettings.MergeSectionsIntoKoanf(k, sections)
}

// buildSnapshotFromKoanf constructs a Layer1Snapshot by reading koanf keys
// corresponding to each section. This covers all sections that have koanf
// paths (i.e. everything except maintenance, which is handled separately).
func buildSnapshotFromKoanf(k *koanf.Koanf) Layer1Snapshot {
	snap := Layer1Snapshot{}

	// Access
	snap.AdminEmails = k.Strings("server.hub.admin_emails")
	snap.UserAccessMode = k.String("server.auth.user_access_mode")
	snap.AuthorizedDomains = k.Strings("server.auth.authorized_domains")

	// Lifecycle
	snap.AutoSuspendStalled = k.Bool("server.hub.auto_suspend_stalled")
	snap.SoftDeleteRetention = k.String("server.hub.soft_delete_retention")
	snap.SoftDeleteRetainFiles = k.Bool("server.hub.soft_delete_retain_files")

	// Telemetry — extract via the section struct for full fidelity.
	if k.Exists("telemetry.enabled") {
		v := k.Bool("telemetry.enabled")
		snap.TelemetryEnabled = &v
	}
	teleSub := k.Cut("telemetry")
	if teleSub != nil && len(teleSub.Keys()) > 0 {
		data, err := json.Marshal(teleSub.Raw())
		if err == nil {
			var tc config.V1TelemetryConfig
			if json.Unmarshal(data, &tc) == nil {
				snap.TelemetryConfig = &tc
			}
		}
	}

	// Agent defaults
	snap.DefaultTemplate = k.String("default_template")
	snap.DefaultHarnessConfig = k.String("default_harness_config")
	snap.DefaultMaxTurns = k.Int("default_max_turns")
	snap.DefaultMaxModelCalls = k.Int("default_max_model_calls")
	snap.DefaultMaxDuration = k.String("default_max_duration")
	if k.Exists("default_resources") {
		data, err := json.Marshal(k.Get("default_resources"))
		if err == nil {
			var rs api.ResourceSpec
			if json.Unmarshal(data, &rs) == nil {
				snap.DefaultResources = &rs
			}
		}
	}

	// Endpoints
	snap.PublicURL = k.String("server.hub.public_url")
	snap.ImageRegistry = k.String("image_registry")

	// GitHub App
	snap.GitHubAppID = k.Int64("server.github_app.app_id")
	snap.GitHubAPIBaseURL = k.String("server.github_app.api_base_url")
	snap.GitHubWebhooksEnabled = k.Bool("server.github_app.webhooks_enabled")
	snap.GitHubInstallationURL = k.String("server.github_app.installation_url")
	snap.GitHubPrivateKeyPath = k.String("server.github_app.private_key_path")

	// Notifications
	if k.Exists("server.notification_channels") {
		raw := k.Get("server.notification_channels")
		data, err := json.Marshal(raw)
		if err == nil {
			var channels []config.V1NotificationChannelConfig
			if json.Unmarshal(data, &channels) == nil {
				snap.NotificationChannels = channels
			}
		}
	}

	return snap
}

// BuildLayer1SnapshotFromFile constructs a Layer1Snapshot from the current
// GlobalConfig, i.e. from settings.yaml + env. This is used in file/SQLite
// mode where there is no DB tier for operational settings.
func BuildLayer1SnapshotFromFile(gc *config.GlobalConfig) Layer1Snapshot {
	snap := Layer1Snapshot{
		AdminEmails:        gc.Hub.AdminEmails,
		UserAccessMode:     gc.Auth.UserAccessMode,
		AuthorizedDomains:  gc.Auth.AuthorizedDomains,
		AutoSuspendStalled: gc.Hub.AutoSuspendStalled,
		TelemetryEnabled:   gc.TelemetryEnabled,
		AdminMode:          gc.AdminMode,
		MaintenanceMessage: gc.MaintenanceMessage,
	}

	if gc.TelemetryConfig != nil {
		snap.TelemetryConfig = gc.TelemetryConfig
	}

	// GitHub App (non-secret)
	snap.GitHubAppID = gc.GitHubApp.AppID
	snap.GitHubAPIBaseURL = gc.GitHubApp.APIBaseURL
	snap.GitHubWebhooksEnabled = gc.GitHubApp.WebhooksEnabled
	snap.GitHubInstallationURL = gc.GitHubApp.InstallationURL
	snap.GitHubPrivateKeyPath = gc.GitHubApp.PrivateKeyPath

	return snap
}

// ApplySnapshot writes the Layer1Snapshot values into the Server's config
// and MaintenanceState. This is the refactored body of the old reloadSettings()
// logic — no consumer sites change; request-path code keeps reading s.config.*
// under s.mu exactly as today.
//
// It returns the list of applied field names and the list of fields that
// require a restart (for parity with the old reloadSettings return value).
func ApplySnapshot(s *Server, snap Layer1Snapshot) map[string]interface{} {
	applied := []string{}

	s.mu.Lock()

	// Telemetry
	if snap.TelemetryEnabled != nil {
		oldVal := s.config.TelemetryDefault
		s.config.TelemetryDefault = snap.TelemetryEnabled
		if oldVal == nil || *oldVal != *snap.TelemetryEnabled {
			applied = append(applied, "telemetry_default")
		}
	}
	if snap.TelemetryConfig != nil {
		s.config.TelemetryConfig = config.ConvertV1TelemetryToAPI(snap.TelemetryConfig)
		applied = append(applied, "telemetry_config")
	}

	// Admin emails
	if len(snap.AdminEmails) > 0 {
		s.config.AdminEmails = snap.AdminEmails
		applied = append(applied, "admin_emails")
	}

	// Auto-suspend stalled
	oldAutoSuspend := s.config.AutoSuspendStalled
	s.config.AutoSuspendStalled = snap.AutoSuspendStalled
	if oldAutoSuspend != snap.AutoSuspendStalled {
		applied = append(applied, "auto_suspend_stalled")
	}

	// User access mode
	if snap.UserAccessMode != "" {
		s.config.UserAccessMode = snap.UserAccessMode
		applied = append(applied, "user_access_mode")
	} else if s.config.UserAccessMode != "" {
		s.config.UserAccessMode = ""
		applied = append(applied, "user_access_mode")
	}

	// GitHub App non-sensitive config
	if snap.GitHubAppID != 0 {
		s.config.GitHubAppConfig.AppID = snap.GitHubAppID
		s.config.GitHubAppConfig.APIBaseURL = snap.GitHubAPIBaseURL
		s.config.GitHubAppConfig.WebhooksEnabled = snap.GitHubWebhooksEnabled
		s.config.GitHubAppConfig.InstallationURL = snap.GitHubInstallationURL
		if snap.GitHubPrivateKeyPath != "" {
			s.config.GitHubAppConfig.PrivateKeyPath = snap.GitHubPrivateKeyPath
		}
		// In-memory private key and webhook secret are kept as-is (loaded from secrets backend)
		applied = append(applied, "github_app")
	}

	s.mu.Unlock()

	// Maintenance state (uses its own mutex)
	s.maintenance.Set(snap.AdminMode, snap.MaintenanceMessage)

	// Settings that require restart
	needsRestart := []string{
		"hub.port", "hub.host",
		"broker.port", "broker.host",
		"database.driver", "database.url",
		"auth.dev_mode",
		"oauth",
		"secrets.backend",
	}

	return map[string]interface{}{
		"applied":          applied,
		"requires_restart": needsRestart,
	}
}

// applySnapshotLogLevel applies the log-level portion of the snapshot.
// This is separated from applySnapshot because log level is a Layer-0 setting
// (per design §3.1) and is only changed in file mode via reloadSettings.
func applySnapshotLogLevel(level string) {
	if level == "" {
		return
	}
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	slog.SetLogLoggerLevel(lvl)
}
