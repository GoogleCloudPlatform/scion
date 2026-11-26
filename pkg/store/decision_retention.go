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

import (
	"context"
	"time"
)

// HubSettingKeyDecisionRetentionCursor is the hub_settings section key storing
// the decision retention sweeper's (created_at, id) resume cursor (design-8984 §3.3).
const HubSettingKeyDecisionRetentionCursor = "decision_retention_cursor"

// Decision retention run triggers and statuses (decision_retention_runs).
const (
	DecisionRetentionTriggerSweep   = "sweep"
	DecisionRetentionTriggerManual  = "manual"
	DecisionRetentionTriggerPreview = "preview"

	DecisionRetentionStatusRunning = "running"
	DecisionRetentionStatusOK      = "ok"
	DecisionRetentionStatusPartial = "partial"
	DecisionRetentionStatusFailed  = "failed"
)

// DecisionRetentionCandidate is one settled decision row returned by the
// oldest-first candidate scan.
type DecisionRetentionCandidate struct {
	ID        string
	MessageID string
	ProjectID string
	CreatedAt time.Time
}

// DecisionRetentionPosition is a keyset position in the (created_at, id) walk.
// The zero value is the start of the walk.
type DecisionRetentionPosition struct {
	CreatedAt time.Time `json:"created_at,omitempty"`
	ID        string    `json:"id,omitempty"`
}

// IsStart reports whether p is the start of the walk.
func (p DecisionRetentionPosition) IsStart() bool { return p.CreatedAt.IsZero() && p.ID == "" }

// DecisionRetentionCursor is the persisted resume point of the decision
// retention sweeper, stored in hub_settings["decision_retention_cursor"].
type DecisionRetentionCursor struct {
	Position  DecisionRetentionPosition `json:"position"`
	ResetAt   *time.Time                `json:"reset_at,omitempty"`
	UpdatedAt time.Time                 `json:"updated_at,omitempty"`
}

// DecisionRetentionCounts are the rows one purge deleted, attributed to one
// project. It never carries a decision ID, answer ID, or content.
type DecisionRetentionCounts struct {
	Decisions int64 `json:"decisions,omitempty"`
	Answers   int64 `json:"answers,omitempty"`
}

// Add adds o to c.
func (c *DecisionRetentionCounts) Add(o DecisionRetentionCounts) {
	c.Decisions += o.Decisions
	c.Answers += o.Answers
}

// DecisionRetentionPolicy is the policy a run applied, recorded on its row.
type DecisionRetentionPolicy struct {
	RetentionDays int       `json:"retention_days"`
	Cutoff        time.Time `json:"cutoff"`
}

// DecisionRetentionRun is one decision_retention_runs row: the audit record of
// a retention run, with policy and per-project counts only and never IDs or
// content.
type DecisionRetentionRun struct {
	ID         string
	Trigger    string
	DryRun     bool
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     string
	Actor      string
	Policy     DecisionRetentionPolicy
	Counts     map[string]DecisionRetentionCounts
	Error      string
}

// DecisionRetentionRunFilter selects decision_retention_runs rows. Zero fields
// do not filter.
type DecisionRetentionRunFilter struct {
	Trigger       string
	FinishedAfter time.Time
	Limit         int
}

// DecisionRetentionPurgeResult is the outcome of one purge transaction.
type DecisionRetentionPurgeResult struct {
	ByProject       map[string]DecisionRetentionCounts
	PurgedMessages  int
	SkippedMessages int
}

// Total sums r over every project.
func (r DecisionRetentionPurgeResult) Total() DecisionRetentionCounts {
	var t DecisionRetentionCounts
	for _, c := range r.ByProject {
		t.Add(c)
	}
	return t
}

// DecisionRetentionStore is the persistence of settled-decision retention
// (design-8984 §§3.2-3.4).
type DecisionRetentionStore interface {
	// SelectDecisionRetentionCandidates returns up to limit settled decisions
	// created before globalCutoff and after position after, oldest first in
	// (created_at, id) order. Served by the partial index
	// decisions(created_at, id) WHERE settled = true.
	SelectDecisionRetentionCandidates(ctx context.Context, globalCutoff time.Time, after DecisionRetentionPosition, limit int) ([]DecisionRetentionCandidate, error)

	// PurgeDecisionMessages purges eligible prompt messages in one transaction.
	// A message is eligible iff every decision row sharing its message_id has
	// a non-zero cutoff in cutoffByProject[project_id], satisfies the exact
	// settled predicate (replaced, withdrawn, dismissed, or decided with a
	// direct answer), and has last activity strictly older than its project's
	// cutoff. On Postgres, all decision rows for messageIDs are locked FOR
	// UPDATE ordered by (key_norm, version, asker_agent_id, id) before
	// re-checking eligibility. Eligible answers
	// and decisions are deleted and decision_facet_entries are decremented,
	// pruned at ref_count <= 0, and recomputed when the purged row was the
	// newest contributor. Chat messages and decision_key_counters are kept.
	PurgeDecisionMessages(ctx context.Context, messageIDs []string, cutoffByProject map[string]time.Time) (DecisionRetentionPurgeResult, error)

	// GetDecisionRetentionCursor returns the persisted cursor from
	// hub_settings["decision_retention_cursor"], or a zero cursor when absent.
	GetDecisionRetentionCursor(ctx context.Context) (DecisionRetentionCursor, error)

	// SaveDecisionRetentionCursor persists cursor into
	// hub_settings["decision_retention_cursor"].
	SaveDecisionRetentionCursor(ctx context.Context, cursor DecisionRetentionCursor) error

	// LatestDecisionRetentionRun returns the most recent non-dry-run run with
	// the given trigger started at or after since, or nil when there is none.
	LatestDecisionRetentionRun(ctx context.Context, trigger string, since time.Time) (*DecisionRetentionRun, error)

	// ListDecisionRetentionRuns returns runs matching filter, newest first by
	// started_at.
	ListDecisionRetentionRuns(ctx context.Context, filter DecisionRetentionRunFilter) ([]DecisionRetentionRun, error)

	// SaveDecisionRetentionRun inserts run when its ID is empty (setting it)
	// and otherwise updates its finished_at, status, policy, counts and error.
	SaveDecisionRetentionRun(ctx context.Context, run *DecisionRetentionRun) error

	// PruneDecisionRetentionRuns deletes runs started before before and
	// returns how many it deleted.
	PruneDecisionRetentionRuns(ctx context.Context, before time.Time) (int, error)
}
