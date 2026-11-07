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

import "unicode/utf8"

// EntryKind names what a reviewer left: a comment, a suggested change or a
// highlight. The strings are part of the CLI's JSON output.
type EntryKind string

const (
	// EntryComment is {>>note<<}, or {==text==}{>>note<<}, which anchors
	// the note to the highlighted text.
	EntryComment EntryKind = "comment"
	// EntrySuggestion is an insertion, a deletion or a substitution.
	EntrySuggestion EntryKind = "suggestion"
	// EntryHighlight is {==text==} with no note right after it.
	EntryHighlight EntryKind = "highlight"
)

// Change is the edit a suggestion proposes.
type Change string

const (
	// ChangeInsert is {++text++}.
	ChangeInsert Change = "insert"
	// ChangeDelete is {--text--}.
	ChangeDelete Change = "delete"
	// ChangeReplace is {~~old~>new~~}.
	ChangeReplace Change = "replace"
)

// Entry is one thing a reviewer left in the text, as a reader lists it.
//
// A comment that immediately follows a highlight or a suggestion, with no
// text between them, belongs to it: the highlight becomes a comment
// anchored to the highlighted text, and a suggestion carries the comment
// as its reason. This is how the review editor writes a comment on a
// selection ({==selection==}{>>note<<}).
type Entry struct {
	Kind EntryKind
	// Change is set for a suggestion only.
	Change Change
	// Anchor is the text the entry is about: the highlighted text, the
	// deleted text, or the old side of a substitution. Empty for a
	// comment on its own and for an insertion.
	Anchor string
	// Comment is the comment text, if any.
	Comment string
	// Suggestion is the proposed text: the inserted text or the new side
	// of a substitution. Empty for a deletion and for other kinds.
	Suggestion string
	// Start and End are byte offsets of the entry's marks in the text,
	// including the tokens and an attached comment.
	Start, End int
	// Line and Column locate Start: Line is 1-based and counts "\n";
	// Column is 1-based and counts UTF-8 characters from the line start.
	Line, Column int
}

// Entries lists the marks of src as entries, in text order.
func Entries(src []byte) []Entry {
	segs := Parse(src)
	var out []Entry
	// Positions are computed in one forward pass, so the cost stays linear
	// however many entries share a line: col counts the characters (bytes
	// that do not continue a UTF-8 sequence) between the line start and
	// pos, the last offset located.
	pos, line, col := 0, 1, 0
	locate := func(off int) (int, int) {
		for ; pos < off; pos++ {
			switch b := src[pos]; {
			case b == '\n':
				line++
				col = 0
			case !utf8.RuneStart(b):
			default:
				col++
			}
		}
		return line, col + 1
	}
	for i, s := range segs {
		var e Entry
		switch s.Kind {
		case Text:
			continue
		case Comment:
			if n := len(out); n > 0 && i > 0 && segs[i-1].Kind != Text && segs[i-1].Kind != Comment && out[n-1].End == s.Start {
				prev := &out[n-1]
				prev.Comment = string(s.Text)
				prev.End = s.End
				if prev.Kind == EntryHighlight {
					prev.Kind = EntryComment
				}
				continue
			}
			e = Entry{Kind: EntryComment, Comment: string(s.Text)}
		case Highlight:
			e = Entry{Kind: EntryHighlight, Anchor: string(s.Text)}
		case Insertion:
			e = Entry{Kind: EntrySuggestion, Change: ChangeInsert, Suggestion: string(s.Text)}
		case Deletion:
			e = Entry{Kind: EntrySuggestion, Change: ChangeDelete, Anchor: string(s.Text)}
		case Substitution:
			e = Entry{Kind: EntrySuggestion, Change: ChangeReplace, Anchor: string(s.Text), Suggestion: string(s.New)}
		}
		e.Start, e.End = s.Start, s.End
		e.Line, e.Column = locate(s.Start)
		out = append(out, e)
	}
	return out
}
