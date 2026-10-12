// Copyright 2026 The Scion Authors.

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// readManifestAgentWorkspace returns agent_workspace from the staged
// container-script manifest in home.
func readManifestAgentWorkspace(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m struct {
		AgentWorkspace string `json:"agent_workspace"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m.AgentWorkspace
}

// The provisioner runs inside the container, so the manifest must name the
// workspace where the container sees it. A worktree under the repo root is
// mounted at /repo-root/<relative path>, not at its host path; a host path
// in agent_workspace makes the claude provisioner pre-trust the wrong
// directory and Claude Code blocks on its folder-trust dialog.
func TestStart_ManifestAgentWorkspaceIsContainerPathForWorktree(t *testing.T) {
	e := newPolicyTestEnv(t)
	if err := os.WriteFile(filepath.Join(e.root, ".gitignore"), []byte(".scion/agents/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, e.root)
	e.projectHC(t, "hc-scripted", policyTestScripted)

	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	})
	opts := api.StartOptions{Name: "wt-agent", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
	home := config.GetAgentHomePath(e.scion, "wt-agent")

	// A restart restages the manifest too, so both launches are checked.
	for i, label := range []string{"first start", "restart"} {
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if !strings.HasPrefix(captured.ContainerWorkspace, "/repo-root/") {
			t.Fatalf("fixture (%s): expected a worktree mounted under /repo-root, got ContainerWorkspace %q", label, captured.ContainerWorkspace)
		}
		got := readManifestAgentWorkspace(t, home)
		if got != captured.ContainerWorkspace {
			t.Errorf("launch %d (%s): manifest agent_workspace = %q, want the container workspace %q (host workspace %q)",
				i+1, label, got, captured.ContainerWorkspace, captured.Workspace)
		}
	}
}
