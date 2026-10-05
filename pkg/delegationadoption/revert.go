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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RevertReader is the read surface revert planning needs. store.Store
// satisfies it.
type RevertReader interface {
	GetDelegationAdoption(ctx context.Context, id string) (*store.DelegationAdoption, error)
	ListDelegationAdoptions(ctx context.Context, filter store.DelegationAdoptionFilter) ([]*store.DelegationAdoption, int, error)
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
	// CoveredRecordIDs are the other revertible records that point at the
	// same adopted edge. The one revert of the edge reverts them too, so no
	// record is left pointing at an inactive edge.
	CoveredRecordIDs []string `json:"coveredRecordIds,omitempty"`
	Fingerprint      string   `json:"fingerprint"`

	record      *store.DelegationAdoption
	covered     []*store.DelegationAdoption
	adopted     *store.DelegationEdge
	original    *store.DelegationEdge
	expectCause store.EdgeDeactivationCause
}

// Record returns the adoption record the hop reverts (nil when missing).
func (h *RevertHop) Record() *store.DelegationAdoption { return h.record }

// Records returns the hop's record followed by the covered records, in
// CoveredRecordIDs order.
func (h *RevertHop) Records() []*store.DelegationAdoption {
	if h.record == nil {
		return nil
	}
	return append([]*store.DelegationAdoption{h.record}, h.covered...)
}

func (h *RevertHop) cover(rec *store.DelegationAdoption) {
	if rec.ID == h.RecordID {
		return
	}
	for _, c := range h.covered {
		if c.ID == rec.ID {
			return
		}
	}
	h.covered = append(h.covered, rec)
	sort.Slice(h.covered, func(i, j int) bool { return h.covered[i].ID < h.covered[j].ID })
	h.CoveredRecordIDs = h.CoveredRecordIDs[:0]
	for _, c := range h.covered {
		h.CoveredRecordIDs = append(h.CoveredRecordIDs, c.ID)
	}
}

func revertibleStatus(s store.DelegationAdoptionStatus) bool {
	switch s {
	case store.DelegationAdoptionAdopted, store.DelegationAdoptionRecognized, store.DelegationAdoptionRecognizedAbovePolicy:
		return true
	}
	return false
}

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
//
// Hops are deduplicated by adopted edge: an edge is reverted once. The first
// revertible hop for an edge carries the revert, and every other revertible
// record that points at the same edge, requested or not, is covered by it
// and reverted with it. Requested records for that edge that would be
// refused on their own are covered too, so their refusal does not block the
// one revert that applies to them.
func BuildRevert(ctx context.Context, r RevertReader, recordIDs []string, confirm map[string]string) (*RevertPlan, error) {
	var hops []*RevertHop
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
		hops = append(hops, h)
	}

	primary := map[string]*RevertHop{}
	for _, h := range hops {
		if h.Outcome == RevertOutcomeRevert && primary[h.AdoptedEdgeID] == nil {
			primary[h.AdoptedEdgeID] = h
		}
	}
	plan := &RevertPlan{}
	for _, h := range hops {
		p := primary[h.AdoptedEdgeID]
		if h.AdoptedEdgeID != "" && p != nil && p != h && h.record != nil && revertibleStatus(h.record.Status) {
			p.cover(h.record)
			continue
		}
		plan.Hops = append(plan.Hops, h)
	}
	for _, h := range plan.Hops {
		if h.Outcome != RevertOutcomeRevert {
			continue
		}
		siblings, _, err := r.ListDelegationAdoptions(ctx, store.DelegationAdoptionFilter{AdoptedEdgeID: h.AdoptedEdgeID})
		if err != nil {
			return nil, fmt.Errorf("adoption records for edge %s: %w", h.AdoptedEdgeID, err)
		}
		for _, sib := range siblings {
			if revertibleStatus(sib.Status) {
				h.cover(sib)
			}
		}
		// The commit records the hop's original edge on every covered
		// record. A covered record that names a different original edge
		// refuses the hop instead of being rewritten.
		for _, c := range h.covered {
			if c.OriginalEdgeID != "" && c.OriginalEdgeID != h.OriginalEdgeID {
				h.Outcome = RevertOutcomeRefused
				h.Reason = ReasonCoveredOriginalDiffers
				break
			}
		}
	}
	for _, h := range plan.Hops {
		h.Fingerprint = revertFingerprint(h)
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
	if !revertibleStatus(rec.Status) {
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
	covered := make([]string, 0, len(h.covered))
	for _, c := range h.covered {
		covered = append(covered, c.ID+":"+string(c.Status))
	}
	return digest(struct {
		RecordID string     `json:"record_id"`
		Status   string     `json:"status"`
		Outcome  string     `json:"outcome"`
		Reason   string     `json:"reason"`
		Adopted  *edgeFacts `json:"adopted"`
		Original *edgeFacts `json:"original"`
		Covered  []string   `json:"covered"`
	}{h.RecordID, status, string(h.Outcome), string(h.Reason), facts(h.adopted), facts(h.original), covered})
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
