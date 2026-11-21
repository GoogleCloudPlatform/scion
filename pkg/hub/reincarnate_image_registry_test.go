//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registryMoveFixture is the move fixture with an explicit bare image on
// the agent and a dispatcher that answers the registry per broker.
func registryMoveFixture(t *testing.T, srcRegistry, dstRegistry string) *moveFixture {
	t.Helper()
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	f.agent.AppliedConfig.Image = srcRegistry + "/user-image:v1"
	f.agent.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: "user-image:v1"}
	f.agent.AppliedConfig.InlineConfig = &api.ScionConfig{Image: "user-image:v1"}
	require.NoError(t, f.s.UpdateAgent(ctx, f.agent))
	f.disp.mu.Lock()
	f.disp.imageRegistryByBroker = map[string]string{f.src.ID: srcRegistry, f.dst.ID: dstRegistry}
	f.disp.mu.Unlock()
	return f
}

// registryCallBrokers returns the brokers the dispatcher was asked for.
func registryCallBrokers(f *moveFixture) (brokers, profiles []string) {
	f.disp.mu.Lock()
	defer f.disp.mu.Unlock()
	for _, c := range f.disp.registryCalls {
		brokers = append(brokers, c[0])
		profiles = append(profiles, c[1])
	}
	return brokers, profiles
}

// A move resolves the new generation's registry on the target broker,
// never the source: the plan shows the image in the target's form
// (ptone/scion#3916).
func TestReincarnateMove_DryRun_ImageRegistryForTargetBroker(t *testing.T) {
	f := registryMoveFixture(t, "src.example", "dst.example")
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "dst.example/user-image:v1", resp.Plan.Image.New, "the target broker's registry")
	assert.Equal(t, "src.example/user-image:v1", resp.Plan.Image.Old)

	brokers, _ := registryCallBrokers(f)
	require.NotEmpty(t, brokers)
	for _, b := range brokers {
		assert.Equal(t, f.dst.ID, b, "the registry is resolved only for the target broker")
	}
}

// A move to a broker the hub does not rewrite for (a flat broker, or a
// co-located agent with no profile: the provider answers "") keeps the
// image bare; the source's registry is not baked in.
func TestReincarnateMove_DryRun_TargetWithoutHubRewriteKeepsBareImage(t *testing.T) {
	f := registryMoveFixture(t, "src.example", "")
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "src.example/user-image:v1", resp.Plan.Image.Old)
	assert.Equal(t, "user-image:v1", resp.Plan.Image.New)
	assert.Equal(t, "same image; registry resolved by target broker", resp.Plan.Image.Reason)
}

// In place, the registry is resolved on the agent's own broker for the new
// generation's profile, and the stored image uses that answer. The old
// generation ran with "old-profile"; the new one's profile comes from its
// create inputs ("new-profile"), and the two profiles have different
// registries, so using the old profile would store the wrong image.
func TestBuildPatchedAppliedConfig_ImageRegistryForNewProfile(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	f.agent.AppliedConfig.Profile = "old-profile"
	f.agent.AppliedConfig.Image = "old.example/user-image:v1"
	f.agent.AppliedConfig.CreateInputs.Profile = "new-profile"
	f.agent.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: "user-image:v1"}
	f.agent.AppliedConfig.InlineConfig = &api.ScionConfig{Image: "user-image:v1"}
	registries := map[string]string{"old-profile": "old.example", "new-profile": "new.example"}
	var asked []string
	fresh, _, err := f.srv.buildPatchedAppliedConfig(ctx, f.agent, f.project, func(profile string) string {
		asked = append(asked, profile)
		return registries[profile]
	}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "new-profile", fresh.Profile, "precondition: the new generation's profile")
	assert.Equal(t, []string{"new-profile"}, asked, "resolved for the new profile, never the old one")
	assert.Equal(t, "new.example/user-image:v1", fresh.Image)
}

func TestSameImageModuloBrokerRewrite(t *testing.T) {
	cases := []struct {
		name     string
		oldImg   string
		freshImg string
		want     bool
	}{
		{name: "empty old", oldImg: "", freshImg: "scion-claude:latest", want: false},
		{name: "empty fresh", oldImg: "us-docker.pkg.dev/p/r/scion-claude:latest", freshImg: "", want: false},
		{name: "exact basename match", oldImg: "us-docker.pkg.dev/p/r/scion-claude:latest", freshImg: "scion-claude:latest", want: true},
		{name: "scion image retagged by broker from latest", oldImg: "us-docker.pkg.dev/p/r/scion-claude:v0.12.0", freshImg: "scion-claude:latest", want: true},
		{name: "scion image retagged by broker from untagged", oldImg: "us-docker.pkg.dev/p/r/scion-claude:v0.12.0", freshImg: "scion-claude", want: true},
		{name: "scion image explicit non-latest tag differs", oldImg: "us-docker.pkg.dev/p/r/scion-claude:latest", freshImg: "scion-claude:v2", want: false},
		{name: "different scion image name", oldImg: "us-docker.pkg.dev/p/r/scion-claude:latest", freshImg: "scion-codex:latest", want: false},
		{name: "non-scion same basename", oldImg: "us-docker.pkg.dev/p/r/user-image:v1", freshImg: "user-image:v1", want: true},
		{name: "non-scion different tag", oldImg: "us-docker.pkg.dev/p/r/user-image:v2", freshImg: "user-image:latest", want: false},
		{name: "digest mismatch", oldImg: "us-docker.pkg.dev/p/r/scion-claude@sha256:aaa", freshImg: "scion-claude:latest", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sameImageModuloBrokerRewrite(tc.oldImg, tc.freshImg))
		})
	}
}

