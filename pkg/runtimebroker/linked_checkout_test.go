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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const linkedTestRemote = "https://github.com/acme/widgets.git"

// gitInitWithOrigin makes dir a git repository whose origin is remote (no
// origin when remote is empty).
func gitInitWithOrigin(t *testing.T, dir, remote string) {
	t.Helper()
	args := [][]string{{"init", "-q", "-b", "main"}}
	if remote != "" {
		args = append(args, []string{"remote", "add", "origin", remote})
	}
	for _, a := range args {
		cmd := exec.Command("git", a...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
}

// newLinkedCheckout returns a repo root with an in-repo .scion directory and
// the given origin.
func newLinkedCheckout(t *testing.T, remote string) (repo, dotScion string) {
	t.Helper()
	repo = t.TempDir()
	gitInitWithOrigin(t, repo, remote)
	dotScion = filepath.Join(repo, ".scion")
	if err := os.MkdirAll(dotScion, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, dotScion
}

func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ea == eb
}

func TestResolveLinkedCheckout(t *testing.T) {
	requireWorktreeGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	repo, dotScion := newLinkedCheckout(t, linkedTestRemote)
	_, otherScion := newLinkedCheckout(t, "git@github.com:acme/other.git")
	_, noOriginScion := newLinkedCheckout(t, "")
	plainDir := t.TempDir()
	hubManaged := filepath.Join(home, ".scion", "projects", "widgets")
	if err := os.MkdirAll(hubManaged, 0o755); err != nil {
		t.Fatal(err)
	}

	base := func() linkedCheckoutInput {
		return linkedCheckoutInput{
			WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
			GitClone:      &api.GitCloneConfig{URL: linkedTestRemote, Branch: "main"},
			ProjectPath:   dotScion,
			Colocated:     true,
			RuntimeName:   "docker",
		}
	}
	tooOld := func() (bool, string) { return false, "found 2.40.1" }

	tests := []struct {
		name        string
		mutate      func(*linkedCheckoutInput)
		wantRoute   bool
		wantRefusal string // substring; "" means no refusal
	}{
		{name: "all preconditions, matching checkout", mutate: func(*linkedCheckoutInput) {}, wantRoute: true},
		{name: "project root instead of .scion dir", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = repo }, wantRoute: true},
		{name: "ssh form of the same remote", mutate: func(in *linkedCheckoutInput) { in.GitClone.URL = "git@github.com:acme/widgets.git" }, wantRoute: true},
		{name: "clone URL with credentials", mutate: func(in *linkedCheckoutInput) {
			in.GitClone.URL = "https://x-access-token:s3cr3t@github.com/acme/widgets.git"
		}, wantRoute: true},
		{name: "per-agent (clone) mode", mutate: func(in *linkedCheckoutInput) { in.WorkspaceMode = store.WorkspaceModePerAgent }},
		{name: "unlabelled (empty mode)", mutate: func(in *linkedCheckoutInput) { in.WorkspaceMode = "" }},
		{name: "no git clone", mutate: func(in *linkedCheckoutInput) { in.GitClone = nil }},
		{name: "no project path", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = "" }},
		{name: "path recovered from container", mutate: func(in *linkedCheckoutInput) { in.ProjectPathFromContainer = true }},
		{name: "hub global project", mutate: func(in *linkedCheckoutInput) { in.HubGlobalProject = true }},
		{name: "hub-managed project dir", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = hubManaged }},
		{name: "not co-located", mutate: func(in *linkedCheckoutInput) { in.Colocated = false }},
		{name: "kubernetes runtime", mutate: func(in *linkedCheckoutInput) { in.RuntimeName = "kubernetes" }},
		{name: "agent already has a broker worktree", mutate: func(in *linkedCheckoutInput) { in.HasBrokerWorktree = true }},
		{name: "git too old", mutate: func(in *linkedCheckoutInput) { in.eligibilityOverride = tooOld },
			wantRefusal: "git 2.48 or newer is required for local worktrees (found 2.40.1)"},
		{name: "not a git checkout", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = plainDir },
			wantRefusal: "the project's local path " + plainDir + " is not in a git checkout"},
		{name: "path missing", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = filepath.Join(plainDir, "gone", ".scion") },
			wantRefusal: "is not in a git checkout"},
		{name: "remote mismatch", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = otherScion },
			wantRefusal: "has origin github.com/acme/other, but the hub project's repository is github.com/acme/widgets"},
		{name: "no origin", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = noOriginScion },
			wantRefusal: "has no origin remote, but the hub project's repository is github.com/acme/widgets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)
			got := resolveLinkedCheckout(in)
			if got.Route != tc.wantRoute {
				t.Fatalf("Route = %v, want %v (decision %+v)", got.Route, tc.wantRoute, got)
			}
			if tc.wantRoute {
				if !samePath(t, got.RepoRoot, repo) {
					t.Errorf("RepoRoot = %q, want %q", got.RepoRoot, repo)
				}
				if got.Refusal != "" {
					t.Errorf("unexpected Refusal %q", got.Refusal)
				}
				return
			}
			if tc.wantRefusal == "" {
				if got.Refusal != "" {
					t.Errorf("unexpected Refusal %q: a failed precondition must keep the existing path", got.Refusal)
				}
				if got.Reason == "" {
					t.Error("expected a Reason when not routed")
				}
				return
			}
			if !strings.HasPrefix(got.Refusal, linkedCheckoutErrPrefix) || !strings.Contains(got.Refusal, tc.wantRefusal) {
				t.Errorf("Refusal = %q, want it to contain %q", got.Refusal, tc.wantRefusal)
			}
		})
	}

	t.Run("refusal never shows clone URL credentials", func(t *testing.T) {
		in := base()
		in.ProjectPath = otherScion
		in.GitClone.URL = "https://x-access-token:s3cr3t@github.com/acme/widgets.git"
		got := resolveLinkedCheckout(in)
		if got.Refusal == "" || strings.Contains(got.Refusal, "s3cr3t") {
			t.Errorf("Refusal = %q, want a refusal without the credential", got.Refusal)
		}
	})
}

