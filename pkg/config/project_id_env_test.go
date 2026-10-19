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
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	envIDTestEnvHub   = "env-hub-project-id"
	envIDTestEnvPlain = "env-plain-project-id"
	envIDTestSettings = "settings-hub-project-id"
	envIDTestFile     = "file-project-id"
)

// setupProjectIDEnvTest creates a fresh HOME with empty global settings and
// a plain project whose settings set hub.project_id to settingsHubID (when
// not empty) and whose project-id file holds fileID (when not empty). It
// clears the project-ID environment, resets the warning dedup and captures
// slog output. It returns the project's .scion path and the log buffer.
func setupProjectIDEnvTest(t *testing.T, settingsHubID, fileID string) (string, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT",
		projectkeys.EnvProjectID, projectkeys.EnvHubProjectID)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\n"), 0o644))

	projectDir := filepath.Join(home, "plain", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	settings := "schema_version: \"1\"\nhub:\n  enabled: true\n"
	if settingsHubID != "" {
		settings += "  project_id: \"" + settingsHubID + "\"\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settings), 0o644))
	if fileID != "" {
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, projectkeys.ProjectIDFile), []byte(fileID+"\n"), 0o644))
	}

	resetWarnedProjectIDOverrides()
	t.Cleanup(resetWarnedProjectIDOverrides)
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return projectDir, &buf
}

// overrideWarnings returns the project-ID override warnings in logs.
func overrideWarnings(logs *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "overrides a different project ID") {
			out = append(out, line)
		}
	}
	return out
}

// assertNoProjectIDValues checks that no warning prints a project ID.
func assertNoProjectIDValues(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	for _, v := range []string{envIDTestEnvHub, envIDTestEnvPlain, envIDTestSettings, envIDTestFile} {
		assert.NotContains(t, logs.String(), v, "the warning names sources, never project ID values")
	}
}

// TestProjectIDEnvPrecedence: SCION_HUB_PROJECT_ID outranks the settings
// key hub.project_id and the project-id file in both loaders, and each
// overridden file source is named in one warning.
func TestProjectIDEnvPrecedence(t *testing.T) {
	tests := []struct {
		name          string
		settingsHubID string
		fileID        string
		// wantSources are the overridden sources the warnings must name
		// ("FILE" stands for the project-id file path).
		wantLegacy []string
		wantV1     []string
	}{
		{name: "env only"},
		{
			name:          "settings hub.project_id",
			settingsHubID: envIDTestSettings,
			wantLegacy:    []string{projectkeys.ConfigHubProjectIDKey},
			wantV1:        []string{projectkeys.ConfigHubProjectIDKey},
		},
		{
			name:       "project-id file",
			fileID:     envIDTestFile,
			wantLegacy: []string{"FILE"},
			wantV1:     []string{"FILE"},
		},
		{
			name:          "settings and project-id file",
			settingsHubID: envIDTestSettings,
			fileID:        envIDTestFile,
			wantLegacy:    []string{projectkeys.ConfigHubProjectIDKey, "FILE"},
			wantV1:        []string{projectkeys.ConfigHubProjectIDKey, "FILE"},
		},
		{
			name:          "file sources agree with env",
			settingsHubID: envIDTestEnvHub,
			fileID:        envIDTestEnvHub,
		},
	}
	for _, tc := range tests {
		check := func(t *testing.T, logs *bytes.Buffer, projectDir string, want []string) {
			t.Helper()
			warnings := overrideWarnings(logs)
			require.Len(t, warnings, len(want), logs.String())
			for i, src := range want {
				if src == "FILE" {
					src = filepath.Join(projectDir, projectkeys.ProjectIDFile)
				}
				assert.Contains(t, warnings[i], projectkeys.EnvHubProjectID)
				assert.Contains(t, warnings[i], src)
			}
			assertNoProjectIDValues(t, logs)
		}
		t.Run(tc.name+"/legacy", func(t *testing.T) {
			projectDir, logs := setupProjectIDEnvTest(t, tc.settingsHubID, tc.fileID)
			t.Setenv(projectkeys.EnvHubProjectID, envIDTestEnvHub)
			s, err := LoadSettingsKoanf(projectDir)
			require.NoError(t, err)
			assert.Equal(t, envIDTestEnvHub, s.ProjectID)
			if tc.settingsHubID != "" {
				assert.Equal(t, envIDTestEnvHub, s.GetHubProjectID(), "the hub project ID follows the env too")
			}
			check(t, logs, projectDir, tc.wantLegacy)
		})
		t.Run(tc.name+"/v1", func(t *testing.T) {
			projectDir, logs := setupProjectIDEnvTest(t, tc.settingsHubID, tc.fileID)
			t.Setenv(projectkeys.EnvHubProjectID, envIDTestEnvHub)
			vs, err := LoadVersionedSettings(projectDir)
			require.NoError(t, err)
			require.NotNil(t, vs.Hub)
			assert.Equal(t, envIDTestEnvHub, vs.Hub.ProjectID)
			check(t, logs, projectDir, tc.wantV1)
		})
	}
}

