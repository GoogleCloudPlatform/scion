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
