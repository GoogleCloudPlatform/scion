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
	"errors"
	"time"
)

// Canonical principal kinds (Phase 1 contract §2).
const (
	PrincipalBroker    = "broker"
	PrincipalAgent     = "agent"
	PrincipalUser      = "user"
	PrincipalRelayPeer = "relay-peer"
)

// Canonical transports (Phase 1 contract §2).
const (
	TransportWS     = "ws"
	TransportGRPC   = "grpc"
	TransportH1Pair = "h1pair"
)

// Default liveness thresholds (design §3.4: relays refresh last_seen every
// 15s and are stale after 60s; sessions are bumped on pong every 30s).
const (
	DefaultRelayStaleAfter   = 60 * time.Second
	DefaultSessionStaleAfter = 90 * time.Second
	// DefaultSessionReapAfter is how old a session's own last_seen must be
	// before ReapStaleSessions removes it (well beyond the 90s staleness).
	DefaultSessionReapAfter = 10 * time.Minute
	// DefaultRelayPruneAfter is how long a relay row with no sessions must
	// have been stale before PruneRelayInstances removes it.
	DefaultRelayPruneAfter = 7 * 24 * time.Hour
	// MinRelayPruneAfter is the smallest explicit horizon PruneRelayInstances
	// accepts. It keeps the Postgres prune/insert cascade window (see
	// PruneRelayInstances) to relays that have missed heartbeats for at
	// least an hour, and keeps the generation seed's clock assumption weak.
	MinRelayPruneAfter = time.Hour
)

var (
	// ErrRelaySuperseded is returned when the caller's relay generation is no
	// longer the stored generation for its instance_id (or the relay row is
	// missing). The caller is a previous incarnation and must stop acting.
	ErrRelaySuperseded = errors.New("conduit registry: relay generation superseded")
	// ErrSessionNotFound is returned when a session row does not exist (for
	// example because it was reaped or replaced).
	ErrSessionNotFound = errors.New("conduit registry: session not found")
	// ErrInvalidInput wraps validation failures.
	ErrInvalidInput = errors.New("conduit registry: invalid input")
	// ErrNotRoutable is returned by Eligible for principal kinds that are
	// never resolved by principal (user, relay-peer).
	ErrNotRoutable = errors.New("conduit registry: principal kind is not routable by principal")
	// ErrIncompleteWant is returned when a Want lacks a field needed to fence
	// (the authoritative incarnation, or the project for an agent). Callers
	// must treat it as "refuse".
	ErrIncompleteWant = errors.New("conduit registry: want is incomplete")
	// ErrUnaddressable is returned by SelfCheck for an empty internal endpoint.
	ErrUnaddressable = errors.New("conduit registry: relay internal endpoint is empty (unaddressable)")
	// ErrSelfCheckMismatch is returned by SelfCheck when the endpoint is
	// answered by a different relay instance.
	ErrSelfCheckMismatch = errors.New("conduit registry: internal endpoint answered by a different relay instance")
)

// RelayInstance is a relay_instances row.
type RelayInstance struct {
	InstanceID       string
	Generation       int64
	InternalEndpoint string // "" = unaddressable (single-node profiles only)
	PublicEndpoint   string // optional
	StartedAt        time.Time
	LastSeen         time.Time
	Draining         bool
}

// TransportLimits mirrors Capabilities.transport_limits (contract §3).
type TransportLimits struct {
	MaxFrame     int64 `json:"max_frame"`
	IdleTimeoutS int64 `json:"idle_timeout_s"`
}

// Capabilities is the structured capabilities document stored in
// conduit_sessions.capabilities (contract §3; snake_case proto field names).
type Capabilities struct {
	StreamKinds         []string        `json:"stream_kinds"`
	RPC                 []string        `json:"rpc"`
	EndpointIncarnation string          `json:"endpoint_incarnation"`
	ExecScope           string          `json:"exec_scope"`
	TransportLimits     TransportLimits `json:"transport_limits"`
	// IncarnationSource says where EndpointIncarnation comes from (design
	// v2.4 §3.4): "launch_id", "generation", or "" for legacy endpoints. The
	// registry only stores and round-trips it; nothing filters on it.
	IncarnationSource string `json:"incarnation_source,omitempty"`
}

// Has reports whether the capabilities advertise name as a stream kind or
// an RPC.
func (c Capabilities) Has(name string) bool {
	for _, k := range c.StreamKinds {
		if k == name {
			return true
		}
	}
	for _, r := range c.RPC {
		if r == name {
			return true
		}
	}
	return false
}

// SessionRecord is a conduit_sessions row. ProjectID and ExecScope use ""
// for SQL NULL.
type SessionRecord struct {
	SessionID           string
	PrincipalKind       string
	PrincipalID         string
	ProjectID           string
	RelayInstanceID     string
	RelayGeneration     int64
	Transport           string
	EndpointIncarnation string
	ExecScope           string
	ConnectionEpoch     int64
	Draining            bool
	Capabilities        Capabilities
	ConnectedAt         time.Time
	LastSeen            time.Time
}

// Want describes what a caller needs from a session. Matching is exact and
// fails closed:
//
//   - ProjectID compares with "" meaning NULL. Agent lookups must set it
//     (empty returns ErrIncompleteWant); broker lookups pass "".
//   - ExecScope compares exactly with "" meaning NULL, so two sessions with
//     different exec_scope are never interchangeable. A caller that needs a
//     stateless capability and genuinely does not care which runtime scope
//     serves it sets AnyExecScope instead. That is an explicit opt-out: a
//     stateful caller that forgets ExecScope matches only unscoped
//     sessions, never a scoped one by accident.
type Want struct {
	ProjectID string
	ExecScope string
	// AnyExecScope disables exec_scope matching (stateless operations
	// only). Setting it together with a non-empty ExecScope is
	// contradictory and returns ErrInvalidInput.
	AnyExecScope bool
	// Incarnation is the authoritative endpoint incarnation, read by the
	// caller from the agent/broker row. Required for agent and broker
	// lookups; an empty value fails closed with ErrIncompleteWant.
	Incarnation string
	// Capability is a stream kind or RPC name the session must advertise;
	// "" means no capability requirement.
	Capability string
}

// Reason classifies why a session is or is not eligible/admissible.
type Reason string

// Reasons returned in Decision.Reason.
const (
	ReasonOK                  Reason = "ok"
	ReasonNotFound            Reason = "not_found"
	ReasonNotRoutable         Reason = "not_routable"
	ReasonRelayMissing        Reason = "relay_missing"
	ReasonRelaySuperseded     Reason = "relay_superseded"
	ReasonRelayStale          Reason = "relay_stale"
	ReasonSessionStale        Reason = "session_stale"
	ReasonDraining            Reason = "draining"
	ReasonProjectMismatch     Reason = "project_mismatch"
	ReasonExecScopeMismatch   Reason = "exec_scope_mismatch"
	ReasonIncarnationMismatch Reason = "incarnation_mismatch"
	ReasonCapabilityMissing   Reason = "capability_missing"
	ReasonEpochObsolete       Reason = "epoch_obsolete"
	ReasonReadError           Reason = "read_error"
)

// Decision is the result of an admission check.
type Decision struct {
	Admissible bool
	Reason     Reason
}
