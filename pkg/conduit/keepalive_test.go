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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// recvUntil reads raw frames until one of type typ arrives.
func (r *rawPeer) recvUntil(typ string) *conduitv1.Frame {
	r.t.Helper()
	for {
		if f := r.recv(); FrameType(f) == typ {
			return f
		}
	}
}

// TestKeepaliveDetectsSilentPeer covers both directions: a dialer facing a
// silent relay and a relay facing a silent dialer both ping at 30s and
// give up at 60s without inbound traffic.
func TestKeepaliveDetectsSilentPeer(t *testing.T) {
	for _, side := range []string{"dialer", "relay"} {
		t.Run(side, func(t *testing.T) {
			clk := clock.NewFake(t0)
			cfg := Config{Clock: clk, WriteWait: testWriteWait}
			var s *session
			var raw *rawPeer
			if side == "dialer" {
				s, raw = dialAgainstRaw(t, cfg, transport.MemoryOptions{Buffer: 64})
			} else {
				s, raw = acceptAgainstRaw(t, cfg, transport.MemoryOptions{Buffer: 64})
			}
			settle(t, clk, 2) // ping + watchdog
			clk.Advance(DefaultPingInterval)
			if p := raw.recvUntil("ping"); p.GetPing().GetNonce() == 0 {
				t.Fatalf("ping without nonce: %v", p)
			}
			settle(t, clk, 2)
			clk.Advance(DefaultPongWait - DefaultPingInterval - time.Millisecond)
			if s.isDone() {
				t.Fatalf("ended before %v: %v", DefaultPongWait, s.Err())
			}
			clk.Advance(time.Millisecond)
			if err := waitDone(t, s); !errors.Is(err, ErrKeepaliveTimeout) {
				t.Fatalf("err = %v, want ErrKeepaliveTimeout", err)
			}
		})
	}
}

// TestKeepaliveAnyInboundFrameProvesLiveness: the watchdog counts from the
// last inbound frame of any type, not only pongs.
func TestKeepaliveAnyInboundFrameProvesLiveness(t *testing.T) {
	clk := clock.NewFake(t0)
	s, raw := dialAgainstRaw(t, Config{Clock: clk, WriteWait: testWriteWait}, transport.MemoryOptions{Buffer: 64})
	settle(t, clk, 2)
	clk.Advance(50 * time.Second)
	// The peer's own ping proves liveness; wait until it was processed.
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Ping{Ping: &conduitv1.Ping{Nonce: 1}}})
	raw.recvUntil("pong")
	settle(t, clk, 2)
	clk.Advance(DefaultPongWait - 50*time.Second + time.Second) // t=61s
	if s.isDone() {
		t.Fatalf("ended at 61s despite traffic at 50s: %v", s.Err())
	}
	clk.Advance(50 * time.Second) // t=111s > 50s + 60s
	if err := waitDone(t, s); !errors.Is(err, ErrKeepaliveTimeout) {
		t.Fatalf("err = %v, want ErrKeepaliveTimeout", err)
	}
}

// TestKeepaliveHealthyPairSurvives runs ten ping rounds between two real
// sessions; neither side times out.
func TestKeepaliveHealthyPairSurvives(t *testing.T) {
	cfg := Config{WriteWait: testWriteWait}
	p := newPair(t, cfg, cfg)
	for i := 0; i < 10; i++ {
		settle(t, p.clk, 4) // ping + watchdog per side
		p.clk.Advance(DefaultPingInterval)
		now := p.clk.Now().UnixNano()
		eventually(t, "pongs received", func() bool {
			return p.dialer.lastRecv.Load() == now && p.relay.lastRecv.Load() == now
		})
	}
	if p.dialer.isDone() || p.relay.isDone() {
		t.Fatalf("healthy pair ended: %v / %v", p.dialer.Err(), p.relay.Err())
	}
}

// TestWriteTimeout: a peer that stops reading stalls the writer; the
// session fails after WriteWait instead of hanging.
func TestWriteTimeout(t *testing.T) {
	clk := clock.NewFake(t0)
	s, _ := dialAgainstRaw(t, Config{Clock: clk}, transport.MemoryOptions{Buffer: 0})
	base := clk.Pending()
	go func() { _, _ = s.Call(context.Background(), &conduitv1.RpcRequest{Method: "GET", Path: "/x"}) }()
	if !clk.WaitFor(waitTimeout, func(n int) bool { return n > base }) {
		t.Fatal("write timer never armed")
	}
	clk.Advance(DefaultWriteWait)
	if err := waitDone(t, s); !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("err = %v, want ErrWriteTimeout", err)
	}
}
