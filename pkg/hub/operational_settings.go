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
	"reflect"
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

	// settingsRev is the operational-settings revision the entry is known
	// at: the revision of the Refresh read that cached it, or the revision
	// of the Update that wrote it (0 for a store without revisions).
	// Unlike Revision (a counter on the row, restarted at 1 when a deleted
	// row is created again), it is global and only grows, so it orders an
	// entry against a write of the same section (ptone/scion#5762).
	settingsRev int64
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
	AutoSuspendStalled  bool
	StalledThreshold    string // DB-backed snapshots only (see type comment)
	SoftDeleteRetention string // DB-backed snapshots only (see type comment); "" = startup value
	// SoftDeleteRetainFiles is nil when unset; ApplySnapshot then uses the
	// startup value. DB-backed snapshots only (see type comment).
	SoftDeleteRetainFiles *bool

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

	// Hub observability export switches (hub_observability section); nil
	// when unset. Read once at startup: the exporters are created then,
	// so ApplySnapshot does not apply them (restart_required).
	HubMetricsEnabled *bool
	HubTracingEnabled *bool

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
	// PublicURL is, in a DB-backed snapshot, the server.hub.public_url an
	// admin saved in a managed endpoints row, else a bootstrap value. When
	// the server has set the unseeded bootstrap (SetUnseededBootstrap, as
	// startup does), that value comes from the merge without the
	// SCION_SEED_* layer (settings.yaml and the environment), so a seed
	// value, which startup ignores, is excluded (ptone/scion#4826). When it
	// is unset (tests, embedded servers), the full bootstrap merge, seed
	// layer included, is used. The endpoint in effect is
	// Server.HubEndpoints.
	PublicURL string
	// HubName is the configured hub_name. "" does NOT mean "leave alone":
	// ApplySnapshot resets the running name (and the GCP secret label) to
	// this replica's startup name. Every snapshot constructor must set it.
	HubName       string
	ImageRegistry string
	// MonitoringDashboardURL is server.hub.monitoring_dashboard_url; ""
	// means unset (no Health page link). Every snapshot constructor sets it.
	MonitoringDashboardURL string

	// GitHub App (non-secret fields only)
	GitHubAppID           int64
	GitHubAPIBaseURL      string
	GitHubWebhooksEnabled bool
	GitHubInstallationURL string
	GitHubPrivateKeyPath  string

	// Notifications
	NotificationChannels []config.V1NotificationChannelConfig

	// FromOperationalSettings marks a DB-backed snapshot
	// (OperationalSettings.Snapshot), which carries the complete set of
	// Layer-1 values. ApplySnapshot then treats an unset value as cleared
	// and returns it to the startup value, and rebuilds the notification
	// channel registry from NotificationChannels (an empty list removes
	// every channel). A snapshot built from the file
	// (BuildLayer1SnapshotFromFile) leaves it false: there an unset value
	// keeps the running value, and the registry is left as it is.
	FromOperationalSettings bool

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

	// Revision is the operational-settings revision the snapshot's values
	// come from (store.HubSettingsRevisionSection): a counter the store
	// bumps in the same transaction as every operational-settings write.
	// OperationalSettings.Snapshot sets it together with the values, under
	// one lock. ApplySnapshot skips a DB-backed snapshot whose Revision is
	// older than the last one applied, so a snapshot built before a newer
	// one never overwrites it (ptone/scion#4457). 0 means no revision: a
	// file snapshot, a store that keeps none, or a database with no
	// operational write yet.
	Revision int64

	// Seq is the OperationalSettings cache sequence the snapshot was built
	// at: it grows with every change of the cache, including a write
	// cached at a revision below the cached high-water mark, which leaves
	// Revision unchanged (ptone/scion#5762). ApplySnapshot orders
	// DB-backed snapshots by (Revision, Seq), so a snapshot built before
	// such a write never overwrites the writer's self-apply. 0 for a
	// snapshot not built by OperationalSettings.Snapshot.
	Seq uint64

	// Generation counts the store resets OperationalSettings resynchronised
	// to (a stored revision that went backward, ptone/scion#5667).
	// ApplySnapshot orders DB-backed snapshots by (Generation, Revision,
	// Seq), so after a reset the lower revisions of the restored store are
	// applied, and a snapshot built before the reset is not.
	Generation uint64
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

	// unseededPublicURL is server.hub.public_url from the bootstrap merge
	// without the SCION_SEED_* layer, valid when hasUnseededPublicURL
	// (SetUnseededBootstrap). Guarded by mu.
	unseededPublicURL    string
	hasUnseededPublicURL bool
	// unseededKoanf is the bootstrap merge without the SCION_SEED_* layer
	// (SetUnseededBootstrap), nil when unset. computeSupersededKeys uses it
	// to tell whether the seed layer gave a key its bootstrap value.
	// Guarded by mu.
	unseededKoanf *koanf.Koanf
	// aboveSeedKoanf is the bootstrap merge with only the layers above
	// SCION_SEED_* (settings files and SCION_SERVER_*), without the coded
	// defaults (SetAboveSeedBootstrap), nil when unset. computeSupersededKeys
	// uses it to tell a coded default from a value set in those layers.
	// Guarded by mu.
	aboveSeedKoanf *koanf.Koanf

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

	// revision is the highest operational-settings revision
	// (store.HubSettingsRevisionSection) seen by Refresh, Update or
	// DeleteSection; 0 until the store reports one. It only grows, except
	// when Refresh resynchronises to a store whose revision went backward
	// (ptone/scion#5667). It is a
	// high-water mark, not a promise that the cache holds every write up
	// to it (Update and DeleteSection raise it while changing one
	// section). Guarded by mu.
	revision int64

	// readRev is the revision of the last Refresh read taken into the
	// cache. Every section absent from the cache was absent at readRev (a
	// read holds every operational write up to its revision), unless a
	// later write cached it. Guarded by mu.
	readRev int64
	// tombstones records, for a section DeleteSection evicted, the
	// revision of the delete, so a missing entry after a later delete can
	// be told apart from a section that was never cached. An accepted
	// Refresh clears them all: its read is at or above every one of them.
	// Guarded by mu.
	tombstones map[string]int64
	// cacheSeq grows with every change of the cache or of revision; the
	// snapshot carries it (Layer1Snapshot.Seq). Refresh also uses it to
	// tell a store reset from a read that raced a newer one. Guarded by mu.
	cacheSeq uint64
	// generation counts the store resets Refresh resynchronised to; the
	// snapshot carries it (Layer1Snapshot.Generation). Guarded by mu.
	generation uint64

	// writeGuard, when set, is asked before every section write or delete
	// (Update, DeleteSection); a non-nil error refuses the write. The
	// server installs the test-admin policy guard here
	// (Server.testAdminSettingsWriteGuard), so every settings write path
	// refuses test identities with admin roles.
	writeGuard atomic.Pointer[settingsWriteGuard]
}

