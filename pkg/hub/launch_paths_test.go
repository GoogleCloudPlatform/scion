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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Handler-level tests for the asynchronous create paths: create (row C),
// env submit (row E), workspace finalize (row W), the scheduler (row S),
// the cross-node owner (row X) and the as_needed resend (row A).

// beginLaunchInvalidPhaseStore answers BeginLaunch as if a stop had reached
// the record first.
type beginLaunchInvalidPhaseStore struct {
	store.Store
}

func (beginLaunchInvalidPhaseStore) BeginLaunch(context.Context, string, string, time.Duration) (string, error) {
	return "", store.ErrInvalidPhase
}

// newAsyncCreateServer is setupCreateAgentServer with an HTTP dispatcher
// whose broker client accepts asynchronous launches.
func newAsyncCreateServer(t *testing.T, flag bool) (*Server, store.Store, *store.Project, *asyncLaunchClient) {
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
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings {
		return AsyncLaunchSettings{Enabled: flag, Timeout: 5 * time.Minute, KeepaliveSeconds: 15}
	})
	srv.SetDispatcher(d)
	return srv, s, project, client
}

func TestLaunchErrorCodes_WireLiterals(t *testing.T) {
	// Clients match these strings; they are part of the API.
	assert.Equal(t, "agent_launching", ErrCodeAgentLaunching)
	assert.Equal(t, "agent_create_incomplete", ErrCodeAgentCreateIncomplete)
}

func TestCreateAgent_AsyncLaunch(t *testing.T) {
	t.Run("accepted answers 201 provisioning with the launch", func(t *testing.T) {
		srv, s, project, client := newAsyncCreateServer(t, true)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": "async-accepted", "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)
		assert.Empty(t, resp.Warnings)
		assert.Equal(t, string(state.PhaseProvisioning), resp.Agent.Phase)
		require.NotNil(t, resp.Agent.Launch)
		assert.True(t, resp.Agent.Launch.Active)

		require.Len(t, client.sends, 1)
		assert.True(t, client.sends[0].AsyncLaunch)
		row := mustAgent(t, s, resp.Agent.ID)
		assert.True(t, row.IsInFlight())
		assert.Equal(t, client.sends[0].LaunchID, row.LaunchID)
		assert.Equal(t, resp.Agent.Launch.ID, row.LaunchID)
		assert.Equal(t, string(state.PhaseProvisioning), row.Phase)
	})

	t.Run("launch that cannot begin answers 409 and keeps the record", func(t *testing.T) {
		srv, s, project, client := newAsyncCreateServer(t, true)
		d := NewHTTPAgentDispatcherWithClient(beginLaunchInvalidPhaseStore{Store: s}, client, false, slog.Default())
		d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings {
			return AsyncLaunchSettings{Enabled: true, Timeout: 5 * time.Minute, KeepaliveSeconds: 15}
		})
		srv.SetDispatcher(d)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": "async-invalid-phase", "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
		})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Equal(t, "invalid_state", decodeLaunchGuardError(t, rec).Code)
		assert.Empty(t, client.sends, "no broker send")
		assert.False(t, client.deleteCalled, "no dispatch-failure delete")
		_, err := s.GetAgentBySlug(context.Background(), project.ID, "async-invalid-phase")
		assert.NoError(t, err, "the record is left to the operation that stopped it")
	})

	// Flag off with opt-in, and flag on without opt-in, are synchronous and
	// answer the pre-change shape: only the agent, no launch view.
	for _, tc := range []struct {
		name  string
		flag  bool
		optIn bool
	}{
		{"flag-off", false, true},
		{"no-opt-in", true, false},
	} {
		t.Run(tc.name+" keeps the synchronous response shape", func(t *testing.T) {
			srv, s, project, client := newAsyncCreateServer(t, tc.flag)
			body := map[string]interface{}{"name": "sync-" + tc.name, "projectId": project.ID, "task": "do it"}
			if tc.optIn {
				body["acceptAsyncLaunch"] = true
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var top map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &top))
			keys := make([]string, 0, len(top))
			for k := range top {
				keys = append(keys, k)
			}
			assert.ElementsMatch(t, []string{"agent"}, keys)
			var agentJSON map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(top["agent"], &agentJSON))
			assert.NotContains(t, agentJSON, "launch")
			var phase string
			require.NoError(t, json.Unmarshal(agentJSON["phase"], &phase))
			assert.Equal(t, string(state.PhaseRunning), phase, "the broker's synchronous answer is applied")

			require.Len(t, client.sends, 1)
			assert.False(t, client.sends[0].AsyncLaunch)
			assert.Empty(t, client.sends[0].LaunchID)
			row, err := s.GetAgentBySlug(context.Background(), project.ID, "sync-"+tc.name)
			require.NoError(t, err)
			assert.Empty(t, row.LaunchID, "no launch is begun")
		})
	}
}

