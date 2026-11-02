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

package config

import (
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Read-only pre-upgrade placement scan: the categories of stored agent
// configurations that operator-owned placement refuses at their next start
// or restart. The scan reports aggregate counts only, never configuration
// values.

// PlacementPreflightCategory is a stable category name of the scan.
type PlacementPreflightCategory string

const (
	// PreflightAgentTolerationsWithoutOperatorPolicy: the agent sets one or
	// more tolerations, but the operator placement sets none.
	PreflightAgentTolerationsWithoutOperatorPolicy PlacementPreflightCategory = "agent_tolerations_without_operator_policy"
	// PreflightAgentRuntimeClassWithoutOperatorPolicy: the agent sets a
	// runtime class, but the operator placement sets none.
	PreflightAgentRuntimeClassWithoutOperatorPolicy PlacementPreflightCategory = "agent_runtime_class_without_operator_policy"
	// PreflightAgentInvalidPlacementField: a node selector entry, a
	// toleration or the runtime class of the agent is not valid for
	// Kubernetes.
	PreflightAgentInvalidPlacementField PlacementPreflightCategory = "agent_invalid_placement_field"
	// PreflightAgentMatchAllToleration: the agent sets a toleration that
	// matches all taints while the operator placement is in effect.
	PreflightAgentMatchAllToleration PlacementPreflightCategory = "agent_match_all_toleration"
	// PreflightAgentPoolSelectorConflict: the agent sets the pool node
	// selector key to a value other than the operator's.
	PreflightAgentPoolSelectorConflict PlacementPreflightCategory = "agent_pool_selector_conflict"
	// PreflightAgentSelectorConflictsOperator: the agent sets another node
	// selector key the operator sets, to a different value.
	PreflightAgentSelectorConflictsOperator PlacementPreflightCategory = "agent_selector_conflicts_operator"
	// PreflightAgentTolerationNotInPolicy: the operator sets tolerations,
	// and one of the agent's is not among them.
	PreflightAgentTolerationNotInPolicy PlacementPreflightCategory = "agent_toleration_not_in_policy"
	// PreflightAgentRuntimeClassDiffers: the agent's runtime class differs
	// from the operator's.
	PreflightAgentRuntimeClassDiffers PlacementPreflightCategory = "agent_runtime_class_differs"
)

// PlacementPreflightCategories lists every agent category, in report order.
// The first two are always shown in a summary; the others when counted.
var PlacementPreflightCategories = []PlacementPreflightCategory{
	PreflightAgentTolerationsWithoutOperatorPolicy,
	PreflightAgentRuntimeClassWithoutOperatorPolicy,
	PreflightAgentInvalidPlacementField,
	PreflightAgentMatchAllToleration,
	PreflightAgentPoolSelectorConflict,
	PreflightAgentSelectorConflictsOperator,
	PreflightAgentTolerationNotInPolicy,
	PreflightAgentRuntimeClassDiffers,
}

// alwaysSummarized are the categories a summary shows even at zero.
var alwaysSummarized = map[PlacementPreflightCategory]bool{
	PreflightAgentTolerationsWithoutOperatorPolicy:  true,
	PreflightAgentRuntimeClassWithoutOperatorPolicy: true,
}

// CategorizeAgentPlacement returns every category the agent's own
// kubernetes block falls into against the operator placement policy, which
// the caller has resolved (nil means the operator sets no placement), each
// once, in PlacementPreflightCategories order. It applies the rules of
// CheckAgentPlacement: the block is refused there exactly when it falls
// into a category here. An operator placement with no tolerations (or no
// runtime class) gives the "without operator policy" category, never the
// "not in policy" (or "differs") one.
func CategorizeAgentPlacement(agent *api.KubernetesConfig, policy *api.ResolvedPlacement) []PlacementPreflightCategory {
	if agent == nil {
		return nil
	}
	var opSelector map[string]string
	var opTolerations []api.K8sToleration
	var opRuntimeClass string
	if policy != nil {
		opSelector, opTolerations, opRuntimeClass = policy.NodeSelector, policy.Tolerations, policy.RuntimeClassName
	}
	hit := map[PlacementPreflightCategory]bool{}
	for k, v := range agent.NodeSelector {
		if ValidateNodeSelector(map[string]string{k: v}) != nil {
			hit[PreflightAgentInvalidPlacementField] = true
		}
		if ov, ok := opSelector[k]; ok && ov != v {
			if k == PoolNodeSelectorKey {
				hit[PreflightAgentPoolSelectorConflict] = true
			} else {
				hit[PreflightAgentSelectorConflictsOperator] = true
			}
		}
	}
	allowed := tolerationSet(opTolerations)
	for _, t := range agent.Tolerations {
		if ValidateTolerations([]api.K8sToleration{t}) != nil {
			hit[PreflightAgentInvalidPlacementField] = true
		}
		if policy.InEffect() && isMatchAllToleration(t) {
			hit[PreflightAgentMatchAllToleration] = true
		}
		if len(opTolerations) == 0 {
			hit[PreflightAgentTolerationsWithoutOperatorPolicy] = true
		} else if _, ok := allowed[identityOfToleration(t)]; !ok {
			hit[PreflightAgentTolerationNotInPolicy] = true
		}
	}
	if ValidateRuntimeClassName(agent.RuntimeClassName) != nil {
		hit[PreflightAgentInvalidPlacementField] = true
	}
	if agent.RuntimeClassName != "" && agent.RuntimeClassName != opRuntimeClass {
		if opRuntimeClass == "" {
			hit[PreflightAgentRuntimeClassWithoutOperatorPolicy] = true
		} else {
			hit[PreflightAgentRuntimeClassDiffers] = true
		}
	}
	var out []PlacementPreflightCategory
	for _, c := range PlacementPreflightCategories {
		if hit[c] {
			out = append(out, c)
		}
	}
	return out
}

// PlacementPreflightReport is the aggregate result of a scan: how many
// agents were categorized, how many could not be (an unreadable record,
// project or configuration, or an unresolved policy; never counted as
// matching nothing), whether the agent records themselves could not be
// listed, and the count per category. Category counts are per agent and
// overlap, so their sum is not a number of agents.
//
// NotCovered counts agents the scan knows of but does not reach (for
// example agents of another Runtime Broker), and NotEnumerated names, by a
// fixed phrase, agent sources the scan cannot list at all (for example
// linked projects). Both are incomplete coverage, never zero findings.
type PlacementPreflightReport struct {
	Scanned           int
	Incomplete        int
	RecordsUnreadable bool
	NotCovered        int
	NotEnumerated     []string
	Counts            map[PlacementPreflightCategory]int
}

// Add categorizes one agent against its resolved policy and counts it.
func (r *PlacementPreflightReport) Add(agent *api.KubernetesConfig, policy *api.ResolvedPlacement) {
	r.Scanned++
	for _, c := range CategorizeAgentPlacement(agent, policy) {
		if r.Counts == nil {
			r.Counts = map[PlacementPreflightCategory]int{}
		}
		r.Counts[c]++
	}
}

// AddIncomplete counts one agent that could not be categorized.
func (r *PlacementPreflightReport) AddIncomplete() {
	r.Incomplete++
}

// NeedsAttention reports whether any agent falls into a category, or the
// scan's coverage is incomplete.
func (r PlacementPreflightReport) NeedsAttention() bool {
	if r.Incomplete > 0 || r.RecordsUnreadable || r.NotCovered > 0 || len(r.NotEnumerated) > 0 {
		return true
	}
	for _, n := range r.Counts {
		if n > 0 {
			return true
		}
	}
	return false
}

// Summary is the report as one line of counts, for example
// "scanned 12, incomplete 1; agent_tolerations_without_operator_policy 2;
// agent_runtime_class_without_operator_policy 0; agent_toleration_not_in_policy 1".
// The two "without operator policy" categories are always shown, the
// others only when counted. It carries no names or values.
func (r PlacementPreflightReport) Summary() string {
	var b strings.Builder
	if r.RecordsUnreadable {
		b.WriteString("agent records could not be read (coverage incomplete); ")
	}
	fmt.Fprintf(&b, "scanned %d, incomplete %d", r.Scanned, r.Incomplete)
	if r.NotCovered > 0 {
		fmt.Fprintf(&b, ", not covered %d", r.NotCovered)
	}
	if len(r.NotEnumerated) > 0 {
		fmt.Fprintf(&b, ", not enumerated: %s", strings.Join(r.NotEnumerated, ", "))
	}
	for _, c := range PlacementPreflightCategories {
		if n := r.Counts[c]; n > 0 || alwaysSummarized[c] {
			fmt.Fprintf(&b, "; %s %d", c, n)
		}
	}
	return b.String()
}
