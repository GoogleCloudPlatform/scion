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
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// writeHarnessConfigDirs creates globalDir/harness-configs/<name>/config.yaml
// for each name and returns the harness-configs directory.
func writeHarnessConfigDirs(t *testing.T, globalDir string, names ...string) string {
	t.Helper()
	hcDir := filepath.Join(globalDir, "harness-configs")
	for _, name := range names {
		dir := filepath.Join(hcDir, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
			[]byte("harness: claude\nimage: scion-claude:latest\nuser: scion\n"), 0o644))
	}
	return hcDir
}

// harnessesSeeded follows the rows the setup wizard lists (active global
// harness configs), not the directories on disk (ptone/scion#7461).
func TestHarnessesSeeded_FollowsStoreRows(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	globalDir := t.TempDir()
	hcDir := writeHarnessConfigDirs(t, globalDir, "my-config")

	assert.False(t, srv.harnessesSeeded(ctx, globalDir), "a directory with no row is not seeded")

	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, hcDir))
	assert.True(t, srv.harnessesSeeded(ctx, globalDir), "an active global row is seeded")
}

// Without a store the directory is the only source, and only its
// subdirectories holding config.yaml count.
func TestHarnessesSeeded_NoStoreReadsDisk(t *testing.T) {
	srv := &Server{}
	ctx := context.Background()

	empty := t.TempDir()
	assert.False(t, srv.harnessesSeeded(ctx, empty))

	noConfig := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(noConfig, "harness-configs", "not-a-config"), 0o755))
	assert.False(t, srv.harnessesSeeded(ctx, noConfig), "a directory without config.yaml is not a harness config")

	globalDir := t.TempDir()
	writeHarnessConfigDirs(t, globalDir, "my-config")
	assert.True(t, srv.harnessesSeeded(ctx, globalDir))
}

// Without a storage backend bootstrap creates no row, so the directory
// counts: the onboarding status is not held incomplete on a hub that cannot
// seed (ptone/scion#7461).
func TestHarnessesSeeded_NoStorageCountsTheDirectory(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	srv.SetStorage(nil)
	ctx := context.Background()

	assert.False(t, srv.harnessesSeeded(ctx, t.TempDir()))

	globalDir := t.TempDir()
	hcDir := writeHarnessConfigDirs(t, globalDir, "my-config")
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, hcDir))
	assert.True(t, srv.harnessesSeeded(ctx, globalDir))
}

// harnessListFailingStore is a store whose harness config listing fails
// while failing is set, and delegates otherwise.
type harnessListFailingStore struct {
	store.Store
	failing *atomic.Bool
}

func (w harnessListFailingStore) ListHarnessConfigs(ctx context.Context, f store.HarnessConfigFilter, o store.ListOptions) (*store.ListResult[store.HarnessConfig], error) {
	if !w.failing.Load() {
		return w.Store.ListHarnessConfigs(ctx, f, o)
	}
	return nil, errors.New("database is locked")
}

// A store error does not read as not seeded: the directory counts instead.
// The polled status logs one WARN per failure streak, then Debug; a good
// read ends the streak, so the next failure WARNs again.
func TestHarnessesSeeded_StoreErrorCountsTheDirectory(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	waitUserScopedDataSweep(t, srv)
	var failing atomic.Bool
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) harnessListFailingStore {
		return harnessListFailingStore{Store: inner, failing: &failing}
	})
	failing.Store(true)
	h := &levelCapturingHandler{}
	srv.resourceLog = slog.New(h)
	ctx := context.Background()

	assert.False(t, srv.harnessesSeeded(ctx, t.TempDir()))

	globalDir := t.TempDir()
	writeHarnessConfigDirs(t, globalDir, "my-config")
	assert.True(t, srv.harnessesSeeded(ctx, globalDir))
	assert.Equal(t, 1, countHarnessListWarns(h), "one WARN for the failure streak, not one per poll")

	failing.Store(false)
	srv.harnessesSeeded(ctx, globalDir)
	failing.Store(true)
	assert.True(t, srv.harnessesSeeded(ctx, globalDir))
	assert.Equal(t, 2, countHarnessListWarns(h), "a failure after a good read starts a new streak")
}

// countHarnessListWarns counts the WARN records of a failed harness-config
// listing.
func countHarnessListWarns(h *levelCapturingHandler) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if strings.Contains(r.Message, "listing harness configs failed") && r.Level == slog.LevelWarn {
			n++
		}
	}
	return n
}

