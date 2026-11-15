/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusCommand(t *testing.T) {
	resetRootCmdState(t)
	// Create a temp directory for test files
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	scrubScionEnv(t)

	tests := []struct {
		name            string
		args            []string
		wantErr         bool
		wantActivity    string
		wantLogContains string
	}{
		{
			name:            "ask_user with message",
			args:            []string{"status", "ask_user", "What should I do?"},
			wantActivity:    "waiting_for_input",
			wantLogContains: "Agent requested input: What should I do?",
		},
		{
			name:            "ask_user with default message",
			args:            []string{"status", "ask_user"},
			wantActivity:    "waiting_for_input",
			wantLogContains: "Agent requested input: Input requested",
		},
		{
			name:            "task_completed with message",
			args:            []string{"status", "task_completed", "Finished the feature"},
			wantActivity:    "completed",
			wantLogContains: "Agent completed task: Finished the feature",
		},
		{
			name:            "task_completed with default message",
			args:            []string{"status", "task_completed"},
			wantActivity:    "completed",
			wantLogContains: "Agent completed task: Task completed",
		},
		{
			name:            "ask_user with multi-word message",
			args:            []string{"status", "ask_user", "Which", "option", "do", "you", "prefer?"},
			wantActivity:    "waiting_for_input",
			wantLogContains: "Agent requested input: Which option do you prefer?",
		},
		{
			name:            "blocked with message",
			args:            []string{"status", "blocked", "Waiting for review"},
			wantActivity:    "blocked",
			wantLogContains: "Agent blocked: Waiting for review",
		},
		{
			name:            "blocked with default message",
			args:            []string{"status", "blocked"},
			wantActivity:    "blocked",
			wantLogContains: "Agent blocked: Agent is blocked",
		},
		{
			name:            "idle with message",
			args:            []string{"status", "idle", "No task assigned"},
			wantActivity:    "idle",
			wantLogContains: "Agent idle: No task assigned",
		},
		{
			name:            "idle with default message",
			args:            []string{"status", "idle"},
			wantActivity:    "idle",
			wantLogContains: "Agent idle: Agent is idle",
		},
		{
			name:            "limits_exceeded with message",
			args:            []string{"status", "limits_exceeded", "max_turns of 50 exceeded"},
			wantActivity:    "limits_exceeded",
			wantLogContains: "Agent limits exceeded: max_turns of 50 exceeded",
		},
		{
			name:            "limits_exceeded with default message",
			args:            []string{"status", "limits_exceeded"},
			wantActivity:    "limits_exceeded",
			wantLogContains: "Agent limits exceeded: Agent limits exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up files before each test
			statusFile := filepath.Join(tempDir, "agent-info.json")
			logFile := filepath.Join(tempDir, "agent.log")
			_ = os.Remove(statusFile)
			_ = os.Remove(logFile)
			setTestLogPath(t, logFile)

			rootCmd.SetArgs(tt.args)
			err := rootCmd.Execute()

			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Check the status file was created with correct fields
			data, err := os.ReadFile(statusFile)
			if err != nil {
				t.Fatalf("Failed to read status file: %v", err)
			}

			var info map[string]string
			if err := json.Unmarshal(data, &info); err != nil {
				t.Fatalf("Failed to parse status file: %v", err)
			}

			if got := info["activity"]; got != tt.wantActivity {
				t.Errorf("activity = %q, want %q", got, tt.wantActivity)
			}

			if _, hasStatus := info["status"]; hasStatus {
				t.Errorf("status field should not exist (legacy field removed), but found %q", info["status"])
			}

			// Check the log file contains the expected message
			logData, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatalf("Failed to read log file: %v", err)
			}

			if !strings.Contains(string(logData), tt.wantLogContains) {
				t.Errorf("log file does not contain %q, got: %s", tt.wantLogContains, logData)
			}
		})
	}
}

func TestStatusCommandUnknownType(t *testing.T) {
	resetRootCmdState(t)
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)
	scrubScionEnv(t)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"status", "unknown_type"})

	_ = rootCmd.Execute()

	output := buf.String()
	if !bytes.Contains([]byte(output), []byte("unknown status type")) {
		t.Errorf("expected error message about unknown status type, got: %s", output)
	}
}

// TestStatusHelpListsIdle checks that the help lists idle and states the
// rule that separates it from blocked and task_completed.
func TestStatusHelpListsIdle(t *testing.T) {
	for _, want := range []string{
		"idle Signal that the agent has nothing to do until new work arrives",
		"is blocked; nothing to do until a new message or task arrives is idle;",
		"sciontool status idle ",
	} {
		if !strings.Contains(strings.Join(strings.Fields(statusCmd.Long), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("status help does not contain %q", want)
		}
	}
}
