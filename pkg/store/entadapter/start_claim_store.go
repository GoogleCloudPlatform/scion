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

package entadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file implements the start-claim writers. Like the run intent writers
// they reuse the launch store's hand-built transaction (beginLaunchTx) and
// store clock (storeNow): each method locks the agent row (FOR UPDATE on
// Postgres; SQLite write transactions are serialised), reads the clock once,
// evaluates its predicate against the locked row and the clock in Go, and
// writes only when it holds. So a lease or hold comparison never depends on
// a hub replica's clock, and two writers to one row are ordered by the lock.
// None of them touches state_version.

// claimTimeResolution is the precision claim times are stored at (the
// Postgres timestamp precision), so a value written compares equal to the
// value read back on either backend.
const claimTimeResolution = time.Microsecond

func claimTime(t time.Time) time.Time { return t.UTC().Truncate(claimTimeResolution) }

// lockedAgentFn runs with the locked row and the store clock. It returns
// whether to commit.
type lockedAgentFn func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error)

// withLockedAgent runs fn in one transaction holding agentID's row lock.
func (s *AgentStore) withLockedAgent(ctx context.Context, agentID string, fn lockedAgentFn) error {
	uid, err := parseUUID(agentID)
	if err != nil {
		return err
	}
	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return err
	}
	defer ltx.cleanup()
	isPG := s.dialect(ctx) == dialect.Postgres
	committed := false
	defer func() {
		if !committed {
			_ = ltx.tx.Rollback()
		}
	}()

	q := ltx.client.Agent.Query().Where(agent.IDEQ(uid))
	if isPG {
		q = q.ForUpdate()
	}
	row, err := q.Only(ctx)
	if err != nil {
		return mapError(err)
	}
	now, err := storeNow(ctx, ltx.tx, isPG)
	if err != nil {
		return err
	}
	commit, err := fn(ctx, ltx.client, row, claimTime(now))
	if err != nil || !commit {
		return err
	}
	if err := ltx.tx.Commit(); err != nil {
		return fmt.Errorf("start claim: commit: %w", err)
	}
	committed = true
	return nil
}

// claimUpdate starts an update of row that keeps agents.updated unchanged:
// claim bookkeeping is not an agent change, and updated feeds sorting and
// activity displays.
func claimUpdate(c *ent.Client, row *ent.Agent) *ent.AgentUpdateOne {
	return c.Agent.UpdateOneID(row.ID).SetUpdated(row.Updated)
}

// heldClaimError returns the claim row holds as a ClaimHeldError, or nil.
func heldClaimError(row *ent.Agent) *store.ClaimHeldError {
	if row.StartClaimID == nil {
		return nil
	}
	return store.HeldClaimFromAgent(entAgentToStore(row))
}

// claimEligible reports whether row may take a claim apart from the
// no-claim-held condition and a delete holding the row (checked first, see
// ClaimAgentStart): not deleted and no reincarnation in flight. A delete
// whose lease expired (abandoned) does not hold the row.
func claimEligible(row *ent.Agent) bool {
	if row.DeletedAt != nil {
		return false
	}
	switch row.ReincarnationState {
	case store.ReincarnationStateNone, store.ReincarnationStateFailed:
	default:
		return false
	}
	return true
}

// clearStartClaim adds the setters that clear every start_claim_* column.
func clearStartClaim(u *ent.AgentUpdateOne) *ent.AgentUpdateOne {
	return u.ClearStartClaimID().
		SetStartClaimKind("").
		SetStartClaimState("").
		SetStartClaimOwner("").
		SetStartClaimTarget("").
		ClearStartClaimAt().
		ClearStartClaimLeaseUntil().
		ClearStartClaimUnconfirmedAt().
		ClearStartClaimHoldUntil().
		SetStartClaimLaunchID("")
}

// demoteStartClaim adds the setters that move a claim to unconfirmed at now.
// A zero hold leaves hold_until unset (see StartClaimHolds.HoldExpiry).
func demoteStartClaim(u *ent.AgentUpdateOne, now time.Time, hold time.Duration) *ent.AgentUpdateOne {
	u.SetStartClaimState(string(store.StartClaimUnconfirmed)).
		SetStartClaimUnconfirmedAt(now)
	if hold > 0 {
		u.SetStartClaimHoldUntil(now.Add(hold))
	} else {
		u.ClearStartClaimHoldUntil()
	}
	return u
}

