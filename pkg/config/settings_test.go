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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestLoadSettings(t *testing.T) {
	// Create temporary directories for global and grove settings
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Test defaults
	s, err := LoadSettings(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}
	if s.ActiveProfile != "local" {
		t.Errorf("expected active profile 'local', got '%s'", s.ActiveProfile)
	}
	if s.DefaultTemplate != "default" {
		t.Errorf("expected default template 'default', got '%s'", s.DefaultTemplate)
	}

	// 2. Test Global overrides
	globalSettings := `{
		"active_profile": "prod",
		"default_template": "claude",
		"runtimes": {
			"kubernetes": { "namespace": "scion-global" }
		},
		"profiles": {
			"prod": { "runtime": "kubernetes", "tmux": false }
		}
	}`
	globalScionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.json"), []byte(globalSettings), 0644); err != nil {
		t.Fatal(err)
	}

	s, err = LoadSettings(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}
	if s.ActiveProfile != "prod" {
		t.Errorf("expected global override active_profile 'prod', got '%s'", s.ActiveProfile)
	}
	if s.DefaultTemplate != "claude" {
		t.Errorf("expected global override template 'claude', got '%s'", s.DefaultTemplate)
	}
	if s.Runtimes["kubernetes"].Namespace != "scion-global" {
		t.Errorf("expected global override runtime namespace 'scion-global', got '%s'", s.Runtimes["kubernetes"].Namespace)
	}

	// 3. Test Project overrides
	groveSettings := `{
		"active_profile": "local-dev",
		"profiles": {
			"local-dev": { "runtime": "local", "tmux": true }
		}
	}`
	if err := os.WriteFile(filepath.Join(groveScionDir, "settings.json"), []byte(groveSettings), 0644); err != nil {
		t.Fatal(err)
	}

	s, err = LoadSettings(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}
	if s.ActiveProfile != "local-dev" {
		t.Errorf("expected grove override active_profile 'local-dev', got '%s'", s.ActiveProfile)
	}
	// Template should still be claude from global
	if s.DefaultTemplate != "claude" {
		t.Errorf("expected inherited global template 'claude', got '%s'", s.DefaultTemplate)
	}
}

