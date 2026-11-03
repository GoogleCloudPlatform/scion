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

// MessagePosition is a keyset position in history order: a message's
// creation instant and its canonical lowercase ID.
type MessagePosition struct {
	Created time.Time
	ID      string
}

// StarHistoryQuery selects one page of a user's starred messages in one
// conversation, in history order (the starred-only center).
type StarHistoryQuery struct {
	UserID   string
	Selector StarSelector
	// Cursor resumes strictly past this position in the page's order
	// (the (created, id) tuple, so a message sharing the cursor's instant
	// is still found); nil starts at the newest (or, ascending, the
	// oldest) message. The two halves of an around page are the anchor's
	// position in both directions.
	Cursor *MessagePosition
	// Ascending orders oldest first; the default is newest first.
	Ascending bool
	// Limit is the page size (at least 1).
	Limit int
}

// StarHistoryPage is one page of starred history. Items are in the
// query's order; Next is the position to resume after when more may
// remain, nil at the end.
type StarHistoryPage struct {
	Items []MessagePosition
	Next  *MessagePosition
	Total int
}

// trimStarHistoryPage cuts a page fetched with limit+1 rows to limit and
// sets Next when the extra row showed that more remain.
func trimStarHistoryPage(page StarHistoryPage, limit int) StarHistoryPage {
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.Next = &last
	}
	return page
}

// validStarHistoryQuery reports whether q names a user, a conversation
// and a page size.
func validStarHistoryQuery(q StarHistoryQuery) bool {
	return q.UserID != "" && q.Selector.ConversationKey != "" && q.Limit >= 1
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
	// ListStarredHistory returns one page of the user's starred messages in
	// the selector's conversation, in history order: the live messages of
	// its history scope that the user starred, (created, id) newest first,
	// or oldest first when q.Ascending. Total counts every live starred
	// message of the conversation, whatever the bounds.
	ListStarredHistory(ctx context.Context, q StarHistoryQuery) (StarHistoryPage, error)
	// ListStarredConversationCandidates returns one page of the distinct
	// conversations in which the user has at least one live star (the
	// Starred rail), grouped by the current conversation key before the
	// page is cut, in f.Sort order. It applies no read access: the handler
	// authorizes every candidate.
	ListStarredConversationCandidates(ctx context.Context, f StarredConversationFilter) (StarredConversationPage, error)

	// Lifecycle. These are not scoped to one owner: they remove or repair
	// the associations of a message, conversation, user or project that
	// changed, and never return message content.

	// MarkMessageDeletedAndClearStars soft-deletes the message as
	// SetMessageDeleted does (the extension row and its mentions) and
	// deletes every star of it in the same transaction. It returns the
	// distinct owners whose star was deleted, collected inside that
	// transaction. SetMessageDeleted shares the transaction and discards
	// the owners.
	MarkMessageDeletedAndClearStars(ctx context.Context, messageID string, deletedAt time.Time) ([]string, error)
	// DeleteMessageStarsForConversation deletes the stars listed under a
	// conversation key (a deleted topic or DM); it returns how many.
	DeleteMessageStarsForConversation(ctx context.Context, conversationKey string) (int64, error)
	// DeleteMessageStarsForUser deletes every star a user owns.
	DeleteMessageStarsForUser(ctx context.Context, userID string) (int64, error)
	// DeleteMessageStarsForProject deletes the stars stored under a
	// project (its threads and the DMs with its agents).
	DeleteMessageStarsForProject(ctx context.Context, projectID string) (int64, error)
	// PurgeMessageStarCandidates examines at most limit associations after
	// the position, in (user_id, message_id) order. It repairs the stored
	// conversation key and project of a star whose message now sits in a
	// different live topic (a promotion an older binary ran), keeping the
	// owner and save time, then deletes the examined stars whose user,
	// message, stored project or topic no longer exists, or whose message
	// or topic is soft-deleted. It never looks at permissions or a user's
	// status. The result's Next is the last examined position, or the zero
	// position when the scan reached the end (the next pass starts over).
	PurgeMessageStarCandidates(ctx context.Context, after StarCleanupPosition, limit int) (StarCleanupResult, error)
}

// StarCleanupPosition is a position in the (user_id, message_id) order of
// the upkeep scan; the zero value is the start.
type StarCleanupPosition struct {
	UserID    string
	MessageID string
}

// StarCleanupResult is the outcome of one upkeep batch.
type StarCleanupResult struct {
	// Next is where the next batch resumes; zero when the scan wrapped.
	Next StarCleanupPosition
	// Examined, Repaired and Purged count associations.
	Examined int
	Repaired int64
	Purged   int64
}

