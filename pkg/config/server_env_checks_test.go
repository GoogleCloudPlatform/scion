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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerEnvValues_FalseIsFalse: =false is the boolean false in the
// opsettings env layer, the seed layer and the bootstrap merge (not the
// string "false"), and in GlobalConfig.
func TestServerEnvValues_FalseIsFalse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_HUB_AUTOSUSPENDSTALLED", "false")
	t.Setenv("SCION_SEED_TELEMETRY_ENABLED", "false")
	t.Setenv("SCION_SERVER_DEFAULTMAXTURNS", "12")
	assert.Equal(t, false, LoadEnvKoanf().Get("server.hub.auto_suspend_stalled"))
	assert.Equal(t, int64(12), LoadEnvKoanf().Get("default_max_turns"))
	assert.Equal(t, false, LoadSeedEnvKoanf().Get("telemetry.enabled"))
	k := LoadBootstrapKoanf()
	assert.Equal(t, false, k.Get("server.hub.auto_suspend_stalled"))
	assert.Equal(t, false, k.Get("telemetry.enabled"))

	t.Setenv("SCION_SERVER_HUB_CORS_ENABLED", "false")
	gc, err := LoadGlobalConfig(t.TempDir())
	require.NoError(t, err)
	assert.False(t, gc.Hub.CORSEnabled)

	// A value that does not parse stays a string in the loaders;
	// ValidateServerEnv rejects it at startup.
	t.Setenv("SCION_SERVER_HUB_AUTOSUSPENDSTALLED", "nope")
	assert.Equal(t, "nope", LoadEnvKoanf().Get("server.hub.auto_suspend_stalled"))
}

// testIsLayer1 stands in for opsettings.IsLayer1Key, which pkg/config cannot
// import: every setting but the Layer-0 keys server.hub.port and
// server.hub.read_timeout counts as one seeding seeds. The opsettings tests
// use the real registry.
func testIsLayer1(path string) bool {
	return path != "server.hub.port" && path != "server.hub.read_timeout"
}

// TestValidateServerEnv: an unparseable value fails, naming the variable and
// the setting but not the value; an empty value is unset; a string setting
// takes anything; the directly-read ADMIN_MODE uses its reader's parser.
func TestValidateServerEnv(t *testing.T) {
	require.NoError(t, ValidateServerEnv([]string{
		"SCION_SERVER_HUB_AUTOSUSPENDSTALLED=false",
		"SCION_SERVER_HUB_CORS_ENABLED=0",
		"SCION_SERVER_DEFAULTMAXTURNS=12",
		"SCION_SEED_SERVER_HUB_AUTOSUSPENDSTALLED=true",
		"SCION_SERVER_HUB_ADMINEMAILS=a@example.com",
		"SCION_SERVER_DATABASE_MAXOPENCONNS=",
		"SCION_SERVER_ADMIN_MODE=yes",
		"SCION_SERVER_NO_SUCH_SETTING=whatever",
		"HOME=/tmp",
	}, testIsLayer1))

	err := ValidateServerEnv([]string{
		"SCION_SERVER_HUB_AUTOSUSPENDSTALLED=secret-garbage",
		"SCION_SEED_DEFAULT_MAX_TURNS=twelve",
		"SCION_SERVER_ADMIN_MODE=maybe",
	}, testIsLayer1)
	require.Error(t, err)
	msg := err.Error()
	for _, want := range []string{
		"SCION_SERVER_HUB_AUTOSUSPENDSTALLED (server.hub.auto_suspend_stalled)",
		"boolean",
		"SCION_SEED_DEFAULT_MAX_TURNS (default_max_turns)",
		"integer",
		"SCION_SERVER_ADMIN_MODE",
	} {
		assert.Contains(t, msg, want)
	}
	for _, leak := range []string{"secret-garbage", "twelve", "maybe"} {
		assert.NotContains(t, msg, leak)
	}

	// Every boolean, integer and number setting is checked, by its
	// canonical name and every alias.
	n := 0
	for _, e := range ServerEnvEntries() {
		switch e.Type {
		case "boolean", "integer", "number":
		default:
			continue
		}
		for _, name := range append([]string{e.Canonical}, e.Aliases...) {
			n++
			assert.Error(t, ValidateServerEnv([]string{name + "=x-not-a-value"}, testIsLayer1), name)
		}
	}
	if n < 50 {
		t.Fatalf("only %d typed names checked", n)
	}
}

