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

	"entgo.io/ent/dialect"
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
	var conversationID string
	if e.ConversationID != nil {
		conversationID = e.ConversationID.String()
	}
	senderProjectID := uuidPtrToStringPtr(e.SenderProjectID)
	recipientProjectID := uuidPtrToStringPtr(e.RecipientProjectID)

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
		ConversationID:        conversationID,
		CreatedAt:             e.Created,
		SenderProjectID:       senderProjectID,
		RecipientProjectID:    recipientProjectID,
		DispatchState:         e.DispatchState,
		DispatchedAt:          e.DispatchedAt,
		DispatchFailureReason: e.DispatchFailureReason,
	}
}

// uuidPtrToStringPtr converts a nullable UUID column to the nullable string
// form used by store models.
func uuidPtrToStringPtr(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	v := u.String()
	return &v
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
	if msg.ConversationID != "" {
		cid, err := parseUUID(msg.ConversationID)
		if err != nil {
			return err
		}
		create.SetConversationID(cid)
	}
	// Cross-project provenance (ptone/scion#2282). An absent or empty stamp
	// persists as NULL; a malformed one is rejected like any other bad ID.
	if msg.SenderProjectID != nil && *msg.SenderProjectID != "" {
		spid, err := parseUUID(*msg.SenderProjectID)
		if err != nil {
			return err
		}
		create.SetSenderProjectID(spid)
	}
	if msg.RecipientProjectID != nil && *msg.RecipientProjectID != "" {
		rpid, err := parseUUID(*msg.RecipientProjectID)
		if err != nil {
			return err
		}
		create.SetRecipientProjectID(rpid)
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
	// A row may be born failed (e.g. a group member that is not
	// deliverable), so its reason must persist with the same write.
	if msg.DispatchFailureReason != nil {
		create.SetDispatchFailureReason(*msg.DispatchFailureReason)
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

// GetMessagesByIDs retrieves messages by a list of IDs.
// Returns only messages that exist; missing IDs are silently skipped.
func (s *MessageStore) GetMessagesByIDs(ctx context.Context, ids []string) (map[string]*store.Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	uuids := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		uid, err := parseUUID(id)
		if err != nil {
			continue // skip invalid UUIDs
		}
		uuids = append(uuids, uid)
	}

	if len(uuids) == 0 {
		return nil, nil
	}

	msgs, err := s.client.Message.Query().
		Where(message.IDIn(uuids...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make(map[string]*store.Message, len(msgs))
	for _, m := range msgs {
		sm := entMessageToStore(m)
		result[sm.ID] = sm
	}

	return result, nil
}

// encodeCursor produces a self-contained, opaque pagination cursor that embeds
// both the created timestamp and the message ID. Format: base64(RFC3339Nano + "," + uuid).
// This avoids a DB round-trip on decode and makes pagination resilient to message deletion.
func encodeCursor(created time.Time, id string) string {
	raw := created.UTC().Format(time.RFC3339Nano) + "," + id
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor is the inverse of encodeCursor. It returns the created timestamp and UUID
// embedded in the cursor, or an error if the cursor is malformed.
func decodeCursor(cursor string) (time.Time, uuid.UUID, error) {
	// Every failure wraps store.ErrInvalidInput: a malformed cursor is caller
	// error, which the hub maps to HTTP 400 rather than 500.
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: base64 decode: %w", store.ErrInvalidInput, err)
	}
	parts := strings.SplitN(string(raw), ",", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: expected 'timestamp,id' format", store.ErrInvalidInput)
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: parse timestamp: %w", store.ErrInvalidInput, err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: parse id: %w", store.ErrInvalidInput, err)
	}
	return ts, id, nil
}

// encodeListCursor extends the ordinary keyset cursor with an optional binding.
// A binding is supplied by endpoint callers that must reject cursors reused
// across different resources or filters.
func encodeListCursor(created time.Time, id, binding string) string {
	raw := created.UTC().Format(time.RFC3339Nano) + "," + id
	if binding != "" {
		raw += "," + binding
	}
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

func decodeListCursor(cursor, binding string) (time.Time, uuid.UUID, error) {
	// Every failure wraps store.ErrInvalidInput: a malformed or mismatched
	// cursor is caller error, which the hub maps to HTTP 400 rather than 500.
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: base64 decode: %w", store.ErrInvalidInput, err)
	}
	parts := strings.SplitN(string(raw), ",", 3)
	if len(parts) < 2 || (binding == "" && len(parts) != 2) || (binding != "" && (len(parts) != 3 || parts[2] != binding)) {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: cursor does not match this list", store.ErrInvalidInput)
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: parse timestamp: %w", store.ErrInvalidInput, err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: parse id: %w", store.ErrInvalidInput, err)
	}
	return ts, id, nil
}

// messageCreatedAfter returns the predicate "created is strictly after t".
//
// On SQLite created holds Go's default time text, and a row written before
// the UTC store boundary existed may still carry a monotonic-clock suffix
// (" m=+0.0336"), while t (bound by timeArg) never does. A raw text
// comparison then puts a suffixed row at exactly t after t, so the chat
// "around" read (After = the anchor's read-back CreatedAt) returned the
// anchor and its same-instant neighbours as newer (ptone/scion#2553). The
// predicate is therefore
//
//	created > t AND timeColumnExpr(created) > t
//
// The raw term keeps the range seek on the (…, created, id) indexes, and it
// is implied by the normalized term (the normalized text is a prefix of the
// stored text), so it never excludes a row the normalized term keeps. The
// normalized term drops the same-instant suffixed rows. Both assume UTC text,
// which the store boundary writes and the utc-timestamp-normalize maintenance
// operation establishes for legacy rows.
//
// Before needs no such treatment: a suffixed row at exactly t already sorts
// after t's text, so the raw created < t excludes it.
//
// On Postgres created is timestamptz and this is a plain comparison.
func messageCreatedAfter(t time.Time) predicate.Message {
	return func(s *entsql.Selector) {
		if s.Dialect() == dialect.Postgres {
			message.CreatedGT(t)(s)
			return
		}
		arg := timeArg(s.Dialect(), t)
		raw := s.C(message.FieldCreated)
		norm := timeColumnExpr(s, message.FieldCreated)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString(raw).WriteString(" > ").Arg(arg).
				WriteString(" AND ").WriteString(norm).WriteString(" > ").Arg(arg)
		}))
	}
}

