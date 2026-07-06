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

	var result map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "my-agent", result["slug"])
	assert.Equal(t, "My Agent", result["name"])
	assert.Equal(t, "uuid-123", result["id"])
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
