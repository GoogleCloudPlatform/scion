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

package logparser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLogFile(t *testing.T) {
	// Create a test log file
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			Severity:  "INFO",
			LogName:   "projects/test/logs/scion-agents",
			Labels: map[string]string{
				"agent_id":      "agent-1",
				"scion.harness": "gemini",
			},
			JSONPayload: map[string]any{
				"message": "agent.lifecycle.pre_start",
			},
		},
		{
			InsertID:  "2",
			Timestamp: "2026-03-22T16:30:01.000Z",
			Severity:  "INFO",
			LogName:   "projects/test/logs/scion-agents",
			Labels: map[string]string{
				"agent_id":      "agent-1",
				"scion.harness": "gemini",
			},
			JSONPayload: map[string]any{
				"message": "agent.session.start",
			},
		},
		{
			InsertID:  "3",
			Timestamp: "2026-03-22T16:30:05.000Z",
			Severity:  "INFO",
			LogName:   "projects/test/logs/scion-agents",
			Labels: map[string]string{
				"agent_id":      "agent-1",
				"scion.harness": "gemini",
			},
			JSONPayload: map[string]any{
				"message":   "agent.tool.call",
				"tool_name": "write_file",
				"file_path": "/workspace/src/main.go",
			},
		},
		{
			InsertID:  "4",
			Timestamp: "2026-03-22T16:30:10.000Z",
			Severity:  "INFO",
			LogName:   "projects/test/logs/scion-messages",
			Labels: map[string]string{
				"sender":       "agent:alpha",
				"sender_id":    "agent-1",
				"recipient":    "agent:beta",
				"recipient_id": "agent-2",
				"msg_type":     "instruction",
				"grove_id":     "grove-1",
			},
			JSONPayload: map[string]any{
				"sender":          "agent:alpha",
				"recipient":       "agent:beta",
				"msg_type":        "instruction",
				"message_content": "do something",
				"message":         "message dispatched",
			},
		},
		{
			InsertID:  "5",
			Timestamp: "2026-03-22T16:30:15.000Z",
			Severity:  "INFO",
			LogName:   "projects/test/logs/scion-agents",
			Labels: map[string]string{
				"agent_id":      "agent-2",
				"scion.harness": "claude",
			},
			JSONPayload: map[string]any{
				"message": "agent.session.start",
			},
		},
		{
			InsertID:  "6",
			Timestamp: "2026-03-22T16:31:00.000Z",
			Severity:  "INFO",
			LogName:   "projects/test/logs/scion-agents",
			Labels: map[string]string{
				"agent_id":      "agent-1",
				"scion.harness": "gemini",
			},
			JSONPayload: map[string]any{
				"message": "agent.session.end",
			},
		},
	}

	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test-logs.json")
	if err := os.WriteFile(logPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ParseLogFile(logPath, "", 0)
	if err != nil {
		t.Fatalf("ParseLogFile failed: %v", err)
	}

	// Verify manifest
	if result.Manifest.Type != "manifest" {
		t.Errorf("expected manifest type, got %s", result.Manifest.Type)
	}

	// Verify agents found
	if len(result.Manifest.Agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(result.Manifest.Agents))
	}

	// Verify agent names resolved from messages
	agentNames := map[string]bool{}
	for _, a := range result.Manifest.Agents {
		agentNames[a.Name] = true
	}
	if !agentNames["alpha"] {
		t.Error("expected agent name 'alpha' from message sender")
	}

	// Files are now empty in manifest (added dynamically via events)
	if len(result.Manifest.Files) != 0 {
		t.Errorf("expected empty files in manifest, got %d", len(result.Manifest.Files))
	}

	// Verify events
	if len(result.Events) == 0 {
		t.Fatal("expected events, got none")
	}

	// Count event types
	typeCounts := map[string]int{}
	for _, e := range result.Events {
		typeCounts[e.Type]++
	}
	if typeCounts["agent_state"] == 0 {
		t.Error("expected agent_state events")
	}
	if typeCounts["message"] != 1 {
		t.Errorf("expected 1 message event, got %d", typeCounts["message"])
	}
	if typeCounts["file_edit"] != 1 {
		t.Errorf("expected 1 file_edit event, got %d", typeCounts["file_edit"])
	}

	// Verify time range
	if result.Manifest.TimeRange.Start != "2026-03-22T16:30:00.000Z" {
		t.Errorf("unexpected start time: %s", result.Manifest.TimeRange.Start)
	}
}

