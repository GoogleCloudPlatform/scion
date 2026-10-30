//go:build unix

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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
			// the broker reads; it must be mounted read-only. Its agents/ must
			// already exist so the read-only bind can be masked on Docker/Podman.
			mkdirs(t, filepath.Join(ws, ".scion", "templates"), filepath.Join(ws, ".scion", "agents"))

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
	if cfg.RuntimeName != appleContainerRuntimeName && !withAgents {
		// On Docker and Podman a nested .scion with no agents/ refuses the start
		// fail-closed (requireExistingAgentsMask never creates the mask point
		// under this read-only bind), and nothing is created on disk. Apple still
		// starts without a mask (handled by the success path below).
		before := fsSnapshot(t, cfg.Workspace)
		args, err := buildCommonRunArgs(cfg)
		if !errors.Is(err, errAgentsMaskPointMissing) {
			t.Fatalf("nested no-agents start: error = %v, want errAgentsMaskPointMissing", err)
		}
		if args != nil {
			t.Errorf("fail-closed: expected nil args, got %q", args)
		}
		assertNothingCreated(t, before, cfg.Workspace)
		return
	}
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
	// nested/own no-create mask flush (requireExistingAgentsMask).
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
				if withAgents || rt == appleContainerRuntimeName {
					// The no-repo-root-exposure check re-runs the build; skip it when
					// the no-agents nested start is refused on Docker/Podman (no args).
					assertNoRepoRootExposure(t, cfg, main)
				}
			})
		}
	}
}

// The outside-root fallback binds the bound workspace's OWN <ws>/.scion
// read-only and masks its agents/ ONLY when it already exists (never created).
// (Removing ONLY the fallback requireExistingAgentsMask call turns the
// with-agents case red.)
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

				if rt != appleContainerRuntimeName && !withAgents {
					// Docker/Podman: an own .scion with no agents/ refuses the start
					// fail-closed, creating nothing (requireExistingAgentsMask never
					// creates the mask point under the read-only bind).
					before := fsSnapshot(t, ws)
					args, err := buildCommonRunArgs(cfg)
					if !errors.Is(err, errAgentsMaskPointMissing) {
						t.Fatalf("own no-agents start: error = %v, want errAgentsMaskPointMissing", err)
					}
					if args != nil {
						t.Errorf("fail-closed: expected nil args, got %q", args)
					}
					assertNothingCreated(t, before, ws)
					return
				}

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
					// The fallback own-.scion mask (via requireExistingAgentsMask) is
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
			// agents/ must already exist so the Docker read-only nested bind is masked.
			mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"), filepath.Join(ws, "sub", ".scion", "agents"))
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
	// Own .scion is a directory but its agents/ entry is not a directory (a
	// symlink or a regular file): refused with errAgentsMaskNotDir on every
	// runtime, creating nothing. The mask point under the read-only bind cannot
	// be a link or a file.
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		for _, kind := range []string{"symlink", "file"} {
			t.Run("agents-"+kind+"/"+rt, func(t *testing.T) {
				main, ws := registeredOutsideWorktree(t)
				mkdirs(t, filepath.Join(ws, ".scion", "templates"))
				agentsPath := filepath.Join(ws, ".scion", "agents")
				if kind == "symlink" {
					mkdirs(t, filepath.Join(ws, ".scion", "realagents"))
					if err := os.Symlink("realagents", agentsPath); err != nil {
						t.Fatal(err)
					}
				} else {
					writeFile(t, agentsPath, "not a directory\n")
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
				if !errors.Is(err, errAgentsMaskNotDir) {
					t.Fatalf("agents-%s/%s: error = %v, want errAgentsMaskNotDir", kind, rt, err)
				}
				if args != nil {
					t.Errorf("fail-closed: expected nil args, got %q", args)
				}
				assertNothingCreated(t, before, ws)
			})
		}
	}
}

