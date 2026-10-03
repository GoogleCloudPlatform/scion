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

package relay

import (
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Test-only access to internals for the external test package.

// TouchInterceptor returns the touch-on-pong interceptor of the live local
// session sessionID, chained with next exactly as Serve chains it.
func (r *Relay) TouchInterceptorForTest(sessionID string, next conduit.Interceptor) conduit.Interceptor {
	r.mu.Lock()
	e := r.sessions[sessionID]
	r.mu.Unlock()
	if e == nil {
		return nil
	}
	return chainInterceptor(r.touchOnPong(e), next)
}

// WaitTouchesForTest waits for in-flight touch goroutines.
func (r *Relay) WaitTouchesForTest() { r.wg.Wait() }

// WaitBridgesForTest waits for owner-side bridges to finish.
func (r *Relay) WaitBridgesForTest() { r.bridges.Wait() }

// SetAfterOpenHookForTest installs the late-accept race seam; the hook
// receives a function reporting when the caller's hop has ended.
func (r *Relay) SetAfterOpenHookForTest(h func(hopDone <-chan struct{})) {
	r.testHookAfterOpen = func(hop *wsStream) { h(hop.Done()) }
}

// SetBeforeReadyHookForTest installs the pipelined-StreamOpen race seam:
// h runs in Serve after Accept returned, before the session is ready.
func (r *Relay) SetBeforeReadyHookForTest(h func()) { r.testHookBeforeReady = h }

// HeartbeatForTest runs one heartbeat now.
func (r *Relay) HeartbeatForTest() { r.heartbeat() }

// SourceForTest returns the incarnation source of a local session.
func (r *Relay) SourceForTest(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.sessions[sessionID]; e != nil {
		return e.source
	}
	return ""
}

// NewAdmitterForTest exposes the per-connection admitter.
func (r *Relay) NewAdmitterForTest(p Principal, transport string) (conduit.Admitter, func() (registry.SessionRecord, bool)) {
	a := &admitter{r: r, p: p, transport: transport}
	return a, func() (registry.SessionRecord, bool) { rec, _, ok := a.admitted(); return rec, ok }
}

// PongFrame is an inbound Pong.
var PongFrame = &conduitv1.Frame{Body: &conduitv1.Frame_Pong{Pong: &conduitv1.Pong{}}}
