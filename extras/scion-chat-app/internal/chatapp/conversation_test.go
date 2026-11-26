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

package chatapp

import (
	"context"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestChatApp_PublishAndRelay_ConversationRoutingAndFallback(t *testing.T) {
	store := newTestStore(t)

	if err := store.SetUserMapping(&state.UserMapping{
		PlatformUserID: "users/12345",
		Platform:       "googlechat",
		HubUserID:      "hub-user-1",
		HubUserEmail:   "alice@example.com",
		RegisteredBy:   "auto",
	}); err != nil {
		t.Fatalf("setting user mapping: %v", err)
	}

	if err := store.SetSpaceLink(&state.SpaceLink{
		SpaceID:     "spaces/AAQAx",
		Platform:    "googlechat",
		ProjectID:   "project-abc",
		ProjectSlug: "my-project",
		LinkedBy:    "test",
	}); err != nil {
		t.Fatalf("setting space link: %v", err)
	}

	fm := &fakeMessenger{}
	relay := NewNotificationRelay(store, fm, slog.Default())
	broker := NewBrokerServer(relay.HandleBrokerMessage, slog.Default())
	if err := broker.Configure(map[string]string{}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	topic := "scion.project.project-abc.user.hub-user-1.messages"
	ctx := context.Background()

	// 1. Outbound with msg.Conversation only (empty legacy Channel/ThreadID) routes to gchat thread.
	err := broker.Publish(ctx, topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		RecipientID:    "hub-user-1",
		Msg:            "phase 11 gchat message",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-1",
		Conversation: &messages.Conversation{
			Surface:     "gchat",
			ExternalRef: "spaces/AAQAx/threads/thread-p11",
			ParentRef:   "spaces/AAQAx",
			DisplayName: "Eng Space",
		},
	})
	if err != nil {
		t.Fatalf("Publish (gchat conversation) failed: %v", err)
	}
	if len(fm.messages) != 1 {
		t.Fatalf("expected 1 delivered message, got %d", len(fm.messages))
	}
	if fm.messages[0].SpaceID != "spaces/AAQAx" {
		t.Errorf("expected SpaceID spaces/AAQAx, got %q", fm.messages[0].SpaceID)
	}
	if fm.messages[0].ThreadID != "spaces/AAQAx/threads/thread-p11" {
		t.Errorf("expected ThreadID spaces/AAQAx/threads/thread-p11, got %q", fm.messages[0].ThreadID)
	}

	// 2. Outbound targeting another external surface ("slack") is ignored.
	err = broker.Publish(ctx, topic, &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      "agent:coder",
		RecipientID: "hub-user-1",
		Msg:         "slack message",
		Type:        messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "C0123ABC:1700000000.000100",
		},
	})
	if err != nil {
		t.Fatalf("Publish (slack surface) failed: %v", err)
	}
	if len(fm.messages) != 1 {
		t.Fatalf("expected slack message to be ignored, got %d total deliveries", len(fm.messages))
	}

	// 2b. Outbound targeting native webchat surface is ignored (both via Publish and direct HandleBrokerMessage).
	nativeMsg := &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      "agent:coder",
		RecipientID: "hub-user-1",
		Msg:         "native message",
		Type:        messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "thread:project-abc:topic-1",
		},
	}
	if err := broker.Publish(ctx, topic, nativeMsg); err != nil {
		t.Fatalf("Publish (native surface) failed: %v", err)
	}
	if err := relay.HandleBrokerMessage(ctx, topic, nativeMsg); err != nil {
		t.Fatalf("HandleBrokerMessage (native surface) failed: %v", err)
	}
	if len(fm.messages) != 1 {
		t.Fatalf("expected native message to be ignored, got %d total deliveries", len(fm.messages))
	}

	// 3. Legacy fallback (older hub sends Channel/ThreadID without Conversation).
	err = broker.Publish(ctx, topic, &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      "agent:coder",
		RecipientID: "hub-user-1",
		Msg:         "legacy hub message",
		Type:        messages.TypeInstruction,
		Channel:     "gchat",
		ThreadID:    "spaces/AAQAx/threads/thread-legacy",
	})
	if err != nil {
		t.Fatalf("Publish (legacy fallback) failed: %v", err)
	}
	if len(fm.messages) != 2 {
		t.Fatalf("expected 2 total deliveries after legacy fallback, got %d", len(fm.messages))
	}
	if fm.messages[1].ThreadID != "spaces/AAQAx/threads/thread-legacy" {
		t.Errorf("expected legacy ThreadID spaces/AAQAx/threads/thread-legacy, got %q", fm.messages[1].ThreadID)
	}
}

func TestChatAppInboundConversation(t *testing.T) {
	// Threaded message populates surface, external_ref, and parent_ref.
	conv := chatAppInboundConversation("", "spaces/AAQAx", "spaces/AAQAx/threads/T1")
	if conv == nil {
		t.Fatal("expected non-nil Conversation for threaded message")
	}
	if conv.Surface != "gchat" {
		t.Errorf("expected surface gchat, got %q", conv.Surface)
	}
	if conv.ExternalRef != "spaces/AAQAx/threads/T1" {
		t.Errorf("expected external_ref spaces/AAQAx/threads/T1, got %q", conv.ExternalRef)
	}
	if conv.ParentRef != "spaces/AAQAx" {
		t.Errorf("expected parent_ref spaces/AAQAx, got %q", conv.ParentRef)
	}

	// Space-level message (no thread) populates surface and external_ref without parent_ref.
	spaceConv := chatAppInboundConversation("gchat", "spaces/AAQAx", "")
	if spaceConv == nil {
		t.Fatal("expected non-nil Conversation for space-level message")
	}
	if spaceConv.Surface != "gchat" || spaceConv.ExternalRef != "spaces/AAQAx" || spaceConv.ParentRef != "" {
		t.Errorf("unexpected space-level Conversation: %+v", spaceConv)
	}

	// Neither space nor thread returns nil.
	if empty := chatAppInboundConversation("gchat", "", ""); empty != nil {
		t.Errorf("expected nil Conversation when space and thread are empty, got %+v", empty)
	}
}
