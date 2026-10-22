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

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// projectConfigLayout builds a host layout for one workspace mode and
// returns the run config that mounts it.
type projectConfigLayout func(t *testing.T, cfg RunConfig) RunConfig

// subdirProjectLayout is a project kept in a subdirectory of a repository:
// the workspace is <repo>/sub, holding the project's .scion.
func subdirProjectLayout(t *testing.T, cfg RunConfig, withAgents bool) RunConfig {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	ws := filepath.Join(root, "sub")
	mkdirs(t, filepath.Join(root, ".git"), filepath.Join(ws, ".scion", "templates"))
	if withAgents {
		mkdirs(t, filepath.Join(ws, ".scion", "agents", "other"))
	}
	cfg.RepoRoot, cfg.Workspace = root, ws
	return cfg
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	mkdirs(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// volumeArgs returns the values of every -v argument.
func volumeArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" {
			out = append(out, args[i+1])
		}
	}
	return out
}

// hasArgPair reports whether args holds flag immediately followed by value.
func hasArgPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// The project configuration directory of a workspace mounted at
// /workspace is mounted read-only in the shared-workspace and plain
// layouts on Docker, Podman and Apple, with the agents mask on top when the
// agents dir exists; the other layouts are unchanged. In every layout the
// agent's own home and workspace mounts stay read-write, and nothing else
// outside the git admin dir is mounted read-only.
func TestBuildCommonRunArgs_ProjectConfigReadOnly(t *testing.T) {
	sharedWith := func(withAgents bool) projectConfigLayout {
		return func(t *testing.T, cfg RunConfig) RunConfig {
			root := filepath.Join(t.TempDir(), "repo")
			mkdirs(t, filepath.Join(root, ".scion", "templates"))
			if withAgents {
				mkdirs(t, filepath.Join(root, ".scion", "agents", "other"))
			}
			cfg.RepoRoot, cfg.Workspace = root, root
			return cfg
		}
	}
	sharedMarker := func(t *testing.T, cfg RunConfig) RunConfig {
		root := filepath.Join(t.TempDir(), "repo")
		writeFile(t, filepath.Join(root, ".scion"), "project-id: x\n")
		cfg.RepoRoot, cfg.Workspace = root, root
		return cfg
	}
	sharedNoScion := func(t *testing.T, cfg RunConfig) RunConfig {
		root := filepath.Join(t.TempDir(), "repo")
		mkdirs(t, root)
		cfg.RepoRoot, cfg.Workspace = root, root
		return cfg
	}
	plainWith := func(withAgents bool) projectConfigLayout {
		return func(t *testing.T, cfg RunConfig) RunConfig {
			ws := filepath.Join(t.TempDir(), "project")
			writeFile(t, filepath.Join(ws, ".scion", "settings.yaml"), "x: y\n")
			if withAgents {
				mkdirs(t, filepath.Join(ws, ".scion", "agents", "other", "home"))
			}
			cfg.Workspace = ws
			return cfg
		}
	}
	// An old-style non-git project keeps the agent's home under the
	// workspace's own .scion/agents, below the read-only bind and the mask;
	// its separate home bind stays read-write.
	plainHomeInside := func(t *testing.T, cfg RunConfig) RunConfig {
		ws := filepath.Join(t.TempDir(), "project")
		writeFile(t, filepath.Join(ws, ".scion", "settings.yaml"), "x: y\n")
		cfg.HomeDir = filepath.Join(ws, ".scion", "agents", "me", "home")
		mkdirs(t, cfg.HomeDir)
		cfg.Workspace = ws
		return cfg
	}
	plainMarker := func(t *testing.T, cfg RunConfig) RunConfig {
		ws := filepath.Join(t.TempDir(), "project")
		writeFile(t, filepath.Join(ws, ".scion"), "project-id: x\n")
		cfg.Workspace = ws
		return cfg
	}
	worktree := func(t *testing.T, cfg RunConfig) RunConfig {
		root := filepath.Join(t.TempDir(), "repo")
		ws := filepath.Join(root, ".scion", "agents", "me", "workspace")
		mkdirs(t, filepath.Join(root, ".git"), filepath.Join(ws, ".scion", "templates"))
		cfg.RepoRoot, cfg.Workspace = root, ws
		cfg.AgentOwnWorkspace = true
		return cfg
	}
	// A subdirectory project where an agent created a .git file: still the
	// project's workspace, since the broker decides AgentOwnWorkspace.
	subdirWithGitFile := func(t *testing.T, cfg RunConfig) RunConfig {
		cfg = subdirProjectLayout(t, cfg, true)
		writeFile(t, filepath.Join(cfg.Workspace, ".git"), "gitdir: ../.git/worktrees/x\n")
		return cfg
	}
	// The agent's own private workspace with a checked-out .scion (plain
	// layout): left writable.
	plainOwn := func(t *testing.T, cfg RunConfig) RunConfig {
		ws := filepath.Join(t.TempDir(), "agents", "me", "workspace")
		mkdirs(t, filepath.Join(ws, ".scion", "templates"))
		cfg.Workspace = ws
		cfg.AgentOwnWorkspace = true
		return cfg
	}
	// A project kept in a subdirectory of the repository: the workspace is
	// that subdirectory of the main checkout, holding the project's .scion.
	subdirProject := func(withAgents bool) projectConfigLayout {
		return func(t *testing.T, cfg RunConfig) RunConfig {
			return subdirProjectLayout(t, cfg, withAgents)
		}
	}
	subdirMarker := func(t *testing.T, cfg RunConfig) RunConfig {
		root := filepath.Join(t.TempDir(), "repo")
		ws := filepath.Join(root, "sub")
		mkdirs(t, filepath.Join(root, ".git"))
		writeFile(t, filepath.Join(ws, ".scion"), "project-id: x\n")
		cfg.RepoRoot, cfg.Workspace = root, ws
		return cfg
	}
	gitClone := func(t *testing.T, cfg RunConfig) RunConfig {
		ws := filepath.Join(t.TempDir(), "clone")
		mkdirs(t, filepath.Join(ws, ".scion"))
		cfg.Workspace = ws
		cfg.GitClone = &api.GitCloneConfig{URL: "https://example.com/r.git"}
		return cfg
	}
	symlinkInside := func(t *testing.T, cfg RunConfig) RunConfig {
		root := filepath.Join(t.TempDir(), "repo")
		mkdirs(t, filepath.Join(root, "config", "scion"))
		if err := os.Symlink(filepath.Join("config", "scion"), filepath.Join(root, ".scion")); err != nil {
			t.Fatal(err)
		}
		cfg.RepoRoot, cfg.Workspace = root, root
		return cfg
	}
	plainSymlink := func(t *testing.T, cfg RunConfig) RunConfig {
		ws := filepath.Join(t.TempDir(), "project")
		mkdirs(t, filepath.Join(ws, "cfg"))
		if err := os.Symlink("cfg", filepath.Join(ws, ".scion")); err != nil {
			t.Fatal(err)
		}
		cfg.Workspace = ws
		return cfg
	}
	symlinkOutside := func(t *testing.T, cfg RunConfig) RunConfig {
		root := filepath.Join(t.TempDir(), "repo")
		outside := filepath.Join(t.TempDir(), "elsewhere")
		mkdirs(t, root, outside)
		if err := os.Symlink(outside, filepath.Join(root, ".scion")); err != nil {
			t.Fatal(err)
		}
		cfg.RepoRoot, cfg.Workspace = root, root
		return cfg
	}

	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		for _, tt := range []struct {
			name   string
			layout projectConfigLayout
			// roSrc is the read-only source relative to the workspace,
			// roTarget its container path; empty means no read-only mount.
			roSrc, roTarget string
			mask            string
			// maskNonApple overrides mask on Docker and Podman, where the
			// broker creates a missing agents dir so that it can be masked.
			maskNonApple string
			// markerRO: the .scion marker file is bound read-only on
			// Docker and Podman.
			markerRO bool
			// symlinkRefused: a symlinked .scion refuses the start.
			symlinkRefused bool
		}{
			{"shared, agents dir exists", sharedWith(true), ".scion", "/workspace/.scion", "/workspace/.scion/agents", "", false, false},
			{"shared, no agents dir", sharedWith(false), ".scion", "/workspace/.scion", "", "/workspace/.scion/agents", false, false},
			{"shared, .scion marker file", sharedMarker, "", "", "", "", true, false},
			{"shared, no .scion", sharedNoScion, "", "", "", "", false, false},
			{"shared, .scion symlink inside the workspace", symlinkInside, "", "", "", "", false, true},
			{"shared, .scion symlink outside the workspace", symlinkOutside, "", "", "", "", false, true},
			{"plain, in-place .scion with agents", plainWith(true), ".scion", "/workspace/.scion", "/workspace/.scion/agents", "", false, false},
			{"plain, in-place .scion", plainWith(false), ".scion", "/workspace/.scion", "", "", false, false},
			{"plain, agent home under .scion/agents", plainHomeInside, ".scion", "/workspace/.scion", "/workspace/.scion/agents", "", false, false},
			{"plain, .scion symlink", plainSymlink, "", "", "", "", false, true},
			{"plain, .scion marker file", plainMarker, "", "", "", "", true, false},
			{"project in a repo subdirectory, agents dir exists", subdirProject(true), ".scion", "/repo-root/sub/.scion", "/repo-root/sub/.scion/agents", "", false, false},
			{"project in a repo subdirectory, no agents dir", subdirProject(false), ".scion", "/repo-root/sub/.scion", "", "/repo-root/sub/.scion/agents", false, false},
			{"project in a repo subdirectory, .scion marker", subdirMarker, "", "", "", "", true, false},
			{"project in a repo subdirectory with an agent-created .git file", subdirWithGitFile, ".scion", "/repo-root/sub/.scion", "/repo-root/sub/.scion/agents", "", false, false},
			{"plain, agent's own workspace", plainOwn, "", "", "", "", false, false},
			{"worktree", worktree, "", "", "", "", false, false},
			{"git clone", gitClone, "", "", "", "", false, false},
		} {
			t.Run(rt+"/"+tt.name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				cfg := minimalRunConfig()
				cfg.BrokerMode = true
				cfg.RuntimeName = rt
				cfg.HomeDir = filepath.Join(t.TempDir(), "home")
				mkdirs(t, cfg.HomeDir)
				cfg = tt.layout(t, cfg)
				args, err := buildCommonRunArgs(cfg)
				if tt.markerRO && rt == appleContainerRuntimeName {
					// Apple cannot bind the marker read-only: refused.
					if !errors.Is(err, errProjectConfigMarkerNoBind) {
						t.Fatalf("buildCommonRunArgs error = %v, want errProjectConfigMarkerNoBind", err)
					}
					return
				}
				if tt.symlinkRefused {
					if !errors.Is(err, errProjectConfigSymlink) {
						t.Fatalf("buildCommonRunArgs error = %v, want errProjectConfigSymlink", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("buildCommonRunArgs: %v", err)
				}
				vols := volumeArgs(args)

				var ro []string
				for _, v := range vols {
					// The git admin narrowing mounts of the worktree
					// layout are read-only on their own account.
					if strings.HasSuffix(v, ":ro") && !strings.Contains(v, ":/repo-root/.git/") {
						ro = append(ro, v)
					}
				}
				var wantRO []string
				roSrc, roTarget := tt.roSrc, tt.roTarget
				if tt.markerRO && rt != appleContainerRuntimeName {
					// A marker file is bound read-only over itself on
					// runtimes that can bind a single file.
					containerWS := "/workspace"
					if cfg.RepoRoot != "" && cfg.Workspace != cfg.RepoRoot {
						rel, _ := filepath.Rel(cfg.RepoRoot, cfg.Workspace)
						containerWS = "/repo-root/" + filepath.ToSlash(rel)
					}
					roSrc, roTarget = ".scion", containerWS+"/.scion"
				}
				if roTarget != "" {
					src, err := filepath.EvalSymlinks(filepath.Join(cfg.Workspace, roSrc))
					if err != nil {
						t.Fatal(err)
					}
					wantRO = []string{src + ":" + roTarget + ":ro"}
				}
				if !slices.Equal(ro, wantRO) {
					t.Errorf("read-only mounts = %q, want %q", ro, wantRO)
				}

				// The agent's own home and its workspace stay writable.
				home, err := filepath.EvalSymlinks(cfg.HomeDir)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(vols, home+":/home/scion") {
					t.Errorf("agent home is not mounted read-write: %q", vols)
				}
				for _, v := range vols {
					if strings.HasPrefix(v, home) && strings.HasSuffix(v, ":ro") {
						t.Errorf("agent home mount is read-only: %s", v)
					}
				}
				workspaceRW := false
				for _, v := range vols {
					parts := strings.Split(v, ":")
					if len(parts) == 2 && (parts[1] == "/workspace" || strings.HasPrefix(parts[1], "/repo-root/")) {
						workspaceRW = true
					}
				}
				if !workspaceRW {
					t.Errorf("no read-write workspace mount in %q", vols)
				}

				// With a read-only bind, the agents mask follows it in the
				// -v/mount order the runtimes apply by depth.
				gotMask := ""
				for i := 0; i < len(args)-1; i++ {
					switch {
					case args[i] == "--tmpfs":
						gotMask = args[i+1]
					case args[i] == "--mount" && (strings.HasPrefix(args[i+1], "type=tmpfs,destination=/workspace") || strings.HasPrefix(args[i+1], "type=tmpfs,destination=/repo-root/")):
						gotMask = strings.TrimPrefix(args[i+1], "type=tmpfs,destination=")
					}
				}
				wantMask := tt.mask
				if tt.maskNonApple != "" && rt != appleContainerRuntimeName {
					wantMask = tt.maskNonApple
				}
				if gotMask != wantMask {
					t.Errorf("agents mask = %q, want %q", gotMask, wantMask)
				}
				if wantMask != "" {
					if fi, err := os.Stat(filepath.Join(cfg.Workspace, ".scion", "agents")); err != nil || !fi.IsDir() {
						t.Errorf("the masked agents dir does not exist on the broker: %v", err)
					}
					flag := "--mount"
					if rt == appleContainerRuntimeName {
						flag = "--tmpfs"
					}
					if !slices.Contains(args, flag) {
						t.Errorf("mask uses the wrong flag for %s: %q", rt, args)
					}
				}
			})
		}
	}
}

