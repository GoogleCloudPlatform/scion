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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// gitCloneCapturingManager captures env and GitClone from Start options.
type gitCloneCapturingManager struct {
	mockManager
	lastEnv            map[string]string
	lastGitClone       *api.GitCloneConfig
	lastWorkspace      string
	lastProjectPath    string
	lastBranch         string
	lastFreshProvision bool
	// lastSharedWorkspace and lastSharedWorkspaceClone capture the shared
	// workspace inputs (shared_workspace_clone_test.go).
	lastSharedWorkspace      bool
	lastSharedWorkspaceClone *api.GitCloneConfig
}

func (m *gitCloneCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.lastEnv = opts.Env
	m.lastGitClone = opts.GitClone
	m.lastWorkspace = opts.Workspace
	m.lastProjectPath = opts.ProjectPath
	m.lastBranch = opts.Branch
	m.lastFreshProvision = opts.FreshProvision
	m.lastSharedWorkspace = opts.SharedWorkspace
	m.lastSharedWorkspaceClone = opts.SharedWorkspaceClone
	return m.mockManager.Start(ctx, opts)
}

func newTestServerWithGitCloneCapture() (*Server, *gitCloneCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &gitCloneCapturingManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt, config.RuntimeBuildRecord{}), mgr
}

func TestCreateAgentWithGitClone(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"name": "git-clone-agent",
		"config": {
			"template": "claude",
			"gitClone": {
				"url": "https://github.com/example/repo.git",
				"branch": "develop",
				"depth": 1
			}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify git clone env vars were injected
	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	if got := mgr.lastEnv["SCION_GIT_CLONE_URL"]; got != "https://github.com/example/repo.git" {
		t.Errorf("expected SCION_GIT_CLONE_URL='https://github.com/example/repo.git', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_BRANCH"]; got != "develop" {
		t.Errorf("expected SCION_GIT_BRANCH='develop', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_DEPTH"]; got != "1" {
		t.Errorf("expected SCION_GIT_DEPTH='1', got %q", got)
	}

	// Verify workspace and projectPath were cleared
	if mgr.lastWorkspace != "" {
		t.Errorf("expected workspace to be empty in git clone mode, got '%s'", mgr.lastWorkspace)
	}
	if mgr.lastProjectPath != "" {
		t.Errorf("expected projectPath to be empty in git clone mode, got '%s'", mgr.lastProjectPath)
	}

	// Verify GitClone was passed through
	if mgr.lastGitClone == nil {
		t.Fatal("expected GitClone to be set in StartOptions")
	}
	if mgr.lastGitClone.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected GitClone.URL 'https://github.com/example/repo.git', got '%s'", mgr.lastGitClone.URL)
	}
}

func TestCreateAgentWithGitCloneAndBranch(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"name": "branch-agent",
		"config": {
			"template": "claude",
			"branch": "my-feature",
			"gitClone": {
				"url": "https://github.com/example/repo.git",
				"branch": "main",
				"depth": 1
			}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}
	if got := mgr.lastEnv["SCION_AGENT_BRANCH"]; got != "my-feature" {
		t.Errorf("expected SCION_AGENT_BRANCH='my-feature', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_BRANCH"]; got != "main" {
		t.Errorf("expected SCION_GIT_BRANCH='main', got %q", got)
	}
}

func TestCreateAgentWithoutGitClone(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"name": "regular-agent",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify no git clone env vars are set
	if mgr.lastEnv != nil {
		if _, exists := mgr.lastEnv["SCION_GIT_CLONE_URL"]; exists {
			t.Error("expected SCION_GIT_CLONE_URL to NOT be set for regular agent")
		}
	}

	// Verify GitClone is nil
	if mgr.lastGitClone != nil {
		t.Error("expected GitClone to be nil for regular agent")
	}
}

// TestStartAgentWithGitCloneOnly proves the start handler decodes a
// top-level gitClone field the same way create's config.gitClone is decoded
// (GoogleCloudPlatform/scion#1931), so a workspace that did not survive a
// stop can be recreated on start.
func TestStartAgentWithGitCloneOnly(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"gitClone": {
			"url": "https://github.com/example/repo.git",
			"branch": "develop",
			"depth": 1
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/git-clone-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}
	if got := mgr.lastEnv["SCION_GIT_CLONE_URL"]; got != "https://github.com/example/repo.git" {
		t.Errorf("expected SCION_GIT_CLONE_URL='https://github.com/example/repo.git', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_BRANCH"]; got != "develop" {
		t.Errorf("expected SCION_GIT_BRANCH='develop', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_DEPTH"]; got != "1" {
		t.Errorf("expected SCION_GIT_DEPTH='1', got %q", got)
	}
	if _, ok := mgr.lastEnv["SCION_AGENT_BRANCH"]; ok {
		t.Errorf("expected SCION_AGENT_BRANCH to be unset when no top-level branch is sent, got %q", mgr.lastEnv["SCION_AGENT_BRANCH"])
	}
	if mgr.lastGitClone == nil || mgr.lastGitClone.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected GitClone to be passed through to StartOptions, got %+v", mgr.lastGitClone)
	}
	// A start dispatch must never set FreshProvision: GitClone being present
	// means the workspace may need recreating on a runtime that dropped it,
	// not that a same-named leftover should be wiped (GoogleCloudPlatform/scion#1931).
	if mgr.lastFreshProvision {
		t.Error("expected opts.FreshProvision=false for a start dispatch, got true")
	}
}

// TestStartAgentWithGitCloneAndBranch mirrors TestCreateAgentWithGitCloneAndBranch
// for the start path: the top-level branch (agent's checkout branch) and the
// gitClone's own branch (the clone source ref) are independent.
func TestStartAgentWithGitCloneAndBranch(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"branch": "my-feature",
		"gitClone": {
			"url": "https://github.com/example/repo.git",
			"branch": "main",
			"depth": 1
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/branch-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}
	if got := mgr.lastEnv["SCION_AGENT_BRANCH"]; got != "my-feature" {
		t.Errorf("expected SCION_AGENT_BRANCH='my-feature', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_BRANCH"]; got != "main" {
		t.Errorf("expected SCION_GIT_BRANCH='main', got %q", got)
	}
}

// TestStartAgentOldHubPayloadHasNoWorkspaceFields proves a start request
// without gitClone/branch/workspaceMode (an older Hub, or a non-git project)
// behaves exactly as it did before those fields existed: no git-clone env is
// injected and the request still succeeds.
func TestStartAgentOldHubPayloadHasNoWorkspaceFields(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"resolvedEnv": {"SCION_HUB_ENDPOINT": "https://hub.example.com"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/old-payload-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastGitClone != nil {
		t.Errorf("expected GitClone to be nil for an old-shape payload, got %+v", mgr.lastGitClone)
	}
	if mgr.lastBranch != "" {
		t.Errorf("expected opts.Branch to be empty for an old-shape payload, got %q", mgr.lastBranch)
	}
	// SCION_WORKSPACE_MODE is excluded: it always defaults to shared-plain
	// when no workspace mode is supplied, independent of gitClone/branch.
	for _, key := range []string{"SCION_GIT_CLONE_URL", "SCION_GIT_BRANCH", "SCION_GIT_DEPTH", "SCION_AGENT_BRANCH"} {
		if _, ok := mgr.lastEnv[key]; ok {
			t.Errorf("expected %s to be unset for an old-shape payload, got %q", key, mgr.lastEnv[key])
		}
	}
}

// TestStartAgentIgnoresUnknownJSONKey proves an unrecognized top-level key in
// the start request body (e.g. from a newer Hub sending a field this broker
// version doesn't know about yet) does not break decoding of the fields this
// broker does recognize. The unknown key is placed BEFORE the recognized
// fields in the JSON object. The assertions check that gitClone and
// workspaceMode — sent in the same payload — were actually applied, not
// just that the response status was 202: a vacuous assertion (e.g. only
// checking GitClone is nil) would pass even if decoding silently stopped
// after the unknown key.
func TestStartAgentIgnoresUnknownJSONKey(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"someFutureField": {"nested": "value"},
		"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main"},
		"workspaceMode": "clone-per-agent"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/unknown-key-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastGitClone == nil || mgr.lastGitClone.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected GitClone to be applied despite the preceding unknown key, got %+v", mgr.lastGitClone)
	}
	if got := mgr.lastEnv["SCION_WORKSPACE_MODE"]; got != "clone-per-agent" {
		t.Errorf("expected SCION_WORKSPACE_MODE='clone-per-agent' despite the preceding unknown key, got %q", got)
	}
}

// TestStartAgentBranchOnlyPropagatesToOpts proves a start request carrying
// only a top-level branch (no gitClone) still reaches opts.Branch: the
// broker-side cfg-building condition includes startReq.Branch != "" as one
// of its triggers, not only GitClone.
func TestStartAgentBranchOnlyPropagatesToOpts(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{"branch": "my-feature"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/branch-only-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastBranch != "my-feature" {
		t.Errorf("expected opts.Branch='my-feature', got %q", mgr.lastBranch)
	}
	if mgr.lastGitClone != nil {
		t.Errorf("expected GitClone to remain nil for a branch-only start, got %+v", mgr.lastGitClone)
	}
}

// TestStartAndCreate_GitWorkspaceEnvParity proves create and start inject the
// identical set of SCION_GIT_*, SCION_AGENT_BRANCH, and SCION_WORKSPACE_*
// keys and values for equivalent GitClone/Branch/WorkspaceMode inputs
// (GoogleCloudPlatform/scion#1931) — the two paths must not drift.
func TestStartAndCreate_GitWorkspaceEnvParity(t *testing.T) {
	const parityKeys = "SCION_GIT_CLONE_URL,SCION_GIT_BRANCH,SCION_GIT_DEPTH,SCION_AGENT_BRANCH,SCION_WORKSPACE_MODE,SCION_WORKSPACE_GIT"
	keys := strings.Split(parityKeys, ",")

	createSrv, createMgr := newTestServerWithGitCloneCapture()
	createBody := `{
		"name": "parity-agent",
		"workspaceMode": "clone-per-agent",
		"config": {
			"template": "claude",
			"branch": "my-feature",
			"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main", "depth": 1}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	createSrv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	startSrv, startMgr := newTestServerWithGitCloneCapture()
	startBody := `{
		"branch": "my-feature",
		"workspaceMode": "clone-per-agent",
		"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main", "depth": 1}
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/parity-agent/start", strings.NewReader(startBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	startSrv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	for _, key := range keys {
		createVal, createOK := createMgr.lastEnv[key]
		startVal, startOK := startMgr.lastEnv[key]
		if createOK != startOK || createVal != startVal {
			t.Errorf("%s: create=%q(present=%v) start=%q(present=%v), want identical", key, createVal, createOK, startVal, startOK)
		}
	}

	// FreshProvision is the one input that must NOT be at parity: create
	// dispatches it true (safe to wipe a same-named leftover), start leaves
	// it false (must preserve a workspace that survived a stop).
	if !createMgr.lastFreshProvision {
		t.Error("expected opts.FreshProvision=true for a create dispatch, got false")
	}
	if startMgr.lastFreshProvision {
		t.Error("expected opts.FreshProvision=false for a start dispatch, got true")
	}
}
