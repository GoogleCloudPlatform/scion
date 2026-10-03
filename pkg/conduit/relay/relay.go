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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Defaults.
const (
	// DefaultHeartbeatInterval is how often the relay refreshes its
	// relay_instances row (design §3.4: every 15s, stale after 60s).
	DefaultHeartbeatInterval = 15 * time.Second
	// DefaultRPCTimeout caps an internal relay RPC (design §3.5).
	DefaultRPCTimeout = 120 * time.Second
	// DefaultDeleteAttempts bounds the DeleteSessionCAS retries after a
	// session ends; ReapStaleSessions is the backstop beyond that.
	DefaultDeleteAttempts = 5
	// defaultDeleteBackoff is the first retry delay (doubling, capped at
	// maxDeleteBackoff).
	defaultDeleteBackoff = 200 * time.Millisecond
	maxDeleteBackoff     = 5 * time.Second
	// deleteTimeout bounds one DeleteSessionCAS attempt.
	deleteTimeout = 10 * time.Second
	// DefaultReconnectJitter bounds the uniform random
	// GoAway.reconnect_after_ms the relay sends when it ends many sessions
	// at once (supersede, drain, rows reaped), so their targets do not
	// redial in one synchronized wave.
	DefaultReconnectJitter = 5 * time.Second
)

// Errors.
var (
	// ErrSuperseded is reported on Fatal when the registry says another
	// generation of this relay instance now owns the row
	// (registry.ErrRelaySuperseded). The relay has stopped serving: it
	// refuses new sessions and has sent GoAway to every live session. The
	// process must drain and exit (or build a new Relay and re-register).
	ErrSuperseded = errors.New("conduit relay: superseded by a newer generation; stopped serving")
	// ErrNotServing is returned by operations on a relay that has not
	// started or has stopped.
	ErrNotServing = errors.New("conduit relay: not serving")
)

// Principal is a target principal already authenticated by the hub at the
// HTTP layer (agent JWT, broker HMAC, user bearer). The Hello must match it.
// ProjectID, ExecScope, Agent and Incarnation are the hub's authoritative
// values (from the agent/broker row), not the dialer's claims. The
// endpoint_incarnation recorded on the row is decided by the policy in
// incarnation.go (AdmitAgentIncarnation / AdmitBrokerIncarnation).
type Principal struct {
	Kind      string // registry.PrincipalAgent | PrincipalBroker | PrincipalUser
	ID        string
	ProjectID string // agents: required; brokers and users: ""
	ExecScope string // "" = unscoped
	// Agent holds the agent row's launch_id and generation (agents only).
	Agent AgentIncarnationFacts
	// Incarnation is a broker's authoritative incarnation, if the hub
	// knows one; "" accepts the broker's presented process start id.
	Incarnation string
}

// GrantKeySource returns the grant verification keys to publish in
// Welcome.grant_keys: every key still within not_after (contracts §3).
type GrantKeySource func(ctx context.Context) ([]*conduitv1.GrantKey, error)

// RefreshFunc re-validates an in-band AuthRefresh credential for p. An
// error closes the session with 4401 (or the *conduit.CloseError code).
type RefreshFunc func(ctx context.Context, p Principal, ar *conduitv1.AuthRefresh) error

