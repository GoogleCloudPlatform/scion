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
	"log/slog"
	"maps"
	"math/rand"
	"sync"
	"sync/atomic"
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

	// Malformed is true when Value is not valid JSON. Set once at
	// Refresh/Update time so the parse failure is logged exactly once per
	// ingest rather than once per getter call (DEF-92). Getters can then
	// distinguish "validated document" from "unreadable document" without
	// re-parsing and without swallowing errors silently.
	Malformed bool

	// ExperimentsOverrides is the parsed overrides for the "experiments"
	// section only; nil for every other section, and nil for "experiments"
	// itself when absent, malformed, or a valid document with no overrides
	// field. Populated at the same ingest points as Malformed (Refresh,
	// Update), so it is always part of the same sectionState value and is
	// replaced or removed together with it — a delete, a replace, or an
	// eviction can never leave it stale. ExperimentsSnapshot is on a hot
	// path (GET /api/v1/experiments runs on every page load, and
	// requireExperiment runs per request), so parsing once here rather than
	// per read matters.
	ExperimentsOverrides map[string]bool
}

// Layer1Snapshot is an immutable merged view of all Layer-1 operational settings.
// It carries everything that reloadSettings() currently derives from the config
// file, so that ApplySnapshot can populate ServerConfig without re-reading any
// external source.
//
// Field population depends on the source:
//   - DB-backed, any driver (OperationalSettings.Snapshot): ALL fields are populated via
//     the koanf merge (DB > bootstrap merge). This includes
//     SoftDeleteRetention, SoftDeleteRetainFiles, PublicURL, ImageRegistry,
//     DefaultTemplate, DefaultHarnessConfig, DefaultMaxTurns, DefaultMaxModelCalls,
//     DefaultMaxDuration, DefaultResources, and NotificationChannels.
//   - No OperationalSettings (BuildLayer1SnapshotFromFile): only the fields that the old
//     reloadSettings() consumed are populated, plus DefaultHarnessConfig and
//     DefaultTimezone, which are read from the top-level
//     default_harness_config and default_timezone keys in settings.yaml,
//     and HubName (see the HubName field: every constructor must set it).
//     Fields like SoftDeleteRetention, DefaultTemplate, etc. remain at zero
//     values because the old reloadSettings never applied them on reload — they
//     are consumed only at startup. This maintains file-mode parity (the
//     pre-refactor code never touched them on config reload either).
type Layer1Snapshot struct {
	// Access
	AdminEmails       []string
	UserAccessMode    string
	DefaultUserRole   string
	AuthorizedDomains []string

	// Lifecycle
	AutoSuspendStalled    bool
	StalledThreshold      string // DB-backed snapshots only (see type comment)
	SoftDeleteRetention   string // DB-backed snapshots only (see type comment)
	SoftDeleteRetainFiles bool   // DB-backed snapshots only (see type comment)

	// Start claim timing (durations as strings; empty keeps the startup value)
	StartClaimLeaseTTL         string
	StartMaxDuration           string
	StartUnconfirmedHold       string
	StartCreateUnconfirmedHold string

	// GCP service-account permission check (gcp_iam section). DB-backed
	// snapshots only; "" means no stored or file value, and ApplySnapshot
	// then uses the deploy-time value (resolveGCPIAMSettings).
	GCPIAMCheckMode         string
	GCPIAMDenyUnknownPolicy string

	// Maintenance
	AdminMode          bool
	MaintenanceMessage string
	// HasMaintenanceRow indicates whether a maintenance section row exists in
	// the DB. When false (row absent), ApplyMaintenanceFromSnapshot leaves
	// MaintenanceState as initialized at startup rather than resetting to
	// defaults. This field is only meaningful for DB-backed snapshots (any
	// driver) — snapshots built from the file should never apply maintenance
	// state.
	HasMaintenanceRow bool

	// Telemetry
	TelemetryEnabled *bool
	TelemetryConfig  *config.V1TelemetryConfig

	// Auto-expose ports
	AutoExposePortsEnabled *bool

	// Quotas
	EnforceBrokerQuotas *bool

	// Agent secrets
	AgentSecretsUserScopeOnly *bool

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
	DefaultRuntimeBroker string
	DefaultTimezone      string
	// DefaultGCPIdentityMode/DefaultGCPIdentityServiceAccountID are the
	// hub-wide GCP identity default, DB-backed snapshots only (see type
	// comment).
	DefaultGCPIdentityMode             string
	DefaultGCPIdentityServiceAccountID string

	// Endpoints
	PublicURL string
	// HubName is the configured hub_name. "" does NOT mean "leave alone":
	// ApplySnapshot resets the running name (and the GCP secret label) to
	// this replica's startup name. Every snapshot constructor must set it.
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
// It is owned by the Server and used on all database drivers. Cross-replica
// propagation (§3.6) requires a PostgresEventPublisher and is a no-op on
// SQLite.
type OperationalSettings struct {
	store          store.HubSettingStore
	bootstrapKoanf *koanf.Koanf    // Full bootstrap merge: defaults → SEED → yaml → SERVER
	envOverrides   map[string]bool // Layer-1 koanf keys satisfied by env
	envKoanf       *koanf.Koanf    // env-only koanf for merge
	mu             sync.RWMutex
	cache          map[string]sectionState // section name → cached value + revision

	// Audit observation is copied under mu with the matching experiment snapshot.
	// Mutations prevent a read begun across a write from certifying freshness.
	decisionAuditObserver    atomic.Pointer[decisionAuditRouter]
	decisionAuditObservation decisionAuditRefreshObservation
	decisionAuditMutations   uint8

	// remoteImagesWarnedRev is the artifacts revision whose invalid remote
	// image setting was last logged.
	remoteImagesWarnedRev atomic.Int64

	// Event publisher for cross-replica propagation: LISTEN/NOTIFY on
	// postgres, in-process channel on SQLite; nil until SetEventPublisher.
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
	return o.refreshForDecisionAuditAttachment(ctx, nil)
}

// Propagation reads retain their original attachment even if an explicit source
// handoff occurs before their read begins. Generic cache ingestion is unchanged.
func (o *OperationalSettings) refreshForDecisionAuditAttachment(ctx context.Context, attachment *decisionAuditPropagationAttachment) ([]string, error) {
	observer := o.decisionAuditObserver.Load()
	if attachment != nil {
		observer = attachment.observer
	}
	var observation decisionAuditRefreshObservation
	if observer != nil {
		if attachment == nil {
			observation = observer.beginRefresh(o)
		} else {
			observation = observer.beginRefreshForAttachment(o, &attachment.attachment)
		}
	}
	rows, err := o.store.ListHubSettings(ctx)
	if err != nil {
		o.mu.Lock()
		if observer != nil {
			if _, current := observer.finishRefreshForAttachment(observation, ExperimentsSnapshot{}, err); current {
				o.decisionAuditObservation = decisionAuditRefreshObservation{}
			}
		}
		o.mu.Unlock()
		return nil, fmt.Errorf("operational settings refresh: %w", err)
	}

	// Bound audit proof work only. Generic cache ingestion/changed-list behavior
	// stays as before, including when these local proof caps reject the read.
	var auditErr error
	if auditErr == nil && observer != nil && observation.tracked {
		if len(rows) > decisionAuditSettingsMaxRows {
			auditErr = fmt.Errorf("audit read row cap")
		} else {
			size := 0
			for _, row := range rows {
				for _, n := range []int{len(row.Value), len(row.Section), len(row.UpdatedBy), len(row.Origin)} {
					if n > decisionAuditSettingsMaxBytes-size {
						auditErr = fmt.Errorf("audit read byte cap")
						break
					}
					size += n
				}
				if auditErr != nil {
					break
				}
			}
		}
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
		malformed := !json.Valid(row.Value)
		if malformed {
			slog.Error("operational settings: malformed JSON in section document",
				"section", row.Section,
				"revision", row.Revision,
			)
		} else if sec := opsettings.SectionByName(row.Section); sec != nil && sec.New != nil {
			// Syntactically valid JSON can still be semantically wrong (e.g.
			// {"conversation_envelope_switch":"yes"} — valid JSON, but "yes"
			// does not unmarshal into *bool). Detect at ingest so the failure
			// is logged once per refresh, not silently swallowed per getter.
			target := sec.New()
			if err := json.Unmarshal(row.Value, target); err != nil {
				malformed = true
				slog.Error("operational settings: section document has type-incompatible fields",
					"section", row.Section,
					"revision", row.Revision,
					"error", err,
				)
			}
		}
		experimentsOverrides, malformed := experimentsOverridesFor(row.Section, row.Value, malformed)
		o.cache[row.Section] = sectionState{
			Value:                row.Value,
			Revision:             row.Revision,
			UpdatedAt:            row.UpdatedAt,
			UpdatedBy:            row.UpdatedBy,
			Origin:               row.Origin,
			Malformed:            malformed,
			ExperimentsOverrides: experimentsOverrides,
		}
	}

	// Detect deleted sections (in cache but not in DB).
	for name := range o.cache {
		if !seen[name] {
			changed = append(changed, name)
			delete(o.cache, name)
		}
	}

	if observer != nil {
		if o.decisionAuditMutations != 0 {
			auditErr = fmt.Errorf("audit source mutation in flight")
		}
		var snapshot ExperimentsSnapshot
		if auditErr == nil && observation.tracked {
			snapshot = o.experimentsSnapshotLocked()
		}
		if result, current := observer.finishRefreshForAttachment(observation, snapshot, auditErr); current {
			o.decisionAuditObservation = result
		}
	}
	return changed, nil
}

// experimentsOverridesFor returns the parsed "experiments" section overrides
// and the (possibly updated) malformed flag, for storage in sectionState
// alongside the generic ingest check that produced malformed. It is a no-op
// for any section other than "experiments": callers pass malformed straight
// through unchanged and get a nil map back.
//
// Folding this into sectionState (rather than a second, separately-tracked
// field on OperationalSettings) means every write, delete, or replace of the
// cache entry carries the parsed overrides automatically — there is no
// second place that can go out of step with the cache.
func experimentsOverridesFor(section string, raw json.RawMessage, malformed bool) (map[string]bool, bool) {
	if section != "experiments" || malformed {
		return nil, malformed
	}
	doc, docMalformed := opsettings.ParseExperimentsDoc(raw)
	if docMalformed {
		// The caller's ingest check (Refresh/Update's sec.New() unmarshal)
		// already applies the same predicate (the "experiments" section's
		// New() unmarshals into the same ExperimentsSettings shape
		// ParseExperimentsDoc uses), so this cannot happen in practice. Fail
		// closed rather than trust an inconsistent parse, and let it show up
		// in Malformed too.
		return nil, true
	}
	return doc.Overrides, malformed
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

	// hub_name is the one endpoints key that keeps its bootstrap value when
	// a DB row omits it: a managed endpoints row carries hub_name only after
	// an admin sets it, and clearing it returns to the bootstrap name. The
	// snapshot holds the configured value only; "" means unset, and
	// ApplySnapshot then uses the name this replica resolved at startup
	// (startupHubNameOrDefault). A replica's hostname must not appear
	// here: GET returns this value and clients echo it back to any replica.
	if snap.HubName == "" && o.bootstrapKoanf != nil {
		snap.HubName = o.bootstrapKoanf.String("server.hub.hub_name")
	}

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
	o.beginDecisionAuditMutation()
	defer o.endDecisionAuditMutation()
	// Validate via opsettings registry.
	if errs := opsettings.Validate(section, doc); len(errs) > 0 {
		return 0, fmt.Errorf("validation failed for section %q: %v", section, errs)
	}

	result, err := o.store.UpsertHubSetting(ctx, section, doc, updatedBy, expectedRevision, origin)
	if err != nil {
		return 0, err
	}

	// Update local cache.
	malformed := !json.Valid(result.Value)
	if malformed {
		slog.Error("operational settings: malformed JSON after write (should not happen)",
			"section", section,
			"revision", result.Revision,
		)
	} else if sec := opsettings.SectionByName(section); sec != nil && sec.New != nil {
		target := sec.New()
		if err := json.Unmarshal(result.Value, target); err != nil {
			malformed = true
			slog.Error("operational settings: section document has type-incompatible fields after write",
				"section", section,
				"revision", result.Revision,
				"error", err,
			)
		}
	}
	experimentsOverrides, malformed := experimentsOverridesFor(section, result.Value, malformed)
	o.mu.Lock()
	o.cache[section] = sectionState{
		Value:                result.Value,
		Revision:             result.Revision,
		UpdatedAt:            result.UpdatedAt,
		UpdatedBy:            result.UpdatedBy,
		Origin:               result.Origin,
		Malformed:            malformed,
		ExperimentsOverrides: experimentsOverrides,
	}
	o.decisionAuditObservation = decisionAuditRefreshObservation{}
	if observer := o.decisionAuditObserver.Load(); observer != nil {
		observer.invalidateSettings(o)
	}
	o.mu.Unlock()

	// Publish admin.settings.updated event to propagate the change to other
	// replicas via PostgresEventPublisher (design §3.6). On SQLite the
	// publisher is an in-process ChannelEventPublisher (single replica); it
	// is nil only before StartPropagation is wired, e.g. in tests.
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
	o.beginDecisionAuditMutation()
	defer o.endDecisionAuditMutation()
	if err := o.store.DeleteHubSetting(ctx, section); err != nil {
		return err
	}

	o.mu.Lock()
	delete(o.cache, section)
	o.decisionAuditObservation = decisionAuditRefreshObservation{}
	if observer := o.decisionAuditObserver.Load(); observer != nil {
		observer.invalidateSettings(o)
	}
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
// in Update). Called on every DB driver: postgres wires the LISTEN/NOTIFY
// publisher, SQLite an in-process ChannelEventPublisher.
func (o *OperationalSettings) SetEventPublisher(ep EventPublisher) {
	o.events = ep
}

// StartPropagation begins the cross-replica change propagation loop (design
// §3.6). It subscribes to admin.settings.updated events, starts a 60s jittered
// poll backstop, and wires the reconnect callback for unconditional refresh.
//
// Must be called after SetEventPublisher. Runs on every DB driver; on SQLite
// (single replica, in-process publisher) the writing node's synchronous
// self-apply is what matters and the poll backstop is a harmless re-read.
//
// The ctx should be the server's lifetime context; cancellation stops the
// propagation goroutines.
func (o *OperationalSettings) StartPropagation(ctx context.Context, server *Server) {
	if o.events == nil {
		return
	}

	o.server = server

	propCtx, cancel := context.WithCancel(ctx)
	attachment := o.decisionAuditPropagationAttachment()
	o.stopPropagation = func() { o.loseDecisionAuditPropagation(attachment); cancel() }

	// --- Subscribe to admin.settings.updated events (§3.6 primary) ---
	ch, unsub := o.events.Subscribe(settingsUpdatedSubject)

	o.propagationWg.Add(1)
	go func() {
		defer o.propagationWg.Done()
		defer unsub()
		defer func() {
			if r := recover(); r != nil {
				o.loseDecisionAuditPropagation(attachment)
				slog.Error("Settings propagation subscription loop panicked — propagation stopped on this replica", "panic", r)
			}
		}()
		o.runSubscriptionLoopForAttachment(propCtx, ch, server, attachment)
	}()

	// --- Poll backstop at 60s with jitter (§3.6 backstop) ---
	o.propagationWg.Add(1)
	go func() {
		defer o.propagationWg.Done()
		defer func() {
			if r := recover(); r != nil {
				o.loseDecisionAuditPropagation(attachment)
				slog.Error("Settings propagation poll backstop panicked — propagation stopped on this replica", "panic", r)
			}
		}()
		o.runPollBackstopForAttachment(propCtx, server, attachment)
	}()

	// --- Reconnect refresh callback (§3.6 reconnect) ---
	// Use propCtx (not the parent ctx) so reconnect refreshes respect the
	// propagation lifecycle and stop when StopPropagation cancels propCtx.
	if pgPub, ok := o.events.(*PostgresEventPublisher); ok {
		pgPub.SetOnReconnect(func() {
			slog.Info("Event listener reconnected — refreshing operational settings unconditionally")
			o.refreshAndApplyForAttachment(propCtx, server, &attachment)
		})
	}
}

// StopPropagation stops the propagation goroutines and waits for them to exit.
func (o *OperationalSettings) StopPropagation() {
	if o.stopPropagation != nil {
		o.stopPropagation()
	} else {
		o.loseDecisionAuditPropagation(o.decisionAuditPropagationAttachment())
	}
	o.propagationWg.Wait()
}

// runSubscriptionLoop listens for admin.settings.updated events and triggers
// Refresh + apply on receipt.
func (o *OperationalSettings) runSubscriptionLoop(ctx context.Context, ch <-chan Event, server *Server) {
	o.runSubscriptionLoopForAttachment(ctx, ch, server, o.decisionAuditPropagationAttachment())
}
func (o *OperationalSettings) runSubscriptionLoopForAttachment(ctx context.Context, ch <-chan Event, server *Server, attachment decisionAuditPropagationAttachment) {
	defer o.loseDecisionAuditPropagation(attachment)
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
			o.refreshAndApplyForAttachment(ctx, server, &attachment)
		}
	}
}

