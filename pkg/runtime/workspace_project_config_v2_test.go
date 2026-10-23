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
	"strings"
	"testing"
)

// fsSnapshot records every path under roots, for the fail-closed
// "nothing created on disk" check on a refused start.
func fsSnapshot(t *testing.T, roots ...string) map[string]bool {
	t.Helper()
	m := map[string]bool{}
	for _, r := range roots {
		_ = filepath.Walk(r, func(p string, _ os.FileInfo, err error) error {
			if err == nil {
				m[p] = true
			}
			return nil
		})
	}
	return m
}

// assertNothingCreated fails if any path under roots is new since before.
func assertNothingCreated(t *testing.T, before map[string]bool, roots ...string) {
	t.Helper()
	for p := range fsSnapshot(t, roots...) {
		if !before[p] {
			t.Errorf("refused start created %s on disk", p)
		}
	}
}

// tmpfsTargets returns the destinations of every tmpfs mask in args, from both
// the Apple --tmpfs form and the docker/podman --mount type=tmpfs form.
func tmpfsTargets(args []string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		switch {
		case args[i] == "--tmpfs":
			out = append(out, args[i+1])
		case args[i] == "--mount" && strings.Contains(args[i+1], "type=tmpfs"):
			for _, kv := range strings.Split(args[i+1], ",") {
				if v, ok := strings.CutPrefix(kv, "destination="); ok {
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// bindTargetIndex returns the position in binds of the first bind with the
// given target, or -1.
func bindTargetIndex(binds []layoutBind, target string) int {
	for i, b := range binds {
		if b.target == target {
			return i
		}
	}
	return -1
}

// TestBuildCommonRunArgs_RegisteredOutsideWorktreeProjectConfig covers the
// workspace outside the repository root that IS a registered worktree. Only the
// base .git (narrowed) and the workspace are mounted — never the whole main
// checkout — and the worktree's own project .scion is bound read-only. The
// negative assertions pin that nothing above the project root is exposed: no
// mount targets /repo-root (other than /repo-root/.git) and no mount source is
// the main checkout root or its .scion. (A regression that made /repo-root
// read-write would turn these red.)
func TestBuildCommonRunArgs_RegisteredOutsideWorktreeProjectConfig(t *testing.T) {
	for _, rt := range []string{"docker", appleContainerRuntimeName} {
		t.Run(rt, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("HOME", filepath.Join(base, "home"))
			main := filepath.Join(base, "main")
			if err := os.MkdirAll(main, 0o755); err != nil {
				t.Fatal(err)
			}
			layoutGit(t, main, "init", "-q")
			layoutGit(t, main, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
			// A registered worktree whose path is OUTSIDE the main checkout.
			ws := filepath.Join(base, "outside-wt")
			layoutGit(t, main, "worktree", "add", "-q", "-b", "dev", ws)
			// The worktree holds this project's .scion (settings/templates), which
			// the broker reads; it must be mounted read-only.
			mkdirs(t, filepath.Join(ws, ".scion", "templates"))

			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = rt
			cfg.HomeDir = filepath.Join(base, "agenthome")
			mkdirs(t, cfg.HomeDir)
			cfg.RepoRoot, cfg.Workspace = main, ws
			cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)

			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			if rt == appleContainerRuntimeName {
				args = stripUnsupportedAppleFlags(args)
			}
			binds := layoutBinds(t, args)

			resolvedMain := layoutRealPath(t, main)
			resolvedWs := layoutRealPath(t, ws)
			resolvedScion := layoutRealPath(t, filepath.Join(ws, ".scion"))

			// The workspace is bound read-write at /workspace.
			wsIdx := -1
			scionIdx := -1
			sawWorkspaceRW := false
			for i, b := range binds {
				src := b.source
				if r, err := filepath.EvalSymlinks(src); err == nil {
					src = r
				}
				if b.target == "/workspace" {
					wsIdx = i
					if !b.readOnly && src == resolvedWs {
						sawWorkspaceRW = true
					}
				}
				if b.target == "/workspace/.scion" {
					scionIdx = i
					if !b.readOnly {
						t.Errorf("project .scion bound read-write: %+v", b)
					}
					if src != resolvedScion {
						t.Errorf("project .scion source = %s, want %s", src, resolvedScion)
					}
				}
				// NEGATIVE: nothing above the project root is exposed. Only
				// /repo-root/.git (and below) may be mounted under /repo-root.
				if b.target == "/repo-root" || (strings.HasPrefix(b.target, "/repo-root/") && b.target != "/repo-root/.git" && !strings.HasPrefix(b.target, "/repo-root/.git/")) {
					t.Errorf("mount exposes a path under /repo-root outside .git: %+v", b)
				}
				// NEGATIVE: the main checkout root and its .scion are never a
				// mount source.
				if src == resolvedMain || src == filepath.Join(resolvedMain, ".scion") {
					t.Errorf("mount source is the main checkout root or its .scion: %+v", b)
				}
			}
			if !sawWorkspaceRW {
				t.Errorf("no read-write /workspace bind of %s in %+v", resolvedWs, binds)
			}
			if scionIdx < 0 {
				t.Fatalf("project .scion is not mounted read-only; binds: %+v", binds)
			}
			// The project .scion bind follows the /workspace bind in arg order.
			if wsIdx < 0 || scionIdx < wsIdx {
				t.Errorf(".scion bind (idx %d) must come after the /workspace bind (idx %d)", scionIdx, wsIdx)
			}
			// No tmpfs mask lands under /repo-root in this layout.
			for _, m := range tmpfsTargets(args) {
				if m == "/repo-root" || strings.HasPrefix(m, "/repo-root/") {
					t.Errorf("unexpected tmpfs mask under /repo-root: %s", m)
				}
			}
		})
	}
}

// nestedProjectConfigCase sets up a workspace whose project configuration lives
// in a SUBDIRECTORY (<ws>/sub/.scion), returning the run config. sharedPlain
// selects the shared-plain branch (RepoRoot == Workspace) or the config-only
// branch (RepoRoot == ""); withAgents adds a nested agents/ dir to mask.
func nestedProjectConfigCase(t *testing.T, rt string, sharedPlain, withAgents bool) RunConfig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := filepath.Join(t.TempDir(), "workspace")
	mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
	if withAgents {
		mkdirs(t, filepath.Join(ws, "sub", ".scion", "agents", "other"))
	}
	cfg := minimalRunConfig()
	cfg.BrokerMode = true
	cfg.RuntimeName = rt
	cfg.HomeDir = filepath.Join(t.TempDir(), "home")
	mkdirs(t, cfg.HomeDir)
	cfg.Workspace = ws
	cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
	if sharedPlain {
		// Shared-plain: the workspace IS the repo root. Use an old-style (non
		// git) shared workspace so the layout is reached on every runtime (a
		// .git directory would make the shared-plain git narrowing refuse on
		// Apple, which is a separate, already-tested behaviour).
		cfg.RepoRoot = ws
	}
	return cfg
}

// assertNestedProjectConfig checks that <ws>/sub/.scion is bound read-only at
// /workspace/sub/.scion, after the /workspace bind, and that its agents/ is
// masked with a tmpfs only when it exists.
func assertNestedProjectConfig(t *testing.T, cfg RunConfig, withAgents bool) {
	t.Helper()
	assertNestedProjectConfigAt(t, cfg, "/workspace", withAgents)
}

// assertNestedProjectConfigAt is assertNestedProjectConfig for a workspace bound
// at an arbitrary container path (e.g. "/repo-root/worktrees/me" for the non-own
// in-repo worktree branch).
func assertNestedProjectConfigAt(t *testing.T, cfg RunConfig, containerWorkspace string, withAgents bool) {
	t.Helper()
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	if cfg.RuntimeName == appleContainerRuntimeName {
		args = stripUnsupportedAppleFlags(args)
	}
	binds := layoutBinds(t, args)

	wantSrc := layoutRealPath(t, filepath.Join(cfg.Workspace, "sub", ".scion"))
	wantTarget := containerWorkspace + "/sub/.scion"
	nestedIdx := -1
	for i, b := range binds {
		if b.target != wantTarget {
			continue
		}
		nestedIdx = i
		if !b.readOnly {
			t.Errorf("nested .scion bound read-write: %+v", b)
		}
		src := b.source
		if r, err := filepath.EvalSymlinks(src); err == nil {
			src = r
		}
		if src != wantSrc {
			t.Errorf("nested .scion source = %s, want %s", src, wantSrc)
		}
	}
	if nestedIdx < 0 {
		t.Fatalf("nested .scion is not mounted read-only; binds: %+v", binds)
	}
	// The nested target sits UNDER the workspace and after the workspace bind.
	if !strings.HasPrefix(wantTarget, containerWorkspace+"/") {
		t.Fatalf("nested target %s is not under %s", wantTarget, containerWorkspace)
	}
	wsIdx := bindTargetIndex(binds, containerWorkspace)
	if wsIdx < 0 || nestedIdx < wsIdx {
		t.Errorf("nested .scion bind (idx %d) must come after the workspace bind (idx %d)", nestedIdx, wsIdx)
	}

	masks := tmpfsTargets(args)
	wantMask := wantTarget + "/agents"
	hasMask := false
	for _, m := range masks {
		if m == wantMask {
			hasMask = true
		}
	}
	if withAgents && !hasMask {
		t.Errorf("nested agents dir exists but is not masked; tmpfs targets: %q", masks)
	}
	if !withAgents && hasMask {
		t.Errorf("no nested agents dir, but a tmpfs mask was added; tmpfs targets: %q", masks)
	}

	// Every tmpfs mask present in these args must be emitted AFTER all
	// -v binds, so each tmpfs lands over its read-only .scion bind rather than
	// before it. In this helper the nested mask is what's present. The two mask
	// emit paths are each covered by a caller that also calls assertMasksAfterBinds:
	// WorktreeNotOwnNested (which creates <ws>/.scion/agents) exercises the
	// sharedWorkspaceAgentsMask block; FallbackOwnScionAgentsMask exercises the
	// nested/own no-create mask flush (maskAgentsIfPresent).
	if withAgents {
		assertMasksAfterBinds(t, args)
	}
}

// assertMasksAfterBinds pins that no tmpfs mask (--tmpfs or --mount type=tmpfs)
// present in args is emitted before any -v bind: the last -v index must precede
// the first mask index. It covers whatever masks are present — nested and the
// shared-workspace mask — in the args it is given.
func assertMasksAfterBinds(t *testing.T, args []string) {
	t.Helper()
	lastBind, firstMask := -1, -1
	for i := 0; i < len(args)-1; i++ {
		switch {
		case args[i] == "-v":
			lastBind = i
		case args[i] == "--tmpfs", args[i] == "--mount" && strings.HasPrefix(args[i+1], "type=tmpfs"):
			if firstMask < 0 {
				firstMask = i
			}
		}
	}
	if firstMask < 0 {
		t.Errorf("no tmpfs mask found for the ordering check; args: %q", args)
	} else if lastBind >= 0 && firstMask < lastBind {
		t.Errorf("a tmpfs mask (idx %d) is emitted before a -v bind (idx %d); every mask must follow all binds", firstMask, lastBind)
	}
}

// TestBuildCommonRunArgs_SharedPlainNestedProjectConfigReadOnly proves a project
// kept in a subdirectory of a shared-plain workspace (its .scion is
// <ws>/sub/.scion) is bound read-only with its agents/ masked, so the real
// project config is not writable under the read-write /workspace bind. (Removing
// ONLY the shared-plain applyNestedProjectConfig call must turn this red.)
func TestBuildCommonRunArgs_SharedPlainNestedProjectConfigReadOnly(t *testing.T) {
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		for _, withAgents := range []bool{true, false} {
			name := rt
			if !withAgents {
				name += "/no agents dir"
			}
			t.Run(name, func(t *testing.T) {
				cfg := nestedProjectConfigCase(t, rt, true, withAgents)
				assertNestedProjectConfig(t, cfg, withAgents)
			})
		}
	}
}

// TestBuildCommonRunArgs_ConfigOnlyNestedProjectConfigReadOnly proves the same
// nested-config protection in the config-Workspace-only branch (RepoRoot ""),
// for example a settings workspace_path pointing at an enclosing directory with
// the project .scion below it. (Removing ONLY the config-only
// applyNestedProjectConfig call must turn this red.)
func TestBuildCommonRunArgs_ConfigOnlyNestedProjectConfigReadOnly(t *testing.T) {
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		for _, withAgents := range []bool{true, false} {
			name := rt
			if !withAgents {
				name += "/no agents dir"
			}
			t.Run(name, func(t *testing.T) {
				cfg := nestedProjectConfigCase(t, rt, false, withAgents)
				assertNestedProjectConfig(t, cfg, withAgents)
			})
		}
	}
}

