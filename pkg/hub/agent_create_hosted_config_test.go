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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostedSocketVolume is a member-written host bind that no Runtime Broker
// control path covers.
var hostedSocketVolume = api.VolumeMount{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}

func decodeHostedRefusal(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp.Error.Code, resp.Error.Message
}

// TestCreateAgent_HostedRefusesMemberVolumes: on a hosted hub, a create
// whose config names a host bind, an NFS server or another non-gcs volume
// is refused (400 hosted_config_refused naming the target) and creates
// and dispatches nothing.
func TestCreateAgent_HostedRefusesMemberVolumes(t *testing.T) {
	for name, vol := range map[string]api.VolumeMount{
		"socket bind":       hostedSocketVolume,
		"etc bind":          {Source: "/etc", Target: "/host-etc", ReadOnly: true},
		"nfs":               {Type: "nfs", Server: "10.1.2.3", Source: "/export", Target: "/n"},
		"gke shared volume": {Type: "gke-shared-volume", VolumeName: "other-project", Target: "/g"},
		"gcs not canonical": {Type: "GCS", Bucket: "b", Target: "/b"},
	} {
		t.Run(name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			require.False(t, srv.workstation, "test servers run hosted")
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "hosted-vol-agent", ProjectID: project.ID,
				Config: &api.ScionConfig{Volumes: []api.VolumeMount{vol}},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			code, msg := decodeHostedRefusal(t, rec)
			assert.Equal(t, hostedguard.ErrorCode, code)
			assert.True(t, strings.HasPrefix(msg, `volume "`+vol.Target+`": `), msg)
			assert.Contains(t, msg, "in agent config")
			for _, v := range []string{"10.1.2.3", "/export", "other-project", `"/etc"`} {
				assert.NotContains(t, msg, v, "the refusal never echoes a source")
			}
			assert.Nil(t, disp.capturedAgent, "nothing dispatched")
			_, err := s.GetAgentBySlug(context.Background(), project.ID, "hosted-vol-agent")
			assert.ErrorIs(t, err, store.ErrNotFound, "a refused create stores nothing")
		})
	}
}

// TestCreateAgent_HostedLetsGCSThrough: the hub does not know the runtime
// the agent will run in, so a gcs volume is left to the broker.
func TestCreateAgent_HostedLetsGCSThrough(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "hosted-gcs-agent", ProjectID: project.ID,
		Config: &api.ScionConfig{Volumes: []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: "/b"}}},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestCreateAgent_WorkstationKeepsMemberVolumes: a workstation hub stores
// and dispatches a host bind unchanged.
func TestCreateAgent_WorkstationKeepsMemberVolumes(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.workstation = true
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "workstation-vol-agent", ProjectID: project.ID,
		Config: &api.ScionConfig{Volumes: []api.VolumeMount{hostedSocketVolume}},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	got, err := s.GetAgentBySlug(context.Background(), project.ID, "workstation-vol-agent")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig.InlineConfig)
	assert.Equal(t, []api.VolumeMount{hostedSocketVolume}, got.AppliedConfig.InlineConfig.Volumes)
	require.NotNil(t, disp.capturedAgent, "dispatched")
}

// hostedPrecheckDispatcher is a dispatcher in hosted mode hosted with one
// stored agent whose inline config holds vols.
func hostedPrecheckDispatcher(t *testing.T, hosted bool, vols []api.VolumeMount) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	s := createTestStore(t)
	agent := createHostedPrecheckAgent(t, s, vols)
	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, m, false, slog.Default())
	d.SetHostedMode(hosted)
	return d, m, agent
}