// Config configures a Relay.
type Config struct {
	// InstanceID is this relay's instance id (the hub's instance id for an
	// in-process relay). Required.
	InstanceID string
	// InternalEndpoint is the base URL (http://host:port) other relays use
	// to reach this relay's internal API. "" means unaddressable: allowed
	// only in single-node profiles; RequireSelfCheck refuses it.
	InternalEndpoint string
	// PublicEndpoint is optional (separate relay profiles).
	PublicEndpoint string
	// RequireSelfCheck makes Start fail unless the relay is addressable and
	// registry.SelfCheck passes (hosted-HA, design §3.9). Without it a
	// failed self-check is logged and the relay registers as unaddressable,
	// so no other node routes to it.
	RequireSelfCheck bool
	// Registry is the conduit registry. Required.
	Registry *registry.Registry
	// Store is the registry's store (the one Registry wraps). Required:
	// admission reads a principal's raw rows (liveness and epoch currency
	// ignored) for the launch-id fallback fence.
	Store registry.Store
	// Session is the template for every accepted session. StreamHandler is
	// overridden (targets do not open streams toward the relay in Phase 1);
	// Interceptor is chained, not replaced.
	Session conduit.Config
	// GrantKeys supplies Welcome.grant_keys. Required: a target without
	// keys cannot verify grants, so admission fails closed if it errors.
	GrantKeys GrantKeySource
	// Refresh validates AuthRefresh. Nil refuses every refresh with 4401.
	Refresh RefreshFunc
	// PeerAuth authenticates internal API calls in both directions.
	// Required for InternalHandler and for the self-check probe.
	PeerAuth PeerAuth
	// HTTPClient performs the self-check probe (default: a client with a
	// 10s timeout).
	HTTPClient *http.Client
	// HeartbeatInterval defaults to DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration
	// LifetimeHint is sent as Welcome.lifetime_hint_s (0 = none).
	LifetimeHint time.Duration
	// DeleteAttempts bounds DeleteSessionCAS retries (default 5).
	DeleteAttempts int
	// RPCTimeout caps internal RPCs (default 120s).
	RPCTimeout time.Duration
	// Clock drives the relay's timers (default: Session.Clock, else real).
	// The registry keeps its own clock (registry.Config.Clock).
	Clock clock.Clock
	// RegistryNow is the time used for registry eligibility reads made
	// during admission; it should agree with the registry's clock
	// (default time.Now).
	RegistryNow func() time.Time
	// NewSessionID generates session ids (default uuid v4).
	NewSessionID func() string
	// ReconnectJitter bounds the random reconnect hint sent with relay
	// initiated GoAways (default DefaultReconnectJitter; negative: none).
	ReconnectJitter time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

type relayState int

const (
	stateNew relayState = iota
	stateServing
	stateDraining
	stateStopped
)

// Relay is the in-process relay role.
type Relay struct {
	cfg Config
	log *slog.Logger
	clk clock.Clock

	mu       sync.Mutex
	state    relayState
	gen      int64
	endpoint string // registered internal endpoint ("" if unaddressable)
	sessions map[string]*entry
	hbTimer  clock.Timer
	killed   bool

	fatal     chan error
	fatalOnce sync.Once
	// wg tracks touch goroutines and serves tracks Serve calls (including
	// the session-row delete after the session ends). Both are only Added
	// under mu while the state is not stopped, and only waited on after
	// the state became stopped, so an Add never races a Wait.
	wg     sync.WaitGroup
	serves sync.WaitGroup

	// bridges tracks owner-side stream bridges (internal stream WS
	// handlers) so tests can prove none leak.
	bridges       sync.WaitGroup
	activeBridges atomic.Int64
	// testHookAfterOpen runs after the target accepted a bridged stream
	// and before the late-accept check (test seam for the cancel race).
	testHookAfterOpen func(hop *wsStream)
	// testHookBeforeReady runs in Serve after Accept returned and before
	// the session is marked ready (the pipelined-StreamOpen race seam).
	testHookBeforeReady func()
}

// ActiveBridges returns the number of owner-side stream bridges running.
func (r *Relay) ActiveBridges() int64 { return r.activeBridges.Load() }

// entry is one live local session.
type entry struct {
	rec       registry.SessionRecord
	source    string // incarnation source (incarnation.go)
	principal Principal
	sess      conduit.LocalSession

	ready    atomic.Bool // sess is set
	touching atomic.Bool // a TouchSession is in flight
	again    atomic.Bool // a pong arrived while touching (coalesced)
	// closeForbidden records that a user StreamOpen was refused before
	// sess was set; Serve closes the session with 4403 once it is.
	closeForbidden atomic.Bool
}

// New validates cfg and returns a Relay. Call Start before Serve.
func New(cfg Config) (*Relay, error) {
	if cfg.InstanceID == "" {
		return nil, errors.New("conduit relay: InstanceID is required")
	}
	if cfg.Registry == nil || cfg.Store == nil {
		return nil, errors.New("conduit relay: Registry and Store are required")
	}
	if cfg.GrantKeys == nil {
		return nil, errors.New("conduit relay: GrantKeys is required")
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.DeleteAttempts <= 0 {
		cfg.DeleteAttempts = DefaultDeleteAttempts
	}
	if cfg.RPCTimeout <= 0 {
		cfg.RPCTimeout = DefaultRPCTimeout
	}
	if cfg.Clock == nil {
		cfg.Clock = cfg.Session.Clock
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.Session.Clock == nil {
		cfg.Session.Clock = cfg.Clock
	}
	if cfg.NewSessionID == nil {
		cfg.NewSessionID = uuid.NewString
	}
	if cfg.ReconnectJitter == 0 {
		cfg.ReconnectJitter = DefaultReconnectJitter
	}
	if cfg.RegistryNow == nil {
		cfg.RegistryNow = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Relay{
		cfg:      cfg,
		log:      cfg.Logger.With("component", "conduit-relay", "relay_instance_id", cfg.InstanceID),
		clk:      cfg.Clock,
		sessions: make(map[string]*entry),
		fatal:    make(chan error, 1),
	}, nil
}

// InstanceID returns the relay's instance id.
func (r *Relay) InstanceID() string { return r.cfg.InstanceID }

// Generation returns the registered generation (0 before Start).
func (r *Relay) Generation() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen
}

// InternalEndpoint returns the endpoint the relay registered ("" when
// unaddressable).
func (r *Relay) InternalEndpoint() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endpoint
}

// Fatal delivers at most one error after which the relay no longer serves
// (ErrSuperseded). The hub treats it like a failed listener: drain and exit.
func (r *Relay) Fatal() <-chan error { return r.fatal }

// Start runs the startup sequence (design §3.4): self-check of the internal
// endpoint, RegisterRelay, SweepOwnOlderGenerations, then the heartbeat
// loop. The internal API must already be served at InternalEndpoint, since
// the self-check dials it. With RequireSelfCheck any failure is returned
// and the relay does not register (hosted-HA must exit non-zero).
func (r *Relay) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.state != stateNew {
		r.mu.Unlock()
		return errors.New("conduit relay: Start called twice")
	}
	r.mu.Unlock()

	endpoint := r.cfg.InternalEndpoint
	if err := registry.SelfCheck(ctx, endpoint, r.cfg.InstanceID, r.probe); err != nil {
		if r.cfg.RequireSelfCheck {
			return fmt.Errorf("conduit relay %s: owner-addressability self-check failed (hosted-HA requires an addressable relay; set --internal-listen and an internal endpoint that reaches exactly this process): %w", r.cfg.InstanceID, err)
		}
		if endpoint != "" {
			r.log.Warn("Conduit relay self-check failed; registering as unaddressable, so no other node routes to this relay", "internal_endpoint", endpoint, "error", err)
		}
		endpoint = ""
	}
	gen, err := r.cfg.Registry.RegisterRelay(ctx, registry.RelayInstance{
		InstanceID:       r.cfg.InstanceID,
		InternalEndpoint: endpoint,
		PublicEndpoint:   r.cfg.PublicEndpoint,
	})
	if err != nil {
		return fmt.Errorf("conduit relay %s: register: %w", r.cfg.InstanceID, err)
	}
	if n, err := r.cfg.Registry.SweepOwnOlderGenerations(ctx, r.cfg.InstanceID, gen); err != nil {
		// Not fatal: the rows are already ineligible (relay_superseded)
		// and the reaper removes them.
		r.log.Warn("Conduit relay: sweeping older-generation sessions failed", "error", err)
	} else if n > 0 {
		r.log.Info("Conduit relay: swept sessions of an older generation", "count", n)
	}
	r.mu.Lock()
	r.gen, r.endpoint, r.state = gen, endpoint, stateServing
	r.mu.Unlock()
	r.log.Info("Conduit relay serving", "generation", gen, "internal_endpoint", endpoint)
	r.armHeartbeat()
	return nil
}

func (r *Relay) armHeartbeat() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != stateServing && r.state != stateDraining {
		return
	}
	r.hbTimer = r.clk.AfterFunc(r.cfg.HeartbeatInterval, r.heartbeat)
}

