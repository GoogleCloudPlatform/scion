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
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// session is the multiplexer behind every LocalSession.
type session struct {
	cfg      Config
	clk      clock.Clock
	conn     transport.Conn
	isDialer bool
	adm      Admitter
	sched    *scheduler

	ctx    context.Context // cancelled when the session ends
	cancel context.CancelFunc

	done     chan struct{}
	failOnce sync.Once
	errMu    sync.Mutex
	err      error

	mu           sync.Mutex
	info         SessionInfo
	streams      map[uint32]*stream
	nextID       uint32
	lastPeerID   uint32
	draining     bool
	goAwaySent   bool
	goAwayCode   uint32 // code of the GoAway we sent
	closing      bool   // a close-after-flush sentinel is queued
	peerStreams  int    // peer-opened streams in the table
	remoteGoAway *conduitv1.GoAway
	goAwayRecv   chan struct{}
	pendingRPC   map[string]chan *conduitv1.RpcResponse
	inboundRPC   map[string]context.CancelFunc
	pingTimer    clock.Timer
	watchdog     clock.Timer
	drainTimer   clock.Timer
	refreshing   bool                   // an AuthRefresh is being validated
	refreshNext  *conduitv1.AuthRefresh // latest refresh queued behind it

	rpcSeq   atomic.Uint64
	pingSeq  atomic.Uint64
	lastRecv atomic.Int64 // clock UnixNano of the last inbound frame

	// Flow control (session level).
	fcMu          sync.Mutex
	fcCond        *sync.Cond
	sendSession   int64 // credit the peer granted us
	recvSession   int64 // credit we granted the peer, not yet used
	pendingSess   int64 // credit due to the peer, batched
	recvBufFC     int64 // received stream data not yet read or discarded
	owedSess      int64 // credit withheld for buffered data (RecvBufferLimit)
	recvBuffered  atomic.Int64
	peakRecvBufrd atomic.Int64
}

var _ LocalSession = (*session)(nil)

func newSession(cfg Config, conn transport.Conn, isDialer bool) *session {
	s := &session{
		cfg:         cfg,
		clk:         cfg.Clock,
		conn:        conn,
		isDialer:    isDialer,
		sched:       newScheduler(cfg.BufferBudget),
		done:        make(chan struct{}),
		streams:     map[uint32]*stream{},
		goAwayRecv:  make(chan struct{}),
		pendingRPC:  map[string]chan *conduitv1.RpcResponse{},
		inboundRPC:  map[string]context.CancelFunc{},
		sendSession: SessionWindow,
		recvSession: SessionWindow,
	}
	s.fcCond = sync.NewCond(&s.fcMu)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if isDialer {
		s.nextID = 1
	} else {
		s.nextID = 2
	}
	s.info.Transport = conn.Transport()
	return s
}

// ---------------------------------------------------------------- handshake

// Dial opens a session to the relay: it dials the transport, sends hello
// and waits for Welcome. A GoAway in place of Welcome (admission refused)
// is returned as a *CloseError carrying 4401/4403.
func Dial(ctx context.Context, d transport.Dialer, cfg Config, hello *conduitv1.Hello) (Session, *conduitv1.Welcome, error) {
	cfg = cfg.withDefaults()
	if _, err := PrincipalKindFromProto(hello.GetPrincipalKind()); err != nil {
		return nil, nil, err
	}
	conn, err := d.Dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	s := newSession(cfg, conn, true)
	if err := s.writeDirect(&conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: hello}}); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	f, err := s.readDirect(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	var w *conduitv1.Welcome
	switch b := f.GetBody().(type) {
	case *conduitv1.Frame_Welcome:
		w = b.Welcome
	case *conduitv1.Frame_GoAway:
		_ = conn.Close()
		return nil, nil, &CloseError{Code: b.GoAway.GetCode(), Reason: b.GoAway.GetReason()}
	default:
		_ = conn.Close()
		return nil, nil, fmt.Errorf("conduit: expected welcome, got %s", FrameType(f))
	}
	if ms := w.GetPingIntervalMs(); ms > 0 {
		if pi := time.Duration(ms) * time.Millisecond; pi < s.cfg.PongWait {
			s.cfg.PingInterval = pi
		}
	}
	s.setInfo(hello, w)
	s.start()
	return s, w, nil
}

