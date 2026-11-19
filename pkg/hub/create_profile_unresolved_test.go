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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Hub create/provision/finalize relaying a Runtime Broker's
// profile_unresolved (runtime_unavailable 503) refusal with valid profile
// names from resolvedBroker.Profiles and rolling back row, quota and
// credentials (ptone/scion#3817).

func brokerProfileUnresolvedErr(profile string) error {
	return &brokerStatusError{
		StatusCode: http.StatusServiceUnavailable,
		Body:       profileUnresolvedBody(`"` + profile + `"`),
		RetryAfter: "30",
	}
}

// TestCreateAgent_ProfileUnresolvedRelayedAndRolledBack verifies that across
// normal create, gather-env create, and provisionOnly create:
//   - a broker profile_unresolved 503 is relayed via the
//     writeBrokerRuntimeUnavailable path (503 runtime_unavailable, no
//     Retry-After, broker body not echoed) instead of 502 runtime_error or a
//     201 with a warning;
//   - valid profile names from resolvedBroker.Profiles are appended when known;
//   - the agent row and both project and broker quota reservations are rolled
//     back cleanly.
func TestCreateAgent_ProfileUnresolvedRelayedAndRolledBack(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gatherEnv     bool
		provisionOnly bool
	}{
		{name: "create"},
		{name: "gather_env", gatherEnv: true},
		{name: "provision_only", provisionOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			refusal := brokerProfileUnresolvedErr("typo-prof")
			var disp AgentDispatcher = &failingCreateDispatcher{createErr: refusal}
			if tc.provisionOnly {
				disp = &skillFailDispatcher{provisionErr: refusal}
			}
			srv, s, project := setupCreateAgentServer(t, disp)
			setBrokerAgentCeiling(t, s, 1)
			setProjectAgentCeiling(t, s, 1)

			brokerID := project.DefaultRuntimeBrokerID
			broker, err := s.GetRuntimeBroker(ctx, brokerID)
			require.NoError(t, err)
			broker.Profiles = []store.BrokerProfile{
				{Name: "local", Type: "docker", Available: true},
				{Name: "k8s-prod", Type: "kubernetes", Available: true},
			}
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

			slug := "refused-" + tidSlugSafe(tc.name)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:          slug,
				ProjectID:     project.ID,
				Profile:       "typo-prof",
				GatherEnv:     tc.gatherEnv,
				ProvisionOnly: tc.provisionOnly,
			})
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			assert.Empty(t, rec.Header().Get("Retry-After"), "profile_unresolved is not transient")

			code, msg := decodeHubError(t, rec)
			assert.Equal(t, brokerCodeRuntimeUnavailable, code)
			assert.Contains(t, msg, `Runtime profile "typo-prof" cannot be resolved`)
			assert.Contains(t, msg, "not configured for this broker")
			assert.NotContains(t, msg, "hub's stored profiles")
			assert.Contains(t, msg, "(available profiles: k8s-prod, local)")
			assert.NotContains(t, msg, "available profiles: x", "raw broker text must not be echoed")

			_, err = s.GetAgentBySlug(ctx, project.ID, slug)
			assert.ErrorIs(t, err, store.ErrNotFound, "agent row must be rolled back")
			assert.Empty(t, activeReservationResources(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, brokerID),
				"broker quota reservation must be rolled back")
			assert.Empty(t, activeReservationResources(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, project.ID),
				"project quota reservation must be rolled back")
		})
	}
}

