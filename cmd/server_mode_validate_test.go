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

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
)

// An unknown server.mode, from settings.yaml or SCION_SERVER_MODE, stops
// server startup instead of silently meaning workstation.
func TestLoadAndReconcileConfig_RejectsUnknownServerMode(t *testing.T) {
	for _, tc := range []struct {
		name, file, env string
	}{
		{"settings.yaml typo", "schema_version: \"1\"\nserver:\n  mode: Hosted\n", ""},
		{"env typo", "schema_version: \"1\"\n", "prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.env != "" {
				t.Setenv("SCION_SERVER_MODE", tc.env)
			}
			_, err := loadAndReconcileConfig(serverStartCmd)
			if err == nil || !strings.Contains(err.Error(), "invalid server.mode") {
				t.Fatalf("loadAndReconcileConfig err = %v, want an invalid server.mode error", err)
			}
		})
	}
}

// writeServerSettings points HOME at a temp dir whose global settings.yaml
// holds content.
func writeServerSettings(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --enable-debug-endpoints is refused when the server runs in hosted mode,
// and accepted in workstation mode.
func TestLoadAndReconcileConfig_RefusesDebugEndpointsInHostedMode(t *testing.T) {
	const refusal = "--enable-debug-endpoints is not allowed in hosted mode"
	for _, tc := range []struct {
		mode       string
		wantRefuse bool
	}{
		{mode: "hosted", wantRefuse: true},
		{mode: "workstation", wantRefuse: false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			savedEndpoints := enableDebugEndpoints
			t.Cleanup(func() {
				resetServerFlags()
				enableDebugEndpoints = savedEndpoints
			})
			resetServerFlags()
			enableDebugEndpoints = true
			t.Setenv("SCION_SERVER_MODE", "")
			writeServerSettings(t, "schema_version: \"1\"\nserver:\n  mode: "+tc.mode+"\n")

			_, err := loadAndReconcileConfig(serverStartCmd)
			refused := err != nil && strings.Contains(err.Error(), refusal)
			if refused != tc.wantRefuse {
				t.Fatalf("server.mode %s: loadAndReconcileConfig err = %v, want refusal %v", tc.mode, err, tc.wantRefuse)
			}
		})
	}
}

// The daemon parent applies the same check before starting the child, so
// the reason is reported directly.
func TestResolveDaemonServerMode_DebugEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		args       []string
		wantErr    bool
		wantHosted bool
	}{
		{name: "settings hosted", mode: "hosted", wantErr: true, wantHosted: true},
		{name: "settings workstation", mode: "workstation"},
		{name: "explicit --hosted=false wins over settings", mode: "hosted", args: []string{"--hosted=false"}},
		{name: "explicit --hosted", mode: "workstation", args: []string{"--hosted"}, wantErr: true, wantHosted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savedHosted, savedEndpoints := hostedMode, enableDebugEndpoints
			t.Cleanup(func() { hostedMode, enableDebugEndpoints = savedHosted, savedEndpoints })
			hostedMode, enableDebugEndpoints = false, true
			t.Setenv("SCION_SERVER_MODE", "")
			writeServerSettings(t, "server:\n  mode: "+tc.mode+"\n")

			c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
			c.Flags().BoolVar(&hostedMode, "hosted", false, "")
			c.Flags().Bool("production", false, "")
			c.SetArgs(tc.args)
			if err := c.Execute(); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}

			err := resolveDaemonServerMode(c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveDaemonServerMode err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--enable-debug-endpoints is not allowed in hosted mode") {
				t.Fatalf("resolveDaemonServerMode err = %v, want the hosted-mode refusal", err)
			}
			if hostedMode != tc.wantHosted {
				t.Errorf("hostedMode = %v, want %v", hostedMode, tc.wantHosted)
			}
		})
	}
}

