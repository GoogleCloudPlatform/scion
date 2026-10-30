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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const workspaceGitProjectID = "1a2b3c4d-0000-4000-8000-00000000c0de"

// workspaceGitFixture is a start-context server on a shared NFS Layout and
// the project's shared workspace directory on it (not created).
type workspaceGitFixture struct {
	srv  *Server
	leaf string
}

func newWorkspaceGitFixture(t *testing.T) *workspaceGitFixture {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	hostBase := filepath.Join(t.TempDir(), "ws1")
	cfg.StorageLayout = projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: hostBase, SubPathRoot: "projects", Share: projectfs.Share{ID: "ws1"}}
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	return &workspaceGitFixture{srv: srv, leaf: filepath.Join(hostBase, "projects", workspaceGitProjectID, "workspace")}
}

func (f *workspaceGitFixture) mkLeaf(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(f.leaf, 0o755); err != nil {
		t.Fatal(err)
	}
}

func gitInitDir(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Skipf("git init failed: %s", out)
	}
}

func (f *workspaceGitFixture) start(t *testing.T, in startContextInputs) map[string]string {
	t.Helper()
	in.Name = "workspace-git-agent"
	in.ProjectID = workspaceGitProjectID
	in.HTTPRequest = httptest.NewRequest("POST", "/api/v1/agents", nil)
	if in.Operation == "" {
		in.Operation = opCreate
	}
	sc, err := f.srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	return sc.Opts.Env
}

