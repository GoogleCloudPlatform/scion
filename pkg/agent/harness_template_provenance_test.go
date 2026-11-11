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

// TestStart_AgentInfoTemplateDoesNotChangeHarnessResolve pins that, for an
// agent with broker-side image provenance, the template whose bundled
// harness-config harness.Resolve uses comes from the provenance record, not
// from agent-info.json. Templates alpha and beta both
// bundle a harness-config named test-harness, with different contents;
// rewriting agent-info.json's template to beta must not change the resolved
// harness-config (observed through its revision).
func TestStart_AgentInfoTemplateDoesNotChangeHarnessResolve(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	bundled := map[string]string{}
	for _, tpl := range []string{"alpha", "beta"} {
		tplDir := filepath.Join(globalScionDir, "templates", tpl)
		hcDir := filepath.Join(tplDir, "harness-configs", "test-harness")
		if err := os.MkdirAll(hcDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: "+tpl+"-user\nimage: "+tpl+"-image:v1\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
			t.Fatal(err)
		}
		bundled[tpl] = config.ComputeHarnessConfigRevision(hcDir)
	}
	if bundled["alpha"] == bundled["beta"] {
		t.Fatal("precondition: the two bundled harness-configs must differ")
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0644); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	_, info := startCapturingRun(t, api.StartOptions{Name: "test-agent", Template: "alpha", ProjectPath: projectScionDir})
	if info == nil || info.HarnessConfigRevision != bundled["alpha"] {
		t.Fatalf("first start: harness-config revision = %v, want alpha's bundled %q", info, bundled["alpha"])
	}

	rewriteAgentInfo(t, projectScionDir, func(raw map[string]any) { raw["template"] = "beta" })

	// A restart that names the template by (non-absolute) name, as a local
	// start can: the name is then resolved through the recorded template.
	_, info = startCapturingRun(t, api.StartOptions{Name: "test-agent", Template: "alpha", ProjectPath: projectScionDir})
	if info == nil || info.HarnessConfigRevision != bundled["alpha"] {
		got := ""
		if info != nil {
			got = info.HarnessConfigRevision
		}
		t.Fatalf("restart after rewriting agent-info.json template: harness-config revision = %q, want alpha's %q (beta's is %q)", got, bundled["alpha"], bundled["beta"])
	}
}