// runPollBackstop runs a ticker at the configured PollInterval (default 60s,
// with ±10s jitter) that calls Refresh and applies any changes. This is the
// backstop for missed NOTIFY events (design §3.6). It also runs on SQLite,
// where it is a cheap re-read of the local DB.
func (o *OperationalSettings) runPollBackstop(ctx context.Context, server *Server) {
	o.runPollBackstopForAttachment(ctx, server, o.decisionAuditPropagationAttachment())
}
func (o *OperationalSettings) runPollBackstopForAttachment(ctx context.Context, server *Server, attachment decisionAuditPropagationAttachment) {
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
			o.refreshAndApplyForAttachment(ctx, server, &attachment)
		}
	}
}

// refreshAndApply performs a Refresh and applies any changed sections to the
// server. Idempotent: applying the same snapshot twice is harmless because
// ApplySnapshot writes the same values and ApplyMaintenanceFromSnapshot
// is idempotent by design.
func (o *OperationalSettings) refreshAndApply(ctx context.Context, server *Server) {
	o.refreshAndApplyForAttachment(ctx, server, nil)
}
func (o *OperationalSettings) refreshAndApplyForAttachment(ctx context.Context, server *Server, attachment *decisionAuditPropagationAttachment) {
	changed, err := o.refreshForDecisionAuditAttachment(ctx, attachment)
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
	snap.DefaultUserRole = k.String("server.auth.default_user_role")
	snap.AuthorizedDomains = k.Strings("server.auth.authorized_domains")

	// Lifecycle
	snap.AutoSuspendStalled = k.Bool("server.hub.auto_suspend_stalled")
	snap.StalledThreshold = k.String("server.hub.stalled_threshold")
	snap.SoftDeleteRetention = k.String("server.hub.soft_delete_retention")
	snap.SoftDeleteRetainFiles = k.Bool("server.hub.soft_delete_retain_files")
	snap.StartClaimLeaseTTL = k.String("server.hub.start_claim_lease_ttl")
	snap.StartMaxDuration = k.String("server.hub.start_max_duration")
	snap.StartUnconfirmedHold = k.String("server.hub.start_unconfirmed_hold")
	snap.StartCreateUnconfirmedHold = k.String("server.hub.start_create_unconfirmed_hold")

	// GCP service-account permission check
	snap.GCPIAMCheckMode = k.String(gcpIAMCheckModeKey)
	snap.GCPIAMDenyUnknownPolicy = k.String(gcpIAMDenyUnknownKey)

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

	// Quotas
	if k.Exists("quotas.enforce_broker_quotas") {
		v := k.Bool("quotas.enforce_broker_quotas")
		snap.EnforceBrokerQuotas = &v
	}

	// Agent secrets
	if k.Exists("agent_secrets.user_scope_only") {
		v := k.Bool("agent_secrets.user_scope_only")
		snap.AgentSecretsUserScopeOnly = &v
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
	snap.DefaultRuntimeBroker = k.String("default_runtime_broker")
	snap.DefaultTimezone = k.String("default_timezone")
	snap.DefaultGCPIdentityMode = k.String("default_gcp_identity_mode")
	snap.DefaultGCPIdentityServiceAccountID = k.String("default_gcp_identity_service_account_id")
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
// GlobalConfig, i.e. from settings.yaml + env. This is used only by a hub
// with no OperationalSettings (no DB tier for operational settings).
//
// NOTE: Only fields that the old reloadSettings() consumed are populated here.
// Fields like SoftDeleteRetention, DefaultTemplate, DefaultMaxTurns, PublicURL,
// ImageRegistry, DefaultResources, and NotificationChannels remain at zero
// values — the old reloadSettings never applied those on config reload (they
// are consumed at startup, not on reload). With OperationalSettings (any DB
// driver), the full koanf-based Snapshot() populates all fields. See the Layer1Snapshot type comment for details.
//
// Exception: DefaultHarnessConfig, DefaultTimezone, DefaultGCPIdentityMode and
// DefaultGCPIdentityServiceAccountID are populated from GlobalConfig so that
// hubAgentDefaults() reflects them in file mode, including immediately after a
// file-mode admin PUT (reloadSettings).
//
// HubName is populated too: "" in a snapshot makes ApplySnapshot reset the
// running name to the startup name, so leaving it out would rename a
// configured hub on every file-mode reload.
func BuildLayer1SnapshotFromFile(gc *config.GlobalConfig) Layer1Snapshot {
	snap := Layer1Snapshot{
		AdminEmails:        gc.Hub.AdminEmails,
		UserAccessMode:     gc.Auth.UserAccessMode,
		DefaultUserRole:    gc.Auth.DefaultUserRole,
		AuthorizedDomains:  gc.Auth.AuthorizedDomains,
		AutoSuspendStalled: gc.Hub.AutoSuspendStalled,
		TelemetryEnabled:   gc.TelemetryEnabled,
		AdminMode:          gc.AdminMode,
		MaintenanceMessage: gc.MaintenanceMessage,
		// The configured hub_name ("" when unset); ApplySnapshot resolves
		// "" to the startup default, as at startup.
		HubName: gc.Hub.HubName,
	}

	if gc.TelemetryConfig != nil {
		snap.TelemetryConfig = gc.TelemetryConfig
	}

	// Start claim timing: applied on reload in file mode too.
	durStr := func(d time.Duration) string {
		if d > 0 {
			return d.String()
		}
		return ""
	}
	snap.StartClaimLeaseTTL = durStr(gc.Hub.StartClaimLeaseTTL)
	snap.StartMaxDuration = durStr(gc.Hub.StartMaxDuration)
	snap.StartUnconfirmedHold = durStr(gc.Hub.StartUnconfirmedHold)
	snap.StartCreateUnconfirmedHold = durStr(gc.Hub.StartCreateUnconfirmedHold)

	// GitHub App (non-secret)
	snap.GitHubAppID = gc.GitHubApp.AppID
	snap.GitHubAPIBaseURL = gc.GitHubApp.APIBaseURL
	snap.GitHubWebhooksEnabled = gc.GitHubApp.WebhooksEnabled
	snap.GitHubInstallationURL = gc.GitHubApp.InstallationURL
	snap.GitHubPrivateKeyPath = gc.GitHubApp.PrivateKeyPath

	// Project defaults — read from settings.yaml project_defaults section
	snap.DefaultScratchpad = gc.DefaultScratchpad

	// Quotas — read from settings.yaml top-level quotas section, so a
	// file-mode admin save takes effect without a restart (unlike
	// AutoExposePortsEnabled, which is intentionally not populated here).
	snap.EnforceBrokerQuotas = gc.EnforceBrokerQuotas

	// Agent secrets — read from settings.yaml top-level agent_secrets
	// section, so a file-mode admin save takes effect without a restart.
	snap.AgentSecretsUserScopeOnly = gc.AgentSecretsUserScopeOnly

	// Agent defaults — read from settings.yaml top-level keys
	snap.DefaultHarnessConfig = gc.DefaultHarnessConfig
	snap.DefaultTimezone = gc.DefaultTimezone
	snap.DefaultGCPIdentityMode = gc.DefaultGCPIdentityMode
	snap.DefaultGCPIdentityServiceAccountID = gc.DefaultGCPIdentityServiceAccountID

	// Federation — read from GlobalConfig
	if gc.Federation.Enabled || len(gc.Federation.TrustedIssuers) > 0 {
		snap.FederationConfig = &gc.Federation
	}

	return snap
}

// boolPtrEqual reports whether two *bool values are equal, treating nil as a
// distinct value from both true and false (unlike dereferencing, which would
// panic on nil, or treating nil as false, which would conflate "unset" with
// "explicitly false").
func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// startupHubNameOrDefault returns the hub name resolved at startup, or the
// startup default (config.ResolveHubNameOrDefault) for a Server not built
// by New.
func (s *Server) startupHubNameOrDefault() string {
	return config.ResolveHubNameOrDefault(s.startupHubName)
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

	// GCP service-account permission check: decided before s.mu is taken
	// (the decision reads the store), applied below under s.mu.
	iamT, iamOK := s.decideGCPIAMReload(snap)

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

	// Quotas. Unlike the other *bool settings above, nil here is a real,
	// meaningful value — the fail-safe default (enforced) — not "unset,
	// leave the current value alone". So this assigns unconditionally: a
	// snapshot with EnforceBrokerQuotas==nil (switch cleared, section
	// deleted, or a PUT of {}) must flip the live hub back to enforced, not
	// silently keep an old in-memory `false` in place while GET/the UI both
	// report "enforced" (findings F3).
	oldEnforceBrokerQuotas := s.config.EnforceBrokerQuotas
	s.config.EnforceBrokerQuotas = snap.EnforceBrokerQuotas
	if !boolPtrEqual(oldEnforceBrokerQuotas, snap.EnforceBrokerQuotas) {
		applied = append(applied, "enforce_broker_quotas")
	}

	// Agent secrets. Like quotas above, nil is a real, meaningful value —
	// the permissive default (agents may write project scope) — not
	// "unset, leave the current value alone". So this assigns
	// unconditionally: a snapshot with AgentSecretsUserScopeOnly==nil
	// (switch cleared, section deleted, or a PUT of {}) must flip live
	// enforcement off immediately.
	oldAgentSecretsUserScopeOnly := s.config.AgentSecretsUserScopeOnly
	s.config.AgentSecretsUserScopeOnly = snap.AgentSecretsUserScopeOnly
	if !boolPtrEqual(oldAgentSecretsUserScopeOnly, snap.AgentSecretsUserScopeOnly) {
		applied = append(applied, "agent_secrets_user_scope_only")
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

	// Start claim timing. The base is the startup value, so removing a
	// setting reverts to it.
	sc, scWarns := parseStartClaimSettings(s.config.StartClaim, snap.StartClaimLeaseTTL, snap.StartMaxDuration, snap.StartUnconfirmedHold, snap.StartCreateUnconfirmedHold)
	for _, w := range scWarns {
		slog.Warn("start claim setting: " + w)
	}
	if s.setStartClaimSettings(sc) {
		applied = append(applied, "start_claim")
	}

	// GCP service-account permission check: both keys are applied together
	// under s.mu, only when admitted, and only if the applied pair is still
	// the one the decision started from.
	iamChanged := false
	if iamOK && s.gcpIAMSettingsLocked() == iamT.From {
		iamChanged = s.applyGCPIAMSettingsLocked(iamT.To)
	}
	if s.gcpIAMApproved != nil && *s.gcpIAMApproved == iamT {
		s.gcpIAMApproved = nil
	}
	if iamChanged {
		applied = append(applied, gcpIAMSection)
	}

	// User access mode
	if snap.UserAccessMode != "" {
		s.config.UserAccessMode = snap.UserAccessMode
		applied = append(applied, "user_access_mode")
	} else if s.config.UserAccessMode != "" {
		s.config.UserAccessMode = ""
		applied = append(applied, "user_access_mode")
	}

	// Default user role. Writes are validated (schema enum in DB mode,
	// explicit check in the file-mode PUT), but a hand-edited settings.yaml
	// or DB row can still hold garbage. Normalize anything other than
	// member/viewer to member so the live config never grants an
	// unexpected role at user creation (design D6). The rule itself lives in
	// normalizedDefaultRole; "" stays unset here (clear semantics).
	defaultRole := snap.DefaultUserRole
	if defaultRole != "" {
		if n := normalizedDefaultRole(defaultRole); n != defaultRole {
			slog.Warn("invalid default_user_role, using member",
				"configured", defaultRole, "allowed", []string{store.UserRoleMember, store.UserRoleViewer})
			defaultRole = n
		}
	}
	if defaultRole != "" {
		s.config.DefaultUserRole = defaultRole
		applied = append(applied, "default_user_role")
	} else if s.config.DefaultUserRole != "" {
		s.config.DefaultUserRole = ""
		applied = append(applied, "default_user_role")
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

	// Hub name: the configured value, or when unset the name this replica
	// resolved at startup, so a cleared hub_name does not leave a stale
	// managed name in use and a name set only through --config survives.
	// Reported as applied only when it changes. (Other Layer-1 keys set
	// only in a --config file are still overridden; tracked in ptone/scion#3070.)
	hubName := snap.HubName
	if hubName == "" {
		hubName = s.startupHubNameOrDefault()
	}
	if s.config.HubName != hubName {
		s.config.HubName = hubName
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
	// so that clearing a value in the DB clears it here too. In file mode,
	// BuildLayer1SnapshotFromFile populates DefaultHarnessConfig,
	// DefaultTimezone and the two GCP identity defaults; other agent-defaults
	// fields remain at zero values in file mode, so this assignment is a
	// no-op for those fields and file-mode dispatch is unchanged.
	newDefaults := opsettings.AgentDefaultsSettings{
		DefaultTemplate:                    snap.DefaultTemplate,
		DefaultHarnessConfig:               snap.DefaultHarnessConfig,
		DefaultMaxTurns:                    snap.DefaultMaxTurns,
		DefaultMaxModelCalls:               snap.DefaultMaxModelCalls,
		DefaultMaxDuration:                 snap.DefaultMaxDuration,
		DefaultModel:                       snap.DefaultModel,
		DefaultThinkingLevel:               snap.DefaultThinkingLevel,
		DefaultRuntimeBroker:               snap.DefaultRuntimeBroker,
		DefaultTimezone:                    snap.DefaultTimezone,
		DefaultGCPIdentityMode:             snap.DefaultGCPIdentityMode,
		DefaultGCPIdentityServiceAccountID: snap.DefaultGCPIdentityServiceAccountID,
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

	if iamChanged {
		s.invalidateCallerPermissionCaches()
	}

	// Propagate hub_name to the GCP secret backend so new secrets get the
	// correct label value. Log handlers have a similar limitation (§7.4).
	if gcpBackend, ok := s.secretBackend.(*secret.GCPBackend); ok {
		gcpBackend.SetHubName(hubName)
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
	// Maintenance is runtime/API-owned state. On a hub without
	// OperationalSettings, reloadSettings must never touch MaintenanceState
	// (restoring pre-refactor behavior). With OperationalSettings (any DB
	// driver), the caller uses ApplyMaintenanceFromSnapshot separately.

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

// ApplyMaintenanceFromSnapshot applies maintenance state from a DB-backed
// (any driver) snapshot.
//
// This function must be called ONLY on DB-backed paths (any driver) — the
// file-mode reloadSettings must never touch MaintenanceState (it is
// runtime/API-owned).
//
// Behavior:
//   - If snap.HasMaintenanceRow is false (no DB row): no-op — MaintenanceState
//     keeps its current value (which honors the startup admin mode).
//   - If snap.HasMaintenanceRow is true on a hosted hub: apply the DB values.
//     Maintenance must be cluster-consistent, so there is no per-node
//     override.
//   - If snap.HasMaintenanceRow is true on a workstation hub whose startup
//     admin mode is on (SCION_SERVER_ADMIN_MODE=true or settings.yaml
//     admin_mode: true): the hub stays in maintenance — the break-glass wins
//     over the row for the life of the process (ptone/scion#1091 option C).
//     The row's message is still used when it has one.
func ApplyMaintenanceFromSnapshot(s *Server, snap Layer1Snapshot) {
	if !snap.HasMaintenanceRow {
		return
	}

	if s.maintenanceBreakGlass() {
		_, msg := s.maintenance.State()
		if snap.MaintenanceMessage != "" {
			msg = snap.MaintenanceMessage
		}
		s.maintenance.Set(true, msg)
		return
	}
	s.maintenance.Set(snap.AdminMode, snap.MaintenanceMessage)
}

// maintenanceBreakGlass reports whether a workstation hub was started in
// admin mode (SCION_SERVER_ADMIN_MODE=true or settings.yaml admin_mode: true).
// That startup state wins over a DB maintenance row; on hosted hubs the row
// wins.
func (s *Server) maintenanceBreakGlass() bool {
	return s.workstation && s.config.AdminMode
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

// ConversationEnvelopeSwitch returns whether the consolidated conversation
// envelope switch is enabled. This replaces ConversationReadSwitch and
// ConversationWriteDenySwitch with a single switch that:
//   - defaults ON when the section is absent from the DB (compiled default)
//   - defaults ON when the section is present but the key is omitted
//   - returns OFF when the document is malformed (fail-closed, DEF-92)
//   - returns the explicit value when the key is present
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) ConversationEnvelopeSwitch() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache["messaging"]
	if !ok {
		return true // section absent → compiled default → ON
	}

	if state.Malformed {
		return false // unreadable → pre-refactor behaviour → OFF
	}

	var ms opsettings.MessagingSettings
	if err := json.Unmarshal(state.Value, &ms); err != nil {
		return false // parse error → fail closed → OFF
	}

	if ms.ConversationEnvelopeSwitch != nil {
		return *ms.ConversationEnvelopeSwitch
	}
	return true // field omitted in doc → compiled default → ON
}

// OffloadThresholdRunes returns the rune-count threshold above which an
// agent-recipient DM body is offloaded to a fetch stub at dispatch
// (ptone/scion#2257, design auto-offload-large-dm §5, §8.1). Returns 0
// (disabled) when the section is absent, the document is malformed, the
// field is omitted, or the stored value is negative — matching
// messaging.OffloadPolicy's "<= 0 disables" contract.
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) OffloadThresholdRunes() int {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache["messaging"]
	if !ok {
		return 0 // section absent → compiled default → disabled
	}
	if state.Malformed {
		return 0 // unreadable → fail closed → disabled
	}

	var ms opsettings.MessagingSettings
	if err := json.Unmarshal(state.Value, &ms); err != nil {
		return 0 // parse error → fail closed → disabled
	}

	if ms.OffloadThresholdRunes == nil {
		return 0 // field omitted → compiled default → disabled
	}
	if *ms.OffloadThresholdRunes < 0 {
		return 0
	}
	return *ms.OffloadThresholdRunes
}

// SectionRevision returns the current revision of the named settings section.
// Returns 0 if the section does not exist or operational settings are unavailable.
func (o *OperationalSettings) SectionRevision(section string) int64 {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache[section]
	if !ok {
		return 0
	}
	return state.Revision
}

// CrossProjectMessagingEnabled returns whether cross-project agent messaging
// is enabled on this Hub. Default is false (off). This is a security-critical
// control: when disabled, no cross-project agent messages are authorized.
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) CrossProjectMessagingEnabled() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache["messaging"]
	if !ok {
		return false // section absent → compiled default → OFF
	}

	if state.Malformed {
		return false // unreadable → fail closed → OFF
	}

	var ms opsettings.MessagingSettings
	if err := json.Unmarshal(state.Value, &ms); err != nil {
		return false // parse error → fail closed → OFF
	}

	if ms.CrossProjectMessagingEnabled != nil {
		return *ms.CrossProjectMessagingEnabled
	}
	return false // field omitted → compiled default → OFF
}

// ReadinessMarks returns whether the web client writes readiness marks
// (the "profiling" section's readiness_marks key). It is false when the
// section is absent, the document is malformed or the key is unset.
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) ReadinessMarks() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache["profiling"]
	if !ok || state.Malformed {
		return false
	}
	var ps opsettings.ProfilingSettings
	if err := json.Unmarshal(state.Value, &ps); err != nil {
		return false
	}
	return ps.ReadinessMarks != nil && *ps.ReadinessMarks
}

// Artifacts returns the resolved artifact service settings (the
// "artifacts" section). It returns opsettings.DefaultArtifactsConfig when
// the section is absent, and fails closed (opsettings.MalformedArtifactsConfig:
// service disabled, compiled-default limits) when the stored document is
// unreadable or holds an invalid value.
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) Artifacts() opsettings.ArtifactsConfig {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache["artifacts"]
	if !ok {
		return opsettings.DefaultArtifactsConfig() // section absent → compiled defaults
	}
	if state.Malformed {
		return opsettings.MalformedArtifactsConfig() // unreadable → fail closed
	}
	cfg, err := opsettings.ParseArtifactsDoc(state.Value)
	if err != nil {
		return opsettings.MalformedArtifactsConfig() // invalid value → fail closed
	}
	if cfg.RemoteImagesInvalid != "" && o.remoteImagesWarnedRev.Swap(state.Revision) != state.Revision {
		// Writes refuse such a value; a stored one (written before that
		// check existed) turns remote images off. Say so once per revision.
		slog.Warn("artifacts settings: remote images are off because a remote image setting is invalid",
			"problem", cfg.RemoteImagesInvalid, "revision", state.Revision)
	}
	return cfg
}

// CrossProjectSettingResult holds the authoritative cross-project messaging
// setting and its revision, read directly from the store (not the cache).
type CrossProjectSettingResult struct {
	Enabled  bool
	Revision int64
	Err      error
}

// ReadAuthoritativeCrossProjectEnabled reads the cross_project_messaging_enabled
// setting directly from the store, bypassing the replica-local cache. This
// provides an authoritative read for security-critical cross-project admission
// decisions, ensuring that the very next send after a policy change enforces the
// current setting regardless of cache/notification propagation state.
//
// Defaults:
//   - Section absent from store → false (off), revision 0.
//   - Section present but field omitted → false (off).
//   - Malformed JSON → false (fail closed).
//   - Store read error → false with Err set (fail closed).
func (o *OperationalSettings) ReadAuthoritativeCrossProjectEnabled(ctx context.Context) CrossProjectSettingResult {
	setting, err := o.store.GetHubSetting(ctx, "messaging")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Section absent → compiled default → OFF.
			return CrossProjectSettingResult{Enabled: false, Revision: 0}
		}
		// Store infrastructure error → fail closed.
		slog.Warn("ReadAuthoritativeCrossProjectEnabled: store read failed, failing closed",
			"error", err)
		return CrossProjectSettingResult{Enabled: false, Err: fmt.Errorf("authoritative setting read: %w", err)}
	}

	if !json.Valid(setting.Value) {
		// Malformed document → fail closed.
		slog.Warn("ReadAuthoritativeCrossProjectEnabled: malformed JSON in messaging section, failing closed",
			"revision", setting.Revision)
		return CrossProjectSettingResult{Enabled: false, Revision: setting.Revision}
	}

	var ms opsettings.MessagingSettings
	if err := json.Unmarshal(setting.Value, &ms); err != nil {
		// Parse error → fail closed.
		slog.Warn("ReadAuthoritativeCrossProjectEnabled: failed to unmarshal messaging section, failing closed",
			"revision", setting.Revision, "error", err)
		return CrossProjectSettingResult{Enabled: false, Revision: setting.Revision}
	}

	if ms.CrossProjectMessagingEnabled != nil {
		return CrossProjectSettingResult{
			Enabled:  *ms.CrossProjectMessagingEnabled,
			Revision: setting.Revision,
		}
	}
	// Field omitted → compiled default → OFF.
	return CrossProjectSettingResult{Enabled: false, Revision: setting.Revision}
}

