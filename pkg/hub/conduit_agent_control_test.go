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
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentControlRoute_MatchesSciontool: the hub's rotate-token name and
// path are the ones sciontool's control handler advertises and serves.
func TestAgentControlRoute_MatchesSciontool(t *testing.T) {
	assert.Equal(t, control.Prefix, agentControlPrefix)
	assert.Equal(t, control.RouteRotateToken, agentControlRouteRotateToken)
	ctl := control.New(control.Options{KickTokenRefresh: func() {}})
	assert.Contains(t, ctl.Routes(), agentControlRouteRotateToken)
}

// TestRequestAgentTokenRotation_CrossNode: the agent's session is held by
// another hub node (hub-b); RequestAgentTokenRotation on hub-a reaches it
// through hub-b's relay, the control handler sees exactly one
// rotate-token request, and the call returns nil (202).
func TestRequestAgentTokenRotation_CrossNode(t *testing.T) {
	f := newPTYConduitFixture(t)
	hubB := f.startPeerRelay(t, "hub-b")
	ca := f.startControlAgent(t, hubB, true, nil)

	recs := f.agentSessions(t)
	require.Len(t, recs, 1)
	require.Equal(t, "hub-b", recs[0].RelayInstanceID, "the agent session is on the other node")
	require.Equal(t, []string{agentControlRouteRotateToken}, recs[0].Capabilities.RPC)

	require.NoError(t, f.srv.RequestAgentTokenRotation(context.Background(), f.launched.ID))
	assert.Equal(t, int32(1), ca.rpcs.Load(), "exactly one RPC reached the agent")
	assert.Equal(t, int32(1), ca.kicks.Load(), "exactly one refresh kick")
	assert.Equal(t, "/v1/control/rotate-token", <-ca.paths)
}

// TestRequestAgentTokenRotation_Local: the agent's session is on this
// node; the call is made on the local session and returns nil.
func TestRequestAgentTokenRotation_Local(t *testing.T) {
	f := newPTYConduitFixture(t)
	ca := f.startControlAgent(t, f.public.URL, true, nil)

	recs := f.agentSessions(t)
	require.Len(t, recs, 1)
	require.Equal(t, "hub-a", recs[0].RelayInstanceID)

	require.NoError(t, f.srv.RequestAgentTokenRotation(context.Background(), f.launched.ID))
	assert.Equal(t, int32(1), ca.rpcs.Load())
	assert.Equal(t, int32(1), ca.kicks.Load())
	assert.Equal(t, "/v1/control/rotate-token", <-ca.paths)
}

// TestRequestAgentTokenRotation_NotAdvertised: a connected agent whose
// session does not list rotate-token in Hello.capabilities.rpc (though it
// would serve it) gets ErrAgentControlNotAdvertised, and no RPC reaches
// it, locally or across nodes.
func TestRequestAgentTokenRotation_NotAdvertised(t *testing.T) {
	for _, crossNode := range []bool{false, true} {
		name := map[bool]string{false: "local", true: "cross-node"}[crossNode]
		t.Run(name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			hubURL := f.public.URL
			if crossNode {
				hubURL = f.startPeerRelay(t, "hub-b")
			}
			ca := f.startControlAgent(t, hubURL, false, nil)
			recs := f.agentSessions(t)
			require.Len(t, recs, 1)
			require.Empty(t, recs[0].Capabilities.RPC)

			err := f.srv.RequestAgentTokenRotation(context.Background(), f.launched.ID)
			require.ErrorIs(t, err, ErrAgentControlNotAdvertised)
			assert.NotErrorIs(t, err, router.ErrNoSession)
			assert.Equal(t, int32(0), ca.rpcs.Load(), "no RPC was sent")
			assert.Equal(t, int32(0), ca.kicks.Load())
		})
	}
}

// TestRequestAgentTokenRotation_NoSession: an agent without a conduit
// session gets router.ErrNoSession itself, and nothing reaches its
// broker. The broker path is live (broker row, token generator, recording
// broker client): the positive control at the end shows that a reset-auth
// dispatch for this agent would reach the client, so a fallback would be
// observed.
func TestRequestAgentTokenRotation_NoSession(t *testing.T) {
	f := newPTYConduitFixture(t)
	broker := &mockRuntimeBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(f.store, broker, false, slog.Default())
	disp.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	f.srv.SetDispatcher(disp)
	require.Empty(t, f.agentSessions(t))

	err := f.srv.RequestAgentTokenRotation(context.Background(), f.launched.ID)
	require.Equal(t, router.ErrNoSession, err, "ErrNoSession is returned unchanged")
	assert.False(t, broker.resetAuthCalled, "no broker reset-auth fallback")
	assert.False(t, broker.startCalled || broker.stopCalled || broker.restartCalled || broker.messageCalled,
		"no other broker call")

	// Positive control: the broker path is reachable for this agent.
	agent, err := f.store.GetAgent(context.Background(), f.launched.ID)
	require.NoError(t, err)
	require.NoError(t, f.srv.GetDispatcher().DispatchAgentResetAuth(context.Background(), agent))
	require.True(t, broker.resetAuthCalled, "a reset-auth dispatch reaches the broker client")
	assert.Equal(t, "test-token", broker.lastResetToken)
}

