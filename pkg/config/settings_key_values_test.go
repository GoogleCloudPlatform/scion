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
	"fmt"
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

// TestDecodeSettingsKey_SplitsCommaSeparatedListEnv: a list key whose value
// arrives as a comma-separated string (the SCION_SERVER_* form) decodes to the
// list the bootstrap merge produces, entries trimmed and empties dropped.
func TestDecodeSettingsKey_SplitsCommaSeparatedListEnv(t *testing.T) {
	k := settingsKoanf(t, map[string]any{
		"server.auth.authorized_domains": "env-a.example, env-b.example,",
		"server.hub.admin_emails":        "only@example.com",
	})
	vs, err := DecodeSettingsKey(k, "server.auth.authorized_domains")
	if err != nil {
		t.Fatalf("DecodeSettingsKey: %v", err)
	}
	if got, want := vs.Server.Auth.AuthorizedDomains, []string{"env-a.example", "env-b.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("authorized_domains = %#v, want %#v", got, want)
	}
	vs, err = DecodeSettingsKey(k, "server.hub.admin_emails")
	if err != nil {
		t.Fatalf("DecodeSettingsKey: %v", err)
	}
	if got, want := vs.Server.Hub.AdminEmails, []string{"only@example.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("admin_emails = %#v, want %#v", got, want)
	}
	// The caller's koanf is not changed.
	if got := k.Get("server.auth.authorized_domains"); got != "env-a.example, env-b.example," {
		t.Errorf("source koanf changed: %#v", got)
	}
}

// TestDecodeSettingsKey_ListEnvMatchesBootstrapMerge covers every list-typed
// setting the SCION_SERVER_* table holds: set as a comma-separated variable,
// the value env_values decodes from the env layer (LoadEnvKoanf, then
// DecodeSettingsKey) has the same entries as the value the hub's bootstrap
// merge itself holds for the key (LoadBootstrapKoanf().Get, read directly, so
// the bootstrap side does not go through DecodeSettingsKey). The keys the
// bootstrap splits (commaSplitKoanfKeys) give two entries; the others keep
// the single string, in both.
func TestDecodeSettingsKey_ListEnvMatchesBootstrapMerge(t *testing.T) {
	split := map[string]bool{}
	for _, k := range commaSplitKoanfKeys {
		split[k] = true
	}
	var listKeys []ServerEnvEntry
	for _, e := range ServerEnvEntries() {
		if e.Type == "array" {
			listKeys = append(listKeys, e)
		}
	}
	if len(listKeys) == 0 {
		t.Fatal("no list-typed settings in the env table")
	}
	for k := range split {
		found := false
		for _, e := range listKeys {
			found = found || e.Path == k
		}
		if !found {
			t.Errorf("comma-split key %s is not a list-typed table entry", k)
		}
	}
	for _, e := range listKeys {
		e := e
		t.Run(e.Path, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv(e.Canonical, "8081,8082")
			envK := LoadEnvKoanf()
			vs, err := DecodeSettingsKey(envK, e.Path)
			if err != nil {
				t.Fatalf("decode from the env layer: %v", err)
			}
			got, err := FlattenSettingsJSON(vs)
			if err != nil {
				t.Fatal(err)
			}
			// The bootstrap merge's own value for the key: a list after its
			// split, else the raw string (one entry).
			var want []string
			switch b := LoadBootstrapKoanf().Get(e.Path).(type) {
			case []any:
				for _, x := range b {
					want = append(want, fmt.Sprint(x))
				}
			case []string:
				want = append(want, b...)
			case string:
				want = []string{b}
			default:
				t.Fatalf("bootstrap merge value %#v (%T) is neither a list nor a string", b, b)
			}
			list, ok := got[e.Path].([]any)
			if !ok {
				t.Fatalf("decoded value %#v is not a list", got[e.Path])
			}
			var have []string
			for _, x := range list {
				have = append(have, fmt.Sprint(x))
			}
			if !reflect.DeepEqual(have, want) {
				t.Errorf("env layer entries %#v != bootstrap merge entries %#v", have, want)
			}
			wantN := 1
			if split[e.Path] {
				wantN = 2
			}
			if len(list) != wantN {
				t.Errorf("decoded %d element(s) %#v, want %d", len(list), list, wantN)
			}
		})
	}
}

// TestDecodeSettingsKey_ReportedTwoDomainCase: the reported case,
// SCION_SERVER_AUTH_AUTHORIZEDDOMAINS with two comma-separated domains,
// decodes from the env layer to the two domains, as the hub's settings
// loader (LoadGlobalConfig) also reads it.
func TestDecodeSettingsKey_ReportedTwoDomainCase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_AUTH_AUTHORIZEDDOMAINS", "env-a.example,env-b.example")
	vs, err := DecodeSettingsKey(LoadEnvKoanf(), "server.auth.authorized_domains")
	if err != nil {
		t.Fatalf("DecodeSettingsKey: %v", err)
	}
	want := []string{"env-a.example", "env-b.example"}
	if got := vs.Server.Auth.AuthorizedDomains; !reflect.DeepEqual(got, want) {
		t.Errorf("env layer decode = %#v, want %#v", got, want)
	}
	cfg, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("LoadGlobalConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg.Auth.AuthorizedDomains, want) {
		t.Errorf("LoadGlobalConfig = %#v, want %#v", cfg.Auth.AuthorizedDomains, want)
	}
}
