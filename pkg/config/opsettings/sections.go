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

// Package opsettings defines the Layer-1 operational settings section registry
// for the two-tier settings architecture. Each section maps to a Go struct,
// a JSON-schema fragment, and a set of koanf key paths — providing the single
// source of truth for Layer-0 vs Layer-1 classification.
package opsettings

import (
	"encoding/json"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// AccessSettings holds Layer-1 access control settings.
type AccessSettings struct {
	AdminEmails       []string `json:"admin_emails,omitempty"`
	UserAccessMode    string   `json:"user_access_mode,omitempty"`
	DefaultUserRole   string   `json:"default_user_role,omitempty"`
	AuthorizedDomains []string `json:"authorized_domains,omitempty"`
}

// LifecycleSettings holds Layer-1 agent lifecycle settings.
type LifecycleSettings struct {
	AutoSuspendStalled         *bool  `json:"auto_suspend_stalled,omitempty"`
	StalledThreshold           string `json:"stalled_threshold,omitempty"`
	SoftDeleteRetention        string `json:"soft_delete_retention,omitempty"`
	SoftDeleteRetainFiles      *bool  `json:"soft_delete_retain_files,omitempty"`
	StartClaimLeaseTTL         string `json:"start_claim_lease_ttl,omitempty"`
	StartMaxDuration           string `json:"start_max_duration,omitempty"`
	StartUnconfirmedHold       string `json:"start_unconfirmed_hold,omitempty"`
	StartCreateUnconfirmedHold string `json:"start_create_unconfirmed_hold,omitempty"`
}

// MaintenanceSettings holds Layer-1 maintenance/admin-mode settings.
type MaintenanceSettings struct {
	AdminMode          bool   `json:"admin_mode,omitempty"`
	MaintenanceMessage string `json:"maintenance_message,omitempty"`
}

// TelemetrySettings holds the Layer-1 telemetry configuration.
// It reuses the existing V1TelemetryConfig to preserve full fidelity.
type TelemetrySettings struct {
	config.V1TelemetryConfig
}

// AutoExposePortsSettings holds Layer-1 auto-expose ports configuration.
type AutoExposePortsSettings struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// AgentDefaultsSettings holds Layer-1 default agent configuration.
type AgentDefaultsSettings struct {
	DefaultTemplate      string            `json:"default_template,omitempty"`
	DefaultHarnessConfig string            `json:"default_harness_config,omitempty"`
	DefaultHarnessAuth   string            `json:"default_harness_auth,omitempty"`
	DefaultMaxTurns      int               `json:"default_max_turns,omitempty"`
	DefaultMaxModelCalls int               `json:"default_max_model_calls,omitempty"`
	DefaultMaxDuration   string            `json:"default_max_duration,omitempty"`
	DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty"`
	DefaultModel         string            `json:"default_model,omitempty"`
	DefaultThinkingLevel *int              `json:"default_thinking_level,omitempty"`
	DefaultMaxAgentRole  string            `json:"default_max_agent_role,omitempty"`
	DefaultAgentRole     string            `json:"default_agent_role,omitempty"`
	DefaultRuntimeBroker string            `json:"default_runtime_broker,omitempty"`
	// DefaultTimezone is the hub-level IANA timezone fallback (e.g.
	// "America/Los_Angeles") for agent containers: applied as TZ when the
	// agent has no pinned timezone and no storage-scope TZ environment
	// variable applies.
	DefaultTimezone string `json:"default_timezone,omitempty"`
	// DefaultGCPIdentityMode is the hub-wide fallback GCP metadata mode
	// ("block", "passthrough", or "assign") applied when neither the agent
	// create request nor the project's default GCP identity setting names one.
	DefaultGCPIdentityMode string `json:"default_gcp_identity_mode,omitempty"`
	// DefaultGCPIdentityServiceAccountID is the service account used when
	// DefaultGCPIdentityMode is "assign". Ignored otherwise.
	DefaultGCPIdentityServiceAccountID string `json:"default_gcp_identity_service_account_id,omitempty"`
}

// EndpointsSettings holds Layer-1 endpoint configuration.
type EndpointsSettings struct {
	PublicURL     string `json:"public_url,omitempty"`
	HubName       string `json:"hub_name,omitempty"`
	ImageRegistry string `json:"image_registry,omitempty"`
}

// GitHubAppSettings holds the Layer-1 GitHub App configuration.
// Secret material (private_key, webhook_secret) is excluded — stays in secret backend.
type GitHubAppSettings struct {
	AppID           int64  `json:"app_id,omitempty"`
	APIBaseURL      string `json:"api_base_url,omitempty"`
	WebhooksEnabled *bool  `json:"webhooks_enabled,omitempty"`
	InstallationURL string `json:"installation_url,omitempty"`
	PrivateKeyPath  string `json:"private_key_path,omitempty"`
}

// NotificationsSettings holds the Layer-1 notification channel configuration.
type NotificationsSettings struct {
	NotificationChannels []config.V1NotificationChannelConfig `json:"notification_channels,omitempty"`
}

// FederationSettings is the opsettings section struct for federation config.
type FederationSettings struct {
	Enabled          *bool                          `json:"enabled,omitempty"`
	TrustedIssuers   []config.V1TrustedIssuerConfig `json:"trusted_issuers,omitempty"`
	Algorithms       []string                       `json:"algorithms,omitempty"`
	RefreshInterval  string                         `json:"refresh_interval,omitempty"`
	DebounceInterval string                         `json:"debounce_interval,omitempty"`
}

// ArtifactsSettings holds the Layer-1 settings of the artifact service
// (pkg/artifacts). DB-only (runtime state), no settings.yaml
// representation. Every field is optional; an omitted field takes its
// compiled default (see the ArtifactsDefault* constants). Read it through
// Resolve, never field by field, so every reader gets the same validation.
type ArtifactsSettings struct {
	// Enabled is the hub's switch for the artifact service, on top of the
	// hub.artifacts experiment. Default true, so enabling the experiment is
	// enough.
	Enabled *bool `json:"enabled,omitempty"`
	// MaxFileBytes caps the size of one file in an artifact version.
	MaxFileBytes *int64 `json:"max_file_bytes,omitempty"`
	// MaxBundleBytes caps the total size of the files in one version.
	MaxBundleBytes *int64 `json:"max_bundle_bytes,omitempty"`
	// MaxFiles caps the number of files in one version.
	MaxFiles *int `json:"max_files,omitempty"`
	// DefaultRetentionDays is how long an artifact is kept when it has no
	// retention of its own. 0 means forever.
	DefaultRetentionDays *int `json:"default_retention_days,omitempty"`
	// LinkDefaultTTLHours is the lifetime of a share link created without
	// an explicit expiry.
	LinkDefaultTTLHours *int `json:"link_default_ttl_hours,omitempty"`
	// LinkMaxTTLHours is the longest lifetime a share link may be given.
	// Share links always expire.
	LinkMaxTTLHours *int `json:"link_max_ttl_hours,omitempty"`
}

// Compiled defaults for ArtifactsSettings.
const (
	ArtifactsDefaultEnabled               = true
	ArtifactsDefaultMaxFileBytes    int64 = 32 << 20  // 32 MiB
	ArtifactsDefaultMaxBundleBytes  int64 = 256 << 20 // 256 MiB
	ArtifactsDefaultMaxFiles              = 200
	ArtifactsDefaultRetentionDays         = 0   // never expire
	ArtifactsDefaultLinkTTLHours          = 168 // 7 days
	ArtifactsDefaultLinkMaxTTLHours       = 720 // 30 days
)

// ArtifactsConfig is the resolved artifact service configuration: every
// field holds its effective value.
type ArtifactsConfig struct {
	Enabled              bool
	MaxFileBytes         int64
	MaxBundleBytes       int64
	MaxFiles             int
	DefaultRetentionDays int
	LinkDefaultTTLHours  int
	LinkMaxTTLHours      int
	// Malformed is true when the stored document could not be used. The
	// service is then disabled and the limits are the compiled defaults.
	Malformed bool
}

// DefaultArtifactsConfig returns the configuration used when the artifacts
// section is absent.
func DefaultArtifactsConfig() ArtifactsConfig {
	return ArtifactsConfig{
		Enabled:              ArtifactsDefaultEnabled,
		MaxFileBytes:         ArtifactsDefaultMaxFileBytes,
		MaxBundleBytes:       ArtifactsDefaultMaxBundleBytes,
		MaxFiles:             ArtifactsDefaultMaxFiles,
		DefaultRetentionDays: ArtifactsDefaultRetentionDays,
		LinkDefaultTTLHours:  ArtifactsDefaultLinkTTLHours,
		LinkMaxTTLHours:      ArtifactsDefaultLinkMaxTTLHours,
	}
}

// MalformedArtifactsConfig returns the fail-closed configuration: the
// service disabled, the limits at their compiled defaults.
func MalformedArtifactsConfig() ArtifactsConfig {
	c := DefaultArtifactsConfig()
	c.Enabled = false
	c.Malformed = true
	return c
}

// Resolve applies compiled defaults to omitted fields and validates the
// result. An invalid value anywhere makes the whole section unusable: it
// returns MalformedArtifactsConfig and an error naming the problem, so the
// service fails closed rather than running with a limit nobody set.
//
// Valid means: every size and count limit and both link TTLs are at least
// 1, retention is at least 0, a file limit does not exceed the bundle
// limit, and the default link TTL does not exceed the maximum.
func (a ArtifactsSettings) Resolve() (ArtifactsConfig, error) {
	c := DefaultArtifactsConfig()
	if a.Enabled != nil {
		c.Enabled = *a.Enabled
	}
	if a.MaxFileBytes != nil {
		c.MaxFileBytes = *a.MaxFileBytes
	}
	if a.MaxBundleBytes != nil {
		c.MaxBundleBytes = *a.MaxBundleBytes
	}
	if a.MaxFiles != nil {
		c.MaxFiles = *a.MaxFiles
	}
	if a.DefaultRetentionDays != nil {
		c.DefaultRetentionDays = *a.DefaultRetentionDays
	}
	if a.LinkDefaultTTLHours != nil {
		c.LinkDefaultTTLHours = *a.LinkDefaultTTLHours
	}
	if a.LinkMaxTTLHours != nil {
		c.LinkMaxTTLHours = *a.LinkMaxTTLHours
	}

	var err error
	switch {
	case c.MaxFileBytes < 1:
		err = fmt.Errorf("max_file_bytes must be at least 1, got %d", c.MaxFileBytes)
	case c.MaxBundleBytes < 1:
		err = fmt.Errorf("max_bundle_bytes must be at least 1, got %d", c.MaxBundleBytes)
	case c.MaxFiles < 1:
		err = fmt.Errorf("max_files must be at least 1, got %d", c.MaxFiles)
	case c.DefaultRetentionDays < 0:
		err = fmt.Errorf("default_retention_days must be at least 0, got %d", c.DefaultRetentionDays)
	case c.LinkDefaultTTLHours < 1:
		err = fmt.Errorf("link_default_ttl_hours must be at least 1, got %d", c.LinkDefaultTTLHours)
	case c.LinkMaxTTLHours < 1:
		err = fmt.Errorf("link_max_ttl_hours must be at least 1, got %d", c.LinkMaxTTLHours)
	case c.MaxFileBytes > c.MaxBundleBytes:
		err = fmt.Errorf("max_file_bytes (%d) exceeds max_bundle_bytes (%d)", c.MaxFileBytes, c.MaxBundleBytes)
	case c.LinkDefaultTTLHours > c.LinkMaxTTLHours:
		err = fmt.Errorf("link_default_ttl_hours (%d) exceeds link_max_ttl_hours (%d)", c.LinkDefaultTTLHours, c.LinkMaxTTLHours)
	}
	if err != nil {
		return MalformedArtifactsConfig(), fmt.Errorf("artifacts settings: %w", err)
	}
	return c, nil
}

// ParseArtifactsDoc parses and resolves a stored artifacts document. Bytes
// that are not valid JSON, do not unmarshal into ArtifactsSettings, or fail
// Resolve yield MalformedArtifactsConfig and an error.
func ParseArtifactsDoc(raw json.RawMessage) (ArtifactsConfig, error) {
	if !json.Valid(raw) {
		return MalformedArtifactsConfig(), fmt.Errorf("artifacts settings: invalid JSON")
	}
	var doc ArtifactsSettings
	if err := json.Unmarshal(raw, &doc); err != nil {
		return MalformedArtifactsConfig(), fmt.Errorf("artifacts settings: %w", err)
	}
	return doc.Resolve()
}

// ProjectDefaultsSettings holds Layer-1 project creation defaults.
type ProjectDefaultsSettings struct {
	// DefaultScratchpad controls whether new projects automatically get a
	// "scratchpad" shared directory. When nil (section absent from DB),
	// the compiled default is true (ON).
	DefaultScratchpad *bool `json:"default_scratchpad,omitempty"`
}

// RuntimesSettings holds the Layer-1 runtimes map (map of named runtime configs).
// The entire map is stored as a single JSONB document in hub_settings.
type RuntimesSettings = map[string]config.V1RuntimeConfig

// ProfilesSettings holds the Layer-1 profiles map (map of named profile configs).
// The entire map is stored as a single JSONB document in hub_settings.
type ProfilesSettings = map[string]config.V1ProfileConfig

// HarnessConfigsSettings holds the Layer-1 harness_configs map.
// The entire map is stored as a single JSONB document in hub_settings.
type HarnessConfigsSettings = map[string]config.HarnessConfigEntry

// QuotaSettings holds Layer-1 quota enforcement settings.
type QuotaSettings struct {
	// EnforceBrokerQuotas controls whether max_agents_per_broker is enforced
	// on create. Default true (fail-safe) when absent. When false, usage is
	// still counted (reservations, release, reconcile, backfill all run) —
	// only the reject is skipped (design P1-D5).
	EnforceBrokerQuotas *bool `json:"enforce_broker_quotas,omitempty" koanf:"enforce_broker_quotas"`
}

// AgentSecretsSettings holds Layer-1 hub policy for secrets written by
// agents. UserScopeOnly is nil when unset, meaning agents may write project
// scope as they do today (default false/permissive).
type AgentSecretsSettings struct {
	// UserScopeOnly, when true, restricts agents to writing user (profile)
	// scope secrets only. The hub rejects agent writes at project scope,
	// including harness auth capture. User-originated writes are unaffected
	// (design ptone/scion#2291 §5).
	UserScopeOnly *bool `json:"user_scope_only,omitempty" koanf:"user_scope_only"`
}

// MessagingSettings holds Layer-1 messaging configuration.
// DB-only (runtime state), no settings.yaml representation.
//
// ConversationEnvelopeSwitch is the consolidated switch that replaces the
// former conversation_read_switch and conversation_write_deny_switch.
// It defaults ON when absent or omitted, and OFF when the document is
// malformed (Phase 9a §4.6).
//
// The two stale fields are retained for deserialization of existing rows
// (Go's json.Unmarshal ignores unknown fields, but keeping them lets us
// read old documents cleanly). They are never written by new code and
// self-clean on first PUT via the admin endpoint.
type MessagingSettings struct {
	ConversationEnvelopeSwitch *bool `json:"conversation_envelope_switch,omitempty"`

	// CrossProjectMessagingEnabled controls whether agents may communicate
	// across project boundaries on this Hub. Default false (off).
	// This is a security-critical flag requiring revision/ETag concurrency.
	CrossProjectMessagingEnabled *bool `json:"cross_project_messaging_enabled,omitempty"`

	// OffloadThresholdRunes is the rune-count threshold above which an
	// agent-recipient DM body is replaced by a fetch stub at dispatch
	// (ptone/scion#2257, design auto-offload-large-dm §5, §8.1). Compiled
	// default 0 (disabled); negative values are treated as 0. Nothing in
	// Phase 1/2 wires `offload_fetch_by_id` — that setting is added in
	// Phase 3.
	OffloadThresholdRunes *int `json:"offload_threshold_runes,omitempty"`

	// Stale fields — kept for backward-compatible deserialization only.
	// New code must not read or write these.
	ConversationReadSwitch      *bool `json:"conversation_read_switch,omitempty"`
	ConversationWriteDenySwitch *bool `json:"conversation_write_deny_switch,omitempty"`
}

// ExperimentsSettings stores only explicit admin overrides for the
// pkg/experiments registry. An absent key means "use the registry default".
// Absent row = no overrides.
//
// DB-only (runtime state), no settings.yaml representation: experiment names
// contain dots, and koanf uses "." as its key delimiter, so a koanf-backed
// map keyed by experiment name would split "web.terminal_workspace" into
// nested keys (ptone/scion#2217).
type ExperimentsSettings struct {
	Overrides map[string]bool `json:"overrides,omitempty"`
}

// ParseExperimentsDoc applies exactly the Refresh/Update malformed predicate
// (operational_settings.go): malformed = the raw bytes are not valid JSON, or
// they do not unmarshal into ExperimentsSettings. Schema validity is
// deliberately NOT part of this predicate — a parseable but schema-invalid
// document (e.g. an extra top-level key) is not "malformed" in this sense,
// even though Validate rejects it on write. Refresh and Update keep their
// generic check through sec.New(), which is the same predicate for this
// struct; ReadAuthoritativeExperiments calls this function directly.
func ParseExperimentsDoc(raw json.RawMessage) (doc ExperimentsSettings, malformed bool) {
	if !json.Valid(raw) {
		return ExperimentsSettings{}, true
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ExperimentsSettings{}, true
	}
	return doc, false
}