// A ledger that cannot be loaded makes bootstrap create no missing
// built-in. The built-ins are on disk but no row can be created for them,
// so the directory counts and onboarding is not held incomplete.
func TestHarnessesSeeded_FailingLedgerLoadCountsTheDirectory(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	globalDir := t.TempDir()
	hcDir := filepath.Join(globalDir, "harness-configs")

	_, err := s.UpsertHubSetting(ctx, builtinSeedLedgerSection, json.RawMessage(`"not-a-ledger"`),
		"test", -1, "seeded")
	require.NoError(t, err)
	require.NoError(t, config.MaterializeBundledHarnessConfigs(globalDir, config.MaterializeOptions{Force: true}))

	require.Error(t, srv.BootstrapHarnessConfigsFromDir(ctx, hcDir), "an unreadable ledger is reported")
	list, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
	}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, list.Items, "premise: no built-in row was created")
	assert.True(t, srv.harnessesSeeded(ctx, globalDir))
}

// The hosted bootstrap (BootstrapBundledResources) records a ledger that
// cannot be loaded too: it creates no row, the harness-configs directory is
// materialized as hosted startup does, and the directory counts.
func TestHarnessesSeeded_HostedBootstrapFailingLedgerCountsTheDirectory(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	globalDir := t.TempDir()

	_, err := s.UpsertHubSetting(ctx, builtinSeedLedgerSection, json.RawMessage(`"not-a-ledger"`),
		"test", -1, "seeded")
	require.NoError(t, err)
	require.NoError(t, config.MaterializeBundledHarnessConfigs(globalDir, config.MaterializeOptions{}))

	require.Error(t, srv.BootstrapBundledResources(ctx, BootstrapOptions{}), "an unreadable ledger is reported")
	list, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
	}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, list.Items, "premise: no row was created")
	assert.True(t, srv.harnessesSeeded(ctx, globalDir))
}

// computeOnboardingStatus: with identity, a runtime and a harness config
// directory, onboarding is complete where no row can be created for the
// directory (no store, no storage backend), and incomplete on a hub that
// could seed it but has no row, until bootstrap creates one.
func TestComputeOnboardingStatus_HarnessesSeededAndComplete(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	require.NoError(t, os.WriteFile(settingsPath,
		[]byte("schema_version: \"1\"\nserver:\n  auth:\n    username: alice\n"), 0o644))
	restore := config.OverrideRuntimeDetection(
		func(string) (string, error) { return "/usr/bin/docker", nil },
		func(string, []string) error { return nil },
	)
	t.Cleanup(restore)
	globalDir := filepath.Dir(settingsPath)
	hcDir := writeHarnessConfigDirs(t, globalDir, "my-config")
	ctx := context.Background()

	t.Run("no store", func(t *testing.T) {
		srv := &Server{}
		st := srv.computeOnboardingStatus(ctx)
		assert.True(t, st.HarnessesSeeded)
		assert.True(t, st.Complete)
	})
	t.Run("no storage backend", func(t *testing.T) {
		srv, _, _ := testTemplateBootstrapServer(t)
		srv.SetStorage(nil)
		st := srv.computeOnboardingStatus(ctx)
		assert.True(t, st.HarnessesSeeded)
		assert.True(t, st.Complete)
	})
	t.Run("store and storage, rows decide", func(t *testing.T) {
		srv, _, _ := testTemplateBootstrapServer(t)
		st := srv.computeOnboardingStatus(ctx)
		require.True(t, st.Initialized && st.IdentitySet && st.RuntimeOK, "premise: %+v", st)
		assert.False(t, st.HarnessesSeeded, "a directory with no row is not seeded")
		assert.False(t, st.Complete)

		require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, hcDir))
		st = srv.computeOnboardingStatus(ctx)
		assert.True(t, st.HarnessesSeeded)
		assert.True(t, st.Complete)
	})
}

// The storage-missing skip names the directory and how many configs it
// skips (ptone/scion#7462).
func TestBootstrapHarnessConfigsFromDir_NoStorageWarnNamesDirAndCount(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	srv.SetStorage(nil)
	h := &levelCapturingHandler{}
	srv.resourceLog = slog.New(h)
	hcDir := writeHarnessConfigDirs(t, t.TempDir(), "one", "two")
	require.NoError(t, os.MkdirAll(filepath.Join(hcDir, "not-a-config"), 0o755))

	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(context.Background(), hcDir))

	h.mu.Lock()
	defer h.mu.Unlock()
	var found []slog.Record
	for _, r := range h.records {
		if strings.Contains(r.Message, "no storage backend configured") {
			found = append(found, r)
		}
	}
	require.Len(t, found, 1)
	assert.Equal(t, slog.LevelWarn, found[0].Level)
	dir, ok := recordAttr(found[0], "dir")
	require.True(t, ok)
	assert.Equal(t, hcDir, dir.String())
	skipped, ok := recordAttr(found[0], "skipped")
	require.True(t, ok)
	assert.Equal(t, int64(2), skipped.Int64())
}