// TestBuildCommonRunArgs_TemplateWorkspacePlainSubdirRefused covers the refusal of
// a template-supplied /workspace pointing at a plain subdirectory of a
// repository (not a registered worktree). There is no broker-recorded
// worktree admin for a plain subdirectory, so no per-agent private .git surface
// can be built, and the start fails closed rather than mounting the enclosing
// repository or leaving the project config writable.
func TestBuildCommonRunArgs_TemplateWorkspacePlainSubdirRefused(t *testing.T) {
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		t.Run(rt, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := filepath.Join(t.TempDir(), "repo")
			ws := filepath.Join(root, "sub")
			mkdirs(t, filepath.Join(root, ".git"), filepath.Join(ws, ".scion", "templates"))
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = rt
			cfg.HomeDir = filepath.Join(t.TempDir(), "home")
			mkdirs(t, cfg.HomeDir)
			// The template resolves the agent workspace to this plain subdirectory
			// of the repository.
			cfg.RepoRoot, cfg.Workspace = root, ws

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
			// It is refused, not silently routed through the outside-root fallback.
			if errors.Is(err, errOutsideRootNotRegistered) {
				t.Errorf("a plain in-repo subdirectory should hit the worktree-admin refusal, not the outside-root refusal: %v", err)
			}
		})
	}
}

