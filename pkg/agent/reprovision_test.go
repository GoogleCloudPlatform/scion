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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// snapshotTree walks dir and returns a map of relative path -> file content,
// for a byte-identical before/after comparison. Missing dir snapshots as an
// empty map (useful for a ".git" subdirectory that must not be touched) --
// but any OTHER walk/read error (a permission error, a file that vanishes
// mid-walk, etc.) fails the test outright rather than silently returning a
// partial or empty snapshot that could make a before/after comparison pass
// for the wrong reason (upstream review, GoogleCloudPlatform/scion#2037,
// comment 4121261329).
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("snapshotTree %s: %v", dir, err)
	}
	return out
}

// reprovisionSetup builds a git project with a gitignored .scion/agents and a
// global generic harness-config + default template. CWD is set OUTSIDE the
// repo, as on a runtime broker — this is what makes util.BranchExists return
// false for a broker process, which is the data-loss condition Reprovision's
// workspace-mode gate (design §3.4 Amendment A2) exists to catch.
func reprovisionSetup(t *testing.T) (scionDir, globalScionDir string) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "project")
	_ = os.MkdirAll(projectDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte(".scion/agents/\n"), 0644)
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", ".gitignore"},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	scionDir = filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(filepath.Join(scionDir, "templates"), 0755)

	globalScionDir = filepath.Join(tmpDir, ".scion")
	_ = os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "generic", "generic")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"generic"}`), 0644)
	return scionDir, globalScionDir
}

// TestReprovision_WorktreeMode_Refused is a data-loss regression test
// (design Amendment A2): Reprovision must refuse — not silently delete — a
// worktree-mode agent's workspace, so uncommitted work always survives.
// Before the fix, ProvisionAgent's CWD-dependent BranchExists check returned
// false on a broker (CWD outside the repo), so it ran os.RemoveAll on the
// live worktree.
func TestReprovision_WorktreeMode_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "wt-agent"

	if _, _, _, err := ProvisionAgent(context.Background(), agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		t.Fatalf("expected worktree after initial provision: %v", err)
	}
	work := filepath.Join(ws, "uncommitted-work.txt")
	if err := os.WriteFile(work, []byte("hours of agent work"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse a non-clone-per-agent (worktree) agent, got nil error")
	}
	if _, statErr := os.Stat(work); statErr != nil {
		t.Fatalf("DATA LOSS: uncommitted file in worktree workspace is gone after the refused Reprovision: %v", statErr)
	}
}

// TestReprovision_GitCloneSetButNoRealClone_Refused is the design §3.4
// Amendment A2.1 regression test for the SECOND half of the workspace-mode
// gate: GitClone IS set (opts.GitClone != nil passes the first check), but
// the workspace on disk is a worktree — its .git is a FILE, not a directory,
// exactly what a worktree-per-agent agent looks like on disk. Without the
// .git-is-a-directory check, ProvisionAgent's git-clone branch would treat
// this as "not a real clone" and os.RemoveAll it before re-cloning.
func TestReprovision_GitCloneSetButNoRealClone_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "wt-agent2"
	if _, _, _, err := ProvisionAgent(context.Background(), agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	fi, err := os.Stat(filepath.Join(ws, ".git"))
	if err != nil || fi.IsDir() {
		t.Fatalf("expected a worktree .git FILE (not a directory) after initial provision: %v (isDir=%v)", err, fi != nil && fi.IsDir())
	}
	work := filepath.Join(ws, "uncommitted-work.txt")
	if err := os.WriteFile(work, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	_, err = mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		GitClone: &api.GitCloneConfig{URL: "https://example.com/repo.git"},
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse a GitClone-set agent whose workspace is not a real clone, got nil error")
	}
	t.Logf("refusal: %v", err)
	if _, statErr := os.Stat(work); statErr != nil {
		t.Fatalf("DATA LOSS: uncommitted file is gone after the refused Reprovision: %v", statErr)
	}
}

// TestReprovision_CloneMode_WorkspaceAndHomeSurvive is the positive control:
// a clone-per-agent workspace with a real .git dir, and its home directory,
// both survive Reprovision.
func TestReprovision_CloneMode_WorkspaceAndHomeSurvive(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "clone-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0755)
	work := filepath.Join(ws, "uncommitted-work.txt")
	_ = os.WriteFile(work, []byte("x"), 0644)
	homeFile := filepath.Join(config.GetAgentHomePath(scionDir, agentName), "notes.txt")
	_ = os.WriteFile(homeFile, []byte("home note"), 0644)

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
	}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("clone workspace file lost: %v", err)
	}
	if _, err := os.Stat(homeFile); err != nil {
		t.Fatalf("home file lost: %v", err)
	}
}

// TestReprovision_HarnessConfigImageBumpReplacesFrozenImage pins the whole
// reason reincarnate exists: a plain restart (Provision, via GetAgent) keeps
// scion-agent.json frozen at whatever image generation N resolved, but
// Reprovision replaces it with the current harness-config image.
func TestReprovision_HarnessConfigImageBumpReplacesFrozenImage(t *testing.T) {
	scionDir, globalScionDir := reprovisionSetup(t)
	agentName := "img-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0755)

	cfgPath := filepath.Join(scionDir, "agents", agentName, "scion-agent.json")
	read := func() string {
		var c api.ScionConfig
		b, _ := os.ReadFile(cfgPath)
		_ = json.Unmarshal(b, &c)
		return c.Image
	}
	if got := read(); got != "test-image:latest" {
		t.Fatalf("gen N image = %q", got)
	}
	_ = os.WriteFile(filepath.Join(globalScionDir, "harness-configs", "generic", "config.yaml"),
		[]byte("harness: generic\nimage: test-image:v2\n"), 0644)

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Provision(context.Background(), api.StartOptions{Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := read(); got != "test-image:latest" {
		t.Fatalf("plain Provision (restart) must keep the frozen image; got %q", got)
	}
	// The restart above went through GetAgent, which clears and recreates an
	// empty workspace dir for git-clone mode so sciontool performs a fresh
	// clone (see GetAgent's "clearing existing workspace for git-clone
	// re-provision" comment) — that's real, unrelated production behavior,
	// but it also wipes this test's synthetic ".git exists" marker. Restore
	// it: in production the actual clone lives inside the container, not
	// managed by this directory at all, and Reprovision's own precondition
	// only needs a real .git directory to be present.
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0755)

	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}
	if got := read(); got != "test-image:v2" {
		t.Fatalf("after Reprovision image = %q, want test-image:v2", got)
	}
}

// TestReprovision_PreStartHookReStagedOrRemoved covers the design §3.4
// "re-stage the pre-start hook" bullet: a hook present at generation N is
// updated to generation N+1's script, and a hook removed between generations
// (empty script) is actually removed, not left stale.
func TestReprovision_PreStartHookReStagedOrRemoved(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "hook-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0755)

	hookPath := filepath.Join(config.GetAgentHomePath(scionDir, agentName), ".scion", "hooks", "pre-start.d", "30-project-custom")

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
		ProjectPreStartHookScript: "#!/bin/sh\necho gen2\n",
	}); err != nil {
		t.Fatalf("Reprovision (stage hook): %v", err)
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("expected pre-start hook to be staged: %v", err)
	}
	if string(data) != "#!/bin/sh\necho gen2\n" {
		t.Fatalf("hook content = %q", data)
	}

	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
		ProjectPreStartHookScript: "",
	}); err != nil {
		t.Fatalf("Reprovision (remove hook): %v", err)
	}
	if _, err := os.Stat(hookPath); err == nil {
		t.Fatal("expected the deactivated pre-start hook to be removed, but it still exists")
	}
}

// TestReprovision_PromptMDUntouched covers the design §3.4 note that the new
// generation's task is delivered by DispatchAgentStart, not pre-staged as a
// file: Reprovision must never write or clear prompt.md.
func TestReprovision_PromptMDUntouched(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "prompt-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0755)

	promptPath := filepath.Join(scionDir, "agents", agentName, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("gen 1 task"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
		Task: "this must not be written to prompt.md",
	}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("prompt.md must survive: %v", err)
	}
	if string(data) != "gen 1 task" {
		t.Fatalf("prompt.md was modified by Reprovision: got %q", data)
	}
}

// TestReprovision_RunningContainer_Refused is a design §3.4 Amendment A2
// regression test: Reprovision must refuse when the runtime reports the agent's container as
// still running, regardless of what the caller believes the state to be.
func TestReprovision_RunningContainer_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "running-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0755)

	mockRT := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{Name: agentName, Phase: "running"}}, nil
		},
	}
	mgr := NewManager(mockRT)
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse a running container, got nil error")
	}
}

// TestReprovision_StoppedContainer_Proceeds is the companion to the above: a
// container the runtime reports as stopped (or absent) must NOT be refused —
// the running-container precondition is specifically about a still-running
// container, not about requiring a prior successful stop confirmation.
func TestReprovision_StoppedContainer_Proceeds(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "stopped-agent"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0755)

	mockRT := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{Name: agentName, Phase: "stopped"}}, nil
		},
	}
	mgr := NewManager(mockRT)
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
	}); err != nil {
		t.Fatalf("Reprovision must proceed for a stopped container: %v", err)
	}
}

// =============================================================================
// Design §3.4 Amendment A23: explicit-mount (shared-workspace / hub-managed)
// =============================================================================

// TestReprovision_ExplicitMount_CheckoutByteIdenticalAndSiblingUntouched is
// the design §3.4 Amendment A23 positive-control test for the explicit-mount
// case: two agents share one external workspace mount (as shared-workspace
// projects do). Reprovision on one of them must leave the shared checkout
// byte-identical (including an untracked file) and must never write into
// its .git directory, must re-render the reprovisioned agent's own
// scion-agent.json and home, and must leave the sibling agent's directory
// (config and home) completely untouched.
func TestReprovision_ExplicitMount_CheckoutByteIdenticalAndSiblingUntouched(t *testing.T) {
	scionDir, globalScionDir := reprovisionSetup(t)
	agentName := "shared-agent"
	siblingName := "shared-agent-sibling"

	// R4 (review p1b-r1): write a project-id so GetAgentDir/GetAgentHomePath
	// resolve to the EXTERNAL agents root (design §3.4 Amendment A23: "In
	// shared mode the per-agent dir lives under the external agents root").
	// Without this, config.GetAgentDir silently falls back to the in-project
	// path, and the isolation this test claims to check would never actually
	// be exercised.
	if err := config.WriteProjectID(scionDir, "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("WriteProjectID: %v", err)
	}

	// The shared checkout: a real git repo mounted directly for every agent
	// in the project — not a per-agent worktree or clone.
	sharedCheckout := t.TempDir()
	runGit(t, sharedCheckout, "init")
	runGit(t, sharedCheckout, "config", "user.email", "test@example.com")
	runGit(t, sharedCheckout, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(sharedCheckout, "tracked.txt"), []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sharedCheckout, "add", "tracked.txt")
	runGit(t, sharedCheckout, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(sharedCheckout, "untracked.txt"), []byte("scratch work"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := api.ContextWithSharedWorkspace(context.Background())
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", sharedCheckout); err != nil {
		t.Fatalf("initial ProvisionAgent (agent): %v", err)
	}
	if _, _, _, err := ProvisionAgent(ctx, siblingName, "default", "", "", scionDir, "", "created", "", sharedCheckout); err != nil {
		t.Fatalf("initial ProvisionAgent (sibling): %v", err)
	}

	agentDir := config.GetAgentDir(scionDir, agentName, true)
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	siblingDir := config.GetAgentDir(scionDir, siblingName, true)
	siblingHome := config.GetAgentHomePath(scionDir, siblingName)
	if agentDir == filepath.Join(scionDir, "agents", agentName) {
		t.Fatal("fixture check: WriteProjectID must route agentDir to the external root, not the in-project fallback")
	}

	if err := os.WriteFile(filepath.Join(agentHome, "notes.txt"), []byte("agent note"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siblingHome, "notes.txt"), []byte("sibling note"), 0644); err != nil {
		t.Fatal(err)
	}

	// A real content change between provision and reprovision (R4): bump the
	// harness-config image, as TestReprovision_HarnessConfigImageBumpReplacesFrozenImage
	// does, and delete home/agent-info.json so its re-appearance proves
	// re-rendering rather than "it was already there from the initial
	// provision".
	if err := os.WriteFile(filepath.Join(globalScionDir, "harness-configs", "generic", "config.yaml"),
		[]byte("harness: generic\nimage: test-image:v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	agentInfoPath := filepath.Join(agentHome, "agent-info.json")
	if err := os.Remove(agentInfoPath); err != nil {
		t.Fatalf("failed to remove agent-info.json fixture: %v", err)
	}

	// Whole-tree snapshots (R4), not spot checks of one file: the sibling's
	// entire agentDir and home must be provably untouched.
	siblingDirBefore := snapshotTree(t, siblingDir)
	siblingHomeBefore := snapshotTree(t, siblingHome)
	checkoutBefore := snapshotTree(t, sharedCheckout)
	gitDirBefore := snapshotTree(t, filepath.Join(sharedCheckout, ".git"))

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		Workspace: sharedCheckout, SharedWorkspace: true,
	}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}
	checkoutAfter := snapshotTree(t, sharedCheckout)
	if !maps.Equal(checkoutBefore, checkoutAfter) {
		t.Fatalf("shared checkout changed after Reprovision:\nbefore=%v\nafter=%v", checkoutBefore, checkoutAfter)
	}
	gitDirAfter := snapshotTree(t, filepath.Join(sharedCheckout, ".git"))
	if !maps.Equal(gitDirBefore, gitDirAfter) {
		t.Fatalf(".git directory was written to during Reprovision:\nbefore=%v\nafter=%v", gitDirBefore, gitDirAfter)
	}

	// The reprovisioned agent's own state is genuinely RE-RENDERED (R4), not
	// merely still present from the initial provision.
	cfgData, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		t.Fatalf("expected scion-agent.json to exist: %v", err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		t.Fatalf("failed to parse scion-agent.json: %v", err)
	}
	if cfg.Image != "test-image:v2" {
		t.Fatalf("expected the bumped harness-config image to be re-rendered into scion-agent.json, got %q", cfg.Image)
	}
	if _, err := os.Stat(agentInfoPath); err != nil {
		t.Fatalf("expected agent-info.json (deleted before Reprovision) to be re-rendered: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentHome, "notes.txt")); err != nil {
		t.Fatalf("agent home file lost: %v", err)
	}

	// The sibling agent, sharing the same mount, must be completely
	// untouched: a whole-tree snapshot of both its agentDir and its home.
	siblingDirAfter := snapshotTree(t, siblingDir)
	if !maps.Equal(siblingDirBefore, siblingDirAfter) {
		t.Fatalf("sibling agentDir changed after Reprovision:\nbefore=%v\nafter=%v", siblingDirBefore, siblingDirAfter)
	}
	siblingHomeAfter := snapshotTree(t, siblingHome)
	if !maps.Equal(siblingHomeBefore, siblingHomeAfter) {
		t.Fatalf("sibling home changed after Reprovision:\nbefore=%v\nafter=%v", siblingHomeBefore, siblingHomeAfter)
	}
}

// TestReprovision_ExplicitMount_HubManaged_Positive is the design §3.4
// Amendment A23.1 (review p1b-r1) R4 positive case for a hub-managed project
// (SharedWorkspace=false): the explicit-mount branch must also work for an
// agent whose Workspace is not under the external shared-workspace agents
// root at all (config.GetAgentDir's in-project fallback), the same as for a
// shared-workspace git project.
func TestReprovision_ExplicitMount_HubManaged_Positive(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "hub-managed-agent"
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("hub-managed project"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := ProvisionAgent(context.Background(), agentName, "default", "", "", scionDir, "", "created", "", workspace); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	agentDir := config.GetAgentDir(scionDir, agentName, false)
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	if err := os.WriteFile(filepath.Join(agentHome, "notes.txt"), []byte("hub-managed note"), 0644); err != nil {
		t.Fatal(err)
	}
	workspaceBefore := snapshotTree(t, workspace)

	mgr := NewManager(&runtime.MockRuntime{})
	if _, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		Workspace: workspace, SharedWorkspace: false,
	}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}

	workspaceAfter := snapshotTree(t, workspace)
	if !maps.Equal(workspaceBefore, workspaceAfter) {
		t.Fatalf("hub-managed workspace changed after Reprovision:\nbefore=%v\nafter=%v", workspaceBefore, workspaceAfter)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "scion-agent.json")); err != nil {
		t.Fatalf("expected scion-agent.json to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentHome, "notes.txt")); err != nil {
		t.Fatalf("agent home file lost: %v", err)
	}
}

// TestReprovision_ExplicitMount_NoAgentDir_Refused is the design §3.4
// Amendment A23.1 (review p1b-r1) R4 regression test for the "agent dir must
// already exist" precondition (added deliberately in the initial A23 PR,
// but previously untested): Reprovision must refuse a name that was never
// actually provisioned, rather than letting ProvisionAgent's unconditional
// os.MkdirAll silently stand up a fresh, empty agent directory for it.
func TestReprovision_ExplicitMount_NoAgentDir_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "never-provisioned-agent"
	workspace := t.TempDir()

	agentDir := config.GetAgentDir(scionDir, agentName, true)
	if _, statErr := os.Stat(agentDir); !os.IsNotExist(statErr) {
		t.Fatalf("fixture check: agentDir must not exist yet: %v", statErr)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		Workspace: workspace, SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse an agent with no existing agent directory, got nil error")
	}
	if !errors.Is(err, ErrReprovisionRefused) {
		t.Fatalf("expected ErrReprovisionRefused, got %v", err)
	}
	if _, statErr := os.Stat(agentDir); !os.IsNotExist(statErr) {
		t.Fatalf("DATA INTEGRITY: Reprovision must never create the agent directory for a name that was never provisioned: %v", statErr)
	}
}

// TestReprovision_ExplicitMount_MissingWorkspacePath_Refused is the design
// §3.4 Amendment A23 regression test for the "an absolute workspace path
// exists and is a directory" precondition: Reprovision must refuse — not
// create — a missing explicit-mount workspace path.
func TestReprovision_ExplicitMount_MissingWorkspacePath_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "missing-ws-agent"
	validWorkspace := t.TempDir()

	ctx := api.ContextWithSharedWorkspace(context.Background())
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", validWorkspace); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}

	missingWorkspace := filepath.Join(t.TempDir(), "gone")
	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		Workspace: missingWorkspace, SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse a missing explicit workspace path, got nil error")
	}
	if !errors.Is(err, ErrReprovisionRefused) {
		t.Fatalf("expected ErrReprovisionRefused, got %v", err)
	}
	if _, statErr := os.Stat(missingWorkspace); statErr == nil {
		t.Fatal("DATA INTEGRITY: Reprovision must never create the missing workspace path")
	}
}

// TestReprovision_ExplicitMount_RelativeWorkspaceEscapesRoot_Refused is the
// design §3.4 Amendment A23 regression test for the relative-workspace case:
// Reprovision reuses resolveWorkspaceSubdir's containment check, so a
// relative workspace that escapes the project root is refused rather than
// resolved to a path outside it.
func TestReprovision_ExplicitMount_RelativeWorkspaceEscapesRoot_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "escape-ws-agent"
	validWorkspace := t.TempDir()

	ctx := api.ContextWithSharedWorkspace(context.Background())
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", validWorkspace); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		Workspace: "../../etc", SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse a relative workspace escaping the project root, got nil error")
	}
	if !errors.Is(err, ErrReprovisionRefused) {
		t.Fatalf("expected ErrReprovisionRefused, got %v", err)
	}
}

// TestReprovision_ExplicitMount_RelativeWorkspaceResolvesToFile_Refused is
// the design §3.4 Amendment A23.1 (review p1b-r1) O2 regression test:
// resolveWorkspaceSubdir confirms a relative workspace exists and is
// contained under the project root, but not that it is a directory —
// unlike the absolute-path branch. A relative workspace that resolves to a
// regular file must be refused the same way a missing path is.
func TestReprovision_ExplicitMount_RelativeWorkspaceResolvesToFile_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "relative-file-ws-agent"
	validWorkspace := t.TempDir()

	ctx := api.ContextWithSharedWorkspace(context.Background())
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", validWorkspace); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}

	// resolveProjectRoot treats the parent of a ".scion" projectDir as the
	// project root a relative --workspace resolves against.
	projectRoot := filepath.Dir(scionDir)
	if err := os.WriteFile(filepath.Join(projectRoot, "not-a-dir.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		Workspace: "not-a-dir.txt", SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse a relative workspace that resolves to a regular file, got nil error")
	}
	if !errors.Is(err, ErrReprovisionRefused) {
		t.Fatalf("expected ErrReprovisionRefused, got %v", err)
	}
}

// TestReprovision_ExplicitMount_AbsoluteWorkspaceIsFile_Refused is the
// upstream review regression test (GoogleCloudPlatform/scion#2037, comment
// 4121261313) for the absolute-path counterpart of
// TestReprovision_ExplicitMount_RelativeWorkspaceResolvesToFile_Refused: an
// absolute --workspace that exists but is a regular file must be refused
// with the precise "is not a directory" message, not the "does not exist"
// message the unsplit check used to produce.
func TestReprovision_ExplicitMount_AbsoluteWorkspaceIsFile_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "absolute-file-ws-agent"
	validWorkspace := t.TempDir()

	ctx := api.ContextWithSharedWorkspace(context.Background())
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", validWorkspace); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}

	notADir := filepath.Join(t.TempDir(), "not-a-dir.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		Workspace: notADir, SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse an absolute workspace that is a regular file, got nil error")
	}
	if !errors.Is(err, ErrReprovisionRefused) {
		t.Fatalf("expected ErrReprovisionRefused, got %v", err)
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("expected the precise 'is not a directory' message, got: %v", err)
	}
	if strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("the path DOES exist (as a file); the message must not claim otherwise: %v", err)
	}
}

// TestReprovision_ExplicitMount_RelativeWorkspace_SettingsLoadError_Refused
// is the upstream review regression test (GoogleCloudPlatform/scion#2037,
// comment 4121261307): a relative --workspace requires resolveProjectRoot,
// which reads project settings via config.LoadEffectiveSettings. A genuine
// load failure (here, a settings.yaml that fails to parse) must surface as
// an explicit error -- not be silently ignored and left to
// resolveProjectRoot to run against a nil/stale *VersionedSettings.
//
// This is deliberately NOT wrapped in ErrReprovisionRefused (see the
// production comment at the call site): it is an environment/config
// failure, not an eligibility refusal, matching how the resolve-project-dir
// error a few lines above the fix is also left unwrapped.
func TestReprovision_ExplicitMount_RelativeWorkspace_SettingsLoadError_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "settings-load-error-agent"
	validWorkspace := t.TempDir()

	ctx := api.ContextWithSharedWorkspace(context.Background())
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", validWorkspace); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}

	// A settings.yaml that DetectSettingsFormat/koanf's YAML parser cannot
	// parse: valid enough to be recognized as a candidate settings file, but
	// syntactically broken, so config.LoadEffectiveSettings returns a real
	// error rather than silently falling back to defaults.
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("not: valid: yaml: [[["), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
		Workspace: "relative-subdir", SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Reprovision to fail when project settings cannot be loaded, got nil error")
	}
	if errors.Is(err, ErrReprovisionRefused) {
		t.Fatalf("a settings-load failure is an environment error, not an eligibility refusal; must not be ErrReprovisionRefused: %v", err)
	}
	if !strings.Contains(err.Error(), "load effective settings") {
		t.Fatalf("expected the error to name the settings-load failure, got: %v", err)
	}
}

// TestReprovision_NeitherGitCloneNorWorkspace_Refused covers the remaining
// api.ReincarnateEligible case: an agent with no GitClone and no Workspace at
// all must be refused, the same as today, rather than silently falling
// through to some other ProvisionAgent branch.
func TestReprovision_NeitherGitCloneNorWorkspace_Refused(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "neither-agent"

	mgr := NewManager(&runtime.MockRuntime{})
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true,
	})
	if err == nil {
		t.Fatal("expected Reprovision to refuse an agent with neither GitClone nor Workspace, got nil error")
	}
	if !errors.Is(err, ErrReprovisionRefused) {
		t.Fatalf("expected ErrReprovisionRefused, got %v", err)
	}
}

// TestReprovision_IgnoresProvisionedWorktreeSignalForCloneWorkspace covers
// Reprovision's path through persistProvisionedWorktreeRepoRootIfValid — the
// same shared gate ProvisionAgent uses for the hub's provision-only flow
// (see TestTryProvisionWorktree_ProvisionThenStart_RepoRootSurvives in
// pkg/runtimebroker). Reprovision is clone-per-agent only (GitClone must be
// set), so ProvisionAgent's workspace-resolution logic never assigns
// workspaceSource for it — only the git-clone branch runs, which leaves
// workspaceSource empty. Reprovision's clone-per-agent path leaves
// workspaceSource empty, so the shared gate must not persist any ctx value,
// even one naming a genuine worktree base: no repo-root state file should
// end up on disk. (A ctx signal is never produced on this path in practice
// today, since tryProvisionWorktree and Reprovision's GitClone precondition
// are mutually exclusive; this test supplies one anyway to prove the gate
// itself is safe regardless.)
func TestReprovision_IgnoresProvisionedWorktreeSignalForCloneWorkspace(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	agentName := "clone-agent-with-signal"
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	ctx := api.ContextWithGitClone(context.Background(), gc)
	if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "created", "", ""); err != nil {
		t.Fatalf("initial ProvisionAgent: %v", err)
	}
	ws := filepath.Join(scionDir, "agents", agentName, "workspace")
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0755)

	// A REAL base with a REAL worktree — deliberately a value that WOULD
	// validate if it were ever checked against a matching workspace, so this
	// test cannot pass merely because the injected value is garbage. The
	// gate still must not persist it, because workspaceSource stays empty on
	// this path regardless of what the ctx value names.
	realBase := t.TempDir()
	setupGitRepo(t, realBase)
	_ = createRealWorktree(t, realBase, "agent-1")

	mgr := NewManager(&runtime.MockRuntime{})
	reprovisionCtx := api.ContextWithProvisionedWorktreeRepoRoot(
		api.ContextWithGitClone(context.Background(), gc), realBase)
	if _, err := mgr.Reprovision(reprovisionCtx, api.StartOptions{
		Name: agentName, Template: "default", ProjectPath: scionDir, BrokerMode: true, GitClone: gc,
	}); err != nil {
		t.Fatalf("Reprovision: %v", err)
	}
	agentDir := config.GetAgentDir(scionDir, agentName, false)
	if got := readProvisionedWorktreeRepoRoot(agentDir); got != "" {
		t.Fatalf("readProvisionedWorktreeRepoRoot(agentDir) = %q, want \"\" (a ctx signal must not be trusted against a clone-per-agent workspace, even one naming a genuine worktree base elsewhere)", got)
	}
}
