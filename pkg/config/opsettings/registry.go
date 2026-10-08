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

package opsettings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Section describes a single Layer-1 operational settings section.
type Section struct {
	Name       string
	Schema     *jsonschema.Schema
	KoanfPaths []string
	New        func() any
}

// dns1123SubdomainOrEmptyPattern mirrors the pattern used for
// priority_class_name / priorityClassName in settings-v1.schema.json and
// agent-v1.schema.json: a DNS-1123 subdomain (the Kubernetes PriorityClass
// name format), or the empty string, which means unset to the runtime and
// would otherwise fail the strict subdomain pattern.
const dns1123SubdomainOrEmptyPattern = `^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`

// dns1123LabelOrEmptyPattern mirrors the runtime namespace pattern in
// settings-v1.schema.json: a DNS-1123 label (the Kubernetes namespace name
// format, at most 63 characters), or the empty string for the runtime's
// default namespace.
const dns1123LabelOrEmptyPattern = `^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`

// kubernetesServiceAccountMappingsSchema mirrors
// kubernetes_service_account_mappings in settings-v1.schema.json: lowercase
// GCP service account email keys, Kubernetes ServiceAccount name values
// (DNS-1123 subdomain, at most 253 characters).
func kubernetesServiceAccountMappingsSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"propertyNames": map[string]interface{}{
			"pattern": `^[a-z0-9]([a-z0-9-]*[a-z0-9])?@([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+gserviceaccount\.com$`,
		},
		"additionalProperties": map[string]interface{}{
			"type":      "string",
			"pattern":   `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`,
			"maxLength": 253,
		},
	}
}

// sharedDirStorageBackendsSchema mirrors shared_dir_storage_backends in
// settings-v1.schema.json: shared dir name keys (lowercase letters, digits
// and hyphens, as api.ValidateSharedDirs requires) mapped to local or nfs.
func sharedDirStorageBackendsSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"propertyNames": map[string]interface{}{
			"pattern": config.SharedDirNamePattern,
		},
		"additionalProperties": map[string]interface{}{"type": "string", "enum": []string{"local", "nfs"}},
	}
}

// Registry is the single source of truth for Layer-0 vs Layer-1 classification.
// Every Layer-1 section is listed here; any koanf key not owned by a section is
// Layer-0 (bootstrap) and must not be written via the admin API.
var Registry []Section

// sectionIndex maps section name → *Section for O(1) lookup.
var sectionIndex map[string]*Section

// keyIndex maps koanf path → section name for ownership lookup.
var keyIndex map[string]string

