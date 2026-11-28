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
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pruneStubIntegrationManager struct {
	IntegrationManager
	loaded []string
}

func (m *pruneStubIntegrationManager) ListPlugins() []string {
	keys := make([]string, 0, len(m.loaded))
	for _, name := range m.loaded {
		keys = append(keys, plugin.PluginTypeBroker+":"+name)
	}
	return keys
}

func (m *pruneStubIntegrationManager) HasPlugin(pluginType, name string) bool {
	if pluginType != plugin.PluginTypeBroker {
		return false
	}
	for _, n := range m.loaded {
		if n == name {
			return true
		}
	}
	return false
}

func (m *pruneStubIntegrationManager) GetDeploymentMode(_, _ string) plugin.DeploymentMode {
	return plugin.DeploymentModePlugin
}

func (m *pruneStubIntegrationManager) IsSelfManaged(_, _ string) bool {
	return false
}

type pruneFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *pruneFaultStore) ListHubInstances(ctx context.Context, recentWindow time.Duration) ([]store.HubInstance, time.Time, error) {
	if !f.fault.Active() {
		return f.Store.ListHubInstances(ctx, recentWindow)
	}
	return nil, time.Time{}, errors.New("db unavailable")
}

func createPrunePluginBrokerRecord(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid("plugin-prune-" + name)
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID:              id,
		Name:            "plugin-" + name,
		Slug:            "plugin-" + name,
		Status:          store.BrokerStatusOnline,
		ConnectionState: "embedded",
		Labels:          map[string]string{pluginBrokerLabel: name},
		Created:         time.Now(),
		Updated:         time.Now(),
	}))
	return id
}

func TestPruneStalePluginRecords_PrunesUnrunUnconfiguredWithoutBinary(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	pluginsDir := t.TempDir()
	brokerDir := filepath.Join(pluginsDir, plugin.PluginTypeBroker)
	require.NoError(t, os.MkdirAll(brokerDir, 0755))

	// 1. Stale backup integration record: even if the .bak file still sits on disk,
	// its name is invalid so it is not backed by a valid binary and must be pruned.
	backupName := "discord.bak.20261128"
	backupID := createPrunePluginBrokerRecord(t, s, backupName)
	require.NoError(t, os.WriteFile(
		filepath.Join(brokerDir, plugin.PluginBinaryPrefix+backupName),
		[]byte("#!/bin/sh\n"), 0755,
	))

	// 2. Stale valid-named integration record whose binary was deleted.
	removedID := createPrunePluginBrokerRecord(t, s, "removed-plugin")

	// 3. Integration record backed by a valid executable binary on disk -> must survive.
	discordID := createPrunePluginBrokerRecord(t, s, "discord")
	require.NoError(t, os.WriteFile(
		filepath.Join(brokerDir, plugin.PluginBinaryPrefix+"discord"),
		[]byte("#!/bin/sh\n"), 0755,
	))

	// 4. Configured (e.g. self-managed/HA) integration with no local binary -> must survive.
	gchatID := createPrunePluginBrokerRecord(t, s, "googlechat")

	// 5. Integration loaded in this server's plugin manager -> must survive.
	telegramID := createPrunePluginBrokerRecord(t, s, "telegram")
	srv.SetPluginManager(&pruneStubIntegrationManager{loaded: []string{"telegram"}})

	// 6. Integration reported by another live hub instance -> must survive.
	slackID := createPrunePluginBrokerRecord(t, s, "slack")
	peerStats, err := json.Marshal(api.HubInstanceStats{
		Integrations: []api.HubInstanceIntegration{
			{Name: "slack", Health: HealthStatusHealthy, Connected: true},
		},
	})
	require.NoError(t, err)
	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID:     "peer-hub-1",
		Label:  "peer-hub-1",
		Status: HealthStatusHealthy,
		Stats:  peerStats,
	}))

	// 7. Regular non-plugin runtime broker -> must never be touched.
	regularID := tid("prune-regular-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     regularID,
		Name:   "regular-broker",
		Slug:   "regular-broker",
		Status: store.BrokerStatusOffline,
	}))

	pruned, err := srv.PruneStalePluginRecords(ctx, map[string]bool{"googlechat": true}, pluginsDir)
	require.NoError(t, err)
	assert.Equal(t, []string{backupName, "removed-plugin"}, pruned)

	// Verify pruned rows are gone and surviving rows remain.
	_, err = s.GetRuntimeBroker(ctx, backupID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetRuntimeBroker(ctx, removedID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	for _, id := range []string{discordID, gchatID, telegramID, slackID, regularID} {
		_, getErr := s.GetRuntimeBroker(ctx, id)
		assert.NoError(t, getErr, "expected broker %s to remain", id)
	}

	// Verify health summary (doctor's data source) no longer lists the pruned records.
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	var names []string
	for _, it := range resp.Integrations {
		names = append(names, it.Name)
	}
	assert.NotContains(t, names, backupName)
	assert.NotContains(t, names, "removed-plugin")
	assert.Contains(t, names, "discord")
	assert.Contains(t, names, "googlechat")
	assert.Contains(t, names, "telegram")
	assert.Contains(t, names, "slack")
}

func TestPruneStalePluginRecords_NonExecutableBinaryStillPruned(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	pluginsDir := t.TempDir()
	brokerDir := filepath.Join(pluginsDir, plugin.PluginTypeBroker)
	require.NoError(t, os.MkdirAll(brokerDir, 0755))

	createPrunePluginBrokerRecord(t, s, "noexec")
	require.NoError(t, os.WriteFile(
		filepath.Join(brokerDir, plugin.PluginBinaryPrefix+"noexec"),
		[]byte("not executable"), 0644,
	))

	pruned, err := srv.PruneStalePluginRecords(ctx, nil, pluginsDir)
	require.NoError(t, err)
	assert.Equal(t, []string{"noexec"}, pruned)
}

func TestPruneStalePluginRecords_StaleHubInstanceReportDoesNotBlockPrune(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	pluginsDir := t.TempDir()

	createPrunePluginBrokerRecord(t, s, "orphaned")
	// A stopped/stale hub instance that reported "orphaned" in the past must not prevent pruning.
	staleStats, err := json.Marshal(api.HubInstanceStats{
		Integrations: []api.HubInstanceIntegration{
			{Name: "orphaned", Health: HealthStatusHealthy, Connected: true},
		},
	})
	require.NoError(t, err)
	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID:     "old-hub",
		Label:  "old-hub",
		Status: HealthStatusHealthy,
		Stats:  staleStats,
	}))
	require.NoError(t, s.MarkHubInstanceStopped(ctx, "old-hub"))

	pruned, err := srv.PruneStalePluginRecords(ctx, nil, pluginsDir)
	require.NoError(t, err)
	assert.Equal(t, []string{"orphaned"}, pruned)
}

func TestPruneStalePluginRecords_FailSafeOnHubInstanceListError(t *testing.T) {
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *pruneFaultStore {
		return &pruneFaultStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	id := createPrunePluginBrokerRecord(t, s, "discord.bak.123")

	fault.Arm()
	pruned, err := srv.PruneStalePluginRecords(ctx, nil, t.TempDir())
	require.Error(t, err)
	assert.Empty(t, pruned)

	// Record must remain intact when registry cannot be read.
	_, getErr := s.GetRuntimeBroker(ctx, id)
	assert.NoError(t, getErr)
}
