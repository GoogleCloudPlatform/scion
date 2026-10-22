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
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// brokerPresentationColumns are the six runtime_brokers columns of a flat
// Runtime Broker's presentation; dropping them reproduces the table shape
// before they were added.
var brokerPresentationColumns = []string{
	"runtime_target_presentation", "runtime_target_presentation_hash", "presentation_epoch",
	"presentation_seq", "presentation_fenced_at", "presentation_observed_at",
}

// runBrokerPresentationColumnsAdditiveUpgrade writes a flat and a legacy row,
// drops the presentation columns, re-runs AutoMigrate, and checks that the
// existing rows read no presentation and keep their other values, and that
// the fenced presentation writer works on an upgraded row.
func runBrokerPresentationColumnsAdditiveUpgrade(t *testing.T, client *ent.Client, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	cs := NewCompositeStore(client)

	flatID := uuid.NewString()
	target := &api.RuntimeTargetDescriptor{ID: "rt-" + flatID, Type: "kubernetes", DisplayName: "Cluster A"}
	require.NoError(t, cs.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: flatID, Name: "flat", Slug: "flat", Status: store.BrokerStatusOnline, RuntimeTarget: target,
	}))
	legacyID := uuid.NewString()
	require.NoError(t, cs.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: legacyID, Name: "legacy", Slug: "legacy", Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}},
	}))
	evict := false
	p := &api.RuntimeTargetPresentation{
		SchemaVersion: 1, Key: "k8s-1", Name: "k8s one",
		Target: &api.PresentedTarget{Type: "kubernetes", DisplayName: "Cluster A", Namespace: "ns-a", SafeToEvict: &evict},
	}
	now := time.Now().UTC().Truncate(time.Second)
	out, err := cs.SetRuntimeBrokerPresentation(ctx, flatID, target.ID, p, "h1", 100, 1, now)
	require.NoError(t, err)
	require.Equal(t, store.PresentationWritten, out)

	for _, col := range brokerPresentationColumns {
		stmt := "ALTER TABLE runtime_brokers DROP COLUMN " + col
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	require.NoError(t, entc.AutoMigrate(ctx, client))
	require.NoError(t, entc.AutoMigrate(ctx, client), "AutoMigrate must be idempotent")

	for _, id := range []string{flatID, legacyID} {
		got, err := cs.GetRuntimeBroker(ctx, id)
		require.NoError(t, err)
		assert.Nil(t, got.RuntimeTargetPresentation, "%s: no presentation after the upgrade", got.Name)
		assert.Empty(t, got.RuntimeTargetPresentationHash, got.Name)
		assert.Nil(t, got.RuntimeTargetPresentationObservedAt, got.Name)
		assert.Nil(t, got.PresentationEpoch, got.Name)
		assert.Nil(t, got.PresentationSeq, got.Name)
		assert.Nil(t, got.PresentationFencedAt, got.Name)
	}
	got, err := cs.GetRuntimeBroker(ctx, flatID)
	require.NoError(t, err)
	assert.Equal(t, target, got.RuntimeTarget, "the runtime target is kept")

	// The fenced writer works on the upgraded row.
	out, err = cs.SetRuntimeBrokerPresentation(ctx, flatID, target.ID, p, "h1", 100, 1, now)
	require.NoError(t, err)
	assert.Equal(t, store.PresentationWritten, out)
	got, err = cs.GetRuntimeBroker(ctx, flatID)
	require.NoError(t, err)
	assert.Equal(t, p, got.RuntimeTargetPresentation)
	assert.Equal(t, "h1", got.RuntimeTargetPresentationHash)
}

func TestRuntimeBrokerPresentationColumns_AdditiveUpgrade_SQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "brokers.db")
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	runBrokerPresentationColumnsAdditiveUpgrade(t, client, raw)
}

// TestRuntimeBrokerPresentationColumns_AdditiveUpgrade_Postgres skips unless
// the enttest Postgres backend is built (-tags integration) and
// SCION_TEST_POSTGRES_URL is set.
func TestRuntimeBrokerPresentationColumns_AdditiveUpgrade_Postgres(t *testing.T) {
	url := enttest.NewSchemaURL(t)
	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	raw, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	runBrokerPresentationColumnsAdditiveUpgrade(t, client, raw)
}
