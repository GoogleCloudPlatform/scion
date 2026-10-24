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

package config

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the git ls-remote seam used by resolveGitHubRef (ptone/scion#3670).
// None of these tests may reach the network: they either stub the seam or
// point PATH at a fake git binary.

func TestResolveGitHubRef_UsesLsRemoteSeam(t *testing.T) {
	var gotURLs, gotTokens []string
	restore := SetGitLsRemoteForTest(func(_ context.Context, repoURL, token string) ([]byte, error) {
		gotURLs = append(gotURLs, repoURL)
		gotTokens = append(gotTokens, token)
		// A remote that can exist in real git: "feature-x" shares a prefix
		// with "feature/x" as a string but is not a path prefix of
		// "feature/x/templates/one", so only "feature/x" may match. (Real git
		// cannot hold both "feature" and "feature/x".)
		return []byte("aaaa\trefs/heads/main\nbbbb\trefs/heads/feature-x\ncccc\trefs/heads/feature/x\r\n"), nil
	})
	t.Cleanup(restore)

	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "feature", Path: "x/templates/one"}
	resolveGitHubRef(context.Background(), parts, "tok")

	assert.Equal(t, "feature/x", parts.Branch)
	assert.Equal(t, "templates/one", parts.Path)
	// The seam receives the plain repository URL and the token as separate arguments.
	assert.Equal(t, []string{"https://github.com/org/repo.git"}, gotURLs)
	assert.Equal(t, []string{"tok"}, gotTokens)

	gotURLs, gotTokens = nil, nil
	parts = &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "main", Path: "templates"}
	resolveGitHubRef(context.Background(), parts, "")
	assert.Equal(t, "main", parts.Branch)
	assert.Equal(t, "templates", parts.Path)
	assert.Equal(t, []string{"https://github.com/org/repo.git"}, gotURLs)
	assert.Equal(t, []string{""}, gotTokens)
}

func TestResolveGitHubRef_NoSlashSkipsLsRemote(t *testing.T) {
	t.Cleanup(SetGitLsRemoteForTest(func(context.Context, string, string) ([]byte, error) {
		t.Errorf("ls-remote must not run when the ref has no slash")
		return nil, errors.New("unexpected")
	}))
	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "main"}
	resolveGitHubRef(context.Background(), parts, "")
	assert.Equal(t, "main", parts.Branch)
	assert.Equal(t, "", parts.Path)
}

func TestResolveGitHubRef_LsRemoteErrorKeepsNaiveParse(t *testing.T) {
	t.Cleanup(SetGitLsRemoteForTest(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("git unavailable")
	}))
	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "feature", Path: "x/templates"}
	resolveGitHubRef(context.Background(), parts, "")
	assert.Equal(t, "feature", parts.Branch)
	assert.Equal(t, "x/templates", parts.Path)
}

func TestSetGitLsRemoteForTest_Restores(t *testing.T) {
	before := gitLsRemote
	stub := GitLsRemoteFunc(func(context.Context, string, string) ([]byte, error) { return nil, nil })
	restore := SetGitLsRemoteForTest(stub)
	// The stub must be the runner actually installed.
	assert.Equal(t, reflect.ValueOf(stub).Pointer(), reflect.ValueOf(gitLsRemote).Pointer())
	restore()
	// The production runner must be back in place.
	assert.Equal(t, reflect.ValueOf(before).Pointer(), reflect.ValueOf(gitLsRemote).Pointer())
	assert.Equal(t, reflect.ValueOf(execGitLsRemote).Pointer(), reflect.ValueOf(gitLsRemote).Pointer())
}