func TestIsFileEditTool(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"write_file", true},
		{"create_file", true},
		{"Write", true},
		{"edit_file", true},
		{"Edit", true},
		{"patch_file", true},
		{"read_file", false},
		{"Read", false},
		{"run_shell_command", false},
		{"Bash", false},
	}

	for _, tt := range tests {
		if got := isFileEditTool(tt.name); got != tt.expected {
			t.Errorf("isFileEditTool(%q) = %v, want %v", tt.name, got, tt.expected)
		}
	}
}

func TestIsFileReadTool(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"read_file", true},
		{"Read", true},
		{"Grep", true},
		{"Glob", true},
		{"Write", false},
		{"Bash", false},
	}

	for _, tt := range tests {
		if got := isFileReadTool(tt.name); got != tt.expected {
			t.Errorf("isFileReadTool(%q) = %v, want %v", tt.name, got, tt.expected)
		}
	}
}

func TestIsShellTool(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"Bash", true},
		{"run_shell_command", true},
		{"Read", false},
		{"Write", false},
		{"edit_file", false},
	}

	for _, tt := range tests {
		if got := isShellTool(tt.name); got != tt.expected {
			t.Errorf("isShellTool(%q) = %v, want %v", tt.name, got, tt.expected)
		}
	}
}

func TestTimestampToTime(t *testing.T) {
	ts := "2026-03-22T16:30:00.123456789Z"
	tm, err := TimestampToTime(ts)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Year() != 2026 || tm.Month() != 3 || tm.Day() != 22 {
		t.Errorf("unexpected parsed time: %v", tm)
	}
}

func TestExtractProjectInfo_ProjectIDLabel(t *testing.T) {
	entries := []GCPLogEntry{
		{
			LogName: "projects/test/logs/scion-messages",
			Labels: map[string]string{
				"project_id": "proj-1",
			},
			JSONPayload: map[string]any{
				"message": "message dispatched",
			},
		},
	}
	id, name := extractProjectInfo(entries)
	if id != "proj-1" {
		t.Errorf("expected project id %q, got %q", "proj-1", id)
	}
	if name != "proj-1" {
		t.Errorf("expected project name %q, got %q", "proj-1", name)
	}
}

func TestExtractProjectInfo_LegacyGroveIDLabel(t *testing.T) {
	// Older logs only carry the grove_id label; it must still be picked up
	// as a fallback when project_id is absent.
	entries := []GCPLogEntry{
		{
			LogName: "projects/test/logs/scion-messages",
			Labels: map[string]string{
				"grove_id": "grove-1",
			},
			JSONPayload: map[string]any{
				"message": "message dispatched",
			},
		},
	}
	id, name := extractProjectInfo(entries)
	if id != "grove-1" {
		t.Errorf("expected project id %q, got %q", "grove-1", id)
	}
	if name != "grove-1" {
		t.Errorf("expected project name %q, got %q", "grove-1", name)
	}
}

func TestExtractProjectInfo_PrefersProjectIDOverGroveID(t *testing.T) {
	// An entry carrying both labels is realistic during migration (a mixed
	// fleet of old and new logging). project_id must win.
	entries := []GCPLogEntry{
		{
			LogName: "projects/test/logs/scion-messages",
			Labels: map[string]string{
				"project_id": "proj-1",
				"grove_id":   "grove-1",
			},
			JSONPayload: map[string]any{
				"message": "message dispatched",
			},
		},
	}
	id, name := extractProjectInfo(entries)
	if id != "proj-1" {
		t.Errorf("expected project_id to take precedence, got id %q", id)
	}
	if name != "proj-1" {
		t.Errorf("expected project_id to take precedence, got name %q", name)
	}
}

