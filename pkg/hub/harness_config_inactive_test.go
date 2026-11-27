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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedHarnessConfigWithStatus(t *testing.T, s store.Store, slug, harnessType, scope, scopeID, status string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        slug,
		Slug:        slug,
		Harness:     harnessType,
		ContentHash: slug + "-" + scope + "-" + status,
		Scope:       scope,
		ScopeID:     scopeID,
		Status:      status,
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

func TestResolveHarnessConfigBySlug_RefusesInactiveNoFallthrough(t *testing.T) {
	ctx := context.Background()

	for _, status := range []string{store.HarnessConfigStatusArchived, store.HarnessConfigStatusPending} {
		t.Run("hosted_"+status+"_global_no_fallthrough_to_project", func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = false
			projectID := tid("hc-inactive-hosted-" + status)

			seedHarnessConfigWithStatus(t, s, "shared-hc", "claude", store.HarnessConfigScopeGlobal, "", status)
			seedHarnessConfigWithStatus(t, s, "shared-hc", "claude", store.HarnessConfigScopeProject, projectID, store.HarnessConfigStatusActive)

			got, err := srv.resolveHarnessConfigBySlug(ctx, projectID, "shared-hc")
			assert.Nil(t, got)
			var notActive *harnessConfigNotActiveError
			require.ErrorAs(t, err, &notActive)
			assert.Equal(t, "shared-hc", notActive.Name)
			assert.Equal(t, status, notActive.Status)
			assert.Equal(t, store.HarnessConfigScopeGlobal, notActive.Scope)
			assert.Contains(t, err.Error(), "shared-hc")
			assert.Contains(t, err.Error(), status)
		})

		t.Run("workstation_"+status+"_project_no_fallthrough_to_global", func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = true
			projectID := tid("hc-inactive-ws-" + status)

			seedHarnessConfigWithStatus(t, s, "shared-hc", "claude", store.HarnessConfigScopeProject, projectID, status)
			seedHarnessConfigWithStatus(t, s, "shared-hc", "claude", store.HarnessConfigScopeGlobal, "", store.HarnessConfigStatusActive)

			got, err := srv.resolveHarnessConfigBySlug(ctx, projectID, "shared-hc")
			assert.Nil(t, got)
			var notActive *harnessConfigNotActiveError
			require.ErrorAs(t, err, &notActive)
			assert.Equal(t, "shared-hc", notActive.Name)
			assert.Equal(t, status, notActive.Status)
			assert.Equal(t, store.HarnessConfigScopeProject, notActive.Scope)
		})
	}

	t.Run("active_resolves_and_absent_returns_nil", func(t *testing.T) {
		srv, s := testServer(t)
		srv.workstation = false
		projectID := tid("hc-active-and-absent")

		active := seedHarnessConfigWithStatus(t, s, "active-hc", "claude", store.HarnessConfigScopeGlobal, "", store.HarnessConfigStatusActive)

		got, err := srv.resolveHarnessConfigBySlug(ctx, projectID, "active-hc")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, active.ID, got.ID)

		got, err = srv.resolveHarnessConfigBySlug(ctx, projectID, "deleted-or-absent-hc")
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

func TestCreateAgent_RefusesInactiveHarnessConfig(t *testing.T) {
	ctx := context.Background()

	setupProjectWithBroker := func(t *testing.T, s store.Store, suffix string) *store.Project {
		t.Helper()
		project := &store.Project{
			ID:        tid("proj-create-" + suffix),
			Name:      "Create " + suffix,
			Slug:      "create-" + suffix,
			GitRemote: "https://github.com/example/repo.git",
		}
		require.NoError(t, s.CreateProject(ctx, project))

		broker := &store.RuntimeBroker{
			ID:     tid("broker-create-" + suffix),
			Name:   "Broker " + suffix,
			Slug:   "broker-" + suffix,
			Status: store.BrokerStatusOnline,
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			Status:     store.BrokerStatusOnline,
		}))
		project.DefaultRuntimeBrokerID = broker.ID
		require.NoError(t, s.UpdateProject(ctx, project))
		return project
	}

	for _, status := range []string{store.HarnessConfigStatusArchived, store.HarnessConfigStatusPending} {
		t.Run("refuses_"+status+"_global", func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = false

			project := setupProjectWithBroker(t, s, "inactive-"+status)
			seedHarnessConfigWithStatus(t, s, "claude", "claude", store.HarnessConfigScopeGlobal, "", status)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
				Name:          "agent-" + status,
				HarnessConfig: "claude",
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), rec.Body.String())
			assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, "claude")
			assert.Contains(t, errResp.Error.Message, status)

			// No agent row must have been persisted.
			listed, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
			require.NoError(t, err)
			assert.Empty(t, listed.Items)
		})
	}

	t.Run("hosted_archived_global_does_not_fall_through_to_active_project", func(t *testing.T) {
		srv, s := testServer(t)
		srv.workstation = false

		project := setupProjectWithBroker(t, s, "no-fallthrough")
		seedHarnessConfigWithStatus(t, s, "custom-hc", "claude", store.HarnessConfigScopeGlobal, "", store.HarnessConfigStatusArchived)
		seedHarnessConfigWithStatus(t, s, "custom-hc", "claude", store.HarnessConfigScopeProject, project.ID, store.HarnessConfigStatusActive)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
			Name:          "agent-no-fallthrough",
			HarnessConfig: "custom-hc",
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), rec.Body.String())
		assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, "custom-hc")
		assert.Contains(t, errResp.Error.Message, store.HarnessConfigStatusArchived)

		listed, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, listed.Items)
	})

	t.Run("absent_keeps_broker_fallback_and_active_succeeds", func(t *testing.T) {
		srv, s := testServer(t)
		srv.workstation = false

		project := setupProjectWithBroker(t, s, "active-absent")
		activeHC := seedHarnessConfigWithStatus(t, s, "active-hc", "claude", store.HarnessConfigScopeGlobal, "", store.HarnessConfigStatusActive)

		// Active harness config succeeds and stamps its ID.
		recActive := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
			Name:          "agent-active",
			HarnessConfig: "active-hc",
		})
		require.Equal(t, http.StatusCreated, recActive.Code, recActive.Body.String())
		var respActive CreateAgentResponse
		require.NoError(t, json.Unmarshal(recActive.Body.Bytes(), &respActive))
		require.NotNil(t, respActive.Agent)
		require.NotNil(t, respActive.Agent.AppliedConfig)
		assert.Equal(t, activeHC.ID, respActive.Agent.AppliedConfig.HarnessConfigID)

		// Absent (deleted) harness config still falls back to broker-local resolution.
		recAbsent := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
			Name:          "agent-absent",
			HarnessConfig: "absent-hc",
		})
		require.Equal(t, http.StatusCreated, recAbsent.Code, recAbsent.Body.String())
		var respAbsent CreateAgentResponse
		require.NoError(t, json.Unmarshal(recAbsent.Body.Bytes(), &respAbsent))
		require.NotNil(t, respAbsent.Agent)
		require.NotNil(t, respAbsent.Agent.AppliedConfig)
		assert.Empty(t, respAbsent.Agent.AppliedConfig.HarnessConfigID)
	})
}

