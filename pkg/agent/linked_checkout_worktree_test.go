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
	"io"
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

// TestProvisionAgent_BrokerMode_AgentNamedAfterCheckedOutBranch pins the
// local-mode behaviour when an agent's name equals the branch the user has
// checked out: broker-mode ProvisionAgent attaches the agent to the user's
// own checkout (with a "Relying on existing worktree" warning) rather than
// creating a worktree. Deleting that agent, even with removeBranch, must
// leave the user's checkout, its files (tracked, modified and untracked)
// and its checked-out branch intact.
func TestProvisionAgent_BrokerMode_AgentNamedAfterCheckedOutBranch(t *testing.T) {
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
	// The user works on branch a2, with a committed file, an uncommitted
	// edit and an untracked file.
	git("checkout", "-q", "-b", "a2")
	tracked := filepath.Join(repo, "notes.txt")
	if err := os.WriteFile(tracked, []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "notes.txt")
	git("commit", "-q", "-m", "user work")
	if err := os.WriteFile(tracked, []byte("committed\nuncommitted edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(repo, "scratch.txt")
	if err := os.WriteFile(untracked, []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitBefore := git("rev-parse", "HEAD")

	t.Chdir(t.TempDir())
	ctx := api.ContextWithBrokerMode(context.Background())
	var provErr error
	out := captureStdoutForTest(t, func() {
		_, _, _, provErr = ProvisionAgent(ctx, "a2", "default", "", "", scionDir, "", "created", "", "")
	})
	if provErr != nil {
		t.Fatalf("ProvisionAgent: %v", provErr)
	}
	// The docs quote this warning; the path it names is the user's checkout.
	const warnPrefix = "Warning: Relying on existing worktree for branch 'a2' at '"
	if i := strings.Index(out, warnPrefix); i < 0 {
		t.Errorf("ProvisionAgent output does not contain %q; got:\n%s", warnPrefix, out)
	} else {
		rest := out[i+len(warnPrefix):]
		if j := strings.Index(rest, "'"); j < 0 || !sameDir(rest[:j], repo) {
			t.Errorf("warning does not name the user's checkout %s; got:\n%s", repo, out)
		}
	}
	resolvedScionDir, err := config.GetResolvedProjectDir(scionDir)
	if err != nil {
		t.Fatal(err)
	}
	// Attached, not created: no worktree of its own.
	if _, err := os.Lstat(filepath.Join(resolvedScionDir, "agents", "a2", "workspace", ".git")); !os.IsNotExist(err) {
		t.Errorf("expected no worktree of the agent's own (stat err: %v)", err)
	}
	if got := git("worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("expected only the user's main worktree, got:\n%s", got)
	}

	if _, err := DeleteAgentFiles("a2", scionDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}
	if got := git("symbolic-ref", "--short", "HEAD"); got != "a2" {
		t.Errorf("user's checked-out branch = %q after delete, want a2", got)
	}
	if got := git("rev-parse", "HEAD"); got != commitBefore {
		t.Errorf("user's HEAD commit changed: %q -> %q", commitBefore, got)
	}
	if got := git("branch", "--list", "a2"); got == "" {
		t.Error("branch a2 was deleted")
	}
	if b, err := os.ReadFile(tracked); err != nil || string(b) != "committed\nuncommitted edit\n" {
		t.Errorf("tracked file with uncommitted edit changed: %q, %v", b, err)
	}
	if b, err := os.ReadFile(untracked); err != nil || string(b) != "untracked\n" {
		t.Errorf("untracked file changed: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".gitignore")); err != nil {
		t.Errorf("committed .gitignore missing after delete: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(resolvedScionDir, "agents", "a2")); !os.IsNotExist(err) {
		t.Errorf("agent dir still exists after delete (stat err: %v)", err)
	}
}

// captureStdoutForTest returns what fn writes to os.Stdout.
func captureStdoutForTest(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}
