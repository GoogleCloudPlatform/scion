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
	"math/rand"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/knadh/koanf/v2"
)

// sectionState holds the cached value, revision, and provenance metadata for a
// single section. UpdatedAt and UpdatedBy are populated during Refresh from DB
// rows, enabling buildSectionMetadata to serve metadata directly from the cache
// without an extra per-GET DB round-trip (N4).
type sectionState struct {
	Value     json.RawMessage
	Revision  int64
	UpdatedAt time.Time
	UpdatedBy string
	Origin    string
}

// Layer1Snapshot is an immutable merged view of all Layer-1 operational settings.
// It carries everything that reloadSettings() currently derives from the config
// file, so that ApplySnapshot can populate ServerConfig without re-reading any
// external source.
//
// Field population depends on the source:
//   - Postgres mode (OperationalSettings.Snapshot): ALL fields are populated via
//     the koanf merge (DB > bootstrap merge). This includes
//     SoftDeleteRetention, SoftDeleteRetainFiles, PublicURL, ImageRegistry,
//     DefaultTemplate, DefaultHarnessConfig, DefaultMaxTurns, DefaultMaxModelCalls,
//     DefaultMaxDuration, DefaultResources, and NotificationChannels.
//   - File mode (BuildLayer1SnapshotFromFile): only the fields that the old
//     reloadSettings() consumed are populated. Fields like SoftDeleteRetention,
//     DefaultTemplate, etc. remain at zero values because the old reloadSettings
//     never applied them on reload — they are consumed only at startup. This
//     maintains file-mode parity (the pre-refactor code never touched them on
//     config reload either).
type Layer1Snapshot struct {
	// Access
	AdminEmails       []string
	UserAccessMode    string
	AuthorizedDomains []string

	// Lifecycle
	AutoSuspendStalled    bool
	StalledThreshold      string // postgres-mode only (see type comment)
	SoftDeleteRetention   string // postgres-mode only (see type comment)
	SoftDeleteRetainFiles bool   // postgres-mode only (see type comment)

	// Maintenance
	AdminMode          bool
	MaintenanceMessage string
	// HasMaintenanceRow indicates whether a maintenance section row exists in
	// the DB. When false (row absent), ApplyMaintenanceFromSnapshot leaves
	// MaintenanceState as initialized at startup rather than resetting to
	// defaults. This field is only meaningful in postgres mode — file-mode
	// snapshots should never apply maintenance state.
	HasMaintenanceRow bool

	// Telemetry
	TelemetryEnabled *bool
	TelemetryConfig  *config.V1TelemetryConfig

	// Auto-expose ports
	AutoExposePortsEnabled *bool

	// Project defaults
	DefaultScratchpad *bool

	// Agent defaults
	DefaultTemplate      string
	DefaultHarnessConfig string
	DefaultMaxTurns      int
	DefaultMaxModelCalls int
	DefaultMaxDuration   string
	DefaultResources     *api.ResourceSpec
	DefaultModel         string
	DefaultThinkingLevel *int

	// Endpoints
	PublicURL     string
	HubName       string
	ImageRegistry string

	// GitHub App (non-secret fields only)
	GitHubAppID           int64
	GitHubAPIBaseURL      string
	GitHubWebhooksEnabled bool
	GitHubInstallationURL string
	GitHubPrivateKeyPath  string

	// Notifications
	NotificationChannels []config.V1NotificationChannelConfig

	// Federation
	FederationConfig *config.FederationConfig // nil when federation section not present

	// Runtimes — map of named runtime configs (DB > bootstrap fallback)
	Runtimes map[string]config.V1RuntimeConfig

	// Profiles — map of named profile configs (DB > bootstrap fallback)
	Profiles map[string]config.V1ProfileConfig

	// HarnessConfigs — map of named harness configurations (DB > bootstrap fallback)
	HarnessConfigs map[string]config.HarnessConfigEntry

	// EnvOverrides lists Layer-1 koanf keys that are overridden by env vars
	// on this node — used for drift warnings.
	EnvOverrides []string
}

// settingsUpdatedSubject is the LISTEN/NOTIFY subject used to propagate
// settings changes across hub replicas (design §3.6).
const settingsUpdatedSubject = "admin.settings.updated"

// SettingsUpdatedEvent is the payload published on admin.settings.updated.
type SettingsUpdatedEvent struct {
	Section  string `json:"section"`
	Revision int64  `json:"revision"`
}

