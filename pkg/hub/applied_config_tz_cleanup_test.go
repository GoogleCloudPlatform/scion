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
	"testing"

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
	}
	for id, ac := range configs {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID:            id,
			Slug:          id,
			Name:          id,
			ProjectID:     f.project.ID,
			AppliedConfig: ac,
		}))
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
	assert.Equal(t, 7, result.AgentsScanned)
	assert.Equal(t, 4, result.AgentsAdopted, "envOnly, inlineMatch, inlineOnly and storageMatch are adopted")
	assert.Equal(t, 2, result.AgentsStripped, "pinned and emptyMarker are only stripped")
	assert.Contains(t, log, "Adopted 4 agent TZ value(s)")
	assert.Contains(t, log, "ADOPT agent="+f.envOnly+" source=legacy")
	assert.NotContains(t, log, "Asia/Kathmandu", "TZ values are never logged")

	assertLegacyPin(t, s, f.envOnly, "Asia/Kathmandu")
	assertLegacyPin(t, s, f.inlineMatch, "Europe/Paris")
	assertLegacyPin(t, s, f.inlineOnly, "Asia/Tokyo")
	assertLegacyPin(t, s, f.storageMatch, "America/New_York")
	assert.Equal(t, "bar", loadAppliedConfig(t, s, f.envOnly).Env["FOO"], "other env keys are untouched")

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
	require.Equal(t, 4, first.AgentsAdopted)

	versions := map[string]int64{}
	for _, id := range []string{f.envOnly, f.inlineMatch, f.inlineOnly, f.storageMatch, f.pinned, f.emptyMarker, f.clean} {
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
	assert.Equal(t, 4, result.AgentsAdopted)
	assert.Equal(t, 2, result.AgentsStripped)
	assert.Contains(t, log, "DRY RUN")
	assert.Contains(t, log, "Would adopt 4")

	ac := loadAppliedConfig(t, s, f.envOnly)
	assert.Empty(t, ac.ExplicitTimezone)
	assert.Equal(t, "Asia/Kathmandu", ac.Env["TZ"])

	realRun, _ := runTZCleanup(t, s, nil)
	assert.Equal(t, 4, realRun.AgentsAdopted, "a dry run leaves the work for the real run")
}

// TZ cleanup first, then env cleanup: every saved TZ is pinned, and the env
// cleanup leaves the pins alone.
func TestAppliedConfigTZCleanupThenEnvCleanup(t *testing.T) {
	s := createTestStore(t)
	f := newTZCleanupFixture(t, s)

	result, _ := runTZCleanup(t, s, nil)
	assert.Equal(t, 4, result.AgentsAdopted)
	runEnvCleanup(t, s)

	assertLegacyPin(t, s, f.envOnly, "Asia/Kathmandu")
	assertLegacyPin(t, s, f.inlineMatch, "Europe/Paris")
	assertLegacyPin(t, s, f.inlineOnly, "Asia/Tokyo")
	assertLegacyPin(t, s, f.storageMatch, "America/New_York")
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
