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
// or restart because the operator placement sets nothing for them. The scan
// reports aggregate counts only, never configuration values.

// PlacementPreflightCategory is a stable category name of the scan.
type PlacementPreflightCategory string

const (
	// PreflightAgentTolerationsWithoutOperatorPolicy: the agent sets one or
	// more tolerations, but the operator placement sets none.
	PreflightAgentTolerationsWithoutOperatorPolicy PlacementPreflightCategory = "agent_tolerations_without_operator_policy"
	// PreflightAgentRuntimeClassWithoutOperatorPolicy: the agent sets a
	// runtime class, but the operator placement sets none.
	PreflightAgentRuntimeClassWithoutOperatorPolicy PlacementPreflightCategory = "agent_runtime_class_without_operator_policy"
)

// PlacementPreflightCategories lists every category, in report order.
var PlacementPreflightCategories = []PlacementPreflightCategory{
	PreflightAgentTolerationsWithoutOperatorPolicy,
	PreflightAgentRuntimeClassWithoutOperatorPolicy,
}

// CategorizeAgentPlacement returns the categories the agent's own
// kubernetes block falls into against the operator placement policy, which
// the caller has resolved (nil means the operator sets no placement). An
// agent that meets both conditions is in both categories, once each.
func CategorizeAgentPlacement(agent *api.KubernetesConfig, policy *api.ResolvedPlacement) []PlacementPreflightCategory {
	if agent == nil {
		return nil
	}
	var opTolerations int
	var opRuntimeClass string
	if policy != nil {
		opTolerations, opRuntimeClass = len(policy.Tolerations), policy.RuntimeClassName
	}
	var out []PlacementPreflightCategory
	if len(agent.Tolerations) > 0 && opTolerations == 0 {
		out = append(out, PreflightAgentTolerationsWithoutOperatorPolicy)
	}
	if agent.RuntimeClassName != "" && opRuntimeClass == "" {
		out = append(out, PreflightAgentRuntimeClassWithoutOperatorPolicy)
	}
	return out
}

// PlacementPreflightReport is the aggregate result of a scan: how many
// agents were categorized, how many could not be (an unreadable record,
// project or configuration, or an unresolved policy; never counted as
// matching nothing), whether the agent records themselves could not be
// listed, and the count per category. Category counts are per agent and
// overlap, so their sum is not a number of agents.
type PlacementPreflightReport struct {
	Scanned           int
	Incomplete        int
	RecordsUnreadable bool
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
	if r.Incomplete > 0 || r.RecordsUnreadable {
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
// agent_runtime_class_without_operator_policy 0". It carries no names or
// values.
func (r PlacementPreflightReport) Summary() string {
	var b strings.Builder
	if r.RecordsUnreadable {
		b.WriteString("agent records could not be read (coverage incomplete); ")
	}
	fmt.Fprintf(&b, "scanned %d, incomplete %d", r.Scanned, r.Incomplete)
	for _, c := range PlacementPreflightCategories {
		fmt.Fprintf(&b, "; %s %d", c, r.Counts[c])
	}
	return b.String()
}