func TestUpdateSetting(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Update local setting
	err := UpdateSetting(groveScionDir, "active_profile", "kubernetes", false)
	if err != nil {
		t.Fatalf("UpdateSetting failed: %v", err)
	}

	// Verify file content (now writes YAML)
	content, err := os.ReadFile(filepath.Join(groveScionDir, "settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "active_profile: kubernetes") {
		t.Errorf("expected file to contain active_profile: kubernetes, got %s", string(content))
	}

	// Update default_template
	err = UpdateSetting(groveScionDir, "default_template", "my-template", false)
	if err != nil {
		t.Fatalf("UpdateSetting default_template failed: %v", err)
	}
	content, _ = os.ReadFile(filepath.Join(groveScionDir, "settings.yaml"))
	if !strings.Contains(string(content), "default_template: my-template") {
		t.Errorf("expected file to contain default_template: my-template, got %s", string(content))
	}
}

func TestResolve(t *testing.T) {
	s := &Settings{
		ActiveProfile: "local",
		Runtimes: map[string]RuntimeConfig{
			"docker": {Host: "unix:///var/run/docker.sock"},
		},
		Harnesses: map[string]HarnessConfig{
			"gemini": {Image: "gemini:latest", User: "root"},
		},
		Profiles: map[string]ProfileConfig{
			"local": {
				Runtime: "docker",
				HarnessOverrides: map[string]HarnessOverride{
					"gemini": {Image: "gemini:dev"},
				},
			},
		},
	}

	runtimeCfg, name, err := s.ResolveRuntime("")
	if err != nil {
		t.Fatal(err)
	}
	if name != "docker" {
		t.Errorf("expected runtime name docker, got %s", name)
	}
	if runtimeCfg.Host != "unix:///var/run/docker.sock" {
		t.Errorf("expected host unix:///var/run/docker.sock, got %s", runtimeCfg.Host)
	}

	h, err := s.ResolveHarness("", "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if h.Image != "gemini:dev" {
		t.Errorf("expected image gemini:dev, got %s", h.Image)
	}
}

func TestEnvMerging(t *testing.T) {
	s := &Settings{
		Harnesses: map[string]HarnessConfig{
			"gemini": {
				Env: map[string]string{
					"H1": "V1",
					"H2": "V2",
				},
			},
		},
		Profiles: map[string]ProfileConfig{
			"dev": {
				Env: map[string]string{
					"H2": "P2", // Overrides harness
					"P1": "PV1",
				},
				HarnessOverrides: map[string]HarnessOverride{
					"gemini": {
						Env: map[string]string{
							"P1": "PH1", // Overrides profile
							"O1": "OV1",
						},
					},
				},
			},
		},
	}

	h, err := s.ResolveHarness("dev", "gemini")
	if err != nil {
		t.Fatal(err)
	}

	expected := map[string]string{
		"H1": "V1",  // From harness base
		"H2": "P2",  // Harness base, overridden by profile root
		"P1": "PH1", // Profile root, overridden by harness override
		"O1": "OV1", // From harness override
	}

	if len(h.Env) != len(expected) {
		t.Errorf("expected %d env vars, got %d", len(expected), len(h.Env))
	}

	for k, v := range expected {
		if h.Env[k] != v {
			t.Errorf("expected %s=%s, got %s", k, v, h.Env[k])
		}
	}
}

func TestMergeSettingsEnv(t *testing.T) {
	base := &Settings{
		Harnesses: map[string]HarnessConfig{
			"gemini": {
				Env: map[string]string{"A": "1", "B": "2"},
			},
		},
	}
	overrideJSON := `{
		"harnesses": {
			"gemini": {
				"env": {"B": "3", "C": "4"}
			}
		}
	}`

	err := MergeSettings(base, []byte(overrideJSON))
	if err != nil {
		t.Fatal(err)
	}

	env := base.Harnesses["gemini"].Env
	if env["A"] != "1" || env["B"] != "3" || env["C"] != "4" {
		t.Errorf("unexpected env after merge: %v", env)
	}
}

func TestMergeSettingsAuthSelectedType(t *testing.T) {
	base := &Settings{
		Harnesses: map[string]HarnessConfig{
			"gemini": {
				AuthSelectedType: "api-key",
			},
		},
	}

	overrideJSON := `{
		"harnesses": {
			"gemini": {
				"auth_selectedType": "vertex-ai"
			}
		}
	}`

	err := MergeSettings(base, []byte(overrideJSON))
	if err != nil {
		t.Fatal(err)
	}

	if base.Harnesses["gemini"].AuthSelectedType != "vertex-ai" {
		t.Errorf("expected AuthSelectedType to be vertex-ai, got %s", base.Harnesses["gemini"].AuthSelectedType)
	}
}

func TestRuntimeEnvMerging(t *testing.T) {
	s := &Settings{
		Runtimes: map[string]RuntimeConfig{
			"docker": {
				Env: map[string]string{
					"R1": "V1",
					"R2": "V2",
				},
			},
		},
		Profiles: map[string]ProfileConfig{
			"dev": {
				Runtime: "docker",
				Env: map[string]string{
					"R2": "P2", // Overrides runtime
					"P1": "PV1",
				},
			},
		},
	}

	r, _, err := s.ResolveRuntime("dev")
	if err != nil {
		t.Fatal(err)
	}

	expected := map[string]string{
		"R1": "V1",
		"R2": "P2",
		"P1": "PV1",
	}

	if len(r.Env) != len(expected) {
		t.Errorf("expected %d env vars, got %d", len(expected), len(r.Env))
	}

	for k, v := range expected {
		if r.Env[k] != v {
			t.Errorf("expected %s=%s, got %s", k, v, r.Env[k])
		}
	}
}

func TestVolumeMerging(t *testing.T) {
	s := &Settings{
		Harnesses: map[string]HarnessConfig{
			"gemini": {
				Volumes: []api.VolumeMount{
					{Source: "/host/1", Target: "/container/1"},
				},
			},
		},
		Profiles: map[string]ProfileConfig{
			"dev": {
				Volumes: []api.VolumeMount{
					{Source: "/host/2", Target: "/container/2"},
				},
				HarnessOverrides: map[string]HarnessOverride{
					"gemini": {
						Volumes: []api.VolumeMount{
							{Source: "/host/3", Target: "/container/3"},
						},
					},
				},
			},
		},
	}

	h, err := s.ResolveHarness("dev", "gemini")
	if err != nil {
		t.Fatal(err)
	}

	if len(h.Volumes) != 3 {
		t.Errorf("expected 3 volumes, got %d", len(h.Volumes))
	}

	// Check for existence of all expected volumes
	found := make(map[string]bool)
	for _, v := range h.Volumes {
		found[v.Source] = true
	}

	if !found["/host/1"] || !found["/host/2"] || !found["/host/3"] {
		t.Errorf("missing expected volumes: got %v", h.Volumes)
	}
}

func TestHubMethods(t *testing.T) {
	trueBool := true
	falseBool := false

	tests := []struct {
		name                   string
		hub                    *HubClientConfig
		wantConfigured         bool
		wantEnabled            bool
		wantExplicitlyDisabled bool
	}{
		{
			name:                   "nil hub",
			hub:                    nil,
			wantConfigured:         false,
			wantEnabled:            false,
			wantExplicitlyDisabled: false,
		},
		{
			name:                   "empty hub",
			hub:                    &HubClientConfig{},
			wantConfigured:         false,
			wantEnabled:            false,
			wantExplicitlyDisabled: false,
		},
		{
			name:                   "hub with endpoint only",
			hub:                    &HubClientConfig{Endpoint: "https://hub.example.com"},
			wantConfigured:         true,
			wantEnabled:            false,
			wantExplicitlyDisabled: false,
		},
		{
			name:                   "hub with endpoint and enabled=true",
			hub:                    &HubClientConfig{Endpoint: "https://hub.example.com", Enabled: &trueBool},
			wantConfigured:         true,
			wantEnabled:            true,
			wantExplicitlyDisabled: false,
		},
		{
			name:                   "hub with endpoint and enabled=false",
			hub:                    &HubClientConfig{Endpoint: "https://hub.example.com", Enabled: &falseBool},
			wantConfigured:         true,
			wantEnabled:            false,
			wantExplicitlyDisabled: true,
		},
		{
			name:                   "hub with enabled=false but no endpoint",
			hub:                    &HubClientConfig{Enabled: &falseBool},
			wantConfigured:         false,
			wantEnabled:            false,
			wantExplicitlyDisabled: true,
		},
		{
			name:                   "hub with endpoint and token implies enabled",
			hub:                    &HubClientConfig{Endpoint: "https://hub.example.com", Token: "scion_pat_xxx"},
			wantConfigured:         true,
			wantEnabled:            true,
			wantExplicitlyDisabled: false,
		},
		{
			name:                   "hub with endpoint and apiKey implies enabled",
			hub:                    &HubClientConfig{Endpoint: "https://hub.example.com", APIKey: "key123"},
			wantConfigured:         true,
			wantEnabled:            true,
			wantExplicitlyDisabled: false,
		},
		{
			name:                   "hub with token only (no endpoint) not enabled",
			hub:                    &HubClientConfig{Token: "scion_pat_xxx"},
			wantConfigured:         false,
			wantEnabled:            false,
			wantExplicitlyDisabled: false,
		},
		{
			name:                   "hub with endpoint, token, and enabled=false overrides to enabled",
			hub:                    &HubClientConfig{Endpoint: "https://hub.example.com", Token: "scion_pat_xxx", Enabled: &falseBool},
			wantConfigured:         true,
			wantEnabled:            true,
			wantExplicitlyDisabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Settings{Hub: tt.hub}

			if got := s.IsHubConfigured(); got != tt.wantConfigured {
				t.Errorf("IsHubConfigured() = %v, want %v", got, tt.wantConfigured)
			}
			if got := s.IsHubEnabled(); got != tt.wantEnabled {
				t.Errorf("IsHubEnabled() = %v, want %v", got, tt.wantEnabled)
			}
			if got := s.IsHubExplicitlyDisabled(); got != tt.wantExplicitlyDisabled {
				t.Errorf("IsHubExplicitlyDisabled() = %v, want %v", got, tt.wantExplicitlyDisabled)
			}
		})
	}
}