func TestExtractFilesEmpty(t *testing.T) {
	// When no file tool calls, files list should be empty (no placeholders)
	entries := []GCPLogEntry{
		{
			LogName: "projects/test/logs/scion-agents",
			Labels:  map[string]string{"agent_id": "a1"},
			JSONPayload: map[string]any{
				"message": "agent.session.start",
			},
		},
	}
	files := extractFiles(entries)
	if len(files) != 0 {
		t.Errorf("expected empty files when no tool calls found, got %d", len(files))
	}
}

func TestExtractFilesFromReads(t *testing.T) {
	entries := []GCPLogEntry{
		{
			LogName: "projects/test/logs/scion-agents",
			Labels:  map[string]string{"agent_id": "a1"},
			JSONPayload: map[string]any{
				"message":   "agent.tool.call",
				"tool_name": "Read",
				"file_path": "/workspace/config.yaml",
			},
		},
	}
	files := extractFiles(entries)

	fileIDs := map[string]bool{}
	for _, f := range files {
		fileIDs[f.ID] = true
	}
	if !fileIDs["."] {
		t.Error("expected root '.' node")
	}
	if !fileIDs["config.yaml"] {
		t.Error("expected 'config.yaml' from Read tool call")
	}
}

func TestFileReadEvents(t *testing.T) {
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "a1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message":   "agent.tool.call",
				"tool_name": "Read",
				"file_path": "/workspace/README.md",
			},
		},
	}
	agents := extractAgents(entries)
	events := extractEvents(entries, agents)

	var foundRead bool
	for _, e := range events {
		if e.Type == "file_read" {
			if fe, ok := e.Data.(FileEditEvent); ok {
				if fe.FilePath == "README.md" && fe.Action == "read" {
					foundRead = true
				}
			}
		}
	}
	if !foundRead {
		t.Error("expected file_read event for Read tool call")
	}
}

func TestExtractAgentsFromMessages(t *testing.T) {
	// Agents referenced only in messages (no scion-agents entries) should be discovered
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-messages",
			Labels: map[string]string{
				"sender":       "agent:green-agent",
				"sender_id":    "sender-uuid-1",
				"recipient":    "agent:orchestrator",
				"recipient_id": "recipient-uuid-2",
			},
			JSONPayload: map[string]any{
				"sender":          "agent:green-agent",
				"recipient":       "agent:orchestrator",
				"sender_id":       "sender-uuid-1",
				"recipient_id":    "recipient-uuid-2",
				"msg_type":        "state-change",
				"message_content": "test message",
				"message":         "message dispatched",
			},
		},
	}
	agents := extractAgents(entries)
	if len(agents) < 2 {
		t.Fatalf("expected at least 2 agents from messages, got %d", len(agents))
	}
	nameSet := map[string]bool{}
	for _, a := range agents {
		nameSet[a.Name] = true
	}
	if !nameSet["green-agent"] {
		t.Error("expected agent 'green-agent' discovered from message sender")
	}
	if !nameSet["orchestrator"] {
		t.Error("expected agent 'orchestrator' discovered from message recipient")
	}
}

func TestBackfillAgentCreateEvents(t *testing.T) {
	// Agents without lifecycle events should get synthetic agent_create events
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "gemini"},
			JSONPayload: map[string]any{
				"message": "agent.session.start",
			},
		},
	}
	agents := extractAgents(entries)
	events := extractEvents(entries, agents)

	// Should have a backfilled agent_create event before the session.start
	var foundCreate bool
	for _, e := range events {
		if e.Type == "agent_create" {
			if lce, ok := e.Data.(AgentLifecycleEvent); ok && lce.AgentID == "agent-1" {
				foundCreate = true
				break
			}
		}
	}
	if !foundCreate {
		t.Error("expected backfilled agent_create event for agent without lifecycle events")
	}
}

