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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// badValueSettingsYAML has valid values next to the ones each test breaks.
const badValueSettingsYAML = `schema_version: "1"
default_template: file-template
server:
  broker:
    broker_name: file-broker
    port: 9800
  hub:
    auto_suspend_stalled: %s
    stalled_threshold: 7m
`

const badValueWarning = "value does not parse"

// setupBadValueProject writes a settings.yaml under a fresh HOME, clears
// the SCION_* variables the tests use, and captures slog output.
func setupBadValueProject(t *testing.T, autoSuspendStalled string) (string, *bytes.Buffer) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	unsetTestEnv(t,
		"SCION_DEFAULT_TEMPLATE", "SCION_CLI_AUTOHELP",
		"SCION_SERVER_BROKER_PORT", "SCION_SERVER_BROKER_BROKER_NAME",
		"SCION_SERVER_HUB_AUTO_SUSPEND_STALLED", "SCION_SERVER_HUB_STALLED_THRESHOLD",
		"SCION_PROJECT", "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID",
		"CLOUD_RUN_INSTANCE")
	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	content := []byte(fmt.Sprintf(badValueSettingsYAML, autoSuspendStalled))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), content, 0644))

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return projectDir, &buf
}

// TestLoadVersionedSettings_BadValueFallsBackPerField: an int from the
// environment and a bool from the settings file that do not parse must not
// fail the load. Only those two fields fall back; the rest still load.
func TestLoadVersionedSettings_BadValueFallsBackPerField(t *testing.T) {
	projectDir, logs := setupBadValueProject(t, "maybe")
	t.Setenv("SCION_SERVER_BROKER_PORT", "abc")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err, "one unparseable value must not fail the whole load")

	assert.Equal(t, "file-template", vs.DefaultTemplate)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "file-broker", vs.Server.Broker.BrokerName)
	assert.Equal(t, 0, vs.Server.Broker.Port, "bad port falls back to its default, not the file value it overrode")
	require.NotNil(t, vs.Server.Hub)
	assert.Equal(t, "7m", vs.Server.Hub.StalledThreshold)
	assert.Nil(t, vs.Server.Hub.AutoSuspendStalled, "bad bool falls back to unset")

	out := logs.String()
	assert.Contains(t, out, badValueWarning)
	assert.Contains(t, out, "server.broker.port")
	assert.Contains(t, out, "server.hub.auto_suspend_stalled")
	assert.NotContains(t, out, "abc", "the bad value must not be logged")
	assert.NotContains(t, out, "maybe", "the bad value must not be logged")
}

// TestLoadVersionedSettings_BadValueRestoresEmbeddedDefault: a key that has
// an embedded default falls back to that default, not to the zero value.
func TestLoadVersionedSettings_BadValueRestoresEmbeddedDefault(t *testing.T) {
	projectDir, logs := setupBadValueProject(t, "true")
	t.Setenv("SCION_CLI_AUTOHELP", "notabool")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.CLI)
	require.NotNil(t, vs.CLI.AutoHelp, "cli.autohelp must fall back to the embedded default")
	assert.True(t, *vs.CLI.AutoHelp)
	assert.Equal(t, 9800, vs.Server.Broker.Port)
	assert.Contains(t, logs.String(), "cli.autohelp")
}

// TestLoadVersionedSettings_GoodValuesUnchanged guards the normal path: valid
// settings load as before and no fallback warning is logged.
func TestLoadVersionedSettings_GoodValuesUnchanged(t *testing.T) {
	projectDir, logs := setupBadValueProject(t, "true")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "file-template", vs.DefaultTemplate)
	assert.Equal(t, "file-broker", vs.Server.Broker.BrokerName)
	assert.Equal(t, 9800, vs.Server.Broker.Port)
	require.NotNil(t, vs.Server.Hub.AutoSuspendStalled)
	assert.True(t, *vs.Server.Hub.AutoSuspendStalled)
	assert.Equal(t, "7m", vs.Server.Hub.StalledThreshold)
	assert.NotContains(t, logs.String(), badValueWarning)
}
