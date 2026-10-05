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
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The heartbeat stores the export identity marker (inside the descriptor)
// and the default profile; a heartbeat that omits either (an older broker)
// keeps the stored value, and an explicit empty default profile clears it.
func TestBrokerHeartbeat_ExportIDAndDefaultProfileRoundTrip(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-facts-heartbeat"),
		Name:   "Facts Heartbeat Broker",
		Slug:   "facts-heartbeat-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"
	get := func() *store.RuntimeBroker {
		t.Helper()
		b, err := s.GetRuntimeBroker(ctx, broker.ID)
		require.NoError(t, err)
		return b
	}

	desc := moveFixtureStorage()
	rec := doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{
		Status: "online", WorkspaceStorage: desc, DefaultProfile: strPtr("k8s"),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := get()
	require.NotNil(t, got.WorkspaceStorage)
	assert.Equal(t, moveTestExportID, got.WorkspaceStorage.NFS.ExportID)
	assert.Equal(t, "k8s", got.DefaultProfile)

	// An older broker: neither field.
	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = get()
	assert.Equal(t, moveTestExportID, got.WorkspaceStorage.NFS.ExportID, "an omitted descriptor keeps the marker")
	assert.Equal(t, "k8s", got.DefaultProfile, "an omitted default profile keeps the stored one")

	// The default profile alone is refreshed, and a marker change is too.
	changed := moveFixtureStorage()
	changed.NFS.ExportID = "0a0a0a0a-0000-4000-8000-000000000000"
	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", DefaultProfile: strPtr("gke")})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "gke", get().DefaultProfile)
	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", WorkspaceStorage: changed})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = get()
	assert.Equal(t, changed.NFS.ExportID, got.WorkspaceStorage.NFS.ExportID)
	assert.Equal(t, "gke", got.DefaultProfile)

	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", DefaultProfile: strPtr("")})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "", get().DefaultProfile, "an explicit empty default profile clears it")
}

// Join stores the reported default profile; a join that omits it (an older
// broker, or unreadable settings) keeps the stored value.
func TestCompleteBrokerJoin_DefaultProfile(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()

	join := func(name string, defaultProfile *string, before func(b *store.RuntimeBroker)) *store.RuntimeBroker {
		t.Helper()
		reg, err := svc.CreateBrokerRegistration(ctx, CreateBrokerRegistrationRequest{Name: name}, "admin-user-id")
		require.NoError(t, err)
		if before != nil {
			b, err := s.GetRuntimeBroker(ctx, reg.BrokerID)
			require.NoError(t, err)
			before(b)
			require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
		}
		_, err = svc.CompleteBrokerJoin(ctx, BrokerJoinRequest{
			BrokerID: reg.BrokerID, JoinToken: reg.JoinToken, Hostname: name, Version: "1.0.0",
			Capabilities: []string{"sync"}, DefaultProfile: defaultProfile,
		}, "http://localhost:9810")
		require.NoError(t, err)
		b, err := s.GetRuntimeBroker(ctx, reg.BrokerID)
		require.NoError(t, err)
		return b
	}

	assert.Equal(t, "gke", join("dp-join", strPtr("gke"), nil).DefaultProfile)
	assert.Equal(t, "kept", join("dp-rejoin-old", nil, func(b *store.RuntimeBroker) { b.DefaultProfile = "kept" }).DefaultProfile)
}

// newPlacementDispatcher returns a dispatcher over the reincarnate test
// server's store, with a stored agent on its broker.
func newPlacementDispatcher(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, store.Store, *store.Agent) {
	t.Helper()
	_, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	client := &mockRuntimeBrokerClient{}
	return NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()), client, s, agent
}

func startResponseWithPlacement(agentID, placement string) *RemoteAgentResponse {
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{
		ID: agentID, Name: agentID, Phase: string(state.PhaseRunning), WorkspacePlacement: placement,
	}}
}

