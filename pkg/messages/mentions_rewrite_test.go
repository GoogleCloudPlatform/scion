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

// TestRewriteAgentMentions checks the body rewrite and the mention names
// extracted from the rewritten body together, so what renders matches what
// routes (ptone/scion#5132).
func TestRewriteAgentMentions(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantBody string
		wantMent []string
	}{
		{
			name:     "agent_prefix_rewritten",
			text:     "ping @agent:foo please",
			wantBody: "ping @foo please",
			wantMent: []string{"foo"},
		},
		{
			name:     "plain_mention_unchanged",
			text:     "ping @foo please",
			wantBody: "ping @foo please",
			wantMent: []string{"foo"},
		},
		{
			name:     "inline_code_span_unchanged",
			text:     "run `@agent:foo` now",
			wantBody: "run `@agent:foo` now",
			wantMent: nil,
		},
		{
			name:     "fenced_block_unchanged",
			text:     "```\n@agent:foo\n```\nthen @agent:bar",
			wantBody: "```\n@agent:foo\n```\nthen @bar",
			wantMent: []string{"bar"},
		},
		{
			name:     "quoted_line_unchanged",
			text:     "> @agent:foo said\n@agent:bar ok",
			wantBody: "> @agent:foo said\n@bar ok",
			wantMent: []string{"bar"},
		},
		{
			name:     "multiple_mentions",
			text:     "@agent:hub-ops @agent:self-migrate-lead and @plain",
			wantBody: "@hub-ops @self-migrate-lead and @plain",
			wantMent: []string{"hub-ops", "self-migrate-lead", "plain"},
		},
		{
			name:     "trailing_punctuation",
			text:     "thanks @agent:foo, and @agent:bar!",
			wantBody: "thanks @foo, and @bar!",
			wantMent: []string{"foo", "bar"},
		},
		{
			name:     "email_like_address_unchanged",
			text:     "mail ops@agent:foo or user@example.com",
			wantBody: "mail ops@agent:foo or user@example.com",
			wantMent: nil,
		},
		{
			// A bare prefix has no slug, so the body is unchanged and
			// extraction keeps its existing result for that token.
			name:     "bare_prefix_unchanged",
			text:     "a bare @agent: here and @agent:!",
			wantBody: "a bare @agent: here and @agent:!",
			wantMent: []string{"agent"},
		},
		{
			name:     "mention_after_code_span",
			text:     "`x`@agent:foo",
			wantBody: "`x`@foo",
			wantMent: []string{"foo"},
		},
		{
			name:     "multibyte_text_preserved",
			text:     "héllo @agent:foo ✓",
			wantBody: "héllo @foo ✓",
			wantMent: []string{"foo"},
		},
		{
			name:     "no_mentions",
			text:     "hello world",
			wantBody: "hello world",
			wantMent: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RewriteAgentMentions(tt.text)
			assert.Equal(t, tt.wantBody, got, "rewritten body")
			assert.Equal(t, tt.wantMent, ExtractProseMentions(got), "mentions from rewritten body")
		})
	}
}

// TestRewriteAgentMentionsIdempotent checks that rewriting an already
// rewritten body changes nothing.
func TestRewriteAgentMentionsIdempotent(t *testing.T) {
	in := "@agent:foo and `@agent:bar`\n> @agent:baz\n@agent:qux."
	once := RewriteAgentMentions(in)
	assert.Equal(t, "@foo and `@agent:bar`\n> @agent:baz\n@qux.", once)
	assert.Equal(t, once, RewriteAgentMentions(once))
}
