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

// Starred messages in native web chat (web.chat_starred_messages).
//
// A star is a private (user, message) association in webchat_message_star.
// The message ID is its identity; conversation_key and project_id are
// query accelerators derived by the hub when the star is added, never
// authority: every read joins the live message and applies the same
// channel and conversation selector as native history, and the handler
// re-checks the caller's read access to the conversation.

// StarSelector is the native history scope a starred message must be in:
// the conversation key the star is listed under, the history channel, and
// the conversation selector history uses (the conversation ID when the
// read switch resolved one, else the legacy thread ID).
type StarSelector struct {
	// ConversationKey is the topic ID (or exact DM key) the star is
	// listed under.
	ConversationKey string
	// Channel limits matching messages to one channel ("web" for native
	// chat); empty matches every channel.
	Channel string
	// ConversationID selects messages by conversation when set; otherwise
	// ThreadID selects them by legacy thread ID.
	ConversationID string
	ThreadID       string
}

// StarTarget is a server-resolved star to add. It is never a client DTO.
type StarTarget struct {
	MessageID string
	// ProjectID is the topic's project; empty for a human DM.
	ProjectID string
	Selector  StarSelector
}

// StarRecord is one stored association.
type StarRecord struct {
	UserID          string
	MessageID       string
	ConversationKey string
	ProjectID       string
	StarredAt       time.Time
}

// StarPosition is a keyset position in (starred_at DESC, message_id DESC)
// order.
type StarPosition struct {
	StarredAt time.Time
	MessageID string
}

// StarFilter selects one page of a user's stars in one conversation.
type StarFilter struct {
	UserID   string
	Selector StarSelector
	// After resumes strictly after this position; nil starts at the
	// newest star.
	After *StarPosition
	// Limit is the page size (at least 1).
	Limit int
}

// StarCandidatePage is one page of stars whose messages are live and in
// the selector's history scope. Next is the position to resume after when
// more may remain, nil at the end.
type StarCandidatePage struct {
	Items []StarRecord
	Next  *StarPosition
}

// StarredMessageRef is a live starred message with the routing fields
// the handler needs to apply the history selector of its conversation.
type StarredMessageRef struct {
	MessageID       string
	ConversationKey string
	ProjectID       string
	Channel         string
	ThreadID        string
	ConversationID  string
}

// matches reports whether ref is in the history scope of sel.
func (ref StarredMessageRef) matches(sel StarSelector) bool {
	if ref.ConversationKey != sel.ConversationKey {
		return false
	}
	if sel.Channel != "" && ref.Channel != sel.Channel {
		return false
	}
	if sel.ConversationID != "" {
		return strings.EqualFold(ref.ConversationID, sel.ConversationID)
	}
	return ref.ThreadID == sel.ThreadID
}

// MessageStarStore persists starred messages. Both webchat store dialects
// implement it; obtain it with messageStarStoreFrom. Every method takes
// the owner's user ID and matches it, so one user can never read or
// change another user's association.
type MessageStarStore interface {
	// AddMessageStar inserts the association when the target message
	// exists, is in the selector's history scope and is not deleted; the
	// check and the insert run in one transaction. An existing
	// association keeps its starred_at; changed reports whether a row was
	// inserted. A target that fails the check returns store.ErrNotFound.
	AddMessageStar(ctx context.Context, userID string, target StarTarget) (rec StarRecord, changed bool, err error)
	// RemoveMessageStar deletes the user's association; it reports
	// whether one existed.
	RemoveMessageStar(ctx context.Context, userID, messageID string) (bool, error)
	// GetMessageStar returns the user's stored association, or nil. It
	// does not check the message: callers resolve the current target.
	GetMessageStar(ctx context.Context, userID, messageID string) (*StarRecord, error)
	// ListMessageStarCandidates returns one page of the user's stars in
	// the selector's conversation whose messages are live and in its
	// history scope, newest star first.
	ListMessageStarCandidates(ctx context.Context, f StarFilter) (StarCandidatePage, error)
	// ListStarredMessageIDs returns which of messageIDs the user starred
	// and are live (not deleted), with their routing fields.
	ListStarredMessageIDs(ctx context.Context, userID string, messageIDs []string) ([]StarredMessageRef, error)
}

// messageStarStoreFrom returns the star store behind a webchat store, or
// nil when it has none (test doubles that did not opt in).
func messageStarStoreFrom(wcs WebChatStore) MessageStarStore {
	if mss, ok := wcs.(MessageStarStore); ok {
		return mss
	}
	return nil
}