func init() {
	Registry = []Section{
		{
			Name:       "access",
			KoanfPaths: []string{"server.hub.admin_emails", "server.auth.user_access_mode", "server.auth.default_user_role", "server.auth.authorized_domains"},
			New:        func() any { return &AccessSettings{} },
		},
		{
			Name:       "lifecycle",
			KoanfPaths: []string{"server.hub.auto_suspend_stalled", "server.hub.stalled_threshold", "server.hub.soft_delete_retention", "server.hub.soft_delete_retain_files", "server.hub.start_claim_lease_ttl", "server.hub.start_max_duration", "server.hub.start_unconfirmed_hold", "server.hub.start_create_unconfirmed_hold"},
			New:        func() any { return &LifecycleSettings{} },
		},
		{
			// maintenance is durable via DB but has no settings.yaml representation.
			// It is runtime/API-owned state: absent DB row = compiled defaults
			// (admin_mode=false). Seeding skips this section.
			Name:       "maintenance",
			KoanfPaths: nil,
			New:        func() any { return &MaintenanceSettings{} },
		},
		{
			// messaging is durable via DB but has no settings.yaml representation.
			// It is runtime/API-owned state: absent DB row = compiled defaults
			// (both switches OFF). Seeding skips this section (KoanfPaths nil).
			Name:       "messaging",
			KoanfPaths: nil,
			New:        func() any { return &MessagingSettings{} },
		},
		{
			Name: "telemetry",
			KoanfPaths: []string{
				"telemetry.enabled",
				"telemetry.cloud", "telemetry.cloud.enabled", "telemetry.cloud.endpoint",
				"telemetry.cloud.protocol", "telemetry.cloud.headers", "telemetry.cloud.provider",
				"telemetry.cloud.tls", "telemetry.cloud.tls.enabled", "telemetry.cloud.tls.insecure_skip_verify", "telemetry.cloud.tls.ca_file",
				"telemetry.cloud.batch", "telemetry.cloud.batch.max_size", "telemetry.cloud.batch.timeout",
				"telemetry.hub", "telemetry.hub.enabled", "telemetry.hub.report_interval",
				"telemetry.local", "telemetry.local.enabled", "telemetry.local.file", "telemetry.local.console",
				"telemetry.filter", "telemetry.filter.enabled", "telemetry.filter.respect_debug_mode",
				"telemetry.filter.events", "telemetry.filter.events.include", "telemetry.filter.events.exclude",
				"telemetry.filter.attributes", "telemetry.filter.attributes.redact", "telemetry.filter.attributes.hash",
				"telemetry.filter.sampling", "telemetry.filter.sampling.default", "telemetry.filter.sampling.rates",
				"telemetry.resource",
			},
			New: func() any { return &TelemetrySettings{} },
		},
		{
			Name: "auto_expose_ports",
			KoanfPaths: []string{
				"auto_expose_ports.enabled",
			},
			New: func() any { return &AutoExposePortsSettings{} },
		},
		{
			Name: "quotas",
			KoanfPaths: []string{
				"quotas.enforce_broker_quotas",
			},
			New: func() any { return &QuotaSettings{} },
		},
		{
			Name: "agent_secrets",
			KoanfPaths: []string{
				"agent_secrets.user_scope_only",
			},
			New: func() any { return &AgentSecretsSettings{} },
		},
		{
			Name: "agent_defaults",
			KoanfPaths: []string{
				"default_template", "default_harness_config",
				"default_max_turns", "default_max_model_calls",
				"default_max_duration", "default_resources",
				"default_model", "default_thinking_level",
				"default_max_agent_role", "default_agent_role",
				"default_runtime_broker",
				"default_timezone",
				"default_gcp_identity_mode",
				"default_gcp_identity_service_account_id",
			},
			New: func() any { return &AgentDefaultsSettings{} },
		},
		{
			Name:       "endpoints",
			KoanfPaths: []string{"server.hub.public_url", "server.hub.hub_name", "image_registry"},
			New:        func() any { return &EndpointsSettings{} },
		},
		{
			Name: "github_app",
			KoanfPaths: []string{
				"server.github_app", "server.github_app.app_id", "server.github_app.api_base_url",
				"server.github_app.webhooks_enabled", "server.github_app.installation_url",
				"server.github_app.private_key_path",
			},
			New: func() any { return &GitHubAppSettings{} },
		},
		{
			Name:       "notifications",
			KoanfPaths: []string{"server.notification_channels"},
			New:        func() any { return &NotificationsSettings{} },
		},
		{
			// project_defaults controls hub-level defaults applied at
			// project creation time (e.g. default scratchpad shared dir).
			// Absent DB row = compiled defaults (default_scratchpad=true).
			Name: "project_defaults",
			KoanfPaths: []string{
				"project_defaults.default_scratchpad",
			},
			New: func() any { return &ProjectDefaultsSettings{} },
		},
		{
			Name: "federation",
			KoanfPaths: []string{
				"server.federation.enabled",
				"server.federation.trusted_issuers",
				"server.federation.algorithms",
				"server.federation.refresh_interval",
				"server.federation.debounce_interval",
			},
			New: func() any { return &FederationSettings{} },
		},
		{
			// runtimes is a map-of-objects section: one hub_settings row stores
			// the entire map as JSONB. Prefix-only KoanfPath ("runtimes") means
			// OwningSection walks up from any nested key (e.g. "runtimes.docker.type")
			// and hits this entry.
			Name:       "runtimes",
			KoanfPaths: []string{"runtimes"},
			New:        func() any { m := make(RuntimesSettings); return &m },
		},
		{
			// profiles is a map-of-objects section, same pattern as runtimes.
			Name:       "profiles",
			KoanfPaths: []string{"profiles"},
			New:        func() any { m := make(ProfilesSettings); return &m },
		},
		{
			// harness_configs is a map-of-objects section, same pattern as runtimes.
			Name:       "harness_configs",
			KoanfPaths: []string{"harness_configs"},
			New:        func() any { m := make(HarnessConfigsSettings); return &m },
		},
		{
			// experiments is durable via DB but has no settings.yaml
			// representation. It is runtime/API-owned state: absent DB
			// row = compiled registry defaults for every experiment.
			// Seeding skips this section (KoanfPaths nil) -- experiment
			// names contain dots, which koanf would otherwise split on.
			Name:       "experiments",
			KoanfPaths: nil,
			New:        func() any { return &ExperimentsSettings{} },
		},
		{
			// artifacts is durable via DB but has no settings.yaml
			// representation. Absent DB row = compiled defaults (see
			// ArtifactsSettings). Seeding skips this section (KoanfPaths nil).
			Name:       "artifacts",
			KoanfPaths: nil,
			New:        func() any { return &ArtifactsSettings{} },
		},
		{
			// profiling is durable via DB but has no settings.yaml
			// representation; it is written through PUT
			// /api/v1/admin/profiling. Absent DB row = everything off.
			// Seeding skips this section (KoanfPaths nil).
			Name:       "profiling",
			KoanfPaths: nil,
			New:        func() any { return &ProfilingSettings{} },
		},
	}

	ensureIndexes()
	compileSchemas()
}

