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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Upgrade path for the notification dispatch_attempts and dispatch_outcome
// columns (ptone/scion#4800). The legacy DDL below is a frozen snapshot of
// the notifications table before those columns existed, so it must not be
// regenerated from the current schema.

const (
	legacyNotifDelivered = "11111111-1111-1111-1111-111111111111" // dispatched = true
	legacyNotifPending   = "22222222-2222-2222-2222-222222222222" // dispatched = false
)

// legacyNotificationsSQLite is the pre-#4800 SQLite notifications table and
// its indexes, plus one dispatched and one undispatched row.
var legacyNotificationsSQLite = []string{
	"CREATE TABLE `notifications` (" +
		"`id` uuid NOT NULL, `subscription_id` uuid NOT NULL, `agent_id` uuid NOT NULL, " +
		"`project_id` uuid NOT NULL, `subscriber_type` text NOT NULL, `subscriber_id` text NOT NULL, " +
		"`status` text NOT NULL, `message` text NOT NULL, " +
		"`dispatched` bool NOT NULL DEFAULT (false), `acknowledged` bool NOT NULL DEFAULT (false), " +
		"`created` datetime NOT NULL, PRIMARY KEY (`id`))",
	"CREATE INDEX `notification_subscription_id` ON `notifications` (`subscription_id`)",
	"CREATE INDEX `notification_project_id_subscriber_type_subscriber_id` ON `notifications` (`project_id`, `subscriber_type`, `subscriber_id`)",
	"CREATE INDEX `notification_subscriber_type_created` ON `notifications` (`subscriber_type`, `created`) WHERE dispatched = false",
	"INSERT INTO notifications (id, subscription_id, agent_id, project_id, subscriber_type, subscriber_id, status, message, dispatched, acknowledged, created) VALUES " +
		"('" + legacyNotifDelivered + "', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'cccccccc-cccc-cccc-cccc-cccccccccccc', 'agent', 'sub-agent', 'COMPLETED', 'done', true, false, '2026-01-01 00:00:00+00:00'), " +
		"('" + legacyNotifPending + "', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'cccccccc-cccc-cccc-cccc-cccccccccccc', 'agent', 'sub-agent', 'WAITING_FOR_INPUT', 'waiting', false, false, '2026-01-01 00:00:01+00:00')",
}

// legacyNotificationsPostgres is the Postgres form of
// legacyNotificationsSQLite.
var legacyNotificationsPostgres = []string{
	`CREATE TABLE "notifications" (` +
		`"id" uuid NOT NULL, "subscription_id" uuid NOT NULL, "agent_id" uuid NOT NULL, ` +
		`"project_id" uuid NOT NULL, "subscriber_type" character varying NOT NULL, "subscriber_id" character varying NOT NULL, ` +
		`"status" character varying NOT NULL, "message" character varying NOT NULL, ` +
		`"dispatched" boolean NOT NULL DEFAULT false, "acknowledged" boolean NOT NULL DEFAULT false, ` +
		`"created" timestamptz NOT NULL, PRIMARY KEY ("id"))`,
	`CREATE INDEX "notification_subscription_id" ON "notifications" ("subscription_id")`,
	`CREATE INDEX "notification_project_id_subscriber_type_subscriber_id" ON "notifications" ("project_id", "subscriber_type", "subscriber_id")`,
	`CREATE INDEX "notification_subscriber_type_created" ON "notifications" ("subscriber_type", "created") WHERE dispatched = false`,
	`INSERT INTO notifications (id, subscription_id, agent_id, project_id, subscriber_type, subscriber_id, status, message, dispatched, acknowledged, created) VALUES ` +
		`('` + legacyNotifDelivered + `', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'cccccccc-cccc-cccc-cccc-cccccccccccc', 'agent', 'sub-agent', 'COMPLETED', 'done', true, false, '2026-01-01 00:00:00+00'), ` +
		`('` + legacyNotifPending + `', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'cccccccc-cccc-cccc-cccc-cccccccccccc', 'agent', 'sub-agent', 'WAITING_FOR_INPUT', 'waiting', false, false, '2026-01-01 00:00:01+00')`,
}

// execAll runs stmts on db in order.
func execAll(t *testing.T, db *sql.DB, stmts []string) {
	t.Helper()
	for _, stmt := range stmts {
		_, err := db.ExecContext(context.Background(), stmt)
		require.NoError(t, err, stmt)
	}
}

