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
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// explicitImageAgentConfig is an applied config whose user chose image.
func explicitImageAgentConfig(image string) *store.AgentAppliedConfig {
	return &store.AgentAppliedConfig{
		CreateInputs: &store.AgentCreateInputs{InlineConfig: &api.ScionConfig{Image: image}},
	}
}

// TestDispatch_ImageTag_AllPaths (ptone/scion#6625): with a tag, create,
// start, resume and restart all send it as HubAgentDefaults.ImageTag and
// send the explicit image tagged and registry-qualified; with no tag (the
// experiment off) the wire is exactly the registry-only form and carries
// no hub defaults.
func TestDispatch_ImageTag_AllPaths(t *testing.T) {
	ctx := context.Background()
	d, client, ag := autoExposeDispatchFixture(t)
	ag.AppliedConfig = explicitImageAgentConfig("scion-claude:latest")
	d.SetImageRegistryProvider(func(string, string) string { return "ghcr.io/org" })
	tag := "v0.3.0"
	var asked [][2]string
	d.SetImageTagProvider(func(brokerID, profile string) string {
		asked = append(asked, [2]string{brokerID, profile})
		return tag
	})

	req, err := d.buildCreateRequest(ctx, ag, "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config)
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", req.Config.Image, "create: explicit image tagged, then qualified")
	require.NotNil(t, req.Config.HubAgentDefaults)
	assert.Equal(t, "v0.3.0", req.Config.HubAgentDefaults.ImageTag)

	// The broker decodes the same tag.
	blob, err := json.Marshal(req.Config)
	require.NoError(t, err)
	var got runtimebroker.CreateAgentConfig
	require.NoError(t, json.Unmarshal(blob, &got))
	assert.Equal(t, "v0.3.0", got.HubAgentDefaults.ResolvedImageTag(), "broker side: %s", blob)

	require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
	require.NotNil(t, client.lastStartExtras.HubAgentDefaults)
	assert.Equal(t, "v0.3.0", client.lastStartExtras.HubAgentDefaults.ImageTag, "start")
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", client.lastStartExtras.Image, "start")

	require.NoError(t, d.DispatchAgentStart(ctx, ag, "", true))
	require.NotNil(t, client.lastStartExtras.HubAgentDefaults)
	assert.Equal(t, "v0.3.0", client.lastStartExtras.HubAgentDefaults.ImageTag, "resume")
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", client.lastStartExtras.Image, "resume")

	require.NoError(t, d.DispatchAgentRestart(ctx, ag))
	require.NotNil(t, client.lastRestartExtras.HubAgentDefaults)
	assert.Equal(t, "v0.3.0", client.lastRestartExtras.HubAgentDefaults.ImageTag, "restart")
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", client.lastRestartExtras.Image, "restart")

	for _, a := range asked {
		assert.Equal(t, ag.RuntimeBrokerID, a[0], "the tag is resolved for the agent's broker")
	}

	// Experiment off: no tag, byte-identical to the registry-only wire.
	tag = ""
	req, err = d.buildCreateRequest(ctx, ag, "test")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/org/scion-claude:latest", req.Config.Image)
	assert.Nil(t, req.Config.HubAgentDefaults, "no tag and no other default: nothing on the wire")
	require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
	assert.Nil(t, client.lastStartExtras.HubAgentDefaults)
	assert.Equal(t, "ghcr.io/org/scion-claude:latest", client.lastStartExtras.Image)
	require.NoError(t, d.DispatchAgentRestart(ctx, ag))
	assert.Nil(t, client.lastRestartExtras.HubAgentDefaults)
	assert.Equal(t, "ghcr.io/org/scion-claude:latest", client.lastRestartExtras.Image)
}

// TestDispatch_ImageTag_ExplicitTagKept: an explicit non-latest tag, a
// digest and another registry's reference are sent as they are.
func TestDispatch_ImageTag_ExplicitTagKept(t *testing.T) {
	d, _, ag := autoExposeDispatchFixture(t)
	d.SetImageRegistryProvider(func(string, string) string { return "ghcr.io/org" })
	d.SetImageTagProvider(func(string, string) string { return "v0.3.0" })
	digest := "scion-claude@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for image, want := range map[string]string{
		"scion-claude:v0.2.0":           "ghcr.io/org/scion-claude:v0.2.0",
		digest:                          "ghcr.io/org/" + digest,
		"quay.io/x/scion-claude:latest": "quay.io/x/scion-claude:latest",
		"ghcr.io/org/scion-claude":      "ghcr.io/org/scion-claude:v0.3.0",
	} {
		ag.AppliedConfig = explicitImageAgentConfig(image)
		assert.Equal(t, want, d.dispatchImageForBroker(ag, d.dispatchImageTag(ag)), image)
	}
}

// TestDispatch_ImageTagProviderInstalled: the server wires the provider,
// and it follows the experiment.
func TestDispatch_ImageTagProviderInstalled(t *testing.T) {
	srv, _, ops := newTestDBServerWithStore(t, createTestStore(t))
	setTestImageTagFileTier(t, ops, "v0.3.0", nil)
	d := srv.CreateAuthenticatedDispatcher()
	require.NotNil(t, d.imageTagProvider, "CreateAuthenticatedDispatcher did not install the image tag provider")
	assert.Equal(t, "", d.ImageTagFor("", ""), "off by default")
	setImageTagExperiment(t, ops, true)
	assert.Equal(t, "v0.3.0", d.ImageTagFor("", ""))
}

