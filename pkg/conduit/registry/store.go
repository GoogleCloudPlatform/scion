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
	"time"
)

// SessionView is a session together with the relay row it references
// (Relay is nil if the relay row is missing).
type SessionView struct {
	Session SessionRecord
	Relay   *RelayInstance
}

// PrincipalSessions is a consistent snapshot of one principal's registry
// state: its durable epoch counter and every session row it holds.
type PrincipalSessions struct {
	PrincipalKind string
	PrincipalID   string
	// CurrentEpoch is the principal's conduit_principal_epochs value, 0 if
	// no row exists.
	CurrentEpoch int64
	Sessions     []SessionView
}

// Store is the persistence contract for the registry. Implementations must
// keep the Postgres and SQLite behaviour identical; the production
// implementation is entadapter.ConduitRegistryStore. All times are passed in
// by the Registry (from its Clock) so tests can control them.
type Store interface {
	// RegisterRelay upserts the relay row and atomically assigns the next
	// generation (1 for a new instance_id, stored+1 otherwise), resetting
	// endpoints, started_at, last_seen and draining=false. It returns the
	// new generation.
	RegisterRelay(ctx context.Context, r RelayInstance) (int64, error)
	// HeartbeatRelay sets last_seen iff the stored generation equals gen;
	// otherwise (or if the row is missing) it returns ErrRelaySuperseded.
	HeartbeatRelay(ctx context.Context, instanceID string, gen int64, now time.Time) error
	// SetRelayDraining sets draining iff the stored generation equals gen;
	// otherwise it returns ErrRelaySuperseded.
	SetRelayDraining(ctx context.Context, instanceID string, gen int64, draining bool) error
	// DeleteSessionsOfOlderGenerations deletes session rows with this
	// relay_instance_id and relay_generation < currentGen.
	DeleteSessionsOfOlderGenerations(ctx context.Context, instanceID string, currentGen int64) (int, error)
	// InsertSessionWithNextEpoch, in ONE transaction: verifies the relay row
	// exists with generation == s.RelayGeneration (else ErrRelaySuperseded),
	// bumps the principal's epoch with upsert … RETURNING, and inserts the
	// session row with that epoch (s.ConnectionEpoch is ignored). It returns
	// the allocated epoch.
	InsertSessionWithNextEpoch(ctx context.Context, s SessionRecord) (int64, error)
	// TouchSession sets last_seen; ErrSessionNotFound if the row is gone.
	TouchSession(ctx context.Context, sessionID string, now time.Time) error
	// SetSessionDraining sets draining=true; ErrSessionNotFound if gone.
	SetSessionDraining(ctx context.Context, sessionID string) error
	// DeleteSessionCAS deletes the row iff all three of session_id,
	// relay_instance_id and relay_generation match.
	DeleteSessionCAS(ctx context.Context, sessionID, relayInstanceID string, relayGen int64) (bool, error)
	// ListPrincipalSessions returns a consistent snapshot of the principal's
	// epoch counter and sessions (with their relay rows).
	ListPrincipalSessions(ctx context.Context, principalKind, principalID string) (PrincipalSessions, error)
	// ListPrincipalSessionsBySession is ListPrincipalSessions for the
	// principal owning sessionID; found is false if the session is missing.
	ListPrincipalSessionsBySession(ctx context.Context, sessionID string) (ps PrincipalSessions, found bool, err error)
	// DeleteSessionsOfStaleRelays deletes every session whose relay's
	// last_seen is before staleBefore, in one statement, and returns the
	// number of sessions deleted. Relay rows are kept.
	DeleteSessionsOfStaleRelays(ctx context.Context, staleBefore time.Time) (int, error)
	// DeleteStaleSessions deletes, in one statement, every session whose
	// own last_seen is before staleBefore, regardless of its relay, and
	// returns the number deleted. It never touches epoch rows.
	DeleteStaleSessions(ctx context.Context, staleBefore time.Time) (int, error)
	// DeleteIdleRelays deletes relay rows with no sessions whose last_seen
	// is before staleBefore and returns how many were deleted.
	DeleteIdleRelays(ctx context.Context, staleBefore time.Time) (int, error)
	// DeletePrincipalEpoch removes a principal's epoch row (agent deletion).
	DeletePrincipalEpoch(ctx context.Context, principalKind, principalID string) error
}
