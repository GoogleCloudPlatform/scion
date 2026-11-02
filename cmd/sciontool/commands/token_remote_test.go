/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// tokenRemoteTestToken is the git token the tests offer. A server that sees
// it as the basic-auth password records that the token was sent to it.
const tokenRemoteTestToken = "token-remote-test-credential"

// tokenGitServer is an https git server (dumb http) for one bare repository.
// It records every request and whether the token was ever sent to it.
type tokenGitServer struct {
	url      string // https://127.0.0.1:<port>/org/repo.git
	remote   string // the recorded-remote form: 127.0.0.1:<port>/org/repo
	host     string // 127.0.0.1:<port>
	requests atomic.Int64
	sawToken atomic.Bool
}

// newTokenGitServer serves bare over https. With requireAuth, a request
// without the token as its basic-auth password gets 401 (a private
// repository); without it every request is served (a public repository), so
// a clone succeeds with or without a credential.
func newTokenGitServer(t *testing.T, bare string, requireAuth bool) *tokenGitServer {
	t.Helper()
	g := &tokenGitServer{}
	files := http.StripPrefix("/org/", http.FileServer(http.Dir(filepath.Dir(bare))))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		_, pass, ok := r.BasicAuth()
		if ok && pass == tokenRemoteTestToken {
			g.sawToken.Store(true)
		}
		if requireAuth && (!ok || pass != tokenRemoteTestToken) {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	g.host = u.Host
	g.url = srv.URL + "/org/" + filepath.Base(bare)
	g.remote = g.host + "/org/" + strings.TrimSuffix(filepath.Base(bare), ".git")
	// Every httptest TLS server shares one certificate; trust it.
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	return g
}

// tokenRemoteBareRepo creates root/repo.git with one commit on main, served
// over dumb http (update-server-info), and isolates git from the user's and
// the system's config. git must be installed: the test fails without it.
func tokenRemoteBareRepo(t *testing.T, root string) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	globalCfg := filepath.Join(root, "global.gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[user]\n\temail=a@b.c\n\tname=A\n[init]\n\tdefaultBranch=main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "")
	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command(gitPath, args...)
		c.Dir = dir
		if out, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git %v: %v\n%s", args, e, out)
		}
	}
	bare := filepath.Join(root, "repo.git")
	run(root, "init", "-q", "--bare", bare)
	seed := filepath.Join(root, "seed")
	run(root, "init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", ".")
	run(seed, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "init")
	run(seed, "push", "-q", bare, "main")
	run(bare, "update-server-info")
	return bare
}

// --- in-container clone (sciontool init) ---

// initCloneWithTokenRemote runs gitCloneWorkspace for cloneURL with the
// token in GITHUB_TOKEN and tokenRemote in api.EnvGitTokenRemote ("" leaves
// it unset), and returns the agent home it used and the clone error.
func initCloneWithTokenRemote(t *testing.T, cloneURL, tokenRemote string) (agentHome string, err error) {
	t.Helper()
	agentHome = t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if mkErr := os.MkdirAll(workspace, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	t.Setenv("HOME", agentHome)
	t.Setenv("SCION_GIT_CLONE_URL", cloneURL)
	t.Setenv("SCION_WORKSPACE_PATH", workspace)
	t.Setenv("SCION_GIT_BRANCH", "main")
	t.Setenv("SCION_GIT_DEPTH", "0")
	t.Setenv("SCION_AGENT_BRANCH", "")
	t.Setenv("SCION_AGENT_NAME", "test-agent")
	t.Setenv("SCION_GITHUB_APP_ENABLED", "")
	t.Setenv("GITHUB_TOKEN", tokenRemoteTestToken)
	t.Setenv(api.EnvGitTokenRemote, tokenRemote)
	if tokenRemote == "" {
		if err := os.Unsetenv(api.EnvGitTokenRemote); err != nil {
			t.Fatal(err)
		}
	}
	return agentHome, gitCloneWorkspace(os.Getuid(), os.Getgid(), agentHome, false)
}

// agentHelperConfigured reports whether the agent's .gitconfig configures any
// credential helper (unscoped or host-scoped).
func agentHelperConfigured(t *testing.T, agentHome string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentHome, ".gitconfig"))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), "helper")
}

