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

package runtimebroker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStartAgentAfterCreate_WorktreePerAgent_RealHTTPHandlers dispatches a
// create and then a start for a worktree-per-agent agent through the real
// HTTP handler chain (srv.Handler().ServeHTTP) rather than a hand-built
// startContextInputs value. The control-channel transport reconstructs an
// http.Request and calls this exact same handler chain
// (controlchannel.go's c.handlers.ServeHTTP) instead of a separate code
// path, so this reproduces the failure for both transports.
//
// The start request never carries the Hub UUID in its URL path (that slot
// holds the agent's slug) — the Hub UUID is only available via
// resolvedEnv["SCION_AGENT_ID"], which the Hub always sets on every start
// dispatch. Likewise, the request's own projectId query parameter carries
// the project ID. Both must reach buildStartContext so the worktree path
// resolves to this agent's own worktree, not the shared "worktrees" parent
// directory that already exists for the project.
func TestStartAgentAfterCreate_WorktreePerAgent_RealHTTPHandlers(t *testing.T) {
	requireWorktreeGit(t)
	srv, mgr := newTestServerWithGitCloneCapture()

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	createBody := fmt.Sprintf(`{
		"id": "agent-hub-uuid-1",
		"name": "wt-agent",
		"projectId": "proj-1",
		"projectPath": %q,
		"projectSlug": "proj-1",
		"workspaceMode": "worktree-per-agent",
		"config": {
			"template": "claude",
			"gitClone": {"url": %q, "branch": "main"}
		}
	}`, projectPath, bare)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	firstWorkspace := mgr.lastWorkspace
	if firstWorkspace == "" {
		t.Fatal("expected create to provision a worktree and set opts.Workspace")
	}
	if _, err := os.Stat(firstWorkspace); err != nil {
		t.Fatalf("expected the created worktree to exist on disk: %v", err)
	}

	// Simulate un-pushed work sitting in the agent's own worktree.
	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(firstWorkspace, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start carries the same workspace inputs create did, plus the
	// resolvedEnv the Hub always injects for a start dispatch
	// (pkg/hub/httpdispatcher.go's DispatchAgentStart) — SCION_AGENT_ID and
	// SCION_PROJECT_ID. The URL path segment is the agent's slug ("wt-agent"),
	// matching what the broker's Hub client actually sends, not the Hub UUID.
	startBody := fmt.Sprintf(`{
		"projectPath": %q,
		"projectSlug": "proj-1",
		"workspaceMode": "worktree-per-agent",
		"gitClone": {"url": %q, "branch": "main"},
		"resolvedEnv": {"SCION_AGENT_ID": "agent-hub-uuid-1", "SCION_PROJECT_ID": "proj-1"}
	}`, projectPath, bare)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/wt-agent/start?projectId=proj-1", strings.NewReader(startBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if mgr.lastWorkspace != firstWorkspace {
		t.Errorf("expected start to reuse create's worktree %q, got %q", firstWorkspace, mgr.lastWorkspace)
	}

	got, err := os.ReadFile(unpushedFile)
	if err != nil {
		t.Fatalf("un-pushed file must survive a start dispatch, but reading it failed: %v", err)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestStartAgentAfterCreate_WorktreePerAgent_NeverRemovesSharedWorktreesDir
// is the inverse-hazard guard: with AgentID/ProjectID correctly threaded
// through from the start request, a start whose own worktree target is
// occupied by something that is not a git worktree fails closed — it is
// never reused, and it is never removed — and the project's shared
// "worktrees" parent directory and every other agent's worktree inside it
// survive untouched.
func TestStartAgentAfterCreate_WorktreePerAgent_NeverRemovesSharedWorktreesDir(t *testing.T) {
	requireWorktreeGit(t)
	srv, mgr := newTestServerWithGitCloneCapture()

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	createBody := fmt.Sprintf(`{
		"id": "agent-hub-uuid-1",
		"name": "wt-agent",
		"projectId": "proj-1",
		"projectPath": %q,
		"projectSlug": "proj-1",
		"workspaceMode": "worktree-per-agent",
		"config": {
			"template": "claude",
			"gitClone": {"url": %q, "branch": "main"}
		}
	}`, projectPath, bare)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	worktreesDir := filepath.Join(projectPath, "workspace", "worktrees")
	if _, err := os.Stat(worktreesDir); err != nil {
		t.Fatalf("expected the shared worktrees dir to exist after create: %v", err)
	}
	agent1Worktree := mgr.lastWorkspace

	// A second agent under the same project, whose worktree target is
	// occupied by a plain file rather than a real git worktree.
	const occupyingContent = "occupied"
	agent2WorktreePath := filepath.Join(worktreesDir, "agent-hub-uuid-2")
	if err := os.WriteFile(agent2WorktreePath, []byte(occupyingContent), 0o644); err != nil {
		t.Fatal(err)
	}

	startBody := fmt.Sprintf(`{
		"projectPath": %q,
		"projectSlug": "proj-1",
		"workspaceMode": "worktree-per-agent",
		"gitClone": {"url": %q, "branch": "main"},
		"resolvedEnv": {"SCION_AGENT_ID": "agent-hub-uuid-2", "SCION_PROJECT_ID": "proj-1"}
	}`, projectPath, bare)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/wt-agent-2/start?projectId=proj-1", strings.NewReader(startBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusAccepted {
		t.Fatalf("start: expected the non-worktree occupant to fail the request, got status %d: %s", w.Code, w.Body.String())
	}

	// The occupying file must survive byte-for-byte: it is never reused as
	// a workspace, and it is never removed.
	got, err := os.ReadFile(agent2WorktreePath)
	if err != nil {
		t.Fatalf("agent-2's occupying file must survive, but reading it failed: %v", err)
	}
	if string(got) != occupyingContent {
		t.Errorf("agent-2's occupying file content changed: got %q, want %q", got, occupyingContent)
	}

	// The shared worktrees dir — and agent-1's worktree inside it — must
	// also survive.
	if _, err := os.Stat(worktreesDir); err != nil {
		t.Fatalf("shared worktrees dir must never be removed, stat error: %v", err)
	}
	if _, err := os.Stat(agent1Worktree); err != nil {
		t.Errorf("agent-1's worktree must survive agent-2's start, stat error: %v", err)
	}
}

// TestStartAgentAfterCreate_WorktreePerAgent_RealManagerPreservesWorkspace
// drives create-then-start through New(cfg, agent.NewManager(&runtime.MockRuntime{}), rt)
// — the real agent.Manager, not a hand-rolled capture shim — so the start
// dispatch traverses the full production chain: HTTP handler ->
// buildStartContext -> ctx -> Manager.Start -> GetAgent. It proves an
// un-pushed file and .git in the worktree survive that full chain. In
// worktree-per-agent mode opts.GitClone is suppressed and GetAgent's
// workspace-clearing step targets agents/<name>/workspace, not the
// worktree, so this test does not exercise that step; the FreshProvision
// assertions in handlers_test.go and freshprovision_manager_test.go cover it.
func TestStartAgentAfterCreate_WorktreePerAgent_RealManagerPreservesWorkspace(t *testing.T) {
	requireWorktreeGit(t)
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	mustMkdirAll := func(path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWriteFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	mustMkdirAll(tplDir)
	mustWriteFile(filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	mustMkdirAll(hcDir)
	mustWriteFile(filepath.Join(hcDir, "config.yaml"), "harness: gemini\nuser: scion\nimage: test-image:latest\n")

	mustWriteFile(filepath.Join(globalScionDir, "settings.yaml"),
		"schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	mustMkdirAll(projectScionDir)

	bare := initBareRepoWithCommit(t)

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.ForceRuntime = "mock"
	realMgr := agent.NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			return "mock-container-id", nil
		},
	})
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, realMgr, rt)

	createBody := fmt.Sprintf(`{
		"id": "agent-hub-uuid-1",
		"name": "wt-agent",
		"projectId": "proj-1",
		"projectPath": %q,
		"projectSlug": "proj-1",
		"workspaceMode": "worktree-per-agent",
		"config": {
			"template": "default",
			"gitClone": {"url": %q, "branch": "main"}
		}
	}`, projectScionDir, bare)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	worktreePath := filepath.Join(projectScionDir, "workspace", "worktrees", "agent-hub-uuid-1")
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected create to provision the worktree on disk: %v", err)
	}

	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(worktreePath, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0o644); err != nil {
		t.Fatal(err)
	}

	startBody := fmt.Sprintf(`{
		"projectPath": %q,
		"projectSlug": "proj-1",
		"workspaceMode": "worktree-per-agent",
		"gitClone": {"url": %q, "branch": "main"},
		"resolvedEnv": {"SCION_AGENT_ID": "agent-hub-uuid-1", "SCION_PROJECT_ID": "proj-1"}
	}`, projectScionDir, bare)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/wt-agent/start?projectId=proj-1", strings.NewReader(startBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	got, err := os.ReadFile(unpushedFile)
	if err != nil {
		t.Fatalf("un-pushed file must survive a start dispatch through the real Manager, but reading it failed: %v", err)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
	if _, err := os.Stat(filepath.Join(worktreePath, ".git")); err != nil {
		t.Errorf(".git must survive a start dispatch through the real Manager, stat error: %v", err)
	}
}

