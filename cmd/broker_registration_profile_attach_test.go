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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveStoredBroker round-trips a persisted store.RuntimeBroker through
// JSON and serves it as a Hub GET /runtime-brokers/{id} response, returning
// a HubContext whose client reads it back in the hubclient wire shape.
func serveStoredBroker(t *testing.T, broker *store.RuntimeBroker) *HubContext {
	t.Helper()
	raw, err := json.Marshal(broker)
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runtime-brokers/"+broker.ID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL}
}

// TestRegisterGlobalProjectAndBroker_AttachOptOut_ConfiguredProfiles_PerProfileAttach
// covers the settings-profiles branch of buildStoreBrokerProfiles through
// the real registration producer — the branch every broker with profiles
// configured takes, and which the empty-settings producer test never
// reaches. A profile resolving to the opted-out default runtime's type must
// persist Attach=&false (so the CLI refuses it), while a profile of another
// type — no live instance backs it — must persist Attach=nil (unknown, read
// as supported), not inherit the default runtime's answer.
func TestRegisterGlobalProjectAndBroker_AttachOptOut_ConfiguredProfiles_PerProfileAttach(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"optout-prof": {Runtime: "optout"},
			"k8s-prof":    {Runtime: "kubernetes"},
		},
	}
	rt := &optOutRuntime{MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "optout" }}}
	brokerID := tid("broker-optout-profiles")

	_, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "optout-profiles-broker", "http://localhost:9800", rt, true, settings)
	require.NoError(t, err)

	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	byName := map[string]store.BrokerProfile{}
	for _, p := range broker.Profiles {
		byName[p.Name] = p
	}
	require.Len(t, byName, 2)

	optout, ok := byName["optout-prof"]
	require.True(t, ok)
	require.NotNil(t, optout.Attach, "a profile of the opted-out default runtime's type must carry an explicit Attach, not unknown")
	assert.False(t, *optout.Attach)

	k8s, ok := byName["k8s-prof"]
	require.True(t, ok)
	assert.Nil(t, k8s.Attach, "a profile of a different type has no live instance to ask and must stay unknown, not inherit the default runtime's answer")

	hubCtx := serveStoredBroker(t, broker)
	assert.False(t, attachSupportedByBroker(ctx, hubCtx, brokerID, "optout-prof"),
		"CLI must refuse attach for the opted-out profile as persisted by the real producer")
	// k8s-prof's own Attach is unknown (nil), so attachSupportedByBroker
	// falls through to the broker-wide Capabilities.Attach, which is false
	// here — the accepted cost documented on attachSupportedByBroker: a
	// non-default-type profile with no live instance of its own inherits
	// the opted-out default runtime's broker-wide answer rather than being
	// treated as supported by default.
	assert.False(t, attachSupportedByBroker(ctx, hubCtx, brokerID, "k8s-prof"),
		"CLI must fall through to the broker-wide false for a profile with no Attach of its own")
}

// TestRegisterGlobalProjectAndBroker_ReRegistration_RefreshesAttachFromLiveRuntime
// covers the update branch of registerGlobalProjectAndBroker (an existing
// broker record re-registering — every restart of an embedded broker):
// a record first written while the runtime supported attach must be
// refreshed to Attach=false, broker-wide and on the profile, once the live
// runtime opts out. Without this, a hard-coded or stale value on the update
// path would never be caught, since the create branch is the only one the
// other producer test reaches.
func TestRegisterGlobalProjectAndBroker_ReRegistration_RefreshesAttachFromLiveRuntime(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}
	brokerID := tid("broker-optout-rereg")

	supporting := &runtime.MockRuntime{NameFunc: func() string { return "optout" }}
	_, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "optout-rereg-broker", "http://localhost:9800", supporting, true, settings)
	require.NoError(t, err)
	before, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	require.NotNil(t, before.Capabilities)
	require.True(t, before.Capabilities.Attach, "precondition: first registration with an attach-capable runtime records Attach=true")

	optOut := &optOutRuntime{MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "optout" }}}
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "optout-rereg-broker", "http://localhost:9800", optOut, true, settings)
	require.NoError(t, err)
	require.Equal(t, brokerID, effectiveID, "precondition: second registration must take the update branch for the same record")

	after, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	require.NotNil(t, after.Capabilities)
	assert.False(t, after.Capabilities.Attach, "re-registration must refresh broker-wide Capabilities.Attach from the live runtime")
	require.Len(t, after.Profiles, 1)
	require.NotNil(t, after.Profiles[0].Attach)
	assert.False(t, *after.Profiles[0].Attach, "re-registration must refresh the default profile's Attach from the live runtime")
}

// TestBuildStoreBrokerProfiles_AllProfilesFiltered_DefaultAsksLiveRuntime
// covers the second synthesized-"default" fallback of the embedded broker's
// registration producer: every configured profile is filtered out (a
// local-only runtime on a non-local default), so buildStoreBrokerProfiles
// synthesizes a "default" profile of the default runtime type. That profile
// is backed by the live default runtime, so it must carry that runtime's
// real answer (&false for an opted-out runtime), not be left unknown — an
// unknown here would hand the CLI a nil profile Attach for the only profile
// the broker advertises.
func TestBuildStoreBrokerProfiles_AllProfilesFiltered_DefaultAsksLiveRuntime(t *testing.T) {
	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}
	rt := &optOutRuntime{MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "optout" }}}

	profiles := buildStoreBrokerProfiles(settings, "optout", rt)

	require.Len(t, profiles, 1)
	require.Equal(t, "default", profiles[0].Name, "precondition: the docker profile must be filtered out on a non-local default")
	require.Equal(t, "optout", profiles[0].Type)
	require.NotNil(t, profiles[0].Attach, "synthesized default profile of an opted-out default runtime must carry an explicit Attach")
	require.False(t, *profiles[0].Attach)
}