// Accept runs the relay side of the handshake on conn: it reads Hello,
// asks adm to admit the principal and replies with the Welcome adm
// returns (filling ping_interval_ms and max_frame if unset). On rejection
// the dialer receives GoAway{code} and conn is closed.
func Accept(ctx context.Context, conn transport.Conn, cfg Config, adm Admitter) (Session, error) {
	if adm == nil {
		_ = conn.Close()
		return nil, errors.New("conduit: Accept requires an Admitter")
	}
	cfg = cfg.withDefaults()
	s := newSession(cfg, conn, false)
	s.adm = adm
	f, err := s.readDirect(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	hello := f.GetHello()
	if hello == nil {
		s.rejectHandshake(CloseProtocolError, "expected hello")
		return nil, &CloseError{Code: CloseProtocolError, Reason: "expected hello, got " + FrameType(f)}
	}
	if _, err := PrincipalKindFromProto(hello.GetPrincipalKind()); err != nil {
		s.rejectHandshake(CloseProtocolError, err.Error())
		return nil, &CloseError{Code: CloseProtocolError, Reason: err.Error()}
	}
	w, err := adm.Admit(ctx, hello)
	if err == nil && w == nil {
		err = errors.New("conduit: admitter returned no welcome")
	}
	if err != nil {
		code := CodeOf(err, CloseForbidden)
		s.rejectHandshake(code, rejectReason(err))
		return nil, err
	}
	w = proto.Clone(w).(*conduitv1.Welcome)
	if w.PingIntervalMs == 0 {
		w.PingIntervalMs = uint32(cfg.PingInterval / time.Millisecond)
	}
	if w.MaxFrame == 0 {
		w.MaxFrame = MaxDataFrame
	}
	if err := s.writeDirect(&conduitv1.Frame{Body: &conduitv1.Frame_Welcome{Welcome: w}}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	s.setInfo(hello, w)
	s.start()
	return s, nil
}

func rejectReason(err error) string {
	var ce *CloseError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return "admission refused"
}

func (s *session) rejectHandshake(code uint32, reason string) {
	_ = s.writeDirect(&conduitv1.Frame{Body: &conduitv1.Frame_GoAway{GoAway: &conduitv1.GoAway{Code: code, Reason: reason}}})
	_ = s.conn.Close()
}

func (s *session) setInfo(h *conduitv1.Hello, w *conduitv1.Welcome) {
	pk, _ := PrincipalKindFromProto(h.GetPrincipalKind())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info.SessionID = w.GetSessionId()
	s.info.RelayInstanceID = w.GetRelayInstanceId()
	s.info.ConnectionEpoch = w.GetConnectionEpoch()
	s.info.PrincipalKind = string(pk)
	s.info.PrincipalID = h.GetPrincipalId()
	s.info.Capabilities = h.GetCapabilities()
	s.info.EndpointIncarnation = h.GetCapabilities().GetEndpointIncarnation()
	s.info.ExecScope = h.GetCapabilities().GetExecScope()
	s.info.ConnectedAt = s.clk.Now()
}

// writeDirect writes a handshake frame before the writer goroutine runs.
func (s *session) writeDirect(f *conduitv1.Frame) error {
	for _, g := range s.intercept(Outbound, f) {
		b, err := proto.Marshal(g)
		if err != nil {
			return err
		}
		if err := s.conn.WriteFrame(b); err != nil {
			return err
		}
	}
	return nil
}

// readDirect reads the first handshake frame, bounded by ctx and the
// handshake timeout.
func (s *session) readDirect(ctx context.Context) (*conduitv1.Frame, error) {
	type result struct {
		f   *conduitv1.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			b, err := s.conn.ReadFrame()
			if err != nil {
				ch <- result{err: err}
				return
			}
			f := &conduitv1.Frame{}
			if err := proto.Unmarshal(b, f); err != nil {
				ch <- result{err: fmt.Errorf("conduit: decode handshake frame: %w", err)}
				return
			}
			if fs := s.intercept(Inbound, f); len(fs) > 0 {
				ch <- result{f: fs[0]}
				return
			}
		}
	}()
	timeout, stop := clock.After(s.clk, s.cfg.HandshakeTimeout)
	defer stop()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-timeout:
		_ = s.conn.Close()
		return nil, errors.New("conduit: handshake timeout")
	case <-ctx.Done():
		_ = s.conn.Close()
		return nil, ctx.Err()
	}
}

func (s *session) intercept(dir Direction, f *conduitv1.Frame) []*conduitv1.Frame {
	if s.cfg.Interceptor == nil {
		return []*conduitv1.Frame{f}
	}
	return s.cfg.Interceptor(dir, f)
}

// start launches the reader, writer and keepalive.
func (s *session) start() {
	s.lastRecv.Store(s.clk.Now().UnixNano())
	if extra := int64(s.cfg.SessionWindow) - SessionWindow; extra > 0 {
		s.fcMu.Lock()
		s.recvSession += extra
		s.fcMu.Unlock()
		s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamWindow{StreamWindow: &conduitv1.StreamWindow{Increment: uint32(extra)}}})
	}
	go s.readLoop()
	go s.writeLoop()
	s.armPing()
	s.armWatchdog(s.cfg.PongWait)
}

// ---------------------------------------------------------------- lifecycle

func (s *session) Done() <-chan struct{}           { return s.done }
func (s *session) GoAwayReceived() <-chan struct{} { return s.goAwayRecv }

func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.info
	info.Draining = s.draining
	return info
}

func (s *session) Close() error {
	s.fail(ErrSessionClosed)
	return nil
}

// CloseWithCode implements LocalSession.
func (s *session) CloseWithCode(code uint32, reason string) error {
	if s.isDone() {
		return ErrSessionClosed
	}
	s.closeWithCode(code, reason)
	return nil
}

// endErr is the error streams and pending calls see once the session
// ended: ErrSessionClosed, carrying the session's reason so CodeOf sees
// e.g. the 4503 of a drained session.
func (s *session) endErr() error {
	err := s.Err()
	if err == nil || err == ErrSessionClosed {
		return ErrSessionClosed
	}
	return fmt.Errorf("%w: %w", ErrSessionClosed, err)
}

