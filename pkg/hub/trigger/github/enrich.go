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
	"maps"
	"strconv"
	"strings"
)

// Enrichment attribute values and provenance list key (§5.3, §5.5).
const (
	EnrichedTrue   = "true"
	EnrichedFailed = "failed"
	ListEnriched   = "enriched"
)

// NeedsPullRequestEnrichment reports whether an event requires §5.5 PR
// enrichment: source_kind == "github", event family == "issue_comment",
// is_pull_request == "true", and the enriched attribute is absent.
func NeedsPullRequestEnrichment(sourceKind string, attrs map[string]string) bool {
	if sourceKind != SourceKind || attrs == nil {
		return false
	}
	if attrs["event"] != EventIssueComment {
		return false
	}
	if attrs["is_pull_request"] != "true" {
		return false
	}
	_, already := attrs[AttrEnriched]
	return !already
}

// ApplyPullRequestEnrichment merges pull request facts fetched from
// `GET /repos/{owner}/{repo}/pulls/{number}` into cloned copies of
// existingAttrs and existingLists (§5.3, §5.5).
//
// It sets enriched="true", records every fetched attribute key in
// lists["enriched"], updates labels/requested_reviewers (and requested_teams
// when present), and enforces both the 8 KiB lists ceiling and the 8 KiB
// attributes ceiling (shortening body first, never dropping enriched keys).
// If protected attributes alone exceed 8 KiB, it returns an error wrapping
// ErrAttributesCeilingExceeded.
func ApplyPullRequestEnrichment(
	existingAttrs map[string]string,
	existingLists map[string][]string,
	prBody []byte,
) (map[string]string, map[string][]string, error) {
	prMap, err := decodeJSONObject(prBody)
	if err != nil {
		return nil, nil, err
	}

	baseMap, err := reqMap(prMap, "base", "pull_request.base")
	if err != nil {
		return nil, nil, err
	}
	headMap, err := reqMap(prMap, "head", "pull_request.head")
	if err != nil {
		return nil, nil, err
	}
	baseRef, err := reqString(baseMap, "ref", "pull_request.base.ref")
	if err != nil {
		return nil, nil, err
	}
	baseSHA, err := reqString(baseMap, "sha", "pull_request.base.sha")
	if err != nil {
		return nil, nil, err
	}
	headRef, err := reqString(headMap, "ref", "pull_request.head.ref")
	if err != nil {
		return nil, nil, err
	}
	headSHA, err := reqString(headMap, "sha", "pull_request.head.sha")
	if err != nil {
		return nil, nil, err
	}

	var headRepo string
	if headRepoMap, ok := optMap(headMap, "repo"); ok {
		headRepo = optCleanString(headRepoMap, "full_name")
	}
	repoFullName := ""
	if existingAttrs != nil {
		repoFullName = existingAttrs["repo"]
	}
	isFork := headRepo == "" || !strings.EqualFold(headRepo, repoFullName)

	draft, err := optBool(prMap, "draft", "pull_request.draft")
	if err != nil {
		return nil, nil, err
	}
	merged, err := extractPRMerged(prMap)
	if err != nil {
		return nil, nil, err
	}

	attrs := make(map[string]string, len(existingAttrs)+12)
	maps.Copy(attrs, existingAttrs)

	fetchedKeys := []string{
		"base_ref",
		"base_sha",
		"head_ref",
		"head_sha",
		"head_repo",
		"is_fork",
		"draft",
		"merged",
	}
	attrs["base_ref"] = cleanUTF8(baseRef)
	attrs["base_sha"] = cleanUTF8(baseSHA)
	attrs["head_ref"] = cleanUTF8(headRef)
	attrs["head_sha"] = cleanUTF8(headSHA)
	attrs["head_repo"] = headRepo
	attrs["is_fork"] = strconv.FormatBool(isFork)
	attrs["draft"] = strconv.FormatBool(draft)
	attrs["merged"] = strconv.FormatBool(merged)

	if state := optCleanString(prMap, "state"); state != "" {
		attrs["state"] = state
		fetchedKeys = append(fetchedKeys, "state")
	}
	if mergeableState := optCleanString(prMap, "mergeable_state"); mergeableState != "" {
		attrs["mergeable_state"] = mergeableState
		fetchedKeys = append(fetchedKeys, "mergeable_state")
	}
	attrs[AttrEnriched] = EnrichedTrue

	lists := cloneListsMap(existingLists)
	lists["labels"] = extractObjectStrings(prMap, "labels", "name")
	lists["requested_reviewers"] = extractObjectStrings(prMap, "requested_reviewers", "login")
	if _, hasTeams := prMap["requested_teams"]; hasTeams {
		lists["requested_teams"] = extractObjectStrings(prMap, "requested_teams", "slug")
	}
	lists[ListEnriched] = fetchedKeys

	listsTruncated, err := enforceListsCeiling(lists, DefaultMaxListsBytes)
	if err != nil {
		return nil, nil, err
	}
	if listsTruncated {
		attrs["lists_truncated"] = "true"
	}
	if err := enforceAttributesCeiling(attrs, DefaultMaxAttributesBytes); err != nil {
		return nil, nil, err
	}
	return attrs, lists, nil
}

// ApplyPullRequestEnrichmentFailure marks cloned copies of existingAttrs and
// existingLists with enriched="failed" while enforcing the 8 KiB attributes
// ceiling (§5.5).
func ApplyPullRequestEnrichmentFailure(
	existingAttrs map[string]string,
	existingLists map[string][]string,
) (map[string]string, map[string][]string, error) {
	attrs := make(map[string]string, len(existingAttrs)+1)
	maps.Copy(attrs, existingAttrs)
	attrs[AttrEnriched] = EnrichedFailed
	lists := cloneListsMap(existingLists)
	if err := enforceAttributesCeiling(attrs, DefaultMaxAttributesBytes); err != nil {
		return nil, nil, err
	}
	return attrs, lists, nil
}

func cloneListsMap(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in)+3)
	for k, vals := range in {
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}
