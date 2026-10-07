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
	"hash/maphash"
	"sort"
	"strings"
)

// Remote images in markdown (design section 8.4). At publish time the hub
// finds the absolute http(s) image URLs a markdown entry references, fetches
// each one once and stores it in the version's manifest under
// _remote/<sha256(url)>. The renderer then loads that copy from the hub
// instead of the remote server.
//
// Finding the URLs is a scan of the entry's first imageScanWindow bytes.
// The scan treats every candidate URL as an opaque substring of the window:
// it accepts one only if a cheap byte-level check passes, keeps it as a
// substring (no copy) until it is known to be new, and stops once it holds
// limit distinct candidates. Only the kept candidates are copied and
// normalized, and only the URLs the fetcher fetches are ever parsed. Every
// pass moves forward through the window, and no position is examined more
// than a fixed number of times, so the work is linear in the window.

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

	// maxLabelBytes caps a reference label (the CommonMark limit).
	maxLabelBytes = 999

	// maxTagBytes caps an <img> tag.
	maxTagBytes = 4096

	// bracketDepth is how many open '[' the scan remembers.
	bracketDepth = 32
)

// candidate kinds: how a raw candidate is decoded.
const (
	fromMarkdown  = iota // a markdown destination (inline or definition)
	fromAttribute        // an HTML attribute value (<img src>)
)

// imageHit is one candidate URL, as an undecoded substring of the window,
// and where it is first used.
type imageHit struct {
	pos  int
	raw  string
	kind int
}

// labelUse is a reference image (![alt][label], ![label][], ![label])
// waiting for its definition.
type labelUse struct {
	pos   int
	label string
	hash  uint64
	// def is the definition's destination once found.
	def     string
	defined bool
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
}

// imageScan is the state of one scan. It only ever holds substrings of doc
// and at most limit candidates and limit reference uses.
type imageScan struct {
	doc   string
	limit int
	steps int

	seen   map[string]struct{}
	hits   []imageHit
	uses   []labelUse
	byHash map[uint64][]int
	seed   maphash.Seed
	full   bool
}

// extractImageURLs returns the absolute http(s) image URLs doc (already cut
// to the scan window) references through markdown images, reference images
// and <img> tags, at most limit of them.
func extractImageURLs(doc string, limit int) imageExtract {
	if limit <= 0 {
		return imageExtract{}
	}
	s := &imageScan{doc: doc, limit: limit, seen: make(map[string]struct{}, min(limit, 64)), seed: maphash.MakeSeed()}
	s.scanInline()
	if len(s.uses) > 0 {
		s.scanDefinitions()
	}
	return s.result()
}

// addHit records a candidate unless it is a duplicate or the scan is full.
func (s *imageScan) addHit(pos int, raw string, kind int) {
	if s.full {
		return
	}
	key := raw
	if _, dup := s.seen[key]; dup {
		return
	}
	s.seen[key] = struct{}{}
	s.hits = append(s.hits, imageHit{pos: pos, raw: raw, kind: kind})
	if len(s.seen) >= s.limit {
		s.full = true
	}
}

// addUse records a reference image's label.
func (s *imageScan) addUse(pos int, label string) {
	if s.full || len(s.uses) >= s.limit || label == "" || len(label) > maxLabelBytes {
		return
	}
	h, ok := s.labelHash(label)
	if !ok {
		return
	}
	if s.byHash == nil {
		s.byHash = make(map[uint64][]int)
	}
	s.byHash[h] = append(s.byHash[h], len(s.uses))
	s.uses = append(s.uses, labelUse{pos: pos, label: label, hash: h})
}

// bracket is an open '[' the inline scan remembers.
type bracket struct {
	start int // first byte after '['
	image bool
}

// scanInline is the first pass: inline images, reference image uses and
// <img> tags, in one forward pass.
//
// Inner scans (a destination, a label, a tag) only read bytes that cannot
// start another construct of the same kind before the point where they
// stop, so the regions they read do not overlap and the pass is linear.
func (s *imageScan) scanInline() {
	doc := s.doc
	// The open brackets, a ring of the most recent bracketDepth: an older
	// one is forgotten when a newer one needs its slot.
	var stack [bracketDepth]bracket
	top, depth := 0, 0
	for i := 0; i < len(doc) && !s.full; i++ {
		s.steps++
		switch doc[i] {
		case '\\':
			i++ // the next byte is escaped
		case '[':
			top = (top + 1) % bracketDepth
			stack[top] = bracket{start: i + 1, image: i > 0 && doc[i-1] == '!' && !escapedAt(doc, i-1)}
			depth = min(depth+1, bracketDepth)
		case ']':
			if depth == 0 {
				continue
			}
			b := stack[top]
			top = (top + bracketDepth - 1) % bracketDepth
			depth--
			if !b.image {
				continue
			}
			s.imageAfterAlt(b.start, i)
		case '<':
			if end, ok := s.imgTag(i); ok {
				i = end
			}
		}
	}
}