// fail ends the session once: every stream and pending call fails, every
// handler context is cancelled, and then the transport is closed (closing
// may block briefly on a stuck writer, so it comes last).
func (s *session) fail(err error) {
	s.failOnce.Do(func() {
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.done)
		s.cancel()
		s.sched.close()

		s.mu.Lock()
		streams := make([]*stream, 0, len(s.streams))
		for _, st := range s.streams {
			streams = append(streams, st)
		}
		s.streams = map[uint32]*stream{}
		timers := []clock.Timer{s.pingTimer, s.watchdog, s.drainTimer}
		sessionID := s.info.SessionID
		s.mu.Unlock()
		for _, t := range timers {
			if t != nil {
				t.Stop()
			}
		}
		streamErr := s.endErr()
		for _, st := range streams {
			st.sessionFailed(streamErr)
		}
		s.wakeCreditWaiters()
		_ = s.conn.Close()
		if err != ErrSessionClosed {
			s.cfg.Logger.Debug("conduit session ended", "session_id", sessionID, "error", err)
		}
	})
}

func (s *session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// setTimer arms a session-scoped timer in slot (replacing and stopping
// any previous one). Timers in slots are stopped when the session ends,
// and callbacks never run after it ended.
func (s *session) setTimer(slot *clock.Timer, d time.Duration, f func()) {
	t := s.clk.AfterFunc(d, func() {
		if !s.isDone() {
			f()
		}
	})
	s.mu.Lock()
	if s.isDone() {
		s.mu.Unlock()
		t.Stop()
		return
	}
	prev := *slot
	*slot = t
	s.mu.Unlock()
	if prev != nil {
		prev.Stop()
	}
}

// closeAfterFlush ends the session with err once every frame queued so far
// has been written.
func (s *session) closeAfterFlush(err error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	s.mu.Unlock()
	if qerr := s.sched.enqueueInternal(&outFrame{class: classControl, sent: func() { s.fail(err) }}); qerr != nil {
		s.fail(err)
	}
}

// closeWithCode tells the peer why (GoAway{code}, no drain) and ends the
// session after the frame is flushed.
func (s *session) closeWithCode(code uint32, reason string) {
	s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_GoAway{GoAway: &conduitv1.GoAway{Code: code, Reason: reason}}})
	s.closeAfterFlush(&CloseError{Code: code, Reason: reason})
}

// protocolError ends the session for a peer protocol violation.
func (s *session) protocolError(format string, args ...any) {
	s.closeWithCode(CloseProtocolError, fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------- keepalive

func (s *session) armPing() {
	s.setTimer(&s.pingTimer, s.cfg.PingInterval, func() {
		s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_Ping{Ping: &conduitv1.Ping{Nonce: s.pingSeq.Add(1)}}})
		s.armPing()
	})
}

// armWatchdog fails the session when nothing has been received for
// PongWait. Every inbound frame (not only Pong) proves liveness.
func (s *session) armWatchdog(d time.Duration) {
	s.setTimer(&s.watchdog, d, func() {
		silent := s.clk.Now().Sub(time.Unix(0, s.lastRecv.Load()))
		if silent >= s.cfg.PongWait {
			s.fail(ErrKeepaliveTimeout)
			return
		}
		s.armWatchdog(s.cfg.PongWait - silent)
	})
}

// ---------------------------------------------------------------- writer

func (s *session) sendInternal(f *conduitv1.Frame) {
	if err := s.sched.enqueueInternal(&outFrame{f: f, class: classControl, size: int64(proto.Size(f))}); err != nil && err != ErrSessionClosed {
		s.fail(err)
	}
}

func (s *session) sendClose(id, code uint32, reason string) {
	s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamClose{StreamClose: &conduitv1.StreamClose{StreamId: id, Code: code, Reason: reason}}})
}

// sendControl queues a caller-originated control frame, blocking on the
// aggregate budget.
func (s *session) sendControl(ctx context.Context, f *conduitv1.Frame) error {
	return s.sched.enqueue(&outFrame{f: f, class: classControl, size: int64(proto.Size(f))}, ctx.Done(), ctx.Err)
}

func (s *session) writeLoop() {
	for {
		of, ok := s.sched.next()
		if !ok {
			return
		}
		if of.f != nil {
			for _, f := range s.intercept(Outbound, of.f) {
				b, err := proto.Marshal(f)
				if err != nil {
					s.fail(fmt.Errorf("conduit: encode frame: %w", err))
					return
				}
				t := s.clk.AfterFunc(s.cfg.WriteWait, func() { s.fail(ErrWriteTimeout) })
				err = s.conn.WriteFrame(b)
				t.Stop()
				if err != nil {
					s.fail(s.transportErr("write", err))
					return
				}
				if of.class == classData && f.GetStreamData() != nil {
					s.sched.dataSent.Add(1)
				} else {
					s.sched.controlSent.Add(1)
				}
			}
		}
		if of.sent != nil {
			of.sent()
		}
	}
}

