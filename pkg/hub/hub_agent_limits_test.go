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
