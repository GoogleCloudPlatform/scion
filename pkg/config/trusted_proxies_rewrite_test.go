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
	"strings"
	"testing"
)

// trustedProxiesState is server.trusted_proxies as the hub resolves it at
// startup: whether it is set, its list, and the transport policy's
// networks.
type trustedProxiesState struct {
	set         bool
	list        []string
	usedDefault bool
	nets        []string
}

// loadTrustedProxiesState loads the server config from dir as the hub does
// at startup and resolves the transport policy's trusted networks.
func loadTrustedProxiesState(t *testing.T, dir string) trustedProxiesState {
	t.Helper()
	gc, err := LoadGlobalConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	nets, usedDefault, err := TransportTrustedProxies(gc.TrustedProxies, gc.TrustedProxiesSet)
	if err != nil {
		t.Fatal(err)
	}
	return trustedProxiesState{set: gc.TrustedProxiesSet, list: gc.TrustedProxies, usedDefault: usedDefault, nets: netStrings(nets)}
}

// checkTrustedProxiesState compares got with the expected state of an
// explicit empty list (set, no proxy trusted), a populated list, or no
// setting (the loopback default).
func checkTrustedProxiesState(t *testing.T, when string, got trustedProxiesState, wantSet bool, wantList []string) {
	t.Helper()
	if got.set != wantSet {
		t.Errorf("%s: TrustedProxiesSet = %t, want %t", when, got.set, wantSet)
	}
	if strings.Join(got.list, "|") != strings.Join(wantList, "|") {
		t.Errorf("%s: TrustedProxies = %q, want %q", when, got.list, wantList)
	}
	switch {
	case !wantSet:
		if !got.usedDefault || strings.Join(got.nets, "|") != "127.0.0.1/32|::1/128" {
			t.Errorf("%s: not set must trust only a proxy on this host; usedDefault=%t nets=%q", when, got.usedDefault, got.nets)
		}
	case len(wantList) == 0:
		if got.usedDefault || len(got.nets) != 0 {
			t.Errorf("%s: an explicit empty list must trust no proxy; usedDefault=%t nets=%q", when, got.usedDefault, got.nets)
		}
	default:
		if got.usedDefault || len(got.nets) != len(wantList) {
			t.Errorf("%s: a list must trust exactly its entries; usedDefault=%t nets=%q", when, got.usedDefault, got.nets)
		}
	}
}

// trustedProxiesRewriteCases are the three states server.trusted_proxies
// can have in settings.yaml. wantInFile is what the rewritten file must
// hold for the key ("" when the key must be absent).
var trustedProxiesRewriteCases = map[string]struct {
	server     string // the server block's trusted_proxies lines
	wantSet    bool
	wantList   []string
	wantInFile string
}{
	"explicit empty": {server: "  trusted_proxies: []\n", wantSet: true, wantInFile: "trusted_proxies: []"},
	"list":           {server: "  trusted_proxies:\n    - 10.0.0.0/8\n", wantSet: true, wantList: []string{"10.0.0.0/8"}, wantInFile: "trusted_proxies:"},
	"not set":        {server: "", wantSet: false},
}

