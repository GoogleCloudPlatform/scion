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
	"strings"
	"testing"
)

func TestSeedPrivateGitConfig_RedactsAndWritesRegularFile(t *testing.T) {
	agentDir := t.TempDir()
	base := filepath.Join(t.TempDir(), "config")
	body := strings.Join([]string{
		"[core]",
		"\tfilemode = true",
		"[remote \"origin\"]",
		"\turl = https://alice:secret@host.example/o/r.git",
		"[credential]",
		"\thelper = store",
		"",
	}, "\n")
	if err := os.WriteFile(base, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	dest, err := seedPrivateGitConfig(base, agentDir, "agent-x")
	if err != nil {
		t.Fatalf("seedPrivateGitConfig: %v", err)
	}
	if want := filepath.Join(agentDir, "git", "config"); dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}

	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		t.Errorf("dest must be a regular file, got mode %v", fi.Mode())
	}
	out, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"secret", "store", "alice:"} {
		if strings.Contains(s, bad) {
			t.Errorf("seeded config leaks %q:\n%s", bad, s)
		}
	}
	if !strings.Contains(s, "https://host.example/o/r.git") {
		t.Errorf("seeded config lost the (userinfo-stripped) origin url:\n%s", s)
	}
}

// Re-seeding replaces any stale file, including a symlink an earlier run or the
// agent could have left in the seed location: the result is always a fresh,
// broker-written regular file.
func TestSeedPrivateGitConfig_ReplacesSymlink(t *testing.T) {
	agentDir := t.TempDir()
	base := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(base, []byte("[core]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Put a symlink where the seed will be written.
	gitDir := filepath.Join(agentDir, "git")
	if err := os.MkdirAll(gitDir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(gitDir, "config")); err != nil {
		t.Fatal(err)
	}

	dest, err := seedPrivateGitConfig(base, agentDir, "agent-x")
	if err != nil {
		t.Fatalf("seedPrivateGitConfig: %v", err)
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("seed must replace a pre-existing symlink with a regular file")
	}
	// The pointed-to file is untouched (rename replaced the link, did not follow it).
	if b, _ := os.ReadFile(outside); string(b) != "original" {
		t.Error("seeding followed the symlink and overwrote its target")
	}
}

func TestSeedPrivateGitConfig_MissingBaseSeedsUpstreamOnly(t *testing.T) {
	agentDir := t.TempDir()
	dest, err := seedPrivateGitConfig(filepath.Join(t.TempDir(), "nope"), agentDir, "agent-x")
	if err != nil {
		t.Fatalf("seedPrivateGitConfig with a missing base should succeed, got: %v", err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	// No base content, but the upstream + autoSetupRemote pre-seed is always present.
	s := string(b)
	if !strings.Contains(s, "autoSetupRemote = true") || !strings.Contains(s, `[branch "agent-x"]`) {
		t.Errorf("missing base should still seed the upstream pre-seed, got %q", s)
	}
	if strings.Contains(s, "[core]") || strings.Contains(s, "[remote") {
		t.Errorf("missing base must not invent base config, got %q", s)
	}
	// Automatic gc and maintenance are disabled so routine commands do not try to
	// write lock files in the read-only .git root (which would only print warnings).
	// Read the values through git so the section names (gc.auto / maintenance.auto),
	// not just the bare values, are checked.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := func(key string) string {
		out, _ := exec.Command("git", "config", "--file", dest, "--get", key).Output()
		return strings.TrimSpace(string(out))
	}
	if got := cfg("gc.auto"); got != "0" {
		t.Errorf("seed must set gc.auto=0, got %q", got)
	}
	if got := cfg("maintenance.auto"); got != "false" {
		t.Errorf("seed must set maintenance.auto=false, got %q", got)
	}
}

// Under the agent's real read-only .git root, `git push -u` for the agent's own
// branch exits 0 and the push reaches the remote, even though git cannot persist
// the upstream (it logs "could not lock config file" / "unable to write upstream
// branch configuration" and continues). This is the mitigation: the upstream is
// pre-seeded, so the push targets the right remote/branch and succeeds without
// needing a writable config. The config is left unchanged because the write is
// blocked, not because git skips it.
func TestSeedPrivateGitConfig_PushUExitsZeroUnderReadOnlyGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gc"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("SCION_HOST_UID", "")
	if err := os.WriteFile(filepath.Join(root, "gc"), []byte("[user]\n\temail=a@b.c\n\tname=A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// A base repo whose .git/config has an origin; seed the private copy for
	// branch agent-x, then install it as the working repo's .git/config.
	bare := filepath.Join(root, "bare.git")
	run(root, "init", "-q", "--bare", bare)
	repo := filepath.Join(root, "repo")
	run(root, "init", "-q", "-b", "agent-x", repo)
	run(repo, "remote", "add", "origin", bare)
	agentDir := t.TempDir()
	dest, err := seedPrivateGitConfig(filepath.Join(repo, ".git", "config"), agentDir, "agent-x")
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seeded), "autoSetupRemote") || !strings.Contains(string(seeded), `[branch "agent-x"]`) {
		t.Fatalf("seed missing upstream/autoSetupRemote:\n%s", seeded)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), seeded, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", ".")
	run(repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "x")

	before, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
	// Simulate the agent's layout: the .git ROOT is read-only (so config.lock
	// cannot be created and the config cannot be rewritten), while refs/objects/
	// logs stay writable (they are overlaid read-write in the real container).
	gitRoot := filepath.Join(repo, ".git")
	if err := os.Chmod(gitRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitRoot, 0o755) })
	cmd := exec.Command("git", "push", "-u", "origin", "agent-x")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	// push -u exits 0 even though the upstream write is blocked by the read-only root.
	if err != nil {
		t.Fatalf("git push -u should exit 0 under the read-only .git with the upstream pre-seeded, got: %v\n%s", err, out)
	}
	// The push actually reached the remote (the mitigation keeps push working).
	ls := exec.Command("git", "-C", bare, "rev-parse", "--verify", "refs/heads/agent-x")
	if lsout, lserr := ls.CombinedOutput(); lserr != nil {
		t.Fatalf("push -u did not create the branch on the remote: %v\n%s", lserr, lsout)
	}
	// The config was not persisted (the read-only root blocked the write); this is
	// the documented limitation, not "git skipped the write".
	after, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if string(before) != string(after) {
		t.Errorf("read-only .git config changed unexpectedly:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A base .git/config that already carries branch.<b>.remote/merge (the normal
// pre-upgrade state after an agent or the host user ran `git push -u`) must not
// produce duplicate upstream entries in the private copy: duplicates make bare
// `git push` fail with "multiple upstream branches". Both bare `git push` and
// `git push -u` must exit 0, and the config must be unchanged by the push.
func TestSeedPrivateGitConfig_DedupesExistingUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, base := range []string{"same value", "different value"} {
		t.Run(base, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gc"))
			t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
			t.Setenv("SCION_HOST_UID", "")
			if err := os.WriteFile(filepath.Join(root, "gc"), []byte("[user]\n\temail=a@b.c\n\tname=A\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			run := func(dir string, args ...string) {
				t.Helper()
				c := exec.Command("git", args...)
				c.Dir = dir
				if out, err := c.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			bare := filepath.Join(root, "bare.git")
			run(root, "init", "-q", "--bare", bare)
			repo := filepath.Join(root, "repo")
			run(root, "init", "-q", "-b", "agent-x", repo)
			run(repo, "remote", "add", "origin", bare)
			// Pre-existing upstream for agent-x in the base config.
			mergeVal := "refs/heads/agent-x"
			if base == "different value" {
				mergeVal = "refs/heads/stale"
			}
			run(repo, "config", "branch.agent-x.remote", "origin")
			run(repo, "config", "branch.agent-x.merge", mergeVal)
			run(repo, "config", "push.autoSetupRemote", "true")

			agentDir := t.TempDir()
			dest, err := seedPrivateGitConfig(filepath.Join(repo, ".git", "config"), agentDir, "agent-x")
			if err != nil {
				t.Fatal(err)
			}
			seeded, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			// Exactly one branch.agent-x.merge entry survives.
			if n := strings.Count(string(seeded), "merge = "); n != 1 {
				t.Fatalf("expected exactly one merge entry after dedupe, got %d:\n%s", n, seeded)
			}
			if err := os.WriteFile(filepath.Join(repo, ".git", "config"), seeded, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(repo, "add", ".")
			run(repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "x")

			before, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
			// Bare `git push` must succeed (no "multiple upstream branches").
			bareCmd := exec.Command("git", "push")
			bareCmd.Dir = repo
			if out, err := bareCmd.CombinedOutput(); err != nil {
				t.Fatalf("bare git push should exit 0 after dedupe, got: %v\n%s", err, out)
			}
			// And `git push -u` too.
			uCmd := exec.Command("git", "push", "-u", "origin", "agent-x")
			uCmd.Dir = repo
			if out, err := uCmd.CombinedOutput(); err != nil {
				t.Fatalf("git push -u should exit 0, got: %v\n%s", err, out)
			}
			after, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
			if string(before) != string(after) {
				t.Errorf("push modified the read-only-intended config:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// A branch name containing a double quote (legal in a git refname) must be
// escaped in the [branch "..."] header and the merge value, so the seeded config
// still parses and git can read it.
func TestSeedPrivateGitConfig_EscapesBranchSubsection(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	agentDir := t.TempDir()
	dest, err := seedPrivateGitConfig(filepath.Join(t.TempDir(), "nobase"), agentDir, `od"d`)
	if err != nil {
		t.Fatal(err)
	}
	// git must parse the seeded config without error (an unescaped quote would
	// break the [branch "..."] header and every key after it).
	c := exec.Command("git", "config", "--file", dest, "--list")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git could not parse the seeded config with a quoted branch name: %v\n%s", err, out)
	}
	for _, want := range []string{`branch.od"d.remote=origin`, `branch.od"d.merge=refs/heads/od"d`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("expected %q to read back, got:\n%s", want, out)
		}
	}
}

// A base .git/config that is a symlink must be refused (O_NOFOLLOW), so the
// broker's read of the base cannot be redirected elsewhere by a symlink an agent
// (or anything) left at the base config path.
func TestSeedPrivateGitConfig_RefusesSymlinkedBase(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "outside")
	if err := os.WriteFile(target, []byte("[core]\n\tfilemode = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "config")
	if err := os.Symlink(target, base); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := seedPrivateGitConfig(base, t.TempDir(), "agent-x"); err == nil {
		t.Error("a symlinked base .git/config must be refused, got nil")
	}
}