// In the full-repo-root fallback (workspace outside the repo root) the
// repo's .scion is shadowed with a tmpfs on every runtime: --mount on
// Docker and Podman, --tmpfs on Apple, which drops --mount arguments. On
// Apple the mask is added only when the directory exists.
func TestBuildCommonRunArgs_FallbackScionMask(t *testing.T) {
	for _, tt := range []struct {
		runtime   string
		withScion bool
		flag      string
		value     string
	}{
		{"docker", true, "--mount", "type=tmpfs,destination=/repo-root/.scion"},
		{"podman", true, "--mount", "type=tmpfs,destination=/repo-root/.scion"},
		{appleContainerRuntimeName, true, "--tmpfs", "/repo-root/.scion"},
		{appleContainerRuntimeName, false, "", ""},
	} {
		name := tt.runtime
		if !tt.withScion {
			name += "/no .scion"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := filepath.Join(t.TempDir(), "repo")
			mkdirs(t, filepath.Join(root, ".git"))
			if tt.withScion {
				mkdirs(t, filepath.Join(root, ".scion", "agents", "other", "home"))
			}
			ws := filepath.Join(t.TempDir(), "outside")
			mkdirs(t, ws)
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = tt.runtime
			cfg.RepoRoot, cfg.Workspace = root, ws
			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			for i := 0; i < len(args)-1; i++ {
				if (args[i] == "--tmpfs" || args[i] == "--mount") && strings.Contains(args[i+1], "/repo-root/.scion") && tt.flag == "" {
					t.Errorf("unexpected mask %s %s", args[i], args[i+1])
				}
			}
			if tt.flag != "" && !hasArgPair(args, tt.flag, tt.value) {
				t.Errorf("args lack %s %s: %q", tt.flag, tt.value, args)
			}
			if tt.runtime == appleContainerRuntimeName {
				if got := stripUnsupportedAppleFlags(args); tt.flag != "" && !hasArgPair(got, tt.flag, tt.value) {
					t.Errorf("the Apple mask does not survive stripUnsupportedAppleFlags: %q", got)
				}
			}
		})
	}
}

