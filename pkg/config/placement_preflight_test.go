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
	r.AddIncomplete() // the same agent with an unresolved policy
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
	r.AddIncomplete()
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
