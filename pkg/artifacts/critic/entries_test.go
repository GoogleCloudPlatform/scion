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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEntries(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []Entry
	}{
		{"no marks", "plain text\n", nil},
		{"insertion", "a {++b++} c", []Entry{
			{Kind: EntrySuggestion, Change: ChangeInsert, Suggestion: "b", Start: 2, End: 9, Line: 1, Column: 3},
		}},
		{"deletion", "a {--b--} c", []Entry{
			{Kind: EntrySuggestion, Change: ChangeDelete, Anchor: "b", Start: 2, End: 9, Line: 1, Column: 3},
		}},
		{"substitution", "a {~~b~>c~~} d", []Entry{
			{Kind: EntrySuggestion, Change: ChangeReplace, Anchor: "b", Suggestion: "c", Start: 2, End: 12, Line: 1, Column: 3},
		}},
		{"lone comment", "word{>>why?<<}.", []Entry{
			{Kind: EntryComment, Comment: "why?", Start: 4, End: 14, Line: 1, Column: 5},
		}},
		{"highlight", "{==key==} point", []Entry{
			{Kind: EntryHighlight, Anchor: "key", Start: 0, End: 9, Line: 1, Column: 1},
		}},
		{"comment on a highlight", "x {==sel==}{>>note<<} y", []Entry{
			{Kind: EntryComment, Anchor: "sel", Comment: "note", Start: 2, End: 21, Line: 1, Column: 3},
		}},
		{"comment on a suggestion", "{~~a~>b~~}{>>reason<<}", []Entry{
			{Kind: EntrySuggestion, Change: ChangeReplace, Anchor: "a", Suggestion: "b", Comment: "reason", Start: 0, End: 22, Line: 1, Column: 1},
		}},
		{"text between highlight and comment keeps them apart", "{==a==} {>>n<<}", []Entry{
			{Kind: EntryHighlight, Anchor: "a", Start: 0, End: 7, Line: 1, Column: 1},
			{Kind: EntryComment, Comment: "n", Start: 8, End: 15, Line: 1, Column: 9},
		}},
		{"a second comment stands alone", "{==a==}{>>one<<}{>>two<<}", []Entry{
			{Kind: EntryComment, Anchor: "a", Comment: "one", Start: 0, End: 16, Line: 1, Column: 1},
			{Kind: EntryComment, Comment: "two", Start: 16, End: 25, Line: 1, Column: 17},
		}},
		{"lines and characters", "# T\n\nhé {++x++}\r\nnext {--y\nz--}", []Entry{
			{Kind: EntrySuggestion, Change: ChangeInsert, Suggestion: "x", Start: 9, End: 16, Line: 3, Column: 4},
			{Kind: EntrySuggestion, Change: ChangeDelete, Anchor: "y\nz", Start: 23, End: 32, Line: 4, Column: 6},
		}},
		{"marks in code are literal", "`{++no++}` and {++yes++}", []Entry{
			{Kind: EntrySuggestion, Change: ChangeInsert, Suggestion: "yes", Start: 15, End: 24, Line: 1, Column: 16},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Entries([]byte(tt.src)))
		})
	}
}