// transportErr is the session error for a failed transport read or write.
// If the peer announced why it is going away (handshake reject, protocol
// error, end of a drain), that reason wins over the bare transport error.
func (s *session) transportErr(op string, err error) error {
	s.mu.Lock()
	ga := s.remoteGoAway
	s.mu.Unlock()
	if ga != nil && ga.GetCode() != 0 {
		return &CloseError{Code: ga.GetCode(), Reason: ga.GetReason()}
	}
	return fmt.Errorf("conduit: transport %s: %w", op, err)
}

// ---------------------------------------------------------------- reader

func (s *session) readLoop() {
	for {
		b, err := s.conn.ReadFrame()
		if err != nil {
			s.fail(s.transportErr("read", err))
			return
		}
		s.lastRecv.Store(s.clk.Now().UnixNano())
		f := &conduitv1.Frame{}
		if err := proto.Unmarshal(b, f); err != nil {
			s.protocolError("undecodable frame: %v", err)
			return
		}
		for _, g := range s.intercept(Inbound, f) {
			s.dispatch(g)
		}
	}
}

func (s *session) dispatch(f *conduitv1.Frame) {
	switch b := f.GetBody().(type) {
	case *conduitv1.Frame_Ping:
		s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_Pong{Pong: &conduitv1.Pong{Nonce: b.Ping.GetNonce()}}})
	case *conduitv1.Frame_Pong:
		// Liveness was recorded on receipt.
	case *conduitv1.Frame_StreamData:
		s.handleData(b.StreamData)
	case *conduitv1.Frame_StreamWindow:
		s.handleWindow(b.StreamWindow)
	case *conduitv1.Frame_StreamOpen:
		s.handleOpen(b.StreamOpen)
	case *conduitv1.Frame_StreamAccept:
		s.handleAccept(b.StreamAccept)
	case *conduitv1.Frame_StreamClose:
		if st := s.lookup(b.StreamClose.GetStreamId()); st != nil {
			st.remoteClose(b.StreamClose.GetCode(), b.StreamClose.GetReason())
		}
	case *conduitv1.Frame_StreamResize:
		if st := s.lookup(b.StreamResize.GetStreamId()); st != nil {
			st.deliverResize(b.StreamResize.GetCols(), b.StreamResize.GetRows())
		}
	case *conduitv1.Frame_RpcRequest:
		s.handleRPCRequest(b.RpcRequest)
	case *conduitv1.Frame_RpcResponse:
		s.handleRPCResponse(b.RpcResponse)
	case *conduitv1.Frame_RpcCancel:
		s.mu.Lock()
		cancel := s.inboundRPC[b.RpcCancel.GetRequestId()]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case *conduitv1.Frame_GoAway:
		s.handleGoAway(b.GoAway)
	case *conduitv1.Frame_AuthRefresh:
		s.handleAuthRefresh(b.AuthRefresh)
	case *conduitv1.Frame_Hello, *conduitv1.Frame_Welcome:
		s.protocolError("unexpected %s after handshake", FrameType(f))
	default:
		// Unknown frame types are ignored for forward compatibility.
	}
}

func (s *session) lookup(id uint32) *stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *session) ownsID(id uint32) bool { return (id%2 == 1) == s.isDialer }

func (s *session) removeStream(st *stream) {
	s.mu.Lock()
	if cur, ok := s.streams[st.id]; ok && cur == st {
		delete(s.streams, st.id)
		if !st.opener {
			s.peerStreams--
		}
	}
	s.mu.Unlock()
	s.closeIfDrained()
}

// drainedLocked reports whether a drain we started (GoAway) has nothing
// left to wait for: no stream and no RPC in flight in either direction.
func (s *session) drainedLocked() bool {
	return s.goAwaySent && len(s.streams) == 0 && len(s.pendingRPC) == 0 && len(s.inboundRPC) == 0
}

// closeIfDrained ends a draining session once it has drained. It is
// called whenever a stream or an RPC finishes.
func (s *session) closeIfDrained() {
	s.mu.Lock()
	drained := s.drainedLocked()
	code := s.goAwayCode
	s.mu.Unlock()
	if drained {
		s.closeAfterFlush(&CloseError{Code: code, Reason: "drained"})
	}
}

// ---------------------------------------------------------------- flow control

func (s *session) wakeCreditWaiters() {
	s.fcMu.Lock()
	s.fcCond.Broadcast()
	s.fcMu.Unlock()
}

// acquireSessionCredit takes up to n bytes of session send credit,
// blocking while none is available.
func (s *session) acquireSessionCredit(st *stream, n int64) (int64, error) {
	s.fcMu.Lock()
	defer s.fcMu.Unlock()
	for s.sendSession <= 0 {
		if s.isDone() {
			return 0, ErrSessionClosed
		}
		if st.writeDead.Load() {
			return 0, errStreamDead
		}
		s.fcCond.Wait()
	}
	if s.isDone() {
		return 0, ErrSessionClosed
	}
	got := min(n, s.sendSession)
	s.sendSession -= got
	return got, nil
}

// errStreamDead is an internal signal from acquireSessionCredit; Write
// replaces it with the stream's real error.
var errStreamDead = errors.New("conduit: stream not writable")

func (s *session) releaseSessionCredit(n int64) { s.restoreSessionCredit(n) }

