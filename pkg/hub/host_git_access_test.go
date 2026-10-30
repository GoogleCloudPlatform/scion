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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hostGitTestToken = "project-git-token-value"

// hostGitTrace is an https git endpoint that answers every request 401
// (Basic) and records the decoded user:password of every Authorization
// header it receives. The host user's git config (GIT_CONFIG_GLOBAL, system
// config off) trusts its certificate and carries an ambient credential store
// holding "ambient:ambient-secret" for it.
type hostGitTrace struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
	// headers records every X-Ambient request header value.
	headers []string
	// askpassLog records every run of the GIT_ASKPASS program the trace
	// sets up (it answers "askpass-secret").
	askpassLog string
}

// hostGitMark is a position in a trace's records.
type hostGitMark struct{ auth, askpass, headers int }

func newHostGitTrace(t *testing.T) *hostGitTrace {
	t.Helper()
	_, err := exec.LookPath("git")
	require.NoError(t, err, "git is required for this test")
	tr := &hostGitTrace{}
	tr.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			decoded := h
			if b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic ")); err == nil {
				decoded = string(b)
			}
			tr.mu.Lock()
			tr.auth = append(tr.auth, decoded)
			tr.mu.Unlock()
		}
		if v := r.Header.Values("X-Ambient"); len(v) > 0 {
			tr.mu.Lock()
			tr.headers = append(tr.headers, v...)
			tr.mu.Unlock()
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(tr.srv.Close)

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tr.srv.Certificate().Raw}), 0o600))
	creds := filepath.Join(dir, "credentials")
	require.NoError(t, os.WriteFile(creds, []byte(strings.Replace(tr.srv.URL, "https://", "https://ambient:ambient-secret@", 1)+"\n"), 0o600))
	cfg := filepath.Join(dir, "gitconfig")
	body := "[http]\n\tsslCAInfo = " + ca + "\n\textraHeader = X-Ambient: generic\n" +
		"[http \"" + tr.srv.URL + "/\"]\n\textraHeader = X-Ambient: host\n" +
		"[http \"" + tr.url("org/repo") + "\"]\n\textraHeader = X-Ambient: full\n" +
		"[http \"" + tr.url("other/repo") + "\"]\n\textraHeader = X-Ambient: full\n" +
		"[credential]\n\thelper = store --file=" + creds + "\n"
	require.NoError(t, os.WriteFile(cfg, []byte(body), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tr.askpassLog = filepath.Join(dir, "askpass.log")
	askpass := filepath.Join(dir, "askpass")
	require.NoError(t, os.WriteFile(askpass, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> '"+tr.askpassLog+"'\necho askpass-secret\n"), 0o700))
	t.Setenv("GIT_ASKPASS", askpass)
	return tr
}

// url returns the endpoint's URL for owner/repo.
func (tr *hostGitTrace) url(ownerRepo string) string { return tr.srv.URL + "/" + ownerRepo + ".git" }

func (tr *hostGitTrace) seen() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.auth...)
}

func (tr *hostGitTrace) askpassRuns() []string {
	b, _ := os.ReadFile(tr.askpassLog)
	return strings.Fields(string(b))
}

func (tr *hostGitTrace) seenHeaders() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.headers...)
}

func (tr *hostGitTrace) mark() hostGitMark {
	return hostGitMark{auth: len(tr.seen()), askpass: len(tr.askpassRuns()), headers: len(tr.seenHeaders())}
}

// assertNoCredential asserts that since m git offered no credential (not
// the project's token, not the host's credential store) and ran no askpass.
func (tr *hostGitTrace) assertNoCredential(t *testing.T, m hostGitMark) {
	t.Helper()
	assert.Empty(t, tr.seen()[m.auth:], "git must offer no credential (neither the project's token nor the host's)")
	assert.Empty(t, tr.askpassRuns()[m.askpass:], "git must not run GIT_ASKPASS")
	assert.Empty(t, tr.seenHeaders()[m.headers:], "git must send none of the host's extra headers (generic, host or URL scoped)")
}

// assertTokenOnly asserts that since m git offered the project's token and
// no other credential.
func (tr *hostGitTrace) assertTokenOnly(t *testing.T, m hostGitMark) {
	t.Helper()
	got := tr.seen()[m.auth:]
	require.NotEmpty(t, got, "git must offer the project's token")
	for _, a := range got {
		assert.True(t, strings.HasSuffix(a, ":"+hostGitTestToken), "only the project's token may be offered, got %q", a)
	}
	assert.Empty(t, tr.askpassRuns()[m.askpass:], "git must not run GIT_ASKPASS")
}

