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
	"encoding/json"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pending_config column round-trips on the store under test: SQLite by
// default, Postgres when run with -tags integration and
// SCION_TEST_POSTGRES_URL (newTestCompositeStore uses enttest.NewClient).

func seedPendingConfigAgent(t *testing.T, ctx context.Context, cs *CompositeStore) *store.Agent {
	t.Helper()
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "pending-" + projectID[:8], Slug: "pending-" + projectID[:8]}))
	a := makeAgent(projectID, "pending-"+projectID[:8])
	a.AppliedConfig = &store.AgentAppliedConfig{
		Model:        "model-a",
		Env:          map[string]string{"KEEP": "1"},
		InlineConfig: &api.ScionConfig{Model: "model-a"},
	}
	require.NoError(t, cs.CreateAgent(ctx, a))
	return a
}

func samplePendingConfig() *store.AgentPendingConfig {
	return &store.AgentPendingConfig{
		Config: map[string]json.RawMessage{
			"model":          json.RawMessage(`"model-b"`),
			"thinking_level": json.RawMessage(`null`),
			"env":            json.RawMessage(`{"SECRET":"v"}`),
		},
		SavedBy:          "user:u1",
		SavedByName:      "User One",
		SavedAt:          time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		BaseGeneration:   2,
		BaseStateVersion: 9,
		KeySavers:        map[string]string{"model": "user:u1", "env": "user:u2"},
	}
}

func TestAgentStore_PendingConfig_RoundTripsThroughUpdateAgent(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	a := seedPendingConfigAgent(t, ctx, cs)

	got, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Nil(t, got.PendingConfig, "a new agent has no pending config (NULL column)")

	got.PendingConfig = samplePendingConfig()
	require.NoError(t, cs.UpdateAgent(ctx, got))

	reread, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, reread.PendingConfig)
	want := samplePendingConfig()
	assert.Equal(t, want.SavedBy, reread.PendingConfig.SavedBy)
	assert.Equal(t, want.SavedByName, reread.PendingConfig.SavedByName)
	assert.True(t, want.SavedAt.Equal(reread.PendingConfig.SavedAt))
	assert.Equal(t, want.BaseGeneration, reread.PendingConfig.BaseGeneration)
	assert.Equal(t, want.BaseStateVersion, reread.PendingConfig.BaseStateVersion)
	assert.Equal(t, want.KeySavers, reread.PendingConfig.KeySavers, "persistence keeps the per-key savers")
	require.Len(t, reread.PendingConfig.Config, 3)
	assert.JSONEq(t, `"model-b"`, string(reread.PendingConfig.Config["model"]))
	assert.Equal(t, "null", string(reread.PendingConfig.Config["thinking_level"]), "a stored null (clear at apply) survives")
	assert.JSONEq(t, `{"SECRET":"v"}`, string(reread.PendingConfig.Config["env"]), "env values are persisted unredacted")

	// A whole-row write of a copy without pending edits clears the column.
	reread.PendingConfig = nil
	require.NoError(t, cs.UpdateAgent(ctx, reread))
	cleared, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Nil(t, cleared.PendingConfig)
}

func TestAgentStore_UpdateAgentConfig(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	a := seedPendingConfigAgent(t, ctx, cs)

	cur, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	// Status columns written separately (as a start's starting write is)
	// must survive the config write.
	require.NoError(t, cs.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting", Message: "starting up"}))

	applied := *cur.AppliedConfig
	applied.Model = "model-b"
	applied.Env = map[string]string{"KEEP": "1", "NEW": "2"}
	version, err := cs.UpdateAgentConfig(ctx, a.ID, cur.StateVersion, &applied, samplePendingConfig())
	require.NoError(t, err)
	assert.Equal(t, cur.StateVersion+1, version)

	got, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, version, got.StateVersion)
	assert.Equal(t, "starting", got.Phase, "the phase written by the status write is kept")
	assert.Equal(t, "starting up", got.Message)
	assert.Equal(t, "model-b", got.AppliedConfig.Model)
	assert.Equal(t, map[string]string{"KEEP": "1", "NEW": "2"}, got.AppliedConfig.Env)
	require.NotNil(t, got.PendingConfig)
	assert.Len(t, got.PendingConfig.Config, 3)

	// A stale version is a conflict and writes nothing.
	_, err = cs.UpdateAgentConfig(ctx, a.ID, cur.StateVersion, &applied, nil)
	assert.ErrorIs(t, err, store.ErrVersionConflict)
	still, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.NotNil(t, still.PendingConfig)

	// nil pending clears the column.
	_, err = cs.UpdateAgentConfig(ctx, a.ID, version, got.AppliedConfig, nil)
	require.NoError(t, err)
	final, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Nil(t, final.PendingConfig)
	assert.Equal(t, "model-b", final.AppliedConfig.Model)

	// An unknown agent is not found.
	_, err = cs.UpdateAgentConfig(ctx, uuid.NewString(), 1, got.AppliedConfig, nil)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAgentStore_UpdateAgentConfig_SoftDeletedIsNotFound(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	a := seedPendingConfigAgent(t, ctx, cs)
	cur, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	cur.DeletedAt = time.Now()
	require.NoError(t, cs.UpdateAgent(ctx, cur))

	_, err = cs.UpdateAgentConfig(ctx, a.ID, cur.StateVersion, cur.AppliedConfig, samplePendingConfig())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAgentStore_CreateAgentWithPendingConfig(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "pc-" + projectID[:8], Slug: "pc-" + projectID[:8]}))
	a := makeAgent(projectID, "pc-"+projectID[:8])
	a.PendingConfig = samplePendingConfig()
	require.NoError(t, cs.CreateAgent(ctx, a))
	got, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.PendingConfig)
	assert.JSONEq(t, `"model-b"`, string(got.PendingConfig.Config["model"]))
}
