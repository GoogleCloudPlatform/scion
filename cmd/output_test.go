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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsJSONOutput(t *testing.T) {
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()

	outputFormat = "json"
	assert.True(t, isJSONOutput())

	outputFormat = "plain"
	assert.False(t, isJSONOutput())

	outputFormat = ""
	assert.False(t, isJSONOutput())
}

func TestOutputJSON(t *testing.T) {
	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	data := map[string]string{"key": "value"}
	err := outputJSON(data)
	require.NoError(t, err)

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Verify it's valid JSON
	var parsed map[string]string
	err = json.Unmarshal([]byte(output), &parsed)
	require.NoError(t, err)
	assert.Equal(t, "value", parsed["key"])
}

func TestOutputActionResult(t *testing.T) {
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()

	t.Run("json mode outputs JSON", func(t *testing.T) {
		outputFormat = "json"

		// Capture stdout
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		result := ActionResult{
			Status:  "success",
			Command: "test",
			Agent:   "test-agent",
			Message: "test message",
		}
		err := outputActionResult(result)
		require.NoError(t, err)

		w.Close()
		os.Stdout = oldStdout

		var buf bytes.Buffer
		buf.ReadFrom(r)
		output := buf.String()

		var parsed ActionResult
		err = json.Unmarshal([]byte(output), &parsed)
		require.NoError(t, err)
		assert.Equal(t, "success", parsed.Status)
		assert.Equal(t, "test", parsed.Command)
		assert.Equal(t, "test-agent", parsed.Agent)
		assert.Equal(t, "test message", parsed.Message)
	})

	t.Run("plain mode outputs message to stdout and warnings to stderr", func(t *testing.T) {
		outputFormat = "plain"

		// Capture stdout
		oldStdout := os.Stdout
		stdoutR, stdoutW, _ := os.Pipe()
		os.Stdout = stdoutW

		// Capture stderr
		oldStderr := os.Stderr
		stderrR, stderrW, _ := os.Pipe()
		os.Stderr = stderrW

		result := ActionResult{
			Status:   "success",
			Command:  "test",
			Message:  "test message",
			Warnings: []string{"warning 1"},
		}
		err := outputActionResult(result)
		require.NoError(t, err)

		stdoutW.Close()
		stderrW.Close()
		os.Stdout = oldStdout
		os.Stderr = oldStderr

		var stdoutBuf, stderrBuf bytes.Buffer
		stdoutBuf.ReadFrom(stdoutR)
		stderrBuf.ReadFrom(stderrR)

		assert.Contains(t, stdoutBuf.String(), "test message")
		assert.NotContains(t, stdoutBuf.String(), "Warning:")
		assert.Contains(t, stderrBuf.String(), "Warning: warning 1")
	})
}

func TestStatusfWritesToStderr(t *testing.T) {
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()

	t.Run("plain mode writes to stderr not stdout", func(t *testing.T) {
		outputFormat = "plain"

		oldStdout := os.Stdout
		stdoutR, stdoutW, _ := os.Pipe()
		os.Stdout = stdoutW

		oldStderr := os.Stderr
		stderrR, stderrW, _ := os.Pipe()
		os.Stderr = stderrW

		statusf("hello %s\n", "world")
		statusln("goodbye")

		stdoutW.Close()
		stderrW.Close()
		os.Stdout = oldStdout
		os.Stderr = oldStderr

		var stdoutBuf, stderrBuf bytes.Buffer
		stdoutBuf.ReadFrom(stdoutR)
		stderrBuf.ReadFrom(stderrR)

		assert.Empty(t, stdoutBuf.String(), "statusf/statusln should not write to stdout")
		assert.Contains(t, stderrBuf.String(), "hello world")
		assert.Contains(t, stderrBuf.String(), "goodbye")
	})

	t.Run("json mode suppresses output entirely", func(t *testing.T) {
		outputFormat = "json"

		oldStdout := os.Stdout
		stdoutR, stdoutW, _ := os.Pipe()
		os.Stdout = stdoutW

		oldStderr := os.Stderr
		stderrR, stderrW, _ := os.Pipe()
		os.Stderr = stderrW

		statusf("should not appear\n")
		statusln("also hidden")

		stdoutW.Close()
		stderrW.Close()
		os.Stdout = oldStdout
		os.Stderr = oldStderr

		var stdoutBuf, stderrBuf bytes.Buffer
		stdoutBuf.ReadFrom(stdoutR)
		stderrBuf.ReadFrom(stderrR)

		assert.Empty(t, stdoutBuf.String(), "statusf/statusln should not write to stdout in JSON mode")
		assert.Empty(t, stderrBuf.String(), "statusf/statusln should not write to stderr in JSON mode")
	})
}

