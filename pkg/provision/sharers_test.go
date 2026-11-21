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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		{name: "windows reserved vs escaped literal", a: "con", b: "%63on"},
		{
			name: "overlong multi-segment branches exceeding NAME_MAX when flattened",
			a:    "feature/" + strings.Repeat("a", 130) + "/" + strings.Repeat("b", 130) + "/1",
			b:    "feature/" + strings.Repeat("a", 130) + "/" + strings.Repeat("b", 130) + "/2",
		},
	}

	for _, tc := range pairs {
		for _, reverse := range []bool{false, true} {
			first, second := tc.a, tc.b
			orderName := "a_then_b"
			if reverse {
				first, second = tc.b, tc.a
				orderName = "b_then_a"
			}
			t.Run(tc.name+"/"+orderName, func(t *testing.T) {
				base := setupBase(t)
				wtInit := inTreeWT(base, "wt-init")
				wtFirst := inTreeWT(base, "wt-first")
				wtSecond := inTreeWT(base, "wt-second")
				for _, wt := range []string{wtInit, wtFirst, wtSecond} {
					if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				// Pre-populate .git/scion-sharers/ so legacy fallback lookups
				// hit an existing directory (exposing ENAMETOOLONG if an
				// un-bounded sanitized basename is probed).
				if err := RegisterSharer(base, "", "init-branch", wtInit, "agent-init"); err != nil {
					t.Fatalf("RegisterSharer(init-branch): %v", err)
				}

				pathFirst := sharerPath(base, first)
				pathSecond := sharerPath(base, second)
				if pathFirst == pathSecond {
					t.Fatalf("sharerPath(%q) == sharerPath(%q) == %q", first, second, pathFirst)
				}
				if len(filepath.Base(pathFirst)) > 105 || len(filepath.Base(pathSecond)) > 105 {
					t.Fatalf("marker filename exceeds safe length: %d, %d", len(filepath.Base(pathFirst)), len(filepath.Base(pathSecond)))
				}

				// Before registering first, join/list/unregister on first must succeed with empty state.
				if sharers1, joinPath1, err := ListSharersForJoin(base, first); err != nil || len(sharers1) != 0 || joinPath1 != "" {
					t.Fatalf("ListSharersForJoin(%q) before register = (%v, %q, %v), want empty", first, sharers1, joinPath1, err)
				}

				if err := RegisterSharer(base, "", first, wtFirst, "agent-first"); err != nil {
					t.Fatalf("RegisterSharer(%q): %v", first, err)
				}

				// Before registering second, join/list/unregister lookups for second
				// must see nothing and must not touch first's marker (even when
				// second's legacySharerPath collides onto first's canonical path).
				if sharers2, joinPath2, err := ListSharersForJoin(base, second); err != nil || len(sharers2) != 0 || joinPath2 != "" {
					t.Fatalf("ListSharersForJoin(%q) before register = (%v, %q, %v), want empty", second, sharers2, joinPath2, err)
				}
				if sharers2, listPath2, err := ListSharers(base, "", second); err != nil || len(sharers2) != 0 || listPath2 != "" {
					t.Fatalf("ListSharers(%q) before register = (%v, %q, %v), want empty", second, sharers2, listPath2, err)
				}
				if rem2, unregPath2, err := UnregisterSharer(base, "", second, "agent-first"); err != nil || rem2 != nil || unregPath2 != "" {
					t.Fatalf("UnregisterSharer(%q) before register = (%v, %q, %v), want (nil, \"\", nil)", second, rem2, unregPath2, err)
				}

				if err := RegisterSharer(base, "", second, wtSecond, "agent-second"); err != nil {
					t.Fatalf("RegisterSharer(%q): %v", second, err)
				}

				sharersFirst, gotWtFirst, err := ListSharers(base, "", first)
				if err != nil {
					t.Fatalf("ListSharers(%q): %v", first, err)
				}
				if gotWtFirst != wtFirst || !slices.Equal(sharersFirst, []string{"agent-first"}) {
					t.Errorf("branch %q state = (%v, %q), want ([agent-first], %q)", first, sharersFirst, gotWtFirst, wtFirst)
				}

				sharersSecond, gotWtSecond, err := ListSharers(base, "", second)
				if err != nil {
					t.Fatalf("ListSharers(%q): %v", second, err)
				}
				if gotWtSecond != wtSecond || !slices.Equal(sharersSecond, []string{"agent-second"}) {
					t.Errorf("branch %q state = (%v, %q), want ([agent-second], %q)", second, sharersSecond, gotWtSecond, wtSecond)
				}

				foundBranch, foundWT, found, err := FindBranchForAgent(base, "", "agent-second")
				if err != nil || !found || foundBranch != second || foundWT != wtSecond {
					t.Errorf("FindBranchForAgent(agent-second) = (%q, %q, %v, %v), want (%q, %q, true, nil)", foundBranch, foundWT, found, err, second, wtSecond)
				}

				if _, _, err := UnregisterSharer(base, "", second, "agent-second"); err != nil {
					t.Fatalf("UnregisterSharer(%q): %v", second, err)
				}
				// First branch must remain completely untouched.
				sharersFirst, gotWtFirst, err = ListSharers(base, "", first)
				if err != nil || gotWtFirst != wtFirst || !slices.Equal(sharersFirst, []string{"agent-first"}) {
					t.Errorf("branch %q after unregistering %q = (%v, %q, %v), want ([agent-first], %q, nil)", first, second, sharersFirst, gotWtFirst, err, wtFirst)
				}
			})
		}
	}
}

