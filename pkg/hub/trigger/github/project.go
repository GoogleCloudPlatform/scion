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
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

var keptPrimaryKeys = []string{
	"pull_request",
	"issue",
	"comment",
	"review",
	"label",
	"assignee",
	"requested_reviewer",
	"requested_team",
	"changes",
}

// decodeJSONObject parses raw as a single JSON object preserving number tokens.
func decodeJSONObject(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) || bytes.ContainsRune(raw, '\x00') {
		return nil, errors.New("github trigger: invalid UTF-8 or NUL byte in webhook payload")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("github trigger: empty webhook payload")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return nil, fmt.Errorf("github trigger: invalid JSON payload: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("github trigger: trailing data after JSON object")
	}
	obj, ok := val.(map[string]any)
	if !ok || obj == nil {
		return nil, errors.New("github trigger: webhook payload must be a JSON object")
	}
	return obj, nil
}

// projectDecodedPayload builds the §5.8 bounded JSON projection from a decoded root object.
//
// For push events (eventName == EventPush), every projected scalar and array is
// bounded by construction via an explicit typed allow-list (capped UTF-8
// strings, int64 parsed from json.Number, or bools; zero sanitizeValue or
// arbitrary JSON pass-through) so a valid push delivery cannot fail projection
// under the 64 KiB (DefaultMaxPayloadBytes) ceiling. payload_truncated is set
// whenever commits[:20] slicing, per-commit file-array reduction (count > 20 or
// path > 1 KiB), any push scalar cap/type-mismatch omission, or a shrink tier
// applies.
//
// Worst-case push projection arithmetic (normal UTF-8, after Tier 4b drops
// commit added/removed/modified file arrays):
//   - Per commit (up to 21 objects: commits[:20] + head_commit):
//     id (64) + tree_id (64) + timestamp (64) + url (256) + message (512) +
//     distinct (5) + author{name(256),username(256)} (512) +
//     committer{name(256),username(256)} (512) + JSON keys/syntax (~190)
//     <= 2,179 bytes/commit -> 21 * 2,179 = 45,759 bytes.
//   - Top-level push fields:
//     ref (1,024) + before (64) + after (64) + created/deleted/forced (~40) +
//     pusher{name(256),username(256)} (~547) <= ~1,750 bytes.
//   - Reduced repository: id (int64 <= 20) + private (5) + full_name (256) +
//     name (256) + default_branch (256) + html_url (256) +
//     owner{id (int64 <= 20), login (256), type (64)} + keys/syntax (~150)
//     <= ~1,540 bytes.
//   - Reduced sender: id (int64 <= 20) + login (256) + type (64) + syntax (~45)
//     <= ~385 bytes.
//   - Reduced installation: id (int64 <= 20) + syntax (~25) <= ~45 bytes.
//   - Total <= ~49,479 bytes (< 65,536 bytes; >16 KiB headroom).
//
// Even under pathological JSON-escape expansion (e.g. control characters
// expanding 6x) or a caller-configured smaller maxBytes, Tier 5 shrinks
// unprotected push strings and Tier 5b shrinks commit/pusher names so Tier 5's
// protected-key set cannot cause a refusal for push.
func projectDecodedPayload(root map[string]any, eventName string, maxBytes int) ([]byte, bool, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPayloadBytes
	}

	isPush := eventName == EventPush
	proj := make(map[string]any)
	reducedTruncated := false

	if isPush {
		if copyCappedString(proj, root, "ref", MaxPushRefBytes) {
			reducedTruncated = true
		}
		for _, shaKey := range []string{"before", "after"} {
			if copyCappedString(proj, root, shaKey, MaxPushSHABytes) {
				reducedTruncated = true
			}
		}
		for _, boolKey := range []string{"created", "deleted", "forced"} {
			if copyBool(proj, root, boolKey) {
				reducedTruncated = true
			}
		}
		if rawHead, exists := root["head_commit"]; exists && rawHead != nil {
			if headMap, ok := rawHead.(map[string]any); ok {
				reduced, cut := reducePushCommit(headMap)
				proj["head_commit"] = reduced
				reducedTruncated = reducedTruncated || cut
			} else {
				reducedTruncated = true
			}
		}
		if rawPusher, exists := root["pusher"]; exists && rawPusher != nil {
			if pusherMap, ok := rawPusher.(map[string]any); ok {
				reduced, cut := reducePushActor(pusherMap)
				proj["pusher"] = reduced
				reducedTruncated = reducedTruncated || cut
			} else {
				reducedTruncated = true
			}
		}
		if rawCommits, exists := root["commits"]; exists && rawCommits != nil {
			if arr, isArr := rawCommits.([]any); isArr {
				if len(arr) > MaxPushCommits {
					arr = arr[:MaxPushCommits]
					reducedTruncated = true
				}
				reduced := make([]any, 0, len(arr))
				for _, elem := range arr {
					if cm, isMap := elem.(map[string]any); isMap {
						rc, cut := reducePushCommit(cm)
						reduced = append(reduced, rc)
						reducedTruncated = reducedTruncated || cut
					} else {
						reducedTruncated = true
					}
				}
				proj["commits"] = reduced
			} else {
				reducedTruncated = true
			}
		}
		if rawRepo, exists := root["repository"]; exists && rawRepo != nil {
			if repoMap, ok := rawRepo.(map[string]any); ok {
				reduced, cut := reducePushRepository(repoMap)
				proj["repository"] = reduced
				reducedTruncated = reducedTruncated || cut
			} else {
				reducedTruncated = true
			}
		}
		if rawSender, exists := root["sender"]; exists && rawSender != nil {
			if senderMap, ok := rawSender.(map[string]any); ok {
				reduced, cut := reducePushSender(senderMap)
				proj["sender"] = reduced
				reducedTruncated = reducedTruncated || cut
			} else {
				reducedTruncated = true
			}
		}
		if rawInst, exists := root["installation"]; exists && rawInst != nil {
			if instMap, ok := rawInst.(map[string]any); ok {
				reduced, cut := reducePushInstallation(instMap)
				proj["installation"] = reduced
				reducedTruncated = reducedTruncated || cut
			} else {
				reducedTruncated = true
			}
		}
	} else {
		if v, ok := root["action"]; ok {
			proj["action"] = sanitizeValue(v)
		}
		if v, ok := root["number"]; ok {
			proj["number"] = sanitizeValue(v)
		}
		for _, key := range keptPrimaryKeys {
			val, ok := root[key]
			if !ok || val == nil {
				continue
			}
			if key == "pull_request" {
				if prMap, isMap := val.(map[string]any); isMap {
					val = reducePRBranchRepos(prMap)
				}
			}
			proj[key] = sanitizeValue(val)
		}
		if repoMap, ok := root["repository"].(map[string]any); ok {
			proj["repository"] = reduceRepository(repoMap)
		}
		if senderMap, ok := root["sender"].(map[string]any); ok {
			proj["sender"] = reduceActor(senderMap)
		}
		if instMap, ok := root["installation"].(map[string]any); ok {
			proj["installation"] = reduceInstallation(instMap)
		}
	}

	encoded, err := encodeCompactJSON(proj)
	if err != nil {
		return nil, false, err
	}
	if len(encoded) <= maxBytes {
		return encoded, reducedTruncated, nil
	}

	// §5.8 fallback: truncate body fields (longest first) to fit within maxBytes,
	// then secondary text fields (changes.*.from, diff_hunk), then non-essential
	// containers if still oversize.
	encoded, err = shrinkProjectedToFit(proj, isPush, encoded, maxBytes)
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

