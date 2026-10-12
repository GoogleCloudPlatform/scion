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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- Struct round-trip tests ---

func TestVersionedSettings_YAMLRoundTrip(t *testing.T) {
	autoHelp := true

	vs := &VersionedSettings{
		SchemaVersion:   "1",
		ActiveProfile:   "local",
		DefaultTemplate: "gemini",
		Hub: &V1HubClientConfig{
			Enabled:   boolPtr(true),
			Endpoint:  "https://hub.example.com",
			ProjectID: "test-project",
		},
		CLI: &V1CLIConfig{
			AutoHelp:            &autoHelp,
			InteractiveDisabled: boolPtr(false),
		},
		Runtimes: map[string]V1RuntimeConfig{
			"docker":    {Type: "docker", Host: ""},
			"container": {Type: "container"},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
				Model:   "gemini-2.5-pro",
				Args:    []string{"--sandbox=strict"},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {
				Runtime:              "container",
				DefaultTemplate:      "gemini",
				DefaultHarnessConfig: "gemini",
			},
		},
	}

	// Marshal to YAML
	data, err := yaml.Marshal(vs)
	require.NoError(t, err)

	// Validate against schema
	valErrors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "round-tripped YAML should validate against schema, got: %v", valErrors)

	// Unmarshal back
	var roundTripped VersionedSettings
	err = yaml.Unmarshal(data, &roundTripped)
	require.NoError(t, err)

	assert.Equal(t, vs.SchemaVersion, roundTripped.SchemaVersion)
	assert.Equal(t, vs.ActiveProfile, roundTripped.ActiveProfile)
	assert.Equal(t, vs.DefaultTemplate, roundTripped.DefaultTemplate)
	assert.Equal(t, vs.Hub.Endpoint, roundTripped.Hub.Endpoint)
	assert.Equal(t, vs.Hub.ProjectID, roundTripped.Hub.ProjectID)
	assert.Equal(t, vs.HarnessConfigs["gemini"].Model, roundTripped.HarnessConfigs["gemini"].Model)
	assert.Equal(t, vs.HarnessConfigs["gemini"].Args, roundTripped.HarnessConfigs["gemini"].Args)
	assert.Equal(t, vs.Profiles["local"].DefaultHarnessConfig, roundTripped.Profiles["local"].DefaultHarnessConfig)
}

// --- LoadVersionedSettings tests ---

func TestLoadVersionedSettings_DefaultsOnly(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "default", vs.DefaultTemplate)
	assert.Equal(t, "antigravity", vs.DefaultHarnessConfig)
	// harness_configs block is no longer in default settings (lives on disk as harness-config dirs)
	assert.Contains(t, vs.Runtimes, "docker")
	assert.Equal(t, "docker", vs.Runtimes["docker"].Type)
}

func TestLoadVersionedSettings_GlobalOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `
schema_version: "1"
active_profile: prod
default_template: claude
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "prod", vs.ActiveProfile)
	assert.Equal(t, "claude", vs.DefaultTemplate)
}

// TestLoadVersionedSettings_GlobalHarnessConfigNotOverriddenByProjectDefaults
// verifies that when a user sets default_harness_config and default_template in
// their global settings, initializing a project (which writes the embedded
// project defaults into the project settings file) does not override those
// global values.
//
// Regression test for GoogleCloudPlatform/scion#212.
func TestLoadVersionedSettings_GlobalHarnessConfigNotOverriddenByProjectDefaults(t *testing.T) {
	tmpDir := t.TempDir()

	t.Setenv("HOME", tmpDir)

	// Set up global settings with custom default_harness_config and default_template.
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `
schema_version: "1"
default_harness_config: opencode
default_template: my-template
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Set up a project directory with the embedded project defaults — this is
	// exactly what scion init writes into the project settings file.
	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	projectDefaults, err := GetProjectDefaultSettingsYAML()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), projectDefaults, 0644))

	// Load merged settings. The project defaults must NOT override the user's
	// global preferences for default_harness_config and default_template.
	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "opencode", vs.DefaultHarnessConfig,
		"global default_harness_config should not be overridden by project defaults (issue #212)")
	assert.Equal(t, "my-template", vs.DefaultTemplate,
		"global default_template should not be overridden by project defaults")
}

