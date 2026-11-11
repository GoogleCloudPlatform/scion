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
	"strings"
	"testing"
)

// Design §5.E: the documented env var uses the collapsed segment form that
// the existing mappers already understand.
func TestLoadEnvKoanf_DefaultUserRole(t *testing.T) {
	t.Setenv("SCION_SERVER_AUTH_DEFAULTUSERROLE", "viewer")
	if got := LoadEnvKoanf().String("server.auth.default_user_role"); got != "viewer" {
		t.Errorf("LoadEnvKoanf server.auth.default_user_role = %q, want viewer", got)
	}
}

func TestLoadSeedEnvKoanf_DefaultUserRole(t *testing.T) {
	t.Setenv("SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE", "viewer")
	if got := LoadSeedEnvKoanf().String("server.auth.default_user_role"); got != "viewer" {
		t.Errorf("LoadSeedEnvKoanf server.auth.default_user_role = %q, want viewer", got)
	}
}

// findSchemaProperty walks the "properties" objects of a JSON schema along
// path and returns the leaf property object.
func findSchemaProperty(t *testing.T, root map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	node := root
	for _, seg := range path {
		props, ok := node["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("schema: no properties at %q (path %v)", seg, path)
		}
		next, ok := props[seg].(map[string]interface{})
		if !ok {
			t.Fatalf("schema: property %q not found (path %v)", seg, path)
		}
		// Resolve local "#/..." refs (e.g. "#/$defs/serverAuth").
		if ref, ok := next["$ref"].(string); ok {
			next = resolveLocalSchemaRef(t, root, ref)
		}
		node = next
	}
	return node
}

func resolveLocalSchemaRef(t *testing.T, root map[string]interface{}, ref string) map[string]interface{} {
	t.Helper()
	if !strings.HasPrefix(ref, "#/") {
		t.Fatalf("schema: unsupported $ref %q", ref)
	}
	node := root
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		next, ok := node[seg].(map[string]interface{})
		if !ok {
			t.Fatalf("schema: $ref %q does not resolve at %q", ref, seg)
		}
		node = next
	}
	return node
}

// The schema's x-env-var for default_user_role must map to its own koanf
// path through the same mapper LoadEnvKoanf uses, so the Admin UI env badge
// and the documented name actually work.
func TestSchema_DefaultUserRoleEnvVarMapsToOwnKey(t *testing.T) {
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	prop := findSchemaProperty(t, root, "server", "auth", "default_user_role")
	envVar, _ := prop["x-env-var"].(string)
	if envVar != "SCION_SERVER_AUTH_DEFAULTUSERROLE" {
		t.Errorf("x-env-var = %q, want SCION_SERVER_AUTH_DEFAULTUSERROLE", envVar)
	}
	if !strings.HasPrefix(envVar, "SCION_SERVER_") {
		t.Fatalf("x-env-var %q lacks SCION_SERVER_ prefix", envVar)
	}
	if got := serverEnvToOpsettingsKey(strings.TrimPrefix(envVar, "SCION_SERVER_")); got != "server.auth.default_user_role" {
		t.Errorf("serverEnvToOpsettingsKey(%q) = %q, want server.auth.default_user_role", envVar, got)
	}
}

// Legacy server.yaml migration keeps default_user_role (design Q2 / §5.A item 5).
func TestConvertGlobalToV1ServerConfig_DefaultUserRoleRoundTrip(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.Auth.DefaultUserRole = "viewer"

	v1 := ConvertGlobalToV1ServerConfig(&gc)
	if v1.Auth == nil || v1.Auth.DefaultUserRole != "viewer" {
		t.Fatalf("ConvertGlobalToV1ServerConfig dropped DefaultUserRole: %+v", v1.Auth)
	}
	gc2 := ConvertV1ServerToGlobalConfig(v1)
	if gc2.Auth.DefaultUserRole != "viewer" {
		t.Errorf("round-trip DefaultUserRole = %q, want viewer", gc2.Auth.DefaultUserRole)
	}
}
