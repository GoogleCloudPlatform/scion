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
	"encoding/json"
	"log/slog"
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

// TestFailedMessageRetention_Postgres_SameRowsNoOrphans runs the
// failed-message purge handler on Postgres against the real web chat
// tables: exactly the failed agent messages older than the default 7-day
// window are purged over several chunks, never pending, deferred,
// delivered or "user:" recipient rows, no dependent row is orphaned, and
// the purge keeps its cursor.
func TestFailedMessageRetention_Postgres_SameRowsNoOrphans(t *testing.T) {
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

	srv := &Server{store: cs, agentLifecycleLog: slog.Default()}
	srv.SetChatRetentionStore(cs.ChatRetention())
	// The smallest chunk and no pause, so the 60 failed rows take three
	// chunks end to end.
	settings := newFakeHubSettingStore()
	settings.seed(opsettings.ChatRetentionSectionName, json.RawMessage(`{"sweep":{"chunk_size":25,"pause_ms":0}}`))
	ops := NewOperationalSettings(settings, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	project := api.NewUUID()
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	exec := func(q string, args ...any) {
		_, err := db.ExecContext(ctx, q, args...)
		require.NoError(t, err, q)
	}
	seed := func(created time.Time, state, recipient string) string {
		m := &store.Message{ID: api.NewUUID(), ProjectID: project, Sender: "user:a", Recipient: recipient, Msg: "hello",
			Type: "chat", Channel: "web", DispatchState: state, CreatedAt: created}
		require.NoError(t, cs.CreateMessage(ctx, m))
		exec(`INSERT INTO message_addressees (id, message_id, principal_kind, principal_id, via, delivery_state) VALUES ($1, $2, 'user', 'u1', 'explicit', 'delivered')`, api.NewUUID(), m.ID)
		exec(`INSERT INTO webchat_message_ext (message_id) VALUES ($1)`, m.ID)
		exec(`INSERT INTO webchat_mention (user_id, conversation_key, message_id) VALUES ('u1', 'topic:x', $1)`, m.ID)
		exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES ('u1', $1, 'topic:x', $2, now())`, m.ID, project)
		att := api.NewUUID()
		exec(`INSERT INTO webchat_attachment (id, project_id, filename, mime_type, size, uploaded_by) VALUES ($1, $2, 'f', 'text/plain', 1, 'u1')`, att, project)
		exec(`INSERT INTO webchat_message_attachment (message_id, attachment_id) VALUES ($1, $2)`, m.ID, att)
		return m.ID
	}

	var purge []string
	for i := 0; i < 60; i++ {
		purge = append(purge, seed(old.Add(time.Duration(i)*time.Second), store.MessageDispatchFailed, "agent:b"))
	}
	keep := []string{
		seed(old, store.MessageDispatchFailed, "user:a"),
		seed(old, store.MessageDispatchPending, "agent:b"),
		seed(old, store.MessageDispatchDeferred, "agent:b"),
		seed(old, store.MessageDispatchDispatched, "agent:b"),
		seed(now.Add(-6*24*time.Hour), store.MessageDispatchFailed, "agent:b"),
	}

	srv.failedMessageRetentionHandler()(ctx)

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
	assert.Equal(t, 0, count(`SELECT COUNT(*) FROM chat_retention_runs`), "chat retention off: no run is opened")
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM chat_retention_cursors WHERE phase = $1`, store.ChatRetentionPhaseFailed))
}
