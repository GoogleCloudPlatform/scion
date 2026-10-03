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
	"sync"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// stream is one multiplexed stream. Lock order: session.fcMu may be held
// while reading stream atomics, but never while taking stream.mu, and
// stream.mu is never held while taking session.mu.
type stream struct {
	s      *session
	id     uint32
	opener bool

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	cond  *sync.Cond
	state StreamState

	// Opening resolution (opener side): opened is closed when the stream
	// leaves the opening state; openErr is nil on accept.
	opened    chan struct{}
	openErr   error
	openTimer clock.Timer

	// Send side.
	writeMu     sync.Mutex // serialises Write calls
	sendCredit  int64      // peer-granted stream credit not yet used
	writeClosed bool       // CloseWrite or Close called
	dead        chan struct{}
	deadOnce    sync.Once
	writeDead   atomic.Bool // mirrors dead for lock-free checks

	// Receive side.
	buf           bytes.Buffer
	recvWindow    int64 // advertised receive window
	recvRemaining int64 // credit the peer still holds
	pendingWin    int64 // consumed bytes not yet returned
	remoteFin     bool
	remoteErr     error // delivered to Read after buf drains
	localClosed   bool  // Close/CloseWithCode called: reads stop
	closeSent     bool  // a StreamClose for this id was sent or queued
	removed       bool
	failErr       error // session failure
}

func newStream(s *session, id uint32, opener bool, recvWindow uint32) *stream {
	st := &stream{
		s:             s,
		id:            id,
		opener:        opener,
		state:         StateOpening,
		opened:        make(chan struct{}),
		dead:          make(chan struct{}),
		recvWindow:    int64(recvWindow),
		recvRemaining: int64(recvWindow),
	}
	st.cond = sync.NewCond(&st.mu)
	st.ctx, st.cancel = context.WithCancel(s.ctx)
	return st
}

// ID implements Stream.
func (st *stream) ID() uint32 { return st.id }

// State returns the lifecycle state.
func (st *stream) State() StreamState {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.state
}

// markDeadLocked makes the stream unwritable and wakes every waiter.
func (st *stream) markDeadLocked() {
	st.deadOnce.Do(func() {
		st.writeDead.Store(true)
		close(st.dead)
	})
	st.cond.Broadcast()
	st.s.wakeCreditWaiters()
}

func (st *stream) writableLocked() error {
	switch {
	case st.failErr != nil:
		return st.failErr
	case st.localClosed || st.writeClosed:
		return ErrStreamClosed
	case st.remoteErr != nil:
		return st.remoteErr
	case st.state == StateFailed || st.state == StateClosed:
		return ErrStreamClosed
	}
	return nil
}

// Write implements io.Writer. It blocks until every byte is queued for
// sending within stream and session credit; it never queues more than the
// peer granted.
func (st *stream) Write(p []byte) (int, error) {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	total := 0
	for len(p) > 0 {
		want := int64(min(len(p), MaxDataFrame))

		st.mu.Lock()
		for st.sendCredit <= 0 && st.writableLocked() == nil {
			st.cond.Wait()
		}
		if err := st.writableLocked(); err != nil {
			st.mu.Unlock()
			return total, err
		}
		n := min(want, st.sendCredit)
		st.mu.Unlock()

		n, err := st.s.acquireSessionCredit(st, n)
		if errors.Is(err, errStreamDead) {
			err = st.deadErr()
		}
		if err != nil {
			return total, err
		}

		st.mu.Lock()
		if err := st.writableLocked(); err != nil {
			st.mu.Unlock()
			st.s.releaseSessionCredit(n)
			return total, err
		}
		st.sendCredit -= n
		st.mu.Unlock()

		chunk := append([]byte(nil), p[:n]...)
		of := &outFrame{
			f:        &conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: st.id, Data: chunk}}},
			class:    classData,
			streamID: st.id,
			size:     n + dataFrameOverhead,
		}
		if err := st.s.sched.enqueue(of, st.dead, st.deadErr); err != nil {
			// Never queued: the peer will not see these bytes.
			st.s.releaseSessionCredit(n)
			return total, err
		}
		total += int(n)
		p = p[n:]
	}
	return total, nil
}

func (st *stream) deadErr() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.writableLocked(); err != nil {
		return err
	}
	return ErrStreamClosed
}