// restoreSessionCredit gives back send credit for bytes that were never
// put on the wire.
func (s *session) restoreSessionCredit(n int64) {
	if n <= 0 {
		return
	}
	s.fcMu.Lock()
	s.sendSession += n
	s.fcCond.Broadcast()
	s.fcMu.Unlock()
}

func (s *session) addRecvBuffered(n int64) {
	v := s.recvBuffered.Add(n)
	for {
		p := s.peakRecvBufrd.Load()
		if v <= p || s.peakRecvBufrd.CompareAndSwap(p, v) {
			return
		}
	}
}

// consumed is called when buffered stream data was read or discarded.
func (s *session) consumed(n int64) {
	if n <= 0 {
		return
	}
	s.recvBuffered.Add(-n)
	s.adjustBuffered(-n)
}

// adjustBuffered tracks buffered stream data (delta > 0 on receipt,
// < 0 when read or discarded) and returns session credit for it: on
// receipt while the buffered data whose credit was already returned stays
// within RecvBufferLimit, otherwise as it is consumed.
func (s *session) adjustBuffered(delta int64) {
	s.fcMu.Lock()
	s.recvBufFC += delta
	if delta > 0 {
		s.owedSess += delta
	}
	var inc int64
	if room := s.cfg.RecvBufferLimit - (s.recvBufFC - s.owedSess); room > 0 && s.owedSess > 0 {
		r := min(room, s.owedSess)
		s.owedSess -= r
		inc = s.creditLocked(r)
	}
	s.fcMu.Unlock()
	s.sendSessionWindow(inc)
}

// returnSessionCredit returns credit for bytes that were not buffered
// (discarded, or for an unknown stream).
func (s *session) returnSessionCredit(n int64) {
	if n <= 0 {
		return
	}
	s.fcMu.Lock()
	inc := s.creditLocked(n)
	s.fcMu.Unlock()
	s.sendSessionWindow(inc)
}

// creditLocked adds n bytes of credit due to the peer and returns the
// increment to send now: updates are batched a quarter of the window at a
// time.
func (s *session) creditLocked(n int64) int64 {
	s.pendingSess += n
	if s.pendingSess < int64(s.cfg.SessionWindow)/4 {
		return 0
	}
	inc := s.pendingSess
	s.pendingSess = 0
	s.recvSession += inc
	return inc
}

func (s *session) sendSessionWindow(inc int64) {
	if inc > 0 {
		s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamWindow{StreamWindow: &conduitv1.StreamWindow{StreamId: 0, Increment: uint32(inc)}}})
	}
}

func (s *session) handleData(d *conduitv1.StreamData) {
	n := int64(len(d.GetData()))
	if n > MaxDataFrame {
		s.protocolError("stream %d: data frame of %d bytes exceeds %d", d.GetStreamId(), n, MaxDataFrame)
		return
	}
	s.fcMu.Lock()
	if n > s.recvSession {
		s.fcMu.Unlock()
		s.protocolError("session flow control: %d bytes exceed credit %d", n, s.recvSession)
		return
	}
	s.recvSession -= n
	s.fcMu.Unlock()

	// Buffered bytes are credited by adjustBuffered (on receipt, within
	// RecvBufferLimit, so a stalled reader holds only its own stream
	// window); bytes not buffered are credited back at once.
	st := s.lookup(d.GetStreamId())
	if st == nil {
		s.returnSessionCredit(n)
		return
	}
	ok, buffered := st.deliver(d)
	if !buffered {
		s.returnSessionCredit(n)
	}
	if !ok {
		st.abort(CloseProtocolError, "stream flow control: data exceeds credit", true)
	}
}

func (s *session) handleWindow(w *conduitv1.StreamWindow) {
	if w.GetStreamId() == 0 {
		s.fcMu.Lock()
		s.sendSession += int64(w.GetIncrement())
		over := s.sendSession > MaxWindow
		s.fcCond.Broadcast()
		s.fcMu.Unlock()
		if over {
			s.protocolError("session window overflow")
		}
		return
	}
	if st := s.lookup(w.GetStreamId()); st != nil && !st.grant(w.GetIncrement()) {
		st.abort(CloseProtocolError, "stream window overflow", true)
	}
}

// ---------------------------------------------------------------- streams