// messageStarTableMigration names the webchat_migrations row that records
// the creation of webchat_message_star.
const messageStarTableMigration = "message_star_table"

// starTimePrecision is the stored precision of starred_at in both
// dialects (PostgreSQL TIMESTAMPTZ keeps microseconds), so both return
// identical values and cursors.
const starTimePrecision = time.Microsecond

// starNow returns the current instant at the stored precision.
func starNow() time.Time {
	return time.Now().UTC().Truncate(starTimePrecision)
}

// sqliteStarTimeLayout is the fixed-width TEXT form of starred_at, so a
// lexical comparison is chronological even for whole seconds.
const sqliteStarTimeLayout = "2006-01-02T15:04:05.000000000Z"

func sqliteStarTime(t time.Time) string {
	return t.UTC().Truncate(starTimePrecision).Format(sqliteStarTimeLayout)
}

func parseSQLiteStarTime(s string) (time.Time, error) {
	t, err := time.Parse(sqliteStarTimeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("webchat store: parse starred_at %q: %w", s, err)
	}
	return t.UTC(), nil
}

// ---------------------------------------------------------------------------
// SQLite implementation
// ---------------------------------------------------------------------------

const sqliteMessageStarDDL = `
CREATE TABLE IF NOT EXISTS webchat_message_star (
    user_id          TEXT NOT NULL,
    message_id       TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    project_id       TEXT,
    starred_at       TEXT NOT NULL,
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
// (idempotent) and records the migration after all DDL succeeded. It runs
// whether or not the experiment is on.
func (s *sqliteWebChatStore) initMessageStars() error {
	if _, err := s.db.Exec(sqliteMessageStarDDL); err != nil {
		return fmt.Errorf("webchat store: create message star table: %w", err)
	}
	const query = `INSERT INTO webchat_migrations (name, completed_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`
	if _, err := s.db.Exec(query, messageStarTableMigration, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("webchat store: record message star migration: %w", err)
	}
	return nil
}

// sqliteStarSelectorSQL returns the predicate (on messages m) and its
// arguments for sel.
func sqliteStarSelectorSQL(sel StarSelector) (string, []any) {
	cond := "(? = '' OR m.channel = ?)"
	args := []any{sel.Channel, sel.Channel}
	if sel.ConversationID != "" {
		cond += " AND m.conversation_id = ?"
		args = append(args, strings.ToLower(sel.ConversationID))
	} else {
		cond += " AND m.thread_id = ?"
		args = append(args, sel.ThreadID)
	}
	return cond, args
}

// sqliteStarTargetGuard is the shared existence/scope/deletion check of an
// add: the message exists, is in the selector's scope, is not deleted, and
// (for a topic key) the topic is live.
func sqliteStarTargetGuard(target StarTarget) (string, []any) {
	selCond, selArgs := sqliteStarSelectorSQL(target.Selector)
	cond := `m.id = ? AND ` + selCond + `
   AND NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = m.id AND e.deleted_at IS NOT NULL)
   AND (substr(?, 1, 3) = 'dm:' OR EXISTS (SELECT 1 FROM webchat_topic t WHERE t.id = ? AND t.deleted_at IS NULL))`
	args := append([]any{target.MessageID}, selArgs...)
	args = append(args, target.Selector.ConversationKey, target.Selector.ConversationKey)
	return cond, args
}

func (s *sqliteWebChatStore) AddMessageStar(ctx context.Context, userID string, target StarTarget) (StarRecord, bool, error) {
	if userID == "" || target.MessageID == "" || target.Selector.ConversationKey == "" {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", store.ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The guarded insert is the first statement of the write transaction,
	// so SQLite's writer serialization orders it against a deletion.
	guard, guardArgs := sqliteStarTargetGuard(target)
	insert := `
INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at)
SELECT ?, m.id, ?, ?, ?
  FROM messages m
 WHERE ` + guard + `
ON CONFLICT (user_id, message_id) DO NOTHING`
	args := append([]any{userID, target.Selector.ConversationKey, nullableString(target.ProjectID), sqliteStarTime(starNow())}, guardArgs...)
	res, err := tx.ExecContext(ctx, insert, args...)
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", err)
	}
	if n == 0 {
		// Either the association already existed or the target failed the
		// check; an existing row never makes an invalid target valid.
		var ok int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m WHERE `+guard, guardArgs...).Scan(&ok)
		if err != nil {
			return StarRecord{}, false, fmt.Errorf("webchat store: add message star: check target: %w", err)
		}
		if ok == 0 {
			return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", store.ErrNotFound)
		}
	}
	rec, err := scanSQLiteStar(tx.QueryRowContext(ctx, `
SELECT user_id, message_id, conversation_key, COALESCE(project_id, ''), starred_at
  FROM webchat_message_star WHERE user_id = ? AND message_id = ?`, userID, target.MessageID))
	if err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: read back: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StarRecord{}, false, fmt.Errorf("webchat store: add message star: %w", err)
	}
	return *rec, n == 1, nil
}