// The in-container clone offers the git token only to a clone URL that
// names the project's recorded remote (https, same host and owner/repo). A
// clone-url label naming another host, another repository on the recorded
// host, or no recorded remote at all gets no credential: a private
// repository there is never sent the token.
func TestGitCloneWorkspace_TokenBoundToRecordedRemote(t *testing.T) {
	root := t.TempDir()
	bare := tokenRemoteBareRepo(t, root)

	t.Run("matching remote gets the token", func(t *testing.T) {
		own := newTokenGitServer(t, bare, true)
		home, err := initCloneWithTokenRemote(t, own.url, own.remote)
		if err != nil {
			t.Fatalf("clone of the recorded remote: %v", err)
		}
		if !own.sawToken.Load() {
			t.Error("the recorded remote's clone must be offered the token")
		}
		if !agentHelperConfigured(t, home) {
			t.Error("the recorded remote's clone configures the agent credential helper")
		}
	})

	for _, tc := range []struct {
		name   string
		remote func(other *tokenGitServer) string
	}{
		{"label naming another host", func(*tokenGitServer) string { return "github.com/org/repo" }},
		{"same host, other owner/repo", func(h *tokenGitServer) string { return h.host + "/other/repo" }},
		{"no token remote", func(*tokenGitServer) string { return "" }},
	} {
		t.Run(tc.name+" gets no token", func(t *testing.T) {
			other := newTokenGitServer(t, bare, true)
			_, err := initCloneWithTokenRemote(t, other.url, tc.remote(other))
			if err == nil {
				t.Fatal("a private repository cloned without a credential must fail")
			}
			if strings.Contains(err.Error(), tokenRemoteTestToken) {
				t.Errorf("the clone error carries the token: %v", err)
			}
			if other.requests.Load() == 0 {
				t.Fatal("fixture check: git never reached the server")
			}
			if other.sawToken.Load() {
				t.Error("the token was sent to a clone URL that does not name the recorded remote")
			}
		})
	}
}

// A clone URL that does not name the recorded remote leaves no credential
// helper in the agent's .gitconfig even when the clone itself succeeds (a
// public repository on the label's host), so later fetches and pushes to
// that origin are not offered the token either.
func TestGitCloneWorkspace_NoAgentHelperForUnboundRemote(t *testing.T) {
	root := t.TempDir()
	bare := tokenRemoteBareRepo(t, root)
	public := newTokenGitServer(t, bare, false)

	home, err := initCloneWithTokenRemote(t, public.url, "github.com/org/repo")
	if err != nil {
		t.Fatalf("clone of a public repository: %v", err)
	}
	if agentHelperConfigured(t, home) {
		t.Error("an unbound clone URL must not get a credential helper in the agent's .gitconfig")
	}

	home, err = initCloneWithTokenRemote(t, public.url, "")
	if err != nil {
		t.Fatalf("clone of a public repository: %v", err)
	}
	if agentHelperConfigured(t, home) {
		t.Error("without a token remote the agent's .gitconfig gets no credential helper")
	}

	home, err = initCloneWithTokenRemote(t, public.url, public.remote)
	if err != nil {
		t.Fatalf("clone of a public repository: %v", err)
	}
	if !agentHelperConfigured(t, home) {
		t.Error("fixture check: the matching remote configures the agent credential helper")
	}
}

// --- provision init container (sciontool provision) ---

// provisionCloneWithTokenRemote runs runProvision (the real
// provision.ProvisionShared) for a shared-plain clone of cloneURL with the
// token in GITHUB_TOKEN and tokenRemote in api.EnvGitTokenRemote ("" leaves
// it unset).
func provisionCloneWithTokenRemote(t *testing.T, cloneURL, tokenRemote string) error {
	t.Helper()
	wsDir := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(wsDir, 0o770); err != nil {
		t.Fatal(err)
	}
	oldWorkspace, oldMode, oldUID, oldGID, oldDepth := provisionWorkspace, provisionMode, provisionUID, provisionGID, provisionDepth
	t.Cleanup(func() {
		provisionWorkspace, provisionMode, provisionUID, provisionGID, provisionDepth = oldWorkspace, oldMode, oldUID, oldGID, oldDepth
	})
	provisionWorkspace = wsDir
	provisionMode = "shared-plain"
	provisionUID = os.Getuid()
	provisionGID = os.Getgid()
	provisionDepth = 0
	t.Setenv("SCION_CLONE_URL", cloneURL)
	t.Setenv("SCION_CLONE_BRANCH", "main")
	t.Setenv("SCION_WORKSPACE_MODE", "")
	t.Setenv("SCION_PROVISION_STATE_DIR", "")
	t.Setenv("SCION_PROJECT_ID", "test-proj")
	t.Setenv("SCION_SHARED_DIR_PATHS", "")
	t.Setenv(provision.ChownBestEffortEnv, "1")
	t.Setenv(provision.GitTokenEnv, tokenRemoteTestToken)
	t.Setenv(api.EnvGitTokenRemote, tokenRemote)
	if tokenRemote == "" {
		if err := os.Unsetenv(api.EnvGitTokenRemote); err != nil {
			t.Fatal(err)
		}
	}
	return runProvision(context.Background())
}