func (r *Relay) heartbeat() {
	r.mu.Lock()
	gen, state, killed := r.gen, r.state, r.killed
	r.mu.Unlock()
	if killed || state == stateStopped || state == stateNew {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.HeartbeatInterval)
	err := r.cfg.Registry.HeartbeatRelay(ctx, r.cfg.InstanceID, gen)
	cancel()
	switch {
	case errors.Is(err, registry.ErrRelaySuperseded):
		r.supersede()
		return
	case err != nil:
		// Keep serving: if the outage outlasts RelayStaleAfter the
		// registry already treats our sessions as stale, so nobody
		// routes to them; the next successful beat restores them.
		r.log.Warn("Conduit relay heartbeat failed", "error", err)
	}
	r.armHeartbeat()
}

// supersede stops serving after ErrRelaySuperseded (fail closed): no new
// sessions, GoAway to every live session (their rows are already
// ineligible), and ErrSuperseded on Fatal.
func (r *Relay) supersede() {
	r.mu.Lock()
	if r.state == stateStopped {
		r.mu.Unlock()
		return
	}
	r.state = stateStopped
	if r.hbTimer != nil {
		r.hbTimer.Stop()
	}
	entries := r.snapshotLocked()
	r.mu.Unlock()
	r.log.Error("Conduit relay superseded by a newer generation of this instance; stopped serving")
	for _, e := range entries {
		_ = e.sess.GoAway(conduit.GoAwayOptions{Code: conduit.CloseRelayRestart, Reason: reason(ReasonRelayRestart, "relay superseded"), ReconnectAfter: r.reconnectAfter()})
	}
	r.fatalOnce.Do(func() { r.fatal <- ErrSuperseded })
}

