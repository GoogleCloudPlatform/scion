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

// These tests exercise the unix git-metadata mounts and private config checks
// (symlinks, link counts, ownership); on other platforms they are
// ErrUnsupported stubs.
//go:build unix

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// A private-config source that is a symlink, a non-regular file, or missing
// is refused, so the start fails closed rather than binding an unsafe source.
func TestCheckPrivateGitConfigSource(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "config")
	if err := os.WriteFile(good, []byte("[core]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateGitConfigSource(good); err != nil {
		t.Errorf("a regular broker-owned file should be accepted, got: %v", err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateGitConfigSource(link); err == nil {
		t.Error("a symlink source must be refused, got nil")
	}

	if err := checkPrivateGitConfigSource(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing source must be refused, got nil")
	}

	if err := checkPrivateGitConfigSource(dir); err == nil {
		t.Error("a directory source must be refused, got nil")
	}
}

// Via buildCommonRunArgs: a worktree layout whose private-config source is a
// symlink refuses the start.
func TestBuildCommonRunArgs_RefusesSymlinkedPrivateConfig(t *testing.T) {
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
	realCfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(realCfg, []byte("[core]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realCfg, link); err != nil {
		t.Fatal(err)
	}
	_, err := buildCommonRunArgs(RunConfig{
		Harness:                &harness.Generic{},
		Name:                   "test-agent",
		UnixUsername:           "scion",
		Image:                  "scion-agent:latest",
		RuntimeName:            "docker",
		RepoRoot:               repoRoot,
		Workspace:              workspace,
		PrivateGitConfigSource: link,
	})
	if err == nil {
		t.Fatal("expected a refusal for a symlinked private-config source, got nil")
	}
}

// Sibling worktree admin dirs are not writable — the .git root is read-only
// (so worktrees/ is read-only by inheritance) with only the agent's own
// worktrees/<name>/ overlaid read-write.
func TestWorktreesDir_SiblingNotWritable(t *testing.T) {
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
	gitDir := filepath.Join(repoRoot, ".git")
	// The agent's own worktree admin dir and a sibling's.
	for _, name := range []string{"agent-1", "other"} {
		d := filepath.Join(gitDir, "worktrees", name)
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "HEAD"), []byte("ref: refs/heads/x\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	args, err := buildCommonRunArgs(RunConfig{
		Harness:                &harness.Generic{},
		Name:                   "test-agent",
		UnixUsername:           "scion",
		Image:                  "scion-agent:latest",
		RuntimeName:            "docker",
		RepoRoot:               repoRoot,
		Workspace:              workspace,
		PrivateGitConfigSource: seedTestPrivateGitConfig(t),
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	argStr := strings.Join(args, " ")

	// The .git root is read-only (worktrees/ read-only by inheritance); the
	// agent's own worktrees/agent-1 is overlaid read-write.
	wantRO := "-v " + gitDir + ":/repo-root/.git:ro"
	if !strings.Contains(argStr, wantRO) {
		t.Errorf("expected read-only .git root mount %q, got: %s", wantRO, argStr)
	}
	wantOwnRW := "-v " + filepath.Join(gitDir, "worktrees", "agent-1") + ":/repo-root/.git/worktrees/agent-1 "
	if !strings.Contains(argStr, wantOwnRW) {
		t.Errorf("expected the agent's own worktree admin dir to be read-write %q, got: %s", wantOwnRW, argStr)
	}
	// There must be no read-write overlay for the sibling.
	if strings.Contains(argStr, "/repo-root/.git/worktrees/other ") {
		t.Errorf("a sibling worktree admin dir must not be writable, got: %s", argStr)
	}
}

// The shared-plain git layout is refused on the Apple runtime (cannot bind a
// single-file private config; a read-only .git would break the main worktree).
func TestSharedPlainGitMounts_RefusedOnApple(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git", "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".git", "config"), []byte("[core]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  appleContainerRuntimeName,
		RepoRoot:     repoRoot,
		Workspace:    repoRoot, // shared-plain: workspace == repo root
		// Seed the private config source so the ONLY reason to refuse is the
		// Apple shared-plain branch: removing that branch would then start, so
		// the errors.Is below pins it (not a missing-source error).
		PrivateGitConfigSource: seedTestPrivateGitConfig(t),
	})
	if !errors.Is(err, errAppleSharedWorkspaceUnsupported) {
		t.Fatalf("expected errAppleSharedWorkspaceUnsupported for the shared-plain Apple layout, got %v", err)
	}
}

// A non-git shared-plain workspace (no .git dir) is allowed on Apple: there is no
// admin surface to protect and nothing to make private.
func TestSharedPlainGitMounts_NonGitAllowedOnApple(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := t.TempDir() // no .git
	if _, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  appleContainerRuntimeName,
		RepoRoot:     repoRoot,
		Workspace:    repoRoot,
	}); err != nil {
		t.Fatalf("a non-git shared workspace should be allowed on Apple, got: %v", err)
	}
}