// The provisioning init container's clone (through provision.ProvisionShared)
// offers the git token only to a clone URL that names the project's recorded
// remote, the same rule as the in-container clone.
func TestRunProvision_TokenBoundToRecordedRemote(t *testing.T) {
	root := t.TempDir()
	bare := tokenRemoteBareRepo(t, root)

	t.Run("matching remote gets the token", func(t *testing.T) {
		own := newTokenGitServer(t, bare, true)
		if err := provisionCloneWithTokenRemote(t, own.url, own.remote); err != nil {
			t.Fatalf("clone of the recorded remote: %v", err)
		}
		if !own.sawToken.Load() {
			t.Error("the recorded remote's clone must be offered the token")
		}
	})

	for _, tc := range []struct {
		name   string
		remote func(other *tokenGitServer) string
	}{
		{"label naming another host", func(*tokenGitServer) string { return "github.com/org/repo" }},
		{"same host, other owner/repo", func(h *tokenGitServer) string { return h.host + "/other/repo" }},
		{"no token remote", func(*tokenGitServer) string { return "" }},
	} {
		t.Run(tc.name+" gets no token", func(t *testing.T) {
			other := newTokenGitServer(t, bare, true)
			err := provisionCloneWithTokenRemote(t, other.url, tc.remote(other))
			if err == nil {
				t.Fatal("a private repository cloned without a credential must fail")
			}
			if strings.Contains(err.Error(), tokenRemoteTestToken) {
				t.Errorf("the clone error carries the token: %v", err)
			}
			if other.requests.Load() == 0 {
				t.Fatal("fixture check: git never reached the server")
			}
			if other.sawToken.Load() {
				t.Error("the token was sent to a clone URL that does not name the recorded remote")
			}
		})
	}
}

