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
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// gcpIdentityDispatchFixture is shared with pkg/runtimebroker: this package
// pins that the hub's real create and start dispatch output matches it, and
// the broker's TestBuildStartContext_HubGCPIdentityDispatchFixture feeds the
// same file into buildStartContext. Together they cover the hub-to-broker
// GCP identity contract end to end without exporting either side's
// unexported request builders.
var gcpIdentityDispatchFixture = filepath.Join("..", "runtimebroker", "testdata", "hub_gcp_identity_dispatch.json")

var updateGCPIdentityDispatchFixture = flag.Bool("update-gcp-identity-fixture", false,
	"rewrite "+gcpIdentityDispatchFixture+" from the hub's current dispatch output")

// gcpIdentityDispatchCase is the GCP identity slice of one agent's create
// and start dispatch: the create request's Config.GCPIdentity mode (empty
// when that struct is nil) and the SCION_METADATA_* keys of each path's
// resolvedEnv. Nothing else from the requests is recorded, so unrelated
// dispatch changes do not touch the fixture.
type gcpIdentityDispatchCase struct {
	Create struct {
		GCPIdentityMode string            `json:"gcpIdentityMode,omitempty"`
		ResolvedEnv     map[string]string `json:"resolvedEnv"`
	} `json:"create"`
	Start struct {
		ResolvedEnv map[string]string `json:"resolvedEnv"`
	} `json:"start"`
}

func metadataEnvSlice(env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		if strings.HasPrefix(k, "SCION_METADATA_") {
			out[k] = v
		}
	}
	return out
}

// TestDispatch_GCPIdentityRuntimeDefault_MatchesBrokerFixture creates agents
// through the real create handler for each way an agent can end up with no
// identity or an explicit "block", then records what the real dispatcher
// sends the broker on create (buildCreateRequest) and start/restart
// (buildStartEnv). The output must match the shared fixture byte for byte.
//
//   - no_identity: no agent, project or hub setting. The hub sends no
//     SCION_METADATA_MODE, so the broker applies its runtime default.
//   - agent_block, project_default_block, hub_default_block: an explicit
//     "block" at each rung. The hub sends it explicitly.
//   - project_default_assign_without_sa: a stored project default of
//     "assign" with no service account selected. It stays an explicit
//     "block" (fail closed) rather than following the runtime default.
func TestDispatch_GCPIdentityRuntimeDefault_MatchesBrokerFixture(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *bypassAgentsFixture) CreateAgentRequest
	}{
		{"no_identity", func(t *testing.T, f *bypassAgentsFixture) CreateAgentRequest {
			return CreateAgentRequest{}
		}},
		{"agent_block", func(t *testing.T, f *bypassAgentsFixture) CreateAgentRequest {
			return CreateAgentRequest{GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModeBlock}}
		}},
		{"project_default_block", func(t *testing.T, f *bypassAgentsFixture) CreateAgentRequest {
			setProjectGCPIdentityDefault(t, f, store.GCPMetadataModeBlock)
			return CreateAgentRequest{}
		}},
		{"hub_default_block", func(t *testing.T, f *bypassAgentsFixture) CreateAgentRequest {
			setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultGCPIdentityMode: store.GCPMetadataModeBlock})
			return CreateAgentRequest{}
		}},
		{"project_default_assign_without_sa", func(t *testing.T, f *bypassAgentsFixture) CreateAgentRequest {
			setProjectGCPIdentityDefault(t, f, store.GCPMetadataModeAssign)
			return CreateAgentRequest{}
		}},
	}

	got := map[string]gcpIdentityDispatchCase{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := bypassAgentsSetup(t)
			req := tc.setup(t, f)
			req.Name = strings.ReplaceAll(tc.name, "_", "-")

			rec := createAgentAsOwner(t, f, req)
			require.Equal(t, http.StatusCreated, rec.Code, "create: %s", rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			agent, err := f.store.GetAgent(ctx, resp.Agent.ID)
			require.NoError(t, err)

			d := NewHTTPAgentDispatcherWithClient(f.store, &mockRuntimeBrokerClient{}, false, slog.Default())
			createReq, err := d.buildCreateRequest(ctx, agent, "test")
			require.NoError(t, err)
			startEnv, err := d.buildStartEnv(ctx, agent, "test", mintSiteStart)
			require.NoError(t, err)

			var c gcpIdentityDispatchCase
			if createReq.Config != nil && createReq.Config.GCPIdentity != nil {
				c.Create.GCPIdentityMode = createReq.Config.GCPIdentity.MetadataMode
			}
			c.Create.ResolvedEnv = metadataEnvSlice(createReq.ResolvedEnv)
			c.Start.ResolvedEnv = metadataEnvSlice(startEnv.env)
			got[tc.name] = c
		})
	}
	if t.Failed() {
		return
	}

	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	blob, err := json.MarshalIndent(got, "", "  ")
	require.NoError(t, err)
	blob = append(blob, '\n')

	if *updateGCPIdentityDispatchFixture {
		require.NoError(t, os.WriteFile(gcpIdentityDispatchFixture, blob, 0o644))
		return
	}
	want, err := os.ReadFile(gcpIdentityDispatchFixture)
	require.NoError(t, err)
	if !bytes.Equal(want, blob) {
		t.Fatalf("hub GCP identity dispatch output drifted from %s (cases %v).\n"+
			"The broker test feeds this fixture into buildStartContext, so review the change, "+
			"then rerun with -update-gcp-identity-fixture.\n--- want\n%s\n--- got\n%s",
			gcpIdentityDispatchFixture, names, want, blob)
	}
}

