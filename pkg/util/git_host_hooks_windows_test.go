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

//go:build windows

package util

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestHostGitRunsNoHooksWindows checks that host-side git runs no repository
// hook on Windows. HostGitArgs sets core.hooksPath to os.DevNull, which is
// "NUL" here; Git for Windows must treat that as an empty hooks directory.
//
// A positive control first proves that a hook in .git/hooks does run on this
// machine for plain git, so the final "no marker" check cannot pass just
// because hooks never run here. Any failed precondition fails the test.
func TestHostGitRunsNoHooksWindows(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not on PATH: %v", err)
	}

	// Keep the host user's global and system config (which could set
	// core.hooksPath itself) out of the test.
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	plainGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}

	repo := filepath.Join(t.TempDir(), "repo")
	plainGit(filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	plainGit(repo, "config", "user.name", "Test User")
	plainGit(repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plainGit(repo, "add", "file.txt")
	plainGit(repo, "commit", "-q", "-m", "initial")

	// The hook is a sh script; Git for Windows runs it with its bundled sh.
	// The marker path uses forward slashes so sh reads it as one path.
	marker := filepath.Join(t.TempDir(), "post-checkout-ran")
	hook := "#!/bin/sh\necho ran > '" + filepath.ToSlash(marker) + "'\n"
	hookDir := filepath.Join(repo, ".git", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	markerExists := func() bool {
		t.Helper()
		_, err := os.Stat(marker)
		if err == nil {
			return true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s: %v", marker, err)
		}
		return false
	}

	// Positive control: plain git, without the host-git settings, runs the
	// hook.
	plainGit(repo, "checkout", "-q", "-b", "ctl")
	if !markerExists() {
		t.Fatalf("positive control failed: plain git checkout did not run the post-checkout hook in %s, so this machine cannot show whether host-side git skips hooks", hookDir)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	// Subject: a work-tree-writing command through HostGitCommand, as
	// production runs checkout.
	checkout, err := HostGitCommand(t.Context(), repo, HostGitOptions{}, "checkout", "-q", "-b", "subject")
	if err != nil {
		t.Fatalf("HostGitCommand(checkout): %v", err)
	}
	if out, err := checkout.CombinedOutput(); err != nil {
		t.Fatalf("host-side git checkout -b subject: %v\n%s", err, out)
	}

	// A read-only command with HostGitArgs and HostGitEnv, as production runs
	// one-off queries. ls-files runs no hook, so this is not a second hook
	// check; it shows that core.hooksPath=NUL does not break such a command.
	lsFiles := exec.CommandContext(t.Context(), "git", append(HostGitArgs(), "-C", repo, "ls-files")...)
	lsFiles.Env = HostGitEnv(os.Environ())
	out, err := lsFiles.CombinedOutput()
	if err != nil {
		t.Fatalf("host-side git ls-files: %v\n%s", err, out)
	}
	if got := string(out); got != "file.txt\n" {
		t.Errorf("host-side git ls-files = %q, want %q", got, "file.txt\n")
	}

	if markerExists() {
		t.Errorf("host-side git ran the repository's post-checkout hook despite core.hooksPath=%s (HostGitArgs: %q)", os.DevNull, HostGitArgs())
	}
}
