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
	"strings"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// layer0Prefixes defines the Layer-0 bootstrap key prefixes from design §3.1.
// These are settings that require a restart and MUST NOT be written to the DB.
// The classification logic matches any key that equals or is nested under these
// prefixes (e.g. "server.database" matches "server.database.driver").
//
// See design §3.1 "Two tiers" table for the full rationale.
var layer0Prefixes = []string{
	// Database
	"server.database",
	// Listeners
	"server.hub.port",
	"server.hub.host",
	"server.hub.read_timeout",
	"server.hub.write_timeout",
	"server.broker",
	// Auth stack
	"server.auth.mode",
	"server.auth.dev_mode",
	"server.auth.dev_token",
	// N4: dev_token_file was missing — added per design §3.1 "auth.dev_*".
	"server.auth.dev_token_file",
	"server.auth.proxy",
	"server.auth.transport",
	"server.oauth",
	// Secrets/storage
	"server.secrets",
	"server.storage",
	"server.workspace_storage",
	// Identity/mode
	"server.mode",
	"server.env",
	"server.hub.hub_id",
	"server.hub.gcp_project_id",
	// Logging
	"server.log_level",
	"server.log_format",
	// CORS
	"server.hub.cors",
	// Messaging/plugins
	"server.message_broker",
	"server.plugins",
}

// isLayer0Key reports whether the given koanf key belongs to the Layer-0
// bootstrap set (design §3.1). Matches exact keys and any nested children.
func isLayer0Key(key string) bool {
	for _, prefix := range layer0Prefixes {
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			return true
		}
	}
	return false
}

// koanfPathToJSONField maps a koanf path to the JSON field name used in the
// section document. For most paths the last segment is the field name, but some
// sections aggregate fields from multiple koanf subtrees, requiring explicit
// mapping.
var koanfPathToJSONField = map[string]map[string]string{
	"access": {
		"server.hub.admin_emails":        "admin_emails",
		"server.auth.user_access_mode":   "user_access_mode",
		"server.auth.authorized_domains": "authorized_domains",
	},
	"lifecycle": {
		"server.hub.auto_suspend_stalled":     "auto_suspend_stalled",
		"server.hub.soft_delete_retention":    "soft_delete_retention",
		"server.hub.soft_delete_retain_files": "soft_delete_retain_files",
	},
	"endpoints": {
		"server.hub.public_url": "public_url",
		"image_registry":        "image_registry",
	},
	"github_app": {
		"server.github_app.app_id":           "app_id",
		"server.github_app.api_base_url":     "api_base_url",
		"server.github_app.webhooks_enabled": "webhooks_enabled",
		"server.github_app.installation_url": "installation_url",
		"server.github_app.private_key_path": "private_key_path",
	},
}

// jsonFieldToKoanfPaths maps section name → json field → koanf path for the
// reverse direction (section doc → koanf keyspace).
var jsonFieldToKoanfPaths = map[string]map[string]string{
	"access": {
		"admin_emails":       "server.hub.admin_emails",
		"user_access_mode":   "server.auth.user_access_mode",
		"authorized_domains": "server.auth.authorized_domains",
	},
	"lifecycle": {
		"auto_suspend_stalled":     "server.hub.auto_suspend_stalled",
		"soft_delete_retention":    "server.hub.soft_delete_retention",
		"soft_delete_retain_files": "server.hub.soft_delete_retain_files",
	},
	"endpoints": {
		"public_url":     "server.hub.public_url",
		"image_registry": "image_registry",
	},
	"github_app": {
		"app_id":           "server.github_app.app_id",
		"api_base_url":     "server.github_app.api_base_url",
		"webhooks_enabled": "server.github_app.webhooks_enabled",
		"installation_url": "server.github_app.installation_url",
		"private_key_path": "server.github_app.private_key_path",
	},
}

