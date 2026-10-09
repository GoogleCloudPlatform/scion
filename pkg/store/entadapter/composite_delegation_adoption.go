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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

func (c *CompositeStore) adoptionLog() *slog.Logger {
	if c.adoptionLogger != nil {
		return c.adoptionLogger
	}
	return slog.Default()
}

// AdoptLegacyDelegationProvenance runs the delegation-provenance adoption
// migration. It is idempotent and resumable:
//
//   - with the completion marker present it does nothing;
//   - without a cohort header it plans the live-agent ancestor closure and
//     writes every examined hop's record together with the header in one
//     transaction (the snapshot);
//   - it then adopts each pending record of the snapshot, top-down, one
//     transaction per hop (delegationadoption.ApplyAdopt). A changed hop is
//     recorded as skipped_changed; a write error stops the loop and leaves
//     the marker unset, so the next boot resumes the same snapshot;
//   - when no pending record remains it writes the marker.
//
// A write failure does not fail the boot: unadopted hops keep their current
// denial, and the summary and the admin status view report them.
//
// Stop on the first failing hop: the loop does not skip a hop whose write
// fails and continue. Hops run top-down and a descendant needs its parent
// recorded, so continuing past a failure would mostly record
// ancestor-not-adopted skips. The trade-off is that a write error that
// repeats on every boot leaves every later pending hop pending, and they
// are never adopted automatically; the summary log and the admin status
// view report them, and an admin commit can adopt them.
//
// If planning or the snapshot write fails on the first boot, no snapshot
// exists while the hub serves, and the next boot's snapshot includes rows
// written in between (see Migrate).
func (c *CompositeStore) AdoptLegacyDelegationProvenance(ctx context.Context) error {
	log := c.adoptionLog()
	if s, err := c.GetHubSetting(ctx, delegationadoption.MarkerSection); err == nil {
		var m delegationadoption.Header
		if jerr := json.Unmarshal(s.Value, &m); jerr != nil || m.SchemaVersion != delegationadoption.HeaderSchemaVersion {
			log.Warn("delegation provenance adoption: marker has an unknown layout; treated as complete",
				"section", delegationadoption.MarkerSection)
		}
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	cohortID, fresh, err := c.ensureAdoptionSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("delegation provenance adoption snapshot: %w", err)
	}

	pending, _, err := c.ListDelegationAdoptions(ctx, store.DelegationAdoptionFilter{
		CohortID: cohortID, Status: store.DelegationAdoptionPending,
	})
	if err != nil {
		return fmt.Errorf("delegation provenance adoption: list pending: %w", err)
	}
	failed := false
	for i, rec := range pending {
		if c.adoptionHopHook != nil {
			if herr := c.adoptionHopHook(i, rec); herr != nil {
				log.Error("delegation provenance adoption: hop write failed; remaining hops left pending",
					"delegate_id", rec.DelegateID, "error", herr)
				failed = true
				break
			}
		}
		// A write error stops the loop (see the trade-off above).
		if aerr := c.adoptOneHop(ctx, rec); aerr != nil {
			log.Error("delegation provenance adoption: hop write failed; remaining hops left pending",
				"delegate_id", rec.DelegateID, "error", aerr)
			failed = true
			break
		}
	}

	counts, err := c.adoptionCounts(ctx, cohortID)
	if err != nil {
		return err
	}
	// A snapshot taken by this run examined every live chain moments ago,
	// so the full plan is not built a second time to count hops outside
	// it; rows another replica writes meanwhile surface in the admin
	// status view. A resumed run counts them.
	notInCohort := 0
	if !fresh {
		notInCohort, err = c.adoptionNotInCohort(ctx, cohortID)
		if err != nil {
			log.Warn("delegation provenance adoption: could not count unrecorded hops outside the cohort", "error", err)
		}
	}
	c.logAdoptionSummary(cohortID, counts, notInCohort)

	if failed || counts[string(store.DelegationAdoptionPending)] > 0 {
		return nil
	}
	value, err := json.Marshal(delegationadoption.Header{
		SchemaVersion: delegationadoption.HeaderSchemaVersion,
		PolicyVersion: int(delegationadoption.PolicyVersion),
		CohortID:      cohortID,
		Completed:     true,
		Counts:        counts,
	})
	if err != nil {
		return err
	}
	_, err = c.UpsertHubSetting(ctx, delegationadoption.MarkerSection, value, "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// ensureAdoptionSnapshot returns the cohort ID of the existing snapshot, or
// takes the snapshot: every examined hop's record and the header, written
// in one transaction. fresh reports that this call took the snapshot.
func (c *CompositeStore) ensureAdoptionSnapshot(ctx context.Context) (cohortID string, fresh bool, err error) {
	if s, err := c.GetHubSetting(ctx, delegationadoption.CohortSection); err == nil {
		var h delegationadoption.Header
		if jerr := json.Unmarshal(s.Value, &h); jerr != nil || h.CohortID == "" {
			return "", false, fmt.Errorf("cohort header is unreadable")
		}
		return h.CohortID, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", false, err
	}

	plan, err := delegationadoption.Build(ctx, c, delegationadoption.Scope{})
	if err != nil {
		return "", false, err
	}
	cohortID = uuid.NewString()
	records := delegationadoption.SnapshotRecords(plan, cohortID, store.DelegationAdoptionOriginBoot)
	counts := map[string]int{}
	for _, r := range records {
		counts[string(r.Status)]++
	}
	header, err := json.Marshal(delegationadoption.Header{
		SchemaVersion: delegationadoption.HeaderSchemaVersion,
		PolicyVersion: int(delegationadoption.PolicyVersion),
		CohortID:      cohortID,
		Counts:        counts,
	})
	if err != nil {
		return "", false, err
	}
	// One ent transaction for the records and the header row.
	// UpsertHubSetting opens its own transaction, so the header is created
	// directly on the transaction's client.
	tx, err := c.client.Tx(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	txStore := newTxCompositeStore(tx)
	for _, r := range records {
		if err := txStore.CreateDelegationAdoption(ctx, r); err != nil {
			return "", false, err
		}
	}
	if _, err := tx.HubSetting.Create().
		SetSection(delegationadoption.CohortSection).
		SetValue(header).
		SetRevision(1).
		SetUpdatedBy("migration").
		SetOrigin(hubsetting.OriginSeeded).
		Save(ctx); err != nil {
		return "", false, mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit snapshot: %w", err)
	}
	return cohortID, true, nil
}

// adoptOneHop applies one pending record in its own transaction and writes
// the record's outcome in the same transaction. A unique-index rejection of
// the adopted row (a concurrent adoption of the same hop) rolls the hop
// back and records recognized when the hop's active edge is an already-adopted edge,
// skipped_changed otherwise. Any other error is returned.
func (c *CompositeStore) adoptOneHop(ctx context.Context, rec *store.DelegationAdoption) error {
	updated := *rec
	err := c.WithTx(ctx, func(tx store.Store) error {
		res, err := delegationadoption.ApplyAdopt(ctx, tx, &updated, delegationadoption.Actor{})
		if err != nil {
			return err
		}
		if c.adoptionTxHook != nil {
			if err := c.adoptionTxHook(tx, &updated); err != nil {
				return err
			}
		}
		updated.Status = res.Status
		updated.Reason = string(res.Reason)
		updated.AdoptedEdgeID = res.AdoptedEdgeID
		updated.AfterSummary = res.AfterSummary
		return tx.UpdateDelegationAdoption(ctx, &updated)
	})
	if err == nil {
		*rec = updated
		return nil
	}
	if !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	resolved := *rec
	resolved.Status = store.DelegationAdoptionSkippedChanged
	resolved.Reason = string(delegationadoption.ReasonConcurrentAdoption)
	plan, perr := delegationadoption.Build(ctx, c, delegationadoption.Scope{AgentIDs: []string{rec.DelegateID}})
	if perr != nil {
		return perr
	}
	if h := plan.Hop(rec.DelegateID); h != nil && h.Edge != nil &&
		(h.Outcome == delegationadoption.OutcomeRecognized || h.Outcome == delegationadoption.OutcomeRecognizedAbovePolicy) &&
		h.OriginalEdgeID == rec.OriginalEdgeID {
		resolved.Status = store.DelegationAdoptionStatus(h.Outcome)
		resolved.AdoptedEdgeID = h.Edge.ID
	}
	if uerr := c.UpdateDelegationAdoption(ctx, &resolved); uerr != nil {
		return uerr
	}
	*rec = resolved
	return nil
}

func (c *CompositeStore) adoptionCounts(ctx context.Context, cohortID string) (map[string]int, error) {
	recs, _, err := c.ListDelegationAdoptions(ctx, store.DelegationAdoptionFilter{CohortID: cohortID})
	if err != nil {
		return nil, fmt.Errorf("delegation provenance adoption: count records: %w", err)
	}
	counts := map[string]int{}
	for _, r := range recs {
		counts[string(r.Status)]++
	}
	return counts, nil
}

// adoptionNotInCohort counts adoptable unrecorded hops on live chains whose
// edge is not in the cohort snapshot (for example rows an older replica
// wrote after the snapshot). Only an admin commit can adopt them.
func (c *CompositeStore) adoptionNotInCohort(ctx context.Context, cohortID string) (int, error) {
	recs, _, err := c.ListDelegationAdoptions(ctx, store.DelegationAdoptionFilter{CohortID: cohortID})
	if err != nil {
		return 0, err
	}
	inCohort := make(map[string]bool, len(recs))
	for _, r := range recs {
		if r.OriginalEdgeID != "" {
			inCohort[r.OriginalEdgeID] = true
		}
	}
	plan, err := delegationadoption.Build(ctx, c, delegationadoption.Scope{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range plan.Hops {
		if h.Outcome == delegationadoption.OutcomeAdopt && !inCohort[h.Edge.ID] {
			n++
		}
	}
	return n, nil
}

func (c *CompositeStore) logAdoptionSummary(cohortID string, counts map[string]int, notInCohort int) {
	log := c.adoptionLog()
	args := []any{"cohort_id", cohortID, "not_in_cohort", notInCohort}
	for _, s := range []store.DelegationAdoptionStatus{
		store.DelegationAdoptionPending, store.DelegationAdoptionAdopted,
		store.DelegationAdoptionRecognized, store.DelegationAdoptionRecognizedAbovePolicy,
		store.DelegationAdoptionExcluded, store.DelegationAdoptionSkippedChanged,
		store.DelegationAdoptionReverted,
	} {
		args = append(args, string(s), counts[string(s)])
	}
	log.Info("delegation provenance adoption summary", args...)
	unresolved := counts[string(store.DelegationAdoptionExcluded)] +
		counts[string(store.DelegationAdoptionSkippedChanged)] +
		counts[string(store.DelegationAdoptionPending)] + notInCohort
	if unresolved > 0 {
		log.Warn(fmt.Sprintf("delegation provenance adoption: %d hops on live agent chains remain unrecorded; review GET /api/v1/admin/delegation-adoption", unresolved),
			"cohort_id", cohortID)
	}
}
