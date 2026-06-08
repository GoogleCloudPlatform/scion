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

func TestRegisterAndListSharers(t *testing.T) {
	base := setupBase(t)
	branch := "feature/foo"
	wt := "/workspace/wt-foo"

	if err := RegisterSharer(base, branch, wt, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSharer(base, branch, wt, "agent-2"); err != nil {
		t.Fatal(err)
	}

	sharers, path, err := ListSharers(base, branch)
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
	wt := "/workspace/wt-bar"

	RegisterSharer(base, branch, wt, "agent-1")
	RegisterSharer(base, branch, wt, "agent-2")

	remaining, path, err := UnregisterSharer(base, branch, "agent-1")
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
	wt := "/workspace/wt-baz"

	RegisterSharer(base, branch, wt, "agent-1")

	remaining, path, err := UnregisterSharer(base, branch, "agent-1")
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

	RegisterSharer(base, "feature/alpha", "/wt/alpha", "agent-A")
	RegisterSharer(base, "feature/beta", "/wt/beta", "agent-B")

	branch, wt, found, err := FindBranchForAgent(base, "agent-A")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected found=true for agent-A")
	}
	if branch != "feature/alpha" {
		t.Errorf("branch = %q, want %q", branch, "feature/alpha")
	}
	if wt != "/wt/alpha" {
		t.Errorf("worktreePath = %q, want %q", wt, "/wt/alpha")
	}

	_, _, found, err = FindBranchForAgent(base, "agent-missing")
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
	wt := "/wt/idem"

	RegisterSharer(base, branch, wt, "agent-1")
	RegisterSharer(base, branch, wt, "agent-1")
	RegisterSharer(base, branch, wt, "agent-1")

	sharers, _, err := ListSharers(base, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(sharers) != 1 {
		t.Errorf("len(sharers) = %d after idempotent register, want 1", len(sharers))
	}
}

func TestListSharers_NoMarker(t *testing.T) {
	base := setupBase(t)

	sharers, path, err := ListSharers(base, "nonexistent-branch")
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

	remaining, path, err := UnregisterSharer(base, "nonexistent", "agent-1")
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
	wt := "/wt/noop"

	RegisterSharer(base, branch, wt, "agent-1")

	remaining, path, err := UnregisterSharer(base, branch, "agent-unknown")
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

func TestFindBranchForAgent_NoDir(t *testing.T) {
	base := setupBase(t)

	_, _, found, err := FindBranchForAgent(base, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("expected found=false when scion-sharers dir does not exist")
	}
}
