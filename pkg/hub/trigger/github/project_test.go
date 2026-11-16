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

package github

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectPayload_StripsBloatAndReducesObjects(t *testing.T) {
	raw := []byte(`{
		"action": "opened",
		"number": 42,
		"organization": {"login": "octo-org", "id": 99, "repos_url": "https://api.github.com/orgs/octo-org/repos"},
		"enterprise": {"id": 7, "name": "Octo Corp"},
		"installation": {"id": 98765432101234567, "node_id": "MDIzOklu", "repositories_url": "https://api.github.com/installation/repositories"},
		"sender": {
			"login": "octocat",
			"id": 583231,
			"type": "User",
			"avatar_url": "https://avatars.githubusercontent.com/u/583231",
			"gravatar_id": "",
			"url": "https://api.github.com/users/octocat",
			"followers_url": "https://api.github.com/users/octocat/followers",
			"html_url": "https://github.com/octocat"
		},
		"repository": {
			"id": 1296269,
			"full_name": "octo-org/octo-repo",
			"name": "octo-repo",
			"private": true,
			"default_branch": "main",
			"html_url": "https://github.com/octo-org/octo-repo",
			"url": "https://api.github.com/repos/octo-org/octo-repo",
			"forks_url": "https://api.github.com/repos/octo-org/octo-repo/forks",
			"description": "bloat field not in reduced repository",
			"owner": {
				"login": "octo-org",
				"id": 99,
				"type": "Organization",
				"avatar_url": "https://avatars.githubusercontent.com/u/99",
				"repos_url": "https://api.github.com/orgs/octo-org/repos"
			}
		},
		"pull_request": {
			"id": 1001,
			"number": 42,
			"title": "Add normaliser",
			"body": "Implements §5.8 projection.",
			"html_url": "https://github.com/octo-org/octo-repo/pull/42",
			"url": "https://api.github.com/repos/octo-org/octo-repo/pulls/42",
			"diff_url": "https://github.com/octo-org/octo-repo/pull/42.diff",
			"commits_url": "https://api.github.com/repos/octo-org/octo-repo/pulls/42/commits",
			"_links": {"self": {"href": "https://api.github.com/repos/octo-org/octo-repo/pulls/42"}},
			"user": {
				"login": "octocat",
				"id": 583231,
				"type": "User",
				"avatar_url": "https://avatars.githubusercontent.com/u/583231",
				"gravatar_id": "abc",
				"html_url": "https://github.com/octocat",
				"followers_url": "https://api.github.com/users/octocat/followers"
			},
			"head": {
				"ref": "feat/norm",
				"sha": "1111111111111111111111111111111111111111",
				"repo": {
					"id": 2222,
					"full_name": "fork-user/octo-repo",
					"name": "octo-repo",
					"private": false,
					"default_branch": "main",
					"html_url": "https://github.com/fork-user/octo-repo",
					"archive_url": "https://api.github.com/repos/fork-user/octo-repo/{archive_format}{/ref}",
					"stargazers_count": 500,
					"owner": {
						"login": "fork-user",
						"id": 777,
						"type": "User",
						"avatar_url": "https://avatars.githubusercontent.com/u/777"
					}
				}
			},
			"base": {
				"ref": "main",
				"sha": "2222222222222222222222222222222222222222",
				"repo": {
					"id": 1296269,
					"full_name": "octo-org/octo-repo",
					"name": "octo-repo",
					"private": true,
					"default_branch": "main",
					"html_url": "https://github.com/octo-org/octo-repo",
					"hooks_url": "https://api.github.com/repos/octo-org/octo-repo/hooks",
					"owner": {"login": "octo-org", "id": 99, "type": "Organization"}
				}
			}
		}
	}`)

	out, truncated, err := projectPayload(raw, DefaultMaxPayloadBytes)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.LessOrEqual(t, len(out), DefaultMaxPayloadBytes)

	// Verify 64-bit integer precision is preserved verbatim.
	assert.Contains(t, string(out), `98765432101234567`)

	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var m map[string]any
	require.NoError(t, dec.Decode(&m))

	assert.NotContains(t, m, "organization")
	assert.NotContains(t, m, "enterprise")
	assert.Equal(t, map[string]any{"id": json.Number("98765432101234567")}, m["installation"])
	assert.Equal(t, map[string]any{
		"login": "octocat",
		"id":    json.Number("583231"),
		"type":  "User",
	}, m["sender"])

	repo := m["repository"].(map[string]any)
	assert.Equal(t, "octo-org/octo-repo", repo["full_name"])
	assert.Equal(t, "https://github.com/octo-org/octo-repo", repo["html_url"])
	assert.NotContains(t, repo, "description")
	assert.NotContains(t, repo, "forks_url")
	assert.NotContains(t, repo, "url")
	assert.Equal(t, map[string]any{
		"login": "octo-org",
		"id":    json.Number("99"),
		"type":  "Organization",
	}, repo["owner"])

	pr := m["pull_request"].(map[string]any)
	assert.Equal(t, "https://github.com/octo-org/octo-repo/pull/42", pr["html_url"])
	assert.NotContains(t, pr, "_links")
	assert.NotContains(t, pr, "diff_url")
	assert.NotContains(t, pr, "commits_url")
	assert.NotContains(t, pr, "url")
	prUser := pr["user"].(map[string]any)
	assert.Equal(t, "octocat", prUser["login"])
	assert.Equal(t, "https://github.com/octocat", prUser["html_url"])
	assert.NotContains(t, prUser, "avatar_url")
	assert.NotContains(t, prUser, "gravatar_id")
	assert.NotContains(t, prUser, "followers_url")

	headRepo := pr["head"].(map[string]any)["repo"].(map[string]any)
	assert.Equal(t, "fork-user/octo-repo", headRepo["full_name"])
	assert.NotContains(t, headRepo, "stargazers_count")
	assert.NotContains(t, headRepo, "archive_url")
}

