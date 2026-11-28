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
	"context"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestBrokerServer_Publish_ConversationAndBridgeSemantics(t *testing.T) {
	var received []*messages.StructuredMessage
	b := NewBrokerServer(func(_ context.Context, _ string, msg *messages.StructuredMessage) error {
		received = append(received, msg)
		return nil
	}, slog.Default(), context.Background())

	topic := "scion.project.p1.user.u1.messages"
	ctx := context.Background()

	// 1. Outbound with msg.Conversation only (empty legacy Channel/ThreadID) is forwarded intact.
	convMsg := &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "phase 11 reply",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-1",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "direct:p1:u1:coder",
		},
	}
	if err := b.Publish(ctx, topic, convMsg); err != nil {
		t.Fatalf("Publish (native conversation) failed: %v", err)
	}

	// 2. Outbound targeting another surface is also forwarded (A2A bridge does not spoke-filter;
	// correlation in HandleBrokerMessage decides ownership).
	otherSurfaceMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "external surface reply",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "C0123ABC:1700000000.000100",
		},
	}
	if err := b.Publish(ctx, topic, otherSurfaceMsg); err != nil {
		t.Fatalf("Publish (other surface) failed: %v", err)
	}

	// 3. Legacy fallback (older hub sends Channel/ThreadID without Conversation).
	legacyMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "legacy reply",
		Type:      messages.TypeInstruction,
		Channel:   "native",
		ThreadID:  "thread-legacy-1",
	}
	if err := b.Publish(ctx, topic, legacyMsg); err != nil {
		t.Fatalf("Publish (legacy) failed: %v", err)
	}

	// 4. Nil message is safely ignored.
	if err := b.Publish(ctx, topic, nil); err != nil {
		t.Fatalf("Publish (nil) failed: %v", err)
	}

	if len(received) != 3 {
		t.Fatalf("expected 3 forwarded messages, got %d", len(received))
	}

	// Native conversation does not populate external Surface/ExternalRef,
	// while ConversationID is preserved and the bridge still receives the message.
	t0 := plugin.ResolveConversationTarget(received[0])
	if t0.ConversationID != "conv-1" || t0.Surface != "" || t0.ExternalRef != "" {
		t.Errorf("unexpected resolved target[0]: %+v", t0)
	}
	t1 := plugin.ResolveConversationTarget(received[1])
	if t1.Surface != "slack" || t1.ExternalRef != "C0123ABC:1700000000.000100" {
		t.Errorf("unexpected resolved target[1]: %+v", t1)
	}
	t2 := plugin.ResolveConversationTarget(received[2])
	if t2.Surface != "native" || t2.ExternalRef != "thread-legacy-1" {
		t.Errorf("unexpected resolved target[2]: %+v", t2)
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

type bridgeForwardingBus struct {
	broker *BrokerServer
}

func (f *bridgeForwardingBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	wire := grpcbroker.ProtoToStructuredMessage(grpcbroker.StructuredMessageToProto(msg))
	return f.broker.Publish(ctx, topic, wire)
}

func (f *bridgeForwardingBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, nil
}

func (f *bridgeForwardingBus) Close() error { return nil }

// TestBrokerServer_Publish_HubEnrichedOptionAShapes drives BrokerServer.Publish
// from REAL hub-enriched shapes produced by hub.MessageBrokerProxy and
// round-tripped through grpcbroker proto conversion:
//  1. External thread (Kind:"group", translated ExternalRef, empty ThreadID)
//  2. Direct user DM (Kind:"direct", Surface:"native", empty ExternalRef, bogus
//     ThreadID ignored by ResolveConversationTarget)
//  3. Agent-to-agent observer DM (Kind:"direct", ObserverOnly:true, Channel:"")
func TestBrokerServer_Publish_HubEnrichedOptionAShapes(t *testing.T) {
	var received []*messages.StructuredMessage
	b := NewBrokerServer(func(_ context.Context, _ string, msg *messages.StructuredMessage) error {
		received = append(received, msg)
		return nil
	}, slog.Default(), context.Background())

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

	proxy := hub.NewMessageBrokerProxy(&bridgeForwardingBus{broker: b}, convStore, nil, nil, slog.Default())

	// 1. External thread reply.
	if err := proxy.PublishUserMessage(ctx, projectID, "u1", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "thread reply",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ConversationID: "conv-thread",
	}); err != nil {
		t.Fatalf("PublishUserMessage(thread): %v", err)
	}

	// 2. Direct user DM with bogus ThreadID (ignored when Conversation != nil).
	if err := proxy.PublishUserMessage(ctx, projectID, "u1", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "dm reply",
		Type:           messages.TypeInstruction,
		Channel:        "web",
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
		Msg:            "a2a observer",
		Type:           messages.TypeInstruction,
		ObserverOnly:   true,
		ConversationID: "conv-a2a",
	}); err != nil {
		t.Fatalf("PublishMessage(a2a): %v", err)
	}

	if len(received) != 3 {
		t.Fatalf("expected 3 forwarded messages, got %d", len(received))
	}

	t0 := plugin.ResolveConversationTarget(received[0])
	if t0.Kind != "group" || t0.Surface != "slack" || t0.ExternalRef != "C0123ABC:1700000000.000100" || t0.ParentRef != "C0123ABC" {
		t.Errorf("unexpected target[0]: %+v", t0)
	}
	t1 := plugin.ResolveConversationTarget(received[1])
	if t1.Kind != "direct" || t1.Surface != "web" || t1.ExternalRef != "" {
		t.Errorf("unexpected target[1] (bogus ThreadID must be ignored): %+v", t1)
	}
	t2 := plugin.ResolveConversationTarget(received[2])
	if t2.Kind != "direct" || t2.Surface != "" || t2.ExternalRef != "" {
		t.Errorf("unexpected target[2]: %+v", t2)
	}
}
