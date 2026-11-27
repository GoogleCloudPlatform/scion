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
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestUsageReadAPI_UsageYAMLMeasurementStatus_PerAgentAndScoped(t *testing.T) {
	srv, s := testServer(t)
	srv.SetHubID("hub-usage-cap")
	setUsageLedgerExperimentForTest(t, srv, true)
	ctx := context.Background()

	admin := &store.User{
		ID:          uuid.NewString(),
		Email:       "usage-cap-admin@example.com",
		DisplayName: "Usage Cap Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)

	proj := &store.Project{
		ID:      uuid.NewString(),
		Slug:    "usage-cap-proj",
		Name:    "Usage Cap Proj",
		OwnerID: admin.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, admin.ID, proj.ID, store.ProjectRoleOwner)

	// Seed global HarnessConfigs with usage.yaml capabilities.
	seedHC := func(slug, scope, scopeID, harness string, cap *api.HarnessUsageCapabilities) {
		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID:      uuid.NewString(),
			Name:    slug,
			Slug:    slug,
			Harness: harness,
			Scope:   scope,
			ScopeID: scopeID,
			Status:  store.HarnessConfigStatusActive,
			Config: &store.HarnessConfigData{
				Harness: harness,
				Usage:   cap,
			},
		}))
	}
	seedHC("claude-live", store.HarnessConfigScopeGlobal, "", "claude",
		makeUsageCap(api.UsageLiveSourceNative, []string{"input", "output", "cache_read", "cache_write"}, nil, true))
	seedHC("agy-calls", store.HarnessConfigScopeGlobal, "", "antigravity",
		makeUsageCap(api.UsageLiveSourceHooks, nil, nil, false))
	seedHC("hermes-none", store.HarnessConfigScopeGlobal, "", "hermes",
		makeUsageCap(api.UsageLiveSourceNone, nil, nil, false))
	seedHC("custom-partial", store.HarnessConfigScopeGlobal, "", "custom",
		makeUsageCap(api.UsageLiveSourceHooks, []string{"input", "output"}, nil, false))

	dbNow, err := s.DBNow(ctx)
	require.NoError(t, err)

	seedAgent := func(slug, runID, hcSlug, harness string, createdAt time.Time) *store.Agent {
		ag := &store.Agent{
			ID:            uuid.NewString(),
			Slug:          slug,
			Name:          slug,
			ProjectID:     proj.ID,
			OwnerID:       admin.ID,
			HarnessConfig: hcSlug,
			Phase:         string(state.PhaseRunning),
		}
		require.NoError(t, s.CreateAgent(ctx, ag))
		_, err := s.SetAgentRunID(ctx, ag.ID, runID, nil)
		require.NoError(t, err)
		ag.RunID = runID
		_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
			RunID:         runID,
			Generation:    1,
			ProjectID:     proj.ID,
			AgentID:       ag.ID,
			BillingUserID: &admin.ID,
			Harness:       harness,
			HarnessConfig: hcSlug,
			CreatedAt:     createdAt,
		})
		require.NoError(t, err)
		return ag
	}

	// 1. P1 deferral (b): claude-live running with heartbeat-only fresh meter -> measured.
	agHeartbeat := seedAgent("ag-hb", "run-hb", "claude-live", "claude", dbNow.Add(-5*time.Minute))
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agHeartbeat.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agHeartbeat.ID,
			RunID:         agHeartbeat.RunID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow.Add(-15 * time.Second),
			ProducerSeq:   1,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// 2. antigravity calls_only with fresh heartbeat -> calls_only.
	agCalls := seedAgent("ag-calls", "run-calls", "agy-calls", "antigravity", dbNow.Add(-5*time.Minute))
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agCalls.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agCalls.ID,
			RunID:         agCalls.RunID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow.Add(-15 * time.Second),
			ProducerSeq:   1,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// 3. hermes source=none running past grace with no receipt -> unmeasured (NOT stale).
	agNone := seedAgent("ag-none", "run-none", "hermes-none", "hermes", dbNow.Add(-5*time.Minute))

	// 4. custom-partial with fresh heartbeat -> partial.
	agPartial := seedAgent("ag-partial", "run-partial", "custom-partial", "custom", dbNow.Add(-5*time.Minute))
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agPartial.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agPartial.ID,
			RunID:         agPartial.RunID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow.Add(-15 * time.Second),
			ProducerSeq:   1,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// 5. P1 deferral (a): claude-live running past 60s grace with NO receipt -> stale.
	agStaleGrace := seedAgent("ag-stale-grace", "run-stale-grace", "claude-live", "claude", dbNow.Add(-2*time.Minute))

	// Check per-agent GET /api/v1/agents/{id}/usage responses.
	checkAgentStatus := func(agentID, wantStatus string) {
		t.Helper()
		rec := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+agentID+"/usage", admin)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp AgentUsageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, wantStatus, resp.MeasurementStatus, "agent %s", agentID)
	}

	checkAgentStatus(agHeartbeat.ID, MeasurementStatusMeasured)
	checkAgentStatus(agCalls.ID, MeasurementStatusCallsOnly)
	checkAgentStatus(agNone.ID, MeasurementStatusUnmeasured)
	checkAgentStatus(agPartial.ID, MeasurementStatusPartial)
	checkAgentStatus(agStaleGrace.ID, MeasurementStatusStale)

	// Check scoped GET /api/v1/projects/{id}/usage -> stale (1 stale from agStaleGrace, 1 unmeasured from agNone).
	recProj := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since=24h", admin)
	require.Equal(t, http.StatusOK, recProj.Code, recProj.Body.String())
	var projResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recProj.Body.Bytes(), &projResp))
	assert.Equal(t, MeasurementStatusStale, projResp.MeasurementStatus)
	assert.Equal(t, 1, projResp.StaleAgents)
	assert.Equal(t, 1, projResp.UnmeasuredAgents)

	// Check scoped GET /api/v1/me/usage -> same counts for admin's billing user.
	recMe := doUserUsageReadRequest(t, srv, "/api/v1/me/usage?since=24h", admin)
	require.Equal(t, http.StatusOK, recMe.Code, recMe.Body.String())
	var meResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recMe.Body.Bytes(), &meResp))
	assert.Equal(t, MeasurementStatusStale, meResp.MeasurementStatus)
	assert.Equal(t, 1, meResp.StaleAgents)
	assert.Equal(t, 1, meResp.UnmeasuredAgents)

	// Check scoped GET /api/v1/usage (global) with agent_id filter targeting agPartial -> partial, 0 stale, 0 unmeasured.
	recGlobalPartial := doUserUsageReadRequest(t, srv, "/api/v1/usage?since=24h&agent_id="+agPartial.ID, admin)
	require.Equal(t, http.StatusOK, recGlobalPartial.Code, recGlobalPartial.Body.String())
	var globalPartialResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recGlobalPartial.Body.Bytes(), &globalPartialResp))
	assert.Equal(t, MeasurementStatusPartial, globalPartialResp.MeasurementStatus)
	assert.Equal(t, 0, globalPartialResp.StaleAgents)
	assert.Equal(t, 0, globalPartialResp.UnmeasuredAgents)
}

