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
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests that asynchronous agent create follows the hub.async_agent_launch
// experiment (ptone/scion#4991), read at each create send.

// asyncLaunchToggle overrides hub.async_agent_launch on a Server the way an
// admin toggle would, and can flip it later on the same Server.
type asyncLaunchToggle struct {
	t     *testing.T
	store *fakeHubSettingStore
	ops   *OperationalSettings
}

// newAsyncLaunchToggle installs operational settings with no experiment
// overrides on srv, so every experiment starts at its registry default.
func newAsyncLaunchToggle(t *testing.T, srv *Server) *asyncLaunchToggle {
	t.Helper()
	g := &asyncLaunchToggle{t: t, store: newFakeHubSettingStore()}
	g.ops = NewOperationalSettings(g.store, emptyKoanf(), emptyKoanf())
	g.seed(`{"overrides":{}}`)
	srv.SetOperationalSettings(g.ops)
	return g
}

func (g *asyncLaunchToggle) set(enabled bool) {
	g.t.Helper()
	g.seed(fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.AsyncAgentLaunch, enabled))
}

func (g *asyncLaunchToggle) seed(doc string) {
	g.t.Helper()
	g.store.seed("experiments", json.RawMessage(doc))
	_, err := g.ops.Refresh(context.Background())
	require.NoError(g.t, err)
}

func TestAsyncLaunchSettings_FollowsExperiment(t *testing.T) {
	srv, _ := testServer(t)
	toggle := newAsyncLaunchToggle(t, srv)

	assert.True(t, srv.asyncLaunchSettings().Enabled, "on by default (registry default, ptone/scion#6640)")

	toggle.set(false)
	assert.False(t, srv.asyncLaunchSettings().Enabled, "off once an admin turns the experiment off")

	// Same Server, no restart: the next read sees the change.
	toggle.set(true)
	assert.True(t, srv.asyncLaunchSettings().Enabled, "on again once the experiment is turned on")
}

// newExperimentAsyncCreateServer is newAsyncCreateServer with the server's
// own async launch settings accessor instead of a fixed flag, so the create
// path is decided by the experiment.
func newExperimentAsyncCreateServer(t *testing.T) (*Server, *store.Project, *asyncLaunchClient, *asyncLaunchToggle) {
	t.Helper()
	ctx := context.Background()
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-create"))
	require.NoError(t, err)
	broker.Endpoint = "http://localhost:9800"
	broker.Capabilities = &store.BrokerCapabilities{AsyncLaunch: true}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	client := &asyncLaunchClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(srv.asyncLaunchSettings)
	srv.SetDispatcher(d)
	return srv, project, client, newAsyncLaunchToggle(t, srv)
}

func TestCreateAgent_AsyncLaunchFollowsExperiment(t *testing.T) {
	create := func(t *testing.T, srv *Server, project *store.Project, name string) {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": name, "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	t.Run("no override sends an asynchronous create (default on)", func(t *testing.T) {
		srv, project, client, _ := newExperimentAsyncCreateServer(t)
		create(t, srv, project, "exp-default")
		require.Len(t, client.sends, 1)
		assert.True(t, client.sends[0].AsyncLaunch)
		assert.NotEmpty(t, client.sends[0].LaunchID)
	})

	t.Run("experiment off sends a synchronous create", func(t *testing.T) {
		srv, project, client, toggle := newExperimentAsyncCreateServer(t)
		toggle.set(false)
		create(t, srv, project, "exp-off")
		require.Len(t, client.sends, 1)
		assert.False(t, client.sends[0].AsyncLaunch)
		assert.Empty(t, client.sends[0].LaunchID)
	})

	t.Run("experiment on sends an asynchronous create", func(t *testing.T) {
		srv, project, client, toggle := newExperimentAsyncCreateServer(t)
		toggle.set(true)
		create(t, srv, project, "exp-on")
		require.Len(t, client.sends, 1)
		assert.True(t, client.sends[0].AsyncLaunch)
		assert.NotEmpty(t, client.sends[0].LaunchID)
	})

	t.Run("turning it off at runtime makes the next create synchronous", func(t *testing.T) {
		srv, project, client, toggle := newExperimentAsyncCreateServer(t)
		toggle.set(true)
		create(t, srv, project, "exp-flip-a")
		toggle.set(false)
		create(t, srv, project, "exp-flip-b")
		require.Len(t, client.sends, 2)
		assert.True(t, client.sends[0].AsyncLaunch)
		assert.False(t, client.sends[1].AsyncLaunch)
	})
}
