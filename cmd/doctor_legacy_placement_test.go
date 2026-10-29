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

// The legacy placement pre-upgrade line reports settings and agent counts
// (never names or values) with its coverage; it warns with the migration
// remediation when either scan has a finding or incomplete coverage,
// including the linked projects it cannot list, and passes otherwise.
func TestCheckDoctorLegacyPlacementPreflight(t *testing.T) {
	settings := config.ScanSettingsPlacement(
		map[string]config.V1RuntimeConfig{"k": {Type: "kubernetes", NodeSelector: map[string]string{config.PoolNodeSelectorKey: "agents"}}},
		map[string]config.V1ProfileConfig{"p": {Runtime: "k", NodeSelector: map[string]string{config.PoolNodeSelectorKey: "batch"}}})
	agents := config.PlacementPreflightReport{Scanned: 2, Incomplete: 1,
		Counts: map[config.PlacementPreflightCategory]int{config.PreflightAgentTolerationsWithoutOperatorPolicy: 1}}
	res := checkDoctorLegacyPlacementPreflight(settings, agents)
	if res.Name != "legacy-placement-preflight" || res.Status != "warn" || res.Remediation != legacyPlacementPreflightRemedy {
		t.Fatalf("got %+v", res)
	}
	for _, want := range []string{"settings_pool_selector_conflict 1", "repair sections: profiles",
		"agents: scanned 2, incomplete 1; agent_tolerations_without_operator_policy 1"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message %q lacks %q", res.Message, want)
		}
	}
	if strings.Contains(res.Message, "batch") {
		t.Fatalf("message carries a value: %q", res.Message)
	}

	clean := config.ScanSettingsPlacement(nil, nil)
	if res := checkDoctorLegacyPlacementPreflight(clean, config.PlacementPreflightReport{Scanned: 3}); res.Status != "pass" || res.Remediation != "" {
		t.Fatalf("clean scan: got %+v", res)
	}
	// Linked projects the host cannot list are incomplete coverage, never
	// a clean result.
	partial := config.PlacementPreflightReport{Scanned: 3, NotEnumerated: []string{legacyPlacementNotEnumerated}}
	res = checkDoctorLegacyPlacementPreflight(clean, partial)
	if res.Status != "warn" || !strings.Contains(res.Message, "not enumerated: linked projects") {
		t.Fatalf("unlisted linked projects: got %+v", res)
	}
}