func ensureIndexes() {
	sectionIndex = make(map[string]*Section, len(Registry))
	keyIndex = make(map[string]string)
	for i := range Registry {
		s := &Registry[i]
		sectionIndex[s.Name] = s
		for _, kp := range s.KoanfPaths {
			keyIndex[kp] = s.Name
		}
	}
}

// SectionByName returns the Section with the given name, or nil if not found.
func SectionByName(name string) *Section {
	return sectionIndex[name]
}

// SectionNames returns the names of all registered sections.
func SectionNames() []string {
	names := make([]string, len(Registry))
	for i, s := range Registry {
		names[i] = s.Name
	}
	return names
}

// OwningSection returns the section name that owns the given koanf key,
// or "" if no section owns it (i.e. it is a Layer-0 key).
func OwningSection(koanfKey string) string {
	if sec, ok := keyIndex[koanfKey]; ok {
		return sec
	}
	for prefix := koanfKey; prefix != ""; {
		idx := strings.LastIndex(prefix, ".")
		if idx < 0 {
			break
		}
		prefix = prefix[:idx]
		if sec, ok := keyIndex[prefix]; ok {
			return sec
		}
	}
	return ""
}

// IsLayer1Key reports whether the given koanf key belongs to a Layer-1 section.
func IsLayer1Key(koanfKey string) bool {
	return OwningSection(koanfKey) != ""
}

// --- Schema compilation and validation ---

var schemaCompileErr error

// rawSchemas stores the raw JSON-schema definitions per section, populated
// during compileSchemas(). Used by SchemaInfo() for the schema endpoint.
// Concurrency: written once during init() and read-only thereafter — safe
// for concurrent access without synchronization.
var rawSchemas map[string]map[string]interface{}

// SectionSchemaInfo holds the raw schema definition and koanf paths for a
// section, intended for the GET /admin/server-config/schema endpoint.
type SectionSchemaInfo struct {
	Schema     interface{} `json:"schema"`
	KoanfPaths []string    `json:"koanf_paths"`
}

// SchemaInfo returns the raw JSON-schema fragment and koanf paths for every
// registered section. The result is safe to serialize as JSON for the schema
// endpoint. Returns nil if schemas failed to compile.
func SchemaInfo() map[string]SectionSchemaInfo {
	if rawSchemas == nil {
		if schemaCompileErr != nil {
			slog.Error("Schema compilation failed during init; schema endpoint unavailable", "error", schemaCompileErr)
		}
		return nil
	}
	result := make(map[string]SectionSchemaInfo, len(Registry))
	for _, s := range Registry {
		info := SectionSchemaInfo{
			KoanfPaths: s.KoanfPaths,
		}
		if info.KoanfPaths == nil {
			info.KoanfPaths = []string{}
		}
		if schema, ok := rawSchemas[s.Name]; ok {
			info.Schema = schema
		}
		result[s.Name] = info
	}
	return result
}