// TestExecGitLsRemote_Invocation pins the production runner's argv (the
// host-side pins, then ls-remote) and the prompt-suppressing env vars, using a
// fake git on PATH (no network). Without a token no credential helper is set.
func TestExecGitLsRemote_Invocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git script requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s|' \"$@\"\nprintf 'TP=%s|AP=%s|CC=%s' \"$GIT_TERMINAL_PROMPT\" \"$GIT_ASKPASS\" \"${GIT_CONFIG_COUNT-}\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir)

	out, err := execGitLsRemote(context.Background(), "https://github.com/org/repo.git", "")
	require.NoError(t, err)
	pins := strings.Join(util.HostGitArgs(), "|")
	assert.Equal(t, pins+"|ls-remote|--heads|https://github.com/org/repo.git|TP=0|AP=echo|CC=", strings.TrimSpace(string(out)))
}

// lsRemoteFakeGit puts a fake `git` on PATH that logs each argument on its
// own line to argvLog and the GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n entries of
// its environment ("key=value", one per line) plus a CRED= entry (the value of
// scopedCredentialVar, or "<unset>") to configLog, and prints no refs. It uses
// shell builtins only (PATH holds nothing else).
func lsRemoteFakeGit(t *testing.T) (argv, config func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake git script requires a POSIX shell")
	}
	dir := t.TempDir()
	logDir := t.TempDir()
	argvLog := filepath.Join(logDir, "argv.log")
	configLog := filepath.Join(logDir, "config.log")
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_GIT_ARGV_LOG"; done
: >> "$FAKE_GIT_CONFIG_LOG"
n=${GIT_CONFIG_COUNT:-0}
i=0
while [ "$i" -lt "$n" ]; do
  eval "k=\${GIT_CONFIG_KEY_$i-}; v=\${GIT_CONFIG_VALUE_$i-}"
  printf '%s=%s\n' "$k" "$v" >> "$FAKE_GIT_CONFIG_LOG"
  i=$((i+1))
done
printf 'CRED=%s\n' "${SCION_SCOPED_GIT_CREDENTIAL-<unset>}" >> "$FAKE_GIT_CONFIG_LOG"
exit 0
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir)
	t.Setenv("FAKE_GIT_ARGV_LOG", argvLog)
	t.Setenv("FAKE_GIT_CONFIG_LOG", configLog)
	read := func(p string) []string {
		data, err := os.ReadFile(p)
		require.NoError(t, err, "fake git must have run")
		trimmed := strings.TrimRight(string(data), "\n")
		if trimmed == "" {
			return nil
		}
		return strings.Split(trimmed, "\n")
	}
	return func() []string { return read(argvLog) }, func() []string { return read(configLog) }
}

// TestResolveGitHubRef_TokenFromScopedCredentialHelper runs branch resolution
// end to end through the production ls-remote runner (fake git, no network):
// git's arguments are the host-side pins, ls-remote and the plain repository
// URL, and the token reaches git through the credential helper scoped to
// https://github.com, which reads it from the git command's environment.
func TestResolveGitHubRef_TokenFromScopedCredentialHelper(t *testing.T) {
	const token = "ghs_lsremote_test_token"
	argv, config := lsRemoteFakeGit(t)

	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "feature", Path: "x/templates"}
	resolveGitHubRef(context.Background(), parts, token)

	args := argv()
	want := append(append([]string{}, util.HostGitArgs()...), "ls-remote", "--heads", "https://github.com/org/repo.git")
	assert.Equal(t, want, args)
	for _, a := range args {
		assert.NotContains(t, a, token, "git arguments contain no secret values")
		if strings.Contains(a, "://") {
			u, err := url.Parse(a)
			require.NoError(t, err)
			assert.Nil(t, u.User, "repository URL is the plain URL: %s", a)
		}
	}

	entries := config()
	creds := credentialEntries(entries)
	require.Len(t, creds, 2, "%v", creds)
	assert.Equal(t, "credential.helper=", creds[0], "configured credential helpers are reset")
	assert.True(t, strings.HasPrefix(creds[1], "credential.https://github.com.helper="), creds[1])
	assert.Contains(t, creds[1], "${"+scopedCredentialVar+"}", "the helper reads the credential variable at run time")
	assert.NotContains(t, creds[1], token, "git config values contain no secret values")
	assert.Equal(t, "CRED="+token, credentialVarEntry(entries), "the token is in the git command's environment")
}

