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
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversation"
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
	// webchat_topic's time columns are TEXT on SQLite and TIMESTAMPTZ on
	// Postgres, as the web chat store creates them.
	timeType := "TEXT"
	if f.pg() {
		timeType = "TIMESTAMPTZ"
	}
	topicDDL := []string{
		`CREATE TABLE webchat_topic (
    id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL, conversation_id TEXT,
    created_by TEXT NOT NULL, created_at ` + timeType + ` NOT NULL, deleted_at ` + timeType + `)`,
		`CREATE INDEX idx_webchat_topic_deleted ON webchat_topic (deleted_at) WHERE deleted_at IS NOT NULL`,
	}
	for _, ddl := range append(append([]string{}, chatRetentionTestDDL...), topicDDL...) {
		_, err := f.db.ExecContext(f.ctx, ddl)
		require.NoError(t, err, ddl)
	}
	return f
}

// webchatTime is the stored form of t in a webchat_topic time column.
func (f *chatRetentionFixture) webchatTime(t time.Time) any {
	if f.pg() {
		return t.UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// topic stores a web chat topic; conv "" stores a NULL conversation_id and
// a zero deleted a live topic.
func (f *chatRetentionFixture) topic(id, conv string, deleted time.Time) {
	f.t.Helper()
	var convArg, deletedArg any
	if conv != "" {
		convArg = conv
	}
	if !deleted.IsZero() {
		deletedArg = f.webchatTime(deleted)
	}
	_, err := f.db.ExecContext(f.ctx, f.q(`INSERT INTO webchat_topic (id, project_id, name, conversation_id, created_by, created_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		id, uuid.NewString(), id, convArg, "u1", f.webchatTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)), deletedArg)
	require.NoError(f.t, err)
}

// conversation stores a conversation, soft-deleted at deleted unless it is
// zero.
func (f *chatRetentionFixture) conversation(deleted time.Time) string {
	f.t.Helper()
	c := f.client.Conversation.Create().
		SetKind(conversation.KindGroup).
		SetSurface(conversation.SurfaceNative)
	if !deleted.IsZero() {
		c.SetDeletedAt(deleted)
	}
	row, err := c.Save(f.ctx)
	require.NoError(f.t, err)
	return row.ID.String()
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
	thread  string
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
	if m.thread != "" {
		c.SetThreadID(m.thread)
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
	assert.Equal(t, []store.ChatRetentionAttachment{{ID: "att-only-m1", ProjectID: p1.String(), Size: 1}}, res.UnlinkedAttachments)

	assert.Equal(t, noOrphans(), f.orphans())
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM messages`))
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_mention", "webchat_message_star"} {
		assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM `+table), table)
	}
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM webchat_message_attachment WHERE message_id = ?`, keep))
	// The purge leaves attachment metadata; the caller deletes the bytes
	// and then the row.
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
// the purge fails (here the unlinked-attachment read, after the messages
// were deleted, because webchat_attachment is gone), nothing is deleted.
// This is the single-transaction property of design Open Question 1: the
// ent tables and the web chat tables share one *sql.DB.
func TestChatRetentionStore_PurgeIsOneTransaction(t *testing.T) {
	f := newChatRetentionFixture(t)
	p := uuid.New()
	m := f.message(chatRetentionMsg{project: p, created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	f.dependents(m, p)
	f.attachment("att-m", p, m)
	_, err := f.db.ExecContext(f.ctx, `DROP TABLE webchat_attachment`)
	require.NoError(t, err)

	_, err = f.st.PurgeChatMessages(f.ctx, []string{m}, map[string]string{m: p.String()},
		store.ChatRetentionCursor{Phase: store.ChatRetentionPhaseMain, UpdatedAt: time.Now()})
	require.Error(t, err)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM messages`))
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_mention", "webchat_message_star"} {
		assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM `+table), table)
	}
}

