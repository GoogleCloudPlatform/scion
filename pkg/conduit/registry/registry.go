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

package registry

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Clock is the registry's time source. Inject a fake in tests.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (f ClockFunc) Now() time.Time { return f() }

// Config configures a Registry. Zero values select the defaults.
type Config struct {
	// RelayStaleAfter is how old a relay's last_seen may be before the relay
	// (and therefore all its sessions) is considered dead. Default 60s.
	RelayStaleAfter time.Duration
	// SessionStaleAfter is how old a session's last_seen may be before the
	// session is no longer live. Default 90s.
	SessionStaleAfter time.Duration
	// Clock supplies timestamps for writes and admission checks. Default:
	// the wall clock.
	Clock Clock
}

// Registry implements the Conduit registry logic on top of a Store.
type Registry struct {
	store Store
	cfg   Config
}

// New returns a Registry over store.
func New(store Store, cfg Config) *Registry {
	if cfg.RelayStaleAfter <= 0 {
		cfg.RelayStaleAfter = DefaultRelayStaleAfter
	}
	if cfg.SessionStaleAfter <= 0 {
		cfg.SessionStaleAfter = DefaultSessionStaleAfter
	}
	if cfg.Clock == nil {
		cfg.Clock = ClockFunc(time.Now)
	}
	return &Registry{store: store, cfg: cfg}
}

func (r *Registry) now() time.Time { return r.cfg.Clock.Now().UTC() }

// RegisterRelay registers (or re-registers) a relay instance and returns its
// new generation, which is strictly greater than any generation previously
// returned for the same instance_id. In hosted-HA mode the caller must run
// SelfCheck first and refuse to start if it fails (design §3.4, §3.9).
// Zero StartedAt/LastSeen are filled from the clock.
func (r *Registry) RegisterRelay(ctx context.Context, ri RelayInstance) (int64, error) {
	if ri.InstanceID == "" {
		return 0, fmt.Errorf("%w: empty instance_id", ErrInvalidInput)
	}
	now := r.now()
	if ri.StartedAt.IsZero() {
		ri.StartedAt = now
	}
	if ri.LastSeen.IsZero() {
		ri.LastSeen = now
	}
	return r.store.RegisterRelay(ctx, ri)
}

// SweepOwnOlderGenerations deletes the session rows a previous incarnation
// of instanceID left behind (relay_generation < currentGen). Call it right
// after RegisterRelay.
func (r *Registry) SweepOwnOlderGenerations(ctx context.Context, instanceID string, currentGen int64) (int, error) {
	if instanceID == "" || currentGen <= 0 {
		return 0, fmt.Errorf("%w: sweep needs instance_id and a positive generation", ErrInvalidInput)
	}
	return r.store.DeleteSessionsOfOlderGenerations(ctx, instanceID, currentGen)
}

// HeartbeatRelay refreshes the relay's last_seen. ErrRelaySuperseded means a
// newer generation of this instance registered (or the row is gone): the
// caller must stop acting as this relay.
func (r *Registry) HeartbeatRelay(ctx context.Context, instanceID string, gen int64) error {
	return r.store.HeartbeatRelay(ctx, instanceID, gen, r.now())
}

// SetRelayDraining marks the relay draining (or not). Generation-fenced.
func (r *Registry) SetRelayDraining(ctx context.Context, instanceID string, gen int64, draining bool) error {
	return r.store.SetRelayDraining(ctx, instanceID, gen, draining)
}

// InsertSessionWithNextEpoch validates rec, allocates the principal's next
// connection_epoch and inserts the session row in one transaction. The
// input ConnectionEpoch is ignored; zero ConnectedAt/LastSeen are filled
// from the clock. relay-peer principals are rejected: they never have
// session rows (§3.4).
func (r *Registry) InsertSessionWithNextEpoch(ctx context.Context, rec SessionRecord) (int64, error) {
	if err := validateSession(rec); err != nil {
		return 0, err
	}
	now := r.now()
	if rec.ConnectedAt.IsZero() {
		rec.ConnectedAt = now
	}
	if rec.LastSeen.IsZero() {
		rec.LastSeen = now
	}
	rec.ConnectionEpoch = 0
	return r.store.InsertSessionWithNextEpoch(ctx, rec)
}

