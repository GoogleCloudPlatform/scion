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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/messageaddressee"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// The TestChatRetentionStore_ suite runs on SQLite by default and on
// Postgres in the Postgres store CI job (make test-launch-store-postgres
// runs the full entadapter suite), so both dialects run the same tests.
//
// The web chat tables are created by pkg/hub (webchannel_store*.go), which
// this package cannot import; chatRetentionTestDDL reproduces the columns
// and message_id indexes the purge uses. The pkg/hub chat retention tests
// run the purge against the real tables.

// chatRetentionTestDDL is the subset of the web chat schema the purge
// touches. TEXT columns are valid on both dialects.
var chatRetentionTestDDL = []string{
	`CREATE TABLE webchat_attachment (
    id TEXT PRIMARY KEY, project_id TEXT NOT NULL, filename TEXT NOT NULL,
    mime_type TEXT NOT NULL, size BIGINT NOT NULL, uploaded_by TEXT NOT NULL, created_at TEXT NOT NULL)`,
	`CREATE TABLE webchat_message_attachment (
    message_id TEXT NOT NULL, attachment_id TEXT NOT NULL, PRIMARY KEY (message_id, attachment_id))`,
	`CREATE INDEX idx_webchat_message_attachment_message ON webchat_message_attachment (message_id)`,
	`CREATE INDEX idx_webchat_message_attachment_attachment ON webchat_message_attachment (attachment_id)`,
	`CREATE TABLE webchat_message_ext (
    message_id TEXT PRIMARY KEY, reply_to_id TEXT, edited_at TEXT, deleted_at TEXT)`,
	`CREATE TABLE webchat_mention (
    user_id TEXT NOT NULL, conversation_key TEXT NOT NULL, message_id TEXT NOT NULL, PRIMARY KEY (message_id, user_id))`,
	`CREATE TABLE webchat_message_star (
    user_id TEXT NOT NULL, message_id TEXT NOT NULL, conversation_key TEXT NOT NULL,
    project_id TEXT, starred_at TEXT NOT NULL, PRIMARY KEY (user_id, message_id))`,
	`CREATE INDEX idx_webchat_message_star_message ON webchat_message_star (message_id)`,
}

type chatRetentionFixture struct {
	t      *testing.T
	ctx    context.Context
	client *ent.Client
	db     *sql.DB
	st     *ChatRetentionStore
}

func newChatRetentionFixture(t *testing.T) *chatRetentionFixture {
	t.Helper()
	client := enttest.NewClient(t)
	f := &chatRetentionFixture{t: t, ctx: context.Background(), client: client, db: clientDB(client), st: NewChatRetentionStore(client)}
	require.NotNil(t, f.db)
	for _, ddl := range chatRetentionTestDDL {
		_, err := f.db.ExecContext(f.ctx, ddl)
		require.NoError(t, err, ddl)
	}
	return f
}

func (f *chatRetentionFixture) pg() bool { return f.client.Driver().Dialect() == dialect.Postgres }

// q rewrites ? placeholders for the fixture's dialect.
func (f *chatRetentionFixture) q(query string) string { return f.st.rebind(query) }

type chatRetentionMsg struct {
	project uuid.UUID
	created time.Time
	state   string
	channel string
	conv    *uuid.UUID
}

func (f *chatRetentionFixture) message(m chatRetentionMsg) string {
	f.t.Helper()
	if m.state == "" {
		m.state = "delivered"
	}
	c := f.client.Message.Create().
		SetProjectID(m.project).
		SetSender("user:alice").
		SetRecipient("agent:bot").
		SetMsg("hello").
		SetDispatchState(m.state).
		SetCreated(m.created)
	if m.channel != "" {
		c.SetChannel(m.channel)
	}
	if m.conv != nil {
		c.SetConversationID(*m.conv)
	}
	row, err := c.Save(f.ctx)
	require.NoError(f.t, err)
	return row.ID.String()
}