// ListMessages returns messages matching the given filter, ordered by
// created_at descending unless opts.SortDir is "asc".
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
	if filter.Sender != "" {
		query.Where(message.SenderEQ(filter.Sender))
	}
	if filter.OnlyUnread {
		query.Where(message.ReadEQ(false))
	}
	if filter.Type != "" {
		query.Where(message.TypeEQ(filter.Type))
	}
	if filter.ExcludeType != "" {
		query.Where(message.TypeNEQ(filter.ExcludeType))
	}
	if filter.Channel != "" {
		query.Where(message.ChannelEQ(filter.Channel))
	}
	if filter.ThreadID != "" {
		query.Where(message.ThreadIDEQ(filter.ThreadID))
	}
	if filter.ConversationID != "" {
		cid, err := parseUUID(filter.ConversationID)
		if err != nil {
			return nil, fmt.Errorf("invalid conversation_id filter: %w", err)
		}
		query.Where(message.ConversationIDEQ(cid))
	}
	if !filter.Before.IsZero() {
		query.Where(message.CreatedLT(filter.Before))
	}
	if !filter.After.IsZero() {
		query.Where(messageCreatedAfter(filter.After))
	}

	// totalCount represents the total number of messages matching the base
	// filter (before cursor pagination is applied). Clone and count before
	// adding the cursor predicate so the count stays stable across pages.
	var totalCount int
	if !opts.SkipTotalCount {
		var err error
		totalCount, err = query.Clone().Count(ctx)
		if err != nil {
			return nil, err
		}
	}

	ascending := strings.EqualFold(opts.SortDir, "asc")

	// Apply cursor-based keyset pagination.
	// The cursor is a self-contained base64-encoded string carrying (created, id).
	// This avoids a DB round-trip and makes pagination resilient to message deletion.
	if opts.Cursor != "" {
		cursorCreated, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		if ascending {
			query.Where(message.Or(
				message.CreatedGT(cursorCreated),
				message.And(
					message.CreatedEQ(cursorCreated),
					message.IDGT(cursorID),
				),
			))
		} else {
			query.Where(message.Or(
				message.CreatedLT(cursorCreated),
				message.And(
					message.CreatedEQ(cursorCreated),
					message.IDLT(cursorID),
				),
			))
		}
	}

	// The keyset cursor and the ORDER BY use the raw created column so the
	// (conversation_id, channel, created, id) and (thread_id, channel,
	// created, id) indexes serve both the seek and the order, with no sort.
	// On SQLite a legacy row whose stored text still carries a
	// monotonic-clock suffix can therefore order after a suffix-free row at
	// the same instant, and a page boundary at such a tie can repeat or skip
	// it. That ordering is left to the utc-timestamp-normalize maintenance
	// operation, which rewrites legacy rows to canonical text, rather than
	// paid for with a sort on every page (ptone/scion#2553).
	limit := clampLimit(opts.Limit)
	createdOrder := entsql.OrderDesc()
	idOrder := entsql.OrderDesc()
	if ascending {
		createdOrder = entsql.OrderAsc()
		idOrder = entsql.OrderAsc()
	}
	entities, err := query.
		Order(message.ByCreated(createdOrder)).
		Order(message.ByID(idOrder)).
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

