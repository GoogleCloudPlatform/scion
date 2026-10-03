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

package conduit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// waitTimeout bounds every real-time wait in tests. Waits are on
// conditions, never fixed sleeps.
const waitTimeout = 10 * time.Second

type testAdmitter struct {
	mu        sync.Mutex
	admitErr  error
	refreshFn func(*conduitv1.AuthRefresh) error
	refreshes []*conduitv1.AuthRefresh
	welcome   *conduitv1.Welcome
}

func (a *testAdmitter) Admit(_ context.Context, h *conduitv1.Hello) (*conduitv1.Welcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admitErr != nil {
		return nil, a.admitErr
	}
	if a.welcome != nil {
		return a.welcome, nil
	}
	return &conduitv1.Welcome{SessionId: "sess-1", RelayInstanceId: "relay-a", ConnectionEpoch: 7}, nil
}

func (a *testAdmitter) Refresh(_ context.Context, ar *conduitv1.AuthRefresh) error {
	a.mu.Lock()
	a.refreshes = append(a.refreshes, ar)
	fn := a.refreshFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ar)
	}
	return nil
}

func testHello() *conduitv1.Hello {
	return &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		PrincipalId:   "agent-1",
		ClientVersion: "test",
		Capabilities: &conduitv1.Capabilities{
			StreamKinds:         []string{"tcp", "pty"},
			EndpointIncarnation: "inc-1",
			ExecScope:           "scope-1",
		},
	}
}

// pair is a connected dialer/relay session pair on a fake clock.
type pair struct {
	clk    *clock.Fake
	dialer *session
	relay  *session
	adm    *testAdmitter
}

func newPair(t *testing.T, dcfg, rcfg Config) *pair {
	t.Helper()
	clk := clock.NewFake(t0)
	if dcfg.Clock == nil {
		dcfg.Clock = clk
	}
	if rcfg.Clock == nil {
		rcfg.Clock = clk
	}
	l := transport.NewMemoryListener(transport.MemoryOptions{Buffer: 64})
	adm := &testAdmitter{}
	type res struct {
		s   Session
		err error
	}
	rc := make(chan res, 1)
	go func() {
		c, err := l.Accept(context.Background())
		if err != nil {
			rc <- res{err: err}
			return
		}
		s, err := Accept(context.Background(), c, rcfg, adm)
		rc <- res{s, err}
	}()
	ds, _, err := Dial(context.Background(), l, dcfg, testHello())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	r := <-rc
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	p := &pair{clk: clk, dialer: ds.(*session), relay: r.s.(*session), adm: adm}
	t.Cleanup(func() {
		_ = p.dialer.Close()
		_ = p.relay.Close()
	})
	return p
}

// rawPeer drives one end of a pipe by hand, to play a misbehaving,
// silent or precisely scripted peer.
type rawPeer struct {
	t    *testing.T
	conn transport.Conn
}

func (r *rawPeer) send(f *conduitv1.Frame) {
	r.t.Helper()
	b, err := proto.Marshal(f)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.conn.WriteFrame(b); err != nil {
		r.t.Fatalf("raw write: %v", err)
	}
}

func (r *rawPeer) recv() *conduitv1.Frame {
	r.t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := r.conn.ReadFrame()
		ch <- res{b, err}
	}()
	select {
	case x := <-ch:
		if x.err != nil {
			r.t.Fatalf("raw read: %v", x.err)
		}
		f := &conduitv1.Frame{}
		if err := proto.Unmarshal(x.b, f); err != nil {
			r.t.Fatal(err)
		}
		return f
	case <-time.After(waitTimeout):
		r.t.Fatal("raw read: timeout")
		return nil
	}
}

// recvType reads frames until one of type typ arrives, skipping pings,
// pongs and window updates.
func (r *rawPeer) recvType(typ string) *conduitv1.Frame {
	r.t.Helper()
	for {
		f := r.recv()
		if FrameType(f) == typ {
			return f
		}
		switch FrameType(f) {
		case "ping", "pong", "stream_window":
			continue
		}
		r.t.Fatalf("raw: want %s, got %s (%v)", typ, FrameType(f), f)
	}
}

// dialAgainstRaw returns a dialer session under test whose relay is a
// rawPeer that has completed the handshake.
func dialAgainstRaw(t *testing.T, cfg Config, opts transport.MemoryOptions) (*session, *rawPeer) {
	t.Helper()
	a, b := transport.Pipe(opts)
	raw := &rawPeer{t: t, conn: b}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h := raw.recv()
		if h.GetHello() == nil {
			t.Errorf("want hello, got %v", h)
			return
		}
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Welcome{Welcome: &conduitv1.Welcome{SessionId: "raw", ConnectionEpoch: 1}}})
	}()
	s, _, err := Dial(context.Background(), transport.DialerFunc(func(context.Context) (transport.Conn, error) { return a, nil }), cfg, testHello())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	<-done
	t.Cleanup(func() { _ = s.Close(); _ = b.Close() })
	return s.(*session), raw
}

// acceptAgainstRaw returns a relay session under test whose dialer is a
// rawPeer that has completed the handshake.
func acceptAgainstRaw(t *testing.T, cfg Config, opts transport.MemoryOptions) (*session, *rawPeer) {
	t.Helper()
	a, b := transport.Pipe(opts)
	raw := &rawPeer{t: t, conn: b}
	type res struct {
		s   Session
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := Accept(context.Background(), a, cfg, &testAdmitter{})
		ch <- res{s, err}
	}()
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: testHello()}})
	if w := raw.recv(); w.GetWelcome() == nil {
		t.Fatalf("want welcome, got %v", w)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	t.Cleanup(func() { _ = r.s.Close(); _ = b.Close() })
	return r.s.(*session), raw
}

// eventually polls cond until it holds or waitTimeout passes. It waits on
// a condition (stream tables emptying, goroutines finishing); it is not
// used to order events.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitDone(t *testing.T, s LocalSession) error {
	t.Helper()
	select {
	case <-s.Done():
		return s.Err()
	case <-time.After(waitTimeout):
		t.Fatal("session did not end")
		return nil
	}
}

func closeCode(err error) (uint32, bool) {
	var ce *CloseError
	if errors.As(err, &ce) {
		return ce.Code, true
	}
	return 0, false
}

// acceptAll is a StreamHandler that accepts every stream and hands it to
// the streams channel.
func acceptAll(streams chan<- Stream) StreamHandler {
	return StreamHandlerFunc(func(ctx context.Context, _ *conduitv1.StreamOpen, ps PendingStream) error {
		st, err := ps.Accept()
		if err != nil {
			return err
		}
		streams <- st
		return nil
	})
}

func tcpOpen() *conduitv1.StreamOpen {
	return &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_TCP, Params: map[string]string{"port": "8080"}}
}

func recvStream(t *testing.T, ch <-chan Stream) Stream {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(waitTimeout):
		t.Fatal("no inbound stream")
		return nil
	}
}

// settle waits until exactly want timers are armed on clk. Call it before
// Advance so a write timer of a write that has already completed (but not
// yet stopped its timer) cannot fire as a false write timeout.
func settle(t *testing.T, clk *clock.Fake, want int) {
	t.Helper()
	if !clk.WaitFor(waitTimeout, func(n int) bool { return n == want }) {
		t.Fatalf("timers did not settle at %d (have %d)", want, clk.Pending())
	}
}
