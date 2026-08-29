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

package messaging

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ---------- MapLegacyType ----------

func TestMapLegacyType_AllOldTypes(t *testing.T) {
	tests := []struct {
		name           string
		oldType        string
		systemCategory string
		hasAddressee   bool
		wantKind       MessageKind
		wantIntent     *TextIntent
		wantEventType  *EventType // nil if no event expected
	}{
		{
			name:       "instruction → text/request",
			oldType:    messages.TypeInstruction,
			wantKind:   KindText,
			wantIntent: ptrIntent(IntentRequest),
		},
		{
			name:       "chat → text/inform",
			oldType:    messages.TypeChat,
			wantKind:   KindText,
			wantIntent: ptrIntent(IntentInform),
		},
		{
			name:       "assistant-reply → text/inform",
			oldType:    messages.TypeAssistantReply,
			wantKind:   KindText,
			wantIntent: ptrIntent(IntentInform),
		},
		{
			name:          "input-needed (addressed) → text/question",
			oldType:       messages.TypeInputNeeded,
			hasAddressee:  true,
			wantKind:      KindText,
			wantIntent:    ptrIntent(IntentQuestion),
			wantEventType: nil,
		},
		{
			name:          "input-needed (broadcast) → event/agent.input-needed",
			oldType:       messages.TypeInputNeeded,
			hasAddressee:  false,
			wantKind:      KindEvent,
			wantIntent:    nil,
			wantEventType: ptrEventType(EventAgentInputNeeded),
		},
		{
			name:          "state-change → event/agent.state-changed",
			oldType:       messages.TypeStateChange,
			wantKind:      KindEvent,
			wantEventType: ptrEventType(EventAgentStateChanged),
		},
		{
			name:           "system (scheduler) → event/schedule.fired",
			oldType:        messages.TypeSystem,
			systemCategory: messages.SystemCategoryScheduler,
			wantKind:       KindEvent,
			wantEventType:  ptrEventType(EventScheduleFired),
		},
		{
			name:           "system (port-forward) → event/port.exposed",
			oldType:        messages.TypeSystem,
			systemCategory: messages.SystemCategoryPortForward,
			wantKind:       KindEvent,
			wantEventType:  ptrEventType(EventPortExposed),
		},
		{
			name:           "system (delivery-failed) → event/delivery.failed",
			oldType:        messages.TypeSystem,
			systemCategory: messages.SystemCategoryDeliveryFailed,
			wantKind:       KindEvent,
			wantEventType:  ptrEventType(EventDeliveryFailed),
		},
		{
			name:           "system (unknown category) → event/agent.state-changed",
			oldType:        messages.TypeSystem,
			systemCategory: "unknown-category",
			wantKind:       KindEvent,
			wantEventType:  ptrEventType(EventAgentStateChanged),
		},
		{
			name:       "mention → text/request (delivery artifact)",
			oldType:    messages.TypeMention,
			wantKind:   KindText,
			wantIntent: ptrIntent(IntentRequest),
		},
		{
			name:       "group-set → text/request (delivery artifact)",
			oldType:    messages.TypeGroupSet,
			wantKind:   KindText,
			wantIntent: ptrIntent(IntentRequest),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, intent, event := MapLegacyType(tc.oldType, tc.systemCategory, tc.hasAddressee)

			if kind != tc.wantKind {
				t.Errorf("kind: got %q, want %q", kind, tc.wantKind)
			}

			if tc.wantIntent == nil {
				if intent != nil {
					t.Errorf("intent: got %q, want nil", *intent)
				}
			} else {
				if intent == nil {
					t.Fatalf("intent: got nil, want %q", *tc.wantIntent)
				}
				if *intent != *tc.wantIntent {
					t.Errorf("intent: got %q, want %q", *intent, *tc.wantIntent)
				}
			}

			if tc.wantEventType == nil {
				if event != nil {
					t.Errorf("event: got %+v, want nil", event)
				}
			} else {
				if event == nil {
					t.Fatalf("event: got nil, want type %q", *tc.wantEventType)
				}
				if event.Type != *tc.wantEventType {
					t.Errorf("event.Type: got %q, want %q", event.Type, *tc.wantEventType)
				}
			}
		})
	}
}