// OpenStream implements Session.
func (s *session) OpenStream(ctx context.Context, open *conduitv1.StreamOpen) (Stream, error) {
	if _, err := StreamKindFromProto(open.GetKind()); err != nil {
		return nil, err
	}
	open = proto.Clone(open).(*conduitv1.StreamOpen)
	if open.InitialWindow == 0 {
		open.InitialWindow = s.cfg.StreamWindow
	}
	timeout := openTimeout(open.GetOpenTimeoutMs())
	open.OpenTimeoutMs = uint32(timeout / time.Millisecond)

	s.mu.Lock()
	if s.isDone() {
		s.mu.Unlock()
		return nil, ErrSessionClosed
	}
	if s.draining {
		s.mu.Unlock()
		return nil, ErrDraining
	}
	id := s.nextID
	s.nextID += 2
	open.StreamId = id
	st := newStream(s, id, true, open.InitialWindow)
	s.streams[id] = st
	s.mu.Unlock()

	timedOut := make(chan struct{})
	st.mu.Lock()
	st.openTimer = s.clk.AfterFunc(timeout, func() { close(timedOut) })
	st.mu.Unlock()

	if err := s.sendControl(ctx, &conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: open}}); err != nil {
		st.abort(CloseCancelled, "open not sent", false)
		return nil, err
	}

	select {
	case <-st.opened:
		st.mu.Lock()
		err := st.openErr
		st.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return st, nil
	case <-ctx.Done():
		// If the accept raced the cancel and won, abort turns the
		// accepted stream into a cancelled one (StreamClose{4499}), so
		// the caller always sees the cancel and the target cleans up.
		st.abort(CloseCancelled, "cancelled", true)
		return nil, ctx.Err()
	case <-timedOut:
		st.abort(CloseCancelled, "open timeout", true)
		return nil, (fmt.Errorf("conduit: stream %d: open timeout after %v: %w", id, timeout, context.DeadlineExceeded))
	}
}

func openTimeout(ms uint32) time.Duration {
	if ms == 0 {
		return DefaultOpenTimeout
	}
	return min(time.Duration(ms)*time.Millisecond, MaxOpenTimeout)
}

func (s *session) handleAccept(a *conduitv1.StreamAccept) {
	id := a.GetStreamId()
	st := s.lookup(id)
	if st == nil || !st.opener {
		if s.ownsID(id) {
			// Late accept for a stream we cancelled: reject and let the
			// target clean up (T8).
			s.sendClose(id, CloseCancelled, "late accept")
		}
		return
	}
	st.mu.Lock()
	if st.state != StateOpening {
		send := !st.closeSent
		st.closeSent = true
		st.mu.Unlock()
		if send {
			s.sendClose(id, CloseCancelled, "late accept")
		}
		return
	}
	st.state = StateAccepted
	st.sendCredit = int64(a.GetInitialWindow())
	if st.openTimer != nil {
		st.openTimer.Stop()
	}
	st.state = StateActive
	close(st.opened)
	st.cond.Broadcast()
	st.mu.Unlock()
	s.markDrainingIfNeeded(st)
}

func (s *session) handleOpen(o *conduitv1.StreamOpen) {
	id := o.GetStreamId()
	s.mu.Lock()
	if id == 0 || s.ownsID(id) || id <= s.lastPeerID {
		s.mu.Unlock()
		s.protocolError("invalid peer stream id %d", id)
		return
	}
	s.lastPeerID = id
	// Admission and insertion happen in one critical section with the
	// draining check, so a concurrent GoAway either refuses this stream
	// or sees it in the table and waits for it.
	code, reason := uint32(0), ""
	h := s.cfg.StreamHandler
	switch _, kindErr := StreamKindFromProto(o.GetKind()); {
	case s.draining:
		code, reason = CloseRelayRestart, "session draining"
	case kindErr != nil:
		code, reason = CloseProtocolError, kindErr.Error()
	case h == nil:
		code, reason = CloseProtocolError, "streams not supported"
	case o.GetInitialWindow() > MaxWindow:
		code, reason = CloseProtocolError, "initial window overflow"
	case s.peerStreams >= s.cfg.MaxConcurrentStreams:
		code, reason = CloseProtocolError, "too many concurrent streams"
	}
	if code != 0 {
		s.mu.Unlock()
		s.sendClose(id, code, reason)
		return
	}
	st := newStream(s, id, false, s.cfg.StreamWindow)
	st.sendCredit = int64(o.GetInitialWindow())
	s.streams[id] = st
	s.peerStreams++
	s.mu.Unlock()

	st.mu.Lock()
	st.openTimer = s.clk.AfterFunc(openTimeout(o.GetOpenTimeoutMs()), func() {
		st.mu.Lock()
		opening := st.state == StateOpening
		st.mu.Unlock()
		if opening {
			st.abort(CloseCancelled, "open timeout", true)
		}
	})
	st.mu.Unlock()

	go func() {
		err := h.HandleStreamOpen(st.ctx, o, pendingStream{st: st})
		if err != nil {
			st.mu.Lock()
			opening := st.state == StateOpening
			st.mu.Unlock()
			if opening {
				st.abort(CodeOf(err, CloseForbidden), rejectReason(err), true)
			}
		}
	}()
}

// markDrainingIfNeeded puts a newly active stream into draining when the
// session is already draining.
func (s *session) markDrainingIfNeeded(st *stream) {
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if draining {
		st.mu.Lock()
		if st.state == StateActive {
			st.state = StateDraining
		}
		st.mu.Unlock()
	}
}

// ---------------------------------------------------------------- GoAway

