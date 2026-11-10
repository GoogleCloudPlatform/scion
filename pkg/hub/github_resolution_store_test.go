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

package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// TestComputeCacheKey tests cache key computation.
func TestComputeCacheKey(t *testing.T) {
	key1 := computeCacheKey("owner", "repo", "skills/test", "main", "public")
	key2 := computeCacheKey("owner", "repo", "skills/test", "main", "public")
	require.Equal(t, key1, key2, "same inputs should produce same key")

	key3 := computeCacheKey("owner", "repo", "skills/test", "main", "12345")
	require.NotEqual(t, key1, key3, "different token scope should produce different key")

	key4 := computeCacheKey("Owner", "Repo", "skills/test", "main", "public")
	require.Equal(t, key1, key4, "owner/repo should be lowercased")
}

// TestIsFullCommitSHA tests SHA validation.
func TestIsFullCommitSHA(t *testing.T) {
	require.True(t, isFullCommitSHA("abcdef1234567890abcdef1234567890abcdef12"))
	require.True(t, isFullCommitSHA("0000000000000000000000000000000000000000"))
	require.False(t, isFullCommitSHA("abcdef123456"))                             // Too short
	require.False(t, isFullCommitSHA("ABCDEF1234567890ABCDEF1234567890ABCDEF12")) // Uppercase
	require.False(t, isFullCommitSHA("main"))                                     // Not a SHA
	require.False(t, isFullCommitSHA(""))                                         // Empty
}