// reducePRBranchRepos returns a shallow copy of rawPR with head.repo and
// base.repo replaced by reduced repository maps before sanitizeValue runs,
// avoiding deep-copying large raw repository objects without mutating rawPR.
func reducePRBranchRepos(rawPR map[string]any) map[string]any {
	var prCopy map[string]any
	for _, branchKey := range []string{"head", "base"} {
		rawBranch, ok := rawPR[branchKey].(map[string]any)
		if !ok {
			continue
		}
		rawRepoMap, ok := rawBranch["repo"].(map[string]any)
		if !ok {
			continue
		}
		if prCopy == nil {
			prCopy = make(map[string]any, len(rawPR))
			for k, v := range rawPR {
				prCopy[k] = v
			}
		}
		branchCopy := make(map[string]any, len(rawBranch))
		for k, v := range rawBranch {
			branchCopy[k] = v
		}
		branchCopy["repo"] = reduceRepository(rawRepoMap)
		prCopy[branchKey] = branchCopy
	}
	if prCopy == nil {
		return rawPR
	}
	return prCopy
}

func reduceRepository(repo map[string]any) map[string]any {
	out := make(map[string]any, 7)
	copySanitizedKeys(out, repo, "id", "full_name", "name", "private", "default_branch", "html_url")
	if owner, ok := repo["owner"].(map[string]any); ok {
		out["owner"] = reduceActor(owner)
	}
	return out
}

