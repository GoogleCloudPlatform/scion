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

package delegationadoption

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RevertReader is the read surface revert planning needs. store.Store
// satisfies it.
type RevertReader interface {
	GetDelegationAdoption(ctx context.Context, id string) (*store.DelegationAdoption, error)
	GetDelegationEdge(ctx context.Context, edgeID string) (*store.DelegationEdge, error)
}

// RevertOutcome is the planned treatment of one record in a revert.
type RevertOutcome string

const (
	// RevertOutcomeRevert: the adopted edge is deactivated and the original
	// unrecorded row reactivated.
	RevertOutcomeRevert RevertOutcome = "revert"
	// RevertOutcomeRefused: the record cannot be reverted (Reason).
	RevertOutcomeRefused RevertOutcome = "refused"
)

// RevertHop is the plan for one adoption record.
type RevertHop struct {
	RecordID       string        `json:"recordId"`
	DelegateID     string        `json:"delegateId,omitempty"`
	AdoptedEdgeID  string        `json:"adoptedEdgeId,omitempty"`
	OriginalEdgeID string        `json:"originalEdgeId,omitempty"`
	Outcome        RevertOutcome `json:"outcome"`
	Reason         Reason        `json:"reason,omitempty"`
	Fingerprint    string        `json:"fingerprint"`

	record      *store.DelegationAdoption
	adopted     *store.DelegationEdge
	original    *store.DelegationEdge
	expectCause store.EdgeDeactivationCause
}

// Record returns the adoption record the hop reverts (nil when missing).
func (h *RevertHop) Record() *store.DelegationAdoption { return h.record }

// RevertPlan is the plan for a revert.
type RevertPlan struct {
	Hops []*RevertHop `json:"hops"`
}

// Refused returns the number of refused hops.
func (p *RevertPlan) Refused() int {
	n := 0
	for _, h := range p.Hops {
		if h.Outcome == RevertOutcomeRefused {
			n++
		}
	}
	return n
}

// Fingerprint returns the revert plan fingerprint.
func (p *RevertPlan) Fingerprint() string {
	fps := make([]string, 0, len(p.Hops))
	for _, h := range p.Hops {
		fps = append(fps, h.Fingerprint)
	}
	return digest(struct {
		PolicyVersion int      `json:"policy_version"`
		Operation     string   `json:"operation"`
		Hops          []string `json:"hops"`
	}{int(PolicyVersion), "revert", fps})
}

// BuildRevert plans the revert of recordIDs, in the given order. A record
// without a resolved original row is refused unless confirm names one for it
// (record ID → edge ID) that matches the adopted edge.
func BuildRevert(ctx context.Context, r RevertReader, recordIDs []string, confirm map[string]string) (*RevertPlan, error) {
	plan := &RevertPlan{}
	seen := map[string]bool{}
	for _, id := range recordIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		h, err := planRevertHop(ctx, r, id, confirm[id])
		if err != nil {
			return nil, err
		}
		h.Fingerprint = revertFingerprint(h)
		plan.Hops = append(plan.Hops, h)
	}
	return plan, nil
}