// TestResolveGitHubRef_NoTokenNoCredentialHelper: without a token, ls-remote
// runs anonymously with no credential helper configured.
func TestResolveGitHubRef_NoTokenNoCredentialHelper(t *testing.T) {
	argv, config := lsRemoteFakeGit(t)

	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "feature", Path: "x/templates"}
	resolveGitHubRef(context.Background(), parts, "")

	want := append(append([]string{}, util.HostGitArgs()...), "ls-remote", "--heads", "https://github.com/org/repo.git")
	assert.Equal(t, want, argv())
	entries := config()
	assert.Empty(t, credentialEntries(entries), "no credential helper without a token")
	assert.Equal(t, "CRED=<unset>", credentialVarEntry(entries), "no credential variable without a token")
}

// writeSleepingFakeGit puts a fake `git` on PATH that blocks far longer than
// the shortened ls-remote timeout. `exec` replaces the shell so the sleep is
// the direct child that exec.CommandContext kills; no grandchild lingers.
func writeSleepingFakeGit(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake git script requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexec /bin/sleep 30\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir)
}

func shortenLsRemoteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prevTimeout, prevWait := gitLsRemoteTimeout, gitLsRemoteWaitDelay
	gitLsRemoteTimeout, gitLsRemoteWaitDelay = d, time.Second
	t.Cleanup(func() { gitLsRemoteTimeout, gitLsRemoteWaitDelay = prevTimeout, prevWait })
}

func TestExecGitLsRemote_Timeout(t *testing.T) {
	writeSleepingFakeGit(t)
	shortenLsRemoteTimeout(t, 200*time.Millisecond)

	const token = "ghs_SECRETTOKEN123"
	start := time.Now()
	_, err := execGitLsRemote(context.Background(), "https://github.com/org/repo.git", token)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "want DeadlineExceeded, got %v", err)
	assert.Equal(t, "git ls-remote for org/repo timed out after 200ms: context deadline exceeded", err.Error())
	assert.NotContains(t, err.Error(), token)
	assert.NotContains(t, err.Error(), "x-access-token")
	assert.Less(t, elapsed, 5*time.Second, "ls-remote should be cut off by the timeout, not run to completion")
}

func TestExecGitLsRemote_CallerDeadlineWins(t *testing.T) {
	writeSleepingFakeGit(t)
	shortenLsRemoteTimeout(t, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := execGitLsRemote(ctx, "https://github.com/org/repo.git", "")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	// The caller's own deadline fired, so this is not reported as our bound.
	assert.NotContains(t, err.Error(), "timed out after")
	// Not wrapping DeadlineExceeded is what keeps resolveGitHubRef silent
	// on caller cancellation/deadline.
	assert.False(t, errors.Is(err, context.DeadlineExceeded), "caller deadline must not be reported as our timeout: %v", err)
}

func TestResolveGitHubRef_TimeoutKeepsNaiveParse(t *testing.T) {
	writeSleepingFakeGit(t)
	shortenLsRemoteTimeout(t, 200*time.Millisecond)

	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	const token = "ghs_SECRETTOKEN123"
	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "feature", Path: "x/templates"}
	start := time.Now()
	resolveGitHubRef(context.Background(), parts, token)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, "feature", parts.Branch)
	assert.Equal(t, "x/templates", parts.Path)

	logs := buf.String()
	assert.Equal(t, 1, strings.Count(logs, "level=WARN"), "want exactly one WARN line, got:\n%s", logs)
	assert.Contains(t, logs, "repo=org/repo")
	assert.NotContains(t, logs, token)
	assert.NotContains(t, logs, "x-access-token")
}
