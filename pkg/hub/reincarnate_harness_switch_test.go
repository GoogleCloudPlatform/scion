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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for `scion reincarnate --harness` (ptone/scion#7757, Phase 1
// ptone/scion#7758).

type harnessSwitchFixture struct {
	srv     *Server
	s       store.Store
	project *store.Project
	broker  *store.RuntimeBroker
	agent   *store.Agent
	disp    *reincarnateTestDispatcher
	target  *store.HarnessConfig
}

// newHarnessSwitchFixture builds a docker agent on harness config claude-hc
// (with an explicit image, model and harness auth), a codex-hc target
// harness config, a broker that resets homes on docker, and the experiment
// on. mutateBroker may change the broker's capabilities.
func newHarnessSwitchFixture(t *testing.T, mutateBroker func(*store.BrokerCapabilities), mutateTarget func(*store.HarnessConfig)) *harnessSwitchFixture {
	t.Helper()
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setExperimentOverrides(t, srv, map[string]bool{experiments.ReincarnateHarnessSwitch: true})

	broker.Capabilities = &store.BrokerCapabilities{Reprovision: true, ReprovisionResetHomeRuntimes: []string{"docker", "podman", "container"}}
	if mutateBroker != nil {
		mutateBroker(broker.Capabilities)
	}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	for _, hc := range []*store.HarnessConfig{
		{ID: tid("hc-claude-" + t.Name()), Name: "claude-hc", Slug: "claude-hc", Harness: "claude", ContentHash: "claude-hash"},
		{ID: tid("hc-codex-" + t.Name()), Name: "codex-hc", Slug: "codex-hc", Harness: "codex", ContentHash: "codex-hash"},
	} {
		hc.Scope = store.HarnessConfigScopeGlobal
		hc.Status = store.HarnessConfigStatusActive
		if hc.Slug == "codex-hc" && mutateTarget != nil {
			mutateTarget(hc)
		}
		require.NoError(t, s.CreateHarnessConfig(ctx, hc))
	}
	target, err := s.GetHarnessConfigBySlug(ctx, "codex-hc", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Runtime = "docker"
		a.AppliedConfig.HarnessConfig = "claude-hc"
		a.AppliedConfig.Image = "claude-img:v1"
		a.AppliedConfig.Model = "opus"
		a.AppliedConfig.CreateInputs.HarnessConfig = "claude-hc"
		a.AppliedConfig.CreateInputs.HarnessAuth = "api-key"
		a.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{
			Harness: "claude", HarnessConfig: "claude-hc", Image: "claude-img:v1", Model: "opus", AuthSelectedType: "api-key",
		}
	})
	return &harnessSwitchFixture{srv: srv, s: s, project: project, broker: broker, agent: agent, disp: disp, target: target}
}

func decodeReincarnateResponse(t *testing.T, rec *httptest.ResponseRecorder) ReincarnateAgentResponse {
	t.Helper()
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

func requireHarnessSwitchError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	assert.Equal(t, code, body.Error.Code, rec.Body.String())
}

// AC 1: with the experiment off, --harness is a 400 experiment_disabled and
// nothing is persisted or stopped.
func TestReincarnateHarness_ExperimentOff(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, nil)
	setExperimentOverrides(t, f.srv, map[string]bool{})
	before := snapshotAgent(t, f.s, f.agent.ID)
	for _, dryRun := range []bool{true, false} {
		rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "codex-hc"})
		requireHarnessSwitchError(t, rec, http.StatusBadRequest, ErrCodeExperimentDisabled)
		assert.Contains(t, rec.Body.String(), experiments.ReincarnateHarnessSwitch)
	}
	assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)
}

// AC 3: an unknown harness config is a 404, a malformed name and the
// --broker combination are 400s, and --harness-auth is checked against the
// target harness; all before anything is stopped.
func TestReincarnateHarness_RequestRefusals(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, nil)
	before := snapshotAgent(t, f.s, f.agent.ID)
	for _, dryRun := range []bool{true, false} {
		rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "no-such-hc"})
		requireHarnessSwitchError(t, rec, http.StatusNotFound, ErrCodeHarnessConfigNotFound)

		rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "a/b"})
		requireHarnessSwitchError(t, rec, http.StatusBadRequest, ErrCodeValidationError)

		rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "codex-hc", TargetBroker: f.broker.ID})
		requireHarnessSwitchError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
		assert.Contains(t, rec.Body.String(), "targetBroker")

		// codex declares vertex-ai unsupported; claude (the current
		// harness) would accept it.
		rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "codex-hc", HarnessAuth: "vertex-ai"})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "harnessAuth is not supported by harness codex")

		// "none" is accepted only with a harness switch.
		rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessAuth: "none"})
		requireHarnessSwitchError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
	}
	assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)
}

