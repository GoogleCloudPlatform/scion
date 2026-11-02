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

package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// chatRetentionProvider is the store capability cmd/server_foreground.go
// wires the chat retention store from.
type chatRetentionProvider interface {
	DB() *sql.DB
	ChatRetention() store.ChatRetentionStore
}

// setChatRetentionSettings replaces srv's operational settings with ones
// holding the hub.chat_retention override and, when doc is not empty, the
// chat_retention section.
func setChatRetentionSettings(t *testing.T, srv *Server, experimentOn bool, doc string) {
	t.Helper()
	fake := newFakeHubSettingStore()
	fake.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.ChatRetention, experimentOn)))
	if doc != "" {
		fake.seed(opsettings.ChatRetentionSectionName, json.RawMessage(doc))
	}
	ops := NewOperationalSettings(fake, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

// chatRetentionHubFixture is a hub on one SQLite database holding the ent
// tables and the real web chat tables, with the chat retention store wired
// as in production.
type chatRetentionHubFixture struct {
	t       *testing.T
	ctx     context.Context
	srv     *Server
	s       store.Store
	db      *sql.DB
	wcs     WebChatStore
	crs     store.ChatRetentionStore
	alice   *store.User
	project *store.Project
}

func newChatRetentionHubFixture(t *testing.T) *chatRetentionHubFixture {
	t.Helper()
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	p, ok := s.(chatRetentionProvider)
	require.True(t, ok, "the test store provides the chat retention store")
	wcs := NewWebChatStore(p.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	srv.SetChatRetentionStore(p.ChatRetention())
	return &chatRetentionHubFixture{t: t, ctx: context.Background(), srv: srv, s: s, db: p.DB(), wcs: wcs,
		crs: p.ChatRetention(), alice: alice, project: project}
}

type chatRetentionSeed struct {
	created   time.Time
	state     string
	channel   string
	conv      string
	thread    string
	recipient string // default "agent:bot"
}

// message stores a message and one row in every dependent table.
func (f *chatRetentionHubFixture) message(sd chatRetentionSeed) string {
	f.t.Helper()
	if sd.state == "" {
		sd.state = store.MessageDispatchDispatched
	}
	if sd.recipient == "" {
		sd.recipient = "agent:bot"
	}
	m := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "user:alice@test.com", SenderID: f.alice.ID,
		Recipient: sd.recipient, Msg: "hello", Type: "chat", Channel: sd.channel, ThreadID: sd.thread,
		ConversationID: sd.conv, DispatchState: sd.state, CreatedAt: sd.created,
	}
	require.NoError(f.t, f.s.CreateMessage(f.ctx, m))
	f.dependents(m.ID)
	return m.ID
}

func (f *chatRetentionHubFixture) exec(query string, args ...any) {
	f.t.Helper()
	_, err := f.db.ExecContext(f.ctx, query, args...)
	require.NoError(f.t, err, query)
}

func (f *chatRetentionHubFixture) dependents(id string) {
	f.t.Helper()
	f.exec(`INSERT INTO message_addressees (id, message_id, principal_kind, principal_id, via, delivery_state) VALUES (?, ?, 'user', ?, 'explicit', 'delivered')`,
		api.NewUUID(), id, f.alice.ID)
	f.exec(`INSERT INTO webchat_message_ext (message_id, reply_to_id) VALUES (?, NULL)`, id)
	f.exec(`INSERT INTO webchat_mention (user_id, conversation_key, message_id) VALUES (?, ?, ?)`, f.alice.ID, "topic:x", id)
	f.exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, ?, ?)`,
		f.alice.ID, id, "topic:x", f.project.ID, time.Now().UTC().Format(time.RFC3339))
	att := api.NewUUID()
	f.exec(`INSERT INTO webchat_attachment (id, project_id, filename, mime_type, size, uploaded_by, created_at) VALUES (?, ?, 'a.txt', 'text/plain', 1, ?, ?)`,
		att, f.project.ID, f.alice.ID, time.Now().UTC().Format(time.RFC3339))
	f.exec(`INSERT INTO webchat_message_attachment (message_id, attachment_id) VALUES (?, ?)`, id, att)
}

func (f *chatRetentionHubFixture) count(query string, args ...any) int {
	f.t.Helper()
	return chatRetentionCount(f.t, f.db, query, args...)
}

func chatRetentionCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n), query)
	return n
}

// chatRetentionOrphans counts, per dependent table, the rows whose message
// does not exist (the cascade completeness property).
func chatRetentionOrphans(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_mention", "webchat_message_attachment", "webchat_message_star"} {
		out[table] = chatRetentionCount(t, db, `SELECT COUNT(*) FROM `+table+` d WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = d.message_id)`)
	}
	return out
}

func chatRetentionNoOrphans() map[string]int {
	return map[string]int{"message_addressees": 0, "webchat_message_ext": 0, "webchat_mention": 0, "webchat_message_attachment": 0, "webchat_message_star": 0}
}

func (f *chatRetentionHubFixture) exists(id string) bool {
	return f.count(`SELECT COUNT(*) FROM messages WHERE id = ?`, id) == 1
}

// TestChatRetention_SweepSQLite: the chat-retention handler, with the
// experiment on, enabled and a 30-day window, purges exactly the messages
// older than the cutoff that are not pending or deferred (DMs, topics,
// agent-to-agent rows without a conversation, other channels), with every
// dependent row on the real web chat tables, and records the run.
func TestChatRetention_SweepSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	dmConv, topicConv := api.NewUUID(), api.NewUUID()

	purge := []string{
		f.message(chatRetentionSeed{created: old, channel: "web", conv: dmConv, thread: "dm:user:a:user:b"}),
		f.message(chatRetentionSeed{created: old.Add(time.Minute), channel: "web", conv: topicConv, thread: "topic-1"}),
		f.message(chatRetentionSeed{created: old.Add(2 * time.Minute)}), // agent-to-agent, no conversation
		f.message(chatRetentionSeed{created: old.Add(3 * time.Minute), channel: "discord"}),
		f.message(chatRetentionSeed{created: old.Add(4 * time.Minute), state: store.MessageDispatchFailed}),
	}
	keep := []string{
		f.message(chatRetentionSeed{created: old.Add(5 * time.Minute), state: store.MessageDispatchPending}),
		f.message(chatRetentionSeed{created: old.Add(6 * time.Minute), state: store.MessageDispatchDeferred}),
		f.message(chatRetentionSeed{created: now.Add(-29 * 24 * time.Hour), channel: "web", conv: topicConv}),
		f.message(chatRetentionSeed{created: now.Add(-time.Hour), channel: "web", conv: dmConv}),
	}

	f.srv.chatRetentionHandler()(f.ctx)

	for _, id := range purge {
		assert.False(t, f.exists(id), "purged %s", id)
	}
	for _, id := range keep {
		assert.True(t, f.exists(id), "kept %s", id)
	}
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))
	assert.Equal(t, len(keep), f.count(`SELECT COUNT(*) FROM webchat_message_star`))
	// Phase 1: unlinked attachment metadata stays for the attachment GC.
	assert.Equal(t, len(purge)+len(keep), f.count(`SELECT COUNT(*) FROM webchat_attachment`))

	cur, err := f.crs.GetChatRetentionCursor(f.ctx, store.ChatRetentionPhaseMain)
	require.NoError(t, err)
	require.NotNil(t, cur.LastAppliedCutoff, "the walk reached the cutoff")
	assert.WithinDuration(t, now.Add(-30*24*time.Hour), *cur.LastAppliedCutoff, time.Minute)

	run, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, store.ChatRetentionStatusOK, run.Status)
	assert.Equal(t, 30, run.Policy.MessagesRetentionDays)
	assert.Equal(t, store.ChatRetentionCounts{
		Main: 5, Addressees: 5, Mentions: 5, Stars: 5, Ext: 5, Attachments: 5, UnlinkedAttachments: 5,
	}, run.Counts[f.project.ID])

	// The stored run row holds no message ID and no content.
	var policy, counts string
	require.NoError(t, f.db.QueryRow(`SELECT policy, counts FROM chat_retention_runs WHERE id = ?`, run.ID).Scan(&policy, &counts))
	for _, id := range append(purge, keep...) {
		assert.NotContains(t, policy+counts, id)
	}
	assert.NotContains(t, policy+counts, "hello")
}

// TestChatRetention_GatingSQLite: with the experiment off, enabled off or
// no window, the handler deletes nothing and records no run.
func TestChatRetention_GatingSQLite(t *testing.T) {
	tests := []struct {
		name       string
		experiment bool
		doc        string
	}{
		{"experiment off", false, `{"enabled":true,"messages_retention_days":30}`},
		{"enabled off", true, `{"enabled":false,"messages_retention_days":30}`},
		{"window null", true, `{"enabled":true}`},
		{"section absent", true, ``},
		{"malformed section fails closed", true, `{"enabled":true,"messages_retention_days":3}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newChatRetentionHubFixture(t)
			setChatRetentionSettings(t, f.srv, tt.experiment, tt.doc)
			id := f.message(chatRetentionSeed{created: time.Now().Add(-400 * 24 * time.Hour)})
			f.srv.chatRetentionHandler()(f.ctx)
			assert.True(t, f.exists(id))
			assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM chat_retention_runs`))
		})
	}
}

// TestChatRetention_ConcurrentWritesSQLite: sends, stars and replies on
// rows being purged, while the sweep runs, never fail. A star written on a
// message after it was purged is an orphan the star upkeep removes.
func TestChatRetention_ConcurrentWritesSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"chunk_size":25,"pause_ms":0}}`)
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	var oldIDs []string
	for i := 0; i < 300; i++ {
		oldIDs = append(oldIDs, f.message(chatRetentionSeed{created: old.Add(time.Duration(i) * time.Second), channel: "web"}))
	}

	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			target := oldIDs[i%len(oldIDs)]
			m := &store.Message{ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "user:alice@test.com", Recipient: "agent:bot",
				Msg: "new", Type: "chat", Channel: "web", DispatchState: store.MessageDispatchDispatched}
			if err := f.s.CreateMessage(f.ctx, m); err != nil {
				errs <- err
			}
			if err := f.wcs.SetMessageReplyTo(f.ctx, m.ID, target); err != nil {
				errs <- err
			}
			if _, err := f.db.ExecContext(f.ctx, `INSERT OR IGNORE INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, 'topic:x', ?, ?)`,
				"other-user", target, f.project.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
				errs <- err
			}
			i++
		}
	}()

	sw := newChatRetentionSweeper(f.crs)
	res, err := sw.RunTick(f.ctx, f.srv.chatRetentionPolicyAt(time.Now()))
	close(stop)
	wg.Wait()
	close(errs)
	require.NoError(t, err)
	for e := range errs {
		t.Errorf("concurrent write failed: %v", e)
	}
	assert.True(t, res.ReachedCutoff)
	assert.Equal(t, int64(300), res.Purged.Main)
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM messages WHERE created < ?`, time.Now().UTC().Add(-30*24*time.Hour)))
}

// TestChatHistory_ReplyToPurgedMessageUnavailable: a reply whose target
// message no longer exists gets a preview marked unavailable; a reply to a
// message that exists keeps its normal preview.
func TestChatHistory_ReplyToPurgedMessageUnavailable(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	conv := api.NewUUID()
	parentGone := f.message(chatRetentionSeed{created: time.Now().Add(-time.Hour), channel: "web", conv: conv})
	parentKept := f.message(chatRetentionSeed{created: time.Now().Add(-50 * time.Minute), channel: "web", conv: conv})
	replyA := &store.Message{ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "user:alice@test.com", Recipient: "agent:bot",
		Msg: "re", Type: "chat", Channel: "web", ConversationID: conv, DispatchState: store.MessageDispatchDispatched}
	replyB := &store.Message{ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "user:alice@test.com", Recipient: "agent:bot",
		Msg: "re", Type: "chat", Channel: "web", ConversationID: conv, DispatchState: store.MessageDispatchDispatched}
	require.NoError(t, f.s.CreateMessage(f.ctx, replyA))
	require.NoError(t, f.s.CreateMessage(f.ctx, replyB))
	require.NoError(t, f.wcs.SetMessageReplyTo(f.ctx, replyA.ID, parentGone))
	require.NoError(t, f.wcs.SetMessageReplyTo(f.ctx, replyB.ID, parentKept))

	_, err := f.crs.PurgeChatMessages(f.ctx, []string{parentGone}, nil, store.ChatRetentionCursor{Phase: "test"})
	require.NoError(t, err)

	page := f.srv.chatHistoryPage(f.ctx, f.wcs, "topic:x", conv, &store.ListResult[store.Message]{Items: []store.Message{*replyA, *replyB}}, nil)
	gone, ok := page.ReplyPreviews[parentGone]
	require.True(t, ok, "a purged reply target still has a preview")
	assert.Equal(t, chatReplyPreview{MessageID: parentGone, Content: "[no longer available]", Unavailable: true}, gone)
	kept, ok := page.ReplyPreviews[parentKept]
	require.True(t, ok)
	assert.False(t, kept.Unavailable)
	assert.Equal(t, "hello", kept.Content)

	raw, err := json.Marshal(page.ReplyPreviews[parentGone])
	require.NoError(t, err)
	assert.JSONEq(t, `{"messageId":"`+parentGone+`","senderName":"","content":"[no longer available]","unavailable":true}`, string(raw))
	raw, err = json.Marshal(page.ReplyPreviews[parentKept])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "unavailable")
}

// TestChatHistory_RetentionMarkerSQLite: the history of a topic created
// before the last applied cutoff carries retention.purgedBefore on its
// oldest page, and not while retention is off.
func TestChatHistory_RetentionMarkerSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	topicID := api.NewUUID()
	require.NoError(t, f.wcs.CreateTopic(f.ctx, WebChatTopic{
		ID: topicID, ProjectID: f.project.ID, Name: "retained", CreatedBy: f.alice.ID,
		CreatedAt: time.Now().UTC().Add(-90 * 24 * time.Hour),
	}))
	setTopicConversationID(t, f.db, f.s, topicID, f.project.ID)
	conv := topicConversationID(t, f.wcs, topicID)
	f.message(chatRetentionSeed{created: time.Now().UTC().Add(-time.Hour), channel: "web", conv: conv, thread: topicID})
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	require.NoError(t, f.crs.SaveChatRetentionCursor(f.ctx, store.ChatRetentionCursor{
		Phase: store.ChatRetentionPhaseMain, LastAppliedCutoff: &cutoff, UpdatedAt: time.Now(),
	}))

	history := func() map[string]json.RawMessage {
		rec := doRequestAsUser(t, f.srv, f.alice, "GET", "/api/v1/chat/conversations/"+topicID+"/messages", nil)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body
	}

	setChatRetentionSettings(t, f.srv, false, `{"enabled":true,"messages_retention_days":30}`)
	_, present := history()["retention"]
	assert.False(t, present, "no marker while the experiment is off")

	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30}`)
	f.srv.SetChatRetentionStore(f.crs) // drop the cached cutoff
	raw, present := history()["retention"]
	require.True(t, present, "marker on the oldest page")
	var marker struct {
		PurgedBefore time.Time `json:"purgedBefore"`
	}
	require.NoError(t, json.Unmarshal(raw, &marker))
	assert.True(t, marker.PurgedBefore.Equal(cutoff), "purgedBefore %v, want %v", marker.PurgedBefore, cutoff)
	assert.Contains(t, string(raw), cutoff.Format(time.RFC3339))
}

