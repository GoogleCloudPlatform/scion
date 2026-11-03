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
	"os"
	"path/filepath"
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
	res := checkDoctorLegacyPlacementPreflight(settings, agents, false)
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
	if res := checkDoctorLegacyPlacementPreflight(clean, config.PlacementPreflightReport{Scanned: 3}, false); res.Status != "pass" || res.Remediation != "" {
		t.Fatalf("clean scan: got %+v", res)
	}
	// Linked projects the host cannot list are incomplete coverage, never
	// a clean result.
	partial := config.PlacementPreflightReport{Scanned: 3, NotEnumerated: []string{legacyPlacementNotEnumerated}}
	res = checkDoctorLegacyPlacementPreflight(clean, partial, false)
	if res.Status != "warn" || !strings.Contains(res.Message, "not enumerated: linked projects") {
		t.Fatalf("unlisted linked projects: got %+v", res)
	}
}

// legacyDoctorHome makes a temp HOME with the global settings file content
// (none when empty) and an isolated working directory, and returns the
// global directory.
func legacyDoctorHome(t *testing.T, settings string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if settings != "" {
		if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return globalDir
}

// A legacy host with no configured Kubernetes runtime entry still gets the
// line: its Kubernetes agents run on the built-in entry, whose operator
// policy is empty, so an agent toleration there is counted.
func TestCheckDoctorLegacyPlacementPreflightBuiltinEntry(t *testing.T) {
	globalDir := legacyDoctorHome(t, "schema_version: \"1\"\n")
	projectDir, err := config.GetResolvedProjectDir("global")
	if err != nil {
		t.Fatal(err)
	}
	home := config.GetAgentHomePath(projectDir, "tol-agent")
	agentDir := config.GetAgentDir(projectDir, "tol-agent", false)
	for _, d := range []string{home, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(`{"runtime":"kubernetes"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"kubernetes":{"tolerations":[{"key":"dedicated","operator":"Exists"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, ok := checkDoctorLegacyPlacementPreflightHere()
	if !ok || res.Name != "legacy-placement-preflight" || res.Status != "warn" {
		t.Fatalf("got %v %+v (global dir %s)", ok, res, globalDir)
	}
	if !strings.Contains(res.Message, "agents: scanned 1, incomplete 0") || !strings.Contains(res.Message, "agent_tolerations_without_operator_policy 1") {
		t.Fatalf("message %q", res.Message)
	}
}

// Settings that cannot be read give a warning line with incomplete
// coverage, never a missing line; a host with flat instances gets none
// (the flat check covers it).
func TestCheckDoctorLegacyPlacementPreflightUnreadableSettings(t *testing.T) {
	legacyDoctorHome(t, "schema_version: \"1\"\nserver:\n  broker:\n    instances: not-a-list\n")
	res, ok := checkDoctorLegacyPlacementPreflightHere()
	if !ok || res.Status != "warn" || !strings.Contains(res.Message, "coverage incomplete") {
		t.Fatalf("unreadable instances: got %v %+v", ok, res)
	}

	legacyDoctorHome(t, "schema_version: [\n")
	res, ok = checkDoctorLegacyPlacementPreflightHere()
	if !ok || res.Status != "warn" || !strings.Contains(res.Message, "coverage incomplete") {
		t.Fatalf("unparseable settings: got %v %+v", ok, res)
	}

	legacyDoctorHome(t, "schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n    instances:\n      - key: local-docker\n        name: example-docker\n        runtime_target:\n          type: docker\n          display_name: Local Docker\n")
	if res, ok := checkDoctorLegacyPlacementPreflightHere(); ok {
		t.Fatalf("a flat host got a legacy line: %+v", res)
	}
}

// The line's status follows Kubernetes evidence; its coverage phrase is
// always there. With no Kubernetes use, what the scan cannot list leaves
// the line passing (naming it); with Kubernetes use (a configured entry, or
// a Kubernetes agent found), it warns.
func TestCheckDoctorLegacyPlacementPreflightStatusFollowsKubernetesUse(t *testing.T) {
	clean := config.ScanSettingsPlacement(nil, nil)
	unlisted := config.PlacementPreflightReport{NotEnumerated: []string{legacyPlacementNotEnumerated}}
	res := checkDoctorLegacyPlacementPreflight(clean, unlisted, false)
	if res.Status != "pass" || res.Remediation != "" || !strings.Contains(res.Message, "not enumerated: linked projects") {
		t.Fatalf("no Kubernetes use: %+v", res)
	}
	if res := checkDoctorLegacyPlacementPreflight(clean, unlisted, true); res.Status != "warn" {
		t.Fatalf("configured Kubernetes entry: %+v", res)
	}
	found := unlisted
	found.Scanned = 1
	if res := checkDoctorLegacyPlacementPreflight(clean, found, false); res.Status != "warn" {
		t.Fatalf("Kubernetes agent found: %+v", res)
	}
	incomplete := unlisted
	incomplete.Incomplete = 1
	if res := checkDoctorLegacyPlacementPreflight(clean, incomplete, false); res.Status != "warn" {
		t.Fatalf("incomplete records: %+v", res)
	}
}

// A Docker-only legacy host (no Kubernetes entry configured, only a Docker
// agent) passes, and the line still names the sources it cannot list.
func TestCheckDoctorLegacyPlacementPreflightDockerOnlyHost(t *testing.T) {
	legacyDoctorHome(t, "schema_version: \"1\"\n")
	writeLegacyDoctorAgent(t, "dock", `{"runtime":"docker"}`, `{}`)
	res, ok := checkDoctorLegacyPlacementPreflightHere()
	if !ok || res.Status != "pass" || !strings.Contains(res.Message, "not enumerated: linked projects") {
		t.Fatalf("got %v %+v", ok, res)
	}
}

// A host whose only Kubernetes evidence is a configured Kubernetes runtime
// entry warns on the sources it cannot list.
func TestCheckDoctorLegacyPlacementPreflightConfiguredEntryWarns(t *testing.T) {
	legacyDoctorHome(t, "schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n    namespace: agents\n")
	res, ok := checkDoctorLegacyPlacementPreflightHere()
	if !ok || res.Status != "warn" || !strings.Contains(res.Message, "scanned 0, incomplete 0") || !strings.Contains(res.Message, "not enumerated: linked projects") {
		t.Fatalf("got %v %+v", ok, res)
	}
}

// writeLegacyDoctorAgent writes agent name's agent-info.json and
// scion-agent.json in the global project of the current HOME.
func writeLegacyDoctorAgent(t *testing.T, name, info, cfg string) {
	t.Helper()
	projectDir, err := config.GetResolvedProjectDir("global")
	if err != nil {
		t.Fatal(err)
	}
	home := config.GetAgentHomePath(projectDir, name)
	agentDir := config.GetAgentDir(projectDir, name, false)
	for _, d := range []string{home, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A host whose only Kubernetes evidence is an operator profile on a
// Kubernetes runtime entry (here the built-in one: no runtimes configured,
// no agents) warns on the sources it cannot list.
func TestCheckDoctorLegacyPlacementPreflightOperatorProfileWarns(t *testing.T) {
	legacyDoctorHome(t, "schema_version: \"1\"\nprofiles:\n  gpu:\n    runtime: kubernetes\n")
	res, ok := checkDoctorLegacyPlacementPreflightHere()
	if !ok || res.Status != "warn" || !strings.Contains(res.Message, "scanned 0, incomplete 0") || !strings.Contains(res.Message, "not enumerated: linked projects") {
		t.Fatalf("got %v %+v", ok, res)
	}
}