func scanSQLiteStar(row interface{ Scan(...any) error }) (*StarRecord, error) {
	var rec StarRecord
	var starredAt string
	if err := row.Scan(&rec.UserID, &rec.MessageID, &rec.ConversationKey, &rec.ProjectID, &starredAt); err != nil {
		return nil, err
	}
	t, err := parseSQLiteStarTime(starredAt)
	if err != nil {
		return nil, err
	}
	rec.StarredAt = t
	return &rec, nil
}

func (s *sqliteWebChatStore) RemoveMessageStar(ctx context.Context, userID, messageID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webchat_message_star WHERE user_id = ? AND message_id = ?`, userID, messageID)
	if err != nil {
		return false, fmt.Errorf("webchat store: remove message star: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("webchat store: remove message star: %w", err)
	}
	return n > 0, nil
}

func (s *sqliteWebChatStore) GetMessageStar(ctx context.Context, userID, messageID string) (*StarRecord, error) {
	rec, err := scanSQLiteStar(s.db.QueryRowContext(ctx, `
SELECT user_id, message_id, conversation_key, COALESCE(project_id, ''), starred_at
  FROM webchat_message_star WHERE user_id = ? AND message_id = ?`, userID, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("webchat store: get message star: %w", err)
	}
	return rec, nil
}

func (s *sqliteWebChatStore) ListMessageStarCandidates(ctx context.Context, f StarFilter) (StarCandidatePage, error) {
	if f.UserID == "" || f.Selector.ConversationKey == "" || f.Limit < 1 {
		return StarCandidatePage{}, fmt.Errorf("webchat store: list message stars: %w", store.ErrInvalidInput)
	}
	selCond, selArgs := sqliteStarSelectorSQL(f.Selector)
	conds := []string{"st.user_id = ?", "st.conversation_key = ?", selCond,
		"NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)"}
	args := []any{f.UserID, f.Selector.ConversationKey}
	args = append(args, selArgs...)
	if f.After != nil {
		at := sqliteStarTime(f.After.StarredAt)
		conds = append(conds, "(st.starred_at < ? OR (st.starred_at = ? AND st.message_id < ?))")
		args = append(args, at, at, f.After.MessageID)
	}
	args = append(args, f.Limit+1)
	rows, err := s.db.QueryContext(ctx, `
SELECT st.user_id, st.message_id, st.conversation_key, COALESCE(st.project_id, ''), st.starred_at
  FROM webchat_message_star st
  JOIN messages m ON m.id = st.message_id
 WHERE `+strings.Join(conds, " AND ")+`
 ORDER BY st.starred_at DESC, st.message_id DESC
 LIMIT ?`, args...)
	if err != nil {
		return StarCandidatePage{}, fmt.Errorf("webchat store: list message stars: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := StarCandidatePage{Items: []StarRecord{}}
	for rows.Next() {
		rec, err := scanSQLiteStar(rows)
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

// trimStarPage cuts a page fetched with limit+1 rows to limit and sets
// Next when the extra row showed that more remain.
func trimStarPage(page StarCandidatePage, limit int) StarCandidatePage {
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.Next = &StarPosition{StarredAt: last.StarredAt, MessageID: last.MessageID}
	}
	return page
}

func (s *sqliteWebChatStore) ListStarredMessageIDs(ctx context.Context, userID string, messageIDs []string) ([]StarredMessageRef, error) {
	out := []StarredMessageRef{}
	if userID == "" || len(messageIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(messageIDs))
	args := make([]any, 0, len(messageIDs)+1)
	args = append(args, userID)
	for i, id := range messageIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT st.message_id, st.conversation_key, COALESCE(st.project_id, ''),
       COALESCE(m.channel, ''), COALESCE(m.thread_id, ''), COALESCE(m.conversation_id, '')
  FROM webchat_message_star st
  JOIN messages m ON m.id = st.message_id
 WHERE st.user_id = ? AND st.message_id IN (`+strings.Join(placeholders, ",")+`)
   AND NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)
 ORDER BY st.message_id`, args...)
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