func TestSchedulerDispatch_AsyncLaunch(t *testing.T) {
	t.Run("flag on persists provisioning with the launch", func(t *testing.T) {
		srv, s, project, client := newAsyncCreateServer(t, true)
		agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-async", "")
		require.Len(t, client.sends, 1)
		assert.True(t, client.sends[0].AsyncLaunch, "scheduled creates opt in server-side")
		assert.True(t, agent.LaunchAsyncOptIn)
		assert.Equal(t, string(state.PhaseProvisioning), agent.Phase)
		assert.True(t, agent.IsInFlight())
		assert.Equal(t, client.sends[0].LaunchID, agent.LaunchID)
		assert.Equal(t, "broker-instance-1", agent.LaunchOwner)
	})
	t.Run("flag off stays synchronous", func(t *testing.T) {
		srv, s, project, client := newAsyncCreateServer(t, false)
		agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-sync", "")
		require.Len(t, client.sends, 1)
		assert.False(t, client.sends[0].AsyncLaunch)
		assert.Empty(t, agent.LaunchID)
	})
}

func TestSubmitAgentEnv_AsyncLaunch(t *testing.T) {
	setup := func(t *testing.T, s store.Store) (*asyncLaunchFixture, string) {
		f := newAsyncLaunchFixtureOn(t, s, &store.BrokerCapabilities{AsyncLaunch: true})
		return f, fmt.Sprintf("/api/v1/projects/%s/agents/%%s/env", tid("al-project"))
	}
	envBody := map[string]interface{}{"env": map[string]string{"API_KEY": "v"}}

	t.Run("in flight answers 200 with a warning and no dispatch", func(t *testing.T) {
		srv, s := testServer(t)
		f, path := setup(t, s)
		srv.SetDispatcher(f.dispatcher)
		agent := f.agent(t, "env-inflight", string(state.PhaseCreated), true)
		agent = seedLaunch(t, s, agent, seedInFlight)

		rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf(path, agent.Slug), envBody)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, []string{launchInFlightInputsWarning}, resp.Warnings)
		assert.Empty(t, f.client.sends, "no dispatch")
		assert.Equal(t, agent.LaunchID, f.row(t, agent.ID).LaunchID, "the launch is unchanged")
	})

	t.Run("accepted does not write running", func(t *testing.T) {
		srv, s := testServer(t)
		f, path := setup(t, s)
		srv.SetDispatcher(f.dispatcher)
		agent := f.agent(t, "env-accepted", string(state.PhaseProvisioning), true)

		rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf(path, agent.Slug), envBody)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings)
		require.NotNil(t, resp.Agent)
		assert.Equal(t, string(state.PhaseProvisioning), resp.Agent.Phase)
		require.Len(t, f.client.sends, 1)
		row := f.row(t, agent.ID)
		assert.Equal(t, string(state.PhaseProvisioning), row.Phase)
		assert.True(t, row.IsInFlight())
		assert.Equal(t, f.client.sends[0].LaunchID, row.LaunchID)
	})

	t.Run("launch that cannot begin answers 409 with no send", func(t *testing.T) {
		srv, s := testServer(t)
		f, path := setup(t, s)
		d := NewHTTPAgentDispatcherWithClient(beginLaunchInvalidPhaseStore{Store: s}, f.client, false, slog.Default())
		d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return f.settings })
		srv.SetDispatcher(d)
		agent := f.agent(t, "env-invalid", string(state.PhaseProvisioning), true)

		rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf(path, agent.Slug), envBody)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Equal(t, "invalid_state", decodeLaunchGuardError(t, rec).Code)
		assert.Empty(t, f.client.sends)
	})
}

