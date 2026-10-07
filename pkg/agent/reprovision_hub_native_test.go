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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const hubNativeTestProjectID = "44444444-5555-6666-7777-888888888888"

// makeHubNativeProject writes a hub-native project under $HOME: a .scion
// marker file in ~/.scion/projects/<slug>, which resolves to the external
// project-config dir. It returns the marker path and the resolved project
// dir, whose agents root is also the external agents root for the project ID.
func makeHubNativeProject(t *testing.T, home, slug, projectID string) (markerPath, projectDir string) {
	t.Helper()
	root := filepath.Join(home, ".scion", "projects", slug)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath = filepath.Join(root, config.DotScion)
	marker := &config.ProjectMarker{ProjectID: projectID, ProjectName: slug, ProjectSlug: slug}
	if err := config.WriteProjectMarker(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	projectDir, err := config.GetResolvedProjectDir(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	ext, err := config.AgentsRootForProject(projectDir, true, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(ext) != filepath.Join(projectDir, "agents") {
		t.Fatalf("fixture: hub-native external agents root %s should equal the project agents root %s", ext, filepath.Join(projectDir, "agents"))
	}
	return markerPath, projectDir
}

// In a hub-native project the external agents root is the project's own
// agents root, so an existing agent's scion-agent.json there says nothing
// about a shared workspace.
func TestEffectiveSharedWorkspace_HubNativeProjectIsNotShared(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, projectDir := makeHubNativeProject(t, home, "hn-proj", hubNativeTestProjectID)
	agentDir := filepath.Join(projectDir, "agents", "dev")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{hubNativeTestProjectID, ""} {
		if effectiveSharedWorkspace(projectDir, "dev", false, id) {
			t.Errorf("hubProjectID %q: an agent in a hub-native project must not be reported as shared-workspace", id)
		}
		dir, shared, err := agentStateDir(projectDir, "dev", false, id, true)
		if err != nil {
			t.Fatalf("hubProjectID %q: agentStateDir: %v", id, err)
		}
		if shared || dir != agentDir {
			t.Errorf("hubProjectID %q: agentStateDir = (%s, %v), want (%s, false)", id, dir, shared, agentDir)
		}
	}
	if !effectiveSharedWorkspace(projectDir, "dev", true, hubNativeTestProjectID) {
		t.Error("an explicit shared-workspace flag stays shared")
	}

	ctx := api.ContextWithHubProjectID(api.ContextWithBrokerMode(context.Background()), hubNativeTestProjectID)
	gotCtx, dir, shared, err := withAgentStateDir(ctx, projectDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if shared || dir != agentDir || api.IsSharedWorkspaceFromContext(gotCtx) {
		t.Errorf("withAgentStateDir = (%s, %v, ctx shared %v), want (%s, false, false)", dir, shared, api.IsSharedWorkspaceFromContext(gotCtx), agentDir)
	}
}

// With HOME behind a symlink, the canonical project dir (ResolveProjectPath
// runs EvalSymlinks on a .scion directory) and the external agents root
// derived from HOME are different paths for the same directory; the agent is
// still not reported as shared-workspace.
func TestEffectiveSharedWorkspace_HubNativeProjectViaSymlinkedHome(t *testing.T) {
	realHome := t.TempDir()
	home := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(realHome, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	_, linkedDir := makeHubNativeProject(t, home, "hn-proj", hubNativeTestProjectID)
	projectDir, err := filepath.EvalSymlinks(linkedDir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := config.GetResolvedProjectDir(projectDir); err != nil || resolved != projectDir {
		t.Fatalf("fixture: canonical project dir should resolve to itself: %q, %v", resolved, err)
	}
	ext, err := config.AgentsRootForProject(projectDir, true, hubNativeTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(ext) == filepath.Join(projectDir, "agents") {
		t.Fatal("fixture: expected the external root and the project agents root to differ as paths")
	}
	agentDir := filepath.Join(projectDir, "agents", "dev")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if effectiveSharedWorkspace(projectDir, "dev", false, hubNativeTestProjectID) {
		t.Error("an agent in a hub-native project reached via a symlinked HOME must not be reported as shared-workspace")
	}
}

// A linked project with a hub project ID keeps the external-root detection:
// its external agents root differs from the in-project one, and an agent
// whose external dir holds scion-agent.json is shared-workspace.
func TestEffectiveSharedWorkspace_LinkedProjectWithHubIDStillDetectsExternal(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	ext, err := config.AgentDirForProject(scionDir, "ext-agent", true, hubNativeTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(ext) == filepath.Join(scionDir, "agents") {
		t.Fatal("fixture: expected a distinct external agents root")
	}
	if effectiveSharedWorkspace(scionDir, "ext-agent", false, hubNativeTestProjectID) {
		t.Error("no external scion-agent.json: expected in-project")
	}
	if err := os.MkdirAll(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !effectiveSharedWorkspace(scionDir, "ext-agent", false, hubNativeTestProjectID) {
		t.Error("external scion-agent.json present: expected shared-workspace")
	}
}

// Reincarnating a clone-per-agent agent of a hub-native project keeps its
// per-agent workspace: before the fix, the agent was taken for a
// shared-workspace one, its workspace path was empty, and provisioning
// failed with "failed to create workspace dir: mkdir : no such file or
// directory".
func TestReprovision_HubNativeMarkerProject_GitCloneAgentKeepsPerAgentWorkspace(t *testing.T) {
	_, _ = reprovisionSetup(t)
	home := os.Getenv("HOME")
	markerPath, projectDir := makeHubNativeProject(t, home, "hn-proj", hubNativeTestProjectID)
	agentName := "clone-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}

	createCtx := api.ContextWithGitClone(context.Background(), gc)
	createCtx = api.ContextWithHubProjectID(api.ContextWithBrokerMode(createCtx), hubNativeTestProjectID)
	if _, _, _, err := ProvisionAgent(createCtx, agentName, "default", "", "", markerPath, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	agentDir := filepath.Join(projectDir, "agents", agentName)
	ws := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(ws, "uncommitted-work.txt")
	if err := os.WriteFile(work, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: markerPath, BrokerMode: true,
		GitClone: gc, HubProjectID: hubNativeTestProjectID,
	}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("clone workspace file lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "scion-agent.json")); err != nil {
		t.Fatalf("scion-agent.json missing after Reprovision: %v", err)
	}
}

// GetAgent on an existing clone-per-agent agent of a hub-native project
// returns the agent's own workspace, not an empty (shared-workspace) one.
func TestGetAgent_HubNativeMarkerProject_GitCloneAgentReturnsPerAgentWorkspace(t *testing.T) {
	_, _ = reprovisionSetup(t)
	home := os.Getenv("HOME")
	markerPath, projectDir := makeHubNativeProject(t, home, "hn-proj", hubNativeTestProjectID)
	agentName := "clone-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}

	ctx := api.ContextWithGitClone(context.Background(), gc)
	ctx = api.ContextWithHubProjectID(api.ContextWithBrokerMode(ctx), hubNativeTestProjectID)
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", markerPath, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(projectDir, "agents", agentName, "workspace")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, gotWS, _, err := GetAgent(ctx, agentName, "default", "", "", markerPath, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if gotWS != ws {
		t.Errorf("GetAgent workspace = %q, want %q", gotWS, ws)
	}
}
