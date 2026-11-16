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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize_UnsupportedFamiliesNotHandled(t *testing.T) {
	for _, fam := range []string{
		"push",
		"ping",
		"installation",
		"installation_repositories",
		"check_run",
		"workflow_run",
		"",
		"PULL_REQUEST",
	} {
		t.Run(fam, func(t *testing.T) {
			res, err := Normalize(fam, "deliv-1", []byte(`not-even-json`), Options{AppSlug: "scion"})
			require.NoError(t, err)
			assert.False(t, res.Handled)
		})
	}
}

func TestNormalize_UnlistedActionOnKnownFamily(t *testing.T) {
	payload := []byte(`{
		"action": "locked",
		"number": 42,
		"installation": {"id": 9901},
		"repository": {
			"id": 1001,
			"name": "octo-repo",
			"full_name": "octo-org/octo-repo",
			"private": true,
			"default_branch": "main",
			"html_url": "https://github.com/octo-org/octo-repo",
			"owner": {"login": "octo-org", "id": 500, "type": "Organization"}
		},
		"sender": {"login": "maintainer", "id": 700, "type": "User"},
		"pull_request": {
			"number": 42,
			"state": "open",
			"title": "Add widget",
			"body": "Full PR description stays in payload only",
			"html_url": "https://github.com/octo-org/octo-repo/pull/42",
			"author_association": "MEMBER",
			"draft": false,
			"merged": false,
			"created_at": "2026-05-01T10:00:00Z",
			"updated_at": "2026-05-01T11:30:00Z",
			"user": {"login": "pr-author", "id": 701, "type": "User"},
			"base": {
				"ref": "main",
				"sha": "1111111111111111111111111111111111111111",
				"repo": {"id": 1001, "full_name": "octo-org/octo-repo", "name": "octo-repo", "private": true}
			},
			"head": {
				"ref": "feat/widget",
				"sha": "2222222222222222222222222222222222222222",
				"repo": {"id": 1001, "full_name": "octo-org/octo-repo", "name": "octo-repo", "private": true}
			},
			"labels": [{"name": "area:widgets"}],
			"assignees": [],
			"requested_reviewers": [],
			"requested_teams": []
		}
	}`)

	res, err := Normalize(EventPullRequest, "deliv-locked-1", payload, Options{
		AppSlug:    "scion",
		ReceivedAt: time.Date(2026, 5, 1, 11, 31, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.True(t, res.Handled)
	assert.Equal(t, SourceKind, res.SourceKind)
	assert.Equal(t, "9901", res.SourceID)
	assert.Equal(t, "github.pull_request.locked", res.Type)
	assert.Equal(t, "github:octo-org/octo-repo#42", res.Subject)
	assert.Equal(t, time.Date(2026, 5, 1, 11, 30, 0, 0, time.UTC), res.OccurredAt)
	assert.Equal(t, "locked", res.Attributes["action"])
	assert.Equal(t, "feat/widget", res.Attributes["head_ref"])
	assert.Equal(t, "false", res.Attributes["is_fork"])
	_, hasBody := res.Attributes["body"]
	assert.False(t, hasBody, "body must be absent on pull_request actions other than opened/edited")
	_, hasBranch := res.Attributes["branch"]
	assert.False(t, hasBranch, "branch attribute must never exist")
	_, hasListsTrunc := res.Attributes["lists_truncated"]
	assert.False(t, hasListsTrunc, "lists_truncated must be absent when lists fit within 8 KiB")
	assert.Equal(t, []string{"area:widgets"}, res.Lists["labels"])
}

func TestNormalize_PullRequestForkAndDeletedFork(t *testing.T) {
	buildPR := func(headRepoJSON string) []byte {
		return []byte(fmt.Sprintf(`{
			"action": "opened",
			"number": 17,
			"installation": {"id": 4455},
			"repository": {
				"id": 100,
				"name": "Octo-Repo",
				"full_name": "Octo-Org/Octo-Repo",
				"private": false,
				"default_branch": "main",
				"html_url": "https://github.com/Octo-Org/Octo-Repo",
				"owner": {"login": "Octo-Org", "id": 10, "type": "Organization"}
			},
			"sender": {"login": "ext-dev", "id": 20, "type": "User"},
			"pull_request": {
				"number": 17,
				"state": "open",
				"title": "External contribution",
				"body": "Fixes edge case",
				"html_url": "https://github.com/Octo-Org/Octo-Repo/pull/17",
				"author_association": "FIRST_TIME_CONTRIBUTOR",
				"draft": false,
				"merged": false,
				"mergeable_state": "clean",
				"changed_files": 2,
				"additions": 14,
				"deletions": 3,
				"commits": 1,
				"created_at": "2026-05-01T12:00:00Z",
				"updated_at": "2026-05-01T12:00:00Z",
				"user": {"login": "ext-dev", "id": 20, "type": "User"},
				"base": {
					"ref": "main",
					"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"repo": {"id": 100, "full_name": "Octo-Org/Octo-Repo", "name": "Octo-Repo", "private": false}
				},
				"head": {
					"ref": "fix-edge-case",
					"sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					"repo": %s
				},
				"labels": [],
				"assignees": [],
				"requested_reviewers": [],
				"requested_teams": []
			}
		}`, headRepoJSON))
	}

	t.Run("same repo case-insensitive", func(t *testing.T) {
		res, err := Normalize(EventPullRequest, "d-same", buildPR(`{"id": 100, "full_name": "octo-org/octo-repo", "name": "octo-repo", "private": false}`), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.Equal(t, "false", res.Attributes["is_fork"])
		assert.Equal(t, "octo-org/octo-repo", res.Attributes["head_repo"])
		assert.Equal(t, "fix-edge-case", res.Attributes["head_ref"])
		assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", res.Attributes["head_sha"])
		_, hasBranch := res.Attributes["branch"]
		assert.False(t, hasBranch)
	})

	t.Run("live fork repo", func(t *testing.T) {
		res, err := Normalize(EventPullRequest, "d-fork", buildPR(`{"id": 999, "full_name": "ext-dev/Octo-Repo", "name": "Octo-Repo", "private": false}`), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.Equal(t, "true", res.Attributes["is_fork"])
		assert.Equal(t, "ext-dev/Octo-Repo", res.Attributes["head_repo"])
		assert.Equal(t, "fix-edge-case", res.Attributes["head_ref"])
		assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", res.Attributes["head_sha"])
		_, hasBranch := res.Attributes["branch"]
		assert.False(t, hasBranch)
	})

	t.Run("deleted fork head.repo null", func(t *testing.T) {
		res, err := Normalize(EventPullRequest, "d-delfork", buildPR(`null`), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.Equal(t, "true", res.Attributes["is_fork"])
		headRepo, exists := res.Attributes["head_repo"]
		assert.True(t, exists, "head_repo must be present with empty value when fork was deleted")
		assert.Equal(t, "", headRepo)
		assert.Equal(t, "fix-edge-case", res.Attributes["head_ref"])
		_, hasBranch := res.Attributes["branch"]
		assert.False(t, hasBranch)
	})
}

func TestNormalize_IssueCommentOnIssueVsPullRequest(t *testing.T) {
	buildComment := func(prField string) []byte {
		return []byte(fmt.Sprintf(`{
			"action": "created",
			"installation": {"id": 77},
			"repository": {
				"id": 200,
				"name": "scion",
				"full_name": "acme/scion",
				"private": false,
				"default_branch": "main",
				"html_url": "https://github.com/acme/scion",
				"owner": {"login": "acme", "id": 1, "type": "Organization"}
			},
			"sender": {"login": "reviewer-alice", "id": 302, "type": "User"},
			"issue": {
				"number": 88,
				"state": "open",
				"title": "Support GitHub triggers",
				"body": "Original issue body",
				"html_url": "https://github.com/acme/scion/issues/88",
				"author_association": "OWNER",
				"created_at": "2026-05-01T09:00:00Z",
				"updated_at": "2026-05-01T10:00:00Z",
				"user": {"login": "issue-bob", "id": 301, "type": "User"},
				"labels": [{"name": "enhancement"}],
				"assignees": [{"login": "issue-bob"}]%s
			},
			"comment": {
				"id": 555001,
				"body": "Thanks! @scion /review focus on edge cases",
				"html_url": "https://github.com/acme/scion/issues/88#issuecomment-555001",
				"author_association": "COLLABORATOR",
				"created_at": "2026-05-01T10:05:00Z",
				"updated_at": "2026-05-01T10:05:00Z",
				"user": {"login": "reviewer-alice", "id": 302, "type": "User"}
			}
		}`, prField))
	}

	t.Run("plain issue comment", func(t *testing.T) {
		res, err := Normalize(EventIssueComment, "d-ic-issue", buildComment(""), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.True(t, res.Handled)
		assert.Equal(t, "false", res.Attributes["is_pull_request"])
		assert.Equal(t, "COLLABORATOR", res.Attributes["author_association"], "must reflect commenter's association, not issue author's")
		assert.Equal(t, "issue-bob", res.Attributes["author"])
		assert.Equal(t, "reviewer-alice", res.Attributes["sender"])
		assert.Equal(t, "555001", res.Attributes["comment_id"])
		assert.Equal(t, "https://github.com/acme/scion/issues/88#issuecomment-555001", res.Attributes["comment_url"])
		assert.Equal(t, "https://github.com/acme/scion/issues/88#issuecomment-555001", res.Attributes["html_url"])
		assert.Equal(t, "https://github.com/acme/scion/issues/88", res.Attributes["issue_url"])
		_, hasPRURL := res.Attributes["pull_request_url"]
		assert.False(t, hasPRURL)
		assert.Equal(t, "review", res.Attributes["command"])
		assert.Equal(t, "focus on edge cases", res.Attributes["command_args"])
		assert.Equal(t, []string{"enhancement"}, res.Lists["labels"])
		assert.Equal(t, []string{"issue-bob"}, res.Lists["assignees"])
	})

	t.Run("PR conversation comment", func(t *testing.T) {
		res, err := Normalize(EventIssueComment, "d-ic-pr", buildComment(`,"pull_request": {"html_url": "https://github.com/acme/scion/pull/88"}`), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.Equal(t, "true", res.Attributes["is_pull_request"])
		assert.Equal(t, "https://github.com/acme/scion/pull/88", res.Attributes["pull_request_url"])
		_, hasHeadRef := res.Attributes["head_ref"]
		assert.False(t, hasHeadRef, "PR branch facts on issue_comment arrive later via §5.5 enrichment")
		_, hasEnriched := res.Attributes[AttrEnriched]
		assert.False(t, hasEnriched)
	})
}

func TestNormalize_BodyAttributeActionRules(t *testing.T) {
	prPayload := func(action string) []byte {
		return []byte(fmt.Sprintf(`{
			"action": %q,
			"number": 5,
			"installation": {"id": 12},
			"repository": {"id": 1, "name": "r", "full_name": "o/r", "private": false, "owner": {"login": "o", "id": 1, "type": "User"}},
			"sender": {"login": "u", "id": 2, "type": "User"},
			"pull_request": {
				"number": 5, "state": "open", "title": "T", "body": "PR description",
				"html_url": "https://github.com/o/r/pull/5", "author_association": "OWNER",
				"draft": false, "merged": false,
				"created_at": "2026-05-01T12:00:00Z", "updated_at": "2026-05-01T12:00:00Z",
				"user": {"login": "u", "id": 2, "type": "User"},
				"base": {"ref": "main", "sha": "1111", "repo": {"id": 1, "full_name": "o/r", "name": "r", "private": false}},
				"head": {"ref": "dev", "sha": "2222", "repo": {"id": 1, "full_name": "o/r", "name": "r", "private": false}}
			}
		}`, action))
	}

	for _, action := range []string{"opened", "edited"} {
		res, err := Normalize(EventPullRequest, "d-"+action, prPayload(action), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.Equal(t, "PR description", res.Attributes["body"], "action %s should include body", action)
	}
	for _, action := range []string{"synchronize", "closed", "reopened", "labeled", "ready_for_review"} {
		res, err := Normalize(EventPullRequest, "d-"+action, prPayload(action), Options{AppSlug: "scion"})
		require.NoError(t, err)
		_, ok := res.Attributes["body"]
		assert.False(t, ok, "action %s must omit attributes.body", action)
		assert.Contains(t, string(res.Payload), "PR description", "projected payload still keeps body")
	}

	issuePayload := func(action string) []byte {
		return []byte(fmt.Sprintf(`{
			"action": %q,
			"installation": {"id": 12},
			"repository": {"id": 1, "name": "r", "full_name": "o/r", "private": false, "owner": {"login": "o", "id": 1, "type": "User"}},
			"sender": {"login": "u", "id": 2, "type": "User"},
			"issue": {
				"number": 9, "state": "open", "title": "Issue T", "body": "Issue description",
				"html_url": "https://github.com/o/r/issues/9", "author_association": "MEMBER",
				"created_at": "2026-05-01T12:00:00Z", "updated_at": "2026-05-01T12:00:00Z",
				"user": {"login": "u", "id": 2, "type": "User"}
			}
		}`, action))
	}

	for _, action := range []string{"opened", "edited"} {
		res, err := Normalize(EventIssues, "di-"+action, issuePayload(action), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.Equal(t, "Issue description", res.Attributes["body"])
	}
	for _, action := range []string{"closed", "reopened", "labeled", "assigned"} {
		res, err := Normalize(EventIssues, "di-"+action, issuePayload(action), Options{AppSlug: "scion"})
		require.NoError(t, err)
		_, ok := res.Attributes["body"]
		assert.False(t, ok, "issues.%s must omit attributes.body", action)
	}
}

func TestNormalize_CommandDetectionBotEditedAndSlugUnavailable(t *testing.T) {
	commentPayload := func(action, senderLogin, senderType, body string) []byte {
		return []byte(fmt.Sprintf(`{
			"action": %q,
			"installation": {"id": 55},
			"repository": {"id": 1, "name": "r", "full_name": "o/r", "private": false, "owner": {"login": "o", "id": 1, "type": "Organization"}},
			"sender": {"login": %q, "id": 99, "type": %q},
			"issue": {
				"number": 3, "state": "open", "title": "T", "body": "B",
				"html_url": "https://github.com/o/r/issues/3", "author_association": "OWNER",
				"created_at": "2026-05-01T12:00:00Z", "updated_at": "2026-05-01T12:00:00Z",
				"user": {"login": "o", "id": 1, "type": "User"}
			},
			"comment": {
				"id": 900, "body": %q,
				"html_url": "https://github.com/o/r/issues/3#issuecomment-900",
				"author_association": "CONTRIBUTOR",
				"created_at": "2026-05-01T12:00:00Z", "updated_at": "2026-05-01T12:10:00Z",
				"user": {"login": %q, "id": 99, "type": %q}
			}
		}`, action, senderLogin, senderType, body, senderLogin, senderType))
	}

	t.Run("edited skips command and mention detection", func(t *testing.T) {
		res, err := Normalize(EventIssueComment, "d-edit", commentPayload("edited", "alice", "User", "@scion /review now"), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.Equal(t, "@scion /review now", res.Attributes["body"], "edited comment still populates body")
		_, hasMention := res.Attributes["mention"]
		_, hasCmd := res.Attributes["command"]
		_, hasUnavailable := res.Attributes["mention_detection"]
		assert.False(t, hasMention)
		assert.False(t, hasCmd)
		assert.False(t, hasUnavailable)
	})

	t.Run("slug unavailable sets mention_detection=unavailable on creation action only", func(t *testing.T) {
		res, err := Normalize(EventIssueComment, "d-noslug", commentPayload("created", "alice", "User", "@scion /review"), Options{})
		require.NoError(t, err)
		assert.Equal(t, "unavailable", res.Attributes["mention_detection"])
		_, hasCmd := res.Attributes["command"]
		assert.False(t, hasCmd)

		resEdit, err := Normalize(EventIssueComment, "d-noslug-edit", commentPayload("edited", "alice", "User", "@scion /review"), Options{})
		require.NoError(t, err)
		_, hasUnavailable := resEdit.Attributes["mention_detection"]
		assert.False(t, hasUnavailable)
	})

	t.Run("non-self Bot sender parses commands on creation action", func(t *testing.T) {
		res, err := Normalize(EventIssueComment, "d-bot", commentPayload("created", "helper-ci[bot]", "Bot", "@scion /validate --fast"), Options{AppSlug: "scion-app", MentionAliases: []string{"scion"}})
		require.NoError(t, err)
		assert.Equal(t, "Bot", res.SenderType)
		assert.Equal(t, "Bot", res.Attributes["sender_type"])
		assert.False(t, res.IsOwnApp("scion-app"))
		assert.Equal(t, "true", res.Attributes["mention"])
		assert.Equal(t, "validate", res.Attributes["command"])
		assert.Equal(t, "--fast", res.Attributes["command_args"])
	})

	t.Run("own-App Bot sender flagged by IsOwnApp", func(t *testing.T) {
		res, err := Normalize(EventIssueComment, "d-self", commentPayload("created", "Scion-App[bot]", "Bot", "Started agent"), Options{AppSlug: "scion-app"})
		require.NoError(t, err)
		assert.True(t, res.IsOwnApp("scion-app"))
	})

	t.Run("mention without command sets mention=true only", func(t *testing.T) {
		res, err := Normalize(EventIssueComment, "d-mention", commentPayload("created", "alice", "User", "Hey @scion thanks for the help!"), Options{AppSlug: "scion"})
		require.NoError(t, err)
		assert.Equal(t, "true", res.Attributes["mention"])
		_, hasCmd := res.Attributes["command"]
		_, hasArgs := res.Attributes["command_args"]
		_, hasLine := res.Attributes["command_line"]
		assert.False(t, hasCmd)
		assert.False(t, hasArgs)
		assert.False(t, hasLine)
	})
}

func TestNormalize_FieldBoundsAndCeilings(t *testing.T) {
	// Multi-byte rune '🚀' is 4 bytes; test rune-boundary truncation across title, body, diff_hunk, payload, and 8 KiB ceilings.
	longTitle := strings.Repeat("🚀", 200)                                                                              // 800 bytes > 512
	longHunk := strings.Repeat("🔍", 350)                                                                               // 1400 bytes > 1024
	longBody := "@scion /fix " + strings.Repeat("🔥", 2000) + "\n" + strings.Repeat("line of review context 🚀\n", 3000) // > 64 KiB

	var labelsJSON []string
	for i := 0; i < 400; i++ {
		labelsJSON = append(labelsJSON, fmt.Sprintf(`{"name": "very-long-label-name-%03d-padding"}`, i))
	}

	rawObj := map[string]any{
		"action":       "created",
		"installation": map[string]any{"id": 888},
		"repository": map[string]any{
			"id": 10, "name": "repo", "full_name": "org/repo", "private": false,
			"owner": map[string]any{"login": "org", "id": 1, "type": "Organization"},
		},
		"sender": map[string]any{"login": "dev", "id": 2, "type": "User"},
		"pull_request": map[string]any{
			"number": 101, "state": "open", "title": longTitle, "body": "PR desc",
			"html_url": "https://github.com/org/repo/pull/101", "author_association": "MEMBER",
			"draft": false, "merged": false,
			"created_at": "2026-05-01T12:00:00Z", "updated_at": "2026-05-01T12:00:00Z",
			"user": map[string]any{"login": "dev", "id": 2, "type": "User"},
			"base": map[string]any{"ref": "main", "sha": "111", "repo": map[string]any{"id": 10, "full_name": "org/repo", "name": "repo", "private": false}},
			"head": map[string]any{"ref": "feat", "sha": "222", "repo": map[string]any{"id": 10, "full_name": "org/repo", "name": "repo", "private": false}},
		},
		"comment": map[string]any{
			"id": 777, "body": longBody, "diff_hunk": longHunk,
			"path": "pkg/foo.go", "line": 44, "side": "RIGHT", "in_reply_to_id": 776,
			"html_url":           "https://github.com/org/repo/pull/101#discussion_r777",
			"author_association": "MEMBER",
			"created_at":         "2026-05-01T12:01:00Z", "updated_at": "2026-05-01T12:01:00Z",
			"user": map[string]any{"login": "dev", "id": 2, "type": "User"},
		},
	}
	// Inject 400 labels into pull_request to exceed 8 KiB lists if uncapped.
	var parsedLabels []any
	require.NoError(t, json.Unmarshal([]byte("["+strings.Join(labelsJSON, ",")+"]"), &parsedLabels))
	rawObj["pull_request"].(map[string]any)["labels"] = parsedLabels

	payload, err := json.Marshal(rawObj)
	require.NoError(t, err)
	require.Greater(t, len(payload), DefaultMaxPayloadBytes)

	res, err := Normalize(EventPullRequestReviewComment, "d-oversize", payload, Options{AppSlug: "scion"})
	require.NoError(t, err)
	assert.True(t, res.Handled)
	assert.True(t, res.Truncated)
	assert.Equal(t, "true", res.Attributes["payload_truncated"])
	assert.LessOrEqual(t, res.PayloadBytes, DefaultMaxPayloadBytes)
	assert.Equal(t, len(res.Payload), res.PayloadBytes)
	assert.True(t, json.Valid(res.Payload))

	assert.LessOrEqual(t, len(res.Attributes["title"]), MaxTitleBytes)
	assert.True(t, utf8.ValidString(res.Attributes["title"]))
	assert.LessOrEqual(t, len(res.Attributes["body"]), MaxBodyBytes)
	assert.True(t, utf8.ValidString(res.Attributes["body"]))
	assert.LessOrEqual(t, len(res.Attributes["diff_hunk"]), MaxDiffHunkBytes)
	assert.True(t, utf8.ValidString(res.Attributes["diff_hunk"]))
	assert.LessOrEqual(t, len(res.Attributes["command_args"]), MaxCommandArgsBytes)
	assert.True(t, utf8.ValidString(res.Attributes["command_args"]))
	assert.LessOrEqual(t, len(res.Attributes["command_line"]), MaxCommandLineBytes)
	assert.True(t, utf8.ValidString(res.Attributes["command_line"]))
	assert.Equal(t, "776", res.Attributes["in_reply_to"])
	assert.Equal(t, "44", res.Attributes["line"])

	attrBytes, err := encodeCompactJSON(res.Attributes)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(attrBytes), DefaultMaxAttributesBytes)

	listBytes, err := encodeCompactJSON(res.Lists)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(listBytes), DefaultMaxListsBytes)
	assert.Equal(t, "true", res.Attributes["lists_truncated"], "dropping list entries to fit 8 KiB must flag lists_truncated=true")
}

func TestNormalize_OccurredAtStaleAndHelpers(t *testing.T) {
	payload := []byte(`{
		"action": "submitted",
		"installation": {"id": 321},
		"repository": {"id": 1, "name": "r", "full_name": "o/r", "private": false, "owner": {"login": "o", "id": 1, "type": "User"}},
		"sender": {"login": "rev", "id": 9, "type": "User"},
		"pull_request": {
			"number": 12, "state": "open", "title": "PR", "body": null,
			"html_url": "https://github.com/o/r/pull/12",
			"draft": false, "merged_at": null,
			"created_at": "2026-05-01T08:00:00Z", "updated_at": "2026-05-01T09:00:00Z",
			"user": {"login": "author", "id": 8, "type": "User"},
			"base": {"ref": "main", "sha": "111", "repo": {"id": 1, "full_name": "o/r", "name": "r", "private": false}},
			"head": {"ref": "feat", "sha": "222", "repo": {"id": 1, "full_name": "o/r", "name": "r", "private": false}}
		},
		"review": {
			"id": 404,
			"body": null,
			"state": "CHANGES_REQUESTED",
			"html_url": "https://github.com/o/r/pull/12#pullrequestreview-404",
			"submitted_at": "2026-05-01T10:00:00Z"
		}
	}`)

	// Received 25 hours after submitted_at -> stale = "true".
	resStale, err := Normalize(EventPullRequestReview, "deliv-rev-1", payload, Options{
		AppSlug:    "scion",
		ReceivedAt: time.Date(2026, 5, 2, 11, 0, 1, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC), resStale.OccurredAt)
	assert.Equal(t, "true", resStale.Attributes["stale"])
	assert.Equal(t, "changes_requested", resStale.Attributes["review_state"])
	assert.Equal(t, "NONE", resStale.Attributes["author_association"], "missing author_association defaults to NONE")
	assert.Equal(t, "deliv-rev-1/proj-99", resStale.DedupKey("proj-99"))
	assert.Equal(t, "", resStale.DedupKey(""))

	// Received 1 hour after submitted_at -> stale absent.
	resFresh, err := Normalize(EventPullRequestReview, "deliv-rev-2", payload, Options{
		AppSlug:    "scion",
		ReceivedAt: time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	_, hasStale := resFresh.Attributes["stale"]
	assert.False(t, hasStale)

	// FilterHeaders allow-list check.
	hdr := make(http.Header)
	hdr.Set("X-GitHub-Event", "pull_request_review")
	hdr.Set("X-GitHub-Delivery", "deliv-rev-2")
	hdr.Set("X-Hub-Signature-256", "sha256=secret-must-not-be-copied")
	hdr.Set("User-Agent", "GitHub-Hookshot/abc")
	filtered := FilterHeaders(hdr)
	assert.Equal(t, map[string]string{
		"X-GitHub-Event":    "pull_request_review",
		"X-GitHub-Delivery": "deliv-rev-2",
		"User-Agent":        "GitHub-Hookshot/abc",
	}, filtered)
}

func TestNormalize_DiffHunkKeepsTailAfterNewline(t *testing.T) {
	buildReviewComment := func(hunk string) []byte {
		raw, err := json.Marshal(map[string]any{
			"action":       "created",
			"installation": map[string]any{"id": 10},
			"repository": map[string]any{
				"id": 1, "name": "r", "full_name": "o/r", "private": false,
				"owner": map[string]any{"login": "o", "id": 1, "type": "User"},
			},
			"sender": map[string]any{"login": "u", "id": 2, "type": "User"},
			"pull_request": map[string]any{
				"number": 7, "state": "open", "title": "PR", "body": "",
				"html_url": "https://github.com/o/r/pull/7", "author_association": "MEMBER",
				"draft": false, "merged": false,
				"created_at": "2026-05-01T12:00:00Z", "updated_at": "2026-05-01T12:00:00Z",
				"user": map[string]any{"login": "u", "id": 2, "type": "User"},
				"base": map[string]any{"ref": "main", "sha": "111", "repo": map[string]any{"id": 1, "full_name": "o/r", "name": "r", "private": false}},
				"head": map[string]any{"ref": "feat", "sha": "222", "repo": map[string]any{"id": 1, "full_name": "o/r", "name": "r", "private": false}},
			},
			"comment": map[string]any{
				"id": 500, "body": "Check nil here", "diff_hunk": hunk,
				"path": "pkg/foo.go", "line": 99, "side": "RIGHT",
				"html_url":           "https://github.com/o/r/pull/7#discussion_r500",
				"author_association": "MEMBER",
				"created_at":         "2026-05-01T12:01:00Z", "updated_at": "2026-05-01T12:01:00Z",
			},
		})
		require.NoError(t, err)
		return raw
	}

	t.Run("multi-line hunk cuts cleanly after newline and preserves commented tail", func(t *testing.T) {
		var lines []string
		lines = append(lines, "@@ -10,40 +10,45 @@ func hugeContext() {")
		for i := 0; i < 30; i++ {
			lines = append(lines, fmt.Sprintf(" \tearlierContextLine(%02d, %q)", i, "🚀-padding-context"))
		}
		targetLine := "+\treturn commentedTargetLine(ctx) // <--- review comment refers to this line"
		lines = append(lines, targetLine)
		hunk := strings.Join(lines, "\n")
		require.Greater(t, len(hunk), MaxDiffHunkBytes)

		res, err := Normalize(EventPullRequestReviewComment, "d-hunk-1", buildReviewComment(hunk), Options{AppSlug: "scion"})
		require.NoError(t, err)
		got := res.Attributes["diff_hunk"]
		assert.LessOrEqual(t, len(got), MaxDiffHunkBytes)
		assert.True(t, utf8.ValidString(got))
		assert.True(t, strings.HasSuffix(got, targetLine), "tail with commented line must be preserved, got: %q", got)
		assert.True(t, strings.HasPrefix(got, " \tearlierContextLine(") || strings.HasPrefix(got, targetLine),
			"tail should start cleanly at a line boundary after newline, got: %q", got)
	})

	t.Run("single giant line with trailing newline still keeps rune-safe tail", func(t *testing.T) {
		singleLine := "+" + strings.Repeat("🚀", 300) + "END_MARKER\n"
		require.Greater(t, len(singleLine), MaxDiffHunkBytes)

		res, err := Normalize(EventPullRequestReviewComment, "d-hunk-2", buildReviewComment(singleLine), Options{AppSlug: "scion"})
		require.NoError(t, err)
		got := res.Attributes["diff_hunk"]
		assert.NotEmpty(t, got)
		assert.LessOrEqual(t, len(got), MaxDiffHunkBytes)
		assert.True(t, utf8.ValidString(got))
		assert.True(t, strings.HasSuffix(got, "END_MARKER\n"))
	})
}

func TestNormalize_LastResortAttributeCeilingDeletesInsteadOfCorrupting(t *testing.T) {
	// Pathological payload with empty prose fields but huge identifier attributes
	// (6 KiB head_ref + 3 KiB label > 8 KiB). Last-resort shrinking must delete the
	// oversize unprotected key rather than leaving a silently truncated ref/label prefix.
	hugeHeadRef := "feat/" + strings.Repeat("a", 6000)
	bigLabel := "lbl-" + strings.Repeat("b", 3000)

	raw, err := json.Marshal(map[string]any{
		"action":       "labeled",
		"installation": map[string]any{"id": 10},
		"repository": map[string]any{
			"id": 1, "name": "r", "full_name": "o/r", "private": false,
			"owner": map[string]any{"login": "o", "id": 1, "type": "User"},
		},
		"sender": map[string]any{"login": "u", "id": 2, "type": "User"},
		"label":  map[string]any{"name": bigLabel},
		"pull_request": map[string]any{
			"number": 7, "state": "open", "title": "", "body": "",
			"html_url": "https://github.com/o/r/pull/7", "author_association": "MEMBER",
			"draft": false, "merged": false,
			"created_at": "2026-05-01T12:00:00Z", "updated_at": "2026-05-01T12:00:00Z",
			"user": map[string]any{"login": "u", "id": 2, "type": "User"},
			"base": map[string]any{"ref": "main", "sha": "111", "repo": map[string]any{"id": 1, "full_name": "o/r", "name": "r", "private": false}},
			"head": map[string]any{"ref": hugeHeadRef, "sha": "222", "repo": map[string]any{"id": 1, "full_name": "o/r", "name": "r", "private": false}},
		},
	})
	require.NoError(t, err)

	res, err := Normalize(EventPullRequest, "d-ceil-del", raw, Options{AppSlug: "scion"})
	require.NoError(t, err)

	_, hasHeadRef := res.Attributes["head_ref"]
	assert.False(t, hasHeadRef, "oversize identifier key must be deleted rather than truncated to a wrong ref prefix")
	assert.Equal(t, bigLabel, res.Attributes["label"], "remaining identifier key that fits must stay whole")

	attrBytes, err := encodeCompactJSON(res.Attributes)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(attrBytes), DefaultMaxAttributesBytes)
}
