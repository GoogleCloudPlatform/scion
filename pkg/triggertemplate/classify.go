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

package triggertemplate

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Grammar limits (§5.2).
const (
	MaxPlaceholders = 64
	MaxSegmentLen   = 64
	MaxArrayIndex   = 999
)

// Class identifies the trust classification of a template placeholder path.
type Class string

const (
	// ClassTrusted identifies hub-assigned or authoring-time values usable in any phase.
	ClassTrusted Class = "trusted"
	// ClassTrustedPost identifies post-fire hub-assigned values (result.*) usable
	// only in PhasePost slots.
	ClassTrustedPost Class = "trusted_post"
	// ClassUntrusted identifies sender- or event-derived values that require
	// explicit opt-in, per-slot transforms, and task framing.
	ClassUntrusted Class = "untrusted"
)

// IsTrusted reports whether c is ClassTrusted or ClassTrustedPost.
func (c Class) IsTrusted() bool {
	return c == ClassTrusted || c == ClassTrustedPost
}

var fixedPathClasses = map[string]Class{
	// Trusted (§5.2)
	"event.id":          ClassTrusted,
	"event.source.kind": ClassTrusted,
	"event.source.id":   ClassTrusted,
	"event.source.name": ClassTrusted,
	"event.received_at": ClassTrusted,
	"trigger.id":        ClassTrusted,
	"trigger.name":      ClassTrusted,
	"project.id":        ClassTrusted,
	"project.name":      ClassTrusted,

	// Trusted, post-fire only (§5.2)
	"result.agent_id":   ClassTrustedPost,
	"result.agent_name": ClassTrustedPost,
	"result.agent_url":  ClassTrustedPost,
	"result.fire_id":    ClassTrustedPost,

	// Untrusted fixed (§5.2)
	"event.type":        ClassUntrusted,
	"event.subject":     ClassUntrusted,
	"event.occurred_at": ClassUntrusted,
}

// payloadStep is one navigation step inside event.payload.<path>.
type payloadStep struct {
	isIndex bool
	key     string
	index   int
}

// Classify validates path against the §5.2 grammar and prefix table and returns
// its trust Class. Unknown roots and unknown fixed paths return an error rather
// than defaulting to untrusted.
func Classify(path string) (Class, error) {
	if path == "" {
		return "", errors.New("empty placeholder path")
	}
	if cls, ok := fixedPathClasses[path]; ok {
		return cls, nil
	}

	const attrPrefix = "event.attributes."
	if strings.HasPrefix(path, attrPrefix) {
		key := path[len(attrPrefix):]
		if !isValidIdentSegment(key) {
			return "", errors.New("attribute key segment must match ^[A-Za-z0-9_-]{1,64}$")
		}
		return ClassUntrusted, nil
	}

	if strings.HasPrefix(path, "event.payload.") || strings.HasPrefix(path, "event.payload[") {
		if _, err := parsePayloadPath(path); err != nil {
			return "", err
		}
		return ClassUntrusted, nil
	}

	// Provide targeted diagnostics for grammar violations vs unknown paths without echoing unbounded input.
	if strings.Contains(path, "[") || strings.Contains(path, "]") {
		return "", errors.New("array index is only allowed under event.payload")
	}
	root := path
	if dot := strings.IndexByte(path, '.'); dot >= 0 {
		root = path[:dot]
	}
	switch root {
	case "event", "trigger", "project", "result":
		return "", fmt.Errorf("unknown placeholder path under root %q", root)
	default:
		return "", errors.New("unknown placeholder root (expected event, trigger, project, or result)")
	}
}

// parsePayloadPath parses a full "event.payload..." placeholder path into one
// or more payload navigation steps. Both "event.payload.items[0]" and
// "event.payload.items.[0]" (as well as root "event.payload[0]" and
// "event.payload.[0]") are accepted.
func parsePayloadPath(path string) ([]payloadStep, error) {
	const prefix = "event.payload"
	if !strings.HasPrefix(path, prefix) {
		return nil, errors.New("path does not start with event.payload")
	}
	rest := path[len(prefix):]
	if rest == "" {
		return nil, errors.New("event.payload requires a subpath")
	}

	var steps []payloadStep
	i := 0
	for i < len(rest) {
		switch rest[i] {
		case '.':
			i++
			if i >= len(rest) {
				return nil, errors.New("trailing '.' in payload path")
			}
			if rest[i] == '[' {
				idx, next, err := parseArrayIndex(rest, i)
				if err != nil {
					return nil, err
				}
				steps = append(steps, payloadStep{isIndex: true, index: idx})
				i = next
			} else {
				segEnd := i
				for segEnd < len(rest) && rest[segEnd] != '.' && rest[segEnd] != '[' {
					segEnd++
				}
				seg := rest[i:segEnd]
				if !isValidIdentSegment(seg) {
					return nil, errors.New("payload path segment must match ^[A-Za-z0-9_-]{1,64}$")
				}
				steps = append(steps, payloadStep{key: seg})
				i = segEnd
			}
		case '[':
			idx, next, err := parseArrayIndex(rest, i)
			if err != nil {
				return nil, err
			}
			steps = append(steps, payloadStep{isIndex: true, index: idx})
			i = next
		default:
			return nil, fmt.Errorf("unexpected character %q in payload path", rest[i])
		}
	}
	if len(steps) == 0 {
		return nil, errors.New("event.payload requires at least one segment")
	}
	return steps, nil
}

func parseArrayIndex(s string, start int) (int, int, error) {
	// s[start] == '['
	closeRel := strings.IndexByte(s[start+1:], ']')
	if closeRel < 0 {
		return 0, 0, errors.New("unclosed array index '['")
	}
	closeIdx := start + 1 + closeRel
	digits := s[start+1 : closeIdx]
	if digits == "" {
		return 0, 0, errors.New("empty array index")
	}
	if len(digits) > 1 && digits[0] == '0' {
		return 0, 0, errors.New("leading zeros are not allowed in array index")
	}
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return 0, 0, errors.New("array index must be a non-negative integer")
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n > MaxArrayIndex {
		return 0, 0, fmt.Errorf("array index must be between 0 and %d", MaxArrayIndex)
	}
	next := closeIdx + 1
	if next < len(s) && s[next] != '.' && s[next] != '[' {
		return 0, 0, errors.New("array index must be followed by '.', '[', or end of path")
	}
	return n, next, nil
}

func isValidIdentSegment(s string) bool {
	if len(s) < 1 || len(s) > MaxSegmentLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isLower := c >= 'a' && c <= 'z'
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isUpper && !isDigit && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
