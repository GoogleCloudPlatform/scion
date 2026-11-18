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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newClaimedAsyncCreate builds a server with start claims on whose HTTP
// dispatcher launches opted-in creates asynchronously, and an opted-in agent
// in phase created on its broker.
func newClaimedAsyncCreate(t *testing.T, name string) (*Server, store.Store, *asyncLaunchClient, *store.Agent) {
	t.Helper()
	srv, s, project, client := newAsyncCreateServer(t, true)
	srv.startClaimsOn = true
	a := &store.Agent{
		ID: tid("cuc-" + name), Slug: "cuc-" + name, Name: "cuc-" + name,
		ProjectID: project.ID, RuntimeBrokerID: tid("broker-create"),
		Phase: string(state.PhaseCreated), LaunchAsyncOptIn: true,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "do the thing"},
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return srv, s, client, mustAgent(t, s, a.ID)
}

// A create send that fails after BeginLaunch with an error that is not
// known to be undelivered (startOutcomeOf: unconfirmed) but is not an
// unknown outcome either (isUnknownDispatchOutcome) ends the launch
// not_launched, but the broker may still have admitted the create: the
// holder keeps its create claim unconfirmed, so a second start is refused
// (ptone/scion#4709). A timeout is an unknown outcome instead and keeps the
// launch (TestCreateUnderClaim_SendTimeoutAfterBeginLaunchHandsClaimToLaunch).
func TestCreateUnderClaim_SendFailureAfterBeginLaunchKeepsClaimUnconfirmed(t *testing.T) {
	srv, s, client, a := newClaimedAsyncCreate(t, "send-failure")
	sendErr := errors.New("broker unreachable")
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		require.True(t, req.AsyncLaunch, "the send is an async launch")
		require.NotEmpty(t, req.LaunchID)
		return nil, nil, sendErr
	}

	_, err := srv.createUnderClaim(context.Background(), a, func(ctx context.Context) (*CreateDispatchResult, error) {
		return srv.GetDispatcher().DispatchAgentCreate(ctx, a)
	})
	require.ErrorIs(t, err, sendErr)
	require.NotErrorIs(t, err, errStartClaimLost)
	require.Len(t, client.sends, 1)

	got := mustAgent(t, s, a.ID)
	assert.Equal(t, store.LaunchStateEnded, got.LaunchState)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, got.LaunchEndReason)
	require.NotEmpty(t, got.StartClaimID, "the agent still holds its create claim")
	assert.Equal(t, store.StartClaimCreate, got.StartClaimKind)
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "an ambiguous send keeps the claim unconfirmed")
	require.NotNil(t, got.StartClaimHoldUntil, "the unconfirmed claim has a hold")
	require.NotNil(t, got.StartClaimUnconfirmedAt)
	assert.Equal(t, srv.startClaimSettings().CreateUnconfirmedHold, got.StartClaimHoldUntil.Sub(*got.StartClaimUnconfirmedAt))

	_, err = s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	var held *store.ClaimHeldError
	require.ErrorAs(t, err, &held, "a second start is refused while the create may be running")
	assert.Equal(t, store.StartClaimCreate, held.Kind)
}

// A create send that times out after BeginLaunch is an unknown outcome
// (ptone/scion#4866): the launch is kept and accepted with no owner, so the
// create claim passes to the launch (linked to it, still live; the launch's
// end settles it) and a second start is still refused.
func TestCreateUnderClaim_SendTimeoutAfterBeginLaunchHandsClaimToLaunch(t *testing.T) {
	srv, s, client, a := newClaimedAsyncCreate(t, "send-timeout-kept")
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		require.True(t, req.AsyncLaunch, "the send is an async launch")
		return nil, nil, fmt.Errorf("failed to send request: %w", context.DeadlineExceeded)
	}

	created, err := srv.createUnderClaim(context.Background(), a, func(ctx context.Context) (*CreateDispatchResult, error) {
		return srv.GetDispatcher().DispatchAgentCreate(ctx, a)
	})
	require.NoError(t, err)
	require.Len(t, client.sends, 1)
	accepted := created.AcceptedLaunch()
	require.NotNil(t, accepted, "the timed-out launch is kept as accepted")
	assert.Equal(t, client.sends[0].LaunchID, accepted.ID)

	got := mustAgent(t, s, a.ID)
	assert.Equal(t, store.LaunchStateActive, got.LaunchState)
	assert.Equal(t, string(state.PhaseProvisioning), got.Phase)
	require.NotEmpty(t, got.StartClaimID, "the agent still holds its create claim")
	assert.Equal(t, store.StartClaimCreate, got.StartClaimKind)
	assert.Equal(t, store.StartClaimLive, got.StartClaimState, "the claim passed to the launch, not settled by the holder")
	assert.Equal(t, accepted.ID, got.StartClaimLaunchID, "the claim is linked to the kept launch, whose end settles it")

	_, err = s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	var held *store.ClaimHeldError
	require.ErrorAs(t, err, &held, "a second start is refused while the launch may be running")
	assert.Equal(t, store.StartClaimCreate, held.Kind)
}

