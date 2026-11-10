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
	"log/slog"
	"maps"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sealStoredSA = "stored@example-project.iam.gserviceaccount.com"

// sealStoredEnv is a stored env holding SCION_METADATA_* values a merge
// could carry into a dispatch.
func sealStoredEnv() map[string]string {
	return map[string]string{
		"A":                          "1",
		"SCION_METADATA_SA_EMAIL":    sealStoredSA,
		"SCION_METADATA_MODE":        "passthrough",
		"SCION_METADATA_MODE_SOURCE": "hub",
	}
}

// assertSealed checks a sent env: no SCION_METADATA_* value of the stored
// env, and the hub's own source marker.
func assertSealed(t *testing.T, label string, env map[string]string) {
	t.Helper()
	assert.NotEqual(t, sealStoredSA, env["SCION_METADATA_SA_EMAIL"], "%s: stored account not sent", label)
	_, hasMode := env["SCION_METADATA_MODE"]
	assert.False(t, hasMode, "%s: stored mode not sent (the agent has no hub identity)", label)
	assert.Equal(t, "hub", env["SCION_METADATA_MODE_SOURCE"], "%s: the hub's own source marker", label)
	_, hasMarker := env[envNativeCloudRun]
	assert.False(t, hasMarker, "%s: no native marker on a dispatch not decided native", label)
}

// Each send wrapper seals the env it is handed, whatever a merge put in it:
// a stored account, mode and source never reach the transport, and on
// start and restart of an assign agent the hub's own account does.
func TestSendWrappersSealMetadataEnv(t *testing.T) {
	ctx := context.Background()
	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(createTestStore(t), m, false, slog.Default())
	plain := &store.Agent{ID: tid("agent-seal"), RuntimeBrokerID: tid("broker-seal"), AppliedConfig: &store.AgentAppliedConfig{}}

	req := &RemoteCreateAgentRequest{ResolvedEnv: sealStoredEnv()}
	_, _ = d.sendCreate(ctx, plain, "http://broker", req)
	require.NotNil(t, m.lastCreateReq)
	assertSealed(t, "create", m.lastCreateReq.ResolvedEnv)

	req = &RemoteCreateAgentRequest{ResolvedEnv: sealStoredEnv()}
	_, _, _ = d.sendCreateWithGather(ctx, plain, "http://broker", req)
	assertSealed(t, "create with gather", req.ResolvedEnv)

	_, _ = d.sendStart(ctx, plain, "http://broker", "a", "p", "", "", "", "", "", "", sealStoredEnv(), nil, nil, nil, false, false, StartExtras{})
	assertSealed(t, "start", m.lastResolvedEnv)

	_, _ = d.sendRestart(ctx, plain, "http://broker", "a", "p", sealStoredEnv(), StartExtras{})
	assertSealed(t, "restart", m.lastRestartResolvedEnv)

	assign := &store.Agent{ID: tid("agent-seal-assign"), RuntimeBrokerID: tid("broker-seal"), AppliedConfig: &store.AgentAppliedConfig{
		GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountEmail: "hub@example-project.iam.gserviceaccount.com", ProjectID: "hub-project"},
	}}
	env := sealStoredEnv()
	env[envNativeCloudRun] = "1"
	_, _ = d.sendStart(ctx, assign, "http://broker", "a", "p", "", "", "", "", "", "", env, nil, nil, nil, false, false, StartExtras{})
	assert.Equal(t, "hub@example-project.iam.gserviceaccount.com", m.lastResolvedEnv["SCION_METADATA_SA_EMAIL"])
	assert.Equal(t, store.GCPMetadataModeAssign, m.lastResolvedEnv["SCION_METADATA_MODE"])
	_, hasMarker := m.lastResolvedEnv[envNativeCloudRun]
	assert.False(t, hasMarker, "a marker not decided by this dispatch is removed")

	env = sealStoredEnv()
	_, _ = d.sendRestart(withCloudRunNative(ctx), assign, "http://broker", "a", "p", env, StartExtras{})
	assert.Equal(t, "1", m.lastRestartResolvedEnv[envNativeCloudRun], "a dispatch decided native carries the hub's marker")
}

// sealRecordingClient records a copy of every create-with-gather env sent.
type sealRecordingClient struct {
	gatherMockBrokerClient
	sent []map[string]string
}

func (c *sealRecordingClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	c.sent = append(c.sent, maps.Clone(req.ResolvedEnv))
	return c.gatherMockBrokerClient.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
}

