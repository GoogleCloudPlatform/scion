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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReincarnate_PinsLiveHarnessConfigWhenUnsetInCreateInputs verifies that
// an agent created without an explicit --harness stays on its live harness
// config when the hub default later changes, and that a real reincarnation
// pins CreateInputs.HarnessConfig without mutating PreviousAppliedConfig
// (ptone/scion#7918).
func TestReincarnate_PinsLiveHarnessConfigWhenUnsetInCreateInputs(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	for _, hc := range []*store.HarnessConfig{
		{ID: tid("hc-codex-pin-" + t.Name()), Name: "codex-hc", Slug: "codex-hc", Harness: "codex", ContentHash: "codex-hash"},
		{ID: tid("hc-claude-pin-" + t.Name()), Name: "claude-hc", Slug: "claude-hc", Harness: "claude", ContentHash: "claude-hash"},
	} {
		hc.Scope = store.HarnessConfigScopeGlobal
		hc.Status = store.HarnessConfigStatusActive
		require.NoError(t, s.CreateHarnessConfig(ctx, hc))
	}

	// Agent was created when the default was codex-hc, without --harness.
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.HarnessConfig = "codex-hc"
		a.AppliedConfig.HarnessConfigHash = "codex-hash"
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	// Hub default later changes to claude-hc.
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "claude-hc"})

	// Plain dry-run keeps codex-hc unchanged, with no Harness switch line.
	dryRec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true})
	require.Equal(t, http.StatusOK, dryRec.Code, dryRec.Body.String())
	dryPlan := decodeReincarnateResponse(t, dryRec).Plan
	assert.Nil(t, dryPlan.Harness)
	assert.False(t, dryPlan.HomeReset)
	assert.Equal(t, "codex-hash", dryPlan.HarnessCfg.Old)
	assert.Equal(t, "codex-hash", dryPlan.HarnessCfg.New)

	// Real reincarnation persists the pin into gen 2's CreateInputs without
	// mutating the outgoing generation's CreateInputs in PreviousAppliedConfig.
	realRec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{})
	require.Equal(t, http.StatusAccepted, realRec.Code, realRec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

	gen2, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, gen2.AppliedConfig)
	assert.Equal(t, "codex-hc", gen2.AppliedConfig.HarnessConfig)
	require.NotNil(t, gen2.AppliedConfig.CreateInputs)
	assert.Equal(t, "codex-hc", gen2.AppliedConfig.CreateInputs.HarnessConfig)

	require.NotNil(t, r.PreviousAppliedConfig)
	require.NotNil(t, r.PreviousAppliedConfig.CreateInputs)
	assert.Empty(t, r.PreviousAppliedConfig.CreateInputs.HarnessConfig,
		"PreviousAppliedConfig.CreateInputs must not be mutated")
}

// TestReincarnate_ImplicitHarnessSwitchRefusedInPlace verifies that if the
// pinned harness config cannot be used (e.g. dropped by
// dropUnusableDefaultHarnessConfig) and derivation falls back to another
// harness config, both dry-run and real in-place requests are refused with
// 412 ErrCodeImplicitHarnessSwitch and leave the agent untouched.
func TestReincarnate_ImplicitHarnessSwitchRefusedInPlace(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:          tid("hc-claude-guard-" + t.Name()),
		Name:        "claude-hc",
		Slug:        "claude-hc",
		Harness:     "claude",
		ContentHash: "claude-hash",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
	}))
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "claude-hc"})

	// Recorded harness config is not a valid single name component, so
	// dropUnusableDefaultHarnessConfig(harnessConfigTierRecorded) drops it
	// and deriveAgentConfig falls back to the hub default "claude-hc".
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.HarnessConfig = "legacy/bad-hc"
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})
	before := snapshotAgent(t, s, agent.ID)

	for _, dryRun := range []bool{true, false} {
		rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: dryRun})
		requireHarnessSwitchError(t, rec, http.StatusPreconditionFailed, ErrCodeImplicitHarnessSwitch)
		body := rec.Body.String()
		assert.Contains(t, body, "legacy/bad-hc")
		assert.Contains(t, body, "claude-hc")
		assert.Contains(t, body, "recorded harness config")
		assert.Contains(t, body, "--harness claude-hc")
		assert.Contains(t, body, experiments.ReincarnateHarnessSwitch)
	}
	assertAgentUntouched(t, s, disp, agent.ID, before)
}

// TestReincarnate_ImplicitHarnessSwitchRefusedOnMove verifies that the
// implicit harness-switch guard also refuses cross-broker moves (dry-run
// and real) with 412 ErrCodeImplicitHarnessSwitch before claiming or
// stopping anything.
func TestReincarnate_ImplicitHarnessSwitchRefusedOnMove(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	require.NoError(t, f.s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:          tid("hc-claude-move-guard-" + t.Name()),
		Name:        "claude-hc",
		Slug:        "claude-hc",
		Harness:     "claude",
		ContentHash: "claude-hash",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
	}))
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "claude-hc"})

	f.agent.AppliedConfig.HarnessConfig = "legacy/bad-hc"
	f.agent.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	require.NoError(t, f.s.UpdateAgent(ctx, f.agent))
	before := snapshotAgent(t, f.s, f.agent.ID)

	for _, dryRun := range []bool{true, false} {
		rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: dryRun, TargetBroker: f.dst.ID})
		requireHarnessSwitchError(t, rec, http.StatusPreconditionFailed, ErrCodeImplicitHarnessSwitch)
		assert.Contains(t, rec.Body.String(), "--harness claude-hc")
		assert.Contains(t, rec.Body.String(), experiments.ReincarnateHarnessSwitch)
	}
	assertAgentUntouched(t, f.s, f.disp, f.agent.ID, before)
}