func TestAgentDestroyFromPreStop(t *testing.T) {
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message": "agent.lifecycle.pre_start",
			},
		},
		{
			InsertID:  "2",
			Timestamp: "2026-03-22T16:31:00.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message": "agent.lifecycle.pre_stop",
			},
		},
	}
	agents := extractAgents(entries)
	events := extractEvents(entries, agents)

	var foundDestroy bool
	for _, e := range events {
		if e.Type == "agent_destroy" {
			if lce, ok := e.Data.(AgentLifecycleEvent); ok && lce.AgentID == "agent-1" {
				if lce.Action != "destroy" {
					t.Errorf("expected action 'destroy', got %q", lce.Action)
				}
				foundDestroy = true
				break
			}
		}
	}
	if !foundDestroy {
		t.Error("expected agent_destroy event from pre_stop lifecycle event")
	}
}

func TestAgentCreatedFromServerLog(t *testing.T) {
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-server",
			Labels: map[string]string{
				"agent_id": "agent-uuid-1",
			},
			JSONPayload: map[string]any{
				"message":  "Agent created",
				"agent_id": "agent-uuid-1",
				"name":     "poet-red",
				"slug":     "poet-red",
			},
		},
		{
			InsertID:  "2",
			Timestamp: "2026-03-22T16:30:01.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-uuid-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message": "agent.lifecycle.pre_start",
			},
		},
	}
	agents := extractAgents(entries)

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Name != "poet-red" {
		t.Errorf("expected agent name 'poet-red' from server log, got %q", agents[0].Name)
	}
}

func TestParseFSLog(t *testing.T) {
	// Create a test NDJSON fs-watcher log
	fsLogLines := `{"ts":"2026-03-22T16:30:02.000Z","agent_id":"frontend","action":"modify","path":"web/src/App.tsx","size":4096}
{"ts":"2026-03-22T16:30:03.500Z","agent_id":"backend","action":"create","path":"pkg/api/handler.go","size":1523}
{"ts":"2026-03-22T16:30:05.200Z","agent_id":"frontend","action":"delete","path":"web/src/old-util.ts"}
{"ts":"2026-03-22T16:30:06.000Z","agent_id":"backend","action":"rename_to","path":"pkg/api/routes.go","size":800}
{"ts":"2026-03-22T16:30:06.000Z","agent_id":"backend","action":"rename_from","path":"pkg/api/old-routes.go"}
`
	tmpDir := t.TempDir()
	fsLogPath := filepath.Join(tmpDir, "fs-events.ndjson")
	if err := os.WriteFile(fsLogPath, []byte(fsLogLines), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := parseFSLog(fsLogPath)
	if err != nil {
		t.Fatalf("parseFSLog failed: %v", err)
	}

	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}

	// Verify first event: modify -> edit
	fe0 := events[0].Data.(FileEditEvent)
	if fe0.AgentID != "frontend" || fe0.Action != "edit" || fe0.FilePath != "web/src/App.tsx" {
		t.Errorf("event 0: got %+v", fe0)
	}

	// Verify create
	fe1 := events[1].Data.(FileEditEvent)
	if fe1.Action != "create" {
		t.Errorf("event 1: expected action 'create', got %q", fe1.Action)
	}

	// Verify delete
	fe2 := events[2].Data.(FileEditEvent)
	if fe2.Action != "delete" {
		t.Errorf("event 2: expected action 'delete', got %q", fe2.Action)
	}

	// Verify rename_to -> create
	fe3 := events[3].Data.(FileEditEvent)
	if fe3.Action != "create" {
		t.Errorf("event 3: expected action 'create' for rename_to, got %q", fe3.Action)
	}

	// Verify rename_from -> delete
	fe4 := events[4].Data.(FileEditEvent)
	if fe4.Action != "delete" {
		t.Errorf("event 4: expected action 'delete' for rename_from, got %q", fe4.Action)
	}

	// All events should be file_edit type
	for i, e := range events {
		if e.Type != "file_edit" {
			t.Errorf("event %d: expected type 'file_edit', got %q", i, e.Type)
		}
	}
}

