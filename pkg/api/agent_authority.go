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

package api

import "time"

// Agent authority states. The hub computes the state when it reads the
// agent, from the agent's recorded delegation chain; nothing stores it.
const (
	// AgentAuthorityLive: the agent's chain reaches an active user.
	AgentAuthorityLive = "live"
	// AgentAuthorityOrphaned: the agent has an active delegation, but its
	// chain reaches a principal that is deleted, not active or without
	// authority of its own. Every decision that needs the chain denies.
	AgentAuthorityOrphaned = "orphaned"
	// AgentAuthorityRevoked: the agent has no active delegation, and the
	// last one was deactivated with a recorded cause.
	AgentAuthorityRevoked = "revoked"
	// AgentAuthorityUnrecorded: the agent's delegation was never recorded
	// (an agent from before delegations were recorded), or its last
	// delegation was deactivated without a recorded cause. It is not
	// reported as orphaned or revoked.
	AgentAuthorityUnrecorded = "unrecorded"
)

// Agent authority causes computed from the chain. A revoked agent instead
// carries the deactivation cause recorded on its last delegation, for
// example "user_delete_cascade" or "delegator_deleted".
const (
	// AgentAuthorityCauseDelegatorDeleted: a delegator on the chain is a
	// user or agent that is deleted.
	AgentAuthorityCauseDelegatorDeleted = "delegator_deleted"
	// AgentAuthorityCauseDelegatorInactive: the user at the root of the
	// chain exists but is not active (for example suspended, or being
	// deleted).
	AgentAuthorityCauseDelegatorInactive = "delegator_inactive"
	// AgentAuthorityCauseDelegatorRevoked: an agent delegator on the chain
	// has no active delegation of its own.
	AgentAuthorityCauseDelegatorRevoked = "delegator_revoked"
	// AgentAuthorityCauseChainInvalid: the recorded chain cannot be
	// resolved (more than one active delegation, a cycle, a mismatch or
	// too many hops).
	AgentAuthorityCauseChainInvalid = "chain_invalid"
)

// AgentAuthority is the authority state of an agent, as the hub computes
// it at read. It is descriptive only and never an authority input.
type AgentAuthority struct {
	State string `json:"state"`
	Cause string `json:"cause,omitempty"`
	// Principal is the principal the cause names. Its ID is set only for
	// a reader who may read that principal.
	Principal *AgentAuthorityPrincipal `json:"principal,omitempty"`
	// Since is when the cause took effect, when the hub knows it.
	Since *time.Time `json:"since,omitempty"`
}

// AgentAuthorityPrincipal is the principal an AgentAuthority cause names.
type AgentAuthorityPrincipal struct {
	Kind    string `json:"kind"` // "user" or "agent"
	ID      string `json:"id,omitempty"`
	Deleted bool   `json:"deleted"`
}

// Flagged reports whether the state is one an operator should act on:
// orphaned or revoked. A nil authority is not flagged.
func (a *AgentAuthority) Flagged() bool {
	return a != nil && (a.State == AgentAuthorityOrphaned || a.State == AgentAuthorityRevoked)
}