func TestMapLegacyType_UnknownType(t *testing.T) {
	kind, intent, event := MapLegacyType("totally-unknown", "", false)
	if kind != KindText {
		t.Errorf("unknown type kind: got %q, want text", kind)
	}
	if intent == nil || *intent != IntentInform {
		t.Errorf("unknown type intent: got %v, want inform", intent)
	}
	if event != nil {
		t.Errorf("unknown type event: got %v, want nil", event)
	}
}

// ---------- MapLegacyDeliveryArtifact ----------

func TestMapLegacyDeliveryArtifact(t *testing.T) {
	tests := []struct {
		oldType string
		wantVia *AddressedVia
	}{
		{messages.TypeMention, ptrVia(ViaBodyMention)},
		{messages.TypeGroupSet, ptrVia(ViaExplicit)},
		{messages.TypeInstruction, nil},
		{messages.TypeChat, nil},
		{messages.TypeStateChange, nil},
		{messages.TypeSystem, nil},
		{messages.TypeInputNeeded, nil},
		{messages.TypeAssistantReply, nil},
	}

	for _, tc := range tests {
		t.Run(tc.oldType, func(t *testing.T) {
			got := MapLegacyDeliveryArtifact(tc.oldType)
			if tc.wantVia == nil {
				if got != nil {
					t.Errorf("got %q, want nil", *got)
				}
			} else {
				if got == nil {
					t.Fatalf("got nil, want %q", *tc.wantVia)
				}
				if *got != *tc.wantVia {
					t.Errorf("got %q, want %q", *got, *tc.wantVia)
				}
			}
		})
	}
}

// ---------- MapLegacyEnvelope ----------

func TestMapLegacyEnvelope_NilInput(t *testing.T) {
	_, _, err := MapLegacyEnvelope(nil)
	if err == nil {
		t.Fatal("expected error for nil input")
	}
}

func TestMapLegacyEnvelope_Instruction(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   1,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		SenderID:  "user:alice",
		Recipient: "agent:builder",
		Msg:       "Build the feature",
		Type:      messages.TypeInstruction,
	}

	msg, addrs, err := MapLegacyEnvelope(old)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Kind != KindText {
		t.Errorf("kind: got %q, want text", msg.Kind)
	}
	if msg.Intent == nil || *msg.Intent != IntentRequest {
		t.Errorf("intent: got %v, want request", msg.Intent)
	}
	if msg.From != "user:alice" {
		t.Errorf("from: got %q, want user:alice", msg.From)
	}
	if msg.Body != "Build the feature" {
		t.Errorf("body: got %q, want 'Build the feature'", msg.Body)
	}
	if len(addrs) != 1 {
		t.Fatalf("addressees: got %d, want 1", len(addrs))
	}
	if addrs[0].PrincipalKind != "agent" || addrs[0].PrincipalID != "builder" {
		t.Errorf("addressee: got %s:%s, want agent:builder", addrs[0].PrincipalKind, addrs[0].PrincipalID)
	}
}

func TestMapLegacyEnvelope_StateChange(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   1,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "agent:builder",
		SenderID:  "agent:builder",
		Recipient: "user:alice",
		Msg:       "Agent state changed to COMPLETED",
		Type:      messages.TypeStateChange,
		Status:    "COMPLETED",
	}

	msg, _, err := MapLegacyEnvelope(old)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Kind != KindEvent {
		t.Errorf("kind: got %q, want event", msg.Kind)
	}
	if msg.Event == nil {
		t.Fatal("event body should not be nil")
	}
	if msg.Event.Type != EventAgentStateChanged {
		t.Errorf("event.type: got %q, want agent.state-changed", msg.Event.Type)
	}
	if msg.Event.Status != "COMPLETED" {
		t.Errorf("event.status: got %q, want COMPLETED", msg.Event.Status)
	}
}