// reconnectAfter is a uniform random reconnect hint in [0,
// ReconnectJitter], spreading the redials of sessions the relay ends
// together.
func (r *Relay) reconnectAfter() time.Duration {
	if r.cfg.ReconnectJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(r.cfg.ReconnectJitter) + 1))
}

func (r *Relay) snapshotLocked() []*entry {
	out := make([]*entry, 0, len(r.sessions))
	for _, e := range r.sessions {
		out = append(out, e)
	}
	return out
}

// Serve runs one target session on conn, already authenticated as p by the
// hub. It performs the conduit handshake (admission inserts the registry
// row), serves the session until it ends, then deletes the row (CAS,
// retried). It blocks for the life of the session and returns why it ended.
func (r *Relay) Serve(ctx context.Context, conn transport.Conn, p Principal) error {
	if r.trackServe() {
		defer r.serves.Done()
	}
	adm := &admitter{r: r, p: p, transport: conn.Transport()}
	e := &entry{principal: p}
	cfg := r.cfg.Session
	cfg.Interceptor = chainInterceptor(r.touchOnPong(e), r.cfg.Session.Interceptor)
	cfg.StreamHandler = conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
		return r.refuseDialerStream(e, ps)
	})
	if cfg.Logger == nil {
		cfg.Logger = r.log
	}
	sess, err := conduit.Accept(ctx, conn, cfg, adm)
	if err != nil {
		return err
	}
	ls, ok := sess.(conduit.LocalSession)
	if !ok { // cannot happen: Accept returns *session
		_ = sess.Close()
		return errors.New("conduit relay: accepted session is not local")
	}
	rec, source, ok := adm.admitted()
	if !ok {
		_ = ls.Close()
		return errors.New("conduit relay: session started without an admission record")
	}
	if h := r.testHookBeforeReady; h != nil {
		h()
	}
	e.rec, e.source, e.sess = rec, source, ls
	e.ready.Store(true)

	r.mu.Lock()
	r.sessions[rec.SessionID] = e
	state := r.state
	r.mu.Unlock()
	switch {
	case e.closeForbidden.Load():
		// A user StreamOpen arrived before sess was set (pipelined
		// after the Hello); refuseDialerStream could not close it.
		_ = ls.CloseWithCode(conduit.CloseForbidden, userStreamRefused)
	case state != stateServing:
		// Drain or supersede began while this session was admitted.
		r.goAway(e, conduit.GoAwayOptions{Reason: ReasonDraining, ReconnectAfter: r.reconnectAfter()})
	}

	<-ls.Done()

	r.mu.Lock()
	if r.sessions[rec.SessionID] == e {
		delete(r.sessions, rec.SessionID)
	}
	killed := r.killed
	r.mu.Unlock()
	if !killed {
		r.deleteRow(context.Background(), rec.SessionID, rec.RelayGeneration)
	}
	return ls.Err()
}

