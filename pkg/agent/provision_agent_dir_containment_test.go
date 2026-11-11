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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestGetAgent_RejectsNameOutsideSelectedRoot is the regression anchor for
// the containment check GetAgent runs (via checkAgentDirContained)
// immediately after agentDir is computed and before the stale-directory
// branch that removes it. GetAgent's caller is not always a validated path
// element (defense in depth for createAgent's own check), so GetAgent must
// independently confirm agentDir is still a direct child of the root
// config.GetAgentDir selected it under before doing anything with it.
//
// This matters under both root selections config.SelectAgentsRoot can
// return -- the external agents dir when the caller is in shared-workspace
// mode and a project-id marker exists, or <projectDir>/agents otherwise --
// because a check hardcoded to one of them would be wrong for the other
// (see config.SelectAgentsRoot's doc comment). Each subtest below plants a
// sentinel directory exactly where a name of "../sibling" would land for
// that root selection, and confirms GetAgent errors out before touching it.
func TestGetAgent_RejectsNameOutsideSelectedRoot(t *testing.T) {
	const outOfRootName = "../sibling"

	t.Run("default root (<projectDir>/agents)", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		scionDir := filepath.Join(tmpDir, "project", ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("MkdirAll scionDir: %v", err)
		}

		// A "../sibling" name joined onto <scionDir>/agents lands here.
		sentinel := filepath.Join(scionDir, "sibling")
		if err := os.MkdirAll(sentinel, 0755); err != nil {
			t.Fatalf("MkdirAll sentinel: %v", err)
		}
		markerPath := filepath.Join(sentinel, "marker.txt")
		if err := os.WriteFile(markerPath, []byte("keep"), 0644); err != nil {
			t.Fatalf("WriteFile marker: %v", err)
		}

		_, _, _, _, err := GetAgent(context.Background(), outOfRootName, "", "", "", scionDir, "", "", "", "")
		if err == nil {
			t.Fatal("expected GetAgent to reject a name resolving outside the agents root, got nil error")
		}
		if !strings.Contains(err.Error(), "is not a single path element under") {
			t.Fatalf("expected error to mention 'is not a single path element under' (the containment check), got: %v", err)
		}
		if _, statErr := os.Stat(markerPath); statErr != nil {
			t.Fatalf("sentinel must survive untouched, but stat failed: %v", statErr)
		}
	})

	t.Run("external root (shared workspace, project-id marker present)", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)

		scionDir := filepath.Join(tmpDir, "project", ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("MkdirAll scionDir: %v", err)
		}
		if err := config.WriteProjectID(scionDir, "11111111-2222-3333-4444-555555555555"); err != nil {
			t.Fatalf("WriteProjectID: %v", err)
		}

		externalAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir)
		if err != nil {
			t.Fatalf("GetGitProjectExternalAgentsDir: %v", err)
		}
		if externalAgentsDir == "" {
			t.Fatal("expected a non-empty external agents dir once a project-id marker is present")
		}

		// A "../sibling" name joined onto the external agents dir lands here,
		// a sibling of "agents" under the external .scion directory -- a
		// different location than the default-root case above, which is
		// exactly why the containment check must recompute the selected
		// root rather than assume <projectDir>/agents.
		sentinel := filepath.Join(filepath.Dir(externalAgentsDir), "sibling")
		if err := os.MkdirAll(sentinel, 0755); err != nil {
			t.Fatalf("MkdirAll sentinel: %v", err)
		}
		markerPath := filepath.Join(sentinel, "marker.txt")
		if err := os.WriteFile(markerPath, []byte("keep"), 0644); err != nil {
			t.Fatalf("WriteFile marker: %v", err)
		}

		ctx := api.ContextWithSharedWorkspace(context.Background())
		_, _, _, _, err = GetAgent(ctx, outOfRootName, "", "", "", scionDir, "", "", "", "")
		if err == nil {
			t.Fatal("expected GetAgent to reject a name resolving outside the selected (external) agents root, got nil error")
		}
		if !strings.Contains(err.Error(), "is not a single path element under") {
			t.Fatalf("expected error to mention 'is not a single path element under' (the containment check), got: %v", err)
		}
		if _, statErr := os.Stat(markerPath); statErr != nil {
			t.Fatalf("sentinel must survive untouched, but stat failed: %v", statErr)
		}
	})
}

