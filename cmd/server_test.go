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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	dbName := strings.ReplaceAll(t.Name(), "/", "_")
	client, err := entc.OpenSQLite("file:"+dbName+"?mode=memory&cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	s := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRegisterGlobalGroveAndBroker_DedupByName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// First registration: creates broker with ID tid("broker-1") and name "test-broker"
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Verify broker was created
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "test-broker", broker.Name)
	assert.Equal(t, store.BrokerStatusOnline, broker.Status)

	// Second registration with a DIFFERENT ID but SAME name.
	// This simulates a restart where the broker ID was lost/regenerated.
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-2"), "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)

	// Should return the original broker-1 ID (dedup by name)
	assert.Equal(t, tid("broker-1"), effectiveID, "should reuse existing broker ID found by name")

	// Verify no duplicate was created
	_, err = s.GetRuntimeBroker(ctx, tid("broker-2"))
	assert.ErrorIs(t, err, store.ErrNotFound, "broker-2 should NOT exist in the database")

	// Verify original broker was updated
	broker, err = s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "test-broker", broker.Name)
	assert.Equal(t, store.BrokerStatusOnline, broker.Status)
}

func TestRegisterSystemProject_DisabledNoop(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	workspace := filepath.Join(t.TempDir(), "system-project")

	require.NoError(t, registerSystemProject(ctx, s, tid("broker-1"), "test-broker", config.SystemProjectConfig{
		Enabled:       false,
		WorkspacePath: workspace,
	}))

	_, err := s.GetProjectBySlug(ctx, SystemProjectName)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = os.Stat(workspace)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRegisterSystemProject_CreatesProjectWorkspaceAndPolicy(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	workspace := filepath.Join(t.TempDir(), "system-project")

	require.NoError(t, registerSystemProject(ctx, s, tid("broker-1"), "test-broker", config.SystemProjectConfig{
		Enabled:       true,
		WorkspacePath: workspace,
	}))

	project, err := s.GetProjectBySlug(ctx, SystemProjectName)
	require.NoError(t, err)
	assert.Equal(t, "System", project.Name)
	assert.Equal(t, store.VisibilityPrivate, project.Visibility)
	assert.Equal(t, "true", project.Labels[projectcompat.LabelScionSystem])
	assert.Equal(t, "true", project.Labels[projectcompat.LabelSystemProject])
	assert.Equal(t, tid("broker-1"), project.DefaultRuntimeBrokerID)
	require.Len(t, project.SharedDirs, 1)
	assert.Equal(t, "shared", project.SharedDirs[0].Name)

	for _, rel := range []string{"shared", "shared/notes", "shared/runbooks", "agents", "config"} {
		info, err := os.Stat(filepath.Join(workspace, rel))
		require.NoError(t, err)
		assert.True(t, info.IsDir(), rel)
	}
	journal, err := os.ReadFile(filepath.Join(workspace, "shared", "journal.md"))
	require.NoError(t, err)
	assert.Contains(t, string(journal), "System Project Journal")

	provider, err := s.GetProjectProvider(ctx, project.ID, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, workspace, provider.LocalPath)
	assert.Equal(t, store.BrokerStatusOnline, provider.Status)

	group, err := s.GetGroupBySlug(ctx, "project:"+SystemProjectName+":members")
	require.NoError(t, err)
	assert.Equal(t, project.ID, group.ProjectID)

	policies, err := s.ListPolicies(ctx, store.PolicyFilter{Name: "project:" + SystemProjectName + ":member-create-agents"}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, policies.Items, 1)
	assert.Equal(t, project.ID, policies.Items[0].ScopeID)
	assert.ElementsMatch(t, []string{"create", "stop_all"}, policies.Items[0].Actions)
	bindings, err := s.GetPolicyBindings(ctx, policies.Items[0].ID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	assert.Equal(t, group.ID, bindings[0].PrincipalID)
}

func TestRegisterSystemProject_IdempotentPreservesJournal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	workspace := filepath.Join(t.TempDir(), "system-project")

	require.NoError(t, registerSystemProject(ctx, s, tid("broker-1"), "test-broker", config.SystemProjectConfig{
		Enabled:       true,
		WorkspacePath: workspace,
	}))
	journalPath := filepath.Join(workspace, "shared", "journal.md")
	require.NoError(t, os.WriteFile(journalPath, []byte("keep me\n"), 0644))

	require.NoError(t, registerSystemProject(ctx, s, tid("broker-1"), "test-broker", config.SystemProjectConfig{
		Enabled:       true,
		WorkspacePath: workspace,
	}))

	journal, err := os.ReadFile(journalPath)
	require.NoError(t, err)
	assert.Equal(t, "keep me\n", string(journal))

	result, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{})
	require.NoError(t, err)
	count := 0
	for _, project := range result.Items {
		if project.Slug == SystemProjectName {
			count++
		}
	}
	assert.Equal(t, 1, count)

	project, err := s.GetProjectBySlug(ctx, SystemProjectName)
	require.NoError(t, err)
	groups, err := s.ListGroups(ctx, store.GroupFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, groups.Items, 1)
}

func TestRegisterGlobalGroveAndBroker_SameIDNoDedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// First registration
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Second registration with the same ID (normal restart case)
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, false, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Verify broker was updated (not duplicated)
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "test-broker", broker.Name)
	assert.Equal(t, false, broker.AutoProvide, "auto-provide should be updated to false")
}

func TestRegisterGlobalGroveAndBroker_NewBrokerNewName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// Register first broker
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "broker-alpha", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Register a genuinely different broker (different ID AND different name)
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-2"), "broker-beta", "http://localhost:9801", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-2"), effectiveID)

	// Both brokers should exist
	_, err = s.GetRuntimeBroker(ctx, tid("broker-1"))
	assert.NoError(t, err)
	_, err = s.GetRuntimeBroker(ctx, tid("broker-2"))
	assert.NoError(t, err)
}

func TestRegisterGlobalGroveAndBroker_DedupCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// Register broker with lowercase name
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "scion-demo", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Register with different ID and mixed-case name
	// GetRuntimeBrokerByName uses LOWER() for case-insensitive match
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-2"), "Scion-Demo", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID, "should match case-insensitively")
}
