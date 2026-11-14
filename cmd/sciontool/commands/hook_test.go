/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
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
		// An agent container of a native-usage harness exports
		// SCION_USAGE_SOURCE=native, which makes session-end hooks hold
		// their reports; tests that want it set it themselves.
		"SCION_USAGE_SOURCE",
	} {
		t.Setenv(key, "")
	}
}

func TestProcessHookData_Claude(t *testing.T) {
	// Set up temp home directory for status/log files
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))

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
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))

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
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))

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
	_ = json.Unmarshal(statusData, &status)
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
	_ = json.Unmarshal(statusData, &status)
	assert.Equal(t, "stopped", status["phase"]) // session-end sets stopped phase
	assert.Nil(t, status["status"])             // legacy field removed
}

func TestProcessHookData_CodexTurnComplete(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))

	oldDialect := hookDialect
	hookDialect = "codex"
	defer func() { hookDialect = oldDialect }()

	// Prime status to thinking (as UserPromptSubmit does during a turn).
	promptData, err := json.Marshal(map[string]interface{}{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      "s-1",
		"prompt":          "Investigate issue",
	})
	require.NoError(t, err)
	require.NoError(t, processHookData(promptData))

	// agent-turn-complete is a turn boundary, not task completion: it must
	// transition to non-terminal working rather than sticky completed.
	data := map[string]interface{}{
		"type":                   "agent-turn-complete",
		"title":                  "Implemented telemetry wiring",
		"last-assistant-message": "Inspecting brief and diff",
	}
	jsonData, err := json.Marshal(data)
	require.NoError(t, err)

	err = processHookData(jsonData)
	require.NoError(t, err)

	statusPath := filepath.Join(tmpDir, "agent-info.json")
	statusData, err := os.ReadFile(statusPath)
	require.NoError(t, err)

	var status map[string]interface{}
	err = json.Unmarshal(statusData, &status)
	require.NoError(t, err)
	assert.Equal(t, "working", status["activity"])
}

