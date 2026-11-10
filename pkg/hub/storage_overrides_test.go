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

package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removed per-profile storage overrides stored in the hub's settings are
// reported, live, as a degraded health check and in the admin server config;
// an admin save that removes them clears both (ptone/scion#6371).
func TestRemovedStorageOverrides_HealthAndAdminConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fakeStore, ops := newTestDBServer(t)

	checks := map[string]string{}
	srv.checkRemovedStorageOverrides(checks)
	assert.NotContains(t, checks, storageOverridesCheck, "no overrides: no check entry")
	assert.Empty(t, getServerConfigDB(t, srv, ops).StorageOverrideErrors)

	fakeStore.seed("profiles", json.RawMessage(`{"nfs-profile":{"runtime":"cluster","shared_dir_storage_backend":"nfs"}}`))
	_, _ = ops.Refresh(context.Background())

	checks = map[string]string{}
	srv.checkRemovedStorageOverrides(checks)
	require.Contains(t, checks, storageOverridesCheck)
	assert.True(t, strings.HasPrefix(checks[storageOverridesCheck], "degraded:"), checks[storageOverridesCheck])
	assert.NotContains(t, checks[storageOverridesCheck], "nfs-profile", "the health text names no profile")
	assert.False(t, criticalHealthChecks[storageOverridesCheck], "the check never makes the hub unhealthy")

	resp := getServerConfigDB(t, srv, ops)
	require.Len(t, resp.StorageOverrideErrors, 1)
	assert.Contains(t, resp.StorageOverrideErrors[0], "the hub's stored settings")
	assert.Contains(t, resp.StorageOverrideErrors[0], `profiles.nfs-profile.shared_dir_storage_backend ("nfs")`)

	// The overrides are removed (as an admin save does): both clear.
	fakeStore.seed("profiles", json.RawMessage(`{"nfs-profile":{"runtime":"cluster"}}`))
	_, _ = ops.Refresh(context.Background())
	checks = map[string]string{}
	srv.checkRemovedStorageOverrides(checks)
	assert.NotContains(t, checks, storageOverridesCheck)
	assert.Empty(t, getServerConfigDB(t, srv, ops).StorageOverrideErrors)
}

// The settings-file overrides found at startup are reported the same way.
func TestRemovedStorageOverrides_FileErrorReported(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _, ops := newTestDBServer(t)
	srv.config.RemovedStorageOverridesFileError = "the global settings file sets a removed storage override: profiles.p.home_storage_leaf (\"pod\")."

	checks := map[string]string{}
	srv.checkRemovedStorageOverrides(checks)
	assert.Contains(t, checks, storageOverridesCheck)
	assert.Equal(t, []string{srv.config.RemovedStorageOverridesFileError}, getServerConfigDB(t, srv, ops).StorageOverrideErrors)
}