// trackServe registers a Serve call with serves unless the relay has
// stopped (then admission refuses the session anyway).
func (r *Relay) trackServe() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateStopped || r.killed {
		return false
	}
	r.serves.Add(1)
	return true
}

var userStreamRefused = reason(ReasonForbidden, "user sessions may not open streams")

// refuseDialerStream handles a StreamOpen sent by a target. In Phase 1 only
// the relay opens streams (toward targets). A user session must never open
// a stream (design §3.10): the frame is refused and the session is closed
// with 4403. Agents and brokers get the stream refused with 4403.
func (r *Relay) refuseDialerStream(e *entry, ps conduit.PendingStream) error {
	if e.principal.Kind == registry.PrincipalUser {
		_ = ps.Reject(conduit.CloseForbidden, userStreamRefused)
		// The session may still be starting: Accept runs the read loop
		// before Serve sets e.sess. Record the refusal first, then check
		// ready; Serve sets ready, then checks the flag, so at least one
		// side closes (a second close is a no-op).
		e.closeForbidden.Store(true)
		if e.ready.Load() {
			go func() { _ = e.sess.CloseWithCode(conduit.CloseForbidden, userStreamRefused) }()
		}
		return nil
	}
	return ps.Reject(conduit.CloseForbidden, reason(ReasonForbidden, "targets may not open streams toward the relay"))
}

// touchOnPong returns an inbound interceptor that refreshes the session
// row's last_seen on every Pong (design §3.4: bumped on pong). It never
// blocks the read loop: the touch runs in a goroutine, at most one per
// session is in flight, and pongs arriving meanwhile are coalesced into one
// follow-up touch. registry.ErrSessionNotFound (row reaped or replaced)
// sends GoAway 4503 with a jittered reconnect hint so the target
// reconnects (rows reaped together, e.g. after a long registry outage, do
// not redial together).
func (r *Relay) touchOnPong(e *entry) conduit.Interceptor {
	return func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if dir == conduit.Inbound && f.GetPong() != nil && e.ready.Load() {
			r.scheduleTouch(e)
		}
		return []*conduitv1.Frame{f}
	}
}