// sealGatherSetup stores a broker and two hub as-needed env vars: one under
// an alternative name the broker maps onto SCION_METADATA_SA_EMAIL, and
// GEMINI_API_KEY, so the second pass is sent. It returns a dispatcher on a
// recording client and an agent whose stored env holds metadata values.
func sealGatherSetup(t *testing.T) (*HTTPAgentDispatcher, *sealRecordingClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	hubID := "test-hub-seal"
	broker := &store.RuntimeBroker{ID: tid("broker-seal-gather"), Name: "seal-broker", Slug: "seal-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, broker))
	for key, value := range map[string]string{"SEAL_ALT_ACCOUNT": sealStoredSA, "GEMINI_API_KEY": "gemini"} {
		require.NoError(t, memStore.CreateEnvVar(ctx, &store.EnvVar{
			ID: tid("env-seal-" + key), Key: key, Value: value,
			Scope: store.ScopeHub, ScopeID: hubID, InjectionMode: store.InjectionModeAsNeeded,
		}))
	}
	needs := &RemoteEnvRequirementsResponse{
		AgentID:      "agent-seal-gather",
		Required:     []string{"SCION_METADATA_SA_EMAIL", "GEMINI_API_KEY"},
		HubHas:       []string{},
		Needs:        []string{"SCION_METADATA_SA_EMAIL", "GEMINI_API_KEY"},
		Alternatives: map[string][]string{"SCION_METADATA_SA_EMAIL": {"SEAL_ALT_ACCOUNT"}},
	}
	c := &sealRecordingClient{gatherMockBrokerClient: gatherMockBrokerClient{gatherEnvReqs: needs}}
	d := NewHTTPAgentDispatcherWithClient(memStore, c, false, slog.Default())
	d.SetHubID(hubID)
	agent := &store.Agent{
		ID: tid("agent-seal-gather"), Name: "seal-agent", Slug: "seal-agent",
		ProjectID: "project-seal", OwnerID: "user-seal", RuntimeBrokerID: broker.ID,
		AppliedConfig: &store.AgentAppliedConfig{Env: sealStoredEnv()},
	}
	return d, c, agent
}

// On provision, create with gather and finalize, every send (both passes)
// carries neither a stored SCION_METADATA_* value nor one an as-needed
// alternative maps onto, and the hub's own source marker is present.
func TestDispatchGatherPathsSealMetadataEnv(t *testing.T) {
	ctx := context.Background()
	paths := map[string]func(d *HTTPAgentDispatcher, a *store.Agent) error{
		"provision": func(d *HTTPAgentDispatcher, a *store.Agent) error { return d.DispatchAgentProvision(ctx, a) },
		"create with gather": func(d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreateWithGather(ctx, a)
			return err
		},
		"finalize": func(d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchFinalizeEnv(ctx, a, nil)
			return err
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			d, c, a := sealGatherSetup(t)
			_ = run(d, a)
			require.GreaterOrEqual(t, len(c.sent), 2, "both passes sent")
			for i, env := range c.sent {
				assertSealed(t, name, env)
				_, has := env["SEAL_ALT_ACCOUNT"]
				assert.False(t, has, "send %d: the alternative name is not sent", i)
			}
			assert.Equal(t, "gemini", c.sent[len(c.sent)-1]["GEMINI_API_KEY"], "the second pass carries the resolved key")
		})
	}
}

// An as-needed value stored under an alternative name never satisfies a
// reserved canonical key.
func TestResolveAsNeededForKeys_ReservedResultKeyRefused(t *testing.T) {
	ctx := context.Background()
	d, _, a := sealGatherSetup(t)
	got := mustResolveAsNeeded(t, d, ctx, a, []string{"SCION_METADATA_SA_EMAIL", "GEMINI_API_KEY"},
		map[string][]string{"SCION_METADATA_SA_EMAIL": {"SEAL_ALT_ACCOUNT"}})
	_, has := got["SCION_METADATA_SA_EMAIL"]
	assert.False(t, has, "not stored under the reserved canonical key")
	_, has = got["SEAL_ALT_ACCOUNT"]
	assert.False(t, has)
	assert.Equal(t, "gemini", got["GEMINI_API_KEY"])
}

// A create carries the metadata mode of its own GCPIdentity config: the
// send seal reads it from the request, so a create with an assign or a
// block identity sends that mode and the hub's source marker, whatever
// the agent struct the wrapper is handed carries.
func TestSendCreateSealsRequestIdentityMode(t *testing.T) {
	ctx := context.Background()
	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(createTestStore(t), m, false, slog.Default())
	bare := &store.Agent{ID: tid("agent-seal-mode"), RuntimeBrokerID: tid("broker-seal")}
	for _, mode := range []string{store.GCPMetadataModeAssign, store.GCPMetadataModeBlock} {
		req := &RemoteCreateAgentRequest{
			Config:      &RemoteAgentConfig{GCPIdentity: &RemoteGCPIdentityConfig{MetadataMode: mode, SAEmail: "hub@example-project.iam.gserviceaccount.com"}},
			ResolvedEnv: sealStoredEnv(),
		}
		_, _ = d.sendCreate(ctx, bare, "http://broker", req)
		require.NotNil(t, m.lastCreateReq)
		assert.Equal(t, mode, m.lastCreateReq.ResolvedEnv["SCION_METADATA_MODE"], mode)
		assert.Equal(t, "hub", m.lastCreateReq.ResolvedEnv["SCION_METADATA_MODE_SOURCE"], mode)
		_, hasSA := m.lastCreateReq.ResolvedEnv["SCION_METADATA_SA_EMAIL"]
		assert.False(t, hasSA, "a create carries the account in its config")
	}
}

// End to end on every create path: an agent with an assign or a block
// identity sends that mode and the hub's source marker.
func TestDispatchCreatePathsSendIdentityMode(t *testing.T) {
	ctx := context.Background()
	local := []store.BrokerProfile{{Name: "local", Type: "docker"}}
	for _, mode := range []string{store.GCPMetadataModeAssign, store.GCPMetadataModeBlock} {
		for _, op := range precheckOps(ctx) {
			if op.name == "start" || op.name == "restart" {
				continue
			}
			t.Run(mode+" "+op.name, func(t *testing.T) {
				gcpID := &store.GCPIdentityConfig{MetadataMode: mode}
				if mode == store.GCPMetadataModeAssign {
					gcpID.ServiceAccountEmail = "hub@example-project.iam.gserviceaccount.com"
				}
				d, m, a := crDispatcher(t, local, "local", gcpID, false)
				_ = op.run(d, a)
				require.True(t, op.called(m))
				require.NotNil(t, m.lastCreateReq)
				assert.Equal(t, mode, m.lastCreateReq.ResolvedEnv["SCION_METADATA_MODE"])
				assert.Equal(t, "hub", m.lastCreateReq.ResolvedEnv["SCION_METADATA_MODE_SOURCE"])
			})
		}
	}
}
