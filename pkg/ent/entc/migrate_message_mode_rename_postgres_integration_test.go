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

package entc_test

import (
	"context"
	"os"
	"testing"
	"time"

	entagent "github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateMessageModeRename_Postgres(t *testing.T) {
	baseDSN := os.Getenv("SCION_TEST_POSTGRES_URL")
	if baseDSN == "" {
		baseDSN = os.Getenv("SCION_PG_TEST_DSN")
	}
	if baseDSN == "" {
		t.Skip("SCION_TEST_POSTGRES_URL (or SCION_PG_TEST_DSN) not set; skipping Postgres integration test")
	}
	ctx := context.Background()

	dsn := newIsolatedPostgresSchema(t, baseDSN)
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	require.NoError(t, entc.AutoMigrate(ctx, client))

	projectID := uuid.New()
	_, err = client.Project.Create().
		SetID(projectID).
		SetName("PG Project").
		SetSlug("pg-project").
		Save(ctx)
	require.NoError(t, err)

	sqlDrv, ok := entc.SQLDriver(client.Driver())
	require.True(t, ok)

	createAgent := func(name string, mode entagent.MessageMode, deleted bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		b := client.Agent.Create().
			SetID(id).
			SetName(name).
			SetSlug(name).
			SetProjectID(projectID).
			SetMessageMode(mode)
		if deleted {
			b = b.SetDeletedAt(time.Now().UTC())
		}
		_, err := b.Save(ctx)
		require.NoError(t, err)
		return id
	}

	lineageID := createAgent("pg-lineage", entagent.MessageModeProject, false)
	branchID := createAgent("pg-branch", entagent.MessageModeProject, false)
	deletedLineageID := createAgent("pg-del-lineage", entagent.MessageModeProject, true)
	noneID := createAgent("pg-none", entagent.MessageModeNone, false)
	projectAgentID := createAgent("pg-project", entagent.MessageModeProject, false)

	_, err = sqlDrv.DB().ExecContext(ctx, `UPDATE agents SET message_mode = 'lineage' WHERE id IN ($1, $2)`, lineageID, deletedLineageID)
	require.NoError(t, err)
	_, err = sqlDrv.DB().ExecContext(ctx, `UPDATE agents SET message_mode = 'branch' WHERE id = $1`, branchID)
	require.NoError(t, err)

	// Re-run AutoMigrate twice to verify rewrite and idempotency on Postgres.
	for range 2 {
		require.NoError(t, entc.AutoMigrate(ctx, client))
	}

	want := map[uuid.UUID]entagent.MessageMode{
		lineageID:        entagent.MessageModeOwner,
		branchID:         entagent.MessageModeDirect,
		deletedLineageID: entagent.MessageModeOwner,
		noneID:           entagent.MessageModeNone,
		projectAgentID:   entagent.MessageModeProject,
	}
	for id, wantMode := range want {
		got, err := client.Agent.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, wantMode, got.MessageMode, "agent %s", got.Name)
	}

	for _, id := range []uuid.UUID{lineageID, branchID} {
		got, err := client.Agent.Get(ctx, id)
		require.NoError(t, err)
		_, err = client.Agent.UpdateOneID(id).
			SetMessageMode(got.MessageMode).
			SetPhase("running").
			Save(ctx)
		require.NoError(t, err)
	}
}