func compileSchemas() {
	schemaData, err := config.GetSettingsSchemaJSON("1")
	if err != nil {
		schemaCompileErr = fmt.Errorf("opsettings: loading settings schema: %w", err)
		return
	}

	var root map[string]interface{}
	if err := json.Unmarshal(schemaData, &root); err != nil {
		schemaCompileErr = fmt.Errorf("opsettings: parsing settings schema: %w", err)
		return
	}

	defs, _ := root["$defs"].(map[string]interface{})

	sectionSchemaMap := map[string]map[string]interface{}{
		"access": {
			"type": "object",
			"properties": map[string]interface{}{
				"admin_emails":       getSchemaProperty(root, "server", "hub", "admin_emails"),
				"user_access_mode":   getSchemaProperty(root, "server", "auth", "user_access_mode"),
				"default_user_role":  getSchemaProperty(root, "server", "auth", "default_user_role"),
				"authorized_domains": getSchemaProperty(root, "server", "auth", "authorized_domains"),
			},
			"additionalProperties": false,
		},
		"lifecycle": {
			"type": "object",
			"properties": map[string]interface{}{
				"auto_suspend_stalled":          getSchemaProperty(root, "server", "hub", "auto_suspend_stalled"),
				"stalled_threshold":             getSchemaProperty(root, "server", "hub", "stalled_threshold"),
				"soft_delete_retention":         getSchemaProperty(root, "server", "hub", "soft_delete_retention"),
				"soft_delete_retain_files":      getSchemaProperty(root, "server", "hub", "soft_delete_retain_files"),
				"start_claim_lease_ttl":         getSchemaProperty(root, "server", "hub", "start_claim_lease_ttl"),
				"start_max_duration":            getSchemaProperty(root, "server", "hub", "start_max_duration"),
				"start_unconfirmed_hold":        getSchemaProperty(root, "server", "hub", "start_unconfirmed_hold"),
				"start_create_unconfirmed_hold": getSchemaProperty(root, "server", "hub", "start_create_unconfirmed_hold"),
			},
			"additionalProperties": false,
		},
		// Tech debt: maintenance schema is hand-written — admin_mode and
		// maintenance_message have no $defs in settings-v1.schema.json because
		// they are runtime state, not file config. If they are ever added to
		// the canonical schema, unify here.
		"maintenance": {
			"type": "object",
			"properties": map[string]interface{}{
				"admin_mode":          map[string]interface{}{"type": "boolean"},
				"maintenance_message": map[string]interface{}{"type": "string"},
			},
			"additionalProperties": false,
		},
		// messaging schema is hand-written — conversation_envelope_switch is
		// runtime/DB state with no $defs in settings-v1.schema.json.
		// The stale keys (conversation_read_switch, conversation_write_deny_switch)
		// are deliberately absent: additionalProperties:false rejects them on
		// write, and existing rows carrying them are never validated (Validate
		// runs only on write paths). They self-clean on first PUT.
		"messaging": {
			"type": "object",
			"properties": map[string]interface{}{
				"conversation_envelope_switch":    map[string]interface{}{"type": "boolean"},
				"cross_project_messaging_enabled": map[string]interface{}{"type": "boolean"},
				"offload_threshold_runes":         map[string]interface{}{"type": "integer", "minimum": 0},
			},
			"additionalProperties": false,
		},
		// auto_expose_ports, quotas, agent_secrets, project_defaults,
		// github_app and federation are derived from settings-v1.schema.json
		// so the section schema and the settings file schema cannot drift
		// apart (the $defs are attached to every section at compile time).
		"auto_expose_ports": schemaObject(getSchemaProperty(root, "auto_expose_ports")),
		"quotas":            schemaObject(getSchemaProperty(root, "quotas")),
		"agent_secrets":     schemaObject(getSchemaProperty(root, "agent_secrets")),
		// experiments schema is hand-written -- it is runtime/API-owned
		// state with no $defs in settings-v1.schema.json (like maintenance
		// and messaging). overrides is a map of experiment name -> bool;
		// the pattern is the single definition in experiments.NamePattern
		// (pkg/experiments), so this schema cannot drift from NewRegistry's
		// own name validation.
		"experiments": {
			"type": "object",
			"properties": map[string]interface{}{
				"overrides": map[string]interface{}{
					"type": "object",
					"patternProperties": map[string]interface{}{
						experiments.NamePattern: map[string]interface{}{"type": "boolean"},
					},
					"additionalProperties": false,
				},
			},
			"additionalProperties": false,
		},
		// artifacts schema is hand-written -- it is runtime/API-owned state
		// with no $defs in settings-v1.schema.json. The per-field bounds
		// match ArtifactsSettings.Resolve; the cross-field rules (file limit
		// <= bundle limit, default TTL <= max TTL, remote image limits
		// against the file limits, budget >= fetch timeout) are not
		// expressible here: writes are refused by validateCrossField, and a
		// stored document that breaks them is handled by Resolve (an invalid
		// remote image value turns remote images off; any other invalid
		// value disables the service).
		// profiling schema is hand-written -- it is runtime/API-owned state
		// with no $defs in settings-v1.schema.json (like messaging).
		"profiling": {
			"type": "object",
			"properties": map[string]interface{}{
				"readiness_marks": map[string]interface{}{"type": "boolean"},
			},
			"additionalProperties": false,
		},
		"artifacts": {
			"type": "object",
			"properties": map[string]interface{}{
				"enabled":                      map[string]interface{}{"type": "boolean"},
				"max_file_bytes":               map[string]interface{}{"type": "integer", "minimum": 1},
				"max_bundle_bytes":             map[string]interface{}{"type": "integer", "minimum": 1},
				"max_files":                    map[string]interface{}{"type": "integer", "minimum": 1},
				"default_retention_days":       map[string]interface{}{"type": "integer", "minimum": 0},
				"link_default_ttl_hours":       map[string]interface{}{"type": "integer", "minimum": 1},
				"link_max_ttl_hours":           map[string]interface{}{"type": "integer", "minimum": 1},
				"remote_images_enabled":        map[string]interface{}{"type": "boolean"},
				"remote_image_max_count":       map[string]interface{}{"type": "integer", "minimum": 1},
				"remote_image_max_bytes":       map[string]interface{}{"type": "integer", "minimum": 1},
				"remote_image_fetch_timeout_s": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": ArtifactsMaxRemoteImageTotalBudgetS},
				"remote_image_total_budget_s":  map[string]interface{}{"type": "integer", "minimum": 1, "maximum": ArtifactsMaxRemoteImageTotalBudgetS},
			},
			"additionalProperties": false,
		},
		"telemetry": buildTelemetrySchema(defs),
		"agent_defaults": {
			"type": "object",
			"properties": map[string]interface{}{
				"default_template":                        getSchemaProperty(root, "default_template"),
				"default_harness_config":                  getSchemaProperty(root, "default_harness_config"),
				"default_max_turns":                       getSchemaProperty(root, "default_max_turns"),
				"default_max_model_calls":                 getSchemaProperty(root, "default_max_model_calls"),
				"default_max_duration":                    getSchemaProperty(root, "default_max_duration"),
				"default_resources":                       getSchemaProperty(root, "default_resources"),
				"default_model":                           map[string]interface{}{"type": "string"},
				"default_thinking_level":                  map[string]interface{}{"type": "integer"},
				"default_max_agent_role":                  getSchemaProperty(root, "default_max_agent_role"),
				"default_agent_role":                      getSchemaProperty(root, "default_agent_role"),
				"default_runtime_broker":                  getSchemaProperty(root, "default_runtime_broker"),
				"default_timezone":                        getSchemaProperty(root, "default_timezone"),
				"default_gcp_identity_mode":               getSchemaProperty(root, "default_gcp_identity_mode"),
				"default_gcp_identity_service_account_id": getSchemaProperty(root, "default_gcp_identity_service_account_id"),
			},
			"additionalProperties": false,
		},
		"endpoints": {
			"type": "object",
			"properties": map[string]interface{}{
				"public_url":     getSchemaProperty(root, "server", "hub", "public_url"),
				"hub_name":       getSchemaProperty(root, "server", "hub", "hub_name"),
				"image_registry": getSchemaProperty(root, "image_registry"),
			},
			"additionalProperties": false,
		},
		// github_app omits private_key and webhook_secret: secret material
		// stays in the secret backend and is never stored in the DB section.
		"github_app":       withoutProperties(schemaObject(getSchemaProperty(root, "server", "github_app")), "private_key", "webhook_secret"),
		"notifications":    buildNotificationsSchema(),
		"project_defaults": schemaObject(getSchemaProperty(root, "project_defaults")),
		// runtimes: map-of-objects — keys are runtime names, values are
		// runtime config objects. additionalProperties validates each entry.
		"runtimes": {
			"type": "object",
			"additionalProperties": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type":                        map[string]interface{}{"type": "string"},
					"host":                        map[string]interface{}{"type": "string"},
					"context":                     map[string]interface{}{"type": "string"},
					"namespace":                   map[string]interface{}{"type": "string", "maxLength": 63, "pattern": dns1123LabelOrEmptyPattern},
					"env":                         map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}},
					"sync":                        map[string]interface{}{"type": "string"},
					"gke":                         map[string]interface{}{"type": "boolean"},
					"list_all_namespaces":         map[string]interface{}{"type": "boolean"},
					"priority_class_name":         map[string]interface{}{"type": "string", "maxLength": 253, "pattern": dns1123SubdomainOrEmptyPattern},
					"cloudrun":                    map[string]interface{}{"type": "object"},
					"shared_dir_storage_class":    map[string]interface{}{"type": "string"},
					"shared_dir_size":             map[string]interface{}{"type": "string"},
					"safe_to_evict":               map[string]interface{}{"type": "boolean"},
					"shared_dir_storage_backend":  map[string]interface{}{"type": "string", "enum": []string{"", "local", "nfs"}},
					"shared_dir_storage_backends": sharedDirStorageBackendsSchema(),
					"home_storage_backend":        map[string]interface{}{"type": "string", "enum": []string{"", "local", "nfs"}},
					"home_storage_leaf":           map[string]interface{}{"type": "string", "enum": []string{"", "pod", "broker"}},

					// GCP identity "assign" on Kubernetes.
					"kubernetes_service_account_mappings": kubernetesServiceAccountMappingsSchema(),
				},
			},
		},
		// profiles: map-of-objects — keys are profile names, values are
		// profile config objects.
		"profiles": {
			"type": "object",
			"additionalProperties": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"runtime":                map[string]interface{}{"type": "string"},
					"default_template":       map[string]interface{}{"type": "string"},
					"default_harness_config": map[string]interface{}{"type": "string"},
					"image_registry":         map[string]interface{}{"type": "string"},
					"env":                    map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}},
					"volumes":                map[string]interface{}{"type": "array"},
					"resources":              map[string]interface{}{"type": "object"},
					"harness_overrides": map[string]interface{}{
						"type": "object",
						"additionalProperties": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"image":              map[string]interface{}{"type": "string"},
								"image_pull_policy":  map[string]interface{}{"type": "string", "enum": []string{"Always", "IfNotPresent", "Never"}},
								"user":               map[string]interface{}{"type": "string"},
								"env":                map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}},
								"volumes":            map[string]interface{}{"type": "array"},
								"resources":          map[string]interface{}{"type": "object"},
								"auth_selected_type": map[string]interface{}{"type": "string"},
							},
						},
					},
					"secrets":                     map[string]interface{}{"type": "array"},
					"shared_dir_storage_class":    map[string]interface{}{"type": "string"},
					"shared_dir_size":             map[string]interface{}{"type": "string"},
					"safe_to_evict":               map[string]interface{}{"type": "boolean"},
					"shared_dir_storage_backend":  map[string]interface{}{"type": "string", "enum": []string{"", "local", "nfs"}},
					"shared_dir_storage_backends": sharedDirStorageBackendsSchema(),
					"home_storage_backend":        map[string]interface{}{"type": "string", "enum": []string{"", "local", "nfs"}},
					"home_storage_leaf":           map[string]interface{}{"type": "string", "enum": []string{"", "pod", "broker"}},

					// GCP identity "assign" on Kubernetes.
					"kubernetes_service_account_mappings": kubernetesServiceAccountMappingsSchema(),
				},
			},
		},
		// harness_configs: map-of-objects — keys are harness config names,
		// values are harness config entry objects.
		"harness_configs": {
			"type": "object",
			"additionalProperties": map[string]interface{}{
				"type":     "object",
				"required": []string{"harness"},
				"properties": map[string]interface{}{
					"name":               map[string]interface{}{"type": "string"},
					"harness":            map[string]interface{}{"type": "string"},
					"image":              map[string]interface{}{"type": "string"},
					"image_pull_policy":  map[string]interface{}{"type": "string", "enum": []string{"Always", "IfNotPresent", "Never"}},
					"user":               map[string]interface{}{"type": "string"},
					"model":              map[string]interface{}{"type": "string"},
					"task_flag":          map[string]interface{}{"type": "string"},
					"args":               map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					"env":                map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}},
					"volumes":            map[string]interface{}{"type": "array"},
					"auth_selected_type": map[string]interface{}{"type": "string"},
					"secrets":            map[string]interface{}{"type": "array"},
					"model_aliases":      map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}},
				},
			},
		},
		// federation: a DB section doc decodes strictly into
		// FederationSettings, so the intervals keep only the string branch
		// of the root anyOf (the file loader also takes an unquoted 0).
		"federation": withStringOnlyIntervals(schemaObject(getSchemaProperty(root, "server", "federation"))),
	}

	// SchemaInfo serves self-contained schemas: inline every local $ref so
	// a client does not need the root $defs. Compilation below still uses
	// sectionSchemaMap with the $defs attached.
	rawSchemas = make(map[string]map[string]interface{}, len(sectionSchemaMap))
	for name, def := range sectionSchemaMap {
		inlined, _ := inlineRefs(root, def, nil).(map[string]interface{})
		rawSchemas[name] = inlined
	}

	for i := range Registry {
		s := &Registry[i]
		schemaDef, ok := sectionSchemaMap[s.Name]
		if !ok {
			continue
		}
		schemaBytes, err := json.Marshal(schemaDef)
		if err != nil {
			schemaCompileErr = fmt.Errorf("opsettings: marshaling schema for %s: %w", s.Name, err)
			return
		}
		var schemaDoc interface{}
		if err := json.Unmarshal(schemaBytes, &schemaDoc); err != nil {
			schemaCompileErr = fmt.Errorf("opsettings: unmarshaling schema for %s: %w", s.Name, err)
			return
		}

		c := jsonschema.NewCompiler()
		resourceURI := fmt.Sprintf("opsettings/%s.schema.json", s.Name)

		if defs != nil {
			fullSchema := map[string]interface{}{
				"$defs": defs,
			}
			for k, v := range schemaDef {
				fullSchema[k] = v
			}
			var fullDoc interface{}
			fullBytes, _ := json.Marshal(fullSchema)
			if err := json.Unmarshal(fullBytes, &fullDoc); err != nil {
				schemaCompileErr = fmt.Errorf("opsettings: unmarshalling schema for %s: %w", s.Name, err)
				return
			}
			schemaDoc = fullDoc
		}

		if err := c.AddResource(resourceURI, schemaDoc); err != nil {
			schemaCompileErr = fmt.Errorf("opsettings: adding schema resource for %s: %w", s.Name, err)
			return
		}
		compiled, err := c.Compile(resourceURI)
		if err != nil {
			schemaCompileErr = fmt.Errorf("opsettings: compiling schema for %s: %w", s.Name, err)
			return
		}
		s.Schema = compiled
	}
}

