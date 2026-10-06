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

// TestTranslateExplicitReplyProducesArtifact pins the message types an
// explicit agent reply (`scion message user:<caller> ...`) arrives as. Each
// must yield a task artifact on both translation paths, independent of any
// harness-specific turn output.
func TestTranslateExplicitReplyProducesArtifact(t *testing.T) {
	for _, typ := range []string{messages.TypeInstruction, ""} {
		t.Run("type="+typ, func(t *testing.T) {
			reply := &messages.StructuredMessage{
				Version: 1,
				Sender:  "agent:agent-a",
				Msg:     "explicit reply",
				Type:    typ,
			}

			_, artifacts := TranslateScionToA2A(reply)
			if len(artifacts) != 1 {
				t.Fatalf("TranslateScionToA2A artifacts = %d, want 1", len(artifacts))
			}
			if len(artifacts[0].Parts) < 1 {
				t.Fatalf("TranslateScionToA2A artifact has no parts: %+v", artifacts[0])
			}
			if artifacts[0].Parts[0].Text != "explicit reply" {
				t.Errorf("TranslateScionToA2A artifacts = %+v, want one with the reply", artifacts)
			}

			_, sdkArtifacts := TranslateScionToA2AParts(reply)
			if len(sdkArtifacts) != 1 {
				t.Fatalf("TranslateScionToA2AParts artifacts = %d, want 1", len(sdkArtifacts))
			}
			if len(sdkArtifacts[0].Parts) < 1 {
				t.Fatalf("TranslateScionToA2AParts artifact has no parts: %+v", sdkArtifacts[0])
			}
			if sdkArtifacts[0].Parts[0].Text() != "explicit reply" {
				t.Errorf("TranslateScionToA2AParts artifacts = %+v, want one with the reply", sdkArtifacts)
			}
		})
	}
}

// TestTranslateInputNeededProducesArtifact covers ptone/scion#3377: an agent
// asking the A2A caller for input must produce an artifact carrying the
// question, on both translation paths.
func TestTranslateInputNeededProducesArtifact(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: 1,
		Sender:  "agent:agent-a",
		Msg:     "Which region should I deploy to?",
		Type:    messages.TypeInputNeeded,
	}

	_, artifacts := TranslateScionToA2A(msg)
	if len(artifacts) != 1 {
		t.Fatalf("TranslateScionToA2A artifacts = %d, want 1", len(artifacts))
	}
	if len(artifacts[0].Parts) < 1 {
		t.Fatalf("TranslateScionToA2A artifact has no parts: %+v", artifacts[0])
	}
	if artifacts[0].Parts[0].Text != msg.Msg {
		t.Errorf("TranslateScionToA2A artifacts = %+v, want one with the question", artifacts)
	}

	_, sdkArtifacts := TranslateScionToA2AParts(msg)
	if len(sdkArtifacts) != 1 {
		t.Fatalf("TranslateScionToA2AParts artifacts = %d, want 1", len(sdkArtifacts))
	}
	if len(sdkArtifacts[0].Parts) < 1 {
		t.Fatalf("TranslateScionToA2AParts artifact has no parts: %+v", sdkArtifacts[0])
	}
	if sdkArtifacts[0].Parts[0].Text() != msg.Msg {
		t.Errorf("TranslateScionToA2AParts artifacts = %+v, want one with the question", sdkArtifacts)
	}
}

// The retired end-of-turn assistant-reply mirror no longer becomes an
// artifact on either translation path.
func TestTranslateRetiredAssistantReplyProducesNoArtifact(t *testing.T) {
	mirror := &messages.StructuredMessage{Version: 1, Msg: "turn text", Type: messages.TypeAssistantReply}
	if _, arts := TranslateScionToA2A(mirror); len(arts) != 0 {
		t.Errorf("assistant-reply: artifacts = %d, want 0", len(arts))
	}
	if _, arts := TranslateScionToA2AParts(mirror); len(arts) != 0 {
		t.Errorf("assistant-reply (SDK): artifacts = %d, want 0", len(arts))
	}
}
