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

package capacityplan

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func i64(v int64) *int64 { return &v }

// hasReason reports whether plan carries a reason with code.
func hasReason(plan CapacityPlan, code string) bool {
	for _, r := range plan.Reasons {
		if strings.HasPrefix(r, code+":") {
			return true
		}
	}
	return false
}

func sum(alloc map[string]int64) int64 {
	var s int64
	for _, v := range alloc {
		s += v
	}
	return s
}

// One fixture per row of the design's legacy-situation table (§4.5 G5).
func TestPlanInstanceCapacity_Fixtures(t *testing.T) {
	ids := []string{"inst-b", "inst-a", "inst-c"}
	tests := []struct {
		name      string
		legacy    LegacyCapacity
		ids       []string
		choice    *SplitChoice
		outcome   string
		alloc     map[string]int64
		reasons   []string
		noReasons []string
	}{
		{
			name:    "override L >= n: default split, remainder to the first sorted instance",
			legacy:  LegacyCapacity{LegacyBrokerID: "legacy", Override: i64(10), Enforced: true},
			ids:     ids,
			outcome: OutcomePreserveBySplit,
			alloc:   map[string]int64{"inst-a": 4, "inst-b": 3, "inst-c": 3},
		},
		{
			name:    "override L == n: one each",
			legacy:  LegacyCapacity{Override: i64(3), Enforced: true},
			ids:     ids,
			outcome: OutcomePreserveBySplit,
			alloc:   map[string]int64{"inst-a": 1, "inst-b": 1, "inst-c": 1},
		},
		{
			name:    "override with an operator split summing below L",
			legacy:  LegacyCapacity{Override: i64(10), Enforced: true},
			ids:     ids,
			choice:  &SplitChoice{Allocations: map[string]int64{"inst-a": 1, "inst-b": 2, "inst-c": 6}},
			outcome: OutcomePreserveBySplit,
			alloc:   map[string]int64{"inst-a": 1, "inst-b": 2, "inst-c": 6},
		},
		{
			name:    "override with an operator split above L is refused",
			legacy:  LegacyCapacity{Override: i64(10), Enforced: true},
			ids:     ids,
			choice:  &SplitChoice{Allocations: map[string]int64{"inst-a": 5, "inst-b": 5, "inst-c": 1}},
			outcome: OutcomeOperatorDecisionRequired,
			reasons: []string{ReasonInvalidChoice},
		},
		{
			name:    "override with an operator split containing 0 (unlimited) is refused",
			legacy:  LegacyCapacity{Override: i64(10), Enforced: true},
			ids:     ids,
			choice:  &SplitChoice{Allocations: map[string]int64{"inst-a": 0, "inst-b": 5, "inst-c": 5}},
			outcome: OutcomeOperatorDecisionRequired,
			reasons: []string{ReasonInvalidChoice},
		},
		{
			name:    "override with an operator split missing an instance is refused",
			legacy:  LegacyCapacity{Override: i64(10), Enforced: true},
			ids:     ids,
			choice:  &SplitChoice{Allocations: map[string]int64{"inst-a": 2, "inst-b": 2, "other": 2}},
			outcome: OutcomeOperatorDecisionRequired,
			reasons: []string{ReasonInvalidChoice},
		},
		{
			name:    "override L < n: decision required",
			legacy:  LegacyCapacity{Override: i64(2), Enforced: true},
			ids:     ids,
			outcome: OutcomeOperatorDecisionRequired,
			reasons: []string{ReasonLimitBelowInstances},
		},
		{
			name:    "override 0 (unlimited): no limit",
			legacy:  LegacyCapacity{Override: i64(0), Enforced: true},
			ids:     ids,
			outcome: OutcomeNoLimit,
		},
		{
			name:    "no override, broker bindings: the inherited aggregate needs a decision",
			legacy:  LegacyCapacity{BrokerBindings: []int64{5, 8}, SystemDefault: 100, Enforced: true},
			ids:     ids,
			outcome: OutcomeOperatorDecisionRequired,
			reasons: []string{ReasonInheritedAggregate},
		},
		{
			name:    "no override, system default: the inherited aggregate needs a decision",
			legacy:  LegacyCapacity{SystemDefault: 100, Enforced: true},
			ids:     ids,
			outcome: OutcomeOperatorDecisionRequired,
			reasons: []string{ReasonInheritedAggregate},
		},
		{
			name:    "not enforced: plan computed and flagged",
			legacy:  LegacyCapacity{Override: i64(6), Enforced: false},
			ids:     ids,
			outcome: OutcomePreserveBySplit,
			alloc:   map[string]int64{"inst-a": 2, "inst-b": 2, "inst-c": 2},
			reasons: []string{ReasonNotEnforced},
		},
		{
			name:    "active reservations above the receiving instance's allocation: flagged",
			legacy:  LegacyCapacity{Override: i64(6), Enforced: true, ActiveReserved: 5, ReceivingInstanceID: "inst-c"},
			ids:     ids,
			outcome: OutcomePreserveBySplit,
			alloc:   map[string]int64{"inst-a": 2, "inst-b": 2, "inst-c": 2},
			reasons: []string{ReasonActiveAboveAllocation},
		},
		{
			name:      "active reservations within the receiving allocation: not flagged",
			legacy:    LegacyCapacity{Override: i64(6), Enforced: true, ActiveReserved: 2},
			ids:       ids,
			outcome:   OutcomePreserveBySplit,
			alloc:     map[string]int64{"inst-a": 2, "inst-b": 2, "inst-c": 2},
			noReasons: []string{ReasonActiveAboveAllocation, ReasonNotEnforced},
		},
		{
			name:    "no instances",
			legacy:  LegacyCapacity{Override: i64(6), Enforced: true},
			ids:     nil,
			outcome: OutcomeOperatorDecisionRequired,
			reasons: []string{ReasonNoInstances},
		},
		{
			name:    "negative override",
			legacy:  LegacyCapacity{Override: i64(-1), Enforced: true},
			ids:     ids,
			outcome: OutcomeOperatorDecisionRequired,
			reasons: []string{ReasonInvalidOverride},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := PlanInstanceCapacity(tc.legacy, tc.ids, tc.choice)
			assert.Equal(t, tc.outcome, plan.Outcome)
			assert.Equal(t, tc.alloc, plan.Allocations)
			for _, r := range tc.reasons {
				assert.True(t, hasReason(plan, r), "reason %s in %v", r, plan.Reasons)
			}
			for _, r := range tc.noReasons {
				assert.False(t, hasReason(plan, r), "reason %s not in %v", r, plan.Reasons)
			}
			if plan.Outcome == OutcomePreserveBySplit {
				require.NotNil(t, tc.legacy.Override)
				assert.LessOrEqual(t, sum(plan.Allocations), *tc.legacy.Override, "never above the legacy limit")
				for id, v := range plan.Allocations {
					assert.Positive(t, v, "instance %s gets a positive cap", id)
				}
			} else {
				assert.Nil(t, plan.Allocations, "nothing proposed")
			}
		})
	}
}