// ExtractSectionFromKoanf extracts a section's JSON document from a koanf
// instance that has the full file-based settings loaded. This is used for
// startup seeding: file values → section documents.
func ExtractSectionFromKoanf(k *koanf.Koanf, sectionName string) (json.RawMessage, error) {
	sec := SectionByName(sectionName)
	if sec == nil {
		return nil, fmt.Errorf("unknown section %q", sectionName)
	}

	if len(sec.KoanfPaths) == 0 {
		return json.Marshal(map[string]interface{}{})
	}

	switch sectionName {
	case "telemetry":
		return extractSubtree(k, "telemetry")
	case "agent_defaults":
		return extractAgentDefaults(k)
	case "github_app":
		return extractGitHubApp(k)
	case "notifications":
		return extractNotifications(k)
	default:
		return extractMappedSection(k, sectionName)
	}
}

func extractSubtree(k *koanf.Koanf, prefix string) (json.RawMessage, error) {
	sub := k.Cut(prefix)
	if sub.Raw() == nil || len(sub.Keys()) == 0 {
		return json.Marshal(map[string]interface{}{})
	}
	data, err := json.Marshal(sub.Raw())
	if err != nil {
		return nil, err
	}
	return data, nil
}

func extractAgentDefaults(k *koanf.Koanf) (json.RawMessage, error) {
	doc := make(map[string]interface{})
	fields := []string{"default_template", "default_harness_config", "default_max_turns",
		"default_max_model_calls", "default_max_duration", "default_resources"}
	for _, f := range fields {
		if k.Exists(f) {
			doc[f] = k.Get(f)
		}
	}
	return json.Marshal(doc)
}

func extractGitHubApp(k *koanf.Koanf) (json.RawMessage, error) {
	return extractMappedSection(k, "github_app")
}

func extractNotifications(k *koanf.Koanf) (json.RawMessage, error) {
	doc := make(map[string]interface{})
	if k.Exists("server.notification_channels") {
		doc["notification_channels"] = k.Get("server.notification_channels")
	}
	return json.Marshal(doc)
}

func extractMappedSection(k *koanf.Koanf, sectionName string) (json.RawMessage, error) {
	mapping, ok := koanfPathToJSONField[sectionName]
	if !ok {
		return json.Marshal(map[string]interface{}{})
	}
	doc := make(map[string]interface{})
	for koanfPath, jsonField := range mapping {
		if k.Exists(koanfPath) {
			doc[jsonField] = k.Get(koanfPath)
		}
	}
	return json.Marshal(doc)
}

// LoadSectionsIntoKoanf loads a set of section documents into a new koanf
// instance using the same keyspace as the file-based config. The resulting
// koanf can be merged between the file and env providers using koanf.Merge().
//
// sections maps section name → JSON document.
func LoadSectionsIntoKoanf(sections map[string]json.RawMessage) (*koanf.Koanf, error) {
	k := koanf.New(".")

	for sectionName, doc := range sections {
		if err := loadSectionIntoKoanf(k, sectionName, doc); err != nil {
			return nil, fmt.Errorf("loading section %q: %w", sectionName, err)
		}
	}
	return k, nil
}