func TestMapLegacyEnvelope_SystemScheduler(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   1,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "system:scheduler",
		Recipient: "agent:worker",
		Msg:       "Schedule fired",
		Type:      messages.TypeSystem,
		Metadata:  map[string]string{"system_category": messages.SystemCategoryScheduler},
	}

	msg, _, err := MapLegacyEnvelope(old)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Kind != KindEvent {
		t.Errorf("kind: got %q, want event", msg.Kind)
	}
	if msg.Event == nil || msg.Event.Type != EventScheduleFired {
		t.Errorf("event.type: got %v, want schedule.fired", msg.Event)
	}
}

func TestMapLegacyEnvelope_InputNeeded_Addressed(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:     1,
		Timestamp:   "2026-08-27T10:00:00Z",
		Sender:      "agent:worker",
		SenderID:    "agent:worker",
		Recipient:   "user:alice",
		RecipientID: "user:alice",
		Msg:         "I need input",
		Type:        messages.TypeInputNeeded,
		Broadcasted: false,
	}

	msg, _, err := MapLegacyEnvelope(old)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Kind != KindText {
		t.Errorf("kind: got %q, want text", msg.Kind)
	}
	if msg.Intent == nil || *msg.Intent != IntentQuestion {
		t.Errorf("intent: got %v, want question", msg.Intent)
	}
}

func TestMapLegacyEnvelope_InputNeeded_Broadcast(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:     1,
		Timestamp:   "2026-08-27T10:00:00Z",
		Sender:      "agent:worker",
		SenderID:    "agent:worker",
		Recipient:   "user:alice",
		RecipientID: "user:alice",
		Msg:         "Agent needs input",
		Type:        messages.TypeInputNeeded,
		Broadcasted: true,
	}

	msg, _, err := MapLegacyEnvelope(old)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Kind != KindEvent {
		t.Errorf("kind: got %q, want event", msg.Kind)
	}
	if msg.Event == nil || msg.Event.Type != EventAgentInputNeeded {
		t.Errorf("event.type: got %v, want agent.input-needed", msg.Event)
	}
}

func TestMapLegacyEnvelope_Mention(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   1,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		Recipient: "agent:reviewer",
		Msg:       "Hey @reviewer look at this",
		Type:      messages.TypeMention,
		Metadata:  map[string]string{"mention_source": "agent:builder", "mention_position": "body"},
	}

	msg, addrs, err := MapLegacyEnvelope(old)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Kind != KindText {
		t.Errorf("kind: got %q, want text", msg.Kind)
	}
	if msg.Intent == nil || *msg.Intent != IntentRequest {
		t.Errorf("intent: got %v, want request", msg.Intent)
	}
	if len(addrs) != 1 {
		t.Fatalf("addressees: got %d, want 1", len(addrs))
	}
	if addrs[0].Via != ViaBodyMention {
		t.Errorf("addressee.via: got %q, want body-mention", addrs[0].Via)
	}
}

func TestMapLegacyEnvelope_GroupSet(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   1,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		Recipient: "agent:builder",
		Msg:       "Build this for the group",
		Type:      messages.TypeGroupSet,
	}

	msg, addrs, err := MapLegacyEnvelope(old)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Kind != KindText {
		t.Errorf("kind: got %q, want text", msg.Kind)
	}
	if msg.Intent == nil || *msg.Intent != IntentRequest {
		t.Errorf("intent: got %v, want request", msg.Intent)
	}
	if len(addrs) != 1 {
		t.Fatalf("addressees: got %d, want 1", len(addrs))
	}
	if addrs[0].Via != ViaExplicit {
		t.Errorf("addressee.via: got %q, want explicit", addrs[0].Via)
	}
}