// The inherited aggregate is stated: n times the inherited limit, never
// accepted silently, and never copied as an allocation.
func TestPlanInstanceCapacity_InheritedAggregateIsStated(t *testing.T) {
	plan := PlanInstanceCapacity(LegacyCapacity{LegacyBrokerID: "legacy", SystemDefault: 100, Enforced: true},
		[]string{"a", "b"}, nil)
	require.Equal(t, OutcomeOperatorDecisionRequired, plan.Outcome)
	require.Len(t, plan.Reasons, 1)
	assert.Contains(t, plan.Reasons[0], "aggregate of 200")
	assert.Nil(t, plan.Allocations)

	// The most generous binding is the inherited limit; 0 (unlimited) wins.
	plan = PlanInstanceCapacity(LegacyCapacity{BrokerBindings: []int64{5, 0}, SystemDefault: 100, Enforced: true},
		[]string{"a", "b"}, nil)
	require.Equal(t, OutcomeOperatorDecisionRequired, plan.Outcome)
	assert.Contains(t, plan.Reasons[0], "unlimited")
}

// An operator choice is ignored unless the legacy limit is a positive
// override: there is nothing to split otherwise.
func TestPlanInstanceCapacity_ChoiceOnlyForOverride(t *testing.T) {
	choice := &SplitChoice{Allocations: map[string]int64{"a": 1, "b": 1}}
	plan := PlanInstanceCapacity(LegacyCapacity{SystemDefault: 100, Enforced: true}, []string{"a", "b"}, choice)
	assert.Equal(t, OutcomeOperatorDecisionRequired, plan.Outcome)
	assert.Nil(t, plan.Allocations)
	plan = PlanInstanceCapacity(LegacyCapacity{Override: i64(0), Enforced: true}, []string{"a", "b"}, choice)
	assert.Equal(t, OutcomeNoLimit, plan.Outcome)
	assert.Nil(t, plan.Allocations)
}

// Instance IDs are de-duplicated and ordered, so the plan does not depend
// on input order.
func TestPlanInstanceCapacity_StableOrder(t *testing.T) {
	l := LegacyCapacity{Override: i64(7), Enforced: true}
	a := PlanInstanceCapacity(l, []string{"z", "y", "x", "y", ""}, nil)
	b := PlanInstanceCapacity(l, []string{"x", "y", "z"}, nil)
	assert.Equal(t, b, a)
	assert.Equal(t, map[string]int64{"x": 3, "y": 2, "z": 2}, a.Allocations)
}