// GoAway implements LocalSession.
func (s *session) GoAway(opts GoAwayOptions) error {
	if opts.Code == 0 {
		opts.Code = CloseRelayRestart
	}
	if opts.DrainDeadline <= 0 {
		opts.DrainDeadline = s.cfg.DrainDeadline
	}
	s.mu.Lock()
	if s.isDone() {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	if s.goAwaySent {
		s.mu.Unlock()
		return nil
	}
	s.goAwaySent = true
	s.goAwayCode = opts.Code
	s.draining = true
	last := s.lastPeerID
	streams := s.snapshotLocked()
	drained := s.drainedLocked()
	s.mu.Unlock()

	for _, st := range streams {
		st.mu.Lock()
		if st.state == StateActive || st.state == StateAccepted {
			st.state = StateDraining
		}
		st.mu.Unlock()
	}
	s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_GoAway{GoAway: &conduitv1.GoAway{
		LastStreamId:     last,
		Code:             opts.Code,
		Reason:           opts.Reason,
		ReconnectAfterMs: uint32(opts.ReconnectAfter / time.Millisecond),
		DrainDeadlineMs:  uint32(opts.DrainDeadline / time.Millisecond),
	}}})
	if drained {
		s.closeAfterFlush(&CloseError{Code: opts.Code, Reason: "drained"})
		return nil
	}
	code, reason := opts.Code, opts.Reason
	s.setTimer(&s.drainTimer, opts.DrainDeadline, func() { s.drainDeadline(code, reason) })
	return nil
}

func (s *session) snapshotLocked() []*stream {
	out := make([]*stream, 0, len(s.streams))
	for _, st := range s.streams {
		out = append(out, st)
	}
	return out
}

// drainDeadline closes every remaining stream with code (4503), cancels
// the inbound RPC handlers still running and ends the session; calls
// still waiting for the peer fail with the session's code.
func (s *session) drainDeadline(code uint32, reason string) {
	s.mu.Lock()
	streams := s.snapshotLocked()
	cancels := make([]context.CancelFunc, 0, len(s.inboundRPC))
	for _, c := range s.inboundRPC {
		cancels = append(cancels, c)
	}
	s.mu.Unlock()
	if reason == "" {
		reason = "drain deadline"
	}
	for _, c := range cancels {
		c()
	}
	for _, st := range streams {
		st.abort(code, reason, true)
	}
	s.closeAfterFlush(&CloseError{Code: code, Reason: reason})
}

func (s *session) handleGoAway(g *conduitv1.GoAway) {
	s.mu.Lock()
	first := s.remoteGoAway == nil
	s.remoteGoAway = g
	s.draining = true
	var refused []*stream
	for id, st := range s.streams {
		if s.ownsID(id) && id > g.GetLastStreamId() {
			refused = append(refused, st)
		}
	}
	streams := s.snapshotLocked()
	s.mu.Unlock()
	if first {
		close(s.goAwayRecv)
	}
	// Streams we opened that the peer never processed are refused now
	// rather than waiting for the peer's 4503.
	for _, st := range refused {
		st.mu.Lock()
		opening := st.state == StateOpening
		st.mu.Unlock()
		if opening {
			st.abort(CloseRelayRestart, "session draining", false)
		}
	}
	for _, st := range streams {
		st.mu.Lock()
		if st.state == StateActive || st.state == StateAccepted {
			st.state = StateDraining
		}
		st.mu.Unlock()
	}
	if g.GetDrainDeadlineMs() == 0 && g.GetCode() != 0 {
		// Immediate close announced by the peer.
		return
	}
	if first {
		// Backstop: the peer closes at its deadline; if the transport
		// lingers, end the session ourselves shortly after.
		d := time.Duration(g.GetDrainDeadlineMs())*time.Millisecond + s.cfg.WriteWait
		code, reason := g.GetCode(), g.GetReason()
		s.setTimer(&s.drainTimer, d, func() { s.drainDeadline(code, reason) })
	}
}

// ---------------------------------------------------------------- auth refresh

// RefreshAuth implements LocalSession.
func (s *session) RefreshAuth(credential []byte, streamID uint32) error {
	if s.isDone() {
		return ErrSessionClosed
	}
	return s.sendControl(s.ctx, &conduitv1.Frame{Body: &conduitv1.Frame_AuthRefresh{AuthRefresh: &conduitv1.AuthRefresh{Credential: credential, StreamId: streamID}}})
}

