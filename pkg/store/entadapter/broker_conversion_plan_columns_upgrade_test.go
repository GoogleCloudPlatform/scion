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

// brokerConversionPlanColumns are the three runtime_brokers columns of a
// legacy Runtime Broker's conversion plan; dropping them reproduces the
// table shape before they were added.
var brokerConversionPlanColumns = []string{
	"conversion_plan_presentation", "conversion_plan_presentation_hash", "conversion_plan_observed_at",
}

// runBrokerConversionPlanColumnsAdditiveUpgrade writes a legacy row with a
// plan, drops the conversion plan columns, re-runs AutoMigrate, and checks
// that the row reads no plan and keeps its other values, and that the
// writer works on the upgraded row.
func runBrokerConversionPlanColumnsAdditiveUpgrade(t *testing.T, client *ent.Client, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	cs := NewCompositeStore(client)

	legacyID := uuid.NewString()
	require.NoError(t, cs.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: legacyID, Name: "legacy", Slug: "legacy", Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}},
	}))
	now := time.Now().UTC().Truncate(time.Second)
	p := &api.ConversionPlanPresentation{
		SchemaVersion: api.ConversionPlanPresentationSchemaVersion,
		Instances:     []api.RuntimeTargetPresentation{{SchemaVersion: 1, Key: "docker-1", Name: "docker-1", Target: &api.PresentedTarget{Type: "docker"}}},
		ProfileMap:    map[string]string{"local": "docker-1"},
		Findings:      []api.ConversionPlanFinding{},
		ComputedAt:    now,
	}
	out, err := cs.SetRuntimeBrokerConversionPlan(ctx, legacyID, p, "h1", now)
	require.NoError(t, err)
	require.Equal(t, store.PresentationWritten, out)

	for _, col := range brokerConversionPlanColumns {
		stmt := "ALTER TABLE runtime_brokers DROP COLUMN " + col
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	require.NoError(t, entc.AutoMigrate(ctx, client))
	require.NoError(t, entc.AutoMigrate(ctx, client), "AutoMigrate must be idempotent")

	got, err := cs.GetRuntimeBroker(ctx, legacyID)
	require.NoError(t, err)
	assert.Nil(t, got.ConversionPlanPresentation, "no plan after the upgrade")
	assert.Empty(t, got.ConversionPlanHash)
	assert.Nil(t, got.ConversionPlanObservedAt)
	require.Len(t, got.Profiles, 1, "the row keeps its other values")
	assert.Equal(t, "local", got.Profiles[0].Name)

	out, err = cs.SetRuntimeBrokerConversionPlan(ctx, legacyID, p, "h1", now)
	require.NoError(t, err)
	assert.Equal(t, store.PresentationWritten, out)
	got, err = cs.GetRuntimeBroker(ctx, legacyID)
	require.NoError(t, err)
	require.NotNil(t, got.ConversionPlanPresentation)
	assert.Equal(t, "h1", got.ConversionPlanHash)
}

func TestRuntimeBrokerConversionPlanColumns_AdditiveUpgrade_SQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "brokers.db")
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	runBrokerConversionPlanColumnsAdditiveUpgrade(t, client, raw)
}

// TestRuntimeBrokerConversionPlanColumns_AdditiveUpgrade_Postgres skips
// unless the enttest Postgres backend is built (-tags integration) and
// SCION_TEST_POSTGRES_URL is set.
func TestRuntimeBrokerConversionPlanColumns_AdditiveUpgrade_Postgres(t *testing.T) {
	url := enttest.NewSchemaURL(t)
	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	raw, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	runBrokerConversionPlanColumnsAdditiveUpgrade(t, client, raw)
}
