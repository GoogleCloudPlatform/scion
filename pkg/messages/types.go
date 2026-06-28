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

// Package messages defines structured message types for the Scion messaging system.
package messages

import (
	"fmt"
	"regexp"
	"time"
)

// Schema version for the structured message format.
const Version = 1

// Maximum length of the Msg field in characters for outbound messages.
const MaxMessageLength = 2000

// Maximum size of the Msg field in bytes.
const MaxMsgSize = 64 * 1024 // 64KB

// Maximum number of attachments.
const MaxAttachments = 10

// Maximum number of metadata entries.
const MaxMetadataEntries = 32

// Maximum size of a single metadata key in bytes.
const MaxMetadataKeySize = 256

// Maximum size of a single metadata value in bytes.
const MaxMetadataValueSize = 4 * 1024 // 4KB

// Maximum length of the Channel field.
const MaxChannelLength = 64

// channelRegexp validates that a channel name contains only alphanumeric characters and hyphens.
var channelRegexp = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

// Message type constants (closed enum).
const (
	TypeInstruction    = "instruction"
	TypeInputNeeded    = "input-needed"
	TypeStateChange    = "state-change"
	TypeAssistantReply = "assistant-reply"
	TypeGroupSet       = "group-set"
)

// Visibility constants control which consumers see a message.
// Downstream consumers (chat apps, web UI, broker plugins) filter
// messages by visibility level to avoid surfacing raw agent output
// (e.g. thinking traces) in normal chat views.
const (
	// VisibilityNormal — always shown. Used for explicit agent→user
	// messages (scion message, ask_user) and user→agent instructions.
	VisibilityNormal = "normal"

	// VisibilityVerbose — shown in verbose mode. Used for automatic
	// assistant replies from hook events (agent turn output without
	// thinking content).
	VisibilityVerbose = "verbose"

	// VisibilityFull — shown only in full-fidelity mode. Used for
	// content that includes thinking/reasoning traces and raw tool
	// output. Intended for ACP streams and debugging interfaces.
	VisibilityFull = "full"
)

// validTypes is the set of valid message types.
var validTypes = map[string]bool{
	TypeInstruction:    true,
	TypeInputNeeded:    true,
	TypeStateChange:    true,
	TypeAssistantReply: true,
	TypeGroupSet:       true,
}

