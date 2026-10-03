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

// Package transport is the only conduit package that knows how frames move
// between peers (design §3.6). The session layer in pkg/conduit sees a Conn:
// an ordered, reliable, message-oriented duplex carrying one encoded Frame
// per message. Transports never interpret frames.
package transport

import (
	"context"
	"errors"
)

// Canonical transport names (contracts §2).
const (
	WS     = "ws"
	GRPC   = "grpc"
	H1Pair = "h1pair"
	// Memory is the in-process transport used by tests. It is never
	// recorded in the registry.
	Memory = "memory"
)

// ErrClosed is returned by Conn operations after Close.
var ErrClosed = errors.New("conduit/transport: connection closed")

// ErrFrameTooLarge is returned when a message exceeds the transport limit.
var ErrFrameTooLarge = errors.New("conduit/transport: frame exceeds transport limit")

// Conn is one physical connection carrying encoded frames.
//
// ReadFrame is called from a single goroutine and WriteFrame from a single
// (other) goroutine; Close may be called concurrently with both and must
// unblock them. Implementations may apply their own deadlines, but the
// session enforces keepalive and write timeouts itself by closing the Conn.
type Conn interface {
	// ReadFrame blocks until the next message arrives.
	ReadFrame() ([]byte, error)
	// WriteFrame sends one message. The slice may be reused after return.
	WriteFrame(b []byte) error
	// Close tears the connection down. It is idempotent.
	Close() error
	// Transport returns the canonical transport name.
	Transport() string
}

// Dialer opens a new Conn to the relay. Credentials travel in transport
// headers, so a Dialer is responsible for attaching them (fresh on every
// call).
type Dialer interface {
	Dial(ctx context.Context) (Conn, error)
}

// DialerFunc adapts a function to Dialer.
type DialerFunc func(ctx context.Context) (Conn, error)

// Dial implements Dialer.
func (f DialerFunc) Dial(ctx context.Context) (Conn, error) { return f(ctx) }
