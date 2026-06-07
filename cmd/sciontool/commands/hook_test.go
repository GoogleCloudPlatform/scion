/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scrubScionEnv clears all Hub and telemetry environment variables for the
// duration of the test, preventing accidental communication with a real Hub
// or telemetry backend when tests run inside an agent container.
func scrubScionEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SCION_HUB_ENDPOINT",
		"SCION_HUB_URL",
		"SCION_AUTH_TOKEN",
		"SCION_AGENT_ID",
		"SCION_AGENT_MODE",
		"SCION_TELEMETRY_ENABLED",
		"SCION_TELEMETRY_CLOUD_ENABLED",
		"SCION_OTEL_ENDPOINT",
		"SCION_OTEL_GCP_CREDENTIALS",
		"SCION_GCP_PROJECT_ID",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
	} {
		t.Setenv(key, "")
	}
}

func TestProcessHookData_Claude(t *testing.T) {
	// Set up temp home directory for status/log files
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)
	scrubScionEnv(t)
	log.SetLogPath(filepath.Join(tmpDir, "agent.log"))

	hookDialect = "claude"

	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
	}
	jsonData, _ := json.Marshal(data)

	err := processHookData(jsonData)
	require.NoError(t, err)

	// Verify status file was created
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	statusData, err := os.ReadFile(statusPath)
	require.NoError(t, err)

	var status map[string]interface{}
	err = json.Unmarshal(statusData, &status)
	require.NoError(t, err)
	assert.Equal(t, "executing", status["activity"])
	assert.Nil(t, status["status"]) // legacy field removed
	assert.Equal(t, "Bash", status["toolName"])

	// Verify log file was created
	logPath := filepath.Join(tmpDir, "agent.log")
	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(logData), "Running tool: Bash")
}

func TestProcessHookData_Gemini(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)
	scrubScionEnv(t)
	log.SetLogPath(filepath.Join(tmpDir, "agent.log"))

	hookDialect = "gemini"

	data := map[string]interface{}{
		"hook_event_name": "BeforeAgent",
		"prompt":          "Help me code",
	}
	jsonData, _ := json.Marshal(data)

	err := processHookData(jsonData)
	require.NoError(t, err)

	// Verify status
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	statusData, err := os.ReadFile(statusPath)
	require.NoError(t, err)

	var status map[string]interface{}
	err = json.Unmarshal(statusData, &status)
	require.NoError(t, err)
	assert.Equal(t, "thinking", status["activity"])
	assert.Nil(t, status["status"]) // legacy field removed
}

func TestProcessHookData_SessionEvents(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)
	scrubScionEnv(t)
	log.SetLogPath(filepath.Join(tmpDir, "agent.log"))

	hookDialect = "claude"

	// Test SessionStart
	data := map[string]interface{}{
		"hook_event_name": "SessionStart",
		"source":          "cli",
	}
	jsonData, _ := json.Marshal(data)

	err := processHookData(jsonData)
	require.NoError(t, err)

	statusPath := filepath.Join(tmpDir, "agent-info.json")
	statusData, _ := os.ReadFile(statusPath)
	var status map[string]interface{}
	json.Unmarshal(statusData, &status)
	assert.Equal(t, "working", status["activity"]) // session-start sets working activity
	assert.Nil(t, status["status"])                // legacy field removed

	// Test SessionEnd
	data = map[string]interface{}{
		"hook_event_name": "SessionEnd",
		"reason":          "user_exit",
	}
	jsonData, _ = json.Marshal(data)

	err = processHookData(jsonData)
	require.NoError(t, err)

	statusData, _ = os.ReadFile(statusPath)
	json.Unmarshal(statusData, &status)
	assert.Equal(t, "stopped", status["phase"]) // session-end sets stopped phase
	assert.Nil(t, status["status"])             // legacy field removed
}

func TestProcessHookData_CodexCompletion(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)
	scrubScionEnv(t)
	log.SetLogPath(filepath.Join(tmpDir, "agent.log"))

	hookDialect = "codex"

	data := map[string]interface{}{
		"type":  "agent-turn-complete",
		"title": "Implemented telemetry wiring",
	}
	jsonData, _ := json.Marshal(data)

	err := processHookData(jsonData)
	require.NoError(t, err)

	statusPath := filepath.Join(tmpDir, "agent-info.json")
	statusData, err := os.ReadFile(statusPath)
	require.NoError(t, err)

	var status map[string]interface{}
	err = json.Unmarshal(statusData, &status)
	require.NoError(t, err)
	assert.Equal(t, "completed", status["activity"])
}
