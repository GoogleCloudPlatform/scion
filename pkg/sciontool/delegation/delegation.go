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

// Package delegation is sciontool's agent-side helper for agent-bound
// delegation (.design/agent-delegation.md §8.3, §9.3 and §18.7).
//
// The helper exchanges delegation grants for delegated credentials with
// the agent's own credential, holds each delegated credential in memory
// only, and serves hub requests for processes in the agent container
// through a local unix socket: a process sends an ordinary HTTP request
// over the socket, naming a grant in the X-Scion-Delegation-Grant header,
// and the helper forwards it to the hub with the delegated credential
// attached. The credential never leaves sciontool: it is never written to
// a file, put in a child process's environment, returned to a caller or
// logged.
//
// The helper re-exchanges a grant before its credential expires and after
// every change of the agent credential (a refresh revokes the agent
// credential a delegated credential was exchanged with, §9.3). When the hub
// refuses the agent credential at exchange (either 401), it does not retry
// with that credential: it waits for the agent credential to change, and
// backs off. A 5xx answer is retried with backoff using the same agent
// credential.
//
// Any process that can open the socket can use the delegated authority:
// the socket is mode 0600, owned by the agent user, and accepts only peers
// running as the agent user or as sciontool's own user. This is not proof
// of possession.
package delegation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/backoff"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

const (
	// GrantHeader names the delegation grant a proxied request uses.
	GrantHeader = "X-Scion-Delegation-Grant"

	// SocketName is the file name of the delegation socket in the agent
	// user's ~/.scion directory.
	SocketName = "delegation.sock"

	// maxGrants bounds the grants the helper tracks at once. The hub
	// allows at most 10 active grants per agent.
	maxGrants = 32

	// exchangeTimeout bounds one background exchange.
	exchangeTimeout = 30 * time.Second

	// minRemaining is the least remaining lifetime a credential needs to be
	// used for a request; a credential closer to expiry is re-exchanged
	// first.
	minRemaining = 5 * time.Second

	// Bounds of the margin before expiry at which a credential is
	// re-exchanged: a fifth of its lifetime, within these bounds.
	minRefreshMargin = 5 * time.Second
	maxRefreshMargin = 2 * time.Minute

	// idleAfter is how long after its last request a grant stops being
	// retried in the background after a failure (the longest credential
	// lifetime). A later request exchanges it again.
	idleAfter = time.Hour

	// Exchange retry backoff after a refused agent credential or a 5xx.
	retryBaseDelay = time.Second
	retryMaxDelay  = 5 * time.Minute
)

// grantIDPattern is the accepted form of a grant ID in GrantHeader. Grant
// IDs are hub-generated identifiers; the pattern keeps the value safe as a
// URL path segment and in a log line.
var grantIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// HubClient is what the helper needs from sciontool's hub client.
// *hub.Client implements it.
type HubClient interface {
	// ExchangeDelegation exchanges a grant with the agent's current
	// credential.
	ExchangeDelegation(ctx context.Context, grantID, audience string) (*hub.DelegatedCredential, error)
	// CredentialChanged returns a channel closed at the next change of
	// the agent credential (refresh or reset).
	CredentialChanged() <-chan struct{}
	// HubURL is the hub's base URL.
	HubURL() string
	// HTTPClient carries the hub transport settings.
	HTTPClient() *http.Client
}

// Config configures a Helper.
type Config struct {
	// Client is the hub client. Required.
	Client HubClient
	// Audience is the hub's delegated-credential audience. When empty it
	// is resolved from the hub's public /healthz (see resolveAudience).
	Audience string
	// SocketPath is where Start listens. Required for Start.
	SocketPath string
	// UID and GID own the socket, and UID is the peer user the socket
	// accepts besides sciontool's own. UID <= 0 leaves ownership to
	// sciontool's user.
	UID, GID int
	// Clock drives expiry scheduling and backoff. nil is the real clock.
	Clock clock.Clock
}

// Helper exchanges grants and serves the delegation socket. Create it
// with New.
type Helper struct {
	client  HubClient
	clk     clock.Clock
	sockCfg socketConfig

	// gen counts agent credential changes seen by the watcher. A
	// delegated credential is served only while gen equals the generation
	// it was exchanged under.
	gen atomic.Uint64

	mu       sync.Mutex
	grants   map[string]*grant
	audience string // resolved or configured audience; "" until known
	fixedAud bool   // audience came from Config
	ctx      context.Context
	cancel   context.CancelFunc
	ln       net.Listener
	srv      *http.Server
	done     chan struct{}
}