// AC 4: a broker that cannot reset a home on the agent's runtime (an old
// broker, another runtime, an unknown runtime) gets 412, for a dry run too.
func TestReincarnateHarness_ResetCapability412(t *testing.T) {
	cases := map[string]struct {
		caps    func(*store.BrokerCapabilities)
		runtime string
	}{
		"old broker":            {caps: func(c *store.BrokerCapabilities) { c.ReprovisionResetHomeRuntimes = nil }, runtime: "docker"},
		"runtime not listed":    {caps: func(c *store.BrokerCapabilities) { c.ReprovisionResetHomeRuntimes = []string{"podman"} }, runtime: "docker"},
		"cloudrun not admitted": {runtime: "cloudrun"},
		"sandbox refused":       {runtime: "cloudrun-sandbox"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHarnessSwitchFixture(t, tc.caps, nil)
			a, err := f.s.GetAgent(context.Background(), f.agent.ID)
			require.NoError(t, err)
			a.Runtime = tc.runtime
			require.NoError(t, f.s.UpdateAgent(context.Background(), a))
			before := snapshotAgent(t, f.s, f.agent.ID)
			for _, dryRun := range []bool{true, false} {
				rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "codex-hc"})
				requireHarnessSwitchError(t, rec, http.StatusPreconditionFailed, ErrCodeUnsupportedCapability)
			}
			assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)
		})
	}
}

// AC 6: the dry-run plan shows the harness name change, homeReset, the
// home-reset warning first, the harness auth change and the drop warnings
// for the explicit image and model; nothing is written.
func TestReincarnateHarness_DryRunPlan(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, nil)
	before := snapshotAgent(t, f.s, f.agent.ID)
	rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: true, HarnessConfig: "codex-hc"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	plan := decodeReincarnateResponse(t, rec).Plan

	require.NotNil(t, plan.Harness)
	assert.Equal(t, FieldChange{Old: "claude-hc", New: "codex-hc"}, *plan.Harness)
	assert.True(t, plan.HomeReset)
	assert.Equal(t, "codex-hash", plan.HarnessCfg.New)
	require.NotEmpty(t, plan.Warnings)
	assert.True(t, strings.HasPrefix(plan.Warnings[0], "HOME RESET:"), "home-reset warning must be first: %v", plan.Warnings)
	assert.Contains(t, plan.Warnings[0], "codex-hc")
	assert.Contains(t, strings.Join(plan.Warnings, "\n"), "explicit image claude-img:v1 dropped")
	assert.Contains(t, strings.Join(plan.Warnings, "\n"), "explicit model opus dropped")
	require.NotNil(t, plan.HarnessAuth)
	assert.Equal(t, "api-key", plan.HarnessAuth.Old)
	require.NotEmpty(t, plan.Patched)
	assert.Equal(t, "harness", plan.Patched[0])
	assert.NotEqual(t, "claude-img:v1", plan.Image.New)
	assert.Empty(t, plan.Model.New)
	assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)

	// D-f: the same config is a deliberate home reset with the same warning.
	rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: true, HarnessConfig: "claude-hc"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	plan = decodeReincarnateResponse(t, rec).Plan
	assert.True(t, plan.HomeReset)
	assert.Contains(t, plan.Warnings[0], "unchanged")

	// An explicit --image and --model in the same request are kept.
	rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: true, HarnessConfig: "codex-hc", Image: "codex-img:v2", Model: "gpt-x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	plan = decodeReincarnateResponse(t, rec).Plan
	assert.Equal(t, "codex-img:v2", plan.Image.New)
	assert.Equal(t, "gpt-x", plan.Model.New)
	assert.NotContains(t, strings.Join(plan.Warnings, "\n"), "dropped")
}

