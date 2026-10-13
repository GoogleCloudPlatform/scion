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

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// A flat instance's agent_defaults in agent provisioning and start
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC5): each value
// takes the rung the active profile's value took.

func agentDefaultsCtx(d *config.V1InstanceAgentDefaults) context.Context {
	return config.WithFlatInstancePolicy(context.Background(), config.NewFlatInstancePolicy(config.V1RuntimeBrokerInstanceConfig{
		Key: "docker-1", Name: "docker-1", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}, AgentDefaults: d}))
}

func agentDefaultsView(vs *config.VersionedSettings, d *config.V1InstanceAgentDefaults) *config.VersionedSettings {
	return vs.ForFlatInstance(config.FlatInstancePolicyFrom(agentDefaultsCtx(d)))
}

// W5 and W7: lowest tier first, settings default_resources < Hub
// agent_defaults.resources < instance resources < the agent's own <
// instance harness_overrides.<hc>.resources, exactly the profile's rungs.
// The active profile's resources never apply.
func TestResolveEnvironmentResources_FlatInstanceRungs(t *testing.T) {
	vs := &config.VersionedSettings{
		ActiveProfile: "batch",
		DefaultResources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "100m", Memory: "100Mi"},
			Limits:   api.ResourceList{CPU: "1", Memory: "8Gi"},
			Disk:     "10Gi",
		},
		Profiles: map[string]config.V1ProfileConfig{"batch": {Runtime: "docker",
			Resources: &api.ResourceSpec{Requests: api.ResourceList{Memory: "7777Mi"}}}},
	}
	instance := &config.V1InstanceAgentDefaults{
		Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "500m", Memory: "1Gi"}},
		HarnessOverrides: map[string]config.V1HarnessOverride{
			"claude": {Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}},
		},
	}
	hubCtx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{
		Resources: &api.ResourceSpec{Requests: api.ResourceList{Memory: "2Gi"}, Limits: api.ResourceList{Memory: "4Gi"}}})

	view := agentDefaultsView(vs, instance)
	got := resolveEnvironmentResources(hubCtx, nil, view, "", "claude")
	require.NotNil(t, got)
	assert.Equal(t, "1Gi", got.Requests.Memory, "instance resources beat the Hub agent_defaults.resources")
	assert.Equal(t, "500m", got.Requests.CPU, "instance resources beat default_resources")
	assert.Equal(t, "4Gi", got.Limits.Memory, "the Hub agent_defaults.resources fill what the instance leaves unset, over default_resources")
	assert.Equal(t, "2", got.Limits.CPU, "the instance harness override resources apply")
	assert.Equal(t, "10Gi", got.Disk, "default_resources fill the rest")

	own := &api.ResourceSpec{Requests: api.ResourceList{Memory: "3Gi"}, Limits: api.ResourceList{CPU: "3"}}
	got = resolveEnvironmentResources(hubCtx, own, view, "", "claude")
	require.NotNil(t, got)
	assert.Equal(t, "3Gi", got.Requests.Memory, "the agent's own resources beat the instance resources")
	assert.Equal(t, "500m", got.Requests.CPU, "the instance fills what the agent leaves unset")
	assert.Equal(t, "2", got.Limits.CPU, "the instance harness override resources rank above the agent's own (W7)")
	assert.Equal(t, "3Gi", own.Requests.Memory, "own is not modified")
	assert.Equal(t, "3", own.Limits.CPU, "own is not modified")

	// A harness config without an instance override gets no override
	// resources; the profile name is not used.
	got = resolveEnvironmentResources(hubCtx, nil, view, "batch", "gemini")
	require.NotNil(t, got)
	assert.Equal(t, "1Gi", got.Requests.Memory)
	assert.Equal(t, "1", got.Limits.CPU, "no override for gemini: default_resources' limit stays")

	// Fall-through: no instance resources, so the Hub tier and
	// default_resources apply, never the active profile's.
	got = resolveEnvironmentResources(hubCtx, nil, agentDefaultsView(vs, nil), "", "claude")
	require.NotNil(t, got)
	assert.Equal(t, "2Gi", got.Requests.Memory, "the Hub tier applies when the instance sets no resources")
	assert.Equal(t, "100m", got.Requests.CPU)

	// Control: legacy resolution still applies the active profile's.
	got = resolveEnvironmentResources(hubCtx, nil, vs, "", "claude")
	require.NotNil(t, got)
	assert.Equal(t, "7777Mi", got.Requests.Memory)
}