// grant is the helper's state for one delegation grant.
type grant struct {
	mu sync.Mutex
	// cred is the current delegated credential, nil when none is held.
	cred *hub.DelegatedCredential
	// credGen is the agent credential generation cred was exchanged under.
	credGen uint64
	// waiting is set when the hub refused the agent credential at
	// exchange; no exchange is made until the generation passes waitGen.
	waiting bool
	waitGen uint64
	// notBefore paces exchanges after a failure.
	notBefore time.Time
	retry     backoff.Backoff
	// lastUsed is when a request last asked for the grant's credential
	// (Unix nanoseconds). Atomic, so lookup can find idle grants without
	// taking the grant's lock.
	lastUsed atomic.Int64
	// timer re-exchanges the grant: before cred expires, or when a
	// backoff ends. timerSeq identifies the armed timer, so a timer that
	// fires after it was replaced or stopped does nothing.
	timer    clock.Timer
	timerSeq uint64
}

// New returns a helper for cfg. It does not listen until Start.
func New(cfg Config) *Helper {
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Helper{
		client:   cfg.Client,
		clk:      clk,
		sockCfg:  socketConfig{path: cfg.SocketPath, uid: cfg.UID, gid: cfg.GID},
		grants:   make(map[string]*grant),
		audience: cfg.Audience,
		fixedAud: cfg.Audience != "",
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start listens on the configured socket and serves it until Close. It
// also starts watching the agent credential, so that every change
// re-exchanges the grants in use.
func (h *Helper) Start() error {
	if h.client == nil {
		return errors.New("delegation helper: no hub client")
	}
	ln, err := listenSocket(h.sockCfg)
	if err != nil {
		return fmt.Errorf("delegation helper: %w", err)
	}
	h.mu.Lock()
	h.ln = ln
	h.srv = &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	h.done = make(chan struct{})
	srv := h.srv
	h.mu.Unlock()

	go h.watchCredential(h.client.CredentialChanged())
	go func() {
		defer close(h.done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("Delegation helper: socket server stopped: %v", err)
		}
	}()
	log.Info("Delegation helper listening on %s", h.sockCfg.path)
	return nil
}

// Close stops serving, removes the socket and drops every delegated
// credential the helper holds.
func (h *Helper) Close() error {
	h.cancel()
	h.mu.Lock()
	srv, ln, done := h.srv, h.ln, h.done
	h.mu.Unlock()
	var err error
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = srv.Shutdown(ctx)
		cancel()
		<-done
	}
	if ln != nil {
		if rmErr := removeSocket(h.sockCfg, ln); rmErr != nil && err == nil {
			err = rmErr
		}
	}
	h.mu.Lock()
	grants := h.grants
	h.grants = make(map[string]*grant)
	h.mu.Unlock()
	for _, g := range grants {
		g.mu.Lock()
		g.stopTimerLocked()
		g.cred = nil
		g.mu.Unlock()
	}
	return err
}

// watchCredential bumps the agent credential generation, and re-exchanges
// the grants in use, at every change of the agent credential. ch is the
// channel taken in Start, before any request could be served, so no
// change after Start is missed.
func (h *Helper) watchCredential(ch <-chan struct{}) {
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ch:
		}
		// Take the next channel before acting on this change, so a change
		// made while re-exchanging is not missed.
		ch = h.client.CredentialChanged()
		gen := h.gen.Add(1)
		log.Info("Delegation helper: agent credential changed; re-exchanging grants in use")
		for id, g := range h.snapshot() {
			go h.reexchangeAfterCredentialChange(id, g, gen)
		}
	}
}

// snapshot returns the tracked grants.
func (h *Helper) snapshot() map[string]*grant {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]*grant, len(h.grants))
	for id, g := range h.grants {
		out[id] = g
	}
	return out
}

// reexchangeAfterCredentialChange drops a grant's credential, which the
// refresh revoked along with the old agent credential (§9.3), and
// exchanges the grant again with the new agent credential: at once, or
// when a backoff from an earlier failure ends.
func (h *Helper) reexchangeAfterCredentialChange(id string, g *grant, gen uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.inUseLocked() || h.ctx.Err() != nil || !h.trackedLocked(id, g) {
		return
	}
	if h.idle(g, h.clk.Now()) {
		h.dropLocked(id, g)
		return
	}
	if g.cred != nil && g.credGen >= gen {
		// Already exchanged under this or a newer credential.
		return
	}
	g.cred = nil
	g.stopTimerLocked()
	if wait := g.notBefore.Sub(h.clk.Now()); wait > 0 {
		h.armLocked(id, g, wait)
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, exchangeTimeout)
	defer cancel()
	_, _ = h.exchangeLocked(ctx, id, g)
}

// inUseLocked reports whether the grant has been exchanged and not denied
// since: it holds a credential, or waits to exchange again.
func (g *grant) inUseLocked() bool {
	return g.cred != nil || g.waiting || !g.notBefore.IsZero()
}

