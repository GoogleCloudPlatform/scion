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
	"errors"
	"time"
)

// Budget state values for usage_budget_state.state (Tokenomics design §4.2, §4.3).
// States are monotonic within a window through the evaluator: ok -> warn -> exhausted.
const (
	UsageBudgetStateOK        = "ok"
	UsageBudgetStateWarn      = "warn"
	UsageBudgetStateExhausted = "exhausted"

	// Threshold notification bitmask positions for UsageBudgetState.ThresholdsNotified.
	// Used to deduplicate soft (50%, 80%) and hard (100%) threshold alerts and to
	// selectively clear bits when a cap raise lowers consumption below a threshold.
	UsageBudgetThresholdBit50  int64 = 1 << 0
	UsageBudgetThresholdBit80  int64 = 1 << 1
	UsageBudgetThresholdBit100 int64 = 1 << 2
)

// ErrBudgetStateConflict is returned by CompareAndSwapUsageBudgetState when
// the stored state_version no longer matches expectedVersion.
var ErrBudgetStateConflict = errors.New("usage budget state version conflict")

// UsageBudgetStateKey uniquely identifies a usage_budget_state row.
// For ScopeKind == "user_project", ScopeID is the user ID and ProjectScopeID
// is the project ID. For ScopeKind == "project" or "user", ProjectScopeID is "".
type UsageBudgetStateKey struct {
	LimitDefinitionID string
	ScopeKind         string
	ScopeID           string
	ProjectScopeID    string
	WindowStartUTC    time.Time
}