// A start records the placement its broker reports; the next start
// (a re-provision) overwrites it; a start answered without a placement (an
// older broker) and a stale whole-row write leave it alone.
func TestDispatchAgentStart_RecordsAndOverwritesWorkspacePlacement(t *testing.T) {
	d, client, s, agent := newPlacementDispatcher(t)
	ctx := context.Background()
	placement := func() string {
		t.Helper()
		a, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		return a.WorkspacePlacement
	}
	require.Equal(t, "", placement(), "no placement before any start reports one")
	stale, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	client.startReturnResp = startResponseWithPlacement(agent.ID, api.WorkspacePlacementExport)
	require.NoError(t, d.DispatchAgentStart(ctx, agent, "", false))
	assert.Equal(t, api.WorkspacePlacementExport, placement())
	assert.Equal(t, api.WorkspacePlacementExport, agent.WorkspacePlacement, "the in-memory agent mirrors the report")

	// A whole-row write from a copy read before the report does not clobber it.
	stale.Message = "unrelated update"
	stale.StateVersion = 0
	latest, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	stale.StateVersion = latest.StateVersion
	require.NoError(t, s.UpdateAgent(ctx, stale))
	assert.Equal(t, api.WorkspacePlacementExport, placement(), "UpdateAgent never writes the placement")

	// Re-provisioned onto local storage: overwritten.
	client.startReturnResp = startResponseWithPlacement(agent.ID, api.WorkspacePlacementLocal)
	require.NoError(t, d.DispatchAgentStart(ctx, agent, "", false))
	assert.Equal(t, api.WorkspacePlacementLocal, placement())

	// An answer without a placement records nothing.
	client.startReturnResp = startResponseWithPlacement(agent.ID, "")
	require.NoError(t, d.DispatchAgentStart(ctx, agent, "", false))
	assert.Equal(t, api.WorkspacePlacementLocal, placement())

	// A malformed placement is not recorded.
	client.startReturnResp = startResponseWithPlacement(agent.ID, "bad\nvalue")
	require.NoError(t, d.DispatchAgentStart(ctx, agent, "", false))
	assert.Equal(t, api.WorkspacePlacementLocal, placement())
}

// A provision-only dispatch (no start) records no placement.
func TestDispatchAgentProvision_RecordsNoWorkspacePlacement(t *testing.T) {
	d, _, s, agent := newPlacementDispatcher(t)
	ctx := context.Background()
	require.NoError(t, s.SetAgentWorkspacePlacement(ctx, agent.ID, api.WorkspacePlacementExport))
	require.NoError(t, d.DispatchAgentProvision(ctx, agent))
	a, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, api.WorkspacePlacementExport, a.WorkspacePlacement)
}

// An async launch's succeeded report records the placement of its start.
func TestAgentLaunchReport_SucceededRecordsWorkspacePlacement(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("lr-wp-project"), Slug: "lr-wp-project", Name: "LR WP Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("lr-wp-agent"), Slug: "lr-wp-agent", Name: "LR WP Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	placement := func() string {
		t.Helper()
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		return got.WorkspacePlacement
	}

	// A succeeded report the store rejects (a stale launch ID) records nothing.
	rec := postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: "stale-launch", InstanceID: "i1", State: "succeeded",
		Agent: &RemoteAgentInfo{ID: agent.ID, WorkspacePlacement: api.WorkspacePlacementLocal},
	})
	require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "", placement(), "a rejected report must not record a placement")

	rec = postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "succeeded",
		Agent: &RemoteAgentInfo{ID: agent.ID, WorkspacePlacement: api.WorkspacePlacementExport},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, api.WorkspacePlacementExport, placement())

	// The same report again is not applied (duplicate): it records nothing.
	rec = postLaunchReport(t, srv, "broker-1", agent.ID, "broker-1", AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: "succeeded",
		Agent: &RemoteAgentInfo{ID: agent.ID, WorkspacePlacement: api.WorkspacePlacementLocal},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"applied"`, "the repeat must not be applied")
	assert.Equal(t, api.WorkspacePlacementExport, placement(), "a report the store did not apply must not record a placement")
}

// A move dry run between brokers whose heartbeats carried no marker or
// default profile (older brokers) is refused, never a crash.
func TestReincarnateMove_OldBrokerHeartbeatFactsRefused(t *testing.T) {
	f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
		dst.WorkspaceStorage.NFS.ExportID = ""
		dst.DefaultProfile = ""
	})
	count := f.agentCount(t)
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	_, _, v := decodeMoveRefusal(t, rec)
	assertVerdictFailedAt(t, v, moveCheckSameExport)
	f.assertNoMoveSideEffects(t, count)
}
