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
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// schemaEnvVarEntry is one x-env-var annotation found in the settings schema.
type schemaEnvVarEntry struct {
	Path    string // dotted settings.yaml path, e.g. server.hub.admin_emails
	EnvVar  string
	Type    string
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

// versionedSettingsReadServerKeys lists the Layer-0 server.* keys whose
// effective value the hub takes from LoadVersionedSettings rather than from
// GlobalConfig: broker identity, resolved in cmd/server_foreground.go
// (resolveBrokerID, resolveBrokerName, the auto_provide lookup) and
// convertVersionedToLegacy (broker_token -> settings.Hub.BrokerToken). Every
// other Layer-0 key is read from GlobalConfig, and every Layer-1 key from
// the opsettings env/bootstrap koanf.
var versionedSettingsReadServerKeys = map[string]bool{
	"server.broker.broker_id":       true,
	"server.broker.broker_name":     true,
	"server.broker.broker_nickname": true,
	"server.broker.broker_token":    true,
	"server.broker.auto_provide":    true,
}

// globalConfigReadLayer1Keys lists registry (Layer-1) keys whose live value
// the hub still takes from GlobalConfig at startup rather than from the
// opsettings snapshot: server.hub.public_url is Hub.Endpoint, consumed by
// resolveHubEndpoint in cmd/server_foreground.go; the snapshot's PublicURL
// only feeds the admin server-config view.
var globalConfigReadLayer1Keys = map[string]bool{
	"server.hub.public_url": true,
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
// (versionedSettingsReadServerKeys), and GlobalConfig for every other
// Layer-0 key. The list of names is derived from the schema, so a new
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
	for k := range versionedSettingsReadServerKeys {
		if !seenPaths[k] {
			t.Errorf("versionedSettingsReadServerKeys entry %q has no x-env-var in the schema; remove it", k)
		}
	}
	for k := range globalConfigReadLayer1Keys {
		if !seenPaths[k] || !IsLayer1Key(k) {
			t.Errorf("globalConfigReadLayer1Keys entry %q is not a Layer-1 key with an x-env-var; remove it", k)
		}
	}

	// One HOME and config dir for every load: GlobalConfig derives
	// defaults (e.g. the sqlite URL) from them, so they must not vary
	// between the baseline and the per-variable loads.
	t.Setenv("HOME", t.TempDir())
	configDir := t.TempDir()
	baselineGC, err := config.LoadGlobalConfig(configDir)
	if err != nil {
		t.Fatalf("baseline LoadGlobalConfig: %v", err)
	}

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

			case versionedSettingsReadServerKeys[e.Path]:
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
				for _, v := range samples {
					t.Setenv(e.EnvVar, v)
					gc, err := config.LoadGlobalConfig(configDir)
					if err != nil {
						t.Fatalf("LoadGlobalConfig with %s=%q: %v", e.EnvVar, v, err)
					}
					if !reflect.DeepEqual(gc, baselineGC) {
						return
					}
				}
				t.Errorf("%s (%s, read via GlobalConfig) does not change GlobalConfig for any of %q; the hub never sees it",
					e.EnvVar, e.Path, samples)
			}
		})
	}
}
