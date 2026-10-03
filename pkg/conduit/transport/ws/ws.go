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

// Package ws is the conduit "ws" transport (design §3.6): one encoded
// Frame per binary WebSocket message over gorilla/websocket, using the
// limits and timeouts of pkg/wsprotocol.
package ws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// Options tunes a ws Conn. Zero values take the wsprotocol defaults.
type Options struct {
	// MaxMessageSize bounds an inbound message (default
	// wsprotocol.DefaultMaxMessageSize, 1 MiB: room for a 768 KiB RPC body
	// plus headers, and far above the 64 KiB data frame).
	MaxMessageSize int64
	// WriteWait is the per-message write deadline (default
	// wsprotocol.DefaultWriteWait, 10s). The session enforces its own
	// write timeout as well; this one guards the socket.
	WriteWait time.Duration
}

func (o Options) withDefaults() Options {
	if o.MaxMessageSize <= 0 {
		o.MaxMessageSize = wsprotocol.DefaultMaxMessageSize
	}
	if o.WriteWait <= 0 {
		o.WriteWait = wsprotocol.DefaultWriteWait
	}
	return o
}

// ErrNotBinary is returned when the peer sends a non-binary data message.
var ErrNotBinary = errors.New("conduit/ws: non-binary message")

// Conn adapts a *websocket.Conn to transport.Conn.
type Conn struct {
	c    *websocket.Conn
	opts Options

	closeOnce sync.Once
	closeErr  error
}

var _ transport.Conn = (*Conn)(nil)

// New wraps an established WebSocket connection.
func New(c *websocket.Conn, opts Options) *Conn {
	opts = opts.withDefaults()
	c.SetReadLimit(opts.MaxMessageSize)
	return &Conn{c: c, opts: opts}
}

// ReadFrame implements transport.Conn.
func (c *Conn) ReadFrame() ([]byte, error) {
	mt, b, err := c.c.ReadMessage()
	if err != nil {
		if errors.Is(err, websocket.ErrReadLimit) {
			return nil, transport.ErrFrameTooLarge
		}
		return nil, err
	}
	if mt != websocket.BinaryMessage {
		return nil, ErrNotBinary
	}
	return b, nil
}

// WriteFrame implements transport.Conn. It must not be called
// concurrently with itself.
func (c *Conn) WriteFrame(b []byte) error {
	if err := c.c.SetWriteDeadline(time.Now().Add(c.opts.WriteWait)); err != nil {
		return err
	}
	return c.c.WriteMessage(websocket.BinaryMessage, b)
}

// Close implements transport.Conn. It sends a best-effort close message
// (gorilla allows WriteControl concurrently with WriteMessage) and closes
// the socket, which unblocks a pending read or write.
func (c *Conn) Close() error {
	return c.CloseWithCode(websocket.CloseNormalClosure, "")
}

// CloseWithCode closes with a WebSocket close code (e.g. a conduit 44xx
// code, which lies in the application range 4000-4999).
func (c *Conn) CloseWithCode(code int, reason string) error {
	c.closeOnce.Do(func() {
		_ = c.c.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(code, wsprotocol.TruncateCloseReason(reason)),
			time.Now().Add(c.opts.WriteWait))
		c.closeErr = c.c.Close()
	})
	return c.closeErr
}

// Transport implements transport.Conn.
func (c *Conn) Transport() string { return transport.WS }

// Underlying exposes the gorilla connection (for addresses and tests).
func (c *Conn) Underlying() *websocket.Conn { return c.c }

// Upgrade upgrades an authenticated HTTP request to a conduit ws Conn.
// Authentication happens before Upgrade (credentials are transport
// headers), so the default upgrader accepts every origin, as
// wsprotocol.DefaultUpgrader does.
func Upgrade(w http.ResponseWriter, r *http.Request, up *websocket.Upgrader, opts Options) (*Conn, error) {
	if up == nil {
		u := wsprotocol.DefaultUpgrader()
		up = &u
	}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	return New(c, opts), nil
}

// DialError reports a failed WebSocket handshake. StatusCode is the HTTP
// status when the server answered (0 for network or TLS failures). The
// transport negotiation rules (§3.6) key off it: 401/403 is an auth
// denial and never a reason to downgrade transports.
type DialError struct {
	StatusCode int
	Err        error
}

func (e *DialError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("conduit/ws: dial: HTTP %d: %v", e.StatusCode, e.Err)
	}
	return fmt.Sprintf("conduit/ws: dial: %v", e.Err)
}

func (e *DialError) Unwrap() error { return e.Err }

// IsAuthDenial reports whether the handshake was refused for
// authentication or policy reasons.
func (e *DialError) IsAuthDenial() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// Dialer dials the relay's conduit endpoint. It implements
// transport.Dialer.
type Dialer struct {
	// URL is the ws:// or wss:// conduit endpoint.
	URL string
	// Header returns the handshake headers (credentials) for one attempt.
	// It is called on every Dial so rotated credentials are picked up.
	Header func(ctx context.Context) (http.Header, error)
	// WS overrides the gorilla dialer (TLS config, proxy). Nil uses a
	// dialer with the wsprotocol buffer sizes.
	WS *websocket.Dialer
	// Options apply to the resulting Conn.
	Options Options
}

var _ transport.Dialer = (*Dialer)(nil)

// Dial implements transport.Dialer.
func (d *Dialer) Dial(ctx context.Context) (transport.Conn, error) {
	var h http.Header
	if d.Header != nil {
		var err error
		if h, err = d.Header(ctx); err != nil {
			return nil, fmt.Errorf("conduit/ws: handshake headers: %w", err)
		}
	}
	wd := d.WS
	if wd == nil {
		wd = &websocket.Dialer{
			ReadBufferSize:   wsprotocol.DefaultReadBufferSize,
			WriteBufferSize:  wsprotocol.DefaultWriteBufferSize,
			HandshakeTimeout: 30 * time.Second,
			Proxy:            http.ProxyFromEnvironment,
		}
	}
	c, resp, err := wd.DialContext(ctx, d.URL, h)
	if err != nil {
		de := &DialError{Err: err}
		if resp != nil {
			de.StatusCode = resp.StatusCode
			_ = resp.Body.Close()
		}
		return nil, de
	}
	return New(c, d.Options), nil
}
