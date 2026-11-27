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

// Package conduittest provides reusable Conduit test fakes (fault-injectable
// WebSocket dialer, fake agent, fake broker, and fake PTY) shared by the
// 2-node Postgres HA harness tests in pkg/hub and the hack/conduit-c19 load
// driver. It must not be imported by production binaries.
package conduittest

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// FaultConn wraps a net.Conn so a test can abruptly close it or simulate a
// transient silent TCP partition: while dropped, both Read and Write block
// until Restore, Close, or their respective deadlines expire, without sending
// a TCP FIN/RST or WebSocket close frame. On Restore, pending writes proceed
// and buffered inbound data is read, so pending frames are delayed rather than
// discarded (subject to any active read/write deadlines).
type FaultConn struct {
	net.Conn
	mu            sync.Mutex
	dropped       bool
	unblock       chan struct{}
	deadlineCh    chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	readDeadline  time.Time
	writeDeadline time.Time
}

// NewFaultConn wraps c in a FaultConn.
func NewFaultConn(c net.Conn) *FaultConn {
	return &FaultConn{
		Conn:       c,
		unblock:    make(chan struct{}),
		deadlineCh: make(chan struct{}),
		closed:     make(chan struct{}),
	}
}

// SetDropped toggles the silent TCP partition state.
func (c *FaultConn) SetDropped(drop bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == drop {
		return
	}
	c.dropped = drop
	if !drop {
		close(c.unblock)
		c.unblock = make(chan struct{})
	}
}

func (c *FaultConn) waitIfDropped(isWrite bool) error {
	for {
		c.mu.Lock()
		dropped := c.dropped
		unblock := c.unblock
		deadlineCh := c.deadlineCh
		deadline := c.readDeadline
		if isWrite {
			deadline = c.writeDeadline
		}
		c.mu.Unlock()
		if !dropped {
			return nil
		}
		var timer <-chan time.Time
		var t *time.Timer
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return osTimeoutErr{}
			}
			t = time.NewTimer(d)
			timer = t.C
		}
		select {
		case <-c.closed:
			if t != nil {
				t.Stop()
			}
			return net.ErrClosed
		case <-unblock:
			if t != nil {
				t.Stop()
			}
		case <-deadlineCh:
			if t != nil {
				t.Stop()
			}
		case <-timer:
			return osTimeoutErr{}
		}
	}
}

// Read implements net.Conn.
func (c *FaultConn) Read(b []byte) (int, error) {
	if err := c.waitIfDropped(false); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(b)
	c.mu.Lock()
	dropped := c.dropped
	c.mu.Unlock()
	if dropped {
		if werr := c.waitIfDropped(false); werr != nil {
			return 0, werr
		}
	}
	return n, err
}

// Write implements net.Conn.
func (c *FaultConn) Write(b []byte) (int, error) {
	if err := c.waitIfDropped(true); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func (c *FaultConn) notifyDeadlineChange() {
	close(c.deadlineCh)
	c.deadlineCh = make(chan struct{})
}

// SetReadDeadline implements net.Conn.
func (c *FaultConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.notifyDeadlineChange()
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

// SetWriteDeadline implements net.Conn.
func (c *FaultConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.notifyDeadlineChange()
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

// SetDeadline implements net.Conn.
func (c *FaultConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.writeDeadline = t
	c.notifyDeadlineChange()
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

// Close implements net.Conn.
func (c *FaultConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type osTimeoutErr struct{}

func (osTimeoutErr) Error() string   { return "i/o timeout" }
func (osTimeoutErr) Timeout() bool   { return true }
func (osTimeoutErr) Temporary() bool { return true }

// FaultWSDialer tracks the latest dialed FaultConn so tests can close or
// silently pause the active WebSocket transport.
type FaultWSDialer struct {
	mu      sync.Mutex
	current *FaultConn
	dialer  *websocket.Dialer
}

// NewFaultWSDialer creates a FaultWSDialer wrapping a standard TCP dialer.
func NewFaultWSDialer() *FaultWSDialer {
	f := &FaultWSDialer{}
	base := &net.Dialer{Timeout: 10 * time.Second}
	f.dialer = &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := base.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			fc := NewFaultConn(raw)
			f.mu.Lock()
			f.current = fc
			f.mu.Unlock()
			return fc, nil
		},
	}
	return f
}

// Dialer returns the underlying *websocket.Dialer.
func (f *FaultWSDialer) Dialer() *websocket.Dialer {
	return f.dialer
}

// CloseCurrent abruptly closes the most recently dialed TCP connection.
func (f *FaultWSDialer) CloseCurrent() {
	f.mu.Lock()
	c := f.current
	f.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// SilentDrop stalls reads and writes on the most recently dialed TCP conn.
func (f *FaultWSDialer) SilentDrop() {
	f.mu.Lock()
	c := f.current
	f.mu.Unlock()
	if c != nil {
		c.SetDropped(true)
	}
}

// Restore resumes reads and writes on the most recently dialed TCP conn.
func (f *FaultWSDialer) Restore() {
	f.mu.Lock()
	c := f.current
	f.mu.Unlock()
	if c != nil {
		c.SetDropped(false)
	}
}
