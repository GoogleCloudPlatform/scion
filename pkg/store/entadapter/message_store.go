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

package entadapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// MessagePublisher is the hook through which newly created messages are
// announced to other hub replicas. On Postgres this is implemented with
// LISTEN/NOTIFY (a pg_notify on a "user_message" channel) so that subscribers
// receive new messages without polling; the SQLite backend leaves it nil.
//
// It is intentionally an interface rather than a hard dependency so the message
// store stays decoupled from the notification transport, and so the publish
// call can be a no-op until the Postgres LISTEN/NOTIFY listener (Wave B) is
// wired in.
type MessagePublisher interface {
	// PublishUserMessage announces that msg was persisted. Implementations must
	// be best-effort: a publish failure must not fail the originating write.
	PublishUserMessage(ctx context.Context, msg *store.Message) error
}

// MessageStore implements store.MessageStore using the Ent ORM.
type MessageStore struct {
	client *ent.Client
	// publisher, when non-nil, is notified after each successful CreateMessage.
	// See MessagePublisher.
	publisher MessagePublisher
}

// NewMessageStore creates a new Ent-backed MessageStore.
func NewMessageStore(client *ent.Client) *MessageStore {
	return &MessageStore{client: client}
}

// WithPublisher returns a copy of the store that announces newly created
// messages via the given publisher. Used to wire in the Postgres LISTEN/NOTIFY
// transport without changing the store's construction site.
func (s *MessageStore) WithPublisher(p MessagePublisher) *MessageStore {
	clone := *s
	clone.publisher = p
	return &clone
}

func entMessageToStore(e *ent.Message) *store.Message {
	// Read-time visibility backfill (design §4.6):
	// Old rows have empty visibility. Normalise so every consumer sees a
	// consistent value without requiring a data migration.
	vis := e.Visibility
	if vis == "" {
		if e.Type == "assistant-reply" {
			vis = "verbose"
		} else {
			vis = "normal"
		}
	}

	return &store.Message{
		ID:                    e.ID.String(),
		ProjectID:             e.ProjectID.String(),
		Sender:                e.Sender,
		SenderID:              e.SenderID,
		Recipient:             e.Recipient,
		RecipientID:           e.RecipientID,
		Msg:                   e.Msg,
		Type:                  e.Type,
		Urgent:                e.Urgent,
		Broadcasted:           e.Broadcasted,
		Read:                  e.Read,
		AgentID:               e.AgentID,
		GroupID:               e.GroupID,
		Channel:               e.Channel,
		ThreadID:              e.ThreadID,
		Visibility:            vis,
		CreatedAt:             e.Created,
		DispatchState:         e.DispatchState,
		DispatchedAt:          e.DispatchedAt,
		DispatchFailureReason: e.DispatchFailureReason,
	}
}

