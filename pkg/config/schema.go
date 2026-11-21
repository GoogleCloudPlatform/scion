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

package config

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"gopkg.in/yaml.v3"
)

//go:embed schemas/settings-v1.schema.json schemas/agent-v1.schema.json
var schemasFS embed.FS

// Supported schema versions. Maps version string to embedded file path.
var settingsSchemaFiles = map[string]string{
	"1": "schemas/settings-v1.schema.json",
}

var agentSchemaFiles = map[string]string{
	"1": "schemas/agent-v1.schema.json",
}

// ValidationError represents a single schema validation error.
type ValidationError struct {
	// Path is the JSON pointer to the field that failed validation (e.g., "hub/endpoint").
	Path string `json:"path"`
	// Message describes the validation failure.
	Message string `json:"message"`
	// StorageField marks a project storage field rule
	// (ValidateProjectStorage). Path is then the field's settings path. A
	// settings migration converts the file anyway and reports these as
	// warnings, so the operator gets the file to fix.
	StorageField bool `json:"storage_field,omitempty"`
}

func (e ValidationError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %s", e.Path, e.Message)
	}
	return e.Message
}

// DetectSettingsFormat inspects raw settings data (YAML or JSON) and determines
// whether it uses the versioned format or the legacy format.
//
// Returns:
//   - version: the schema_version value if present (e.g., "1"), or "1" if v1-only
//     runtime fields are detected (see v1RuntimeIndicatorKeys), or ""
//     for legacy/empty files.
//   - isLegacy: true if the file uses the legacy format (has "harnesses" key but no schema_version)
//
// An empty or missing file returns ("", false).
func DetectSettingsFormat(data []byte) (version string, isLegacy bool) {
	if len(data) == 0 {
		return "", false
	}

	// Parse into a generic map to inspect top-level keys.
	var raw map[string]interface{}
	if err := unmarshalSettingsYAML(data, &raw); err != nil {
		// Unparseable data is treated as neither versioned nor legacy.
		return "", false
	}

	if raw == nil {
		return "", false
	}

	// Check for schema_version key (versioned format).
	if sv, ok := raw["schema_version"]; ok {
		if s, ok := sv.(string); ok {
			return s, false
		}
		// schema_version exists but is not a string — treat as versioned with
		// unknown version; validation will catch the type mismatch.
		return fmt.Sprintf("%v", sv), false
	}

	// No schema_version. Check for legacy indicators.
	if _, ok := raw["harnesses"]; ok {
		return "", true
	}

	// Check for v1-only structural indicators in runtime entries.
	// A runtimes map containing any v1RuntimeIndicatorKeys key indicates a v1-format file that is missing the schema_version marker.
	// Treat as versioned v1 to prevent silent data loss via the legacy loading path.
	if hasV1RuntimeIndicators(raw) {
		return "1", false
	}

	// No schema_version and no harnesses key — could be a minimal or empty file.
	// Treat as neither versioned nor legacy (will use defaults).
	return "", false
}

// v1RuntimeIndicatorKeys are runtime-entry keys that exist only on
// V1RuntimeConfig, not on the legacy RuntimeConfig. A file using any of them
// without schema_version is loaded as v1 so the key is not dropped by the
// legacy loader.
var v1RuntimeIndicatorKeys = []string{
	"type", "cloudrun", "gke", "list_all_namespaces", "shared_dir_storage_class", "shared_dir_size", "safe_to_evict",
	"shared_dir_storage_backend", "shared_dir_storage_backends", "home_storage_backend", "home_storage_leaf",
	"priority_class_name", "runtime_class_name", "node_selector", "tolerations",
	"cloudrun_instances", "cloudrun_sandbox", "substrate",
	"kubernetes_service_account_mappings", "kubernetes_block_service_account", "workload_identity_pool", "create_service_accounts",
}

// hasV1RuntimeIndicators reports whether a parsed settings map contains v1-only
// runtime fields (v1RuntimeIndicatorKeys) that are absent from the legacy
// RuntimeConfig struct. Used to detect v1-shaped files missing schema_version.
func hasV1RuntimeIndicators(raw map[string]interface{}) bool {
	runtimes, ok := raw["runtimes"]
	if !ok {
		return false
	}
	rmap, ok := runtimes.(map[string]interface{})
	if !ok {
		return false
	}
	for _, rv := range rmap {
		entry, ok := rv.(map[string]interface{})
		if !ok {
			continue
		}
		for _, key := range v1RuntimeIndicatorKeys {
			if _, has := entry[key]; has {
				return true
			}
		}
	}
	return false
}

