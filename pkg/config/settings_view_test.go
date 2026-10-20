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

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupSettingsViewTest gives the test a fresh HOME with the given global
// settings and a project .scion directory with the given settings (each
// skipped when empty), clears every SCION_* variable and runs the test from
// an empty directory. It returns the project .scion directory.
func setupSettingsViewTest(t *testing.T, globalSettings, projectSettings string) string {
	t.Helper()
	var scionVars []string
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "SCION_") {
			scionVars = append(scionVars, name)
		}
	}
	unsetTestEnv(t, scionVars...)
	unsetTestEnv(t, "CLOUD_RUN_INSTANCE")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	resetWarnedProjectIDOverrides()
	t.Cleanup(resetWarnedProjectIDOverrides)

	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	if globalSettings != "" {
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(globalSettings), 0o644))
	}
	projectDir := filepath.Join(home, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	if projectSettings != "" {
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0o644))
	}
	return projectDir
}

// The view keeps the two legacy project IDs apart: ProjectID is the
// project's own ID (the project-id file outranks hub.project_id), and
// GetHubProjectID is the hub project ID the settings files record.
func TestLoadSettingsView_LocalAndHubProjectID(t *testing.T) {
	const local, hub, env = "local-id", "hub-id", "env-hub-id"
	for _, tc := range []struct {
		name                 string
		settingsHubID        string
		fileID               string
		hubEnv               string
		ignoreEnv            bool
		wantLocal, wantHubID string
	}{
		{name: "file and settings", settingsHubID: hub, fileID: local, wantLocal: local, wantHubID: hub},
		{name: "settings only", settingsHubID: hub, wantLocal: hub, wantHubID: hub},
		{name: "file only", fileID: local, wantLocal: local, wantHubID: ""},
		{name: "env replaces both", settingsHubID: hub, fileID: local, hubEnv: env, wantLocal: env, wantHubID: env},
		{name: "env without a settings hub ID", fileID: local, hubEnv: env, wantLocal: env, wantHubID: ""},
		{name: "env ignored", settingsHubID: hub, fileID: local, hubEnv: env, ignoreEnv: true, wantLocal: local, wantHubID: hub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := "schema_version: \"1\"\nhub:\n  endpoint: https://hub.example.com\n"
			if tc.settingsHubID != "" {
				project += "  project_id: " + tc.settingsHubID + "\n"
			}
			projectDir := setupSettingsViewTest(t, "schema_version: \"1\"\n", project)
			if tc.fileID != "" {
				require.NoError(t, os.WriteFile(filepath.Join(projectDir, projectkeys.ProjectIDFile), []byte(tc.fileID+"\n"), 0o644))
			}
			if tc.hubEnv != "" {
				t.Setenv(projectkeys.EnvHubProjectID, tc.hubEnv)
			}

			load := LoadSettings
			if tc.ignoreEnv {
				load = LoadSettingsIgnoringEnvProjectID
			}
			s, err := load(projectDir)
			require.NoError(t, err)
			assert.Equal(t, tc.wantLocal, s.ProjectID, "ProjectID")
			assert.Equal(t, tc.wantHubID, s.GetHubProjectID(), "GetHubProjectID")
		})
	}
}

