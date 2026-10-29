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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// writePlacementPreflightAgent writes agent slug's agent-info.json and,
// when cfg is not empty, its scion-agent.json in the broker project path
// projectPath.
func writePlacementPreflightAgent(t *testing.T, projectPath, slug, info, cfg string) {
	t.Helper()
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	require.NoError(t, err)
	home := config.GetAgentHomePath(projectDir, slug)
	agentDir := config.GetAgentDir(projectDir, slug, false)
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(info), 0o644))
	if cfg != "" {
		require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(cfg), 0o644))
	}
}

// The co-located broker's agents are read from the hub's rows and this
// host's files: a categorized agent counts, an agent whose configuration
// cannot be read is incomplete, and agents of another broker or not yet
// placed are not covered. None of them is reported as clean.
func TestPlacementPreflightUnreadableAgentIsIncomplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, s := testServer(t)
	ctx := context.Background()
	globalDir := t.TempDir()
	broker := newLocalPathTestBroker(t, s, "pp-broker")
	other := newLocalPathTestBroker(t, s, "pp-other")
	linked := filepath.Join(t.TempDir(), "linked", config.DotScion)
	require.NoError(t, os.MkdirAll(linked, 0o755))
	project := &store.Project{ID: tid("pp-project"), Name: "pp-linked", Slug: "pp-linked"}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: linked, Status: store.BrokerStatusOnline,
	}))
	for _, a := range []*store.Agent{
		{ID: tid("pp-tol"), Slug: "tol", Name: "tol", ProjectID: project.ID, RuntimeBrokerID: broker.ID},
		{ID: tid("pp-broken"), Slug: "broken", Name: "broken", ProjectID: project.ID, RuntimeBrokerID: broker.ID},
		{ID: tid("pp-other"), Slug: "elsewhere", Name: "elsewhere", ProjectID: project.ID, RuntimeBrokerID: other.ID},
		{ID: tid("pp-unplaced"), Slug: "unplaced", Name: "unplaced", ProjectID: project.ID},
	} {
		require.NoError(t, s.CreateAgent(ctx, a))
	}
	writePlacementPreflightAgent(t, linked, "tol", `{"runtime":"kubernetes"}`, `{"kubernetes":{"tolerations":[{"key":"x","operator":"Exists"}]}}`)
	writePlacementPreflightAgent(t, linked, "broken", `{"runtime":"kubernetes"}`, `{"kubernetes":`)

	policy := func(profile string) (*api.ResolvedPlacement, bool) {
		return config.ResolveLegacyPolicyOffline(map[string]config.V1RuntimeConfig{"k": {Type: "kubernetes"}}, nil, nil, profile)
	}
	r := srv.scanColocatedPlacement(ctx, broker.ID, globalDir, policy)
	require.Equal(t, 1, r.Scanned, "%+v", r)
	require.Equal(t, 1, r.Incomplete, "%+v", r)
	require.Equal(t, 2, r.NotCovered, "%+v", r)
	require.False(t, r.RecordsUnreadable)
	require.Equal(t, 1, r.Counts[config.PreflightAgentTolerationsWithoutOperatorPolicy])
	require.True(t, r.NeedsAttention())

	// Past the report's deadline nothing is read and nothing is clean.
	prev := placementPreflightTimeout
	placementPreflightTimeout = time.Nanosecond
	t.Cleanup(func() { placementPreflightTimeout = prev })
	r = srv.scanColocatedPlacement(ctx, broker.ID, globalDir, policy)
	require.Equal(t, 0, r.Scanned, "%+v", r)
	require.True(t, r.RecordsUnreadable || r.Incomplete == 2, "%+v", r)
	require.True(t, r.NeedsAttention())
}
