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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envBearingAppliedConfig returns an applied config with every env-style map
// populated: the top-level Env, InlineConfig.Env, CreateInputs.InlineConfig.Env
// and the telemetry headers on both inline configs. tag keeps two configs
// distinguishable when a test stores both.
func envBearingAppliedConfig(tag string) *store.AgentAppliedConfig {
	inline := func(prefix string) *api.ScionConfig {
		return &api.ScionConfig{
			Env: map[string]string{prefix + "_INLINE_VAR": tag + "-inline"},
			Telemetry: &api.TelemetryConfig{
				Cloud: &api.TelemetryCloudConfig{
					Endpoint: "https://otel.example.com",
					Headers:  map[string]string{"x-" + prefix + "-header": tag + "-header"},
				},
			},
		}
	}
	return &store.AgentAppliedConfig{
		Image:        "img:" + tag,
		Env:          map[string]string{"TOP_VAR": tag + "-top", "OTHER_VAR": tag + "-other"},
		InlineConfig: inline("APPLIED"),
		CreateInputs: &store.AgentCreateInputs{
			InlineConfig: inline("CREATE"),
			Workspace:    "/tmp/" + tag,
		},
	}
}

// TestAgentReincarnationStore_SnapshotsRoundTripEnvMaps pins that both
// snapshot columns round-trip every env-style map exactly, through each
// writer (create, update, compare-and-advance). A failed reincarnation
// restores the agent row from PreviousAppliedConfig, so anything the snapshot
// drops is lost from the agent on rollback.
func TestAgentReincarnationStore_SnapshotsRoundTripEnvMaps(t *testing.T) {
	ctx := context.Background()
	s := newTestAgentReincarnationStore(t)

	prev := envBearingAppliedConfig("previous")
	next := envBearingAppliedConfig("new")

	rec := &store.AgentReincarnation{
		AgentID:               "agent-env-roundtrip",
		FromGeneration:        1,
		ToGeneration:          2,
		State:                 store.AgentReincarnationStatePending,
		PreviousAppliedConfig: prev,
		NewAppliedConfig:      next,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, rec))

	assertSnapshots := func(step string) {
		t.Helper()
		got, err := s.GetAgentReincarnation(ctx, rec.ID)
		require.NoError(t, err)
		assert.Equal(t, prev, got.PreviousAppliedConfig, "%s: previous snapshot must round-trip exactly", step)
		assert.Equal(t, next, got.NewAppliedConfig, "%s: new snapshot must round-trip exactly", step)
	}
	assertSnapshots("create")

	// Update rewrites both columns from the in-memory record.
	next = envBearingAppliedConfig("updated")
	rec.NewAppliedConfig = next
	rec.State = store.AgentReincarnationStateStopping
	require.NoError(t, s.UpdateAgentReincarnation(ctx, rec))
	assertSnapshots("update")

	// TryAdvance is the worker's per-step write path.
	next = envBearingAppliedConfig("advanced")
	rec.NewAppliedConfig = next
	rec.State = store.AgentReincarnationStateProvisioning
	rec.UpdatedAt = time.Now()
	ok, err := s.TryAdvanceAgentReincarnation(ctx, rec, string(store.AgentReincarnationStateStopping), time.Time{})
	require.NoError(t, err)
	require.True(t, ok)
	assertSnapshots("advance")
}

// TestAgentStore_AppliedConfigRoundTripsEnvMaps pins the agents-row side of
// the same contract, which the reincarnation snapshots share a serializer
// with.
func TestAgentStore_AppliedConfigRoundTripsEnvMaps(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "env-roundtrip")
	a.AppliedConfig = envBearingAppliedConfig("created")
	require.NoError(t, s.CreateAgent(ctx, a))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, envBearingAppliedConfig("created"), got.AppliedConfig)

	got.AppliedConfig = envBearingAppliedConfig("updated")
	require.NoError(t, s.UpdateAgent(ctx, got))

	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, envBearingAppliedConfig("updated"), got.AppliedConfig)
}