// Read implements io.Reader.
func (st *stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	st.mu.Lock()
	for st.buf.Len() == 0 && !st.remoteFin && st.remoteErr == nil && !st.localClosed && st.failErr == nil {
		st.cond.Wait()
	}
	if st.localClosed {
		st.mu.Unlock()
		return 0, ErrStreamClosed
	}
	if st.buf.Len() == 0 {
		err := st.failErr
		switch {
		case err != nil:
		case st.remoteErr != nil:
			err = st.remoteErr
		default:
			err = io.EOF
		}
		st.mu.Unlock()
		return 0, err
	}
	n, _ := st.buf.Read(p)
	var winUpdate int64
	st.pendingWin += int64(n)
	if !st.remoteFin && st.remoteErr == nil && st.pendingWin >= st.recvWindow/2 {
		winUpdate = st.pendingWin
		st.recvRemaining += winUpdate
		st.pendingWin = 0
	}
	st.mu.Unlock()

	if winUpdate > 0 {
		st.s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamWindow{StreamWindow: &conduitv1.StreamWindow{StreamId: st.id, Increment: uint32(winUpdate)}}})
	}
	st.s.consumed(int64(n))
	return n, nil
}

// deliver appends inbound data (reader goroutine). It returns false if the
// peer exceeded the stream credit.
func (st *stream) deliver(d *conduitv1.StreamData) (ok bool) {
	n := int64(len(d.GetData()))
	st.mu.Lock()
	if n > st.recvRemaining {
		st.mu.Unlock()
		return false
	}
	st.recvRemaining -= n
	if st.localClosed || st.remoteFin || st.remoteErr != nil || st.failErr != nil {
		st.mu.Unlock()
		st.s.returnSessionCredit(n) // discarded: return the session credit
		return true
	}
	if n > 0 {
		st.buf.Write(d.GetData())
		st.s.addRecvBuffered(n)
	}
	if d.GetFin() {
		st.remoteFin = true
	}
	st.cond.Broadcast()
	st.mu.Unlock()
	return true
}

// grant adds peer-granted send credit. It returns false on overflow.
func (st *stream) grant(inc uint32) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sendCredit += int64(inc)
	st.cond.Broadcast()
	return st.sendCredit <= MaxWindow
}

// discardBufferLocked drops unread data and returns its session credit.
func (st *stream) discardBufferLocked() int64 {
	n := int64(st.buf.Len())
	st.buf.Reset()
	return n
}

// Close implements io.Closer.
func (st *stream) Close() error { return st.CloseWithCode(CloseNormal, "") }

// CloseWithCode implements Stream. The StreamClose is queued behind the
// stream's pending data (the stream is draining until it is written).
func (st *stream) CloseWithCode(code uint32, reason string) error {
	st.mu.Lock()
	if st.localClosed {
		st.mu.Unlock()
		return nil
	}
	st.localClosed = true
	discarded := st.discardBufferLocked()
	alreadyEnded := st.closeSent || st.remoteErr != nil || st.failErr != nil
	if !alreadyEnded {
		st.closeSent = true
		st.state = StateDraining
	}
	st.markDeadLocked()
	st.mu.Unlock()
	st.s.consumed(discarded)

	if alreadyEnded {
		st.s.removeStream(st)
		return nil
	}
	of := &outFrame{
		f:        &conduitv1.Frame{Body: &conduitv1.Frame_StreamClose{StreamClose: &conduitv1.StreamClose{StreamId: st.id, Code: code, Reason: reason}}},
		class:    classData,
		streamID: st.id,
		size:     int64(32 + len(reason)),
		sent: func() {
			st.finish(code)
		},
	}
	if err := st.s.sched.enqueueInternal(of); err != nil {
		st.s.fail(err)
		return err
	}
	return nil
}

// finish moves a gracefully closed stream to its terminal state once its
// StreamClose has been written.
func (st *stream) finish(code uint32) {
	st.mu.Lock()
	if code == CloseNormal {
		st.state = StateClosed
	} else {
		st.state = StateFailed
	}
	st.mu.Unlock()
	st.cancel()
	st.s.removeStream(st)
}

// CloseWrite half-closes the sending direction (StreamData{fin}), queued
// behind pending data.
func (st *stream) CloseWrite() error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	st.mu.Lock()
	if err := st.writableLocked(); err != nil {
		st.mu.Unlock()
		return err
	}
	st.writeClosed = true
	st.cond.Broadcast()
	st.mu.Unlock()
	return st.s.sched.enqueueInternal(&outFrame{
		f:        &conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: st.id, Fin: true}}},
		class:    classData,
		streamID: st.id,
		size:     dataFrameOverhead,
	})
}

// Resize implements Stream.
func (st *stream) Resize(cols, rows uint16) error {
	st.mu.Lock()
	err := st.writableLocked()
	if errors.Is(err, ErrStreamClosed) && st.writeClosed && !st.localClosed {
		err = nil // half-closed streams may still resize
	}
	st.mu.Unlock()
	if err != nil {
		return err
	}
	return st.s.sched.enqueueInternal(&outFrame{
		f:     &conduitv1.Frame{Body: &conduitv1.Frame_StreamResize{StreamResize: &conduitv1.StreamResize{StreamId: st.id, Cols: uint32(cols), Rows: uint32(rows)}}},
		class: classControl,
		size:  24,
	})
}

