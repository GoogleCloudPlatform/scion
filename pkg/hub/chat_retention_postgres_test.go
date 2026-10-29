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