// --- agents-mask guarantee gates (a) and (b) ---
//
// These two gates pin the reconciled agents-mask behaviour across every call
// site, runtime and agents/src state. S1-S3 run upstream's ensureAgentsMaskPoint
// verbatim (no sentinel, no seam); S4, the nested cases (N) and an S3 agent's own
// workspace reached through N use the no-create requireExistingAgentsMask helper.
// No row calls t.Skip.

// maskGateSite builds a run config that reaches one agents-mask call site, with
// the project .scion created as a directory but no agents/ entry, and returns the
// config and the broker-side .scion directory whose agents/ a row populates.
//
//	S1    in-repo non-own worktree, project .scion at <ws>/.scion
//	S2    shared-plain (non-git, reached on every runtime), <ws>/.scion
//	S3    config-only (RepoRoot ""), <ws>/.scion
//	S4    outside-root registered worktree, not own, <ws>/.scion
//	N     a nested .scion in a subdirectory of a config-only workspace
//	S3own an S3 agent's OWN workspace with a nested .scion (reached via N)
func maskGateSite(t *testing.T, site, rt string) (RunConfig, string) {
	t.Helper()
	cfg := minimalRunConfig()
	cfg.BrokerMode = true
	cfg.RuntimeName = rt
	cfg.HomeDir = filepath.Join(t.TempDir(), "home")
	mkdirs(t, cfg.HomeDir)
	cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
	switch site {
	case "S1":
		repoRoot, ws := setupHubManagedBaseRepo(t, "me")
		mkdirs(t, filepath.Join(ws, ".scion", "templates"))
		cfg.RepoRoot, cfg.Workspace = repoRoot, ws
		cfg.AgentOwnWorkspace = false
		return cfg, filepath.Join(ws, ".scion")
	case "S2":
		ws := filepath.Join(t.TempDir(), "workspace")
		mkdirs(t, filepath.Join(ws, ".scion", "templates"))
		cfg.RepoRoot, cfg.Workspace = ws, ws
		return cfg, filepath.Join(ws, ".scion")
	case "S3":
		ws := filepath.Join(t.TempDir(), "workspace")
		mkdirs(t, filepath.Join(ws, ".scion", "templates"))
		cfg.Workspace = ws
		return cfg, filepath.Join(ws, ".scion")
	case "S4":
		main, ws := registeredOutsideWorktree(t)
		mkdirs(t, filepath.Join(ws, ".scion", "templates"))
		cfg.RepoRoot, cfg.Workspace = main, ws
		cfg.AgentOwnWorkspace = false
		return cfg, filepath.Join(ws, ".scion")
	case "N":
		ws := filepath.Join(t.TempDir(), "workspace")
		mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
		cfg.Workspace = ws
		cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
		return cfg, filepath.Join(ws, "sub", ".scion")
	case "S3own":
		ws := filepath.Join(t.TempDir(), "workspace")
		mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
		cfg.Workspace = ws
		cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
		cfg.AgentOwnWorkspace = true
		return cfg, filepath.Join(ws, "sub", ".scion")
	case "NfromS1":
		// A nested .scion reached through the in-repo non-own worktree layout
		// (no own <ws>/.scion, so the nested one is the only mask).
		repoRoot, ws := setupHubManagedBaseRepo(t, "me")
		mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
		cfg.RepoRoot, cfg.Workspace = repoRoot, ws
		cfg.AgentOwnWorkspace = false
		cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
		return cfg, filepath.Join(ws, "sub", ".scion")
	case "NfromS2":
		// A nested .scion reached through the shared-plain layout (non-git so it
		// is reached on every runtime).
		ws := filepath.Join(t.TempDir(), "workspace")
		mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
		cfg.RepoRoot, cfg.Workspace = ws, ws
		cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
		return cfg, filepath.Join(ws, "sub", ".scion")
	case "NfromS4":
		// A nested .scion reached through the outside-root fallback layout.
		main, ws := registeredOutsideWorktree(t)
		mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
		cfg.RepoRoot, cfg.Workspace = main, ws
		cfg.AgentOwnWorkspace = false
		cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
		return cfg, filepath.Join(ws, "sub", ".scion")
	}
	t.Fatalf("unknown mask gate site %q", site)
	return cfg, ""
}

