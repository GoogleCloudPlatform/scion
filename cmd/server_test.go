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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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

func TestRegisterGlobalProjectAndBroker_DedupByName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// First registration: creates broker with ID tid("broker-1") and name "test-broker"
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Verify broker was created
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "test-broker", broker.Name)
	assert.Equal(t, store.BrokerStatusOnline, broker.Status)

	// Second registration with a DIFFERENT ID but SAME name.
	// This simulates a restart where the broker ID was lost/regenerated.
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-2"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
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

func TestRegisterGlobalProjectAndBroker_SameIDNoDedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// First registration
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Second registration with the same ID (normal restart case)
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, false, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Verify broker was updated (not duplicated)
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "test-broker", broker.Name)
	assert.Equal(t, false, broker.AutoProvide, "auto-provide should be updated to false")
}

func TestRegisterGlobalProjectAndBroker_NewBrokerNewName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// Register first broker
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "broker-alpha", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Register a genuinely different broker (different ID AND different name)
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-2"), "broker-beta", "http://localhost:9801", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-2"), effectiveID)

	// Both brokers should exist
	_, err = s.GetRuntimeBroker(ctx, tid("broker-1"))
	assert.NoError(t, err)
	_, err = s.GetRuntimeBroker(ctx, tid("broker-2"))
	assert.NoError(t, err)
}

func TestRegisterGlobalProjectAndBroker_DedupCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// Register broker with lowercase name
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "scion-demo", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Register with different ID and mixed-case name
	// GetRuntimeBrokerByName uses LOWER() for case-insensitive match
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-2"), "Scion-Demo", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID, "should match case-insensitively")
}

func TestRegisterGlobalProjectAndBroker_SetsEmbeddedLabel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// Register a new co-located broker
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
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
	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)

	// Manually add a user-set label to simulate prior customization
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	broker.Labels["custom-label"] = "custom-value"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	// Re-register (same ID, same name)
	_, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
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
	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)

	// Second registration with different ID but same name (dedup path)
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-2"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, tid("broker-1"), effectiveID)

	// Verify the embedded label survives the dedup re-registration
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "embedded", broker.Labels["scion.io/broker-role"])
}

// TestRegisterGlobalProjectAndBroker_RecordsDefaultProfile covers the hub's
// side of resolving an agent dispatch with no explicit profile: the broker
// record must carry the same active profile the broker itself would use
// locally, so it survives create, re-registration (when settings change),
// and the stock two-profile default settings shape.
func TestRegisterGlobalProjectAndBroker_RecordsDefaultProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{
		ActiveProfile: "local",
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}

	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)

	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "local", broker.DefaultProfile,
		"the broker record must carry the same active profile the broker resolves an empty profile to locally")

	// Re-registration with a different active profile (e.g. settings.yaml
	// edited and the broker restarted) must update the stored value, not
	// leave the stale one in place.
	settings.ActiveProfile = "remote"
	_, err = registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)

	broker, err = s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Equal(t, "remote", broker.DefaultProfile)
}

// TestRegisterGlobalProjectAndBroker_NoActiveProfileLeavesDefaultProfileEmpty
// covers the case buildStoreBrokerProfiles itself falls back on: settings
// with no profiles defined at all produce a single implicit "default"
// profile, and there is no active-profile setting to record. An empty
// DefaultProfile is correct here — the hub resolves this broker's single
// profile without needing it (resolveAgentRuntimeProfileType,
// pkg/hub/default_gcp_identity.go).
func TestRegisterGlobalProjectAndBroker_NoActiveProfileLeavesDefaultProfileEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)

	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)
	assert.Empty(t, broker.DefaultProfile)
	require.Len(t, broker.Profiles, 1, "settings with no profiles defined fall back to a single implicit profile")
}

func TestBuildStoreBrokerProfiles_CloudRunFiltersLocalRuntimes(t *testing.T) {
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}

	profiles := buildStoreBrokerProfiles(settings, "cloudrun", nil)

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

	profiles := buildStoreBrokerProfiles(settings, "docker", nil)

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

	profiles := buildStoreBrokerProfiles(settings, "cloudrun", nil)

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

	profiles := buildStoreBrokerProfiles(settings, "docker", nil)

	assert.Len(t, profiles, 2, "starter hub (docker default) should keep docker profiles")
	types := map[string]bool{}
	for _, p := range profiles {
		types[p.Type] = true
	}
	assert.True(t, types["docker"], "docker profile should be present")
	assert.True(t, types["kubernetes"], "kubernetes profile should be present")
}

func TestRegisterGlobalProjectAndBroker_CloudRunSuppressesDockerProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}

	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "hosted-broker", "http://localhost:9800", rt, true, settings, nil)
	require.NoError(t, err)

	broker, err := s.GetRuntimeBroker(ctx, tid("broker-1"))
	require.NoError(t, err)

	for _, p := range broker.Profiles {
		assert.NotEqual(t, "docker", p.Type, "docker profile should not appear in Cloud Run mode")
	}
	assert.Len(t, broker.Profiles, 1)
	assert.Equal(t, "kubernetes", broker.Profiles[0].Type)
}

