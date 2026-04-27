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
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
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
	Path string
	// Message describes the validation failure.
	Message string
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
//   - version: the schema_version value if present (e.g., "1"), or "" for legacy/empty
//   - isLegacy: true if the file uses the legacy format (has "harnesses" key but no schema_version)
//
// An empty or missing file returns ("", false).
func DetectSettingsFormat(data []byte) (version string, isLegacy bool) {
	if len(data) == 0 {
		return "", false
	}

	// Parse into a generic map to inspect top-level keys.
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
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

	// No schema_version and no harnesses key — could be a minimal or empty file.
	// Treat as neither versioned nor legacy (will use defaults).
	return "", false
}

// ValidateSettings validates raw settings data (YAML or JSON) against the
// embedded JSON Schema for the given schema version.
//
// The data is first converted from YAML to a generic map, then validated
// against the JSON Schema. Returns a slice of validation errors (empty if valid).
//
// Returns an error (not ValidationError) if the schema version is unsupported
// or if the data cannot be parsed.
func ValidateSettings(data []byte, schemaVersion string) ([]ValidationError, error) {
	return validateAgainstSchema(data, schemaVersion, settingsSchemaFiles)
}

// ValidateAgentConfig validates raw agent config data (YAML or JSON) against
// the embedded JSON Schema for the given schema version.
func ValidateAgentConfig(data []byte, schemaVersion string) ([]ValidationError, error) {
	return validateAgainstSchema(data, schemaVersion, agentSchemaFiles)
}

// ValidateHarnessConfig validates a standalone harness-config config.yaml by
// wrapping it in the versioned settings schema's harness_configs map.
func ValidateHarnessConfig(data []byte) ([]ValidationError, error) {
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

	errors, err := ValidateSettings(wrappedData, "1")
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
	if err := yaml.Unmarshal(data, &doc); err != nil {
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
// ValidationError type using the BasicOutput format.
func extractValidationErrors(err error) []ValidationError {
	validationErr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []ValidationError{{Message: err.Error()}}
	}

	basic := validationErr.BasicOutput()
	return collectBasicErrors(basic)
}

// collectBasicErrors extracts leaf validation errors from the BasicOutput tree.
func collectBasicErrors(unit *jsonschema.OutputUnit) []ValidationError {
	var result []ValidationError

	if unit.Error != nil {
		path := strings.TrimPrefix(unit.InstanceLocation, "/")
		result = append(result, ValidationError{
			Path:    path,
			Message: unit.Error.String(),
		})
	}

	for i := range unit.Errors {
		result = append(result, collectBasicErrors(&unit.Errors[i])...)
	}

	return result
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
