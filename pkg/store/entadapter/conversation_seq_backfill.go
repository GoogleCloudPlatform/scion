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
	"fmt"
	"log/slog"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"
)

const conversationSeqBackfillBatchSize = 100

// BackfillConversationMessageSeqs assigns monotonic per-conversation sequence
// numbers to messages whose conversation_seq is NULL or duplicated within a
// conversation, in (created, id) order, and advances
// conversations.last_message_seq to the new maximum (ptone/scion#8136).
//
// The migration is idempotent and touches only NULL and duplicate-seq rows, so
// it is a fast no-op once complete and also repairs rows inserted or re-stamped
// (e.g. via PromoteDM / SetMessageConversationID) by an older binary after a
// downgrade.
func (c *CompositeStore) BackfillConversationMessageSeqs(ctx context.Context) error {
	db := c.DB()
	if db == nil {
		return nil
	}

	isPostgres := c.Dialect() == dialect.Postgres
	var totalUpdated int64

	for {
		convIDs, err := listConversationsNeedingSeqBackfill(ctx, db, conversationSeqBackfillBatchSize)
		if err != nil {
			return fmt.Errorf("list conversations needing seq backfill: %w", err)
		}
		if len(convIDs) == 0 {
			break
		}

		var batchUpdated int64
		for _, convID := range convIDs {
			n, err := backfillOneConversationSeq(ctx, db, isPostgres, convID)
			if err != nil {
				return fmt.Errorf("backfill conversation %s seqs: %w", convID, err)
			}
			batchUpdated += n
		}
		if batchUpdated == 0 {
			idStrs := make([]string, len(convIDs))
			for i, id := range convIDs {
				idStrs[i] = id.String()
			}
			slog.WarnContext(ctx, "conversation_seq backfill batch made no progress; stopping to avoid infinite loop",
				"conversation_count", len(convIDs),
				"conversation_ids", idStrs)
			break
		}
		totalUpdated += batchUpdated
	}

	if totalUpdated > 0 {
		slog.Info("backfilled messages.conversation_seq", "rows_updated", totalUpdated)
	}
	return nil
}

func listConversationsNeedingSeqBackfill(ctx context.Context, db *sql.DB, limit int) ([]uuid.UUID, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT conv_id FROM (
			SELECT m.conversation_id AS conv_id
			FROM messages m
			INNER JOIN conversations c ON c.id = m.conversation_id
			WHERE m.conversation_id IS NOT NULL
			  AND m.conversation_seq IS NULL
			UNION
			SELECT m.conversation_id AS conv_id
			FROM messages m
			INNER JOIN conversations c ON c.id = m.conversation_id
			WHERE m.conversation_id IS NOT NULL
			  AND m.conversation_seq IS NOT NULL
			GROUP BY m.conversation_id, m.conversation_seq
			HAVING COUNT(*) > 1
		) AS needing
		LIMIT %d
	`, limit))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func backfillOneConversationSeq(ctx context.Context, db *sql.DB, isPostgres bool, convID uuid.UUID) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	updated, err := BackfillConversationSeqInTx(ctx, tx, isPostgres, "messages", "conversations", convID.String())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

// BackfillConversationSeqInTx locks the target conversation row inside tx,
// assigns base + ROW_NUMBER() OVER (ORDER BY created ASC, id ASC) to every
// message in convID whose conversation_seq IS NULL or duplicates an earlier
// message's conversation_seq in (created ASC, id ASC) order, and advances
// conversations.last_message_seq to the new maximum.
//
// Shared by BackfillConversationMessageSeqs and PromoteDM (both SQLite and
// Postgres) so all re-sequencing logic lives in one place (ptone/scion#8136).
// messagesTable and conversationsTable must be trusted SQL identifiers (either
// literals or schema-qualified table names returned by the store).
func BackfillConversationSeqInTx(
	ctx context.Context,
	tx *sql.Tx,
	isPostgres bool,
	messagesTable string,
	conversationsTable string,
	convID string,
) (int64, error) {
	ph := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	idExpr := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d::uuid", n)
		}
		return "?"
	}

	// Lock the conversation row (FOR UPDATE on Postgres) and read its current
	// last_message_seq so concurrent writers and re-sequencers serialize in the
	// same lock order as CreateMessage.
	lockQuery := "SELECT last_message_seq FROM " + conversationsTable + " WHERE id = " + idExpr(1)
	if isPostgres {
		lockQuery += " FOR UPDATE"
	}
	var lastSeq int64
	if err := tx.QueryRowContext(ctx, lockQuery, convID).Scan(&lastSeq); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}

	// Guard against last_message_seq lagging behind existing non-NULL seqs.
	var maxExisting sql.NullInt64
	maxQuery := "SELECT MAX(conversation_seq) FROM " + messagesTable + " WHERE conversation_id = " + idExpr(1)
	if err := tx.QueryRowContext(ctx, maxQuery, convID).Scan(&maxExisting); err != nil {
		return 0, err
	}
	base := lastSeq
	if maxExisting.Valid && maxExisting.Int64 > base {
		base = maxExisting.Int64
	}

	// Assign base + ROW_NUMBER() OVER (ORDER BY created ASC, id ASC) to all
	// NULL-seq rows and any duplicate-seq rows beyond the earliest row per
	// distinct non-NULL conversation_seq. Supported on SQLite >= 3.33 and Postgres.
	updateMsgs := fmt.Sprintf(`
		UPDATE %s
		SET conversation_seq = sub.seq
		FROM (
			SELECT id, %s + ROW_NUMBER() OVER (
				ORDER BY created ASC, id ASC
			) AS seq
			FROM (
				SELECT id, created, conversation_seq,
					ROW_NUMBER() OVER (
						PARTITION BY conversation_seq
						ORDER BY created ASC, id ASC
					) AS dup_rn
				FROM %s
				WHERE conversation_id = %s
			) AS ranked
			WHERE ranked.conversation_seq IS NULL
			   OR ranked.dup_rn > 1
		) AS sub
		WHERE %s.id = sub.id
	`, messagesTable, ph(1), messagesTable, idExpr(2), messagesTable)

	res, err := tx.ExecContext(ctx, updateMsgs, base, convID)
	if err != nil {
		return 0, err
	}
	updated, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}

	newHead := base + updated
	if newHead > lastSeq {
		updateConv := fmt.Sprintf("UPDATE %s SET last_message_seq = %s WHERE id = %s", conversationsTable, ph(1), idExpr(2))
		if _, err := tx.ExecContext(ctx, updateConv, newHead, convID); err != nil {
			return 0, err
		}
	}

	return updated, nil
}