func TestLoadVersionedSettings_ProjectOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `
schema_version: "1"
active_profile: prod
default_template: claude
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	projectSettings := `
schema_version: "1"
active_profile: staging
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "staging", vs.ActiveProfile)
	// Template should still be claude from global
	assert.Equal(t, "claude", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_EnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Set environment variable overrides
	_ = os.Setenv("SCION_ACTIVE_PROFILE", "remote")
	defer func() { _ = os.Unsetenv("SCION_ACTIVE_PROFILE") }()

	_ = os.Setenv("SCION_DEFAULT_TEMPLATE", "opencode")
	defer func() { _ = os.Unsetenv("SCION_DEFAULT_TEMPLATE") }()

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "remote", vs.ActiveProfile)
	assert.Equal(t, "opencode", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_HubEnvVars(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Test SCION_HUB_PROJECT_ID maps correctly (regression test)
	_ = os.Setenv("SCION_HUB_PROJECT_ID", "my-project-id")
	defer func() { _ = os.Unsetenv("SCION_HUB_PROJECT_ID") }()

	_ = os.Setenv("SCION_HUB_LOCAL_ONLY", "true")
	defer func() { _ = os.Unsetenv("SCION_HUB_LOCAL_ONLY") }()

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.Hub)
	assert.Equal(t, "my-project-id", vs.Hub.ProjectID)
}

// TestLoadVersionedSettings_LegacyHubEnvNeverAdopted is the negative half of
// TestLoadVersionedSettings_HubEnvVars: SCION_HUB_GROVE_ID must never
// resolve to a project ID here either, even though it maps to the same
// hub.grove_id key the *file*-based fallback reads. Guards against the
// generic "hub_" env mapper reviving the variable via hub.grove_id when
// only the EnvHubGroveID special case is removed.
func TestLoadVersionedSettings_LegacyHubEnvNeverAdopted(t *testing.T) {
	tmpDir := t.TempDir()

	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Agent containers export the canonical project-ID env vars, which
	// populate ProjectID and would make this test fail for a reason
	// unrelated to the variable under test.
	unsetTestEnv(t, "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID")

	t.Setenv("SCION_HUB_GROVE_ID", "legacy-env-uuid")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	if vs.Hub != nil {
		assert.Empty(t, vs.Hub.ProjectID, "SCION_HUB_GROVE_ID must not be adopted")
	}
}

// TestLoadVersionedSettings_LegacyHubEnvDoesNotOverrideFile pins that the
// file-based hub.grove_id fallback is unaffected by the env
// var's removal: a legacy file value still resolves, and a legacy env var
// set alongside it changes nothing (it is dropped entirely, not merely
// out-ranked).
func TestLoadVersionedSettings_LegacyHubEnvDoesNotOverrideFile(t *testing.T) {
	tmpDir := t.TempDir()

	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	v1Settings := "schema_version: \"1\"\nhub:\n  grove_id: \"file-grove\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(v1Settings), 0644))

	// Agent containers export the canonical project-ID env vars, which
	// populate ProjectID and would make this test fail for a reason
	// unrelated to the variable under test.
	unsetTestEnv(t, "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID")

	t.Setenv("SCION_HUB_GROVE_ID", "legacy-env-uuid")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.Hub)
	assert.Equal(t, "file-grove", vs.Hub.ProjectID)
}

func TestLoadVersionedSettings_CLIEnvVars(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	_ = os.Setenv("SCION_CLI_AUTOHELP", "false")
	defer func() { _ = os.Unsetenv("SCION_CLI_AUTOHELP") }()

	_ = os.Setenv("SCION_CLI_INTERACTIVE_DISABLED", "true")
	defer func() { _ = os.Unsetenv("SCION_CLI_INTERACTIVE_DISABLED") }()

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.CLI)
}