// An old broker answers the create synchronously, which ends the launch
// not_launched. A claim renewal after that end still finds the claim held,
// so the create is not reported as abandoned (ptone/scion#4709).
func TestCreateUnderClaim_SyncAnswerOutlivingRenewIsNotLost(t *testing.T) {
	srv, s, client, a := newClaimedAsyncCreate(t, "sync-renew")
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		// A broker without async support ignores the launch fields.
		return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: string(state.PhaseRunning)}, Created: true}, nil, nil
	}
	const ttl = 600 * time.Millisecond
	fastClaims(srv, ttl)

	created, err := srv.createUnderClaim(context.Background(), a, func(ctx context.Context) (*CreateDispatchResult, error) {
		res, err := srv.GetDispatcher().DispatchAgentCreate(ctx, a)
		if err != nil {
			return res, err
		}
		// The post-dispatch writes outlast the renew interval, so the
		// claim renews after the launch ended not_launched.
		require.Equal(t, store.LaunchEndReasonNotLaunched, mustAgent(t, s, a.ID).LaunchEndReason)
		time.Sleep(2 * ttl / 3)
		return res, nil
	})
	require.NoError(t, err, "a create that succeeded is not reported as an abandoned start")
	// A synchronous success returns no dispatch result (nil, nil).
	assert.Nil(t, created.AcceptedLaunch(), "a synchronous answer is not an accepted launch")

	got := mustAgent(t, s, a.ID)
	assert.Equal(t, store.LaunchEndReasonNotLaunched, got.LaunchEndReason)
	assert.Empty(t, got.StartClaimID, "the holder released its claim on success")
}

// closedLoopbackEndpoint allocates a loopback TCP port and closes the
// listener immediately so a subsequent HTTP dial fails with connection refused.
func closedLoopbackEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return "http://" + addr
}

// newRealHTTPDispatchServer sets up a Server with start claims enabled, an
// HTTPAgentDispatcher backed by a real HTTPRuntimeBrokerClient, and a broker
// pointing at endpoint.
func newRealHTTPDispatchServer(t *testing.T, name, endpoint string, asyncLaunch bool, clientTimeout time.Duration, rt http.RoundTripper) (*Server, store.Store, *store.Project, *store.RuntimeBroker, *HTTPRuntimeBrokerClient) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)
	srv.startClaimsOn = true
	project := &store.Project{ID: tid("proj-5284-" + name), Name: "proj-" + name, Slug: "proj-" + name}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID:       tid("broker-5284-" + name),
		Name:     "broker-" + name,
		Slug:     "broker-" + name,
		Endpoint: endpoint,
		Status:   store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{
			AsyncLaunch: asyncLaunch,
			AsyncStart:  asyncLaunch,
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	httpClient := NewHTTPRuntimeBrokerClient()
	if clientTimeout > 0 {
		httpClient.transport.client.Timeout = clientTimeout
	}
	if rt != nil {
		httpClient.transport.client.Transport = rt
	}
	d := NewHTTPAgentDispatcherWithClient(s, httpClient, false, slog.Default())
	d.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	if asyncLaunch {
		d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings {
			return AsyncLaunchSettings{Enabled: true, Timeout: 5 * time.Minute, KeepaliveSeconds: 15}
		})
	}
	srv.SetDispatcher(d)
	return srv, s, project, broker, httpClient
}