// maskGateResolvedSrc is the broker-side .scion path requireExistingAgentsMask
// receives (workspaceProjectConfigMount resolves the parent with EvalSymlinks).
func maskGateResolvedSrc(t *testing.T, scionSrc string) string {
	t.Helper()
	return filepath.Join(layoutRealPath(t, filepath.Dir(scionSrc)), filepath.Base(scionSrc))
}

// injectAgentsMaskLstat swaps the agentsMaskLstat seam so a lookup of target
// returns injErr, and restores it with t.Cleanup. Tests that use it must not run
// in parallel.
func injectAgentsMaskLstat(t *testing.T, target string, injErr error) {
	t.Helper()
	real := agentsMaskLstat
	agentsMaskLstat = func(p string) (os.FileInfo, error) {
		if p == target {
			return nil, injErr
		}
		return real(p)
	}
	t.Cleanup(func() { agentsMaskLstat = real })
}

// arrangeMaskState sets up the <src>/agents entry (or an injected Lstat fault) for
// a gate row at a nocreate site. scionSrc is the on-disk .scion directory from
// maskGateSite.
func arrangeMaskState(t *testing.T, scionSrc, state string) {
	t.Helper()
	agentsPath := filepath.Join(scionSrc, "agents")
	switch state {
	case "present":
		mkdirs(t, agentsPath)
	case "absent":
		// nothing
	case "symlink-agents":
		mkdirs(t, filepath.Join(scionSrc, "realagents"))
		if err := os.Symlink("realagents", agentsPath); err != nil {
			t.Fatal(err)
		}
	case "dangling-agents":
		if err := os.Symlink("nonexistent-target", agentsPath); err != nil {
			t.Fatal(err)
		}
	case "file-agents":
		writeFile(t, agentsPath, "not a directory\n")
	case "agents-lstat-error":
		injectAgentsMaskLstat(t, filepath.Join(maskGateResolvedSrc(t, scionSrc), "agents"),
			&fs.PathError{Op: "lstat", Path: agentsPath, Err: syscall.EIO})
	case "src-symlink":
		// Replace the .scion directory with a symlink; it is refused by
		// workspaceProjectConfigMount before the helper runs.
		if err := os.RemoveAll(scionSrc); err != nil {
			t.Fatal(err)
		}
		mkdirs(t, filepath.Join(filepath.Dir(scionSrc), "realscion", "templates"))
		if err := os.Symlink("realscion", scionSrc); err != nil {
			t.Fatal(err)
		}
	case "src-lstat-error":
		injectAgentsMaskLstat(t, maskGateResolvedSrc(t, scionSrc),
			&fs.PathError{Op: "lstat", Path: scionSrc, Err: syscall.EIO})
	case "src-not-exist":
		injectAgentsMaskLstat(t, maskGateResolvedSrc(t, scionSrc),
			&fs.PathError{Op: "lstat", Path: scionSrc, Err: fs.ErrNotExist})
	default:
		t.Fatalf("unknown mask state %q", state)
	}
}

type maskExpect struct {
	refuse   bool
	wantMask bool   // when !refuse
	is       error  // errors.Is target (when refuse)
	contains string // substring of err.Error() (when refuse && is == nil)
}

