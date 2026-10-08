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