// The view fills Hub.Token and Hub.APIKey from SCION_HUB_TOKEN and
// SCION_HUB_API_KEY only (a parity guard for IsHubEnabled); a hub.token in a
// settings file is not read.
func TestLoadSettingsView_HubTokenFromEnvOnly(t *testing.T) {
	legacyProject := "hub:\n  endpoint: https://hub.example.com\n  token: file-token\n  apiKey: file-key\n"

	t.Run("file keys are not read", func(t *testing.T) {
		projectDir := setupSettingsViewTest(t, "", legacyProject)
		s, err := LoadSettings(projectDir)
		require.NoError(t, err)
		require.NotNil(t, s.Hub)
		assert.Empty(t, s.Hub.Token)
		assert.Empty(t, s.Hub.APIKey)
		assert.False(t, s.IsHubEnabled(), "an endpoint without a token or hub.enabled is not enabled")
	})

	t.Run("environment", func(t *testing.T) {
		projectDir := setupSettingsViewTest(t, "", legacyProject)
		t.Setenv("SCION_HUB_TOKEN", "env-token")
		t.Setenv("SCION_HUB_API_KEY", "env-key")
		s, err := LoadSettings(projectDir)
		require.NoError(t, err)
		require.NotNil(t, s.Hub)
		assert.Equal(t, "env-token", s.Hub.Token)
		assert.Equal(t, "env-key", s.Hub.APIKey)
		assert.True(t, s.IsHubEnabled(), "an endpoint and a token from the environment is enabled, as before")
	})

	t.Run("environment without hub settings", func(t *testing.T) {
		projectDir := setupSettingsViewTest(t, "", "")
		t.Setenv("SCION_HUB_TOKEN", "env-token")
		s, err := LoadSettings(projectDir)
		require.NoError(t, err)
		require.NotNil(t, s.Hub)
		assert.Equal(t, "env-token", s.Hub.Token)
	})
}

// Every field read outside the package is carried into the view.
func TestLoadSettingsView_CarriesV1Fields(t *testing.T) {
	global := `schema_version: "1"
workspace_path: /work/space
cli:
  autohelp: false
  mode: agent
hub:
  endpoint: https://hub.example.com
  enabled: true
  linked: true
  local_only: false
  transport:
    mode: iap
    audience: aud-1
hub_connections:
  main:
    endpoint: https://main.example.com
server:
  broker:
    broker_id: broker-1
    broker_token: broker-secret
    broker_nickname: nick
`
	projectDir := setupSettingsViewTest(t, global, "")
	s, err := LoadSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "/work/space", s.WorkspacePath)
	require.NotNil(t, s.CLI)
	require.NotNil(t, s.CLI.AutoHelp)
	assert.False(t, *s.CLI.AutoHelp)
	assert.Equal(t, "agent", s.CLI.Mode)
	require.NotNil(t, s.Hub)
	assert.Equal(t, "https://hub.example.com", s.Hub.Endpoint)
	assert.True(t, s.IsHubLinked())
	require.NotNil(t, s.Hub.LocalOnly)
	assert.False(t, *s.Hub.LocalOnly)
	require.NotNil(t, s.Hub.Transport)
	assert.Equal(t, HubTransportConfig{Mode: "iap", Audience: "aud-1"}, *s.Hub.Transport)
	assert.Equal(t, map[string]HubConnectionConfig{"main": {Endpoint: "https://main.example.com"}}, s.HubConnections)
	assert.Equal(t, "broker-1", s.Hub.BrokerID)
	assert.Equal(t, "broker-secret", s.Hub.BrokerToken)
	assert.Equal(t, "nick", s.Hub.BrokerNickname)
	assert.Contains(t, s.Profiles, "local", "the embedded defaults are a layer of the view")
}

// The legacy-only settings that no v1 setting reads stay out of the view:
// SCION_HUB_BROKER_ID / _BROKER_TOKEN (the v1 broker identity is
// server.broker), the bucket settings, and hub.lastSyncedAt in a file.
func TestLoadSettingsView_LegacyOnlyValuesNotRead(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "hub:\n  endpoint: https://hub.example.com\n  lastSyncedAt: \"2026-01-01T00:00:00Z\"\nbucket:\n  name: b\n")
	t.Setenv("SCION_HUB_BROKER_ID", "env-broker")
	t.Setenv("SCION_HUB_BROKER_TOKEN", "env-broker-token")
	t.Setenv("SCION_BUCKET_NAME", "env-bucket")

	s, err := LoadSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, s.Hub)
	assert.Empty(t, s.Hub.BrokerID)
	assert.Empty(t, s.Hub.BrokerToken)
	assert.Empty(t, s.Hub.LastSyncedAt)
	assert.Nil(t, s.Bucket)
	assert.Equal(t, "https://hub.example.com", s.Hub.Endpoint)
}

