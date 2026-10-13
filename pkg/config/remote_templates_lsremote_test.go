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
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the git ls-remote seam used by resolveGitHubRef (ptone/scion#3670).
// None of these tests may reach the network: they either stub the seam or
// point PATH at a fake git binary.

func TestResolveGitHubRef_UsesLsRemoteSeam(t *testing.T) {
	var gotURLs []string
	restore := SetGitLsRemoteForTest(func(_ context.Context, repoURL string) ([]byte, error) {
		gotURLs = append(gotURLs, repoURL)
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
	assert.Equal(t, []string{"https://x-access-token:tok@github.com/org/repo.git"}, gotURLs)

	gotURLs = nil
	parts = &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "main", Path: "templates"}
	resolveGitHubRef(context.Background(), parts, "")
	assert.Equal(t, "main", parts.Branch)
	assert.Equal(t, "templates", parts.Path)
	assert.Equal(t, []string{"https://github.com/org/repo.git"}, gotURLs)
}

func TestResolveGitHubRef_NoSlashSkipsLsRemote(t *testing.T) {
	t.Cleanup(SetGitLsRemoteForTest(func(context.Context, string) ([]byte, error) {
		t.Errorf("ls-remote must not run when the ref has no slash")
		return nil, errors.New("unexpected")
	}))
	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "main"}
	resolveGitHubRef(context.Background(), parts, "")
	assert.Equal(t, "main", parts.Branch)
	assert.Equal(t, "", parts.Path)
}

func TestResolveGitHubRef_LsRemoteErrorKeepsNaiveParse(t *testing.T) {
	t.Cleanup(SetGitLsRemoteForTest(func(context.Context, string) ([]byte, error) {
		return nil, errors.New("git unavailable")
	}))
	parts := &GitHubURLParts{Owner: "org", Repo: "repo", Branch: "feature", Path: "x/templates"}
	resolveGitHubRef(context.Background(), parts, "")
	assert.Equal(t, "feature", parts.Branch)
	assert.Equal(t, "x/templates", parts.Path)
}

func TestSetGitLsRemoteForTest_Restores(t *testing.T) {
	before := gitLsRemote
	stub := GitLsRemoteFunc(func(context.Context, string) ([]byte, error) { return nil, nil })
	restore := SetGitLsRemoteForTest(stub)
	// The stub must be the runner actually installed.
	assert.Equal(t, reflect.ValueOf(stub).Pointer(), reflect.ValueOf(gitLsRemote).Pointer())
	restore()
	// The production runner must be back in place.
	assert.Equal(t, reflect.ValueOf(before).Pointer(), reflect.ValueOf(gitLsRemote).Pointer())
	assert.Equal(t, reflect.ValueOf(execGitLsRemote).Pointer(), reflect.ValueOf(gitLsRemote).Pointer())
}

// TestExecGitLsRemote_Invocation pins the production runner's argv and the
// prompt-suppressing env vars, using a fake git on PATH (no network).
func TestExecGitLsRemote_Invocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git script requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s|' \"$@\"\nprintf 'TP=%s|AP=%s' \"$GIT_TERMINAL_PROMPT\" \"$GIT_ASKPASS\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir)

	out, err := execGitLsRemote(context.Background(), "https://github.com/org/repo.git")
	require.NoError(t, err)
	assert.Equal(t, "ls-remote|--heads|https://github.com/org/repo.git|TP=0|AP=echo", strings.TrimSpace(string(out)))
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
	_, err := execGitLsRemote(context.Background(),
		"https://x-access-token:"+token+"@github.com/org/repo.git")
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
	_, err := execGitLsRemote(ctx, "https://github.com/org/repo.git")
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
