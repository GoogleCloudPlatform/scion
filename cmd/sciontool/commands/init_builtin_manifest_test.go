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

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunInit_BuiltinManifestAbortsStartup: a staged harness manifest naming
// the unsupported "builtin" provisioner aborts sciontool init (return 1)
// before the child starts, and the failure is reported in agent-info.json.
func TestRunInit_BuiltinManifestAbortsStartup(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	bundleDir := filepath.Join(agentHome, ".scion", "harness")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.json"),
		[]byte(`{"harness_config": {"harness": "claude", "provisioner": {"type": "builtin"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "child-ran")

	got := RunInit([]string{"sh", "-c", "touch " + marker}, InitRunOptions{DisableTermSignalForwarding: true})
	if got != 1 {
		t.Fatalf("RunInit() = %d, want 1", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the child must not start when the harness manifest cannot be used")
	}
	info, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("expected an init-failure report in agent-info.json: %v", err)
	}
	if !strings.Contains(string(info), "error") || !strings.Contains(string(info), "builtin") {
		t.Errorf("agent-info.json does not report the manifest failure: %s", info)
	}
}
