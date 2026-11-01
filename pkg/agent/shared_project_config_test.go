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

// These tests exercise unix-only, Windows-gated agent storage code.
//go:build unix

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const sharedCfgProjectID = "0f9e8d7c-6b5a-4321-8fed-cba987654321"

// sharedConfigProject creates a hub-seeded shared project root
// (<share>/projects/<id>) under a temp HOME and returns the root and HOME.
func sharedConfigProject(t *testing.T) (root, home string) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	root = filepath.Join(t.TempDir(), "share-a", "projects", sharedCfgProjectID)
	scionDir := filepath.Join(root, config.DotScion)
	mustMkdirAll(t, scionDir)
	for name, content := range map[string]string{
		projectkeys.ProjectIDFile:   sharedCfgProjectID + "\n",
		projectkeys.ProjectSlugFile: "web\n",
		"settings.yaml":             "schema_version: \"1\"\n",
	} {
		if err := os.WriteFile(filepath.Join(scionDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, home
}

// The external agents root of a shared project holds a worktree-mode
// agent's state too, and that alone does not make the agent a
// shared-workspace agent.
func TestEffectiveSharedWorkspace_SharedProjectConfig(t *testing.T) {
	root, home := sharedConfigProject(t)
	scionDir := filepath.Join(root, config.DotScion)
	dir, shared, err := AgentStateDir(scionDir, "a1", false, sharedCfgProjectID, true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir, "web__0f9e8d7c", config.DotScion, "agents", "a1")
	if dir != want || shared {
		t.Fatalf("AgentStateDir = (%q, shared=%v), want (%q, false)", dir, shared, want)
	}
	mustMkdirAll(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, shared, _ := AgentStateDir(scionDir, "a1", false, sharedCfgProjectID, true); shared {
		t.Error("state in the forced external root must not flip the agent to shared-workspace")
	}
}

// An agent provisioned before the configuration moved keeps its legacy
// in-project dir: it is found for delete and removed with the agent, so a
// later agent of the same name never resolves to it.
func TestDeleteAgentFiles_SharedProjectLegacyDir(t *testing.T) {
	for _, hubID := range []string{sharedCfgProjectID, ""} {
		t.Run("hub id "+hubID, func(t *testing.T) { testDeleteSharedProjectLegacyDir(t, hubID) })
	}
}

func testDeleteSharedProjectLegacyDir(t *testing.T, hubID string) {
	root, home := sharedConfigProject(t)
	legacyScion := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "web", config.DotScion)
	mustMkdirAll(t, legacyScion)
	if err := config.WriteProjectID(legacyScion, sharedCfgProjectID); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyScion, "agents", "old")
	mustMkdirAll(t, legacy)
	if err := os.WriteFile(filepath.Join(legacy, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	exists, err := LocalAgentDirExists("old", root)
	if err != nil || !exists {
		t.Fatalf("LocalAgentDirExists = (%v, %v), want the legacy dir found", exists, err)
	}
	if _, err := DeleteAgentFilesForHubProject("old", root, hubID, false); err != nil {
		t.Fatalf("DeleteAgentFilesForHubProject: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy agent dir should be removed with the agent (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, config.DotScion, projectkeys.ProjectIDFile)); err != nil {
		t.Fatalf("the shared project configuration must be left alone: %v", err)
	}
	if got := config.ResolveAgentDir(filepath.Join(root, config.DotScion), "old"); got == legacy {
		t.Error("a new agent of the same name must not resolve to the removed legacy dir")
	}
}

// removeCheckedAgentDir removes the agent's scion-agent.json before the
// rest of the directory; when it is a symbolic link, the link goes and its
// target is left unchanged.
func TestRemoveCheckedAgentDir_AgentJSONLinkNotFollowed(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(t.TempDir(), "agents", "old")
	mustMkdirAll(t, filepath.Join(agentDir, "home"))
	if err := os.Symlink(outside, filepath.Join(agentDir, "scion-agent.json")); err != nil {
		t.Fatal(err)
	}

	removeCheckedAgentDir(agentDir, "")

	if _, err := os.Lstat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("agent dir should be removed (err %v)", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("the link's target must be left unchanged: %q, %v", data, err)
	}
}

// sharedRootUUID is the project ID of the shared-root start tests: the
// shared tree is keyed by canonical project UUIDs.
const sharedRootUUID = "4f1d2c3b-5a69-4e70-8b91-a2b3c4d5e6f7"

// A hub-managed agent started from a shared project root (the broker's
// ProjectPath on a shared Layout) mounts the project's workspace on the
// share, keeps its state and home in the broker-local external root, and
// writes nothing into the hub-seeded configuration.
func TestStart_SharedProjectRoot(t *testing.T) {
	for _, rtName := range []string{"docker", "kubernetes"} {
		t.Run(rtName, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
			root := filepath.Join(mountRoot, "share-1", "projects", sharedRootUUID)
			scionDir := filepath.Join(root, config.DotScion)
			mustMkdirAll(t, filepath.Join(root, "workspace"))
			mustMkdirAll(t, scionDir)
			for name, content := range map[string]string{
				projectkeys.ProjectIDFile:   sharedRootUUID + "\n",
				projectkeys.ProjectSlugFile: "web\n",
				"settings.yaml":             "schema_version: \"1\"\n",
			} {
				if err := os.WriteFile(filepath.Join(scionDir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := listTree(t, scionDir)

			var cfg runtime.RunConfig
			mockRT := &runtime.MockRuntime{
				NameFunc: func() string { return rtName },
				RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
					cfg = rc
					return "mock-id", nil
				},
			}
			_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name:            "test-agent",
				ProjectPath:     root,
				NoAuth:          true,
				SharedWorkspace: true,
				Env: map[string]string{
					"SCION_AGENT_ID":   "agent-5823",
					"SCION_PROJECT_ID": sharedRootUUID,
				},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if rtName == "docker" && cfg.Workspace != filepath.Join(root, "workspace") {
				t.Errorf("workspace = %q, want the shared workspace %q", cfg.Workspace, filepath.Join(root, "workspace"))
			}
			if rtName == "kubernetes" && cfg.NFSSubPath != "projects/"+sharedRootUUID+"/workspace" {
				t.Errorf("NFS subPath = %q", cfg.NFSSubPath)
			}
			ext := filepath.Join(f.tmpDir, config.GlobalDir, config.ProjectConfigsDir, "web__"+strings.ReplaceAll(sharedRootUUID, "-", "")[:8], config.DotScion, "agents", "test-agent")
			if _, err := os.Stat(filepath.Join(ext, "scion-agent.json")); err != nil {
				t.Errorf("agent state should be in the broker-local external root %s: %v", ext, err)
			}
			if after := listTree(t, scionDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Errorf("the agent manager wrote into the shared project configuration:\nbefore %v\nafter  %v", before, after)
			}
			if strings.HasPrefix(cfg.HomeDir, mountRoot) {
				t.Errorf("home %q is on the share; homes: local keeps it on the broker", cfg.HomeDir)
			}
		})
	}
}

func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	if err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %d", strings.TrimPrefix(p, dir), info.Size()))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// An agent started before its project's configuration moved to the shared
// project root, in a mode whose state stayed in the legacy in-project dir,
// is found there (look-up fallback). Its workspace is the legacy
// broker-local project dir (project-scoped), which is never admitted from
// the legacy tree, so a restart from the shared root is refused by the
// workspace containment check, cleanly: nothing is provisioned in the
// external root and the legacy state is left as it was.
func TestStart_SharedProjectRootLegacyAgentRestartRefusedCleanly(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	root := filepath.Join(mountRoot, "share-1", "projects", sharedRootUUID)
	mustMkdirAll(t, filepath.Join(root, "workspace"))
	scionDir := filepath.Join(root, config.DotScion)
	mustMkdirAll(t, scionDir)
	for name, content := range map[string]string{
		projectkeys.ProjectIDFile:   sharedRootUUID + "\n",
		projectkeys.ProjectSlugFile: "web\n",
		"settings.yaml":             "schema_version: \"1\"\n",
	} {
		if err := os.WriteFile(filepath.Join(scionDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The pre-move layout: the broker-local hub-managed project dir.
	legacyRoot := filepath.Join(f.tmpDir, config.GlobalDir, config.ProjectsDir, "web")
	legacyScion := filepath.Join(legacyRoot, config.DotScion)
	mustMkdirAll(t, legacyScion)
	if err := config.WriteProjectID(legacyScion, sharedRootUUID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyScion, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := func(projectPath string) error {
		mockRT := &runtime.MockRuntime{
			NameFunc: func() string { return "docker" },
			RunFunc:  func(ctx context.Context, rc runtime.RunConfig) (string, error) { return "mock-id", nil },
		}
		_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
			Name:        "test-agent",
			ProjectPath: projectPath,
			NoAuth:      true,
			Env: map[string]string{
				"SCION_AGENT_ID":   "agent-5823",
				"SCION_PROJECT_ID": sharedRootUUID,
			},
		})
		return err
	}
	if err := start(legacyRoot); err != nil {
		t.Fatalf("pre-move start: %v", err)
	}
	legacyAgent := filepath.Join(legacyScion, "agents", "test-agent")
	before := listTree(t, legacyAgent)
	if got := config.ResolveAgentDir(scionDir, "test-agent"); got != legacyAgent {
		t.Fatalf("look-up from the shared root = %q, want the legacy dir %q", got, legacyAgent)
	}

	err := start(root)
	if err == nil || !strings.Contains(err.Error(), "outside the permitted workspace root") {
		t.Fatalf("restart from the shared root = %v, want the workspace containment refusal", err)
	}
	ext := filepath.Join(f.tmpDir, config.GlobalDir, config.ProjectConfigsDir, "web__"+strings.ReplaceAll(sharedRootUUID, "-", "")[:8], config.DotScion, "agents", "test-agent")
	if _, err := os.Stat(filepath.Join(ext, "scion-agent.json")); !os.IsNotExist(err) {
		t.Errorf("nothing may be provisioned in the external root (err %v)", err)
	}
	if after := listTree(t, legacyAgent); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the legacy state changed:\nbefore %v\nafter  %v", before, after)
	}
}

// A pre-move clone-per-agent agent (state and workspace in its legacy agent
// dir) restarts from the shared project root with that workspace intact:
// exactly its own legacy agent dir is admitted as a workspace root.
func TestStart_SharedProjectRootRestartsLegacyClonePerAgent(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	// Clone-per-agent keeps a node-local workspace in the agent's own dir
	// (the shared tree holds only the project configuration here).
	f.writeGlobalSettings(t, "")
	root := filepath.Join(t.TempDir(), "share-1", "projects", sharedRootUUID)
	mustMkdirAll(t, filepath.Join(root, "workspace"))
	scionDir := filepath.Join(root, config.DotScion)
	mustMkdirAll(t, scionDir)
	for name, content := range map[string]string{
		projectkeys.ProjectIDFile:   sharedRootUUID + "\n",
		projectkeys.ProjectSlugFile: "web\n",
		"settings.yaml":             "schema_version: \"1\"\n",
	} {
		if err := os.WriteFile(filepath.Join(scionDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	legacyRoot := filepath.Join(f.tmpDir, config.GlobalDir, config.ProjectsDir, "web")
	legacyScion := filepath.Join(legacyRoot, config.DotScion)
	mustMkdirAll(t, legacyScion)
	if err := config.WriteProjectID(legacyScion, sharedRootUUID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyScion, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var cfg runtime.RunConfig
	start := func(projectPath string) error {
		mockRT := &runtime.MockRuntime{
			NameFunc: func() string { return "docker" },
			RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
				cfg = rc
				return "mock-id", nil
			},
		}
		_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
			Name:        "test-agent",
			ProjectPath: projectPath,
			NoAuth:      true,
			GitClone:    &api.GitCloneConfig{URL: "https://example.com/repo.git"},
			Env: map[string]string{
				"SCION_AGENT_ID":       "agent-5823",
				"SCION_PROJECT_ID":     sharedRootUUID,
				"SCION_WORKSPACE_MODE": "clone-per-agent",
			},
		})
		return err
	}
	if err := start(legacyRoot); err != nil {
		t.Fatalf("pre-move start: %v", err)
	}
	legacyAgent := filepath.Join(legacyScion, "agents", "test-agent")
	if !strings.HasPrefix(cfg.Workspace, legacyAgent+string(filepath.Separator)) {
		t.Fatalf("fixture: the pre-move workspace %q should be in the agent's dir %q", cfg.Workspace, legacyAgent)
	}
	work := filepath.Join(cfg.Workspace, "unpushed.txt")
	if err := os.WriteFile(work, []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	preWorkspace := cfg.Workspace

	if err := start(root); err != nil {
		t.Fatalf("restart from the shared root: %v", err)
	}
	if cfg.Workspace != preWorkspace {
		t.Errorf("restart workspace = %q, want the agent's own legacy workspace %q", cfg.Workspace, preWorkspace)
	}
	if data, err := os.ReadFile(work); err != nil || string(data) != "work" {
		t.Errorf("the legacy workspace content must be intact: %q, %v", data, err)
	}
}

// The legacy admission is exactly the agent's own legacy dir, reached through
// the state fallback, for its own (agent-scoped) workspace.
func TestLegacyAgentWorkspaceRoot(t *testing.T) {
	root, home := sharedConfigProject(t)
	scionDir := filepath.Join(root, config.DotScion)
	legacyScion := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "web", config.DotScion)
	mustMkdirAll(t, legacyScion)
	if err := config.WriteProjectID(legacyScion, sharedCfgProjectID); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyScion, "agents", "old")
	mustMkdirAll(t, legacy)
	if err := os.WriteFile(filepath.Join(legacy, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := config.AgentDirForProject(scionDir, "new", false, sharedCfgProjectID)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, agent, agentDir string
		shared                bool
		want                  string
	}{
		{"own legacy dir", "old", legacy, false, legacy},
		{"shared or explicit workspace", "old", legacy, true, ""},
		{"another agent naming the legacy dir", "new", legacy, false, ""},
		{"an agent in the external root", "new", ext, false, ""},
		{"no agent dir", "old", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyAgentWorkspaceRoot(scionDir, tc.agent, tc.agentDir, tc.shared); got != tc.want {
				t.Errorf("legacyAgentWorkspaceRoot = %q, want %q", got, tc.want)
			}
		})
	}
	// An ordinary (local-backend) project has no legacy admission.
	if got := legacyAgentWorkspaceRoot(legacyScion, "old", legacy, false); got != "" {
		t.Errorf("local project: got %q, want \"\"", got)
	}
}

// On a shared project root the repo-root candidate is the shared layout's
// project-level workspace (<ProjectRoot>/workspace), whatever the backend
// places on this host; an ordinary project keeps its own root.
func TestBrokerProjectRoot_SharedProjectRoot(t *testing.T) {
	root, _ := sharedConfigProject(t)
	scionDir := filepath.Join(root, config.DotScion)
	if got := brokerProjectRoot(nil, scionDir); got != filepath.Join(root, "workspace") {
		t.Errorf("brokerProjectRoot(shared) = %q, want %q", got, filepath.Join(root, "workspace"))
	}
	plain := filepath.Join(t.TempDir(), "proj", config.DotScion)
	if got := brokerProjectRoot(nil, plain); got != filepath.Dir(plain) {
		t.Errorf("brokerProjectRoot(plain) = %q, want %q", got, filepath.Dir(plain))
	}
}

// A create (FreshProvision) of a new agent whose name resolves to a legacy
// agent directory that no agent owns is refused before any side effect;
// a start of an existing legacy agent, an agent with external state, a new
// agent without a legacy dir and an ordinary project are not.
func TestRefuseRetiredLegacyAgent(t *testing.T) {
	root, home := sharedConfigProject(t)
	scionDir := filepath.Join(root, config.DotScion)
	legacyScion := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "web", config.DotScion)
	mustMkdirAll(t, legacyScion)
	if err := config.WriteProjectID(legacyScion, sharedCfgProjectID); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyScion, "agents", "old")
	mustMkdirAll(t, legacy)
	if err := os.WriteFile(filepath.Join(legacy, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	create := api.StartOptions{Name: "old", FreshProvision: true, HubProjectID: sharedCfgProjectID}
	if err := refuseRetiredLegacyAgent(scionDir, create); !errors.Is(err, ErrLegacyAgentStateRetired) {
		t.Fatalf("create over an orphan legacy dir: %v, want ErrLegacyAgentStateRetired", err)
	}
	if strings.Contains(ErrLegacyAgentStateRetired.Error(), home) {
		t.Error("the refusal must not name a host path")
	}
	restart := create
	restart.FreshProvision = false
	if err := refuseRetiredLegacyAgent(scionDir, restart); err != nil {
		t.Errorf("start of an existing legacy agent: %v", err)
	}
	fresh := create
	fresh.Name = "new"
	if err := refuseRetiredLegacyAgent(scionDir, fresh); err != nil {
		t.Errorf("create of a new agent without a legacy dir: %v", err)
	}
	if err := refuseRetiredLegacyAgent(legacyScion, create); err != nil {
		t.Errorf("ordinary project: %v", err)
	}
	ext, err := config.AgentDirForProject(scionDir, "zzz", false, sharedCfgProjectID)
	if err != nil {
		t.Fatal(err)
	}
	extOld := filepath.Join(filepath.Dir(ext), "old")
	mustMkdirAll(t, extOld)
	if err := os.WriteFile(filepath.Join(extOld, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := refuseRetiredLegacyAgent(scionDir, create); err != nil {
		t.Errorf("agent with external state: %v", err)
	}
}

// A create of a new agent over a legacy dir no agent owns is refused
// through Start, cleanly: nothing is provisioned in the external root and
// the legacy dir is left as it was. (A start of the live legacy agent is
// TestStart_SharedProjectRootRestartsLegacyClonePerAgent.)
func TestStart_SharedProjectRootCreateOverOrphanLegacyDirRefused(t *testing.T) {
	root, home := sharedConfigProject(t)
	scionDir := filepath.Join(root, config.DotScion)
	legacyScion := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "web", config.DotScion)
	mustMkdirAll(t, legacyScion)
	if err := config.WriteProjectID(legacyScion, sharedCfgProjectID); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyScion, "agents", "old")
	mustMkdirAll(t, filepath.Join(legacy, "workspace"))
	if err := os.WriteFile(filepath.Join(legacy, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "workspace", "unpushed.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	mockRT := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name:           "old",
		ProjectPath:    root,
		NoAuth:         true,
		FreshProvision: true,
		HubProjectID:   sharedCfgProjectID,
		Env:            map[string]string{"SCION_AGENT_ID": "agent-new", "SCION_PROJECT_ID": sharedCfgProjectID},
	})
	if !errors.Is(err, ErrLegacyAgentStateRetired) {
		t.Fatalf("create over an orphan legacy dir = %v, want ErrLegacyAgentStateRetired", err)
	}
	ext, _ := config.AgentDirForProject(scionDir, "zzz", false, sharedCfgProjectID)
	if _, err := os.Stat(filepath.Join(filepath.Dir(ext), "old", "scion-agent.json")); !os.IsNotExist(err) {
		t.Errorf("nothing may be provisioned in the external root (err %v)", err)
	}
	if data, err := os.ReadFile(filepath.Join(legacy, "workspace", "unpushed.txt")); err != nil || string(data) != "work" {
		t.Errorf("the legacy dir must be left as it was: %q %v", data, err)
	}
}

// Delete removes an agent's state in the forced external root of a shared
// project configuration, with or without a Hub project ID, and leaves the
// configuration and other agents alone.
func TestDeleteAgentFiles_SharedProjectExternalState(t *testing.T) {
	for _, hubID := range []string{sharedCfgProjectID, ""} {
		t.Run("hub id "+hubID, func(t *testing.T) {
			root, _ := sharedConfigProject(t)
			scionDir := filepath.Join(root, config.DotScion)
			ext, err := config.AgentDirForProject(scionDir, "a1", false, sharedCfgProjectID)
			if err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(filepath.Dir(ext), "a2")
			for _, d := range []string{ext, other} {
				mustMkdirAll(t, filepath.Join(d, "home"))
				if err := os.WriteFile(filepath.Join(d, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if exists, err := LocalAgentDirExists("a1", root); err != nil || !exists {
				t.Fatalf("LocalAgentDirExists = (%v, %v)", exists, err)
			}
			if _, err := DeleteAgentFilesForHubProject("a1", root, hubID, false); err != nil {
				t.Fatalf("DeleteAgentFilesForHubProject: %v", err)
			}
			if _, err := os.Stat(ext); !os.IsNotExist(err) {
				t.Errorf("the external state dir must be removed (err %v)", err)
			}
			if _, err := os.Stat(filepath.Join(other, "scion-agent.json")); err != nil {
				t.Errorf("another agent's state must stay: %v", err)
			}
			if _, err := os.Stat(filepath.Join(scionDir, projectkeys.ProjectIDFile)); err != nil {
				t.Errorf("the shared project configuration must stay: %v", err)
			}
		})
	}
}

// The refusal is an agent-state conflict, so brokers answer it with 409 and
// its message, never an opaque 500.
func TestErrLegacyAgentStateRetiredIsStateConflict(t *testing.T) {
	err := fmt.Errorf("start: %w", ErrLegacyAgentStateRetired)
	if !config.IsAgentStateConflict(err) {
		t.Fatal("ErrLegacyAgentStateRetired must be an agent-state conflict")
	}
	if !errors.Is(err, ErrLegacyAgentStateRetired) {
		t.Fatal("errors.Is must match the refusal")
	}
	if !strings.Contains(err.Error(), "delete this agent") {
		t.Errorf("the message must say what to do: %q", err.Error())
	}
}

// The create refusal runs in Provision too, before anything runs: nothing
// is provisioned in the external root and the legacy dir is left alone.
func TestProvision_CreateOverOrphanLegacyDirRefused(t *testing.T) {
	root, home := sharedConfigProject(t)
	scionDir := filepath.Join(root, config.DotScion)
	legacyScion := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "web", config.DotScion)
	mustMkdirAll(t, legacyScion)
	if err := config.WriteProjectID(legacyScion, sharedCfgProjectID); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyScion, "agents", "old")
	mustMkdirAll(t, legacy)
	if err := os.WriteFile(filepath.Join(legacy, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(&runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	_, err := mgr.Provision(context.Background(), api.StartOptions{
		Name: "old", ProjectPath: root, FreshProvision: true, HubProjectID: sharedCfgProjectID,
	})
	if !errors.Is(err, ErrLegacyAgentStateRetired) {
		t.Fatalf("Provision over an orphan legacy dir = %v, want ErrLegacyAgentStateRetired", err)
	}
	ext, _ := config.AgentDirForProject(scionDir, "zzz", false, sharedCfgProjectID)
	if _, err := os.Stat(filepath.Join(filepath.Dir(ext), "old")); !os.IsNotExist(err) {
		t.Errorf("nothing may be provisioned in the external root (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "scion-agent.json")); err != nil {
		t.Errorf("the legacy dir must be left alone: %v", err)
	}
}
