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
		// The message_id tie-break (here and in ORDER BY) compares TEXT
		// under the database collation, while SQLite compares bytes. The
		// two orders agree because message_id is always a canonical
		// lowercase UUID (hex digits and hyphens; the handlers canonicalize
		// every ID), and the shared store contract relies on that byte order.
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

func (s *pgWebChatStore) ListStarredHistory(ctx context.Context, q StarHistoryQuery) (StarHistoryPage, error) {
	if !validStarHistoryQuery(q) {
		return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: %w", store.ErrInvalidInput)
	}
	messagesTable, err := s.table(ctx, "messages")
	if err != nil {
		return StarHistoryPage{}, err
	}
	a := &pgArgs{}
	conds := []string{
		"st.user_id = " + a.add(q.UserID),
		"st.conversation_key = " + a.add(q.Selector.ConversationKey),
		pgStarSelectorSQL(a, q.Selector),
		"NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)",
	}
	from := `
  FROM webchat_message_star st
  JOIN ` + messagesTable + ` m ON m.id = st.message_id::uuid
 WHERE `

	page := StarHistoryPage{Items: []MessagePosition{}}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from+strings.Join(conds, " AND "), a.vals...).Scan(&page.Total); err != nil {
		return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: count: %w", err)
	}

	cmp, dir := "<", "DESC"
	if q.Ascending {
		cmp, dir = ">", "ASC"
	}
	if q.Cursor != nil {
		at := a.add(q.Cursor.Created.UTC())
		id := a.add(strings.ToLower(q.Cursor.ID))
		conds = append(conds, "(m.created "+cmp+" "+at+"::timestamptz OR (m.created = "+at+"::timestamptz AND m.id "+cmp+" "+id+"::uuid))")
	}
	limit := a.add(q.Limit + 1)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id::text, m.created`+from+strings.Join(conds, " AND ")+`
 ORDER BY m.created `+dir+`, m.id `+dir+`
 LIMIT `+limit, a.vals...)
	if err != nil {
		return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p MessagePosition
		if err := rows.Scan(&p.ID, &p.Created); err != nil {
			return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: %w", err)
		}
		p.Created = p.Created.UTC()
		page.Items = append(page.Items, p)
	}
	if err := rows.Err(); err != nil {
		return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: %w", err)
	}
	return trimStarHistoryPage(page, q.Limit), nil
}

// ---------------------------------------------------------------------------
// Postgres lifecycle
// ---------------------------------------------------------------------------

// pgUUIDText casts a TEXT expression to uuid only when it is a canonical
// UUID (NULL otherwise), so one malformed stored value cannot fail a
// statement over many rows; NULL never matches a uuid key.
func pgUUIDText(expr string) string {
	return `(CASE WHEN ` + expr + ` ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN (` + expr + `)::uuid END)`
}

// markMessageDeletedTx is the deletion transaction SetMessageDeleted and
// MarkMessageDeletedAndClearStars share. It first locks the messages row
// (as AddMessageStar and a promotion's re-key do), so a concurrent star
// add either commits first and its star is deleted here, or waits and
// then finds the message deleted; the owners are collected after the lock.
func (s *pgWebChatStore) markMessageDeletedTx(ctx context.Context, messageID string, deletedAt time.Time) ([]string, error) {
	const query = `
INSERT INTO webchat_message_ext (message_id, deleted_at)
VALUES ($1, $2)
ON CONFLICT (message_id)
DO UPDATE SET deleted_at = EXCLUDED.deleted_at
`
	messagesTable, err := s.table(ctx, "messages")
	if err != nil {
		return nil, err
	}
	// One transaction: the soft delete, its mention cleanup and its star
	// cleanup land together, so a deleted message never keeps lighting a
	// mention dot or a star.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("webchat store: set message deleted begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	// A message that is already gone (or an ID that is not a UUID, which
	// no message has) has no row to lock; the cleanup still runs.
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+messagesTable+` WHERE id = `+pgUUIDText("$1::text")+` FOR UPDATE`, messageID).Scan(&one)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("webchat store: set message deleted: lock message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, query, messageID, deletedAt); err != nil {
		return nil, fmt.Errorf("webchat store: set message deleted: %w", err)
	}
	// A deleted message no longer mentions anyone.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webchat_mention WHERE message_id = $1`, messageID); err != nil {
		return nil, fmt.Errorf("webchat store: delete message mentions: %w", err)
	}
	owners, err := queryStarOwners(ctx, tx,
		`WITH gone AS (DELETE FROM webchat_message_star WHERE message_id = $1 RETURNING user_id)
		 SELECT DISTINCT user_id FROM gone ORDER BY user_id`, messageID)
	if err != nil {
		return nil, fmt.Errorf("webchat store: delete message stars: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("webchat store: set message deleted: %w", err)
	}
	return owners, nil
}

func (s *pgWebChatStore) MarkMessageDeletedAndClearStars(ctx context.Context, messageID string, deletedAt time.Time) ([]string, error) {
	return s.markMessageDeletedTx(ctx, messageID, deletedAt)
}

