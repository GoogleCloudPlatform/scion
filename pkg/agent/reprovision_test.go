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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// reprovisionSetup builds a git project with a gitignored .scion/agents and a
// global generic harness-config + default template. CWD is set OUTSIDE the
// repo, as on a runtime broker — this is what makes util.BranchExists return
// false for a broker process, which is the root cause C1 fixes against.
func reprovisionSetup(t *testing.T) (scionDir, globalScionDir string) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
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
// R3 is specifically about a still-running container, not about requiring a
// prior successful stop confirmation.
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
