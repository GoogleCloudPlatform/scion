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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateAgent_CapturesCreateInputs is the Phase 1 regression test for
// design.md §3.3 Amendment A1: the create path must snapshot the explicit
// request inputs into AppliedConfig.CreateInputs, independent of anything
// resolveDerivedConfig later fills in on top of them. `scion reincarnate`
// depends on this snapshot to tell "the requester set this" from "the
// template/hub defaulted it" — a distinction the live AppliedConfig alone
// cannot make once derivation has run.
func TestCreateAgent_CapturesCreateInputs(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Configure a hub-level telemetry default so we can prove it lands on
	// the live InlineConfig but NOT on the CreateInputs snapshot (Amendment
	// A1 point 2: resolveDerivedConfig writes hub defaults into InlineConfig
	// in place).
	srv.mu.Lock()
	enabled := true
	srv.config.TelemetryConfig = &api.TelemetryConfig{Enabled: &enabled}
	srv.mu.Unlock()

	thinkingLevel := 2
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "create-inputs-test",
		ProjectID: project.ID,
		Task:      "do something",
		Profile:   "explicit-profile",
		Branch:    "explicit-branch",
		Workspace: "explicit/workspace",
		Config: &api.ScionConfig{
			Image:            "explicit-image:v1",
			Model:            "explicit-model",
			Env:              map[string]string{"EXPLICIT_KEY": "explicit-value"},
			ThinkingLevel:    &thinkingLevel,
			HarnessConfig:    "explicit-harness-config",
			AuthSelectedType: "api-key",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig)
	require.NotNil(t, persisted.AppliedConfig.CreateInputs, "CreateInputs must be captured at create time")

	ci := persisted.AppliedConfig.CreateInputs
	assert.Equal(t, "explicit-profile", ci.Profile)
	assert.Equal(t, "explicit-branch", ci.Branch)
	assert.Equal(t, "explicit/workspace", ci.Workspace)
	assert.Equal(t, "explicit-harness-config", ci.HarnessConfig)
	assert.Equal(t, "api-key", ci.HarnessAuth)
	require.NotNil(t, ci.ThinkingLevel)
	assert.Equal(t, 2, *ci.ThinkingLevel)

	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, "explicit-image:v1", ci.InlineConfig.Image)
	assert.Equal(t, "explicit-model", ci.InlineConfig.Model)
	assert.Equal(t, "explicit-value", ci.InlineConfig.Env["EXPLICIT_KEY"])

	// The hub telemetry default must have landed on the live InlineConfig...
	require.NotNil(t, persisted.AppliedConfig.InlineConfig)
	require.NotNil(t, persisted.AppliedConfig.InlineConfig.Telemetry,
		"hub telemetry default should be stamped onto the live InlineConfig")

	// ...but the CreateInputs snapshot, captured before that stamping ran,
	// must NOT show it: it was never part of the explicit request.
	assert.Nil(t, ci.InlineConfig.Telemetry,
		"CreateInputs.InlineConfig must not observe hub defaults stamped onto AppliedConfig.InlineConfig after the snapshot was taken")
}

// TestCreateAgent_CreateInputsIndependentOfLaterMutation is a narrower,
// mutation-focused proof that CreateInputs.InlineConfig is a deep copy, not
// an alias of AppliedConfig.InlineConfig: mutating the live config after
// create must never retroactively change the snapshot.
func TestCreateAgent_CreateInputsIndependentOfLaterMutation(t *testing.T) {
	disp := &createAgentDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "create-inputs-mutation-test",
		ProjectID: project.ID,
		Config: &api.ScionConfig{
			Image: "original-image:v1",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig.CreateInputs)
	require.NotNil(t, persisted.AppliedConfig.CreateInputs.InlineConfig)

	// Mutate the live InlineConfig in place, exactly as resolveDerivedConfig
	// would (e.g. stamping a telemetry default).
	persisted.AppliedConfig.InlineConfig.Image = "mutated-image:v2"

	assert.Equal(t, "original-image:v1", persisted.AppliedConfig.CreateInputs.InlineConfig.Image,
		"CreateInputs.InlineConfig must be a deep copy, unaffected by later mutation of the live InlineConfig")
}
