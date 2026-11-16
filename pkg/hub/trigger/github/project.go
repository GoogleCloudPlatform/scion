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
func projectDecodedPayload(root map[string]any, maxBytes int) ([]byte, bool, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPayloadBytes
	}

	proj := make(map[string]any)
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

	encoded, err := encodeCompactJSON(proj)
	if err != nil {
		return nil, false, err
	}
	if len(encoded) <= maxBytes {
		return encoded, false, nil
	}

	// §5.8 fallback: truncate body fields (longest first) to fit within maxBytes,
	// then secondary text fields (changes.*.from, diff_hunk), then non-essential
	// containers if still oversize.
	encoded, err = shrinkProjectedToFit(proj, encoded, maxBytes)
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

func shrinkProjectedToFit(proj map[string]any, encoded []byte, maxBytes int) ([]byte, error) {
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

	// Tier 5: truncate any remaining large non-identity strings inside primary objects.
	protected := map[string]bool{
		"action": true, "full_name": true, "name": true, "login": true,
		"type": true, "ref": true, "sha": true, "state": true,
	}
	encoded, err = shrinkSubtreeFieldsLongestFirst(proj, proj, encoded, maxBytes, func(key string) bool {
		return !protected[key]
	}, truncateUTF8)
	if err != nil {
		return nil, err
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