func TestMapLegacyEnvelope_Attachments(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:     1,
		Timestamp:   "2026-08-27T10:00:00Z",
		Sender:      "user:alice",
		Recipient:   "agent:builder",
		Msg:         "Here are the files",
		Type:        messages.TypeInstruction,
		Attachments: []string{"/tmp/file1.go", "/tmp/file2.go"},
	}

	msg, _, err := MapLegacyEnvelope(old)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(msg.Attachments) != 2 {
		t.Fatalf("attachments: got %d, want 2", len(msg.Attachments))
	}
	if msg.Attachments[0].Path != "/tmp/file1.go" {
		t.Errorf("attachment[0].Path: got %q, want /tmp/file1.go", msg.Attachments[0].Path)
	}
}

func TestMapLegacyEnvelope_Visibility(t *testing.T) {
	tests := []struct {
		oldVis string
		want   Visibility
	}{
		{"", VisibilityNormal},
		{messages.VisibilityNormal, VisibilityNormal},
		{messages.VisibilityVerbose, VisibilityVerbose},
		{messages.VisibilityFull, VisibilityFull},
	}
	for _, tc := range tests {
		old := &messages.StructuredMessage{
			Version:    1,
			Timestamp:  "2026-08-27T10:00:00Z",
			Sender:     "user:alice",
			Recipient:  "agent:builder",
			Msg:        "Hello",
			Type:       messages.TypeInstruction,
			Visibility: tc.oldVis,
		}
		msg, _, err := MapLegacyEnvelope(old)
		if err != nil {
			t.Fatalf("unexpected error for vis=%q: %v", tc.oldVis, err)
		}
		if msg.Visibility != tc.want {
			t.Errorf("visibility %q: got %q, want %q", tc.oldVis, msg.Visibility, tc.want)
		}
	}
}

// ---------- NewEnvelopeToLegacy ----------

func TestNewEnvelopeToLegacy_NilInput(t *testing.T) {
	result := NewEnvelopeToLegacy(nil, nil)
	if result != nil {
		t.Fatal("expected nil for nil input")
	}
}

func TestNewEnvelopeToLegacy_TextRequest(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-1",
		From:      "user:alice",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Build the feature",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	addrs := []Addressee{{
		MessageID:     "msg-1",
		PrincipalKind: "agent",
		PrincipalID:   "builder",
		Via:           ViaExplicit,
		DeliveryState: DeliveryPending,
	}}

	old := NewEnvelopeToLegacy(msg, addrs)
	if old.Type != messages.TypeInstruction {
		t.Errorf("type: got %q, want instruction", old.Type)
	}
	if old.Sender != "user:alice" {
		t.Errorf("sender: got %q, want user:alice", old.Sender)
	}
	if old.Recipient != "agent:builder" {
		t.Errorf("recipient: got %q, want agent:builder", old.Recipient)
	}
	if old.Msg != "Build the feature" {
		t.Errorf("msg: got %q, want 'Build the feature'", old.Msg)
	}
}

func TestNewEnvelopeToLegacy_TextInform_Agent(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:        "msg-1",
		From:      "agent:builder",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Task completed",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	addrs := []Addressee{{
		MessageID:     "msg-1",
		PrincipalKind: "user",
		PrincipalID:   "alice",
		Via:           ViaDirect,
		DeliveryState: DeliveryDelivered,
	}}

	old := NewEnvelopeToLegacy(msg, addrs)
	if old.Type != messages.TypeAssistantReply {
		t.Errorf("type: got %q, want assistant-reply", old.Type)
	}
}

func TestNewEnvelopeToLegacy_TextInform_User(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:        "msg-1",
		From:      "user:bob",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hey",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}

	old := NewEnvelopeToLegacy(msg, nil)
	if old.Type != messages.TypeChat {
		t.Errorf("type: got %q, want chat", old.Type)
	}
}

func TestNewEnvelopeToLegacy_TextQuestion(t *testing.T) {
	intent := IntentQuestion
	msg := &Message{
		ID:        "msg-1",
		From:      "agent:worker",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "What should I do?",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}

	old := NewEnvelopeToLegacy(msg, nil)
	if old.Type != messages.TypeInputNeeded {
		t.Errorf("type: got %q, want input-needed", old.Type)
	}
}

