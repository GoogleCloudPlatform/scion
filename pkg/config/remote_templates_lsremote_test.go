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
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

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
		return []byte("aaaa\trefs/heads/main\nbbbb\trefs/heads/feature\ncccc\trefs/heads/feature/x\r\n"), nil
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
	restore := SetGitLsRemoteForTest(func(context.Context, string) ([]byte, error) { return nil, nil })
	require.NotNil(t, gitLsRemote)
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
