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
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{name: "wrapped mentions stay ignored for agent routing", text: "ping (@alice) and **@bob**", want: nil},
		{name: "possessive ascii", text: "check @instance-investigator's proposal", want: []string{"instance-investigator"}},
		{name: "possessive typographic", text: "check @instance-investigator\u2019s proposal", want: []string{"instance-investigator"}},
		{name: "possessive uppercase", text: "check @ALICE'S and @BOB\u2019S notes", want: []string{"ALICE", "BOB"}},
		{name: "possessive on slug ending in s", text: "check @hub-ops's and @devops' and @secops\u2019 logs", want: []string{"hub-ops", "devops", "secops"}},
		{name: "possessive with trailing prose punctuation", text: "per @alice's, @bob\u2019s. (see @carol's) and 'check @dave's'!", want: []string{"alice", "bob", "carol", "dave"}},
		{name: "possessive deduplicates with plain mention", text: "see @instance-investigator's note; cc @instance-investigator", want: []string{"instance-investigator"}},
		{name: "bare possessive not a mention", text: "hey @'s and @\u2019s and @!'s here", want: nil},
		{name: "wrapped possessive stays ignored for agent routing", text: "ping (@alice's) and **@bob\u2019s** and '@carol's", want: nil},
		{name: "email possessive not mention", text: "send to user@example.com's inbox", want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractMentions(tc.text)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtractWrappedMentions(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "no mentions", text: "hello world", want: nil},
		{name: "plain mention", text: "hey @alice check this", want: []string{"alice"}},
		{name: "parentheses tight", text: "ping (@alice) please", want: []string{"alice"}},
		{name: "bold markdown", text: "thanks **@alice**!", want: []string{"alice"}},
		{name: "italic asterisk", text: "cc *@alice*", want: []string{"alice"}},
		{name: "italic underscore", text: "cc _@alice_", want: []string{"alice"}},
		{name: "bold underscore", text: "cc __@alice__", want: []string{"alice"}},
		{name: "italic underscore around snake_case slug", text: "cc _@my_agent_", want: []string{"my_agent"}},
		{name: "intraword underscore before at is ignored", text: "foo_@alice", want: nil},
		{name: "strikethrough", text: "not ~~@alice~~", want: []string{"alice"}},
		{name: "double quotes", text: `ask "@alice" about it`, want: []string{"alice"}},
		{name: "single quotes", text: "ask '@alice' about it", want: []string{"alice"}},
		{name: "square brackets", text: "owner [@alice]", want: []string{"alice"}},
		{name: "angle brackets", text: "ping <@alice>", want: []string{"alice"}},
		{name: "after colon without space", text: "FYI:@alice", want: []string{"alice"}},
		{name: "nested markdown and parens", text: "(**@alice-smith**)", want: []string{"alice-smith"}},
		{name: "wrapped email mention", text: "cc **@alice.smith@test.com**", want: []string{"alice.smith@test.com"}},
		{name: "email without leading at is still ignored", text: "(alice.smith@test.com) and **bob@test.com** and mailto:carol@test.com", want: nil},
		{name: "inline backtick before at is ignored", text: "run `@alice`", want: nil},
		{name: "multiple mentions in one token", text: "@alice/@bob", want: []string{"alice", "bob"}},
		{name: "case insensitive dedup", text: "(@Alice) and **@alice**", want: []string{"Alice"}},
		{name: "bare at in parens", text: "(@) and @!!!", want: nil},
		{name: "possessive ascii and typographic", text: "check @alice's and (@bob\u2019s)", want: []string{"alice", "bob"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractWrappedMentions(tc.text))
		})
	}

	t.Run("ExtractProseWrappedMentions keeps code and quote protections", func(t *testing.T) {
		text := "inline `(**@ignored1**)` and `(cc @ignored2)`\n> **@ignored3**\n```\n(@ignored4)\n```\nreal (**@alice**) and (@bob)"
		assert.Equal(t, []string{"alice", "bob"}, ExtractProseWrappedMentions(text))
	})
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
		{
			name: "possessive in prose extracted while code and quote possessives ignored",
			text: "see `@ignored-a's` log\n> @ignored-b\u2019s quote\n```\n@ignored-c's block\n```\ntake @instance-investigator's and @hub-ops\u2019s.",
			want: []string{"instance-investigator", "hub-ops"},
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
		{name: "leading ascii possessive", text: "@agent-a's proposal looks good", firstMentionName: "agent-a", want: true},
		{name: "leading typographic possessive with comma", text: "@agent-a\u2019s, please check", firstMentionName: "agent-a", want: true},
		{name: "bare possessive", text: "@'s hello", firstMentionName: "agent-a", want: false},
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

func TestReplaceProseMention(t *testing.T) {
	tests := []struct {
		name    string
		content string
		old     string
		new     string
		want    string
	}{
		{
			name:    "known email in prose and markdown wrappers",
			content: "Ask @john@example.com, **@JOHN@EXAMPLE.COM**, _@john@example.com_, or (@john@example.com).",
			old:     "john@example.com",
			new:     "john-smith",
			want:    "Ask @john-smith, **@john-smith**, _@john-smith_, or (@john-smith).",
		},
		{
			name:    "leaves plain email in prose untouched",
			content: "Email john@example.com or user@example.com directly.",
			old:     "john@example.com",
			new:     "john-smith",
			want:    "Email john@example.com or user@example.com directly.",
		},
		{
			name:    "does not partially match longer domain or local suffix",
			content: "Skip @john@example.com.au and @@john@example.com and foo_@john@example.com.",
			old:     "john@example.com",
			new:     "john-smith",
			want:    "Skip @john@example.com.au and @@john@example.com and foo_@john@example.com.",
		},
		{
			name:    "skips inline code spans, fenced code blocks, and blockquotes",
			content: "Use `@john@example.com` and ping @john@example.com:\n```\n@john@example.com\n```\n> @john@example.com",
			old:     "john@example.com",
			new:     "john-smith",
			want:    "Use `@john@example.com` and ping @john-smith:\n```\n@john@example.com\n```\n> @john@example.com",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ReplaceProseMention(tc.content, tc.old, tc.new))
		})
	}
}

