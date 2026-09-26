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

// Package brokerownership implements a one-time, idempotent backfill of
// CreatedBy on runtime brokers left ownerless (CreatedBy == "") by legacy
// registration flows.
//
// A broker's true origin is attributed from timing correlation, not from
// current linkage: a legitimate broker is created in the same request as
// its owning project, so RuntimeBroker.Created (immutable) falls a short,
// bounded time after that project's Created (also immutable). Current
// linkage (which broker a project links today) is only ever used to reject
// a candidate, never to select one, because it is exactly the state a
// caller who does not own a broker can still change after the fact by
// linking a broker they know the ID of to their own project. See the
// project design note for the full rationale, the rule's five outcomes,
// and the residual risk this backfill does not (and cannot) eliminate.
//
// Every call to Backfill re-evaluates the rule against the store's current
// state. There is no cross-call memoization or persisted intermediate
// decision list: two separate calls (for example a DryRun preview followed
// later by a real pass) may legitimately reach different outcomes for the
// same broker if the underlying data changed in between. That is correct
// behavior, not a defect.
package brokerownership

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// LabelBrokerRole is the RuntimeBroker label key used to mark the co-located
// (operator-owned) broker. Brokers carrying this label are excluded from
// attribution outright.
const LabelBrokerRole = "scion.io/broker-role"

// BrokerRoleEmbedded is the LabelBrokerRole value that excludes a broker
// from attribution.
const BrokerRoleEmbedded = "embedded"

// DefaultWindowSeconds is the timing window (W) used to correlate a broker's
// Created timestamp with a candidate project's Created timestamp. Both
// timestamps are time.Now() calls a handful of statements apart in one
// request, so a genuine same-request pair is sub-second even under load; 1s
// is generous headroom over that without inviting coincidental candidates.
// This is a tight default by design, not a starting point to loosen: the
// unsafe direction is mis-attribution (a coincidental candidate landing
// inside W), and the safe direction is under-attribution (a genuine pair
// landing just outside W, which falls to no-timing-match and admin review).
// Execute logs the observed signed delta for every attributed broker plus
// the run's maximum in-window delta (see Result.Deltas and the boot
// wiring's log output), so a too-tight default is visible after the fact
// for diagnosis. Note that changing this constant alone does not, by
// itself, cause anything to be re-evaluated: once the boot migration's
// completion marker is written, IsMigrationComplete short-circuits every
// later boot regardless of this value. Widening coverage after the fact
// requires a deliberate operational step beyond this constant (a marker
// reset or a new migration pass) — see the design note.
const DefaultWindowSeconds = 1.0

// defaultPageSize is used for list pagination when Config.PageSize is unset.
const defaultPageSize = 200

// cursorBinding scopes every List* cursor this package produces to this
// package's own pagination, so a cursor from one of these calls can never be
// mistaken for a cursor belonging to a different listing (store.ListOptions
// rejects a cursor whose binding does not match).
const cursorBinding = "brokerownership.backfill"

// Reason is a stable, neutral code recording why a broker's CreatedBy was
// (or was not) backfilled. Reasons are logged and reported; they never
// carry deployment-specific or otherwise sensitive detail.
type Reason string

const (
	// ReasonAttributed means CreatedBy was (or, in DryRun, would be) set to
	// OwnerID.
	ReasonAttributed Reason = "attributed"
	// ReasonExcludedEmbedded means the broker carries the embedded-role
	// label and is never considered.
	ReasonExcludedEmbedded Reason = "excluded-embedded"
	// ReasonNoTimingMatch means no project's Created falls in the window
	// ending at the broker's Created.
	ReasonNoTimingMatch Reason = "no-timing-match"
	// ReasonAmbiguousTiming means more than one project's Created falls in
	// the window; the single-candidate requirement is not met.
	ReasonAmbiguousTiming Reason = "ambiguous-timing"
	// ReasonLinkMismatch means the single timing candidate does not
	// currently link the broker at all.
	ReasonLinkMismatch Reason = "link-mismatch"
	// ReasonMultiLinked means the broker is currently linked by more than
	// one project (including, but not only, the timing candidate).
	ReasonMultiLinked Reason = "multi-linked"
	// ReasonLinkedByMismatch means the (project, broker) contributor row
	// records an explicit linker other than the candidate project's
	// CreatedBy.
	ReasonLinkedByMismatch Reason = "linked-by-mismatch"
	// ReasonAlreadySet means the rule decided to attribute the broker, but
	// by the time this pass tried to write it, CreatedBy was no longer
	// empty — another writer (e.g. a concurrent pass on another node) set
	// it first. This is a benign no-op, not a failure and not an ambiguous
	// outcome: it is reported separately from ReasonAttributed so a pass
	// never claims credit for a write it did not make.
	ReasonAlreadySet Reason = "already-set"
)