// provisionCloneToken returns the token only for a clone whose URL names the
// recorded remote; no remote, no clone or no token yields none.
func TestProvisionCloneToken(t *testing.T) {
	gc := func(u string) *api.GitCloneConfig { return &api.GitCloneConfig{URL: u} }
	for _, tc := range []struct {
		name, remote, token string
		gc                  *api.GitCloneConfig
		want                string
	}{
		{"matching remote", "github.com/org/repo", "tok", gc("https://github.com/org/repo.git"), "tok"},
		{"label naming another host", "github.com/org/repo", "tok", gc("https://mirror.example/org/repo.git"), ""},
		{"same host, other repo", "github.com/org/repo", "tok", gc("https://github.com/org/other.git"), ""},
		{"no token remote", "", "tok", gc("https://github.com/org/repo.git"), ""},
		{"no clone", "github.com/org/repo", "tok", nil, ""},
		{"no token", "github.com/org/repo", "", gc("https://github.com/org/repo.git"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := provisionCloneToken(tc.gc, tc.token, tc.remote); got != tc.want {
				t.Errorf("provisionCloneToken = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- shared workspace (sciontool init, configureSharedWorkspaceGit) ---

// The shared workspace's credential helper is scoped to the host of the
// project's recorded remote (api.EnvGitTokenRemote), never left unscoped: an
// unscoped helper an earlier version wrote is removed, and without a
// recorded remote no helper is configured at all.
func TestConfigureSharedWorkspaceGit_HelperScopedToTokenRemote(t *testing.T) {
	var failures []string
	old := gitConfigFailureHook
	gitConfigFailureHook = func(args []string, err error) { failures = append(failures, strings.Join(args, " ")) }
	t.Cleanup(func() { gitConfigFailureHook = old })

	for _, tc := range []struct {
		name, remote, wantKey string
	}{
		{"recorded remote", "github.com/org/shared", "credential.https://github.com.helper"},
		{"recorded remote with a port", "ghe.example.com:8443/team/app", "credential.https://ghe.example.com:8443.helper"},
		{"no recorded remote", "", ""},
		{"remote without a repository", "github.com", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failures = nil
			agentHome := t.TempDir()
			gitconfigPath := filepath.Join(agentHome, ".gitconfig")
			// An unscoped helper, as an earlier version left it.
			if err := os.WriteFile(gitconfigPath, []byte("[credential]\n\thelper = !old-helper\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv(api.EnvGitTokenRemote, tc.remote)
			if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
				t.Fatalf("configureSharedWorkspaceGit: %v", err)
			}
			if got := gitConfigGet(t, gitconfigPath, "credential.helper"); got != "" {
				t.Errorf("an unscoped credential.helper is still configured: %q", got)
			}
			data, err := os.ReadFile(gitconfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantKey == "" {
				if strings.Contains(string(data), "helper") {
					t.Errorf("no credential helper may be configured, got:\n%s", data)
				}
			} else if got := gitConfigGet(t, gitconfigPath, tc.wantKey); got == "" {
				t.Errorf("%s not configured, got:\n%s", tc.wantKey, data)
			}
			if len(failures) != 0 {
				t.Errorf("git config failures: %v", failures)
			}
		})
	}
}

// End to end: with the agent's .gitconfig from configureSharedWorkspaceGit,
// git offers the token to the recorded remote's host only. A shared
// workspace whose origin is a clone-url label naming another host is never
// sent the token.
func TestConfigureSharedWorkspaceGit_TokenOnlyToRecordedRemoteHost(t *testing.T) {
	root := t.TempDir()
	bare := tokenRemoteBareRepo(t, root)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}

	fetch := func(t *testing.T, tokenRemote, originURL string) error {
		t.Helper()
		agentHome := t.TempDir()
		t.Setenv(api.EnvGitTokenRemote, tokenRemote)
		if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
			t.Fatalf("configureSharedWorkspaceGit: %v", err)
		}
		ws := filepath.Join(t.TempDir(), "ws")
		run := func(args ...string) error {
			c := exec.Command(gitPath, args...)
			c.Env = append(os.Environ(),
				"GIT_CONFIG_GLOBAL="+filepath.Join(agentHome, ".gitconfig"),
				"GIT_TERMINAL_PROMPT=0",
				"GITHUB_TOKEN="+tokenRemoteTestToken)
			out, err := c.CombinedOutput()
			if err != nil && strings.Contains(string(out), tokenRemoteTestToken) {
				t.Errorf("git output carries the token: %s", out)
			}
			return err
		}
		if err := run("init", "-q", ws); err != nil {
			t.Fatalf("git init: %v", err)
		}
		if err := run("-C", ws, "remote", "add", "origin", originURL); err != nil {
			t.Fatalf("git remote add: %v", err)
		}
		return run("-C", ws, "fetch", "-q", "origin", "main")
	}

	t.Run("recorded remote's host gets the token", func(t *testing.T) {
		record := newTokenGitServer(t, bare, true)
		if err := fetch(t, record.remote, record.url); err != nil {
			t.Fatalf("fetch from the recorded remote: %v", err)
		}
		if !record.sawToken.Load() {
			t.Error("the recorded remote's host must be offered the token")
		}
	})

	for _, tc := range []struct {
		name   string
		remote func(record *tokenGitServer) string
	}{
		{"origin on a label naming another host", func(r *tokenGitServer) string { return r.remote }},
		{"no recorded remote", func(*tokenGitServer) string { return "" }},
	} {
		t.Run(tc.name+" gets no token", func(t *testing.T) {
			record := newTokenGitServer(t, bare, true)
			other := newTokenGitServer(t, bare, true)
			if err := fetch(t, tc.remote(record), other.url); err == nil {
				t.Fatal("a private repository fetched without a credential must fail")
			}
			if other.requests.Load() == 0 {
				t.Fatal("fixture check: git never reached the server")
			}
			if other.sawToken.Load() {
				t.Error("the token was sent to a host other than the recorded remote's")
			}
		})
	}
}

// The clone-per-agent agent .gitconfig helper is scoped to the recorded
// remote's host, like the shared workspace's: a clone of the recorded remote
// gets credential.https://<host>.helper and no unscoped helper (one an
// earlier version left is removed); a clone URL naming another host, or no
// recorded remote, gets no helper at all and the unscoped one is removed too.
// End to end, git with that .gitconfig offers the token to the recorded
// remote's host only.
func TestGitCloneWorkspace_AgentHelperScopedToTokenRemote(t *testing.T) {
	root := t.TempDir()
	bare := tokenRemoteBareRepo(t, root)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	seedUnscoped := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n\thelper = !old-helper\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return home
	}
	clone := func(t *testing.T, home, cloneURL, tokenRemote string) error {
		t.Helper()
		workspace := filepath.Join(t.TempDir(), "workspace")
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		t.Setenv("SCION_GIT_CLONE_URL", cloneURL)
		t.Setenv("SCION_WORKSPACE_PATH", workspace)
		t.Setenv("SCION_GIT_BRANCH", "main")
		t.Setenv("SCION_GIT_DEPTH", "0")
		t.Setenv("SCION_AGENT_BRANCH", "")
		t.Setenv("SCION_AGENT_NAME", "test-agent")
		t.Setenv("SCION_GITHUB_APP_ENABLED", "")
		t.Setenv("GITHUB_TOKEN", tokenRemoteTestToken)
		t.Setenv(api.EnvGitTokenRemote, tokenRemote)
		return gitCloneWorkspace(os.Getuid(), os.Getgid(), home, false)
	}
	fetchWith := func(t *testing.T, home, originURL string) error {
		t.Helper()
		ws := filepath.Join(t.TempDir(), "ws")
		run := func(args ...string) error {
			c := exec.Command(gitPath, args...)
			c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"), "GIT_TERMINAL_PROMPT=0", "GITHUB_TOKEN="+tokenRemoteTestToken)
			return c.Run()
		}
		if err := run("init", "-q", ws); err != nil {
			t.Fatal(err)
		}
		if err := run("-C", ws, "remote", "add", "origin", originURL); err != nil {
			t.Fatal(err)
		}
		return run("-C", ws, "fetch", "-q", "origin", "main")
	}

	t.Run("recorded remote", func(t *testing.T) {
		own := newTokenGitServer(t, bare, true)
		home := seedUnscoped(t)
		if err := clone(t, home, own.url, own.remote); err != nil {
			t.Fatalf("clone: %v", err)
		}
		gitconfig := filepath.Join(home, ".gitconfig")
		if got := gitConfigGet(t, gitconfig, "credential.helper"); got != "" {
			t.Errorf("an unscoped credential.helper is still configured: %q", got)
		}
		if got := gitConfigGet(t, gitconfig, "credential.https://"+own.host+".helper"); got == "" {
			t.Error("the helper scoped to the recorded remote's host is not configured")
		}
		// End to end with only that .gitconfig (no -c on the command line):
		// the recorded remote's host gets the token, another host does not.
		own.sawToken.Store(false)
		if err := fetchWith(t, home, own.url); err != nil {
			t.Errorf("fetch from the recorded remote with the agent .gitconfig: %v", err)
		}
		if !own.sawToken.Load() {
			t.Error("the agent helper did not offer the token to the recorded remote's host")
		}
		other := newTokenGitServer(t, bare, true)
		_ = fetchWith(t, home, other.url)
		if other.requests.Load() == 0 {
			t.Fatal("fixture check: git never reached the other server")
		}
		if other.sawToken.Load() {
			t.Error("the agent helper offered the token to another host")
		}
	})

	for _, tc := range []struct {
		name   string
		remote func(s *tokenGitServer) string
	}{
		{"label naming another host", func(*tokenGitServer) string { return "github.com/org/repo" }},
		{"no recorded remote", func(*tokenGitServer) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			public := newTokenGitServer(t, bare, false)
			home := seedUnscoped(t)
			if err := clone(t, home, public.url, tc.remote(public)); err != nil {
				t.Fatalf("clone of a public repository: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "helper") {
				t.Errorf("no credential helper may remain, got:\n%s", data)
			}
		})
	}
}