// ValidateSettings validates raw settings data (YAML or JSON) against the
// embedded JSON Schema for the given schema version.
//
// The data is first converted from YAML to a generic map, then validated
// against the JSON Schema. Returns a slice of validation errors (empty if valid).
//
// Returns an error (not ValidationError) if the schema version is unsupported
// or if the data cannot be parsed.
//
// For schema version "1" it also checks value formats the schema cannot
// express, such as shared_dir_size being a Kubernetes quantity, and that a
// shared_dir_storage_backend of "nfs" has a complete
// server.shared_dir_storage.nfs block. It also validates the Kubernetes
// placement fields and requires runtime entries with literally the same
// context and namespace to agree on placement (ValidatePlacement).
func ValidateSettings(data []byte, schemaVersion string) ([]ValidationError, error) {
	errs, err := validateAgainstSchema(data, schemaVersion, settingsSchemaFiles)
	if err != nil || schemaVersion != "1" {
		return errs, err
	}
	var vs struct {
		Runtimes map[string]V1RuntimeConfig `yaml:"runtimes"`
		Profiles map[string]V1ProfileConfig `yaml:"profiles"`
		Server   struct {
			ProjectStorage   *V1ProjectStorageConfig   `yaml:"project_storage"`
			WorkspaceStorage *V1WorkspaceStorageConfig `yaml:"workspace_storage"`
			SharedDirStorage *V1SharedDirStorageConfig `yaml:"shared_dir_storage"`
			HomeStorage      *V1HomeStorageConfig      `yaml:"home_storage"`
			Broker           struct {
				Instances []V1RuntimeBrokerInstanceConfig `yaml:"instances"`
			} `yaml:"broker"`
		} `yaml:"server"`
	}
	// A decode failure here (e.g. a wrongly typed field) is already
	// reported by the schema pass above.
	if unmarshalSettingsYAML(data, &vs) == nil {
		errs = append(errs, ValidateSharedDirSizes(vs.Runtimes, vs.Profiles)...)
		errs = append(errs, RemovedStorageOverrideErrors(vs.Runtimes, vs.Profiles, RuntimeBrokerInstanceTargets(vs.Server.Broker.Instances))...)
		if err := ValidateStorageBlocks(vs.Server.ProjectStorage, vs.Server.WorkspaceStorage, vs.Server.SharedDirStorage, vs.Server.HomeStorage); err != nil {
			var fields FieldErrorList
			if errors.As(err, &fields) {
				for _, f := range fields {
					errs = append(errs, ValidationError{Path: f.Path, Message: strings.TrimPrefix(strings.TrimPrefix(f.Error(), f.Path), ": "), StorageField: true})
				}
			} else {
				errs = append(errs, ValidationError{Path: ProjectStorageKey, Message: err.Error()})
			}
		}
		errs = append(errs, ValidatePlacement(vs.Runtimes, vs.Profiles, LiteralSiblingGroups(vs.Runtimes))...)
		errs = append(errs, RuntimeBrokerInstanceDuplicateKeys(vs.Server.Broker.Instances)...)
		errs = append(errs, RuntimeBrokerInstanceSharedDirSizes(vs.Server.Broker.Instances)...)
		errs = append(errs, RuntimeBrokerInstanceCloudRunServiceAccounts(vs.Server.Broker.Instances)...)
	}
	return errs, nil
}