func validateSession(rec SessionRecord) error {
	switch rec.PrincipalKind {
	case PrincipalAgent, PrincipalBroker, PrincipalUser:
	case PrincipalRelayPeer:
		return fmt.Errorf("%w: relay-peer principals never have conduit_sessions rows", ErrInvalidInput)
	default:
		return fmt.Errorf("%w: unknown principal kind %q", ErrInvalidInput, rec.PrincipalKind)
	}
	switch rec.Transport {
	case TransportWS, TransportGRPC, TransportH1Pair:
	default:
		return fmt.Errorf("%w: unknown transport %q", ErrInvalidInput, rec.Transport)
	}
	switch {
	case rec.SessionID == "":
		return fmt.Errorf("%w: empty session_id", ErrInvalidInput)
	case rec.PrincipalID == "":
		return fmt.Errorf("%w: empty principal_id", ErrInvalidInput)
	case rec.RelayInstanceID == "":
		return fmt.Errorf("%w: empty relay_instance_id", ErrInvalidInput)
	case rec.RelayGeneration <= 0:
		return fmt.Errorf("%w: relay_generation must be positive", ErrInvalidInput)
	case rec.PrincipalKind != PrincipalUser && rec.EndpointIncarnation == "":
		return fmt.Errorf("%w: %s sessions need an endpoint_incarnation", ErrInvalidInput, rec.PrincipalKind)
	}
	return nil
}

// TouchSession bumps the session's last_seen (on pong). ErrSessionNotFound
// means the row was reaped or replaced; the relay should close the session.
func (r *Registry) TouchSession(ctx context.Context, sessionID string) error {
	return r.store.TouchSession(ctx, sessionID, r.now())
}

// SetSessionDraining marks a session draining: it stops being eligible and
// admissible for new work.
func (r *Registry) SetSessionDraining(ctx context.Context, sessionID string) error {
	return r.store.SetSessionDraining(ctx, sessionID)
}

// DeleteSessionCAS deletes a session row only if it was created by
// relayInstanceID in generation relayGen. A stale generation (e.g. an old
// disconnect arriving after replacement) deletes nothing.
func (r *Registry) DeleteSessionCAS(ctx context.Context, sessionID, relayInstanceID string, relayGen int64) (bool, error) {
	return r.store.DeleteSessionCAS(ctx, sessionID, relayInstanceID, relayGen)
}

// ForgetPrincipalEpoch deletes an agent's epoch counter row when the agent
// itself is deleted (agent uuids are never reused). Broker and user rows are
// kept forever because those IDs are reused; asking to forget them is an
// error.
func (r *Registry) ForgetPrincipalEpoch(ctx context.Context, principalKind, principalID string) error {
	if principalKind != PrincipalAgent {
		return fmt.Errorf("%w: only agent epoch rows may be deleted (got %q)", ErrInvalidInput, principalKind)
	}
	if principalID == "" {
		return fmt.Errorf("%w: empty principal_id", ErrInvalidInput)
	}
	return r.store.DeletePrincipalEpoch(ctx, principalKind, principalID)
}

// ReapStaleRelays is the singleton reaper body: it deletes every session
// whose relay has not been seen for longer than staleAfter (cascading the
// relay's death to its sessions). Relay rows are kept so generations stay
// monotonic. The caller (1d) runs it under leader election. A relay that
// comes back after being reaped finds TouchSession returning
// ErrSessionNotFound for its old sessions and must close them.
func (r *Registry) ReapStaleRelays(ctx context.Context, staleAfter time.Duration, now time.Time) (int, error) {
	if staleAfter <= 0 {
		staleAfter = r.cfg.RelayStaleAfter
	}
	return r.store.DeleteSessionsOfStaleRelays(ctx, now.UTC().Add(-staleAfter))
}

