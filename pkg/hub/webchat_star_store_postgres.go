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
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Postgres twin of the starred-message store in webchat_star_store.go.
// messages is Ent-managed (id and conversation_id are uuid columns), so it
// is always qualified with the store's schema (pgWebChatStore.table) and
// star message IDs are cast to uuid when joined to it.

const pgMessageStarDDL = `
CREATE TABLE IF NOT EXISTS webchat_message_star (
    user_id          TEXT NOT NULL,
    message_id       TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    project_id       TEXT,
    starred_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, message_id)
);

CREATE INDEX IF NOT EXISTS idx_webchat_message_star_user_starred
    ON webchat_message_star (user_id, starred_at DESC, message_id DESC);

CREATE INDEX IF NOT EXISTS idx_webchat_message_star_user_conversation
    ON webchat_message_star (user_id, conversation_key, starred_at DESC, message_id DESC);

CREATE INDEX IF NOT EXISTS idx_webchat_message_star_user_project
    ON webchat_message_star (user_id, project_id, starred_at DESC, message_id DESC);

CREATE INDEX IF NOT EXISTS idx_webchat_message_star_message
    ON webchat_message_star (message_id);
`

// initMessageStars creates webchat_message_star and its indexes
// (idempotent) and records the migration after all DDL succeeded. Like
// initScheduledMessages it runs on every replica's Init.
func (s *pgWebChatStore) initMessageStars() error {
	if _, err := s.db.Exec(pgMessageStarDDL); err != nil {
		return fmt.Errorf("webchat store: create message star table: %w", err)
	}
	if err := s.markMigrationCompleted(messageStarTableMigration); err != nil {
		return fmt.Errorf("webchat store: record message star migration: %w", err)
	}
	return nil
}

// pgArgs builds positional placeholders.
type pgArgs struct{ vals []any }

func (a *pgArgs) add(v any) string {
	a.vals = append(a.vals, v)
	return fmt.Sprintf("$%d", len(a.vals))
}

// pgStarSelectorSQL returns the predicate (on messages m) for sel.
func pgStarSelectorSQL(a *pgArgs, sel StarSelector) string {
	ch := a.add(sel.Channel)
	cond := "(" + ch + "::text = '' OR m.channel = " + ch + "::text)"
	if sel.ConversationID != "" {
		cond += " AND m.conversation_id = " + a.add(strings.ToLower(sel.ConversationID)) + "::uuid"
	} else {
		cond += " AND m.thread_id = " + a.add(sel.ThreadID) + "::text"
	}
	return cond
}

// pgStarTargetGuard is the add check of sqliteStarTargetGuard.
func pgStarTargetGuard(a *pgArgs, target StarTarget) string {
	cond := "m.id = " + a.add(target.MessageID) + "::uuid AND " + pgStarSelectorSQL(a, target.Selector)
	key := a.add(target.Selector.ConversationKey)
	cond += `
   AND NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = m.id::text AND e.deleted_at IS NOT NULL)
   AND (left(` + key + `::text, 3) = 'dm:' OR EXISTS (SELECT 1 FROM webchat_topic t WHERE t.id = ` + key + `::text AND t.deleted_at IS NULL))`
	return cond
}

func (s *pgWebChatStore) AddMessageStar(ctx context.Context, userID string, target StarTarget) (StarRecord, bool, error) {
	if userID == "" || target.MessageID == "" || target.Selector.ConversationKey == "" {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", store.ErrInvalidInput)
	}
	messagesTable, err := s.table(ctx, "messages")
	if err != nil {
		return StarRecord{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the message row before reading its extension or writing the
	// association, so a concurrent deletion or move of the same message
	// is serialized with this add; the checks below run after the lock,
	// in new statements, and see the committed state.
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+messagesTable+` WHERE id = $1::uuid FOR UPDATE`, target.MessageID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", store.ErrNotFound)
	}
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: lock message: %w", err)
	}

	ins := &pgArgs{}
	user := ins.add(userID)
	key := ins.add(target.Selector.ConversationKey)
	project := ins.add(nullableString(target.ProjectID))
	at := ins.add(starNow())
	guard := pgStarTargetGuard(ins, target)
	res, err := tx.ExecContext(ctx, `
INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at)
SELECT `+user+`::text, m.id::text, `+key+`::text, `+project+`::text, `+at+`::timestamptz
  FROM `+messagesTable+` m
 WHERE `+guard+`
ON CONFLICT (user_id, message_id) DO NOTHING`, ins.vals...)
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", err)
	}
	if n == 0 {
		chk := &pgArgs{}
		cond := pgStarTargetGuard(chk, target)
		var ok int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+messagesTable+` m WHERE `+cond, chk.vals...).Scan(&ok); err != nil {
			return StarRecord{}, false, fmt.Errorf("webchat store: add message star: check target: %w", err)
		}
		if ok == 0 {
			return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", store.ErrNotFound)
		}
	}
	rec, err := scanPGStar(tx.QueryRowContext(ctx, `
SELECT user_id, message_id, conversation_key, COALESCE(project_id, ''), starred_at
  FROM webchat_message_star WHERE user_id = $1 AND message_id = $2`, userID, target.MessageID))
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: read back: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", err)
	}
	return *rec, n == 1, nil
}