// TestProjectIDEnvPrecedence_LegacyCamelHubProjectID: in the legacy
// loader, a settings hub.projectId that differs from hub.project_id is its
// own overridden source: one warning names it, and GetHubProjectID returns
// the env value.
func TestProjectIDEnvPrecedence_LegacyCamelHubProjectID(t *testing.T) {
	const camelID = "camel-hub-project-id"
	projectDir, logs := setupProjectIDEnvTest(t, envIDTestSettings, "")
	settings := filepath.Join(projectDir, "settings.yaml")
	data, err := os.ReadFile(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settings, append(data, []byte("  projectId: \""+camelID+"\"\n")...), 0o644))
	t.Setenv(projectkeys.EnvHubProjectID, envIDTestEnvHub)

	s, err := LoadSettingsKoanf(projectDir)
	require.NoError(t, err)
	assert.Equal(t, envIDTestEnvHub, s.ProjectID)
	assert.Equal(t, envIDTestEnvHub, s.GetHubProjectID())

	var camel []string
	for _, w := range overrideWarnings(logs) {
		if strings.Contains(w, "overridden="+projectkeys.ConfigHubProjectIDJSON) {
			camel = append(camel, w)
		}
	}
	require.Len(t, camel, 1, logs.String())
	assert.Contains(t, camel[0], "source="+projectkeys.EnvHubProjectID)
	assert.NotContains(t, logs.String(), camelID, "the warning names sources, never project ID values")
	assertNoProjectIDValues(t, logs)
}

// TestProjectIDEnvPrecedence_PlainVarUnchanged: SCION_PROJECT_ID keeps its
// rank. In the legacy loader it loses to hub.project_id and to the
// project-id file; in the v1 loader it sets only the top-level project_id
// and the hub project ID still comes from the files. No warning.
func TestProjectIDEnvPrecedence_PlainVarUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, settingsHubID, fileID, want string
	}{
		{name: "settings hub.project_id", settingsHubID: envIDTestSettings, want: envIDTestSettings},
		{name: "project-id file", fileID: envIDTestFile, want: envIDTestFile},
		{name: "both", settingsHubID: envIDTestSettings, fileID: envIDTestFile, want: envIDTestFile},
	} {
		t.Run(tc.name+"/legacy", func(t *testing.T) {
			projectDir, logs := setupProjectIDEnvTest(t, tc.settingsHubID, tc.fileID)
			t.Setenv(projectkeys.EnvProjectID, envIDTestEnvPlain)
			s, err := LoadSettingsKoanf(projectDir)
			require.NoError(t, err)
			assert.Equal(t, tc.want, s.ProjectID)
			assert.Empty(t, overrideWarnings(logs))
		})
		t.Run(tc.name+"/v1", func(t *testing.T) {
			projectDir, logs := setupProjectIDEnvTest(t, tc.settingsHubID, tc.fileID)
			t.Setenv(projectkeys.EnvProjectID, envIDTestEnvPlain)
			vs, err := LoadVersionedSettings(projectDir)
			require.NoError(t, err)
			require.NotNil(t, vs.Hub)
			assert.Equal(t, tc.want, vs.Hub.ProjectID)
			assert.Equal(t, envIDTestEnvPlain, vs.ProjectID)
			assert.Empty(t, overrideWarnings(logs))
		})
	}

	// With no file source, the plain variable is the legacy project ID.
	t.Run("env only/legacy", func(t *testing.T) {
		projectDir, _ := setupProjectIDEnvTest(t, "", "")
		t.Setenv(projectkeys.EnvProjectID, envIDTestEnvPlain)
		s, err := LoadSettingsKoanf(projectDir)
		require.NoError(t, err)
		assert.Equal(t, envIDTestEnvPlain, s.ProjectID)
	})
}

