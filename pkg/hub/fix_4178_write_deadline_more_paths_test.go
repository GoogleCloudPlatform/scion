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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4178: the remaining paths that wait on the broker past the
// listener's WriteTimeout (lifecycle stop and suspend, a direct message that
// wakes its target, and the workspace apply to a running agent) extend the
// write deadline as ptone/scion#3890 did for the launch paths. Each test
// serves through a listener whose WriteTimeout (200ms) is shorter than the
// path's wait and requires the hub's real answer, not a dropped connection.

// slowStopDispatcher answers the stop dispatch after delay (as
// slowLaunchDispatcher does) and holds the pre-stop workspace check exec
// until its deadline, as a check that never answers would.
type slowStopDispatcher struct {
	slowLaunchDispatcher
	execCalls atomic.Int32
}

func (d *slowStopDispatcher) DispatchAgentExec(ctx context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	d.execCalls.Add(1)
	<-ctx.Done()
	return "", 0, ctx.Err()
}

// The stop waits on three steps in turn: the workspace sync-back to the
// broker, the ephemeral workspace check and the stop dispatch. The timeouts
// are set so the three together outlast the one-dispatch budget but not the
// stop's own: the stop site must pass stopWriteBudget.
func TestSlowLifecycleStop_AfterWriteTimeout_GetsResponse(t *testing.T) {
	const (
		uploadDelay = 350 * time.Millisecond
		stopDelay   = 350 * time.Millisecond
		checkBudget = 300 * time.Millisecond
	)
	t.Setenv("HOME", t.TempDir())
	disp := &slowStopDispatcher{slowLaunchDispatcher: slowLaunchDispatcher{delay: stopDelay}}
	srv, s, project := setupCreateAgentServer(t, disp) // hub-managed: no GitRemote.
	shortenSyncDispatchTimeout(t, 600*time.Millisecond)
	setSyncDispatchWriteSlack(t, 50*time.Millisecond)
	setWorkspaceCheckTimeout(t, checkBudget)
	// The check exec runs until its share of the check budget runs out.
	minWait := uploadDelay + checkBudget - workspaceRecordReserve() + stopDelay
	require.Less(t, syncDispatchWriteBudget(), minWait, "fixture check: the one-dispatch budget must not cover the stop's steps")
	require.Greater(t, stopWriteBudget(), minWait+500*time.Millisecond, "fixture check: the stop budget must cover the stop's steps")
	setAgentQuotaLimits(t, s)

	srv.SetStorage(newContentMockStorage("test-bucket"))
	var downloaded atomic.Bool
	srv.setHubWorkspaceDownloader(func(context.Context, string, string, string) error {
		downloaded.Store(true)
		return nil
	})
	agent := &store.Agent{
		ID:              tid("agent-site-slow-stop"),
		Slug:            "slow-stop",
		Name:            "slow-stop",
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
		Runtime:         "kubernetes",
		Phase:           string(state.PhaseRunning),
	}
	ctx := context.Background()
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	require.NoError(t, s.SetAgentWorkspacePlacement(ctx, agent.ID, api.WorkspacePlacementLocal))

	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	uploaded := make(chan string, 1)
	go func() {
		for {
			var env wsprotocol.RequestEnvelope
			if err := broker.ws.ReadJSON(&env); err != nil {
				return
			}
			if env.Type != wsprotocol.TypeRequest {
				continue
			}
			uploaded <- env.Path
			time.Sleep(uploadDelay)
			body, _ := json.Marshal(RuntimeBrokerWorkspaceUploadResponse{})
			_ = broker.ws.WriteJSON(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"}, body))
			return
		}
	}()

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil, minWait)
	select {
	case path := <-uploaded:
		assert.Equal(t, "/api/v1/workspace/upload", path, "fixture check: the sync-back is tunneled")
	default:
		t.Fatal("fixture check: the sync-back never reached the broker")
	}
	assert.True(t, downloaded.Load(), "fixture check: the sync-back downloads into the hub workspace")
	assert.Equal(t, int32(1), disp.execCalls.Load(), "fixture check: the workspace check runs")
	assert.Equal(t, http.StatusOK, code, string(body))
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
}

