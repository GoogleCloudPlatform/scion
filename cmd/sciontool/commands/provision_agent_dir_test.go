/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// newTestAgentDir returns <tmp>/projects/proj-1/agents/agent-1, created, as
// the broker prepares it before the pod starts.
func newTestAgentDir(t *testing.T) string {
	t.Helper()
	agentDir := filepath.Join(t.TempDir(), "projects", "proj-1", "agents", "agent-1")
	if err := os.MkdirAll(filepath.Join(agentDir, provision.AgentWorkspaceDir), 0o770); err != nil {
		t.Fatal(err)
	}
	return agentDir
}

// setAgentDirEnv sets the env the Kubernetes runtime passes to the
// provisioning init container in clone-per-agent mode.
func setAgentDirEnv(t *testing.T, agentSlug, branch string) {
	t.Helper()
	t.Setenv("SCION_WORKSPACE_MODE", "clone-per-agent")
	t.Setenv("SCION_AGENT_SLUG", agentSlug)
	t.Setenv("SCION_AGENT_BRANCH", branch)
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Clone-per-agent: the init container prepares the agent's directory and
// leaves the workspace empty for the agent container's clone. It does not
// clone, even when clone settings are in its environment, and the sentinel
// and the branch record are written next to the workspace, not in it.
func TestRunProvision_AgentDirMode_PreparesEmptyWorkspace(t *testing.T) {
	origin := provisionTestRepo(t)
	agentDir := newTestAgentDir(t)
	setupProvisionCmd(t, agentDir, "shared-plain", origin)
	setAgentDirEnv(t, "agent-1", "scion/agent-1")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if names := entryNames(t, filepath.Join(agentDir, provision.AgentWorkspaceDir)); len(names) != 0 {
		t.Errorf("workspace should stay empty for the agent container's clone, has %v", names)
	}
	if _, err := os.Stat(filepath.Join(agentDir, provision.ProvisionSentinelFile)); err != nil {
		t.Errorf("sentinel not written in the agent directory: %v", err)
	}
	if got := readTestFile(t, filepath.Join(agentDir, provision.AgentBranchFile)); strings.TrimSpace(got) != "scion/agent-1" {
		t.Errorf("branch record = %q", got)
	}

	// A restart on the same branch reuses the workspace as it is.
	ws := filepath.Join(agentDir, provision.AgentWorkspaceDir)
	if err := os.WriteFile(filepath.Join(ws, "work.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := readTestFile(t, filepath.Join(ws, "work.txt")); got != "kept" {
		t.Errorf("kept file = %q", got)
	}

	// A start on another branch with the kept workspace is refused.
	t.Setenv("SCION_AGENT_BRANCH", "feature/other")
	err := runProvision(context.Background())
	if !errors.Is(err, provision.ErrAgentBranchMismatch) {
		t.Fatalf("want ErrAgentBranchMismatch, got %v", err)
	}
}

// The --mode flag selects clone-per-agent too, as for the other modes.
func TestRunProvision_AgentDirMode_FromFlag(t *testing.T) {
	agentDir := newTestAgentDir(t)
	setupProvisionCmd(t, agentDir, "clone-per-agent", "")
	t.Setenv("SCION_AGENT_SLUG", "agent-1")
	t.Setenv("SCION_AGENT_BRANCH", "scion/agent-1")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, provision.AgentBranchFile)); err != nil {
		t.Errorf("branch record not written: %v", err)
	}
}

// The agent's slug and branch must be set; nothing is written otherwise.
func TestRunProvision_AgentDirMode_NeedsSlugAndBranch(t *testing.T) {
	for _, tc := range []struct{ slug, branch string }{
		{"", "scion/agent-1"},
		{"..", "scion/agent-1"},
		{"a/b", "scion/agent-1"},
		{"Agent-1", "scion/agent-1"},
		{"agent-1", ""},
	} {
		agentDir := newTestAgentDir(t)
		setupProvisionCmd(t, agentDir, "shared-plain", "")
		setAgentDirEnv(t, tc.slug, tc.branch)
		err := runProvision(context.Background())
		switch {
		case err == nil:
			t.Errorf("slug %q branch %q: want an error", tc.slug, tc.branch)
		case tc.branch != "" && !strings.Contains(err.Error(), "needs SCION_AGENT_SLUG"):
			t.Errorf("slug %q: want the SCION_AGENT_SLUG error, got %v", tc.slug, err)
		}
		if names := entryNames(t, agentDir); len(names) != 1 || names[0] != provision.AgentWorkspaceDir {
			t.Errorf("slug %q branch %q: agent directory changed: %v", tc.slug, tc.branch, names)
		}
	}
}

// An older sciontool that does not know the clone-per-agent env sees the
// pod as a project without a clone URL: it prepares the mounted agent
// directory as a plain workspace (the sentinel goes there, outside the
// agent's workspace), and the workspace the broker created stays empty
// for the agent container's clone.
func TestRunProvision_AgentDir_OlderImageFallback(t *testing.T) {
	agentDir := newTestAgentDir(t)
	setupProvisionCmd(t, agentDir, "shared-plain", "")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, provision.ProvisionSentinelFile)); err != nil {
		t.Errorf("sentinel not written in the agent directory: %v", err)
	}
	if names := entryNames(t, filepath.Join(agentDir, provision.AgentWorkspaceDir)); len(names) != 0 {
		t.Errorf("workspace should stay empty, has %v", names)
	}
}