func (g *grant) stopTimerLocked() {
	g.timerSeq++
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// lookup returns the state for grant id, creating it when absent.
func (h *Helper) lookup(id string) (*grant, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if g, ok := h.grants[id]; ok {
		return g, nil
	}
	if len(h.grants) >= maxGrants {
		// Make room by forgetting idle grants (their timers find
		// themselves untracked and stop).
		now := h.clk.Now()
		for gid, old := range h.grants {
			if h.idle(old, now) {
				delete(h.grants, gid)
			}
		}
	}
	if len(h.grants) >= maxGrants {
		return nil, errTooManyGrants
	}
	g := &grant{retry: backoff.Backoff{Base: retryBaseDelay, Max: retryMaxDelay, Jitter: backoff.DefaultJitter}}
	g.lastUsed.Store(h.clk.Now().UnixNano())
	h.grants[id] = g
	return g, nil
}

// forget stops tracking grant id if its state is still g.
func (h *Helper) forget(id string, g *grant) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants[id] == g {
		delete(h.grants, id)
	}
}

// credential returns a usable delegated credential for grant id,
// exchanging the grant when no usable one is held. Exchanges of one grant
// are serialized, so concurrent requests share one exchange.
func (h *Helper) credential(ctx context.Context, id string) (string, error) {
	g, err := h.lookup(id)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastUsed.Store(h.clk.Now().UnixNano())
	if tok, ok := h.usableLocked(g); ok {
		return tok, nil
	}
	cred, err := h.exchangeLocked(ctx, id, g)
	if err != nil {
		return "", err
	}
	return cred.Token, nil
}

// usableLocked returns g's credential when it was exchanged under the
// current agent credential and has at least minRemaining left.
func (h *Helper) usableLocked(g *grant) (string, bool) {
	if g.cred == nil || g.credGen != h.gen.Load() {
		return "", false
	}
	if !h.clk.Now().Add(minRemaining).Before(g.cred.ExpiresAt) {
		return "", false
	}
	return g.cred.Token, true
}

// invalidate drops grant id's credential if it is still tok, after the hub
// refused tok on a proxied request (for example because a later exchange
// superseded it, or it was revoked).
func (h *Helper) invalidate(id, tok string) {
	h.mu.Lock()
	g := h.grants[id]
	h.mu.Unlock()
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cred != nil && g.cred.Token == tok {
		g.cred = nil
		g.stopTimerLocked()
	}
}

// Errors the helper answers with itself.
var (
	errTooManyGrants = errors.New("too many delegation grants in use")
	// errReauthPending: the hub refused the agent credential at exchange,
	// and it has not changed since.
	errReauthPending = errors.New("the hub refused the agent credential; waiting for its refresh")
)

// backoffError is an exchange not attempted because of an earlier failure.
type backoffError struct {
	until time.Time
	cause error
}

func (e *backoffError) Error() string {
	return fmt.Sprintf("delegation exchange backing off after a failure: %v", e.cause)
}

func (e *backoffError) Unwrap() error { return e.cause }

// exchangeLocked exchanges grant id and records the outcome in g.
func (h *Helper) exchangeLocked(ctx context.Context, id string, g *grant) (*hub.DelegatedCredential, error) {
	gen := h.gen.Load()
	now := h.clk.Now()
	if g.waiting && gen <= g.waitGen {
		return nil, &backoffError{until: g.notBefore, cause: errReauthPending}
	}
	if now.Before(g.notBefore) {
		return nil, &backoffError{until: g.notBefore, cause: errors.New("retrying later")}
	}

	aud, err := h.resolveAudience(ctx)
	if err != nil {
		h.failedLocked(id, g, now, err)
		return nil, err
	}
	cred, err := h.client.ExchangeDelegation(ctx, id, aud)
	if err == nil {
		prev := g.cred
		g.cred = cred
		g.credGen = gen
		g.waiting = false
		g.notBefore = time.Time{}
		g.retry.Reset()
		h.scheduleLocked(id, g, prev)
		log.Info("Delegation helper: exchanged grant %s for credential %s (expires %s)",
			id, cred.CredentialID, cred.ExpiresAt.UTC().Format(time.RFC3339))
		return cred, nil
	}
	if ctx.Err() != nil {
		// The caller went away; this is not the hub's answer.
		return nil, err
	}

	var xe *hub.ExchangeError
	switch {
	case errors.As(err, &xe) && xe.Reauthenticate():
		// Both 401 answers (§8.2 step 2): do not retry with this agent
		// credential. Wait for it to change, and back off.
		g.cred = nil
		g.stopTimerLocked()
		g.waiting = true
		g.waitGen = gen
		g.notBefore = now.Add(g.retry.Failure())
		log.Error("Delegation helper: exchange of grant %s refused the agent credential (%d %s); waiting for its refresh",
			id, xe.StatusCode, xe.Code)
	case errors.As(err, &xe) && !xe.Retryable():
		// The hub denied the exchange (grant revoked, expired, not found,
		// policy narrowed, ...). Stop tracking the grant; a later request
		// exchanges it afresh.
		if xe.Code == "invalid_audience" && !h.fixedAud {
			h.mu.Lock()
			h.audience = ""
			h.mu.Unlock()
		}
		g.cred = nil
		g.stopTimerLocked()
		g.waiting = false
		g.notBefore = time.Time{}
		g.retry.Reset()
		h.forget(id, g)
		log.Info("Delegation helper: exchange of grant %s denied (%d %s)", id, xe.StatusCode, xe.Code)
	default:
		// 5xx or no answer: retry with the same agent credential after a
		// backoff.
		h.failedLocked(id, g, now, err)
	}
	return nil, err
}

