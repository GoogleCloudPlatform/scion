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
	assert.Equal(t, "user-image:v1", resp.Plan.Image.New)
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
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "new-profile", fresh.Profile, "precondition: the new generation's profile")
	assert.Equal(t, []string{"new-profile"}, asked, "resolved for the new profile, never the old one")
	assert.Equal(t, "new.example/user-image:v1", fresh.Image)
}