func TestPrintUsingHubWritesToStderr(t *testing.T) {
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()

	t.Run("plain mode writes to stderr", func(t *testing.T) {
		outputFormat = "plain"

		oldStdout := os.Stdout
		stdoutR, stdoutW, _ := os.Pipe()
		os.Stdout = stdoutW

		oldStderr := os.Stderr
		stderrR, stderrW, _ := os.Pipe()
		os.Stderr = stderrW

		PrintUsingHub("http://localhost:8080")

		stdoutW.Close()
		stderrW.Close()
		os.Stdout = oldStdout
		os.Stderr = oldStderr

		var stdoutBuf, stderrBuf bytes.Buffer
		stdoutBuf.ReadFrom(stdoutR)
		stderrBuf.ReadFrom(stderrR)

		assert.Empty(t, stdoutBuf.String(), "PrintUsingHub should not write to stdout")
		assert.Contains(t, stderrBuf.String(), "Using hub: http://localhost:8080")
	})

	t.Run("json mode suppresses output", func(t *testing.T) {
		outputFormat = "json"

		oldStdout := os.Stdout
		stdoutR, stdoutW, _ := os.Pipe()
		os.Stdout = stdoutW

		oldStderr := os.Stderr
		stderrR, stderrW, _ := os.Pipe()
		os.Stderr = stderrW

		PrintUsingHub("http://localhost:8080")

		stdoutW.Close()
		stderrW.Close()
		os.Stdout = oldStdout
		os.Stderr = oldStderr

		var stdoutBuf, stderrBuf bytes.Buffer
		stdoutBuf.ReadFrom(stdoutR)
		stderrBuf.ReadFrom(stderrR)

		assert.Empty(t, stdoutBuf.String(), "PrintUsingHub should not write to stdout in JSON mode")
		assert.Empty(t, stderrBuf.String(), "PrintUsingHub should not write to stderr in JSON mode")
	})
}

func TestInteractiveOnlyCommands(t *testing.T) {
	// Verify key interactive commands are in the map
	expectedCommands := []string{
		"scion attach",
		"scion logs",
		"scion runtime-broker start",
		"scion runtime-broker stop",
		"scion broker start",
		"scion broker stop",
		"scion server start",
		"scion cdw",
		"scion message",
	}

	for _, cmd := range expectedCommands {
		reason, ok := interactiveOnlyCommands[cmd]
		assert.True(t, ok, "command %q should be in interactiveOnlyCommands", cmd)
		assert.NotEmpty(t, reason, "reason for %q should not be empty", cmd)
	}

	// Verify non-interactive commands are NOT in the map
	nonInteractiveCmds := []string{
		"scion list",
		"scion version",
		"scion config list",
		"scion create",
		"scion start",
		"scion stop",
		"scion delete",
		"scion look",
	}

	for _, cmd := range nonInteractiveCmds {
		_, ok := interactiveOnlyCommands[cmd]
		assert.False(t, ok, "command %q should NOT be in interactiveOnlyCommands", cmd)
	}
}

func TestJSONNoOpCommands(t *testing.T) {
	// Verify expected commands are in the no-op map
	assert.True(t, jsonNoOpCommands["scion look"], "scion look should be in jsonNoOpCommands")

	// Verify no-op commands are not also in the interactive-only map
	for cmd := range jsonNoOpCommands {
		_, ok := interactiveOnlyCommands[cmd]
		assert.False(t, ok, "command %q should not be in both jsonNoOpCommands and interactiveOnlyCommands", cmd)
	}
}

func TestActionResultJSONSerialization(t *testing.T) {
	result := ActionResult{
		Status:   "success",
		Command:  "create",
		Agent:    "my-agent",
		Message:  "Agent created",
		Warnings: []string{"low disk"},
		Details: map[string]interface{}{
			"slug":   "my-agent-123",
			"status": "provisioned",
		},
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	var parsed map[string]interface{}
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)

	assert.Equal(t, "success", parsed["status"])
	assert.Equal(t, "create", parsed["command"])
	assert.Equal(t, "my-agent", parsed["agent"])
	assert.Equal(t, "Agent created", parsed["message"])

	warnings, ok := parsed["warnings"].([]interface{})
	require.True(t, ok)
	assert.Len(t, warnings, 1)
	assert.Equal(t, "low disk", warnings[0])

	details, ok := parsed["details"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "my-agent-123", details["slug"])
}

func TestActionResultOmitsEmptyFields(t *testing.T) {
	result := ActionResult{
		Status:  "success",
		Command: "clean",
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	var parsed map[string]interface{}
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)

	// These should be present
	assert.Contains(t, parsed, "status")
	assert.Contains(t, parsed, "command")

	// These should be omitted (omitempty)
	assert.NotContains(t, parsed, "agent")
	assert.NotContains(t, parsed, "message")
	assert.NotContains(t, parsed, "warnings")
	assert.NotContains(t, parsed, "details")
}
