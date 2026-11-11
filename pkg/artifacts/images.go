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

package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Remote images in markdown (design section 8.4). At publish time the hub
// finds the absolute http(s) image URLs a markdown entry references, fetches
// each one once and stores it in the version's manifest under
// _remote/<sha256(url)>. The renderer then loads that copy from the hub
// instead of the remote server.
//
// Finding the URLs is one forward scan of the entry's first imageScanWindow
// bytes for inline images (![alt](url)) and <img> tags. Reference-style
// images are not fetched. The scan treats every candidate URL as an opaque
// substring of the window: it accepts one only if a cheap byte-level check
// passes, keeps it as a substring (no copy) until it is known to be new, and
// stops once it holds limit distinct candidates. Only the kept candidates
// are copied and normalized, and only the URLs the fetcher fetches are ever
// parsed.
//
// Each byte of the window is examined a fixed, small number of times, so
// the work is linear in the window. The main loop reads each byte once,
// except that it resumes after an inline destination or an <img> tag that
// an inner scan read in full. An inner scan that fails leaves its bytes to
// the main loop, which may read them again; a destination scan never starts
// inside a region another destination scan read, because a destination
// holds no brackets or parentheses. The bytes of a candidate URL are then
// read by the duplicate check and, if the candidate is kept, by
// normalization (at most maxImageURLBytes bytes per candidate) and the
// duplicate check of the normalized URL. The tests charge each of these
// passes as described here and check at most maxStepsPerByte charged steps
// per byte.

// RemotePrefix is the reserved path prefix of the files the hub fetched at
// publish time. Uploads may not use it.
const RemotePrefix = "_remote/"

// RemotePath is the manifest path of the copy of the remote resource at
// sourceURL. It depends on the URL, not on the content, so it is known
// before the fetch succeeds or fails.
func RemotePath(sourceURL string) string {
	sum := sha256.Sum256([]byte(sourceURL))
	return RemotePrefix + hex.EncodeToString(sum[:])
}

// isReservedPath reports whether p is under RemotePrefix or is that
// directory name itself.
func isReservedPath(p string) bool {
	return p == strings.TrimSuffix(RemotePrefix, "/") || strings.HasPrefix(p, RemotePrefix)
}

const (
	// imageScanWindow is how much of a markdown entry is scanned for image
	// URLs. Images beyond it are not fetched; the publisher gets one
	// warning.
	imageScanWindow = 2 << 20

	// maxImageURLBytes caps a candidate URL, before and after decoding.
	maxImageURLBytes = 2048

	// maxTagBytes caps an <img> tag.
	maxTagBytes = 4096

	// bracketDepth is how many open '[' the scan remembers.
	bracketDepth = 32
)

// candidate kinds: how a raw candidate is decoded.
const (
	fromMarkdown  = iota // an inline markdown destination
	fromAttribute        // an HTML attribute value (<img src>)
)

// imageHit is one candidate URL, as an undecoded substring of the window,
// in order of first use.
type imageHit struct {
	raw  string
	kind int
}

// imageExtract is the result of scanning a markdown entry.
type imageExtract struct {
	// urls are the distinct normalized image URLs in order of first use,
	// at most the scan's limit.
	urls []string
	// full is true when the scan stopped because it had found limit
	// distinct candidates.
	full bool
	// steps counts the bytes the scan examined, for the linearity tests.
	steps int
	// normalized and normSteps count the candidates normalized and the
	// bytes normalization examined, for the per-candidate work test.
	normalized, normSteps int
}

// imageScan is the state of one scan. It only ever holds substrings of doc
// and at most limit candidates.
type imageScan struct {
	doc   string
	limit int
	steps int

	seen map[string]struct{}
	hits []imageHit
	full bool
}

// extractImageURLs returns the absolute http(s) image URLs doc (already cut
// to the scan window) references through inline markdown images and <img>
// tags, at most limit of them.
func extractImageURLs(doc string, limit int) imageExtract {
	if limit <= 0 {
		return imageExtract{}
	}
	s := &imageScan{doc: doc, limit: limit, seen: make(map[string]struct{}, min(limit, 64))}
	s.scan()
	return s.result()
}