// TestProvisionAgent_RejectsNameOutsideSelectedRoot covers ProvisionAgent
// the same way TestGetAgent_RejectsNameOutsideSelectedRoot covers GetAgent.
// AgentManager.Reprovision calls ProvisionAgent directly, not GetAgent, so
// the containment check must run on the provision path too:
// checkAgentDirContained is shared between both functions, and this test
// exercises it through ProvisionAgent -- same two root selections, with the
// sentinel's "workspace" subdirectory mirroring what ProvisionAgent's
// git-clone and worktree branches would otherwise remove under agentDir.
func TestProvisionAgent_RejectsNameOutsideSelectedRoot(t *testing.T) {
	const outOfRootName = "../sibling"

	t.Run("default root (<projectDir>/agents)", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		scionDir := filepath.Join(tmpDir, "project", ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("MkdirAll scionDir: %v", err)
		}

		sentinel := filepath.Join(scionDir, "sibling", "workspace")
		if err := os.MkdirAll(sentinel, 0755); err != nil {
			t.Fatalf("MkdirAll sentinel: %v", err)
		}
		markerPath := filepath.Join(sentinel, "marker.txt")
		if err := os.WriteFile(markerPath, []byte("keep"), 0644); err != nil {
			t.Fatalf("WriteFile marker: %v", err)
		}

		_, _, _, err := ProvisionAgent(context.Background(), outOfRootName, "", "", "", scionDir, "", "", "", "")
		if err == nil {
			t.Fatal("expected ProvisionAgent to reject a name resolving outside the agents root, got nil error")
		}
		if !strings.Contains(err.Error(), "is not a single path element under") {
			t.Fatalf("expected error to mention 'is not a single path element under' (the containment check), got: %v", err)
		}
		if _, statErr := os.Stat(markerPath); statErr != nil {
			t.Fatalf("sentinel must survive untouched, but stat failed: %v", statErr)
		}
	})

	t.Run("external root (shared workspace, project-id marker present)", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)

		scionDir := filepath.Join(tmpDir, "project", ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("MkdirAll scionDir: %v", err)
		}
		if err := config.WriteProjectID(scionDir, "11111111-2222-3333-4444-555555555555"); err != nil {
			t.Fatalf("WriteProjectID: %v", err)
		}

		externalAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir)
		if err != nil {
			t.Fatalf("GetGitProjectExternalAgentsDir: %v", err)
		}
		if externalAgentsDir == "" {
			t.Fatal("expected a non-empty external agents dir once a project-id marker is present")
		}

		sentinel := filepath.Join(filepath.Dir(externalAgentsDir), "sibling", "workspace")
		if err := os.MkdirAll(sentinel, 0755); err != nil {
			t.Fatalf("MkdirAll sentinel: %v", err)
		}
		markerPath := filepath.Join(sentinel, "marker.txt")
		if err := os.WriteFile(markerPath, []byte("keep"), 0644); err != nil {
			t.Fatalf("WriteFile marker: %v", err)
		}

		ctx := api.ContextWithSharedWorkspace(context.Background())
		_, _, _, err = ProvisionAgent(ctx, outOfRootName, "", "", "", scionDir, "", "", "", "")
		if err == nil {
			t.Fatal("expected ProvisionAgent to reject a name resolving outside the selected (external) agents root, got nil error")
		}
		if !strings.Contains(err.Error(), "is not a single path element under") {
			t.Fatalf("expected error to mention 'is not a single path element under' (the containment check), got: %v", err)
		}
		if _, statErr := os.Stat(markerPath); statErr != nil {
			t.Fatalf("sentinel must survive untouched, but stat failed: %v", statErr)
		}
	})
}

// TestCheckAgentDirContained_RejectsNameThatCleansToADirectChild covers the
// case Dir(Clean(agentDir)) == root alone would miss: a multi-segment name
// like "a/../b" cleans down to <root>/b, a direct child of root, even though
// "a/../b" is not the single path element "b" it collapses to. Base(agentDir)
// must equal agentName too, or a name shaped like this would pass the
// direct-child check while still not being what createAgent's
// isSingleCleanPathElement requires at the request boundary.
func TestCheckAgentDirContained_RejectsNameThatCleansToADirectChild(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("MkdirAll projectDir: %v", err)
	}

	if _, err := checkAgentDirContained(projectDir, "a/../b", false); err == nil {
		t.Fatal("expected checkAgentDirContained to reject a name that cleans to a direct child but isn't one, got nil error")
	} else if !strings.Contains(err.Error(), "is not a single path element under") {
		t.Fatalf("expected error to mention 'is not a single path element under', got: %v", err)
	}

	// A genuinely single-element name must still pass.
	agentDir, err := checkAgentDirContained(projectDir, "b", false)
	if err != nil {
		t.Fatalf("expected a single-element name to be accepted, got: %v", err)
	}
	want := filepath.Join(projectDir, "agents", "b")
	if agentDir != want {
		t.Fatalf("expected agentDir %q, got %q", want, agentDir)
	}
}

// TestDeleteAgentFiles_RejectsNameOutsideProjectRoot is the regression anchor
// for DeleteAgentFiles's containment check: unlike GetAgent and
// ProvisionAgent, DeleteAgentFiles joined agentName onto its candidate
// directories (the project's agents dir, the global agents dir, the
// external per-agent state dir, and the shared worktree base) without ever
// confirming the join stayed a direct, single-element child of the root it
// was joined onto. This creates a sentinel directory exactly where
// "../sibling" would land, and confirms DeleteAgentFiles rejects the call --
// and leaves the sentinel untouched -- before doing anything to the
// filesystem.
func TestDeleteAgentFiles_RejectsNameOutsideProjectRoot(t *testing.T) {
	const outOfRootName = "../sibling"

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "project")
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0755); err != nil {
		t.Fatalf("MkdirAll scionDir/agents: %v", err)
	}

	// A "../sibling" name joined onto <scionDir>/agents lands here, a
	// sibling of "agents" under scionDir.
	sentinel := filepath.Join(scionDir, "sibling")
	if err := os.MkdirAll(sentinel, 0755); err != nil {
		t.Fatalf("MkdirAll sentinel: %v", err)
	}
	markerPath := filepath.Join(sentinel, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("keep"), 0644); err != nil {
		t.Fatalf("WriteFile marker: %v", err)
	}

	if _, err := DeleteAgentFiles(outOfRootName, scionDir, false); err == nil {
		t.Fatal("expected DeleteAgentFiles to reject a name resolving outside the agents root, got nil error")
	} else if !strings.Contains(err.Error(), "is not a single path element under") {
		t.Fatalf("expected error to mention 'is not a single path element under' (the containment check), got: %v", err)
	}

	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Fatalf("sentinel must survive untouched, but stat failed: %v", statErr)
	}
}
