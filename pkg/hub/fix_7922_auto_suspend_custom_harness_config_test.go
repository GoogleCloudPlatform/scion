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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStalledAgentWithHarnessConfig creates a global harness config with an
// opaque (custom) slug naming harnessType, and a stalled running agent on an
// online broker whose applied config references it by that slug.
func newStalledAgentWithHarnessConfig(t *testing.T, s store.Store, name, slug, harnessType string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: tid("hc-" + name), Name: slug, Slug: slug, Harness: harnessType,
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{Harness: harnessType},
	}))
	agent := newStalledBrokerAgent(t, s, name)
	agent.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: slug}
	require.NoError(t, s.UpdateAgent(ctx, agent))
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	return loaded
}

// ptone/scion#7922: an agent whose harness config has a custom name is
// suspend-eligible when the config's harness type supports resume.
func TestAutoSuspend_CustomNamedResumableHarnessConfigSuspends(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	agent := newStalledAgentWithHarnessConfig(t, s, "as-7922-claude", "team-reviewer", "claude")

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*agent})

	assert.Equal(t, int32(1), disp.stops.Load(), "the container is stopped")
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
}

// ptone/scion#7922: an agent whose custom-named harness config is of a
// non-resumable harness is skipped, and the skip is logged with its reason.
func TestAutoSuspend_CustomNamedNonResumableHarnessConfigSkipsWithLog(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	agent := newStalledAgentWithHarnessConfig(t, s, "as-7922-generic", "team-shell", "generic")
	logs := captureSlog(t)

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*agent})

	assert.Zero(t, disp.stops.Load(), "no stop is dispatched")
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	out := logs.String()
	assert.Contains(t, out, "level=INFO")
	assert.Contains(t, out, "skipping auto-suspend; harness does not support resume")
	assert.Contains(t, out, "harness_config=team-shell")
	assert.Contains(t, out, "harness=generic")
	assert.Contains(t, out, `reason="Generic harness does not support session resume"`)
}