// TestChatRetention_AcceptanceSQLite is the Phase 1 acceptance harness
// (design §9). It seeds messages across DMs, topics and agent-to-agent rows
// with dependent rows, spread over 60 days, on a file-backed SQLite
// database in WAL mode (as a workstation or community hub runs it), runs
// the sweep with a 30-day window at the default chunk size, and checks that
// exactly the eligible rows are purged, that no dependent row is left
// orphaned, and that no purge transaction took 250 ms or more.
//
// It seeds 5,000 messages by default. Set CHAT_RETENTION_ACCEPTANCE_ROWS
// (for example 100000) for the full harness; the transaction time bound is
// asserted only then, because the small default is too short to measure
// and shared CI machines vary:
//
//	CHAT_RETENTION_ACCEPTANCE_ROWS=100000 go test -run TestChatRetention_AcceptanceSQLite -v ./pkg/hub
//
// (through make test-hub-shard, locate the shard with hack/hubshard/hubshard.sh).
func TestChatRetention_AcceptanceSQLite(t *testing.T) {
	rows := 5000
	full := false
	if v := os.Getenv("CHAT_RETENTION_ACCEPTANCE_ROWS"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err)
		rows, full = n, true
	}
	ctx := context.Background()
	s, err := newTestStoreAt(t, "file:"+filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	p, ok := s.(chatRetentionProvider)
	require.True(t, ok)
	db := p.DB()
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())

	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)
	projects := []string{api.NewUUID(), api.NewUUID(), api.NewUUID()}
	dmConvs := []string{api.NewUUID(), api.NewUUID(), api.NewUUID(), api.NewUUID()}
	topicConvs := []string{api.NewUUID(), api.NewUUID(), api.NewUUID()}

	seedStart := time.Now()
	eligible := 0
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	insMsg, err := tx.PrepareContext(ctx, `INSERT INTO messages (id, project_id, sender, recipient, msg, type, urgent, broadcasted, read, dispatch_state, channel, thread_id, conversation_id, created)
VALUES (?, ?, 'user:alice', 'agent:bot', 'retention harness', 'chat', false, false, false, ?, ?, ?, ?, ?)`)
	require.NoError(t, err)
	insAddr, err := tx.PrepareContext(ctx, `INSERT INTO message_addressees (id, message_id, principal_kind, principal_id, via, delivery_state) VALUES (?, ?, 'agent', 'bot', 'direct', 'delivered')`)
	require.NoError(t, err)
	insExt, err := tx.PrepareContext(ctx, `INSERT INTO webchat_message_ext (message_id, reply_to_id) VALUES (?, ?)`)
	require.NoError(t, err)
	insMention, err := tx.PrepareContext(ctx, `INSERT INTO webchat_mention (user_id, conversation_key, message_id) VALUES ('u1', ?, ?)`)
	require.NoError(t, err)
	insStar, err := tx.PrepareContext(ctx, `INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES ('u1', ?, ?, ?, '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	insAtt, err := tx.PrepareContext(ctx, `INSERT INTO webchat_attachment (id, project_id, filename, mime_type, size, uploaded_by, created_at) VALUES (?, ?, 'f.txt', 'text/plain', 1, 'u1', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	insLink, err := tx.PrepareContext(ctx, `INSERT INTO webchat_message_attachment (message_id, attachment_id) VALUES (?, ?)`)
	require.NoError(t, err)
	var prev string
	span := 60 * 24 * time.Hour
	// One step per row; span*i itself would overflow int64 nanoseconds.
	step := span / time.Duration(rows)
	for i := 0; i < rows; i++ {
		id := uuid.NewString()
		created := now.Add(-span + time.Duration(i)*step)
		project := projects[i%len(projects)]
		state := store.MessageDispatchDispatched
		switch {
		case i%997 == 0:
			state = store.MessageDispatchPending
		case i%991 == 0:
			state = store.MessageDispatchDeferred
		case i%101 == 0:
			state = store.MessageDispatchFailed
		}
		var channel, thread, conv any
		switch i % 10 {
		case 0, 1, 2, 3: // DMs
			channel, conv, thread = "web", dmConvs[i%len(dmConvs)], "dm:user:a:agent:b"
		case 4, 5, 6: // topics
			channel, conv, thread = "web", topicConvs[i%len(topicConvs)], "topic"
		case 7, 8: // agent-to-agent, no conversation
		default: // another channel
			channel = "discord"
		}
		_, err := insMsg.ExecContext(ctx, id, project, state, channel, thread, conv, created)
		require.NoError(t, err)
		_, err = insAddr.ExecContext(ctx, uuid.NewString(), id)
		require.NoError(t, err)
		var reply any
		if prev != "" && i%3 == 0 {
			reply = prev
		}
		_, err = insExt.ExecContext(ctx, id, reply)
		require.NoError(t, err)
		if i%5 == 0 {
			_, err = insMention.ExecContext(ctx, "topic:x", id)
			require.NoError(t, err)
		}
		if i%20 == 0 {
			_, err = insStar.ExecContext(ctx, id, "topic:x", project)
			require.NoError(t, err)
		}
		if i%50 == 0 {
			att := uuid.NewString()
			_, err = insAtt.ExecContext(ctx, att, project)
			require.NoError(t, err)
			_, err = insLink.ExecContext(ctx, id, att)
			require.NoError(t, err)
		}
		if created.Before(cutoff) && state != store.MessageDispatchPending && state != store.MessageDispatchDeferred {
			eligible++
		}
		prev = id
	}
	require.NoError(t, tx.Commit())
	seedTime := time.Since(seedStart)
	total := chatRetentionCount(t, db, `SELECT COUNT(*) FROM messages`)
	require.Equal(t, rows, total)
	keptOld := chatRetentionCount(t, db, `SELECT COUNT(*) FROM messages WHERE created < ? AND dispatch_state IN ('pending', 'deferred')`, cutoff)
	newer := chatRetentionCount(t, db, `SELECT COUNT(*) FROM messages WHERE created >= ?`, cutoff)
	// The fixture must straddle the cutoff, or "nothing newer is purged"
	// proves nothing: about half of the rows are newer than 30 days.
	require.Greater(t, newer, rows/3, "seeded rows newer than the cutoff")
	require.Greater(t, eligible, rows/3, "seeded rows eligible for purge")
	require.Positive(t, keptOld, "seeded old pending or deferred rows")

	cfg := opsettings.DefaultChatRetentionConfig()
	cfg.Enabled = true
	cfg.MessagesRetentionDays = 30
	cfg.PauseMS = 0 // the pause does not change transaction times; keep the run short
	// One tick for the whole backlog: past the settings bound, which only
	// exists to fit the scheduler's handler context (not used here).
	cfg.TickBudgetSeconds = 3600
	policy := newChatRetentionPolicy(true, cfg, now)

	// Record every purge transaction's duration.
	timed := &timedChatRetentionStore{ChatRetentionStore: p.ChatRetention()}
	sw := newChatRetentionSweeper(timed)
	runStart := time.Now()
	res, err := sw.RunTick(ctx, policy)
	runTime := time.Since(runStart)
	require.NoError(t, err)
	require.True(t, res.ReachedCutoff, "the walk reached the cutoff in one tick")

	assert.Equal(t, int64(eligible), res.Purged.Main, "exactly the eligible rows are purged")
	assert.Equal(t, 0, chatRetentionCount(t, db, `SELECT COUNT(*) FROM messages WHERE created < ? AND dispatch_state NOT IN ('pending', 'deferred')`, cutoff))
	assert.Equal(t, keptOld+newer, chatRetentionCount(t, db, `SELECT COUNT(*) FROM messages`))
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, db))

	var sum time.Duration
	for _, d := range timed.durations {
		sum += d
	}
	mean := time.Duration(0)
	if len(timed.durations) > 0 {
		mean = sum / time.Duration(len(timed.durations))
	}
	t.Logf("chat retention acceptance: rows=%d eligible=%d purged=%d kept_old_pending_deferred=%d newer=%d chunks=%d max_tx=%v mean_tx=%v sweep_time=%v seed_time=%v chunk_sizes_first=%v",
		rows, eligible, res.Purged.Main, keptOld, newer, res.Chunks, res.MaxTx, mean, runTime, seedTime, firstN(res.ChunkSizes, 5))
	if full {
		assert.Less(t, res.MaxTx, 250*time.Millisecond, "no retention transaction may take 250 ms or more on SQLite")
	}
}