// AC 7, 9, 10: a real request needs no acknowledgment, answers 202 with the
// dry run's plan, records HomeReset, dispatches a home-resetting reprovision
// under the reincarnation ID, and starts the new generation with the
// home-reset sentence and the handoff. A later plain reincarnation keeps the
// new harness config and does not bring the dropped values back.
func TestReincarnateHarness_RealSwitchAndLaterReincarnation(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, nil)
	dry := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: true, HarnessConfig: "codex-hc", Handoff: "HANDOFF-TEXT"})
	require.Equal(t, http.StatusOK, dry.Code, dry.Body.String())

	rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{HarnessConfig: "codex-hc", Handoff: "HANDOFF-TEXT"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	realPlan, err := json.Marshal(decodeReincarnateResponse(t, rec).Plan)
	require.NoError(t, err)
	dryPlan, err := json.Marshal(decodeReincarnateResponse(t, dry).Plan)
	require.NoError(t, err)
	assert.JSONEq(t, string(dryPlan), string(realPlan), "the 202 carries the dry run's plan")

	r := waitForReincarnationSettled(t, f.s, f.agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	assert.True(t, r.HomeReset)

	f.disp.mu.Lock()
	opts := append([]ReprovisionOptions(nil), f.disp.reprovisionOpts...)
	task := f.disp.lastStartTask
	f.disp.mu.Unlock()
	require.Len(t, opts, 1)
	assert.Equal(t, ReprovisionOptions{ResetHome: true, ResetID: r.ID}, opts[0])
	assert.Contains(t, task, "This generation runs on harness config codex-hc (previously claude-hc).")
	assert.Contains(t, task, "Your home directory was reset")
	assert.Contains(t, task, "HANDOFF-TEXT")

	gen2, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	cfg := gen2.AppliedConfig
	assert.Equal(t, "codex-hc", cfg.HarnessConfig)
	assert.Equal(t, f.target.ID, cfg.HarnessConfigID)
	assert.Equal(t, "codex-hash", cfg.HarnessConfigHash)
	require.NotNil(t, cfg.CreateInputs)
	assert.Equal(t, "codex-hc", cfg.CreateInputs.HarnessConfig)
	assert.Empty(t, cfg.CreateInputs.HarnessAuth)
	require.NotNil(t, cfg.CreateInputs.InlineConfig)
	assert.Equal(t, "codex", cfg.CreateInputs.InlineConfig.Harness)
	assert.Equal(t, "codex-hc", cfg.CreateInputs.InlineConfig.HarnessConfig)
	assert.Empty(t, cfg.CreateInputs.InlineConfig.Image)
	assert.Empty(t, cfg.CreateInputs.InlineConfig.Model)
	assert.Empty(t, cfg.CreateInputs.InlineConfig.AuthSelectedType)
	assert.NotEqual(t, "claude-img:v1", cfg.Image)
	assert.NotEqual(t, "opus", cfg.Model)

	// A later plain reincarnation: no reset, same harness config, no
	// resurrected image, model or auth.
	f.disp.resetReprovisions(nil)
	rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	plain := decodeReincarnateResponse(t, rec).Plan
	assert.False(t, plain.HomeReset)
	assert.Nil(t, plain.Harness)
	waitForGeneration(t, f.s, f.agent.ID, 3)
	gen3, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "codex-hc", gen3.AppliedConfig.HarnessConfig)
	assert.NotEqual(t, "claude-img:v1", gen3.AppliedConfig.Image)
	assert.NotEqual(t, "opus", gen3.AppliedConfig.Model)
	assert.NotEqual(t, "api-key", gen3.AppliedConfig.CreateInputs.HarnessAuth)
	f.disp.mu.Lock()
	defer f.disp.mu.Unlock()
	require.Len(t, f.disp.reprovisionOpts, 1)
	assert.False(t, f.disp.reprovisionOpts[0].ResetHome)
}

// waitForGeneration waits for the agent to settle at generation gen.
func waitForGeneration(t *testing.T, s store.Store, agentID string, gen int) {
	t.Helper()
	waitForReincarnationSettled(t, s, agentID)
	a, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	require.Equal(t, gen, a.Generation)
}

// §3.3.4: a drop-to-shell target with no credentials is refused (422)
// instead of silently switching to NoAuth; --harness-auth none asks for no
// credentials explicitly.
func TestReincarnateHarness_NoAuthFallbackRefused(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, func(hc *store.HarnessConfig) {
		hc.Config = &store.HarnessConfigData{
			NoAuthBehavior: "drop-to-shell",
			AuthMeta: &api.HarnessAuthMetadata{Types: map[string]api.HarnessAuthTypeMetadata{
				"api-key": {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"OPENAI_API_KEY_FOR_TEST_ONLY"}}}},
			}},
		}
	})
	before := snapshotAgent(t, f.s, f.agent.ID)
	for _, dryRun := range []bool{true, false} {
		rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "codex-hc"})
		requireHarnessSwitchError(t, rec, http.StatusUnprocessableEntity, ErrCodeHarnessCredentialsMissing)
		assert.Contains(t, rec.Body.String(), "OPENAI_API_KEY_FOR_TEST_ONLY")
	}
	assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)

	rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: true, HarnessConfig: "codex-hc", HarnessAuth: "none"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	plan := decodeReincarnateResponse(t, rec).Plan
	require.NotNil(t, plan.HarnessAuth)
	assert.Equal(t, "none", plan.HarnessAuth.New)
}

