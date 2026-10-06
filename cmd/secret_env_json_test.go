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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonModes lists the two ways to ask for JSON output.
var jsonModes = []struct {
	name     string
	format   string
	jsonFlag bool
}{
	{name: "json flag", jsonFlag: true},
	{name: "format json", format: "json"},
}

// newJSONRoutesServer serves fixed JSON bodies by path, plus /healthz.
func newJSONRoutesServer(t *testing.T, routes map[string]interface{}) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server
}

// setupJSONCmdTest saves package state and points the CLI at server.
func setupJSONCmdTest(t *testing.T, routes map[string]interface{}) {
	t.Helper()
	secretState := saveSecretTestState()
	envState := saveEnvTestState()
	origFormat := outputFormat
	t.Cleanup(func() {
		secretState.restore()
		envState.restore()
		outputFormat = origFormat
	})

	server := newJSONRoutesServer(t, routes)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	projectPath = setupSecretProject(t, tmpHome, server.URL)

	origCtx := agentSecretListCmd.Context()
	t.Cleanup(func() { agentSecretListCmd.SetContext(origCtx) })
	agentSecretListCmd.SetContext(context.Background())

	outputFormat = ""
	secretOutputJSON, secretProjectScope, secretBrokerScope, secretScope = false, "", "", ""
	envOutputJSON, envProjectScope, envBrokerScope, envScope = false, "", "", ""
}

func decodeJSONObject(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &got), "output must be valid JSON: %q", out)
	return got
}

func TestWantJSON(t *testing.T) {
	orig := outputFormat
	t.Cleanup(func() { outputFormat = orig })

	outputFormat = ""
	assert.False(t, wantJSON(false))
	assert.True(t, wantJSON(true))
	outputFormat = "json"
	assert.True(t, wantJSON(false))
}

func TestRunAgentSecretList_FormatJSON(t *testing.T) {
	setupJSONCmdTest(t, map[string]interface{}{
		"/api/v1/secrets": map[string]interface{}{"secrets": secretListFixture(), "scope": "project"},
	})
	outputFormat = "json"

	out := captureStdout(t, func() {
		require.NoError(t, runAgentSecretList(agentSecretListCmd, nil))
	})
	assert.NotContains(t, out, "do-not-print")

	got := decodeJSONObject(t, out)
	assert.Equal(t, "project", got["scope"])
	items, ok := got["secrets"].([]interface{})
	require.True(t, ok, "secrets must be an array")
	require.Len(t, items, 2)
	first := items[0].(map[string]interface{})
	assert.ElementsMatch(t, []string{"key", "type", "allowProgeny", "version", "updated"}, mapKeys(first))
	assert.Equal(t, "API_KEY", first["key"])
	assert.Equal(t, "environment", items[1].(map[string]interface{})["type"])
}

func TestRunAgentSecretList_FormatJSONEmpty(t *testing.T) {
	setupJSONCmdTest(t, map[string]interface{}{
		"/api/v1/secrets": map[string]interface{}{"secrets": []interface{}{}, "scope": "project"},
	})
	outputFormat = "json"

	out := captureStdout(t, func() {
		require.NoError(t, runAgentSecretList(agentSecretListCmd, nil))
	})
	got := decodeJSONObject(t, out)
	assert.Equal(t, []interface{}{}, got["secrets"], "empty list must encode as []")
}

func TestRunSecretGet_JSON(t *testing.T) {
	for _, mode := range jsonModes {
		t.Run(mode.name, func(t *testing.T) {
			setupJSONCmdTest(t, map[string]interface{}{
				"/api/v1/secrets/API_KEY": secretListFixture()[0],
			})
			outputFormat = mode.format
			secretOutputJSON = mode.jsonFlag

			out := captureStdout(t, func() {
				require.NoError(t, runSecretGet(hubSecretGetCmd, []string{"API_KEY"}))
			})
			assert.NotContains(t, out, "Secret: API_KEY", "JSON output must not include the text form")
			assert.NotContains(t, out, "do-not-print")
			got := decodeJSONObject(t, out)
			assert.Equal(t, "API_KEY", got["key"])
			assert.Equal(t, float64(3), got["version"])
		})
	}
}