// Inside a hub-managed agent container, SCION_WORKSPACE_PATH is the
// runtime's own variable, not a settings override.
func TestLoadSettingsView_WorkspacePathEnvInAgentContainer(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "workspace_path: /from/settings\n")
	t.Setenv("SCION_WORKSPACE_PATH", "/from/env")

	s, err := LoadSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, "/from/env", s.WorkspacePath, "outside an agent container the variable applies")

	t.Setenv("SCION_AGENT_ID", "agent-1")
	s, err = LoadSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, "/from/settings", s.WorkspacePath, "inside an agent container it does not")
}

// SCION_CLI_AUTOHELP and SCION_CLI_MODE apply as settings.
func TestLoadSettingsView_CLIEnv(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "")
	t.Setenv("SCION_CLI_AUTOHELP", "false")
	t.Setenv("SCION_CLI_MODE", "agent")

	s, err := LoadSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, s.CLI)
	require.NotNil(t, s.CLI.AutoHelp)
	assert.False(t, *s.CLI.AutoHelp)
	assert.Equal(t, "agent", s.CLI.Mode)
}

// A legacy-format project file under a versioned global file is converted at
// its own layer: its legacy keys reach their v1 settings, its v1-only keys
// are kept, and the conversion's deprecation warnings are returned.
func TestLoadEffectiveSettings_MixedHierarchyConvertsLegacyFile(t *testing.T) {
	legacyProject := `harnesses:
  gemini:
    image: example.com/gemini:1
    auth_selectedType: api-key
hub:
  endpoint: https://hub.example.com
  projectId: hub-camel
  brokerId: legacy-broker
cli:
  interactive_disabled: true
image_registry: registry.example.com
profiles:
  dev:
    runtime: docker
    harness_overrides:
      gemini:
        auth_selectedType: vertex-ai
`
	projectDir := setupSettingsViewTest(t, "schema_version: \"1\"\nactive_profile: local\n", legacyProject)

	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	require.Contains(t, vs.HarnessConfigs, "gemini")
	assert.Equal(t, "example.com/gemini:1", vs.HarnessConfigs["gemini"].Image)
	assert.Equal(t, "api-key", vs.HarnessConfigs["gemini"].AuthSelectedType)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "hub-camel", vs.Hub.ProjectID)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "legacy-broker", vs.Server.Broker.BrokerID)
	require.NotNil(t, vs.CLI)
	require.NotNil(t, vs.CLI.InteractiveDisabled)
	assert.True(t, *vs.CLI.InteractiveDisabled, "a v1-only key of a legacy file is kept")
	assert.Equal(t, "registry.example.com", vs.ImageRegistry)
	require.Contains(t, vs.Profiles, "dev")
	assert.Equal(t, "vertex-ai", vs.Profiles["dev"].HarnessOverrides["gemini"].AuthSelectedType)
	assert.Contains(t, vs.Profiles, "local", "the default profiles still load")

	joined := strings.Join(warnings, "\n")
	assert.Contains(t, joined, "harnesses is deprecated")
	assert.Contains(t, joined, "hub.brokerId is deprecated")
}

// An all-legacy hierarchy loads through the same layers, and
// LoadEffectiveSettings agrees with the LoadSettings view.
func TestLoadEffectiveSettings_LegacyHierarchyMatchesView(t *testing.T) {
	projectDir := setupSettingsViewTest(t,
		"active_profile: remote\nhub:\n  brokerId: g-broker\n",
		"default_template: tpl\nhub:\n  endpoint: https://p.example.com\n  projectId: p-hub\n")

	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	s, err := LoadSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "remote", vs.ActiveProfile)
	assert.Equal(t, vs.ActiveProfile, s.ActiveProfile)
	assert.Equal(t, "tpl", s.DefaultTemplate)
	assert.Equal(t, "https://p.example.com", s.Hub.Endpoint)
	assert.Equal(t, "p-hub", s.GetHubProjectID())
	assert.Equal(t, "g-broker", s.Hub.BrokerID)
	assert.Equal(t, vs.Server.Broker.BrokerID, s.Hub.BrokerID)
}

