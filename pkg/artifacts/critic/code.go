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

// span is a half-open byte range [start, end) of the input.
type span struct{ start, end int }

// codeSpans returns the Markdown code in src, in order and not
// overlapping: fenced code blocks and inline code spans, with CommonMark's
// rules for those two constructs.
//
//   - Lines end at LF, CRLF or a lone CR.
//   - A fence is a line of up to three spaces, then at least three
//     backticks or at least three tildes. A backtick fence's info string
//     holds no backtick. The block ends at a line of up to three spaces,
//     a run of the same character at least as long as the opening one, and
//     only spaces or tabs after it; with no such line it runs to the end of
//     the text. The range covers the opening line through the closing
//     line's terminator.
//   - A code span opens at a run of n backticks and closes at the next run
//     of exactly n backticks. A run with no partner is literal. A backslash
//     escapes the first backtick of a run that would open (the run is one
//     shorter); it does not escape a closing run. Spans pair up within a
//     run of consecutive non-blank lines that are not fence lines, so they
//     may cross line ends but not a blank line or a fence.
//
// Indented code blocks, block quotes and list items are not recognised,
// nor the precedence of HTML tags and autolinks over code spans. Other
// block boundaries, such as headings and table rows, do not end a run of
// lines.
//
// One forward pass: each line is examined once, and each paragraph's
// backtick runs are matched with forward-only pointers into per-length
// lists, so the work is linear. steps counts the bytes and runs examined.
func codeSpans(src []byte, steps *int) []span {
	var out []span
	var p paragraph
	n := len(src)
	fenceOpen := -1 // start of the open fence's first line, or -1
	var fenceChar byte
	fenceLen := 0
	for ls := 0; ls < n; {
		le, next := lineEnd(src, ls)
		*steps += next - ls
		line := src[ls:le]
		if fenceOpen >= 0 {
			if c, l, rest := fenceRun(line); c == fenceChar && l >= fenceLen && blank(rest) {
				out = append(out, span{fenceOpen, next})
				fenceOpen = -1
			}
			ls = next
			continue
		}
		if c, l, rest := fenceRun(line); c != 0 && (c == '~' || !hasByte(rest, '`')) {
			out = p.flush(src, out, steps)
			fenceOpen, fenceChar, fenceLen = ls, c, l
			ls = next
			continue
		}
		if blank(line) {
			out = p.flush(src, out, steps)
		} else {
			p.add(src, ls, le, steps)
		}
		ls = next
	}
	if fenceOpen >= 0 {
		return append(out, span{fenceOpen, n})
	}
	return p.flush(src, out, steps)
}

// lineEnd returns the end of the line starting at ls (before its
// terminator) and the start of the next line.
func lineEnd(src []byte, ls int) (int, int) {
	for i := ls; i < len(src); i++ {
		switch src[i] {
		case '\n':
			return i, i + 1
		case '\r':
			if i+1 < len(src) && src[i+1] == '\n' {
				return i, i + 2
			}
			return i, i + 1
		}
	}
	return len(src), len(src)
}

// fenceRun reports the fence character and run length a line opens with,
// after up to three spaces, and the rest of the line; c is 0 when the line
// does not start with a run of three or more backticks or tildes.
func fenceRun(line []byte) (c byte, l int, rest []byte) {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i == len(line) || (line[i] != '`' && line[i] != '~') {
		return 0, 0, nil
	}
	c = line[i]
	j := i
	for j < len(line) && line[j] == c {
		j++
	}
	if j-i < 3 {
		return 0, 0, nil
	}
	return c, j - i, line[j:]
}

func blank(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}

func hasByte(b []byte, c byte) bool {
	for _, x := range b {
		if x == c {
			return true
		}
	}
	return false
}

// run is a maximal run of backticks in a paragraph.
type run struct {
	start, n int
	escaped  bool // the first backtick is backslash-escaped when opening
}

// paragraph collects the backtick runs of consecutive non-blank lines.
type paragraph struct {
	runs []run
}

func (p *paragraph) add(src []byte, ls, le int, steps *int) {
	for i := ls; i < le; {
		if src[i] != '`' {
			i++
			continue
		}
		j := i
		for j < le && src[j] == '`' {
			j++
		}
		bs := 0
		for k := i - 1; k >= ls && src[k] == '\\'; k-- {
			bs++
		}
		*steps += bs
		// A run that continues a run on the previous line is a new run:
		// the line end separates them.
		p.runs = append(p.runs, run{start: i, n: j - i, escaped: bs%2 == 1})
		i = j
	}
}

// flush matches the collected runs into code spans and resets p.
func (p *paragraph) flush(src []byte, out []span, steps *int) []span {
	runs := p.runs
	p.runs = p.runs[:0]
	if len(runs) < 2 {
		return out
	}
	// byLen[n] lists the indices of runs of length n in order; ptr[n] is
	// the first entry not yet passed. Pointers only move forward.
	byLen := map[int][]int{}
	for i, r := range runs {
		byLen[r.n] = append(byLen[r.n], i)
	}
	ptr := map[int]int{}
	for i := 0; i < len(runs); {
		*steps++
		r := runs[i]
		start, n := r.start, r.n
		if r.escaped {
			start, n = start+1, n-1
		}
		if n == 0 {
			i++
			continue
		}
		list := byLen[n]
		k := ptr[n]
		for k < len(list) && list[k] <= i {
			k++
			*steps++
		}
		ptr[n] = k
		if k == len(list) {
			i++
			continue
		}
		j := list[k]
		out = append(out, span{start, runs[j].start + n})
		i = j + 1
	}
	return out
}