// The read-write own-worktree overlay is chosen from the broker-trusted
// back-links in the common dir, NOT from the agent-writable <workspace>/.git
// gitfile. An agent-written gitfile (pointing at a sibling admin dir, at ".",
// or at "..") must not change which admin dir is overlaid read-write.
func TestWorktreeAdmin_IgnoresAgentWritableGitfile(t *testing.T) {
	for _, agentPtr := range []string{
		"gitdir: ../../.git/worktrees/other", // (a) a sibling's admin dir
		"gitdir: .",                          // (b) the whole worktrees/ dir
		"gitdir: ..",                         // (c) the .git root (would void Apple ro base)
	} {
		t.Run(agentPtr, func(t *testing.T) {
			repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
			gitDir := filepath.Join(repoRoot, ".git")
			// A sibling worktree admin dir alongside the agent's own.
			sibling := filepath.Join(gitDir, "worktrees", "other")
			if err := os.MkdirAll(sibling, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sibling, "HEAD"), []byte("ref: refs/heads/x\n"), 0644); err != nil {
				t.Fatal(err)
			}
			// The agent rewrites its own workspace/.git gitfile.
			if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte(agentPtr+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			args, err := buildCommonRunArgs(RunConfig{
				Harness:                &harness.Generic{},
				Name:                   "test-agent",
				UnixUsername:           "scion",
				Image:                  "scion-agent:latest",
				RuntimeName:            "docker",
				RepoRoot:               repoRoot,
				Workspace:              workspace,
				PrivateGitConfigSource: seedTestPrivateGitConfig(t),
			})
			if err != nil {
				t.Fatalf("buildCommonRunArgs failed: %v", err)
			}
			argStr := strings.Join(args, " ")
			// The own-worktree rw overlay targets the trusted admin (agent-1).
			if !strings.Contains(argStr, "-v "+filepath.Join(gitDir, "worktrees", "agent-1")+":/repo-root/.git/worktrees/agent-1 ") {
				t.Errorf("own rw overlay should target the trusted admin agent-1, got: %s", argStr)
			}
			// It must NOT give the sibling, the whole worktrees dir, or the .git
			// root a read-write overlay.
			if strings.Contains(argStr, "/repo-root/.git/worktrees/other ") {
				t.Errorf("a sibling admin dir must not be read-write, got: %s", argStr)
			}
			if strings.Contains(argStr, "/repo-root/.git/worktrees :") || strings.Contains(argStr, "/repo-root/.git/worktrees ") {
				t.Errorf("the whole worktrees dir must not be read-write, got: %s", argStr)
			}
			// The .git root is read-only, so sibling worktree admin dirs are
			// read-only by inheritance (no explicit rw overlay for them).
			if !strings.Contains(argStr, "-v "+gitDir+":/repo-root/.git:ro") {
				t.Errorf("the .git root must be read-only, got: %s", argStr)
			}
		})
	}
}

// checkPrivateGitConfigSource refuses a hard-linked source (link count > 1).
func TestCheckPrivateGitConfigSource_RefusesHardLink(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "config")
	if err := os.WriteFile(good, []byte("[core]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.Link(good, other); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	if err := checkPrivateGitConfigSource(good); err == nil {
		t.Error("a hard-linked (nlink>1) source must be refused, got nil")
	}
}

