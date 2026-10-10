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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/require"
)

// scion server backfill opens its store with the same pre-migration step as
// server start, so a hub database holding duplicate access_policies,
// delegation_edges and agent_session_metrics rows migrates instead of
// failing on the unique indexes.
func TestOpenBackfillStore_RunsPreMigrate(t *testing.T) {
	origDB := backfillDB
	origConfigPath := serverConfigPath
	defer func() {
		backfillDB = origDB
		serverConfigPath = origConfigPath
	}()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "hub.db")
	enttest.SeedPreMigrateDuplicates(t, dbPath)

	backfillDB = dbPath
	serverConfigPath = filepath.Join(tmpDir, "nonexistent.yaml")
	s, err := openBackfillStore(context.Background())
	require.NoError(t, err)
	require.NoError(t, s.Close())

	enttest.AssertPreMigrateDeduplicated(t, dbPath)
}

// scion server migrate-dm-keys opens its store with the same pre-migration
// step as server start; see TestOpenBackfillStore_RunsPreMigrate.
func TestOpenDMMigrationStore_RunsPreMigrate(t *testing.T) {
	origDB := dmMigrationDB
	origConfigPath := serverConfigPath
	defer func() {
		dmMigrationDB = origDB
		serverConfigPath = origConfigPath
	}()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "hub.db")
	enttest.SeedPreMigrateDuplicates(t, dbPath)

	dmMigrationDB = dbPath
	serverConfigPath = filepath.Join(tmpDir, "nonexistent.yaml")
	s, err := openDMMigrationStore(context.Background())
	require.NoError(t, err)
	require.NoError(t, s.Close())

	enttest.AssertPreMigrateDeduplicated(t, dbPath)
}
