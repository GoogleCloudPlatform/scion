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
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	sciontoollog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	scionportforward "github.com/GoogleCloudPlatform/scion/pkg/sciontool/portforward"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleAgentLogs_BrokerErrorBodyNotRelayed: a broker error on the logs
// path keeps its 502 and code, carries the hub's prefix and the broker's
// status, and none of the broker's body; the body reaches the hub log.
func TestHandleAgentLogs_BrokerErrorBodyNotRelayed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"json", `{"error":{"code":"runtime_error","message":"` + brokerBodyMarker + `"}}`},
		{"plain", "<html>" + brokerBodyMarker + "</html>"},
	} {
		body := tc.body
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			disp := &createAgentDispatcher{
				createPhase: string(state.PhaseRunning),
				logsErr:     newBrokerStatusError("broker-1", http.StatusInternalServerError, []byte(body), ""),
			}
			srv, _, project := setupCreateAgentServer(t, disp)
			agentID := createLoggableAgent(t, srv, project.ID)

			rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/logs", nil)

			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), brokerBodyMarker)
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeInternalError, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, "Failed to retrieve logs from broker: runtime broker returned error 500")
			assert.Contains(t, logs.String(), brokerBodyMarker, "the broker's body stays in the hub log")
		})
	}
}

// TestDeleteAgent_BrokerErrorBodyNotRelayed: a failed broker delete keeps
// its 502 and code, and the response carries none of the broker's body,
// for an admin caller too.
func TestDeleteAgent_BrokerErrorBodyNotRelayed(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&deleteDispatcher{
		deleteErr: &brokerStatusError{
			StatusCode: http.StatusInternalServerError,
			Body:       "<html>" + brokerBodyMarker + "</html>",
		},
	})
	_, _, agent := setupOnlineBrokerAgent(t, s, "body")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), brokerBodyMarker)
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
	assert.Equal(t, "Failed to delete agent on runtime broker: runtime broker returned error 500", errResp.Error.Message,
		"the admin caller gets the status, not the body")

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err, "the agent stays when the broker delete fails")
	assert.NotContains(t, got.Message, brokerBodyMarker)
}

// TestAgentPortProxy_TunnelErrorTextNotRelayed: when the agent side of the
// legacy port tunnel answers with an error (here: nothing listens on the
// port), the client gets 502, the same code and the fixed message, never
// the far end's error text.
func TestAgentPortProxy_TunnelErrorTextNotRelayed(t *testing.T) {
	srv, s := testServer(t)
	hubHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(hubHTTP.Close)

	// A loopback port with nothing listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": port,
		"host": "127.0.0.1",
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// As in TestAgentPortProxyThroughTunnel: trip sciontool/log's lazy Init
	// and restore the globals it replaces.
	prevDefault, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	sciontoollog.Init()
	slog.SetDefault(prevDefault)
	log.SetOutput(prevWriter)
	log.SetFlags(prevFlags)

	manager := scionportforward.NewManager(scionhub.NewClientWithConfig(hubHTTP.URL, token, agent.ID))
	go manager.Run(ctx)
	require.Eventually(t, func() bool {
		srv.portTunnels.mu.RLock()
		defer srv.portTunnels.mu.RUnlock()
		return srv.portTunnels.sessions[agent.ID] != nil
	}, 5*time.Second, 50*time.Millisecond)

	rec = doAgentTokenRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/"+strconv.Itoa(port)+"/proxy/hello", nil, token)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "dial tcp")
	assert.NotContains(t, rec.Body.String(), "connection refused")
	assert.NotContains(t, rec.Body.String(), "127.0.0.1")
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
	assert.Equal(t, portTunnelFailedMessage, errResp.Error.Message)
}

// TestHybridBrokerClient_ImageStatusErrorBodyNotInError: a broker error
// answer to an image status query gives an error naming the status only;
// the body reaches the hub log.
func TestHybridBrokerClient_ImageStatusErrorBodyNotInError(t *testing.T) {
	logs := captureSlog(t)
	f := newRoutedBrokerFixture(t, nil)
	f.dialRoutedBroker(t, routedBrokerOpts{answer: func(_ context.Context, _ string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
		return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusInternalServerError, nil,
			[]byte("<html>"+brokerBodyMarker+"</html>"))
	}})

	_, err := f.client.ImageStatus(context.Background(), f.brokerID, "", "img:short", "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime broker returned error 500")
	assert.NotContains(t, err.Error(), brokerBodyMarker)
	assert.Contains(t, logs.String(), brokerBodyMarker, "the broker's body stays in the hub log")
}