// starCleanupRange reads the (user_id, message_id) rows of an upkeep batch
// and returns the first and last positions and how many there are.
func starCleanupRange(rows *sql.Rows) (first, last StarCleanupPosition, n int, err error) {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p StarCleanupPosition
		if err := rows.Scan(&p.UserID, &p.MessageID); err != nil {
			return first, last, 0, err
		}
		if n == 0 {
			first = p
		}
		last = p
		n++
	}
	return first, last, n, rows.Err()
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

// sqliteMessageCreatedExpr is messages.created on SQLite (Go's default time
// text in UTC) without the monotonic-clock suffix (" m=+0.03") a row
// written before the UTC store boundary may still carry; the entadapter's
// timeColumnExpr form. Comparisons and order use it so such a row sorts
// at its instant.
const sqliteMessageCreatedExpr = `(CASE WHEN instr(m.created, ' m=') > 0 THEN substr(m.created, 1, instr(m.created, ' m=') - 1) ELSE m.created END)`

// sqliteMessageTimeLayout is the layout of that text.
const sqliteMessageTimeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

// sqliteMessageTime is the text an instant compares equal to in
// sqliteMessageCreatedExpr (the entadapter's timeArg on SQLite).
func sqliteMessageTime(t time.Time) string {
	return t.UTC().String()
}

func (s *sqliteWebChatStore) ListStarredHistory(ctx context.Context, q StarHistoryQuery) (StarHistoryPage, error) {
	if !validStarHistoryQuery(q) {
		return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: %w", store.ErrInvalidInput)
	}
	selCond, selArgs := sqliteStarSelectorSQL(q.Selector)
	base := []string{"st.user_id = ?", "st.conversation_key = ?", selCond,
		"NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = st.message_id AND e.deleted_at IS NOT NULL)"}
	baseArgs := append([]any{q.UserID, q.Selector.ConversationKey}, selArgs...)
	from := `
  FROM webchat_message_star st
  JOIN messages m ON m.id = st.message_id
 WHERE `

	page := StarHistoryPage{Items: []MessagePosition{}}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from+strings.Join(base, " AND "), baseArgs...).Scan(&page.Total); err != nil {
		return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: count: %w", err)
	}

	conds := append([]string{}, base...)
	args := append([]any{}, baseArgs...)
	cmp, dir := "<", "DESC"
	if q.Ascending {
		cmp, dir = ">", "ASC"
	}
	if q.Cursor != nil {
		at := sqliteMessageTime(q.Cursor.Created)
		conds = append(conds, "("+sqliteMessageCreatedExpr+" "+cmp+" ? OR ("+sqliteMessageCreatedExpr+" = ? AND m.id "+cmp+" ?))")
		args = append(args, at, at, strings.ToLower(q.Cursor.ID))
	}
	args = append(args, q.Limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, `+sqliteMessageCreatedExpr+from+strings.Join(conds, " AND ")+`
 ORDER BY `+sqliteMessageCreatedExpr+` `+dir+`, m.id `+dir+`
 LIMIT ?`, args...)
	if err != nil {
		return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, created string
		if err := rows.Scan(&id, &created); err != nil {
			return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: %w", err)
		}
		t, err := time.Parse(sqliteMessageTimeLayout, created)
		if err != nil {
			return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: message time: %w", err)
		}
		page.Items = append(page.Items, MessagePosition{Created: t.UTC(), ID: id})
	}
	if err := rows.Err(); err != nil {
		return StarHistoryPage{}, fmt.Errorf("webchat store: list starred history: %w", err)
	}
	return trimStarHistoryPage(page, q.Limit), nil
}

// ---------------------------------------------------------------------------
// SQLite lifecycle
// ---------------------------------------------------------------------------

// markMessageDeletedTx is the deletion transaction SetMessageDeleted and
// MarkMessageDeletedAndClearStars share: the extension upsert first (it
// takes SQLite's write lock, so a concurrent star add, whose guarded insert
// is its first statement, is ordered before or after the whole deletion),
// then mentions, then the stars, whose owners it returns.
func (s *sqliteWebChatStore) markMessageDeletedTx(ctx context.Context, messageID string, deletedAt time.Time) ([]string, error) {
	const query = `
INSERT INTO webchat_message_ext (message_id, deleted_at)
VALUES (?, ?)
ON CONFLICT (message_id)
DO UPDATE SET deleted_at = excluded.deleted_at
`
	// One transaction: the soft delete, its mention cleanup and its star
	// cleanup land together, so a deleted message never keeps lighting a
	// mention dot or a star.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("webchat store: set message deleted begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, query, messageID, deletedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("webchat store: set message deleted: %w", err)
	}
	// A deleted message no longer mentions anyone.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webchat_mention WHERE message_id = ?`, messageID); err != nil {
		return nil, fmt.Errorf("webchat store: delete message mentions: %w", err)
	}
	owners, err := queryStarOwners(ctx, tx, `SELECT DISTINCT user_id FROM webchat_message_star WHERE message_id = ? ORDER BY user_id`, messageID)
	if err != nil {
		return nil, fmt.Errorf("webchat store: list message star owners: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webchat_message_star WHERE message_id = ?`, messageID); err != nil {
		return nil, fmt.Errorf("webchat store: delete message stars: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("webchat store: set message deleted: %w", err)
	}
	return owners, nil
}

// queryStarOwners reads a single column of user IDs.
func queryStarOwners(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	owners := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		owners = append(owners, id)
	}
	return owners, rows.Err()
}