func TestLoadVersionedSettings_JSONFallback(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	// Write JSON settings (should load via JSON fallback)
	globalJSON := `{
		"schema_version": "1",
		"active_profile": "json-profile",
		"default_template": "json-template"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.json"), []byte(globalJSON), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "json-profile", vs.ActiveProfile)
	assert.Equal(t, "json-template", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_NewFields(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	projectSettings := `
schema_version: "1"
harness_configs:
  gemini-custom:
    harness: gemini
    image: example.com/gemini:v2
    user: scion
    model: gemini-2.5-pro
    args: ["--sandbox=strict", "--verbose"]
runtimes:
  my-docker:
    type: docker
    host: tcp://remote:2376
profiles:
  custom:
    runtime: my-docker
    default_template: gemini
    default_harness_config: gemini-custom
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	// Check new harness config fields
	hc, ok := vs.HarnessConfigs["gemini-custom"]
	require.True(t, ok)
	assert.Equal(t, "gemini", hc.Harness)
	assert.Equal(t, "gemini-2.5-pro", hc.Model)
	assert.Equal(t, []string{"--sandbox=strict", "--verbose"}, hc.Args)

	// Check runtime type field
	rt, ok := vs.Runtimes["my-docker"]
	require.True(t, ok)
	assert.Equal(t, "docker", rt.Type)
	assert.Equal(t, "tcp://remote:2376", rt.Host)

	// Check new profile fields
	profile, ok := vs.Profiles["custom"]
	require.True(t, ok)
	assert.Equal(t, "gemini", profile.DefaultTemplate)
	assert.Equal(t, "gemini-custom", profile.DefaultHarnessConfig)
}

// TestLoadVersionedSettings_FederationGoogleIssuerFields loads
// allowed_domains and allowed_gcp_projects from a settings.yaml file through
// the real koanf decode path (LoadVersionedSettings), rather than
// constructing a V1TrustedIssuerConfig struct literal directly. A koanf tag
// typo on either field would silently drop it from the decoded settings
// (failing open to "no domain/project constraint") without this test
// noticing, since every other test for these fields builds the Go struct by
// hand.
func TestLoadVersionedSettings_FederationGoogleIssuerFields(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	projectSettings := `
schema_version: "1"
server:
  federation:
    enabled: true
    trusted_issuers:
      - issuer_url: "https://accounts.google.com"
        issuer_type: "user"
        expected_audience: "client-id.apps.googleusercontent.com"
        allowed_domains: ["example.com", "other.example"]
        allowed_gcp_projects: ["gcp-proj-1", "gcp-proj-2"]
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Federation)
	require.Len(t, vs.Server.Federation.TrustedIssuers, 1)
	issuer := vs.Server.Federation.TrustedIssuers[0]
	assert.Equal(t, []string{"example.com", "other.example"}, issuer.AllowedDomains)
	assert.Equal(t, []string{"gcp-proj-1", "gcp-proj-2"}, issuer.AllowedGCPProjects)
}

// --- LoadEffectiveSettings tests ---

func TestLoadEffectiveSettings_VersionedFileRouting(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Write versioned project settings
	projectSettings := `
schema_version: "1"
active_profile: versioned-profile
harness_configs:
  gemini:
    harness: gemini
    image: example.com/gemini:latest
    user: scion
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "versioned-profile", vs.ActiveProfile)
	assert.Empty(t, warnings, "versioned path should produce no deprecation warnings")
}