func TestNewEnvelopeToLegacy_EventStateChanged(t *testing.T) {
	msg := &Message{
		ID:   "msg-1",
		From: "agent:builder",
		Kind: KindEvent,
		Event: &EventBody{
			Type:   EventAgentStateChanged,
			Status: "COMPLETED",
		},
		Body:      "Agent completed",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}

	old := NewEnvelopeToLegacy(msg, nil)
	if old.Type != messages.TypeStateChange {
		t.Errorf("type: got %q, want state-change", old.Type)
	}
	if old.Status != "COMPLETED" {
		t.Errorf("status: got %q, want COMPLETED", old.Status)
	}
}

func TestNewEnvelopeToLegacy_EventSystem(t *testing.T) {
	tests := []struct {
		name     string
		eventType EventType
		wantCat  string
	}{
		{"schedule.fired", EventScheduleFired, messages.SystemCategoryScheduler},
		{"port.exposed", EventPortExposed, messages.SystemCategoryPortForward},
		{"delivery.failed", EventDeliveryFailed, messages.SystemCategoryDeliveryFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := &Message{
				ID:        "msg-1",
				From:      "system:scheduler",
				Kind:      KindEvent,
				Event:     &EventBody{Type: tc.eventType},
				Body:      "System event",
				CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
			}

			old := NewEnvelopeToLegacy(msg, nil)
			if old.Type != messages.TypeSystem {
				t.Errorf("type: got %q, want system", old.Type)
			}
			if old.Metadata["system_category"] != tc.wantCat {
				t.Errorf("system_category: got %q, want %q", old.Metadata["system_category"], tc.wantCat)
			}
		})
	}
}

func TestNewEnvelopeToLegacy_EventInputNeeded(t *testing.T) {
	msg := &Message{
		ID:        "msg-1",
		From:      "agent:worker",
		Kind:      KindEvent,
		Event:     &EventBody{Type: EventAgentInputNeeded},
		Body:      "Agent needs input",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}

	old := NewEnvelopeToLegacy(msg, nil)
	if old.Type != messages.TypeInputNeeded {
		t.Errorf("type: got %q, want input-needed", old.Type)
	}
}

func TestNewEnvelopeToLegacy_MentionVia(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-1",
		From:      "user:alice",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hey @reviewer",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	addrs := []Addressee{{
		MessageID:     "msg-1",
		PrincipalKind: "agent",
		PrincipalID:   "reviewer",
		Via:           ViaBodyMention,
		DeliveryState: DeliveryPending,
	}}

	old := NewEnvelopeToLegacy(msg, addrs)
	if old.Type != messages.TypeMention {
		t.Errorf("type: got %q, want mention", old.Type)
	}
	if old.Metadata["mention_source"] != "user:alice" {
		t.Errorf("mention_source: got %q, want user:alice", old.Metadata["mention_source"])
	}
}

func TestNewEnvelopeToLegacy_Attachments(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:     "msg-1",
		From:   "user:alice",
		Kind:   KindText,
		Intent: &intent,
		Body:   "Files",
		Attachments: []AttachmentRef{
			{Path: "/tmp/a.go"},
			{Path: "/tmp/b.go", Name: "b.go"},
		},
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}

	old := NewEnvelopeToLegacy(msg, nil)
	if len(old.Attachments) != 2 {
		t.Fatalf("attachments: got %d, want 2", len(old.Attachments))
	}
	if old.Attachments[0] != "/tmp/a.go" {
		t.Errorf("attachment[0]: got %q, want /tmp/a.go", old.Attachments[0])
	}
}

