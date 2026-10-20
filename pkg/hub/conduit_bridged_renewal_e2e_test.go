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
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// bridgedRenewalE2E is the separate relay world under enforce with a 2m
// user stream interval: hub B holds the leaf, and its stream reaches the
// agent's session on relay-0 through one bridge. f.clock (grants, the
// agent's time), the agent's timers and hub B's re-check move together
// (advance).
type bridgedRenewalE2E struct {
	w       *separateRelayWorld
	spawned chan *fakePTY

	mu sync.Mutex
	// refreshed are the stream ids of the AuthRefresh frames the agent's
	// session received (relay-0 forwarding a renewal).
	refreshed []uint32
	// sends are the results of hub B's renewal sends on the leaf's bridge.
	sends []error
}

func newBridgedRenewalE2E(t *testing.T) *bridgedRenewalE2E {
	t.Helper()
	e := &bridgedRenewalE2E{spawned: make(chan *fakePTY, 4)}
	e.w = newSeparateRelayWorldWith(t, separateRelayOptions{
		enforce: true,
		srvMod:  func(s *Server) { s.config.ConduitUserStreamAuthzMax = e2eInterval },
	}, func(w *separateRelayWorld) {
		w.spawnPTY = func(_ context.Context, req sconduit.PTYRequest) (sconduit.PTYProcess, error) {
			p := newFakePTY(req)
			e.spawned <- p
			return p, nil
		}
		// Keepalive and write timers far out, so advancing the agent's clock
		// fires only the timers under test (as the in-process PTY fixture
		// does): at the defaults (ping 30s, pong wait 60s) an advance past
		// 80% of the interval would end the agent's session and redial it.
		w.agentSession.PingInterval = time.Hour
		w.agentSession.PongWait = 24 * time.Hour
		w.agentSession.WriteWait = time.Hour
		// Record every AuthRefresh that reaches the agent's session, in
		// front of the world's own interceptor.
		next := w.agentSession.Interceptor
		w.agentSession.Interceptor = func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
			if ar := f.GetAuthRefresh(); dir == conduit.Inbound && ar != nil {
				e.mu.Lock()
				e.refreshed = append(e.refreshed, ar.GetStreamId())
				e.mu.Unlock()
			}
			if next == nil {
				return []*conduitv1.Frame{f}
			}
			return next(dir, f)
		}
	})
	require.NotNil(t, e.w.hubB.conduitExternal.Load(), "hub B is in external relay mode")
	require.True(t, e.w.clock.Now().Equal(e.w.authzClocks["hub-b"].Now()), "the clocks start together")
	return e
}

// recordSends wraps st's renewal sender (under st.mu, where the driver
// reads it) so each send's result is recorded.
func (e *bridgedRenewalE2E) recordSends(t *testing.T, st *conduitUserStream) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	send := st.Renew
	require.NotNil(t, send, "the leaf has a renewal sender (its bridge)")
	st.Renew = func(g []byte) error {
		err := send(g)
		e.mu.Lock()
		e.sends = append(e.sends, err)
		e.mu.Unlock()
		return err
	}
}

