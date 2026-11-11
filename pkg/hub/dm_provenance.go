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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Server-derived provenance stamping (#1690)
// ---------------------------------------------------------------------------
//
// StampProvenance sets immutable sender and recipient project IDs on a
// store.Message from the authoritative agent records. The project IDs are
// derived from the server-held agent records at admission time — never from
// client-supplied metadata — so that stale or spoofed token claims cannot
// override the canonical provenance.
//
// Both SenderProjectID and RecipientProjectID are always set for agent-to-agent
// DMs. Agents that share a slug across projects are disambiguated by their
// immutable agent ID (already on the message as SenderID/RecipientID).
//
// This function is idempotent: calling it again with the same inputs produces
// the same result.

// StampProvenance stamps server-derived sender and recipient project IDs onto
// a store.Message from the authoritative agent records. The project IDs are
// always taken from the fresh agent records held by the server, never from
// client-supplied metadata.
//
// Precondition: senderAgent and targetAgent must be non-nil and freshly read
// from the store.
func StampProvenance(msg *store.Message, senderAgent, targetAgent *store.Agent) {
	if msg == nil || senderAgent == nil || targetAgent == nil {
		return
	}
	senderProjID := senderAgent.ProjectID
	recipientProjID := targetAgent.ProjectID
	msg.SenderProjectID = &senderProjID
	msg.RecipientProjectID = &recipientProjID
}

// ValidateProvenance checks that the project IDs stamped on a persisted
// message match the authoritative agent records. Returns true when the
// stored provenance agrees with the server-derived values. This is a
// post-persist consistency check (AC-2) — it detects bugs in the stamping
// path, not client-side attacks (those are prevented by StampProvenance
// ignoring client metadata).
func ValidateProvenance(msg *store.Message, senderAgent, targetAgent *store.Agent) bool {
	if msg == nil || senderAgent == nil || targetAgent == nil {
		return false
	}
	if msg.SenderProjectID == nil || msg.RecipientProjectID == nil {
		return false
	}
	return *msg.SenderProjectID == senderAgent.ProjectID &&
		*msg.RecipientProjectID == targetAgent.ProjectID
}
