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
			WorkspaceMode:   store.WorkspaceModeWorktreePerAgent,
			GitClone:        &api.GitCloneConfig{URL: linkedTestRemote, Branch: "main"},
			ProjectPath:     dotScion,
			Colocated:       true,
			WorkstationMode: true,
			RuntimeName:     "docker",
		}
	}
	tooOld := func() (bool, string) {
		return false, "git >= 2.48.0 required for worktree-per-agent mode (--relative-paths), found 2.40.1"
	}

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
		{name: "hosted-mode server", mutate: func(in *linkedCheckoutInput) { in.WorkstationMode = false }},
		{name: "kubernetes runtime", mutate: func(in *linkedCheckoutInput) { in.RuntimeName = "kubernetes" }},
		{name: "agent already has a broker worktree or clone", mutate: func(in *linkedCheckoutInput) { in.HasExistingNonLocalWorkspace = true }},
		{name: "existing workspace wins over git too old", mutate: func(in *linkedCheckoutInput) {
			in.HasExistingNonLocalWorkspace = true
			in.eligibilityOverride = tooOld
		}},
		{name: "git too old", mutate: func(in *linkedCheckoutInput) { in.eligibilityOverride = tooOld },
			wantRefusal: "found 2.40.1; upgrade git on this machine"},
		{name: "not a git checkout", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = plainDir },
			wantRefusal: "the project's local path " + plainDir + " is not in a git checkout"},
		{name: "path missing", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = filepath.Join(plainDir, "gone", ".scion") },
			wantRefusal: "is not in a git checkout"},
		{name: "remote mismatch", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = otherScion },
			wantRefusal: "has origin github.com:acme/other.git, but the hub project's repository is https://github.com/acme/widgets.git"},
		{name: "no origin", mutate: func(in *linkedCheckoutInput) { in.ProjectPath = noOriginScion },
			wantRefusal: "has no origin remote, but the hub project's repository is https://github.com/acme/widgets.git"},
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

	t.Run("refusal never shows credentials", func(t *testing.T) {
		// Passwords containing '/' (unencoded) and a numeric prefix that a
		// URL parser can mistake for a port.
		_, credScion := newLinkedCheckout(t, "https://user:123/or1g-pw@github.com/acme/other.git")
		in := base()
		in.ProjectPath = credScion
		in.GitClone.URL = "https://x-access-token:45/s3cr3t@github.com/acme/widgets.git?token=q7#frag"
		got := resolveLinkedCheckout(in)
		if got.Refusal == "" {
			t.Fatal("expected a remote-mismatch refusal")
		}
		// The refusal names the checkout path, a t.TempDir whose numeric
		// suffix (e.g. ".../TestX1714162123/001") can contain markers such
		// as "123/". The path holds no credentials, so check only the text
		// after it, where the remotes are shown.
		const pathLead = linkedCheckoutErrPrefix + "the project's checkout at "
		cut := strings.Index(got.Refusal, " has origin ")
		if !strings.HasPrefix(got.Refusal, pathLead) || cut <= len(pathLead) {
			t.Fatalf("Refusal %q does not have the expected checkout-path layout", got.Refusal)
		}
		remotes := got.Refusal[cut:]
		for _, secret := range []string{"s3cr3t", "45/", "or1g-pw", "123/", "x-access-token", "user:", "q7", "frag"} {
			if strings.Contains(remotes, secret) {
				t.Errorf("Refusal %q contains %q", got.Refusal, secret)
			}
		}
		if !strings.Contains(got.Refusal, "has origin https://github.com/acme/other.git") ||
			!strings.Contains(got.Refusal, "repository is https://github.com/acme/widgets.git") {
			t.Errorf("Refusal %q does not show the redacted remotes", got.Refusal)
		}
	})
}