func TestSharerMarkers_WindowsAndCrossPlatformFilenameSafety(t *testing.T) {
	winReservedStems := map[string]bool{
		"CON": true, "PRN": true, "AUX": true, "NUL": true,
		"CONIN$": true, "CONOUT$": true, "CLOCK$": true,
		"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
		"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
		"COM¹": true, "COM²": true, "COM³": true,
		"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
		"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
		"LPT¹": true, "LPT²": true, "LPT³": true,
	}

	var inputs []string
	inputs = append(inputs,
		"", ".", "..", "-", "--", "_", "%", "%63on", "%2563on", "%h-deadbeef",
		"con", "prn", "aux", "nul",
		"CON", "PRN", "AUX", "NUL",
		"Con", "Prn", "Aux", "Nul", "cOn",
		"con.txt", "aux.json", "nul ", "prn.", "con/sub", "sub/nul",
		"conin$", "CONIN$", "conout$", "CONOUT$", "clock$",
		"com¹", "com²", "com³", "lpt¹", "lpt²", "lpt³",
		"COM¹", "LPT³",
		"co", "con1", "cona", "com", "com10", "lpt", "lpt10", "aux0", "nula",
		"a~1", "featur~1", "a:b", `a\b`, "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b",
		"a\x00b", "a\x1fb", "a\x7fb", "caf\u00e9", "cafe\u0301",
		strings.Repeat("a", 99),
		strings.Repeat("a", 100),
		strings.Repeat("a", 101),
		strings.Repeat("a", 200),
		strings.Repeat("a", 300),
		"feature/"+strings.Repeat("x", 130)+"/"+strings.Repeat("y", 130),
	)
	for i := 0; i <= 9; i++ {
		inputs = append(inputs,
			fmt.Sprintf("com%d", i),
			fmt.Sprintf("COM%d", i),
			fmt.Sprintf("Com%d", i),
			fmt.Sprintf("%%63om%d", i),
			fmt.Sprintf("lpt%d", i),
			fmt.Sprintf("LPT%d", i),
			fmt.Sprintf("Lpt%d", i),
			fmt.Sprintf("%%6cpt%d", i),
		)
	}

	seenFold := make(map[string]string, len(inputs))
	base := "/repo"
	for _, branch := range inputs {
		full := sharerPath(base, branch)
		name := filepath.Base(full)
		if !strings.HasSuffix(name, ".json") {
			t.Fatalf("branch %q -> %q missing .json suffix", branch, name)
		}
		// Keep component + writeMarkerAtomic's ".tmp-4294967295" well below
		// 143B (eCryptfs), 255B (NAME_MAX), and within Win32 MAX_PATH budget.
		if len(name) > 105 {
			t.Errorf("branch %q -> filename %q (%d bytes) exceeds 105-byte cap", branch, name, len(name))
		}
		if name == "." || name == ".." {
			t.Errorf("branch %q -> dot path component %q", branch, name)
		}
		for i := 0; i < len(name); i++ {
			c := name[i]
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '%' || c == '-' || c == '_' || c == '.'
			if !ok {
				t.Errorf("branch %q -> filename %q contains unsafe byte 0x%02x", branch, name, c)
			}
		}
		stem, _, _ := strings.Cut(name, ".")
		if stem == "" {
			t.Errorf("branch %q -> filename %q has empty stem before '.'", branch, name)
		}
		if winReservedStems[strings.ToUpper(stem)] {
			t.Errorf("branch %q -> filename %q uses Windows reserved device stem %q", branch, name, stem)
		}
		folded := strings.ToLower(name)
		if prev, exists := seenFold[folded]; exists && prev != branch {
			t.Errorf("case-insensitive collision: branch %q and %q both map to %q", prev, branch, folded)
		}
		seenFold[folded] = branch
	}

	// Pre-existing Unix legacy marker for a reserved stem (e.g. aux.json) must
	// still resolve and migrate cleanly to %61ux.json.
	repoBase := setupBase(t)
	wtAux := inTreeWT(repoBase, "wt-aux")
	if err := os.MkdirAll(filepath.Join(wtAux, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyAux := filepath.Join(repoBase, ".git", sharerDir, "aux.json")
	if err := writeMarkerAtomic(legacyAux, &sharerMarker{
		Branch:       "aux",
		WorktreePath: wtAux,
		Sharers:      []string{"agent-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if sharers, gotWT, err := ListSharersForJoin(repoBase, "aux"); err != nil || gotWT != wtAux || !slices.Equal(sharers, []string{"agent-1"}) {
		t.Fatalf("ListSharersForJoin(aux) on legacy aux.json = (%v, %q, %v), want ([agent-1], %q, nil)", sharers, gotWT, err, wtAux)
	}
	if foundBranch, foundWT, found, err := FindBranchForAgent(repoBase, "", "agent-1"); err != nil || !found || foundBranch != "aux" || foundWT != wtAux {
		t.Fatalf("FindBranchForAgent on legacy aux.json = (%q, %q, %v, %v), want (\"aux\", %q, true, nil)", foundBranch, foundWT, found, err, wtAux)
	}
	if err := RegisterSharer(repoBase, "", "aux", wtAux, "agent-2"); err != nil {
		t.Fatalf("RegisterSharer(aux): %v", err)
	}
	if _, err := os.Stat(legacyAux); !os.IsNotExist(err) {
		t.Errorf("legacy aux.json should be removed after migration, stat err = %v", err)
	}
	if _, err := os.Stat(sharerPath(repoBase, "aux")); err != nil {
		t.Errorf("canonical %%61ux.json should exist after migration: %v", err)
	}
}

func TestSharerMarkers_RecordedBranchMismatchRefused(t *testing.T) {
	base := setupBase(t)
	projectDir := filepath.Join(base, ".scion")
	wt := inTreeWT(base, "wt-1")
	wtVictim := filepath.Join(projectDir, "agents", "victim", "workspace")
	for _, d := range []string{wt, wtVictim} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Register a real marker on unrelated-branch with a ProvisionAgent-shaped
	// worktree path so we can verify directory scans do not rewrite/clobber it.
	if err := RegisterSharer(base, projectDir, "unrelated-branch", wtVictim, "victim"); err != nil {
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

	// Directory scans (FindBranchForAgent and UnregisterSharerElsewhere) must
	// skip the mismatched marker rather than trusting its recorded Branch or
	// rewriting the victim branch's real marker.
	if gotBranch, gotWT, found, err := FindBranchForAgent(base, projectDir, "agent-1"); err != nil || found {
		t.Errorf("FindBranchForAgent(agent-1) = (%q, %q, %v, %v), want (\"\", \"\", false, nil)", gotBranch, gotWT, found, err)
	}
	rewritten, err := UnregisterSharerElsewhere(base, "agent-1", "keep")
	if err != nil || len(rewritten) != 0 {
		t.Errorf("UnregisterSharerElsewhere(agent-1) = (%v, %v), want ([], nil)", rewritten, err)
	}
	victimSharers, gotVictimWT, err := ListSharers(base, projectDir, "unrelated-branch")
	if err != nil || gotVictimWT != wtVictim || !slices.Equal(victimSharers, []string{"victim"}) {
		t.Errorf("unrelated-branch marker after UnregisterSharerElsewhere = (%v, %q, %v), want ([victim], %q, nil)", victimSharers, gotVictimWT, err, wtVictim)
	}

	// Also verify a marker at agent.json missing a recorded Branch ("") fails
	// closed for "agent" rather than being treated as a displaced legacy marker
	// for "", and is skipped by directory scans.
	if err := writeMarkerAtomic(sharerPath(base, "agent"), &sharerMarker{
		Branch:       "",
		WorktreePath: wt,
		Sharers:      []string{"agent-empty"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSharer(base, "", "agent", wt, "agent-real"); err == nil {
		t.Error("RegisterSharer(agent) on empty-branch marker: expected error, got nil")
	}
	if gotBranch, _, found, err := FindBranchForAgent(base, "", "agent-empty"); err != nil || found {
		t.Errorf("FindBranchForAgent(agent-empty) = (%q, %v, %v), want (\"\", false, nil)", gotBranch, found, err)
	}
	if isLegacyMarkerAtPath(base, sharerPath(base, "plain"), "plain") {
		t.Error("isLegacyMarkerAtPath should be false when canonical and legacy paths match")
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

	t.Run("legacy marker for one escaped branch is invisible to another escaped branch with same sanitized name", func(t *testing.T) {
		base := setupBase(t)
		wtSlash := inTreeWT(base, "wt-slash")
		wtDots := inTreeWT(base, "wt-dots")
		for _, wt := range []string{wtSlash, wtDots} {
			if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		legacyPath := filepath.Join(base, ".git", sharerDir, "feature-x.json")
		if err := writeMarkerAtomic(legacyPath, &sharerMarker{
			Branch:       "feature/x",
			WorktreePath: wtSlash,
			Sharers:      []string{"agent-1"},
		}); err != nil {
			t.Fatal(err)
		}

		// Both "feature..x" and "feature:x" have absent canonical markers and
		// fall back to legacyPath ("feature-x.json"), whose recorded Branch is
		// "feature/x". None of ListSharersForJoin, ListSharers, UnregisterSharer,
		// or RegisterSharer may confuse it for their own marker.
		for _, other := range []string{"feature..x", "feature:x"} {
			if sharers, gotWT, err := ListSharersForJoin(base, other); err != nil || len(sharers) != 0 || gotWT != "" {
				t.Fatalf("ListSharersForJoin(%q) = (%v, %q, %v), want empty", other, sharers, gotWT, err)
			}
			if sharers, gotWT, err := ListSharers(base, "", other); err != nil || len(sharers) != 0 || gotWT != "" {
				t.Fatalf("ListSharers(%q) = (%v, %q, %v), want empty", other, sharers, gotWT, err)
			}
			if rem, gotWT, err := UnregisterSharer(base, "", other, "agent-1"); err != nil || rem != nil || gotWT != "" {
				t.Fatalf("UnregisterSharer(%q) = (%v, %q, %v), want (nil, \"\", nil)", other, rem, gotWT, err)
			}
		}

		if err := RegisterSharer(base, "", "feature..x", wtDots, "agent-2"); err != nil {
			t.Fatalf("RegisterSharer(feature..x): %v", err)
		}
		if sharersSlash, gotSlashWT, err := ListSharers(base, "", "feature/x"); err != nil || gotSlashWT != wtSlash || !slices.Equal(sharersSlash, []string{"agent-1"}) {
			t.Fatalf("ListSharers(feature/x) = (%v, %q, %v), want ([agent-1], %q, nil)", sharersSlash, gotSlashWT, err, wtSlash)
		}
		if sharersDots, gotDotsWT, err := ListSharers(base, "", "feature..x"); err != nil || gotDotsWT != wtDots || !slices.Equal(sharersDots, []string{"agent-2"}) {
			t.Fatalf("ListSharers(feature..x) = (%v, %q, %v), want ([agent-2], %q, nil)", sharersDots, gotDotsWT, err, wtDots)
		}
	})

	t.Run("superseded legacy marker is ignored by directory scans", func(t *testing.T) {
		base := setupBase(t)
		wtSlash := inTreeWT(base, "wt-slash")
		wtOther := inTreeWT(base, "wt-other")
		for _, wt := range []string{wtSlash, wtOther} {
			if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		// Simulate an interrupted migration where canonical feature%2fx.json
		// was updated (removing agent-1, keeping agent-2) before the stale
		// legacy feature-x.json (still listing agent-1 and agent-2) was deleted.
		if err := writeMarkerAtomic(sharerPath(base, "feature/x"), &sharerMarker{
			Branch:       "feature/x",
			WorktreePath: wtSlash,
			Sharers:      []string{"agent-2"},
		}); err != nil {
			t.Fatal(err)
		}
		staleLegacy := legacySharerPath(base, "feature/x")
		if err := writeMarkerAtomic(staleLegacy, &sharerMarker{
			Branch:       "feature/x",
			WorktreePath: wtSlash,
			Sharers:      []string{"agent-1", "agent-2"},
		}); err != nil {
			t.Fatal(err)
		}

		// agent-1 is only in the superseded legacy file, so FindBranchForAgent
		// must report found=false.
		if gotBranch, gotWT, found, err := FindBranchForAgent(base, "", "agent-1"); err != nil || found {
			t.Fatalf("FindBranchForAgent(agent-1) with stale legacy only = (%q, %q, %v, %v), want (\"\", \"\", false, nil)", gotBranch, gotWT, found, err)
		}

		// If agent-1 is legitimately registered on a branch sorting after
		// feature-x.json, FindBranchForAgent must skip feature-x.json and find it.
		if err := RegisterSharer(base, "", "other-branch", wtOther, "agent-1"); err != nil {
			t.Fatal(err)
		}
		if gotBranch, gotWT, found, err := FindBranchForAgent(base, "", "agent-1"); err != nil || !found || gotBranch != "other-branch" || gotWT != wtOther {
			t.Fatalf("FindBranchForAgent(agent-1) = (%q, %q, %v, %v), want (\"other-branch\", %q, true, nil)", gotBranch, gotWT, found, err, wtOther)
		}

		// UnregisterSharerElsewhere for agent-2 deletes canonical feature%2fx.json
		// (last sharer) and cleans up the stale legacy feature-x.json so neither
		// the remainder of the scan nor subsequent lookups resurrect agent-1.
		if _, err := UnregisterSharerElsewhere(base, "agent-2", "keep"); err != nil {
			t.Fatalf("UnregisterSharerElsewhere(agent-2): %v", err)
		}
		if _, err := os.Stat(sharerPath(base, "feature/x")); !os.IsNotExist(err) {
			t.Errorf("canonical feature%%2fx.json should remain deleted, stat err = %v", err)
		}
		if _, err := os.Stat(staleLegacy); !os.IsNotExist(err) {
			t.Errorf("stale legacy feature-x.json should be cleaned up, stat err = %v", err)
		}
		if sharers, gotWT, err := ListSharers(base, "", "feature/x"); err != nil || len(sharers) != 0 || gotWT != "" {
			t.Errorf("ListSharers(feature/x) after last unregister = (%v, %q, %v), want empty", sharers, gotWT, err)
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
