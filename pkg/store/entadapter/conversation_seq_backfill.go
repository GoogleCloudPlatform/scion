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
// numbers to messages whose conversation_seq is NULL, in (created, id) order,
// and advances conversations.last_message_seq to the new maximum (ptone/scion#8136).
//
// The migration is idempotent and touches only NULL rows, so it is a fast no-op
// once complete and also repairs rows inserted by an older binary after a
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

		for _, convID := range convIDs {
			n, err := backfillOneConversationSeq(ctx, db, isPostgres, convID)
			if err != nil {
				return fmt.Errorf("backfill conversation %s seqs: %w", convID, err)
			}
			totalUpdated += n
		}
	}

	if totalUpdated > 0 {
		slog.Info("backfilled messages.conversation_seq", "rows_updated", totalUpdated)
	}
	return nil
}

func listConversationsNeedingSeqBackfill(ctx context.Context, db *sql.DB, limit int) ([]uuid.UUID, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT DISTINCT m.conversation_id
		FROM messages m
		INNER JOIN conversations c ON c.id = m.conversation_id
		WHERE m.conversation_id IS NOT NULL
		  AND m.conversation_seq IS NULL
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

	ph := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	// Lock the conversation row (FOR UPDATE on Postgres) and read its current
	// last_message_seq so concurrent writers and backfills serialize in the
	// same lock order as CreateMessage.
	lockQuery := "SELECT last_message_seq FROM conversations WHERE id = " + ph(1)
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
	maxQuery := "SELECT MAX(conversation_seq) FROM messages WHERE conversation_id = " + ph(1)
	if err := tx.QueryRowContext(ctx, maxQuery, convID).Scan(&maxExisting); err != nil {
		return 0, err
	}
	base := lastSeq
	if maxExisting.Valid && maxExisting.Int64 > base {
		base = maxExisting.Int64
	}

	// Assign base + ROW_NUMBER() OVER (PARTITION BY conversation_id ORDER BY created ASC, id ASC)
	// to all NULL-seq rows in this conversation. Supported on SQLite ≥ 3.33 and Postgres.
	updateMsgs := fmt.Sprintf(`
		UPDATE messages
		SET conversation_seq = sub.seq
		FROM (
			SELECT id, %s + ROW_NUMBER() OVER (
				PARTITION BY conversation_id
				ORDER BY created ASC, id ASC
			) AS seq
			FROM messages
			WHERE conversation_id = %s
			  AND conversation_seq IS NULL
		) AS sub
		WHERE messages.id = sub.id
	`, ph(1), ph(2))

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
		updateConv := fmt.Sprintf("UPDATE conversations SET last_message_seq = %s WHERE id = %s", ph(1), ph(2))
		if _, err := tx.ExecContext(ctx, updateConv, newHead, convID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}
