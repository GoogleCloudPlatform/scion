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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ptone/scion#3881: the envelope carries "message_id" with the persisted
// message ID, and omits it when there is none.

func rawEnvelope3881(t *testing.T, rendered string) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(extractJSON(t, rendered)), &raw); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, rendered)
	}
	return raw
}

func TestFormatNewDelivery_MessageID(t *testing.T) {
	intent := IntentRequest
	created := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	msg := &Message{
		ID: "msg-3881", From: PrincipalRef("user:alice"), Kind: KindText,
		Intent: &intent, Body: "hello", CreatedAt: created,
	}

	raw := rawEnvelope3881(t, FormatNewDelivery(msg, nil, nil, DeliveryOptions{}, false, false))
	if got := raw["message_id"]; got != "msg-3881" {
		t.Errorf("message_id = %v, want %q", got, "msg-3881")
	}
	// The existing fields keep their names and meaning.
	if got := raw["timestamp"]; got != "2026-10-08T12:00:00Z" {
		t.Errorf("timestamp = %v, want the message created time", got)
	}
	if _, ok := raw["created_at"]; ok {
		t.Error("envelope must not add created_at; timestamp already carries it")
	}

	env := extractEnvelope(t, FormatNewDelivery(msg, nil, nil, DeliveryOptions{}, false, false))
	if env.MessageID != "msg-3881" {
		t.Errorf("DeliveryEnvelope.MessageID = %q, want %q", env.MessageID, "msg-3881")
	}
}

func TestFormatNewDelivery_NoMessageID_OmitsKey(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		From: PrincipalRef("user:alice"), Kind: KindText, Intent: &intent,
		Body: "hello", CreatedAt: time.Now().UTC(),
	}
	raw := rawEnvelope3881(t, FormatNewDelivery(msg, nil, nil, DeliveryOptions{}, false, false))
	if _, ok := raw["message_id"]; ok {
		t.Errorf("message_id present (%v); want absent when there is no persisted message", raw["message_id"])
	}
}

func TestFormatNewDelivery_Plain_NoMessageID(t *testing.T) {
	msg := &Message{ID: "msg-3881", From: PrincipalRef("user:alice"), Kind: KindText, Body: "raw text"}
	if got := FormatNewDelivery(msg, nil, nil, DeliveryOptions{Plain: true}, false, false); got != "raw text" {
		t.Errorf("plain delivery = %q, want the body only", got)
	}
}

func TestRenderDeliveryText_MessageID(t *testing.T) {
	sm := messages.NewInstruction("user:alice", "agent:builder", "hello")

	t.Run("persisted", func(t *testing.T) {
		raw := rawEnvelope3881(t, RenderDeliveryText(RenderDeliveryInput{MessageID: "row-3881", Msg: sm}))
		if got := raw["message_id"]; got != "row-3881" {
			t.Errorf("message_id = %v, want %q", got, "row-3881")
		}
	})

	t.Run("reply keeps reply_to distinct", func(t *testing.T) {
		raw := rawEnvelope3881(t, RenderDeliveryText(RenderDeliveryInput{MessageID: "row-3881", ReplyToID: "parent-1", Msg: sm}))
		if raw["message_id"] != "row-3881" || raw["reply_to"] != "parent-1" {
			t.Errorf("message_id = %v, reply_to = %v; want row-3881 and parent-1", raw["message_id"], raw["reply_to"])
		}
	})

	t.Run("not persisted", func(t *testing.T) {
		raw := rawEnvelope3881(t, RenderDeliveryText(RenderDeliveryInput{Msg: sm}))
		if _, ok := raw["message_id"]; ok {
			t.Errorf("message_id present (%v); want absent with no MessageID", raw["message_id"])
		}
	})
}

func TestFormatLegacyAsNewDelivery_OmitsMessageID(t *testing.T) {
	sm := messages.NewInstruction("user:alice", "agent:builder", "hello")
	out := FormatLegacyAsNewDelivery(sm, nil)
	if strings.Contains(out, `"message_id"`) {
		t.Errorf("compat path has no persisted identity and must omit message_id:\n%s", out)
	}
}