// holdsClaim reports whether row holds claimID.
func holdsClaim(row *ent.Agent, claimID string) bool {
	return claimID != "" && row.StartClaimID != nil && *row.StartClaimID == claimID
}

// holdsLiveClaim is the holder predicate shared by renew, mark-unconfirmed
// and release: the same claim and owner, live, with an unexpired lease.
func holdsLiveClaim(row *ent.Agent, claimID, owner string, now time.Time) bool {
	return holdsClaim(row, claimID) &&
		row.StartClaimOwner == owner &&
		row.StartClaimState == string(store.StartClaimLive) &&
		row.StartClaimLeaseUntil != nil && row.StartClaimLeaseUntil.After(now)
}

func launchActive(row *ent.Agent) bool { return row.LaunchState == store.LaunchStateActive }

// ClaimAgentStart implements store.AgentStore.ClaimAgentStart.
func (s *AgentStore) ClaimAgentStart(ctx context.Context, agentID, owner string, kind store.StartClaimKind, target string, ttl time.Duration) (store.StartClaim, error) {
	if !kind.Valid() || kind == store.StartClaimStop {
		return store.StartClaim{}, fmt.Errorf("%w: start claim kind %q", store.ErrInvalidInput, kind)
	}
	if ttl <= 0 || owner == "" {
		return store.StartClaim{}, fmt.Errorf("%w: start claim needs an owner and a positive lease", store.ErrInvalidInput)
	}
	var claim store.StartClaim
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		// A start on a row a delete holds, or a soft-deleted row, is
		// refused exactly as the running-intent write refuses it.
		if row.DeletedAt != nil || store.DeletionHoldsRow(row.DeletionState, row.DeletionLeaseAt, time.Now()) {
			return false, store.ErrDeleteInProgress
		}
		if held := heldClaimError(row); held != nil {
			return false, held
		}
		if !claimEligible(row) {
			return false, store.ErrClaimPredicate
		}
		intentAt := nextRunIntentAt(now, row.RunIntentAt)
		claim = store.StartClaim{
			ID:          uuid.NewString(),
			Kind:        kind,
			Owner:       owner,
			Target:      target,
			At:          now,
			LeaseUntil:  now.Add(ttl),
			RunIntentAt: intentAt,
		}
		if _, err := claimUpdate(c, row).
			SetStartClaimID(claim.ID).
			SetStartClaimKind(string(kind)).
			SetStartClaimState(string(store.StartClaimLive)).
			SetStartClaimOwner(owner).
			SetStartClaimTarget(target).
			SetStartClaimAt(claim.At).
			SetStartClaimLeaseUntil(claim.LeaseUntil).
			ClearStartClaimUnconfirmedAt().
			ClearStartClaimHoldUntil().
			SetStartClaimLaunchID("").
			SetRunIntent(string(store.RunIntentRunning)).
			SetRunIntentAt(intentAt).
			SetRunIntentMarkedAt(intentAt).
			Save(ctx); err != nil {
			return false, mapError(err)
		}
		return true, nil
	})
	if err != nil {
		return store.StartClaim{}, err
	}
	return claim, nil
}

// ClaimAgentStop implements store.AgentStore.ClaimAgentStop.
func (s *AgentStore) ClaimAgentStop(ctx context.Context, agentID, owner string, intentAt time.Time, ttl time.Duration) (store.StartClaim, error) {
	if ttl <= 0 || owner == "" {
		return store.StartClaim{}, fmt.Errorf("%w: stop claim needs an owner and a positive lease", store.ErrInvalidInput)
	}
	var claim store.StartClaim
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if row.DeletedAt != nil {
			return false, store.ErrClaimPredicate
		}
		if held := heldClaimError(row); held != nil {
			return false, held
		}
		if !entAgentToStore(row).RunIntentMatches(store.RunIntentStopped, intentAt) {
			return false, store.ErrClaimPredicate
		}
		claim = store.StartClaim{
			ID:          uuid.NewString(),
			Kind:        store.StartClaimStop,
			Owner:       owner,
			At:          now,
			LeaseUntil:  now.Add(ttl),
			RunIntentAt: store.NormalizeRunIntentTime(intentAt),
		}
		if _, err := claimUpdate(c, row).
			SetStartClaimID(claim.ID).
			SetStartClaimKind(string(store.StartClaimStop)).
			SetStartClaimState(string(store.StartClaimLive)).
			SetStartClaimOwner(owner).
			SetStartClaimTarget("").
			SetStartClaimAt(claim.At).
			SetStartClaimLeaseUntil(claim.LeaseUntil).
			ClearStartClaimUnconfirmedAt().
			ClearStartClaimHoldUntil().
			SetStartClaimLaunchID("").
			Save(ctx); err != nil {
			return false, mapError(err)
		}
		return true, nil
	})
	if err != nil {
		return store.StartClaim{}, err
	}
	return claim, nil
}