func TestLoadEffectiveSettings_LegacyFileRouting(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Write legacy project settings (has harnesses, no schema_version)
	projectSettings := `
active_profile: legacy-profile
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
profiles:
  legacy-profile:
    runtime: docker
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "legacy-profile", vs.ActiveProfile)
	assert.Equal(t, "1", vs.SchemaVersion) // Should be set by adapter
	assert.NotEmpty(t, warnings, "legacy path should produce deprecation warnings")
}

func TestLoadEffectiveSettings_NoUserFiles(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// No settings files — should use defaults via legacy path
	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "default", vs.DefaultTemplate)
	// Defaults flow through legacy path since no user files, so we get harness warnings
	// from the adaptation of embedded defaults
	_ = warnings
}

func TestLoadEffectiveSettings_WarnOnIgnoredSettingsFile(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	t.Run("non-empty file without schema_version produces warning", func(t *testing.T) {
		// Write settings with real keys but no schema_version, no harnesses,
		// and no v1 runtime indicators — this file will be silently ignored.
		settingsContent := "default_template: my-template\n"
		settingsPath := filepath.Join(projectDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte(settingsContent), 0644))
		defer func() { _ = os.Remove(settingsPath) }()

		_, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)

		found := false
		for _, w := range warnings {
			if strings.Contains(w, "no schema_version field") && strings.Contains(w, settingsPath) {
				found = true
				break
			}
		}
		assert.True(t, found, "expected warning about missing schema_version, got warnings: %v", warnings)
	})

	t.Run("empty file does not produce warning", func(t *testing.T) {
		settingsPath := filepath.Join(projectDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte(""), 0644))
		defer func() { _ = os.Remove(settingsPath) }()

		_, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)

		for _, w := range warnings {
			assert.NotContains(t, w, "no schema_version field",
				"empty file should not produce schema_version warning")
		}
	})

	t.Run("file with only comments does not produce warning", func(t *testing.T) {
		settingsPath := filepath.Join(projectDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte("# just a comment\n"), 0644))
		defer func() { _ = os.Remove(settingsPath) }()

		_, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)

		for _, w := range warnings {
			assert.NotContains(t, w, "no schema_version field",
				"comment-only file should not produce schema_version warning")
		}
	})

	t.Run("legacy format file does not produce warning", func(t *testing.T) {
		// Write a file with harnesses key (legacy format)
		settingsPath := filepath.Join(projectDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte("harnesses:\n  claude:\n    image: test\n"), 0644))
		defer func() { _ = os.Remove(settingsPath) }()

		_, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)
		for _, w := range warnings {
			assert.NotContains(t, w, "no schema_version field",
				"legacy format file should not produce schema_version warning")
		}
	})
}

// TestLoadVersionedSettings_AutoExposePortsEnvDoesNotBreakDecode is the
// regression test for https://github.com/ptone/scion/issues/2447
// ("runtimebroker: project runtime settings silently ignored when a
// colliding SCION_* env var is set"). SCION_AUTO_EXPOSE_PORTS and
// SCION_AUTO_EXPOSE_PORTS_LIST are variables the hub sets inside agent
// containers (consumed only by
// sciontool's auto-expose scanner); they are never settings overrides. Left
// mapped, they land on the bare key "auto_expose_ports", which collides
// with the struct-typed AutoExposePorts field and used to make koanf's
// Unmarshal fail outright for every LoadVersionedSettings/LoadEffectiveSettings
// caller whose own process happened to have one of them set — not just
// resolveManagerForOpts. Before the fix, LoadVersionedSettings below returns
// an error; after it, decoding succeeds and file + real env overrides both
// still apply.
func TestLoadVersionedSettings_AutoExposePortsEnvDoesNotBreakDecode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	settingsYAML := `schema_version: "1"
active_profile: file-profile
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsYAML), 0644))

	t.Run("colliding vars set, no real override: file value wins", func(t *testing.T) {
		t.Setenv("SCION_AUTO_EXPOSE_PORTS", "true")
		t.Setenv("SCION_AUTO_EXPOSE_PORTS_LIST", "8000,8080,3000")

		vs, err := LoadVersionedSettings(projectDir)
		require.NoError(t, err, "SCION_AUTO_EXPOSE_PORTS/_LIST must never break LoadVersionedSettings decoding")
		assert.Equal(t, "file-profile", vs.ActiveProfile)
	})

	t.Run("colliding vars set alongside a real override: override still applies", func(t *testing.T) {
		t.Setenv("SCION_AUTO_EXPOSE_PORTS", "true")
		t.Setenv("SCION_AUTO_EXPOSE_PORTS_LIST", "8000,8080,3000")
		t.Setenv("SCION_ACTIVE_PROFILE", "env-profile")

		vs, err := LoadVersionedSettings(projectDir)
		require.NoError(t, err)
		assert.Equal(t, "env-profile", vs.ActiveProfile, "SCION_ACTIVE_PROFILE must still override the file value")
	})
}

