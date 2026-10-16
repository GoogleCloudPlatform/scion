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

package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Runtime Broker conduit session re-check (design §3.4, criterion 16c).
//
// A broker's conduit sessions close with 4401 authz_expired when the
// broker is deregistered, its secret is rotated or replaced, or its secret
// is no longer active (status other than active, expired, or removed).
// Each session binds to the broker secret it was admitted with: the
// session stays open only while that same secret is the broker's current,
// active secret.
//
// The re-check reuses the stream re-check engine (conduitStreamAuthz) with
// its own instance, one tracked entry per broker session this node serves
// and no authorization deadline. Its triggers:
//   - notify: conduitBrokerCredentialChanged, called once a change has
//     committed, re-checks the broker's sessions on this node and publishes
//     conduitBrokerChangedSubject, which every other hub node acts on for
//     the sessions it holds;
//   - sweep: every conduit.authz_recheck_interval, every tracked session;
//   - resync: every tracked session whenever the node's LISTEN connection
//     is (re)established and whenever the binding to the event publisher is
//     (re)installed.
//
// A check that cannot be evaluated (a store read error) leaves the session
// open and is logged at WARN; the next trigger evaluates it again. The
// trigger and outcome are recorded in the hub's log line and in the
// stream_authz metric (kind "broker"); the broker sees only the close.

// conduitBrokerChangedSubject is the event subject of a committed change to
// a broker's registration or secret. The payload is a conduitBrokerChange;
// an empty or malformed payload re-checks every broker session. It is a
// non-project subject, so it travels on the global channel.
const conduitBrokerChangedSubject = "conduit.broker.changed"

// conduitBrokerSessionKind is the kind attribute of a broker session's
// re-check log line and metric.
const conduitBrokerSessionKind = "broker"

// conduitBrokerChange is the payload of conduitBrokerChangedSubject.
type conduitBrokerChange struct {
	BrokerID string `json:"brokerId,omitempty"`
}

// errConduitBrokerCredentialChanged is the revalidation failure of a
// broker session whose admitting secret is no longer the broker's current,
// active secret.
var errConduitBrokerCredentialChanged = errors.New("runtime broker credential no longer current")

// conduitBrokerSecretFingerprint identifies the broker secret a session was
// admitted with. It is held in memory only and never logged.
type conduitBrokerSecretFingerprint [sha256.Size]byte

func fingerprintBrokerSecret(key []byte) conduitBrokerSecretFingerprint {
	return sha256.Sum256(key)
}

// conduitBrokerSession is the hub's state for one broker session this
// node serves, from before admission until the session ends.
type conduitBrokerSession struct {
	brokerID string
	// fp is the fingerprint of the secret that verified the upgrade.
	fp conduitBrokerSecretFingerprint
	// denied is set once a re-check found the session's credential no
	// longer current. It is set before the re-check closes the broker's
	// registered sessions, and the post-registration check honours it, so
	// a session that registers after that close still closes. It is
	// never cleared.
	denied atomic.Bool
}

// conduitBrokerRevoker is one node's broker session re-check.
type conduitBrokerRevoker struct {
	s     *Server
	authz *conduitStreamAuthz

	mu       sync.Mutex
	sessions map[*conduitUserStream]*conduitBrokerSession

	// bindMu guards unbind, which releases the binding to the current
	// event publisher.
	bindMu sync.Mutex
	unbind func()

	// testHookBeforeClose, when set, runs in a denying re-check's close
	// just before the broker's registered sessions are closed. Tests only.
	testHookBeforeClose atomic.Pointer[func(*conduitBrokerSession)]
}

