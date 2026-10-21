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
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// shutdownRetryAfterSeconds is the Retry-After a request refused during
// shutdown carries: about the time a restarted server takes to listen
// again.
const shutdownRetryAfterSeconds = 10

// shutdownState is the "shutting down" mark of a server (ptone/scion#5054).
// begin is the first step of a graceful shutdown, before any listener or
// background service stops: from then on /readyz fails, new requests are
// refused with a retryable 503, and streaming responses end (their
// contexts are cancelled), so none of them holds the HTTP drain. The zero
// value is ready to use.
type shutdownState struct {
	flag atomic.Bool

	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
	initMu sync.Mutex
}

// init creates the stream context on first use.
func (st *shutdownState) init() {
	st.initMu.Lock()
	defer st.initMu.Unlock()
	if st.ctx == nil {
		st.ctx, st.cancel = context.WithCancel(context.Background())
	}
}

// begin marks the server shutting down and cancels every stream context.
// It is idempotent.
func (st *shutdownState) begin() {
	st.once.Do(func() {
		st.init()
		st.flag.Store(true)
		st.cancel()
	})
}

// shuttingDown reports whether begin has run.
func (st *shutdownState) shuttingDown() bool {
	return st.flag.Load()
}

// streamContext returns a context that ends when the request's context
// ends or when the server begins shutting down, whichever is first, and
// its cancel func (call it when the stream ends). Streaming handlers (SSE)
// use it so they do not hold the HTTP drain.
func (st *shutdownState) streamContext(r *http.Request) (context.Context, context.CancelFunc) {
	st.init()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(st.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// refuseDuringShutdown answers new requests with 503 unavailable,
// Retry-After and Connection: close once the server is shutting down, so a
// caller gets a clean retryable error instead of a half-torn-down server's
// answer or a refused connection. The health endpoints are exempt (/readyz
// itself reports not_ready, handleReadyz), and so is every request for
// which exempt (may be nil) returns true.
func (st *shutdownState) refuseDuringShutdown(next http.Handler, exempt func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !st.shuttingDown() || isHealthEndpoint(r.URL.Path) || (exempt != nil && exempt(r)) {
			next.ServeHTTP(w, r)
			return
		}
		writeShuttingDown(w)
	})
}

// isBrokerRequest reports whether r is authenticated as a Runtime Broker
// acting for itself. The hub lets these through during shutdown: a
// co-located broker still reports to the hub through the hub's listener
// while it shuts down, and the hub waits for it
// (ShutdownHooks.BeforeHTTPDrain) before closing the listener. A broker
// request on behalf of a user (a user identity in the context too) is user
// work and is refused like any other.
func isBrokerRequest(r *http.Request) bool {
	ctx := r.Context()
	return GetBrokerIdentityFromContext(ctx) != nil && GetUserIdentityFromContext(ctx) == nil
}

// isMountedHubRequest reports whether the web server's mux dispatches r to
// the mounted hub API (the "/api/v1/" and "/.well-known/" patterns of
// MountHubAPI). The hub handler applies its own shutdown refusal, which
// knows the caller's identity, so the web server leaves these requests to
// it. Web routes under /api/v1/ with their own, more specific pattern (for
// example /api/v1/auth/test-login) stay with the web server's refusal.
func (ws *WebServer) isMountedHubRequest(r *http.Request) bool {
	if ws.hubHandler == nil || ws.mux == nil {
		return false
	}
	_, pattern := ws.mux.Handler(r)
	return pattern == "/api/v1/" || pattern == "/.well-known/"
}

// writeShuttingDown writes the refusal of a request received during
// shutdown.
func writeShuttingDown(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(shutdownRetryAfterSeconds))
	w.Header().Set("Connection", "close")
	writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "the server is shutting down; retry shortly", nil)
}

// BeginShutdown is the first step of the hub's graceful shutdown
// (ptone/scion#5054): /readyz answers 503, new requests are refused with a
// retryable 503 and streaming responses end. Listeners and background
// services keep running until Shutdown. It is idempotent.
//
// It also stops the hub's own dispatches toward the Runtime Brokers, so
// none reaches a broker that is stopping: every dispatch resolved after
// the mark fails with ErrHubShuttingDown (dispatchShutdownGate), broker
// connection work admits nothing new (brokerWork.closeAdmission), and the
// dispatch producers stop starting work: the scheduler (recurring
// handlers and event timers), the scheduled-send sweeper loop and the
// auto-suspend worker. Work already running finishes or is joined by
// the background teardown, as before.
func (s *Server) BeginShutdown() {
	s.shutdownMark.begin()
	s.brokerWork.closeAdmission()
	s.haltDispatchProducers()
}