func TestImplicitHarnessSwitch(t *testing.T) {
	hcPatch := &reincarnatePatch{HarnessConfig: &store.HarnessConfig{Slug: "claude"}}
	otherPatch := &reincarnatePatch{Model: "sonnet"}

	cases := []struct {
		name     string
		oldHC    string
		freshHC  string
		patch    *reincarnatePatch
		wantOK   bool
		wantFrom string
		wantTo   string
	}{
		{name: "same harness", oldHC: "codex", freshHC: "codex", wantOK: false},
		{name: "empty old harness", oldHC: "", freshHC: "claude", wantOK: false},
		{name: "explicit harness patch allowed", oldHC: "codex", freshHC: "claude", patch: hcPatch, wantOK: false},
		{name: "implicit change with nil patch", oldHC: "codex", freshHC: "claude", wantOK: true, wantFrom: "codex", wantTo: "claude"},
		{name: "implicit change with non-harness patch", oldHC: "codex", freshHC: "claude", patch: otherPatch, wantOK: true, wantFrom: "codex", wantTo: "claude"},
		{name: "implicit clear", oldHC: "codex", freshHC: "", wantOK: true, wantFrom: "codex", wantTo: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := implicitHarnessSwitch(
				&store.AgentAppliedConfig{HarnessConfig: tc.oldHC},
				&store.AgentAppliedConfig{HarnessConfig: tc.freshHC},
				tc.patch,
			)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantFrom, from)
			assert.Equal(t, tc.wantTo, to)
		})
	}
}

// TestReincarnate_PlanReasons verifies hub-written FieldChange.Reason and
// ReincarnationPlan.HarnessName across alias re-resolution, default model
// re-resolution, same-harness content updates, template updates, explicit
// patches, and unchanged fields (ptone/scion#7918).
func TestReincarnate_PlanReasons(t *testing.T) {
	t.Run("alias re-resolved and same-harness content updated", func(t *testing.T) {
		ctx := context.Background()
		disp := newReincarnateTestDispatcher()
		disp.imageRegistry = "us-docker.pkg.dev/p/r"
		srv, s, project, broker := setupReincarnateTestServer(t, disp)

		hc := &store.HarnessConfig{
			ID:          tid("hc-ag-reason-" + t.Name()),
			Name:        "antigravity",
			Slug:        "antigravity",
			Harness:     "antigravity",
			ContentHash: "9c1d-new-hash",
			Scope:       store.HarnessConfigScopeGlobal,
			Status:      store.HarnessConfigStatusActive,
			Config: &store.HarnessConfigData{
				Image: "scion-antigravity:latest",
				ModelAliases: map[string]string{
					"medium": "gemini-4-hydrogen-medium",
				},
			},
		}
		require.NoError(t, s.CreateHarnessConfig(ctx, hc))

		tmpl := &store.Template{
			ID:          tid("tmpl-reason-" + t.Name()),
			Name:        "dev",
			Slug:        "dev-reason-" + tidSlugSafe(t.Name()),
			Harness:     "antigravity",
			ContentHash: "tmpl-hash-v2",
			Scope:       store.TemplateScopeGlobal,
			Status:      store.TemplateStatusActive,
		}
		require.NoError(t, s.CreateTemplate(ctx, tmpl))

		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.Template = tmpl.Slug
			a.AppliedConfig.TemplateHash = "tmpl-hash-v1"
			a.AppliedConfig.HarnessConfig = "antigravity"
			a.AppliedConfig.HarnessConfigHash = "3f2a-old-hash"
			a.AppliedConfig.Image = "us-docker.pkg.dev/p/r/scion-antigravity:latest"
			a.AppliedConfig.Model = "Gemini 3.8 Flash (Medium)"
			a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
				HarnessConfig: "antigravity",
				InlineConfig:  &api.ScionConfig{Model: "medium"},
			}
		})

		rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		plan := decodeReincarnateResponse(t, rec).Plan

		assert.Equal(t, "antigravity", plan.HarnessName)
		assert.Equal(t, "template content updated", plan.Template.Reason)
		assert.Equal(t, `harness config "antigravity" content updated`, plan.HarnessCfg.Reason)
		assert.Equal(t, `alias "medium" re-resolved by harness config "antigravity"`, plan.Model.Reason)
		assert.Equal(t, "us-docker.pkg.dev/p/r/scion-antigravity:latest", plan.Image.Old)
		assert.Equal(t, "us-docker.pkg.dev/p/r/scion-antigravity:latest", plan.Image.New)
		assert.Empty(t, plan.Image.Reason, "unchanged hub-rewritten image must have no reason")
	})

	t.Run("unset model re-resolved from default", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultModel: "new-default-model"})

		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.AppliedConfig.Model = "old-default-model"
			a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
		})

		rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		plan := decodeReincarnateResponse(t, rec).Plan

		assert.Equal(t, "old-default-model", plan.Model.Old)
		assert.Equal(t, "new-default-model", plan.Model.New)
		assert.Equal(t, "not set at create; re-resolved from current project/template/hub default", plan.Model.Reason)
		assert.Empty(t, plan.Template.Reason, "unchanged template must have no reason")
	})

	t.Run("patched model gets requested reason and unchanged gets none", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.AppliedConfig.Model = "old-model"
		})

		rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true, Model: "explicit-model"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		plan := decodeReincarnateResponse(t, rec).Plan
		assert.Equal(t, "requested", plan.Model.Reason)

		recSame := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true, Model: "old-model"})
		require.Equal(t, http.StatusOK, recSame.Code, recSame.Body.String())
		planSame := decodeReincarnateResponse(t, recSame).Plan
		assert.Empty(t, planSame.Model.Reason, "no-op patch must have no reason")
	})
}