func TestExpansion(t *testing.T) {
	os.Setenv("TEST_EXP_VAR", "expanded_value")
	os.Setenv("TEST_EXP_KEY", "EXP_KEY")
	defer os.Unsetenv("TEST_EXP_VAR")
	defer os.Unsetenv("TEST_EXP_KEY")

	base := &Settings{}
	overrideJSON := `{
		"harnesses": {
			"gemini": {
				"env": {"${TEST_EXP_KEY}": "${TEST_EXP_VAR}", "NORMAL": "VAL"},
				"volumes": [
					{ "source": "${TEST_EXP_VAR}/src", "target": "/dest" }
				]
			}
		}
	}`

	err := MergeSettings(base, []byte(overrideJSON))
	if err != nil {
		t.Fatal(err)
	}

	h := base.Harnesses["gemini"]
	if h.Env["EXP_KEY"] != "expanded_value" {
		t.Errorf("expected Env[EXP_KEY]=expanded_value, got %s", h.Env["EXP_KEY"])
	}
	if h.Env["NORMAL"] != "VAL" {
		t.Errorf("expected Env[NORMAL]=VAL, got %s", h.Env["NORMAL"])
	}

	if len(h.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(h.Volumes))
	}
	if h.Volumes[0].Source != "expanded_value/src" {
		t.Errorf("expected volume source 'expanded_value/src', got %s", h.Volumes[0].Source)
	}
}

