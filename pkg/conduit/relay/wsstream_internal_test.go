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
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"google.golang.org/protobuf/proto"
)

// TestSpliceCloseMapping (r2-F6): a splice leg that ended with a normal
// close (1000) ends the other leg with 1000, never 4504; other close codes
// pass through and non-close errors are 4504 upstream_unreachable.
func TestSpliceCloseMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantCode   uint32
		wantReason string
	}{
		{name: "normal close", err: &conduit.CloseError{Code: conduit.CloseNormal}, wantCode: conduit.CloseNormal},
		{name: "wrapped normal close", err: fmt.Errorf("write: %w", &conduit.CloseError{Code: conduit.CloseNormal, Reason: "bye"}), wantCode: conduit.CloseNormal},
		{name: "other close code", err: &conduit.CloseError{Code: conduit.CloseForbidden, Reason: "forbidden: x"}, wantCode: conduit.CloseForbidden, wantReason: "forbidden: x"},
		{name: "transport error", err: errors.New("broken pipe"), wantCode: conduit.CloseRelayTimeout, wantReason: reason(ReasonUpstreamUnreachable, "peer leg closed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, why := spliceClose(tc.err, "peer leg closed")
			if code != tc.wantCode || why != tc.wantReason {
				t.Fatalf("spliceClose = %d %q, want %d %q", code, why, tc.wantCode, tc.wantReason)
			}
		})
	}
}

// failingWriteConn delivers queued frames to the reader and fails every
// write.
type failingWriteConn struct {
	frames chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFailingWriteConn() *failingWriteConn {
	return &failingWriteConn{frames: make(chan []byte, 4), closed: make(chan struct{})}
}

func (c *failingWriteConn) ReadFrame() ([]byte, error) {
	select {
	case b := <-c.frames:
		return b, nil
	case <-c.closed:
		return nil, errors.New("closed")
	}
}

func (c *failingWriteConn) WriteFrame([]byte) error { return errors.New("write failed") }

func (c *failingWriteConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *failingWriteConn) Transport() string { return "test" }

// TestWSStreamWindowUpdateSendFailure: when returning credit to the peer
// fails to send, the stream ends as a lost link, as a failed data write
// does. The bytes already read are still returned.
func TestWSStreamWindowUpdateSendFailure(t *testing.T) {
	conn := newFailingWriteConn()
	s := newWSStream(conn, 0, 8, clock.Real(), conduit.DefaultHandshakeTimeout)
	b, err := proto.Marshal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: hopStreamID, Data: []byte("12345678")}}})
	if err != nil {
		t.Fatal(err)
	}
	conn.frames <- b

	p := make([]byte, 8)
	n, err := s.Read(p)
	if err != nil || string(p[:n]) != "12345678" {
		t.Fatalf("Read = %d %q, %v; want the 8 bytes", n, p[:n], err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after the window update failed to send")
	}
	if ended, endErr := s.endedErr(); !ended || !errors.Is(endErr, errLinkLost) {
		t.Fatalf("endedErr = %v, %v; want ended with errLinkLost", ended, endErr)
	}
	if _, err := s.Read(p); err == nil {
		t.Fatal("Read after the stream ended: want an error")
	}
}

// peerConn is one end of a hop whose peer the test plays: frames queued
// on in are read by the stream, writes are recorded, and closing peer
// makes the stream's next read fail as when the peer closes the link.
type peerConn struct {
	in       chan []byte
	writes   chan []byte
	peer     chan struct{}
	closed   chan struct{}
	peerOnce sync.Once
	once     sync.Once
}

func newPeerConn() *peerConn {
	return &peerConn{in: make(chan []byte, 4), writes: make(chan []byte, 16), peer: make(chan struct{}), closed: make(chan struct{})}
}

func (c *peerConn) ReadFrame() ([]byte, error) {
	select {
	case b := <-c.in:
		return b, nil
	case <-c.peer:
		return nil, errors.New("peer closed")
	case <-c.closed:
		return nil, errors.New("closed")
	}
}

func (c *peerConn) WriteFrame(b []byte) error {
	select {
	case <-c.closed:
		return errors.New("closed")
	case c.writes <- append([]byte(nil), b...):
		return nil
	}
}

func (c *peerConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *peerConn) closePeer() { c.peerOnce.Do(func() { close(c.peer) }) }

func (c *peerConn) Transport() string { return "test" }

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestWSStreamCloseWaitsForPeer: after CloseWithCode the stream has ended
// and StreamClose is sent at once, but the link stays open, discarding
// what the peer still sends, until the peer closes it or the close wait
// on the clock has passed.
func TestWSStreamCloseWaitsForPeer(t *testing.T) {
	const wait = 5 * time.Second
	window, err := proto.Marshal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamWindow{StreamWindow: &conduitv1.StreamWindow{StreamId: hopStreamID, Increment: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// finish ends the wait: the peer closes, or the clock passes it.
		finish func(t *testing.T, clk *clock.Fake, c *peerConn)
	}{
		{
			name: "peer closes after reading StreamClose",
			finish: func(_ *testing.T, _ *clock.Fake, c *peerConn) {
				c.closePeer()
			},
		},
		{
			name: "peer never closes",
			finish: func(t *testing.T, clk *clock.Fake, c *peerConn) {
				clk.Advance(wait - time.Nanosecond)
				if isClosed(c.closed) {
					t.Fatal("link closed before the close wait passed")
				}
				clk.Advance(time.Nanosecond)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock.NewFake(time.Now())
			c := newPeerConn()
			s := newWSStream(c, 8, 8, clk, wait)
			if err := s.CloseWithCode(conduit.CloseForbidden, "forbidden: test"); err != nil {
				t.Fatal(err)
			}
			if !isClosed(s.Done()) {
				t.Fatal("Done not closed after CloseWithCode")
			}
			f := &conduitv1.Frame{}
			if err := proto.Unmarshal(<-c.writes, f); err != nil {
				t.Fatal(err)
			}
			if sc := f.GetStreamClose(); sc == nil || sc.GetCode() != conduit.CloseForbidden || sc.GetReason() != "forbidden: test" {
				t.Fatalf("sent %v, want stream_close 4403", f)
			}
			if _, err := s.Write([]byte("x")); !errors.Is(err, conduit.ErrStreamClosed) {
				t.Fatalf("Write after close = %v, want ErrStreamClosed", err)
			}
			// The peer had not seen the close yet: its frame is read and
			// discarded, and the link stays open.
			c.in <- window
			if isClosed(c.closed) || isClosed(s.linkClosed()) {
				t.Fatal("link closed while waiting for the peer")
			}
			tc.finish(t, clk, c)
			select {
			case <-s.linkClosed():
			case <-time.After(5 * time.Second):
				t.Fatal("link not closed")
			}
			if !isClosed(c.closed) {
				t.Fatal("conn not closed")
			}
			if n := clk.Pending(); n != 0 {
				t.Fatalf("%d timers still armed", n)
			}
		})
	}
}