func firstN(v []int, n int) []int {
	if len(v) < n {
		return v
	}
	return v[:n]
}

// timedChatRetentionStore records the wall time of every purge.
type timedChatRetentionStore struct {
	store.ChatRetentionStore
	mu        sync.Mutex
	durations []time.Duration
}

func (s *timedChatRetentionStore) PurgeChatMessages(ctx context.Context, ids []string, projectOf map[string]string, c store.ChatRetentionCursor) (store.ChatRetentionPurgeResult, error) {
	start := time.Now()
	res, err := s.ChatRetentionStore.PurgeChatMessages(ctx, ids, projectOf, c)
	s.mu.Lock()
	s.durations = append(s.durations, time.Since(start))
	s.mu.Unlock()
	return res, err
}

// TestChatHistoryRetentionMarker_DMByConversation: for a DM listed by
// conversation, the marker follows the conversation row's creation time:
// shown when it was created before the last applied cutoff, not after.
func TestChatHistoryRetentionMarker_DMByConversation(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30}`)
	conv, err := f.s.UpsertConversationByExternalRef(f.ctx, &store.Conversation{
		Kind: "direct", Surface: "native", ExternalRef: "dm:user:a:user:b", DriftState: "active",
	})
	require.NoError(t, err)
	stored, err := f.s.GetConversation(f.ctx, conv.ID)
	require.NoError(t, err)
	created := stored.CreatedAt
	require.False(t, created.IsZero())

	marker := func(cutoff time.Time) *chatHistoryRetention {
		f.srv.chatRetention.mu.Lock()
		c := cutoff
		f.srv.chatRetention.cutoff = &c
		f.srv.chatRetention.cutoffAt = time.Now()
		f.srv.chatRetention.mu.Unlock()
		return f.srv.chatHistoryRetentionMarker(f.ctx, &store.ListResult[store.Message]{}, conv.ID, nil)
	}

	got := marker(created.Add(time.Hour))
	require.NotNil(t, got, "a DM created before the cutoff shows the marker")
	assert.True(t, got.PurgedBefore.Equal(created.Add(time.Hour)))
	assert.Nil(t, marker(created.Add(-time.Hour)), "a DM created after the cutoff shows none")
	assert.Nil(t, f.srv.chatHistoryRetentionMarker(f.ctx, &store.ListResult[store.Message]{}, "", nil),
		"a legacy DM without a conversation shows none")
}

// cancellingChatRetentionStore cancels the tick's context after a given
// number of purges, as a replica that stops (or loses the lock at the end
// of its tick) mid-walk would.
type cancellingChatRetentionStore struct {
	store.ChatRetentionStore
	cancel     context.CancelFunc
	after      int
	purges     int
	firstAfter *store.ChatRetentionPosition
}

func (s *cancellingChatRetentionStore) SelectChatRetentionCandidates(ctx context.Context, cutoff time.Time, after store.ChatRetentionPosition, limit int) ([]store.ChatRetentionCandidate, error) {
	if s.firstAfter == nil {
		a := after
		s.firstAfter = &a
	}
	return s.ChatRetentionStore.SelectChatRetentionCandidates(ctx, cutoff, after, limit)
}

func (s *cancellingChatRetentionStore) PurgeChatMessages(ctx context.Context, ids []string, projectOf map[string]string, c store.ChatRetentionCursor) (store.ChatRetentionPurgeResult, error) {
	res, err := s.ChatRetentionStore.PurgeChatMessages(ctx, ids, projectOf, c)
	s.purges++
	if s.cancel != nil && s.purges == s.after {
		s.cancel()
	}
	return res, err
}

// TestChatRetention_FailoverResumesFromCursorSQLite: a sweeper that stops
// mid-tick leaves its cursor and run row in the database; a fresh sweeper
// (another replica, at its next tick) resumes from that cursor, not from the
// start, and the two together purge every eligible message exactly once.
func TestChatRetention_FailoverResumesFromCursorSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"chunk_size":25,"pause_ms":0}}`)
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	for i := 0; i < 120; i++ {
		f.message(chatRetentionSeed{created: old.Add(time.Duration(i) * time.Second), channel: "web"})
	}
	now := time.Now()

	ctxA, cancelA := context.WithCancel(f.ctx)
	defer cancelA()
	a := &cancellingChatRetentionStore{ChatRetentionStore: f.crs, cancel: cancelA, after: 2}
	resA, errA := newChatRetentionSweeper(a).RunTick(ctxA, f.srv.chatRetentionPolicyAt(now))
	require.ErrorIs(t, errA, context.Canceled)
	assert.Equal(t, int64(50), resA.Purged.Main)
	stored, err := f.crs.GetChatRetentionCursor(f.ctx, store.ChatRetentionPhaseMain)
	require.NoError(t, err)
	require.False(t, stored.Position.IsStart(), "replica A's cursor is stored")
	runA, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, runA)
	assert.Equal(t, store.ChatRetentionStatusFailed, runA.Status)
	assert.Equal(t, int64(50), runA.Counts[f.project.ID].Main)

	// Replica B, at the next tick after the interval.
	b := &cancellingChatRetentionStore{ChatRetentionStore: f.crs}
	swB := newChatRetentionSweeper(b)
	later := now.Add(15 * time.Minute)
	swB.now = func() time.Time { return later }
	resB, err := swB.RunTick(f.ctx, f.srv.chatRetentionPolicyAt(later))
	require.NoError(t, err)
	require.NotNil(t, b.firstAfter)
	assert.Equal(t, stored.Position.ID, b.firstAfter.ID, "B resumes from A's cursor")
	assert.True(t, stored.Position.Created.Equal(b.firstAfter.Created))
	assert.Equal(t, int64(70), resB.Purged.Main)
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM messages WHERE created < ?`, now.UTC().Add(-30*24*time.Hour)))
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))
}