// OperationalSettings is the runtime component that merges file, DB, and env
// sources into a single Layer-1 view per §3.5 of the settings-db design.
//
// It is owned by the Server and used only when database.driver == "postgres".
// In file/SQLite mode the legacy reloadSettings path is used instead.
type OperationalSettings struct {
	store          store.HubSettingStore
	bootstrapKoanf *koanf.Koanf    // Full bootstrap merge: defaults → SEED → yaml → SERVER
	envOverrides   map[string]bool // Layer-1 koanf keys satisfied by env
	envKoanf       *koanf.Koanf    // env-only koanf for merge
	mu             sync.RWMutex
	cache          map[string]sectionState // section name → cached value + revision

	// Event publisher for cross-replica propagation (nil in SQLite/file mode).
	events EventPublisher

	// server is set by StartPropagation — used for self-apply in Update
	// and for apply in the propagation loop. Nil until propagation starts.
	server *Server

	// PollInterval is the backstop poll interval. Defaults to 60s.
	// Exposed for testing with shortened timers (AC9).
	PollInterval time.Duration

	// stopPropagation cancels the propagation goroutines (subscriber + poll ticker).
	stopPropagation context.CancelFunc
	propagationWg   sync.WaitGroup
}

// NewOperationalSettings creates a new OperationalSettings service.
//
// bootstrapKoanf is the full bootstrap merge (defaults → SEED → yaml → SERVER)
// used as the fallback for sections absent from the DB. envKoanf is retained
// only for env-override detection; it is NOT merged into Snapshot.
func NewOperationalSettings(
	st store.HubSettingStore,
	bootstrapKoanf *koanf.Koanf,
	envKoanf *koanf.Koanf,
) *OperationalSettings {
	envOverrides := make(map[string]bool)
	for _, key := range opsettings.DetectEnvOverrides(envKoanf) {
		envOverrides[key] = true
	}

	return &OperationalSettings{
		store:          st,
		bootstrapKoanf: bootstrapKoanf,
		envOverrides:   envOverrides,
		envKoanf:       envKoanf,
		cache:          make(map[string]sectionState),
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
			Value:     row.Value,
			Revision:  row.Revision,
			UpdatedAt: row.UpdatedAt,
			UpdatedBy: row.UpdatedBy,
			Origin:    row.Origin,
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
// Precedence (per design §3.1):
//
//	DB rows > bootstrap merge (defaults → SEED → yaml → SERVER)
//
// DB sections fully own their keys; absent sections fall back to the bootstrap
// merge. Env vars are NOT merged on top — they feed the bootstrap layer and
// are honored as seed input during the deprecation window.
func (o *OperationalSettings) Snapshot() Layer1Snapshot {
	o.mu.RLock()
	dbSections := make(map[string]json.RawMessage, len(o.cache))
	for name, ss := range o.cache {
		dbSections[name] = ss.Value
	}
	o.mu.RUnlock()

	// Build the merged koanf: bootstrap fallback, overlaid by DB sections.
	merged := koanf.New(".")

	// Layer: bootstrap merge (lowest precedence for Layer-1 keys).
	// Only load keys for sections NOT present in DB.
	for _, sec := range opsettings.Registry {
		if _, inDB := dbSections[sec.Name]; inDB {
			// DB rows must contain complete section documents. The Update
			// path builds a full document from the PUT payload, so partial
			// rows should not occur in normal operation. If a partial row
			// is created externally, missing keys will have zero values
			// rather than bootstrap defaults.
			continue
		}
		if len(sec.KoanfPaths) == 0 {
			continue
		}
		// Extract this section from the file fallback and load it.
		doc, err := opsettings.ExtractSectionFromKoanf(o.bootstrapKoanf, sec.Name)
		if err != nil {
			continue
		}
		_ = loadSectionDocIntoKoanf(merged, sec.Name, doc)
	}

	// Layer: DB sections (highest precedence — DB wins over bootstrap).
	for name, doc := range dbSections {
		_ = loadSectionDocIntoKoanf(merged, name, doc)
	}

	snap := buildSnapshotFromKoanf(merged)

	// Map-of-objects sections (runtimes, profiles, harness_configs): extract
	// directly from DB docs or bootstrap koanf rather than going through the
	// merged koanf. The koanf round-trip loses empty-map semantics ({} → no
	// keys → nil), which would cause an admin-cleared section to silently
	// fall back to file values instead of returning the empty map.
	o.populateMapSections(&snap, dbSections)

	// Populate env overrides list.
	overrides := make([]string, 0, len(o.envOverrides))
	for key := range o.envOverrides {
		overrides = append(overrides, key)
	}
	snap.EnvOverrides = overrides

	// For maintenance, pull from DB section directly (not koanf — it has no
	// koanf paths). If no DB row exists, compiled defaults apply.
	snap.AdminMode, snap.MaintenanceMessage = o.maintenanceFromCache(dbSections)
	if _, ok := dbSections["maintenance"]; ok {
		snap.HasMaintenanceRow = true
	}

	return snap
}

// populateMapSections fills the Runtimes, Profiles, and HarnessConfigs
// snapshot fields. When a DB row exists for the section, the doc is
// deserialized directly (preserving empty maps). When no DB row exists,
// the bootstrap koanf is used as the file-based fallback.
func (o *OperationalSettings) populateMapSections(snap *Layer1Snapshot, dbSections map[string]json.RawMessage) {
	// Runtimes
	if doc, ok := dbSections["runtimes"]; ok {
		var v map[string]config.V1RuntimeConfig
		if err := json.Unmarshal(doc, &v); err != nil {
			slog.Warn("runtimes: failed to unmarshal DB doc for snapshot", "error", err)
		} else {
			if v == nil {
				v = map[string]config.V1RuntimeConfig{}
			}
			snap.Runtimes = v
		}
	} else {
		snap.Runtimes = extractRuntimesFromKoanf(o.bootstrapKoanf)
	}

	// Profiles
	if doc, ok := dbSections["profiles"]; ok {
		var v map[string]config.V1ProfileConfig
		if err := json.Unmarshal(doc, &v); err != nil {
			slog.Warn("profiles: failed to unmarshal DB doc for snapshot", "error", err)
		} else {
			if v == nil {
				v = map[string]config.V1ProfileConfig{}
			}
			snap.Profiles = v
		}
	} else {
		snap.Profiles = extractProfilesFromKoanf(o.bootstrapKoanf)
	}

	// HarnessConfigs
	if doc, ok := dbSections["harness_configs"]; ok {
		var v map[string]config.HarnessConfigEntry
		if err := json.Unmarshal(doc, &v); err != nil {
			slog.Warn("harness_configs: failed to unmarshal DB doc for snapshot", "error", err)
		} else {
			if v == nil {
				v = map[string]config.HarnessConfigEntry{}
			}
			snap.HarnessConfigs = v
		}
	} else {
		snap.HarnessConfigs = extractHarnessConfigsFromKoanf(o.bootstrapKoanf)
	}
}

// extractRuntimesFromKoanf extracts runtimes from a koanf instance (file fallback).
func extractRuntimesFromKoanf(k *koanf.Koanf) map[string]config.V1RuntimeConfig {
	if k == nil || !k.Exists("runtimes") {
		return nil
	}
	sub := k.Cut("runtimes")
	if sub == nil || len(sub.Keys()) == 0 {
		return nil
	}
	data, err := json.Marshal(sub.Raw())
	if err != nil {
		slog.Warn("runtimes: failed to marshal from bootstrap koanf", "error", err)
		return nil
	}
	var v map[string]config.V1RuntimeConfig
	if err := json.Unmarshal(data, &v); err != nil {
		slog.Warn("runtimes: failed to unmarshal from bootstrap koanf", "error", err)
		return nil
	}
	return v
}

// extractProfilesFromKoanf extracts profiles from a koanf instance (file fallback).
func extractProfilesFromKoanf(k *koanf.Koanf) map[string]config.V1ProfileConfig {
	if k == nil || !k.Exists("profiles") {
		return nil
	}
	sub := k.Cut("profiles")
	if sub == nil || len(sub.Keys()) == 0 {
		return nil
	}
	data, err := json.Marshal(sub.Raw())
	if err != nil {
		slog.Warn("profiles: failed to marshal from bootstrap koanf", "error", err)
		return nil
	}
	var v map[string]config.V1ProfileConfig
	if err := json.Unmarshal(data, &v); err != nil {
		slog.Warn("profiles: failed to unmarshal from bootstrap koanf", "error", err)
		return nil
	}
	return v
}

// extractHarnessConfigsFromKoanf extracts harness_configs from a koanf instance (file fallback).
func extractHarnessConfigsFromKoanf(k *koanf.Koanf) map[string]config.HarnessConfigEntry {
	if k == nil || !k.Exists("harness_configs") {
		return nil
	}
	sub := k.Cut("harness_configs")
	if sub == nil || len(sub.Keys()) == 0 {
		return nil
	}
	data, err := json.Marshal(sub.Raw())
	if err != nil {
		slog.Warn("harness_configs: failed to marshal from bootstrap koanf", "error", err)
		return nil
	}
	var v map[string]config.HarnessConfigEntry
	if err := json.Unmarshal(data, &v); err != nil {
		slog.Warn("harness_configs: failed to unmarshal from bootstrap koanf", "error", err)
		return nil
	}
	return v
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
	origin string,
) (int64, error) {
	// Validate via opsettings registry.
	if errs := opsettings.Validate(section, doc); len(errs) > 0 {
		return 0, fmt.Errorf("validation failed for section %q: %v", section, errs)
	}

	result, err := o.store.UpsertHubSetting(ctx, section, doc, updatedBy, expectedRevision, origin)
	if err != nil {
		return 0, err
	}

	// Update local cache.
	o.mu.Lock()
	o.cache[section] = sectionState{
		Value:     result.Value,
		Revision:  result.Revision,
		UpdatedAt: result.UpdatedAt,
		UpdatedBy: result.UpdatedBy,
		Origin:    result.Origin,
	}
	o.mu.Unlock()

	// Publish admin.settings.updated event to propagate the change to other
	// replicas via PostgresEventPublisher (design §3.6). The event publisher
	// is nil in file/SQLite mode — no-op there.
	if o.events != nil {
		o.events.PublishRaw(settingsUpdatedSubject, SettingsUpdatedEvent{
			Section:  section,
			Revision: result.Revision,
		})
	}

	// Self-apply: the writing node applies its own change synchronously
	// rather than waiting for its own event (design §3.6). Double-apply
	// (sync here + own event received via subscription) is harmless because
	// ApplySnapshot and ApplyMaintenanceFromSnapshot are idempotent.
	if o.server != nil {
		snap := o.Snapshot()
		ApplySnapshot(o.server, snap)
		ApplyMaintenanceFromSnapshot(o.server, snap)
	}

	return result.Revision, nil
}

// DeleteSection removes a section row from the store, evicts it from the local
// cache, publishes an event so peers refresh, and self-applies. The section
// falls back to bootstrap material immediately (design §3.2.4).
func (o *OperationalSettings) DeleteSection(ctx context.Context, section string) error {
	if err := o.store.DeleteHubSetting(ctx, section); err != nil {
		return err
	}

	o.mu.Lock()
	delete(o.cache, section)
	o.mu.Unlock()

	if o.events != nil {
		o.events.PublishRaw(settingsUpdatedSubject, SettingsUpdatedEvent{
			Section:  section,
			Revision: 0,
		})
	}

	if o.server != nil {
		snap := o.Snapshot()
		ApplySnapshot(o.server, snap)
		ApplyMaintenanceFromSnapshot(o.server, snap)
	}

	return nil
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

// SetEventPublisher wires the event publisher for cross-replica propagation.
// Must be called before StartPropagation. Nil is safe (disables publishing
// in Update). In file/SQLite mode this is never called.
func (o *OperationalSettings) SetEventPublisher(ep EventPublisher) {
	o.events = ep
}

// StartPropagation begins the cross-replica change propagation loop (design
// §3.6). It subscribes to admin.settings.updated events, starts a 60s jittered
// poll backstop, and wires the reconnect callback for unconditional refresh.
//
// Must be called after SetEventPublisher. Postgres mode only; in file/SQLite
// mode this is never called (the writing handler applies synchronously).
//
// The ctx should be the server's lifetime context; cancellation stops the
// propagation goroutines.
func (o *OperationalSettings) StartPropagation(ctx context.Context, server *Server) {
	if o.events == nil {
		return
	}

	o.server = server

	propCtx, cancel := context.WithCancel(ctx)
	o.stopPropagation = cancel

	// --- Subscribe to admin.settings.updated events (§3.6 primary) ---
	ch, unsub := o.events.Subscribe(settingsUpdatedSubject)

	o.propagationWg.Add(1)
	go func() {
		defer o.propagationWg.Done()
		defer unsub()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Settings propagation subscription loop panicked — propagation stopped on this replica", "panic", r)
			}
		}()
		o.runSubscriptionLoop(propCtx, ch, server)
	}()

	// --- Poll backstop at 60s with jitter (§3.6 backstop) ---
	o.propagationWg.Add(1)
	go func() {
		defer o.propagationWg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Settings propagation poll backstop panicked — propagation stopped on this replica", "panic", r)
			}
		}()
		o.runPollBackstop(propCtx, server)
	}()

	// --- Reconnect refresh callback (§3.6 reconnect) ---
	// Use propCtx (not the parent ctx) so reconnect refreshes respect the
	// propagation lifecycle and stop when StopPropagation cancels propCtx.
	if pgPub, ok := o.events.(*PostgresEventPublisher); ok {
		pgPub.SetOnReconnect(func() {
			slog.Info("Event listener reconnected — refreshing operational settings unconditionally")
			o.refreshAndApply(propCtx, server)
		})
	}
}