const flatImageSettings = `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image: profile-pinned:v4
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
`

func startFlatCapturingRun(t *testing.T, ctx context.Context, opts api.StartOptions) runtime.RunConfig {
	t.Helper()
	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	}
	opts.BrokerMode = true
	opts.NoAuth = true
	// A flat start records its ownership under the dispatch's agent and
	// project IDs, as a Hub dispatch carries them.
	opts.Env = map[string]string{"SCION_AGENT_ID": "agent-" + opts.Name, "SCION_PROJECT_ID": "pid-flat"}
	_, err := NewManager(mockRT).Start(ctx, opts)
	require.NoError(t, err)
	return captured
}

// W6, the ptone/scion#1799 exception: on a flat instance the explicit
// agent_defaults.harness_overrides.<hc>.image outranks the plain settings
// image and the inline image, as a profile override does, while a request
// image still beats it. The active profile's override never applies.
func TestStart_FlatInstanceOverrideImageKeepsProfileRank(t *testing.T) {
	projectScionDir := imagePrecedenceFixture(t, "template-pinned:v2", flatImageSettings)
	ctx := agentDefaultsCtx(&config.V1InstanceAgentDefaults{HarnessOverrides: map[string]config.V1HarnessOverride{
		"test-harness": {Image: "instance-pinned:v5"}}})

	run := startFlatCapturingRun(t, ctx, api.StartOptions{Name: "flat-img", ProjectPath: projectScionDir})
	assert.Equal(t, "instance-pinned:v5", run.Image, "the instance override image beats the settings image")

	run = startFlatCapturingRun(t, ctx, api.StartOptions{Name: "flat-img-inline", ProjectPath: projectScionDir,
		InlineConfig: &api.ScionConfig{Image: "inline-pinned:v3"}})
	assert.Equal(t, "instance-pinned:v5", run.Image, "the instance override image beats the inline image")

	run = startFlatCapturingRun(t, ctx, api.StartOptions{Name: "flat-img-req", ProjectPath: projectScionDir, Image: "request:v9"})
	assert.Equal(t, "request:v9", run.Image, "a request image still beats the instance override")

	// Fall-through: an instance without an override image gets the plain
	// settings image, not the active profile's override.
	run = startFlatCapturingRun(t, agentDefaultsCtx(nil), api.StartOptions{Name: "flat-img-none", ProjectPath: projectScionDir})
	assert.Equal(t, "settings-pinned:v1", run.Image)

	// Control: legacy resolution applies the active profile's override.
	run = startFlatCapturingRun(t, context.Background(), api.StartOptions{Name: "legacy-img", ProjectPath: projectScionDir})
	assert.Equal(t, "profile-pinned:v4", run.Image)
}

// End to end: a flat start applies the instance's resources and its
// harness override env and resources, and none of the active profile's.
func TestFlatProfileResolution_StartUsesInstanceAgentDefaults(t *testing.T) {
	r := newFlatResolutionRun(t)
	ctx := agentDefaultsCtx(&config.V1InstanceAgentDefaults{
		Resources: &api.ResourceSpec{Requests: api.ResourceList{Memory: "1234Mi"}},
		HarnessOverrides: map[string]config.V1HarnessOverride{"test-harness": {
			Env:       map[string]string{"INSTANCE_ENV": "from-instance"},
			Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "3"}},
		}},
	})
	cfg := r.start(t, ctx, "flat-defaults")
	assertNoProfileTier(t, cfg)
	assert.Equal(t, "from-instance", runEnv(cfg)["INSTANCE_ENV"], "the instance harness override env reaches the agent")
	require.NotNil(t, cfg.Resources)
	assert.Equal(t, "1234Mi", cfg.Resources.Requests.Memory, "the instance resources reach the agent")
	assert.Equal(t, "3", cfg.Resources.Limits.CPU, "the instance harness override resources reach the agent")
}