func TestNewEnvelopeToLegacy_Visibility(t *testing.T) {
	tests := []struct {
		vis    Visibility
		wantOld string
	}{
		{VisibilityNormal, ""},
		{VisibilityVerbose, "verbose"},
		{VisibilityFull, "full"},
	}
	for _, tc := range tests {
		intent := IntentInform
		msg := &Message{
			ID:         "msg-1",
			From:       "user:alice",
			Kind:       KindText,
			Intent:     &intent,
			Body:       "Hello",
			Visibility: tc.vis,
			CreatedAt:  time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
		}
		old := NewEnvelopeToLegacy(msg, nil)
		if old.Visibility != tc.wantOld {
			t.Errorf("vis=%q: old.Visibility got %q, want %q", tc.vis, old.Visibility, tc.wantOld)
		}
	}
}

// ---------- Round-trip tests ----------

func TestRoundTrip_OldToNewToOld(t *testing.T) {
	tests := []struct {
		name string
		old  *messages.StructuredMessage
		// expectedType is the type we expect after round-tripping. Some mappings
		// are lossy; when the round-trip produces a different but semantically
		// equivalent type, we accept it here.
		expectedType string
	}{
		{
			name: "instruction",
			old: &messages.StructuredMessage{
				Version:   1,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "user:alice",
				SenderID:  "user:alice",
				Recipient: "agent:builder",
				Msg:       "Build it",
				Type:      messages.TypeInstruction,
			},
			expectedType: messages.TypeInstruction,
		},
		{
			name: "chat",
			old: &messages.StructuredMessage{
				Version:   1,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "user:alice",
				SenderID:  "user:alice",
				Recipient: "user:bob",
				Msg:       "Hey",
				Type:      messages.TypeChat,
			},
			expectedType: messages.TypeChat,
		},
		{
			name: "assistant-reply",
			old: &messages.StructuredMessage{
				Version:   1,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "agent:builder",
				SenderID:  "agent:builder",
				Recipient: "user:alice",
				Msg:       "Done",
				Type:      messages.TypeAssistantReply,
			},
			expectedType: messages.TypeAssistantReply,
		},
		{
			name: "state-change",
			old: &messages.StructuredMessage{
				Version:   1,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "agent:builder",
				SenderID:  "agent:builder",
				Recipient: "user:alice",
				Msg:       "State changed",
				Type:      messages.TypeStateChange,
				Status:    "COMPLETED",
			},
			expectedType: messages.TypeStateChange,
		},
		{
			name: "system/scheduler",
			old: &messages.StructuredMessage{
				Version:   1,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "system:scheduler",
				Recipient: "agent:worker",
				Msg:       "Fired",
				Type:      messages.TypeSystem,
				Metadata:  map[string]string{"system_category": messages.SystemCategoryScheduler},
			},
			expectedType: messages.TypeSystem,
		},
		{
			name: "input-needed (addressed)",
			old: &messages.StructuredMessage{
				Version:     1,
				Timestamp:   "2026-08-27T10:00:00Z",
				Sender:      "agent:worker",
				SenderID:    "agent:worker",
				Recipient:   "user:alice",
				RecipientID: "user:alice",
				Msg:         "Need input",
				Type:        messages.TypeInputNeeded,
				Broadcasted: false,
			},
			expectedType: messages.TypeInputNeeded,
		},
		{
			name: "input-needed (broadcast)",
			old: &messages.StructuredMessage{
				Version:     1,
				Timestamp:   "2026-08-27T10:00:00Z",
				Sender:      "agent:worker",
				SenderID:    "agent:worker",
				Recipient:   "user:alice",
				Msg:         "Need input",
				Type:        messages.TypeInputNeeded,
				Broadcasted: true,
			},
			expectedType: messages.TypeInputNeeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, addrs, err := MapLegacyEnvelope(tc.old)
			if err != nil {
				t.Fatalf("MapLegacyEnvelope failed: %v", err)
			}

			roundTripped := NewEnvelopeToLegacy(msg, addrs)

			// Check that essential semantics are preserved.
			if roundTripped.Type != tc.expectedType {
				t.Errorf("type: got %q, want %q", roundTripped.Type, tc.expectedType)
			}
			if roundTripped.Msg != tc.old.Msg {
				t.Errorf("msg: got %q, want %q", roundTripped.Msg, tc.old.Msg)
			}
			if roundTripped.Sender != tc.old.SenderID && roundTripped.Sender != tc.old.Sender {
				t.Errorf("sender: got %q, want %q or %q", roundTripped.Sender, tc.old.Sender, tc.old.SenderID)
			}
		})
	}
}