// TestDispatchSend_NothingStartedReleasesStartClaimForImmediateRetry covers
// ptone/scion#5284: when a dispatch send fails in a way that shows nothing
// started (dial refused, pending dial timing out before any connection, TLS
// handshake failure before any connection, or a 4xx refusal from the broker),
// the Hub releases the start claim at once so an immediate env resubmit or
// start retry succeeds instead of getting 409 start_in_progress.
func TestDispatchSend_NothingStartedReleasesStartClaimForImmediateRetry(t *testing.T) {
	// Good broker server used for the immediate retry after the failed send.
	goodBroker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if launchID, _ := body["launchId"].(string); body["asyncLaunch"] == true && launchID != "" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"launchPending":    true,
				"launchId":         launchID,
				"launchInstanceId": "inst-ok",
			})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"created": true,
			"agent":   map[string]interface{}{"id": "ok", "name": "ok", "phase": "running"},
		})
	}))
	t.Cleanup(goodBroker.Close)

	refusal422Broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_error","message":"invalid harness config"}}`))
	}))
	t.Cleanup(refusal422Broker.Close)

	tlsUntrustedBroker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tlsUntrustedBroker.Close)

	pendingDialTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	t.Cleanup(pendingDialTransport.CloseIdleConnections)

	cases := []struct {
		name     string
		endpoint string
		timeout  time.Duration
		rt       http.RoundTripper
	}{
		{
			name:     "dial refused",
			endpoint: closedLoopbackEndpoint(t),
		},
		{
			name:     "dial timeout before connection",
			endpoint: "http://127.0.0.1:9",
			timeout:  60 * time.Millisecond,
			rt:       pendingDialTransport,
		},
		{
			name:     "TLS handshake failure before connection",
			endpoint: tlsUntrustedBroker.URL,
		},
		{
			name:     "broker 422 refusal",
			endpoint: refusal422Broker.URL,
		},
	}

	for _, tc := range cases {
		t.Run("env finalize/"+tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv, s, project, broker, httpClient := newRealHTTPDispatchServer(t, "env-"+tc.name, tc.endpoint, true, tc.timeout, tc.rt)
			agent := &store.Agent{
				ID: tid("a-env-" + tc.name), Slug: "a-env", Name: "a-env",
				ProjectID: project.ID, RuntimeBrokerID: broker.ID,
				Phase: string(state.PhaseProvisioning), LaunchAsyncOptIn: true,
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "run"},
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			envPath := fmt.Sprintf("/api/v1/projects/%s/agents/%s/env", project.ID, agent.Slug)
			envBody := map[string]interface{}{"env": map[string]string{"API_KEY": "secret"}}

			rec := doRequest(t, srv, http.MethodPost, envPath, envBody)
			require.GreaterOrEqual(t, rec.Code, 400, "the failed send must fail the request: %s", rec.Body.String())
			assert.NotEqual(t, http.StatusGatewayTimeout, rec.Code, "a pre-send failure is never 504 start_outcome_unknown")

			got := mustAgent(t, s, agent.ID)
			assert.Equal(t, string(state.PhaseProvisioning), got.Phase)
			assert.Equal(t, store.LaunchStateEnded, got.LaunchState)
			assert.Equal(t, store.LaunchEndReasonNotLaunched, got.LaunchEndReason)
			assert.Equal(t, store.LaunchErrorDispatchFailed, got.LaunchError)
			assert.Empty(t, got.StartClaimID, "nothing started: the start claim must be released at once")
			assert.Empty(t, got.StartClaimState)

			// Point at the working broker and resubmit immediately without
			// waiting for start_create_unconfirmed_hold or running scion stop.
			broker.Endpoint = goodBroker.URL
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
			httpClient.transport.client.Transport = http.DefaultTransport
			httpClient.transport.client.Timeout = 5 * time.Second

			rec = doRequest(t, srv, http.MethodPost, envPath, envBody)
			require.Equal(t, http.StatusOK, rec.Code, "immediate env resubmit must succeed instead of 409 start_in_progress: %s", rec.Body.String())
			assert.True(t, mustAgent(t, s, agent.ID).IsInFlight())
		})

		t.Run("lifecycle start/"+tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv, s, project, broker, httpClient := newRealHTTPDispatchServer(t, "start-"+tc.name, tc.endpoint, false, tc.timeout, tc.rt)
			agent := &store.Agent{
				ID: tid("a-start-" + tc.name), Slug: "a-start", Name: "a-start",
				ProjectID: project.ID, RuntimeBrokerID: broker.ID,
				Phase:         string(state.PhaseStopped),
				AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "run"},
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			startPath := "/api/v1/agents/" + agent.ID + "/" + string(api.AgentActionStart)
			rec := doRequest(t, srv, http.MethodPost, startPath, nil)
			require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())

			got := mustAgent(t, s, agent.ID)
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
			assert.Empty(t, got.StartClaimID, "nothing started: the start claim must be released at once")
			assert.Empty(t, got.StartClaimState)

			// Retry start immediately against the working broker.
			broker.Endpoint = goodBroker.URL
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
			httpClient.transport.client.Transport = http.DefaultTransport
			httpClient.transport.client.Timeout = 5 * time.Second

			rec = doRequest(t, srv, http.MethodPost, startPath, nil)
			require.Equal(t, http.StatusOK, rec.Code, "immediate start retry must succeed instead of 409 start_in_progress: %s", rec.Body.String())
			assert.Equal(t, string(state.PhaseRunning), mustAgent(t, s, agent.ID).Phase)
		})
	}
}