// TestCreateAgent_ProfileUnresolvedWithoutKnownBrokerProfiles verifies that
// when resolvedBroker.Profiles is empty, the base writeBrokerRuntimeUnavailable
// message is returned without an empty available-profiles suffix, and a plain
// runtime_unavailable 503 (not profile_unresolved) preserves Retry-After.
func TestCreateAgent_ProfileUnresolvedWithoutKnownBrokerProfiles(t *testing.T) {
	t.Run("no stored broker profiles", func(t *testing.T) {
		refusal := brokerProfileUnresolvedErr("typo-prof")
		disp := &failingCreateDispatcher{createErr: refusal}
		srv, _, project := setupCreateAgentServer(t, disp)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "no-profiles-list",
			ProjectID: project.ID,
			Profile:   "typo-prof",
		})
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		assert.Empty(t, rec.Header().Get("Retry-After"))
		code, msg := decodeHubError(t, rec)
		assert.Equal(t, brokerCodeRuntimeUnavailable, code)
		assert.Equal(t, brokerProfileUnresolvedMessage("typo-prof"), msg)
	})

	t.Run("generic runtime_unavailable keeps Retry-After", func(t *testing.T) {
		generic := &brokerStatusError{
			StatusCode: http.StatusServiceUnavailable,
			Body:       `{"error":{"code":"runtime_unavailable","message":"runtime down"}}`,
			RetryAfter: "15",
		}
		disp := &failingCreateDispatcher{createErr: generic}
		srv, _, project := setupCreateAgentServer(t, disp)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "generic-rt-down",
			ProjectID: project.ID,
		})
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		assert.Equal(t, "15", rec.Header().Get("Retry-After"))
		code, _ := decodeHubError(t, rec)
		assert.Equal(t, brokerCodeRuntimeUnavailable, code)
	})
}

// TestCreateAgent_BrokerDrainingRelayedAndRolledBack verifies that across
// normal create, gather-env create, and provisionOnly create, a broker's 503
// broker_draining is relayed with code broker_draining and Retry-After instead
// of 502 runtime_error or a 201 with a warning, and rolls back the agent row
// and both project and broker quota reservations.
func TestCreateAgent_BrokerDrainingRelayedAndRolledBack(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gatherEnv     bool
		provisionOnly bool
	}{
		{name: "create"},
		{name: "gather_env", gatherEnv: true},
		{name: "provision_only", provisionOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			draining := drainingErr("7")
			var disp AgentDispatcher = &failingCreateDispatcher{createErr: draining}
			if tc.provisionOnly {
				disp = &skillFailDispatcher{provisionErr: draining}
			}
			srv, s, project := setupCreateAgentServer(t, disp)
			setBrokerAgentCeiling(t, s, 1)
			setProjectAgentCeiling(t, s, 1)
			brokerID := project.DefaultRuntimeBrokerID

			slug := "draining-" + tidSlugSafe(tc.name)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:          slug,
				ProjectID:     project.ID,
				GatherEnv:     tc.gatherEnv,
				ProvisionOnly: tc.provisionOnly,
			})
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			assert.Equal(t, "7", rec.Header().Get("Retry-After"))

			code, msg := decodeHubError(t, rec)
			assert.Equal(t, api.BrokerErrCodeBrokerDraining, code)
			assert.Equal(t, brokerDrainingMessage, msg)

			_, err := s.GetAgentBySlug(ctx, project.ID, slug)
			assert.ErrorIs(t, err, store.ErrNotFound, "agent row must be rolled back")
			assert.Empty(t, activeReservationResources(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, brokerID),
				"broker quota reservation must be rolled back")
			assert.Empty(t, activeReservationResources(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, project.ID),
				"project quota reservation must be rolled back")
		})
	}
}

// TestFinalizeCreate_ProfileUnresolvedRelayed covers workspace-upload finalize
// and env-gather finalize relaying profile_unresolved with known broker
// profiles instead of 500/502.
func TestFinalizeCreate_ProfileUnresolvedRelayed(t *testing.T) {
	t.Run("workspace finalize", func(t *testing.T) {
		ctx := context.Background()
		disp := &skillFailDispatcher{createErr: brokerProfileUnresolvedErr("typo-prof")}
		srv, s, project := setupCreateAgentServer(t, disp)
		srv.SetStorage(newMockStorage("test-bucket"))

		broker, err := s.GetRuntimeBroker(ctx, project.DefaultRuntimeBrokerID)
		require.NoError(t, err)
		broker.Profiles = []store.BrokerProfile{
			{Name: "local", Type: "docker", Available: true},
			{Name: "staging", Type: "kubernetes", Available: true},
		}
		require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

		ag := createSiteAgent(t, s, project, "ws-prof-fail", state.PhaseProvisioning, store.RunIntentStopped)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+ag.ID+"/workspace/sync-to/finalize",
			map[string]any{"manifest": map[string]any{"version": "1.0", "files": []any{}}})
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		code, msg := decodeHubError(t, rec)
		assert.Equal(t, brokerCodeRuntimeUnavailable, code)
		assert.Contains(t, msg, "(available profiles: local, staging)")
	})

	t.Run("env finalize", func(t *testing.T) {
		rec := submitEnvWithFinalizeErr(t, "env-prof-fail", brokerProfileUnresolvedErr("typo-prof"))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		code, msg := decodeHubError(t, rec)
		assert.Equal(t, brokerCodeRuntimeUnavailable, code)
		assert.Contains(t, msg, `Runtime profile "typo-prof" cannot be resolved`)
	})
}