// getSchemaProperty traverses the root schema to find a property definition.
// The path segments are: property → $ref resolution → nested property.
func getSchemaProperty(root map[string]interface{}, path ...string) interface{} {
	current := root
	for i, seg := range path {
		props, ok := current["properties"].(map[string]interface{})
		if !ok {
			return map[string]interface{}{}
		}
		val, ok := props[seg]
		if !ok {
			return map[string]interface{}{}
		}

		if i == len(path)-1 {
			valMap, ok := val.(map[string]interface{})
			if !ok {
				return val
			}
			if ref, ok := valMap["$ref"].(string); ok {
				return resolveRef(root, ref)
			}
			return val
		}

		valMap, ok := val.(map[string]interface{})
		if !ok {
			return map[string]interface{}{}
		}
		if ref, ok := valMap["$ref"].(string); ok {
			resolved := resolveRef(root, ref)
			resolvedMap, ok := resolved.(map[string]interface{})
			if !ok {
				return map[string]interface{}{}
			}
			current = resolvedMap
		} else {
			current = valMap
		}
	}
	return map[string]interface{}{}
}

// withStringOnlyIntervals returns a copy of the federation schema whose
// refresh_interval and debounce_interval keep only their string anyOf
// branch, with the parent's description carried over. The copy is shallow:
// only the top-level map, the properties map and the two new interval maps
// are new; everything else is shared with the root schema and must not be
// modified.
func withStringOnlyIntervals(node map[string]interface{}) map[string]interface{} {
	out := withoutProperties(node)
	props, ok := out["properties"].(map[string]interface{})
	if !ok || props == nil {
		return out // unexpected shape: leave the schema as it is
	}
	for _, key := range []string{"refresh_interval", "debounce_interval"} {
		prop, ok := props[key].(map[string]interface{})
		if !ok || prop == nil {
			continue
		}
		branches, _ := prop["anyOf"].([]interface{})
		for _, b := range branches {
			bm, ok := b.(map[string]interface{})
			if !ok || bm["type"] != "string" {
				continue
			}
			repl := make(map[string]interface{}, len(bm)+1)
			for k, v := range bm {
				repl[k] = v
			}
			if desc, ok := prop["description"]; ok {
				repl["description"] = desc
			}
			props[key] = repl
		}
	}
	return out
}