func (r *Relay) scheduleTouch(e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateStopped || r.killed {
		return
	}
	if !e.touching.CompareAndSwap(false, true) {
		e.again.Store(true)
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			e.again.Store(false)
			r.touch(e)
			e.touching.Store(false)
			// A pong that arrived during the touch is served by one more
			// touch, unless another goroutine has claimed it.
			if !e.again.Load() || !e.touching.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

func (r *Relay) touch(e *entry) {
	select {
	case <-e.sess.Done():
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	err := r.cfg.Registry.TouchSession(ctx, e.rec.SessionID)
	switch {
	case errors.Is(err, registry.ErrSessionNotFound):
		r.log.Warn("Conduit session row is gone (reaped or replaced); closing the session", "session_id", e.rec.SessionID)
		_ = e.sess.GoAway(conduit.GoAwayOptions{Code: conduit.CloseRelayRestart, Reason: reason(ReasonRelayRestart, "session no longer registered"), ReconnectAfter: r.reconnectAfter()})
	case err != nil:
		r.log.Warn("Conduit session touch failed", "session_id", e.rec.SessionID, "error", err)
	}
}

// chainInterceptor runs first, then next on each frame first returns.
func chainInterceptor(first, next conduit.Interceptor) conduit.Interceptor {
	if next == nil {
		return first
	}
	return func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		var out []*conduitv1.Frame
		for _, g := range first(dir, f) {
			out = append(out, next(dir, g)...)
		}
		return out
	}
}

// deleteRow deletes a session row this relay created, retrying with bounded
// backoff (DeleteSessionCAS is idempotent) until parent is done.
// ReapStaleSessions is the backstop if every attempt fails.
func (r *Relay) deleteRow(parent context.Context, sessionID string, gen int64) {
	backoff := defaultDeleteBackoff
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(parent, deleteTimeout)
		_, err := r.cfg.Registry.DeleteSessionCAS(ctx, sessionID, r.cfg.InstanceID, gen)
		cancel()
		if err == nil {
			return
		}
		if attempt >= r.cfg.DeleteAttempts || parent.Err() != nil {
			r.log.Warn("Conduit session row delete failed; the stale-session reaper will remove it", "session_id", sessionID, "attempts", attempt, "error", err)
			return
		}
		ch, stop := clock.After(r.clk, backoff)
		select {
		case <-ch:
		case <-parent.Done():
			stop()
			r.log.Warn("Conduit session row delete abandoned; the stale-session reaper will remove it", "session_id", sessionID, "attempts", attempt, "error", err)
			return
		}
		backoff = min(backoff*2, maxDeleteBackoff)
	}
}

// GoAway starts a planned drain of one local session: the row is marked
// draining first (so routing stops choosing it), then GoAway is sent.
func (r *Relay) GoAway(sessionID string, opts conduit.GoAwayOptions) error {
	r.mu.Lock()
	e := r.sessions[sessionID]
	r.mu.Unlock()
	if e == nil {
		return registry.ErrSessionNotFound
	}
	r.goAway(e, opts)
	return nil
}

func (r *Relay) goAway(e *entry, opts conduit.GoAwayOptions) {
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	if err := r.cfg.Registry.SetSessionDraining(ctx, e.rec.SessionID); err != nil && !errors.Is(err, registry.ErrSessionNotFound) {
		r.log.Warn("Conduit: marking session draining failed", "session_id", e.rec.SessionID, "error", err)
	}
	cancel()
	_ = e.sess.GoAway(opts)
}

// Local returns the live local session with sessionID, if this relay holds
// it, together with its registry record.
func (r *Relay) Local(sessionID string) (conduit.LocalSession, registry.SessionRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.sessions[sessionID]
	if e == nil || r.killed {
		return nil, registry.SessionRecord{}, false
	}
	return e.sess, e.rec, true
}

// CloseSessionsOf closes every local session of the principal (agent
// deletion). Rows are deleted as each session ends.
func (r *Relay) CloseSessionsOf(kind, id string, code uint32, reason string) int {
	r.mu.Lock()
	var hit []*entry
	for _, e := range r.sessions {
		if e.rec.PrincipalKind == kind && e.rec.PrincipalID == id {
			hit = append(hit, e)
		}
	}
	r.mu.Unlock()
	for _, e := range hit {
		_ = e.sess.CloseWithCode(code, reason)
	}
	return len(hit)
}

// Sessions returns the number of live local sessions.
func (r *Relay) Sessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Shutdown drains the relay: it refuses new sessions, marks its relay row
// draining, sends GoAway (jittered reconnect hint) to every session
// (marking each row draining first) and waits, until ctx is done, for
// every session to end and every Serve call to finish deleting its row,
// so the caller may close the registry store once Shutdown returns nil.
// Sessions still live at the deadline, including ones admitted while the
// drain began, are closed with 4503 and ctx's error is returned; their row
// deletes continue in the background (the reaper is the backstop).
func (r *Relay) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	if r.state != stateServing {
		r.mu.Unlock()
		return nil
	}
	r.state = stateDraining
	gen := r.gen
	entries := r.snapshotLocked()
	r.mu.Unlock()

	dctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	if err := r.cfg.Registry.SetRelayDraining(dctx, r.cfg.InstanceID, gen, true); err != nil {
		r.log.Warn("Conduit relay: marking relay draining failed", "error", err)
	}
	cancel()
	for _, e := range entries {
		r.goAway(e, conduit.GoAwayOptions{Reason: ReasonDraining, ReconnectAfter: r.reconnectAfter()})
	}
	for _, e := range entries {
		select {
		case <-e.sess.Done():
		case <-ctx.Done():
		}
	}
	// No Serve or touch is registered once stopped, so waiting is safe.
	r.mu.Lock()
	r.state = stateStopped
	if r.hbTimer != nil {
		r.hbTimer.Stop()
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.serves.Wait()
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	r.mu.Lock()
	late := r.snapshotLocked()
	r.mu.Unlock()
	for _, e := range late {
		_ = e.sess.CloseWithCode(conduit.CloseRelayRestart, reason(ReasonRelayRestart, "drain deadline"))
	}
	r.log.Warn("Conduit relay: drain deadline reached; closed the remaining sessions", "sessions", len(late))
	return ctx.Err()
}

// Kill simulates a crash (test seam for 1v and fault tests): the heartbeat
// stops, every local session is dropped without deleting its row and the
// internal API answers 503. The rows stay until the reaper removes them.
func (r *Relay) Kill() {
	r.mu.Lock()
	r.killed = true
	r.state = stateStopped
	if r.hbTimer != nil {
		r.hbTimer.Stop()
	}
	entries := r.snapshotLocked()
	r.mu.Unlock()
	for _, e := range entries {
		_ = e.sess.Close()
	}
}

func (r *Relay) serving() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == stateServing && !r.killed
}