func TestExpandVolumeMountsPreservesAllFields(t *testing.T) {
	vols := []api.VolumeMount{
		{
			Source:   "/host/path",
			Target:   "/container/path",
			ReadOnly: true,
			Type:     "gcs",
			Bucket:   "my-bucket",
			Prefix:   "some/prefix",
			Mode:     "ro",
		},
	}

	expanded := expandVolumeMounts(vols)
	if len(expanded) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(expanded))
	}

	v := expanded[0]
	if v.Source != "/host/path" {
		t.Errorf("Source = %q, want /host/path", v.Source)
	}
	if v.Target != "/container/path" {
		t.Errorf("Target = %q, want /container/path", v.Target)
	}
	if !v.ReadOnly {
		t.Error("ReadOnly = false, want true")
	}
	if v.Type != "gcs" {
		t.Errorf("Type = %q, want gcs", v.Type)
	}
	if v.Bucket != "my-bucket" {
		t.Errorf("Bucket = %q, want my-bucket", v.Bucket)
	}
	if v.Prefix != "some/prefix" {
		t.Errorf("Prefix = %q, want some/prefix", v.Prefix)
	}
	if v.Mode != "ro" {
		t.Errorf("Mode = %q, want ro", v.Mode)
	}
}

// TestUpdateHubSettingsGlobal tests that hub settings can be saved to global settings.
// This relates to Fix 5 from progress-report.md: Save hub endpoint to global settings during registration.
func TestUpdateHubSettingsGlobal(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Unset SCION_HUB_ENDPOINT so it doesn't override file-loaded values
	if orig, ok := os.LookupEnv("SCION_HUB_ENDPOINT"); ok {
		os.Unsetenv("SCION_HUB_ENDPOINT")
		t.Cleanup(func() { os.Setenv("SCION_HUB_ENDPOINT", orig) })
	}

	// Create global .scion directory
	globalScionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Run("save hub endpoint to global settings", func(t *testing.T) {
		err := UpdateSetting(globalScionDir, "hub.endpoint", "https://hub.example.com", true)
		if err != nil {
			t.Fatalf("UpdateSetting failed: %v", err)
		}

		// Reload settings and verify
		s, err := LoadSettings(globalScionDir)
		if err != nil {
			t.Fatalf("LoadSettings failed: %v", err)
		}

		if s.Hub == nil {
			t.Fatal("expected Hub config to be present")
		}

		if s.Hub.Endpoint != "https://hub.example.com" {
			t.Errorf("expected hub.endpoint 'https://hub.example.com', got %q", s.Hub.Endpoint)
		}
	})

	t.Run("save hub brokerId to global settings", func(t *testing.T) {
		err := UpdateSetting(globalScionDir, "hub.brokerId", "host-uuid-123", true)
		if err != nil {
			t.Fatalf("UpdateSetting failed: %v", err)
		}

		s, err := LoadSettings(globalScionDir)
		if err != nil {
			t.Fatalf("LoadSettings failed: %v", err)
		}

		if s.Hub == nil {
			t.Fatal("expected Hub config to be present")
		}

		if s.Hub.BrokerID != "host-uuid-123" {
			t.Errorf("expected hub.brokerId 'host-uuid-123', got %q", s.Hub.BrokerID)
		}

		// Previous setting should still be present
		if s.Hub.Endpoint != "https://hub.example.com" {
			t.Errorf("expected hub.endpoint to be preserved, got %q", s.Hub.Endpoint)
		}
	})
}