func TestParseLogFileWithFSLog(t *testing.T) {
	// Primary log with a write tool call and a read tool call
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message":   "agent.tool.call",
				"tool_name": "Write",
				"file_path": "/workspace/main.go",
			},
		},
		{
			InsertID:  "2",
			Timestamp: "2026-03-22T16:30:01.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message":   "agent.tool.call",
				"tool_name": "Read",
				"file_path": "/workspace/README.md",
			},
		},
		{
			InsertID:  "3",
			Timestamp: "2026-03-22T16:30:02.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message": "agent.turn.end",
			},
		},
	}
	data, _ := json.Marshal(entries)
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "logs.json")
	os.WriteFile(logPath, data, 0o644)

	// FS log with different file events
	fsLogLines := `{"ts":"2026-03-22T16:30:00.500Z","agent_id":"agent-1","action":"modify","path":"main.go","size":2048}
`
	fsLogPath := filepath.Join(tmpDir, "fs.ndjson")
	os.WriteFile(fsLogPath, []byte(fsLogLines), 0o644)

	// Parse with fs-log: file_edit from Write tool should be suppressed,
	// but file_read from Read tool should still appear (fs-watcher doesn't capture reads yet).
	result, err := ParseLogFile(logPath, fsLogPath, 0)
	if err != nil {
		t.Fatalf("ParseLogFile with fs-log failed: %v", err)
	}

	fileEditCount := 0
	fileReadCount := 0
	for _, e := range result.Events {
		if e.Type == "file_edit" {
			fileEditCount++
			fe := e.Data.(FileEditEvent)
			// Should be from fs-log, not from tool call
			if fe.FilePath != "main.go" {
				t.Errorf("unexpected file_edit path: %q", fe.FilePath)
			}
			if fe.Action != "edit" {
				t.Errorf("expected action 'edit' from fs-log modify, got %q", fe.Action)
			}
		}
		if e.Type == "file_read" {
			fileReadCount++
			fe := e.Data.(FileEditEvent)
			if fe.FilePath != "README.md" {
				t.Errorf("unexpected file_read path: %q", fe.FilePath)
			}
		}
	}
	if fileEditCount != 1 {
		t.Errorf("expected exactly 1 file_edit (from fs-log), got %d", fileEditCount)
	}
	if fileReadCount != 1 {
		t.Errorf("expected 1 file_read from primary log even with --fs-log, got %d", fileReadCount)
	}

	// Parse without fs-log: should have both tool-based file events
	resultNoFS, err := ParseLogFile(logPath, "", 0)
	if err != nil {
		t.Fatalf("ParseLogFile without fs-log failed: %v", err)
	}

	fileEditCount = 0
	fileReadCount = 0
	for _, e := range resultNoFS.Events {
		if e.Type == "file_edit" {
			fileEditCount++
		}
		if e.Type == "file_read" {
			fileReadCount++
		}
	}
	if fileEditCount != 1 {
		t.Errorf("expected 1 file_edit from tool calls, got %d", fileEditCount)
	}
	if fileReadCount != 1 {
		t.Errorf("expected 1 file_read from tool calls, got %d", fileReadCount)
	}
}

