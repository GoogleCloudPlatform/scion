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
	"bytes"
	"errors"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// hopStreamID is the stream id on an internal stream hop. Each internal WS
// carries exactly one stream, so the id is fixed.
const hopStreamID = 1

// errLinkLost closes a hop stream whose internal WS failed.
var errLinkLost = closeErr(conduit.CloseRelayTimeout, ReasonUpstreamUnreachable, "relay link lost")

// wsStream is one conduit stream carried over one internal WebSocket
// between two relays (design §3.5). It speaks conduit frames only
// (StreamData, StreamWindow, StreamClose, StreamResize) with the same
// credit-based flow control as a session: a side sends data only within
// the window its peer granted and returns credit as its reader consumes.
// It implements conduit.Stream, conduit.Resizable and CloseWrite.
//
// The opening handshake (StreamOpen, StreamAccept) is done by the caller
// before or around newWSStream; see client.go and internal.go.
type wsStream struct {
	conn transport.Conn
	win  uint32 // our receive window size, for credit return

	wmu sync.Mutex // serialises conn.WriteFrame

	mu         sync.Mutex
	cond       *sync.Cond
	rbuf       bytes.Buffer
	recvAvail  int64  // bytes the peer may still send us
	consumed   uint32 // bytes read but not yet returned as credit
	sendCredit int64  // bytes we may still send
	rfin       bool   // peer half-closed
	wfin       bool   // we half-closed
	ended      bool   // closed (either side) or link lost
	endErr     error  // nil: normal close (Read -> io.EOF after the buffer drains)
	localClose bool   // we closed: local ops fail with ErrStreamClosed

	resizes chan conduit.WindowSize
	done    chan struct{}
}

var (
	_ conduit.Stream    = (*wsStream)(nil)
	_ conduit.Resizable = (*wsStream)(nil)
)

// newWSStream starts the frame reader. sendCredit is the peer's initial
// window; recvWin is the window we grant (0 until the stream is accepted:
// see grant).
func newWSStream(conn transport.Conn, sendCredit, recvWin uint32) *wsStream {
	s := &wsStream{
		conn:       conn,
		win:        recvWin,
		recvAvail:  int64(recvWin),
		sendCredit: int64(sendCredit),
		resizes:    make(chan conduit.WindowSize, 1),
		done:       make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	go s.readLoop()
	return s
}

// grant opens our receive window (owner side, on accept) and sends
// StreamAccept carrying it.
func (s *wsStream) grant(win uint32) error {
	s.mu.Lock()
	s.win = win
	s.recvAvail += int64(win)
	s.mu.Unlock()
	return s.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{StreamId: hopStreamID, InitialWindow: win}}})
}

// Done is closed when the stream has ended (either side closed or the
// link was lost).
func (s *wsStream) Done() <-chan struct{} { return s.done }

// endedErr returns the end reason once ended (nil for a normal close).
func (s *wsStream) endedErr() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended, s.endErr
}

func (s *wsStream) send(f *conduitv1.Frame) error {
	b, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.conn.WriteFrame(b)
}

func (s *wsStream) readLoop() {
	for {
		b, err := s.conn.ReadFrame()
		if err != nil {
			s.end(errLinkLost, false)
			return
		}
		f := &conduitv1.Frame{}
		if err := proto.Unmarshal(b, f); err != nil {
			s.fail(conduit.CloseProtocolError, reason(ReasonBadFrame, "malformed frame"))
			return
		}
		switch body := f.GetBody().(type) {
		case *conduitv1.Frame_StreamData:
			d := body.StreamData
			s.mu.Lock()
			if s.rfin || int64(len(d.GetData())) > s.recvAvail {
				s.mu.Unlock()
				s.fail(conduit.CloseProtocolError, reason(ReasonBadFrame, "flow-control violation"))
				return
			}
			s.recvAvail -= int64(len(d.GetData()))
			s.rbuf.Write(d.GetData())
			if d.GetFin() {
				s.rfin = true
			}
			s.cond.Broadcast()
			s.mu.Unlock()
		case *conduitv1.Frame_StreamWindow:
			s.mu.Lock()
			s.sendCredit += int64(body.StreamWindow.GetIncrement())
			s.cond.Broadcast()
			s.mu.Unlock()
		case *conduitv1.Frame_StreamResize:
			r := body.StreamResize
			ws := conduit.WindowSize{Cols: clamp16(r.GetCols()), Rows: clamp16(r.GetRows())}
			s.mu.Lock()
			if !s.ended {
				select { // latest size wins; never block the reader
				case <-s.resizes:
				default:
				}
				s.resizes <- ws
			}
			s.mu.Unlock()
		case *conduitv1.Frame_StreamClose:
			c := body.StreamClose
			var e error
			if c.GetCode() != conduit.CloseNormal {
				e = &conduit.CloseError{Code: c.GetCode(), Reason: c.GetReason()}
			}
			s.end(e, false)
			return
		default:
			s.fail(conduit.CloseProtocolError, reason(ReasonBadFrame, "unexpected frame on a stream hop"))
			return
		}
	}
}