// startConduitBrokerRevoke creates and starts this node's broker session
// re-check and binds it to the current event publisher. It returns a stop
// function that stops the re-check and then calls next (the caller's own
// stop function; nil is allowed), so the caller can keep one stop
// function for both. Starting twice keeps the running re-check.
func (s *Server) startConduitBrokerRevoke(ctx context.Context, clk clock.Clock, interval time.Duration, next func()) (stop func()) {
	v := &conduitBrokerRevoker{s: s, sessions: map[*conduitUserStream]*conduitBrokerSession{}}
	v.authz = newConduitStreamAuthz(conduitStreamAuthzConfig{
		Check:           v.check,
		Clock:           clk,
		RecheckInterval: interval,
		// A broker session has no authorization deadline: it stays open
		// while its credential is current.
		UserStreamAuthzMax: -1,
		Metrics:            serverConduitAuthzMetrics{s},
		Logger:             slog.Default().With("subsystem", "hub.conduit", "recheck", "broker_session"),
	})
	if next == nil {
		next = func() {}
	}
	if !s.conduitBrokerRevoke.CompareAndSwap(nil, v) {
		slog.Warn("Conduit: broker session re-check already started")
		return next
	}
	runCtx, cancel := context.WithCancel(ctx)
	v.authz.Start(runCtx)
	s.bindConduitBrokerRevokeEvents()
	return func() {
		cancel()
		s.conduitBrokerRevoke.CompareAndSwap(v, nil)
		next()
	}
}

// bindConduitBrokerRevokeEvents (re)binds the running broker session
// re-check to the current event publisher: the change subscription and,
// for a publisher with a LISTEN connection, the resync on every
// (re)connect. A previous binding is released first. Changes published
// before the binding was in place were not seen, so every broker session
// is re-checked once the binding is in place.
func (s *Server) bindConduitBrokerRevokeEvents() {
	v := s.conduitBrokerRevoke.Load()
	if v == nil {
		return
	}
	v.bindMu.Lock()
	if v.unbind != nil {
		v.unbind()
		v.unbind = nil
	}
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	runCtx := v.authz.runContext()
	if events == nil || runCtx.Err() != nil {
		v.bindMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(runCtx)
	var removeListen func()
	if ln, ok := events.(conduitListenNotifier); ok {
		removeListen = ln.AddOnListen(func() {
			v.authz.Recheck(ctx, conduitAuthzTriggerResync, conduitAuthzMatch{})
		})
	}
	ch, unsubscribe := events.Subscribe(conduitBrokerChangedSubject)
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			if removeListen != nil {
				removeListen()
			}
			unsubscribe()
		})
	}
	v.unbind = release
	v.bindMu.Unlock()

	go func() {
		<-ctx.Done()
		release()
	}()
	if ch != nil {
		q := newConduitNotifyQueue()
		go receiveConduitBrokerChanges(ctx, q, ch)
		go q.work(ctx, v.authz)
	}
	v.authz.Recheck(ctx, conduitAuthzTriggerResync, conduitAuthzMatch{})
}

// receiveConduitBrokerChanges records the broker of every change event
// until ctx ends.
func receiveConduitBrokerChanges(ctx context.Context, q *conduitNotifyQueue, ch <-chan Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if evt.Subject == conduitBrokerChangedSubject {
				q.add(conduitBrokerMatchForEvent(evt))
			}
		}
	}
}

// conduitBrokerMatchForEvent selects the broker sessions a change event
// affects: the named broker's, or every broker session for an event that
// names none or cannot be read.
func conduitBrokerMatchForEvent(evt Event) conduitAuthzMatch {
	var c conduitBrokerChange
	if err := json.Unmarshal(evt.Data, &c); err != nil {
		return conduitAuthzMatch{}
	}
	return conduitBrokerMatch(c.BrokerID)
}

// conduitBrokerMatch selects the tracked sessions of brokerID. Broker
// sessions are tracked with the broker id as their principal id.
func conduitBrokerMatch(brokerID string) conduitAuthzMatch {
	return conduitAuthzMatch{UserID: brokerID}
}

