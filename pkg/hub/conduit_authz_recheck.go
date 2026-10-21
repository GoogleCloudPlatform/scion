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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
		// The deadline bounds every user stream for its whole life, so
		// it is read from the server configuration (0 = 8h).
		UserStreamAuthzMax: s.config.ConduitUserStreamAuthzMax,
		Metrics:            serverConduitAuthzMetrics{s},
		Logger:             slog.Default().With("subsystem", "hub.conduit"),
		Renew:              s.renewConduitUserStream,
		Enforce: func() bool {
			return s.conduitTargetStreamDeadlines() == config.ConduitTargetStreamDeadlinesEnforce
		},
		// A sweep shares broker reads among its checks (conduitBrokerMemo).
		SweepContext: withConduitBrokerMemo,
	})
	if !s.conduitAuthz.CompareAndSwap(nil, a) {
		return nil, nil, errors.New("conduit stream re-check already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.Start(runCtx)
	s.bindConduitAuthzEvents()
	s.bindConduitGrantKeyEvents()
	go s.runConduitGrantKeySettle(runCtx, a.cfg.Clock)
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
			a.Recheck(withConduitBrokerMemo(ctx), conduitAuthzTriggerResync, conduitAuthzMatch{})
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
	// A resync is a full pass: like a sweep, it shares broker reads.
	a.Recheck(withConduitBrokerMemo(ctx), conduitAuthzTriggerResync, conduitAuthzMatch{})
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

// checkConduitUserStream is the in-process entry point of the stream
// evaluation (design v2.21.40 §3.9, One evaluation, three entry points):
// it evaluates st's principal against current state with
// evaluateConduitStream, from what this node recorded at open (the
// principal, the agent and its project, the kind, origin and port), and
// adds no decision of its own. Every read goes to s.store, the primary
// store (the hub has no read-replica routing).
//
// A store or authorization lookup failure is unavailable, never denied: a
// deny is acted on only while the store answers for the agent
// (conduitConfirmAgentPresent).
func (s *Server) checkConduitUserStream(ctx context.Context, st *conduitUserStream) (conduitAuthzVerdict, string) {
	verdict, reason, _ := s.evaluateConduitStream(ctx, s.recordedStreamEval(ctx, st))
	return verdict, reason
}

// conduitStreamActions returns the actions a user stream requires, in the
// order they are evaluated: the kind's action (conduitStreamAction), then
// ActionTunnel for a stream opened through the tunnel path. It returns nil
// for an unknown kind (denied).
func conduitStreamActions(kind string, viaTunnel bool) []Action {
	action := conduitStreamAction(kind)
	if action == "" {
		return nil
	}
	if viaTunnel {
		return []Action{action, ActionTunnel}
	}
	return []Action{action}
}

// errConduitPrincipalInactive marks a deny by the user-status rule
// (conduitUserStatus).
var errConduitPrincipalInactive = errors.New("conduit principal is not active")

// errConduitUserStatusUnavailable marks a user-status lookup that failed.
var errConduitUserStatusUnavailable = errors.New("user status")

// conduitUserStreamAuthz is the input to authorizeConduitUserStream.
type conduitUserStreamAuthz struct {
	// Identity is the principal that opens (or opened) the stream.
	Identity Identity
	// Agent is the agent the stream reaches, as currently stored.
	Agent *store.Agent
	// Kind is the grant stream kind.
	Kind string
	// ViaTunnel marks a stream opened through the tunnel path.
	ViaTunnel bool
	// Port is the agent-local port of a tcp stream.
	Port int
	// DeferTCPTarget leaves the tcp port check to the caller. Only the
	// tunnel open sets it, so that a stopped agent is reported before the
	// port it no longer exposes. Re-checks never set it.
	DeferTCPTarget bool
	// ConfirmDeny, if set, is asked before an action deny is reported;
	// false makes the decision unavailable instead.
	ConfirmDeny func() bool
}

// authorizeConduitUserStream decides a user stream against current state.
// The tunnel open path and every re-check of an open user stream decide
// with it, so a re-check evaluates the same authorization as the open:
// the principal (a user identity, by its id), the user-status rule
// (conduitUserStatus), every action conduitStreamActions requires, and for
// tcp the agent-local target port.
//
// It returns conduitAuthzAllowed, conduitAuthzDenied or
// conduitAuthzUnavailable, the re-check's reason, and the cause: a deny by
// the user-status rule wraps errConduitPrincipalInactive, a user lookup
// that failed wraps errConduitUserStatusUnavailable, an authorization
// decision that could not be evaluated wraps errConduitAuthzUnavailable,
// and a policy deny is the error of the check that denied.
func (s *Server) authorizeConduitUserStream(ctx context.Context, req conduitUserStreamAuthz) (conduitAuthzVerdict, string, error) {
	ident, ok := req.Identity.(UserIdentity)
	if !ok || isNilIdentity(req.Identity) {
		return conduitAuthzDenied, "identity may not open streams", fmt.Errorf("%w: identity may not open streams", errConduitForbidden)
	}
	switch verdict, reason := s.conduitUserStatus(ctx, req.Identity, ident.ID()); verdict {
	case conduitAuthzAllowed:
	case conduitAuthzDenied:
		return verdict, reason, fmt.Errorf("%w: %s", errConduitPrincipalInactive, reason)
	default:
		return verdict, reason, fmt.Errorf("%w: %s", errConduitUserStatusUnavailable, reason)
	}

	actions := conduitStreamActions(req.Kind, req.ViaTunnel)
	if len(actions) == 0 {
		return conduitAuthzDenied, "unknown stream kind", fmt.Errorf("%w: unknown stream kind %q", errConduitForbidden, req.Kind)
	}
	authorize := s.authorizeConduitAction
	if hook := s.conduitAuthorizeActionTestHook; hook != nil {
		authorize = hook
	}
	for _, action := range actions {
		if err := authorize(ctx, req.Identity, req.Agent, action); err != nil {
			if errors.Is(err, errConduitAuthzUnavailable) {
				return conduitAuthzUnavailable, "authorization lookup failed", err
			}
			if req.ConfirmDeny != nil && !req.ConfirmDeny() {
				return conduitAuthzUnavailable, "authorization lookup failed", fmt.Errorf("%w: deny not confirmed: %w", errConduitAuthzUnavailable, err)
			}
			return conduitAuthzDenied, "permission no longer held", err
		}
	}
	if req.Kind == grant.StreamKindTCP && !req.DeferTCPTarget {
		if err := s.authorizeConduitTCPTarget(req.Agent, req.Port); err != nil {
			return conduitAuthzDenied, "port no longer an authorized target", err
		}
	}
	return conduitAuthzAllowed, "", nil
}

// conduitUserStatus is the user-status rule of a user principal: the
// account-status rule (checkUserAccount) applied to a local user
// (conduitIdentityHasUserRow), the development user included. A user whose
// row is gone or not active is denied, and a failed lookup is unavailable.
// Identities without a user row are allowed. It reads no agent, so the
// tunnel path applies it before the target is resolved.
func (s *Server) conduitUserStatus(ctx context.Context, identity Identity, userID string) (conduitAuthzVerdict, string) {
	if !conduitIdentityHasUserRow(identity) {
		return conduitAuthzAllowed, ""
	}
	user, err := checkUserAccount(ctx, s.store, PrincipalContext{Kind: PrincipalKindUser, ID: userID})
	switch refusal := classifyUserAccount(user, err); refusal {
	case accountAllowed:
		return conduitAuthzAllowed, ""
	case accountLookupFault:
		return conduitAuthzUnavailable, accountRefusalReason(refusal)
	default:
		return conduitAuthzDenied, accountRefusalReason(refusal)
	}
}

// conduitIdentityHasUserRow reports whether identity is a local user whose
// store row decides whether it is still active. The development user has a
// row like any other local user and is checked the same way.
func conduitIdentityHasUserRow(identity Identity) bool {
	switch identity.(type) {
	case *AuthenticatedUser, *ScopedUserIdentity, *DevUser:
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