func TestGHRawContentURL(t *testing.T) {
	cases := []struct {
		name      string
		rawBase   string
		owner     string
		repo      string
		commitSHA string
		filePath  string
		want      string
	}{
		{
			name:      "simple path",
			rawBase:   "https://raw.githubusercontent.com",
			owner:     "acme",
			repo:      "skills",
			commitSHA: "abcdef1234567890abcdef1234567890abcdef12",
			filePath:  "skills/deploy/SKILL.md",
			want:      "https://raw.githubusercontent.com/acme/skills/abcdef1234567890abcdef1234567890abcdef12/skills/deploy/SKILL.md",
		},
		{
			name:      "trailing slash on base is not doubled",
			rawBase:   "https://raw.githubusercontent.com/",
			owner:     "acme",
			repo:      "skills",
			commitSHA: "abcdef1234567890abcdef1234567890abcdef12",
			filePath:  "SKILL.md",
			want:      "https://raw.githubusercontent.com/acme/skills/abcdef1234567890abcdef1234567890abcdef12/SKILL.md",
		},
		{
			name:      "spaces are escaped but separators are preserved",
			rawBase:   "https://raw.githubusercontent.com",
			owner:     "acme",
			repo:      "skills",
			commitSHA: "abcdef1234567890abcdef1234567890abcdef12",
			filePath:  "my skills/read me.md",
			want:      "https://raw.githubusercontent.com/acme/skills/abcdef1234567890abcdef1234567890abcdef12/my%20skills/read%20me.md",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ghRawContentURL(tc.rawBase, tc.owner, tc.repo, tc.commitSHA, tc.filePath)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestGHListTree_FilesModesAndURLs pins what the hub reads from the recursive
// git tree of a skill directory (ptone/scion#5781): every regular file at
// any depth, with its normalised mode, its git blob SHA and a permanent raw
// URL built from the pinned commit; symlinks and directory entries are not
// files.
func TestGHListTree_FilesModesAndURLs(t *testing.T) {
	const commitSHA = "abcdef1234567890abcdef1234567890abcdef12"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/repos/acme/skills/git/trees/"+commitSHA+":skills/deploy", r.URL.Path)
		require.Equal(t, "1", r.URL.Query().Get("recursive"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"t","truncated":false,"tree":[
			{"path":"SKILL.md","mode":"100644","type":"blob","sha":"ce013625030ba8dba906f756967f9e9ca394464a","size":6},
			{"path":"scripts","mode":"040000","type":"tree","sha":"1111111111111111111111111111111111111111"},
			{"path":"scripts/run.sh","mode":"100755","type":"blob","sha":"95d09f2b10159347eece71399a7e2e907ea3df4f","size":11},
			{"path":"link","mode":"120000","type":"blob","sha":"2222222222222222222222222222222222222222","size":8}
		]}`))
	}))
	defer srv.Close()

	entries, err := ghListTree(context.Background(), agent.NewGitHubCooldown(nil), "anon", srv.URL, "https://raw.githubusercontent.com",
		"acme", "skills", "skills/deploy", commitSHA, "")
	require.NoError(t, err)

	require.Equal(t, []GitHubFileEntry{
		{
			Path: "SKILL.md",
			URL:  "https://raw.githubusercontent.com/acme/skills/" + commitSHA + "/skills/deploy/SKILL.md",
			Hash: "ce013625030ba8dba906f756967f9e9ca394464a",
			Size: 6,
			Mode: "0644",
		},
		{
			Path: "scripts/run.sh",
			URL:  "https://raw.githubusercontent.com/acme/skills/" + commitSHA + "/skills/deploy/scripts/run.sh",
			Hash: "95d09f2b10159347eece71399a7e2e907ea3df4f",
			Size: 11,
			Mode: "0755",
		},
	}, entries)
}

// TestGHListTree_RejectsUnusableTrees covers the listings the hub refuses
// instead of caching an incomplete or unsafe skill (ptone/scion#5781).
func TestGHListTree_RejectsUnusableTrees(t *testing.T) {
	const commitSHA = "abcdef1234567890abcdef1234567890abcdef12"
	cases := []struct {
		name string
		body string
		want error
	}{
		{
			name: "truncated",
			body: `{"truncated":true,"tree":[{"path":"SKILL.md","mode":"100644","type":"blob","sha":"x","size":1}]}`,
			want: agent.ErrGitHubTreeTruncated,
		},
		{
			name: "submodule",
			body: `{"truncated":false,"tree":[{"path":"SKILL.md","mode":"100644","type":"blob","sha":"x","size":1},{"path":"vendor","mode":"160000","type":"commit","sha":"y"}]}`,
			want: agent.ErrGitHubTreeSubmodule,
		},
		{
			name: "more than the file limit",
			body: ghManyFilesTree(agent.MaxGitHubSkillFiles + 1),
			want: agent.ErrGitHubSkillTooManyFiles,
		},
		{
			name: "parent segment",
			body: `{"truncated":false,"tree":[{"path":"../SKILL.md","mode":"100644","type":"blob","sha":"x","size":1}]}`,
			want: agent.ErrGitHubTreeUnsafePath,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := ghListTree(context.Background(), agent.NewGitHubCooldown(nil), "anon", srv.URL, "https://raw.githubusercontent.com",
				"acme", "skills", "skills/deploy", commitSHA, "")
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// ghManyFilesTree is a git trees API response listing n regular files.
func ghManyFilesTree(n int) string {
	var b strings.Builder
	b.WriteString(`{"truncated":false,"tree":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"path":"f%04d.md","mode":"100644","type":"blob","sha":"a","size":1}`, i)
	}
	b.WriteString("]}")
	return b.String()
}

// TestGHListTree_RawBaseOverride confirms the raw origin is configurable,
// which is what lets a GitHub Enterprise deployment work.
func TestGHListTree_RawBaseOverride(t *testing.T) {
	const commitSHA = "abcdef1234567890abcdef1234567890abcdef12"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"truncated":false,"tree":[{"path":"SKILL.md","mode":"100644","type":"blob","sha":"ce013625030ba8dba906f756967f9e9ca394464a","size":6}]}`))
	}))
	defer srv.Close()

	entries, err := ghListTree(context.Background(), agent.NewGitHubCooldown(nil), "anon", srv.URL, "https://raw.ghe.example.com",
		"acme", "skills", "s", commitSHA, "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t,
		"https://raw.ghe.example.com/acme/skills/"+commitSHA+"/s/SKILL.md",
		entries[0].URL)
}

// TestComputeCacheKey_EmptyRefDiffersFromHEAD documents why resolveGitHubSkill
// must default an omitted ref before computing the cache key: the two spellings
// resolve to the same commit but key differently, so leaving the ref empty
// would halve the hit rate for every unpinned gh:// URI.
func TestComputeCacheKey_EmptyRefDiffersFromHEAD(t *testing.T) {
	empty := computeCacheKey("acme", "skills", "skills/deploy", "", "public")
	head := computeCacheKey("acme", "skills", "skills/deploy", "HEAD", "public")
	require.NotEqual(t, empty, head)
}
