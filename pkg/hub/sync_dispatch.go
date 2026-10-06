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

// syncDispatchTimeout bounds one synchronous launch dispatch (create, start,
// and each leg of a restart) once it no longer follows the client's request
// (ptone/scion#1961). It equals the hub-to-broker request limit: the control
// channel's RequestTimeout (server.go, controlchannel.go) and the broker HTTP
// transport's client timeout (broker_http_transport.go). When it fires, the
// dispatch ctx is done, and the control channel sends the broker a cancel
// frame (BrokerConnection.TunnelRequest), so the broker stops its launch before
// the hub's failure cleanup removes the agent row. A variable so tests can
// shorten it.
var syncDispatchTimeout = 120 * time.Second

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

// syncDispatch runs one synchronous broker dispatch under
// syncDispatchTimeout, derived from ctx (normally a detachLaunchFromClient
// context).
func syncDispatch(ctx context.Context, fn func(context.Context) error) error {
	dctx, cancel := context.WithTimeout(ctx, syncDispatchTimeout)
	defer cancel()
	return fn(dctx)
}