// AC 2 (D-a): --harness is a patch: a self request without lifecycle and a
// user without agent.update are refused, dry run too.
func TestReincarnateHarness_PatchAuthz(t *testing.T) {
	t.Run("self without lifecycle", func(t *testing.T) {
		f := newHarnessSwitchFixture(t, nil, nil)
		self := agentIdentityFor(f.agent.ID, f.project.ID, ScopesForRole(AgentRoleBaseline)...)
		before := snapshotAgent(t, f.s, f.agent.ID)
		for _, dryRun := range []bool{true, false} {
			req := reincarnateRequest(t, f.agent.ID, self, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "codex-hc"})
			rec := httptest.NewRecorder()
			f.srv.handleReincarnateAgent(rec, req, f.agent.ID)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		}
		assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)
	})
	t.Run("user without update", func(t *testing.T) {
		f := newHarnessSwitchFixture(t, nil, nil)
		user := newReincarnateAuthzUser(t, f.s, "harness-no-update")
		grantAgentLifecycleAtProject(t, f.s, user.ID, f.project.ID)
		grantAgentDelegationAtProject(t, f.s, user.ID, f.project.ID)
		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
		before := snapshotAgent(t, f.s, f.agent.ID)
		for _, dryRun := range []bool{true, false} {
			req := reincarnateRequest(t, f.agent.ID, identity, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "codex-hc"})
			rec := httptest.NewRecorder()
			f.srv.handleReincarnateAgent(rec, req, f.agent.ID)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "agent.update")
		}
		assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)
	})
}

// AC 11 (D-b): once the home may have been reset, a failure keeps the row on
// the new harness config with phase error, does not re-render the previous
// config, and names the recovery commands. A failure that certainly left the
// home intact restores the previous config as before.
func TestReincarnateHarness_FailureHandling(t *testing.T) {
	cases := map[string]struct {
		setup       func(d *reincarnateTestDispatcher)
		wantForward bool
		wantCalls   int
	}{
		"start fails after the reset": {
			setup:       func(d *reincarnateTestDispatcher) { d.startErr = definitiveStartErr() },
			wantForward: true, wantCalls: 1,
		},
		"broker answer says the home was reset": {
			setup: func(d *reincarnateTestDispatcher) {
				d.reprovisionErr = provisionSent(brokerHomeResetErr(http.StatusInternalServerError, api.BrokerHomeResetApplied))
			},
			wantForward: true, wantCalls: 1,
		},
		"broker answer without the detail (ambiguous)": {
			setup:       func(d *reincarnateTestDispatcher) { d.reprovisionErr = provisionSent(errors.New("timeout")) },
			wantForward: true, wantCalls: 1,
		},
		"broker 409 refusal with the home intact": {
			setup: func(d *reincarnateTestDispatcher) {
				d.reprovisionErr = provisionSent(brokerHomeResetErr(http.StatusConflict, api.BrokerHomeResetIntact))
			},
			wantForward: false, wantCalls: 2,
		},
		"broker 500 hydration failure with the home intact": {
			setup: func(d *reincarnateTestDispatcher) {
				d.reprovisionErr = provisionSent(brokerHomeResetErr(http.StatusInternalServerError, api.BrokerHomeResetIntact))
			},
			wantForward: false, wantCalls: 2,
		},
		"broker 403 policy refusal with the home intact": {
			setup: func(d *reincarnateTestDispatcher) {
				d.reprovisionErr = provisionSent(brokerHomeResetErr(http.StatusForbidden, api.BrokerHomeResetIntact))
			},
			wantForward: false, wantCalls: 2,
		},
		"reprovision fails before sending": {
			setup:       func(d *reincarnateTestDispatcher) { d.reprovisionErr = errors.New("no endpoint") },
			wantForward: false, wantCalls: 2,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHarnessSwitchFixture(t, nil, nil)
			tc.setup(f.disp)
			rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{HarnessConfig: "codex-hc"})
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			r := waitForReincarnationSettled(t, f.s, f.agent.ID)
			require.Equal(t, store.AgentReincarnationStateFailed, r.State)

			final, err := f.s.GetAgent(context.Background(), f.agent.ID)
			require.NoError(t, err)
			assert.Equal(t, "error", final.Phase)
			assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
			calls, _ := f.disp.reprovisionSnapshot()
			assert.Equal(t, tc.wantCalls, calls, "reprovision calls (the second is the re-render of the previous config)")
			if tc.wantForward {
				assert.Equal(t, "codex-hc", final.AppliedConfig.HarnessConfig, "fail forward keeps the new harness config")
				assert.Contains(t, r.Error, "the home was already reset")
				// Every stage names a plain reincarnate; a start of an
				// error-phase agent is refused (ptone/scion#7757 D2).
				assert.Contains(t, r.Error, fmt.Sprintf("run `scion reincarnate %s`", f.agent.Slug))
				assert.NotContains(t, r.Error, "scion start")
				assert.Contains(t, r.Error, fmt.Sprintf("scion reincarnate %s --harness claude-hc", f.agent.Slug))
				assert.Contains(t, final.Message, "the home was already reset")
			} else {
				assert.Equal(t, "claude-hc", final.AppliedConfig.HarnessConfig, "the previous config is restored")
				assert.NotContains(t, r.Error, "the home was already reset")
			}
		})
	}
}

