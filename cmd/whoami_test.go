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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
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

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stderr
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
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

func TestWhoamiFull(t *testing.T) {
	// Set up a mock Hub server.
	agentResp := map[string]interface{}{
		"id":          "agent-full-id",
		"slug":        "full-agent",
		"name":        "Full Agent",
		"phase":       "running",
		"activity":    "working",
		"labels":      map[string]string{"env": "prod", "team": "infra"},
		"annotations": map[string]string{"note": "test annotation"},
		"ancestry":    []string{"user-root", "parent-agent"},
		"taskSummary": "Implementing feature X",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.Path, "/api/v1/agents/agent-full-id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentResp)
	}))
	defer srv.Close()

	// Set env vars.
	t.Setenv("SCION_AGENT_SLUG", "full-agent")
	t.Setenv("SCION_AGENT_NAME", "Full Agent")
	t.Setenv("SCION_AGENT_ID", "agent-full-id")
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)

	// Override the Hub client factory with one pointing at our test server.
	cleanup := sciontoolhub.SetHubTestSandboxed()
	defer cleanup()
	origFactory := newHubClient
	newHubClient = func() *sciontoolhub.Client {
		return sciontoolhub.NewClientWithConfig(srv.URL, "test-token", "agent-full-id")
	}
	defer func() { newHubClient = origFactory }()

	// Save and restore the --full flag.
	oldFull := whoamiFull
	whoamiFull = true
	defer func() { whoamiFull = oldFull }()

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

	// Tier 1 fields.
	assert.Equal(t, "full-agent", result.Slug)
	assert.Equal(t, "Full Agent", result.Name)
	assert.Equal(t, "agent-full-id", result.ID)
	assert.Equal(t, fmt.Sprintf("%s/agents/agent-full-id", srv.URL), result.HubURL)

	// Tier 2 fields from Hub API.
	assert.Equal(t, "running", result.Phase)
	assert.Equal(t, "working", result.Activity)
	assert.Equal(t, map[string]string{"env": "prod", "team": "infra"}, result.Labels)
	assert.Equal(t, map[string]string{"note": "test annotation"}, result.Annotations)
	assert.Equal(t, []string{"user-root", "parent-agent"}, result.Ancestry)
	assert.Equal(t, "Implementing feature X", result.TaskSummary)
}

func TestWhoamiFullNoHub(t *testing.T) {
	t.Setenv("SCION_AGENT_SLUG", "nohub-agent")
	t.Setenv("SCION_AGENT_NAME", "NoHub Agent")
	t.Setenv("SCION_AGENT_ID", "agent-nohub-id")
	// No Hub endpoint set — client will be nil.
	t.Setenv("SCION_HUB_ENDPOINT", "")

	// Override the Hub client factory to return nil (no Hub).
	origFactory := newHubClient
	newHubClient = func() *sciontoolhub.Client {
		return nil
	}
	defer func() { newHubClient = origFactory }()

	oldFull := whoamiFull
	whoamiFull = true
	defer func() { whoamiFull = oldFull }()

	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	cmd := whoamiCmd

	var stderr string
	out := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			err := cmd.RunE(cmd, nil)
			require.NoError(t, err) // Must exit 0 even without Hub.
		})
	})

	// Verify stderr warning.
	assert.Contains(t, stderr, "Hub not available")

	// Verify Tier 1 fields are still present.
	var result WhoamiResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "nohub-agent", result.Slug)
	assert.Equal(t, "NoHub Agent", result.Name)
	assert.Equal(t, "agent-nohub-id", result.ID)

	// Verify Tier 2 fields are absent.
	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &raw))
	assert.NotContains(t, raw, "phase")
	assert.NotContains(t, raw, "activity")
	assert.NotContains(t, raw, "labels")
	assert.NotContains(t, raw, "annotations")
	assert.NotContains(t, raw, "ancestry")
	assert.NotContains(t, raw, "taskSummary")
}

func TestWhoamiFullPlainText(t *testing.T) {
	t.Setenv("SCION_AGENT_SLUG", "text-agent")
	t.Setenv("SCION_AGENT_NAME", "Text Agent")
	t.Setenv("SCION_AGENT_ID", "agent-text-id")
	t.Setenv("SCION_PROJECT", "my-project")
	t.Setenv("SCION_TEMPLATE_NAME", "developer")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_MODEL", "sonnet")
	t.Setenv("SCION_CREATOR", "ptone")
	t.Setenv("SCION_BROKER_NAME", "my-broker")
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")

	// Override the Hub client factory to return nil (skip Hub enrichment for this test).
	origFactory := newHubClient
	newHubClient = func() *sciontoolhub.Client {
		return nil
	}
	defer func() { newHubClient = origFactory }()

	oldFull := whoamiFull
	whoamiFull = true
	defer func() { whoamiFull = oldFull }()

	// Plain text (not JSON).
	oldFormat := outputFormat
	outputFormat = ""
	defer func() { outputFormat = oldFormat }()

	cmd := whoamiCmd
	out := captureStdout(t, func() {
		_ = captureStderr(t, func() {
			err := cmd.RunE(cmd, nil)
			require.NoError(t, err)
		})
	})

	assert.Contains(t, out, "Agent:    text-agent (Text Agent)")
	assert.Contains(t, out, "ID:       agent-text-id")
	assert.Contains(t, out, "Project:  my-project")
	assert.Contains(t, out, "Template: developer")
	assert.Contains(t, out, "Harness:  claude")
	assert.Contains(t, out, "Model:    sonnet")
	assert.Contains(t, out, "Creator:  ptone")
	assert.Contains(t, out, "Broker:   my-broker")
	assert.Contains(t, out, "Hub:      https://hub.example.com/agents/agent-text-id")

	// Verify plain-text does not contain JSON braces.
	assert.False(t, strings.HasPrefix(strings.TrimSpace(out), "{"))
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
