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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

func settingsKoanf(t *testing.T, flat map[string]any) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		t.Fatalf("load koanf: %v", err)
	}
	return k
}

func TestDecodeSettingsKey_KnownKey(t *testing.T) {
	k := settingsKoanf(t, map[string]any{
		"server.auth.authorized_domains": []string{"a.example", "b.example"},
		"server.hub.admin_emails":        []string{"x@example.com"},
	})
	vs, err := DecodeSettingsKey(k, "server.auth.authorized_domains")
	if err != nil {
		t.Fatalf("DecodeSettingsKey: %v", err)
	}
	if vs.Server == nil || vs.Server.Auth == nil {
		t.Fatal("server.auth not decoded")
	}
	if got, want := vs.Server.Auth.AuthorizedDomains, []string{"a.example", "b.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("authorized_domains = %v, want %v", got, want)
	}
	// Only the requested key is decoded.
	if vs.Server.Hub != nil && len(vs.Server.Hub.AdminEmails) > 0 {
		t.Errorf("admin_emails decoded too: %v", vs.Server.Hub.AdminEmails)
	}
}

func TestDecodeSettingsKey_Refusals(t *testing.T) {
	k := settingsKoanf(t, map[string]any{
		"server.auth.devtokn": "typo-value",
	})
	if _, err := DecodeSettingsKey(k, "server.auth.devtokn"); err == nil {
		t.Error("a key matching no settings field must fail")
	}
	if _, err := DecodeSettingsKey(k, "server.auth.authorized_domains"); err == nil {
		t.Error("an unset key must fail")
	}
	if _, err := DecodeSettingsKey(nil, "server.auth.authorized_domains"); err == nil {
		t.Error("a nil koanf must fail")
	}
}

func TestFlattenSettingsJSON(t *testing.T) {
	k := settingsKoanf(t, map[string]any{
		"server.auth.authorized_domains": []string{"a.example", "b.example"},
	})
	vs, err := DecodeSettingsKey(k, "server.auth.authorized_domains")
	if err != nil {
		t.Fatalf("DecodeSettingsKey: %v", err)
	}
	flat, err := FlattenSettingsJSON(vs)
	if err != nil {
		t.Fatalf("FlattenSettingsJSON: %v", err)
	}
	got, ok := flat["server.auth.authorized_domains"]
	if !ok {
		t.Fatalf("key missing from %v", flat)
	}
	if want := []any{"a.example", "b.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("value = %#v, want %#v", got, want)
	}
}

func TestIsZeroSettingScalar(t *testing.T) {
	for _, v := range []any{false, "false", "0", 0, int64(0), float64(0)} {
		if !IsZeroSettingScalar(v) {
			t.Errorf("IsZeroSettingScalar(%#v) = false, want true", v)
		}
	}
	for _, v := range []any{true, "true", "1", "x", 1, []string{}, nil, map[string]any{}} {
		if IsZeroSettingScalar(v) {
			t.Errorf("IsZeroSettingScalar(%#v) = true, want false", v)
		}
	}
}

// TestLoadGlobalConfig_ServerEnvBeatsSettingsFile backs the file-tier
// env_values in_effect=true claim: the loader layers SCION_SERVER_* above
// settings.yaml, so the environment value is the one the hub runs with.
func TestLoadGlobalConfig_ServerEnvBeatsSettingsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `schema_version: "1"
server:
  auth:
    authorized_domains:
      - saved.example
`
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("LoadGlobalConfig: %v", err)
	}
	if got, want := cfg.Auth.AuthorizedDomains, []string{"saved.example"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("without env: authorized domains = %v, want %v (settings file not read)", got, want)
	}

	t.Setenv("SCION_SERVER_AUTH_AUTHORIZEDDOMAINS", "env-a.example,env-b.example")
	cfg, err = LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("LoadGlobalConfig: %v", err)
	}
	if got, want := cfg.Auth.AuthorizedDomains, []string{"env-a.example", "env-b.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("with env: authorized domains = %v, want %v", got, want)
	}
}
