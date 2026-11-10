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
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Purge of soft-deleted agents and their retained files (ptone/scion#4744).
//
// A soft delete with soft_delete_retain_files leaves the agent's files on its
// runtime broker. Once the retention window has passed, the purge job asks
// the broker to remove them (a delete with api.PurgeRetainedQueryParam)
// before it removes the agent record, so a record is never purged while the
// broker still holds its files and could still be asked to remove them.
//
// The soft delete does not record on the row whether the files were kept,
// so every expired soft-deleted agent with a broker and a run ID gets the
// purge dispatch (a row with no run ID is purged without one, see
// settleRetainedFiles).
// That is safe for one whose files were deleted: the broker removes only the
// files of an agent whose agent-info.json carries the soft-delete mark and
// that no runtime entry or start holds, and otherwise answers 404 (no files)
// or 409 retained_files_refused, removing nothing.
//
// Before dispatching, the purge claims the row with the delete marker
// (deletion_state deleting under a short lease), so a restore or start of
// the agent is refused while the broker removes its files (deleteBlocksStart),
// and purges the record right after the broker answers, while the claim is
// still live.
// A restore that landed first makes the claim match nothing, and the row is
// left alone. If the hub dies mid-dispatch, the lease lapses: a restore is no
// longer refused (restoreAgentTx clears the lapsed marker), and the next
// purge run re-claims the row.

const (
	// retainedPurgeLease is the lease of the purge's claim on a row. It
	// outlasts retainedPurgeDispatchTimeout, so the claim holds for the
	// whole dispatch.
	retainedPurgeLease = 5 * time.Minute
	// retainedPurgeDispatchTimeout bounds one purge dispatch.
	retainedPurgeDispatchTimeout = 2 * time.Minute
	// retainedPurgeGiveUp is how long past the retention window the purge
	// keeps a record whose broker is registered but cannot be reached (or
	// keeps failing), retrying on every run. After that the record is
	// purged and its retained files, if any, are reported left on the
	// broker.
	retainedPurgeGiveUp = 30 * 24 * time.Hour
	// retainedPurgePageSize is how many expired agents one store read
	// returns.
	retainedPurgePageSize = 200
)

// retainedPurgeOutcome is what the purge does with one expired record.
type retainedPurgeOutcome int

const (
	// retainedPurgeKeep keeps the record; the next run retries.
	retainedPurgeKeep retainedPurgeOutcome = iota
	// retainedPurgeRemove purges the record.
	retainedPurgeRemove
	// retainedPurgeSkip leaves the record alone this run (restored, or
	// claimed by someone else).
	retainedPurgeSkip
)

// purgeExpiredAgents purges the agents soft-deleted before cutoff, after
// removing the files their brokers retained (see the comment at the top of
// this file). It returns the number of records purged.
func (s *Server) purgeExpiredAgents(ctx context.Context, cutoff time.Time) (int, error) {
	total := 0
	after := ""
	for {
		page, err := s.store.ListSoftDeletedAgentsBefore(ctx, cutoff, after, retainedPurgePageSize)
		if err != nil {
			return total, err
		}
		if len(page) == 0 {
			return total, nil
		}
		for _, a := range page {
			if s.settleRetainedFiles(ctx, a, cutoff) != retainedPurgeRemove {
				continue
			}
			// Purged at once, while the claim taken for its dispatch is
			// still live, so the claim's lease never lapses between the
			// broker removing the files and the record going away (a
			// restore in that gap would bring back an agent without its
			// files). Purged rows drop out of later pages.
			n, err := s.store.PurgeDeletedAgentsByID(ctx, []string{a.ID}, cutoff)
			total += n
			if err != nil {
				return total, err
			}
			if n > 0 {
				// The record and the files its broker retained are gone:
				// remove the agent's storage the hub owns (its agents/<slug>
				// directory on the shared project storage and its
				// object-storage prefix), audited (ptone/scion#5835).
				s.startAgentStorageCleanup(a, false)
			}
		}
		if len(page) < retainedPurgePageSize {
			return total, nil
		}
		after = page[len(page)-1].ID
	}
}

// settleRetainedFiles decides whether the expired record a may be purged,
// first asking its broker to remove the files it retained.
func (s *Server) settleRetainedFiles(ctx context.Context, a *store.Agent, cutoff time.Time) retainedPurgeOutcome {
	log := s.agentLifecycleLog.With("agent_id", a.ID, "agent", a.Slug, "project_id", a.ProjectID,
		"broker_id", a.RuntimeBrokerID, "deleted_at", a.DeletedAt)
	if a.RuntimeBrokerID == "" {
		return retainedPurgeRemove
	}
	// A row with no run ID predates run IDs. A purge of it could not be
	// fenced to its run, and a broker without the purge checks would
	// remove by name alone, so the files of a newer agent reusing the name
	// could go with it. Such a record is purged without a dispatch.
	if a.RunID == "" {
		log.Warn("Purge: the agent has no run ID; its retained files, if any, were left on its broker")
		return retainedPurgeRemove
	}
	broker, err := s.store.GetRuntimeBroker(ctx, a.RuntimeBrokerID)
	if errors.Is(err, store.ErrNotFound) {
		log.Warn("Purge: the agent's broker no longer exists; its retained files, if any, were left on that host")
		return retainedPurgeRemove
	}
	if err != nil {
		log.Warn("Purge: could not read the agent's broker; will retry", "error", err)
		return s.retainedPurgeKeepOrGiveUp(log, a, cutoff, "broker lookup failed")
	}
	if !s.brokerRecordReachable(ctx, broker) {
		log.Debug("Purge: the agent's broker is not reachable; will retry")
		return s.retainedPurgeKeepOrGiveUp(log, a, cutoff, "broker not reachable")
	}

	claim, ok := s.claimForRetainedPurge(ctx, a.ID)
	if !ok {
		// A soft-deleted row left finalizing by a permanent delete that
		// stopped after its teardown cannot be claimed; teardown already
		// ran, so it is purged as before, with no dispatch, rather than
		// kept forever. Anything else (restored, or claimed by another
		// purge) is left alone this run.
		if row, err := s.store.GetAgent(ctx, a.ID); err == nil && !row.DeletedAt.IsZero() &&
			row.DeletionState == store.DeletionStateFinalizing {
			log.Info("Purge: the agent's permanent delete already ran its teardown; purging the record")
			return retainedPurgeRemove
		}
		return retainedPurgeSkip
	}
	row, err := s.store.GetAgent(ctx, a.ID)
	if err != nil || row.DeletedAt.IsZero() || !row.DeletedAt.Before(cutoff) {
		s.releaseRetainedPurgeClaim(ctx, a.ID, claim)
		return retainedPurgeSkip
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		s.releaseRetainedPurgeClaim(ctx, a.ID, claim)
		return s.retainedPurgeKeepOrGiveUp(log, a, cutoff, "no dispatcher")
	}

	dctx, cancel := context.WithTimeout(withPurgeRetained(ctx), retainedPurgeDispatchTimeout)
	err = dispatcher.DispatchAgentDelete(dctx, row, true, false, false, time.Time{})
	cancel()
	switch outcome, reason := classifyRetainedPurgeResult(err); outcome {
	case retainedPurgeRemove:
		if reason == "" {
			log.Info("Purge: broker removed the agent's retained files, or had none")
		} else {
			log.Warn("Purge: broker removed nothing; purging the record", "reason", reason, "error", err)
		}
		return retainedPurgeRemove
	default:
		s.releaseRetainedPurgeClaim(ctx, a.ID, claim)
		log.Warn("Purge: removing the agent's retained files failed; will retry", "error", err)
		return s.retainedPurgeKeepOrGiveUp(log, a, cutoff, "dispatch failed")
	}
}

// classifyRetainedPurgeResult maps the purge dispatch's result to an
// outcome, with a reason when the record is purged although the broker
// removed nothing:
//   - nil (removed, or 404: no files there): purge;
//   - run mismatch: another run holds the name there, so none of this
//     agent's files are left: purge;
//   - 409 retained_files_refused: the files there are not this soft-deleted
//     agent's to remove (no mark, held by a runtime entry or start, or a
//     symlink): purge;
//   - 400: the request is refused as malformed; a retry cannot change it:
//     purge;
//   - anything else (unreachable, timeout, 5xx, other 409): keep.
func classifyRetainedPurgeResult(err error) (retainedPurgeOutcome, string) {
	if err == nil {
		return retainedPurgeRemove, ""
	}
	if errors.Is(err, ErrDeleteRunMismatch) {
		return retainedPurgeRemove, "another run of the agent holds its name on the broker"
	}
	var se *brokerStatusError
	if errors.As(err, &se) {
		switch {
		case se.StatusCode == http.StatusConflict && se.brokerErrorCode() == api.BrokerErrCodeRetainedFilesRefused:
			reason, _ := se.brokerErrorDetails()[api.BrokerErrDetailReason].(string)
			if reason == "" {
				reason = "retained files refused"
			}
			return retainedPurgeRemove, reason
		case se.StatusCode == http.StatusBadRequest:
			return retainedPurgeRemove, "broker refused the request as invalid"
		}
	}
	return retainedPurgeKeep, ""
}

// retainedPurgeKeepOrGiveUp keeps the record, unless it was soft-deleted
// more than retainedPurgeGiveUp before cutoff: then it is purged and its
// retained files, if any, are reported left on the broker.
func (s *Server) retainedPurgeKeepOrGiveUp(log *slog.Logger, a *store.Agent, cutoff time.Time, why string) retainedPurgeOutcome {
	if a.DeletedAt.Before(cutoff.Add(-retainedPurgeGiveUp)) {
		log.Warn("Purge: giving up on the agent's retained files; they were left on its broker",
			"why", why, "give_up_after", retainedPurgeGiveUp)
		return retainedPurgeRemove
	}
	return retainedPurgeKeep
}

// claimForRetainedPurge claims the soft-deleted row id for a purge dispatch:
// deletion_state deleting under retainedPurgeLease, with a new claim epoch.
// It matches a row with no delete marker (or a failed one), or one whose
// earlier purge claim lapsed. A row restored since it was listed
// (deleted_at null), held by a live claim, or finalizing matches nothing.
// It returns the claim taken.
func (s *Server) claimForRetainedPurge(ctx context.Context, id string) (int64, bool) {
	now := time.Now()
	lease := now.Add(retainedPurgeLease)
	deleting := store.DeletionStateDeleting
	set := store.DeletionFields{State: &deleting, BumpClaim: true, LeaseAt: &lease, KeepUpdated: true}
	// A soft-deleted row normally carries no marker (the soft finish clears
	// it); a failed one is claimed too, so no stale marker keeps a record
	// from ever being purged.
	preds := []store.DeletionPredicate{
		{States: []string{store.DeletionStateNone, store.DeletionStateFailed}, DeletedAtNotNull: true},
		{States: []string{store.DeletionStateDeleting}, DeletedAtNotNull: true, LeaseExpiredBefore: &now},
	}
	for _, pred := range preds {
		n, err := s.store.UpdateAgentDeletion(ctx, id, pred, set)
		if err != nil {
			s.agentLifecycleLog.Warn("Purge: could not claim the agent row", "agent_id", id, "error", err)
			return 0, false
		}
		if n == 0 {
			continue
		}
		row, err := s.store.GetAgent(ctx, id)
		if err != nil {
			return 0, false
		}
		return row.DeletionClaim, true
	}
	return 0, false
}

// releaseRetainedPurgeClaim clears the purge's claim on row id, if it still
// holds it, so the row reads as plainly soft-deleted again.
func (s *Server) releaseRetainedPurgeClaim(ctx context.Context, id string, claim int64) {
	none := store.DeletionStateNone
	pred := store.DeletionPredicate{Claim: &claim, States: []string{store.DeletionStateDeleting}, DeletedAtNotNull: true}
	set := store.DeletionFields{State: &none, ClearLeaseAt: true, KeepUpdated: true}
	if _, err := s.store.UpdateAgentDeletion(ctx, id, pred, set); err != nil {
		s.agentLifecycleLog.Warn("Purge: could not release the claim on the agent row; it lapses with its lease",
			"agent_id", id, "error", err)
	}
}

// purgeRetainedKey marks a delete dispatch context as a purge of retained
// files (api.PurgeRetainedQueryParam).
type purgeRetainedKey struct{}

// withPurgeRetained marks ctx so DispatchAgentDelete sends the delete as a
// purge of retained files. It rides on the context, as the project path
// does, so the AgentDispatcher interface is unchanged.
func withPurgeRetained(ctx context.Context) context.Context {
	return context.WithValue(ctx, purgeRetainedKey{}, true)
}

// purgeRetainedFrom reports whether ctx was marked by withPurgeRetained.
func purgeRetainedFrom(ctx context.Context) bool {
	v, _ := ctx.Value(purgeRetainedKey{}).(bool)
	return v
}