// registeredOutsideWorktree creates a real git main repo and a registered
// worktree OUTSIDE its root, returning (main, ws). Used for the outside-root
// fallback (RepoRoot=main, Workspace=ws, not the agent's own).
func registeredOutsideWorktree(t *testing.T) (main, ws string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	main = filepath.Join(base, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	layoutGit(t, main, "init", "-q")
	layoutGit(t, main, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	ws = filepath.Join(base, "outside-wt")
	layoutGit(t, main, "worktree", "add", "-q", "-b", "dev", ws)
	return main, ws
}

// assertNoRepoRootExposure pins the negative property: no mount targets
// /repo-root (other than /repo-root/.git) and no mount source is the main
// checkout root or its .scion, and no tmpfs lands under /repo-root. A regression
// that made /repo-root read-write would turn this red.
func assertNoRepoRootExposure(t *testing.T, cfg RunConfig, main string) {
	t.Helper()
	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	if cfg.RuntimeName == appleContainerRuntimeName {
		args = stripUnsupportedAppleFlags(args)
	}
	resolvedMain := layoutRealPath(t, main)
	for _, b := range layoutBinds(t, args) {
		if b.target == "/repo-root" || (strings.HasPrefix(b.target, "/repo-root/") && b.target != "/repo-root/.git" && !strings.HasPrefix(b.target, "/repo-root/.git/")) {
			t.Errorf("mount exposes a path under /repo-root outside .git: %+v", b)
		}
		src := b.source
		if r, err := filepath.EvalSymlinks(src); err == nil {
			src = r
		}
		if src == resolvedMain || src == filepath.Join(resolvedMain, ".scion") {
			t.Errorf("mount source is the main checkout root or its .scion: %+v", b)
		}
	}
	for _, m := range tmpfsTargets(args) {
		if m == "/repo-root" || strings.HasPrefix(m, "/repo-root/") {
			t.Errorf("unexpected tmpfs mask under /repo-root: %s", m)
		}
	}
}

// The outside-root fallback binds a nested <ws>/sub/.scion read-only and
// masks its agents/ (when present), on docker and Apple, while
// still exposing nothing under /repo-root. (Removing ONLY the fallback
// applyNestedProjectConfig call turns this red.)
func TestBuildCommonRunArgs_FallbackNestedProjectConfigReadOnly(t *testing.T) {
	for _, rt := range []string{"docker", appleContainerRuntimeName} {
		for _, withAgents := range []bool{true, false} {
			name := rt
			if !withAgents {
				name += "/no agents dir"
			}
			t.Run(name, func(t *testing.T) {
				main, ws := registeredOutsideWorktree(t)
				mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
				if withAgents {
					mkdirs(t, filepath.Join(ws, "sub", ".scion", "agents", "other"))
				}
				cfg := minimalRunConfig()
				cfg.BrokerMode = true
				cfg.RuntimeName = rt
				cfg.HomeDir = filepath.Join(t.TempDir(), "home")
				mkdirs(t, cfg.HomeDir)
				cfg.RepoRoot, cfg.Workspace = main, ws
				cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
				cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
				assertNestedProjectConfig(t, cfg, withAgents)
				assertNoRepoRootExposure(t, cfg, main)
			})
		}
	}
}

// The outside-root fallback binds the bound workspace's OWN <ws>/.scion
// read-only and masks its agents/ ONLY when it already exists (never created).
// (Removing ONLY the fallback maskAgentsIfPresent call turns the with-agents
// case red.)
func TestBuildCommonRunArgs_FallbackOwnScionAgentsMask(t *testing.T) {
	for _, rt := range []string{"docker", appleContainerRuntimeName} {
		for _, withAgents := range []bool{true, false} {
			name := rt
			if !withAgents {
				name += "/no agents dir"
			}
			t.Run(name, func(t *testing.T) {
				main, ws := registeredOutsideWorktree(t)
				mkdirs(t, filepath.Join(ws, ".scion", "templates"))
				if withAgents {
					mkdirs(t, filepath.Join(ws, ".scion", "agents", "other"))
				}
				cfg := minimalRunConfig()
				cfg.BrokerMode = true
				cfg.RuntimeName = rt
				cfg.HomeDir = filepath.Join(t.TempDir(), "home")
				mkdirs(t, cfg.HomeDir)
				cfg.RepoRoot, cfg.Workspace = main, ws
				cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)

				args, err := buildCommonRunArgs(cfg)
				if err != nil {
					t.Fatalf("buildCommonRunArgs: %v", err)
				}
				if rt == appleContainerRuntimeName {
					args = stripUnsupportedAppleFlags(args)
				}
				// Own .scion bound read-only at /workspace/.scion.
				wantSrc := layoutRealPath(t, filepath.Join(ws, ".scion"))
				foundRO := false
				for _, b := range layoutBinds(t, args) {
					if b.target != "/workspace/.scion" {
						continue
					}
					foundRO = true
					if !b.readOnly {
						t.Errorf("own .scion bound read-write: %+v", b)
					}
					src := b.source
					if r, e := filepath.EvalSymlinks(src); e == nil {
						src = r
					}
					if src != wantSrc {
						t.Errorf("own .scion source = %s, want %s", src, wantSrc)
					}
				}
				if !foundRO {
					t.Fatalf("own .scion not mounted read-only; args: %q", args)
				}
				// agents mask present only when the dir exists (never created).
				const wantMask = "/workspace/.scion/agents"
				hasMask := false
				for _, m := range tmpfsTargets(args) {
					if m == wantMask {
						hasMask = true
					}
				}
				if withAgents != hasMask {
					t.Errorf("agents mask = %v, want %v (withAgents=%v); masks: %q", hasMask, withAgents, withAgents, tmpfsTargets(args))
				}
				if withAgents {
					// The fallback own-.scion mask (via maskAgentsIfPresent) is
					// emitted after all binds.
					assertMasksAfterBinds(t, args)
				}
				if !withAgents {
					if _, serr := os.Stat(filepath.Join(ws, ".scion", "agents")); serr == nil {
						t.Errorf("fallback created <ws>/.scion/agents on disk; it must never create it")
					}
				}
				assertNoRepoRootExposure(t, cfg, main)
			})
		}
	}
}

// The non-own in-repo worktree branch binds a nested <ws>/sub/.scion
// read-only at /repo-root/<rel>/sub/.scion and masks its agents/. (Removing
// ONLY the worktree-branch applyNestedProjectConfig call turns this red.)
func TestBuildCommonRunArgs_WorktreeNotOwnNestedProjectConfigReadOnly(t *testing.T) {
	for _, rt := range []string{"docker", appleContainerRuntimeName} {
		for _, withAgents := range []bool{true, false} {
			name := rt
			if !withAgents {
				name += "/no agents dir"
			}
			t.Run(name, func(t *testing.T) {
				repoRoot, ws := setupHubManagedBaseRepo(t, "me")
				mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
				if withAgents {
					mkdirs(t, filepath.Join(ws, "sub", ".scion", "agents", "other"))
					// Also give the workspace its OWN .scion/agents so this layout
					// sets sharedWorkspaceAgentsMask (the create-path mask), exercising
					// that mask block's emit-order alongside the nested mask.
					mkdirs(t, filepath.Join(ws, ".scion", "agents", "other"))
				}
				cfg := minimalRunConfig()
				cfg.BrokerMode = true
				cfg.RuntimeName = rt
				cfg.HomeDir = filepath.Join(t.TempDir(), "home")
				mkdirs(t, cfg.HomeDir)
				cfg.RepoRoot, cfg.Workspace = repoRoot, ws
				cfg.AgentOwnWorkspace = false
				cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
				cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
				rel, err := filepath.Rel(repoRoot, ws)
				if err != nil {
					t.Fatal(err)
				}
				containerWorkspace := "/repo-root/" + filepath.ToSlash(rel)
				assertNestedProjectConfigAt(t, cfg, containerWorkspace, withAgents)
				if withAgents {
					// The own-.scion shared mask (sharedWorkspaceAgentsMask block) is
					// present and — via assertNestedProjectConfigAt's ordering check —
					// emitted after the binds.
					args, berr := buildCommonRunArgs(cfg)
					if berr != nil {
						t.Fatal(berr)
					}
					if rt == appleContainerRuntimeName {
						args = stripUnsupportedAppleFlags(args)
					}
					wantShared := containerWorkspace + "/.scion/agents"
					sharedFound := false
					for _, m := range tmpfsTargets(args) {
						if m == wantShared {
							sharedFound = true
						}
					}
					if !sharedFound {
						t.Errorf("own-.scion shared mask %q not present; tmpfs targets: %q", wantShared, tmpfsTargets(args))
					}
				}
			})
		}
	}
}

// A nested .scion that is a symlink is refused fail-closed on both the
// shared-plain and config-only branches.
func TestBuildCommonRunArgs_NestedProjectConfigSymlinkRefused(t *testing.T) {
	for _, sharedPlain := range []bool{true, false} {
		for _, rt := range []string{"docker", appleContainerRuntimeName} {
			name := rt
			if sharedPlain {
				name = "shared-plain/" + name
			} else {
				name = "config-only/" + name
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				ws := filepath.Join(t.TempDir(), "workspace")
				mkdirs(t, filepath.Join(ws, "sub", "cfg"))
				if err := os.Symlink("cfg", filepath.Join(ws, "sub", ".scion")); err != nil {
					t.Fatal(err)
				}
				cfg := minimalRunConfig()
				cfg.BrokerMode = true
				cfg.RuntimeName = rt
				cfg.HomeDir = filepath.Join(t.TempDir(), "home")
				mkdirs(t, cfg.HomeDir)
				cfg.Workspace = ws
				cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
				if sharedPlain {
					cfg.RepoRoot = ws
				}
				before := fsSnapshot(t, ws)
				args, err := buildCommonRunArgs(cfg)
				if !errors.Is(err, errProjectConfigSymlink) {
					t.Fatalf("error = %v, want errProjectConfigSymlink", err)
				}
				if args != nil {
					t.Errorf("fail-closed: expected nil args, got %q", args)
				}
				assertNothingCreated(t, before, ws)
			})
		}
	}
}