// addHit records a candidate unless it is a duplicate or the scan is full.
func (s *imageScan) addHit(raw string, kind int) {
	if s.full {
		return
	}
	key := raw
	s.steps += len(raw) // the duplicate check hashes the candidate once
	if _, dup := s.seen[key]; dup {
		return
	}
	s.seen[key] = struct{}{}
	s.hits = append(s.hits, imageHit{raw: raw, kind: kind})
	if len(s.seen) >= s.limit {
		s.full = true
	}
}

// bracket is an open '[' the inline scan remembers.
type bracket struct {
	image bool // opened by "!["
}

// scan reads the window once, forward, for inline images and <img> tags.
//
// Its inner scans (an inline destination, an <img> tag) only read bytes
// that cannot start another construct of the same kind before the point
// where they stop: a destination holds no '(' , '[' or ']', and a tag scan
// stops at the next '<' or '>'. So two successful scans of one kind never
// read the same bytes, and a failed scan's bytes are read again only by the
// main loop.
func (s *imageScan) scan() {
	doc := s.doc
	// The open brackets, a ring of the most recent bracketDepth: an older
	// one is forgotten when a newer one needs its slot. Only whether a
	// bracket opened an image matters.
	var stack [bracketDepth]bracket
	top, depth := 0, 0
	bang := -2 // index of the last unescaped '!'
	for i := 0; i < len(doc) && !s.full; i++ {
		s.steps++
		switch doc[i] {
		case '\\':
			i++ // the next byte is escaped
		case '!':
			bang = i
		case '[':
			top = (top + 1) % bracketDepth
			stack[top] = bracket{image: bang == i-1}
			depth = min(depth+1, bracketDepth)
		case ']':
			if depth == 0 {
				continue
			}
			b := stack[top]
			top = (top + bracketDepth - 1) % bracketDepth
			depth--
			// An inline image: "![alt](url)". Anything else after an
			// image's ']' (a reference) is not fetched.
			if b.image && i+1 < len(doc) && doc[i+1] == '(' {
				if raw, next, ok := s.inlineDestination(i + 2); ok {
					s.addHit(raw, fromMarkdown)
					i = next - 1 // resume at the byte that ended it
				}
			}
		case '<':
			if end, ok := s.imgTag(i); ok {
				i = end
			}
		}
	}
}

// inlineDestination reads the destination of an inline image starting at
// i (just after '('). It accepts <url> or a bare url that starts with an
// http(s) scheme and holds only bytes a kept URL may hold, ending at ')'
// or at whitespace before a title. The bytes it reads hold no '[' or ']'.
//
// It also returns the index of the byte that ended the destination, where
// the main loop resumes, so the main loop does not read the destination
// again.
func (s *imageScan) inlineDestination(i int) (string, int, bool) {
	doc := s.doc
	for i < len(doc) && (doc[i] == ' ' || doc[i] == '\t') {
		s.steps++
		i++
	}
	angle := i < len(doc) && doc[i] == '<'
	if angle {
		i++
	}
	if !hasHTTPScheme(doc[i:]) {
		return "", 0, false
	}
	j := i
	for j < len(doc) && j-i < maxImageURLBytes && markdownURLByte(doc[j]) {
		s.steps++
		j++
	}
	if j >= len(doc) || j-i >= maxImageURLBytes {
		return "", 0, false
	}
	end := doc[j]
	if angle {
		if end != '>' {
			return "", 0, false
		}
	} else if end != ')' && end != ' ' && end != '\t' && end != '\n' {
		return "", 0, false
	}
	return doc[i:j], j, true
}

// imgTag reads an <img ...> tag at i and records its src. It reports the
// index of the tag's '>' when it read a whole tag. It stops at the first
// '<' or '>' after i or after maxTagBytes, so the bytes it reads hold no
// other tag start.
func (s *imageScan) imgTag(i int) (int, bool) {
	doc := s.doc
	if !hasPrefixFold(doc[i:], "<img") {
		return 0, false
	}
	if i+4 >= len(doc) || !isTagNameEnd(doc[i+4]) {
		return 0, false
	}
	j := i + 4
	for ; j < len(doc) && j-i < maxTagBytes; j++ {
		s.steps++
		if doc[j] == '<' {
			return 0, false
		}
		if doc[j] == '>' {
			break
		}
	}
	if j >= len(doc) || doc[j] != '>' {
		return 0, false
	}
	s.steps += j - i // attrValue reads the tag's attributes once more
	if src, ok := attrValue(doc[i+4:j], "src"); ok {
		v := trimURLSpace(src)
		if hasHTTPSchemeLoose(v) && len(v) <= maxImageURLBytes {
			s.addHit(v, fromAttribute)
		}
	}
	return j, true
}