func TestReincarnateAgent_RefusesInactiveHarnessConfig(t *testing.T) {
	ctx := context.Background()

	setupFixture := func(t *testing.T, existingSlug string) (*Server, store.Store, *store.Project, *store.Agent, *reincarnateTestDispatcher) {
		t.Helper()
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		srv.workstation = false
		setExperimentOverrides(t, srv, map[string]bool{experiments.ReincarnateHarnessSwitch: true})

		broker.Capabilities = &store.BrokerCapabilities{
			Reprovision:                  true,
			ReprovisionResetHomeRuntimes: []string{"docker", "podman", "container"},
		}
		require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.Runtime = "docker"
			a.AppliedConfig.HarnessConfig = existingSlug
			a.AppliedConfig.Image = "claude-img:v1"
			a.AppliedConfig.CreateInputs.HarnessConfig = existingSlug
			a.AppliedConfig.CreateInputs.HarnessAuth = "api-key"
			a.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{
				Harness:          "claude",
				HarnessConfig:    existingSlug,
				Image:            "claude-img:v1",
				AuthSelectedType: "api-key",
			}
		})
		return srv, s, project, agent, disp
	}

	for _, status := range []string{store.HarnessConfigStatusArchived, store.HarnessConfigStatusPending} {
		t.Run("plain_reincarnate_refuses_"+status+"_without_fallthrough", func(t *testing.T) {
			srv, s, project, agent, disp := setupFixture(t, "claude-hc")
			seedHarnessConfigWithStatus(t, s, "claude-hc", "claude", store.HarnessConfigScopeGlobal, "", status)
			// Active project row with the same slug must NOT be fallen through to.
			seedHarnessConfigWithStatus(t, s, "claude-hc", "claude", store.HarnessConfigScopeProject, project.ID, store.HarnessConfigStatusActive)

			before := snapshotAgent(t, s, agent.ID)
			for _, dryRun := range []bool{true, false} {
				rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: dryRun})
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

				var errResp ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), rec.Body.String())
				assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
				assert.Contains(t, errResp.Error.Message, "claude-hc")
				assert.Contains(t, errResp.Error.Message, status)
			}
			assertAgentUntouched(t, s, disp, agent.ID, before)
		})

		t.Run("harness_switch_refuses_"+status+"_without_fallthrough", func(t *testing.T) {
			srv, s, project, agent, disp := setupFixture(t, "claude-hc")
			seedHarnessConfigWithStatus(t, s, "claude-hc", "claude", store.HarnessConfigScopeGlobal, "", store.HarnessConfigStatusActive)
			seedHarnessConfigWithStatus(t, s, "codex-hc", "codex", store.HarnessConfigScopeGlobal, "", status)
			// Active project row with the same slug must NOT be fallen through to.
			seedHarnessConfigWithStatus(t, s, "codex-hc", "codex", store.HarnessConfigScopeProject, project.ID, store.HarnessConfigStatusActive)

			before := snapshotAgent(t, s, agent.ID)
			for _, dryRun := range []bool{true, false} {
				rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{
					DryRun:        dryRun,
					HarnessConfig: "codex-hc",
				})
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

				var errResp ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), rec.Body.String())
				assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
				assert.Contains(t, errResp.Error.Message, "codex-hc")
				assert.Contains(t, errResp.Error.Message, status)
			}
			assertAgentUntouched(t, s, disp, agent.ID, before)
		})
	}

	t.Run("active_and_absent_plain_reincarnate_succeed", func(t *testing.T) {
		srv, s, _, agentActive, _ := setupFixture(t, "claude-hc")
		seedHarnessConfigWithStatus(t, s, "claude-hc", "claude", store.HarnessConfigScopeGlobal, "", store.HarnessConfigStatusActive)

		recActive := reincarnateAsDev(t, srv, agentActive.ID, ReincarnateAgentRequest{DryRun: true})
		require.Equal(t, http.StatusOK, recActive.Code, recActive.Body.String())

		// Absent (deleted) harness config keeps broker-local fallback on plain reincarnate.
		srv2, _, _, agentAbsent, _ := setupFixture(t, "deleted-hc")
		recAbsent := reincarnateAsDev(t, srv2, agentAbsent.ID, ReincarnateAgentRequest{DryRun: true})
		require.Equal(t, http.StatusOK, recAbsent.Code, recAbsent.Body.String())
	})
}

