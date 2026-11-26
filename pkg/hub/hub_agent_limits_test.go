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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

func setAgentLimitsOpSettings(t *testing.T, srv *Server, experimentOn bool, ceilingsDoc string) *OperationalSettings {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(
		`{"overrides":{%q:%t}}`, experiments.AgentPolicyLimitCeilings, experimentOn,
	)))
	if ceilingsDoc != "" {
		fakeStore.seed(opsettings.AgentLimitsSectionName, json.RawMessage(ceilingsDoc))
	}
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	require.NoError(t, func() error { _, err := ops.Refresh(context.Background()); return err }())
	srv.SetOperationalSettings(ops)
	if hd, ok := srv.GetDispatcher().(*HTTPAgentDispatcher); ok && hd != nil {
		hd.SetAgentLimitsProvider(srv.agentLimitCeilings)
	}
	return ops
}

func TestAgentLimits_OpsettingsCrossSectionValidation(t *testing.T) {
	ctx := context.Background()
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)

	// Set ceiling max_turns=50.
	_, err = ops.Update(ctx, opsettings.AgentLimitsSectionName, json.RawMessage(`{"max_turns":50}`), "admin", -1, "managed")
	require.NoError(t, err)
	assert.Equal(t, 50, ops.AgentLimits().MaxTurns)

	// default_max_turns=80 exceeds ceiling 50 -> refused, names both keys.
	_, err = ops.Update(ctx, "agent_defaults", json.RawMessage(`{"default_max_turns":80}`), "admin", -1, "managed")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSectionValidation), "want ErrSectionValidation, got %v", err)
	assert.Contains(t, err.Error(), "agent_defaults.default_max_turns")
	assert.Contains(t, err.Error(), "agent_limits.max_turns")

	// Explicit 0 (unlimited) default refused while ceiling exists.
	_, err = ops.Update(ctx, "agent_defaults", json.RawMessage(`{"default_max_turns":0}`), "admin", -1, "managed")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSectionValidation))

	// Within ceiling accepted.
	_, err = ops.Update(ctx, "agent_defaults", json.RawMessage(`{"default_max_turns":50}`), "admin", -1, "managed")
	require.NoError(t, err)

	// Tightening ceiling below existing default_max_turns=50 -> refused.
	_, err = ops.Update(ctx, opsettings.AgentLimitsSectionName, json.RawMessage(`{"max_turns":30}`), "admin", -1, "managed")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSectionValidation))
}

type limitCeilingsBrokerClient struct {
	*mockRuntimeBrokerClient
	mu                    sync.Mutex
	lastCreateReq         *RemoteCreateAgentRequest
	lastCreateHubDefaults *RemoteHubAgentDefaults
	lastStartHubDefaults  *RemoteHubAgentDefaults
	templateMaxTurns      *int
}

func (c *limitCeilingsBrokerClient) CreateAgent(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	c.mu.Lock()
	c.lastCreateReq = req
	if req != nil && req.Config != nil {
		c.lastCreateHubDefaults = req.Config.HubAgentDefaults
	} else {
		c.lastCreateHubDefaults = nil
	}
	tplTurns := api.CloneIntPtr(c.templateMaxTurns)
	c.mu.Unlock()

	resp, err := c.mockRuntimeBrokerClient.CreateAgent(ctx, brokerID, endpoint, req)
	if err != nil || resp == nil || resp.Agent == nil {
		return resp, err
	}
	if req != nil {
		resp.Agent.RunID = req.RunID
	}
	c.applySimulatedBrokerClamp(resp.Agent, req.Config.HubAgentDefaults, req.InlineConfig, tplTurns)
	return resp, nil
}

func (c *limitCeilingsBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	resp, err := c.CreateAgent(ctx, brokerID, endpoint, req)
	return resp, nil, err
}

func (c *limitCeilingsBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	c.mu.Lock()
	c.lastStartHubDefaults = extras.HubAgentDefaults
	tplTurns := api.CloneIntPtr(c.templateMaxTurns)
	c.mu.Unlock()

	resp, err := c.mockRuntimeBrokerClient.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume, extras)
	if err != nil || resp == nil || resp.Agent == nil {
		return resp, err
	}
	resp.Agent.RunID = extras.RunID
	c.applySimulatedBrokerClamp(resp.Agent, extras.HubAgentDefaults, inlineConfig, tplTurns)
	return resp, nil
}