// inlineRefs returns a deep copy of v with every local "#/$defs/..." $ref
// replaced by the referenced definition (sibling keywords such as
// description are kept, and win over the definition's). stack holds the
// definitions being expanded; a recursive reference is left as a $ref
// rather than expanded forever.
func inlineRefs(root map[string]interface{}, v interface{}, stack []string) interface{} {
	switch node := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(node))
		if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			recursive := false
			for _, s := range stack {
				if s == ref {
					recursive = true
				}
			}
			if def, ok := resolveRef(root, ref).(map[string]interface{}); ok && !recursive {
				expanded, _ := inlineRefs(root, def, append(stack, ref)).(map[string]interface{})
				for k, val := range expanded {
					out[k] = val
				}
				for k, val := range node {
					if k != "$ref" {
						out[k] = inlineRefs(root, val, stack)
					}
				}
				return out
			}
		}
		for k, val := range node {
			out[k] = inlineRefs(root, val, stack)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(node))
		for i, val := range node {
			out[i] = inlineRefs(root, val, stack)
		}
		return out
	default:
		return v
	}
}

// schemaObject asserts a getSchemaProperty result is a schema object. A
// missing property yields an empty object, which
// TestSectionSchemas_MatchRootSchema reports.
func schemaObject(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	if m == nil {
		return map[string]interface{}{}
	}
	return m
}