func loadSectionIntoKoanf(k *koanf.Koanf, sectionName string, doc json.RawMessage) error {
	sec := SectionByName(sectionName)
	if sec != nil && len(sec.KoanfPaths) == 0 {
		return nil
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(doc, &raw); err != nil {
		return err
	}

	switch sectionName {
	case "telemetry":
		return loadPrefixed(k, "telemetry", raw)
	case "agent_defaults":
		return k.Load(confmap.Provider(raw, "."), nil)
	case "github_app":
		return loadMappedSection(k, "github_app", raw)
	case "notifications":
		flat := make(map[string]interface{})
		for key, val := range raw {
			flat["server."+key] = val
		}
		return k.Load(confmap.Provider(flat, "."), nil)
	default:
		return loadMappedSection(k, sectionName, raw)
	}
}

func loadPrefixed(k *koanf.Koanf, prefix string, raw map[string]interface{}) error {
	flat := make(map[string]interface{})
	flattenMap(prefix, raw, flat)
	return k.Load(confmap.Provider(flat, "."), nil)
}

func flattenMap(prefix string, m map[string]interface{}, out map[string]interface{}) {
	for key, val := range m {
		fullKey := prefix + "." + key
		if sub, ok := val.(map[string]interface{}); ok {
			flattenMap(fullKey, sub, out)
		} else {
			out[fullKey] = val
		}
	}
}

func loadMappedSection(k *koanf.Koanf, sectionName string, raw map[string]interface{}) error {
	mapping, ok := jsonFieldToKoanfPaths[sectionName]
	if !ok {
		return nil
	}
	flat := make(map[string]interface{})
	for jsonField, koanfPath := range mapping {
		if val, exists := raw[jsonField]; exists {
			flat[koanfPath] = val
		}
	}
	return k.Load(confmap.Provider(flat, "."), nil)
}

// EnvOverriddenLayer1Keys returns the set of Layer-1 koanf keys that are
// present in the env provider's keyspace. These keys indicate per-node drift
// from the shared DB values.
func EnvOverriddenLayer1Keys(envKeys []string) []string {
	var overridden []string
	for _, ek := range envKeys {
		if IsLayer1Key(ek) {
			overridden = append(overridden, ek)
		}
	}
	return overridden
}

// DetectEnvOverrides creates a koanf instance from the SCION_ environment
// variables and returns which Layer-1 keys are overridden by env.
// The envMapper should be the same mapper used by the main config loader.
func DetectEnvOverrides(envKoanf *koanf.Koanf) []string {
	var overridden []string
	for _, key := range envKoanf.Keys() {
		if IsLayer1Key(key) {
			overridden = append(overridden, key)
		}
	}
	return overridden
}

// ExtractAllSections extracts all registered section documents from a fully
// loaded koanf instance. Returns a map of section name → JSON document.
func ExtractAllSections(k *koanf.Koanf) (map[string]json.RawMessage, error) {
	result := make(map[string]json.RawMessage, len(Registry))
	for _, sec := range Registry {
		doc, err := ExtractSectionFromKoanf(k, sec.Name)
		if err != nil {
			return nil, fmt.Errorf("extracting section %q: %w", sec.Name, err)
		}
		result[sec.Name] = doc
	}
	return result, nil
}

// MergeSectionsIntoKoanf is a convenience that calls LoadSectionsIntoKoanf and
// then merges the result into the target koanf instance. This places DB values
// above file values but below env values in the precedence chain.
func MergeSectionsIntoKoanf(target *koanf.Koanf, sections map[string]json.RawMessage) error {
	overlay, err := LoadSectionsIntoKoanf(sections)
	if err != nil {
		return err
	}
	return target.Merge(overlay)
}

// Layer1KoanfKeys returns all koanf keys that belong to Layer-1 sections,
// sorted by section. Useful for classifying incoming PUT payloads.
func Layer1KoanfKeys() map[string][]string {
	result := make(map[string][]string, len(Registry))
	for _, sec := range Registry {
		result[sec.Name] = sec.KoanfPaths
	}
	return result
}

// ClassifyKeys partitions a set of koanf keys into three groups:
//   - layer1: keys owned by a Layer-1 section, grouped by section name
//   - layer0: keys explicitly in the Layer-0 bootstrap set (design §3.1) — must be rejected
//   - unclassified: keys not in any Layer-1 section and not explicitly Layer-0 — should be ignored
//
// Used by PUT partitioning: Layer-1 → write to DB, Layer-0 → 422 reject,
// unclassified → ignore with warning.
func ClassifyKeys(keys []string) (layer1 map[string][]string, layer0 []string, unclassified []string) {
	layer1 = make(map[string][]string)
	for _, key := range keys {
		sec := OwningSection(key)
		if sec != "" {
			layer1[sec] = append(layer1[sec], key)
		} else if isLayer0Key(key) {
			layer0 = append(layer0, key)
		} else {
			unclassified = append(unclassified, key)
		}
	}
	return layer1, layer0, unclassified
}
