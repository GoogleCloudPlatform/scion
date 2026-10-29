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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// writePlacementScanAgent writes agent slug's agent-info.json (info, unless
// empty) and scion-agent.json (cfg, unless empty) in the project at
// projectPath.
func writePlacementScanAgent(t *testing.T, projectPath, slug, info, cfg string) {
	t.Helper()
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	home := config.GetAgentHomePath(projectDir, slug)
	agentDir := config.GetAgentDir(projectDir, slug, false)
	for _, d := range []string{home, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if info != "" {
		if err := os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// placementScanPolicy resolves profile "pool" to a pool placement with
// tolerations, "" to the empty operator policy, and every other profile as
// unresolved.
func placementScanPolicy(profile string) (*api.ResolvedPlacement, bool) {
	switch profile {
	case "pool":
		return &api.ResolvedPlacement{NodeSelector: map[string]string{config.PoolNodeSelectorKey: "agents"},
			Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}, OperatorOwned: true}, true
	case "":
		return &api.ResolvedPlacement{OperatorOwned: true}, true
	}
	return nil, false
}

// The host-local scan reads every agent directory of each project:
// Kubernetes agents are categorized against their profile's policy, other
// runtimes are skipped, and a missing or unreadable record, an unknown
// runtime or an unresolved profile is incomplete. Nothing is written.
func TestScanLocalLegacyPlacement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := filepath.Join(t.TempDir(), "proj", config.DotScion)
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	writePlacementScanAgent(t, project, "tol", `{"runtime":"kubernetes"}`, `{"kubernetes":{"tolerations":[{"key":"x","operator":"Exists"}]}}`)
	writePlacementScanAgent(t, project, "pool-ok", `{"runtime":"kubernetes","profile":"pool"}`, `{"kubernetes":{"tolerations":[{"key":"dedicated","operator":"Exists"}]}}`)
	writePlacementScanAgent(t, project, "pool-bad", `{"runtime":"kubernetes","profile":"pool"}`, `{"kubernetes":{"nodeSelector":{"scion.dev/pool":"batch"}}}`)
	writePlacementScanAgent(t, project, "docker", `{"runtime":"docker"}`, `{"kubernetes":{"runtimeClassName":"kata"}}`)
	writePlacementScanAgent(t, project, "unresolved", `{"runtime":"kubernetes","profile":"gone"}`, `{}`)
	writePlacementScanAgent(t, project, "no-info", "", `{}`)
	writePlacementScanAgent(t, project, "bad-info", `{`, `{}`)
	writePlacementScanAgent(t, project, "no-runtime", `{"profile":"pool"}`, `{}`)
	writePlacementScanAgent(t, project, "no-config", `{"runtime":"kubernetes"}`, "")
	writePlacementScanAgent(t, project, "bad-config", `{"runtime":"kubernetes"}`, `{"kubernetes":`)

	r := config.PlacementPreflightReport{NotEnumerated: []string{"linked projects"}}
	ScanLocalLegacyPlacement(&r, []string{project, project}, placementScanPolicy)
	if r.Scanned != 3 || r.Incomplete != 6 || r.RecordsUnreadable {
		t.Fatalf("report = %+v", r)
	}
	if r.Counts[config.PreflightAgentTolerationsWithoutOperatorPolicy] != 1 || r.Counts[config.PreflightAgentPoolSelectorConflict] != 1 || len(r.Counts) != 2 {
		t.Fatalf("counts = %v", r.Counts)
	}
	if !r.NeedsAttention() {
		t.Fatal("an unenumerated source does not need attention")
	}

	// A project with no agent directory adds nothing.
	empty := filepath.Join(t.TempDir(), "empty", config.DotScion)
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	var none config.PlacementPreflightReport
	ScanLocalLegacyPlacement(&none, []string{empty}, placementScanPolicy)
	if none.Scanned != 0 || none.Incomplete != 0 || none.RecordsUnreadable {
		t.Fatalf("empty project: %+v", none)
	}
}
