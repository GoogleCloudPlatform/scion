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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Tests for ptone/scion#2898: harness-config hash-mismatch repair must act on
// the record the agent was stamped with (by ID), or — for name-only agents —
// on the project-scoped then global record, never on "the newest record with
// that name in any scope".

const repairStaleHash = "stale-db-hash"
const repairStorageHash = "actual-storage-hash"

type repairScopeFixture struct {
	srv      *Server
	store    store.Store
	projectA *store.Project
	projectB *store.Project
	global   *store.HarnessConfig
	inA      *store.HarnessConfig
}

func newRepairScopeFixture(t *testing.T) *repairScopeFixture {
	t.Helper()
	ctx := context.Background()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(ctx))
	t.Cleanup(func() { _ = s.Close() })

	srv := &Server{store: s, resourceLog: logging.Subsystem("hub.resources")}
	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)

	f := &repairScopeFixture{srv: srv, store: s}
	for _, p := range []**store.Project{&f.projectA, &f.projectB} {
		name := "repair-proj-a"
		if p == &f.projectB {
			name = "repair-proj-b"
		}
		proj := &store.Project{ID: tid(name + t.Name()), Name: name, Slug: name}
		require.NoError(t, s.CreateProject(ctx, proj))
		*p = proj
	}

	mk := func(id, scope, scopeID, storagePath string) *store.HarnessConfig {
		hc := &store.HarnessConfig{
			ID:          tid(id + t.Name()),
			Name:        "claude",
			Slug:        "claude",
			Harness:     "claude",
			Scope:       scope,
			ScopeID:     scopeID,
			StoragePath: storagePath,
			Files:       []store.TemplateFile{{Path: "config.yaml", Hash: repairStaleHash}},
			ContentHash: "stale-content",
			Status:      store.HarnessConfigStatusActive,
		}
		require.NoError(t, s.CreateHarnessConfig(ctx, hc))
		// Storage holds the real content for every record, so a repair of
		// either record would "succeed" — the test distinguishes which one
		// was touched by which DB row changed.
		_, err := stor.Upload(ctx, storagePath+"/config.yaml", nil, storage.UploadOptions{
			Metadata: map[string]string{"sha256": repairStorageHash},
		})
		require.NoError(t, err)
		return hc
	}
	// Global first, then project A: under the old "newest by name" lookup the
	// project-A record wins for everyone.
	f.global = mk("hc-global", store.HarnessConfigScopeGlobal, "", "hc/global/claude")
	time.Sleep(10 * time.Millisecond)
	f.inA = mk("hc-proj-a", store.HarnessConfigScopeProject, f.projectA.ID, "hc/project-a/claude")
	return f
}

func (f *repairScopeFixture) repaired(t *testing.T, hc *store.HarnessConfig) bool {
	t.Helper()
	got, err := f.store.GetHarnessConfig(context.Background(), hc.ID)
	require.NoError(t, err)
	require.Len(t, got.Files, 1)
	return got.Files[0].Hash == repairStorageHash
}

func TestHarnessConfigRepair_NameOnlyOtherProjectRepairsGlobal(t *testing.T) {
	f := newRepairScopeFixture(t)
	err := f.srv.syncHarnessConfigFromStorage(context.Background(), HarnessConfigRepairRef{
		Name: "claude", ProjectID: f.projectB.ID,
	})
	require.NoError(t, err)
	assert.True(t, f.repaired(t, f.global), "project B has no 'claude'; the global record must be repaired")
	assert.False(t, f.repaired(t, f.inA), "project A's record must not be touched by a project-B agent")
}

func TestHarnessConfigRepair_GlobalStampedIDRepairsGlobal(t *testing.T) {
	f := newRepairScopeFixture(t)
	// A global-stamped agent living in project A: the ID wins over the
	// project-first name fallback.
	err := f.srv.syncHarnessConfigFromStorage(context.Background(), HarnessConfigRepairRef{
		ID: f.global.ID, Name: "claude", ProjectID: f.projectA.ID,
	})
	require.NoError(t, err)
	assert.True(t, f.repaired(t, f.global))
	assert.False(t, f.repaired(t, f.inA), "the stamped ID is authoritative over the name fallback")
}

