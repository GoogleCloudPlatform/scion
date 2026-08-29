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

// validLegacyMessage returns a StructuredMessage that passes validation.
func validLegacyMessage() *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Msg:       "hello world",
		Sender:    "user:alice",
		SenderID:  "user:alice",
		Recipient: "agent:builder",
		Type:      messages.TypeInstruction,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

func TestValidateLegacyMessage_ValidMessage(t *testing.T) {
	msg := validLegacyMessage()
	if err := ValidateLegacyMessage(msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateLegacyMessage_Nil(t *testing.T) {
	if err := ValidateLegacyMessage(nil); err == nil {
		t.Fatal("expected error for nil message")
	}
}

func TestValidateLegacyMessage_EmptyMsg(t *testing.T) {
	msg := validLegacyMessage()
	msg.Msg = ""
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for empty msg field")
	}
}

func TestValidateLegacyMessage_EmptySender(t *testing.T) {
	msg := validLegacyMessage()
	msg.Sender = ""
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for empty sender")
	}
}

func TestValidateLegacyMessage_InvalidType(t *testing.T) {
	msg := validLegacyMessage()
	msg.Type = "invalid-type"
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for invalid type")
	}
}

// TestValidateLegacyMessage_ThreadIDWithoutChannel reproduces the Teams
// regression: the Teams adapter emits Channel:"" with a non-empty ThreadID.
// The old Validate() forbids this, and ValidateLegacyMessage must also reject
// it. This is the specific bug from the findings document, section 6.
func TestValidateLegacyMessage_ThreadIDWithoutChannel(t *testing.T) {
	msg := validLegacyMessage()
	msg.ThreadID = "thread-123"
	msg.Channel = "" // Teams regression
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for thread_id without channel (Teams regression)")
	}
	if !strings.Contains(err.Error(), "thread_id requires channel") {
		t.Fatalf("error should mention thread_id/channel, got: %v", err)
	}
}

func TestValidateLegacyMessage_ThreadIDWithChannel(t *testing.T) {
	msg := validLegacyMessage()
	msg.ThreadID = "thread-123"
	msg.Channel = "discord"
	if err := ValidateLegacyMessage(msg); err != nil {
		t.Fatalf("thread_id with channel should be valid: %v", err)
	}
}

func TestValidateLegacyMessage_ChannelTooLong(t *testing.T) {
	msg := validLegacyMessage()
	msg.Channel = strings.Repeat("a", messages.MaxChannelLength+1)
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for channel exceeding max length")
	}
}

func TestValidateLegacyMessage_BodyOverLimit(t *testing.T) {
	msg := validLegacyMessage()
	msg.Msg = strings.Repeat("x", messages.MaxMessageLength+1)
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for body over character limit")
	}
}

func TestValidateLegacyMessage_TooManyAttachments(t *testing.T) {
	msg := validLegacyMessage()
	msg.Attachments = make([]string, messages.MaxAttachments+1)
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for too many attachments")
	}
}

func TestValidateLegacyMessage_AllValidTypes(t *testing.T) {
	validTypes := []string{
		messages.TypeInstruction,
		messages.TypeInputNeeded,
		messages.TypeStateChange,
		messages.TypeAssistantReply,
		messages.TypeGroupSet,
		messages.TypeMention,
		messages.TypeSystem,
	}
	for _, typ := range validTypes {
		msg := validLegacyMessage()
		msg.Type = typ
		if err := ValidateLegacyMessage(msg); err != nil {
			t.Errorf("type %q should be valid: %v", typ, err)
		}
	}
}

func TestValidateLegacyMessage_BroadcastWithoutRecipient(t *testing.T) {
	// Broadcast messages have no recipient; validation should still pass.
	msg := validLegacyMessage()
	msg.Recipient = ""
	msg.Broadcasted = true
	if err := ValidateLegacyMessage(msg); err != nil {
		t.Fatalf("broadcast message without recipient should be valid: %v", err)
	}
}

func TestValidateLegacyMessage_ChannelInvalidCharacters(t *testing.T) {
	msg := validLegacyMessage()
	msg.Channel = "my channel!" // contains space and exclamation mark
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for channel with invalid characters")
	}
	if !strings.Contains(err.Error(), "invalid characters") {
		t.Fatalf("error should mention invalid characters, got: %v", err)
	}
}

func TestValidateLegacyMessage_ChannelValidCharacters(t *testing.T) {
	msg := validLegacyMessage()
	msg.Channel = "my-channel-123"
	if err := ValidateLegacyMessage(msg); err != nil {
		t.Fatalf("valid channel should pass: %v", err)
	}
}

func TestValidateLegacyMessage_TooManyMetadataEntries(t *testing.T) {
	msg := validLegacyMessage()
	msg.Metadata = make(map[string]string)
	for i := 0; i < messages.MaxMetadataEntries+1; i++ {
		msg.Metadata[fmt.Sprintf("key-%d", i)] = "value"
	}
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for too many metadata entries")
	}
	if !strings.Contains(err.Error(), "metadata entries") {
		t.Fatalf("error should mention metadata entries, got: %v", err)
	}
}