// A nested .scion marker FILE is bound read-only on docker/podman and
// refused on Apple (cannot bind a single file), on both branches.
func TestBuildCommonRunArgs_NestedProjectConfigMarker(t *testing.T) {
	for _, sharedPlain := range []bool{true, false} {
		for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
			name := rt
			if sharedPlain {
				name = "shared-plain/" + name
			} else {
				name = "config-only/" + name
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				ws := filepath.Join(t.TempDir(), "workspace")
				mkdirs(t, filepath.Join(ws, "sub"))
				writeFile(t, filepath.Join(ws, "sub", ".scion"), "project-id: x\n")
				cfg := minimalRunConfig()
				cfg.BrokerMode = true
				cfg.RuntimeName = rt
				cfg.HomeDir = filepath.Join(t.TempDir(), "home")
				mkdirs(t, cfg.HomeDir)
				cfg.Workspace = ws
				cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
				if sharedPlain {
					cfg.RepoRoot = ws
				}
				args, err := buildCommonRunArgs(cfg)
				if rt == appleContainerRuntimeName {
					if !errors.Is(err, errProjectConfigMarkerNoBind) {
						t.Fatalf("apple: error = %v, want errProjectConfigMarkerNoBind", err)
					}
					if args != nil {
						t.Errorf("apple: fail-closed expected nil args, got %q", args)
					}
					return
				}
				if err != nil {
					t.Fatalf("buildCommonRunArgs: %v", err)
				}
				wantSrc := layoutRealPath(t, filepath.Join(ws, "sub", ".scion"))
				found := false
				for _, b := range layoutBinds(t, args) {
					if b.target != "/workspace/sub/.scion" {
						continue
					}
					found = true
					if !b.readOnly {
						t.Errorf("nested marker bound read-write: %+v", b)
					}
					src := b.source
					if r, e := filepath.EvalSymlinks(src); e == nil {
						src = r
					}
					if src != wantSrc {
						t.Errorf("nested marker source = %s, want %s", src, wantSrc)
					}
				}
				if !found {
					t.Errorf("nested marker not mounted read-only; args: %q", args)
				}
			})
		}
	}
}

