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

package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errStoredMessageUnrecoverable reports that a persisted store.Message row
// lacks a required envelope input (id, sender, recipient, body, CreatedAt, or
// a known message type) and cannot be safely re-driven with a Scion sender
// envelope (ptone/scion#7881, contract shared with ptone/scion#7949). Callers
// must fail such a row rather than dispatching raw body text.
var errStoredMessageUnrecoverable = errors.New("stored message cannot be rebuilt for dispatch")

// storedMessageDispatch rebuilds the StructuredMessage and message-ID dispatch
// context from a persisted store.Message row so re-delivery carries the hub
// message ID (for Conduit row binding) and the server-derived sender envelope.
//
// Pure function: performs no I/O, excludes attachments (ptone/scion#7939),
// sets StoredMessageID = row.ID so downstream subscribers never persist a
// duplicate row, and sets ConversationAsserted = false.
//
// Documented functional limitations (design.md Phase A2 §3, T1):
//   - Plain, caller Metadata, and the original pre-rendered DeliveryText are
//     not stored on store.Message, so re-driven messages are formatted by the
//     broker from StructuredMessage fields (including adding the standard
//     sender envelope to messages originally sent plain). For TypeChoiceResponse,
//     processClaimedHeldRow enriches the rebuilt StructuredMessage from the
//     persisted decision_answers / decisions rows so the asking agent still
//     receives the [choice-response] line and hub-set choice metadata.
func storedMessageDispatch(ctx context.Context, row *store.Message) (context.Context, *messages.StructuredMessage, error) {
	if row == nil {
		return nil, nil, fmt.Errorf("%w: nil row", errStoredMessageUnrecoverable)
	}
	if row.ID == "" {
		return nil, nil, fmt.Errorf("%w: missing id", errStoredMessageUnrecoverable)
	}
	if row.Sender == "" {
		return nil, nil, fmt.Errorf("%w: missing sender", errStoredMessageUnrecoverable)
	}
	if row.Recipient == "" {
		return nil, nil, fmt.Errorf("%w: missing recipient", errStoredMessageUnrecoverable)
	}
	if row.Msg == "" {
		return nil, nil, fmt.Errorf("%w: empty body", errStoredMessageUnrecoverable)
	}
	if row.CreatedAt.IsZero() {
		return nil, nil, fmt.Errorf("%w: missing created timestamp", errStoredMessageUnrecoverable)
	}
	if err := messages.ValidateType(row.Type); err != nil {
		return nil, nil, fmt.Errorf("%w: unknown type %q", errStoredMessageUnrecoverable, messages.LogType(row.Type))
	}

	sm := &messages.StructuredMessage{
		Version:              messages.Version,
		Timestamp:            row.CreatedAt.UTC().Format(time.RFC3339),
		Sender:               row.Sender,
		SenderID:             row.SenderID,
		Recipient:            row.Recipient,
		RecipientID:          row.RecipientID,
		Msg:                  row.Msg,
		Type:                 row.Type,
		Urgent:               row.Urgent,
		Broadcasted:          row.Broadcasted,
		Channel:              row.Channel,
		ThreadID:             row.ThreadID,
		ConversationID:       row.ConversationID,
		ConversationAsserted: false,
		StoredMessageID:      row.ID,
	}
	return withDispatchMessageID(ctx, row.ID), sm, nil
}
