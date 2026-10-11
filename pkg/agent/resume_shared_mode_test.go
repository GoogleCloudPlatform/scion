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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// sharedModeCtx is the context a start carries once the project is marked
// shared: the shared-workspace flag plus the hub-supplied project ID, which
// locates the broker-side (external) agents root.
func sharedModeCtx() context.Context {
	return api.ContextWithHubProjectID(api.ContextWithSharedWorkspace(context.Background()), "proj-4439")
}

// TestGetAgent_WorktreeAgentResumedAfterProjectSwitchedToShared covers
// ptone/scion#4439: an agent created while its project used
// worktree-per-agent is resumed after the project was switched to a shared
// workspace. The process runs outside the repository, as a runtime broker
// does. The resume must reuse the agent's existing worktree, keeping its
// uncommitted work, rather than try to create a worktree from an empty
// path. The broker-mode subcase also passes GetAgent's broker-only check
// that a non-fresh start of a shared-workspace agent has its broker-side
// state directory.
func TestGetAgent_WorktreeAgentResumedAfterProjectSwitchedToShared(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
	}{
		{"local", sharedModeCtx},
		{"broker", func() context.Context { return api.ContextWithBrokerMode(sharedModeCtx()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testResumeAfterSwitchToShared(t, tc.ctx())
		})
	}
}

func testResumeAfterSwitchToShared(t *testing.T, resumeCtx context.Context) {
	scionDir, _ := reprovisionSetup(t) // CWD is outside the repository
	agentName := "mode-switch-agent"

	if _, _, _, err := ProvisionAgent(context.Background(), agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial worktree-per-agent ProvisionAgent: %v", err)
	}
	worktree := filepath.Join(scionDir, "agents", agentName, "workspace")
	if _, err := os.Stat(filepath.Join(worktree, ".git")); err != nil {
		t.Fatalf("expected a worktree after the initial provision: %v", err)
	}
	work := filepath.Join(worktree, "uncommitted-work.txt")
	if err := os.WriteFile(work, []byte("agent work"), 0644); err != nil {
		t.Fatal(err)
	}

	_, _, _, cfg, err := GetAgent(resumeCtx, agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent after the project switched to shared: %v", err)
	}

	wantSource, _ := filepath.EvalSymlinks(worktree)
	var gotSource string
	for _, v := range cfg.Volumes {
		if v.Target == "/workspace" {
			gotSource, _ = filepath.EvalSymlinks(v.Source)
		}
	}
	if gotSource != wantSource {
		t.Errorf("/workspace source = %q, want the agent's existing worktree %q", gotSource, wantSource)
	}
	if data, err := os.ReadFile(work); err != nil || string(data) != "agent work" {
		t.Errorf("uncommitted work in the existing worktree was not kept: %q, %v", data, err)
	}
}

// TestProvisionAgent_SharedModeWithoutWorktree_ClearError checks the guard
// for a shared-workspace git project with no workspace path and no existing
// worktree for the agent's branch: provisioning returns a clear error and
// never runs git worktree creation with an empty path (which would fail
// with an opaque git error and could leave a new branch behind).
func TestProvisionAgent_SharedModeWithoutWorktree_ClearError(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "shared-no-worktree"

	_, _, _, err := ProvisionAgent(sharedModeCtx(), agentName, "default", "", "", scionDir, "", "created", "", "")
	if err == nil {
		t.Fatal("expected an error for a shared-workspace git project with no worktree to reuse")
	}
	if !strings.Contains(err.Error(), "shared workspace") || strings.Contains(err.Error(), "git common dir") {
		t.Errorf("expected the shared-workspace guard error, got: %v", err)
	}
	projectDir := filepath.Dir(scionDir)
	if out, _ := exec.Command("git", "-C", projectDir, "branch", "--list", api.Slugify(agentName)).Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("expected no branch %q to be created, got %q", api.Slugify(agentName), out)
	}
}

// TestProvisionAgent_StaleWorktreeRegistrationRecreated covers an agent
// directory removed out of band, so git still lists the agent's worktree
// (marked prunable) but its directory no longer exists. Re-provisioning,
// with the process outside the repository as on a runtime broker, must
// recreate the worktree rather than attach to the missing path. In shared
// mode, which has no per-agent worktree path, it must hit the
// shared-workspace guard instead.
func TestProvisionAgent_StaleWorktreeRegistrationRecreated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ctx    func() context.Context
		shared bool
	}{
		{"worktree-per-agent", context.Background, false},
		{"shared", sharedModeCtx, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scionDir, _ := reprovisionSetup(t)
			agentName := "stale-registration"
			if _, _, _, err := ProvisionAgent(context.Background(), agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
				t.Fatalf("initial ProvisionAgent: %v", err)
			}
			agentDir := filepath.Join(scionDir, "agents", agentName)
			worktree := filepath.Join(agentDir, "workspace")
			if err := os.RemoveAll(agentDir); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("git", "-C", filepath.Dir(scionDir), "worktree", "list", "--porcelain").CombinedOutput()
			if err != nil || !strings.Contains(string(out), "prunable") {
				t.Fatalf("expected a prunable worktree registration after removing the agent dir: %v\n%s", err, out)
			}

			_, _, cfg, err := ProvisionAgent(tc.ctx(), agentName, "default", "", "", scionDir, "", "created", "", "")
			if tc.shared {
				if err == nil || !strings.Contains(err.Error(), "shared workspace") {
					t.Fatalf("expected the shared-workspace guard error, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("re-provision after the agent dir was removed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(worktree, ".git")); err != nil {
				t.Errorf("expected the worktree to be recreated at %s: %v", worktree, err)
			}
			for _, v := range cfg.Volumes {
				if v.Target != "/workspace" {
					continue
				}
				if _, err := os.Stat(v.Source); err != nil {
					t.Errorf("/workspace source %q does not exist: %v", v.Source, err)
				}
			}
		})
	}
}