// abort terminates the stream abortively: queued data is purged, a
// StreamClose is sent on the control queue (if send and none was sent
// yet), and readers/writers fail with *CloseError{code}.
func (st *stream) abort(code uint32, reason string, send bool) {
	st.mu.Lock()
	if st.state == StateClosed || st.state == StateFailed {
		send = send && !st.closeSent
		st.closeSent = st.closeSent || send
		st.mu.Unlock()
		if send {
			st.s.sendClose(st.id, code, reason)
		}
		return
	}
	wasOpening := st.state == StateOpening
	if code == CloseNormal {
		st.state = StateClosed
	} else {
		st.state = StateFailed
	}
	ce := &CloseError{Code: code, Reason: reason}
	if st.remoteErr == nil {
		st.remoteErr = ce
	}
	send = send && !st.closeSent
	st.closeSent = st.closeSent || send
	discarded := st.discardBufferLocked()
	if st.openTimer != nil {
		st.openTimer.Stop()
	}
	if wasOpening {
		st.openErr = ce
		close(st.opened)
	}
	st.markDeadLocked()
	st.mu.Unlock()

	st.s.consumed(discarded)
	st.s.restoreSessionCredit(st.s.sched.purge(st.id))
	if send {
		st.s.sendClose(st.id, code, reason)
	}
	st.cancel()
	st.s.removeStream(st)
}

// remoteClose handles a StreamClose from the peer. Buffered data is still
// delivered; then reads return io.EOF (code 0) or *CloseError.
func (st *stream) remoteClose(code uint32, reason string) {
	st.mu.Lock()
	wasOpening := st.state == StateOpening
	if code == CloseNormal && !wasOpening {
		st.state = StateClosed
	} else {
		st.state = StateFailed
	}
	if code == CloseNormal && !wasOpening {
		st.remoteFin = true
		if st.remoteErr == nil {
			st.remoteErr = io.EOF
		}
	} else if st.remoteErr == nil {
		st.remoteErr = &CloseError{Code: code, Reason: reason}
	}
	if st.openTimer != nil {
		st.openTimer.Stop()
	}
	if wasOpening {
		st.openErr = &CloseError{Code: code, Reason: reason}
		close(st.opened)
	}
	var discarded int64
	if st.localClosed {
		discarded = st.discardBufferLocked()
	}
	st.markDeadLocked()
	st.mu.Unlock()

	st.s.consumed(discarded)
	st.s.restoreSessionCredit(st.s.sched.purge(st.id))
	st.cancel()
	st.s.removeStream(st)
}

// sessionFailed fails the stream because the session ended.
func (st *stream) sessionFailed(err error) {
	st.mu.Lock()
	if st.failErr == nil {
		st.failErr = err
	}
	if st.state != StateClosed {
		st.state = StateFailed
	}
	if st.openTimer != nil {
		st.openTimer.Stop()
	}
	select {
	case <-st.opened:
	default:
		st.openErr = err
		close(st.opened)
	}
	st.markDeadLocked()
	st.mu.Unlock()
	st.cancel()
}

// --- inbound opening (target side) ---

// pendingStream is the PendingStream handed to a StreamHandler.
type pendingStream struct{ st *stream }

func (p pendingStream) ID() uint32 { return p.st.id }

func (p pendingStream) Accept() (Stream, error) {
	st := p.st
	st.mu.Lock()
	if st.state != StateOpening {
		send := !st.closeSent
		st.closeSent = true
		st.mu.Unlock()
		if send {
			st.s.sendClose(st.id, CloseCancelled, "late accept")
		}
		return nil, ErrStreamCancelled
	}
	if st.openTimer != nil {
		st.openTimer.Stop()
	}
	st.state = StateAccepted
	close(st.opened)
	// Queue StreamAccept while holding st.mu so no data frame for this
	// stream can be queued before it (control is also served first).
	err := st.s.sched.enqueueInternal(&outFrame{
		f:     &conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{StreamId: st.id, InitialWindow: uint32(st.recvWindow)}}},
		class: classControl,
		size:  16,
	})
	if err == nil {
		st.state = StateActive
	}
	st.mu.Unlock()
	if err != nil {
		st.s.fail(err)
		return nil, err
	}
	st.s.markDrainingIfNeeded(st)
	return st, nil
}

func (p pendingStream) Reject(code uint32, reason string) error {
	st := p.st
	st.mu.Lock()
	opening := st.state == StateOpening
	st.mu.Unlock()
	if !opening {
		return ErrStreamCancelled
	}
	st.abort(code, reason, true)
	return nil
}
