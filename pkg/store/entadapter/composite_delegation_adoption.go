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

// DelegationProvenanceAdoptionMarkerSection is the completion marker of the
// delegation-provenance adoption migration. It is independent of the edge
// backfill marker (migration_delegation_edge_backfill_v1), which plays no
// part in whether this migration runs.
const DelegationProvenanceAdoptionMarkerSection = "migration_delegation_provenance_adoption_v1"

// DelegationProvenanceAdoptionCohortSection is the cohort snapshot header.
// Once it exists, no new snapshot is taken: later runs only work through
// the snapshot's pending records, so no row written after the snapshot can
// enter the automatic cohort.
const DelegationProvenanceAdoptionCohortSection = "delegation_provenance_adoption_cohort"

// delegationAdoptionSchemaVersion is the layout of the header and marker.
const delegationAdoptionSchemaVersion = 1

// DelegationAdoptionHeader is the JSON value of the cohort header and of the
// completion marker.
type DelegationAdoptionHeader struct {
	SchemaVersion int            `json:"schema_version"`
	PolicyVersion int            `json:"policy_version"`
	CohortID      string         `json:"cohort_id"`
	Completed     bool           `json:"completed,omitempty"`
	Counts        map[string]int `json:"counts,omitempty"`
}

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
func (c *CompositeStore) AdoptLegacyDelegationProvenance(ctx context.Context) error {
	log := c.adoptionLog()
	if s, err := c.GetHubSetting(ctx, DelegationProvenanceAdoptionMarkerSection); err == nil {
		var m DelegationAdoptionHeader
		if jerr := json.Unmarshal(s.Value, &m); jerr != nil || m.SchemaVersion != delegationAdoptionSchemaVersion {
			log.Warn("delegation provenance adoption: marker has an unknown layout; treated as complete",
				"section", DelegationProvenanceAdoptionMarkerSection)
		}
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	cohortID, err := c.ensureAdoptionSnapshot(ctx)
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
	notInCohort, err := c.adoptionNotInCohort(ctx, cohortID)
	if err != nil {
		log.Warn("delegation provenance adoption: could not count unrecorded hops outside the cohort", "error", err)
	}
	c.logAdoptionSummary(cohortID, counts, notInCohort)

	if failed || counts[string(store.DelegationAdoptionPending)] > 0 {
		return nil
	}
	value, err := json.Marshal(DelegationAdoptionHeader{
		SchemaVersion: delegationAdoptionSchemaVersion,
		PolicyVersion: int(delegationadoption.PolicyVersion),
		CohortID:      cohortID,
		Completed:     true,
		Counts:        counts,
	})
	if err != nil {
		return err
	}
	_, err = c.UpsertHubSetting(ctx, DelegationProvenanceAdoptionMarkerSection, value, "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// ensureAdoptionSnapshot returns the cohort ID of the existing snapshot, or
// takes the snapshot: every examined hop's record and the header, written
// in one transaction.
func (c *CompositeStore) ensureAdoptionSnapshot(ctx context.Context) (string, error) {
	if s, err := c.GetHubSetting(ctx, DelegationProvenanceAdoptionCohortSection); err == nil {
		var h DelegationAdoptionHeader
		if jerr := json.Unmarshal(s.Value, &h); jerr != nil || h.CohortID == "" {
			return "", fmt.Errorf("cohort header is unreadable")
		}
		return h.CohortID, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}

	plan, err := delegationadoption.Build(ctx, c, delegationadoption.Scope{})
	if err != nil {
		return "", err
	}
	cohortID := uuid.NewString()
	records := SnapshotRecords(plan, cohortID, store.DelegationAdoptionOriginBoot)
	counts := map[string]int{}
	for _, r := range records {
		counts[string(r.Status)]++
	}
	header, err := json.Marshal(DelegationAdoptionHeader{
		SchemaVersion: delegationAdoptionSchemaVersion,
		PolicyVersion: int(delegationadoption.PolicyVersion),
		CohortID:      cohortID,
		Counts:        counts,
	})
	if err != nil {
		return "", err
	}
	// One ent transaction for the records and the header row.
	// UpsertHubSetting opens its own transaction, so the header is created
	// directly on the transaction's client.
	tx, err := c.client.Tx(ctx)
	if err != nil {
		return "", fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	txStore := newTxCompositeStore(tx)
	for _, r := range records {
		if err := txStore.CreateDelegationAdoption(ctx, r); err != nil {
			return "", err
		}
	}
	if _, err := tx.HubSetting.Create().
		SetSection(DelegationProvenanceAdoptionCohortSection).
		SetValue(header).
		SetRevision(1).
		SetUpdatedBy("migration").
		SetOrigin(hubsetting.OriginSeeded).
		Save(ctx); err != nil {
		return "", mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit snapshot: %w", err)
	}
	return cohortID, nil
}

// SnapshotRecords returns the records a plan contributes to a cohort:
// adoptable hops as pending, already-adopted hops as recognized (or
// recognized_above_policy), and excluded hops whose edge is not a recorded
// edge. Recorded hops are valid path members but not cohort members, so
// they get no record.
func SnapshotRecords(plan *delegationadoption.Plan, cohortID, origin string) []*store.DelegationAdoption {
	var out []*store.DelegationAdoption
	for _, h := range plan.Hops {
		rec := &store.DelegationAdoption{
			CohortID:          cohortID,
			Origin:            origin,
			PolicyVersion:     int(plan.PolicyVersion),
			DelegateID:        h.DelegateID,
			ScopeID:           h.ProjectID,
			Depth:             h.Depth,
			Reason:            string(h.Reason),
			BeforeFingerprint: h.Fingerprint,
		}
		if h.Edge != nil {
			rec.DelegatorType = h.Edge.DelegatorType
			rec.DelegatorID = h.Edge.DelegatorID
			rec.ScopeID = h.Edge.ScopeID
			rec.Role = h.Edge.Role
		}
		switch h.Outcome {
		case delegationadoption.OutcomeAdopt:
			rec.Status = store.DelegationAdoptionPending
			rec.OriginalEdgeID = h.Edge.ID
		case delegationadoption.OutcomeRecognized, delegationadoption.OutcomeRecognizedAbovePolicy:
			rec.Status = store.DelegationAdoptionStatus(h.Outcome)
			rec.OriginalEdgeID = h.OriginalEdgeID
			rec.AdoptedEdgeID = h.Edge.ID
		case delegationadoption.OutcomeExcluded:
			if h.Edge != nil && h.Edge.ProvenanceVersion == store.ProvenanceVersionV1 && h.Edge.Kind != store.EffectCeilingUnrecorded {
				continue
			}
			rec.Status = store.DelegationAdoptionExcluded
			if h.Edge != nil {
				rec.OriginalEdgeID = h.Edge.ID
			}
		default:
			continue
		}
		out = append(out, rec)
	}
	return out
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