// conduitBrokerCredentialChanged announces a committed change to a broker's
// registration or secret (deregistration, secret rotation or replacement,
// secret status change). It re-checks the broker's sessions on this node
// and notifies the other hub nodes. Call it only after the change has
// committed. It is a no-op while hub.conduit is off.
func (s *Server) conduitBrokerCredentialChanged(brokerID string) {
	if s == nil || brokerID == "" || !s.experimentEnabled(conduitExperiment) {
		return
	}
	if v := s.conduitBrokerRevoke.Load(); v != nil {
		ctx := v.authz.runContext()
		go v.authz.Recheck(ctx, conduitAuthzTriggerNotify, conduitBrokerMatch(brokerID))
	}
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	if events == nil {
		return
	}
	events.PublishRaw(conduitBrokerChangedSubject, conduitBrokerChange{BrokerID: brokerID})
}

// conduitBrokerSessionCtxKey carries the *conduitBrokerSession from
// serveConduitBroker to the post-registration revalidation.
type conduitBrokerSessionCtxKey struct{}

// conduitBrokerAdmissionSecret returns the fingerprint of the broker secret
// that signed the upgrade request r, which authenticateBrokerUpgrade has
// already accepted. The session binds to that verified secret: the
// broker's current secret is read and must verify r's signature, or a 401
// is written and false returned. This check reads only; it records no
// nonce.
func (s *Server) conduitBrokerAdmissionSecret(w http.ResponseWriter, r *http.Request, brokerID string) (conduitBrokerSecretFingerprint, bool) {
	if s.brokerAuthService == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Broker authentication not enabled", nil)
		return conduitBrokerSecretFingerprint{}, false
	}
	secret, err := s.store.GetBrokerSecret(r.Context(), brokerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed, "Invalid broker signature", nil)
			return conduitBrokerSecretFingerprint{}, false
		}
		writeErrorFromErr(w, err, "")
		return conduitBrokerSecretFingerprint{}, false
	}
	canonical := s.brokerAuthService.buildCanonicalString(r, r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderNonce))
	expected := base64.StdEncoding.EncodeToString(computeHMAC(secret.SecretKey, canonical))
	if !hmac.Equal([]byte(r.Header.Get(HeaderSignature)), []byte(expected)) {
		writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed, "Invalid broker signature", nil)
		return conduitBrokerSecretFingerprint{}, false
	}
	return fingerprintBrokerSecret(secret.SecretKey), true
}

// trackConduitBrokerSession registers a broker session this node is about
// to serve for re-checks, bound to the secret fingerprint fp. Call it
// before the session is admitted, so every re-check that runs from then on
// covers the session. It returns the context to serve the session with (it
// carries the session state to the post-registration revalidation) and the
// function that unregisters the session (call it when the session ends).
// While no re-check runs only the context is set.
//
// A re-check that finds the credential no longer current marks the session
// denied and then closes the broker's registered sessions on this node. A
// session that registers after that close finds the mark in its
// post-registration check (conduitRevalidateBrokerCredential) and closes
// there, whatever that check reads.
func (s *Server) trackConduitBrokerSession(ctx context.Context, brokerID string, fp conduitBrokerSecretFingerprint) (context.Context, func()) {
	bs := &conduitBrokerSession{brokerID: brokerID, fp: fp}
	ctx = context.WithValue(ctx, conduitBrokerSessionCtxKey{}, bs)
	v := s.conduitBrokerRevoke.Load()
	if v == nil {
		return ctx, func() {}
	}
	st := &conduitUserStream{
		Kind:     conduitBrokerSessionKind,
		Identity: NewBrokerIdentity(brokerID),
		UserID:   brokerID,
		Admitted: v.authz.cfg.Clock.Now(),
		Close: func(code uint32, reason string) {
			// Mark first: CloseSessionsOf sees only registered
			// sessions, and registration takes the relay's lock
			// before the post-registration check reads the mark.
			bs.denied.Store(true)
			if h := v.testHookBeforeClose.Load(); h != nil {
				(*h)(bs)
			}
			if rt := s.conduit.Load(); rt != nil {
				rt.relay.CloseSessionsOf(registry.PrincipalBroker, brokerID, code, reason)
			}
		},
	}
	v.mu.Lock()
	v.sessions[st] = bs
	v.mu.Unlock()
	untrack := v.authz.Track(st)
	return ctx, func() {
		untrack()
		v.mu.Lock()
		delete(v.sessions, st)
		v.mu.Unlock()
	}
}

