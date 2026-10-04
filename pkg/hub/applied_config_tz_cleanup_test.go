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
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tzCleanupFixture creates a project and one agent per legacy TZ shape the
// cleanup has to handle.
type tzCleanupFixture struct {
	project *store.Project
	// envOnly: TZ only in AppliedConfig.Env, with no live plain source.
	envOnly string
	// inlineMatch: the same TZ in Env and InlineConfig.Env (configure-page pin).
	inlineMatch string
	// inlineOnly: TZ only in InlineConfig.Env (diverged copies).
	inlineOnly string
	// storageMatch: Env TZ equal to a project-scope plain env var.
	storageMatch string
	// pinned: an existing explicit pin plus a stale Env TZ.
	pinned string
	// emptyMarker: an empty TZ record.
	emptyMarker string
	// clean: no TZ anywhere.
	clean string
	// unpinned: an explicit unpin plus a TZ that reappeared in Env.
	unpinned string
	// softDeleted: a soft-deleted agent with an Env-only TZ.
	softDeleted string
}

// all returns every fixture agent ID.
func (f tzCleanupFixture) all() []string {
	return []string{f.envOnly, f.inlineMatch, f.inlineOnly, f.storageMatch, f.pinned, f.emptyMarker, f.clean, f.unpinned, f.softDeleted}
}

func newTZCleanupFixture(t *testing.T, s store.Store) tzCleanupFixture {
	t.Helper()
	ctx := context.Background()
	f := tzCleanupFixture{
		project:      &store.Project{ID: tid("project-tzc"), Name: "TZ Cleanup Project", Slug: "tzc-project"},
		envOnly:      tid("agent-tzc-env-only"),
		inlineMatch:  tid("agent-tzc-inline-match"),
		inlineOnly:   tid("agent-tzc-inline-only"),
		storageMatch: tid("agent-tzc-storage-match"),
		pinned:       tid("agent-tzc-pinned"),
		emptyMarker:  tid("agent-tzc-empty"),
		clean:        tid("agent-tzc-clean"),
		unpinned:     tid("agent-tzc-unpinned"),
		softDeleted:  tid("agent-tzc-soft-deleted"),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:      tid("envvar-tzc-tz"),
		Key:     "TZ",
		Value:   "America/New_York",
		Scope:   store.ScopeProject,
		ScopeID: f.project.ID,
	}))

	configs := map[string]*store.AgentAppliedConfig{
		f.envOnly: {Env: map[string]string{"TZ": "Asia/Kathmandu", "FOO": "bar"}},
		f.inlineMatch: {
			Env:          map[string]string{"TZ": "Europe/Paris"},
			InlineConfig: &api.ScionConfig{Env: map[string]string{"TZ": "Europe/Paris"}},
		},
		f.inlineOnly:   {InlineConfig: &api.ScionConfig{Env: map[string]string{"TZ": "Asia/Tokyo"}}},
		f.storageMatch: {Env: map[string]string{"TZ": "America/New_York"}},
		f.pinned:       {ExplicitTimezone: "UTC", Env: map[string]string{"TZ": "Europe/Berlin"}},
		f.emptyMarker:  {Env: map[string]string{"TZ": ""}},
		f.clean:        {Env: map[string]string{"FOO": "bar"}},
		f.unpinned:     {ExplicitTimezoneUnpinned: true, Env: map[string]string{"TZ": "Europe/Rome"}},
		f.softDeleted:  {Env: map[string]string{"TZ": "Australia/Adelaide"}},
	}
	for id, ac := range configs {
		a := &store.Agent{
			ID:            id,
			Slug:          id,
			Name:          id,
			ProjectID:     f.project.ID,
			AppliedConfig: ac,
		}
		if id == f.softDeleted {
			a.DeletedAt = time.Now()
		}
		require.NoError(t, s.CreateAgent(ctx, a))
	}
	return f
}

func loadAppliedConfig(t *testing.T, s store.Store, id string) *store.AgentAppliedConfig {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, a.AppliedConfig)
	return a.AppliedConfig
}

// assertLegacyPin checks the agent holds a legacy pin of want, reports
// source "legacy", and has no TZ left in either env copy.
func assertLegacyPin(t *testing.T, s store.Store, id, want string) {
	t.Helper()
	ac := loadAppliedConfig(t, s, id)
	assert.Equal(t, want, ac.ExplicitTimezone, "agent %s pin", id)
	assert.True(t, ac.ExplicitTimezoneLegacy, "agent %s legacy flag", id)
	assert.Equal(t, agentTZ{TZ: want, Source: TZSourceLegacy}, chooseAgentTZ(ac, agentTZ{}, "", false))
	assert.False(t, appliedConfigHasEnvTZ(ac), "agent %s env TZ must be stripped", id)
}

func runTZCleanup(t *testing.T, s store.Store, params map[string]string) (appliedConfigTZCleanupResult, string) {
	t.Helper()
	var buf bytes.Buffer
	result, err := (&AppliedConfigTZCleanupExecutor{Store: s}).run(context.Background(), &buf, params)
	require.NoError(t, err)
	return result, buf.String()
}