func TestSlowLifecycleSuspend_AfterWriteTimeout_GetsResponse(t *testing.T) {
	srv, s, project := setupSlowLaunchServer(t)
	setAgentQuotaLimits(t, s)
	agent := createSiteAgent(t, s, project, "slow-suspend", state.PhaseRunning, store.RunIntentRunning)

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil, slowPathDelay)
	assert.Equal(t, http.StatusOK, code, string(body))
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
}

// The apply to a running agent is tunneled to the broker over the control
// channel; the fake broker answers after slowPathDelay.
func TestSlowWorkspaceApplyRunning_AfterWriteTimeout_GetsResponse(t *testing.T) {
	srv, s, project := setupSlowLaunchServer(t)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	agent := createSiteAgent(t, s, project, "slow-apply", state.PhaseRunning, store.RunIntentRunning)

	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	applied := make(chan string, 1)
	go func() {
		for {
			var env wsprotocol.RequestEnvelope
			if err := broker.ws.ReadJSON(&env); err != nil {
				return
			}
			if env.Type != wsprotocol.TypeRequest {
				continue
			}
			applied <- env.Path
			time.Sleep(slowPathDelay)
			body, _ := json.Marshal(RuntimeBrokerWorkspaceApplyResponse{Applied: true})
			_ = broker.ws.WriteJSON(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"}, body))
			return
		}
	}()

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize", SyncToFinalizeRequest{
		Manifest: &transfer.Manifest{Version: "1.0"},
	}, slowPathDelay)
	select {
	case path := <-applied:
		assert.Equal(t, "/api/v1/workspace/apply", path, "fixture check: the running-agent apply is tunneled")
	default:
		t.Fatal("fixture check: the apply never reached the broker")
	}
	assert.Equal(t, http.StatusOK, code, string(body))
	var resp SyncToFinalizeResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.True(t, resp.Applied)
}

// serveAsIdentityThroughSlowListener serves h through a real http.Server
// whose WriteTimeout is slowPathWriteTimeout (the hub's configured one too),
// with identity on every request context, and returns the status and body.
// It fails the test if the response is dropped.
func serveAsIdentityThroughSlowListener(t *testing.T, srv *Server, identity Identity, h http.HandlerFunc, method, path string, body any, minElapsed time.Duration) (int, []byte) {
	t.Helper()
	srv.config.WriteTimeout = slowPathWriteTimeout
	hs := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := contextWithIdentity(r.Context(), identity)
		if user, ok := identity.(UserIdentity); ok {
			ctx = context.WithValue(ctx, userContextKey{}, user)
		}
		h(w, r.WithContext(ctx))
	}))
	hs.Config.WriteTimeout = slowPathWriteTimeout
	hs.Start()
	t.Cleanup(hs.Close)

	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, hs.URL+path, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := hs.Client().Do(req)
	require.NoError(t, err, "the response must not be dropped at the listener's WriteTimeout")
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err, "the response body must arrive in full")
	require.GreaterOrEqual(t, time.Since(start), minElapsed, "fixture check: the wait must outlast the WriteTimeout")
	return resp.StatusCode, buf.Bytes()
}