// ProjectConfigPath given as the project ROOT (not its .scion) still
// resolves and binds the nested <ws>/sub/.scion read-only.
func TestBuildCommonRunArgs_NestedProjectConfigPathAsRoot(t *testing.T) {
	for _, sharedPlain := range []bool{true, false} {
		name := "config-only"
		if sharedPlain {
			name = "shared-plain"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			ws := filepath.Join(t.TempDir(), "workspace")
			mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = "docker"
			cfg.HomeDir = filepath.Join(t.TempDir(), "home")
			mkdirs(t, cfg.HomeDir)
			cfg.Workspace = ws
			// Project root, not <...>/.scion.
			cfg.ProjectConfigPath = filepath.Join(ws, "sub")
			if sharedPlain {
				cfg.RepoRoot = ws
			}
			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			if !hasROTarget(layoutBinds(t, args), "/workspace/sub/.scion") {
				t.Errorf("nested .scion not mounted read-only when ProjectConfigPath is the project root; args: %q", args)
			}
		})
	}
}

// hasROTarget reports whether binds holds a read-only bind at target.
func hasROTarget(binds []layoutBind, target string) bool {
	for _, b := range binds {
		if b.target == target && b.readOnly {
			return true
		}
	}
	return false
}

// The shared-plain nested mask also holds for a real git-backed shared
// workspace (docker; Apple shared-plain is refused separately).
func TestBuildCommonRunArgs_SharedPlainNestedProjectConfigReadOnly_GitBacked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	layoutGit(t, ws, "init", "-q")
	layoutGit(t, ws, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	mkdirs(t, filepath.Join(ws, "sub", ".scion", "agents", "other"))
	cfg := minimalRunConfig()
	cfg.BrokerMode = true
	cfg.RuntimeName = "docker"
	cfg.HomeDir = filepath.Join(t.TempDir(), "home")
	mkdirs(t, cfg.HomeDir)
	cfg.RepoRoot, cfg.Workspace = ws, ws
	cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
	cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
	assertNestedProjectConfig(t, cfg, true)
}