func TestWorkspaceFinalize_AsyncLaunch(t *testing.T) {
	finalize := func(t *testing.T, srv *Server, agentID string) SyncToFinalizeResponse {
		t.Helper()
		rec := doBootstrapRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/api/v1/agents/%s/workspace/sync-to/finalize", agentID),
			SyncToFinalizeRequest{Manifest: &transfer.Manifest{Version: "1.0"}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp SyncToFinalizeResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	t.Run("in flight answers not applied with a warning and no dispatch", func(t *testing.T) {
		srv, s, _, _ := testBootstrapServer(t)
		f := newAsyncLaunchFixtureOn(t, s, &store.BrokerCapabilities{AsyncLaunch: true})
		srv.SetDispatcher(f.dispatcher)
		agent := f.agent(t, "ws-inflight", string(state.PhaseCreated), true)
		agent = seedLaunch(t, s, agent, seedInFlight)

		resp := finalize(t, srv, agent.ID)
		assert.False(t, resp.Applied)
		assert.Equal(t, []string{launchInFlightInputsWarning}, resp.Warnings)
		assert.Empty(t, f.client.sends, "no dispatch")
		assert.Equal(t, agent.LaunchID, f.row(t, agent.ID).LaunchID)
	})

	t.Run("accepted keeps provisioning and the storage path", func(t *testing.T) {
		srv, s, _, _ := testBootstrapServer(t)
		f := newAsyncLaunchFixtureOn(t, s, &store.BrokerCapabilities{AsyncLaunch: true})
		srv.SetDispatcher(f.dispatcher)
		agent := f.agent(t, "ws-accepted", string(state.PhaseProvisioning), true)

		resp := finalize(t, srv, agent.ID)
		assert.True(t, resp.Applied)
		require.Len(t, f.client.sends, 1)
		assert.True(t, f.client.sends[0].AsyncLaunch)
		row := f.row(t, agent.ID)
		assert.Equal(t, string(state.PhaseProvisioning), row.Phase)
		assert.True(t, row.IsInFlight())
		assert.Equal(t, f.client.sends[0].LaunchID, row.LaunchID)
		require.NotNil(t, row.AppliedConfig)
		assert.NotEmpty(t, row.AppliedConfig.WorkspaceStoragePath, "the pre-dispatch write survives the launch")
		assert.Equal(t, "tmpl-from-broker", row.Template, "non-status fields are merged")
	})
}

func TestExecDispatch_AsyncLaunchResult(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		srv, s := testServer(t)
		f := newAsyncLaunchFixtureOn(t, s, &store.BrokerCapabilities{AsyncLaunch: true})
		srv.SetDispatcher(f.dispatcher)
		agent := f.agent(t, "x-create", string(state.PhaseCreated), true)

		out, err := srv.executeDispatch(context.Background(), store.BrokerDispatch{ID: tid("d-x-create"), AgentID: agent.ID, Op: "create"})
		require.NoError(t, err)
		var cr CreateWithGatherResult
		require.NoError(t, json.Unmarshal([]byte(out), &cr))
		require.NotNil(t, cr.Launch)
		assert.Equal(t, f.row(t, agent.ID).LaunchID, cr.Launch.ID)
		assert.Equal(t, "broker-instance-1", cr.Launch.Owner)
		assert.Nil(t, cr.EnvRequirements)
	})
	t.Run("finalize_env", func(t *testing.T) {
		srv, s := testServer(t)
		f := newAsyncLaunchFixtureOn(t, s, &store.BrokerCapabilities{AsyncLaunch: true})
		srv.SetDispatcher(f.dispatcher)
		agent := f.agent(t, "x-finalize", string(state.PhaseProvisioning), true)
		args, err := MarshalDispatchArgs(&FinalizeEnvDispatchArgs{Env: map[string]string{"K": "v"}})
		require.NoError(t, err)

		out, err := srv.executeDispatch(context.Background(), store.BrokerDispatch{ID: tid("d-x-finalize"), AgentID: agent.ID, Op: "finalize_env", Args: args})
		require.NoError(t, err)
		var fr FinalizeEnvResult
		require.NoError(t, json.Unmarshal([]byte(out), &fr))
		require.NotNil(t, fr.Launch)
		assert.Equal(t, f.row(t, agent.ID).LaunchID, fr.Launch.ID)
	})
}