func TestCodeSpanMentions_SharedVectors(t *testing.T) {
	type vectorCase struct {
		Name          string   `json:"name"`
		Text          string   `json:"text"`
		CodeSpanOff   []string `json:"codeSpanOff"`
		CodeSpanOn    []string `json:"codeSpanOn"`
		LeadingTarget string   `json:"leadingTarget"`
		IsLeadingOff  *bool    `json:"isLeadingOff"`
		IsLeadingOn   *bool    `json:"isLeadingOn"`
	}
	type vectorFile struct {
		Cases []vectorCase `json:"cases"`
	}

	raw, err := os.ReadFile("testdata/code_span_mentions_vectors.json")
	require.NoError(t, err)
	var vf vectorFile
	require.NoError(t, json.Unmarshal(raw, &vf))
	require.NotEmpty(t, vf.Cases)

	normalize := func(s []string) []string {
		if len(s) == 0 {
			return []string{}
		}
		return s
	}

	for _, tc := range vf.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			offOpts := MentionOptions{CodeSpanMentions: false}
			onOpts := MentionOptions{CodeSpanMentions: true}

			assert.Equal(t, normalize(tc.CodeSpanOff), normalize(ExtractProseMentionsWithOptions(tc.Text, offOpts)),
				"ExtractProseMentionsWithOptions(off)")
			assert.Equal(t, normalize(tc.CodeSpanOn), normalize(ExtractProseMentionsWithOptions(tc.Text, onOpts)),
				"ExtractProseMentionsWithOptions(on)")
			assert.Equal(t, normalize(tc.CodeSpanOff), normalize(ExtractProseWrappedMentionsWithOptions(tc.Text, offOpts)),
				"ExtractProseWrappedMentionsWithOptions(off)")
			assert.Equal(t, normalize(tc.CodeSpanOn), normalize(ExtractProseWrappedMentionsWithOptions(tc.Text, onOpts)),
				"ExtractProseWrappedMentionsWithOptions(on)")
			assert.Equal(t, normalize(tc.CodeSpanOn), normalize(ExtractMentionsWithOptions(tc.Text, onOpts)),
				"ExtractMentionsWithOptions(on)")

			if tc.IsLeadingOff != nil {
				assert.Equal(t, *tc.IsLeadingOff, IsLeadingMentionWithOptions(tc.Text, tc.LeadingTarget, offOpts),
					"IsLeadingMentionWithOptions(off)")
			}
			if tc.IsLeadingOn != nil {
				assert.Equal(t, *tc.IsLeadingOn, IsLeadingMentionWithOptions(tc.Text, tc.LeadingTarget, onOpts),
					"IsLeadingMentionWithOptions(on)")
			}
		})
	}
}

func TestCodeSpanMentions_EscapedBackticksAndPathPrefixes(t *testing.T) {
	// Odd backslashes escape the opening backtick (CommonMark §6.1), so it is
	// not a code span and never promotes to a mention.
	assert.Empty(t, ExtractProseMentionsWithOptions(`\@alice`, MentionOptions{CodeSpanMentions: true}))
	assert.Empty(t, ExtractProseMentionsWithOptions("\\`@alice\\`", MentionOptions{CodeSpanMentions: true}))
	// Even backslashes escape the backslash itself, leaving an unescaped code span.
	assert.Equal(t, []string{"alice"}, ExtractProseMentionsWithOptions("\\\\`@alice`", MentionOptions{CodeSpanMentions: true}))
	// Inside a code span, a trailing backslash is literal and does not escape
	// the closing backtick; the span is closed and does not swallow the next mention.
	assert.Equal(t, []string{"bob"}, ExtractProseMentionsWithOptions("`foo\\` then @bob", MentionOptions{CodeSpanMentions: true}))

	// Path-like @./, @../, @/, @. tokens are skipped in ExtractMentions (ptone/scion#7476).
	assert.Equal(t, []string{"alice"}, ExtractMentions("see @./local @../parent @/etc/hosts @.env and @alice"))
	assert.Equal(t, []string{"alice"}, ExtractProseMentions("see @./local @../parent @/etc/hosts @.env and @alice"))
	assert.False(t, IsLeadingMention("@./local check", "./local"))
	assert.False(t, IsLeadingMention("@/etc/hosts check", "/etc/hosts"))

	// RewriteAgentMentions leaves `@agent:slug` untouched in code spans.
	assert.Equal(t, "format is `@agent:alice`", RewriteAgentMentions("format is `@agent:alice`"))
}