// TestProjectIDEnvPrecedence_HubVarBeatsPlainVar: in the legacy loader,
// SCION_HUB_PROJECT_ID is applied after the env provider (which maps only
// SCION_PROJECT_ID), so it wins over SCION_PROJECT_ID; repeated loads warn
// once, naming both variables.
func TestProjectIDEnvPrecedence_HubVarBeatsPlainVar(t *testing.T) {
	projectDir, logs := setupProjectIDEnvTest(t, "", "")
	t.Setenv(projectkeys.EnvProjectID, envIDTestEnvPlain)
	t.Setenv(projectkeys.EnvHubProjectID, envIDTestEnvHub)
	for i := 0; i < 5; i++ {
		s, err := LoadSettingsKoanf(projectDir)
		require.NoError(t, err)
		assert.Equal(t, envIDTestEnvHub, s.ProjectID)
	}
	warnings := overrideWarnings(logs)
	require.Len(t, warnings, 1, logs.String())
	assert.Contains(t, warnings[0], "source="+projectkeys.EnvHubProjectID)
	assert.Contains(t, warnings[0], "overridden="+projectkeys.EnvProjectID)
	assertNoProjectIDValues(t, logs)
}

// TestProjectIDEnvPrecedence_IgnoringEnv: LoadSettingsIgnoringEnvProjectID
// still ignores SCION_HUB_PROJECT_ID, and warns about nothing.
func TestProjectIDEnvPrecedence_IgnoringEnv(t *testing.T) {
	projectDir, logs := setupProjectIDEnvTest(t, envIDTestSettings, envIDTestFile)
	t.Setenv(projectkeys.EnvHubProjectID, envIDTestEnvHub)
	s, err := LoadSettingsIgnoringEnvProjectID(projectDir)
	require.NoError(t, err)
	assert.Equal(t, envIDTestFile, s.ProjectID)
	assert.Empty(t, overrideWarnings(logs))
}

// TestProjectIDEnvPrecedence_WarningDedup: loading the same settings again
// does not repeat the warning; each distinct disagreement (another env
// value, another file value) warns again.
func TestProjectIDEnvPrecedence_WarningDedup(t *testing.T) {
	projectDir, logs := setupProjectIDEnvTest(t, "", envIDTestFile)
	marker := filepath.Join(projectDir, projectkeys.ProjectIDFile)
	t.Setenv(projectkeys.EnvHubProjectID, envIDTestEnvHub)

	load := func() {
		t.Helper()
		_, err := LoadSettingsKoanf(projectDir)
		require.NoError(t, err)
		_, err = LoadVersionedSettings(projectDir)
		require.NoError(t, err)
	}
	load()
	load()
	require.Len(t, overrideWarnings(logs), 1, "an identical repeat is reported once across both loaders")

	t.Setenv(projectkeys.EnvHubProjectID, envIDTestEnvHub+"-2")
	load()
	require.Len(t, overrideWarnings(logs), 2, "another env value is a new disagreement")

	require.NoError(t, os.WriteFile(marker, []byte(envIDTestFile+"-2\n"), 0o644))
	load()
	load()
	require.Len(t, overrideWarnings(logs), 3, "another file value is a new disagreement")

	for _, w := range overrideWarnings(logs) {
		assert.Contains(t, w, marker)
	}
	assertNoProjectIDValues(t, logs)
}

func TestWarnProjectIDOverride_NoWarningWithoutDisagreement(t *testing.T) {
	_, logs := setupProjectIDEnvTest(t, "", "")
	warnProjectIDOverride("A", "", "B", "x")
	warnProjectIDOverride("A", "x", "B", "")
	warnProjectIDOverride("A", "x", "B", "x")
	assert.Empty(t, overrideWarnings(logs))
	warnProjectIDOverride("A", "x", "B", "y")
	warnProjectIDOverride("A", "x", "C", "y")
	assert.Len(t, overrideWarnings(logs), 2, "each pair of sources is its own disagreement")
}