// assertLegacyNotificationsUpgraded checks the legacy rows after Migrate:
// both read zero attempts and no outcome, keep their dispatched flag (so a
// dispatched legacy row keeps its old meaning and the pending one is still
// retried), and take attempts and an outcome like a new row.
func assertLegacyNotificationsUpgraded(t *testing.T, cs *CompositeStore) {
	t.Helper()
	ctx := context.Background()

	delivered, err := cs.GetNotification(ctx, legacyNotifDelivered)
	require.NoError(t, err)
	assert.True(t, delivered.Dispatched, "a dispatched legacy row must stay dispatched")
	assert.Zero(t, delivered.DispatchAttempts, "legacy rows read 0 attempts (not recorded)")
	assert.Empty(t, delivered.DispatchOutcome, "legacy rows have no recorded outcome")

	pending, err := cs.GetNotification(ctx, legacyNotifPending)
	require.NoError(t, err)
	assert.False(t, pending.Dispatched, "an undispatched legacy row must stay undispatched")
	assert.Zero(t, pending.DispatchAttempts)
	assert.Empty(t, pending.DispatchOutcome)
	for _, n := range []*store.Notification{delivered, pending} {
		assert.Empty(t, n.DispatchReason, "legacy rows have no recorded reason (ptone/scion#5763)")
		assert.Nil(t, n.NextAttemptAt, "legacy rows have no retry backoff (ptone/scion#5763)")
	}

	claimed, err := cs.ClaimNotificationForDispatch(ctx, legacyNotifDelivered)
	require.NoError(t, err)
	assert.False(t, claimed, "a dispatched legacy row must not be retried")

	claimed, err = cs.ClaimNotificationForDispatch(ctx, legacyNotifPending)
	require.NoError(t, err)
	require.True(t, claimed, "an undispatched legacy row must still be retried")
	require.NoError(t, cs.RecordNotificationDispatchAttempt(ctx, legacyNotifPending))
	applied, err := cs.SetNotificationDispatchOutcome(ctx, legacyNotifPending, store.NotificationOutcomeFailed, "")
	require.NoError(t, err)
	require.True(t, applied, "a legacy row with no outcome takes one")

	pending, err = cs.GetNotification(ctx, legacyNotifPending)
	require.NoError(t, err)
	assert.Equal(t, 1, pending.DispatchAttempts)
	assert.Equal(t, store.NotificationOutcomeFailed, pending.DispatchOutcome)
	assert.True(t, pending.Dispatched)
}

// notificationColumn describes one column of the SQLite notifications
// table.
type notificationColumn struct {
	notNull bool
	dflt    sql.NullString
}

// sqliteNotificationColumns returns the SQLite notifications table's columns
// by name (pragma_table_info).
func sqliteNotificationColumns(t *testing.T, cs *CompositeStore) map[string]notificationColumn {
	t.Helper()
	cols := map[string]notificationColumn{}
	rows, err := cs.DB().QueryContext(context.Background(), `SELECT name, "notnull", dflt_value FROM pragma_table_info('notifications')`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var notNull int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&name, &notNull, &dflt))
		cols[name] = notificationColumn{notNull: notNull != 0, dflt: dflt}
	}
	require.NoError(t, rows.Err())
	return cols
}

// postgresNotificationColumn returns is_nullable and column_default of the
// Postgres notifications column col.
func postgresNotificationColumn(t *testing.T, raw *sql.DB, col string) (nullable string, dflt sql.NullString) {
	t.Helper()
	require.NoError(t, raw.QueryRowContext(context.Background(),
		`SELECT is_nullable, column_default FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'notifications' AND column_name = $1`,
		col).Scan(&nullable, &dflt), col)
	return nullable, dflt
}

func TestNotificationDispatchFields_UpgradeFromLegacySchema_SQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "legacy-notifications.db")
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	execAll(t, raw, legacyNotificationsSQLite)
	require.NoError(t, raw.Close())

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	cols := sqliteNotificationColumns(t, cs)
	require.Contains(t, cols, "dispatch_attempts")
	assert.True(t, cols["dispatch_attempts"].notNull)
	assert.Contains(t, cols["dispatch_attempts"].dflt.String, "0")
	require.Contains(t, cols, "dispatch_outcome")
	assert.True(t, cols["dispatch_outcome"].notNull)
	assert.Contains(t, cols["dispatch_outcome"].dflt.String, "''")
	require.Contains(t, cols, "dispatch_reason")
	assert.True(t, cols["dispatch_reason"].notNull)
	assert.Contains(t, cols["dispatch_reason"].dflt.String, "''")
	require.Contains(t, cols, "next_attempt_at")
	assert.False(t, cols["next_attempt_at"].notNull, "next_attempt_at is nullable")

	assertLegacyNotificationsUpgraded(t, cs)
}

// Runs under -tags integration with SCION_TEST_POSTGRES_URL set; skips
// otherwise.
func TestNotificationDispatchFields_UpgradeFromLegacySchema_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping the Postgres notifications upgrade case")
	}
	dsn := enttest.NewEmptySchemaURL(t)
	raw, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	execAll(t, raw, legacyNotificationsPostgres)

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 3, MaxIdleConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	for _, c := range []struct{ col, dflt string }{
		{"dispatch_attempts", "0"},
		{"dispatch_outcome", "''"},
		{"dispatch_reason", "''"},
	} {
		nullable, dflt := postgresNotificationColumn(t, raw, c.col)
		assert.Equal(t, "NO", nullable, c.col)
		assert.Contains(t, dflt.String, c.dflt, c.col)
	}
	var nullable string
	require.NoError(t, raw.QueryRowContext(ctx,
		`SELECT is_nullable FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'notifications' AND column_name = 'next_attempt_at'`,
	).Scan(&nullable))
	assert.Equal(t, "YES", nullable, "next_attempt_at is nullable")

	assertLegacyNotificationsUpgraded(t, cs)
}
