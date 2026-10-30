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

package cmd

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The flat placement pre-upgrade check reports each instance's counts (never
// names or values), warns with the migration remediation when any instance
// has an agent in a category or incomplete coverage, and passes otherwise.
func TestCheckDoctorFlatPlacementPreflight(t *testing.T) {
	instances := []config.V1RuntimeBrokerInstanceConfig{{Key: "agents"}, {Key: "batch"}}
	reports := map[string]config.PlacementPreflightReport{
		"agents": {Scanned: 3, Counts: map[config.PlacementPreflightCategory]int{config.PreflightAgentTolerationsWithoutOperatorPolicy: 1}},
		"batch":  {Scanned: 2, Incomplete: 1},
	}
	scan := func(inst config.V1RuntimeBrokerInstanceConfig) config.PlacementPreflightReport {
		return reports[inst.Key]
	}
	res := checkDoctorFlatPlacementPreflight(instances, scan)
	if res.Name != "flat-placement-preflight" || res.Status != "warn" || res.Remediation != flatPlacementPreflightRemedy {
		t.Fatalf("got %+v", res)
	}
	for _, want := range []string{"this release checks flat instances",
		"instance agents: scanned 3, incomplete 0; agent_tolerations_without_operator_policy 1; agent_runtime_class_without_operator_policy 0",
		"instance batch: scanned 2, incomplete 1"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message %q lacks %q", res.Message, want)
		}
	}

	reports = map[string]config.PlacementPreflightReport{"agents": {Scanned: 3}, "batch": {}}
	if res := checkDoctorFlatPlacementPreflight(instances, scan); res.Status != "pass" || res.Remediation != "" {
		t.Fatalf("clean scan: got %+v", res)
	}
}