// TestFindServerEnvAliases: every legacy alias set is reported with its
// canonical name, and one another spelling overrides is reported as ignored.
func TestFindServerEnvAliases(t *testing.T) {
	got := FindServerEnvAliases([]string{
		"SCION_SERVER_HUB_ADMIN_EMAILS=a@example.com",
		"SCION_SERVER_HUB_ADMINEMAILS=b@example.com",
		"SCION_SERVER_BROKER_PORT=9801",
		"SCION_SEED_SERVER_HUB_STALLED_THRESHOLD=5m",
		"SCION_SERVER_BROKER_HUB_ENDPOINT=http://a:1",
		"SCION_SERVER_BROKER_HUBENDPOINT=http://b:1",
		"SCION_SERVER_RUNTIMEBROKER_PORT=", // empty: unset
		"SCION_SERVER_HUB_PORT=9810",       // canonical: not an alias
	}, testIsLayer1)
	assert.Equal(t, []ServerEnvAliasUse{
		{Name: "SCION_SEED_SERVER_HUB_STALLED_THRESHOLD", Use: "SCION_SEED_SERVER_HUB_STALLEDTHRESHOLD", Path: "server.hub.stalled_threshold"},
		{Name: "SCION_SERVER_BROKER_HUBENDPOINT", Use: "SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT", Path: "server.broker.hub_endpoint"},
		{Name: "SCION_SERVER_BROKER_HUB_ENDPOINT", Use: "SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT", Path: "server.broker.hub_endpoint", IgnoredFor: "SCION_SERVER_BROKER_HUBENDPOINT"},
		{Name: "SCION_SERVER_BROKER_PORT", Use: "SCION_SERVER_RUNTIMEBROKER_PORT", Path: "server.broker.port"},
		{Name: "SCION_SERVER_HUB_ADMIN_EMAILS", Use: "SCION_SERVER_HUB_ADMINEMAILS", Path: "server.hub.admin_emails", IgnoredFor: "SCION_SERVER_HUB_ADMINEMAILS"},
	}, got)
}

// TestWarnDeprecatedServerEnvAliases_NamesNotValues: one warning per alias,
// naming the alias and its replacement, never the value.
func TestWarnDeprecatedServerEnvAliases_NamesNotValues(t *testing.T) {
	var buf bytes.Buffer
	WarnDeprecatedServerEnvAliases(slog.New(slog.NewTextHandler(&buf, nil)), []string{
		"SCION_SERVER_HUB_ADMIN_EMAILS=topsecret@example.com",
		"SCION_SERVER_HUB_PORT=9810",
	}, testIsLayer1)
	out := buf.String()
	assert.Equal(t, 1, strings.Count(out, "level=WARN"), out)
	assert.Contains(t, out, "env_var=SCION_SERVER_HUB_ADMIN_EMAILS")
	assert.Contains(t, out, "use=SCION_SERVER_HUB_ADMINEMAILS")
	assert.Contains(t, out, "next release")
	assert.NotContains(t, out, "topsecret")
}

