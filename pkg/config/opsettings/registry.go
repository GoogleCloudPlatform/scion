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
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Section describes a single Layer-1 operational settings section.
type Section struct {
	Name       string
	Schema     *jsonschema.Schema
	KoanfPaths []string
	New        func() any
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
			KoanfPaths: []string{"server.hub.admin_emails", "server.auth.user_access_mode", "server.auth.authorized_domains"},
			New:        func() any { return &AccessSettings{} },
		},
		{
			Name:       "lifecycle",
			KoanfPaths: []string{"server.hub.auto_suspend_stalled", "server.hub.soft_delete_retention", "server.hub.soft_delete_retain_files"},
			New:        func() any { return &LifecycleSettings{} },
		},
		{
			// maintenance is durable via DB but has no settings.yaml representation.
			// It is runtime/API-owned state: absent DB row = compiled defaults
			// (admin_mode=false). Seeding skips this section. The env var
			// SCION_SERVER_ADMINMODE remains a per-node force-enable (design §3.4/§3.8).
			Name:       "maintenance",
			KoanfPaths: nil,
			New:        func() any { return &MaintenanceSettings{} },
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
			Name: "agent_defaults",
			KoanfPaths: []string{
				"default_template", "default_harness_config",
				"default_max_turns", "default_max_model_calls",
				"default_max_duration", "default_resources",
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
				"authorized_domains": getSchemaProperty(root, "server", "auth", "authorized_domains"),
			},
			"additionalProperties": false,
		},
		"lifecycle": {
			"type": "object",
			"properties": map[string]interface{}{
				"auto_suspend_stalled":     getSchemaProperty(root, "server", "hub", "auto_suspend_stalled"),
				"soft_delete_retention":    getSchemaProperty(root, "server", "hub", "soft_delete_retention"),
				"soft_delete_retain_files": getSchemaProperty(root, "server", "hub", "soft_delete_retain_files"),
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
		"telemetry": buildTelemetrySchema(defs),
		"agent_defaults": {
			"type": "object",
			"properties": map[string]interface{}{
				"default_template":        getSchemaProperty(root, "default_template"),
				"default_harness_config":  getSchemaProperty(root, "default_harness_config"),
				"default_max_turns":       getSchemaProperty(root, "default_max_turns"),
				"default_max_model_calls": getSchemaProperty(root, "default_max_model_calls"),
				"default_max_duration":    getSchemaProperty(root, "default_max_duration"),
				"default_resources":       getSchemaProperty(root, "default_resources"),
			},
			"additionalProperties": false,
		},
		"endpoints": {
			"type": "object",
			"properties": map[string]interface{}{
				"public_url":     getSchemaProperty(root, "server", "hub", "public_url"),
				"image_registry": getSchemaProperty(root, "image_registry"),
			},
			"additionalProperties": false,
		},
		// Tech debt: github_app schema is hand-written — the canonical
		// settings-v1.schema.json has no $defs for GitHub App fields. If a
		// gitHubApp $def is added later, unify here.
		"github_app": {
			"type": "object",
			"properties": map[string]interface{}{
				"app_id":           map[string]interface{}{"type": "integer"},
				"api_base_url":     map[string]interface{}{"type": "string"},
				"webhooks_enabled": map[string]interface{}{"type": "boolean"},
				"installation_url": map[string]interface{}{"type": "string"},
				"private_key_path": map[string]interface{}{"type": "string"},
			},
			"additionalProperties": false,
		},
		"notifications": buildNotificationsSchema(),
	}

	rawSchemas = sectionSchemaMap

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
	if err == nil {
		return nil
	}

	return extractValidationErrors(err)
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
