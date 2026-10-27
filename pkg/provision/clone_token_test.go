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

package provision

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCloneToken is the token the fake git server accepts. It is a test
// value, not a credential.
const testCloneToken = "test-clone-token-6b1f"

// privateGitServer serves bareRepo over git's dumb HTTP protocol and
// answers 401 unless the request carries basic auth oauth2:testCloneToken,
// the credentials sciontool's helper sends. It returns the repository's
// clone URL. The dumb protocol does not support shallow clones, so the
// tests clone with depth 0.
func privateGitServer(t *testing.T, bareRepo string) string {
	t.Helper()
	run(t, "git", "-C", bareRepo, "update-server-info")
	files := http.StripPrefix("/org/", http.FileServer(http.Dir(filepath.Dir(bareRepo))))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "oauth2" || pass != testCloneToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/org/" + filepath.Base(bareRepo)
}

// isolateGitConfig points git's global config at a file of the test's own
// that configures a "store" credential helper, and returns the path that
// helper would write. System config is ignored.
func isolateGitConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store := filepath.Join(dir, "stored-credentials")
	global := filepath.Join(dir, "gitconfig")
	require.NoError(t, os.WriteFile(global, []byte("[credential]\n\thelper = store --file "+store+"\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "")
	return store
}

// assertTokenAbsent fails if testCloneToken appears in any file under dir.
func assertTokenAbsent(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assert.NotContains(t, string(data), testCloneToken, "token found in %s", path)
		return nil
	})
	require.NoError(t, err)
}

// A private repository is cloned with the token in GITHUB_TOKEN. The token
// is not in the remote URL, the workspace (including .git), the
// provisioning state directory, or a credential store from git's config.
func TestProvisionShared_CloneWithToken_PrivateRepo(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	store := isolateGitConfig(t)
	t.Setenv(GitTokenEnv, testCloneToken)
	cloneURL := privateGitServer(t, initBareGitRepo(t))

	in, stateDir := sharedPlainStateDirInput(t, cloneURL)
	in.CloneWithToken = true
	require.NoError(t, ProvisionShared(in))

	ws := in.Resolved.HostPath
	assert.FileExists(t, filepath.Join(ws, "README.md"))
	assert.FileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
	out, err := exec.Command("git", "-C", ws, "remote", "get-url", "origin").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, cloneURL, strings.TrimSpace(string(out)), "the remote URL is the configured one, without credentials")
	out, err = exec.Command("git", "-C", ws, "config", "--local", "--get-all", "credential.helper").CombinedOutput()
	assert.Error(t, err, "no credential helper is written to the workspace's git config: %s", out)

	assertTokenAbsent(t, ws)
	assertTokenAbsent(t, stateDir)
	assert.NoFileExists(t, store, "a credential helper from git's config must not store the token")
	assertNoGitConfigEnv(t)
}

// assertNoGitConfigEnv fails if this process's environment has any
// GIT_CONFIG_COUNT/KEY/VALUE entry: the credential helper entries belong
// to the clone command only, so later steps never inherit them.
func assertNoGitConfigEnv(t *testing.T) {
	t.Helper()
	assert.Empty(t, os.Getenv("GIT_CONFIG_COUNT"), "GIT_CONFIG_COUNT set in the process env")
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			t.Errorf("%s set in the process env", name)
		}
	}
}

// Defensive redaction: the token value never appears in the error, even if
// git prints it.
func TestCloneError_RedactsCredentialValue(t *testing.T) {
	output := "fatal: unexpected response containing " + testCloneToken + " from server\n"
	for _, withToken := range []bool{false, true} {
		err := cloneError("https://github.com/org/repo.git", output, nil, withToken, testCloneToken)
		assert.NotContains(t, err.Error(), testCloneToken)
		assert.Contains(t, err.Error(), redactedCredential)
	}
	// An empty token redacts nothing.
	err := cloneError("https://github.com/org/repo.git", "fatal: something else\n", nil, true, "")
	assert.Equal(t, "git clone https://github.com/org/repo.git: fatal: something else", err.Error())
	assert.NotContains(t, err.Error(), redactedCredential)
	// The run error is redacted too when git printed nothing.
	err = cloneError("https://github.com/org/repo.git", "", errors.New("exit "+testCloneToken), false, testCloneToken)
	assert.NotContains(t, err.Error(), testCloneToken)
}