// RenewAgentStart implements store.AgentStore.RenewAgentStart.
func (s *AgentStore) RenewAgentStart(ctx context.Context, agentID, claimID, owner string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("%w: lease must be positive", store.ErrInvalidInput)
	}
	held := false
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !holdsLiveClaim(row, claimID, owner, now) {
			return false, nil
		}
		if _, err := claimUpdate(c, row).SetStartClaimLeaseUntil(now.Add(ttl)).Save(ctx); err != nil {
			return false, mapError(err)
		}
		held = true
		return true, nil
	})
	return held, notFoundAsLost(err)
}

// MarkStartUnconfirmed implements store.AgentStore.MarkStartUnconfirmed.
func (s *AgentStore) MarkStartUnconfirmed(ctx context.Context, agentID, claimID, owner string, hold time.Duration) (bool, error) {
	if hold <= 0 {
		return false, fmt.Errorf("%w: hold must be positive", store.ErrInvalidInput)
	}
	held := false
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !holdsLiveClaim(row, claimID, owner, now) {
			return false, nil
		}
		if _, err := demoteStartClaim(claimUpdate(c, row), now, hold).Save(ctx); err != nil {
			return false, mapError(err)
		}
		held = true
		return true, nil
	})
	return held, notFoundAsLost(err)
}

// ReleaseAgentStart implements store.AgentStore.ReleaseAgentStart.
func (s *AgentStore) ReleaseAgentStart(ctx context.Context, agentID, claimID, owner string) (bool, error) {
	held := false
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !holdsLiveClaim(row, claimID, owner, now) {
			return false, nil
		}
		if _, err := clearStartClaim(claimUpdate(c, row)).Save(ctx); err != nil {
			return false, mapError(err)
		}
		held = true
		return true, nil
	})
	return held, notFoundAsLost(err)
}

// ReleaseSupersededStart implements store.AgentStore.ReleaseSupersededStart.
func (s *AgentStore) ReleaseSupersededStart(ctx context.Context, agentID, claimID string, stopIntentAt time.Time) (bool, error) {
	released := false
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !holdsClaim(row, claimID) || !entAgentToStore(row).RunIntentMatches(store.RunIntentStopped, stopIntentAt) {
			return false, nil
		}
		// A stop-kind claim belongs to a queued stop being applied; another
		// stop does not supersede it.
		if row.StartClaimKind == string(store.StartClaimStop) {
			return false, nil
		}
		if _, err := clearStartClaim(claimUpdate(c, row)).Save(ctx); err != nil {
			return false, mapError(err)
		}
		released = true
		return true, nil
	})
	return released, notFoundAsLost(err)
}

// DemoteExpiredStartClaim implements store.AgentStore.DemoteExpiredStartClaim.
func (s *AgentStore) DemoteExpiredStartClaim(ctx context.Context, agentID, claimID string, holds store.StartClaimHolds) (bool, error) {
	demoted := false
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !holdsClaim(row, claimID) || launchActive(row) ||
			row.StartClaimState != string(store.StartClaimLive) ||
			row.StartClaimLeaseUntil == nil || !row.StartClaimLeaseUntil.Before(now) {
			return false, nil
		}
		hold := holds.For(store.StartClaimKind(row.StartClaimKind))
		if _, err := demoteStartClaim(claimUpdate(c, row), now, hold).Save(ctx); err != nil {
			return false, mapError(err)
		}
		demoted = true
		return true, nil
	})
	return demoted, notFoundAsLost(err)
}