// TestHubSettingsLoadFromGlobal tests that hub settings from global are loaded into project settings.
// This relates to Fix 6 from progress-report.md: RuntimeBroker falls back to settings for hub endpoint.
func TestHubSettingsLoadFromGlobal(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Unset SCION_HUB_ENDPOINT so it doesn't override file-loaded values
	if orig, ok := os.LookupEnv("SCION_HUB_ENDPOINT"); ok {
		os.Unsetenv("SCION_HUB_ENDPOINT")
		t.Cleanup(func() { os.Setenv("SCION_HUB_ENDPOINT", orig) })
	}

	// Create global settings with hub config
	globalScionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	globalSettings := `{
		"hub": {
			"endpoint": "https://global-hub.example.com",
			"brokerId": "global-host-id"
		}
	}`
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.json"), []byte(globalSettings), 0644); err != nil {
		t.Fatal(err)
	}

	// Create project grove directory (no hub settings)
	groveDir := filepath.Join(tmpDir, "my-project")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Load settings from project grove (should inherit from global)
	s, err := LoadSettings(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}

	if s.Hub == nil {
		t.Fatal("expected Hub config to be inherited from global")
	}

	if s.Hub.Endpoint != "https://global-hub.example.com" {
		t.Errorf("expected hub.endpoint from global, got %q", s.Hub.Endpoint)
	}

	if s.Hub.BrokerID != "global-host-id" {
		t.Errorf("expected hub.brokerId from global, got %q", s.Hub.BrokerID)
	}
}

func TestMergeSettingsHubConnections(t *testing.T) {
	base := &Settings{
		HubConnections: map[string]HubConnectionConfig{
			"hub-prod": {Endpoint: "https://hub.prod.example.com"},
		},
	}
	overrideJSON := `{
		"hub_connections": {
			"hub-prod": {"endpoint": "https://hub.prod.v2.example.com"},
			"hub-staging": {"endpoint": "https://hub.staging.example.com"}
		}
	}`

	err := MergeSettings(base, []byte(overrideJSON))
	if err != nil {
		t.Fatal(err)
	}

	if len(base.HubConnections) != 2 {
		t.Fatalf("expected 2 hub connections, got %d", len(base.HubConnections))
	}

	if base.HubConnections["hub-prod"].Endpoint != "https://hub.prod.v2.example.com" {
		t.Errorf("expected hub-prod endpoint to be overridden, got %q", base.HubConnections["hub-prod"].Endpoint)
	}

	if base.HubConnections["hub-staging"].Endpoint != "https://hub.staging.example.com" {
		t.Errorf("expected hub-staging endpoint, got %q", base.HubConnections["hub-staging"].Endpoint)
	}
}

func TestMergeSettingsHubConnectionsNilBase(t *testing.T) {
	base := &Settings{}
	overrideJSON := `{
		"hub_connections": {
			"hub-dev": {"endpoint": "https://hub.dev.example.com"}
		}
	}`

	err := MergeSettings(base, []byte(overrideJSON))
	if err != nil {
		t.Fatal(err)
	}

	if base.HubConnections == nil {
		t.Fatal("expected HubConnections to be initialized")
	}

	if base.HubConnections["hub-dev"].Endpoint != "https://hub.dev.example.com" {
		t.Errorf("expected hub-dev endpoint, got %q", base.HubConnections["hub-dev"].Endpoint)
	}
}

func TestUpdateSettingHubConnections(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// hub_connections.* keys are silently skipped in v1 format (not supported)
	err := UpdateSetting(groveScionDir, "hub_connections.hub-prod.endpoint", "https://hub.prod.example.com", false)
	if err != nil {
		t.Fatalf("UpdateSetting hub_connections should not error (silently skipped in v1): %v", err)
	}
}

func TestUpdateSettingHubConnectionsInvalidKey(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveScionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// In v1 format, hub_connections.* keys are silently skipped regardless of structure
	err := UpdateSetting(groveScionDir, "hub_connections.hub-prod", "value", false)
	if err != nil {
		t.Errorf("hub_connections keys should be silently skipped in v1: %v", err)
	}

	err = UpdateSetting(groveScionDir, "hub_connections.hub-prod.unknown_field", "value", false)
	if err != nil {
		t.Errorf("hub_connections keys should be silently skipped in v1: %v", err)
	}
}

func TestGetSettingValueHubConnections(t *testing.T) {
	s := &Settings{
		HubConnections: map[string]HubConnectionConfig{
			"hub-prod": {Endpoint: "https://hub.prod.example.com"},
		},
	}

	val, err := GetSettingValue(s, "hub_connections.hub-prod.endpoint")
	if err != nil {
		t.Fatalf("GetSettingValue failed: %v", err)
	}
	if val != "https://hub.prod.example.com" {
		t.Errorf("expected hub.prod.example.com, got %q", val)
	}

	// Non-existent connection
	val, err = GetSettingValue(s, "hub_connections.nonexistent.endpoint")
	if err != nil {
		t.Fatalf("GetSettingValue for nonexistent should not error: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string for nonexistent, got %q", val)
	}
}

