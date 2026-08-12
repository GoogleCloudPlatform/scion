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

func TestDeliveredSlugs(t *testing.T) {
	results := []MentionResult{
		{Slug: "alice", Status: "delivered"},
		{Slug: "nobody", Status: "not_found"},
		{Slug: "bob", Status: "delivered"},
	}
	slugs := DeliveredSlugs(results)
	assert.Equal(t, []string{"alice", "bob"}, slugs)
}
