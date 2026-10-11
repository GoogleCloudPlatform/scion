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

package hub

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// The splice of a user tunnel (design §3.5, §3.8): conduit frames are
// copied between the user's stream and the target stream, never a raw TCP
// splice. Each leg keeps its own credit flow control, so backpressure
// propagates end to end; half-closes, close codes and pty resizes are
// passed through, and a close of either leg closes the other.

// Close reason when one leg ended without a close code (its session was
// lost).
const conduitTunnelReasonUpstreamUnreachable = "upstream_unreachable"

// conduitTunnelCopyBuffer is the size of one read while copying between
// the two legs of a tunnel.
const conduitTunnelCopyBuffer = 32 << 10

// conduitTunnel is one open tunnel: the stream on the user session and
// the target stream on the agent's session.
type conduitTunnel struct {
	key     conduitTunnelKey
	kind    string
	user    conduit.Stream
	target  conduit.Stream
	untrack func()
	tracked atomic.Pointer[conduitUserStream]
	// closing is the close code and reason the authorization re-check
	// gave the tunnel (nil: none). Once set, every close the splice makes
	// on either leg carries it.
	closing atomic.Pointer[conduit.CloseError]
}

// deadline returns the tunnel's authorization deadline, if tracked.
func (t *conduitTunnel) deadline() time.Time {
	if st := t.tracked.Load(); st != nil {
		return st.Deadline()
	}
	return time.Time{}
}

// closeBoth is the re-check's Close: both legs end with code and reason.
// The code is recorded first (the first recorded close wins), so the
// splice passes the same code to both legs whichever leg closes first. It
// never blocks the caller.
func (t *conduitTunnel) closeBoth(code uint32, reason string) {
	t.closing.CompareAndSwap(nil, &conduit.CloseError{Code: code, Reason: reason})
	c := t.closing.Load()
	go func() { _ = t.user.CloseWithCode(c.Code, c.Reason) }()
	go func() { _ = t.target.CloseWithCode(c.Code, c.Reason) }()
}

// run copies framed data both ways until both directions end, forwarding
// resizes on pty tunnels, then releases the tunnel.
func (t *conduitTunnel) run(ts *conduitTunnelSession) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.pump(t.target, t.user) }()
	go func() { defer wg.Done(); t.pump(t.user, t.target) }()
	if t.kind == grant.StreamKindPTY {
		if rz, ok := t.user.(conduit.Resizable); ok {
			go func() {
				for sz := range rz.Resizes() {
					if sz.Cols < ptyMinDim || sz.Cols > conduitPTYMaxDim || sz.Rows < ptyMinDim || sz.Rows > conduitPTYMaxDim {
						continue
					}
					_ = t.target.Resize(sz.Cols, sz.Rows)
				}
			}()
		}
	}
	wg.Wait()
	code, reason := t.closeFor(io.EOF)
	_ = t.user.CloseWithCode(code, reason)
	_ = t.target.CloseWithCode(code, reason)
	if t.untrack != nil {
		t.untrack()
	}
	ts.remove(t.key)
}

// pump copies src to dst. A half-close (fin) half-closes dst; a close of
// src, with any code, closes dst with the same code; a failed write closes
// src with the code dst ended with. Once the tunnel has a recorded close
// (closing), every close here carries that code instead.
func (t *conduitTunnel) pump(dst, src conduit.Stream) {
	buf := make([]byte, conduitTunnelCopyBuffer)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				code, reason := t.closeFor(werr)
				_ = src.CloseWithCode(code, reason)
				return
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) && t.closing.Load() == nil && !conduitStreamEnded(src) {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
				return
			}
		}
		code, reason := t.closeFor(err)
		_ = dst.CloseWithCode(code, reason)
		return
	}
}

// closeFor returns the close code and reason that pass the end of one leg
// on to the other: the tunnel's recorded close if it has one, else the
// code the leg ended with.
func (t *conduitTunnel) closeFor(err error) (uint32, string) {
	if c := t.closing.Load(); c != nil {
		return c.Code, c.Reason
	}
	return conduitTunnelCloseFor(err)
}

// conduitTunnelCloseFor returns the close code and reason that pass the
// end of one leg on to the other.
func conduitTunnelCloseFor(err error) (uint32, string) {
	var ce *conduit.CloseError
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, conduit.ErrStreamClosed):
		return conduit.CloseNormal, ""
	case errors.As(err, &ce):
		return ce.Code, ce.Reason
	default:
		return conduit.CloseRelayTimeout, conduitTunnelReasonUpstreamUnreachable
	}
}

// conduitStreamEnded reports whether st has ended in both directions (the
// peer closed it, rather than only half-closing).
func conduitStreamEnded(st conduit.Stream) bool {
	switch v := st.(type) {
	case interface{ State() conduit.StreamState }:
		s := v.State()
		return s == conduit.StateClosed || s == conduit.StateFailed
	case interface{ Done() <-chan struct{} }:
		select {
		case <-v.Done():
			return true
		default:
			return false
		}
	}
	return false
}