func runEnvCleanup(t *testing.T, s store.Store) {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, (&AppliedConfigEnvCleanupExecutor{Store: s}).Run(context.Background(), &buf, nil))
}

func TestAppliedConfigTZCleanupAdoptsAndCounts(t *testing.T) {
	s := createTestStore(t)
	f := newTZCleanupFixture(t, s)

	result, log := runTZCleanup(t, s, nil)
	assert.Equal(t, 9, result.AgentsScanned, "soft-deleted agents are scanned too")
	assert.Equal(t, 5, result.AgentsAdopted, "envOnly, inlineMatch, inlineOnly, storageMatch and softDeleted are adopted")
	assert.Equal(t, 3, result.AgentsStripped, "pinned, emptyMarker and unpinned are only stripped")
	assert.Contains(t, log, "Adopted 5 agent TZ value(s)")
	assert.Contains(t, log, "stripped TZ from 3 agent(s) without a new pin")
	assert.Contains(t, log, "ADOPT agent="+f.envOnly+" source=legacy")
	assert.NotContains(t, log, "Asia/Kathmandu", "TZ values are never logged")

	assertLegacyPin(t, s, f.envOnly, "Asia/Kathmandu")
	assertLegacyPin(t, s, f.inlineMatch, "Europe/Paris")
	assertLegacyPin(t, s, f.inlineOnly, "Asia/Tokyo")
	assertLegacyPin(t, s, f.storageMatch, "America/New_York")
	assertLegacyPin(t, s, f.softDeleted, "Australia/Adelaide")
	assert.Equal(t, "bar", loadAppliedConfig(t, s, f.envOnly).Env["FOO"], "other env keys are untouched")

	unpinned := loadAppliedConfig(t, s, f.unpinned)
	assert.Empty(t, unpinned.ExplicitTimezone, "an explicit unpin is not re-pinned")
	assert.False(t, unpinned.ExplicitTimezoneLegacy)
	assert.True(t, unpinned.ExplicitTimezoneUnpinned)
	assert.False(t, appliedConfigHasEnvTZ(unpinned))

	pinned := loadAppliedConfig(t, s, f.pinned)
	assert.Equal(t, "UTC", pinned.ExplicitTimezone, "an existing pin is kept")
	assert.False(t, pinned.ExplicitTimezoneLegacy)
	assert.False(t, appliedConfigHasEnvTZ(pinned))

	empty := loadAppliedConfig(t, s, f.emptyMarker)
	assert.Empty(t, empty.ExplicitTimezone)
	assert.False(t, appliedConfigHasEnvTZ(empty))
}

func TestAppliedConfigTZCleanupIsIdempotent(t *testing.T) {
	s := createTestStore(t)
	f := newTZCleanupFixture(t, s)

	first, _ := runTZCleanup(t, s, nil)
	require.Equal(t, 5, first.AgentsAdopted)

	versions := map[string]int64{}
	for _, id := range f.all() {
		a, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		versions[id] = a.StateVersion
	}

	second, log := runTZCleanup(t, s, nil)
	assert.Equal(t, 0, second.AgentsAdopted, "a second run adopts 0")
	assert.Equal(t, 0, second.AgentsStripped)
	assert.Contains(t, log, "Adopted 0 agent TZ value(s)")
	for id, v := range versions {
		a, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, v, a.StateVersion, "agent %s must not be written by the second run", id)
	}
	assertLegacyPin(t, s, f.envOnly, "Asia/Kathmandu")
}

func TestAppliedConfigTZCleanupDryRunMakesNoChanges(t *testing.T) {
	s := createTestStore(t)
	f := newTZCleanupFixture(t, s)

	result, log := runTZCleanup(t, s, map[string]string{"dryRun": "true"})
	assert.Equal(t, 5, result.AgentsAdopted)
	assert.Equal(t, 3, result.AgentsStripped)
	assert.Contains(t, log, "DRY RUN")
	assert.Contains(t, log, "Would adopt 5")

	ac := loadAppliedConfig(t, s, f.envOnly)
	assert.Empty(t, ac.ExplicitTimezone)
	assert.Equal(t, "Asia/Kathmandu", ac.Env["TZ"])

	realRun, _ := runTZCleanup(t, s, nil)
	assert.Equal(t, 5, realRun.AgentsAdopted, "a dry run leaves the work for the real run")
}