// TestCrossNodeCreate_AsyncLaunchRoundTrip runs a deferred create through
// the durable dispatch row: the requesting node writes no launch, the owner
// begins and accepts it, and the requesting node returns the owner's launch.
func TestCrossNodeCreate_AsyncLaunchRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	owner := newAsyncLaunchFixtureOn(t, s, &store.BrokerCapabilities{AsyncLaunch: true})
	srv.SetDispatcher(owner.dispatcher)
	agent := owner.agent(t, "x-roundtrip", string(state.PhaseCreated), true)

	events := NewChannelEventPublisher()
	defer events.Close()
	reqClient := &asyncLaunchClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, wouldDefer: true}
	requester := NewHTTPAgentDispatcherWithClient(s, reqClient, false, slog.Default())
	requester.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return owner.settings })
	requester.SetCrossNodeDeps(events, NoopCommandBus{})

	// The owner node: claim the pending row, execute it, complete it.
	ownerErr := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			pending, err := s.ListPendingDispatch(ctx, agent.RuntimeBrokerID)
			if err != nil {
				ownerErr <- err
				return
			}
			if len(pending) == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			row := pending[0]
			if _, err := s.ClaimBrokerDispatch(ctx, row.ID, "owner"); err != nil {
				ownerErr <- err
				return
			}
			out, err := srv.executeDispatch(ctx, row)
			if err != nil {
				ownerErr <- err
				return
			}
			if err := s.CompleteBrokerDispatch(ctx, row.ID, out); err != nil {
				ownerErr <- err
				return
			}
			events.PublishDispatchDone(ctx, row.ID)
			ownerErr <- nil
			return
		}
		ownerErr <- fmt.Errorf("no dispatch row appeared")
	}()

	res, err := requester.DispatchAgentCreateWithGather(ctx, agent)
	require.NoError(t, err)
	require.NoError(t, <-ownerErr)

	assert.Empty(t, reqClient.sends, "the requesting node sends nothing")
	require.Len(t, owner.client.sends, 1)
	assert.True(t, owner.client.sends[0].AsyncLaunch)
	accepted := res.AcceptedLaunch()
	require.NotNil(t, accepted)
	row := owner.row(t, agent.ID)
	assert.Equal(t, owner.client.sends[0].LaunchID, row.LaunchID, "one launch, begun by the owner")
	assert.Equal(t, row.LaunchID, accepted.ID)
	assert.Equal(t, "broker-instance-1", accepted.Owner)
}

// TestDispatchFinalizeEnv_ResendUsesFreshRequestID covers the inner
// as_needed resend: it always carries a new RequestID (with the flag off as
// well), so the broker's attempt cache cannot replay the first answer; with
// the flag on it also carries a new launch ID. The resend happens for an
// as_needed key and for a TZ-only need, which the hub answers itself.
func TestDispatchFinalizeEnv_ResendUsesFreshRequestID(t *testing.T) {
	for _, tc := range []struct {
		need string
		flag bool
	}{{"SECRET_A", false}, {"SECRET_A", true}, {"TZ", false}, {"TZ", true}} {
		flag := tc.flag
		t.Run(fmt.Sprintf("need=%s/flag=%v", tc.need, flag), func(t *testing.T) {
			ctx := context.Background()
			s := createTestStore(t)
			var asNeeded []store.EnvVar
			if tc.need == "SECRET_A" {
				asNeeded = []store.EnvVar{{Key: "SECRET_A", Value: "val-a"}}
			}
			agent := setupFinalizeEnvTest(t, ctx, s, asNeeded)
			agent.Phase = string(state.PhaseProvisioning)
			agent.LaunchAsyncOptIn = true
			agent.OwnerID = "" // the fixture's owner is not a stored user
			require.NoError(t, s.CreateAgent(ctx, agent))
			agent = mustAgent(t, s, agent.ID)

			var sends []RemoteCreateAgentRequest
			client := &mockRuntimeBrokerClient{
				createWithGatherFunc: func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
					sends = append(sends, *req)
					if len(sends) == 1 {
						return nil, &RemoteEnvRequirementsResponse{AgentID: req.ID, Needs: []string{tc.need}}, nil
					}
					if req.AsyncLaunch {
						return acceptedAnswer(req, req.LaunchID), nil, nil
					}
					return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Phase: string(state.PhaseRunning)}, Created: true}, nil, nil
				},
			}
			d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
			d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings {
				return AsyncLaunchSettings{Enabled: flag, Timeout: 5 * time.Minute, KeepaliveSeconds: 15}
			})

			res, err := d.DispatchFinalizeEnv(ctx, agent, map[string]string{"CLI_VAR": "v"})
			require.NoError(t, err)
			require.Len(t, sends, 2)
			require.NotEmpty(t, sends[0].RequestID)
			require.NotEmpty(t, sends[1].RequestID)
			assert.NotEqual(t, sends[0].RequestID, sends[1].RequestID, "the resend must not reuse the first RequestID")
			if tc.need == "SECRET_A" {
				assert.Equal(t, "val-a", sends[1].ResolvedEnv["SECRET_A"])
			} else {
				assert.NotEmpty(t, sends[1].ResolvedEnv["TZ"], "the hub answers the TZ need")
			}

			if !flag {
				assert.Nil(t, res.AcceptedLaunch())
				assert.False(t, sends[1].AsyncLaunch)
				assert.Empty(t, mustAgent(t, s, agent.ID).LaunchID)
				return
			}
			require.True(t, sends[0].AsyncLaunch)
			require.True(t, sends[1].AsyncLaunch)
			assert.NotEqual(t, sends[0].LaunchID, sends[1].LaunchID, "the resend begins a new launch")
			accepted := res.AcceptedLaunch()
			require.NotNil(t, accepted)
			assert.Equal(t, sends[1].LaunchID, accepted.ID)
			row := mustAgent(t, s, agent.ID)
			assert.True(t, row.IsInFlight())
			assert.Equal(t, sends[1].LaunchID, row.LaunchID)
		})
	}
}

