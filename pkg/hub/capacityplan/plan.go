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

// Package capacityplan plans how a legacy Runtime Broker's
// max_agents_per_broker limit carries over to the flat Runtime Broker
// instances that replace it. Each flat instance has its own Runtime Broker
// ID and therefore its own limit, so a legacy limit is never copied to the
// instances automatically: copying it to n instances would multiply the
// aggregate by n. The planner proposes a conservative split whose sum never
// exceeds the legacy limit, or asks for an operator decision. It is pure: it
// reads nothing and writes nothing.
package capacityplan

import (
	"fmt"
	"sort"
)

// Plan outcomes.
const (
	// OutcomePreserveBySplit: the legacy limit is split across the
	// instances; the allocations sum to at most the legacy limit.
	OutcomePreserveBySplit = "preserve_by_split"
	// OutcomeOperatorDecisionRequired: no split preserves the legacy
	// behaviour; Reasons says why. Nothing is proposed.
	OutcomeOperatorDecisionRequired = "operator_decision_required"
	// OutcomeNoLimit: the legacy broker was explicitly unlimited; the
	// instances inherit, and there is nothing to preserve.
	OutcomeNoLimit = "no_limit"
)

// Reason codes, the prefix of each entry in CapacityPlan.Reasons.
const (
	ReasonNoInstances           = "no_instances"
	ReasonInvalidOverride       = "invalid_override"
	ReasonLimitBelowInstances   = "limit_below_instance_count"
	ReasonInheritedAggregate    = "inherited_aggregate_changes"
	ReasonInvalidChoice         = "invalid_split_choice"
	ReasonNotEnforced           = "not_enforced"
	ReasonActiveAboveAllocation = "active_above_allocation"
)

// LegacyCapacity is what the Hub knows about a legacy Runtime Broker's
// max_agents_per_broker limit.
type LegacyCapacity struct {
	LegacyBrokerID string
	// Override is the broker's own maxAgents setting: nil inherit, 0
	// unlimited, >0 cap.
	Override *int64
	// BrokerBindings are the values of the entitlement bindings on the limit
	// with scope type broker.
	BrokerBindings []int64
	// SystemDefault is the limit definition's default value.
	SystemDefault int64
	// Enforced is the hub-wide enforce_broker_quotas switch.
	Enforced bool
	// ActiveReserved is the number of active reservations on the legacy ID.
	ActiveReserved int
	// ReceivingInstanceID is the instance that takes over the legacy
	// broker's agents; "" means the first instance in the stable order.
	ReceivingInstanceID string
}

// SplitChoice is an operator's split of the legacy limit: a positive cap
// for every instance, summing to at most the legacy limit.
type SplitChoice struct {
	Allocations map[string]int64
}

// CapacityPlan is the planner's answer.
type CapacityPlan struct {
	// Outcome is OutcomePreserveBySplit, OutcomeOperatorDecisionRequired or
	// OutcomeNoLimit.
	Outcome string
	// Allocations maps an instance's Runtime Broker ID to its proposed
	// maxAgents. Set only for OutcomePreserveBySplit; the values sum to at
	// most the legacy effective limit.
	Allocations map[string]int64
	// Reasons explains a required decision and carries the flags (not
	// enforced, active reservations above the receiving allocation). Each
	// entry starts with a reason code.
	Reasons []string
}

// PlanInstanceCapacity plans the per-instance limits that replace l's
// limit on instanceIDs. choice, when not nil, is the operator's split; it
// is used only when the legacy limit is a positive override.
//
//   - A positive Override L with L >= n instances: preserve_by_split. The
//     default proposal gives each instance floor(L/n), the remainder going to
//     the first instance in sorted order; a valid choice replaces it.
//   - A positive Override L with L < n: operator_decision_required, because
//     0 means unlimited and L cannot be split into n positive caps.
//   - Override 0: no_limit.
//   - No Override: operator_decision_required. Each instance would inherit
//     the binding or default limit by itself, so the aggregate changes to n
//     times it; that is presented for decision, never accepted silently.
//
// Enforced false and an ActiveReserved above the receiving instance's
// allocation are flagged in Reasons; they do not change the outcome.
func PlanInstanceCapacity(l LegacyCapacity, instanceIDs []string, choice *SplitChoice) CapacityPlan {
	ids := sortedUnique(instanceIDs)
	plan := planOutcome(l, ids, choice)
	if !l.Enforced {
		plan.Reasons = append(plan.Reasons, ReasonNotEnforced+": broker quotas are not enforced on this hub; the plan changes no admission either way")
	}
	if plan.Outcome == OutcomePreserveBySplit && len(ids) > 0 {
		receiver := l.ReceivingInstanceID
		if receiver == "" {
			receiver = ids[0]
		}
		if alloc, ok := plan.Allocations[receiver]; ok && int64(l.ActiveReserved) > alloc {
			plan.Reasons = append(plan.Reasons, fmt.Sprintf("%s: %d active reservations on legacy Runtime Broker %s exceed the %d proposed for instance %s; drain the legacy agents before enforcing",
				ReasonActiveAboveAllocation, l.ActiveReserved, l.LegacyBrokerID, alloc, receiver))
		}
	}
	return plan
}