// Pins the os.Lstat regular-file guard in util.TrustedWorktreeAdminName. The workspace
// .git gitfile is agent-writable, so an agent could replace it with a symlink. The
// resolver's lexical canonicalisation (canonicalGitfilePath names the final ".git"
// component without following it) already stops a symlink from *redirecting* the
// match to a sibling admin dir. This test exercises the SECOND barrier: the agent's
// OWN back-link legitimately matches <ws>/.git, so without the regular-file guard a
// symlinked <ws>/.git would still select the own admin dir; the guard refuses a
// non-regular <ws>/.git outright (fail closed). Disabling the os.Lstat ... IsRegular
// guard makes this test return "agent-1"/true instead of refusing — i.e. the guard,
// not the lexical step, is what fails this fixture closed.
func TestTrustedWorktreeAdminName_RefusesSymlinkedGitfile(t *testing.T) {
	repoRoot := t.TempDir()
	gitDir := filepath.Join(repoRoot, ".git")
	siblingWs := filepath.Join(repoRoot, "sib")
	if err := os.MkdirAll(siblingWs, 0o755); err != nil {
		t.Fatal(err)
	}
	// A legitimate sibling worktree admin whose back-link points at its own gitfile.
	setupWorktreeAdmin(t, repoRoot, "other", siblingWs)

	ws := filepath.Join(repoRoot, "wt")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	ownAdmin := filepath.Join(gitDir, "worktrees", "agent-1")
	if err := os.MkdirAll(ownAdmin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The own back-link MATCHES <ws>/.git (lexically), so the regular-file guard is
	// the only thing standing between a symlinked <ws>/.git and selecting this admin.
	if err := os.WriteFile(filepath.Join(ownAdmin, "gitdir"), []byte(filepath.Join(ws, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The agent replaces <ws>/.git with a symlink (here, at the sibling's gitfile).
	if err := os.Symlink(filepath.Join(siblingWs, ".git"), filepath.Join(ws, ".git")); err != nil {
		t.Fatal(err)
	}

	name, ok := util.TrustedWorktreeAdminName(gitDir, ws)
	if ok || name != "" {
		t.Errorf("a symlinked <workspace>/.git must be refused (fail closed), got name=%q ok=%v", name, ok)
	}
}

// Invariant: the .git ROOT is read-only in the docker/podman worktree
// layout, so no new root-level entry can be created, and the ONLY read-write
// binds under /repo-root/.git are the bounded overlay set (objects, refs, logs,
// lfs, rr-cache and the agent's own worktrees/<name>/). A sibling
// worktree admin dir is read-only by inheritance (no rw overlay for it).
func TestReadOnlyGitRoot_WorktreeLayout_BoundedRwOverlays(t *testing.T) {
	for _, rt := range []string{"docker", "podman"} {
		t.Run(rt, func(t *testing.T) {
			repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
			gitDir := filepath.Join(repoRoot, ".git")
			// A sibling worktree admin dir that must stay read-only.
			sibling := filepath.Join(gitDir, "worktrees", "other")
			if err := os.MkdirAll(sibling, 0o755); err != nil {
				t.Fatal(err)
			}
			args, err := buildCommonRunArgs(RunConfig{
				Harness:                &harness.Generic{},
				Name:                   "test-agent",
				UnixUsername:           "scion",
				Image:                  "scion-agent:latest",
				RuntimeName:            rt,
				RepoRoot:               repoRoot,
				Workspace:              workspace,
				PrivateGitConfigSource: seedTestPrivateGitConfig(t),
			})
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			argStr := strings.Join(args, " ")
			// The base .git root is read-only.
			if !strings.Contains(argStr, "-v "+gitDir+":/repo-root/.git:ro") {
				t.Fatalf("expected a read-only base .git mount, got: %s", argStr)
			}
			if strings.Contains(argStr, "-v "+gitDir+":/repo-root/.git ") {
				t.Fatalf("base .git unexpectedly read-write for %s", rt)
			}
			// Every read-write (-v …:<target> with no :ro) bind under
			// /repo-root/.git must be in the bounded overlay allow-list.
			allowed := map[string]bool{
				"/repo-root/.git/objects":           true,
				"/repo-root/.git/refs":              true,
				"/repo-root/.git/logs":              true,
				"/repo-root/.git/lfs":               true,
				"/repo-root/.git/rr-cache":          true,
				"/repo-root/.git/worktrees/agent-1": true,
			}
			for i, a := range args {
				if a != "-v" || i+1 >= len(args) {
					continue
				}
				spec := args[i+1]
				j := strings.Index(spec, ":/repo-root/.git")
				if j < 0 || strings.HasSuffix(spec, ":ro") {
					continue // not a .git bind, or read-only
				}
				target := spec[j+1:]
				if target == "/repo-root/.git" {
					t.Errorf("the .git root must not be read-write: %s", spec)
					continue
				}
				if !allowed[target] {
					t.Errorf("unexpected read-write bind under /repo-root/.git: %s", spec)
				}
			}
			// The sibling admin dir gets no read-write overlay.
			if strings.Contains(argStr, ":/repo-root/.git/worktrees/other ") {
				t.Errorf("a sibling admin dir must not be read-write, got: %s", argStr)
			}
		})
	}
}

// bindChecked (bindFileCreate) must return an error (so the caller refuses the
// start) when the create-first file cannot be created because an intermediate
// component is not a directory, rather than silently registering no read-only bind
// and leaving a writable file inside the rw .git root.
func TestBindReadOnlyCreateFirstFile_FailsClosed(t *testing.T) {
	gitRoot := t.TempDir()
	// A regular file where a directory is needed: the intermediate "blocker"
	// component is not a directory, so the component walk refuses.
	if err := os.WriteFile(filepath.Join(gitRoot, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	registered := false
	err := bindChecked(func(string, string, bool, bool) { registered = true },
		gitRoot, "/repo-root/.git", true, bindFileCreate, "blocker", "config.worktree")
	if err == nil {
		t.Error("expected an error when an intermediate component is not a directory, got nil")
	}
	if registered {
		t.Error("no mount must be registered when the source cannot be created")
	}
}

// Functional check of the read-only-.git-root layout with real git: with the
// common .git root made read-only and its subdirectories left writable (the real
// container overlays objects/refs/logs and the agent's own worktree admin dir
// read-write), an in-container git can still commit, create a branch, fetch, pull
// and push, but it CANNOT delete a branch (that needs packed-refs.lock in the
// read-only root, which fails cleanly) or create a new root-level file such as
// .git/commondir. This simulates ONLY
// the read-only .git root — in the real container hooks/ and info/ are read-only by
// inheritance from that root (not separate binds), objects/info is pinned read-only,
// and the private .git/config is bound read-only; none of that is simulated here. It exercises the worktree layout; the outside-root fallback mounts a
// registered worktree of the same common .git, so its git behaviour is identical.
func TestReadOnlyGitRoot_Functional(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; cannot simulate a read-only .git root")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gc"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	if err := os.WriteFile(filepath.Join(root, "gc"), []byte("[user]\n\temail=a@b.c\n\tname=A\n[init]\n\tdefaultBranch=main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) (string, error) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := c.CombinedOutput()
		return string(out), err
	}
	mustGit := func(dir string, args ...string) {
		t.Helper()
		if out, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// A bare remote, a main checkout, and a linked worktree on branch "agent-1"
	// with its upstream pre-set (so push needs no config write).
	bare := filepath.Join(root, "bare.git")
	mustGit(root, "init", "-q", "--bare", bare)
	main := filepath.Join(root, "main")
	mustGit(root, "init", "-q", "-b", "main", main)
	mustGit(main, "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(main, "add", ".")
	mustGit(main, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "init")
	mustGit(main, "push", "-q", "origin", "main")
	wt := filepath.Join(root, "wt")
	mustGit(main, "worktree", "add", "-q", "-b", "agent-1", wt)
	mustGit(wt, "config", "branch.agent-1.remote", "origin")
	mustGit(wt, "config", "branch.agent-1.merge", "refs/heads/agent-1")
	mustGit(wt, "push", "-q", "-u", "origin", "agent-1")

	// Make the common .git root read-only, leaving its subdirectories (objects,
	// refs, logs, worktrees) writable. This simulates only the read-only .git ROOT;
	// the real container also binds hooks/, info/, objects/info and the private
	// .git/config read-only, which this functional check does not reproduce.
	commonGit := filepath.Join(main, ".git")
	if err := os.Chmod(commonGit, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(commonGit, 0o755) })

	// A new root-level file (e.g. commondir) cannot be created in the read-only root.
	if err := os.WriteFile(filepath.Join(commonGit, "commondir"), []byte("../evil\n"), 0o644); err == nil {
		t.Error("creating a root-level .git/commondir must fail under the read-only root")
	}

	// Normal git write operations in the worktree still work.
	if err := os.WriteFile(filepath.Join(wt, "g.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(wt, "add", ".")
	mustGit(wt, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "work")
	mustGit(wt, "branch", "tmp")         // create a (loose) branch
	mustGit(wt, "fetch", "-q", "origin") // fetch
	mustGit(wt, "push", "-q")            // push (upstream pre-set)
	mustGit(wt, "pull", "-q", "--ff-only", "origin", "agent-1")
	// A trivial rebase onto the already-current upstream.
	mustGit(wt, "-c", "core.hooksPath="+os.DevNull, "rebase", "-q", "origin/agent-1")

	// Documented limitation: deleting a branch needs a packed-refs.lock in the
	// read-only .git root, so it fails cleanly (a clear error) and leaves the ref
	// in place — no corruption. git gc / pack-refs, fetch --prune and shallow
	// fetch share this limitation, as on the Apple layout.
	if out, err := git(wt, "branch", "-D", "tmp"); err == nil {
		t.Errorf("branch deletion should fail cleanly under the read-only .git root, got success:\n%s", out)
	}
	if out, err := git(wt, "rev-parse", "--verify", "-q", "refs/heads/tmp"); err != nil {
		t.Errorf("the branch ref must remain intact after the clean failure, got: %v\n%s", err, out)
	}

	// Tag deletion shares the limitation (it needs the packed-refs.lock in the
	// read-only root): it fails cleanly and the tag survives.
	mustGit(wt, "tag", "keep-tag")
	if out, err := git(wt, "tag", "-d", "keep-tag"); err == nil {
		t.Errorf("tag deletion should fail cleanly under the read-only .git root, got success:\n%s", out)
	}
	if out, err := git(wt, "rev-parse", "--verify", "-q", "refs/tags/keep-tag"); err != nil {
		t.Errorf("the tag ref must remain intact after the clean failure, got: %v\n%s", err, out)
	}

	// git gc needs to write under the read-only root (lock / gc.log), so it fails
	// cleanly and the object store stays intact (HEAD still resolves).
	if out, err := git(wt, "gc", "--quiet"); err == nil {
		t.Errorf("git gc should fail cleanly under the read-only .git root, got success:\n%s", out)
	}
	if out, err := git(wt, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		t.Errorf("HEAD must still resolve after a failed gc, got: %v\n%s", err, out)
	}

	// A shallow fetch writes .git/shallow (+ shallow.lock) in the read-only root,
	// so it fails cleanly and the full history is preserved.
	if out, err := git(wt, "fetch", "-q", "--depth=1", "origin", "agent-1"); err == nil {
		t.Errorf("a shallow fetch should fail cleanly under the read-only .git root, got success:\n%s", out)
	}
	if out, err := git(wt, "rev-list", "--count", "HEAD"); err != nil || strings.TrimSpace(out) == "1" {
		t.Errorf("history must stay complete after a failed shallow fetch, got count=%q err=%v", strings.TrimSpace(out), err)
	}

	// fetch --prune must delete a stale remote-tracking ref, which needs the
	// packed-refs.lock in the read-only root, so it fails cleanly and the stale
	// ref survives (no corruption).
	mustGit(wt, "push", "-q", "origin", "agent-1:refs/heads/throwaway")
	mustGit(wt, "fetch", "-q", "origin") // creates refs/remotes/origin/throwaway
	mustGit(bare, "update-ref", "-d", "refs/heads/throwaway")
	if out, err := git(wt, "fetch", "-q", "--prune", "origin"); err == nil {
		t.Errorf("fetch --prune of a stale ref should fail cleanly under the read-only .git root, got success:\n%s", out)
	}
	if out, err := git(wt, "rev-parse", "--verify", "-q", "refs/remotes/origin/throwaway"); err != nil {
		t.Errorf("the stale remote-tracking ref must remain intact after the clean failure, got: %v\n%s", err, out)
	}

	// Stash round-trips: refs/stash is a loose ref in the writable refs/ overlay,
	// so a stash push/pop cycle preserves the change (a cosmetic packed-refs.lock
	// warning may print, but push still exits 0). Prove the full round-trip: push
	// succeeds (mustGit), refs/stash exists, the change left the working tree, and
	// pop brings it back with its content intact.
	if err := os.WriteFile(filepath.Join(wt, "h.txt"), []byte("z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(wt, "add", "h.txt")
	mustGit(wt, "stash", "push", "-m", "wip")
	if out, err := git(wt, "rev-parse", "--verify", "-q", "refs/stash"); err != nil {
		t.Errorf("refs/stash must exist after a successful stash push, got: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(wt, "h.txt")); !os.IsNotExist(err) {
		t.Errorf("the stashed change must have left the working tree, stat err: %v", err)
	}
	mustGit(wt, "stash", "pop")
	if data, err := os.ReadFile(filepath.Join(wt, "h.txt")); err != nil || string(data) != "z\n" {
		t.Errorf("stash pop must restore the change under the read-only root, got %q err=%v", string(data), err)
	}
}

// On the Apple runtime the agent reads the repository .git/config directly (no
// redacted private copy), so a config carrying credentials is refused at start,
// with the refusal naming only the sections, never the values. A clean config
// starts normally.
func TestAppleRepoConfigWithSecrets_RefusedAtStart(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		refused bool
		section string
	}{
		{"http extraheader", "[http \"https://host.example\"]\n\textraheader = Authorization: Basic QUJD\n", true, "http"},
		{"userinfo in remote url", "[remote \"origin\"]\n\turl = https://alice:s3cret@host.example/o/r.git\n", true, "remote"},
		{"credential entry", "[credential]\n\thelper = store\n", true, "credential"},
		{"clean config", "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
			if err := os.WriteFile(filepath.Join(repoRoot, ".git", "config"), []byte(tc.config), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := buildCommonRunArgs(RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				RuntimeName:  appleContainerRuntimeName,
				RepoRoot:     repoRoot,
				Workspace:    workspace,
			})
			if tc.refused {
				if err == nil {
					t.Fatalf("expected the start to be refused for a config with %s secrets", tc.section)
				}
				_, sections, named := strings.Cut(err.Error(), "section(s) ")
				if !strings.Contains(err.Error(), "holds credentials") || !named || !strings.Contains(sections, tc.section) {
					t.Errorf("refusal should name the section %q in the section(s) list and no value, got: %v", tc.section, err)
				}
				// The refusal must not echo a secret value.
				for _, secret := range []string{"s3cret", "QUJD", "store", "alice:"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("refusal leaked a secret value %q: %v", secret, err)
					}
				}
			} else if err != nil {
				t.Errorf("a clean config must start, got: %v", err)
			}
		})
	}
}

// ensureOverlayDir is the single fail-closed inspection for overlay sources.
// With createIfMissing it creates a missing directory; without it it reports a
// missing source absent (skip, no create). A symlink or a non-directory is always
// refused, and a real directory is reported present.
func TestEnsureOverlayDir(t *testing.T) {
	dir := t.TempDir()
	// Missing + createIfMissing -> created, present. (ensureOverlayDir inspects a
	// single overlay component directly under a trusted parent, as its real callers
	// do — objects/refs/logs/lfs/rr-cache under the .git root.)
	missing := filepath.Join(dir, "overlay")
	if present, err := ensureOverlayDir(missing, true); err != nil || !present {
		t.Errorf("missing dir with createIfMissing should be created and present, got present=%v err=%v", present, err)
	}
	if fi, err := os.Stat(missing); err != nil || !fi.IsDir() {
		t.Errorf("overlay dir not created: %v", err)
	}
	// Missing + !createIfMissing -> absent, not created (rr-cache semantics).
	skip := filepath.Join(dir, "skip-overlay")
	if present, err := ensureOverlayDir(skip, false); err != nil || present {
		t.Errorf("missing dir without createIfMissing should be absent and uncreated, got present=%v err=%v", present, err)
	}
	if _, err := os.Stat(skip); !os.IsNotExist(err) {
		t.Errorf("a skip-if-missing overlay must not be created, stat err: %v", err)
	}
	// A real directory -> present.
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if present, err := ensureOverlayDir(real, false); err != nil || !present {
		t.Errorf("a real directory should be present, got present=%v err=%v", present, err)
	}
	// A symlink -> refused (both modes).
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, create := range []bool{true, false} {
		if present, err := ensureOverlayDir(link, create); err == nil {
			t.Errorf("a symlink overlay source must be refused (createIfMissing=%v), got present=%v", create, present)
		}
	}
	// A non-directory (regular file) -> refused.
	reg := filepath.Join(dir, "file")
	if err := os.WriteFile(reg, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureOverlayDir(reg, true); err == nil {
		t.Error("a non-directory overlay source must be refused")
	}
}

// rr-cache is overlaid read-write only if it already exists; it is never
// created host-side (creating it would enable rerere in the user's checkout).
func TestReadOnlyGitRoot_RrCacheOnlyIfExists(t *testing.T) {
	// Absent: no rr-cache overlay, and it is not created on the host.
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
	gitDir := filepath.Join(repoRoot, ".git")
	args, err := buildCommonRunArgs(RunConfig{
		Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
		Image: "scion-agent:latest", RuntimeName: "docker", RepoRoot: repoRoot,
		Workspace: workspace, PrivateGitConfigSource: seedTestPrivateGitConfig(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "/repo-root/.git/rr-cache") {
		t.Error("rr-cache must not be overlaid when it does not exist")
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rr-cache")); !os.IsNotExist(err) {
		t.Errorf("rr-cache must not be created host-side, stat err: %v", err)
	}

	// Present: overlaid read-write.
	repoRoot2, workspace2 := setupHubManagedBaseRepo(t, "agent-1")
	if err := os.MkdirAll(filepath.Join(repoRoot2, ".git", "rr-cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	args2, err := buildCommonRunArgs(RunConfig{
		Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
		Image: "scion-agent:latest", RuntimeName: "docker", RepoRoot: repoRoot2,
		Workspace: workspace2, PrivateGitConfigSource: seedTestPrivateGitConfig(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "-v " + filepath.Join(repoRoot2, ".git", "rr-cache") + ":/repo-root/.git/rr-cache "
	if !strings.Contains(strings.Join(args2, " "), want) {
		t.Errorf("rr-cache must be overlaid read-write when it exists, got: %s", strings.Join(args2, " "))
	}

	// A symlink at .git/rr-cache is refused (fail closed via Lstat): following it
	// would bind a directory outside the repository read-write into the agent.
	// Reverting ensureOverlayDir to os.Stat (which follows the link) would make this
	// case bind the link target instead of refusing.
	repoRoot3, workspace3 := setupHubManagedBaseRepo(t, "agent-1")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repoRoot3, ".git", "rr-cache")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err = buildCommonRunArgs(RunConfig{
		Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
		Image: "scion-agent:latest", RuntimeName: "docker", RepoRoot: repoRoot3,
		Workspace: workspace3, PrivateGitConfigSource: seedTestPrivateGitConfig(t),
	})
	if err == nil || !strings.Contains(err.Error(), "rr-cache") {
		t.Errorf("a symlinked .git/rr-cache must be refused naming rr-cache, got: %v", err)
	}
}

// readSmallConfigFile accepts a file exactly at the cap and refuses one a
// single byte over it, so the whole config is covered by the credential check.
// Reverting it to read only up to the cap (the silent-truncation form) makes the
// over-cap case return the truncated prefix with no error — this test goes red.
func TestReadSmallConfigFile_SizeCap(t *testing.T) {
	const maxConfig = 1 << 20
	dir := t.TempDir()

	atCap := filepath.Join(dir, "at-cap")
	if err := os.WriteFile(atCap, make([]byte, maxConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSmallConfigFile(atCap)
	if err != nil {
		t.Errorf("a file exactly at the cap must be accepted, got: %v", err)
	}
	if len(got) != maxConfig {
		t.Errorf("expected %d bytes read, got %d", maxConfig, len(got))
	}

	overCap := filepath.Join(dir, "over-cap")
	if err := os.WriteFile(overCap, make([]byte, maxConfig+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSmallConfigFile(overCap); err == nil {
		t.Error("a file one byte over the cap must be refused")
	}
}

// An Apple start is refused when a [credential] section sits past the size
// cap — the over-cap refusal keeps a credential beyond the cap from going
// unchecked.
func TestAppleRepoConfigWithSecrets_OverCapRefused(t *testing.T) {
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
	cfgPath := filepath.Join(repoRoot, ".git", "config")
	const maxConfig = 1 << 20
	var b []byte
	b = append(b, []byte("[core]\n\tfiller = ")...)
	b = append(b, make([]byte, maxConfig)...) // pushes the next section past the cap
	for i := range b[len("[core]\n\tfiller = "):] {
		b[len("[core]\n\tfiller = ")+i] = 'x'
	}
	b = append(b, []byte("\n[credential]\n\thelper = store\n")...)
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := buildCommonRunArgs(RunConfig{
		Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
		Image: "scion-agent:latest", RuntimeName: appleContainerRuntimeName,
		RepoRoot: repoRoot, Workspace: workspace,
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("an over-cap repository config must be refused on Apple, got: %v", err)
	}
}

// Structural: every read-write overlay source in the read-only-root layout is
// reached through the single ensureOverlayDir refusal — EXCEPT the agent's own
// worktree admin overlay, whose source is the broker-trusted util.TrustedWorktreeAdminName,
// not ensureOverlayDir — so a symlink planted at an ensureOverlayDir source is refused
// (fail closed) rather than followed. A future overlay bound without going through
// ensureOverlayDir would be caught by adding its name here.
func TestReadOnlyGitRoot_AllOverlaysRefuseSymlink(t *testing.T) {
	// objects/info is a nested read-only source (narrowObjectsInfo) in this layout;
	// the rest are the read-write overlay roots.
	for _, name := range []string{"objects", "refs", "logs", "lfs", "rr-cache", "objects/info"} {
		t.Run(name, func(t *testing.T) {
			repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(repoRoot, ".git", filepath.FromSlash(name))
			_ = os.RemoveAll(src)
			if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, src); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			_, err := buildCommonRunArgs(RunConfig{
				Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
				Image: "scion-agent:latest", RuntimeName: "docker", RepoRoot: repoRoot,
				Workspace: workspace, PrivateGitConfigSource: seedTestPrivateGitConfig(t),
			})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("a symlinked .git/%s overlay must be refused naming %s, got: %v", name, name, err)
			}
		})
	}
}

// setupOutsideRootWorktree creates a real git repo and a registered worktree
// OUTSIDE the repo root, returning (repoRoot, workspace). It drives the
// outside-root fallback layout in buildCommonRunArgs.
func setupOutsideRootWorktree(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(base, "gc"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	if err := os.WriteFile(filepath.Join(base, "gc"), []byte("[user]\n\temail=a@b.c\n\tname=A\n[init]\n\tdefaultBranch=main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Join(base, "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(base, "init", "-q", "-b", "main", repoRoot)
	if err := os.WriteFile(filepath.Join(repoRoot, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repoRoot, "add", ".")
	run(repoRoot, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "init")
	workspace := filepath.Join(base, "wt") // outside repoRoot
	run(repoRoot, "worktree", "add", "-q", "-b", "agent-1", workspace)
	return repoRoot, workspace
}

// The outside-root fallback layout mounts the .git root read-only with the
// bounded read-write overlay set (docker/podman), and no root-level entry can be
// created, mirroring the in-repo worktree layout.
func TestReadOnlyGitRoot_OutsideRootLayout(t *testing.T) {
	for _, rt := range []string{"docker", "podman"} {
		t.Run(rt, func(t *testing.T) {
			repoRoot, workspace := setupOutsideRootWorktree(t)
			gitDir := filepath.Join(repoRoot, ".git")
			// Add a SIBLING worktree so there is a second worktrees/<name> admin
			// dir that MUST NOT be read-write bound: only the agent's OWN overlay
			// may be. A fallback that passed a sibling or empty admin name would
			// bind the wrong (or no) overlay, which the exact-own + sibling-negative
			// assertions below catch.
			sibling := filepath.Join(filepath.Dir(workspace), "sibling-wt")
			if out, err := exec.Command("git", "-C", repoRoot, "worktree", "add", "-q", "-b", "sibling", sibling).CombinedOutput(); err != nil {
				t.Fatalf("git worktree add sibling: %v\n%s", err, out)
			}
			ownName, ok := util.TrustedWorktreeAdminName(gitDir, workspace)
			if !ok {
				t.Fatal("fixture: cannot resolve the own worktree admin name")
			}
			ownTarget := "/repo-root/.git/worktrees/" + ownName
			args, err := buildCommonRunArgs(RunConfig{
				Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
				Image: "scion-agent:latest", RuntimeName: rt, RepoRoot: repoRoot,
				Workspace: workspace, PrivateGitConfigSource: seedTestPrivateGitConfig(t),
			})
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			argStr := strings.Join(args, " ")
			if !strings.Contains(argStr, "-v "+gitDir+":/repo-root/.git:ro") {
				t.Fatalf("expected a read-only .git root in the outside-root layout, got: %s", argStr)
			}
			if strings.Contains(argStr, "-v "+gitDir+":/repo-root/.git ") {
				t.Fatalf("the .git root must not be read-write")
			}
			// Only the bounded overlay set is read-write under /repo-root/.git, and
			// the ONLY read-write worktrees/* overlay is the agent's own.
			allowed := map[string]bool{
				"/repo-root/.git/objects": true, "/repo-root/.git/refs": true,
				"/repo-root/.git/logs": true, "/repo-root/.git/lfs": true,
			}
			sawOwnOverlay := false
			for i, a := range args {
				if a != "-v" || i+1 >= len(args) {
					continue
				}
				spec := args[i+1]
				j := strings.Index(spec, ":/repo-root/.git")
				if j < 0 || strings.HasSuffix(spec, ":ro") {
					continue
				}
				target := spec[j+1:]
				switch {
				case target == "/repo-root/.git":
					t.Errorf(".git root must not be read-write: %s", spec)
				case target == ownTarget:
					sawOwnOverlay = true
				case strings.HasPrefix(target, "/repo-root/.git/worktrees/"):
					// A worktrees/* overlay other than the agent's own (e.g. the
					// sibling, or an empty/sibling admin name) is forbidden.
					t.Errorf("read-write bind of a non-own worktree admin dir: %s (own is %s)", spec, ownTarget)
				case !allowed[target]:
					t.Errorf("unexpected read-write bind under /repo-root/.git: %s", spec)
				}
			}
			if !sawOwnOverlay {
				t.Errorf("the agent's own worktree overlay %s is not mounted read-write; args: %s", ownTarget, argStr)
			}
		})
	}
}

// On the outside-root path, an Apple agent on the fallback path is
// refused when the repository config holds credentials, and starts on a clean one.
func TestAppleRepoConfigWithSecrets_OutsideRoot(t *testing.T) {
	repoRoot, workspace := setupOutsideRootWorktree(t)
	cfgPath := filepath.Join(repoRoot, ".git", "config")
	base, _ := os.ReadFile(cfgPath)
	// Clean config (the real one) starts.
	if _, err := buildCommonRunArgs(RunConfig{
		Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
		Image: "scion-agent:latest", RuntimeName: appleContainerRuntimeName,
		RepoRoot: repoRoot, Workspace: workspace,
	}); err != nil {
		t.Fatalf("clean config must start on Apple outside-root, got: %v", err)
	}
	// Add a credential -> refused, naming the section.
	if err := os.WriteFile(cfgPath, append(base, []byte("[credential]\n\thelper = store\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := buildCommonRunArgs(RunConfig{
		Harness: &harness.Generic{}, Name: "test-agent", UnixUsername: "scion",
		Image: "scion-agent:latest", RuntimeName: appleContainerRuntimeName,
		RepoRoot: repoRoot, Workspace: workspace,
	})
	if err == nil || !strings.Contains(err.Error(), "holds credentials") {
		t.Errorf("Apple outside-root must refuse a credential-bearing config, got: %v", err)
	} else if _, sections, named := strings.Cut(err.Error(), "section(s) "); !named || !strings.Contains(sections, "credential") {
		t.Errorf("refusal must name the credential section in the section(s) list, got: %v", err)
	}
}