// maskGateExpect is the pinned outcome for (siteClass, runtime, state).
func maskGateExpect(nocreate bool, rt, state string) maskExpect {
	apple := rt == appleContainerRuntimeName
	if !nocreate { // S1-S3: upstream ensureAgentsMaskPoint, verbatim.
		switch state {
		case "present":
			return maskExpect{wantMask: true}
		case "absent":
			if apple {
				return maskExpect{}
			}
			return maskExpect{wantMask: true} // dir created on host, then masked
		case "symlink-agents":
			if apple {
				return maskExpect{wantMask: true} // isDir follows the link
			}
			return maskExpect{refuse: true, contains: "the project's agents directory"}
		case "dangling-agents", "file-agents":
			if apple {
				return maskExpect{}
			}
			return maskExpect{refuse: true, contains: "the project's agents directory"}
		}
	} else { // S4, N, S3own: no-create requireExistingAgentsMask.
		switch state {
		case "present":
			return maskExpect{wantMask: true}
		case "absent":
			if apple {
				return maskExpect{}
			}
			return maskExpect{refuse: true, is: errAgentsMaskPointMissing}
		case "symlink-agents", "dangling-agents", "file-agents":
			return maskExpect{refuse: true, is: errAgentsMaskNotDir}
		case "agents-lstat-error", "src-lstat-error":
			return maskExpect{refuse: true, is: syscall.EIO}
		case "src-not-exist":
			return maskExpect{refuse: true, is: fs.ErrNotExist}
		case "src-symlink":
			return maskExpect{refuse: true, is: errProjectConfigSymlink}
		}
	}
	panic("unreachable mask expect")
}

// hasScionAgentsMask reports whether any tmpfs mask target is a project
// .scion/agents path.
func hasScionAgentsMask(args []string) bool {
	for _, m := range tmpfsTargets(args) {
		if strings.HasSuffix(m, "/.scion/agents") {
			return true
		}
	}
	return false
}

// assertEveryROScionDirIsMasked is the (a) universal rule: on a successful
// Docker/Podman start, each read-only bind whose source is a real .scion
// directory has a later tmpfs at <target>/agents.
func assertEveryROScionDirIsMasked(t *testing.T, args []string) {
	t.Helper()
	masks := map[string]bool{}
	for _, m := range tmpfsTargets(args) {
		masks[m] = true
	}
	for _, b := range layoutBinds(t, args) {
		if !b.readOnly || !strings.HasSuffix(b.target, "/.scion") {
			continue
		}
		src := b.source
		if r, err := filepath.EvalSymlinks(src); err == nil {
			src = r
		}
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			continue
		}
		if !masks[b.target+"/agents"] {
			t.Errorf("read-only .scion dir bind %s has no tmpfs mask at %s/agents; masks: %q", b.target, b.target, tmpfsTargets(args))
		}
	}
}

