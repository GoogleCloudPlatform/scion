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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// setupEmptyPerAgentStartProject writes an in-place project .scion dir under
// parent with a test harness-config and template. storage is spliced into
// settings.yaml under server: (e.g. a workspace_storage block), or omitted.
func setupEmptyPerAgentStartProject(t *testing.T, parent, storage string) string {
	t.Helper()
	projectScionDir := filepath.Join(parent, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	server := ""
	if storage != "" {
		server = "server:\n" + storage
	}
	settingsYAML := `schema_version: "1"
active_profile: local
` + server + `harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

func emptyPerAgentStartRuntime(captured *runtime.RunConfig, ran *bool) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			*captured = cfg
			*ran = true
			return "mock-id", nil
		},
	}
}

// TestStart_EmptyPerAgent_MountsPrivateWorkspace covers the Start half of
// design #2703 P2: the container's workspace is exactly the private
// agents/<slug>/workspace dir, no repo root is mounted even when the
// project sits in a git repository, and SCION_WORKSPACE_MODE alone (the
// start/restart shape, where the hub pre-resolves it into the env) is
// enough to select the mode.
func TestStart_EmptyPerAgent_MountsPrivateWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		viaFlag bool
		inGit   bool
	}{
		{name: "StartOptions flag", viaFlag: true},
		{name: "SCION_WORKSPACE_MODE env only"},
		{name: "project inside a git repository", viaFlag: true, inGit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)
			projectRoot := filepath.Join(tmpDir, "project")
			if tc.inGit {
				gitInitForTest(t, projectRoot)
			}
			projectScionDir := setupEmptyPerAgentStartProject(t, projectRoot, "")

			var captured runtime.RunConfig
			var ran bool
			mgr := NewManager(emptyPerAgentStartRuntime(&captured, &ran))
			_, err = mgr.Start(context.Background(), api.StartOptions{
				Name:                   "worker",
				ProjectPath:            projectScionDir,
				NoAuth:                 true,
				EmptyPerAgentWorkspace: tc.viaFlag,
				Env: map[string]string{
					"SCION_AGENT_ID":        "agent-1",
					"SCION_PROJECT_ID":      "proj-1",
					"SCION_WORKSPACE_MODE":  "empty-per-agent",
					"SCION_SOMETHING_OTHER": "x",
				},
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			if !ran {
				t.Fatal("runtime Run was not called")
			}
			want := filepath.Join(projectScionDir, "agents", "worker", "workspace")
			if captured.Workspace != want {
				t.Fatalf("Workspace = %q, want %q", captured.Workspace, want)
			}
			if captured.RepoRoot != "" {
				t.Fatalf("RepoRoot = %q, want empty (empty-per-agent is never a git checkout)", captured.RepoRoot)
			}
			for _, kv := range captured.Env {
				if strings.HasPrefix(kv, "SCION_WORKSPACE_GIT=") {
					t.Fatalf("unexpected %s in container env", kv)
				}
			}
		})
	}
}

// TestStart_EmptyPerAgent_PersistedModeWithoutFlag pins review N1 of #2760:
// a restart whose request lost the mode (no flag, no SCION_WORKSPACE_MODE)
// still mounts the private workspace from the mode persisted at provision,
// never the enclosing repo root, and the container gets the mode env.
func TestStart_EmptyPerAgent_PersistedModeWithoutFlag(t *testing.T) {
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	projectRoot := filepath.Join(tmpDir, "project")
	gitInitForTest(t, projectRoot)
	projectScionDir := setupEmptyPerAgentStartProject(t, projectRoot, "")

	var captured runtime.RunConfig
	var ran bool
	mgr := NewManager(emptyPerAgentStartRuntime(&captured, &ran))
	env := map[string]string{"SCION_AGENT_ID": "agent-1", "SCION_PROJECT_ID": "proj-1"}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "worker", ProjectPath: projectScionDir, NoAuth: true,
		EmptyPerAgentWorkspace: true, Env: env,
	}); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}

	captured, ran = runtime.RunConfig{}, false
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "worker", ProjectPath: projectScionDir, NoAuth: true, Env: env,
	}); err != nil {
		t.Fatalf("restart without the mode failed: %v", err)
	}
	if !ran {
		t.Fatal("runtime Run was not called")
	}
	want := filepath.Join(projectScionDir, "agents", "worker", "workspace")
	if captured.Workspace != want {
		t.Fatalf("Workspace = %q, want %q", captured.Workspace, want)
	}
	if captured.RepoRoot != "" {
		t.Fatalf("RepoRoot = %q, want empty", captured.RepoRoot)
	}
	hasMode := false
	for _, kv := range captured.Env {
		if kv == "SCION_WORKSPACE_MODE=empty-per-agent" {
			hasMode = true
		}
		if strings.HasPrefix(kv, "SCION_WORKSPACE_GIT=") {
			t.Fatalf("unexpected %s in container env", kv)
		}
	}
	if !hasMode {
		t.Fatalf("container env must carry SCION_WORKSPACE_MODE=empty-per-agent, got %v", captured.Env)
	}
	if _, ok := env["SCION_WORKSPACE_MODE"]; ok {
		t.Fatal("Start must not mutate the caller's Env map")
	}
}

// TestStart_EmptyPerAgent_NFSFailsClosed pins that empty-per-agent on NFS
// workspace storage with a runtime other than Kubernetes is refused before
// anything is started: only the Kubernetes runtime mounts the agent's own
// directory on the export (design #2703 P3), so any other runtime would
// get the project's shared workspace path.
func TestStart_EmptyPerAgent_NFSFailsClosed(t *testing.T) {
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	storage := fmt.Sprintf(`  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
`, filepath.Join(tmpDir, "nfs"))
	projectScionDir := setupEmptyPerAgentStartProject(t, filepath.Join(tmpDir, "project"), storage)

	var captured runtime.RunConfig
	var ran bool
	mgr := NewManager(emptyPerAgentStartRuntime(&captured, &ran))
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "worker",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":       "agent-1",
			"SCION_PROJECT_ID":     "proj-1",
			"SCION_WORKSPACE_MODE": "empty-per-agent",
		},
	})
	if !errors.Is(err, errEmptyPerAgentNFSRuntime) {
		t.Fatalf("Start error = %v, want errEmptyPerAgentNFSRuntime", err)
	}
	if ran {
		t.Fatalf("runtime Run must not be called; got workspace %q backend %q", captured.Workspace, captured.WorkspaceBackendName)
	}
}

// TestReprovision_EmptyPerAgent_ReusesWorkspaceInPlace pins same-broker
// reincarnation of an empty-per-agent agent (miller79/scion#167): Reprovision
// accepts it, the private workspace keeps its content, nothing is created
// in it, and the next generation's Start mounts the same directory.
func TestReprovision_EmptyPerAgent_ReusesWorkspaceInPlace(t *testing.T) {
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	projectScionDir := setupEmptyPerAgentStartProject(t, filepath.Join(tmpDir, "project"), "")

	var captured runtime.RunConfig
	var ran bool
	rt := emptyPerAgentStartRuntime(&captured, &ran)
	rt.NameFunc = func() string { return "docker" }
	mgr := NewManager(rt)
	opts := api.StartOptions{
		Name:                   "worker",
		ProjectPath:            projectScionDir,
		NoAuth:                 true,
		EmptyPerAgentWorkspace: true,
	}
	// Generation N: a normal provision persists the empty-per-agent mode
	// and creates the private workspace.
	if _, err := mgr.Provision(context.Background(), opts); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	ws := filepath.Join(projectScionDir, "agents", "worker", "workspace")
	note := filepath.Join(ws, "notes.md")
	if err := os.WriteFile(note, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	listTree := func() []string {
		var out []string
		if err := filepath.Walk(ws, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			out = append(out, fmt.Sprintf("%s %v", path, info.Mode()))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := listTree()
	wsInfoBefore, err := os.Stat(ws)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.Reprovision(context.Background(), opts); err != nil {
		t.Fatalf("Reprovision failed: %v", err)
	}

	if got, err := os.ReadFile(note); err != nil || string(got) != "keep" {
		t.Fatalf("workspace content changed: %q, %v", got, err)
	}
	if after := listTree(); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("workspace tree changed:\nbefore: %v\nafter:  %v", before, after)
	}
	wsInfoAfter, err := os.Stat(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(wsInfoBefore, wsInfoAfter) {
		t.Fatal("workspace directory was recreated, want the same directory reused in place")
	}
	if !persistedEmptyPerAgent([]string{filepath.Join(projectScionDir, "agents")}, "", "worker") {
		t.Fatal("reprovisioned scion-agent.json must still record empty-per-agent")
	}

	// Generation N+1 mounts the same directory.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "worker", ProjectPath: projectScionDir, NoAuth: true,
		EmptyPerAgentWorkspace: true,
		Env:                    map[string]string{"SCION_AGENT_ID": "agent-1", "SCION_PROJECT_ID": "proj-1"},
	}); err != nil {
		t.Fatalf("Start after Reprovision failed: %v", err)
	}
	if !ran {
		t.Fatal("runtime Run was not called")
	}
	if captured.Workspace != ws {
		t.Fatalf("Workspace = %q, want %q", captured.Workspace, ws)
	}
	if got, err := os.ReadFile(note); err != nil || string(got) != "keep" {
		t.Fatalf("workspace content changed after Start: %q, %v", got, err)
	}
}

// listTreeForTest returns a sorted path+mode listing of root (Lstat, so a
// symlink is listed, not followed), or nil when root does not exist.
func listTreeForTest(t *testing.T, root string) []string {
	t.Helper()
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var out []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %v %d", path, info.Mode(), info.Size()))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestReprovision_EmptyPerAgent_Refused pins the refusals of Reprovision's
// empty-per-agent branch (miller79/scion#167). Each case is refused with
// ErrReprovisionRefused (409 at the broker), and nothing is created,
// cleared or replaced: the agents directory tree (and, for the symlink case,
// the link target) is byte-for-byte the same afterwards. The preflight
// refuses before the runtime is consulted at all (no List call), so these
// cases pin Manager.Reprovision's own checks, not only ProvisionAgent's
// second line of defence (TestProvisionAgent_EmptyPerAgentReprovision_*).
func TestReprovision_EmptyPerAgent_Refused(t *testing.T) {
	cases := []struct {
		name string
		// setup mutates the provisioned agent; ws is its workspace and
		// outside a directory outside the project.
		setup   func(t *testing.T, projectScionDir, ws, outside string)
		opts    func(o *api.StartOptions)
		rtName  string
		wantMsg string
	}{
		{
			name: "workspace missing",
			setup: func(t *testing.T, _, ws, _ string) {
				if err := os.RemoveAll(ws); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "workspace missing or not a real directory",
		},
		{
			name: "workspace is a symlink to a directory",
			setup: func(t *testing.T, _, ws, outside string) {
				if err := os.RemoveAll(ws); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, ws); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "workspace missing or not a real directory",
		},
		{
			name: "workspace is a regular file",
			setup: func(t *testing.T, _, ws, _ string) {
				if err := os.RemoveAll(ws); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ws, []byte("not a dir"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "workspace missing or not a real directory",
		},
		{
			name: "persisted config does not record empty-per-agent",
			setup: func(t *testing.T, projectScionDir, _, _ string) {
				cfgPath := filepath.Join(projectScionDir, "agents", "worker", "scion-agent.json")
				raw, err := os.ReadFile(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				if _, ok := m["empty_per_agent_workspace"]; !ok {
					t.Fatalf("fixture check: %s has no empty_per_agent_workspace key: %s", cfgPath, raw)
				}
				delete(m, "empty_per_agent_workspace")
				out, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfgPath, out, 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "was not created empty-per-agent",
		},
		{
			name: "agent was never provisioned",
			setup: func(t *testing.T, projectScionDir, _, _ string) {
				if err := os.RemoveAll(filepath.Join(projectScionDir, "agents", "worker")); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg: "was not created empty-per-agent",
		},
		{
			name:    "request also names a git clone",
			opts:    func(o *api.StartOptions) { o.GitClone = &api.GitCloneConfig{URL: "https://example.com/r.git"} },
			wantMsg: "also names a git clone, workspace path or shared workspace",
		},
		{
			name:    "request also names a workspace path",
			opts:    func(o *api.StartOptions) { o.Workspace = "/somewhere" },
			wantMsg: "also names a git clone, workspace path or shared workspace",
		},
		{
			name:    "request also names a shared workspace",
			opts:    func(o *api.StartOptions) { o.SharedWorkspace = true },
			wantMsg: "also names a git clone, workspace path or shared workspace",
		},
		{
			name:    "kubernetes runtime",
			rtName:  "kubernetes",
			wantMsg: "supported only on local-disk runtimes",
		},
		{
			name:    "cloudrun runtime",
			rtName:  "cloudrun",
			wantMsg: "supported only on local-disk runtimes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)
			projectScionDir := setupEmptyPerAgentStartProject(t, filepath.Join(tmpDir, "project"), "")

			var captured runtime.RunConfig
			var ran bool
			rt := emptyPerAgentStartRuntime(&captured, &ran)
			rtName := "docker"
			rt.NameFunc = func() string { return rtName }
			mgr := NewManager(rt)
			base := api.StartOptions{Name: "worker", ProjectPath: projectScionDir, NoAuth: true, EmptyPerAgentWorkspace: true}
			if _, err := mgr.Provision(context.Background(), base); err != nil {
				t.Fatalf("Provision failed: %v", err)
			}
			ws := filepath.Join(projectScionDir, "agents", "worker", "workspace")
			if err := os.WriteFile(filepath.Join(ws, "notes.md"), []byte("keep"), 0644); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(tmpDir, "outside")
			if err := os.MkdirAll(outside, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "theirs.md"), []byte("theirs"), 0644); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, projectScionDir, ws, outside)
			}
			if tc.rtName != "" {
				rtName = tc.rtName
			}
			agentsRoot := filepath.Join(projectScionDir, "agents")
			beforeAgents := listTreeForTest(t, agentsRoot)
			beforeOutside := listTreeForTest(t, outside)

			listCalls := 0
			rt.ListFunc = func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				listCalls++
				return []api.AgentInfo{}, nil
			}
			opts := base
			if tc.opts != nil {
				tc.opts(&opts)
			}
			_, err = mgr.Reprovision(context.Background(), opts)
			if !errors.Is(err, ErrReprovisionRefused) {
				t.Fatalf("Reprovision error = %v, want ErrReprovisionRefused", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("Reprovision error = %q, want it to contain %q", err, tc.wantMsg)
			}
			if listCalls != 0 {
				t.Fatalf("runtime List called %d times; the preflight must refuse before consulting the runtime", listCalls)
			}
			if after := listTreeForTest(t, agentsRoot); strings.Join(after, "\n") != strings.Join(beforeAgents, "\n") {
				t.Fatalf("agents tree changed:\nbefore: %v\nafter:  %v", beforeAgents, after)
			}
			if after := listTreeForTest(t, outside); strings.Join(after, "\n") != strings.Join(beforeOutside, "\n") {
				t.Fatalf("symlink target changed:\nbefore: %v\nafter:  %v", beforeOutside, after)
			}
		})
	}
}

// TestProvisionAgent_EmptyPerAgentReprovision_NeverCreatesWorkspace pins
// review p1-r1 N2: ProvisionAgent itself, called for a reprovision of an
// empty-per-agent agent, refuses a workspace that is gone (or a symlink, or a
// file) instead of recreating it, so "reincarnate never recreates the
// workspace" holds even if it vanishes after Manager.Reprovision's preflight.
// The normal (non-reprovision) provision still creates it.
func TestProvisionAgent_EmptyPerAgentReprovision_NeverCreatesWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, ws string)
	}{
		{name: "missing", setup: func(t *testing.T, ws string) {}},
		{name: "symlink", setup: func(t *testing.T, ws string) {
			target := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(ws))), "elsewhere")
			if err := os.MkdirAll(target, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, ws); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file", setup: func(t *testing.T, ws string) {
			if err := os.WriteFile(ws, []byte("x"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)
			projectScionDir := setupEmptyPerAgentStartProject(t, filepath.Join(tmpDir, "project"), "")
			agentDir := filepath.Join(projectScionDir, "agents", "worker")
			if err := os.MkdirAll(agentDir, 0755); err != nil {
				t.Fatal(err)
			}
			ws := filepath.Join(agentDir, "workspace")
			tc.setup(t, ws)
			before := listTreeForTest(t, agentDir)

			ctx := api.ContextWithReprovision(api.ContextWithEmptyPerAgentWorkspace(context.Background()))
			_, _, _, err = ProvisionAgent(ctx, "worker", "", "", "", projectScionDir, "", "created", "", "")
			if !errors.Is(err, ErrReprovisionRefused) {
				t.Fatalf("ProvisionAgent error = %v, want ErrReprovisionRefused", err)
			}
			if after := listTreeForTest(t, agentDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("agent dir changed:\nbefore: %v\nafter:  %v", before, after)
			}
		})
	}

	t.Run("normal provision still creates it", func(t *testing.T) {
		tmpDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Chdir(tmpDir)
		t.Setenv("HOME", tmpDir)
		projectScionDir := setupEmptyPerAgentStartProject(t, filepath.Join(tmpDir, "project"), "")
		ctx := api.ContextWithEmptyPerAgentWorkspace(context.Background())
		if _, _, _, err := ProvisionAgent(ctx, "worker", "", "", "", projectScionDir, "", "created", "", ""); err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}
		if info, err := os.Lstat(filepath.Join(projectScionDir, "agents", "worker", "workspace")); err != nil || !info.IsDir() {
			t.Fatalf("workspace not created by a normal provision: %v", err)
		}
	})
}