// TestDispatchSend_CrossNodeNothingStartedReleasesStartClaimForImmediateRetry
// verifies that on a cross-node dispatch (ErrLifecycleDeferred -> owner node
// executes the broker_dispatch row), when the owner's send fails before
// anything started on the Runtime Broker (dial failure, broker not connected,
// request not sent, run not begun, or broker 4xx), the originating node
// releases its start claim at once so an immediate retry succeeds.
func TestDispatchSend_CrossNodeNothingStartedReleasesStartClaimForImmediateRetry(t *testing.T) {
	dialErr := &url.Error{
		Op:  "Post",
		URL: "http://127.0.0.1:9/api/v1/agents",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
	}
	refusal400 := newBrokerStatusError("broker-x", http.StatusBadRequest,
		[]byte(`{"error":{"code":"validation_error","message":"bad request"}}`), "")

	cases := []struct {
		name     string
		ownerErr error
	}{
		{"owner dial failure", fmt.Errorf("failed to send request: %w", dialErr)},
		{"owner broker not connected", fmt.Errorf("broker x not connected: %w", errStartBrokerNotConnected)},
		{"owner request not sent", fmt.Errorf("failed to sign request: %w (%w)", errors.New("expired"), errStartRequestNotSent)},
		{"owner run not begun", fmt.Errorf("%w: %w", errStartRunNotBegun, errAgentTokenRecord)},
		{"owner broker 400", refusal400},
	}

	for _, tc := range cases {
		t.Run("env finalize/"+tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.startClaimsOn = true
			bus := NewChannelEventPublisher()
			t.Cleanup(bus.Close)
			srv.events = bus

			owner := &Server{
				store:             s,
				instanceID:        "hub-owner-" + uuid.NewString()[:8],
				agentLifecycleLog: slog.Default(),
				events:            bus,
			}
			ownerDisp := &ownerErrDispatcher{finalizeErr: tc.ownerErr}
			owner.SetDispatcher(ownerDisp)
			owner.execDispatch = owner.executeDispatch
			owner.deliverMsg = owner.deliverMessage

			requester := NewHTTPAgentDispatcherWithClient(s, &deferredDataOpTestClient{localBroker: "local-only"}, false, slog.Default())
			requester.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
			requester.SetCrossNodeDeps(bus, ownerSignalBus{owner: owner})
			srv.SetDispatcher(requester)

			agent := setupBrokerAgentInPhase(t, s, "xnode-env-"+uuid.NewString()[:6], state.PhaseProvisioning)
			envPath := fmt.Sprintf("/api/v1/projects/%s/agents/%s/env", agent.ProjectID, agent.Slug)
			envBody := map[string]interface{}{"env": map[string]string{"API_KEY": "v"}}

			rec := doRequest(t, srv, http.MethodPost, envPath, envBody)
			require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())

			got := mustAgent(t, s, agent.ID)
			assert.Empty(t, got.StartClaimID, "cross-node nothing-started failure must release the start claim: %s", tc.name)
			assert.Empty(t, got.StartClaimState)

			// Clear the owner error and immediately resubmit env.
			ownerDisp.finalizeErr = nil
			rec = doRequest(t, srv, http.MethodPost, envPath, envBody)
			require.Equal(t, http.StatusOK, rec.Code, "immediate env resubmit must succeed: %s", rec.Body.String())
		})

		t.Run("lifecycle start/"+tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.startClaimsOn = true
			bus := NewChannelEventPublisher()
			t.Cleanup(bus.Close)
			srv.events = bus

			owner := &Server{
				store:             s,
				instanceID:        "hub-owner-" + uuid.NewString()[:8],
				agentLifecycleLog: slog.Default(),
				events:            bus,
			}
			var failStart atomic.Bool
			failStart.Store(true)
			ownerDisp := &ownerErrDispatcher{err: tc.ownerErr}
			ownerDisp.beforeStart = func() {
				if !failStart.Load() {
					ownerDisp.err = nil
					// Simulate the owner's successful start publishing running.
					go func() {
						a, gerr := s.GetAgent(context.Background(), tid("placeholder"))
						_ = a
						_ = gerr
					}()
				}
			}
			owner.SetDispatcher(ownerDisp)
			owner.execDispatch = owner.executeDispatch
			owner.deliverMsg = owner.deliverMessage

			requester := NewHTTPAgentDispatcherWithClient(s, &deferredTestClient{localBroker: "local-only"}, false, slog.Default())
			requester.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
			requester.SetCrossNodeDeps(bus, ownerSignalBus{owner: owner})
			srv.SetDispatcher(requester)

			agent := setupBrokerAgentInPhase(t, s, "xnode-start-"+uuid.NewString()[:6], state.PhaseStopped)
			ownerDisp.beforeStart = func() {
				if !failStart.Load() {
					ownerDisp.err = nil
					running := *agent
					running.Phase = string(state.PhaseRunning)
					bus.PublishAgentStatus(context.Background(), &running)
				}
			}

			startPath := "/api/v1/agents/" + agent.ID + "/" + string(api.AgentActionStart)
			rec := doRequest(t, srv, http.MethodPost, startPath, nil)
			require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())

			got := mustAgent(t, s, agent.ID)
			assert.Empty(t, got.StartClaimID, "cross-node nothing-started failure must release the start claim: %s", tc.name)
			assert.Empty(t, got.StartClaimState)

			// Immediately retry start now that the owner succeeds.
			failStart.Store(false)
			rec = doRequest(t, srv, http.MethodPost, startPath, nil)
			require.Equal(t, http.StatusOK, rec.Code, "immediate start retry must succeed: %s", rec.Body.String())
		})
	}
}