func reduceActor(actor map[string]any) map[string]any {
	out := make(map[string]any, 3)
	copySanitizedKeys(out, actor, "login", "id", "type")
	return out
}

func reduceInstallation(inst map[string]any) map[string]any {
	out := make(map[string]any, 1)
	copySanitizedKeys(out, inst, "id")
	return out
}

func truncateCleanString(s string, maxBytes int) (string, bool) {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) <= maxBytes {
		return s, false
	}
	return truncateUTF8(s, maxBytes), true
}

func copyCappedString(dst, src map[string]any, key string, maxBytes int) bool {
	raw, ok := src[key]
	if !ok || raw == nil {
		return false
	}
	s, ok := raw.(string)
	if !ok {
		return true
	}
	capped, cut := truncateCleanString(s, maxBytes)
	dst[key] = capped
	return cut
}

func copyInt64(dst, src map[string]any, key string) bool {
	raw, ok := src[key]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return true
		}
		dst[key] = n
		return false
	case int64:
		dst[key] = v
		return false
	default:
		return true
	}
}

func copyBool(dst, src map[string]any, key string) bool {
	raw, ok := src[key]
	if !ok || raw == nil {
		return false
	}
	b, ok := raw.(bool)
	if !ok {
		return true
	}
	dst[key] = b
	return false
}

func reducePushInstallation(inst map[string]any) (map[string]any, bool) {
	out := make(map[string]any, 1)
	cutID := copyInt64(out, inst, "id")
	return out, cutID
}

func reducePushActor(actor map[string]any) (map[string]any, bool) {
	out := make(map[string]any, 2)
	cutName := copyCappedString(out, actor, "name", MaxPushActorNameBytes)
	cutUser := copyCappedString(out, actor, "username", MaxPushActorNameBytes)
	return out, cutName || cutUser
}

func reducePushSender(actor map[string]any) (map[string]any, bool) {
	out := make(map[string]any, 3)
	cutLogin := copyCappedString(out, actor, "login", MaxPushRepoStringBytes)
	cutID := copyInt64(out, actor, "id")
	cutType := copyCappedString(out, actor, "type", MaxPushSHABytes)
	return out, cutLogin || cutID || cutType
}

func reducePushRepository(repo map[string]any) (map[string]any, bool) {
	out := make(map[string]any, 7)
	truncated := copyInt64(out, repo, "id")
	if copyBool(out, repo, "private") {
		truncated = true
	}
	for _, key := range []string{"full_name", "name", "default_branch", "html_url"} {
		if copyCappedString(out, repo, key, MaxPushRepoStringBytes) {
			truncated = true
		}
	}
	if rawOwner, exists := repo["owner"]; exists && rawOwner != nil {
		if owner, ok := rawOwner.(map[string]any); ok {
			reducedOwner, cutOwner := reducePushSender(owner)
			out["owner"] = reducedOwner
			truncated = truncated || cutOwner
		} else {
			truncated = true
		}
	}
	return out, truncated
}