func TestValidateDebugEndpoints(t *testing.T) {
	for _, tc := range []struct {
		hosted, enabled, wantErr bool
	}{
		{hosted: false, enabled: false},
		{hosted: false, enabled: true},
		{hosted: true, enabled: false},
		{hosted: true, enabled: true, wantErr: true},
	} {
		err := validateDebugEndpoints(tc.hosted, tc.enabled)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateDebugEndpoints(hosted=%v, enabled=%v) err = %v, wantErr %v", tc.hosted, tc.enabled, err, tc.wantErr)
		}
	}
}

// The daemon path reads server.mode from settings.yaml through
// LoadServerMode; the same validation applies to its result.
func TestLoadServerMode_TypoFailsValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("server:\n  mode: prod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateServerMode(config.LoadServerMode()); err == nil {
		t.Error("a server.mode typo must fail validation on the daemon path")
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("server:\n  mode: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateServerMode(config.LoadServerMode()); err != nil {
		t.Errorf("legacy production must stay valid: %v", err)
	}
}

// In workstation mode, --host moves only the hub and web listeners through
// the real config path; the co-located broker stays on loopback unless
// server.broker.host is set explicitly (ptone/scion#3990).
func TestLoadAndReconcileConfig_WorkstationHostFlagKeepsBrokerLoopback(t *testing.T) {
	for _, tc := range []struct {
		name, settings, wantBroker string
	}{
		{
			name:       "broker host unset",
			settings:   "schema_version: \"1\"\nserver:\n  mode: workstation\n",
			wantBroker: "127.0.0.1",
		},
		{
			name:       "broker host set explicitly",
			settings:   "schema_version: \"1\"\nserver:\n  mode: workstation\n  broker:\n    host: 192.168.1.10\n",
			wantBroker: "192.168.1.10",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostFlag := serverStartCmd.Flags().Lookup("host")
			t.Cleanup(func() {
				hostFlag.Changed = false
				resetServerFlags()
			})
			resetServerFlags()
			t.Setenv("SCION_SERVER_MODE", "")
			t.Setenv("SCION_SERVER_HUB_HOST", "")
			t.Setenv("SCION_SERVER_BROKER_HOST", "")
			t.Setenv("SCION_SERVER_RUNTIMEBROKER_HOST", "")
			writeServerSettings(t, tc.settings)
			if err := serverStartCmd.Flags().Set("host", "0.0.0.0"); err != nil {
				t.Fatal(err)
			}

			cfg, err := loadAndReconcileConfig(serverStartCmd)
			if err != nil {
				t.Fatalf("loadAndReconcileConfig: %v", err)
			}
			if cfg.Hub.Host != "0.0.0.0" {
				t.Errorf("hub host = %q, want 0.0.0.0", cfg.Hub.Host)
			}
			if cfg.RuntimeBroker.Host != tc.wantBroker {
				t.Errorf("broker host = %q, want %s", cfg.RuntimeBroker.Host, tc.wantBroker)
			}
		})
	}
}

func TestValidateHostedDevAuth(t *testing.T) {
	for _, tc := range []struct {
		hosted, devAuth, wantErr bool
	}{
		{hosted: false, devAuth: false},
		{hosted: false, devAuth: true},
		{hosted: true, devAuth: false},
		{hosted: true, devAuth: true, wantErr: true},
	} {
		err := validateHostedDevAuth(tc.hosted, tc.devAuth)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateHostedDevAuth(hosted=%v, devAuth=%v) err = %v, wantErr %v", tc.hosted, tc.devAuth, err, tc.wantErr)
		}
	}
}