// TZ cleanup first, then env cleanup: every saved TZ is pinned, and the env
// cleanup leaves the pins alone.
func TestAppliedConfigTZCleanupThenEnvCleanup(t *testing.T) {
	s := createTestStore(t)
	f := newTZCleanupFixture(t, s)

	result, _ := runTZCleanup(t, s, nil)
	assert.Equal(t, 5, result.AgentsAdopted)
	runEnvCleanup(t, s)

	assertLegacyPin(t, s, f.envOnly, "Asia/Kathmandu")
	assertLegacyPin(t, s, f.inlineMatch, "Europe/Paris")
	assertLegacyPin(t, s, f.inlineOnly, "Asia/Tokyo")
	assertLegacyPin(t, s, f.storageMatch, "America/New_York")
	assertLegacyPin(t, s, f.softDeleted, "Australia/Adelaide")
	assert.Equal(t, "UTC", loadAppliedConfig(t, s, f.pinned).ExplicitTimezone)

	again, _ := runTZCleanup(t, s, nil)
	assert.Equal(t, 0, again.AgentsAdopted)
}

// Env cleanup first, then TZ cleanup: a TZ with no live plain source is
// stripped by the env cleanup and not adopted, so that agent follows the
// resolver; a TZ that matches InlineConfig or a storage var is kept by the
// env cleanup and adopted.
func TestAppliedConfigEnvCleanupThenTZCleanup(t *testing.T) {
	s := createTestStore(t)
	f := newTZCleanupFixture(t, s)

	runEnvCleanup(t, s)
	result, _ := runTZCleanup(t, s, nil)
	assert.Equal(t, 3, result.AgentsAdopted, "inlineMatch, inlineOnly and storageMatch are adopted")

	envOnly := loadAppliedConfig(t, s, f.envOnly)
	assert.Empty(t, envOnly.ExplicitTimezone, "a TZ with no live source is not pinned")
	assert.False(t, envOnly.ExplicitTimezoneLegacy)
	assert.False(t, appliedConfigHasEnvTZ(envOnly))
	assert.Equal(t, TZSourceNone, chooseAgentTZ(envOnly, agentTZ{}, "", false).Source, "the agent follows the resolver")

	assertLegacyPin(t, s, f.inlineMatch, "Europe/Paris")
	assertLegacyPin(t, s, f.inlineOnly, "Asia/Tokyo")
	assertLegacyPin(t, s, f.storageMatch, "America/New_York")
	assert.Empty(t, loadAppliedConfig(t, s, f.softDeleted).ExplicitTimezone,
		"the env cleanup also sweeps soft-deleted rows, so their no-source TZ is not pinned")
	assert.Equal(t, "UTC", loadAppliedConfig(t, s, f.pinned).ExplicitTimezone)

	again, _ := runTZCleanup(t, s, nil)
	assert.Equal(t, 0, again.AgentsAdopted)
}

func TestAppliedConfigTZCleanupRegistered(t *testing.T) {
	srv, s := newTestServerWithStore(t)

	op, err := s.GetMaintenanceOperation(context.Background(), "applied-config-tz-cleanup")
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceCategoryMigration, op.Category, "optional migration, run only on request")
	assert.Equal(t, store.MaintenanceStatusPending, op.Status)
	assert.Contains(t, op.Description, "applied-config-env-cleanup", "the description states the order interaction")

	exec, err := srv.resolveMaintenanceExecutor("applied-config-tz-cleanup")
	require.NoError(t, err)
	assert.IsType(t, &AppliedConfigTZCleanupExecutor{}, exec)
}

// TestAppliedConfigTZCleanupRerunsThroughExecuteMigration runs the migration
// twice through the admin endpoint. The second run is accepted (not 409,
// because the key is in rerunnableMigrations), completes, converts 0 and
// writes no agent row.
func TestAppliedConfigTZCleanupRerunsThroughExecuteMigration(t *testing.T) {
	const key = "applied-config-tz-cleanup"
	ctx := context.Background()
	srv, s := newTestServerWithStore(t)
	f := newTZCleanupFixture(t, s)
	require.True(t, rerunnableMigrations[key], "the description promises a safe re-run")

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	run := func() *store.MaintenanceOperation {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/"+key+"/run", strings.NewReader(`{}`))
		req = req.WithContext(contextWithIdentity(req.Context(), admin))
		rr := httptest.NewRecorder()
		srv.handleAdminMaintenanceMigrations(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var got *store.MaintenanceOperation
		require.Eventually(t, func() bool {
			var err error
			got, err = s.GetMaintenanceOperation(ctx, key)
			return err == nil && got.Status != store.MaintenanceStatusRunning
		}, 10*time.Second, 20*time.Millisecond)
		return got
	}

	first := run()
	require.Equal(t, store.MaintenanceStatusCompleted, first.Status, first.Result)
	assert.Contains(t, first.Result, "Adopted 5 agent TZ value(s)")

	versions := map[string]int64{}
	for _, id := range f.all() {
		a, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		versions[id] = a.StateVersion
	}

	second := run()
	require.Equal(t, store.MaintenanceStatusCompleted, second.Status, second.Result)
	assert.Contains(t, second.Result, "Adopted 0 agent TZ value(s) as legacy pins; stripped TZ from 0 agent(s)")
	for id, v := range versions {
		a, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, v, a.StateVersion, "agent %s must not be written by the second run", id)
	}
	assertLegacyPin(t, s, f.envOnly, "Asia/Kathmandu")
}