func TestUsageReadAPI_CapabilityScopeOrderHostedVsWorkstationAndArchived(t *testing.T) {
	srv, s := testServer(t)
	srv.SetHubID("hub-usage-scope-order")
	setUsageLedgerExperimentForTest(t, srv, true)
	ctx := context.Background()

	admin := &store.User{
		ID:          uuid.NewString(),
		Email:       "usage-scope-order-admin@example.com",
		DisplayName: "Usage Scope Order Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)

	proj := &store.Project{
		ID:      uuid.NewString(),
		Slug:    "usage-scope-order-proj",
		Name:    "Usage Scope Order Proj",
		OwnerID: admin.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, admin.ID, proj.ID, store.ProjectRoleOwner)

	// Same slug "shared-cfg" in BOTH global (native full) and project (source: none).
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "shared-cfg",
		Slug:    "shared-cfg",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeGlobal,
		ScopeID: "",
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Harness: "claude",
			Usage:   makeUsageCap(api.UsageLiveSourceNative, []string{"input", "output", "cache_read", "cache_write"}, nil, true),
		},
	}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "shared-cfg",
		Slug:    "shared-cfg",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: proj.ID,
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Harness: "claude",
			Usage:   makeUsageCap(api.UsageLiveSourceNone, nil, nil, false),
		},
	}))

	// Archived project-scoped config (hooks, calls_only) with no status filter parity.
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "archived-cfg",
		Slug:    "archived-cfg",
		Harness: "antigravity",
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: proj.ID,
		Status:  store.HarnessConfigStatusArchived,
		Config: &store.HarnessConfigData{
			Harness: "antigravity",
			Usage:   makeUsageCap(api.UsageLiveSourceHooks, nil, nil, false),
		},
	}))

	dbNow, err := s.DBNow(ctx)
	require.NoError(t, err)

	seedRunningWithHeartbeat := func(slug, runID, hcSlug, harness string) *store.Agent {
		ag := &store.Agent{
			ID:            uuid.NewString(),
			Slug:          slug,
			Name:          slug,
			ProjectID:     proj.ID,
			OwnerID:       admin.ID,
			HarnessConfig: hcSlug,
			Phase:         string(state.PhaseRunning),
		}
		require.NoError(t, s.CreateAgent(ctx, ag))
		_, err := s.SetAgentRunID(ctx, ag.ID, runID, nil)
		require.NoError(t, err)
		ag.RunID = runID
		_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
			RunID:         runID,
			Generation:    1,
			ProjectID:     proj.ID,
			AgentID:       ag.ID,
			BillingUserID: &admin.ID,
			Harness:       harness,
			HarnessConfig: hcSlug,
			CreatedAt:     dbNow.Add(-5 * time.Minute),
		})
		require.NoError(t, err)
		_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
			AgentID: ag.ID,
			MeterHealth: &store.UsageMeterHealthUpdate{
				AgentID:       ag.ID,
				RunID:         runID,
				ProducerEpoch: "ep-1",
				LastReceiptAt: dbNow.Add(-15 * time.Second),
				ProducerSeq:   1,
				FinalState:    store.MeterHealthFinalStateOpen,
			},
		})
		require.NoError(t, err)
		return ag
	}

	agShared := seedRunningWithHeartbeat("ag-shared", "run-shared", "shared-cfg", "claude")
	agArchived := seedRunningWithHeartbeat("ag-archived", "run-archived", "archived-cfg", "antigravity")

	getAgentStatus := func(agentID string) string {
		t.Helper()
		rec := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+agentID+"/usage", admin)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp AgentUsageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.MeasurementStatus
	}
	getScopedStatus := func(agentID string) string {
		t.Helper()
		rec := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since=24h&agent_id="+agentID, admin)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp UsageSummaryResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.MeasurementStatus
	}

	// 1. Hosted hub (workstation=false): global config wins -> measured on BOTH per-agent and scoped.
	srv.workstation = false
	assert.Equal(t, MeasurementStatusMeasured, getAgentStatus(agShared.ID), "hosted per-agent must prefer global config")
	assert.Equal(t, MeasurementStatusMeasured, getScopedStatus(agShared.ID), "hosted scoped must prefer global config")

	// 2. Workstation hub (workstation=true): project config wins -> unmeasured on BOTH per-agent and scoped.
	srv.workstation = true
	assert.Equal(t, MeasurementStatusUnmeasured, getAgentStatus(agShared.ID), "workstation per-agent must prefer project config")
	assert.Equal(t, MeasurementStatusUnmeasured, getScopedStatus(agShared.ID), "workstation scoped must prefer project config")

	// 3. Archived project config: per-agent and scoped must agree (calls_only, no status filter).
	srv.workstation = false
	assert.Equal(t, MeasurementStatusCallsOnly, getAgentStatus(agArchived.ID), "per-agent must resolve archived config matching dispatch")
	assert.Equal(t, MeasurementStatusCallsOnly, getScopedStatus(agArchived.ID), "scoped must resolve archived config matching dispatch")
}