// TestLoadEffectiveSettings_AutoExposePortsEnvDoesNotBreakDecode exercises
// the same regression through LoadEffectiveSettings (the caller
// resolveManagerForOpts actually uses), covering both the versioned and
// legacy settings-file branches.
func TestLoadEffectiveSettings_AutoExposePortsEnvDoesNotBreakDecode(t *testing.T) {
	t.Run("versioned settings file", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		t.Setenv("SCION_AUTO_EXPOSE_PORTS", "true")
		t.Setenv("SCION_AUTO_EXPOSE_PORTS_LIST", "8000,8080,3000")

		projectDir := filepath.Join(tmpDir, "my-project", ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0755))

		settingsYAML := `schema_version: "1"
active_profile: file-profile
`
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsYAML), 0644))

		vs, _, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err, "SCION_AUTO_EXPOSE_PORTS/_LIST must never break LoadEffectiveSettings decoding")
		assert.Equal(t, "file-profile", vs.ActiveProfile)
	})

	// "legacy settings file" is a guard, not a regression test: the legacy
	// Settings struct has no auto_expose_ports field, so this branch never
	// failed to decode even before the env key mapper excluded these two
	// variables. It still passes with that exclusion reverted.
	t.Run("legacy settings file", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		t.Setenv("SCION_AUTO_EXPOSE_PORTS", "true")
		t.Setenv("SCION_AUTO_EXPOSE_PORTS_LIST", "8000,8080,3000")

		projectDir := filepath.Join(tmpDir, "my-project", ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0755))

		settingsYAML := `active_profile: legacy-file-profile
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
`
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsYAML), 0644))

		vs, _, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err, "SCION_AUTO_EXPOSE_PORTS/_LIST must never break the legacy LoadEffectiveSettings branch")
		assert.Equal(t, "legacy-file-profile", vs.ActiveProfile)
	})
}

// TestLoadSettings_BareScionHubEnvDoesNotBreakDecode is the regression
// test for https://github.com/ptone/scion/issues/2724. A bare SCION_HUB
// variable used to map to the top-level key "hub" as a string, which
// collides with the struct-typed hub settings and made koanf's Unmarshal
// fail with "'hub' expected a map or struct, got string" for any project
// whose settings.yaml has a hub: map. Nothing in scion reads a bare
// SCION_HUB, so the env key mappers drop it.
func TestLoadSettings_BareScionHubEnvDoesNotBreakDecode(t *testing.T) {
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS", "SCION_HUB")

	writeProject := func(t *testing.T, settingsYAML string) string {
		t.Helper()
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		projectDir := filepath.Join(tmpDir, "my-project", ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsYAML), 0644))
		return projectDir
	}
	newProject := func(t *testing.T) string {
		t.Helper()
		return writeProject(t, `schema_version: "1"
hub:
  endpoint: https://file.example.com
`)
	}
	// newLegacyProject writes an unversioned settings.yaml (no
	// schema_version), which LoadEffectiveSettings routes through
	// LoadSettingsKoanf and its legacy env key mapper. The legacy
	// Settings.Hub field is struct-typed too, so it collides the same way.
	newLegacyProject := func(t *testing.T) string {
		t.Helper()
		return writeProject(t, `hub:
  endpoint: https://file.example.com
`)
	}

	t.Run("SCION_HUB unset: file hub map loads", func(t *testing.T) {
		projectDir := newProject(t)
		vs, err := LoadVersionedSettings(projectDir)
		require.NoError(t, err)
		require.NotNil(t, vs.Hub)
		assert.Equal(t, "https://file.example.com", vs.Hub.Endpoint)
	})

	t.Run("SCION_HUB set: versioned load ignores it", func(t *testing.T) {
		projectDir := newProject(t)
		t.Setenv("SCION_HUB", "https://env.example.com")
		vs, err := LoadVersionedSettings(projectDir)
		require.NoError(t, err, "a bare SCION_HUB must never break LoadVersionedSettings decoding")
		require.NotNil(t, vs.Hub)
		assert.Equal(t, "https://file.example.com", vs.Hub.Endpoint)
	})

	t.Run("SCION_HUB set: effective load of legacy file ignores it", func(t *testing.T) {
		projectDir := newLegacyProject(t)
		t.Setenv("SCION_HUB", "https://env.example.com")
		vs, _, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err, "a bare SCION_HUB must never break LoadEffectiveSettings decoding")
		require.NotNil(t, vs.Hub)
		assert.Equal(t, "https://file.example.com", vs.Hub.Endpoint)
	})

	t.Run("SCION_HUB set: legacy koanf load ignores it", func(t *testing.T) {
		projectDir := newLegacyProject(t)
		t.Setenv("SCION_HUB", "https://env.example.com")
		s, err := LoadSettingsKoanf(projectDir)
		require.NoError(t, err, "a bare SCION_HUB must never break LoadSettingsKoanf decoding")
		require.NotNil(t, s.Hub)
		assert.Equal(t, "https://file.example.com", s.Hub.Endpoint)
	})

	t.Run("SCION_HUB set: SCION_HUB_ENDPOINT still applies", func(t *testing.T) {
		projectDir := newProject(t)
		t.Setenv("SCION_HUB", "https://env.example.com")
		t.Setenv("SCION_HUB_ENDPOINT", "https://endpoint.example.com")
		vs, err := LoadVersionedSettings(projectDir)
		require.NoError(t, err)
		require.NotNil(t, vs.Hub)
		assert.Equal(t, "https://endpoint.example.com", vs.Hub.Endpoint)
	})
}

