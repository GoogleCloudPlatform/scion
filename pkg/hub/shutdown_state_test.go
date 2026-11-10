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
	"sync"
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

	// A broker request on behalf of a user is user work (code review K1).
	user := NewAuthenticatedUser("u1", "user@example.com", "User", "member", "cli")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
	ctx := contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("b1"))
	ctx = context.WithValue(contextWithIdentity(ctx, user), userContextKey{}, user)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "a broker request on behalf of a user is refused")
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

// R5 through the real middleware chain (code review Q2): the refusal runs
// inside BrokerAuth, so after the mark a validly signed broker request is
// answered exactly as before it (not 503), while the same path with a user
// token gets 503 + Retry-After.
func TestRefuseDuringShutdown_SignedBrokerServedFullChain(t *testing.T) {
	srv, s := testServer(t)
	require.NotNil(t, srv.brokerAuthService, "the test server must run broker HMAC auth")
	brokerID, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)
	path := "/api/v1/runtime-brokers/" + brokerID

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, signReq(http.MethodGet, path, nil))
	before := rec.Code
	require.NotEqual(t, http.StatusServiceUnavailable, before)
	require.NotEqual(t, http.StatusUnauthorized, before, "the signed broker request must authenticate")

	srv.BeginShutdown()
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, signReq(http.MethodGet, path, nil))
	assert.Equal(t, before, rec.Code, "a signed broker request is served during shutdown as before the mark")
	assert.Empty(t, rec.Header().Get("Retry-After"))

	rec = doRequest(t, srv, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "a user request to the same path is refused during shutdown")
	assert.Equal(t, "10", rec.Header().Get("Retry-After"))
}

// The web server leaves requests for the mounted hub API to the hub's own
// refusal (which knows the caller) and refuses its own routes.
func TestWebServer_IsMountedHubRequest(t *testing.T) {
	ws := &WebServer{mux: http.NewServeMux()}
	ws.mux.Handle("/api/v1/auth/test-login", http.NotFoundHandler()) // a web route under /api/v1/
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	assert.False(t, ws.isMountedHubRequest(req), "no hub mounted")
	ws.MountHubAPI(http.NotFoundHandler(), nil)
	assert.True(t, ws.isMountedHubRequest(req))
	assert.True(t, ws.isMountedHubRequest(httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)))
	assert.False(t, ws.isMountedHubRequest(httptest.NewRequest(http.MethodGet, "/agents", nil)))
	assert.False(t, ws.isMountedHubRequest(httptest.NewRequest(http.MethodPost, "/api/v1/auth/test-login", nil)), "a web route under /api/v1/ stays with the web server's refusal (code review K4)")
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

// ptone/scion#4668: WebServer.Shutdown keeps the same order as
// Server.Shutdown. With the web front enabled the hub API is mounted on
// the web server, so a co-located broker's reports (its launch drain's
// broker_restarted reports among them) arrive on the web listener: the
// web server waits for the broker (BeforeHTTPDrain) while its listener
// still accepts, then closes the listener and drains in-flight requests,
// and only then runs the mounted hub's teardown.
func TestWebServerShutdown_BrokerThenListenerThenHubTeardown(t *testing.T) {
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
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = hs.Close()
	})

	ws := NewWebServer(WebServerConfig{Host: "127.0.0.1"})
	var hubTornDown atomic.Bool
	ws.MountHubAPI(http.NotFoundHandler(), func(context.Context) error {
		hubTornDown.Store(true)
		return nil
	})
	ws.httpServer = hs

	reqDone := make(chan int, 1)
	go func() {
		// Bounded, but well past the test's own waits (the listener
		// close and the drain), so the timeout never fails the in-flight
		// request before the test releases it.
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get("http://" + addr + "/")
		if err != nil {
			reqDone <- 0
			return
		}
		_ = resp.Body.Close()
		reqDone <- resp.StatusCode
	}()
	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	var brokerWaited, listenerOpenInWait, tornDownInWait atomic.Bool
	ws.SetShutdownHooks(ShutdownHooks{
		BeforeHTTPDrain: func(context.Context) {
			listenerOpenInWait.Store(shutdownTestDialable(addr))
			tornDownInWait.Store(hubTornDown.Load())
			brokerWaited.Store(true)
		},
	})
	shutdownDone := make(chan struct{})
	go func() {
		_ = ws.Shutdown(context.Background())
		close(shutdownDone)
	}()

	require.Eventually(t, func() bool { return !shutdownTestDialable(addr) }, 5*time.Second, 10*time.Millisecond, "the web listener did not close")
	assert.True(t, brokerWaited.Load(), "web BeforeHTTPDrain did not run before the listener closed")
	assert.True(t, listenerOpenInWait.Load(), "web listener closed before the broker wait")
	assert.False(t, tornDownInWait.Load(), "the mounted hub was torn down before the broker wait")
	assert.False(t, hubTornDown.Load(), "the mounted hub was torn down while a request was still draining")

	releaseOnce.Do(func() { close(release) })
	assert.Equal(t, http.StatusOK, <-reqDone, "the in-flight request completes")
	select {
	case <-shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("WebServer.Shutdown did not return")
	}
	assert.True(t, hubTornDown.Load(), "the mounted hub's teardown ran after the drain")
}

// shutdownTestDialable reports whether addr accepts a TCP connection.
func shutdownTestDialable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