func TestUsageReadAPI_ShadowedConfigWithoutUsageYAML(t *testing.T) {
	srv, s := testServer(t)
	srv.SetHubID("hub-usage-shadow")
	setUsageLedgerExperimentForTest(t, srv, true)
	ctx := context.Background()

	admin := &store.User{
		ID:          uuid.NewString(),
		Email:       "usage-shadow-admin@example.com",
		DisplayName: "Usage Shadow Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)

	proj := &store.Project{
		ID:      uuid.NewString(),
		Slug:    "usage-shadow-proj",
		Name:    "Usage Shadow Proj",
		OwnerID: admin.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, admin.ID, proj.ID, store.ProjectRoleOwner)

	// Hosted case: global "hosted-shadow" has NO usage.yaml (dispatch winner);
	// project-scoped "hosted-shadow" declares hooks/calls_only.
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "hosted-shadow",
		Slug:    "hosted-shadow",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeGlobal,
		ScopeID: "",
		Status:  store.HarnessConfigStatusActive,
		Config:  &store.HarnessConfigData{Harness: "claude"},
	}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "hosted-shadow",
		Slug:    "hosted-shadow",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: proj.ID,
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Harness: "claude",
			Usage:   makeUsageCap(api.UsageLiveSourceHooks, nil, nil, false),
		},
	}))

	// Workstation mirror: project "ws-shadow" has NO usage.yaml (dispatch winner);
	// global "ws-shadow" declares native full (which would turn past-grace no-receipt into stale).
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "ws-shadow",
		Slug:    "ws-shadow",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: proj.ID,
		Status:  store.HarnessConfigStatusActive,
		Config:  &store.HarnessConfigData{Harness: "claude"},
	}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "ws-shadow",
		Slug:    "ws-shadow",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeGlobal,
		ScopeID: "",
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Harness: "claude",
			Usage:   makeUsageCap(api.UsageLiveSourceNative, []string{"input", "output", "cache_read", "cache_write"}, nil, true),
		},
	}))

	dbNow, err := s.DBNow(ctx)
	require.NoError(t, err)

	// Hosted agent: running with fresh heartbeat-only meter on hosted-shadow.
	agHosted := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "ag-hosted-shadow",
		Name:          "ag-hosted-shadow",
		ProjectID:     proj.ID,
		OwnerID:       admin.ID,
		HarnessConfig: "hosted-shadow",
		Phase:         string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agHosted))
	_, err = s.SetAgentRunID(ctx, agHosted.ID, "run-hosted-shadow", nil)
	require.NoError(t, err)
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         "run-hosted-shadow",
		Generation:    1,
		ProjectID:     proj.ID,
		AgentID:       agHosted.ID,
		BillingUserID: &admin.ID,
		Harness:       "claude",
		HarnessConfig: "hosted-shadow",
		CreatedAt:     dbNow.Add(-5 * time.Minute),
	})
	require.NoError(t, err)
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agHosted.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agHosted.ID,
			RunID:         "run-hosted-shadow",
			ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow.Add(-15 * time.Second),
			ProducerSeq:   1,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// Workstation agent: running past 60s grace with NO receipt on ws-shadow.
	agWS := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "ag-ws-shadow",
		Name:          "ag-ws-shadow",
		ProjectID:     proj.ID,
		OwnerID:       admin.ID,
		HarnessConfig: "ws-shadow",
		Phase:         string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agWS))
	_, err = s.SetAgentRunID(ctx, agWS.ID, "run-ws-shadow", nil)
	require.NoError(t, err)
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         "run-ws-shadow",
		Generation:    1,
		ProjectID:     proj.ID,
		AgentID:       agWS.ID,
		BillingUserID: &admin.ID,
		Harness:       "claude",
		HarnessConfig: "ws-shadow",
		CreatedAt:     dbNow.Add(-5 * time.Minute),
	})
	require.NoError(t, err)

	getAgentStatus := func(agentID string) string {
		t.Helper()
		rec := doUserUsageReadRequest(t, srv, "/api/v1/agents/"+agentID+"/usage", admin)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp AgentUsageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.MeasurementStatus
	}
	getScopedStatus := func(agentID string) string {
		t.Helper()
		rec := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since=24h&agent_id="+agentID, admin)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp UsageSummaryResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.MeasurementStatus
	}

	// Hosted: global winner has no usage.yaml -> unknown capability -> heartbeat-only reads unmeasured (NOT calls_only).
	srv.workstation = false
	assert.Equal(t, MeasurementStatusUnmeasured, getAgentStatus(agHosted.ID), "hosted per-agent must not fall through to shadowed project config")
	assert.Equal(t, MeasurementStatusUnmeasured, getScopedStatus(agHosted.ID), "hosted scoped must not fall through to shadowed project config")

	// Workstation: project winner has no usage.yaml -> unknown capability -> past-grace no-receipt reads unmeasured (NOT stale).
	srv.workstation = true
	assert.Equal(t, MeasurementStatusUnmeasured, getAgentStatus(agWS.ID), "workstation per-agent must not fall through to shadowed global config")
	assert.Equal(t, MeasurementStatusUnmeasured, getScopedStatus(agWS.ID), "workstation scoped must not fall through to shadowed global config")
}
