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

package bridge

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestMapActivityToTaskState(t *testing.T) {
	tests := []struct {
		activity string
		want     string
	}{
		{"WORKING", TaskStateWorking},
		{"THINKING", TaskStateWorking},
		{"EXECUTING", TaskStateWorking},
		{"WAITING_FOR_INPUT", TaskStateInputRequired},
		{"COMPLETED", TaskStateCompleted},
		{"ERROR", TaskStateFailed},
		{"STALLED", TaskStateFailed},
		{"LIMITS_EXCEEDED", TaskStateFailed},
		{"OFFLINE", TaskStateFailed},
		{"UNKNOWN_ACTIVITY", TaskStateWorking},
		{"working", TaskStateWorking},
	}

	for _, tt := range tests {
		t.Run(tt.activity, func(t *testing.T) {
			got := MapActivityToTaskState(tt.activity)
			if got != tt.want {
				t.Errorf("MapActivityToTaskState(%q) = %q, want %q", tt.activity, got, tt.want)
			}
		})
	}
}

func TestIsTerminalState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{TaskStateCompleted, true},
		{TaskStateFailed, true},
		{TaskStateCanceled, true},
		{TaskStateRejected, true},
		{TaskStateSubmitted, false},
		{TaskStateWorking, false},
		{TaskStateInputRequired, false},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			got := IsTerminalState(tt.state)
			if got != tt.want {
				t.Errorf("IsTerminalState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestTranslateA2AToScion(t *testing.T) {
	parts := []Part{
		{Text: "Hello, agent!"},
		{Text: "How are you?"},
		{URL: "https://example.com/file.pdf"},
	}

	msg := TranslateA2AToScion(parts)

	if msg.Msg != "Hello, agent!\nHow are you?" {
		t.Errorf("Msg = %q, want concatenated text", msg.Msg)
	}
	if len(msg.Attachments) != 1 {
		t.Errorf("Attachments = %d, want 1", len(msg.Attachments))
	}
	if msg.Attachments[0] != "https://example.com/file.pdf" {
		t.Errorf("Attachment = %q, want URL", msg.Attachments[0])
	}
	if msg.Type != messages.TypeInstruction {
		t.Errorf("Type = %q, want %q", msg.Type, messages.TypeInstruction)
	}
	if msg.Version != 1 {
		t.Errorf("Version = %d, want 1", msg.Version)
	}
}

func TestTranslateA2AToScionWithData(t *testing.T) {
	parts := []Part{
		{Data: map[string]string{"key": "value"}},
	}

	msg := TranslateA2AToScion(parts)

	if msg.Msg != `{"key":"value"}` {
		t.Errorf("Msg = %q, want JSON data", msg.Msg)
	}
}

func TestTranslateScionToA2A(t *testing.T) {
	scionMsg := &messages.StructuredMessage{
		Version:     1,
		Msg:         "Task completed successfully",
		Type:        messages.TypeInstruction,
		Attachments: []string{"https://example.com/output.txt"},
	}

	msg, artifacts := TranslateScionToA2A(scionMsg)

	if msg.Role != RoleAgent {
		t.Errorf("Role = %q, want %q", msg.Role, RoleAgent)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2", len(msg.Parts))
	}
	if msg.Parts[0].Text != "Task completed successfully" {
		t.Errorf("Parts[0].Text = %q, want message text", msg.Parts[0].Text)
	}
	if msg.Parts[1].URL != "https://example.com/output.txt" {
		t.Errorf("Parts[1].URL = %q, want attachment URL", msg.Parts[1].URL)
	}
	if len(artifacts) != 1 {
		t.Fatalf("Artifacts = %d, want 1", len(artifacts))
	}
	if !artifacts[0].LastChunk {
		t.Error("expected LastChunk = true")
	}
}

func TestTranslateScionToA2APartsNilMessage(t *testing.T) {
	msg, artifacts := TranslateScionToA2AParts(nil)
	if msg == nil {
		t.Fatal("expected non-nil message for nil input")
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("Parts = %d, want 1", len(msg.Parts))
	}
	if artifacts != nil {
		t.Errorf("Artifacts = %v, want nil for nil input", artifacts)
	}
}

func TestTranslateScionToA2AStateChange(t *testing.T) {
	scionMsg := &messages.StructuredMessage{
		Version: 1,
		Msg:     "Agent state changed",
		Type:    messages.TypeStateChange,
	}

	_, artifacts := TranslateScionToA2A(scionMsg)

	if len(artifacts) != 0 {
		t.Errorf("Artifacts = %d, want 0 for state-change messages", len(artifacts))
	}
}

// --- SDK translation function tests ---

func TestTranslateA2APartsToScionText(t *testing.T) {
	parts := a2a.ContentParts{
		{Content: a2a.Text("Hello"), MediaType: "text/plain"},
		{Content: a2a.Text("World"), MediaType: "text/plain"},
	}

	msg := TranslateA2APartsToScion(parts)

	if msg.Msg != "Hello\nWorld" {
		t.Errorf("Msg = %q, want %q", msg.Msg, "Hello\nWorld")
	}
	if msg.Type != messages.TypeInstruction {
		t.Errorf("Type = %q, want %q", msg.Type, messages.TypeInstruction)
	}
	if msg.Version != 1 {
		t.Errorf("Version = %d, want 1", msg.Version)
	}
	if msg.Timestamp == "" {
		t.Error("expected non-empty Timestamp")
	}
}

func TestTranslateA2APartsToScionURL(t *testing.T) {
	parts := a2a.ContentParts{
		{Content: a2a.Text("See this file:"), MediaType: "text/plain"},
		{Content: a2a.URL("https://example.com/file.pdf")},
	}

	msg := TranslateA2APartsToScion(parts)

	if msg.Msg != "See this file:" {
		t.Errorf("Msg = %q, want %q", msg.Msg, "See this file:")
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0] != "https://example.com/file.pdf" {
		t.Errorf("Attachments = %v, want [https://example.com/file.pdf]", msg.Attachments)
	}
}

func TestTranslateA2APartsToScionData(t *testing.T) {
	parts := a2a.ContentParts{
		{Content: a2a.Data{Value: map[string]interface{}{"key": "value"}}},
	}

	msg := TranslateA2APartsToScion(parts)

	if msg.Msg != `{"key":"value"}` {
		t.Errorf("Msg = %q, want JSON data", msg.Msg)
	}
}

func TestTranslateA2APartsToScionEmpty(t *testing.T) {
	msg := TranslateA2APartsToScion(nil)

	if msg.Msg != "[empty A2A request]" {
		t.Errorf("Msg = %q, want %q", msg.Msg, "[empty A2A request]")
	}
}

func TestTranslateA2APartsToScionAttachmentOnly(t *testing.T) {
	parts := a2a.ContentParts{
		{Content: a2a.URL("https://example.com/data.csv")},
	}

	msg := TranslateA2APartsToScion(parts)

	if msg.Msg != "[A2A request with attachments only]" {
		t.Errorf("Msg = %q, want attachment-only placeholder", msg.Msg)
	}
	if len(msg.Attachments) != 1 {
		t.Errorf("Attachments = %d, want 1", len(msg.Attachments))
	}
}

func TestTranslateScionToA2AParts(t *testing.T) {
	scionMsg := &messages.StructuredMessage{
		Version:     1,
		Msg:         "Agent response text",
		Type:        messages.TypeAssistantReply,
		Attachments: []string{"https://example.com/output.pdf"},
	}

	message, artifacts := TranslateScionToA2AParts(scionMsg)

	if message == nil {
		t.Fatal("expected non-nil message")
	}
	if message.Role != a2a.MessageRoleAgent {
		t.Errorf("Role = %v, want %v", message.Role, a2a.MessageRoleAgent)
	}
	if len(message.Parts) != 2 {
		t.Fatalf("Parts = %d, want 2", len(message.Parts))
	}
	if text, ok := message.Parts[0].Content.(a2a.Text); !ok || string(text) != "Agent response text" {
		t.Errorf("Parts[0] = %v, want Text('Agent response text')", message.Parts[0].Content)
	}
	if url, ok := message.Parts[1].Content.(a2a.URL); !ok || string(url) != "https://example.com/output.pdf" {
		t.Errorf("Parts[1] = %v, want URL attachment", message.Parts[1].Content)
	}

	if len(artifacts) != 1 {
		t.Fatalf("Artifacts = %d, want 1 for assistant reply", len(artifacts))
	}
	if artifacts[0].ID == "" {
		t.Error("expected non-empty artifact ID")
	}
}

func TestTranslateScionToA2APartsStateChange(t *testing.T) {
	scionMsg := &messages.StructuredMessage{
		Version: 1,
		Msg:     "State changed",
		Type:    messages.TypeStateChange,
	}

	_, artifacts := TranslateScionToA2AParts(scionMsg)

	if len(artifacts) != 0 {
		t.Errorf("Artifacts = %d, want 0 for state-change", len(artifacts))
	}
}

func TestMapActivityToSDKTaskState(t *testing.T) {
	tests := []struct {
		activity string
		want     a2a.TaskState
	}{
		{"WORKING", a2a.TaskStateWorking},
		{"THINKING", a2a.TaskStateWorking},
		{"EXECUTING", a2a.TaskStateWorking},
		{"WAITING_FOR_INPUT", a2a.TaskStateInputRequired},
		{"COMPLETED", a2a.TaskStateCompleted},
		{"ERROR", a2a.TaskStateFailed},
		{"STALLED", a2a.TaskStateFailed},
		{"LIMITS_EXCEEDED", a2a.TaskStateFailed},
		{"OFFLINE", a2a.TaskStateFailed},
		{"UNKNOWN_ACTIVITY", a2a.TaskStateWorking},
		{"working", a2a.TaskStateWorking},
	}

	for _, tt := range tests {
		t.Run(tt.activity, func(t *testing.T) {
			got := MapActivityToSDKTaskState(tt.activity)
			if got != tt.want {
				t.Errorf("MapActivityToSDKTaskState(%q) = %q, want %q", tt.activity, got, tt.want)
			}
		})
	}
}
