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

package store

import "time"

// DeletionPredicate is the condition UpdateAgentDeletion evaluates against
// the current row inside its transaction (design ptone/scion#2483 §2.1).
// Every set term must hold (they are ANDed); an unset term is ignored.
type DeletionPredicate struct {
	// Claim, when set, requires deletion_claim == *Claim (the caller still
	// holds its claim).
	Claim *int64
	// States, when non-empty, requires deletion_state to be one of these
	// values. "" matches a row with no delete marker (including NULL).
	States []string
	// DeletedAtNull requires deleted_at IS NULL.
	DeletedAtNull bool
	// LeaseExpiredBefore, when set, requires deletion_lease_at to be set and
	// strictly before *LeaseExpiredBefore.
	LeaseExpiredBefore *time.Time
}

// Matches reports whether a satisfies p. It is the reference semantics every
// UpdateAgentDeletion implementation applies to the row it read inside its
// transaction.
func (p DeletionPredicate) Matches(a *Agent) bool {
	if a == nil {
		return false
	}
	if p.Claim != nil && a.DeletionClaim != *p.Claim {
		return false
	}
	if len(p.States) > 0 {
		ok := false
		for _, s := range p.States {
			if a.DeletionState == s {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if p.DeletedAtNull && !a.DeletedAt.IsZero() {
		return false
	}
	if p.LeaseExpiredBefore != nil {
		if a.DeletionLeaseAt == nil || !a.DeletionLeaseAt.Before(*p.LeaseExpiredBefore) {
			return false
		}
	}
	return true
}

// DeletionFields is the set of changes UpdateAgentDeletion applies when its
// predicate holds. A nil pointer leaves the column unchanged; a pointer to
// the zero value writes it ("" for strings). Time columns use a separate
// Clear flag because nil means "unchanged".
//
// Besides the deletion_* columns, a delete may also move the agent's phase
// and activity (the claim writes stopping for an active agent; a failed
// delete restores the prior state), so Phase and Activity are writable here
// too. Derive lets a caller compute the change from the locked current row
// (for example "capture prior only on a fresh claim"); it runs inside the
// transaction after the predicate has matched, and may modify the fields
// before they are written.
type DeletionFields struct {
	State *string
	// Claim sets deletion_claim; BumpClaim increments it (claim+1) and wins
	// over Claim when both are set.
	Claim     *int64
	BumpClaim bool

	LeaseAt        *time.Time
	ClearLeaseAt   bool
	StartedAt      *time.Time
	ClearStartedAt bool
	FailedAt       *time.Time
	ClearFailedAt  bool

	Code    *string
	Error   *string
	Prior   *string
	Request *string

	Phase    *string
	Activity *string

	// Derive, when set, is called with the current row (as read inside the
	// transaction) after the predicate matched, before the write.
	Derive func(current *Agent, f *DeletionFields)
}