func TestRunSecretGet_NoKeyEmptyJSON(t *testing.T) {
	for _, mode := range jsonModes {
		t.Run(mode.name, func(t *testing.T) {
			setupJSONCmdTest(t, map[string]interface{}{
				"/api/v1/secrets": map[string]interface{}{"secrets": []interface{}{}, "scope": "user"},
			})
			outputFormat = mode.format
			secretOutputJSON = mode.jsonFlag

			out := captureStdout(t, func() {
				require.NoError(t, runSecretGet(hubSecretGetCmd, nil))
			})
			got := decodeJSONObject(t, out)
			assert.Equal(t, []interface{}{}, got["secrets"])
		})
	}
}

func envJSONFixture() []map[string]interface{} {
	return []map[string]interface{}{
		{"id": "e1", "key": "LOG_LEVEL", "value": "debug", "scope": "user", "scopeId": "u1", "description": "log verbosity", "injectionMode": "as_needed", "created": "2026-01-01T00:00:00Z", "updated": "2026-01-02T00:00:00Z", "createdBy": "user-1"},
		{"id": "e2", "key": "TOKEN", "value": "hidden-value", "scope": "user", "sensitive": true, "secret": true},
	}
}

func TestRunEnvGet_JSON(t *testing.T) {
	for _, mode := range jsonModes {
		t.Run(mode.name, func(t *testing.T) {
			setupJSONCmdTest(t, map[string]interface{}{
				"/api/v1/env/LOG_LEVEL": envJSONFixture()[0],
				"/api/v1/env/TOKEN":     envJSONFixture()[1],
			})
			outputFormat = mode.format
			envOutputJSON = mode.jsonFlag

			out := captureStdout(t, func() {
				require.NoError(t, runEnvGet(hubEnvGetCmd, []string{"LOG_LEVEL"}))
			})
			got := decodeJSONObject(t, out)
			assert.Equal(t, map[string]interface{}{
				"id": "e1", "key": "LOG_LEVEL", "value": "debug", "scope": "user",
				"scopeId": "u1", "description": "log verbosity",
				"injectionMode": "as_needed", "createdBy": "user-1",
				"created": "2026-01-01T00:00:00Z", "updated": "2026-01-02T00:00:00Z",
			}, got)

			out = captureStdout(t, func() {
				require.NoError(t, runEnvGet(hubEnvGetCmd, []string{"TOKEN"}))
			})
			assert.NotContains(t, out, "hidden-value")
			got = decodeJSONObject(t, out)
			assert.NotContains(t, got, "value", "sensitive values are not shown, as in the text form")
			assert.Equal(t, true, got["sensitive"])
			assert.Equal(t, true, got["secret"])
		})
	}
}

func TestRunEnvList_JSONModes(t *testing.T) {
	for _, mode := range jsonModes {
		t.Run(mode.name, func(t *testing.T) {
			setupJSONCmdTest(t, map[string]interface{}{
				"/api/v1/env": map[string]interface{}{"envVars": envJSONFixture(), "scope": "user", "scopeId": "u1"},
			})
			outputFormat = mode.format
			envOutputJSON = mode.jsonFlag

			out := captureStdout(t, func() {
				require.NoError(t, runEnvList(hubEnvListCmd, nil))
			})
			assert.NotContains(t, out, "Environment variables (scope:")
			assert.NotContains(t, out, "hidden-value")

			got := decodeJSONObject(t, out)
			assert.ElementsMatch(t, []string{"envVars", "scope", "scopeId"}, mapKeys(got))
			assert.Equal(t, "user", got["scope"])
			assert.Equal(t, "u1", got["scopeId"])
			items, ok := got["envVars"].([]interface{})
			require.True(t, ok, "envVars must be an array")
			require.Len(t, items, 2)
			first := items[0].(map[string]interface{})
			assert.ElementsMatch(t, []string{
				"id", "key", "value", "scope", "scopeId", "description",
				"injectionMode", "created", "updated", "createdBy",
			}, mapKeys(first), "false flags are left out, as in the Hub record")
			assert.Equal(t, "debug", first["value"])
			second := items[1].(map[string]interface{})
			assert.NotContains(t, second, "value")
			assert.Equal(t, true, second["sensitive"])
			assert.Equal(t, true, second["secret"])
			assert.Equal(t, "e2", second["id"])
		})
	}
}