// setProjectGCPIdentityDefault writes a project default GCP identity mode
// straight to the project's annotations, with no service account. This
// stands in for stored data: the settings endpoint itself rejects "assign"
// without a service account.
func setProjectGCPIdentityDefault(t *testing.T, f *bypassAgentsFixture, mode string) {
	t.Helper()
	ctx := context.Background()
	p, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[projectSettingDefaultGCPIdentityMode] = mode
	require.NoError(t, f.store.UpdateProject(ctx, p))
}

// TestDispatch_NoGCPIdentity_DropsStoredMetadataEnvOnCreateAndStart pins
// that, with no GCP identity configured, stored env vars named
// SCION_METADATA_MODE or SCION_METADATA_REQUIRE_LOCAL_RUNTIME reach neither
// the create nor the start dispatch. The broker reads both from resolvedEnv
// when the request carries no GCPIdentity struct, so only the hub's own
// decision may set them.
func TestDispatch_NoGCPIdentity_DropsStoredMetadataEnvOnCreateAndStart(t *testing.T) {
	ctx := context.Background()
	f := bypassAgentsSetup(t)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "no-identity-stored-env"})
	require.Equal(t, http.StatusCreated, rec.Code, "create: %s", rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := f.store.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.Nil(t, agent.AppliedConfig.GCPIdentity)

	for k, v := range map[string]string{
		"SCION_METADATA_MODE":                  store.GCPMetadataModePassthrough,
		"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
	} {
		_, err := f.store.UpsertEnvVar(ctx, &store.EnvVar{
			ID:            uuid.New().String(),
			Key:           k,
			Value:         v,
			Scope:         store.ScopeUser,
			ScopeID:       agent.OwnerID,
			InjectionMode: store.InjectionModeAlways,
		})
		require.NoError(t, err)
	}
	// The same names in the agent's own config env, which seeds both paths.
	agent.AppliedConfig.Env = map[string]string{
		"SCION_METADATA_MODE":                  store.GCPMetadataModePassthrough,
		"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
	}

	d := NewHTTPAgentDispatcherWithClient(f.store, &mockRuntimeBrokerClient{}, false, slog.Default())
	createReq, err := d.buildCreateRequest(ctx, agent, "test")
	require.NoError(t, err)
	startEnv, err := d.buildStartEnv(ctx, agent, "test", mintSiteStart)
	require.NoError(t, err)

	for label, env := range map[string]map[string]string{
		"create": createReq.ResolvedEnv,
		"start":  startEnv.env,
	} {
		require.Equal(t, map[string]string{"SCION_METADATA_MODE_SOURCE": "hub"}, metadataEnvSlice(env), label)
	}
	for label, cls := range map[string]map[string]api.EnvKind{
		"create": createReq.EnvClassifications,
		"start":  startEnv.classifications,
	} {
		require.NotContains(t, cls, "SCION_METADATA_MODE", label)
		require.NotContains(t, cls, "SCION_METADATA_REQUIRE_LOCAL_RUNTIME", label)
	}
}
