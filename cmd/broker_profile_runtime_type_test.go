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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Broker profiles register their resolved runtime type as Type (the
// runtime entry's explicit type, else its key), not the runtime key.

// runtimeTypeSettings returns versioned settings for one profile per case:
//   - gke:          key "gke", type kubernetes
//   - docker-k8s:   key "docker", type kubernetes
//   - docker-local: key "docker-local", type docker
//   - plain-k8s:    key "kubernetes", no explicit type (key == type)
//   - drifted:      names a different entry than the legacy settings do
func runtimeTypeSettings() *config.VersionedSettings {
	return &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{
			"gke":          {Runtime: "gke"},
			"docker-k8s":   {Runtime: "docker"},
			"docker-local": {Runtime: "docker-local"},
			"plain-k8s":    {Runtime: "kubernetes"},
			// The versioned settings name a different runtime entry than
			// the settings the profiles are built from: keep the key.
			"drifted": {Runtime: "gke"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"gke":          {Type: "kubernetes"},
			"docker":       {Type: "kubernetes"},
			"docker-local": {Type: "docker"},
			"kubernetes":   {},
		},
	}
}

func runtimeTypeLegacySettings(names ...string) *config.Settings {
	all := map[string]config.ProfileConfig{
		"gke":          {Runtime: "gke"},
		"docker-k8s":   {Runtime: "docker"},
		"docker-local": {Runtime: "docker-local"},
		"plain-k8s":    {Runtime: "kubernetes"},
		"drifted":      {Runtime: "k8s-old"},
		"unknown":      {Runtime: "podman"}, // not in the versioned settings
	}
	if len(names) == 0 {
		return &config.Settings{Profiles: all}
	}
	out := map[string]config.ProfileConfig{}
	for _, n := range names {
		out[n] = all[n]
	}
	return &config.Settings{Profiles: out}
}

// stubEmbeddedBrokerProfileSettings substitutes the settings the embedded
// broker resolves profile runtime types from.
func stubEmbeddedBrokerProfileSettings(t *testing.T, vs *config.VersionedSettings, err error) {
	t.Helper()
	prev := loadEmbeddedBrokerProfileSettings
	t.Cleanup(func() { loadEmbeddedBrokerProfileSettings = prev })
	loadEmbeddedBrokerProfileSettings = func() (*config.VersionedSettings, error) { return vs, err }
}

func storeProfileByName(t *testing.T, profiles []store.BrokerProfile, name string) store.BrokerProfile {
	t.Helper()
	for _, p := range profiles {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("profile %q not found in %+v", name, profiles)
	return store.BrokerProfile{}
}

func TestBuildBrokerProfiles_RegistersResolvedRuntimeType(t *testing.T) {
	stubBrokerMappingSettings(t, runtimeTypeSettings(), nil)

	profiles := buildBrokerProfiles(runtimeTypeLegacySettings())

	for name, want := range map[string]string{
		"gke":          "kubernetes",
		"docker-k8s":   "kubernetes",
		"docker-local": "docker",
		"plain-k8s":    "kubernetes", // key == type: unchanged
		"drifted":      "k8s-old",    // settings disagree on the entry: key kept
		"unknown":      "podman",     // not in the versioned settings: key kept
	} {
		assert.Equal(t, want, profileByName(t, profiles, name).Type, "profile %q", name)
	}
}

// A settings load failure at join must not fail or block the join: profiles
// register their runtime key as Type, as brokers did before.
func TestBuildBrokerProfiles_SettingsLoadErrorKeepsRuntimeKey(t *testing.T) {
	stubBrokerMappingSettings(t, nil, errors.New("boom"))

	profiles := buildBrokerProfiles(runtimeTypeLegacySettings("gke", "docker-k8s", "docker-local"))

	assert.Equal(t, "gke", profileByName(t, profiles, "gke").Type)
	assert.Equal(t, "docker", profileByName(t, profiles, "docker-k8s").Type)
	assert.Equal(t, "docker-local", profileByName(t, profiles, "docker-local").Type)
}

func TestBuildStoreBrokerProfiles_RegistersResolvedRuntimeType(t *testing.T) {
	stubEmbeddedBrokerProfileSettings(t, runtimeTypeSettings(), nil)

	profiles := buildStoreBrokerProfiles(runtimeTypeLegacySettings(), "docker", nil)

	for name, want := range map[string]string{
		"gke":          "kubernetes",
		"docker-k8s":   "kubernetes",
		"docker-local": "docker",
		"plain-k8s":    "kubernetes",
		"drifted":      "k8s-old",
		"unknown":      "podman",
	} {
		assert.Equal(t, want, storeProfileByName(t, profiles, name).Type, "profile %q", name)
	}
}

func TestBuildStoreBrokerProfiles_SettingsLoadErrorKeepsRuntimeKey(t *testing.T) {
	stubEmbeddedBrokerProfileSettings(t, nil, errors.New("boom"))

	profiles := buildStoreBrokerProfiles(runtimeTypeLegacySettings("gke", "docker-k8s"), "docker", nil)

	assert.Equal(t, "gke", storeProfileByName(t, profiles, "gke").Type)
	assert.Equal(t, "docker", storeProfileByName(t, profiles, "docker-k8s").Type)
}

// The embedded broker's local-only filter uses the resolved type: on a
// non-local default runtime, a docker-typed entry under a custom key is
// filtered out, and a kubernetes-typed entry under a custom key is kept.
func TestBuildStoreBrokerProfiles_LocalOnlyFilterUsesResolvedType(t *testing.T) {
	stubEmbeddedBrokerProfileSettings(t, runtimeTypeSettings(), nil)

	profiles := buildStoreBrokerProfiles(runtimeTypeLegacySettings("gke", "docker-local", "docker-k8s"), "kubernetes", nil)

	names := map[string]bool{}
	for _, p := range profiles {
		names[p.Name] = true
	}
	assert.True(t, names["gke"], "key gke / type kubernetes must be kept on a kubernetes default runtime")
	assert.True(t, names["docker-k8s"], "key docker / type kubernetes must be kept on a kubernetes default runtime")
	assert.False(t, names["docker-local"], "key docker-local / type docker is local-only and must be filtered out")
}

// attachForType answers for a profile whose resolved type is the default
// runtime's, whatever its key.
func TestBuildStoreBrokerProfiles_AttachUsesResolvedType(t *testing.T) {
	stubEmbeddedBrokerProfileSettings(t, runtimeTypeSettings(), nil)

	k8sRT := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	profiles := buildStoreBrokerProfiles(runtimeTypeLegacySettings("gke"), "kubernetes", k8sRT)
	gke := storeProfileByName(t, profiles, "gke")
	require.NotNil(t, gke.Attach, "key gke / type kubernetes must get the kubernetes default runtime's attach answer")
	assert.Equal(t, runtime.HasAttachSupport(k8sRT), *gke.Attach)

	dockerRT := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	profiles = buildStoreBrokerProfiles(runtimeTypeLegacySettings("docker-local"), "docker", dockerRT)
	local := storeProfileByName(t, profiles, "docker-local")
	require.NotNil(t, local.Attach, "key docker-local / type docker must get the docker default runtime's attach answer")
	assert.Equal(t, runtime.HasAttachSupport(dockerRT), *local.Attach)
}

// --- End to end: embedded registration, then the hub's checks. ---

const profileTypeTestDevToken = "scion_dev_profile_type_test_0123456789abcdef"

type profileTypeHub struct {
	srv      *hub.Server
	store    store.Store
	brokerID string
	project  *store.Project
}

// newProfileTypeHub registers the embedded broker with the given profiles
// through registerGlobalProjectAndBroker and starts a hub on the same store.
func newProfileTypeHub(t *testing.T, rtName string, legacy *config.Settings, defaults opsettings.AgentDefaultsSettings) *profileTypeHub {
	t.Helper()
	return newProfileTypeHubWith(t, runtimeTypeSettings(), rtName, legacy, defaults)
}

func newProfileTypeHubWith(t *testing.T, vs *config.VersionedSettings, rtName string, legacy *config.Settings, defaults opsettings.AgentDefaultsSettings) *profileTypeHub {
	t.Helper()
	stubEmbeddedBrokerProfileSettings(t, vs, nil)
	ctx := context.Background()
	s := newTestStore(t)
	rt := &runtime.MockRuntime{NameFunc: func() string { return rtName }}
	brokerID := tid("broker-profile-type")
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "profile-type-broker", "http://localhost:9800", rt, true, legacy, nil)
	require.NoError(t, err)

	cfg := hub.DefaultServerConfig()
	cfg.DevAuthToken = profileTypeTestDevToken
	cfg.DisableCloudLogQuery = true
	cfg.DevUserConfig = hub.DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	cfg.AgentDefaults = defaults
	srv, err := hub.New(cfg, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	srv.SetEmbeddedBrokerID(effectiveID)

	project, err := s.GetProjectBySlug(ctx, GlobalProjectName)
	require.NoError(t, err)
	return &profileTypeHub{srv: srv, store: s, brokerID: effectiveID, project: project}
}

func (h *profileTypeHub) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+profileTypeTestDevToken)
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *profileTypeHub) storedProfile(t *testing.T, name string) store.BrokerProfile {
	t.Helper()
	b, err := h.store.GetRuntimeBroker(context.Background(), h.brokerID)
	require.NoError(t, err)
	return storeProfileByName(t, b.Profiles, name)
}

