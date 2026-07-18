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
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := clampLimit(opts.Limit)
	entities, err := query.
		Order(message.ByCreated(entsql.OrderDesc())).
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
		result.NextCursor = msgs[limit-1].ID
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