// TestAgentPortProxy_TunnelTransportErrorTextNotRelayed: when the tunnel
// request itself fails (here: the request's context ends while the agent
// side holds the request unanswered), the client gets 502, the same code and
// the fixed message, never the error's text.
func TestAgentPortProxy_TunnelTransportErrorTextNotRelayed(t *testing.T) {
	srv, s := testServer(t)
	hubHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(hubHTTP.Close)

	const port = 18765
	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": port,
		"host": "127.0.0.1",
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// A fake agent side: it opens the tunnel, reads the request and never
	// answers it.
	header := http.Header{}
	header.Set("X-Scion-Agent-Token", token)
	wsURL := "ws" + strings.TrimPrefix(hubHTTP.URL, "http") + "/api/v1/agents/" + agent.ID + "/ports/tunnel"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close() })
	received := make(chan struct{}, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			select {
			case received <- struct{}{}:
			default:
			}
		}
	}()
	require.Eventually(t, func() bool {
		srv.portTunnels.mu.RLock()
		defer srv.portTunnels.mu.RUnlock()
		return srv.portTunnels.sessions[agent.ID] != nil
	}, 5*time.Second, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-received:
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/"+strconv.Itoa(port)+"/proxy/hello", nil).WithContext(ctx)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "context canceled", "the tunnel error's text must not reach the client")
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
	assert.Equal(t, portTunnelFailedMessage, errResp.Error.Message)
}

// TestWorkspaceSyncFrom_BrokerErrorBodyNotRelayed: a broker error answer to
// the sync-from upload keeps the hub's 502 and code, with the generic text
// and none of the broker's body; the body reaches the hub log.
func TestWorkspaceSyncFrom_BrokerErrorBodyNotRelayed(t *testing.T) {
	logs := captureSlog(t)
	t.Setenv("HOME", t.TempDir())
	srv, s, project := setupCreateAgentServer(t, &slowLaunchDispatcher{})
	shortenSyncDispatchTimeout(t, 3*time.Second)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	agent := createSiteAgent(t, s, project, "sync-from-broker-error", state.PhaseRunning, store.RunIntentRunning)
	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	uploaded := answerBrokerUploadWith(t, broker, http.StatusInternalServerError,
		[]byte(`{"error":{"code":"runtime_error","message":"`+brokerBodyMarker+`"}}`))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-from", nil)
	select {
	case <-uploaded:
	default:
		t.Fatal("fixture check: the upload never reached the broker")
	}

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), brokerBodyMarker)
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), rec.Body.String())
	assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
	assert.Equal(t, "Failed to sync workspace: runtime broker returned error 500 (runtime_error)", errResp.Error.Message)
	assert.Contains(t, logs.String(), brokerBodyMarker, "the broker's body stays in the hub log")
}

// TestWorkspaceApply_BrokerErrorBodyNotRelayed: a broker error answer to the
// running-agent apply keeps the hub's 502 and code, with the generic text
// and none of the broker's body; the body reaches the hub log.
func TestWorkspaceApply_BrokerErrorBodyNotRelayed(t *testing.T) {
	logs := captureSlog(t)
	srv, s, project := setupSlowLaunchServer(t)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	agent := createSiteAgent(t, s, project, "apply-broker-error", state.PhaseRunning, store.RunIntentRunning)
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
			_ = broker.ws.WriteJSON(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusInternalServerError,
				map[string]string{"Content-Type": "text/plain"}, []byte("<html>"+brokerBodyMarker+"</html>")))
			return
		}
	}()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize", SyncToFinalizeRequest{
		Manifest: &transfer.Manifest{Version: "1.0"},
	})
	select {
	case path := <-applied:
		assert.Equal(t, "/api/v1/workspace/apply", path, "fixture check: the running-agent apply is tunneled")
	default:
		t.Fatal("fixture check: the apply never reached the broker")
	}

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), brokerBodyMarker)
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), rec.Body.String())
	assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
	assert.Equal(t, "Failed to apply workspace: runtime broker returned error 500", errResp.Error.Message)
	assert.Contains(t, logs.String(), brokerBodyMarker, "the broker's body stays in the hub log")
}

