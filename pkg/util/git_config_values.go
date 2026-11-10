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

package util

import (
	"strings"
)

// Value checks for the repository config keys RepoPolicyLinked carries only
// with a strictly valid value. Each accepts a small grammar and refuses
// everything else, so none of these keys can carry a command, a path or a
// network destination.

// maxGrepThreadsDigits bounds grep.threads to 0..999999.
const maxGrepThreadsDigits = 6

// validGrepThreads reports whether v is a non-negative decimal integer of at
// most maxGrepThreadsDigits digits (no sign, space or unit).
func validGrepThreads(v string) bool {
	return v != "" && len(v) <= maxGrepThreadsDigits && allDigits(v)
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// maxRefNameLen bounds a ref-name value.
const maxRefNameLen = 255

// validRefName reports whether v is a ref name git's check-ref-format accepts
// (a short name such as "main" or "origin/main", or a full "refs/..." name),
// and does not start with '-'. It refuses: an empty name or one longer than
// maxRefNameLen; control characters, space, '~', '^', ':', '?', '*', '[' and
// '\'; "..", "@{" and the name "@"; a leading or trailing '/', and "//"; a
// component that starts with '.' or ends with ".lock"; a trailing '.'.
func validRefName(v string) bool {
	if v == "" || len(v) > maxRefNameLen || v == "@" || v[0] == '-' {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0 {
			return false
		}
	}
	if strings.Contains(v, "..") || strings.Contains(v, "@{") || strings.Contains(v, "//") {
		return false
	}
	if v[0] == '/' || v[len(v)-1] == '/' || v[len(v)-1] == '.' {
		return false
	}
	for _, comp := range strings.Split(v, "/") {
		if strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}

// maxFilterSpecLen bounds a partial-clone filter value.
const maxFilterSpecLen = 256

// validFilterSpec reports whether v is a partial-clone filter spec from this
// subset of git's grammar:
//
//	blob:none
//	blob:limit=<n>[k|m|g]      (n: 1 to 12 digits)
//	tree:<depth>               (1 to 6 digits)
//	object:type=(blob|tree|commit|tag)
//	sparse:oid=<hex object id> (40 or 64 lower-case hex digits)
//	combine:<spec>+<spec>[+...] (two or more; each sub-spec %XX-decoded once
//	                            and itself one of the forms above, never
//	                            another combine)
//
// sparse:path (a host file) and sparse:oid with a <rev>:<path> form are
// refused.
func validFilterSpec(v string) bool {
	if v == "" || len(v) > maxFilterSpecLen {
		return false
	}
	if rest, ok := strings.CutPrefix(v, "combine:"); ok {
		subs := strings.Split(rest, "+")
		if len(subs) < 2 {
			return false
		}
		for _, s := range subs {
			d, ok := percentDecode(s)
			if !ok || strings.HasPrefix(d, "combine:") || !validSimpleFilterSpec(d) {
				return false
			}
		}
		return true
	}
	return validSimpleFilterSpec(v)
}

func validSimpleFilterSpec(v string) bool {
	switch {
	case v == "blob:none":
		return true
	case strings.HasPrefix(v, "blob:limit="):
		n := strings.TrimPrefix(v, "blob:limit=")
		if n != "" && strings.IndexByte("kmgKMG", n[len(n)-1]) >= 0 {
			n = n[:len(n)-1]
		}
		return n != "" && len(n) <= 12 && allDigits(n)
	case strings.HasPrefix(v, "tree:"):
		n := strings.TrimPrefix(v, "tree:")
		return n != "" && len(n) <= 6 && allDigits(n)
	case strings.HasPrefix(v, "object:type="):
		switch strings.TrimPrefix(v, "object:type=") {
		case "blob", "tree", "commit", "tag":
			return true
		}
		return false
	case strings.HasPrefix(v, "sparse:oid="):
		return isHexOID(strings.TrimPrefix(v, "sparse:oid="))
	}
	return false
}

func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// percentDecode decodes %XX escapes once. A '%' not followed by two hex
// digits is invalid.
func percentDecode(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return "", false
		}
		b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
		i += 2
	}
	return b.String(), true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}