// ExperimentsSnapshot is one consistent view of the cached "experiments"
// section, taken under a single RLock, so revision, overrides, malformed
// flag and metadata always belong to the same refresh.
type ExperimentsSnapshot struct {
	// Overrides is a copy of the stored admin overrides; empty when
	// malformed or absent. May contain names this binary does not know
	// (ptone/scion#2217).
	Overrides map[string]bool
	Revision  int64
	Malformed bool
	UpdatedAt time.Time
	UpdatedBy string
	// Present is false when no row exists.
	Present bool
}

// ExperimentsSnapshot returns one consistent view of the cached "experiments"
// section. Read path: it never parses JSON. state.ExperimentsOverrides is
// parsed once, when the cache entry is written (Refresh, Update); this only
// clones that already-parsed map, so a caller mutating the returned map can
// never affect another caller or a later snapshot. No logging here (Refresh
// logs once per ingest).
func (o *OperationalSettings) ExperimentsSnapshot() ExperimentsSnapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.experimentsSnapshotLocked()
}

// Caller holds mu. Snapshot mapping and cloning retain their existing semantics.
func (o *OperationalSettings) experimentsSnapshotLocked() ExperimentsSnapshot {
	state, ok := o.cache["experiments"]
	if !ok {
		return ExperimentsSnapshot{Overrides: map[string]bool{}}
	}

	snap := ExperimentsSnapshot{
		Revision:  state.Revision,
		Malformed: state.Malformed,
		UpdatedAt: state.UpdatedAt,
		UpdatedBy: state.UpdatedBy,
		Present:   true,
	}
	snap.Overrides = maps.Clone(state.ExperimentsOverrides)
	if snap.Overrides == nil {
		snap.Overrides = map[string]bool{}
	}
	return snap
}