// TestBuildCommonRunArgs_EveryReadOnlyScionDirBindIsMasked is guarantee (a): no
// Docker/Podman start succeeds with an unmasked read-only .scion directory bind,
// so an agents dir created after the start is always under a tmpfs; fail-closed
// states are refused with the pinned error. (The injected-Lstat rows have no skip
// path and apply at the no-create sites.)
func TestBuildCommonRunArgs_EveryReadOnlyScionDirBindIsMasked(t *testing.T) {
	upstreamStates := []string{"present", "absent", "symlink-agents", "dangling-agents", "file-agents"}
	nocreateStates := []string{"present", "absent", "symlink-agents", "dangling-agents", "file-agents", "agents-lstat-error", "src-symlink", "src-lstat-error", "src-not-exist"}
	runtimes := []string{"docker", "podman", appleContainerRuntimeName}
	sites := []struct {
		name     string
		nocreate bool
	}{
		{"S1", false}, {"S2", false}, {"S3", false},
		{"S4", true}, {"N", true}, {"S3own", true},
	}
	for _, site := range sites {
		states := upstreamStates
		if site.nocreate {
			states = nocreateStates
		}
		for _, rt := range runtimes {
			for _, state := range states {
				t.Run(site.name+"/"+rt+"/"+state, func(t *testing.T) {
					cfg, scionSrc := maskGateSite(t, site.name, rt)
					arrangeMaskState(t, scionSrc, state)
					want := maskGateExpect(site.nocreate, rt, state)
					args, err := buildCommonRunArgs(cfg)
					if want.refuse {
						if err == nil {
							t.Fatalf("expected a refusal, got args %q", args)
						}
						if args != nil {
							t.Errorf("fail-closed: expected nil args, got %q", args)
						}
						switch {
						case want.is != nil:
							if !errors.Is(err, want.is) {
								t.Fatalf("error = %v, want errors.Is %v", err, want.is)
							}
						case want.contains != "":
							if !strings.Contains(err.Error(), want.contains) {
								t.Fatalf("error = %q, want it to contain %q", err.Error(), want.contains)
							}
						}
						return
					}
					if err != nil {
						t.Fatalf("buildCommonRunArgs: %v", err)
					}
					if rt == appleContainerRuntimeName {
						args = stripUnsupportedAppleFlags(args)
					}
					if got := hasScionAgentsMask(args); got != want.wantMask {
						t.Errorf("agents mask present = %v, want %v; masks: %q", got, want.wantMask, tmpfsTargets(args))
					}
					if rt != appleContainerRuntimeName {
						assertEveryROScionDirIsMasked(t, args)
					}
				})
			}
		}
	}
	// N reached from the S1, S2 and S4 parents (not only the config-only parent):
	// the no-create helper refuses a non-directory agents entry on every runtime
	// and creates nothing. Present/absent for these parents are covered by the
	// dedicated nested tests.
	for _, site := range []string{"NfromS1", "NfromS2", "NfromS4"} {
		for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
			for _, state := range []string{"symlink-agents", "file-agents", "agents-lstat-error"} {
				t.Run(site+"/"+rt+"/"+state, func(t *testing.T) {
					cfg, scionSrc := maskGateSite(t, site, rt)
					arrangeMaskState(t, scionSrc, state)
					want := maskGateExpect(true, rt, state)
					before := fsSnapshot(t, cfg.Workspace)
					args, err := buildCommonRunArgs(cfg)
					if !errors.Is(err, want.is) {
						t.Fatalf("error = %v, want errors.Is %v", err, want.is)
					}
					if args != nil {
						t.Errorf("fail-closed: expected nil args, got %q", args)
					}
					assertNothingCreated(t, before, cfg.Workspace)
				})
			}
		}
	}
	// A git-backed shared-plain workspace on Apple is refused before any mask
	// logic (Apple cannot bind a per-agent read-only .git/config).
	t.Run("S2-git-backed/apple", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		ws := filepath.Join(t.TempDir(), "workspace")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		layoutGit(t, ws, "init", "-q")
		layoutGit(t, ws, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
		mkdirs(t, filepath.Join(ws, ".scion", "agents"))
		cfg := minimalRunConfig()
		cfg.BrokerMode = true
		cfg.RuntimeName = appleContainerRuntimeName
		cfg.HomeDir = filepath.Join(t.TempDir(), "home")
		mkdirs(t, cfg.HomeDir)
		cfg.RepoRoot, cfg.Workspace = ws, ws
		args, err := buildCommonRunArgs(cfg)
		if !errors.Is(err, errAppleSharedWorkspaceUnsupported) {
			t.Fatalf("error = %v, want errAppleSharedWorkspaceUnsupported", err)
		}
		if args != nil {
			t.Errorf("fail-closed: expected nil args, got %q", args)
		}
	})
}

