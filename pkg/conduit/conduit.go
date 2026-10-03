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

// Package conduit implements the conduit session protocol (design §3.3,
// §3.5): one long-lived, multiplexed, flow-controlled connection between a
// principal (broker, agent, user, relay-peer) and a relay, carrying
// HTTP-shaped RPCs and bidirectional byte streams.
//
// The package is transport-agnostic (see pkg/conduit/transport) and has no
// hub wiring: the relay side plugs in authentication and registry work
// through Admitter, the target side plugs in stream and RPC handling
// through StreamHandler and RPCHandler.
//
// Load-bearing mechanics implemented here:
//   - odd/even stream ids (dialer/relay);
//   - mandatory credit flow control, 256 KiB per stream and 4 MiB per
//     session by default; a sender never exceeds credit;
//   - a scheduler that sends control frames from a reserved queue ahead of
//     stream data, enforces an aggregate buffer budget (16 MiB) and
//     round-robins data across streams;
//   - the stream state machine opening → accepted → active → draining →
//     closed | failed, with a bounded opening state, cancel-during-opening
//     (4499) and late-accept cleanup;
//   - GoAway with a drain deadline (4503);
//   - keepalive (ping 30s, pong wait 60s, write 10s) in both directions;
//   - 64 KiB maximum data payload, RPC bodies above 768 KiB answered 413;
//   - AuthRefresh plumbing to Admitter.Refresh.
package conduit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Close codes carried in StreamClose.code and GoAway.code (contracts §2).
const (
	// CloseNormal is an orderly close.
	CloseNormal uint32 = 0
	// CloseProtocolError reports a peer protocol violation: a frame that
	// exceeds credit or the size limit, an invalid stream id, or an
	// unspecified/unsupported stream kind.
	CloseProtocolError uint32 = 4400
	// CloseUnauthenticated is unauthenticated / authz_expired.
	CloseUnauthenticated uint32 = 4401
	// CloseForbidden is forbidden.
	CloseForbidden uint32 = 4403
	// CloseCancelled cancels a stream in the opening state (or reports a
	// cancelled RPC).
	CloseCancelled uint32 = 4499
	// CloseRelayRestart (relay_restart) refuses streams on a draining
	// session and closes streams still open at the drain deadline.
	CloseRelayRestart uint32 = 4503
	// CloseRelayTimeout (relay_timeout) is reserved for the Phase 2 PTY
	// contract.
	CloseRelayTimeout uint32 = 4504
)

// CloseError is a stream or session termination carrying a close code.
type CloseError struct {
	Code   uint32
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("conduit: closed with code %d", e.Code)
	}
	return fmt.Sprintf("conduit: closed with code %d: %s", e.Code, e.Reason)
}

// Reject returns an error that, returned from Admitter.Admit,
// Admitter.Refresh or StreamHandler.HandleStreamOpen, closes the session or
// stream with code.
func Reject(code uint32, reason string) error { return &CloseError{Code: code, Reason: reason} }

// CodeOf extracts the close code of err, or def if err carries none.
func CodeOf(err error, def uint32) uint32 {
	var ce *CloseError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return def
}

var (
	// ErrSessionClosed is returned by operations on a session that has
	// ended (Close, transport loss, keepalive timeout, protocol error).
	ErrSessionClosed = errors.New("conduit: session closed")
	// ErrDraining is returned by OpenStream on a session that has sent or
	// received GoAway. Open the stream on the replacement session.
	ErrDraining = errors.New("conduit: session draining")
	// ErrKeepaliveTimeout ends a session whose peer has been silent for
	// longer than the pong wait.
	ErrKeepaliveTimeout = errors.New("conduit: keepalive timeout")
	// ErrWriteTimeout ends a session whose transport write did not
	// complete within the write wait.
	ErrWriteTimeout = errors.New("conduit: write timeout")
	// ErrStreamCancelled is returned by PendingStream.Accept when the
	// stream left the opening state (opener cancel, open timeout, session
	// loss) before the accept.
	ErrStreamCancelled = errors.New("conduit: stream cancelled during opening")
	// ErrStreamClosed is returned by Read/Write after the local side
	// closed the stream.
	ErrStreamClosed = errors.New("conduit: stream closed")
	// ErrBufferBudget ends a session whose aggregate buffer budget was
	// overrun by protocol-internal frames (the writer is stuck).
	ErrBufferBudget = errors.New("conduit: aggregate buffer budget exceeded")
)

// Session is one conduit session as seen by consumers (router, relay,
// target). A remote (relay-to-relay) session implements the same
// interface in pkg/conduit/relay.
type Session interface {
	// Call sends an HTTP-shaped request and waits for the response.
	// Bodies above the RPC limit (768 KiB) are answered locally with
	// RpcResponse{status:413} so callers fall back to direct HTTP.
	// Cancelling ctx sends RpcCancel.
	Call(ctx context.Context, req *conduitv1.RpcRequest) (*conduitv1.RpcResponse, error)
	// OpenStream proposes a stream and waits for the peer to accept it.
	// The session assigns stream_id (odd on the dialer, even on the
	// relay); a zero initial_window or open_timeout_ms takes the session
	// default. Cancelling ctx or reaching open_timeout_ms during opening
	// sends StreamClose{4499}.
	OpenStream(ctx context.Context, open *conduitv1.StreamOpen) (Stream, error)
	// Info describes the session.
	Info() SessionInfo
	// Close ends the session immediately; open streams fail.
	Close() error
}

