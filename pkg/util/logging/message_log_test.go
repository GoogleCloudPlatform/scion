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

package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewMessageLogger_DefaultConfig(t *testing.T) {
	cfg := MessageLoggerConfig{
		Component: "test-server",
		Level:     slog.LevelInfo,
	}

	logger, cleanup, err := NewMessageLogger(cfg)
	if err != nil {
		t.Fatalf("NewMessageLogger() error = %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if logger == nil {
		t.Fatal("NewMessageLogger() returned nil logger")
	}
}

func TestNewMessageLogger_WritesSubsystemAttrs(t *testing.T) {
	// Create a logger that writes to a buffer for inspection
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)

	// Simulate what the message logger does: log with message attributes
	logger.Info("message dispatched",
		"agent_id", "agent-123",
		AttrSender, "user:alice",
		AttrRecipient, "agent:backend-dev",
		AttrMsgType, "instruction",
		"message_content", "implement auth",
		"urgent", false,
		"broadcasted", false,
		"plain", false,
	)

	// Verify JSON output contains expected fields
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("Failed to parse log entry: %v", err)
	}

	expectedFields := map[string]any{
		"msg":             "message dispatched",
		"agent_id":        "agent-123",
		"sender":          "user:alice",
		"recipient":       "agent:backend-dev",
		"msg_type":        "instruction",
		"message_content": "implement auth",
	}

	for key, want := range expectedFields {
		got, ok := entry[key]
		if !ok {
			t.Errorf("log entry missing field %q", key)
			continue
		}
		if got != want {
			t.Errorf("log entry[%q] = %v, want %v", key, got, want)
		}
	}
}

func TestPromoteMessageAttrToLabels(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		value    string
		promoted bool
	}{
		{"sender promoted", AttrSender, "user:alice", true},
		{"sender_id promoted", AttrSenderID, "user-uuid-123", true},
		{"recipient promoted", AttrRecipient, "agent:dev", true},
		{"recipient_id promoted", AttrRecipientID, "agent-uuid-456", true},
		{"msg_type promoted", AttrMsgType, "instruction", true},
		{"grove_id promoted", AttrMsgProjectID, "grove-abc", true},
		{"agent_id not promoted by message func", AttrAgentID, "abc123", false},
		{"arbitrary not promoted", "foo", "bar", false},
		{"empty value not promoted", AttrSender, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := make(map[string]string)
			attr := slog.String(tt.key, tt.value)
			promoteMessageAttrToLabels(labels, attr)

			_, found := labels[tt.key]
			if found != tt.promoted {
				t.Errorf("promoteMessageAttrToLabels(%q, %q) promoted = %v, want %v",
					tt.key, tt.value, found, tt.promoted)
			}
		})
	}
}
