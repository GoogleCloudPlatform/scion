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
		{"a..b", "check-ref-format"},
		{"a b", "check-ref-format"},
		{"x.lock", "check-ref-format"},
		{"HEAD", "check-ref-format"},
		{"feature/", "check-ref-format"},
		{"/feature", "check-ref-format"},
		{"a~1", "check-ref-format"},
		{"a:b", "check-ref-format"},
		{"a\nb", "check-ref-format"},
	} {
		err := validateBranchName(ctx, dir, tc.name)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("validateBranchName(%q) = %v, want an error containing %q", tc.name, err, tc.want)
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
