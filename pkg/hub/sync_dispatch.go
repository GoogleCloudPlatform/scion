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
	"log/slog"
	"net/http"
	"time"
)

// syncDispatchTimeout bounds one dispatcher call of a synchronous launch
// (create, provision, env finalize, workspace-bootstrap create, start, and
// each leg of a restart) once the launch no longer follows the client's
// request (ptone/scion#1961). One call is one launch attempt, which may make
// several broker requests (a hash-mismatch retry, a second-pass finalize, or
// a cross-node deferred wait); the bound covers them all together.
//
// It is the hub-to-broker request limit, brokerRequestTimeout (180s,
// ptone/scion#6829), which also sets the control channel's RequestTimeout
// (in New and in DefaultControlChannelConfig) and the broker HTTP
// transport's client timeout (broker_http_transport.go). When it
// fires, the dispatch ctx is done, and what the broker sees depends on the
// transport: on the control channel, BrokerConnection.TunnelRequest sends a
// cancel frame; on direct HTTP, the request is aborted; on a cross-node
// deferred dispatch, nothing is sent, and the owning node runs the durable
// intent to its end. A variable so tests can shorten it.
var syncDispatchTimeout = brokerRequestTimeout

// detachLaunchFromClient returns a context for the rest of a synchronous
// launch: it keeps ctx's values (identity, trace, dispatch warnings) but not
// its cancellation, so a client that disconnects or times out (the CLI hub
// client gives up after 30s) no longer cancels the broker launch, its
// post-dispatch store writes, or the rollback of a real failure
// (ptone/scion#1961). Each broker call made under it is bounded by
// syncDispatch. The asynchronous launch path does not use it: it already
// returns before the launch finishes.
func detachLaunchFromClient(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// detachLaunchKeepDeadline is detachLaunchFromClient for a launch whose
// caller set its own budget: the returned context drops ctx's cancellation
// but keeps its deadline, when it has one. A client that disconnects no
// longer cancels the launch, while a deadline the caller chose (the chat
// wake's resume budget) still bounds it. The direct-message wake uses it
// (ptone/scion#3471). The caller must call the returned cancel func.
func detachLaunchKeepDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := detachLaunchFromClient(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(detached, deadline)
	}
	return detached, func() {}
}

// detachStopFromClient returns a context for the rest of a lifecycle stop or
// suspend, and its cancel func, which the caller must call. Like
// detachLaunchFromClient, it keeps ctx's values (identity, trace, dispatch
// warnings) but not its cancellation, so a client that disconnects or gives
// up (the CLI hub client's 30s timeout) no longer cancels the stop part way
// through, leaving the container stopped with no stopped status recorded,
// or still running with its intent set to stopped (ptone/scion#4211,
// ptone/scion#2661). Unlike a launch, which bounds each broker call with
// syncDispatch and nothing else, the whole stop is bounded by timeout: the
// stop's write budget for a single stop or suspend (stopWriteBudget), or
// for a stop-all (stopAllWriteBudget), so the stop's work ends no later
// than its response's write deadline.
func detachStopFromClient(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(detachLaunchFromClient(ctx), timeout)
}

// syncDispatch runs one synchronous dispatcher call under
// syncDispatchTimeout, derived from ctx (normally a detachLaunchFromClient
// context). fn must use the ctx it is given, not the caller's.
func syncDispatch(ctx context.Context, fn func(context.Context) error) error {
	dctx, cancel := context.WithTimeout(ctx, syncDispatchTimeout)
	defer cancel()
	return fn(dctx)
}

// syncDispatchWriteSlack is the time a synchronous create response may take
// after its dispatch wait, for the post-dispatch store writes and the
// response write itself. A var so tests can shrink it.
var syncDispatchWriteSlack = 30 * time.Second

// syncDispatchWriteBudget is the write deadline, from the start of the
// dispatch, of a request that waits on one synchronous dispatch: the
// dispatch wait plus syncDispatchWriteSlack.
func syncDispatchWriteBudget() time.Duration {
	return syncDispatchTimeout + syncDispatchWriteSlack
}

// servingWriteTimeout is the WriteTimeout of the http.Server serving the
// request, read from ctx (http.ServerContextKey, which the server sets on
// every request context and context.WithoutCancel keeps). In combo mode the
// hub handler is served by the web listener, whose WriteTimeout differs from
// the hub's configured one, so the configured value is only the fallback for
// a context without a server (a handler driven without net/http).
func servingWriteTimeout(ctx context.Context, configured time.Duration) time.Duration {
	if hs, ok := ctx.Value(http.ServerContextKey).(*http.Server); ok && hs != nil {
		return hs.WriteTimeout
	}
	return configured
}

