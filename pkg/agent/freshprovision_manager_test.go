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
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// freshProvisionManagerFixture is a minimal on-disk Scion installation plus a
// mock runtime, wired so a real AgentManager.Start() call reaches GetAgent's
// FreshProvision-gated workspace-clearing block (GoogleCloudPlatform/scion#1931)
// through the full broker -> StartOptions -> ctx -> GetAgent chain, not a
// hand-built context. It seeds an existing agent directory with a populated
// workspace before Start() runs, simulating a leftover from a prior agent
// with the same name.
type freshProvisionManagerFixture struct {
	t                *testing.T
	projectScionDir  string
	agentWorkspace   string
	unpushedFilePath string
	mgr              Manager
	opts             api.StartOptions
}

const freshProvisionUnpushedContent = "package main // un-pushed change\n"

func newFreshProvisionManagerFixture(t *testing.T) *freshProvisionManagerFixture {
	t.Helper()

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	mkdirAll(t, tplDir)
	writeFile(t, filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	mkdirAll(t, hcDir)
	writeFile(t, filepath.Join(hcDir, "config.yaml"), "harness: gemini\nuser: scion\nimage: test-image:latest\n")

	writeFile(t, filepath.Join(globalScionDir, "settings.yaml"),
		"schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	mkdirAll(t, projectScionDir)

	// Seed a fully provisioned agent directory with a populated workspace,
	// as if left over from a same-named agent whose local files were not
	// cleaned up (create's case) or whose container stopped without
	// destroying the host workspace (start/restart's case).
	agentDir := filepath.Join(projectScionDir, "agents", "existing-agent")
	agentWorkspace := filepath.Join(agentDir, "workspace")
	agentHome := filepath.Join(agentDir, "home")
	mkdirAll(t, agentWorkspace)
	mkdirAll(t, agentHome)
	writeFile(t, filepath.Join(agentDir, "scion-agent.json"), `{"harness":"gemini","default_harness_config":"test-harness"}`)
	writeFile(t, filepath.Join(agentWorkspace, ".git"), "gitdir: ../../../.git/worktrees/existing-agent\n")
	unpushedFilePath := filepath.Join(agentWorkspace, "unpushed.go")
	writeFile(t, unpushedFilePath, freshProvisionUnpushedContent)

	fx := &freshProvisionManagerFixture{
		t:                t,
		projectScionDir:  projectScionDir,
		agentWorkspace:   agentWorkspace,
		unpushedFilePath: unpushedFilePath,
	}

	fx.mgr = NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	})
	fx.opts = api.StartOptions{
		Name:        "existing-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/example/repo.git",
			Branch: "main",
		},
	}
	return fx
}

// TestManagerStart_PreservesExistingWorkspaceWithoutFreshProvision is the
// real-Manager guard for GoogleCloudPlatform/scion#1931's create-only wipe
// gate: a start dispatch (StartOptions.FreshProvision left at its zero value, as
// buildStartContext sets it for opHTTPStart/opHTTPRestart) reaches
// GetAgent through the real Start() -> ctx -> GetAgent chain, and must never
// clear an existing populated workspace even though GitClone is set.
func TestManagerStart_PreservesExistingWorkspaceWithoutFreshProvision(t *testing.T) {
	fx := newFreshProvisionManagerFixture(t)
	// fx.opts.FreshProvision left at its zero value (false): this is what
	// buildStartContext sets for opHTTPStart and opHTTPRestart.

	if _, err := fx.mgr.Start(context.Background(), fx.opts); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	got, err := os.ReadFile(fx.unpushedFilePath)
	if err != nil {
		t.Fatalf("un-pushed file must survive a start dispatch, but reading it failed: %v", err)
	}
	if string(got) != freshProvisionUnpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, freshProvisionUnpushedContent)
	}
	if _, err := os.Stat(filepath.Join(fx.agentWorkspace, ".git")); err != nil {
		t.Errorf(".git must survive a start dispatch, stat error: %v", err)
	}
}