// The registered outside-root fallback binds the bound workspace's OWN
// .scion when it is a marker FILE (read-only on docker/podman; refused on Apple,
// which cannot bind a single file) and refuses a symlinked own .scion on every
// runtime. The code lives at the fallback own-.scion block; dropping its err
// return makes the Apple-marker and symlink cases stop failing closed.
func TestBuildCommonRunArgs_FallbackOwnScionMarkerAndSymlink(t *testing.T) {
	// Marker file: ro bind on docker/podman, errProjectConfigMarkerNoBind on Apple.
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		t.Run("marker/"+rt, func(t *testing.T) {
			main, ws := registeredOutsideWorktree(t)
			writeFile(t, filepath.Join(ws, ".scion"), "project-id: x\n")
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = rt
			cfg.HomeDir = filepath.Join(t.TempDir(), "home")
			mkdirs(t, cfg.HomeDir)
			cfg.RepoRoot, cfg.Workspace = main, ws
			cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
			before := fsSnapshot(t, ws)
			args, err := buildCommonRunArgs(cfg)
			if rt == appleContainerRuntimeName {
				if !errors.Is(err, errProjectConfigMarkerNoBind) {
					t.Fatalf("apple: error = %v, want errProjectConfigMarkerNoBind", err)
				}
				if args != nil {
					t.Errorf("apple: fail-closed expected nil args, got %q", args)
				}
				assertNothingCreated(t, before, ws)
				return
			}
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			if !hasROTarget(layoutBinds(t, args), "/workspace/.scion") {
				t.Errorf("own .scion marker not bound read-only; args: %q", args)
			}
			assertNoRepoRootExposure(t, cfg, main)
		})
	}
	// Symlinked own .scion: refused fail-closed on every runtime.
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		t.Run("symlink/"+rt, func(t *testing.T) {
			main, ws := registeredOutsideWorktree(t)
			mkdirs(t, filepath.Join(ws, "cfg"))
			if err := os.Symlink("cfg", filepath.Join(ws, ".scion")); err != nil {
				t.Fatal(err)
			}
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = rt
			cfg.HomeDir = filepath.Join(t.TempDir(), "home")
			mkdirs(t, cfg.HomeDir)
			cfg.RepoRoot, cfg.Workspace = main, ws
			cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
			before := fsSnapshot(t, ws)
			args, err := buildCommonRunArgs(cfg)
			if !errors.Is(err, errProjectConfigSymlink) {
				t.Fatalf("%s: error = %v, want errProjectConfigSymlink", rt, err)
			}
			if args != nil {
				t.Errorf("%s: fail-closed expected nil args, got %q", rt, args)
			}
			assertNothingCreated(t, before, ws)
		})
	}
}