// TestCodexTurn_SingleMaxTurnsCountAndNoAutoComplete pins ptone/scion#7856:
// one Codex turn (even if a legacy config still delivers agent-turn-complete
// alongside the Stop hook through scion_notify.sh) must neither auto-emit
// COMPLETED nor increment max_turns more than once per turn.
func TestCodexTurn_SingleMaxTurnsCountAndNoAutoComplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scion_notify.sh requires POSIX sh")
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found: %v", err)
	}

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	t.Setenv("SCION_MAX_TURNS", "5")
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))

	oldDialect := hookDialect
	hookDialect = "codex"
	defer func() { hookDialect = oldDialect }()

	// Stage the real harnesses/codex/dialect.yaml into $HOME/.scion/harness/dialect.yaml.
	dialectSrc := testutil.HarnessDialectPath(t, "codex")
	harnessDir := filepath.Dir(dialectSrc)
	notifyScript := filepath.Join(harnessDir, "home", ".codex", "scion_notify.sh")

	stagedDir := filepath.Join(tmpDir, ".scion", "harness")
	require.NoError(t, os.MkdirAll(stagedDir, 0755))
	dialectBytes, err := os.ReadFile(dialectSrc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stagedDir, "dialect.yaml"), dialectBytes, 0644))

	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	require.NoError(t, handlers.InitLimitsFile(limitsPath, 5, 0, 0, 0))

	// Put a recording sciontool shim first on PATH so scion_notify.sh runs
	// unmodified and we replay every captured sciontool invocation in-process.
	binDir := filepath.Join(tmpDir, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0755))
	callsDir := filepath.Join(tmpDir, "calls")
	require.NoError(t, os.MkdirAll(callsDir, 0755))
	shimPath := filepath.Join(binDir, "sciontool")
	shimScript := "#!/bin/sh\nset -eu\ni=0\nwhile [ -e \"$CALLS_DIR/$i.args\" ]; do i=$((i + 1)); done\nprintf '%s\\n' \"$@\" > \"$CALLS_DIR/$i.args\"\ncat > \"$CALLS_DIR/$i.stdin\"\n"
	require.NoError(t, os.WriteFile(shimPath, []byte(shimScript), 0755))

	runNotify := func(stdinPayload string, argv ...string) {
		t.Helper()
		cmdArgs := append([]string{notifyScript}, argv...)
		cmd := exec.Command(shPath, cmdArgs...)
		cmd.Stdin = strings.NewReader(stdinPayload)
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"CALLS_DIR="+callsDir,
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "scion_notify.sh failed: %s", string(out))
	}

	// Simulate one Codex turn: UserPromptSubmit -> PreToolUse -> PostToolUse ->
	// Stop, plus a legacy notify agent-turn-complete callback (passed as argv[1],
	// as Codex's notify config does on un-upgraded configs).
	runNotify(`{"hook_event_name":"UserPromptSubmit","session_id":"s-1","prompt":"Inspect brief"}`)
	runNotify(`{"hook_event_name":"PreToolUse","session_id":"s-1","tool_name":"Bash"}`)
	runNotify(`{"hook_event_name":"PostToolUse","session_id":"s-1","tool_name":"Bash"}`)
	runNotify(`{"hook_event_name":"Stop","session_id":"s-1"}`)
	runNotify("", `{"type":"agent-turn-complete","title":"Turn 1","last-assistant-message":"Inspecting brief and diff"}`)

	// Replay every recorded sciontool call against the real command handlers.
	for i := 0; ; i++ {
		argsBytes, err := os.ReadFile(filepath.Join(callsDir, strconv.Itoa(i)+".args"))
		if os.IsNotExist(err) {
			require.Greater(t, i, 0, "expected scion_notify.sh to invoke sciontool for hook events")
			break
		}
		require.NoError(t, err)
		stdinBytes, err := os.ReadFile(filepath.Join(callsDir, strconv.Itoa(i)+".stdin"))
		require.NoError(t, err)

		args := strings.Split(strings.TrimRight(string(argsBytes), "\n"), "\n")
		if len(args) >= 2 && args[0] == "status" && args[1] == "task_completed" {
			msg := "Task completed"
			if len(args) >= 3 {
				msg = args[2]
			}
			runTaskCompleted(msg)
			t.Fatalf("scion_notify.sh invoked `sciontool status task_completed %q` on turn end", msg)
		}
		require.Equal(t, []string{"hook", "--dialect=codex"}, args, "unexpected sciontool invocation")
		require.NotEmpty(t, bytes.TrimSpace(stdinBytes))
		require.NoError(t, processHookData(stdinBytes))
	}

	// Verify final status is non-terminal working (not completed), and a follow-up
	// tool call in turn 2 transitions cleanly to executing instead of staying stuck
	// on sticky completed.
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	statusData, err := os.ReadFile(statusPath)
	require.NoError(t, err)
	var status map[string]interface{}
	require.NoError(t, json.Unmarshal(statusData, &status))
	assert.Equal(t, "working", status["activity"])

	// Verify max_turns incremented once for the turn, never twice.
	limitsData, err := os.ReadFile(limitsPath)
	require.NoError(t, err)
	var limits handlers.LimitsState
	require.NoError(t, json.Unmarshal(limitsData, &limits))
	assert.Equal(t, 1, limits.TurnCount, "turn_count must increment once per turn, not twice")

	// Turn 2 tool call transitions to executing (proving activity was not sticky completed).
	require.NoError(t, processHookData([]byte(`{"hook_event_name":"PreToolUse","session_id":"s-1","tool_name":"Bash"}`)))
	statusData, err = os.ReadFile(statusPath)
	require.NoError(t, err)
	status = nil
	require.NoError(t, json.Unmarshal(statusData, &status))
	assert.Equal(t, "executing", status["activity"])
}

func TestProcessHookData_HarnessBundledDialectOverridesBuiltin(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))

	oldDialect := hookDialect
	hookDialect = "codex"
	defer func() { hookDialect = oldDialect }()

	bundleDir := filepath.Join(tmpDir, ".scion", "harness")
	require.NoError(t, os.MkdirAll(bundleDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bundleDir, "dialect.yaml"), []byte(`
dialect: codex
event_name_field: custom_event
mappings:
  OverrideTool:
    event: tool-start
`), 0644))

	data := map[string]interface{}{
		"type":         "agent-turn-complete",
		"custom_event": "OverrideTool",
		"tool_name":    "BundleTool",
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
	assert.Equal(t, "executing", status["activity"])
	assert.Equal(t, "BundleTool", status["toolName"])

	logData, err := os.ReadFile(filepath.Join(tmpDir, "agent.log"))
	require.NoError(t, err)
	assert.Contains(t, string(logData), "Running tool: BundleTool")
}
