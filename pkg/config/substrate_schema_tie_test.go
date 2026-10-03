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
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// substrateSchemaProperties loads the embedded settings-v1 schema the same
// way validateAgainstSchema does, and returns the property set of
// $defs.runtimeConfig.properties.substrate.
func substrateSchemaProperties(t *testing.T) map[string]bool {
	t.Helper()

	schemaData, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)

	var schemaDoc map[string]interface{}
	require.NoError(t, json.Unmarshal(schemaData, &schemaDoc))

	defs, ok := schemaDoc["$defs"].(map[string]interface{})
	require.True(t, ok, "schema has no $defs")

	runtimeConfig, ok := defs["runtimeConfig"].(map[string]interface{})
	require.True(t, ok, "schema has no $defs.runtimeConfig")

	runtimeProps, ok := runtimeConfig["properties"].(map[string]interface{})
	require.True(t, ok, "$defs.runtimeConfig has no properties")

	substrate, ok := runtimeProps["substrate"].(map[string]interface{})
	require.True(t, ok, "$defs.runtimeConfig.properties has no substrate")

	substrateProps, ok := substrate["properties"].(map[string]interface{})
	require.True(t, ok, "substrate object has no properties")

	result := make(map[string]bool, len(substrateProps))
	for name := range substrateProps {
		result[name] = true
	}
	return result
}

// v1SubstrateConfigJSONFields returns the set of JSON field names declared on
// V1SubstrateConfig, derived from its `json` struct tags (stripping
// ",omitempty"). Every field must carry a usable json tag: none of
// V1SubstrateConfig's fields are deliberately excluded from JSON today, so a
// missing or "-" json tag fails the test instead of being silently skipped
// (a silent skip would let a new field bypass the schema tie unnoticed). If
// V1SubstrateConfig ever gains a field that is genuinely JSON-exempt, that
// exemption should be visible here rather than absorbed by a blanket skip.
// It also asserts that each field's `yaml` and `koanf` tag names match its
// `json` name, since settings files are keyed by the yaml/koanf name.
func v1SubstrateConfigJSONFields(t *testing.T) map[string]bool {
	t.Helper()

	rt := reflect.TypeOf(V1SubstrateConfig{})
	result := make(map[string]bool, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		jsonTag := field.Tag.Get("json")
		jsonName := strings.Split(jsonTag, ",")[0]
		if jsonTag == "" || jsonTag == "-" || jsonName == "" || jsonName == "-" {
			t.Errorf("V1SubstrateConfig.%s has no usable json tag (got %q); every field must "+
				"name itself explicitly, since none of this struct's fields are deliberately "+
				"excluded from JSON", field.Name, jsonTag)
			continue
		}
		result[jsonName] = true

		if yamlName := strings.Split(field.Tag.Get("yaml"), ",")[0]; yamlName != jsonName {
			t.Errorf("V1SubstrateConfig.%s yaml tag name %q does not match json name %q",
				field.Name, yamlName, jsonName)
		}
		if koanfName := strings.Split(field.Tag.Get("koanf"), ",")[0]; koanfName != jsonName {
			t.Errorf("V1SubstrateConfig.%s koanf tag name %q does not match json name %q",
				field.Name, koanfName, jsonName)
		}
	}
	return result
}

// TestSubstrateSchemaStructTie asserts that the substrate object in the
// embedded settings-v1 schema and V1SubstrateConfig's json tags name exactly
// the same set of fields, in both directions: it fails if a new
// V1SubstrateConfig field is ever added without a matching schema property
// (or vice versa).
func TestSubstrateSchemaStructTie(t *testing.T) {
	schemaFields := substrateSchemaProperties(t)
	structFields := v1SubstrateConfigJSONFields(t)

	for name := range structFields {
		if !schemaFields[name] {
			t.Errorf("V1SubstrateConfig field with json tag %q has no matching property in the schema's substrate object", name)
		}
	}
	for name := range schemaFields {
		if !structFields[name] {
			t.Errorf("schema's substrate object has property %q with no matching V1SubstrateConfig field", name)
		}
	}
}
