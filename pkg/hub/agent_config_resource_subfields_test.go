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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#5676: a resources edit that drops a stored
// sub-field records resources.<sub-field> in cleared_fields, which a
// broker with clearedResourceSubfields applies at the next start, and the
// hub reports it applied only for such a broker.

// resourceSubfieldCapSets are the broker capability sets the sub-field
// clear tests run against: none, clearedFields only (an older broker that
// ignores the sub-field entries), and clearedResourceSubfields.
var resourceSubfieldCapSets = []struct {
	name    string
	caps    store.BrokerCapabilities
	applies bool
}{
	{name: "none", caps: store.BrokerCapabilities{Reprovision: true}},
	{name: "clearedFields", caps: store.BrokerCapabilities{Reprovision: true, ClearedFields: true}},
	{name: "clearedResourceSubfields", caps: store.BrokerCapabilities{Reprovision: true, ClearedFields: true, ClearedResourceSubfields: true}, applies: true},
}

// subfieldTestResources is the agent's own stored resources in these tests.
func subfieldTestResources() *api.ResourceSpec {
	return &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "1", Memory: "1Gi"},
		Limits:   api.ResourceList{CPU: "2", Memory: "4Gi"},
	}
}

func newSubfieldTestAgent(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker, phase state.Phase) *store.Agent {
	t.Helper()
	a := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(phase)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Resources: subfieldTestResources()}
	})
	return pendingReloadAgent(t, s, a.ID)
}

// TestAgentConfigPatch_ResourcesSubFieldDrop: on the live path (stopped
// agent), dropping limits.memory while changing limits.cpu records
// resources.limits.memory whatever the broker supports; config.resources
// is applied with no warning only on a broker with
// clearedResourceSubfields, and held for reincarnation with the cleared
// warning otherwise (the R292 boundary: an absent capability holds).
func TestAgentConfigPatch_ResourcesSubFieldDrop(t *testing.T) {
	for _, c := range resourceSubfieldCapSets {
		t.Run(c.name, func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			caps := c.caps
			setBrokerCapabilities(t, s, broker.ID, &caps)
			agent := newSubfieldTestAgent(t, s, project, broker, state.PhaseStopped)

			resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"resources": map[string]interface{}{
					"requests": map[string]string{"cpu": "1", "memory": "1Gi"},
					"limits":   map[string]string{"cpu": "3"},
				}},
			})
			require.Equal(t, http.StatusOK, code, body)
			if c.applies {
				assert.Equal(t, []string{"config.resources"}, resp.Disposition.Applied)
				assert.Empty(t, resp.Disposition.HeldForReincarnate)
				assert.Empty(t, resp.Warnings)
			} else {
				assert.Empty(t, resp.Disposition.Applied)
				assert.Equal(t, []string{"config.resources"}, resp.Disposition.HeldForReincarnate)
				require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
				assert.Contains(t, resp.Warnings[0], "config.resources: cleared now")
			}
			stored := pendingReloadAgent(t, s, agent.ID)
			assert.Equal(t, []string{"resources.limits.memory"}, stored.AppliedConfig.InlineConfig.ClearedFields, "recorded on every broker")
			assert.Equal(t, "3", stored.AppliedConfig.InlineConfig.Resources.Limits.CPU)
			assert.Empty(t, stored.AppliedConfig.InlineConfig.Resources.Limits.Memory)
		})
	}
}

// TestAgentPendingConfig_ResourcesSubFieldDrop: on the held path (running
// agent, held edits on), the same edit is held (applied at the next start)
// only on a broker with clearedResourceSubfields, and held for
// reincarnation otherwise. On the capable broker the next start applies
// it: the dispatched inline config lists the sub-path and omits the value.
func TestAgentPendingConfig_ResourcesSubFieldDrop(t *testing.T) {
	for _, c := range resourceSubfieldCapSets {
		t.Run(c.name, func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			setHeldEditsExperiment(t, srv, true)
			caps := c.caps
			setBrokerCapabilities(t, s, broker.ID, &caps)
			agent := newSubfieldTestAgent(t, s, project, broker, state.PhaseRunning)

			resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
				"resources": map[string]interface{}{
					"requests": map[string]string{"cpu": "1", "memory": "1Gi"},
					"limits":   map[string]string{"cpu": "3"},
				},
			}})
			if !c.applies {
				assert.Empty(t, resp.Disposition.Held)
				assert.Equal(t, []string{"config.resources"}, resp.Disposition.HeldForReincarnate)
				return
			}
			assert.Equal(t, []string{"config.resources"}, resp.Disposition.Held)
			assert.Empty(t, resp.Disposition.HeldForReincarnate)
			assert.Empty(t, resp.Warnings)

			pendingLifecycleWarnings(t, srv, agent.ID, "stop")
			pendingLifecycleWarnings(t, srv, agent.ID, "start")
			starts := disp.snapshot()
			require.Len(t, starts, 1)
			assert.Equal(t, []string{"resources.limits.memory"}, starts[0].inline.ClearedFields)
			require.NotNil(t, starts[0].inline.Resources)
			assert.Equal(t, "3", starts[0].inline.Resources.Limits.CPU)
			assert.Empty(t, starts[0].inline.Resources.Limits.Memory)
		})
	}
}

