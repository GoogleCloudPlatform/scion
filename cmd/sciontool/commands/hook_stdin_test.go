/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copilotPostToolUse is a postToolUse payload in Copilot's camelCase format,
// the format selected by the camelCase event names provision.py registers
// (docs.github.com/en/copilot/reference/hooks-configuration).
const copilotPostToolUse = `{
  "sessionId": "copilot-session-42",
  "timestamp": 1760136000000,
  "cwd": "/workspace",
  "toolName": "bash",
  "toolArgs": {"command": "ls"},
  "toolResult": {"resultType": "success", "textResultForLlm": "README.md"}
}`

func decodeEventData(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func TestPositionalEventData_NoPayloadIsSynthetic(t *testing.T) {
	for _, payload := range [][]byte{nil, {}, []byte("  \n")} {
		got := positionalEventData("sessionStart", payload)
		assert.JSONEq(t, `{"hook_event_name":"sessionStart"}`, string(got))
	}
}

func TestPositionalEventData_MergesPayload(t *testing.T) {
	got := decodeEventData(t, positionalEventData("postToolUse", []byte(copilotPostToolUse)))
	assert.Equal(t, "postToolUse", got["hook_event_name"])
	assert.Equal(t, "copilot-session-42", got["sessionId"])
	assert.Equal(t, "bash", got["toolName"])
}

func TestPositionalEventData_ExplicitEventWins(t *testing.T) {
	got := decodeEventData(t, positionalEventData("preToolUse", []byte(`{"hook_event_name":"other","toolName":"bash"}`)))
	assert.Equal(t, "preToolUse", got["hook_event_name"])
	assert.Equal(t, "bash", got["toolName"])
}

func TestPositionalEventData_MalformedPayloadIgnored(t *testing.T) {
	setTestLogPath(t, filepath.Join(t.TempDir(), "agent.log"))
	for _, payload := range []string{`{not json`, `[1,2]`, `"text"`, `null`, `42`} {
		got := positionalEventData("postToolUse", []byte(payload))
		assert.JSONEq(t, `{"hook_event_name":"postToolUse"}`, string(got), "payload %q", payload)
	}
}

func TestReadOptionalStdin(t *testing.T) {
	t.Run("pipe with payload", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		_, err = w.WriteString(copilotPostToolUse)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		assert.Equal(t, copilotPostToolUse, string(readOptionalStdin(r, time.Second)))
	})

	t.Run("payload on a pipe left open", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		defer func() { _ = w.Close() }()
		_, err = w.WriteString(copilotPostToolUse)
		require.NoError(t, err)
		start := time.Now()
		assert.Equal(t, copilotPostToolUse, string(readOptionalStdin(r, 5*time.Second)))
		assert.Less(t, time.Since(start), 2*time.Second, "a complete object must not wait for EOF")
	})

	t.Run("malformed payload is ignored", func(t *testing.T) {
		setTestLogPath(t, filepath.Join(t.TempDir(), "agent.log"))
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		_, err = w.WriteString(`{not json`)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		assert.Nil(t, readOptionalStdin(r, time.Second))
	})

	t.Run("partial value on a pipe left open is bounded", func(t *testing.T) {
		setTestLogPath(t, filepath.Join(t.TempDir(), "agent.log"))
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		defer func() { _ = w.Close() }()
		_, err = w.WriteString(`{"a":1`)
		require.NoError(t, err)
		start := time.Now()
		assert.Nil(t, readOptionalStdin(r, 100*time.Millisecond))
		assert.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("oversized payload is ignored", func(t *testing.T) {
		setTestLogPath(t, filepath.Join(t.TempDir(), "agent.log"))
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		// One JSON object larger than maxPositionalStdinBytes, written
		// concurrently because it exceeds the pipe buffer. The writer is
		// left open: the size cap, not EOF, must end the read.
		big := []byte(`{"a":"` + strings.Repeat("x", maxPositionalStdinBytes+1024) + `"}`)
		go func() {
			_, _ = w.Write(big)
		}()
		defer func() { _ = w.Close() }()
		assert.Nil(t, readOptionalStdin(r, 10*time.Second))
	})

	t.Run("trailing bytes after the value are ignored", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		defer func() { _ = w.Close() }()
		_, err = w.WriteString(`{"a":1} junk`)
		require.NoError(t, err)
		start := time.Now()
		assert.Equal(t, `{"a":1}`, string(readOptionalStdin(r, 5*time.Second)))
		assert.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("whitespace only", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		_, err = w.WriteString(" \n ")
		require.NoError(t, err)
		require.NoError(t, w.Close())
		assert.Nil(t, readOptionalStdin(r, time.Second))
	})

	t.Run("closed empty pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		require.NoError(t, w.Close())
		assert.Empty(t, readOptionalStdin(r, time.Second))
	})

	t.Run("open pipe without data does not block", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer func() { _ = r.Close() }()
		defer func() { _ = w.Close() }()
		start := time.Now()
		assert.Nil(t, readOptionalStdin(r, 50*time.Millisecond))
		assert.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("character device is skipped", func(t *testing.T) {
		devNull, err := os.Open(os.DevNull)
		require.NoError(t, err)
		defer func() { _ = devNull.Close() }()
		assert.Nil(t, readOptionalStdin(devNull, time.Second))
	})

	t.Run("nil file", func(t *testing.T) {
		assert.Nil(t, readOptionalStdin(nil, time.Second))
	})
}

func copilotDialectPath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "harnesses", "copilot", "dialect.yaml")
}