// TestManagerStart_ClearsExistingWorkspaceWithFreshProvision is the
// real-Manager guard for the other half of the create-only wipe gate: a create dispatch
// (StartOptions.FreshProvision: true, as buildStartContext sets it for
// opCreate) reaches GetAgent through the real Start() -> ctx -> GetAgent
// chain, and clears a leftover populated workspace from a same-named agent.
func TestManagerStart_ClearsExistingWorkspaceWithFreshProvision(t *testing.T) {
	fx := newFreshProvisionManagerFixture(t)
	fx.opts.FreshProvision = true

	if _, err := fx.mgr.Start(context.Background(), fx.opts); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if _, err := os.Stat(fx.unpushedFilePath); !os.IsNotExist(err) {
		t.Errorf("expected the leftover workspace to be cleared on a fresh-provision dispatch, stat error: %v", err)
	}
}

// TestManagerProvision_ClearsExistingWorkspaceWithFreshProvision is the
// real-Manager guard for the ProvisionOnly path (GoogleCloudPlatform/scion#1918's
// createAgent branch that calls Manager.Provision instead of Start):
// buildProvisionContext must thread opts.FreshProvision onto ctx the same
// way Start() does, so a plain (non-reprovision) provision-only create still
// clears a leftover populated workspace from a same-named agent.
func TestManagerProvision_ClearsExistingWorkspaceWithFreshProvision(t *testing.T) {
	fx := newFreshProvisionManagerFixture(t)
	fx.opts.FreshProvision = true

	if _, err := fx.mgr.Provision(context.Background(), fx.opts); err != nil {
		t.Fatalf("Provision() failed: %v", err)
	}

	if _, err := os.Stat(fx.unpushedFilePath); !os.IsNotExist(err) {
		t.Errorf("expected the leftover workspace to be cleared by a fresh-provision Provision() call, stat error: %v", err)
	}
}

// TestManagerReprovision_PreservesExistingWorkspace proves a reprovision
// (reincarnation) create never wipes the existing workspace it targets, even
// though opts.FreshProvision was true for opCreate before the handler-level
// !Reprovision guard clears it: Manager.Reprovision calls ProvisionAgent, not
// GetAgent, and ProvisionAgent's own git-clone check already preserves a
// real existing clone directory regardless of FreshProvision.
func TestManagerReprovision_PreservesExistingWorkspace(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	mkdirAll(t, tplDir)
	writeFile(t, filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	mkdirAll(t, hcDir)
	writeFile(t, filepath.Join(hcDir, "config.yaml"), "harness: gemini\nuser: scion\nimage: test-image:latest\n")

	writeFile(t, filepath.Join(globalScionDir, "settings.yaml"),
		"schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	mkdirAll(t, projectScionDir)

	// Reprovision requires a REAL existing git clone (.git as a directory),
	// unlike the worktree-pointer-file shape the other fixtures in this file
	// use: it refuses (ErrReprovisionRefused) when that precondition isn't met.
	agentDir := filepath.Join(projectScionDir, "agents", "existing-agent")
	agentWorkspace := filepath.Join(agentDir, "workspace")
	agentHome := filepath.Join(agentDir, "home")
	mkdirAll(t, agentWorkspace)
	mkdirAll(t, agentHome)
	mkdirAll(t, filepath.Join(agentWorkspace, ".git"))
	writeFile(t, filepath.Join(agentDir, "scion-agent.json"), `{"harness":"gemini","default_harness_config":"test-harness"}`)
	unpushedFilePath := filepath.Join(agentWorkspace, "unpushed.go")
	writeFile(t, unpushedFilePath, freshProvisionUnpushedContent)

	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil // no running container: Reprovision proceeds
		},
	})
	opts := api.StartOptions{
		Name:        "existing-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/example/repo.git",
			Branch: "main",
		},
		// The handler-level !Reprovision guard clears this before calling
		// Manager.Reprovision; set it here directly to prove Reprovision
		// itself preserves the workspace, independent of that guard.
		FreshProvision: true,
	}

	if _, err := mgr.Reprovision(context.Background(), opts); err != nil {
		t.Fatalf("Reprovision() failed: %v", err)
	}

	got, err := os.ReadFile(unpushedFilePath)
	if err != nil {
		t.Fatalf("un-pushed file must survive a reprovision, but reading it failed: %v", err)
	}
	if string(got) != freshProvisionUnpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, freshProvisionUnpushedContent)
	}
	if _, err := os.Stat(filepath.Join(agentWorkspace, ".git")); err != nil {
		t.Errorf(".git must survive a reprovision, stat error: %v", err)
	}
}
