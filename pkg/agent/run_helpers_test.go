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
	"os/exec"
	"path/filepath"
	"testing"
)

// setupTestGitRepoWithBranch creates a real git repository at dir with an
// initial commit and a branch named branchName pointing at that commit, for
// tests that need util.BranchExistsIn / util.FindWorktreeByBranchIn to see a
// real repository, not a mocked one.
func setupTestGitRepoWithBranch(t *testing.T, dir, branchName string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	run("init")
	run("config", "user.email", "you@example.com")
	run("config", "user.name", "Your Name")
	run("commit", "--allow-empty", "-m", "root commit")
	run("branch", branchName)
}

// startRepoRootProjectScaffold creates a minimal project under tmpDir that
// Start can resolve harness/template/settings from (docker profile, a
// "test-harness" harness-config, and a "default" template), changes the
// working directory and HOME to tmpDir for the duration of the test, and
// returns the project's .scion directory (the ProjectPath Start expects).
// Mirrors pkg/runtimebroker's setupRepoRootProjectScaffold. A caller that
// needs non-default settings.yaml content (e.g. an NFS-backed
// workspace_storage config) can overwrite
// filepath.Join(projectScionDir, "settings.yaml") after calling this.
func startRepoRootProjectScaffold(t *testing.T, tmpDir string) string {
	t.Helper()

	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}
	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatalf("failed to create harness-config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644); err != nil {
		t.Fatalf("failed to write harness config: %v", err)
	}
	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatalf("failed to create template dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatalf("failed to write template: %v", err)
	}
	return projectScionDir
}
