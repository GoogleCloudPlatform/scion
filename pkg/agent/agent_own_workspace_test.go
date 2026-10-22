/*
Copyright 2026 The Scion Authors.
*/

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// isAgentOwnWorkspace compares the broker's own paths only: an agent-created
// .git file in a shared workspace does not make it the agent's own.
func TestIsAgentOwnWorkspace(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "repo", ".scion", "agents", "a")
	own := filepath.Join(agentDir, "workspace")
	shared := filepath.Join(root, "repo", "sub")
	for _, d := range []string{own, shared} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(own, link); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name                string
		agentDir, workspace string
		want                bool
	}{
		{"own worktree", agentDir, own, true},
		{"own worktree through a symlink", agentDir, link, true},
		{"shared subdirectory with a .git file", agentDir, shared, false},
		{"repo root", agentDir, filepath.Join(root, "repo"), false},
		{"no agent dir", "", own, false},
		{"no workspace", agentDir, "", false},
		{"another worktree with no record (fail closed)", agentDir, shared, false},
	} {
		if got := isAgentOwnWorkspace(tt.agentDir, tt.workspace); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A worktree provisioning attached the agent to, outside <agentDir>/workspace,
// counts as its own only through the broker's record in agentDir.
func TestIsAgentOwnWorkspace_RecordedAttachedWorktree(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "state", "agents", "a")
	attached := filepath.Join(root, "repo", ".scion", "agents", "a", "workspace")
	other := filepath.Join(root, "repo", "sub")
	for _, d := range []string{agentDir, attached, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if isAgentOwnWorkspace(agentDir, attached) {
		t.Fatal("attached worktree is own before any record")
	}
	if err := writeAgentOwnWorkspace(agentDir, attached); err != nil {
		t.Fatal(err)
	}
	if !isAgentOwnWorkspace(agentDir, attached) {
		t.Error("recorded attached worktree is not own")
	}
	if isAgentOwnWorkspace(agentDir, other) {
		t.Error("an unrecorded workspace counts as own")
	}
	if err := os.WriteFile(filepath.Join(agentDir, agentOwnWorkspaceFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isAgentOwnWorkspace(agentDir, attached) {
		t.Error("an unreadable record counts (must fail closed)")
	}
}