// restartWriteBudget is the write deadline, from the start of the restart's
// broker work (taken right after the restart detaches from the client), of
// a lifecycle restart: the ephemeral workspace check before the stop, then
// two synchronous dispatches (the stop leg and the start leg), each bounded
// by syncDispatchTimeout, plus syncDispatchWriteSlack.
func restartWriteBudget() time.Duration {
	return workspaceCheckTimeout + 2*syncDispatchTimeout + syncDispatchWriteSlack
}

// stopWriteBudget is the write deadline, from the start of the broker
// work, of a lifecycle stop or suspend: the ephemeral workspace check, then
// the stop dispatch (under syncDispatch), plus
// syncDispatchWriteSlack. It is also the bound of the whole stop once it is
// detached from the client (detachStopFromClient): the slack is then what
// is left for the stopped status write and the other store writes after
// the broker steps.
func stopWriteBudget() time.Duration {
	return workspaceCheckTimeout + syncDispatchTimeout + syncDispatchWriteSlack
}

// stopAllAgentOpTimeout bounds each agent's broker work in a stop-all: the
// ephemeral workspace check and the stop dispatch together. All agents share one
// deadline, this long after the stops begin. A variable so tests can
// shorten it.
var stopAllAgentOpTimeout = 60 * time.Second

// stopAllWriteBudget is the write deadline, from the start of the stops, of
// a stop-all: the agents are stopped in parallel, so one agent's broker
// work (stopAllAgentOpTimeout), plus syncDispatchWriteSlack for each
// agent's status write and the response write (ptone/scion#4212). It is
// also the bound of the whole stop-all once it is detached from the client
// (detachStopFromClient, ptone/scion#2661).
func stopAllWriteBudget() time.Duration {
	return stopAllAgentOpTimeout + syncDispatchWriteSlack
}

// dmWakeWriteBudget is the write deadline, from the start of the wake, of a
// direct message that resumes a suspended agent before delivering
// (wakeAgentForDM): the resume dispatch (bounded by syncDispatchTimeout),
// then the wait for the resumed agent's first status (wakeReadyTimeout),
// plus syncDispatchWriteSlack for the delivery and the response write.
func dmWakeWriteBudget() time.Duration {
	return syncDispatchTimeout + wakeReadyTimeout + syncDispatchWriteSlack
}

// extendWriteDeadlineForSyncDispatch moves the connection's write deadline
// to syncDispatchWriteBudget from now, so a launch that finishes within
// syncDispatchTimeout still gets its response written instead of being cut
// at the serving listener's WriteTimeout (ptone/scion#3850). See
// extendWriteDeadline.
func extendWriteDeadlineForSyncDispatch(ctx context.Context, w http.ResponseWriter, configuredWriteTimeout time.Duration) {
	extendWriteDeadline(ctx, w, configuredWriteTimeout, syncDispatchWriteBudget())
}

// extendWriteDeadlineForDMWake moves the connection's write deadline to
// dmWakeWriteBudget from now, for a direct message that wakes its target
// (ptone/scion#4178). See extendWriteDeadline.
func (s *Server) extendWriteDeadlineForDMWake(ctx context.Context, w http.ResponseWriter) {
	extendWriteDeadline(ctx, w, s.config.WriteTimeout, dmWakeWriteBudget())
}

// extendWriteDeadline moves the connection's write deadline to budget from
// now, for a request that waits on the broker (or on storage) for longer
// than the serving listener's WriteTimeout (ptone/scion#3850,
// ptone/scion#3890). The timeout compared is that of the server actually
// serving the request (see servingWriteTimeout), with
// configuredWriteTimeout as the fallback: the deadline is extended whenever
// that timeout is positive and shorter than the budget, and left alone when
// it is unbounded (0) or already at least the budget, so this never
// shortens it below the listener's own timeout. http.NewResponseController
// reaches the connection through the middleware wrappers that implement
// Unwrap; a ResponseWriter without deadline support is logged at debug and
// otherwise ignored.
func extendWriteDeadline(ctx context.Context, w http.ResponseWriter, configuredWriteTimeout, budget time.Duration) {
	serverWriteTimeout := servingWriteTimeout(ctx, configuredWriteTimeout)
	if serverWriteTimeout <= 0 || serverWriteTimeout >= budget {
		return
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(budget)); err != nil {
		slog.DebugContext(ctx, "sync dispatch: SetWriteDeadline not applied", "error", err)
	}
}
