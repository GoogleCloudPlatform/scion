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
	} {
		if got := isAgentOwnWorkspace(tt.agentDir, tt.workspace); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
