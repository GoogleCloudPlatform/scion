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
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/require"
)

// relayWriteGate holds one write of the hub side of a connection. After
// arm, the next write blocks (entered is closed) until open is called or
// the connection is closed (closed is closed).
type relayWriteGate struct {
	mu          sync.Mutex
	armed       bool
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	closed      chan struct{}
	closeOnce   sync.Once
}

func newRelayWriteGate() *relayWriteGate {
	return &relayWriteGate{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
}

func (g *relayWriteGate) arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

func (g *relayWriteGate) open() { g.releaseOnce.Do(func() { close(g.release) }) }

func (g *relayWriteGate) isClosed() bool {
	select {
	case <-g.closed:
		return true
	default:
		return false
	}
}

func (g *relayWriteGate) hold() error {
	g.mu.Lock()
	armed := g.armed
	g.armed = false
	g.mu.Unlock()
	if !armed {
		return nil
	}
	close(g.entered)
	select {
	case <-g.release:
		return nil
	case <-g.closed:
		return net.ErrClosed
	}
}

// gatedListener hands the server connections whose writes pass g. The
// conduit handler hijacks the connection, so the relay session writes
// through it too.
type gatedListener struct {
	net.Listener
	g *relayWriteGate
}

func (l gatedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &gatedConn{Conn: c, g: l.g}, nil
}

type gatedConn struct {
	net.Conn
	g *relayWriteGate
}

func (c *gatedConn) Write(b []byte) (int, error) {
	if err := c.g.hold(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func (c *gatedConn) Close() error {
	c.g.closeOnce.Do(func() { close(c.g.closed) })
	return c.Conn.Close()
}

// startHeldRelayWrite admits an agent session on a relay fixture (mod
// adjusts its options) served through a write gate, then makes the agent
// call the hub so the relay writes the answer, and returns once that write
// is held: its write deadline is armed on the returned fake relay clock.
// call receives the result of the agent's call.
func startHeldRelayWrite(t *testing.T, mod func(*ConduitRelayOptions)) (clk *clock.Fake, g *relayWriteGate, ls conduit.LocalSession, call <-chan error) {
	t.Helper()
	f := newRelayFixture(t, func(o *ConduitRelayOptions) {
		if mod != nil {
			mod(o)
		}
		clk = o.Clock.(*clock.Fake)
	})
	require.NotNil(t, clk)
	g = newRelayWriteGate()
	srv := httptest.NewUnstartedServer(f.srv.Handler())
	srv.Listener = gatedListener{Listener: srv.Listener, g: g}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(g.open)
	f.public = srv

	ls, _, err := f.dial(t, f.agentToken(t, f.launched), f.launched.RunID)
	require.NoError(t, err)
	g.arm()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := ls.Call(ctx, &conduitv1.RpcRequest{Method: http.MethodGet, Path: "/"})
		done <- err
	}()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no relay write reached the gate")
	}
	return clk, g, ls, done
}

// TestConduitRelay_FixtureWriteWaitOutlastsAdvance (ptone/scion#5210): with
// the conduit fixture's session write deadline, advancing the fake relay
// clock past conduit.DefaultWriteWait while a relay write is in flight no
// longer fails the session: the write completes once released and the
// agent's call is answered.
func TestConduitRelay_FixtureWriteWaitOutlastsAdvance(t *testing.T) {
	clk, g, ls, call := startHeldRelayWrite(t, nil)

	// Fake timers fire synchronously inside Advance, and a write timeout
	// closes the connection from the timer, so a fired deadline shows here.
	clk.Advance(2 * conduit.DefaultWriteWait)
	require.False(t, g.isClosed(), "the held write's deadline fired: the relay closed the session")

	g.open()
	select {
	case err := <-call:
		require.NoError(t, err, "the agent's call must be answered on the live session")
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's call was not answered after the write was released")
	}
	select {
	case <-ls.Done():
		t.Fatalf("the agent session ended: %v", ls.Err())
	default:
	}
	require.False(t, g.isClosed())
}

// TestConduitRelay_DefaultSessionWriteWait: with the option unset, the relay
// session write deadline stays conduit.DefaultWriteWait. A held write
// survives an advance just short of it and fails the session at it.
func TestConduitRelay_DefaultSessionWriteWait(t *testing.T) {
	clk, g, ls, call := startHeldRelayWrite(t, func(o *ConduitRelayOptions) { o.sessionWriteWait = 0 })

	clk.Advance(conduit.DefaultWriteWait - time.Nanosecond)
	require.False(t, g.isClosed(), "the write deadline fired before conduit.DefaultWriteWait")

	clk.Advance(time.Nanosecond)
	select {
	case <-g.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the write deadline did not fire at conduit.DefaultWriteWait")
	}
	select {
	case err := <-call:
		require.Error(t, err, "the agent's call must fail with the session")
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's call did not end with the session")
	}
	select {
	case <-ls.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the agent session did not end")
	}
}
