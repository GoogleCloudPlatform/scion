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
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestWhoamiAgentContext(t *testing.T) {
	t.Setenv("SCION_AGENT_SLUG", "my-agent")
	t.Setenv("SCION_AGENT_NAME", "My Agent")
	t.Setenv("SCION_AGENT_ID", "uuid-123")

	cmd := whoamiCmd
	out := captureStdout(t, func() {
		err := cmd.RunE(cmd, nil)
		require.NoError(t, err)
	})
	assert.Equal(t, "my-agent\n", out)
}

func TestWhoamiAgentContextJSON(t *testing.T) {
	t.Setenv("SCION_AGENT_SLUG", "my-agent")
	t.Setenv("SCION_AGENT_NAME", "My Agent")
	t.Setenv("SCION_AGENT_ID", "uuid-123")

	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	cmd := whoamiCmd
	out := captureStdout(t, func() {
		err := cmd.RunE(cmd, nil)
		require.NoError(t, err)
	})

	var result WhoamiResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "my-agent", result.Slug)
	assert.Equal(t, "My Agent", result.Name)
	assert.Equal(t, "uuid-123", result.ID)
}

func TestWhoamiTier1FieldsJSON(t *testing.T) {
	// Set all Tier 1 env vars.
	t.Setenv("SCION_AGENT_SLUG", "dev-agent")
	t.Setenv("SCION_AGENT_NAME", "Dev Agent")
	t.Setenv("SCION_AGENT_ID", "agent-456")
	t.Setenv("SCION_PROJECT", "my-project")
	t.Setenv("SCION_PROJECT_ID", "proj-789")
	t.Setenv("SCION_TEMPLATE_NAME", "developer")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_MODEL", "sonnet")
	t.Setenv("SCION_CREATOR", "ptone")
	t.Setenv("SCION_BROKER_NAME", "my-broker")
	t.Setenv("SCION_BROKER_ID", "broker-001")
	t.Setenv("SCION_CLI_MODE", "non-interactive")
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")

	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	cmd := whoamiCmd
	out := captureStdout(t, func() {
		err := cmd.RunE(cmd, nil)
		require.NoError(t, err)
	})

	var result WhoamiResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))

	assert.Equal(t, "dev-agent", result.Slug)
	assert.Equal(t, "Dev Agent", result.Name)
	assert.Equal(t, "agent-456", result.ID)
	assert.Equal(t, "my-project", result.Project)
	assert.Equal(t, "proj-789", result.ProjectID)
	assert.Equal(t, "developer", result.Template)
	assert.Equal(t, "claude", result.Harness)
	assert.Equal(t, "sonnet", result.Model)
	assert.Equal(t, "ptone", result.Creator)
	assert.Equal(t, "my-broker", result.BrokerName)
	assert.Equal(t, "broker-001", result.BrokerID)
	assert.Equal(t, "non-interactive", result.CLIMode)
	assert.Equal(t, "https://hub.example.com", result.HubEndpoint)
	assert.Equal(t, "https://hub.example.com/agents/agent-456", result.HubURL)
}

func TestWhoamiOmitEmpty(t *testing.T) {
	// Set only slug and name — all other env vars absent.
	t.Setenv("SCION_AGENT_SLUG", "minimal-agent")
	t.Setenv("SCION_AGENT_NAME", "Minimal Agent")
	t.Setenv("SCION_AGENT_ID", "")

	// Clear all optional env vars explicitly.
	t.Setenv("SCION_PROJECT", "")
	t.Setenv("SCION_PROJECT_ID", "")
	t.Setenv("SCION_TEMPLATE_NAME", "")
	t.Setenv("SCION_HARNESS", "")
	t.Setenv("SCION_MODEL", "")
	t.Setenv("SCION_CREATOR", "")
	t.Setenv("SCION_BROKER_NAME", "")
	t.Setenv("SCION_BROKER_ID", "")
	t.Setenv("SCION_CLI_MODE", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")

	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	cmd := whoamiCmd
	out := captureStdout(t, func() {
		err := cmd.RunE(cmd, nil)
		require.NoError(t, err)
	})

	// Parse raw JSON to check key absence (omitempty).
	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &raw))

	// Required fields are always present.
	assert.Contains(t, raw, "slug")
	assert.Contains(t, raw, "name")
	assert.Contains(t, raw, "id")

	// Optional fields must be absent when env vars are empty.
	assert.NotContains(t, raw, "project")
	assert.NotContains(t, raw, "projectId")
	assert.NotContains(t, raw, "template")
	assert.NotContains(t, raw, "harness")
	assert.NotContains(t, raw, "model")
	assert.NotContains(t, raw, "creator")
	assert.NotContains(t, raw, "brokerName")
	assert.NotContains(t, raw, "brokerId")
	assert.NotContains(t, raw, "cliMode")
	assert.NotContains(t, raw, "hubEndpoint")
	assert.NotContains(t, raw, "hubUrl")
}

func TestWhoamiHubURL(t *testing.T) {
	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	t.Run("present when both endpoint and ID set", func(t *testing.T) {
		t.Setenv("SCION_AGENT_SLUG", "agent-a")
		t.Setenv("SCION_AGENT_NAME", "Agent A")
		t.Setenv("SCION_AGENT_ID", "id-abc")
		t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")

		cmd := whoamiCmd
		out := captureStdout(t, func() {
			err := cmd.RunE(cmd, nil)
			require.NoError(t, err)
		})

		var result WhoamiResult
		require.NoError(t, json.Unmarshal([]byte(out), &result))
		assert.Equal(t, "https://hub.example.com/agents/id-abc", result.HubURL)
	})

	t.Run("absent when endpoint missing", func(t *testing.T) {
		t.Setenv("SCION_AGENT_SLUG", "agent-b")
		t.Setenv("SCION_AGENT_NAME", "Agent B")
		t.Setenv("SCION_AGENT_ID", "id-def")
		t.Setenv("SCION_HUB_ENDPOINT", "")

		cmd := whoamiCmd
		out := captureStdout(t, func() {
			err := cmd.RunE(cmd, nil)
			require.NoError(t, err)
		})

		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(out), &raw))
		assert.NotContains(t, raw, "hubUrl")
	})

	t.Run("absent when ID missing", func(t *testing.T) {
		t.Setenv("SCION_AGENT_SLUG", "agent-c")
		t.Setenv("SCION_AGENT_NAME", "Agent C")
		t.Setenv("SCION_AGENT_ID", "")
		t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")

		cmd := whoamiCmd
		out := captureStdout(t, func() {
			err := cmd.RunE(cmd, nil)
			require.NoError(t, err)
		})

		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(out), &raw))
		assert.NotContains(t, raw, "hubUrl")
	})
}

func TestWhoamiNameOnly(t *testing.T) {
	t.Setenv("SCION_AGENT_SLUG", "")
	t.Setenv("SCION_AGENT_NAME", "fallback-agent")
	t.Setenv("SCION_AGENT_ID", "")

	cmd := whoamiCmd
	out := captureStdout(t, func() {
		err := cmd.RunE(cmd, nil)
		require.NoError(t, err)
	})
	assert.Equal(t, "fallback-agent\n", out)
}

func TestWhoamiNonAgent(t *testing.T) {
	t.Setenv("SCION_AGENT_SLUG", "")
	t.Setenv("SCION_AGENT_NAME", "")
	t.Setenv("SCION_AGENT_ID", "")

	cmd := whoamiCmd
	err := cmd.RunE(cmd, nil)
	// Should attempt system whoami — may succeed or fail depending on the environment,
	// but should not return agent identity.
	_ = err
}