// TestCopilotPostToolUse_StdinThroughDialect runs a real camelCase
// postToolUse payload from a stdin pipe through the positional event path
// and the shipped Copilot dialect into the normalized event.
func TestCopilotPostToolUse_StdinThroughDialect(t *testing.T) {
	md, err := dialects.LoadMappingDialect(copilotDialectPath(t))
	require.NoError(t, err)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	_, err = w.WriteString(copilotPostToolUse)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(positionalEventData("postToolUse", readOptionalStdin(r, time.Second)), &raw))
	event, err := md.Parse(raw)
	require.NoError(t, err)

	assert.Equal(t, hooks.EventToolEnd, event.Name)
	assert.Equal(t, "bash", event.Data.ToolName)
	assert.Equal(t, "copilot-session-42", event.Data.SessionID)
	assert.True(t, event.Data.Success)
	assert.JSONEq(t, `{"command":"ls"}`, event.Data.ToolInput)
	assert.Equal(t, "README.md", event.Data.ToolOutput)
}

func TestCopilotDialect_CamelCasePayloads(t *testing.T) {
	md, err := dialects.LoadMappingDialect(copilotDialectPath(t))
	require.NoError(t, err)

	parse := func(event, payload string) *hooks.Event {
		t.Helper()
		var raw map[string]interface{}
		require.NoError(t, json.Unmarshal(positionalEventData(event, []byte(payload)), &raw))
		ev, err := md.Parse(raw)
		require.NoError(t, err)
		return ev
	}

	t.Run("non-success resultType string is not success", func(t *testing.T) {
		// Guard on the success string rule, not a documented Copilot payload:
		// the docs list only resultType "success" on postToolUse (failed
		// tools arrive on postToolUseFailure, which is not registered here).
		ev := parse("postToolUse", `{"sessionId":"s1","toolName":"bash","toolResult":{"resultType":"failure","textResultForLlm":"exit 1"}}`)
		assert.Equal(t, "bash", ev.Data.ToolName)
		assert.False(t, ev.Data.Success)
	})
	t.Run("tool start", func(t *testing.T) {
		ev := parse("preToolUse", `{"sessionId":"s1","toolName":"view","toolArgs":"{\"path\":\"a\"}"}`)
		assert.Equal(t, hooks.EventToolStart, ev.Name)
		assert.Equal(t, "view", ev.Data.ToolName)
		assert.Equal(t, "s1", ev.Data.SessionID)
		assert.Equal(t, `{"path":"a"}`, ev.Data.ToolInput)
	})
	t.Run("session start and end", func(t *testing.T) {
		ev := parse("sessionStart", `{"sessionId":"s1","source":"new"}`)
		assert.Equal(t, hooks.EventSessionStart, ev.Name)
		assert.Equal(t, "s1", ev.Data.SessionID)
		assert.Equal(t, "new", ev.Data.Source)
		ev = parse("sessionEnd", `{"sessionId":"s1","reason":"complete"}`)
		assert.Equal(t, hooks.EventSessionEnd, ev.Name)
		assert.Equal(t, "s1", ev.Data.SessionID)
		assert.Equal(t, "complete", ev.Data.Reason)
	})
	t.Run("prompt", func(t *testing.T) {
		ev := parse("userPromptSubmitted", `{"sessionId":"s1","prompt":"hi"}`)
		assert.Equal(t, "hi", ev.Data.Prompt)
		assert.Equal(t, "s1", ev.Data.SessionID)
	})
	t.Run("error object", func(t *testing.T) {
		ev := parse("errorOccurred", `{"sessionId":"s1","error":{"message":"boom","name":"Error"}}`)
		assert.Equal(t, "boom", ev.Data.Error)
		assert.Equal(t, "boom", ev.Data.Message)
	})
	t.Run("no payload still maps the event", func(t *testing.T) {
		ev := parse("agentStop", ``)
		assert.Equal(t, hooks.EventAgentEnd, ev.Name)
		assert.Empty(t, ev.Data.SessionID)
	})
}

// TestProcessHookData_CopilotPositionalPayload runs the merged payload
// through processHookData with the Copilot dialect staged where sciontool
// discovers it, and checks the tool name reaches the agent status.
func TestProcessHookData_CopilotPositionalPayload(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))

	spec, err := os.ReadFile(copilotDialectPath(t))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion", "harness"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "harness", "dialect.yaml"), spec, 0o644))

	prev := hookDialect
	hookDialect = "copilot"
	t.Cleanup(func() { hookDialect = prev })

	require.NoError(t, processHookData(positionalEventData("preToolUse",
		[]byte(`{"sessionId":"s1","toolName":"bash","toolArgs":{"command":"ls"}}`))))

	statusData, err := os.ReadFile(filepath.Join(tmpDir, "agent-info.json"))
	require.NoError(t, err)
	var status map[string]interface{}
	require.NoError(t, json.Unmarshal(statusData, &status))
	assert.Equal(t, "executing", status["activity"])
	assert.Equal(t, "bash", status["toolName"])
}
