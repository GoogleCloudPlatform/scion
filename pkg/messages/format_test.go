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

package messages

import (
	"strings"
	"testing"
)

func TestFormatForDelivery_Plain(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "just raw text",
		Type:      TypeInstruction,
		Plain:     true,
	}

	result := FormatForDelivery(msg)
	if result != "just raw text" {
		t.Errorf("plain mode should return raw msg, got %q", result)
	}
}

func TestFormatForDelivery_Structured(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "implement auth",
		Type:      TypeInstruction,
		Urgent:    true,
	}

	result := FormatForDelivery(msg)

	// Should have the intro
	if !strings.Contains(result, deliveryIntro) {
		t.Error("missing delivery intro")
	}

	// Should have delimiters
	if !strings.Contains(result, beginDelimiter) {
		t.Error("missing begin delimiter")
	}
	if !strings.Contains(result, endDelimiter) {
		t.Error("missing end delimiter")
	}

	// Should contain key fields
	if !strings.Contains(result, `"sender": "user:alice"`) {
		t.Error("missing sender in output")
	}
	if !strings.Contains(result, `"msg": "implement auth"`) {
		t.Error("missing msg in output")
	}
	if !strings.Contains(result, `"urgent": true`) {
		t.Error("missing urgent in output")
	}

	// Should NOT contain recipient or version (stripped)
	if strings.Contains(result, `"recipient"`) {
		t.Error("recipient should be stripped from delivery")
	}
	if strings.Contains(result, `"version"`) {
		t.Error("version should be stripped from delivery")
	}
}

func TestFormatForDelivery_StripsRecipient(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "agent:lead",
		Recipient: "agent:worker",
		Msg:       "check the schema",
		Type:      TypeInstruction,
	}

	result := FormatForDelivery(msg)
	if strings.Contains(result, "agent:worker") {
		t.Error("recipient identity should not appear in delivery output")
	}
}

func TestFormatForDelivery_EmptyMsg(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "",
		Type:      TypeInstruction,
		Plain:     true,
	}

	result := FormatForDelivery(msg)
	if result != "" {
		t.Errorf("empty plain message should return empty string, got %q", result)
	}
}

func TestFormatForDelivery_Raw(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "Escape",
		Type:      TypeInstruction,
		Raw:       true,
	}

	result := FormatForDelivery(msg)
	if result != "Escape" {
		t.Errorf("raw mode should return raw msg, got %q", result)
	}
}

func TestFormatForDelivery_WithAttachments(t *testing.T) {
	msg := &StructuredMessage{
		Version:     Version,
		Timestamp:   "2026-03-07T14:30:00Z",
		Sender:      "user:alice",
		Recipient:   "agent:dev",
		Msg:         "review these",
		Type:        TypeInstruction,
		Attachments: []string{"src/auth.go", "src/middleware.go"},
	}

	result := FormatForDelivery(msg)
	if !strings.Contains(result, "src/auth.go") {
		t.Error("missing attachment in output")
	}
}