// escapedAt reports whether doc[i] is preceded by an odd number of
// backslashes. It looks back at most a few bytes.
func escapedAt(doc string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && doc[j] == '\\' && n < 8; j-- {
		n++
	}
	return n%2 == 1
}

// imageAfterAlt handles what follows the ']' at close of an image whose alt
// text starts at altStart.
func (s *imageScan) imageAfterAlt(altStart, close int) {
	doc := s.doc
	next := close + 1
	switch {
	case next < len(doc) && doc[next] == '(':
		if raw, ok := s.inlineDestination(next + 1); ok {
			s.addHit(close, raw, fromMarkdown)
		}
	case next < len(doc) && doc[next] == '[':
		label, ok := s.label(next + 1)
		if !ok {
			return
		}
		if label == "" {
			label = doc[altStart:close]
		}
		s.addUse(close, label)
	default:
		s.addUse(close, doc[altStart:close])
	}
}

// label reads a reference label starting at i (just after '[') up to its
// ']'. It stops at the first '[' or ']' or after maxLabelBytes, so the
// bytes it reads hold no bracket the outer scan would act on.
func (s *imageScan) label(i int) (string, bool) {
	doc := s.doc
	for j := i; j < len(doc) && j-i <= maxLabelBytes; j++ {
		s.steps++
		switch doc[j] {
		case '\\':
			j++
		case '[':
			return "", false
		case ']':
			return doc[i:j], true
		}
	}
	return "", false
}