// DemoteOwnerStartClaims implements store.AgentStore.DemoteOwnerStartClaims.
func (s *AgentStore) DemoteOwnerStartClaims(ctx context.Context, ownerPrefix, excludeOwner string, holds store.StartClaimHolds) (int, error) {
	if ownerPrefix == "" {
		return 0, fmt.Errorf("%w: owner prefix must not be empty", store.ErrInvalidInput)
	}
	ids, err := s.client.Agent.Query().
		Where(
			agent.StartClaimIDNotNil(),
			agent.StartClaimStateEQ(string(store.StartClaimLive)),
			agent.StartClaimOwnerHasPrefix(ownerPrefix),
			agent.StartClaimOwnerNEQ(excludeOwner),
		).
		IDs(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	n := 0
	for _, id := range ids {
		err := s.withLockedAgent(ctx, id.String(), func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
			if row.StartClaimID == nil || launchActive(row) ||
				row.StartClaimState != string(store.StartClaimLive) ||
				!strings.HasPrefix(row.StartClaimOwner, ownerPrefix) ||
				row.StartClaimOwner == excludeOwner {
				return false, nil
			}
			hold := holds.For(store.StartClaimKind(row.StartClaimKind))
			if _, err := demoteStartClaim(claimUpdate(c, row), now, hold).Save(ctx); err != nil {
				return false, mapError(err)
			}
			n++
			return true, nil
		})
		if notFoundAsLost(err) != nil {
			return n, err
		}
	}
	return n, nil
}

// ConvertUnconfirmedToStop implements store.AgentStore.ConvertUnconfirmedToStop.
func (s *AgentStore) ConvertUnconfirmedToStop(ctx context.Context, agentID, claimID, owner string, ttl time.Duration) (store.StartClaim, error) {
	if ttl <= 0 || owner == "" {
		return store.StartClaim{}, fmt.Errorf("%w: stop claim needs an owner and a positive lease", store.ErrInvalidInput)
	}
	var claim store.StartClaim
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !holdsClaim(row, claimID) || launchActive(row) ||
			row.StartClaimState != string(store.StartClaimUnconfirmed) {
			return false, store.ErrClaimPredicate
		}
		claim = store.StartClaim{
			ID:         uuid.NewString(),
			Kind:       store.StartClaimStop,
			Owner:      owner,
			Target:     row.StartClaimTarget,
			At:         now,
			LeaseUntil: now.Add(ttl),
		}
		if row.RunIntentAt != nil {
			claim.RunIntentAt = *row.RunIntentAt
		}
		if _, err := claimUpdate(c, row).
			SetStartClaimID(claim.ID).
			SetStartClaimKind(string(store.StartClaimStop)).
			SetStartClaimState(string(store.StartClaimLive)).
			SetStartClaimOwner(owner).
			SetStartClaimAt(claim.At).
			SetStartClaimLeaseUntil(claim.LeaseUntil).
			ClearStartClaimUnconfirmedAt().
			ClearStartClaimHoldUntil().
			SetStartClaimLaunchID("").
			Save(ctx); err != nil {
			return false, mapError(err)
		}
		return true, nil
	})
	if err != nil {
		return store.StartClaim{}, err
	}
	return claim, nil
}

// ReleaseUnconfirmedStart implements store.AgentStore.ReleaseUnconfirmedStart.
func (s *AgentStore) ReleaseUnconfirmedStart(ctx context.Context, agentID, claimID string) (bool, error) {
	released := false
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !holdsClaim(row, claimID) || launchActive(row) ||
			row.StartClaimState != string(store.StartClaimUnconfirmed) {
			return false, nil
		}
		if _, err := clearStartClaim(claimUpdate(c, row)).Save(ctx); err != nil {
			return false, mapError(err)
		}
		released = true
		return true, nil
	})
	return released, notFoundAsLost(err)
}

// SettleEndedLaunchClaim implements store.AgentStore.SettleEndedLaunchClaim.
func (s *AgentStore) SettleEndedLaunchClaim(ctx context.Context, agentID, claimID string) (bool, error) {
	changed := false
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !holdsClaim(row, claimID) || launchActive(row) || row.LaunchState == "" {
			return false, nil
		}
		u := settleLaunchClaim(claimUpdate(c, row), row, row.LaunchEndReason, now)
		if u == nil {
			return false, nil
		}
		if _, err := u.Save(ctx); err != nil {
			return false, mapError(err)
		}
		changed = true
		return true, nil
	})
	return changed, notFoundAsLost(err)
}

// settleLaunchClaim adds to u the change a launch ending with endReason
// makes to row's create claim, when the claim is linked to that launch
// (the row's current launch; see linkLaunchClaim): release, or demote a
// live claim to unconfirmed (hold_until left unset; the reaper applies the kind's hold
// from unconfirmed_at). It returns nil when there is nothing to settle: no
// create claim, or the end reason settles nothing, or the claim is already
// unconfirmed and would only be demoted. Callers writing a launch's terminal
// transition call it on the same update so the claim settles in the same
// transaction.
func settleLaunchClaim(u *ent.AgentUpdateOne, row *ent.Agent, endReason string, now time.Time) *ent.AgentUpdateOne {
	if row.StartClaimID == nil || row.StartClaimKind != string(store.StartClaimCreate) ||
		row.StartClaimLaunchID == "" || row.StartClaimLaunchID != row.LaunchID {
		return nil
	}
	release, demote := store.LaunchEndClaimSettlement(endReason)
	switch {
	case release:
		return clearStartClaim(u)
	case demote && row.StartClaimState == string(store.StartClaimLive):
		return demoteStartClaim(u, claimTime(now), 0)
	}
	return nil
}

