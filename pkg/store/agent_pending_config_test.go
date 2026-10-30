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

package store

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pendingWithSecrets() *AgentPendingConfig {
	return &AgentPendingConfig{
		Config: map[string]json.RawMessage{
			"model":     json.RawMessage(`"m"`),
			"env":       json.RawMessage(`{"API_KEY":"secret","GITHUB_TOKEN":"ghp","PLAIN":"x"}`),
			"telemetry": json.RawMessage(`{"enabled":true,"cloud":{"endpoint":"https://otel","headers":{"Authorization":"Bearer t"}}}`),
		},
		SavedBy:            "user:u1",
		EnvSaverCanViewEnv: true,
	}
}

func decodeObject(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func TestAgentPendingConfig_MarshalJSONFailsClosed(t *testing.T) {
	// Without ResponseView, a response never carries env or the telemetry
	// headers, and never the internal saver flag.
	b, err := json.Marshal(pendingWithSecrets())
	require.NoError(t, err)
	out := decodeObject(t, b)
	assert.NotContains(t, out, "envSaverCanViewEnv")
	cfg := decodeObject(t, out["config"])
	assert.NotContains(t, cfg, "env")
	assert.JSONEq(t, `"m"`, string(cfg["model"]))
	assert.JSONEq(t, `{"enabled":true,"cloud":{"endpoint":"https://otel"}}`, string(cfg["telemetry"]))
	assert.NotContains(t, string(b), "secret")
	assert.NotContains(t, string(b), "Bearer")
}

func TestAgentPendingConfig_ResponseView(t *testing.T) {
	p := pendingWithSecrets()

	t.Run("without attach", func(t *testing.T) {
		b, err := json.Marshal(p.ResponseView(false))
		require.NoError(t, err)
		cfg := decodeObject(t, decodeObject(t, b)["config"])
		assert.NotContains(t, cfg, "env")
		assert.NotContains(t, string(b), "Bearer")
	})
	t.Run("with attach", func(t *testing.T) {
		b, err := json.Marshal(p.ResponseView(true))
		require.NoError(t, err)
		cfg := decodeObject(t, decodeObject(t, b)["config"])
		assert.JSONEq(t, `{"API_KEY":"secret","PLAIN":"x"}`, string(cfg["env"]), "GITHUB_TOKEN is never returned")
		assert.Contains(t, string(cfg["telemetry"]), "Bearer t")
		assert.NotContains(t, decodeObject(t, b), "envSaverCanViewEnv")
	})
	t.Run("a stored null env is shown as null", func(t *testing.T) {
		pc := &AgentPendingConfig{Config: map[string]json.RawMessage{"env": json.RawMessage(`null`)}}
		b, err := json.Marshal(pc.ResponseView(true))
		require.NoError(t, err)
		assert.Equal(t, "null", string(decodeObject(t, decodeObject(t, b)["config"])["env"]))
	})
	t.Run("the view does not change the original", func(t *testing.T) {
		_ = p.ResponseView(true)
		assert.JSONEq(t, `{"API_KEY":"secret","GITHUB_TOKEN":"ghp","PLAIN":"x"}`, string(p.Config["env"]))
	})
	assert.Nil(t, (*AgentPendingConfig)(nil).ResponseView(true))
}

func TestAgentPendingConfig_StorageRoundTrip(t *testing.T) {
	raw, err := MarshalPendingConfigForStorage(pendingWithSecrets())
	require.NoError(t, err)
	assert.Contains(t, raw, "secret", "storage keeps env values")
	assert.Contains(t, raw, "GITHUB_TOKEN")
	got, err := UnmarshalPendingConfigFromStorage(raw)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.EnvSaverCanViewEnv)
	assert.JSONEq(t, string(pendingWithSecrets().Config["env"]), string(got.Config["env"]))

	empty, err := MarshalPendingConfigForStorage(&AgentPendingConfig{SavedBy: "user:u1"})
	require.NoError(t, err)
	assert.Equal(t, "", empty, "a pending config without keys clears the column")
	none, err := UnmarshalPendingConfigFromStorage("")
	require.NoError(t, err)
	assert.Nil(t, none)
	_, err = UnmarshalPendingConfigFromStorage("{not json")
	assert.Error(t, err)
}

func TestAgentPendingConfig_CloneIsDeep(t *testing.T) {
	p := pendingWithSecrets()
	c := p.Clone()
	c.Config["model"][1] = 'X'
	delete(c.Config, "env")
	assert.JSONEq(t, `"m"`, string(p.Config["model"]))
	assert.Contains(t, p.Config, "env")
	assert.Nil(t, (*AgentPendingConfig)(nil).Clone())
	assert.True(t, (*AgentPendingConfig)(nil).IsEmpty())
}