type e2eCreateProfileManager struct {
	credentialStartManager
	mu          sync.Mutex
	provisioned []string
}

func (m *e2eCreateProfileManager) Provision(_ context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	m.mu.Lock()
	m.provisioned = append(m.provisioned, opts.Name)
	m.mu.Unlock()
	return &api.ScionConfig{Harness: "claude", HarnessConfig: "claude"}, nil
}

func (m *e2eCreateProfileManager) Preflight(_ context.Context, _ api.StartOptions) error {
	return nil
}

func (m *e2eCreateProfileManager) CleanupLaunch(_ context.Context, _ []agent.ResourceHandle) error {
	return nil
}

type credRecordingTokenGen struct {
	inner AgentTokenGenerator
	mu    sync.Mutex
	creds []*store.AgentCredential
}

func (g *credRecordingTokenGen) AuthorizeAgentToken(ctx context.Context, ag *store.Agent) (AgentTokenGrant, error) {
	return g.inner.AuthorizeAgentToken(ctx, ag)
}

func (g *credRecordingTokenGen) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	tok, cred, err := g.inner.SignAgentToken(grant, runID)
	if err == nil && cred != nil {
		g.mu.Lock()
		g.creds = append(g.creds, cred)
		g.mu.Unlock()
	}
	return tok, cred, err
}

func (g *credRecordingTokenGen) minted() []*store.AgentCredential {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*store.AgentCredential(nil), g.creds...)
}