// PurgeFailedMessages removes messages with dispatch_state="failed" whose
// created timestamp is before cutoff. Returns the number of messages removed.
// Unlike PurgeOldMessages, this filters strictly on dispatch_state so
// successfully delivered (dispatched) message history is never touched.
// Scoped to agent recipients only (message.RecipientHasPrefix "agent:"):
// only a message addressed to an agent can have genuinely and irrecoverably
// failed dispatch. A "user:" recipient row reaching "failed" only ever came
// from ExpireStuckPendingMessages sweeping a writer bug (nc-promote-busy);
// deleting it would destroy real chat history the user never saw fail.
func (s *MessageStore) PurgeFailedMessages(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := s.client.Message.Delete().
		Where(
			message.DispatchStateEQ(store.MessageDispatchFailed),
			message.CreatedLT(cutoff),
			message.RecipientHasPrefix("agent:"),
		).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// SetMessageConversationID updates the conversation_id on an existing message.
// Used by Phase 4 backfill to link legacy messages to Conversation records.
func (s *MessageStore) SetMessageConversationID(ctx context.Context, messageID, conversationID string) error {
	mid, err := parseUUID(messageID)
	if err != nil {
		return err
	}
	cid, err := parseUUID(conversationID)
	if err != nil {
		return err
	}
	n, err := s.client.Message.Update().
		Where(message.IDEQ(mid)).
		SetConversationID(cid).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CountUnbackfilledMessages returns the number of messages with a NULL
// conversation_id. When projectID is non-empty the count is scoped to that
// project; otherwise it counts across all projects.
func (s *MessageStore) CountUnbackfilledMessages(ctx context.Context, projectID string) (int, error) {
	query := s.client.Message.Query().Where(message.ConversationIDIsNil())
	if projectID != "" {
		pid, err := parseUUID(projectID)
		if err != nil {
			return 0, err
		}
		query.Where(message.ProjectIDEQ(pid))
	}
	count, err := query.Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return count, nil
}

// CountUnreachableUnbackfilledMessages returns the number of messages with
// a NULL conversation_id whose project_id does not reference an existing
// project row. These are permanently unattributable by the per-project
// backfill because ListProjects never returns their project (DEF-111).
//
// The predicate here — "unbackfilled AND project_id NOT IN (SELECT id FROM
// projects)" — is the inverse of what the backfill can reach.
//
// DEPENDENCY: this count is correct only because ListProjects (with an
// empty ProjectFilter) applies no unconditional filter — no soft-delete,
// no archived exclusion — so it returns every row in the projects table,
// and NOT EXISTS is its exact complement. If ListProjects ever adds an
// unconditional filter, this counter would undercount the unreachable
// population (some messages whose projects are filtered out of ListProjects
// would be classified as reachable when the backfill cannot reach them),
// relocating the alarm-fatigue bug rather than fixing it.
//
// GATE (M7, DEF-112): TestReachableCountConsistency_DEF112 enforces this
// invariant. TestUnreachableCounterTableNames guards the raw SQL identifiers
// against Ent schema renames.
func (s *MessageStore) CountUnreachableUnbackfilledMessages(ctx context.Context) (int, error) {
	count, err := s.client.Message.Query().
		Where(
			message.ConversationIDIsNil(),
			func(sel *entsql.Selector) {
				sel.Where(entsql.P(func(b *entsql.Builder) {
					b.WriteString("NOT EXISTS (SELECT 1 FROM projects WHERE projects.id = ").
						WriteString(sel.C(message.FieldProjectID)).
						WriteString(")")
				}))
			},
		).
		Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return count, nil
}
