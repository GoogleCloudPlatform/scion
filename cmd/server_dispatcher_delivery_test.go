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

package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ---------- resolveDeliveryText preference order (Phase 9b(i)) ----------

func TestResolveDeliveryText_PrefersDeliveryText(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:      messages.Version,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Sender:       "user:alice",
		Recipient:    "agent:bot",
		Msg:          "body text",
		Type:         messages.TypeInstruction,
		DeliveryText: "pre-rendered envelope",
	}
	result := resolveDeliveryText(msg)
	if result != "pre-rendered envelope" {
		t.Errorf("resolveDeliveryText = %q, want %q", result, "pre-rendered envelope")
	}
}

func TestResolveDeliveryText_FallsBackToFormatForDelivery(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "body text",
		Type:      messages.TypeInstruction,
		// DeliveryText empty — should fall back to FormatForDelivery.
	}
	result := resolveDeliveryText(msg)
	// FormatForDelivery produces delimited output with the message body.
	if !strings.Contains(result, "body text") {
		t.Errorf("resolveDeliveryText should contain body text via FormatForDelivery, got %q", result)
	}
	// Should contain the SCION MESSAGE delimiters from FormatForDelivery.
	if !strings.Contains(result, "SCION MESSAGE") {
		t.Errorf("resolveDeliveryText should contain SCION MESSAGE delimiter, got %q", result)
	}
}

func TestResolveDeliveryText_DeliveryTextOverridesFormatForDelivery(t *testing.T) {
	// Verify the pre-rendered text takes absolute precedence — even when
	// the StructuredMessage has all fields set for FormatForDelivery.
	msg := &messages.StructuredMessage{
		Version:      messages.Version,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Sender:       "user:alice",
		Recipient:    "agent:bot",
		Msg:          "body text that FormatForDelivery would include",
		Type:         messages.TypeInstruction,
		DeliveryText: "exact pre-rendered text",
	}
	result := resolveDeliveryText(msg)
	if result != "exact pre-rendered text" {
		t.Errorf("resolveDeliveryText = %q, want exact pre-rendered text", result)
	}
}
