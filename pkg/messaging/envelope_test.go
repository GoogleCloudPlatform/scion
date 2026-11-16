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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ---------- MessageKind ----------

func TestValidateMessageKind(t *testing.T) {
	tests := []struct {
		kind    MessageKind
		wantErr bool
	}{
		{KindText, false},
		{KindEvent, false},
		{"unknown", true},
		{"", true},
	}
	for _, tc := range tests {
		err := ValidateMessageKind(tc.kind)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateMessageKind(%q): got err=%v, wantErr=%v", tc.kind, err, tc.wantErr)
		}
	}
}

// ---------- TextIntent ----------

func TestValidateTextIntent(t *testing.T) {
	tests := []struct {
		intent  TextIntent
		wantErr bool
	}{
		{IntentInform, false},
		{IntentRequest, false},
		{IntentQuestion, false},
		{"unknown", true},
		{"", true},
	}
	for _, tc := range tests {
		err := ValidateTextIntent(tc.intent)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateTextIntent(%q): got err=%v, wantErr=%v", tc.intent, err, tc.wantErr)
		}
	}
}

// ---------- EventType ----------

func TestValidateEventType(t *testing.T) {
	tests := []struct {
		et      EventType
		wantErr bool
	}{
		{EventAgentStateChanged, false},
		{EventAgentInputNeeded, false},
		{EventDeliveryFailed, false},
		{EventScheduleFired, false},
		{EventPortExposed, false},
		{EventArtifactReview, false},
		{"unknown.event", true},
		{"", true},
	}
	for _, tc := range tests {
		err := ValidateEventType(tc.et)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateEventType(%q): got err=%v, wantErr=%v", tc.et, err, tc.wantErr)
		}
	}
}

// ---------- PrincipalRef ----------

func TestValidatePrincipalRef(t *testing.T) {
	tests := []struct {
		ref     PrincipalRef
		wantErr bool
	}{
		{"user:alice", false},
		{"agent:code-reviewer", false},
		{"system:scheduler", false},
		{"user:", true},   // empty id
		{"bad:ref", true}, // unknown prefix
		{"nocolon", true}, // no colon
		{"", true},        // empty
		{":orphan", true}, // empty prefix
	}
	for _, tc := range tests {
		err := ValidatePrincipalRef(tc.ref)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidatePrincipalRef(%q): got err=%v, wantErr=%v", tc.ref, err, tc.wantErr)
		}
	}
}

func TestValidatePrincipalRef_TooLong(t *testing.T) {
	// Create a PrincipalRef that exceeds the maximum length.
	long := PrincipalRef("user:" + strings.Repeat("x", MaxPrincipalRefLength))
	err := ValidatePrincipalRef(long)
	if err == nil {
		t.Fatal("expected error for PrincipalRef exceeding maximum length")
	}
	if !strings.Contains(err.Error(), "maximum length") {
		t.Fatalf("error should mention maximum length, got: %v", err)
	}
}

func TestValidatePrincipalRef_AtMaxLength(t *testing.T) {
	// Create a PrincipalRef exactly at the maximum length — should pass.
	id := strings.Repeat("x", MaxPrincipalRefLength-len("user:"))
	ref := PrincipalRef("user:" + id)
	if len(string(ref)) != MaxPrincipalRefLength {
		t.Fatalf("test setup error: expected len %d, got %d", MaxPrincipalRefLength, len(string(ref)))
	}
	if err := ValidatePrincipalRef(ref); err != nil {
		t.Fatalf("PrincipalRef at exact max length should pass: %v", err)
	}
}

func TestPrincipalRefParts(t *testing.T) {
	tests := []struct {
		ref      PrincipalRef
		wantKind string
		wantID   string
	}{
		{"user:alice", "user", "alice"},
		{"agent:builder", "agent", "builder"},
		{"system:scheduler", "system", "scheduler"},
		{"nocolon", "nocolon", ""},
	}
	for _, tc := range tests {
		if got := tc.ref.PrincipalKind(); got != tc.wantKind {
			t.Errorf("PrincipalRef(%q).PrincipalKind() = %q, want %q", tc.ref, got, tc.wantKind)
		}
		if got := tc.ref.PrincipalID(); got != tc.wantID {
			t.Errorf("PrincipalRef(%q).PrincipalID() = %q, want %q", tc.ref, got, tc.wantID)
		}
	}
}

// ---------- AddressedVia ----------

func TestValidateAddressedVia(t *testing.T) {
	tests := []struct {
		via     AddressedVia
		wantErr bool
	}{
		{ViaExplicit, false},
		{ViaBodyMention, false},
		{ViaDefaultAgent, false},
		{ViaDirect, false},
		{"unknown", true},
		{"", true},
	}
	for _, tc := range tests {
		err := ValidateAddressedVia(tc.via)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateAddressedVia(%q): got err=%v, wantErr=%v", tc.via, err, tc.wantErr)
		}
	}
}

// ---------- DeliveryState ----------