// brokerHomeResetErr is a broker error answer carrying the home reset
// detail state.
func brokerHomeResetErr(status int, state string) *brokerStatusError {
	return &brokerStatusError{StatusCode: status,
		Body: `{"error":{"code":"x","message":"x","details":{"` + api.BrokerErrDetailHomeReset + `":"` + state + `"}}}`}
}

func TestReprovisionLeftHomeIntact(t *testing.T) {
	plain := &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"conflict","message":"x"}}`}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                     {nil, false},
		"not sent":                {errors.New("x"), true},
		"sent, 409 intact":        {provisionSent(brokerHomeResetErr(http.StatusConflict, api.BrokerHomeResetIntact)), true},
		"sent, 500 intact":        {provisionSent(brokerHomeResetErr(http.StatusInternalServerError, api.BrokerHomeResetIntact)), true},
		"sent, 500 applied":       {provisionSent(brokerHomeResetErr(http.StatusInternalServerError, api.BrokerHomeResetApplied)), false},
		"sent, env still missing": {provisionSent(fmt.Errorf("needs: %w", errReprovisionHomeIntact)), true},
		"sent, no detail":         {provisionSent(plain), false},
		"sent, transport failure": {provisionSent(errors.New("timeout")), false},
		"sent, not JSON":          {provisionSent(&brokerStatusError{StatusCode: http.StatusBadGateway, Body: "<html>"}), false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, reprovisionLeftHomeIntact(tc.err))
		})
	}
}

func TestResetHomeRuntimeRefusal(t *testing.T) {
	// Consume the actual registration advertisement through the hub parser.
	supported := capabilitiesFromStrings(runtimebroker.StaticCapabilityNames(nil)).ReprovisionResetHomeRuntimes
	assert.Empty(t, resetHomeRuntimeRefusal("docker", supported))
	assert.Empty(t, resetHomeRuntimeRefusal("container", supported))
	assert.Empty(t, resetHomeRuntimeRefusal("cloudrun", supported))
	assert.Empty(t, resetHomeRuntimeRefusal("cloudrun-instances", supported))
	assert.NotEmpty(t, resetHomeRuntimeRefusal("cloudrun", nil))
	assert.NotEmpty(t, resetHomeRuntimeRefusal("cloudrun-instances", nil))
	assert.Empty(t, resetHomeRuntimeRefusal("kubernetes", supported))
	assert.Contains(t, resetHomeRuntimeRefusal("kubernetes", nil), "upgrade the broker")
	assert.Contains(t, resetHomeRuntimeRefusal("kubernetes", []string{"docker", "podman", "container", "cloudrun", "cloudrun-instances"}), "upgrade the broker")
	assert.Contains(t, resetHomeRuntimeRefusal("docker", nil), "upgrade the broker")
	assert.Contains(t, resetHomeRuntimeRefusal("substrate", supported), "not supported on runtime substrate")
	assert.NotEmpty(t, resetHomeRuntimeRefusal("", supported))
}

func TestCapabilitiesFromStrings_ResetHomeRuntimes(t *testing.T) {
	caps := capabilitiesFromStrings([]string{"reprovision", "reprovisionResetHome:docker", "reprovisionResetHome:podman", "reprovisionResetHome:cloudrun", "reprovisionResetHome:cloudrun-instances", "reprovisionResetHome:docker", "reprovisionResetHome:"})
	assert.True(t, caps.Reprovision)
	assert.Equal(t, []string{"docker", "podman", "cloudrun", "cloudrun-instances"}, caps.ReprovisionResetHomeRuntimes)
}

// The dispatcher sends resetHome and the reset ID on a home-resetting
// reprovision, and fails closed, marked home-intact, when the broker does
// not confirm the reset (an old broker ignores resetHome).
func TestHTTPAgentDispatcher_DispatchAgentReprovision_ResetHome(t *testing.T) {
	for _, echo := range []bool{true, false} {
		t.Run(fmt.Sprintf("echo=%v", echo), func(t *testing.T) {
			ctx := context.Background()
			memStore := createTestStore(t)
			require.NoError(t, memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: tid("host-reset"), Name: "reset-host", Slug: "reset-host",
				Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
			}))
			var sent *RemoteCreateAgentRequest
			mockClient := &mockRuntimeBrokerClient{
				createWithGatherFunc: func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
					cp := *req
					sent = &cp
					return &RemoteAgentResponse{
						Agent:         &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name},
						Created:       true,
						Reprovisioned: true,
						HomeReset:     echo && req.ResetHome,
					}, nil, nil
				},
			}
			dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
			agent := &store.Agent{
				ID: tid("agent-reset"), Name: "a", Slug: "a", ProjectID: tid("project-reset"), RuntimeBrokerID: tid("host-reset"),
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "codex"},
			}
			err := dispatcher.DispatchAgentReprovision(ctx, agent, ReprovisionOptions{ResetHome: true, ResetID: "reinc-1"})
			require.NotNil(t, sent)
			assert.True(t, sent.ResetHome)
			assert.Equal(t, "reinc-1", sent.ResetHomeID)
			if echo {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, reprovisionLeftHomeIntact(err), "an unconfirmed reset left the home intact: %v", err)
		})
	}
}

// The sweep fails a stale home-resetting record that reached provisioning
// forward: the broker may already have reset the home, so the previous
// config is not restored (D-b). A plain record in the same state is
// restored, as before.
func TestSweepStaleReincarnations_HomeResetFailsForward(t *testing.T) {
	for _, homeReset := range []bool{true, false} {
		t.Run(fmt.Sprintf("homeReset=%v", homeReset), func(t *testing.T) {
			ctx := context.Background()
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, nil)
			previous := *agent.AppliedConfig
			agent.ReincarnationState = store.ReincarnationStateProvisioning
			agent.Phase = "provisioning"
			agent.AppliedConfig.HarnessConfig = "codex-hc"
			require.NoError(t, s.UpdateAgent(ctx, agent))
			require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{
				AgentID: agent.ID, FromGeneration: 1, ToGeneration: 2,
				State:                 store.AgentReincarnationStateProvisioning,
				PreviousAppliedConfig: &previous,
				HomeReset:             homeReset,
			}))
			n, err := srv.sweepStaleReincarnationsOlderThan(ctx, time.Now().Add(time.Hour))
			require.NoError(t, err)
			require.Equal(t, 1, n)
			after, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
			if homeReset {
				assert.Equal(t, "codex-hc", after.AppliedConfig.HarnessConfig, "not restored")
			} else {
				assert.Empty(t, after.AppliedConfig.HarnessConfig, "restored to the previous config")
			}
		})
	}
}

// O3: an ambiguous start after a reset keeps the row on the new config and
// its message carries the home-reset recovery text.
func TestReincarnateHarness_AmbiguousStartNamesRecovery(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, nil)
	f.disp.startErr = errors.New("connection reset by peer")
	rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{HarnessConfig: "codex-hc"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, f.s, f.agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	final, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "codex-hc", final.AppliedConfig.HarnessConfig)
	assert.Contains(t, r.Error, "the home was already reset")
	assert.Contains(t, r.Error, fmt.Sprintf("scion reincarnate %s --harness claude-hc", f.agent.Slug))
	calls, _ := f.disp.reprovisionSnapshot()
	assert.Equal(t, 1, calls, "no re-render after an ambiguous start")
}

// The fail-forward hint names commands the failed agent accepts
// (ptone/scion#7757 D2): for each stage, the hinted retry, a plain
// reincarnate or one with -i replacing a bad recorded image, is accepted
// (202, not 409) and completes on the new harness config, while the
// `scion start` the hint used to name takes the duplicate-name branch.
func TestReincarnateHarness_RecoveryHintCommandAccepted(t *testing.T) {
	cases := map[string]struct {
		fail          func(d *reincarnateTestDispatcher)
		homeComposed  bool
		retry         ReincarnateAgentRequest
		wantImageEdit string
	}{
		"start fails after the reset, plain retry": {
			fail:         func(d *reincarnateTestDispatcher) { d.startErr = definitiveStartErr() },
			homeComposed: true,
			retry:        ReincarnateAgentRequest{},
		},
		"start fails after the reset, retry replacing the image": {
			fail:          func(d *reincarnateTestDispatcher) { d.startErr = definitiveStartErr() },
			homeComposed:  true,
			retry:         ReincarnateAgentRequest{Image: "codex-img:good"},
			wantImageEdit: "codex-img:good",
		},
		"ambiguous start after the reset": {
			fail:         func(d *reincarnateTestDispatcher) { d.startErr = errors.New("connection reset by peer") },
			homeComposed: true,
			retry:        ReincarnateAgentRequest{},
		},
		"reprovision fails after the reset": {
			fail:  func(d *reincarnateTestDispatcher) { d.reprovisionErr = provisionSent(errors.New("timeout")) },
			retry: ReincarnateAgentRequest{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newHarnessSwitchFixture(t, nil, nil)
			f.disp.mu.Lock()
			tc.fail(f.disp)
			f.disp.mu.Unlock()
			rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{HarnessConfig: "codex-hc", Image: "codex-img:bad"})
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			failed := waitForReincarnationSettled(t, f.s, f.agent.ID)
			require.Equal(t, store.AgentReincarnationStateFailed, failed.State)

			slug := f.agent.Slug
			assert.Contains(t, failed.Error, fmt.Sprintf("to retry on harness config codex-hc, fix the cause and run `scion reincarnate %s`", slug))
			assert.Contains(t, failed.Error, "add `-i <image>` or `--model <model>` to replace them")
			assert.Contains(t, failed.Error, fmt.Sprintf("or switch back with `scion reincarnate %s --harness claude-hc`", slug))
			assert.NotContains(t, failed.Error, "scion start")
			assert.Equal(t, !tc.homeComposed, strings.Contains(failed.Error, "the home was already reset and may be incomplete"), failed.Error)

			agent, err := f.s.GetAgent(ctx, f.agent.ID)
			require.NoError(t, err)
			require.Equal(t, "error", agent.Phase)
			require.Equal(t, store.ReincarnationStateFailed, agent.ReincarnationState)
			require.Equal(t, "codex-img:bad", agent.AppliedConfig.Image, "the image edit is recorded and replayed")
			// The command the hint used to name: a create of the existing
			// name without resume is a duplicate-name conflict.
			assert.Equal(t, existingBranchNone, classifyExistingAgent(agent, CreateAgentRequest{Name: slug}))

			// The cause is fixed; run the hinted command.
			f.disp.mu.Lock()
			f.disp.startErr, f.disp.reprovisionErr = nil, nil
			f.disp.mu.Unlock()
			rec = reincarnateAsDev(t, f.srv, f.agent.ID, tc.retry)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			var done *store.AgentReincarnation
			require.Eventually(t, func() bool {
				done = waitForReincarnationSettled(t, f.s, f.agent.ID)
				return done.ID != failed.ID
			}, 5*time.Second, 5*time.Millisecond)
			require.Equal(t, store.AgentReincarnationStateCompleted, done.State, done.Error)

			final, err := f.s.GetAgent(ctx, f.agent.ID)
			require.NoError(t, err)
			assert.NotEqual(t, "error", final.Phase)
			assert.Equal(t, store.ReincarnationStateNone, final.ReincarnationState)
			assert.Equal(t, "codex-hc", final.AppliedConfig.HarnessConfig)
			wantImage := "codex-img:bad"
			if tc.wantImageEdit != "" {
				wantImage = tc.wantImageEdit
			}
			assert.Equal(t, wantImage, final.AppliedConfig.Image)
		})
	}
}

// N2: harness-specific inline settings from a create-time config are
// dropped on a switch, recorded so later generations do not bring them
// back, with a plan warning each.
func TestReincarnateHarness_DropsHarnessSpecificInlineSettings(t *testing.T) {
	f := newHarnessSwitchFixture(t, nil, nil)
	a, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	inline := a.AppliedConfig.CreateInputs.InlineConfig
	inline.ConfigDir = ".claude"
	inline.CommandArgs = []string{"--dangerously-skip-permissions"}
	inline.TaskFlag = "-p"
	inline.User = "claude"
	require.NoError(t, f.s.UpdateAgent(context.Background(), a))

	rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: true, HarnessConfig: "codex-hc"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	warnings := strings.Join(decodeReincarnateResponse(t, rec).Plan.Warnings, "\n")
	for _, key := range []string{"config_dir", "command_args", "task_flag", "user"} {
		assert.Contains(t, warnings, "explicit "+key+" dropped")
	}

	rec = reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{HarnessConfig: "codex-hc"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForGeneration(t, f.s, f.agent.ID, 2)
	gen2, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	got := gen2.AppliedConfig.CreateInputs.InlineConfig
	require.NotNil(t, got)
	assert.Empty(t, got.ConfigDir)
	assert.Empty(t, got.CommandArgs)
	assert.Empty(t, got.TaskFlag)
	assert.Empty(t, got.User)
}

// AC 5: the target harness is checked before either dry-run or real admission.
// Include a partially satisfied requirement to ensure values never reach the error.
func TestReincarnateHarness_CredentialPreflight(t *testing.T) {
	for _, tc := range []struct {
		file bool
		auth string
	}{
		{false, ""}, {false, "api-key"}, {true, ""}, {true, "api-key"},
	} {
		t.Run(fmt.Sprintf("file=%t/auth=%s", tc.file, tc.auth), func(t *testing.T) {
			f := newHarnessSwitchFixture(t, nil, func(hc *store.HarnessConfig) {
				auth := api.HarnessAuthTypeMetadata{RequiredEnv: []api.HarnessAuthEnvRequirement{
					{AnyOf: []string{"TARGET_PRESENT_KEY"}},
					{AnyOf: []string{"TARGET_MISSING_KEY", "TARGET_ALTERNATIVE_KEY"}},
				}}
				if tc.file {
					auth.RequiredEnv = auth.RequiredEnv[:1]
					auth.RequiredFiles = []api.HarnessAuthFileRequirement{{Name: "target-auth-file", Required: true, AlternativeEnvKeys: []string{"TARGET_FILE_KEY"}}}
				}
				hc.Config = &store.HarnessConfigData{AuthMeta: &api.HarnessAuthMetadata{
					DefaultType: "api-key", Types: map[string]api.HarnessAuthTypeMetadata{
						"api-key":     auth,
						"oauth-token": {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"TARGET_OAUTH_KEY"}}}},
					},
				}}
			})
			a, err := f.s.GetAgent(context.Background(), f.agent.ID)
			require.NoError(t, err)
			a.AppliedConfig.CreateInputs.InlineConfig.Env = map[string]string{"TARGET_PRESENT_KEY": "credential-value-must-stay-private"}
			require.NoError(t, f.s.UpdateAgent(context.Background(), a))
			before := snapshotAgent(t, f.s, f.agent.ID)
			var dryBody string
			for _, dryRun := range []bool{true, false} {
				rec := reincarnateAsDev(t, f.srv, f.agent.ID, ReincarnateAgentRequest{DryRun: dryRun, HarnessConfig: "codex-hc", HarnessAuth: tc.auth})
				requireHarnessSwitchError(t, rec, http.StatusUnprocessableEntity, ErrCodeHarnessCredentialsMissing)
				if dryRun {
					dryBody = rec.Body.String()
				} else {
					assert.JSONEq(t, dryBody, rec.Body.String())
				}
				assert.Contains(t, rec.Body.String(), "codex-hc")
				if tc.auth == "" {
					assert.Contains(t, rec.Body.String(), "TARGET_OAUTH_KEY")
				} else {
					assert.NotContains(t, rec.Body.String(), "TARGET_OAUTH_KEY")
				}
				if tc.file {
					assert.Contains(t, rec.Body.String(), "target-auth-file")
					assert.Contains(t, rec.Body.String(), "TARGET_FILE_KEY")
				} else {
					assert.Contains(t, rec.Body.String(), "TARGET_MISSING_KEY")
					assert.Contains(t, rec.Body.String(), "TARGET_ALTERNATIVE_KEY")
				}
				assert.Contains(t, rec.Body.String(), "hub secrets")
				assert.Contains(t, rec.Body.String(), "--harness-auth")
				assert.NotContains(t, rec.Body.String(), "credential-value-must-stay-private")
				assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)
			}
		})
	}
}

func TestReincarnateHarness_CredentialPreflightAllows(t *testing.T) {
	for _, mode := range []string{"credentials", "secret", "none", "no-auth"} {
		t.Run(mode, func(t *testing.T) {
			f := newHarnessSwitchFixture(t, nil, func(hc *store.HarnessConfig) {
				hc.Config = &store.HarnessConfigData{AuthMeta: &api.HarnessAuthMetadata{
					DefaultType: "api-key", Types: map[string]api.HarnessAuthTypeMetadata{
						"api-key": {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"TARGET_REQUIRED_KEY"}}}},
					},
				}}
			})
			a, err := f.s.GetAgent(context.Background(), f.agent.ID)
			require.NoError(t, err)
			req := ReincarnateAgentRequest{HarnessConfig: "codex-hc"}
			switch mode {
			case "credentials":
				a.AppliedConfig.CreateInputs.InlineConfig.Env = map[string]string{"TARGET_REQUIRED_KEY": "private-value"}
			case "secret":
				require.NoError(t, f.s.CreateSecret(context.Background(), &store.Secret{
					ID: tid("target-credential"), Key: "TARGET_REQUIRED_KEY", EncryptedValue: "private-value",
					SecretType: store.SecretTypeEnvironment, Scope: store.ScopeProject, ScopeID: f.project.ID,
					InjectionMode: store.InjectionModeAsNeeded,
				}))
			case "none":
				req.HarnessAuth = "none"
			case "no-auth":
				a.AppliedConfig.CreateInputs.NoAuth = true
				a.AppliedConfig.NoAuth = true
			}
			require.NoError(t, f.s.UpdateAgent(context.Background(), a))
			before := snapshotAgent(t, f.s, f.agent.ID)
			req.DryRun = true
			rec := reincarnateAsDev(t, f.srv, f.agent.ID, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "private-value")
			assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)
			req.DryRun = false
			rec = reincarnateAsDev(t, f.srv, f.agent.ID, req)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			settled := waitForReincarnationSettled(t, f.s, f.agent.ID)
			require.Equal(t, store.AgentReincarnationStateCompleted, settled.State, settled.Error)
		})
	}
}
