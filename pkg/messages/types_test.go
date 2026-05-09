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

package messages

import (
	"strings"
	"testing"
)

func TestValidateType(t *testing.T) {
	tests := []struct {
		typ     string
		wantErr bool
	}{
		{TypeInstruction, false},
		{TypeInputNeeded, false},
		{TypeStateChange, false},
		{TypeAssistantReply, false},
		{"unknown", true},
		{"", true},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			err := ValidateType(tt.typ)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateType(%q) error = %v, wantErr %v", tt.typ, err, tt.wantErr)
			}
		})
	}
}

func TestStructuredMessage_Validate(t *testing.T) {
	validMsg := func() *StructuredMessage {
		return &StructuredMessage{
			Version:   Version,
			Timestamp: "2026-03-07T14:30:00Z",
			Sender:    "user:alice",
			Recipient: "agent:backend-dev",
			Msg:       "implement auth",
			Type:      TypeInstruction,
		}
	}

	t.Run("valid message", func(t *testing.T) {
		if err := validMsg().Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		m := validMsg()
		m.Version = 99
		if err := m.Validate(); err == nil {
			t.Error("expected error for wrong version")
		}
	})

	t.Run("empty msg", func(t *testing.T) {
		m := validMsg()
		m.Msg = ""
		if err := m.Validate(); err == nil {
			t.Error("expected error for empty msg")
		}
	})

	t.Run("msg too large", func(t *testing.T) {
		m := validMsg()
		m.Msg = strings.Repeat("x", MaxMsgSize+1)
		if err := m.Validate(); err == nil {
			t.Error("expected error for oversized msg")
		}
	})

	t.Run("invalid type", func(t *testing.T) {
		m := validMsg()
		m.Type = "bogus"
		if err := m.Validate(); err == nil {
			t.Error("expected error for invalid type")
		}
	})

	t.Run("empty sender", func(t *testing.T) {
		m := validMsg()
		m.Sender = ""
		if err := m.Validate(); err == nil {
			t.Error("expected error for empty sender")
		}
	})

	t.Run("empty recipient", func(t *testing.T) {
		m := validMsg()
		m.Recipient = ""
		if err := m.Validate(); err == nil {
			t.Error("expected error for empty recipient")
		}
	})

	t.Run("too many attachments", func(t *testing.T) {
		m := validMsg()
		m.Attachments = make([]string, MaxAttachments+1)
		if err := m.Validate(); err == nil {
			t.Error("expected error for too many attachments")
		}
	})

	t.Run("valid with attachments", func(t *testing.T) {
		m := validMsg()
		m.Attachments = []string{"file1.go", "file2.go"}
		if err := m.Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestNewInstruction(t *testing.T) {
	m := NewInstruction("user:alice", "agent:dev", "do something")
	if m.Version != Version {
		t.Errorf("version = %d, want %d", m.Version, Version)
	}
	if m.Type != TypeInstruction {
		t.Errorf("type = %q, want %q", m.Type, TypeInstruction)
	}
	if m.Sender != "user:alice" {
		t.Errorf("sender = %q, want %q", m.Sender, "user:alice")
	}
	if m.Recipient != "agent:dev" {
		t.Errorf("recipient = %q, want %q", m.Recipient, "agent:dev")
	}
	if m.Msg != "do something" {
		t.Errorf("msg = %q, want %q", m.Msg, "do something")
	}
	if m.Timestamp == "" {
		t.Error("timestamp should be set")
	}
}

func TestNewNotification(t *testing.T) {
	m := NewNotification("agent:worker", "agent:lead", "worker has completed", TypeStateChange)
	if m.Version != Version {
		t.Errorf("version = %d, want %d", m.Version, Version)
	}
	if m.Type != TypeStateChange {
		t.Errorf("type = %q, want %q", m.Type, TypeStateChange)
	}
	if m.Sender != "agent:worker" {
		t.Errorf("sender = %q, want %q", m.Sender, "agent:worker")
	}
	if m.Recipient != "agent:lead" {
		t.Errorf("recipient = %q, want %q", m.Recipient, "agent:lead")
	}
	if m.Msg != "worker has completed" {
		t.Errorf("msg = %q, want %q", m.Msg, "worker has completed")
	}
	if m.Timestamp == "" {
		t.Error("timestamp should be set")
	}

	// Test with input-needed type
	m2 := NewNotification("agent:helper", "agent:lead", "needs input", TypeInputNeeded)
	if m2.Type != TypeInputNeeded {
		t.Errorf("type = %q, want %q", m2.Type, TypeInputNeeded)
	}
}

func TestLogAttrs(t *testing.T) {
	m := &StructuredMessage{
		Version:     Version,
		Sender:      "user:alice",
		SenderID:    "user-uuid-123",
		Recipient:   "agent:dev",
		RecipientID: "agent-uuid-456",
		Msg:         "hello",
		Type:        TypeInstruction,
		Urgent:      true,
		Broadcasted: false,
		Plain:       true,
	}

	attrs := m.LogAttrs()

	// Should contain 10 key-value pairs (20 elements) when IDs are set
	if len(attrs) != 20 {
		t.Fatalf("LogAttrs() returned %d elements, want 20", len(attrs))
	}

	// Verify key-value pairs
	expected := map[string]any{
		"sender":          "user:alice",
		"sender_id":       "user-uuid-123",
		"recipient":       "agent:dev",
		"recipient_id":    "agent-uuid-456",
		"msg_type":        TypeInstruction,
		"message_content": "hello",
		"urgent":          true,
		"broadcasted":     false,
		"plain":           true,
		"raw":             false,
	}
	for i := 0; i < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if !ok {
			t.Errorf("attrs[%d] is not a string key", i)
			continue
		}
		want, exists := expected[key]
		if !exists {
			t.Errorf("unexpected key %q in LogAttrs", key)
			continue
		}
		if attrs[i+1] != want {
			t.Errorf("LogAttrs()[%q] = %v, want %v", key, attrs[i+1], want)
		}
	}
}

func TestLogAttrsWithoutIDs(t *testing.T) {
	m := &StructuredMessage{
		Version:   Version,
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "hello",
		Type:      TypeInstruction,
	}

	attrs := m.LogAttrs()

	// Without IDs, should contain 8 key-value pairs (16 elements)
	if len(attrs) != 16 {
		t.Fatalf("LogAttrs() returned %d elements, want 16", len(attrs))
	}

	// Verify sender_id and recipient_id are not present
	for i := 0; i < len(attrs); i += 2 {
		key := attrs[i].(string)
		if key == "sender_id" || key == "recipient_id" {
			t.Errorf("LogAttrs() should not include %q when empty", key)
		}
	}
}

func TestSenderPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"user:alice", "user"},
		{"agent:code-reviewer", "agent"},
		{"system:notifications", "system"},
		{"all", "all"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := SenderPrefix(tt.input); got != tt.want {
				t.Errorf("SenderPrefix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