// --- Default settings compatibility tests ---

func TestGetDefaultSettingsData_ProducesSameEffectiveDefaults(t *testing.T) {
	// GetDefaultSettingsData should produce the same effective config regardless
	// of whether the embedded file is versioned or legacy
	data, err := GetDefaultSettingsData()
	require.NoError(t, err)

	var settings Settings
	require.NoError(t, json.Unmarshal(data, &settings))

	// Harness configs are no longer inline in settings (they live on disk as harness-config dirs)
	// So the Harnesses map should be empty or nil
	assert.Empty(t, settings.Harnesses, "harness configs should not be inline in default settings")

	// Should have expected runtimes
	assert.Contains(t, settings.Runtimes, "docker")
	assert.Contains(t, settings.Runtimes, "container")
	assert.Contains(t, settings.Runtimes, "kubernetes")

	// Should have expected profiles
	assert.Contains(t, settings.Profiles, "local")
	assert.Contains(t, settings.Profiles, "remote")

	// OS-specific runtime check
	expectedRuntime := "docker"
	if runtime.GOOS == "darwin" {
		expectedRuntime = "container"
	}
	assert.Equal(t, expectedRuntime, settings.Profiles["local"].Runtime)
}

func TestDefaultSettingsValidateAgainstSchema(t *testing.T) {
	// The embedded default_settings.yaml should validate against the v1 schema
	data, err := EmbedsFS.ReadFile("embeds/default_settings.yaml")
	require.NoError(t, err)

	valErrors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "default settings should validate against v1 schema, got: %v", valErrors)
}

func TestDefaultSettingsDataYAML_OSAdjustment(t *testing.T) {
	data, err := GetDefaultSettingsDataYAML()
	require.NoError(t, err)

	// Parse as versioned settings to check OS adjustment
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))

	expectedRuntime := "docker"
	if runtime.GOOS == "darwin" {
		expectedRuntime = "container"
	}

	localProfile, ok := vs.Profiles["local"]
	require.True(t, ok, "local profile should exist")
	assert.Equal(t, expectedRuntime, localProfile.Runtime)
}

// --- resolveEffectiveProjectPath tests ---

func TestResolveEffectiveProjectPath_Global(t *testing.T) {
	result := resolveEffectiveProjectPath("global")
	assert.Equal(t, "", result, "global should resolve to empty (already loaded)")

	result = resolveEffectiveProjectPath("home")
	assert.Equal(t, "", result, "home should resolve to empty (already loaded)")
}

func TestResolveEffectiveProjectPath_Explicit(t *testing.T) {
	// A plain .scion path with no project-id → returned as-is (non-git project)
	result := resolveEffectiveProjectPath("/some/path/.scion")
	assert.Equal(t, "/some/path/.scion", result)
}

func TestResolveEffectiveProjectPath_GitProject(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Simulate a git project with project-id → should redirect to external config dir
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	_ = os.MkdirAll(projectDir, 0755)
	_ = WriteProjectID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	result := resolveEffectiveProjectPath(projectDir)

	want := filepath.Join(tmpHome, ".scion", "project-configs", "my-repo__550e8400", ".scion")
	assert.Equal(t, want, result)
}

// --- versionedEnvKeyMapper tests ---

