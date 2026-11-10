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

// These tests use the unix-only repository fixtures (mkGitRepo).
//go:build unix

package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// addRefusedInclude appends, as data, an include directive (refused by the
// repository config policy) to repo's .git/config. git itself ignores an
// include whose file does not exist.
func addRefusedInclude(t *testing.T, repo string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[include]\n\tpath = does-not-exist\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// The caller's ceiling bounds discovery: git finds the repository several
// levels above dir when the ceiling allows it, and nothing when it does not.
// Mutants this catches: CeilingOn instead of CeilingOff plus the env ceiling
// (the first lookup is bounded at dir's parent and fails); dropping the env
// ceiling (the second lookup finds the repository).
func TestBoundedRevParse_CeilingBoundsDiscovery(t *testing.T) {
	repo := mkGitRepo(t, filepath.Join(t.TempDir(), "repo"))
	dir := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	top, err := boundedRevParse(dir, filepath.Dir(repo), "--show-toplevel")
	if err != nil {
		t.Fatalf("boundedRevParse with the ceiling above the repository: %v", err)
	}
	if eval(t, top) != eval(t, repo) {
		t.Fatalf("top level = %q, want %q", top, repo)
	}

	_, err = boundedRevParse(dir, filepath.Join(repo, "a"), "--show-toplevel")
	if err == nil {
		t.Fatal("boundedRevParse with the ceiling below the repository found a repository")
	}
	if errors.Is(err, errGitProbeTimeout) || errors.Is(err, errGitProbeRefused) {
		t.Fatalf("error = %v, want git's own non-zero exit (not a timeout or a refusal)", err)
	}
}

// A workspace inside the project's repository with no .git but a HEAD entry
// is refused by the host-side git door, and the refusal refuses the start.
// Mutant this catches: a door refusal falling through to the project-dir
// fallback (which returns the whole project repository).
func TestDetectRepoRoot_WorkspaceDoorRefusalNeverWidens(t *testing.T) {
	root := mkGitRepo(t, filepath.Join(t.TempDir(), "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	ws := filepath.Join(root, "sub", "ws")
	for _, d := range []string{projectDir, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err == nil || got != "" {
		t.Fatalf("detectRepoRoot = (%q, %v), want a refusal, never the project repository %q", got, err, root)
	}
	if !errors.Is(err, errGitProbeRefused) {
		t.Fatalf("error = %v, want errGitProbeRefused", err)
	}
}

// A checkout outside the project whose own repository config the policy
// refuses refuses the start, with the refusal. Before the probe went through
// the door this returned the workspace as the root (an intended change).
// Mutant this catches: a door refusal read as "not a repository" (a plain
// mount, ("", nil)).
func TestDetectRepoRoot_OwnGitDirPolicyRefusalRefuses(t *testing.T) {
	base := t.TempDir()
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := mkGitRepo(t, filepath.Join(base, "backend", "workspace"))
	addRefusedInclude(t, ws)
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err == nil || got != "" {
		t.Fatalf("detectRepoRoot = (%q, %v), want a refusal", got, err)
	}
	var refused *util.RepoConfigRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want a *util.RepoConfigRefusedError", err)
	}
}

// The project-dir fallback, with the project directory the repository root
// itself, returns a repository config policy refusal instead of giving a
// plain mount; without the refused key it returns the repository root.
// Mutant this catches: the fallback treating every error as a plain mount
// (("", nil) with the refused key).
func TestDetectRepoRoot_ProjectFallbackPolicyRefusalReturned(t *testing.T) {
	root := mkGitRepo(t, filepath.Join(t.TempDir(), "repo"))
	got, err := detectRepoRoot(false, "", root, nil)
	if err != nil || eval(t, got) != eval(t, root) {
		t.Fatalf("control: detectRepoRoot = (%q, %v), want (%q, nil)", got, err, root)
	}
	addRefusedInclude(t, root)
	got, err = detectRepoRoot(false, "", root, nil)
	if err == nil || got != "" {
		t.Fatalf("detectRepoRoot = (%q, %v), want a refusal", got, err)
	}
	var refused *util.RepoConfigRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want a *util.RepoConfigRefusedError", err)
	}
}

// A policy refusal from the worktree-registration check is returned with its
// remedy instead of narrowing the roots silently.
// Mutant this catches: discarding the IsRegisteredWorktree error (returns
// [projectRepoRoot], nil).
func TestWorkspaceSourceRoots_WorktreeRegistrationRefusalReturned(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	base := t.TempDir()
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	runGitIn(t, root, "worktree", "add", "-b", "ws", ws)
	addRefusedInclude(t, root)
	roots, err := workspaceSourceRoots(false, ws, nil, projectDir)
	if err == nil || roots != nil {
		t.Fatalf("workspaceSourceRoots = (%v, %v), want (nil, refusal)", roots, err)
	}
	var refused *util.RepoConfigRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want a *util.RepoConfigRefusedError", err)
	}
}

// Control: a workspace that is not a registered worktree, with a clean
// config, still gives only the project's repository root and no error.
func TestWorkspaceSourceRoots_UnregisteredWorkspaceNoRefusal(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := mkGitRepo(t, filepath.Join(t.TempDir(), "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	ws := filepath.Join(root, "plain")
	for _, d := range []string{projectDir, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := workspaceSourceRoots(false, ws, nil, projectDir)
	if err != nil || len(roots) != 1 || eval(t, roots[0]) != eval(t, root) {
		t.Fatalf("workspaceSourceRoots = (%v, %v), want ([%q], nil)", roots, err, root)
	}
}

// A missing git binary is not a door refusal: it reads as git's own failure,
// so a non-git workspace on a host without git keeps a plain mount.
// Mutant this catches: dropping the exec.ErrNotFound case (the error would
// wrap errGitProbeRefused).
func TestBoundedRevParse_GitNotFoundIsNotARefusal(t *testing.T) {
	t.Run("discovery mode", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		_, err := boundedRevParse(dir, filepath.Dir(dir), "--show-toplevel")
		if err == nil {
			t.Fatal("boundedRevParse succeeded without git on PATH")
		}
		if errors.Is(err, errGitProbeTimeout) || errors.Is(err, errGitProbeRefused) {
			t.Fatalf("error = %v, want neither a timeout nor a refusal", err)
		}
		if !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("error = %v, want it to wrap exec.ErrNotFound", err)
		}
	})
	// In repository mode the door's policy check runs git first; the
	// missing binary is still not a refusal.
	t.Run("repository mode", func(t *testing.T) {
		repo := mkGitRepo(t, filepath.Join(t.TempDir(), "repo"))
		t.Setenv("PATH", t.TempDir())
		_, err := boundedRevParse(repo, filepath.Dir(repo), "--git-common-dir")
		if !errors.Is(err, exec.ErrNotFound) || errors.Is(err, errGitProbeRefused) {
			t.Fatalf("error = %v, want exec.ErrNotFound and not errGitProbeRefused", err)
		}
	})
}

// Control: an IsRegisteredWorktree error that is not a policy refusal (here
// a workspace path that does not exist) keeps today's behaviour: no extra
// root and no error.
// Mutant this catches: returning every IsRegisteredWorktree error.
func TestWorkspaceSourceRoots_RegistrationErrorNotRefusalKeepsRoots(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := mkGitRepo(t, filepath.Join(t.TempDir(), "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "does-not-exist")
	roots, err := workspaceSourceRoots(false, ws, nil, projectDir)
	if err != nil || len(roots) != 1 || eval(t, roots[0]) != eval(t, root) {
		t.Fatalf("workspaceSourceRoots = (%v, %v), want ([%q], nil)", roots, err, root)
	}
}

// A workspace that does not exist yet, outside the project's repository, is
// not probed: it falls through to the project-dir fallback, which gives a
// plain mount for a workspace outside the repository ("", nil), as before
// the probe went through the door.
// Mutant this catches: dropping the Lstat guard (the door cannot resolve the
// missing directory and the start is refused).
func TestDetectRepoRoot_MissingWorkspaceOutsideProjectRepoIsPlain(t *testing.T) {
	// Resolve the temp dir so the missing workspace path and the resolved
	// repository root share a prefix (macOS /var -> /private/var).
	base := eval(t, t.TempDir())
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "elsewhere", "workspace")
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err != nil || got != "" {
		t.Fatalf("detectRepoRoot = (%q, %v), want (\"\", nil)", got, err)
	}
}

// A workspace that does not exist yet, inside the project's repository, is
// not probed: it falls through to the project-dir fallback, which probes
// projectDir (<repo>/.scion) and returns the project repository root, as
// before the probe went through the door.
// Mutant this catches: dropping the Lstat guard (the door cannot resolve the
// missing directory and the start is refused).
func TestDetectRepoRoot_MissingWorkspaceInsideProjectRepoFallsBack(t *testing.T) {
	// Resolve the temp dir so the missing workspace path and the resolved
	// repository root share a prefix (macOS /var -> /private/var).
	base := eval(t, t.TempDir())
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "agents", "dev", "workspace")
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err != nil || eval(t, got) != eval(t, root) {
		t.Fatalf("detectRepoRoot = (%q, %v), want (%q, nil)", got, err, root)
	}
}

// A dangling symlink workspace exists for Lstat, so it is probed, and the
// door cannot resolve it, so the start is refused; it never reaches the
// project-dir fallback.
// Mutant this catches: Lstat replaced by Stat (the symlink reads as missing
// and falls through to the fallback, which returns the repository root).
func TestDetectRepoRoot_DanglingSymlinkWorkspaceIsRefused(t *testing.T) {
	// Resolve the temp dir so paths compare with the resolved repository
	// root (macOS /var -> /private/var).
	base := eval(t, t.TempDir())
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	ws := filepath.Join(root, "agents", "dev", "workspace")
	for _, d := range []string{projectDir, filepath.Dir(ws)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "no-such-target"), ws); err != nil {
		t.Fatal(err)
	}
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if got != "" || !errors.Is(err, errGitProbeRefused) {
		t.Fatalf("detectRepoRoot = (%q, %v), want (\"\", errGitProbeRefused)", got, err)
	}
}

// writeGitfile writes ws/.git as a regular file with content.
func writeGitfile(t *testing.T, ws, content string) {
	t.Helper()
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A workspace whose .git file names a gitdir that does not exist (a leftover
// from a removed worktree), with a project that is not a git repository, is
// not probed: the fallback finds no repository and gives a plain mount
// ("", nil), as before the probe went through the door.
// Mutant this catches: dropping the dangling-gitfile skip (the door's config
// listing fails and the start is refused).
func TestDetectRepoRoot_DanglingGitfileOutsideProjectRepoIsPlain(t *testing.T) {
	base := eval(t, t.TempDir())
	projectDir := filepath.Join(base, "project", config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(projectDir, "agents", "dev", "workspace")
	writeGitfile(t, ws, "gitdir: ../../../.git/worktrees/dev\n")
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err != nil || got != "" {
		t.Fatalf("detectRepoRoot = (%q, %v), want (\"\", nil)", got, err)
	}
}

// A workspace inside the project's repository whose .git file names a gitdir
// that does not exist is not probed: the fallback probes projectDir
// (<repo>/.scion) and returns the project repository root, as before the
// probe went through the door.
// Mutant this catches: dropping the dangling-gitfile skip (the start is
// refused).
func TestDetectRepoRoot_DanglingGitfileInsideProjectRepoFallsBack(t *testing.T) {
	base := eval(t, t.TempDir())
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "agents", "dev", "workspace")
	writeGitfile(t, ws, "gitdir: "+filepath.Join(root, ".git", "worktrees", "removed")+"\n")
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err != nil || eval(t, got) != eval(t, root) {
		t.Fatalf("detectRepoRoot = (%q, %v), want (%q, nil)", got, err, root)
	}
}

// Only a well-formed dangling pointer is skipped: a malformed .git file is
// still probed, and the door refuses it, which refuses the start.
// Mutant this catches: skipping the probe for any unreadable gitfile (the
// fallback would return the repository root).
func TestDetectRepoRoot_MalformedGitfileStillProbed(t *testing.T) {
	base := eval(t, t.TempDir())
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "agents", "dev", "workspace")
	writeGitfile(t, ws, "not a gitdir line\n")
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if got != "" || !errors.Is(err, errGitProbeRefused) {
		t.Fatalf("detectRepoRoot = (%q, %v), want (\"\", errGitProbeRefused)", got, err)
	}
}

// A .git file that git parses differently from a well-formed dangling
// pointer is probed, not skipped, and the door refuses it, which refuses the
// start. Git keeps everything after "gitdir: " (stripping only trailing CR
// and LF) and resolves a relative path physically.
// Mutants this catches: for "two spaces" and "interior newline", a looser
// parse (first line only, surrounding whitespace trimmed) that reads a
// missing target and skips the probe (the fallback returns the repository
// root); for "symlink then dotdot", a lexical Clean or Join of the target
// (it reads ws/missing as absent); for "over 4096 bytes", accepting a
// truncated read.
func TestDetectRepoRoot_GitfileGitParsesDifferentlyIsProbed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, base, ws string) string
	}{
		{name: "two spaces", setup: func(t *testing.T, base, ws string) string {
			return "gitdir:  " + filepath.Join(base, "missing-a") + "\n"
		}},
		{name: "interior newline", setup: func(t *testing.T, base, ws string) string {
			return "gitdir: x\nfoo\n"
		}},
		{name: "symlink then dotdot", setup: func(t *testing.T, base, ws string) string {
			// ws/link points at <base>/elsewhere/sub, so link/../missing is
			// <base>/elsewhere/missing for the kernel (it exists) while a
			// lexical reading gives ws/missing (absent).
			for _, d := range []string{filepath.Join(base, "elsewhere", "sub"), filepath.Join(base, "elsewhere", "missing")} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(base, "elsewhere", "sub"), filepath.Join(ws, "link")); err != nil {
				t.Fatal(err)
			}
			return "gitdir: link/../missing\n"
		}},
		{name: "over 4096 bytes", setup: func(t *testing.T, base, ws string) string {
			return "gitdir: " + filepath.Join(base, "missing-d") + strings.Repeat("\n", 4200)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := eval(t, t.TempDir())
			root := mkGitRepo(t, filepath.Join(base, "repo"))
			projectDir := filepath.Join(root, config.DotScion)
			if err := os.MkdirAll(projectDir, 0o755); err != nil {
				t.Fatal(err)
			}
			ws := filepath.Join(root, "agents", "dev", "workspace")
			writeGitfile(t, ws, tc.setup(t, base, ws))
			got, err := detectRepoRoot(false, ws, projectDir, nil)
			if got != "" || !errors.Is(err, errGitProbeRefused) {
				t.Fatalf("detectRepoRoot = (%q, %v), want (\"\", errGitProbeRefused)", got, err)
			}
		})
	}
}
