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
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func projectPayload(raw []byte, maxBytes int) ([]byte, bool, error) {
	return projectPayloadForEvent("", raw, maxBytes)
}

func projectPayloadForEvent(eventName string, raw []byte, maxBytes int) ([]byte, bool, error) {
	root, err := decodeJSONObject(raw)
	if err != nil {
		return nil, false, err
	}
	return projectDecodedPayload(root, eventName, maxBytes)
}

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

func TestProjectPayload_OversizeEditedTruncatesChangesFromBeforeCurrentBody(t *testing.T) {
	// Both current issue.body (40 KiB) and pre-edit changes.body.from (40 KiB) are large;
	// together (~80 KiB) they exceed the 64 KiB cap, while current issue.body alone fits.
	// Pre-edit changes.body.from must be truncated first so the agent sees the current body intact.
	currentBody := strings.Repeat("current-prose-🚀-", 2400) // ~45,600 bytes (< 64 KiB)
	oldBody := strings.Repeat("previous-prose-🔥-", 2400)    // ~48,000 bytes

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
			"title":  "Edited large issue",
			"body":   currentBody,
		},
		"changes": map[string]any{
			"body": map[string]any{
				"from": oldBody,
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

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(out, &decoded))

	gotCurrent := decoded["issue"].(map[string]any)["body"].(string)
	gotOld := decoded["changes"].(map[string]any)["body"].(map[string]any)["from"].(string)
	assert.Equal(t, currentBody, gotCurrent, "current body should stay intact when shrinking pre-edit changes.body.from suffices")
	assert.Less(t, len(gotOld), len(oldBody), "pre-edit changes.body.from should be truncated first")
	assert.True(t, utf8.ValidString(gotOld))
}

func TestProjectPayload_OversizeDiffHunkKeepsTail(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, " \tearlierDiffLine-🚀-"+strings.Repeat("x", 40))
	}
	lines = append(lines, "+\tfinalCommentedDiffLine()")
	bigHunk := strings.Join(lines, "\n")

	rawMap := map[string]any{
		"action":       "created",
		"installation": map[string]any{"id": 12},
		"sender":       map[string]any{"login": "u", "id": 1, "type": "User"},
		"repository":   map[string]any{"id": 1, "full_name": "o/r", "name": "r", "owner": map[string]any{"login": "o", "id": 1, "type": "User"}},
		"comment": map[string]any{
			"id":        55,
			"body":      "",
			"diff_hunk": bigHunk,
		},
	}
	raw, err := json.Marshal(rawMap)
	require.NoError(t, err)

	out, truncated, err := projectPayload(raw, 1200)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.LessOrEqual(t, len(out), 1200)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(out, &decoded))
	gotHunk := decoded["comment"].(map[string]any)["diff_hunk"].(string)
	assert.True(t, strings.HasSuffix(gotHunk, "+\tfinalCommentedDiffLine()"), "projected diff_hunk should preserve commented tail, got: %q", gotHunk)
	assert.True(t, utf8.ValidString(gotHunk))
}