func TestHarnessConfigRepair_NameOnlyProjectARepairsProjectA(t *testing.T) {
	f := newRepairScopeFixture(t)
	err := f.srv.syncHarnessConfigFromStorage(context.Background(), HarnessConfigRepairRef{
		Name: "claude", ProjectID: f.projectA.ID,
	})
	require.NoError(t, err)
	assert.True(t, f.repaired(t, f.inA))
	assert.False(t, f.repaired(t, f.global))
}

// A stamped ID that no longer exists is "not found": no name fallback, so no
// unrelated same-named record is touched (the dispatch retry still carries
// the stale ID/hash, so re-targeting could not help it succeed).
func TestHarnessConfigRepair_StaleIDDoesNotFallBackToName(t *testing.T) {
	f := newRepairScopeFixture(t)
	err := f.srv.syncHarnessConfigFromStorage(context.Background(), HarnessConfigRepairRef{
		ID: tid("deleted-record"), Name: "claude", ProjectID: f.projectA.ID,
	})
	require.ErrorContains(t, err, "not found")
	assert.False(t, f.repaired(t, f.global))
	assert.False(t, f.repaired(t, f.inA))
}

// The ID-only path (sync-all passes just the record ID).
func TestHarnessConfigRepair_IDOnlyRepairsThatRecord(t *testing.T) {
	f := newRepairScopeFixture(t)
	err := f.srv.syncHarnessConfigFromStorage(context.Background(), HarnessConfigRepairRef{ID: f.inA.ID})
	require.NoError(t, err)
	assert.True(t, f.repaired(t, f.inA))
	assert.False(t, f.repaired(t, f.global))
}

func TestHarnessConfigRepair_UnknownNameNotFound(t *testing.T) {
	f := newRepairScopeFixture(t)
	err := f.srv.syncHarnessConfigFromStorage(context.Background(), HarnessConfigRepairRef{
		Name: "no-such-config", ProjectID: f.projectA.ID,
	})
	require.ErrorContains(t, err, "not found")
}

// Sync-all must repair each listed record by its own ID. Both same-named
// records (global and project A) are stale; a name-based call would resolve
// both to the same record (global) and leave project A's unrepaired.
func TestSyncAllHarnessConfigsFromStorage_RepairsEachSameNamedRecordByID(t *testing.T) {
	f := newRepairScopeFixture(t)

	f.srv.SyncAllHarnessConfigsFromStorage(context.Background())

	assert.True(t, f.repaired(t, f.global), "the global record must be repaired")
	assert.True(t, f.repaired(t, f.inA), "the project-A record must be repaired by its own ID")
}

// The dispatcher repairer must hand the stamped ID and the agent's project to
// the repair callback, not just the name.
func TestHTTPDispatcher_RepairHarnessConfigPassesIDAndProject(t *testing.T) {
	d := NewHTTPAgentDispatcherWithClient(nil, nil, false, slog.Default())
	var got HarnessConfigRepairRef
	d.SetHarnessConfigRepairer(func(_ context.Context, ref HarnessConfigRepairRef) error {
		got = ref
		return nil
	})
	agent := &store.Agent{
		Slug:      "a",
		ProjectID: "proj-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig:   "claude",
			HarnessConfigID: "hc-id-1",
		},
	}
	err := d.repairHashMismatch(context.Background(), agent,
		errors.New("Failed to hydrate harness-config: hash mismatch for file config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, HarnessConfigRepairRef{ID: "hc-id-1", Name: "claude", ProjectID: "proj-1"}, got)
}

// An agent stamped with an ID but no name (the case the dispatcher guard now
// admits) is still repaired, by ID.
func TestHTTPDispatcher_RepairHarnessConfigIDOnlyAgent(t *testing.T) {
	d := NewHTTPAgentDispatcherWithClient(nil, nil, false, slog.Default())
	var got HarnessConfigRepairRef
	called := false
	d.SetHarnessConfigRepairer(func(_ context.Context, ref HarnessConfigRepairRef) error {
		got, called = ref, true
		return nil
	})
	agent := &store.Agent{
		Slug:          "a",
		ProjectID:     "proj-1",
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfigID: "hc-id-only"},
	}
	err := d.repairHashMismatch(context.Background(), agent,
		errors.New("Failed to hydrate harness-config: hash mismatch for file config.yaml"))
	require.NoError(t, err)
	require.True(t, called)
	assert.Equal(t, HarnessConfigRepairRef{ID: "hc-id-only", ProjectID: "proj-1"}, got)
}