// UsageBudgetState represents one row in usage_budget_state.
type UsageBudgetState struct {
	ID                  string     `json:"id"`
	LimitDefinitionID   string     `json:"limitDefinitionId"`
	ScopeKind           string     `json:"scopeKind"`
	ScopeID             string     `json:"scopeId"`
	ProjectScopeID      string     `json:"projectScopeId"`
	WindowStartUTC      time.Time  `json:"windowStartUtc"`
	WindowEndUTC        time.Time  `json:"windowEndUtc"`
	State               string     `json:"state"`
	ConsumedAtEval      int64      `json:"consumedAtEval"`
	CapAtEval           int64      `json:"capAtEval"`
	ThresholdsNotified  int64      `json:"thresholdsNotified"`
	AckAnomalousThrough *time.Time `json:"ackAnomalousThrough,omitempty"`
	OverrideUntil       *time.Time `json:"overrideUntil,omitempty"`
	StateVersion        int64      `json:"stateVersion"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

// UsageGroupSeedEpisode tracks one group charging episode in
// usage_group_budget_seeds (Tokenomics P3.3, D6a).
//
// An episode starts when a group's first active pooled binding (any scope, any
// window kind) activates, and ends (EndedAt set) when its last active pooled
// binding deactivates. Seed rows written for this episode are tagged with
// origin="seed:<ID>" in usage_group_rollups_daily so episodes never overwrite
// each other.
type UsageGroupSeedEpisode struct {
	ID                string     `json:"id"`
	GroupID           string     `json:"group_id"`
	ActivatedAt       time.Time  `json:"activated_at"`
	SeedFromUTC       time.Time  `json:"seed_from_utc"`
	CutoffAt          time.Time  `json:"cutoff_at"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
	FrozenMemberIDs   []string   `json:"frozen_member_ids,omitempty"`
	MemberSnapshotSet bool       `json:"member_snapshot_set"`
	Finalized         bool       `json:"finalized"`
	StateVersion      int64      `json:"state_version"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// UsageGroupEpisodeSeedPassInput carries parameters for
// ExecuteUsageGroupEpisodeSeedPass.
type UsageGroupEpisodeSeedPassInput struct {
	EpisodeID             string
	GroupID               string
	LedgerRetentionCutoff time.Time // zero means no retention clamp
	Now                   time.Time
	IsFinalPass           bool
}

// UsageGroupEpisodeSeedPassResult reports the outcome of one atomic episode
// seed pass.
type UsageGroupEpisodeSeedPassResult struct {
	Episode             *UsageGroupSeedEpisode
	DidMutate           bool
	TouchedProjectScope []string
}

// UsageGroupMemberBreakdown holds one user's attributed consumption within a
// pooled group budget window.
type UsageGroupMemberBreakdown struct {
	UserID         string `json:"userId"`
	TokensSummable int64  `json:"tokensSummable"`
	Calls          int64  `json:"calls"`
	Events         int64  `json:"events"`
}

// UsageBudgetStore defines persistence operations for raw-token budget state
// and scope-counter window sums (Tokenomics P3.1, P3.3).
type UsageBudgetStore interface {
	// SumUsageScopeCounterTokens returns the saturating SUM(tokens_summable)
	// from usage_scope_counters_daily for (scopeKind, counterScopeID) over the
	// half-open UTC day range [fromDayUTC, toDayUTC). For ScopeKind ==
	// "user_project", counterScopeID is "<user_id>:<project_id>".
	SumUsageScopeCounterTokens(ctx context.Context, scopeKind, counterScopeID string, fromDayUTC, toDayUTC time.Time) (int64, error)

	// SumUsageGroupRollupTokens returns the saturating SUM(tokens_summable)
	// from usage_group_rollups_daily across ALL origins ("ingest" and every
	// "seed:<episode_id>") for (groupID, projectScopeID) over the half-open UTC
	// day range [fromDayUTC, toDayUTC). projectScopeID == "" selects the
	// system-wide pool.
	SumUsageGroupRollupTokens(ctx context.Context, groupID, projectScopeID string, fromDayUTC, toDayUTC time.Time) (int64, error)

	// OpenUsageGroupChargingEpisode ensures an open charging episode exists for
	// groupID under a single per-group advisory lock. If an open episode
	// (ended_at IS NULL) already exists, returns (existing, opened=false, nil)
	// and triggers no seed (D6a). Otherwise creates a new episode with
	// seed_from_utc = max(utcMonthStart(activatedAt), prev.ended_at) and
	// cutoff_at = activatedAt + 2m, returning (created, opened=true, nil).
	OpenUsageGroupChargingEpisode(ctx context.Context, groupID string, activatedAt time.Time) (*UsageGroupSeedEpisode, bool, error)

	// CloseUsageGroupChargingEpisodeIfInactive checks inside the per-group
	// locked transaction whether groupID still has any active pooled binding
	// (value > 0 on day or month, any scope). If none remain and an open
	// episode exists, sets ended_at = endedAt and returns (closedEp, true, nil).
	CloseUsageGroupChargingEpisodeIfInactive(ctx context.Context, groupID string, endedAt time.Time) (*UsageGroupSeedEpisode, bool, error)

	// ExecuteUsageGroupEpisodeSeedPass atomically executes one D6a episode seed
	// pass under the per-group advisory lock: load episode -> snapshot
	// effective members if unset -> compute uncharged events in
	// [max(seed_from_utc, retentionCutoff), cutoff_at) grouped by (day, project)
	// -> replace ONLY origin="seed:<episode_id>" rows for both (day, G, "") and
	// (day, G, <project>) -> finalize if IsFinalPass || now >= cutoff_at -> CAS
	// save episode.
	ExecuteUsageGroupEpisodeSeedPass(ctx context.Context, in UsageGroupEpisodeSeedPassInput) (*UsageGroupEpisodeSeedPassResult, error)

	// GetUsageGroupSeedEpisode returns an episode by ID or ErrNotFound.
	GetUsageGroupSeedEpisode(ctx context.Context, episodeID string) (*UsageGroupSeedEpisode, error)

	// GetOpenUsageGroupChargingEpisode returns the currently open episode
	// (ended_at IS NULL) for groupID, or ErrNotFound.
	GetOpenUsageGroupChargingEpisode(ctx context.Context, groupID string) (*UsageGroupSeedEpisode, error)

	// ListIncompleteUsageGroupSeedEpisodes returns all non-finalized episodes
	// (finalized = false).
	ListIncompleteUsageGroupSeedEpisodes(ctx context.Context) ([]*UsageGroupSeedEpisode, error)

	// ListUsageGroupSeedEpisodesOverlapping returns all episodes for groupID
	// whose [seed_from_utc, cutoff_at) overlaps [windowStartUTC, windowEndUTC)
	// and whose member snapshot has been recorded.
	ListUsageGroupSeedEpisodesOverlapping(ctx context.Context, groupID string, windowStartUTC, windowEndUTC time.Time) ([]*UsageGroupSeedEpisode, error)

	// QueryGroupMemberUsageBreakdown aggregates per-member consumption in SQL
	// (GROUP BY billing_user_id) from usage_events over [windowStartUTC,
	// windowEndUTC) for (groupID, projectScopeID), covering both ingest-charged
	// events (groupID in charged_group_ids) and pre-activation seeded events
	// from the overlapping episodes (each event counted at most once).
	QueryGroupMemberUsageBreakdown(ctx context.Context, groupID, projectScopeID string, episodes []*UsageGroupSeedEpisode, windowStartUTC, windowEndUTC time.Time) ([]UsageGroupMemberBreakdown, error)

	// GetUsageBudgetState returns the usage_budget_state row for key, or
	// ErrNotFound if no row exists yet.
	GetUsageBudgetState(ctx context.Context, key UsageBudgetStateKey) (*UsageBudgetState, error)

	// GetUsageBudgetStateByID returns the usage_budget_state row with the given
	// UUID, or ErrNotFound if no such row exists.
	GetUsageBudgetStateByID(ctx context.Context, id string) (*UsageBudgetState, error)

	// ListUsageBudgetStatesForScope returns all usage_budget_state rows for
	// (scopeKind, scopeID, projectScopeID) whose window is active at activeAt
	// (window_start_utc <= activeAt < window_end_utc).
	ListUsageBudgetStatesForScope(ctx context.Context, scopeKind, scopeID, projectScopeID string, activeAt time.Time) ([]*UsageBudgetState, error)

	// ListOpenUsageBudgetStatesByLimit returns up to limit open-window
	// usage_budget_state rows (window_end_utc > activeAt) for limitDefinitionID,
	// ordered by window_start_utc ASC, id ASC. When limit <= 0, no explicit cap
	// is applied.
	ListOpenUsageBudgetStatesByLimit(ctx context.Context, limitDefinitionID string, activeAt time.Time, limit int) ([]*UsageBudgetState, error)

	// CreateUsageBudgetState inserts a new usage_budget_state row with
	// state_version = 1 (or st.StateVersion if >= 1). Returns ErrAlreadyExists
	// if a row with the same composite key already exists.
	CreateUsageBudgetState(ctx context.Context, st *UsageBudgetState) (*UsageBudgetState, error)

	// CompareAndSwapUsageBudgetState updates an existing usage_budget_state row
	// only if its stored state_version equals expectedVersion, atomically
	// incrementing state_version by 1. Returns ErrBudgetStateConflict if 0 rows
	// were updated (version mismatch or row missing).
	CompareAndSwapUsageBudgetState(ctx context.Context, expectedVersion int64, next *UsageBudgetState) (*UsageBudgetState, error)
}
