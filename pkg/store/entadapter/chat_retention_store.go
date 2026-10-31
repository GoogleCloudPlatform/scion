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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/chatretentionrun"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ChatRetentionStore implements store.ChatRetentionStore. The selection and
// the purge are raw SQL over the *sql.DB the ent client uses, which is also
// the database of the web chat tables (webchat_*), so one *sql.Tx deletes a
// message and all of its dependent rows. The SQL is the same on SQLite and
// Postgres apart from the placeholder style: every statement is an IN list
// or an index range, and DELETE ... RETURNING and INSERT ... ON CONFLICT
// exist in both dialects.
//
// Timestamps are bound as UTC time.Time values, the form the ent columns
// store (on SQLite the driver writes time.Time.String() text; ent's UTC hook
// makes ent writes UTC too), so comparisons against messages.created and
// the cursor columns agree with ent's own.
type ChatRetentionStore struct {
	client *ent.Client
	db     *sql.DB
	pg     bool
}

// NewChatRetentionStore creates a ChatRetentionStore over client and its
// underlying database.
func NewChatRetentionStore(client *ent.Client) *ChatRetentionStore {
	return &ChatRetentionStore{
		client: client,
		db:     clientDB(client),
		pg:     client.Driver().Dialect() == dialect.Postgres,
	}
}

var _ store.ChatRetentionStore = (*ChatRetentionStore)(nil)

