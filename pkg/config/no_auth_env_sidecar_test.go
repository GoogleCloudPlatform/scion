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
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/harnesses"
)

const (
	noAuthTestConfig  = "harness: generic\nimage: scion-base:test\nno_auth:\n  behavior: drop-to-shell\n  message: sign in\n"
	noAuthTestSidecar = "env:\n  DBUS_SESSION_BUS_ADDRESS: unix:path=/home/scion/.scion/harness/no-session-bus\n"
	noAuthTestBus     = "unix:path=/home/scion/.scion/harness/no-session-bus"
)

func writeNoAuthEnvTestDir(t *testing.T, configYAML, sidecar string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if sidecar != "" {
		if err := os.WriteFile(filepath.Join(dir, NoAuthEnvSidecarFile), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func noAuthEnvOf(entry HarnessConfigEntry) map[string]string {
	if entry.NoAuthConfig == nil {
		return nil
	}
	return entry.NoAuthConfig.Env
}

// TestLoadHarnessConfig_NoAuthEnvSidecar: both loaders fill
// HarnessNoAuthConfig.Env from no-auth-env.yaml, and a bundle without the file
// has no no-auth env.
func TestLoadHarnessConfig_NoAuthEnvSidecar(t *testing.T) {
	want := map[string]string{"DBUS_SESSION_BUS_ADDRESS": noAuthTestBus}

	t.Run("LoadHarnessConfigDir", func(t *testing.T) {
		hc, err := LoadHarnessConfigDir(writeNoAuthEnvTestDir(t, noAuthTestConfig, noAuthTestSidecar))
		if err != nil {
			t.Fatal(err)
		}
		if got := noAuthEnvOf(hc.Config); !maps.Equal(got, want) {
			t.Fatalf("no-auth env = %v, want %v", got, want)
		}
		if hc.Config.NoAuthConfig.Behavior != "drop-to-shell" || hc.Config.NoAuthConfig.Message != "sign in" {
			t.Fatalf("no_auth block not kept: %+v", hc.Config.NoAuthConfig)
		}
	})

	t.Run("LoadHarnessConfigFromFiles", func(t *testing.T) {
		fsys := fstest.MapFS{
			"config.yaml":        {Data: []byte(noAuthTestConfig)},
			NoAuthEnvSidecarFile: {Data: []byte(noAuthTestSidecar)},
		}
		hc, err := LoadHarnessConfigFromFiles("hc", "/hub/hc", func(name string) ([]byte, error) {
			return fs.ReadFile(fsys, name)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := noAuthEnvOf(hc.Config); !maps.Equal(got, want) {
			t.Fatalf("no-auth env = %v, want %v", got, want)
		}
	})

	t.Run("missing sidecar adds no env", func(t *testing.T) {
		hc, err := LoadHarnessConfigDir(writeNoAuthEnvTestDir(t, noAuthTestConfig, ""))
		if err != nil {
			t.Fatal(err)
		}
		if got := noAuthEnvOf(hc.Config); got != nil {
			t.Fatalf("no-auth env = %v without a sidecar, want none", got)
		}
	})
}

// TestLoadHarnessConfigDir_InvalidNoAuthEnvSidecar: a sidecar that does not
// parse, or one in a bundle without a no_auth block, is ignored; the load
// never fails because of it.
func TestLoadHarnessConfigDir_InvalidNoAuthEnvSidecar(t *testing.T) {
	cases := []struct {
		name, configYAML, sidecar string
	}{
		{"does not parse", noAuthTestConfig, "env: [unclosed\n"},
		{"env is not a map", noAuthTestConfig, "env:\n  - DBUS_SESSION_BUS_ADDRESS\n"},
		{"no no_auth block", "harness: generic\nimage: scion-base:test\n", noAuthTestSidecar},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hc, err := LoadHarnessConfigDir(writeNoAuthEnvTestDir(t, tc.configYAML, tc.sidecar))
			if err != nil {
				t.Fatalf("load failed: %v", err)
			}
			if got := noAuthEnvOf(hc.Config); got != nil {
				t.Fatalf("no-auth env = %v, want none", got)
			}
		})
	}
}

// TestNoAuthEnv_NotAcceptedInConfigYAML: config.yaml's no_auth block stays
// what older binaries accept; the env lives only in the sidecar.
func TestNoAuthEnv_NotAcceptedInConfigYAML(t *testing.T) {
	doc := noAuthTestConfig + "  env:\n    DBUS_SESSION_BUS_ADDRESS: " + noAuthTestBus + "\n"
	verrs, err := ValidateHarnessConfig([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(verrs) == 0 {
		t.Fatal("config.yaml with no_auth.env was accepted")
	}
}

// TestSeedHarnessConfigFromDir_NoAuthEnvSidecarAtRoot: seeding keeps the
// sidecar at the bundle root (not in the agent home template) and a
// non-force reseed refreshes a stale copy, as for agent-plugins.yaml.
func TestSeedHarnessConfigFromDir_NoAuthEnvSidecarAtRoot(t *testing.T) {
	target := filepath.Join(t.TempDir(), "copilot")
	if err := SeedHarnessConfigFromDir(target, harnesses.FS, "copilot", false); err != nil {
		t.Fatal(err)
	}
	bundled, err := fs.ReadFile(harnesses.FS, "copilot/"+NoAuthEnvSidecarFile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, NoAuthEnvSidecarFile))
	if err != nil {
		t.Fatalf("sidecar not at the bundle root: %v", err)
	}
	if string(got) != string(bundled) {
		t.Fatal("seeded sidecar differs from the bundled one")
	}
	if _, err := os.Stat(filepath.Join(target, "home", ".copilot", NoAuthEnvSidecarFile)); !os.IsNotExist(err) {
		t.Fatalf("sidecar also seeded under home/.copilot: stat err = %v", err)
	}

	if err := os.WriteFile(filepath.Join(target, NoAuthEnvSidecarFile), []byte("env:\n  STALE: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SeedHarnessConfigFromDir(target, harnesses.FS, "copilot", false); err != nil {
		t.Fatal(err)
	}
	if got, err = os.ReadFile(filepath.Join(target, NoAuthEnvSidecarFile)); err != nil || string(got) != string(bundled) {
		t.Fatalf("non-force reseed did not refresh a stale sidecar (err=%v)", err)
	}
}

// TestCopilotConfig_NoAuthEnvScoped (HC6): the shipped Copilot bundle sets
// DBUS_SESSION_BUS_ADDRESS only in its no-auth env, never in env_template
// (which applies to every start) or in config.yaml, and runs no automatic
// login in no-auth mode.
func TestCopilotConfig_NoAuthEnvScoped(t *testing.T) {
	const key = "DBUS_SESSION_BUS_ADDRESS"
	raw, err := fs.ReadFile(harnesses.FS, "copilot/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Errorf("copilot config.yaml mentions %s; it belongs only in %s", key, NoAuthEnvSidecarFile)
	}
	hc, err := LoadHarnessConfigFromFiles("copilot", "copilot", func(name string) ([]byte, error) {
		return fs.ReadFile(harnesses.FS, "copilot/"+name)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hc.Config.EnvTemplate[key]; ok {
		t.Errorf("env_template sets %s for every start", key)
	}
	if hc.Config.NoAuthConfig == nil {
		t.Fatal("copilot has no no_auth block")
	}
	if got, want := hc.Config.NoAuthConfig.Env, map[string]string{key: noAuthTestBus}; !maps.Equal(got, want) {
		t.Errorf("no-auth env = %v, want %v", got, want)
	}
	if hc.Config.NoAuthConfig.Command != "" {
		t.Errorf("no_auth.command = %q, want none (no automatic login)", hc.Config.NoAuthConfig.Command)
	}
}
