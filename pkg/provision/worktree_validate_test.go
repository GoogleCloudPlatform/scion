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

package provision

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestRepo creates a real (non-bare) git repo with one commit, suitable
// as a worktree base for ValidateWorktreeForBase tests.
func newTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run(t, "git", "-C", root, "init", "-q", "--initial-branch=main")
	run(t, "git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "--allow-empty", "-m", "init")
	return root
}

// newTestWorktree creates base/worktrees/<name> as a genuine git worktree of
// base, via the same --relative-paths flag production code uses, and
// returns its path.
func newTestWorktree(t *testing.T, base, name string) string {
	t.Helper()
	worktreesDir := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("mkdir worktrees dir: %v", err)
	}
	worktree := filepath.Join(worktreesDir, name)
	run(t, "git", "-C", base, "worktree", "add", "--relative-paths", "-b", name, worktree)
	return worktree
}

// --- ValidateWorktreeForBase (full relationship check) ---

// TestValidateWorktreeForBase_Accepts is the accept case: a real git repo
// base with a candidate that is a genuine git worktree nested under
// base/worktrees/<id> — the exact layout ensureWorktree creates.
func TestValidateWorktreeForBase_Accepts(t *testing.T) {
	base := newTestRepo(t)
	worktree := newTestWorktree(t, base, "agent-1")

	if err := ValidateWorktreeForBase(base, worktree); err != nil {
		t.Fatalf("ValidateWorktreeForBase(base, worktree) = %v, want nil", err)
	}
}

// TestValidateWorktreeForBase_RejectsPlainDirectory is the required
// containment-invariant guard: a plain directory at base/worktrees/<name> —
// the right shape but no actual git worktree relationship — must be
// rejected. Directory shape alone is not sufficient; the function must prove
// a real worktree relationship.
func TestValidateWorktreeForBase_RejectsPlainDirectory(t *testing.T) {
	base := newTestRepo(t)
	worktree := filepath.Join(base, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	if err := ValidateWorktreeForBase(base, worktree); err == nil {
		t.Fatal("plain directory: got nil error, want a rejection (not a real git worktree)")
	}
}

// TestValidateWorktreeForBase_RejectsWorktreesRootExactly covers a candidate
// exactly AT base/worktrees (no name segment): not the layout ensureWorktree
// creates, so it must be rejected.
func TestValidateWorktreeForBase_RejectsWorktreesRootExactly(t *testing.T) {
	base := newTestRepo(t)
	worktreesDir := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("mkdir worktrees dir: %v", err)
	}

	if err := ValidateWorktreeForBase(base, worktreesDir); err == nil {
		t.Fatal("candidate == base/worktrees exactly: got nil error, want a rejection")
	}
}

// TestValidateWorktreeForBase_RejectsNestedBeyondName covers a candidate
// nested one level deeper than the exact layout, e.g.
// base/worktrees/<name>/extra — must be rejected, not just anything "under"
// worktrees/.
func TestValidateWorktreeForBase_RejectsNestedBeyondName(t *testing.T) {
	base := newTestRepo(t)
	worktree := newTestWorktree(t, base, "agent-1")
	nested := filepath.Join(worktree, "extra")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	if err := ValidateWorktreeForBase(base, nested); err == nil {
		t.Fatal("candidate nested beyond worktrees/<name>: got nil error, want a rejection")
	}
}

// TestValidateWorktreeForBase_RejectsMismatchedAdminBackLink covers a
// pairing that satisfies the path-shape and gitfile-presence checks but not
// the admin-directory back-link: a candidate gitfile that points at a
// correctly-shaped admin directory under base, whose own back-link resolves
// somewhere other than the candidate. A genuine `git worktree add` always
// keeps these two pointers consistent with each other, so a mismatch here
// means the pairing did not come from one.
func TestValidateWorktreeForBase_RejectsMismatchedAdminBackLink(t *testing.T) {
	base := newTestRepo(t)

	worktree := filepath.Join(base, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	adminDir := filepath.Join(base, ".git", "worktrees", "agent-1")
	if err := os.MkdirAll(adminDir, 0755); err != nil {
		t.Fatalf("mkdir admin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+adminDir+"\n"), 0644); err != nil {
		t.Fatalf("write candidate gitfile: %v", err)
	}
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(adminDir, "gitdir"), []byte(elsewhere+"\n"), 0644); err != nil {
		t.Fatalf("write admin back-link: %v", err)
	}

	if err := ValidateWorktreeForBase(base, worktree); err == nil {
		t.Fatal("mismatched admin back-link: got nil error, want a rejection")
	}
}

// TestValidateWorktreeForBase_RejectsAliasedBase is the required lexical
// regression guard: a base and candidate that relate only after resolving a
// symlink, not lexically. Git itself can report a resolved path for a base
// reached through a symlinked directory (e.g. an aliased project path), so
// this pairing is realistic, not synthetic. pkg/runtime/common.go computes
// its mount branch from the unresolved (base, workspace) pair, so a pairing
// that only validates after symlink resolution must still be rejected —
// accepting it here would approve a mount decision common.go does not
// actually make.
func TestValidateWorktreeForBase_RejectsAliasedBase(t *testing.T) {
	realBase := newTestRepo(t)
	worktree := newTestWorktree(t, realBase, "agent-1")

	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "alias")
	if err := os.Symlink(realBase, alias); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}
	aliasedCandidate := filepath.Join(alias, "worktrees", "agent-1")

	// realBase paired with the aliased candidate path: resolves to a valid
	// worktree relationship, but the lexical pair does not name
	// "worktrees/agent-1" under realBase at all.
	if err := ValidateWorktreeForBase(realBase, aliasedCandidate); err == nil {
		t.Fatal("base paired with an aliased candidate: got nil error, want a rejection")
	}
	// The symmetric case: the aliased base paired with the real candidate.
	if err := ValidateWorktreeForBase(alias, worktree); err == nil {
		t.Fatal("aliased base paired with the real candidate: got nil error, want a rejection")
	}
}