// hostGitTokenServer returns a test server whose secret backend holds a
// project GITHUB_TOKEN for each project passed to setToken.
func hostGitTokenServer(t *testing.T) (*Server, store.Store, func(projectID string)) {
	t.Helper()
	identityTestHome(t)
	srv, st := testServer(t)
	require.False(t, srv.workspaceBackendShared())
	backend := secret.NewLocalBackend(st, "host-git-hub", "host-git-secret")
	srv.SetSecretBackend(backend)
	setToken := func(projectID string) {
		_, _, err := backend.Set(context.Background(), &secret.SetSecretInput{
			Name: "GITHUB_TOKEN", Value: hostGitTestToken, SecretType: store.SecretTypeEnvironment,
			Target: "GITHUB_TOKEN", Scope: store.ScopeProject, ScopeID: projectID,
			CreatedBy: DevUserID, UpdatedBy: DevUserID,
		})
		require.NoError(t, err)
	}
	return srv, st, setToken
}

// hostGitProject stores a shared-workspace git project with the given
// recorded remote and clone-url label ("" for none).
func hostGitProject(t *testing.T, st store.Store, slug, gitRemote, cloneURLLabel string) *store.Project {
	t.Helper()
	p := &store.Project{
		ID:        api.NewUUID(),
		Name:      slug,
		Slug:      slug,
		GitRemote: gitRemote,
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared},
	}
	if cloneURLLabel != "" {
		p.Labels[store.LabelCloneURL] = cloneURLLabel
	}
	require.NoError(t, st.CreateProject(context.Background(), p))
	return p
}

// hostGitBindingCases are the clone URL / recorded remote pairs, against
// the trace endpoint, that must and must not receive the project's token.
func hostGitBindingCases(tr *hostGitTrace) []struct {
	name, gitRemote, label string
	bound                  bool
} {
	return []struct {
		name, gitRemote, label string
		bound                  bool
	}{
		{"label names another host", "github.com/org/repo", tr.url("org/repo"), false},
		{"label git cannot normalize", "github.com/org/repo", tr.srv.URL + "/../org/repo.git", false},
		{"same host, other owner/repo", tr.url("org/repo"), tr.url("other/repo"), false},
		{"matching recorded remote", tr.url("org/repo"), "", true},
		{"label equals the recorded remote", tr.url("org/repo"), tr.url("org/repo"), true},
	}
}

// The local-backend clone offers the project's token only to the project's
// recorded remote over https; any other https host gets neither the token
// nor a credential from the host user's credential helper or askpass, nor
// the host's extra headers.
func TestHostGitAccess_LocalCloneTokenBoundToRecordedRemote(t *testing.T) {
	tr := newHostGitTrace(t)
	for i, tc := range hostGitBindingCases(tr) {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, setToken := hostGitTokenServer(t)
			m := tr.mark()
			p := hostGitProject(t, st, fmt.Sprintf("host-git-clone-%d", i), tc.gitRemote, tc.label)
			setToken(p.ID)

			err := srv.cloneSharedWorkspaceProject(context.Background(), p)
			require.Error(t, err, "the 401 endpoint cannot be cloned")
			assert.NotContains(t, err.Error(), hostGitTestToken)
			if tc.bound {
				tr.assertTokenOnly(t, m)
			} else {
				tr.assertNoCredential(t, m)
			}
		})
	}
}

// hostGitSourceRepo returns a local repository with one commit on main.
func hostGitSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	return dir
}

// hostGitProvisionedProject stores a shared-workspace project whose local
// workspace was cloned from a registered local fixture, then points its
// clone URL at cloneURLLabel ("" to drop the label) for a later pull.
func hostGitProvisionedProject(t *testing.T, srv *Server, st store.Store, slug, gitRemote, cloneURLLabel string) *store.Project {
	t.Helper()
	source := hostGitSourceRepo(t)
	allowLocalCloneFixture(t, source)
	p := hostGitProject(t, st, slug, gitRemote, source)
	p.Labels[store.LabelDefaultBranch] = "main"
	require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), p))
	if cloneURLLabel == "" {
		delete(p.Labels, store.LabelCloneURL)
	} else {
		p.Labels[store.LabelCloneURL] = cloneURLLabel
	}
	require.NoError(t, st.UpdateProject(context.Background(), p))
	return p
}

func hostGitPull(t *testing.T, srv *Server, projectID string) (int, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/workspace/pull", nil)
	return rec.Code, rec.Body.String()
}

