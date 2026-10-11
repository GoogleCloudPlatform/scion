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

	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// shellWord evaluates arg as a single POSIX shell word, the way a user
// pasting git's suggested command would.
func shellWord(t *testing.T, arg string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", "printf %s "+arg).Output()
	if err != nil {
		t.Fatalf("sh evaluation of %q: %v", arg, err)
	}
	return string(out)
}

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
	testutil.RequireGitAssumeDifferentOwner(t)
	for _, name := range []string{"repo", "my repo", "it's"} {
		t.Run(name, func(t *testing.T) {
			home := isolateGitEnv(t)
			repo := filepath.Join(home, name)
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
			if !strings.HasSuffix(err.Error(), "git config --global --add safe.directory "+oe.SuggestedArg) {
				t.Errorf("error does not end with git's suggested command: %v", err)
			}
			// The printed argument, pasted into a shell, names the repository.
			want, _ := filepath.EvalSymlinks(repo)
			got, _ := filepath.EvalSymlinks(shellWord(t, oe.SuggestedArg))
			if got != want {
				t.Errorf("SuggestedArg %q evaluates to %q, want %q", oe.SuggestedArg, got, want)
			}

			// The bool-only helper keeps its existing contract.
			if IsGitRepoDir(repo) {
				t.Error("IsGitRepoDir should still return false for a refused repository")
			}
		})
	}
}

func TestCheckGitRepoDir_OwnershipRefusalNewlineInPath(t *testing.T) {
	testutil.RequireGitAssumeDifferentOwner(t)
	home := isolateGitEnv(t)
	repo := filepath.Join(home, "new\nline")
	initTestRepo(t, repo)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	_, err := CheckGitRepoDir(repo)
	var oe *GitOwnershipError
	if !errors.As(err, &oe) {
		t.Fatalf("expected *GitOwnershipError, got %v", err)
	}
	// Git prints the quoted path across two lines; the partial token is not
	// offered as a command, and the fallback text is used instead.
	if oe.SuggestedArg != "" {
		t.Errorf("SuggestedArg = %q, want empty for a path containing a newline", oe.SuggestedArg)
	}
	if !strings.Contains(err.Error(), "top-level directory") {
		t.Errorf("expected the fallback message, got %v", err)
	}
}

func TestIsGitOwnershipRefusal(t *testing.T) {
	tests := []struct {
		name, stderr string
		want         bool
	}{
		{
			name: "dubious ownership (git 2.36.2, 2.37.1 and later)",
			stderr: "fatal: detected dubious ownership in repository at '/src/r'\n" +
				"To add an exception for this directory, call:\n\n" +
				"\tgit config --global --add safe.directory /src/r\n",
			want: true,
		},
		{
			name: "owned by someone else (git 2.35.2 to 2.36.1, 2.37.0)",
			stderr: "fatal: unsafe repository ('/src/r' is owned by someone else)\n" +
				"To add an exception for this directory, call:\n\n" +
				"\tgit config --global --add safe.directory /src/r\n",
			want: true,
		},
		{
			name:   "not a repository",
			stderr: "fatal: not a git repository (or any of the parent directories): .git\n",
			want:   false,
		},
		{
			name:   "empty",
			stderr: "",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGitOwnershipRefusal(tt.stderr); got != tt.want {
				t.Errorf("isGitOwnershipRefusal() = %v, want %v", got, tt.want)
			}
		})
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
			name:   "space",
			stderr: "\tgit config --global --add safe.directory '/src/my repo'\n",
			want:   "'/src/my repo'",
		},
		{
			name:   "single quote",
			stderr: "\tgit config --global --add safe.directory '/src/it'\\''s'\n",
			want:   `'/src/it'\''s'`,
		},
		{
			name: "older git wording",
			stderr: "fatal: unsafe repository ('/src/r' is owned by someone else)\n" +
				"To add an exception for this directory, call:\n\n" +
				"\tgit config --global --add safe.directory /src/r\n",
			want: "/src/r",
		},
		{
			name: "newline in path",
			stderr: "fatal: detected dubious ownership in repository at '/src/a\nb'\n" +
				"To add an exception for this directory, call:\n\n" +
				"\tgit config --global --add safe.directory '/src/a\nb'\n",
			want: "",
		},
		{
			name:   "unbalanced quote",
			stderr: "\tgit config --global --add safe.directory '/src/r\n",
			want:   "",
		},
		{
			name:   "trailing backslash",
			stderr: "\tgit config --global --add safe.directory /src/r\\\n",
			want:   "",
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

func TestGitOwnershipError_RendersSuggestedArgVerbatim(t *testing.T) {
	for _, arg := range []string{"/src/r", "'/src/my repo'", `'/src/it'\''s'`} {
		err := &GitOwnershipError{Dir: "/src/r", SuggestedArg: arg}
		want := "git config --global --add safe.directory " + arg
		if !strings.HasSuffix(err.Error(), want) {
			t.Errorf("message %q should end with %q", err.Error(), want)
		}
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