func (s *sqliteWebChatStore) MarkMessageDeletedAndClearStars(ctx context.Context, messageID string, deletedAt time.Time) ([]string, error) {
	return s.markMessageDeletedTx(ctx, messageID, deletedAt)
}

func (s *sqliteWebChatStore) DeleteMessageStarsForConversation(ctx context.Context, conversationKey string) (int64, error) {
	return s.deleteStarsWhere(ctx, "conversation", `conversation_key = ?`, conversationKey)
}

func (s *sqliteWebChatStore) DeleteMessageStarsForUser(ctx context.Context, userID string) (int64, error) {
	return s.deleteStarsWhere(ctx, "user", `user_id = ?`, userID)
}

func (s *sqliteWebChatStore) DeleteMessageStarsForProject(ctx context.Context, projectID string) (int64, error) {
	return s.deleteStarsWhere(ctx, "project", `project_id = ?`, projectID)
}

func (s *sqliteWebChatStore) deleteStarsWhere(ctx context.Context, what, cond, value string) (int64, error) {
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

// sqliteStarRange is the predicate selecting the stars between first and
// last (inclusive) in (user_id, message_id) order.
func sqliteStarRange(first, last StarCleanupPosition) (string, []any) {
	const t = "webchat_message_star."
	return `(` + t + `user_id > ? OR (` + t + `user_id = ? AND ` + t + `message_id >= ?))
   AND (` + t + `user_id < ? OR (` + t + `user_id = ? AND ` + t + `message_id <= ?))`,
		[]any{first.UserID, first.UserID, first.MessageID, last.UserID, last.UserID, last.MessageID}
}

func (s *sqliteWebChatStore) PurgeMessageStarCandidates(ctx context.Context, after StarCleanupPosition, limit int) (StarCleanupResult, error) {
	if limit < 1 {
		return StarCleanupResult{}, fmt.Errorf("webchat store: purge message stars: %w", store.ErrInvalidInput)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT user_id, message_id FROM webchat_message_star
 WHERE user_id > ? OR (user_id = ? AND message_id > ?)
 ORDER BY user_id, message_id
 LIMIT ?`, after.UserID, after.UserID, after.MessageID, limit)
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

	// Repair first: a star whose live message sits in a different live
	// topic follows it (owner and save time unchanged).
	rng, rngArgs := sqliteStarRange(first, last)
	repaired, err := tx.ExecContext(ctx, `
UPDATE webchat_message_star
   SET conversation_key = t.id, project_id = t.project_id
  FROM messages m
  JOIN webchat_topic t ON t.id = m.thread_id AND t.deleted_at IS NULL
 WHERE m.id = webchat_message_star.message_id
   AND `+rng+`
   AND (webchat_message_star.conversation_key <> t.id OR webchat_message_star.project_id IS NOT t.project_id)
   AND NOT EXISTS (SELECT 1 FROM webchat_message_ext e WHERE e.message_id = m.id AND e.deleted_at IS NOT NULL)`, rngArgs...)
	if err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: repair message stars: %w", err)
	}
	if res.Repaired, err = repaired.RowsAffected(); err != nil {
		return StarCleanupResult{}, fmt.Errorf("webchat store: repair message stars: %w", err)
	}

	// Then purge targets that are permanently gone. Nothing here reads a
	// permission or a user's status.
	purged, err := tx.ExecContext(ctx, `
DELETE FROM webchat_message_star
 WHERE `+rng+`
   AND (NOT EXISTS (SELECT 1 FROM users u WHERE u.id = webchat_message_star.user_id)
     OR NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = webchat_message_star.message_id)
     OR EXISTS (SELECT 1 FROM webchat_message_ext e
                 WHERE e.message_id = webchat_message_star.message_id AND e.deleted_at IS NOT NULL)
     OR (COALESCE(webchat_message_star.project_id, '') <> ''
         AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.id = webchat_message_star.project_id))
     OR (substr(webchat_message_star.conversation_key, 1, 3) <> 'dm:'
         AND NOT EXISTS (SELECT 1 FROM webchat_topic t
                          WHERE t.id = webchat_message_star.conversation_key AND t.deleted_at IS NULL))
     OR EXISTS (SELECT 1 FROM messages m JOIN webchat_topic t ON t.id = m.thread_id
                 WHERE m.id = webchat_message_star.message_id AND t.deleted_at IS NOT NULL))`, rngArgs...)
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