// tryLockSettingsDir never waits: it fails while the lock is held in this
// process and succeeds once it is free.
func TestTryLockSettingsDir(t *testing.T) {
	dir := t.TempDir()
	held, err := LockSettingsDir(dir)
	require.NoError(t, err)
	if l, ok := tryLockSettingsDir(dir); ok {
		l.Unlock()
		t.Fatal("tryLockSettingsDir succeeded while the lock was held")
	}
	held.Unlock()

	l, ok := tryLockSettingsDir(dir)
	require.True(t, ok)
	require.NoError(t, l.checkFor(filepath.Join(dir, "settings.yaml")))
	l.Unlock()

	again, err := LockSettingsDir(dir)
	require.NoError(t, err, "the lock is free again after Unlock")
	again.Unlock()
}

// A legacy file's hub.projectId names the hub project only: it is the view's
// GetHubProjectID, and ProjectID stays the file's top-level project_id (the
// LoadEffectiveSettings result carries it as hub.project_id, as before).
func TestLoadSettingsView_LegacyCamelHubProjectIDIsHubOnly(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "project_id: local-id\nhub:\n  enabled: true\n  projectId: hub-id\n")

	s, err := LoadSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, "local-id", s.ProjectID)
	assert.Equal(t, "hub-id", s.GetHubProjectID())

	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "hub-id", vs.Hub.ProjectID)
	assert.Equal(t, "local-id", vs.ProjectID)

	// A later layer's snake-case hub.project_id is the project's own ID
	// again.
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("project_id: local-id\nhub:\n  enabled: true\n  project_id: snake-id\n"), 0o644))
	s, err = LoadSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, "snake-id", s.ProjectID)
	assert.Equal(t, "snake-id", s.GetHubProjectID())
}

// A legacy-format file with a value of the wrong type fails to load, as it
// did with the legacy loader (a versioned file falls back to the default for
// that key instead).
func TestLoadSettings_LegacyFileWrongTypeFails(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "harnesses:\n  claude: {}\nactive_profile: [1, 2]\n", "")
	_, err := LoadSettings(projectDir)
	require.Error(t, err)
	_, _, err = LoadGlobalSettings()
	require.Error(t, err)
}

// A legacy-format file that only partly sets a profile or runtime entry
// converts only the keys it sets: the lower layer's profile runtime and
// runtime type survive (the conversion would otherwise emit runtime: "" and
// type: NAME for the entry).
func TestLoadSettings_PartialLegacyEntryKeepsLowerLayer(t *testing.T) {
	partialProject := `profiles:
  dev:
    volumes:
      - source: /src
        target: /dst
    harness_overrides:
      claude:
        image: example.com/claude:2
runtimes:
  gke:
    namespace: proj-ns
`
	for _, tc := range []struct {
		name, global string
	}{
		{name: "v1 global", global: "schema_version: \"1\"\nprofiles:\n  dev:\n    runtime: gke\nruntimes:\n  gke:\n    type: kubernetes\n    namespace: base-ns\n"},
		{name: "legacy global", global: "harnesses: {}\nprofiles:\n  dev:\n    runtime: gke\nruntimes:\n  gke:\n    namespace: base-ns\n    context: base-ctx\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := setupSettingsViewTest(t, tc.global, partialProject)

			vs, _, err := LoadEffectiveSettings(projectDir)
			require.NoError(t, err)
			require.Contains(t, vs.Profiles, "dev")
			assert.Equal(t, "gke", vs.Profiles["dev"].Runtime, "the global profile runtime survives")
			assert.Len(t, vs.Profiles["dev"].Volumes, 1, "the project's own profile key applies")
			assert.Equal(t, "example.com/claude:2", vs.Profiles["dev"].HarnessOverrides["claude"].Image)
			require.Contains(t, vs.Runtimes, "gke")
			assert.Equal(t, "proj-ns", vs.Runtimes["gke"].Namespace, "the project's own runtime key applies")
			if tc.name == "v1 global" {
				assert.Equal(t, "kubernetes", vs.Runtimes["gke"].Type, "the global runtime type survives")
			} else {
				assert.Equal(t, "base-ctx", vs.Runtimes["gke"].Context)
				assert.Empty(t, vs.Runtimes["gke"].Type, "a legacy file sets no runtime type")
			}

			s, err := LoadSettings(projectDir)
			require.NoError(t, err)
			_, name, err := s.ResolveRuntime("dev")
			require.NoError(t, err)
			assert.Equal(t, "gke", name)
		})
	}
}

