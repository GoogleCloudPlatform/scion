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

package rebuildpreflight

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func runGitSteps(dir string, steps []Step) error {
	for _, s := range steps {
		cmd := exec.Command("git", s.Args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return &stepExecError{step: s.Name, err: err, out: string(out)}
		}
	}
	return nil
}

type stepExecError struct {
	step string
	err  error
	out  string
}

func (e *stepExecError) Error() string {
	return e.step + ": " + e.err.Error() + ": " + e.out
}

// setupRealGitRepos creates a local bare origin and a working clone with:
//   - commit1 on main
//   - commit2 on main (newer tip on origin/main)
//   - orphanSHA on a separate branch "other" (not an ancestor of origin/main)
func setupRealGitRepos(t *testing.T) (workDir, commit1, commit2, orphanSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	workDir = filepath.Join(tmp, "work")
	runGit(t, tmp, "init", "--bare", "-b", "main", origin)
	runGit(t, tmp, "clone", origin, workDir)
	runGit(t, workDir, "checkout", "-b", "main")

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("a.txt", "one\n")
	runGit(t, workDir, "add", "a.txt")
	runGit(t, workDir, "commit", "-m", "commit 1")
	commit1 = runGit(t, workDir, "rev-parse", "HEAD")

	writeFile("a.txt", "two\n")
	runGit(t, workDir, "add", "a.txt")
	runGit(t, workDir, "commit", "-m", "commit 2")
	commit2 = runGit(t, workDir, "rev-parse", "HEAD")
	runGit(t, workDir, "push", "-u", "origin", "main")

	// Create a divergent branch "other" not merged into main.
	runGit(t, workDir, "checkout", "-b", "other", commit1)
	writeFile("b.txt", "other\n")
	runGit(t, workDir, "add", "b.txt")
	runGit(t, workDir, "commit", "-m", "other branch commit")
	orphanSHA = runGit(t, workDir, "rev-parse", "HEAD")
	runGit(t, workDir, "push", "-u", "origin", "other")

	// Return to main at commit2 (tip).
	runGit(t, workDir, "checkout", "main")
	return workDir, commit1, commit2, orphanSHA
}

// TestGitSteps_RealGit proves GitSteps argv against real git (no network)
// for branch pin (S09), unreachable-commit ancestry check (S10), detached
// pin + missing commit (S11), and unforced detached reset (S37).
func TestGitSteps_RealGit(t *testing.T) {
	workDir, commit1, commit2, orphanSHA := setupRealGitRepos(t)

	t.Run("branch pin lands on pinned commit, not tip (kills S09)", func(t *testing.T) {
		steps := GitSteps("main", &Pin{Branch: "main", Commit: commit1})
		if err := runGitSteps(workDir, steps); err != nil {
			t.Fatalf("branch pin steps failed: %v", err)
		}
		got := runGit(t, workDir, "rev-parse", "HEAD")
		if got != commit1 {
			t.Fatalf("HEAD = %s, want pinned commit %s (tip is %s)", got, commit1, commit2)
		}
	})

	t.Run("branch pin rejects commit not on origin/branch (kills S10)", func(t *testing.T) {
		steps := GitSteps("main", &Pin{Branch: "main", Commit: orphanSHA})
		if err := runGitSteps(workDir, steps); err == nil {
			got := runGit(t, workDir, "rev-parse", "HEAD")
			t.Fatalf("expected ancestry check to fail for commit %s not on origin/main, but steps succeeded (HEAD=%s)", orphanSHA, got)
		}
	})

	t.Run("detached pin checks out commit and rejects missing commit (kills S11)", func(t *testing.T) {
		steps := GitSteps(DetachedSentinel, &Pin{Branch: DetachedSentinel, Commit: commit1, Detached: true})
		if err := runGitSteps(workDir, steps); err != nil {
			t.Fatalf("detached pin steps failed: %v", err)
		}
		got := runGit(t, workDir, "rev-parse", "HEAD")
		if got != commit1 {
			t.Fatalf("HEAD = %s, want %s", got, commit1)
		}

		// Missing commit must fail at cat-file before checkout.
		missingSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		missSteps := GitSteps(DetachedSentinel, &Pin{Branch: DetachedSentinel, Commit: missingSHA, Detached: true})
		if len(missSteps) < 2 || missSteps[1].Args[0] != "cat-file" {
			t.Fatalf("expected cat-file step before checkout, got %+v", missSteps)
		}
		if err := runGitSteps(workDir, missSteps); err == nil {
			t.Fatalf("expected missing commit %s to fail", missingSHA)
		}
	})

	t.Run("unforced detached resets to origin/HEAD (kills S37)", func(t *testing.T) {
		steps := GitSteps(DetachedSentinel, nil)
		if len(steps) != 2 || steps[1].Args[0] != "reset" || steps[1].Args[2] != "origin/HEAD" {
			t.Fatalf("unexpected unforced detached steps: %+v", steps)
		}
		if err := runGitSteps(workDir, steps); err != nil {
			t.Fatalf("unforced detached steps failed: %v", err)
		}
		got := runGit(t, workDir, "rev-parse", "HEAD")
		if got != commit2 {
			t.Fatalf("HEAD = %s, want origin/HEAD (%s)", got, commit2)
		}
	})
}