// TestBuildCommonRunArgs_NestedAndFallbackNeverCreateUnderScion is guarantee (b):
// at the no-create sites (S4, N, S3own), the runtime creates nothing under the
// read-only .scion bind source, whether the start succeeds or is refused, and
// every emitted mask follows its bind. The snapshot is a full equality of entry
// type, inode, size and symlink target under the workspace (G5).
func TestBuildCommonRunArgs_NestedAndFallbackNeverCreateUnderScion(t *testing.T) {
	nocreateStates := []string{"present", "absent", "symlink-agents", "dangling-agents", "file-agents", "agents-lstat-error", "src-symlink", "src-lstat-error", "src-not-exist"}
	runtimes := []string{"docker", "podman", appleContainerRuntimeName}
	for _, site := range []string{"S4", "N", "S3own"} {
		for _, rt := range runtimes {
			for _, state := range nocreateStates {
				t.Run(site+"/"+rt+"/"+state, func(t *testing.T) {
					cfg, scionSrc := maskGateSite(t, site, rt)
					arrangeMaskState(t, scionSrc, state)
					before := strongSnapshot(t, cfg.Workspace)
					args, err := buildCommonRunArgs(cfg)
					assertStrongUnchanged(t, before, cfg.Workspace)
					if err == nil && args != nil {
						if rt == appleContainerRuntimeName {
							args = stripUnsupportedAppleFlags(args)
						}
						if hasScionAgentsMask(args) {
							assertMasksAfterBinds(t, args)
						}
					}
				})
			}
		}
	}
	// Combined row: an S4 own .scion without agents/ AND a nested N, both without
	// agents/. The snapshot covers both bind sources, so a reintroduced emit-block
	// MkdirAll (or any line keyed on config.Workspace) is caught at the no-create
	// sites. On Docker/Podman the start is refused; nothing is created either way.
	for _, rt := range runtimes {
		t.Run("combined-S4-and-N/"+rt, func(t *testing.T) {
			main, ws := registeredOutsideWorktree(t)
			mkdirs(t, filepath.Join(ws, ".scion", "templates"))
			mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
			cfg := minimalRunConfig()
			cfg.BrokerMode = true
			cfg.RuntimeName = rt
			cfg.HomeDir = filepath.Join(t.TempDir(), "home")
			mkdirs(t, cfg.HomeDir)
			cfg.RepoRoot, cfg.Workspace = main, ws
			cfg.AgentOwnWorkspace = false
			cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
			cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
			before := strongSnapshot(t, ws)
			args, err := buildCommonRunArgs(cfg)
			assertStrongUnchanged(t, before, ws)
			if rt != appleContainerRuntimeName {
				if !errors.Is(err, errAgentsMaskPointMissing) {
					t.Fatalf("combined: error = %v, want errAgentsMaskPointMissing", err)
				}
				if args != nil {
					t.Errorf("fail-closed: expected nil args, got %q", args)
				}
			}
		})
	}
}

// fsEntry is a strong snapshot of one path for the (b) gate: enough to detect any
// creation, removal, retype, resize or relink under a bind source.
type fsEntry struct {
	typ  os.FileMode // Mode().Type()
	ino  uint64
	size int64
	link string // symlink target, else ""
}

// strongSnapshot records every path under roots by (type, inode, size, link).
// Walk errors are fatal; symlinks are recorded, not followed, so a create through
// a link inside a snapshotted root is seen.
func strongSnapshot(t *testing.T, roots ...string) map[string]fsEntry {
	t.Helper()
	m := map[string]fsEntry{}
	for _, root := range roots {
		if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				t.Fatalf("strong snapshot walk %s: %v", p, err)
			}
			e := fsEntry{typ: info.Mode().Type(), size: info.Size()}
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				e.ino = st.Ino
			}
			if info.Mode()&os.ModeSymlink != 0 {
				tgt, lerr := os.Readlink(p)
				if lerr != nil {
					t.Fatalf("strong snapshot readlink %s: %v", p, lerr)
				}
				e.link = tgt
			}
			m[p] = e
			return nil
		}); err != nil {
			t.Fatalf("strong snapshot walk %s: %v", root, err)
		}
	}
	return m
}

// assertStrongUnchanged fails if the tree under roots differs at all from before.
func assertStrongUnchanged(t *testing.T, before map[string]fsEntry, roots ...string) {
	t.Helper()
	after := strongSnapshot(t, roots...)
	for p, b := range before {
		a, ok := after[p]
		if !ok {
			t.Errorf("the start removed %s on disk", p)
			continue
		}
		if a != b {
			t.Errorf("the start changed %s on disk: %+v -> %+v", p, b, a)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("the start created %s on disk", p)
		}
	}
}

