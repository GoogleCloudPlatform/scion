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

package transport

import (
	"context"
	"sync"
)

// MemoryOptions configures an in-memory pipe.
type MemoryOptions struct {
	// Buffer is the number of messages a direction buffers before
	// WriteFrame blocks. 0 means fully synchronous (a write blocks until
	// the peer reads), which is what tests use to simulate a peer that
	// stopped reading.
	Buffer int
	// MaxFrame rejects messages larger than this many bytes (0 = no limit).
	MaxFrame int
}

// Pipe returns two connected in-memory Conns.
func Pipe(opts MemoryOptions) (Conn, Conn) {
	ab := make(chan []byte, opts.Buffer)
	ba := make(chan []byte, opts.Buffer)
	done := make(chan struct{})
	shared := &pipeShared{done: done}
	a := &memConn{in: ba, out: ab, shared: shared, maxFrame: opts.MaxFrame}
	b := &memConn{in: ab, out: ba, shared: shared, maxFrame: opts.MaxFrame}
	return a, b
}

// pipeShared closes both directions together, like a TCP reset: closing
// either end makes reads and writes on both ends fail.
type pipeShared struct {
	once sync.Once
	done chan struct{}
}

type memConn struct {
	in, out  chan []byte
	shared   *pipeShared
	maxFrame int
}

func (c *memConn) ReadFrame() ([]byte, error) {
	// Frames written before Close are still delivered, as on a WebSocket
	// whose close message follows its last data message.
	select {
	case b := <-c.in:
		return b, nil
	default:
	}
	select {
	case b := <-c.in:
		return b, nil
	case <-c.shared.done:
		select {
		case b := <-c.in:
			return b, nil
		default:
			return nil, ErrClosed
		}
	}
}

func (c *memConn) WriteFrame(b []byte) error {
	if c.maxFrame > 0 && len(b) > c.maxFrame {
		return ErrFrameTooLarge
	}
	cp := append([]byte(nil), b...)
	select {
	case <-c.shared.done:
		return ErrClosed
	default:
	}
	select {
	case c.out <- cp:
		return nil
	case <-c.shared.done:
		return ErrClosed
	}
}

func (c *memConn) Close() error {
	c.shared.once.Do(func() { close(c.shared.done) })
	return nil
}

func (c *memConn) Transport() string { return Memory }

// MemoryListener pairs Dial calls with Accept calls in process. It
// implements Dialer for the dialer side.
type MemoryListener struct {
	opts   MemoryOptions
	conns  chan Conn
	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// NewMemoryListener returns a listener whose pipes use opts.
func NewMemoryListener(opts MemoryOptions) *MemoryListener {
	return &MemoryListener{opts: opts, conns: make(chan Conn), done: make(chan struct{})}
}

// Dial implements Dialer. It blocks until Accept takes the other end.
func (l *MemoryListener) Dial(ctx context.Context) (Conn, error) {
	a, b := Pipe(l.opts)
	select {
	case l.conns <- b:
		return a, nil
	case <-l.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Accept returns the relay end of the next dialed pipe.
func (l *MemoryListener) Accept(ctx context.Context) (Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops the listener; pending and future Dial/Accept calls fail.
func (l *MemoryListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.done)
	}
	return nil
}
