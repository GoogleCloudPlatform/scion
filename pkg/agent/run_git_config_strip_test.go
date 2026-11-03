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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// startWithRepoKey runs Start with a mock runtime named runtimeName for an agent whose existing worktree belongs to
// a repository whose .git/config sets core.pager (a never-carry key that host-side git allows, pinned). It returns
// the Start result, the RunConfig the runtime got, whether Run was called, and the Start error.
func startWithRepoKey(t *testing.T, runtimeName string) (*api.AgentInfo, runtime.RunConfig, bool, error) {
	t.Helper()
	// Real git worktree detection in ProvisionAgent is off when SCION_HOST_UID is set (inside an agent container).
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	must(os.MkdirAll(hcDir, 0755))
	must(os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644))
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	must(os.MkdirAll(tplDir, 0755))
	must(os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644))
	must(os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0644))

	agentName := "strip-agent"
	targetBranch := api.Slugify(agentName)
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, targetBranch)
	projectScionDir := filepath.Join(projectDir, ".scion")
	must(os.MkdirAll(projectScionDir, 0755))
	must(os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644))
	wt := filepath.Join(tmpDir, "project-worktree-sibling")
	if out, err := exec.Command("git", "-C", projectDir, "worktree", "add", wt, targetBranch).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	f, err := os.OpenFile(filepath.Join(projectDir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[core]\n\tpager = less-pagermarker-s9\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var cfg runtime.RunConfig
	ran := false
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			cfg, ran = config, true
			return "mock-id", nil
		},
	}
	info, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{Name: agentName, ProjectPath: projectScionDir, NoAuth: true})
	return info, cfg, ran, err
}

// On docker the never-carry key is left out of the private copy the runtime binds, and the start notice names it
// (StartNotice and RelayedWarnings). On the Apple container runtime, which does not bind the copy, the start is
// refused before the runtime is asked to run anything.
func TestStart_NeverCarryKeyStrippedOrRefusedByRuntime(t *testing.T) {
	t.Run("docker leaves it out and names it", func(t *testing.T) {
		info, cfg, ran, err := startWithRepoKey(t, "docker")
		if err != nil || !ran {
			t.Fatalf("Start = %v (ran %v), want the agent started", err, ran)
		}
		copyBytes, rerr := os.ReadFile(cfg.PrivateGitConfigSource)
		if rerr != nil {
			t.Fatalf("private copy: %v", rerr)
		}
		if strings.Contains(strings.ToLower(string(copyBytes)), "pager") || strings.Contains(string(copyBytes), "pagermarker-s9") {
			t.Errorf("the private copy still carries core.pager:\n%s", copyBytes)
		}
		want := util.NeverCarriedNotice([]string{"core.pager"})
		if info == nil || info.StartNotice != want {
			t.Errorf("StartNotice = %q, want %q", infoNotice(info), want)
		}
		if info == nil || !slices.Contains(info.RelayedWarnings, want) {
			t.Errorf("RelayedWarnings do not carry the notice: %v", info)
		}
	})
	t.Run("Apple container runtime refuses before Run", func(t *testing.T) {
		_, _, ran, err := startWithRepoKey(t, "container")
		if err == nil || !strings.Contains(err.Error(), "core.pager") || ran {
			t.Fatalf("Start = %v (ran %v), want a refusal naming core.pager and no Run", err, ran)
		}
		if strings.Contains(err.Error(), "pagermarker-s9") {
			t.Errorf("the refusal carries the value: %v", err)
		}
	})
}

func infoNotice(info *api.AgentInfo) string {
	if info == nil {
		return "<nil info>"
	}
	return info.StartNotice
}
