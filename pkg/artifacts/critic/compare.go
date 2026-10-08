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

package critic

import (
	"bytes"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Normalize returns src in Unicode NFC with CRLF and lone CR line endings
// rewritten to LF. It is the comparison form for review checks.
func Normalize(src []byte) []byte {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\r' {
			out = append(out, '\n')
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
			continue
		}
		out = append(out, c)
	}
	return norm.NFC.Bytes(out)
}

// Hunk limits. Each one is checked before the work it bounds, so the size of
// a Diff result is bounded by construction: at most MaxHunks hunks, each with
// at most MaxExcerptBytes per side.
const (
	// MaxHunks is the most hunks Diff returns for one file.
	MaxHunks = 20
	// MaxExcerptBytes is the most bytes of each side quoted in a hunk.
	MaxExcerptBytes = 256
)

// Hunk describes one region where a review's clean projection differs from
// its parent. Lines are 1-based line numbers in the parent; ParentLines and
// CleanLines count the lines the region spans on each side. Parent and
// Clean quote the start of each side, cut at MaxExcerptBytes on a UTF-8
// boundary; Truncated reports a cut.
type Hunk struct {
	Line        int    `json:"line"`
	ParentLines int    `json:"parent_lines"`
	CleanLines  int    `json:"clean_lines"`
	Parent      string `json:"parent"`
	Clean       string `json:"clean"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// Diff compares two normalised texts line by line and returns the regions
// that differ, or nil when they are equal. It runs in linear time: it trims
// the common leading and trailing lines; if the remaining middles have the
// same number of lines it reports each run of differing lines (at most
// MaxHunks; more reports Truncated on the last), otherwise one hunk spans
// the whole middle. It is a summary, not a minimal edit script.
func Diff(parent, clean []byte) []Hunk {
	if bytes.Equal(parent, clean) {
		return nil
	}
	pl := splitLines(parent)
	cl := splitLines(clean)
	pre := 0
	for pre < len(pl) && pre < len(cl) && bytes.Equal(pl[pre], cl[pre]) {
		pre++
	}
	suf := 0
	for suf < len(pl)-pre && suf < len(cl)-pre && bytes.Equal(pl[len(pl)-1-suf], cl[len(cl)-1-suf]) {
		suf++
	}
	pm := pl[pre : len(pl)-suf]
	cm := cl[pre : len(cl)-suf]
	if len(pm) != len(cm) {
		return []Hunk{newHunk(pre+1, pm, cm)}
	}
	var hunks []Hunk
	for i := 0; i < len(pm); {
		if bytes.Equal(pm[i], cm[i]) {
			i++
			continue
		}
		j := i
		for j < len(pm) && !bytes.Equal(pm[j], cm[j]) {
			j++
		}
		if len(hunks) == MaxHunks {
			hunks[len(hunks)-1].Truncated = true
			break
		}
		hunks = append(hunks, newHunk(pre+i+1, pm[i:j], cm[i:j]))
		i = j
	}
	return hunks
}

// splitLines splits on LF, keeping the terminator, so that a missing final
// newline is a difference.
func splitLines(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	return bytes.SplitAfter(b, []byte("\n"))
}

func newHunk(line int, p, c [][]byte) Hunk {
	ps, pt := excerpt(p)
	cs, ct := excerpt(c)
	return Hunk{Line: line, ParentLines: len(p), CleanLines: len(c), Parent: ps, Clean: cs, Truncated: pt || ct}
}

// excerpt joins lines up to MaxExcerptBytes, cutting on a rune boundary.
func excerpt(lines [][]byte) (string, bool) {
	buf := make([]byte, 0, MaxExcerptBytes)
	for _, l := range lines {
		room := MaxExcerptBytes - len(buf)
		if len(l) <= room {
			buf = append(buf, l...)
			continue
		}
		cut := room
		for cut > 0 && !utf8.RuneStart(l[cut]) {
			cut--
		}
		return string(append(buf, l[:cut]...)), true
	}
	return string(buf), false
}