// TestAgentConfigPatch_ResourcesSubFieldDispositionInvariant is the
// design's invariant for resources sub-field clears: on the live and held
// paths and for each broker capability set, config.resources is listed
// under applied (live) or held (held) exactly when the next start,
// simulated by the shared merge and cleared-field rules, leaves the agent's
// own resources as the edit asked: limits.cpu 3, limits.memory empty (so
// the per-start resource tiers fill it), the requests kept.
func TestAgentConfigPatch_ResourcesSubFieldDispositionInvariant(t *testing.T) {
	want := &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "1", Memory: "1Gi"},
		Limits:   api.ResourceList{CPU: "3"},
	}
	for _, held := range []bool{false, true} {
		path := "live"
		if held {
			path = "held"
		}
		for _, c := range resourceSubfieldCapSets {
			t.Run(path+"/"+c.name, func(t *testing.T) {
				disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
				srv, s, project, broker := setupReincarnateTestServer(t, disp)
				caps := c.caps
				setBrokerCapabilities(t, s, broker.ID, &caps)
				phase := state.PhaseStopped
				if held {
					setHeldEditsExperiment(t, srv, true)
					phase = state.PhaseRunning
				}
				agent := newSubfieldTestAgent(t, s, project, broker, phase)

				resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
					"config": map[string]interface{}{"resources": map[string]interface{}{
						"requests": map[string]string{"cpu": "1", "memory": "1Gi"},
						"limits":   map[string]string{"cpu": "3"},
					}},
				})
				require.Equal(t, http.StatusOK, code, body)
				list := resp.Disposition.Applied
				if held {
					list = resp.Disposition.Held
					pendingLifecycleWarnings(t, srv, agent.ID, "stop")
				}
				listed := false
				for _, k := range list {
					listed = listed || k == "config.resources"
				}
				pendingLifecycleWarnings(t, srv, agent.ID, "start")
				starts := disp.snapshot()
				require.NotEmpty(t, starts)
				persisted := &api.ScionConfig{Resources: subfieldTestResources()}
				effective := simulateBrokerStart(persisted, starts[len(starts)-1].inline, c.caps, &api.ScionConfig{}, nil)
				require.NotNil(t, effective.Resources)
				assert.Equal(t, *want == *effective.Resources, listed,
					"listed=%v, but the start yields %+v", listed, *effective.Resources)
			})
		}
	}
}

// TestAgentConfigPatch_ResourcesSubFieldDropResumes: a sub-field clear made
// while the agent is suspended reaches the resume's dispatched inline
// config, as it reaches a start's.
func TestAgentConfigPatch_ResourcesSubFieldDropResumes(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setBrokerCapabilities(t, s, broker.ID, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true, ClearedResourceSubfields: true})
	agent := newSubfieldTestAgent(t, s, project, broker, state.PhaseSuspended)

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"resources": map[string]interface{}{
			"limits": map[string]string{"cpu": "2", "memory": "4Gi"},
		}},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.resources"}, resp.Disposition.Applied)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.True(t, starts[0].resume, "a start of a suspended agent resumes it")
	assert.Equal(t, []string{"resources.requests.cpu", "resources.requests.memory"}, starts[0].inline.ClearedFields)
	require.NotNil(t, starts[0].inline.Resources)
	assert.Empty(t, starts[0].inline.Resources.Requests.CPU)
	assert.Empty(t, starts[0].inline.Resources.Requests.Memory)
}

// TestAgentConfigPatch_ResourcesSubFieldDropRestarts: a sub-field clear
// held for a running agent reaches the dispatched inline config of a
// user restart's start leg, as it reaches a start's and a resume's.
func TestAgentConfigPatch_ResourcesSubFieldDropRestarts(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setHeldEditsExperiment(t, srv, true)
	setBrokerCapabilities(t, s, broker.ID, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true, ClearedResourceSubfields: true})
	agent := newSubfieldTestAgent(t, s, project, broker, state.PhaseRunning)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
		"resources": map[string]interface{}{
			"requests": map[string]string{"cpu": "1", "memory": "1Gi"},
			"limits":   map[string]string{"cpu": "2"},
		},
	}})
	assert.Equal(t, []string{"config.resources"}, resp.Disposition.Held)

	pendingLifecycleWarnings(t, srv, agent.ID, "restart")
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.False(t, starts[0].resume, "a restart's start leg is a fresh start")
	assert.Equal(t, []string{"resources.limits.memory"}, starts[0].inline.ClearedFields)
	require.NotNil(t, starts[0].inline.Resources)
	assert.Equal(t, "2", starts[0].inline.Resources.Limits.CPU)
	assert.Empty(t, starts[0].inline.Resources.Limits.Memory)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
}

// TestDispatchAgentStart_SendsResourceSubfieldClears drives the real start
// dispatch, for a start and a resume: the inline config handed to the
// broker client lists the agent's resources sub-field clears and omits the
// cleared value (ptone/scion#5676 acceptance).
func TestDispatchAgentStart_SendsResourceSubfieldClears(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "resume"}[resume], func(t *testing.T) {
			f := newTiersFixture(t, tiersKnownFalse)
			agent := &store.Agent{
				ID: tid("subfield-start"), Name: "subfield-start", Slug: "subfield-start", ProjectID: f.project.ID,
				OwnerID: "dev@localhost", RuntimeBrokerID: tid("tiers-broker"),
				AppliedConfig: &store.AgentAppliedConfig{
					InlineConfig: &api.ScionConfig{
						Resources:     &api.ResourceSpec{Limits: api.ResourceList{CPU: "3"}},
						ClearedFields: []string{"resources.limits.memory"},
					},
					CreateInputs: &store.AgentCreateInputs{},
				},
			}
			require.NoError(t, f.d.DispatchAgentStart(context.Background(), agent, "", resume))
			ic := f.client.lastInlineConfig
			require.NotNil(t, ic)
			assert.Equal(t, []string{"resources.limits.memory"}, ic.ClearedFields)
			require.NotNil(t, ic.Resources)
			assert.Equal(t, "3", ic.Resources.Limits.CPU)
			assert.Empty(t, ic.Resources.Limits.Memory)
		})
	}
}
