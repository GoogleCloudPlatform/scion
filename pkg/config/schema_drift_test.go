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
	"schema-only: default_harness_auth": "no VersionedSettings field yet; see comment",
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
