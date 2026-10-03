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
	"io"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func rawOpen(id uint32) *conduitv1.Frame {
	return &conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{StreamId: id, Kind: conduitv1.StreamKind_STREAM_KIND_TCP, InitialWindow: DefaultStreamWindow}}}
}

// TestGoAwayRefusesNewStreams: after GoAway the relay answers a new
// StreamOpen with 4503, and neither side can open streams.
func TestGoAwayRefusesNewStreams(t *testing.T) {
	clk := clock.NewFake(t0)
	accepted := make(chan Stream, 4)
	s, raw := acceptAgainstRaw(t, Config{Clock: clk, StreamHandler: acceptAll(accepted)}, transport.MemoryOptions{Buffer: 64})
	raw.send(rawOpen(1))
	raw.recvType("stream_accept")
	recvStream(t, accepted)

	if err := s.GoAway(GoAwayOptions{Reason: "restart"}); err != nil {
		t.Fatal(err)
	}
	g := raw.recvType("go_away").GetGoAway()
	if g.GetCode() != CloseRelayRestart || g.GetLastStreamId() != 1 || g.GetDrainDeadlineMs() != uint32(DefaultDrainDeadline/time.Millisecond) {
		t.Fatalf("GoAway = %v", g)
	}
	raw.send(rawOpen(3))
	c := raw.recvType("stream_close").GetStreamClose()
	if c.GetStreamId() != 3 || c.GetCode() != CloseRelayRestart {
		t.Fatalf("new stream answered with %v, want StreamClose{3, 4503}", c)
	}
	if _, err := s.OpenStream(context.Background(), tcpOpen()); !errors.Is(err, ErrDraining) {
		t.Fatalf("relay OpenStream err = %v, want ErrDraining", err)
	}
	if !s.Info().Draining {
		t.Fatal("Info().Draining = false")
	}
}

func TestGoAwayReceivedByDialer(t *testing.T) {
	accepted := make(chan Stream, 4)
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(accepted)})
	st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	recvStream(t, accepted)
	if err := p.relay.GoAway(GoAwayOptions{ReconnectAfter: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.dialer.GoAwayReceived():
	case <-time.After(waitTimeout):
		t.Fatal("GoAwayReceived never closed")
	}
	if _, err := p.dialer.OpenStream(context.Background(), tcpOpen()); !errors.Is(err, ErrDraining) {
		t.Fatalf("dialer OpenStream err = %v, want ErrDraining", err)
	}
	if !p.dialer.Info().Draining {
		t.Fatal("dialer Info().Draining = false")
	}
	if got := reconnectAfter(p.dialer); got != 2*time.Second {
		t.Fatalf("reconnectAfter = %v", got)
	}
}

// TestGoAwayRefusesUnprocessedOpens: our opening streams above the peer's
// last_stream_id fail at once with 4503.
func TestGoAwayRefusesUnprocessedOpens(t *testing.T) {
	s, raw := dialAgainstRaw(t, Config{}, transport.MemoryOptions{Buffer: 64})
	errc := make(chan error, 1)
	go func() {
		_, err := s.OpenStream(context.Background(), tcpOpen())
		errc <- err
	}()
	raw.recvType("stream_open")
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_GoAway{GoAway: &conduitv1.GoAway{Code: CloseRelayRestart, DrainDeadlineMs: 30000}}})
	if err := <-errc; CodeOf(err, 0) != CloseRelayRestart {
		t.Fatalf("OpenStream err = %v, want 4503", err)
	}
}

// TestGoAwayDrainsToDeadlineThen4503: accepted streams keep moving data
// in both directions while draining; at the deadline they are closed with
// 4503 and the session ends with 4503.
func TestGoAwayDrainsToDeadlineThen4503(t *testing.T) {
	accepted := make(chan Stream, 4)
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(accepted)})
	opener, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	target := recvStream(t, accepted)
	if err := p.relay.GoAway(GoAwayOptions{}); err != nil {
		t.Fatal(err)
	}
	<-p.dialer.GoAwayReceived()
	eventually(t, "both streams draining", func() bool {
		return opener.(*stream).State() == StateDraining && target.(*stream).State() == StateDraining
	})
	echo := func(from, to Stream, msg string) {
		t.Helper()
		if _, err := from.Write([]byte(msg)); err != nil {
			t.Fatalf("write while draining: %v", err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(to, buf); err != nil || string(buf) != msg {
			t.Fatalf("read while draining: %q %v", buf, err)
		}
	}
	echo(opener, target, "up")
	settle(t, p.clk, 6) // ping, watchdog, drain timer per side
	p.clk.Advance(DefaultDrainDeadline - time.Millisecond)
	echo(target, opener, "down")
	settle(t, p.clk, 6)
	if p.relay.isDone() {
		t.Fatalf("relay ended before deadline: %v", p.relay.Err())
	}

	p.clk.Advance(time.Millisecond)
	if err := waitDone(t, p.relay); CodeOf(err, 0) != CloseRelayRestart {
		t.Fatalf("relay err = %v, want 4503", err)
	}
	if err := waitDone(t, p.dialer); CodeOf(err, 0) != CloseRelayRestart {
		t.Fatalf("dialer err = %v, want 4503", err)
	}
	if _, err := opener.Read(make([]byte, 1)); CodeOf(err, 0) != CloseRelayRestart {
		t.Fatalf("opener read err = %v, want 4503", err)
	}
}

// TestGoAwayClosesWhenDrained: once the last stream finishes the session
// closes without waiting for the deadline; with no streams it closes at once.
func TestGoAwayClosesWhenDrained(t *testing.T) {
	t.Run("streams finish early", func(t *testing.T) {
		accepted := make(chan Stream, 4)
		p := newPair(t, Config{}, Config{StreamHandler: acceptAll(accepted)})
		opener, err := p.dialer.OpenStream(context.Background(), tcpOpen())
		if err != nil {
			t.Fatal(err)
		}
		target := recvStream(t, accepted)
		if err := p.relay.GoAway(GoAwayOptions{}); err != nil {
			t.Fatal(err)
		}
		_ = opener.Close()
		if _, err := target.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("target read = %v, want EOF", err)
		}
		_ = target.Close()
		if err := waitDone(t, p.relay); CodeOf(err, 0) != CloseRelayRestart {
			t.Fatalf("relay err = %v, want 4503", err)
		}
	})
	t.Run("no streams", func(t *testing.T) {
		p := newPair(t, Config{}, Config{})
		if err := p.relay.GoAway(GoAwayOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := waitDone(t, p.dialer); CodeOf(err, 0) != CloseRelayRestart {
			t.Fatalf("dialer err = %v, want 4503", err)
		}
	})
}