// TestReincarnateInPlace_DryRun_BrokerOwnedRegistryShowsUnchanged verifies
// that when the hub leaves registry rewriting to the broker (flat broker or
// co-located broker with empty profile) and the broker-echoed qualified image
// matches the bare harness image, an in-place dry-run reports Image as
// unchanged using the qualified old image (ptone/scion#7917).
func TestReincarnateInPlace_DryRun_BrokerOwnedRegistryShowsUnchanged(t *testing.T) {
	for _, mode := range []string{"flat-broker", "colocated-empty-profile"} {
		t.Run(mode, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			// Empty imageRegistry simulates dispatchImageRegistryFor returning "".
			disp.imageRegistry = ""
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			if mode == "colocated-empty-profile" {
				srv.embeddedBrokerID = broker.ID
			}

			hc := createImageHarnessConfig(t, s, "scion-claude:latest")
			qualified := "us-docker.pkg.dev/my-proj/scion/scion-claude:latest"
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.AppliedConfig.HarnessConfig = hc.Slug
				a.AppliedConfig.HarnessConfigID = hc.ID
				a.AppliedConfig.HarnessConfigHash = hc.ContentHash
				a.AppliedConfig.Image = qualified
				a.AppliedConfig.Profile = ""
				a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: hc.Slug}
			})

			rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			plan := decodeReincarnateResponse(t, rec).Plan
			assert.Equal(t, qualified, plan.Image.Old)
			assert.Equal(t, qualified, plan.Image.New)
			assert.Equal(t, "registry resolved by broker", plan.Image.Reason)
		})
	}
}

// TestComputeReincarnationPlan_HubImageTagBareOldUnchanged verifies that when
// the hub owns the registry and image_tag rewrite and old.Image is stored bare
// without a tag, RewriteImageRef normalizes old to match fresh (ptone/scion#7917).
func TestComputeReincarnationPlan_HubImageTagBareOldUnchanged(t *testing.T) {
	old := &store.AgentAppliedConfig{Image: "scion-claude"}
	fresh := &store.AgentAppliedConfig{Image: "ghcr.io/org/scion-claude:v0.3.0"}
	plan := computeReincarnationPlan(old, fresh, nil, planImageContext{
		Registry:   "ghcr.io/org",
		Tag:        "v0.3.0",
		SameBroker: true,
	})
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", plan.Image.Old)
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", plan.Image.New)
}

// TestReincarnateInPlace_DryRun_BrokerOwnedRegistryGenuineImageChange verifies
// that a real image change (e.g. scion-claude -> scion-codex via --harness)
// still renders old -> new when the broker owns the registry rewrite.
func TestReincarnateInPlace_DryRun_BrokerOwnedRegistryGenuineImageChange(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	disp.imageRegistry = ""
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setExperimentOverrides(t, srv, map[string]bool{experiments.ReincarnateHarnessSwitch: true})
	broker.Capabilities = &store.BrokerCapabilities{Reprovision: true, ReprovisionResetHomeRuntimes: []string{"docker"}}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	claudeHC := &store.HarnessConfig{
		ID: tid("hc-claude-img-" + t.Name()), Name: "claude-hc", Slug: "claude-hc",
		Harness: "claude", ContentHash: "h-claude", Scope: store.HarnessConfigScopeGlobal,
		Status: store.HarnessConfigStatusActive, Config: &store.HarnessConfigData{Image: "scion-claude:latest"},
	}
	codexHC := &store.HarnessConfig{
		ID: tid("hc-codex-img-" + t.Name()), Name: "codex-hc", Slug: "codex-hc",
		Harness: "codex", ContentHash: "h-codex", Scope: store.HarnessConfigScopeGlobal,
		Status: store.HarnessConfigStatusActive, Config: &store.HarnessConfigData{Image: "scion-codex:latest"},
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, claudeHC))
	require.NoError(t, s.CreateHarnessConfig(ctx, codexHC))

	qualified := "us-docker.pkg.dev/my-proj/scion/scion-claude:latest"
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Runtime = "docker"
		a.AppliedConfig.HarnessConfig = claudeHC.Slug
		a.AppliedConfig.HarnessConfigID = claudeHC.ID
		a.AppliedConfig.HarnessConfigHash = claudeHC.ContentHash
		a.AppliedConfig.Image = qualified
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: claudeHC.Slug}
	})

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{DryRun: true, HarnessConfig: codexHC.Slug})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	plan := decodeReincarnateResponse(t, rec).Plan
	assert.Equal(t, qualified, plan.Image.Old)
	assert.Equal(t, "scion-codex:latest", plan.Image.New)
	assert.Equal(t, "registry resolved by broker", plan.Image.Reason)
}