// withLaunchEndSettlement is settleLaunchClaim for a terminal write that
// must happen regardless: it returns u, with the claim change added when
// there is one.
func withLaunchEndSettlement(u *ent.AgentUpdateOne, row *ent.Agent, endReason string, now time.Time) *ent.AgentUpdateOne {
	if s := settleLaunchClaim(u, row, endReason, now); s != nil {
		return s
	}
	return u
}

// ListAgentsWithStartClaim implements store.AgentStore.ListAgentsWithStartClaim.
func (s *AgentStore) ListAgentsWithStartClaim(ctx context.Context) ([]*store.Agent, error) {
	rows, err := s.client.Agent.Query().Where(agent.StartClaimIDNotNil(), agent.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.Agent, 0, len(rows))
	for _, r := range rows {
		out = append(out, entAgentToStore(r))
	}
	return out, nil
}

// ClaimAgentReincarnation implements store.AgentStore.ClaimAgentReincarnation.
func (s *AgentStore) ClaimAgentReincarnation(ctx context.Context, agentID string, expectedVersion int64, at time.Time) (int64, error) {
	var newVersion int64
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if row.StateVersion != expectedVersion {
			return false, store.ErrVersionConflict
		}
		if held := heldClaimError(row); held != nil {
			return false, held
		}
		switch row.ReincarnationState {
		case store.ReincarnationStateNone, store.ReincarnationStateFailed:
		default:
			return false, store.ErrClaimPredicate
		}
		newVersion = row.StateVersion + 1
		if _, err := claimUpdate(c, row).
			SetReincarnationState(store.ReincarnationStatePending).
			SetReincarnationUpdatedAt(at).
			SetStateVersion(newVersion).
			SetUpdated(now).
			Save(ctx); err != nil {
			return false, mapError(err)
		}
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	return newVersion, nil
}

// notFoundAsLost maps ErrNotFound to nil: for a holder or the reaper, a
// deleted agent means the claim is gone, which the false result reports.
func notFoundAsLost(err error) error {
	if err == nil || errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// linkLaunchClaim adds to u, a BeginLaunch write of launch launchID on row,
// the link from row's create claim to that launch, so only that launch's
// end settles the claim. A claim with no link is linked. A claim linked to
// an earlier launch is relinked while it is live with an unexpired lease:
// its holder is still running the start, and a deferred create runs a
// second BeginLaunch for the same start on the node that owns the broker.
// Otherwise (unconfirmed, or an expired lease) the link is left alone and
// the start-claim reaper settles the claim. Nothing is added when row holds
// no create claim.
//
// This relies on every BeginLaunch of an agent holding a create claim
// coming from the claim's holder or from its deferred continuation on the
// node that owns the broker: BeginLaunch has no claim ID, so a launch begun
// by anyone else would be adopted by the claim.
func linkLaunchClaim(u *ent.AgentUpdateOne, row *ent.Agent, launchID string, now time.Time) *ent.AgentUpdateOne {
	if row.StartClaimID == nil || row.StartClaimKind != string(store.StartClaimCreate) {
		return u
	}
	switch {
	case row.StartClaimLaunchID == "":
		u.SetStartClaimLaunchID(launchID)
	case row.StartClaimLaunchID != launchID &&
		row.StartClaimState == string(store.StartClaimLive) &&
		row.StartClaimLeaseUntil != nil && row.StartClaimLeaseUntil.After(claimTime(now)):
		u.SetStartClaimLaunchID(launchID)
	}
	return u
}

// StoreClock implements store.AgentStore.StoreClock.
func (s *AgentStore) StoreClock(ctx context.Context) (time.Time, error) {
	ltx, err := s.beginLaunchTx(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer ltx.cleanup()
	defer func() { _ = ltx.tx.Rollback() }()
	now, err := storeNow(ctx, ltx.tx, s.dialect(ctx) == dialect.Postgres)
	if err != nil {
		return time.Time{}, err
	}
	return claimTime(now), nil
}