// Without CloneWithToken the clone runs as before: no credential, and a
// private repository fails with the existing message.
func TestProvisionShared_CloneWithoutToken_PrivateRepoFails(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	t.Setenv(GitTokenEnv, testCloneToken)
	cloneURL := privateGitServer(t, initBareGitRepo(t))

	in, stateDir := sharedPlainStateDirInput(t, cloneURL)
	err := ProvisionShared(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs credentials")
	assert.NotContains(t, err.Error(), testCloneToken)
	assert.NoFileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
	assertNoCloneScratch(t, in.Resolved.HostPath)
}

// A token the server rejects fails the clone with a message naming the
// project's credential, never its value, and writes no sentinel.
func TestProvisionShared_CloneWithToken_RejectedToken(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	const wrong = "wrong-test-token-0c2d"
	t.Setenv(GitTokenEnv, wrong)
	cloneURL := privateGitServer(t, initBareGitRepo(t))

	in, stateDir := sharedPlainStateDirInput(t, cloneURL)
	in.CloneWithToken = true
	err := ProvisionShared(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not accept the project's git credential")
	assert.NotContains(t, err.Error(), wrong)
	assert.NoFileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
}

// A public repository clones the same with or without CloneWithToken.
func TestProvisionShared_CloneWithToken_PublicRepoUnchanged(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	t.Setenv(GitTokenEnv, testCloneToken)
	bareRepo := initBareGitRepo(t)
	for _, withToken := range []bool{false, true} {
		in, _ := sharedPlainStateDirInput(t, bareRepo)
		in.CloneWithToken = withToken
		require.NoError(t, ProvisionShared(in), "withToken=%v", withToken)
		assert.FileExists(t, filepath.Join(in.Resolved.HostPath, "README.md"))
	}
}

func TestRunGitClone_UsesScopedHelperNotUnscoped(t *testing.T) {
	// runGitClone must authenticate through a host-scoped credential helper,
	// never an unscoped credential.helper. Build the scoped env the way
	// runGitClone does and assert it keys on the clone URL's host.
	env := append(util.HostGitEnv([]string{"PATH=/bin", "GITHUB_TOKEN=tok"}), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	scoped, ok := util.ScopedCredentialHelperEnv(env, "https://github.com/org/repo.git", "tok")
	require.True(t, ok)
	joined := strings.Join(scoped, "\n")
	assert.Contains(t, joined, "credential.https://github.com.helper")
	// The only unscoped credential.helper entry is the empty reset (so no
	// other helper runs or persists); it must have an empty value.
	assert.Contains(t, joined, "GIT_CONFIG_KEY_0=credential.helper\nGIT_CONFIG_VALUE_0=\n")
	// A non-http(s) URL yields no helper (token ignored).
	_, ok = util.ScopedCredentialHelperEnv(env, "git@github.com:org/repo.git", "tok")
	assert.False(t, ok)
}

func TestCloneError_WithToken(t *testing.T) {
	auth := cloneError("https://github.com/org/private.git",
		"fatal: Authentication failed for 'https://github.com/org/private.git/'\n", nil, true, "")
	assert.Contains(t, auth.Error(), "did not accept the project's git credential")
	assert.NotContains(t, auth.Error(), "without a git token")

	notFound := cloneError("https://github.com/org/gone.git", "remote: Repository not found.\n", nil, true, "")
	assert.Contains(t, notFound.Error(), "has no access to it")
}

// runGitClone passes the clone environment's token to cloneError, so a
// token that git prints is redacted. A fake git on PATH prints the value of
// GITHUB_TOKEN when asked to clone and fails; other git commands run the
// real git.
func TestProvisionShared_CloneWithToken_GitOutputRedacted(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do if [ \"$a\" = clone ]; then echo \"fatal: unexpected server response for $GITHUB_TOKEN\" >&2; exit 128; fi; done\n" +
		"exec '" + realGit + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(GitTokenEnv, testCloneToken)

	in, stateDir := sharedPlainStateDirInput(t, "https://git.example.invalid/org/private.git")
	in.CloneWithToken = true
	err = ProvisionShared(in)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testCloneToken)
	assert.Contains(t, err.Error(), "unexpected server response for "+redactedCredential)
	assert.NoFileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
}

// Regression: a provisioning clone (runGitClone) of a repo
// with a tracked .lfsconfig naming a recording server must not contact it (the
// clone skips the LFS smudge), so the server receives no request and no
// Authorization header. Live part gated on git-lfs; the scoped-helper wiring is
// asserted unconditionally in TestRunGitClone_UsesScopedHelperNotUnscoped.
func TestRunGitClone_SkipsLFSSmudge_NoEndpointContact(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs not installed; argv/env wiring covered by TestRunGitClone_UsesScopedHelperNotUnscoped")
	}
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	// Configure the git-lfs filter globally, as `git lfs install` does.
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	f, err := os.OpenFile(global, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		assert.Empty(t, r.Header.Get("Authorization"), "endpoint received an Authorization header")
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer srv.Close()

	// A source repo with a tracked .lfsconfig, .gitattributes and a valid LFS
	// pointer, pushed to a bare repo that runGitClone will clone (file path).
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare.git")
	run(t, "git", "init", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(dir, "work")
	run(t, "git", "clone", bare, work)
	writeTestFile(t, filepath.Join(work, ".lfsconfig"), "[lfs]\n\turl = "+srv.URL+"/lfs\n")
	writeTestFile(t, filepath.Join(work, ".gitattributes"), "*.bin filter=lfs -text\n")
	writeTestFile(t, filepath.Join(work, "big.bin"), "version https://git-lfs.github.com/spec/v1\noid sha256:1111111111111111111111111111111111111111111111111111111111111111\nsize 3\n")
	runIn(t, work, "git", "add", ".")
	runIn(t, work, "git", "-c", "user.name=t", "-c", "user.email=t@t.com", "commit", "-m", "lfs")
	// git-lfs installs a pre-push hook into the fixture on `git add`; push with
	// hooks off so the fixture's own push does not POST to the recording
	// server. Then reset the counter: only the clone under test should matter.
	runIn(t, work, "git", "-c", "core.hooksPath="+os.DevNull, "push", "origin", "main")
	atomic.StoreInt32(&hits, 0)

	dest := filepath.Join(dir, "dest")
	err = runGitClone(context.Background(), ProvisionInput{
		GitClone: &api.GitCloneConfig{URL: bare},
		Resolved: ResolvedWorkspace{HostPath: dest},
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(dest, "big.bin"))
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits), "the .lfsconfig endpoint must not be contacted by the clone")
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// A provisioning clone that needs a token (auth-requiring server) with a
// global credential.helper=store configured must not let the store persist the
// token, and must not contact an endpoint named in the cloned .lfsconfig. The
// credential path runs unconditionally; the LFS smudge only fires when git-lfs
// is installed (then skip-smudge keeps the hit count at 0 regardless).
func TestRunGitClone_WithToken_NoStorePersist_NoLFSContact(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	store := isolateGitConfig(t) // global credential.helper = store --file store
	t.Setenv(GitTokenEnv, testCloneToken)
	if _, err := exec.LookPath("git-lfs"); err == nil {
		// Configure the git-lfs filter globally so a smudge would fire (and be
		// skipped) during the clone's checkout.
		global := os.Getenv("GIT_CONFIG_GLOBAL")
		f, err := os.OpenFile(global, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteString("[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	var hits int32
	lfsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		assert.Empty(t, r.Header.Get("Authorization"), "the .lfsconfig endpoint received an Authorization header")
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer lfsSrv.Close()

	// A bare repo whose content tracks LFS and names the recording server.
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare.git")
	run(t, "git", "init", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(dir, "work")
	run(t, "git", "clone", bare, work)
	writeTestFile(t, filepath.Join(work, ".lfsconfig"), "[lfs]\n\turl = "+lfsSrv.URL+"/lfs\n")
	writeTestFile(t, filepath.Join(work, ".gitattributes"), "*.bin filter=lfs -text\n")
	writeTestFile(t, filepath.Join(work, "big.bin"), "version https://git-lfs.github.com/spec/v1\noid sha256:1111111111111111111111111111111111111111111111111111111111111111\nsize 3\n")
	runIn(t, work, "git", "add", ".")
	runIn(t, work, "git", "-c", "user.name=t", "-c", "user.email=t@t.com", "commit", "-m", "lfs")
	runIn(t, work, "git", "-c", "core.hooksPath="+os.DevNull, "push", "origin", "main")

	// Serve it over dumb HTTP requiring oauth2:testCloneToken.
	cloneURL := privateGitServer(t, bare)
	atomic.StoreInt32(&hits, 0)

	dest := filepath.Join(dir, "dest")
	fullClone := 0 // dumb http does not support shallow
	err := runGitClone(context.Background(), ProvisionInput{
		GitClone:       &api.GitCloneConfig{URL: cloneURL, Depth: &fullClone},
		Resolved:       ResolvedWorkspace{HostPath: dest},
		CloneWithToken: true,
	})
	require.NoError(t, err)
	assert.NoFileExists(t, store, "the global store helper must not persist the token")
	assertTokenAbsent(t, dest)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits), "the .lfsconfig endpoint must not be contacted")
}

// CloneToken: a private repository is cloned with the token given in the
// input alone, with no GITHUB_TOKEN in the process environment, and the
// process environment is left unchanged (the hub's per-project token at
// project create, ptone/scion#5813).
func TestProvisionShared_CloneToken_PrivateRepoWithoutProcessEnv(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	store := isolateGitConfig(t)
	t.Setenv(GitTokenEnv, "")
	cloneURL := privateGitServer(t, initBareGitRepo(t))

	in, stateDir := sharedPlainStateDirInput(t, cloneURL)
	in.CloneToken = testCloneToken
	require.NoError(t, ProvisionShared(in))

	ws := in.Resolved.HostPath
	assert.FileExists(t, filepath.Join(ws, "README.md"))
	assert.FileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
	assert.Empty(t, os.Getenv(GitTokenEnv), "the process environment is not changed")
	assertTokenAbsent(t, ws)
	assertTokenAbsent(t, stateDir)
	assert.NoFileExists(t, store, "a credential helper from git's config must not store the token")
	assertNoGitConfigEnv(t)
}

// CloneToken takes precedence over a different GITHUB_TOKEN in the process
// environment, and a rejected CloneToken is never printed.
func TestProvisionShared_CloneToken_TakesPrecedenceAndIsRedacted(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	isolateGitConfig(t)
	const wrong = "wrong-test-token-77aa"
	cloneURL := privateGitServer(t, initBareGitRepo(t))

	t.Setenv(GitTokenEnv, wrong)
	in, _ := sharedPlainStateDirInput(t, cloneURL)
	in.CloneToken = testCloneToken
	require.NoError(t, ProvisionShared(in), "the input token is used, not the process env")

	t.Setenv(GitTokenEnv, testCloneToken)
	in, stateDir := sharedPlainStateDirInput(t, cloneURL)
	in.CloneToken = wrong
	err := ProvisionShared(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not accept the project's git credential")
	assert.NotContains(t, err.Error(), wrong)
	assert.NoFileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
}