func TestRunEnvList_JSONModesEmpty(t *testing.T) {
	for _, mode := range jsonModes {
		t.Run(mode.name, func(t *testing.T) {
			setupJSONCmdTest(t, map[string]interface{}{
				"/api/v1/env": map[string]interface{}{"envVars": []interface{}{}, "scope": "user"},
			})
			outputFormat = mode.format
			envOutputJSON = mode.jsonFlag

			out := captureStdout(t, func() {
				require.NoError(t, runEnvList(hubEnvListCmd, nil))
			})
			got := decodeJSONObject(t, out)
			assert.Equal(t, "user", got["scope"])
			require.Contains(t, got, "scopeId", "scopeId is kept even when empty, as in the Hub response")
			assert.Equal(t, "", got["scopeId"])
			assert.Equal(t, []interface{}{}, got["envVars"], "empty list must encode as []")
		})
	}
}

func TestRunEnvList_JSONScopeFromResponse(t *testing.T) {
	for _, mode := range jsonModes {
		t.Run(mode.name, func(t *testing.T) {
			setupJSONCmdTest(t, map[string]interface{}{
				"/api/v1/env": map[string]interface{}{"envVars": []interface{}{}, "scope": "hub", "scopeId": "h1"},
			})
			outputFormat = mode.format
			envOutputJSON = mode.jsonFlag

			out := captureStdout(t, func() {
				require.NoError(t, runEnvList(hubEnvListCmd, nil))
			})
			got := decodeJSONObject(t, out)
			assert.Equal(t, "hub", got["scope"], "the scope in the Hub response wins over the command scope")
			assert.Equal(t, "h1", got["scopeId"])
		})
	}
}

func TestRunEnvGet_NoKeyEmptyJSON(t *testing.T) {
	for _, mode := range jsonModes {
		t.Run(mode.name, func(t *testing.T) {
			setupJSONCmdTest(t, map[string]interface{}{
				"/api/v1/env": map[string]interface{}{"envVars": []interface{}{}, "scope": "user"},
			})
			outputFormat = mode.format
			envOutputJSON = mode.jsonFlag

			out := captureStdout(t, func() {
				require.NoError(t, runEnvGet(hubEnvGetCmd, nil))
			})
			got := decodeJSONObject(t, out)
			assert.Equal(t, []interface{}{}, got["envVars"])
		})
	}
}

func TestRunEnvGet_JSONEmptyValue(t *testing.T) {
	for _, mode := range jsonModes {
		t.Run(mode.name, func(t *testing.T) {
			setupJSONCmdTest(t, map[string]interface{}{
				"/api/v1/env/EMPTY": map[string]interface{}{"id": "e3", "key": "EMPTY", "value": "", "scope": "user"},
				"/api/v1/env": map[string]interface{}{"envVars": []interface{}{
					map[string]interface{}{"id": "e3", "key": "EMPTY", "value": "", "scope": "user"},
				}, "scope": "user"},
			})
			outputFormat = mode.format
			envOutputJSON = mode.jsonFlag

			out := captureStdout(t, func() {
				require.NoError(t, runEnvGet(hubEnvGetCmd, []string{"EMPTY"}))
			})
			got := decodeJSONObject(t, out)
			require.Contains(t, got, "value", "an empty value of a plain variable is still shown")
			assert.Equal(t, "", got["value"])

			out = captureStdout(t, func() {
				require.NoError(t, runEnvList(hubEnvListCmd, nil))
			})
			got = decodeJSONObject(t, out)
			items, ok := got["envVars"].([]interface{})
			require.True(t, ok, "envVars must be an array")
			require.Len(t, items, 1)
			item := items[0].(map[string]interface{})
			require.Contains(t, item, "value")
			assert.Equal(t, "", item["value"])
		})
	}
}