// admissionState returns the generation and the state admission sees (a
// killed relay reports stateStopped).
func (r *Relay) admissionState() (int64, relayState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.killed {
		return r.gen, stateStopped
	}
	return r.gen, r.state
}

// evictObsoleteLaunchRows deletes the agent's non-draining launch_id rows
// that are no longer epoch-current (see admitter.recheckFallback), so the
// relays holding them close the sessions and the launch container
// reconnects. Errors are logged: the launch session then stays
// unroutable until it reconnects or its row is reaped.
func (r *Relay) evictObsoleteLaunchRows(ctx context.Context, p Principal) {
	cur, ok := launchIncarnation(p.Agent)
	if !ok {
		return
	}
	ps, err := r.cfg.Store.ListPrincipalSessions(ctx, registry.PrincipalAgent, p.ID)
	if err != nil {
		r.log.Warn("Conduit: reading launch sessions to evict failed", "principal_id", p.ID, "error", err)
		return
	}
	for _, v := range ps.Sessions {
		s := v.Session
		if !isLaunchRow(s, cur.Value) || registry.EpochCurrent(ps, s) {
			continue
		}
		if _, err := r.cfg.Registry.DeleteSessionCAS(ctx, s.SessionID, s.RelayInstanceID, s.RelayGeneration); err != nil {
			r.log.Warn("Conduit: evicting an epoch-obsolete launch session failed", "session_id", s.SessionID, "error", err)
			continue
		}
		r.log.Info("Conduit: evicted a launch session made epoch-obsolete by a refused fallback admission; it will reconnect",
			"session_id", s.SessionID, "principal_id", p.ID, "relay_instance_id", s.RelayInstanceID)
	}
}