// PruneRelayInstances deletes relay_instances rows that have no sessions and
// whose last_seen is older than olderThan (DefaultRelayPruneAfter when <= 0).
// It bounds the growth of the table when instance ids are unique per process
// start. It is safe because a pruned relay that comes back finds
// HeartbeatRelay/SetRelayDraining/InsertSessionWithNextEpoch fenced with
// ErrRelaySuperseded and must re-register; the horizon is far beyond the
// reaper's, so any session such a relay could still insert would be reaped
// anyway. Run it from the same singleton as ReapStaleRelays.
func (r *Registry) PruneRelayInstances(ctx context.Context, olderThan time.Duration, now time.Time) (int, error) {
	if olderThan <= 0 {
		olderThan = DefaultRelayPruneAfter
	}
	return r.store.DeleteIdleRelays(ctx, now.UTC().Add(-olderThan))
}

// checkWant validates w for a lookup of principalKind. The returned reason
// is used by Admission when it refuses.
func checkWant(principalKind string, w Want) (Reason, error) {
	if w.AnyExecScope && w.ExecScope != "" {
		return ReasonExecScopeMismatch, fmt.Errorf("%w: want sets both ExecScope and AnyExecScope", ErrInvalidInput)
	}
	switch principalKind {
	case PrincipalAgent, PrincipalBroker:
	default:
		return ReasonOK, nil
	}
	if w.Incarnation == "" {
		return ReasonIncarnationMismatch, fmt.Errorf("%w: no authoritative incarnation", ErrIncompleteWant)
	}
	if principalKind == PrincipalAgent && w.ProjectID == "" {
		return ReasonProjectMismatch, fmt.Errorf("%w: agent lookup without project", ErrIncompleteWant)
	}
	return ReasonOK, nil
}

// Eligible returns the sessions of (principalKind, principalID) that may
// serve w, filtered BEFORE ranking (live ∧ ¬draining ∧ project ∧ exec_scope
// ∧ current incarnation ∧ capability ∧ current epoch) and then ranked
// freshest-first (sessions on non-draining relays first, then last_seen,
// then epoch, newest first). Only agent and broker principals are routable;
// other kinds return ErrNotRoutable.
//
// A session on a draining *relay* stays eligible (and admissible) and is
// only ranked after sessions on non-draining relays, so draining a relay
// never makes a principal unreachable before it has reconnected elsewhere.
// Exclusion during a drain happens at the session level: the relay marks
// each session draining (SetSessionDraining) when it sends GoAway, and
// draining sessions are filtered out.
func (r *Registry) Eligible(ctx context.Context, principalKind, principalID string, w Want, now time.Time) ([]SessionRecord, error) {
	switch principalKind {
	case PrincipalAgent, PrincipalBroker:
	default:
		return nil, fmt.Errorf("%w: %q", ErrNotRoutable, principalKind)
	}
	if _, err := checkWant(principalKind, w); err != nil {
		return nil, err
	}
	ps, err := r.store.ListPrincipalSessions(ctx, principalKind, principalID)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	var out []SessionView
	for _, v := range ps.Sessions {
		if r.classify(ps, v, w, now) == ReasonOK {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ad, bd := a.Relay.Draining, b.Relay.Draining; ad != bd {
			return !ad
		}
		if !a.Session.LastSeen.Equal(b.Session.LastSeen) {
			return a.Session.LastSeen.After(b.Session.LastSeen)
		}
		if a.Session.ConnectionEpoch != b.Session.ConnectionEpoch {
			return a.Session.ConnectionEpoch > b.Session.ConnectionEpoch
		}
		return a.Session.SessionID < b.Session.SessionID
	})
	recs := make([]SessionRecord, len(out))
	for i, v := range out {
		recs[i] = v.Session
	}
	return recs, nil
}

// IsAdmissible is the admission fence for StreamOpen and RPC on sessionID.
// It fails closed: on a read error it returns (false, err) and the caller
// must refuse.
func (r *Registry) IsAdmissible(ctx context.Context, sessionID string, w Want) (bool, error) {
	d, err := r.Admission(ctx, sessionID, w)
	return d.Admissible, err
}

