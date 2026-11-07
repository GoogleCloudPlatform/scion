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

package maintenance

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestValidateBranchName runs the real git check-ref-format (ptone/scion#6987).
func TestValidateBranchName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	ctx := context.Background()

	for _, ok := range []string{"", "main", "feature/x", "release/v0.3", "user.name-1"} {
		if err := validateBranchName(ctx, dir, ok); err != nil {
			t.Errorf("validateBranchName(%q) = %v, want nil", ok, err)
		}
	}

	for _, tc := range []struct{ name, want string }{
		// Option-shaped: refused before git runs.
		{"-x", "cannot start with '-'"},
		{"--upload-pack=touch /tmp/pwned", "cannot start with '-'"},
		{"--orphan", "cannot start with '-'"},
		{"-", "cannot start with '-'"},
		// Not valid branch names: refused by git check-ref-format --branch.
		{"a..b", "not a valid git branch name"},
		{"a b", "not a valid git branch name"},
		{"x.lock", "not a valid git branch name"},
		{"HEAD", "not a valid git branch name"},
		{"feature/", "not a valid git branch name"},
		{"/feature", "not a valid git branch name"},
		{"a~1", "not a valid git branch name"},
		{"a:b", "not a valid git branch name"},
		{"a\nb", "not a valid git branch name"},
	} {
		err := validateBranchName(ctx, dir, tc.name)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("validateBranchName(%q) = %v, want an error containing %q", tc.name, err, tc.want)
		}
		// The name is quoted and git's output is not echoed, so no raw
		// control character can reach a run log line.
		if err != nil && strings.ContainsAny(err.Error(), "\n\r\t") {
			t.Errorf("validateBranchName(%q) error carries a raw control character: %q", tc.name, err.Error())
		}
	}
}

// TestValidateBranchName_RefusesExpansion: git expands "@{-1}" to the
// previously checked-out branch; the name must be the branch itself.
func TestValidateBranchName_RefusesExpansion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x")
	run("checkout", "-q", "-b", "other")

	err := validateBranchName(context.Background(), dir, "@{-1}")
	if err == nil || !strings.Contains(err.Error(), "resolves it to") {
		t.Fatalf("validateBranchName(@{-1}) = %v, want refused as an expansion", err)
	}
}

func TestParseGitVersion(t *testing.T) {
	for _, tc := range []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"git version 2.43.0", 2, 43, true},
		{"git version 2.39.5 (Apple Git-154)", 2, 39, true},
		{"git version 2.24.0.windows.1", 2, 24, true},
		{"git version 3.0", 3, 0, true},
		{"git version 2", 0, 0, false},
		{"git version x.y", 0, 0, false},
		{"version 2.43.0", 0, 0, false},
		{"", 0, 0, false},
	} {
		major, minor, ok := parseGitVersion(tc.in)
		if major != tc.major || minor != tc.minor || ok != tc.ok {
			t.Errorf("parseGitVersion(%q) = %d, %d, %v; want %d, %d, %v", tc.in, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

func TestBranchArgs(t *testing.T) {
	if got, want := branchArgs(true, "origin/main", "reset", "--hard"), []string{"reset", "--hard", "--end-of-options", "origin/main"}; !reflect.DeepEqual(got, want) {
		t.Errorf("branchArgs(true) = %q, want %q", got, want)
	}
	if got, want := branchArgs(false, "main", "checkout"), []string{"checkout", "main"}; !reflect.DeepEqual(got, want) {
		t.Errorf("branchArgs(false) = %q, want %q", got, want)
	}
}

// branchArgs must not write into the caller's backing array, even when the
// variadic args slice has spare capacity.
func TestBranchArgs_DoesNotMutateCallerSlice(t *testing.T) {
	for _, eoo := range []bool{false, true} {
		s := make([]string, 1, 4)
		s[0] = "checkout"
		got := branchArgs(eoo, "main", s...)
		if full := s[:cap(s)]; !reflect.DeepEqual(full, []string{"checkout", "", "", ""}) {
			t.Errorf("branchArgs(%v) changed the caller's backing array: %q", eoo, full)
		}
		want := []string{"checkout", "main"}
		if eoo {
			want = []string{"checkout", "--end-of-options", "main"}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("branchArgs(%v) = %q, want %q", eoo, got, want)
		}
	}
}

// A git that cannot run (here: the checkout directory is gone) is reported
// as such, quoted, not as an invalid branch name.
func TestValidateBranchName_GitCannotRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	missing := filepath.Join(t.TempDir(), "gone")
	err := validateBranchName(context.Background(), missing, "main")
	if err == nil || !strings.HasPrefix(err.Error(), `could not validate branch "main": "`) {
		t.Fatalf("validateBranchName(missing dir) = %v, want a could-not-validate error", err)
	}
	if strings.Contains(err.Error(), "invalid branch") || strings.ContainsAny(err.Error(), "\n\r") {
		t.Fatalf("validateBranchName(missing dir) = %q", err.Error())
	}
}

// git 2.24 added --end-of-options, but checkout and reset accept it only
// from 2.44 (#6987 review C1).
func TestEndOfOptionsSupported(t *testing.T) {
	for _, tc := range []struct {
		major, minor int
		want         bool
	}{
		{2, 24, false}, {2, 34, false}, {2, 39, false}, {2, 43, false},
		{2, 44, true}, {2, 51, true}, {3, 0, true}, {1, 99, false},
	} {
		if got := endOfOptionsSupported(tc.major, tc.minor); got != tc.want {
			t.Errorf("endOfOptionsSupported(%d, %d) = %v, want %v", tc.major, tc.minor, got, tc.want)
		}
	}
}