// Stream is a flow-controlled bidirectional byte stream.
//
// Read returns io.EOF after the peer half-closed (fin) or closed with
// CloseNormal, and a *CloseError for any other close code. Close is
// CloseWithCode(CloseNormal, ""). Streams from this package also implement
// CloseWrite() error (half-close, like *net.TCPConn) and
// State() StreamState.
type Stream interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	// CloseWithCode closes the stream. Data already written is flushed
	// before the StreamClose (the stream is draining meanwhile).
	CloseWithCode(code uint32, reason string) error
	ID() uint32
}

// WindowSize is a terminal size carried by StreamResize.
type WindowSize struct{ Cols, Rows uint16 }

// Resizable is implemented by every Stream of this package. Resizes
// delivers the peer's StreamResize frames (values above 65535 are
// clamped). Delivery never blocks the session: if the consumer falls
// behind, only the latest size is kept. The channel is closed when the
// stream ends or is closed locally, so it can be ranged over.
type Resizable interface {
	Resizes() <-chan WindowSize
}

// SessionInfo describes a session. Field values use the canonical strings
// of contracts §2.
type SessionInfo struct {
	SessionID, RelayInstanceID, Transport, PrincipalKind, PrincipalID, EndpointIncarnation, ExecScope string

	ConnectionEpoch int64
	Capabilities    *conduitv1.Capabilities
	ConnectedAt     time.Time
	Draining        bool
}

// LocalSession is implemented by every session returned by Accept and
// Dial. It exposes lifecycle, drain and test-seam operations that a
// remote session does not have.
type LocalSession interface {
	Session
	// Done is closed when the session has ended.
	Done() <-chan struct{}
	// Err returns why the session ended (nil while it is live).
	Err() error
	// GoAway starts a planned drain (§3.3): new streams from the peer are
	// refused with 4503, OpenStream returns ErrDraining, accepted streams
	// may finish until the drain deadline and are then closed with 4503,
	// after which the session closes.
	GoAway(opts GoAwayOptions) error
	// GoAwayReceived is closed when the peer sent GoAway; the dialer
	// should open a replacement session at once.
	GoAwayReceived() <-chan struct{}
	// CloseWithCode ends the session at once, without a drain: the peer
	// receives GoAway{code, reason} (no drain deadline), then the
	// transport is closed. Every stream and pending call fails with the
	// code (e.g. 4401 authz expired, 4403 forbidden). It returns
	// ErrSessionClosed if the session already ended; Done reports when
	// the close completed.
	CloseWithCode(code uint32, reason string) error
	// RefreshAuth sends AuthRefresh{credential, stream_id} (dialer side).
	// The receiving side validates refreshes one at a time; if several
	// arrive while one is being validated, only the latest is kept.
	RefreshAuth(credential []byte, streamID uint32) error
	// Stats returns scheduler and buffer counters.
	Stats() Stats
}

// GoAwayOptions parameterises a planned drain.
type GoAwayOptions struct {
	Code   uint32 // default CloseRelayRestart
	Reason string
	// DrainDeadline bounds how long accepted streams may continue
	// (default Config.DrainDeadline, 30s).
	DrainDeadline time.Duration
	// ReconnectAfter hints the dialer how long to wait before dialing a
	// replacement (default 0: immediately).
	ReconnectAfter time.Duration
}

// Admitter is the relay-side hook (1d): it authenticates the principal,
// allocates the connection epoch, inserts the registry row and returns the
// Welcome. Returning an error rejects the session: the code of a
// *CloseError (see Reject) is sent to the dialer, any other error is sent
// as 4403.
type Admitter interface {
	Admit(ctx context.Context, hello *conduitv1.Hello) (*conduitv1.Welcome, error)
	// Refresh re-validates an in-band credential. An error closes the
	// session with 4401 (or the *CloseError code).
	Refresh(ctx context.Context, ar *conduitv1.AuthRefresh) error
}

// PendingStream is an inbound stream in the opening state.
type PendingStream interface {
	ID() uint32
	// Accept moves the stream to accepted/active and sends StreamAccept.
	// It returns ErrStreamCancelled (and sends StreamClose) if the stream
	// has left the opening state.
	Accept() (Stream, error)
	// Reject refuses the stream with code.
	Reject(code uint32, reason string) error
}

// StreamHandler handles inbound StreamOpen frames (target side, 1e).
//
// ctx is cancelled when the opener cancels or the open timeout fires
// during opening, when the stream later ends, or when the session ends;
// the handler must then tear down any preparation (spawned pty, dialed
// socket). The handler may decide synchronously or keep the PendingStream
// and decide later; returning an error without deciding rejects with the
// error's code (default 4403).
type StreamHandler interface {
	HandleStreamOpen(ctx context.Context, open *conduitv1.StreamOpen, ps PendingStream) error
}

// StreamHandlerFunc adapts a function to StreamHandler.
type StreamHandlerFunc func(ctx context.Context, open *conduitv1.StreamOpen, ps PendingStream) error

// HandleStreamOpen implements StreamHandler.
func (f StreamHandlerFunc) HandleStreamOpen(ctx context.Context, open *conduitv1.StreamOpen, ps PendingStream) error {
	return f(ctx, open, ps)
}

// RPCHandler serves inbound RpcRequest frames. ctx is cancelled on
// RpcCancel or session loss.
type RPCHandler interface {
	HandleRPC(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse
}

// RPCHandlerFunc adapts a function to RPCHandler.
type RPCHandlerFunc func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse

// HandleRPC implements RPCHandler.
func (f RPCHandlerFunc) HandleRPC(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
	return f(ctx, req)
}