// fail closes the hop with code after a protocol violation by the peer.
func (s *wsStream) fail(code uint32, reason string) {
	_ = s.send(closeFrame(code, reason))
	s.end(&conduit.CloseError{Code: code, Reason: reason}, false)
}

// end marks the stream ended (first reason wins), wakes every waiter and
// closes the link.
func (s *wsStream) end(err error, local bool) bool {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return false
	}
	s.ended, s.endErr, s.localClose = true, err, local
	close(s.resizes)
	s.cond.Broadcast()
	s.mu.Unlock()
	_ = s.conn.Close()
	close(s.done)
	return true
}

// Read implements io.Reader.
func (s *wsStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	for s.rbuf.Len() == 0 && !s.rfin && !s.ended {
		s.cond.Wait()
	}
	if s.rbuf.Len() > 0 {
		n, _ := s.rbuf.Read(p)
		var inc uint32
		s.consumed += uint32(n)
		if !s.ended && !s.rfin && s.consumed >= s.win/2 {
			inc, s.consumed = s.consumed, 0
			s.recvAvail += int64(inc)
		}
		s.mu.Unlock()
		if inc > 0 {
			// The peer cannot send more without this credit, so a failed
			// send ends the stream as a lost link, as a failed Write does.
			if err := s.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamWindow{StreamWindow: &conduitv1.StreamWindow{StreamId: hopStreamID, Increment: inc}}}); err != nil {
				s.end(errLinkLost, false)
			}
		}
		return n, nil
	}
	defer s.mu.Unlock()
	switch {
	case s.localClose:
		return 0, conduit.ErrStreamClosed
	case s.rfin || s.endErr == nil:
		return 0, io.EOF
	default:
		return 0, s.endErr
	}
}

// Write implements io.Writer, splitting into MaxDataFrame frames within
// the peer's credit.
func (s *wsStream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		s.mu.Lock()
		for s.sendCredit <= 0 && !s.ended && !s.wfin {
			s.cond.Wait()
		}
		if err := s.writeErrLocked(); err != nil {
			s.mu.Unlock()
			return written, err
		}
		n := int64(len(p))
		n = min(n, s.sendCredit, int64(conduit.MaxDataFrame))
		s.sendCredit -= n
		s.mu.Unlock()
		if err := s.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: hopStreamID, Data: p[:n]}}}); err != nil {
			s.end(errLinkLost, false)
			return written, errLinkLost
		}
		written += int(n)
		p = p[n:]
	}
	return written, nil
}

func (s *wsStream) writeErrLocked() error {
	switch {
	case s.wfin, s.localClose:
		return conduit.ErrStreamClosed
	case s.ended && s.endErr != nil:
		return s.endErr
	case s.ended:
		return conduit.ErrStreamClosed
	}
	return nil
}

// CloseWrite half-closes: StreamData{fin}.
func (s *wsStream) CloseWrite() error {
	s.mu.Lock()
	if err := s.writeErrLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.wfin = true
	s.cond.Broadcast()
	s.mu.Unlock()
	return s.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: hopStreamID, Fin: true}}})
}