// createHostedPrecheckAgent stores a broker, a project and one agent whose
// inline config holds vols in s, and returns the agent.
func createHostedPrecheckAgent(t *testing.T, s store.Store, vols []api.VolumeMount) *store.Agent {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("hosted-broker"), Name: "hosted-broker", Slug: "hosted-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	project := &store.Project{ID: tid("hosted-project"), Name: "hosted", Slug: "hosted"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("hosted-agent"), Name: "hosted-agent", Slug: "hosted-agent",
		ProjectID: project.ID, RuntimeBrokerID: tid("hosted-broker"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			InlineConfig:  &api.ScionConfig{Harness: "claude", Volumes: vols},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	return agent
}

// TestCreateAuthenticatedDispatcher_HostedMode: the production dispatcher
// of a hosted hub is in hosted mode and refuses a host volume in an agent's
// stored inline config; that of a workstation hub is not and does not.
func TestCreateAuthenticatedDispatcher_HostedMode(t *testing.T) {
	agent := &store.Agent{
		ID: "a1", AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{Volumes: []api.VolumeMount{hostedSocketVolume}},
		},
	}
	for _, workstation := range []bool{false, true} {
		cfg := testServerConfig()
		cfg.Workstation = workstation
		srv, _ := testServerWithStoreConfig(t, createTestStore(t), cfg)
		d := srv.CreateAuthenticatedDispatcher()
		assert.Equal(t, !workstation, d.hostedMode, "workstation=%v", workstation)
		err := d.hostedConfigPrecheck(context.Background(), agent)
		var refusal *RuntimeTargetRefusal
		refused := errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode
		assert.Equal(t, !workstation, refused, "workstation=%v: %v", workstation, err)
	}
}

// TestDispatch_HostedConfigPrecheck: on a hosted hub every dispatch path
// refuses an agent whose stored inline config holds a host volume (config
// stored before the check existed), with a 400 hosted_config_refused and
// no broker call; a workstation hub, and a hosted hub with nothing to
// refuse, are not refused for it.
func TestDispatch_HostedConfigPrecheck(t *testing.T) {
	ctx := context.Background()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" refuses stored host volumes", func(t *testing.T) {
			d, m, a := hostedPrecheckDispatcher(t, true, []api.VolumeMount{hostedSocketVolume})
			err := op.run(d, a)
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(err, &refusal), "refused with a typed refusal: %v", err)
			assert.Equal(t, http.StatusBadRequest, refusal.Status)
			assert.Equal(t, hostedguard.ErrorCode, refusal.Code)
			assert.Contains(t, refusal.Message, `volume "/var/run/docker.sock": host volumes in agent config`)
			assert.False(t, op.called(m), "no broker call")
		})
	}
	for name, tc := range map[string]struct {
		hosted bool
		vols   []api.VolumeMount
	}{
		"workstation hub with a host volume": {false, []api.VolumeMount{hostedSocketVolume}},
		"hosted hub with a gcs volume":       {true, []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: "/b"}}},
		"hosted hub without volumes":         {true, nil},
	} {
		t.Run(name+" is not refused for its config", func(t *testing.T) {
			d, m, a := hostedPrecheckDispatcher(t, tc.hosted, tc.vols)
			err := d.DispatchAgentStart(ctx, a, "task", false)
			var refusal *RuntimeTargetRefusal
			assert.False(t, errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode, "no hosted refusal: %v", err)
			assert.True(t, m.startCalled, "broker called")
		})
	}
}

// TestStartAgent_HostedRefusesStoredMemberVolumes: through the HTTP start
// handler, a hosted hub answers a stored host volume with the same 400
// hosted_config_refused as at create.
func TestStartAgent_HostedRefusesStoredMemberVolumes(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, nil)
	m := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, m, false, slog.Default())
	d.SetHostedMode(true)
	srv.SetDispatcher(d)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Volumes: []api.VolumeMount{hostedSocketVolume}}
	})
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.True(t, strings.HasPrefix(msg, `volume "/var/run/docker.sock": `), msg)
	assert.False(t, m.startCalled, "no broker call")
}

// TestRelayBrokerRefusal_HostedConfigRefused: a hosted broker's refusal
// reaching a hub that did not refuse first (a workstation hub) is relayed
// as the broker's 400 and code, not folded into a 502.
func TestRelayBrokerRefusal_HostedConfigRefused(t *testing.T) {
	body := `{"error":{"code":"hosted_config_refused","message":"volume \"/data\": host volumes in agent config are not allowed on a hosted server"}}`
	rec := httptest.NewRecorder()
	dispatchCreateErrorResponse(rec, &brokerStatusError{StatusCode: http.StatusBadRequest, Body: body}, "agent-1")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.Equal(t, `volume "/data": host volumes in agent config are not allowed on a hosted server`, msg)
}
