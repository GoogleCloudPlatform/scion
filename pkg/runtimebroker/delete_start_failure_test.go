// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtimebroker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Both failed-start cleanups (createAgent and cleanupLaunchFiles) clean up
// through agent.DeleteStartFailureFiles: a start that checked out an
// existing branch keeps that branch and its commits.

func startFailureGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// existingBranchWithCommit clones the shared base at base (provisioning an
// unrelated agent) and creates branch with a commit of its own, checked
// out nowhere. It returns the branch tip.
func existingBranchWithCommit(t *testing.T, base, branch string, gc *api.GitCloneConfig) string {
	t.Helper()
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved:  provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
		Mode:      store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "seed-id", AgentName: "seed", GitClone: gc,
	}); err != nil {
		t.Fatalf("provision seed: %v", err)
	}
	tree := startFailureGitOut(t, base, "rev-parse", "HEAD^{tree}")
	tip := startFailureGitOut(t, base, "commit-tree", tree, "-p", "HEAD", "-m", "work")
	startFailureGitOut(t, base, "branch", branch, tip)
	return tip
}

func startFailureBranchTip(t *testing.T, base, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", base, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// A failed synchronous create that checked out an existing branch removes
// its worktree and keeps the branch.
func TestSyncCreateFailure_KeepsExistingBranch(t *testing.T) {
	requireWorktreeGit(t)
	t.Setenv("SCION_HOST_UID", "")
	srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)
	base := filepath.Join(filepath.Dir(projectPath), "workspace")
	writeAgentFiles(t, agentDir, "provisioned")
	gc := &api.GitCloneConfig{URL: initBareRepoWithCommit(t), Branch: "main"}
	tip := existingBranchWithCommit(t, base, "same-name", gc)
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved:  provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
		Mode:      store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a-id", AgentName: "same-name", GitClone: gc,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	wt := provision.WorktreePath(base, "agent-a-id")
	if got := startFailureGitOut(t, wt, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("setup: the worktree is at %s, want the existing branch tip %s", got, tip)
	}

	var started atomic.Bool
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		started.Store(true)
		return nil, errors.New("container start failed")
	}
	body := fmt.Sprintf(`{"id":"agent-a-id","name":"same-name","projectPath":%q,"config":{"task":"t"}}`, projectPath)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code < 400 {
		t.Fatalf("expected the create to fail, got %d: %s", w.Code, w.Body.String())
	}
	if !started.Load() {
		t.Fatal("the create did not reach Manager.Start; the test does not exercise the failure cleanup")
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Errorf("the failure cleanup left the worktree: %v", err)
	}
	if got := startFailureBranchTip(t, base, "same-name"); got != tip {
		t.Errorf("branch same-name = %q after the failure cleanup, want kept at %s", got, tip)
	}
}

// An aborted async create launch that checked out an existing branch
// removes its worktree and keeps the branch.
func TestCleanupLaunchFiles_KeepsExistingBranch(t *testing.T) {
	requireWorktreeGit(t)
	t.Setenv("SCION_HOST_UID", "")
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentDir := filepath.Join(projectDir, "agents", "agent-x")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(filepath.Dir(projectDir), "workspace")
	gc := &api.GitCloneConfig{URL: initBareRepoWithCommit(t), Branch: "main"}
	tip := existingBranchWithCommit(t, base, "agent-x", gc)
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved:  provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
		Mode:      store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-id-x", AgentName: "agent-x", GitClone: gc,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	wt := provision.WorktreePath(base, "agent-id-x")
	if got := startFailureGitOut(t, wt, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("setup: the worktree is at %s, want the existing branch tip %s", got, tip)
	}
	if err := writeLaunchMarker(projectDir, false, "agent-x", "L1"); err != nil {
		t.Fatal(err)
	}
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	rec := newLaunchRecord("L1", "agent-id-x", store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})

	if err := srv.cleanupLaunchFiles(rec, api.StartOptions{Name: "agent-x", ProjectPath: projectDir, RunID: "run-a"}, false, "agent-x"); err != nil {
		t.Fatalf("cleanupLaunchFiles: %v", err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Errorf("the launch cleanup left the worktree: %v", err)
	}
	if got := startFailureBranchTip(t, base, "agent-x"); got != tip {
		t.Errorf("branch agent-x = %q after the launch cleanup, want kept at %s", got, tip)
	}
}
