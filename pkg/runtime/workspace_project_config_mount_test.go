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
	"path"
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
		// #5865: the worktree branch resolves the broker-recorded worktree admin
		// and binds a per-agent private .git/config. Use a real registered
		// worktree (setupHubManagedBaseRepo) so the start is not refused; the
		// agent's own .scion stays writable (no read-only bind).
		root, ws := setupHubManagedBaseRepo(t, "me")
		mkdirs(t, filepath.Join(ws, ".scion", "templates"))
		cfg.RepoRoot, cfg.Workspace = root, ws
		cfg.AgentOwnWorkspace = true
		cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
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
			{"plain, in-place .scion", plainWith(false), ".scion", "/workspace/.scion", "", "/workspace/.scion/agents", false, false},
			{"plain, agent home under .scion/agents", plainHomeInside, ".scion", "/workspace/.scion", "/workspace/.scion/agents", "", false, false},
			{"plain, .scion symlink", plainSymlink, "", "", "", "", false, true},
			{"plain, .scion marker file", plainMarker, "", "", "", "", true, false},
			// A project kept in a plain subdirectory of a repository
			// (RepoRoot set, workspace a subdir that is not a registered
			// worktree) drives the worktree branch and is refused; see
			// TestBuildCommonRunArgs_SubdirProjectWorktreeBranchRefused.
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
					// The git admin narrowing mounts of the worktree layout
					// (the read-only base .git root and its read-only children,
					// e.g. .git/config) are read-only on their own account, not
					// project-config protection; exclude the whole /repo-root/.git
					// subtree from this comparison.
					if strings.HasSuffix(v, ":ro") && !strings.Contains(v, ":/repo-root/.git") {
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

// A project kept in a plain subdirectory of a repository (RepoRoot set,
// workspace a subdirectory that is NOT a registered worktree) drives the
// worktree branch, where the broker-recorded worktree admin cannot be
// resolved: the start is refused fail-closed on every runtime. Production
// never routes a subdir project this way — detectRepoRoot returns "" for a
// subdir project with no own .git, taking the config-Workspace-only branch,
// where real subdir-project protection is covered (config-only tests). This
// pins that the RepoRoot-set shape is refused rather than silently mounting
// the enclosing repository or leaving the project .scion writable.
func TestBuildCommonRunArgs_SubdirProjectWorktreeBranchRefused(t *testing.T) {
	marker := func(t *testing.T, cfg RunConfig) RunConfig {
		root := filepath.Join(t.TempDir(), "repo")
		ws := filepath.Join(root, "sub")
		mkdirs(t, filepath.Join(root, ".git"))
		writeFile(t, filepath.Join(ws, ".scion"), "project-id: x\n")
		cfg.RepoRoot, cfg.Workspace = root, ws
		return cfg
	}
	// A subdirectory project where an agent created a .git gitfile: the
	// trusted resolver ignores that agent-writable file, so the start is still
	// refused (no broker-recorded admin back-link).
	gitFile := func(t *testing.T, cfg RunConfig) RunConfig {
		cfg = subdirProjectLayout(t, cfg, true)
		writeFile(t, filepath.Join(cfg.Workspace, ".git"), "gitdir: ../.git/worktrees/x\n")
		return cfg
	}
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		for _, tt := range []struct {
			name   string
			layout projectConfigLayout
		}{
			{"agents dir exists", func(t *testing.T, cfg RunConfig) RunConfig { return subdirProjectLayout(t, cfg, true) }},
			{"no agents dir", func(t *testing.T, cfg RunConfig) RunConfig { return subdirProjectLayout(t, cfg, false) }},
			{".scion marker", marker},
			{"agent-created .git file", gitFile},
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
				if !errors.Is(err, errWorktreeAdminUnresolved) {
					t.Fatalf("buildCommonRunArgs error = %v, want errWorktreeAdminUnresolved", err)
				}
				if !strings.Contains(err.Error(), "cannot resolve the broker-recorded worktree admin directory") {
					t.Errorf("error message lacks the worktree-admin reason: %v", err)
				}
				if args != nil {
					t.Errorf("fail-closed: expected nil args, got %q", args)
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
	// #5865 removed the full-repo-root fallback: an UNREGISTERED workspace outside
	// the repo root is refused fail-closed (errOutsideRootNotRegistered), so there
	// is no /repo-root mount and no .scion mask. A REGISTERED outside-root worktree
	// is covered by TestBuildCommonRunArgs_RegisteredOutsideWorktreeProjectConfig.
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		t.Run(rt, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := filepath.Join(t.TempDir(), "repo")
			mkdirs(t, filepath.Join(root, ".git"))
			mkdirs(t, filepath.Join(root, ".scion", "agents", "other", "home"))
			ws := filepath.Join(t.TempDir(), "outside")
			mkdirs(t, ws)
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = rt
			cfg.RepoRoot, cfg.Workspace = root, ws
			before := fsSnapshot(t, root, ws)
			args, err := buildCommonRunArgs(cfg)
			if !errors.Is(err, errOutsideRootNotRegistered) {
				t.Fatalf("buildCommonRunArgs error = %v, want errOutsideRootNotRegistered", err)
			}
			if !strings.Contains(err.Error(), "is not a registered worktree") {
				t.Errorf("error message lacks the registered-worktree reason: %v", err)
			}
			if args != nil {
				t.Errorf("fail-closed: expected nil args, got %q", args)
			}
			assertNothingCreated(t, before, root, ws)
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
	// #5865 removed the full-repo-root fallback, so an unregistered outside
	// workspace is refused before any .scion marker handling. The Apple marker
	// refusal for a REGISTERED outside worktree's own .scion is exercised by
	// TestBuildCommonRunArgs_FallbackOwnScionMarkerAndSymlink.
	before := fsSnapshot(t, root, ws)
	args, err := buildCommonRunArgs(cfg)
	if !errors.Is(err, errOutsideRootNotRegistered) {
		t.Fatalf("buildCommonRunArgs error = %v, want errOutsideRootNotRegistered", err)
	}
	if !strings.Contains(err.Error(), "is not a registered worktree") {
		t.Errorf("error message lacks the registered-worktree reason: %v", err)
	}
	if args != nil {
		t.Errorf("fail-closed: expected nil args, got %q", args)
	}
	assertNothingCreated(t, before, root, ws)
}

// A .scion entry that cannot be checked (other than not existing) refuses
// the mount decision with an error instead of leaving the workspace
// without its read-only bind.
func TestWorkspaceProjectConfigMount_UncheckableEntryRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	ws := filepath.Join(t.TempDir(), "ws")
	mkdirs(t, filepath.Join(ws, ".scion"))
	if err := os.Chmod(ws, 0o600); err != nil { // no search permission
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })
	if _, _, ok, err := workspaceProjectConfigMount(ws, "/workspace", true); ok || err == nil {
		t.Fatalf("(ok=%v, err=%v), want an error", ok, err)
	}
	// A workspace path that cannot be resolved for another reason than not
	// existing (here a parent without search permission) is refused too.
	parent := filepath.Join(t.TempDir(), "locked")
	inner := filepath.Join(parent, "ws")
	mkdirs(t, inner)
	if err := os.Chmod(parent, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if _, _, ok, err := workspaceProjectConfigMount(inner, "/workspace", true); ok || err == nil {
		t.Fatalf("unresolvable workspace: (ok=%v, err=%v), want an error", ok, err)
	}
	// A workspace that does not exist yet has nothing to protect.
	if _, _, ok, err := workspaceProjectConfigMount(filepath.Join(t.TempDir(), "absent"), "/workspace", true); ok || err != nil {
		t.Fatalf("absent workspace: (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

// The Apple marker refusal says what to do on that runtime.
func TestErrProjectConfigMarkerNoBindText(t *testing.T) {
	msg := errProjectConfigMarkerNoBind.Error()
	for _, want := range []string{"commit a .scion/ directory to the repository", "use a non-shared workspace", "on this runtime"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

// In the full-repo-root fallback, a project kept in a subdirectory of the
// repository has its own .scion below the repo root: it is masked (a
// directory, with its agents root and homes) or bound read-only (a marker
// file; refused on Apple). A project .scion at the repo root itself gets
// only the existing /repo-root/.scion mask.
func TestBuildCommonRunArgs_FallbackNestedProjectConfig(t *testing.T) {
	// #5865 removed the full-repo-root fallback, so an unregistered workspace
	// outside the repo root is refused regardless of where the project's .scion
	// sits; the nested-.scion read-only mask is now exercised in the shared-plain
	// and config-only layouts (TestBuildCommonRunArgs_SharedPlainNestedProjectConfigReadOnly,
	// TestBuildCommonRunArgs_ConfigOnlyNestedProjectConfigReadOnly).
	for _, rt := range []string{"docker", appleContainerRuntimeName} {
		t.Run(rt, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			repo := filepath.Join(t.TempDir(), "repo")
			mkdirs(t, filepath.Join(repo, ".git"), filepath.Join(repo, "sub", ".scion"))
			ws := filepath.Join(t.TempDir(), "outside")
			mkdirs(t, ws)
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = rt
			cfg.RepoRoot, cfg.Workspace = repo, ws
			cfg.ProjectConfigPath = filepath.Join(repo, "sub", ".scion")
			before := fsSnapshot(t, repo, ws)
			args, err := buildCommonRunArgs(cfg)
			if !errors.Is(err, errOutsideRootNotRegistered) {
				t.Fatalf("buildCommonRunArgs error = %v, want errOutsideRootNotRegistered", err)
			}
			if !strings.Contains(err.Error(), "is not a registered worktree") {
				t.Errorf("error message lacks the registered-worktree reason: %v", err)
			}
			if args != nil {
				t.Errorf("fail-closed: expected nil args, got %q", args)
			}
			assertNothingCreated(t, before, repo, ws)
		})
	}
}

func TestNestedProjectConfigRel(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	mkdirs(t, filepath.Join(repo, "sub"))
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		path string
		rel  string
		ok   bool
	}{
		{filepath.Join(repo, "sub", ".scion"), filepath.Join("sub", ".scion"), true},
		{filepath.Join(repo, "sub"), filepath.Join("sub", ".scion"), true},
		{filepath.Join(repo, ".scion"), "", false},
		{repo, "", false},
		{filepath.Join(t.TempDir(), "elsewhere", ".scion"), "", false},
		{"", "", false},
	} {
		rel, ok := nestedProjectConfigRel(resolved, tt.path)
		if rel != tt.rel || ok != tt.ok {
			t.Errorf("%s: (%q, %v), want (%q, %v)", tt.path, rel, ok, tt.rel, tt.ok)
		}
	}
}

// In the plain layout on Docker and Podman the agents root is masked even
// when it does not exist yet: the broker creates it before the run, so an
// agent dir created there later (after the run args were built) lies under
// the mask. Apple masks only an existing agents root.
func TestBuildCommonRunArgs_PlainAgentsMaskCoversLaterAgentDirs(t *testing.T) {
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		t.Run(rt, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			ws := filepath.Join(t.TempDir(), "project")
			writeFile(t, filepath.Join(ws, ".scion", "settings.yaml"), "x: y\n")
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = rt
			cfg.HomeDir = filepath.Join(t.TempDir(), "home")
			mkdirs(t, cfg.HomeDir)
			cfg.Workspace = ws
			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			var masks []string
			for i := 0; i < len(args)-1; i++ {
				switch {
				case args[i] == "--tmpfs":
					masks = append(masks, args[i+1])
				case args[i] == "--mount" && strings.HasPrefix(args[i+1], "type=tmpfs,destination="):
					masks = append(masks, strings.TrimPrefix(args[i+1], "type=tmpfs,destination="))
				}
			}
			if rt == appleContainerRuntimeName {
				if len(masks) != 0 {
					t.Errorf("Apple: unexpected masks %q (the agents root did not exist)", masks)
				}
				return
			}
			if fi, err := os.Stat(filepath.Join(ws, ".scion", "agents")); err != nil || !fi.IsDir() {
				t.Fatalf("the agents root was not created before the run: %v", err)
			}
			// An agent dir created after the run args were built.
			later := filepath.Join(ws, ".scion", "agents", "later-agent", "home")
			mkdirs(t, later)
			rel, err := filepath.Rel(ws, later)
			if err != nil {
				t.Fatal(err)
			}
			inContainer := path.Join("/workspace", filepath.ToSlash(rel))
			covered := false
			for _, m := range masks {
				if inContainer == m || strings.HasPrefix(inContainer, m+"/") {
					covered = true
				}
			}
			if !covered {
				t.Errorf("%s is not under any mask %q", inContainer, masks)
			}
		})
	}
}