// dependents writes one row in every dependent table for message id.
func (f *chatRetentionFixture) dependents(id string, project uuid.UUID) {
	f.t.Helper()
	mid := uuid.MustParse(id)
	_, err := f.client.MessageAddressee.Create().
		SetMessageID(mid).SetPrincipalKind(messageaddressee.PrincipalKindUser).
		SetPrincipalID("u-" + id).SetVia(messageaddressee.ViaExplicit).Save(f.ctx)
	require.NoError(f.t, err)
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO webchat_message_ext (message_id, reply_to_id) VALUES (?, ?)`, []any{id, nil}},
		{`INSERT INTO webchat_mention (user_id, conversation_key, message_id) VALUES (?, ?, ?)`, []any{"u1", "topic:x", id}},
		{`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, ?, ?)`, []any{"u1", id, "topic:x", project.String(), "2026-01-01T00:00:00Z"}},
	} {
		_, err := f.db.ExecContext(f.ctx, f.q(stmt.q), stmt.args...)
		require.NoError(f.t, err, stmt.q)
	}
}

func (f *chatRetentionFixture) attachment(id string, project uuid.UUID, messageIDs ...string) {
	f.t.Helper()
	_, err := f.db.ExecContext(f.ctx, f.q(`INSERT INTO webchat_attachment (id, project_id, filename, mime_type, size, uploaded_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		id, project.String(), "f.txt", "text/plain", 1, "u1", "2026-01-01T00:00:00Z")
	require.NoError(f.t, err)
	for _, mid := range messageIDs {
		_, err := f.db.ExecContext(f.ctx, f.q(`INSERT INTO webchat_message_attachment (message_id, attachment_id) VALUES (?, ?)`), mid, id)
		require.NoError(f.t, err)
	}
}

func (f *chatRetentionFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRowContext(f.ctx, f.q(query), args...).Scan(&n), query)
	return n
}

// orphans counts the rows of every dependent table that reference a
// message that does not exist: the cascade completeness property.
func (f *chatRetentionFixture) orphans() map[string]int {
	f.t.Helper()
	out := map[string]int{}
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_mention", "webchat_message_attachment", "webchat_message_star"} {
		col := "message_id"
		out[table] = f.count(`SELECT COUNT(*) FROM ` + table + ` d WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE CAST(m.id AS TEXT) = CAST(d.` + col + ` AS TEXT))`)
	}
	return out
}

func noOrphans() map[string]int {
	return map[string]int{"message_addressees": 0, "webchat_message_ext": 0, "webchat_mention": 0, "webchat_message_attachment": 0, "webchat_message_star": 0}
}

