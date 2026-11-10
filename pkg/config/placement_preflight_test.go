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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// An agent with tolerations is counted in
// agent_tolerations_without_operator_policy when the resolved operator
// placement sets no tolerations (none, or a placement without them); not
// when the operator sets tolerations; and an agent whose policy could not be
// resolved is counted as incomplete, never in the category.
func TestPlacementPreflightAgentTolerationsWithoutOperatorPolicy(t *testing.T) {
	agent := &api.KubernetesConfig{Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}}
	var r PlacementPreflightReport
	r.Add(agent, nil)
	r.Add(agent, &api.ResolvedPlacement{NodeSelector: map[string]string{"scion.dev/pool": "agents"}, OperatorOwned: true})
	r.Add(agent, &api.ResolvedPlacement{Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}, OperatorOwned: true})
	addUnresolvedLegacyAgent(t, &r, agent) // the same agent with an unresolved policy
	if r.Counts[PreflightAgentTolerationsWithoutOperatorPolicy] != 2 || r.Counts[PreflightAgentRuntimeClassWithoutOperatorPolicy] != 0 {
		t.Fatalf("counts = %v", r.Counts)
	}
	if r.Scanned != 3 || r.Incomplete != 1 || !r.NeedsAttention() {
		t.Fatalf("report = %+v", r)
	}
	if got := CategorizeAgentPlacement(&api.KubernetesConfig{}, nil); got != nil {
		t.Fatalf("an agent without tolerations is categorized: %v", got)
	}
}

// An agent with a runtime class is counted in
// agent_runtime_class_without_operator_policy when the resolved operator
// placement sets no runtime class; not when it sets one; an unresolved
// policy is incomplete. An agent meeting both conditions counts once in each
// category, and the summary carries counts only.
func TestPlacementPreflightAgentRuntimeClassWithoutOperatorPolicy(t *testing.T) {
	agent := &api.KubernetesConfig{RuntimeClassName: "secret-class-name"}
	var r PlacementPreflightReport
	r.Add(agent, nil)
	r.Add(agent, &api.ResolvedPlacement{RuntimeClassName: "gvisor", OperatorOwned: true})
	addUnresolvedLegacyAgent(t, &r, agent)
	if r.Counts[PreflightAgentRuntimeClassWithoutOperatorPolicy] != 1 || r.Counts[PreflightAgentTolerationsWithoutOperatorPolicy] != 0 || r.Incomplete != 1 {
		t.Fatalf("report = %+v", r)
	}

	both := &api.KubernetesConfig{RuntimeClassName: "secret-class-name",
		Tolerations: []api.K8sToleration{{Key: "secret-key", Value: "secret-value", Effect: "NoSchedule"}, {Key: "b", Operator: "Exists"}}}
	var c PlacementPreflightReport
	c.Add(both, &api.ResolvedPlacement{OperatorOwned: true})
	if c.Scanned != 1 || c.Counts[PreflightAgentTolerationsWithoutOperatorPolicy] != 1 || c.Counts[PreflightAgentRuntimeClassWithoutOperatorPolicy] != 1 {
		t.Fatalf("combined = %+v", c)
	}
	sum := c.Summary()
	if sum != "scanned 1, incomplete 0; agent_tolerations_without_operator_policy 1; agent_runtime_class_without_operator_policy 1" {
		t.Fatalf("summary = %q", sum)
	}
	for _, v := range []string{"secret"} {
		if strings.Contains(sum, v) {
			t.Fatalf("summary carries a value: %q", sum)
		}
	}
	var clean PlacementPreflightReport
	clean.Add(&api.KubernetesConfig{NodeSelector: map[string]string{"disk": "ssd"}}, nil)
	if clean.NeedsAttention() {
		t.Fatalf("a clean scan needs attention: %+v", clean)
	}
	unreadable := PlacementPreflightReport{RecordsUnreadable: true}
	if !unreadable.NeedsAttention() || !strings.Contains(unreadable.Summary(), "coverage incomplete") {
		t.Fatalf("unreadable records: %q", unreadable.Summary())
	}
}