func TestProjectDecodedPayload_ReducesPRBranchReposBeforeSanitizeWithoutMutatingInput(t *testing.T) {
	buildRoot := func(bloatEntries int) map[string]any {
		makeRepo := func(fullName string, id int) map[string]any {
			repo := map[string]any{
				"id":             json.Number(fmt.Sprintf("%d", id)),
				"full_name":      fullName,
				"name":           "octo-repo",
				"private":        false,
				"default_branch": "main",
				"html_url":       "https://github.com/" + fullName,
				"owner": map[string]any{
					"login":      "octo-org",
					"id":         json.Number("99"),
					"type":       "Organization",
					"avatar_url": "https://avatars.githubusercontent.com/u/99",
				},
			}
			for i := 0; i < bloatEntries; i++ {
				// Slice values force per-entry heap allocations if sanitizeValue
				// deep-copies the raw repo map before reduction.
				repo[fmt.Sprintf("bloat_%04d", i)] = []any{"a", "b", "c"}
			}
			return repo
		}

		return map[string]any{
			"action":       "opened",
			"number":       json.Number("42"),
			"installation": map[string]any{"id": json.Number("10")},
			"sender":       map[string]any{"login": "octocat", "id": json.Number("1"), "type": "User"},
			"repository":   makeRepo("octo-org/octo-repo", 100),
			"pull_request": map[string]any{
				"id":       json.Number("1001"),
				"number":   json.Number("42"),
				"title":    "PR title",
				"body":     "PR body",
				"html_url": "https://github.com/octo-org/octo-repo/pull/42",
				"url":      "https://api.github.com/repos/octo-org/octo-repo/pulls/42",
				"head": map[string]any{
					"ref":  "feat/branch",
					"sha":  "1111111111111111111111111111111111111111",
					"user": map[string]any{"login": "fork-user", "avatar_url": "https://avatars.githubusercontent.com/u/7"},
					"repo": makeRepo("fork-user/octo-repo", 200),
				},
				"base": map[string]any{
					"ref":  "main",
					"sha":  "2222222222222222222222222222222222222222",
					"user": map[string]any{"login": "octo-org", "avatar_url": "https://avatars.githubusercontent.com/u/99"},
					"repo": makeRepo("octo-org/octo-repo", 100),
				},
			},
		}
	}

	minimalRoot := buildRoot(0)
	bloatedRoot := buildRoot(1500)

	minAllocs := testing.AllocsPerRun(10, func() {
		_, _, err := projectDecodedPayload(minimalRoot, EventPullRequest, DefaultMaxPayloadBytes)
		require.NoError(t, err)
	})
	bloatedAllocs := testing.AllocsPerRun(10, func() {
		_, _, err := projectDecodedPayload(bloatedRoot, EventPullRequest, DefaultMaxPayloadBytes)
		require.NoError(t, err)
	})

	// Because head.repo and base.repo are reduced before sanitizeValue runs,
	// 3,000 extra nested slice fields in the raw branch repo objects (~15,000
	// allocations if deep-copied) must not be copied during projection. Allow a
	// small delta for race-detector/GC shadow allocations across runs.
	assert.InDelta(t, minAllocs, bloatedAllocs, 50, "raw head.repo/base.repo bloat must not be deep-copied by sanitizeValue")

	// Verify caller's decoded input was not mutated in-place.
	rawPR := bloatedRoot["pull_request"].(map[string]any)
	assert.Equal(t, "https://api.github.com/repos/octo-org/octo-repo/pulls/42", rawPR["url"], "caller pull_request map must remain unmutated")
	rawHead := rawPR["head"].(map[string]any)
	assert.Contains(t, rawHead["user"].(map[string]any), "avatar_url", "caller head.user map must remain unmutated")
	rawHeadRepo := rawHead["repo"].(map[string]any)
	assert.Contains(t, rawHeadRepo, "bloat_0000", "caller head.repo map must retain all raw fields")
	assert.Contains(t, rawHeadRepo, "bloat_1499", "caller head.repo map must retain all raw fields")
	rawBaseRepo := rawPR["base"].(map[string]any)["repo"].(map[string]any)
	assert.Contains(t, rawBaseRepo, "bloat_1499", "caller base.repo map must retain all raw fields")

	// Output must be identical between minimal and bloated raw branch repos, and across repeated calls.
	outMin, truncMin, err := projectDecodedPayload(minimalRoot, EventPullRequest, DefaultMaxPayloadBytes)
	require.NoError(t, err)
	assert.False(t, truncMin)
	outBloat1, truncBloat1, err := projectDecodedPayload(bloatedRoot, EventPullRequest, DefaultMaxPayloadBytes)
	require.NoError(t, err)
	assert.False(t, truncBloat1)
	outBloat2, _, err := projectDecodedPayload(bloatedRoot, EventPullRequest, DefaultMaxPayloadBytes)
	require.NoError(t, err)
	assert.Equal(t, string(outMin), string(outBloat1))
	assert.Equal(t, string(outBloat1), string(outBloat2))
}