// ExperimentsReadResult holds the authoritative experiments overrides and
// revision, read directly from the store (not the cache).
type ExperimentsReadResult struct {
	Overrides map[string]bool
	Revision  int64
	Malformed bool
	Err       error
}

// ReadAuthoritativeExperiments reads the "experiments" section straight from
// the store, bypassing the replica-local cache. Write path only; the
// precedent is ReadAuthoritativeCrossProjectEnabled.
//
//	row absent (store.ErrNotFound)             → {Overrides: {}, Revision: 0}
//	row present, ParseExperimentsDoc ok        → {Overrides, Revision}
//	row present, ParseExperimentsDoc malformed → {Overrides: {}, Revision, Malformed: true}
//	store error                                → {Err}
func (o *OperationalSettings) ReadAuthoritativeExperiments(ctx context.Context) ExperimentsReadResult {
	setting, err := o.store.GetHubSetting(ctx, "experiments")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ExperimentsReadResult{Overrides: map[string]bool{}, Revision: 0}
		}
		slog.Warn("ReadAuthoritativeExperiments: store read failed", "error", err)
		return ExperimentsReadResult{Err: fmt.Errorf("authoritative experiments read: %w", err)}
	}

	doc, malformed := opsettings.ParseExperimentsDoc(setting.Value)
	if malformed {
		return ExperimentsReadResult{Overrides: map[string]bool{}, Revision: setting.Revision, Malformed: true}
	}
	overrides := doc.Overrides
	if overrides == nil {
		overrides = map[string]bool{}
	}
	return ExperimentsReadResult{Overrides: overrides, Revision: setting.Revision}
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

