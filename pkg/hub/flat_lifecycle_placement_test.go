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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// P1.3 part 1 (ptone/scion#3269): placement stays on the saved Runtime Broker
// instance and target across default changes and a service restart.

// restartFlatHub simulates a Hub service restart: the running server is shut
// down and a new one is built over the same store, with a fresh dispatcher
// client. The experiment is set as given.
func restartFlatHub(t *testing.T, f *flatHubFixture, experimentOn bool) {
	t.Helper()
	require.NoError(t, f.srv.Shutdown(context.Background()))
	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.DevUserConfig = DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	srv, err := New(cfg, f.s)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	f.srv = srv
	f.client = &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.s, f.client, false, slog.Default()))
	setFlatExperiment(t, srv, experimentOn)
}

// TestFlatPlacement_StaysOnSavedInstanceAcrossDefaultChangeAndRestart is the
// #3269 acceptance in Hub form: create on the flat Runtime Broker, change
// every future-create default, restart the service, then stop, start and
// restart the agent. Every dispatch goes to the saved Runtime Broker with the
// saved target, and the stored placement never moves.
func TestFlatPlacement_StaysOnSavedInstanceAcrossDefaultChangeAndRestart(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	rec := f.create(t, map[string]interface{}{"name": "placed", "runtimeBrokerId": f.flat.ID, "task": "t"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	a := f.agentBySlug(t, "placed")
	require.NotNil(t, a)
	require.True(t, a.PinValid())

	// Change the future-create defaults: project default, Hub default and
	// project active profile all point elsewhere.
	f.project.DefaultRuntimeBrokerID = f.legacy.ID
	require.NoError(t, f.s.UpdateProject(ctx, f.project))
	setProjectAnnotations(t, f.s, f.project, map[string]string{projectSettingActiveProfile: "local"})

	// Simulated service restart; lifecycle does not depend on the experiment.
	// The Hub default is set on the restarted server (a write before the
	// restart would be replaced by the new server's config).
	restartFlatHub(t, f, false)
	f.srv.mu.Lock()
	f.srv.config.AgentDefaults.DefaultRuntimeBroker = f.legacy.ID
	f.srv.mu.Unlock()

	placement := func(step string) {
		t.Helper()
		got, err := f.s.GetAgent(ctx, a.ID)
		require.NoError(t, err, step)
		assert.Equal(t, f.flat.ID, got.RuntimeBrokerID, "%s: runtime_broker_id", step)
		assert.Equal(t, f.flat.ID, got.PinnedRuntimeBrokerID, "%s: pinned Runtime Broker", step)
		assert.Equal(t, f.flat.RuntimeTarget.ID, got.PinnedRuntimeTargetID, "%s: pinned target", step)
		assert.Equal(t, f.flat.RuntimeTarget.Type, got.PinnedRuntimeTargetType, "%s: pinned target type", step)
	}

	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted, "stop: %d %s", rec.Code, rec.Body.String())
	assert.True(t, f.client.stopCalled)
	assert.Equal(t, f.flat.ID, f.client.lastBrokerID, "stop goes to the saved Runtime Broker")
	placement("after stop")

	got, err := f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Phase = string(state.PhaseStopped)
	require.NoError(t, f.s.UpdateAgent(ctx, got))

	f.client.lastBrokerID = ""
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted, "start: %d %s", rec.Code, rec.Body.String())
	assert.True(t, f.client.startCalled)
	assert.Equal(t, f.flat.ID, f.client.lastBrokerID, "start goes to the saved Runtime Broker")
	assert.Equal(t, f.flat.RuntimeTarget.ID, startExtrasWireKey(f.client.lastStartExtras, "expectedRuntimeTargetId"),
		"start carries the saved target")
	placement("after start")

	f.client.lastBrokerID = ""
	f.client.startCalled = false
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted, "restart: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, f.flat.ID, f.client.lastBrokerID, "restart goes to the saved Runtime Broker")
	assert.Equal(t, f.flat.RuntimeTarget.ID, startExtrasWireKey(f.client.lastStartExtras, "expectedRuntimeTargetId"),
		"restart carries the saved target")
	placement("after restart")

	// The saved Runtime Broker row itself is unchanged by the restart.
	row, err := f.s.GetRuntimeBroker(ctx, f.flat.ID)
	require.NoError(t, err)
	require.NotNil(t, row.RuntimeTarget)
	assert.Equal(t, *f.flat.RuntimeTarget, *row.RuntimeTarget)
}