// The decision itself: only broker and hub facts, and the workspace
// directory as it is now.
func TestSharedWorkspaceKnownNotGit(t *testing.T) {
	shared := &api.GitCloneConfig{URL: "https://example.com/org/repo.git", Branch: "main"}
	cases := []struct {
		name    string
		prepare func(f *workspaceGitFixture, t *testing.T)
		in      startContextInputs
		want    bool
	}{
		{name: "hub-native project, plain workspace directory", prepare: (*workspaceGitFixture).mkLeaf, want: true},
		{name: "hub-native project, explicit shared-plain mode", prepare: (*workspaceGitFixture).mkLeaf,
			in: startContextInputs{WorkspaceMode: "shared-plain"}, want: true},
		{name: "workspace directory does not exist yet (undeterminable)"},
		{name: "workspace directory is a git repository", prepare: func(f *workspaceGitFixture, t *testing.T) {
			f.mkLeaf(t)
			gitInitDir(t, f.leaf)
		}},
		{name: "workspace directory has a .git file", prepare: func(f *workspaceGitFixture, t *testing.T) {
			f.mkLeaf(t)
			if err := os.WriteFile(filepath.Join(f.leaf, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "workspace directory is a symlink", prepare: func(f *workspaceGitFixture, t *testing.T) {
			target := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(f.leaf), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, f.leaf); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hub sent shared-workspace clone settings (git remote)", prepare: (*workspaceGitFixture).mkLeaf,
			in: startContextInputs{Config: &CreateAgentConfig{SharedWorkspace: true, SharedWorkspaceClone: shared}}},
		{name: "hub sent the shared-workspace flag (git remote)", prepare: (*workspaceGitFixture).mkLeaf,
			in: startContextInputs{Config: &CreateAgentConfig{SharedWorkspace: true}}},
		{name: "per-agent git clone", prepare: (*workspaceGitFixture).mkLeaf,
			in: startContextInputs{Config: &CreateAgentConfig{GitClone: shared}}},
		{name: "worktree-per-agent mode", prepare: (*workspaceGitFixture).mkLeaf,
			in: startContextInputs{WorkspaceMode: store.WorkspaceModeWorktreePerAgent}},
		{name: "clone-per-agent mode", prepare: (*workspaceGitFixture).mkLeaf,
			in: startContextInputs{WorkspaceMode: "clone-per-agent"}},
		{name: "no project ID", prepare: (*workspaceGitFixture).mkLeaf, in: startContextInputs{ProjectID: "-"}},
		// A restart from an older hub builds no Config and carries the flag
		// only at the top level (restartAgentConfig).
		{name: "top-level SharedWorkspace with no Config (git remote)", prepare: (*workspaceGitFixture).mkLeaf,
			in: startContextInputs{SharedWorkspace: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkspaceGitFixture(t)
			if tc.prepare != nil {
				tc.prepare(f, t)
			}
			in := tc.in
			switch in.ProjectID {
			case "-":
				in.ProjectID = ""
			default:
				in.ProjectID = workspaceGitProjectID
			}
			opts := &api.StartOptions{}
			if got := f.srv.sharedWorkspaceKnownNotGit(context.Background(), in, opts, false, false); got != tc.want {
				t.Fatalf("sharedWorkspaceKnownNotGit = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("worktree provisioned or linked checkout", func(t *testing.T) {
		f := newWorkspaceGitFixture(t)
		f.mkLeaf(t)
		in := startContextInputs{ProjectID: workspaceGitProjectID}
		if f.srv.sharedWorkspaceKnownNotGit(context.Background(), in, &api.StartOptions{}, true, false) ||
			f.srv.sharedWorkspaceKnownNotGit(context.Background(), in, &api.StartOptions{}, false, true) {
			t.Fatal("a provisioned worktree or a linked checkout must never be reported non-git")
		}
	})
	t.Run("non-shared Layout", func(t *testing.T) {
		f := newWorkspaceGitFixture(t)
		f.mkLeaf(t)
		f.srv.config.StorageLayout = projectfs.Layout{}
		if f.srv.sharedWorkspaceKnownNotGit(context.Background(), startContextInputs{ProjectID: workspaceGitProjectID}, &api.StartOptions{}, false, false) {
			t.Fatal("a non-shared Layout must never be reported non-git")
		}
	})
}

// The env a Kubernetes start gets. SCION_WORKSPACE_GIT=false (which the
// Kubernetes shared-repository guard admits) only for a hub-native project
// whose shared workspace is not a git repository; a git shared checkout and
// an undeterminable workspace leave it absent (the guard refuses). A value
// from the hub's resolved env (project env) or the request's env never
// survives: the broker's computed value replaces it.
func TestBuildStartContext_WorkspaceGitOnSharedLayout(t *testing.T) {
	gitShared := &CreateAgentConfig{SharedWorkspace: true, SharedWorkspaceClone: &api.GitCloneConfig{URL: "https://example.com/org/repo.git", Branch: "main"}}
	t.Run("hub-native project: explicit false", func(t *testing.T) {
		f := newWorkspaceGitFixture(t)
		f.mkLeaf(t)
		env := f.start(t, startContextInputs{})
		if v, ok := env["SCION_WORKSPACE_GIT"]; !ok || v != "false" {
			t.Fatalf("SCION_WORKSPACE_GIT = %q (present %v), want \"false\"", v, ok)
		}
	})
	t.Run("git shared checkout: absent", func(t *testing.T) {
		f := newWorkspaceGitFixture(t)
		f.mkLeaf(t)
		gitInitDir(t, f.leaf)
		env := f.start(t, startContextInputs{Config: gitShared})
		if v, ok := env["SCION_WORKSPACE_GIT"]; ok && v != "true" {
			t.Fatalf("SCION_WORKSPACE_GIT = %q, want absent or true for a git shared checkout", v)
		}
	})
	// Mutant check target: without the strip in buildStartContext, the
	// project-level "false" survives and this test fails.
	for _, src := range []string{"resolved env (project env)", "request env"} {
		t.Run("git shared checkout with a project-level false from the "+src+": not false", func(t *testing.T) {
			f := newWorkspaceGitFixture(t)
			f.mkLeaf(t) // no .git yet: the first agent's init container clones it
			cfg := *gitShared
			in := startContextInputs{Config: &cfg}
			if src == "request env" {
				cfg.Env = []string{"SCION_WORKSPACE_GIT=false"}
			} else {
				in.ResolvedEnv = map[string]string{"SCION_WORKSPACE_GIT": "false"}
			}
			env := f.start(t, in)
			if v, ok := env["SCION_WORKSPACE_GIT"]; ok {
				t.Fatalf("SCION_WORKSPACE_GIT = %q, want absent (the project-level value must be replaced)", v)
			}
		})
	}
	t.Run("hub-native project, git cannot be run: absent", func(t *testing.T) {
		stubWorkspaceGitProbe(t, func(context.Context, string) (string, error) {
			return "", &exec.Error{Name: "git", Err: exec.ErrNotFound}
		})
		f := newWorkspaceGitFixture(t)
		f.mkLeaf(t)
		env := f.start(t, startContextInputs{})
		if v, ok := env["SCION_WORKSPACE_GIT"]; ok {
			t.Fatalf("SCION_WORKSPACE_GIT = %q, want absent when git cannot be run", v)
		}
	})
	t.Run("hub-native project, workspace directory not there yet: absent", func(t *testing.T) {
		f := newWorkspaceGitFixture(t)
		env := f.start(t, startContextInputs{ResolvedEnv: map[string]string{"SCION_WORKSPACE_GIT": "false"}})
		if v, ok := env["SCION_WORKSPACE_GIT"]; ok {
			t.Fatalf("SCION_WORKSPACE_GIT = %q, want absent when the workspace cannot be checked", v)
		}
	})
}

// stubWorkspaceGitProbe replaces workspaceGitProbe for the rest of the test.
func stubWorkspaceGitProbe(t *testing.T, probe func(context.Context, string) (string, error)) {
	t.Helper()
	prev := workspaceGitProbe
	workspaceGitProbe = probe
	t.Cleanup(func() { workspaceGitProbe = prev })
}

// Only a clean "not a git repository" failure of git reads as "not git";
// every other outcome is unknown.
func TestGitReportsNotARepository(t *testing.T) {
	plain := t.TempDir()
	t.Run("missing git binary: unknown", func(t *testing.T) {
		stubWorkspaceGitProbe(t, func(context.Context, string) (string, error) {
			return "", &exec.Error{Name: "git", Err: exec.ErrNotFound}
		})
		if gitReportsNotARepository(context.Background(), plain) {
			t.Fatal("an exec error must be unknown, not \"not git\"")
		}
		if workspaceDirKnownNotGit(context.Background(), plain) {
			t.Fatal("workspaceDirKnownNotGit must be false when git cannot be run")
		}
	})
	t.Run("IO error with the message: unknown", func(t *testing.T) {
		stubWorkspaceGitProbe(t, func(context.Context, string) (string, error) {
			return "fatal: not a git repository (or any of the parent directories): .git", errors.New("read |0: input/output error")
		})
		if gitReportsNotARepository(context.Background(), plain) {
			t.Fatal("an error that is not a clean git exit must be unknown")
		}
	})
	t.Run("timeout: unknown", func(t *testing.T) {
		prev := workspaceGitTimeout
		workspaceGitTimeout = 20 * time.Millisecond
		t.Cleanup(func() { workspaceGitTimeout = prev })
		stubWorkspaceGitProbe(t, func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "fatal: not a git repository", ctx.Err()
		})
		start := time.Now()
		if gitReportsNotARepository(context.Background(), plain) {
			t.Fatal("a timed-out git call must be unknown")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("the git call was not bounded: took %s", d)
		}
	})
	t.Run("real git", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not installed")
		}
		if !gitReportsNotARepository(context.Background(), plain) {
			t.Fatal("a plain directory must be reported as not a git repository")
		}
		repo := t.TempDir()
		gitInitDir(t, repo)
		if gitReportsNotARepository(context.Background(), repo) {
			t.Fatal("a git work tree must not be reported as not a git repository")
		}
	})
}

// shProbe returns a workspaceGitProbe stand-in that runs script under sh, a
// real process, so its result is a genuine *exec.ExitError (or nil).
func shProbe(t *testing.T, script string) func(context.Context, string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	return func(ctx context.Context, _ string) (string, error) {
		return runWorkspaceGitProbe(exec.CommandContext(ctx, "sh", "-c", script))
	}
}

// Real processes for every branch of the decision: only a positive exit
// code with a stderr line that starts with git's "fatal: not a git
// repository" reads as "not git".
func TestGitReportsNotARepository_RealProcesses(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		script string
		want   bool
	}{
		{"positive control: exit 128 with git's message", `echo "fatal: not a git repository (or any of the parent directories): .git" >&2; exit 128`, true},
		{"git's message on a later line", `printf 'hint: something\nfatal: not a git repository\n' >&2; exit 128`, true},
		{"non-zero exit with other stderr", `echo "fatal: detected dubious ownership in repository" >&2; exit 128`, false},
		{"the phrase not at the start of a line", `echo "warning: this is not a git repository, maybe" >&2; exit 128`, false},
		{"signalled exit with the message", `echo "fatal: not a git repository" >&2; kill -9 $$`, false},
		{"exit 0 with the message", `echo "fatal: not a git repository" >&2; exit 0`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubWorkspaceGitProbe(t, shProbe(t, tc.script))
			if got := gitReportsNotARepository(context.Background(), dir); got != tc.want {
				t.Fatalf("gitReportsNotARepository = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("clean exit 128 with the message after the bound expired", func(t *testing.T) {
		prev := workspaceGitTimeout
		workspaceGitTimeout = 20 * time.Millisecond
		t.Cleanup(func() { workspaceGitTimeout = prev })
		realProbe := shProbe(t, `echo "fatal: not a git repository" >&2; exit 128`)
		stubWorkspaceGitProbe(t, func(ctx context.Context, d string) (string, error) {
			<-ctx.Done()
			// A genuine ExitError, obtained without the expired context.
			return realProbe(context.Background(), d)
		})
		if gitReportsNotARepository(context.Background(), dir) {
			t.Fatal("an answer that arrives after the bound expired must be unknown")
		}
	})
}
