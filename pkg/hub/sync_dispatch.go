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
// It equals the hub-to-broker request limit: the control channel's
// RequestTimeout (set in New, server.go:2007; default in
// DefaultControlChannelConfig, controlchannel.go:59) and the
// broker HTTP transport's client timeout (broker_http_transport.go). When it
// fires, the dispatch ctx is done, and what the broker sees depends on the
// transport: on the control channel, BrokerConnection.TunnelRequest sends a
// cancel frame; on direct HTTP, the request is aborted; on a cross-node
// deferred dispatch, nothing is sent, and the owning node runs the durable
// intent to its end. A variable so tests can shorten it.
var syncDispatchTimeout = 120 * time.Second

// hubWorkspaceUploadTimeout bounds the create-time upload of a hub-managed
// project workspace to storage, which runs detached from the client
// (ptone/scion#1961). Generous: a large workspace can take minutes. A
// variable so tests can shorten it.
var hubWorkspaceUploadTimeout = 10 * time.Minute

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
// work, of a lifecycle stop or suspend: the workspace sync-back request to
// the broker (syncWorkspaceOnStop, bounded like a dispatch by the
// hub-to-broker request limit, syncDispatchTimeout), the ephemeral workspace
// check, then the stop dispatch (bounded by the same limit), plus
// syncDispatchWriteSlack.
func stopWriteBudget() time.Duration {
	return syncDispatchTimeout + workspaceCheckTimeout + syncDispatchTimeout + syncDispatchWriteSlack
}

// dmWakeWriteBudget is the write deadline, from the start of the wake, of a
// direct message that resumes a suspended agent before delivering
// (wakeAgentForDM): the resume dispatch (bounded by syncDispatchTimeout),
// then the wait for the resumed agent's first status (wakeReadyTimeout),
// plus syncDispatchWriteSlack for the delivery and the response write.
func dmWakeWriteBudget() time.Duration {
	return syncDispatchTimeout + wakeReadyTimeout + syncDispatchWriteSlack
}

// hubWorkspaceUploadWriteBudget is the write deadline, from the start of the
// upload, of a create that uploads its hub-managed project workspace: the
// upload's own budget plus syncDispatchWriteSlack, for the failure answer
// written when the upload runs out of time. A create that goes on to
// dispatch moves the deadline again (extendWriteDeadlineForSyncDispatch).
func hubWorkspaceUploadWriteBudget() time.Duration {
	return hubWorkspaceUploadTimeout + syncDispatchWriteSlack
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
