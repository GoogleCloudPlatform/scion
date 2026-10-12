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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// badValueSettingsYAML has valid values next to the ones each test breaks.
const badValueSettingsYAML = `schema_version: "1"
default_template: file-template
profiles:
  local:
    runtime: docker
    clone_depth: %s
    kubernetes_service_account_mappings:
      agent@example-project.iam.gserviceaccount.com: agent-ksa
server:
  broker:
    broker_name: file-broker
    port: 9800
  hub:
    auto_suspend_stalled: %s
telemetry:
  cloud:
    tls:
      enabled: true
    stalled_threshold: 7m
`

const badValueWarning = "value does not parse"

// setupBadValueProject writes a settings.yaml under a fresh HOME, clears
// the SCION_* variables the tests use, and captures slog output.
func setupBadValueProject(t *testing.T, cloneDepth, autoSuspendStalled string) (string, *bytes.Buffer) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	unsetTestEnv(t,
		"SCION_DEFAULT_TEMPLATE", "SCION_CLI_AUTOHELP",
		"SCION_SERVER_BROKER_PORT", "SCION_SERVER_BROKER_BROKER_NAME",
		"SCION_SERVER_HUB_AUTO_SUSPEND_STALLED", "SCION_SERVER_HUB_STALLED_THRESHOLD",
		"SCION_OTEL_INSECURE", "SCION_OTEL_SKIP_TLS_VERIFY",
		"SCION_PROJECT", "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID",
		"CLOUD_RUN_INSTANCE")
	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	content := []byte(fmt.Sprintf(badValueSettingsYAML, cloneDepth, autoSuspendStalled))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), content, 0644))

	resetWarnedUnparseableSettingsCache()
	t.Cleanup(resetWarnedUnparseableSettingsCache)

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
	projectDir, logs := setupBadValueProject(t, "5", "maybe")
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
	projectDir, logs := setupBadValueProject(t, "5", "true")
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
	projectDir, logs := setupBadValueProject(t, "5", "true")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "file-template", vs.DefaultTemplate)
	assert.Equal(t, "file-broker", vs.Server.Broker.BrokerName)
	assert.Equal(t, 9800, vs.Server.Broker.Port)
	require.NotNil(t, vs.Server.Hub.AutoSuspendStalled)
	assert.True(t, *vs.Server.Hub.AutoSuspendStalled)
	assert.Equal(t, "7m", vs.Server.Hub.StalledThreshold)
	assert.Equal(t, "5", string(vs.Profiles["local"].CloneDepth))
	assert.NotContains(t, logs.String(), badValueWarning)
}

// TestLoadVersionedSettings_BadOtelInsecureFallsBack: a SCION_OTEL_INSECURE
// value that is not a bool must not fail the load. The switch is ignored, so
// telemetry.cloud.tls.enabled keeps its settings value.
func TestLoadVersionedSettings_BadOtelInsecureFallsBack(t *testing.T) {
	projectDir, logs := setupBadValueProject(t, "5", "true")
	t.Setenv("SCION_OTEL_INSECURE", "sometimes")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err, "an unparseable SCION_OTEL_INSECURE must not fail the whole load")

	assert.Equal(t, "file-broker", vs.Server.Broker.BrokerName)
	require.NotNil(t, vs.Telemetry)
	require.NotNil(t, vs.Telemetry.Cloud)
	require.NotNil(t, vs.Telemetry.Cloud.TLS)
	require.NotNil(t, vs.Telemetry.Cloud.TLS.Enabled)
	assert.True(t, *vs.Telemetry.Cloud.TLS.Enabled)

	out := logs.String()
	assert.Contains(t, out, badValueWarning)
	assert.Contains(t, out, "SCION_OTEL_INSECURE")
	assert.NotContains(t, out, "sometimes", "the bad value must not be logged")
}

// TestLoadVersionedSettings_BadCloneDepthFallsBack: a profiles.*.clone_depth
// value that cannot be normalized (a list) must not drop all settings. Only
// that field falls back to unset, which means the default depth.
func TestLoadVersionedSettings_BadCloneDepthFallsBack(t *testing.T) {
	projectDir, logs := setupBadValueProject(t, "[1, 2]", "true")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err, "an unparseable clone_depth must not fail the whole load")

	assert.Equal(t, "file-broker", vs.Server.Broker.BrokerName)
	require.Contains(t, vs.Profiles, "local")
	assert.Equal(t, "docker", vs.Profiles["local"].Runtime)
	assert.Empty(t, vs.Profiles["local"].CloneDepth, "bad clone_depth falls back to unset")

	out := logs.String()
	assert.Contains(t, out, badValueWarning)
	assert.Contains(t, out, "profiles.local.clone_depth")
}

// TestLoadVersionedSettings_BadValueKeepsDottedMapKeys: the fallback must not
// touch a valid map entry whose key contains a dot. A service account email
// key is split into several parts if the probe re-splits the flat key on
// ".", and the entry would then be reported as bad and dropped.
func TestLoadVersionedSettings_BadValueKeepsDottedMapKeys(t *testing.T) {
	projectDir, logs := setupBadValueProject(t, "5", "true")
	t.Setenv("SCION_SERVER_BROKER_PORT", "abc")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, 0, vs.Server.Broker.Port)
	require.Contains(t, vs.Profiles, "local")
	assert.Equal(t, map[string]string{"agent@example-project.iam.gserviceaccount.com": "agent-ksa"},
		vs.Profiles["local"].KubernetesServiceAccountMappings,
		"a valid mapping keyed by a service account email must survive the fallback")

	out := logs.String()
	assert.Contains(t, out, "server.broker.port")
	assert.NotContains(t, out, "kubernetes_service_account_mappings", "no warning for the valid mapping")
}

// TestLoadVersionedSettings_BadValueWarnsOncePerKey: settings are loaded
// several times per process, so the warning for one bad key is logged once.
func TestLoadVersionedSettings_BadValueWarnsOncePerKey(t *testing.T) {
	projectDir, logs := setupBadValueProject(t, "5", "true")
	t.Setenv("SCION_SERVER_BROKER_PORT", "abc")

	for i := 0; i < 3; i++ {
		vs, err := LoadVersionedSettings(projectDir)
		require.NoError(t, err)
		assert.Equal(t, 0, vs.Server.Broker.Port)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "key=server.broker.port"))
}
