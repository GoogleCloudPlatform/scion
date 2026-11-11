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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const legacyHealthBrokerID = "33333333-3333-3333-3333-333333333333"

// assertLegacyBrokerHealthNull reads a broker row written before the health
// column existed and checks its health is nil (not reported), then that a
// report written afterwards round-trips.
func assertLegacyBrokerHealthNull(t *testing.T, s *ProjectStore) {
	t.Helper()
	ctx := context.Background()
	b, err := s.GetRuntimeBroker(ctx, legacyHealthBrokerID)
	require.NoError(t, err)
	assert.Nil(t, b.Health, "a broker row from before the migration reads back as not reported")
	assert.Equal(t, store.BrokerStatusOnline, b.Status)

	report := &api.BrokerHealthReport{Status: "degraded", Checks: map[string]string{"runtime": "unavailable"}}
	b.Health = report
	require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
	got, err := s.GetRuntimeBroker(ctx, legacyHealthBrokerID)
	require.NoError(t, err)
	assert.Equal(t, report, got.Health)
	assert.Equal(t, store.BrokerStatusOnline, got.Status, "storing health leaves status as it was")
}

// TestMigrationRuntimeBrokerHealthColumn upgrades a runtime_brokers table
// without the health column and checks that an existing broker reads back
// with nil health. The SQLite case always runs. The Postgres case runs
// under -tags integration with SCION_TEST_POSTGRES_URL set.
func TestMigrationRuntimeBrokerHealthColumn(t *testing.T) {
	insert := "INSERT INTO runtime_brokers (id, name, slug, status, connection_state, auto_provide, lock_version, created, updated) " +
		"VALUES ('" + legacyHealthBrokerID + "', 'legacy', 'legacy', 'online', 'connected', false, 0, '2026-01-01 00:00:00+00:00', '2026-01-01 00:00:00+00:00')"

	t.Run("sqlite", func(t *testing.T) {
		dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
		ctx := context.Background()

		// Build the current schema, then return runtime_brokers to its
		// shape before the health column and insert a row the way the
		// earlier schema wrote it.
		client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
		require.NoError(t, err)
		require.NoError(t, entc.AutoMigrate(ctx, client))
		require.NoError(t, client.Close())

		raw, err := sql.Open("sqlite", dsn)
		require.NoError(t, err)
		_, err = raw.ExecContext(ctx, "ALTER TABLE runtime_brokers DROP COLUMN health")
		require.NoError(t, err)
		_, err = raw.ExecContext(ctx, insert)
		require.NoError(t, err)
		require.NoError(t, raw.Close())

		client, err = entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, entc.AutoMigrate(ctx, client))

		assertLegacyBrokerHealthNull(t, NewProjectStore(client))
	})

	t.Run("postgres", func(t *testing.T) {
		if !enttest.Active() {
			t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres migration case")
		}
		ctx := context.Background()
		dsn := enttest.NewSchemaURL(t)
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })

		_, err = db.ExecContext(ctx, "ALTER TABLE runtime_brokers DROP COLUMN health")
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, insert)
		require.NoError(t, err)

		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, entc.AutoMigrate(ctx, client))

		assertLegacyBrokerHealthNull(t, NewProjectStore(client))
	})
}