func TestBrokerManagedWorktreeExists(t *testing.T) {
	dotScion := filepath.Join(t.TempDir(), ".scion")
	in := startContextInputs{ProjectPath: dotScion, ProjectID: "p1", AgentID: "agent-uuid"}
	if brokerManagedWorktreeExists(in) {
		t.Fatal("reported a broker worktree before one exists")
	}
	if err := os.MkdirAll(provision.WorktreePath(filepath.Join(dotScion, "workspace"), "agent-uuid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !brokerManagedWorktreeExists(in) {
		t.Error("did not report the existing broker worktree")
	}
	for name, mutate := range map[string]func(*startContextInputs){
		"no agent id":         func(in *startContextInputs) { in.AgentID = "" },
		"agent id traversal":  func(in *startContextInputs) { in.AgentID = "../x" },
		"no project id":       func(in *startContextInputs) { in.ProjectID = "" },
		"path from container": func(in *startContextInputs) { in.ProjectPathFromContainer = true },
	} {
		c := in
		mutate(&c)
		if brokerManagedWorktreeExists(c) {
			t.Errorf("%s: reported a broker worktree", name)
		}
	}
}

// TestDispatchFromColocatedHub_IgnoresHostCredentialPolicy: the co-located
// check used for linked-checkout routing does not depend on the host
// credentials opt-in that colocatedHostCredentials also requires.
func TestDispatchFromColocatedHub_IgnoresHostCredentialPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	local := &HubConnection{Name: "local", IsColocated: true, HubEndpoint: "http://localhost:8080"}
	s := &Server{config: ServerConfig{HostCredentials: false}, hubConnections: map[string]*HubConnection{"local": local}}
	if !s.dispatchFromColocatedHub(context.Background(), hostCredRequest(""), local) {
		t.Error("dispatchFromColocatedHub = false for the sole co-located loopback connection")
	}
	if s.colocatedHostCredentials(context.Background(), hostCredRequest(""), local) {
		t.Error("colocatedHostCredentials = true with the host-credentials policy off")
	}
	remote := &HubConnection{Name: "remote", HubEndpoint: "http://localhost:8080"}
	if s.dispatchFromColocatedHub(context.Background(), hostCredRequest(""), remote) {
		t.Error("dispatchFromColocatedHub = true for a connection that is not co-located")
	}
}

// linkedStartContextServer builds a docker-named mock broker whose working
// project (<tmp>/.scion) is a git checkout with origin remote, and one hub
// connection that is co-located (loopback endpoint) when colocated is set.
func linkedStartContextServer(t *testing.T, remote string, gitInit, colocated bool) (*Server, string) {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContextRuntime(t, cfg, "docker")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if gitInit {
		gitInitWithOrigin(t, wd, remote)
	}
	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{
		Name:        "hub-1",
		IsColocated: colocated,
		HubEndpoint: "http://localhost:8080",
		Hydrator:    newTestHydrator(t),
	}
	srv.hubMu.Unlock()
	return srv, filepath.Join(wd, ".scion")
}

func linkedStartInputs(dotScion, cloneURL string, op startOperation) startContextInputs {
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	r.Header.Set("X-Scion-Hub-Connection", "hub-1")
	return startContextInputs{
		Name:          "a1",
		AgentID:       "a1-uuid",
		ProjectID:     "p1",
		ProjectSlug:   "widgets",
		ProjectPath:   dotScion,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config: &CreateAgentConfig{
			GitClone: &api.GitCloneConfig{URL: cloneURL, Branch: "main", Depth: intPtr(1)},
		},
		HTTPRequest: r,
		Operation:   op,
	}
}

