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
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// profileResourcesSettings gives profile "other" a CPU request that
// "staging" (the active profile) does not have, so the profile a start
// resolves settings with shows in its RunConfig.
const profileResourcesSettings = `schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
  other:
    runtime: docker
    resources:
      requests:
        cpu: "7"
harness_configs:
  test-harness:
    harness: generic
`

func requestedCPU(cfg runtime.RunConfig) string {
	if cfg.Resources == nil {
		return ""
	}
	return cfg.Resources.Requests.CPU
}

// TestStart_ProfileNotTakenFromAgentInfo: a start that names no profile
// resolves settings with the profile recorded in broker-side image
// provenance, or with none for an agent provisioned before that record
// existed. The profile in agent-info.json (in the agent home) is never
// used for it, nor reported as the start's profile.
func TestStart_ProfileNotTakenFromAgentInfo(t *testing.T) {
	t.Run("named profile applies (control)", func(t *testing.T) {
		projectScionDir := imagePrecedenceFixture(t, "", profileResourcesSettings)
		cfg, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "other"})
		if requestedCPU(cfg) != "7" {
			t.Fatalf("start with profile other: cpu request = %q, want 7", requestedCPU(cfg))
		}
	})
	for _, tc := range []struct {
		name   string
		legacy bool
	}{
		{name: "provisioned agent", legacy: false},
		{name: "agent without image provenance", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir := imagePrecedenceFixture(t, "", profileResourcesSettings)
			first, _ := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir, Profile: "staging"})
			if requestedCPU(first) == "7" {
				t.Fatalf("first start: cpu request = 7, want the staging profile's (none)")
			}
			rewriteAgentInfo(t, projectScionDir, func(raw map[string]any) { raw["profile"] = "other" })
			if tc.legacy {
				if err := os.Remove(filepath.Join(projectScionDir, "agents", "test-agent", imageProvenanceFile)); err != nil {
					t.Fatal(err)
				}
			}

			restart, info := startCapturingRun(t, api.StartOptions{Name: "test-agent", ProjectPath: projectScionDir})
			if requestedCPU(restart) == "7" {
				t.Errorf("restart after rewriting agent-info.json profile: cpu request = 7, want the agent-info.json profile ignored")
			}
			if info != nil && info.Profile == "other" {
				t.Errorf("restart reported profile %q from agent-info.json", info.Profile)
			}
		})
	}
}