func reducePushCommit(commit map[string]any) (map[string]any, bool) {
	out := make(map[string]any, 11)
	truncated := copyCappedString(out, commit, "id", MaxPushSHABytes)
	if copyCappedString(out, commit, "tree_id", MaxPushSHABytes) {
		truncated = true
	}
	if copyCappedString(out, commit, "message", MaxPushCommitMessageBytes) {
		truncated = true
	}
	if copyCappedString(out, commit, "timestamp", MaxPushTimestampBytes) {
		truncated = true
	}
	if copyCappedString(out, commit, "url", MaxPushCommitURLBytes) {
		truncated = true
	}
	if copyBool(out, commit, "distinct") {
		truncated = true
	}
	if rawAuthor, exists := commit["author"]; exists && rawAuthor != nil {
		if author, ok := rawAuthor.(map[string]any); ok {
			reduced, cut := reducePushActor(author)
			out["author"] = reduced
			truncated = truncated || cut
		} else {
			truncated = true
		}
	}
	if rawCommitter, exists := commit["committer"]; exists && rawCommitter != nil {
		if committer, ok := rawCommitter.(map[string]any); ok {
			reduced, cut := reducePushActor(committer)
			out["committer"] = reduced
			truncated = truncated || cut
		} else {
			truncated = true
		}
	}
	for _, listKey := range []string{"added", "removed", "modified"} {
		rawVal, exists := commit[listKey]
		if !exists || rawVal == nil {
			continue
		}
		rawList, ok := rawVal.([]any)
		if !ok {
			truncated = true
			continue
		}
		if len(rawList) > MaxPushCommitFiles {
			rawList = rawList[:MaxPushCommitFiles]
			truncated = true
		}
		paths := make([]any, 0, len(rawList))
		for _, elem := range rawList {
			if p, isStr := elem.(string); isStr {
				capped, cut := truncateCleanString(p, MaxPushFilePathBytes)
				paths = append(paths, capped)
				truncated = truncated || cut
			} else {
				truncated = true
			}
		}
		out[listKey] = paths
	}
	return out, truncated
}

func copySanitizedKeys(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			dst[k] = sanitizeValue(v)
		}
	}
}