// Lifecycle authority belongs to this captured router/source attachment, never
// whichever attachment happens to be live when an old callback finally runs.
type decisionAuditPropagationAttachment struct {
	observer   *decisionAuditRouter
	attachment uint64
}

func (o *OperationalSettings) decisionAuditPropagationAttachment() decisionAuditPropagationAttachment {
	observer := o.decisionAuditObserver.Load()
	if observer == nil {
		return decisionAuditPropagationAttachment{}
	}
	return decisionAuditPropagationAttachment{observer: observer, attachment: observer.propagationAttachment(o)}
}
func (o *OperationalSettings) loseDecisionAuditPropagation(attachment decisionAuditPropagationAttachment) {
	if attachment.observer == nil {
		return
	}
	o.mu.Lock()
	cancel, current := attachment.observer.losePropagation(o, attachment.attachment)
	if current {
		o.decisionAuditObservation = decisionAuditRefreshObservation{}
	}
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (o *OperationalSettings) decisionAuditSnapshot() (ExperimentsSnapshot, decisionAuditRefreshObservation) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	// Reject before cloning if a concurrent generic write installed uncapped data.
	state, present := o.cache["experiments"]
	if !present || len(state.Value) > decisionAuditSettingsMaxBytes || len(state.ExperimentsOverrides) > decisionAuditSettingsMaxRows {
		return ExperimentsSnapshot{}, decisionAuditRefreshObservation{}
	}
	size := 0
	for name := range state.ExperimentsOverrides {
		if len(name) > decisionAuditSettingsMaxBytes-size {
			return ExperimentsSnapshot{}, decisionAuditRefreshObservation{}
		}
		size += len(name)
	}
	observation := o.decisionAuditObservation
	observation.snapshot = cloneDecisionAuditSnapshot(observation.snapshot)
	return o.experimentsSnapshotLocked(), observation
}

func (o *OperationalSettings) beginDecisionAuditMutation() {
	o.mu.Lock()
	if o.decisionAuditMutations < 2 {
		o.decisionAuditMutations++
	}
	o.decisionAuditObservation = decisionAuditRefreshObservation{}
	if observer := o.decisionAuditObserver.Load(); observer != nil {
		observer.mutation(o, true)
	}
	o.mu.Unlock()
}

func (o *OperationalSettings) endDecisionAuditMutation() {
	o.mu.Lock()
	if o.decisionAuditMutations > 0 {
		o.decisionAuditMutations--
	}
	o.decisionAuditObservation = decisionAuditRefreshObservation{}
	if observer := o.decisionAuditObserver.Load(); observer != nil {
		observer.mutation(o, false)
	}
	o.mu.Unlock()
}