// applySimulatedBrokerClamp simulates the real wire round-trip from Hub
// RemoteHubAgentDefaults -> runtimebroker api.HubAgentDefaults ->
// config.FillDefaultLimits + config.ClampLimits -> runtimebroker.AgentInfoToResponse
// -> Hub RemoteAgentInfo.
func (c *limitCeilingsBrokerClient) applySimulatedBrokerClamp(
	agentInfo *RemoteAgentInfo,
	wireDefaults *RemoteHubAgentDefaults,
	inline *api.ScionConfig,
	tplTurns *int,
) {
	var decodedDefaults *api.HubAgentDefaults
	if wireDefaults != nil {
		raw, _ := json.Marshal(wireDefaults)
		var d api.HubAgentDefaults
		_ = json.Unmarshal(raw, &d)
		decodedDefaults = &d
	}

	cfg := &api.ScionConfig{MaxTurns: api.CloneIntPtr(tplTurns)}
	if inline != nil && inline.MaxTurns != nil {
		cfg.MaxTurns = api.CloneIntPtr(inline.MaxTurns)
	}
	config.FillDefaultLimits(cfg, decodedDefaults, 0, 0, "")
	var clamped []string
	if decodedDefaults != nil && !decodedDefaults.LimitCeilings.IsEmpty() {
		clamped = config.ClampLimits(cfg, decodedDefaults.LimitCeilings)
	}

	var effTurns int
	if n, ok := cfg.TurnLimit(); ok {
		effTurns = n
	}
	var relayed []string
	if len(clamped) > 0 {
		relayed = append(relayed, fmt.Sprintf("Run limits were clamped to hub ceiling: max_turns=%d", effTurns))
	}

	brokerResp := runtimebroker.AgentInfoToResponse(api.AgentInfo{
		ID:                agentInfo.ID,
		Name:              agentInfo.Name,
		RunID:             agentInfo.RunID,
		Phase:             string(state.PhaseRunning),
		Harness:           "claude",
		Model:             "test-model",
		TemplateHash:      "sha256:test-tpl",
		ConfigResolved:    true,
		EffectiveMaxTurns: effTurns,
		LimitsClampedBy:   clamped,
		RelayedWarnings:   relayed,
	})
	rawResp, _ := json.Marshal(brokerResp)
	var wireInfo RemoteAgentInfo
	_ = json.Unmarshal(rawResp, &wireInfo)

	agentInfo.Phase = wireInfo.Phase
	agentInfo.Harness = wireInfo.Harness
	agentInfo.Model = wireInfo.Model
	agentInfo.TemplateHash = wireInfo.TemplateHash
	agentInfo.BrokerBuild = wireInfo.BrokerBuild
	agentInfo.MaxTurns = wireInfo.MaxTurns
	agentInfo.MaxModelCalls = wireInfo.MaxModelCalls
	agentInfo.MaxDuration = wireInfo.MaxDuration
	agentInfo.LimitsClampedBy = wireInfo.LimitsClampedBy
	agentInfo.Warnings = wireInfo.Warnings
}

func TestAgentLimits_CreateFailFastAndExperimentGate(t *testing.T) {
	srv, s, project, mock := setupThinkingLevelDispatch(t)
	brokerClient := &limitCeilingsBrokerClient{mockRuntimeBrokerClient: mock}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, brokerClient, false, slog.Default()))

	// 1. Experiment OFF with ceiling 50: behavior identical to today (100 and 0 accepted, no LimitCeilings on wire).
	setAgentLimitsOpSettings(t, srv, false, `{"max_turns":50}`)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", map[string]interface{}{
		"name":   "exp-off-100",
		"config": map[string]interface{}{"max_turns": 100},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NotNil(t, brokerClient.lastCreateReq, "expected broker create request to be captured")
	raw100, err := json.Marshal(brokerClient.lastCreateReq)
	require.NoError(t, err)
	assert.NotContains(t, string(raw100), "limitCeilings", "experiment off must omit limitCeilings on wire")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", map[string]interface{}{
		"name":   "exp-off-0",
		"config": map[string]interface{}{"max_turns": 0},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NotNil(t, brokerClient.lastCreateReq, "expected broker create request to be captured")
	raw0, err := json.Marshal(brokerClient.lastCreateReq)
	require.NoError(t, err)
	assert.NotContains(t, string(raw0), "limitCeilings", "experiment off must omit limitCeilings on wire")

	// 2. Experiment ON with ceiling 50: 100 and 0 refused with 400 limit_exceeds_hub_ceiling.
	setAgentLimitsOpSettings(t, srv, true, `{"max_turns":50}`)

	for _, tc := range []struct {
		name     string
		maxTurns int
	}{
		{"over ceiling 100", 100},
		{"explicit unlimited 0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", map[string]interface{}{
				"name":   "refused-" + strings.ReplaceAll(tc.name, " ", "-"),
				"config": map[string]interface{}{"max_turns": tc.maxTurns},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), ErrCodeLimitExceedsHubCeiling)
		})
	}

	// Within ceiling (50) succeeds and dispatches LimitCeilings.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", map[string]interface{}{
		"name":   "allowed-50",
		"config": map[string]interface{}{"max_turns": 50},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NotNil(t, brokerClient.lastCreateHubDefaults)
	require.NotNil(t, brokerClient.lastCreateHubDefaults.LimitCeilings)
	assert.Equal(t, 50, brokerClient.lastCreateHubDefaults.LimitCeilings.MaxTurns)
}