// StopPropagation stops the propagation goroutines and waits for them to exit.
func (o *OperationalSettings) StopPropagation() {
	if o.stopPropagation != nil {
		o.stopPropagation()
	}
	o.propagationWg.Wait()
}

// runSubscriptionLoop listens for admin.settings.updated events and triggers
// Refresh + apply on receipt.
func (o *OperationalSettings) runSubscriptionLoop(ctx context.Context, ch <-chan Event, server *Server) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			// Decode the event to log which section changed; the actual
			// application always does a full Refresh (cheap revision-only diff).
			var payload SettingsUpdatedEvent
			if err := json.Unmarshal(evt.Data, &payload); err == nil {
				slog.Info("Received settings update event", "section", payload.Section, "revision", payload.Revision)
			}
			o.refreshAndApply(ctx, server)
		}
	}
}

// runPollBackstop runs a ticker at the configured PollInterval (default 60s,
// with ±10s jitter) that calls Refresh and applies any changes. This is the
// backstop for missed NOTIFY events (design §3.6). Postgres mode only.
func (o *OperationalSettings) runPollBackstop(ctx context.Context, server *Server) {
	interval := o.PollInterval
	if interval == 0 {
		interval = 60 * time.Second
	}

	// Initial jitter: 0-10s offset so replicas don't all poll at the same instant.
	// Skip jitter for short test intervals.
	if interval > 5*time.Second {
		jitter := time.Duration(rand.Int63n(int64(10 * time.Second)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter):
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.refreshAndApply(ctx, server)
		}
	}
}