// placeholders returns n bind placeholders starting at bind position
// start (1-based): "?, ?, ..." on SQLite and "$start, $start+1, ..." on
// Postgres.
func (s *ChatRetentionStore) placeholders(start, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		if s.pg {
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(start + i))
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// rebind rewrites the ? placeholders of query to $1.. on Postgres. Queries
// passed here contain no ? other than placeholders.
func (s *ChatRetentionStore) rebind(query string) string {
	if !s.pg {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *ChatRetentionStore) requireDB() error {
	if s.db == nil {
		return errors.New("chat retention store: no SQL database")
	}
	return nil
}

// SelectChatRetentionCandidates implements store.ChatRetentionStore.
//
// The lower bound "created >= cursor" is spelled out (besides the
// (created, id) tiebreak) so SQLite's planner gets a two-sided range on the
// created index rather than walking it from the oldest row.
func (s *ChatRetentionStore) SelectChatRetentionCandidates(ctx context.Context, cutoff time.Time, after store.ChatRetentionPosition, limit int) ([]store.ChatRetentionCandidate, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	query := `SELECT id, created, project_id, conversation_id FROM messages WHERE created < ?`
	args := []any{cutoff.UTC()}
	if !after.IsStart() {
		c := after.Created.UTC()
		query += ` AND created >= ? AND (created > ? OR id > ?)`
		args = append(args, c, c, after.ID)
	}
	query += ` AND dispatch_state NOT IN ('pending', 'deferred') ORDER BY created, id LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("chat retention: select candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]store.ChatRetentionCandidate, 0, limit)
	for rows.Next() {
		var c store.ChatRetentionCandidate
		var conv sql.NullString
		if err := rows.Scan(&c.ID, &c.Created, &c.ProjectID, &conv); err != nil {
			return nil, fmt.Errorf("chat retention: scan candidate: %w", err)
		}
		c.Created = c.Created.UTC()
		c.ConversationID = conv.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat retention: select candidates: %w", err)
	}
	return out, nil
}

// chatRetentionDependent is one dependent table the purge clears, keyed by
// message_id, with the counter it adds to.
type chatRetentionDependent struct {
	table string
	count func(*store.ChatRetentionCounts) *int64
}

// chatRetentionDependents lists the dependent tables in the order the purge
// clears them (design §6.3); webchat_message_attachment is handled apart
// because its attachment IDs are captured.
var chatRetentionDependents = []chatRetentionDependent{
	{"webchat_message_star", func(c *store.ChatRetentionCounts) *int64 { return &c.Stars }},
	{"webchat_mention", func(c *store.ChatRetentionCounts) *int64 { return &c.Mentions }},
	{"webchat_message_ext", func(c *store.ChatRetentionCounts) *int64 { return &c.Ext }},
}

// PurgeChatMessages implements store.ChatRetentionStore.
func (s *ChatRetentionStore) PurgeChatMessages(ctx context.Context, ids []string, projectOf map[string]string, cursor store.ChatRetentionCursor) (store.ChatRetentionPurgeResult, error) {
	res := store.ChatRetentionPurgeResult{ByProject: map[string]store.ChatRetentionCounts{}}
	if err := s.requireDB(); err != nil {
		return res, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("chat retention: begin purge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(ids) > 0 {
		if err := s.purgeTx(ctx, tx, ids, projectOf, &res); err != nil {
			return store.ChatRetentionPurgeResult{ByProject: map[string]store.ChatRetentionCounts{}}, err
		}
	}
	if err := s.upsertCursor(ctx, tx, cursor); err != nil {
		return store.ChatRetentionPurgeResult{ByProject: map[string]store.ChatRetentionCounts{}}, err
	}
	if err := tx.Commit(); err != nil {
		return store.ChatRetentionPurgeResult{ByProject: map[string]store.ChatRetentionCounts{}}, fmt.Errorf("chat retention: commit purge: %w", err)
	}
	return res, nil
}

// purgeTx runs the cascade of one chunk on tx.
func (s *ChatRetentionStore) purgeTx(ctx context.Context, tx *sql.Tx, ids []string, projectOf map[string]string, res *store.ChatRetentionPurgeResult) error {
	in := s.placeholders(1, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	add := func(messageID string, f func(*store.ChatRetentionCounts) *int64) {
		p := projectOf[messageID]
		c := res.ByProject[p]
		*f(&c)++
		res.ByProject[p] = c
	}

	for _, d := range chatRetentionDependents {
		if err := s.deleteReturning(ctx, tx, "DELETE FROM "+d.table+" WHERE message_id IN ("+in+") RETURNING message_id", args, func(rows *sql.Rows) error {
			var mid string
			if err := rows.Scan(&mid); err != nil {
				return err
			}
			add(mid, d.count)
			return nil
		}); err != nil {
			return fmt.Errorf("chat retention: purge %s: %w", d.table, err)
		}
	}

	// Attachment links: capture the attachment IDs so the ones left with
	// no link can be reported (their metadata and bytes stay for the
	// attachment GC in this phase).
	var attachmentIDs []string
	seenAttachment := map[string]bool{}
	if err := s.deleteReturning(ctx, tx, "DELETE FROM webchat_message_attachment WHERE message_id IN ("+in+") RETURNING message_id, attachment_id", args, func(rows *sql.Rows) error {
		var mid, aid string
		if err := rows.Scan(&mid, &aid); err != nil {
			return err
		}
		add(mid, func(c *store.ChatRetentionCounts) *int64 { return &c.Attachments })
		if !seenAttachment[aid] {
			seenAttachment[aid] = true
			attachmentIDs = append(attachmentIDs, aid)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("chat retention: purge webchat_message_attachment: %w", err)
	}

	if err := s.deleteReturning(ctx, tx, "DELETE FROM message_addressees WHERE message_id IN ("+in+") RETURNING message_id", args, func(rows *sql.Rows) error {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			return err
		}
		add(mid, func(c *store.ChatRetentionCounts) *int64 { return &c.Addressees })
		return nil
	}); err != nil {
		return fmt.Errorf("chat retention: purge message_addressees: %w", err)
	}

	if err := s.deleteReturning(ctx, tx, "DELETE FROM messages WHERE id IN ("+in+") RETURNING id", args, func(rows *sql.Rows) error {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			return err
		}
		add(mid, func(c *store.ChatRetentionCounts) *int64 { return &c.Main })
		return nil
	}); err != nil {
		return fmt.Errorf("chat retention: purge messages: %w", err)
	}

	if len(attachmentIDs) > 0 {
		aargs := make([]any, len(attachmentIDs))
		for i, id := range attachmentIDs {
			aargs[i] = id
		}
		q := "SELECT a.id, a.project_id FROM webchat_attachment a WHERE a.id IN (" + s.placeholders(1, len(attachmentIDs)) +
			") AND NOT EXISTS (SELECT 1 FROM webchat_message_attachment l WHERE l.attachment_id = a.id)"
		rows, err := tx.QueryContext(ctx, q, aargs...)
		if err != nil {
			return fmt.Errorf("chat retention: unlinked attachments: %w", err)
		}
		for rows.Next() {
			var aid, project string
			if err := rows.Scan(&aid, &project); err != nil {
				_ = rows.Close()
				return fmt.Errorf("chat retention: scan unlinked attachment: %w", err)
			}
			res.UnlinkedAttachmentIDs = append(res.UnlinkedAttachmentIDs, aid)
			c := res.ByProject[project]
			c.UnlinkedAttachments++
			res.ByProject[project] = c
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("chat retention: unlinked attachments: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("chat retention: unlinked attachments: %w", err)
		}
	}
	return nil
}

// deleteReturning runs a DELETE ... RETURNING on tx and passes each
// returned row to scan. The rows are drained and closed before it returns,
// so the next statement can use the transaction's connection.
func (s *ChatRetentionStore) deleteReturning(ctx context.Context, tx *sql.Tx, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := scan(rows); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

// chatRetentionExecer is a *sql.DB or *sql.Tx.
type chatRetentionExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// upsertCursor writes cursor on ex. It is raw SQL (not ent) so it runs in
// the purge transaction: on SQLite the pool has one connection, which the
// transaction holds.
func (s *ChatRetentionStore) upsertCursor(ctx context.Context, ex chatRetentionExecer, c store.ChatRetentionCursor) error {
	if c.Phase == "" {
		return errors.New("chat retention: cursor has no phase")
	}
	nullTime := func(t *time.Time) any {
		if t == nil || t.IsZero() {
			return nil
		}
		return t.UTC()
	}
	var created any
	if !c.Position.Created.IsZero() {
		created = c.Position.Created.UTC()
	}
	updated := c.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	q := s.rebind(`INSERT INTO chat_retention_cursors (phase, cursor_created, cursor_id, reset_at, last_applied_cutoff, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (phase) DO UPDATE SET cursor_created = excluded.cursor_created, cursor_id = excluded.cursor_id,
  reset_at = excluded.reset_at, last_applied_cutoff = excluded.last_applied_cutoff, updated_at = excluded.updated_at`)
	if _, err := ex.ExecContext(ctx, q, c.Phase, created, c.Position.ID, nullTime(c.ResetAt), nullTime(c.LastAppliedCutoff), updated.UTC()); err != nil {
		return fmt.Errorf("chat retention: save cursor: %w", err)
	}
	return nil
}

// GetChatRetentionCursor implements store.ChatRetentionStore.
func (s *ChatRetentionStore) GetChatRetentionCursor(ctx context.Context, phase string) (store.ChatRetentionCursor, error) {
	row, err := s.client.ChatRetentionCursor.Get(ctx, phase)
	if ent.IsNotFound(err) {
		return store.ChatRetentionCursor{Phase: phase}, nil
	}
	if err != nil {
		return store.ChatRetentionCursor{}, fmt.Errorf("chat retention: get cursor: %w", err)
	}
	c := store.ChatRetentionCursor{
		Phase:     row.ID,
		UpdatedAt: row.UpdatedAt.UTC(),
	}
	if row.CursorCreated != nil {
		c.Position = store.ChatRetentionPosition{Created: row.CursorCreated.UTC(), ID: row.CursorID}
	}
	if row.ResetAt != nil {
		t := row.ResetAt.UTC()
		c.ResetAt = &t
	}
	if row.LastAppliedCutoff != nil {
		t := row.LastAppliedCutoff.UTC()
		c.LastAppliedCutoff = &t
	}
	return c, nil
}

// SaveChatRetentionCursor implements store.ChatRetentionStore.
func (s *ChatRetentionStore) SaveChatRetentionCursor(ctx context.Context, cursor store.ChatRetentionCursor) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	return s.upsertCursor(ctx, s.db, cursor)
}

// LatestChatRetentionRun implements store.ChatRetentionStore.
func (s *ChatRetentionStore) LatestChatRetentionRun(ctx context.Context, trigger string, since time.Time) (*store.ChatRetentionRun, error) {
	row, err := s.client.ChatRetentionRun.Query().
		Where(
			chatretentionrun.TriggerEQ(chatretentionrun.Trigger(trigger)),
			chatretentionrun.DryRun(false),
			chatretentionrun.StartedAtGTE(since.UTC()),
		).
		Order(ent.Desc(chatretentionrun.FieldStartedAt)).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("chat retention: latest run: %w", err)
	}
	return chatRetentionRunFromEnt(row)
}

func chatRetentionRunFromEnt(row *ent.ChatRetentionRun) (*store.ChatRetentionRun, error) {
	r := &store.ChatRetentionRun{
		ID:        row.ID.String(),
		Trigger:   string(row.Trigger),
		DryRun:    row.DryRun,
		StartedAt: row.StartedAt.UTC(),
		Status:    string(row.Status),
	}
	if row.FinishedAt != nil {
		t := row.FinishedAt.UTC()
		r.FinishedAt = &t
	}
	if row.Actor != nil {
		r.Actor = *row.Actor
	}
	if row.Error != nil {
		r.Error = *row.Error
	}
	if len(row.Policy) > 0 {
		if err := json.Unmarshal(row.Policy, &r.Policy); err != nil {
			return nil, fmt.Errorf("chat retention: run %s policy: %w", r.ID, err)
		}
	}
	if len(row.Counts) > 0 {
		if err := json.Unmarshal(row.Counts, &r.Counts); err != nil {
			return nil, fmt.Errorf("chat retention: run %s counts: %w", r.ID, err)
		}
	}
	return r, nil
}

// SaveChatRetentionRun implements store.ChatRetentionStore.
func (s *ChatRetentionStore) SaveChatRetentionRun(ctx context.Context, run *store.ChatRetentionRun) error {
	if run == nil {
		return errors.New("chat retention: nil run")
	}
	policy := run.Policy
	policy.MainCutoff = policy.MainCutoff.UTC()
	policyDoc, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("chat retention: encode run policy: %w", err)
	}
	counts := run.Counts
	if counts == nil {
		counts = map[string]store.ChatRetentionCounts{}
	}
	countsDoc, err := json.Marshal(counts)
	if err != nil {
		return fmt.Errorf("chat retention: encode run counts: %w", err)
	}
	var finished *time.Time
	if run.FinishedAt != nil {
		t := run.FinishedAt.UTC()
		finished = &t
	}
	status := run.Status
	if status == "" {
		status = store.ChatRetentionStatusRunning
	}

	if run.ID == "" {
		create := s.client.ChatRetentionRun.Create().
			SetTrigger(chatretentionrun.Trigger(run.Trigger)).
			SetDryRun(run.DryRun).
			SetStartedAt(run.StartedAt.UTC()).
			SetNillableFinishedAt(finished).
			SetStatus(chatretentionrun.Status(status)).
			SetPolicy(policyDoc).
			SetCounts(countsDoc)
		if run.Actor != "" {
			create.SetActor(run.Actor)
		}
		if run.Error != "" {
			create.SetError(run.Error)
		}
		row, err := create.Save(ctx)
		if err != nil {
			return fmt.Errorf("chat retention: create run: %w", err)
		}
		run.ID = row.ID.String()
		return nil
	}

	id, err := uuid.Parse(run.ID)
	if err != nil {
		return fmt.Errorf("chat retention: run id %q: %w", run.ID, err)
	}
	upd := s.client.ChatRetentionRun.UpdateOneID(id).
		SetNillableFinishedAt(finished).
		SetStatus(chatretentionrun.Status(status)).
		SetPolicy(policyDoc).
		SetCounts(countsDoc)
	if run.Error != "" {
		upd.SetError(run.Error)
	} else {
		upd.ClearError()
	}
	if err := upd.Exec(ctx); err != nil {
		return fmt.Errorf("chat retention: update run: %w", err)
	}
	return nil
}

// PruneChatRetentionRuns implements store.ChatRetentionStore.
func (s *ChatRetentionStore) PruneChatRetentionRuns(ctx context.Context, before time.Time) (int, error) {
	n, err := s.client.ChatRetentionRun.Delete().
		Where(chatretentionrun.StartedAtLT(before.UTC())).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("chat retention: prune runs: %w", err)
	}
	return n, nil
}