func planOutcome(l LegacyCapacity, ids []string, choice *SplitChoice) CapacityPlan {
	n := int64(len(ids))
	if n == 0 {
		return decision(ReasonNoInstances + ": no flat instances to plan for")
	}
	if l.Override == nil {
		inherited, unlimited := inheritedLimit(l)
		if unlimited {
			return decision(fmt.Sprintf("%s: legacy Runtime Broker %s has no maxAgents setting and inherits an unlimited limit; each of the %d instances would inherit it too",
				ReasonInheritedAggregate, l.LegacyBrokerID, n))
		}
		return decision(fmt.Sprintf("%s: legacy Runtime Broker %s has no maxAgents setting and inherits %d; each of the %d instances would inherit %d, an aggregate of %d",
			ReasonInheritedAggregate, l.LegacyBrokerID, inherited, n, inherited, n*inherited))
	}
	limit := *l.Override
	switch {
	case limit < 0:
		return decision(fmt.Sprintf("%s: maxAgents %d is negative", ReasonInvalidOverride, limit))
	case limit == 0:
		return CapacityPlan{Outcome: OutcomeNoLimit}
	case limit < n:
		return decision(fmt.Sprintf("%s: a per-instance cap of 0 means unlimited; the aggregate %d cannot be split into %d positive caps",
			ReasonLimitBelowInstances, limit, n))
	}
	if choice != nil {
		if reason := validateChoice(choice, ids, limit); reason != "" {
			return decision(reason)
		}
		alloc := make(map[string]int64, len(ids))
		for _, id := range ids {
			alloc[id] = choice.Allocations[id]
		}
		return CapacityPlan{Outcome: OutcomePreserveBySplit, Allocations: alloc}
	}
	alloc := make(map[string]int64, len(ids))
	for _, id := range ids {
		alloc[id] = limit / n
	}
	alloc[ids[0]] += limit % n
	return CapacityPlan{Outcome: OutcomePreserveBySplit, Allocations: alloc}
}

// inheritedLimit is the limit a broker with no maxAgents setting gets: the
// most generous broker-scoped binding (0 is unlimited and wins), else the
// system default (0 is unlimited).
func inheritedLimit(l LegacyCapacity) (limit int64, unlimited bool) {
	if len(l.BrokerBindings) > 0 {
		var best int64
		for _, v := range l.BrokerBindings {
			if v == 0 {
				return 0, true
			}
			if v > best {
				best = v
			}
		}
		return best, false
	}
	return l.SystemDefault, l.SystemDefault == 0
}

// validateChoice returns "" when choice gives every instance, and only
// those, a positive cap and the caps sum to at most limit; otherwise the
// reason it is refused.
func validateChoice(choice *SplitChoice, ids []string, limit int64) string {
	if len(choice.Allocations) != len(ids) {
		return fmt.Sprintf("%s: the choice must set a cap for each of the %d instances and no others", ReasonInvalidChoice, len(ids))
	}
	var sum int64
	for _, id := range ids {
		v, ok := choice.Allocations[id]
		if !ok {
			return fmt.Sprintf("%s: no cap for instance %s", ReasonInvalidChoice, id)
		}
		if v <= 0 {
			return fmt.Sprintf("%s: cap %d for instance %s is not positive (0 means unlimited)", ReasonInvalidChoice, v, id)
		}
		sum += v
	}
	if sum > limit {
		return fmt.Sprintf("%s: the caps sum to %d, above the legacy limit %d", ReasonInvalidChoice, sum, limit)
	}
	return ""
}

func decision(reason string) CapacityPlan {
	return CapacityPlan{Outcome: OutcomeOperatorDecisionRequired, Reasons: []string{reason}}
}

func sortedUnique(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
