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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
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

func TestRegisterGlobalProjectAndBroker_SetsEmbeddedLabel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// Register a new co-located broker
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Verify the broker has the embedded label
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "embedded", broker.Labels["scion.io/broker-role"])
}

func TestRegisterGlobalProjectAndBroker_LabelsOnReregistration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// First registration
	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)

	// Manually add a user-set label to simulate prior customization
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	broker.Labels["custom-label"] = "custom-value"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	// Re-register (same ID, same name)
	_, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)

	// Verify the embedded label is set AND the custom label is preserved
	broker, err = s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "embedded", broker.Labels["scion.io/broker-role"])
	assert.Equal(t, "custom-value", broker.Labels["custom-label"])
}

func TestRegisterGlobalProjectAndBroker_LabelsOnDedupByName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// First registration
	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)

	// Second registration with different ID but same name (dedup path)
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-2"), "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Verify the embedded label survives the dedup re-registration
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "embedded", broker.Labels["scion.io/broker-role"])
}
func TestBuildStoreBrokerProfiles_CloudRunFiltersLocalRuntimes(t *testing.T) {
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}

	profiles := buildStoreBrokerProfiles(settings, "cloudrun")

	assert.Len(t, profiles, 1, "Cloud Run should filter out docker profile")
	assert.Equal(t, "kubernetes", profiles[0].Type)
	assert.Equal(t, "remote", profiles[0].Name)
}

func TestBuildStoreBrokerProfiles_DockerDefaultKeepsAllProfiles(t *testing.T) {
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}

	profiles := buildStoreBrokerProfiles(settings, "docker")

	assert.Len(t, profiles, 2, "docker default should keep all profiles")
	types := map[string]bool{}
	for _, p := range profiles {
		types[p.Type] = true
	}
	assert.True(t, types["docker"])
	assert.True(t, types["kubernetes"])
}

func TestBuildStoreBrokerProfiles_EmptyAfterFilterFallsBackToDefault(t *testing.T) {
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	profiles := buildStoreBrokerProfiles(settings, "cloudrun")

	assert.Len(t, profiles, 1, "should fall back to default profile when all are filtered")
	assert.Equal(t, "default", profiles[0].Name)
	assert.Equal(t, "cloudrun", profiles[0].Type)
	assert.True(t, profiles[0].Available)
}

func TestBuildStoreBrokerProfiles_StarterHubKeepsDockerProfiles(t *testing.T) {
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}

	profiles := buildStoreBrokerProfiles(settings, "docker")

	assert.Len(t, profiles, 2, "starter hub (docker default) should keep docker profiles")
	types := map[string]bool{}
	for _, p := range profiles {
		types[p.Type] = true
	}
	assert.True(t, types["docker"], "docker profile should be present")
	assert.True(t, types["kubernetes"], "kubernetes profile should be present")
}

func TestRegisterGlobalGroveAndBroker_CloudRunSuppressesDockerProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}

	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "hosted-broker", "http://localhost:9800", rt, true, settings)
	require.NoError(t, err)

	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)

	for _, p := range broker.Profiles {
		assert.NotEqual(t, "docker", p.Type, "docker profile should not appear in Cloud Run mode")
	}
	assert.Len(t, broker.Profiles, 1)
	assert.Equal(t, "kubernetes", broker.Profiles[0].Type)
}

func TestRegisterGlobalGroveAndBroker_StarterHubKeepsDockerProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "starter-hub", "http://localhost:9800", rt, true, settings)
	require.NoError(t, err)

	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)

	assert.Len(t, broker.Profiles, 2, "starter hub should keep all profiles including docker")
	types := map[string]bool{}
	for _, p := range broker.Profiles {
		types[p.Type] = true
	}
	assert.True(t, types["docker"], "docker profile should be present on starter hub")
	assert.True(t, types["kubernetes"], "kubernetes profile should be present")
}
