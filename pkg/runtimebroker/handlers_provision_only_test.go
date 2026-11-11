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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestCreateAgentProvisionOnly(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "provisioned-agent",
		"id": "agent-uuid-456",
		"slug": "provisioned-agent",
		"provisionOnly": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Provision was called, not Start
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
	if mgr.startCalled {
		t.Error("expected Start NOT to be called for provision-only")
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Created {
		t.Error("expected Created to be true")
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be present")
	}

	// Agent status should be "created" (not "running")
	if resp.Agent.Status != string(state.PhaseCreated) {
		t.Errorf("expected status '%s', got '%s'", string(state.PhaseCreated), resp.Agent.Status)
	}

	// ID and slug should be passed through
	if resp.Agent.ID != "agent-uuid-456" {
		t.Errorf("expected ID 'agent-uuid-456', got '%s'", resp.Agent.ID)
	}
	if resp.Agent.Slug != "provisioned-agent" {
		t.Errorf("expected slug 'provisioned-agent', got '%s'", resp.Agent.Slug)
	}
}

// TestCreateAgentProvisionOnly_Reprovision_CallsReprovisionNotProvision is
// the design §3.4 Amendment A2.5 regression test: a
// provisionOnly+reprovision request must call Manager.Reprovision, never
// Manager.Provision, and the response must echo reprovisioned:true — the
// echo the hub's whole fail-closed dispatch design rests on
// (design §3.4 Amendment A2.2(a)).
func TestCreateAgentProvisionOnly_Reprovision_CallsReprovisionNotProvision(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "reprovisioned-agent",
		"id": "agent-uuid-reprov",
		"slug": "reprovisioned-agent",
		"provisionOnly": true,
		"reprovision": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled {
		t.Error("expected Reprovision to be called")
	}
	if mgr.provisionCalled {
		t.Error("expected Provision NOT to be called when reprovision=true")
	}
	if mgr.startCalled {
		t.Error("expected Start NOT to be called for provision-only")
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Reprovisioned {
		t.Error("expected Reprovisioned=true in the response")
	}
}

// TestCreateAgentProvisionOnly_SetsFreshProvision proves a plain
// provisionOnly create (no reprovision) sets opts.FreshProvision, so
// Manager.Provision -> GetAgent clears a leftover populated workspace from a
// same-named agent, extending GoogleCloudPlatform/scion#1931's create-only
// wipe gate to the ProvisionOnly path.
func TestCreateAgentProvisionOnly_SetsFreshProvision(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "provisioned-agent",
		"id": "agent-uuid-456",
		"slug": "provisioned-agent",
		"provisionOnly": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if !mgr.lastOpts.FreshProvision {
		t.Error("expected opts.FreshProvision to be true for a plain provisionOnly create")
	}
}

// TestCreateAgentProvisionOnly_Reprovision_NeverSetsFreshProvision proves a
// reprovision (reincarnation) create never sets opts.FreshProvision, even
// though it is still an opCreate dispatch: reincarnation targets an existing
// agent's workspace, and FreshProvision would let GetAgent wipe it.
func TestCreateAgentProvisionOnly_Reprovision_NeverSetsFreshProvision(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "reprovisioned-agent",
		"id": "agent-uuid-reprov",
		"slug": "reprovisioned-agent",
		"provisionOnly": true,
		"reprovision": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if mgr.lastOpts.FreshProvision {
		t.Error("expected opts.FreshProvision to be false for a reprovision create")
	}
}

// TestCreateAgentProvisionOnly_PlainProvision_DoesNotEchoReprovisioned is the
// reverse of the above: a plain provisionOnly request (no reprovision) must
// call Manager.Provision, never Manager.Reprovision, and must NOT echo
// reprovisioned:true — a broker that always echoed true regardless of which
// branch ran would defeat the hub's mandatory-echo fail-closed check.
func TestCreateAgentProvisionOnly_PlainProvision_DoesNotEchoReprovisioned(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "plain-provisioned-agent",
		"id": "agent-uuid-plain",
		"slug": "plain-provisioned-agent",
		"provisionOnly": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
	if mgr.reprovisionCalled {
		t.Error("expected Reprovision NOT to be called when reprovision is unset")
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Reprovisioned {
		t.Error("expected Reprovisioned to be false/absent for a plain provision")
	}
}

// TestCreateAgentProvisionOnly_ReprovisionError_ReturnsErrorNoEcho covers a
// plain (unwrapped, not agent.ErrReprovisionRefused) Reprovision failure: the
// handler must return the generic 500 — not the 409 the sentinel-wrapped
// path gets — and must not echo reprovisioned:true for a request that never
// actually succeeded. The body must carry the fixed, identity-free
// "Failed to provision agent" message, never Reprovision's own raw error
// text, which could carry a runtime-specific detail this response must not
// disclose.
func TestCreateAgentProvisionOnly_ReprovisionError_ReturnsErrorNoEcho(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	mgr.reprovisionErr = errors.New("boom: transient broker failure")

	body := `{
		"name": "reprovision-fail-agent",
		"id": "agent-uuid-reprov-fail",
		"slug": "reprovision-fail-agent",
		"provisionOnly": true,
		"reprovision": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for a plain (non-refusal) Reprovision error, got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled {
		t.Error("expected Reprovision to have been attempted")
	}
	if strings.Contains(w.Body.String(), "boom: transient broker failure") {
		t.Errorf("the error body must never surface Reprovision's own raw error text, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to provision agent") {
		t.Errorf("expected the fixed, identity-free provision-failure message, got: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"reprovisioned":true`) {
		t.Errorf("a failed Reprovision must never echo reprovisioned:true, got: %s", w.Body.String())
	}
}

// TestCreateAgentProvisionOnly_ReprovisionRefused_Returns409 covers design
// §3.4 Amendment A4.2: a Reprovision failure that wraps agent.ErrReprovisionRefused
// (workspace preconditions, running-container check) must surface as 409
// Conflict specifically, not a generic 500 — so the reincarnate worker's
// failure message and any future caller-side retry logic can tell "refused
// to run" apart from an actual provisioning error.
func TestCreateAgentProvisionOnly_ReprovisionRefused_Returns409(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	mgr.reprovisionErr = fmt.Errorf("%w: agent %q container is still running; stop it first", agent.ErrReprovisionRefused, "reprovision-409-agent")

	body := `{
		"name": "reprovision-409-agent",
		"id": "agent-uuid-reprov-409",
		"slug": "reprovision-409-agent",
		"provisionOnly": true,
		"reprovision": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for a refused reprovision, got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled {
		t.Error("expected Reprovision to have been attempted")
	}
	if !strings.Contains(w.Body.String(), "reprovision refused") {
		t.Errorf("expected the error body to surface the refusal reason, got: %s", w.Body.String())
	}
}

// snapshotFileTimes walks dir and records the ModTime of every regular file
// found under it, keyed by path relative to dir. Used to prove that NO
// write happened anywhere under a directory tree across an operation —
// including a content-identical rewrite (e.g. RegisterSharer's atomic
// temp-file-plus-rename always changes ModTime, even when the marshaled
// JSON is byte-identical to what was already there), which a before/after
// content comparison alone would miss.
func snapshotFileTimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		out[rel] = info.ModTime()
		return nil
	})
	return out
}

// TestCreateAgentProvisionOnly_Reprovision_WorktreePerAgent_Returns409 is the
// design §3.4 Amendment A23.1/A23.2 (review p1b-r1 R2, p1b-r2 R2-T)
// regression test: a reprovision request for a worktree-per-agent project
// must be refused with 409 before buildStartContext ever runs —
// buildStartContext's tryProvisionWorktree finds or creates the agent's
// worktree and hands Manager.Reprovision an explicit-mount-shaped
// StartOptions (GitClone=nil, Workspace=<worktree path>), a mode reincarnate
// has neither designed for nor reviewed, and whose OWN failure path can
// `git worktree remove --force` / os.RemoveAll the agent's LIVE worktree
// (round-1 review FYI-2) if ProvisionShared ever errors on it.
//
// The fixture is realistic, not a stand-in: a real git-backed project
// (config.gitClone set, matching what populateAgentConfig always sends for a
// worktree-per-agent agent), a real ProjectPath, and the agent's own base
// clone + worktree ALREADY provisioned on this broker — exactly the state a
// second reprovision of a running agent finds. Manager.Reprovision/Provision
// must never be called, the pre-seeded sentinel file in the agent's worktree
// must survive untouched, and — the property a placement-blind assertion on
// the manager mock alone cannot prove — NOTHING under the base clone
// (including worktrees/ and the .git/ sharer registry) may be written to at
// all: buildStartContext, and therefore tryProvisionWorktree, must never
// run. A refusal placed anywhere AFTER buildStartContext (e.g. immediately
// before the Manager.Reprovision call) would let tryProvisionWorktree run
// its (safe, in this fixture) JOIN path, which still rewrites the sharer
// registry file and would be caught by the whole-tree ModTime snapshot even
// though Manager.Reprovision itself is never reached in that JOIN case
// either.
func TestCreateAgentProvisionOnly_Reprovision_WorktreePerAgent_Returns409(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	srv, mgr := newTestServerWithProvisionCapture()

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}
	agentID := "worktree-reprov-agent"

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-seed the agent's own base clone + worktree, as an earlier
	// successful provision on this broker would have left them.
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: agentID,
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: agentID, AgentName: agentID, GitClone: gc,
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	base := resolved.HostPath
	worktreePath := provision.WorktreePath(base, agentID)
	sentinelPath := filepath.Join(worktreePath, "uncommitted-work.txt")
	if err := os.WriteFile(sentinelPath, []byte("hours of agent work"), 0644); err != nil {
		t.Fatal(err)
	}

	beforeTimes := snapshotFileTimes(t, base)

	reqBody, err := json.Marshal(CreateAgentRequest{
		Name:          agentID,
		ID:            "agent-uuid-worktree-reprov",
		Slug:          agentID,
		ProjectID:     "p1",
		ProjectPath:   projectPath,
		ProvisionOnly: true,
		Reprovision:   true,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config: &CreateAgentConfig{
			Template: "claude",
			GitClone: gc,
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for a worktree-per-agent reprovision, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.reprovisionCalled {
		t.Error("expected Manager.Reprovision NOT to be called: the refusal must happen before buildStartContext, which is what would otherwise call it")
	}
	if mgr.provisionCalled {
		t.Error("expected Manager.Provision NOT to be called either")
	}
	if !strings.Contains(w.Body.String(), "worktree-per-agent") {
		t.Errorf("expected the error body to name the worktree-per-agent refusal, got: %s", w.Body.String())
	}

	data, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatalf("sentinel file in the agent's live worktree was lost: %v", err)
	}
	if string(data) != "hours of agent work" {
		t.Fatalf("sentinel content changed: %q", data)
	}

	afterTimes := snapshotFileTimes(t, base)
	if !reflect.DeepEqual(beforeTimes, afterTimes) {
		t.Fatalf("base clone / worktree tree was written to -- buildStartContext (and therefore tryProvisionWorktree) must never run before the refusal:\nbefore=%v\nafter=%v", beforeTimes, afterTimes)
	}
}

// TestCreateAgentProvisionOnly_Reprovision_SharedWorkspace_StillCallsReprovision
// is the companion negative control for the R2 fix above: a reprovision
// request for a shared-workspace (not worktree-per-agent) project must still
// reach Manager.Reprovision as normal — the new gate must not over-refuse
// every WorkspaceMode.
func TestCreateAgentProvisionOnly_Reprovision_SharedWorkspace_StillCallsReprovision(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "shared-reprov-agent",
		"id": "agent-uuid-shared-reprov",
		"slug": "shared-reprov-agent",
		"provisionOnly": true,
		"reprovision": true,
		"workspaceMode": "shared",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a shared-workspace reprovision, got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled {
		t.Error("expected Manager.Reprovision to be called for a shared-workspace reprovision")
	}
}

func TestCreateAgentProvisionOnlyHarnessConfig(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()

	body := `{
		"name": "harness-agent",
		"id": "agent-uuid-hc",
		"slug": "harness-agent",
		"provisionOnly": true,
		"config": {"template": "claude", "harness": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be present")
	}

	// HarnessConfig should be populated from Provision's ScionConfig
	if resp.Agent.HarnessConfig != "claude" {
		t.Errorf("expected HarnessConfig 'claude', got '%s'", resp.Agent.HarnessConfig)
	}

	// Template should NOT be overwritten with the harness name
	if resp.Agent.Template == "claude" {
		t.Error("Template should not be overwritten with harness name")
	}
}

func TestCreateAgentFullStart(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "running-agent",
		"config": {"template": "claude", "task": "do something"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Start was called, not Provision
	if mgr.provisionCalled {
		t.Error("expected Provision NOT to be called for full start")
	}
	if !mgr.startCalled {
		t.Error("expected Start to be called")
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be present")
	}

	// Agent status should not be "created" since it was fully started
	if resp.Agent.Status == string(state.PhaseCreated) {
		t.Error("expected status to NOT be 'created' for fully started agent")
	}
}

func TestCreateAgentProvisionOnlyWithTask(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "agent-with-task",
		"id": "agent-uuid-789",
		"slug": "agent-with-task",
		"provisionOnly": true,
		"config": {"template": "claude", "task": "implement feature X"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Provision was called, not Start
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
	if mgr.startCalled {
		t.Error("expected Start NOT to be called for provision-only with task")
	}

	// Verify the task was passed through to the Provision options
	if mgr.lastOpts.Task != "implement feature X" {
		t.Errorf("expected task 'implement feature X', got '%s'", mgr.lastOpts.Task)
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be present")
	}

	if resp.Agent.Status != string(state.PhaseCreated) {
		t.Errorf("expected status '%s', got '%s'", string(state.PhaseCreated), resp.Agent.Status)
	}
}