// waitFor polls cond for up to 10s.
func (e *bridgedRenewalE2E) waitFor(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		ok := cond()
		e.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// advance moves f.clock first, then the agent's timers, then both hubs'
// re-checks, so a renewal hub B sends reaches an agent whose clock already
// reads the same time.
func (e *bridgedRenewalE2E) advance(d time.Duration) {
	e.w.clock.Advance(d)
	e.w.agentClock.Advance(d)
	e.w.authzClocks["hub-a"].Advance(d)
	e.w.authzClocks["hub-b"].Advance(d)
}

// soleBridgedLeaf returns hub B's one tracked leaf, which must be enforced,
// carry a deadline and keep a bridge to relay-0.
func (e *bridgedRenewalE2E) soleBridgedLeaf(t *testing.T) *conduitUserStream {
	t.Helper()
	leaves := trackedLeaves(t, e.w.hubB)
	require.Len(t, leaves, 1)
	st := leaves[0]
	require.True(t, st.isEnforced())
	require.False(t, st.AuthzUntil.IsZero(), "the open grant carried authz_until")
	require.NotNil(t, st.Bridge, "the leaf reaches the target through a bridge")
	assert.Equal(t, "relay-0", st.Bridge.Owner().InstanceID)
	return st
}

// openPTY opens an agent PTY through hub B and returns its upstream.
func (e *bridgedRenewalE2E) openPTY(t *testing.T) *agentPTYUpstream {
	t.Helper()
	u := &agentPTYUpstream{s: e.w.hubB, identity: e.w.owner, agent: e.w.launched}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, u.Open(ctx, 80, 24), "the open is not refused with 503 stream_renewal_unavailable")
	t.Cleanup(u.Close)
	waitSpawn(t, e.spawned)
	_, isBridge := u.st.(relay.Bridge)
	require.True(t, isBridge, "the PTY leaf's stream is a bridge")
	ptyEcho(t, u, "opened")
	return u
}

// ptyEcho writes s on u and reads until the fake PTY echoes it back.
func ptyEcho(t *testing.T, u *agentPTYUpstream, s string) {
	t.Helper()
	require.NoError(t, u.Write([]byte(s)))
	var got strings.Builder
	for !strings.Contains(got.String(), s) {
		b, err := u.Read(context.Background())
		require.NoError(t, err)
		got.Write(b)
	}
}

// openPort opens a port proxy stream through hub B to the agent's app and
// returns it.
func (e *bridgedRenewalE2E) openPort(t *testing.T) conduit.Stream {
	t.Helper()
	conn, err := e.w.hubB.openConduitPort(context.Background(), e.w.owner, e.w.launched, e.w.app.port)
	require.NoError(t, err, "the open is not refused with 503 stream_renewal_unavailable")
	t.Cleanup(func() { _ = conn.Close() })
	st, ok := conn.(conduit.Stream)
	require.True(t, ok, "the proxy connection is a conduit stream")
	return st
}

// portHello sends one HTTP/1.1 request on st and requires the app's
// answer, so the stream is still open end to end.
func portHello(t *testing.T, st conduit.Stream, path string) {
	t.Helper()
	_, err := io.WriteString(st, "GET "+path+" HTTP/1.1\r\nHost: app\r\n\r\n")
	require.NoError(t, err)
	var got strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(got.String(), "hello "+path) {
		n, err := st.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			require.Contains(t, got.String(), "hello "+path, "read error %v", err)
			return
		}
	}
}