// TestChatRetentionStore_PurgeWithoutCursor: a purge whose cursor has no
// phase (the deleted-conversation phase) deletes the messages and stores
// no cursor row.
func TestChatRetentionStore_PurgeWithoutCursor(t *testing.T) {
	f := newChatRetentionFixture(t)
	p := uuid.New()
	m := f.message(chatRetentionMsg{project: p, created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	f.dependents(m, p)

	res, err := f.st.PurgeChatMessages(f.ctx, []string{m}, map[string]string{m: p.String()}, store.ChatRetentionCursor{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Total().Main)
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM messages`))
	assert.Equal(t, noOrphans(), f.orphans())
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM chat_retention_cursors`))
}

// TestChatRetentionStore_DeletedConversationsAndTopics: only
// conversations and topics soft-deleted strictly before the cutoff are
// listed, in ID order, in pages resumed after the last ID; a legacy topic
// lists with no conversation.
func TestChatRetentionStore_DeletedConversationsAndTopics(t *testing.T) {
	f := newChatRetentionFixture(t)
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	var oldConvs []string
	for i := 0; i < 3; i++ {
		oldConvs = append(oldConvs, f.conversation(cutoff.Add(-time.Duration(i+1)*24*time.Hour)))
	}
	sort.Strings(oldConvs)
	_ = f.conversation(cutoff.Add(time.Hour)) // deleted inside the grace
	_ = f.conversation(time.Time{})           // live

	got, err := f.st.ListChatRetentionDeletedConversations(f.ctx, cutoff, "", 100)
	require.NoError(t, err)
	require.Equal(t, oldConvs, got)
	page, err := f.st.ListChatRetentionDeletedConversations(f.ctx, cutoff, "", 2)
	require.NoError(t, err)
	require.Equal(t, oldConvs[:2], page)
	page, err = f.st.ListChatRetentionDeletedConversations(f.ctx, cutoff, page[1], 2)
	require.NoError(t, err)
	require.Equal(t, oldConvs[2:], page)

	f.topic("t-a-linked", "conv-a", cutoff.Add(-48*time.Hour))
	f.topic("t-b-legacy", "", cutoff.Add(-time.Minute))
	f.topic("t-c-recent", "conv-c", cutoff.Add(time.Minute))
	f.topic("t-d-live", "conv-d", time.Time{})

	topics, err := f.st.ListChatRetentionDeletedTopics(f.ctx, cutoff, "", 100)
	require.NoError(t, err)
	require.Equal(t, []store.ChatRetentionDeletedTopic{{ID: "t-a-linked", ConversationID: "conv-a"}, {ID: "t-b-legacy"}}, topics)
	topics, err = f.st.ListChatRetentionDeletedTopics(f.ctx, cutoff, "t-a-linked", 100)
	require.NoError(t, err)
	require.Equal(t, []store.ChatRetentionDeletedTopic{{ID: "t-b-legacy"}}, topics)
}

// TestChatRetentionStore_DeletedCandidatePlans: the deleted-candidate
// page queries, first page and resumed, are served by the partial
// deleted_at indexes and never scan the table (design §6.4: no
// whole-table work). SQLite's plan is asserted. Postgres plans by cost, so
// a test-sized table says little about a real one: its plan is logged.
func TestChatRetentionStore_DeletedCandidatePlans(t *testing.T) {
	f := newChatRetentionFixture(t)
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		_ = f.conversation(time.Time{})
		f.topic(fmt.Sprintf("live-%02d", i), "", time.Time{})
	}
	_ = f.conversation(cutoff.Add(-48 * time.Hour))
	f.topic("gone", "", cutoff.Add(-48*time.Hour))

	for _, tc := range []struct {
		name, cols, table, index string
		cutoffArg                any
	}{
		{"conversations", "id", "conversations", "conversation_deleted_at", cutoff.UTC()},
		{"topics", "id, conversation_id", "webchat_topic", "idx_webchat_topic_deleted", f.webchatTime(cutoff)},
	} {
		for _, withAfter := range []bool{false, true} {
			args := []any{tc.cutoffArg}
			if withAfter {
				args = append(args, "00000000-0000-0000-0000-000000000000")
			}
			args = append(args, 100)
			query := f.st.deletedCandidatesQuery(tc.cols, tc.table, withAfter)
			name := fmt.Sprintf("%s after=%t", tc.name, withAfter)
			if f.pg() {
				rows, err := f.db.QueryContext(f.ctx, "EXPLAIN "+query, args...)
				require.NoError(t, err, name)
				var plan []string
				for rows.Next() {
					var line string
					require.NoError(t, rows.Scan(&line))
					plan = append(plan, line)
				}
				require.NoError(t, rows.Close())
				t.Logf("%s plan (Postgres):\n%s", name, strings.Join(plan, "\n"))
				continue
			}
			rows, err := f.db.QueryContext(f.ctx, "EXPLAIN QUERY PLAN "+query, args...)
			require.NoError(t, err, name)
			var details []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Close())
			plan := strings.Join(details, " | ")
			assert.Contains(t, plan, "INDEX "+tc.index, "%s: plan %q", name, plan)
			for _, d := range details {
				if strings.HasPrefix(d, "SCAN "+tc.table) {
					assert.Contains(t, d, tc.index, "%s: a table scan %q", name, d)
				}
			}
		}
	}

	// The queries still return the deleted rows in ID order.
	ids, err := f.st.ListChatRetentionDeletedConversations(f.ctx, cutoff, "", 100)
	require.NoError(t, err)
	assert.Len(t, ids, 1)
	topics, err := f.st.ListChatRetentionDeletedTopics(f.ctx, cutoff, "", 100)
	require.NoError(t, err)
	assert.Equal(t, []store.ChatRetentionDeletedTopic{{ID: "gone"}}, topics)
}