// TestResolveLinkedCheckout_RepoRootFailureRefuses: when git cannot
// resolve the checkout's repository root, the dispatch is refused with the
// per-cause message, without git's own error text.
func TestResolveLinkedCheckout_RepoRootFailureRefuses(t *testing.T) {
	requireWorktreeGit(t)
	t.Setenv("HOME", t.TempDir())
	_, dotScion := newLinkedCheckout(t, linkedTestRemote)
	orig := linkedCheckoutRepoRoot
	t.Cleanup(func() { linkedCheckoutRepoRoot = orig })
	linkedCheckoutRepoRoot = func(string) (string, error) {
		return "", errors.New("fatal: detected dubious ownership SECRET-GIT-OUTPUT")
	}
	got := resolveLinkedCheckout(linkedCheckoutInput{
		WorkspaceMode:   store.WorkspaceModeWorktreePerAgent,
		GitClone:        &api.GitCloneConfig{URL: linkedTestRemote},
		ProjectPath:     dotScion,
		Colocated:       true,
		WorkstationMode: true,
		RuntimeName:     "docker",
	})
	if got.Route {
		t.Fatal("routed despite a repository root failure")
	}
	if !strings.HasPrefix(got.Refusal, linkedCheckoutErrPrefix) ||
		!strings.Contains(got.Refusal, "git could not determine the repository root of the project's checkout at") {
		t.Errorf("Refusal = %q, want the repository-root refusal", got.Refusal)
	}
	if strings.Contains(got.Refusal, "SECRET-GIT-OUTPUT") {
		t.Errorf("Refusal %q contains git's error text", got.Refusal)
	}
}