// TestStartAgentAfterCreate_WorktreePerAgent_UsesSlugForBranchKey proves the
// start handler always uses the URL's own slug as the agent identity — the
// same value used for the agent directory, container name, and worktree
// branch/sharer-registry key — never a value carried in resolvedEnv. A
// start whose resolvedEnv carries a different display name under
// SCION_AGENT_NAME (as happens after a rename, since the display name and
// the slug can diverge) still resolves and registers under the slug that
// create used for the same agent.
func TestStartAgentAfterCreate_WorktreePerAgent_UsesSlugForBranchKey(t *testing.T) {
	requireWorktreeGit(t)
	srv, mgr := newTestServerWithGitCloneCapture()

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	const slug = "wt-agent-slug"
	const renamedDisplayName = "renamed-agent-name"

	createBody := fmt.Sprintf(`{
		"id": "agent-hub-uuid-1",
		"name": %q,
		"projectId": "proj-1",
		"projectPath": %q,
		"projectSlug": "proj-1",
		"workspaceMode": "worktree-per-agent",
		"config": {
			"template": "claude",
			"gitClone": {"url": %q, "branch": "main"}
		}
	}`, slug, projectPath, bare)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	firstWorkspace := mgr.lastWorkspace
	if firstWorkspace == "" {
		t.Fatal("expected create to provision a worktree and set opts.Workspace")
	}

	// Simulate a display-name change (a PATCH rename) between create and
	// start: resolvedEnv carries the new name, which this handler must
	// never consume for identity.
	startBody := fmt.Sprintf(`{
		"projectPath": %q,
		"projectSlug": "proj-1",
		"workspaceMode": "worktree-per-agent",
		"gitClone": {"url": %q, "branch": "main"},
		"resolvedEnv": {"SCION_AGENT_ID": "agent-hub-uuid-1", "SCION_AGENT_NAME": %q, "SCION_PROJECT_ID": "proj-1"}
	}`, projectPath, bare, renamedDisplayName)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+slug+"/start?projectId=proj-1", strings.NewReader(startBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastWorkspace != firstWorkspace {
		t.Errorf("expected start to reuse create's worktree %q, got %q", firstWorkspace, mgr.lastWorkspace)
	}

	base := filepath.Join(projectPath, "workspace")
	sharers, wtPath, err := provision.ListSharers(base, "", slug)
	if err != nil {
		t.Fatalf("ListSharers(%q): %v", slug, err)
	}
	if wtPath != firstWorkspace {
		t.Errorf("registry worktreePath for %q = %q, want %q", slug, wtPath, firstWorkspace)
	}
	if len(sharers) != 1 {
		t.Errorf("expected 1 sharer under the slug %q, got %d: %v", slug, len(sharers), sharers)
	}

	if _, strayPath, err := provision.ListSharers(base, "", renamedDisplayName); err == nil && strayPath != "" {
		t.Errorf("expected no registration under the renamed display name %q, found %q", renamedDisplayName, strayPath)
	}
}

// TestStartAgentAfterCreate_EnvAgentNameIgnored proves the start handler
// takes an agent's identity only from the URL's own slug: an env-supplied
// display name in resolvedEnv is never used for any filesystem path,
// container name, or branch key. Driven through a real agent.Manager so the
// assertion covers the full production chain, not just the handler's own
// decoding. An unrelated, pre-existing agent's directory and workspace
// file, and the project's own .scion directory, must survive regardless of
// what this dispatch does.
func TestStartAgentAfterCreate_EnvAgentNameIgnored(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	mustMkdirAll := func(path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWriteFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	mustMkdirAll(tplDir)
	mustWriteFile(filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	mustMkdirAll(hcDir)
	mustWriteFile(filepath.Join(hcDir, "config.yaml"), "harness: gemini\nuser: scion\nimage: test-image:latest\n")

	mustWriteFile(filepath.Join(globalScionDir, "settings.yaml"),
		"schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	mustMkdirAll(projectScionDir)

	// A pre-existing, unrelated agent under the same project. Its files
	// must survive regardless of what this test's start dispatch does.
	existingAgentDir := filepath.Join(projectScionDir, "agents", "existing-agent")
	existingWorkspace := filepath.Join(existingAgentDir, "workspace")
	mustMkdirAll(existingWorkspace)
	mustWriteFile(filepath.Join(existingAgentDir, "scion-agent.json"), `{"harness":"gemini","default_harness_config":"test-harness"}`)
	sentinelFile := filepath.Join(existingWorkspace, "sentinel.go")
	mustWriteFile(sentinelFile, "package main // must survive\n")

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.ForceRuntime = "mock"
	realMgr := agent.NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			return "mock-container-id", nil
		},
	})
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, realMgr, rt)

	// Start a different agent, "other-agent", with a distinct value under
	// SCION_AGENT_NAME in resolvedEnv. If the handler ever used that value
	// for opts.Name instead of the URL slug, it would take effect here.
	startBody := fmt.Sprintf(`{
		"projectPath": %q,
		"resolvedEnv": {"SCION_AGENT_NAME": ".."}
	}`, projectScionDir)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/other-agent/start", strings.NewReader(startBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Whatever the response status, the project directory and the
	// unrelated agent must survive untouched.
	if _, err := os.Stat(projectScionDir); err != nil {
		t.Fatalf("project .scion dir must survive, stat error: %v", err)
	}
	if _, err := os.Stat(existingAgentDir); err != nil {
		t.Errorf("an unrelated agent's directory must survive, stat error: %v", err)
	}
	got, err := os.ReadFile(sentinelFile)
	if err != nil {
		t.Fatalf("an unrelated agent's workspace file must survive, but reading it failed: %v", err)
	}
	if string(got) != "package main // must survive\n" {
		t.Errorf("sentinel file content changed: %q", got)
	}
}