// TestBuildCommonRunArgs_NestedProjectConfigAgentsNotDir adds the nested
// symlinked-agents and file-agents rows: a nested .scion directory whose agents/
// entry is not a directory is refused with errAgentsMaskNotDir on every runtime,
// creating nothing.
func TestBuildCommonRunArgs_NestedProjectConfigAgentsNotDir(t *testing.T) {
	for _, rt := range []string{"docker", "podman", appleContainerRuntimeName} {
		for _, kind := range []string{"symlink", "file"} {
			t.Run(kind+"/"+rt, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				ws := filepath.Join(t.TempDir(), "workspace")
				mkdirs(t, filepath.Join(ws, "sub", ".scion", "templates"))
				agentsPath := filepath.Join(ws, "sub", ".scion", "agents")
				if kind == "symlink" {
					mkdirs(t, filepath.Join(ws, "sub", ".scion", "realagents"))
					if err := os.Symlink("realagents", agentsPath); err != nil {
						t.Fatal(err)
					}
				} else {
					writeFile(t, agentsPath, "not a directory\n")
				}
				cfg := minimalRunConfig()
				cfg.BrokerMode = true
				cfg.RuntimeName = rt
				cfg.HomeDir = filepath.Join(t.TempDir(), "home")
				mkdirs(t, cfg.HomeDir)
				cfg.Workspace = ws
				cfg.ProjectConfigPath = filepath.Join(ws, "sub", ".scion")
				before := fsSnapshot(t, ws)
				args, err := buildCommonRunArgs(cfg)
				if !errors.Is(err, errAgentsMaskNotDir) {
					t.Fatalf("nested agents-%s/%s: error = %v, want errAgentsMaskNotDir", kind, rt, err)
				}
				if args != nil {
					t.Errorf("fail-closed: expected nil args, got %q", args)
				}
				assertNothingCreated(t, before, ws)
			})
		}
	}
}