// check evaluates one tracked broker session.
func (v *conduitBrokerRevoker) check(ctx context.Context, st *conduitUserStream) (conduitAuthzVerdict, string) {
	v.mu.Lock()
	bs, ok := v.sessions[st]
	v.mu.Unlock()
	if !ok {
		// Untracked meanwhile (the session ended): nothing to act on.
		return conduitAuthzAllowed, "session no longer tracked"
	}
	return v.s.conduitBrokerCredentialVerdict(ctx, bs.brokerID, bs.fp)
}

// conduitBrokerCredentialVerdict evaluates whether brokerID is still
// registered and fp is still its current, active secret. Every read goes to
// s.store, the primary store. A read error is unavailable, never denied.
func (s *Server) conduitBrokerCredentialVerdict(ctx context.Context, brokerID string, fp conduitBrokerSecretFingerprint) (conduitAuthzVerdict, string) {
	_, err := s.store.GetRuntimeBroker(ctx, brokerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return conduitAuthzDenied, "broker deregistered"
	case err != nil:
		return conduitAuthzUnavailable, "broker lookup failed"
	}
	secret, err := s.store.GetBrokerSecret(ctx, brokerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return conduitAuthzDenied, "broker secret removed"
	case err != nil:
		return conduitAuthzUnavailable, "broker secret lookup failed"
	case secret.Status != store.BrokerSecretStatusActive:
		return conduitAuthzDenied, "broker secret not active"
	case !secret.ExpiresAt.IsZero() && time.Now().After(secret.ExpiresAt):
		return conduitAuthzDenied, "broker secret expired"
	}
	if cur := fingerprintBrokerSecret(secret.SecretKey); !hmac.Equal(fp[:], cur[:]) {
		return conduitAuthzDenied, "broker secret replaced"
	}
	return conduitAuthzAllowed, ""
}

// conduitBrokerSessionDenied reports whether ctx carries a broker session
// a re-check has already denied. It reads no store.
func conduitBrokerSessionDenied(ctx context.Context) bool {
	bs, ok := ctx.Value(conduitBrokerSessionCtxKey{}).(*conduitBrokerSession)
	return ok && bs != nil && bs.denied.Load()
}

// conduitRevalidateBrokerCredential runs once a broker session is admitted
// and registered: a session a re-check has already denied, or whose
// admitting secret is no longer the broker's current, active secret, is
// closed with 4401. A check that cannot be evaluated keeps the session (it
// is still tracked, so the next re-check evaluates it). A passing result
// changes nothing: it never undoes a re-check's close.
func (s *Server) conduitRevalidateBrokerCredential(ctx context.Context, brokerID string) error {
	bs, ok := ctx.Value(conduitBrokerSessionCtxKey{}).(*conduitBrokerSession)
	if !ok || bs == nil {
		return nil
	}
	if bs.denied.Load() {
		return errConduitBrokerCredentialChanged
	}
	verdict, detail := s.conduitBrokerCredentialVerdict(ctx, brokerID, bs.fp)
	switch verdict {
	case conduitAuthzDenied:
		return errConduitBrokerCredentialChanged
	case conduitAuthzUnavailable:
		slog.Warn("Conduit: re-checking the runtime broker credential after admission failed; keeping the session", "broker_id", brokerID, "detail", detail)
	}
	return nil
}
