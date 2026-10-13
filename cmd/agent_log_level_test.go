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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for --agent-log-level, the explicit opt-in for an agent's log level
// (ptone/scion#4098). Agents no longer inherit the CLI's --debug.

// resetAgentLogLevelGlobals saves and restores the package-level flags these
// tests change.
func resetAgentLogLevelGlobals(t *testing.T) {
	t.Helper()
	origLevel, origDebug := agentLogLevelFlag, debugMode
	origEnable, origDisable := enableTelemetry, disableTelemetry
	t.Cleanup(func() {
		agentLogLevelFlag, debugMode = origLevel, origDebug
		enableTelemetry, disableTelemetry = origEnable, origDisable
	})
	agentLogLevelFlag, debugMode = "", false
	enableTelemetry, disableTelemetry = false, false
}

func TestValidateAgentLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		set     bool
		want    string
		wantErr string
	}{
		{name: "not given", raw: "", set: false, want: ""},
		{name: "single level", raw: "debug", set: true, want: "debug"},
		{name: "per-component spec", raw: "info,hub.auth=debug", set: true, want: "info,hub.auth=debug"},
		{name: "surrounding space trimmed", raw: "  warn  ", set: true, want: "warn"},
		{name: "explicit empty", raw: "", set: true, wantErr: "must not be empty"},
		{name: "only separators", raw: " , ", set: true, wantErr: "must not be empty"},
		{name: "unknown level", raw: "verbose", set: true, wantErr: `invalid log level "verbose"`},
		{name: "bad component level", raw: "info,hub=loud", set: true, wantErr: `component "hub"`},
		{name: "bare level after first entry", raw: "info,debug", set: true, wantErr: "unexpected bare level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateAgentLogLevel(tc.raw, tc.set)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "invalid --agent-log-level value")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAgentLogLevelFlagRegistered(t *testing.T) {
	for _, c := range []struct {
		name string
		f    func(string) bool
	}{
		{"start", func(n string) bool { return startCmd.Flags().Lookup(n) != nil }},
		{"resume", func(n string) bool { return resumeCmd.Flags().Lookup(n) != nil }},
	} {
		assert.True(t, c.f("agent-log-level"), "--agent-log-level should be registered on %s", c.name)
	}
	f := startCmd.Flags().Lookup("agent-log-level")
	require.NotNil(t, f)
	assert.Equal(t, "string", f.Value.Type())
	assert.Equal(t, "", f.DefValue)
	assert.Contains(t, f.Usage, "SCION_LOG_LEVEL")
}

// The root --debug help must no longer claim agents get SCION_DEBUG.
func TestRootDebugHelpDoesNotPromiseAgentDebug(t *testing.T) {
	f := rootCmd.PersistentFlags().Lookup("debug")
	require.NotNil(t, f)
	assert.NotContains(t, f.Usage, "SCION_DEBUG")
	assert.Contains(t, f.Usage, "--agent-log-level")
}

// An invalid --agent-log-level is a usage error raised before any start work.
func TestRunAgentRejectsInvalidAgentLogLevel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Chdir(tmp)
	resetAgentLogLevelGlobals(t)

	for _, val := range []string{"verbose", ""} {
		t.Run("value="+val, func(t *testing.T) {
			f := startCmd.Flags().Lookup("agent-log-level")
			require.NotNil(t, f)
			t.Cleanup(func() {
				agentLogLevelFlag = ""
				f.Changed = false
			})
			require.NoError(t, startCmd.Flags().Set("agent-log-level", val))

			err := RunAgent(startCmd, []string{"agent-x"}, false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid --agent-log-level value")
			assert.True(t, isUsageError(err), "error %q should be a usage error", err)
		})
	}
}

// Local path: the initial agent env carries SCION_LOG_LEVEL only when
// --agent-log-level is given, and never anything from the CLI's --debug.
func TestLocalAgentStartEnv(t *testing.T) {
	t.Run("debug without flag adds nothing", func(t *testing.T) {
		resetAgentLogLevelGlobals(t)
		debugMode = true
		assert.Nil(t, localAgentStartEnv())
	})
	t.Run("flag sets SCION_LOG_LEVEL", func(t *testing.T) {
		resetAgentLogLevelGlobals(t)
		debugMode = true
		agentLogLevelFlag = " info,hubsync=debug "
		assert.Equal(t, map[string]string{"SCION_LOG_LEVEL": "info,hubsync=debug"}, localAgentStartEnv())
	})
}

// createConfigEnv returns config.env from the captured hub create body.
func createConfigEnv(t *testing.T, body map[string]interface{}) (map[string]interface{}, bool) {
	t.Helper()
	cfg, ok := body["config"].(map[string]interface{})
	if !ok {
		return nil, false
	}
	env, ok := cfg["env"].(map[string]interface{})
	return env, ok
}

// Hub path: --agent-log-level reaches the create request's config env, and
// the CLI's --debug alone adds neither SCION_DEBUG nor SCION_LOG_LEVEL.
func TestStartAgentViaHub_AgentLogLevel(t *testing.T) {
	const projectID, agentName = "proj-loglevel", "loglevel-agent"

	t.Run("debug without flag adds nothing", func(t *testing.T) {
		resetHubStartGlobals(t)
		resetAgentLogLevelGlobals(t)
		debugMode = true
		stub := newHubStartStub(t, projectID, agentName, "")
		var err error
		captureStdIO(t, func() {
			err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.NoError(t, err)
		require.Equal(t, 1, stub.createCalls)
		env, _ := createConfigEnv(t, stub.createBody)
		assert.NotContains(t, env, "SCION_DEBUG")
		assert.NotContains(t, env, "SCION_LOG_LEVEL")
	})

	t.Run("flag sets SCION_LOG_LEVEL", func(t *testing.T) {
		resetHubStartGlobals(t)
		resetAgentLogLevelGlobals(t)
		agentLogLevelFlag = "warn,hub.auth=debug"
		stub := newHubStartStub(t, projectID, agentName, "")
		var err error
		captureStdIO(t, func() {
			err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.NoError(t, err)
		require.Equal(t, 1, stub.createCalls)
		env, ok := createConfigEnv(t, stub.createBody)
		require.True(t, ok, "create body must carry config.env: %v", stub.createBody)
		assert.Equal(t, "warn,hub.auth=debug", env["SCION_LOG_LEVEL"])
		assert.NotContains(t, env, "SCION_DEBUG")
	})

	t.Run("flag overrides inline config and keeps its other env", func(t *testing.T) {
		resetHubStartGlobals(t)
		resetAgentLogLevelGlobals(t)
		agentLogLevelFlag = "debug"
		stub := newHubStartStub(t, projectID, agentName, "")
		inline := &api.ScionConfig{Env: map[string]string{"SCION_LOG_LEVEL": "error", "OTHER": "kept"}}
		var err error
		captureStdIO(t, func() {
			err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", false, inline)
		})
		require.NoError(t, err)
		env, ok := createConfigEnv(t, stub.createBody)
		require.True(t, ok)
		assert.Equal(t, "debug", env["SCION_LOG_LEVEL"])
		assert.Equal(t, "kept", env["OTHER"])
	})
}

// --agent-log-level is listed among the flags not applied when the hub
// reuses an existing agent, so the user is told it had no effect.
func TestStartAgentViaHub_AgentLogLevelWarnsForExistingAgent(t *testing.T) {
	const projectID, agentName = "proj-loglevel-existing", "loglevel-existing"
	resetHubStartGlobals(t)
	resetAgentLogLevelGlobals(t)

	stub := newHubStartStub(t, projectID, agentName, "suspended")
	cmd := newFlagCmd(t, nil)
	cmd.Flags().StringVar(&agentLogLevelFlag, "agent-log-level", "", "")
	require.NoError(t, cmd.Flags().Set("agent-log-level", "debug"))

	var err error
	_, stderr := captureStdIO(t, func() {
		err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
	})
	require.NoError(t, err)
	require.True(t, strings.Contains(stderr, "not applied to an existing agent"), "stderr: %s", stderr)
	assert.Contains(t, stderr, "--agent-log-level")
}