// TestAgentLimits_EndToEndTemplateUnlimitedClampedWithEchoAndWarning verifies
// the full P4a vertical slice: template max_turns=0 + Hub ceiling 50 ->
// Hub sends LimitCeilings -> broker clamps to 50 and echoes EffectiveConfig ->
// Hub persists EffectiveConfig.MaxTurns=50, LimitsClampedBy=["max_turns"] and
// returns a clamped warning in the create response.
func TestAgentLimits_EndToEndTemplateUnlimitedClampedWithEchoAndWarning(t *testing.T) {
	srv, s, project, mock := setupThinkingLevelDispatch(t)
	brokerClient := &limitCeilingsBrokerClient{
		mockRuntimeBrokerClient: mock,
		templateMaxTurns:        api.IntPtr(0),
	}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, brokerClient, false, slog.Default()))
	setAgentLimitsOpSettings(t, srv, true, `{"max_turns":50}`)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", map[string]interface{}{
		"name": "tpl-unlimited-clamped",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var createResp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Agent)
	require.NotNil(t, brokerClient.lastCreateHubDefaults)
	require.NotNil(t, brokerClient.lastCreateHubDefaults.LimitCeilings)
	assert.Equal(t, 50, brokerClient.lastCreateHubDefaults.LimitCeilings.MaxTurns)

	// Warning surfaced to caller.
	assert.Contains(t, strings.Join(createResp.Warnings, "\n"), "clamped to hub ceiling: max_turns=50",
		"expected clamped warning in create response, got %v", createResp.Warnings)

	// EffectiveConfig persisted on agent row.
	stored, err := s.GetAgent(context.Background(), createResp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.EffectiveConfig, "expected EffectiveConfig to be recorded")
	assert.Equal(t, 50, stored.EffectiveConfig.MaxTurns)
	assert.Equal(t, []string{"max_turns"}, stored.EffectiveConfig.LimitsClampedBy)
}

// TestAgentLimits_StoredOverCeilingClampedOnStart verifies D4: a pre-existing
// agent row with inline max_turns=100 is not refused at start when a ceiling
// of 50 is later enabled; dispatch sends the ceiling, broker clamps to 50, and
// Hub records EffectiveConfig + returns the warning.
func TestAgentLimits_StoredOverCeilingClampedOnStart(t *testing.T) {
	srv, s, project, mock := setupThinkingLevelDispatch(t)
	brokerClient := &limitCeilingsBrokerClient{mockRuntimeBrokerClient: mock}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, brokerClient, false, slog.Default()))

	// Create agent before ceilings are active with max_turns=100.
	setAgentLimitsOpSettings(t, srv, false, "")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", map[string]interface{}{
		"name":   "stored-over-ceiling",
		"config": map[string]interface{}{"max_turns": 100},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createResp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	agentID := createResp.Agent.ID

	// Transition agent to stopped so start dispatches.
	ag, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	ag.Phase = string(state.PhaseStopped)
	require.NoError(t, s.UpdateAgent(context.Background(), ag))

	// Enable experiment + ceiling 50.
	setAgentLimitsOpSettings(t, srv, true, `{"max_turns":50}`)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, brokerClient.lastStartHubDefaults)
	require.NotNil(t, brokerClient.lastStartHubDefaults.LimitCeilings)
	assert.Equal(t, 50, brokerClient.lastStartHubDefaults.LimitCeilings.MaxTurns)

	stored, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	require.NotNil(t, stored.EffectiveConfig)
	assert.Equal(t, 50, stored.EffectiveConfig.MaxTurns)
	assert.Equal(t, []string{"max_turns"}, stored.EffectiveConfig.LimitsClampedBy)
}

