/*
Copyright 2026 The Scion Authors.
*/

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

// An agent created with --branch naming the branch the user has checked
// out attaches to the main checkout, which holds the project's .scion. No
// record is written, so the workspace is not the agent's own and its .scion
// is mounted read-only in every layout (repo root, subdirectory, plain, k8s
// NFS: all consume AgentOwnWorkspace). A provision that attaches nothing
// clears an earlier record.
func TestProvisionAgent_MainCheckoutAttachNotRecorded(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	projectDir := filepath.Dir(scionDir)
	out, err := exec.Command("git", "-C", projectDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	checkedOut := strings.TrimSpace(string(out))

	agentDir := config.ResolveAgentDir(scionDir, "main-attach")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A record left by an earlier provision must not survive either.
	if err := writeAgentOwnWorkspace(agentDir, projectDir); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ProvisionAgent(context.Background(), "main-attach", "default", "", "", scionDir, "", "created", checkedOut, ""); err != nil {
		t.Fatalf("provision with --branch %s: %v", checkedOut, err)
	}
	if got := readAgentOwnWorkspace(agentDir); got != "" {
		t.Errorf("record %q written for the main checkout", got)
	}
	if isAgentOwnWorkspace(agentDir, scionDir, projectDir) {
		t.Error("the main checkout counts as the agent's own")
	}

	// A provision that creates its own worktree clears a stale record.
	other := config.ResolveAgentDir(scionDir, "fresh")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentOwnWorkspace(other, projectDir); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ProvisionAgent(context.Background(), "fresh", "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("provision fresh: %v", err)
	}
	if got := readAgentOwnWorkspace(other); got != "" {
		t.Errorf("stale record %q kept after a provision that attached nothing", got)
	}
}

// A project kept in a subdirectory of the repository: the main checkout
// (the repository root) contains the project, so a record naming it is
// refused on read.
func TestIsAgentOwnWorkspace_SubdirProjectMainCheckoutRefused(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	scionDir := filepath.Join(repo, "sub", ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	agentDir := filepath.Join(root, "state", "agents", "a")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, recorded := range []string{repo, filepath.Join(repo, "sub")} {
		if err := writeAgentOwnWorkspace(agentDir, recorded); err != nil {
			t.Fatal(err)
		}
		if isAgentOwnWorkspace(agentDir, scionDir, recorded) {
			t.Errorf("recorded %s counts as own", recorded)
		}
	}
}