func TestProjectPayload_Tier5ProtectsHTMLURLAndPath(t *testing.T) {
	t.Run("preserves html_url and path while shrinking unprotected strings in Tier 5", func(t *testing.T) {
		// Make comment.html_url (~515 bytes) and comment.path (~470 bytes) strictly
		// longer than unprotected issue.title (~425 bytes) so an unprotected Tier 5
		// pass would pick html_url/path first before touching title.
		longPath := "pkg/hub/trigger/github/" + strings.Repeat("subdir/", 62) + "target_file.go"
		longURL := "https://github.com/octo-org/octo-repo/pull/42/" + strings.Repeat("seg/", 110) + "#discussion_r55"
		bigTitle := strings.Repeat("prose-title-🚀-", 25)
		require.Greater(t, len(longURL), len(longPath))
		require.Greater(t, len(longPath), len(bigTitle))

		rawMap := map[string]any{
			"action":       "created",
			"installation": map[string]any{"id": 12},
			"sender":       map[string]any{"login": "u", "id": 1, "type": "User"},
			"repository": map[string]any{
				"id":        1,
				"full_name": "o/r",
				"name":      "r",
				"html_url":  "https://github.com/o/r",
				"owner":     map[string]any{"login": "o", "id": 1, "type": "User"},
			},
			"issue": map[string]any{
				"number":   42,
				"title":    bigTitle,
				"body":     "",
				"html_url": "https://github.com/o/r/pull/42",
			},
			"comment": map[string]any{
				"id":       55,
				"body":     "",
				"path":     longPath,
				"html_url": longURL,
			},
		}
		raw, err := json.Marshal(rawMap)
		require.NoError(t, err)

		const budget = 1500
		out, truncated, err := projectPayload(raw, budget)
		require.NoError(t, err)
		assert.True(t, truncated)
		assert.LessOrEqual(t, len(out), budget)

		var decoded map[string]any
		require.NoError(t, json.Unmarshal(out, &decoded))

		comment := decoded["comment"].(map[string]any)
		assert.Equal(t, longPath, comment["path"], "Tier 5 must never truncate protected path")
		assert.Equal(t, longURL, comment["html_url"], "Tier 5 must never truncate protected html_url")

		issue := decoded["issue"].(map[string]any)
		assert.Equal(t, "https://github.com/o/r/pull/42", issue["html_url"], "Tier 5 must never truncate issue html_url")
		gotTitle := issue["title"].(string)
		assert.Less(t, len(gotTitle), len(bigTitle), "unprotected title should absorb the Tier 5 shrinkage")
		assert.True(t, utf8.ValidString(gotTitle))
	})

	t.Run("final fallback returns error when only protected html_url and path are large", func(t *testing.T) {
		// Protected html_url (~750 bytes) + protected path (~750 bytes) alone
		// exceed the 1200-byte payload budget, while unprotected prose fields are
		// tiny/empty. Tier 5 leaves html_url and path untouched and falls through
		// to the explicit final error fallback (Normalize returns Handled=false, err!=nil).
		hugePath := "pkg/hub/trigger/" + strings.Repeat("deep_module/", 65) + "handler.go"
		hugeURL := "https://github.com/octo-org/octo-repo/pull/42/" + strings.Repeat("segment/", 90) + "#discussion_r77"

		raw, err := json.Marshal(map[string]any{
			"action":       "created",
			"installation": map[string]any{"id": 12},
			"sender":       map[string]any{"login": "u", "id": 1, "type": "User"},
			"repository": map[string]any{
				"id":        1,
				"full_name": "o/r",
				"name":      "r",
				"private":   false,
				"html_url":  "https://github.com/o/r",
				"owner":     map[string]any{"login": "o", "id": 1, "type": "User"},
			},
			"pull_request": map[string]any{
				"number":   42,
				"state":    "open",
				"title":    "t",
				"body":     "",
				"html_url": "https://github.com/o/r/pull/42",
				"base":     map[string]any{"ref": "main", "sha": "111", "repo": map[string]any{"id": 1, "full_name": "o/r", "name": "r", "private": false}},
				"head":     map[string]any{"ref": "feat", "sha": "222", "repo": map[string]any{"id": 1, "full_name": "o/r", "name": "r", "private": false}},
			},
			"comment": map[string]any{
				"id":         77,
				"body":       "",
				"path":       hugePath,
				"line":       10,
				"side":       "RIGHT",
				"html_url":   hugeURL,
				"created_at": "2026-05-01T12:00:00Z",
				"updated_at": "2026-05-01T12:00:00Z",
			},
		})
		require.NoError(t, err)

		out, truncated, projErr := projectPayload(raw, 1200)
		require.Error(t, projErr)
		assert.Contains(t, projErr.Error(), "exceeds MaxPayloadBytes")
		assert.Nil(t, out)
		assert.False(t, truncated)

		res, normErr := Normalize(EventPullRequestReviewComment, "d-prot-pay", raw, Options{
			AppSlug:         "scion",
			MaxPayloadBytes: 1200,
		})
		require.Error(t, normErr)
		assert.Contains(t, normErr.Error(), "exceeds MaxPayloadBytes")
		assert.False(t, res.Handled)
		assert.Nil(t, res.Payload)
	})
}