func planRevertHop(ctx context.Context, r RevertReader, recordID, confirmed string) (*RevertHop, error) {
	h := &RevertHop{RecordID: recordID, Outcome: RevertOutcomeRefused}
	rec, err := r.GetDelegationAdoption(ctx, recordID)
	if errors.Is(err, store.ErrNotFound) {
		h.Reason = ReasonRecordMissing
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("adoption record %s: %w", recordID, err)
	}
	h.record = rec
	h.DelegateID = rec.DelegateID
	h.AdoptedEdgeID = rec.AdoptedEdgeID
	switch rec.Status {
	case store.DelegationAdoptionAdopted, store.DelegationAdoptionRecognized, store.DelegationAdoptionRecognizedAbovePolicy:
	default:
		h.Reason = ReasonNotRevertible
		return h, nil
	}
	if rec.AdoptedEdgeID == "" {
		h.Reason = ReasonNotRevertible
		return h, nil
	}
	adopted, err := r.GetDelegationEdge(ctx, rec.AdoptedEdgeID)
	if errors.Is(err, store.ErrNotFound) {
		h.Reason = ReasonAdoptedEdgeChanged
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("edge %s: %w", rec.AdoptedEdgeID, err)
	}
	h.adopted = adopted
	if !adopted.Active || !alreadyAdopted(adopted) || adopted.DelegateID != rec.DelegateID {
		h.Reason = ReasonAdoptedEdgeChanged
		return h, nil
	}
	originalID := rec.OriginalEdgeID
	if originalID == "" {
		originalID = confirmed
	}
	if originalID == "" {
		h.Reason = ReasonAmbiguousOriginal
		return h, nil
	}
	h.OriginalEdgeID = originalID
	original, err := r.GetDelegationEdge(ctx, originalID)
	if errors.Is(err, store.ErrNotFound) {
		h.Reason = ReasonOriginalChanged
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("edge %s: %w", originalID, err)
	}
	h.original = original
	if !MatchesOriginal(adopted, original) {
		h.Reason = ReasonOriginalChanged
		return h, nil
	}
	h.expectCause = original.Cause
	h.Outcome = RevertOutcomeRevert
	return h, nil
}

func revertFingerprint(h *RevertHop) string {
	type edgeFacts struct {
		ID      string   `json:"id"`
		Active  bool     `json:"active"`
		Updated string   `json:"updated"`
		Version int      `json:"provenance_version"`
		Kind    string   `json:"ceiling_kind"`
		IDs     []string `json:"ceiling_ids"`
		Cause   string   `json:"cause"`
	}
	facts := func(e *store.DelegationEdge) *edgeFacts {
		if e == nil {
			return nil
		}
		return &edgeFacts{e.ID, e.Active, e.UpdatedAt.UTC().Format(time.RFC3339Nano), e.ProvenanceVersion, string(e.Kind), e.PermissionIDs, string(e.Cause)}
	}
	status := ""
	if h.record != nil {
		status = string(h.record.Status)
	}
	return digest(struct {
		RecordID string     `json:"record_id"`
		Status   string     `json:"status"`
		Outcome  string     `json:"outcome"`
		Reason   string     `json:"reason"`
		Adopted  *edgeFacts `json:"adopted"`
		Original *edgeFacts `json:"original"`
	}{h.RecordID, status, string(h.Outcome), string(h.Reason), facts(h.adopted), facts(h.original)})
}

// ApplyRevert reverts hop inside tx: deactivate the adopted edge only if it
// is active and unchanged (cause adoption_reverted), then reactivate
// the original row only if it is inactive with the planned cause and
// no other active edge exists for the delegate and scope. A changed state
// returns a skipped_changed Result; the caller must then roll the
// transaction back, because the first step may already have been written.
// ApplyRevert does not write the record.
func ApplyRevert(ctx context.Context, tx store.Store, hop *RevertHop, opID string) (Result, error) {
	if hop == nil || hop.Outcome != RevertOutcomeRevert {
		return skipped(ReasonNotRevertible), nil
	}
	updated := hop.adopted.UpdatedAt
	ok, err := tx.DeactivateDelegationEdgeGuarded(ctx, hop.adopted.ID,
		store.DelegationEdgeDeactivateGuard{Recorded: true, UpdatedAt: &updated},
		store.EdgeDeactivationAdoptionReverted, opID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return skipped(ReasonAdoptedEdgeChanged), nil
		}
		return Result{}, fmt.Errorf("deactivate edge %s: %w", hop.adopted.ID, err)
	}
	if !ok {
		return skipped(ReasonAdoptedEdgeChanged), nil
	}
	if err := tx.ReactivateDelegationEdge(ctx, hop.original.ID, hop.expectCause); err != nil {
		if errors.Is(err, store.ErrRevisionConflict) || errors.Is(err, store.ErrNotFound) {
			return skipped(ReasonOriginalChanged), nil
		}
		return Result{}, fmt.Errorf("reactivate edge %s: %w", hop.original.ID, err)
	}
	return Result{
		Status:       store.DelegationAdoptionReverted,
		Before:       EdgeSummary(hop.adopted),
		AfterSummary: EdgeSummary(hop.original),
	}, nil
}