// TestDispatchSend_UnknownOutcomeKeepsClaimUnconfirmed verifies that sends
// whose outcome is unknown (a 502/504 gateway error, or a response timeout
// after the TCP connection was established and the request was sent) keep
// today's behaviour: the start claim stays held/unconfirmed and an immediate
// retry gets 409 start_in_progress.
func TestDispatchSend_UnknownOutcomeKeepsClaimUnconfirmed(t *testing.T) {
	gateway502 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream proxy bad gateway"))
	}))
	t.Cleanup(gateway502.Close)

	releaseStalled := make(chan struct{})
	stalledAfterReceive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Connection was established (GotConn fired) and the broker received
		// the request, then stalled past the client timeout.
		select {
		case <-r.Context().Done():
		case <-releaseStalled:
		}
	}))
	t.Cleanup(func() {
		close(releaseStalled)
		stalledAfterReceive.CloseClientConnections()
		stalledAfterReceive.Close()
	})

	t.Run("502 gateway error keeps claim unconfirmed and blocks immediate retry with 409", func(t *testing.T) {
		ctx := context.Background()
		srv, s, project, broker, _ := newRealHTTPDispatchServer(t, "502", gateway502.URL, true, 0, nil)
		agent := &store.Agent{
			ID: tid("a-502"), Slug: "a-502", Name: "a-502",
			ProjectID: project.ID, RuntimeBrokerID: broker.ID,
			Phase: string(state.PhaseProvisioning), LaunchAsyncOptIn: true,
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "run"},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		envPath := fmt.Sprintf("/api/v1/projects/%s/agents/%s/env", project.ID, agent.Slug)
		envBody := map[string]interface{}{"env": map[string]string{"API_KEY": "v"}}

		rec := doRequest(t, srv, http.MethodPost, envPath, envBody)
		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

		got := mustAgent(t, s, agent.ID)
		require.NotEmpty(t, got.StartClaimID, "502 may have reached the broker: keep the claim")
		assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState)

		// Immediate retry must be refused with 409 start_in_progress.
		rec = doRequest(t, srv, http.MethodPost, envPath, envBody)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeStartInProgress, decodeLaunchGuardError(t, rec).Code)
	})

	t.Run("post-connection timeout on sync start keeps claim unconfirmed and blocks retry with 409", func(t *testing.T) {
		ctx := context.Background()
		srv, s, project, broker, _ := newRealHTTPDispatchServer(t, "postconn-timeout", stalledAfterReceive.URL, false, 60*time.Millisecond, nil)
		agent := &store.Agent{
			ID: tid("a-postconn"), Slug: "a-postconn", Name: "a-postconn",
			ProjectID: project.ID, RuntimeBrokerID: broker.ID,
			Phase:         string(state.PhaseStopped),
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "run"},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		startPath := "/api/v1/agents/" + agent.ID + "/" + string(api.AgentActionStart)
		rec := doRequest(t, srv, http.MethodPost, startPath, nil)
		require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())

		got := mustAgent(t, s, agent.ID)
		require.NotEmpty(t, got.StartClaimID, "timeout after GotConn may have started the container: keep the claim")
		assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState)

		rec = doRequest(t, srv, http.MethodPost, startPath, nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeStartInProgress, decodeLaunchGuardError(t, rec).Code)
	})

	t.Run("post-connection timeout on async env finalize keeps launch accepted", func(t *testing.T) {
		ctx := context.Background()
		srv, s, project, broker, _ := newRealHTTPDispatchServer(t, "postconn-async", stalledAfterReceive.URL, true, 60*time.Millisecond, nil)
		agent := &store.Agent{
			ID: tid("a-postconn-async"), Slug: "a-postconn-async", Name: "a-postconn-async",
			ProjectID: project.ID, RuntimeBrokerID: broker.ID,
			Phase: string(state.PhaseProvisioning), LaunchAsyncOptIn: true,
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "run"},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		envPath := fmt.Sprintf("/api/v1/projects/%s/agents/%s/env", project.ID, agent.Slug)
		rec := doRequest(t, srv, http.MethodPost, envPath, map[string]interface{}{"env": map[string]string{"API_KEY": "v"}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got := mustAgent(t, s, agent.ID)
		assert.Equal(t, store.LaunchStateActive, got.LaunchState, "timeout after GotConn keeps the async launch active")
		assert.Equal(t, store.StartClaimLive, got.StartClaimState, "claim stays linked to the active launch")
		assert.Equal(t, got.LaunchID, got.StartClaimLaunchID)
	})
}

type claimLaunchRunIDFailStore struct {
	store.Store
}

func (s claimLaunchRunIDFailStore) SetAgentRunID(_ context.Context, _, _ string, _ *store.AgentCredential) (string, error) {
	return "", fmt.Errorf("%w: %w", store.ErrCredentialNotRecorded, errors.New("insert refused"))
}

// TestCreateBeginRunPreSendFailure_ReleasesStartClaim verifies that if
// beginRun fails in DispatchFinalizeEnv before any broker send (for example
// the credential or run ID could not be recorded), the start claim is
// released at once rather than held unconfirmed.
func TestCreateBeginRunPreSendFailure_ReleasesStartClaim(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.startClaimsOn = true
	project := &store.Project{ID: tid("proj-beginrun-5284"), Name: "proj-br", Slug: "proj-br"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("broker-beginrun-5284"), Name: "broker-br", Slug: "broker-br",
		Endpoint: "http://127.0.0.1:9", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	mockClient := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(claimLaunchRunIDFailStore{Store: s}, mockClient, false, slog.Default())
	d.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	srv.SetDispatcher(d)

	agent := &store.Agent{
		ID: tid("a-beginrun-5284"), Slug: "a-br", Name: "a-br",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase:         string(state.PhaseProvisioning),
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "run"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	envPath := fmt.Sprintf("/api/v1/projects/%s/agents/%s/env", project.ID, agent.Slug)
	rec := doRequest(t, srv, http.MethodPost, envPath, map[string]interface{}{"env": map[string]string{"API_KEY": "v"}})
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	got := mustAgent(t, s, agent.ID)
	assert.Empty(t, got.StartClaimID, "beginRun failed before any send: start claim must be released")
	assert.Empty(t, got.StartClaimState)
}
