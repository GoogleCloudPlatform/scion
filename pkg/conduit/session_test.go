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
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func TestHandshakeInfo(t *testing.T) {
	p := newPair(t, Config{}, Config{})
	for name, s := range map[string]*session{"dialer": p.dialer, "relay": p.relay} {
		info := s.Info()
		if info.SessionID != "sess-1" || info.RelayInstanceID != "relay-a" || info.ConnectionEpoch != 7 {
			t.Errorf("%s: welcome fields not reflected: %+v", name, info)
		}
		if info.PrincipalKind != "agent" || info.PrincipalID != "agent-1" {
			t.Errorf("%s: principal = %q/%q", name, info.PrincipalKind, info.PrincipalID)
		}
		if info.EndpointIncarnation != "inc-1" || info.ExecScope != "scope-1" {
			t.Errorf("%s: capabilities not reflected: %+v", name, info)
		}
		if info.Transport != transport.Memory || !info.ConnectedAt.Equal(t0) || info.Draining {
			t.Errorf("%s: info = %+v", name, info)
		}
	}
}

func TestHandshakeWelcomeDefaults(t *testing.T) {
	l := transport.NewMemoryListener(transport.MemoryOptions{Buffer: 8})
	clk := clock.NewFake(t0)
	go func() {
		c, _ := l.Accept(context.Background())
		_, _ = Accept(context.Background(), c, Config{Clock: clk, PingInterval: 20 * time.Second}, &testAdmitter{})
	}()
	s, w, err := Dial(context.Background(), l, Config{Clock: clk}, testHello())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if w.GetPingIntervalMs() != 20000 || w.GetMaxFrame() != MaxDataFrame {
		t.Fatalf("welcome defaults: %v", w)
	}
	if got := s.(*session).cfg.PingInterval; got != 20*time.Second {
		t.Fatalf("dialer ping interval = %v, want relay's 20s", got)
	}
}

func TestHandshakeRejected(t *testing.T) {
	tests := []struct {
		name     string
		admitErr error
		hello    *conduitv1.Hello
		wantCode uint32
	}{
		{"unauthenticated", Reject(CloseUnauthenticated, "bad token"), testHello(), CloseUnauthenticated},
		{"forbidden", Reject(CloseForbidden, "nope"), testHello(), CloseForbidden},
		{"generic error is forbidden", errors.New("db down"), testHello(), CloseForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := transport.NewMemoryListener(transport.MemoryOptions{Buffer: 8})
			go func() {
				c, _ := l.Accept(context.Background())
				_, _ = Accept(context.Background(), c, Config{}, &testAdmitter{admitErr: tc.admitErr})
			}()
			_, _, err := Dial(context.Background(), l, Config{}, tc.hello)
			if code, ok := closeCode(err); !ok || code != tc.wantCode {
				t.Fatalf("Dial err = %v, want code %d", err, tc.wantCode)
			}
		})
	}
}

func TestHandshakeRejectsUnspecifiedPrincipal(t *testing.T) {
	if _, _, err := Dial(context.Background(), transport.NewMemoryListener(transport.MemoryOptions{}), Config{}, &conduitv1.Hello{}); err == nil {
		t.Fatal("Dial with unspecified principal kind succeeded")
	}
	// A raw dialer that sends one anyway is refused with 4400.
	a, b := transport.Pipe(transport.MemoryOptions{Buffer: 8})
	raw := &rawPeer{t: t, conn: b}
	errc := make(chan error, 1)
	go func() {
		_, err := Accept(context.Background(), a, Config{}, &testAdmitter{})
		errc <- err
	}()
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: &conduitv1.Hello{PrincipalId: "x"}}})
	if g := raw.recv().GetGoAway(); g.GetCode() != CloseProtocolError {
		t.Fatalf("want GoAway 4400, got %v", g)
	}
	if code, _ := closeCode(<-errc); code != CloseProtocolError {
		t.Fatal("Accept did not fail with 4400")
	}
}

func TestHandshakeTimeout(t *testing.T) {
	clk := clock.NewFake(t0)
	a, _ := transport.Pipe(transport.MemoryOptions{Buffer: 8})
	errc := make(chan error, 1)
	go func() {
		_, err := Accept(context.Background(), a, Config{Clock: clk}, &testAdmitter{})
		errc <- err
	}()
	if !clk.WaitFor(waitTimeout, func(n int) bool { return n == 1 }) {
		t.Fatal("handshake timer not armed")
	}
	clk.Advance(DefaultHandshakeTimeout)
	if err := <-errc; err == nil {
		t.Fatal("Accept succeeded without Hello")
	}
}