func TestVersionedEnvKeyMapper(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"SCION_ACTIVE_PROFILE", "active_profile"},
		{"SCION_DEFAULT_TEMPLATE", "default_template"},
		{"SCION_HUB_ENDPOINT", "hub.endpoint"},
		{"SCION_HUB_PROJECT_ID", "hub.project_id"},
		{"SCION_HUB_GROVE_ID", ""},
		{"SCION_HUB_LOCAL_ONLY", "hub.local_only"},
		{"SCION_HUB_ENABLED", "hub.enabled"},
		{"SCION_CLI_AUTOHELP", "cli.autohelp"},
		{"SCION_CLI_INTERACTIVE_DISABLED", "cli.interactive_disabled"},
		{"SCION_SERVER_ENV", "server.env"},
		{"SCION_SERVER_LOG_LEVEL", "server.log_level"},
		{"SCION_AUTO_EXPOSE_PORTS", ""},
		{"SCION_AUTO_EXPOSE_PORTS_LIST", ""},
		{"SCION_HUB", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := versionedEnvKeyMapper(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// --- detectHierarchyFormat tests ---

func TestDetectHierarchyFormat_Versioned(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	versionedSettings := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(versionedSettings), 0644))

	hasVersioned, missingSchemaVersion := detectHierarchyFormat("")
	assert.True(t, hasVersioned)
	assert.False(t, missingSchemaVersion)
}

func TestDetectHierarchyFormat_Legacy(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	legacySettings := `active_profile: local
harnesses:
  gemini:
    image: test
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(legacySettings), 0644))

	hasVersioned, missingSchemaVersion := detectHierarchyFormat("")
	assert.False(t, hasVersioned)
	assert.False(t, missingSchemaVersion)
}

func TestDetectHierarchyFormat_NoFiles(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	hasVersioned, missingSchemaVersion := detectHierarchyFormat("")
	assert.False(t, hasVersioned)
	assert.False(t, missingSchemaVersion)
}

func TestDetectHierarchyFormat_ProjectVersioned(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Global is legacy, project is versioned
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	legacySettings := `active_profile: local
harnesses:
  gemini:
    image: test
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(legacySettings), 0644))

	versionedSettings := `schema_version: "1"
active_profile: custom
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(versionedSettings), 0644))

	hasVersioned, missingSchemaVersion := detectHierarchyFormat(projectDir)
	assert.True(t, hasVersioned)
	assert.False(t, missingSchemaVersion)
}

func TestDetectHierarchyFormat_V1StructuralIndicators(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	// v1-shaped but missing schema_version
	v1ShapedSettings := `active_profile: cr
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project: my-project
      region: us-central1
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(v1ShapedSettings), 0644))

	hasVersioned, missingSchemaVersion := detectHierarchyFormat("")
	assert.True(t, hasVersioned, "v1-shaped file without schema_version should be detected as versioned")
	assert.True(t, missingSchemaVersion, "should flag that schema_version is missing")
}

// --- Hub helper method tests ---

func TestVersionedSettings_GetHubEndpoint(t *testing.T) {
	vs := &VersionedSettings{}
	assert.Equal(t, "", vs.GetHubEndpoint())

	vs.Hub = &V1HubClientConfig{Endpoint: "https://hub.example.com"}
	assert.Equal(t, "https://hub.example.com", vs.GetHubEndpoint())
}

func TestVersionedSettings_IsHubConfigured(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubConfigured())

	vs.Hub = &V1HubClientConfig{}
	assert.False(t, vs.IsHubConfigured())

	vs.Hub.Endpoint = "https://hub.example.com"
	assert.True(t, vs.IsHubConfigured())
}

func TestVersionedSettings_IsHubEnabled(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubEnabled())

	vs.Hub = &V1HubClientConfig{}
	assert.False(t, vs.IsHubEnabled())

	vs.Hub.Enabled = boolPtr(false)
	assert.False(t, vs.IsHubEnabled())

	vs.Hub.Enabled = boolPtr(true)
	assert.True(t, vs.IsHubEnabled())
}

func TestVersionedSettings_IsHubLinked(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubLinked())

	vs.Hub = &V1HubClientConfig{}
	assert.False(t, vs.IsHubLinked())

	vs.Hub.Linked = boolPtr(false)
	assert.False(t, vs.IsHubLinked())

	vs.Hub.Linked = boolPtr(true)
	assert.True(t, vs.IsHubLinked())
}

func TestVersionedSettings_IsHubExplicitlyDisabled(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubExplicitlyDisabled())

	vs.Hub = &V1HubClientConfig{Enabled: boolPtr(true)}
	assert.False(t, vs.IsHubExplicitlyDisabled())

	vs.Hub.Enabled = boolPtr(false)
	assert.True(t, vs.IsHubExplicitlyDisabled())
}