// The workspace pull offers the project's token only to the project's
// recorded remote over https; any other https host gets neither the token
// nor a credential from the host user's credential helper or askpass, nor
// the host's extra headers.
func TestHostGitAccess_PullTokenBoundToRecordedRemote(t *testing.T) {
	tr := newHostGitTrace(t)
	for i, tc := range hostGitBindingCases(tr) {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, setToken := hostGitTokenServer(t)
			p := hostGitProvisionedProject(t, srv, st, fmt.Sprintf("host-git-pull-%d", i), tc.gitRemote, tc.label)
			setToken(p.ID)
			m := tr.mark()

			code, body := hostGitPull(t, srv, p.ID)
			require.NotEqual(t, http.StatusOK, code, "the 401 endpoint cannot be pulled: %s", body)
			assert.NotContains(t, body, hostGitTestToken)
			if tc.bound {
				tr.assertTokenOnly(t, m)
			} else {
				tr.assertNoCredential(t, m)
			}
		})
	}
}

// hostGitFakeGit puts a fake git first on PATH. Every invocation's argv is
// appended to the returned log, one argument per line, then a line
// "GIT_ALLOW_PROTOCOL=<value>" and an "--END--" line. Every invocation runs
// the real git.
func hostGitFakeGit(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	root := t.TempDir()
	log := filepath.Join(root, "argv.log")
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + log + "'; done\n" +
		"printf 'GIT_ALLOW_PROTOCOL=%s\\n--END--\\n' \"${GIT_ALLOW_PROTOCOL-<unset>}\" >> '" + log + "'\n" +
		"exec '" + realGit + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// hostGitInvocations returns every logged invocation's argv (with its
// GIT_ALLOW_PROTOCOL line last); none when the log does not exist.
func hostGitInvocations(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--END--" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return out
}

// The pull refuses a clone URL a hardened host fetch may not use, with the
// clone's fixed 422, before any git runs, and offers no credential.
func TestHostGitAccess_PullRefusesHostCloneURLs(t *testing.T) {
	tr := newHostGitTrace(t)
	unregistered := hostGitSourceRepo(t)
	for i, label := range []string{
		unregistered,
		"file://" + unregistered,
		"http://" + strings.TrimPrefix(tr.url("org/repo"), "https://"),
		"fd::3",
		"-uoption-shaped",
	} {
		t.Run(label, func(t *testing.T) {
			srv, st, setToken := hostGitTokenServer(t)
			p := hostGitProvisionedProject(t, srv, st, fmt.Sprintf("host-git-refuse-%d", i), "github.com/org/repo", label)
			setToken(p.ID)
			log := hostGitFakeGit(t)
			m := tr.mark()

			code, body := hostGitPull(t, srv, p.ID)
			require.Equal(t, http.StatusUnprocessableEntity, code, body)
			assert.Contains(t, body, provision.ErrHostCloneURLRefused.Error())
			assert.NotContains(t, body, unregistered)
			assert.Empty(t, hostGitInvocations(t, log), "no git may run for a refused URL")
			tr.assertNoCredential(t, m)
		})
	}
}

// The pull's fetch runs with the hardened transport settings: after the
// network allow-list, file and http are refused, and GIT_ALLOW_PROTOCOL
// leaves only https, ssh and git.
func TestHostGitAccess_PullFetchHardened(t *testing.T) {
	tr := newHostGitTrace(t)
	srv, st, _ := hostGitTokenServer(t)
	p := hostGitProvisionedProject(t, srv, st, "host-git-hardened", "github.com/org/repo", tr.url("org/repo"))
	log := hostGitFakeGit(t)

	code, body := hostGitPull(t, srv, p.ID)
	require.NotEqual(t, http.StatusOK, code, body)
	var fetch []string
	for _, argv := range hostGitInvocations(t, log) {
		if slices.Contains(argv, "fetch") {
			fetch = argv
		}
	}
	require.NotNil(t, fetch, "the pull must run a fetch")
	sub := slices.Index(fetch, "fetch")
	last := func(prefix string) string {
		v := ""
		for _, a := range fetch[:sub] {
			if strings.HasPrefix(a, prefix) {
				v = a
			}
		}
		return v
	}
	assert.Equal(t, "protocol.file.allow=never", last("protocol.file.allow="), "argv: %q", fetch)
	assert.Equal(t, "protocol.http.allow=never", last("protocol.http.allow="), "argv: %q", fetch)
	assert.Equal(t, "protocol.allow=never", last("protocol.allow="), "argv: %q", fetch)
	assert.Equal(t, "http.followRedirects=false", last("http.followRedirects="), "argv: %q", fetch)
	assert.Equal(t, "GIT_ALLOW_PROTOCOL="+provision.HostCloneProtocols, fetch[len(fetch)-1])
	n := slices.Index(fetch, "--")
	require.Greater(t, n, sub)
	assert.Equal(t, tr.url("org/repo"), fetch[n+1], "the URL must follow --")
}

// hostGitAuthedRepo serves a bare copy of source over https as a dumb HTTP
// git repository at <url>/org/repo.git, answering 401 (Basic) unless a
// request carries the project's token as git's scoped helper sends it
// (oauth2:<token>). The host user's git config trusts its certificate.
func hostGitAuthedRepo(t *testing.T, source string) string {
	t.Helper()
	return hostGitDumbRepo(t, source, true)
}

// hostGitDumbRepo is hostGitAuthedRepo, without the token check when
// requireToken is false.
func hostGitDumbRepo(t *testing.T, source string, requireToken bool) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "org", "repo.git")
	for _, args := range [][]string{
		{"clone", "-q", "--bare", source, bare},
		{"-C", bare, "repack", "-a", "-d", "-q"},
		{"-C", bare, "update-server-info"},
	} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+hostGitTestToken))
	files := http.FileServer(http.Dir(root))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireToken && r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	cfg := filepath.Join(dir, "gitconfig")
	require.NoError(t, os.WriteFile(cfg, []byte("[http]\n\tsslCAInfo = "+ca+"\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return srv.URL + "/org/repo.git"
}

// A clone of the project's recorded remote over https still authenticates
// with the project's token, through the hardened clone.
func TestHostGitAccess_BoundCloneAuthenticates(t *testing.T) {
	srv, st, setToken := hostGitTokenServer(t)
	url := hostGitAuthedRepo(t, hostGitSourceRepo(t))
	p := hostGitProject(t, st, "host-git-bound-clone", url, "")
	p.Labels[store.LabelDefaultBranch] = "main"
	setToken(p.ID)

	require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), p))
	workspacePath, err := localProjectPath(p.Slug)
	require.NoError(t, err)
	out, err := exec.Command("git", "-C", workspacePath, "rev-parse", "--verify", "HEAD").CombinedOutput()
	require.NoError(t, err, "the clone must hold the remote's commit: %s", out)
}