// TestValidateWorktreeForBase_RejectsNonGitBase covers a candidate base with
// no .git directory at all — must be rejected rather than trusted.
func TestValidateWorktreeForBase_RejectsNonGitBase(t *testing.T) {
	base := t.TempDir() // no git repo here
	worktree := filepath.Join(base, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	if err := ValidateWorktreeForBase(base, worktree); err == nil {
		t.Fatal("non-git base: got nil error, want a rejection (must not trust a base with no .git)")
	}
}

// TestValidateWorktreeForBase_RejectsCandidateOutsideBase covers a real git
// repo base paired with a candidate that isn't inside it at all.
func TestValidateWorktreeForBase_RejectsCandidateOutsideBase(t *testing.T) {
	base := newTestRepo(t)
	outside := t.TempDir() // sibling, not under base at all

	if err := ValidateWorktreeForBase(base, outside); err == nil {
		t.Fatal("candidate outside base: got nil error, want a rejection")
	}
}

// TestValidateWorktreeForBase_RejectsCandidateEqualsBase covers the case
// where the candidate IS the base itself: not the worktrees/<name> layout,
// so it must be rejected.
func TestValidateWorktreeForBase_RejectsCandidateEqualsBase(t *testing.T) {
	base := newTestRepo(t)

	if err := ValidateWorktreeForBase(base, base); err == nil {
		t.Fatal("candidate == base: got nil error, want a rejection")
	}
}

// TestValidateWorktreeForBase_RejectsEmptyOrRelativeInputs covers every
// empty/relative combination: both arguments must be non-empty, absolute
// paths, or the function must reject rather than resolve the relationship
// against the calling process's current working directory.
func TestValidateWorktreeForBase_RejectsEmptyOrRelativeInputs(t *testing.T) {
	base := newTestRepo(t)
	worktree := newTestWorktree(t, base, "agent-1")

	cases := []struct {
		name      string
		base      string
		candidate string
	}{
		{"empty base", "", worktree},
		{"empty candidate", base, ""},
		{"both empty", "", ""},
		{"dot base, relative candidate", ".", "worktrees/agent-1"},
		{"relative base, relative candidate", "base", "base/worktrees/agent-1"},
		{"absolute base, relative candidate", base, "worktrees/agent-1"},
		{"relative base, absolute candidate", "base", worktree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateWorktreeForBase(tc.base, tc.candidate); err == nil {
				t.Fatalf("ValidateWorktreeForBase(%q, %q) = nil, want a rejection", tc.base, tc.candidate)
			}
		})
	}
}

