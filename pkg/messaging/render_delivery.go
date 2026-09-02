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
	"context"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RenderDeliveryInput carries everything the rendering helper needs from each
// call site. No call site should build a ConversationInfo or call
// FormatNewDelivery directly — the helper owns the envelope's shape.
type RenderDeliveryInput struct {
	// MessageID is the persisted row's ID. Required — the helper will not
	// fabricate an identifier.
	MessageID string

	// ConvResult is the conversation resolution outcome. May be nil when
	// the conversation is absent or unresolvable (e.g. broadcasts). When
	// nil, the envelope omits the conversation key entirely (§4.3).
	ConvResult *ConversationResult

	// Msg is the StructuredMessage being dispatched. The helper reads
	// sender, recipient, type, body, attachments, metadata, visibility,
	// and transport flags (Plain, Raw) from it.
	Msg *messages.StructuredMessage

	// CreatedAt is the message timestamp. When zero, time.Now().UTC() is
	// used — but callers should supply the persisted row's CreatedAt.
	CreatedAt time.Time
}

// RenderDeliveryText is the single shared rendering entry point for all hub
// send paths (Phase 9b(ii)). It converts a StructuredMessage and its
// conversation context into the fully rendered agent-facing envelope text.
//
// Invariants:
//   - Never fabricates an identifier. Where data is absent, the field is
//     omitted rather than synthesised.
//   - reply_to is always omitted in this phase. A genuine reply target
//     requires work in 9c(iii); until then, absent is correct and a
//     fabricated thread ID is not.
//   - When ConvResult is nil, the envelope carries no conversation key
//     (honest absence per §4.3).
func RenderDeliveryText(in RenderDeliveryInput) string {
	if in.Msg == nil {
		return ""
	}

	// Transport flags: plain/raw messages deliver body text only.
	if in.Msg.Plain || in.Msg.Raw {
		return in.Msg.Msg
	}

	// Use MapLegacyEnvelope for the type→kind/intent/event conversion,
	// PrincipalRef construction, visibility mapping and addressee building.
	// Then override the three fabricated identifiers with real data.
	msg, addrs, err := MapLegacyEnvelope(in.Msg)
	if err != nil {
		// MapLegacyEnvelope only fails on nil input, which we checked.
		return in.Msg.Msg
	}

	// Override fabricated message ID with the persisted row's ID.
	msg.ID = in.MessageID

	// Override fabricated reply_to. In this phase, always omit — a genuine
	// reply target does not exist yet (9c(iii)). The hard constraint says:
	// an identifier that dereferences to nothing is worse than absent.
	msg.ReplyToID = nil

	// Override timestamp if we have a real one from the persisted row.
	if !in.CreatedAt.IsZero() {
		msg.CreatedAt = in.CreatedAt.UTC()
	}

	// Build ConversationInfo from the enriched ConversationResult.
	var convInfo ConversationInfo
	if in.ConvResult != nil {
		convInfo = ConversationInfo{
			ID:      in.ConvResult.ConversationID,
			Kind:    in.ConvResult.Kind,
			Surface: in.ConvResult.Surface,
			Name:    in.ConvResult.DisplayName,
		}
	}

	// FormatNewDelivery handles the envelope framing.
	opts := DeliveryOptions{
		Plain: in.Msg.Plain,
		Raw:   in.Msg.Raw,
	}
	return FormatNewDelivery(msg, addrs, convInfo, opts)
}

// ConversationGetter is the minimal interface for looking up a conversation
// by ID. Used by RenderDeliveryTextWithLookup when the call site does not
// have a ConversationResult (e.g. paths from the off-limits file).
type ConversationGetter interface {
	GetConversation(ctx context.Context, id string) (*store.Conversation, error)
}

// RenderDeliveryTextWithLookup renders the delivery envelope, looking up the
// conversation by ID from the StructuredMessage when no ConversationResult is
// provided. This is the fallback path for call sites where the conversation
// resolution happens in a file that cannot be modified (e.g.
// handlers_agent_messaging.go).
//
// When the lookup fails or the ConversationID is empty, the envelope omits
// the conversation key (honest absence). The lookup failure is logged at
// DEBUG — it is not an error because broadcasts and pre-migration messages
// legitimately have no conversation.
func RenderDeliveryTextWithLookup(
	ctx context.Context,
	cg ConversationGetter,
	log *slog.Logger,
	in RenderDeliveryInput,
) string {
	if in.Msg == nil {
		return ""
	}

	// If we already have a ConvResult, use it directly.
	if in.ConvResult != nil {
		return RenderDeliveryText(in)
	}

	// Attempt to look up the conversation from the ID on the StructuredMessage.
	convID := in.Msg.ConversationID
	if convID == "" {
		// No conversation — render without it. This is the broadcast path
		// and any message where conversation resolution was skipped.
		return RenderDeliveryText(in)
	}

	conv, err := cg.GetConversation(ctx, convID)
	if err != nil || conv == nil {
		if log != nil {
			log.Debug("delivery render: conversation lookup failed, omitting conversation",
				"conversation_id", convID,
				"message_id", in.MessageID,
				"error", err)
		}
		return RenderDeliveryText(in)
	}

	in.ConvResult = &ConversationResult{
		ConversationID: conv.ID,
		ExternalRef:    conv.ExternalRef,
		Kind:           conv.Kind,
		Surface:        conv.Surface,
		DisplayName:    conv.DisplayName,
	}
	return RenderDeliveryText(in)
}
