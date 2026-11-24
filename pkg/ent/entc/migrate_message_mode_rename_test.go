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

package entc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	entagent "github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateMessageModeRename_SQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	client, err := OpenSQLite(path, PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, AutoMigrate(ctx, client))

	projectID := uuid.New()
	_, err = client.Project.Create().
		SetID(projectID).
		SetName("Test Project").
		SetSlug("test-project").
		Save(ctx)
	require.NoError(t, err)

	sqlDrv, ok := SQLDriver(client.Driver())
	require.True(t, ok)

	// Create agents via Ent (valid modes first) then inject retired values
	// directly via raw SQL to simulate a pre-migration database.
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

	lineageID := createAgent("ag-lineage", entagent.MessageModeProject, false)
	branchID := createAgent("ag-branch", entagent.MessageModeProject, false)
	deletedLineageID := createAgent("ag-del-lineage", entagent.MessageModeProject, true)
	deletedBranchID := createAgent("ag-del-branch", entagent.MessageModeProject, true)
	noneID := createAgent("ag-none", entagent.MessageModeNone, false)
	projectAgentID := createAgent("ag-project", entagent.MessageModeProject, false)
	hubID := createAgent("ag-hub", entagent.MessageModeHub, false)
	treeID := createAgent("ag-tree", entagent.MessageModeTree, false)

	_, err = sqlDrv.DB().ExecContext(ctx, `UPDATE agents SET message_mode = 'lineage' WHERE id IN (?, ?)`, lineageID.String(), deletedLineageID.String())
	require.NoError(t, err)
	_, err = sqlDrv.DB().ExecContext(ctx, `UPDATE agents SET message_mode = 'branch' WHERE id IN (?, ?)`, branchID.String(), deletedBranchID.String())
	require.NoError(t, err)

	// Run AutoMigrate twice to verify rewrite and idempotency.
	require.NoError(t, AutoMigrate(ctx, client))
	require.NoError(t, AutoMigrate(ctx, client))

	want := map[uuid.UUID]entagent.MessageMode{
		lineageID:        entagent.MessageModeOwner,
		branchID:         entagent.MessageModeDirect,
		deletedLineageID: entagent.MessageModeOwner,
		deletedBranchID:  entagent.MessageModeDirect,
		noneID:           entagent.MessageModeNone,
		projectAgentID:   entagent.MessageModeProject,
		hubID:            entagent.MessageModeHub,
		treeID:           entagent.MessageModeTree,
	}
	for id, wantMode := range want {
		got, err := client.Agent.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, wantMode, got.MessageMode, "agent %s", got.Name)
	}

	// Verify round-trip through Ent UpdateOneID succeeds on migrated rows
	// (confirming MessageModeValidator accepts the rewritten values).
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