func sanitizeValue(v any) any {
	switch x := v.(type) {
	case string:
		if !utf8.ValidString(x) {
			return strings.ToValidUTF8(x, "\uFFFD")
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if shouldDropProjectedKey(k) {
				continue
			}
			out[k] = sanitizeValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, elem := range x {
			out[i] = sanitizeValue(elem)
		}
		return out
	default:
		return v
	}
}

func shouldDropProjectedKey(key string) bool {
	if key == "_links" || key == "gravatar_id" || key == "url" {
		return true
	}
	if strings.HasSuffix(key, "_url") && key != "html_url" {
		return true
	}
	return false
}

func encodeCompactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

type stringFieldRef struct {
	parent map[string]any
	key    string
}

func (r stringFieldRef) val() string {
	s, _ := r.parent[r.key].(string)
	return s
}

func (r stringFieldRef) set(s string) {
	r.parent[r.key] = s
}

func shrinkProjectedToFit(proj map[string]any, isPush bool, encoded []byte, maxBytes int) ([]byte, error) {
	var err error

	// Tier 1: truncate pre-edit previous values in "changes" ("from", longest first)
	// before touching current "body" text so edited deliveries keep the latest body intact.
	if changesMap, ok := proj["changes"].(map[string]any); ok {
		encoded, err = shrinkSubtreeFieldsLongestFirst(proj, changesMap, encoded, maxBytes, func(key string) bool {
			return key == "from"
		}, truncateUTF8)
		if err != nil || len(encoded) <= maxBytes {
			return encoded, err
		}
	}

	// Tier 2 (§5.8): truncate current "body" fields (longest first).
	encoded, err = shrinkSubtreeFieldsLongestFirst(proj, proj, encoded, maxBytes, func(key string) bool {
		return key == "body"
	}, truncateUTF8)
	if err != nil || len(encoded) <= maxBytes {
		return encoded, err
	}

	// Tier 3: truncate "diff_hunk" fields (longest first), keeping the tail where
	// the commented diff line sits.
	encoded, err = shrinkSubtreeFieldsLongestFirst(proj, proj, encoded, maxBytes, func(key string) bool {
		return key == "diff_hunk"
	}, truncateUTF8Tail)
	if err != nil || len(encoded) <= maxBytes {
		return encoded, err
	}

	// Tier 4: drop "changes" and cap oversize nested arrays inside primary objects.
	delete(proj, "changes")
	capNestedArrays(proj, 20)
	encoded, err = encodeCompactJSON(proj)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= maxBytes {
		return encoded, nil
	}

	// Tier 4b (push): drop commit file-path arrays from commits[], then from
	// head_commit, so large pushes with many long file paths stay bounded without
	// failing projection.
	if commitsArr, ok := proj["commits"].([]any); ok {
		if dropCommitFileArrays(commitsArr) {
			encoded, err = encodeCompactJSON(proj)
			if err != nil {
				return nil, err
			}
			if len(encoded) <= maxBytes {
				return encoded, nil
			}
		}
	}
	if headMap, ok := proj["head_commit"].(map[string]any); ok {
		if dropCommitFileArraysFromMap(headMap) {
			encoded, err = encodeCompactJSON(proj)
			if err != nil {
				return nil, err
			}
			if len(encoded) <= maxBytes {
				return encoded, nil
			}
		}
	}

	// Tier 5: truncate any remaining large non-identity strings inside primary objects.
	protected := map[string]bool{
		"action": true, "full_name": true, "name": true, "login": true,
		"type": true, "ref": true, "sha": true, "before": true, "after": true,
		"state": true, "html_url": true, "path": true,
	}
	encoded, err = shrinkSubtreeFieldsLongestFirst(proj, proj, encoded, maxBytes, func(key string) bool {
		return !protected[key]
	}, truncateUTF8)
	if err != nil {
		return nil, err
	}
	if isPush && len(encoded) > maxBytes {
		// Tier 5b (push): under pathological JSON-escape expansion (or small
		// custom maxBytes), allow shrinking commit author/committer/pusher
		// names so Tier 5's "name" protection cannot cause a refusal for push.
		for _, subKey := range []string{"commits", "head_commit", "pusher"} {
			if sub, ok := proj[subKey]; ok {
				encoded, err = shrinkSubtreeFieldsLongestFirst(proj, sub, encoded, maxBytes, func(key string) bool {
					return key == "name"
				}, truncateUTF8)
				if err != nil || len(encoded) <= maxBytes {
					return encoded, err
				}
			}
		}
	}
	if len(encoded) > maxBytes {
		return nil, fmt.Errorf("github trigger: projected payload (%d bytes) exceeds MaxPayloadBytes (%d bytes)", len(encoded), maxBytes)
	}
	return encoded, nil
}

func shrinkSubtreeFieldsLongestFirst(
	root map[string]any,
	subtree any,
	encoded []byte,
	maxBytes int,
	matchKey func(string) bool,
	cutFn func(string, int) string,
) ([]byte, error) {
	for len(encoded) > maxBytes {
		refs := collectStringFieldRefs(subtree, matchKey)
		var longest *stringFieldRef
		maxLen := 0
		for i := range refs {
			if l := len(refs[i].val()); l > maxLen {
				maxLen = l
				longest = &refs[i]
			}
		}
		if longest == nil || maxLen == 0 {
			break
		}

		excess := len(encoded) - maxBytes
		cur := longest.val()
		var target int
		if excess >= len(cur) {
			target = 0
		} else {
			// Cut by excess bytes (at least 1 byte, plus 1 more below if target
			// lands mid-rune) so the loop makes strictly monotonic progress.
			step := excess
			if step < 1 {
				step = 1
			}
			target = len(cur) - step
			if target < 0 {
				target = 0
			}
		}
		next := cutFn(cur, target)
		if len(next) == len(cur) && target > 0 {
			next = cutFn(cur, target-1)
		}
		longest.set(next)

		var err error
		encoded, err = encodeCompactJSON(root)
		if err != nil {
			return nil, err
		}
	}
	return encoded, nil
}

func collectStringFieldRefs(v any, matchKey func(string) bool) []stringFieldRef {
	var out []stringFieldRef
	var walk func(node any)
	walk = func(node any) {
		switch m := node.(type) {
		case map[string]any:
			for k, val := range m {
				if _, isStr := val.(string); isStr && matchKey(k) {
					out = append(out, stringFieldRef{parent: m, key: k})
				} else {
					walk(val)
				}
			}
		case []any:
			for _, elem := range m {
				walk(elem)
			}
		}
	}
	walk(v)
	return out
}

func dropCommitFileArrays(commits []any) bool {
	changed := false
	for _, elem := range commits {
		if m, ok := elem.(map[string]any); ok {
			if dropCommitFileArraysFromMap(m) {
				changed = true
			}
		}
	}
	return changed
}

func dropCommitFileArraysFromMap(m map[string]any) bool {
	changed := false
	for _, key := range []string{"added", "removed", "modified"} {
		if _, exists := m[key]; exists {
			delete(m, key)
			changed = true
		}
	}
	return changed
}

func capNestedArrays(v any, limit int) {
	switch m := v.(type) {
	case map[string]any:
		for k, val := range m {
			if arr, ok := val.([]any); ok {
				if len(arr) > limit {
					arr = arr[:limit]
					m[k] = arr
				}
				for _, elem := range arr {
					capNestedArrays(elem, limit)
				}
			} else {
				capNestedArrays(val, limit)
			}
		}
	}
}
