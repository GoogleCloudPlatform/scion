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

// These tests exercise the unix private git config seed (readBaseGitConfig
// uses O_NOFOLLOW); on other platforms it is an ErrUnsupported stub.
//go:build unix

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
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

func TestSeedPrivateGitConfig_MissingBaseSeedsDefaultsOnly(t *testing.T) {
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

// Under the agent's real read-only .git root, the pre-seed keeps normal pushes
// working:
//   - `git push -u origin <branch>` (run first, remote empty) exits 0 and reaches
//     the remote — it creates the branch there, pinned by comparing the remote ref
//     to local HEAD — and cannot persist the upstream under the read-only .git (git
//     reports an error locking the config), but the push still succeeds.
//   - a BARE `git push` then exits 0 because the upstream is pre-seeded; without the
//     seed a bare push exits 128, "no upstream branch" — this is what the seed buys.
//
// The repo config is left unchanged because the write is blocked, not skipped.
func TestSeedPrivateGitConfig_PushUnderReadOnlyGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the read-only .git bits, so this test cannot exercise the read-only path")
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

	// `git push -u origin <branch>` under the read-only .git exits 0 and REACHES the
	// remote (it creates the branch there), even though it cannot persist the upstream
	// config. Run it FIRST, while the remote has no agent-x, so its remote effect is
	// pinned (a new commit cannot be made under the read-only root to re-test it later,
	// and a push -u against an already-present branch would only say "up-to-date").
	lh, lherr := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if lherr != nil {
		t.Fatalf("rev-parse HEAD: %v", lherr)
	}
	localHead := strings.TrimSpace(string(lh))
	pushU := exec.Command("git", "push", "-u", "origin", "agent-x")
	pushU.Dir = repo
	if out, err := pushU.CombinedOutput(); err != nil {
		t.Fatalf("git push -u should exit 0 under the read-only .git, got: %v\n%s", err, out)
	}
	rh, rherr := exec.Command("git", "-C", bare, "rev-parse", "--verify", "refs/heads/agent-x").CombinedOutput()
	if rherr != nil {
		t.Fatalf("git push -u did not create the branch on the remote: %v\n%s", rherr, rh)
	}
	if remoteHead := strings.TrimSpace(string(rh)); remoteHead != localHead {
		t.Errorf("git push -u did not reach the remote: remote agent-x = %q, want local HEAD %q", remoteHead, localHead)
	}
	// The seed's effect: a BARE `git push` (no args) now exits 0 because the upstream
	// is pre-seeded. Without the seed a bare push fails with "no upstream branch"
	// (exit 128) even when the remote is up to date — so this exercises the seed.
	barePush := exec.Command("git", "push")
	barePush.Dir = repo
	if out, err := barePush.CombinedOutput(); err != nil {
		t.Fatalf("bare `git push` should exit 0 under the read-only .git with the upstream pre-seeded, got: %v\n%s", err, out)
	}
	// The config was not persisted (the read-only root blocked the write).
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

// TestSeedPrivateGitConfig_CloseErrorFailsClosed injects a close error on the
// written temp config and confirms the write path fails closed: an error is
// returned, no config file is created, and the temp file is removed.
func TestSeedPrivateGitConfig_CloseErrorFailsClosed(t *testing.T) {
	agentDir := t.TempDir()
	orig := tmpConfigClose
	tmpConfigClose = func(f *os.File) error {
		_ = f.Close() // close the real fd so it does not leak, then report a failure
		return fmt.Errorf("injected close error")
	}
	t.Cleanup(func() { tmpConfigClose = orig })

	dest, err := seedPrivateGitConfig(filepath.Join(t.TempDir(), "nobase"), agentDir, "agent-x")
	if err == nil {
		t.Fatalf("expected a close error, got nil (dest=%q)", dest)
	}
	if !strings.Contains(err.Error(), "close") {
		t.Errorf("error = %v, want it to mention the close failure", err)
	}
	// Fail closed: the config file is never created, and the temp file is removed.
	cfg := filepath.Join(agentDir, "git", "config")
	if _, statErr := os.Stat(cfg); !os.IsNotExist(statErr) {
		t.Errorf("a config file was left behind on a close failure: os.Stat(%s) = %v", cfg, statErr)
	}
	entries, rderr := os.ReadDir(filepath.Join(agentDir, "git"))
	if rderr != nil {
		t.Fatalf("read git dir: %v", rderr)
	}
	for _, e := range entries {
		t.Errorf("leftover file after a close failure (temp not removed): %s", e.Name())
	}
}

// A base that sets a command-running key refuses the seed: the error names the
// key, never the value, and no copy is written.
func TestSeedPrivateGitConfig_RefusesCommandKeys(t *testing.T) {
	for _, tc := range []struct{ key, line string }{
		{"core.sshcommand", "sshCommand = sshpass -p seedmarker-h1 ssh"},
		{"core.hookspath", "hooksPath = /tmp/seedmarker-h1"},
		{"core.fsmonitor", "fsmonitor = /tmp/seedmarker-h1"},
		{"core.pager", "pager = /tmp/seedmarker-h1"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			agentDir := t.TempDir()
			base := filepath.Join(t.TempDir(), "config")
			body := "[core]\n\tfilemode = true\n\t" + tc.line + "\n"
			if err := os.WriteFile(base, []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
			dest, err := seedPrivateGitConfig(base, agentDir, "agent-x")
			if err == nil {
				t.Fatalf("seed accepted a base setting %s (dest %q)", tc.key, dest)
			}
			var refused *util.RepoConfigRefusedError
			if !errors.As(err, &refused) || refused.Key != tc.key {
				t.Fatalf("error = %v, want a refusal naming %s", err, tc.key)
			}
			if msg := err.Error(); !strings.Contains(msg, tc.key) || strings.Contains(msg, "seedmarker-h1") {
				t.Errorf("error must name the key and not the value: %s", msg)
			}
			if !strings.Contains(err.Error(), "remove it from the repository's .git/config") || strings.Contains(err.Error(), "global") {
				t.Errorf("error lacks the repository-config remedy: %s", err)
			}
			if _, statErr := os.Lstat(filepath.Join(agentDir, "git")); !os.IsNotExist(statErr) {
				t.Errorf("a refused seed created the git dir (stat err %v)", statErr)
			}
		})
	}
}

// A base written by earlier Scion versions still seeds, and the seed branch's
// stale upstream is replaced by the broker's own.
func TestSeedPrivateGitConfig_EarlierBaseSeeds(t *testing.T) {
	agentDir := t.TempDir()
	base := filepath.Join(t.TempDir(), "config")
	body := strings.Join([]string{
		"[core]",
		"\trepositoryformatversion = 0",
		"\tfilemode = true",
		"\tbare = false",
		"\tlogallrefupdates = true",
		`[remote "origin"]`,
		"\turl = https://github.com/o/r.git",
		"\tfetch = +refs/heads/*:refs/remotes/origin/*",
		`[branch "main"]`,
		"\tremote = origin",
		"\tmerge = refs/heads/main",
		`[branch "agent-x"]`,
		"\tremote = origin",
		"\tmerge = refs/heads/stale",
		"[push]",
		"\tautoSetupRemote = true",
		"[gc]",
		"\tauto = 0",
		"[maintenance]",
		"\tauto = false",
		"[credential]",
		"\thelper = store",
		"",
	}, "\n")
	if err := os.WriteFile(base, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	dest, err := seedPrivateGitConfig(base, agentDir, "agent-x")
	if err != nil {
		t.Fatalf("earlier base refused: %v", err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"https://github.com/o/r.git", `[branch "main"]`, "refs/heads/main", "merge = refs/heads/agent-x"} {
		if !strings.Contains(s, want) {
			t.Errorf("seeded config lacks %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"refs/heads/stale", "credential", "store"} {
		if strings.Contains(s, bad) {
			t.Errorf("seeded config keeps %q:\n%s", bad, s)
		}
	}
	if n := strings.Count(s, `[branch "agent-x"]`); n != 1 {
		t.Errorf("want one agent-x branch section, got %d:\n%s", n, s)
	}
}

// The bytes written are exactly util.PrepareSeededGitConfig's output (the
// serialization of the checked, filtered parse) followed by the broker's own
// upstream pre-seed: nothing from the raw base is written directly.
func TestSeedPrivateGitConfig_WritesPreparedBytes(t *testing.T) {
	agentDir := t.TempDir()
	base := filepath.Join(t.TempDir(), "config")
	raw := []byte("# comment kept out of the copy\n[remote \"or\\\"ig\"]\n\turl = https://u:pw-h2@h.example/r.git\n[branch \"agent-x\"]\n\tremote = stale\n")
	if err := os.WriteFile(base, raw, 0644); err != nil {
		t.Fatal(err)
	}
	dest, err := seedPrivateGitConfig(base, agentDir, "agent-x")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	prepared, _, err := util.PrepareSeededGitConfig(raw, util.RepoPolicyLinked, staleUpstreamKey("agent-x"))
	if err != nil {
		t.Fatal(err)
	}
	want := string(prepared) + string(privateConfigUpstreamSeed("agent-x"))
	if string(got) != want {
		t.Errorf("written bytes differ from the prepared copy:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(string(got), "pw-h2") || strings.Contains(string(got), "# comment") || strings.Contains(string(got), "stale") {
		t.Errorf("raw base content reached the copy:\n%s", got)
	}
}

// A base that does not parse fails closed: an error, and no copy or git dir.
func TestSeedPrivateGitConfig_ParseErrorFailsClosed(t *testing.T) {
	agentDir := t.TempDir()
	base := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(base, []byte("[core\n\tbare = false\n"), 0644); err != nil {
		t.Fatal(err)
	}
	dest, err := seedPrivateGitConfig(base, agentDir, "agent-x")
	if err == nil {
		t.Fatalf("seed accepted a malformed base (dest %q)", dest)
	}
	var refused *util.RepoConfigRefusedError
	if errors.As(err, &refused) {
		t.Errorf("parse error reported as a policy refusal: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(agentDir, "git")); !os.IsNotExist(statErr) {
		t.Errorf("a failed seed created the git dir (stat err %v)", statErr)
	}
}

// seedCheckedPrivateGitConfig checks the config.worktree layers before seeding:
// a key planted in the agent's own config.worktree refuses the start, naming
// the layer, and writes nothing; a clean repository seeds.
func TestSeedCheckedPrivateGitConfig_ChecksWorktreeLayers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "SCION_HOST_UID=",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "f")
	run(repo, "commit", "-q", "-m", "init")
	ws := filepath.Join(root, "ws")
	run(repo, "worktree", "add", "-q", "-b", "agent-x", ws)

	agentDir := t.TempDir()
	dest, err := seedCheckedPrivateGitConfig(context.Background(), repo, ws, agentDir, "agent-x")
	if err != nil {
		t.Fatalf("clean repository refused: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("clean seed wrote no copy: %v", err)
	}

	admin := filepath.Join(repo, ".git", "worktrees", filepath.Base(ws), "config.worktree")
	if err := os.WriteFile(admin, []byte("[core]\n\tfsmonitor = /tmp/ownlayermarker-r1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir2 := t.TempDir()
	_, err = seedCheckedPrivateGitConfig(context.Background(), repo, ws, agentDir2, "agent-x")
	var refused *util.RepoConfigRefusedError
	if !errors.As(err, &refused) || refused.Key != "core.fsmonitor" || refused.Layer == "" {
		t.Fatalf("error = %v, want a refusal of core.fsmonitor in a layer", err)
	}
	if strings.Contains(err.Error(), "ownlayermarker-r1") {
		t.Errorf("error leaks the value: %s", err)
	}
	if _, statErr := os.Lstat(filepath.Join(agentDir2, "git")); !os.IsNotExist(statErr) {
		t.Errorf("a refused seed created the git dir (stat err %v)", statErr)
	}
}

// A linked-worktree workspace whose broker-recorded admin dir does not resolve
// is refused before anything is written: a back-link that names another path,
// and a symlinked .git gitfile.
func TestSeedCheckedPrivateGitConfig_UnresolvedWorktreeRefuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, tc := range []string{"stale back-link", "symlinked gitfile"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			run := func(dir string, args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "SCION_HOST_UID=",
					"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			run(root, "init", "-q", "-b", "main", repo)
			if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(repo, "add", "f")
			run(repo, "commit", "-q", "-m", "init")
			ws := filepath.Join(root, "ws")
			run(repo, "worktree", "add", "-q", "-b", "agent-x", ws)
			switch tc {
			case "stale back-link":
				back := filepath.Join(repo, ".git", "worktrees", filepath.Base(ws), "gitdir")
				if err := os.WriteFile(back, []byte(filepath.Join(root, "elsewhere", ".git")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlinked gitfile":
				real := filepath.Join(root, "real-gitfile")
				if err := os.Rename(filepath.Join(ws, ".git"), real); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, filepath.Join(ws, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			agentDir := t.TempDir()
			dest, err := seedCheckedPrivateGitConfig(context.Background(), repo, ws, agentDir, "agent-x")
			if err == nil {
				t.Fatalf("unresolved worktree admin dir accepted (dest %q)", dest)
			}
			if !strings.Contains(err.Error(), "cannot resolve the broker-recorded worktree admin directory") {
				t.Errorf("error does not mirror the runtime refusal: %v", err)
			}
			if _, statErr := os.Lstat(filepath.Join(agentDir, "git")); !os.IsNotExist(statErr) {
				t.Errorf("a refused seed created the git dir (stat err %v)", statErr)
			}
		})
	}
}

// gitRepoWithCommit makes a repository with one commit under root and returns
// its path.
func gitRepoWithCommit(t *testing.T, root string) string {
	t.Helper()
	repo := filepath.Join(root, "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "SCION_HOST_UID=",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "f")
	run(repo, "commit", "-q", "-m", "init")
	return repo
}

// A repository whose .git is a symlink (or a gitfile) is refused at the seed,
// before anything is written.
func TestCheckSeedSubmodules_SymlinkedDotGitRefuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, tc := range []string{"symlink", "gitfile"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			repo := gitRepoWithCommit(t, root)
			real := filepath.Join(root, "real-dot-git")
			if err := os.Rename(filepath.Join(repo, ".git"), real); err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "symlink":
				if err := os.Symlink(real, filepath.Join(repo, ".git")); err != nil {
					t.Fatal(err)
				}
			case "gitfile":
				if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: "+real+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			agentDir := t.TempDir()
			_, err := seedCheckedPrivateGitConfig(context.Background(), repo, repo, agentDir, "agent-x")
			if err == nil || !strings.Contains(err.Error(), "must be a real directory") {
				t.Fatalf("error = %v, want a refusal of the %s .git", err, tc)
			}
			if _, statErr := os.Lstat(filepath.Join(agentDir, "git")); !os.IsNotExist(statErr) {
				t.Errorf("a refused seed created the git dir (stat err %v)", statErr)
			}
		})
	}
}

// When the worktree admin name does not resolve, an Lstat of <ws>/.git that
// fails with anything but "does not exist" refuses (here: no search
// permission on the workspace).
func TestSeedCheckedPrivateGitConfig_UncheckableWorkspaceGitRefuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the Lstat cannot fail here")
	}
	root := t.TempDir()
	repo := gitRepoWithCommit(t, root)
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ws, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })
	agentDir := t.TempDir()
	_, err := seedCheckedPrivateGitConfig(context.Background(), repo, ws, agentDir, "agent-x")
	if err == nil || !strings.Contains(err.Error(), "cannot resolve the broker-recorded worktree admin directory") {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if _, statErr := os.Lstat(filepath.Join(agentDir, "git")); !os.IsNotExist(statErr) {
		t.Errorf("a refused seed created the git dir (stat err %v)", statErr)
	}
}