// refreshAndApply performs a Refresh and applies any changed sections to the
// server. Idempotent: applying the same snapshot twice is harmless because
// ApplySnapshot writes the same values and ApplyMaintenanceFromSnapshot
// is idempotent by design.
func (o *OperationalSettings) refreshAndApply(ctx context.Context, server *Server) {
	changed, err := o.Refresh(ctx)
	if err != nil {
		slog.Error("Settings propagation refresh failed", "error", err)
		return
	}
	if len(changed) > 0 {
		slog.Info("Settings propagation detected changes", "sections", changed)
		snap := o.Snapshot()
		ApplySnapshot(server, snap)
		ApplyMaintenanceFromSnapshot(server, snap)
	}
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
	snap.StalledThreshold = k.String("server.hub.stalled_threshold")
	snap.SoftDeleteRetention = k.String("server.hub.soft_delete_retention")
	snap.SoftDeleteRetainFiles = k.Bool("server.hub.soft_delete_retain_files")

	// Telemetry — extract via the section struct for full fidelity.
	if k.Exists("telemetry.enabled") {
		v := k.Bool("telemetry.enabled")
		snap.TelemetryEnabled = &v
	}

	// Auto-expose ports
	if k.Exists("auto_expose_ports.enabled") {
		v := k.Bool("auto_expose_ports.enabled")
		snap.AutoExposePortsEnabled = &v
	}

	// Project defaults
	if k.Exists("project_defaults.default_scratchpad") {
		v := k.Bool("project_defaults.default_scratchpad")
		snap.DefaultScratchpad = &v
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
	snap.DefaultModel = k.String("default_model")
	if k.Exists("default_thinking_level") {
		v := k.Int("default_thinking_level")
		snap.DefaultThinkingLevel = &v
	}
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
	snap.HubName = k.String("server.hub.hub_name")
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

	// Federation
	if k.Exists("server.federation.enabled") || k.Exists("server.federation.trusted_issuers") {
		fedCfg := config.FederationConfig{
			Enabled: k.Bool("server.federation.enabled"),
		}
		// TrustedIssuers: unmarshal from the koanf slice
		if raw := k.Get("server.federation.trusted_issuers"); raw != nil {
			data, err := json.Marshal(raw)
			if err != nil {
				slog.Warn("federation: failed to marshal trusted_issuers from koanf", "error", err)
			} else if err := json.Unmarshal(data, &fedCfg.TrustedIssuers); err != nil {
				slog.Warn("federation: failed to unmarshal trusted_issuers", "error", err)
			}
		}
		if algs := k.Strings("server.federation.algorithms"); len(algs) > 0 {
			fedCfg.Algorithms = algs
		}
		if ri := k.String("server.federation.refresh_interval"); ri != "" {
			if d, err := time.ParseDuration(ri); err == nil {
				fedCfg.Cache.RefreshInterval = d
			}
		}
		if di := k.String("server.federation.debounce_interval"); di != "" {
			if d, err := time.ParseDuration(di); err == nil {
				fedCfg.Cache.DebounceInterval = d
			}
		}
		snap.FederationConfig = &fedCfg
	}

	// NOTE: Runtimes, profiles, and harness_configs are NOT extracted from
	// koanf here. Map-of-objects sections lose empty-map semantics in the
	// koanf round-trip ({} → no keys → nil), so they are extracted directly
	// from the DB docs (or bootstrap koanf) in Snapshot(). See the
	// populateMapSections call in Snapshot().

	return snap
}

// BuildLayer1SnapshotFromFile constructs a Layer1Snapshot from the current
// GlobalConfig, i.e. from settings.yaml + env. This is used in file/SQLite
// mode where there is no DB tier for operational settings.
//
// NOTE: Only fields that the old reloadSettings() consumed are populated here.
// Fields like SoftDeleteRetention, DefaultTemplate, DefaultMaxTurns, PublicURL,
// ImageRegistry, DefaultResources, and NotificationChannels remain at zero
// values — the old reloadSettings never applied those on config reload (they
// are consumed at startup, not on reload). In postgres mode, the full koanf-based
// Snapshot() populates all fields. See the Layer1Snapshot type comment for details.
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

	// Project defaults — read from settings.yaml project_defaults section
	snap.DefaultScratchpad = gc.DefaultScratchpad

	// Federation — read from GlobalConfig
	if gc.Federation.Enabled || len(gc.Federation.TrustedIssuers) > 0 {
		snap.FederationConfig = &gc.Federation
	}

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

	// Auto-expose ports
	if snap.AutoExposePortsEnabled != nil {
		oldVal := s.config.AutoExposePortsDefault
		s.config.AutoExposePortsDefault = snap.AutoExposePortsEnabled
		if oldVal == nil || *oldVal != *snap.AutoExposePortsEnabled {
			applied = append(applied, "auto_expose_ports_default")
		}
	}

	// Project defaults
	if snap.DefaultScratchpad != nil {
		oldVal := s.config.DefaultScratchpad
		s.config.DefaultScratchpad = snap.DefaultScratchpad
		if oldVal == nil || *oldVal != *snap.DefaultScratchpad {
			applied = append(applied, "default_scratchpad")
		}
	}

	// Admin emails — sanitize (TrimSpace + ToLower, drop empties) to match
	// the normalization the user store applies (D11-fix).
	if len(snap.AdminEmails) > 0 {
		s.config.AdminEmails = config.SanitizeEmailList(snap.AdminEmails)
		applied = append(applied, "admin_emails")
	}

	// Auto-suspend stalled
	oldAutoSuspend := s.config.AutoSuspendStalled
	s.config.AutoSuspendStalled = snap.AutoSuspendStalled
	if oldAutoSuspend != snap.AutoSuspendStalled {
		applied = append(applied, "auto_suspend_stalled")
	}

	// Stalled threshold
	if snap.StalledThreshold != "" {
		if d, err := time.ParseDuration(snap.StalledThreshold); err == nil {
			if d < 2*time.Minute {
				defaultThreshold := DefaultServerConfig().StalledThreshold
				slog.Warn("stalled_threshold below minimum 2m, using default",
					"configured", snap.StalledThreshold, "default", defaultThreshold)
				d = defaultThreshold
			}
			if s.config.StalledThreshold != d {
				applied = append(applied, "stalled_threshold")
			}
			s.config.StalledThreshold = d
		} else {
			slog.Warn("invalid stalled_threshold duration, keeping current value", "value", snap.StalledThreshold, "error", err)
		}
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

	// Hub name
	if snap.HubName != "" {
		s.config.HubName = snap.HubName
		applied = append(applied, "hub_name")
	}

	// Image registry (#985) — wire DB value to the consumption path.
	// resolveImageRegistry() reads s.config.MaintenanceConfig.ImageRegistry.
	if snap.ImageRegistry != "" {
		old := s.config.MaintenanceConfig.ImageRegistry
		s.config.MaintenanceConfig.ImageRegistry = snap.ImageRegistry
		if old != snap.ImageRegistry {
			applied = append(applied, "image_registry")
		}
	}

	// Agent defaults (hub operational agent_defaults section).
	//
	// Written unconditionally from the snapshot rather than only-if-non-empty,
	// so that clearing a value in the DB clears it here too. In file mode the
	// snapshot's agent-defaults fields are always zero — see
	// BuildLayer1SnapshotFromFile — so this assignment is a no-op there and
	// file-mode dispatch is unchanged.
	newDefaults := opsettings.AgentDefaultsSettings{
		DefaultTemplate:      snap.DefaultTemplate,
		DefaultHarnessConfig: snap.DefaultHarnessConfig,
		DefaultMaxTurns:      snap.DefaultMaxTurns,
		DefaultMaxModelCalls: snap.DefaultMaxModelCalls,
		DefaultMaxDuration:   snap.DefaultMaxDuration,
		DefaultModel:         snap.DefaultModel,
		DefaultThinkingLevel: snap.DefaultThinkingLevel,
	}
	// Deep-copy the one pointer field, symmetrically with hubAgentDefaults()'s
	// read side. Aliasing the snapshot's pointee would leave the CALLER of
	// ApplySnapshot holding a live, lock-free handle on s.config, which is the
	// same hazard the accessor deep-copies to avoid. Benign today — Snapshot()
	// allocates a fresh spec per call and every caller discards it — but the
	// asymmetry would read as an oversight later, and the fix is two lines.
	if snap.DefaultResources != nil {
		rs := *snap.DefaultResources
		newDefaults.DefaultResources = &rs
	}
	if snap.DefaultThinkingLevel != nil {
		v := *snap.DefaultThinkingLevel
		newDefaults.DefaultThinkingLevel = &v
	}
	if !agentDefaultsEqual(s.config.AgentDefaults, newDefaults) {
		applied = append(applied, "agent_defaults")
	}
	s.config.AgentDefaults = newDefaults

	s.mu.Unlock()

	// Propagate hub_name to the GCP secret backend so new secrets get the
	// correct label value. Log handlers have a similar limitation (§7.4).
	if snap.HubName != "" {
		if gcpBackend, ok := s.secretBackend.(*secret.GCPBackend); ok {
			gcpBackend.SetHubName(snap.HubName)
		}
	}

	// Runtimes, profiles, and harness configs: update the global settings
	// overlay so that the co-located broker (which reads via
	// config.LoadEffectiveSettings on every dispatch) sees DB-backed values.
	// The overlay is installed at hub startup in co-located mode (see
	// cmd/server_foreground.go). For standalone brokers the overlay is nil
	// and they continue reading from disk only.
	if snap.Runtimes != nil || snap.Profiles != nil || snap.HarnessConfigs != nil {
		if overlay := config.GetGlobalSettingsOverlay(); overlay != nil {
			overlay.Update(snap.Runtimes, snap.Profiles, snap.HarnessConfigs, snap.ImageRegistry)
			if snap.Runtimes != nil {
				applied = append(applied, "runtimes")
			}
			if snap.Profiles != nil {
				applied = append(applied, "profiles")
			}
			if snap.HarnessConfigs != nil {
				applied = append(applied, "harness_configs")
			}
			slog.Info("Settings overlay updated with DB-backed runtimes/profiles/harness_configs",
				"runtimes", len(snap.Runtimes),
				"profiles", len(snap.Profiles),
				"harness_configs", len(snap.HarnessConfigs),
			)
		}
	}

	// NOTE: Maintenance state is deliberately NOT applied here.
	// Maintenance is runtime/API-owned state. In file mode, reloadSettings
	// must never touch MaintenanceState (restoring pre-refactor behavior).
	// In postgres mode, the caller uses ApplyMaintenanceFromSnapshot
	// separately, which respects env > DB precedence (§3.4/§3.8).

	// Federation (outside mutex — atomic.Pointer swap is lock-free,
	// and NewFederationAuthenticator may do network I/O)
	//
	// When federation is disabled (nil config or Enabled=false), clear the
	// authenticator so the middleware returns 401. This ensures disabling
	// federation at runtime actually takes effect.
	if snap.FederationConfig == nil || !snap.FederationConfig.Enabled {
		s.federationAuth.Store(nil)
		if snap.FederationConfig != nil {
			slog.Info("Federation disabled via config, authenticator cleared")
		}
		applied = append(applied, "federation")
	} else {
		if errs := snap.FederationConfig.Validate(); len(errs) > 0 {
			slog.Error("Federation config validation failed during apply, keeping old config",
				"errors", fmt.Sprintf("%v", errs))
		} else {
			// Derive federation mode using the same pattern as New().
			federationMode := s.config.Mode
			if federationMode == "" {
				if s.config.Workstation {
					federationMode = "workstation"
				} else {
					federationMode = "hosted"
				}
			}
			// Use the OIDC issuer URL as the default expected audience.
			federationAudience := s.oidcIssuerURL
			if federationAudience == "" {
				federationAudience = s.config.OIDCConfig.IssuerURL
			}
			newAuth, err := NewFederationAuthenticator(
				*snap.FederationConfig,
				federationAudience,
				s.federationClient,
				federationMode,
				logging.Subsystem("hub.federation"),
			)
			if err != nil {
				slog.Error("Federation authenticator rebuild failed, keeping old config",
					"error", err)
			} else {
				s.federationAuth.Store(newAuth)
				slog.Info("Federation authenticator hot-reloaded",
					"trusted_issuers", len(snap.FederationConfig.TrustedIssuers),
					"enabled", snap.FederationConfig.Enabled)
				applied = append(applied, "federation")
			}
		}
	}

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

// ApplyMaintenanceFromSnapshot applies maintenance state from a postgres-mode
// snapshot, respecting the env > DB precedence rule (design §3.4/§3.8).
//
// This function must be called ONLY in postgres-mode paths — file-mode
// reloadSettings must never touch MaintenanceState (it is runtime/API-owned).
//
// Behavior:
//   - If snap.HasMaintenanceRow is false (no DB row): no-op — MaintenanceState
//     keeps its current value (which honors the env var set at startup).
//   - If snap.HasMaintenanceRow is true: apply DB values, UNLESS the
//     SCION_SERVER_ADMIN_MODE env var is set (per-node break-glass override).
//
// ApplyMaintenanceFromSnapshot applies the maintenance settings from the
// snapshot to the server's maintenance state. In HA mode, maintenance must
// be cluster-consistent — per-node env force-win is removed.
func ApplyMaintenanceFromSnapshot(s *Server, snap Layer1Snapshot) {
	if !snap.HasMaintenanceRow {
		return
	}

	s.maintenance.Set(snap.AdminMode, snap.MaintenanceMessage)
}

// ProjectDefaultScratchpad returns whether the default scratchpad shared
// dir is enabled for new projects. Returns true (compiled default) when
// the project_defaults section is absent from the DB.
func (o *OperationalSettings) ProjectDefaultScratchpad() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache["project_defaults"]
	if !ok {
		return true // compiled default: ON
	}

	var pd opsettings.ProjectDefaultsSettings
	if err := json.Unmarshal(state.Value, &pd); err != nil {
		return true // parse error → fall back to compiled default
	}

	if pd.DefaultScratchpad != nil {
		return *pd.DefaultScratchpad
	}
	return true // field omitted in doc → compiled default
}

// ConversationReadSwitch returns whether the Phase 8 conversation read-switch
// is enabled. Returns false (compiled default) when the messaging section is
// absent from the DB. Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) ConversationReadSwitch() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache["messaging"]
	if !ok {
		return false // compiled default: OFF
	}

	var ms opsettings.MessagingSettings
	if err := json.Unmarshal(state.Value, &ms); err != nil {
		return false // parse error → fall back to compiled default
	}

	if ms.ConversationReadSwitch != nil {
		return *ms.ConversationReadSwitch
	}
	return false // field omitted in doc → compiled default
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