func isTagNameEnd(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '/' || c == '>'
}

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// attrValue returns the raw (undecoded) value of the first attribute named
// name (ASCII case-insensitive) in the attribute text of a tag, following
// the HTML tokenizer's attribute states. It reports false when the
// attribute is absent, has no value, or the text has an unterminated
// quote. It does not allocate.
func attrValue(attrs, name string) (string, bool) {
	i := 0
	for i < len(attrs) {
		for i < len(attrs) && (isHTMLSpace(attrs[i]) || attrs[i] == '/') {
			i++
		}
		if i >= len(attrs) {
			break
		}
		// Attribute name: up to whitespace, '/', '>' or '='; a leading '='
		// is part of the name.
		start := i
		i++
		for i < len(attrs) && !isHTMLSpace(attrs[i]) && attrs[i] != '/' && attrs[i] != '=' {
			i++
		}
		attrName := attrs[start:i]
		for i < len(attrs) && isHTMLSpace(attrs[i]) {
			i++
		}
		var value string
		hasValue := false
		if i < len(attrs) && attrs[i] == '=' {
			i++
			for i < len(attrs) && isHTMLSpace(attrs[i]) {
				i++
			}
			hasValue = true
			switch {
			case i < len(attrs) && (attrs[i] == '"' || attrs[i] == '\''):
				q := attrs[i]
				end := strings.IndexByte(attrs[i+1:], q)
				if end < 0 {
					return "", false
				}
				value = attrs[i+1 : i+1+end]
				i += end + 2
			default:
				vs := i
				for i < len(attrs) && !isHTMLSpace(attrs[i]) {
					i++
				}
				value = attrs[vs:i]
			}
		}
		if strings.EqualFold(attrName, name) {
			// The first attribute of a name wins; later ones are dropped.
			return value, hasValue
		}
	}
	return "", false
}

// trimURLSpace removes the leading and trailing C0 controls and spaces the
// URL parser strips.
func trimURLSpace(v string) string {
	for len(v) > 0 && v[0] <= ' ' {
		v = v[1:]
	}
	for len(v) > 0 && v[len(v)-1] <= ' ' {
		v = v[:len(v)-1]
	}
	return v
}

// result copies, decodes and normalizes the candidates in order of first
// use, keeping at most limit distinct URLs.
func (s *imageScan) result() imageExtract {
	out := imageExtract{full: s.full, steps: s.steps}
	kept := make(map[string]struct{}, len(s.hits))
	// s.hits holds at most limit candidates, so at most limit URLs are
	// kept.
	for _, h := range s.hits {
		before := out.steps
		out.normalized++
		u, ok := normalizeCounted(h.raw, h.kind, &out.steps)
		out.normSteps += out.steps - before
		if !ok {
			continue
		}
		out.steps += len(u) // the duplicate check hashes the URL once
		if _, dup := kept[u]; dup {
			continue
		}
		kept[u] = struct{}{}
		out.urls = append(out.urls, u)
	}
	return out
}

// hasHTTPScheme reports whether s starts with "http:" or "https:" in any
// case.
func hasHTTPScheme(s string) bool {
	return hasPrefixFold(s, "http:") || hasPrefixFold(s, "https:")
}

// hasHTTPSchemeLoose is hasHTTPScheme for an attribute value, where the URL
// parser removes tabs and line breaks anywhere, including in the scheme.
func hasHTTPSchemeLoose(s string) bool {
	if hasHTTPScheme(s) {
		return true
	}
	var buf [6]byte
	n := 0
	for i := 0; i < len(s) && n < len(buf); i++ {
		if c := s[i]; c != '\t' && c != '\n' && c != '\r' {
			buf[n] = c
			n++
		}
	}
	return hasHTTPScheme(string(buf[:n]))
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// markdownURLByte reports whether c may appear in a markdown destination
// the scan keeps: printable ASCII except space and the bytes that end or
// escape a destination or that a kept URL never holds.
func markdownURLByte(c byte) bool {
	if c <= ' ' || c >= 0x7f {
		return false
	}
	switch c {
	case '<', '>', '(', ')', '[', ']', '\\', '"', '`', '{', '}', '|', '^':
		return false
	}
	return true
}