// SettingsWarnings returns non-fatal findings for a schema version "1"
// settings file: settings that are accepted but have no effect where they
// are set, such as safe_to_evict on a non-Kubernetes runtime entry. Data
// that does not decode yields no warnings (ValidateSettings reports it).
func SettingsWarnings(data []byte, schemaVersion string) []string {
	if schemaVersion != "1" {
		return nil
	}
	var vs struct {
		Runtimes         map[string]V1RuntimeConfig    `yaml:"runtimes"`
		Profiles         map[string]V1ProfileConfig    `yaml:"profiles"`
		HarnessConfigs   map[string]HarnessConfigEntry `yaml:"harness_configs"`
		DefaultResources *api.ResourceSpec             `yaml:"default_resources"`
		Server           struct {
			HomeStorage *V1HomeStorageConfig `yaml:"home_storage"`
		} `yaml:"server"`
	}
	if unmarshalSettingsYAML(data, &vs) != nil {
		return nil
	}
	warnings := SafeToEvictIgnoredWarnings(vs.Runtimes, vs.Profiles)
	// Resource quantities the server-config API rejects or warns about
	// (ptone/scion#6581) are warnings here: a loaded file is never refused
	// for them.
	qErrs, qWarnings := ValidateResourceQuantities(vs.DefaultResources, vs.Profiles)
	for _, e := range append(qErrs, qWarnings...) {
		warnings = append(warnings, e.Error())
	}
	warnings = append(warnings, PlacementIgnoredWarnings(vs.Runtimes, vs.Profiles)...)
	warnings = append(warnings, PlacementSettingsWarnings(vs.Runtimes, vs.Profiles)...)
	warnings = append(warnings, HarnessArgsIgnoredWarnings(vs.HarnessConfigs)...)
	return append(warnings, HomeStorageWindowWarnings(vs.Server.HomeStorage)...)
}

// ValidateAgentConfig validates raw agent config data (YAML or JSON) against
// the embedded JSON Schema for the given schema version.
func ValidateAgentConfig(data []byte, schemaVersion string) ([]ValidationError, error) {
	return validateAgainstSchema(data, schemaVersion, agentSchemaFiles)
}

// agentV1TopLevelProps holds the top-level property names from
// agent-v1.schema.json for exact and Unicode simple case-folding lookup
// (matching Go's encoding/json struct field matching).
type agentV1PropIndex struct {
	exact map[string]struct{}
	names []string
}

var agentV1TopLevelProps = sync.OnceValue(func() agentV1PropIndex {
	idx := agentV1PropIndex{exact: make(map[string]struct{})}
	schemaData, err := schemasFS.ReadFile(agentSchemaFiles["1"])
	if err != nil {
		return idx
	}
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schemaData, &parsed); err != nil {
		return idx
	}
	idx.names = make([]string, 0, len(parsed.Properties))
	for k := range parsed.Properties {
		idx.exact[k] = struct{}{}
		idx.names = append(idx.names, k)
	}
	sort.Strings(idx.names)
	return idx
})

// canonicalAgentV1Prop maps key to its canonical top-level property name in
// agent-v1.schema.json using exact match first, then strings.EqualFold
// (Unicode simple case folding, matching encoding/json). Keys that do not
// match any schema property are returned unchanged.
func canonicalAgentV1Prop(key string) string {
	idx := agentV1TopLevelProps()
	if _, ok := idx.exact[key]; ok {
		return key
	}
	for _, prop := range idx.names {
		if strings.EqualFold(key, prop) {
			return prop
		}
	}
	return key
}

// decodeJSONCanonicalTopLevel decodes a JSON document using UseNumber and,
// when the root value is a JSON object, streams its top-level members in
// document order so each key is canonicalized with canonicalAgentV1Prop and
// later colliding keys overwrite earlier ones — matching Go's encoding/json
// unmarshaling into api.ScionConfig.
func decodeJSONCanonicalTopLevel(data []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); ok && delim == '{' {
		m := make(map[string]interface{})
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kTok.(string)
			if !ok {
				return nil, fmt.Errorf("expected object key string, got %T", kTok)
			}
			var val interface{}
			if err := dec.Decode(&val); err != nil {
				return nil, err
			}
			m[canonicalAgentV1Prop(key)] = val
		}
		if _, err := dec.Token(); err != nil { // closing '}'
			return nil, err
		}
		if _, err := dec.Token(); err != io.EOF {
			if err == nil {
				err = errors.New("unexpected trailing data")
			}
			return nil, err
		}
		return m, nil
	}

	// Non-object root value (array, scalar, null): decode normally so schema
	// validation reports the type mismatch.
	dec2 := json.NewDecoder(bytes.NewReader(data))
	dec2.UseNumber()
	var doc interface{}
	if err := dec2.Decode(&doc); err != nil {
		return nil, err
	}
	var trailing interface{}
	if err := dec2.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected trailing data")
		}
		return nil, err
	}
	return doc, nil
}

