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

// Package critic parses CriticMarkup and computes the projections a review
// version is read through.
//
// Five marks are recognised, as in the MultiMarkdown CriticMarkup spec:
//
//	{++insertion++}
//	{--deletion--}
//	{~~old~>new~~}
//	{>>comment<<}
//	{==highlight==}
//
// Two projections resolve them deterministically:
//
//	mark            Clean (reject all)   Accept (accept all)
//	{++ins++}       removed              ins
//	{--del--}       del                  removed
//	{~~old~>new~~}  old                  new
//	{>>comment<<}   removed              removed
//	{==hl==}        hl                   hl
//
// Parsing rules (the spec leaves these open; this package fixes them):
//
//   - Marks do not nest. A mark ends at the first closing token of its own
//     type after the opening token; any other opening tokens inside are
//     literal content.
//   - An opening token with no closing token of its type later in the text
//     is literal text. So is a substitution with no "~>" before its
//     closing token. Scanning resumes one byte after the literal "{".
//   - In a substitution, the first "~>" after the opening token separates
//     old from new. Either side may be empty.
//   - Marks may span newlines. Code spans and fences are not special: marks
//     inside them are marks, matching the MultiMarkdown preprocessor.
//   - Tokens are ASCII, so a mark boundary never splits a UTF-8 sequence.
//     Projections are byte-exact outside marks; CRLF is preserved.
//
// Cost: Parse makes one forward pass over the input. The search for each
// closing token type (and for "~>") keeps its own cursor that only moves
// forward, and a search that finds nothing is remembered, so the total work
// is linear in the input length regardless of how many unterminated or
// malformed marks it contains. TestParseLinearWork enforces this.
package critic

import (
	"bytes"
)

// Kind identifies a segment of parsed text.
type Kind int

const (
	// Text is unmarked text, copied unchanged by every projection.
	Text Kind = iota
	// Insertion is {++...++}.
	Insertion
	// Deletion is {--...--}.
	Deletion
	// Substitution is {~~old~>new~~}.
	Substitution
	// Comment is {>>...<<}.
	Comment
	// Highlight is {==...==}.
	Highlight
)

// Segment is one contiguous piece of the input. Start and End are byte
// offsets of the whole segment, including any mark tokens.
type Segment struct {
	Kind Kind
	// Text is the content: the unmarked text, the inserted, deleted,
	// highlighted or comment text, or the old side of a substitution.
	Text []byte
	// New is the new side of a substitution; nil for other kinds.
	New        []byte
	Start, End int
}

// mark describes one mark type's tokens.
type mark struct {
	kind  Kind
	open  byte // third byte of the opening token; the first is '{'
	close string
}

// marks indexed by closer slot; the order fixes the cursor slots.
var marks = [...]mark{
	{Insertion, '+', "++}"},
	{Deletion, '-', "--}"},
	{Substitution, '~', "~~}"},
	{Comment, '>', "<<}"},
	{Highlight, '=', "==}"},
}

// subSep separates the old and new sides of a substitution.
const subSep = "~>"

// cursor finds the next occurrence of a fixed token at or after a position.
// Callers ask with non-decreasing positions. The last occurrence found is
// reused while it is still at or after the position asked for, so a new
// search always starts past the previous match; a failed search marks the
// cursor exhausted, so every later search fails in O(1). All searches
// through one cursor together therefore examine O(len(src)) bytes.
type cursor struct {
	tok       []byte
	found     int // offset of the last occurrence found, or -1 if none yet
	exhausted bool
}

func newCursor(tok string) cursor {
	return cursor{tok: []byte(tok), found: -1}
}

// next returns the offset of the first occurrence of the token at or after
// from, or -1. steps counts bytes examined.
func (c *cursor) next(src []byte, from int, steps *int) int {
	if c.found >= from {
		return c.found
	}
	if c.exhausted || from >= len(src) {
		return -1
	}
	i := bytes.Index(src[from:], c.tok)
	if i < 0 {
		*steps += len(src) - from
		c.exhausted = true
		return -1
	}
	*steps += i + len(c.tok)
	c.found = from + i
	return c.found
}

// Parse splits src into segments. Concatenating the source bytes of all
// segments in order reproduces src exactly. Segment content slices alias src.
func Parse(src []byte) []Segment {
	segs, _ := parse(src)
	return segs
}

// parse is Parse that also reports the number of bytes examined, for the
// linear-work test.
func parse(src []byte) ([]Segment, int) {
	var cur [len(marks)]cursor
	for i, m := range marks {
		cur[i] = newCursor(m.close)
	}
	sep := newCursor(subSep)
	steps := 0

	var segs []Segment
	textStart := 0
	flush := func(end int) {
		if end > textStart {
			segs = append(segs, Segment{Kind: Text, Text: src[textStart:end], Start: textStart, End: end})
		}
	}
	for i := 0; i < len(src); {
		j := bytes.IndexByte(src[i:], '{')
		if j < 0 {
			steps += len(src) - i
			break
		}
		steps += j + 1
		i += j
		slot := -1
		if i+2 < len(src) && src[i+1] == src[i+2] {
			for k, m := range marks {
				if src[i+1] == m.open {
					slot = k
					break
				}
			}
		}
		if slot < 0 {
			i++
			continue
		}
		body := i + 3
		end := cur[slot].next(src, body, &steps)
		if end < 0 {
			i++
			continue
		}
		seg := Segment{Kind: marks[slot].kind, Start: i, End: end + 3}
		if seg.Kind == Substitution {
			s := sep.next(src, body, &steps)
			if s < 0 || s+len(subSep) > end {
				i++
				continue
			}
			seg.Text = src[body:s]
			seg.New = src[s+len(subSep) : end]
		} else {
			seg.Text = src[body:end]
		}
		flush(i)
		segs = append(segs, seg)
		i = seg.End
		textStart = i
	}
	flush(len(src))
	return segs, steps
}

// Mode selects a projection.
type Mode int

const (
	// Raw returns the text unchanged.
	Raw Mode = iota
	// Clean rejects every mark: the text the reviewer started from.
	Clean
	// Accept accepts every mark.
	Accept
)

// ParseMode maps the API and CLI spelling ("clean", "accept", "" or "raw")
// to a Mode.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "", "raw":
		return Raw, true
	case "clean":
		return Clean, true
	case "accept":
		return Accept, true
	}
	return Raw, false
}

// Project applies mode to src. The result never exceeds len(src) bytes.
func Project(src []byte, mode Mode) []byte {
	if mode == Raw {
		return append([]byte(nil), src...)
	}
	out := make([]byte, 0, len(src))
	for _, s := range Parse(src) {
		switch s.Kind {
		case Text, Highlight:
			out = append(out, s.Text...)
		case Insertion:
			if mode == Accept {
				out = append(out, s.Text...)
			}
		case Deletion:
			if mode == Clean {
				out = append(out, s.Text...)
			}
		case Substitution:
			if mode == Clean {
				out = append(out, s.Text...)
			} else {
				out = append(out, s.New...)
			}
		case Comment:
		}
	}
	return out
}

// CleanText is Project(src, Clean).
func CleanText(src []byte) []byte { return Project(src, Clean) }

// AcceptText is Project(src, Accept).
func AcceptText(src []byte) []byte { return Project(src, Accept) }
