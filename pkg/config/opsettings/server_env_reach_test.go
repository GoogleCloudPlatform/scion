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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// schemaEnvVarEntry is one x-env-var annotation found in the settings schema.
type schemaEnvVarEntry struct {
	Path    string // dotted settings.yaml path, e.g. server.hub.admin_emails
	EnvVar  string
	Type    string
	Items   string // item type of an array property
	Enum    []string
	Default interface{}
}

// collectSchemaEnvVars walks settings-v1.schema.json (properties and local
// $refs) and returns every property carrying an x-env-var annotation, plus
// one entry per x-env-var-alias.
func collectSchemaEnvVars(t *testing.T) []schemaEnvVarEntry {
	t.Helper()
	data, err := config.GetSettingsSchemaJSON("1")
	if err != nil {
		t.Fatalf("read settings schema: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse settings schema: %v", err)
	}
	defs, _ := root["$defs"].(map[string]interface{})

	var out []schemaEnvVarEntry
	var walk func(node map[string]interface{}, path string, depth int)
	walk = func(node map[string]interface{}, path string, depth int) {
		if depth > 20 {
			return
		}
		if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			if def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]interface{}); ok {
				walk(def, path, depth+1)
			}
		}
		if ev, ok := node["x-env-var"].(string); ok && ev != "" {
			e := schemaEnvVarEntry{Path: path, EnvVar: ev, Default: node["default"]}
			e.Type, _ = node["type"].(string)
			if items, ok := node["items"].(map[string]interface{}); ok {
				e.Items, _ = items["type"].(string)
			}
			if enum, ok := node["enum"].([]interface{}); ok {
				for _, v := range enum {
					e.Enum = append(e.Enum, fmt.Sprint(v))
				}
			}
			out = append(out, e)
			if alias, ok := node["x-env-var-alias"].(string); ok && alias != "" {
				a := e
				a.EnvVar = alias
				out = append(out, a)
			}
		}
		props, _ := node["properties"].(map[string]interface{})
		for name, child := range props {
			if cm, ok := child.(map[string]interface{}); ok {
				p := name
				if path != "" {
					p = path + "." + name
				}
				walk(cm, p, depth+1)
			}
		}
	}
	walk(root, "", 0)
	sort.Slice(out, func(i, j int) bool { return out[i].EnvVar < out[j].EnvVar })
	return out
}

// globalConfigReadLayer1Keys lists registry (Layer-1) keys whose live value
// the hub still takes from GlobalConfig at startup rather than from the
// opsettings snapshot: server.hub.public_url is Hub.Endpoint, consumed by
// resolveHubEndpoint in cmd/server_foreground.go; the snapshot's PublicURL
// only feeds the admin server-config view.
var globalConfigReadLayer1Keys = map[string]bool{
	"server.hub.public_url": true,
}

// schemaToGlobalConfigPath maps the schema keys whose GlobalConfig field is
// not the mechanical translation (see globalConfigPathFor).
var schemaToGlobalConfigPath = map[string]string{
	"server.hub.public_url": "hub.endpoint", // ConvertV1ServerToGlobalConfig
}

// globalConfigModeExceptions lists "<ENV_VAR>@<mode>" pairs, where mode is
// legacy or settings, in which a schema env var legitimately does not reach
// GlobalConfig. Each entry must cite the issue that tracks it, e.g.
// "SCION_SERVER_OIDCLOGIN_ENABLED@legacy": "ptone/scion#3038" if that name
// were advertised in the schema. None are needed today.
var globalConfigModeExceptions = map[string]string{}

// globalConfigPathFor maps a schema path (server.hub.read_timeout) to the
// GlobalConfig koanf path (hub.read_timeout, matched loosely by structLeaf):
// strip "server.", and server.broker is GlobalConfig.RuntimeBroker.
func globalConfigPathFor(schemaPath string) string {
	if p, ok := schemaToGlobalConfigPath[schemaPath]; ok {
		return p
	}
	p := strings.TrimPrefix(schemaPath, "server.")
	if rest, ok := strings.CutPrefix(p, "broker."); ok {
		p = "runtimeBroker." + rest
	}
	return p
}