// StructuredMessage represents a formatted Scion message.
type StructuredMessage struct {
	Version      int               `json:"version"`
	Timestamp    string            `json:"timestamp"`
	Sender       string            `json:"sender"`
	SenderID     string            `json:"sender_id,omitempty"`
	Recipient    string            `json:"recipient"`
	RecipientID  string            `json:"recipient_id,omitempty"`
	Recipients   string            `json:"recipients,omitempty"`
	Msg          string            `json:"msg"`
	Type         string            `json:"type"`
	Plain        bool              `json:"plain,omitempty"`
	Raw          bool              `json:"raw,omitempty"`
	Urgent       bool              `json:"urgent,omitempty"`
	Broadcasted  bool              `json:"broadcasted,omitempty"`
	ObserverOnly bool              `json:"observer_only,omitempty"`
	Status       string            `json:"status,omitempty"`
	Attachments  []string          `json:"attachments,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Channel      string            `json:"channel,omitempty"`
	ThreadID     string            `json:"thread_id,omitempty"`

	// Visibility controls which consumers see this message.
	// One of VisibilityNormal, VisibilityVerbose, or VisibilityFull.
	// Empty defaults to VisibilityNormal for backward compatibility.
	Visibility string `json:"visibility,omitempty"`
}

// ValidateType returns an error if the message type is not in the closed enum.
func ValidateType(t string) error {
	if !validTypes[t] {
		return fmt.Errorf("invalid message type %q: must be one of: instruction, input-needed, state-change, assistant-reply, group-set", t)
	}
	return nil
}

// Validate checks the structured message for correctness.
func (m *StructuredMessage) Validate() error {
	if m.Version != Version {
		return fmt.Errorf("unsupported message version %d (expected %d)", m.Version, Version)
	}
	if m.Msg == "" {
		return fmt.Errorf("msg field is required")
	}
	if len([]rune(m.Msg)) > MaxMessageLength {
		return fmt.Errorf("message exceeds %d character limit (current: %d chars). Consider splitting into multiple messages using multiple scion message invocations", MaxMessageLength, len([]rune(m.Msg)))
	}
	if len(m.Msg) > MaxMsgSize {
		return fmt.Errorf("msg exceeds maximum size of %d bytes", MaxMsgSize)
	}
	if err := ValidateType(m.Type); err != nil {
		return err
	}
	if m.Sender == "" {
		return fmt.Errorf("sender is required")
	}
	if m.Recipient == "" {
		return fmt.Errorf("recipient is required")
	}
	if len(m.Attachments) > MaxAttachments {
		return fmt.Errorf("too many attachments: %d (max %d)", len(m.Attachments), MaxAttachments)
	}
	if len(m.Metadata) > MaxMetadataEntries {
		return fmt.Errorf("too many metadata entries: %d (max %d)", len(m.Metadata), MaxMetadataEntries)
	}
	for k, v := range m.Metadata {
		if len(k) > MaxMetadataKeySize {
			return fmt.Errorf("metadata key %q... exceeds maximum size of %d bytes", k[:32], MaxMetadataKeySize)
		}
		if len(v) > MaxMetadataValueSize {
			return fmt.Errorf("metadata value for key %q exceeds maximum size of %d bytes", k, MaxMetadataValueSize)
		}
	}
	if m.ThreadID != "" && m.Channel == "" {
		return fmt.Errorf("thread_id requires channel to be set")
	}
	if m.Channel != "" {
		if len(m.Channel) > MaxChannelLength {
			return fmt.Errorf("channel exceeds maximum length of %d characters", MaxChannelLength)
		}
		if !channelRegexp.MatchString(m.Channel) {
			return fmt.Errorf("channel %q contains invalid characters: must be alphanumeric or hyphens", m.Channel)
		}
	}
	return nil
}

// NewInstruction creates a new instruction message with standard defaults.
func NewInstruction(sender, recipient, msg string) *StructuredMessage {
	return &StructuredMessage{
		Version:   Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    sender,
		Recipient: recipient,
		Msg:       msg,
		Type:      TypeInstruction,
	}
}

// NewNotification creates a new notification message (state-change or input-needed).
func NewNotification(sender, recipient, msg, msgType string) *StructuredMessage {
	return &StructuredMessage{
		Version:   Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    sender,
		Recipient: recipient,
		Msg:       msg,
		Type:      msgType,
	}
}

// LogAttrs returns slog attributes for structured logging of this message.
func (m *StructuredMessage) LogAttrs() []any {
	attrs := []any{
		"sender", m.Sender,
		"recipient", m.Recipient,
		"msg_type", m.Type,
		"message_content", m.Msg,
		"urgent", m.Urgent,
		"broadcasted", m.Broadcasted,
		"plain", m.Plain,
		"raw", m.Raw,
	}
	if m.SenderID != "" {
		attrs = append(attrs, "sender_id", m.SenderID)
	}
	if m.RecipientID != "" {
		attrs = append(attrs, "recipient_id", m.RecipientID)
	}
	if m.Recipients != "" {
		attrs = append(attrs, "recipients", m.Recipients)
	}
	if m.Channel != "" {
		attrs = append(attrs, "channel", m.Channel)
	}
	if m.ThreadID != "" {
		attrs = append(attrs, "thread_id", m.ThreadID)
	}
	return attrs
}

// SenderPrefix returns the type prefix for a sender identity string.
// For example, "user:alice" returns "user", "agent:code-reviewer" returns "agent".
func SenderPrefix(identity string) string {
	for i, c := range identity {
		if c == ':' {
			return identity[:i]
		}
	}
	return identity
}