// TestCreateAgent_EndToEndUnknownProfileRefusedAndRolledBack exercises the full
// Hub -> HTTPAgentDispatcher -> runtimebroker.Server path without a DB
// settings overlay:
//   - typo profile on create and provisionOnly is refused with 503
//     runtime_unavailable + valid names, rolling back the row, quotas, and any
//     minted agent credential;
//   - valid explicit and default-profile creates succeed;
//   - flat Runtime Brokers keep returning 422 runtime_profile_unsupported.
func TestCreateAgent_EndToEndUnknownProfileRefusedAndRolledBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	prevOverlay := config.GetGlobalSettingsOverlay()
	config.SetGlobalSettingsOverlay(nil)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prevOverlay) })

	ctx := context.Background()
	srv, s := testServer(t)
	reconcileBuiltInRoles(ctx, s)

	projectID := tid("proj-e2e-prof")
	scionDir := filepath.Join(t.TempDir(), "proj", ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: docker
    staging:
        runtime: docker
runtimes:
    docker:
        type: docker
`), 0o644))
	for _, tpl := range []string{"default", "claude"} {
		tplDir := filepath.Join(scionDir, "templates", tpl)
		require.NoError(t, os.MkdirAll(tplDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness_config: "+tpl+"\n"), 0o644))
		hcDir := filepath.Join(scionDir, "harness-configs", tpl)
		require.NoError(t, os.MkdirAll(hcDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nimage: test-image:claude\n"), 0o644))
	}
	require.NoError(t, config.WriteProjectID(scionDir, projectID))

	mgr := &e2eCreateProfileManager{}
	bCfg := runtimebroker.DefaultServerConfig()
	bCfg.BrokerAuthEnabled = false
	bCfg.BrokerID = "e2e-prof-broker"
	bCfg.BrokerName = "e2e-prof-broker"
	bCfg.StateDir = t.TempDir()
	bCfg.ForceRuntime = ""
	brokerSrv := runtimebroker.New(bCfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	httpSrv := httptest.NewServer(brokerSrv.Handler())
	defer httpSrv.Close()

	brokerID := tid("broker-e2e-prof")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                     projectID,
		Name:                   "E2E Profile Project",
		Slug:                   "e2e-profile-project",
		DefaultRuntimeBrokerID: brokerID,
	}))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:             brokerID,
		Name:           "e2e-prof-broker",
		Slug:           "e2e-prof-broker",
		Endpoint:       httpSrv.URL,
		Status:         store.BrokerStatusOnline,
		DefaultProfile: "local",
		Profiles: []store.BrokerProfile{
			{Name: "local", Type: "docker", Available: true},
			{Name: "staging", Type: "docker", Available: true},
		},
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "e2e-prof-broker",
		LocalPath:  scionDir,
		Status:     store.BrokerStatusOnline,
	}))

	svc, err := NewAgentTokenService(AgentTokenConfig{})
	require.NoError(t, err)
	gen := &credRecordingTokenGen{inner: runTokenGenerator{svc: svc}}
	d := NewHTTPAgentDispatcherWithClient(s, NewHTTPRuntimeBrokerClient(), false, slog.Default())
	d.SetTokenGenerator(gen)
	srv.SetDispatcher(d)

	setBrokerAgentCeiling(t, s, 2)
	setProjectAgentCeiling(t, s, 2)

	// 1. Typo profile on create and provisionOnly is refused with 503 + valid
	// names, rolling back row, quotas, and minted credential.
	for _, provOnly := range []bool{false, true} {
		slug := "e2e-typo-create"
		if provOnly {
			slug = "e2e-typo-prov"
		}
		beforeMints := len(gen.minted())
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:          slug,
			ProjectID:     projectID,
			Template:      "claude",
			Profile:       "no-such-profile",
			ProvisionOnly: provOnly,
		})
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		code, msg := decodeHubError(t, rec)
		assert.Equal(t, brokerCodeRuntimeUnavailable, code)
		assert.Contains(t, msg, `Runtime profile "no-such-profile" cannot be resolved`)
		assert.Contains(t, msg, "(available profiles: local, staging)")

		_, err := s.GetAgentBySlug(ctx, projectID, slug)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Empty(t, activeReservationResources(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, projectID))
		assert.Empty(t, activeReservationResources(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, brokerID))

		mintedNow := gen.minted()[beforeMints:]
		require.NotEmpty(t, mintedNow, "dispatch must have minted an agent credential before calling the broker")
		for _, c := range mintedNow {
			storedCred, cerr := s.GetAgentCredentialByJTIHash(ctx, c.TokenJTIHash)
			require.NoError(t, cerr)
			assert.NotNil(t, storedCred.RevokedAt, "minted credential %s must be revoked on refused create", c.ID)
		}
	}
	assert.Empty(t, mgr.starts, "broker manager Start must not be called for unknown profile")
	assert.Empty(t, mgr.provisioned, "broker manager Provision must not be called for unknown profile")

	// 2. Valid explicit profile and default profile succeed.
	for _, prof := range []string{"staging", ""} {
		slug := "e2e-ok-" + prof
		if prof == "" {
			slug = "e2e-ok-default"
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:          slug,
			ProjectID:     projectID,
			Template:      "claude",
			Profile:       prof,
			NoAuth:        true,
			ProvisionOnly: true,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	// 3. Flat Runtime Broker remains unaffected (422 runtime_profile_unsupported).
	flatFix := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	require.NoError(t, flatFix.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  flatFix.project.ID,
		BrokerID:   flatFix.flat.ID,
		BrokerName: flatFix.flat.Name,
		Status:     store.BrokerStatusOnline,
	}))
	recFlat := flatFix.create(t, map[string]interface{}{
		"name":            "e2e-flat-prof",
		"runtimeBrokerId": flatFix.flat.ID,
		"profile":         "no-such-profile",
	})
	requireAPIError(t, recFlat, http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported)
}
