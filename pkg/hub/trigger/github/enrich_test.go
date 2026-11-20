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
	"errors"
	"strings"
	"testing"
)

func TestNeedsPullRequestEnrichment(t *testing.T) {
	cases := []struct {
		name       string
		sourceKind string
		attrs      map[string]string
		want       bool
	}{
		{
			name:       "github issue_comment on PR without enriched",
			sourceKind: SourceKind,
			attrs:      map[string]string{"event": EventIssueComment, "is_pull_request": "true"},
			want:       true,
		},
		{
			name:       "already enriched true",
			sourceKind: SourceKind,
			attrs:      map[string]string{"event": EventIssueComment, "is_pull_request": "true", AttrEnriched: EnrichedTrue},
			want:       false,
		},
		{
			name:       "already enriched failed",
			sourceKind: SourceKind,
			attrs:      map[string]string{"event": EventIssueComment, "is_pull_request": "true", AttrEnriched: EnrichedFailed},
			want:       false,
		},
		{
			name:       "issue_comment on plain issue",
			sourceKind: SourceKind,
			attrs:      map[string]string{"event": EventIssueComment, "is_pull_request": "false"},
			want:       false,
		},
		{
			name:       "pull_request_review needs no enrichment",
			sourceKind: SourceKind,
			attrs:      map[string]string{"event": EventPullRequestReview, "is_pull_request": "true"},
			want:       false,
		},
		{
			name:       "non-github source",
			sourceKind: "webhook",
			attrs:      map[string]string{"event": EventIssueComment, "is_pull_request": "true"},
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsPullRequestEnrichment(tc.sourceKind, tc.attrs); got != tc.want {
				t.Errorf("NeedsPullRequestEnrichment = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyPullRequestEnrichment_SameRepoAndFork(t *testing.T) {
	baseAttrs := map[string]string{
		"provider":        "github",
		"repo":            "octo-org/octo-repo",
		"event":           "issue_comment",
		"action":          "created",
		"is_pull_request": "true",
		"number":          "42",
		"body":            "@octo-app /review",
	}
	baseLists := map[string][]string{
		"labels":    {"old-label"},
		"assignees": {"alice"},
	}

	t.Run("same repo PR", func(t *testing.T) {
		prJSON := []byte(`{
			"number": 42,
			"state": "open",
			"draft": false,
			"merged": false,
			"mergeable_state": "clean",
			"base": {"ref": "main", "sha": "base111"},
			"head": {"ref": "feat/enrich", "sha": "head222", "repo": {"full_name": "Octo-Org/octo-repo"}},
			"labels": [{"name": "needs-review"}, {"name": "area:triggers"}],
			"requested_reviewers": [{"login": "bob"}],
			"requested_teams": [{"slug": "core-reviewers"}]
		}`)
		attrs, lists, err := ApplyPullRequestEnrichment(baseAttrs, baseLists, prJSON)
		if err != nil {
			t.Fatalf("ApplyPullRequestEnrichment: %v", err)
		}
		// Must not mutate input maps.
		if _, leaked := baseAttrs[AttrEnriched]; leaked {
			t.Fatal("input attrs was mutated")
		}
		wantAttrs := map[string]string{
			"base_ref":        "main",
			"base_sha":        "base111",
			"head_ref":        "feat/enrich",
			"head_sha":        "head222",
			"head_repo":       "Octo-Org/octo-repo",
			"is_fork":         "false",
			"draft":           "false",
			"merged":          "false",
			"state":           "open",
			"mergeable_state": "clean",
			AttrEnriched:      EnrichedTrue,
		}
		for k, want := range wantAttrs {
			if got := attrs[k]; got != want {
				t.Errorf("attrs[%q] = %q, want %q", k, got, want)
			}
		}
		if len(lists["labels"]) != 2 || lists["labels"][0] != "needs-review" {
			t.Errorf("labels = %v", lists["labels"])
		}
		if len(lists["requested_reviewers"]) != 1 || lists["requested_reviewers"][0] != "bob" {
			t.Errorf("requested_reviewers = %v", lists["requested_reviewers"])
		}
		if len(lists["requested_teams"]) != 1 || lists["requested_teams"][0] != "core-reviewers" {
			t.Errorf("requested_teams = %v", lists["requested_teams"])
		}
		if len(lists["assignees"]) != 1 || lists["assignees"][0] != "alice" {
			t.Errorf("assignees should be preserved, got %v", lists["assignees"])
		}
		if len(lists[ListEnriched]) != 10 {
			t.Errorf("lists.enriched = %v, want 10 fetched keys", lists[ListEnriched])
		}
	})

	t.Run("fork PR and deleted fork repo", func(t *testing.T) {
		forkJSON := []byte(`{
			"state": "open",
			"draft": true,
			"merged": false,
			"base": {"ref": "main", "sha": "base111"},
			"head": {"ref": "patch-1", "sha": "head333", "repo": null}
		}`)
		attrs, lists, err := ApplyPullRequestEnrichment(baseAttrs, baseLists, forkJSON)
		if err != nil {
			t.Fatalf("ApplyPullRequestEnrichment: %v", err)
		}
		if attrs["is_fork"] != "true" || attrs["head_repo"] != "" || attrs["draft"] != "true" || attrs[AttrEnriched] != EnrichedTrue {
			t.Errorf("unexpected fork attrs: %+v", attrs)
		}
		if len(lists[ListEnriched]) != 9 {
			t.Errorf("lists.enriched = %v, want 9 keys (without mergeable_state)", lists[ListEnriched])
		}
	})
}

func TestApplyPullRequestEnrichment_Ceilings(t *testing.T) {
	prJSON := []byte(`{
		"state": "open",
		"draft": false,
		"merged": false,
		"base": {"ref": "main", "sha": "0123456789abcdef0123456789abcdef01234567"},
		"head": {"ref": "feature/very-important", "sha": "fedcba9876543210fedcba9876543210fedcba98", "repo": {"full_name": "octo-org/octo-repo"}}
	}`)

	t.Run("shortens body first and preserves all enriched keys within 8 KiB", func(t *testing.T) {
		nearCeiling := map[string]string{
			"provider":        "github",
			"repo":            "octo-org/octo-repo",
			"event":           "issue_comment",
			"action":          "created",
			"is_pull_request": "true",
			"number":          "42",
			"body":            strings.Repeat("x", DefaultMaxAttributesBytes-200),
		}
		attrs, _, err := ApplyPullRequestEnrichment(nearCeiling, nil, prJSON)
		if err != nil {
			t.Fatalf("ApplyPullRequestEnrichment: %v", err)
		}
		encoded, err := encodeCompactJSON(attrs)
		if err != nil {
			t.Fatalf("encodeCompactJSON: %v", err)
		}
		if len(encoded) > DefaultMaxAttributesBytes {
			t.Errorf("encoded attrs len = %d > %d", len(encoded), DefaultMaxAttributesBytes)
		}
		if len(attrs["body"]) >= len(nearCeiling["body"]) {
			t.Errorf("expected body to be shortened: before=%d after=%d", len(nearCeiling["body"]), len(attrs["body"]))
		}
		for _, k := range []string{"base_ref", "base_sha", "head_ref", "head_sha", "head_repo", "is_fork", "draft", "merged", "state", AttrEnriched} {
			if attrs[k] == "" {
				t.Errorf("enriched key %q was dropped or empty", k)
			}
		}
	})

	t.Run("protected keys alone exceeding 8 KiB returns ErrAttributesCeilingExceeded", func(t *testing.T) {
		unshrinkable := map[string]string{
			"provider":        "github",
			"repo":            "octo-org/octo-repo",
			"event":           "issue_comment",
			"is_pull_request": "true",
			"html_url":        "https://github.com/" + strings.Repeat("a", DefaultMaxAttributesBytes),
		}
		_, _, err := ApplyPullRequestEnrichment(unshrinkable, nil, prJSON)
		if !errors.Is(err, ErrAttributesCeilingExceeded) {
			t.Fatalf("err = %v, want ErrAttributesCeilingExceeded", err)
		}
		_, _, failErr := ApplyPullRequestEnrichmentFailure(unshrinkable, nil)
		if !errors.Is(failErr, ErrAttributesCeilingExceeded) {
			t.Fatalf("failure err = %v, want ErrAttributesCeilingExceeded", failErr)
		}
	})

	t.Run("lists ceiling truncates labels while preserving lists.enriched", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"state":"open","draft":false,"merged":false,"base":{"ref":"main","sha":"aaa"},"head":{"ref":"f","sha":"bbb","repo":{"full_name":"octo-org/octo-repo"}},"labels":[`)
		for i := 0; i < 400; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"name":"label-` + strings.Repeat("z", 30) + `"}`)
		}
		b.WriteString(`]}`)

		attrs, lists, err := ApplyPullRequestEnrichment(map[string]string{
			"repo":            "octo-org/octo-repo",
			"event":           "issue_comment",
			"is_pull_request": "true",
		}, nil, []byte(b.String()))
		if err != nil {
			t.Fatalf("ApplyPullRequestEnrichment: %v", err)
		}
		if attrs["lists_truncated"] != "true" {
			t.Errorf("expected lists_truncated=true, got %q", attrs["lists_truncated"])
		}
		if len(lists[ListEnriched]) != 9 {
			t.Errorf("lists.enriched was truncated: %v", lists[ListEnriched])
		}
		encodedLists, err := encodeCompactJSON(lists)
		if err != nil {
			t.Fatalf("encodeCompactJSON: %v", err)
		}
		if len(encodedLists) > DefaultMaxListsBytes {
			t.Errorf("encoded lists len = %d > %d", len(encodedLists), DefaultMaxListsBytes)
		}
	})
}