// withoutProperties returns a copy of an object schema with the named
// properties removed. The root schema maps are shared, so they are copied
// rather than modified.
func withoutProperties(node map[string]interface{}, names ...string) map[string]interface{} {
	out := make(map[string]interface{}, len(node))
	for k, v := range node {
		out[k] = v
	}
	if props, ok := node["properties"].(map[string]interface{}); ok {
		cp := make(map[string]interface{}, len(props))
		for k, v := range props {
			cp[k] = v
		}
		for _, n := range names {
			delete(cp, n)
		}
		out["properties"] = cp
	}
	return out
}

func resolveRef(root map[string]interface{}, ref string) interface{} {
	if !strings.HasPrefix(ref, "#/$defs/") {
		return map[string]interface{}{}
	}
	defName := strings.TrimPrefix(ref, "#/$defs/")
	defs, ok := root["$defs"].(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	return defs[defName]
}

func buildTelemetrySchema(defs map[string]interface{}) map[string]interface{} {
	if tc, ok := defs["telemetryConfig"]; ok {
		return tc.(map[string]interface{})
	}
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": true,
	}
}

func buildNotificationsSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"notification_channels": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"type":               map[string]interface{}{"type": "string"},
						"params":             map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}},
						"filter_types":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
						"filter_urgent_only": map[string]interface{}{"type": "boolean"},
					},
					"required": []interface{}{"type"},
				},
			},
		},
		"additionalProperties": false,
	}
}