// ErrHubShuttingDown is the error of a hub dispatch toward a Runtime
// Broker attempted after the hub began shutting down (BeginShutdown). It
// is retryable once the hub is back.
var ErrHubShuttingDown = errors.New("the hub is shutting down; retry shortly")

// dispatchShutdownGate returns ErrHubShuttingDown once the hub has begun
// shutting down. The dispatcher calls it where every dispatch resolves
// its broker (HTTPAgentDispatcher.getBrokerEndpoint and
// getProvisioningBrokerEndpoint).
func (s *Server) dispatchShutdownGate() error {
	if s.ShuttingDown() {
		return ErrHubShuttingDown
	}
	return nil
}

// haltDispatchProducers stops the hub's background producers of broker
// dispatches from starting new work, without waiting for work in flight
// (the background teardown joins it).
func (s *Server) haltDispatchProducers() {
	s.mu.RLock()
	scheduler := s.scheduler
	s.mu.RUnlock()
	if scheduler != nil {
		scheduler.halt()
	}
	s.haltScheduledSendSweeper()
	s.haltAutoSuspendWorker()
}

// ShuttingDown reports whether BeginShutdown has run.
func (s *Server) ShuttingDown() bool {
	return s.shutdownMark.shuttingDown()
}

// BeginShutdown is the web server's first shutdown step: its streaming
// responses end and new requests to it are refused with a retryable 503
// (health endpoints excepted). The mounted hub API has its own mark
// (Server.BeginShutdown). It is idempotent.
func (ws *WebServer) BeginShutdown() {
	ws.shutdownMark.begin()
}

// defaultHTTPDrainTimeout bounds a server's HTTP drain when its shutdown
// ctx has no earlier deadline (the bound before ptone/scion#5054).
const defaultHTTPDrainTimeout = 30 * time.Second

// ShutdownHooks are the process's seams into a server's graceful shutdown
// (ptone/scion#5054). The zero value keeps the defaults: Shutdown runs
// under context.Background() and nothing is waited for before the HTTP
// drain.
type ShutdownHooks struct {
	// Context returns the context Start passes to Shutdown when its ctx
	// ends. With server.hub.conduit.shutdown_grace set, cmd returns one
	// whose deadline is the process's teardown deadline. Nil:
	// context.Background().
	Context func() (context.Context, context.CancelFunc)
	// BeforeHTTPDrain runs in Shutdown before the listener closes. cmd
	// uses it to wait (bounded by ctx) for the co-located Runtime Broker
	// to stop, since the broker reports to the hub through this listener
	// while it shuts down. Nil: nothing to wait for.
	BeforeHTTPDrain func(ctx context.Context)
	// HTTPDrainContext returns the context bounding the HTTP drain, a
	// child of Shutdown's ctx (cmd: the HTTP-drain deadline of the
	// shutdown budget), so the background teardown after it keeps time.
	// The 30s bound still applies within it. Nil: Shutdown's ctx.
	HTTPDrainContext func(ctx context.Context) (context.Context, context.CancelFunc)
}

// shutdownContext returns the context Start's Shutdown runs under.
func (h ShutdownHooks) shutdownContext() (context.Context, context.CancelFunc) {
	if h.Context != nil {
		return h.Context()
	}
	return context.Background(), func() {}
}

// beforeHTTPDrain runs BeforeHTTPDrain, if set.
func (h ShutdownHooks) beforeHTTPDrain(ctx context.Context) {
	if h.BeforeHTTPDrain != nil {
		h.BeforeHTTPDrain(ctx)
	}
}

// httpDrainContext returns the context bounding the HTTP drain (see
// HTTPDrainContext).
func (h ShutdownHooks) httpDrainContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if h.HTTPDrainContext != nil {
		return h.HTTPDrainContext(ctx)
	}
	return ctx, func() {}
}

// drainHTTPServer closes srv's listener and waits for in-flight requests,
// bounded by ctx and defaultHTTPDrainTimeout; connections still open at
// the bound are closed. It returns the drain's error.
func drainHTTPServer(ctx context.Context, srv *http.Server) error {
	dctx, cancel := context.WithTimeout(ctx, defaultHTTPDrainTimeout)
	defer cancel()
	err := srv.Shutdown(dctx)
	if err != nil {
		_ = srv.Close()
	}
	return err
}

// SetShutdownHooks sets the shutdown seams (see ShutdownHooks). Call it
// before Start.
func (s *Server) SetShutdownHooks(h ShutdownHooks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdownHooks = h
}

// SetShutdownHooks sets the web server's shutdown seams (see
// ShutdownHooks). Call it before Start.
func (ws *WebServer) SetShutdownHooks(h ShutdownHooks) {
	ws.shutdownHooks = h
}
