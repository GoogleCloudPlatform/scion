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

package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractMentions(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "no mentions", text: "hello world", want: nil},
		{name: "single mention", text: "hey @alice check this", want: []string{"alice"}},
		{name: "multiple mentions", text: "hey @alice and @bob check this", want: []string{"alice", "bob"}},
		{name: "mention at start", text: "@alice please review", want: []string{"alice"}},
		{name: "mention at end", text: "please review @alice", want: []string{"alice"}},
		{name: "duplicate mentions", text: "@alice hey @alice check this", want: []string{"alice"}},
		{name: "case insensitive dedup", text: "@Alice hey @alice check this", want: []string{"Alice"}},
		{name: "mention with trailing punctuation", text: "hey @alice, @bob! @charlie.", want: []string{"alice", "bob", "charlie"}},
		{name: "mention with hyphen", text: "hey @my-agent check this", want: []string{"my-agent"}},
		{name: "mention with underscore", text: "hey @my_agent check this", want: []string{"my_agent"}},
		{name: "bare at sign", text: "hey @ what", want: nil},
		{name: "email not mention", text: "send to user@example.com", want: nil},
		{name: "mention followed by colon", text: "hey @alice: check this", want: []string{"alice"}},
		{name: "mention in parentheses", text: "(cc @bob)", want: []string{"bob"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractMentions(tc.text)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtractProseMentions(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "plain mention unaffected", text: "hey @alice check this", want: []string{"alice"}},
		{name: "email not mention", text: "send to user@example.com", want: nil},
		{name: "trailing punctuation stripped", text: "hey @alice, @bob! @charlie.", want: []string{"alice", "bob", "charlie"}},
		{
			name: "closed triple-backtick fence removed",
			text: "see the log:\n```\n@builder failed to start\n```\nplease look",
			want: nil,
		},
		{
			name: "unclosed triple-backtick fence removed through EOF",
			text: "see the log:\n```\n@builder failed to start",
			want: nil,
		},
		{
			name: "tilde fence removed",
			text: "output:\n~~~\n@builder crashed\n~~~\ndone",
			want: nil,
		},
		{
			name: "mention outside fence still extracted",
			text: "```\n@ignored\n```\n@alice please look",
			want: []string{"alice"},
		},
		{
			name: "inline backtick span removed",
			text: "run `@builder --help` to see options",
			want: nil,
		},
		{
			name: "inline backtick span does not hide mention outside it",
			text: "run `@builder --help`, then ping @alice",
			want: []string{"alice"},
		},
		{
			name: "blockquote line removed",
			text: "> @builder said this already\nnothing new @alice",
			want: []string{"alice"},
		},
		{
			name: "indented blockquote line removed",
			text: "  > @builder quoted\nfresh text @alice",
			want: []string{"alice"},
		},
		{name: "no mentions", text: "hello world", want: nil},

		// Fence-closing and inline-span edge cases.
		{
			// A naive HasPrefix("```") close-check would treat "```go" as
			// closing the block, so "@alice" on the next line would wrongly
			// end up outside the fence and get extracted. It must not: an
			// info string on the line rules it out as a closing fence.
			name: "language-tagged fence line inside the block does not close it",
			text: "```\n@builder\n```go\n@alice still inside\n```\nafter @carol",
			want: []string{"carol"},
		},
		{
			name: "shorter nested fence does not close a longer one",
			text: "````\n```\n@builder\n```\n````\n@alice after",
			want: []string{"alice"},
		},
		{
			name: "longer closing fence closes a shorter opener",
			text: "```\n@builder\n````\n@alice after",
			want: []string{"alice"},
		},
		{
			name: "closing fence with trailing whitespace still closes",
			text: "```\n@builder\n```   \n@alice after",
			want: []string{"alice"},
		},
		{
			name: "double-backtick span wrapping a literal backtick is not mis-closed by it",
			text: "see `` `@builder` `` for the syntax, then ping @alice",
			want: []string{"alice"},
		},
		{
			name: "unmatched backtick run is literal text, not a code span",
			text: "use a ` here, @builder please",
			want: []string{"builder"},
		},
		{
			// CommonMark treats 4+ spaces of indentation as an indented code
			// block, not a fence opener; a backtick run there is literal
			// text, so it does not start a fence and swallow what follows.
			name: "fence opener indented 4+ spaces is not a fence, mention after it is extracted",
			text: "    ```\n@alice please look",
			want: []string{"alice"},
		},
		{
			// A backtick fence's info string may not itself contain a
			// backtick per CommonMark; a line shaped like one is not a
			// valid opener, so it does not swallow the next line's mention.
			name: "backtick fence opener whose info string contains a backtick is not a fence",
			text: "``` `weird` ```\n@alice please look",
			want: []string{"alice"},
		},
		{
			// A candidate closing run indented 4+ spaces is an indented
			// code block, not a valid closer, per CommonMark — the fence
			// stays open through it, so a mention on that still-fenced line
			// is not extracted; only the next properly unindented closer
			// actually ends the fence.
			name: "closing fence indented 4+ spaces does not close it",
			text: "```\n@builder failed\n    ```\n@alice still inside\n```\nafter @carol",
			want: []string{"carol"},
		},
		{
			// A tab expands to the next 4-column tab stop, so a
			// tab-indented backtick run is 4+ columns of indentation — an
			// indented code block, not a fence opener, per CommonMark.
			name: "tab-indented fence opener is not a fence (tab counts as 4 columns)",
			text: "\t```\n@alice please look",
			want: []string{"alice"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractProseMentions(tc.text)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestIsLeadingMention(t *testing.T) {
	tests := []struct {
		name             string
		text             string
		firstMentionName string
		want             bool
	}{
		{name: "leading mention", text: "@agent-a hello", firstMentionName: "agent-a", want: true},
		{name: "non-leading mention", text: "hello @agent-a", firstMentionName: "agent-a", want: false},
		{name: "case insensitive", text: "@AGENT-A hello", firstMentionName: "agent-a", want: true},
		{name: "name mismatch", text: "@typo hello", firstMentionName: "agent-a", want: false},
		{name: "empty name after trim", text: "@ hello", firstMentionName: "agent-a", want: false},
		{name: "trailing punct stripped", text: "@agent-a, hello", firstMentionName: "agent-a", want: true},
		{name: "empty text", text: "", firstMentionName: "agent-a", want: false},
		{name: "leading whitespace", text: "  @agent-a hello", firstMentionName: "agent-a", want: true},
		{name: "all-punct name", text: "@!!! hello", firstMentionName: "agent-a", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsLeadingMention(tc.text, tc.firstMentionName)
			assert.Equal(t, tc.want, got, "IsLeadingMention(%q, %q)", tc.text, tc.firstMentionName)
		})
	}
}

func TestParseCCFlag(t *testing.T) {
	tests := []struct {
		name string
		cc   string
		want []string
	}{
		{name: "empty", cc: "", want: nil},
		{name: "single", cc: "alice", want: []string{"alice"}},
		{name: "multiple", cc: "alice,bob,charlie", want: []string{"alice", "bob", "charlie"}},
		{name: "whitespace trimmed", cc: " alice , bob ", want: []string{"alice", "bob"}},
		{name: "empty entries", cc: "alice,,bob", want: []string{"alice", "bob"}},
		{name: "duplicates", cc: "alice,bob,alice", want: []string{"alice", "bob"}},
		{name: "case insensitive dedup", cc: "Alice,alice", want: []string{"Alice"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseCCFlag(tc.cc)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveMentions(t *testing.T) {
	agents := []AgentInfo{
		{Slug: "alice", Name: "Alice Agent"},
		{Slug: "bob", Name: "Bob Agent"},
		{Slug: "charlie", Name: "Charlie Agent"},
	}

	t.Run("empty input", func(t *testing.T) {
		results := ResolveMentions(nil, agents, "agent:primary")
		assert.Nil(t, results)
	})

	t.Run("single valid mention", func(t *testing.T) {
		results := ResolveMentions([]string{"alice"}, agents, "agent:primary")
		assert.Len(t, results, 1)
		assert.Equal(t, "alice", results[0].Slug)
		assert.Equal(t, "delivered", results[0].Status)
	})

	t.Run("unknown mention", func(t *testing.T) {
		results := ResolveMentions([]string{"nobody"}, agents, "agent:primary")
		assert.Len(t, results, 1)
		assert.Equal(t, "not_found", results[0].Status)
	})

	t.Run("primary recipient excluded", func(t *testing.T) {
		results := ResolveMentions([]string{"alice", "bob"}, agents, "agent:alice")
		assert.Len(t, results, 1)
		assert.Equal(t, "bob", results[0].Slug)
		assert.Equal(t, "delivered", results[0].Status)
	})

	t.Run("deduplication", func(t *testing.T) {
		results := ResolveMentions([]string{"alice", "Alice"}, agents, "agent:primary")
		assert.Len(t, results, 1)
		assert.Equal(t, "alice", results[0].Slug)
	})

	t.Run("cap at max", func(t *testing.T) {
		// Create many agents
		manyAgents := make([]AgentInfo, 15)
		names := make([]string, 15)
		for i := range manyAgents {
			slug := "agent-" + string(rune('a'+i))
			manyAgents[i] = AgentInfo{Slug: slug, Name: slug}
			names[i] = slug
		}
		results := ResolveMentions(names, manyAgents, "agent:primary")
		delivered := DeliveredSlugs(results)
		assert.Equal(t, MaxMentionRecipients, len(delivered))
	})
}

func TestDedupMentionNames(t *testing.T) {
	tests := []struct {
		name  string
		lists [][]string
		want  []string
	}{
		{name: "no lists", lists: nil, want: nil},
		{name: "single list dedup", lists: [][]string{{"alice", "Alice", "bob"}}, want: []string{"alice", "bob"}},
		{
			name:  "earlier list wins position, cross-list dedup case-insensitive",
			lists: [][]string{{"alice", "Bob"}, {"ALICE", "carol"}},
			want:  []string{"alice", "Bob", "carol"},
		},
		{name: "blank and whitespace-only entries skipped", lists: [][]string{{" ", "", "alice"}}, want: []string{"alice"}},
		{name: "whitespace trimmed", lists: [][]string{{"  alice  "}}, want: []string{"alice"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DedupMentionNames(tc.lists...))
		})
	}
}

func TestDeliveredSlugs(t *testing.T) {
	results := []MentionResult{
		{Slug: "alice", Status: "delivered"},
		{Slug: "nobody", Status: "not_found"},
		{Slug: "bob", Status: "delivered"},
	}
	slugs := DeliveredSlugs(results)
	assert.Equal(t, []string{"alice", "bob"}, slugs)
}