// TestValidateWorktreeForBase_RejectsNonexistentPaths covers paths that
// don't resolve at all — e.g. a stored value pointing at a since-deleted
// directory.
func TestValidateWorktreeForBase_RejectsNonexistentPaths(t *testing.T) {
	base := newTestRepo(t)
	worktree := filepath.Join(base, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	nonexistentBase := filepath.Join(t.TempDir(), "does-not-exist")
	if err := ValidateWorktreeForBase(nonexistentBase, worktree); err == nil {
		t.Fatal("nonexistent base: got nil error, want a rejection")
	}

	nonexistentCandidate := filepath.Join(base, "worktrees", "no-such-agent")
	if err := ValidateWorktreeForBase(base, nonexistentCandidate); err == nil {
		t.Fatal("nonexistent candidate: got nil error, want a rejection")
	}
}

// --- WorktreeIsLexicallyUnderBase (lexical-only containment check) ---

// TestWorktreeIsLexicallyUnderBase_Accepts confirms the accept case does not
// require the worktree to exist on disk at all — only the lexical shape
// matters, unlike ValidateWorktreeForBase.
func TestWorktreeIsLexicallyUnderBase_Accepts(t *testing.T) {
	base := t.TempDir() // no .git, no worktree — irrelevant to this check
	candidate := filepath.Join(base, "worktrees", "agent-1")

	if !WorktreeIsLexicallyUnderBase(base, candidate) {
		t.Fatalf("WorktreeIsLexicallyUnderBase(%q, %q) = false, want true", base, candidate)
	}
}

// TestWorktreeIsLexicallyUnderBase_AcceptsPartiallyRemoved confirms the
// check still passes when the candidate directory has already been removed
// from disk — the case it exists for: a teardown path deciding whether a
// path is even eligible to remove, potentially after the worktree itself is
// already gone.
func TestWorktreeIsLexicallyUnderBase_AcceptsPartiallyRemoved(t *testing.T) {
	base := newTestRepo(t)
	worktree := newTestWorktree(t, base, "agent-1")
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("RemoveAll(worktree): %v", err)
	}

	if !WorktreeIsLexicallyUnderBase(base, worktree) {
		t.Fatal("removed worktree: WorktreeIsLexicallyUnderBase = false, want true (shape check only)")
	}
}

// TestWorktreeIsLexicallyUnderBase_RejectsWorktreesRootExactly covers
// base/worktrees itself: with no name segment, it is not a valid single
// path element.
func TestWorktreeIsLexicallyUnderBase_RejectsWorktreesRootExactly(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "worktrees")

	if WorktreeIsLexicallyUnderBase(base, candidate) {
		t.Fatal("base/worktrees exactly: got true, want false")
	}
}

// TestWorktreeIsLexicallyUnderBase_RejectsNestedBeyondName mirrors the full
// check's nesting case.
func TestWorktreeIsLexicallyUnderBase_RejectsNestedBeyondName(t *testing.T) {
	base := t.TempDir()
	candidate := filepath.Join(base, "worktrees", "agent-1", "extra")

	if WorktreeIsLexicallyUnderBase(base, candidate) {
		t.Fatal("nested beyond worktrees/<name>: got true, want false")
	}
}

// TestWorktreeIsLexicallyUnderBase_RejectsOutsideBase covers a candidate
// that isn't under base at all.
func TestWorktreeIsLexicallyUnderBase_RejectsOutsideBase(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()

	if WorktreeIsLexicallyUnderBase(base, outside) {
		t.Fatal("candidate outside base: got true, want false")
	}
}

// TestWorktreeIsLexicallyUnderBase_RejectsAlias confirms the check is purely
// lexical: a candidate reached through a different path spelling is rejected
// even if it resolves into base, since the check is intentionally lexical
// (no EvalSymlinks) and an alias pointing elsewhere does not lexically match
// base/worktrees/<name> under the base path actually being validated.
func TestWorktreeIsLexicallyUnderBase_RejectsAlias(t *testing.T) {
	realBase := t.TempDir()
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "alias")
	if err := os.Symlink(realBase, alias); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}
	aliasedCandidate := filepath.Join(alias, "worktrees", "agent-1")

	if WorktreeIsLexicallyUnderBase(realBase, aliasedCandidate) {
		t.Fatal("base paired with an aliased candidate: got true, want false")
	}
}

// TestWorktreeIsLexicallyUnderBase_RejectsEmptyOrRelativeInputs covers every
// empty/relative combination. The prior version of this test (named
// EmptyInputsRejected) only ever exercised an absolute candidate, so it
// passed for the wrong reason (filepath.Rel(".", <absolute>) happens to
// error) without ever proving a relative base or candidate is rejected —
// worktreeRelName now checks filepath.IsAbs directly, and this table
// exercises that check for both arguments and their empty/dot/relative
// forms.
func TestWorktreeIsLexicallyUnderBase_RejectsEmptyOrRelativeInputs(t *testing.T) {
	absBase := t.TempDir()

	cases := []struct {
		name      string
		base      string
		candidate string
	}{
		{"empty base, relative candidate", "", "worktrees/agent-1"},
		{"dot base, relative candidate", ".", "worktrees/agent-1"},
		{"relative base, relative candidate", "base", "base/worktrees/agent-1"},
		{"absolute base, relative candidate", absBase, "worktrees/agent-1"},
		{"absolute base, empty candidate", absBase, ""},
		{"empty base, absolute candidate", "", filepath.Join(absBase, "worktrees", "agent-1")},
		{"both empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if WorktreeIsLexicallyUnderBase(tc.base, tc.candidate) {
				t.Fatalf("WorktreeIsLexicallyUnderBase(%q, %q) = true, want false", tc.base, tc.candidate)
			}
		})
	}
}
