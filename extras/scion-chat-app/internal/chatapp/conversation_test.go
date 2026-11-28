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
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	hubstore "github.com/GoogleCloudPlatform/scion/pkg/store"
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
		SpaceID:          "spaces/AAQAx",
		Platform:         "googlechat",
		ProjectID:        "project-abc",
		ProjectSlug:      "my-project",
		LinkedBy:         "test",
		ShowAgentToAgent: true,
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

	// 2b. Defeat scenario (c): Channel:"web" user DM with native DM conversation
	// MUST be relayed (matching pre-P4 behavior: user-targeted messages are always relayed).
	webDM := &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		RecipientID:    "hub-user-1",
		Msg:            "web user DM",
		Type:           messages.TypeInstruction,
		Channel:        "web",
		ThreadID:       "dm:agent:a1:user:hub-user-1",
		ConversationID: "conv-web-dm",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "dm:agent:a1:user:hub-user-1",
		},
	}
	if err := broker.Publish(ctx, topic, webDM); err != nil {
		t.Fatalf("Publish (web user DM) failed: %v", err)
	}
	if len(fm.messages) != 2 {
		t.Fatalf("expected web user DM to be relayed, got %d total deliveries", len(fm.messages))
	}

	// 2c. Defeat scenario (b): agent-to-agent observer copy with native DM conv
	// (Channel == "") MUST be relayed to ShowAgentToAgent links.
	a2aTopic := "scion.project.project-abc.agent.reviewer.messages"
	a2aMsg := &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "agent:reviewer",
		Msg:            "a2a observer copy",
		Type:           messages.TypeInstruction,
		ObserverOnly:   true,
		ConversationID: "conv-a2a",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "dm:agent:a1:agent:a2",
		},
	}
	if err := broker.Publish(ctx, a2aTopic, a2aMsg); err != nil {
		t.Fatalf("Publish (a2a observer) failed: %v", err)
	}
	if len(fm.messages) != 3 {
		t.Fatalf("expected a2a observer copy to be relayed, got %d total deliveries", len(fm.messages))
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
	if len(fm.messages) != 4 {
		t.Fatalf("expected 4 total deliveries after legacy fallback, got %d", len(fm.messages))
	}
	if fm.messages[3].ThreadID != "spaces/AAQAx/threads/thread-legacy" {
		t.Errorf("expected legacy ThreadID spaces/AAQAx/threads/thread-legacy, got %q", fm.messages[3].ThreadID)
	}
}

func TestChatApp_R3_PluginNameGoogleChatSingleGate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.SetSpaceLink(&state.SpaceLink{
		SpaceID:     "spaces/AAQAx",
		ProjectID:   "project-abc",
		ProjectSlug: "my-project",
		Platform:    "googlechat",
		LinkedBy:    "test",
	}); err != nil {
		t.Fatalf("SetSpaceLink: %v", err)
	}
	if err := st.SetUserMapping(&state.UserMapping{
		PlatformUserID: "users/12345",
		Platform:       "googlechat",
		HubUserID:      "hub-user-1",
		HubUserEmail:   "alice@example.com",
		RegisteredBy:   "auto",
	}); err != nil {
		t.Fatalf("SetUserMapping: %v", err)
	}

	fm := &fakeMessenger{}
	relay := NewNotificationRelay(st, fm, slog.Default())
	broker := NewBrokerServer(relay.HandleBrokerMessage, slog.Default())
	// Documented plugin_name in README is "googlechat" while canonical surface is "gchat".
	if err := broker.Configure(map[string]string{"plugin_name": "googlechat"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	topic := "scion.project.project-abc.user.hub-user-1.messages"

	// 1. Affinity reply carrying Channel:"googlechat" (routing key) must be relayed.
	if err := broker.Publish(ctx, topic, &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      "agent:coder",
		RecipientID: "hub-user-1",
		Msg:         "affinity reply on googlechat",
		Type:        messages.TypeInstruction,
		Channel:     "googlechat",
		ThreadID:    "spaces/AAQAx/threads/t1",
	}); err != nil {
		t.Fatalf("Publish(Channel=googlechat): %v", err)
	}

	// 2. Reply carrying Conversation.Surface:"gchat" (canonical surface) must be relayed.
	if err := broker.Publish(ctx, topic, &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      "agent:coder",
		RecipientID: "hub-user-1",
		Msg:         "conversation reply on gchat",
		Type:        messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "gchat",
			ExternalRef: "spaces/AAQAx/threads/t2",
			ParentRef:   "spaces/AAQAx",
		},
	}); err != nil {
		t.Fatalf("Publish(Surface=gchat): %v", err)
	}

	if len(fm.messages) != 2 {
		t.Fatalf("expected both Channel=googlechat and Surface=gchat replies to be relayed, got %d", len(fm.messages))
	}
	if fm.messages[0].ThreadID != "spaces/AAQAx/threads/t1" {
		t.Errorf("msg 0 ThreadID = %q, want spaces/AAQAx/threads/t1", fm.messages[0].ThreadID)
	}
	if fm.messages[1].ThreadID != "spaces/AAQAx/threads/t2" {
		t.Errorf("msg 1 ThreadID = %q, want spaces/AAQAx/threads/t2", fm.messages[1].ThreadID)
	}
}