// CreateMessage persists a new message and announces it via the publisher.
func (s *MessageStore) CreateMessage(ctx context.Context, msg *store.Message) error {
	if msg.ID == "" || msg.ProjectID == "" || msg.Msg == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(msg.ID)
	if err != nil {
		return err
	}
	pid, err := parseUUID(msg.ProjectID)
	if err != nil {
		return err
	}

	create := s.client.Message.Create().
		SetID(uid).
		SetProjectID(pid).
		SetSender(msg.Sender).
		SetSenderID(msg.SenderID).
		SetRecipient(msg.Recipient).
		SetRecipientID(msg.RecipientID).
		SetMsg(msg.Msg).
		SetType(msg.Type).
		SetUrgent(msg.Urgent).
		SetBroadcasted(msg.Broadcasted).
		SetRead(msg.Read).
		SetAgentID(msg.AgentID).
		SetGroupID(msg.GroupID)

	if msg.Channel != "" {
		create.SetChannel(msg.Channel)
	}
	if msg.ThreadID != "" {
		create.SetThreadID(msg.ThreadID)
	}
	if msg.Visibility != "" {
		create.SetVisibility(msg.Visibility)
	}

	if msg.Type == "" {
		create.SetType("instruction")
	}
	if msg.DispatchState != "" {
		create.SetDispatchState(msg.DispatchState)
	}
	if msg.DispatchedAt != nil {
		create.SetDispatchedAt(*msg.DispatchedAt)
	}
	if !msg.CreatedAt.IsZero() {
		create.SetCreated(msg.CreatedAt)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	msg.CreatedAt = created.Created
	msg.Type = created.Type
	msg.DispatchState = created.DispatchState

	// Design-in: announce the new message for LISTEN/NOTIFY subscribers.
	// Best-effort — a publish failure must not fail the write that succeeded.
	if s.publisher != nil {
		_ = s.publisher.PublishUserMessage(ctx, msg)
	}
	return nil
}

// GetMessage returns a single message by ID.
func (s *MessageStore) GetMessage(ctx context.Context, id string) (*store.Message, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.Message.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entMessageToStore(e), nil
}

// encodeCursor produces a self-contained, opaque pagination cursor that embeds
// both the created timestamp and the message ID. Format: base64(RFC3339Nano + "," + uuid).
// This avoids a DB round-trip on decode and makes pagination resilient to message deletion.
func encodeCursor(created time.Time, id string) string {
	raw := created.Format(time.RFC3339Nano) + "," + id
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor is the inverse of encodeCursor. It returns the created timestamp and UUID
// embedded in the cursor, or an error if the cursor is malformed.
func decodeCursor(cursor string) (time.Time, uuid.UUID, error) {
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("base64 decode: %w", err)
	}
	parts := strings.SplitN(string(raw), ",", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("expected 'timestamp,id' format")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("parse timestamp: %w", err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("parse id: %w", err)
	}
	return ts, id, nil
}

// ListMessages returns messages matching the given filter, ordered by
// created_at DESC.
func (s *MessageStore) ListMessages(ctx context.Context, filter store.MessageFilter, opts store.ListOptions) (*store.ListResult[store.Message], error) {
	query := s.client.Message.Query()

	if filter.ProjectID != "" {
		pid, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		query.Where(message.ProjectIDEQ(pid))
	}
	if filter.AgentID != "" {
		query.Where(message.AgentIDEQ(filter.AgentID))
	}
	if filter.RecipientID != "" {
		query.Where(message.RecipientIDEQ(filter.RecipientID))
	}
	if filter.SenderID != "" {
		query.Where(message.SenderIDEQ(filter.SenderID))
	}
	if filter.ParticipantID != "" {
		query.Where(message.Or(
			message.RecipientIDEQ(filter.ParticipantID),
			message.SenderIDEQ(filter.ParticipantID),
		))
	}
	if filter.OnlyUnread {
		query.Where(message.ReadEQ(false))
	}
	if filter.Type != "" {
		query.Where(message.TypeEQ(filter.Type))
	}
	if filter.Channel != "" {
		query.Where(message.ChannelEQ(filter.Channel))
	}
	// Visibility filter with NULL backfill awareness (review R1 fix):
	// Old rows have NULL visibility. The read-time backfill in entMessageToStore
	// normalises after fetch, but a simple IN predicate drops NULL rows before
	// they reach the Go layer. We expand the predicate to mirror the backfill
	// logic: NULL + type != "assistant-reply" → normal; NULL + type = "assistant-reply" → verbose.
	if len(filter.Visibility) > 0 {
		var preds []predicate.Message
		for _, v := range filter.Visibility {
			switch v {
			case "normal":
				preds = append(preds,
					message.VisibilityEQ("normal"),
					message.And(
						message.Or(
							message.VisibilityIsNil(),
							message.VisibilityEQ(""),
						),
						message.TypeNEQ("assistant-reply"),
					),
				)
			case "verbose":
				preds = append(preds,
					message.VisibilityEQ("verbose"),
					message.And(
						message.Or(
							message.VisibilityIsNil(),
							message.VisibilityEQ(""),
						),
						message.TypeEQ("assistant-reply"),
					),
				)
			default: // "full" or future values
				preds = append(preds, message.VisibilityEQ(v))
			}
		}
		query.Where(message.Or(preds...))
	}
	if !filter.Before.IsZero() {
		query.Where(message.CreatedLT(filter.Before))
	}

	// totalCount represents the total number of messages matching the base
	// filter (before cursor pagination is applied). Clone and count before
	// adding the cursor predicate so the count stays stable across pages.
	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	// Apply cursor-based keyset pagination.
	// The cursor is a self-contained base64-encoded string carrying (created, id).
	// This avoids a DB round-trip and makes pagination resilient to message deletion.
	if opts.Cursor != "" {
		cursorCreated, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		query.Where(message.Or(
			message.CreatedLT(cursorCreated),
			message.And(
				message.CreatedEQ(cursorCreated),
				message.IDLT(cursorID),
			),
		))
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(message.ByCreated(entsql.OrderDesc())).
		Order(message.ByID(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	msgs := make([]store.Message, 0, len(entities))
	for _, e := range entities {
		msgs = append(msgs, *entMessageToStore(e))
	}

	result := &store.ListResult[store.Message]{TotalCount: totalCount}
	if len(msgs) > limit {
		result.Items = msgs[:limit]
		last := msgs[limit-1]
		result.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	} else {
		result.Items = msgs
	}
	return result, nil
}

// MarkMessageRead marks a message as read.
func (s *MessageStore) MarkMessageRead(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	n, err := s.client.Message.Update().
		Where(message.IDEQ(uid)).
		SetRead(true).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// MarkAllMessagesRead marks all messages for a recipient as read.
func (s *MessageStore) MarkAllMessagesRead(ctx context.Context, recipientID string) error {
	_, err := s.client.Message.Update().
		Where(message.RecipientIDEQ(recipientID)).
		SetRead(true).
		Save(ctx)
	return err
}

// PurgeOldMessages removes read messages older than readCutoff and unread
// messages older than unreadCutoff. Returns the number of messages removed.
func (s *MessageStore) PurgeOldMessages(ctx context.Context, readCutoff time.Time, unreadCutoff time.Time) (int, error) {
	n, err := s.client.Message.Delete().
		Where(message.Or(
			message.And(message.ReadEQ(true), message.CreatedLT(readCutoff)),
			message.And(message.ReadEQ(false), message.CreatedLT(unreadCutoff)),
		)).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return n, nil
}