func TestStreamIDParity(t *testing.T) {
	dIn, rIn := make(chan Stream, 4), make(chan Stream, 4)
	p := newPair(t, Config{StreamHandler: acceptAll(dIn)}, Config{StreamHandler: acceptAll(rIn)})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		ds, err := p.dialer.OpenStream(ctx, tcpOpen())
		if err != nil {
			t.Fatal(err)
		}
		rs, err := p.relay.OpenStream(ctx, tcpOpen())
		if err != nil {
			t.Fatal(err)
		}
		if ds.ID()%2 != 1 || rs.ID()%2 != 0 {
			t.Fatalf("dialer id %d must be odd, relay id %d even", ds.ID(), rs.ID())
		}
		if got := recvStream(t, rIn).ID(); got != ds.ID() {
			t.Fatalf("relay saw %d, want %d", got, ds.ID())
		}
		if got := recvStream(t, dIn).ID(); got != rs.ID() {
			t.Fatalf("dialer saw %d, want %d", got, rs.ID())
		}
	}
}

func TestStreamRoundTripAndStates(t *testing.T) {
	in := make(chan Stream, 1)
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(in)})
	st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	if got := st.(*stream).State(); got != StateActive {
		t.Fatalf("opener state %v, want active", got)
	}
	rs := recvStream(t, in)
	if got := rs.(*stream).State(); got != StateActive {
		t.Fatalf("target state %v, want active", got)
	}

	msg := bytes.Repeat([]byte("abcdefgh"), 100_000) // 800 KB, several frames and window updates
	go func() {
		_, _ = st.Write(msg)
		_ = st.(*stream).CloseWrite()
	}()
	got, err := io.ReadAll(rs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("payload mismatch: %d bytes", len(got))
	}
	// Echo back the other way and close normally.
	go func() {
		_, _ = rs.Write([]byte("bye"))
		_ = rs.Close()
	}()
	back, err := io.ReadAll(st)
	if err != nil || string(back) != "bye" {
		t.Fatalf("read back %q, %v", back, err)
	}
	eventually(t, "streams removed", func() bool {
		return p.dialer.Stats().OpenStreams == 0 && p.relay.Stats().OpenStreams == 0
	})
	if got := rs.(*stream).State(); got != StateClosed {
		t.Fatalf("target state after close %v, want closed", got)
	}
	if _, err := rs.Write([]byte("x")); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestStreamCloseWithCodeReachesPeer(t *testing.T) {
	in := make(chan Stream, 1)
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(in)})
	st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	rs := recvStream(t, in)
	if _, err := st.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWithCode(CloseForbidden, "authz expired"); err != nil {
		t.Fatal(err)
	}
	// Data written before the close is delivered first (draining), then the
	// code.
	buf := make([]byte, 16)
	n, err := io.ReadFull(rs, buf[:4])
	if err != nil || string(buf[:n]) != "tail" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	_, err = rs.Read(buf)
	if code, ok := closeCode(err); !ok || code != CloseForbidden {
		t.Fatalf("read err = %v, want code 4403", err)
	}
}

func TestStreamRejectedByHandler(t *testing.T) {
	h := StreamHandlerFunc(func(ctx context.Context, o *conduitv1.StreamOpen, ps PendingStream) error {
		return Reject(CloseForbidden, "port not exposed")
	})
	p := newPair(t, Config{}, Config{StreamHandler: h})
	_, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if code, ok := closeCode(err); !ok || code != CloseForbidden {
		t.Fatalf("OpenStream err = %v, want 4403", err)
	}
}

func TestStreamKindValidation(t *testing.T) {
	p := newPair(t, Config{}, Config{})
	if _, err := p.dialer.OpenStream(context.Background(), &conduitv1.StreamOpen{}); err == nil {
		t.Fatal("unspecified kind accepted locally")
	}
	// Relay without a StreamHandler refuses with 4400.
	_, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if code, _ := closeCode(err); code != CloseProtocolError {
		t.Fatalf("err = %v, want 4400", err)
	}
	// A raw peer sending an unspecified kind gets 4400.
	s, raw := acceptAgainstRaw(t, Config{StreamHandler: acceptAll(make(chan Stream, 1))}, transport.MemoryOptions{Buffer: 64})
	_ = s
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{StreamId: 1}}})
	if c := raw.recvType("stream_close").GetStreamClose(); c.GetCode() != CloseProtocolError {
		t.Fatalf("got %v", c)
	}
}

