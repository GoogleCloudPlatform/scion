/*
Copyright 2026 The Scion Authors.
*/

package agent

import (
	"os"
	"os/exec"
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
		if got := isAgentOwnWorkspace(tt.agentDir, filepath.Join(root, "repo", ".scion"), tt.workspace); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A worktree provisioning attached the agent to, outside <agentDir>/workspace,
// counts as its own only through the broker's record in agentDir, and only
// when the recorded path is a registered linked worktree of the project's
// repository that is neither the project or repository root nor contains
// either. Anything else fails closed (not own, so read-only).
func TestIsAgentOwnWorkspace_RecordedAttachedWorktree(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	scionDir := filepath.Join(repo, ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "init")
	attached := filepath.Join(scionDir, "agents", "a", "workspace")
	git("worktree", "add", "-q", "-b", "a", attached)
	plainDir := filepath.Join(root, "not-a-worktree")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(root, "state", "agents", "a")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if isAgentOwnWorkspace(agentDir, scionDir, attached) {
		t.Fatal("attached worktree is own before any record")
	}
	for _, tt := range []struct {
		name     string
		recorded string
		want     bool
	}{
		{"registered linked worktree", attached, true},
		{"the project and repository root", repo, false},
		{"a directory containing the repository", root, false},
		{"a directory that is not a worktree", plainDir, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := writeAgentOwnWorkspace(agentDir, tt.recorded); err != nil {
				t.Fatal(err)
			}
			if got := isAgentOwnWorkspace(agentDir, scionDir, tt.recorded); got != tt.want {
				t.Errorf("recorded %s: own = %v, want %v", tt.recorded, got, tt.want)
			}
		})
	}
	// A record that names another path does not make this workspace own.
	if err := writeAgentOwnWorkspace(agentDir, attached); err != nil {
		t.Fatal(err)
	}
	if isAgentOwnWorkspace(agentDir, scionDir, plainDir) {
		t.Error("an unrecorded workspace counts as own")
	}
	if err := os.WriteFile(filepath.Join(agentDir, agentOwnWorkspaceFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isAgentOwnWorkspace(agentDir, scionDir, attached) {
		t.Error("an unreadable record counts (must fail closed)")
	}
}
