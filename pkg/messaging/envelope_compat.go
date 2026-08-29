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
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// MapLegacyType maps an old type enum value (and optional system_category
// metadata) to the new split taxonomy: kind, intent, and event body.
//
// The old 8-value type enum mixes four concerns (provenance, intent, lifecycle,
// delivery artifact) into a single field. This function decomposes them.
//
// Ambiguous cases:
//   - "input-needed": hasAddressee=true → text/question; false → event/agent.input-needed.
//   - "system": maps system_category to event type; unknown categories default
//     to agent.state-changed with a log warning.
//   - "mention", "group-set": delivery artifacts, not message kinds. Mapped to
//     text/request (the typical underlying intent). Use MapLegacyDeliveryArtifact
//     to get the corresponding AddressedVia.
func MapLegacyType(oldType, systemCategory string, hasAddressee bool) (MessageKind, *TextIntent, *EventBody) {
	switch oldType {
	case messages.TypeInstruction:
		intent := IntentRequest
		return KindText, &intent, nil

	case messages.TypeChat:
		intent := IntentInform
		return KindText, &intent, nil

	case messages.TypeAssistantReply:
		// Provenance (assistant vs user) is now captured in the From field.
		intent := IntentInform
		return KindText, &intent, nil

	case messages.TypeInputNeeded:
		// Ambiguous: when addressed to a specific recipient it is a question
		// to that recipient. When broadcast/unaddressed it is a lifecycle event.
		if hasAddressee {
			intent := IntentQuestion
			return KindText, &intent, nil
		}
		return KindEvent, nil, &EventBody{Type: EventAgentInputNeeded}

	case messages.TypeStateChange:
		return KindEvent, nil, &EventBody{Type: EventAgentStateChanged}

	case messages.TypeSystem:
		return KindEvent, nil, mapSystemCategory(systemCategory)

	case messages.TypeMention:
		// Delivery artifact. The underlying message is typically a request.
		intent := IntentRequest
		return KindText, &intent, nil

	case messages.TypeGroupSet:
		// Delivery artifact. The underlying message is typically a request.
		intent := IntentRequest
		return KindText, &intent, nil

	default:
		// Unknown type — treat as text/inform and log a warning.
		slog.Warn("unknown legacy message type, defaulting to text/inform",
			"old_type", oldType)
		intent := IntentInform
		return KindText, &intent, nil
	}
}

// mapSystemCategory converts a system_category metadata value to an EventBody.
func mapSystemCategory(category string) *EventBody {
	switch category {
	case messages.SystemCategoryScheduler:
		return &EventBody{Type: EventScheduleFired}
	case messages.SystemCategoryPortForward:
		return &EventBody{Type: EventPortExposed}
	case messages.SystemCategoryDeliveryFailed:
		return &EventBody{Type: EventDeliveryFailed}
	default:
		slog.Warn("unknown system_category, defaulting to agent.state-changed",
			"system_category", category)
		return &EventBody{Type: EventAgentStateChanged}
	}
}

// MapLegacyDeliveryArtifact returns the AddressedVia value for old types that
// are delivery artifacts rather than message kinds. Returns nil for types that
// are not delivery artifacts.
func MapLegacyDeliveryArtifact(oldType string) *AddressedVia {
	switch oldType {
	case messages.TypeMention:
		via := ViaBodyMention
		return &via
	case messages.TypeGroupSet:
		via := ViaExplicit
		return &via
	default:
		return nil
	}
}

