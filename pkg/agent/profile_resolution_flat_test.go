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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Flat Runtime Broker profile resolution (.design/flat-runtime-brokers-contract.md
// section 10): a flat instance skips the Runtime Broker Profile tier,
// including the active_profile fallback, while non-profile defaults (the
// settings harness config, top-level image registry) still apply. Legacy
// resolution is unchanged.

// The active profile carries distinctive resources and harness overrides;
// the settings-level harness config carries a non-profile default.
const flatProfileResolutionSettings = `schema_version: "1"
image_registry: registry.example/global
active_profile: batch
harness_configs:
  test-harness:
    harness: gemini
    env:
      GLOBAL_HC_ENV: from-settings-harness-config
runtimes:
  docker:
    type: docker
profiles:
  batch:
    runtime: docker
    resources:
      requests:
        memory: 7777Mi
    harness_overrides:
      test-harness:
        image: profile-only-image:batch
        env:
          PROFILE_ONLY_ENV: from-active-profile
        resources:
          limits:
            cpu: "7"
`

type flatResolutionRun struct {
	fixture sharedDirStorageRunFixture
	runs    []runtime.RunConfig
	mgr     Manager
}

func newFlatResolutionRun(t *testing.T) *flatResolutionRun {
	t.Helper()
	r := &flatResolutionRun{fixture: newSharedDirStorageRunFixture(t)}
	require.NoError(t, os.WriteFile(filepath.Join(r.fixture.globalScionDir, "settings.yaml"),
		[]byte(flatProfileResolutionSettings), 0o644))
	r.mgr = NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			r.runs = append(r.runs, cfg)
			return "mock-id", nil
		},
	})
	return r
}

func (r *flatResolutionRun) start(t *testing.T, ctx context.Context, name string) runtime.RunConfig {
	t.Helper()
	_, err := r.mgr.Start(ctx, api.StartOptions{
		Name:        name,
		ProjectPath: r.fixture.projectScionDir,
		NoAuth:      true,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-" + name, "SCION_PROJECT_ID": "pid-flat"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, r.runs)
	return r.runs[len(r.runs)-1]
}

func runEnv(cfg runtime.RunConfig) map[string]string {
	out := map[string]string{}
	for _, kv := range cfg.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

func flatCtx() context.Context {
	return config.WithProfileResolution(context.Background(), config.ProfileResolutionFlatInstance)
}

func assertNoProfileTier(t *testing.T, cfg runtime.RunConfig) {
	t.Helper()
	env := runEnv(cfg)
	assert.NotContains(t, env, "PROFILE_ONLY_ENV", "active-profile harness override env must not reach a flat agent")
	assert.NotContains(t, cfg.Image, "profile-only-image", "active-profile harness override image must not reach a flat agent")
	if cfg.Resources != nil {
		assert.NotEqual(t, "7777Mi", cfg.Resources.Requests.Memory, "active-profile resources must not reach a flat agent")
		assert.NotEqual(t, "7", cfg.Resources.Limits.CPU, "active-profile harness override resources must not reach a flat agent")
	}
	assert.Equal(t, "from-settings-harness-config", env["GLOBAL_HC_ENV"], "the non-profile settings harness config still applies")
}

func TestFlatProfileResolution_StartAndRestartSkipProfileTier(t *testing.T) {
	r := newFlatResolutionRun(t)
	cfg := r.start(t, flatCtx(), "flat-agent")
	assertNoProfileTier(t, cfg)
	assert.Empty(t, GetSavedProfile("flat-agent", r.fixture.projectScionDir), "no profile is recorded for a flat agent")

	// A later start (and the start leg of a restart) re-reads settings and
	// still skips the profile tier.
	cfg = r.start(t, flatCtx(), "flat-agent")
	assertNoProfileTier(t, cfg)
	assert.Empty(t, GetSavedProfile("flat-agent", r.fixture.projectScionDir))
}

func TestFlatProfileResolution_LegacyStartStillAppliesActiveProfile(t *testing.T) {
	r := newFlatResolutionRun(t)
	cfg := r.start(t, context.Background(), "legacy-agent")
	env := runEnv(cfg)
	assert.Equal(t, "from-active-profile", env["PROFILE_ONLY_ENV"], "legacy resolution applies the active profile's harness overrides")
	assert.Contains(t, cfg.Image, "profile-only-image", "legacy resolution applies the active profile's image override")
	require.NotNil(t, cfg.Resources, "legacy resolution applies the active profile's resources")
	assert.Equal(t, "7777Mi", cfg.Resources.Requests.Memory)
	assert.Equal(t, "from-settings-harness-config", env["GLOBAL_HC_ENV"])
	assert.Equal(t, "batch", GetSavedProfile("legacy-agent", r.fixture.projectScionDir))
}

func TestFlatProfileResolution_ProvisionSkipsProfileTier(t *testing.T) {
	r := newFlatResolutionRun(t)
	_, _, flatCfg, err := ProvisionAgent(flatCtx(), "flat-prov", "", "", "", r.fixture.projectScionDir, "", "created", "", "")
	require.NoError(t, err)
	require.NotNil(t, flatCfg)
	if flatCfg.Resources != nil {
		assert.NotEqual(t, "7777Mi", flatCfg.Resources.Requests.Memory, "active-profile resources are not provisioned for a flat agent")
	}
	assert.Empty(t, GetSavedProfile("flat-prov", r.fixture.projectScionDir))

	// Legacy provisioning is unchanged.
	_, _, legacyCfg, err := ProvisionAgent(context.Background(), "legacy-prov", "", "", "", r.fixture.projectScionDir, "", "created", "", "")
	require.NoError(t, err)
	require.NotNil(t, legacyCfg)
	require.NotNil(t, legacyCfg.Resources)
	assert.Equal(t, "7777Mi", legacyCfg.Resources.Requests.Memory)
}

func TestForProfileResolution_DoesNotMutateSettings(t *testing.T) {
	vs := &config.VersionedSettings{ActiveProfile: "batch", ImageRegistry: "registry.example/global",
		Profiles: map[string]config.V1ProfileConfig{"batch": {Runtime: "docker", ImageRegistry: "registry.example/profile"}}}
	view := vs.ForProfileResolution(config.ProfileResolutionFlatInstance)
	assert.Equal(t, "batch", vs.ActiveProfile, "the loaded settings are not modified")
	assert.Len(t, vs.Profiles, 1)
	assert.Equal(t, "registry.example/global", view.ResolveImageRegistry(""), "the non-profile default still applies")
	assert.Equal(t, "registry.example/profile", vs.ResolveImageRegistry(""), "legacy resolution still uses the active profile")
	assert.Same(t, vs, vs.ForProfileResolution(config.ProfileResolutionLegacy))
}
