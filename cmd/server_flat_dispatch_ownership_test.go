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

package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestFlatOwnership_RealHubDispatchCarriesBothIDs: the Hub's real create,
// start and restart dispatch (HTTPAgentDispatcher over the authenticated
// broker client) reaches a flat instance with the project ID and the
// immutable Hub agent ID, which the instance records as the ownership key
// (ptone/scion#3274). The slug is never the key.
func TestFlatOwnership_RealHubDispatchCarriesBothIDs(t *testing.T) {
	isolateAgentFiles(t)
	ctx := context.Background()
	s := newTestStore(t)
	srv := flatAuthTestHub(t, s)
	globalDir := t.TempDir()
	stateDirs := map[string]string{}
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: globalDir,
		Instances: []config.V1RuntimeBrokerInstanceConfig{{Key: "docker-a", Name: "a", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}},
		Mode:      brokerhost.ModeColocated,
		NewRuntime: func(context.Context, config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			return &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, nil
		},
		ProbeScope: func(context.Context, config.V1RuntimeBrokerInstanceConfig, runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-a"}}, nil
		},
		Activator: &colocatedFlatActivator{hubSrv: srv, endpoint: "http://localhost:9800"},
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = ic.Identity.RuntimeBrokerID
			cfg.StateDir = filepath.Join(globalDir, "state", ic.Identity.RuntimeBrokerID)
			stateDirs[ic.Identity.RuntimeBrokerID] = cfg.StateDir
			cfg.HubEnabled = true
			cfg.HubEndpoint = "http://127.0.0.1:1"
			cfg.InMemoryCredentials = ic.Activation.InMemoryCredentials
			cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
			return runtimebroker.New(cfg, ic.Manager, ic.Runtime), nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(ctx))
	require.Len(t, h.Active(), 1)
	id := h.Active()[0].Context.Identity
	listener := httptest.NewServer(h.Handler())
	t.Cleanup(listener.Close)

	// The Hub's row for the instance points at its instance route on the
	// test listener.
	row, err := s.GetRuntimeBroker(ctx, id.RuntimeBrokerID)
	require.NoError(t, err)
	row.Endpoint = listener.URL + brokerhost.InstancePrefix(id.RuntimeBrokerID)
	require.NoError(t, s.UpdateRuntimeBroker(ctx, row))

	project := &store.Project{ID: api.NewUUID(), Name: "Dispatch", Slug: "dispatch"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{ID: api.NewUUID(), Slug: "worker", Name: "worker", ProjectID: project.ID, RuntimeBrokerID: id.RuntimeBrokerID,
		PinnedRuntimeBrokerID: id.RuntimeBrokerID, PinnedRuntimeTargetID: id.RuntimeTarget.ID, PinnedRuntimeTargetType: id.RuntimeTarget.Type}
	require.NoError(t, s.CreateAgent(ctx, agent))
	agent, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.True(t, agent.IsPinned(), "the agent is pinned to the instance's target")

	disp := hub.NewHTTPAgentDispatcherWithClient(s, hub.NewAuthenticatedBrokerClient(s, false), false, nil)
	records := runtimebroker.NewOwnershipStore(stateDirs[id.RuntimeBrokerID], id.RuntimeBrokerID)
	runs := func() int {
		rec, ok, err := records.Get(project.ID, agent.ID)
		require.NoError(t, err)
		require.True(t, ok, "the instance recorded ownership under (project ID, Hub agent ID)")
		assert.Equal(t, "worker", rec.AgentSlug)
		return len(rec.Runs)
	}

	// Create: the dispatch may fail later in this fixture (no real
	// workspace), but ownership is recorded first, keyed by both IDs.
	_, _ = disp.DispatchAgentCreate(ctx, agent)
	assert.Equal(t, 1, runs(), "create")
	_, slugKeyed, _ := records.Get(project.ID, "worker")
	assert.False(t, slugKeyed, "the slug is never the ownership key")

	_ = disp.DispatchAgentStart(ctx, agent, "", false)
	assert.Equal(t, 2, runs(), "start carries both IDs")

	_ = disp.DispatchAgentRestart(ctx, agent)
	assert.Equal(t, 3, runs(), "restart carries both IDs")
}

// storeSigner signs a broker request with the target broker's stored
// secret, as the Hub's own signer does.
type storeSigner struct{ s store.Store }

func (g storeSigner) Sign(ctx context.Context, req *http.Request, brokerID string) error {
	secret, err := g.s.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		return err
	}
	return (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret.SecretKey}).ApplyAuth(req)
}

// TestFlatOwnership_RealControlChannelDispatchCarriesBothIDs: the same
// create, start and restart dispatch over the control channel reaches the
// instance with both IDs.
func TestFlatOwnership_RealControlChannelDispatchCarriesBothIDs(t *testing.T) {
	r := newCCTestRig(t)
	ctx := r.ctx
	a := r.active[0].Context.Identity
	project := &store.Project{ID: api.NewUUID(), Name: "Dispatch", Slug: "dispatch"}
	require.NoError(t, r.s.CreateProject(ctx, project))
	agent := &store.Agent{ID: api.NewUUID(), Slug: "worker", Name: "worker", ProjectID: project.ID, RuntimeBrokerID: a.RuntimeBrokerID,
		PinnedRuntimeBrokerID: a.RuntimeBrokerID, PinnedRuntimeTargetID: a.RuntimeTarget.ID, PinnedRuntimeTargetType: a.RuntimeTarget.Type}
	require.NoError(t, r.s.CreateAgent(ctx, agent))
	agent, err := r.s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	disp := hub.NewHTTPAgentDispatcherWithClient(r.s, hub.NewControlChannelBrokerClient(r.mgr, storeSigner{r.s}, false), false, nil)
	records := runtimebroker.NewOwnershipStore(r.stateDirs[a.RuntimeBrokerID], a.RuntimeBrokerID)
	runs := func() int {
		rec, ok, err := records.Get(project.ID, agent.ID)
		require.NoError(t, err)
		require.True(t, ok, "recorded under (project ID, Hub agent ID) over the control channel")
		return len(rec.Runs)
	}
	_, _ = disp.DispatchAgentCreate(ctx, agent)
	assert.Equal(t, 1, runs(), "create")
	_ = disp.DispatchAgentStart(ctx, agent, "", false)
	assert.Equal(t, 2, runs(), "start")
	_ = disp.DispatchAgentRestart(ctx, agent)
	assert.Equal(t, 3, runs(), "restart")

	// The other instance recorded nothing.
	other := r.active[1].Context.Identity
	_, ok, err := runtimebroker.NewOwnershipStore(r.stateDirs[other.RuntimeBrokerID], other.RuntimeBrokerID).Get(project.ID, agent.ID)
	require.NoError(t, err)
	assert.False(t, ok)
}

// isolateAgentFiles points HOME and the working directory at temporary
// directories, so an agent the test provisions through a real manager never
// lands in the repository's own .scion.
func isolateAgentFiles(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { _ = os.Chdir(wd) })
}