// TestAgentMessage_ProvisioningAgent characterises message delivery to an
// agent whose create launch is in flight (phase provisioning): without wake
// it is refused as not yet running; with wake the start guard skips the
// agent. Nothing is dispatched either way.
func TestAgentMessage_ProvisioningAgent(t *testing.T) {
	setup := func(t *testing.T, suffix string) (*Server, *recordingDispatcher, *store.Agent) {
		srv, s := testServer(t)
		agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseCreated)
		agent = seedLaunch(t, s, agent, seedInFlight)
		_, err := s.MarkLaunchAccepted(context.Background(), agent.ID, agent.LaunchID, "i1")
		require.NoError(t, err)
		agent = mustAgent(t, s, agent.ID)
		require.Equal(t, string(state.PhaseProvisioning), agent.Phase)
		disp := &recordingDispatcher{}
		srv.SetDispatcher(disp)
		return srv, disp, agent
	}

	t.Run("without wake", func(t *testing.T) {
		srv, disp, agent := setup(t, "d6-nowake")
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
			"message": "hello",
		})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		apiErr := decodeLaunchGuardError(t, rec)
		assert.Equal(t, ErrCodeAgentNotRunning, apiErr.Code)
		assert.Contains(t, apiErr.Message, "not yet running (phase: provisioning)")
		assert.Empty(t, disp.getCalls())
	})

	t.Run("with wake", func(t *testing.T) {
		srv, disp, agent := setup(t, "d6-wake")
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
			"message": "hello", "wake": true,
		})
		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		apiErr := decodeLaunchGuardError(t, rec)
		assert.Equal(t, ErrCodeRuntimeError, apiErr.Code)
		assert.Equal(t, launchInFlightMessage, apiErr.Message)
		assert.Empty(t, disp.getCalls())
	})

	t.Run("agent-to-agent delivery gate", func(t *testing.T) {
		_, _, agent := setup(t, "d6-a2a")
		dmErr := validateAgentDeliverable(agent)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	})
}

// raceLaunchClient defers every start, and begins a create launch on the
// agent first: the launch lands between the caller's guard and the owner's.
type raceLaunchClient struct {
	mockRuntimeBrokerClient
	s       store.Store
	agentID string
}

func (c *raceLaunchClient) StartAgent(ctx context.Context, _, _, _, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, _ StartExtras) (*RemoteAgentResponse, error) {
	return nil, c.beginThenDefer(ctx)
}

func (c *raceLaunchClient) RestartAgent(ctx context.Context, _, _, _, _ string, _ map[string]string, _ StartExtras) (*RemoteAgentResponse, error) {
	return nil, c.beginThenDefer(ctx)
}

func (c *raceLaunchClient) beginThenDefer(ctx context.Context) error {
	if _, err := c.s.BeginLaunch(ctx, c.agentID, store.LaunchKindCreate, 5*time.Minute); err != nil {
		return err
	}
	return ErrLifecycleDeferred
}

func TestDeferredStart_LaunchRaceMapsToGuardError(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			_, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "race-"+op, state.PhaseCreated)
			client := &raceLaunchClient{s: s, agentID: agent.ID}
			d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
			events := NewChannelEventPublisher()
			defer events.Close()
			d.SetCrossNodeDeps(events, NoopCommandBus{})

			// The owner refuses and publishes nothing; the wait ends with
			// the caller's deadline.
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			var err error
			if op == "start" {
				err = d.DispatchAgentStart(ctx, agent, "", false)
			} else {
				err = d.DispatchAgentRestart(ctx, agent)
			}
			require.ErrorIs(t, err, ErrLaunchInFlight)
		})
	}
}