// TestBuildCommonRunArgs_AgentsMaskRefusalMessages pins (A3) the full refusal
// messages, with the absolute path and the mkdir remedy, at S4 and N on docker.
func TestBuildCommonRunArgs_AgentsMaskRefusalMessages(t *testing.T) {
	for _, site := range []string{"S4", "N"} {
		t.Run(site+"/missing", func(t *testing.T) {
			cfg, scionSrc := maskGateSite(t, site, "docker")
			arrangeMaskState(t, scionSrc, "absent")
			src := maskGateResolvedSrc(t, scionSrc)
			agentsPath := filepath.Join(src, "agents")
			want := fmt.Sprintf("agent %q: %s: %s; run: mkdir %q and start the agent again",
				cfg.Name, src, errAgentsMaskPointMissing.Error(), agentsPath)
			_, err := buildCommonRunArgs(cfg)
			if err == nil || err.Error() != want {
				t.Fatalf("missing message:\n got %q\nwant %q", errText(err), want)
			}
		})
		for _, kind := range []struct{ state, typ string }{{"file-agents", "regular file"}, {"symlink-agents", "symlink"}} {
			t.Run(site+"/notdir-"+kind.typ, func(t *testing.T) {
				cfg, scionSrc := maskGateSite(t, site, "docker")
				arrangeMaskState(t, scionSrc, kind.state)
				src := maskGateResolvedSrc(t, scionSrc)
				agentsPath := filepath.Join(src, "agents")
				want := fmt.Sprintf("agent %q: %s (%s): %s; replace it with a directory",
					cfg.Name, agentsPath, kind.typ, errAgentsMaskNotDir.Error())
				_, err := buildCommonRunArgs(cfg)
				if err == nil || err.Error() != want {
					t.Fatalf("not-a-directory message:\n got %q\nwant %q", errText(err), want)
				}
			})
		}
	}
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// fakeFileInfo is a minimal os.FileInfo for injecting an entry type through the
// agentsMaskLstat seam, so requireExistingAgentsMask's own src/agents type
// branches are exercised directly (these are fail-closed branches a regression
// could silently open; workspaceProjectConfigMount shadows the symlink case on
// the real path).
type fakeFileInfo struct {
	name string
	mode os.FileMode
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// injectAgentsMaskLstatInfo makes the agentsMaskLstat seam return info (no error)
// for target, restoring it with t.Cleanup. Tests using it must not run parallel.
func injectAgentsMaskLstatInfo(t *testing.T, target string, info os.FileInfo) {
	t.Helper()
	real := agentsMaskLstat
	agentsMaskLstat = func(p string) (os.FileInfo, error) {
		if p == target {
			return info, nil
		}
		return real(p)
	}
	t.Cleanup(func() { agentsMaskLstat = real })
}

// TestBuildCommonRunArgs_AgentsMaskHelperTypeRefusals drives requireExistingAgentsMask's
// own src/agents type classification through the agentsMaskLstat seam, so the
// fail-closed branches are covered directly and nothing is created.
func TestBuildCommonRunArgs_AgentsMaskHelperTypeRefusals(t *testing.T) {
	t.Run("src-symlink", func(t *testing.T) {
		cfg, scionSrc := maskGateSite(t, "S4", "docker")
		src := maskGateResolvedSrc(t, scionSrc)
		injectAgentsMaskLstatInfo(t, src, fakeFileInfo{name: ".scion", mode: os.ModeSymlink})
		before := fsSnapshot(t, cfg.Workspace)
		args, err := buildCommonRunArgs(cfg)
		if !errors.Is(err, errProjectConfigSymlink) {
			t.Fatalf("error = %v, want errProjectConfigSymlink", err)
		}
		if args != nil {
			t.Errorf("fail-closed: expected nil args, got %q", args)
		}
		assertNothingCreated(t, before, cfg.Workspace)
	})
	t.Run("src-special-file", func(t *testing.T) {
		cfg, scionSrc := maskGateSite(t, "S4", "docker")
		src := maskGateResolvedSrc(t, scionSrc)
		injectAgentsMaskLstatInfo(t, src, fakeFileInfo{name: ".scion", mode: os.ModeNamedPipe})
		before := fsSnapshot(t, cfg.Workspace)
		args, err := buildCommonRunArgs(cfg)
		if !errors.Is(err, errAgentsMaskNotDir) {
			t.Fatalf("error = %v, want errAgentsMaskNotDir", err)
		}
		if !strings.Contains(err.Error(), "special file (") {
			t.Errorf("error %q lacks the special-file type", err.Error())
		}
		if args != nil {
			t.Errorf("fail-closed: expected nil args, got %q", args)
		}
		assertNothingCreated(t, before, cfg.Workspace)
	})
	t.Run("agents-special-file", func(t *testing.T) {
		cfg, scionSrc := maskGateSite(t, "S4", "docker")
		agentsPath := filepath.Join(maskGateResolvedSrc(t, scionSrc), "agents")
		injectAgentsMaskLstatInfo(t, agentsPath, fakeFileInfo{name: "agents", mode: os.ModeNamedPipe})
		before := fsSnapshot(t, cfg.Workspace)
		args, err := buildCommonRunArgs(cfg)
		if !errors.Is(err, errAgentsMaskNotDir) {
			t.Fatalf("error = %v, want errAgentsMaskNotDir", err)
		}
		if !strings.Contains(err.Error(), "special file (") {
			t.Errorf("error %q lacks the special-file type", err.Error())
		}
		if args != nil {
			t.Errorf("fail-closed: expected nil args, got %q", args)
		}
		assertNothingCreated(t, before, cfg.Workspace)
	})
}