// TestIsHubManagedProjectPath covers the lexical check and the file
// identity check (os.SameFile) that also recognises the projects directory
// through a symlink or, on a case-insensitive filesystem, with different
// letter case.
func TestIsHubManagedProjectPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, ".scion", "projects")
	if err := os.MkdirAll(filepath.Join(projects, "widgets"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(projects, alias); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	tests := map[string]struct {
		path string
		want bool
	}{
		"projects dir":                 {projects, true},
		"project under it":             {filepath.Join(projects, "widgets"), true},
		"missing project under it":     {filepath.Join(projects, "gone", ".scion"), true},
		"via symlink":                  {filepath.Join(alias, "widgets"), true},
		"missing project via symlink":  {filepath.Join(alias, "gone"), true},
		"unrelated dir":                {other, false},
		"sibling with a common prefix": {projects + "-x", false},
		"global dir itself":            {filepath.Join(home, ".scion"), false},
	}
	for name, tc := range tests {
		if got := isHubManagedProjectPath(tc.path); got != tc.want {
			t.Errorf("%s: isHubManagedProjectPath(%q) = %v, want %v", name, tc.path, got, tc.want)
		}
	}

	// Letter case: only meaningful on a case-insensitive filesystem.
	upper := filepath.Join(home, ".scion", "PROJECTS", "widgets")
	if _, err := os.Stat(upper); err != nil {
		t.Log("filesystem is case-sensitive; skipping the letter-case check")
		return
	}
	if !isHubManagedProjectPath(upper) {
		t.Errorf("isHubManagedProjectPath(%q) = false on a case-insensitive filesystem, want true", upper)
	}
}

func TestRedactRemoteForDisplay(t *testing.T) {
	tests := map[string]string{
		"https://github.com/acme/w.git":                     "https://github.com/acme/w.git",
		"https://tok@github.com/acme/w.git":                 "https://github.com/acme/w.git",
		"https://u:p@ss@github.com/acme/w.git":              "https://github.com/acme/w.git",
		"https://u:123/x@github.com/acme/w.git":             "https://github.com/acme/w.git",
		"https://u:p?w@github.com/acme/w.git":               "https://github.com/acme/w.git",
		"https://github.com/acme/w.git?access_token=abc":    "https://github.com/acme/w.git",
		"https://github.com/acme/w.git#frag":                "https://github.com/acme/w.git",
		"ssh://git@github.com/acme/w.git":                   "ssh://github.com/acme/w.git",
		"git@github.com:acme/w.git":                         "github.com:acme/w.git",
		"/srv/git/w.git":                                    "/srv/git/w.git",
		"  https://u:p@github.com/acme/w.git  ":             "https://github.com/acme/w.git",
		"https://x-access-token:a/b/c@host.example/r.git":   "https://host.example/r.git",
		"git@host.example:team/repo-with-dash_and.dots.git": "host.example:team/repo-with-dash_and.dots.git",
	}
	for in, want := range tests {
		if got := redactRemoteForDisplay(in); got != want {
			t.Errorf("redactRemoteForDisplay(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExistingNonLocalWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newIn := func(t *testing.T) startContextInputs {
		dotScion := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(dotScion, 0o755); err != nil {
			t.Fatal(err)
		}
		return startContextInputs{Name: "a1", ProjectPath: dotScion, ProjectID: "p1", AgentID: "agent-uuid"}
	}

	t.Run("nothing on disk", func(t *testing.T) {
		if existingNonLocalWorkspace(newIn(t)) {
			t.Error("reported an existing workspace")
		}
	})
	t.Run("own broker worktree", func(t *testing.T) {
		in := newIn(t)
		if err := os.MkdirAll(provision.WorktreePath(filepath.Join(in.ProjectPath, "workspace"), "agent-uuid"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !existingNonLocalWorkspace(in) {
			t.Error("did not report the agent's broker worktree")
		}
		for name, mutate := range map[string]func(*startContextInputs){
			"no agent id":         func(in *startContextInputs) { in.AgentID = "" },
			"agent id traversal":  func(in *startContextInputs) { in.AgentID = "../x" },
			"no project id":       func(in *startContextInputs) { in.ProjectID = "" },
			"path from container": func(in *startContextInputs) { in.ProjectPathFromContainer = true },
		} {
			c := in
			mutate(&c)
			if existingNonLocalWorkspace(c) {
				t.Errorf("%s: reported an existing workspace", name)
			}
		}
	})
	t.Run("clone in the agent workspace", func(t *testing.T) {
		in := newIn(t)
		ws := filepath.Join(in.ProjectPath, "agents", "a1", "workspace")
		if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !existingNonLocalWorkspace(in) {
			t.Error("did not report the agent's clone")
		}
	})
	t.Run("local worktree in the agent workspace is not a clone", func(t *testing.T) {
		in := newIn(t)
		ws := filepath.Join(in.ProjectPath, "agents", "a1", "workspace")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: ../../../../.git/worktrees/workspace\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if existingNonLocalWorkspace(in) {
			t.Error("reported a local worktree (.git file) as an existing non-local workspace")
		}
	})
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
	cfg.WorkstationMode = true
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
	return linkedStartInputsFor(dotScion, cloneURL, op, "a1", "a1-uuid", "")
}

func linkedStartInputsFor(dotScion, cloneURL string, op startOperation, name, agentID, branch string) startContextInputs {
	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	r.Header.Set("X-Scion-Hub-Connection", "hub-1")
	return startContextInputs{
		Name:          name,
		AgentID:       agentID,
		ProjectID:     "p1",
		ProjectSlug:   "widgets",
		ProjectPath:   dotScion,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config: &CreateAgentConfig{
			GitClone: &api.GitCloneConfig{URL: cloneURL, Branch: "main", Depth: intPtr(1)},
			Branch:   branch,
		},
		HTTPRequest: r,
		Operation:   op,
	}
}

func assertRoutedToLocalWorktree(t *testing.T, sc *startContext) {
	t.Helper()
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
}

func assertBrokerWorktree(t *testing.T, sc *startContext, dotScion string) {
	t.Helper()
	wantPrefix := filepath.Join(dotScion, "workspace", provision.WorktreesSubdir) + string(filepath.Separator)
	if !strings.HasPrefix(sc.Opts.Workspace, wantPrefix) {
		t.Errorf("Opts.Workspace = %q, want a broker-managed worktree under %s", sc.Opts.Workspace, wantPrefix)
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
			assertRoutedToLocalWorktree(t, sc)
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
	assertBrokerWorktree(t, sc, dotScion)
}

// TestBuildStartContext_LinkedCheckoutHostedModeKeepsExistingPath: a hosted
// server is never routed, even with a co-located loopback hub.
func TestBuildStartContext_LinkedCheckoutHostedModeKeepsExistingPath(t *testing.T) {
	requireWorktreeGit(t)
	clearSCIONEnv(t)
	bare := initBareRepoWithCommit(t)
	srv, dotScion := linkedStartContextServer(t, bare, true, true)
	srv.config.WorkstationMode = false
	sc, err := srv.buildStartContext(context.Background(), linkedStartInputs(dotScion, bare, opCreate))
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	assertBrokerWorktree(t, sc, dotScion)
}

// TestBuildStartContext_LinkedCheckoutExistingAgentsKeepTheirWorkspace: on
// the co-located workstation broker, a start or restart of an agent whose
// workspace already exists as a broker-managed worktree (its own, or one it
// joined) or as a clone keeps that path, with no refusal. A create is
// routed even when a stale broker worktree directory exists.
func TestBuildStartContext_LinkedCheckoutExistingAgentsKeepTheirWorkspace(t *testing.T) {
	requireWorktreeGit(t)

	// provisionBrokerWorktree creates agent a1's broker-managed worktree
	// through today's path, by dispatching its create while routing is off.
	provisionBrokerWorktree := func(t *testing.T) (*Server, string, string) {
		t.Helper()
		clearSCIONEnv(t)
		bare := initBareRepoWithCommit(t)
		srv, dotScion := linkedStartContextServer(t, bare, true, true)
		srv.config.WorkstationMode = false
		sc, err := srv.buildStartContext(context.Background(), linkedStartInputs(dotScion, bare, opCreate))
		if err != nil {
			t.Fatalf("provision a1: %v", err)
		}
		assertBrokerWorktree(t, sc, dotScion)
		srv.config.WorkstationMode = true
		return srv, dotScion, bare
	}

	for _, op := range []startOperation{opHTTPStart, opHTTPRestart} {
		t.Run("own broker worktree/"+string(op), func(t *testing.T) {
			srv, dotScion, bare := provisionBrokerWorktree(t)
			sc, err := srv.buildStartContext(context.Background(), linkedStartInputs(dotScion, bare, op))
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			assertBrokerWorktree(t, sc, dotScion)
		})
	}

	t.Run("joined broker worktree", func(t *testing.T) {
		srv, dotScion, bare := provisionBrokerWorktree(t)
		// b1 shares a1's branch, so its registry entry is a1's worktree and
		// it has no worktree of its own.
		in := linkedStartInputsFor(dotScion, bare, opHTTPStart, "b1", "b1-uuid", "a1")
		if !existingNonLocalWorkspace(in) {
			t.Fatal("setup: b1 is not seen as joined to a1's worktree")
		}
		sc, err := srv.buildStartContext(context.Background(), in)
		if err != nil {
			t.Fatalf("buildStartContext: %v", err)
		}
		if sc.Opts.GitClone == nil && sc.Opts.Workspace == "" {
			t.Error("joined agent was routed to a local worktree")
		}
	})

	t.Run("clone in the agent workspace", func(t *testing.T) {
		clearSCIONEnv(t)
		bare := initBareRepoWithCommit(t)
		srv, dotScion := linkedStartContextServer(t, bare, true, true)
		gitInitWithOrigin(t, mkdirAll(t, filepath.Join(dotScion, "agents", "c1", "workspace")), bare)
		for _, op := range []startOperation{opHTTPStart, opHTTPRestart} {
			sc, err := srv.buildStartContext(context.Background(), linkedStartInputsFor(dotScion, bare, op, "c1", "c1-uuid", ""))
			if err != nil {
				t.Fatalf("%s: buildStartContext: %v", op, err)
			}
			if sc.Opts.GitClone == nil && sc.Opts.Workspace == "" {
				t.Errorf("%s: clone agent was routed to a local worktree", op)
			}
		}
	})

	t.Run("create with a stale broker worktree dir is routed", func(t *testing.T) {
		clearSCIONEnv(t)
		srv, dotScion := linkedStartContextServer(t, linkedTestRemote, true, true)
		mkdirAll(t, provision.WorktreePath(filepath.Join(dotScion, "workspace"), "a1-uuid"))
		sc, err := srv.buildStartContext(context.Background(), linkedStartInputs(dotScion, linkedTestRemote, opCreate))
		if err != nil {
			t.Fatalf("buildStartContext: %v", err)
		}
		assertRoutedToLocalWorktree(t, sc)
	})
}

func mkdirAll(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
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
			want: "has origin https://github.com/acme/other.git, but the hub project's repository is https://github.com/acme/widgets.git"},
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
			// The CLI shows details.reason (never the free-text message),
			// keyed by this code.
			if sce.Code != ErrCodeLocalWorktreeRefused {
				t.Errorf("Code = %q, want %q", sce.Code, ErrCodeLocalWorktreeRefused)
			}
			if got, _ := sce.Details[api.BrokerErrDetailReason].(string); got != sce.Message {
				t.Errorf("Details[reason] = %q, want the message %q", got, sce.Message)
			}
			if _, err := os.Lstat(filepath.Join(dotScion, "workspace")); !os.IsNotExist(err) {
				t.Errorf("base clone dir exists after a refusal (stat err: %v)", err)
			}
		})
	}
}