// TestPutServerConfigDB_AgentDefaultsExceedsAgentLimitsCeiling_422 pins the
// PUT /api/v1/admin/server-config validation check against stored agent_limits
// ceilings: over-ceiling default_max_turns returns HTTP 422 validation_error
// naming both keys without writing agent_defaults.
func TestPutServerConfigDB_AgentDefaultsExceedsAgentLimitsCeiling_422(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)
	ctx := context.Background()

	_, err := ops.Update(ctx, opsettings.AgentLimitsSectionName, json.RawMessage(`{"max_turns":50}`), "admin", -1, "managed")
	require.NoError(t, err)

	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"default_max_turns":80}`)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)

	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "validation_error")
	assert.Contains(t, rr.Body.String(), "agent_defaults.default_max_turns")
	assert.Contains(t, rr.Body.String(), "agent_limits.max_turns")
	_, lookupErr := fakeStore.GetHubSetting(ctx, "agent_defaults")
	assert.Error(t, lookupErr, "refused PUT must not write agent_defaults")

	// Within ceiling succeeds.
	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"default_max_turns":50}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Reverse order: tightening agent_limits below the saved default_max_turns is refused.
	_, err = ops.Update(ctx, opsettings.AgentLimitsSectionName, json.RawMessage(`{"max_turns":30}`), "admin", -1, "managed")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSectionValidation), "want ErrSectionValidation, got %v", err)
	assert.Contains(t, err.Error(), "agent_defaults.default_max_turns")
	assert.Contains(t, err.Error(), "agent_limits.max_turns")
	assert.Equal(t, 50, ops.AgentLimits().MaxTurns, "refused update must leave stored ceiling unchanged")
}

// TestPutServerConfigDB_AgentLimitsMultiSectionAtomicity pins:
//  1. Sending agent_limits on PUT /api/v1/admin/server-config is refused with
//     412 experiment_disabled when agent_policy.limit_ceilings is off (while
//     echoing stored values or clearing passes), and persists cleanly when on.
//  2. Unknown nested fields under agent_limits are refused with 422
//     unpersisted_keys_rejected, and invalid durations return 422 validation_error.
//  3. A multi-section PUT carrying an earlier alphabetical section ("access")
//     plus an invalid agent_defaults value (default_max_turns > ceiling)
//     fails upfront with 422 and writes neither section (no partial write).
//  4. Simultaneous valid updates to agent_defaults + agent_limits succeed
//     atomically, and lowering agent_limits below stored agent_defaults fails
//     with 422.
func TestPutServerConfigDB_AgentLimitsMultiSectionAtomicity(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)
	ctx := context.Background()

	_, err := ops.Update(ctx, opsettings.AgentLimitsSectionName, json.RawMessage(`{"max_turns":50}`), "admin", -1, "managed")
	require.NoError(t, err)

	// (1a) Experiment OFF: changing agent_limits to a new non-empty value is
	// refused with 412 experiment_disabled.
	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"agent_limits":{"max_turns":40}}`)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusPreconditionFailed, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeExperimentDisabled)
	assert.Equal(t, 50, ops.AgentLimits().MaxTurns, "rejected PUT must not change agent_limits")

	// (1b) Experiment OFF: echoing the stored agent_limits value passes.
	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"agent_limits":{"max_turns":50}}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// (2) Enable experiment: unknown nested key returns 422 unpersisted_keys_rejected,
	// invalid duration returns 422 validation_error, valid values persist and round-trip via GET.
	_, err = ops.Update(ctx, "experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.AgentPolicyLimitCeilings)), "admin", -1, "managed")
	require.NoError(t, err)

	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"agent_limits":{"max_turns":40,"bogus":1}}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "unpersisted_keys_rejected")

	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"agent_limits":{"max_duration":"not-a-duration"}}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "validation_error")

	// Multi-section PUT where "access" sorts before "agent_defaults" and
	// agent_defaults exceeds the ceiling: 422 upfront, nothing written.
	multiBody := `{"server":{"hub":{"admin_emails":["new@example.com"]}},"default_max_turns":80}`
	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", multiBody)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "validation_error")
	assert.Contains(t, rr.Body.String(), "agent_defaults.default_max_turns")
	assert.Contains(t, rr.Body.String(), "agent_limits.max_turns")
	_, accessErr := fakeStore.GetHubSetting(ctx, "access")
	assert.Error(t, accessErr, "earlier section 'access' must not be written when later 'agent_defaults' fails validation")
	_, defaultsErr := fakeStore.GetHubSetting(ctx, "agent_defaults")
	assert.Error(t, defaultsErr, "invalid 'agent_defaults' must not be written")

	// Simultaneous raise of ceiling + default in one PUT succeeds and GET returns agent_limits.
	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"default_max_turns":80,"agent_limits":{"max_turns":100,"max_model_calls":250,"max_duration":"2h"}}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	getReq := adminRequest(http.MethodGet, "/api/v1/admin/server-config", "")
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRR, getReq, ops)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	var getResp ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &getResp))
	require.NotNil(t, getResp.AgentLimits)
	assert.Equal(t, 100, getResp.AgentLimits.MaxTurns)
	assert.Equal(t, 250, getResp.AgentLimits.MaxModelCalls)
	assert.Equal(t, "2h", getResp.AgentLimits.MaxDuration)

	// Lowering ceiling below stored default_max_turns=80 is refused with 422.
	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"agent_limits":{"max_turns":60}}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "validation_error")
	assert.Equal(t, 100, ops.AgentLimits().MaxTurns)

	// Clearing ceilings via empty object succeeds, and clearing via null
	// (the UI full-clear payload) also succeeds.
	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"agent_limits":{}}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.True(t, ops.AgentLimits().IsEmpty())

	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"agent_limits":{"max_turns":100}}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, 100, ops.AgentLimits().MaxTurns)

	req = adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"agent_limits":null}`)
	rr = httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.True(t, ops.AgentLimits().IsEmpty())

	// Direct validator checks for simultaneous and single-section cases.
	t.Run("simultaneous lower ceiling below incoming default refused", func(t *testing.T) {
		errs := make(map[string][]config.ValidationError)
		validateAgentLimitsSectionDocs(ops, map[string]json.RawMessage{
			"agent_defaults":                  json.RawMessage(`{"default_max_turns":40}`),
			opsettings.AgentLimitsSectionName: json.RawMessage(`{"max_turns":30}`),
		}, errs)
		require.Len(t, errs["agent_defaults"], 1)
		assert.Contains(t, errs["agent_defaults"][0].Message, "agent_defaults.default_max_turns")
		assert.Contains(t, errs["agent_defaults"][0].Message, "agent_limits.max_turns")
	})
}

// TestResolvedSettings_IncludesLimitCeilingsWhenExperimentEnabled verifies
// GET /api/v1/projects/{id}/settings/resolved exposes ceiling per limit when
// agent_policy.limit_ceilings is enabled, and null when off or unset.
func TestResolvedSettings_IncludesLimitCeilingsWhenExperimentEnabled(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	// Experiment OFF: ceilings are null even when stored in opsettings.
	setAgentLimitsOpSettings(t, srv, false, `{"max_turns":50,"max_model_calls":200,"max_duration":"2h"}`)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings/resolved", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var offResp ResolvedProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &offResp))
	assert.Nil(t, offResp.Settings[projectSettingDefaultMaxTurns].Ceiling)
	assert.Nil(t, offResp.Settings[projectSettingDefaultMaxModelCalls].Ceiling)
	assert.Nil(t, offResp.Settings[projectSettingDefaultMaxDuration].Ceiling)

	// Experiment ON: ceilings are populated for the three limit keys only.
	setAgentLimitsOpSettings(t, srv, true, `{"max_turns":50,"max_model_calls":200,"max_duration":"2h"}`)
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings/resolved", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var onResp ResolvedProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &onResp))
	assert.Equal(t, float64(50), onResp.Settings[projectSettingDefaultMaxTurns].Ceiling)
	assert.Equal(t, float64(200), onResp.Settings[projectSettingDefaultMaxModelCalls].Ceiling)
	assert.Equal(t, "2h", onResp.Settings[projectSettingDefaultMaxDuration].Ceiling)
	assert.Nil(t, onResp.Settings[projectSettingDefaultModel].Ceiling)
}

// TestGetAgent_IncludesEffectiveConfigLimits verifies GET /api/v1/agents/{id}
// projects EffectiveConfig (maxTurns, maxModelCalls, maxDuration, limitsClampedBy)
// onto the single-agent response when recorded.
func TestGetAgent_IncludesEffectiveConfigLimits(t *testing.T) {
	srv, s, project, mock := setupThinkingLevelDispatch(t)
	brokerClient := &limitCeilingsBrokerClient{
		mockRuntimeBrokerClient: mock,
		templateMaxTurns:        api.IntPtr(0),
	}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, brokerClient, false, slog.Default()))
	setAgentLimitsOpSettings(t, srv, true, `{"max_turns":50}`)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", map[string]interface{}{
		"name": "effective-limits-get",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createResp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Agent)

	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+createResp.Agent.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var got AgentWithCapabilities
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &got))
	require.NotNil(t, got.EffectiveConfig)
	assert.Equal(t, 50, got.EffectiveConfig.MaxTurns)
	assert.Equal(t, []string{"max_turns"}, got.EffectiveConfig.LimitsClampedBy)
}