func TestStreamKindMapping(t *testing.T) {
	for _, k := range []StreamKind{StreamTCP, StreamPTY, StreamSSH, StreamLogs, StreamEvents} {
		back, err := StreamKindFromProto(k.Proto())
		if err != nil || back != k {
			t.Errorf("%s: round trip %q, %v", k, back, err)
		}
	}
	for _, k := range []PrincipalKind{PrincipalBroker, PrincipalAgent, PrincipalUser, PrincipalRelayPeer} {
		back, err := PrincipalKindFromProto(k.Proto())
		if err != nil || back != k {
			t.Errorf("%s: round trip %q, %v", k, back, err)
		}
	}
	if _, err := StreamKindFromProto(conduitv1.StreamKind_STREAM_KIND_UNSPECIFIED); err == nil {
		t.Error("unspecified stream kind accepted")
	}
	if _, err := PrincipalKindFromProto(conduitv1.PrincipalKind_PRINCIPAL_KIND_UNSPECIFIED); err == nil {
		t.Error("unspecified principal kind accepted")
	}
	if StreamKind("bogus").Proto() != conduitv1.StreamKind_STREAM_KIND_UNSPECIFIED {
		t.Error("unknown kind must map to UNSPECIFIED")
	}
}

func TestInvalidPeerStreamIDIsProtocolError(t *testing.T) {
	tests := []struct {
		name string
		ids  []uint32
	}{
		{"wrong parity", []uint32{2}},
		{"zero", []uint32{0}},
		{"not increasing (duplicate open)", []uint32{3, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, raw := acceptAgainstRaw(t, Config{StreamHandler: StreamHandlerFunc(func(context.Context, *conduitv1.StreamOpen, PendingStream) error { return nil })}, transport.MemoryOptions{Buffer: 64})
			for _, id := range tc.ids {
				raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{StreamId: id, Kind: conduitv1.StreamKind_STREAM_KIND_TCP}}})
			}
			if code, _ := closeCode(waitDone(t, s)); code != CloseProtocolError {
				t.Fatalf("session err = %v, want 4400", s.Err())
			}
		})
	}
}

func TestMaxDataFrame(t *testing.T) {
	in := make(chan Stream, 1)
	var maxSeen int
	icpt := func(d Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if d == Outbound {
			if n := len(f.GetStreamData().GetData()); n > maxSeen {
				maxSeen = n
			}
		}
		return []*conduitv1.Frame{f}
	}
	p := newPair(t, Config{Interceptor: icpt}, Config{StreamHandler: acceptAll(in)})
	st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	rs := recvStream(t, in)
	go func() { _, _ = st.Write(make([]byte, 200*1024)); _ = st.Close() }()
	if _, err := io.ReadAll(rs); err != nil {
		t.Fatal(err)
	}
	if maxSeen != MaxDataFrame {
		t.Fatalf("largest data frame %d, want %d", maxSeen, MaxDataFrame)
	}

	// An oversized inbound frame is a protocol error.
	s, raw := acceptAgainstRaw(t, Config{}, transport.MemoryOptions{Buffer: 64})
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: 1, Data: make([]byte, MaxDataFrame+1)}}})
	if code, _ := closeCode(waitDone(t, s)); code != CloseProtocolError {
		t.Fatalf("session err = %v, want 4400", s.Err())
	}
	if g := raw.recvType("go_away").GetGoAway(); g.GetCode() != CloseProtocolError {
		t.Fatalf("peer told %v", g)
	}
}

func TestSessionCloseFailsStreamsAndCalls(t *testing.T) {
	in := make(chan Stream, 1)
	block := make(chan struct{})
	p := newPair(t, Config{}, Config{
		StreamHandler: acceptAll(in),
		RPCHandler: RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			<-ctx.Done()
			close(block)
			return nil
		}),
	})
	st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	rs := recvStream(t, in)
	callErr := make(chan error, 1)
	go func() {
		_, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Method: "GET", Path: "/x"})
		callErr <- err
	}()
	eventually(t, "rpc in flight", func() bool {
		p.relay.mu.Lock()
		defer p.relay.mu.Unlock()
		return len(p.relay.inboundRPC) == 1
	})
	_ = p.relay.Close()
	if err := <-callErr; !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Call err = %v", err)
	}
	<-block // handler ctx cancelled by session loss
	if _, err := rs.Read(make([]byte, 1)); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("relay stream read err = %v", err)
	}
	waitDone(t, p.dialer)
	if _, err := st.Write([]byte("x")); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("dialer stream write err = %v", err)
	}
}