// TestAgentContainerEnvExcluded: inside a hub-managed agent container,
// SCION_RUNTIME and SCION_WORKSPACE_PATH (set by the runtime) do not alter
// LoadVersionedSettings, and the process env keeps them for their direct
// readers. Outside an agent container SCION_WORKSPACE_PATH still maps.
func TestAgentContainerEnvExcluded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_AGENT_ID", "agent-uuid-1")
	t.Setenv("SCION_RUNTIME", "")
	t.Setenv("SCION_WORKSPACE_PATH", "")
	require.NoError(t, os.Unsetenv("SCION_RUNTIME"))
	require.NoError(t, os.Unsetenv("SCION_WORKSPACE_PATH"))
	base, err := LoadVersionedSettings("")
	require.NoError(t, err)

	t.Setenv("SCION_RUNTIME", "substrate")
	t.Setenv("SCION_WORKSPACE_PATH", "/agent/workspace")
	assert.Equal(t, "", versionedEnvKeyMapper("SCION_RUNTIME"))
	assert.Equal(t, "", versionedEnvKeyMapper("SCION_WORKSPACE_PATH"))
	got, err := LoadVersionedSettings("")
	require.NoError(t, err)
	assert.Equal(t, base, got, "the container-injected variables do not alter settings loading")
	assert.Equal(t, "substrate", os.Getenv("SCION_RUNTIME"), "the process env is unchanged")
	assert.Equal(t, "/agent/workspace", os.Getenv("SCION_WORKSPACE_PATH"), "the process env is unchanged")

	t.Setenv("SCION_AGENT_ID", "")
	assert.Equal(t, "workspace_path", versionedEnvKeyMapper("SCION_WORKSPACE_PATH"), "outside an agent container it maps as before")
}

// TestValidateServerEnv_OnlyValuesARead: ValidateServerEnv checks only values
// a consumer reads. A SCION_SEED_* name of a setting seeding does not seed and
// an alias its canonical name overrides pass with a bad value; the same alias
// alone, and a bad canonical value, fail. A blank SCION_SERVER_ADMIN_MODE is
// unset, as for its reader.
func TestValidateServerEnv_OnlyValuesARead(t *testing.T) {
	assert.NoError(t, ValidateServerEnv([]string{"SCION_SEED_SERVER_HUB_PORT=abc"}, testIsLayer1), "a Layer-0 seed name is not read")
	assert.NoError(t, ValidateServerEnv([]string{
		"SCION_SERVER_HUB_AUTO_SUSPEND_STALLED=garbage",
		"SCION_SERVER_HUB_AUTOSUSPENDSTALLED=true",
	}, testIsLayer1), "an overridden alias is not read")
	assert.NoError(t, ValidateServerEnv([]string{
		"SCION_SERVER_BROKER_HUB_ENDPOINT=x",
		"SCION_SERVER_BROKER_PORT=9801",
	}, testIsLayer1))
	assert.Error(t, ValidateServerEnv([]string{"SCION_SERVER_HUB_AUTO_SUSPEND_STALLED=garbage"}, testIsLayer1), "the alias alone is read")
	assert.Error(t, ValidateServerEnv([]string{
		"SCION_SERVER_HUB_AUTO_SUSPEND_STALLED=true",
		"SCION_SERVER_HUB_AUTOSUSPENDSTALLED=garbage",
	}, testIsLayer1), "the canonical value is read")
	assert.Error(t, ValidateServerEnv([]string{"SCION_SEED_SERVER_HUB_AUTOSUSPENDSTALLED=garbage"}, testIsLayer1), "a Layer-1 seed name is read")
	assert.NoError(t, ValidateServerEnv([]string{"SCION_SERVER_ADMIN_MODE=   "}, testIsLayer1), "a blank ADMIN_MODE is unset")
	// A seed alias of a setting seeding does not seed gets no deprecation
	// warning (FindUnmatchedSettingsEnv reports it as matching nothing).
	e := serverEnvEntry(t, "server.hub.read_timeout")
	require.Contains(t, e.Aliases, "SCION_SERVER_HUB_READ_TIMEOUT")
	assert.Empty(t, FindServerEnvAliases([]string{"SCION_SEED_SERVER_HUB_READ_TIMEOUT=30s"}, testIsLayer1))
}