func TestReplaceProseMentionWithOptions_CodeSpanMentions(t *testing.T) {
	on := MentionOptions{CodeSpanMentions: true}
	off := MentionOptions{CodeSpanMentions: false}

	in := "Ask `@john@example.com`, ` @JOHN@EXAMPLE.COM `, ``@john@example.com``, `` `@john@example.com` ``, `@john@example.com please`, and \\`@john@example.com\\`.\n```\n`@john@example.com`\n```\n> `@john@example.com`"

	assert.Equal(t, in, ReplaceProseMentionWithOptions(in, "john@example.com", "john-smith", off))

	wantOn := "Ask `@john-smith`, ` @john-smith `, ``@john-smith``, `` `@john@example.com` ``, `@john@example.com please`, and \\`@john@example.com\\`.\n```\n`@john@example.com`\n```\n> `@john@example.com`"
	assert.Equal(t, wantOn, ReplaceProseMentionWithOptions(in, "john@example.com", "john-smith", on))
}

func TestExtractMentions_IgnoreNumericAndHexTokens(t *testing.T) {
	const fullSHA = "847772c4cf1e6e1a692cb12ebd06c718ebcb7d4e"
	const fullSHA256 = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "bare numeric tokens ignored",
			text: "count @0, @10, @8833, @123456, @10, @10. @10: @10's",
			want: nil,
		},
		{
			name: "short hex IDs and commit prefixes ignored",
			text: "see @847772c, @5aaf533a, @0fb0e904, @5AAF533A, @deadbeef.",
			want: nil,
		},
		{
			name: "full 40-char SHA-1 and 64-char SHA-256 ignored",
			text: "commit @" + fullSHA + " and @" + fullSHA256 + " landed",
			want: nil,
		},
		{
			name: "mixed valid mentions with numeric and hex tokens",
			text: "reviewed @5aaf533a and @10 with @alice and @dev-8833 at @" + fullSHA,
			want: []string{"alice", "dev-8833"},
		},
		{
			name: "short hex-letter names and short alphanumeric slugs preserved",
			text: "ping @abe @ada @ed @cafe @facade @a1 @b2 @e2e @be1 @fe2 @db01",
			want: []string{"abe", "ada", "ed", "cafe", "facade", "a1", "b2", "e2e", "be1", "fe2", "db01"},
		},
		{
			name: "hyphenated underscored non-hex and email mentions preserved",
			text: "cc @agent-10 @10-agent @dev-5aaf533a @5aaf533a-dev @my_10 @v10 @5aaf533g @10@example.com @5aaf533a@example.com @proj/agent",
			want: []string{"agent-10", "10-agent", "dev-5aaf533a", "5aaf533a-dev", "my_10", "v10", "5aaf533g", "10@example.com", "5aaf533a@example.com", "proj/agent"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractMentions(tc.text), "ExtractMentions")
			assert.Equal(t, tc.want, ExtractProseMentions(tc.text), "ExtractProseMentions")
		})
	}

	t.Run("ExtractWrappedMentions ignores wrapped numeric and hex tokens", func(t *testing.T) {
		text := "see (@10) and (**@5aaf533a**) and (@" + fullSHA + ") cc (@alice) and **@e2e** and (@10@example.com)"
		assert.Equal(t, []string{"alice", "e2e", "10@example.com"}, ExtractWrappedMentions(text))
	})

	t.Run("CodeSpanMentions does not promote numeric or hex code spans", func(t *testing.T) {
		opts := MentionOptions{CodeSpanMentions: true}
		text := "see `@10` and `@5aaf533a` and `@" + fullSHA + "` cc `@alice` and `@e2e`"
		assert.Equal(t, []string{"alice", "e2e"}, ExtractProseMentionsWithOptions(text, opts))
		assert.Equal(t, []string{"alice", "e2e"}, ExtractMentionsWithOptions(text, opts))
		assert.False(t, IsLeadingMentionWithOptions("`@10` hello", "10", opts))
		assert.False(t, IsLeadingMentionWithOptions("`@5aaf533a` hello", "5aaf533a", opts))
	})

	t.Run("IsLeadingMention ignores numeric and hex tokens", func(t *testing.T) {
		assert.False(t, IsLeadingMention("@10 hello", "10"))
		assert.False(t, IsLeadingMention("@5aaf533a hello", "5aaf533a"))
		assert.False(t, IsLeadingMention("@"+fullSHA+" hello", fullSHA))
		assert.True(t, IsLeadingMention("@e2e hello", "e2e"))
	})
}