// ValidateTemplateAgentConfig validates a template's agent config file the way
// the runtime reads it. name is the file name (selects YAML vs JSON).
//
// Empty or comments-only files return no findings, matching LoadConfig and
// ParseScionAgentConfig. YAML files normalize top-level hyphenated keys to
// underscored keys before validation; JSON files canonicalize top-level keys
// in document order using Unicode simple case folding (strings.EqualFold),
// matching encoding/json.
func ValidateTemplateAgentConfig(name string, data []byte) ([]ValidationError, error) {
	const schemaVersion = "1"
	schemaPath := agentSchemaFiles[schemaVersion]

	var doc interface{}
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".yaml" || ext == ".yml" {
		var node yaml.Node
		if err := unmarshalSettingsYAML(data, &node); err != nil {
			return nil, fmt.Errorf("failed to parse agent config %s: %w", name, err)
		}
		if node.Kind == 0 {
			return nil, nil
		}
		normalizeYAMLMappingKeys(&node)
		if err := decodeSettingsYAMLNode(&node, &doc); err != nil {
			return nil, fmt.Errorf("failed to parse agent config %s: %w", name, err)
		}
		if doc == nil {
			return nil, nil
		}
		doc = convertYAMLToJSON(doc)
	} else {
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, nil
		}
		var err error
		doc, err = decodeJSONCanonicalTopLevel(data)
		if err != nil {
			return nil, fmt.Errorf("failed to parse agent config %s: %w", name, err)
		}
	}

	schemaData, err := schemasFS.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded schema: %w", err)
	}
	return validateDocAgainstSchemaJSON(doc, schemaPath, schemaData)
}

const maxFormattedAgentConfigFindings = 5

// FormatAgentConfigValidationError formats template agent config validation
// findings in the canonical human-readable format:
//
//	template <name>: agent config <file> is invalid: <path>: <message>[; ...] [(+K more)]
//
// At most 5 findings are included in the summary; document-level findings
// (empty Path) print as "(document)".
func FormatAgentConfigValidationError(templateName, fileName string, findings []ValidationError) string {
	limit := len(findings)
	if limit > maxFormattedAgentConfigFindings {
		limit = maxFormattedAgentConfigFindings
	}
	parts := make([]string, 0, limit)
	for _, f := range findings[:limit] {
		path := f.Path
		if path == "" {
			path = "(document)"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", path, f.Message))
	}
	msg := fmt.Sprintf("template %s: agent config %s is invalid: %s",
		templateName, fileName, strings.Join(parts, "; "))
	if extra := len(findings) - limit; extra > 0 {
		msg += fmt.Sprintf(" (+%d more)", extra)
	}
	return msg
}

// ValidateHarnessConfig validates a standalone harness-config config.yaml by
// wrapping it in the versioned settings schema's harness_configs map.
func ValidateHarnessConfig(data []byte) ([]ValidationError, error) {
	return validateHarnessConfigWith(data, func(wrapped []byte) ([]ValidationError, error) {
		return ValidateSettings(wrapped, "1")
	})
}

// validateHarnessConfigWith wraps a harness-config config.yaml in a v1
// settings document and validates it with validateSettings. Tests pass a
// validator for an older settings schema (see
// TestHarnessConfigs_ValidateAgainstPre3698Schema).
func validateHarnessConfigWith(data []byte, validateSettings func([]byte) ([]ValidationError, error)) ([]ValidationError, error) {
	var doc interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse harness config: %w", err)
	}

	wrapped := map[string]interface{}{
		"schema_version": "1",
		"harness_configs": map[string]interface{}{
			"_": doc,
		},
	}
	wrappedData, err := yaml.Marshal(wrapped)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare harness config for validation: %w", err)
	}

	errors, err := validateSettings(wrappedData)
	if err != nil {
		return nil, err
	}
	for i := range errors {
		errors[i].Path = strings.TrimPrefix(errors[i].Path, "harness_configs/_/")
		errors[i].Path = strings.TrimPrefix(errors[i].Path, "harness_configs/_")
	}
	return errors, nil
}

// validateAgainstSchema performs JSON Schema validation using the specified
// schema file map.
func validateAgainstSchema(data []byte, schemaVersion string, schemaFiles map[string]string) ([]ValidationError, error) {
	schemaPath, ok := schemaFiles[schemaVersion]
	if !ok {
		supported := make([]string, 0, len(schemaFiles))
		for v := range schemaFiles {
			supported = append(supported, v)
		}
		return nil, fmt.Errorf("unsupported schema version %q (supported: %s)", schemaVersion, strings.Join(supported, ", "))
	}

	// Parse the input data from YAML to a generic structure.
	var doc interface{}
	if err := unmarshalSettingsYAML(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse settings: %w", err)
	}

	// Convert YAML-native types to JSON-compatible types.
	// yaml.v3 produces map[string]interface{} which is what jsonschema expects,
	// but we need to ensure numeric types and nested structures are correct.
	doc = convertYAMLToJSON(doc)

	// Load the embedded schema.
	schemaData, err := schemasFS.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded schema: %w", err)
	}
	return validateDocAgainstSchemaJSON(doc, schemaPath, schemaData)
}