func normKey(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }

// structLeaf follows a dotted path through v by koanf tag (ignoring case
// and underscores, so read_timeout finds readTimeout) and returns the leaf.
// A nil pointer on the way means the leaf is absent.
func structLeaf(v reflect.Value, path string) (reflect.Value, bool) {
	for _, seg := range strings.Split(path, ".") {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		found := false
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			tag := strings.Split(f.Tag.Get("koanf"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if normKey(tag) == normKey(seg) {
				v = v.Field(i)
				found = true
				break
			}
		}
		if !found {
			return reflect.Value{}, false
		}
	}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	return v, true
}

// leafEquals reports whether a GlobalConfig leaf holds the env value s.
func leafEquals(v reflect.Value, s string) bool {
	if !v.IsValid() {
		return false
	}
	if v.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(s)
		return err == nil && time.Duration(v.Int()) == d
	}
	if v.Kind() == reflect.Slice {
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = fmt.Sprint(v.Index(i).Interface())
		}
		return strings.Join(parts, ",") == s
	}
	return fmt.Sprint(v.Interface()) == s
}

// sampleEnvValues returns candidate values for an env override of entry, in
// the order to try. Several are offered so that at least one differs from
// the field's default.
func sampleEnvValues(e schemaEnvVarEntry) []string {
	if len(e.Enum) > 0 {
		var out []string
		for _, v := range e.Enum {
			if fmt.Sprint(e.Default) != v {
				out = append(out, v)
			}
		}
		return out
	}
	switch e.Type {
	case "boolean":
		return []string{"true", "false"}
	case "integer", "number":
		return []string{"4243", "17"}
	case "array":
		if e.Items == "integer" {
			return []string{"4243,17"}
		}
		return []string{"envtest-a@example.com,envtest-b@example.com"}
	default:
		// A duration-shaped value is also a valid plain string.
		return []string{"17s", "http://envtest.example.com:4243"}
	}
}

// jsonPathValue returns the value at a dotted path in v's JSON encoding.
func jsonPathValue(t *testing.T, v interface{}, path string) (interface{}, bool) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var cur interface{}
	if err := json.Unmarshal(data, &cur); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// TestSchemaServerEnvVars_ReachHubConfig sets every SCION_SERVER_* name the
