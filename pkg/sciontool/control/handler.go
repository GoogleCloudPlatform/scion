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

// Package control is sciontool's hub-to-agent control surface: an
// http.Handler that serves only /v1/control/* routes. It is transport
// agnostic: the same handler can be served from a plain net/http server
// or, through RPCHandler, as the RPC handler of the agent's conduit
// session, where it is the only thing a hub-originated RpcRequest can
// reach.
//
// Every route answers POST only (other methods get 405), unknown paths
// get 404, and no request or response body carries a credential (Conduit
// design §3.7). Every route is idempotent, coalescing, or idempotent by key:
// a route that acts (deliver-message) takes an idempotency key in its JSON
// body, and a replay of a finished key answers the stored outcome with
// "replayed": true and never acts twice (§3.7 as amended by v2.22; see
// .design/conduit/v2.22-deliver-message.md). A body-carrying route takes
// application/json only and refuses unknown fields with 400; its wire
// contract lives in pkg/agentcontrol.
package control

import (
	"net/http"
	"strings"
)

// Prefix is the path prefix of every control route.
const Prefix = "/v1/control/"

// RouteRotateToken asks sciontool to run its token refresh now. It is a
// pull trigger: the handler kicks the existing refresh loop, which
// coalesces kicks, and answers 202 with no body. 202 means the request is
// queued for the refresh loop, not that a refresh ran: a kick made before
// the loop starts, or while no loop runs, stays pending until one starts.
const RouteRotateToken = "rotate-token"

// Options configure a Handler.
type Options struct {
	// KickTokenRefresh wakes the agent's token refresh loop. It must not
	// block and must not refresh the token itself; the loop coalesces
	// kicks and keeps refreshes single-flight. Required.
	KickTokenRefresh func()
	// Deliver, when set, registers deliver-message (RouteDeliverMessage)
	// and so advertises it. Set it only when DeliverEnabled holds.
	Deliver *DeliverOptions
	// Exec, when set, registers exec-start and exec-status
	// (RouteExecStart, RouteExecStatus) and so advertises them. Set it
	// only when ExecEnabled holds.
	Exec *ExecOptions
}

// Handler serves the /v1/control/* routes.
type Handler struct {
	routes map[string]http.HandlerFunc
	names  []string
	exec   *execRoute // nil unless Options.Exec is set
}

// New returns a Handler serving the routes opts supports. It panics if
// KickTokenRefresh is nil, Deliver is set without a Runner, or Exec is set
// without Env.
func New(opts Options) *Handler {
	if opts.KickTokenRefresh == nil {
		panic("control: KickTokenRefresh is required")
	}
	kick := opts.KickTokenRefresh
	h := &Handler{routes: map[string]http.HandlerFunc{}}
	h.add(RouteRotateToken, func(w http.ResponseWriter, _ *http.Request) {
		kick()
		w.WriteHeader(http.StatusAccepted)
	})
	if opts.Deliver != nil {
		h.add(RouteDeliverMessage, newDeliverRoute(*opts.Deliver).serve)
	}
	if opts.Exec != nil {
		e := newExecRoute(*opts.Exec)
		h.exec = e
		h.add(RouteExecStart, e.serveStart)
		h.add(RouteExecStatus, e.serveStatus)
	}
	return h
}

func (h *Handler) add(name string, fn http.HandlerFunc) {
	h.routes[name] = fn
	h.names = append(h.names, name)
}

// Routes returns the names of the implemented routes, in the form
// advertised as Hello.capabilities.rpc.
func (h *Handler) Routes() []string {
	return append([]string(nil), h.names...)
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, Prefix)
	fn := h.routes[name]
	if !ok || fn == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	fn(w, r)
}