// A pull from the project's recorded remote over https still authenticates
// with the project's token, through the hardened fetch, and fast-forwards.
func TestHostGitAccess_BoundPullAuthenticates(t *testing.T) {
	srv, st, setToken := hostGitTokenServer(t)
	source := hostGitSourceRepo(t)
	allowLocalCloneFixture(t, source)
	p := hostGitProject(t, st, "host-git-bound-pull", "github.com/org/placeholder", source)
	p.Labels[store.LabelDefaultBranch] = "main"
	require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), p))

	cmd := exec.Command("git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "second")
	cmd.Dir = source
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	url := hostGitAuthedRepo(t, source)
	p.GitRemote = url
	delete(p.Labels, store.LabelCloneURL)
	require.NoError(t, st.UpdateProject(context.Background(), p))
	setToken(p.ID)

	code, body := hostGitPull(t, srv, p.ID)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"updated":true`)
}

// The trace endpoint's ambient credential store and extra headers answer a
// pull of the project's recorded remote when the project has no token
// (bound URLs keep the host's own git authentication), so the pull's
// no-credential assertions above are not vacuous.
func TestHostGitAccess_PullTraceIsLive(t *testing.T) {
	tr := newHostGitTrace(t)
	srv, st, _ := hostGitTokenServer(t)
	p := hostGitProvisionedProject(t, srv, st, "host-git-pull-live", tr.url("org/repo"), "")
	m := tr.mark()

	code, body := hostGitPull(t, srv, p.ID)
	require.NotEqual(t, http.StatusOK, code, body)
	got := tr.seen()[m.auth:]
	require.NotEmpty(t, got, "the host's credential store must answer a bound pull with no project token")
	assert.Equal(t, "ambient:ambient-secret", got[0])
	assert.Subset(t, tr.seenHeaders()[m.headers:], []string{"generic", "host", "full"})
}

// An unbound https clone and pull of a repository that needs no credential
// still succeed under the unbound-path settings.
func TestHostGitAccess_UnboundUnauthenticatedCloneAndPullSucceed(t *testing.T) {
	srv, st, setToken := hostGitTokenServer(t)
	url := hostGitDumbRepo(t, hostGitSourceRepo(t), false)
	p := hostGitProject(t, st, "host-git-open-clone", "github.com/org/repo", url)
	p.Labels[store.LabelDefaultBranch] = "main"
	setToken(p.ID)
	require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), p))

	code, body := hostGitPull(t, srv, p.ID)
	require.Equal(t, http.StatusOK, code, body)
}