// validateDocAgainstSchemaJSON validates doc (already converted by
// convertYAMLToJSON) against the JSON Schema schemaData, registered under
// resource name schemaPath.
func validateDocAgainstSchemaJSON(doc interface{}, schemaPath string, schemaData []byte) ([]ValidationError, error) {
	var schemaDoc interface{}
	if err := json.Unmarshal(schemaData, &schemaDoc); err != nil {
		return nil, fmt.Errorf("failed to parse schema: %w", err)
	}

	// Compile the schema.
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaPath, schemaDoc); err != nil {
		return nil, fmt.Errorf("failed to add schema resource: %w", err)
	}
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("failed to compile schema: %w", err)
	}

	// Validate the document.
	err = schema.Validate(doc)
	if err == nil {
		return nil, nil
	}

	// Extract validation errors.
	return extractValidationErrors(err), nil
}

// extractValidationErrors converts jsonschema validation errors into our
// ValidationError type using the DetailedOutput tree so wrapper units with
// child errors (such as "validation failed") are dropped and leaf findings
// name the specific failing field.
func extractValidationErrors(err error) []ValidationError {
	validationErr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []ValidationError{{Message: err.Error()}}
	}

	detailed := validationErr.DetailedOutput()
	return collectBasicErrors(detailed)
}

// collectBasicErrors extracts leaf validation errors from the OutputUnit tree.
// Wrapper units that have child errors are skipped in favour of their children;
// missing required property errors append the missing property name to Path.
func collectBasicErrors(unit *jsonschema.OutputUnit) []ValidationError {
	if len(unit.Errors) > 0 {
		var result []ValidationError
		for i := range unit.Errors {
			result = append(result, collectBasicErrors(&unit.Errors[i])...)
		}
		if len(result) > 0 {
			return result
		}
	}

	if unit.Error == nil {
		return nil
	}

	path := strings.TrimPrefix(unit.InstanceLocation, "/")
	if req, ok := unit.Error.Kind.(*kind.Required); ok && len(req.Missing) > 0 {
		result := make([]ValidationError, 0, len(req.Missing))
		for _, prop := range req.Missing {
			p := prop
			if path != "" {
				p = path + "/" + prop
			}
			result = append(result, ValidationError{
				Path:    p,
				Message: "missing required property",
			})
		}
		return result
	}

	return []ValidationError{{
		Path:    path,
		Message: unit.Error.String(),
	}}
}

// convertYAMLToJSON converts YAML-decoded values to JSON-compatible types.
// The main issue is that YAML integers need to remain as-is (not float64),
// and map keys need to be strings.
func convertYAMLToJSON(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(val))
		for k, v := range val {
			result[k] = convertYAMLToJSON(v)
		}
		return result
	case map[interface{}]interface{}:
		// YAML sometimes produces this type for maps.
		result := make(map[string]interface{}, len(val))
		for k, v := range val {
			result[fmt.Sprintf("%v", k)] = convertYAMLToJSON(v)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, v := range val {
			result[i] = convertYAMLToJSON(v)
		}
		return result
	default:
		return v
	}
}

// GetSettingsSchemaJSON returns the raw JSON Schema for the given settings
// schema version. This can be used to expose the schema to users or tools.
func GetSettingsSchemaJSON(version string) ([]byte, error) {
	path, ok := settingsSchemaFiles[version]
	if !ok {
		return nil, fmt.Errorf("unsupported schema version %q", version)
	}
	return schemasFS.ReadFile(path)
}

// GetAgentSchemaJSON returns the raw JSON Schema for the given agent config
// schema version.
func GetAgentSchemaJSON(version string) ([]byte, error) {
	path, ok := agentSchemaFiles[version]
	if !ok {
		return nil, fmt.Errorf("unsupported schema version %q", version)
	}
	return schemasFS.ReadFile(path)
}
