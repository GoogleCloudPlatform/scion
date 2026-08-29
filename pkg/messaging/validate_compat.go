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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ValidateLegacyMessage validates a StructuredMessage by mapping it to the
// new envelope types and running the new validator. This is the adapter that
// makes the old envelope go through the new validation choke point.
//
// It checks legacy-specific invariants (e.g. thread_id requires channel) that
// are not represented in the new types, then converts and validates.
func ValidateLegacyMessage(msg *messages.StructuredMessage) error {
	if msg == nil {
		return fmt.Errorf("message must not be nil")
	}

	// ---- Legacy-specific checks (not representable in new envelope) ----

	// The old type must be from the closed enum.
	if err := messages.ValidateType(msg.Type); err != nil {
		return err
	}

	// thread_id requires channel (the Teams regression check).
	if msg.ThreadID != "" && msg.Channel == "" {
		return fmt.Errorf("thread_id requires channel to be set")
	}

	// Channel format checks.
	if msg.Channel != "" {
		if len(msg.Channel) > messages.MaxChannelLength {
			return fmt.Errorf("channel exceeds maximum length of %d characters", messages.MaxChannelLength)
		}
		if !messages.IsValidChannel(msg.Channel) {
			return fmt.Errorf("channel %q contains invalid characters", msg.Channel)
		}
	}

	// Metadata limits (from old Validate).
	if len(msg.Metadata) > messages.MaxMetadataEntries {
		return fmt.Errorf("too many metadata entries: %d (max %d)",
			len(msg.Metadata), messages.MaxMetadataEntries)
	}
	for k, v := range msg.Metadata {
		if len(k) > messages.MaxMetadataKeySize {
			return fmt.Errorf("metadata key exceeds maximum size of %d bytes",
				messages.MaxMetadataKeySize)
		}
		if len(v) > messages.MaxMetadataValueSize {
			return fmt.Errorf("metadata value for key %q exceeds maximum size of %d bytes",
				k, messages.MaxMetadataValueSize)
		}
	}

	// Body must not be empty unless attachments are present (legacy rule).
	// Attachment-only messages (e.g. image uploads from chat v2) have an empty
	// Msg field but carry content via StructuredMessage.Attachments.
	if msg.Msg == "" && len(msg.Attachments) == 0 {
		return fmt.Errorf("msg field is required")
	}

	// Sender must be set (legacy rule).
	if msg.Sender == "" {
		return fmt.Errorf("sender is required")
	}

	// ---- Convert to new types and validate through new choke point ----

	newMsg, addrs, err := MapLegacyEnvelope(msg)
	if err != nil {
		return fmt.Errorf("legacy envelope conversion failed: %w", err)
	}

	// Shape/content validation only — ConversationID is checked later by
	// ValidateAttributed, after the conversation attribution layer has had
	// the chance to set a real one. See DEF-41.
	if err := validateMessageContent(newMsg); err != nil {
		return err
	}

	if len(addrs) > 0 {
		if err := ValidateAddressees(addrs, newMsg); err != nil {
			return err
		}
	}

	return nil
}
