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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestWriteConversationTarget_Phase11Fields(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Type:           messages.TypeInstruction,
		Msg:            "hello world",
		ConversationID: "conv-123",
		Conversation: &messages.Conversation{
			Surface:     "discord",
			ExternalRef: "123456789012345678",
			ParentRef:   "987654321098765432",
			DisplayName: "#eng-thread",
		},
	}

	var humanBuf bytes.Buffer
	writeHumanLine(&humanBuf, "scion.project.p1.user.u1.messages", msg, false, nil)
	human := humanBuf.String()
	for _, want := range []string{
		"conversation=conv-123",
		"surface=discord",
		"external_ref=123456789012345678",
		"parent_ref=987654321098765432",
		`display_name="#eng-thread"`,
	} {
		if !strings.Contains(human, want) {
			t.Errorf("human output missing %q:\n%s", want, human)
		}
	}

	var jsonBuf bytes.Buffer
	writeJSONLine(&jsonBuf, "scion.project.p1.user.u1.messages", msg, false, nil)
	var entry jsonEntry
	if err := json.Unmarshal(jsonBuf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to unmarshal JSON log entry: %v", err)
	}
	if entry.ConversationID != "conv-123" {
		t.Errorf("conversation_id = %q, want %q", entry.ConversationID, "conv-123")
	}
	if entry.Surface != "discord" {
		t.Errorf("surface = %q, want %q", entry.Surface, "discord")
	}
	if entry.ExternalRef != "123456789012345678" {
		t.Errorf("external_ref = %q, want %q", entry.ExternalRef, "123456789012345678")
	}
	if entry.ParentRef != "987654321098765432" {
		t.Errorf("parent_ref = %q, want %q", entry.ParentRef, "987654321098765432")
	}
	if entry.DisplayName != "#eng-thread" {
		t.Errorf("display_name = %q, want %q", entry.DisplayName, "#eng-thread")
	}
}

func TestWriteConversationTarget_LegacyFallback(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Type:      messages.TypeInstruction,
		Msg:       "legacy hub message",
		Channel:   "slack",
		ThreadID:  "C0123ABC:1700000000.000100",
	}

	var humanBuf bytes.Buffer
	writeHumanLine(&humanBuf, "scion.project.p1.user.u1.messages", msg, false, nil)
	human := humanBuf.String()
	for _, want := range []string{
		"surface=slack",
		"external_ref=C0123ABC:1700000000.000100",
	} {
		if !strings.Contains(human, want) {
			t.Errorf("human output missing %q:\n%s", want, human)
		}
	}

	var jsonBuf bytes.Buffer
	writeJSONLine(&jsonBuf, "scion.project.p1.user.u1.messages", msg, false, nil)
	var entry jsonEntry
	if err := json.Unmarshal(jsonBuf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to unmarshal JSON log entry: %v", err)
	}
	if entry.Surface != "slack" {
		t.Errorf("surface = %q, want %q", entry.Surface, "slack")
	}
	if entry.ExternalRef != "C0123ABC:1700000000.000100" {
		t.Errorf("external_ref = %q, want %q", entry.ExternalRef, "C0123ABC:1700000000.000100")
	}
}

func TestBrokerLog_ObserverLogsAllSurfaces(t *testing.T) {
	bl := &brokerLog{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		jsonOutput:    true,
		subscriptions: make(map[string]bool),
	}

	for _, surface := range []string{"", "native", "slack", "discord", "telegram"} {
		err := bl.Publish(context.Background(), "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
			Version:   messages.Version,
			Sender:    "agent:coder",
			Recipient: "user:alice",
			Type:      messages.TypeInstruction,
			Msg:       "observe " + surface,
			Conversation: &messages.Conversation{
				Surface:     surface,
				ExternalRef: "ref-" + surface,
			},
		})
		if err != nil {
			t.Fatalf("Publish(surface=%q) returned error: %v", surface, err)
		}
	}
	if got := bl.msgCount.Load(); got != 5 {
		t.Errorf("msgCount = %d, want 5 (observer must not filter any surface)", got)
	}
}