func TestGetSettingsMapHubConnections(t *testing.T) {
	s := &Settings{
		HubConnections: map[string]HubConnectionConfig{
			"hub-prod":    {Endpoint: "https://hub.prod.example.com"},
			"hub-staging": {Endpoint: "https://hub.staging.example.com"},
		},
	}

	m := GetSettingsMap(s)
	if m["hub_connections.hub-prod.endpoint"] != "https://hub.prod.example.com" {
		t.Errorf("expected hub-prod endpoint in map, got %q", m["hub_connections.hub-prod.endpoint"])
	}
	if m["hub_connections.hub-staging.endpoint"] != "https://hub.staging.example.com" {
		t.Errorf("expected hub-staging endpoint in map, got %q", m["hub_connections.hub-staging.endpoint"])
	}
}

func TestDeleteHubConnection(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveScionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a legacy settings file with two hub connections directly
	// (hub_connections are a legacy-only feature, not supported in v1)
	legacyContent := `hub_connections:
  hub-prod:
    endpoint: https://hub.prod.example.com
  hub-staging:
    endpoint: https://hub.staging.example.com
`
	if err := os.WriteFile(filepath.Join(groveScionDir, "settings.yaml"), []byte(legacyContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Delete one
	err := DeleteHubConnection(groveScionDir, "hub-prod", false)
	if err != nil {
		t.Fatalf("DeleteHubConnection failed: %v", err)
	}

	// Verify only staging remains
	content, _ := os.ReadFile(filepath.Join(groveScionDir, "settings.yaml"))
	if strings.Contains(string(content), "hub-prod") {
		t.Errorf("expected hub-prod to be deleted, got %s", string(content))
	}
	if !strings.Contains(string(content), "hub-staging") {
		t.Errorf("expected hub-staging to still exist, got %s", string(content))
	}

	// Delete the last one
	err = DeleteHubConnection(groveScionDir, "hub-staging", false)
	if err != nil {
		t.Fatalf("DeleteHubConnection last failed: %v", err)
	}

	// Verify hub_connections is cleaned up
	content, _ = os.ReadFile(filepath.Join(groveScionDir, "settings.yaml"))
	if strings.Contains(string(content), "hub_connections") {
		t.Errorf("expected hub_connections to be cleaned up, got %s", string(content))
	}
}

func TestUpdateSetting_SplitStorageWritesToExternalDir(t *testing.T) {
	// When a grove has split storage (grove-id file), UpdateSetting should
	// write to the external config dir (~/.scion/project-configs/…), not the
	// local .scion/ directory, so that LoadSettingsKoanf reads the same values.
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create a project with .scion and a grove-id file (split storage marker)
	projectDir := filepath.Join(tmpHome, "my-project")
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	projectID := "abcd1234-5678-9abc-def0-123456789abc"
	if err := WriteProjectID(scionDir, projectID); err != nil {
		t.Fatal(err)
	}

	// Compute expected external config dir
	projectSlug := api.Slugify("my-project")
	shortUUID := strings.ReplaceAll(projectID, "-", "")[:8]
	externalDir := filepath.Join(tmpHome, ".scion", "project-configs",
		projectSlug+"__"+shortUUID, ".scion")
	if err := os.MkdirAll(externalDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a setting via UpdateSetting
	if err := UpdateSetting(scionDir, "hub.enabled", "true", false); err != nil {
		t.Fatalf("UpdateSetting failed: %v", err)
	}

	// Verify the setting was written to the EXTERNAL config dir
	extContent, err := os.ReadFile(filepath.Join(externalDir, "settings.yaml"))
	if err != nil {
		t.Fatalf("expected settings.yaml in external dir %s, got error: %v", externalDir, err)
	}
	if !strings.Contains(string(extContent), "enabled: true") {
		t.Errorf("expected external settings.yaml to contain 'enabled: true', got:\n%s", extContent)
	}

	// Verify the local .scion/ did NOT get a settings.yaml written
	localPath := filepath.Join(scionDir, "settings.yaml")
	if _, err := os.Stat(localPath); err == nil {
		t.Errorf("expected no settings.yaml in local .scion/ dir, but file exists")
	}

	// Verify LoadSettingsKoanf reads the value correctly
	settings, err := LoadSettingsKoanf(scionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}
	if !settings.IsHubEnabled() {
		t.Errorf("expected hub.enabled=true from effective settings, got false")
	}
}