// Dev auth stops a hosted server at startup, whether it comes from the
// settings file, the environment or the --dev-auth flag. Workstation mode,
// where dev auth is the default, is unchanged.
func TestLoadAndReconcileConfig_RefusesDevAuthInHostedMode(t *testing.T) {
	const refusal = "dev auth is not allowed in hosted mode"
	for _, tc := range []struct {
		name, settings, env string
		args                []string
		wantRefuse          bool
	}{
		{name: "hosted, dev_mode in settings", settings: "  mode: hosted\n  auth:\n    dev_mode: true\n", wantRefuse: true},
		{name: "hosted, dev mode from env", settings: "  mode: hosted\n", env: "true", wantRefuse: true},
		{name: "hosted, --dev-auth flag", settings: "  mode: hosted\n", args: []string{"--dev-auth"}, wantRefuse: true},
		{name: "hosted, dev auth off", settings: "  mode: hosted\n"},
		{name: "hosted, --dev-auth=false overrides settings", settings: "  mode: hosted\n  auth:\n    dev_mode: true\n", args: []string{"--dev-auth=false"}},
		{name: "workstation default", settings: "  mode: workstation\n"},
		{name: "workstation, --dev-auth flag", settings: "  mode: workstation\n", args: []string{"--dev-auth"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(resetServerFlags)
			resetServerFlags()
			t.Setenv("SCION_SERVER_MODE", "")
			t.Setenv("SCION_SERVER_AUTH_DEVMODE", tc.env)
			writeServerSettings(t, "schema_version: \"1\"\nserver:\n"+tc.settings)
			// A fresh command, so flag Changed state does not leak between
			// cases the way it would on the shared serverStartCmd.
			c := &cobra.Command{Use: "start"}
			c.Flags().BoolVar(&hostedMode, "hosted", false, "")
			c.Flags().Bool("production", false, "")
			c.Flags().BoolVar(&enableDevAuth, "dev-auth", false, "")
			if err := c.ParseFlags(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}

			cfg, err := loadAndReconcileConfig(c)
			refused := err != nil && strings.Contains(err.Error(), refusal)
			if refused != tc.wantRefuse {
				t.Fatalf("loadAndReconcileConfig err = %v, want refusal %v", err, tc.wantRefuse)
			}
			if !tc.wantRefuse && err != nil {
				t.Fatalf("loadAndReconcileConfig err = %v", err)
			}
			if tc.wantRefuse {
				return
			}
			if cfg == nil {
				t.Fatal("loadAndReconcileConfig returned a nil config without an error")
			}
			if strings.HasPrefix(tc.name, "workstation") && !cfg.Auth.Enabled {
				t.Errorf("workstation: cfg.Auth.Enabled = false, want dev auth on")
			}
		})
	}
}

// The daemon parent refuses an explicit --dev-auth in hosted mode itself,
// so the reason is reported directly instead of as a failed child start.
func TestResolveDaemonServerMode_DevAuth(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		args       []string
		wantErr    bool
	}{
		{name: "hosted, --dev-auth", mode: "hosted", args: []string{"--dev-auth"}, wantErr: true},
		{name: "hosted, --dev-auth=false", mode: "hosted", args: []string{"--dev-auth=false"}},
		{name: "hosted, flag unset", mode: "hosted"},
		{name: "workstation, --dev-auth", mode: "workstation", args: []string{"--dev-auth"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savedHosted, savedDevAuth, savedEndpoints := hostedMode, enableDevAuth, enableDebugEndpoints
			t.Cleanup(func() { hostedMode, enableDevAuth, enableDebugEndpoints = savedHosted, savedDevAuth, savedEndpoints })
			hostedMode, enableDevAuth, enableDebugEndpoints = false, false, false
			t.Setenv("SCION_SERVER_MODE", "")
			writeServerSettings(t, "server:\n  mode: "+tc.mode+"\n")

			c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
			c.Flags().BoolVar(&hostedMode, "hosted", false, "")
			c.Flags().Bool("production", false, "")
			c.Flags().BoolVar(&enableDevAuth, "dev-auth", false, "")
			c.SetArgs(tc.args)
			if err := c.Execute(); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}

			err := resolveDaemonServerMode(c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveDaemonServerMode err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "dev auth is not allowed in hosted mode") {
				t.Fatalf("resolveDaemonServerMode err = %v, want the hosted dev auth refusal", err)
			}
		})
	}
}