// TestBuildPatchedAppliedConfig_ImageTag: a reincarnation stores the image
// in the form the broker runs, tagged for the new generation profile;
// with no tag resolver the image is the registry-only form.
func TestBuildPatchedAppliedConfig_ImageTag(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	f.agent.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: "scion-claude:latest"}
	f.agent.AppliedConfig.InlineConfig = &api.ScionConfig{Image: "scion-claude:latest"}

	var asked []string
	fresh, _, err := f.srv.buildPatchedAppliedConfig(ctx, f.agent, f.project, fixedImageRegistry("ghcr.io/org"), func(profile string) string {
		asked = append(asked, profile)
		return "v0.3.0"
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", fresh.Image)
	assert.Equal(t, []string{fresh.Profile}, asked, "resolved once, for the new generation profile")

	fresh, _, err = f.srv.buildPatchedAppliedConfig(ctx, f.agent, f.project, fixedImageRegistry("ghcr.io/org"), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/org/scion-claude:latest", fresh.Image, "no tag: the registry-only form")
}

// TestComputeReincarnationPlan_ImageTagIsAChange: the plan compares
// registry-only, so a generation that moves from latest to the tag shows
// the image change.
func TestComputeReincarnationPlan_ImageTagIsAChange(t *testing.T) {
	old := &store.AgentAppliedConfig{Image: "scion-claude:latest"}
	fresh := &store.AgentAppliedConfig{Image: "ghcr.io/org/scion-claude:v0.3.0"}
	plan := computeReincarnationPlan(old, fresh, nil, "ghcr.io/org")
	assert.Equal(t, "scion-claude:latest", plan.Image.Old)
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", plan.Image.New)

	same := computeReincarnationPlan(old, &store.AgentAppliedConfig{Image: "ghcr.io/org/scion-claude:latest"}, nil, "ghcr.io/org")
	assert.Equal(t, same.Image.New, same.Image.Old, "without a tag the canonical forms still match")
}

// TestSystemImagesPull_ImageTag: POST /api/v1/system/images/pull pulls the
// harness images with the hub-scope tag while the experiment is on, and
// with latest while it is off.
func TestSystemImagesPull_ImageTag(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
		want []string
	}{
		{"experiment on", true, []string{"ghcr.io/org/scion-claude:v0.3.0"}},
		{"experiment off", false, []string{"ghcr.io/org/scion-claude:latest"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The handler builds the runtime from the settings under $HOME
			// (runtime.GetRuntime); keep it off the real home.
			tempSettingsHome(t)
			srv, _, ops := newTestDBServerWithStore(t, createTestStore(t))
			srv.ctx = context.Background()
			srv.events = noopEventPublisher{}
			setTestImageRegistryFileTier(t, ops, "ghcr.io/org", nil)
			setTestImageTagFileTier(t, ops, "v0.3.0", nil)
			setImageTagExperiment(t, ops, tc.on)
			pulled := make(chan []string, 1)
			srv.systemImagePull = func(_ context.Context, _ runtime.Runtime, harnesses []string, registry, tag string, _ func(runtime.PullResult)) error {
				pulled <- runtime.HarnessImages(harnesses, registry, tag)
				return nil
			}

			rr := httptest.NewRecorder()
			req := adminRequest(http.MethodPost, "/api/v1/system/images/pull", `{"harnesses":["claude"]}`)
			req.RemoteAddr = "127.0.0.1:1234"
			srv.handleSystemImagesPull(rr, req)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			select {
			case got := <-pulled:
				assert.Equal(t, tc.want, got)
			case <-time.After(10 * time.Second):
				t.Fatal("the pull did not run")
			}
			require.Eventually(t, func() bool { return !srv.imagePullActive.Load() }, 10*time.Second, 10*time.Millisecond)
		})
	}
}

// TestPutServerConfigFile_ImageTagNeedsExperiment (L2): the settings-file
// save refuses a profile image_tag while the experiment is off, with the
// same 412, and an echo of the stored value still saves.
func TestPutServerConfigFile_ImageTagNeedsExperiment(t *testing.T) {
	srv, _, settingsPath := fileModeGCPIdentityServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		return rr
	}
	before, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	rr := put(`{"profiles":{"local":{"runtime":"docker","image_tag":"v1"}}}`)
	requireImageTagExperimentDisabled(t, rr)
	after, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a refused save writes nothing")

	require.NoError(t, os.WriteFile(settingsPath, append(before, []byte("runtimes:\n  docker:\n    type: docker\nprofiles:\n  local:\n    runtime: docker\n    image_tag: v1\n")...), 0o644))
	rr = put(`{"runtimes":{"docker":{"type":"docker"}},"profiles":{"local":{"runtime":"docker","image_tag":"v1"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, "an echo of the stored value saves: %s", rr.Body.String())
	rr = put(`{"runtimes":{"docker":{"type":"docker"}},"profiles":{"local":{"runtime":"docker","image_tag":"v2"}}}`)
	requireImageTagExperimentDisabled(t, rr)
}