// Resize implements conduit.Stream.
func (s *wsStream) Resize(cols, rows uint16) error {
	if ended, _ := s.endedErr(); ended {
		return conduit.ErrStreamClosed
	}
	return s.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamResize{StreamResize: &conduitv1.StreamResize{StreamId: hopStreamID, Cols: uint32(cols), Rows: uint32(rows)}}})
}

// Resizes implements conduit.Resizable.
func (s *wsStream) Resizes() <-chan conduit.WindowSize { return s.resizes }

// Close implements io.Closer.
func (s *wsStream) Close() error { return s.CloseWithCode(conduit.CloseNormal, "") }

// CloseWithCode sends StreamClose{code} and closes the link. Data already
// written was sent synchronously, so nothing is lost.
func (s *wsStream) CloseWithCode(code uint32, reason string) error {
	if ended, _ := s.endedErr(); ended {
		return nil
	}
	_ = s.send(closeFrame(code, reason))
	s.end(nil, true)
	return nil
}

// ID implements conduit.Stream (the hop-local id).
func (s *wsStream) ID() uint32 { return hopStreamID }

func closeFrame(code uint32, reason string) *conduitv1.Frame {
	return &conduitv1.Frame{Body: &conduitv1.Frame_StreamClose{StreamClose: &conduitv1.StreamClose{StreamId: hopStreamID, Code: code, Reason: reason}}}
}

func clamp16(v uint32) uint16 {
	if v > 0xffff {
		return 0xffff
	}
	return uint16(v)
}

// splice copies between two framed streams until both directions finish
// (design §3.5: every hop is framed; this is a frame-level copy, never a
// raw socket splice). A half-close (EOF) on one side becomes CloseWrite on
// the other; a close code (or a failed write) closes both sides with that
// code; resizes are forwarded both ways.
func splice(a, b conduit.Stream) {
	var once sync.Once
	abort := func(code uint32, reason string) {
		once.Do(func() {
			_ = a.CloseWithCode(code, reason)
			_ = b.CloseWithCode(code, reason)
		})
	}
	var copies, fwds sync.WaitGroup
	cp := func(dst, src conduit.Stream) {
		defer copies.Done()
		buf := make([]byte, conduit.MaxDataFrame)
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					abort(spliceClose(werr, "peer leg closed"))
					return
				}
			}
			switch {
			case rerr == nil:
				continue
			case errors.Is(rerr, io.EOF):
				if cw, ok := dst.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
					return
				}
				abort(conduit.CloseNormal, "")
				return
			default:
				abort(spliceClose(rerr, "stream failed"))
				return
			}
		}
	}
	fwd := func(dst, src conduit.Stream) {
		defer fwds.Done()
		r, ok := src.(conduit.Resizable)
		if !ok {
			return
		}
		for ws := range r.Resizes() {
			_ = dst.Resize(ws.Cols, ws.Rows)
		}
	}
	copies.Add(2)
	fwds.Add(2)
	go cp(a, b)
	go cp(b, a)
	go fwd(a, b)
	go fwd(b, a)
	copies.Wait()
	// Both directions finished (half-closed both ways, or aborted): end
	// both streams, which also ends the resize forwarders.
	abort(conduit.CloseNormal, "")
	fwds.Wait()
}

// spliceClose maps a splice read/write error to the close sent to both
// legs: a leg that ended with a normal close (1000) ends the other one
// normally too (design §3.3.1: 1000 is a normal end, never 4504);
// anything else as codeAndReason.
func spliceClose(err error, detail string) (uint32, string) {
	var ce *conduit.CloseError
	if errors.As(err, &ce) && ce.Code == conduit.CloseNormal {
		return conduit.CloseNormal, ""
	}
	return codeAndReason(err, detail)
}

// codeAndReason maps a hop or target error to a §3.3.1 close: the code and
// reason of a *conduit.CloseError as is, otherwise 4504
// upstream_unreachable with detail.
func codeAndReason(err error, detail string) (uint32, string) {
	var ce *conduit.CloseError
	if errors.As(err, &ce) && ce.Code != conduit.CloseNormal {
		return ce.Code, ce.Reason
	}
	return conduit.CloseRelayTimeout, reason(ReasonUpstreamUnreachable, detail)
}