// Admission is IsAdmissible with the reason. For agent and broker sessions
// it requires: row present, relay live and of the session's generation,
// session live, not draining, project/exec_scope/incarnation equal to w,
// capability advertised (if w.Capability is set), and the epoch current for
// the principal's kind. For user sessions only presence, liveness and
// draining apply (w is ignored; epochs are audit-only for users).
func (r *Registry) Admission(ctx context.Context, sessionID string, w Want) (Decision, error) {
	ps, found, err := r.store.ListPrincipalSessionsBySession(ctx, sessionID)
	if err != nil {
		return Decision{Reason: ReasonReadError}, err
	}
	if !found {
		return Decision{Reason: ReasonNotFound}, nil
	}
	var v SessionView
	ok := false
	for _, s := range ps.Sessions {
		if s.Session.SessionID == sessionID {
			v, ok = s, true
			break
		}
	}
	if !ok {
		return Decision{Reason: ReasonNotFound}, nil
	}
	if reason, err := checkWant(v.Session.PrincipalKind, w); err != nil {
		return Decision{Reason: reason}, err
	}
	reason := r.classify(ps, v, w, r.now())
	return Decision{Admissible: reason == ReasonOK, Reason: reason}, nil
}

// classify applies the full filter to one session. It is the single policy
// function shared by Eligible and Admission.
func (r *Registry) classify(ps PrincipalSessions, v SessionView, w Want, now time.Time) Reason {
	s := v.Session
	switch s.PrincipalKind {
	case PrincipalAgent, PrincipalBroker, PrincipalUser:
	default:
		return ReasonNotRoutable
	}
	if reason := r.liveness(v, now); reason != ReasonOK {
		return reason
	}
	if s.Draining {
		return ReasonDraining
	}
	if s.PrincipalKind == PrincipalUser {
		// Users are never resolved by principal; the hub splices to the
		// exact session_id. No Want matching, no epoch currency.
		return ReasonOK
	}
	switch {
	case s.ProjectID != w.ProjectID:
		return ReasonProjectMismatch
	case !w.AnyExecScope && s.ExecScope != w.ExecScope:
		return ReasonExecScopeMismatch
	case w.Incarnation == "" || s.EndpointIncarnation != w.Incarnation:
		return ReasonIncarnationMismatch
	case w.Capability != "" && !s.Capabilities.Has(w.Capability):
		return ReasonCapabilityMissing
	case !EpochCurrent(ps, s):
		return ReasonEpochObsolete
	}
	return ReasonOK
}

func (r *Registry) liveness(v SessionView, now time.Time) Reason {
	switch {
	case v.Relay == nil:
		return ReasonRelayMissing
	case v.Relay.Generation != v.Session.RelayGeneration:
		return ReasonRelaySuperseded
	case now.Sub(v.Relay.LastSeen) > r.cfg.RelayStaleAfter:
		return ReasonRelayStale
	case now.Sub(v.Session.LastSeen) > r.cfg.SessionStaleAfter:
		return ReasonSessionStale
	}
	return ReasonOK
}

// EpochCurrent reports whether s holds the current connection_epoch for its
// principal, per the kind-specific rule of design §3.4 ("Epoch currency by
// principal kind"):
//
//   - agent: s.ConnectionEpoch == the principal's durable counter, so the
//     newest connection fences every older one.
//   - broker: no other session of the same broker on the same relay
//     instance has a higher epoch (a reconnect to the same relay supersedes
//     its predecessor; sessions on other relays stay current).
//   - user: always true (audit-only epoch).
//   - relay-peer / unknown: false.
func EpochCurrent(ps PrincipalSessions, s SessionRecord) bool {
	switch s.PrincipalKind {
	case PrincipalAgent:
		return ps.CurrentEpoch > 0 && s.ConnectionEpoch == ps.CurrentEpoch
	case PrincipalBroker:
		for _, o := range ps.Sessions {
			if o.Session.RelayInstanceID == s.RelayInstanceID && o.Session.ConnectionEpoch > s.ConnectionEpoch {
				return false
			}
		}
		return true
	case PrincipalUser:
		return true
	default:
		return false
	}
}