// TestWorkspaceSyncFrom_BrokerTimeoutBodyStillGatewayTimeout: a broker error
// answer whose body names a timeout still maps to the hub's fixed 504 (the
// match reads the body through brokerErrorMatchText), and the reply carries
// none of the body.
func TestWorkspaceSyncFrom_BrokerTimeoutBodyStillGatewayTimeout(t *testing.T) {
	_ = captureSlog(t)
	t.Setenv("HOME", t.TempDir())
	srv, s, project := setupCreateAgentServer(t, &slowLaunchDispatcher{})
	shortenSyncDispatchTimeout(t, 3*time.Second)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	agent := createSiteAgent(t, s, project, "sync-from-broker-timeout", state.PhaseRunning, store.RunIntentRunning)
	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	uploaded := answerBrokerUploadWith(t, broker, http.StatusInternalServerError,
		[]byte(`{"error":{"code":"runtime_error","message":"upload timeout `+brokerBodyMarker+`"}}`))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-from", nil)
	select {
	case <-uploaded:
	default:
		t.Fatal("fixture check: the upload never reached the broker")
	}

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeBrokerTimeout)
	assert.NotContains(t, rec.Body.String(), brokerBodyMarker)
}

// markerBrokerError is a broker error answer whose body holds the marker.
func markerBrokerError(context.Context) error {
	return newBrokerStatusError("broker-n2", http.StatusInternalServerError,
		[]byte(`{"error":{"code":"runtime_error","message":"`+brokerBodyMarker+`"}}`), "")
}

// TestBrokerErrorBody_MessagePathsNotRelayed: a broker error answer on the
// group send and on the inbound broker-message path keeps the status-only
// text in results[].Error, the stored failure reason and the response,
// never the broker's body.
func TestBrokerErrorBody_MessagePathsNotRelayed(t *testing.T) {
	const wantText = "runtime broker returned error 500 (runtime_error)"

	t.Run("group send", func(t *testing.T) {
		_ = captureSlog(t)
		srv, s := testServer(t)
		projectID, agents := setupGroupTest(t, s, "n2-grp", map[string]string{
			"grp-a": string(state.PhaseRunning),
			"grp-b": string(state.PhaseRunning),
		})
		srv.SetDispatcher(&ctxHonoringDispatcher{fail: map[string]bool{"grp-b": true}, onFail: markerBrokerError})

		resp := sendGroup(t, srv, projectID, "grp-a", "group[agent:grp-a,agent:grp-b]")
		res := groupResult(t, resp, "agent:grp-b")
		assert.Equal(t, "failed", res.Status)
		assert.Equal(t, wantText, res.Error)
		assert.NotContains(t, res.Error, brokerBodyMarker)
		rows := rowsByAgent(t, s, agents["grp-b"].ID)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].DispatchFailureReason)
		assert.NotContains(t, *rows[0].DispatchFailureReason, brokerBodyMarker)
	})

	t.Run("inbound broker message", func(t *testing.T) {
		_ = captureSlog(t)
		srv, s := testServer(t)
		ctx := context.Background()
		broker := &store.RuntimeBroker{ID: tid("n2-inbound-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		project := &store.Project{ID: tid("n2-inbound-project"), Slug: "n2-inbound-project", Name: "n2-inbound-project"}
		require.NoError(t, s.CreateProject(ctx, project))
		target := &store.Agent{
			ID: tid("n2-inbound-target"), Slug: "n2-inbound-target", Name: "n2-inbound-target",
			ProjectID: project.ID, Phase: string(state.PhaseRunning),
			RuntimeBrokerID: broker.ID,
		}
		require.NoError(t, s.CreateAgent(ctx, target))
		senderEmail := "n2-inbound-sender@test.com"
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: tid("n2-inbound-sender"), Email: senderEmail, DisplayName: "Sender",
			Role: "admin", Status: store.UserStatusActive,
		}))
		srv.SetDispatcher(&ctxHonoringDispatcher{fail: map[string]bool{target.Slug: true}, onFail: markerBrokerError})

		bodyBytes, err := json.Marshal(inboundMessageRequest{
			Topic: "scion.project." + project.ID + ".agent." + target.Slug + ".messages",
			Message: &messages.StructuredMessage{
				Sender: "user:" + senderEmail, Recipient: "agent:" + target.Slug,
				Msg: "hello", Type: messages.TypeInstruction,
			},
		})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(broker.ID)))
		rec := httptest.NewRecorder()
		srv.handleBrokerInbound(rec, req)

		require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), brokerBodyMarker)
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
		assert.Equal(t, "failed to deliver message to agent: "+wantText, errResp.Error.Message)
	})
}
