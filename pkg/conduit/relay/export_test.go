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
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Test-only access to internals for the external test package.

// localEntryForTest waits (bounded) for sessionID to be registered, as
// Local does for a pending session, and returns its entry. MustDial returns
// on the Welcome, before Serve registers the session (r3-F1), so helpers
// that look a session up right after a dial must wait. It fails the test
// if the session never becomes live.
func (r *Relay) localEntryForTest(t testing.TB, sessionID string) *entry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, ok := r.Local(ctx, sessionID); !ok {
		t.Fatalf("session %s is not live on %s", sessionID, r.cfg.InstanceID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.sessions[sessionID]
	if e == nil {
		t.Fatalf("session %s left %s before the lookup", sessionID, r.cfg.InstanceID)
	}
	return e
}

// TouchInterceptorForTest returns the touch-on-pong interceptor of the live
// local session sessionID, chained with next exactly as Serve chains it. It
// waits for a just-admitted session to be registered.
func (r *Relay) TouchInterceptorForTest(t testing.TB, sessionID string, next conduit.Interceptor) conduit.Interceptor {
	t.Helper()
	return chainInterceptor(r.touchOnPong(r.localEntryForTest(t, sessionID)), next)
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

// SetPendingWaitHookForTest installs the r2-F1 seam: h runs in Local or
// GoAway when it starts waiting for an admitted, not yet registered session.
func (r *Relay) SetPendingWaitHookForTest(h func()) { r.testHookPendingWait = h }

// HeartbeatForTest runs one heartbeat now.
func (r *Relay) HeartbeatForTest() { r.heartbeat() }

// SourceForTest returns the incarnation source of a live local session,
// waiting for a just-admitted session to be registered.
func (r *Relay) SourceForTest(t testing.TB, sessionID string) string {
	t.Helper()
	return r.localEntryForTest(t, sessionID).source
}

// NewAdmitterForTest exposes the per-connection admitter.
func (r *Relay) NewAdmitterForTest(p Principal, transport string) (conduit.Admitter, func() (registry.SessionRecord, bool)) {
	a := &admitter{r: r, p: p, transport: transport}
	return a, func() (registry.SessionRecord, bool) { rec, _, ok := a.admitted(); return rec, ok }
}

// DrainWriteConcurrency is the bound on Shutdown's in-flight session
// draining writes.
const DrainWriteConcurrency = drainWriteConcurrency

// PongFrame is an inbound Pong.
var PongFrame = &conduitv1.Frame{Body: &conduitv1.Frame_Pong{Pong: &conduitv1.Pong{}}}

// ServingForTest reports whether the relay is still serving.
func (r *Relay) ServingForTest() bool { return r.serving() }
