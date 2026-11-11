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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the pinned resolution seams: ls-remote by ref and the archive URL
// by commit sha. None of them reaches the network; the ls-remote seam is
// stubbed.

const (
	pinnedSHA1 = "1111111111111111111111111111111111111111"
	pinnedSHA2 = "2222222222222222222222222222222222222222"
	pinnedSHA3 = "3333333333333333333333333333333333333333"
)

func stubLsRemoteRef(t *testing.T, out string) *[]string {
	t.Helper()
	var calls []string
	restore := SetGitLsRemoteRefForTest(func(_ context.Context, repoURL, ref string) ([]byte, error) {
		calls = append(calls, repoURL+" "+ref)
		return []byte(out), nil
	})
	t.Cleanup(restore)
	return &calls
}

func TestResolveGitHubRefSHA_FullSHAUsedAsIs(t *testing.T) {
	calls := stubLsRemoteRef(t, "")
	got, err := ResolveGitHubRefSHA(context.Background(), "org", "repo", strings.ToUpper(pinnedSHA1[:20])+pinnedSHA1[20:])
	require.NoError(t, err)
	assert.Equal(t, pinnedSHA1, got)
	assert.Empty(t, *calls, "a full sha needs no remote call")
}

func TestResolveGitHubRefSHA_RefsResolvedByTheirOwnRef(t *testing.T) {
	out := strings.Join([]string{
		pinnedSHA1 + "\tHEAD",
		pinnedSHA1 + "\trefs/heads/main",
		pinnedSHA2 + "\trefs/heads/release",
		pinnedSHA2 + "\trefs/remotes/origin/v1",
		pinnedSHA3 + "\trefs/tags/v1",
		pinnedSHA2 + "\trefs/tags/v1^{}",
		pinnedSHA3 + "\trefs/tags/v2\r",
	}, "\n")
	cases := []struct {
		ref, want, wantArg string
	}{
		{"", pinnedSHA1, "HEAD"},
		{"main", pinnedSHA1, "main"},
		{"release", pinnedSHA2, "release"},
		{"v1", pinnedSHA2, "v1"}, // peeled tag commit, not the tag object
		{"v2", pinnedSHA3, "v2"},
		{"refs/heads/release", pinnedSHA2, "refs/heads/release"},
	}
	for _, c := range cases {
		t.Run(c.ref, func(t *testing.T) {
			calls := stubLsRemoteRef(t, out)
			got, err := ResolveGitHubRefSHA(context.Background(), "org", "repo", c.ref)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
			assert.Equal(t, []string{"https://github.com/org/repo.git " + c.wantArg}, *calls)
		})
	}
}

func TestResolveGitHubRefSHA_UnresolvableAndAmbiguousFail(t *testing.T) {
	out := pinnedSHA1 + "\trefs/heads/dup\n" + pinnedSHA2 + "\trefs/tags/dup\n" + pinnedSHA3 + "\trefs/heads/feature/x\n"
	stubLsRemoteRef(t, out)
	_, err := ResolveGitHubRefSHA(context.Background(), "org", "repo", "missing")
	assert.ErrorIs(t, err, ErrGitRefUnresolvable)
	_, err = ResolveGitHubRefSHA(context.Background(), "org", "repo", "x")
	assert.ErrorIs(t, err, ErrGitRefUnresolvable, "a suffix match is not a match")
	_, err = ResolveGitHubRefSHA(context.Background(), "org", "repo", "dup")
	assert.ErrorIs(t, err, ErrGitRefAmbiguous)

	for _, bad := range []string{"-upload-pack=x", "a..b", "has space", "semi;colon"} {
		_, err := ResolveGitHubRefSHA(context.Background(), "org", "repo", bad)
		assert.ErrorIs(t, err, ErrGitRefUnresolvable, bad)
	}
}

func TestResolveGitHubRefSHA_RemoteErrorFails(t *testing.T) {
	restore := SetGitLsRemoteRefForTest(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("boom")
	})
	t.Cleanup(restore)
	_, err := ResolveGitHubRefSHA(context.Background(), "org", "repo", "main")
	require.Error(t, err)
}

func TestExecGitLsRemoteRef_RefusesNonHTTPS(t *testing.T) {
	_, err := execGitLsRemoteRef(context.Background(), "ssh://github.com/org/repo.git", "HEAD")
	require.Error(t, err)
	_, err = execGitLsRemoteRef(context.Background(), "git://github.com/org/repo.git", "HEAD")
	require.Error(t, err)
}

func TestCanonicalGitHubRepo(t *testing.T) {
	ok := []struct{ in, owner, repo string }{
		{"https://github.com/Scion-Frontiers/Agent-Team", "scion-frontiers", "agent-team"},
		{"https://github.com/scion-frontiers/agent-team.git", "scion-frontiers", "agent-team"},
		{"https://GitHub.com/scion-frontiers/agent-team/", "scion-frontiers", "agent-team"},
	}
	for _, c := range ok {
		owner, repo, err := CanonicalGitHubRepo(c.in)
		require.NoError(t, err, c.in)
		assert.Equal(t, c.owner, owner, c.in)
		assert.Equal(t, c.repo, repo, c.in)
	}
	refused := []string{
		"http://github.com/org/repo",
		"https://gitlab.com/org/repo",
		"https://github.com.evil.test/org/repo",
		"https://user@github.com/org/repo",
		"https://user:pw@github.com/org/repo",
		"https://github.com:443/org/repo",
		"https://github.com/org/repo/tree/main",
		"https://github.com/org",
		"https://github.com/org/repo?x=1",
		"https://github.com/org/repo#frag",
		"ssh://git@github.com/org/repo.git",
		"git@github.com:org/repo.git",
	}
	for _, in := range refused {
		_, _, err := CanonicalGitHubRepo(in)
		assert.Error(t, err, in)
	}
}

func TestGitHubArchiveURLBySHA(t *testing.T) {
	got, err := GitHubArchiveURLBySHA("Org", "Repo.git", pinnedSHA1)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/org/repo/archive/"+pinnedSHA1+".tar.gz", got)
	_, err = GitHubArchiveURLBySHA("org", "repo", "main")
	assert.Error(t, err, "a branch name is not a commit id")
	_, err = GitHubArchiveURLBySHA("org", "repo", pinnedSHA1[:12])
	assert.Error(t, err, "an abbreviated sha is refused")
}
