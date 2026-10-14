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
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#5054: /readyz answers 200 until the hub begins shutting
// down, then 503 not_ready "shutting down", so a front proxy stops routing
// to a draining process.
func TestReadyz_ShuttingDown(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequestNoAuth(t, srv, http.MethodGet, "/readyz", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	srv.BeginShutdown()
	rec = doRequestNoAuth(t, srv, http.MethodGet, "/readyz", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "readyz during shutdown")
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "not_ready", body["status"])
	assert.Equal(t, "shutting down", body["reason"])
}

// ptone/scion#5054 (R5): once the hub begins shutting down, an
// authenticated user request is refused with 503 unavailable, Retry-After
// and Connection: close, while /healthz still answers. The refusal runs
// after authentication: it is keyed on the authenticated principal.
func TestRefuseDuringShutdown_HubAPI(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents", nil)
	require.Equal(t, http.StatusOK, rec.Code, "served before the mark")

	srv.BeginShutdown()
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "10", rec.Header().Get("Retry-After"))
	assert.Equal(t, "close", rec.Header().Get("Connection"))
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeUnavailable, body.Error.Code)

	rec = doRequestNoAuth(t, srv, http.MethodGet, "/healthz", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "health endpoints stay reachable")
}

// R5: the exemption is keyed on the authenticated broker principal in the
// request context: a request authenticated as a Runtime Broker passes, the
// same request without that identity is refused, and a request carrying a
// forged broker header (no valid signature) through the full middleware
// chain is never served.
func TestRefuseDuringShutdown_BrokerExemption(t *testing.T) {
	var st shutdownState
	st.begin()
	var reached atomic.Int32
	h := st.refuseDuringShutdown(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}), isBrokerRequest)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/b1/heartbeat", nil)
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("b1")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "authenticated broker request served")

	req = httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/b1/heartbeat", nil)
	req.Header.Set(HeaderBrokerID, "b1") // a header alone is not an identity
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "unauthenticated broker-shaped request refused")
	assert.Equal(t, int32(1), reached.Load())

	srv, _ := testServer(t)
	srv.BeginShutdown()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/b1/heartbeat", nil)
	req.Header.Set(HeaderBrokerID, "b1")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable}, rec.Code,
		"a forged broker header must not be served during shutdown (got %d)", rec.Code)
}

// The web server leaves requests for the mounted hub API to the hub's own
// refusal (which knows the caller) and refuses its own routes.
func TestWebServer_IsMountedHubRequest(t *testing.T) {
	ws := &WebServer{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	assert.False(t, ws.isMountedHubRequest(req), "no hub mounted")
	ws.hubHandler = http.NotFoundHandler()
	assert.True(t, ws.isMountedHubRequest(req))
	assert.True(t, ws.isMountedHubRequest(httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)))
	assert.False(t, ws.isMountedHubRequest(httptest.NewRequest(http.MethodGet, "/agents", nil)))
}

// Streams (SSE) end when the server begins shutting down: their context is
// cancelled at the mark, so they do not hold the HTTP drain; before the
// mark only the request's own context ends them.
func TestShutdownState_StreamContext(t *testing.T) {
	var st shutdownState
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	ctx, end := st.streamContext(req)
	defer end()
	select {
	case <-ctx.Done():
		t.Fatal("stream context ended before the mark")
	default:
	}
	st.begin()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stream context not cancelled at the mark")
	}
	// A stream opened after the mark ends at once.
	ctx2, end2 := st.streamContext(req)
	defer end2()
	select {
	case <-ctx2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stream opened after the mark did not end")
	}
}