func TestValidateDeliveryState(t *testing.T) {
	tests := []struct {
		state   DeliveryState
		wantErr bool
	}{
		{DeliveryPending, false},
		{DeliveryDelivered, false},
		{DeliveryFailed, false},
		{"unknown", true},
		{"", true},
	}
	for _, tc := range tests {
		err := ValidateDeliveryState(tc.state)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateDeliveryState(%q): got err=%v, wantErr=%v", tc.state, err, tc.wantErr)
		}
	}
}

// ---------- Message.Validate ----------

func TestMessageValidate_TextMessage(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-1",
		From:      "user:alice",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Now(),
	}
	if err := msg.Validate(); err != nil {
		t.Fatalf("valid text message should pass validation: %v", err)
	}
}

func TestMessageValidate_EventMessage(t *testing.T) {
	msg := &Message{
		ID:   "msg-2",
		From: "system:scheduler",
		Kind: KindEvent,
		Event: &EventBody{
			Type:   EventScheduleFired,
			Status: "fired",
		},
		Body:      "Schedule triggered",
		CreatedAt: time.Now(),
	}
	if err := msg.Validate(); err != nil {
		t.Fatalf("valid event message should pass validation: %v", err)
	}
}

func TestMessageValidate_MissingID(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		From:      "user:alice",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Now(),
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("message without ID should fail validation")
	}
}

func TestMessageValidate_InvalidFrom(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:        "msg-1",
		From:      "nocolon",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Now(),
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("message with invalid from should fail validation")
	}
}

func TestMessageValidate_TextWithoutIntent(t *testing.T) {
	msg := &Message{
		ID:        "msg-1",
		From:      "user:alice",
		Kind:      KindText,
		Body:      "Hello",
		CreatedAt: time.Now(),
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("text message without intent should fail validation")
	}
}

func TestMessageValidate_TextWithEventBody(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:     "msg-1",
		From:   "user:alice",
		Kind:   KindText,
		Intent: &intent,
		Event:  &EventBody{Type: EventScheduleFired},
		Body:   "Hello",
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("text message with event body should fail validation")
	}
}

func TestMessageValidate_EventWithoutBody(t *testing.T) {
	msg := &Message{
		ID:   "msg-1",
		From: "system:scheduler",
		Kind: KindEvent,
		Body: "Fired",
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("event message without event body should fail validation")
	}
}

func TestMessageValidate_EventWithIntent(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:     "msg-1",
		From:   "system:scheduler",
		Kind:   KindEvent,
		Intent: &intent,
		Event:  &EventBody{Type: EventScheduleFired},
		Body:   "Fired",
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("event message with intent should fail validation")
	}
}

func TestMessageValidate_InvalidKind(t *testing.T) {
	msg := &Message{
		ID:   "msg-1",
		From: "user:alice",
		Kind: "invalid",
		Body: "Hello",
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("message with invalid kind should fail validation")
	}
}

// ---------- Message metadata validation ----------

func TestMessageValidateStructural_MetadataTooManyEntries(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:        "msg-1",
		From:      "user:alice",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Now(),
		Metadata:  make(map[string]string),
	}
	for i := 0; i < messages.MaxMetadataEntries+1; i++ {
		msg.Metadata[fmt.Sprintf("key-%d", i)] = "value"
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("expected error for too many metadata entries")
	} else if !strings.Contains(err.Error(), "metadata exceeds maximum entries") {
		t.Fatalf("error should mention metadata entries, got: %v", err)
	}
}

func TestMessageValidateStructural_MetadataKeyTooLong(t *testing.T) {
	intent := IntentInform
	longKey := strings.Repeat("k", messages.MaxMetadataKeySize+1)
	msg := &Message{
		ID:        "msg-1",
		From:      "user:alice",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Now(),
		Metadata:  map[string]string{longKey: "value"},
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("expected error for metadata key exceeding max size")
	} else if !strings.Contains(err.Error(), "metadata key exceeds maximum size") {
		t.Fatalf("error should mention metadata key, got: %v", err)
	}
}

func TestMessageValidateStructural_MetadataValueTooLong(t *testing.T) {
	intent := IntentInform
	longValue := strings.Repeat("v", messages.MaxMetadataValueSize+1)
	msg := &Message{
		ID:        "msg-1",
		From:      "user:alice",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Now(),
		Metadata:  map[string]string{"key": longValue},
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("expected error for metadata value exceeding max size")
	} else if !strings.Contains(err.Error(), "metadata value exceeds maximum size") {
		t.Fatalf("error should mention metadata value, got: %v", err)
	}
}

func TestMessageValidateStructural_MetadataValidSizes(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:        "msg-1",
		From:      "user:alice",
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Now(),
		Metadata:  map[string]string{"short-key": "short-value"},
	}
	if err := msg.Validate(); err != nil {
		t.Fatalf("valid metadata should pass validation: %v", err)
	}
}

// ---------- Addressee.Validate ----------