// settingsWriteGuard decides whether the caller in ctx may write section.
type settingsWriteGuard func(ctx context.Context, section string) error

// setWriteGuard installs the write guard (nil removes it).
func (o *OperationalSettings) setWriteGuard(g settingsWriteGuard) {
	if g == nil {
		o.writeGuard.Store(nil)
		return
	}
	o.writeGuard.Store(&g)
}

// checkWriteGuard runs the write guard for section, if one is installed.
func (o *OperationalSettings) checkWriteGuard(ctx context.Context, section string) error {
	if g := o.writeGuard.Load(); g != nil && *g != nil {
		return (*g)(ctx, section)
	}
	return nil
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
		tombstones:     make(map[string]int64),
	}
}

// SetUnseededBootstrap records the bootstrap merge without the SCION_SEED_*
// layer (config.LoadUnseededBootstrapKoanfWithConfigPath). The hub's
// endpoint resolution at startup ignores seed values, so with no saved
// public URL, Snapshot takes the public URL the admin view shows from this
// merge rather than from the full bootstrap merge (ptone/scion#4826).
// The admin view also uses it to attribute a superseded key to the seed
// layer only when that layer gave the key its value (ptone/scion#4904).
// Without it (tests, embedded servers) the full bootstrap merge is used.
func (o *OperationalSettings) SetUnseededBootstrap(k *koanf.Koanf) {
	if k == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.unseededKoanf = k
	o.unseededPublicURL = k.String(config.PublicURLKey)
	o.hasUnseededPublicURL = true
}

// SetAboveSeedBootstrap records the bootstrap merge with only the layers
// above SCION_SEED_*, without the coded defaults
// (config.LoadAboveSeedBootstrapKoanfWithConfigPath). With it and the
// unseeded bootstrap set, the admin view attributes a superseded key that
// none of those layers sets to the seed layer when a seed value exists,
// else to the coded defaults (ptone/scion#4983). Without it (tests,
// embedded servers) such a key is attributed to settings.yaml.
func (o *OperationalSettings) SetAboveSeedBootstrap(k *koanf.Koanf) {
	if k == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.aboveSeedKoanf = k
}

// SettingsRevision returns the cached operational-settings revision, the
// value Snapshot carries in Layer1Snapshot.Revision (0 when the store
// reports none). Settings applies are ordered by it (ApplySnapshot).
//
// It is a cache high-water mark: the cache can lack writes at or below it
// until the next read. So it is only an advisory hint for a client. The
// store revision it mirrors is the shared mechanism for a revision-checked
// save precondition, and such a precondition must compare the client's
// revision with the stored store.HubSettingsRevisionSection revision
// inside the write transaction (for example after locking that row), never
// with this value.
func (o *OperationalSettings) SettingsRevision() int64 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.revision
}

// Refresh re-reads all hub_settings rows from the store, diffs revisions
// against the cache, and returns the names of sections that changed.
//
// The rows and the operational-settings revision come from one read
// (ListHubSettings returns the store.HubSettingsRevisionSection row with
// the sections), so the read holds every operational write up to its
// revision. A read whose revision is below the cached revision because it
// raced a newer read or write is discarded and nothing is reported changed
// (ptone/scion#4457); a read below it with no such race means the stored
// revision went backward, and is resynchronised to (see refresh,
// ptone/scion#5667). The cached
// revision is a high-water mark, not a promise that the cache holds every
// write up to it: Update and DeleteSection raise it while caching one
// section. Discarding is still safe because a later read at or above the
// cached revision is guaranteed: every write that raised it publishes its
// own settings event, whose refresh reads the store after that write
// committed (and the poll backstop reads it again).
func (o *OperationalSettings) Refresh(ctx context.Context) ([]string, error) {
	changed, _, _, err := o.refresh(ctx)
	return changed, err
}