// TestHandleAgentByID_InvalidIDRejected proves the broker rejects a URL
// path segment that is not a single valid path element, for every action
// this handler dispatches to except a bare DELETE, before any of them run.
// "a\b" (a literal backslash, not a "." or ".." segment) is used because
// net/http's own mux already redirects and cleans dot-segments before a
// handler ever sees them, so it would not exercise this check. A bare
// DELETE (no action sub-path) is exempted from this gate: see
// TestDeleteAgent_ContainerOnlyRemovalSurvivesBadName and
// TestDeleteAgent_InvalidIDWithDeleteFilesRejected for that path's own,
// narrower validation. The "delete with action" and "delete attach with
// websocket upgrade" cases below prove a DELETE naming an action sub-path
// is not exempted: the gate applies to it exactly as it would to any other
// method.
func TestHandleAgentByID_InvalidIDRejected(t *testing.T) {
	srv, _ := newTestServerWithGitCloneCapture()

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"get", http.MethodGet, "/api/v1/agents/a%5Cb"},
		{"start action", http.MethodPost, "/api/v1/agents/a%5Cb/start"},
		{"delete with action", http.MethodDelete, "/api/v1/agents/a%5Cb/start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
			}
		})
	}

	// A DELETE naming the "attach" action, with WebSocket upgrade headers,
	// is not a bare DELETE either (it has an action sub-path): the id check
	// must still apply before the WebSocket-upgrade branch is reached.
	t.Run("delete attach with websocket upgrade", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/a%5Cb/attach", nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
		}
	})
}

// TestStartAgent_InvalidProjectIDRejected and TestCreateAgent_InvalidProjectIDRejected
// prove the broker validates a non-empty projectId as a single path element
// before it reaches buildStartContext, on both entry points that accept one.
func TestStartAgent_InvalidProjectIDRejected(t *testing.T) {
	srv, _ := newTestServerWithGitCloneCapture()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/some-agent/start?projectId=..", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestCreateAgent_InvalidProjectIDRejected(t *testing.T) {
	srv, _ := newTestServerWithGitCloneCapture()

	body := `{"name": "some-agent", "projectId": ".."}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}