func TestRoundTrip_NewToOldToNew(t *testing.T) {
	intent := IntentRequest
	original := &Message{
		ID:   "msg-1",
		From: "user:alice",
		Kind: KindText,
		Intent: &intent,
		Body: "Build it",
		Attachments: []AttachmentRef{{Path: "/tmp/a.go"}},
		Visibility: VisibilityVerbose,
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	originalAddrs := []Addressee{{
		MessageID:     "msg-1",
		PrincipalKind: "agent",
		PrincipalID:   "builder",
		Via:           ViaExplicit,
		DeliveryState: DeliveryPending,
	}}

	// Convert new → old.
	legacy := NewEnvelopeToLegacy(original, originalAddrs)

	// Convert old → new.
	restored, restoredAddrs, err := MapLegacyEnvelope(legacy)
	if err != nil {
		t.Fatalf("MapLegacyEnvelope failed: %v", err)
	}

	// Check preserved semantics.
	if restored.Kind != original.Kind {
		t.Errorf("kind: got %q, want %q", restored.Kind, original.Kind)
	}
	if restored.Intent == nil || *restored.Intent != *original.Intent {
		t.Errorf("intent: got %v, want %v", restored.Intent, original.Intent)
	}
	if restored.Body != original.Body {
		t.Errorf("body: got %q, want %q", restored.Body, original.Body)
	}
	if len(restored.Attachments) != len(original.Attachments) {
		t.Errorf("attachments count: got %d, want %d", len(restored.Attachments), len(original.Attachments))
	}
	if restored.Visibility != original.Visibility {
		t.Errorf("visibility: got %q, want %q", restored.Visibility, original.Visibility)
	}
	if len(restoredAddrs) != len(originalAddrs) {
		t.Errorf("addressees count: got %d, want %d", len(restoredAddrs), len(originalAddrs))
	}
}

// ---------- buildPrincipalRef ----------

func TestBuildPrincipalRef_RawUUIDWithPrefixedName(t *testing.T) {
	// When SenderID is a raw UUID (no colon), the kind should be derived
	// from the Sender name field.
	ref := buildPrincipalRef("user:alice", "be67fbc9-c869-5d43-b15d-c28ca3e8d355")
	if ref != "user:be67fbc9-c869-5d43-b15d-c28ca3e8d355" {
		t.Fatalf("expected user-prefixed ref, got %q", ref)
	}
	// Must pass PrincipalRef validation.
	if err := ValidatePrincipalRef(ref); err != nil {
		t.Fatalf("expected valid PrincipalRef, got error: %v", err)
	}
}

func TestBuildPrincipalRef_PrefixedID(t *testing.T) {
	// When SenderID already has a colon, use it directly.
	ref := buildPrincipalRef("user:alice", "user:alice-uuid")
	if ref != "user:alice-uuid" {
		t.Fatalf("expected prefixed id used directly, got %q", ref)
	}
}

func TestBuildPrincipalRef_AgentKindDerived(t *testing.T) {
	ref := buildPrincipalRef("agent:builder", "814b7c0b-1a15-43a2-a3f1-2aa3b1548c94")
	if ref != "agent:814b7c0b-1a15-43a2-a3f1-2aa3b1548c94" {
		t.Fatalf("expected agent-prefixed ref, got %q", ref)
	}
	if err := ValidatePrincipalRef(ref); err != nil {
		t.Fatalf("expected valid PrincipalRef, got error: %v", err)
	}
}

// ---------- helpers ----------

func ptrIntent(i TextIntent) *TextIntent     { return &i }
func ptrEventType(e EventType) *EventType     { return &e }
func ptrVia(v AddressedVia) *AddressedVia     { return &v }