// R4: at the mark the hub stops its own dispatches toward the brokers:
// every dispatch fails with ErrHubShuttingDown before any broker call
// (not "broker not connected"), broker connection work admits nothing new,
// and the scheduler, the scheduled-send sweeper and the auto-suspend
// worker stop starting work.
func TestBeginShutdown_StopsHubDispatch(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	broker := &store.RuntimeBroker{ID: tid("host-1"), Name: "test-host", Slug: "test-host", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	srv.SetDispatcher(d)
	// testServer does not start the background services; give it a
	// scheduler (not started) so the mark's halt is observable.
	srv.mu.Lock()
	srv.scheduler = NewScheduler(s, slog.Default())
	srv.mu.Unlock()
	agent := &store.Agent{ID: tid("agent-1"), Name: "test-agent", Slug: "test-agent", RuntimeBrokerID: broker.ID}

	require.NoError(t, d.DispatchAgentStop(ctx, agent), "dispatch works before the mark")
	require.True(t, client.stopCalled)
	client.stopCalled = false

	srv.BeginShutdown()
	assert.True(t, srv.ShuttingDown())

	err := d.DispatchAgentStop(ctx, agent)
	require.Error(t, err, "a dispatch after the mark must fail")
	assert.True(t, errors.Is(err, ErrHubShuttingDown), "got %v", err)
	assert.False(t, client.stopCalled, "no broker call after the mark")
	err = d.DispatchAgentMessage(ctx, agent, "hello", false, nil)
	assert.True(t, errors.Is(err, ErrHubShuttingDown), "message dispatch: got %v", err)
	assert.False(t, client.messageCalled)

	assert.True(t, srv.brokerWork.closing(), "broker connection work admission closed")
	srv.mu.RLock()
	scheduler := srv.scheduler
	srv.mu.RUnlock()
	select {
	case <-scheduler.stopCh:
	default:
		t.Error("scheduler not halted at the mark")
	}
	rt := srv.scheduledRuntime()
	rt.mu.Lock()
	sweeperStopped := rt.stopped
	rt.mu.Unlock()
	assert.True(t, sweeperStopped, "scheduled-send sweeper not halted at the mark")
	srv.autoSuspend.mu.Lock()
	autoSuspendStopped := srv.autoSuspend.stopped
	srv.autoSuspend.mu.Unlock()
	assert.True(t, autoSuspendStopped, "auto-suspend worker not halted at the mark")
}

// R1 (ptone/scion#5054): Server.Shutdown waits for the co-located broker
// (BeforeHTTPDrain) while the listener still accepts, then closes the
// listener and drains in-flight requests, and only then runs the
// background teardown: while a request is still draining the listener is
// closed and the teardown has not started.
func TestServerShutdown_BrokerThenListenerThenTeardown(t *testing.T) {
	srv, _ := testServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()

	inFlight, release := make(chan struct{}), make(chan struct{})
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inFlight)
		<-release
		w.WriteHeader(http.StatusOK)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	srv.mu.Lock()
	srv.httpServer = hs
	srv.mu.Unlock()

	reqDone := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			reqDone <- 0
			return
		}
		_ = resp.Body.Close()
		reqDone <- resp.StatusCode
	}()
	<-inFlight

	var brokerWaited atomic.Bool
	srv.SetShutdownHooks(ShutdownHooks{
		BeforeHTTPDrain: func(context.Context) {
			if brokerWaited.Load() {
				return // the test cleanup's second Shutdown
			}
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if assert.NoError(t, err, "the listener must still accept during the broker wait") {
				_ = conn.Close()
			}
			assert.NoError(t, srv.ctx.Err(), "the teardown started before the broker wait")
			brokerWaited.Store(true)
		},
	})
	shutdownDone := make(chan struct{})
	go func() {
		_ = srv.Shutdown(context.Background())
		close(shutdownDone)
	}()

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		_ = conn.Close()
		return false
	}, 5*time.Second, 10*time.Millisecond, "the listener did not close")
	assert.True(t, brokerWaited.Load(), "BeforeHTTPDrain did not run before the listener closed")
	assert.NoError(t, srv.ctx.Err(), "the background teardown started while a request was still draining")

	close(release)
	assert.Equal(t, http.StatusOK, <-reqDone, "the in-flight request completes")
	select {
	case <-shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	assert.Error(t, srv.ctx.Err(), "the background teardown ran after the drain")
}

// The HTTP drain is bounded by HTTPDrainContext (cmd: the HTTP-drain
// deadline of the shutdown budget): a request that never finishes is cut
// at that bound and the teardown still runs.
func TestServerShutdown_HTTPDrainBounded(t *testing.T) {
	srv, _ := testServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	inFlight := make(chan struct{})
	block := make(chan struct{})
	defer close(block)
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inFlight)
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	srv.mu.Lock()
	srv.httpServer = hs
	srv.mu.Unlock()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-inFlight

	drainCtx, cancelDrain := context.WithCancel(context.Background())
	srv.SetShutdownHooks(ShutdownHooks{
		HTTPDrainContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return drainCtx, func() {}
		},
	})
	shutdownDone := make(chan struct{})
	go func() {
		_ = srv.Shutdown(context.Background())
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while the drain was still open")
	case <-time.After(100 * time.Millisecond):
	}
	cancelDrain() // the HTTP-drain deadline
	select {
	case <-shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the HTTP-drain bound")
	}
	assert.Error(t, srv.ctx.Err(), "the teardown ran after the bounded drain")
}