type hubConvStore struct {
	hubstore.Store
	convs map[string]*hubstore.Conversation
}

func (s *hubConvStore) GetConversation(_ context.Context, id string) (*hubstore.Conversation, error) {
	c, ok := s.convs[id]
	if !ok {
		return nil, hubstore.ErrNotFound
	}
	return c, nil
}

type chatAppForwardingBus struct {
	broker *BrokerServer
}

func (f *chatAppForwardingBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	wire := grpcbroker.ProtoToStructuredMessage(grpcbroker.StructuredMessageToProto(msg))
	return f.broker.Publish(ctx, topic, wire)
}

func (f *chatAppForwardingBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, nil
}

func (f *chatAppForwardingBus) Close() error { return nil }

// TestChatApp_Publish_HubEnrichedOptionAShapes drives chat-app from REAL
// hub-enriched shapes produced by hub.MessageBrokerProxy (backed by real
// store.Conversation rows and round-tripped through grpcbroker proto
// conversion):
//  1. Existing Google Chat thread stored as Surface:"native", Kind:"group"
//     (documented P5b exception: chat-app sends inbound via user-token
//     /agents/{id}/message, so ExternalRef is empty on the wire) — verifies
//     replies still post IN that Google Chat thread via msg.ThreadID.
//  2. External Surface:"gchat", Kind:"group" thread with empty ThreadID —
//     posts in the translated ExternalRef thread.
//  3. Direct user DM (Surface:"native", Kind:"direct") — posts unthreaded
//     (ThreadID == ""), confirming design §10 Q2 even if ThreadID is set.
//  4. Unmapped user DM — skipped (not relayed to space links).
//  5. Agent-to-agent observer DM (Surface:"native", Kind:"direct") — relayed
//     to ShowAgentToAgent space links.
//  6. Pre-P5a hub (Conversation == nil) DM with dm: ThreadID — posts unthreaded.
func TestChatApp_Publish_HubEnrichedOptionAShapes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.SetSpaceLink(&state.SpaceLink{
		SpaceID:          "spaces/AAQAx",
		Platform:         "googlechat",
		ProjectID:        "project-abc",
		ProjectSlug:      "my-project",
		LinkedBy:         "test",
		ShowAgentToAgent: true,
	}); err != nil {
		t.Fatalf("SetSpaceLink: %v", err)
	}
	if err := st.SetUserMapping(&state.UserMapping{
		PlatformUserID: "users/12345",
		Platform:       "googlechat",
		HubUserID:      "hub-user-1",
		HubUserEmail:   "alice@example.com",
		RegisteredBy:   "auto",
	}); err != nil {
		t.Fatalf("SetUserMapping: %v", err)
	}

	fm := &fakeMessenger{}
	relay := NewNotificationRelay(st, fm, slog.Default())
	broker := NewBrokerServer(relay.HandleBrokerMessage, slog.Default())
	if err := broker.Configure(map[string]string{"plugin_name": "googlechat"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	projectID := "project-abc"
	gchatThreadKey, gchatThreadKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		ThreadID:  "spaces/AAQAx/threads/t-orig",
		ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("DeriveConversationKey(gchat): %v", err)
	}
	nativeThreadKey, nativeThreadKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		ThreadID:  "spaces/AAQAx/threads/t-existing",
		ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("DeriveConversationKey(native thread): %v", err)
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
		convs: map[string]*hubstore.Conversation{
			"conv-native-gchat-thread": {
				ID:          "conv-native-gchat-thread",
				ProjectID:   &projectID,
				Kind:        nativeThreadKind,
				Surface:     "native",
				ExternalRef: nativeThreadKey,
			},
			"conv-gchat-thread": {
				ID:          "conv-gchat-thread",
				ProjectID:   &projectID,
				Kind:        gchatThreadKind,
				Surface:     "gchat",
				ExternalRef: gchatThreadKey,
				ParentRef:   "spaces/AAQAx",
				DisplayName: "Eng Space",
			},
			"conv-dm-1": {
				ID:          "conv-dm-1",
				ProjectID:   &projectID,
				Kind:        dmKind,
				Surface:     "native",
				ExternalRef: dmKey,
				DisplayName: "Direct Message",
			},
			"conv-dm-a2a": {
				ID:          "conv-dm-a2a",
				ProjectID:   &projectID,
				Kind:        a2aKind,
				Surface:     "native",
				ExternalRef: a2aKey,
			},
		},
	}

	proxy := hub.NewMessageBrokerProxy(&chatAppForwardingBus{broker: broker}, convStore, nil, nil, slog.Default())

	// 1. Reply in an existing Google Chat thread stored as Surface:"native",
	// Kind:"group" (chat-app user-token ingress). enrichConversation emits
	// empty ExternalRef; chat-app must still post in the existing thread.
	if err := proxy.PublishUserMessage(ctx, projectID, "hub-user-1", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		RecipientID:    "hub-user-1",
		Msg:            "reply in existing gchat thread",
		Type:           messages.TypeInstruction,
		Channel:        "googlechat",
		ThreadID:       "spaces/AAQAx/threads/t-existing",
		ConversationID: "conv-native-gchat-thread",
	}); err != nil {
		t.Fatalf("Publish(existing native-surface gchat thread): %v", err)
	}

	// 2. External gchat thread enriched by MessageBrokerProxy (empty ThreadID).
	if err := proxy.PublishUserMessage(ctx, projectID, "hub-user-1", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		RecipientID:    "hub-user-1",
		Msg:            "reply on external gchat thread",
		Type:           messages.TypeInstruction,
		Channel:        "googlechat",
		ConversationID: "conv-gchat-thread",
	}); err != nil {
		t.Fatalf("Publish(external gchat thread): %v", err)
	}

	// 3. Direct user DM enriched by MessageBrokerProxy: Kind:"direct" posts
	// unthreaded (ThreadID == "") even when a legacy/bogus ThreadID is present.
	if err := proxy.PublishUserMessage(ctx, projectID, "hub-user-1", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		RecipientID:    "hub-user-1",
		Msg:            "dm reply via web",
		Type:           messages.TypeInstruction,
		Channel:        "web",
		ThreadID:       dmKey,
		ConversationID: "conv-dm-1",
	}); err != nil {
		t.Fatalf("Publish(web DM): %v", err)
	}

	// 4. Unmapped user DM is skipped and never posted to linked spaces.
	if err := proxy.PublishUserMessage(ctx, projectID, "hub-user-unmapped", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:bob@example.com",
		RecipientID:    "hub-user-unmapped",
		Msg:            "dm for unmapped user",
		Type:           messages.TypeInstruction,
		Channel:        "googlechat",
		ConversationID: "conv-dm-1",
	}); err != nil {
		t.Fatalf("Publish(unmapped user DM): %v", err)
	}

	// 5. Agent-to-agent observer DM still delivers to ShowAgentToAgent space links.
	if err := proxy.PublishMessage(ctx, projectID, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "agent:reviewer",
		Msg:            "a2a observer dm",
		Type:           messages.TypeInstruction,
		ObserverOnly:   true,
		ConversationID: "conv-dm-a2a",
	}); err != nil {
		t.Fatalf("Publish(a2a observer DM): %v", err)
	}

	// 6. Pre-P5a hub (Conversation == nil) DM with dm: ThreadID posts unthreaded.
	if err := broker.Publish(ctx, "scion.project.project-abc.user.hub-user-1.messages", &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      "agent:coder",
		Recipient:   "user:alice@example.com",
		RecipientID: "hub-user-1",
		Msg:         "pre-p5a dm reply",
		Type:        messages.TypeInstruction,
		Channel:     "web",
		ThreadID:    dmKey,
	}); err != nil {
		t.Fatalf("Publish(pre-p5a DM): %v", err)
	}

	if len(fm.messages) != 5 {
		t.Fatalf("expected 5 relayed messages, got %d", len(fm.messages))
	}
	if fm.messages[0].SpaceID != "spaces/AAQAx" || fm.messages[0].ThreadID != "spaces/AAQAx/threads/t-existing" {
		t.Errorf("msg 0 = (%q, %q), want (spaces/AAQAx, spaces/AAQAx/threads/t-existing)", fm.messages[0].SpaceID, fm.messages[0].ThreadID)
	}
	if fm.messages[1].SpaceID != "spaces/AAQAx" || fm.messages[1].ThreadID != "spaces/AAQAx/threads/t-orig" {
		t.Errorf("msg 1 = (%q, %q), want (spaces/AAQAx, spaces/AAQAx/threads/t-orig)", fm.messages[1].SpaceID, fm.messages[1].ThreadID)
	}
	if fm.messages[2].SpaceID != "spaces/AAQAx" || fm.messages[2].ThreadID != "" {
		t.Errorf("msg 2 (DM) = (%q, %q), want unthreaded (spaces/AAQAx, \"\")", fm.messages[2].SpaceID, fm.messages[2].ThreadID)
	}
	if fm.messages[3].SpaceID != "spaces/AAQAx" || fm.messages[3].Card == nil {
		t.Errorf("msg 3 (a2a observer) expected card in spaces/AAQAx, got %+v", fm.messages[3])
	}
	if fm.messages[4].SpaceID != "spaces/AAQAx" || fm.messages[4].ThreadID != "" {
		t.Errorf("msg 4 (pre-P5a DM) = (%q, %q), want unthreaded (spaces/AAQAx, \"\")", fm.messages[4].SpaceID, fm.messages[4].ThreadID)
	}
}