// server.trusted_proxies keeps its state when the hub rewrites
// settings.yaml through the settings struct (LoadModifySaveVersionedSettings,
// as the system runtime and image registry PUTs do): an explicit [] stays
// set and trusts no proxy, a list stays the list, and an absent key stays
// not set (the loopback default).
func TestTrustedProxies_StateSurvivesSettingsRewrite(t *testing.T) {
	for name, tc := range trustedProxiesRewriteCases {
		t.Run(name, func(t *testing.T) {
			clearTrustedProxiesEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			content := "schema_version: \"1\"\nserver:\n  log_level: info\n" + tc.server
			path := filepath.Join(home, "settings.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			checkTrustedProxiesState(t, "before the rewrite", loadTrustedProxiesState(t, home), tc.wantSet, tc.wantList)

			// A change to an unrelated key, saved through the struct.
			if err := LoadModifySaveVersionedSettings(home, func(vs *VersionedSettings) error {
				vs.ImageRegistry = "rewrite.example"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "image_registry: rewrite.example") {
				t.Fatalf("the file was not rewritten:\n%s", data)
			}
			if tc.wantInFile == "" {
				if strings.Contains(string(data), "trusted_proxies") {
					t.Errorf("an absent key was written back:\n%s", data)
				}
			} else if !strings.Contains(string(data), tc.wantInFile) {
				t.Errorf("the rewritten file lacks %q:\n%s", tc.wantInFile, data)
			}
			checkTrustedProxiesState(t, "after the rewrite", loadTrustedProxiesState(t, home), tc.wantSet, tc.wantList)
		})
	}
}

// Migrating a legacy settings file with a server.yaml keeps
// server.trusted_proxies' state: trustedProxies: [] becomes
// trusted_proxies: [] (set, trusting no proxy), not an absent key.
func TestTrustedProxies_StateSurvivesLegacyMigration(t *testing.T) {
	for name, tc := range map[string]struct {
		serverYAML string
		wantSet    bool
		wantList   []string
		wantInFile string
	}{
		"explicit empty": {serverYAML: "trustedProxies: []\n", wantSet: true, wantInFile: "trusted_proxies: []"},
		"list":           {serverYAML: "trustedProxies:\n  - 10.0.0.0/8\n", wantSet: true, wantList: []string{"10.0.0.0/8"}, wantInFile: "trusted_proxies:"},
		"not set":        {serverYAML: "", wantSet: false},
	} {
		t.Run(name, func(t *testing.T) {
			clearTrustedProxiesEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			legacy := "active_profile: local\ndefault_template: gemini\nharnesses:\n  gemini:\n    image: example.com/gemini:latest\n    user: scion\nruntimes:\n  docker: {}\nprofiles:\n  local:\n    runtime: docker\n"
			if err := os.WriteFile(filepath.Join(home, "settings.yaml"), []byte(legacy), 0o600); err != nil {
				t.Fatal(err)
			}
			serverYAML := "hub:\n  port: 9810\nlogLevel: info\n" + tc.serverYAML
			if err := os.WriteFile(filepath.Join(home, "server.yaml"), []byte(serverYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := MigrateSettingsFile(home, false)
			if err != nil {
				t.Fatal(err)
			}
			if !result.ServerMigrated {
				t.Fatalf("server.yaml was not migrated: %+v", result)
			}
			data, err := os.ReadFile(filepath.Join(home, "settings.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantInFile == "" {
				if strings.Contains(string(data), "trusted_proxies") {
					t.Errorf("an absent key was written:\n%s", data)
				}
			} else if !strings.Contains(string(data), tc.wantInFile) {
				t.Errorf("the migrated file lacks %q:\n%s", tc.wantInFile, data)
			}
			checkTrustedProxiesState(t, "after the migration", loadTrustedProxiesState(t, home), tc.wantSet, tc.wantList)
		})
	}
}

// The converters carry the set state: a nil V1 list is not set, a non-nil
// empty one is set, and a set but empty GlobalConfig list converts to a
// non-nil empty V1 list (so it is written as trusted_proxies: []).
func TestTrustedProxies_ConvertersCarrySetState(t *testing.T) {
	if gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{}); gc.TrustedProxiesSet || gc.TrustedProxies != nil {
		t.Errorf("nil V1 list: set=%t list=%q, want not set", gc.TrustedProxiesSet, gc.TrustedProxies)
	}
	empty := []string{}
	if gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{TrustedProxies: &empty}); !gc.TrustedProxiesSet || len(gc.TrustedProxies) != 0 {
		t.Errorf("empty V1 list: set=%t list=%q, want set and empty", gc.TrustedProxiesSet, gc.TrustedProxies)
	}
	if v1 := ConvertGlobalToV1ServerConfig(&GlobalConfig{}); v1.TrustedProxies != nil {
		t.Errorf("not set: V1 list = %q, want nil", *v1.TrustedProxies)
	}
	v1 := ConvertGlobalToV1ServerConfig(&GlobalConfig{TrustedProxiesSet: true})
	if v1.TrustedProxies == nil || len(*v1.TrustedProxies) != 0 {
		t.Errorf("set and empty: V1 list = %v, want a non-nil empty list", v1.TrustedProxies)
	}
	v1 = ConvertGlobalToV1ServerConfig(&GlobalConfig{TrustedProxies: []string{"192.0.2.7"}, TrustedProxiesSet: true})
	if v1.TrustedProxies == nil || strings.Join(*v1.TrustedProxies, "|") != "192.0.2.7" {
		t.Errorf("list: V1 list = %v, want [192.0.2.7]", v1.TrustedProxies)
	}
}