// failedLocked records a retryable exchange failure and, while the grant
// is in recent use, schedules a background retry for when the backoff
// ends.
func (h *Helper) failedLocked(id string, g *grant, now time.Time, err error) {
	delay := g.retry.Failure()
	g.notBefore = now.Add(delay)
	log.Error("Delegation helper: exchange of grant %s failed, retrying in %s: %v", id, delay.Round(time.Second), err)
	if h.idle(g, now) {
		h.dropLocked(id, g)
		return
	}
	h.armLocked(id, g, delay)
}

// idle reports whether no request has asked for g's credential for
// idleAfter or longer.
func (h *Helper) idle(g *grant, now time.Time) bool {
	return now.Sub(time.Unix(0, g.lastUsed.Load())) >= idleAfter
}

// dropLocked drops g's credential and timer and stops tracking grant id; a
// later request exchanges the grant afresh.
func (h *Helper) dropLocked(id string, g *grant) {
	g.cred = nil
	g.stopTimerLocked()
	h.forget(id, g)
}

// trackedLocked reports whether g is still the tracked state for grant id.
func (h *Helper) trackedLocked(id string, g *grant) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.grants[id] == g
}

// refreshMargin is how long before expiry a credential with lifetime ttl
// is re-exchanged.
func refreshMargin(ttl time.Duration) time.Duration {
	return min(max(ttl/5, minRefreshMargin), maxRefreshMargin)
}

// scheduleLocked arms the re-exchange of g's new credential before it
// expires, unless a re-exchange could not usefully extend it. The hub caps a
// credential's lifetime at the grant's expiry and the agent credential's
// (§8.2 step 14), so near either the lifetime stops growing: no timer is
// armed when the delay would be under minRefreshMargin, or when the new
// credential does not outlive prev (the one it replaces) by more than
// minRefreshMargin. The credential then lapses, and the next request
// exchanges on demand. An idle grant is not re-exchanged.
func (h *Helper) scheduleLocked(id string, g *grant, prev *hub.DelegatedCredential) {
	// A timer armed for an earlier credential (an on-demand exchange can
	// replace one before its timer fires) never outlives this decision.
	g.stopTimerLocked()
	now := h.clk.Now()
	if h.idle(g, now) {
		return
	}
	if prev != nil && !g.cred.ExpiresAt.After(prev.ExpiresAt.Add(minRefreshMargin)) {
		log.Info("Delegation helper: credential for grant %s cannot be extended further; not re-exchanging before it expires", id)
		return
	}
	ttl := g.cred.ExpiresAt.Sub(now)
	d := ttl - refreshMargin(ttl)
	if d < minRefreshMargin {
		return
	}
	h.armLocked(id, g, d)
}

// armLocked replaces g's timer with one that re-exchanges grant id after d.
func (h *Helper) armLocked(id string, g *grant, d time.Duration) {
	g.stopTimerLocked()
	seq := g.timerSeq
	g.timer = h.clk.AfterFunc(d, func() { go h.timerFired(id, g, seq) })
}

// timerFired re-exchanges grant id when the timer armed as seq fires,
// unless that timer was replaced or stopped meanwhile.
func (h *Helper) timerFired(id string, g *grant, seq uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h.ctx.Err() != nil || g.timerSeq != seq {
		return
	}
	g.timer = nil
	if !h.trackedLocked(id, g) {
		g.cred = nil
		return
	}
	if h.idle(g, h.clk.Now()) {
		h.dropLocked(id, g)
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, exchangeTimeout)
	defer cancel()
	_, _ = h.exchangeLocked(ctx, id, g)
}
