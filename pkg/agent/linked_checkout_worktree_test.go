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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestProvisionAgent_BrokerMode_LinkedCheckoutWorktree pins the path a
// worktree-per-agent create for a linked checkout takes on the co-located
// broker (the broker leaves GitClone and Workspace empty): broker-mode
// ProvisionAgent, run from outside the repository, creates the agent's
// worktree at <repo>/.scion/agents/<name>/workspace on branch <name> in the
// user's repository, without touching the user's HEAD and without any clone
// or base clone. DeleteAgentFiles with removeBranch then removes the
// worktree, its registration and the branch.
func TestProvisionAgent_BrokerMode_LinkedCheckoutWorktree(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	repo := filepath.Dir(scionDir)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	headBefore := git("symbolic-ref", "HEAD")
	commitBefore := git("rev-parse", "HEAD")

	// A broker runs outside the repository.
	t.Chdir(t.TempDir())
	ctx := api.ContextWithBrokerMode(context.Background())

	if _, _, _, err := ProvisionAgent(ctx, "a1", "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}

	resolvedScionDir, err := config.GetResolvedProjectDir(scionDir)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(resolvedScionDir, "agents", "a1", "workspace")
	if info, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || info.IsDir() {
		t.Fatalf("expected a worktree (.git file) at %s: info=%v err=%v", wt, info, err)
	}
	if got := strings.TrimSpace(runGitIn(t, wt, "rev-parse", "--abbrev-ref", "HEAD")); got != "a1" {
		t.Errorf("worktree branch = %q, want a1", got)
	}
	common := strings.TrimSpace(runGitIn(t, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if want := filepath.Join(repo, ".git"); !sameDir(common, want) {
		t.Errorf("worktree common dir = %q, want the user's repository %q", common, want)
	}
	if !strings.Contains(git("worktree", "list"), wt) {
		t.Errorf("git worktree list does not show %s", wt)
	}
	if got := git("symbolic-ref", "HEAD"); got != headBefore {
		t.Errorf("user's HEAD changed: %q -> %q", headBefore, got)
	}
	if got := git("rev-parse", "HEAD"); got != commitBefore {
		t.Errorf("user's HEAD commit changed: %q -> %q", commitBefore, got)
	}
	for _, p := range []string{filepath.Join(repo, "worktrees"), filepath.Join(scionDir, "workspace")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("unexpected %s (stat err: %v): no base clone or in-repo worktrees dir may be created", p, err)
		}
	}

	if _, err := DeleteAgentFiles("a1", scionDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s still exists after delete (stat err: %v)", wt, err)
	}
	if strings.Contains(git("worktree", "list"), wt) {
		t.Errorf("git worktree list still shows %s after delete", wt)
	}
	if got := git("branch", "--list", "a1"); got != "" {
		t.Errorf("branch a1 still exists after delete with removeBranch: %q", got)
	}
}

func runGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
	}
	return string(out)
}
