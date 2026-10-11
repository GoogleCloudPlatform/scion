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

package util

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolateGitEnv keeps the developer's git configuration (including any
// safe.directory entries) out of the test.
func isolateGitEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	return home
}

func initTestRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

func TestCheckGitRepoDir_OwnershipRefusal(t *testing.T) {
	home := isolateGitEnv(t)
	repo := filepath.Join(home, "repo")
	initTestRepo(t, repo)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	ok, err := CheckGitRepoDir(repo)
	if ok {
		t.Fatal("expected false for a repository git refuses")
	}
	if !errors.Is(err, ErrGitDubiousOwnership) {
		t.Fatalf("expected ErrGitDubiousOwnership, got %v", err)
	}
	var oe *GitOwnershipError
	if !errors.As(err, &oe) {
		t.Fatalf("expected *GitOwnershipError, got %T", err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	got, _ := filepath.EvalSymlinks(oe.SuggestedPath)
	if got != want {
		t.Errorf("SuggestedPath = %q, want %q", oe.SuggestedPath, repo)
	}
	if !strings.Contains(err.Error(), "git config --global --add safe.directory "+oe.SuggestedPath) {
		t.Errorf("error does not name the safe.directory fix: %v", err)
	}

	// The bool-only helper keeps its existing contract.
	if IsGitRepoDir(repo) {
		t.Error("IsGitRepoDir should still return false for a refused repository")
	}
}

func TestCheckGitRepoDir_PlainDir(t *testing.T) {
	home := isolateGitEnv(t)
	ok, err := CheckGitRepoDir(home)
	if ok || err != nil {
		t.Fatalf("plain dir: got (%v, %v), want (false, nil)", ok, err)
	}
}

func TestCheckGitRepoDir_TrustedRepo(t *testing.T) {
	home := isolateGitEnv(t)
	repo := filepath.Join(home, "repo")
	initTestRepo(t, repo)
	ok, err := CheckGitRepoDir(repo)
	if !ok || err != nil {
		t.Fatalf("trusted repo: got (%v, %v), want (true, nil)", ok, err)
	}
}

func TestParseSafeDirectorySuggestion(t *testing.T) {
	tests := []struct {
		name, stderr, want string
	}{
		{
			name: "plain",
			stderr: "fatal: detected dubious ownership in repository at '/src/r'\n" +
				"To add an exception for this directory, call:\n\n" +
				"\tgit config --global --add safe.directory /src/r\n",
			want: "/src/r",
		},
		{
			name:   "quoted",
			stderr: "\tgit config --global --add safe.directory '/src/my repo'\n",
			want:   "/src/my repo",
		},
		{
			name:   "missing",
			stderr: "fatal: detected dubious ownership in repository at '/src/r'\n",
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseSafeDirectorySuggestion(tt.stderr); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitOwnershipError_FallbackMessage(t *testing.T) {
	err := &GitOwnershipError{Dir: "/src/r/sub"}
	msg := err.Error()
	for _, want := range []string{"/src/r/sub", "git config --global --add safe.directory", "top-level directory"} {
		if !strings.Contains(msg, want) {
			t.Errorf("fallback message %q missing %q", msg, want)
		}
	}
	if !errors.Is(err, ErrGitDubiousOwnership) {
		t.Error("expected errors.Is match")
	}
}
