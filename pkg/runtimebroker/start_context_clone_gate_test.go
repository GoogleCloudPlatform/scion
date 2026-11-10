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
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// recordProvisionSharedWorktree wraps provisionSharedWorktree (still the
// real provision.ProvisionShared underneath) and records every input and
// the error it returned.
func recordProvisionSharedWorktree(t *testing.T) (*[]provision.ProvisionInput, *[]error) {
	t.Helper()
	var inputs []provision.ProvisionInput
	var errs []error
	orig := provisionSharedWorktree
	provisionSharedWorktree = func(in provision.ProvisionInput) error {
		err := orig(in)
		inputs = append(inputs, in)
		errs = append(errs, err)
		return err
	}
	t.Cleanup(func() { provisionSharedWorktree = orig })
	return &inputs, &errs
}

// requireWorktreeEligible fails, rather than skips, on a host whose git is
// too old for worktree mode: these tests pin the broker's clone check and
// hardening, which must not pass silently by skipping.
func requireWorktreeEligible(t *testing.T) {
	t.Helper()
	if eligible, reason := runtime.WorktreeModeEligible(); !eligible {
		t.Fatalf("this test needs worktree mode (git 2.48 or later): %s", reason)
	}
}

// unregisteredBareRepo returns a bare repository path that is not a
// registered clone fixture.
func unregisteredBareRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "other.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s", out)
	}
	return bare
}

func tryProvisionWorktreeWithURL(t *testing.T, srv *Server, projectPath, cloneURL string) (bool, error) {
	t.Helper()
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-1", AgentID: "agent-1",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: cloneURL, Branch: "main"}},
	}, &api.StartOptions{}, map[string]string{}, srv.runtime.Name())
	return ok, err
}

// The broker's host-side shared-base clone is hardened and its URL is
// checked by provision.ProvisionShared: a local path, file://, http://, a
// <transport>::<address> URL and a value starting with "-" are refused
// before anything is cloned, the dispatch fails with that refusal (no
// fallback to the in-container clone), and every call is made with
// HostGitHardened set.
func TestTryProvisionWorktree_RefusesHostCloneURLs(t *testing.T) {
	requireWorktreeEligible(t)
	bare := unregisteredBareRepo(t)
	for name, cloneURL := range map[string]string{
		"local path":   bare,
		"file":         "file://" + bare,
		"http":         "http://git.example.com/org/repo.git",
		"transport":    "ext::sh -c touch% /tmp/x",
		"leading dash": "--upload-pack=touch /tmp/x",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)
			inputs, errs := recordProvisionSharedWorktree(t)
			projectPath := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatal(err)
			}

			ok, err := tryProvisionWorktreeWithURL(t, srv, projectPath, cloneURL)
			if ok || !errors.Is(err, provision.ErrHostCloneURLRefused) {
				t.Fatalf("tryProvisionWorktree: ok=%v err=%v, want the refusal (no clone-per-agent fallback)", ok, err)
			}
			if len(*inputs) != 1 {
				t.Fatalf("provisioning ran %d times, want 1", len(*inputs))
			}
			if !(*inputs)[0].HostGitHardened {
				t.Error("the broker's host-side clone must be hardened (HostGitHardened)")
			}
			if !errors.Is((*errs)[0], provision.ErrHostCloneURLRefused) {
				t.Errorf("provisioning error = %v, want the clone URL refused", (*errs)[0])
			}
			if _, statErr := os.Stat(filepath.Join(projectPath, "workspace", ".git")); !os.IsNotExist(statErr) {
				t.Errorf("nothing may be cloned for a refused URL (stat .git: %v)", statErr)
			}
		})
	}
}

// Pins HostGitHardened on the broker's provisioning input, both as built
// and as passed to provision.ProvisionShared on a successful start.
func TestTryProvisionWorktree_HostCloneHardened(t *testing.T) {
	requireWorktreeEligible(t)
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
		ProjectPath:   t.TempDir(),
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	})
	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision, reason: %s", result.Reason)
	}
	if !result.ProvisionInput.HostGitHardened {
		t.Error("resolveWorktreeProvision must set HostGitHardened")
	}

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	inputs, errs := recordProvisionSharedWorktree(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ok, err := tryProvisionWorktreeWithURL(t, srv, projectPath, initBareRepoWithCommit(t))
	if err != nil || !ok {
		t.Fatalf("tryProvisionWorktree with a registered fixture: ok=%v err=%v", ok, err)
	}
	if len(*inputs) != 1 || (*errs)[0] != nil {
		t.Fatalf("provisioning calls = %d, errors = %v", len(*inputs), *errs)
	}
	if !(*inputs)[0].HostGitHardened {
		t.Error("the broker's host-side clone must be hardened (HostGitHardened)")
	}
}

// Path-level guard of the up-front refusal on the worktree-per-agent route:
// buildStartContext, on create and on start, given a refused clone URL,
// fails with 422 and "worktree-per-agent: " plus the refusal's fixed text
// (naming no URL), builds no start context and never reaches the host-side
// provisioning (no fallback to the in-container clone either). The 422
// comes from the project clone URL check ahead of every route; if that
// check moved, this route would fail with 500 from tryProvisionWorktree's
// fail-closed error and this test would fail.
func TestBuildStartContext_RefusedCloneURLFailsWithoutFallback(t *testing.T) {
	requireWorktreeEligible(t)
	for _, op := range []startOperation{opCreate, opHTTPStart} {
		t.Run(string(op), func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)
			projectPath := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatal(err)
			}
			inputs, _ := recordProvisionSharedWorktree(t)
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-1", AgentID: "agent-1",
				ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: "http://git.example.com/org/repo.git", Branch: "main"}},
				Operation:     op,
			})
			if len(*inputs) != 0 {
				t.Errorf("host-side provisioning ran %d times for a refused URL, want 0", len(*inputs))
			}
			if sc != nil {
				t.Fatalf("a start context was built (in-container clone env %q, GitClone %+v), want none", sc.Opts.Env["SCION_GIT_CLONE_URL"], sc.Opts.GitClone)
			}
			var sce *startContextError
			if !errors.As(err, &sce) {
				t.Fatalf("buildStartContext error = %v (%T), want a *startContextError", err, err)
			}
			if sce.Status != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want %d", sce.Status, http.StatusUnprocessableEntity)
			}
			if want := "worktree-per-agent: " + provision.ErrHostCloneURLRefused.Error(); sce.Message != want {
				t.Errorf("message = %q, want %q", sce.Message, want)
			}
			if strings.Contains(sce.Message, "git.example.com") {
				t.Errorf("message %q names the refused URL", sce.Message)
			}
		})
	}
}