func TestProjectPayload_OversizeTruncatesLongestBodyFirst(t *testing.T) {
	// Construct an issue_comment payload where issue.body is 55 KiB of multi-byte
	// UTF-8 ('🔥' = 4 bytes) and comment.body is 30 KiB, exceeding 64 KiB total.
	bigIssueBody := strings.Repeat("🔥", 14000)   // 56,000 bytes
	bigCommentBody := strings.Repeat("é", 15000) // 30,000 bytes

	payloadMap := map[string]any{
		"action":       "created",
		"installation": map[string]any{"id": 1234},
		"sender":       map[string]any{"login": "octocat", "id": 1, "type": "User"},
		"repository": map[string]any{
			"id":             10,
			"full_name":      "octo-org/octo-repo",
			"name":           "octo-repo",
			"private":        false,
			"default_branch": "main",
			"html_url":       "https://github.com/octo-org/octo-repo",
			"owner":          map[string]any{"login": "octo-org", "id": 2, "type": "Organization"},
		},
		"issue": map[string]any{
			"number":   7,
			"title":    "Oversize issue",
			"body":     bigIssueBody,
			"html_url": "https://github.com/octo-org/octo-repo/issues/7",
		},
		"comment": map[string]any{
			"id":       999,
			"body":     bigCommentBody,
			"html_url": "https://github.com/octo-org/octo-repo/issues/7#issuecomment-999",
		},
	}
	raw, err := json.Marshal(payloadMap)
	require.NoError(t, err)
	require.Greater(t, len(raw), DefaultMaxPayloadBytes)

	out, truncated, err := projectPayload(raw, DefaultMaxPayloadBytes)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.LessOrEqual(t, len(out), DefaultMaxPayloadBytes)
	assert.True(t, utf8.Valid(out))

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(out, &decoded))

	gotIssueBody := decoded["issue"].(map[string]any)["body"].(string)
	gotCommentBody := decoded["comment"].(map[string]any)["body"].(string)
	assert.True(t, utf8.ValidString(gotIssueBody))
	assert.True(t, utf8.ValidString(gotCommentBody))
	// Longest body (issue.body, 56 KiB) is truncated first while shorter comment.body (30 KiB) fits intact!
	assert.Equal(t, bigCommentBody, gotCommentBody)
	assert.Less(t, len(gotIssueBody), len(bigIssueBody))
	assert.NotContains(t, gotIssueBody, "\uFFFD")
}

func TestProjectPayload_OversizeChangesFromBodyFallback(t *testing.T) {
	// An edited event where body is now short, but changes.body.from is 80 KiB.
	rawMap := map[string]any{
		"action":       "edited",
		"installation": map[string]any{"id": 1234},
		"sender":       map[string]any{"login": "octocat", "id": 1, "type": "User"},
		"repository": map[string]any{
			"id":        10,
			"full_name": "octo-org/octo-repo",
			"name":      "octo-repo",
			"owner":     map[string]any{"login": "octo-org", "id": 2, "type": "Organization"},
		},
		"issue": map[string]any{
			"number": 8,
			"title":  "Short now",
			"body":   "short updated body",
		},
		"changes": map[string]any{
			"body": map[string]any{
				"from": strings.Repeat("old line with quotes \"and\" emojis 🚀\n", 2500),
			},
		},
	}
	raw, err := json.Marshal(rawMap)
	require.NoError(t, err)
	require.Greater(t, len(raw), DefaultMaxPayloadBytes)

	out, truncated, err := projectPayload(raw, DefaultMaxPayloadBytes)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.LessOrEqual(t, len(out), DefaultMaxPayloadBytes)
	assert.True(t, json.Valid(out))
}