// handleAuthRefresh validates refreshes one at a time, in arrival order;
// refreshes arriving while one is in flight collapse to the latest (a
// newer credential supersedes older ones). A failed validation closes the
// session.
func (s *session) handleAuthRefresh(ar *conduitv1.AuthRefresh) {
	if s.adm == nil {
		return // only the relay side validates credentials
	}
	s.mu.Lock()
	if s.refreshing {
		s.refreshNext = ar
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	s.mu.Unlock()
	go func() {
		for ar != nil {
			if err := s.adm.Refresh(s.ctx, ar); err != nil {
				if !s.isDone() {
					s.closeWithCode(CodeOf(err, CloseUnauthenticated), rejectReason(err))
				}
				return // the session is ending; refreshing stays set
			}
			s.mu.Lock()
			ar, s.refreshNext = s.refreshNext, nil
			if ar == nil {
				s.refreshing = false
			}
			s.mu.Unlock()
		}
	}()
}

// ---------------------------------------------------------------- RPC

// Call implements Session.
//
// Requests whose body exceeds MaxRPCBody, or whose encoded frame exceeds
// Config.MaxRPCFrame, are answered with 413 locally. On a draining
// session (GoAway sent or received) Call returns ErrDraining: new work
// belongs on the replacement session.
func (s *session) Call(ctx context.Context, req *conduitv1.RpcRequest) (*conduitv1.RpcResponse, error) {
	if len(req.GetBody()) > MaxRPCBody {
		return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: 413}, nil
	}
	if s.isDone() {
		return nil, ErrSessionClosed
	}
	req = proto.Clone(req).(*conduitv1.RpcRequest)
	if req.RequestId == "" {
		req.RequestId = "c" + strconv.FormatUint(s.rpcSeq.Add(1), 10)
	}
	id := req.RequestId
	frame := &conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: req}}
	if proto.Size(frame) > s.cfg.MaxRPCFrame {
		return &conduitv1.RpcResponse{RequestId: id, Status: 413}, nil
	}
	ch := make(chan *conduitv1.RpcResponse, 1)
	s.mu.Lock()
	if s.isDone() {
		s.mu.Unlock()
		return nil, s.endErr()
	}
	if s.draining {
		s.mu.Unlock()
		return nil, ErrDraining
	}
	if _, dup := s.pendingRPC[id]; dup {
		s.mu.Unlock()
		return nil, fmt.Errorf("conduit: duplicate request id %q", id)
	}
	s.pendingRPC[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingRPC, id)
		s.mu.Unlock()
		s.closeIfDrained()
	}()

	if err := s.sendControl(ctx, frame); err != nil {
		if err == ErrSessionClosed {
			err = s.endErr()
		}
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_RpcCancel{RpcCancel: &conduitv1.RpcCancel{RequestId: id}}})
		return nil, ctx.Err()
	case <-s.done:
		select {
		case resp := <-ch: // answered just before a drained close
			return resp, nil
		default:
		}
		return nil, s.endErr()
	}
}

func (s *session) handleRPCResponse(r *conduitv1.RpcResponse) {
	s.mu.Lock()
	ch := s.pendingRPC[r.GetRequestId()]
	s.mu.Unlock()
	if ch != nil {
		// Call removes the entry (and re-checks the drain) when it
		// returns; the channel has room for exactly one response.
		select {
		case ch <- r:
		default: // duplicate response
		}
	}
}

// RetryAfterHeader is set (to "0") on the 503 answering an RPC that
// reached a draining session: the caller should retry on the replacement
// session.
const RetryAfterHeader = "Retry-After"

func (s *session) handleRPCRequest(req *conduitv1.RpcRequest) {
	id := req.GetRequestId()
	// refuse answers from the read loop without blocking: status-only
	// responses are tiny and use the internal control reserve, and they
	// are queued ahead of any close-after-flush that follows.
	refuse := func(status int32, reason string) {
		resp := &conduitv1.RpcResponse{RequestId: id, Status: status}
		if reason != "" {
			resp.Body = []byte(reason)
		}
		if status == 503 {
			resp.Headers = map[string]string{RetryAfterHeader: "0"}
		}
		s.sendInternal(&conduitv1.Frame{Body: &conduitv1.Frame_RpcResponse{RpcResponse: resp}})
	}
	if len(req.GetBody()) > MaxRPCBody || proto.Size(&conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: req}}) > s.cfg.MaxRPCFrame {
		refuse(413, "")
		return
	}
	h := s.cfg.RPCHandler
	if h == nil {
		refuse(501, "")
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	var status int32
	var reason string
	switch _, dup := s.inboundRPC[id]; {
	case s.draining:
		status, reason = 503, "conduit: session draining"
	case dup:
		status = 409
	case len(s.inboundRPC) >= s.cfg.MaxConcurrentRPCs:
		status, reason = 429, "conduit: too many concurrent rpcs"
	}
	if status != 0 {
		s.mu.Unlock()
		cancel()
		refuse(status, reason)
		return
	}
	s.inboundRPC[id] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.inboundRPC, id)
			s.mu.Unlock()
			cancel()
			s.closeIfDrained() // after the reply was queued
		}()
		resp := h.HandleRPC(ctx, req)
		if ctx.Err() != nil {
			return // cancelled: the caller is gone
		}
		if resp == nil {
			resp = &conduitv1.RpcResponse{Status: 500}
		}
		resp = proto.Clone(resp).(*conduitv1.RpcResponse)
		resp.RequestId = id
		frame := &conduitv1.Frame{Body: &conduitv1.Frame_RpcResponse{RpcResponse: resp}}
		if len(resp.GetBody()) > MaxRPCBody || proto.Size(frame) > s.cfg.MaxRPCFrame {
			frame = &conduitv1.Frame{Body: &conduitv1.Frame_RpcResponse{RpcResponse: &conduitv1.RpcResponse{RequestId: id, Status: 413}}}
		}
		_ = s.sendControl(s.ctx, frame)
	}()
}

// ---------------------------------------------------------------- stats

// Stats implements LocalSession.
func (s *session) Stats() Stats {
	var st Stats
	s.sched.stats(&st)
	st.RecvBufferedBytes = s.recvBuffered.Load()
	st.PeakRecvBufferedBytes = s.peakRecvBufrd.Load()
	s.mu.Lock()
	st.OpenStreams = len(s.streams)
	s.mu.Unlock()
	return st
}
