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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// brokerColumnsAddedForGCPHost are the runtime_brokers columns this change
// adds; dropping them reproduces the pre-change table shape.
var brokerColumnsAddedForGCPHost = []string{"gcp_host_source"}

// runBrokerColumnsAdditiveUpgrade writes rows, drops the new columns to
// reproduce the pre-change shape, re-runs AutoMigrate, and checks that the
// existing rows read the new columns as empty, keep their GCP host values,
// and get the expected source from the one-time backfill.
func runBrokerColumnsAdditiveUpgrade(t *testing.T, client *ent.Client, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	cs := NewCompositeStore(client)

	mk := func(name, email, createdBy string, labels map[string]string) string {
		id := uuid.NewString()
		b := &store.RuntimeBroker{ID: id, Name: name, Slug: name, Status: store.BrokerStatusOnline, CreatedBy: createdBy, Labels: labels}
		if email != "" {
			b.GCPHostServiceAccountEmail, b.GCPHostProjectID = email, "proj"
		}
		require.NoError(t, cs.CreateRuntimeBroker(ctx, b))
		return id
	}
	colocated := mk("colocated", "host@proj.iam.gserviceaccount.com", "", map[string]string{store.BrokerRoleLabel: store.BrokerRoleEmbedded})
	remote := mk("remote", "remote@proj.iam.gserviceaccount.com", "user-1", nil)
	unset := mk("unset", "", "user-1", nil)

	for _, col := range brokerColumnsAddedForGCPHost {
		stmt := "ALTER TABLE runtime_brokers DROP COLUMN " + col
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	require.NoError(t, entc.AutoMigrate(ctx, client))
	require.NoError(t, entc.AutoMigrate(ctx, client), "AutoMigrate must be idempotent")

	for _, id := range []string{colocated, remote, unset} {
		got, err := cs.GetRuntimeBroker(ctx, id)
		require.NoError(t, err)
		assert.Empty(t, got.GCPHostSource, "%s: new column reads empty", got.Name)
	}
	got, err := cs.GetRuntimeBroker(ctx, remote)
	require.NoError(t, err)
	assert.Equal(t, "remote@proj.iam.gserviceaccount.com", got.GCPHostServiceAccountEmail, "existing values are kept")
	assert.Equal(t, "proj", got.GCPHostProjectID)

	require.NoError(t, cs.BackfillRuntimeBrokerGCPHostSource(ctx))
	for id, want := range map[string]string{colocated: store.GCPHostSourceDetected, remote: store.GCPHostSourceAdmin, unset: ""} {
		got, err := cs.GetRuntimeBroker(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, got.GCPHostSource, got.Name)
	}

	// The new writers work on upgraded rows.
	applied, err := cs.SetRuntimeBrokerGCPHostIfUnset(ctx, unset, "filled@proj.iam.gserviceaccount.com", "proj", store.GCPHostSourceRegistration)
	require.NoError(t, err)
	assert.True(t, applied)
	applied, err = cs.SetRuntimeBrokerGCPHostIfUnset(ctx, remote, "other@proj.iam.gserviceaccount.com", "proj", store.GCPHostSourceRegistration)
	require.NoError(t, err)
	assert.False(t, applied, "the backfilled admin source keeps the existing value")
}

func TestRuntimeBrokerGCPHostColumns_AdditiveUpgrade_SQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "brokers.db")
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	runBrokerColumnsAdditiveUpgrade(t, client, raw)
}

// TestRuntimeBrokerGCPHostColumns_AdditiveUpgrade_Postgres skips unless the
// enttest Postgres backend is built (-tags integration) and
// SCION_TEST_POSTGRES_URL is set.
func TestRuntimeBrokerGCPHostColumns_AdditiveUpgrade_Postgres(t *testing.T) {
	url := enttest.NewSchemaURL(t)
	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	raw, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	runBrokerColumnsAdditiveUpgrade(t, client, raw)
}