// TestBuildStartContext_LinkedCheckoutRoutesToLocalWorktree: a
// worktree-per-agent create or start for a linked checkout, from the
// co-located hub, leaves GitClone and Workspace empty (so ProvisionAgent
// creates or reuses <repo>/.scion/agents/<slug>/workspace), sets no clone
// env, marks the workspace as git, and creates no base clone.
func TestBuildStartContext_LinkedCheckoutRoutesToLocalWorktree(t *testing.T) {
	requireWorktreeGit(t)
	clearSCIONEnv(t)
	for _, op := range []startOperation{opCreate, opHTTPStart, opHTTPRestart} {
		t.Run(string(op), func(t *testing.T) {
			srv, dotScion := linkedStartContextServer(t, linkedTestRemote, true, true)
			sc, err := srv.buildStartContext(context.Background(), linkedStartInputs(dotScion, linkedTestRemote, op))
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			if sc.Opts.GitClone != nil {
				t.Errorf("Opts.GitClone = %+v, want nil", sc.Opts.GitClone)
			}
			if sc.Opts.Workspace != "" {
				t.Errorf("Opts.Workspace = %q, want empty", sc.Opts.Workspace)
			}
			for _, k := range []string{"SCION_GIT_CLONE_URL", "SCION_GIT_BRANCH", "SCION_GIT_DEPTH"} {
				if v, ok := sc.Opts.Env[k]; ok {
					t.Errorf("%s = %q, want unset", k, v)
				}
			}
			if got := sc.Opts.Env["SCION_WORKSPACE_GIT"]; got != "true" {
				t.Errorf("SCION_WORKSPACE_GIT = %q, want true", got)
			}
			if sc.ProvisionedWorktreeRepoRoot != "" {
				t.Errorf("ProvisionedWorktreeRepoRoot = %q, want empty", sc.ProvisionedWorktreeRepoRoot)
			}
			if _, err := os.Lstat(filepath.Join(dotScion, "workspace")); !os.IsNotExist(err) {
				t.Errorf("base clone dir %s exists (stat err: %v)", filepath.Join(dotScion, "workspace"), err)
			}
		})
	}
}

// TestBuildStartContext_LinkedCheckoutNotColocatedKeepsExistingPath: the
// same dispatch from a hub that is not co-located keeps today's
// worktree-per-agent path (a broker-managed worktree off a base clone).
func TestBuildStartContext_LinkedCheckoutNotColocatedKeepsExistingPath(t *testing.T) {
	requireWorktreeGit(t)
	clearSCIONEnv(t)
	bare := initBareRepoWithCommit(t)
	srv, dotScion := linkedStartContextServer(t, bare, true, false)
	sc, err := srv.buildStartContext(context.Background(), linkedStartInputs(dotScion, bare, opCreate))
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	wantPrefix := filepath.Join(dotScion, "workspace", provision.WorktreesSubdir) + string(filepath.Separator)
	if !strings.HasPrefix(sc.Opts.Workspace, wantPrefix) {
		t.Errorf("Opts.Workspace = %q, want a broker-managed worktree under %s", sc.Opts.Workspace, wantPrefix)
	}
}

// TestBuildStartContext_LinkedCheckoutFailsClosed: when the routing
// preconditions hold but the checkout fails verification, the dispatch is
// refused with a 409 and an actionable message, and nothing falls back to a
// clone or creates a base clone.
func TestBuildStartContext_LinkedCheckoutFailsClosed(t *testing.T) {
	requireWorktreeGit(t)
	tests := []struct {
		name    string
		gitInit bool
		origin  string
		want    string
	}{
		{name: "remote mismatch", gitInit: true, origin: "https://github.com/acme/other.git",
			want: "has origin github.com/acme/other, but the hub project's repository is github.com/acme/widgets"},
		{name: "no origin", gitInit: true, want: "has no origin remote"},
		{name: "not a git checkout", gitInit: false, want: "is not in a git checkout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearSCIONEnv(t)
			srv, dotScion := linkedStartContextServer(t, tc.origin, tc.gitInit, true)
			sc, err := srv.buildStartContext(context.Background(), linkedStartInputs(dotScion, linkedTestRemote, opCreate))
			if err == nil {
				t.Fatalf("buildStartContext succeeded (GitClone=%+v Workspace=%q), want a refusal", sc.Opts.GitClone, sc.Opts.Workspace)
			}
			var sce *startContextError
			if !errors.As(err, &sce) {
				t.Fatalf("error %v is not a startContextError", err)
			}
			if sce.Status != http.StatusConflict {
				t.Errorf("Status = %d, want %d", sce.Status, http.StatusConflict)
			}
			if !strings.Contains(sce.Message, tc.want) {
				t.Errorf("Message = %q, want it to contain %q", sce.Message, tc.want)
			}
			if _, err := os.Lstat(filepath.Join(dotScion, "workspace")); !os.IsNotExist(err) {
				t.Errorf("base clone dir exists after a refusal (stat err: %v)", err)
			}
		})
	}
}