// refresh is Refresh, also returning the revision of the read and whether
// the read was taken into the cache (false when it was discarded as older
// than the cached revision).
//
// A read below the cached revision is normally a read that raced a newer
// one: another Refresh, Update or DeleteSection raised the cached revision
// while the read was in flight, so the cache sequence moved. When the
// sequence did not move, the cached revision was already set before the
// read began, and every value it takes was committed in the store first.
// A read that then sees a lower revision means the stored counter went
// backward: hub_settings was restored from a backup or the database was
// replaced (ptone/scion#5667). Discarding such reads would freeze settings
// on this hub until the counter caught up, so the read is taken as a
// reset: a Warn is logged, the cache and the revision are resynchronised
// to the store, every section is reported changed, and the generation
// moves so ApplySnapshot accepts the lower revision.
//
// A read that began before such a reset (the generation moved while it
// was in flight) is discarded, so state from before the reset never enters
// the new generation.
//
// This relies on every read seeing committed state at least as new as any
// revision this process has observed: one primary database, which is how
// the hub runs (one database URL for reads and writes, no read-replica
// routing). The database URL must target the read-write primary; with
// several hosts, set target_session_attrs=read-write, so no connection
// reads from a lagging standby. A failover to a standby that lost commits
// is a genuine step backward and is resynchronised the same way.
func (o *OperationalSettings) refresh(ctx context.Context) (changed []string, readRevision int64, accepted bool, err error) {
	o.mu.RLock()
	seqAtRead := o.cacheSeq
	genAtRead := o.generation
	o.mu.RUnlock()

	rows, err := o.store.ListHubSettings(ctx)
	if err != nil {
		return nil, 0, false, fmt.Errorf("operational settings refresh: %w", err)
	}

	for _, row := range rows {
		if row.Section == store.HubSettingsRevisionSection {
			readRevision = row.Revision
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	// A read that was in flight while another read resynchronised to a
	// reset store may hold the state from before the reset; it is never
	// taken into the new generation, whatever its revision.
	if o.generation != genAtRead {
		slog.Debug("operational settings refresh: discarding a read that began before a store reset",
			"read_revision", readRevision, "cached_revision", o.revision)
		return nil, readRevision, false, nil
	}
	reset := false
	if readRevision < o.revision {
		if o.cacheSeq != seqAtRead {
			slog.Debug("operational settings refresh: discarding a read below the cached revision",
				"read_revision", readRevision, "cached_revision", o.revision)
			return nil, readRevision, false, nil
		}
		reset = true
		o.generation++
		slog.Warn("operational settings: the stored settings revision went backward (hub_settings restored or the database replaced?); resynchronising this hub's settings to the store",
			"read_revision", readRevision, "cached_revision", o.revision)
	}
	revisionRaised := readRevision != o.revision
	o.revision = readRevision
	o.readRev = readRevision
	clear(o.tombstones)

	seen := make(map[string]bool, len(rows))

	for _, row := range rows {
		if row.Section == "_meta" || row.Section == store.HubSettingsRevisionSection {
			continue
		}
		seen[row.Section] = true

		prev, exists := o.cache[row.Section]
		if reset || !exists || prev.Revision != row.Revision {
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
			settingsRev:          readRevision,
		}
	}

	// Detect deleted sections (in cache but not in DB).
	for name := range o.cache {
		if !seen[name] {
			changed = append(changed, name)
			delete(o.cache, name)
		}
	}
	if reset || revisionRaised || len(changed) > 0 {
		o.cacheSeq++
	}

	return changed, readRevision, true, nil
}

// knownAtLocked is the operational-settings revision at which the cache's
// view of section is known: the entry's settingsRev when it is cached,
// otherwise the later of its delete tombstone and the last accepted read
// (the section was absent then). A write of section at a revision at or
// above it is newer than everything the cache holds for that section and
// can be cached directly; one below it is superseded by a newer read,
// write or delete of the same section (ptone/scion#5762). Caller holds mu.
func (o *OperationalSettings) knownAtLocked(section string) int64 {
	if entry, ok := o.cache[section]; ok {
		return entry.settingsRev
	}
	return max(o.tombstones[section], o.readRev)
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
	revision := o.revision
	seq := o.cacheSeq
	generation := o.generation
	dbSections := make(map[string]json.RawMessage, len(o.cache))
	for name, ss := range o.cache {
		dbSections[name] = ss.Value
	}
	endpoints, hasEndpoints := o.cache["endpoints"]
	bootstrapPublicURL := ""
	if o.hasUnseededPublicURL {
		bootstrapPublicURL = o.unseededPublicURL
	} else if o.bootstrapKoanf != nil {
		bootstrapPublicURL = o.bootstrapKoanf.String(config.PublicURLKey)
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
			slog.Error("operational settings: cannot read bootstrap section", "section", sec.Name, "error", err)
			continue
		}
		if err := loadSectionDocIntoKoanf(merged, sec.Name, doc); err != nil {
			slog.Error("operational settings: cannot load bootstrap section", "section", sec.Name, "error", err)
		}
	}

	// Layer: DB sections (highest precedence — DB wins over bootstrap).
	// A section that cannot be loaded is logged; its keys stay unset
	// (ptone/scion#4108).
	for name, doc := range dbSections {
		if err := loadSectionDocIntoKoanf(merged, name, doc); err != nil {
			slog.Error("operational settings: cannot load section document", "section", name, "error", err)
		}
	}

	snap := buildSnapshotFromKoanf(merged)
	snap.FromOperationalSettings = true
	snap.Revision = revision
	snap.Seq = seq
	snap.Generation = generation

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

	// public_url is the value an admin saved in a managed endpoints row,
	// else the bootstrap value without the SCION_SEED_* layer: with none
	// saved, the hub resolves its endpoint from settings.yaml and the
	// environment at startup (ptone/scion#3905) and ignores seed values
	// (StoredPublicURL). A seeded row is rewritten from the full bootstrap
	// merge, seed layer included, so its value is not used either
	// (ptone/scion#4826).
	snap.PublicURL = managedPublicURL(endpoints, hasEndpoints)
	if snap.PublicURL == "" {
		snap.PublicURL = bootstrapPublicURL
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
// snapshot fields with config.OverlayMapSections: when a DB row exists for
// the section, the doc is deserialized directly (preserving empty maps);
// when no DB row exists, the bootstrap koanf is used as the file-based
// fallback. A DB doc that does not decode is logged and leaves that field
// unchanged.
func (o *OperationalSettings) populateMapSections(snap *Layer1Snapshot, dbSections map[string]json.RawMessage) {
	sections, decodeErrs := config.OverlayMapSections(dbSections, o.bootstrapKoanf)
	failed := map[string]bool{}
	for _, e := range decodeErrs {
		failed[e.Section] = true
		slog.Warn(e.Section+": failed to unmarshal DB doc for snapshot", "error", e.Err)
	}
	if !failed["runtimes"] {
		snap.Runtimes = sections.Runtimes
	}
	if !failed["profiles"] {
		snap.Profiles = sections.Profiles
	}
	if !failed["harness_configs"] {
		snap.HarnessConfigs = sections.HarnessConfigs
	}
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

// ErrSectionValidation is wrapped by Update when the section document fails
// schema validation, so callers can report a client error instead of 500.
var ErrSectionValidation = errors.New("validation failed")

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
	rev, _, err := o.update(ctx, section, doc, updatedBy, expectedRevision, origin)
	return rev, err
}

// update is Update, also reporting whether the cache held the write (or a
// newer state of its section) before the self-apply. That is false only
// when the write could not be cached directly and the re-read of the
// store failed or kept losing to newer writes: the write is saved, and
// this hub applies it on its own settings event or the next poll, but the
// self-apply did not include it (ptone/scion#5691).
func (o *OperationalSettings) update(
	ctx context.Context,
	section string,
	doc json.RawMessage,
	updatedBy string,
	expectedRevision int64,
	origin string,
) (int64, bool, error) {
	if err := o.checkWriteGuard(ctx, section); err != nil {
		return 0, false, err
	}
	// Validate via opsettings registry.
	if errs := opsettings.Validate(section, doc); len(errs) > 0 {
		return 0, false, fmt.Errorf("%w for section %q: %v", ErrSectionValidation, section, errs)
	}

	result, err := o.store.UpsertHubSetting(ctx, section, doc, updatedBy, expectedRevision, origin)
	if err != nil {
		return 0, false, err
	}

	reread := o.cacheWrite(section, result)
	inEffect := true
	if reread {
		inEffect = o.refreshThrough(ctx, result.SettingsRevision, section)
	}
	o.publishAndSelfApply(section, result.Revision)

	return result.Revision, inEffect, nil
}

// cacheWrite caches one written row, as every write path does: the write
// is cached directly when its revision is at or above the revision the
// cache knows this section at (knownAtLocked). It reports whether the
// cache already held newer state of the section, so the store must be
// read again (refreshThrough) before the self-apply.
func (o *OperationalSettings) cacheWrite(section string, result *store.HubSetting) (reread bool) {
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
	// The write is cached directly when its revision is at or above the
	// revision the cache knows this section at (knownAtLocked): the cache
	// then holds nothing newer for the section, even when the cached
	// high-water mark is above the write because of other sections. Below
	// it, a newer read, write or delete of the same section is already
	// cached, so writing this row would replace newer state; the cache is
	// not written, and the store is read again below, before the
	// self-apply (ptone/scion#4457, ptone/scion#5762). A store without
	// revisions (0) is cached directly, as before.
	reread = result.SettingsRevision > 0 && result.SettingsRevision < o.knownAtLocked(section)
	if !reread {
		o.cache[section] = sectionState{
			Value:                result.Value,
			Revision:             result.Revision,
			UpdatedAt:            result.UpdatedAt,
			UpdatedBy:            result.UpdatedBy,
			Origin:               result.Origin,
			Malformed:            malformed,
			ExperimentsOverrides: experimentsOverrides,
			settingsRev:          result.SettingsRevision,
		}
		delete(o.tombstones, section)
		if result.SettingsRevision > o.revision {
			o.revision = result.SettingsRevision
		}
		o.cacheSeq++
	}
	o.mu.Unlock()
	return reread
}

// publishAndSelfApply publishes the settings event of a written section and
// self-applies the current snapshot.
func (o *OperationalSettings) publishAndSelfApply(section string, revision int64) {
	o.publish(section, revision)
	o.selfApply()
}

// publish sends the admin.settings.updated event for a written section, so
// other replicas refresh (design §3.6). On SQLite the publisher is an
// in-process ChannelEventPublisher (single replica); it is nil only before
// StartPropagation is wired, e.g. in tests.
func (o *OperationalSettings) publish(section string, revision int64) {
	if o.events != nil {
		o.events.PublishRaw(settingsUpdatedSubject, SettingsUpdatedEvent{
			Section:  section,
			Revision: revision,
		})
	}
}

// selfApply applies the current snapshot on the writing node synchronously
// rather than waiting for its own event (design §3.6). Double-apply (sync
// here + own event received via subscription) is harmless because
// ApplySnapshot and ApplyMaintenanceFromSnapshot are idempotent.
func (o *OperationalSettings) selfApply() {
	if o.server != nil {
		snap := o.Snapshot()
		ApplySnapshot(o.server, snap)
		ApplyMaintenanceFromSnapshot(o.server, snap)
	}
}

// DeleteSection removes a section row from the store, evicts it from the local
// cache, publishes an event so peers refresh, and self-applies. The section
// falls back to bootstrap material immediately (design §3.2.4).
func (o *OperationalSettings) DeleteSection(ctx context.Context, section string) error {
	if err := o.checkWriteGuard(ctx, section); err != nil {
		return err
	}
	// The delete's own revision comes from the optional
	// store.HubSettingRevisionDeleter, so DeleteHubSetting keeps its
	// signature for its many callers. A store without it gives revision 0.
	var deleteRevision int64
	if rd, ok := o.store.(store.HubSettingRevisionDeleter); ok {
		rev, err := rd.DeleteHubSettingRevision(ctx, section)
		if err != nil {
			return err
		}
		deleteRevision = rev
	} else if err := o.store.DeleteHubSetting(ctx, section); err != nil {
		return err
	}
	o.applyDelete(ctx, section, deleteRevision)
	return nil
}

// deleteGuarded is DeleteSection in a guarded store transaction
// (store.HubSettingGuardedBatchWriter): the row must still be at
// expectedRevision and each section in reads at its revision, or
// store.ErrRevisionConflict is returned and nothing is deleted.
func (o *OperationalSettings) deleteGuarded(ctx context.Context, section string, expectedRevision int64, reads map[string]int64) error {
	bw, ok := o.store.(store.HubSettingGuardedBatchWriter)
	if !ok {
		return errGuardedBatchUnsupported
	}
	if err := o.checkWriteGuard(ctx, section); err != nil {
		return err
	}
	rows, err := bw.UpsertHubSettingsGuarded(ctx,
		[]store.HubSettingWrite{{Section: section, ExpectedRevision: expectedRevision, Delete: true}}, reads, "", "")
	if err != nil {
		return err
	}
	o.applyDelete(ctx, section, rows[0].SettingsRevision)
	return nil
}

// applyDelete evicts a deleted section from the cache, publishes the event
// so peers refresh and self-applies. deleteRevision is the
// operational-settings revision of the delete (0 when unknown).
func (o *OperationalSettings) applyDelete(ctx context.Context, section string, deleteRevision int64) {
	o.mu.Lock()
	// As in Update: a delete at or above the revision the cache knows the
	// section at is applied to the cache directly, with a tombstone so a
	// later-arriving older write of the section cannot resurrect it;
	// below it, the cache already holds newer state of the section and the
	// store is read again before the self-apply. Revision 0 (a store or
	// section without a revision) never skips the cache delete, and also
	// re-reads, since nothing orders it against the cache
	// (ptone/scion#4457, ptone/scion#5762).
	direct := deleteRevision > 0 && deleteRevision >= o.knownAtLocked(section)
	reread := !direct
	if deleteRevision == 0 || direct {
		delete(o.cache, section)
		if direct {
			o.tombstones[section] = deleteRevision
		}
		if deleteRevision > o.revision {
			o.revision = deleteRevision
		}
		o.cacheSeq++
	}
	o.mu.Unlock()
	if reread {
		o.refreshThrough(ctx, deleteRevision, section)
	}

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
}

// refreshThroughAttempts bounds refreshThrough's reads.
const refreshThroughAttempts = 3

// refreshThrough re-reads the store until a read at or above writeRevision
// is taken into the cache, so the cache holds the write that got
// writeRevision before the writer's self-apply. Update and DeleteSection
// call it, outside o.mu, when the cache already holds newer state of the
// written section than their write (knownAtLocked).
// Any accepted read qualifies: it is at or above the cached revision,
// which was above writeRevision, and a read at revision N holds every
// operational write up to N. A read is discarded only when another write
// raised the cached revision meanwhile, so the next read is newer; after
// refreshThroughAttempts reads (or on a read error) it logs and returns
// false, and the writer's own settings event refreshes again. It returns
// true when a read at or above writeRevision was taken into the cache.
func (o *OperationalSettings) refreshThrough(ctx context.Context, writeRevision int64, section string) bool {
	for attempt := 1; attempt <= refreshThroughAttempts; attempt++ {
		_, read, accepted, err := o.refresh(ctx)
		if err != nil {
			slog.Warn("operational settings: re-read after a write failed; the settings event will refresh, and a save report lists the write's keys as skipped",
				"section", section, "write_revision", writeRevision, "error", err)
			return false
		}
		if accepted && read >= writeRevision {
			return true
		}
	}
	slog.Warn("operational settings: re-read after a write kept losing to newer writes; the settings event will refresh, and a save report lists the write's keys as skipped",
		"section", section, "write_revision", writeRevision, "attempts", refreshThroughAttempts)
	return false
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

	// --- Built-in profile reference counts (ptone/scion#5113) ---
	o.propagationWg.Add(1)
	go func() {
		defer o.propagationWg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Built-in profile reference worker panicked — reference counts stopped on this replica", "panic", r)
			}
		}()
		server.runProfileReferenceWorker(propCtx)
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
// backstop for missed NOTIFY events (design §3.6). It also runs on SQLite,
// where it is a cheap re-read of the local DB.
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
			// Recount built-in profile references every tick, changed
			// sections or not (ptone/scion#5113).
			server.triggerProfileReferences()
		}
	}
}