func (s *pgWebChatStore) DeleteMessageStarsForConversation(ctx context.Context, conversationKey string) (int64, error) {
	return s.deleteStarsWhere(ctx, "conversation", `conversation_key = $1`, conversationKey)
}

func (s *pgWebChatStore) DeleteMessageStarsForUser(ctx context.Context, userID string) (int64, error) {
	return s.deleteStarsWhere(ctx, "user", `user_id = $1`, userID)
}

func (s *pgWebChatStore) DeleteMessageStarsForProject(ctx context.Context, projectID string) (int64, error) {
	return s.deleteStarsWhere(ctx, "project", `project_id = $1`, projectID)
}

func (s *pgWebChatStore) deleteStarsWhere(ctx context.Context, what, cond, value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("webchat store: delete message stars for %s: %w", what, store.ErrInvalidInput)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM webchat_message_star WHERE `+cond, value)
	if err != nil {
		return 0, fmt.Errorf("webchat store: delete message stars for %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("webchat store: delete message stars for %s: %w", what, err)
	}
	return n, nil
}

func (s *pgWebChatStore) PurgeMessageStarCandidates(ctx context.Context, after StarCleanupPosition, limit int) (StarCleanupResult, error) {
	if limit < 1 {
		return StarCleanupResult{}, fmt.Errorf("webchat store: purge message stars: %w", store.ErrInvalidInput)
	}
	messagesTable, err := s.table(ctx, "messages")
	if err != nil {
		return StarCleanupResult{}, err
	}
	usersTable, err := s.table(ctx, "users")
	if err != nil {
		return StarCleanupResult{}, err
	}
	projectsTable, err := s.table(ctx, "projects")
	if err != nil {
		return StarCleanupResult{}, err
	}
	// The (user_id, message_id) order compares TEXT under the database
	// collation; IDs are canonical lowercase UUIDs, so it matches the byte
	// order and the PK index (see ListMessageStarCandidates).
	rows, err := s.db.QueryContext(ctx, `
SELECT user_id, message_id FROM webchat_message_star
 WHERE user_id > $1 OR (user_id = $1 AND message_id > $2)
 ORDER BY user_id, message_id
 LIMIT $3`, after.UserID, after.MessageID, limit)
	if err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: purge message stars: %w", err)
	}
	first, last, n, err := starCleanupRange(rows)
	if err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: purge message stars: %w", err)
	}
	res := StarCleanupResult{Examined: n}
	if n == limit {
		res.Next = last
	}
	if n == 0 {
		return res, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: purge message stars: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const rng = `(st.user_id > $1 OR (st.user_id = $1 AND st.message_id >= $2))
   AND (st.user_id < $3 OR (st.user_id = $3 AND st.message_id <= $4))`
	rngArgs := []any{first.UserID, first.MessageID, last.UserID, last.MessageID}

	// Repair first: a star whose live message sits in a different live
	// topic follows it (owner and save time unchanged). Messages in a topic
	// never move again, so the values written are current.
	repaired, err := tx.ExecContext(ctx, `
UPDATE webchat_message_star st
   SET conversation_key = t.id, project_id = t.project_id
  FROM `+messagesTable+` m
  JOIN webchat_topic t ON t.id = m.thread_id AND t.deleted_at IS NULL
 WHERE m.id = `+pgUUIDText("st.message_id")+`
   AND `+rng+`
   AND (st.conversation_key <> t.id OR st.project_id IS DISTINCT FROM t.project_id)
   AND NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)`, rngArgs...)
	if err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: repair message stars: %w", err)
	}
	if res.Repaired, err = repaired.RowsAffected(); err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: repair message stars: %w", err)
	}

	// Then purge targets that are permanently gone. Nothing here reads a
	// permission or a user's status.
	purged, err := tx.ExecContext(ctx, `
DELETE FROM webchat_message_star st
 WHERE `+rng+`
   AND (NOT EXISTS (SELECT 1 FROM `+usersTable+` u WHERE u.id = `+pgUUIDText("st.user_id")+`)
     OR NOT EXISTS (SELECT 1 FROM `+messagesTable+` m WHERE m.id = `+pgUUIDText("st.message_id")+`)
     OR EXISTS (SELECT 1 FROM webchat_message_ext e
                 WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)
     OR (COALESCE(st.project_id, '') <> ''
         AND NOT EXISTS (SELECT 1 FROM `+projectsTable+` p WHERE p.id = `+pgUUIDText("st.project_id")+`))
     OR (left(st.conversation_key, 3) <> 'dm:'
         AND NOT EXISTS (SELECT 1 FROM webchat_topic t
                          WHERE t.id = st.conversation_key AND t.deleted_at IS NULL))
     OR EXISTS (SELECT 1 FROM `+messagesTable+` m JOIN webchat_topic t ON t.id = m.thread_id
                 WHERE m.id = `+pgUUIDText("st.message_id")+` AND t.deleted_at IS NOT NULL))`, rngArgs...)
	if err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: purge message stars: %w", err)
	}
	if res.Purged, err = purged.RowsAffected(); err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: purge message stars: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: purge message stars: %w", err)
	}
	return res, nil
}
