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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// runtime_brokers.workload_identity_targets (ptone/scion#3329 phase 5):
// never reported reads nil, an empty report reads empty, the heartbeat
// refresh writes it, and a flat row keeps it through the full-row writer's
// strip of profiles.
func TestRuntimeBroker_WorkloadIdentityTargets(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := NewProjectStore(client)
	legacy := newRecordsTestBroker(t, ctx, s, "55555555-5555-5555-5555-555555555555", "wi-legacy")

	got, err := s.GetRuntimeBroker(ctx, legacy.ID)
	require.NoError(t, err)
	assert.Nil(t, got.WorkloadIdentityTargets, "never reported")

	targets := []store.WorkloadIdentityTarget{{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "agents"}}
	require.NoError(t, s.RefreshRuntimeBrokerState(ctx, legacy.ID, func(b *store.RuntimeBroker) bool {
		b.WorkloadIdentityTargets = targets
		return true
	}))
	got, err = s.GetRuntimeBroker(ctx, legacy.ID)
	require.NoError(t, err)
	assert.Equal(t, targets, got.WorkloadIdentityTargets)

	require.NoError(t, s.RefreshRuntimeBrokerState(ctx, legacy.ID, func(b *store.RuntimeBroker) bool {
		b.WorkloadIdentityTargets = []store.WorkloadIdentityTarget{}
		return true
	}))
	got, err = s.GetRuntimeBroker(ctx, legacy.ID)
	require.NoError(t, err)
	require.NotNil(t, got.WorkloadIdentityTargets, "an empty report is not 'never reported'")
	assert.Empty(t, got.WorkloadIdentityTargets)

	flat := &store.RuntimeBroker{
		ID: "66666666-6666-6666-6666-666666666666", Name: "wi-flat", Slug: "wi-flat",
		Status: store.BrokerStatusOnline, Endpoint: "https://flat.example.test",
		RuntimeTarget:           &api.RuntimeTargetDescriptor{ID: "rt-wi-flat", Type: "kubernetes", DisplayName: "gke"},
		WorkloadIdentityTargets: []store.WorkloadIdentityTarget{{Pool: "cluster-project.svc.id.goog", Namespace: "agents"}},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, flat))
	cur, err := s.GetRuntimeBroker(ctx, flat.ID)
	require.NoError(t, err)
	require.True(t, cur.IsFlat())
	cur.Profiles = []store.BrokerProfile{{Name: "stripped", Type: "kubernetes"}}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, cur))
	got, err = s.GetRuntimeBroker(ctx, flat.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Profiles, "the flat strip drops profiles")
	assert.Equal(t, flat.WorkloadIdentityTargets, got.WorkloadIdentityTargets, "the column survives the flat strip")
}

// The column is additive: dropping it reproduces the table before it, and
// AutoMigrate adds it back with existing rows reading "never reported".
func TestRuntimeBroker_WorkloadIdentityTargetsColumnAdditiveUpgrade_SQLite(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "brokers.db")
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(ctx, client))
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	s := NewProjectStore(client)
	b := newRecordsTestBroker(t, ctx, s, "77777777-7777-7777-7777-777777777777", "wi-upgrade")

	_, err = raw.ExecContext(ctx, "ALTER TABLE runtime_brokers DROP COLUMN workload_identity_targets")
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(ctx, client))

	got, err := s.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Nil(t, got.WorkloadIdentityTargets)
	assert.Equal(t, "wi-upgrade", got.Name)
}