func TestProjectPayload_PushKeepsHeadCommitAndCapsCommitsAt20(t *testing.T) {
	manyFiles := make([]string, 30)
	for i := range manyFiles {
		manyFiles[i] = fmt.Sprintf("pkg/file_%02d.go", i)
	}
	commits := make([]map[string]any, 25)
	for i := range commits {
		commits[i] = map[string]any{
			"id":        fmt.Sprintf("%040d", i+1),
			"tree_id":   fmt.Sprintf("%040d", i+100),
			"distinct":  true,
			"message":   fmt.Sprintf("commit %d", i+1),
			"timestamp": "2026-05-01T10:00:00Z",
			"url":       "https://github.com/octo-org/octo-repo/commit/" + fmt.Sprintf("%040d", i+1),
			"author":    map[string]any{"name": "Monalisa", "username": "monalisa", "email": "monalisa@example.com"},
			"committer": map[string]any{"name": "Monalisa", "username": "monalisa", "email": "monalisa@example.com"},
			"added":     manyFiles,
			"removed":   []string{},
			"modified":  manyFiles,
			"extra_key": "should-be-dropped",
		}
	}
	raw, err := json.Marshal(map[string]any{
		"ref":          "refs/heads/main",
		"before":       fmt.Sprintf("%040d", 0),
		"after":        fmt.Sprintf("%040d", 25),
		"created":      false,
		"deleted":      false,
		"forced":       false,
		"organization": map[string]any{"login": "octo-org", "id": 99},
		"enterprise":   map[string]any{"id": 7, "name": "Octo Corp"},
		"installation": map[string]any{"id": 42, "repositories_url": "https://api.github.com/installation/repositories"},
		"sender":       map[string]any{"login": "monalisa", "id": 201, "type": "User", "avatar_url": "https://avatars.example/201"},
		"repository": map[string]any{
			"id":             1001,
			"full_name":      "octo-org/octo-repo",
			"name":           "octo-repo",
			"private":        false,
			"default_branch": "main",
			"html_url":       "https://github.com/octo-org/octo-repo",
			"archive_url":    "https://api.github.com/repos/octo-org/octo-repo/{archive_format}",
			"owner":          map[string]any{"login": "octo-org", "id": 99, "type": "Organization", "avatar_url": "https://avatars.example/99"},
		},
		"pusher":      map[string]any{"name": "monalisa", "username": "monalisa", "email": "monalisa@example.com"},
		"head_commit": commits[24],
		"commits":     commits,
	})
	require.NoError(t, err)

	out, truncated, err := projectPayloadForEvent(EventPush, raw, DefaultMaxPayloadBytes)
	require.NoError(t, err)
	assert.True(t, truncated, "cutting commits >20 and file arrays >20 must flag payload_truncated")

	var proj map[string]any
	require.NoError(t, json.Unmarshal(out, &proj))
	assert.NotContains(t, proj, "organization")
	assert.NotContains(t, proj, "enterprise")
	assert.Equal(t, "refs/heads/main", proj["ref"])
	assert.NotNil(t, proj["head_commit"])

	// O3: pusher is reduced to {name, username} with no email.
	pusherMap, ok := proj["pusher"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "monalisa", pusherMap["name"])
	assert.Equal(t, "monalisa", pusherMap["username"])
	assert.NotContains(t, pusherMap, "email")

	// R1: commits capped at 20, reduced fields kept (including url), no email, file arrays capped at 20.
	projCommits, ok := proj["commits"].([]any)
	require.True(t, ok)
	require.Len(t, projCommits, MaxPushCommits)
	firstCommit, ok := projCommits[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, fmt.Sprintf("%040d", 1), firstCommit["id"])
	assert.Equal(t, "https://github.com/octo-org/octo-repo/commit/"+fmt.Sprintf("%040d", 1), firstCommit["url"])
	assert.NotContains(t, firstCommit, "extra_key")
	authorMap, ok := firstCommit["author"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Monalisa", authorMap["name"])
	assert.Equal(t, "monalisa", authorMap["username"])
	assert.NotContains(t, authorMap, "email")
	addedArr, ok := firstCommit["added"].([]any)
	require.True(t, ok)
	assert.Len(t, addedArr, MaxPushCommitFiles)

	repoMap, ok := proj["repository"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, repoMap, "archive_url")
	senderMap, ok := proj["sender"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, senderMap, "avatar_url")
}

func TestNormalize_PushLargeFileListsBoundedByPushShrinkTier(t *testing.T) {
	// R1 regression test: 25 commits each touching 50 long file paths across
	// added/removed/modified must succeed (Handled, payload_truncated=true,
	// PayloadBytes <= 64 KiB) rather than failing projection with an error.
	longPaths := make([]string, 50)
	for i := range longPaths {
		longPaths[i] = fmt.Sprintf("services/payments/internal/handlers/v2/subpackage/refund_handler_%03d.go", i)
	}
	commits := make([]map[string]any, 25)
	for i := range commits {
		commits[i] = map[string]any{
			"id":        fmt.Sprintf("%040d", i+1),
			"tree_id":   fmt.Sprintf("%040d", i+1000),
			"distinct":  true,
			"message":   fmt.Sprintf("feat: update payment handler batch %d", i+1),
			"timestamp": "2026-05-01T11:00:00Z",
			"url":       "https://github.com/octo-org/octo-repo/commit/" + fmt.Sprintf("%040d", i+1),
			"author":    map[string]any{"name": "Monalisa", "username": "monalisa", "email": "monalisa@example.com"},
			"committer": map[string]any{"name": "Monalisa", "username": "monalisa", "email": "monalisa@example.com"},
			"added":     longPaths,
			"removed":   longPaths,
			"modified":  longPaths,
		}
	}
	raw, err := json.Marshal(map[string]any{
		"ref":          "refs/heads/main",
		"before":       fmt.Sprintf("%040d", 0),
		"after":        fmt.Sprintf("%040d", 25),
		"created":      false,
		"deleted":      false,
		"forced":       false,
		"commits":      commits,
		"head_commit":  commits[24],
		"pusher":       map[string]any{"name": "monalisa", "email": "monalisa@example.com"},
		"repository":   map[string]any{"id": 1, "name": "octo-repo", "full_name": "octo-org/octo-repo", "default_branch": "main"},
		"sender":       map[string]any{"login": "monalisa", "id": 201, "type": "User"},
		"installation": map[string]any{"id": 987654},
	})
	require.NoError(t, err)

	res, err := Normalize(EventPush, "d-large-push", raw, Options{AppSlug: "octo-app"})
	require.NoError(t, err)
	assert.True(t, res.Handled)
	assert.True(t, res.Truncated)
	assert.Equal(t, "true", res.Attributes["payload_truncated"])
	assert.LessOrEqual(t, res.PayloadBytes, DefaultMaxPayloadBytes)
	assert.Equal(t, "25", res.Attributes["commit_count"])
	assert.Equal(t, "true", res.Attributes["lists_truncated"])
	assert.Len(t, res.Lists["commits"], MaxPushCommits)

	var proj map[string]any
	require.NoError(t, json.Unmarshal(res.Payload, &proj))
	projCommits, ok := proj["commits"].([]any)
	require.True(t, ok)
	require.Len(t, projCommits, MaxPushCommits)
	first, ok := projCommits[0].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, first, "added")
	assert.NotContains(t, first, "removed")
	assert.NotContains(t, first, "modified")
}

