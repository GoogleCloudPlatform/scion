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
	srv, s, _, expire := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *listThenExpireStore {
		return &listThenExpireStore{Store: inner, fault: f}
	})
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

	// The deadline passes after the listing and before any agent is read:
	// both co-located agents are incomplete, never clean.
	prev := placementPreflightTimeout
	placementPreflightTimeout = 50 * time.Millisecond
	t.Cleanup(func() { placementPreflightTimeout = prev })
	expire.Arm()
	r = srv.scanColocatedPlacement(ctx, broker.ID, globalDir, policy)
	require.Equal(t, 0, r.Scanned, "%+v", r)
	require.False(t, r.RecordsUnreadable, "%+v", r)
	require.Equal(t, 2, r.Incomplete, "%+v", r)
	require.Equal(t, 2, r.NotCovered, "%+v", r)
	require.True(t, r.NeedsAttention())
}

// listThenExpireStore, once armed, lists agents normally, then waits for
// the caller's deadline before returning them, so the scan's deadline
// falls between the listing and the agents' reads.
type listThenExpireStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *listThenExpireStore) ListAgents(ctx context.Context, f store.AgentFilter, o store.ListOptions) (*store.ListResult[store.Agent], error) {
	res, err := s.Store.ListAgents(ctx, f, o)
	if s.fault.Active() {
		<-ctx.Done()
	}
	return res, err
}

// The report's policy uses the runtime entries the co-located broker
// treats as the operator's: a profiled Kubernetes agent of the co-located
// broker, on an operator entry with placement, is categorized through the
// GET's report even though the bootstrap merge also carries the built-in
// kubernetes entry (which, counted as the operator's, would make it
// unresolvable).
func TestPlacementPreflightReportUsesConfiguredEntries(t *testing.T) {
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(nil)
	path := tempSettingsHome(t)
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nruntimes:\n  k8s:\n    type: kubernetes\n    node_selector:\n      pool: agents\n"), 0o644))
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "pp-cfg-broker")
	linked := filepath.Join(t.TempDir(), "linked", config.DotScion)
	require.NoError(t, os.MkdirAll(linked, 0o755))
	project := &store.Project{ID: tid("pp-cfg-project"), Name: "pp-cfg", Slug: "pp-cfg"}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: linked, Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: tid("pp-cfg-agent"), Slug: "gpu-agent", Name: "gpu-agent", ProjectID: project.ID, RuntimeBrokerID: broker.ID}))
	writePlacementPreflightAgent(t, linked, "gpu-agent", `{"runtime":"kubernetes","profile":"gpu"}`, `{"kubernetes":{"nodeSelector":{"pool":"other"}}}`)

	fileK := newFileKoanf(t, map[string]interface{}{
		"runtimes.k8s.type":                "kubernetes",
		"runtimes.k8s.node_selector.pool":  "agents",
		"runtimes.kubernetes.type":         "kubernetes",
		"profiles.gpu.runtime":             "k8s",
		"profiles.gpu.node_selector.accel": "l4",
	})
	ops := NewOperationalSettings(newFakeHubSettingStore(), fileK, emptyKoanf())
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)
	publishTarget(srv, nil, broker.ID, t.TempDir())

	view := srv.placementPreflight(ctx, ops)
	require.Equal(t, 1, view.AgentsScanned, "%+v", view)
	require.Equal(t, 0, view.AgentsIncomplete, "%+v", view)
	require.Equal(t, 1, view.AgentCounts[string(config.PreflightAgentSelectorConflictsOperator)], "%+v", view)
}