func TestNoDuplicateAgentsFromSlugMessages(t *testing.T) {
	// When messages reference agents by slug name without a UUID, the parser
	// should resolve them to existing UUID-based agents, not create duplicates.
	entries := []GCPLogEntry{
		// Server creates agent with UUID
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-server",
			JSONPayload: map[string]any{
				"message":  "Agent created",
				"agent_id": "aaaa-bbbb-cccc",
				"slug":     "runner",
			},
		},
		// Agent lifecycle (has UUID)
		{
			InsertID:  "2",
			Timestamp: "2026-03-22T16:30:01.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "aaaa-bbbb-cccc", "scion.harness": "gemini"},
			JSONPayload: map[string]any{
				"message": "agent.lifecycle.pre_start",
			},
		},
		// Message sent by "agent:runner" WITHOUT a sender UUID
		{
			InsertID:  "3",
			Timestamp: "2026-03-22T16:31:00.000Z",
			LogName:   "projects/test/logs/scion-messages",
			Labels: map[string]string{
				"recipient":    "agent:runner",
				"recipient_id": "aaaa-bbbb-cccc",
			},
			JSONPayload: map[string]any{
				"sender":    "agent:runner",
				"recipient": "agent:runner",
				// Note: no sender_id — this is the scenario that caused duplicates
				"recipient_id":    "aaaa-bbbb-cccc",
				"msg_type":        "broadcast",
				"message_content": "hello",
				"message":         "message dispatched",
			},
		},
	}

	agents := extractAgents(entries)

	// Should have exactly 1 agent, not 2
	if len(agents) != 1 {
		t.Errorf("expected 1 agent, got %d:", len(agents))
		for _, a := range agents {
			t.Logf("  %s (id=%s)", a.Name, a.ID)
		}
	}

	// The agent should use the UUID, not the slug
	if agents[0].ID != "aaaa-bbbb-cccc" {
		t.Errorf("expected agent ID 'aaaa-bbbb-cccc', got %q", agents[0].ID)
	}

	// Events should not contain a bogus agent_create for the slug-based agent
	events := extractEvents(entries, agents)
	createCount := 0
	for _, e := range events {
		if e.Type == "agent_create" {
			createCount++
		}
	}
	if createCount != 1 {
		t.Errorf("expected 1 agent_create event, got %d", createCount)
		for _, e := range events {
			if e.Type == "agent_create" {
				t.Logf("  %s %+v", e.Timestamp, e.Data)
			}
		}
	}
}

func TestPostStartSetsRunningPhase(t *testing.T) {
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message": "agent.lifecycle.pre_start",
			},
		},
		{
			InsertID:  "2",
			Timestamp: "2026-03-22T16:30:01.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message": "agent.lifecycle.post_start",
			},
		},
	}
	agents := extractAgents(entries)
	events := extractEvents(entries, agents)

	var foundRunning bool
	for _, e := range events {
		if e.Type == "agent_state" {
			if ase, ok := e.Data.(AgentStateEvent); ok && ase.AgentID == "agent-1" && ase.Phase == "running" {
				foundRunning = true
			}
		}
	}
	if !foundRunning {
		t.Error("expected agent_state with phase 'running' from post_start event")
	}
}

func TestMaxDepthInManifest(t *testing.T) {
	entries := []GCPLogEntry{
		{
			InsertID:  "1",
			Timestamp: "2026-03-22T16:30:00.000Z",
			LogName:   "projects/test/logs/scion-agents",
			Labels:    map[string]string{"agent_id": "agent-1", "scion.harness": "claude"},
			JSONPayload: map[string]any{
				"message": "agent.session.start",
			},
		},
	}
	data, _ := json.Marshal(entries)
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "logs.json")
	os.WriteFile(logPath, data, 0o644)

	result, err := ParseLogFile(logPath, "", 3)
	if err != nil {
		t.Fatalf("ParseLogFile failed: %v", err)
	}
	if result.Manifest.MaxDepth != 3 {
		t.Errorf("expected maxDepth=3 in manifest, got %d", result.Manifest.MaxDepth)
	}

	// maxDepth=0 means unlimited
	result2, err := ParseLogFile(logPath, "", 0)
	if err != nil {
		t.Fatalf("ParseLogFile failed: %v", err)
	}
	if result2.Manifest.MaxDepth != 0 {
		t.Errorf("expected maxDepth=0 in manifest, got %d", result2.Manifest.MaxDepth)
	}
}