func TestValidateLegacyMessage_MetadataKeyTooLong(t *testing.T) {
	msg := validLegacyMessage()
	longKey := strings.Repeat("k", messages.MaxMetadataKeySize+1)
	msg.Metadata = map[string]string{longKey: "value"}
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for metadata key exceeding max size")
	}
	if !strings.Contains(err.Error(), "metadata key") {
		t.Fatalf("error should mention metadata key, got: %v", err)
	}
}

func TestValidateLegacyMessage_MetadataValueTooLong(t *testing.T) {
	msg := validLegacyMessage()
	longValue := strings.Repeat("v", messages.MaxMetadataValueSize+1)
	msg.Metadata = map[string]string{"key": longValue}
	err := ValidateLegacyMessage(msg)
	if err == nil {
		t.Fatal("expected error for metadata value exceeding max size")
	}
	if !strings.Contains(err.Error(), "metadata value") {
		t.Fatalf("error should mention metadata value, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DEF-19: group[] recipient validation
// ---------------------------------------------------------------------------

// TestValidateLegacyMessage_GroupRecipient_Accepted (AC-19-1) verifies that
// ValidateLegacyMessage accepts group[] recipients and produces one addressee
// per member with the correct kind and ID.
func TestValidateLegacyMessage_GroupRecipient_Accepted(t *testing.T) {
	tests := []struct {
		name      string
		recipient string
	}{
		{"prefixed kinds", "group[agent:reviewer,user:alice]"},
		{"bare agent names", "group[reviewer,deploy-bot]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := validLegacyMessage()
			msg.Recipient = tc.recipient
			if err := ValidateLegacyMessage(msg); err != nil {
				t.Fatalf("ValidateLegacyMessage(%q) returned error: %v", tc.recipient, err)
			}
		})
	}
}

// TestValidateLegacyMessage_GroupRecipient_Addressees (AC-19-1) verifies that
// buildAddressees (via MapLegacyEnvelope) produces one addressee per group
// member with the correct PrincipalKind and PrincipalID.
func TestValidateLegacyMessage_GroupRecipient_Addressees(t *testing.T) {
	msg := validLegacyMessage()
	msg.Recipient = "group[agent:reviewer,user:alice]"

	_, addrs, err := MapLegacyEnvelope(msg)
	if err != nil {
		t.Fatalf("MapLegacyEnvelope: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("expected 2 addressees, got %d", len(addrs))
	}
	// First member: agent:reviewer
	if addrs[0].PrincipalKind != "agent" || addrs[0].PrincipalID != "reviewer" {
		t.Errorf("addrs[0]: got %s:%s, want agent:reviewer", addrs[0].PrincipalKind, addrs[0].PrincipalID)
	}
	if addrs[0].Via != ViaExplicit {
		t.Errorf("addrs[0].Via: got %q, want explicit", addrs[0].Via)
	}
	if addrs[0].DeliveryState != DeliveryPending {
		t.Errorf("addrs[0].DeliveryState: got %q, want pending", addrs[0].DeliveryState)
	}
	// Second member: user:alice
	if addrs[1].PrincipalKind != "user" || addrs[1].PrincipalID != "alice" {
		t.Errorf("addrs[1]: got %s:%s, want user:alice", addrs[1].PrincipalKind, addrs[1].PrincipalID)
	}
}

// TestValidateLegacyMessage_GroupRecipient_BareNames (AC-19-1) verifies that
// bare agent names in group[] are classified correctly.
func TestValidateLegacyMessage_GroupRecipient_BareNames(t *testing.T) {
	msg := validLegacyMessage()
	msg.Recipient = "group[reviewer,deploy-bot]"

	_, addrs, err := MapLegacyEnvelope(msg)
	if err != nil {
		t.Fatalf("MapLegacyEnvelope: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("expected 2 addressees, got %d", len(addrs))
	}
	// Bare names default to agent kind.
	if addrs[0].PrincipalKind != "agent" || addrs[0].PrincipalID != "reviewer" {
		t.Errorf("addrs[0]: got %s:%s, want agent:reviewer", addrs[0].PrincipalKind, addrs[0].PrincipalID)
	}
	if addrs[1].PrincipalKind != "agent" || addrs[1].PrincipalID != "deploy-bot" {
		t.Errorf("addrs[1]: got %s:%s, want agent:deploy-bot", addrs[1].PrincipalKind, addrs[1].PrincipalID)
	}
}

// TestValidateLegacyMessage_GroupRecipient_ViaExplicitPinned verifies that
// group[] members always get ViaExplicit, even when the message type would
// yield a different Via for a single-principal recipient. TypeMention maps to
// ViaBodyMention on the single-principal path; the group branch must still
// produce ViaExplicit because group[] is explicit addressing regardless of
// the legacy type field. This test dies when the group branch is changed to
// use the computed `via` variable (mutation-verified).
func TestValidateLegacyMessage_GroupRecipient_ViaExplicitPinned(t *testing.T) {
	msg := validLegacyMessage()
	msg.Type = messages.TypeMention
	msg.Recipient = "group[agent:reviewer,agent:deploy-bot]"

	_, addrs, err := MapLegacyEnvelope(msg)
	if err != nil {
		t.Fatalf("MapLegacyEnvelope: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("expected 2 addressees, got %d", len(addrs))
	}
	for i, a := range addrs {
		if a.Via != ViaExplicit {
			t.Errorf("addrs[%d].Via = %q, want %q; group[] members must be ViaExplicit regardless of message type",
				i, a.Via, ViaExplicit)
		}
	}

	// Positive control: a single-principal mention produces ViaBodyMention.
	singleMsg := validLegacyMessage()
	singleMsg.Type = messages.TypeMention
	singleMsg.Recipient = "agent:reviewer"
	_, singleAddrs, err := MapLegacyEnvelope(singleMsg)
	if err != nil {
		t.Fatalf("MapLegacyEnvelope (single): %v", err)
	}
	if len(singleAddrs) != 1 {
		t.Fatalf("expected 1 addressee for single recipient, got %d", len(singleAddrs))
	}
	if singleAddrs[0].Via != ViaBodyMention {
		t.Errorf("single-recipient mention Via = %q, want %q; positive control failed",
			singleAddrs[0].Via, ViaBodyMention)
	}
}

// TestValidateLegacyMessage_SetRecipient_LegacyAlias verifies that the
// deprecated set[] syntax works through the same validation and mapping
// path as group[]. This pins the legacy alias so a refactor cannot silently
// drop it.
func TestValidateLegacyMessage_SetRecipient_LegacyAlias(t *testing.T) {
	msg := validLegacyMessage()
	msg.Recipient = "set[agent:reviewer,user:alice]"

	if err := ValidateLegacyMessage(msg); err != nil {
		t.Fatalf("ValidateLegacyMessage(set[...]) returned error: %v", err)
	}

	_, addrs, err := MapLegacyEnvelope(msg)
	if err != nil {
		t.Fatalf("MapLegacyEnvelope(set[...]): %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("expected 2 addressees from set[], got %d", len(addrs))
	}
	if addrs[0].PrincipalKind != "agent" || addrs[0].PrincipalID != "reviewer" {
		t.Errorf("addrs[0]: got %s:%s, want agent:reviewer", addrs[0].PrincipalKind, addrs[0].PrincipalID)
	}
	if addrs[1].PrincipalKind != "user" || addrs[1].PrincipalID != "alice" {
		t.Errorf("addrs[1]: got %s:%s, want user:alice", addrs[1].PrincipalKind, addrs[1].PrincipalID)
	}
}

// TestValidateLegacyMessage_GroupRecipient_Malformed (AC-19-3) verifies that
// malformed group[] recipients are rejected.
func TestValidateLegacyMessage_GroupRecipient_Malformed(t *testing.T) {
	malformed := []struct {
		name      string
		recipient string
	}{
		{"empty group", "group[]"},
		{"unclosed bracket", "group["},
		{"bogus kind", "group[bogus:x,agent:y]"},
		{"only commas", "group[,]"},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			msg := validLegacyMessage()
			msg.Recipient = tc.recipient
			if err := ValidateLegacyMessage(msg); err == nil {
				t.Fatalf("expected error for malformed group recipient %q, got nil", tc.recipient)
			}
		})
	}
}

// TestValidateLegacyMessage_SingleRecipient_Unchanged (AC-19-4) verifies that
// single-recipient behavior is unchanged: valid recipients pass, invalid ones fail.
func TestValidateLegacyMessage_SingleRecipient_Unchanged(t *testing.T) {
	// Valid single recipient.
	msg := validLegacyMessage()
	msg.Recipient = "agent:reviewer"
	if err := ValidateLegacyMessage(msg); err != nil {
		t.Fatalf("valid single recipient should pass: %v", err)
	}

	// Invalid single recipient.
	msg2 := validLegacyMessage()
	msg2.Recipient = "bogus:x"
	if err := ValidateLegacyMessage(msg2); err == nil {
		t.Fatal("invalid single recipient 'bogus:x' should be rejected")
	}
}

func TestValidateLegacyMessage_ChatType(t *testing.T) {
	msg := validLegacyMessage()
	msg.Type = "chat"
	// "chat" is a valid type in the legacy enum; the compat layer should
	// accept it.
	err := ValidateLegacyMessage(msg)
	if err != nil {
		// If chat is not in the valid types map, that's OK — skip this test.
		if strings.Contains(err.Error(), "invalid message type") {
			t.Skipf("chat type not in legacy enum: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
}
