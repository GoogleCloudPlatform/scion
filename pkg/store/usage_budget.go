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

// UsageGroupSeedStateSection is the internal hub_settings section that
// persists D6 pooled-group seeder activation/finalization records across
// replicas and restarts.
const UsageGroupSeedStateSection = "system.usage_group_budget_seeds"

// UsageGroupRollupDay holds one daily bucket computed for
// usage_group_rollups_daily (origin="seed").
type UsageGroupRollupDay struct {
	DayUTC         time.Time
	TokensSummable int64
	Input          int64
	Output         int64
	CacheRead      int64
	CacheWrite     int64
	Calls          int64
	Events         int64
}

// UsageGroupSeedRecord tracks D6 seed state for one (group, project_scope, window).
type UsageGroupSeedRecord struct {
	GroupID           string    `json:"group_id"`
	ProjectScopeID    string    `json:"project_scope_id"`
	WindowStartUTC    time.Time `json:"window_start_utc"`
	WindowEndUTC      time.Time `json:"window_end_utc"`
	ActivatedAt       time.Time `json:"activated_at"`
	CutoffAt          time.Time `json:"cutoff_at"`
	FrozenMemberIDs   []string  `json:"frozen_member_ids,omitempty"`
	MemberSnapshotSet bool      `json:"member_snapshot_set"`
	Finalized         bool      `json:"finalized"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// UsageGroupSeedState holds all tracked D6 seed records keyed by
// "<group_id>|<project_scope_id>|<YYYY-MM-DD>".
type UsageGroupSeedState struct {
	Records map[string]*UsageGroupSeedRecord `json:"records"`
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
	// from usage_group_rollups_daily across BOTH origins ("ingest" and "seed")
	// for (groupID, projectScopeID) over the half-open UTC day range
	// [fromDayUTC, toDayUTC). projectScopeID == "" selects the system-wide pool.
	SumUsageGroupRollupTokens(ctx context.Context, groupID, projectScopeID string, fromDayUTC, toDayUTC time.Time) (int64, error)

	// ComputeUnchargedGroupRollupSeed aggregates usage_events in [fromTS, toTS)
	// for memberUserIDs (optionally restricted to projectScopeID when non-empty)
	// where groupID is NOT present in usage_events.charged_group_ids.
	// Anomalous events are excluded from token counters identically to ingest.
	ComputeUnchargedGroupRollupSeed(ctx context.Context, groupID, projectScopeID string, memberUserIDs []string, fromTS, toTS time.Time) ([]UsageGroupRollupDay, error)

	// ReplaceUsageGroupRollupSeed idempotently replaces (SET, never adds) all
	// origin="seed" rows for (groupID, projectScopeID) in [windowStartUTC,
	// windowEndUTC) in a single transaction, never touching origin="ingest" rows.
	ReplaceUsageGroupRollupSeed(ctx context.Context, groupID, projectScopeID string, windowStartUTC, windowEndUTC time.Time, rows []UsageGroupRollupDay) error

	// GetUsageGroupSeedState loads the persisted D6 pooled-group seed state.
	GetUsageGroupSeedState(ctx context.Context) (*UsageGroupSeedState, error)

	// SaveUsageGroupSeedState persists the D6 pooled-group seed state.
	SaveUsageGroupSeedState(ctx context.Context, state *UsageGroupSeedState) error

	// QueryGroupMemberUsageBreakdown aggregates per-member consumption from
	// usage_events over [windowStartUTC, windowEndUTC) for (groupID,
	// projectScopeID), covering both ingest-charged events (groupID in
	// charged_group_ids) and pre-activation seeded events (uncharged events in
	// [seedFromTS, seedToTS) by seededMemberUserIDs).
	QueryGroupMemberUsageBreakdown(ctx context.Context, groupID, projectScopeID string, seededMemberUserIDs []string, seedFromTS, seedToTS, windowStartUTC, windowEndUTC time.Time) ([]UsageGroupMemberBreakdown, error)

	// GetUsageBudgetState returns the usage_budget_state row for key, or
	// ErrNotFound if no row exists yet.
	GetUsageBudgetState(ctx context.Context, key UsageBudgetStateKey) (*UsageBudgetState, error)

	// ListUsageBudgetStatesForScope returns all usage_budget_state rows for
	// (scopeKind, scopeID, projectScopeID) whose window is active at activeAt
	// (window_start_utc <= activeAt < window_end_utc).
	ListUsageBudgetStatesForScope(ctx context.Context, scopeKind, scopeID, projectScopeID string, activeAt time.Time) ([]*UsageBudgetState, error)

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