func TestNormalize_PushWorstCaseBoundedByConstruction(t *testing.T) {
	// R1 worst-case test: 2048 commits with every field far beyond its maximum
	// (3000-byte author/committer names, 4000-byte message, 1000-byte url,
	// 200-byte ids/timestamps, 50 x 2000-byte file paths, ~9 KB ref, large
	// repository/sender fields) must succeed with PayloadBytes <= 64 KiB,
	// payload_truncated=true, and Handled=true.
	longName := strings.Repeat("é", 1500) // 3000 bytes, multi-byte UTF-8
	longMsg := strings.Repeat("m", 4000)
	longURL := "https://github.com/octo-org/octo-repo/commit/" + strings.Repeat("u", 1000)
	longID := strings.Repeat("a", 200)
	longPath := "services/deep/" + strings.Repeat("p", 2000) + ".go"
	longPaths := make([]string, 50)
	for i := range longPaths {
		longPaths[i] = longPath
	}

	commits := make([]map[string]any, 2048)
	for i := range commits {
		commits[i] = map[string]any{
			"id":        longID,
			"tree_id":   longID,
			"distinct":  true,
			"message":   longMsg,
			"timestamp": strings.Repeat("t", 200),
			"url":       longURL,
			"author":    map[string]any{"name": longName, "username": longName, "email": "x@example.com"},
			"committer": map[string]any{"name": longName, "username": longName, "email": "x@example.com"},
			"added":     longPaths,
			"removed":   longPaths,
			"modified":  longPaths,
		}
	}
	headCommit := map[string]any{
		"id":        strings.Repeat("b", 200),
		"tree_id":   strings.Repeat("c", 200),
		"distinct":  true,
		"message":   longMsg,
		"timestamp": "2026-05-01T11:00:00Z",
		"url":       longURL,
		"author":    map[string]any{"name": longName, "username": longName, "email": "x@example.com"},
		"committer": map[string]any{"name": longName, "username": longName, "email": "x@example.com"},
		"added":     longPaths,
		"removed":   longPaths,
		"modified":  longPaths,
	}

	longRef := "refs/heads/" + strings.Repeat("r", 9*1024)
	raw, err := json.Marshal(map[string]any{
		"ref":         longRef,
		"before":      strings.Repeat("0", 40),
		"after":       strings.Repeat("f", 40),
		"created":     false,
		"deleted":     false,
		"forced":      true,
		"commits":     commits,
		"head_commit": headCommit,
		"pusher":      map[string]any{"name": longName, "username": longName, "email": "p@example.com"},
		"repository": map[string]any{
			"id":             1296269,
			"name":           "octo-repo",
			"full_name":      "octo-org/octo-repo",
			"private":        false,
			"default_branch": strings.Repeat("d", 500),
			"html_url":       "https://github.com/octo-org/octo-repo/" + strings.Repeat("h", 1000),
			"pushed_at":      1777630500,
			"owner": map[string]any{
				"login": "octo-org",
				"id":    100,
				"type":  strings.Repeat("o", 200),
			},
		},
		"sender": map[string]any{
			"login": "monalisa",
			"id":    201,
			"type":  strings.Repeat("s", 200),
		},
		"installation": map[string]any{"id": 987654},
	})
	require.NoError(t, err)

	res, err := Normalize(EventPush, "d-worst-case", raw, Options{AppSlug: "octo-app"})
	require.NoError(t, err)
	assert.True(t, res.Handled)
	assert.True(t, res.Truncated)
	assert.Equal(t, "true", res.Attributes["payload_truncated"])
	assert.Equal(t, "true", res.Attributes["ref_truncated"])
	assert.LessOrEqual(t, len(res.Attributes["ref"]), MaxPushRefBytes)
	assert.LessOrEqual(t, len(res.Subject), MaxSubjectBytes)
	assert.LessOrEqual(t, res.PayloadBytes, DefaultMaxPayloadBytes)
	assert.True(t, utf8.Valid(res.Payload))
	assert.Equal(t, "2048", res.Attributes["commit_count"])

	// Also verify pathological JSON-escape expansion (control chars expanding 6x)
	// in protected "name" fields does not cause Tier 5 to refuse a push delivery.
	escName := strings.Repeat("\x01", 3000)
	escCommits := make([]map[string]any, 30)
	for i := range escCommits {
		escCommits[i] = map[string]any{
			"id":        fmt.Sprintf("%040d", i+1),
			"tree_id":   fmt.Sprintf("%040d", i+100),
			"distinct":  true,
			"message":   strings.Repeat("\x01", 1000),
			"timestamp": "2026-05-01T11:00:00Z",
			"url":       longURL,
			"author":    map[string]any{"name": escName, "username": escName},
			"committer": map[string]any{"name": escName, "username": escName},
		}
	}
	escRaw, err := json.Marshal(map[string]any{
		"ref":          "refs/heads/main",
		"before":       strings.Repeat("0", 40),
		"after":        strings.Repeat("f", 40),
		"commits":      escCommits,
		"head_commit":  escCommits[29],
		"pusher":       map[string]any{"name": escName, "username": escName},
		"repository":   map[string]any{"id": 1, "name": "octo-repo", "full_name": "octo-org/octo-repo", "default_branch": "main"},
		"sender":       map[string]any{"login": "monalisa", "id": 201, "type": "User"},
		"installation": map[string]any{"id": 987654},
	})
	require.NoError(t, err)

	escRes, err := Normalize(EventPush, "d-worst-escape", escRaw, Options{AppSlug: "octo-app"})
	require.NoError(t, err)
	assert.True(t, escRes.Handled)
	assert.True(t, escRes.Truncated)
	assert.Equal(t, "true", escRes.Attributes["payload_truncated"])
	assert.LessOrEqual(t, escRes.PayloadBytes, DefaultMaxPayloadBytes)
}