// signalReadyAfterResume posts the resumed agent's first status once the
// wake has dispatched the resume and marked the agent starting, as the new
// container does. The wake's readiness poll then answers on its first tick
// (500ms), well after the 200ms WriteTimeout.
func signalReadyAfterResume(t *testing.T, s store.Store, disp *wakeTrackingDispatcher, agentID string) {
	t.Helper()
	go func() {
		bg := context.Background()
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			if len(disp.getStartCalls()) > 0 {
				if got, err := s.GetAgent(bg, agentID); err == nil && got.Phase == string(state.PhaseStarting) {
					_ = s.UpdateAgentStatus(bg, agentID, store.AgentStatusUpdate{Activity: "idle"})
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// requireWokeAndDelivered checks the wake resumed the target once and the
// message was delivered.
func requireWokeAndDelivered(t *testing.T, disp *wakeTrackingDispatcher, msg string) {
	t.Helper()
	require.Len(t, disp.getStartCalls(), 1, "fixture check: exactly one resume")
	msgs := disp.getMessageCalls()
	require.Len(t, msgs, 1, "the message is delivered")
	assert.Equal(t, msg, msgs[0].Message)
}

// wakeReadyFirstPoll is the readiness poll's first tick
// (waitForAgentReady): a wake waits at least this long.
const wakeReadyFirstPoll = 500 * time.Millisecond

// shrinkBudgetsBelowWake sets the timeouts so the one-dispatch budget does
// not cover a wake's readiness wait but the wake's own budget does: a DM
// wake site must pass dmWakeWriteBudget. The resume dispatch is immediate.
func shrinkBudgetsBelowWake(t *testing.T) {
	t.Helper()
	shortenSyncDispatchTimeout(t, 200*time.Millisecond)
	setSyncDispatchWriteSlack(t, 50*time.Millisecond)
	require.Less(t, syncDispatchWriteBudget(), wakeReadyFirstPoll, "fixture check: the one-dispatch budget must not cover the wake")
	require.Greater(t, dmWakeWriteBudget(), wakeReadyFirstPoll+time.Second, "fixture check: the wake budget must cover the wake")
}

// A user's message with wake (the inline wake in handleAgentMessage).
func TestSlowDMWake_UserMessage_AfterWriteTimeout_GetsResponse(t *testing.T) {
	shrinkBudgetsBelowWake(t)
	srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	u := wakeLifecycleUser(t, s, "wake-slow-user", target.ProjectID, "agent.message", "agent.read", "agent.lifecycle")
	signalReadyAfterResume(t, s, disp, target.ID)

	code, body := serveAsIdentityThroughSlowListener(t, srv, authUser(u), srv.mux.ServeHTTP,
		http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
		map[string]interface{}{"message": "slow wake", "wake": true}, wakeReadyFirstPoll)
	assert.Equal(t, http.StatusOK, code, string(body))
	requireWokeAndDelivered(t, disp, "slow wake")
}

// An agent's message with wake through the message route (ExecuteAgentDM,
// whose BeforeWake the route sets).
func TestSlowDMWake_AgentMessage_AfterWriteTimeout_GetsResponse(t *testing.T) {
	shrinkBudgetsBelowWake(t)
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	signalReadyAfterResume(t, s, disp, target.ID)
	ident := wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)

	code, body := serveAsIdentityThroughSlowListener(t, srv, ident, func(w http.ResponseWriter, r *http.Request) {
		srv.handleAgentMessage(w, r, target.ID)
	}, http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
		map[string]interface{}{"message": "slow agent wake", "wake": true}, wakeReadyFirstPoll)
	assert.Equal(t, http.StatusOK, code, string(body))
	requireWokeAndDelivered(t, disp, "slow agent wake")
}

// An agent's outbound message with wake to another agent (ExecuteAgentDM,
// whose BeforeWake the outbound route sets).
func TestSlowDMWake_AgentOutbound_AfterWriteTimeout_GetsResponse(t *testing.T) {
	shrinkBudgetsBelowWake(t)
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	signalReadyAfterResume(t, s, disp, target.ID)
	ident := wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	code, body := serveAsIdentityThroughSlowListener(t, srv, ident, func(w http.ResponseWriter, r *http.Request) {
		srv.handleAgentOutboundMessage(w, r, sender.ID)
	}, http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		Msg:             "slow outbound wake",
		Type:            "instruction",
		Wake:            true,
	}, wakeReadyFirstPoll)
	assert.Equal(t, http.StatusOK, code, string(body))
	requireWokeAndDelivered(t, disp, "slow outbound wake")
}

// At production values the new budgets outlast the default WriteTimeout
// and the one-dispatch budget. Which site uses which budget is checked by
// the stop and DM wake tests above.
func TestWriteBudgets_4178(t *testing.T) {
	def := DefaultServerConfig().WriteTimeout
	for _, b := range []time.Duration{stopWriteBudget(), dmWakeWriteBudget()} {
		assert.Greater(t, b, def)
		assert.Greater(t, b, syncDispatchWriteBudget())
	}
}