// The embedded defaults' runtime type survives a legacy file that sets
// other keys of the same runtime.
func TestLoadSettings_PartialLegacyRuntimeKeepsDefaultType(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "runtimes:\n  kubernetes:\n    namespace: proj-ns\n")
	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	require.Contains(t, vs.Runtimes, "kubernetes")
	assert.Equal(t, "kubernetes", vs.Runtimes["kubernetes"].Type)
	assert.Equal(t, "proj-ns", vs.Runtimes["kubernetes"].Namespace)
}

// legacySourceKey maps each converted v1 key to the legacy file key it comes
// from.
func TestLegacySourceKey(t *testing.T) {
	for key, want := range map[string]string{
		"hub.project_id":                                      "hub.projectId",
		"server.broker.broker_id":                             "hub.brokerId",
		"server.broker.broker_token":                          "hub.brokerToken",
		"server.broker.broker_nickname":                       "hub.brokerNickname",
		"runtimes.gke.type":                                   "",
		"runtimes.gke.namespace":                              "runtimes.gke.namespace",
		"harness_configs.claude.harness":                      "harnesses.claude",
		"harness_configs.claude.auth_selected_type":           "harnesses.claude.auth_selectedType",
		"harness_configs.claude.env.FOO":                      "harnesses.claude.env.FOO",
		"profiles.dev.harness_overrides.c.auth_selected_type": "profiles.dev.harness_overrides.c.auth_selectedType",
		"profiles.dev.runtime":                                "profiles.dev.runtime",
		"cli.mode":                                            "cli.mode",
	} {
		assert.Equal(t, want, legacySourceKey(key), key)
	}
}

// A legacy entry's harness, derived from the entry's name, does not replace
// the harness a lower layer sets for the same entry; with no lower entry it
// is set from the name, as before.
func TestLoadSettings_LegacyHarnessEntryKeepsLowerHarness(t *testing.T) {
	legacyProject := "harnesses:\n  claude-vertex:\n    image: example.com/claude:3\n  gemini:\n    image: example.com/gemini:3\n"
	projectDir := setupSettingsViewTest(t,
		"schema_version: \"1\"\nharness_configs:\n  claude-vertex:\n    harness: claude\n    user: scion\n", legacyProject)

	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	require.Contains(t, vs.HarnessConfigs, "claude-vertex")
	assert.Equal(t, "claude", vs.HarnessConfigs["claude-vertex"].Harness, "the lower layer's harness survives")
	assert.Equal(t, "example.com/claude:3", vs.HarnessConfigs["claude-vertex"].Image)
	assert.Equal(t, "scion", vs.HarnessConfigs["claude-vertex"].User)
	require.Contains(t, vs.HarnessConfigs, "gemini")
	assert.Equal(t, "gemini", vs.HarnessConfigs["gemini"].Harness, "with no lower entry the harness comes from the name")
}

// Legacy keys that differ only in case convert, as the legacy decode reads
// them case-insensitively.
func TestLoadSettings_LegacyKeysMatchCaseInsensitively(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "",
		"project_id: local-id\nhub:\n  projectID: hub-ci\n  brokerID: broker-ci\nprofiles:\n  dev:\n    runtime: docker\n    harness_overrides:\n      claude:\n        auth_selectedtype: vertex-ai\n")

	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "hub-ci", vs.Hub.ProjectID)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "broker-ci", vs.Server.Broker.BrokerID)
	assert.Equal(t, "vertex-ai", vs.Profiles["dev"].HarnessOverrides["claude"].AuthSelectedType)

	s, err := LoadSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, "hub-ci", s.GetHubProjectID(), "a case-variant hub.projectId is the hub ID only")
	assert.Equal(t, "local-id", s.ProjectID)
}