func scanPGStar(row interface{ Scan(...any) error }) (*StarRecord, error) {
	var rec StarRecord
	var starredAt time.Time
	if err := row.Scan(&rec.UserID, &rec.MessageID, &rec.ConversationKey, &rec.ProjectID, &starredAt); err != nil {
		return nil, err
	}
	rec.StarredAt = starredAt.UTC().Truncate(starTimePrecision)
	return &rec, nil
}

func (s *pgWebChatStore) RemoveMessageStar(ctx context.Context, userID, messageID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webchat_message_star WHERE user_id = $1 AND message_id = $2`, userID, messageID)
	if err != nil {
		return false, fmt.Errorf("webchat store: remove message star: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("webchat store: remove message star: %w", err)
	}
	return n > 0, nil
}

func (s *pgWebChatStore) GetMessageStar(ctx context.Context, userID, messageID string) (*StarRecord, error) {
	rec, err := scanPGStar(s.db.QueryRowContext(ctx, `
SELECT user_id, message_id, conversation_key, COALESCE(project_id, ''), starred_at
  FROM webchat_message_star WHERE user_id = $1 AND message_id = $2`, userID, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("webchat store: get message star: %w", err)
	}
	return rec, nil
}

func (s *pgWebChatStore) ListMessageStarCandidates(ctx context.Context, f StarFilter) (StarCandidatePage, error) {
	if f.UserID == "" || f.Selector.ConversationKey == "" || f.Limit < 1 {
		return StarCandidatePage{}, fmt.Errorf("webchat store: list message stars: %w", store.ErrInvalidInput)
	}
	messagesTable, err := s.table(ctx, "messages")
	if err != nil {
		return StarCandidatePage{}, err
	}
	a := &pgArgs{}
	conds := []string{
		"st.user_id = " + a.add(f.UserID),
		"st.conversation_key = " + a.add(f.Selector.ConversationKey),
		pgStarSelectorSQL(a, f.Selector),
		"NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)",
	}
	if f.After != nil {
		at := a.add(f.After.StarredAt.UTC().Truncate(starTimePrecision))
		id := a.add(f.After.MessageID)
		conds = append(conds, "(st.starred_at < "+at+"::timestamptz OR (st.starred_at = "+at+"::timestamptz AND st.message_id < "+id+"::text))")
	}
	limit := a.add(f.Limit + 1)
	rows, err := s.db.QueryContext(ctx, `
SELECT st.user_id, st.message_id, st.conversation_key, COALESCE(st.project_id, ''), st.starred_at
  FROM webchat_message_star st
  JOIN `+messagesTable+` m ON m.id = st.message_id::uuid
 WHERE `+strings.Join(conds, " AND ")+`
 ORDER BY st.starred_at DESC, st.message_id DESC
 LIMIT `+limit, a.vals...)
	if err != nil {
		return StarCandidatePage{}, fmt.Errorf("webchat store: list message stars: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := StarCandidatePage{Items: []StarRecord{}}
	for rows.Next() {
		rec, err := scanPGStar(rows)
		if err != nil {
			return StarCandidatePage{}, fmt.Errorf("webchat store: list message stars: %w", err)
		}
		page.Items = append(page.Items, *rec)
	}
	if err := rows.Err(); err != nil {
		return StarCandidatePage{}, fmt.Errorf("webchat store: list message stars: %w", err)
	}
	return trimStarPage(page, f.Limit), nil
}

func (s *pgWebChatStore) ListStarredMessageIDs(ctx context.Context, userID string, messageIDs []string) ([]StarredMessageRef, error) {
	out := []StarredMessageRef{}
	if userID == "" || len(messageIDs) == 0 {
		return out, nil
	}
	messagesTable, err := s.table(ctx, "messages")
	if err != nil {
		return nil, err
	}
	a := &pgArgs{}
	user := a.add(userID)
	placeholders := make([]string, len(messageIDs))
	for i, id := range messageIDs {
		placeholders[i] = a.add(id)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT st.message_id, st.conversation_key, COALESCE(st.project_id, ''),
       COALESCE(m.channel, ''), COALESCE(m.thread_id, ''), COALESCE(m.conversation_id::text, '')
  FROM webchat_message_star st
  JOIN `+messagesTable+` m ON m.id = st.message_id::uuid
 WHERE st.user_id = `+user+` AND st.message_id IN (`+strings.Join(placeholders, ",")+`)
   AND NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)
 ORDER BY st.message_id`, a.vals...)
	if err != nil {
		return nil, fmt.Errorf("webchat store: list starred message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref StarredMessageRef
		if err := rows.Scan(&ref.MessageID, &ref.ConversationKey, &ref.ProjectID,
			&ref.Channel, &ref.ThreadID, &ref.ConversationID); err != nil {
			return nil, fmt.Errorf("webchat store: list starred message ids: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webchat store: list starred message ids: %w", err)
	}
	return out, nil
}