// Config controls a single backfill pass.
type Config struct {
	// DryRun computes and reports decisions without writing anything. This
	// is an out-of-band analysis mode for manual review against a copy of
	// the data — the boot-wired migration always runs with DryRun false, so
	// that legacy ownerless brokers are not left locked out of the
	// ownership gate pending an operator's action (see the design note
	// §7/§9 Q1).
	DryRun bool
	// WindowSeconds is W. Zero (the default value) means DefaultWindowSeconds.
	WindowSeconds float64
	// PageSize controls the page size used for list pagination. Zero means
	// defaultPageSize.
	PageSize int
}

// Decision records the outcome reached for a single ownerless broker.
type Decision struct {
	BrokerID string
	Reason   Reason
	// OwnerID is set only when Reason == ReasonAttributed.
	OwnerID string
	// CandidateProjectID is the single timing-candidate project considered,
	// when the rule got as far as evaluating one (Attributed, LinkMismatch,
	// MultiLinked, LinkedByMismatch). Empty for ExcludedEmbedded,
	// NoTimingMatch, and AmbiguousTiming.
	CandidateProjectID string
	// DeltaSeconds is the signed delta (broker.Created - candidate.Created)
	// for CandidateProjectID, when set.
	DeltaSeconds float64
}

// DeltaObservation records one signed timing delta observed during a pass,
// for tightening WindowSeconds from real data (design note §7/§9 Q1).
// InWindow distinguishes a delta that counted as a timing candidate from a
// near-miss just outside the window boundary.
type DeltaObservation struct {
	BrokerID     string
	ProjectID    string
	DeltaSeconds float64
	InWindow     bool
}

// Result summarizes a full backfill pass.
type Result struct {
	// TotalScanned is the number of brokers considered (CreatedBy == "" at
	// scan time).
	TotalScanned int
	// Reasons buckets every Decision by Reason.
	Reasons map[Reason]int
	// Decisions holds one entry per scanned broker.
	Decisions []Decision
	// Deltas holds every signed timing delta observed during the pass,
	// including near-misses just outside the window, regardless of DryRun.
	Deltas []DeltaObservation
}

// Backfill scans every RuntimeBroker with an empty CreatedBy and attributes
// ownership to the project that originally created it when that can be
// established with confidence, or leaves it ownerless for admin review
// otherwise. See the package doc and the design note for the full rule.
//
// Only rows with CreatedBy == "" are ever considered or written; a broker
// that already has a non-empty CreatedBy is skipped outright, and
// store.Store.SetRuntimeBrokerCreatedByIfEmpty re-checks the same
// precondition atomically at write time. Both make the pass idempotent and
// safe to re-run.
func Backfill(ctx context.Context, s store.Store, cfg Config) (*Result, error) {
	window := cfg.WindowSeconds
	if window <= 0 {
		window = DefaultWindowSeconds
	}
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}

	projects, err := loadCandidateProjects(ctx, s, pageSize)
	if err != nil {
		return nil, fmt.Errorf("loading candidate projects: %w", err)
	}

	result := &Result{Reasons: make(map[Reason]int)}

	cursor := ""
	for {
		page, err := s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: pageSize, Cursor: cursor, CursorBinding: cursorBinding})
		if err != nil {
			return nil, fmt.Errorf("listing runtime brokers: %w", err)
		}
		for i := range page.Items {
			broker := page.Items[i]
			if broker.CreatedBy != "" {
				continue // already owned; out of scope
			}
			result.TotalScanned++

			decision, deltas, err := evaluate(ctx, s, &broker, projects, window, pageSize)
			if err != nil {
				return nil, fmt.Errorf("evaluating broker %s: %w", broker.ID, err)
			}
			result.Deltas = append(result.Deltas, deltas...)

			if decision.Reason == ReasonAttributed && !cfg.DryRun {
				applied, err := s.SetRuntimeBrokerCreatedByIfEmpty(ctx, broker.ID, decision.OwnerID)
				if err != nil {
					return nil, fmt.Errorf("setting created_by for broker %s: %w", broker.ID, err)
				}
				if !applied {
					// Another writer set CreatedBy on this row between our
					// read and this write. The rule's decision was correct
					// at evaluation time, but we did not perform this
					// write — record that, not a false attribution.
					decision.Reason = ReasonAlreadySet
					decision.OwnerID = ""
				}
			}

			result.Reasons[decision.Reason]++
			result.Decisions = append(result.Decisions, decision)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	return result, nil
}

// projectRef is the minimal, sorted-by-Created view of a project used for
// the timing-window search.
type projectRef struct {
	id        string
	createdBy string
	created   time.Time
}