// MapLegacyEnvelope converts a legacy StructuredMessage into the new Message
// and Addressee types. The conversion is best-effort: fields that have no
// direct equivalent are mapped to the closest semantic match.
//
// The returned message ID is synthesised from the timestamp if no other
// identifier is available in the old format.
func MapLegacyEnvelope(old *messages.StructuredMessage) (*Message, []Addressee, error) {
	if old == nil {
		return nil, nil, fmt.Errorf("cannot convert nil StructuredMessage")
	}

	// Determine whether this message has a specific addressee (not broadcast).
	hasAddressee := old.Recipient != "" && !old.Broadcasted

	// Map type to new taxonomy.
	systemCategory := old.Metadata["system_category"]
	kind, intent, event := MapLegacyType(old.Type, systemCategory, hasAddressee)

	// Enrich event body from old fields where possible.
	if event != nil {
		if old.Status != "" {
			event.Status = old.Status
		}
		// For state-change, the sender is typically the subject.
		if event.Type == EventAgentStateChanged && old.Sender != "" {
			event.Subject = old.Sender
		}
	}

	// Build the sender PrincipalRef.
	from := buildPrincipalRef(old.Sender, old.SenderID)

	// Parse timestamp.
	createdAt, err := time.Parse(time.RFC3339, old.Timestamp)
	if err != nil {
		createdAt = time.Now().UTC()
	}

	// Map attachments.
	var attachments []AttachmentRef
	for _, path := range old.Attachments {
		attachments = append(attachments, AttachmentRef{Path: path})
	}

	// Map visibility.
	vis := mapLegacyVisibility(old.Visibility)

	// Synthesise a message ID from the timestamp (old format has no ID field).
	msgID := fmt.Sprintf("legacy-%s", old.Timestamp)

	// Map thread to reply-to (best-effort).
	var replyToID *string
	if old.ThreadID != "" {
		tid := old.ThreadID
		replyToID = &tid
	}

	msg := &Message{
		ID:          msgID,
		ReplyToID:   replyToID,
		From:        from,
		Kind:        kind,
		Intent:      intent,
		Event:       event,
		Body:        old.Msg,
		Attachments: attachments,
		Visibility:  vis,
		CreatedAt:   createdAt,
	}

	// Build addressees.
	addrs := buildAddressees(old, msgID)

	return msg, addrs, nil
}

// buildPrincipalRef constructs a PrincipalRef from old sender/senderID fields.
// The old format has name (e.g. "user:alice", "agent:builder") and id (a raw
// UUID or a prefixed ref). When id is a raw identifier without a colon, the
// kind prefix is derived from name so the result is a valid PrincipalRef.
func buildPrincipalRef(name, id string) PrincipalRef {
	if id != "" {
		// If id is already a valid PrincipalRef (contains colon), use directly.
		if strings.Contains(id, ":") {
			return PrincipalRef(id)
		}
		// id is a raw identifier (e.g. UUID) — derive kind from name.
		if idx := strings.IndexByte(name, ':'); idx > 0 {
			return PrincipalRef(name[:idx] + ":" + id)
		}
		// No kind derivable from name — default to system.
		return PrincipalRef("system:" + id)
	}
	// Heuristic: if name already has a colon prefix, use it directly.
	if idx := len(name); idx > 0 {
		for i, c := range name {
			if c == ':' && i > 0 {
				return PrincipalRef(name)
			}
		}
	}
	// Default: treat as system component.
	if name != "" {
		return PrincipalRef("system:" + name)
	}
	return PrincipalRef("system:unknown")
}

// buildAddressees constructs Addressee records from the old message.
//
// When old.Recipient uses the group[] form (e.g. "group[agent:a,user:b]"),
// one Addressee is emitted per member using messages.ParseGroupRecipient.
// The old.RecipientID field does not apply to group recipients (it names a
// single principal) and is ignored in that case.
func buildAddressees(old *messages.StructuredMessage, msgID string) []Addressee {
	var addrs []Addressee

	// Determine the via from the old type if it is a delivery artifact.
	via := ViaExplicit
	if artifact := MapLegacyDeliveryArtifact(old.Type); artifact != nil {
		via = *artifact
	}

	if old.Recipient == "" {
		return addrs
	}

	// Handle group[] recipients: parse with the canonical parser and emit
	// one Addressee per member. If parsing fails, fall through to the
	// single-principal path so downstream validation catches the error.
	//
	// Via is ViaExplicit unconditionally, not the computed `via` from
	// MapLegacyDeliveryArtifact. group[] is an explicit addressing form —
	// every member was named by the sender. The single-principal `via`
	// varies only for TypeMention (→ ViaBodyMention), which the CLI cannot
	// combine with group[] (it uses TypeGroupSet; mentions fan out as
	// individual single-recipient messages). A raw API caller sending
	// TypeMention + group[] would still get ViaExplicit, which is correct:
	// they explicitly listed the recipients.
	if messages.IsGroupRecipient(old.Recipient) {
		members, err := messages.ParseGroupRecipient(old.Recipient)
		if err == nil {
			for _, m := range members {
				addrs = append(addrs, Addressee{
					MessageID:     msgID,
					PrincipalKind: string(m.Kind),
					PrincipalID:   m.Name,
					Via:           ViaExplicit,
					DeliveryState: DeliveryPending,
				})
			}
			return addrs
		}
		// Parse error: fall through to single-principal path.
	}

	// Single-principal path.
	ref := buildPrincipalRef(old.Recipient, old.RecipientID)
	addrs = append(addrs, Addressee{
		MessageID:     msgID,
		PrincipalKind: ref.PrincipalKind(),
		PrincipalID:   ref.PrincipalID(),
		Via:           via,
		DeliveryState: DeliveryPending,
	})

	return addrs
}

