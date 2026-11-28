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

// UsageBudgetStore defines persistence operations for raw-token budget state
// and scope-counter window sums (Tokenomics P3.1).
type UsageBudgetStore interface {
	// SumUsageScopeCounterTokens returns the saturating SUM(tokens_summable)
	// from usage_scope_counters_daily for (scopeKind, counterScopeID) over the
	// half-open UTC day range [fromDayUTC, toDayUTC). For ScopeKind ==
	// "user_project", counterScopeID is "<user_id>:<project_id>".
	SumUsageScopeCounterTokens(ctx context.Context, scopeKind, counterScopeID string, fromDayUTC, toDayUTC time.Time) (int64, error)

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