// refreshAndApply performs a Refresh and applies any changed sections to the
// server. Idempotent: applying the same snapshot twice is harmless because
// ApplySnapshot writes the same values and ApplyMaintenanceFromSnapshot
// is idempotent by design.
func (o *OperationalSettings) refreshAndApply(ctx context.Context, server *Server) {
	o.mu.RLock()
	generation := o.generation
	o.mu.RUnlock()
	changed, err := o.Refresh(ctx)
	if err != nil {
		slog.Error("Settings propagation refresh failed", "error", err)
		return
	}
	o.mu.RLock()
	reset := o.generation != generation
	o.mu.RUnlock()
	if len(changed) > 0 || reset {
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
	if k.Exists("server.hub.soft_delete_retain_files") {
		v := k.Bool("server.hub.soft_delete_retain_files")
		snap.SoftDeleteRetainFiles = &v
	}
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

	// Hub observability export switches
	if k.Exists(config.HubMetricsEnabledKey) {
		v := k.Bool(config.HubMetricsEnabledKey)
		snap.HubMetricsEnabled = &v
	}
	if k.Exists(config.HubTracingEnabledKey) {
		v := k.Bool(config.HubTracingEnabledKey)
		snap.HubTracingEnabled = &v
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
		// A decode failure is logged rather than dropped silently: it
		// leaves the telemetry config unset, which GET and the running
		// hub both show (ptone/scion#4108).
		data, err := json.Marshal(teleSub.Raw())
		if err != nil {
			slog.Error("operational settings: cannot encode telemetry settings", "error", err)
		} else {
			var tc config.V1TelemetryConfig
			if err := json.Unmarshal(data, &tc); err != nil {
				slog.Error("operational settings: cannot decode telemetry settings; telemetry config left unset", "error", err)
			} else {
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
	snap.MonitoringDashboardURL = k.String(config.MonitoringDashboardURLKey)

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
		if err != nil {
			slog.Error("operational settings: cannot encode notification channels", "error", err)
		} else {
			var channels []config.V1NotificationChannelConfig
			if err := json.Unmarshal(data, &channels); err != nil {
				slog.Error("operational settings: cannot decode notification channels; no channels applied", "error", err)
			} else {
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
		// Applied live in file mode too, so a settings.yaml edit followed
		// by a reload changes the Health page link without a restart.
		MonitoringDashboardURL: gc.Hub.MonitoringDashboardURL,
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

	// Hub observability export switches — read at startup only.
	snap.HubMetricsEnabled = gc.Hub.Observability.MetricsEnabled
	snap.HubTracingEnabled = gc.Hub.Observability.TracingEnabled

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

// settingsApplyState is the apply lock and the operational-settings
// revision last applied (Server.settingsApply).
//
// Lock order: settings-file lock (a workstation save applies while holding
// it), then mu, then s.mu and the other locks ApplySnapshot takes inside
// (the notification registry, OperationalSettings.mu). mu is never taken
// while holding s.mu, OperationalSettings.mu or a DB transaction: callers
// build the snapshot first, then apply. Nothing under mu takes the
// settings-file lock.
type settingsApplyState struct {
	mu sync.Mutex
	// lastRevision is the highest Layer1Snapshot.Revision applied.
	// Guarded by mu.
	lastRevision int64
	// lastSeq is the Layer1Snapshot.Seq of the last DB-backed snapshot
	// applied at lastRevision (ptone/scion#5762). Guarded by mu.
	lastSeq uint64
	// lastGeneration is the Layer1Snapshot.Generation of the last
	// DB-backed snapshot applied (ptone/scion#5667). Guarded by mu.
	lastGeneration uint64
}

// staleLocked reports whether snap is older than the last snapshot
// applied and must be skipped. Only DB-backed snapshots are ordered. A
// DB-backed snapshot with Revision 0 was built before the store had a
// revision (startup before the first operational write, or a read that
// predates it); once a revision has been applied it is older, so it is
// skipped as well. A file snapshot (FromOperationalSettings false) has no
// revision and is always applied: it occurs only on a hub without
// OperationalSettings, where no DB-backed snapshot is ever applied and
// lastRevision stays 0. A snapshot of a later generation (taken after
// the store was reset, ptone/scion#5667) is newer whatever its revision,
// and one of an earlier generation is older. At an equal revision the
// cache sequence decides
// (Layer1Snapshot.Seq): a snapshot built before a write that was cached
// below the high-water mark, which leaves the revision unchanged, is older
// than the writer's self-apply and is skipped (ptone/scion#5762). An equal
// revision and sequence is applied again, so the double apply of a write
// (self-apply and the propagation event) stays harmless. A snapshot not
// built by OperationalSettings.Snapshot carries Seq 0 and is ordered by
// revision alone, as before. Caller holds mu.
func (a *settingsApplyState) staleLocked(snap Layer1Snapshot) bool {
	if !snap.FromOperationalSettings {
		return false
	}
	if snap.Generation != a.lastGeneration {
		return snap.Generation < a.lastGeneration
	}
	if snap.Revision != a.lastRevision {
		return snap.Revision < a.lastRevision
	}
	return snap.Seq != 0 && snap.Seq < a.lastSeq
}

// recordLocked records snap as applied when it is DB-backed and newer
// than the last snapshot applied. Caller holds mu and has checked
// staleLocked.
func (a *settingsApplyState) recordLocked(snap Layer1Snapshot) {
	if !snap.FromOperationalSettings {
		return
	}
	if snap.Generation > a.lastGeneration {
		a.lastGeneration = snap.Generation
		a.lastRevision = snap.Revision
		a.lastSeq = snap.Seq
	} else if snap.Revision > a.lastRevision {
		a.lastRevision = snap.Revision
		a.lastSeq = snap.Seq
	} else if snap.Revision == a.lastRevision && snap.Seq > a.lastSeq {
		a.lastSeq = snap.Seq
	}
}

// ApplySnapshot writes the Layer1Snapshot values into the Server's config
// and MaintenanceState. This is the refactored body of the old reloadSettings()
// logic — no consumer sites change; request-path code keeps reading s.config.*
// under s.mu exactly as today.
//
// Applies are ordered by the snapshot revision (ptone/scion#4457): the
// whole apply runs under the apply lock (Server.settingsApply), and a
// DB-backed snapshot older than the last one applied is skipped, with
// nothing applied. Callers that build a snapshot before any lock (the
// self-apply after a save, the propagation event, the poll) are covered:
// whichever of two concurrent applies runs second, the newer values stay.
//
// It returns the list of applied field names and the list of fields that
// require a restart (for parity with the old reloadSettings return value).
func ApplySnapshot(s *Server, snap Layer1Snapshot) map[string]interface{} {
	s.settingsApply.mu.Lock()
	defer s.settingsApply.mu.Unlock()
	if s.settingsApply.staleLocked(snap) {
		slog.Info("Settings apply skipped: snapshot older than the last applied",
			"revision", snap.Revision, "last_applied_revision", s.settingsApply.lastRevision)
		return map[string]interface{}{
			"applied":          []string{},
			"requires_restart": []string{},
			"skipped_stale":    true,
		}
	}
	s.settingsApply.recordLocked(snap)

	applied := []string{}

	// GCP service-account permission check: decided before s.mu is taken
	// (the decision reads the store), applied below under s.mu.
	iamT, iamOK := s.decideGCPIAMReload(snap)

	s.mu.Lock()

	// Telemetry. In a DB-backed snapshot an unset value (cleared, or the
	// section removed) returns to the startup value rather than keeping
	// the last applied one (ptone/scion#3904); in a file snapshot it keeps
	// the running value.
	telemetryDefault := copyBoolPtr(snap.TelemetryEnabled)
	if telemetryDefault == nil {
		telemetryDefault = copyBoolPtr(s.config.TelemetryDefault)
		if snap.FromOperationalSettings {
			telemetryDefault = copyBoolPtr(s.startupLayer1.TelemetryDefault)
		}
	}
	if !boolPtrEqual(s.config.TelemetryDefault, telemetryDefault) {
		applied = append(applied, "telemetry_default")
	}
	s.config.TelemetryDefault = telemetryDefault
	telemetryConfig := s.config.TelemetryConfig
	if snap.TelemetryConfig != nil {
		telemetryConfig = config.ConvertV1TelemetryToAPI(snap.TelemetryConfig)
	} else if snap.FromOperationalSettings {
		telemetryConfig = s.startupLayer1.TelemetryConfig
	}
	if !reflect.DeepEqual(s.config.TelemetryConfig, telemetryConfig) {
		applied = append(applied, "telemetry_config")
	}
	s.config.TelemetryConfig = telemetryConfig

	// Auto-expose ports. In a DB-backed snapshot an unset value (cleared,
	// or the section removed) returns to the startup value
	// (ptone/scion#3904); in a file snapshot it keeps the running value.
	autoExpose := copyBoolPtr(snap.AutoExposePortsEnabled)
	if autoExpose == nil {
		autoExpose = copyBoolPtr(s.config.AutoExposePortsDefault)
		if snap.FromOperationalSettings {
			autoExpose = copyBoolPtr(s.startupLayer1.AutoExposePortsDefault)
		}
	}
	if !boolPtrEqual(s.config.AutoExposePortsDefault, autoExpose) {
		applied = append(applied, "auto_expose_ports_default")
	}
	s.config.AutoExposePortsDefault = autoExpose

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

	// GitHub App non-sensitive config. In a DB-backed snapshot without an
	// app_id the startup values are used, so clearing the section reverts
	// to them rather than keeping the last applied app (ptone/scion#3904);
	// an unset private_key_path then keeps the startup path. A file
	// snapshot without an app_id keeps the running values. The in-memory
	// private key and webhook secret are kept as-is (loaded from the
	// secrets backend).
	gh := githubAppPublicFrom(s.config.GitHubAppConfig)
	if snap.FromOperationalSettings {
		gh = s.startupLayer1.GitHubApp
	}
	if snap.GitHubAppID != 0 {
		gh.AppID = snap.GitHubAppID
		gh.APIBaseURL = snap.GitHubAPIBaseURL
		gh.WebhooksEnabled = snap.GitHubWebhooksEnabled
		gh.InstallationURL = snap.GitHubInstallationURL
		if snap.GitHubPrivateKeyPath != "" {
			gh.PrivateKeyPath = snap.GitHubPrivateKeyPath
		}
	}
	if s.setGitHubAppPublicLocked(gh) {
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

	// Monitoring dashboard link: written unconditionally so that clearing
	// the key removes the link. A value that fails validation (one that
	// did not come through the admin API) is not applied.
	monitoringURL := snap.MonitoringDashboardURL
	if monitoringURL != "" && config.ValidateMonitoringDashboardURL(monitoringURL) != nil {
		slog.Warn("ignoring invalid server.hub.monitoring_dashboard_url; the Health page shows no monitoring link")
		monitoringURL = ""
	}
	if s.config.MonitoringDashboardURL != monitoringURL {
		s.config.MonitoringDashboardURL = monitoringURL
		applied = append(applied, "monitoring_dashboard_url")
	}

	// image_registry is not applied here: every hub read resolves it from
	// the operational settings when it needs it (hubScopeImageRegistry,
	// dispatchImageRegistryFor; ptone/scion#3916), and the co-located
	// broker's overlay is fed below.

	// Soft-delete retention and retain-files. Consumers read them through
	// softDeleteSettings, so a change applies to the next delete and the
	// next purge. In a DB-backed snapshot an unset value returns to the
	// startup value; in a file snapshot it keeps the running value. An
	// invalid retention keeps the running one (ptone/scion#3904).
	retention := s.config.SoftDeleteRetention
	if snap.FromOperationalSettings {
		retention = s.startupLayer1.SoftDeleteRetention
	}
	if snap.SoftDeleteRetention != "" {
		if d, err := time.ParseDuration(snap.SoftDeleteRetention); err != nil || d < 0 {
			slog.Warn("invalid soft_delete_retention duration, keeping current value", "value", snap.SoftDeleteRetention, "error", err)
			retention = s.config.SoftDeleteRetention
		} else {
			retention = d
		}
	}
	if s.config.SoftDeleteRetention != retention {
		s.config.SoftDeleteRetention = retention
		applied = append(applied, "soft_delete_retention")
	}
	retainFiles := s.config.SoftDeleteRetainFiles
	if snap.FromOperationalSettings {
		retainFiles = s.startupLayer1.SoftDeleteRetainFiles
	}
	if snap.SoftDeleteRetainFiles != nil {
		retainFiles = *snap.SoftDeleteRetainFiles
	}
	if s.config.SoftDeleteRetainFiles != retainFiles {
		s.config.SoftDeleteRetainFiles = retainFiles
		applied = append(applied, "soft_delete_retain_files")
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

	// Notification channels: the new registry is built outside s.mu and
	// then swapped in whole. A registry is never changed after it is
	// built, so a dispatch already holding the old one finishes on it
	// (ptone/scion#3904).
	if snap.FromOperationalSettings && s.applyNotificationChannels(snap.NotificationChannels) {
		applied = append(applied, "notification_channels")
	}

	// Propagate hub_name to the GCP secret backend so new secrets get the
	// correct label value.
	if gcpBackend, ok := s.secretBackend.(*secret.GCPBackend); ok {
		gcpBackend.SetHubName(hubName)
	}
	// Log handlers are built before the stored settings load; label their
	// records with the applied name, at startup and on every later rename,
	// so the hub's own log queries (which filter on this name) match them
	// (ptone/scion#4903). The value is process-wide: tests that apply a
	// snapshot set it too. Metrics resource attributes cannot follow a
	// rename; see newHubTelemetryIdentity in cmd.
	logging.SetHubName(hubName)

	// Runtimes, profiles, and harness configs: update the global settings
	// overlay so that the co-located broker (which reads via
	// config.LoadEffectiveSettings on every dispatch) sees DB-backed values.
	// The overlay is installed at hub startup in co-located mode (see
	// cmd/server_foreground.go). For standalone brokers the overlay is nil
	// and they continue reading from disk only.
	// The image_registry layers are fed whether or not any catalog section
	// is present, so a managed endpoints value reaches the co-located
	// broker with no stored catalog (ptone/scion#3916, ptone/scion#5007).
	if snap.FromOperationalSettings {
		feedImageRegistryOverlay(s.GetOperationalSettings())
	}
	if snap.Runtimes != nil || snap.Profiles != nil || snap.HarnessConfigs != nil {
		if overlay := config.GetGlobalSettingsOverlay(); overlay != nil {
			overlay.Update(snap.Runtimes, snap.Profiles, snap.HarnessConfigs)
			// Recount the built-in profile references the overlay retains
			// (ptone/scion#5113); never blocks.
			s.triggerProfileReferences()
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
	if s.testAdminTierOn() && federationConfigured(snap.FederationConfig) {
		// G8: while the test admin tier is on no federation trust is ever
		// installed, whatever the stored settings say. Startup refuses such
		// a snapshot (cmd); this keeps a later reload from installing one.
		s.federationAuth.Store(nil)
		s.setFederationSkipReason(federationSkipTestAdminTier)
		logTestAdminFederationSkipped()
	} else if snap.FederationConfig == nil || !snap.FederationConfig.Enabled {
		s.federationAuth.Store(nil)
		s.setFederationSkipReason("")
		if snap.FederationConfig != nil {
			slog.Info("Federation disabled via config, authenticator cleared")
		}
		applied = append(applied, "federation")
	} else {
		if errs := snap.FederationConfig.Validate(); len(errs) > 0 {
			slog.Error("Federation config validation failed during apply, keeping old config",
				"errors", fmt.Sprintf("%v", errs))
			s.setFederationSkipReason(federationSkipInvalid)
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
				s.setFederationSkipReason(federationSkipBuildFailed)
			} else {
				s.federationAuth.Store(newAuth)
				s.setFederationSkipReason("")
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
	// Ordered like ApplySnapshot, under the same apply lock: a snapshot
	// older than the last one applied leaves the maintenance state alone
	// (ptone/scion#4457).
	s.settingsApply.mu.Lock()
	defer s.settingsApply.mu.Unlock()
	if s.settingsApply.staleLocked(snap) {
		return
	}
	s.settingsApply.recordLocked(snap)

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

// AgentDelegationPolicy returns the narrowing of the hub agent-delegation
// policy (the "agent_delegation_policy" section). An absent section narrows
// nothing. A stored document that is unreadable or that the code cannot
// fully parse returns an error: callers deny rather than treat it as "no
// narrowing" (.design/agent-delegation.md §11.2 step 9).
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) AgentDelegationPolicy() (opsettings.AgentDelegationPolicySettings, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache[opsettings.AgentDelegationPolicySectionName]
	if !ok {
		return opsettings.AgentDelegationPolicySettings{}, nil
	}
	if state.Malformed {
		return opsettings.AgentDelegationPolicySettings{}, fmt.Errorf("agent_delegation_policy settings: unreadable document")
	}
	return opsettings.ParseAgentDelegationPolicyDoc(state.Value)
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

// applySnapshotLogLevel applies server.log_level to the shared level state
// at setting precedence (logging.SetLogLevelSetting), so the installed level
// filter and the handlers built with logging.ResolveLogLeveler follow it.
// SCION_LOG_LEVEL and the --debug flag still win. An empty level reverts the
// setting to the built-in default (info).
//
// It deliberately does not call slog.SetLogLoggerLevel: that changes only the
// level of the standard-library log bridge, and also re-levels log.Printf
// lines so that they bypass the level filter.
//
// This is separated from applySnapshot because log level is a Layer-0 setting
// (per design §3.1) and is only changed in file mode via reloadSettings or a
// workstation server-config save.
func applySnapshotLogLevel(level string) {
	logging.ApplyLogLevelSetting("server.log_level", level)
}

// DefaultAttachmentDeliveryMaxBytes is the compiled default of
// messaging.attachment_delivery_max_bytes (ptone/scion#4596): 32 MiB.
const DefaultAttachmentDeliveryMaxBytes int64 = 32 << 20

// DefaultAttachmentInboxRetentionDays is the compiled default of
// messaging.attachment_inbox_retention_days (ptone/scion#4596).
const DefaultAttachmentInboxRetentionDays = 7

// messagingSection returns the cached messaging section, and false when it
// is absent or unreadable.
func (o *OperationalSettings) messagingSection() (opsettings.MessagingSettings, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var ms opsettings.MessagingSettings
	state, ok := o.cache["messaging"]
	if !ok || state.Malformed {
		return ms, false
	}
	if err := json.Unmarshal(state.Value, &ms); err != nil {
		return opsettings.MessagingSettings{}, false
	}
	return ms, true
}

// AttachmentDeliveryMaxBytes returns the per-message total size limit of
// attachments for a message with an agent recipient (ptone/scion#4596).
// 0 means delivery is off. The compiled default applies when the section
// or field is absent or unreadable, or the value is negative.
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) AttachmentDeliveryMaxBytes() int64 {
	ms, ok := o.messagingSection()
	if !ok || ms.AttachmentDeliveryMaxBytes == nil || *ms.AttachmentDeliveryMaxBytes < 0 {
		return DefaultAttachmentDeliveryMaxBytes
	}
	return *ms.AttachmentDeliveryMaxBytes
}

// AttachmentInboxRetentionDays returns how many days a broker keeps
// delivered attachments in an agent's inbox (ptone/scion#4596). The
// compiled default applies when the section or field is absent or
// unreadable, or the value is below 1.
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) AttachmentInboxRetentionDays() int {
	ms, ok := o.messagingSection()
	if !ok || ms.AttachmentInboxRetentionDays == nil || *ms.AttachmentInboxRetentionDays < 1 {
		return DefaultAttachmentInboxRetentionDays
	}
	return *ms.AttachmentInboxRetentionDays
}

// ChatRetention returns the resolved chat retention settings (the
// "chat_retention" section). It returns opsettings.DefaultChatRetentionConfig
// (retention off, no window) when the section is absent, and fails closed
// to the same disabled config when the stored document is unreadable or
// holds an invalid value, so a bad document never purges anything.
//
// Hot-reloadable: reads from the DB-backed cache.
func (o *OperationalSettings) ChatRetention() opsettings.ChatRetentionConfig {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state, ok := o.cache[opsettings.ChatRetentionSectionName]
	if !ok || state.Malformed {
		return opsettings.DefaultChatRetentionConfig()
	}
	cfg, err := opsettings.ParseChatRetentionDoc(state.Value)
	if err != nil {
		return opsettings.DefaultChatRetentionConfig()
	}
	return cfg
}