// mapLegacyVisibility converts a legacy visibility string to the new Visibility type.
func mapLegacyVisibility(old string) Visibility {
	switch old {
	case messages.VisibilityVerbose:
		return VisibilityVerbose
	case messages.VisibilityFull:
		return VisibilityFull
	default:
		return VisibilityNormal
	}
}

// NewEnvelopeToLegacy converts a new Message and its Addressees back to the
// old StructuredMessage format. This supports backward compatibility during the
// transition period for code that still reads the old format.
//
// Loss is expected: the new format captures information (kind/intent split,
// multiple addressees, delivery state) that the old format cannot represent.
func NewEnvelopeToLegacy(msg *Message, addrs []Addressee) *messages.StructuredMessage {
	if msg == nil {
		return nil
	}

	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: msg.CreatedAt.UTC().Format(time.RFC3339),
		Msg:       msg.Body,
	}

	// Map From to sender fields.
	old.Sender = string(msg.From)
	old.SenderID = string(msg.From)

	// Map kind/intent/event back to old type.
	old.Type = mapNewTypeToLegacy(msg)

	// Map visibility.
	old.Visibility = string(msg.Visibility)
	if old.Visibility == string(VisibilityNormal) {
		old.Visibility = "" // old format uses empty for normal
	}

	// Map attachments.
	for _, a := range msg.Attachments {
		old.Attachments = append(old.Attachments, a.Path)
	}

	// Map metadata from event body.
	if msg.Event != nil {
		old.Metadata = make(map[string]string)
		if cat := eventTypeToSystemCategory(msg.Event.Type); cat != "" {
			old.Metadata["system_category"] = cat
		}
		if msg.Event.Status != "" {
			old.Status = msg.Event.Status
		}
	}

	// Map first addressee to recipient.
	if len(addrs) > 0 {
		first := addrs[0]
		old.Recipient = first.PrincipalKind + ":" + first.PrincipalID
		old.RecipientID = old.Recipient

		// Override type for delivery artifact via values.
		switch first.Via {
		case ViaBodyMention:
			old.Type = messages.TypeMention
			old.Metadata = ensureMetadata(old.Metadata)
			old.Metadata["mention_source"] = string(msg.From)
			old.Metadata["mention_position"] = "body"
		case ViaExplicit:
			// If there are multiple addressees, the old format uses group-set.
			if len(addrs) > 1 {
				old.Type = messages.TypeGroupSet
			}
		}
	}

	return old
}

// mapNewTypeToLegacy maps the new kind/intent/event back to the old type enum.
func mapNewTypeToLegacy(msg *Message) string {
	switch msg.Kind {
	case KindText:
		if msg.Intent == nil {
			return messages.TypeChat
		}
		switch *msg.Intent {
		case IntentRequest:
			return messages.TypeInstruction
		case IntentQuestion:
			return messages.TypeInputNeeded
		case IntentInform:
			// Check if sender is an agent — old format distinguished assistant-reply.
			if msg.From.PrincipalKind() == "agent" {
				return messages.TypeAssistantReply
			}
			return messages.TypeChat
		default:
			return messages.TypeChat
		}
	case KindEvent:
		if msg.Event == nil {
			return messages.TypeStateChange
		}
		switch msg.Event.Type {
		case EventAgentStateChanged:
			return messages.TypeStateChange
		case EventAgentInputNeeded:
			return messages.TypeInputNeeded
		case EventScheduleFired, EventPortExposed, EventDeliveryFailed:
			return messages.TypeSystem
		default:
			return messages.TypeStateChange
		}
	default:
		return messages.TypeChat
	}
}

// eventTypeToSystemCategory maps an EventType back to the old system_category.
func eventTypeToSystemCategory(et EventType) string {
	switch et {
	case EventScheduleFired:
		return messages.SystemCategoryScheduler
	case EventPortExposed:
		return messages.SystemCategoryPortForward
	case EventDeliveryFailed:
		return messages.SystemCategoryDeliveryFailed
	default:
		return ""
	}
}

// ensureMetadata returns the metadata map, creating it if nil.
func ensureMetadata(m map[string]string) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	return m
}
