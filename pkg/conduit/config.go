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
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Protocol defaults (§3.3, §3.5).
const (
	// DefaultStreamWindow is the per-stream receive window.
	DefaultStreamWindow = 256 * 1024
	// SessionWindow is the per-session window both peers assume at
	// session start. A peer may grow its receive window beyond it with
	// StreamWindow{stream_id:0} but never shrink it.
	SessionWindow = 4 * 1024 * 1024
	// MaxWindow bounds any credit total (as in HTTP/2).
	MaxWindow = 1<<31 - 1
	// MaxDataFrame is the maximum StreamData payload.
	MaxDataFrame = 64 * 1024
	// MaxRPCBody is the largest RPC body sent over a session; larger
	// requests (and handler responses) are answered with status 413.
	MaxRPCBody = 768 * 1024
	// DefaultBufferBudget is the aggregate per-session outbound buffer
	// budget.
	DefaultBufferBudget = 16 * 1024 * 1024
	// DefaultPingInterval, DefaultPongWait and DefaultWriteWait are the
	// keepalive timings, applied in both directions.
	DefaultPingInterval = 30 * time.Second
	DefaultPongWait     = 60 * time.Second
	DefaultWriteWait    = 10 * time.Second
	// DefaultHandshakeTimeout bounds Hello → Welcome.
	DefaultHandshakeTimeout = 10 * time.Second
	// DefaultOpenTimeout bounds the opening state when StreamOpen does not
	// set open_timeout_ms; MaxOpenTimeout caps any requested value.
	DefaultOpenTimeout = 15 * time.Second
	MaxOpenTimeout     = 60 * time.Second
	// DefaultDrainDeadline bounds how long accepted streams and in-flight
	// RPCs may continue after GoAway.
	DefaultDrainDeadline = 30 * time.Second
	// DefaultMaxConcurrentStreams bounds the streams the peer may have
	// open (or opening) on a session at once; excess StreamOpens are
	// refused with 4400. Together with the per-stream window it bounds
	// the receive memory of a session (see Config.MaxConcurrentStreams).
	DefaultMaxConcurrentStreams = 128
	// DefaultMaxConcurrentRPCs bounds the peer's in-flight RPCs on a
	// session; excess requests are answered with status 429.
	DefaultMaxConcurrentRPCs = 128
	// DefaultMaxRPCFrame bounds an encoded RpcRequest/RpcResponse frame
	// (body, headers, path and query together). It stays below the 1 MiB
	// message limit of the ws transport so an oversized RPC is answered
	// with 413 instead of failing the transport.
	DefaultMaxRPCFrame = 1000 * 1024
	// DefaultRecvBufferLimit is how much received-but-unread stream data
	// a session credits back to the peer on receipt (64 full default
	// stream windows); see Config.RecvBufferLimit.
	DefaultRecvBufferLimit = 16 * 1024 * 1024
)

// Config tunes a session. The zero value is valid and uses the defaults.
type Config struct {
	// Clock drives every timer (default clock.Real()).
	Clock clock.Clock
	// StreamWindow is this side's per-stream receive window (default
	// 256 KiB).
	StreamWindow uint32
	// SessionWindow is this side's session receive window; values above
	// the 4 MiB protocol baseline are advertised after the handshake.
	SessionWindow uint32
	// BufferBudget is the aggregate outbound buffer budget (default
	// 16 MiB).
	BufferBudget int64
	// PingInterval, PongWait and WriteWait are the keepalive timings. On
	// the dialer a nonzero Welcome.ping_interval_ms overrides
	// PingInterval.
	PingInterval time.Duration
	PongWait     time.Duration
	WriteWait    time.Duration
	// HandshakeTimeout bounds the Hello/Welcome exchange.
	HandshakeTimeout time.Duration
	// DrainDeadline is the default GoAway drain deadline.
	DrainDeadline time.Duration
	// RecvBufferLimit (default 16 MiB) decides when session credit goes
	// back to the peer. While the stream data received but not yet read
	// stays within RecvBufferLimit, session credit is returned on
	// receipt, so a stalled reader holds only its own stream window and
	// never the session window: slow readers stall only their own
	// streams, however many there are, up to RecvBufferLimit/StreamWindow
	// fully stalled streams. Beyond it, credit returns as the application
	// reads (classic HTTP/2 behaviour), which bounds the receive memory of
	// a session at RecvBufferLimit + SessionWindow.
	RecvBufferLimit int64
	// MaxConcurrentStreams bounds peer-opened streams (default 128).
	MaxConcurrentStreams int
	// MaxConcurrentRPCs bounds the peer's in-flight RPCs (default 128).
	MaxConcurrentRPCs int
	// MaxRPCFrame bounds an encoded RPC frame (default 1000 KiB, below
	// the transport message limit). Larger requests and handler
	// responses are answered with 413 locally.
	MaxRPCFrame int

	// StreamHandler serves inbound streams. Nil refuses them with 4400.
	StreamHandler StreamHandler
	// RPCHandler serves inbound RPCs. Nil answers 501.
	RPCHandler RPCHandler

	// Interceptor, if set, sees every frame (test seam for fault
	// injection; see Interceptor).
	Interceptor Interceptor
	// Logger receives diagnostics (default slog.Default()).
	Logger *slog.Logger

	// admitWG, if set, tracks the goroutines Accept leaves behind for
	// discarded admissions (waiting for a late Admit result, calling
	// AdmitAbandoner), so that tests can wait for them instead of
	// sleeping.
	admitWG *sync.WaitGroup
}

