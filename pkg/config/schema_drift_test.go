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
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// schemaDriftAllowList lists intentional mismatches between the Go settings
// types (rooted at VersionedSettings) and settings-v1.schema.json. Keys are
// dotted paths as reported by TestSettingsSchema_NoDriftFromGoTypes, using
// "*" for map values and "[]" for slice elements. Every entry must say why
// the mismatch is intentional.
var schemaDriftAllowList = map[string]string{
	// Schema-only: lets editors associate the file with this schema. It is
	// not a setting and has no Go field.
	"schema-only: $schema": "JSON Schema pointer for IDE support; not a setting",

	// V1NFSConfig is shared by server.workspace_storage.nfs and
	// server.shared_dir_storage.nfs, but only workspace_storage reads
	// auto_mount (see the V1NFSConfig.AutoMount comment). The shared-dir
	// schema deliberately rejects it so it is not set expecting an effect.
	"go-only: server.shared_dir_storage.nfs.auto_mount": "auto_mount is ignored by shared_dir_storage",

	// default_harness_auth was added to the schema (with
	// SCION_DEFAULT_HARNESS_AUTH) for the hub agent_defaults setting, but
	// VersionedSettings has no DefaultHarnessAuth field, so the key validates
	// and is then dropped when settings.yaml loads. Kept in the schema so
	// existing files keep validating; whether to add the Go field or remove
	// the key is tracked separately.
	"schema-only: default_harness_auth": "no VersionedSettings field yet; tracked in ptone/scion#3023",

	// Type unions: the schema accepts more than one JSON type for these
	// keys because the loader does.
	//
	// oidc.token_lifetime is a time.Duration. koanf's
	// StringToTimeDurationHook accepts a Go duration string ("15m"), and
	// an integer is taken as nanoseconds.
	"type-union: server.oidc.token_lifetime": "time.Duration: Go duration string or integer nanoseconds",
	// map[string]string values decode weakly (WeaklyTypedInput), so an
	// unquoted number or boolean loads as its string form.
	// (Notification params are deliberately string-only: the hub decodes
	// seeded channels strictly, so a number there would drop them all.)
	"type-union: server.plugins.broker.*.config.*": "weakly decoded map[string]string value",
	// Federation intervals are strings, but an unquoted YAML 0 weakly
	// decodes to "0", which ParseDuration accepts, so the schema takes the
	// integer 0 as well (anyOf string pattern | const 0).
	"type-union: server.federation.refresh_interval":  "unquoted 0 loads as \"0\"",
	"type-union: server.federation.debounce_interval": "unquoted 0 loads as \"0\"",
}

// TestSettingsSchema_NoDriftFromGoTypes walks the Go settings types reachable
// from VersionedSettings (using their json tags) alongside the settings-v1
// schema and reports:
//
//   - "go-only": a Go field with no schema property, where the schema object
//     sets additionalProperties: false. Such a key loads at runtime but is
//     rejected by `scion config validate`.
//   - "schema-only": a schema property with no Go field. Such a key validates
//     but is silently dropped when settings load.
//   - "type": the schema's JSON type does not match the Go kind (string,
//     integer, number, boolean, array, object). Nodes without a "type"
//     (e.g. a bare enum) are not compared.
//   - "type-union": the schema allows several JSON types for one Go field;
//     each such union must be allow-listed with the reason.
//
// Enums, patterns and required lists are not compared.
//
// The walk is derived from the types and the schema, not from a key list, so
// new fields are checked automatically. Intentional mismatches go in
// schemaDriftAllowList.
func TestSettingsSchema_NoDriftFromGoTypes(t *testing.T) {
	raw, err := GetSettingsSchemaJSON("1")
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(raw, &root))

	w := &schemaDriftWalker{root: root, seen: map[string]bool{}}
	w.walk("", reflect.TypeOf(VersionedSettings{}), root)

	var unexpected []string
	for _, d := range w.drift {
		if _, ok := schemaDriftAllowList[d]; !ok {
			unexpected = append(unexpected, d)
		}
	}
	sort.Strings(unexpected)

	// Every allow-list entry must still correspond to real drift, so stale
	// entries do not hide a future regression.
	found := map[string]bool{}
	for _, d := range w.drift {
		found[d] = true
	}
	var stale []string
	for k := range schemaDriftAllowList {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)

	require.Empty(t, unexpected, "settings-v1 schema drifted from the Go settings types; add the missing schema properties / Go fields, or allow-list intentional mismatches in schemaDriftAllowList:\n  %s", strings.Join(unexpected, "\n  "))
	require.Empty(t, stale, "schemaDriftAllowList entries no longer match any drift; remove them:\n  %s", strings.Join(stale, "\n  "))
}