// TestChatRetentionStore_SelectConversationMessages: a deleted
// conversation's messages are selected by conversation_id, and a legacy
// topic's by thread_id when they have no conversation_id; pending and
// deferred messages are skipped; the limit spans both arms.
func TestChatRetentionStore_SelectConversationMessages(t *testing.T) {
	f := newChatRetentionFixture(t)
	project := uuid.New()
	conv, other := uuid.New(), uuid.New()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	byConv := []string{
		f.message(chatRetentionMsg{project: project, created: at, conv: &conv, thread: "topic-1"}),
		f.message(chatRetentionMsg{project: project, created: at, conv: &conv, state: "failed"}),
	}
	_ = f.message(chatRetentionMsg{project: project, created: at, conv: &conv, state: "pending"})
	_ = f.message(chatRetentionMsg{project: project, created: at, conv: &conv, state: "deferred"})
	byThread := []string{
		f.message(chatRetentionMsg{project: project, created: at, thread: "topic-1"}),
		f.message(chatRetentionMsg{project: project, created: at, thread: "topic-1"}),
	}
	_ = f.message(chatRetentionMsg{project: project, created: at, thread: "topic-1", state: "pending"})
	_ = f.message(chatRetentionMsg{project: project, created: at, conv: &other, thread: "topic-1"}) // re-keyed elsewhere
	_ = f.message(chatRetentionMsg{project: project, created: at, conv: &other})
	_ = f.message(chatRetentionMsg{project: project, created: at, thread: "topic-2"})

	sorted := func(ids []string) []string {
		out := append([]string{}, ids...)
		sort.Strings(out)
		return out
	}
	got, err := f.st.SelectChatRetentionConversationMessages(f.ctx, conv.String(), "", 100)
	require.NoError(t, err)
	assert.Equal(t, sorted(byConv), sorted(candidateIDs(got)))
	assert.Equal(t, project.String(), got[0].ProjectID)

	got, err = f.st.SelectChatRetentionConversationMessages(f.ctx, "", "topic-1", 100)
	require.NoError(t, err)
	assert.Equal(t, sorted(byThread), sorted(candidateIDs(got)))

	got, err = f.st.SelectChatRetentionConversationMessages(f.ctx, conv.String(), "topic-1", 100)
	require.NoError(t, err)
	assert.Equal(t, sorted(append(append([]string{}, byConv...), byThread...)), sorted(candidateIDs(got)))

	got, err = f.st.SelectChatRetentionConversationMessages(f.ctx, conv.String(), "topic-1", 3)
	require.NoError(t, err)
	assert.Len(t, got, 3, "the limit spans both arms")

	got, err = f.st.SelectChatRetentionConversationMessages(f.ctx, uuid.NewString(), "no-such-topic", 100)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestChatRetentionStore_UnlinkedAttachmentRow: an attachment row is
// deleted only while no message links it.
func TestChatRetentionStore_UnlinkedAttachmentRow(t *testing.T) {
	f := newChatRetentionFixture(t)
	p := uuid.New()
	m := f.message(chatRetentionMsg{project: p, created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	f.attachment("att-linked", p, m)
	f.attachment("att-free", p)

	linked, err := f.st.ChatAttachmentLinked(f.ctx, "att-linked")
	require.NoError(t, err)
	assert.True(t, linked)
	linked, err = f.st.ChatAttachmentLinked(f.ctx, "att-free")
	require.NoError(t, err)
	assert.False(t, linked)

	deleted, err := f.st.DeleteUnlinkedChatAttachment(f.ctx, "att-linked")
	require.NoError(t, err)
	assert.False(t, deleted, "a linked attachment keeps its row")
	deleted, err = f.st.DeleteUnlinkedChatAttachment(f.ctx, "att-free")
	require.NoError(t, err)
	assert.True(t, deleted)
	deleted, err = f.st.DeleteUnlinkedChatAttachment(f.ctx, "att-free")
	require.NoError(t, err)
	assert.False(t, deleted, "idempotent")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM webchat_attachment`))
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
