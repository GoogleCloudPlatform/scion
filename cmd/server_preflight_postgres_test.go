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

package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestServerPreflight_Postgres runs the preflight's Postgres path against a
// live database (-tags integration with SCION_TEST_POSTGRES_URL; the
// Makefile's test-store-postgres-tail runs it in CI): the snapshot's
// isolation, the read-only session, the pool, and the read-only half of the
// search_path shadow guard.
func TestServerPreflight_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping the preflight Postgres cases")
	}
	ctx := context.Background()

	t.Run("snapshot", func(t *testing.T) {
		client, err := openPreflightClient("postgres", enttest.NewSchemaURL(t))
		require.NoError(t, err)
		defer func() { _ = client.Close() }()
		db := client.Driver().(*entsql.Driver).DB()
		tx, err := db.BeginTx(ctx, snapshotTxOptions(dialect.Postgres))
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		var isolation, readOnly string
		require.NoError(t, tx.QueryRowContext(ctx, "SHOW transaction_isolation").Scan(&isolation))
		require.NoError(t, tx.QueryRowContext(ctx, "SHOW transaction_read_only").Scan(&readOnly))
		assert.Equal(t, "repeatable read", isolation)
		assert.Equal(t, "on", readOnly)
	})

	t.Run("write_fails", func(t *testing.T) {
		client, err := openPreflightClient("postgres", enttest.NewSchemaURL(t))
		require.NoError(t, err)
		defer func() { _ = client.Close() }()
		err = client.HubSetting.Create().SetSection("x").SetValue(json.RawMessage(`{}`)).Exec(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "25006", "a read-only transaction refuses the write")
		tx, err := client.BeginTx(ctx, snapshotTxOptions(dialect.Postgres))
		require.NoError(t, err)
		err = tx.HubSetting.Create().SetSection("y").SetValue(json.RawMessage(`{}`)).Exec(ctx)
		_ = tx.Rollback()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "25006")
	})

	t.Run("pool", func(t *testing.T) {
		client, err := openPreflightClient("postgres", enttest.NewSchemaURL(t))
		require.NoError(t, err)
		defer func() { _ = client.Close() }()
		assert.Equal(t, 2, client.Driver().(*entsql.Driver).DB().Stats().MaxOpenConnections)
	})

	shadowed := func(t *testing.T) (dsn, empty, hub string) {
		t.Helper()
		hubURL := enttest.NewSchemaURL(t)
		emptyURL := enttest.NewEmptySchemaURL(t)
		hub, empty = currentSchema(t, hubURL), currentSchema(t, emptyURL)
		dsn, err := enttest.WithConnParam(hubURL, "search_path", empty+","+hub)
		require.NoError(t, err)
		return dsn, empty, hub
	}

	t.Run("shadow_refuses", func(t *testing.T) {
		newPreflightHub(t)
		t.Setenv("SCION_SERVER_DATABASE_DRIVER", "postgres")
		dsn, empty, hub := shadowed(t)
		stdout, _, err := runPreflightForTest(t, preflightOptions{executor: true, brokerID: uuid.NewString(), envDSN: dsn})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the restarted hub would refuse to start: PostgreSQL schema \""+empty+"\"")
		assert.Contains(t, err.Error(), "schema \""+hub+"\" on the search_path")
		assert.Contains(t, err.Error(), "server.database.allow_shadowed_schema")
		assert.Equal(t, 1, exitCodeFor(err))
		assert.Empty(t, stdout)
	})

	t.Run("shadow_override_reads", func(t *testing.T) {
		newPreflightHub(t)
		t.Setenv("SCION_SERVER_DATABASE_DRIVER", "postgres")
		t.Setenv(allowShadowedSchemaEnv, "true")
		dsn, _, _ := shadowed(t)
		stdout, _, err := runPreflightForTest(t, preflightOptions{executor: true, brokerID: uuid.NewString(), envDSN: dsn})
		// The reads go ahead against the shadowed tables: the broker ID has
		// no row there, so the check could not run (a block, exit 3).
		require.Error(t, err)
		assert.Equal(t, 3, exitCodeFor(err))
		assert.Contains(t, reportCodes(decodeReport(t, stdout)), "settings.profiles.could_not_run")
	})
}