func TestWorkspaceProjectConfigMount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, filepath.Join(root, ".scion"))
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	src, tgt, ok, err := workspaceProjectConfigMount(root, "/workspace", false)
	if err != nil || !ok || src != filepath.Join(resolved, ".scion") || tgt != "/workspace/.scion" {
		t.Errorf("directory: got (%q, %q, %v, %v)", src, tgt, ok, err)
	}
	for _, tt := range []struct{ name, ws, cw string }{
		{"empty workspace", "", "/workspace"},
		{"empty container path", root, ""},
		{"missing workspace", filepath.Join(root, "missing"), "/workspace"},
	} {
		if _, _, ok, err := workspaceProjectConfigMount(tt.ws, tt.cw, true); ok || err != nil {
			t.Errorf("%s: (ok=%v, err=%v), want (false, nil)", tt.name, ok, err)
		}
	}

	// A marker file is mounted only when the runtime can bind a file.
	marker := filepath.Join(t.TempDir(), "marker")
	writeFile(t, filepath.Join(marker, ".scion"), "project-id: x\n")
	resolvedMarker, err := filepath.EvalSymlinks(marker)
	if err != nil {
		t.Fatal(err)
	}
	if src, tgt, ok, err := workspaceProjectConfigMount(marker, "/workspace", true); err != nil || !ok || src != filepath.Join(resolvedMarker, ".scion") || tgt != "/workspace/.scion" {
		t.Errorf("marker, file binds: got (%q, %q, %v, %v)", src, tgt, ok, err)
	}
	if _, _, ok, err := workspaceProjectConfigMount(marker, "/workspace", false); ok || !errors.Is(err, errProjectConfigMarkerNoBind) {
		t.Errorf("marker, no file binds: (ok=%v, err=%v), want errProjectConfigMarkerNoBind", ok, err)
	}

	// Any symlinked .scion is refused, wherever it points.
	for _, target := range []string{".", "cfg", t.TempDir()} {
		ws := filepath.Join(t.TempDir(), "ws")
		mkdirs(t, filepath.Join(ws, "cfg"))
		if err := os.Symlink(target, filepath.Join(ws, ".scion")); err != nil {
			t.Fatal(err)
		}
		if _, _, ok, err := workspaceProjectConfigMount(ws, "/workspace", true); ok || !errors.Is(err, errProjectConfigSymlink) {
			t.Errorf(".scion -> %s: (ok=%v, err=%v), want errProjectConfigSymlink", target, ok, err)
		}
	}
}

// In the full-repo-root fallback on Apple, a .scion marker file can be
// neither masked nor bound read-only, so the start is refused.
func TestBuildCommonRunArgs_FallbackAppleMarkerRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, ".scion"), "project-id: x\n")
	ws := filepath.Join(t.TempDir(), "outside")
	mkdirs(t, ws)
	cfg := minimalRunConfig()
	cfg.BrokerMode = true
	cfg.RuntimeName = appleContainerRuntimeName
	cfg.RepoRoot, cfg.Workspace = root, ws
	if _, err := buildCommonRunArgs(cfg); !errors.Is(err, errProjectConfigMarkerNoBind) {
		t.Fatalf("buildCommonRunArgs error = %v, want errProjectConfigMarkerNoBind", err)
	}
}
