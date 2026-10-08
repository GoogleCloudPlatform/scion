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
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Hub wiring of the user-stream re-check (conduit_stream_authz.go): the
// check itself, the revocation events that trigger it, and the LISTEN
// resync.

// conduitAuthzChangedSubject is the event subject of a change that may
// revoke a user's stream authorization. The payload is a
// conduitAuthzMatch naming the affected user, project or agent (empty
// fields match every stream). It is a non-project subject, so it travels
// on the global channel, and SSE clients cannot subscribe to it.
const conduitAuthzChangedSubject = "conduit.authz.changed"

// conduitAuthzEventPatterns are the subjects that trigger a notify
// re-check: the explicit change event, plus agent and project deletion and
// exposed port changes (which end or narrow a TCP target's authorization).
var conduitAuthzEventPatterns = []string{conduitAuthzChangedSubject, "agent.*.deleted", "agent.*.ports", "project.*.deleted"}

// errConduitAuthzUnavailable marks an authorization decision that could not
// be evaluated (a store or lookup fault), as opposed to a policy deny.
var errConduitAuthzUnavailable = errors.New("conduit authorization could not be evaluated")

// conduitListenNotifier is implemented by event publishers with a LISTEN
// connection that can miss events while it is down
// (PostgresEventPublisher). fn runs each time the connection is
// established and listening.
type conduitListenNotifier interface {
	AddOnListen(fn func()) (remove func())
}

// startConduitStreamAuthz creates and starts the user-stream re-check for
// this node and binds it to the current event publisher. It runs until
// ctx ends or stop is called. It fails if a re-check already runs.
//
// The event publisher may be set later (the server sets it after the
// relay starts): SetEventPublisher rebinds the notify subscription and the
// LISTEN resync to the new publisher.
func (s *Server) startConduitStreamAuthz(ctx context.Context, clk clock.Clock, interval time.Duration) (a *conduitStreamAuthz, stop func(), err error) {
	a = newConduitStreamAuthz(conduitStreamAuthzConfig{
		Check:           s.checkConduitUserStream,
		Clock:           clk,
		RecheckInterval: interval,
		Metrics:         serverConduitAuthzMetrics{s},
		Logger:          slog.Default().With("subsystem", "hub.conduit"),
	})
	if !s.conduitAuthz.CompareAndSwap(nil, a) {
		return nil, nil, errors.New("conduit stream re-check already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.Start(runCtx)
	s.bindConduitAuthzEvents()
	stop = func() {
		cancel()
		s.conduitAuthz.CompareAndSwap(a, nil)
	}
	return a, stop, nil
}

// bindConduitAuthzEvents (re)binds the running re-check to the current
// event publisher: the notify subscription and, for a publisher with a
// LISTEN connection, the resync on every (re)connect. A previous binding is
// released first. Events published before the binding was in place were
// not seen, so every stream is re-checked once the binding is in place.
func (s *Server) bindConduitAuthzEvents() {
	a := s.conduitAuthz.Load()
	if a == nil {
		return
	}
	s.conduitAuthzBindMu.Lock()
	if s.conduitAuthzUnbind != nil {
		s.conduitAuthzUnbind()
		s.conduitAuthzUnbind = nil
	}
	// Read under the bind lock, so concurrent binds install the latest
	// publisher.
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	runCtx := a.runContext()
	if events == nil || runCtx.Err() != nil {
		s.conduitAuthzBindMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(runCtx)
	var removeListen func()
	if ln, ok := events.(conduitListenNotifier); ok {
		removeListen = ln.AddOnListen(func() {
			a.Recheck(ctx, conduitAuthzTriggerResync, conduitAuthzMatch{})
		})
	}
	ch, unsubscribe := events.Subscribe(conduitAuthzEventPatterns...)
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
	s.conduitAuthzUnbind = release
	s.conduitAuthzBindMu.Unlock()

	go func() {
		<-ctx.Done()
		release()
	}()
	if ch != nil {
		q := newConduitNotifyQueue()
		go q.receive(ctx, ch)
		go q.work(ctx, a)
	}
	a.Recheck(ctx, conduitAuthzTriggerResync, conduitAuthzMatch{})
}

// conduitNotifyMaxPending bounds the distinct pending notify matches; past
// it the queue collapses to one re-check of every stream.
const conduitNotifyMaxPending = 256

// conduitNotifyQueue decouples receiving trigger events from re-checking:
// the receiver only records the match, so the subscription keeps draining
// while a re-check runs (the publishers drop events for a subscriber that
// falls behind), and matches that pile up meanwhile are coalesced.
type conduitNotifyQueue struct {
	mu      sync.Mutex
	pending map[conduitAuthzMatch]struct{}
	all     bool
	wake    chan struct{}
}

func newConduitNotifyQueue() *conduitNotifyQueue {
	return &conduitNotifyQueue{pending: map[conduitAuthzMatch]struct{}{}, wake: make(chan struct{}, 1)}
}

// add records m (the zero match selects every stream).
func (q *conduitNotifyQueue) add(m conduitAuthzMatch) {
	q.mu.Lock()
	switch {
	case q.all:
	case m == conduitAuthzMatch{} || len(q.pending) >= conduitNotifyMaxPending:
		q.all = true
		q.pending = map[conduitAuthzMatch]struct{}{}
	default:
		q.pending[m] = struct{}{}
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take returns and clears the pending matches.
func (q *conduitNotifyQueue) take() (all bool, ms []conduitAuthzMatch) {
	q.mu.Lock()
	defer q.mu.Unlock()
	all = q.all
	for m := range q.pending {
		ms = append(ms, m)
	}
	q.all = false
	q.pending = map[conduitAuthzMatch]struct{}{}
	return all, ms
}

// receive records the match of every trigger event until ctx ends.
func (q *conduitNotifyQueue) receive(ctx context.Context, ch <-chan Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if m, ok := conduitAuthzMatchForEvent(evt); ok {
				q.add(m)
			}
		}
	}
}

// work runs the notify re-checks for the pending matches until ctx ends.
func (q *conduitNotifyQueue) work(ctx context.Context, a *conduitStreamAuthz) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		}
		all, ms := q.take()
		if all {
			a.Recheck(ctx, conduitAuthzTriggerNotify, conduitAuthzMatch{})
			continue
		}
		for _, m := range ms {
			a.Recheck(ctx, conduitAuthzTriggerNotify, m)
		}
	}
}