func TestResolveHarnessTypeFromConfigRef_IgnoresInactiveNoFallthrough(t *testing.T) {
	ctx := context.Background()

	for _, status := range []string{store.HarnessConfigStatusArchived, store.HarnessConfigStatusPending} {
		t.Run("hosted_"+status+"_global_no_fallthrough", func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = false
			projectID := tid("hc-cap-hosted-" + status)

			seedHarnessConfigWithStatus(t, s, "custom-cfg", "codex", store.HarnessConfigScopeGlobal, "", status)
			seedHarnessConfigWithStatus(t, s, "custom-cfg", "claude", store.HarnessConfigScopeProject, projectID, store.HarnessConfigStatusActive)

			assert.Equal(t, "", srv.resolveHarnessTypeFromConfigRef(ctx, projectID, "custom-cfg"))
			memo := newHarnessTypeMemo()
			assert.Equal(t, "", srv.resolveHarnessTypeFromConfigRefMemo(ctx, projectID, "custom-cfg", memo))
			// Repeat call hits memo with the same result.
			assert.Equal(t, "", srv.resolveHarnessTypeFromConfigRefMemo(ctx, projectID, "custom-cfg", memo))
		})

		t.Run("workstation_"+status+"_project_no_fallthrough", func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = true
			projectID := tid("hc-cap-ws-" + status)

			seedHarnessConfigWithStatus(t, s, "custom-cfg", "claude", store.HarnessConfigScopeProject, projectID, status)
			seedHarnessConfigWithStatus(t, s, "custom-cfg", "codex", store.HarnessConfigScopeGlobal, "", store.HarnessConfigStatusActive)

			assert.Equal(t, "", srv.resolveHarnessTypeFromConfigRef(ctx, projectID, "custom-cfg"))
			memo := newHarnessTypeMemo()
			assert.Equal(t, "", srv.resolveHarnessTypeFromConfigRefMemo(ctx, projectID, "custom-cfg", memo))
		})
	}

	t.Run("absent_global_resolves_active_project_and_active_global_resolves", func(t *testing.T) {
		srv, s := testServer(t)
		srv.workstation = false
		projectID := tid("hc-cap-active")

		seedHarnessConfigWithStatus(t, s, "proj-only-cfg", "claude", store.HarnessConfigScopeProject, projectID, store.HarnessConfigStatusActive)
		seedHarnessConfigWithStatus(t, s, "global-active-cfg", "codex", store.HarnessConfigScopeGlobal, "", store.HarnessConfigStatusActive)

		assert.Equal(t, "claude", srv.resolveHarnessTypeFromConfigRef(ctx, projectID, "proj-only-cfg"))
		assert.Equal(t, "codex", srv.resolveHarnessTypeFromConfigRef(ctx, projectID, "global-active-cfg"))
	})
}