// Each agent category is reached by exactly the condition it names, and an
// agent meeting several conditions is counted once in each.
func TestPlacementPreflightEveryCategory(t *testing.T) {
	policy := operatorPlacement()
	cases := map[PlacementPreflightCategory]*api.KubernetesConfig{
		PreflightAgentInvalidPlacementField:     {NodeSelector: map[string]string{"bad key!": "x"}},
		PreflightAgentMatchAllToleration:        {Tolerations: []api.K8sToleration{poolToleration(), {Operator: "Exists"}}},
		PreflightAgentPoolSelectorConflict:      {NodeSelector: map[string]string{PoolNodeSelectorKey: "other"}},
		PreflightAgentSelectorConflictsOperator: nil, // set below: needs an operator key other than the pool
		PreflightAgentTolerationNotInPolicy:     {Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}},
		PreflightAgentRuntimeClassDiffers:       {RuntimeClassName: "kata"},
	}
	for want, agent := range cases {
		p := policy
		if want == PreflightAgentSelectorConflictsOperator {
			p = &api.ResolvedPlacement{NodeSelector: map[string]string{"disk": "ssd"}, OperatorOwned: true}
			agent = &api.KubernetesConfig{NodeSelector: map[string]string{"disk": "hdd"}}
		}
		got := CategorizeAgentPlacement(agent, p)
		if want == PreflightAgentMatchAllToleration {
			// The match-all entry is also not one of the operator's.
			if len(got) != 2 || got[0] != want || got[1] != PreflightAgentTolerationNotInPolicy {
				t.Errorf("%s: got %v", want, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %v", want, got)
		}
	}

	// Matching declarations and extra selector keys fall into none.
	ok := &api.KubernetesConfig{NodeSelector: map[string]string{PoolNodeSelectorKey: "agents", "disk": "ssd"},
		Tolerations: []api.K8sToleration{poolToleration()}, RuntimeClassName: "gvisor"}
	if got := CategorizeAgentPlacement(ok, policy); got != nil {
		t.Fatalf("a matching agent is categorized: %v", got)
	}

	// Several conditions: each category once, summed per agent.
	var r PlacementPreflightReport
	multi := &api.KubernetesConfig{NodeSelector: map[string]string{PoolNodeSelectorKey: "other"},
		Tolerations: []api.K8sToleration{{Key: "a", Operator: "Exists"}, {Key: "b", Operator: "Exists"}}, RuntimeClassName: "kata"}
	r.Add(multi, policy)
	r.Add(multi, policy)
	for _, c := range []PlacementPreflightCategory{PreflightAgentPoolSelectorConflict, PreflightAgentTolerationNotInPolicy, PreflightAgentRuntimeClassDiffers} {
		if r.Counts[c] != 2 {
			t.Errorf("%s = %d, want 2", c, r.Counts[c])
		}
	}
	want := "scanned 2, incomplete 0; agent_tolerations_without_operator_policy 0; agent_runtime_class_without_operator_policy 0; " +
		"agent_pool_selector_conflict 2; agent_toleration_not_in_policy 2; agent_runtime_class_differs 2"
	if got := r.Summary(); got != want {
		t.Fatalf("summary = %q", got)
	}
}

// The categoriser and the start-time check agree: an agent block is
// refused by CheckAgentPlacement exactly when it falls into a category.
func TestCategorizeAgentPlacementAgreesWithCheck(t *testing.T) {
	policies := []*api.ResolvedPlacement{
		nil,
		{OperatorOwned: true},
		operatorPlacement(),
		{NodeSelector: map[string]string{"disk": "ssd"}, OperatorOwned: true},
		{Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}, OperatorOwned: true},
	}
	blocks := []*api.KubernetesConfig{
		nil,
		{},
		{NodeSelector: map[string]string{"disk": "ssd"}},
		{NodeSelector: map[string]string{"disk": "hdd"}},
		{NodeSelector: map[string]string{PoolNodeSelectorKey: "agents"}},
		{NodeSelector: map[string]string{PoolNodeSelectorKey: "batch"}},
		{NodeSelector: map[string]string{"bad key!": "x"}},
		{Tolerations: []api.K8sToleration{poolToleration()}},
		{Tolerations: []api.K8sToleration{{Key: PoolNodeSelectorKey, Value: "agents", Effect: "NoSchedule"}}},
		{Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}},
		{Tolerations: []api.K8sToleration{{Operator: "Exists"}}},
		{Tolerations: []api.K8sToleration{{Key: "k", Operator: "Bogus"}}},
		{RuntimeClassName: "gvisor"},
		{RuntimeClassName: "kata"},
		{RuntimeClassName: "Not_Valid"},
		{NodeSelector: map[string]string{"disk": "hdd"}, Tolerations: []api.K8sToleration{{Key: "x", Operator: "Exists"}}, RuntimeClassName: "kata"},
	}
	for pi, p := range policies {
		for bi, b := range blocks {
			refused := CheckAgentPlacement(b, p, AgentPlacementLayerStored) != nil
			categorized := len(CategorizeAgentPlacement(b, p)) > 0
			if refused != categorized {
				t.Errorf("policy %d, block %d: refused %v, categorized %v (%v)", pi, bi, refused, categorized, CategorizeAgentPlacement(b, p))
			}
		}
	}
}

// What a scan does not reach is incomplete coverage, never a clean result:
// agents it knows of but does not reach, and agent sources it cannot list.
func TestPlacementPreflightNotCoveredIsIncomplete(t *testing.T) {
	notCovered := PlacementPreflightReport{Scanned: 1, NotCovered: 3}
	if !notCovered.NeedsAttention() || !strings.Contains(notCovered.Summary(), "not covered 3") {
		t.Fatalf("not covered: %q", notCovered.Summary())
	}
	notListed := PlacementPreflightReport{Scanned: 1, NotEnumerated: []string{"linked projects"}}
	if !notListed.NeedsAttention() || !strings.Contains(notListed.Summary(), "not enumerated: linked projects") {
		t.Fatalf("not enumerated: %q", notListed.Summary())
	}
	if (PlacementPreflightReport{Scanned: 1}).NeedsAttention() {
		t.Fatal("a complete clean scan needs attention")
	}
}

// addUnresolvedLegacyAgent sends agent through the real offline policy
// resolution with a recorded profile the settings do not define: the
// policy cannot be resolved, so the agent must count as incomplete and in
// no category.
func addUnresolvedLegacyAgent(t *testing.T, r *PlacementPreflightReport, agent *api.KubernetesConfig) {
	t.Helper()
	runtimes := map[string]V1RuntimeConfig{"k": {Type: "kubernetes"}}
	before := map[PlacementPreflightCategory]int{}
	for c, n := range r.Counts {
		before[c] = n
	}
	incomplete := r.Incomplete
	// The scan's decision: categorize against a resolved policy, else
	// count the agent as incomplete.
	if policy, ok := ResolveLegacyPolicyOffline(runtimes, nil, nil, "missing-profile"); ok {
		r.Add(agent, policy)
	} else {
		r.AddIncomplete()
	}
	for _, c := range PlacementPreflightCategories {
		if r.Counts[c] != before[c] {
			t.Fatalf("an agent with an unresolved policy was categorized in %s", c)
		}
	}
	if r.Incomplete != incomplete+1 {
		t.Fatal("an agent with an unresolved policy was not counted as incomplete")
	}
}