// settings schema advertises (x-env-var) and checks the value reaches the
// config the hub actually reads for that key: the opsettings env koanf for
// Layer-1 keys, LoadVersionedSettings for broker identity
// (config.VersionedSettingsReadServerKeys), and the specific GlobalConfig
// field for every other Layer-0 key, on both the legacy and the
// settings.yaml load paths. The list of names is derived from the schema, so a new
// x-env-var whose spelling no loader maps to the field fails here
// (ptone/scion#1081).
func TestSchemaServerEnvVars_ReachHubConfig(t *testing.T) {
	entries := collectSchemaEnvVars(t)

	var server []schemaEnvVarEntry
	seenPaths := map[string]bool{}
	for _, e := range entries {
		if strings.HasPrefix(e.EnvVar, "SCION_SERVER_") {
			server = append(server, e)
			seenPaths[e.Path] = true
		}
	}
	// Guard the walker itself: the schema has ~50 SCION_SERVER_ entries.
	if len(server) < 30 {
		t.Fatalf("found only %d SCION_SERVER_* x-env-var entries; schema walker is broken", len(server))
	}
	for k := range config.VersionedSettingsReadServerKeys {
		if !seenPaths[k] {
			t.Errorf("config.VersionedSettingsReadServerKeys entry %q has no x-env-var in the schema; remove it", k)
		}
	}
	seenEnv := map[string]bool{}
	for _, e := range server {
		seenEnv[e.EnvVar] = true
	}
	for k := range globalConfigModeExceptions {
		name, mode, _ := strings.Cut(k, "@")
		if !seenEnv[name] || (mode != "legacy" && mode != "settings") {
			t.Errorf("globalConfigModeExceptions entry %q names no schema env var or an unknown mode; remove it", k)
		}
	}
	for k := range globalConfigReadLayer1Keys {
		if !seenPaths[k] || !IsLayer1Key(k) {
			t.Errorf("globalConfigReadLayer1Keys entry %q is not a Layer-1 key with an x-env-var; remove it", k)
		}
	}

	// GlobalConfig is checked on both load paths: the legacy one (no
	// settings.yaml, so server.yaml + env) and the settings.yaml one
	// (loadServerFromSettingsFile + applyEnvOverrides), which bind env
	// names differently.
	legacyHome := t.TempDir()
	settingsHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(settingsHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsHome, ".scion", "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  mode: workstation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gcModes := []struct{ name, home string }{{"legacy", legacyHome}, {"settings", settingsHome}}
	configDir := t.TempDir()
	baseline := map[string]*config.GlobalConfig{}
	for _, m := range gcModes {
		t.Setenv("HOME", m.home)
		gc, err := config.LoadGlobalConfig(configDir)
		if err != nil {
			t.Fatalf("baseline LoadGlobalConfig (%s): %v", m.name, err)
		}
		baseline[m.name] = gc
	}
	t.Setenv("HOME", legacyHome)

	for _, e := range server {
		t.Run(e.EnvVar, func(t *testing.T) {
			samples := sampleEnvValues(e)
			if len(samples) == 0 {
				t.Fatalf("no sample value for %s (type %q)", e.Path, e.Type)
			}

			switch {
			case IsLayer1Key(e.Path) && !globalConfigReadLayer1Keys[e.Path]:
				v := samples[0]
				t.Setenv(e.EnvVar, v)
				k := config.LoadEnvKoanf()
				if got := k.String(e.Path); got != v {
					t.Errorf("%s=%q is a Layer-1 key but LoadEnvKoanf()[%q]=%q (keys: %v); the hub never sees it",
						e.EnvVar, v, e.Path, got, k.Keys())
				}

			case config.VersionedSettingsReadServerKeys[e.Path]:
				v := samples[0]
				t.Setenv(e.EnvVar, v)
				vs, err := config.LoadVersionedSettings("")
				if err != nil {
					t.Fatalf("LoadVersionedSettings with %s=%q: %v", e.EnvVar, v, err)
				}
				got, ok := jsonPathValue(t, vs, e.Path)
				if !ok || fmt.Sprint(got) != v {
					t.Errorf("%s=%q: VersionedSettings %s = %v (present=%v); the hub never sees it",
						e.EnvVar, v, e.Path, got, ok)
				}

			default:
				gcPath := globalConfigPathFor(e.Path)
				for _, m := range gcModes {
					if reason, skip := globalConfigModeExceptions[e.EnvVar+"@"+m.name]; skip {
						t.Logf("%s: skipped in %s mode: %s", e.EnvVar, m.name, reason)
						continue
					}
					base, ok := structLeaf(reflect.ValueOf(baseline[m.name]), gcPath)
					if !ok {
						base = reflect.Value{}
					}
					v := samples[0]
					for _, cand := range samples {
						if !ok || !leafEquals(base, cand) {
							v = cand
							break
						}
					}
					t.Setenv("HOME", m.home)
					t.Setenv(e.EnvVar, v)
					gc, err := config.LoadGlobalConfig(configDir)
					if err != nil {
						t.Fatalf("LoadGlobalConfig (%s) with %s=%q: %v", m.name, e.EnvVar, v, err)
					}
					got, found := structLeaf(reflect.ValueOf(gc), gcPath)
					if !found || !leafEquals(got, v) {
						var shown interface{} = "<absent>"
						if found {
							shown = got.Interface()
						}
						t.Errorf("%s=%q (%s mode): GlobalConfig %s = %v; the hub never sees it",
							e.EnvVar, v, m.name, gcPath, shown)
					}
				}
			}
		})
	}
}
