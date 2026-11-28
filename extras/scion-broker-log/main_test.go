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

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
			Kind:        "group",
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
		"kind=group",
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
	if entry.Kind != "group" {
		t.Errorf("kind = %q, want %q", entry.Kind, "group")
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

type hubConvStore struct {
	store.Store
	convs map[string]*store.Conversation
}

func (s *hubConvStore) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	c, ok := s.convs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return c, nil
}

type logCaptureBus struct {
	jsonBuf  *bytes.Buffer
	humanBuf *bytes.Buffer
}

func (f *logCaptureBus) Publish(_ context.Context, topic string, msg *messages.StructuredMessage) error {
	wire := grpcbroker.ProtoToStructuredMessage(grpcbroker.StructuredMessageToProto(msg))
	writeJSONLine(f.jsonBuf, topic, wire, false, nil)
	writeHumanLine(f.humanBuf, topic, wire, false, nil)
	return nil
}

func (f *logCaptureBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, nil
}

func (f *logCaptureBus) Close() error { return nil }

// TestBrokerLog_Publish_HubEnrichedOptionAShapes drives broker-log formatting
// from REAL hub-enriched shapes produced by hub.MessageBrokerProxy and
// round-tripped through grpcbroker proto conversion:
//  1. External thread (Kind:"group", translated ExternalRef, empty ThreadID)
//  2. Direct user DM (Kind:"direct", Surface:"native", empty ExternalRef, bogus
//     ThreadID ignored)
//  3. Agent-to-agent observer DM (Kind:"direct", ObserverOnly:true, Channel:"")
func TestBrokerLog_Publish_HubEnrichedOptionAShapes(t *testing.T) {
	ctx := context.Background()
	projectID := "p1"
	threadKey, threadKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		ThreadID:  "C0123ABC:1700000000.000100",
		ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("DeriveConversationKey(thread): %v", err)
	}
	dmKey, dmKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      "11111111-1111-1111-1111-111111111111",
		RecipientKind: "user",
		RecipientID:   "22222222-2222-2222-2222-222222222222",
	})
	if err != nil {
		t.Fatalf("DeriveConversationKey(dm): %v", err)
	}
	a2aKey, a2aKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      "11111111-1111-1111-1111-111111111111",
		RecipientKind: "agent",
		RecipientID:   "33333333-3333-3333-3333-333333333333",
	})
	if err != nil {
		t.Fatalf("DeriveConversationKey(a2a): %v", err)
	}

	convStore := &hubConvStore{
		convs: map[string]*store.Conversation{
			"conv-thread": {
				ID:          "conv-thread",
				ProjectID:   &projectID,
				Kind:        threadKind,
				Surface:     "slack",
				ExternalRef: threadKey,
				ParentRef:   "C0123ABC",
				DisplayName: "#eng",
			},
			"conv-dm": {
				ID:          "conv-dm",
				ProjectID:   &projectID,
				Kind:        dmKind,
				Surface:     "native",
				ExternalRef: dmKey,
				DisplayName: "Direct Message",
			},
			"conv-a2a": {
				ID:          "conv-a2a",
				ProjectID:   &projectID,
				Kind:        a2aKind,
				Surface:     "native",
				ExternalRef: a2aKey,
			},
		},
	}

	var jsonBuf, humanBuf bytes.Buffer
	proxy := hub.NewMessageBrokerProxy(&logCaptureBus{jsonBuf: &jsonBuf, humanBuf: &humanBuf}, convStore, nil, nil, slog.Default())

	// 1. External thread reply.
	if err := proxy.PublishUserMessage(ctx, projectID, "u1", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Type:           messages.TypeInstruction,
		Msg:            "thread reply",
		Channel:        "slack",
		ConversationID: "conv-thread",
	}); err != nil {
		t.Fatalf("PublishUserMessage(thread): %v", err)
	}

	// 2. Direct user DM with bogus ThreadID that must be ignored when Conversation != nil.
	if err := proxy.PublishUserMessage(ctx, projectID, "u1", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Type:           messages.TypeInstruction,
		Msg:            "dm reply",
		Channel:        "slack",
		ThreadID:       "bogus-stale-thread",
		ConversationID: "conv-dm",
	}); err != nil {
		t.Fatalf("PublishUserMessage(dm): %v", err)
	}

	// 3. Agent-to-agent observer DM.
	if err := proxy.PublishMessage(ctx, projectID, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "agent:reviewer",
		Type:           messages.TypeInstruction,
		Msg:            "a2a observer",
		ObserverOnly:   true,
		ConversationID: "conv-a2a",
	}); err != nil {
		t.Fatalf("PublishMessage(a2a): %v", err)
	}

	lines := strings.Split(strings.TrimSpace(jsonBuf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 JSON lines, got %d:\n%s", len(lines), jsonBuf.String())
	}

	var entries [3]jsonEntry
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &entries[i]); err != nil {
			t.Fatalf("unmarshal entry %d: %v", i, err)
		}
	}

	if entries[0].Kind != "group" || entries[0].Surface != "slack" || entries[0].ExternalRef != "C0123ABC:1700000000.000100" {
		t.Errorf("entry[0] = %+v, want kind=group surface=slack external_ref=C0123ABC:1700000000.000100", entries[0])
	}
	if entries[1].Kind != "direct" || entries[1].Surface != "slack" || entries[1].ExternalRef != "" {
		t.Errorf("entry[1] = %+v, want kind=direct surface=slack empty external_ref (ThreadID ignored)", entries[1])
	}
	if entries[2].Kind != "direct" || entries[2].Surface != "" || entries[2].ExternalRef != "" {
		t.Errorf("entry[2] = %+v, want kind=direct empty surface/external_ref", entries[2])
	}

	human := humanBuf.String()
	if !strings.Contains(human, "kind=group") || !strings.Contains(human, "kind=direct") {
		t.Errorf("human output missing kind=group/kind=direct:\n%s", human)
	}
	if strings.Contains(human, "bogus-stale-thread") {
		t.Errorf("human output leaked bogus ThreadID:\n%s", human)
	}
}