func candidateIDs(cs []store.ChatRetentionCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// TestChatRetentionStore_SelectCandidates: only messages created strictly
// before the cutoff are returned, oldest first, never pending or deferred;
// rows without a conversation and of any channel are included; ties on
// created are ordered by id and resumed by the cursor.
func TestChatRetentionStore_SelectCandidates(t *testing.T) {
	f := newChatRetentionFixture(t)
	project := uuid.New()
	conv := uuid.New()
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	oldWeb := f.message(chatRetentionMsg{project: project, created: cutoff.Add(-72 * time.Hour), channel: "web", conv: &conv})
	_ = f.message(chatRetentionMsg{project: project, created: cutoff.Add(-71 * time.Hour), state: "pending"})
	_ = f.message(chatRetentionMsg{project: project, created: cutoff.Add(-70 * time.Hour), state: "deferred"})
	oldFailed := f.message(chatRetentionMsg{project: project, created: cutoff.Add(-69 * time.Hour), state: "failed"})
	oldNoConv := f.message(chatRetentionMsg{project: project, created: cutoff.Add(-68 * time.Hour)})
	oldDiscord := f.message(chatRetentionMsg{project: project, created: cutoff.Add(-67 * time.Hour), channel: "discord"})
	tieAt := cutoff.Add(-time.Hour)
	tieA := f.message(chatRetentionMsg{project: project, created: tieAt})
	tieB := f.message(chatRetentionMsg{project: project, created: tieAt})
	_ = f.message(chatRetentionMsg{project: project, created: cutoff})                  // boundary: not older
	_ = f.message(chatRetentionMsg{project: project, created: cutoff.Add(time.Second)}) // newer

	ties := []string{tieA, tieB}
	sort.Strings(ties)
	if f.pg() {
		// uuid order on Postgres is byte order, the same as the canonical
		// lower-case text order.
		sort.Strings(ties)
	}
	want := []string{oldWeb, oldFailed, oldNoConv, oldDiscord, ties[0], ties[1]}

	got, err := f.st.SelectChatRetentionCandidates(f.ctx, cutoff, store.ChatRetentionPosition{}, 100)
	require.NoError(t, err)
	require.Equal(t, want, candidateIDs(got))
	assert.Equal(t, conv.String(), got[0].ConversationID)
	assert.Equal(t, "", got[2].ConversationID)
	assert.Equal(t, project.String(), got[0].ProjectID)
	assert.True(t, got[0].Created.Equal(cutoff.Add(-72*time.Hour)), "created %v", got[0].Created)

	// The limit bounds the chunk.
	got, err = f.st.SelectChatRetentionCandidates(f.ctx, cutoff, store.ChatRetentionPosition{}, 2)
	require.NoError(t, err)
	require.Equal(t, want[:2], candidateIDs(got))

	// Resuming after the first tie returns only the second; after the last
	// row, nothing.
	got, err = f.st.SelectChatRetentionCandidates(f.ctx, cutoff, store.ChatRetentionPosition{Created: tieAt, ID: ties[0]}, 100)
	require.NoError(t, err)
	require.Equal(t, []string{ties[1]}, candidateIDs(got))
	got, err = f.st.SelectChatRetentionCandidates(f.ctx, cutoff, store.ChatRetentionPosition{Created: tieAt, ID: ties[1]}, 100)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestChatRetentionStore_PurgeCascade: one purge deletes the messages and
// every dependent row, leaves no orphan, keeps the survivors' rows and
// unlinked attachment metadata, reports per-project counts and the
// unlinked attachments, stores the cursor, and is idempotent.
func TestChatRetentionStore_PurgeCascade(t *testing.T) {
	f := newChatRetentionFixture(t)
	p1, p2 := uuid.New(), uuid.New()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	m1 := f.message(chatRetentionMsg{project: p1, created: old})
	m2 := f.message(chatRetentionMsg{project: p2, created: old.Add(time.Minute)})
	keep := f.message(chatRetentionMsg{project: p1, created: old.Add(2 * time.Minute)})
	for _, id := range []string{m1, m2, keep} {
		pid := p1
		if id == m2 {
			pid = p2
		}
		f.dependents(id, pid)
	}
	f.attachment("att-only-m1", p1, m1)
	f.attachment("att-shared", p1, m1, keep)

	cursor := store.ChatRetentionCursor{
		Phase:     store.ChatRetentionPhaseMain,
		Position:  store.ChatRetentionPosition{Created: old.Add(time.Minute), ID: m2},
		UpdatedAt: time.Now(),
	}
	res, err := f.st.PurgeChatMessages(f.ctx, []string{m1, m2}, map[string]string{m1: p1.String(), m2: p2.String()}, cursor)
	require.NoError(t, err)

	assert.Equal(t, store.ChatRetentionCounts{Main: 1, Addressees: 1, Mentions: 1, Stars: 1, Ext: 1, Attachments: 2, UnlinkedAttachments: 1}, res.ByProject[p1.String()])
	assert.Equal(t, store.ChatRetentionCounts{Main: 1, Addressees: 1, Mentions: 1, Stars: 1, Ext: 1}, res.ByProject[p2.String()])
	assert.Equal(t, []string{"att-only-m1"}, res.UnlinkedAttachmentIDs)

	assert.Equal(t, noOrphans(), f.orphans())
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM messages`))
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_mention", "webchat_message_star"} {
		assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM `+table), table)
	}
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM webchat_message_attachment WHERE message_id = ?`, keep))
	// Phase 1 leaves attachment metadata for the attachment GC.
	assert.Equal(t, 2, f.count(`SELECT COUNT(*) FROM webchat_attachment`))

	got, err := f.st.GetChatRetentionCursor(f.ctx, store.ChatRetentionPhaseMain)
	require.NoError(t, err)
	assert.Equal(t, m2, got.Position.ID)
	assert.True(t, got.Position.Created.Equal(old.Add(time.Minute)))

	// Idempotent: a second purge of the same IDs deletes nothing.
	res, err = f.st.PurgeChatMessages(f.ctx, []string{m1, m2}, nil, cursor)
	require.NoError(t, err)
	assert.Equal(t, store.ChatRetentionCounts{}, res.Total())
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM messages`))
}

// TestChatRetentionStore_PurgeIsOneTransaction: when a later statement of
// the purge fails (here the cursor write), nothing is deleted. This is the
// single-transaction property of design Open Question 1: the ent tables
// and the web chat tables share one *sql.DB.
func TestChatRetentionStore_PurgeIsOneTransaction(t *testing.T) {
	f := newChatRetentionFixture(t)
	p := uuid.New()
	m := f.message(chatRetentionMsg{project: p, created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	f.dependents(m, p)

	_, err := f.st.PurgeChatMessages(f.ctx, []string{m}, map[string]string{m: p.String()}, store.ChatRetentionCursor{})
	require.Error(t, err)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM messages`))
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_mention", "webchat_message_star"} {
		assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM `+table), table)
	}
}