// Validate validates a section document (JSON) against the section's schema.
// Returns nil if valid, or a list of ValidationErrors on failure.
func Validate(section string, doc json.RawMessage) []config.ValidationError {
	if schemaCompileErr != nil {
		return []config.ValidationError{{Message: schemaCompileErr.Error()}}
	}

	sec := SectionByName(section)
	if sec == nil {
		return []config.ValidationError{{Message: fmt.Sprintf("unknown section %q", section)}}
	}
	if sec.Schema == nil {
		return nil
	}

	var parsed interface{}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		return []config.ValidationError{{Message: fmt.Sprintf("invalid JSON: %v", err)}}
	}

	err := sec.Schema.Validate(parsed)
	if err != nil {
		return extractValidationErrors(err)
	}
	return validateCrossField(section, doc)
}

// validateCrossField applies the rules a JSON schema cannot express, so a
// write that would leave a section unusable is refused instead of stored.
func validateCrossField(section string, doc json.RawMessage) []config.ValidationError {
	if section != "artifacts" {
		return nil
	}
	cfg, err := ParseArtifactsDoc(doc)
	if err != nil {
		return []config.ValidationError{{Message: err.Error()}}
	}
	if cfg.RemoteImagesInvalid != "" {
		return []config.ValidationError{{Message: "artifacts settings: " + cfg.RemoteImagesInvalid}}
	}
	return nil
}

func extractValidationErrors(err error) []config.ValidationError {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []config.ValidationError{{Message: err.Error()}}
	}
	basic := ve.BasicOutput()
	return collectBasicErrors(basic)
}

func collectBasicErrors(unit *jsonschema.OutputUnit) []config.ValidationError {
	var result []config.ValidationError
	if unit.Error != nil {
		path := strings.TrimPrefix(unit.InstanceLocation, "/")
		result = append(result, config.ValidationError{
			Path:    path,
			Message: unit.Error.String(),
		})
	}
	for i := range unit.Errors {
		result = append(result, collectBasicErrors(&unit.Errors[i])...)
	}
	return result
}