func TestProjectPayload_PushStructuralPropertyBound(t *testing.T) {
	// R1 structural property test: derive every key path emitted by the push
	// projection from a complete baseline push payload (plus non-push decoys
	// action/number), then replace each path one at a time with:
	//   1) an 8,000-key nested object (> 80 KiB),
	//   2) a 100 KB digit json.Number,
	//   3) a 100 KB string.
	// Every mutation must project cleanly with err == nil and len(out) <= 64 KiB.
	baseRaw := []byte(`{
		"ref": "refs/heads/main",
		"before": "0000000000000000000000000000000000000000",
		"after": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"created": false,
		"deleted": false,
		"forced": false,
		"pusher": {"name": "monalisa", "username": "monalisa"},
		"head_commit": {
			"id": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			"tree_id": "1111111111111111111111111111111111111111",
			"distinct": true,
			"message": "feat: baseline",
			"timestamp": "2026-05-01T10:00:00Z",
			"url": "https://github.com/octo-org/octo-repo/commit/a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			"author": {"name": "Monalisa", "username": "monalisa"},
			"committer": {"name": "Monalisa", "username": "monalisa"},
			"added": ["a.go"],
			"removed": ["b.go"],
			"modified": ["c.go"]
		},
		"commits": [{
			"id": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			"tree_id": "1111111111111111111111111111111111111111",
			"distinct": true,
			"message": "feat: baseline",
			"timestamp": "2026-05-01T10:00:00Z",
			"url": "https://github.com/octo-org/octo-repo/commit/a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			"author": {"name": "Monalisa", "username": "monalisa"},
			"committer": {"name": "Monalisa", "username": "monalisa"},
			"added": ["a.go"],
			"removed": ["b.go"],
			"modified": ["c.go"]
		}],
		"repository": {
			"id": 1296269,
			"full_name": "octo-org/octo-repo",
			"name": "octo-repo",
			"private": false,
			"default_branch": "main",
			"html_url": "https://github.com/octo-org/octo-repo",
			"owner": {"id": 100, "login": "octo-org", "type": "Organization"}
		},
		"sender": {"id": 201, "login": "monalisa", "type": "User"},
		"installation": {"id": 987654}
	}`)

	baseRoot, err := decodeJSONObject(baseRaw)
	require.NoError(t, err)
	baseOut, baseTrunc, err := projectDecodedPayload(baseRoot, EventPush, DefaultMaxPayloadBytes)
	require.NoError(t, err)
	assert.False(t, baseTrunc)

	projRoot, err := decodeJSONObject(baseOut)
	require.NoError(t, err)

	// Dynamically collect every emitted path in projRoot so newly added keys are covered automatically.
	var paths [][]string
	var collect func(node any, prefix []string)
	collect = func(node any, prefix []string) {
		switch m := node.(type) {
		case map[string]any:
			for k, v := range m {
				p := append(append([]string(nil), prefix...), k)
				paths = append(paths, p)
				collect(v, p)
			}
		case []any:
			if len(m) > 0 {
				p := append(append([]string(nil), prefix...), "0")
				paths = append(paths, p)
				collect(m[0], p)
			}
		}
	}
	collect(projRoot, nil)
	// Also test non-push top-level decoys that must never pass through on push.
	paths = append(paths, []string{"action"}, []string{"number"})
	require.Greater(t, len(paths), 40, "expected all emitted push projection paths to be discovered")

	hugeObj := make(map[string]any, 8000)
	for i := 0; i < 8000; i++ {
		hugeObj[fmt.Sprintf("k%04d", i)] = "v"
	}
	hugeNum := json.Number(strings.Repeat("9", 100*1024))
	hugeStr := strings.Repeat("x", 100*1024)

	adversarial := []struct {
		name string
		val  any
	}{
		{name: "huge_object", val: hugeObj},
		{name: "100kb_number", val: hugeNum},
		{name: "100kb_string", val: hugeStr},
	}

	setAtPath := func(root map[string]any, path []string, val any) {
		var cur any = root
		for i := 0; i < len(path)-1; i++ {
			seg := path[i]
			switch node := cur.(type) {
			case map[string]any:
				cur = node[seg]
			case []any:
				cur = node[0]
			}
		}
		last := path[len(path)-1]
		switch node := cur.(type) {
		case map[string]any:
			node[last] = val
		case []any:
			node[0] = val
		}
	}

	for _, path := range paths {
		pathLabel := strings.Join(path, ".")
		for _, adv := range adversarial {
			mutated, err := decodeJSONObject(baseRaw)
			require.NoError(t, err)
			setAtPath(mutated, path, adv.val)

			out, _, err := projectDecodedPayload(mutated, EventPush, DefaultMaxPayloadBytes)
			require.NoErrorf(t, err, "path=%s variant=%s must not fail push projection", pathLabel, adv.name)
			assert.LessOrEqualf(t, len(out), DefaultMaxPayloadBytes, "path=%s variant=%s exceeded 64 KiB", pathLabel, adv.name)
		}
	}
}