// TestAgentPendingConfig_RedactionIgnoresKeyCase: a stored key of another
// spelling (encoding/json decodes keys case-insensitively) is redacted like
// its json key, at every depth.
func TestAgentPendingConfig_RedactionIgnoresKeyCase(t *testing.T) {
	p := &AgentPendingConfig{Config: map[string]json.RawMessage{
		"Telemetry": json.RawMessage(`{"Cloud":{"HEADERS":{"Authorization":"Bearer secret"},"endpoint":"https://otel"}}`),
		"ENV":       json.RawMessage(`{"API_KEY":"secret","GITHUB_TOKEN":"ghp"}`),
	}}
	b, err := json.Marshal(p.ResponseView(false))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "secret")
	assert.Contains(t, string(b), "https://otel", "the rest of the telemetry object is kept")

	b, err = json.Marshal(p.ResponseView(true))
	require.NoError(t, err)
	assert.Contains(t, string(b), "Bearer secret")
	assert.NotContains(t, string(b), "ghp", "GITHUB_TOKEN is never returned, whatever the env key's case")
}

// TestAgentPendingConfig_RedactsMCPServerAndServiceEnv: like the applied
// inline config, a pending MCP server's env and headers and a pending
// service's env are returned only to a viewer who may attach; the servers
// and services themselves are listed for everyone.
func TestAgentPendingConfig_RedactsMCPServerAndServiceEnv(t *testing.T) {
	p := &AgentPendingConfig{Config: map[string]json.RawMessage{
		"mcp_servers": json.RawMessage(`{"docs":{"transport":"http","url":"https://mcp.example","Headers":{"Authorization":"Bearer mcp-secret"},"env":{"TOKEN":"mcp-env-secret"}},"gone":null}`),
		"services":    json.RawMessage(`[{"name":"db","command":["run"],"ENV":{"PASSWORD":"svc-secret"}},null]`),
	}}
	b, err := json.Marshal(p.ResponseView(false))
	require.NoError(t, err)
	out := string(b)
	assert.NotContains(t, out, "mcp-secret")
	assert.NotContains(t, out, "mcp-env-secret")
	assert.NotContains(t, out, "svc-secret")
	assert.Contains(t, out, "https://mcp.example", "the server is still listed")
	assert.Contains(t, out, `"name":"db"`, "the service is still listed")

	b, err = json.Marshal(p.ResponseView(true))
	require.NoError(t, err)
	out = string(b)
	assert.Contains(t, out, "mcp-secret")
	assert.Contains(t, out, "mcp-env-secret")
	assert.Contains(t, out, "svc-secret")

	malformed := &AgentPendingConfig{Config: map[string]json.RawMessage{
		"mcp_servers": json.RawMessage(`["not","a","map"]`),
		"services":    json.RawMessage(`{"not":"a list"}`),
	}}
	b, err = json.Marshal(malformed.ResponseView(false))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "mcp_servers", "a malformed value is dropped (fails closed)")
	assert.NotContains(t, string(b), "services")
}

// TestAgentPendingConfig_LimitsFormat: a row stored before the count limits
// were typed (no limitsFormat) held a clear as 0, so loading it reads a
// count-limit 0 as null, in any key case; a typed row keeps 0 (Unlimited)
// and null apart. Every stored row is written as typed, a fresh in-memory
// row included, and the API view never shows the format (ptone/scion#5605).
func TestAgentPendingConfig_LimitsFormat(t *testing.T) {
	legacy := `{"config":{"max_turns":0,"Max_Model_Calls":0,"model":"m","max_duration":"0"},"savedAt":"2026-10-01T00:00:00Z","baseGeneration":1,"baseStateVersion":3}`
	got, err := UnmarshalPendingConfigFromStorage(legacy)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, PendingLimitsFormatTyped, got.LimitsFormat)
	assert.Equal(t, "null", string(got.Config["max_turns"]))
	assert.Equal(t, "null", string(got.Config["Max_Model_Calls"]))
	assert.Equal(t, `"m"`, string(got.Config["model"]))
	assert.Equal(t, `"0"`, string(got.Config["max_duration"]), "only the count limits are normalized")

	typed := `{"config":{"max_turns":0,"max_model_calls":null},"limitsFormat":2,"savedAt":"2026-10-01T00:00:00Z","baseGeneration":1,"baseStateVersion":3}`
	got, err = UnmarshalPendingConfigFromStorage(typed)
	require.NoError(t, err)
	assert.Equal(t, "0", string(got.Config["max_turns"]), "a typed 0 is Unlimited and is kept")
	assert.Equal(t, "null", string(got.Config["max_model_calls"]))

	// A fresh row built in memory carries no format; storage stamps it, so
	// a reload keeps its 0 as Unlimited.
	fresh := &AgentPendingConfig{Config: map[string]json.RawMessage{"max_turns": json.RawMessage("0")}}
	raw, err := MarshalPendingConfigForStorage(fresh)
	require.NoError(t, err)
	assert.Contains(t, raw, `"limitsFormat":2`)
	assert.Zero(t, fresh.LimitsFormat, "storage does not edit the caller's value")
	got, err = UnmarshalPendingConfigFromStorage(raw)
	require.NoError(t, err)
	assert.Equal(t, "0", string(got.Config["max_turns"]))

	// A normalized legacy row, written again, is stored with null.
	got, err = UnmarshalPendingConfigFromStorage(legacy)
	require.NoError(t, err)
	raw, err = MarshalPendingConfigForStorage(got)
	require.NoError(t, err)
	assert.Contains(t, raw, `"max_turns":null`)

	view, err := json.Marshal(got.ResponseView(true))
	require.NoError(t, err)
	assert.NotContains(t, string(view), "limitsFormat")
}