// The project settings save check (projectIsKubernetesBound ->
// brokerIsKubernetesOnly) refuses a "block" default for a project whose only
// broker runs a kubernetes-typed entry under a custom key.
func TestProfileRuntimeType_E2E_ProjectSettingsSeesCustomKeyAsKubernetes(t *testing.T) {
	h := newProfileTypeHub(t, "kubernetes", runtimeTypeLegacySettings("gke"), opsettings.AgentDefaultsSettings{})
	assert.Equal(t, "kubernetes", h.storedProfile(t, "gke").Type)

	rec := h.do(t, http.MethodPut, "/api/v1/projects/"+h.project.ID+"/settings",
		hubclient.ProjectSettings{DefaultGCPIdentityMode: "block"})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"key gke / type kubernetes must be seen as Kubernetes; body: %s", rec.Body.String())
}

// Key == type is unchanged: a key "kubernetes" entry is still Kubernetes,
// and a key "docker" entry with no explicit type is still docker.
func TestProfileRuntimeType_E2E_ProjectSettingsKeyEqualsTypeUnchanged(t *testing.T) {
	t.Run("key kubernetes", func(t *testing.T) {
		h := newProfileTypeHub(t, "kubernetes", runtimeTypeLegacySettings("plain-k8s"), opsettings.AgentDefaultsSettings{})
		assert.Equal(t, "kubernetes", h.storedProfile(t, "plain-k8s").Type)
		rec := h.do(t, http.MethodPut, "/api/v1/projects/"+h.project.ID+"/settings",
			hubclient.ProjectSettings{DefaultGCPIdentityMode: "block"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("key docker", func(t *testing.T) {
		plain := &config.VersionedSettings{
			Profiles: map[string]config.V1ProfileConfig{"local": {Runtime: "docker"}},
			Runtimes: map[string]config.V1RuntimeConfig{"docker": {}},
		}
		h := newProfileTypeHubWith(t, plain, "docker",
			&config.Settings{Profiles: map[string]config.ProfileConfig{"local": {Runtime: "docker"}}}, opsettings.AgentDefaultsSettings{})
		assert.Equal(t, "docker", h.storedProfile(t, "local").Type)
		rec := h.do(t, http.MethodPut, "/api/v1/projects/"+h.project.ID+"/settings",
			hubclient.ProjectSettings{DefaultGCPIdentityMode: "block"})
		assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	})
}

// createdAgentGCPIdentity creates an agent on the global project and
// returns its stored GCP identity (nil when the hub left it unset).
func (h *profileTypeHub) createdAgentGCPIdentity(t *testing.T, name, profile string) *store.GCPIdentityConfig {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/v1/projects/"+h.project.ID+"/agents",
		hub.CreateAgentRequest{Name: name, Profile: profile})
	require.Equal(t, http.StatusCreated, rec.Code, "agent creation should succeed; body: %s", rec.Body.String())
	var resp hub.CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	got, err := h.store.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	return got.AppliedConfig.GCPIdentity
}

// The hub-default passthrough gate allows only docker/podman runtimes. It
// tightens for a docker-keyed entry whose type is kubernetes (previously
// registered as "docker" and allowed), and now allows a docker-typed entry
// under a custom key (previously registered under that key and denied).
func TestProfileRuntimeType_E2E_PassthroughGateUsesResolvedType(t *testing.T) {
	passthrough := opsettings.AgentDefaultsSettings{DefaultGCPIdentityMode: store.GCPMetadataModePassthrough}

	t.Run("key docker type kubernetes is denied", func(t *testing.T) {
		h := newProfileTypeHub(t, "docker", runtimeTypeLegacySettings("docker-k8s"), passthrough)
		assert.Equal(t, "kubernetes", h.storedProfile(t, "docker-k8s").Type)
		assert.Nil(t, h.createdAgentGCPIdentity(t, "gate-docker-k8s", "docker-k8s"),
			"a docker-keyed kubernetes runtime must not get the hub-default passthrough")
	})

	t.Run("key gke type kubernetes is denied", func(t *testing.T) {
		h := newProfileTypeHub(t, "kubernetes", runtimeTypeLegacySettings("gke"), passthrough)
		assert.Nil(t, h.createdAgentGCPIdentity(t, "gate-gke", "gke"))
	})

	t.Run("key docker-local type docker is allowed", func(t *testing.T) {
		h := newProfileTypeHub(t, "docker", runtimeTypeLegacySettings("docker-local"), passthrough)
		assert.Equal(t, "docker", h.storedProfile(t, "docker-local").Type)
		identity := h.createdAgentGCPIdentity(t, "gate-docker-local", "docker-local")
		require.NotNil(t, identity, "a docker-typed runtime under a custom key must get the hub-default passthrough")
		assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode)
	})
}

// The agent.Runtime backfill (resolveAgentRuntime) reads the profile's Type,
// so an agent on a key-not-equal-type profile backfills the TYPE.
func TestProfileRuntimeType_E2E_AgentRuntimeBackfillUsesType(t *testing.T) {
	h := newProfileTypeHub(t, "kubernetes", runtimeTypeLegacySettings("gke"), opsettings.AgentDefaultsSettings{})
	ctx := context.Background()
	agent := &store.Agent{
		ID:              tid("agent-backfill"),
		Slug:            "backfill-agent",
		Name:            "backfill-agent",
		ProjectID:       h.project.ID,
		RuntimeBrokerID: h.brokerID,
		AppliedConfig:   &store.AgentAppliedConfig{Profile: "gke"},
	}
	require.NoError(t, h.store.CreateAgent(ctx, agent))

	rec := h.do(t, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got struct {
		Runtime string `json:"runtime"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "kubernetes", got.Runtime)
}