// inlineDestination reads the destination of an inline image starting at
// i (just after '('). It accepts <url> or a bare url that starts with an
// http(s) scheme and holds only bytes a kept URL may hold, ending at ')'
// or at whitespace before a title. The bytes it reads hold no '[' or ']'.
func (s *imageScan) inlineDestination(i int) (string, bool) {
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
		return "", false
	}
	j := i
	for j < len(doc) && j-i < maxImageURLBytes && markdownURLByte(doc[j]) {
		s.steps++
		j++
	}
	if j >= len(doc) || j-i >= maxImageURLBytes {
		return "", false
	}
	end := doc[j]
	if angle {
		if end != '>' {
			return "", false
		}
	} else if end != ')' && end != ' ' && end != '\t' && end != '\n' {
		return "", false
	}
	return doc[i:j], true
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
	if src, ok := attrValue(doc[i+4:j], "src"); ok {
		v := trimURLSpace(src)
		if hasHTTPSchemeLoose(v) && len(v) <= maxImageURLBytes {
			s.addHit(i, v, fromAttribute)
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

// scanDefinitions is the second pass: reference definitions
// ([label]: url) for the labels the first pass saw. Each line is read once
// from its start; the first definition of a label wins.
func (s *imageScan) scanDefinitions() {
	doc := s.doc
	pending := len(s.uses)
	for i := 0; i < len(doc) && pending > 0; {
		lineEnd := i
		// Up to three spaces of indentation, then '['.
		j := i
		for j < len(doc) && j-i < 3 && doc[j] == ' ' {
			s.steps++
			j++
		}
		if j < len(doc) && doc[j] == '[' {
			if label, ok := s.label(j + 1); ok && label != "" {
				k := j + 1 + len(label) + 1
				if k < len(doc) && doc[k] == ':' {
					pending -= s.definition(label, k+1)
				}
			}
		}
		// Move to the next line from where this line's scan stopped.
		if lineEnd < j {
			lineEnd = j
		}
		nl := strings.IndexByte(doc[lineEnd:], '\n')
		if nl < 0 {
			s.steps += len(doc) - lineEnd
			break
		}
		s.steps += nl + 1
		i = lineEnd + nl + 1
	}
}

// definition resolves the uses of label to the destination starting at i
// (just after "]:"), unless an earlier definition did. It returns how many
// uses it resolved.
func (s *imageScan) definition(label string, i int) int {
	h, ok := s.labelHash(label)
	if !ok {
		return 0
	}
	idx := s.byHash[h]
	if len(idx) == 0 {
		return 0
	}
	doc := s.doc
	// Whitespace, including at most one line break.
	nl := 0
	for i < len(doc) && (doc[i] == ' ' || doc[i] == '\t' || (doc[i] == '\n' && nl == 0)) {
		if doc[i] == '\n' {
			nl++
		}
		s.steps++
		i++
	}
	angle := i < len(doc) && doc[i] == '<'
	if angle {
		i++
	}
	if !hasHTTPScheme(doc[i:]) {
		return s.markDefined(h, idx, label, "")
	}
	j := i
	for j < len(doc) && j-i < maxImageURLBytes && markdownURLByte(doc[j]) {
		s.steps++
		j++
	}
	raw := doc[i:j]
	switch {
	case j-i >= maxImageURLBytes:
		raw = ""
	case angle:
		if j >= len(doc) || doc[j] != '>' {
			raw = ""
		}
	case j < len(doc) && doc[j] != ' ' && doc[j] != '\t' && doc[j] != '\n':
		raw = ""
	}
	return s.markDefined(h, idx, label, raw)
}

// markDefined gives every not yet defined use whose label matches label
// the destination raw ("" for one that cannot be used), and forgets the
// uses it resolved, so a later definition of the same label costs only
// its hash lookup.
func (s *imageScan) markDefined(h uint64, idx []int, label, raw string) int {
	n := 0
	rest := idx[:0]
	for _, k := range idx {
		u := &s.uses[k]
		s.steps += len(u.label) + len(label)
		if !u.defined && sameLabel(u.label, label) {
			u.defined, u.def = true, raw
			n++
			continue
		}
		if !u.defined {
			rest = append(rest, k)
		}
	}
	if len(rest) == 0 {
		delete(s.byHash, h)
	} else {
		s.byHash[h] = rest
	}
	return n
}

// result merges the inline candidates and the resolved reference uses in
// order of first use, then copies, decodes and normalizes each one, keeping
// at most limit distinct URLs.
func (s *imageScan) result() imageExtract {
	hits := s.hits
	for _, u := range s.uses {
		if u.defined && u.def != "" {
			hits = append(hits, imageHit{pos: u.pos, raw: u.def, kind: fromMarkdown})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].pos < hits[b].pos })
	out := imageExtract{full: s.full, steps: s.steps}
	kept := make(map[string]struct{}, len(hits))
	for _, h := range hits {
		if len(out.urls) >= s.limit {
			out.full = true
			break
		}
		u, ok := normalizeImageURL(h.raw, h.kind)
		if !ok {
			continue
		}
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

// labelHash hashes a label in its matching form (see nextLabelByte) with
// the scan's random seed. Labels with no visible text are refused.
func (s *imageScan) labelHash(label string) (uint64, bool) {
	s.steps += len(label)
	var h maphash.Hash
	h.SetSeed(s.seed)
	k, started, n := 0, false, 0
	for {
		c, ok := nextLabelByte(label, &k, &started)
		if !ok {
			break
		}
		_ = h.WriteByte(c)
		n++
	}
	return h.Sum64(), n > 0
}

// nextLabelByte returns the next byte of label's matching form: ASCII
// letters folded to lower case, each run of whitespace inside the label as
// one space, leading and trailing whitespace dropped. (Labels that differ
// only in the case of non-ASCII letters do not match; such an image shows
// its placeholder.)
func nextLabelByte(s string, k *int, started *bool) (byte, bool) {
	sawSpace := false
	for *k < len(s) {
		c := s[*k]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			sawSpace = true
			*k++
			continue
		}
		if sawSpace && *started {
			return ' ', true
		}
		*k++
		*started = true
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		return c, true
	}
	return 0, false
}

// sameLabel compares two labels in their matching form without building
// it.
func sameLabel(a, b string) bool {
	i, j := 0, 0
	sa, sb := false, false
	for {
		ca, okA := nextLabelByte(a, &i, &sa)
		cb, okB := nextLabelByte(b, &j, &sb)
		if okA != okB || ca != cb {
			return false
		}
		if !okA {
			return true
		}
	}
}