// TestChatRetentionStore_CursorRoundTrip: an absent cursor reads as the
// start; every field round-trips; nil fields clear stored values.
func TestChatRetentionStore_CursorRoundTrip(t *testing.T) {
	f := newChatRetentionFixture(t)
	got, err := f.st.GetChatRetentionCursor(f.ctx, store.ChatRetentionPhaseMain)
	require.NoError(t, err)
	assert.Equal(t, store.ChatRetentionPhaseMain, got.Phase)
	assert.True(t, got.Position.IsStart())
	assert.Nil(t, got.ResetAt)
	assert.Nil(t, got.LastAppliedCutoff)

	created := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
	reset := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	require.NoError(t, f.st.SaveChatRetentionCursor(f.ctx, store.ChatRetentionCursor{
		Phase: store.ChatRetentionPhaseMain, Position: store.ChatRetentionPosition{Created: created, ID: id},
		ResetAt: &reset, LastAppliedCutoff: &cutoff, UpdatedAt: reset,
	}))
	got, err = f.st.GetChatRetentionCursor(f.ctx, store.ChatRetentionPhaseMain)
	require.NoError(t, err)
	assert.True(t, got.Position.Created.Equal(created), "created %v", got.Position.Created)
	assert.Equal(t, id, got.Position.ID)
	require.NotNil(t, got.ResetAt)
	assert.True(t, got.ResetAt.Equal(reset))
	require.NotNil(t, got.LastAppliedCutoff)
	assert.True(t, got.LastAppliedCutoff.Equal(cutoff))

	// Back to the start, keeping the cutoff.
	require.NoError(t, f.st.SaveChatRetentionCursor(f.ctx, store.ChatRetentionCursor{
		Phase: store.ChatRetentionPhaseMain, ResetAt: &reset, LastAppliedCutoff: &cutoff, UpdatedAt: reset,
	}))
	got, err = f.st.GetChatRetentionCursor(f.ctx, store.ChatRetentionPhaseMain)
	require.NoError(t, err)
	assert.True(t, got.Position.IsStart(), "position %+v", got.Position)
	require.NotNil(t, got.LastAppliedCutoff)
}

// TestChatRetentionStore_Runs: a run is inserted, found as the latest
// since a time, updated in place, and pruned by age.
func TestChatRetentionStore_Runs(t *testing.T) {
	f := newChatRetentionFixture(t)
	start := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	none, err := f.st.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, start.Add(-time.Hour))
	require.NoError(t, err)
	require.Nil(t, none)

	run := &store.ChatRetentionRun{Trigger: store.ChatRetentionTriggerSweep, StartedAt: start, Status: store.ChatRetentionStatusRunning}
	require.NoError(t, f.st.SaveChatRetentionRun(f.ctx, run))
	require.NotEmpty(t, run.ID)

	fin := start.Add(time.Minute)
	run.FinishedAt = &fin
	run.Status = store.ChatRetentionStatusOK
	run.Policy = store.ChatRetentionPolicy{MessagesRetentionDays: 30, MainCutoff: start.Add(-30 * 24 * time.Hour)}
	run.Counts = map[string]store.ChatRetentionCounts{"p1": {Main: 5, Mentions: 2}}
	require.NoError(t, f.st.SaveChatRetentionRun(f.ctx, run))

	got, err := f.st.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, start.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, run.ID, got.ID)
	assert.Equal(t, store.ChatRetentionStatusOK, got.Status)
	assert.Equal(t, 30, got.Policy.MessagesRetentionDays)
	assert.True(t, got.Policy.MainCutoff.Equal(run.Policy.MainCutoff))
	assert.Equal(t, run.Counts, got.Counts)
	require.NotNil(t, got.FinishedAt)
	assert.True(t, got.FinishedAt.Equal(fin))

	later, err := f.st.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, start.Add(time.Second))
	require.NoError(t, err)
	assert.Nil(t, later, "the run started before since")

	n, err := f.st.PruneChatRetentionRuns(f.ctx, start)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a run started at the bound is kept")
	n, err = f.st.PruneChatRetentionRuns(f.ctx, start.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestChatRetentionStore_NoAddresseeForeignKey records the answer to design
// Open Question 2: message_addressees.message_id has no foreign key on
// either dialect (the ent schema has no edge), so the explicit delete in the
// purge is what removes addressee rows.
func TestChatRetentionStore_NoAddresseeForeignKey(t *testing.T) {
	f := newChatRetentionFixture(t)
	if f.pg() {
		assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM information_schema.table_constraints
 WHERE table_schema = current_schema() AND table_name = 'message_addressees' AND constraint_type = 'FOREIGN KEY'`))
		return
	}
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM pragma_foreign_key_list('message_addressees')`))
}