// conduitAuthzMatchForEvent maps a trigger event to the streams it
// affects.
func conduitAuthzMatchForEvent(evt Event) (conduitAuthzMatch, bool) {
	if evt.Subject == conduitAuthzChangedSubject {
		var m conduitAuthzMatch
		if err := json.Unmarshal(evt.Data, &m); err != nil {
			// A malformed payload still means something changed:
			// re-check everything rather than nothing.
			return conduitAuthzMatch{}, true
		}
		return m, true
	}
	parts := strings.Split(evt.Subject, ".")
	if len(parts) == 3 && parts[0] == "agent" && parts[1] != "" && (parts[2] == "deleted" || parts[2] == "ports") {
		return conduitAuthzMatch{AgentID: parts[1]}, true
	}
	if len(parts) == 3 && parts[0] == "project" && parts[1] != "" && parts[2] == "deleted" {
		return conduitAuthzMatch{ProjectID: parts[1]}, true
	}
	return conduitAuthzMatch{}, false
}

// publishConduitAuthzChanged announces a committed change that may revoke
// stream authorization for the streams m selects (zero m: all). Call it
// only after the change has committed, so the re-check it triggers on
// every node reads the new state. It is a no-op while hub.conduit is off.
func (s *Server) publishConduitAuthzChanged(m conduitAuthzMatch) {
	if s == nil || !s.experimentEnabled(conduitExperiment) {
		return
	}
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	if events == nil {
		return
	}
	events.PublishRaw(conduitAuthzChangedSubject, m)
}

// conduitAuthzMatchForRoleBinding selects the streams a change to b may
// affect: the bound user's (any user's, for a group or agent principal),
// within b's project for a project-scoped binding.
func conduitAuthzMatchForRoleBinding(b *store.RoleBinding) conduitAuthzMatch {
	var m conduitAuthzMatch
	if b == nil {
		return m
	}
	if b.PrincipalType == store.RoleBindingPrincipalUser {
		m.UserID = b.PrincipalID
	}
	if b.ScopeType == store.RoleScopeProject {
		m.ProjectID = b.ScopeID
	}
	return m
}

