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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
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
