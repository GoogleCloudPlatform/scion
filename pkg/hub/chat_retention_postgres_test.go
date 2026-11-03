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

//go:build integration

package hub

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestChatRetention_Postgres_CascadeAndReplicaRace runs the chat retention
// purge on Postgres against the real web chat tables, from two hub
// replicas' stores at once (as two replicas would race on a chunk without
// the singleton lock): exactly the eligible messages are purged, no
// dependent row is orphaned, and neither sweep fails.
func TestChatRetention_Postgres_CascadeAndReplicaRace(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t)
	_ = stdlib.GetDefaultDriver() // registers the "pgx" driver

	open := func() (*entadapter.CompositeStore, *sql.DB) {
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
		require.NoError(t, err)
		cs := entadapter.NewCompositeStore(client)
		t.Cleanup(func() { _ = cs.Close() })
		require.NoError(t, cs.Migrate(ctx))
		return cs, cs.DB()
	}
	csA, db := open()
	csB, _ := open()
	require.NoError(t, NewWebChatStore(db, "postgres").Init())

	project := api.NewUUID()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	exec := func(q string, args ...any) {
		_, err := db.ExecContext(ctx, q, args...)
		require.NoError(t, err, q)
	}
	seed := func(created time.Time, state, channel, conv string) string {
		m := &store.Message{ID: api.NewUUID(), ProjectID: project, Sender: "user:a", Recipient: "agent:b", Msg: "hello",
			Type: "chat", Channel: channel, ConversationID: conv, DispatchState: state, CreatedAt: created}
		require.NoError(t, csA.CreateMessage(ctx, m))
		exec(`INSERT INTO message_addressees (id, message_id, principal_kind, principal_id, via, delivery_state) VALUES ($1, $2, 'user', 'u1', 'explicit', 'delivered')`, api.NewUUID(), m.ID)
		exec(`INSERT INTO webchat_message_ext (message_id) VALUES ($1)`, m.ID)
		exec(`INSERT INTO webchat_mention (user_id, conversation_key, message_id) VALUES ('u1', 'topic:x', $1)`, m.ID)
		exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES ('u1', $1, 'topic:x', $2, now())`, m.ID, project)
		att := api.NewUUID()
		exec(`INSERT INTO webchat_attachment (id, project_id, filename, mime_type, size, uploaded_by) VALUES ($1, $2, 'f', 'text/plain', 1, 'u1')`, att, project)
		exec(`INSERT INTO webchat_message_attachment (message_id, attachment_id) VALUES ($1, $2)`, m.ID, att)
		return m.ID
	}
	conv := api.NewUUID()
	var purge []string
	for i := 0; i < 120; i++ {
		ch, cv := "web", conv
		if i%3 == 1 {
			ch, cv = "", "" // agent-to-agent, no conversation
		}
		purge = append(purge, seed(old.Add(time.Duration(i)*time.Second), store.MessageDispatchDispatched, ch, cv))
	}
	keep := []string{
		seed(old, store.MessageDispatchPending, "web", conv),
		seed(old, store.MessageDispatchDeferred, "web", conv),
		seed(now.Add(-time.Hour), store.MessageDispatchDispatched, "web", conv),
	}

	cfg := opsettings.DefaultChatRetentionConfig()
	cfg.Enabled, cfg.MessagesRetentionDays, cfg.ChunkSize, cfg.PauseMS = true, 30, 25, 0
	policy := newChatRetentionPolicy(true, cfg, now)

	var wg sync.WaitGroup
	results := make([]chatRetentionTickResult, 2)
	errs := make([]error, 2)
	for i, cs := range []*entadapter.CompositeStore{csA, csB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = newChatRetentionSweeper(cs.ChatRetention()).RunTick(ctx, policy)
		}()
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, int64(len(purge)), results[0].Purged.Main+results[1].Purged.Main, "each message is counted once")

	count := func(q string, args ...any) int {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, q, args...).Scan(&n), q)
		return n
	}
	assert.Equal(t, len(keep), count(`SELECT COUNT(*) FROM messages`))
	for _, id := range keep {
		assert.Equal(t, 1, count(`SELECT COUNT(*) FROM messages WHERE id = $1`, id))
	}
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_mention", "webchat_message_attachment", "webchat_message_star"} {
		assert.Equal(t, 0, count(`SELECT COUNT(*) FROM `+table+` d WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE CAST(m.id AS TEXT) = CAST(d.message_id AS TEXT))`), table)
		assert.Equal(t, len(keep), count(`SELECT COUNT(*) FROM `+table), table)
	}
	assert.Equal(t, len(purge)+len(keep), count(`SELECT COUNT(*) FROM webchat_attachment`), "Phase 1 keeps attachment metadata")
	assert.Equal(t, 0, count(`SELECT COUNT(*) FROM information_schema.table_constraints
 WHERE table_schema = current_schema() AND table_name = 'message_addressees' AND constraint_type = 'FOREIGN KEY'`),
		"message_addressees has no foreign key (design Open Question 2)")

	cur, err := csA.ChatRetention().GetChatRetentionCursor(ctx, store.ChatRetentionPhaseMain)
	require.NoError(t, err)
	require.NotNil(t, cur.LastAppliedCutoff)
}

// TestChatRetention_Postgres_DeletedConversationsAndAttachments runs the
// deleted-conversation phase and the attachment byte delete on Postgres
// against the real tables (TIMESTAMPTZ topic times, uuid conversation
// IDs): with no window, the messages of a conversation, of a legacy topic
// and of a linked topic (by conversation and by thread_id) deleted more
// than the grace ago are purged with their dependent rows; an attachment
// left unlinked loses its bytes and then its row, and one whose byte
// delete fails keeps its row. It also checks that an unread
// mention after a purged read watermark still counts (design §8.5).
func TestChatRetention_Postgres_DeletedConversationsAndAttachments(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t)
	_ = stdlib.GetDefaultDriver() // registers the "pgx" driver
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, cs.Migrate(ctx))
	db := cs.DB()
	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())

	project := api.NewUUID()
	now := time.Now().UTC()
	day := 24 * time.Hour
	exec := func(q string, args ...any) {
		_, err := db.ExecContext(ctx, q, args...)
		require.NoError(t, err, q)
	}
	count := func(q string, args ...any) int {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, q, args...).Scan(&n), q)
		return n
	}
	seed := func(created time.Time, state, conv, thread string) string {
		m := &store.Message{ID: api.NewUUID(), ProjectID: project, Sender: "user:a", Recipient: "agent:b", Msg: "hello",
			Type: "chat", Channel: "web", ConversationID: conv, ThreadID: thread, DispatchState: state, CreatedAt: created}
		require.NoError(t, cs.CreateMessage(ctx, m))
		exec(`INSERT INTO message_addressees (id, message_id, principal_kind, principal_id, via, delivery_state) VALUES ($1, $2, 'user', 'u1', 'explicit', 'delivered')`, api.NewUUID(), m.ID)
		exec(`INSERT INTO webchat_message_ext (message_id) VALUES ($1)`, m.ID)
		exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES ('u1', $1, 'topic:x', $2, now())`, m.ID, project)
		return m.ID
	}
	attach := func(messageID string) string {
		att := api.NewUUID()
		exec(`INSERT INTO webchat_attachment (id, project_id, filename, mime_type, size, uploaded_by) VALUES ($1, $2, 'f', 'text/plain', 10, 'u1')`, att, project)
		exec(`INSERT INTO webchat_message_attachment (message_id, attachment_id) VALUES ($1, $2)`, messageID, att)
		return att
	}
	pid := project
	deleted := now.Add(-40 * day)
	goneConv := &store.Conversation{ID: api.NewUUID(), ProjectID: &pid, Kind: "group", Surface: "native",
		DriftState: "active", LastActivityAt: deleted, CreatedAt: deleted.Add(-time.Hour), DeletedAt: &deleted}
	require.NoError(t, cs.CreateConversation(ctx, goneConv))
	legacyTopic := api.NewUUID()
	exec(`INSERT INTO webchat_topic (id, project_id, name, is_general, created_by, created_at, deleted_at) VALUES ($1, $2, 'legacy', FALSE, 'u1', $3, $4)`,
		legacyTopic, project, now.Add(-400*day), now.Add(-31*day))
	// A linked topic: its conversation_id (TEXT) is bound against the uuid
	// messages.conversation_id, and its pre-stamp rows match by thread_id.
	linkedConv := api.NewUUID()
	linkedTopic := api.NewUUID()
	exec(`INSERT INTO webchat_topic (id, project_id, name, is_general, conversation_id, created_by, created_at, deleted_at) VALUES ($1, $2, 'linked', FALSE, $3, 'u1', $4, $5)`,
		linkedTopic, project, linkedConv, now.Add(-400*day), now.Add(-45*day))
	recentDeleted := now.Add(-5 * day)
	graceConv := &store.Conversation{ID: api.NewUUID(), ProjectID: &pid, Kind: "group", Surface: "native",
		DriftState: "active", LastActivityAt: recentDeleted, CreatedAt: recentDeleted.Add(-time.Hour), DeletedAt: &recentDeleted}
	require.NoError(t, cs.CreateConversation(ctx, graceConv))

	recent := now.Add(-time.Hour)
	g1 := seed(recent, store.MessageDispatchDispatched, goneConv.ID, "")
	g2 := seed(recent, store.MessageDispatchFailed, goneConv.ID, "")
	l1 := seed(recent, store.MessageDispatchDispatched, "", legacyTopic)
	k1 := seed(recent, store.MessageDispatchDispatched, linkedConv, linkedTopic)
	k2 := seed(recent, store.MessageDispatchDispatched, "", linkedTopic) // pre-stamp row
	purge := []string{g1, g2, l1, k1, k2}
	keep := []string{
		seed(recent, store.MessageDispatchPending, goneConv.ID, ""),
		seed(recent, store.MessageDispatchDispatched, graceConv.ID, ""),
		seed(now.Add(-400*day), store.MessageDispatchDispatched, "", ""),          // no window: kept
		seed(recent, store.MessageDispatchDispatched, api.NewUUID(), linkedTopic), // re-keyed elsewhere: kept
	}
	attOK := attach(g1)
	attFail := attach(l1)

	cfg := opsettings.DefaultChatRetentionConfig()
	cfg.Enabled, cfg.PauseMS = true, 0
	sw := newChatRetentionSweeper(cs.ChatRetention())
	blobs := &fakeChatRetentionBlobs{fail: map[string]bool{attFail: true}}
	sw.attachments = blobs
	res, err := sw.RunTick(ctx, newChatRetentionPolicy(true, cfg, now))
	require.NoError(t, err)
	assert.True(t, res.DeletedDone)
	assert.Equal(t, int64(len(purge)), res.Purged.DeletedConv)
	assert.Zero(t, res.Purged.Main)

	for _, id := range purge {
		assert.Equal(t, 0, count(`SELECT COUNT(*) FROM messages WHERE id = $1`, id), "purged %s", id)
	}
	for _, id := range keep {
		assert.Equal(t, 1, count(`SELECT COUNT(*) FROM messages WHERE id = $1`, id), "kept %s", id)
	}
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_message_attachment", "webchat_message_star"} {
		assert.Equal(t, 0, count(`SELECT COUNT(*) FROM `+table+` d WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE CAST(m.id AS TEXT) = CAST(d.message_id AS TEXT))`), table)
	}
	assert.Equal(t, []string{project + "/" + attOK}, blobs.deleted)
	assert.Equal(t, 0, count(`SELECT COUNT(*) FROM webchat_attachment WHERE id = $1`, attOK))
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM webchat_attachment WHERE id = $1`, attFail), "a failed byte delete keeps the row")
	assert.Equal(t, int64(1), res.Purged.AttachmentRows)
	assert.Equal(t, int64(10), res.Purged.AttachmentBytes)
	assert.Equal(t, 1, res.AttachmentDeleteFailures)

	run, err := cs.ChatRetention().LatestChatRetentionRun(ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, store.ChatRetentionStatusOK, run.Status)
	assert.Equal(t, int64(len(purge)), run.Counts[project].DeletedConv)
	assert.Equal(t, 30, run.Policy.DeletedConversationGraceDays)

	// §8.5: a mention after a purged read watermark is unread.
	topicKey := api.NewUUID()
	newer := seed(recent, store.MessageDispatchDispatched, "", topicKey)
	exec(`INSERT INTO webchat_mention (user_id, conversation_key, message_id) VALUES ('u1', $1, $2)`, topicKey, newer)
	require.NoError(t, wcs.SetReadState(ctx, "u1", topicKey, g1)) // purged above
	mentions, err := wcs.UnreadMentionKeys(ctx, "u1", []string{topicKey})
	require.NoError(t, err)
	assert.True(t, mentions[topicKey])
}

// TestChatRetention_Postgres_PreviewEqualsPurge: on Postgres the preview
// counts exactly what a manual sweep then deletes, per project and phase,
// on a fixture with old and new messages of live and deleted
// conversations, a deleted legacy topic and pending rows, and deletes
// nothing itself.
func TestChatRetention_Postgres_PreviewEqualsPurge(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t)
	_ = stdlib.GetDefaultDriver() // registers the "pgx" driver
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, cs.Migrate(ctx))
	db := cs.DB()
	require.NoError(t, NewWebChatStore(db, "postgres").Init())

	p1, p2 := api.NewUUID(), api.NewUUID()
	now := time.Now().UTC()
	day := 24 * time.Hour
	old := now.Add(-40 * day)
	recent := now.Add(-time.Hour)
	seed := func(project string, created time.Time, state, conv, thread string) {
		m := &store.Message{ID: api.NewUUID(), ProjectID: project, Sender: "user:a", Recipient: "agent:b", Msg: "hello",
			Type: "chat", Channel: "web", ConversationID: conv, ThreadID: thread, DispatchState: state, CreatedAt: created}
		require.NoError(t, cs.CreateMessage(ctx, m))
	}
	count := func(q string) int {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, q).Scan(&n), q)
		return n
	}
	pid := p1
	deleted := now.Add(-40 * day)
	gone := &store.Conversation{ID: api.NewUUID(), ProjectID: &pid, Kind: "group", Surface: "native",
		DriftState: "active", LastActivityAt: deleted, CreatedAt: deleted.Add(-time.Hour), DeletedAt: &deleted}
	require.NoError(t, cs.CreateConversation(ctx, gone))
	legacy := api.NewUUID()
	_, err = db.ExecContext(ctx, `INSERT INTO webchat_topic (id, project_id, name, is_general, created_by, created_at, deleted_at) VALUES ($1, $2, 'legacy', FALSE, 'u1', $3, $4)`,
		legacy, p2, now.Add(-400*day), now.Add(-31*day))
	require.NoError(t, err)
	live := api.NewUUID()
	for i := 0; i < 30; i++ {
		at := old.Add(time.Duration(i) * time.Second)
		seed(p1, at, store.MessageDispatchDispatched, live, "")
		seed(p2, at, store.MessageDispatchDispatched, "", "")
		seed(p1, at, store.MessageDispatchDispatched, gone.ID, "")
		seed(p2, recent, store.MessageDispatchDispatched, gone.ID, "")
		seed(p2, at, store.MessageDispatchDispatched, "", legacy)
	}
	seed(p1, old, store.MessageDispatchPending, live, "")
	seed(p1, old, store.MessageDispatchDeferred, gone.ID, "")
	seed(p1, recent, store.MessageDispatchDispatched, live, "")
	total := count(`SELECT COUNT(*) FROM messages`)

	cfg := opsettings.DefaultChatRetentionConfig()
	cfg.Enabled, cfg.MessagesRetentionDays, cfg.ChunkSize, cfg.PauseMS = true, 30, 25, 0
	p := newChatRetentionPolicy(true, cfg, now)
	sw := newChatRetentionSweeper(cs.ChatRetention())

	preview, err := sw.Preview(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, total, count(`SELECT COUNT(*) FROM messages`), "the preview deletes nothing")
	assert.Equal(t, store.ChatRetentionCounts{Main: 30, DeletedConv: 30}, preview.ByProject[p1])
	assert.Equal(t, store.ChatRetentionCounts{Main: 30, DeletedConv: 60}, preview.ByProject[p2])

	run := &store.ChatRetentionRun{Trigger: store.ChatRetentionTriggerManual, StartedAt: now}
	var res chatRetentionTickResult
	start := time.Now()
	require.NoError(t, sw.sweepAndRecord(ctx, p, run, &res, start, start.Add(chatRetentionOpMaxDuration), chatRetentionSweepManual))
	for project, want := range preview.ByProject {
		assert.Equal(t, want.Main, run.Counts[project].Main, "project %s main", project)
		assert.Equal(t, want.DeletedConv, run.Counts[project].DeletedConv, "project %s deleted_conv", project)
	}
	assert.Equal(t, 3, count(`SELECT COUNT(*) FROM messages`), "pending, deferred and recent messages are kept")
}