// TestRequestAgentTokenRotation_Outcomes: 202 from the real control
// handler is nil (one kick); every other status the agent answers,
// including other 2xx, is an AgentControlStatusError carrying it, after
// exactly one RPC; an agent without a session is router.ErrNoSession with
// no RPC.
func TestRequestAgentTokenRotation_Outcomes(t *testing.T) {
	tests := []struct {
		name string
		// status is what the agent answers: 0 means no agent session,
		// http.StatusAccepted the real control handler, anything else an
		// interceptor in front of it.
		status int
	}{
		{"202 accepted", http.StatusAccepted},
		{"no session", 0},
		{"200", http.StatusOK},
		{"204", http.StatusNoContent},
		{"400", http.StatusBadRequest},
		{"404", http.StatusNotFound},
		{"405", http.StatusMethodNotAllowed},
		{"500", http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			var ca *controlAgent
			switch tc.status {
			case 0:
			case http.StatusAccepted:
				ca = f.startControlAgent(t, f.public.URL, true, nil)
			default:
				ca = f.startControlAgent(t, f.public.URL, true, func(conduit.RPCHandler) conduit.RPCHandler {
					return conduit.RPCHandlerFunc(func(context.Context, *conduitv1.RpcRequest) *conduitv1.RpcResponse {
						return &conduitv1.RpcResponse{Status: int32(tc.status)}
					})
				})
			}

			err := f.srv.RequestAgentTokenRotation(context.Background(), f.launched.ID)
			switch tc.status {
			case 0:
				require.Equal(t, router.ErrNoSession, err)
				require.Empty(t, f.agentSessions(t))
			case http.StatusAccepted:
				require.NoError(t, err)
				assert.Equal(t, int32(1), ca.rpcs.Load())
				assert.Equal(t, int32(1), ca.kicks.Load())
			default:
				var se *AgentControlStatusError
				require.ErrorAs(t, err, &se)
				assert.Equal(t, tc.status, se.Status)
				assert.Equal(t, agentControlRouteRotateToken, se.Route)
				assert.Equal(t, f.launched.ID, se.AgentID)
				assert.NotErrorIs(t, err, router.ErrNoSession)
				assert.Equal(t, int32(1), ca.rpcs.Load(), "one call, no retry")
				assert.Equal(t, int32(0), ca.kicks.Load())
			}
		})
	}
}

// TestRequestAgentTokenRotation_FlagOff: with hub.conduit off, or no
// relay on this node, the call returns router.ErrNoSession before reading
// the agent row (an unknown agent id gives the same answer) and sends
// nothing.
func TestRequestAgentTokenRotation_FlagOff(t *testing.T) {
	t.Run("off, no relay", func(t *testing.T) {
		f := newConduitFixture(t)
		setConduitExperiment(t, f.srv, false)
		require.Equal(t, router.ErrNoSession, f.srv.RequestAgentTokenRotation(context.Background(), f.agent.ID))
		require.Equal(t, router.ErrNoSession, f.srv.RequestAgentTokenRotation(context.Background(), "no-such-agent"))
	})
	t.Run("on, no relay", func(t *testing.T) {
		f := newConduitFixture(t)
		require.Nil(t, f.srv.conduit.Load())
		require.Equal(t, router.ErrNoSession, f.srv.RequestAgentTokenRotation(context.Background(), f.agent.ID))
	})
	t.Run("turned off with a connected agent", func(t *testing.T) {
		f := newPTYConduitFixture(t)
		ca := f.startControlAgent(t, f.public.URL, true, nil)
		setConduitExperiment(t, f.srv, false)
		require.False(t, f.srv.conduitServing())
		require.Equal(t, router.ErrNoSession, f.srv.RequestAgentTokenRotation(context.Background(), f.launched.ID))
		require.Equal(t, router.ErrNoSession, f.srv.RequestAgentTokenRotation(context.Background(), "no-such-agent"))
		assert.Equal(t, int32(0), ca.rpcs.Load())
		assert.Equal(t, int32(0), ca.kicks.Load())
	})
}

// TestRequestAgentTokenRotation_CallerCancel: cancelling the caller's ctx
// while the agent is handling the request cancels the agent-side handler
// (RpcCancel, through the owner relay when cross-node) and returns the
// cancellation.
func TestRequestAgentTokenRotation_CallerCancel(t *testing.T) {
	for _, crossNode := range []bool{false, true} {
		name := map[bool]string{false: "local", true: "cross-node"}[crossNode]
		t.Run(name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			hubURL := f.public.URL
			if crossNode {
				hubURL = f.startPeerRelay(t, "hub-b")
			}
			entered := make(chan struct{})
			handlerDone := make(chan error, 1)
			ca := f.startControlAgent(t, hubURL, true, func(conduit.RPCHandler) conduit.RPCHandler {
				return conduit.RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
					close(entered)
					<-ctx.Done()
					handlerDone <- ctx.Err()
					return &conduitv1.RpcResponse{Status: http.StatusAccepted}
				})
			})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- f.srv.RequestAgentTokenRotation(ctx, f.launched.ID) }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the request did not reach the agent")
			}
			cancel()
			select {
			case err := <-result:
				require.ErrorIs(t, err, context.Canceled)
				assert.False(t, errors.Is(err, router.ErrNoSession))
			case <-time.After(10 * time.Second):
				t.Fatal("the call did not return after cancel")
			}
			select {
			case err := <-handlerDone:
				assert.ErrorIs(t, err, context.Canceled, "the agent-side handler was cancelled")
			case <-time.After(10 * time.Second):
				t.Fatal("the cancel did not reach the agent")
			}
			assert.Equal(t, int32(1), ca.rpcs.Load())
			assert.Equal(t, int32(0), ca.kicks.Load())
		})
	}
}
