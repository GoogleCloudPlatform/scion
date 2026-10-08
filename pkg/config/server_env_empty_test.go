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

// serverEnvPathResult is the list field (authorized_domains) and the scalar
// field (hub host) that one SCION_SERVER_/SCION_SEED_ env path produced.
type serverEnvPathResult struct {
	domains []string
	host    string
}

// koanfStringList reads key as a list, splitting a comma-separated string
// (the env-only loaders keep the raw string).
func koanfStringList(k *koanf.Koanf, key string) []string {
	if !k.Exists(key) {
		return nil
	}
	if s, ok := k.Get(key).(string); ok {
		return []string{s}
	}
	return k.Strings(key)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const serverEnvEmptySettingsYAML = `schema_version: "1"
server:
  hub:
    host: "10.1.2.3"
  auth:
    authorized_domains:
      - configured-a.example
      - configured-b.example
`

// configuredKoanf is the lower layer the env-only loaders are merged over.
func configuredKoanf() *koanf.Koanf {
	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.host":                "10.1.2.3",
		"server.auth.authorized_domains": []interface{}{"configured-a.example", "configured-b.example"},
	}, "."), nil)
	return k
}

// TestServerAndSeedEnv_EmptyValueIsUnset checks every SCION_SERVER_ and
// SCION_SEED_ env path: an exported but empty variable leaves the configured
// value intact, a non-empty value (including a comma-separated list) still
// overrides it, and an unset variable behaves as before (ptone/scion#3809).
func TestServerAndSeedEnv_EmptyValueIsUnset(t *testing.T) {
	configured := serverEnvPathResult{
		domains: []string{"configured-a.example", "configured-b.example"},
		host:    "10.1.2.3",
	}

	paths := []struct {
		name       string
		domainsEnv string
		hostEnv    string
		// configured is the result with no env override; defaults to the
		// shared configured value above.
		configured *serverEnvPathResult
		load       func(t *testing.T) serverEnvPathResult
	}{
		{
			// settings.yaml server section, then applyEnvOverrides.
			name:       "LoadGlobalConfig/settings.yaml",
			domainsEnv: "SCION_SERVER_AUTH_AUTHORIZEDDOMAINS",
			hostEnv:    "SCION_SERVER_HUB_HOST",
			load: func(t *testing.T) serverEnvPathResult {
				home := t.TempDir()
				t.Setenv("HOME", home)
				writeTestFile(t, filepath.Join(home, ".scion", "settings.yaml"), serverEnvEmptySettingsYAML)
				gc, err := LoadGlobalConfig("")
				if err != nil {
					t.Fatalf("LoadGlobalConfig: %v", err)
				}
				return serverEnvPathResult{gc.Auth.AuthorizedDomains, gc.Hub.Host}
			},
		},
		{
			// Legacy server.yaml path (loadGlobalConfigLegacy).
			name:       "LoadGlobalConfig/server.yaml",
			domainsEnv: "SCION_SERVER_AUTH_AUTHORIZEDDOMAINS",
			hostEnv:    "SCION_SERVER_HUB_HOST",
			load: func(t *testing.T) serverEnvPathResult {
				t.Setenv("HOME", t.TempDir())
				dir := t.TempDir()
				writeTestFile(t, filepath.Join(dir, "server.yaml"), `
hub:
  host: "10.1.2.3"
auth:
  authorizedDomains:
    - configured-a.example
    - configured-b.example
`)
				gc, err := LoadGlobalConfig(dir)
				if err != nil {
					t.Fatalf("LoadGlobalConfig: %v", err)
				}
				return serverEnvPathResult{gc.Auth.AuthorizedDomains, gc.Hub.Host}
			},
		},
		{
			name:       "LoadEnvKoanf",
			domainsEnv: "SCION_SERVER_AUTH_AUTHORIZEDDOMAINS",
			hostEnv:    "SCION_SERVER_HUB_HOST",
			load: func(t *testing.T) serverEnvPathResult {
				k := configuredKoanf()
				_ = k.Merge(LoadEnvKoanf())
				return serverEnvPathResult{koanfStringList(k, "server.auth.authorized_domains"), k.String("server.hub.host")}
			},
		},
		{
			name:       "LoadSeedEnvKoanf",
			domainsEnv: "SCION_SEED_SERVER_AUTH_AUTHORIZEDDOMAINS",
			hostEnv:    "SCION_SEED_SERVER_HUB_HOST",
			load: func(t *testing.T) serverEnvPathResult {
				k := configuredKoanf()
				_ = k.Merge(LoadSeedEnvKoanf())
				return serverEnvPathResult{koanfStringList(k, "server.auth.authorized_domains"), k.String("server.hub.host")}
			},
		},
		{
			// SCION_SERVER_ layer of the bootstrap merge, over settings.yaml.
			name:       "LoadBootstrapKoanf/server",
			domainsEnv: "SCION_SERVER_AUTH_AUTHORIZEDDOMAINS",
			hostEnv:    "SCION_SERVER_HUB_HOST",
			load: func(t *testing.T) serverEnvPathResult {
				home := t.TempDir()
				t.Setenv("HOME", home)
				writeTestFile(t, filepath.Join(home, ".scion", "settings.yaml"), serverEnvEmptySettingsYAML)
				k := LoadBootstrapKoanf()
				return serverEnvPathResult{k.Strings("server.auth.authorized_domains"), k.String("server.hub.host")}
			},
		},
		{
			// SCION_SEED_ layer of the bootstrap merge, over the coded
			// defaults (no settings.yaml, so the seed layer is visible).
			name:       "LoadBootstrapKoanf/seed",
			domainsEnv: "SCION_SEED_SERVER_AUTH_AUTHORIZEDDOMAINS",
			hostEnv:    "SCION_SEED_SERVER_HUB_HOST",
			configured: &serverEnvPathResult{domains: nil, host: DefaultGlobalConfig().Hub.Host},
			load: func(t *testing.T) serverEnvPathResult {
				t.Setenv("HOME", t.TempDir())
				k := LoadBootstrapKoanf()
				return serverEnvPathResult{koanfStringList(k, "server.auth.authorized_domains"), k.String("server.hub.host")}
			},
		},
	}

	clearPathEnv := func(t *testing.T) {
		t.Helper()
		for _, p := range paths {
			for _, name := range []string{p.domainsEnv, p.hostEnv} {
				t.Setenv(name, "")
				_ = os.Unsetenv(name)
			}
		}
	}

	for _, p := range paths {
		want := configured
		if p.configured != nil {
			want = *p.configured
		}
		check := func(t *testing.T, got, want serverEnvPathResult) {
			t.Helper()
			if len(got.domains) != 0 || len(want.domains) != 0 {
				if !reflect.DeepEqual(got.domains, want.domains) {
					t.Errorf("authorized_domains = %q, want %q", got.domains, want.domains)
				}
			}
			if got.host != want.host {
				t.Errorf("hub host = %q, want %q", got.host, want.host)
			}
		}

		t.Run(p.name+"/unset", func(t *testing.T) {
			clearPathEnv(t)
			check(t, p.load(t), want)
		})
		t.Run(p.name+"/empty", func(t *testing.T) {
			clearPathEnv(t)
			t.Setenv(p.domainsEnv, "")
			t.Setenv(p.hostEnv, "")
			check(t, p.load(t), want)
		})
		t.Run(p.name+"/non-empty", func(t *testing.T) {
			clearPathEnv(t)
			t.Setenv(p.domainsEnv, "env-a.example,env-b.example")
			t.Setenv(p.hostEnv, "10.9.9.9")
			got := p.load(t)
			if len(got.domains) == 1 {
				// The env-only loaders keep the raw comma-separated string.
				got.domains = parseCommaSeparatedList(got.domains[0])
			}
			check(t, got, serverEnvPathResult{
				domains: []string{"env-a.example", "env-b.example"},
				host:    "10.9.9.9",
			})
		})
	}
}
