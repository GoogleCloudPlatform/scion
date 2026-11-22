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
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize_MalformedInputs(t *testing.T) {
	longRepo := "octo/" + strings.Repeat("a", 260)

	cases := []struct {
		name    string
		event   string
		payload string
	}{
		{name: "empty payload", event: EventIssues, payload: ""},
		{name: "whitespace payload", event: EventIssues, payload: "   \n\t "},
		{name: "invalid json", event: EventIssues, payload: `{"action":`},
		{name: "json array", event: EventIssues, payload: `[]`},
		{name: "json null", event: EventIssues, payload: `null`},
		{name: "json string", event: EventIssues, payload: `"hello"`},
		{name: "trailing json object", event: EventIssues, payload: `{}{}`},
		{
			name:  "missing action",
			event: EventIssues,
			payload: `{
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "action with wildcard",
			event: EventIssues,
			payload: `{
				"action": "opened.*",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "missing installation",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "zero installation id",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 0},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "string installation id",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": "123"},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "missing repository",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "repo full_name missing slash",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "noslash"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "repo full_name oversize subject",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "` + longRepo + `"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "repo full_name control char",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "octo/bad\nrepo"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "missing sender",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "empty sender login",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": ""},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "issues missing issue object",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "issues non-positive number",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": -3}
			}`,
		},
		{
			name:  "issue_comment missing comment",
			event: EventIssueComment,
			payload: `{
				"action": "created",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": 1}
			}`,
		},
		{
			name:  "issue_comment missing comment.id",
			event: EventIssueComment,
			payload: `{
				"action": "created",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": 1},
				"comment": {"body": "hi"}
			}`,
		},
		{
			name:  "pull_request missing head",
			event: EventPullRequest,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"pull_request": {
					"number": 1,
					"base": {"ref": "main", "sha": "111"}
				}
			}`,
		},
		{
			name:  "pull_request_review missing review",
			event: EventPullRequestReview,
			payload: `{
				"action": "submitted",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"pull_request": {
					"number": 1,
					"base": {"ref": "main", "sha": "111"},
					"head": {"ref": "feat", "sha": "222"}
				}
			}`,
		},
		{
			name:  "pull_request_review_comment missing comment",
			event: EventPullRequestReviewComment,
			payload: `{
				"action": "created",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"pull_request": {
					"number": 1,
					"base": {"ref": "main", "sha": "111"},
					"head": {"ref": "feat", "sha": "222"}
				}
			}`,
		},
		{
			name:  "malformed RFC3339 timestamp",
			event: EventIssues,
			payload: `{
				"action": "opened",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"},
				"issue": {"number": 1, "updated_at": "not-a-date"}
			}`,
		},
		{
			name:  "push missing ref",
			event: EventPush,
			payload: `{
				"before": "0000000000000000000000000000000000000000",
				"after": "1111111111111111111111111111111111111111",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "push ref with whitespace",
			event: EventPush,
			payload: `{
				"ref": "refs/heads/bad branch",
				"before": "0000000000000000000000000000000000000000",
				"after": "1111111111111111111111111111111111111111",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "push missing before",
			event: EventPush,
			payload: `{
				"ref": "refs/heads/main",
				"after": "1111111111111111111111111111111111111111",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "push missing after",
			event: EventPush,
			payload: `{
				"ref": "refs/heads/main",
				"before": "0000000000000000000000000000000000000000",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "push non-boolean forced",
			event: EventPush,
			payload: `{
				"ref": "refs/heads/main",
				"before": "0000000000000000000000000000000000000000",
				"after": "1111111111111111111111111111111111111111",
				"forced": "yes",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "push non-array commits",
			event: EventPush,
			payload: `{
				"ref": "refs/heads/main",
				"before": "0000000000000000000000000000000000000000",
				"after": "1111111111111111111111111111111111111111",
				"commits": "not-an-array",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "push non-object head_commit",
			event: EventPush,
			payload: `{
				"ref": "refs/heads/main",
				"before": "0000000000000000000000000000000000000000",
				"after": "1111111111111111111111111111111111111111",
				"head_commit": "not-an-object",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "push malformed head_commit timestamp",
			event: EventPush,
			payload: `{
				"ref": "refs/heads/main",
				"before": "0000000000000000000000000000000000000000",
				"after": "1111111111111111111111111111111111111111",
				"head_commit": {"id": "1111", "timestamp": "bad-ts"},
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`,
		},
		{
			name:  "push subject exceeds 256 bytes",
			event: EventPush,
			payload: fmt.Sprintf(`{
				"ref": "refs/heads/%s",
				"before": "0000000000000000000000000000000000000000",
				"after": "1111111111111111111111111111111111111111",
				"installation": {"id": 1},
				"repository": {"full_name": "o/r"},
				"sender": {"login": "u"}
			}`, strings.Repeat("b", MaxSubjectBytes)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Normalize(tc.event, "deliv-bad", []byte(tc.payload), Options{AppSlug: "scion"})
			require.Error(t, err)
			assert.False(t, res.Handled)
		})
	}
}

func FuzzNormalize(f *testing.F) {
	f.Add(EventIssueComment, "d-1", []byte(`{"action":"created","installation":{"id":1},"repository":{"full_name":"o/r"},"sender":{"login":"u"},"issue":{"number":1},"comment":{"id":2,"body":"@scion /review hi"}}`), "scion")
	f.Add(EventPullRequest, "d-2", []byte(`{"action":"opened","installation":{"id":1},"repository":{"full_name":"o/r"},"sender":{"login":"u"},"pull_request":{"number":3,"body":"@scion /plan","base":{"ref":"main","sha":"a"},"head":{"ref":"f","sha":"b","repo":null}}}`), "scion")
	f.Add(EventIssues, "d-3", []byte(`{"action":"labeled","installation":{"id":1},"repository":{"full_name":"o/r"},"sender":{"login":"u"},"issue":{"number":4,"labels":[{"name":"bug"}]},"label":{"name":"bug"}}`), "")
	f.Add("push", "d-4", []byte(`{}`), "scion")
	f.Add(EventPullRequestReviewComment, "d-5", []byte(`{"action":123,"pull_request":"bad"}`), "scion")

	fixedNow := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, eventName, deliveryID string, payload []byte, appSlug string) {
		res, err := Normalize(eventName, deliveryID, payload, Options{
			AppSlug:         appSlug,
			MentionAliases:  []string{"alias-bot"},
			ReceivedAt:      fixedNow,
			MaxPayloadBytes: 4096,
		})
		if err != nil {
			assert.False(t, res.Handled)
			return
		}
		if !res.Handled {
			return
		}

		assert.True(t, utf8.ValidString(res.Type))
		assert.True(t, utf8.ValidString(res.Subject))
		assert.LessOrEqual(t, len(res.Subject), MaxSubjectBytes)
		for _, r := range res.Subject {
			assert.True(t, unicode.IsPrint(r))
		}
		assert.Equal(t, len(res.Payload), res.PayloadBytes)
		assert.LessOrEqual(t, res.PayloadBytes, 4096)
		assert.True(t, json.Valid(res.Payload))

		_, hasBranch := res.Attributes["branch"]
		assert.False(t, hasBranch)
		for k, v := range res.Attributes {
			assert.True(t, utf8.ValidString(k))
			assert.True(t, utf8.ValidString(v))
		}
		attrBytes, encErr := encodeCompactJSON(res.Attributes)
		require.NoError(t, encErr)
		assert.LessOrEqual(t, len(attrBytes), DefaultMaxAttributesBytes)

		listBytes, encErr := encodeCompactJSON(res.Lists)
		require.NoError(t, encErr)
		assert.LessOrEqual(t, len(listBytes), DefaultMaxListsBytes)
	})
}
