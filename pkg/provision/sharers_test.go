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
	"slices"
	"testing"
)

// setupBase creates a temp dir with a .git subdirectory to simulate a repo base.
func setupBase(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return base
}

// inTreeWT returns a worktree path in the shape the registry actually
// records in production (base/worktrees/<name>) so marker validation at the
// read boundary accepts it.
func inTreeWT(base, name string) string {
	return filepath.Join(base, WorktreesSubdir, name)
}

func TestRegisterAndListSharers(t *testing.T) {
	base := setupBase(t)
	branch := "feature/foo"
	wt := inTreeWT(base, "wt-foo")

	if err := RegisterSharer(base, "", branch, wt, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSharer(base, "", branch, wt, "agent-2"); err != nil {
		t.Fatal(err)
	}

	sharers, path, err := ListSharers(base, "", branch)
	if err != nil {
		t.Fatal(err)
	}
	if path != wt {
		t.Errorf("worktreePath = %q, want %q", path, wt)
	}
	if len(sharers) != 2 {
		t.Fatalf("len(sharers) = %d, want 2", len(sharers))
	}
	if !slices.Contains(sharers, "agent-1") || !slices.Contains(sharers, "agent-2") {
		t.Errorf("sharers = %v, want [agent-1 agent-2]", sharers)
	}
}

func TestUnregisterSharer_OneRemaining(t *testing.T) {
	base := setupBase(t)
	branch := "feature/bar"
	wt := inTreeWT(base, "wt-bar")

	if err := RegisterSharer(base, "", branch, wt, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSharer(base, "", branch, wt, "agent-2"); err != nil {
		t.Fatal(err)
	}

	remaining, path, err := UnregisterSharer(base, "", branch, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if path != wt {
		t.Errorf("worktreePath = %q, want %q", path, wt)
	}
	if len(remaining) != 1 || remaining[0] != "agent-2" {
		t.Errorf("remaining = %v, want [agent-2]", remaining)
	}

	// Marker file should still exist.
	p := sharerPath(base, branch)
	if _, err := os.Stat(p); err != nil {
		t.Errorf("marker file should still exist: %v", err)
	}
}

func TestUnregisterSharer_LastRemoves(t *testing.T) {
	base := setupBase(t)
	branch := "feature/baz"
	wt := inTreeWT(base, "wt-baz")

	if err := RegisterSharer(base, "", branch, wt, "agent-1"); err != nil {
		t.Fatal(err)
	}

	remaining, path, err := UnregisterSharer(base, "", branch, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if path != wt {
		t.Errorf("worktreePath = %q, want %q", path, wt)
	}
	if len(remaining) != 0 {
		t.Errorf("remaining = %v, want []", remaining)
	}

	// Marker file should be deleted.
	p := sharerPath(base, branch)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("marker file should be removed, got err = %v", err)
	}
}

func TestFindBranchForAgent(t *testing.T) {
	base := setupBase(t)

	if err := RegisterSharer(base, "", "feature/alpha", inTreeWT(base, "alpha"), "agent-A"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSharer(base, "", "feature/beta", inTreeWT(base, "beta"), "agent-B"); err != nil {
		t.Fatal(err)
	}

	branch, wt, found, err := FindBranchForAgent(base, "", "agent-A")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected found=true for agent-A")
	}
	if branch != "feature/alpha" {
		t.Errorf("branch = %q, want %q", branch, "feature/alpha")
	}
	if wt != inTreeWT(base, "alpha") {
		t.Errorf("worktreePath = %q, want %q", wt, inTreeWT(base, "alpha"))
	}

	_, _, found, err = FindBranchForAgent(base, "", "agent-missing")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("expected found=false for agent-missing")
	}
}

func TestIdempotentRegister(t *testing.T) {
	base := setupBase(t)
	branch := "feature/idem"
	wt := inTreeWT(base, "idem")

	if err := RegisterSharer(base, "", branch, wt, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSharer(base, "", branch, wt, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSharer(base, "", branch, wt, "agent-1"); err != nil {
		t.Fatal(err)
	}

	sharers, _, err := ListSharers(base, "", branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(sharers) != 1 {
		t.Errorf("len(sharers) = %d after idempotent register, want 1", len(sharers))
	}
}

func TestListSharers_NoMarker(t *testing.T) {
	base := setupBase(t)

	sharers, path, err := ListSharers(base, "", "nonexistent-branch")
	if err != nil {
		t.Fatal(err)
	}
	if sharers != nil {
		t.Errorf("sharers = %v, want nil", sharers)
	}
	if path != "" {
		t.Errorf("worktreePath = %q, want empty", path)
	}
}

func TestUnregisterSharer_NoMarker(t *testing.T) {
	base := setupBase(t)

	remaining, path, err := UnregisterSharer(base, "", "nonexistent", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
	if path != "" {
		t.Errorf("worktreePath = %q, want empty", path)
	}
}

func TestUnregisterSharer_AgentNotInList(t *testing.T) {
	base := setupBase(t)
	branch := "feature/noop"
	wt := inTreeWT(base, "noop")

	_ = RegisterSharer(base, "", branch, wt, "agent-1")

	remaining, path, err := UnregisterSharer(base, "", branch, "agent-unknown")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0] != "agent-1" {
		t.Errorf("remaining = %v, want [agent-1]", remaining)
	}
	if path != wt {
		t.Errorf("worktreePath = %q, want %q", path, wt)
	}
}

// TestListSharers_OutOfTreeMarker_DegradesPathKeepsRefcount covers this fix:
// a marker whose WorktreePath matches neither scion-created shape (an
// absolute external path, a ".." traversal, a too-deep nested path, an empty
// path, or a relative path) must never surface that path — but the Sharers
// refcount must be preserved, not discarded. Discarding the whole marker was
// a real data-loss regression: it silently dropped legitimate ProvisionAgent-layout
// sharers (see TestWorktreePathIsScionCreated below and, at the integration
// level, pkg/agent's TestDeleteAgentFiles_ProvisionAgentLayout_* tests),
// causing teardown to remove a worktree another agent still used. A path
// that only diverges after symlink resolution (a symlinked intermediate
// directory or leaf) is NOT covered by this lexical check — see
// TestRemoveWorktree_RefusesOutOfTreePath in pkg/util for that; this test is
// about the shape of the stored path string, not what it resolves to on
// disk.
func TestListSharers_OutOfTreeMarker_DegradesPathKeepsRefcount(t *testing.T) {
	outside := t.TempDir()

	cases := []struct {
		name        string
		makeInvalid func(base string) string
	}{
		{
			name:        "absolute external path",
			makeInvalid: func(base string) string { return outside },
		},
		{
			name: "dot-dot traversal out of worktrees dir",
			makeInvalid: func(base string) string {
				return filepath.Join(base, WorktreesSubdir, "..", "..", "elsewhere")
			},
		},
		{
			name: "nested path deeper than a single worktree segment",
			makeInvalid: func(base string) string {
				return filepath.Join(base, WorktreesSubdir, "name", "nested")
			},
		},
		{
			name:        "empty path",
			makeInvalid: func(base string) string { return "" },
		},
		{
			name:        "relative path",
			makeInvalid: func(base string) string { return "worktrees/name" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := setupBase(t)
			branch := "feature/out-of-tree"
			bad := tc.makeInvalid(base)

			// Write the marker directly (not via RegisterSharer's
			// legitimate-path assumption) to simulate a peer that can write
			// the marker file but not go through the registry API.
			m := &sharerMarker{Branch: branch, WorktreePath: bad, Sharers: []string{"agent-c"}}
			if err := writeMarkerAtomic(sharerPath(base, branch), m); err != nil {
				t.Fatal(err)
			}

			sharers, wtPath, err := ListSharers(base, "", branch)
			if err != nil {
				t.Fatal(err)
			}
			if wtPath != "" {
				t.Errorf("worktreePath = %q, want empty for a marker with an invalid path", wtPath)
			}
			if len(sharers) != 1 || sharers[0] != "agent-c" {
				t.Errorf("sharers = %v, want [agent-c] (refcount must survive an invalid path)", sharers)
			}

			branchFound, wtFound, found, err := FindBranchForAgent(base, "", "agent-c")
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Error("FindBranchForAgent found=false, want true (refcount must survive an invalid path)")
			}
			if branchFound != branch {
				t.Errorf("branch = %q, want %q", branchFound, branch)
			}
			if wtFound != "" {
				t.Errorf("worktreePath = %q, want empty", wtFound)
			}
		})
	}
}

// TestWorktreePathIsScionCreated covers both scion-created worktree shapes
// this fix accepts, and confirms a path matching neither is rejected —
// including the ProvisionAgent "attach to an existing worktree" case, which
// is legitimate but does not match either shape (see readMarker's degrade
// behavior, not a discard, for that case).
func TestWorktreePathIsScionCreated(t *testing.T) {
	base := "/base"
	// projectDir is the actual resolved project directory a caller like
	// ProvisionAgent has in scope — here, the common top-level layout where
	// .scion sits directly under the repo root. See the subdirectory-layout
	// case below and TestDeleteAgentFiles_ProvisionAgentLayout_ScionInSubdirectory
	// (pkg/agent) for the non-top-level case this must NOT be confused with.
	projectDir := "/base/.scion"

	cases := []struct {
		name       string
		projectDir string
		candidate  string
		want       bool
	}{
		{"ProvisionShared shape", projectDir, "/base/worktrees/agent-a", true},
		{"ProvisionAgent create shape", projectDir, "/base/.scion/agents/agent-a/workspace", true},
		{"ProvisionAgent shape, different agent name", projectDir, "/base/.scion/agents/some-other-name/workspace", true},
		{"neither shape: bare agents dir", projectDir, "/base/.scion/agents/agent-a", false},
		{"neither shape: extra nesting under agents", projectDir, "/base/.scion/agents/agent-a/workspace/extra", false},
		{"neither shape: wrong leaf under agents", projectDir, "/base/.scion/agents/agent-a/home", false},
		{"neither shape: external path (e.g. a user hand-created worktree)", projectDir, "/elsewhere/worktree", false},
		{"neither shape: empty", projectDir, "", false},
		{"neither shape: relative", projectDir, "agents/agent-a/workspace", false},
		{"no projectDir (caller with no ProvisionAgent-layout concept): shape-2 candidate rejected", "", "/base/.scion/agents/agent-a/workspace", false},
		{"no projectDir: shape-1 candidate still accepted", "", "/base/worktrees/agent-a", true},
		{
			name:       ".scion in a repo subdirectory: shape-2 candidate under the REAL projectDir is accepted",
			projectDir: "/base/nested/project/.scion",
			candidate:  "/base/nested/project/.scion/agents/agent-a/workspace",
			want:       true,
		},
		{
			name:       ".scion in a repo subdirectory: reconstructing projectDir as base/.scion would wrongly reject this — must use the real projectDir instead",
			projectDir: "/base/nested/project/.scion",
			candidate:  "/base/.scion/agents/agent-a/workspace",
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WorktreePathIsScionCreated(base, tc.projectDir, tc.candidate)
			if got != tc.want {
				t.Errorf("WorktreePathIsScionCreated(%q, %q, %q) = %v, want %v", base, tc.projectDir, tc.candidate, got, tc.want)
			}
		})
	}
}

// TestListSharers_LegitimateInTreePath ensures the read boundary still
// accepts a marker whose WorktreePath is a genuine in-tree location.
func TestListSharers_LegitimateInTreePath(t *testing.T) {
	base := setupBase(t)
	branch := "feature/legit"
	wt := inTreeWT(base, "legit")

	if err := RegisterSharer(base, "", branch, wt, "agent-1"); err != nil {
		t.Fatal(err)
	}

	sharers, wtPath, err := ListSharers(base, "", branch)
	if err != nil {
		t.Fatal(err)
	}
	if wtPath != wt {
		t.Errorf("worktreePath = %q, want %q", wtPath, wt)
	}
	if len(sharers) != 1 || sharers[0] != "agent-1" {
		t.Errorf("sharers = %v, want [agent-1]", sharers)
	}
}

func TestFindBranchForAgent_NoDir(t *testing.T) {
	base := setupBase(t)

	_, _, found, err := FindBranchForAgent(base, "", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("expected found=false when scion-sharers dir does not exist")
	}
}

func TestSharerMarkers_CollidingSanitizedBranchesAreIsolated(t *testing.T) {
	pairs := []struct {
		name string
		a    string
		b    string
	}{
		{name: "slash vs hyphen", a: "feature/x", b: "feature-x"},
		{name: "double dot vs hyphen", a: "x..y", b: "x-y"},
		{name: "case difference", a: "Feature/x", b: "feature/x"},
		{name: "literal percent escape vs slash", a: "feature%2fx", b: "feature/x"},
		{name: "leading hyphen vs trimmed", a: "-leading", b: "leading"},
		{name: "trailing dot vs trimmed", a: "trailing.", b: "trailing"},
		{name: "empty vs agent", a: "", b: "agent"},
		{
			name: "overlong branches differing at tail",
			a:    filepath.ToSlash(string(make([]byte, 0))) + "prefix/" + string(slicesRepeatByte('a', 220)) + "/1",
			b:    "prefix/" + string(slicesRepeatByte('a', 220)) + "/2",
		},
	}

	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			base := setupBase(t)
			wtA := inTreeWT(base, "wt-a")
			wtB := inTreeWT(base, "wt-b")
			for _, wt := range []string{wtA, wtB} {
				if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			pathA := sharerPath(base, tc.a)
			pathB := sharerPath(base, tc.b)
			if pathA == pathB {
				t.Fatalf("sharerPath(%q) == sharerPath(%q) == %q", tc.a, tc.b, pathA)
			}
			if len(filepath.Base(pathA)) > 220 || len(filepath.Base(pathB)) > 220 {
				t.Fatalf("marker filename exceeds safe length: %d, %d", len(filepath.Base(pathA)), len(filepath.Base(pathB)))
			}

			if err := RegisterSharer(base, "", tc.a, wtA, "agent-a"); err != nil {
				t.Fatalf("RegisterSharer(%q): %v", tc.a, err)
			}

			// Before registering b, join/list lookups for b must see nothing.
			if sharersB, joinPathB, err := ListSharersForJoin(base, tc.b); err != nil || len(sharersB) != 0 || joinPathB != "" {
				t.Fatalf("ListSharersForJoin(%q) before register = (%v, %q, %v), want empty", tc.b, sharersB, joinPathB, err)
			}

			if err := RegisterSharer(base, "", tc.b, wtB, "agent-b"); err != nil {
				t.Fatalf("RegisterSharer(%q): %v", tc.b, err)
			}

			sharersA, gotWtA, err := ListSharers(base, "", tc.a)
			if err != nil {
				t.Fatalf("ListSharers(%q): %v", tc.a, err)
			}
			if gotWtA != wtA || !slices.Equal(sharersA, []string{"agent-a"}) {
				t.Errorf("branch %q state = (%v, %q), want ([agent-a], %q)", tc.a, sharersA, gotWtA, wtA)
			}

			sharersB, gotWtB, err := ListSharers(base, "", tc.b)
			if err != nil {
				t.Fatalf("ListSharers(%q): %v", tc.b, err)
			}
			if gotWtB != wtB || !slices.Equal(sharersB, []string{"agent-b"}) {
				t.Errorf("branch %q state = (%v, %q), want ([agent-b], %q)", tc.b, sharersB, gotWtB, wtB)
			}

			foundBranch, foundWT, found, err := FindBranchForAgent(base, "", "agent-b")
			if err != nil || !found || foundBranch != tc.b || foundWT != wtB {
				t.Errorf("FindBranchForAgent(agent-b) = (%q, %q, %v, %v), want (%q, %q, true, nil)", foundBranch, foundWT, found, err, tc.b, wtB)
			}

			if _, _, err := UnregisterSharer(base, "", tc.b, "agent-b"); err != nil {
				t.Fatalf("UnregisterSharer(%q): %v", tc.b, err)
			}
			// Branch a must be completely untouched.
			sharersA, gotWtA, err = ListSharers(base, "", tc.a)
			if err != nil || gotWtA != wtA || !slices.Equal(sharersA, []string{"agent-a"}) {
				t.Errorf("branch %q after unregistering %q = (%v, %q, %v), want ([agent-a], %q, nil)", tc.a, tc.b, sharersA, gotWtA, err, wtA)
			}
		})
	}
}

func slicesRepeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestSharerMarkers_RecordedBranchMismatchRefused(t *testing.T) {
	base := setupBase(t)
	wt := inTreeWT(base, "wt-1")
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a marker at feature/x's canonical path whose recorded Branch is unrelated.
	p := sharerPath(base, "feature/x")
	if err := writeMarkerAtomic(p, &sharerMarker{
		Branch:       "unrelated-branch",
		WorktreePath: wt,
		Sharers:      []string{"agent-1"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ListSharersForJoin(base, "feature/x"); err == nil {
		t.Error("ListSharersForJoin: expected error on recorded branch mismatch, got nil")
	}
	if _, _, err := ListSharers(base, "", "feature/x"); err == nil {
		t.Error("ListSharers: expected error on recorded branch mismatch, got nil")
	}
	if err := RegisterSharer(base, "", "feature/x", wt, "agent-2"); err == nil {
		t.Error("RegisterSharer: expected error on recorded branch mismatch, got nil")
	}
	if _, _, err := UnregisterSharer(base, "", "feature/x", "agent-1"); err == nil {
		t.Error("UnregisterSharer: expected error on recorded branch mismatch, got nil")
	}
}

func TestSharerMarkers_LegacySanitizedMarkerCompatibility(t *testing.T) {
	t.Run("read and migrate legacy marker for slash branch without exposing it to hyphen branch", func(t *testing.T) {
		base := setupBase(t)
		wtSlash := inTreeWT(base, "wt-slash")
		if err := os.MkdirAll(filepath.Join(wtSlash, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}

		legacyPath := filepath.Join(base, ".git", sharerDir, "feature-x.json")
		if err := writeMarkerAtomic(legacyPath, &sharerMarker{
			Branch:       "feature/x",
			WorktreePath: wtSlash,
			Sharers:      []string{"agent-1"},
		}); err != nil {
			t.Fatal(err)
		}

		// Legacy marker for feature/x is readable under feature/x.
		sharers, gotWT, err := ListSharersForJoin(base, "feature/x")
		if err != nil || gotWT != wtSlash || !slices.Equal(sharers, []string{"agent-1"}) {
			t.Fatalf("ListSharersForJoin(feature/x) = (%v, %q, %v), want ([agent-1], %q, nil)", sharers, gotWT, err, wtSlash)
		}

		// Hyphen branch feature-x must NOT see feature/x's legacy marker.
		if sharersDash, wtDash, err := ListSharersForJoin(base, "feature-x"); err != nil || len(sharersDash) != 0 || wtDash != "" {
			t.Fatalf("ListSharersForJoin(feature-x) = (%v, %q, %v), want empty", sharersDash, wtDash, err)
		}
		if rem, wtDash, err := UnregisterSharer(base, "", "feature-x", "agent-1"); err != nil || rem != nil || wtDash != "" {
			t.Fatalf("UnregisterSharer(feature-x) = (%v, %q, %v), want (nil, \"\", nil)", rem, wtDash, err)
		}

		// Registering a second sharer on feature/x migrates the marker to its encoded path.
		if err := RegisterSharer(base, "", "feature/x", wtSlash, "agent-2"); err != nil {
			t.Fatalf("RegisterSharer(feature/x): %v", err)
		}
		if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
			t.Errorf("legacy marker %s should be removed after migration, stat err = %v", legacyPath, err)
		}
		if _, err := os.Stat(sharerPath(base, "feature/x")); err != nil {
			t.Errorf("encoded marker should exist after migration: %v", err)
		}
		sharers, gotWT, err = ListSharers(base, "", "feature/x")
		if err != nil || gotWT != wtSlash || !slices.Equal(sharers, []string{"agent-1", "agent-2"}) {
			t.Fatalf("ListSharers(feature/x) after migrate = (%v, %q, %v)", sharers, gotWT, err)
		}
	})

	t.Run("registering colliding hyphen branch relocates existing legacy slash marker intact", func(t *testing.T) {
		base := setupBase(t)
		projectDir := filepath.Join(base, ".scion")
		wtSlash := filepath.Join(projectDir, "agents", "a1", "workspace")
		wtDash := inTreeWT(base, "wt-dash")
		for _, wt := range []string{wtSlash, wtDash} {
			if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		legacyPath := filepath.Join(base, ".git", sharerDir, "feature-x.json")
		if err := writeMarkerAtomic(legacyPath, &sharerMarker{
			Branch:       "feature/x",
			WorktreePath: wtSlash,
			Sharers:      []string{"a1"},
		}); err != nil {
			t.Fatal(err)
		}

		// Registering feature-x with projectDir=="" (ProvisionShared caller) must
		// relocate feature/x's marker to feature%2fx.json without losing its
		// ProvisionAgent-shaped WorktreePath.
		if err := RegisterSharer(base, "", "feature-x", wtDash, "agent-2"); err != nil {
			t.Fatalf("RegisterSharer(feature-x): %v", err)
		}

		sharersSlash, gotSlashWT, err := ListSharers(base, projectDir, "feature/x")
		if err != nil || gotSlashWT != wtSlash || !slices.Equal(sharersSlash, []string{"a1"}) {
			t.Fatalf("ListSharers(feature/x) = (%v, %q, %v), want ([a1], %q, nil)", sharersSlash, gotSlashWT, err, wtSlash)
		}
		sharersDash, gotDashWT, err := ListSharers(base, "", "feature-x")
		if err != nil || gotDashWT != wtDash || !slices.Equal(sharersDash, []string{"agent-2"}) {
			t.Fatalf("ListSharers(feature-x) = (%v, %q, %v), want ([agent-2], %q, nil)", sharersDash, gotDashWT, err, wtDash)
		}
	})

	t.Run("unregistering from legacy marker migrates remaining sharers and deletes when empty", func(t *testing.T) {
		base := setupBase(t)
		wtSlash := inTreeWT(base, "wt-slash")
		if err := os.MkdirAll(filepath.Join(wtSlash, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}

		legacyPath := filepath.Join(base, ".git", sharerDir, "feature-x.json")
		if err := writeMarkerAtomic(legacyPath, &sharerMarker{
			Branch:       "feature/x",
			WorktreePath: wtSlash,
			Sharers:      []string{"agent-1", "agent-2"},
		}); err != nil {
			t.Fatal(err)
		}

		rem, gotWT, err := UnregisterSharer(base, "", "feature/x", "agent-1")
		if err != nil || gotWT != wtSlash || !slices.Equal(rem, []string{"agent-2"}) {
			t.Fatalf("UnregisterSharer first = (%v, %q, %v), want ([agent-2], %q, nil)", rem, gotWT, err, wtSlash)
		}
		if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
			t.Errorf("legacy marker should be removed after UnregisterSharer migration, stat err = %v", err)
		}
		if _, err := os.Stat(sharerPath(base, "feature/x")); err != nil {
			t.Errorf("canonical marker should exist after UnregisterSharer migration: %v", err)
		}

		// Also test removing the last sharer directly from a legacy marker.
		baseSingle := setupBase(t)
		legacySingle := filepath.Join(baseSingle, ".git", sharerDir, "feature-x.json")
		if err := writeMarkerAtomic(legacySingle, &sharerMarker{
			Branch:       "feature/x",
			WorktreePath: inTreeWT(baseSingle, "wt-slash"),
			Sharers:      []string{"agent-only"},
		}); err != nil {
			t.Fatal(err)
		}
		rem, _, err = UnregisterSharer(baseSingle, "", "feature/x", "agent-only")
		if err != nil || len(rem) != 0 {
			t.Fatalf("UnregisterSharer last from legacy = (%v, %v), want ([], nil)", rem, err)
		}
		if _, err := os.Stat(legacySingle); !os.IsNotExist(err) {
			t.Errorf("legacy marker should be removed when last sharer unregisters, stat err = %v", err)
		}
	})
}