func TestAddresseeValidate(t *testing.T) {
	tests := []struct {
		name    string
		addr    Addressee
		wantErr bool
	}{
		{
			name: "valid user addressee",
			addr: Addressee{
				MessageID:     "msg-1",
				PrincipalKind: "user",
				PrincipalID:   "alice",
				Via:           ViaExplicit,
				DeliveryState: DeliveryPending,
			},
			wantErr: false,
		},
		{
			name: "valid agent addressee",
			addr: Addressee{
				MessageID:     "msg-1",
				PrincipalKind: "agent",
				PrincipalID:   "builder",
				Via:           ViaBodyMention,
				DeliveryState: DeliveryDelivered,
			},
			wantErr: false,
		},
		{
			name: "missing message_id",
			addr: Addressee{
				PrincipalKind: "user",
				PrincipalID:   "alice",
				Via:           ViaExplicit,
				DeliveryState: DeliveryPending,
			},
			wantErr: true,
		},
		{
			name: "invalid principal_kind",
			addr: Addressee{
				MessageID:     "msg-1",
				PrincipalKind: "system",
				PrincipalID:   "sched",
				Via:           ViaExplicit,
				DeliveryState: DeliveryPending,
			},
			wantErr: true,
		},
		{
			name: "missing principal_id",
			addr: Addressee{
				MessageID:     "msg-1",
				PrincipalKind: "user",
				Via:           ViaExplicit,
				DeliveryState: DeliveryPending,
			},
			wantErr: true,
		},
		{
			name: "invalid via",
			addr: Addressee{
				MessageID:     "msg-1",
				PrincipalKind: "user",
				PrincipalID:   "alice",
				Via:           "carrier-pigeon",
				DeliveryState: DeliveryPending,
			},
			wantErr: true,
		},
		{
			name: "invalid delivery_state",
			addr: Addressee{
				MessageID:     "msg-1",
				PrincipalKind: "user",
				PrincipalID:   "alice",
				Via:           ViaExplicit,
				DeliveryState: "lost",
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.addr.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Addressee.Validate(): got err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// ---------- EventBody.Validate ----------

func TestEventBodyValidate(t *testing.T) {
	tests := []struct {
		name    string
		body    EventBody
		wantErr bool
	}{
		{"valid agent.state-changed", EventBody{Type: EventAgentStateChanged}, false},
		{"valid agent.input-needed", EventBody{Type: EventAgentInputNeeded}, false},
		{"valid delivery.failed", EventBody{Type: EventDeliveryFailed, Reason: "timeout"}, false},
		{"valid schedule.fired", EventBody{Type: EventScheduleFired}, false},
		{"valid port.exposed", EventBody{Type: EventPortExposed, URL: "http://localhost:8080"}, false},
		{"valid webhook.received", EventBody{Type: EventWebhookReceived, EventID: "evt-1"}, false},
		{"valid trigger.fired", EventBody{Type: EventTriggerFired, EventType: "webhook.deploy.finished"}, false},
		{"invalid type", EventBody{Type: "unknown"}, true},
		{"empty type", EventBody{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.body.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("EventBody.Validate(): got err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestNewExternalPayload_InlineAndTruncated(t *testing.T) {
	t.Run("inline JSON within 16 KiB", func(t *testing.T) {
		raw := []byte(`{"status":"ok","count":3}`)
		ext := NewExternalPayload("scion-v1", raw)
		if ext.Verified != "scion-v1" {
			t.Fatalf("verified = %q, want scion-v1", ext.Verified)
		}
		if ext.Truncated {
			t.Fatalf("truncated = true, want false")
		}
		if ext.Bytes != len(raw) {
			t.Fatalf("bytes = %d, want %d", ext.Bytes, len(raw))
		}
		if string(ext.JSON) != string(raw) {
			t.Fatalf("json = %s, want %s", string(ext.JSON), string(raw))
		}
		if ext.Text != "" {
			t.Fatalf("text = %q, want empty", ext.Text)
		}
	})

	t.Run("over 16 KiB truncates on UTF-8 rune boundary", func(t *testing.T) {
		// Place a 4-byte emoji ("🎉") across the 16384-byte boundary:
		// 16382 ASCII bytes before the emoji -> 2 of 4 emoji bytes fall inside [:16384].
		prefixLen := MaxExternalPayloadInlineBytes - 12
		buf := make([]byte, 0, MaxExternalPayloadInlineBytes+64)
		buf = append(buf, `{"data":"`...) // 9 bytes
		for len(buf) < MaxExternalPayloadInlineBytes-2 {
			buf = append(buf, 'a')
		}
		_ = prefixLen
		// Now len(buf) == 16382; append 4-byte UTF-8 emoji + closing JSON.
		buf = append(buf, []byte("🎉more\"}")...)

		ext := NewExternalPayload("scion-v1", buf)
		if !ext.Truncated {
			t.Fatalf("truncated = false, want true")
		}
		if ext.Bytes != len(buf) {
			t.Fatalf("bytes = %d, want %d", ext.Bytes, len(buf))
		}
		if len(ext.JSON) != 0 {
			t.Fatalf("json must be omitted when truncated, got %d bytes", len(ext.JSON))
		}
		if len(ext.Text) != MaxExternalPayloadInlineBytes-2 {
			t.Fatalf("text byte length = %d, want %d (cut before split 4-byte rune)", len(ext.Text), MaxExternalPayloadInlineBytes-2)
		}
	})
}