// trackConduitUserStream registers a user-originated stream this node
// holds the leaf of for re-checks, and returns the function that
// unregisters it (call it when the stream ends). Streams opened by other
// principals, or while no re-check runs, are not tracked.
func (s *Server) trackConduitUserStream(st *conduitUserStream) (untrack func()) {
	a := s.conduitAuthz.Load()
	if a == nil || st == nil {
		return func() {}
	}
	ident, ok := st.Identity.(UserIdentity)
	if !ok || isNilIdentity(st.Identity) {
		return func() {}
	}
	st.UserID = ident.ID()
	if st.Admitted.IsZero() {
		st.Admitted = a.cfg.Clock.Now()
	}
	return a.Track(st)
}

// checkConduitUserStream evaluates whether st's principal still holds the
// permission its stream kind requires, against current state: the agent
// row, the user row (active, not deleted) and the same authorization
// decision admission made. Every read goes to s.store, the primary store
// (the hub has no read-replica routing).
//
// A store or authorization lookup failure is unavailable, never denied: a
// deny is acted on only while the store is reachable.
func (s *Server) checkConduitUserStream(ctx context.Context, st *conduitUserStream) (conduitAuthzVerdict, string) {
	agent, err := s.store.GetAgent(ctx, st.AgentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return conduitAuthzTargetGone, "agent not found"
	case err != nil:
		return conduitAuthzUnavailable, "agent lookup failed"
	case !agent.DeletedAt.IsZero():
		return conduitAuthzTargetGone, "agent deleted"
	}

	if conduitIdentityHasUserRow(st.Identity) {
		u, err := s.store.GetUser(ctx, st.UserID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return conduitAuthzDenied, "user not found"
		case err != nil:
			return conduitAuthzUnavailable, "user lookup failed"
		case u.Status == store.UserStatusSuspended:
			return conduitAuthzDenied, "user suspended"
		}
	}

	action := conduitStreamAction(st.Kind)
	if action == "" {
		return conduitAuthzDenied, "unknown stream kind"
	}
	if err := s.authorizeConduitAction(ctx, st.Identity, agent, action); err != nil {
		if errors.Is(err, errConduitAuthzUnavailable) || !s.conduitAuthzStoreReachable(ctx, st) {
			return conduitAuthzUnavailable, "authorization lookup failed"
		}
		return conduitAuthzDenied, "permission no longer held"
	}
	if st.Kind == grant.StreamKindTCP {
		if err := s.authorizeConduitTCPTarget(agent, st.Port); err != nil {
			return conduitAuthzDenied, "port no longer an authorized target"
		}
	}
	return conduitAuthzAllowed, ""
}

// conduitAuthzStoreReachable confirms the store answers after a deny, so
// that a deny observed during a store fault is not acted on.
func (s *Server) conduitAuthzStoreReachable(ctx context.Context, st *conduitUserStream) bool {
	_, err := s.store.GetAgent(ctx, st.AgentID)
	return err == nil && ctx.Err() == nil
}

// conduitIdentityHasUserRow reports whether identity is a local user whose
// store row decides whether it is still active (suspended or deleted).
func conduitIdentityHasUserRow(identity Identity) bool {
	switch identity.(type) {
	case *AuthenticatedUser, *ScopedUserIdentity:
		return true
	default:
		return false
	}
}

// serverConduitAuthzMetrics forwards to the counter set by
// SetConduitStreamAuthzMetrics, if any.
type serverConduitAuthzMetrics struct{ s *Server }

func (m serverConduitAuthzMetrics) RecordConduitStreamAuthz(trigger, outcome, kind string) {
	if r := m.s.conduitAuthzMetrics.Load(); r != nil {
		(*r).RecordConduitStreamAuthz(trigger, outcome, kind)
	}
}

// SetConduitStreamAuthzMetrics wires the scion.hub.conduit.stream_authz
// counter.
func (s *Server) SetConduitStreamAuthzMetrics(m *OTelConduitStreamAuthzMetrics) {
	if m == nil {
		return
	}
	var r conduitStreamAuthzMetrics = m
	s.conduitAuthzMetrics.Store(&r)
}