func (c Config) withDefaults() Config {
	if c.Clock == nil {
		c.Clock = clock.Real()
	}
	if c.StreamWindow == 0 {
		c.StreamWindow = DefaultStreamWindow
	}
	if c.SessionWindow < SessionWindow {
		c.SessionWindow = SessionWindow
	}
	if c.BufferBudget <= 0 {
		c.BufferBudget = DefaultBufferBudget
	}
	if c.PingInterval <= 0 {
		c.PingInterval = DefaultPingInterval
	}
	if c.PongWait <= 0 {
		c.PongWait = DefaultPongWait
	}
	if c.PingInterval >= c.PongWait {
		c.PingInterval = c.PongWait / 2
	}
	if c.WriteWait <= 0 {
		c.WriteWait = DefaultWriteWait
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if c.DrainDeadline <= 0 {
		c.DrainDeadline = DefaultDrainDeadline
	}
	if c.RecvBufferLimit <= 0 {
		c.RecvBufferLimit = DefaultRecvBufferLimit
	}
	if c.MaxConcurrentStreams <= 0 {
		c.MaxConcurrentStreams = DefaultMaxConcurrentStreams
	}
	if c.MaxConcurrentRPCs <= 0 {
		c.MaxConcurrentRPCs = DefaultMaxConcurrentRPCs
	}
	if c.MaxRPCFrame <= 0 {
		c.MaxRPCFrame = DefaultMaxRPCFrame
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Direction tells an Interceptor which way a frame travels.
type Direction int

// Directions.
const (
	Outbound Direction = iota
	Inbound
)

func (d Direction) String() string {
	if d == Outbound {
		return "outbound"
	}
	return "inbound"
}

// Interceptor is the fault-injection seam. It is called for every frame,
// outbound just before encoding (after scheduling) and inbound just after
// decoding (before dispatch), and returns the frames to process instead:
// nil drops the frame, []*Frame{f, f} duplicates it, and blocking inside
// the call delays it (and every frame behind it, as on a real wire).
// Dropping StreamData or StreamWindow frames desynchronises flow-control
// accounting by design; tests that do so are probing exactly that.
type Interceptor func(dir Direction, f *conduitv1.Frame) []*conduitv1.Frame

// FrameType returns a short name for the frame body ("stream_data",
// "ping", ...), convenient for interceptor predicates.
func FrameType(f *conduitv1.Frame) string {
	switch f.GetBody().(type) {
	case *conduitv1.Frame_Hello:
		return "hello"
	case *conduitv1.Frame_Welcome:
		return "welcome"
	case *conduitv1.Frame_GoAway:
		return "go_away"
	case *conduitv1.Frame_AuthRefresh:
		return "auth_refresh"
	case *conduitv1.Frame_Ping:
		return "ping"
	case *conduitv1.Frame_Pong:
		return "pong"
	case *conduitv1.Frame_RpcRequest:
		return "rpc_request"
	case *conduitv1.Frame_RpcResponse:
		return "rpc_response"
	case *conduitv1.Frame_RpcCancel:
		return "rpc_cancel"
	case *conduitv1.Frame_StreamOpen:
		return "stream_open"
	case *conduitv1.Frame_StreamAccept:
		return "stream_accept"
	case *conduitv1.Frame_StreamData:
		return "stream_data"
	case *conduitv1.Frame_StreamWindow:
		return "stream_window"
	case *conduitv1.Frame_StreamClose:
		return "stream_close"
	case *conduitv1.Frame_StreamResize:
		return "stream_resize"
	}
	return "unknown"
}

// DropFrames returns an Interceptor that drops frames of the given types
// travelling in dir while active() reports true (nil active = always).
func DropFrames(dir Direction, active func() bool, types ...string) Interceptor {
	set := map[string]bool{}
	for _, t := range types {
		set[t] = true
	}
	return func(d Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if d == dir && set[FrameType(f)] && (active == nil || active()) {
			return nil
		}
		return []*conduitv1.Frame{f}
	}
}