// loadCandidateProjects returns every project with a non-empty CreatedBy,
// sorted ascending by Created. Projects with an empty CreatedBy (for
// example the co-located/global project+broker pair, both created
// ownerless in the same call) are never timing candidates: attributing a
// broker's CreatedBy to an empty string would be a no-op that only adds
// report noise.
func loadCandidateProjects(ctx context.Context, s store.Store, pageSize int) ([]projectRef, error) {
	var out []projectRef
	cursor := ""
	for {
		page, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: pageSize, Cursor: cursor, CursorBinding: cursorBinding})
		if err != nil {
			return nil, err
		}
		for _, p := range page.Items {
			if p.CreatedBy == "" {
				continue
			}
			out = append(out, projectRef{id: p.ID, createdBy: p.CreatedBy, created: p.Created})
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	sort.Slice(out, func(i, j int) bool { return out[i].created.Before(out[j].created) })
	return out, nil
}

// evaluate applies the migration rule to a single broker and returns the
// decision reached, plus every signed timing delta observed (in-window
// candidates and the immediate near-miss neighbors just outside the
// window), for W-tuning visibility.
func evaluate(ctx context.Context, s store.Store, broker *store.RuntimeBroker, projects []projectRef, window float64, pageSize int) (Decision, []DeltaObservation, error) {
	decision := Decision{BrokerID: broker.ID}

	if broker.Labels[LabelBrokerRole] == BrokerRoleEmbedded {
		decision.Reason = ReasonExcludedEmbedded
		return decision, nil, nil
	}

	// One-sided window: a genuine pair always has project.Created <
	// broker.Created (CreateProject Saves strictly before CreateRuntimeBroker
	// in the same request, both server-set at Save time), so a negative
	// delta can never be a real pair and is excluded outright rather than
	// merely being "outside W".
	lowerBound := broker.Created.Add(-time.Duration(window * float64(time.Second)))
	lo := sort.Search(len(projects), func(i int) bool { return !projects[i].created.Before(lowerBound) })
	hi := sort.Search(len(projects), func(i int) bool { return projects[i].created.After(broker.Created) })

	var deltas []DeltaObservation
	for i := lo; i < hi; i++ {
		deltas = append(deltas, DeltaObservation{
			BrokerID:     broker.ID,
			ProjectID:    projects[i].id,
			DeltaSeconds: broker.Created.Sub(projects[i].created).Seconds(),
			InWindow:     true,
		})
	}
	// Immediate near-miss neighbors just outside the window boundary, for
	// tuning visibility only — they never affect the decision.
	if lo > 0 {
		deltas = append(deltas, DeltaObservation{
			BrokerID:     broker.ID,
			ProjectID:    projects[lo-1].id,
			DeltaSeconds: broker.Created.Sub(projects[lo-1].created).Seconds(),
			InWindow:     false,
		})
	}
	if hi < len(projects) {
		deltas = append(deltas, DeltaObservation{
			BrokerID:     broker.ID,
			ProjectID:    projects[hi].id,
			DeltaSeconds: broker.Created.Sub(projects[hi].created).Seconds(),
			InWindow:     false,
		})
	}

	candidates := projects[lo:hi]
	switch len(candidates) {
	case 0:
		decision.Reason = ReasonNoTimingMatch
		return decision, deltas, nil
	case 1:
		// Continue below.
	default:
		decision.Reason = ReasonAmbiguousTiming
		return decision, deltas, nil
	}

	candidate := candidates[0]
	decision.CandidateProjectID = candidate.id
	decision.DeltaSeconds = broker.Created.Sub(candidate.created).Seconds()

	// Step 3: corroborate against CURRENT linkage. This is a rejection-only
	// check — it never selects an owner, only disqualifies the timing
	// candidate — because current linkage is exactly what a caller who does
	// not own the broker can still change.
	linkers, err := currentLinkers(ctx, s, broker.ID, pageSize)
	if err != nil {
		return Decision{}, nil, err
	}

	isLinker := false
	hasOtherLinker := false
	for _, pid := range linkers {
		if pid == candidate.id {
			isLinker = true
		} else {
			hasOtherLinker = true
		}
	}
	if !isLinker {
		decision.Reason = ReasonLinkMismatch
		return decision, deltas, nil
	}
	if hasOtherLinker {
		decision.Reason = ReasonMultiLinked
		return decision, deltas, nil
	}

	provider, err := s.GetProjectProvider(ctx, candidate.id, broker.ID)
	if err != nil && err != store.ErrNotFound {
		return Decision{}, nil, err
	}
	if err == nil && provider.LinkedBy != "" && provider.LinkedBy != candidate.createdBy {
		decision.Reason = ReasonLinkedByMismatch
		return decision, deltas, nil
	}

	decision.Reason = ReasonAttributed
	decision.OwnerID = candidate.createdBy
	return decision, deltas, nil
}

// currentLinkers returns the IDs of every project that currently lists
// brokerID as a provider.
func currentLinkers(ctx context.Context, s store.Store, brokerID string, pageSize int) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		page, err := s.ListProjects(ctx, store.ProjectFilter{BrokerID: brokerID}, store.ListOptions{Limit: pageSize, Cursor: cursor, CursorBinding: cursorBinding})
		if err != nil {
			return nil, err
		}
		for _, p := range page.Items {
			ids = append(ids, p.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return ids, nil
}