type schemaDriftWalker struct {
	root  map[string]any
	drift []string
	seen  map[string]bool
}

// resolve follows local "$ref" pointers ("#/$defs/name").
func (w *schemaDriftWalker) resolve(node map[string]any) map[string]any {
	for i := 0; i < 16 && node != nil; i++ {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node
		}
		const prefix = "#/$defs/"
		if !strings.HasPrefix(ref, prefix) {
			return node
		}
		defs, _ := w.root["$defs"].(map[string]any)
		next, _ := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
		node = next
	}
	return node
}

func joinDriftPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// goJSONFields returns the json-tagged fields of a struct type, flattening
// anonymous embedded structs the way encoding/json does.
func goJSONFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range goJSONFields(ft) {
					out[k] = v
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func (w *schemaDriftWalker) walk(path string, t reflect.Type, node map[string]any) {
	node = w.resolve(node)
	if node == nil {
		return
	}
	if path != "" {
		w.checkType(path, t, node)
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Map:
		if sub, ok := node["additionalProperties"].(map[string]any); ok {
			w.walk(joinDriftPath(path, "*"), t.Elem(), sub)
		}
		return
	case reflect.Slice, reflect.Array:
		if sub, ok := node["items"].(map[string]any); ok {
			w.walk(joinDriftPath(path, "[]"), t.Elem(), sub)
		}
		return
	case reflect.Struct:
	default:
		return
	}

	props, hasProps := node["properties"].(map[string]any)
	if !hasProps {
		// The schema leaves this object open (e.g. a free-form map or a
		// type with no declared shape); nothing to compare.
		return
	}
	// Recursive types would otherwise loop.
	key := path + "|" + t.String()
	if w.seen[key] {
		return
	}
	w.seen[key] = true

	closed := node["additionalProperties"] == false
	fields := goJSONFields(t)
	for name, ft := range fields {
		sub, ok := props[name].(map[string]any)
		if !ok {
			if closed {
				w.drift = append(w.drift, "go-only: "+joinDriftPath(path, name))
			}
			continue
		}
		w.walk(joinDriftPath(path, name), ft, sub)
	}
	for name := range props {
		if _, ok := fields[name]; !ok {
			w.drift = append(w.drift, "schema-only: "+joinDriftPath(path, name))
		}
	}
}

// goJSONKind returns the JSON Schema type a Go type decodes from.
func goJSONKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return ""
	}
}

// checkType compares the schema node's "type" with the Go kind.
func (w *schemaDriftWalker) checkType(path string, t reflect.Type, node map[string]any) {
	goKind := goJSONKind(t)
	if goKind == "" {
		return
	}
	typeVal := node["type"]
	if typeVal == nil {
		// An anyOf of typed branches is a union of their types.
		if branches, ok := node["anyOf"].([]any); ok {
			var kinds []any
			for _, b := range branches {
				if bm, ok := b.(map[string]any); ok {
					if k, ok := bm["type"].(string); ok {
						kinds = append(kinds, k)
					}
				}
			}
			if len(kinds) > 0 {
				typeVal = kinds
			}
		}
	}
	switch typ := typeVal.(type) {
	case string:
		if typ != goKind {
			w.drift = append(w.drift, "type: "+path+" (schema "+typ+", go "+goKind+")")
		}
	case []any:
		// "null" next to one other type only makes the key nullable; the
		// loader decodes null to the zero value.
		var kinds []string
		for _, v := range typ {
			if k, _ := v.(string); k != "null" {
				kinds = append(kinds, k)
			}
		}
		if len(kinds) == 1 {
			if kinds[0] != goKind {
				w.drift = append(w.drift, "type: "+path+" (schema "+kinds[0]+", go "+goKind+")")
			}
			return
		}
		w.drift = append(w.drift, "type-union: "+path)
		for _, k := range kinds {
			if k == goKind {
				return
			}
		}
		w.drift = append(w.drift, "type: "+path+" (schema union lacks go "+goKind+")")
	}
}