func TestRegisterGlobalProjectAndBroker_StarterHubKeepsDockerProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local":  {Runtime: "docker"},
			"remote": {Runtime: "kubernetes"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	_, err := registerGlobalProjectAndBroker(ctx, s, tid("broker-1"), "starter-hub", "http://localhost:9800", rt, true, settings, nil)
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

// optOutRuntime is a MockRuntime that also implements the optional
// runtime.AttachCapableRuntime capability, always reporting false.
type optOutRuntime struct {
	*runtime.MockRuntime
}

func (r *optOutRuntime) SupportsAttach() bool { return false }

// noEmptyPerAgentRuntime is a MockRuntime that opts out of the optional
// runtime.EmptyPerAgentCapableRuntime capability, as Cloud Run does.
type noEmptyPerAgentRuntime struct {
	*runtime.MockRuntime
}

func (r *noEmptyPerAgentRuntime) SupportsEmptyPerAgentWorkspace() bool { return false }

// TestRegisterGlobalProjectAndBroker_EmptyPerAgentFollowsDefaultRuntime pins
// that the embedded broker's EmptyPerAgentWorkspace capability reflects its
// default runtime on both the create and the re-register branch (design
// #2703 P2): false for one that opts out, as Cloud Run does.
func TestRegisterGlobalProjectAndBroker_EmptyPerAgentFollowsDefaultRuntime(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}
	rt := &noEmptyPerAgentRuntime{MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}}
	brokerID := tid("broker-no-empty")

	for _, branch := range []string{"create", "re-register"} {
		_, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "no-empty-broker", "http://localhost:9800", rt, true, settings, nil)
		require.NoError(t, err, branch)
		broker, err := s.GetRuntimeBroker(ctx, brokerID)
		require.NoError(t, err, branch)
		require.NotNil(t, broker.Capabilities, branch)
		assert.False(t, broker.Capabilities.EmptyPerAgentWorkspace, "%s: EmptyPerAgentWorkspace must be false for an opted-out default runtime", branch)
	}
}

// TestRegisterGlobalProjectAndBroker_AttachOptOut_PersistsFalseAndCLIRefuses
// is the producer-path integration test: a broker whose default runtime
// opts out of attach registers through the real production path
// (registerGlobalProjectAndBroker -> buildStoreBrokerProfiles), the stored
// record ends up with Attach=false on both the default profile and the
// broker-wide capability, and a CLI attach attempt against an agent on that
// broker refuses before any WebSocket dial. The persisted store.RuntimeBroker
// is round-tripped through JSON into the wire shape a real Hub GET
// /runtime-brokers/{id} response carries (hubclient.RuntimeBroker) rather
// than hand-built as a mock literal, so this proves the producer, the
// *bool encode/decode, and the CLI's read all agree — not just the CLI's
// read of a value nothing upstream actually produces.
func TestRegisterGlobalProjectAndBroker_AttachOptOut_PersistsFalseAndCLIRefuses(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}
	rt := &optOutRuntime{MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "optout" }}}
	brokerID := tid("broker-optout")

	_, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "optout-broker", "http://localhost:9800", rt, true, settings, nil)
	require.NoError(t, err)

	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	require.NotNil(t, broker.Capabilities)
	assert.False(t, broker.Capabilities.Attach, "broker-wide Capabilities.Attach must be false for an opted-out default runtime")
	require.Len(t, broker.Profiles, 1)
	require.NotNil(t, broker.Profiles[0].Attach, "the default profile's Attach must be explicitly set, not left unknown")
	assert.False(t, *broker.Profiles[0].Attach)

	raw, err := json.Marshal(broker)
	require.NoError(t, err)
	agentPath := "/api/v1/projects/proj-optout/agents/optout-agent"
	brokerPath := "/api/v1/runtime-brokers/" + brokerID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case brokerPath:
			_, _ = w.Write(raw)
		case agentPath:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID: "a1", Name: "optout-agent", Phase: "running", Runtime: "optout",
				RuntimeBrokerID: brokerID,
				AppliedConfig:   &hubclient.AgentConfig{Profile: broker.Profiles[0].Name},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-optout"}, "optout-agent")
	require.Error(t, err)
	assert.Equal(t, "attach is not supported for agents on the optout runtime", err.Error())
}

// TestRegisterGlobalProjectAndBroker_UpdateSetsReprovisionCapability is the
// design §3.4 Amendment A2.2(b) regression test: the update
// branch (an existing broker record, re-registering) must refresh
// Capabilities to include Reprovision:true, not just the create branch. A
// broker registered before the reincarnate feature existed would otherwise
// keep Capabilities.Reprovision unset forever, since CompleteBrokerJoin (the
// remote-broker capability path) never runs for the embedded broker.
func TestRegisterGlobalProjectAndBroker_UpdateSetsReprovisionCapability(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}
	brokerID := tid("broker-preexisting")

	// Simulate a broker record from before the Reprovision capability
	// existed: created directly, with the old capability set.
	pre := &store.RuntimeBroker{
		ID:           brokerID,
		Name:         "pre-existing-broker",
		Slug:         "pre-existing-broker",
		Status:       store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{Sync: true, Attach: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, pre))

	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "pre-existing-broker", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, brokerID, effectiveID)

	updated, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	require.NotNil(t, updated.Capabilities)
	assert.True(t, updated.Capabilities.Reprovision,
		"re-registering an existing broker must refresh Capabilities.Reprovision to true")
	assert.True(t, updated.Capabilities.EmptyPerAgentWorkspace,
		"re-registering an existing broker must refresh Capabilities.EmptyPerAgentWorkspace to true")
	assert.True(t, updated.Capabilities.Sync)
	assert.True(t, updated.Capabilities.Attach)
}