// TestBridgedRenewalE2E_RenewalMovesTargetDeadline (criterion 29, the
// hub-held leaf in external relay mode, design §3.9 Hub-held leaves): under
// enforce, a PTY and a port proxy stream opened through hub B to an agent
// whose session relay-0 holds are opened (no 503), renewed by hub B's
// driver at about 80% of the interval, and the renewal travels on the
// leaf's bridge, through relay-0, to the agent: the agent's deadline timer
// moves to the renewal's authz_until + 30s and the stream outlives its
// first authz_until + 30s.
func TestBridgedRenewalE2E_RenewalMovesTargetDeadline(t *testing.T) {
	for _, kind := range []string{"pty", "port proxy"} {
		t.Run(kind, func(t *testing.T) {
			e := newBridgedRenewalE2E(t)
			var alive func()
			if kind == "pty" {
				u := e.openPTY(t)
				alive = func() { ptyEcho(t, u, "still open") }
			} else {
				st := e.openPort(t)
				portHello(t, st, "/opened")
				alive = func() { portHello(t, st, "/still-open") }
			}
			st := e.soleBridgedLeaf(t)
			first := st.AuthzUntil
			require.True(t, e.w.agentClock.WaitForTimer(10*time.Second, first.Add(30*time.Second)),
				"S1: the agent enforces the open grant's deadline (a timer at authz_until + 30s)")
			e.recordSends(t, st)
			before := e.soleAgentSession(t)
			require.Equal(t, st.SessionID, before.SessionID, "the leaf's target is the agent's session")

			// Past 80% plus the largest jitter, before the first deadline.
			e.advance(e2eInterval*4/5 + e2eInterval/20 + time.Second)
			renewedUntil := e.w.clock.Now().Truncate(time.Second).Add(e2eInterval)
			e.requireSameSession(t, before, "S1b: across the advance")
			require.True(t, e.waitFor(func() bool { return len(e.sends) > 0 }),
				"S2: hub B minted a renewal and called the leaf's sender")
			e.mu.Lock()
			firstSend := e.sends[0]
			e.mu.Unlock()
			require.NoError(t, firstSend, "S2: hub B's renewal send on the bridge to relay-0")
			require.Truef(t, e.waitFor(func() bool { return slices.Contains(e.refreshed, st.StreamID) }),
				"S3: relay-0 forwarded the renewal to the agent's stream (AuthRefresh for stream %d)", st.StreamID)
			require.True(t, e.w.agentClock.WaitForTimer(10*time.Second, renewedUntil.Add(30*time.Second)),
				"S4: the agent accepted the renewal and moved its deadline to its authz_until + 30s")
			assert.True(t, st.Deadline().Equal(renewedUntil), "hub B's deadline moved with it")
			e.requireSameSession(t, before, "after S4: the renewal reached the original session")

			// Past the first deadline plus the target's grace.
			e.advance(first.Add(30*time.Second).Sub(e.w.clock.Now()) + 10*time.Second)
			alive()
		})
	}
}

// TestBridgedRenewalE2E_NoSenderClosesAtDeadline: the control case. With
// hub B's sender for the leaf removed, the driver mints but nothing reaches
// the agent, and the PTY ends with 4401 authz_expired at its deadline.
func TestBridgedRenewalE2E_NoSenderClosesAtDeadline(t *testing.T) {
	e := newBridgedRenewalE2E(t)
	u := e.openPTY(t)
	st := e.soleBridgedLeaf(t)
	// The driver mints, but nothing is delivered. The driver reads Renew
	// under st.mu.
	st.mu.Lock()
	st.Renew = nil
	st.mu.Unlock()
	e.advance(st.AuthzUntil.Sub(e.w.clock.Now()) + time.Second)

	var err error
	for err == nil {
		_, err = u.Read(context.Background())
	}
	var sc *StreamClosedError
	require.True(t, errors.As(err, &sc), "the stream ended with a close: %v", err)
	assert.Equal(t, 4401, sc.Code)
	assert.Equal(t, conduitReasonAuthzExpired, sc.Reason)
}

// soleAgentSession returns the agent's one session row.
func (e *bridgedRenewalE2E) soleAgentSession(t *testing.T) registry.SessionRecord {
	t.Helper()
	rows := e.w.agentSessions(t)
	require.Len(t, rows, 1, "the agent has one session")
	return rows[0]
}

// sessionIDsAndEpochs lists rows as session@epoch.
func sessionIDsAndEpochs(rows []registry.SessionRecord) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s@%d", r.SessionID, r.ConnectionEpoch))
	}
	return out
}

// requireSameSession requires that the agent still has exactly one session,
// with before's session id and connection epoch: a reconnect is named as
// such (when, says where in the test the check runs).
func (e *bridgedRenewalE2E) requireSameSession(t *testing.T, before registry.SessionRecord, when string) {
	t.Helper()
	after := e.w.agentSessions(t)
	require.Truef(t, len(after) == 1 && after[0].SessionID == before.SessionID && after[0].ConnectionEpoch == before.ConnectionEpoch,
		"%s: the agent kept its session (session %s epoch %d before; rows now: %v)",
		when, before.SessionID, before.ConnectionEpoch, sessionIDsAndEpochs(after))
}
