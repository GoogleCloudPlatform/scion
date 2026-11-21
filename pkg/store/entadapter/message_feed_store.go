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
	"database/sql"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/messageaddressee"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	defaultConversationFeedLimit = 50
	maxConversationFeedLimit     = 200
)

// ConversationHeadSeq returns the current last_message_seq for conversationID.
// Returns store.ErrNotFound if the conversation does not exist.
func (s *MessageStore) ConversationHeadSeq(ctx context.Context, conversationID string) (int64, error) {
	cid, err := parseGetID(conversationID)
	if err != nil {
		return 0, err
	}
	conv, err := s.client.Conversation.Query().
		Where(conversation.IDEQ(cid)).
		Select(conversation.FieldID, conversation.FieldLastMessageSeq).
		Only(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return conv.LastMessageSeq, nil
}

// MessageConversationSeq returns the conversation_seq of messageID within
// conversationID. Returns store.ErrNotFound if the message does not exist in
// that conversation or has no assigned sequence.
func (s *MessageStore) MessageConversationSeq(ctx context.Context, conversationID, messageID string) (int64, error) {
	cid, err := parseGetID(conversationID)
	if err != nil {
		return 0, err
	}
	mid, err := parseGetID(messageID)
	if err != nil {
		return 0, err
	}
	msg, err := s.client.Message.Query().
		Where(
			message.IDEQ(mid),
			message.ConversationIDEQ(cid),
		).
		Select(message.FieldID, message.FieldConversationSeq).
		Only(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	if msg.ConversationSeq == nil {
		return 0, store.ErrNotFound
	}
	return *msg.ConversationSeq, nil
}

// ListConversationFeed returns messages in a conversation with
// conversation_seq in (q.AfterSeq, head], ordered ascending by
// conversation_seq, filtered in SQL according to q.Filter (ptone/scion#8136).
func (s *MessageStore) ListConversationFeed(ctx context.Context, q store.ConversationFeedQuery) (store.ConversationFeedPage, error) {
	cid, err := parseGetID(q.ConversationID)
	if err != nil {
		return store.ConversationFeedPage{}, err
	}

	filter := q.Filter
	if filter == "" {
		filter = store.ConversationFeedFilterOthers
	}
	switch filter {
	case store.ConversationFeedFilterOthers, store.ConversationFeedFilterAll, store.ConversationFeedFilterAddressed:
	default:
		return store.ConversationFeedPage{}, store.ErrInvalidInput
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultConversationFeedLimit
	} else if limit > maxConversationFeedLimit {
		limit = maxConversationFeedLimit
	}

	head, err := s.ConversationHeadSeq(ctx, q.ConversationID)
	if err != nil {
		return store.ConversationFeedPage{}, err
	}
	if head <= q.AfterSeq {
		return store.ConversationFeedPage{
			Messages:  []store.Message{},
			ScannedTo: q.AfterSeq,
		}, nil
	}

	// Gap detection (design §3.2): if rows were deleted between AfterSeq and
	// the first surviving seq > AfterSeq (or all rows in (AfterSeq, head] were
	// deleted), report Truncated = true.
	var truncated bool
	if q.AfterSeq > 0 {
		minSeq, valid, minErr := s.minConversationSeqInRange(ctx, cid, q.AfterSeq, head)
		if minErr != nil {
			return store.ConversationFeedPage{}, minErr
		}
		if !valid || minSeq > q.AfterSeq+1 {
			truncated = true
		}
	}

	// FilterAddressed with no principal cannot match anything; advance cursor
	// cleanly to head.
	if filter == store.ConversationFeedFilterAddressed && (q.PrincipalKind == "" || q.PrincipalID == "") {
		return store.ConversationFeedPage{
			Messages:  []store.Message{},
			ScannedTo: head,
			Truncated: truncated,
		}, nil
	}

	query := s.client.Message.Query().
		Where(
			message.ConversationIDEQ(cid),
			message.ConversationSeqGT(q.AfterSeq),
			message.ConversationSeqLTE(head),
		)

	switch filter {
	case store.ConversationFeedFilterOthers:
		if q.PrincipalKind != "" && q.PrincipalID != "" {
			query = query.Where(feedNotFromPrincipal(q.PrincipalKind, q.PrincipalID))
		}
	case store.ConversationFeedFilterAddressed:
		query = query.Where(
			feedNotFromPrincipal(q.PrincipalKind, q.PrincipalID),
			feedAddressedToPrincipal(q.PrincipalKind, q.PrincipalID),
		)
	case store.ConversationFeedFilterAll:
		// no additional filter
	}

	entities, err := query.
		Order(ent.Asc(message.FieldConversationSeq)).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return store.ConversationFeedPage{}, mapError(err)
	}

	hasMore := len(entities) > limit
	if hasMore {
		entities = entities[:limit]
	}

	msgs := make([]store.Message, 0, len(entities))
	for _, e := range entities {
		msgs = append(msgs, *entMessageToStore(e))
	}

	scannedTo := head
	if hasMore && len(msgs) > 0 {
		scannedTo = msgs[len(msgs)-1].ConversationSeq
	}

	return store.ConversationFeedPage{
		Messages:  msgs,
		ScannedTo: scannedTo,
		HasMore:   hasMore,
		Truncated: truncated,
	}, nil
}

func (s *MessageStore) minConversationSeqInRange(ctx context.Context, cid uuid.UUID, afterSeq, headSeq int64) (int64, bool, error) {
	drv := s.client.Driver()
	var query string
	if drv.Dialect() == dialect.Postgres {
		query = "SELECT MIN(" + message.FieldConversationSeq + ") FROM " + message.Table +
			" WHERE " + message.FieldConversationID + " = $1 AND " + message.FieldConversationSeq + " > $2 AND " + message.FieldConversationSeq + " <= $3"
	} else {
		query = "SELECT MIN(" + message.FieldConversationSeq + ") FROM " + message.Table +
			" WHERE " + message.FieldConversationID + " = ? AND " + message.FieldConversationSeq + " > ? AND " + message.FieldConversationSeq + " <= ?"
	}
	var rows entsql.Rows
	if err := drv.Query(ctx, query, []any{cid, afterSeq, headSeq}, &rows); err != nil {
		return 0, false, mapError(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, false, mapError(err)
		}
		return 0, false, nil
	}
	var minSeq sql.NullInt64
	if err := rows.Scan(&minSeq); err != nil {
		return 0, false, mapError(err)
	}
	return minSeq.Int64, minSeq.Valid, nil
}

// feedNotFromPrincipal excludes messages sent by (kind, id), guarding against
// colliding user/agent UUID namespaces by checking both sender_id and the
// "<kind>:" prefix on sender.
func feedNotFromPrincipal(kind, id string) func(*entsql.Selector) {
	prefix := kind + ":%"
	return func(sel *entsql.Selector) {
		sel.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("NOT (COALESCE(").
				WriteString(sel.C(message.FieldSenderID)).
				WriteString(", '') = ").Arg(id).
				WriteString(" AND ").
				WriteString(sel.C(message.FieldSender)).
				WriteString(" LIKE ").Arg(prefix).
				WriteString(")")
		}))
	}
}

// feedAddressedToPrincipal matches messages where (kind, id) is the direct
// recipient (matching both recipient_id and "<kind>:" prefix on recipient) or
// appears in message_addressees with matching (principal_kind, principal_id).
func feedAddressedToPrincipal(kind, id string) func(*entsql.Selector) {
	prefix := kind + ":%"
	return func(sel *entsql.Selector) {
		sel.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("((COALESCE(").
				WriteString(sel.C(message.FieldRecipientID)).
				WriteString(", '') = ").Arg(id).
				WriteString(" AND ").
				WriteString(sel.C(message.FieldRecipient)).
				WriteString(" LIKE ").Arg(prefix).
				WriteString(") OR EXISTS (SELECT 1 FROM ").
				WriteString(messageaddressee.Table).
				WriteString(" a WHERE a.").
				WriteString(messageaddressee.FieldMessageID).
				WriteString(" = ").
				WriteString(sel.C(message.FieldID)).
				WriteString(" AND a.").
				WriteString(messageaddressee.FieldPrincipalKind).
				WriteString(" = ").Arg(kind).
				WriteString(" AND a.").
				WriteString(messageaddressee.FieldPrincipalID).
				WriteString(" = ").Arg(id).
				WriteString("))")
		}))
	}
}
